package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNonInteractiveTUIFailsBeforeAnySideEffects(t *testing.T) {
	stubRuntimeHooks(t)
	stubExitWithPanic(t)
	old := interactiveTerminal
	interactiveTerminal = func() bool { return false }
	t.Cleanup(func() { interactiveTerminal = old })
	dir := filepath.Join(t.TempDir(), "absent")
	t.Setenv("RECALLDOCK_DATA_DIR", dir)
	withArgs(t, "recalldock", "tui")
	_, stderr, p := captureOutputAndRecover(t, main)
	if p == nil || !strings.Contains(stderr, "interactive terminal") {
		t.Fatalf("panic=%v stderr=%q", p, stderr)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("TUI created data before terminal check: %v", err)
	}
	withArgs(t, "recalldock", "tui", "--help")
	_, stderr, p = captureOutputAndRecover(t, main)
	if p != nil || stderr != "" {
		t.Fatalf("help requires terminal: %v %q", p, stderr)
	}
}

func TestMCPRejectsMalformedArgumentsBeforeOpeningStore(t *testing.T) {
	for _, args := range [][]string{{"--tools"}, {"--tools="}, {"--project"}, {"--project", "--tools=agent"}, {"--typo"}, {"positional"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			stubRuntimeHooks(t)
			stubExitWithPanic(t)
			cfg := testConfig(t)
			cfg.DataDir = filepath.Join(cfg.DataDir, "absent")
			withArgs(t, append([]string{"recalldock", "mcp"}, args...)...)
			_, stderr, p := captureOutputAndRecover(t, func() { cmdMCP(cfg) })
			if p == nil || stderr == "" {
				t.Fatalf("malformed args accepted: %v %q", p, stderr)
			}
			if _, err := os.Stat(cfg.DataDir); !os.IsNotExist(err) {
				t.Fatalf("malformed invocation opened DB: %v", err)
			}
		})
	}
}

func TestSaveMalformedFlagsCannotCreateNoiseOrWrongProject(t *testing.T) {
	for _, tail := range [][]string{{"--type"}, {"--project", "--scope", "personal"}, {"--project", "a", "--project", "b"}, {"--typo"}, {"unexpected"}} {
		t.Run(strings.Join(tail, "_"), func(t *testing.T) {
			stubExitWithPanic(t)
			cfg := testConfig(t)
			cfg.DataDir = filepath.Join(cfg.DataDir, "absent")
			withArgs(t, append([]string{"recalldock", "save", "Safe title", "Durable body"}, tail...)...)
			_, stderr, p := captureOutputAndRecover(t, func() { cmdSave(cfg) })
			if p == nil || stderr == "" {
				t.Fatal("unsafe save arguments accepted")
			}
			if _, err := os.Stat(cfg.DataDir); !os.IsNotExist(err) {
				t.Fatal("invalid save wrote metadata/database")
			}
		})
	}
}
