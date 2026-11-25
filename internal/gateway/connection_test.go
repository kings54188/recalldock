package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func TestBackendProcess(t *testing.T) {
	mode := os.Getenv("RECALLDOCK_GATEWAY_TEST_BACKEND")
	if mode == "" {
		return
	}
	if mode == "oversize" {
		_, _ = os.Stdout.WriteString(strings.Repeat("x", FrameLimit+1))
		os.Exit(0)
	}
	if mode == "hang" {
		for {
			time.Sleep(time.Hour)
		}
	}
	if mode == "gated" {
		// Wait for the test to release this backend by creating the sentinel
		// file. This lets a test hold a connect in flight for exactly as long as
		// it needs, then let it succeed, without a timing assumption.
		gate := os.Getenv("RECALLDOCK_GATEWAY_TEST_GATE")
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(gate); err == nil {
				break
			}
			time.Sleep(time.Millisecond)
		}
	}
	s := server.NewMCPServer("synthetic", "1", server.WithToolCapabilities(true))
	echoTool := mcp.NewTool("echo", mcp.WithString("message"))
	if mode == "schema_v2" {
		echoTool = mcp.NewTool("echo", mcp.WithString("message"), mcp.WithString("extra_v2"))
	}
	s.AddTool(echoTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if req.GetString("message", "") == "wait" {
			if marker := os.Getenv("RECALLDOCK_GATEWAY_TEST_STARTED"); marker != "" {
				if f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600); err == nil {
					_, _ = f.WriteString("started\n")
					_ = f.Close()
				}
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
		return mcp.NewToolResultText(req.GetString("message", "")), nil
	})
	s.AddTool(mcp.NewTool("image"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.ImageContent{Type: "image", Data: "c3ludGhldGlj", MIMEType: "image/png"}}, IsError: true, StructuredContent: map[string]any{"synthetic": true}}, nil
	})
	if mode == "stderr" {
		go func() {
			for i := 0; i < 512; i++ {
				_, _ = os.Stderr.WriteString(strings.Repeat("test diagnostic ", 512))
			}
		}()
	}
	_ = server.NewStdioServer(s).Listen(context.Background(), os.Stdin, os.Stdout)
	os.Exit(0)
}

// backendWorkDir returns a working directory for a synthetic backend child
// process.
//
// It deliberately does NOT use t.TempDir(). On Windows a backend child keeps a
// handle on its working directory for a short, unsynchronizable window after it
// terminates, and testing.TempDir turns the resulting RemoveAll collision into a
// test failure ("The process cannot access the file because it is being used by
// another process"). That is a teardown artefact of the fixture, not behaviour
// under test, and no amount of sleeping makes it deterministic. The directory is
// therefore created and released by this helper on a best-effort basis: losing an
// empty temp directory is harmless, failing a test that already passed is not.
func backendWorkDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "rd-backend-cwd-")
	if err != nil {
		t.Fatalf("create backend working directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func backendRuntime(t *testing.T, mode string) Runtime {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Runtime{Type: "stdio", Command: executable, Args: []string{"-test.run=^TestBackendProcess$"}, CWD: backendWorkDir(t), Env: map[string]string{"RECALLDOCK_GATEWAY_TEST_BACKEND": mode}}
}

func TestConnectionProcessLifecycleAndNativeResults(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := connect(ctx, backendRuntime(t, "stderr"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	tools, err := c.Tools(ctx)
	if err != nil || len(tools) != 2 {
		t.Fatalf("tools: %d %v", len(tools), err)
	}
	result, err := c.Call(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "image", Arguments: map[string]any{}}})
	if err != nil || !result.IsError || result.StructuredContent == nil {
		t.Fatalf("result semantics lost: %+v %v", result, err)
	}
	if _, ok := result.Content[0].(mcp.ImageContent); !ok {
		t.Fatalf("image replaced by text: %T", result.Content[0])
	}
	started := time.Now()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("close was not bounded")
	}
	if c.Alive() {
		t.Fatal("closed backend still alive")
	}
	if err := c.Close(); err != nil {
		t.Fatal("close is not idempotent")
	}
}

func TestBackendCommandUsesDeclaredPath(t *testing.T) {
	rt := backendRuntime(t, "normal")
	bin := t.TempDir()
	if runtime.GOOS == "windows" {
		body, err := os.ReadFile(rt.Command)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bin, "gateway-test-backend.exe"), body, 0700); err != nil {
			t.Fatal(err)
		}
	} else if err := os.Symlink(rt.Command, filepath.Join(bin, "gateway-test-backend")); err != nil {
		t.Fatal(err)
	}
	rt.Command = "gateway-test-backend"
	rt.Env["PATH"] = bin
	t.Setenv("PATH", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := connect(ctx, rt, t.TempDir())
	if err != nil {
		t.Fatalf("declared backend PATH ignored: %v", err)
	}
	defer c.Close()
	if tools, err := c.Tools(ctx); err != nil || len(tools) != 2 {
		t.Fatalf("backend unavailable: %v", err)
	}
	// Never resolve an implicit current-directory command, even if executable.
	rt.Env["PATH"] = "."
	if c, err := connect(ctx, rt, bin); err == nil {
		c.Close()
		t.Fatal("implicit cwd command accepted")
	}
}

func TestConnectionTimeoutAndOversizedFrame(t *testing.T) {
	for _, mode := range []string{"hang", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer cancel()
			start := time.Now()
			c, err := connect(ctx, backendRuntime(t, mode), t.TempDir())
			if c != nil {
				c.Close()
			}
			if err == nil || time.Since(start) > 3*time.Second {
				t.Fatalf("unbounded failed initialization: %v", err)
			}
		})
	}
}

func TestBoundedLinesRejectsOversize(t *testing.T) {
	r := newFrameReader(strings.NewReader("{}\n"+strings.Repeat("x", 17)), 16)
	buf := make([]byte, 64)
	n, err := r.Read(buf)
	if n != 3 || err != nil {
		t.Fatalf("first frame: %d %v", n, err)
	}
	if _, err := r.Read(buf); !errors.Is(err, ErrFrameLimit) {
		t.Fatalf("oversize accepted: %v", err)
	}
}

func TestLoopbackHTTPAndSSEKeepNativeResults(t *testing.T) {
	for _, kind := range []string{"http", "sse"} {
		t.Run(kind, func(t *testing.T) {
			api := server.NewMCPServer("synthetic", "1", server.WithToolCapabilities(false))
			api.AddTool(mcp.NewTool("echo"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return mcp.NewToolResultText("synthetic"), nil
			})
			var remote *httptest.Server
			suffix := ""
			if kind == "http" {
				remote = httptest.NewServer(server.NewStreamableHTTPServer(api))
			} else {
				remote = httptest.NewServer(server.NewSSEServer(api))
				suffix = "/sse"
			}
			defer remote.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			c, err := connect(ctx, Runtime{Type: kind, URL: remote.URL + suffix}, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			tools, err := c.Tools(ctx)
			if err != nil || len(tools) != 1 {
				t.Fatalf("discovery: %v", err)
			}
			r, err := c.Call(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "echo", Arguments: map[string]any{}}})
			if err != nil || r.IsError {
				t.Fatalf("call: %+v %v", r, err)
			}
		})
	}
}

func TestRuntimeConfigFixtureIsExternalToProcessDirectory(t *testing.T) {
	r := backendRuntime(t, "normal")
	if !filepath.IsAbs(r.CWD) || r.Command == "" {
		t.Fatal("unsafe test setup")
	}
	b, err := json.Marshal(r)
	if err != nil || len(b) == 0 {
		t.Fatal(err)
	}
}
