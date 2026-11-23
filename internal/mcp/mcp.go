// Package mcp implements the Model Context Protocol server for Engram.
//
// This exposes memory tools via MCP stdio transport so ANY agent
// (OpenCode, Claude Code, Cursor, Windsurf, etc.) can use Engram's
// persistent memory just by adding it as an MCP server.
//
// Tool profiles allow agents to load only the tools they need:
//
//	recalldock mcp                    → safe agent tools (default)
//	recalldock mcp --tools=agent      → read/propose/lifecycle tools
//	recalldock mcp --tools=admin      → read-only diagnostics (no approval rights)
//	recalldock mcp --tools=agent,admin → combine profiles
//	recalldock mcp --tools=mem_save,mem_search → individual tool names
package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"recalldock.local/recalldock/internal/diagnostic"
	projectpkg "recalldock.local/recalldock/internal/project"
	"recalldock.local/recalldock/internal/store"
	"recalldock.local/recalldock/internal/timeutil"
)

const sourceProcessOverride = "process_override"

// MCPConfig holds configuration for the MCP server.
type MCPConfig struct {
	ReadBudgetID      string // trusted host config; never read from tool arguments
	ReadBudgetTokens  int
	HostOwnsLifecycle bool // lifecycle events come from the host, not model tools
	// Version is the build version advertised in the MCP initialize response.
	// It is supplied by the binary build (cmd/recalldock), never by tool
	// arguments; empty falls back to "dev" for local/source runs.
	Version string
	// DefaultProject is a trusted process-level project override supplied by
	// long-lived MCP hosts (for example, `recalldock mcp --project NAME` or
	// RECALLDOCK_PROJECT). When set, it is used before cwd detection for MCP
	// auto-resolution; per-call project arguments remain separately validated.
	DefaultProject string

	// BM25Floor overrides the default BM25 score floor used by FindCandidates
	// during conflict candidate detection (REQ-001). The floor is the minimum
	// acceptable BM25 rank (negative; closer to 0 = better match). Candidates
	// whose score falls below this threshold are excluded.
	//
	// nil means "use the store default" (-2.0). An explicit pointer value
	// (including 0.0) is forwarded directly. Using a pointer avoids the
	// zero-value ambiguity where 0.0 would otherwise be indistinguishable
	// from "not set".
	BM25Floor *float64

	// Limit overrides the maximum number of conflict candidates returned per
	// mem_save call (REQ-001). nil means "use the store default" (3).
	// An explicit pointer value (including 0) is forwarded directly.
	Limit *int
}

var suggestTopicKey = store.SuggestTopicKey

var addPromptIfMissing = func(s *store.Store, params store.AddPromptParams) (int64, bool, error) {
	return s.AddPromptIfMissing(params)
}

var loadMCPStats = func(s *store.Store) (*store.Stats, error) {
	return s.Stats()
}

func currentWorkingDirectory() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return cwd
}

// ensureImplicitSessionWithCWD backs the operator-only mutation tools
// (mem_save, passive capture, prompt capture). Those tools are denied on agent
// transports, so the implicit session is recorded as operator provenance; it is
// not a model-started session and therefore is not subject to the model
// "binding required" rule.
func ensureImplicitSessionWithCWD(s *store.Store, sessionID, project string) error {
	return s.CreateSessionWithOrigin(sessionID, project, currentWorkingDirectory(), store.SessionOriginOperator)
}

// ─── Tool Profiles ───────────────────────────────────────────────────────────
//
// "agent" — tools AI agents use during coding sessions:
//   mem_save, mem_search, mem_context, mem_session_summary,
//   mem_session_start, mem_session_end, mem_get_observation,
//   mem_suggest_topic_key, mem_capture_passive, mem_save_prompt
//
// "admin" — tools for manual curation, TUI, and dashboards:
//   mem_update, mem_delete, mem_stats, mem_timeline, mem_merge_projects
//
// "all" (default) — every tool registered.

// Profiles minimize discovery overhead. Catalog selection is never write authority.
var ProfileAgent = map[string]bool{
	"mem_commit_session": true, "mem_search": true, "mem_context": true,
	"mem_session_start": true, "mem_session_end": true, "mem_get_observation": true,
	"mem_suggest_topic_key": true, "mem_current_project": true, "mem_doctor": true, "mem_review": true,
}

// The legacy name "admin" now selects read-only diagnostics, not mutation rights.
var ProfileAdmin = map[string]bool{"mem_stats": true, "mem_timeline": true}

// Profiles maps profile names to their tool sets.
var Profiles = map[string]map[string]bool{
	"agent": ProfileAgent,
	"admin": ProfileAdmin,
}

// ResolveTools takes a comma-separated string of profile names and/or
// individual tool names and returns the set of tool names to register.
// An empty input means "all" — every tool is registered.
func ValidateToolsSelection(input string) error {
	known := map[string]bool{}
	for _, name := range []string{
		"mem_search", "mem_save", "mem_commit_session", "mem_update", "mem_review",
		"mem_suggest_topic_key", "mem_delete", "mem_save_prompt", "mem_pin", "mem_unpin",
		"mem_context", "mem_stats", "mem_timeline", "mem_get_observation", "mem_session_summary",
		"mem_session_start", "mem_session_end", "mem_capture_passive", "mem_merge_projects",
		"mem_current_project", "mem_doctor", "mem_judge", "mem_compare",
	} {
		known[name] = true
	}
	if strings.TrimSpace(input) == "" {
		return nil
	}
	for _, token := range strings.Split(input, ",") {
		token = strings.TrimSpace(token)
		if token == "all" || Profiles[token] != nil || known[token] {
			continue
		}
		return fmt.Errorf("unknown MCP tool selection %q; use agent, admin, all, or an exact tool name", token)
	}
	return nil
}

func ResolveTools(input string) map[string]bool {
	input = strings.TrimSpace(input)
	if input == "" || input == "all" {
		return nil // nil means register everything
	}

	result := make(map[string]bool)
	for _, token := range strings.Split(input, ",") {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		if token == "all" {
			return nil
		}
		if profile, ok := Profiles[token]; ok {
			for tool := range profile {
				result[tool] = true
			}
		} else {
			// Treat as individual tool name
			result[token] = true
		}
	}

	if len(result) == 0 {
		return nil
	}
	return result
}

// NewServer creates an MCP server with ALL tools registered (backwards compatible).
func NewServer(s *store.Store) *server.MCPServer {
	return NewServerWithConfig(s, MCPConfig{}, nil)
}

const serverInstructions = `RecallDock is local selective memory, not a full conversation archive.
Default: do not load or write memory unless this task needs it. Ordinary progress,
guesses, raw logs and complete conversations belong outside long-term knowledge.

READ ON DEMAND: mem_search -> a small index, then selected mem_get_observation;
mem_context is optional and bounded, not a mandatory startup/compaction ritual.
Stop when enough; activating a project does not load all its memory.

PROPOSE, DO NOT SELF-APPROVE: mem_commit_session stages bounded durable candidates.
Confidence, digests, user_correction labels and model-supplied approval are claims,
not independent evidence. Candidate admission follows the local operator policy;
only operator configuration can enable automatic admission. Repeated proposals
are deduplicated and the inbox is bounded.

Legacy direct writes (mem_save, mem_update, mem_session_summary, mem_save_prompt,
mem_capture_passive, mem_judge and destructive curation) are blocked on agent
transports. Session lifecycle may store metadata only, not raw summary bodies.
Use the bounded work checkpoint/status CLI for task progress; do not force a
new long-term observation at session end. Preserve evidence references, not logs.
A conflict is a proposal for review, not permission to silently alter old facts.`

// NewServerWithTools creates an MCP server registering only the tools in
// the allowlist. If allowlist is nil, all tools are registered.
func NewServerWithTools(s *store.Store, allowlist map[string]bool) *server.MCPServer {
	return NewServerWithConfig(s, MCPConfig{}, allowlist)
}

// NewServerWithConfig creates an MCP server with full configuration including
// default project detection and optional tool allowlist.
func NewServerWithConfig(s *store.Store, cfg MCPConfig, allowlist map[string]bool) *server.MCPServer {
	return newServerWithActivity(s, cfg, allowlist, NewSessionActivity(10*time.Minute))
}

// mcpServerVersion normalizes the advertised server version. It must reflect
// the running binary, never a model-supplied or per-call value.
func mcpServerVersion(cfg MCPConfig) string {
	version := strings.TrimSpace(cfg.Version)
	if version == "" {
		return "dev"
	}
	return version
}

func newServerWithActivity(s *store.Store, cfg MCPConfig, allowlist map[string]bool, activity *SessionActivity) *server.MCPServer {
	srv := server.NewMCPServer(
		"recalldock",
		mcpServerVersion(cfg),
		server.WithToolCapabilities(true),
		server.WithInstructions(serverInstructions),
	)

	registerTools(srv, s, cfg, allowlist, activity)
	return srv
}

// shouldRegister returns true if the tool should be registered given the
// allowlist. If allowlist is nil, everything is allowed.
func shouldRegister(name string, allowlist map[string]bool) bool {
	if allowlist == nil {
		return true
	}
	return allowlist[name]
}

