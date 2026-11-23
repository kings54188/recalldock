package plugin_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readPluginFile loads a plugin asset relative to the repository root.
func readPluginFile(t *testing.T, rel ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{repoRoot(t)}, rel...)...)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read %s: %v", path, err)
	}
	return string(data)
}

// TestHostHookScriptsDelegateProjectIdentityToCore pins the project-identity
// contract for the legacy shell adapters under plugin/:
//
//  1. The adapters must not contain a name-guessing detect_project anymore.
//     A name derived from a git remote or a directory basename creates a
//     second bucket for the same checkout, so sessions and observations land
//     in a project the rest of RecallDock never reads.
//  2. Identity resolution must go through the RecallDock core via
//     GET /project/current (resolve_project helper).
//  3. The adapters must not call POST /projects/migrate. Renaming memory
//     scopes is an explicit operator decision (recalldock projects
//     consolidate --mapping); an adapter must not merge by guessed names,
//     which would also bypass the project-key retirement fence.
func TestHostHookScriptsDelegateProjectIdentityToCore(t *testing.T) {
	scripts := []string{
		filepath.Join("plugin", "claude-code", "scripts", "_helpers.sh"),
		filepath.Join("plugin", "claude-code", "scripts", "session-start.sh"),
		filepath.Join("plugin", "claude-code", "scripts", "post-compaction.sh"),
		filepath.Join("plugin", "claude-code", "scripts", "subagent-stop.sh"),
		filepath.Join("plugin", "claude-code", "scripts", "user-prompt-submit.sh"),
		filepath.Join("plugin", "codex", "scripts", "_helpers.sh"),
		filepath.Join("plugin", "codex", "scripts", "session-start.sh"),
		filepath.Join("plugin", "codex", "scripts", "post-compaction.sh"),
		filepath.Join("plugin", "codex", "scripts", "subagent-stop.sh"),
		filepath.Join("plugin", "codex", "scripts", "user-prompt-submit.sh"),
	}
	for _, rel := range scripts {
		text := readPluginFile(t, rel)
		if strings.Contains(text, "detect_project") {
			t.Errorf("%s still references detect_project: adapters must not guess project names", rel)
		}
		if strings.Contains(text, "/projects/migrate") {
			t.Errorf("%s still calls /projects/migrate: merging scopes is an operator decision, never an adapter action", rel)
		}
		if strings.Contains(text, "remote get-url") || strings.Contains(text, "rev-parse --show-toplevel") {
			t.Errorf("%s still derives identity from git: identity belongs to the RecallDock core", rel)
		}
	}

	// The shared helper must resolve identity through the core endpoint and
	// fail closed when the core cannot answer.
	for _, helper := range []string{
		filepath.Join("plugin", "claude-code", "scripts", "_helpers.sh"),
		filepath.Join("plugin", "codex", "scripts", "_helpers.sh"),
	} {
		text := readPluginFile(t, helper)
		if !strings.Contains(text, `resolve_project()`) {
			t.Errorf("%s missing resolve_project helper", helper)
		}
		if !strings.Contains(text, "/project/current?cwd=") {
			t.Errorf("%s must resolve identity via GET /project/current", helper)
		}
	}

	// The embedded copy shipped by internal/setup must stay in sync with the
	// source adapter so the installed plugin cannot regress independently.
	embedded := readPluginFile(t, "internal", "setup", "plugins", "opencode", "recalldock.ts")
	source := readPluginFile(t, "plugin", "opencode", "recalldock.ts")
	if embedded != source {
		t.Errorf("internal/setup/plugins/opencode/recalldock.ts is stale: run go generate ./internal/setup/ to resync the embedded adapter")
	}
}

// TestTypeScriptAdaptersDelegateProjectIdentityToCore pins the same contract
// for the TypeScript adapters (opencode and pi).
func TestTypeScriptAdaptersDelegateProjectIdentityToCore(t *testing.T) {
	opencode := readPluginFile(t, "plugin", "opencode", "recalldock.ts")
	if strings.Contains(opencode, "extractProjectName") {
		t.Error("opencode adapter still derives project names locally; it must ask the core")
	}
	if !strings.Contains(opencode, "resolveProjectKey") || !strings.Contains(opencode, "/project/current?cwd=") {
		t.Error("opencode adapter must resolve identity via GET /project/current (resolveProjectKey)")
	}
	if strings.Contains(opencode, "/projects/migrate") {
		t.Error("opencode adapter still renames memory scopes; merging is an operator decision")
	}

	pi := readPluginFile(t, "plugin", "pi", "index.ts")
	for _, banned := range []string{"fallbackProjectName", "rawBasenameProjectName", "/projects/migrate"} {
		if strings.Contains(pi, banned) && !strings.Contains(pi, "no name-based /projects/migrate here") {
			t.Errorf("pi adapter still references %s; identity belongs to the core and merges are operator decisions", banned)
		}
	}
	if !strings.Contains(pi, "/project/current") {
		t.Error("pi adapter must keep resolving identity via /project/current")
	}
}
