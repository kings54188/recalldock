package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"recalldock.local/recalldock/internal/memorypolicy"
	"recalldock.local/recalldock/internal/opsstore"
	"recalldock.local/recalldock/internal/project"
	"recalldock.local/recalldock/internal/store"
)

// Host events consume only lifecycle metadata. Prompt and transcript fields,
// if supplied by a host, are deliberately neither stored nor returned.
func runHostEvent(cfg store.Config, args []string, in io.Reader, out io.Writer) error {
	if len(args) == 0 || (args[0] != "codex" && args[0] != "claude-code" && args[0] != "antigravity") {
		return errors.New("unsupported host")
	}
	options, err := parseCLIOptions(args[1:], []string{"--event"}, []string{"--auto-project"})
	if err != nil {
		return err
	}
	autoProject := options.Flags["--auto-project"]
	if args[0] == "antigravity" {
		if options.Values["--event"] != "SessionStart" {
			return errors.New("Antigravity requires the trusted hook command to specify --event SessionStart")
		}
	} else if _, present := options.Values["--event"]; present {
		return errors.New("--event is only supported for Antigravity SessionStart")
	}
	b, err := io.ReadAll(io.LimitReader(in, 16385))
	if err != nil || len(b) > 16384 {
		return errors.New("invalid or oversized event")
	}
	var event struct {
		ID   string `json:"session_id"`
		CWD  string `json:"cwd"`
		Name string `json:"hook_event_name"`
	}
	if args[0] == "antigravity" {
		var native struct {
			ConversationID string   `json:"conversationId"`
			WorkspacePaths []string `json:"workspacePaths"`
		}
		if json.Unmarshal(b, &native) != nil {
			return errors.New("invalid lifecycle event")
		}
		if len(native.WorkspacePaths) != 1 {
			return errors.New("Antigravity lifecycle requires exactly one workspace path; multi-workspace conversations remain unbound")
		}
		event.ID = native.ConversationID
		event.Name = options.Values["--event"]
		event.CWD = native.WorkspacePaths[0]
	} else if json.Unmarshal(b, &event) != nil {
		return errors.New("invalid lifecycle event")
	}
	subagentStart := args[0] == "codex" && event.Name == "SubagentStart"
	if event.Name != "SessionStart" && event.Name != "SessionEnd" && !subagentStart {
		return errors.New("invalid lifecycle event")
	}
	if subagentStart {
		var native struct {
			AgentID string `json:"agent_id"`
		}
		if json.Unmarshal(b, &native) != nil || event.ID == "" || memorypolicy.ValidateSessionID("codex-"+event.ID) != nil {
			return errors.New("invalid subagent lifecycle metadata")
		}
		if autoProject {
			return errors.New("subagents must reuse an existing project binding")
		}
		event.ID = native.AgentID
	}
	id := args[0] + "-" + event.ID
	if event.ID == "" || memorypolicy.ValidateSessionID(id) != nil || !filepath.IsAbs(event.CWD) || memorypolicy.ValidateLifecycleMetadata(map[string]any{"id": id, "directory": event.CWD}, false) != nil {
		return errors.New("invalid lifecycle metadata")
	}
	// Avoid creating a new database for unconfigured projects on startup.
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "recalldock.db")); err != nil && !(autoProject && event.Name == "SessionStart") {
		return nil
	}
	s, err := store.New(cfg)
	if err != nil {
		return err
	}
	defer s.Close()
	d := project.DetectExplicitWithRegistry(event.CWD, s)
	if autoProject && event.Name == "SessionStart" {
		d, err = ensureHostProject(s, cfg.DataDir, event.CWD)
		if err != nil {
			return err
		}
	}
	if d.Error != nil {
		return errors.New("project binding is invalid")
	}
	if (d.Source != project.SourceRegistered && d.Source != project.SourceConfig && d.Source != project.SourceWorktreeMain) || d.Project == "" {
		return nil
	}
	dir := strings.TrimSpace(d.Path)
	if dir == "" {
		return errors.New("project root unavailable")
	}
	if memorypolicy.ValidateLifecycleMetadata(map[string]any{"id": id, "directory": dir, "project": d.Project}, false) != nil {
		return errors.New("invalid project metadata")
	}
	existing, getErr := s.GetSession(id)
	if getErr != nil && !errors.Is(getErr, sql.ErrNoRows) {
		return errors.New("session lookup failed")
	}
	if getErr == nil && (existing.Project != d.Project || !sameHostPath(existing.Directory, dir)) {
		return errors.New("session belongs to a different project root")
	}
	if getErr == nil && store.NormalizeSessionOrigin(existing.Origin) != store.SessionOriginHost {
		return errors.New("existing session was not registered by the trusted host lifecycle")
	}
	if event.Name == "SessionEnd" {
		if getErr != nil {
			return nil
		}
		if err := s.EndSession(id, ""); err != nil {
			return err
		}
		if _, err := os.Lstat(filepath.Join(cfg.DataDir, "operations.db")); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
		ops, err := opsstore.Open(cfg.DataDir, true)
		if err != nil {
			return err
		}
		defer ops.Close()
		return ops.EndHostSession(id, args[0], dir)
	}
	if getErr != nil {
		if err := s.CreateSessionWithOrigin(id, d.Project, dir, store.SessionOriginHost); err != nil {
			return err
		}
	} else if existing.EndedAt != nil {
		if err := s.ResumeSession(id, d.Project, existing.Directory); err != nil {
			return err
		}
	}
	ops, err := opsstore.Open(cfg.DataDir, true)
	if err != nil {
		return err
	}
	defer ops.Close()
	if err := ops.RegisterHostSession(id, args[0], dir); err != nil {
		return err
	}
	message := fmt.Sprintf("RecallDock session %s; project %s; Work root %s. Use memory/Work only when useful for a task. Chat, confirmations and temporary tests need no record or binding. Missing Work records do not block ordinary work.", id, d.Project, dir)
	if args[0] == "antigravity" {
		return json.NewEncoder(out).Encode(map[string]any{
			"injectSteps": []map[string]any{
				{"systemMessage": map[string]string{"systemMessage": message}},
			},
		})
	}
	return json.NewEncoder(out).Encode(map[string]any{"hookSpecificOutput": map[string]string{"hookEventName": event.Name, "additionalContext": message}})
}

func sameHostPath(a, b string) bool {
	ca, cb := filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(ca, cb)
	}
	return ca == cb
}