func registerTools(srv *server.MCPServer, s *store.Store, cfg MCPConfig, allowlist map[string]bool, activity *SessionActivity) {
	writeQueue := newWriteQueue(defaultMCPWriteQueueSize)
	readGate := newMCPReadGate(s, cfg)
	if cfg.DefaultProject == "" && readGate.err == nil {
		cfg.DefaultProject = readGate.state.Project
	}
	addTool := func(tool mcp.Tool, handler server.ToolHandlerFunc) {
		tool.InputSchema.AdditionalProperties = false
		for name, raw := range tool.InputSchema.Properties {
			property := raw.(map[string]any)
			if property["type"] == "number" && name != "confidence" {
				property["type"] = "integer"
			}
			if property["type"] == "integer" {
				property["minimum"] = 0
				if name == "id" || name == "limit" || strings.Contains(name, "observation_id") || strings.HasPrefix(name, "memory_id_") {
					property["minimum"] = 1
				}
				if tool.Name == "mem_review" && name == "limit" {
					property["maximum"] = 20
				}
			}
		}
		wrapped := readGate.wrap(tool.Name, guardAgentTool(tool.Name, guardProjectScope(s, cfg, tool.Name, handler)))
		srv.AddTool(tool, guardToolArguments(tool.InputSchema, wrapped))
	}

	// ─── mem_search (profile: agent, core — always in context) ─────────
	if shouldRegister("mem_search", allowlist) {
		addTool(newSearchTool(), handleSearch(s, cfg, activity))
	}

	// ─── mem_save (profile: agent, core — always in context) ───────────
	if shouldRegister("mem_save", allowlist) {
		addTool(
			mcp.NewTool("mem_save",
				mcp.WithTitleAnnotation("Save Memory"),
				mcp.WithReadOnlyHintAnnotation(false),
				mcp.WithDestructiveHintAnnotation(false),
				mcp.WithIdempotentHintAnnotation(false),
				mcp.WithOpenWorldHintAnnotation(false),
				mcp.WithDescription(`Save an important observation to persistent memory. Call this PROACTIVELY after completing significant work — don't wait to be asked.

WHEN to save (call this after each of these):
- Architectural decisions or tradeoffs
- Bug fixes (what was wrong, why, how you fixed it)
- New patterns or conventions established
- Configuration changes or environment setup
- Important discoveries or gotchas
- File structure changes

FORMAT for content — use this structured format:
  **What**: [concise description of what was done]
  **Why**: [the reasoning, user request, or problem that drove it]
  **Where**: [files/paths affected, e.g. src/auth/middleware.ts, internal/store/store.go]
  **Learned**: [any gotchas, edge cases, or decisions made — omit if none]

TITLE should be short and searchable, like: "JWT auth middleware", "FTS5 query sanitization", "Fixed N+1 in user list"

Examples:
  title: "Switched from sessions to JWT"
  type: "decision"
  content: "**What**: Replaced express-session with jsonwebtoken for auth\n**Why**: Session storage doesn't scale across multiple instances\n**Where**: src/middleware/auth.ts, src/routes/login.ts\n**Learned**: Must set httpOnly and secure flags on the cookie, refresh tokens need separate rotation logic"

  title: "Fixed FTS5 syntax error on special chars"
  type: "bugfix"
  content: "**What**: Wrapped each search term in quotes before passing to FTS5 MATCH\n**Why**: Users typing queries like 'fix auth bug' would crash because FTS5 interprets special chars as operators\n**Where**: internal/store/store.go — sanitizeFTS() function\n**Learned**: FTS5 MATCH syntax is NOT the same as LIKE — always sanitize user input"`),
				mcp.WithString("title",
					mcp.Required(),
					mcp.Description("Short, searchable title (e.g. 'JWT auth middleware', 'Fixed N+1 query')"),
				),
				mcp.WithString("content",
					mcp.Description("Structured content using **What**, **Why**, **Where**, **Learned** format. Required unless observation alias is provided."),
				),
				mcp.WithString("observation",
					mcp.Description("Backward-compatible alias for content. Prefer content for new clients."),
				),
				mcp.WithString("type",
					mcp.Description("Category: decision, architecture, bugfix, pattern, config, discovery, learning (default: manual)"),
				),
				mcp.WithString("memory_class",
					mcp.Description("Stable memory class: operational, user, agent, or project. Omit to infer deterministically from type."),
				),
				mcp.WithString("memory_path",
					mcp.Description("Optional normalized virtual path rooted at memory_class, for example agent/procedures/release."),
				),
				mcp.WithString("abstract",
					mcp.Description("Optional L0 one-line summary. Falls back to title when omitted."),
				),
				mcp.WithString("overview",
					mcp.Description("Optional L1 bounded overview. Falls back to a content preview when omitted."),
				),
				mcp.WithString("session_id",
					mcp.Description("Session ID to associate with (default: manual-save-{project})"),
				),
				mcp.WithString("scope",
					mcp.Description("Scope for this observation: project (default) or personal"),
				),
				mcp.WithString("topic_key",
					mcp.Description("Optional topic identifier for upserts (e.g. architecture/auth-model). Reuses and updates the latest observation in same project+scope."),
				),
				mcp.WithString("project",
					mcp.Description("Optional explicit project for this memory. Accepted only when backed by known context (existing project, matching session, repo config, or ambiguous-project recovery); invalid or unbacked names fail loudly."),
				),
				mcp.WithString("project_choice_reason",
					mcp.Description("Must be user_selected_after_ambiguous_project, and only after the user explicitly chose one of available_projects from an ambiguous_project error."),
				),
				mcp.WithString("recovery_token",
					mcp.Description("Short-lived token returned by an ambiguous_project error. Required with project_choice_reason=user_selected_after_ambiguous_project."),
				),
				mcp.WithBoolean("capture_prompt",
					mcp.Description("Automatically capture the current user prompt when available (default: true). Set false for SDD artifacts or automated saves."),
				),
			),
			queuedWriteHandler(writeQueue, handleSave(s, cfg, activity)),
		)
	}

	// ─── mem_commit_session (profile: agent, core) ──────────────────────
	if shouldRegister("mem_commit_session", allowlist) {
		addTool(
			mcp.NewTool("mem_commit_session",
				mcp.WithDescription("Submit up to eight durable candidates under the local admission policy. The operator may enable checked automatic admission. Model approval/confidence flags do not grant authority. Returns counts and candidate IDs."),
				mcp.WithTitleAnnotation("Commit Session Memory"),
				mcp.WithReadOnlyHintAnnotation(false),
				mcp.WithDestructiveHintAnnotation(false),
				mcp.WithIdempotentHintAnnotation(true),
				mcp.WithOpenWorldHintAnnotation(false),
				mcp.WithString("session_id", mcp.Required(), mcp.Description("Existing session ID that produced the structured evidence.")),
				mcp.WithString("project", mcp.Description("Optional project; must match the existing session project.")),
				mcp.WithArray("candidates",
					mcp.Required(),
					mcp.MinItems(1),
					mcp.MaxItems(store.MaxCandidatesPerCommit),
					mcp.Items(map[string]any{
						"type":                 "object",
						"additionalProperties": false,
						"required":             []string{"category", "statement", "confidence"},
						"properties": map[string]any{
							"category": map[string]any{"type": "string", "enum": []string{
								store.CandidateCategoryUserPreference, store.CandidateCategoryUserFact,
								store.CandidateCategoryProjectConstraint, store.CandidateCategoryDecision,
								store.CandidateCategoryProcedure, store.CandidateCategoryToolBehavior,
								store.CandidateCategoryFailureAvoidance, store.CandidateCategoryVerifiedOutcome,
							}},
							"statement":          map[string]any{"type": "string", "minLength": 1, "maxLength": 2000},
							"confidence":         map[string]any{"type": "number", "minimum": 0, "maximum": 1},
							"memory_path":        map[string]any{"type": "string"},
							"proposed_topic_key": map[string]any{"type": "string"},
							"evidence_refs": map[string]any{
								"type": "array", "maxItems": 8,
								"items": map[string]any{
									"type": "object", "additionalProperties": false,
									"required": []string{"kind", "ref"},
									"properties": map[string]any{
										"kind":        map[string]any{"type": "string", "enum": []string{"test", "command", "file", "source", "observation", "user_correction"}},
										"ref":         map[string]any{"type": "string", "minLength": 1, "maxLength": 512},
										"digest":      map[string]any{"type": "string"},
										"observed_at": map[string]any{"type": "string"},
									},
								},
							},
						},
					}),
				),
			),
			queuedWriteHandler(writeQueue, handleCommitSession(s, cfg, activity)),
		)
	}

	// ─── mem_update (profile: agent, deferred) ──────────────────────────
	if shouldRegister("mem_update", allowlist) {
		addTool(
			mcp.NewTool("mem_update",
				mcp.WithDescription("Update an existing observation by ID. Only provided fields are changed."),
				mcp.WithDeferLoading(true),
				mcp.WithTitleAnnotation("Update Memory"),
				mcp.WithReadOnlyHintAnnotation(false),
				mcp.WithDestructiveHintAnnotation(false),
				mcp.WithIdempotentHintAnnotation(false),
				mcp.WithOpenWorldHintAnnotation(false),
				mcp.WithNumber("id",
					mcp.Required(),
					mcp.Description("Observation ID to update"),
				),
				mcp.WithString("title",
					mcp.Description("New title"),
				),
				mcp.WithString("content",
					mcp.Description("New content"),
				),
				mcp.WithString("type",
					mcp.Description("New type/category"),
				),
				mcp.WithString("memory_class",
					mcp.Description("New stable memory class: operational, user, agent, or project"),
				),
				mcp.WithString("memory_path",
					mcp.Description("New normalized virtual path rooted at memory_class; use an empty string to return to legacy flat memory"),
				),
				mcp.WithString("abstract",
					mcp.Description("New L0 summary; use an empty string to clear it"),
				),
				mcp.WithString("overview",
					mcp.Description("New L1 overview; use an empty string to clear it"),
				),
				mcp.WithString("scope",
					mcp.Description("New scope: project or personal"),
				),
				mcp.WithString("topic_key",
					mcp.Description("New topic key (normalized internally)"),
				),
			),
			queuedWriteHandler(writeQueue, handleUpdate(s)),
		)
	}

	// ─── mem_review (profile: agent, deferred) ──────────────────────────
	if shouldRegister("mem_review", allowlist) {
		addTool(
			mcp.NewTool("mem_review",
				mcp.WithDescription("Review lifecycle, memory/resource dependency staleness, and staged session candidates without automatic content rewrites or resource-body storage. Agent callers may use action=list only; every other action (mark_reviewed, link_*, list_dependencies, process_invalidations, link_resource, notify_resource_changed, refresh_*, candidate_*) is operator-only and will be rejected with operator_review_required — use the CLI (recalldock review / candidates) instead."),
				mcp.WithDeferLoading(true),
				mcp.WithTitleAnnotation("Review Memories"),
				mcp.WithReadOnlyHintAnnotation(false),
				mcp.WithDestructiveHintAnnotation(false),
				mcp.WithIdempotentHintAnnotation(false),
				mcp.WithOpenWorldHintAnnotation(false),
				mcp.WithString("action", mcp.Required(), mcp.Description("Action: list | mark_reviewed | link_dependency | list_dependencies | process_invalidations | link_resource | list_resource_dependencies | notify_resource_changed | refresh_resource_dependency | candidate_list | candidate_promote | candidate_reject. Only list is available to agents; the rest are operator-only."),
					mcp.Enum("list", "mark_reviewed", "link_dependency", "list_dependencies", "process_invalidations", "link_resource", "list_resource_dependencies", "notify_resource_changed", "refresh_resource_dependency", "candidate_list", "candidate_promote", "candidate_reject")),
				mcp.WithString("project", mcp.Description("Project filter for list actions; required for every resource write action.")),
				mcp.WithNumber("limit", mcp.Description("Max results for action=list (default: 10).")),
				mcp.WithNumber("observation_id", mcp.Description("Observation id for action=mark_reviewed.")),
				mcp.WithNumber("id", mcp.Description("Backward-compatible alias for observation_id.")),
				mcp.WithNumber("source_observation_id", mcp.Description("Source observation id for action=link_dependency.")),
				mcp.WithNumber("dependent_observation_id", mcp.Description("Dependent observation id for action=link_dependency.")),
				mcp.WithString("source_observation_sync_id", mcp.Description("Optional source sync id filter for action=list_dependencies.")),
				mcp.WithString("dependent_observation_sync_id", mcp.Description("Optional dependent sync id filter for action=list_dependencies.")),
				mcp.WithString("status", mcp.Description("Optional state filter: active or stale; list filters memories before applying limit, dependency actions filter dependencies."), mcp.Enum("", "active", "stale")),
				mcp.WithString("reason", mcp.Description("Evidence-backed reason for dependency, resource-change, refresh, promotion, or rejection actions.")),
				mcp.WithString("source_ref", mcp.Description("Immutable resource reference ending in @sha256:<64 hex>; used by resource actions.")),
				mcp.WithString("replacement_source_ref", mcp.Description("Optional immutable replacement for action=notify_resource_changed.")),
				mcp.WithString("resource_dependency_sync_id", mcp.Description("Resource dependency sync ID for action=refresh_resource_dependency.")),
				mcp.WithString("candidate_id", mcp.Description("Candidate sync ID for candidate_promote or candidate_reject.")),
			),
			queuedWriteHandler(writeQueue, handleReview(s, cfg)),
		)
	}

	// ─── mem_suggest_topic_key (profile: agent, deferred) ───────────────
	if shouldRegister("mem_suggest_topic_key", allowlist) {
		addTool(
			mcp.NewTool("mem_suggest_topic_key",
				mcp.WithDescription("Suggest a stable topic_key for memory upserts. Use this before mem_save when you want evolving topics (like architecture decisions) to update a single observation over time."),
				mcp.WithDeferLoading(true),
				mcp.WithTitleAnnotation("Suggest Topic Key"),
				mcp.WithReadOnlyHintAnnotation(true),
				mcp.WithDestructiveHintAnnotation(false),
				mcp.WithIdempotentHintAnnotation(true),
				mcp.WithOpenWorldHintAnnotation(false),
				mcp.WithString("type",
					mcp.Description("Observation type/category, e.g. architecture, decision, bugfix"),
				),
				mcp.WithString("title",
					mcp.Description("Observation title (preferred input for stable keys)"),
				),
				mcp.WithString("content",
					mcp.Description("Observation content used as fallback if title is empty"),
				),
			),
			handleSuggestTopicKey(),
		)
	}

	// ─── mem_delete (profile: admin, deferred) ──────────────────────────
	if shouldRegister("mem_delete", allowlist) {
		addTool(
			mcp.NewTool("mem_delete",
				mcp.WithDescription("Delete an observation by ID. Soft-delete by default; set hard_delete=true for permanent deletion."),
				mcp.WithDeferLoading(true),
				mcp.WithTitleAnnotation("Delete Memory"),
				mcp.WithReadOnlyHintAnnotation(false),
				mcp.WithDestructiveHintAnnotation(true),
				mcp.WithIdempotentHintAnnotation(false),
				mcp.WithOpenWorldHintAnnotation(false),
				mcp.WithNumber("id",
					mcp.Required(),
					mcp.Description("Observation ID to delete"),
				),
				mcp.WithBoolean("hard_delete",
					mcp.Description("If true, permanently deletes the observation"),
				),
			),
			queuedWriteHandler(writeQueue, handleDelete(s)),
		)
	}

	// ─── mem_save_prompt (profile: agent, eager) ────────────────────────
	if shouldRegister("mem_save_prompt", allowlist) {
		addTool(
			mcp.NewTool("mem_save_prompt",
				mcp.WithDescription("Save a user prompt to persistent memory. Use this to record what the user asked — their intent, questions, and requests — so future sessions have context about the user's goals."),
				mcp.WithTitleAnnotation("Save User Prompt"),
				mcp.WithReadOnlyHintAnnotation(false),
				mcp.WithDestructiveHintAnnotation(false),
				mcp.WithIdempotentHintAnnotation(false),
				mcp.WithOpenWorldHintAnnotation(false),
				mcp.WithString("content",
					mcp.Required(),
					mcp.Description("The user's prompt text"),
				),
				mcp.WithString("session_id",
					mcp.Description("Session ID to associate with (default: manual-save-{project})"),
				),
				mcp.WithString("project",
					mcp.Description("Optional recovery target only after ambiguous_project. Ignored unless project_choice_reason is user_selected_after_ambiguous_project."),
				),
				mcp.WithString("project_choice_reason",
					mcp.Description("Must be user_selected_after_ambiguous_project, and only after the user explicitly chose one of available_projects from an ambiguous_project error."),
				),
				mcp.WithString("recovery_token",
					mcp.Description("Short-lived token returned by an ambiguous_project error. Required with project_choice_reason=user_selected_after_ambiguous_project."),
				),
			),
			queuedWriteHandler(writeQueue, handleSavePrompt(s, cfg, activity)),
		)
	}

	// ─── mem_pin / mem_unpin (profile: agent, deferred) ──────────────────
	if shouldRegister("mem_pin", allowlist) {
		addTool(
			mcp.NewTool("mem_pin",
				mcp.WithDescription("Pin a local observation so it appears before recent observations in memory context. Pinned state is local to this device and is not synced."),
				mcp.WithDeferLoading(true),
				mcp.WithTitleAnnotation("Pin Memory"),
				mcp.WithReadOnlyHintAnnotation(false),
				mcp.WithDestructiveHintAnnotation(false),
				mcp.WithIdempotentHintAnnotation(true),
				mcp.WithOpenWorldHintAnnotation(false),
				mcp.WithNumber("id", mcp.Required(), mcp.Description("Observation ID to pin")),
			),
			handlePin(s, true),
		)
	}
	if shouldRegister("mem_unpin", allowlist) {
		addTool(
			mcp.NewTool("mem_unpin",
				mcp.WithDescription("Unpin a local observation so it only appears in normal recency order. Pinned state is local to this device and is not synced."),
				mcp.WithDeferLoading(true),
				mcp.WithTitleAnnotation("Unpin Memory"),
				mcp.WithReadOnlyHintAnnotation(false),
				mcp.WithDestructiveHintAnnotation(false),
				mcp.WithIdempotentHintAnnotation(true),
				mcp.WithOpenWorldHintAnnotation(false),
				mcp.WithNumber("id", mcp.Required(), mcp.Description("Observation ID to unpin")),
			),
			handlePin(s, false),
		)
	}

	// ─── mem_context (profile: agent, core — always in context) ────────
	if shouldRegister("mem_context", allowlist) {
		addTool(
			mcp.NewTool("mem_context",
				mcp.WithDescription("Get bounded memory context for the selected project. Public calls default to mode=index; use l0/l1 for selected summaries. Cross-project reads require explicit all_projects and an unpinned activation."),
				mcp.WithTitleAnnotation("Get Memory Context"),
				mcp.WithReadOnlyHintAnnotation(true),
				mcp.WithDestructiveHintAnnotation(false),
				mcp.WithIdempotentHintAnnotation(true),
				mcp.WithOpenWorldHintAnnotation(false),
				mcp.WithString("project",
					mcp.Description("Select one exact project; omission uses the current explicit activation, not all history."),
				),
				mcp.WithBoolean("all_projects", mcp.Description("Explicitly select cross-project context; cannot override a pinned project activation.")),
				mcp.WithString("scope",
					mcp.Description("Filter observations by scope: project (default) or personal"),
				),
				mcp.WithString("mode",
					mcp.Description("Context view: index (default; metadata only), l0 (abstracts), l1 (abstract plus bounded overview), or explicit full."),
					mcp.Enum("index", "l0", "l1", "full"),
				),
				mcp.WithNumber("token_budget",
					mcp.Description("Estimated-token budget for index/l0/l1. Zero uses a conservative mode default; full mode rejects this parameter."),
				),
			),
			handleContext(s, cfg, activity),
		)
	}

	// ─── mem_stats (profile: admin, deferred) ───────────────────────────
	if shouldRegister("mem_stats", allowlist) {
		addTool(
			mcp.NewTool("mem_stats",
				mcp.WithDescription("Show memory system statistics — total sessions, observations, and projects tracked."),
				mcp.WithDeferLoading(true),
				mcp.WithTitleAnnotation("Memory Stats"),
				mcp.WithReadOnlyHintAnnotation(true),
				mcp.WithDestructiveHintAnnotation(false),
				mcp.WithIdempotentHintAnnotation(true),
				mcp.WithOpenWorldHintAnnotation(false),
				mcp.WithString("project",
					mcp.Description("Filter statistics to one exact project; omission uses the current explicit activation."),
				),
				mcp.WithBoolean("all_projects", mcp.Description("Explicitly select global aggregates; unavailable on a pinned project activation.")),
			),
			handleStats(s, cfg),
		)
	}

	// ─── mem_timeline (profile: admin, deferred) ────────────────────────
	if shouldRegister("mem_timeline", allowlist) {
		addTool(
			mcp.NewTool("mem_timeline",
				mcp.WithDescription("Show chronological context around a specific observation. Use after mem_search to drill into the timeline of events surrounding a search result. This is the progressive disclosure pattern: search first, then timeline to understand context."),
				mcp.WithDeferLoading(true),
				mcp.WithTitleAnnotation("Memory Timeline"),
				mcp.WithReadOnlyHintAnnotation(true),
				mcp.WithDestructiveHintAnnotation(false),
				mcp.WithIdempotentHintAnnotation(true),
				mcp.WithOpenWorldHintAnnotation(false),
				mcp.WithNumber("observation_id",
					mcp.Required(),
					mcp.Description("The observation ID to center the timeline on (from mem_search results)"),
				),
				mcp.WithNumber("before",
					mcp.Description("Number of observations to show before the focus (default: 5)"),
				),
				mcp.WithNumber("after",
					mcp.Description("Number of observations to show after the focus (default: 5)"),
				),
				mcp.WithString("project",
					mcp.Description("Exact known project; omitted uses the trusted process, session, or directory selection"),
				),
				mcp.WithBoolean("all_projects", mcp.Description("Explicit cross-project read; unavailable in a project-pinned process")),
			),
			handleTimeline(s, cfg),
		)
	}

	// ─── mem_get_observation (profile: agent, eager) ────────────────────
	if shouldRegister("mem_get_observation", allowlist) {
		addTool(
			mcp.NewTool("mem_get_observation",
				mcp.WithDescription("Get the full content of a specific observation by ID. Use when you need the complete, untruncated content of an observation found via mem_search or mem_timeline."),
				mcp.WithTitleAnnotation("Get Observation"),
				mcp.WithReadOnlyHintAnnotation(true),
				mcp.WithDestructiveHintAnnotation(false),
				mcp.WithIdempotentHintAnnotation(true),
				mcp.WithOpenWorldHintAnnotation(false),
				mcp.WithNumber("id",
					mcp.Required(),
					mcp.Description("The observation ID to retrieve"),
				),
				mcp.WithString("detail",
					mcp.Description("Detail level: l0 (abstract), l1 (overview), or l2 (full content, default)"),
					mcp.Enum("l0", "l1", "l2"),
				),
				mcp.WithString("project", mcp.Description("Exact known project; omitted uses the trusted process, session, or directory selection")),
				mcp.WithBoolean("all_projects", mcp.Description("Explicit cross-project read; unavailable in a project-pinned process")),
			),
			handleGetObservation(s, cfg),
		)
	}

	// ─── mem_session_summary (profile: agent, core — always in context) ─
	if shouldRegister("mem_session_summary", allowlist) {
		addTool(
			mcp.NewTool("mem_session_summary",
				mcp.WithTitleAnnotation("Save Session Summary"),
				mcp.WithReadOnlyHintAnnotation(false),
				mcp.WithDestructiveHintAnnotation(false),
				mcp.WithIdempotentHintAnnotation(false),
				mcp.WithOpenWorldHintAnnotation(false),
				mcp.WithDescription(`Save a comprehensive end-of-session summary. Call this when a session is ending or when significant work is complete. This creates a structured summary that future sessions will use to understand what happened.

FORMAT — use this exact structure in the content field:

## Goal
[One sentence: what were we building/working on in this session]

## Instructions
[User preferences, constraints, or context discovered during this session. Things a future agent needs to know about HOW the user wants things done. Skip if nothing notable.]

## Discoveries
- [Technical finding, gotcha, or learning 1]
- [Technical finding 2]
- [Important API behavior, config quirk, etc.]

## Accomplished
- ✅ [Completed task 1 — with key implementation details]
- ✅ [Completed task 2 — mention files changed]
- 🔲 [Identified but not yet done — for next session]

## Next Steps
- [What remains to be done — for the next session]

## Relevant Files
- path/to/file.ts — [what it does or what changed]
- path/to/other.go — [role in the architecture]

GUIDELINES:
- Be CONCISE but don't lose important details (file paths, error messages, decisions)
- Focus on WHAT and WHY, not HOW (the code itself is in the repo)
- Include things that would save a future agent time
- The Discoveries section is the most valuable — capture gotchas and non-obvious learnings
- Relevant Files should only include files that were significantly changed or are important for context`),
				mcp.WithString("content",
					mcp.Required(),
					mcp.Description("Full session summary using the Goal/Instructions/Discoveries/Accomplished/Next Steps/Relevant Files format"),
				),
				mcp.WithString("session_id",
					mcp.Description("Session ID (default: manual-save-{project})"),
				),
				// project field intentionally omitted — auto-detect only (REQ-308 write-tool contract)
			),
			queuedWriteHandler(writeQueue, handleSessionSummary(s, cfg, activity)),
		)
	}

	// ─── mem_session_start (profile: agent, deferred) ───────────────────
	if !cfg.HostOwnsLifecycle && shouldRegister("mem_session_start", allowlist) {
		addTool(
			mcp.NewTool("mem_session_start",
				mcp.WithDescription("Register the start of a new coding session. Call this at the beginning of a session to track activity."),
				mcp.WithDeferLoading(true),
				mcp.WithTitleAnnotation("Start Session"),
				mcp.WithReadOnlyHintAnnotation(false),
				mcp.WithDestructiveHintAnnotation(false),
				mcp.WithIdempotentHintAnnotation(true),
				mcp.WithOpenWorldHintAnnotation(false),
				mcp.WithString("id",
					mcp.Required(),
					mcp.Description("Unique session identifier"),
				),
				mcp.WithString("directory",
					mcp.Description("Working directory"),
				),
			),
			queuedWriteHandler(writeQueue, handleSessionStart(s, cfg, activity)),
		)
	}

	// ─── mem_session_end (profile: agent, deferred) ─────────────────────
	if !cfg.HostOwnsLifecycle && shouldRegister("mem_session_end", allowlist) {
		addTool(
			mcp.NewTool("mem_session_end",
				mcp.WithDescription("Mark a coding session as completed. Stores lifecycle metadata only; put progress in the work ledger."),
				mcp.WithDeferLoading(true),
				mcp.WithTitleAnnotation("End Session"),
				mcp.WithReadOnlyHintAnnotation(false),
				mcp.WithDestructiveHintAnnotation(false),
				mcp.WithIdempotentHintAnnotation(true),
				mcp.WithOpenWorldHintAnnotation(false),
				mcp.WithString("id",
					mcp.Required(),
					mcp.Description("Session identifier to close"),
				),
			),
			queuedWriteHandler(writeQueue, handleSessionEnd(s, cfg, activity)),
		)
	}

	// ─── mem_capture_passive (profile: agent, deferred) ─────────────────
	if shouldRegister("mem_capture_passive", allowlist) {
		addTool(
			mcp.NewTool("mem_capture_passive",
				mcp.WithDeferLoading(true),
				mcp.WithTitleAnnotation("Capture Learnings"),
				mcp.WithReadOnlyHintAnnotation(false),
				mcp.WithDestructiveHintAnnotation(false),
				mcp.WithIdempotentHintAnnotation(true),
				mcp.WithOpenWorldHintAnnotation(false),
				mcp.WithDescription(`Extract and save structured learnings from text output. Use this at the end of a task to capture knowledge automatically.

The tool looks for sections like "## Key Learnings:" or "## Aprendizajes Clave:" and extracts numbered or bulleted items. Each item is saved as a separate observation.

Duplicates are automatically detected and skipped — safe to call multiple times with the same content.`),
				mcp.WithString("content",
					mcp.Required(),
					mcp.Description("The text output containing a '## Key Learnings:' section with numbered or bulleted items"),
				),
				mcp.WithString("session_id",
					mcp.Description("Session ID (default: manual-save-{project})"),
				),
				mcp.WithString("source",
					mcp.Description("Source identifier (e.g. 'subagent-stop', 'session-end')"),
				),
			),
			queuedWriteHandler(writeQueue, handleCapturePassive(s, cfg, activity)),
		)
	}

	// ─── mem_merge_projects (profile: admin, deferred) ──────────────────
	if shouldRegister("mem_merge_projects", allowlist) {
		addTool(
			mcp.NewTool("mem_merge_projects",
				mcp.WithDescription("Preview an explicit project mapping. Applying is destructive and requires apply=true plus the exact expected_store_id returned by mem_current_project. Fuzzy or transitive consolidation is never applied automatically."),
				mcp.WithDeferLoading(true),
				mcp.WithTitleAnnotation("Merge Projects"),
				mcp.WithReadOnlyHintAnnotation(false),
				mcp.WithDestructiveHintAnnotation(true),
				mcp.WithIdempotentHintAnnotation(true),
				mcp.WithOpenWorldHintAnnotation(false),
				mcp.WithString("from",
					mcp.Required(),
					mcp.Description("Comma-separated list of project names to merge FROM (e.g. 'Engram,engram-memory,ENGRAM')"),
				),
				mcp.WithString("to",
					mcp.Required(),
					mcp.Description("The canonical project name to merge INTO (e.g. 'engram')"),
				),
				mcp.WithString("expected_store_id",
					mcp.Description("Exact store_id from mem_current_project; required when apply=true"),
				),
				mcp.WithBoolean("apply",
					mcp.Description("Apply the explicit mapping. Defaults to false (dry-run)."),
				),
			),
			queuedWriteHandler(writeQueue, handleMergeProjects(s)),
		)
	}

	// ─── mem_current_project (profile: agent) ────────────────────────────
	if shouldRegister("mem_current_project", allowlist) {
		addTool(
			mcp.NewTool("mem_current_project",
				mcp.WithDescription("Detect the current project from the cwd or the session default established by mem_session_start(directory=...). Returns project/source/path/alternatives. If unbound it returns project_not_selected instead of an empty success; cross-project reads can use all_projects=true where supported."),
				mcp.WithTitleAnnotation("Detect Current Project"),
				mcp.WithReadOnlyHintAnnotation(true),
				mcp.WithDestructiveHintAnnotation(false),
				mcp.WithIdempotentHintAnnotation(true),
				mcp.WithOpenWorldHintAnnotation(false),
			),
			handleCurrentProject(s, cfg),
		)
	}

	// ─── mem_doctor (profile: agent, deferred) ──────────────────────────
	if shouldRegister("mem_doctor", allowlist) {
		addTool(
			mcp.NewTool("mem_doctor",
				mcp.WithDescription("Run read-only operational diagnostics. Returns the same structured envelope as `recalldock doctor --json`."),
				mcp.WithDeferLoading(true),
				mcp.WithTitleAnnotation("Run RecallDock Doctor"),
				mcp.WithReadOnlyHintAnnotation(true),
				mcp.WithDestructiveHintAnnotation(false),
				mcp.WithIdempotentHintAnnotation(true),
				mcp.WithOpenWorldHintAnnotation(false),
				mcp.WithString("project", mcp.Description("Project to diagnose (omit for auto-detect)")),
				mcp.WithString("check", mcp.Description("Optional diagnostic check code to run")),
			),
			handleDoctor(s, cfg),
		)
	}

	// ─── mem_judge (profile: agent, eager) — REQ-003, Design §6 ─────────
	if shouldRegister("mem_judge", allowlist) {
		addTool(
			mcp.NewTool("mem_judge",
				mcp.WithDescription(`Record a verdict on a pending memory conflict surfaced by mem_save.

WHEN TO CALL: After mem_save returns judgment_required=true, iterate candidates[] and call mem_judge once per entry using that entry's judgment_id.

PARAMS:
  judgment_id (required) — from candidates[].judgment_id in the mem_save response
  relation    (required) — one of: related, compatible, scoped, conflicts_with, supersedes, not_conflict
  reason      (optional) — free-text explanation of the verdict
  evidence    (optional) — JSON or text supporting the verdict
  confidence  (optional) — float 0..1; default 1.0

WHEN TO ASK USER:
  - confidence < 0.7: surface to user before judging
  - relation in {supersedes, conflicts_with} AND type in {architecture, policy, decision}: ask user
  - otherwise: resolve silently

SUCCESS: Returns the updated relation row with judgment_status="judged".
ERROR: Returns IsError=true if judgment_id is unknown or relation verb is invalid. Row is NOT mutated on error.

Re-judging an already-judged ID overwrites the verdict (deliberate revision).`),
				mcp.WithTitleAnnotation("Judge Memory Conflict"),
				mcp.WithReadOnlyHintAnnotation(false),
				mcp.WithDestructiveHintAnnotation(false),
				mcp.WithIdempotentHintAnnotation(false),
				mcp.WithOpenWorldHintAnnotation(false),
				mcp.WithString("judgment_id",
					mcp.Required(),
					mcp.Description("The judgment_id from candidates[] in the mem_save response (format: rel-<hex>)"),
				),
				mcp.WithString("relation",
					mcp.Required(),
					mcp.Description("Verdict: related | compatible | scoped | conflicts_with | supersedes | not_conflict"),
				),
				mcp.WithString("reason",
					mcp.Description("Free-text explanation of the verdict"),
				),
				mcp.WithString("evidence",
					mcp.Description("Supporting evidence (JSON or free text)"),
				),
				mcp.WithNumber("confidence",
					mcp.Description("Confidence score 0.0..1.0 (default: 1.0)"),
				),
				mcp.WithString("session_id",
					mcp.Description("Session ID for provenance (default: auto)"),
				),
			),
			queuedWriteHandler(writeQueue, handleJudge(s, activity)),
		)
	}

	// ─── mem_compare (profile: agent, eager) — REQ-011, Design §9 ────────
	if shouldRegister("mem_compare", allowlist) {
		addTool(
			mcp.NewTool("mem_compare",
				mcp.WithDescription(`Persist a semantic verdict you have already judged externally (with your LLM) into Engram.

WHEN TO CALL: After you have evaluated two memories and reached a verdict, call mem_compare to PERSIST that verdict into the relation store. You do the judgment; mem_compare records it.

PARAMS:
  memory_id_a  (required) — integer id of the first observation (from mem_search or mem_get_observation)
  memory_id_b  (required) — integer id of the second observation
  relation     (required) — one of: related, compatible, scoped, conflicts_with, supersedes, not_conflict
  confidence   (required) — float 0..1; your self-reported confidence in the verdict
  reasoning    (required) — explanation of the verdict, max 200 chars
  model        (optional) — your model identifier, stored for provenance (e.g. "claude-haiku-4-5")

BEHAVIOR:
  - Persists the verdict via JudgeBySemantic with system provenance (marked_by_actor="engram").
  - not_conflict: no row is inserted; tool returns success with empty sync_id (the verdict is recorded but not stored — it means "we evaluated these and they do not conflict").
  - Idempotent: calling again for the same pair updates the existing row.
  - Cross-project pairs are rejected.

SUCCESS: Returns { "sync_id": "rel-..." } on persist, { "sync_id": "" } on not_conflict.
ERROR: Returns IsError=true if IDs are unknown, relation is invalid, or cross-project pair.`),
				mcp.WithTitleAnnotation("Compare Memory Pair (Persist Semantic Verdict)"),
				mcp.WithReadOnlyHintAnnotation(false),
				mcp.WithDestructiveHintAnnotation(false),
				mcp.WithIdempotentHintAnnotation(true),
				mcp.WithOpenWorldHintAnnotation(false),
				mcp.WithNumber("memory_id_a",
					mcp.Required(),
					mcp.Description("Integer id of the first observation (from mem_search #id)"),
				),
				mcp.WithNumber("memory_id_b",
					mcp.Required(),
					mcp.Description("Integer id of the second observation (from mem_search #id)"),
				),
				mcp.WithString("relation",
					mcp.Required(),
					mcp.Description("Verdict: related | compatible | scoped | conflicts_with | supersedes | not_conflict"),
				),
				mcp.WithNumber("confidence",
					mcp.Required(),
					mcp.Description("Confidence score 0.0..1.0"),
				),
				mcp.WithString("reasoning",
					mcp.Required(),
					mcp.Description("Brief explanation of the verdict (max 200 chars)"),
				),
				mcp.WithString("model",
					mcp.Description("Your model identifier for provenance (e.g. \"claude-haiku-4-5\")"),
				),
			),
			handleCompare(s, activity),
		)
	}
	readGate.catalog(srv)
}

