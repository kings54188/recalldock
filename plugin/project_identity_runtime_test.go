package plugin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// Run the real hooks against a loopback server. In particular, an empty
// selection must not turn into a /context request using the server's cwd.
func TestSessionStartRequiresResolvedProjectForMemoryAccess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("legacy bash hook test uses POSIX /bin/sh")
	}
	for _, tool := range []string{"bash", "curl", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is required for shell hook execution", tool)
		}
	}
	for _, host := range []string{"codex", "claude-code"} {
		for _, project := range []string{"", "prj-selected"} {
			t.Run(host+"/project="+project, func(t *testing.T) {
				cwd := t.TempDir()
				var mu sync.Mutex
				var memoryCalls []string
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch r.URL.Path {
					case "/health":
						json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
					case "/project/current":
						if r.URL.Query().Get("cwd") != cwd {
							t.Error("hook did not forward the original cwd")
						}
						json.NewEncoder(w).Encode(map[string]string{"project": project})
					case "/sessions", "/context":
						mu.Lock()
						memoryCalls = append(memoryCalls, r.URL.Path)
						mu.Unlock()
						if r.URL.Path == "/context" {
							if project != "" && r.URL.Query().Get("project") != project {
								t.Error("context selected a different project")
							}
							json.NewEncoder(w).Encode(map[string]string{"context": "CONTEXT_FROM_SERVER_DEFAULT"})
						} else {
							var body struct {
								Project string `json:"project"`
							}
							if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Project != project {
								t.Error("session selected a different project")
							}
							json.NewEncoder(w).Encode(map[string]string{"status": "created"})
						}
					default:
						http.NotFound(w, r)
					}
				}))
				defer srv.Close()
				u, err := url.Parse(srv.URL)
				if err != nil {
					t.Fatal(err)
				}
				// Keep the hook from invoking a real installed protocol-mode CLI.
				bin := t.TempDir()
				if err := os.WriteFile(filepath.Join(bin, "recalldock"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, "bash", filepath.Join(repoRoot(t), "plugin", host, "scripts", "session-start.sh"))
				cmd.Dir = cwd
				cmd.Env = append(os.Environ(), "RECALLDOCK_PORT="+u.Port(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
				payload, err := json.Marshal(map[string]string{"session_id": "synthetic-hook-session", "cwd": cwd})
				if err != nil {
					t.Fatal(err)
				}
				cmd.Stdin = strings.NewReader(string(payload))
				output, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("hook failed: %v", err)
				}
				mu.Lock()
				calls := append([]string(nil), memoryCalls...)
				mu.Unlock()
				if project == "" {
					if len(calls) != 0 || strings.Contains(string(output), "CONTEXT_FROM_SERVER_DEFAULT") {
						t.Fatalf("unresolved hook accessed memory: %v", calls)
					}
				} else if strings.Join(calls, ",") != "/sessions,/context" || !strings.Contains(string(output), "CONTEXT_FROM_SERVER_DEFAULT") {
					t.Fatalf("resolved hook lost scoped session/context: %v", calls)
				}
			})
		}
	}
}

func TestCodexPromptSkipsCaptureWhenProjectIsUnresolved(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("legacy bash hook test uses POSIX /tmp")
	}
	for _, tool := range []string{"bash", "curl", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is required for shell hook execution", tool)
		}
	}
	for _, project := range []string{"", "prj-selected"} {
		t.Run("project="+project, func(t *testing.T) {
			cwd := t.TempDir()
			var mu sync.Mutex
			var projects []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/project/current":
					json.NewEncoder(w).Encode(map[string]string{"project": project})
				case "/prompts":
					var body struct {
						Project string `json:"project"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					mu.Lock()
					projects = append(projects, body.Project)
					mu.Unlock()
					w.WriteHeader(http.StatusCreated)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			u, err := url.Parse(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			session := "synthetic-" + filepath.Base(filepath.Dir(cwd)) + "-" + filepath.Base(cwd)
			state := filepath.Join("/tmp", "recalldock-codex-"+session+"-tools-loaded")
			t.Cleanup(func() { _ = os.Remove(state) })
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", filepath.Join(repoRoot(t), "plugin", "codex", "scripts", "user-prompt-submit.sh"))
			cmd.Dir = cwd
			cmd.Env = append(os.Environ(), "RECALLDOCK_PORT="+u.Port())
			payload, _ := json.Marshal(map[string]string{"session_id": session, "cwd": cwd, "prompt": "Synthetic test prompt"})
			cmd.Stdin = strings.NewReader(string(payload))
			if _, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("hook failed: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if project == "" && len(projects) != 0 {
				t.Fatalf("unresolved prompt captured with projects %q", projects)
			}
			if project != "" && (len(projects) != 1 || projects[0] != project) {
				t.Fatalf("resolved prompt lost exact selection: %v", projects)
			}
		})
	}
}
