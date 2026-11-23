package candidatehook

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCandidateHookRejectsNonlocalEndpointBeforeOutbox(t *testing.T) {
	for _, endpoint := range []string{"https://example.invalid", "http://localhost:1234", "http://127.0.0.1.evil.invalid", "http://127.0.0.1@192.0.2.1", "file:///tmp/socket", "http://127.0.0.1:80/?target=remote", "http://127.0.0.1:80/#fragment"} {
		t.Run(endpoint, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "outbox.json")
			err := Run(context.Background(), candidateHookPayload("Use loopback-only proposal delivery"), Config{Endpoint: endpoint, OutboxPath: path})
			if err == nil {
				t.Fatal("nonlocal/ambiguous endpoint accepted")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("invalid endpoint persisted input")
			}
		})
	}
}

func TestCandidateHookNeverFollowsRedirectOrUsesEnvironmentProxy(t *testing.T) {
	var redirected, proxied atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1); w.WriteHeader(200) }))
	defer remote.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", remote.URL)
		w.WriteHeader(307)
	}))
	defer redirect.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxied.Add(1); w.WriteHeader(200) }))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("ALL_PROXY", proxy.URL)
	cfg := Config{Endpoint: redirect.URL, OutboxPath: filepath.Join(t.TempDir(), "outbox.json")}
	if err := Run(context.Background(), candidateHookPayload("Retain failed proposal delivery locally"), cfg); err != nil {
		t.Fatal(err)
	}
	if redirected.Load() != 0 || proxied.Load() != 0 {
		t.Fatalf("redirected=%d proxy=%d", redirected.Load(), proxied.Load())
	}
	data, err := os.ReadFile(cfg.OutboxPath)
	if err != nil || !strings.Contains(string(data), "Retain failed") {
		t.Fatal("redirected request must stay pending")
	}
}