// ─── Tool Handlers ───────────────────────────────────────────────────────────

// handleCurrentProject implements mem_current_project. It NEVER returns an error
// even on ambiguous cwd — it always returns a success result with whatever
// detection info is available (REQ-313).
func handleCurrentProject(s *store.Store, cfg MCPConfig) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		cwd, _ := os.Getwd()
		res := s.DetectProject(cwd)
		if processRes, ok := processProjectResult(cfg.DefaultProject); ok {
			res = processRes
		} else if def, ok := sessionProjectDefaultFor(s); ok && res.Project == "" {
			res = projectpkg.DetectionResult{Project: def.project, Source: projectpkg.SourceSessionProject, Path: def.path}
		}
		if res.Error != nil && len(res.AvailableProjects) == 0 {
			return errorWithMeta("project_binding_invalid", res.Error.Error(), res.AvailableProjects), nil
		}
		if strings.TrimSpace(res.Project) == "" && len(res.AvailableProjects) == 0 {
			return errorWithMeta("project_not_selected", "No explicit project is selected. Bind this directory, select an exact historical project, or pass all_projects=true for a read that supports cross-project selection; cwd/git names do not activate history.", res.AvailableProjects), nil
		}

		envelope := map[string]any{
			"project":        res.Project,
			"project_source": res.Source,
			"project_path":   res.Path,
			"project_id":     res.ProjectID, "project_name": res.ProjectName, "binding_id": res.BindingID,
			"cwd":                cwd,
			"available_projects": res.AvailableProjects,
		}
		if identity, err := s.Identity(); err == nil {
			envelope["store_id"] = identity.StoreID
			envelope["data_dir"] = identity.DataDir
			envelope["database_path"] = identity.DatabasePath
		} else {
			envelope["store_identity_error"] = err.Error()
		}
		if res.Warning != "" {
			envelope["warning"] = res.Warning
		}
		if res.Error != nil {
			// REQ-313: not an error response — just surface the info.
			envelope["error_hint"] = res.Error.Error()
		}
		out, _ := jsonMarshal(envelope)
		return mcp.NewToolResultText(string(out)), nil
	}
}

func handleSearch(s *store.Store, cfg MCPConfig, activity *SessionActivity) server.ToolHandlerFunc {
	schema := newSearchTool().InputSchema
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		if err := validateToolArguments(args, schema); err != nil {
			return errorWithMeta("invalid_arguments", err.Error(), nil), nil
		}
		req.Params.Arguments = args
		query, _ := req.GetArguments()["query"].(string)
		typ, _ := req.GetArguments()["type"].(string)
		projectOverride, _ := req.GetArguments()["project"].(string)
		scope, _ := req.GetArguments()["scope"].(string)
		matchMode, _ := req.GetArguments()["match_mode"].(string)
		profile, _ := req.GetArguments()["profile"].(string)
		strategy, _ := req.GetArguments()["strategy"].(string)
		detail, _ := req.GetArguments()["detail"].(string)
		rootPath, _ := req.GetArguments()["root_path"].(string)
		allProjects := boolArg(req, "all_projects", false)
		rawFTS := boolArg(req, "raw_fts", false)
		limit := intArg(req, "limit", 10)
		limitRequested := limit
		limitClamped := false
		if rawLimit, present := req.GetArguments()["limit"]; present {
			if n, ok := rawLimit.(float64); ok && n < 1 {
				return errorWithMeta("invalid_limit", "limit must be at least 1", nil), nil
			}
			if limit > 20 {
				limit, limitClamped = 20, true
			}
		}
		tokenBudget := intArg(req, "token_budget", 0)
		maxDepth := intArg(req, "max_depth", 0)
		traceRequested := boolArg(req, "trace", false)
		explainEmpty := boolArg(req, "explain_empty", false)
		includeStale := boolArg(req, "include_stale", false)

		if strings.TrimSpace(query) == "" {
			return errorWithMeta("query_required", "query must be a non-empty search string", nil), nil
		}
		// all_projects=true short-circuits project resolution: we search globally
		// regardless of the project override or any auto-detected project. This
		// keeps the cross-project flow independent of cwd-based detection so the
		// agent can recall context from any project without knowing its key.
		var detRes projectpkg.DetectionResult
		var project string
		if allProjects {
			detRes = projectpkg.DetectionResult{Source: projectpkg.SourceAllProjects}
		} else {
			// Resolve project: validate override or auto-detect (REQ-310, REQ-311)
			res, err := resolveReadProjectWithProcessOverride(s, projectOverride, cfg.DefaultProject)
			if err != nil {
				var upe *unknownProjectError
				if errors.As(err, &upe) {
					return errorWithMeta("unknown_project",
						fmt.Sprintf("Project %q not found in store", upe.Name),
						upe.AvailableProjects,
					), nil
				}
				return mcp.NewToolResultError(fmt.Sprintf("Project resolution failed: %s", err)), nil
			}
			detRes = res
			project = detRes.Project
			project, _ = store.NormalizeProject(project)
			detRes.Project = project // JR2-1: keep envelope in sync with normalized query project
		}

		// Scope classifies memory; all_projects is the explicit wider selector.
		searchProject := project

		sessionID := defaultSessionID(project)
		activity.RecordToolCall(sessionID)

		searchResponse, err := s.SearchDetailed(query, store.SearchOptions{
			Type: typ, Project: searchProject, Scope: scope, Limit: limit,
			MatchMode: matchMode, RawFTS: rawFTS, Profile: profile, Strategy: strategy, Detail: detail,
			TokenBudget: tokenBudget, RootPath: rootPath, MaxDepth: maxDepth,
			Trace: traceRequested, ExplainEmpty: explainEmpty, IncludeStale: includeStale, DeferRecallTracking: true,
		})
		if err != nil {
			switch {
			case errors.Is(err, store.ErrInvalidType):
				return errorWithMeta("invalid_type", err.Error(), nil), nil
			case errors.Is(err, store.ErrInvalidSearchOptions):
				return errorWithMeta("invalid_arguments", err.Error(), nil), nil
			case errors.Is(err, store.ErrInvalidQuery):
				return errorWithMeta("invalid_query", err.Error(), nil), nil
			case errors.Is(err, store.ErrQueryHasNoTerms):
				return errorWithMeta("query_has_no_terms", err.Error(), nil), nil
			case errors.Is(err, store.ErrQueryRequired):
				return errorWithMeta("query_required", err.Error(), nil), nil
			case errors.Is(err, store.ErrBudgetTooSmall):
				return errorWithMeta("budget_too_small", err.Error(), nil), nil
			default:
				log.Printf("[mcp] search failed: %v", err)
				return errorWithMeta("search_failed", "Search failed; inspect local server diagnostics before retrying.", nil), nil
			}
		}
		results := searchResponse.Results
		retrievalMeta := map[string]any{
			"strategy_used":    searchResponse.StrategyUsed,
			"latency_ms":       searchResponse.LatencyMS,
			"estimated_tokens": searchResponse.Budget.Used,
			"estimator":        searchResponse.Budget.Estimator,
			"budget_used":      searchResponse.Budget.Used,
			"token_budget":     searchResponse.Budget.Limit,
			"truncated":        searchResponse.Budget.Truncated,
			"recall_summary":   searchResponse.RecallSummary,
		}
		if searchResponse.FallbackReason != "" {
			retrievalMeta["fallback_reason"] = searchResponse.FallbackReason
		}
		if searchResponse.EmptyReason != "" {
			retrievalMeta["empty_reason"] = searchResponse.EmptyReason
		}
		if searchResponse.DidYouMean != nil {
			retrievalMeta["did_you_mean"] = searchResponse.DidYouMean
		}
		if searchResponse.EmptyExplanation != nil {
			retrievalMeta["empty_explanation"] = searchResponse.EmptyExplanation
		}
		if len(searchResponse.QueryGroups) > 0 {
			retrievalMeta["query_groups"] = searchResponse.QueryGroups
		}
		if len(searchResponse.QueryTerms) > 0 {
			retrievalMeta["query_terms"] = searchResponse.QueryTerms
		}
		if len(searchResponse.IgnoredTerms) > 0 {
			retrievalMeta["ignored_terms"] = searchResponse.IgnoredTerms
		}
		if limitClamped {
			retrievalMeta["limit_clamped"] = map[string]any{"requested": limitRequested, "applied": limit}
		}
		if explainEmpty && len(results) > 0 {
			retrievalMeta["explain_empty_note"] = "explain_empty only adds empty_reason when the result is empty"
		}
		if traceRequested || explainEmpty {
			retrievalMeta["trace"] = searchResponse.Trace
		}

		if len(results) == 0 {
			// JW4: use respondWithProject even for empty results.
			retrievalMeta["results"] = []map[string]any{}
			return respondWithProject(s, detRes, fmt.Sprintf("No memories found for: %q", store.QueryPreview(query)), retrievalMeta), nil
		}

		// Batch-load relations for all results (REQ-002). Avoids N+1.
		syncIDs := make([]string, 0, len(results))
		for _, r := range results {
			if r.SyncID != "" {
				syncIDs = append(syncIDs, r.SyncID)
			}
		}
		relationsMap := map[string]store.ObservationRelations{}
		if len(syncIDs) > 0 {
			if rm, relErr := s.GetRelationsForObservations(syncIDs); relErr == nil {
				relationsMap = rm
			}
			// Errors from relation loading are swallowed — search must not fail.
		}

		var b strings.Builder
		fmt.Fprintf(&b, "Found %d memories:\n\n", len(results))
		anyTruncated := false
		structuredResults := make([]map[string]any, 0, len(results))
		for i, r := range results {
			projectDisplay := ""
			if r.Project != nil {
				projectDisplay = fmt.Sprintf(" | project: %s", *r.Project)
			}
			packedContent := r.SelectedContent
			if packedContent == "" {
				packedContent = r.Content
			}
			preview := truncate(packedContent, 300)
			if len(packedContent) > 300 {
				anyTruncated = true
				preview += " [preview]"
			}
			stateDisplay := ""
			if r.State() == store.ObservationStateNeedsReview {
				stateDisplay = " | state: needs_review"
			}
			fmt.Fprintf(&b, "[%d] #%d (%s) — %s\n    %s\n    %s%s | scope: %s%s\n",
				i+1, r.ID, r.Type, r.Title,
				preview,
				timeutil.FormatLocal(r.CreatedAt), projectDisplay, r.Scope, stateDisplay)
			entry := map[string]any{
				"id":               r.ID,
				"sync_id":          r.SyncID,
				"title":            r.Title,
				"type":             r.Type,
				"memory_class":     r.MemoryClass,
				"memory_path":      r.MemoryPath,
				"abstract":         r.EffectiveAbstract(),
				"summary_state":    r.SummaryState,
				"summary_revision": r.SummaryRevision,
				"selected_detail":  r.SelectedDetail,
				"selected_content": r.SelectedContent,
				"estimated_tokens": r.EstimatedTokens,
				"score":            r.Score,
				"score_source":     r.ScoreSource,
				"selection_reason": r.SelectionReason,
				"state":            r.State(),
				"scope":            r.Scope,
				"pinned":           r.Pinned,
			}
			if r.RawBM25 != nil {
				entry["raw_bm25"] = *r.RawBM25
			}
			if r.DetailDowngraded != nil {
				entry["detail_downgraded"] = r.DetailDowngraded
			}
			if r.Project != nil {
				entry["project"] = *r.Project
			}
			if r.ReviewAfter != nil {
				entry["review_after"] = *r.ReviewAfter
			}
			if r.StaleAt != nil {
				entry["stale_at"] = *r.StaleAt
			}
			if r.StaleReason != nil {
				entry["stale_reason"] = *r.StaleReason
			}
			structuredResults = append(structuredResults, entry)

			// Append relation annotations. Skip orphaned (filtered by store).
			//
			// Annotation format contract (REQ-012, Design §7):
			//   supersedes: #<id> (<title>)            judged supersedes
			//   superseded_by: #<id> (<title>)         judged superseded_by
			//   conflicts: #<id> (<title>)             judged conflicts_with
			//   conflict: contested by #<id> (pending) pending (UNCHANGED from Phase 1)
			//
			// <id> is the observation's integer primary key. <title> is the related
			// observation's title; "(deleted)" when the observation is missing or soft-deleted.
			// Prefixes (supersedes:, superseded_by:, conflicts:) are stable across Phase 3.
			if rels, ok := relationsMap[r.SyncID]; ok {
				for _, rel := range rels.AsSource {
					switch {
					case rel.Relation == store.RelationSupersedes && rel.JudgmentStatus == store.JudgmentStatusJudged:
						title := rel.TargetTitle
						if rel.TargetMissing || title == "" {
							title = "deleted"
						}
						fmt.Fprintf(&b, "    supersedes: #%d (%s)\n", rel.TargetIntID, title)
					case rel.Relation == store.RelationConflictsWith && rel.JudgmentStatus == store.JudgmentStatusJudged:
						title := rel.TargetTitle
						if rel.TargetMissing || title == "" {
							title = "deleted"
						}
						fmt.Fprintf(&b, "    conflicts: #%d (%s)\n", rel.TargetIntID, title)
					case rel.JudgmentStatus == store.JudgmentStatusPending:
						// UNCHANGED from Phase 1 — byte-for-byte preserved.
						fmt.Fprintf(&b, "    conflict: contested by #%s (pending)\n", rel.TargetID)
					}
				}
				for _, rel := range rels.AsTarget {
					switch {
					case rel.Relation == store.RelationSupersedes && rel.JudgmentStatus == store.JudgmentStatusJudged:
						title := rel.SourceTitle
						if rel.SourceMissing || title == "" {
							title = "deleted"
						}
						fmt.Fprintf(&b, "    superseded_by: #%d (%s)\n", rel.SourceIntID, title)
					case rel.JudgmentStatus == store.JudgmentStatusPending:
						// UNCHANGED from Phase 1 — byte-for-byte preserved.
						fmt.Fprintf(&b, "    conflict: contested by #%s (pending)\n", rel.SourceID)
					}
				}
			}
			b.WriteString("\n")
		}
		if anyTruncated {
			fmt.Fprintf(&b, "---\nResults above are previews (300 chars). To read the full content of a specific memory, call mem_get_observation(id: <ID>).\n")
		}

		if nudge := activity.NudgeIfNeeded(sessionID); nudge != "" {
			b.WriteString(nudge)
		}

		// JW4: use respondWithProject for the success path (REQ-314).
		retrievalMeta["results"] = structuredResults
		return respondWithProject(s, detRes, b.String(), retrievalMeta), nil
	}
}

