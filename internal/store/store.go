// Package store implements the persistent memory engine for Engram.
//
// It uses SQLite with FTS5 full-text search to store and retrieve
// observations from AI coding sessions. This is the core of Engram —
// everything else (HTTP server, MCP server, CLI, plugins) talks to this.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	sqlite "modernc.org/sqlite"
	"recalldock.local/recalldock/internal/timeutil"
)

var openDB = sql.Open

// sqliteConstraintForeignKey is the extended SQLite result code for a foreign-key
// constraint violation (SQLITE_CONSTRAINT_FOREIGNKEY = 787).
// See https://www.sqlite.org/rescode.html#constraint_foreignkey
const sqliteConstraintForeignKey = 787

const (
	sqlitePrimaryBusy   = 5
	sqlitePrimaryLocked = 6
)

var sqliteWriteRetryBackoffs = []time.Duration{
	10 * time.Millisecond,
	25 * time.Millisecond,
	50 * time.Millisecond,
}

// Sentinel errors returned by delete operations so callers can use errors.Is.
var (
	ErrStoreNotInitialized               = errors.New("store_not_initialized: selected database does not exist; check RECALLDOCK_DATA_DIR or run recalldock store init explicitly")
	ErrSessionNotFound                   = errors.New("session not found")
	ErrProjectBindingRequired            = errors.New("project binding required: bind this workspace or select an established project before starting a model session")
	ErrObservationVersionNotFound        = errors.New("observation version not found")
	ErrSessionScopeMismatch              = errors.New("session ID already belongs to another project/directory; use a new session ID")
	ErrSessionHasObservations            = errors.New("session still has observations")
	ErrSessionDeleteBlocked              = errors.New("session deletion is blocked while cloud sync enrollment is active")
	ErrObservationNotFound               = errors.New("observation not found")
	ErrPromptNotFound                    = errors.New("prompt not found")
	ErrProjectNotFound                   = errors.New("project not found")
	ErrStoreIdentityMismatch             = errors.New("store identity mismatch")
	ErrInvalidProjectMapping             = errors.New("invalid project mapping")
	ErrDependencySelfReference           = errors.New("memory dependency rejected: source and dependent are the same observation")
	ErrCrossProjectDependency            = errors.New("memory dependency rejected: observations are in different projects")
	ErrDependencyCycle                   = errors.New("memory dependency rejected: edge would create a cycle")
	ErrInvalidResourceSourceRef          = errors.New("resource dependency rejected: source_ref must be a bounded immutable reference with a scheme and sha256 suffix")
	ErrResourceDependencyProjectRequired = errors.New("resource dependency rejected: explicit project is required")
	ErrCrossProjectResourceDependency    = errors.New("resource dependency rejected: dependent observation is in a different project")
	ErrResourceDependencyNotFound        = errors.New("resource dependency not found")
	ErrResourceDependencyRequiresRefresh = errors.New("resource dependency is stale and requires explicit refresh")
)

// Sentinel errors for relation sync apply path (Phase 2).
var (
	// ErrRelationFKMissing is returned by applyRelationUpsertTx when one or
	// both observations referenced by the relation payload do not exist locally
	// yet. The caller must write the mutation to sync_apply_deferred and ACK
	// the sequence so the cursor does not stall.
	ErrRelationFKMissing = errors.New("relation FK precondition not met: referenced observation missing")

	// ErrCrossProjectRelation is returned by JudgeRelation when the source and
	// target observations belong to different projects. The write is rejected
	// entirely; no memory_relations row is created and no sync mutation is
	// enqueued.
	ErrCrossProjectRelation = errors.New("relation rejected: source and target observations are in different projects")

	// ErrApplyDead is returned when a deferred relation payload cannot be
	// decoded or fails a hard validation. The row is written to
	// sync_apply_deferred with apply_status='dead' and is never retried
	// automatically; Phase 3 adds a republish CLI.
	ErrApplyDead = errors.New("relation apply permanently failed: payload invalid or undecodable")
)

// ─── Types ───────────────────────────────────────────────────────────────────

type Session struct {
	ID        string  `json:"id"`
	Project   string  `json:"project"`
	Directory string  `json:"directory"`
	StartedAt string  `json:"started_at"`
	EndedAt   *string `json:"ended_at,omitempty"`
	Summary   *string `json:"summary,omitempty"`
	// Origin is transport provenance (host|operator|model|unknown), not an
	// authentication result.
	Origin string `json:"origin,omitempty"`
}

type Observation struct {
	ID              int64   `json:"id"`
	SyncID          string  `json:"sync_id"`
	SessionID       string  `json:"session_id"`
	Type            string  `json:"type"`
	MemoryClass     string  `json:"memory_class"`
	MemoryPath      string  `json:"memory_path,omitempty"`
	Abstract        string  `json:"abstract,omitempty"`
	Overview        string  `json:"overview,omitempty"`
	SummaryState    string  `json:"summary_state"`
	SummaryRevision int     `json:"summary_revision"`
	StaleAt         *string `json:"stale_at,omitempty"`
	StaleReason     *string `json:"stale_reason,omitempty"`
	Title           string  `json:"title"`
	Content         string  `json:"content"`
	ToolName        *string `json:"tool_name,omitempty"`
	Project         *string `json:"project,omitempty"`
	Scope           string  `json:"scope"`
	TopicKey        *string `json:"topic_key,omitempty"`
	RevisionCount   int     `json:"revision_count"`
	DuplicateCount  int     `json:"duplicate_count"`
	LastSeenAt      *string `json:"last_seen_at,omitempty"`
	ReviewAfter     *string `json:"review_after,omitempty"`
	ArchivedAt      *string `json:"archived_at,omitempty"`
	RecallCount     int     `json:"recall_count"`
	LastRecallAt    *string `json:"last_recall_at,omitempty"`
	Pinned          bool    `json:"-"`
	CreatedAt       string  `json:"created_at"`
	UpdatedAt       string  `json:"updated_at"`
	DeletedAt       *string `json:"deleted_at,omitempty"`
}

const (
	ObservationStateActive      = "active"
	ObservationStateNeedsReview = "needs_review"
	ObservationStateArchived    = "archived"

	SummaryStateMissing = "missing"
	SummaryStateFresh   = "fresh"
	SummaryStateStale   = "stale"
)

const (
	MemoryClassOperational = "operational"
	MemoryClassUser        = "user"
	MemoryClassAgent       = "agent"
	MemoryClassProject     = "project"

	SearchProfileAll         = "all"
	SearchProfileSemantic    = "semantic"
	SearchProfileOperational = "operational"
)

// Session origins record which transport registered a session. They are
// provenance, not authentication: a host hook, operator CLI and model tool are
// different trust surfaces, but none of these values proves the caller's
// identity to a hostile same-UID process.
const (
	SessionOriginUnknown  = "unknown"
	SessionOriginModel    = "model"
	SessionOriginOperator = "operator"
	SessionOriginHost     = "host"
)

var sessionOriginRank = map[string]int{
	SessionOriginUnknown:  0,
	SessionOriginModel:    1,
	SessionOriginOperator: 2,
	SessionOriginHost:     3,
}

// NormalizeSessionOrigin maps unknown/empty values to the explicit unknown
// marker so new transports cannot silently inherit a trusted label.
func NormalizeSessionOrigin(origin string) string {
	switch strings.ToLower(strings.TrimSpace(origin)) {
	case SessionOriginHost:
		return SessionOriginHost
	case SessionOriginOperator:
		return SessionOriginOperator
	case SessionOriginModel:
		return SessionOriginModel
	default:
		return SessionOriginUnknown
	}
}

// strongerSessionOrigin keeps the strongest recorded provenance when the same
// session id is re-registered by another transport. It never downgrades.
func strongerSessionOrigin(existing, incoming string) string {
	existing = NormalizeSessionOrigin(existing)
	incoming = NormalizeSessionOrigin(incoming)
	if sessionOriginRank[incoming] > sessionOriginRank[existing] {
		return incoming
	}
	return existing
}

var knownProjectMemoryTypes = map[string]struct{}{
	"architecture": {},
	"bugfix":       {},
	"command":      {},
	"config":       {},
	"decision":     {},
	"discovery":    {},
	"file_change":  {},
	"file_read":    {},
	"manual":       {},
	"note":         {},
	"policy":       {},
	"reference":    {},
	"rule":         {},
	"search":       {},
	"tool_use":     {},
}

// MemoryClassForType deterministically classifies legacy observations. Unknown
// types remain project-scoped and are surfaced separately by diagnostics.
func MemoryClassForType(observationType string) string {
	switch strings.ToLower(strings.TrimSpace(observationType)) {
	case "checkpoint", "session_summary", "summary":
		return MemoryClassOperational
	case "preference", "user_fact":
		return MemoryClassUser
	case "learning", "pattern", "procedure", "experience":
		return MemoryClassAgent
	default:
		return MemoryClassProject
	}
}

// IsKnownMemoryType reports whether a type participates in the deterministic
// migration map. Unknown types still map safely to project memory.
func IsKnownMemoryType(observationType string) bool {
	typ := strings.ToLower(strings.TrimSpace(observationType))
	switch typ {
	case "checkpoint", "session_summary", "summary", "preference", "user_fact", "learning", "pattern", "procedure", "experience":
		return true
	}
	_, ok := knownProjectMemoryTypes[typ]
	return ok
}

func normalizeMemoryClass(value string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	switch normalized {
	case MemoryClassOperational, MemoryClassUser, MemoryClassAgent, MemoryClassProject:
		return normalized, nil
	default:
		return "", fmt.Errorf("invalid memory_class %q: must be operational, user, agent, or project", value)
	}
}

func resolveMemoryClass(value, observationType string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return MemoryClassForType(observationType), nil
	}
	return normalizeMemoryClass(value)
}

// NormalizeMemoryPath validates a virtual memory path and binds its root to
// the observation class. Empty paths remain valid for legacy flat memories.
func NormalizeMemoryPath(memoryClass, value string) (string, error) {
	class, err := normalizeMemoryClass(memoryClass)
	if err != nil {
		return "", err
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.Contains(value, `\`) {
		return "", fmt.Errorf("invalid memory_path %q: must be a normalized relative path", value)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("invalid memory_path %q: control characters are not allowed", value)
		}
	}
	parts := strings.Split(value, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("invalid memory_path %q: empty and traversal segments are not allowed", value)
		}
	}
	if parts[0] != class {
		return "", fmt.Errorf("invalid memory_path %q: root must match memory_class %q", value, class)
	}
	return strings.Join(parts, "/"), nil
}

func normalizeSummaryFields(abstract, overview, state string, revision int) (string, int, error) {
	hasSummary := strings.TrimSpace(abstract) != "" || strings.TrimSpace(overview) != ""
	state = strings.ToLower(strings.TrimSpace(state))
	if state == "" {
		if hasSummary {
			state = SummaryStateFresh
		} else {
			state = SummaryStateMissing
		}
	}
	switch state {
	case SummaryStateMissing:
		if !hasSummary {
			revision = 0
		}
	case SummaryStateFresh, SummaryStateStale:
		if revision < 1 {
			revision = 1
		}
	default:
		return "", 0, fmt.Errorf("invalid summary_state %q", state)
	}
	return state, revision, nil
}

// EffectiveAbstract returns the explicit L0 summary or the legacy title fallback.
func (o Observation) EffectiveAbstract() string {
	if value := strings.TrimSpace(o.Abstract); value != "" {
		return value
	}
	return o.Title
}

// EffectiveOverview returns the explicit L1 summary or a bounded L2 preview.
func (o Observation) EffectiveOverview(limit int) string {
	value := strings.TrimSpace(o.Overview)
	if value == "" {
		value = o.Content
	}
	if limit <= 0 {
		limit = 500
	}
	return truncate(value, limit)
}

// State returns the virtual lifecycle state derived from dependency staleness
// and the scheduled review timestamp.
func (o Observation) State() string {
	if o.ArchivedAt != nil {
		return ObservationStateArchived
	}
	if o.StaleAt != nil && strings.TrimSpace(*o.StaleAt) != "" {
		return ObservationStateNeedsReview
	}
	if o.ReviewAfter == nil || strings.TrimSpace(*o.ReviewAfter) == "" {
		return ObservationStateActive
	}
	reviewAfter, err := parseObservationTime(*o.ReviewAfter)
	if err != nil {
		return ObservationStateActive
	}
	if !reviewAfter.After(time.Now().UTC()) {
		return ObservationStateNeedsReview
	}
	return ObservationStateActive
}

type SearchDowngrade struct {
	Requested string `json:"requested"`
	Applied   string `json:"applied"`
	Reason    string `json:"reason"`
}

type SearchResult struct {
	Observation
	RawBM25          *float64         `json:"raw_bm25,omitempty"`
	DetailDowngraded *SearchDowngrade `json:"detail_downgraded,omitempty"`
	Rank             float64          `json:"rank"`
	Score            float64          `json:"score,omitempty"`
	SelectedDetail   string           `json:"selected_detail,omitempty"`
	SelectedContent  string           `json:"selected_content,omitempty"`
	EstimatedTokens  int              `json:"estimated_tokens,omitempty"`
	SelectionReason  string           `json:"selection_reason,omitempty"`
	ScoreSource      string           `json:"score_source,omitempty"`
}

type SessionSummary struct {
	ID               string  `json:"id"`
	Project          string  `json:"project"`
	Directory        string  `json:"directory,omitempty"`
	StartedAt        string  `json:"started_at"`
	EndedAt          *string `json:"ended_at,omitempty"`
	Summary          *string `json:"summary,omitempty"`
	ObservationCount int     `json:"observation_count"`
}

type Stats struct {
	TotalSessions     int      `json:"total_sessions"`
	TotalObservations int      `json:"total_observations"`
	TotalPrompts      int      `json:"total_prompts"`
	Projects          []string `json:"projects"`
}

type MemoryClassificationTypeCount struct {
	Type  string `json:"type"`
	Count int    `json:"count"`
}

type MemoryClassificationStats struct {
	Total          int                             `json:"total"`
	ClassCounts    map[string]int                  `json:"class_counts"`
	AmbiguousTypes []MemoryClassificationTypeCount `json:"ambiguous_types"`
}

// StoreIdentity identifies one logical Engram SQLite store. StoreID is
// persisted inside the database, so it survives moves and online backups while
// independent databases cannot be silently confused with one another.
type StoreIdentity struct {
	StoreID      string `json:"store_id"`
	DataDir      string `json:"data_dir"`
	DatabasePath string `json:"database_path"`
}

// StoreIdentityMismatchError reports an expected-store handshake failure.
// Callers should treat this as fail-closed and must not retry against the same
// database without correcting the configured data directory or expected ID.
type StoreIdentityMismatchError struct {
	Expected string
	Actual   string
	Path     string
}

func (e *StoreIdentityMismatchError) Error() string {
	return fmt.Sprintf("%v: expected %q, opened %q at %s", ErrStoreIdentityMismatch, e.Expected, e.Actual, e.Path)
}

func (e *StoreIdentityMismatchError) Unwrap() error {
	return ErrStoreIdentityMismatch
}

type TimelineEntry struct {
	ID              int64   `json:"id"`
	SessionID       string  `json:"session_id"`
	Type            string  `json:"type"`
	MemoryClass     string  `json:"memory_class"`
	MemoryPath      string  `json:"memory_path,omitempty"`
	Abstract        string  `json:"abstract,omitempty"`
	Overview        string  `json:"overview,omitempty"`
	SummaryState    string  `json:"summary_state"`
	SummaryRevision int     `json:"summary_revision"`
	StaleAt         *string `json:"stale_at,omitempty"`
	StaleReason     *string `json:"stale_reason,omitempty"`
	Title           string  `json:"title"`
	Content         string  `json:"content"`
	ToolName        *string `json:"tool_name,omitempty"`
	Project         *string `json:"project,omitempty"`
	Scope           string  `json:"scope"`
	TopicKey        *string `json:"topic_key,omitempty"`
	RevisionCount   int     `json:"revision_count"`
	DuplicateCount  int     `json:"duplicate_count"`
	LastSeenAt      *string `json:"last_seen_at,omitempty"`
	CreatedAt       string  `json:"created_at"`
	UpdatedAt       string  `json:"updated_at"`
	DeletedAt       *string `json:"deleted_at,omitempty"`
	IsFocus         bool    `json:"is_focus"` // true for the anchor observation
}

type TimelineResult struct {
	Focus        Observation     `json:"focus"`        // The anchor observation
	Before       []TimelineEntry `json:"before"`       // Observations before the focus (chronological)
	After        []TimelineEntry `json:"after"`        // Observations after the focus (chronological)
	SessionInfo  *Session        `json:"session_info"` // Session that contains the focus observation
	TotalInRange int             `json:"total_in_range"`
}

type SearchOptions struct {
	Type                string `json:"type,omitempty"`
	Project             string `json:"project,omitempty"`
	Scope               string `json:"scope,omitempty"`
	Limit               int    `json:"limit,omitempty"`
	MatchMode           string `json:"match_mode,omitempty"` // "all" (default) | "any"
	RawFTS              bool   `json:"raw_fts,omitempty"`    // explicit opt-in to FTS5 syntax
	Profile             string `json:"profile,omitempty"`    // "all" (default) | "semantic" | "operational"
	Strategy            string `json:"strategy,omitempty"`   // "flat" (default) | "hierarchical" | "auto"
	Detail              string `json:"detail,omitempty"`     // "l0" | "l1"; empty preserves legacy previews
	TokenBudget         int    `json:"token_budget,omitempty"`
	RootPath            string `json:"root_path,omitempty"`
	MaxDepth            int    `json:"max_depth,omitempty"`
	Trace               bool   `json:"trace,omitempty"`
	ExplainEmpty        bool   `json:"explain_empty,omitempty"`
	IncludeStale        bool   `json:"include_stale,omitempty"`
	DeferRecallTracking bool   `json:"-"`
	readOnlyProjection  bool
	// Offset skips ranked results for pagination; it is applied after filtering
	// and ordering, so callers page through a stable ranking.
	Offset int `json:"offset,omitempty"`
	// Since/Until bound created_at. Accept "YYYY-MM-DD", "YYYY-MM-DD HH:MM:SS"
	// or RFC3339; they are normalized to the store's UTC text form. Until is
	// exclusive, so a date-only Until includes that whole day.
	Since string `json:"since,omitempty"`
	Until string `json:"until,omitempty"`
}

type AddObservationParams struct {
	SessionID   string `json:"session_id"`
	Type        string `json:"type"`
	Title       string `json:"title"`
	Content     string `json:"content"`
	ToolName    string `json:"tool_name,omitempty"`
	Project     string `json:"project,omitempty"`
	Scope       string `json:"scope,omitempty"`
	TopicKey    string `json:"topic_key,omitempty"`
	MemoryClass string `json:"memory_class,omitempty"`
	MemoryPath  string `json:"memory_path,omitempty"`
	Abstract    string `json:"abstract,omitempty"`
	Overview    string `json:"overview,omitempty"`
}

type UpdateObservationParams struct {
	Archived    *bool   `json:"archived,omitempty"`
	Type        *string `json:"type,omitempty"`
	Title       *string `json:"title,omitempty"`
	Content     *string `json:"content,omitempty"`
	Project     *string `json:"project,omitempty"`
	Scope       *string `json:"scope,omitempty"`
	TopicKey    *string `json:"topic_key,omitempty"`
	MemoryClass *string `json:"memory_class,omitempty"`
	MemoryPath  *string `json:"memory_path,omitempty"`
	Abstract    *string `json:"abstract,omitempty"`
	Overview    *string `json:"overview,omitempty"`
}

type Prompt struct {
	ID        int64  `json:"id"`
	SyncID    string `json:"sync_id"`
	SessionID string `json:"session_id"`
	Content   string `json:"content"`
	Project   string `json:"project,omitempty"`
	CreatedAt string `json:"created_at"`
}

type AddPromptParams struct {
	SessionID string `json:"session_id"`
	Content   string `json:"content"`
	Project   string `json:"project,omitempty"`
}

const (
	DefaultSyncTargetKey = "cloud"
	LocalChunkTargetKey  = "local"

	SyncLifecycleIdle     = "idle"
	SyncLifecyclePending  = "pending"
	SyncLifecycleRunning  = "running"
	SyncLifecycleHealthy  = "healthy"
	SyncLifecycleDegraded = "degraded"

	SyncEntitySession            = "session"
	SyncEntityObservation        = "observation"
	SyncEntityPrompt             = "prompt"
	SyncEntityRelation           = "relation"
	SyncEntityDependency         = "dependency"
	SyncEntityResourceDependency = "resource_dependency"

	SyncOpUpsert = "upsert"
	SyncOpDelete = "delete"

	SyncSourceLocal  = "local"
	SyncSourceRemote = "remote"

	// Decay defaults — months added to now() to compute review_after on new inserts.
	// expires_at is NULL for all types in Phase 1.
	decayDecisionMonths   = 6
	decayPolicyMonths     = 12
	decayPreferenceMonths = 3
)

// decayReviewAfterMonths maps observation type → month offset for review_after.
// Types absent from this map get review_after = NULL (Phase 1 behavior).
var decayReviewAfterMonths = map[string]int{
	"decision":   decayDecisionMonths,
	"policy":     decayPolicyMonths,
	"preference": decayPreferenceMonths,
}

const observationSelectColumns = `id, ifnull(sync_id, '') as sync_id, session_id, type, memory_class,
	       ifnull(memory_path, ''), ifnull(abstract, ''), ifnull(overview, ''), summary_state, summary_revision, stale_at, stale_reason,
	       title, content, tool_name, project, scope, topic_key, revision_count, duplicate_count, last_seen_at, review_after,
	       recall_count, last_recall_at, pinned, created_at, updated_at, deleted_at, archived_at`

type MemoryNode struct {
	SyncID          string  `json:"sync_id"`
	Project         string  `json:"project"`
	Scope           string  `json:"scope"`
	Path            string  `json:"path"`
	ParentPath      string  `json:"parent_path,omitempty"`
	Abstract        string  `json:"abstract,omitempty"`
	Overview        string  `json:"overview,omitempty"`
	ContentRevision int     `json:"content_revision"`
	SummaryRevision int     `json:"summary_revision"`
	SummaryState    string  `json:"summary_state"`
	CreatedAt       string  `json:"created_at"`
	UpdatedAt       string  `json:"updated_at"`
	DeletedAt       *string `json:"deleted_at,omitempty"`
}

const (
	DependencyStatusActive = "active"
	DependencyStatusStale  = "stale"
)

type MemoryDependency struct {
	ID                         int64  `json:"id"`
	SyncID                     string `json:"sync_id"`
	SourceObservationID        int64  `json:"source_observation_id"`
	SourceObservationSyncID    string `json:"source_observation_sync_id"`
	DependentObservationID     int64  `json:"dependent_observation_id"`
	DependentObservationSyncID string `json:"dependent_observation_sync_id"`
	Project                    string `json:"project"`
	Reason                     string `json:"reason,omitempty"`
	SourceRevision             int    `json:"source_revision"`
	Status                     string `json:"status"`
	CreatedAt                  string `json:"created_at"`
	UpdatedAt                  string `json:"updated_at"`
}

type AddMemoryDependencyParams struct {
	SourceObservationID    int64  `json:"source_observation_id"`
	DependentObservationID int64  `json:"dependent_observation_id"`
	Reason                 string `json:"reason,omitempty"`
}

type MemoryDependencyListOptions struct {
	Project                    string `json:"project,omitempty"`
	SourceObservationSyncID    string `json:"source_observation_sync_id,omitempty"`
	DependentObservationSyncID string `json:"dependent_observation_sync_id,omitempty"`
	Status                     string `json:"status,omitempty"`
	Limit                      int    `json:"limit,omitempty"`
}

type DependencyInvalidationResult struct {
	Processed int `json:"processed"`
	Remaining int `json:"remaining"`
}

type ResourceDependency struct {
	ID                         int64   `json:"id"`
	SyncID                     string  `json:"sync_id"`
	SourceRef                  string  `json:"source_ref"`
	DependentObservationID     int64   `json:"dependent_observation_id"`
	DependentObservationSyncID string  `json:"dependent_observation_sync_id"`
	Project                    string  `json:"project"`
	Reason                     string  `json:"reason,omitempty"`
	Status                     string  `json:"status"`
	ReplacementSourceRef       *string `json:"replacement_source_ref,omitempty"`
	CreatedAt                  string  `json:"created_at"`
	UpdatedAt                  string  `json:"updated_at"`
}

type AddResourceDependencyParams struct {
	SourceRef              string `json:"source_ref"`
	DependentObservationID int64  `json:"dependent_observation_id"`
	Project                string `json:"project"`
	Reason                 string `json:"reason,omitempty"`
}

type ResourceDependencyListOptions struct {
	Project                    string `json:"project,omitempty"`
	SourceRef                  string `json:"source_ref,omitempty"`
	DependentObservationSyncID string `json:"dependent_observation_sync_id,omitempty"`
	Status                     string `json:"status,omitempty"`
	Limit                      int    `json:"limit,omitempty"`
}

type NotifyResourceChangeParams struct {
	Project              string `json:"project"`
	SourceRef            string `json:"source_ref"`
	ReplacementSourceRef string `json:"replacement_source_ref,omitempty"`
	Reason               string `json:"reason,omitempty"`
}

type ResourceChangeResult struct {
	Matched int `json:"matched"`
	Staled  int `json:"staled"`
}

type RefreshResourceDependencyParams struct {
	SyncID    string `json:"sync_id"`
	Project   string `json:"project"`
	SourceRef string `json:"source_ref,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

type SyncState struct {
	TargetKey           string  `json:"target_key"`
	Lifecycle           string  `json:"lifecycle"`
	LastEnqueuedSeq     int64   `json:"last_enqueued_seq"`
	LastAckedSeq        int64   `json:"last_acked_seq"`
	LastPulledSeq       int64   `json:"last_pulled_seq"`
	ConsecutiveFailures int     `json:"consecutive_failures"`
	BackoffUntil        *string `json:"backoff_until,omitempty"`
	LeaseOwner          *string `json:"lease_owner,omitempty"`
	LeaseUntil          *string `json:"lease_until,omitempty"`
	ReasonCode          *string `json:"reason_code,omitempty"`
	ReasonMessage       *string `json:"reason_message,omitempty"`
	LastError           *string `json:"last_error,omitempty"`
	UpdatedAt           string  `json:"updated_at"`
}

type SyncMutation struct {
	Seq        int64   `json:"seq"`
	TargetKey  string  `json:"target_key"`
	Entity     string  `json:"entity"`
	EntityKey  string  `json:"entity_key"`
	Op         string  `json:"op"`
	Payload    string  `json:"payload"`
	Source     string  `json:"source"`
	Project    string  `json:"project"`
	OccurredAt string  `json:"occurred_at"`
	AckedAt    *string `json:"acked_at,omitempty"`
}

type PendingSyncMutationProjectCount struct {
	Project      string `json:"project"`
	Count        int64  `json:"count"`
	PayloadBytes int64  `json:"payload_bytes"`
	MaxSeq       int64  `json:"-"`
}

const (
	UpgradeStagePlanned           = "planned"
	UpgradeStageDoctorReady       = "doctor_ready"
	UpgradeStageDoctorBlocked     = "doctor_blocked"
	UpgradeStageRepairApplied     = "repair_applied"
	UpgradeStageBootstrapEnrolled = "bootstrap_enrolled"
	UpgradeStageBootstrapPushed   = "bootstrap_pushed"
	UpgradeStageBootstrapVerified = "bootstrap_verified"
	UpgradeStageRolledBack        = "rolled_back"

	UpgradeRepairClassNone       = "none"
	UpgradeRepairClassReady      = "ready"
	UpgradeRepairClassRepairable = "repairable"
	UpgradeRepairClassBlocked    = "blocked"
	UpgradeRepairClassPolicy     = "policy"
)

type CloudUpgradeSnapshot struct {
	CloudConfigPresent bool   `json:"cloud_config_present"`
	CloudConfigJSON    string `json:"cloud_config_json,omitempty"`
	ProjectEnrolled    bool   `json:"project_enrolled"`
}

type CloudUpgradeState struct {
	Project          string               `json:"project"`
	Stage            string               `json:"stage"`
	RepairClass      string               `json:"repair_class"`
	Snapshot         CloudUpgradeSnapshot `json:"snapshot"`
	LastErrorCode    string               `json:"last_error_code,omitempty"`
	LastErrorMessage string               `json:"last_error_message,omitempty"`
	FindingsJSON     string               `json:"findings_json,omitempty"`
	AppliedActions   string               `json:"applied_actions,omitempty"`
	UpdatedAt        string               `json:"updated_at"`
}

type CloudUpgradeRepairReport struct {
	Class         string `json:"class"`
	ReasonCode    string `json:"reason_code"`
	Message       string `json:"message"`
	PlannedAction string `json:"planned_action,omitempty"`
	Applied       bool   `json:"applied"`
}

type CloudUpgradeLegacyMutationFinding struct {
	Seq        int64  `json:"seq"`
	Entity     string `json:"entity"`
	Op         string `json:"op"`
	ReasonCode string `json:"reason_code"`
	Message    string `json:"message"`
	Repairable bool   `json:"repairable"`
	RepairHint string `json:"repair_hint,omitempty"`
	EntityKey  string `json:"entity_key,omitempty"`
	TargetKey  string `json:"target_key,omitempty"`
	Project    string `json:"project,omitempty"`
}

type CloudUpgradeLegacyMutationReport struct {
	Project         string                              `json:"project"`
	RepairableCount int                                 `json:"repairable_count"`
	BlockedCount    int                                 `json:"blocked_count"`
	Findings        []CloudUpgradeLegacyMutationFinding `json:"findings,omitempty"`
}

const (
	UpgradeReasonRepairableLegacyMutationPayload = "upgrade_repairable_legacy_mutation_payload"
	UpgradeReasonBlockedLegacyMutationManual     = "upgrade_blocked_legacy_mutation_manual"
)

// EnrolledProject represents a project enrolled for cloud sync.
type EnrolledProject struct {
	Project    string `json:"project"`
	EnrolledAt string `json:"enrolled_at"`
}

type syncSessionPayload struct {
	ID         string  `json:"id"`
	Project    string  `json:"project"`
	Directory  string  `json:"directory,omitempty"`
	Origin     string  `json:"origin,omitempty"`
	StartedAt  string  `json:"started_at,omitempty"`
	EndedAt    *string `json:"ended_at,omitempty"`
	Summary    *string `json:"summary,omitempty"`
	Deleted    bool    `json:"deleted,omitempty"`
	DeletedAt  *string `json:"deleted_at,omitempty"`
	HardDelete bool    `json:"hard_delete,omitempty"`
}

type syncObservationPayload struct {
	ArchivedAt      *string `json:"archived_at,omitempty"`
	SyncID          string  `json:"sync_id"`
	SessionID       string  `json:"session_id"`
	Type            string  `json:"type"`
	MemoryClass     string  `json:"memory_class,omitempty"`
	MemoryPath      string  `json:"memory_path,omitempty"`
	Abstract        string  `json:"abstract,omitempty"`
	Overview        string  `json:"overview,omitempty"`
	SummaryState    string  `json:"summary_state,omitempty"`
	SummaryRevision int     `json:"summary_revision,omitempty"`
	StaleAt         *string `json:"stale_at,omitempty"`
	StaleReason     *string `json:"stale_reason,omitempty"`
	Title           string  `json:"title"`
	Content         string  `json:"content"`
	ToolName        *string `json:"tool_name,omitempty"`
	Project         *string `json:"project,omitempty"`
	Scope           string  `json:"scope"`
	TopicKey        *string `json:"topic_key,omitempty"`
	RevisionCount   int     `json:"revision_count"`
	DuplicateCount  int     `json:"duplicate_count"`
	LastSeenAt      *string `json:"last_seen_at,omitempty"`
	CreatedAt       string  `json:"created_at,omitempty"`
	UpdatedAt       string  `json:"updated_at,omitempty"`
	Deleted         bool    `json:"deleted,omitempty"`
	DeletedAt       *string `json:"deleted_at,omitempty"`
	HardDelete      bool    `json:"hard_delete,omitempty"`
}

type syncPromptPayload struct {
	SyncID     string  `json:"sync_id"`
	SessionID  string  `json:"session_id"`
	Content    string  `json:"content"`
	Project    *string `json:"project,omitempty"`
	CreatedAt  string  `json:"created_at,omitempty"`
	Deleted    bool    `json:"deleted,omitempty"`
	DeletedAt  *string `json:"deleted_at,omitempty"`
	HardDelete bool    `json:"hard_delete,omitempty"`
}

// syncRelationPayload is the wire format for a memory_relations row sent over
// the sync_mutations / cloud_mutations rails (entity = 'relation', op = 'upsert').
//
// Phase 2 design §1: 13-field subset of the 17-column memory_relations row.
// Excluded: id (local autoincrement, not portable), superseded_at,
// superseded_by_relation_id (Phase 3 supersede chain).
// omitempty matches the style of syncSessionPayload / syncObservationPayload.
type syncRelationPayload struct {
	SyncID         string   `json:"sync_id"`
	SourceID       string   `json:"source_id"`
	TargetID       string   `json:"target_id"`
	Relation       string   `json:"relation"`
	Reason         *string  `json:"reason,omitempty"`
	Evidence       *string  `json:"evidence,omitempty"`
	Confidence     *float64 `json:"confidence,omitempty"`
	JudgmentStatus string   `json:"judgment_status"`
	MarkedByActor  *string  `json:"marked_by_actor,omitempty"`
	MarkedByKind   *string  `json:"marked_by_kind,omitempty"`
	MarkedByModel  *string  `json:"marked_by_model,omitempty"`
	SessionID      *string  `json:"session_id,omitempty"`
	Project        string   `json:"project"`
	CreatedAt      string   `json:"created_at"`
	UpdatedAt      string   `json:"updated_at"`
}

type syncDependencyPayload struct {
	SyncID                     string `json:"sync_id"`
	SourceObservationSyncID    string `json:"source_observation_sync_id"`
	DependentObservationSyncID string `json:"dependent_observation_sync_id"`
	Project                    string `json:"project"`
	Reason                     string `json:"reason,omitempty"`
	SourceRevision             int    `json:"source_revision"`
	Status                     string `json:"status"`
	CreatedAt                  string `json:"created_at,omitempty"`
	UpdatedAt                  string `json:"updated_at,omitempty"`
}

type syncResourceDependencyPayload struct {
	SyncID                     string `json:"sync_id"`
	SourceRef                  string `json:"source_ref"`
	DependentObservationSyncID string `json:"dependent_observation_sync_id"`
	Project                    string `json:"project"`
	Reason                     string `json:"reason,omitempty"`
	Status                     string `json:"status"`
	ReplacementSourceRef       string `json:"replacement_source_ref,omitempty"`
	CreatedAt                  string `json:"created_at,omitempty"`
	UpdatedAt                  string `json:"updated_at,omitempty"`
}

// ExportData is the full serializable dump of the recalldock database.
type ExportData struct {
	ProjectIdentities    []RegisteredProject  `json:"project_identities,omitempty"`
	Version              string               `json:"version"`
	ExportedAt           string               `json:"exported_at"`
	Sessions             []Session            `json:"sessions"`
	Observations         []Observation        `json:"observations"`
	Prompts              []Prompt             `json:"prompts"`
	Dependencies         []MemoryDependency   `json:"dependencies,omitempty"`
	ResourceDependencies []ResourceDependency `json:"resource_dependencies,omitempty"`
}

// ─── Config ──────────────────────────────────────────────────────────────────

type Config struct {
	DataDir              string
	RequireExisting      bool
	ExpectedStoreID      string
	MaxObservationLength int
	MaxContextResults    int
	MaxSearchResults     int
	DedupeWindow         time.Duration
	DependencyMaxDepth   int
}

func DefaultConfig() (Config, error) {
	if runtime.GOOS == "windows" {
		if dataDir := DefaultDataDirForHome(""); dataDir != "" {
			return Config{
				DataDir:              dataDir,
				MaxObservationLength: 50000,
				MaxContextResults:    20,
				MaxSearchResults:     20,
				DedupeWindow:         15 * time.Minute,
				DependencyMaxDepth:   16,
			}, nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, fmt.Errorf("recalldock: determine home directory: %w", err)
	}
	return Config{
		DataDir:              DefaultDataDirForHome(home),
		MaxObservationLength: 50000,
		MaxContextResults:    20,
		MaxSearchResults:     20,
		DedupeWindow:         15 * time.Minute,
		DependencyMaxDepth:   16,
	}, nil
}

// FallbackConfig returns a Config with the given DataDir and default values.
// Use this when DefaultConfig fails and you have resolved the home directory
// through alternative means.
func FallbackConfig(dataDir string) Config {
	return Config{
		DataDir:              dataDir,
		MaxObservationLength: 50000,
		MaxContextResults:    20,
		MaxSearchResults:     20,
		DedupeWindow:         15 * time.Minute,
		DependencyMaxDepth:   16,
	}
}

// MaxObservationLength returns the configured maximum content length for observations.
func (s *Store) MaxObservationLength() int {
	return s.cfg.MaxObservationLength
}

// ─── Store ───────────────────────────────────────────────────────────────────

type Store struct {
	db    *sql.DB
	cfg   Config
	hooks storeHooks
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

type queryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

type rowScanner interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close() error
}

type sqlRowScanner struct {
	rows *sql.Rows
}

func (r sqlRowScanner) Next() bool {
	return r.rows.Next()
}

func (r sqlRowScanner) Scan(dest ...any) error {
	return r.rows.Scan(dest...)
}

func (r sqlRowScanner) Err() error {
	return r.rows.Err()
}

func (r sqlRowScanner) Close() error {
	return r.rows.Close()
}

func closeRowsWithError(rows rowScanner, err error) error {
	if closeErr := rows.Close(); closeErr != nil {
		return errors.Join(err, closeErr)
	}
	return err
}

type storeHooks struct {
	exec    func(db execer, query string, args ...any) (sql.Result, error)
	query   func(db queryer, query string, args ...any) (*sql.Rows, error)
	queryIt func(db queryer, query string, args ...any) (rowScanner, error)
	beginTx func(db *sql.DB) (*sql.Tx, error)
	commit  func(tx *sql.Tx) error
}

func defaultStoreHooks() storeHooks {
	return storeHooks{
		exec: func(db execer, query string, args ...any) (sql.Result, error) {
			return db.Exec(query, args...)
		},
		query: func(db queryer, query string, args ...any) (*sql.Rows, error) {
			return db.Query(query, args...)
		},
		queryIt: func(db queryer, query string, args ...any) (rowScanner, error) {
			rows, err := db.Query(query, args...)
			if err != nil {
				return nil, err
			}
			return sqlRowScanner{rows: rows}, nil
		},
		beginTx: func(db *sql.DB) (*sql.Tx, error) {
			return db.Begin()
		},
		commit: func(tx *sql.Tx) error {
			return tx.Commit()
		},
	}
}

// DB returns the underlying *sql.DB. Intended for test helpers and integration
// tests that need to inject raw rows (e.g. legacy data with non-normalized
// project names) without going through the Store's public API.
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) execHook(db execer, query string, args ...any) (sql.Result, error) {
	if s.hooks.exec != nil {
		return s.hooks.exec(db, query, args...)
	}
	return db.Exec(query, args...)
}

func (s *Store) queryHook(db queryer, query string, args ...any) (*sql.Rows, error) {
	if s.hooks.query != nil {
		return s.hooks.query(db, query, args...)
	}
	return db.Query(query, args...)
}

func (s *Store) queryItHook(db queryer, query string, args ...any) (rowScanner, error) {
	if s.hooks.queryIt != nil {
		return s.hooks.queryIt(db, query, args...)
	}
	rows, err := s.queryHook(db, query, args...)
	if err != nil {
		return nil, err
	}
	return sqlRowScanner{rows: rows}, nil
}

func (s *Store) beginTxHook() (*sql.Tx, error) {
	if s.hooks.beginTx != nil {
		return s.hooks.beginTx(s.db)
	}
	return s.db.Begin()
}

func (s *Store) commitHook(tx *sql.Tx) error {
	if s.hooks.commit != nil {
		return s.hooks.commit(tx)
	}
	return tx.Commit()
}

func New(cfg Config) (*Store, error) {
	if !filepath.IsAbs(cfg.DataDir) {
		return nil, fmt.Errorf("recalldock: data directory must be an absolute path, got %q — set RECALLDOCK_DATA_DIR or ensure your home directory is resolvable", cfg.DataDir)
	}
	if cfg.RequireExisting {
		info, err := os.Stat(filepath.Join(cfg.DataDir, "recalldock.db"))
		if errors.Is(err, os.ErrNotExist) || err == nil && info.Size() == 0 {
			return nil, ErrStoreNotInitialized
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, errors.New("selected database must be a regular file")
		}
	} else if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		return nil, fmt.Errorf("recalldock: create data dir: %w", err)
	}
	cfg.DataDir = canonicalDataDir(cfg.DataDir)

	dbPath := filepath.Join(cfg.DataDir, "recalldock.db")
	dsn := dbPath
	if cfg.RequireExisting {
		dsn = sqliteFileURI(dbPath, "mode=rw")
	}
	db, err := openDB("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("recalldock: open database: %w", err)
	}
	initialized := false
	defer func() {
		if !initialized {
			_ = db.Close()
		}
	}()
	db.SetMaxOpenConns(1)
	if err := preflightExpectedStoreID(db, cfg.ExpectedStoreID, dbPath); err != nil {
		return nil, err
	}

	// SQLite performance pragmas
	pragmas := []string{
		// Install the busy handler before journal mode can contend with another opener.
		"PRAGMA busy_timeout = 5000",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA foreign_keys = ON",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			return nil, fmt.Errorf("recalldock: pragma %q: %w", p, err)
		}
	}

	s := &Store{db: db, cfg: cfg, hooks: defaultStoreHooks()}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("recalldock: migration: %w", err)
	}
	if err := s.verifyExpectedStoreID(); err != nil {
		return nil, err
	}
	if err := s.repairEnrolledProjectSyncMutations(); err != nil {
		return nil, fmt.Errorf("recalldock: repair enrolled sync journal: %w", err)
	}

	initialized = true
	return s, nil
}

// newWithoutRepair is the same as New but skips repairEnrolledProjectSyncMutations.
// It exists solely to support tests that need to seed data and call repair manually.
func newWithoutRepair(cfg Config) (*Store, error) {
	if !filepath.IsAbs(cfg.DataDir) {
		return nil, fmt.Errorf("recalldock: data directory must be an absolute path, got %q — set RECALLDOCK_DATA_DIR or ensure your home directory is resolvable", cfg.DataDir)
	}
	if err := os.MkdirAll(cfg.DataDir, 0755); err != nil {
		return nil, fmt.Errorf("recalldock: create data dir: %w", err)
	}
	cfg.DataDir = canonicalDataDir(cfg.DataDir)

	dbPath := filepath.Join(cfg.DataDir, "recalldock.db")
	db, err := openDB("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("recalldock: open database: %w", err)
	}
	initialized := false
	defer func() {
		if !initialized {
			_ = db.Close()
		}
	}()
	db.SetMaxOpenConns(1)
	if err := preflightExpectedStoreID(db, cfg.ExpectedStoreID, dbPath); err != nil {
		return nil, err
	}

	pragmas := []string{
		// Install the busy handler before journal mode can contend with another opener.
		"PRAGMA busy_timeout = 5000",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA foreign_keys = ON",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			return nil, fmt.Errorf("recalldock: pragma %q: %w", p, err)
		}
	}

	s := &Store{db: db, cfg: cfg, hooks: defaultStoreHooks()}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("recalldock: migration: %w", err)
	}
	if err := s.verifyExpectedStoreID(); err != nil {
		return nil, err
	}
	initialized = true
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func canonicalDataDir(path string) string {
	path = normalizePath(path)
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return normalizePath(resolved)
	}
	return path
}

// Identity returns the persistent store ID together with the exact local paths
// used by this process. It does not expose database contents.
func (s *Store) Identity() (StoreIdentity, error) {
	var storeID string
	if err := s.db.QueryRow(`SELECT value FROM store_metadata WHERE key = 'store_id'`).Scan(&storeID); err != nil {
		return StoreIdentity{}, fmt.Errorf("read store identity: %w", err)
	}
	storeID = strings.TrimSpace(storeID)
	if storeID == "" {
		return StoreIdentity{}, errors.New("read store identity: store_id is empty")
	}
	return StoreIdentity{
		StoreID:      storeID,
		DataDir:      s.cfg.DataDir,
		DatabasePath: filepath.Join(s.cfg.DataDir, "recalldock.db"),
	}, nil
}

func (s *Store) verifyExpectedStoreID() error {
	expected := strings.TrimSpace(s.cfg.ExpectedStoreID)
	if expected == "" {
		return nil
	}
	identity, err := s.Identity()
	if err != nil {
		return err
	}
	if identity.StoreID == expected {
		return nil
	}
	return &StoreIdentityMismatchError{
		Expected: expected,
		Actual:   identity.StoreID,
		Path:     identity.DatabasePath,
	}
}

// preflightExpectedStoreID verifies an explicitly pinned store before any
// pragma, migration, or repair can write to it. A legacy/uninitialized store
// must first be opened without a pin to receive an identity, then reopened
// with the returned store_id.
func preflightExpectedStoreID(db *sql.DB, expected, dbPath string) error {
	expected = strings.TrimSpace(expected)
	if expected == "" {
		return nil
	}
	var actual string
	if err := db.QueryRow(`SELECT value FROM store_metadata WHERE key = 'store_id'`).Scan(&actual); err != nil {
		return &StoreIdentityMismatchError{
			Expected: expected,
			Actual:   "<unavailable>",
			Path:     dbPath,
		}
	}
	actual = strings.TrimSpace(actual)
	if actual == expected {
		return nil
	}
	if actual == "" {
		actual = "<unavailable>"
	}
	return &StoreIdentityMismatchError{
		Expected: expected,
		Actual:   actual,
		Path:     dbPath,
	}
}

// ─── Migrations ──────────────────────────────────────────────────────────────

func (s *Store) migrate() error {
	schema := `
			CREATE TABLE IF NOT EXISTS sessions (
				id         TEXT PRIMARY KEY,
			project    TEXT NOT NULL,
			directory  TEXT NOT NULL,
			started_at TEXT NOT NULL DEFAULT (datetime('now')),
			ended_at   TEXT,
			summary    TEXT
		);

			CREATE TABLE IF NOT EXISTS observations (
				id         INTEGER PRIMARY KEY AUTOINCREMENT,
				sync_id    TEXT,
				session_id TEXT    NOT NULL,
			type       TEXT    NOT NULL,
			title      TEXT    NOT NULL,
			content    TEXT    NOT NULL,
			tool_name  TEXT,
			project    TEXT,
			scope      TEXT    NOT NULL DEFAULT 'project',
			memory_class TEXT NOT NULL DEFAULT 'project',
			memory_path TEXT,
			abstract TEXT,
			overview TEXT,
			summary_state TEXT NOT NULL DEFAULT 'missing',
			summary_revision INTEGER NOT NULL DEFAULT 0,
			stale_at TEXT,
			stale_reason TEXT,
			topic_key  TEXT,
			normalized_hash TEXT,
			revision_count INTEGER NOT NULL DEFAULT 1,
			duplicate_count INTEGER NOT NULL DEFAULT 1,
			last_seen_at TEXT,
			pinned     BOOLEAN NOT NULL DEFAULT 0,
			recall_count INTEGER NOT NULL DEFAULT 0,
			last_recall_at TEXT,
			created_at TEXT    NOT NULL DEFAULT (datetime('now')),
			updated_at TEXT    NOT NULL DEFAULT (datetime('now')),
			deleted_at TEXT,
			FOREIGN KEY (session_id) REFERENCES sessions(id)
		);

		CREATE INDEX IF NOT EXISTS idx_obs_session  ON observations(session_id);
		CREATE INDEX IF NOT EXISTS idx_obs_type     ON observations(type);
		CREATE INDEX IF NOT EXISTS idx_obs_project  ON observations(project);
		CREATE INDEX IF NOT EXISTS idx_obs_created  ON observations(created_at DESC);

		CREATE VIRTUAL TABLE IF NOT EXISTS observations_fts USING fts5(
			title,
			content,
			tool_name,
			type,
			project,
			topic_key,
			content='observations',
			content_rowid='id'
		);

			CREATE TABLE IF NOT EXISTS user_prompts (
				id         INTEGER PRIMARY KEY AUTOINCREMENT,
				sync_id    TEXT,
				session_id TEXT    NOT NULL,
			content    TEXT    NOT NULL,
			project    TEXT,
			created_at TEXT    NOT NULL DEFAULT (datetime('now')),
			FOREIGN KEY (session_id) REFERENCES sessions(id)
		);

			CREATE TABLE IF NOT EXISTS observation_tombstones (
				sync_id TEXT PRIMARY KEY,
				session_id TEXT,
				project TEXT,
				deleted_at TEXT NOT NULL
			);

			CREATE TABLE IF NOT EXISTS prompt_tombstones (
				sync_id    TEXT PRIMARY KEY,
				session_id TEXT,
				project    TEXT,
				deleted_at TEXT NOT NULL DEFAULT (datetime('now'))
			);

		CREATE INDEX IF NOT EXISTS idx_prompts_session ON user_prompts(session_id);
		CREATE INDEX IF NOT EXISTS idx_prompts_project ON user_prompts(project);
		CREATE INDEX IF NOT EXISTS idx_prompts_created ON user_prompts(created_at DESC);

		CREATE VIRTUAL TABLE IF NOT EXISTS prompts_fts USING fts5(
			content,
			project,
			content='user_prompts',
			content_rowid='id'
		);

			CREATE TABLE IF NOT EXISTS sync_chunks (
				target_key  TEXT NOT NULL DEFAULT 'local',
				chunk_id    TEXT NOT NULL,
				imported_at TEXT NOT NULL DEFAULT (datetime('now')),
				PRIMARY KEY (target_key, chunk_id)
			);

			CREATE TABLE IF NOT EXISTS sync_state (
				target_key           TEXT PRIMARY KEY,
				lifecycle            TEXT NOT NULL DEFAULT 'idle',
				last_enqueued_seq    INTEGER NOT NULL DEFAULT 0,
				last_acked_seq       INTEGER NOT NULL DEFAULT 0,
				last_pulled_seq      INTEGER NOT NULL DEFAULT 0,
				consecutive_failures INTEGER NOT NULL DEFAULT 0,
				backoff_until        TEXT,
				lease_owner          TEXT,
				lease_until          TEXT,
				last_error           TEXT,
				updated_at           TEXT NOT NULL DEFAULT (datetime('now'))
			);

			CREATE TABLE IF NOT EXISTS sync_mutations (
				seq         INTEGER PRIMARY KEY AUTOINCREMENT,
				target_key  TEXT NOT NULL,
				entity      TEXT NOT NULL,
				entity_key  TEXT NOT NULL,
				op          TEXT NOT NULL,
				payload     TEXT NOT NULL,
				source      TEXT NOT NULL DEFAULT 'local',
				occurred_at TEXT NOT NULL DEFAULT (datetime('now')),
				acked_at    TEXT,
				FOREIGN KEY (target_key) REFERENCES sync_state(target_key)
			);

			CREATE TABLE IF NOT EXISTS cloud_upgrade_state (
				project            TEXT PRIMARY KEY,
				stage              TEXT NOT NULL DEFAULT 'planned',
				repair_class       TEXT NOT NULL DEFAULT 'none',
				snapshot_json      TEXT NOT NULL DEFAULT '{}',
				last_error_code    TEXT,
				last_error_message TEXT,
				findings_json      TEXT,
				applied_actions    TEXT,
				updated_at         TEXT NOT NULL DEFAULT (datetime('now'))
			);

			CREATE TABLE IF NOT EXISTS read_budgets (
                id TEXT PRIMARY KEY, project TEXT NOT NULL,
                token_limit INTEGER NOT NULL, used INTEGER NOT NULL DEFAULT 0,
                expires_at TEXT NOT NULL,
                CHECK(token_limit>0 AND used>=0 AND used<=token_limit)
            );
            CREATE TABLE IF NOT EXISTS read_budget_receipts (
                budget_id TEXT NOT NULL REFERENCES read_budgets(id) ON DELETE CASCADE,
                receipt_key TEXT NOT NULL, PRIMARY KEY(budget_id,receipt_key)
            );
            CREATE TABLE IF NOT EXISTS project_identities (
                id TEXT PRIMARY KEY, name TEXT NOT NULL, created_at TEXT NOT NULL
            );
            CREATE TABLE IF NOT EXISTS project_aliases (
                alias TEXT PRIMARY KEY, project_id TEXT NOT NULL REFERENCES project_identities(id),
                source TEXT NOT NULL DEFAULT 'operator', created_at TEXT NOT NULL DEFAULT (datetime('now'))
            );
            CREATE INDEX IF NOT EXISTS idx_project_aliases_project ON project_aliases(project_id);
            CREATE TABLE IF NOT EXISTS project_bindings (
                id TEXT PRIMARY KEY, project_id TEXT NOT NULL REFERENCES project_identities(id),
                root_path TEXT NOT NULL, device TEXT NOT NULL, inode TEXT NOT NULL,
                state TEXT NOT NULL CHECK(state IN ('active','superseded')),
                created_at TEXT NOT NULL DEFAULT (datetime('now'))
            );
            CREATE UNIQUE INDEX IF NOT EXISTS project_active_root ON project_bindings(root_path) WHERE state='active';
            CREATE TABLE IF NOT EXISTS store_metadata (
				key        TEXT PRIMARY KEY,
				value      TEXT NOT NULL,
				created_at TEXT NOT NULL DEFAULT (datetime('now')),
				updated_at TEXT NOT NULL DEFAULT (datetime('now'))
			);
		`
	if _, err := s.execHook(s.db, schema); err != nil {
		return err
	}
	if _, err := s.execHook(s.db, `
		INSERT OR IGNORE INTO store_metadata (key, value)
		VALUES ('store_id', 'store-' || lower(hex(randomblob(16))))
	`); err != nil {
		return fmt.Errorf("initialize store identity: %w", err)
	}

	observationColumns := []struct {
		name       string
		definition string
	}{
		{name: "sync_id", definition: "TEXT"},
		{name: "scope", definition: "TEXT NOT NULL DEFAULT 'project'"},
		{name: "memory_class", definition: "TEXT NOT NULL DEFAULT 'project'"},
		{name: "memory_path", definition: "TEXT"},
		{name: "abstract", definition: "TEXT"},
		{name: "overview", definition: "TEXT"},
		{name: "summary_state", definition: "TEXT NOT NULL DEFAULT 'missing'"},
		{name: "summary_revision", definition: "INTEGER NOT NULL DEFAULT 0"},
		{name: "stale_at", definition: "TEXT"},
		{name: "stale_reason", definition: "TEXT"},
		{name: "topic_key", definition: "TEXT"},
		{name: "normalized_hash", definition: "TEXT"},
		{name: "revision_count", definition: "INTEGER NOT NULL DEFAULT 1"},
		{name: "duplicate_count", definition: "INTEGER NOT NULL DEFAULT 1"},
		{name: "last_seen_at", definition: "TEXT"},
		{name: "pinned", definition: "BOOLEAN NOT NULL DEFAULT 0"},
		{name: "recall_count", definition: "INTEGER NOT NULL DEFAULT 0"},
		{name: "last_recall_at", definition: "TEXT"},
		{name: "updated_at", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "deleted_at", definition: "TEXT"},
		{name: "archived_at", definition: "TEXT"},
		{name: "review_after", definition: "TEXT"},
	}
	for _, c := range observationColumns {
		if err := s.addColumnIfNotExists("observations", c.name, c.definition); err != nil {
			return err
		}
	}
	if err := s.ensureConsolidationTable(); err != nil {
		return err
	}

	// recall-order index depends on the columns above; create it only after
	// addColumnIfNotExists has run so legacy schemas gain the columns first.
	if _, err := s.execHook(s.db, `CREATE INDEX IF NOT EXISTS idx_obs_recall ON observations(recall_count, last_recall_at DESC)`); err != nil {
		return err
	}

	if err := s.migrateLegacyObservationsTable(); err != nil {
		return err
	}
	if err := s.backfillLegacyMemoryClasses(); err != nil {
		return err
	}
	if err := s.canonicalizeLegacyProjectKeys(); err != nil {
		return err
	}
	if err := s.purgeUnscopedReadBudgets(); err != nil {
		return err
	}
	if _, err := s.execHook(s.db, `
		UPDATE observations SET memory_path = NULL WHERE memory_path IS NOT NULL AND trim(memory_path) = '';
		UPDATE observations SET abstract = NULL WHERE abstract IS NOT NULL AND trim(abstract) = '';
		UPDATE observations SET overview = NULL WHERE overview IS NOT NULL AND trim(overview) = '';
		UPDATE observations
		SET summary_state = CASE
			WHEN trim(ifnull(summary_state, '')) IN ('fresh', 'stale') THEN summary_state
			WHEN abstract IS NOT NULL OR overview IS NOT NULL THEN 'fresh'
			ELSE 'missing'
		END
		WHERE trim(ifnull(summary_state, '')) NOT IN ('missing', 'fresh', 'stale')
		   OR (summary_state = 'missing' AND (abstract IS NOT NULL OR overview IS NOT NULL));
		UPDATE observations
		SET summary_revision = CASE
			WHEN summary_state = 'missing' THEN 0
			WHEN ifnull(summary_revision, 0) < 1 THEN 1
			ELSE summary_revision
		END
		WHERE (summary_state = 'missing' AND ifnull(summary_revision, 0) != 0)
		   OR (summary_state IN ('fresh', 'stale') AND ifnull(summary_revision, 0) < 1);

		CREATE TABLE IF NOT EXISTS memory_nodes (
			sync_id TEXT NOT NULL UNIQUE,
			project TEXT NOT NULL,
			scope TEXT NOT NULL DEFAULT 'project',
			path TEXT NOT NULL,
			parent_path TEXT,
			abstract TEXT,
			overview TEXT,
			content_revision INTEGER NOT NULL DEFAULT 0,
			summary_revision INTEGER NOT NULL DEFAULT 0,
			summary_state TEXT NOT NULL DEFAULT 'missing',
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at TEXT NOT NULL DEFAULT (datetime('now')),
			deleted_at TEXT,
			UNIQUE(project, scope, path)
		);

		CREATE TABLE IF NOT EXISTS memory_dependencies (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			sync_id TEXT NOT NULL UNIQUE,
			source_observation_sync_id TEXT NOT NULL,
			dependent_observation_sync_id TEXT NOT NULL,
			project TEXT NOT NULL,
			reason TEXT,
			source_revision INTEGER NOT NULL,
			status TEXT NOT NULL DEFAULT 'active',
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at TEXT NOT NULL DEFAULT (datetime('now')),
			CHECK(source_observation_sync_id != dependent_observation_sync_id),
			CHECK(status IN ('active', 'stale')),
			UNIQUE(source_observation_sync_id, dependent_observation_sync_id)
		);

		CREATE TABLE IF NOT EXISTS dependency_invalidation_queue (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_observation_sync_id TEXT NOT NULL,
			root_source_sync_id TEXT NOT NULL,
			root_source_revision INTEGER NOT NULL,
			reason TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			UNIQUE(source_observation_sync_id, root_source_sync_id)
		);

		CREATE TABLE IF NOT EXISTS resource_dependencies (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			sync_id TEXT NOT NULL UNIQUE,
			source_ref TEXT NOT NULL,
			dependent_observation_sync_id TEXT NOT NULL,
			project TEXT NOT NULL,
			reason TEXT,
			status TEXT NOT NULL DEFAULT 'active',
			replacement_source_ref TEXT,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at TEXT NOT NULL DEFAULT (datetime('now')),
			CHECK(project != ''),
			CHECK(status IN ('active', 'stale')),
			UNIQUE(source_ref, dependent_observation_sync_id, project)
		);

		CREATE TABLE IF NOT EXISTS memory_candidates (
			sync_id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			project TEXT NOT NULL,
			category TEXT NOT NULL,
			statement TEXT NOT NULL DEFAULT '',
			evidence_refs TEXT NOT NULL DEFAULT '[]',
			confidence REAL NOT NULL DEFAULT 0,
			memory_path TEXT,
			proposed_topic_key TEXT,
			normalized_hash TEXT,
			status TEXT NOT NULL,
			rejection_reason TEXT,
			promoted_observation_sync_id TEXT,
			created_at TEXT NOT NULL,
			expires_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			CHECK(status IN ('review', 'promoted', 'rejected', 'expired')),
			FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
		);

		CREATE TABLE IF NOT EXISTS memory_candidate_audit (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			candidate_sync_id TEXT NOT NULL,
			session_id TEXT NOT NULL,
			project TEXT NOT NULL,
			category TEXT NOT NULL,
			action TEXT NOT NULL,
			reason TEXT,
			created_at TEXT NOT NULL
		);
	`); err != nil {
		return err
	}

	if err := s.addColumnIfNotExists("user_prompts", "sync_id", "TEXT"); err != nil {
		return err
	}

	// Session provenance (host hook vs operator CLI vs model tool). Older rows
	// stay "unknown"; nothing is upgraded retroactively.
	if err := s.addColumnIfNotExists("sessions", "origin", "TEXT NOT NULL DEFAULT 'unknown'"); err != nil {
		return err
	}

	if _, err := s.execHook(s.db, `
		CREATE INDEX IF NOT EXISTS idx_obs_scope ON observations(scope);
		CREATE INDEX IF NOT EXISTS idx_obs_memory_class ON observations(memory_class, project, scope, updated_at DESC);
		CREATE INDEX IF NOT EXISTS idx_obs_memory_path ON observations(project, scope, memory_path, updated_at DESC);
		CREATE INDEX IF NOT EXISTS idx_obs_sync_id ON observations(sync_id);
		CREATE INDEX IF NOT EXISTS idx_obs_topic ON observations(topic_key, project, scope, updated_at DESC);
		CREATE INDEX IF NOT EXISTS idx_obs_deleted ON observations(deleted_at);
		CREATE INDEX IF NOT EXISTS idx_obs_dedupe ON observations(normalized_hash, project, scope, type, title, created_at DESC);
		CREATE INDEX IF NOT EXISTS idx_prompts_sync_id ON user_prompts(sync_id);
		CREATE INDEX IF NOT EXISTS idx_prompt_tombstones_project ON prompt_tombstones(project, deleted_at DESC);
		CREATE INDEX IF NOT EXISTS idx_sync_mutations_target_seq ON sync_mutations(target_key, seq);
		CREATE INDEX IF NOT EXISTS idx_sync_mutations_pending ON sync_mutations(target_key, acked_at, seq);
		CREATE INDEX IF NOT EXISTS idx_memory_nodes_parent ON memory_nodes(project, scope, parent_path, deleted_at);
		CREATE INDEX IF NOT EXISTS idx_memory_dependencies_source ON memory_dependencies(source_observation_sync_id, status);
		CREATE INDEX IF NOT EXISTS idx_memory_dependencies_dependent ON memory_dependencies(dependent_observation_sync_id, status);
		CREATE INDEX IF NOT EXISTS idx_memory_dependencies_project ON memory_dependencies(project, status, updated_at DESC);
		CREATE INDEX IF NOT EXISTS idx_dependency_invalidation_queue_created ON dependency_invalidation_queue(created_at, id);
		CREATE INDEX IF NOT EXISTS idx_resource_dependencies_source ON resource_dependencies(project, source_ref, status);
		CREATE INDEX IF NOT EXISTS idx_resource_dependencies_dependent ON resource_dependencies(dependent_observation_sync_id, status);
		CREATE INDEX IF NOT EXISTS idx_resource_dependencies_project ON resource_dependencies(project, status, updated_at DESC);
		CREATE INDEX IF NOT EXISTS idx_memory_candidates_review ON memory_candidates(project, status, expires_at, created_at);
		CREATE INDEX IF NOT EXISTS idx_memory_candidates_dedupe ON memory_candidates(project, category, normalized_hash, expires_at);
		CREATE INDEX IF NOT EXISTS idx_memory_candidate_audit_created ON memory_candidate_audit(project, created_at DESC, id DESC);
	`); err != nil {
		return err
	}
	if err := s.repairCandidateLineEndings(); err != nil {
		return err
	}

	// Project-scoped sync: add project column to sync_mutations and enrollment table.
	if err := s.addColumnIfNotExists("sync_mutations", "project", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.addColumnIfNotExists("sync_state", "reason_code", "TEXT"); err != nil {
		return err
	}
	if err := s.addColumnIfNotExists("sync_state", "reason_message", "TEXT"); err != nil {
		return err
	}
	if err := s.migrateSyncChunksTable(); err != nil {
		return err
	}

	// ── Phase: memory-conflict-surfacing — B.1 ──────────────────────────────
	// Additive nullable columns on observations for conflict surfacing, decay,
	// and embedding reservation.  All applied via addColumnIfNotExists so that
	// running migrate() on a fresh DB (where CREATE TABLE already added these
	// columns) is a no-op.
	memConflictObsCols := []struct {
		name       string
		definition string
	}{
		{name: "review_after", definition: "TEXT"},
		{name: "expires_at", definition: "TEXT"},
		{name: "embedding", definition: "BLOB"},
		{name: "embedding_model", definition: "TEXT"},
		{name: "embedding_created_at", definition: "TEXT"},
	}
	for _, c := range memConflictObsCols {
		if err := s.addColumnIfNotExists("observations", c.name, c.definition); err != nil {
			return err
		}
	}

	// ── Phase: memory-conflict-surfacing — B.2 ──────────────────────────────
	// Create the memory_relations table (idempotent via IF NOT EXISTS).
	// source_id / target_id are TEXT sync_id keys (cross-machine portable).
	// NO UNIQUE on (source_id, target_id) — multi-actor disagreement allowed.
	if _, err := s.execHook(s.db, `
		CREATE TABLE IF NOT EXISTS memory_relations (
			id                        INTEGER PRIMARY KEY AUTOINCREMENT,
			sync_id                   TEXT    NOT NULL UNIQUE,
			source_id                 TEXT,
			target_id                 TEXT,
			relation                  TEXT    NOT NULL DEFAULT 'pending',
			reason                    TEXT,
			evidence                  TEXT,
			confidence                REAL,
			judgment_status           TEXT    NOT NULL DEFAULT 'pending',
			marked_by_actor           TEXT,
			marked_by_kind            TEXT,
			marked_by_model           TEXT,
			session_id                TEXT,
			superseded_at             TEXT,
			superseded_by_relation_id INTEGER REFERENCES memory_relations(id) ON DELETE SET NULL,
			created_at                TEXT    NOT NULL DEFAULT (datetime('now')),
			updated_at                TEXT    NOT NULL DEFAULT (datetime('now'))
		);
	`); err != nil {
		return err
	}

	// ── Phase: memory-conflict-surfacing — B.3 ──────────────────────────────
	// Indexes for memory_relations (all idempotent via IF NOT EXISTS).
	if _, err := s.execHook(s.db, `
		CREATE INDEX IF NOT EXISTS idx_memrel_source    ON memory_relations(source_id, judgment_status);
		CREATE INDEX IF NOT EXISTS idx_memrel_target    ON memory_relations(target_id, judgment_status);
		CREATE INDEX IF NOT EXISTS idx_memrel_supersede ON memory_relations(superseded_by_relation_id);
	`); err != nil {
		return err
	}

	if _, err := s.execHook(s.db, `
		CREATE TABLE IF NOT EXISTS sync_enrolled_projects (
			project     TEXT PRIMARY KEY,
			enrolled_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE INDEX IF NOT EXISTS idx_sync_mutations_project ON sync_mutations(project);
	`); err != nil {
		return err
	}
	// Backfill: extract project from JSON payload for existing rows with empty project.
	if _, err := s.execHook(s.db, `
		UPDATE sync_mutations
		SET project = COALESCE(json_extract(payload, '$.project'), '')
		WHERE project = '' AND payload != ''
	`); err != nil {
		return err
	}
	if _, err := s.execHook(s.db, `
		UPDATE sync_mutations
		SET project = COALESCE((
			SELECT sessions.project
			FROM sessions
			WHERE sessions.id = json_extract(sync_mutations.payload, '$.session_id')
		), '')
		WHERE project = ''
		  AND payload != ''
		  AND ifnull(json_extract(payload, '$.session_id'), '') != ''
	`); err != nil {
		return err
	}

	if _, err := s.execHook(s.db, `UPDATE observations SET scope = 'project' WHERE scope IS NULL OR scope = ''`); err != nil {
		return err
	}
	if _, err := s.execHook(s.db, `UPDATE observations SET topic_key = NULL WHERE topic_key = ''`); err != nil {
		return err
	}
	if _, err := s.execHook(s.db, `UPDATE observations SET revision_count = 1 WHERE revision_count IS NULL OR revision_count < 1`); err != nil {
		return err
	}
	if _, err := s.execHook(s.db, `UPDATE observations SET duplicate_count = 1 WHERE duplicate_count IS NULL OR duplicate_count < 1`); err != nil {
		return err
	}
	if _, err := s.execHook(s.db, `UPDATE observations SET updated_at = created_at WHERE updated_at IS NULL OR updated_at = ''`); err != nil {
		return err
	}
	if _, err := s.execHook(s.db, `UPDATE observations SET sync_id = 'obs-' || lower(hex(randomblob(16))) WHERE sync_id IS NULL OR sync_id = ''`); err != nil {
		return err
	}
	if err := s.rebuildAllMemoryNodes(); err != nil {
		return fmt.Errorf("rebuild memory nodes: %w", err)
	}

	if _, err := s.execHook(s.db, `UPDATE user_prompts SET project = '' WHERE project IS NULL`); err != nil {
		return err
	}
	if _, err := s.execHook(s.db, `UPDATE prompt_tombstones SET project = '' WHERE project IS NULL`); err != nil {
		return err
	}
	if _, err := s.execHook(s.db, `UPDATE user_prompts SET sync_id = 'prompt-' || lower(hex(randomblob(16))) WHERE sync_id IS NULL OR sync_id = ''`); err != nil {
		return err
	}
	if _, err := s.execHook(s.db, `INSERT OR IGNORE INTO sync_state (target_key, lifecycle, updated_at) VALUES ('cloud', 'idle', datetime('now'))`); err != nil {
		return err
	}
	if _, err := s.execHook(s.db, `
		CREATE INDEX IF NOT EXISTS idx_cloud_upgrade_state_stage ON cloud_upgrade_state(stage);
		CREATE INDEX IF NOT EXISTS idx_sync_mutations_lookup ON sync_mutations(target_key, entity, entity_key, source);
	`); err != nil {
		return err
	}

	// Create triggers to keep FTS in sync (idempotent check)
	var name string
	err := s.db.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='trigger' AND name='obs_fts_insert'",
	).Scan(&name)

	if err == sql.ErrNoRows {
		triggers := `
			CREATE TRIGGER obs_fts_insert AFTER INSERT ON observations BEGIN
				INSERT INTO observations_fts(rowid, title, content, tool_name, type, project, topic_key)
				VALUES (new.id, new.title, new.content, new.tool_name, new.type, new.project, new.topic_key);
			END;

			CREATE TRIGGER obs_fts_delete AFTER DELETE ON observations BEGIN
				INSERT INTO observations_fts(observations_fts, rowid, title, content, tool_name, type, project, topic_key)
				VALUES ('delete', old.id, old.title, old.content, old.tool_name, old.type, old.project, old.topic_key);
			END;

			CREATE TRIGGER obs_fts_update AFTER UPDATE ON observations BEGIN
				INSERT INTO observations_fts(observations_fts, rowid, title, content, tool_name, type, project, topic_key)
				VALUES ('delete', old.id, old.title, old.content, old.tool_name, old.type, old.project, old.topic_key);
				INSERT INTO observations_fts(rowid, title, content, tool_name, type, project, topic_key)
				VALUES (new.id, new.title, new.content, new.tool_name, new.type, new.project, new.topic_key);
			END;
		`
		if _, err := s.execHook(s.db, triggers); err != nil {
			return err
		}
	}

	if err := s.migrateFTSTopicKey(); err != nil {
		return err
	}

	// Prompts FTS triggers (separate idempotent check)
	var promptTrigger string
	err = s.db.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='trigger' AND name='prompt_fts_insert'",
	).Scan(&promptTrigger)

	if err == sql.ErrNoRows {
		promptTriggers := `
			CREATE TRIGGER prompt_fts_insert AFTER INSERT ON user_prompts BEGIN
				INSERT INTO prompts_fts(rowid, content, project)
				VALUES (new.id, new.content, new.project);
			END;

			CREATE TRIGGER prompt_fts_delete AFTER DELETE ON user_prompts BEGIN
				INSERT INTO prompts_fts(prompts_fts, rowid, content, project)
				VALUES ('delete', old.id, old.content, old.project);
			END;

			CREATE TRIGGER prompt_fts_update AFTER UPDATE ON user_prompts BEGIN
				INSERT INTO prompts_fts(prompts_fts, rowid, content, project)
				VALUES ('delete', old.id, old.content, old.project);
				INSERT INTO prompts_fts(rowid, content, project)
				VALUES (new.id, new.content, new.project);
			END;
		`
		if _, err := s.execHook(s.db, promptTriggers); err != nil {
			return err
		}
	}

	// Phase 3: add republish CLI, surface dead rows via mem_status.
	if _, err := s.execHook(s.db, `
		CREATE TABLE IF NOT EXISTS sync_apply_deferred (
			sync_id           TEXT    PRIMARY KEY,
			entity            TEXT    NOT NULL,
			payload           TEXT    NOT NULL,
			apply_status      TEXT    NOT NULL DEFAULT 'deferred',
			retry_count       INTEGER NOT NULL DEFAULT 0,
			last_error        TEXT,
			last_attempted_at TEXT,
			first_seen_at     TEXT    NOT NULL DEFAULT (datetime('now'))
		);
		CREATE INDEX IF NOT EXISTS idx_sad_status_seen
			ON sync_apply_deferred(apply_status, first_seen_at);
	`); err != nil {
		return err
	}

	// Phase 3b: composite index for conflict-audit list/count queries.
	if _, err := s.execHook(s.db, `
		CREATE INDEX IF NOT EXISTS idx_memrel_status_created
			ON memory_relations(judgment_status, created_at DESC);
	`); err != nil {
		return err
	}

	if err := s.installObservationHistory(); err != nil {
		return err
	}
	if err := s.installProjectRetirement(); err != nil {
		return err
	}
	return s.normalizeStoredTimestamps()
}

const memoryClassBackfillMarker = "migration.memory_class.v1"

func (s *Store) backfillLegacyMemoryClasses() error {
	tx, err := s.beginTxHook()
	if err != nil {
		return fmt.Errorf("memory_class backfill: begin: %w", err)
	}
	defer tx.Rollback()

	var marker string
	err = tx.QueryRow(`SELECT value FROM store_metadata WHERE key = ?`, memoryClassBackfillMarker).Scan(&marker)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("memory_class backfill: read marker: %w", err)
	}

	if _, err := s.execHook(tx, `
		UPDATE observations
		SET memory_class = CASE
			WHEN lower(trim(type)) IN ('checkpoint', 'session_summary', 'summary') THEN 'operational'
			WHEN lower(trim(type)) IN ('preference', 'user_fact') THEN 'user'
			WHEN lower(trim(type)) IN ('learning', 'pattern', 'procedure', 'experience') THEN 'agent'
			ELSE 'project'
		END
	`); err != nil {
		return fmt.Errorf("memory_class backfill: classify observations: %w", err)
	}
	if _, err := s.execHook(tx, `INSERT INTO store_metadata (key, value) VALUES (?, 'done')`, memoryClassBackfillMarker); err != nil {
		return fmt.Errorf("memory_class backfill: write marker: %w", err)
	}
	if err := s.commitHook(tx); err != nil {
		return fmt.Errorf("memory_class backfill: commit: %w", err)
	}
	return nil
}

func (s *Store) SaveCloudUpgradeState(state CloudUpgradeState) error {
	project, _ := NormalizeProject(state.Project)
	project = strings.TrimSpace(project)
	if project == "" {
		return fmt.Errorf("cloud upgrade project must not be empty")
	}
	state.Project = project
	state.Stage = normalizeUpgradeStage(state.Stage)
	state.RepairClass = normalizeUpgradeRepairClass(state.RepairClass)

	snapshotJSON, err := json.Marshal(state.Snapshot)
	if err != nil {
		return fmt.Errorf("marshal cloud upgrade snapshot: %w", err)
	}

	_, err = s.execHook(s.db, `
		INSERT INTO cloud_upgrade_state (
			project, stage, repair_class, snapshot_json, last_error_code, last_error_message, findings_json, applied_actions, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT(project) DO UPDATE SET
			stage = excluded.stage,
			repair_class = excluded.repair_class,
			snapshot_json = excluded.snapshot_json,
			last_error_code = excluded.last_error_code,
			last_error_message = excluded.last_error_message,
			findings_json = excluded.findings_json,
			applied_actions = excluded.applied_actions,
			updated_at = datetime('now')
	`, state.Project, state.Stage, state.RepairClass, string(snapshotJSON), nullableString(state.LastErrorCode), nullableString(state.LastErrorMessage), nullableString(state.FindingsJSON), nullableString(state.AppliedActions))
	return err
}

func (s *Store) GetCloudUpgradeState(project string) (*CloudUpgradeState, error) {
	project, _ = NormalizeProject(project)
	project = strings.TrimSpace(project)
	if project == "" {
		return nil, nil
	}

	row := s.db.QueryRow(`
		SELECT project, stage, repair_class, snapshot_json, ifnull(last_error_code, ''), ifnull(last_error_message, ''), ifnull(findings_json, ''), ifnull(applied_actions, ''), updated_at
		FROM cloud_upgrade_state
		WHERE project = ?
	`, project)

	var state CloudUpgradeState
	var snapshotJSON string
	if err := row.Scan(&state.Project, &state.Stage, &state.RepairClass, &snapshotJSON, &state.LastErrorCode, &state.LastErrorMessage, &state.FindingsJSON, &state.AppliedActions, &state.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if strings.TrimSpace(snapshotJSON) != "" {
		if err := json.Unmarshal([]byte(snapshotJSON), &state.Snapshot); err != nil {
			return nil, fmt.Errorf("parse cloud upgrade snapshot: %w", err)
		}
	}
	state.Stage = normalizeUpgradeStage(state.Stage)
	state.RepairClass = normalizeUpgradeRepairClass(state.RepairClass)
	return &state, nil
}

func (s *Store) ClearCloudUpgradeState(project string) error {
	project, _ = NormalizeProject(project)
	project = strings.TrimSpace(project)
	if project == "" {
		return nil
	}
	_, err := s.execHook(s.db, `DELETE FROM cloud_upgrade_state WHERE project = ?`, project)
	return err
}

func (s *Store) CanRollbackCloudUpgrade(project string) (bool, error) {
	state, err := s.GetCloudUpgradeState(project)
	if err != nil {
		return false, err
	}
	if state == nil {
		return false, nil
	}
	return state.Stage != UpgradeStageBootstrapVerified, nil
}

func (s *Store) RollbackCloudUpgrade(project string) (CloudUpgradeState, error) {
	project, _ = NormalizeProject(project)
	project = strings.TrimSpace(project)
	if project == "" {
		return CloudUpgradeState{}, fmt.Errorf("cloud upgrade rollback requires project")
	}

	state, err := s.GetCloudUpgradeState(project)
	if err != nil {
		return CloudUpgradeState{}, fmt.Errorf("read cloud upgrade rollback state: %w", err)
	}
	if state == nil {
		return CloudUpgradeState{}, fmt.Errorf("rollback requires existing upgrade checkpoint state")
	}
	if state.Stage == UpgradeStageBootstrapVerified {
		return CloudUpgradeState{}, fmt.Errorf("rollback is unavailable post-bootstrap; use explicit disconnect/unenroll flows")
	}

	if state.Snapshot.ProjectEnrolled {
		if err := s.EnrollProject(project); err != nil {
			return CloudUpgradeState{}, fmt.Errorf("restore project enrollment from rollback snapshot: %w", err)
		}
	} else {
		if err := s.UnenrollProject(project); err != nil {
			return CloudUpgradeState{}, fmt.Errorf("restore project unenrollment from rollback snapshot: %w", err)
		}
	}

	state.Stage = UpgradeStageRolledBack
	state.LastErrorCode = ""
	state.LastErrorMessage = ""
	state.FindingsJSON = ""
	state.AppliedActions = ""
	if err := s.SaveCloudUpgradeState(*state); err != nil {
		return CloudUpgradeState{}, fmt.Errorf("persist rolled back upgrade state: %w", err)
	}

	rolledBack, err := s.GetCloudUpgradeState(project)
	if err != nil {
		return CloudUpgradeState{}, fmt.Errorf("load rolled back cloud upgrade state: %w", err)
	}
	if rolledBack == nil {
		return CloudUpgradeState{}, fmt.Errorf("rolled back cloud upgrade state not found")
	}
	return *rolledBack, nil
}

func (s *Store) RepairCloudUpgrade(project string, apply bool) (CloudUpgradeRepairReport, error) {
	project, _ = NormalizeProject(project)
	project = strings.TrimSpace(project)
	if project == "" {
		return CloudUpgradeRepairReport{
			Class:      UpgradeRepairClassBlocked,
			ReasonCode: "upgrade_blocked_project_required",
			Message:    "project is required for cloud upgrade repair",
		}, nil
	}

	if blocked, report, err := s.cloudUpgradeManualActionReport(project); err != nil {
		return CloudUpgradeRepairReport{}, err
	} else if blocked {
		return report, nil
	}

	enrolled, err := s.IsProjectEnrolled(project)
	if err != nil {
		return CloudUpgradeRepairReport{}, fmt.Errorf("check project enrollment: %w", err)
	}
	if !enrolled {
		return CloudUpgradeRepairReport{
			Class:      UpgradeRepairClassBlocked,
			ReasonCode: "upgrade_blocked_manual",
			Message:    fmt.Sprintf("project %q is not enrolled; run doctor/bootstrap guidance first", project),
		}, nil
	}

	legacyReport, err := s.DiagnoseCloudUpgradeLegacyMutations(project)
	if err != nil {
		return CloudUpgradeRepairReport{}, fmt.Errorf("diagnose legacy cloud upgrade mutations: %w", err)
	}
	// When both repairable and blocked mutations coexist we must not hold the
	// entire repair pass hostage to the non-repairable entries. Apply the
	// repairable subset first, then surface the residual blockers.
	appliedRepairs := false
	if apply && legacyReport.RepairableCount > 0 {
		if err := s.applyCloudUpgradeLegacyMutationRepairs(project); err != nil {
			return CloudUpgradeRepairReport{}, fmt.Errorf("apply cloud upgrade legacy mutation repairs: %w", err)
		}
		appliedRepairs = true
	}

	if legacyReport.BlockedCount > 0 {
		// Scan for the first non-repairable finding so the operator sees the
		// actual blocker. Findings[0] is ordered by seq and may be a repairable
		// entry, which previously produced a misleading error message (#446).
		var blocked CloudUpgradeLegacyMutationFinding
		for _, f := range legacyReport.Findings {
			if !f.Repairable {
				blocked = f
				break
			}
		}
		var msg string
		switch {
		case appliedRepairs:
			msg = fmt.Sprintf("applied %d repairable payload(s); %d remain blocked: manual-action-required: %s (seq=%d entity=%s entity_key=%q op=%s)",
				legacyReport.RepairableCount, legacyReport.BlockedCount, blocked.Message, blocked.Seq, blocked.Entity, blocked.EntityKey, blocked.Op)
		case legacyReport.RepairableCount > 0:
			msg = fmt.Sprintf("%d repairable payload(s) would apply; %d would remain blocked: manual-action-required: %s (seq=%d entity=%s entity_key=%q op=%s)",
				legacyReport.RepairableCount, legacyReport.BlockedCount, blocked.Message, blocked.Seq, blocked.Entity, blocked.EntityKey, blocked.Op)
		default:
			msg = fmt.Sprintf("manual-action-required: %s (seq=%d entity=%s entity_key=%q op=%s)",
				blocked.Message, blocked.Seq, blocked.Entity, blocked.EntityKey, blocked.Op)
		}
		return CloudUpgradeRepairReport{
			Class:      UpgradeRepairClassBlocked,
			ReasonCode: UpgradeReasonBlockedLegacyMutationManual,
			Message:    msg,
			Applied:    appliedRepairs,
		}, nil
	}

	if legacyReport.RepairableCount > 0 {
		if !appliedRepairs {
			return CloudUpgradeRepairReport{
				Class:         UpgradeRepairClassRepairable,
				ReasonCode:    UpgradeReasonRepairableLegacyMutationPayload,
				Message:       fmt.Sprintf("project %q has %d repairable legacy mutation payload issue(s)", project, legacyReport.RepairableCount),
				PlannedAction: "repair_legacy_mutation_payloads",
				Applied:       false,
			}, nil
		}
		_ = s.SaveCloudUpgradeState(CloudUpgradeState{
			Project:     project,
			Stage:       UpgradeStageRepairApplied,
			RepairClass: UpgradeRepairClassRepairable,
		})
		return CloudUpgradeRepairReport{
			Class:         UpgradeRepairClassRepairable,
			ReasonCode:    UpgradeReasonRepairableLegacyMutationPayload,
			Message:       fmt.Sprintf("applied deterministic legacy mutation payload repairs for project %q", project),
			PlannedAction: "repair_legacy_mutation_payloads",
			Applied:       true,
		}, nil
	}

	requiresBackfill, err := s.projectSyncBackfillRequired(project)
	if err != nil {
		return CloudUpgradeRepairReport{}, err
	}
	if !requiresBackfill {
		return CloudUpgradeRepairReport{
			Class:      UpgradeRepairClassReady,
			ReasonCode: "upgrade_repair_noop",
			Message:    fmt.Sprintf("project %q has no deterministic local repairs to apply", project),
		}, nil
	}

	report := CloudUpgradeRepairReport{
		Class:         UpgradeRepairClassRepairable,
		ReasonCode:    "upgrade_repair_backfill_sync_journal",
		Message:       fmt.Sprintf("project %q has deterministic local sync metadata gaps", project),
		PlannedAction: "backfill_sync_journal",
		Applied:       false,
	}
	if !apply {
		return report, nil
	}

	if err := s.withTx(func(tx *sql.Tx) error {
		return s.backfillProjectSyncMutationsTx(tx, project)
	}); err != nil {
		return CloudUpgradeRepairReport{}, fmt.Errorf("apply cloud upgrade repair: %w", err)
	}
	report.Applied = true
	_ = s.SaveCloudUpgradeState(CloudUpgradeState{
		Project:     project,
		Stage:       UpgradeStageRepairApplied,
		RepairClass: UpgradeRepairClassRepairable,
	})
	return report, nil
}

type cloudUpgradeLegacyMutationEvaluation struct {
	finding         CloudUpgradeLegacyMutationFinding
	hasIssue        bool
	repairedPayload string
	canRepair       bool
}

func (s *Store) DiagnoseCloudUpgradeLegacyMutations(project string) (CloudUpgradeLegacyMutationReport, error) {
	project, _ = NormalizeProject(project)
	project = strings.TrimSpace(project)
	if project == "" {
		return CloudUpgradeLegacyMutationReport{Project: project}, nil
	}

	evaluations, err := s.evaluateCloudUpgradeLegacyMutations(project)
	if err != nil {
		return CloudUpgradeLegacyMutationReport{}, err
	}
	report := CloudUpgradeLegacyMutationReport{Project: project}
	for _, eval := range evaluations {
		if !eval.hasIssue {
			continue
		}
		report.Findings = append(report.Findings, eval.finding)
		if eval.canRepair {
			report.RepairableCount++
		} else {
			report.BlockedCount++
		}
	}
	return report, nil
}

func (s *Store) applyCloudUpgradeLegacyMutationRepairs(project string) error {
	project, _ = NormalizeProject(project)
	project = strings.TrimSpace(project)
	if project == "" {
		return nil
	}
	return s.withTx(func(tx *sql.Tx) error {
		mutations, err := s.listPendingProjectMutationsTx(tx, project)
		if err != nil {
			return err
		}
		for _, mutation := range mutations {
			eval, err := s.evaluateCloudUpgradeLegacyMutationTx(tx, mutation)
			if err != nil {
				return err
			}
			if !eval.hasIssue || !eval.canRepair || strings.TrimSpace(eval.repairedPayload) == "" {
				continue
			}
			if _, err := s.execHook(tx,
				`UPDATE sync_mutations SET payload = ? WHERE target_key = ? AND project = ? AND seq = ? AND acked_at IS NULL`,
				eval.repairedPayload,
				DefaultSyncTargetKey,
				project,
				mutation.Seq,
			); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) evaluateCloudUpgradeLegacyMutations(project string) ([]cloudUpgradeLegacyMutationEvaluation, error) {
	return s.withReadTx(func(tx *sql.Tx) ([]cloudUpgradeLegacyMutationEvaluation, error) {
		mutations, err := s.listPendingProjectMutationsTx(tx, project)
		if err != nil {
			return nil, err
		}
		evaluations := make([]cloudUpgradeLegacyMutationEvaluation, 0, len(mutations))
		for _, mutation := range mutations {
			eval, err := s.evaluateCloudUpgradeLegacyMutationTx(tx, mutation)
			if err != nil {
				return nil, err
			}
			evaluations = append(evaluations, eval)
		}
		return evaluations, nil
	})
}

func (s *Store) withReadTx(fn func(tx *sql.Tx) ([]cloudUpgradeLegacyMutationEvaluation, error)) ([]cloudUpgradeLegacyMutationEvaluation, error) {
	tx, err := s.beginTxHook()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	return fn(tx)
}

func (s *Store) listPendingProjectMutationsTx(tx *sql.Tx, project string) ([]SyncMutation, error) {
	rows, err := s.queryItHook(tx, `
		SELECT seq, target_key, entity, entity_key, op, payload, source, project, occurred_at, acked_at
		FROM sync_mutations
		WHERE target_key = ? AND project = ? AND acked_at IS NULL
		ORDER BY seq ASC
	`, DefaultSyncTargetKey, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	mutations := make([]SyncMutation, 0)
	for rows.Next() {
		var m SyncMutation
		if err := rows.Scan(&m.Seq, &m.TargetKey, &m.Entity, &m.EntityKey, &m.Op, &m.Payload, &m.Source, &m.Project, &m.OccurredAt, &m.AckedAt); err != nil {
			return nil, err
		}
		mutations = append(mutations, m)
	}
	return mutations, rows.Err()
}

func (s *Store) evaluateCloudUpgradeLegacyMutationTx(tx *sql.Tx, mutation SyncMutation) (cloudUpgradeLegacyMutationEvaluation, error) {
	entity := strings.TrimSpace(mutation.Entity)
	op := strings.TrimSpace(mutation.Op)
	payload := strings.TrimSpace(mutation.Payload)
	base := CloudUpgradeLegacyMutationFinding{
		Seq:       mutation.Seq,
		Entity:    entity,
		Op:        op,
		EntityKey: strings.TrimSpace(mutation.EntityKey),
		TargetKey: strings.TrimSpace(mutation.TargetKey),
		Project:   strings.TrimSpace(mutation.Project),
	}

	repairable := func(msg, hint string, repairedPayload string) cloudUpgradeLegacyMutationEvaluation {
		finding := base
		finding.Repairable = true
		finding.ReasonCode = UpgradeReasonRepairableLegacyMutationPayload
		finding.Message = msg
		finding.RepairHint = hint
		return cloudUpgradeLegacyMutationEvaluation{finding: finding, hasIssue: true, canRepair: true, repairedPayload: repairedPayload}
	}
	blocked := func(code, msg string) cloudUpgradeLegacyMutationEvaluation {
		finding := base
		finding.Repairable = false
		finding.ReasonCode = code
		finding.Message = msg
		return cloudUpgradeLegacyMutationEvaluation{finding: finding, hasIssue: true, canRepair: false}
	}

	if payload == "" {
		return blocked(UpgradeReasonBlockedLegacyMutationManual, "legacy mutation payload is empty"), nil
	}

	supported := (entity == SyncEntitySession && (op == SyncOpUpsert || op == SyncOpDelete)) ||
		((entity == SyncEntityObservation || entity == SyncEntityPrompt) && (op == SyncOpUpsert || op == SyncOpDelete)) ||
		((entity == SyncEntityRelation || entity == SyncEntityDependency) && op == SyncOpUpsert)
	if !supported {
		return blocked(UpgradeReasonBlockedLegacyMutationManual, fmt.Sprintf("unsupported legacy mutation %q/%q", entity, op)), nil
	}

	switch entity {
	case SyncEntitySession:
		var body syncSessionPayload
		if err := decodeSyncPayload([]byte(payload), &body); err != nil {
			return blocked(UpgradeReasonBlockedLegacyMutationManual, fmt.Sprintf("decode session payload: %v", err)), nil
		}
		body.ID = strings.TrimSpace(body.ID)
		body.Directory = strings.TrimSpace(body.Directory)
		changed := false
		if body.ID == "" && strings.TrimSpace(mutation.EntityKey) != "" {
			body.ID = strings.TrimSpace(mutation.EntityKey)
			changed = true
		}
		if body.ID == "" {
			return blocked(UpgradeReasonBlockedLegacyMutationManual, "session payload id is required"), nil
		}
		if strings.TrimSpace(mutation.EntityKey) != "" && strings.TrimSpace(mutation.EntityKey) != body.ID {
			return blocked(UpgradeReasonBlockedLegacyMutationManual, fmt.Sprintf("session entity_key %q does not match payload id %q", mutation.EntityKey, body.ID)), nil
		}
		if op == SyncOpUpsert && body.Directory == "" {
			var directory string
			err := tx.QueryRow(`SELECT ifnull(directory, '') FROM sessions WHERE id = ?`, body.ID).Scan(&directory)
			if errors.Is(err, sql.ErrNoRows) || strings.TrimSpace(directory) == "" {
				return blocked(UpgradeReasonBlockedLegacyMutationManual, "session payload directory is required and cannot be inferred from local state"), nil
			}
			if err != nil {
				return cloudUpgradeLegacyMutationEvaluation{}, err
			}
			body.Directory = strings.TrimSpace(directory)
			changed = true
		}
		if !changed {
			return cloudUpgradeLegacyMutationEvaluation{}, nil
		}
		encoded, err := json.Marshal(body)
		if err != nil {
			return cloudUpgradeLegacyMutationEvaluation{}, err
		}
		return repairable("session payload is missing required fields", "repair fills session id/directory from local sessions table", string(encoded)), nil

	case SyncEntityObservation:
		var body syncObservationPayload
		if err := decodeSyncPayload([]byte(payload), &body); err != nil {
			return blocked(UpgradeReasonBlockedLegacyMutationManual, fmt.Sprintf("decode observation payload: %v", err)), nil
		}
		body.SyncID = strings.TrimSpace(body.SyncID)
		body.SessionID = strings.TrimSpace(body.SessionID)
		body.Type = strings.TrimSpace(body.Type)
		body.Title = strings.TrimSpace(body.Title)
		body.Content = strings.TrimSpace(body.Content)
		body.Scope = strings.TrimSpace(body.Scope)
		changed := false
		if body.SyncID == "" && strings.TrimSpace(mutation.EntityKey) != "" {
			body.SyncID = strings.TrimSpace(mutation.EntityKey)
			changed = true
		}
		if body.SyncID == "" {
			return blocked(UpgradeReasonBlockedLegacyMutationManual, "observation payload sync_id is required"), nil
		}
		if strings.TrimSpace(mutation.EntityKey) != "" && strings.TrimSpace(mutation.EntityKey) != body.SyncID {
			return blocked(UpgradeReasonBlockedLegacyMutationManual, fmt.Sprintf("observation entity_key %q does not match payload sync_id %q", mutation.EntityKey, body.SyncID)), nil
		}
		if op == SyncOpUpsert {
			obs, err := s.getObservationBySyncIDTx(tx, body.SyncID, true)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return cloudUpgradeLegacyMutationEvaluation{}, err
			}
			if strings.TrimSpace(body.SessionID) == "" && obs != nil && strings.TrimSpace(obs.SessionID) != "" {
				body.SessionID = strings.TrimSpace(obs.SessionID)
				changed = true
			}
			if strings.TrimSpace(body.Type) == "" && obs != nil && strings.TrimSpace(obs.Type) != "" {
				body.Type = strings.TrimSpace(obs.Type)
				changed = true
			}
			if strings.TrimSpace(body.Title) == "" && obs != nil && strings.TrimSpace(obs.Title) != "" {
				body.Title = strings.TrimSpace(obs.Title)
				changed = true
			}
			if strings.TrimSpace(body.Content) == "" && obs != nil && strings.TrimSpace(obs.Content) != "" {
				body.Content = strings.TrimSpace(obs.Content)
				changed = true
			}
			if strings.TrimSpace(body.Scope) == "" && obs != nil && strings.TrimSpace(obs.Scope) != "" {
				body.Scope = strings.TrimSpace(obs.Scope)
				changed = true
			}
			missing := []string{}
			if strings.TrimSpace(body.SessionID) == "" {
				missing = append(missing, "session_id")
			}
			if strings.TrimSpace(body.Type) == "" {
				missing = append(missing, "type")
			}
			if strings.TrimSpace(body.Title) == "" {
				missing = append(missing, "title")
			}
			if strings.TrimSpace(body.Content) == "" {
				missing = append(missing, "content")
			}
			if strings.TrimSpace(body.Scope) == "" {
				missing = append(missing, "scope")
			}
			if len(missing) > 0 {
				return blocked(UpgradeReasonBlockedLegacyMutationManual, fmt.Sprintf("observation payload missing required upsert fields: %s", strings.Join(missing, ", "))), nil
			}
		}
		if !changed {
			return cloudUpgradeLegacyMutationEvaluation{}, nil
		}
		encoded, err := json.Marshal(body)
		if err != nil {
			return cloudUpgradeLegacyMutationEvaluation{}, err
		}
		return repairable("observation payload is missing required fields for canonical bootstrap", "repair fills missing observation fields from local observations table", string(encoded)), nil

	case SyncEntityPrompt:
		var body syncPromptPayload
		if err := decodeSyncPayload([]byte(payload), &body); err != nil {
			return blocked(UpgradeReasonBlockedLegacyMutationManual, fmt.Sprintf("decode prompt payload: %v", err)), nil
		}
		body.SyncID = strings.TrimSpace(body.SyncID)
		body.SessionID = strings.TrimSpace(body.SessionID)
		body.Content = strings.TrimSpace(body.Content)
		changed := false
		if body.SyncID == "" && strings.TrimSpace(mutation.EntityKey) != "" {
			body.SyncID = strings.TrimSpace(mutation.EntityKey)
			changed = true
		}
		if body.SyncID == "" {
			return blocked(UpgradeReasonBlockedLegacyMutationManual, "prompt payload sync_id is required"), nil
		}
		if strings.TrimSpace(mutation.EntityKey) != "" && strings.TrimSpace(mutation.EntityKey) != body.SyncID {
			return blocked(UpgradeReasonBlockedLegacyMutationManual, fmt.Sprintf("prompt entity_key %q does not match payload sync_id %q", mutation.EntityKey, body.SyncID)), nil
		}
		if op == SyncOpUpsert {
			var local syncPromptPayload
			err := tx.QueryRow(
				`SELECT sync_id, session_id, content, project, created_at FROM user_prompts WHERE sync_id = ? ORDER BY id DESC LIMIT 1`,
				body.SyncID,
			).Scan(&local.SyncID, &local.SessionID, &local.Content, &local.Project, &local.CreatedAt)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return cloudUpgradeLegacyMutationEvaluation{}, err
			}
			if strings.TrimSpace(body.SessionID) == "" && err == nil && strings.TrimSpace(local.SessionID) != "" {
				body.SessionID = strings.TrimSpace(local.SessionID)
				changed = true
			}
			if strings.TrimSpace(body.Content) == "" && err == nil && strings.TrimSpace(local.Content) != "" {
				body.Content = strings.TrimSpace(local.Content)
				changed = true
			}
			missing := []string{}
			if strings.TrimSpace(body.SessionID) == "" {
				missing = append(missing, "session_id")
			}
			if strings.TrimSpace(body.Content) == "" {
				missing = append(missing, "content")
			}
			if len(missing) > 0 {
				return blocked(UpgradeReasonBlockedLegacyMutationManual, fmt.Sprintf("prompt payload missing required upsert fields: %s", strings.Join(missing, ", "))), nil
			}
		}
		if !changed {
			return cloudUpgradeLegacyMutationEvaluation{}, nil
		}
		encoded, err := json.Marshal(body)
		if err != nil {
			return cloudUpgradeLegacyMutationEvaluation{}, err
		}
		return repairable("prompt payload is missing required fields for canonical bootstrap", "repair fills missing prompt fields from local prompts table", string(encoded)), nil

	case SyncEntityRelation:
		var body syncRelationPayload
		if err := decodeSyncPayload([]byte(payload), &body); err != nil {
			return blocked(UpgradeReasonBlockedLegacyMutationManual, fmt.Sprintf("decode relation payload: %v", err)), nil
		}
		body.SyncID = strings.TrimSpace(body.SyncID)
		body.SourceID = strings.TrimSpace(body.SourceID)
		body.TargetID = strings.TrimSpace(body.TargetID)
		body.Relation = strings.TrimSpace(body.Relation)
		body.JudgmentStatus = strings.TrimSpace(body.JudgmentStatus)
		body.Project = strings.TrimSpace(body.Project)
		changed := false
		if body.SyncID == "" && strings.TrimSpace(mutation.EntityKey) != "" {
			body.SyncID = strings.TrimSpace(mutation.EntityKey)
			changed = true
		}
		if body.SyncID == "" {
			return blocked(UpgradeReasonBlockedLegacyMutationManual, "relation payload sync_id is required"), nil
		}
		if strings.TrimSpace(mutation.EntityKey) != "" && strings.TrimSpace(mutation.EntityKey) != body.SyncID {
			return blocked(UpgradeReasonBlockedLegacyMutationManual, fmt.Sprintf("relation entity_key %q does not match payload sync_id %q", mutation.EntityKey, body.SyncID)), nil
		}

		type localRelationPayload struct {
			SyncID         string
			SourceID       string
			TargetID       string
			Relation       string
			Reason         sql.NullString
			Evidence       sql.NullString
			Confidence     sql.NullFloat64
			JudgmentStatus string
			MarkedByActor  sql.NullString
			MarkedByKind   sql.NullString
			MarkedByModel  sql.NullString
			SessionID      sql.NullString
			CreatedAt      string
			UpdatedAt      string
		}
		var local localRelationPayload
		err := tx.QueryRow(`
			SELECT ifnull(sync_id, ''), ifnull(source_id, ''), ifnull(target_id, ''), ifnull(relation, ''),
			       reason, evidence, confidence, ifnull(judgment_status, ''), marked_by_actor,
			       marked_by_kind, marked_by_model, session_id, ifnull(created_at, ''), ifnull(updated_at, '')
			FROM memory_relations
			WHERE sync_id = ?
			ORDER BY id DESC LIMIT 1
		`, body.SyncID).Scan(
			&local.SyncID, &local.SourceID, &local.TargetID, &local.Relation,
			&local.Reason, &local.Evidence, &local.Confidence, &local.JudgmentStatus,
			&local.MarkedByActor, &local.MarkedByKind, &local.MarkedByModel, &local.SessionID,
			&local.CreatedAt, &local.UpdatedAt,
		)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return cloudUpgradeLegacyMutationEvaluation{}, err
		}
		if err == nil {
			if body.SourceID == "" && strings.TrimSpace(local.SourceID) != "" {
				body.SourceID = strings.TrimSpace(local.SourceID)
				changed = true
			}
			if body.TargetID == "" && strings.TrimSpace(local.TargetID) != "" {
				body.TargetID = strings.TrimSpace(local.TargetID)
				changed = true
			}
			if body.Relation == "" && strings.TrimSpace(local.Relation) != "" {
				body.Relation = strings.TrimSpace(local.Relation)
				changed = true
			}
			if body.Reason == nil && local.Reason.Valid {
				v := local.Reason.String
				body.Reason = &v
				changed = true
			}
			if body.Evidence == nil && local.Evidence.Valid {
				v := local.Evidence.String
				body.Evidence = &v
				changed = true
			}
			if body.Confidence == nil && local.Confidence.Valid {
				v := local.Confidence.Float64
				body.Confidence = &v
				changed = true
			}
			if body.JudgmentStatus == "" && strings.TrimSpace(local.JudgmentStatus) != "" {
				body.JudgmentStatus = strings.TrimSpace(local.JudgmentStatus)
				changed = true
			}
			if (body.MarkedByActor == nil || strings.TrimSpace(*body.MarkedByActor) == "") && local.MarkedByActor.Valid && strings.TrimSpace(local.MarkedByActor.String) != "" {
				v := strings.TrimSpace(local.MarkedByActor.String)
				body.MarkedByActor = &v
				changed = true
			}
			if (body.MarkedByKind == nil || strings.TrimSpace(*body.MarkedByKind) == "") && local.MarkedByKind.Valid && strings.TrimSpace(local.MarkedByKind.String) != "" {
				v := strings.TrimSpace(local.MarkedByKind.String)
				body.MarkedByKind = &v
				changed = true
			}
			if body.MarkedByModel == nil && local.MarkedByModel.Valid {
				v := local.MarkedByModel.String
				body.MarkedByModel = &v
				changed = true
			}
			if body.SessionID == nil && local.SessionID.Valid {
				v := local.SessionID.String
				body.SessionID = &v
				changed = true
			}
			if strings.TrimSpace(body.CreatedAt) == "" && strings.TrimSpace(local.CreatedAt) != "" {
				body.CreatedAt = strings.TrimSpace(local.CreatedAt)
				changed = true
			}
			if strings.TrimSpace(body.UpdatedAt) == "" && strings.TrimSpace(local.UpdatedAt) != "" {
				body.UpdatedAt = strings.TrimSpace(local.UpdatedAt)
				changed = true
			}
		}
		if body.Project == "" && strings.TrimSpace(mutation.Project) != "" {
			body.Project = strings.TrimSpace(mutation.Project)
			changed = true
		}
		missing := []string{}
		if body.SourceID == "" {
			missing = append(missing, "source_id")
		}
		if body.TargetID == "" {
			missing = append(missing, "target_id")
		}
		if body.Relation == "" {
			missing = append(missing, "relation")
		}
		if body.JudgmentStatus == "" {
			missing = append(missing, "judgment_status")
		}
		if body.MarkedByActor == nil || strings.TrimSpace(*body.MarkedByActor) == "" {
			missing = append(missing, "marked_by_actor")
		}
		if body.MarkedByKind == nil || strings.TrimSpace(*body.MarkedByKind) == "" {
			missing = append(missing, "marked_by_kind")
		}
		if body.Project == "" {
			missing = append(missing, "project")
		}
		if len(missing) > 0 {
			return blocked(UpgradeReasonBlockedLegacyMutationManual, fmt.Sprintf("relation payload missing required upsert fields: %s", strings.Join(missing, ", "))), nil
		}
		if !changed {
			return cloudUpgradeLegacyMutationEvaluation{}, nil
		}
		encoded, err := json.Marshal(body)
		if err != nil {
			return cloudUpgradeLegacyMutationEvaluation{}, err
		}
		return repairable("relation payload is missing required fields for canonical bootstrap", "repair fills missing relation fields from local memory_relations table and mutation project", string(encoded)), nil
	}

	return cloudUpgradeLegacyMutationEvaluation{}, nil
}

func (s *Store) cloudUpgradeManualActionReport(project string) (bool, CloudUpgradeRepairReport, error) {
	targetKey := DefaultSyncTargetKey
	if project != "" {
		targetKey = fmt.Sprintf("%s:%s", DefaultSyncTargetKey, project)
	}
	state, err := s.GetSyncState(targetKey)
	if err != nil {
		return false, CloudUpgradeRepairReport{}, fmt.Errorf("read sync state for cloud upgrade repair: %w", err)
	}
	if state == nil {
		return false, CloudUpgradeRepairReport{}, nil
	}
	reasonCode := strings.TrimSpace(derefString(state.ReasonCode))
	if reasonCode == "" {
		return false, CloudUpgradeRepairReport{}, nil
	}

	reasonMap := map[string]string{
		"auth_required":      "upgrade_policy_auth_required",
		"policy_forbidden":   "upgrade_policy_forbidden",
		"cloud_config_error": "upgrade_policy_cloud_config_error",
	}
	repairReasonCode, requiresManualAction := reasonMap[reasonCode]
	if !requiresManualAction {
		return false, CloudUpgradeRepairReport{}, nil
	}
	reasonMessage := strings.TrimSpace(derefString(state.ReasonMessage))
	if reasonMessage == "" {
		reasonMessage = "cloud policy/auth precondition must be resolved before repair"
	}
	return true, CloudUpgradeRepairReport{
		Class:      UpgradeRepairClassPolicy,
		ReasonCode: repairReasonCode,
		Message:    fmt.Sprintf("manual-action-required: %s", reasonMessage),
		Applied:    false,
	}, nil
}

func (s *Store) projectSyncBackfillRequired(project string) (bool, error) {
	var missing int
	err := s.db.QueryRow(`
		SELECT EXISTS(
			SELECT 1
			FROM sessions sess
			WHERE sess.project = ?
			  AND NOT EXISTS (
				SELECT 1 FROM sync_mutations sm
				WHERE sm.target_key = ?
				  AND sm.entity = ?
				  AND sm.entity_key = sess.id
				  AND sm.source = ?
			  )
			UNION ALL
			SELECT 1
			FROM observations obs
			LEFT JOIN sessions sess ON sess.id = obs.session_id
			WHERE (
				ifnull(obs.project, '') = ?
				OR (ifnull(obs.project, '') = '' AND ifnull(sess.project, '') = ?)
			)
			  AND obs.deleted_at IS NULL
			  AND NOT EXISTS (
				SELECT 1 FROM sync_mutations sm
				WHERE sm.target_key = ?
				  AND sm.entity = ?
				  AND sm.entity_key = obs.sync_id
				  AND sm.source = ?
			  )
		)
	`, project, DefaultSyncTargetKey, SyncEntitySession, SyncSourceLocal, project, project, DefaultSyncTargetKey, SyncEntityObservation, SyncSourceLocal).Scan(&missing)
	if err != nil {
		return false, fmt.Errorf("detect project sync metadata gaps: %w", err)
	}
	return missing == 1, nil
}

func normalizeUpgradeStage(stage string) string {
	stage = strings.TrimSpace(strings.ToLower(stage))
	switch stage {
	case UpgradeStagePlanned,
		UpgradeStageDoctorReady,
		UpgradeStageDoctorBlocked,
		UpgradeStageRepairApplied,
		UpgradeStageBootstrapEnrolled,
		UpgradeStageBootstrapPushed,
		UpgradeStageBootstrapVerified,
		UpgradeStageRolledBack:
		return stage
	default:
		return UpgradeStagePlanned
	}
}

func normalizeUpgradeRepairClass(class string) string {
	class = strings.TrimSpace(strings.ToLower(class))
	switch class {
	case UpgradeRepairClassNone,
		UpgradeRepairClassReady,
		UpgradeRepairClassRepairable,
		UpgradeRepairClassBlocked,
		UpgradeRepairClassPolicy:
		return class
	default:
		return UpgradeRepairClassNone
	}
}

// migrateFTSTopicKey rebuilds the observations FTS index so it carries the
// topic_key column and the three synchronisation triggers.
//
// The rebuild must be idempotent and resumable: an interrupted run may leave
// the index with the new column layout but no rows and no triggers, because
// SQLite does not make a multi-statement Exec atomic and DDL committed before a
// later failure stays applied. The completion probe therefore checks the whole
// finished shape (column, all three triggers, row parity) rather than only the
// column, and the rebuild itself runs in one transaction so a failure cannot
// commit the drops.
func (s *Store) migrateFTSTopicKey() error {
	done, err := s.ftsTopicKeyMigrationComplete()
	if err != nil {
		return err
	}
	if done {
		return nil
	}

	tx, err := s.beginTxHook()
	if err != nil {
		return fmt.Errorf("migrate fts topic_key: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := s.execHook(tx, `
		DROP TRIGGER IF EXISTS obs_fts_insert;
		DROP TRIGGER IF EXISTS obs_fts_update;
		DROP TRIGGER IF EXISTS obs_fts_delete;
		DROP TABLE IF EXISTS observations_fts;
		CREATE VIRTUAL TABLE observations_fts USING fts5(
			title,
			content,
			tool_name,
			type,
			project,
			topic_key,
			content='observations',
			content_rowid='id'
		);
		INSERT INTO observations_fts(rowid, title, content, tool_name, type, project, topic_key)
		SELECT id, title, content, tool_name, type, project, topic_key
		FROM observations
		WHERE deleted_at IS NULL;

		CREATE TRIGGER obs_fts_insert AFTER INSERT ON observations BEGIN
			INSERT INTO observations_fts(rowid, title, content, tool_name, type, project, topic_key)
			VALUES (new.id, new.title, new.content, new.tool_name, new.type, new.project, new.topic_key);
		END;

		CREATE TRIGGER obs_fts_delete AFTER DELETE ON observations BEGIN
			INSERT INTO observations_fts(observations_fts, rowid, title, content, tool_name, type, project, topic_key)
			VALUES ('delete', old.id, old.title, old.content, old.tool_name, old.type, old.project, old.topic_key);
		END;

		CREATE TRIGGER obs_fts_update AFTER UPDATE ON observations BEGIN
			INSERT INTO observations_fts(observations_fts, rowid, title, content, tool_name, type, project, topic_key)
			VALUES ('delete', old.id, old.title, old.content, old.tool_name, old.type, old.project, old.topic_key);
			INSERT INTO observations_fts(rowid, title, content, tool_name, type, project, topic_key)
			VALUES (new.id, new.title, new.content, new.tool_name, new.type, new.project, new.topic_key);
		END;
	`); err != nil {
		return fmt.Errorf("migrate fts topic_key: %w", err)
	}
	if err := s.commitHook(tx); err != nil {
		return fmt.Errorf("migrate fts topic_key: commit: %w", err)
	}
	return nil
}

// ftsTopicKeyMigrationComplete reports whether the observations FTS index is in
// its finished post-migration shape: the topic_key column plus all three
// synchronisation triggers.
//
// Row-count parity is deliberately not part of this check. The index is an
// external-content projection maintained by triggers, and a soft delete keeps
// the row in the index while removing it from the live set, so `indexed == live`
// is false after any soft delete and would force a full rebuild on every open.
// Structural completeness is what distinguishes a finished migration from an
// interrupted one; index drift caused by a lost trigger is repaired below.
func (s *Store) ftsTopicKeyMigrationComplete() (bool, error) {
	var tables, columns, triggers int
	if err := s.db.QueryRow(`
		SELECT
			(SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='observations_fts'),
			(SELECT COUNT(*) FROM pragma_table_xinfo('observations_fts') WHERE name='topic_key'),
			(SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name IN
				('obs_fts_insert','obs_fts_update','obs_fts_delete'))
	`).Scan(&tables, &columns, &triggers); err != nil {
		return false, fmt.Errorf("migrate fts topic_key: probe: %w", err)
	}
	return tables > 0 && columns > 0 && triggers == 3, nil
}

// ─── Sessions ────────────────────────────────────────────────────────────────

func (s *Store) CreateSession(id, project, directory string) error {
	return s.CreateSessionWithOrigin(id, project, directory, SessionOriginUnknown)
}

// CreateSessionWithOrigin registers a session and records which transport
// created it. The origin is provenance for review, never an authentication
// token: callers must not pass a trusted label they did not obtain from that
// transport.
func (s *Store) CreateSessionWithOrigin(id, project, directory, origin string) error {
	// Normalize and resolve a unique registered name to its project identity.
	// This does not consult the filesystem: deleted projects keep their ID.
	project, _ = NormalizeProject(project)
	canonical, err := s.CanonicalProjectKey(project)
	if err != nil {
		return err
	}
	project = canonical
	origin = NormalizeSessionOrigin(origin)
	if err := s.validateRegisteredWrite(project, directory, origin); err != nil {
		return err
	}
	return s.withTx(func(tx *sql.Tx) error {
		if target, err := projectKeyRetirementTx(tx, project); err != nil {
			return err
		} else if target != "" {
			return ErrRetiredProjectKey
		}
		// Check and write under the same transaction for both registered IDs and
		// legacy keys. A reused ID must never journal a caller's different owner.
		var existingProject, existingDirectory string
		err := tx.QueryRow(`SELECT project,directory FROM sessions WHERE id=?`, id).Scan(&existingProject, &existingDirectory)
		if err == nil {
			strict := isRegisteredProjectID(project)
			if ((strict || existingProject != "" && project != "") && existingProject != project) ||
				((strict || existingDirectory != "" && directory != "") && canonicalDataDir(existingDirectory) != canonicalDataDir(directory)) {
				return ErrSessionScopeMismatch
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := s.createSessionTx(tx, id, project, directory, origin); err != nil {
			return err
		}
		// Idempotent registration does not reopen a completed session. Replicate
		// the actual row, including its end metadata, rather than an active stub.
		payload := syncSessionPayload{ID: id}
		if err := tx.QueryRow(`SELECT project,directory,started_at,ended_at,summary,ifnull(origin,'unknown') FROM sessions WHERE id=?`, id).
			Scan(&payload.Project, &payload.Directory, &payload.StartedAt, &payload.EndedAt, &payload.Summary, &payload.Origin); err != nil {
			return err
		}
		return s.enqueueSyncMutationTx(tx, SyncEntitySession, id, SyncOpUpsert, payload)
	})
}

func (s *Store) EndSession(id string, summary string) error {
	return s.withTx(func(tx *sql.Tx) error {
		// Only end sessions that are still active: repeating mem_session_end
		// must not refresh ended_at or overwrite the stored summary.
		res, err := s.execHook(tx,
			`UPDATE sessions SET ended_at = datetime('now'), summary = ? WHERE id = ? AND ended_at IS NULL`,
			nullableString(summary), id,
		)
		if err != nil {
			return err
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if rows == 0 {
			var one int
			err := tx.QueryRow(`SELECT 1 FROM sessions WHERE id = ?`, id).Scan(&one)
			if errors.Is(err, sql.ErrNoRows) {
				// Unknown ids must surface as errors: silently reporting success
				// hides host/session lifecycle drift from both callers.
				return ErrSessionNotFound
			}
			return err
		}

		var startedAt, endedAt string
		var project, directory string
		var storedSummary *string
		if err := tx.QueryRow(
			`SELECT project, directory, started_at, ended_at, summary FROM sessions WHERE id = ?`,
			id,
		).Scan(&project, &directory, &startedAt, &endedAt, &storedSummary); err != nil {
			return err
		}

		return s.enqueueSyncMutationTx(tx, SyncEntitySession, id, SyncOpUpsert, syncSessionPayload{
			ID:        id,
			Project:   project,
			Directory: directory,
			StartedAt: startedAt,
			EndedAt:   &endedAt,
			Summary:   storedSummary,
		})
	})
}

// ResumeSession reopens an existing matching host session without creating a
// new identity or retaining an old summary. The caller must verify the actual
// host event and project/root before invoking it.
func (s *Store) ResumeSession(id, project, directory string) error {
	return s.withTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE sessions SET ended_at=NULL, summary=NULL WHERE id=? AND project=? AND directory=?`, id, project, directory)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return errors.New("session scope mismatch")
		}
		var startedAt string
		if err := tx.QueryRow(`SELECT started_at FROM sessions WHERE id=?`, id).Scan(&startedAt); err != nil {
			return err
		}
		return s.enqueueSyncMutationTx(tx, SyncEntitySession, id, SyncOpUpsert, syncSessionPayload{ID: id, Project: project, Directory: directory, StartedAt: startedAt})
	})
}

func (s *Store) GetSession(id string) (*Session, error) {
	row := s.db.QueryRow(
		`SELECT id, project, directory, started_at, ended_at, summary, ifnull(origin, 'unknown') FROM sessions WHERE id = ?`, id,
	)
	var sess Session
	if err := row.Scan(&sess.ID, &sess.Project, &sess.Directory, &sess.StartedAt, &sess.EndedAt, &sess.Summary, &sess.Origin); err != nil {
		return nil, err
	}
	return &sess, nil
}

// SessionOriginStats counts sessions by recorded transport provenance. It is
// a report about metadata claims, not proof of caller identity.
func (s *Store) SessionOriginStats(project string) (map[string]int, error) {
	project, _ = NormalizeProject(project)
	query := `SELECT ifnull(origin, 'unknown') AS o, COUNT(*) FROM sessions`
	args := []any{}
	if project != "" {
		query += ` WHERE LOWER(project) = ?`
		args = append(args, project)
	}
	query += ` GROUP BY o`
	rows, err := s.queryItHook(s.db, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{
		SessionOriginHost: 0, SessionOriginOperator: 0, SessionOriginModel: 0, SessionOriginUnknown: 0,
	}
	for rows.Next() {
		var origin string
		var count int
		if err := rows.Scan(&origin, &count); err != nil {
			return nil, err
		}
		out[NormalizeSessionOrigin(origin)] += count
	}
	return out, rows.Err()
}

// MostRecentActiveSession resolves the active (un-ended) session for a project
// from the persisted sessions table. It returns the session ID and ok=true when
// such a session exists, or ok=false when none does.
//
// This is the cross-process resolution that fixes issue #386: the SessionStart
// hook registers a UUID session via the HTTP server (POST /sessions) in one
// process, while mem_save runs in the separate MCP (stdio) process. The two
// share only the SQLite store, so the active session must be read from disk —
// never from in-memory state.
//
// Selection rules:
//   - Scope to the (normalized) project.
//   - Require ended_at IS NULL — ended sessions are never returned, so stale
//     sessions naturally fall out without any explicit clearing step.
//   - Exclude the manual-save fallback sessions (id LIKE 'manual-save%'); those
//     are created by the fallback path itself and must not be resolved as "the
//     active session", which would make resolution circular.
//   - When multiple un-ended sessions exist, pick the MOST RECENT by
//     started_at DESC, with id DESC as a deterministic tie-breaker.
func (s *Store) MostRecentActiveSession(project string) (string, bool, error) {
	project, _ = NormalizeProject(project)
	if project == "" {
		return "", false, nil
	}

	var id string
	err := s.db.QueryRow(`
		SELECT id
		FROM sessions
		WHERE LOWER(project) = ?
		  AND ended_at IS NULL
		  AND id NOT LIKE 'manual-save%'
		ORDER BY datetime(started_at) DESC, id DESC
		LIMIT 1
	`, project).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}

func (s *Store) RecentSessions(project string, limit int) ([]SessionSummary, error) {
	// Normalize project filter for case-insensitive matching
	project, _ = NormalizeProject(project)

	if limit <= 0 {
		limit = 5
	}

	query := `
		SELECT s.id, s.project, s.directory, s.started_at, s.ended_at, s.summary,
		       COUNT(o.id) as observation_count
		FROM sessions s
		LEFT JOIN observations o ON o.session_id = s.id AND o.deleted_at IS NULL
		WHERE 1=1
	`
	args := []any{}

	if project != "" {
		query += " AND LOWER(s.project) = ?"
		args = append(args, project)
	}

	query += " GROUP BY s.id ORDER BY MAX(datetime(COALESCE(o.created_at, s.started_at))) DESC, s.id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.queryItHook(s.db, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []SessionSummary
	for rows.Next() {
		var ss SessionSummary
		if err := rows.Scan(&ss.ID, &ss.Project, &ss.Directory, &ss.StartedAt, &ss.EndedAt, &ss.Summary, &ss.ObservationCount); err != nil {
			return nil, err
		}
		results = append(results, ss)
	}
	return results, rows.Err()
}

// AllSessions returns recent sessions ordered by most recent first (for TUI browsing).
func (s *Store) AllSessions(project string, limit int) ([]SessionSummary, error) {
	if limit <= 0 {
		limit = 50
	}

	query := `
		SELECT s.id, s.project, s.started_at, s.ended_at, s.summary,
		       COUNT(o.id) as observation_count
		FROM sessions s
		LEFT JOIN observations o ON o.session_id = s.id AND o.deleted_at IS NULL
		WHERE 1=1
	`
	args := []any{}

	if project != "" {
		query += " AND s.project = ?"
		args = append(args, project)
	}

	query += " GROUP BY s.id ORDER BY MAX(datetime(COALESCE(o.created_at, s.started_at))) DESC, s.id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.queryItHook(s.db, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []SessionSummary
	for rows.Next() {
		var ss SessionSummary
		if err := rows.Scan(&ss.ID, &ss.Project, &ss.StartedAt, &ss.EndedAt, &ss.Summary, &ss.ObservationCount); err != nil {
			return nil, err
		}
		results = append(results, ss)
	}
	return results, rows.Err()
}

// AllObservations returns recent observations ordered by most recent first (for TUI browsing).
func (s *Store) AllObservations(project, scope string, limit int) ([]Observation, error) {
	if limit <= 0 {
		limit = s.cfg.MaxContextResults
	}

	query := `
		SELECT ` + observationSelectColumns + `
		FROM observations o
		WHERE o.deleted_at IS NULL
	`
	args := []any{}

	if project != "" {
		query += " AND o.project = ?"
		args = append(args, project)
	}
	if scope != "" {
		query += " AND o.scope = ?"
		args = append(args, normalizeScope(scope))
	}

	query += " ORDER BY datetime(o.created_at) DESC, o.id DESC LIMIT ?"
	args = append(args, limit)

	return s.queryObservations(query, args...)
}

// SessionObservations returns all observations for a specific session.
func (s *Store) SessionObservations(sessionID string, limit int) ([]Observation, error) {
	if limit <= 0 {
		limit = 200
	}

	query := `
		SELECT ` + observationSelectColumns + `
		FROM observations
		WHERE session_id = ? AND deleted_at IS NULL
		ORDER BY created_at ASC
		LIMIT ?
	`
	return s.queryObservations(query, sessionID, limit)
}

// ─── Observations ────────────────────────────────────────────────────────────

func (s *Store) AddObservation(p AddObservationParams) (int64, error) {
	mutation, err := s.prepareObservationMutation(p, false)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.withTx(func(tx *sql.Tx) error { var err error; id, err = mutation(tx); return err })
	if err != nil {
		return 0, err
	}
	return id, nil
}

func (s *Store) prepareObservationMutation(p AddObservationParams, strictNew bool) (func(*sql.Tx) (int64, error), error) {
	return s.prepareObservationMutationMode(p, strictNew, false)
}

// prepareHistoricalObservationMutation allows promoting an existing candidate
// whose originating project directory has since been renamed or deleted. The
// project identity and session already exist; requiring a live binding would
// permanently strand reviewed knowledge just because the checkout moved.
func (s *Store) prepareHistoricalObservationMutation(p AddObservationParams, strictNew bool) (func(*sql.Tx) (int64, error), error) {
	return s.prepareObservationMutationMode(p, strictNew, true)
}

func (s *Store) prepareObservationMutationMode(p AddObservationParams, strictNew, allowMissingDirectory bool) (func(*sql.Tx) (int64, error), error) {
	p.Project, _ = NormalizeProject(p.Project)
	if canonical, err := s.CanonicalProjectKey(p.Project); err != nil {
		return nil, err
	} else {
		p.Project = canonical
	}
	if session, err := s.GetSession(p.SessionID); err == nil {
		session.Project, _ = s.CanonicalProjectKey(session.Project)
		if isRegisteredProjectID(session.Project) {
			if p.Project == "" {
				p.Project = session.Project
			}
			if session.Project != p.Project {
				return nil, errors.New("registered memory project must match its session")
			}
			if !(allowMissingDirectory && directoryMissing(session.Directory)) {
				if err := s.validateRegisteredWrite(p.Project, session.Directory, SessionOriginUnknown); err != nil {
					return nil, err
				}
			}
		} else if isRegisteredProjectID(p.Project) {
			return nil, errors.New("registered memory requires a matching registered session")
		}
	} else if isRegisteredProjectID(p.Project) {
		return nil, errors.New("registered memory requires a matching registered session")
	}

	memoryClassExplicit := strings.TrimSpace(p.MemoryClass) != ""
	memoryClass, err := resolveMemoryClass(p.MemoryClass, p.Type)
	if err != nil {
		return nil, err
	}
	title := stripPrivateTags(p.Title)
	content := stripPrivateTags(p.Content)
	abstract := stripPrivateTags(p.Abstract)
	overview := stripPrivateTags(p.Overview)
	if len(content) > s.cfg.MaxObservationLength {
		content = truncateStoredUTF8(content, s.cfg.MaxObservationLength)
	}
	if len(overview) > s.cfg.MaxObservationLength {
		overview = truncateStoredUTF8(overview, s.cfg.MaxObservationLength)
	}
	memoryPath := strings.TrimSpace(p.MemoryPath)
	summaryState, summaryRevision, err := normalizeSummaryFields(abstract, overview, "", 0)
	if err != nil {
		return nil, err
	}
	summaryExplicit := strings.TrimSpace(abstract) != "" || strings.TrimSpace(overview) != ""
	scope := normalizeScope(p.Scope)
	normHash := hashNormalized(content)
	topicKey := normalizeTopicKey(p.TopicKey)

	var observationID int64
	mutation := func(tx *sql.Tx) error {
		var obs *Observation
		if topicKey != "" {
			var existingID int64
			err := tx.QueryRow(
				`SELECT id FROM observations
				 WHERE topic_key = ?
				   AND ifnull(project, '') = ifnull(?, '')
				   AND scope = ?
				   AND deleted_at IS NULL
				 ORDER BY datetime(updated_at) DESC, datetime(created_at) DESC
				 LIMIT 1`,
				topicKey, nullableString(p.Project), scope,
			).Scan(&existingID)
			if err == nil {
				if strictNew {
					return errors.New("topic already exists; use snapshot-checked memory update")
				}

				existing, err := s.getObservationTx(tx, existingID)
				if err != nil {
					return err
				}
				targetClass := existing.MemoryClass
				if memoryClassExplicit {
					targetClass = memoryClass
				}
				targetPath := existing.MemoryPath
				if strings.TrimSpace(p.MemoryPath) != "" {
					targetPath, err = NormalizeMemoryPath(targetClass, p.MemoryPath)
				} else if targetPath != "" {
					targetPath, err = NormalizeMemoryPath(targetClass, targetPath)
				}
				if err != nil {
					return err
				}
				targetAbstract, targetOverview := existing.Abstract, existing.Overview
				targetSummaryState, targetSummaryRevision := existing.SummaryState, existing.SummaryRevision
				var staleAt, staleReason any = existing.StaleAt, existing.StaleReason
				if summaryExplicit {
					targetAbstract, targetOverview = abstract, overview
					targetSummaryState = SummaryStateFresh
					targetSummaryRevision = existing.RevisionCount + 1
					staleAt, staleReason = nil, nil
				} else if existing.Abstract != "" || existing.Overview != "" {
					targetSummaryState = SummaryStateStale
					staleAt, staleReason = Now(), "observation_content_changed"
				}
				if _, err := s.execHook(tx,
					`UPDATE observations
					 SET type = ?,
					     memory_class = ?,
					     memory_path = ?,
					     abstract = ?,
					     overview = ?,
					     summary_state = ?,
					     summary_revision = ?,
					     stale_at = ?,
					     stale_reason = ?,
					     title = ?,
					     content = ?,
					     tool_name = ?,
					     topic_key = ?,
					     normalized_hash = ?,
					     review_after = NULL,
					     archived_at = NULL,
					     revision_count = revision_count + 1,
					     last_seen_at = datetime('now'),
					     updated_at = datetime('now')
					 WHERE id = ?`,
					p.Type,
					targetClass,
					nullableString(targetPath),
					nullableString(targetAbstract),
					nullableString(targetOverview),
					targetSummaryState,
					targetSummaryRevision,
					staleAt,
					staleReason,
					title,
					content,
					nullableString(p.ToolName),
					nullableString(topicKey),
					normHash,
					existingID,
				); err != nil {
					return err
				}
				obs, err = s.getObservationTx(tx, existingID)
				if err != nil {
					return err
				}
				observationID = existingID
				if err := s.rebuildMemoryNodesTx(tx, derefString(obs.Project), obs.Scope); err != nil {
					return err
				}
				return s.enqueueSyncMutationTx(tx, SyncEntityObservation, obs.SyncID, SyncOpUpsert, observationPayloadFromObservation(obs))
			}
			if err != sql.ErrNoRows {
				return err
			}
		}

		if !strictNew {
			window := dedupeWindowExpression(s.cfg.DedupeWindow)
			var existingID int64
			err := tx.QueryRow(
				`SELECT id FROM observations
			 WHERE normalized_hash = ?
			   AND ifnull(project, '') = ifnull(?, '')
			   AND scope = ?
			   AND type = ?
			   AND title = ?
			   AND deleted_at IS NULL
			   AND datetime(created_at) >= datetime('now', ?)
			 ORDER BY created_at DESC
			 LIMIT 1`,
				normHash, nullableString(p.Project), scope, p.Type, title, window,
			).Scan(&existingID)
			if err == nil {
				existing, err := s.getObservationTx(tx, existingID)
				if err != nil {
					return err
				}
				targetClass := existing.MemoryClass
				if memoryClassExplicit {
					targetClass = memoryClass
				}
				targetPath := existing.MemoryPath
				if strings.TrimSpace(p.MemoryPath) != "" {
					targetPath, err = NormalizeMemoryPath(targetClass, p.MemoryPath)
				} else if targetPath != "" {
					targetPath, err = NormalizeMemoryPath(targetClass, targetPath)
				}
				if err != nil {
					return err
				}
				targetAbstract, targetOverview := existing.Abstract, existing.Overview
				targetSummaryState, targetSummaryRevision := existing.SummaryState, existing.SummaryRevision
				if summaryExplicit {
					targetAbstract, targetOverview = abstract, overview
					targetSummaryState = SummaryStateFresh
					targetSummaryRevision = maxInt(existing.RevisionCount, 1)
				}
				if _, err := s.execHook(tx,
					`UPDATE observations
				 SET duplicate_count = duplicate_count + 1,
				     memory_class = ?,
				     memory_path = ?,
				     abstract = ?,
				     overview = ?,
				     summary_state = ?,
				     summary_revision = ?,
				     stale_at = CASE WHEN ? THEN NULL ELSE stale_at END,
				     stale_reason = CASE WHEN ? THEN NULL ELSE stale_reason END,
				     last_seen_at = datetime('now'),
				     updated_at = datetime('now')
				 WHERE id = ?`,
					targetClass,
					nullableString(targetPath),
					nullableString(targetAbstract),
					nullableString(targetOverview),
					targetSummaryState,
					targetSummaryRevision,
					summaryExplicit,
					summaryExplicit,
					existingID,
				); err != nil {
					return err
				}
				obs, err = s.getObservationTx(tx, existingID)
				if err != nil {
					return err
				}
				observationID = existingID
				if err := s.rebuildMemoryNodesTx(tx, derefString(obs.Project), obs.Scope); err != nil {
					return err
				}
				return s.enqueueSyncMutationTx(tx, SyncEntityObservation, obs.SyncID, SyncOpUpsert, observationPayloadFromObservation(obs))
			}
			if err != sql.ErrNoRows {
				return err
			}

		}

		syncID := newSyncID("obs")
		memoryPath, err = NormalizeMemoryPath(memoryClass, memoryPath)
		if err != nil {
			return err
		}
		res, err := s.execHook(tx,
			`INSERT INTO observations (
				sync_id, session_id, type, memory_class, memory_path, abstract, overview,
				summary_state, summary_revision, title, content, tool_name, project, scope,
				topic_key, normalized_hash, revision_count, duplicate_count, last_seen_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, 1, datetime('now'), datetime('now'))`,
			syncID, p.SessionID, p.Type, memoryClass, nullableString(memoryPath), nullableString(abstract), nullableString(overview),
			summaryState, summaryRevision, title, content,
			nullableString(p.ToolName), nullableString(p.Project), scope, nullableString(topicKey), normHash,
		)
		if err != nil {
			return err
		}
		observationID, err = res.LastInsertId()
		if err != nil {
			return err
		}

		// Populate review_after for types that have a configured decay offset.
		// expires_at is intentionally NULL for all types in Phase 1.
		// This UPDATE runs only for NEW inserts (not topic_key revisions or deduplication).
		if months, ok := decayReviewAfterMonths[p.Type]; ok {
			reviewAfter := time.Now().UTC().AddDate(0, months, 0).Format("2006-01-02 15:04:05")
			if _, err := s.execHook(tx,
				`UPDATE observations SET review_after = ? WHERE id = ?`,
				reviewAfter, observationID,
			); err != nil {
				return fmt.Errorf("set review_after: %w", err)
			}
		}

		obs, err = s.getObservationTx(tx, observationID)
		if err != nil {
			return err
		}
		if err := s.rebuildMemoryNodesTx(tx, derefString(obs.Project), obs.Scope); err != nil {
			return err
		}
		return s.enqueueSyncMutationTx(tx, SyncEntityObservation, obs.SyncID, SyncOpUpsert, observationPayloadFromObservation(obs))
	}
	return func(tx *sql.Tx) (int64, error) { err := mutation(tx); return observationID, err }, nil
}

func (s *Store) RecentObservations(project, scope string, limit int) ([]Observation, error) {
	// Normalize project filter for case-insensitive matching
	project, _ = NormalizeProject(project)

	if limit <= 0 {
		limit = s.cfg.MaxContextResults
	}

	query := `
		SELECT ` + observationSelectColumns + `
		FROM observations o
		WHERE o.deleted_at IS NULL AND o.archived_at IS NULL
	`
	args := []any{}

	if project != "" {
		query += " AND LOWER(o.project) = ?"
		args = append(args, project)
	}
	if scope != "" {
		query += " AND o.scope = ?"
		args = append(args, normalizeScope(scope))
	}

	query += " ORDER BY datetime(o.created_at) DESC, o.id DESC LIMIT ?"
	args = append(args, limit)

	return s.queryObservations(query, args...)
}

func (s *Store) PinnedObservations(project, scope string) ([]Observation, error) {
	project, _ = NormalizeProject(project)

	query := `
		SELECT ` + observationSelectColumns + `
		FROM observations o
		WHERE o.deleted_at IS NULL AND o.archived_at IS NULL AND o.pinned = 1
	`
	args := []any{}

	if project != "" {
		query += " AND LOWER(o.project) = ?"
		args = append(args, project)
	}
	if scope != "" {
		query += " AND o.scope = ?"
		args = append(args, normalizeScope(scope))
	}

	query += " ORDER BY datetime(o.created_at) DESC, o.id DESC"
	return s.queryObservations(query, args...)
}

func (s *Store) PinObservation(id int64) error {
	return s.setObservationPinned(id, true)
}

func (s *Store) UnpinObservation(id int64) error {
	return s.setObservationPinned(id, false)
}

func (s *Store) setObservationPinned(id int64, pinned bool) error {
	value := 0
	if pinned {
		value = 1
	}
	res, err := s.execHook(s.db, `UPDATE observations SET pinned = ? WHERE id = ? AND deleted_at IS NULL`, value, id)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrObservationNotFound
	}
	return nil
}

func (s *Store) recentUnpinnedObservations(project, scope string, limit int) ([]Observation, error) {
	project, _ = NormalizeProject(project)
	if limit <= 0 {
		limit = s.cfg.MaxContextResults
	}

	query := `
		SELECT ` + observationSelectColumns + `
		FROM observations o
		WHERE o.deleted_at IS NULL AND o.archived_at IS NULL AND o.pinned = 0
	`
	args := []any{}
	if project != "" {
		query += " AND LOWER(o.project) = ?"
		args = append(args, project)
	}
	if scope != "" {
		query += " AND o.scope = ?"
		args = append(args, normalizeScope(scope))
	}
	query += " ORDER BY datetime(o.created_at) DESC, o.id DESC LIMIT ?"
	args = append(args, limit)
	return s.queryObservations(query, args...)
}

// ObservationsNeedingReview returns non-deleted observations that are stale or
// whose review_after timestamp has passed.
// An empty project searches all projects, matching existing browse/search conventions.
func (s *Store) ObservationsNeedingReview(project string, limit int, status string) ([]Observation, error) {
	project = s.canonicalProjectOrRaw(project)
	if status != "" && status != "active" && status != "stale" {
		return nil, fmt.Errorf("review status must be active or stale")
	}
	if limit <= 0 {
		limit = s.cfg.MaxContextResults
	}
	query := `
		SELECT ` + observationSelectColumns + `
		FROM observations o
		WHERE o.deleted_at IS NULL AND o.archived_at IS NULL
		  AND (
			o.stale_at IS NOT NULL
			OR (o.review_after IS NOT NULL AND datetime(o.review_after) <= datetime('now'))
		  )
	`
	args := []any{}
	if project != "" {
		query += " AND LOWER(o.project) = ?"
		args = append(args, project)
	}
	if status == "active" {
		query += " AND o.stale_at IS NULL"
	} else if status == "stale" {
		query += " AND o.stale_at IS NOT NULL"
	}
	query += " ORDER BY datetime(COALESCE(o.stale_at, o.review_after)) ASC, o.id ASC LIMIT ?"
	args = append(args, limit)

	return s.queryObservations(query, args...)
}

// MarkReviewed resets an observation's review_after using its type's configured decay offset.
// Types without a decay offset return to a NULL review_after value.
// This lifecycle reset is intentionally local-only until the sync wire format includes review_after.
func (s *Store) MarkReviewed(id int64) error {
	return s.withTx(func(tx *sql.Tx) error {
		obs, err := s.getObservationTx(tx, id)
		if err == sql.ErrNoRows {
			return ErrObservationNotFound
		}
		if err != nil {
			return err
		}

		var reviewAfter any
		if months, ok := decayReviewAfterMonths[obs.Type]; ok {
			reviewAfter = time.Now().UTC().AddDate(0, months, 0).Format("2006-01-02 15:04:05")
		}
		if _, err := s.execHook(tx, `UPDATE observations SET review_after = ?, updated_at = datetime('now') WHERE id = ? AND deleted_at IS NULL`, reviewAfter, id); err != nil {
			return err
		}
		return nil
	})
}

// ─── User Prompts ────────────────────────────────────────────────────────────

func (s *Store) AddPrompt(p AddPromptParams) (int64, error) {
	// Normalize and resolve a unique registered name before storing.
	p.Project, _ = NormalizeProject(p.Project)
	if canonical, err := s.CanonicalProjectKey(p.Project); err == nil {
		p.Project = canonical
	}

	content := s.preparePromptContent(p.Content)

	var promptID int64
	err := s.withTx(func(tx *sql.Tx) error {
		syncID := newSyncID("prompt")
		res, err := s.execHook(tx,
			`INSERT INTO user_prompts (sync_id, session_id, content, project) VALUES (?, ?, ?, ?)`,
			syncID, p.SessionID, content, nullableString(p.Project),
		)
		if err != nil {
			return err
		}
		promptID, err = res.LastInsertId()
		if err != nil {
			return err
		}
		var createdAt string
		if err := tx.QueryRow(`SELECT created_at FROM user_prompts WHERE id = ?`, promptID).Scan(&createdAt); err != nil {
			return err
		}
		if _, err := s.execHook(tx, `DELETE FROM prompt_tombstones WHERE sync_id = ?`, syncID); err != nil {
			return err
		}
		return s.enqueueSyncMutationTx(tx, SyncEntityPrompt, syncID, SyncOpUpsert, syncPromptPayload{
			SyncID:    syncID,
			SessionID: p.SessionID,
			Content:   content,
			Project:   nullableString(p.Project),
			CreatedAt: createdAt,
		})
	})
	if err != nil {
		return 0, err
	}
	return promptID, nil
}

func (s *Store) AddPromptIfMissing(p AddPromptParams) (int64, bool, error) {
	p.Project, _ = NormalizeProject(p.Project)
	if canonical, err := s.CanonicalProjectKey(p.Project); err == nil {
		p.Project = canonical
	}
	content := s.preparePromptContent(p.Content)

	var promptID int64
	inserted := false
	err := s.withTx(func(tx *sql.Tx) error {
		err := tx.QueryRow(
			`SELECT id FROM user_prompts WHERE session_id = ? AND ifnull(project, '') = ? AND content = ? ORDER BY id DESC LIMIT 1`,
			p.SessionID, p.Project, content,
		).Scan(&promptID)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		syncID := newSyncID("prompt")
		res, err := s.execHook(tx,
			`INSERT INTO user_prompts (sync_id, session_id, content, project) VALUES (?, ?, ?, ?)`,
			syncID, p.SessionID, content, nullableString(p.Project),
		)
		if err != nil {
			return err
		}
		promptID, err = res.LastInsertId()
		if err != nil {
			return err
		}
		var createdAt string
		if err := tx.QueryRow(`SELECT created_at FROM user_prompts WHERE id = ?`, promptID).Scan(&createdAt); err != nil {
			return err
		}
		if _, err := s.execHook(tx, `DELETE FROM prompt_tombstones WHERE sync_id = ?`, syncID); err != nil {
			return err
		}
		inserted = true
		return s.enqueueSyncMutationTx(tx, SyncEntityPrompt, syncID, SyncOpUpsert, syncPromptPayload{
			SyncID:    syncID,
			SessionID: p.SessionID,
			Content:   content,
			Project:   nullableString(p.Project),
			CreatedAt: createdAt,
		})
	})
	if err != nil {
		return 0, false, err
	}
	return promptID, inserted, nil
}

func (s *Store) preparePromptContent(content string) string {
	content = stripPrivateTags(content)
	if len(content) > s.cfg.MaxObservationLength {
		content = truncateStoredUTF8(content, s.cfg.MaxObservationLength)
	}
	return content
}

func (s *Store) RecentPrompts(project string, limit int) ([]Prompt, error) {
	// Normalize project filter for case-insensitive matching
	project, _ = NormalizeProject(project)

	if limit <= 0 {
		limit = 20
	}

	query := `SELECT id, ifnull(sync_id, '') as sync_id, session_id, content, ifnull(project, '') as project, created_at FROM user_prompts`
	args := []any{}

	if project != "" {
		query += " WHERE project = ?"
		args = append(args, project)
	}

	query += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.queryItHook(s.db, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []Prompt
	for rows.Next() {
		var p Prompt
		if err := rows.Scan(&p.ID, &p.SyncID, &p.SessionID, &p.Content, &p.Project, &p.CreatedAt); err != nil {
			return nil, err
		}
		results = append(results, p)
	}
	return results, rows.Err()
}

func (s *Store) SearchPrompts(query string, project string, limit int) ([]Prompt, error) {
	if limit <= 0 {
		limit = 10
	}

	ftsQuery := sanitizeFTS(query)

	sql := `
		SELECT p.id, ifnull(p.sync_id, '') as sync_id, p.session_id, p.content, ifnull(p.project, '') as project, p.created_at
		FROM prompts_fts fts
		JOIN user_prompts p ON p.id = fts.rowid
		WHERE prompts_fts MATCH ?
	`
	args := []any{ftsQuery}

	if project != "" {
		sql += " AND p.project = ?"
		args = append(args, project)
	}

	sql += " ORDER BY fts.rank LIMIT ?"
	args = append(args, limit)

	rows, err := s.queryItHook(s.db, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("search prompts: %w", err)
	}
	defer rows.Close()

	var results []Prompt
	for rows.Next() {
		var p Prompt
		if err := rows.Scan(&p.ID, &p.SyncID, &p.SessionID, &p.Content, &p.Project, &p.CreatedAt); err != nil {
			return nil, err
		}
		results = append(results, p)
	}
	return results, rows.Err()
}

// ─── Delete Session ──────────────────────────────────────────────────────────

// DeleteSession hard-deletes a session and its prompts.
// It returns ErrSessionHasObservations if the session has any observations
// (including soft-deleted ones) to prevent orphaned rows.
// It returns ErrSessionNotFound if no session with that ID exists.
//
// When the session belongs to an enrolled project, this operation also enqueues
// a session/delete mutation so cloud replicas can remove the session.
func (s *Store) DeleteSession(id string) error {
	return s.withTx(func(tx *sql.Tx) error {
		var project string
		if err := tx.QueryRow(`SELECT project FROM sessions WHERE id = ?`, id).Scan(&project); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: %q", ErrSessionNotFound, id)
			}
			return fmt.Errorf("delete session: load session: %w", err)
		}

		var enrolled int
		if err := tx.QueryRow(`SELECT 1 FROM sync_enrolled_projects WHERE project = ? LIMIT 1`, project).Scan(&enrolled); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("delete session: check enrollment: %w", err)
			}
		}
		// Count ALL observations for the session, including soft-deleted ones,
		// because the FK constraint on observations.session_id has no ON DELETE CASCADE.
		var count int
		rows, err := s.queryItHook(tx, `SELECT COUNT(*) FROM observations WHERE session_id = ?`, id)
		if err != nil {
			return fmt.Errorf("delete session: count observations: %w", err)
		}
		if rows.Next() {
			if err := rows.Scan(&count); err != nil {
				_ = rows.Close()
				return fmt.Errorf("delete session: count observations: %w", err)
			}
		}
		_ = rows.Close()
		if count > 0 {
			return fmt.Errorf("%w: session %q has %d observation(s), including soft-deleted rows; no automatic cascade performed", ErrSessionHasObservations, id, count)
		}

		if err := validateSessionPromptOwnersTx(tx, project, id); err != nil {
			return err
		}
		rows, err = tx.Query(`SELECT id FROM user_prompts WHERE session_id=?`, id)
		if err != nil {
			return err
		}
		var promptIDs []int64
		for rows.Next() {
			var promptID int64
			if err := rows.Scan(&promptID); err != nil {
				return closeRowsWithError(rows, err)
			}
			promptIDs = append(promptIDs, promptID)
		}
		if err := rows.Err(); err != nil {
			return closeRowsWithError(rows, err)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, promptID := range promptIDs {
			if err := s.deletePromptTx(tx, promptID); err != nil {
				return err
			}
		}

		res, err := s.execHook(tx, `DELETE FROM sessions WHERE id = ?`, id)
		if err != nil {
			var sqliteErr *sqlite.Error
			if errors.As(err, &sqliteErr) && sqliteErr.Code() == sqliteConstraintForeignKey {
				return fmt.Errorf("%w: session %q has observation(s), including soft-deleted rows; no automatic cascade performed", ErrSessionHasObservations, id)
			}
			return fmt.Errorf("delete session: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("delete session: rows affected: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("%w: %q", ErrSessionNotFound, id)
		}

		if enrolled == 1 {
			now := Now()
			if err := s.enqueueSyncMutationTx(tx, SyncEntitySession, id, SyncOpDelete, syncSessionPayload{
				ID:        id,
				Project:   project,
				DeletedAt: &now,
			}); err != nil {
				return fmt.Errorf("delete session: enqueue mutation: %w", err)
			}
		}

		return nil
	})
}

// ─── Delete Prompt ───────────────────────────────────────────────────────────

// DeletePrompt hard-deletes a single prompt by ID and records a sync tombstone.
// It returns ErrPromptNotFound if no prompt with that ID exists.
func (s *Store) DeletePrompt(id int64) error {
	return s.withTx(func(tx *sql.Tx) error { return s.deletePromptTx(tx, id) })
}

func (s *Store) deletePromptTx(tx *sql.Tx, id int64) error {
	var payload syncPromptPayload
	var project string
	if err := tx.QueryRow(`SELECT sync_id, session_id, ifnull(project, '') FROM user_prompts WHERE id = ?`, id).Scan(&payload.SyncID, &payload.SessionID, &project); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: prompt #%d", ErrPromptNotFound, id)
		}
		return fmt.Errorf("delete prompt: load row: %w", err)
	}
	payload.Project = nullableString(project)
	now := Now()
	payload.Deleted = true
	payload.HardDelete = true
	payload.DeletedAt = &now

	res, err := s.execHook(tx, `DELETE FROM user_prompts WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete prompt: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete prompt: rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: prompt #%d", ErrPromptNotFound, id)
	}
	if _, err := s.execHook(tx,
		`INSERT INTO prompt_tombstones (sync_id, session_id, project, deleted_at)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT(sync_id) DO UPDATE SET session_id = excluded.session_id, project = excluded.project, deleted_at = excluded.deleted_at`,
		payload.SyncID, payload.SessionID, payload.Project, now,
	); err != nil {
		return fmt.Errorf("delete prompt: upsert tombstone: %w", err)
	}
	if err := s.enqueueSyncMutationTx(tx, SyncEntityPrompt, payload.SyncID, SyncOpDelete, payload); err != nil {
		return fmt.Errorf("delete prompt: enqueue mutation: %w", err)
	}
	return nil
}

// ─── Get Single Observation ──────────────────────────────────────────────────

func (s *Store) GetObservation(id int64) (*Observation, error) {
	row := s.db.QueryRow(
		`SELECT `+observationSelectColumns+`
		 FROM observations WHERE id = ? AND deleted_at IS NULL`, id,
	)
	var o Observation
	if err := scanObservationRow(row, &o); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrObservationNotFound
		}
		return nil, err
	}
	return &o, nil
}

func (s *Store) UpdateObservation(id int64, p UpdateObservationParams) (*Observation, error) {
	var updated *Observation
	err := s.withTx(func(tx *sql.Tx) error { var err error; updated, err = s.updateObservationTx(tx, id, p); return err })
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// Shared by trusted legacy callers and the snapshot-checked operator boundary.
// Edits preserve lifecycle state: archived memories stay archived and
// review_after is changed only by explicit review actions.
// Identical patches return the current row without a revision.
func (s *Store) updateObservationTx(tx *sql.Tx, id int64, p UpdateObservationParams) (*Observation, error) {
	obs, err := s.getObservationTx(tx, id)
	if err != nil {
		return nil, err
	}

	typ := obs.Type
	memoryClass := obs.MemoryClass
	memoryPath := obs.MemoryPath
	abstract := obs.Abstract
	overview := obs.Overview
	title := obs.Title
	content := obs.Content
	project := derefString(obs.Project)
	scope := obs.Scope
	topicKey := derefString(obs.TopicKey)

	if p.Type != nil {
		typ = *p.Type
	}
	if p.MemoryClass != nil {
		memoryClass, err = normalizeMemoryClass(*p.MemoryClass)
		if err != nil {
			return nil, err
		}
	}
	if p.MemoryPath != nil {
		memoryPath = *p.MemoryPath
	}
	memoryPath, err = NormalizeMemoryPath(memoryClass, memoryPath)
	if err != nil {
		return nil, err
	}
	if p.Abstract != nil {
		abstract = stripPrivateTags(*p.Abstract)
	}
	if p.Overview != nil {
		overview = stripPrivateTags(*p.Overview)
		if len(overview) > s.cfg.MaxObservationLength {
			overview = truncateStoredUTF8(overview, s.cfg.MaxObservationLength)
		}
	}
	if p.Title != nil {
		title = stripPrivateTags(*p.Title)
	}
	if p.Content != nil {
		content = stripPrivateTags(*p.Content)
		if len(content) > s.cfg.MaxObservationLength {
			content = truncateStoredUTF8(content, s.cfg.MaxObservationLength)
		}
	}
	if p.Project != nil {
		project, _ = NormalizeProject(*p.Project)
	}
	if p.Scope != nil {
		scope = normalizeScope(*p.Scope)
	}
	if p.TopicKey != nil {
		topicKey = normalizeTopicKey(*p.TopicKey)
	}

	if (p.Archived == nil || *p.Archived == (obs.ArchivedAt != nil)) && typ == obs.Type && memoryClass == obs.MemoryClass && memoryPath == obs.MemoryPath &&
		abstract == obs.Abstract && overview == obs.Overview && title == obs.Title && content == obs.Content &&
		project == derefString(obs.Project) && scope == obs.Scope && topicKey == derefString(obs.TopicKey) &&
		(!(p.Abstract != nil || p.Overview != nil) || obs.SummaryState == SummaryStateFresh && obs.SummaryRevision == obs.RevisionCount) {
		return obs, nil
	}

	summaryState := obs.SummaryState
	summaryRevision := obs.SummaryRevision
	staleAt := obs.StaleAt
	staleReason := obs.StaleReason
	summaryProvided := p.Abstract != nil || p.Overview != nil
	if summaryProvided {
		summaryState, summaryRevision, err = normalizeSummaryFields(abstract, overview, "", obs.RevisionCount+1)
		if err != nil {
			return nil, err
		}
	} else if typ != obs.Type || title != obs.Title || content != obs.Content {
		if strings.TrimSpace(obs.Abstract) != "" || strings.TrimSpace(obs.Overview) != "" {
			summaryState = SummaryStateStale
			if staleAt == nil {
				now, reason := Now(), "observation_content_changed"
				staleAt, staleReason = &now, &reason
			}
		}
	}
	archivedAt := obs.ArchivedAt
	if p.Archived != nil && !*p.Archived {
		archivedAt = nil
	}
	if p.Archived != nil && *p.Archived && obs.ArchivedAt == nil {
		if obs.Pinned || isProtectedMemoryClass(obs.MemoryClass) {
			return nil, errors.New("protected memory cannot be archived")
		}
		now := Now()
		archivedAt = &now
	}
	if p.Archived != nil && summaryState == SummaryStateFresh && summaryRevision == obs.RevisionCount {
		summaryRevision = obs.RevisionCount + 1
	}
	contentRecomputed := content != obs.Content && p.Archived == nil
	if contentRecomputed {
		if err := s.rebindIncomingDependenciesTx(tx, obs.SyncID, true); err != nil {
			return nil, err
		}
	}
	sourcesValid, err := observationSourcesValidTx(tx, obs.SyncID)
	if err != nil {
		return nil, err
	}
	if !sourcesValid && staleAt == nil {
		now, reason := Now(), "source_dependencies_unresolved"
		staleAt, staleReason = &now, &reason
	}
	if sourcesValid && summaryState != SummaryStateStale &&
		(contentRecomputed || summaryProvided && obs.SummaryState == SummaryStateStale) {
		staleAt, staleReason = nil, nil
	}

	if _, err := s.execHook(tx,
		`UPDATE observations
		 SET type = ?,
		     memory_class = ?,
		     memory_path = ?,
		     abstract = ?,
		     overview = ?,
		     summary_state = ?,
		     summary_revision = ?,
		     stale_at = ?,
		     stale_reason = ?,
		     title = ?,
		     content = ?,
		     project = ?,
		     scope = ?,
		     topic_key = ?,
		     normalized_hash = ?,
		     archived_at = ?,
		     revision_count = revision_count + 1,
		     updated_at = datetime('now')
		 WHERE id = ? AND deleted_at IS NULL`,
		typ,
		memoryClass,
		nullableString(memoryPath),
		nullableString(abstract),
		nullableString(overview),
		summaryState,
		summaryRevision,
		staleAt,
		staleReason,
		title,
		content,
		nullableString(project),
		scope,
		nullableString(topicKey),
		hashNormalized(content),
		archivedAt,
		id,
	); err != nil {
		return nil, err
	}

	updated, err := s.getObservationTx(tx, id)
	if err != nil {
		return nil, err
	}
	if err := s.rebuildMemoryNodesTx(tx, derefString(obs.Project), obs.Scope); err != nil {
		return nil, err
	}
	if derefString(obs.Project) != derefString(updated.Project) || obs.Scope != updated.Scope {
		if err := s.rebuildMemoryNodesTx(tx, derefString(updated.Project), updated.Scope); err != nil {
			return nil, err
		}
	}
	if err := s.enqueueSyncMutationTx(tx, SyncEntityObservation, updated.SyncID, SyncOpUpsert, observationPayloadFromObservation(updated)); err != nil {
		return nil, err
	}
	if err := s.invalidateDependentsTx(tx, updated.SyncID, updated.SyncID, updated.RevisionCount, "source_updated", true); err != nil {
		return nil, err
	}
	return updated, nil
}

func (s *Store) DeleteObservation(id int64, hardDelete bool) error {
	return s.withTx(func(tx *sql.Tx) error {
		var obs *Observation
		var err error
		if hardDelete {
			row := tx.QueryRow(`SELECT `+observationSelectColumns+` FROM observations WHERE id=?`, id)
			obs = &Observation{}
			err = scanObservationRow(row, obs)
		} else {
			obs, err = s.getObservationTx(tx, id)
		}
		if err == sql.ErrNoRows {
			return ErrObservationNotFound
		}
		if err != nil {
			return err
		}
		if err := s.invalidateDependentsTx(tx, obs.SyncID, obs.SyncID, obs.RevisionCount, "source_deleted", true); err != nil {
			return err
		}

		deletedAt := Now()
		if _, err := s.execHook(tx, `INSERT INTO observation_tombstones(sync_id,session_id,project,deleted_at)
			VALUES(?,?,?,?) ON CONFLICT(sync_id) DO UPDATE SET deleted_at=excluded.deleted_at`, obs.SyncID, obs.SessionID, obs.Project, deletedAt); err != nil {
			return err
		}
		if hardDelete {
			if _, err := s.execHook(tx, `DELETE FROM observations WHERE id = ?`, id); err != nil {
				return err
			}
			// ── Phase: memory-conflict-surfacing — C.11 ──────────────────────
			// Orphan any memory_relations rows that reference this observation's
			// sync_id (as source or target). Relations are never cascade-deleted;
			// they become audit history with judgment_status='orphaned'.
			if obs.SyncID != "" {
				if _, err := s.execHook(tx, `
					UPDATE memory_relations
					SET judgment_status = 'orphaned',
					    updated_at      = datetime('now')
					WHERE source_id = ? OR target_id = ?
				`, obs.SyncID, obs.SyncID); err != nil {
					return fmt.Errorf("orphan memory_relations after hard-delete: %w", err)
				}
			}
		} else {
			if _, err := s.execHook(tx,
				`UPDATE observations
				 SET deleted_at = datetime('now'),
				     updated_at = datetime('now')
				 WHERE id = ? AND deleted_at IS NULL`,
				id,
			); err != nil {
				return err
			}
			if err := tx.QueryRow(`SELECT deleted_at FROM observations WHERE id = ?`, id).Scan(&deletedAt); err != nil {
				return err
			}
		}
		if err := s.rebuildMemoryNodesTx(tx, derefString(obs.Project), obs.Scope); err != nil {
			return err
		}

		return s.enqueueSyncMutationTx(tx, SyncEntityObservation, obs.SyncID, SyncOpDelete, syncObservationPayload{
			SyncID:     obs.SyncID,
			SessionID:  obs.SessionID,
			Project:    obs.Project,
			Deleted:    true,
			DeletedAt:  &deletedAt,
			HardDelete: hardDelete,
		})
	})
}

// ─── Timeline ────────────────────────────────────────────────────────────────
//
// Timeline provides chronological context around a specific observation.
// Given an observation ID, it returns N observations before and M after,
// all within the same session. This is the "progressive disclosure" pattern
// from claude-mem — agents first search, then use timeline to drill into
// the chronological neighborhood of a result.

func (s *Store) Timeline(observationID int64, before, after int) (*TimelineResult, error) {
	if before < 0 {
		before = 5
	}
	if after < 0 {
		after = 5
	}

	// 1. Get the focus observation
	focus, err := s.GetObservation(observationID)
	if err != nil {
		return nil, fmt.Errorf("timeline: observation #%d not found: %w", observationID, err)
	}

	// 2. Get session info
	session, err := s.GetSession(focus.SessionID)
	if err != nil {
		// Session might be missing for manual-save observations — non-fatal
		session = nil
	}
	owner := derefString(focus.Project)
	if session != nil && owner != "" && session.Project != "" && session.Project != owner {
		return nil, errors.New("timeline: anchor/session project mismatch; review the legacy identity mapping")
	}

	// 3. Get observations BEFORE the focus (same session, older, chronological order)
	beforeRows, err := s.queryItHook(s.db, `
		SELECT id, session_id, type, memory_class, ifnull(memory_path, ''), ifnull(abstract, ''), ifnull(overview, ''),
		       summary_state, summary_revision, stale_at, stale_reason, title, content, tool_name, project,
		       scope, topic_key, revision_count, duplicate_count, last_seen_at, created_at, updated_at, deleted_at
		FROM observations
		WHERE session_id = ? AND id < ? AND deleted_at IS NULL AND archived_at IS NULL AND ifnull(project,'')=?
		ORDER BY id DESC
		LIMIT ?
	`, focus.SessionID, observationID, owner, before)
	if err != nil {
		return nil, fmt.Errorf("timeline: before query: %w", err)
	}
	defer beforeRows.Close()

	var beforeEntries []TimelineEntry
	for beforeRows.Next() {
		var e TimelineEntry
		if err := beforeRows.Scan(
			&e.ID, &e.SessionID, &e.Type, &e.MemoryClass, &e.MemoryPath, &e.Abstract, &e.Overview,
			&e.SummaryState, &e.SummaryRevision, &e.StaleAt, &e.StaleReason, &e.Title, &e.Content,
			&e.ToolName, &e.Project, &e.Scope, &e.TopicKey, &e.RevisionCount, &e.DuplicateCount, &e.LastSeenAt,
			&e.CreatedAt, &e.UpdatedAt, &e.DeletedAt,
		); err != nil {
			return nil, err
		}
		beforeEntries = append(beforeEntries, e)
	}
	if err := beforeRows.Err(); err != nil {
		return nil, err
	}
	// Reverse to get chronological order (oldest first)
	for i, j := 0, len(beforeEntries)-1; i < j; i, j = i+1, j-1 {
		beforeEntries[i], beforeEntries[j] = beforeEntries[j], beforeEntries[i]
	}

	// 4. Get observations AFTER the focus (same session, newer, chronological order)
	afterRows, err := s.queryItHook(s.db, `
		SELECT id, session_id, type, memory_class, ifnull(memory_path, ''), ifnull(abstract, ''), ifnull(overview, ''),
		       summary_state, summary_revision, stale_at, stale_reason, title, content, tool_name, project,
		       scope, topic_key, revision_count, duplicate_count, last_seen_at, created_at, updated_at, deleted_at
		FROM observations
		WHERE session_id = ? AND id > ? AND deleted_at IS NULL AND archived_at IS NULL AND ifnull(project,'')=?
		ORDER BY id ASC
		LIMIT ?
	`, focus.SessionID, observationID, owner, after)
	if err != nil {
		return nil, fmt.Errorf("timeline: after query: %w", err)
	}
	defer afterRows.Close()

	var afterEntries []TimelineEntry
	for afterRows.Next() {
		var e TimelineEntry
		if err := afterRows.Scan(
			&e.ID, &e.SessionID, &e.Type, &e.MemoryClass, &e.MemoryPath, &e.Abstract, &e.Overview,
			&e.SummaryState, &e.SummaryRevision, &e.StaleAt, &e.StaleReason, &e.Title, &e.Content,
			&e.ToolName, &e.Project, &e.Scope, &e.TopicKey, &e.RevisionCount, &e.DuplicateCount, &e.LastSeenAt,
			&e.CreatedAt, &e.UpdatedAt, &e.DeletedAt,
		); err != nil {
			return nil, err
		}
		afterEntries = append(afterEntries, e)
	}
	if err := afterRows.Err(); err != nil {
		return nil, err
	}

	// 5. Count total observations in the session for context
	var totalInRange int
	s.db.QueryRow(
		"SELECT COUNT(*) FROM observations WHERE session_id = ? AND deleted_at IS NULL AND ifnull(project,'')=? AND (archived_at IS NULL OR id=?)", focus.SessionID, owner, observationID,
	).Scan(&totalInRange)

	return &TimelineResult{
		Focus:        *focus,
		Before:       beforeEntries,
		After:        afterEntries,
		SessionInfo:  session,
		TotalInRange: totalInRange,
	}, nil
}

// normalizeSearchTimeBounds validates and normalizes Since/Until. Both are
// compared against observations.created_at (UTC text). An invalid bound is an
// error, never a silently ignored filter.
func normalizeSearchTimeBounds(opts *SearchOptions) error {
	since, err := normalizeSearchTimeBound(opts.Since, false)
	if err != nil {
		return err
	}
	until, err := normalizeSearchTimeBound(opts.Until, true)
	if err != nil {
		return err
	}
	if since != "" && until != "" && since >= until {
		return fmt.Errorf("invalid time range: since %q must be before until %q", opts.Since, opts.Until)
	}
	opts.Since, opts.Until = since, until
	return nil
}

func normalizeSearchTimeBound(value string, endExclusive bool) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	const layout = "2006-01-02 15:04:05"
	for _, candidate := range []string{layout, time.RFC3339, "2006-01-02"} {
		parsed, err := time.Parse(candidate, value)
		if err != nil {
			continue
		}
		if candidate == "2006-01-02" && endExclusive {
			parsed = parsed.Add(24 * time.Hour)
		}
		return parsed.UTC().Format(layout), nil
	}
	return "", fmt.Errorf("invalid time bound %q: use YYYY-MM-DD, YYYY-MM-DD HH:MM:SS or RFC3339", value)
}

// maxSearchOffset bounds pagination so a caller cannot force an unbounded
// scan; the UI pages 20 at a time and the CLI never sets an offset.
func maxSearchOffset(offset int) int {
	if offset < 0 {
		return 0
	}
	if offset > 10000 {
		return 10000
	}
	return offset
}

// ─── Search (FTS5) ───────────────────────────────────────────────────────────

func (s *Store) Search(query string, opts SearchOptions) ([]SearchResult, error) {
	results, err := s.search(query, opts)
	if err == nil && !opts.DeferRecallTracking {
		s.RecordSearchRecalls(results)
	}
	return results, err
}

func (s *Store) SearchPreview(query string, opts SearchOptions) ([]SearchResult, error) {
	opts.DeferRecallTracking = true
	opts.readOnlyProjection = true
	return s.search(query, opts)
}

func (s *Store) search(query string, opts SearchOptions) ([]SearchResult, error) {
	query = NormalizeSearchQuery(query)
	if strings.TrimSpace(query) == "" {
		return nil, ErrQueryRequired
	}
	if err := NormalizeSearchOptions(&opts); err != nil {
		return nil, err
	}
	opts.Project, _ = NormalizeProject(opts.Project)
	opts.Project = s.canonicalProjectOrRaw(opts.Project)
	if err := s.ValidateObservationType(opts.Type, opts.Project); err != nil {
		return nil, err
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > s.cfg.MaxSearchResults {
		limit = s.cfg.MaxSearchResults
	}
	if containsHan(query) && !opts.RawFTS && !hasFTSSyntax(query) {
		return s.searchCJKRanked(query, opts, limit)
	}

	var directResults []SearchResult
	if strings.Contains(query, "/") {
		tkSQL := `
			SELECT ` + observationSelectColumns + `
			FROM observations
			WHERE topic_key = ? AND deleted_at IS NULL AND archived_at IS NULL
		`
		tkArgs := []any{query}

		if opts.Type != "" {
			tkSQL += " AND type = ?"
			tkArgs = append(tkArgs, opts.Type)
		}
		if opts.Project != "" {
			tkSQL += " AND LOWER(project) = ?"
			tkArgs = append(tkArgs, opts.Project)
		}
		if opts.Scope != "" {
			tkSQL += " AND scope = ?"
			tkArgs = append(tkArgs, normalizeScope(opts.Scope))
		}
		tkSQL += memoryClassProfileClause(opts.Profile, "memory_class")
		if opts.Since != "" {
			tkSQL += " AND created_at >= ?"
			tkArgs = append(tkArgs, opts.Since)
		}
		if opts.Until != "" {
			tkSQL += " AND created_at < ?"
			tkArgs = append(tkArgs, opts.Until)
		}

		if opts.RootPath != "" {
			tkSQL += " AND (memory_path = ? OR memory_path LIKE ? ESCAPE '\\')"
			tkArgs = append(tkArgs, opts.RootPath, searchPathPattern(opts.RootPath))
		}
		if !opts.IncludeStale {
			tkSQL += " AND stale_at IS NULL AND summary_state != 'stale' AND " + validObservationSourcesSQL("observations")
		}
		tkSQL += " ORDER BY pinned DESC, last_recall_at DESC, created_at DESC, id DESC LIMIT ? OFFSET ?"
		tkArgs = append(tkArgs, limit, maxSearchOffset(opts.Offset))

		tkRows, err := s.queryItHook(s.db, tkSQL, tkArgs...)
		if err == nil {
			defer tkRows.Close()
			for tkRows.Next() {
				var sr SearchResult
				if err := tkRows.Scan(
					&sr.ID, &sr.SyncID, &sr.SessionID, &sr.Type, &sr.MemoryClass,
					&sr.MemoryPath, &sr.Abstract, &sr.Overview, &sr.SummaryState, &sr.SummaryRevision, &sr.StaleAt, &sr.StaleReason,
					&sr.Title, &sr.Content,
					&sr.ToolName, &sr.Project, &sr.Scope, &sr.TopicKey, &sr.RevisionCount, &sr.DuplicateCount,
					&sr.LastSeenAt, &sr.ReviewAfter, &sr.RecallCount, &sr.LastRecallAt,
					&sr.Pinned, &sr.CreatedAt, &sr.UpdatedAt, &sr.DeletedAt, &sr.ArchivedAt,
				); err != nil {
					break
				}
				sr.Rank = -1000
				directResults = append(directResults, sr)
			}
		}
	}

	// Build FTS5 query: "all" (default) uses AND semantics; "any" uses OR for broader recall.
	var ftsQuery string
	if opts.RawFTS {
		ftsQuery = query
	} else if opts.MatchMode == "any" {
		ftsQuery = sanitizeFTSCandidates(query)
	} else {
		ftsQuery = sanitizeFTS(query)
	}

	// FTS must drive the join so ancestry checks run only for lexical matches.
	sqlQ := `
		SELECT o.id, ifnull(o.sync_id, '') as sync_id, o.session_id, o.type, o.memory_class,
		       ifnull(o.memory_path, ''), ifnull(o.abstract, ''), ifnull(o.overview, ''), o.summary_state, o.summary_revision, o.stale_at, o.stale_reason,
		       o.title, o.content, o.tool_name, o.project, o.scope, o.topic_key, o.revision_count, o.duplicate_count,
		       o.last_seen_at, o.review_after, o.recall_count, o.last_recall_at, o.pinned, o.created_at, o.updated_at, o.deleted_at, o.archived_at,
		       bm25(observations_fts,3.0,1.0,0.0,0.0,0.0,2.0)
		FROM observations_fts fts
		CROSS JOIN observations o ON o.id = fts.rowid
		WHERE observations_fts MATCH ? AND o.deleted_at IS NULL AND o.archived_at IS NULL
	`
	args := []any{ftsQuery}

	if opts.Type != "" {
		sqlQ += " AND o.type = ?"
		args = append(args, opts.Type)
	}

	if opts.Project != "" {
		sqlQ += " AND LOWER(o.project) = ?"
		args = append(args, opts.Project)
	}

	if opts.Scope != "" {
		sqlQ += " AND o.scope = ?"
		args = append(args, normalizeScope(opts.Scope))
	}
	sqlQ += memoryClassProfileClause(opts.Profile, "o.memory_class")
	if opts.Since != "" {
		sqlQ += " AND o.created_at >= ?"
		args = append(args, opts.Since)
	}
	if opts.Until != "" {
		sqlQ += " AND o.created_at < ?"
		args = append(args, opts.Until)
	}

	if opts.RootPath != "" {
		sqlQ += " AND (o.memory_path = ? OR o.memory_path LIKE ? ESCAPE '\\')"
		args = append(args, opts.RootPath, searchPathPattern(opts.RootPath))
	}
	if !opts.IncludeStale {
		sqlQ += " AND o.stale_at IS NULL AND o.summary_state != 'stale' AND " + validObservationSourcesSQL("o")
	}
	sqlQ += " ORDER BY bm25(observations_fts,3.0,1.0,0.0,0.0,0.0,2.0), o.pinned DESC, o.last_recall_at DESC, o.created_at DESC, o.id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, maxSearchOffset(opts.Offset))

	rows, err := s.queryItHook(s.db, sqlQ, args...)
	if err != nil {
		return nil, searchQueryError(err, opts.RawFTS)
	}
	defer rows.Close()

	seen := make(map[int64]bool)
	for _, dr := range directResults {
		seen[dr.ID] = true
	}

	var results []SearchResult
	results = append(results, directResults...)
	for rows.Next() {
		var sr SearchResult
		if err := rows.Scan(
			&sr.ID, &sr.SyncID, &sr.SessionID, &sr.Type, &sr.MemoryClass,
			&sr.MemoryPath, &sr.Abstract, &sr.Overview, &sr.SummaryState, &sr.SummaryRevision, &sr.StaleAt, &sr.StaleReason,
			&sr.Title, &sr.Content,
			&sr.ToolName, &sr.Project, &sr.Scope, &sr.TopicKey, &sr.RevisionCount, &sr.DuplicateCount,
			&sr.LastSeenAt, &sr.ReviewAfter, &sr.RecallCount, &sr.LastRecallAt,
			&sr.Pinned, &sr.CreatedAt, &sr.UpdatedAt, &sr.DeletedAt, &sr.ArchivedAt,
			&sr.Rank,
		); err != nil {
			return nil, err
		}
		if !seen[sr.ID] {
			sr.ScoreSource = "fts5_bm25"
			results = append(results, sr)
			seen[sr.ID] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, searchQueryError(err, opts.RawFTS)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	if !opts.RawFTS && opts.MatchMode != "any" && len(results) < limit && !hasFTSSyntax(query) {
		fallback, err := s.searchSubstringFallback(query, opts, limit-len(results), seen)
		if err != nil {
			return nil, fmt.Errorf("search substring fallback: %w", err)
		}
		results = append(results, fallback...)
	}

	if len(results) > limit {
		results = results[:limit]
	}
	orderSearchResultsByRecall(results)
	return results, nil
}

func searchQueryError(err error, raw bool) error {
	if raw && (strings.Contains(err.Error(), "fts5:") || strings.Contains(err.Error(), "unterminated string") || strings.Contains(err.Error(), "no such column:")) {
		return ErrInvalidQuery
	}
	return fmt.Errorf("search: %w", err)
}

func searchRecencyLess(a, b SearchResult) bool {
	if a.Pinned != b.Pinned {
		return a.Pinned
	}
	if derefString(a.LastRecallAt) != derefString(b.LastRecallAt) {
		return derefString(a.LastRecallAt) > derefString(b.LastRecallAt)
	}
	if a.CreatedAt != b.CreatedAt {
		return a.CreatedAt > b.CreatedAt
	}
	return a.ID > b.ID
}

func orderSearchResultsByRecall(results []SearchResult) {
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Rank != results[j].Rank {
			return results[i].Rank < results[j].Rank
		}
		return searchRecencyLess(results[i], results[j])
	})
}

func memoryClassProfileClause(profile, column string) string {
	switch profile {
	case SearchProfileSemantic:
		return " AND " + column + " IN ('user', 'agent', 'project')"
	case SearchProfileOperational:
		return " AND " + column + " = 'operational'"
	default:
		return ""
	}
}

// ─── Stats ───────────────────────────────────────────────────────────────────

// ProjectStats returns counts scoped to one project key/ID. It is the scoped
// counterpart of Stats() for operator surfaces that pin a project.
func (s *Store) ProjectStats(project string) (*Stats, error) {
	project = s.canonicalProjectOrRaw(project)
	project = strings.TrimSpace(project)
	if project == "" {
		return s.Stats()
	}
	stats := &Stats{Projects: []string{project}}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE LOWER(project) = ?`, project).Scan(&stats.TotalSessions); err != nil {
		return nil, fmt.Errorf("project stats sessions: %w", err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM observations WHERE deleted_at IS NULL AND LOWER(project) = ?`, project).Scan(&stats.TotalObservations); err != nil {
		return nil, fmt.Errorf("project stats observations: %w", err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM user_prompts WHERE LOWER(project) = ?`, project).Scan(&stats.TotalPrompts); err != nil {
		return nil, fmt.Errorf("project stats prompts: %w", err)
	}
	return stats, nil
}

func (s *Store) Stats() (*Stats, error) {
	stats := &Stats{Projects: []string{}}
	for _, count := range []struct {
		query  string
		target *int
	}{
		{"SELECT COUNT(*) FROM sessions", &stats.TotalSessions},
		{"SELECT COUNT(*) FROM observations WHERE deleted_at IS NULL", &stats.TotalObservations},
		{"SELECT COUNT(*) FROM user_prompts", &stats.TotalPrompts},
	} {
		if err := s.db.QueryRow(count.query).Scan(count.target); err != nil {
			return nil, fmt.Errorf("stats counts: %w", err)
		}
	}

	rows, err := s.queryItHook(s.db, "SELECT project FROM observations WHERE project IS NOT NULL AND project != '' AND deleted_at IS NULL GROUP BY project ORDER BY MAX(created_at) DESC")
	if err != nil {
		return nil, fmt.Errorf("stats projects: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("stats project row: %w", err)
		}
		stats.Projects = append(stats.Projects, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("stats project rows: %w", err)
	}
	return stats, nil
}

// MemoryClassificationStats returns read-only classification counts and legacy
// types that fell back to project memory because they are not in the
// deterministic classification map.
func (s *Store) MemoryClassificationStats(project string) (*MemoryClassificationStats, error) {
	project, _ = NormalizeProject(project)
	stats := &MemoryClassificationStats{ClassCounts: map[string]int{
		MemoryClassOperational: 0,
		MemoryClassUser:        0,
		MemoryClassAgent:       0,
		MemoryClassProject:     0,
	}}

	query := `
		SELECT o.memory_class, o.type, COUNT(*)
		FROM observations o
		LEFT JOIN sessions s ON s.id = o.session_id
		WHERE o.deleted_at IS NULL`
	args := []any{}
	if strings.TrimSpace(project) != "" {
		query += ` AND lower(coalesce(nullif(o.project, ''), s.project, '')) = ?`
		args = append(args, project)
	}
	query += ` GROUP BY o.memory_class, o.type ORDER BY lower(o.type), o.type`

	rows, err := s.queryItHook(s.db, query, args...)
	if err != nil {
		return nil, fmt.Errorf("memory classification stats: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var memoryClass, observationType string
		var count int
		if err := rows.Scan(&memoryClass, &observationType, &count); err != nil {
			return nil, fmt.Errorf("memory classification stats: scan: %w", err)
		}
		stats.Total += count
		stats.ClassCounts[memoryClass] += count
		if !IsKnownMemoryType(observationType) {
			stats.AmbiguousTypes = append(stats.AmbiguousTypes, MemoryClassificationTypeCount{
				Type:  observationType,
				Count: count,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory classification stats: rows: %w", err)
	}
	return stats, nil
}

// ─── Project Existence ───────────────────────────────────────────────────────

// ProjectExists returns true if the named project has at least one record in
// any of observations, sessions, prompts, or enrollment tables.
// Uses a single UNION ALL LIMIT 1 query for efficiency (REQ-315).
// The sync_enrolled_projects branch ensures a project enrolled via EnrollProject()
// without any other data is still recognized (JC1).
func (s *Store) ProjectExists(name string) (bool, error) {
	// Preserve exact raw keys, including Unicode that SQLite's ASCII-only
	// LOWER cannot fold. Normalized ASCII callers retain legacy case matching.
	normalized, _ := NormalizeProject(name)
	const query = `
SELECT 1 FROM (
  SELECT project FROM observations WHERE (project = ? OR LOWER(project) = ?) AND deleted_at IS NULL
  UNION ALL
  SELECT project FROM sessions WHERE project = ? OR LOWER(project) = ?
  UNION ALL
  SELECT project FROM user_prompts WHERE project = ? OR LOWER(project) = ?
  UNION ALL
  SELECT project FROM sync_enrolled_projects WHERE project = ? OR LOWER(project) = ?
  UNION ALL
  SELECT id AS project FROM project_identities WHERE id = ?
) LIMIT 1`
	var dummy int
	err := s.db.QueryRow(query, name, normalized, name, normalized, name, normalized, name, normalized, normalized).Scan(&dummy)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ─── Context Formatting ─────────────────────────────────────────────────────

func (s *Store) FormatContext(project, scope string) (string, error) {
	var scopeErr error
	scope, scopeErr = NormalizeReadScope(scope)
	if scopeErr != nil {
		return "", scopeErr
	}
	sessions, err := s.RecentSessions(project, 5)
	if err != nil {
		return "", err
	}

	pinned, err := s.PinnedObservations(project, scope)
	if err != nil {
		return "", err
	}

	observations, err := s.recentUnpinnedObservations(project, scope, s.cfg.MaxContextResults)
	if err != nil {
		return "", err
	}

	prompts, err := s.RecentPrompts(project, 10)
	if err != nil {
		return "", err
	}

	if len(sessions) == 0 && len(pinned) == 0 && len(observations) == 0 && len(prompts) == 0 {
		return "", nil
	}

	var b strings.Builder
	b.WriteString("## Memory from Previous Sessions\n\n")

	if len(sessions) > 0 {
		b.WriteString("### Recent Sessions\n")
		for _, sess := range sessions {
			summary := ""
			if sess.Summary != nil {
				summary = fmt.Sprintf(": %s", truncate(*sess.Summary, 200))
			}
			fmt.Fprintf(&b, "- **%s** (project=%s, directory=%s, %s)%s [%d observations]\n",
				sess.ID, sess.Project, sess.Directory, timeutil.FormatLocal(sess.StartedAt), summary, sess.ObservationCount)
		}
		b.WriteString("\n")
	}

	if len(prompts) > 0 {
		b.WriteString("### Recent User Prompts\n")
		for _, p := range prompts {
			fmt.Fprintf(&b, "- %s: %s\n", timeutil.FormatLocal(p.CreatedAt), truncate(p.Content, 200))
		}
		b.WriteString("\n")
	}

	if len(pinned) > 0 {
		b.WriteString("### Pinned\n")
		for _, obs := range pinned {
			fmt.Fprintf(&b, "- [%s] **%s**: %s\n",
				obs.Type, obs.Title, truncate(obs.Content, 300))
		}
		b.WriteString("\n")
	}

	if len(observations) > 0 {
		b.WriteString("### Recent Observations\n")
		for _, obs := range observations {
			fmt.Fprintf(&b, "- [%s] **%s**: %s\n",
				obs.Type, obs.Title, truncate(obs.Content, 300))
		}
		b.WriteString("\n")
	}

	return b.String(), nil
}

// ─── Export / Import ─────────────────────────────────────────────────────────

func (s *Store) Export() (*ExportData, error) {
	return s.exportWithProjectScope("")
}

// ExportProject returns an export restricted to records relevant to a single
// normalized project. This avoids full-database exports when only one project
// needs to sync.
func (s *Store) ExportProject(project string) (*ExportData, error) {
	normalizedProject, _ := NormalizeProject(project)
	normalizedProject = strings.TrimSpace(normalizedProject)
	if normalizedProject == "" {
		return nil, fmt.Errorf("project is required")
	}
	return s.exportWithProjectScope(normalizedProject)
}

// ExportRelationMutations returns relation upsert mutations for non-orphaned
// relation rows whose source and target observations are available locally.
func (s *Store) ExportRelationMutations(project string) ([]SyncMutation, error) {
	normalizedProject, _ := NormalizeProject(project)
	normalizedProject = strings.TrimSpace(normalizedProject)

	query := `
		SELECT r.sync_id, r.source_id, r.target_id, r.relation, r.reason, r.evidence, r.confidence,
		       r.judgment_status, r.marked_by_actor, r.marked_by_kind, r.marked_by_model,
		       r.session_id, coalesce(nullif(src.project, ''), src_s.project, ''), r.created_at, r.updated_at
		FROM memory_relations r
		JOIN observations src ON src.sync_id = r.source_id AND src.deleted_at IS NULL
		JOIN observations tgt ON tgt.sync_id = r.target_id AND tgt.deleted_at IS NULL
		LEFT JOIN sessions src_s ON src_s.id = src.session_id
		LEFT JOIN sessions tgt_s ON tgt_s.id = tgt.session_id
		WHERE r.judgment_status != ?`
	args := []any{JudgmentStatusOrphaned}
	if normalizedProject != "" {
		query += ` AND coalesce(nullif(src.project, ''), src_s.project, '') = ?
			AND coalesce(nullif(tgt.project, ''), tgt_s.project, '') = ?`
		args = append(args, normalizedProject, normalizedProject)
	}
	query += ` ORDER BY r.created_at, r.sync_id`

	rows, err := s.queryItHook(s.db, query, args...)
	if err != nil {
		return nil, fmt.Errorf("export relation mutations: %w", err)
	}
	defer rows.Close()

	mutations := []SyncMutation{}
	for rows.Next() {
		var p syncRelationPayload
		if err := rows.Scan(
			&p.SyncID, &p.SourceID, &p.TargetID, &p.Relation, &p.Reason, &p.Evidence, &p.Confidence,
			&p.JudgmentStatus, &p.MarkedByActor, &p.MarkedByKind, &p.MarkedByModel,
			&p.SessionID, &p.Project, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("export relation mutations: scan: %w", err)
		}
		payload, err := json.Marshal(p)
		if err != nil {
			return nil, fmt.Errorf("export relation mutations: marshal %s: %w", p.SyncID, err)
		}
		mutations = append(mutations, SyncMutation{
			Entity:     SyncEntityRelation,
			EntityKey:  strings.TrimSpace(p.SyncID),
			Op:         SyncOpUpsert,
			Payload:    string(payload),
			Project:    strings.TrimSpace(p.Project),
			OccurredAt: p.UpdatedAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("export relation mutations: rows: %w", err)
	}
	return mutations, nil
}

func (s *Store) exportWithProjectScope(project string) (*ExportData, error) {
	data := &ExportData{
		Version:    "0.1.0",
		ExportedAt: Now(),
	}

	sessionQuery := "SELECT id, project, directory, started_at, ended_at, summary FROM sessions"
	sessionArgs := []any{}
	if project != "" {
		sessionQuery += `
			WHERE project = ?
			   OR id IN (
				SELECT session_id FROM observations
				 WHERE ifnull(project, '') = ?
				    OR (ifnull(project, '') = '' AND session_id IN (SELECT id FROM sessions WHERE project = ?))
				UNION
				SELECT session_id FROM user_prompts
				 WHERE ifnull(project, '') = ?
				    OR (ifnull(project, '') = '' AND session_id IN (SELECT id FROM sessions WHERE project = ?))
			)`
		sessionArgs = append(sessionArgs, project, project, project, project, project)
	}
	sessionQuery += " ORDER BY started_at"

	// Sessions
	rows, err := s.queryItHook(s.db,
		sessionQuery,
		sessionArgs...,
	)
	if err != nil {
		return nil, fmt.Errorf("export sessions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sess Session
		if err := rows.Scan(&sess.ID, &sess.Project, &sess.Directory, &sess.StartedAt, &sess.EndedAt, &sess.Summary); err != nil {
			return nil, err
		}
		data.Sessions = append(data.Sessions, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Observations
	obsQuery := `SELECT ` + observationSelectColumns + `
	 FROM observations`
	obsArgs := []any{}
	if project != "" {
		obsQuery += `
			WHERE ifnull(project, '') = ?
			   OR (ifnull(project, '') = '' AND session_id IN (SELECT id FROM sessions WHERE project = ?))`
		obsArgs = append(obsArgs, project, project)
	}
	obsQuery += " ORDER BY id"
	obsRows, err := s.queryItHook(s.db, obsQuery, obsArgs...)
	if err != nil {
		return nil, fmt.Errorf("export observations: %w", err)
	}
	defer obsRows.Close()
	for obsRows.Next() {
		var o Observation
		if err := scanObservationRow(obsRows, &o); err != nil {
			return nil, err
		}
		data.Observations = append(data.Observations, o)
	}
	if err := obsRows.Err(); err != nil {
		return nil, err
	}

	// Prompts
	promptQuery := "SELECT id, ifnull(sync_id, '') as sync_id, session_id, content, ifnull(project, '') as project, created_at FROM user_prompts"
	promptArgs := []any{}
	if project != "" {
		promptQuery += `
			WHERE ifnull(project, '') = ?
			   OR (ifnull(project, '') = '' AND session_id IN (SELECT id FROM sessions WHERE project = ?))`
		promptArgs = append(promptArgs, project, project)
	}
	promptQuery += " ORDER BY id"
	promptRows, err := s.queryItHook(s.db, promptQuery, promptArgs...)
	if err != nil {
		return nil, fmt.Errorf("export prompts: %w", err)
	}
	defer promptRows.Close()
	for promptRows.Next() {
		var p Prompt
		if err := promptRows.Scan(&p.ID, &p.SyncID, &p.SessionID, &p.Content, &p.Project, &p.CreatedAt); err != nil {
			return nil, err
		}
		data.Prompts = append(data.Prompts, p)
	}
	if err := promptRows.Err(); err != nil {
		return nil, err
	}

	dependencyQuery := `
		SELECT d.id, d.sync_id,
		       ifnull(source.id, 0), d.source_observation_sync_id,
		       ifnull(dependent.id, 0), d.dependent_observation_sync_id,
		       d.project, ifnull(d.reason, ''), d.source_revision, d.status,
		       d.created_at, d.updated_at
		FROM memory_dependencies d
		LEFT JOIN observations source ON source.sync_id = d.source_observation_sync_id
		LEFT JOIN observations dependent ON dependent.sync_id = d.dependent_observation_sync_id`
	dependencyArgs := []any{}
	if project != "" {
		dependencyQuery += ` WHERE d.project = ?`
		dependencyArgs = append(dependencyArgs, project)
	}
	dependencyQuery += ` ORDER BY d.id`
	dependencyRows, err := s.queryItHook(s.db, dependencyQuery, dependencyArgs...)
	if err != nil {
		return nil, fmt.Errorf("export dependencies: %w", err)
	}
	defer dependencyRows.Close()
	for dependencyRows.Next() {
		var dependency MemoryDependency
		if err := scanMemoryDependency(dependencyRows, &dependency); err != nil {
			return nil, err
		}
		data.Dependencies = append(data.Dependencies, dependency)
	}
	if err := dependencyRows.Err(); err != nil {
		return nil, err
	}

	resourceDependencyQuery := `
		SELECT d.id, d.sync_id, d.source_ref,
		       ifnull(dependent.id, 0), d.dependent_observation_sync_id,
		       d.project, ifnull(d.reason, ''), d.status,
		       d.replacement_source_ref, d.created_at, d.updated_at
		FROM resource_dependencies d
		LEFT JOIN observations dependent ON dependent.sync_id = d.dependent_observation_sync_id`
	resourceDependencyArgs := []any{}
	if project != "" {
		resourceDependencyQuery += ` WHERE d.project = ?`
		resourceDependencyArgs = append(resourceDependencyArgs, project)
	}
	resourceDependencyQuery += ` ORDER BY d.id`
	resourceDependencyRows, err := s.queryItHook(s.db, resourceDependencyQuery, resourceDependencyArgs...)
	if err != nil {
		return nil, fmt.Errorf("export resource dependencies: %w", err)
	}
	defer resourceDependencyRows.Close()
	for resourceDependencyRows.Next() {
		var dependency ResourceDependency
		if err := scanResourceDependency(resourceDependencyRows, &dependency); err != nil {
			return nil, err
		}
		data.ResourceDependencies = append(data.ResourceDependencies, dependency)
	}
	if err := resourceDependencyRows.Err(); err != nil {
		return nil, err
	}

	identities, err := s.ListRegisteredProjects()
	if err != nil {
		return nil, err
	}
	for _, identity := range identities {
		if project == "" || identity.ID == project {
			data.ProjectIdentities = append(data.ProjectIdentities, identity)
		}
	}
	return data, nil
}

func (s *Store) Import(data *ExportData) (*ImportResult, error) {
	if data == nil {
		return nil, errors.New("import data is required")
	}
	var err error
	data, err = normalizeExchangeTimestamps(data)
	if err != nil {
		return nil, fmt.Errorf("import timestamps: %w", err)
	}
	tx, err := s.beginTxHook()
	if err != nil {
		return nil, fmt.Errorf("import: begin tx: %w", err)
	}
	defer tx.Rollback()

	result := &ImportResult{}
	pathsToRebuild := make(map[string][2]string)

	if err := importRegisteredProjects(tx, data.ProjectIdentities); err != nil {
		return nil, err
	}

	// Import sessions (skip duplicates)
	for _, sess := range data.Sessions {
		if isRegisteredProjectID(sess.Project) {
			var count int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM project_identities WHERE id=?`, sess.Project).Scan(&count); err != nil {
				return nil, err
			}
			if count != 1 {
				return nil, fmt.Errorf("%w: exchange lacks project identity", ErrUnknownRegisteredProject)
			}
		}
		var existingProject string
		lookupErr := tx.QueryRow(`SELECT ifnull(project, '') FROM sessions WHERE id = ?`, sess.ID).Scan(&existingProject)
		if lookupErr == nil && existingProject != sess.Project {
			return nil, fmt.Errorf("%w: session belongs to another project", ErrImportIdentityConflict)
		}
		if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
			return nil, fmt.Errorf("import session identity: %w", lookupErr)
		}
		res, err := s.execHook(tx,
			`INSERT OR IGNORE INTO sessions (id, project, directory, started_at, ended_at, summary)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			sess.ID, sess.Project, sess.Directory, sess.StartedAt, sess.EndedAt, sess.Summary,
		)
		if err != nil {
			return nil, fmt.Errorf("import session %s: %w", sess.ID, err)
		}
		n, _ := res.RowsAffected()
		result.SessionsImported += int(n)
	}

	// Import observations (use new IDs — AUTOINCREMENT)
	for _, obs := range data.Observations {
		memoryClass, err := resolveMemoryClass(obs.MemoryClass, obs.Type)
		if err != nil {
			return nil, fmt.Errorf("import observation %d: %w", obs.ID, err)
		}
		memoryPath, err := NormalizeMemoryPath(memoryClass, obs.MemoryPath)
		if err != nil {
			return nil, fmt.Errorf("import observation %d: %w", obs.ID, err)
		}
		summaryState, summaryRevision, err := normalizeSummaryFields(obs.Abstract, obs.Overview, obs.SummaryState, obs.SummaryRevision)
		if err != nil {
			return nil, fmt.Errorf("import observation %d: %w", obs.ID, err)
		}
		syncID, err := importSyncID(obs.SyncID, "obs", obs)
		if err != nil {
			return nil, err
		}
		obs.SyncID, obs.MemoryClass, obs.MemoryPath = syncID, memoryClass, memoryPath
		obs.SummaryState, obs.SummaryRevision = summaryState, summaryRevision
		obs.Scope = normalizeScope(obs.Scope)
		obs.TopicKey = nullableString(normalizeTopicKey(derefString(obs.TopicKey)))
		obs.RevisionCount = maxInt(obs.RevisionCount, 1)
		var tombstone int
		if err := tx.QueryRow(`SELECT count(*) FROM observation_tombstones WHERE sync_id=?`, syncID).Scan(&tombstone); err != nil {
			return nil, err
		}
		if tombstone != 0 {
			result.ObservationsSkipped++
			continue
		}
		if exists, err := s.importedObservationExistsTx(tx, obs); err != nil {
			return nil, fmt.Errorf("import observation %d: %w", obs.ID, err)
		} else if exists {
			result.ObservationsSkipped++
			continue
		}
		_, err = s.execHook(tx,
			`INSERT INTO observations (
				sync_id, session_id, type, memory_class, memory_path, abstract, overview,
				summary_state, summary_revision, stale_at, stale_reason, title, content,
				tool_name, project, scope, topic_key, normalized_hash, revision_count,
				duplicate_count, last_seen_at, review_after, recall_count, last_recall_at, created_at, updated_at, deleted_at, archived_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			syncID,
			obs.SessionID,
			obs.Type,
			memoryClass,
			nullableString(memoryPath),
			nullableString(obs.Abstract),
			nullableString(obs.Overview),
			summaryState,
			summaryRevision,
			obs.StaleAt,
			obs.StaleReason,
			obs.Title,
			obs.Content,
			obs.ToolName,
			obs.Project,
			normalizeScope(obs.Scope),
			nullableString(normalizeTopicKey(derefString(obs.TopicKey))),
			hashNormalized(obs.Content),
			maxInt(obs.RevisionCount, 1),
			maxInt(obs.DuplicateCount, 1),
			obs.LastSeenAt,
			obs.ReviewAfter,
			obs.RecallCount,
			obs.LastRecallAt,
			obs.CreatedAt,
			obs.UpdatedAt,
			obs.DeletedAt,
			obs.ArchivedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("import observation %d: %w", obs.ID, err)
		}
		project := derefString(obs.Project)
		key := project + "\x00" + normalizeScope(obs.Scope)
		pathsToRebuild[key] = [2]string{project, normalizeScope(obs.Scope)}
		result.ObservationsImported++
	}
	for _, pair := range pathsToRebuild {
		if err := s.rebuildMemoryNodesTx(tx, pair[0], pair[1]); err != nil {
			return nil, fmt.Errorf("import memory nodes: %w", err)
		}
	}

	for _, dependency := range data.Dependencies {
		if err := s.applyDependencyUpsertTx(tx, dependencyPayloadFromMemoryDependency(&dependency)); err != nil {
			return nil, fmt.Errorf("import dependency %s: %w", dependency.SyncID, err)
		}
		result.DependenciesImported++
	}
	for _, dependency := range data.ResourceDependencies {
		if err := s.applyResourceDependencyUpsertTx(tx, resourceDependencyPayloadFromResourceDependency(&dependency)); err != nil {
			return nil, fmt.Errorf("import resource dependency %s: %w", dependency.SyncID, err)
		}
		result.ResourceDependenciesImported++
	}

	// Import prompts
	for _, p := range data.Prompts {
		syncID, err := importSyncID(p.SyncID, "prompt", p)
		if err != nil {
			return nil, err
		}
		p.SyncID = syncID
		if exists, err := importedPromptExistsTx(tx, p); err != nil {
			return nil, err
		} else if exists {
			result.PromptsSkipped++
			continue
		}
		_, err = s.execHook(tx,
			`INSERT INTO user_prompts (sync_id, session_id, content, project, created_at)
			 VALUES (?, ?, ?, ?, ?)`,
			syncID, p.SessionID, p.Content, p.Project, p.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("import prompt %d: %w", p.ID, err)
		}
		result.PromptsImported++
	}

	if err := s.commitHook(tx); err != nil {
		return nil, fmt.Errorf("import: commit: %w", err)
	}

	return result, nil
}

type ImportResult struct {
	SessionsImported             int `json:"sessions_imported"`
	ObservationsImported         int `json:"observations_imported"`
	ObservationsSkipped          int `json:"observations_skipped,omitempty"`
	PromptsImported              int `json:"prompts_imported"`
	PromptsSkipped               int `json:"prompts_skipped,omitempty"`
	DependenciesImported         int `json:"dependencies_imported"`
	ResourceDependenciesImported int `json:"resource_dependencies_imported"`
}

// ─── Sync Chunk Tracking ─────────────────────────────────────────────────────

// GetSyncedChunks returns local-target chunk IDs for backwards compatibility.
func (s *Store) GetSyncedChunks() (map[string]bool, error) {
	return s.GetSyncedChunksForTarget(LocalChunkTargetKey)
}

// GetSyncedChunksForTarget returns chunk IDs tracked for a specific sync target.
func (s *Store) GetSyncedChunksForTarget(targetKey string) (map[string]bool, error) {
	targetKey = normalizeChunkTargetKey(targetKey)
	rows, err := s.queryItHook(s.db, "SELECT chunk_id FROM sync_chunks WHERE target_key = ?", targetKey)
	if err != nil {
		return nil, fmt.Errorf("get synced chunks: %w", err)
	}
	defer rows.Close()

	chunks := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		chunks[id] = true
	}
	return chunks, rows.Err()
}

// RecordSyncedChunk marks a local-target chunk as imported/exported.
func (s *Store) RecordSyncedChunk(chunkID string) error {
	return s.RecordSyncedChunkForTarget(LocalChunkTargetKey, chunkID)
}

// RecordSyncedChunkForTarget marks a chunk as imported/exported for a target.
func (s *Store) RecordSyncedChunkForTarget(targetKey, chunkID string) error {
	targetKey = normalizeChunkTargetKey(targetKey)
	_, err := s.execHook(s.db,
		"INSERT OR IGNORE INTO sync_chunks (target_key, chunk_id) VALUES (?, ?)",
		targetKey, chunkID,
	)
	return err
}

// ─── Local Sync State & Mutation Journal ─────────────────────────────────────

func (s *Store) GetSyncState(targetKey string) (*SyncState, error) {
	targetKey = normalizeSyncTargetKey(targetKey)
	if err := s.ensureSyncState(targetKey); err != nil {
		return nil, err
	}
	return s.getSyncState(targetKey)
}

func (s *Store) ListPendingSyncMutations(targetKey string, limit int) ([]SyncMutation, error) {
	targetKey = normalizeSyncTargetKey(targetKey)
	if limit <= 0 {
		limit = 100
	}
	// Only return mutations for enrolled projects or empty-project (global) mutations.
	// Empty-project mutations always sync regardless of enrollment.
	rows, err := s.queryItHook(s.db, `
		SELECT sm.seq, sm.target_key, sm.entity, sm.entity_key, sm.op, sm.payload, sm.source, sm.project, sm.occurred_at, sm.acked_at
		FROM sync_mutations sm
		LEFT JOIN sync_enrolled_projects sep ON sm.project = sep.project
		WHERE sm.target_key = ? AND sm.acked_at IS NULL
		  AND (sm.project = '' OR sep.project IS NOT NULL)
		ORDER BY sm.seq ASC
		LIMIT ?`, targetKey, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var mutations []SyncMutation
	for rows.Next() {
		var mutation SyncMutation
		if err := rows.Scan(&mutation.Seq, &mutation.TargetKey, &mutation.Entity, &mutation.EntityKey, &mutation.Op, &mutation.Payload, &mutation.Source, &mutation.Project, &mutation.OccurredAt, &mutation.AckedAt); err != nil {
			return nil, err
		}
		mutations = append(mutations, mutation)
	}
	return mutations, rows.Err()
}

func (s *Store) ListPendingSyncMutationsAfterSeq(targetKey string, afterSeq int64, limit int) ([]SyncMutation, error) {
	targetKey = normalizeSyncTargetKey(targetKey)
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.queryItHook(s.db, `
		SELECT sm.seq, sm.target_key, sm.entity, sm.entity_key, sm.op, sm.payload, sm.source, sm.project, sm.occurred_at, sm.acked_at
		FROM sync_mutations sm
		LEFT JOIN sync_enrolled_projects sep ON sm.project = sep.project
		WHERE sm.target_key = ? AND sm.acked_at IS NULL
		  AND sm.seq > ?
		  AND (sm.project = '' OR sep.project IS NOT NULL)
		ORDER BY sm.seq ASC
		LIMIT ?`, targetKey, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	mutations := make([]SyncMutation, 0, limit)
	for rows.Next() {
		var mutation SyncMutation
		if err := rows.Scan(&mutation.Seq, &mutation.TargetKey, &mutation.Entity, &mutation.EntityKey, &mutation.Op, &mutation.Payload, &mutation.Source, &mutation.Project, &mutation.OccurredAt, &mutation.AckedAt); err != nil {
			return nil, err
		}
		mutations = append(mutations, mutation)
	}
	return mutations, rows.Err()
}

func (s *Store) CountPendingNonEnrolledSyncMutations(targetKey string) ([]PendingSyncMutationProjectCount, error) {
	targetKey = normalizeSyncTargetKey(targetKey)
	rows, err := s.queryItHook(s.db, `
		SELECT sm.project, COUNT(*), COALESCE(SUM(length(CAST(sm.payload AS BLOB))), 0), MAX(sm.seq)
		FROM sync_mutations sm
		LEFT JOIN sync_enrolled_projects sep ON sm.project = sep.project
		WHERE sm.target_key = ?
		  AND sm.acked_at IS NULL
		  AND sm.project != ''
		  AND sep.project IS NULL
		GROUP BY sm.project
		ORDER BY sm.project ASC`, targetKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := []PendingSyncMutationProjectCount{}
	for rows.Next() {
		var count PendingSyncMutationProjectCount
		if err := rows.Scan(&count.Project, &count.Count, &count.PayloadBytes, &count.MaxSeq); err != nil {
			return nil, err
		}
		counts = append(counts, count)
	}
	return counts, rows.Err()
}

// SkipAckNonEnrolledMutations acks (marks as skipped) all pending mutations
// that belong to non-enrolled projects, preventing journal bloat. Empty-project
// mutations are never skipped — they always sync regardless of enrollment.
func (s *Store) SkipAckNonEnrolledMutations(targetKey string) (int64, error) {
	counts, err := s.CountPendingNonEnrolledSyncMutations(targetKey)
	if err != nil {
		return 0, err
	}
	var maxSeq int64
	for _, count := range counts {
		if count.MaxSeq > maxSeq {
			maxSeq = count.MaxSeq
		}
	}
	return s.SkipAckNonEnrolledMutationsThrough(targetKey, maxSeq)
}

// SkipAckNonEnrolledMutationsThrough only acknowledges rows that existed in a
// previously inspected cleanup snapshot. Mutations created after maxSeq remain
// pending, so an operator backup always predates every affected row.
func (s *Store) SkipAckNonEnrolledMutationsThrough(targetKey string, maxSeq int64) (int64, error) {
	if maxSeq <= 0 {
		return 0, nil
	}
	targetKey = normalizeSyncTargetKey(targetKey)
	res, err := s.execHook(s.db, `
		UPDATE sync_mutations
		SET acked_at = datetime('now')
		WHERE target_key = ?
		  AND acked_at IS NULL
		  AND seq <= ?
		  AND project != ''
		  AND project NOT IN (SELECT project FROM sync_enrolled_projects)`,
		targetKey, maxSeq,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) AckSyncMutations(targetKey string, lastAckedSeq int64) error {
	if lastAckedSeq <= 0 {
		return nil
	}
	targetKey = normalizeSyncTargetKey(targetKey)
	return s.withTx(func(tx *sql.Tx) error {
		affectedProjects := map[string]struct{}{}
		if targetKey == DefaultSyncTargetKey {
			rows, err := s.queryItHook(tx,
				`SELECT DISTINCT ifnull(project, '') FROM sync_mutations
				 WHERE target_key = ? AND seq <= ? AND acked_at IS NULL`,
				targetKey, lastAckedSeq,
			)
			if err != nil {
				return err
			}
			for rows.Next() {
				var project string
				if err := rows.Scan(&project); err != nil {
					_ = rows.Close()
					return err
				}
				project, _ = NormalizeProject(project)
				project = strings.TrimSpace(project)
				if project != "" {
					affectedProjects[project] = struct{}{}
				}
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return err
			}
			_ = rows.Close()
		}

		state, err := s.getSyncStateTx(tx, targetKey)
		if err != nil {
			return err
		}
		if _, err := s.execHook(tx,
			`UPDATE sync_mutations SET acked_at = datetime('now') WHERE target_key = ? AND seq <= ? AND acked_at IS NULL`,
			targetKey, lastAckedSeq,
		); err != nil {
			return err
		}
		acked := state.LastAckedSeq
		if lastAckedSeq > acked {
			acked = lastAckedSeq
		}
		lifecycle := SyncLifecyclePending
		if acked >= state.LastEnqueuedSeq {
			lifecycle = SyncLifecycleHealthy
		}
		if isActivelyDegradedState(state, time.Now().UTC()) {
			lifecycle = SyncLifecycleDegraded
		}
		if lifecycle == SyncLifecycleDegraded {
			_, err = s.execHook(tx,
				`UPDATE sync_state
				 SET last_acked_seq = ?, lifecycle = ?, updated_at = datetime('now')
				 WHERE target_key = ?`,
				acked, lifecycle, targetKey,
			)
		} else {
			_, err = s.execHook(tx,
				`UPDATE sync_state
				 SET last_acked_seq = ?, lifecycle = ?, consecutive_failures = 0, backoff_until = NULL, reason_code = NULL, reason_message = NULL, last_error = NULL, updated_at = datetime('now')
				 WHERE target_key = ?`,
				acked, lifecycle, targetKey,
			)
		}
		if err != nil {
			return err
		}
		if targetKey != DefaultSyncTargetKey {
			return nil
		}
		for project := range affectedProjects {
			if err := s.refreshProjectSyncStateTx(tx, project); err != nil {
				return err
			}
		}
		return nil
	})
}

// AckSyncMutationSeqs acknowledges specific mutation sequence numbers without
// requiring them to be contiguous.
func (s *Store) AckSyncMutationSeqs(targetKey string, seqs []int64) error {
	if len(seqs) == 0 {
		return nil
	}
	targetKey = normalizeSyncTargetKey(targetKey)
	return s.withTx(func(tx *sql.Tx) error {
		affectedProjects := map[string]struct{}{}
		if targetKey == DefaultSyncTargetKey {
			for _, seq := range seqs {
				if seq <= 0 {
					continue
				}
				var project string
				err := tx.QueryRow(
					`SELECT ifnull(project, '') FROM sync_mutations WHERE target_key = ? AND seq = ?`,
					targetKey, seq,
				).Scan(&project)
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				if err != nil {
					return err
				}
				project, _ = NormalizeProject(project)
				project = strings.TrimSpace(project)
				if project != "" {
					affectedProjects[project] = struct{}{}
				}
			}
		}

		state, err := s.getSyncStateTx(tx, targetKey)
		if err != nil {
			return err
		}
		maxSeq := state.LastAckedSeq
		for _, seq := range seqs {
			if seq <= 0 {
				continue
			}
			if _, err := s.execHook(tx,
				`UPDATE sync_mutations SET acked_at = datetime('now') WHERE target_key = ? AND seq = ? AND acked_at IS NULL`,
				targetKey, seq,
			); err != nil {
				return err
			}
			if seq > maxSeq {
				maxSeq = seq
			}
		}
		var remaining int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM sync_mutations WHERE target_key = ? AND acked_at IS NULL`, targetKey).Scan(&remaining); err != nil {
			return err
		}
		lifecycle := SyncLifecyclePending
		if remaining == 0 {
			lifecycle = SyncLifecycleHealthy
		}
		if isActivelyDegradedState(state, time.Now().UTC()) {
			lifecycle = SyncLifecycleDegraded
		}
		if lifecycle == SyncLifecycleDegraded {
			_, err = s.execHook(tx,
				`UPDATE sync_state SET last_acked_seq = ?, lifecycle = ?, updated_at = datetime('now') WHERE target_key = ?`,
				maxSeq, lifecycle, targetKey,
			)
		} else {
			_, err = s.execHook(tx,
				`UPDATE sync_state SET last_acked_seq = ?, lifecycle = ?, consecutive_failures = 0, backoff_until = NULL, reason_code = NULL, reason_message = NULL, last_error = NULL, updated_at = datetime('now') WHERE target_key = ?`,
				maxSeq, lifecycle, targetKey,
			)
		}
		if err != nil {
			return err
		}
		if targetKey != DefaultSyncTargetKey {
			return nil
		}
		for project := range affectedProjects {
			if err := s.refreshProjectSyncStateTx(tx, project); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) HasPendingSyncMutationsForProject(project string) (bool, error) {
	project, _ = NormalizeProject(project)
	project = strings.TrimSpace(project)
	if project == "" {
		return false, nil
	}

	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM sync_mutations WHERE target_key = ? AND project = ? AND acked_at IS NULL`,
		DefaultSyncTargetKey,
		project,
	).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *Store) refreshProjectSyncStateTx(tx *sql.Tx, project string) error {
	project, _ = NormalizeProject(project)
	project = strings.TrimSpace(project)
	if project == "" {
		return nil
	}
	projectTargetKey := syncTargetKeyForProject(project)
	state, err := s.getSyncStateTx(tx, projectTargetKey)
	if err != nil {
		return err
	}

	var maxAckedSeq int64
	if err := tx.QueryRow(
		`SELECT ifnull(MAX(seq), 0)
		 FROM sync_mutations
		 WHERE target_key = ? AND project = ? AND acked_at IS NOT NULL`,
		DefaultSyncTargetKey,
		project,
	).Scan(&maxAckedSeq); err != nil {
		return err
	}
	if maxAckedSeq < state.LastAckedSeq {
		maxAckedSeq = state.LastAckedSeq
	}

	var maxEnqueuedSeq int64
	if err := tx.QueryRow(
		`SELECT ifnull(MAX(seq), 0)
		 FROM sync_mutations
		 WHERE target_key = ? AND project = ?`,
		DefaultSyncTargetKey,
		project,
	).Scan(&maxEnqueuedSeq); err != nil {
		return err
	}
	if maxEnqueuedSeq < state.LastEnqueuedSeq {
		maxEnqueuedSeq = state.LastEnqueuedSeq
	}

	var pendingCount int
	if err := tx.QueryRow(
		`SELECT COUNT(*)
		 FROM sync_mutations
		 WHERE target_key = ? AND project = ? AND acked_at IS NULL`,
		DefaultSyncTargetKey,
		project,
	).Scan(&pendingCount); err != nil {
		return err
	}

	lifecycle := SyncLifecycleHealthy
	if pendingCount > 0 {
		lifecycle = SyncLifecyclePending
	}
	if isActivelyDegradedState(state, time.Now().UTC()) {
		lifecycle = SyncLifecycleDegraded
	}

	if lifecycle == SyncLifecycleDegraded {
		_, err = s.execHook(tx,
			`UPDATE sync_state
			 SET last_enqueued_seq = ?, last_acked_seq = ?, lifecycle = ?, updated_at = datetime('now')
			 WHERE target_key = ?`,
			maxEnqueuedSeq, maxAckedSeq, lifecycle, projectTargetKey,
		)
	} else {
		_, err = s.execHook(tx,
			`UPDATE sync_state
			 SET last_enqueued_seq = ?, last_acked_seq = ?, lifecycle = ?, consecutive_failures = 0, backoff_until = NULL, reason_code = NULL, reason_message = NULL, last_error = NULL, updated_at = datetime('now')
			 WHERE target_key = ?`,
			maxEnqueuedSeq, maxAckedSeq, lifecycle, projectTargetKey,
		)
	}
	return err
}

func isActivelyDegradedState(state *SyncState, now time.Time) bool {
	if state == nil || state.Lifecycle != SyncLifecycleDegraded {
		return false
	}
	reasonCode := strings.TrimSpace(derefString(state.ReasonCode))
	switch reasonCode {
	case "blocked_unenrolled", "paused", "auth_required", "policy_forbidden", "cloud_config_error":
		return true
	}
	if state.BackoffUntil != nil {
		if backoffUntil, err := time.Parse(time.RFC3339, strings.TrimSpace(*state.BackoffUntil)); err == nil && backoffUntil.After(now) {
			return true
		}
	}
	return false
}

func (s *Store) AcquireSyncLease(targetKey, owner string, ttl time.Duration, now time.Time) (bool, error) {
	targetKey = normalizeSyncTargetKey(targetKey)
	if ttl <= 0 {
		ttl = time.Minute
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	var acquired bool
	err := s.withTx(func(tx *sql.Tx) error {
		state, err := s.getSyncStateTx(tx, targetKey)
		if err != nil {
			return err
		}
		if state.LeaseUntil != nil {
			leaseUntil, err := time.Parse(time.RFC3339, *state.LeaseUntil)
			if err == nil && leaseUntil.After(now) && derefString(state.LeaseOwner) != "" && derefString(state.LeaseOwner) != owner {
				acquired = false
				return nil
			}
		}
		leaseUntil := now.Add(ttl).UTC().Format(time.RFC3339)
		_, err = s.execHook(tx,
			`UPDATE sync_state
			 SET lease_owner = ?, lease_until = ?, updated_at = datetime('now')
			 WHERE target_key = ?`,
			owner, leaseUntil, targetKey,
		)
		if err == nil {
			acquired = true
		}
		return err
	})
	return acquired, err
}

func (s *Store) ReleaseSyncLease(targetKey, owner string) error {
	targetKey = normalizeSyncTargetKey(targetKey)
	_, err := s.execHook(s.db,
		`UPDATE sync_state
		 SET lease_owner = NULL, lease_until = NULL, updated_at = datetime('now')
		 WHERE target_key = ? AND (lease_owner = ? OR lease_owner IS NULL OR lease_owner = '')`,
		targetKey, owner,
	)
	return err
}

func (s *Store) MarkSyncBlocked(targetKey, reasonCode, message string) error {
	targetKey = normalizeSyncTargetKey(targetKey)
	return s.withTx(func(tx *sql.Tx) error {
		if _, err := s.getSyncStateTx(tx, targetKey); err != nil {
			return err
		}
		_, err := s.execHook(tx,
			`UPDATE sync_state
			 SET lifecycle = ?, consecutive_failures = 0, backoff_until = NULL, reason_code = ?, reason_message = ?, last_error = ?, updated_at = datetime('now')
			 WHERE target_key = ?`,
			SyncLifecycleDegraded, reasonCode, message, message, targetKey,
		)
		return err
	})
}

func (s *Store) MarkSyncPaused(targetKey, message string) error {
	return s.MarkSyncBlocked(targetKey, "paused", message)
}

func (s *Store) MarkSyncAuthRequired(targetKey, message string) error {
	return s.MarkSyncBlocked(targetKey, "auth_required", message)
}

func (s *Store) MarkSyncFailure(targetKey, message string, backoffUntil time.Time) error {
	targetKey = normalizeSyncTargetKey(targetKey)
	backoff := backoffUntil.UTC().Format(time.RFC3339)
	return s.withTx(func(tx *sql.Tx) error {
		state, err := s.getSyncStateTx(tx, targetKey)
		if err != nil {
			return err
		}
		_, err = s.execHook(tx,
			`UPDATE sync_state
			 SET lifecycle = ?, consecutive_failures = ?, backoff_until = ?, reason_code = ?, reason_message = ?, last_error = ?, updated_at = datetime('now')
			 WHERE target_key = ?`,
			SyncLifecycleDegraded, state.ConsecutiveFailures+1, backoff, "transport_failed", message, message, targetKey,
		)
		return err
	})
}

func (s *Store) MarkSyncHealthy(targetKey string) error {
	targetKey = normalizeSyncTargetKey(targetKey)
	return s.withTx(func(tx *sql.Tx) error {
		if _, err := s.getSyncStateTx(tx, targetKey); err != nil {
			return err
		}
		_, err := s.execHook(tx,
			`UPDATE sync_state
			 SET lifecycle = ?, consecutive_failures = 0, backoff_until = NULL, reason_code = NULL, reason_message = NULL, last_error = NULL, updated_at = datetime('now')
			 WHERE target_key = ?`,
			SyncLifecycleHealthy, targetKey,
		)
		return err
	})
}

func (s *Store) MarkSyncPending(targetKey string) error {
	targetKey = normalizeSyncTargetKey(targetKey)
	return s.withTx(func(tx *sql.Tx) error {
		if _, err := s.getSyncStateTx(tx, targetKey); err != nil {
			return err
		}
		_, err := s.execHook(tx,
			`UPDATE sync_state
			 SET lifecycle = ?, consecutive_failures = 0, backoff_until = NULL, reason_code = NULL, reason_message = NULL, last_error = NULL, updated_at = datetime('now')
			 WHERE target_key = ?`,
			SyncLifecyclePending, targetKey,
		)
		return err
	})
}

func (s *Store) ApplyPulledMutation(targetKey string, mutation SyncMutation) error {
	targetKey = normalizeSyncTargetKey(targetKey)
	return s.withTx(func(tx *sql.Tx) error {
		state, err := s.getSyncStateTx(tx, targetKey)
		if err != nil {
			return err
		}
		if mutation.Seq <= state.LastPulledSeq {
			return nil
		}

		applyErr := s.applyPulledMutationTx(tx, mutation)
		if applyErr != nil {
			// Phase E: per-entity skip+log policy (design §9).
			// For relation FK misses, write to sync_apply_deferred and ACK the seq
			// so the cursor can advance. All other errors propagate and halt the pull.
			if (mutation.Entity == SyncEntityRelation || mutation.Entity == SyncEntityDependency || mutation.Entity == SyncEntityResourceDependency) && errors.Is(applyErr, ErrRelationFKMissing) {
				log.Printf("[store] ApplyPulledMutation: relation FK miss seq=%d entity_key=%s — deferring",
					mutation.Seq, mutation.EntityKey)
				if _, deferErr := s.execHook(tx, `
					INSERT INTO sync_apply_deferred
						(sync_id, entity, payload, apply_status, retry_count, first_seen_at)
					VALUES (?, ?, ?, 'deferred', 0, datetime('now'))
					ON CONFLICT(sync_id) DO UPDATE SET
						payload            = excluded.payload,
						last_attempted_at  = datetime('now')
				`, mutation.EntityKey, mutation.Entity, mutation.Payload); deferErr != nil {
					return fmt.Errorf("ApplyPulledMutation: write deferred row: %w", deferErr)
				}
				// Fall through to advance the cursor (ACK the seq).
			} else if (mutation.Entity == SyncEntityRelation || mutation.Entity == SyncEntityDependency || mutation.Entity == SyncEntityResourceDependency) && errors.Is(applyErr, ErrApplyDead) {
				// Payload is permanently undecodable — write directly as dead and ACK.
				// There is no point retrying; a malformed payload will never become valid.
				log.Printf("[store] ApplyPulledMutation: relation payload dead seq=%d entity_key=%s err=%v — marking dead",
					mutation.Seq, mutation.EntityKey, applyErr)
				if _, deferErr := s.execHook(tx, `
					INSERT INTO sync_apply_deferred
						(sync_id, entity, payload, apply_status, retry_count, first_seen_at)
					VALUES (?, ?, ?, 'dead', 0, datetime('now'))
					ON CONFLICT(sync_id) DO UPDATE SET
						payload           = excluded.payload,
						apply_status      = 'dead',
						last_attempted_at = datetime('now')
				`, mutation.EntityKey, mutation.Entity, mutation.Payload); deferErr != nil {
					return fmt.Errorf("ApplyPulledMutation: write dead row: %w", deferErr)
				}
				// Fall through to advance the cursor (ACK the seq).
			} else {
				return applyErr
			}
		}

		_, err = s.execHook(tx,
			`UPDATE sync_state
			 SET last_pulled_seq = ?, lifecycle = ?, consecutive_failures = 0, backoff_until = NULL, reason_code = NULL, reason_message = NULL, last_error = NULL, updated_at = datetime('now')
			 WHERE target_key = ?`,
			mutation.Seq, SyncLifecycleHealthy, targetKey,
		)
		return err
	})
}

// ApplyPulledChunk atomically applies all mutations contained in a pulled chunk
// and records the chunk as synced in the same transaction. This guarantees
// retry safety: a failed chunk import leaves no partial semantic mutations.
func (s *Store) ApplyPulledChunk(targetKey, chunkID string, mutations []SyncMutation) error {
	return s.applyPulledChunk(targetKey, chunkID, mutations, "")
}

func (s *Store) ApplyPulledChunkForProject(targetKey, chunkID string, mutations []SyncMutation, project string) error {
	project, err := s.ResolveExistingProject(project)
	if err != nil {
		return err
	}
	return s.applyPulledChunk(targetKey, chunkID, mutations, project)
}

func (s *Store) applyPulledChunk(targetKey, chunkID string, mutations []SyncMutation, project string) error {
	targetKey = normalizeSyncTargetKey(targetKey)
	chunkTargetKey := normalizeChunkTargetKey(targetKey)
	chunkID = strings.TrimSpace(chunkID)
	if chunkID == "" {
		return fmt.Errorf("chunk id is required")
	}

	return s.withTx(func(tx *sql.Tx) error {
		if _, err := s.getSyncStateTx(tx, targetKey); err != nil {
			return err
		}

		var alreadyImported int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM sync_chunks WHERE target_key = ? AND chunk_id = ?`, chunkTargetKey, chunkID).Scan(&alreadyImported); err != nil {
			return err
		}
		if alreadyImported > 0 {
			return nil
		}

		state, err := s.getSyncStateTx(tx, targetKey)
		if err != nil {
			return err
		}

		seq := state.LastPulledSeq
		for i, mutation := range mutations {
			seq++
			mutation.Seq = seq
			mutation.TargetKey = targetKey
			mutation.Source = SyncSourceRemote
			if project != "" {
				if err := s.validatePulledMutationProjectTx(tx, mutation, project); err != nil {
					return fmt.Errorf("apply chunk mutation %d scope: %w", i, err)
				}
			}
			if err := s.applyPulledMutationTx(tx, mutation); err != nil {
				return fmt.Errorf("apply chunk mutation %d: %w", i, err)
			}
		}

		if _, err := s.execHook(tx,
			`INSERT OR IGNORE INTO sync_chunks (target_key, chunk_id) VALUES (?, ?)`,
			chunkTargetKey, chunkID,
		); err != nil {
			return err
		}

		_, err = s.execHook(tx,
			`UPDATE sync_state
			 SET last_pulled_seq = ?, lifecycle = ?, consecutive_failures = 0, backoff_until = NULL, reason_code = NULL, reason_message = NULL, last_error = NULL, updated_at = datetime('now')
			 WHERE target_key = ?`,
			seq, SyncLifecycleHealthy, targetKey,
		)
		return err
	})
}

func (s *Store) GetObservationBySyncID(syncID string) (*Observation, error) {
	row := s.db.QueryRow(
		`SELECT `+observationSelectColumns+`
		 FROM observations WHERE sync_id = ? AND deleted_at IS NULL ORDER BY id DESC LIMIT 1`,
		syncID,
	)
	var o Observation
	if err := scanObservationRow(row, &o); err != nil {
		return nil, err
	}
	return &o, nil
}

// ─── Project Enrollment for Cloud Sync ───────────────────────────────────────

// EnrollProject registers a project for cloud sync. Idempotent — re-enrolling
// an already-enrolled project is a no-op.
func (s *Store) EnrollProject(project string) error {
	project, _ = NormalizeProject(project)
	if project == "" {
		return fmt.Errorf("project name must not be empty")
	}
	return s.withTx(func(tx *sql.Tx) error {
		res, err := s.execHook(tx,
			`INSERT OR IGNORE INTO sync_enrolled_projects (project) VALUES (?)`,
			project,
		)
		if err != nil {
			return err
		}
		rowsAffected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if rowsAffected == 0 {
			return nil
		}
		return s.backfillProjectSyncMutationsTx(tx, project)
	})
}

// UnenrollProject removes a project from cloud sync enrollment. Idempotent —
// unenrolling a non-enrolled project is a no-op.
func (s *Store) UnenrollProject(project string) error {
	project, _ = NormalizeProject(project)
	if project == "" {
		return fmt.Errorf("project name must not be empty")
	}
	_, err := s.execHook(s.db,
		`DELETE FROM sync_enrolled_projects WHERE project = ?`,
		project,
	)
	return err
}

// ListEnrolledProjects returns all projects currently enrolled for cloud sync,
// ordered alphabetically by project name.
func (s *Store) ListEnrolledProjects() ([]EnrolledProject, error) {
	rows, err := s.queryItHook(s.db,
		`SELECT project, enrolled_at FROM sync_enrolled_projects ORDER BY project ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var projects []EnrolledProject
	for rows.Next() {
		var ep EnrolledProject
		if err := rows.Scan(&ep.Project, &ep.EnrolledAt); err != nil {
			return nil, err
		}
		projects = append(projects, ep)
	}
	return projects, rows.Err()
}

// IsProjectEnrolled returns true if the given project is enrolled for cloud sync.
func (s *Store) IsProjectEnrolled(project string) (bool, error) {
	project, _ = NormalizeProject(project)
	if project == "" {
		return false, nil
	}
	var exists int
	err := s.db.QueryRow(
		`SELECT 1 FROM sync_enrolled_projects WHERE project = ? LIMIT 1`,
		project,
	).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ─── Project Migration ───────────────────────────────────────────────────────

type MigrateResult struct {
	Migrated                    bool  `json:"migrated"`
	ObservationsUpdated         int64 `json:"observations_updated"`
	SessionsUpdated             int64 `json:"sessions_updated"`
	PromptsUpdated              int64 `json:"prompts_updated"`
	MemoryDependenciesUpdated   int64 `json:"memory_dependencies_updated"`
	ResourceDependenciesUpdated int64 `json:"resource_dependencies_updated"`
}

// MigrateProject keeps the legacy operator API while sharing the same
// ownership, active-binding and retired-key rules as explicit consolidation.
func (s *Store) MigrateProject(oldName, newName string) (*MigrateResult, error) {
	oldName = strings.TrimSpace(oldName)
	newName, _ = NormalizeProject(newName)
	if oldName == "" || newName == "" || oldName == newName {
		return &MigrateResult{}, nil
	}

	result := &MigrateResult{}
	err := s.withTx(func(tx *sql.Tx) error {
		normalizedOld, _ := NormalizeProject(oldName)
		variants := projectMergeSourceVariants(oldName, normalizedOld, newName)
		if len(variants) == 0 {
			return nil
		}
		args := make([]any, len(variants))
		for i, key := range variants {
			args[i] = key
		}
		// Preserve the two legacy response counters. All ownership updates
		// themselves go through the common merge transaction below.
		for _, entry := range []struct {
			table string
			count *int64
		}{
			{"memory_dependencies", &result.MemoryDependenciesUpdated},
			{"resource_dependencies", &result.ResourceDependenciesUpdated},
		} {
			if err := tx.QueryRow("SELECT count(*) FROM "+entry.table+" WHERE project IN ("+sqlPlaceholders(len(variants))+")", args...).Scan(entry.count); err != nil {
				return err
			}
		}
		merged := &MergeResult{Canonical: newName}
		if err := s.mergeProjectsTx(tx, []string{oldName}, newName, merged); err != nil {
			return err
		}
		result.Migrated = len(merged.SourcesMerged) > 0
		result.ObservationsUpdated = merged.ObservationsUpdated
		result.SessionsUpdated = merged.SessionsUpdated
		result.PromptsUpdated = merged.PromptsUpdated
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ─── Project Queries ──────────────────────────────────────────────────────────

// ProjectNameCount holds a project name and how many observations it has.
type ProjectNameCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// ListProjectNames returns all distinct project names from observations,
// ordered alphabetically. Used for fuzzy matching and consolidation.
func (s *Store) ListProjectNames() ([]string, error) {
	rows, err := s.queryItHook(s.db,
		`SELECT DISTINCT project FROM observations
		 WHERE project IS NOT NULL AND project != '' AND deleted_at IS NULL
		 ORDER BY project`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		results = append(results, name)
	}
	return results, rows.Err()
}

// ProjectStats holds aggregate statistics for a single project.
type ProjectStats struct {
	Name             string   `json:"name"`
	ObservationCount int      `json:"observation_count"`
	SessionCount     int      `json:"session_count"`
	PromptCount      int      `json:"prompt_count"`
	Directories      []string `json:"directories"` // unique directories from sessions
}

// ListProjectsWithStats returns all projects with aggregated counts.
// Ordered by observation count descending.
func (s *Store) ListProjectsWithStats() ([]ProjectStats, error) {
	// Observation counts per project
	obsRows, err := s.queryItHook(s.db,
		`SELECT project, COUNT(*) as cnt
		 FROM observations
		 WHERE project IS NOT NULL AND project != '' AND deleted_at IS NULL
		 GROUP BY project`,
	)
	if err != nil {
		return nil, fmt.Errorf("list projects obs: %w", err)
	}
	defer obsRows.Close()

	statsMap := make(map[string]*ProjectStats)
	for obsRows.Next() {
		var name string
		var cnt int
		if err := obsRows.Scan(&name, &cnt); err != nil {
			return nil, err
		}
		statsMap[name] = &ProjectStats{Name: name, ObservationCount: cnt}
	}
	if err := obsRows.Err(); err != nil {
		return nil, err
	}

	// Session counts + directories per project
	sessRows, err := s.queryItHook(s.db,
		`SELECT project, COUNT(*) as cnt, directory
		 FROM sessions
		 WHERE project IS NOT NULL AND project != ''
		 GROUP BY project, directory`,
	)
	if err != nil {
		return nil, fmt.Errorf("list projects sessions: %w", err)
	}
	defer sessRows.Close()

	type projDir struct {
		count int
		dirs  map[string]bool
	}
	sessData := make(map[string]*projDir)
	for sessRows.Next() {
		var name, dir string
		var cnt int
		if err := sessRows.Scan(&name, &cnt, &dir); err != nil {
			return nil, err
		}
		if sessData[name] == nil {
			sessData[name] = &projDir{dirs: make(map[string]bool)}
		}
		sessData[name].count += cnt
		if dir != "" {
			sessData[name].dirs[dir] = true
		}
	}
	if err := sessRows.Err(); err != nil {
		return nil, err
	}

	for name, sd := range sessData {
		if statsMap[name] == nil {
			statsMap[name] = &ProjectStats{Name: name}
		}
		statsMap[name].SessionCount = sd.count
		for d := range sd.dirs {
			statsMap[name].Directories = append(statsMap[name].Directories, d)
		}
	}

	// Prompt counts per project
	promptRows, err := s.queryItHook(s.db,
		`SELECT project, COUNT(*) as cnt
		 FROM user_prompts
		 WHERE project IS NOT NULL AND project != ''
		 GROUP BY project`,
	)
	if err != nil {
		return nil, fmt.Errorf("list projects prompts: %w", err)
	}
	defer promptRows.Close()

	for promptRows.Next() {
		var name string
		var cnt int
		if err := promptRows.Scan(&name, &cnt); err != nil {
			return nil, err
		}
		if statsMap[name] == nil {
			statsMap[name] = &ProjectStats{Name: name}
		}
		statsMap[name].PromptCount = cnt
	}
	if err := promptRows.Err(); err != nil {
		return nil, err
	}

	// Convert to slice, sorted by observation count descending
	results := make([]ProjectStats, 0, len(statsMap))
	for _, ps := range statsMap {
		results = append(results, *ps)
	}
	// Simple insertion sort — project lists are small
	for i := 1; i < len(results); i++ {
		for j := i; j > 0 && results[j].ObservationCount > results[j-1].ObservationCount; j-- {
			results[j], results[j-1] = results[j-1], results[j]
		}
	}

	return results, nil
}

// CountObservationsForProject returns the number of non-deleted observations
// for the given project name. Used by handleSave for the similar-project
// warning instead of the heavier ListProjectsWithStats.
func (s *Store) CountObservationsForProject(name string) (int, error) {
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM observations WHERE project = ? AND deleted_at IS NULL`,
		name,
	).Scan(&count)
	return count, err
}

// MergeResult summarizes the result of merging multiple project name variants
// into a single canonical project name.
type MergeResult struct {
	Canonical                string   `json:"canonical"`
	SourcesMerged            []string `json:"sources_merged"`
	ObservationsUpdated      int64    `json:"observations_updated"`
	SessionsUpdated          int64    `json:"sessions_updated"`
	PromptsUpdated           int64    `json:"prompts_updated"`
	AssociatedRecordsUpdated int64    `json:"associated_records_updated"`
	ActivationsExpired       int64    `json:"activations_expired"`
	SyncRecordsUpdated       int64    `json:"sync_records_updated"`
	KeysRetired              int64    `json:"keys_retired"`
}

// ProjectMapping is an explicit, operator-reviewed project rename. It is not
// inferred from fuzzy names, directory basenames, or transitive groups.
type ProjectMapping struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

// ProjectMappingResult summarizes one atomic mapping manifest application.
type ProjectMappingResult struct {
	MappingsApplied          int      `json:"mappings_applied"`
	SourcesMerged            []string `json:"sources_merged"`
	Targets                  []string `json:"targets"`
	ObservationsUpdated      int64    `json:"observations_updated"`
	SessionsUpdated          int64    `json:"sessions_updated"`
	PromptsUpdated           int64    `json:"prompts_updated"`
	AssociatedRecordsUpdated int64    `json:"associated_records_updated"`
	ActivationsExpired       int64    `json:"activations_expired"`
	SyncRecordsUpdated       int64    `json:"sync_records_updated"`
	KeysRetired              int64    `json:"keys_retired"`
}

// ValidateProjectMappings validates and normalizes an explicit store-bound
// mapping set without changing the store. Preview and apply paths share this
// contract so an invalid manifest is never presented as ready to apply.
func (s *Store) ValidateProjectMappings(expectedStoreID string, mappings []ProjectMapping) ([]ProjectMapping, error) {
	identity, err := s.Identity()
	if err != nil {
		return nil, err
	}
	expectedStoreID = strings.TrimSpace(expectedStoreID)
	if expectedStoreID == "" {
		return nil, fmt.Errorf("%w: expected store_id is required", ErrInvalidProjectMapping)
	}
	if identity.StoreID != expectedStoreID {
		return nil, &StoreIdentityMismatchError{
			Expected: expectedStoreID,
			Actual:   identity.StoreID,
			Path:     identity.DatabasePath,
		}
	}
	if len(mappings) == 0 {
		return nil, fmt.Errorf("%w: at least one mapping is required", ErrInvalidProjectMapping)
	}
	if len(mappings) > 100 {
		return nil, fmt.Errorf("%w: at most 100 mappings may be applied at once", ErrInvalidProjectMapping)
	}

	sourceTargets := make(map[string]string)
	normalized := make([]ProjectMapping, 0, len(mappings))
	for _, mapping := range mappings {
		source := strings.TrimSpace(mapping.Source)
		normalizedSource, _ := NormalizeProject(source)
		target, _ := NormalizeProject(mapping.Target)
		source = strings.TrimSpace(source)
		target = strings.TrimSpace(target)
		if source == "" || target == "" {
			return nil, fmt.Errorf("%w: source and target are required", ErrInvalidProjectMapping)
		}
		if source == target {
			return nil, fmt.Errorf("%w: source %q equals target", ErrInvalidProjectMapping, source)
		}
		if previous, exists := sourceTargets[normalizedSource]; exists {
			return nil, fmt.Errorf("%w: source %q is mapped more than once (to %q and %q)", ErrInvalidProjectMapping, source, previous, target)
		}
		exists, err := s.ProjectExists(source)
		if err != nil {
			return nil, fmt.Errorf("validate mapping source %q: %w", source, err)
		}
		if !exists {
			var retiredTarget string
			err := s.db.QueryRow(`SELECT target FROM project_key_retirements WHERE source=? OR source=? LIMIT 1`, source, normalizedSource).Scan(&retiredTarget)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			if retiredTarget != target {
				return nil, fmt.Errorf("%w: source project %q does not exist or has a different retired mapping", ErrInvalidProjectMapping, source)
			}
		}
		sourceTargets[normalizedSource] = target
		normalized = append(normalized, ProjectMapping{Source: source, Target: target})
	}
	for source, target := range sourceTargets {
		if _, targetIsSource := sourceTargets[target]; targetIsSource && source != target {
			return nil, fmt.Errorf("%w: transitive mapping %q -> %q is not allowed", ErrInvalidProjectMapping, source, target)
		}
	}
	if err := s.withTx(func(tx *sql.Tx) error {
		for _, mapping := range normalized {
			key, _ := NormalizeProject(mapping.Source)
			if err := s.validateProjectMergeTx(tx, projectMergeSourceVariants(mapping.Source, key, mapping.Target), mapping.Target); err != nil {
				return fmt.Errorf("%w: %v", ErrInvalidProjectMapping, err)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return normalized, nil
}

// ApplyProjectMappings validates an explicit store-bound mapping set and
// applies all entries in one transaction. Transitive mappings are rejected so
// a source can never move farther than the operator-approved edge.
func (s *Store) ApplyProjectMappings(expectedStoreID string, mappings []ProjectMapping) (*ProjectMappingResult, error) {
	normalized, err := s.ValidateProjectMappings(expectedStoreID, mappings)
	if err != nil {
		return nil, err
	}

	groups := make(map[string][]string)
	for _, mapping := range normalized {
		groups[mapping.Target] = append(groups[mapping.Target], mapping.Source)
	}

	targets := make([]string, 0, len(groups))
	for target := range groups {
		targets = append(targets, target)
	}
	sort.Strings(targets)
	result := &ProjectMappingResult{MappingsApplied: len(normalized), Targets: targets}
	err = s.withTx(func(tx *sql.Tx) error {
		for _, target := range targets {
			sources := groups[target]
			sort.Strings(sources)
			mergeResult := &MergeResult{Canonical: target}
			if err := s.mergeProjectsTx(tx, sources, target, mergeResult); err != nil {
				return err
			}
			result.SourcesMerged = append(result.SourcesMerged, mergeResult.SourcesMerged...)
			result.ObservationsUpdated += mergeResult.ObservationsUpdated
			result.SessionsUpdated += mergeResult.SessionsUpdated
			result.PromptsUpdated += mergeResult.PromptsUpdated
			result.AssociatedRecordsUpdated += mergeResult.AssociatedRecordsUpdated
			result.ActivationsExpired += mergeResult.ActivationsExpired
			result.SyncRecordsUpdated += mergeResult.SyncRecordsUpdated
			result.KeysRetired += mergeResult.KeysRetired
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// MergeProjects migrates all records from each source project name into the
// canonical name. Sources that equal the canonical (after normalization) or
// have no records are silently skipped — the operation is idempotent.
// All updates are performed inside a single transaction for atomicity.
func (s *Store) MergeProjects(sources []string, canonical string) (*MergeResult, error) {
	canonical, _ = NormalizeProject(canonical)
	if canonical == "" {
		return nil, fmt.Errorf("canonical project name must not be empty")
	}

	result := &MergeResult{Canonical: canonical}
	err := s.withTx(func(tx *sql.Tx) error { return s.mergeProjectsTx(tx, sources, canonical, result) })
	if err != nil {
		return nil, err
	}

	return result, nil
}

func (s *Store) mergeProjectsTx(tx *sql.Tx, sources []string, canonical string, result *MergeResult) error {
	if err := s.validateProjectMergeTx(tx, nil, canonical); err != nil {
		return err
	}
	ownershipChanged := false
	seenSources := make(map[string]struct{})
	for _, srcInput := range sources {
		srcInput = strings.TrimSpace(srcInput)
		srcNormalized, _ := NormalizeProject(srcInput)
		if srcNormalized == "" || srcInput == canonical {
			continue
		}
		if _, seen := seenSources[srcInput]; seen {
			continue
		}
		seenSources[srcInput] = struct{}{}

		sourceVariants := projectMergeSourceVariants(srcInput, srcNormalized, canonical)
		if len(sourceVariants) == 0 {
			continue
		}
		if err := s.validateProjectMergeTx(tx, sourceVariants, canonical); err != nil {
			return err
		}
		before := result.ObservationsUpdated + result.SessionsUpdated + result.PromptsUpdated + result.AssociatedRecordsUpdated + result.ActivationsExpired + result.SyncRecordsUpdated

		placeholders := sqlPlaceholders(len(sourceVariants))
		args := make([]any, 0, len(sourceVariants)+1)
		args = append(args, canonical)
		for _, variant := range sourceVariants {
			args = append(args, variant)
		}

		res, err := s.execHook(tx, `UPDATE observations SET project = ? WHERE project IN (`+placeholders+`)`, args...)
		if err != nil {
			return fmt.Errorf("merge observations %q -> %q: %w", srcNormalized, canonical, err)
		}
		n, _ := res.RowsAffected()
		result.ObservationsUpdated += n

		res, err = s.execHook(tx, `UPDATE sessions SET project = ? WHERE project IN (`+placeholders+`)`, args...)
		if err != nil {
			return fmt.Errorf("merge sessions %q -> %q: %w", srcNormalized, canonical, err)
		}
		n, _ = res.RowsAffected()
		result.SessionsUpdated += n

		res, err = s.execHook(tx, `UPDATE user_prompts SET project = ? WHERE project IN (`+placeholders+`)`, args...)
		if err != nil {
			return fmt.Errorf("merge prompts %q -> %q: %w", srcNormalized, canonical, err)
		}
		n, _ = res.RowsAffected()
		result.PromptsUpdated += n
		if err := s.mergeProjectMetadataTx(tx, sourceVariants, canonical, result); err != nil {
			return err
		}
		changed := result.ObservationsUpdated+result.SessionsUpdated+result.PromptsUpdated+result.AssociatedRecordsUpdated+result.ActivationsExpired+result.SyncRecordsUpdated > before
		retired, err := s.retireMergedKeysTx(tx, sourceVariants, canonical, changed)
		if err != nil {
			return err
		}
		result.KeysRetired += retired
		if changed || retired > 0 {
			ownershipChanged = true
			label := srcNormalized
			if label == canonical {
				label = srcInput
			}
			result.SourcesMerged = append(result.SourcesMerged, label)
		}
	}
	return s.enqueueProjectSyncMutationsTx(tx, canonical, ownershipChanged)
}

// sqlPlaceholders returns a comma-separated list of parameter markers only.
// Values are still passed separately through query arguments; no user data is
// interpolated into SQL here.
func sqlPlaceholders(count int) string {
	return strings.TrimRight(strings.Repeat("?,", count), ",")
}

func projectMergeSourceVariants(rawSource, normalizedSource, canonical string) []string {
	seen := make(map[string]struct{})
	variants := make([]string, 0, 5)
	// Match both the historical raw project name and its normalized form so
	// legacy rows are migrated without reintroducing canonical-source churn.
	candidates := []string{strings.TrimSpace(rawSource), normalizedSource}
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || candidate == canonical {
			continue
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		variants = append(variants, candidate)
	}
	return variants
}

// ─── Project Pruning ─────────────────────────────────────────────────────────

// PruneResult holds the outcome of pruning a single project.
type PruneResult struct {
	Project         string `json:"project"`
	SessionsDeleted int64  `json:"sessions_deleted"`
	PromptsDeleted  int64  `json:"prompts_deleted"`
}

// PruneProject removes all sessions and prompts for a project that has zero
// (non-deleted) observations. Returns an error if the project still has
// observations — the caller must verify first.
func (s *Store) PruneProject(project string) (*PruneResult, error) {
	if project == "" {
		return nil, fmt.Errorf("project name must not be empty")
	}

	// Safety check: refuse to prune if observations exist.
	count, err := s.CountObservationsForProject(project)
	if err != nil {
		return nil, fmt.Errorf("count observations: %w", err)
	}
	if count > 0 {
		return nil, fmt.Errorf("project %q still has %d observations — cannot prune", project, count)
	}
	// Name-keyed prune must not touch registered identities: their data may
	// live under several keys (legacy import names, registered IDs) and the
	// missing-root verification of `projects prune --project <ID>` is the only
	// safe removal path for those.
	var registered int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM project_identities WHERE id=? OR name=?`, project, project).Scan(&registered); err != nil {
		return nil, fmt.Errorf("check registered identity: %w", err)
	}
	if registered != 0 {
		return nil, fmt.Errorf("project %q matches a registered identity (id or name); use `projects prune --project <ID> --apply` after its bound roots are gone, or `projects consolidate`", project)
	}

	result := &PruneResult{Project: project}

	err = s.withTx(func(tx *sql.Tx) error {
		res, err := s.execHook(tx, `DELETE FROM user_prompts WHERE project = ?`, project)
		if err != nil {
			return fmt.Errorf("prune prompts: %w", err)
		}
		result.PromptsDeleted, _ = res.RowsAffected()

		res, err = s.execHook(tx, `DELETE FROM sessions WHERE project = ?`, project)
		if err != nil {
			return fmt.Errorf("prune sessions: %w", err)
		}
		result.SessionsDeleted, _ = res.RowsAffected()

		return nil
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// ─── Delete Project ───────────────────────────────────────────────────────────

// DeleteProjectResult summarises a cascade project deletion.
type DeleteProjectResult struct {
	Project             string `json:"project"`
	ObservationsDeleted int64  `json:"observations_deleted"`
	PromptsDeleted      int64  `json:"prompts_deleted"`
	SessionsDeleted     int64  `json:"sessions_deleted"`
	HardDelete          bool   `json:"hard_delete"`
}

// DeleteProject removes all data associated with a project in a single
// transaction.
//
// When hardDelete is true: observation rows are permanently removed, prompts
// are hard-deleted, and sessions are hard-deleted. memory_relations that
// reference any removed observation are marked orphaned (audit history).
//
// When hardDelete is false: observations are soft-deleted (deleted_at set),
// and prompts are hard-deleted. Sessions are NOT removed in this path because
// observations.session_id is a NOT NULL FK to sessions — removing sessions
// while soft-deleted observation rows still reference them would violate the FK
// constraint. The session rows remain and can be cleaned up with
// recalldock delete session <id> once the observations are purged.
//
// Returns ErrProjectNotFound when no sessions or observations exist for the
// given project name.
func (s *Store) DeleteProject(project string, hardDelete bool) (*DeleteProjectResult, error) {
	project = strings.TrimSpace(project)
	if project == "" {
		return nil, fmt.Errorf("project name must not be empty")
	}

	result := &DeleteProjectResult{Project: project, HardDelete: hardDelete}

	err := s.withTx(func(tx *sql.Tx) error {
		// Existence check: at least one session or observation must exist.
		var sessionCount int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM sessions WHERE project = ?`, project).Scan(&sessionCount); err != nil {
			return fmt.Errorf("delete project: count sessions: %w", err)
		}
		var obsCount int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM observations WHERE project = ?`, project).Scan(&obsCount); err != nil {
			return fmt.Errorf("delete project: count observations: %w", err)
		}
		if sessionCount == 0 && obsCount == 0 {
			return fmt.Errorf("%w: %q", ErrProjectNotFound, project)
		}

		if hardDelete {
			if err := validateSessionPromptOwnersTx(tx, project, ""); err != nil {
				return err
			}
		}
		if err := s.recordProjectDeletesTx(tx, project, hardDelete); err != nil {
			return err
		}
		// 1. Delete/soft-delete observations.
		if hardDelete {
			// Orphan memory_relations rows that reference any observation in this project.
			if _, err := s.execHook(tx, `
				UPDATE memory_relations
				SET judgment_status = 'orphaned',
				    updated_at      = datetime('now')
				WHERE source_id IN (SELECT sync_id FROM observations WHERE project = ?)
				   OR target_id IN (SELECT sync_id FROM observations WHERE project = ?)
			`, project, project); err != nil {
				return fmt.Errorf("delete project: orphan relations: %w", err)
			}
			res, err := s.execHook(tx, `DELETE FROM observations WHERE project = ?`, project)
			if err != nil {
				return fmt.Errorf("delete project: hard-delete observations: %w", err)
			}
			result.ObservationsDeleted, _ = res.RowsAffected()
		} else {
			res, err := s.execHook(tx, `
				UPDATE observations
				SET deleted_at = datetime('now'),
				    updated_at = datetime('now')
				WHERE project = ? AND deleted_at IS NULL
			`, project)
			if err != nil {
				return fmt.Errorf("delete project: soft-delete observations: %w", err)
			}
			result.ObservationsDeleted, _ = res.RowsAffected()
		}

		// 2. Delete prompts for the project (no soft-delete mechanism exists).
		res, err := s.execHook(tx, `DELETE FROM user_prompts WHERE project = ?`, project)
		if err != nil {
			return fmt.Errorf("delete project: delete prompts: %w", err)
		}
		result.PromptsDeleted, _ = res.RowsAffected()

		// 3. Delete sessions — only when hard-deleting, because observation rows
		//    reference sessions via a NOT NULL FK and soft-deleted rows are still
		//    present in the table.
		if hardDelete {
			res, err = s.execHook(tx, `DELETE FROM sessions WHERE project = ?`, project)
			if err != nil {
				return fmt.Errorf("delete project: delete sessions: %w", err)
			}
			result.SessionsDeleted, _ = res.RowsAffected()
		}

		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func (s *Store) withTx(fn func(tx *sql.Tx) error) error {
	return withSQLiteWriteRetry(func() error {
		tx, err := s.beginTxHook()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := fn(tx); err != nil {
			return err
		}
		return s.commitHook(tx)
	})
}

func withSQLiteWriteRetry(fn func() error) error {
	var lastErr error
	for attempt := 0; attempt <= len(sqliteWriteRetryBackoffs); attempt++ {
		if err := fn(); err != nil {
			lastErr = err
			if !isRetryableSQLiteLockError(err) || attempt == len(sqliteWriteRetryBackoffs) {
				return err
			}
			time.Sleep(sqliteWriteRetryBackoffs[attempt])
			continue
		}
		return nil
	}
	return lastErr
}

func isRetryableSQLiteLockError(err error) bool {
	if err == nil {
		return false
	}
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		primaryCode := sqliteErr.Code() & 0xff
		return primaryCode == sqlitePrimaryBusy || primaryCode == sqlitePrimaryLocked
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") || strings.Contains(msg, "database is busy") || strings.Contains(msg, "sqlite_busy") || strings.Contains(msg, "sqlite_locked")
}

func (s *Store) createSessionTx(tx *sql.Tx, id, project, directory, origin string) error {
	origin = NormalizeSessionOrigin(origin)
	_, err := s.execHook(tx,
		`INSERT INTO sessions (id, project, directory, origin) VALUES (?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   project   = CASE WHEN sessions.project = '' THEN excluded.project ELSE sessions.project END,
		   directory = CASE WHEN sessions.directory = '' THEN excluded.directory ELSE sessions.directory END,
		   origin    = CASE
		                 WHEN ifnull(sessions.origin, '') IN ('', 'unknown') THEN excluded.origin
		                 WHEN ifnull(excluded.origin, '') = 'host' THEN 'host'
		                 WHEN ifnull(excluded.origin, '') = 'operator' AND ifnull(sessions.origin, '') = 'model' THEN 'operator'
		                 ELSE ifnull(sessions.origin, 'unknown')
		               END`,
		id, project, directory, origin,
	)
	return err
}

func (s *Store) ensureSyncState(targetKey string) error {
	_, err := s.execHook(s.db,
		`INSERT OR IGNORE INTO sync_state (target_key, lifecycle, updated_at) VALUES (?, ?, datetime('now'))`,
		targetKey, SyncLifecycleIdle,
	)
	return err
}

func (s *Store) getSyncState(targetKey string) (*SyncState, error) {
	row := s.db.QueryRow(`
		SELECT target_key, lifecycle, last_enqueued_seq, last_acked_seq, last_pulled_seq,
		       consecutive_failures, backoff_until, lease_owner, lease_until, reason_code, reason_message, last_error, updated_at
		FROM sync_state WHERE target_key = ?`, targetKey)
	var state SyncState
	if err := row.Scan(&state.TargetKey, &state.Lifecycle, &state.LastEnqueuedSeq, &state.LastAckedSeq, &state.LastPulledSeq, &state.ConsecutiveFailures, &state.BackoffUntil, &state.LeaseOwner, &state.LeaseUntil, &state.ReasonCode, &state.ReasonMessage, &state.LastError, &state.UpdatedAt); err != nil {
		return nil, err
	}
	return &state, nil
}

func (s *Store) getSyncStateTx(tx *sql.Tx, targetKey string) (*SyncState, error) {
	if _, err := s.execHook(tx,
		`INSERT OR IGNORE INTO sync_state (target_key, lifecycle, updated_at) VALUES (?, ?, datetime('now'))`,
		targetKey, SyncLifecycleIdle,
	); err != nil {
		return nil, err
	}
	row := tx.QueryRow(`
		SELECT target_key, lifecycle, last_enqueued_seq, last_acked_seq, last_pulled_seq,
		       consecutive_failures, backoff_until, lease_owner, lease_until, reason_code, reason_message, last_error, updated_at
		FROM sync_state WHERE target_key = ?`, targetKey)
	var state SyncState
	if err := row.Scan(&state.TargetKey, &state.Lifecycle, &state.LastEnqueuedSeq, &state.LastAckedSeq, &state.LastPulledSeq, &state.ConsecutiveFailures, &state.BackoffUntil, &state.LeaseOwner, &state.LeaseUntil, &state.ReasonCode, &state.ReasonMessage, &state.LastError, &state.UpdatedAt); err != nil {
		return nil, err
	}
	return &state, nil
}

func (s *Store) backfillProjectSyncMutationsTx(tx *sql.Tx, project string) error {
	return s.enqueueProjectSyncMutationsTx(tx, project, false)
}

// A completed ownership transfer needs a fresh snapshot even when every old
// mutation was acknowledged under the source project. Read current rows rather
// than replaying historical payloads; keep the old envelopes and acknowledgements.
func (s *Store) enqueueProjectSyncMutationsTx(tx *sql.Tx, project string, fullSnapshot bool) error {
	if err := s.backfillSessionSyncMutationsTx(tx, project, fullSnapshot); err != nil {
		return err
	}
	if err := s.backfillObservationSyncMutationsTx(tx, project, fullSnapshot); err != nil {
		return err
	}
	if err := s.backfillPromptSyncMutationsTx(tx, project, fullSnapshot); err != nil {
		return err
	}
	if err := s.backfillRelationSyncMutationsTx(tx, project, fullSnapshot); err != nil {
		return err
	}
	if err := s.backfillDependencySyncMutationsTx(tx, project, fullSnapshot); err != nil {
		return err
	}
	return s.backfillResourceDependencySyncMutationsTx(tx, project, fullSnapshot)
}

// projectNeedsBackfill returns true when a project has any sessions, live observations,
// or prompts that are missing a corresponding sync_mutation row.
// It runs three lightweight COUNT queries — no cursor is held open.
func (s *Store) projectNeedsBackfill(project string) (bool, error) {
	type countQuery struct {
		q    string
		args []any
	}
	queries := []countQuery{
		{
			q: `SELECT COUNT(*) FROM sessions
			    WHERE project = ?
			      AND NOT EXISTS (
			        SELECT 1 FROM sync_mutations sm
			        WHERE sm.target_key = ? AND sm.entity = ? AND sm.entity_key = sessions.id AND sm.source = ?
			      )`,
			args: []any{project, DefaultSyncTargetKey, SyncEntitySession, SyncSourceLocal},
		},
		{
			q: `SELECT COUNT(*) FROM observations o
			    LEFT JOIN sessions s ON s.id = o.session_id
			    WHERE (ifnull(o.project,'') = ? OR (ifnull(o.project,'') = '' AND ifnull(s.project,'') = ?))
			      AND o.deleted_at IS NULL
			      AND NOT EXISTS (
			        SELECT 1 FROM sync_mutations sm
			        WHERE sm.target_key = ? AND sm.entity = ? AND sm.entity_key = o.sync_id AND sm.source = ?
			      )`,
			args: []any{project, project, DefaultSyncTargetKey, SyncEntityObservation, SyncSourceLocal},
		},
		{
			q: `SELECT COUNT(*) FROM user_prompts p
			    LEFT JOIN sessions s ON s.id = p.session_id
			    WHERE (ifnull(p.project,'') = ? OR (ifnull(p.project,'') = '' AND ifnull(s.project,'') = ?))
			      AND NOT EXISTS (
			        SELECT 1 FROM sync_mutations sm
			        WHERE sm.target_key = ? AND sm.entity = ? AND sm.entity_key = p.sync_id AND sm.source = ?
			      )`,
			args: []any{project, project, DefaultSyncTargetKey, SyncEntityPrompt, SyncSourceLocal},
		},
		{
			// Count only fully-judged relations (not orphaned, not pending, with
			// marked_by_actor/kind populated) whose source and target observations
			// are locally available and that have no local upsert sync_mutations row.
			// Mirrors the SELECT in backfillRelationSyncMutationsTx exactly — any
			// divergence causes the fast-path skip to desync from the write path.
			// Pending/unmarked rows lack marked_by_* and would be rejected by cloud
			// validation (HTTP 400), so we exclude them from both the count and the
			// backfill to avoid polluting the sync journal with undeliverable mutations.
			q: `SELECT COUNT(*)
			    FROM memory_relations r
			    JOIN observations src ON src.sync_id = r.source_id AND src.deleted_at IS NULL
			    JOIN observations tgt ON tgt.sync_id = r.target_id AND tgt.deleted_at IS NULL
			    LEFT JOIN sessions src_s ON src_s.id = src.session_id
			    WHERE r.judgment_status NOT IN (?, ?)
			      AND ifnull(r.marked_by_actor, '') != ''
			      AND ifnull(r.marked_by_kind, '') != ''
			      AND coalesce(nullif(src.project, ''), src_s.project, '') = ?
			      AND NOT EXISTS (
			        SELECT 1 FROM sync_mutations sm
			        WHERE sm.target_key = ? AND sm.entity = ? AND sm.entity_key = r.sync_id AND sm.source = ?
			      )`,
			args: []any{JudgmentStatusOrphaned, JudgmentStatusPending, project, DefaultSyncTargetKey, SyncEntityRelation, SyncSourceLocal},
		},
		{
			q: `SELECT COUNT(*) FROM memory_dependencies d
			    WHERE d.project = ?
			      AND NOT EXISTS (
			        SELECT 1 FROM sync_mutations sm
			        WHERE sm.target_key = ? AND sm.entity = ? AND sm.entity_key = d.sync_id AND sm.source = ?
			      )`,
			args: []any{project, DefaultSyncTargetKey, SyncEntityDependency, SyncSourceLocal},
		},
		{
			q: `SELECT COUNT(*) FROM resource_dependencies d
			    WHERE d.project = ?
			      AND NOT EXISTS (
			        SELECT 1 FROM sync_mutations sm
			        WHERE sm.target_key = ? AND sm.entity = ? AND sm.entity_key = d.sync_id AND sm.source = ?
			      )`,
			args: []any{project, DefaultSyncTargetKey, SyncEntityResourceDependency, SyncSourceLocal},
		},
	}
	for _, cq := range queries {
		var n int
		if err := s.db.QueryRow(cq.q, cq.args...).Scan(&n); err != nil {
			return false, err
		}
		if n > 0 {
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) repairEnrolledProjectSyncMutations() error {
	// Collect enrolled projects outside a transaction so we avoid holding a read
	// cursor open while we later write inside backfillProjectSyncMutationsTx.
	rows, err := s.db.Query(`SELECT project FROM sync_enrolled_projects ORDER BY project ASC`)
	if err != nil {
		return err
	}
	var projects []string
	for rows.Next() {
		var project string
		if err := rows.Scan(&project); err != nil {
			return closeRowsWithError(rows, err)
		}
		projects = append(projects, project)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, project := range projects {
		// Fast path: if the project is already fully backfilled, skip the write tx entirely.
		needs, err := s.projectNeedsBackfill(project)
		if err != nil {
			return err
		}
		if !needs {
			continue
		}
		if err := s.withTx(func(tx *sql.Tx) error {
			return s.backfillProjectSyncMutationsTx(tx, project)
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) backfillSessionSyncMutationsTx(tx *sql.Tx, project string, fullSnapshot bool) error {
	rows, err := s.queryItHook(tx, `
		SELECT id, project, directory, started_at, ended_at, summary, ifnull(origin, 'unknown')
		FROM sessions
		WHERE project = ?
		  AND NOT EXISTS (
			SELECT 1
			FROM sync_mutations sm
			WHERE sm.target_key = ?
			  AND sm.entity = ?
			  AND sm.entity_key = sessions.id
			  AND sm.source = ?
			  AND ? = 0
		  )
		ORDER BY started_at ASC, id ASC`,
		project, DefaultSyncTargetKey, SyncEntitySession, SyncSourceLocal, fullSnapshot,
	)
	if err != nil {
		return err
	}

	// Phase 1: collect all missing sessions into memory before any INSERT.
	// Keeping the cursor open while inserting into sync_mutations causes SQLite
	// to re-evaluate the NOT EXISTS subquery against the in-progress write set,
	// which can produce an O(N*M) busy loop on large stores.
	var pending []syncSessionPayload
	for rows.Next() {
		var payload syncSessionPayload
		if err := rows.Scan(&payload.ID, &payload.Project, &payload.Directory, &payload.StartedAt, &payload.EndedAt, &payload.Summary, &payload.Origin); err != nil {
			return closeRowsWithError(rows, err)
		}
		pending = append(pending, payload)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// Phase 2: insert now that the read cursor is closed.
	for _, payload := range pending {
		if err := s.enqueueSyncMutationTx(tx, SyncEntitySession, payload.ID, SyncOpUpsert, payload); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) backfillObservationSyncMutationsTx(tx *sql.Tx, project string, fullSnapshot bool) error {
	// ── Live observations ─────────────────────────────────────────────────────
	rows, err := s.queryItHook(tx, `
		SELECT o.sync_id, o.session_id, o.type, o.memory_class, ifnull(o.memory_path, ''),
		       ifnull(o.abstract, ''), ifnull(o.overview, ''), o.summary_state, o.summary_revision,
		       o.stale_at, o.stale_reason, o.title, o.content, o.tool_name, o.project, o.scope, o.topic_key,
		       o.revision_count, o.duplicate_count, o.last_seen_at, o.created_at, o.updated_at
		FROM observations o
		LEFT JOIN sessions s ON s.id = o.session_id
		WHERE (
			ifnull(o.project, '') = ?
			OR (ifnull(o.project, '') = '' AND ifnull(s.project, '') = ?)
		)
		  AND deleted_at IS NULL
		  AND NOT EXISTS (
			SELECT 1
			FROM sync_mutations sm
			WHERE sm.target_key = ?
			  AND sm.entity = ?
			  AND sm.entity_key = o.sync_id
			  AND sm.source = ?
			  AND ? = 0
		  )
		ORDER BY o.id ASC`,
		project, project, DefaultSyncTargetKey, SyncEntityObservation, SyncSourceLocal, fullSnapshot,
	)
	if err != nil {
		return err
	}

	// Phase 1: collect live observations before any INSERT.
	var pending []syncObservationPayload
	for rows.Next() {
		var payload syncObservationPayload
		if err := rows.Scan(
			&payload.SyncID,
			&payload.SessionID,
			&payload.Type,
			&payload.MemoryClass,
			&payload.MemoryPath,
			&payload.Abstract,
			&payload.Overview,
			&payload.SummaryState,
			&payload.SummaryRevision,
			&payload.StaleAt,
			&payload.StaleReason,
			&payload.Title,
			&payload.Content,
			&payload.ToolName,
			&payload.Project,
			&payload.Scope,
			&payload.TopicKey,
			&payload.RevisionCount,
			&payload.DuplicateCount,
			&payload.LastSeenAt,
			&payload.CreatedAt,
			&payload.UpdatedAt,
		); err != nil {
			return closeRowsWithError(rows, err)
		}
		pending = append(pending, payload)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// Phase 2: insert live observation mutations.
	for _, payload := range pending {
		if err := s.enqueueSyncMutationTx(tx, SyncEntityObservation, payload.SyncID, SyncOpUpsert, payload); err != nil {
			return err
		}
	}

	// ── Deleted observations ──────────────────────────────────────────────────
	deletedRows, err := s.queryItHook(tx, `
		SELECT o.sync_id, o.session_id, o.project, o.deleted_at
		FROM observations o
		LEFT JOIN sessions s ON s.id = o.session_id
		WHERE (
			ifnull(o.project, '') = ?
			OR (ifnull(o.project, '') = '' AND ifnull(s.project, '') = ?)
		)
		  AND o.deleted_at IS NOT NULL
		  AND NOT EXISTS (
			SELECT 1
			FROM sync_mutations sm
			WHERE sm.target_key = ?
			  AND sm.entity = ?
			  AND sm.entity_key = o.sync_id
			  AND sm.op = ?
			  AND sm.source = ?
			  AND ? = 0
		  )
		ORDER BY o.id ASC`,
		project, project, DefaultSyncTargetKey, SyncEntityObservation, SyncOpDelete, SyncSourceLocal, fullSnapshot,
	)
	if err != nil {
		return err
	}

	// Phase 1: collect deleted observations before any INSERT.
	var deletedPending []syncObservationPayload
	for deletedRows.Next() {
		var payload syncObservationPayload
		if err := deletedRows.Scan(&payload.SyncID, &payload.SessionID, &payload.Project, &payload.DeletedAt); err != nil {
			return closeRowsWithError(deletedRows, err)
		}
		payload.Deleted = true
		payload.HardDelete = false
		deletedPending = append(deletedPending, payload)
	}
	if err := deletedRows.Close(); err != nil {
		return err
	}
	if err := deletedRows.Err(); err != nil {
		return err
	}

	// Phase 2: insert deleted observation mutations.
	for _, payload := range deletedPending {
		if err := s.enqueueSyncMutationTx(tx, SyncEntityObservation, payload.SyncID, SyncOpDelete, payload); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) backfillPromptSyncMutationsTx(tx *sql.Tx, project string, fullSnapshot bool) error {
	// ── Live prompts ──────────────────────────────────────────────────────────
	rows, err := s.queryItHook(tx, `
		SELECT p.sync_id, p.session_id, p.content, p.project, p.created_at
		FROM user_prompts p
		LEFT JOIN sessions s ON s.id = p.session_id
		WHERE (
			ifnull(p.project, '') = ?
			OR (ifnull(p.project, '') = '' AND ifnull(s.project, '') = ?)
		)
		  AND NOT EXISTS (
			SELECT 1
			FROM sync_mutations sm
			WHERE sm.target_key = ?
			  AND sm.entity = ?
			  AND sm.entity_key = p.sync_id
			  AND sm.source = ?
			  AND ? = 0
		  )
		ORDER BY p.id ASC`,
		project, project, DefaultSyncTargetKey, SyncEntityPrompt, SyncSourceLocal, fullSnapshot,
	)
	if err != nil {
		return err
	}

	// Phase 1: collect live prompts before any INSERT.
	var pending []syncPromptPayload
	for rows.Next() {
		var payload syncPromptPayload
		if err := rows.Scan(&payload.SyncID, &payload.SessionID, &payload.Content, &payload.Project, &payload.CreatedAt); err != nil {
			return closeRowsWithError(rows, err)
		}
		pending = append(pending, payload)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// Phase 2: insert live prompt mutations.
	for _, payload := range pending {
		if err := s.enqueueSyncMutationTx(tx, SyncEntityPrompt, payload.SyncID, SyncOpUpsert, payload); err != nil {
			return err
		}
	}

	// ── Tombstoned prompts ────────────────────────────────────────────────────
	tombstoneRows, err := s.queryItHook(tx, `
		SELECT prompt_tombstones.sync_id, prompt_tombstones.session_id, prompt_tombstones.project, prompt_tombstones.deleted_at
		FROM prompt_tombstones
		LEFT JOIN sessions s ON s.id = prompt_tombstones.session_id
		WHERE (
			ifnull(prompt_tombstones.project, '') = ?
			OR (ifnull(prompt_tombstones.project, '') = '' AND ifnull(s.project, '') = ?)
		)
		  AND NOT EXISTS (
			SELECT 1
			FROM sync_mutations sm
			WHERE sm.target_key = ?
			  AND sm.entity = ?
			  AND sm.entity_key = prompt_tombstones.sync_id
			  AND sm.source = ?
			  AND sm.op = ?
			  AND ? = 0
		  )
		ORDER BY deleted_at ASC`,
		project, project, DefaultSyncTargetKey, SyncEntityPrompt, SyncSourceLocal, SyncOpDelete, fullSnapshot,
	)
	if err != nil {
		return err
	}

	// Phase 1: collect tombstones before any INSERT.
	var tombstonePending []syncPromptPayload
	for tombstoneRows.Next() {
		var payload syncPromptPayload
		if err := tombstoneRows.Scan(&payload.SyncID, &payload.SessionID, &payload.Project, &payload.DeletedAt); err != nil {
			return closeRowsWithError(tombstoneRows, err)
		}
		payload.Deleted = true
		payload.HardDelete = true
		tombstonePending = append(tombstonePending, payload)
	}
	if err := tombstoneRows.Close(); err != nil {
		return err
	}
	if err := tombstoneRows.Err(); err != nil {
		return err
	}

	// Phase 2: insert tombstone mutations.
	for _, payload := range tombstonePending {
		if err := s.enqueueSyncMutationTx(tx, SyncEntityPrompt, payload.SyncID, SyncOpDelete, payload); err != nil {
			return err
		}
	}
	return nil
}

// backfillRelationSyncMutationsTx creates sync_mutations rows for non-orphaned
// relations that have no corresponding local sync_mutations row.
//
// This fills the cloud-journal gap described in issue #496: a relation can exist
// in memory_relations with no sync_mutations row and therefore never replicates.
//
// Design mirrors backfillObservationSyncMutationsTx exactly:
//   - Phase 1: collect all missing rows into a slice (close cursor first).
//   - Phase 2: insert, avoiding the SQLite cursor-open-during-write busy loop.
//
// The SELECT mirrors ExportRelationMutations' join/orphan-filter structure
// (join both observations, exclude orphaned status, exclude rows that already
// have a local upsert mutation), but scopes by source-observation project only.
// ExportRelationMutations additionally filters by tgt.project; the backfill
// intentionally omits that filter to avoid skipping cross-project edges where
// only the source belongs to this project.
func (s *Store) backfillRelationSyncMutationsTx(tx *sql.Tx, project string, fullSnapshot bool) error {
	// Only backfill fully-judged relations: exclude orphaned/pending and any row
	// that is missing marked_by_actor or marked_by_kind.  Cloud validation
	// (chunkcodec + server) hard-rejects mutations without those fields (HTTP 400),
	// so enqueueing them would block the entire sync batch.
	// This predicate must stay identical to the COUNT in projectNeedsBackfill.
	rows, err := s.queryItHook(tx, `
		SELECT r.sync_id, r.source_id, r.target_id, r.relation, r.reason, r.evidence, r.confidence,
		       r.judgment_status, r.marked_by_actor, r.marked_by_kind, r.marked_by_model,
		       r.session_id,
		       coalesce(nullif(src.project, ''), src_s.project, ''),
		       r.created_at, r.updated_at
		FROM memory_relations r
		JOIN observations src ON src.sync_id = r.source_id AND src.deleted_at IS NULL
		JOIN observations tgt ON tgt.sync_id = r.target_id AND tgt.deleted_at IS NULL
		LEFT JOIN sessions src_s ON src_s.id = src.session_id
		WHERE r.judgment_status NOT IN (?, ?)
		  AND ifnull(r.marked_by_actor, '') != ''
		  AND ifnull(r.marked_by_kind, '') != ''
		  AND coalesce(nullif(src.project, ''), src_s.project, '') = ?
		  AND NOT EXISTS (
		    SELECT 1 FROM sync_mutations sm
		    WHERE sm.target_key = ?
		      AND sm.entity = ?
		      AND sm.entity_key = r.sync_id
		      AND sm.source = ?
			  AND ? = 0
		  )
		ORDER BY r.created_at ASC, r.sync_id ASC`,
		JudgmentStatusOrphaned, JudgmentStatusPending,
		project,
		DefaultSyncTargetKey, SyncEntityRelation, SyncSourceLocal, fullSnapshot,
	)
	if err != nil {
		return err
	}

	// Phase 1: collect into memory before any INSERT to avoid cursor-open-during-write.
	var pending []syncRelationPayload
	for rows.Next() {
		var p syncRelationPayload
		if err := rows.Scan(
			&p.SyncID, &p.SourceID, &p.TargetID, &p.Relation, &p.Reason, &p.Evidence, &p.Confidence,
			&p.JudgmentStatus, &p.MarkedByActor, &p.MarkedByKind, &p.MarkedByModel,
			&p.SessionID, &p.Project, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return closeRowsWithError(rows, err)
		}
		pending = append(pending, p)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// Phase 2: insert now that the read cursor is closed.
	for _, p := range pending {
		if err := s.enqueueSyncMutationTx(tx, SyncEntityRelation, p.SyncID, SyncOpUpsert, p); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) enqueueSyncMutationTx(tx *sql.Tx, entity, entityKey, op string, payload any) error {
	project := extractProjectFromPayload(payload)
	project, _ = NormalizeProject(strings.TrimSpace(project))
	if project == "" {
		sessionID := extractSessionIDFromPayload(payload)
		if sessionID != "" {
			if derived, err := s.resolveSessionProjectTx(tx, sessionID); err != nil {
				return err
			} else {
				project = derived
			}
		}
	}
	if project != "" {
		var enrolled int
		err := tx.QueryRow(`SELECT 1 FROM sync_enrolled_projects WHERE project = ? LIMIT 1`, project).Scan(&enrolled)
		if errors.Is(err, sql.ErrNoRows) {
			// Enrollment backfills current project state atomically, so retaining
			// mutations before enrollment only creates an unbounded local backlog.
			return nil
		}
		if err != nil {
			return err
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := s.execHook(tx,
		`INSERT OR IGNORE INTO sync_state (target_key, lifecycle, updated_at) VALUES (?, ?, datetime('now'))`,
		DefaultSyncTargetKey, SyncLifecycleIdle,
	); err != nil {
		return err
	}
	res, err := s.execHook(tx,
		`INSERT INTO sync_mutations (target_key, entity, entity_key, op, payload, source, project)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		DefaultSyncTargetKey, entity, entityKey, op, string(encoded), SyncSourceLocal, project,
	)
	if err != nil {
		return err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return err
	}
	_, err = s.execHook(tx,
		`UPDATE sync_state
		 SET lifecycle = ?, last_enqueued_seq = ?, updated_at = datetime('now')
		 WHERE target_key = ?`,
		SyncLifecyclePending, seq, DefaultSyncTargetKey,
	)
	if err != nil {
		return err
	}
	if project == "" {
		return nil
	}
	projectTargetKey := syncTargetKeyForProject(project)
	if _, err := s.execHook(tx,
		`INSERT OR IGNORE INTO sync_state (target_key, lifecycle, updated_at) VALUES (?, ?, datetime('now'))`,
		projectTargetKey, SyncLifecycleIdle,
	); err != nil {
		return err
	}
	_, err = s.execHook(tx,
		`UPDATE sync_state
		 SET lifecycle = ?, last_enqueued_seq = ?, updated_at = datetime('now')
		 WHERE target_key = ?`,
		SyncLifecyclePending, seq, projectTargetKey,
	)
	return err
}

func syncTargetKeyForProject(project string) string {
	project, _ = NormalizeProject(project)
	project = strings.TrimSpace(project)
	if project == "" {
		return DefaultSyncTargetKey
	}
	return fmt.Sprintf("%s:%s", DefaultSyncTargetKey, project)
}

func extractSessionIDFromPayload(payload any) string {
	switch p := payload.(type) {
	case syncObservationPayload:
		return strings.TrimSpace(p.SessionID)
	case syncPromptPayload:
		return strings.TrimSpace(p.SessionID)
	default:
		data, err := json.Marshal(payload)
		if err != nil {
			return ""
		}
		var generic struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(data, &generic); err != nil {
			return ""
		}
		return strings.TrimSpace(generic.SessionID)
	}
}

func (s *Store) resolveSessionProjectTx(tx *sql.Tx, sessionID string) (string, error) {
	if strings.TrimSpace(sessionID) == "" {
		return "", nil
	}
	var project string
	err := tx.QueryRow(`SELECT ifnull(project, '') FROM sessions WHERE id = ?`, sessionID).Scan(&project)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	project, _ = NormalizeProject(strings.TrimSpace(project))
	return project, nil
}

func (s *Store) applyPulledMutationTx(tx *sql.Tx, mutation SyncMutation) error {
	switch mutation.Entity {
	case SyncEntityRelation:
		return s.applyRelationUpsertTx(tx, mutation)
	case SyncEntityDependency:
		var payload syncDependencyPayload
		if err := decodeSyncPayload([]byte(mutation.Payload), &payload); err != nil {
			return fmt.Errorf("%w: decode dependency payload: %v", ErrApplyDead, err)
		}
		if err := validatePulledDependencyMutationEnvelope(mutation, payload.SyncID, payload.Project); err != nil {
			return fmt.Errorf("%w: dependency mutation envelope: %v", ErrApplyDead, err)
		}
		if err := s.applyDependencyUpsertTx(tx, payload); err != nil {
			if errors.Is(err, ErrCrossProjectDependency) {
				return fmt.Errorf("%w: dependency mutation project binding: %v", ErrApplyDead, err)
			}
			return err
		}
		return nil
	case SyncEntityResourceDependency:
		var payload syncResourceDependencyPayload
		if err := decodeSyncPayload([]byte(mutation.Payload), &payload); err != nil {
			return fmt.Errorf("%w: decode resource dependency payload: %v", ErrApplyDead, err)
		}
		if err := validatePulledDependencyMutationEnvelope(mutation, payload.SyncID, payload.Project); err != nil {
			return fmt.Errorf("%w: resource dependency mutation envelope: %v", ErrApplyDead, err)
		}
		if err := s.applyResourceDependencyUpsertTx(tx, payload); err != nil {
			if errors.Is(err, ErrCrossProjectResourceDependency) {
				return fmt.Errorf("%w: resource dependency mutation project binding: %v", ErrApplyDead, err)
			}
			return err
		}
		return nil
	case SyncEntitySession:
		var payload syncSessionPayload
		if err := decodeSyncPayload([]byte(mutation.Payload), &payload); err != nil {
			return err
		}
		if strings.TrimSpace(payload.ID) == "" {
			payload.ID = strings.TrimSpace(mutation.EntityKey)
		}
		if mutation.Op == SyncOpDelete || isSessionDeletePayload(payload) {
			return s.applySessionDeleteTx(tx, payload)
		}
		return s.applySessionPayloadTx(tx, payload)
	case SyncEntityObservation:
		var payload syncObservationPayload
		if err := decodeSyncPayload([]byte(mutation.Payload), &payload); err != nil {
			return err
		}
		if mutation.Op == SyncOpDelete {
			return s.applyObservationDeleteTx(tx, payload)
		}
		return s.applyObservationUpsertTx(tx, payload)
	case SyncEntityPrompt:
		var payload syncPromptPayload
		if err := decodeSyncPayload([]byte(mutation.Payload), &payload); err != nil {
			return err
		}
		if mutation.Op == SyncOpDelete || payload.Deleted || payload.HardDelete {
			return s.applyPromptDeleteTx(tx, payload)
		}
		return s.applyPromptUpsertTx(tx, payload)
	default:
		return fmt.Errorf("unknown sync entity %q", mutation.Entity)
	}
}

func validatePulledDependencyMutationEnvelope(mutation SyncMutation, payloadSyncID, payloadProject string) error {
	if strings.TrimSpace(mutation.Op) != SyncOpUpsert {
		return fmt.Errorf("unsupported op %q", mutation.Op)
	}
	if strings.TrimSpace(payloadSyncID) == "" || strings.TrimSpace(payloadSyncID) != strings.TrimSpace(mutation.EntityKey) {
		return fmt.Errorf("payload sync_id must match entity_key")
	}
	outerProject, _ := NormalizeProject(mutation.Project)
	innerProject, _ := NormalizeProject(payloadProject)
	if outerProject == "" || innerProject == "" || innerProject != outerProject {
		return fmt.Errorf("payload project must match mutation project")
	}
	return nil
}

// applyRelationUpsertTx handles a pulled mutation with entity='relation' and
// op='upsert'. It implements the pull-side behavior for Phase 2:
//
//  1. JSON-decode the payload into syncRelationPayload. Decode errors return
//     ErrApplyDead (non-retryable).
//  2. Verify both source and target observations exist locally by sync_id.
//     If either is missing, return ErrRelationFKMissing. The caller must write
//     the raw mutation to sync_apply_deferred and ACK the seq.
//  3. INSERT INTO memory_relations with ON CONFLICT(sync_id) DO UPDATE
//     (last-write-wins, preserving the original created_at).
//  4. On successful apply, DELETE any pre-existing deferred row for this sync_id
//     so it is not retried unnecessarily.
func (s *Store) applyRelationUpsertTx(tx *sql.Tx, mutation SyncMutation) error {
	// Step 1: decode payload.
	var p syncRelationPayload
	if err := decodeSyncPayload([]byte(mutation.Payload), &p); err != nil {
		return fmt.Errorf("%w: decode relation payload: %v", ErrApplyDead, err)
	}

	// Step 1b: required field validation — missing source_id or target_id is not
	// a retryable FK miss; it is a permanent payload defect (ErrApplyDead).
	if strings.TrimSpace(p.SourceID) == "" || strings.TrimSpace(p.TargetID) == "" {
		return fmt.Errorf("%w: relation payload missing required source_id or target_id", ErrApplyDead)
	}

	// Step 2: FK precondition — both observations must exist locally (by sync_id).
	var obsCount int
	if err := tx.QueryRow(
		`SELECT count(*) FROM observations WHERE sync_id IN (?, ?)`,
		p.SourceID, p.TargetID,
	).Scan(&obsCount); err != nil {
		return fmt.Errorf("applyRelationUpsertTx: check observations: %w", err)
	}
	if obsCount < 2 {
		return ErrRelationFKMissing
	}

	// Step 3: upsert into memory_relations keyed on sync_id (idempotent re-apply).
	if _, err := s.execHook(tx, `
		INSERT INTO memory_relations
			(sync_id, source_id, target_id, relation, reason, evidence, confidence,
			 judgment_status, marked_by_actor, marked_by_kind, marked_by_model,
			 session_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(sync_id) DO UPDATE SET
			source_id       = excluded.source_id,
			target_id       = excluded.target_id,
			relation        = excluded.relation,
			reason          = excluded.reason,
			evidence        = excluded.evidence,
			confidence      = excluded.confidence,
			judgment_status = excluded.judgment_status,
			marked_by_actor = excluded.marked_by_actor,
			marked_by_kind  = excluded.marked_by_kind,
			marked_by_model = excluded.marked_by_model,
			session_id      = excluded.session_id,
			updated_at      = excluded.updated_at
	`,
		p.SyncID, p.SourceID, p.TargetID, p.Relation,
		p.Reason, p.Evidence, p.Confidence,
		p.JudgmentStatus, p.MarkedByActor, p.MarkedByKind, p.MarkedByModel,
		p.SessionID, p.CreatedAt, p.UpdatedAt,
	); err != nil {
		return fmt.Errorf("applyRelationUpsertTx: upsert: %w", err)
	}

	// Step 4: clean up deferred row if one exists (resolves any prior FK-miss deferral).
	if _, err := s.execHook(tx,
		`DELETE FROM sync_apply_deferred WHERE sync_id = ?`, p.SyncID,
	); err != nil {
		return fmt.Errorf("applyRelationUpsertTx: clear deferred: %w", err)
	}

	return nil
}

// extractProjectFromPayload returns the project string from a sync payload struct.
// It handles both string and *string Project fields across all entity payload types.
// Returns empty string if the payload has no project or project is nil.
func extractProjectFromPayload(payload any) string {
	switch p := payload.(type) {
	case syncSessionPayload:
		return p.Project
	case syncObservationPayload:
		if p.Project != nil {
			return *p.Project
		}
		return ""
	case syncPromptPayload:
		if p.Project != nil {
			return *p.Project
		}
		return ""
	case syncRelationPayload:
		return p.Project
	case syncDependencyPayload:
		return p.Project
	case syncResourceDependencyPayload:
		return p.Project
	default:
		// Fallback: marshal to JSON and extract $.project via json.Unmarshal.
		data, err := json.Marshal(payload)
		if err != nil {
			return ""
		}
		var generic struct {
			Project *string `json:"project"`
		}
		if err := json.Unmarshal(data, &generic); err != nil || generic.Project == nil {
			return ""
		}
		return *generic.Project
	}
}

func decodeSyncPayload(payload []byte, dest any) error {
	trimmed := strings.TrimSpace(string(payload))
	if trimmed == "" {
		return fmt.Errorf("empty payload")
	}
	if trimmed[0] == '"' {
		var encoded string
		if err := json.Unmarshal([]byte(trimmed), &encoded); err != nil {
			return err
		}
		trimmed = encoded
	}
	raw, err := normalizeTimestampJSON([]byte(trimmed))
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, dest)
}

func (s *Store) getObservationTx(tx *sql.Tx, id int64) (*Observation, error) {
	row := tx.QueryRow(
		`SELECT `+observationSelectColumns+`
		 FROM observations WHERE id = ? AND deleted_at IS NULL`, id,
	)
	var o Observation
	if err := scanObservationRow(row, &o); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrObservationNotFound
		}
		return nil, err
	}
	return &o, nil
}

func (s *Store) getObservationBySyncIDTx(tx *sql.Tx, syncID string, includeDeleted bool) (*Observation, error) {
	query := `SELECT ` + observationSelectColumns + `
		 FROM observations WHERE sync_id = ?`
	if !includeDeleted {
		query += ` AND deleted_at IS NULL`
	}
	query += ` ORDER BY id DESC LIMIT 1`
	row := tx.QueryRow(query, syncID)
	var o Observation
	if err := scanObservationRow(row, &o); err != nil {
		return nil, err
	}
	return &o, nil
}

func observationPayloadFromObservation(obs *Observation) syncObservationPayload {
	return syncObservationPayload{
		ArchivedAt:      obs.ArchivedAt,
		SyncID:          obs.SyncID,
		SessionID:       obs.SessionID,
		Type:            obs.Type,
		MemoryClass:     obs.MemoryClass,
		MemoryPath:      obs.MemoryPath,
		Abstract:        obs.Abstract,
		Overview:        obs.Overview,
		SummaryState:    obs.SummaryState,
		SummaryRevision: obs.SummaryRevision,
		StaleAt:         obs.StaleAt,
		StaleReason:     obs.StaleReason,
		Title:           obs.Title,
		Content:         obs.Content,
		ToolName:        obs.ToolName,
		Project:         obs.Project,
		Scope:           obs.Scope,
		TopicKey:        obs.TopicKey,
		RevisionCount:   obs.RevisionCount,
		DuplicateCount:  obs.DuplicateCount,
		LastSeenAt:      obs.LastSeenAt,
		CreatedAt:       obs.CreatedAt,
		UpdatedAt:       obs.UpdatedAt,
	}
}

func (s *Store) applySessionPayloadTx(tx *sql.Tx, payload syncSessionPayload) error {
	if isSessionDeletePayload(payload) {
		return s.applySessionDeleteTx(tx, payload)
	}
	_, err := s.execHook(tx,
		`INSERT INTO sessions (id, project, directory, started_at, ended_at, summary, origin)
		 VALUES (?, ?, ?, COALESCE(NULLIF(?, ''), datetime('now')), ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   project = excluded.project,
		   directory = excluded.directory,
		   started_at = COALESCE(NULLIF(excluded.started_at, ''), sessions.started_at),
		   ended_at = COALESCE(excluded.ended_at, sessions.ended_at),
		   summary = COALESCE(excluded.summary, sessions.summary),
		   origin = CASE
		              WHEN ifnull(sessions.origin, '') IN ('', 'unknown') THEN ifnull(excluded.origin, 'unknown')
		              WHEN ifnull(excluded.origin, '') = 'host' THEN 'host'
		              WHEN ifnull(excluded.origin, '') = 'operator' AND ifnull(sessions.origin, '') = 'model' THEN 'operator'
		              ELSE ifnull(sessions.origin, 'unknown')
		            END`,
		payload.ID, payload.Project, payload.Directory, strings.TrimSpace(payload.StartedAt), payload.EndedAt, payload.Summary, NormalizeSessionOrigin(payload.Origin),
	)
	return err
}

func isSessionDeletePayload(payload syncSessionPayload) bool {
	if payload.Deleted || payload.HardDelete {
		return true
	}
	if payload.DeletedAt == nil {
		return false
	}
	return strings.TrimSpace(*payload.DeletedAt) != ""
}

func (s *Store) applySessionDeleteTx(tx *sql.Tx, payload syncSessionPayload) error {
	sessionID := strings.TrimSpace(payload.ID)
	if sessionID == "" {
		return nil
	}
	if _, err := s.execHook(tx, `DELETE FROM user_prompts WHERE session_id = ?`, sessionID); err != nil {
		return err
	}
	_, err := s.execHook(tx, `DELETE FROM sessions WHERE id = ?`, sessionID)
	return err
}

func (s *Store) applyObservationUpsertTx(tx *sql.Tx, payload syncObservationPayload) error {
	var tombstone int
	if err := tx.QueryRow(`SELECT count(*) FROM observation_tombstones WHERE sync_id=?`, payload.SyncID).Scan(&tombstone); err != nil {
		return err
	}
	if tombstone != 0 {
		return nil
	}
	revisionCount := maxInt(payload.RevisionCount, 1)
	duplicateCount := maxInt(payload.DuplicateCount, 1)
	createdAt := strings.TrimSpace(payload.CreatedAt)
	updatedAt := strings.TrimSpace(payload.UpdatedAt)
	if createdAt == "" {
		createdAt = Now()
	}
	if updatedAt == "" {
		updatedAt = createdAt
	}

	existing, err := s.getObservationBySyncIDTx(tx, payload.SyncID, true)
	if err == sql.ErrNoRows {
		memoryClass, classErr := resolveMemoryClass(payload.MemoryClass, payload.Type)
		if classErr != nil {
			return classErr
		}
		memoryPath, pathErr := NormalizeMemoryPath(memoryClass, payload.MemoryPath)
		if pathErr != nil {
			return pathErr
		}
		summaryState, summaryRevision, summaryErr := normalizeSummaryFields(
			payload.Abstract, payload.Overview, payload.SummaryState, payload.SummaryRevision,
		)
		if summaryErr != nil {
			return summaryErr
		}
		_, err = s.execHook(tx,
			`INSERT INTO observations (
				sync_id, session_id, type, memory_class, memory_path, abstract, overview,
				summary_state, summary_revision, stale_at, stale_reason, title, content,
				tool_name, project, scope, topic_key, normalized_hash, revision_count,
				duplicate_count, last_seen_at, created_at, updated_at, deleted_at, archived_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)`,
			payload.SyncID,
			payload.SessionID,
			payload.Type,
			memoryClass,
			nullableString(memoryPath),
			nullableString(payload.Abstract),
			nullableString(payload.Overview),
			summaryState,
			summaryRevision,
			payload.StaleAt,
			payload.StaleReason,
			payload.Title,
			payload.Content,
			payload.ToolName,
			payload.Project,
			normalizeScope(payload.Scope),
			payload.TopicKey,
			hashNormalized(payload.Content),
			revisionCount,
			duplicateCount,
			payload.LastSeenAt,
			createdAt,
			updatedAt,
			payload.ArchivedAt,
		)
		if err != nil {
			return err
		}
		return s.rebuildMemoryNodesTx(tx, derefString(payload.Project), normalizeScope(payload.Scope))
	}
	if err != nil {
		return err
	}
	if existing.DeletedAt != nil {
		return nil
	}

	if payload.RevisionCount <= 0 {
		revisionCount = maxInt(existing.RevisionCount, 1)
	}
	if payload.DuplicateCount <= 0 {
		duplicateCount = maxInt(existing.DuplicateCount, 1)
	}
	if payload.LastSeenAt == nil {
		payload.LastSeenAt = existing.LastSeenAt
	}
	if strings.TrimSpace(payload.CreatedAt) == "" {
		createdAt = existing.CreatedAt
	}
	if strings.TrimSpace(payload.UpdatedAt) == "" {
		updatedAt = existing.UpdatedAt
	}
	memoryClass := existing.MemoryClass
	if strings.TrimSpace(payload.MemoryClass) != "" {
		memoryClass, err = normalizeMemoryClass(payload.MemoryClass)
		if err != nil {
			return err
		}
	}
	memoryPath := existing.MemoryPath
	if strings.TrimSpace(payload.MemoryPath) != "" {
		memoryPath, err = NormalizeMemoryPath(memoryClass, payload.MemoryPath)
	} else if memoryPath != "" {
		memoryPath, err = NormalizeMemoryPath(memoryClass, memoryPath)
	}
	if err != nil {
		return err
	}
	abstract, overview := existing.Abstract, existing.Overview
	summaryState, summaryRevision := existing.SummaryState, existing.SummaryRevision
	staleAt, staleReason := existing.StaleAt, existing.StaleReason
	if strings.TrimSpace(payload.Abstract) != "" || strings.TrimSpace(payload.Overview) != "" || strings.TrimSpace(payload.SummaryState) != "" {
		abstract, overview = payload.Abstract, payload.Overview
		summaryState, summaryRevision, err = normalizeSummaryFields(abstract, overview, payload.SummaryState, payload.SummaryRevision)
		if err != nil {
			return err
		}
		staleAt, staleReason = payload.StaleAt, payload.StaleReason
	}

	contentRecomputed := payload.Content != existing.Content && payload.StaleAt == nil
	if contentRecomputed {
		if err := s.rebindIncomingDependenciesTx(tx, existing.SyncID, false); err != nil {
			return err
		}
	}
	sourcesValid, err := observationSourcesValidTx(tx, existing.SyncID)
	if err != nil {
		return err
	}
	if staleAt == nil && (!sourcesValid || summaryState == SummaryStateStale ||
		existing.StaleAt != nil && !contentRecomputed && !(existing.SummaryState == SummaryStateStale && summaryState != SummaryStateStale)) {
		staleAt, staleReason = existing.StaleAt, existing.StaleReason
		if staleAt == nil {
			now, reason := Now(), "source_dependencies_unresolved"
			staleAt, staleReason = &now, &reason
		}
	}
	_, err = s.execHook(tx,
		`UPDATE observations
		 SET session_id = ?, type = ?, memory_class = ?, memory_path = ?, abstract = ?, overview = ?,
		     summary_state = ?, summary_revision = ?, stale_at = ?, stale_reason = ?,
		     title = ?, content = ?, tool_name = ?, project = ?, scope = ?, topic_key = ?,
		     normalized_hash = ?, revision_count = ?, duplicate_count = ?, last_seen_at = ?,
		     created_at = ?, updated_at = ?, deleted_at = NULL, archived_at = ?
		 WHERE id = ?`,
		payload.SessionID,
		payload.Type,
		memoryClass,
		nullableString(memoryPath),
		nullableString(abstract),
		nullableString(overview),
		summaryState,
		summaryRevision,
		staleAt,
		staleReason,
		payload.Title,
		payload.Content,
		payload.ToolName,
		payload.Project,
		normalizeScope(payload.Scope),
		payload.TopicKey,
		hashNormalized(payload.Content),
		revisionCount,
		duplicateCount,
		payload.LastSeenAt,
		createdAt,
		updatedAt,
		payload.ArchivedAt,
		existing.ID,
	)
	if err != nil {
		return err
	}
	sourceChanged := revisionCount > existing.RevisionCount || payload.Type != existing.Type || payload.Title != existing.Title || payload.Content != existing.Content
	if err := s.rebuildMemoryNodesTx(tx, derefString(existing.Project), existing.Scope); err != nil {
		return err
	}
	newProject := derefString(payload.Project)
	newScope := normalizeScope(payload.Scope)
	if newProject != derefString(existing.Project) || newScope != existing.Scope {
		if err := s.rebuildMemoryNodesTx(tx, newProject, newScope); err != nil {
			return err
		}
	}
	if sourceChanged {
		return s.invalidateDependentsTx(tx, existing.SyncID, existing.SyncID, revisionCount, "source_updated", false)
	}
	return nil
}

func (s *Store) applyObservationDeleteTx(tx *sql.Tx, payload syncObservationPayload) error {
	deletedAt := derefString(payload.DeletedAt)
	if strings.TrimSpace(deletedAt) == "" {
		deletedAt = Now()
	}
	if _, err := s.execHook(tx, `INSERT INTO observation_tombstones(sync_id,session_id,project,deleted_at)
		VALUES(?,?,?,?) ON CONFLICT(sync_id) DO UPDATE SET deleted_at=excluded.deleted_at`, payload.SyncID, payload.SessionID, payload.Project, deletedAt); err != nil {
		return err
	}
	existing, err := s.getObservationBySyncIDTx(tx, payload.SyncID, true)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.invalidateDependentsTx(tx, existing.SyncID, existing.SyncID, existing.RevisionCount, "source_deleted", false); err != nil {
		return err
	}
	if payload.HardDelete {
		_, err = s.execHook(tx, `DELETE FROM observations WHERE id = ?`, existing.ID)
		if err != nil {
			return err
		}
		return s.rebuildMemoryNodesTx(tx, derefString(existing.Project), existing.Scope)
	}
	_, err = s.execHook(tx,
		`UPDATE observations SET deleted_at = ?, updated_at = datetime('now') WHERE id = ?`,
		deletedAt, existing.ID,
	)
	if err != nil {
		return err
	}
	return s.rebuildMemoryNodesTx(tx, derefString(existing.Project), existing.Scope)
}

func (s *Store) applyPromptUpsertTx(tx *sql.Tx, payload syncPromptPayload) error {
	var tombstoneDeletedAt string
	err := tx.QueryRow(`SELECT deleted_at FROM prompt_tombstones WHERE sync_id = ?`, payload.SyncID).Scan(&tombstoneDeletedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		if isStalePromptUpsert(payload, tombstoneDeletedAt) {
			return nil
		}
		if _, err := s.execHook(tx, `DELETE FROM prompt_tombstones WHERE sync_id = ?`, payload.SyncID); err != nil {
			return err
		}
	}

	var existingID int64
	err = tx.QueryRow(`SELECT id FROM user_prompts WHERE sync_id = ? ORDER BY id DESC LIMIT 1`, payload.SyncID).Scan(&existingID)
	if err == sql.ErrNoRows {
		if strings.TrimSpace(payload.CreatedAt) == "" {
			_, err = s.execHook(tx,
				`INSERT INTO user_prompts (sync_id, session_id, content, project) VALUES (?, ?, ?, ?)`,
				payload.SyncID, payload.SessionID, payload.Content, payload.Project,
			)
		} else {
			_, err = s.execHook(tx,
				`INSERT INTO user_prompts (sync_id, session_id, content, project, created_at) VALUES (?, ?, ?, ?, ?)`,
				payload.SyncID, payload.SessionID, payload.Content, payload.Project, payload.CreatedAt,
			)
		}
		return err
	}
	if err != nil {
		return err
	}
	_, err = s.execHook(tx,
		`UPDATE user_prompts
		 SET session_id = ?,
		     content = ?,
		     project = ?,
		     created_at = CASE WHEN ? = '' THEN created_at ELSE ? END
		 WHERE id = ?`,
		payload.SessionID, payload.Content, payload.Project, strings.TrimSpace(payload.CreatedAt), payload.CreatedAt, existingID,
	)
	return err
}

func (s *Store) applyPromptDeleteTx(tx *sql.Tx, payload syncPromptPayload) error {
	if strings.TrimSpace(payload.SyncID) == "" {
		return nil
	}
	if _, err := s.execHook(tx, `DELETE FROM user_prompts WHERE sync_id = ?`, payload.SyncID); err != nil {
		return err
	}
	deletedAt := payload.DeletedAt
	if deletedAt == nil || strings.TrimSpace(*deletedAt) == "" {
		now := Now()
		deletedAt = &now
	}
	_, err := s.execHook(tx,
		`INSERT INTO prompt_tombstones (sync_id, session_id, project, deleted_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(sync_id) DO UPDATE SET session_id = excluded.session_id, project = excluded.project, deleted_at = excluded.deleted_at`,
		payload.SyncID, payload.SessionID, payload.Project, *deletedAt,
	)
	return err
}

func isStalePromptUpsert(payload syncPromptPayload, tombstoneDeletedAt string) bool {
	upsertTime := normalizeComparableTimestamp(payload.CreatedAt)
	if strings.TrimSpace(upsertTime) == "" {
		return true
	}
	return upsertTime <= normalizeComparableTimestamp(tombstoneDeletedAt)
}

func normalizeComparableTimestamp(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	if parsed, err := time.Parse(time.RFC3339, trimmed); err == nil {
		return parsed.UTC().Format("2006-01-02 15:04:05")
	}
	return trimmed
}

func parseObservationTime(value string) (time.Time, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	formats := []string{"2006-01-02 15:04:05", "2006-01-02 15:04:05Z07:00", time.RFC3339, time.RFC3339Nano, "2006-01-02"}
	for _, layout := range formats {
		if parsed, err := time.Parse(layout, trimmed); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported timestamp %q", value)
}

type observationScanner interface {
	Scan(dest ...any) error
}

func scanObservationRow(scanner observationScanner, o *Observation) error {
	return scanner.Scan(
		&o.ID, &o.SyncID, &o.SessionID, &o.Type, &o.MemoryClass,
		&o.MemoryPath, &o.Abstract, &o.Overview, &o.SummaryState, &o.SummaryRevision, &o.StaleAt, &o.StaleReason,
		&o.Title, &o.Content, &o.ToolName, &o.Project, &o.Scope, &o.TopicKey, &o.RevisionCount, &o.DuplicateCount, &o.LastSeenAt, &o.ReviewAfter,
		&o.RecallCount, &o.LastRecallAt,
		&o.Pinned, &o.CreatedAt, &o.UpdatedAt, &o.DeletedAt, &o.ArchivedAt,
	)
}

func (s *Store) queryObservations(query string, args ...any) ([]Observation, error) {
	rows, err := s.queryItHook(s.db, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []Observation
	for rows.Next() {
		var o Observation
		if err := scanObservationRow(rows, &o); err != nil {
			return nil, err
		}
		results = append(results, o)
	}
	return results, rows.Err()
}

type memoryNodeSource struct {
	Path            string
	Title           string
	Content         string
	Abstract        string
	Overview        string
	RevisionCount   int
	SummaryRevision int
	SummaryState    string
}

type memoryNodeAggregate struct {
	abstracts       []string
	overviews       []string
	contentRevision int
	summaryRevision int
	summaryState    string
}

// ListMemoryNodes returns the active, deterministic virtual hierarchy for a
// project and scope. Nodes are a rebuildable index derived from observations.
func (s *Store) ListMemoryNodes(project, scope string) ([]MemoryNode, error) {
	project, _ = NormalizeProject(project)
	scope = normalizeScope(scope)
	rows, err := s.queryItHook(s.db, `
		SELECT sync_id, project, scope, path, ifnull(parent_path, ''),
		       ifnull(abstract, ''), ifnull(overview, ''), content_revision,
		       summary_revision, summary_state, created_at, updated_at, deleted_at
		FROM memory_nodes
		WHERE project = ? AND scope = ? AND deleted_at IS NULL
		ORDER BY path ASC`, project, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var nodes []MemoryNode
	for rows.Next() {
		var node MemoryNode
		if err := rows.Scan(
			&node.SyncID, &node.Project, &node.Scope, &node.Path, &node.ParentPath,
			&node.Abstract, &node.Overview, &node.ContentRevision, &node.SummaryRevision,
			&node.SummaryState, &node.CreatedAt, &node.UpdatedAt, &node.DeletedAt,
		); err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	return nodes, rows.Err()
}

func (s *Store) rebuildAllMemoryNodes() error {
	rows, err := s.queryItHook(s.db, `
		SELECT project, scope FROM (
			SELECT DISTINCT ifnull(project, '') AS project, scope
			FROM observations
			WHERE trim(ifnull(memory_path, '')) != ''
			UNION
			SELECT DISTINCT project, scope FROM memory_nodes WHERE deleted_at IS NULL
		)
		ORDER BY project, scope`)
	if err != nil {
		return err
	}
	type projectScope struct{ project, scope string }
	var pairs []projectScope
	for rows.Next() {
		var pair projectScope
		if err := rows.Scan(&pair.project, &pair.scope); err != nil {
			return closeRowsWithError(rows, err)
		}
		pairs = append(pairs, pair)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return s.withTx(func(tx *sql.Tx) error {
		for _, pair := range pairs {
			if err := s.rebuildMemoryNodesTx(tx, pair.project, pair.scope); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) rebuildMemoryNodesTx(tx *sql.Tx, project, scope string) error {
	project, _ = NormalizeProject(project)
	scope = normalizeScope(scope)
	rows, err := s.queryItHook(tx, `
		SELECT memory_path, title, content, ifnull(abstract, ''), ifnull(overview, ''),
		       revision_count, summary_revision, summary_state
		FROM observations
		WHERE ifnull(project, '') = ? AND scope = ? AND deleted_at IS NULL AND archived_at IS NULL
		  AND trim(ifnull(memory_path, '')) != ''
		ORDER BY memory_path ASC, id ASC`, project, scope)
	if err != nil {
		return err
	}
	var sources []memoryNodeSource
	for rows.Next() {
		var source memoryNodeSource
		if err := rows.Scan(
			&source.Path, &source.Title, &source.Content, &source.Abstract, &source.Overview,
			&source.RevisionCount, &source.SummaryRevision, &source.SummaryState,
		); err != nil {
			return closeRowsWithError(rows, err)
		}
		sources = append(sources, source)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}

	aggregates := make(map[string]*memoryNodeAggregate)
	for _, source := range sources {
		parts := strings.Split(source.Path, "/")
		for depth := 1; depth <= len(parts); depth++ {
			path := strings.Join(parts[:depth], "/")
			agg := aggregates[path]
			if agg == nil {
				agg = &memoryNodeAggregate{summaryState: SummaryStateFresh}
				aggregates[path] = agg
			}
			abstract := strings.TrimSpace(source.Abstract)
			if abstract == "" {
				abstract = source.Title
			}
			overview := strings.TrimSpace(source.Overview)
			if overview == "" {
				overview = truncate(source.Content, 300)
			}
			agg.abstracts = appendUniqueBounded(agg.abstracts, abstract, 8)
			agg.overviews = appendUniqueBounded(agg.overviews, overview, 8)
			agg.contentRevision = maxInt(agg.contentRevision, source.RevisionCount)
			agg.summaryRevision = maxInt(agg.summaryRevision, source.SummaryRevision)
			switch source.SummaryState {
			case SummaryStateStale:
				agg.summaryState = SummaryStateStale
			case SummaryStateMissing:
				if agg.summaryState != SummaryStateStale {
					agg.summaryState = SummaryStateMissing
				}
			}
		}
	}

	if _, err := s.execHook(tx, `
		UPDATE memory_nodes
		SET deleted_at = datetime('now'), updated_at = datetime('now')
		WHERE project = ? AND scope = ? AND deleted_at IS NULL
		  AND NOT EXISTS (
			SELECT 1 FROM observations o
			WHERE ifnull(o.project, '') = memory_nodes.project AND o.scope = memory_nodes.scope
			  AND o.deleted_at IS NULL AND o.archived_at IS NULL
			  AND (o.memory_path = memory_nodes.path
			       OR substr(o.memory_path, 1, length(memory_nodes.path) + 1) = memory_nodes.path || '/')
		  )`, project, scope); err != nil {
		return err
	}
	paths := make([]string, 0, len(aggregates))
	for path := range aggregates {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		agg := aggregates[path]
		parentPath := ""
		if index := strings.LastIndex(path, "/"); index >= 0 {
			parentPath = path[:index]
		}
		if _, err := s.execHook(tx, `
			INSERT INTO memory_nodes (
				sync_id, project, scope, path, parent_path, abstract, overview,
				content_revision, summary_revision, summary_state, updated_at, deleted_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'), NULL)
			ON CONFLICT(project, scope, path) DO UPDATE SET
				sync_id = excluded.sync_id,
				parent_path = excluded.parent_path,
				abstract = excluded.abstract,
				overview = excluded.overview,
				content_revision = excluded.content_revision,
				summary_revision = excluded.summary_revision,
				summary_state = excluded.summary_state,
				updated_at = datetime('now'),
				deleted_at = NULL
			WHERE memory_nodes.sync_id IS NOT excluded.sync_id
			   OR memory_nodes.parent_path IS NOT excluded.parent_path
			   OR memory_nodes.abstract IS NOT excluded.abstract
			   OR memory_nodes.overview IS NOT excluded.overview
			   OR memory_nodes.content_revision IS NOT excluded.content_revision
			   OR memory_nodes.summary_revision IS NOT excluded.summary_revision
			   OR memory_nodes.summary_state IS NOT excluded.summary_state
			   OR memory_nodes.deleted_at IS NOT NULL`,
			nodeSyncID(project, scope, path), project, scope, path, nullableString(parentPath),
			truncate(strings.Join(agg.abstracts, " | "), 1000),
			truncate(strings.Join(agg.overviews, "\n"), 4000),
			agg.contentRevision, agg.summaryRevision, agg.summaryState,
		); err != nil {
			return err
		}
	}
	return nil
}

func appendUniqueBounded(values []string, value string, limit int) []string {
	value = strings.TrimSpace(value)
	if value == "" || len(values) >= limit {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func nodeSyncID(project, scope, path string) string {
	sum := sha256.Sum256([]byte(project + "\x00" + scope + "\x00" + path))
	return "node-" + hex.EncodeToString(sum[:16])
}

func (s *Store) addColumnIfNotExists(tableName, columnName, definition string) error {
	rows, err := s.queryItHook(s.db, fmt.Sprintf("PRAGMA table_info(%s)", tableName))
	if err != nil {
		return err
	}

	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return closeRowsWithError(rows, err)
		}
		if name == columnName {
			rows.Close()
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	_, err = s.db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", tableName, columnName, definition))
	return err
}

func (s *Store) migrateSyncChunksTable() error {
	rows, err := s.queryItHook(s.db, "PRAGMA table_info(sync_chunks)")
	if err != nil {
		return err
	}

	hasTargetKey := false
	targetKeyPK := 0
	chunkIDPK := 0
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return closeRowsWithError(rows, err)
		}
		switch name {
		case "target_key":
			hasTargetKey = true
			targetKeyPK = pk
		case "chunk_id":
			chunkIDPK = pk
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	// Already migrated: composite PK (target_key, chunk_id).
	if hasTargetKey && targetKeyPK == 1 && chunkIDPK == 2 {
		return nil
	}

	tx, err := s.beginTxHook()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := s.execHook(tx, `
		CREATE TABLE IF NOT EXISTS sync_chunks_new (
			target_key  TEXT NOT NULL DEFAULT 'local',
			chunk_id    TEXT NOT NULL,
			imported_at TEXT NOT NULL DEFAULT (datetime('now')),
			PRIMARY KEY (target_key, chunk_id)
		)
	`); err != nil {
		return err
	}

	if hasTargetKey {
		if _, err := s.execHook(tx, `
			INSERT OR IGNORE INTO sync_chunks_new (target_key, chunk_id, imported_at)
			SELECT CASE
				WHEN trim(ifnull(target_key, '')) = '' THEN ?
				ELSE trim(target_key)
			END,
			chunk_id,
			imported_at
			FROM sync_chunks
		`, LocalChunkTargetKey); err != nil {
			return err
		}
	} else {
		if _, err := s.execHook(tx, `
			INSERT OR IGNORE INTO sync_chunks_new (target_key, chunk_id, imported_at)
			SELECT ?, chunk_id, imported_at
			FROM sync_chunks
		`, LocalChunkTargetKey); err != nil {
			return err
		}
	}

	if _, err := s.execHook(tx, `DROP TABLE sync_chunks`); err != nil {
		return err
	}
	if _, err := s.execHook(tx, `ALTER TABLE sync_chunks_new RENAME TO sync_chunks`); err != nil {
		return err
	}

	return s.commitHook(tx)
}

func (s *Store) migrateLegacyObservationsTable() error {
	rows, err := s.queryItHook(s.db, "PRAGMA table_info(observations)")
	if err != nil {
		return err
	}

	var hasID bool
	var idIsPrimaryKey bool
	columns := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return closeRowsWithError(rows, err)
		}
		columns[name] = true
		if name == "id" {
			hasID = true
			idIsPrimaryKey = pk == 1
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	if !hasID || idIsPrimaryKey {
		return nil
	}

	tx, err := s.beginTxHook()
	if err != nil {
		return fmt.Errorf("migrate legacy observations: begin tx: %w", err)
	}
	defer tx.Rollback()
	legacyColumn := func(name, fallback string) string {
		if columns[name] {
			return name
		}
		return fallback
	}
	pinned := "0"
	if columns["pinned"] {
		pinned = "CASE WHEN pinned=1 THEN 1 ELSE 0 END"
	}
	syncID := "'obs-' || lower(hex(randomblob(16)))"
	if columns["sync_id"] {
		syncID = "COALESCE(NULLIF(trim(sync_id),''),'obs-' || lower(hex(randomblob(16))))"
	}
	metadata := strings.Join([]string{
		legacyColumn("memory_class", "'project'"), legacyColumn("memory_path", "NULL"),
		legacyColumn("abstract", "NULL"), legacyColumn("overview", "NULL"),
		legacyColumn("summary_state", "'missing'"), legacyColumn("summary_revision", "0"),
		legacyColumn("stale_at", "NULL"), legacyColumn("stale_reason", "NULL"),
		legacyColumn("recall_count", "0"), legacyColumn("last_recall_at", "NULL"),
		legacyColumn("archived_at", "NULL"), legacyColumn("review_after", "NULL"),
	}, ",")

	if _, err := s.execHook(tx, `
		CREATE TABLE observations_migrated (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			sync_id    TEXT,
			session_id TEXT    NOT NULL,
			type       TEXT    NOT NULL,
			memory_class TEXT NOT NULL DEFAULT 'project',
			memory_path TEXT,
			abstract TEXT,
			overview TEXT,
			summary_state TEXT NOT NULL DEFAULT 'missing',
			summary_revision INTEGER NOT NULL DEFAULT 0,
			stale_at TEXT,
			stale_reason TEXT,
			title      TEXT    NOT NULL,
			content    TEXT    NOT NULL,
			tool_name  TEXT,
			project    TEXT,
			scope      TEXT    NOT NULL DEFAULT 'project',
			topic_key  TEXT,
			normalized_hash TEXT,
			revision_count INTEGER NOT NULL DEFAULT 1,
			duplicate_count INTEGER NOT NULL DEFAULT 1,
			last_seen_at TEXT,
			pinned     BOOLEAN NOT NULL DEFAULT 0,
			recall_count INTEGER NOT NULL DEFAULT 0,
			last_recall_at TEXT,
			created_at TEXT    NOT NULL DEFAULT (datetime('now')),
			updated_at TEXT    NOT NULL DEFAULT (datetime('now')),
			deleted_at TEXT,
			archived_at TEXT,
			review_after TEXT,
			FOREIGN KEY (session_id) REFERENCES sessions(id)
		);
	`); err != nil {
		return fmt.Errorf("migrate legacy observations: create table: %w", err)
	}

	if _, err := s.execHook(tx, `
		INSERT INTO observations_migrated (
			id, sync_id, session_id, type, title, content, tool_name, project,
			scope, topic_key, normalized_hash, revision_count, duplicate_count,
			last_seen_at, pinned, created_at, updated_at, deleted_at,
			memory_class,memory_path,abstract,overview,summary_state,summary_revision,stale_at,stale_reason,
			recall_count,last_recall_at,archived_at,review_after
		)
		SELECT
			CASE
				WHEN id IS NULL THEN NULL
				WHEN ROW_NUMBER() OVER (PARTITION BY id ORDER BY rowid) = 1 THEN CAST(id AS INTEGER)
				ELSE NULL
			END,
			`+syncID+`,
			session_id,
			COALESCE(NULLIF(type, ''), 'manual'),
			COALESCE(NULLIF(title, ''), 'Untitled observation'),
			COALESCE(content, ''),
			tool_name,
			project,
			CASE WHEN scope IS NULL OR scope = '' THEN 'project' ELSE scope END,
			NULLIF(topic_key, ''),
			normalized_hash,
			CASE WHEN revision_count IS NULL OR revision_count < 1 THEN 1 ELSE revision_count END,
			CASE WHEN duplicate_count IS NULL OR duplicate_count < 1 THEN 1 ELSE duplicate_count END,
			last_seen_at,
			`+pinned+`,
			COALESCE(NULLIF(created_at, ''), datetime('now')),
			COALESCE(NULLIF(updated_at, ''), NULLIF(created_at, ''), datetime('now')),
			deleted_at,
			`+metadata+`
		FROM observations
		ORDER BY rowid;
	`); err != nil {
		return fmt.Errorf("migrate legacy observations: copy rows: %w", err)
	}

	if _, err := s.execHook(tx, "DROP TABLE observations"); err != nil {
		return fmt.Errorf("migrate legacy observations: drop old table: %w", err)
	}

	if _, err := s.execHook(tx, "ALTER TABLE observations_migrated RENAME TO observations"); err != nil {
		return fmt.Errorf("migrate legacy observations: rename table: %w", err)
	}

	if _, err := s.execHook(tx, `
		DROP TRIGGER IF EXISTS obs_fts_insert;
		DROP TRIGGER IF EXISTS obs_fts_update;
		DROP TRIGGER IF EXISTS obs_fts_delete;
		DROP TABLE IF EXISTS observations_fts;
		CREATE VIRTUAL TABLE observations_fts USING fts5(
			title,
			content,
			tool_name,
			type,
			project,
			topic_key,
			content='observations',
			content_rowid='id'
		);
		INSERT INTO observations_fts(rowid, title, content, tool_name, type, project, topic_key)
		SELECT id, title, content, tool_name, type, project, topic_key
		FROM observations
		WHERE deleted_at IS NULL;
	`); err != nil {
		return fmt.Errorf("migrate legacy observations: rebuild fts: %w", err)
	}

	if err := s.commitHook(tx); err != nil {
		return fmt.Errorf("migrate legacy observations: commit: %w", err)
	}

	return nil
}

func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func truncateStoredUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + "... [truncated]"
}

func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "..."
}

func normalizeScope(scope string) string {
	v := strings.TrimSpace(strings.ToLower(scope))
	switch v {
	case "personal", "global":
		return v
	default:
		return "project"
	}
}

// NormalizeProject applies canonical project name normalization:
// lowercase + trim whitespace + collapse consecutive hyphens/underscores.
// Returns the normalized name and a warning message if the name was changed
// (empty string if no change was needed).
// Exported so MCP and CLI handlers can surface the warning to users.
func NormalizeProject(project string) (normalized string, warning string) {
	if project == "" {
		return "", ""
	}
	n := strings.TrimSpace(strings.ToLower(project))
	// Collapse multiple consecutive hyphens
	for strings.Contains(n, "--") {
		n = strings.ReplaceAll(n, "--", "-")
	}
	// Collapse multiple consecutive underscores
	for strings.Contains(n, "__") {
		n = strings.ReplaceAll(n, "__", "_")
	}
	if n == project {
		return n, ""
	}
	return n, fmt.Sprintf("⚠️ Project name normalized: %q → %q", project, n)
}

// SuggestTopicKey generates a stable topic key suggestion from type/title/content.
// It infers a topic family (e.g. architecture/*, bug/*) and then appends
// a normalized segment from title/content for stable cross-session keys.
func SuggestTopicKey(typ, title, content string) string {
	family := inferTopicFamily(typ, title, content)
	cleanTitle := stripPrivateTags(title)
	segment := normalizeTopicSegment(cleanTitle)

	if segment == "" {
		cleanContent := stripPrivateTags(content)
		words := strings.Fields(strings.ToLower(cleanContent))
		if len(words) > 8 {
			words = words[:8]
		}
		segment = normalizeTopicSegment(strings.Join(words, " "))
	}

	if segment == "" {
		segment = "general"
	}

	if strings.HasPrefix(segment, family+"-") {
		segment = strings.TrimPrefix(segment, family+"-")
	}
	if segment == "" || segment == family {
		segment = "general"
	}

	return family + "/" + segment
}

func inferTopicFamily(typ, title, content string) string {
	t := strings.TrimSpace(strings.ToLower(typ))
	switch t {
	case "architecture", "design", "adr", "refactor":
		return "architecture"
	case "bug", "bugfix", "fix", "incident", "hotfix":
		return "bug"
	case "decision":
		return "decision"
	case "pattern", "convention", "guideline":
		return "pattern"
	case "config", "setup", "infra", "infrastructure", "ci":
		return "config"
	case "discovery", "investigation", "root_cause", "root-cause":
		return "discovery"
	case "learning", "learn":
		return "learning"
	case "session_summary":
		return "session"
	}

	text := strings.ToLower(title + " " + content)
	if hasAny(text, "bug", "fix", "panic", "error", "crash", "regression", "incident", "hotfix") {
		return "bug"
	}
	if hasAny(text, "architecture", "design", "adr", "boundary", "hexagonal", "refactor") {
		return "architecture"
	}
	if hasAny(text, "decision", "tradeoff", "chose", "choose", "decide") {
		return "decision"
	}
	if hasAny(text, "pattern", "convention", "naming", "guideline") {
		return "pattern"
	}
	if hasAny(text, "config", "setup", "environment", "env", "docker", "pipeline") {
		return "config"
	}
	if hasAny(text, "discovery", "investigate", "investigation", "found", "root cause") {
		return "discovery"
	}
	if hasAny(text, "learned", "learning") {
		return "learning"
	}

	if t != "" && t != "manual" {
		return normalizeTopicSegment(t)
	}

	return "topic"
}

func hasAny(text string, words ...string) bool {
	for _, w := range words {
		if strings.Contains(text, w) {
			return true
		}
	}
	return false
}

func normalizeTopicSegment(s string) string {
	v := strings.ToLower(strings.TrimSpace(s))
	if v == "" {
		return ""
	}
	re := regexp.MustCompile(`[^a-z0-9]+`)
	latin := re.ReplaceAllString(v, " ")
	latin = strings.Join(strings.Fields(latin), "-")
	if latin != "" {
		if len([]rune(latin)) > 100 {
			latin = string([]rune(latin)[:100])
		}
		return latin
	}
	// A Chinese title has no a-z0-9 characters. Keeping only Latin produced
	// "general" for every Chinese memory, which collapsed distinct upserts into
	// one topic. Keep a bounded run of Han characters instead.
	han := make([]rune, 0, 24)
	for _, r := range v {
		if unicode.In(r, unicode.Han) {
			han = append(han, r)
			if len(han) == 24 {
				break
			}
		}
	}
	if len(han) > 0 {
		return string(han)
	}
	return latin
}

func normalizeTopicKey(topic string) string {
	v := strings.TrimSpace(strings.ToLower(topic))
	if v == "" {
		return ""
	}
	v = strings.Join(strings.Fields(v), "-")
	if len(v) > 120 {
		v = v[:120]
	}
	return v
}

func derefString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func hashNormalized(content string) string {
	normalized := strings.ToLower(strings.Join(strings.Fields(content), " "))
	h := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(h[:])
}

func dedupeWindowExpression(window time.Duration) string {
	if window <= 0 {
		window = 15 * time.Minute
	}
	minutes := int(window.Minutes())
	if minutes < 1 {
		minutes = 1
	}
	return "-" + strconv.Itoa(minutes) + " minutes"
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func normalizeSyncTargetKey(targetKey string) string {
	if strings.TrimSpace(targetKey) == "" {
		return DefaultSyncTargetKey
	}
	return strings.TrimSpace(strings.ToLower(targetKey))
}

func normalizeChunkTargetKey(targetKey string) string {
	if strings.TrimSpace(targetKey) == "" {
		return LocalChunkTargetKey
	}
	return strings.TrimSpace(strings.ToLower(targetKey))
}

func newSyncID(prefix string) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UTC().UnixNano())
	}
	return prefix + "-" + hex.EncodeToString(b)
}

func normalizeExistingSyncID(existing, prefix string) string {
	if strings.TrimSpace(existing) != "" {
		return existing
	}
	return newSyncID(prefix)
}

// privateTagRegex matches <private>...</private> tags and their contents.
// Supports multiline and nested content. Case-insensitive.
var privateTagRegex = regexp.MustCompile(`(?is)<private>.*?</private>`)

// stripPrivateTags removes all <private>...</private> content from a string.
// This ensures sensitive information (API keys, passwords, personal data)
// is never persisted to the memory database.
func stripPrivateTags(s string) string {
	result := privateTagRegex.ReplaceAllString(s, "[REDACTED]")
	// Clean up multiple consecutive [REDACTED] and excessive whitespace
	result = strings.TrimSpace(result)
	return result
}

// sanitizeFTS wraps each word in quotes so FTS5 doesn't choke on special chars.
// "fix auth bug" → `"fix" "auth" "bug"`
func sanitizeFTS(query string) string {
	return literalFTSQuery(query, " ")
}

func literalFTSQuery(query, separator string) string {
	words := strings.Fields(query)
	for i, w := range words {
		w = strings.Trim(w, `"`)
		// FTS5 escapes literal quotes by doubling them inside a phrase.
		words[i] = `"` + strings.ReplaceAll(w, `"`, `""`) + `"`
	}
	return strings.Join(words, separator)
}

// hasFTSSyntax reports constructs that are operators rather than ordinary
// user text. The default path treats them as literal terms; callers must opt
// into raw_fts to execute them.
func hasFTSSyntax(query string) bool {
	lower := strings.ToLower(query)
	if strings.ContainsAny(query, `*(){}[]^:`) {
		return true
	}
	for _, operator := range []string{"near", "and", "or", "not"} {
		for _, token := range strings.FieldsFunc(lower, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		}) {
			if token == operator {
				return true
			}
		}
	}
	return false
}

// ─── Passive Capture ─────────────────────────────────────────────────────────

// PassiveCaptureParams holds the input for passive memory capture.
type PassiveCaptureParams struct {
	SessionID string `json:"session_id"`
	Content   string `json:"content"`
	Project   string `json:"project,omitempty"`
	Source    string `json:"source,omitempty"` // e.g. "subagent-stop", "session-end"
}

// PassiveCaptureResult holds the output of passive memory capture.
type PassiveCaptureResult struct {
	Extracted  int `json:"extracted"`  // Total learnings found in text
	Saved      int `json:"saved"`      // New observations created
	Duplicates int `json:"duplicates"` // Skipped because already existed
}

// learningHeaderPattern matches section headers for learnings in both English and Spanish.
var learningHeaderPattern = regexp.MustCompile(
	`(?im)^#{2,3}\s+(?:Aprendizajes(?:\s+Clave)?|Key\s+Learnings?|Learnings?):?\s*$`,
)

const (
	minLearningLength = 20
	minLearningWords  = 4
)

// ExtractLearnings parses structured learning items from text.
// It looks for sections like "## Key Learnings:" or "## Aprendizajes Clave:"
// and extracts numbered (1. text) or bullet (- text) items.
// Returns learnings from the LAST matching section (most recent output).
func ExtractLearnings(text string) []string {
	matches := learningHeaderPattern.FindAllStringIndex(text, -1)
	if len(matches) == 0 {
		return nil
	}

	// Process sections in reverse — use first valid one (most recent)
	for i := len(matches) - 1; i >= 0; i-- {
		sectionStart := matches[i][1]
		sectionText := text[sectionStart:]

		// Cut off at next major section header
		if nextHeader := regexp.MustCompile(`\n#{1,3} `).FindStringIndex(sectionText); nextHeader != nil {
			sectionText = sectionText[:nextHeader[0]]
		}

		var learnings []string

		// Try numbered items: "1. text" or "1) text"
		numbered := regexp.MustCompile(`(?m)^\s*\d+[.)]\s+(.+)`).FindAllStringSubmatch(sectionText, -1)
		if len(numbered) > 0 {
			for _, m := range numbered {
				cleaned := cleanMarkdown(m[1])
				if len(cleaned) >= minLearningLength && len(strings.Fields(cleaned)) >= minLearningWords {
					learnings = append(learnings, cleaned)
				}
			}
		}

		// Fall back to bullet items: "- text" or "* text"
		if len(learnings) == 0 {
			bullets := regexp.MustCompile(`(?m)^\s*[-*]\s+(.+)`).FindAllStringSubmatch(sectionText, -1)
			for _, m := range bullets {
				cleaned := cleanMarkdown(m[1])
				if len(cleaned) >= minLearningLength && len(strings.Fields(cleaned)) >= minLearningWords {
					learnings = append(learnings, cleaned)
				}
			}
		}

		if len(learnings) > 0 {
			return learnings
		}
	}

	return nil
}

// cleanMarkdown strips basic markdown formatting and collapses whitespace.
func cleanMarkdown(text string) string {
	text = regexp.MustCompile(`\*\*([^*]+)\*\*`).ReplaceAllString(text, "$1") // bold
	text = regexp.MustCompile("`([^`]+)`").ReplaceAllString(text, "$1")       // inline code
	text = regexp.MustCompile(`\*([^*]+)\*`).ReplaceAllString(text, "$1")     // italic
	return strings.TrimSpace(strings.Join(strings.Fields(text), " "))
}

// PassiveCapture extracts learnings from text and saves them as observations.
// It deduplicates against existing observations using content hash matching.
func (s *Store) PassiveCapture(p PassiveCaptureParams) (*PassiveCaptureResult, error) {
	// Normalize project name before storing
	p.Project, _ = NormalizeProject(p.Project)

	result := &PassiveCaptureResult{}

	learnings := ExtractLearnings(p.Content)
	result.Extracted = len(learnings)

	if len(learnings) == 0 {
		return result, nil
	}

	for _, learning := range learnings {
		// Check if this learning already exists (by content hash) within this project
		normHash := hashNormalized(learning)
		var existingID int64
		err := s.db.QueryRow(
			`SELECT id FROM observations
			 WHERE normalized_hash = ?
			   AND ifnull(project, '') = ifnull(?, '')
			   AND deleted_at IS NULL
			 LIMIT 1`,
			normHash, nullableString(p.Project),
		).Scan(&existingID)

		if err == nil {
			// Already exists — skip
			result.Duplicates++
			continue
		}

		// Truncate for title: first 60 chars
		title := learning
		if len(title) > 60 {
			title = title[:60] + "..."
		}

		_, err = s.AddObservation(AddObservationParams{
			SessionID: p.SessionID,
			Type:      "passive",
			Title:     title,
			Content:   learning,
			Project:   p.Project,
			Scope:     "project",
			ToolName:  p.Source,
		})
		if err != nil {
			return result, fmt.Errorf("passive capture save: %w", err)
		}
		result.Saved++
	}

	return result, nil
}

// ClassifyTool returns the observation type for a given tool name.
func ClassifyTool(toolName string) string {
	switch toolName {
	case "write", "edit", "patch":
		return "file_change"
	case "bash":
		return "command"
	case "read", "view":
		return "file_read"
	case "grep", "glob", "ls":
		return "search"
	default:
		return "tool_use"
	}
}

// Now returns the current time formatted for SQLite.
func Now() string {
	return time.Now().UTC().Format("2006-01-02 15:04:05")
}

// ─── Test-accessor helpers (REQ-009 / Phase G integration tests) ──────────────

// CountRelationSyncMutations returns the number of sync_mutations rows whose
// entity is NOT 'session', 'observation', or 'prompt'. Used by integration
// tests to verify the enrollment gate: an UNENROLLED project must never enqueue
// relation sync mutations (the enqueue in JudgeBySemantic/JudgeRelation is
// guarded by an enrollment check). The test that calls this uses an unenrolled
// store, so the count must remain zero.
//
// Note: relation sync mutations ARE valid for enrolled projects (#313/#379/#383
// enabled cloud relation sync; #496 extends it with backfill). This function
// is not a blanket "relations are local-only" check — it is an enrollment-gate
// regression guard scoped to the unenrolled test context that uses it.
func (s *Store) CountRelationSyncMutations() (int, error) {
	var count int
	err := s.db.QueryRow(`
		SELECT count(*)
		FROM sync_mutations
		WHERE entity NOT IN ('session', 'observation', 'prompt')
	`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("CountRelationSyncMutations: %w", err)
	}
	return count, nil
}

// ─── Phase E: sync_apply_deferred helpers ────────────────────────────────────

// ReplayDeferredResult holds counts returned by ReplayDeferred.
type ReplayDeferredResult struct {
	Retried   int
	Succeeded int
	Failed    int
	Dead      int
}

// ReplayDeferred retries all rows in sync_apply_deferred with apply_status='deferred'
// (up to 50 per call, ordered by first_seen_at). For each row:
//   - Calls applyPulledMutationTx inside a transaction.
//   - On success: the apply itself deletes the deferred row (applyRelationUpsertTx
//     already includes DELETE FROM sync_apply_deferred on success path).
//   - On ErrRelationFKMissing: increments retry_count; if retry_count reaches 5,
//     marks apply_status='dead'. Otherwise updates last_error + last_attempted_at.
//   - On ErrApplyDead or other decode errors: marks apply_status='dead'.
//
// Dead rows are never retried. Idempotent: calling twice in one cycle does not
// double-retry because successful rows are deleted and failed rows update retry_count
// in place.
//
// Returns counts (retried, succeeded, failed, dead) for caller logging.
func (s *Store) ReplayDeferred() (result ReplayDeferredResult, err error) {
	const limit = 50
	const deadThreshold = 5

	rows, err := s.db.Query(`
		SELECT sync_id, entity, payload, retry_count
		FROM sync_apply_deferred
		WHERE apply_status = 'deferred'
		ORDER BY first_seen_at
		LIMIT ?
	`, limit)
	if err != nil {
		return result, fmt.Errorf("ReplayDeferred: list deferred: %w", err)
	}

	type deferredRow struct {
		syncID     string
		entity     string
		payload    string
		retryCount int
	}

	var pending []deferredRow
	for rows.Next() {
		var r deferredRow
		if err := rows.Scan(&r.syncID, &r.entity, &r.payload, &r.retryCount); err != nil {
			rows.Close()
			return result, fmt.Errorf("ReplayDeferred: scan: %w", err)
		}
		pending = append(pending, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return result, fmt.Errorf("ReplayDeferred: rows error: %w", err)
	}

	for _, row := range pending {
		result.Retried++
		var envelope struct {
			Project string `json:"project"`
		}
		_ = decodeSyncPayload([]byte(row.payload), &envelope)
		mut := SyncMutation{
			Entity:    row.entity,
			EntityKey: row.syncID,
			Op:        SyncOpUpsert,
			Payload:   row.payload,
			Source:    SyncSourceRemote,
			Project:   envelope.Project,
		}

		applyErr := s.withTx(func(tx *sql.Tx) error {
			return s.applyPulledMutationTx(tx, mut)
		})

		if applyErr == nil {
			// Success: applyRelationUpsertTx already deleted the deferred row.
			result.Succeeded++
			log.Printf("[store] replayDeferred: applied sync_id=%s", row.syncID)
			continue
		}

		// Classify the error and update the deferred row.
		newRetry := row.retryCount + 1
		var newStatus string
		if errors.Is(applyErr, ErrRelationFKMissing) && newRetry < deadThreshold {
			// Still retryable.
			newStatus = "deferred"
			result.Failed++
		} else {
			// Dead: either retry cap reached or non-retryable error.
			newStatus = "dead"
			result.Dead++
			log.Printf("[store] replayDeferred: marking dead sync_id=%s retry_count=%d err=%v",
				row.syncID, newRetry, applyErr)
		}

		if _, uErr := s.db.Exec(`
			UPDATE sync_apply_deferred
			SET retry_count = ?, apply_status = ?, last_error = ?, last_attempted_at = datetime('now')
			WHERE sync_id = ?
		`, newRetry, newStatus, applyErr.Error(), row.syncID); uErr != nil {
			log.Printf("[store] replayDeferred: update row sync_id=%s: %v", row.syncID, uErr)
		}
	}

	return result, nil
}

// CountDeferredAndDead returns the count of rows in sync_apply_deferred grouped
// by apply_status. Only 'deferred' and 'dead' statuses are counted; 'applied'
// rows (if any) are not included.
func (s *Store) CountDeferredAndDead() (deferred, dead int, err error) {
	rows, err := s.db.Query(`
		SELECT apply_status, count(*)
		FROM sync_apply_deferred
		WHERE apply_status IN ('deferred', 'dead')
		GROUP BY apply_status
	`)
	if err != nil {
		return 0, 0, fmt.Errorf("CountDeferredAndDead: query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return 0, 0, fmt.Errorf("CountDeferredAndDead: scan: %w", err)
		}
		switch status {
		case "deferred":
			deferred = n
		case "dead":
			dead = n
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("CountDeferredAndDead: rows error: %w", err)
	}
	return deferred, dead, nil
}

// ─── Phase 3: ListDeferred / GetDeferred ─────────────────────────────────────

// ListDeferred returns rows from sync_apply_deferred with optional status filter
// and pagination. The payload field is decoded to map[string]any; on malformed
// JSON, PayloadValid is false and PayloadRaw is preserved.
func (s *Store) ListDeferred(opts ListDeferredOptions) ([]DeferredRow, error) {
	query := `
		SELECT sync_id, entity, payload, apply_status, retry_count,
		       last_error, last_attempted_at, first_seen_at
		FROM sync_apply_deferred
		WHERE 1=1`
	var args []any

	if opts.Status != "" {
		query += ` AND apply_status = ?`
		args = append(args, opts.Status)
	}
	query += ` ORDER BY first_seen_at`
	if opts.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, opts.Limit)
	}
	if opts.Offset > 0 {
		query += ` OFFSET ?`
		args = append(args, opts.Offset)
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("ListDeferred: query: %w", err)
	}
	defer rows.Close()

	var result []DeferredRow
	for rows.Next() {
		row, err := scanDeferredRow(rows)
		if err != nil {
			return nil, fmt.Errorf("ListDeferred: scan: %w", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListDeferred: rows error: %w", err)
	}
	if result == nil {
		result = []DeferredRow{}
	}
	return result, nil
}

// GetDeferred returns a single row from sync_apply_deferred by sync_id.
// Returns an error wrapping "not found" when no row exists (matches FindCandidates style).
func (s *Store) GetDeferred(syncID string) (DeferredRow, error) {
	row := s.db.QueryRow(`
		SELECT sync_id, entity, payload, apply_status, retry_count,
		       last_error, last_attempted_at, first_seen_at
		FROM sync_apply_deferred
		WHERE sync_id = ?
	`, syncID)
	result, err := scanDeferredRow(row)
	if err == sql.ErrNoRows {
		return DeferredRow{}, fmt.Errorf("GetDeferred: deferred row %q not found", syncID)
	}
	if err != nil {
		return DeferredRow{}, fmt.Errorf("GetDeferred: %w", err)
	}
	return result, nil
}

// scannable is a common interface for *sql.Row and *sql.Rows.Scan.
type scannable interface {
	Scan(dest ...any) error
}

// scanDeferredRow scans a single sync_apply_deferred row into a DeferredRow.
// The payload is decoded to map[string]any; malformed JSON sets PayloadValid=false.
func scanDeferredRow(row scannable) (DeferredRow, error) {
	var r DeferredRow
	var rawPayload string
	if err := row.Scan(
		&r.SyncID, &r.Entity, &rawPayload, &r.ApplyStatus, &r.RetryCount,
		&r.LastError, &r.LastAttemptedAt, &r.FirstSeenAt,
	); err != nil {
		return r, err
	}
	r.PayloadRaw = rawPayload
	var decoded map[string]any
	if err := json.Unmarshal([]byte(rawPayload), &decoded); err == nil {
		r.Payload = decoded
		r.PayloadValid = true
	} else {
		r.PayloadValid = false
	}
	return r, nil
}

// ListObservationSyncPayloads returns the decoded payloads of all sync_mutations
// rows whose entity = 'observation'. Used by integration tests to assert that
// new observation columns (review_after, expires_at, embedding*) are NOT present
// in the sync wire format in Phase 1 (REQ-009).
func (s *Store) ListObservationSyncPayloads() ([]any, error) {
	rows, err := s.db.Query(`
		SELECT payload
		FROM sync_mutations
		WHERE entity = 'observation'
		ORDER BY seq ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("ListObservationSyncPayloads: query: %w", err)
	}
	defer rows.Close()

	var payloads []any
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("ListObservationSyncPayloads: scan: %w", err)
		}
		var p syncObservationPayload
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return nil, fmt.Errorf("ListObservationSyncPayloads: unmarshal: %w", err)
		}
		payloads = append(payloads, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListObservationSyncPayloads: rows error: %w", err)
	}
	return payloads, nil
}

// directoryMissing is intentionally narrow: only a definitely absent root
// bypasses the historical-candidate binding check. Permission errors, symlink
// tricks or an existing but unbound root still fail closed elsewhere.
func directoryMissing(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Lstat(path)
	return os.IsNotExist(err)
}
