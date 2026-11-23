package gateway

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func sourceFixture(t *testing.T, format, text string) Source {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source."+format)
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return Source{Path: path, Format: format, Keys: []string{"mcpServers", "test"}}
}

func TestNativeCommandDiscoveryTracksOfficialEntrypointReplacement(t *testing.T) {
	for _, name := range []string{"codex", "claude", "agy", "dsh"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			entry := filepath.Join(dir, name)
			scriptExt := ""
			if runtime.GOOS == "windows" {
				scriptExt = ".cmd"
				entry = filepath.Join(dir, name+scriptExt)
			}
			for _, version := range []string{"native-version-one", "native-version-two"} {
				script := "#!/bin/sh\nprintf '%s\\n' '" + version + "'\n"
				if runtime.GOOS == "windows" {
					script = "@echo off\r\necho " + version + "\r\n"
				}
				if err := os.WriteFile(entry, []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				resolved, err := backendCommand(name, []string{"PATH=" + dir})
				if err != nil || !strings.EqualFold(resolved, entry) {
					t.Fatalf("native PATH entry was pinned elsewhere: %q vs %q %v", resolved, entry, err)
				}
				body, err := exec.Command(resolved, "--version").CombinedOutput()
				if err != nil || strings.TrimSpace(string(body)) != version {
					t.Fatalf("entrypoint replacement did not load current runtime: %s %v", body, err)
				}
			}
			if _, err := backendCommand(name, []string{"PATH=" + filepath.Join(dir, "missing")}); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("missing native entrypoint fell back to a pinned runtime: %v", err)
			}
		})
	}
}

