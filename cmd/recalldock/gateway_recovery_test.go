package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGatewayStartupIdentityOptionsRejectBeforeInitialization(t *testing.T) {
	base := t.TempDir()
	data := filepath.Join(base, "absent")
	t.Setenv("RECALLDOCK_DATA_DIR", data)
	t.Setenv("RECALLDOCK_GATEWAY_REGISTRY", filepath.Join(base, "registry.json"))
	for _, args := range [][]string{
		{"serve", "--host", "invalid"},
		{"serve", "--host", ""},
		{"serve", "--trust", "invalid"},
		{"serve", "--trust", ""},
		{"serve", "--host-session", "claude-code-real"},
		{"serve", "--host", "claude-code", "--host-session", "codex-real"},
		{"serve", "--host", "claude-code", "--host-session", ""},
		{"serve", "--host", "claude-code", "--host-session", "claude-code-unregistered"},
		{"serve", "--host", "antigravity", "--host-session", "codex-real"},
		{"serve", "--host", "antigravity", "--host-session", "antigravity-unregistered"},
	} {
		if err := runGateway(args); err == nil {
			t.Fatalf("invalid startup options accepted: %+v", args)
		}
		if _, err := os.Stat(data); !os.IsNotExist(err) {
			t.Fatalf("invalid startup initialized operations: %v", err)
		}
	}
}