func handlePin(s *store.Store, pinned bool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := int64(intArg(req, "id", 0))
		if id == 0 {
			return mcp.NewToolResultError("id is required"), nil
		}

		var err error
		if pinned {
			err = s.PinObservation(id)
		} else {
			err = s.UnpinObservation(id)
		}
		if err != nil {
			return mcp.NewToolResultError("Failed to update pin state: " + err.Error()), nil
		}

		obs, err := s.GetObservation(id)
		if err != nil {
			return mcp.NewToolResultError("Updated pin state but failed to reload observation: " + err.Error()), nil
		}
		state := "unpinned"
		if pinned {
			state = "pinned"
		}
		out, _ := jsonMarshal(map[string]any{
			"result":  fmt.Sprintf("Memory #%d %s", id, state),
			"id":      obs.ID,
			"sync_id": obs.SyncID,
			"pinned":  obs.Pinned,
		})
		return mcp.NewToolResultText(string(out)), nil
	}
}

func handleSave(s *store.Store, cfg MCPConfig, activity *SessionActivity) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		title, _ := req.GetArguments()["title"].(string)
		content, _ := req.GetArguments()["content"].(string)
		if strings.TrimSpace(content) == "" {
			if observation, _ := req.GetArguments()["observation"].(string); strings.TrimSpace(observation) != "" {
				content = observation
			}
		}
		if strings.TrimSpace(content) == "" {
			return mcp.NewToolResultError("content is required for mem_save (use content, or observation for backward-compatible clients)"), nil
		}
		typ, _ := req.GetArguments()["type"].(string)
		memoryClass, _ := req.GetArguments()["memory_class"].(string)
		memoryPath, _ := req.GetArguments()["memory_path"].(string)
		abstract, _ := req.GetArguments()["abstract"].(string)
		overview, _ := req.GetArguments()["overview"].(string)
		sessionID, _ := req.GetArguments()["session_id"].(string)
		scope, _ := req.GetArguments()["scope"].(string)
		topicKey, _ := req.GetArguments()["topic_key"].(string)
		projectChoice, _ := req.GetArguments()["project"].(string)
		_, explicitProjectProvided := req.GetArguments()["project"]
		projectChoiceReason, _ := req.GetArguments()["project_choice_reason"].(string)
		recoveryToken, _ := req.GetArguments()["recovery_token"].(string)
		capturePrompt := boolArg(req, "capture_prompt", true)
		recoverySessionID := sessionID
		if strings.TrimSpace(recoverySessionID) == "" {
			recoverySessionID = defaultSessionID("")
		}
		validateRecoveryToken := func(res projectpkg.DetectionResult, choice string) (bool, bool) {
			if strings.TrimSpace(recoveryToken) == "" {
				return false, false
			}
			return true, activity.ValidateAmbiguousProjectRecoveryToken(recoverySessionID, recoveryToken, strings.TrimSpace(choice), res.AvailableProjects, res.Path)
		}

		// Resolve write project using the full MCP precedence: explicit request,
		// existing session association, process override, repo config/directory detection, then cwd fallback.
		detRes, err := resolveSaveWriteProjectWithProcessOverride(s, projectChoice, explicitProjectProvided, projectChoiceReason, sessionID, validateRecoveryToken, cfg.DefaultProject)
		if err != nil {
			return writeProjectErrorResult(activity, recoverySessionID, detRes, err), nil
		}
		project := detRes.Project

		// Normalize project name and capture warning
		normalized, normWarning := store.NormalizeProject(project)
		project = normalized

		if typ == "" {
			typ = "manual"
		}
		if sessionID == "" {
			sessionID = resolveFallbackSessionID(s, project)
		}
		suggestedTopicKey := suggestTopicKey(typ, title, content)

		// Check for similar existing projects (only when this project has no existing observations)
		var similarWarning string
		if project != "" {
			existingNames, _ := s.ListProjectNames()
			isNew := true
			for _, e := range existingNames {
				if e == project {
					isNew = false
					break
				}
			}
			if isNew && len(existingNames) > 0 {
				matches := projectpkg.FindSimilar(project, existingNames, 3)
				if len(matches) > 0 {
					bestMatch := matches[0].Name
					obsCount, _ := s.CountObservationsForProject(bestMatch)
					similarWarning = fmt.Sprintf("⚠️ Project %q has no memories. Similar project found: %q (%d memories). Consider using that name instead.", project, bestMatch, obsCount)
				}
			}
		}

		// Ensure the implicit MCP session exists with the current working directory.
		_ = ensureImplicitSessionWithCWD(s, sessionID, project)

		truncated := len(content) > s.MaxObservationLength()

		savedID, err := s.AddObservation(store.AddObservationParams{
			SessionID:   sessionID,
			Type:        typ,
			Title:       title,
			Content:     content,
			Project:     project,
			Scope:       scope,
			TopicKey:    topicKey,
			MemoryClass: memoryClass,
			MemoryPath:  memoryPath,
			Abstract:    abstract,
			Overview:    overview,
		})
		if err != nil {
			return mcp.NewToolResultError("Failed to save: " + err.Error()), nil
		}

		if capturePrompt && activity != nil {
			if prompt, ok := activity.CurrentPrompt(sessionID, project); ok {
				if _, _, promptErr := addPromptIfMissing(s, store.AddPromptParams{
					SessionID: sessionID,
					Content:   prompt,
					Project:   project,
				}); promptErr != nil {
					fmt.Fprintf(os.Stderr, "recalldock: auto prompt capture error (non-fatal): %v\n", promptErr)
				}
			}
		}

		if activity != nil {
			activity.RecordSave(sessionID)
		}

		msg := fmt.Sprintf("Memory saved: %q (%s)", title, typ)
		if topicKey == "" && suggestedTopicKey != "" {
			msg += fmt.Sprintf("\nSuggested topic_key: %s", suggestedTopicKey)
		}
		if truncated {
			msg += fmt.Sprintf("\n⚠ WARNING: Content was truncated from %d to %d chars. Consider splitting into smaller observations.", len(content), s.MaxObservationLength())
		}
		if normWarning != "" {
			msg += "\n" + normWarning
		}
		if similarWarning != "" {
			msg += "\n" + similarWarning
		}

		// Post-transaction conflict candidate detection (REQ-001).
		// Errors are logged and swallowed — detection failure never fails the save.
		extra := map[string]any{}
		// Build CandidateOptions, forwarding any MCPConfig overrides.
		// nil fields mean "use store defaults"; explicit pointer values override.
		candOpts := store.CandidateOptions{
			Project:   project,
			Scope:     scope,
			BM25Floor: cfg.BM25Floor, // nil → store default (-2.0); explicit value overrides
		}
		if cfg.Limit != nil {
			candOpts.Limit = *cfg.Limit
		}
		candidates, candErr := s.FindCandidates(savedID, candOpts)
		if candErr != nil {
			// Log only — do not fail the save.
			fmt.Fprintf(os.Stderr, "recalldock: FindCandidates error (non-fatal): %v\n", candErr)
		}

		// Fetch the saved observation's sync_id for the envelope (REQ-001).
		var savedSyncID string
		if obs, obsErr := s.GetObservation(savedID); obsErr == nil {
			savedSyncID = obs.SyncID
			extra["id"] = savedID
			extra["sync_id"] = savedSyncID
			extra["memory_class"] = obs.MemoryClass
			extra["memory_path"] = obs.MemoryPath
			extra["summary_state"] = obs.SummaryState
			extra["summary_revision"] = obs.SummaryRevision
			extra["state"] = obs.State()
			if obs.ReviewAfter != nil {
				extra["review_after"] = *obs.ReviewAfter
			}
		}

		if len(candidates) > 0 {
			extra["judgment_required"] = true
			extra["judgment_status"] = "pending"
			extra["judgment_id"] = candidates[0].JudgmentID // first candidate's rel sync_id (design convenience)

			candList := make([]map[string]any, 0, len(candidates))
			for _, c := range candidates {
				entry := map[string]any{
					"id":          c.ID,
					"sync_id":     c.SyncID,
					"title":       c.Title,
					"type":        c.Type,
					"score":       c.Score,
					"judgment_id": c.JudgmentID,
				}
				if c.TopicKey != nil {
					entry["topic_key"] = *c.TopicKey
				}
				candList = append(candList, entry)
			}
			extra["candidates"] = candList

			msg += fmt.Sprintf("\nCONFLICT REVIEW PENDING — %d candidate(s); use mem_judge to record verdicts.", len(candidates))
		} else {
			extra["judgment_required"] = false
		}

		// Update detRes to reflect normalized project for envelope accuracy
		detRes.Project = project
		return respondWithProject(s, detRes, msg, extra), nil
	}
}

func handleCommitSession(s *store.Store, cfg MCPConfig, activity *SessionActivity) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := ctx.Err(); err != nil {
			return mcp.NewToolResultError("Session commit cancelled: " + err.Error()), nil
		}
		sessionID, _ := req.GetArguments()["session_id"].(string)
		project, _ := req.GetArguments()["project"].(string)
		if cfg.HostOwnsLifecycle {
			session, err := s.GetSession(strings.TrimSpace(sessionID))
			if errors.Is(err, sql.ErrNoRows) {
				return errorWithMeta("session_not_started", "Host session registration has not completed.", nil), nil
			}
			if err != nil || session.EndedAt != nil || session.Project != cfg.DefaultProject || (strings.TrimSpace(project) != "" && strings.TrimSpace(project) != cfg.DefaultProject) {
				return errorWithMeta("invalid_host_session", "An active host-registered session in the current project is required; do not invent or reuse a session ID.", nil), nil
			}
		}
		rawCandidates, ok := req.GetArguments()["candidates"]
		if !ok {
			return mcp.NewToolResultError("candidates is required"), nil
		}
		encoded, err := json.Marshal(rawCandidates)
		if err != nil {
			return mcp.NewToolResultError("Invalid candidates: " + err.Error()), nil
		}
		var candidates []store.MemoryCandidateInput
		if err := json.Unmarshal(encoded, &candidates); err != nil {
			return mcp.NewToolResultError("Invalid candidates: " + err.Error()), nil
		}
		result, err := s.CommitMemoryCandidates(store.CommitMemoryCandidatesParams{
			SessionID: strings.TrimSpace(sessionID), Project: strings.TrimSpace(project), Candidates: candidates,
		})
		if err != nil {
			if errors.Is(err, store.ErrSessionNotStarted) {
				return errorWithMeta("session_not_started", "Session registration has not completed.", nil), nil
			}
			return mcp.NewToolResultError("Failed to commit session candidates: " + err.Error()), nil
		}
		if activity != nil && result.Promoted > 0 {
			activity.RecordSave(sessionID)
		}
		sessionOrigin := store.SessionOriginUnknown
		if sess, sessErr := s.GetSession(strings.TrimSpace(sessionID)); sessErr == nil && sess != nil {
			sessionOrigin = store.NormalizeSessionOrigin(sess.Origin)
		}
		out, err := json.Marshal(map[string]any{
			"promoted": result.Promoted, "review": result.Review, "rejected": result.Rejected,
			"duplicate":     result.Duplicate,
			"candidate_ids": result.CandidateIDs,
			// Provenance of the session that staged these candidates. It is a
			// claim of transport, not host authentication.
			"session_origin": sessionOrigin,
			// Per-candidate admission results: without them a rejected count is
			// opaque to the agent and the reason is only visible via sqlite/CLI.
			"results": result.Results,
		})
		if err != nil {
			return mcp.NewToolResultError("Failed to encode session commit result: " + err.Error()), nil
		}
		return mcp.NewToolResultText(string(out)), nil
	}
}

func handleSuggestTopicKey() server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		typ, _ := req.GetArguments()["type"].(string)
		title, _ := req.GetArguments()["title"].(string)
		content, _ := req.GetArguments()["content"].(string)

		if strings.TrimSpace(title) == "" && strings.TrimSpace(content) == "" {
			return errorWithMeta("title_required", "provide title or content to suggest a topic_key", nil), nil
		}

		topicKey := suggestTopicKey(typ, title, content)
		if topicKey == "" {
			return errorWithMeta("topic_key_unavailable", "could not suggest topic_key from input", nil), nil
		}

		return mcp.NewToolResultText(fmt.Sprintf("Suggested topic_key: %s", topicKey)), nil
	}
}

func handleUpdate(s *store.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := int64(intArg(req, "id", 0))
		if id == 0 {
			return mcp.NewToolResultError("id is required"), nil
		}

		update := store.UpdateObservationParams{}
		if v, ok := req.GetArguments()["title"].(string); ok {
			update.Title = &v
		}
		if v, ok := req.GetArguments()["content"].(string); ok {
			update.Content = &v
		}
		if v, ok := req.GetArguments()["type"].(string); ok {
			update.Type = &v
		}
		if v, ok := req.GetArguments()["memory_class"].(string); ok {
			update.MemoryClass = &v
		}
		if v, ok := req.GetArguments()["memory_path"].(string); ok {
			update.MemoryPath = &v
		}
		if v, ok := req.GetArguments()["abstract"].(string); ok {
			update.Abstract = &v
		}
		if v, ok := req.GetArguments()["overview"].(string); ok {
			update.Overview = &v
		}
		if v, ok := req.GetArguments()["scope"].(string); ok {
			update.Scope = &v
		}
		if v, ok := req.GetArguments()["topic_key"].(string); ok {
			update.TopicKey = &v
		}

		if update.Title == nil && update.Content == nil && update.Type == nil && update.MemoryClass == nil && update.MemoryPath == nil &&
			update.Abstract == nil && update.Overview == nil && update.Project == nil && update.Scope == nil && update.TopicKey == nil {
			return mcp.NewToolResultError("provide at least one field to update"), nil
		}

		var contentLen int
		if update.Content != nil {
			contentLen = len(*update.Content)
		}

		obs, err := s.UpdateObservation(id, update)
		if err != nil {
			return mcp.NewToolResultError("Failed to update memory: " + err.Error()), nil
		}

		msg := fmt.Sprintf("Memory updated: #%d %q (%s, scope=%s)", obs.ID, obs.Title, obs.Type, obs.Scope)
		if contentLen > s.MaxObservationLength() {
			msg += fmt.Sprintf("\n⚠ WARNING: Content was truncated from %d to %d chars. Consider splitting into smaller observations.", contentLen, s.MaxObservationLength())
		}

		// Auto-detect for envelope; tolerant — don't fail update on resolution error
		detRes, detErr := resolveWriteProject(s)
		if detErr != nil {
			// Still return success for the update itself.
			return mcp.NewToolResultText(msg), nil
		}
		return respondWithProject(s, detRes, msg, nil), nil
	}
}

