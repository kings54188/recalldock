package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Synthetic stores must not inherit the checkout's real project binding.
// Running the CLI from inside the repository makes it resolve the checkout's
// own .recalldock/root.json marker, which fails closed for a synthetic store
// ("copied/moved/stale/foreign root"). That error reached fatal() -> os.Exit(1)
// and silently aborted the whole test binary without a failing test name, so
// the suite looked truncated instead of broken. Running from a neutral
// directory keeps every synthetic store unbound.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "recalldock-cmd-tests-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.Chdir(dir); err != nil {
		_ = os.RemoveAll(dir)
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// repoRootFromCaller resolves the repository root from this source file's
// location so repository-document assertions keep working after TestMain
// moved the working directory out of the checkout.
func repoRootFromCaller(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("repository root not found from caller location")
	return ""
}