func TestRuntimeConfigurationAndLiteralShellArguments(t *testing.T) {
	t.Setenv("GATEWAY_TEST_VALUE", "synthetic-value")
	source := sourceFixture(t, "json", `{"mcpServers":{"test":{"command":"/bin/sh","args":["-c","printf '%s' \"$HOME\"","${GATEWAY_TEST_VALUE}","${UNSET_GATEWAY_VALUE:-fallback}"],"env":{"EXPLICIT":"${GATEWAY_TEST_VALUE}"}}}}`)
	runtime, hash, err := source.Read()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Args[1] != `printf '%s' "$HOME"` || runtime.Args[2] != "synthetic-value" || runtime.Args[3] != "fallback" {
		t.Fatalf("arguments were interpreted: %q", runtime.Args)
	}
	if len(hash) != 64 || runtime.Env["EXPLICIT"] != "synthetic-value" {
		t.Fatal("configuration not resolved")
	}
	t.Setenv("UNRELATED_CREDENTIAL", "must-not-be-forwarded")
	env, err := runtime.Environment()
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(env, "UNRELATED_CREDENTIAL=must-not-be-forwarded") {
		t.Fatal("inherited unrelated credential")
	}
	if !slices.Contains(env, "EXPLICIT=synthetic-value") {
		t.Fatal("lost configured environment")
	}
	old := hash
	body := `{"mcpServers":{"test":{"enabled":false,"command":"/bin/sh","args":["-c","printf '%s' \"$HOME\"","${GATEWAY_TEST_VALUE}","${UNSET_GATEWAY_VALUE:-fallback}"],"env":{"EXPLICIT":"${GATEWAY_TEST_VALUE}"}}}}`
	if err := os.WriteFile(source.Path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	_, hash, err = source.Read()
	if err != nil || hash != old {
		t.Fatal("autostart toggle invalidated same backend")
	}
	if err := os.WriteFile(source.Path, []byte(strings.Replace(body, "/bin/sh", "/bin/false", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	_, hash, err = source.Read()
	if err != nil || hash == old {
		t.Fatal("changed command retained old identity")
	}
	if err := os.WriteFile(source.Path, []byte(`{"mcpServers":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := source.Read(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("deleted backend resolved: %v", err)
	}
}

func TestTOMLSourceAndMissingVariableFailClosed(t *testing.T) {
	source := sourceFixture(t, "toml", "[mcpServers.test]\ncommand = '/bin/false'\nargs = ['literal with spaces']\n")
	runtime, _, err := source.Read()
	if err != nil || runtime.Type != "stdio" || len(runtime.Args) != 1 {
		t.Fatalf("TOML: %+v %v", runtime, err)
	}
	source = sourceFixture(t, "json", `{"mcpServers":{"test":{"command":"${UNSET_REQUIRED_GATEWAY_VARIABLE}"}}}`)
	if _, _, err := source.Read(); err == nil {
		t.Fatal("missing variable accepted")
	}
}

func TestDeclaredPathChangeInvalidatesIdentity(t *testing.T) {
	source := sourceFixture(t, "json", `{"mcpServers":{"test":{"command":"backend","env":{"PATH":"/old/bin","API_TOKEN":"old"}}}}`)
	_, before, err := source.Read()
	if err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(source.Path)
	if err := os.WriteFile(source.Path, []byte(strings.Replace(string(body), "/old/bin", "/new/bin", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	_, after, err := source.Read()
	if err != nil || before == after {
		t.Fatal("execution PATH changed without invalidating old approvals")
	}
	body, _ = os.ReadFile(source.Path)
	if err := os.WriteFile(source.Path, []byte(strings.Replace(string(body), `"API_TOKEN":"old"`, `"API_TOKEN":"rotated"`, 1)), 0600); err != nil {
		t.Fatal(err)
	}
	_, rotated, err := source.Read()
	if err != nil || rotated == after {
		t.Fatal("declared env rotation retained old execution approval")
	}
}

func TestExecutionIdentityCoversResolvedConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, before, after string
	}{
		{"transport alias", `{"command":"/bin/false","url":"http://127.0.0.1/mcp","transport":"stdio"}`, `{"command":"/bin/false","url":"http://127.0.0.1/mcp","transport":"http"}`},
		{"forwarded environment names", `{"command":"/bin/false"}`, `{"command":"/bin/false","env_vars":["GATEWAY_EXECUTION_OPTIONS"]}`},
		{"execution environment value", `{"command":"/bin/false","env":{"NODE_OPTIONS":"--no-warnings"}}`, `{"command":"/bin/false","env":{"NODE_OPTIONS":"--require=/other/script"}}`},
		{"routing header value", `{"url":"https://example.test/mcp","headers":{"X-Workspace":"first"}}`, `{"url":"https://example.test/mcp","headers":{"X-Workspace":"second"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GATEWAY_EXECUTION_OPTIONS", "synthetic")
			source := sourceFixture(t, "json", `{"mcpServers":{"test":`+tc.before+`}}`)
			_, before, err := source.Read()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(source.Path, []byte(`{"mcpServers":{"test":`+tc.after+`}}`), 0600); err != nil {
				t.Fatal(err)
			}
			_, after, err := source.Read()
			if err != nil || before == after {
				t.Fatal("changed execution configuration retained old approval identity")
			}
		})
	}
	for _, field := range []string{"command", "args", "cwd", "url", "env", "forwarded env"} {
		t.Run("resolved "+field, func(t *testing.T) {
			t.Setenv("GATEWAY_EXECUTION_VALUE", "/first")
			config := map[string]any{"command": "/bin/false"}
			switch field {
			case "command", "cwd":
				config[field] = "${GATEWAY_EXECUTION_VALUE}"
			case "args":
				config[field] = []string{"${GATEWAY_EXECUTION_VALUE}"}
			case "url":
				config = map[string]any{"url": "https://example.test${GATEWAY_EXECUTION_VALUE}"}
			case "env":
				config[field] = map[string]string{"PATH": "${GATEWAY_EXECUTION_VALUE}"}
			case "forwarded env":
				config["env_vars"] = []string{"GATEWAY_EXECUTION_VALUE"}
			}
			body, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"test": config}})
			source := sourceFixture(t, "json", string(body))
			_, before, err := source.Read()
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("GATEWAY_EXECUTION_VALUE", "/second")
			_, after, err := source.Read()
			if err != nil || before == after {
				t.Fatal("resolved execution value changed without invalidating identity")
			}
		})
	}
}

func TestExecutionIdentityNormalizationAndHeaderRotation(t *testing.T) {
	t.Setenv("GATEWAY_AUTH_TOKEN", "synthetic-first")
	source := sourceFixture(t, "json", `{"mcpServers":{"test":{"transport":"streamable-http","url":"https://example.test/mcp","bearer_token_env_var":"GATEWAY_AUTH_TOKEN"}}}`)
	_, before, err := source.Read()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GATEWAY_AUTH_TOKEN", "synthetic-rotated")
	_, rotated, err := source.Read()
	if err != nil || before != rotated {
		t.Fatal("bearer value rotation invalidated execution identity")
	}
	body, _ := os.ReadFile(source.Path)
	if err := os.WriteFile(source.Path, []byte(strings.Replace(string(body), `"transport":"streamable-http"`, `"type":"http"`, 1)), 0600); err != nil {
		t.Fatal(err)
	}
	_, normalized, err := source.Read()
	if err != nil || normalized != before {
		t.Fatal("equivalent transport spelling changed identity")
	}
	t.Setenv("GATEWAY_OTHER_TOKEN", "synthetic-rotated")
	body, _ = os.ReadFile(source.Path)
	if err := os.WriteFile(source.Path, []byte(strings.Replace(string(body), "GATEWAY_AUTH_TOKEN", "GATEWAY_OTHER_TOKEN", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	_, renamed, err := source.Read()
	if err != nil || renamed == before {
		t.Fatal("changing credential source retained identity")
	}
}

func TestExecutionIdentityIncludesInheritedSearchPath(t *testing.T) {
	t.Setenv("PATH", "/first/bin")
	source := sourceFixture(t, "json", `{"mcpServers":{"test":{"command":"backend"}}}`)
	_, first, err := source.Read()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "/second/bin")
	_, second, err := source.Read()
	if err != nil || first == second {
		t.Fatal("relative command may resolve to new executable under old permission")
	}
}

func TestExactToolGrantBoundaries(t *testing.T) {
	args := map[string]any{"target": "synthetic"}
	grant := Grant{ID: "once", Tool: "write", SchemaHash: strings.Repeat("a", 64), Roots: []string{"/project"}, ArgumentsHash: Fingerprint(args), Session: "session", ExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339)}
	s := Server{Grants: []Grant{grant}}
	if _, ok := s.Allowed("write", grant.SchemaHash, "/project", "session", args); !ok {
		t.Fatal("exact grant denied")
	}
	for _, tc := range []struct {
		tool, hash, root, session string
		args                      any
	}{
		{"other", grant.SchemaHash, "/project", "session", args},
		{"write", strings.Repeat("b", 64), "/project", "session", args},
		{"write", grant.SchemaHash, "/other", "session", args},
		{"write", grant.SchemaHash, "/project", "other", args},
		{"write", grant.SchemaHash, "/project", "session", map[string]any{"target": "other"}},
	} {
		if _, ok := s.Allowed(tc.tool, tc.hash, tc.root, tc.session, tc.args); ok {
			t.Fatal("grant scope widened")
		}
	}
	s.Grants[0].ExpiresAt = time.Now().Add(-time.Second).UTC().Format(time.RFC3339)
	if _, ok := s.Allowed("write", grant.SchemaHash, "/project", "session", args); ok {
		t.Fatal("expired grant accepted")
	}
}

func TestRegistryRejectsBroadWriteGrant(t *testing.T) {
	source := sourceFixture(t, "json", `{"mcpServers":{"test":{"command":"/bin/false"}}}`)
	registry := Registry{Version: 1, Servers: map[string]Server{"test": {Source: source, Enabled: true, StartAllowed: true, Grants: []Grant{{ID: "unsafe", Tool: "delete", SchemaHash: strings.Repeat("a", 64), Roots: []string{"*"}}}}}}
	path := filepath.Join(t.TempDir(), "gateway.json")
	body, _ := json.Marshal(registry)
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRegistry(path); err == nil {
		t.Fatal("broad side-effect permission accepted")
	}
}