func handleReview(s *store.Store, cfg MCPConfig) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		action, _ := req.GetArguments()["action"].(string)
		switch strings.TrimSpace(action) {
		case "list":
			projectFilter, _ := req.GetArguments()["project"].(string)
			limit := intArg(req, "limit", 10)
			detRes := projectpkg.DetectionResult{Project: projectFilter, Source: projectpkg.SourceAllProjects}
			if strings.TrimSpace(projectFilter) != "" {
				var err error
				detRes, err = resolveReadProject(s, projectFilter)
				if err != nil {
					var upe *unknownProjectError
					if errors.As(err, &upe) {
						return errorWithMeta("unknown_project",
							fmt.Sprintf("Project %q not found in store", upe.Name),
							upe.AvailableProjects,
						), nil
					}
					return mcp.NewToolResultError(fmt.Sprintf("Project resolution failed: %s", err)), nil
				}
				projectFilter = detRes.Project
			} else if res, err := resolveReadProjectWithProcessOverride(s, "", cfg.DefaultProject); err == nil {
				detRes = res
				projectFilter = res.Project
			} else {
				return errorWithMeta("project_not_selected", "Select an exact project for review; review never defaults to all projects.", nil), nil
			}

			status, _ := req.GetArguments()["status"].(string)
			observations, err := s.ObservationsNeedingReview(projectFilter, limit, status)
			if err != nil {
				return mcp.NewToolResultError("Review list error: " + err.Error()), nil
			}

			structured := make([]map[string]any, 0, len(observations))
			if len(observations) == 0 {
				return respondWithProject(s, detRes, "No memories need review.", map[string]any{"observations": structured}), nil
			}

			var b strings.Builder
			fmt.Fprintf(&b, "Found %d memories needing review:\n\n", len(observations))
			for i, obs := range observations {
				projectDisplay := ""
				if obs.Project != nil {
					projectDisplay = fmt.Sprintf(" | project: %s", *obs.Project)
				}
				reviewDisplay := ""
				if obs.ReviewAfter != nil {
					reviewDisplay = fmt.Sprintf(" | review_after: %s", *obs.ReviewAfter)
				}
				staleDisplay := ""
				if obs.StaleReason != nil {
					staleDisplay = fmt.Sprintf(" | stale_reason: %s", *obs.StaleReason)
				}
				dependencies, depErr := s.ListMemoryDependencies(store.MemoryDependencyListOptions{DependentObservationSyncID: obs.SyncID, Limit: 50})
				if depErr != nil {
					return mcp.NewToolResultError("Review dependency lookup error: " + depErr.Error()), nil
				}
				resourceDependencies, resourceErr := s.ListResourceDependencies(store.ResourceDependencyListOptions{DependentObservationSyncID: obs.SyncID, Limit: 50})
				if resourceErr != nil {
					return mcp.NewToolResultError("Review resource dependency lookup error: " + resourceErr.Error()), nil
				}
				fmt.Fprintf(&b, "[%d] #%d (%s) — %s\n    state: %s%s%s%s | incoming_dependencies: %d | resource_dependencies: %d\n",
					i+1, obs.ID, obs.Type, obs.Title, obs.State(), projectDisplay, reviewDisplay, staleDisplay, len(dependencies), len(resourceDependencies))

				entry := map[string]any{
					"id":      obs.ID,
					"sync_id": obs.SyncID,
					"title":   obs.Title,
					"type":    obs.Type,
					"state":   obs.State(),
				}
				if obs.Project != nil {
					entry["project"] = *obs.Project
				}
				if obs.ReviewAfter != nil {
					entry["review_after"] = *obs.ReviewAfter
				}
				if obs.StaleAt != nil {
					entry["stale_at"] = *obs.StaleAt
				}
				if obs.StaleReason != nil {
					entry["stale_reason"] = *obs.StaleReason
				}
				entry["dependencies"] = dependencies
				entry["resource_dependencies"] = resourceDependencies
				structured = append(structured, entry)
			}
			return respondWithProject(s, detRes, b.String(), map[string]any{"observations": structured}), nil

		case "mark_reviewed":
			id := int64(intArg(req, "observation_id", 0))
			if id == 0 {
				id = int64(intArg(req, "id", 0))
			}
			if id == 0 {
				return mcp.NewToolResultError("observation_id is required for mark_reviewed"), nil
			}
			if err := s.MarkReviewed(id); err != nil {
				return mcp.NewToolResultError("Failed to mark reviewed: " + err.Error()), nil
			}
			obs, err := s.GetObservation(id)
			if err != nil {
				return mcp.NewToolResultError("Marked reviewed but failed to reload observation: " + err.Error()), nil
			}
			extra := map[string]any{"id": obs.ID, "sync_id": obs.SyncID, "state": obs.State()}
			if obs.ReviewAfter != nil {
				extra["review_after"] = *obs.ReviewAfter
			}
			if obs.StaleAt != nil {
				extra["stale_at"] = *obs.StaleAt
			}
			if obs.StaleReason != nil {
				extra["stale_reason"] = *obs.StaleReason
			}
			detRes, detErr := resolveReadProjectWithProcessOverride(s, "", cfg.DefaultProject)
			msg := fmt.Sprintf("Memory marked reviewed: #%d %q (%s)", obs.ID, obs.Title, obs.Type)
			if detErr != nil {
				out, _ := jsonMarshal(map[string]any{"result": msg, "id": obs.ID, "sync_id": obs.SyncID, "state": obs.State()})
				return mcp.NewToolResultText(string(out)), nil
			}
			return respondWithProject(s, detRes, msg, extra), nil

		case "link_dependency":
			sourceID := int64(intArg(req, "source_observation_id", 0))
			dependentID := int64(intArg(req, "dependent_observation_id", 0))
			if sourceID == 0 || dependentID == 0 {
				return mcp.NewToolResultError("source_observation_id and dependent_observation_id are required for link_dependency"), nil
			}
			reason, _ := req.GetArguments()["reason"].(string)
			dependency, err := s.AddMemoryDependency(store.AddMemoryDependencyParams{SourceObservationID: sourceID, DependentObservationID: dependentID, Reason: reason})
			if err != nil {
				return mcp.NewToolResultError("Link dependency failed: " + err.Error()), nil
			}
			detRes := projectpkg.DetectionResult{Project: dependency.Project, Source: projectpkg.SourceObservation}
			return respondWithProject(s, detRes, "Memory dependency linked. Future source revisions will mark the dependent memory stale for review.", map[string]any{
				"sync_id": dependency.SyncID, "source_observation_id": dependency.SourceObservationID,
				"dependent_observation_id": dependency.DependentObservationID, "source_revision": dependency.SourceRevision,
				"status": dependency.Status,
			}), nil

		case "list_dependencies":
			projectFilter, _ := req.GetArguments()["project"].(string)
			detRes := projectpkg.DetectionResult{Project: projectFilter, Source: projectpkg.SourceAllProjects}
			if strings.TrimSpace(projectFilter) != "" {
				resolved, err := resolveReadProject(s, projectFilter)
				if err != nil {
					return mcp.NewToolResultError("Project resolution failed: " + err.Error()), nil
				}
				detRes, projectFilter = resolved, resolved.Project
			}
			sourceSyncID, _ := req.GetArguments()["source_observation_sync_id"].(string)
			dependentSyncID, _ := req.GetArguments()["dependent_observation_sync_id"].(string)
			status, _ := req.GetArguments()["status"].(string)
			dependencies, err := s.ListMemoryDependencies(store.MemoryDependencyListOptions{Project: projectFilter, SourceObservationSyncID: sourceSyncID, DependentObservationSyncID: dependentSyncID, Status: status, Limit: intArg(req, "limit", 50)})
			if err != nil {
				return mcp.NewToolResultError("List dependencies failed: " + err.Error()), nil
			}
			return respondWithProject(s, detRes, fmt.Sprintf("Found %d memory dependencies.", len(dependencies)), map[string]any{"dependencies": dependencies}), nil

		case "process_invalidations":
			result, err := s.ProcessDependencyInvalidations(intArg(req, "limit", 50))
			if err != nil {
				return mcp.NewToolResultError("Process dependency invalidations failed: " + err.Error()), nil
			}
			detRes := projectpkg.DetectionResult{Source: projectpkg.SourceAllProjects}
			return respondWithProject(s, detRes, fmt.Sprintf("Processed %d queued dependency invalidations; %d remain.", result.Processed, result.Remaining), map[string]any{"processed": result.Processed, "remaining": result.Remaining}), nil

		case "link_resource":
			projectName, _ := req.GetArguments()["project"].(string)
			if strings.TrimSpace(projectName) == "" {
				return mcp.NewToolResultError("project is required for link_resource"), nil
			}
			detRes, err := resolveReadProject(s, projectName)
			if err != nil {
				return mcp.NewToolResultError("Project resolution failed: " + err.Error()), nil
			}
			sourceRef, _ := req.GetArguments()["source_ref"].(string)
			dependentID := int64(intArg(req, "dependent_observation_id", 0))
			if strings.TrimSpace(sourceRef) == "" || dependentID == 0 {
				return mcp.NewToolResultError("source_ref and dependent_observation_id are required for link_resource"), nil
			}
			reason, _ := req.GetArguments()["reason"].(string)
			dependency, err := s.AddResourceDependency(store.AddResourceDependencyParams{
				SourceRef: sourceRef, DependentObservationID: dependentID, Project: detRes.Project, Reason: reason,
			})
			if err != nil {
				return mcp.NewToolResultError("Link resource dependency failed: " + err.Error()), nil
			}
			return respondWithProject(s, detRes, "Resource dependency linked. Resource content remains outside Engram.", map[string]any{"resource_dependency": dependency}), nil

		case "list_resource_dependencies":
			projectFilter, _ := req.GetArguments()["project"].(string)
			detRes := projectpkg.DetectionResult{Project: projectFilter, Source: projectpkg.SourceAllProjects}
			if strings.TrimSpace(projectFilter) != "" {
				resolved, err := resolveReadProject(s, projectFilter)
				if err != nil {
					return mcp.NewToolResultError("Project resolution failed: " + err.Error()), nil
				}
				detRes, projectFilter = resolved, resolved.Project
			}
			sourceRef, _ := req.GetArguments()["source_ref"].(string)
			dependentSyncID, _ := req.GetArguments()["dependent_observation_sync_id"].(string)
			status, _ := req.GetArguments()["status"].(string)
			dependencies, err := s.ListResourceDependencies(store.ResourceDependencyListOptions{
				Project: projectFilter, SourceRef: sourceRef, DependentObservationSyncID: dependentSyncID,
				Status: status, Limit: intArg(req, "limit", 50),
			})
			if err != nil {
				return mcp.NewToolResultError("List resource dependencies failed: " + err.Error()), nil
			}
			return respondWithProject(s, detRes, fmt.Sprintf("Found %d resource dependencies.", len(dependencies)), map[string]any{"resource_dependencies": dependencies}), nil

		case "notify_resource_changed":
			projectName, _ := req.GetArguments()["project"].(string)
			if strings.TrimSpace(projectName) == "" {
				return mcp.NewToolResultError("project is required for notify_resource_changed"), nil
			}
			detRes, err := resolveReadProject(s, projectName)
			if err != nil {
				return mcp.NewToolResultError("Project resolution failed: " + err.Error()), nil
			}
			sourceRef, _ := req.GetArguments()["source_ref"].(string)
			replacement, _ := req.GetArguments()["replacement_source_ref"].(string)
			reason, _ := req.GetArguments()["reason"].(string)
			result, err := s.NotifyResourceChanged(store.NotifyResourceChangeParams{
				Project: detRes.Project, SourceRef: sourceRef, ReplacementSourceRef: replacement, Reason: reason,
			})
			if err != nil {
				return mcp.NewToolResultError("Notify resource changed failed: " + err.Error()), nil
			}
			return respondWithProject(s, detRes, fmt.Sprintf("Marked %d of %d resource dependencies stale; memory content was not rewritten.", result.Staled, result.Matched), map[string]any{"matched": result.Matched, "staled": result.Staled}), nil

		case "refresh_resource_dependency":
			projectName, _ := req.GetArguments()["project"].(string)
			if strings.TrimSpace(projectName) == "" {
				return mcp.NewToolResultError("project is required for refresh_resource_dependency"), nil
			}
			detRes, err := resolveReadProject(s, projectName)
			if err != nil {
				return mcp.NewToolResultError("Project resolution failed: " + err.Error()), nil
			}
			dependencySyncID, _ := req.GetArguments()["resource_dependency_sync_id"].(string)
			sourceRef, _ := req.GetArguments()["source_ref"].(string)
			reason, _ := req.GetArguments()["reason"].(string)
			dependency, err := s.RefreshResourceDependency(store.RefreshResourceDependencyParams{
				SyncID: dependencySyncID, Project: detRes.Project, SourceRef: sourceRef, Reason: reason,
			})
			if err != nil {
				return mcp.NewToolResultError("Refresh resource dependency failed: " + err.Error()), nil
			}
			return respondWithProject(s, detRes, "Resource dependency explicitly refreshed; unresolved stale bindings remain visible.", map[string]any{"resource_dependency": dependency}), nil

		case "candidate_list":
			projectFilter, _ := req.GetArguments()["project"].(string)
			status, _ := req.GetArguments()["status"].(string)
			if strings.TrimSpace(status) == "" {
				status = store.CandidateStatusReview
			}
			candidates, err := s.ListMemoryCandidates(projectFilter, status, intArg(req, "limit", 10))
			if err != nil {
				return mcp.NewToolResultError("List memory candidates failed: " + err.Error()), nil
			}
			out, _ := jsonMarshal(map[string]any{
				"result":     fmt.Sprintf("Found %d staged memory candidates.", len(candidates)),
				"candidates": candidates,
			})
			return mcp.NewToolResultText(string(out)), nil

		case "candidate_promote":
			candidateID, _ := req.GetArguments()["candidate_id"].(string)
			if strings.TrimSpace(candidateID) == "" {
				return mcp.NewToolResultError("candidate_id is required for candidate_promote"), nil
			}
			reason, _ := req.GetArguments()["reason"].(string)
			observation, err := s.PromoteMemoryCandidate(candidateID, reason)
			if err != nil {
				return mcp.NewToolResultError("Promote memory candidate failed: " + err.Error()), nil
			}
			out, _ := jsonMarshal(map[string]any{
				"result": "Memory candidate promoted.", "candidate_id": candidateID,
				"observation_id": observation.ID, "observation_sync_id": observation.SyncID,
			})
			return mcp.NewToolResultText(string(out)), nil

		case "candidate_reject":
			candidateID, _ := req.GetArguments()["candidate_id"].(string)
			if strings.TrimSpace(candidateID) == "" {
				return mcp.NewToolResultError("candidate_id is required for candidate_reject"), nil
			}
			reason, _ := req.GetArguments()["reason"].(string)
			if err := s.RejectMemoryCandidate(candidateID, reason); err != nil {
				return mcp.NewToolResultError("Reject memory candidate failed: " + err.Error()), nil
			}
			out, _ := jsonMarshal(map[string]any{"result": "Memory candidate rejected.", "candidate_id": candidateID})
			return mcp.NewToolResultText(string(out)), nil

		default:
			return mcp.NewToolResultError("action must be one of: list, mark_reviewed, link_dependency, list_dependencies, process_invalidations, link_resource, list_resource_dependencies, notify_resource_changed, refresh_resource_dependency, candidate_list, candidate_promote, candidate_reject"), nil
		}
	}
}

func handleDelete(s *store.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := int64(intArg(req, "id", 0))
		if id == 0 {
			return mcp.NewToolResultError("id is required"), nil
		}

		hardDelete := boolArg(req, "hard_delete", false)
		if err := s.DeleteObservation(id, hardDelete); err != nil {
			return mcp.NewToolResultError("Failed to delete memory: " + err.Error()), nil
		}

		mode := "soft-deleted"
		if hardDelete {
			mode = "permanently deleted"
		}
		return mcp.NewToolResultText(fmt.Sprintf("Memory #%d %s", id, mode)), nil
	}
}

func handleSavePrompt(s *store.Store, cfg MCPConfig, activity *SessionActivity) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		content, _ := req.GetArguments()["content"].(string)
		sessionID, _ := req.GetArguments()["session_id"].(string)
		projectChoice, _ := req.GetArguments()["project"].(string)
		projectChoiceReason, _ := req.GetArguments()["project_choice_reason"].(string)
		recoveryToken, _ := req.GetArguments()["recovery_token"].(string)
		recoverySessionID := sessionID
		if strings.TrimSpace(recoverySessionID) == "" {
			recoverySessionID = defaultSessionID("")
		}
		validateRecoveryToken := func(res projectpkg.DetectionResult, choice string) (bool, bool) {
			if strings.TrimSpace(recoveryToken) == "" {
				return false, false
			}
			return true, activity.ValidateAmbiguousProjectRecoveryToken(recoverySessionID, recoveryToken, strings.TrimSpace(choice), res.AvailableProjects, res.Path)
		}

		detRes, err := resolveWriteProjectWithChoiceAndProcessOverride(projectChoice, projectChoiceReason, validateRecoveryToken, cfg.DefaultProject, s)
		if err != nil {
			return writeProjectErrorResult(activity, recoverySessionID, detRes, err), nil
		}
		project, _ := store.NormalizeProject(detRes.Project)

		if sessionID == "" {
			sessionID = resolveFallbackSessionID(s, project)
		}

		// Ensure the implicit MCP session exists with the current working directory.
		_ = ensureImplicitSessionWithCWD(s, sessionID, project)

		_, err = s.AddPrompt(store.AddPromptParams{
			SessionID: sessionID,
			Content:   content,
			Project:   project,
		})
		if err != nil {
			return mcp.NewToolResultError("Failed to save prompt: " + err.Error()), nil
		}

		if activity != nil {
			activity.RecordPrompt(sessionID, project, content)
		}

		detRes.Project = project
		return respondWithProject(s, detRes, fmt.Sprintf("Prompt saved: %q", truncate(content, 80)), nil), nil
	}
}

