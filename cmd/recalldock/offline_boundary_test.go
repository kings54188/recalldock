package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"recalldock.local/recalldock/internal/cloud/autosync"
	"recalldock.local/recalldock/internal/store"
	versioncheck "recalldock.local/recalldock/internal/version"
)

func TestLocalOnlyPublicCommandsFailBeforeConfigOrNetwork(t *testing.T) {
	for _, args := range [][]string{{"cloud", "status"}, {"cloud", "serve"}, {"sync", "--cloud", "--project", "p"}, {"conflicts", "scan", "--semantic"}, {"sync", "--", "--cloud"}, {"conflicts", "scan", "--", "--semantic"}} {
		t.Run(strings.Join(args, "-"), func(t *testing.T) {
			stubRuntimeHooks(t)
			stubExitWithPanic(t)
			old := checkForUpdates
			checkForUpdates = func(string) versioncheck.CheckResult {
				t.Fatal("network update invoked")
				return versioncheck.CheckResult{}
			}
			t.Cleanup(func() { checkForUpdates = old })
			dir := filepath.Join(t.TempDir(), "absent")
			t.Setenv("RECALLDOCK_DATA_DIR", dir)
			t.Setenv("RECALLDOCK_RELEASE_API_URL", "http://192.0.2.1")
			withArgs(t, append([]string{"recalldock"}, args...)...)
			_, stderr, p := captureOutputAndRecover(t, main)
			if p == nil || !strings.Contains(stderr, "local-only") {
				t.Fatalf("not blocked: %v %q", p, stderr)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("blocked command created data")
			}
		})
	}
}

func TestProductionServersNeverStartLegacyAutosync(t *testing.T) {
	stubRuntimeHooks(t)
	stubExitWithPanic(t)
	t.Setenv("RECALLDOCK_CLOUD_AUTOSYNC", "1")
	t.Setenv("RECALLDOCK_CLOUD_TOKEN", "synthetic-token")
	t.Setenv("RECALLDOCK_CLOUD_SERVER", "http://192.0.2.1")
	old := newAutosyncManager
	newAutosyncManager = func(*store.Store, autosync.CloudTransport, autosync.Config) startableAutosyncManager {
		t.Fatal("local process started cloud manager")
		return nil
	}
	t.Cleanup(func() { newAutosyncManager = old })
	cfg := testConfig(t)
	for _, command := range []string{"serve", "mcp"} {
		withArgs(t, "recalldock", command)
		_, _, p := captureOutputAndRecover(t, func() {
			if command == "serve" {
				cmdServe(cfg)
			} else {
				cmdMCP(cfg)
			}
		})
		if p != nil {
			t.Fatalf("local server failed: %v", p)
		}
	}
}
