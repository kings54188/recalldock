package main

import (
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGatewayCLIAddListScanAndSecretRedaction(t *testing.T) {
	home := t.TempDir()
	dataDir := filepath.Join(home, "rd-data")
	registry := filepath.Join(dataDir, "gateway.json")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("RECALLDOCK_DATA_DIR", dataDir)
	t.Setenv("RECALLDOCK_GATEWAY_REGISTRY", registry)

	// Prepare isolated host configs with synthetic secrets that MUST NOT be leaked.
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0700); err != nil {
		t.Fatal(err)
	}
	codexCfg := filepath.Join(home, ".codex", "config.toml")
	codexBody := `
[mcp_servers.recalldock_gateway]
command = "/usr/local/bin/recalldock"
args = ["gateway", "serve", "--host", "codex"]

[mcp_servers.codex_direct]
command = "npx"
args = ["-y", "direct-tool"]
env = { SECRET_API_KEY = "synthetic-secret-do-not-leak" }

[mcp_servers.shared_srv]
command = "node"
args = ["shared.js"]

[mcp_servers.disabled_managed]
command = "python3"
args = ["managed.py"]
enabled = false
`
	if err := os.WriteFile(codexCfg, []byte(codexBody), 0600); err != nil {
		t.Fatal(err)
	}

	claudeCfg := filepath.Join(home, ".claude.json")
	claudeBody := `{
  "mcpServers": {
    "claude_http": {
      "type": "http",
      "url": "https://mcp.invalid/endpoint?token=synthetic-url-secret",
      "headers": {
        "Authorization": "Bearer synthetic-bearer-token"
      }
    }
  }
}`
	if err := os.WriteFile(claudeCfg, []byte(claudeBody), 0600); err != nil {
		t.Fatal(err)
	}

	agDir := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(agDir, 0700); err != nil {
		t.Fatal(err)
	}
	agCfg := filepath.Join(agDir, "mcp_config.json")
	if err := os.WriteFile(agCfg, []byte(`{"mcpServers":{"ag_direct":{"command":"uvx","args":["mcp-ag"]}}}`), 0600); err != nil {
		t.Fatal(err)
	}

	// Read-only scan before any gateway registration: no DB or gateway.json created, zero secrets leaked.
	out, stderr := captureOutput(t, func() {
		if err := runGateway([]string{"scan", "--registry", registry, "--home", home}); err != nil {
			t.Fatalf("scan before add: %v", err)
		}
	})
	if stderr != "" {
		t.Fatalf("unexpected scan stderr: %q", stderr)
	}
	for _, secret := range []string{"synthetic-secret-do-not-leak", "synthetic-bearer-token", "synthetic-url-secret"} {
		if strings.Contains(out, secret) {
			t.Fatalf("gateway scan leaked secret material %q", secret)
		}
	}
	if _, err := os.Stat(filepath.Join(dataDir, "operations.db")); !os.IsNotExist(err) {
		t.Fatalf("scan created operations.db unexpectedly")
	}

	var preItems []scannedGatewayServer
	if err := json.Unmarshal([]byte(out), &preItems); err != nil {
		t.Fatalf("decode scan json: %v\n%s", err, out)
	}
	modes := map[string]string{}
	for _, it := range preItems {
		if it.Name == "recalldock_gateway" {
			t.Fatal("scan should skip recalldock_gateway entrypoint")
		}
		modes[it.Host+"/"+it.Name] = it.Mode
	}
	if modes["codex/codex_direct"] != "host_direct" || modes["codex/shared_srv"] != "host_direct" ||
		modes["claude-code/claude_http"] != "host_direct" || modes["antigravity/ag_direct"] != "host_direct" {
		t.Fatalf("unexpected initial modes: %+v", modes)
	}
	if _, ok := modes["codex/disabled_managed"]; ok {
		t.Fatalf("unregistered disabled entry should not be listed as host_direct: %+v", modes)
	}

	// Register shared_srv (remains active in codex -> "both"), disabled_managed (disabled in codex -> "gateway_managed"),
	// and a custom standalone backend via direct CLI flags.
	codexHashBefore := sha256.Sum256([]byte(codexBody))
	if err := runGateway([]string{
		"add", "--registry", registry, "--server", "shared_srv",
		"--source-file", codexCfg, "--keys", "mcp_servers,shared_srv",
		"--host", "codex", "--description", "Shared with Codex", "--enable", "--stateful",
	}); err != nil {
		t.Fatalf("add shared_srv: %v", err)
	}
	if err := runGateway([]string{
		"add", "--registry", registry, "--server", "disabled_managed",
		"--source-file", codexCfg, "--keys", "mcp_servers,disabled_managed",
		"--host", "codex", "--enable",
	}); err != nil {
		t.Fatalf("add disabled_managed: %v", err)
	}
	standalonePath := filepath.Join(home, "custom-mcp.json")
	if err := os.WriteFile(standalonePath, []byte(`{"mcpServers":{"solo":{"command":"solo-mcp"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runGateway([]string{
		"add", "--registry", registry, "--server", "solo",
		"--source-file", standalonePath, "--keys", "mcpServers,solo",
		"--description", "Standalone MCP", "--enable",
	}); err != nil {
		t.Fatalf("add solo: %v", err)
	}
	// Duplicate add must fail.
	if err := runGateway([]string{
		"add", "--registry", registry, "--server", "solo",
		"--source-file", standalonePath, "--keys", "mcpServers,solo",
	}); err == nil {
		t.Fatal("duplicate add should fail")
	}

	// Host config must remain byte-for-byte untouched.
	codexAfter, _ := os.ReadFile(codexCfg)
	if sha256.Sum256(codexAfter) != codexHashBefore {
		t.Fatal("gateway add/scan modified Codex config file")
	}

	// Check gateway list output fields.
	listOut, _ := captureOutput(t, func() {
		if err := runGateway([]string{"list", "--registry", registry, "--trust", "strict"}); err != nil {
			t.Fatalf("list: %v", err)
		}
	})
	var listed []struct {
		ID           string `json:"id"`
		Host         string `json:"host"`
		Description  string `json:"description"`
		SourcePath   string `json:"source_path"`
		Stateful     bool   `json:"stateful"`
		Enabled      bool   `json:"enabled"`
		StartAllowed bool   `json:"start_allowed"`
		Trust        string `json:"trust"`
	}
	if err := json.Unmarshal([]byte(listOut), &listed); err != nil {
		t.Fatalf("decode list: %v\n%s", err, listOut)
	}
	if len(listed) != 3 {
		t.Fatalf("expected 3 registered servers, got %d (%s)", len(listed), listOut)
	}
	if listed[1].ID != "shared_srv" || !listed[1].Stateful || listed[1].Host != "codex" || listed[1].Trust != "strict" || listed[1].SourcePath != codexCfg {
		t.Fatalf("unexpected list entry metadata: %+v", listed[1])
	}

	// Post-add scan must classify "both" vs "gateway_managed" vs "host_direct".
	scanOut2, _ := captureOutput(t, func() {
		if err := runGateway([]string{"scan", "--registry", registry, "--home", home}); err != nil {
			t.Fatalf("scan after add: %v", err)
		}
	})
	var postItems []scannedGatewayServer
	if err := json.Unmarshal([]byte(scanOut2), &postItems); err != nil {
		t.Fatal(err)
	}
	modes2 := map[string]string{}
	for _, it := range postItems {
		modes2[it.Host+"/"+it.Name] = it.Mode
	}
	if modes2["codex/shared_srv"] != "both" || modes2["codex/disabled_managed"] != "gateway_managed" ||
		modes2["codex/codex_direct"] != "host_direct" || modes2["/solo"] != "gateway_managed" {
		t.Fatalf("unexpected post-add scan classification: %+v", modes2)
	}
}