func handleContext(s *store.Store, cfg MCPConfig, activity *SessionActivity) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if raw, exists := req.GetArguments()["scope"]; exists {
			value, ok := raw.(string)
			if !ok {
				return errorWithMeta("invalid_scope", "scope must be a string", nil), nil
			}
			if _, err := store.NormalizeReadScope(value); err != nil {
				return errorWithMeta("invalid_scope", err.Error(), nil), nil
			}
		}
		projectOverride, _ := req.GetArguments()["project"].(string)
		scope, _ := req.GetArguments()["scope"].(string)
		mode, _ := req.GetArguments()["mode"].(string)
		tokenBudget := intArg(req, "token_budget", 0)
		allProjects := boolArg(req, "all_projects", false)

		var detRes projectpkg.DetectionResult
		var err error
		if allProjects {
			detRes = projectpkg.DetectionResult{Source: projectpkg.SourceAllProjects}
		} else {
			detRes, err = resolveReadProjectWithProcessOverride(s, projectOverride, cfg.DefaultProject)
		}
		if err != nil {
			var upe *unknownProjectError
			if errors.As(err, &upe) {
				return errorWithMeta("unknown_project",
					fmt.Sprintf("Project %q not found in store", upe.Name),
					upe.AvailableProjects,
				), nil
			}
			return mcp.NewToolResultError(fmt.Sprintf("Project resolution failed: %s", err)), nil
		}
		project := detRes.Project
		project, _ = store.NormalizeProject(project)
		detRes.Project = project // JR2-1: keep envelope in sync with normalized query project

		// Personal scope cannot override a selected project identity.
		contextProject := project

		sessionID := defaultSessionID(project)
		activity.RecordToolCall(sessionID)

		contextResponse, err := s.FormatContextBounded(store.ContextOptions{
			Project: contextProject, Scope: scope, Mode: mode, TokenBudget: tokenBudget,
		})
		if err != nil {
			return errorWithMeta("context_failed", "Failed to get context: "+err.Error(), nil), nil
		}
		contextResult := contextResponse.Text

		if contextResult == "" {
			return respondWithProject(s, detRes, "No previous session memories found.", map[string]any{
				"context_mode":     contextResponse.Mode,
				"estimated_tokens": contextResponse.Budget.Used,
				"token_budget":     contextResponse.Budget.Limit,
				"truncated":        contextResponse.Budget.Truncated,
				"budget_limited":   contextResponse.Budget.BudgetLimited,
				"empty_reason":     contextResponse.EmptyReason,
			}), nil
		}

		// Do not append unrelated global aggregates to a selected context.
		// Statistics have their own explicitly scoped read surface.
		result := contextResult

		if nudge := activity.NudgeIfNeeded(sessionID); nudge != "" {
			result += nudge
		}

		return respondWithProject(s, detRes, result, map[string]any{
			"context_mode":      contextResponse.Mode,
			"estimated_tokens":  contextResponse.Budget.Used,
			"token_budget":      contextResponse.Budget.Limit,
			"context_entries":   contextResponse.Budget.Entries,
			"context_estimator": contextResponse.Budget.Estimator,
			"truncated":         contextResponse.Budget.Truncated,
			"budget_limited":    contextResponse.Budget.BudgetLimited,
			"empty_reason":      contextResponse.EmptyReason,
		}), nil
	}
}

func handleStats(s *store.Store, cfg MCPConfig) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		projectOverride, _ := req.GetArguments()["project"].(string)
		allProjects := boolArg(req, "all_projects", false)

		var detRes projectpkg.DetectionResult
		var err error
		if allProjects {
			detRes = projectpkg.DetectionResult{Source: projectpkg.SourceAllProjects}
		} else {
			detRes, err = resolveReadProjectWithProcessOverride(s, projectOverride, cfg.DefaultProject)
		}
		if err != nil {
			var upe *unknownProjectError
			if errors.As(err, &upe) {
				return errorWithMeta("unknown_project",
					fmt.Sprintf("Project %q not found in store", upe.Name),
					upe.AvailableProjects,
				), nil
			}
			return mcp.NewToolResultError(fmt.Sprintf("Project resolution failed: %s", err)), nil
		}

		var stats *store.Stats
		if !allProjects && detRes.Project != "" {
			stats, err = s.StatsForProject(detRes.Project)
		} else {
			stats, err = loadMCPStats(s)
		}
		if err != nil {
			return mcp.NewToolResultError("Failed to get stats: " + err.Error()), nil
		}

		var projects string
		if len(stats.Projects) > 0 {
			projects = strings.Join(stats.Projects, ", ")
		} else {
			projects = "none yet"
		}

		result := fmt.Sprintf("Memory System Stats:\n- Sessions: %d\n- Observations: %d\n- Prompts: %d\n- Projects: %s\n- Projects basis: non-deleted observations in this scope (includes archived); not registered identities",
			stats.TotalSessions, stats.TotalObservations, stats.TotalPrompts, projects)

		return respondWithProject(s, detRes, result, nil), nil
	}
}

func DoctorToolHandler(s *store.Store) server.ToolHandlerFunc {
	return handleDoctor(s, MCPConfig{})
}

func handleDoctor(s *store.Store, cfg MCPConfig) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		projectOverride, _ := req.GetArguments()["project"].(string)
		check, _ := req.GetArguments()["check"].(string)
		detRes, err := resolveReadProjectWithProcessOverride(s, projectOverride, cfg.DefaultProject)
		if err != nil {
			var upe *unknownProjectError
			if errors.As(err, &upe) {
				return errorWithMeta("unknown_project", fmt.Sprintf("Project %q not found in store", upe.Name), upe.AvailableProjects), nil
			}
			return mcp.NewToolResultError(fmt.Sprintf("Project resolution failed: %s", err)), nil
		}
		project := detRes.Project
		project, _ = store.NormalizeProject(project)
		runner := diagnostic.NewRunner()
		scope := diagnostic.Scope{Store: s, Project: project, Now: time.Now()}
		var report diagnostic.Report
		if strings.TrimSpace(check) != "" {
			report, err = runner.RunOne(ctx, scope, check)
		} else {
			report, err = runner.RunAll(ctx, scope)
		}
		if err != nil {
			report = diagnostic.ErrorReport(project, err)
		}
		out, marshalErr := jsonMarshal(report)
		if marshalErr != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Doctor JSON error: %s", marshalErr)), nil
		}
		result := mcp.NewToolResultText(string(out))
		if report.Status == diagnostic.StatusError {
			result.IsError = true
		}
		return result, nil
	}
}

func handleTimeline(s *store.Store, cfg MCPConfig) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		observationID := int64(intArg(req, "observation_id", 0))
		if observationID == 0 {
			return mcp.NewToolResultError("observation_id is required"), nil
		}
		before := intArg(req, "before", 5)
		after := intArg(req, "after", 5)
		projectOverride, _ := req.GetArguments()["project"].(string)

		// Resolve project: validate override or auto-detect (REQ-310, REQ-311, REQ-314)
		detRes, err := resolveReadProjectWithProcessOverride(s, projectOverride, cfg.DefaultProject)
		if err != nil {
			var upe *unknownProjectError
			if errors.As(err, &upe) {
				return errorWithMeta("unknown_project",
					fmt.Sprintf("Project %q not found in store", upe.Name),
					upe.AvailableProjects,
				), nil
			}
			return mcp.NewToolResultError(fmt.Sprintf("Project resolution failed: %s", err)), nil
		}

		result, err := s.Timeline(observationID, before, after)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Timeline error: %s", err)), nil
		}

		var b strings.Builder

		// Session header
		if result.SessionInfo != nil {
			summary := ""
			if result.SessionInfo.Summary != nil {
				summary = fmt.Sprintf(" — %s", truncate(*result.SessionInfo.Summary, 100))
			}
			fmt.Fprintf(&b, "Session: %s (%s)%s\n", result.SessionInfo.Project, result.SessionInfo.StartedAt, summary)
			fmt.Fprintf(&b, "Total observations in session: %d\n\n", result.TotalInRange)
		}

		// Before entries
		if len(result.Before) > 0 {
			b.WriteString("─── Before ───\n")
			for _, e := range result.Before {
				fmt.Fprintf(&b, "  #%d [%s] %s — %s\n", e.ID, e.Type, e.Title, truncate(e.Content, 150))
			}
			b.WriteString("\n")
		}

		// Focus observation (highlighted)
		fmt.Fprintf(&b, ">>> #%d [%s] %s <<<\n", result.Focus.ID, result.Focus.Type, result.Focus.Title)
		fmt.Fprintf(&b, "    %s\n", truncate(result.Focus.Content, 500))
		fmt.Fprintf(&b, "    %s\n\n", timeutil.FormatLocal(result.Focus.CreatedAt))

		// After entries
		if len(result.After) > 0 {
			b.WriteString("─── After ───\n")
			for _, e := range result.After {
				fmt.Fprintf(&b, "  #%d [%s] %s — %s\n", e.ID, e.Type, e.Title, truncate(e.Content, 150))
			}
		}

		return respondWithProject(s, detRes, b.String(), nil), nil
	}
}

func handleGetObservation(s *store.Store, cfg MCPConfig) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := int64(intArg(req, "id", 0))
		if id == 0 {
			return mcp.NewToolResultError("id is required"), nil
		}
		detail, _ := req.GetArguments()["detail"].(string)
		detail = strings.ToLower(strings.TrimSpace(detail))
		if detail == "" {
			detail = "l2"
		}
		if detail != "l0" && detail != "l1" && detail != "l2" {
			return mcp.NewToolResultError("detail must be one of: l0, l1, l2"), nil
		}

		obs, err := s.GetObservation(id)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Observation #%d not found", id)), nil
		}
		snapshot, err := s.ObservationSnapshotMetadata(obs)
		if err != nil {
			return mcp.NewToolResultError("Unable to describe selected observation state"), nil
		}

		// Resolve request context separately from persisted ownership. Fetching by
		// exact ID is authoritative even from another or ambiguous workspace.
		requestRes, requestErr := resolveReadProjectWithProcessOverride(s, "", cfg.DefaultProject)

		obsProject := ""
		if obs.Project != nil {
			obsProject = fmt.Sprintf("\nProject: %s", *obs.Project)
		}
		scope := fmt.Sprintf("\nScope: %s", obs.Scope)
		topic := ""
		if obs.TopicKey != nil {
			topic = fmt.Sprintf("\nTopic: %s", *obs.TopicKey)
		}
		toolName := ""
		if obs.ToolName != nil {
			toolName = fmt.Sprintf("\nTool: %s", *obs.ToolName)
		}
		duplicateMeta := fmt.Sprintf("\nDuplicates: %d", obs.DuplicateCount)
		revisionMeta := fmt.Sprintf("\nRevisions: %d", obs.RevisionCount)
		hierarchyMeta := fmt.Sprintf("\nMemory class: %s\nSummary: %s@%d", obs.MemoryClass, obs.SummaryState, obs.SummaryRevision)
		if obs.MemoryPath != "" {
			hierarchyMeta += "\nPath: " + obs.MemoryPath
		}
		if obs.StaleReason != nil {
			hierarchyMeta += "\nStale reason: " + *obs.StaleReason
		}

		body := obs.Content
		switch detail {
		case "l0":
			body = obs.EffectiveAbstract()
		case "l1":
			body = fmt.Sprintf("Abstract: %s\nOverview: %s", obs.EffectiveAbstract(), obs.EffectiveOverview(2000))
		}
		result := fmt.Sprintf("#%d [%s] %s\n%s\nSession: %s%s%s%s\nCreated: %s",
			obs.ID, obs.Type, obs.Title,
			body,
			obs.SessionID, obsProject+scope+topic, toolName+duplicateMeta+revisionMeta, hierarchyMeta,
			timeutil.FormatLocal(obs.CreatedAt),
		)

		owner := projectpkg.DetectionResult{Source: projectpkg.SourceObservation}
		if obs.Project != nil {
			owner.Project = strings.TrimSpace(*obs.Project)
		}
		if session, sessionErr := s.GetSession(obs.SessionID); sessionErr == nil {
			if owner.Project == "" {
				owner.Project = strings.TrimSpace(session.Project)
			}
			owner.Path = strings.TrimSpace(session.Directory)
		}
		extra := map[string]any{
			"detail":          detail,
			"snapshot_sha256": snapshot.SnapshotSHA256,
			"observation_id":  obs.ID, "observation_sync_id": obs.SyncID, "revision_count": obs.RevisionCount,
			"state":                        obs.State(),
			"memory_class":                 obs.MemoryClass,
			"memory_path":                  obs.MemoryPath,
			"summary_state":                obs.SummaryState,
			"summary_revision":             obs.SummaryRevision,
			"request_context_project":      requestRes.Project,
			"request_context_source":       requestRes.Source,
			"request_context_path":         requestRes.Path,
			"request_context_alternatives": requestRes.AvailableProjects,
		}
		if obs.ReviewAfter != nil {
			extra["review_after"] = *obs.ReviewAfter
		}
		if obs.StaleAt != nil {
			extra["stale_at"] = *obs.StaleAt
		}
		if obs.StaleReason != nil {
			extra["stale_reason"] = *obs.StaleReason
		}
		if requestErr != nil {
			extra["request_context_error"] = requestErr.Error()
		}
		return respondWithProject(s, owner, result, extra), nil
	}
}

func handleSessionSummary(s *store.Store, cfg MCPConfig, activity *SessionActivity) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		content, _ := req.GetArguments()["content"].(string)
		sessionID, _ := req.GetArguments()["session_id"].(string)
		// project field intentionally not read — auto-detect only (REQ-308 write-tool contract)

		// Reject empty/whitespace-only content before any project resolution (#393).
		if strings.TrimSpace(content) == "" {
			return mcp.NewToolResultError("content is required for mem_session_summary"), nil
		}

		// Honour process-level project override (cfg.DefaultProject) set via
		// RECALLDOCK_PROJECT or `recalldock mcp --project` (#403/#413). Falls back to cwd
		// detection when no override is configured.
		detRes, err := resolveWriteProjectWithProcessOverride(cfg.DefaultProject, s)
		if err != nil {
			return writeProjectErrorResult(nil, "", detRes, err), nil
		}
		project, _ := store.NormalizeProject(detRes.Project)

		if sessionID == "" {
			sessionID = resolveFallbackSessionID(s, project)
		}

		// Ensure the implicit MCP session exists with the current working directory.
		_ = ensureImplicitSessionWithCWD(s, sessionID, project)

		_, err = s.AddObservation(store.AddObservationParams{
			SessionID: sessionID,
			Type:      "session_summary",
			Title:     fmt.Sprintf("Session summary: %s", project),
			Content:   content,
			Project:   project,
		})
		if err != nil {
			return mcp.NewToolResultError("Failed to save session summary: " + err.Error()), nil
		}

		msg := fmt.Sprintf("Session summary saved for project %q", project)
		if score := activity.ActivityScore(defaultSessionID(project)); score != "" {
			msg += "\n" + score
		}
		detRes.Project = project
		return respondWithProject(s, detRes, msg, nil), nil
	}
}

func handleSessionStart(s *store.Store, cfg MCPConfig, activity *SessionActivity) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, _ := req.GetArguments()["id"].(string)
		directory, _ := req.GetArguments()["directory"].(string)
		resolvedDirectory := strings.TrimSpace(directory)
		// project field intentionally not read — auto-detect only (REQ-308)

		detRes, pinned := processProjectResult(cfg.DefaultProject)
		var err error
		if !pinned {
			detRes, err = resolveSessionStartProject(resolvedDirectory, s)
		}
		if err != nil {
			return writeProjectErrorResult(nil, "", detRes, err), nil
		}
		project, _ := store.NormalizeProject(detRes.Project)

		activity.RecordToolCall(defaultSessionID(project))
		if resolvedDirectory == "" {
			resolvedDirectory = strings.TrimSpace(detRes.Path)
			if resolvedDirectory == "" {
				resolvedDirectory = strings.TrimSpace(currentWorkingDirectory())
			}
		}

		if err := s.CreateSessionWithOrigin(id, project, resolvedDirectory, store.SessionOriginModel); err != nil {
			if errors.Is(err, store.ErrProjectBindingRequired) {
				return projectBindingRequiredResult(resolvedDirectory), nil
			}
			return mcp.NewToolResultError("Failed to start session: " + err.Error()), nil
		}

		session, err := s.GetSession(id)
		if err != nil {
			return mcp.NewToolResultError("Session started but could not read its origin: " + err.Error()), nil
		}
		setSessionProjectDefault(s, project, resolvedDirectory)
		detRes.Project = project
		return respondWithProject(s, detRes, fmt.Sprintf("Session %q started for project %q", id, project), map[string]any{"session_origin": store.NormalizeSessionOrigin(session.Origin), "default_project": project}), nil
	}
}

// projectBindingRequiredResult explains how to establish the binding a model
// session needs. A linked worktree inherits the main checkout automatically,
// so the message points at the main checkout when it is the unbound one.
func projectBindingRequiredResult(directory string) *mcp.CallToolResult {
	directory = strings.TrimSpace(directory)
	linked, mainRoot := projectpkg.IsLinkedWorktree(directory)
	message := "A model session needs an established project binding. Bind this workspace once: `recalldock projects create --name <name>` then `recalldock projects bind <ID> --directory " + directory + "`; or restart the host so its lifecycle hook binds it."
	meta := map[string]any{
		"error_code":      "project_binding_required",
		"message":         message,
		"directory":       directory,
		"linked_worktree": linked,
		"retryable":       true,
	}
	if linked && mainRoot != "" {
		meta["main_worktree"] = mainRoot
		meta["message"] = "This linked worktree inherits the main checkout, but the main checkout is not bound either. Bind " + mainRoot + " once (projects create/bind) and retry."
	}
	body, _ := json.Marshal(meta)
	return mcp.NewToolResultError(string(body))
}

func resolveSessionStartProject(explicitDirectory string, stores ...*store.Store) (projectpkg.DetectionResult, error) {
	if explicitDirectory == "" {
		return resolveWriteProject(stores...)
	}
	res := detectProjectWithStores(explicitDirectory, stores...)
	if res.Error != nil {
		return res, res.Error
	}
	return res, nil
}

