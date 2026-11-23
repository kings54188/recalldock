package opsstore

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"unicode"
	"unicode/utf8"

	"recalldock.local/recalldock/internal/memorypolicy"
	"recalldock.local/recalldock/internal/worklog"
)

const SummaryLimit = 2048
const DetailLimit = 12288

var (
	ErrNotFound          = errors.New("task not found; use work_find (CLI: work find --root ABS_PATH) to select an exact root and lane")
	ErrIdentity          = errors.New("operations workspace identity differs; preserve data and reconcile explicitly")
	ErrGeneration        = worklog.ErrGeneration
	ErrOperationConflict = errors.New("operation ID already refers to different input")
	ErrBudget            = worklog.ErrBudget
	namePattern          = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,79}$`)
)

type Checkpoint struct {
	worklog.Checkpoint
	Status   string   `json:"status,omitempty"`
	Blockers []string `json:"blockers,omitempty"`
}

type Writer struct {
	Host             string `json:"host,omitempty"`
	HostSessionID    string `json:"host_session_id,omitempty"`
	GatewaySessionID string `json:"gateway_session_id,omitempty"`
}

type Task struct {
	Root       string  `json:"root"`
	Lane       string  `json:"lane"`
	ID         int64   `json:"checkpoint_id"`
	Generation int64   `json:"generation"`
	Goal       string  `json:"goal"`
	Status     string  `json:"status"`
	NeedsCheck bool    `json:"needs_check,omitempty"`
	UpdatedAt  string  `json:"updated_at"`
	Replayed   bool    `json:"replayed,omitempty"`
	Writer     *Writer `json:"writer,omitempty"`
}

type FindOptions struct {
	Query  string
	After  string
	Order  string
	Status string
	Limit  int
}

type TaskPage struct {
	Tasks   []Task `json:"tasks"`
	After   string `json:"after,omitempty"`
	Before  int64  `json:"before,omitempty"`
	HasMore bool   `json:"has_more"`
	Total   int    `json:"total"`
	Order   string `json:"order,omitempty"`
}

type GenerationConflictError struct {
	Expected int64
	Current  int64
}

func (e *GenerationConflictError) Error() string {
	return fmt.Sprintf("%s; expected=%d current=%d", ErrGeneration, e.Expected, e.Current)
}

func (e *GenerationConflictError) Unwrap() error { return ErrGeneration }

type Snapshot struct {
	Root       string     `json:"root"`
	Lane       string     `json:"lane"`
	ID         int64      `json:"checkpoint_id"`
	Generation int64      `json:"generation"`
	UpdatedAt  string     `json:"updated_at"`
	Checkpoint Checkpoint `json:"checkpoint"`
	Replayed   bool       `json:"replayed,omitempty"`
	NeedsCheck bool       `json:"needs_check,omitempty"`
	Writer     *Writer    `json:"writer,omitempty"`
}

type Event struct {
	ID               string `json:"id"`
	InputFingerprint string `json:"input_fingerprint"`
	Kind             string `json:"kind,omitempty"`
	Status           string `json:"status"`
	Summary          string `json:"summary"`
	Evidence         string `json:"evidence,omitempty"`
	Details          string `json:"details,omitempty"`
}

type Patch struct {
	Summary  *string   `json:"summary,omitempty"`
	Next     *[]string `json:"next,omitempty"`
	Status   *string   `json:"status,omitempty"`
	Blockers *[]string `json:"blockers,omitempty"`
}

type Envelope struct {
	Mode            string `json:"mode"`
	EstimatedTokens int    `json:"estimated_tokens"`
	Truncated       bool   `json:"truncated,omitempty"`
	Data            any    `json:"data"`
}

// DefaultBaseDir returns the platform-standard RecallDock user directory.
func DefaultBaseDir() (string, error) {
	if runtime.GOOS == "windows" {
		if local := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); local != "" && filepath.IsAbs(local) {
			return filepath.Join(filepath.Clean(local), "RecallDock"), nil
		}
		if cacheDir, err := os.UserCacheDir(); err == nil && filepath.IsAbs(cacheDir) {
			return filepath.Join(filepath.Clean(cacheDir), "RecallDock"), nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(filepath.Clean(home), "AppData", "Local", "RecallDock"), nil
	}
	return filepath.Join(home, ".recalldock"), nil
}

func DataDir() (string, error) {
	if dir := os.Getenv("RECALLDOCK_DATA_DIR"); dir != "" {
		if !filepath.IsAbs(dir) {
			return "", errors.New("RECALLDOCK_DATA_DIR must be absolute")
		}
		return filepath.Clean(dir), nil
	}
	return DefaultBaseDir()
}

func safeText(s string, max int) bool {
	return len(s) <= max && utf8.ValidString(s) && !memorypolicy.ContainsSecret(s) && !memorypolicy.ContainsTranscript(s) && !strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' })
}

func ValidateCheckpoint(root, lane string, cp Checkpoint) error {
	if !filepath.IsAbs(root) || len(root) > 1024 || !namePattern.MatchString(lane) {
		return errors.New("absolute root and exact safe lane required")
	}
	if err := worklog.ValidateCheckpoint(root, cp.Checkpoint); err != nil {
		return err
	}
	switch cp.Status {
	case "", "active", "blocked", "completed", "archived":
	default:
		return errors.New("invalid task status; allowed: active, blocked, completed, archived")
	}
	if len(cp.Blockers) > 5 {
		return ErrBudget
	}
	for _, b := range cp.Blockers {
		if !safeText(b, 240) {
			return errors.New("invalid blocker; use a short summary without secrets or transcripts")
		}
	}
	return validateDelivery(Snapshot{Root: root, Lane: lane, Checkpoint: cp})
}

func validateDelivery(snap Snapshot) error {
	snap.ID, snap.Generation = 9223372036854775807, 9223372036854775807
	snap.UpdatedAt = "2000-01-01T00:00:00.000000000Z"
	snap.Replayed, snap.NeedsCheck = true, true
	for _, mode := range []string{"l0", "l1", "l2"} {
		if _, err := EncodeSnapshot(snap, mode); err != nil {
			return err
		}
	}
	return nil
}

func validateEvent(e Event) error {
	if !namePattern.MatchString(e.ID) || e.InputFingerprint == "" || !safeText(e.InputFingerprint, 200) || strings.ContainsAny(e.InputFingerprint, "\r\n") || !safeText(e.Summary, 600) || !safeText(e.Evidence, 400) || !safeText(e.Details, 6000) {
		return errors.New("invalid bounded operation event")
	}
	switch e.Kind {
	case "", "progress", "attempt", "hypothesis", "blocker", "validation":
	default:
		return errors.New("invalid event kind")
	}
	switch e.Status {
	case "reported", "started", "applied", "failed", "needs_check":
	case "verified":
		if e.Evidence == "" {
			return errors.New("verified is a reported claim requiring an evidence reference")
		}
	default:
		return errors.New("invalid operation status; allowed: reported, started, applied, failed, needs_check, verified (verified requires evidence)")
	}
	return nil
}

func (s Snapshot) Task() Task {
	t := Task{Root: s.Root, Lane: s.Lane, ID: s.ID, Generation: s.Generation, Goal: clip(s.Checkpoint.Goal, 200), Status: s.Checkpoint.Status, NeedsCheck: s.NeedsCheck, UpdatedAt: s.UpdatedAt, Replayed: s.Replayed, Writer: s.Writer}
	for _, op := range s.Checkpoint.Operations {
		if op.Status == "started" || op.Status == "applied" || op.Status == "needs_check" {
			t.NeedsCheck = true
		}
	}
	return t
}

func EncodeSnapshot(s Snapshot, mode string) ([]byte, error) {
	var value any
	limit := SummaryLimit
	switch mode {
	case "l0":
		value = s.Task()
	case "l1":
		value = struct {
			Task
			Summary  string   `json:"summary"`
			Next     []string `json:"next,omitempty"`
			Blockers []string `json:"blockers,omitempty"`
		}{s.Task(), s.Checkpoint.Summary, s.Checkpoint.Next, s.Checkpoint.Blockers}
	case "l2":
		value = s
		limit = DetailLimit
	default:
		return nil, errors.New("mode must be l0, l1 or l2")
	}
	body, err := Encode(value, mode, limit)
	if errors.Is(err, ErrBudget) && mode == "l1" {
		s.Checkpoint.Summary = clip(s.Checkpoint.Summary, 500)
		s.Checkpoint.Next = nil
		s.Checkpoint.Blockers = nil
		value = struct {
			Task
			Summary    string `json:"summary"`
			DetailHint string `json:"detail_hint"`
		}{s.Task(), s.Checkpoint.Summary, "read l2 with this generation for selected details"}
		return encode(value, mode, limit, true)
	}
	return body, err
}

func Encode(value any, mode string, limit int) ([]byte, error) {
	return encode(value, mode, limit, false)
}

func encode(value any, mode string, limit int, truncated bool) ([]byte, error) {
	e := Envelope{Mode: mode, Data: value, Truncated: truncated}
	body, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	e.EstimatedTokens = (len(body) + 16 + 2) / 3
	body, err = json.Marshal(e)
	if len(body)+1 > limit {
		return nil, ErrBudget
	}
	return append(body, '\n'), err
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func digest(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
