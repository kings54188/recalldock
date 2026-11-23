package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"recalldock.local/recalldock/internal/setup"
	"recalldock.local/recalldock/internal/store"
	versioncheck "recalldock.local/recalldock/internal/version"
)

func TestMainCommandHelpIsConfigFree(t *testing.T) {
	commands := [][]string{
		{"--help"}, {"-h"}, {"help"}, {"help", "search"},
		{"serve", "--help"}, {"mcp", "--help"}, {"tui", "--help"},
		{"search", "--help"}, {"save", "--help"}, {"delete", "--help"},
		{"delete", "session", "--help"}, {"delete", "prompt", "-h"},
		{"delete", "project", "--help"}, {"timeline", "--help"},
		{"memory", "--help"}, {"memory", "show", "--help"}, {"memory", "history", "--help"}, {"memory", "version", "--help"}, {"memory", "update", "--help"}, {"backup", "--help"}, {"context", "--help"}, {"stats", "--help"}, {"export", "--help"},
		{"import", "--help"}, {"sync", "--help"}, {"sync", "cleanup", "--help"},
		{"projects", "--help"}, {"projects", "current", "--help"},
		{"projects", "list", "--help"}, {"projects", "consolidate", "--help"},
		{"projects", "prune", "--help"}, {"store", "current", "--help"},
		{"doctor", "--help"}, {"doctor", "repair", "--help"},
		{"conflicts", "--help"}, {"conflicts", "scan", "--help"},
		{"conflicts", "deferred", "--help"}, {"review", "--help"},
		{"review", "resource-refresh", "--help"}, {"cloud", "--help"},
		{"cloud", "serve", "--help"}, {"cloud", "upgrade", "doctor", "--help"},
		{"setup", "--help"}, {"protocol-mode", "--help"},
		{"obsidian-export", "--help"}, {"candidate-hook", "--help"},
	}
	for _, args := range commands {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			dataDir := filepath.Join(t.TempDir(), "must-not-exist")
			t.Setenv("RECALLDOCK_DATA_DIR", dataDir)
			withArgs(t, append([]string{"engram"}, args...)...)
			stubExitWithPanic(t)
			oldStore, oldCheck := storeNew, checkForUpdates
			storeCalls, updateCalls := 0, 0
			storeNew = func(store.Config) (*store.Store, error) {
				storeCalls++
				return nil, errors.New("help must not open a store")
			}
			checkForUpdates = func(string) versioncheck.CheckResult {
				updateCalls++
				return versioncheck.CheckResult{Status: versioncheck.StatusUpToDate}
			}
			t.Cleanup(func() { storeNew, checkForUpdates = oldStore, oldCheck })
			stdout, stderr, recovered := captureOutputAndRecover(t, main)
			if recovered != nil || stderr != "" || !strings.Contains(strings.ToLower(stdout), "usage:") {
				t.Fatalf("help failed: panic=%v stdout=%q stderr=%q", recovered, stdout, stderr)
			}
			if storeCalls != 0 || updateCalls != 0 {
				t.Fatalf("help side effects: store=%d update=%d", storeCalls, updateCalls)
			}
			if _, err := os.Stat(dataDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("help touched data directory: %v", err)
			}
		})
	}
}

func TestMainHelpRejectsUnknownCommandWithoutSideEffects(t *testing.T) {
	stubExitWithPanic(t)
	withArgs(t, "engram", "help", "not-a-command")
	checks := 0
	old := checkForUpdates
	checkForUpdates = func(string) versioncheck.CheckResult {
		checks++
		return versioncheck.CheckResult{Status: versioncheck.StatusUpToDate}
	}
	t.Cleanup(func() { checkForUpdates = old })
	_, stderr, recovered := captureOutputAndRecover(t, main)
	if recovered == nil || !strings.Contains(stderr, "unknown command") || checks != 0 {
		t.Fatalf("unknown help: panic=%v stderr=%q update checks=%d", recovered, stderr, checks)
	}
}

func TestMainSearchHelpAfterDelimiterIsLiteralQuery(t *testing.T) {
	cfg := testConfig(t)
	s, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	t.Setenv("RECALLDOCK_DATA_DIR", cfg.DataDir)
	withArgs(t, "engram", "search", "--all", "--", "--help", "-h")
	stubCheckForUpdates(t, versioncheck.CheckResult{Status: versioncheck.StatusUpToDate})
	old := storeSearch
	var query string
	storeSearch = func(_ *store.Store, q string, _ store.SearchOptions) ([]store.SearchResult, error) {
		query = q
		return nil, nil
	}
	t.Cleanup(func() { storeSearch = old })
	_, stderr := captureOutput(t, main)
	if query != "--help -h" || stderr != "" {
		t.Fatalf("literal query=%q stderr=%q", query, stderr)
	}
}

func TestIndependentSetupDoesNotInstallUpstream(t *testing.T) {
	stubExitWithPanic(t)
	withArgs(t, "recalldock", "setup", "codex")
	old := setupInstallAgent
	calls := 0
	setupInstallAgent = func(string) (*setup.Result, error) {
		calls++
		return nil, errors.New("upstream installation must not run")
	}
	t.Cleanup(func() { setupInstallAgent = old })
	_, stderr, recovered := captureOutputAndRecover(t, main)
	if recovered == nil || calls != 0 || !strings.Contains(stderr, "No upstream plugin will be installed") {
		t.Fatalf("setup isolation: panic=%v calls=%d stderr=%q", recovered, calls, stderr)
	}
}

func TestIndependentVersionDoesNotCheckReleasesByDefault(t *testing.T) {
	t.Setenv("RECALLDOCK_RELEASE_API_URL", "")
	withArgs(t, "recalldock", "version")
	old := checkForUpdates
	calls := 0
	checkForUpdates = func(string) versioncheck.CheckResult {
		calls++
		return versioncheck.CheckResult{Status: versioncheck.StatusUpdateAvailable, Message: "must not appear"}
	}
	t.Cleanup(func() { checkForUpdates = old })
	_, stderr := captureOutput(t, main)
	if calls != 0 || stderr != "" {
		t.Fatalf("default release check: calls=%d stderr=%q", calls, stderr)
	}
}