func handleSessionEnd(s *store.Store, cfg MCPConfig, activity *SessionActivity) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, _ := req.GetArguments()["id"].(string)
		summary, _ := req.GetArguments()["summary"].(string)
		// project field intentionally not read — auto-detect only (REQ-308)

		// The exact session owns its lifecycle metadata even if cwd has changed.
		// Public process scope is checked before entering this queued handler.
		session, err := s.GetSession(id)
		if errors.Is(err, sql.ErrNoRows) {
			return errorWithMeta("session_not_found", "Session id is not registered. Start it with mem_session_start first; do not retry the same unknown id.", nil), nil
		}
		if err != nil {
			return mcp.NewToolResultError("Failed to look up session ownership"), nil
		}
		project, _ := store.NormalizeProject(session.Project)
		detRes := projectpkg.DetectionResult{Project: project, Source: projectpkg.SourceSessionProject, Path: session.Directory}

		if err := s.EndSession(id, summary); err != nil {
			if errors.Is(err, store.ErrSessionNotFound) {
				return errorWithMeta("session_not_found", "Session id is not registered. Start it with mem_session_start first; do not retry the same unknown id.", nil), nil
			}
			return mcp.NewToolResultError("Failed to end session: " + err.Error()), nil
		}

		activity.ClearSession(defaultSessionID(project))
		if def, ok := sessionProjectDefaultFor(s); ok && def.project == project {
			clearSessionProjectDefault(s)
		}

		detRes.Project = project
		origin := store.NormalizeSessionOrigin(session.Origin)
		return respondWithProject(s, detRes, fmt.Sprintf("Session %q completed", id), map[string]any{"session_origin": origin}), nil
	}
}

func handleCapturePassive(s *store.Store, cfg MCPConfig, activity *SessionActivity) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		content, _ := req.GetArguments()["content"].(string)
		sessionID, _ := req.GetArguments()["session_id"].(string)
		source, _ := req.GetArguments()["source"].(string)
		// project field intentionally not read — auto-detect only (REQ-308)

		detRes, err := resolveWriteProjectWithProcessOverride(cfg.DefaultProject, s)
		if err != nil {
			return writeProjectErrorResult(nil, "", detRes, err), nil
		}
		project, _ := store.NormalizeProject(detRes.Project)

		activity.RecordToolCall(defaultSessionID(project))

		if content == "" {
			return mcp.NewToolResultError("content is required — include text with a '## Key Learnings:' section"), nil
		}

		if sessionID == "" {
			sessionID = resolveFallbackSessionID(s, project)
			_ = ensureImplicitSessionWithCWD(s, sessionID, project)
		}

		if source == "" {
			source = "mcp-passive"
		}

		result, err := s.PassiveCapture(store.PassiveCaptureParams{
			SessionID: sessionID,
			Content:   content,
			Project:   project,
			Source:    source,
		})
		if err != nil {
			return mcp.NewToolResultError("Passive capture failed: " + err.Error()), nil
		}

		detRes.Project = project
		return respondWithProject(s, detRes, fmt.Sprintf(
			"Passive capture complete: extracted=%d saved=%d duplicates=%d",
			result.Extracted, result.Saved, result.Duplicates,
		), nil), nil
	}
}

// handleJudge implements mem_judge. It validates params, calls JudgeRelation,
// and returns the updated relation row as JSON.
//
// Tool description contract (Design §6.1):
// "Record a verdict on a pending memory conflict surfaced by mem_save.
// When mem_save returns judgment_required=true, call mem_judge once per
// candidate (judgment_id is in candidates[]). Use to mark SUPERSEDES,
// CONFLICTS_WITH, NOT_CONFLICT, RELATED, COMPATIBLE, or SCOPED.
// Ask the user when ambiguous."
func handleJudge(s *store.Store, activity *SessionActivity) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		judgmentID, _ := req.GetArguments()["judgment_id"].(string)
		relation, _ := req.GetArguments()["relation"].(string)

		if judgmentID == "" {
			return mcp.NewToolResultError("judgment_id is required"), nil
		}
		if relation == "" {
			return mcp.NewToolResultError("relation is required"), nil
		}

		// Collect optional fields.
		var reason *string
		if v, ok := req.GetArguments()["reason"].(string); ok && v != "" {
			reason = &v
		}
		var evidence *string
		if v, ok := req.GetArguments()["evidence"].(string); ok && v != "" {
			evidence = &v
		}
		var confidence *float64
		if v, ok := req.GetArguments()["confidence"].(float64); ok {
			if v < 0 || v > 1 {
				return mcp.NewToolResultError("confidence must be between 0.0 and 1.0"), nil
			}
			confidence = &v
		}

		// Session context for provenance.
		sessionID, _ := req.GetArguments()["session_id"].(string)
		// Actor defaults to "agent" kind for MCP tool calls.
		markedByActor := "agent"
		markedByKind := "agent"
		markedByModel := "" // No model ID available at MCP layer without explicit param.

		result, err := s.JudgeRelation(store.JudgeRelationParams{
			JudgmentID:    judgmentID,
			Relation:      relation,
			Reason:        reason,
			Evidence:      evidence,
			Confidence:    confidence,
			MarkedByActor: markedByActor,
			MarkedByKind:  markedByKind,
			MarkedByModel: markedByModel,
			SessionID:     sessionID,
		})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		envelope := map[string]any{
			"relation": result,
		}
		out, _ := jsonMarshal(envelope)
		return mcp.NewToolResultText(string(out)), nil
	}
}

// handleCompare implements mem_compare. The agent has already judged two
// observations externally; this handler persists the verdict via JudgeBySemantic.
//
// Tool description contract (REQ-011, Design §9):
// "Persist a semantic verdict you have already judged externally into Engram.
// Accepts int IDs for both observations, resolves them to sync_ids, then
// calls JudgeBySemantic. Returns the persisted relation's sync_id."
func handleCompare(s *store.Store, _ *SessionActivity) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// --- required numeric IDs ---
		rawA, okA := req.GetArguments()["memory_id_a"].(float64)
		rawB, okB := req.GetArguments()["memory_id_b"].(float64)
		if !okA {
			return mcp.NewToolResultError("memory_id_a is required (integer observation id)"), nil
		}
		if !okB {
			return mcp.NewToolResultError("memory_id_b is required (integer observation id)"), nil
		}
		idA := int64(rawA)
		idB := int64(rawB)

		// --- required string fields ---
		relation, _ := req.GetArguments()["relation"].(string)
		if relation == "" {
			return mcp.NewToolResultError("relation is required"), nil
		}
		reasoning, _ := req.GetArguments()["reasoning"].(string)
		if reasoning == "" {
			return mcp.NewToolResultError("reasoning is required"), nil
		}

		// --- required confidence ---
		rawConf, okConf := req.GetArguments()["confidence"].(float64)
		if !okConf {
			return mcp.NewToolResultError("confidence is required (float 0.0..1.0)"), nil
		}
		if rawConf < 0 || rawConf > 1 {
			return mcp.NewToolResultError("confidence must be between 0.0 and 1.0"), nil
		}

		// --- optional model ---
		model, _ := req.GetArguments()["model"].(string)

		// Resolve integer IDs to sync_ids.
		obsA, err := s.GetObservation(idA)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("observation id=%d not found: %s", idA, err)), nil
		}
		obsB, err := s.GetObservation(idB)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("observation id=%d not found: %s", idB, err)), nil
		}

		syncID, err := s.JudgeBySemantic(store.JudgeBySemanticParams{
			SourceID:   obsA.SyncID,
			TargetID:   obsB.SyncID,
			Relation:   relation,
			Confidence: rawConf,
			Reasoning:  reasoning,
			Model:      model,
		})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		// syncID is "" when relation == "not_conflict" (JudgeBySemantic no-op).
		envelope := map[string]any{
			"sync_id": syncID,
		}
		out, _ := jsonMarshal(envelope)
		return mcp.NewToolResultText(string(out)), nil
	}
}

func handleMergeProjects(s *store.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		fromStr, _ := req.GetArguments()["from"].(string)
		to, _ := req.GetArguments()["to"].(string)
		expectedStoreID, _ := req.GetArguments()["expected_store_id"].(string)
		apply := boolArg(req, "apply", false)

		if fromStr == "" || to == "" {
			return mcp.NewToolResultError("both 'from' and 'to' are required"), nil
		}

		var sources []string
		for _, src := range strings.Split(fromStr, ",") {
			src = strings.TrimSpace(src)
			if src != "" {
				sources = append(sources, src)
			}
		}

		if len(sources) == 0 {
			return mcp.NewToolResultError("at least one source project name is required in 'from'"), nil
		}

		identity, err := s.Identity()
		if err != nil {
			return mcp.NewToolResultError("Read store identity: " + err.Error()), nil
		}
		mappings := make([]store.ProjectMapping, 0, len(sources))
		for _, source := range sources {
			mappings = append(mappings, store.ProjectMapping{Source: source, Target: to})
		}
		validationStoreID := expectedStoreID
		if !apply {
			validationStoreID = identity.StoreID
		}
		normalizedMappings, err := s.ValidateProjectMappings(validationStoreID, mappings)
		if err != nil {
			return mcp.NewToolResultError("Invalid project mapping: " + err.Error()), nil
		}
		if !apply {
			msg := fmt.Sprintf("[dry-run] Explicit mapping preview for store %s:\n", identity.StoreID)
			for _, mapping := range normalizedMappings {
				msg += fmt.Sprintf("  %q -> %q\n", mapping.Source, mapping.Target)
			}
			msg += "No changes applied. Set apply=true and expected_store_id to this exact store ID after review."
			return mcp.NewToolResultText(msg), nil
		}

		result, err := s.ApplyProjectMappings(expectedStoreID, normalizedMappings)
		if err != nil {
			return mcp.NewToolResultError("Merge failed: " + err.Error()), nil
		}

		msg := fmt.Sprintf("Merged %d explicit source(s) into %q on store %s:\n", len(result.SourcesMerged), to, identity.StoreID)
		msg += fmt.Sprintf("  Observations moved: %d\n", result.ObservationsUpdated)
		msg += fmt.Sprintf("  Sessions moved:     %d\n", result.SessionsUpdated)
		msg += fmt.Sprintf("  Prompts moved:      %d\n", result.PromptsUpdated)

		return mcp.NewToolResultText(msg), nil
	}
}

// ─── Project Resolution Helpers ──────────────────────────────────────────────

// unknownProjectError is returned when a read tool receives a project override
// that does not exist in the store.
type unknownProjectError struct {
	Name              string
	AvailableProjects []string
}

func (e *unknownProjectError) Error() string {
	return "unknown project: " + e.Name
}

type invalidProjectChoiceError struct {
	Name              string
	AvailableProjects []string
}

func (e *invalidProjectChoiceError) Error() string {
	return "invalid project choice: " + e.Name
}

type missingRecoveryTokenError struct {
	Name              string
	AvailableProjects []string
}

func (e *missingRecoveryTokenError) Error() string {
	return "missing ambiguous project recovery token for project choice: " + e.Name
}

type invalidRecoveryTokenError struct {
	Name              string
	AvailableProjects []string
}

func (e *invalidRecoveryTokenError) Error() string {
	return "invalid ambiguous project recovery token for project choice: " + e.Name
}

type invalidExplicitProjectError struct {
	Name   string
	Reason string
}

func (e *invalidExplicitProjectError) Error() string {
	if e.Reason == "" {
		return "invalid project: " + e.Name
	}
	return "invalid project: " + e.Name + " (" + e.Reason + ")"
}

type normalizedProjectCollisionError struct {
	Name              string
	Normalized        string
	CollidingProjects []string
}

func (e *normalizedProjectCollisionError) Error() string {
	return fmt.Sprintf("project %q collides after normalization to %q", e.Name, e.Normalized)
}

type unknownSessionError struct {
	SessionID string
}

func (e *unknownSessionError) Error() string {
	return "unknown session: " + e.SessionID
}

type sessionProjectMismatchError struct {
	SessionID       string
	SessionProject  string
	ExplicitProject string
}

func (e *sessionProjectMismatchError) Error() string {
	return fmt.Sprintf("session %q belongs to project %q, not %q", e.SessionID, e.SessionProject, e.ExplicitProject)
}

// resolveWriteProject detects the current project from the process working
// directory. Returns ErrAmbiguousProject if cwd is a parent of multiple repos.
func resolveWriteProject(stores ...*store.Store) (projectpkg.DetectionResult, error) {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	res := detectProjectWithStores(cwd, stores...)
	if res.Error != nil {
		return res, res.Error
	}
	return res, nil
}

func processProjectResult(project string) (projectpkg.DetectionResult, bool) {
	project = strings.TrimSpace(project)
	if project == "" {
		return projectpkg.DetectionResult{}, false
	}
	normalized, warning := store.NormalizeProject(project)
	return projectpkg.DetectionResult{
		Project: normalized,
		Source:  sourceProcessOverride,
		Path:    "",
		Warning: warning,
	}, true
}

func resolveWriteProjectWithProcessOverride(defaultProject string, stores ...*store.Store) (projectpkg.DetectionResult, error) {
	if res, ok := processProjectResult(defaultProject); ok {
		return res, nil
	}
	return resolveWriteProject(stores...)
}

type ambiguousRecoveryTokenValidator func(projectpkg.DetectionResult, string) (provided bool, valid bool)

func resolveWriteProjectWithChoiceAndProcessOverride(projectChoice, reason string, validateToken ambiguousRecoveryTokenValidator, defaultProject string, stores ...*store.Store) (projectpkg.DetectionResult, error) {
	if strings.TrimSpace(projectChoice) == "" {
		return resolveWriteProjectWithProcessOverride(defaultProject, stores...)
	}
	return resolveWriteProjectWithChoice(projectChoice, reason, validateToken, stores...)
}

// resolveWriteProjectWithChoice preserves normal write resolution authority and
// only uses an explicit project choice as a recovery path from ErrAmbiguousProject.
func resolveWriteProjectWithChoice(projectChoice, reason string, validateToken ambiguousRecoveryTokenValidator, stores ...*store.Store) (projectpkg.DetectionResult, error) {
	res, err := resolveWriteProject(stores...)
	if err == nil {
		// Non-ambiguous config/git/autodetect remains authoritative. Ignore any
		// supplied project choice so agents cannot drift writes to arbitrary buckets.
		return res, nil
	}
	if !errors.Is(err, projectpkg.ErrAmbiguousProject) {
		return res, err
	}

	if strings.TrimSpace(reason) != projectpkg.SourceUserSelectedAfterAmbiguousProject {
		return res, err
	}

	choice := strings.TrimSpace(projectChoice)
	if choice == "" || !containsProjectChoice(res.AvailableProjects, choice) {
		return res, &invalidProjectChoiceError{
			Name:              choice,
			AvailableProjects: res.AvailableProjects,
		}
	}
	if normalized, colliding := normalizedProjectCollisions(res.AvailableProjects, choice); len(colliding) > 1 {
		return res, &normalizedProjectCollisionError{
			Name:              choice,
			Normalized:        normalized,
			CollidingProjects: colliding,
		}
	}
	provided, valid := false, false
	if validateToken != nil {
		provided, valid = validateToken(res, choice)
	}
	if !provided {
		return res, &missingRecoveryTokenError{
			Name:              choice,
			AvailableProjects: res.AvailableProjects,
		}
	}
	if !valid {
		return res, &invalidRecoveryTokenError{
			Name:              choice,
			AvailableProjects: res.AvailableProjects,
		}
	}

	res.Project = choice
	res.Source = projectpkg.SourceUserSelectedAfterAmbiguousProject
	res.Path = resolveAmbiguousChoicePath(res.Path, choice)
	res.Warning = "project selected by user after ambiguous_project recovery"
	return res, nil
}

func resolveSaveWriteProjectWithProcessOverride(s *store.Store, projectChoice string, explicitProjectProvided bool, reason, sessionID string, validateToken ambiguousRecoveryTokenValidator, defaultProject string) (projectpkg.DetectionResult, error) {
	if !explicitProjectProvided && strings.TrimSpace(projectChoice) == "" && strings.TrimSpace(sessionID) == "" && strings.TrimSpace(reason) == "" {
		if processRes, ok := processProjectResult(defaultProject); ok {
			return processRes, nil
		}
	}
	return resolveSaveWriteProject(s, projectChoice, explicitProjectProvided, reason, sessionID, validateToken)
}

