package project

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRootMarkersFailClosedWithoutRegistry(t *testing.T) {
	valid := `{"version":1,"store_id":"store-test","project_id":"prj-0123456789abcdef0123456789abcdef","binding_id":"bind-test"}`
	for _, body := range []string{valid, valid + " {}", `{"version":9}`, strings.Repeat("x", 4097)} {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, ".recalldock"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".recalldock/root.json"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		got := DetectProjectFull(root)
		if got.Error == nil || got.Project != "" || !errors.Is(got.Error, ErrBinding) {
			t.Fatalf("marker must never fall back to basename: %+v", got)
		}
	}
}

func TestRootMarkerSymlinkMetadataAndFileResourceRejected(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, ".recalldock")); err != nil {
		t.Fatal(err)
	}
	if got := DetectProjectFull(root); got.Error == nil {
		t.Fatal("symlink metadata must fail closed")
	}
	file := filepath.Join(t.TempDir(), "resource.md")
	if err := os.WriteFile(file, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := DetectProjectFull(file); got.Error == nil {
		t.Fatal("files must not become directory bindings")
	}
}