func resolveSaveWriteProject(s *store.Store, projectChoice string, explicitProjectProvided bool, reason, sessionID string, validateToken ambiguousRecoveryTokenValidator) (projectpkg.DetectionResult, error) {
	trimmedSessionID := strings.TrimSpace(sessionID)
	trimmedProjectChoice := strings.TrimSpace(projectChoice)
	trimmedReason := strings.TrimSpace(reason)
	var sessionProject string
	var sessionPath string
	if trimmedSessionID != "" {
		sess, err := s.GetSession(trimmedSessionID)
		if err != nil {
			return projectpkg.DetectionResult{}, &unknownSessionError{SessionID: trimmedSessionID}
		}
		sessionProject, err = normalizeExplicitWriteProject(sess.Project)
		if err != nil {
			return projectpkg.DetectionResult{}, err
		}
		sessionPath = strings.TrimSpace(sess.Directory)
	}

	if explicitProjectProvided && trimmedProjectChoice == "" {
		return projectpkg.DetectionResult{}, &invalidExplicitProjectError{Name: projectChoice, Reason: "project is required"}
	}

	if trimmedProjectChoice != "" {
		cwdRes, cwdErr := resolveWriteProject(s)
		if cwdErr != nil {
			if errors.Is(cwdErr, projectpkg.ErrInvalidConfig) {
				return cwdRes, cwdErr
			}
			if errors.Is(cwdErr, projectpkg.ErrAmbiguousProject) {
				if normalized, colliding := normalizedProjectCollisions(cwdRes.AvailableProjects, trimmedProjectChoice); len(colliding) > 1 {
					return cwdRes, &normalizedProjectCollisionError{
						Name:              trimmedProjectChoice,
						Normalized:        normalized,
						CollidingProjects: colliding,
					}
				}
			} else {
				return cwdRes, cwdErr
			}
		}

		project, err := normalizeExplicitWriteProject(projectChoice)
		if err != nil {
			return projectpkg.DetectionResult{}, err
		}
		if collisionErr := explicitWriteProjectCollision(trimmedProjectChoice, project, sessionProject, cwdRes); collisionErr != nil {
			return cwdRes, collisionErr
		}
		if sessionProject != "" && project != sessionProject {
			return projectpkg.DetectionResult{}, &sessionProjectMismatchError{
				SessionID:       trimmedSessionID,
				SessionProject:  sessionProject,
				ExplicitProject: project,
			}
		}

		exists, err := s.ProjectExists(project)
		if err != nil {
			return projectpkg.DetectionResult{}, err
		}
		if exists {
			if explicitProjectHasSeparatorCollapse(trimmedProjectChoice, project) {
				return cwdRes, &normalizedProjectCollisionError{
					Name:              trimmedProjectChoice,
					Normalized:        project,
					CollidingProjects: []string{trimmedProjectChoice, project},
				}
			}
			return projectpkg.DetectionResult{
				Project: project,
				Source:  projectpkg.SourceExplicitOverride,
				Path:    "",
			}, nil
		}

		if sessionProject != "" {
			return projectpkg.DetectionResult{
				Project: project,
				Source:  projectpkg.SourceExplicitOverride,
				Path:    sessionPath,
			}, nil
		}

		if cwdErr != nil {
			if errors.Is(cwdErr, projectpkg.ErrInvalidConfig) {
				return cwdRes, cwdErr
			}
			if errors.Is(cwdErr, projectpkg.ErrAmbiguousProject) {
				if trimmedReason == projectpkg.SourceUserSelectedAfterAmbiguousProject {
					return resolveWriteProjectWithChoice(projectChoice, reason, validateToken, s)
				}
				return cwdRes, cwdErr
			}
			return cwdRes, cwdErr
		}

		if cwdRes.Source == projectpkg.SourceConfig {
			resolvedProject, err := normalizeExplicitWriteProject(cwdRes.Project)
			if err != nil {
				return projectpkg.DetectionResult{}, err
			}
			if resolvedProject == project {
				return projectpkg.DetectionResult{
					Project: project,
					Source:  projectpkg.SourceExplicitOverride,
					Path:    cwdRes.Path,
				}, nil
			}
		}

		return projectpkg.DetectionResult{AvailableProjects: knownWriteProjects(s, cwdRes)}, &unknownProjectError{
			Name:              project,
			AvailableProjects: knownWriteProjects(s, cwdRes),
		}
	}

	if trimmedReason == projectpkg.SourceUserSelectedAfterAmbiguousProject && trimmedProjectChoice != "" {
		res, err := resolveWriteProjectWithChoice(projectChoice, reason, validateToken, s)
		if err != nil {
			return res, err
		}
		if sessionProject != "" {
			resolvedProject, err := normalizeExplicitWriteProject(res.Project)
			if err != nil {
				return projectpkg.DetectionResult{}, err
			}
			if resolvedProject != sessionProject {
				return projectpkg.DetectionResult{}, &sessionProjectMismatchError{
					SessionID:       trimmedSessionID,
					SessionProject:  sessionProject,
					ExplicitProject: resolvedProject,
				}
			}
		}
		return res, nil
	}

	if sessionProject != "" {
		return projectpkg.DetectionResult{
			Project: sessionProject,
			Source:  projectpkg.SourceSessionProject,
			Path:    sessionPath,
		}, nil
	}

	return resolveWriteProject(s)
}

func explicitWriteProjectCollision(trimmedRawProject, normalizedProject, sessionProject string, cwdRes projectpkg.DetectionResult) *normalizedProjectCollisionError {
	trimmedRawProject = strings.TrimSpace(trimmedRawProject)
	if trimmedRawProject == "" || normalizedProject == "" || !explicitProjectHasSeparatorCollapse(trimmedRawProject, normalizedProject) {
		return nil
	}

	if sessionProject != "" && sessionProject == normalizedProject {
		return &normalizedProjectCollisionError{
			Name:              trimmedRawProject,
			Normalized:        normalizedProject,
			CollidingProjects: []string{trimmedRawProject, normalizedProject},
		}
	}

	if cwdRes.Source == projectpkg.SourceConfig {
		canonical := strings.TrimSpace(cwdRes.Project)
		if canonical == trimmedRawProject {
			return nil
		}
		canonicalNormalized, _ := store.NormalizeProject(canonical)
		if canonicalNormalized == normalizedProject {
			return &normalizedProjectCollisionError{
				Name:              trimmedRawProject,
				Normalized:        normalizedProject,
				CollidingProjects: uniqueTrimmedProjects(trimmedRawProject, canonical, normalizedProject),
			}
		}
	}

	return nil
}

func explicitProjectHasSeparatorCollapse(trimmedRawProject, normalizedProject string) bool {
	lowerTrimmed := strings.TrimSpace(strings.ToLower(trimmedRawProject))
	return lowerTrimmed != "" && lowerTrimmed != normalizedProject
}

func uniqueTrimmedProjects(names ...string) []string {
	seen := make(map[string]struct{}, len(names))
	result := make([]string, 0, len(names))
	for _, name := range names {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		result = append(result, trimmed)
	}
	return result
}

func knownWriteProjects(s *store.Store, context projectpkg.DetectionResult) []string {
	seen := make(map[string]struct{})
	projects := make([]string, 0)
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		projects = append(projects, name)
	}

	stats, err := s.Stats()
	if err == nil {
		for _, project := range stats.Projects {
			add(project)
		}
	}
	add(context.Project)
	for _, project := range context.AvailableProjects {
		add(project)
	}

	return projects
}

func normalizeExplicitWriteProject(projectName string) (string, error) {
	trimmed := strings.TrimSpace(projectName)
	if trimmed == "" {
		return "", &invalidExplicitProjectError{Name: projectName, Reason: "project is required"}
	}
	if strings.ContainsAny(trimmed, `/\\`) {
		return "", &invalidExplicitProjectError{Name: projectName, Reason: "project must be a name, not a path"}
	}
	for _, r := range trimmed {
		if r < 0x20 || r == 0x7f {
			return "", &invalidExplicitProjectError{Name: projectName, Reason: "project contains control characters"}
		}
	}
	project, _ := store.NormalizeProject(trimmed)
	if project == "" {
		return "", &invalidExplicitProjectError{Name: projectName, Reason: "project is required"}
	}
	return project, nil
}

func containsProjectChoice(available []string, choice string) bool {
	choice = strings.TrimSpace(choice)
	for _, candidate := range available {
		if strings.TrimSpace(candidate) == choice {
			return true
		}
	}
	return false
}

func normalizedProjectCollisions(candidates []string, choice string) (string, []string) {
	normalized, _ := store.NormalizeProject(strings.TrimSpace(choice))
	if normalized == "" {
		return "", nil
	}

	colliding := make([]string, 0, 2)
	seen := make(map[string]struct{})
	for _, candidate := range candidates {
		trimmed := strings.TrimSpace(candidate)
		if trimmed == "" {
			continue
		}
		candidateNormalized, _ := store.NormalizeProject(trimmed)
		if candidateNormalized != normalized {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		colliding = append(colliding, trimmed)
	}
	if len(colliding) < 2 {
		return normalized, nil
	}
	return normalized, colliding
}

func resolveAmbiguousChoicePath(ambiguousParent, choice string) string {
	parent := strings.TrimSpace(ambiguousParent)
	if parent == "" || strings.TrimSpace(choice) == "" {
		return ""
	}

	entries, err := os.ReadDir(parent)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// Match the same name shape used by project.DetectProjectFull for
		// available_projects: trim + lowercase only. Do not use store.NormalizeProject
		// here because it collapses repeated '-'/'_' and can create collisions.
		if strings.TrimSpace(strings.ToLower(entry.Name())) != choice {
			continue
		}
		childPath := filepath.Join(parent, entry.Name())
		if _, err := os.Stat(filepath.Join(childPath, ".git")); err != nil {
			continue
		}
		absChild, err := filepath.Abs(childPath)
		if err != nil {
			return childPath
		}
		return absChild
	}
	return ""
}

// resolveReadProject validates an optional project override against the store.
// If override is empty, falls back to auto-detection from cwd.
// JW2: normalizes the override (lowercase+trim) before ProjectExists lookup so
// that e.g. "MyApp" and "  myapp  " both resolve to the stored "myapp".
func resolveReadProjectWithProcessOverride(s *store.Store, override, defaultProject string) (projectpkg.DetectionResult, error) {
	if strings.TrimSpace(override) == "" {
		if res, ok := processProjectResult(defaultProject); ok {
			return res, nil
		}
	}
	return resolveReadProject(s, override)
}

func resolveReadProject(s *store.Store, override string) (projectpkg.DetectionResult, error) {
	override = strings.TrimSpace(override)
	if override == "" {
		if def, ok := sessionProjectDefaultFor(s); ok {
			return projectpkg.DetectionResult{
				Project: def.project, Source: projectpkg.SourceSessionProject, Path: def.path,
			}, nil
		}
		return resolveWriteProject(s)
	}
	normalized, _ := store.NormalizeProject(override)
	exists, err := s.ProjectExists(normalized)
	if err != nil {
		return projectpkg.DetectionResult{}, err
	}
	if !exists {
		// Collect available projects for the error.
		stats, _ := s.Stats()
		return projectpkg.DetectionResult{}, &unknownProjectError{
			Name:              normalized,
			AvailableProjects: stats.Projects,
		}
	}
	return projectpkg.DetectionResult{
		Project: normalized,
		Source:  projectpkg.SourceExplicitOverride, // JR2-2: use named constant
		Path:    "",
	}, nil
}

// respondWithProject wraps a tool result by prepending the project envelope
// fields (project, project_source, project_path) to the text output.
// extra is an optional map of additional fields to include.
func respondWithProject(s *store.Store, res projectpkg.DetectionResult, text string, extra map[string]any) *mcp.CallToolResult {
	envelope := map[string]any{
		"project":        res.Project,
		"project_source": res.Source,
		"project_path":   res.Path,
		"result":         text,
	}
	if res.ProjectID != "" {
		envelope["project_id"] = res.ProjectID
		envelope["project_name"] = res.ProjectName
		envelope["binding_id"] = res.BindingID
	}
	if identity, err := s.Identity(); err == nil {
		envelope["store_id"] = identity.StoreID
		envelope["data_dir"] = identity.DataDir
		envelope["database_path"] = identity.DatabasePath
	} else {
		envelope["store_identity_error"] = err.Error()
	}
	if res.Warning != "" {
		envelope["warning"] = res.Warning
	}
	for k, v := range extra {
		envelope[k] = v
	}
	out, _ := jsonMarshal(envelope)
	return mcp.NewToolResultText(string(out))
}

func writeProjectErrorResult(activity *SessionActivity, sessionID string, res projectpkg.DetectionResult, err error) *mcp.CallToolResult {
	code := "ambiguous_project"
	if errors.Is(err, projectpkg.ErrInvalidConfig) {
		code = "invalid_project_config"
	}
	var choiceErr *invalidProjectChoiceError
	if errors.As(err, &choiceErr) {
		if choiceErr.Name == "" {
			return errorWithMeta("invalid_project_choice",
				"Project choice is empty; choose exactly one value from available_projects and retry with project_choice_reason=user_selected_after_ambiguous_project",
				choiceErr.AvailableProjects,
			)
		}
		return errorWithMeta("invalid_project_choice",
			fmt.Sprintf("Project choice %q is not one of available_projects", choiceErr.Name),
			choiceErr.AvailableProjects,
		)
	}
	var missingTokenErr *missingRecoveryTokenError
	if errors.As(err, &missingTokenErr) {
		return errorWithMeta("missing_recovery_token",
			fmt.Sprintf("project_choice_reason=user_selected_after_ambiguous_project for %q requires the recovery_token from the ambiguous_project error", missingTokenErr.Name),
			missingTokenErr.AvailableProjects,
		)
	}
	var invalidTokenErr *invalidRecoveryTokenError
	if errors.As(err, &invalidTokenErr) {
		return errorWithMeta("invalid_recovery_token",
			fmt.Sprintf("recovery_token is invalid, stale, or not valid for selected project %q", invalidTokenErr.Name),
			invalidTokenErr.AvailableProjects,
		)
	}
	var explicitErr *invalidExplicitProjectError
	if errors.As(err, &explicitErr) {
		return errorWithMeta("invalid_project",
			fmt.Sprintf("Project %q is invalid: %s", explicitErr.Name, explicitErr.Reason),
			res.AvailableProjects,
		)
	}
	var collisionErr *normalizedProjectCollisionError
	if errors.As(err, &collisionErr) {
		message := fmt.Sprintf(
			"Project %q collapses to stored bucket %q, but multiple exact candidates would share that bucket: %s. Refuse write until the colliding project names are disambiguated.",
			collisionErr.Name,
			collisionErr.Normalized,
			strings.Join(collisionErr.CollidingProjects, ", "),
		)
		return errorWithMeta("project_name_collision", message, res.AvailableProjects)
	}
	var unknownSessionErr *unknownSessionError
	if errors.As(err, &unknownSessionErr) {
		return errorWithMeta("unknown_session",
			fmt.Sprintf("Session %q was provided but does not exist", unknownSessionErr.SessionID),
			res.AvailableProjects,
		)
	}
	var unknownProjectErr *unknownProjectError
	if errors.As(err, &unknownProjectErr) {
		return errorWithMeta("unknown_project",
			fmt.Sprintf("Project %q is not backed by known context. Use an existing project, a matching session, repo .recalldock/config.json, or ambiguous-project recovery.", unknownProjectErr.Name),
			unknownProjectErr.AvailableProjects,
		)
	}
	var mismatchErr *sessionProjectMismatchError
	if errors.As(err, &mismatchErr) {
		return errorWithMeta("session_project_mismatch",
			fmt.Sprintf("Session %q belongs to project %q, but request targeted %q", mismatchErr.SessionID, mismatchErr.SessionProject, mismatchErr.ExplicitProject),
			res.AvailableProjects,
		)
	}
	if errors.Is(err, projectpkg.ErrBinding) {
		return errorWithMeta("binding_unverified", err.Error(), nil)
	}
	result := errorWithMeta(code, fmt.Sprintf("Cannot determine project: %s", err), res.AvailableProjects)
	if code == "ambiguous_project" && activity != nil {
		if strings.TrimSpace(sessionID) == "" {
			sessionID = defaultSessionID("")
		}
		addErrorMetadata(result, map[string]any{
			"recovery_token":    activity.IssueAmbiguousProjectRecoveryToken(sessionID, res.AvailableProjects, res.Path),
			"token_ttl_seconds": int(ambiguousProjectRecoveryTTL.Seconds()),
		})
	}
	return result
}

func addErrorMetadata(result *mcp.CallToolResult, metadata map[string]any) {
	if result == nil || len(result.Content) == 0 || len(metadata) == 0 {
		return
	}
	text, ok := mcp.AsTextContent(result.Content[0])
	if !ok {
		return
	}
	var envelope map[string]any
	if err := json.Unmarshal([]byte(text.Text), &envelope); err != nil {
		return
	}
	for k, v := range metadata {
		envelope[k] = v
	}
	out, err := jsonMarshal(envelope)
	if err != nil {
		return
	}
	result.Content[0] = mcp.NewTextContent(string(out))
}

// errorWithMeta returns a structured tool error result with error_code,
// message, available_projects, and a hint for resolution.
func errorWithMeta(code, msg string, availableProjects []string) *mcp.CallToolResult {
	envelope := map[string]any{
		"error_code":         code,
		"message":            store.ErrorDescription(msg, code),
		"available_projects": availableProjects,
	}
	switch code {
	case "session_not_started":
		envelope["hint"] = "Wait for the session-start operation or host SessionStart hook to succeed, then retry once with the same session_id. Do not invent an ID or broaden the project scope."
		envelope["retryable"] = true
	case "binding_unverified":
		envelope["hint"] = "Do not fall back to a directory name. Inspect the registered project ID and explicitly rebind; for historical reads pass the exact project ID without binding."
	case "ambiguous_project":
		envelope["hint"] = "Ask the user to choose one of available_projects, then retry mem_save or mem_save_prompt with project and project_choice_reason=user_selected_after_ambiguous_project; alternatively cd into the target repo or add repo .recalldock/config.json."
	case "invalid_project_choice":
		envelope["hint"] = "Use exactly one of available_projects after asking the user, or cd into the target repo, or add repo .recalldock/config.json."
	case "missing_recovery_token":
		envelope["hint"] = "Retry with the recovery_token returned by the ambiguous_project error after the user selects one available_projects value."
	case "invalid_recovery_token":
		envelope["hint"] = "Request a fresh ambiguous_project recovery_token and retry with the same session, cwd context, and selected available_projects value before it expires."
	case "unknown_project":
		envelope["hint"] = "Use one of the available_projects values, or omit project to auto-detect."
	case "invalid_project_config":
		envelope["hint"] = "Fix .recalldock/config.json so project_name is a non-empty project name."
	case "invalid_project":
		envelope["hint"] = "Use a non-empty project name, not a path."
	case "unknown_session":
		envelope["hint"] = "Start the session first, omit session_id, or retry with an existing session_id."
	case "session_project_mismatch":
		envelope["hint"] = "Use a project that matches the existing session, or omit session_id and write to a different project."
	}
	out, _ := jsonMarshal(envelope)
	result := mcp.NewToolResultText(string(out))
	result.IsError = true
	return result
}

// jsonMarshal marshals v to JSON. Named to allow test injection if needed.
func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

// defaultSessionID returns a project-scoped default session ID.
// If project is non-empty: "manual-save-{project}"
// If project is empty: "manual-save"
func defaultSessionID(project string) string {
	if project == "" {
		return "manual-save"
	}
	return "manual-save-" + project
}

// resolveFallbackSessionID resolves the session a write should attach to when
// the caller did not provide an explicit session_id.
//
// It first consults the persisted sessions table for the most recent active
// (un-ended) session of the project (issue #386). The SessionStart hook
// registers a UUID session via the HTTP server, a SEPARATE process from this
// MCP (stdio) server; the two share only the SQLite store, so the active
// session must be resolved from disk rather than from any in-process map.
//
// When no active session exists for the project (or the store query fails for
// any reason), it falls back to the manual-save-{project} session, preserving
// the prior behavior for projects with no live session.
func resolveFallbackSessionID(s *store.Store, project string) string {
	if s != nil {
		if id, ok, err := s.MostRecentActiveSession(project); err == nil && ok {
			return id
		}
	}
	return defaultSessionID(project)
}

func intArg(req mcp.CallToolRequest, key string, defaultVal int) int {
	v, ok := req.GetArguments()[key].(float64)
	if !ok {
		return defaultVal
	}
	return int(v)
}

func boolArg(req mcp.CallToolRequest, key string, defaultVal bool) bool {
	v, ok := req.GetArguments()[key].(bool)
	if !ok {
		return defaultVal
	}
	return v
}

func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "..."
}

func detectProjectWithStores(dir string, stores ...*store.Store) projectpkg.DetectionResult {
	if len(stores) > 0 && stores[0] != nil {
		return stores[0].DetectProject(dir)
	}
	return projectpkg.DetectProjectFull(dir)
}
