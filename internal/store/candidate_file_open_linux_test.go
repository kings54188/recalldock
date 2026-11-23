//go:build linux

package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
	projectpkg "recalldock.local/recalldock/internal/project"
)

func candidateOpenFixture(t *testing.T) (*Store, projectpkg.DetectionResult, string, string) {
	t.Helper()
	s := newTestStore(t)
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateRegisteredProject("safe open fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.BindProjectRoot(p.ID, root); err != nil {
		t.Fatal(err)
	}
	scope := s.DetectProject(root)
	if scope.Error != nil || scope.Source != projectpkg.SourceRegistered {
		t.Fatalf("scope=%+v", scope)
	}
	return s, scope, parent, root
}

func TestBoundCandidateOpenRejectsMountTraversal(t *testing.T) {
	var rootStat, procStat unix.Stat_t
	if err := unix.Stat("/", &rootStat); err != nil {
		t.Skip(err)
	}
	if err := unix.Stat("/proc", &procStat); err != nil || rootStat.Dev == procStat.Dev {
		t.Skip("/proc is not a distinct mount")
	}
	rootFD, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(rootFD)
	childFD, err := openCandidateDirectory(rootFD, "proc")
	if err == nil {
		unix.Close(childFD)
	}
	if !errors.Is(err, unix.EXDEV) {
		t.Fatalf("mount traversal: expected EXDEV, got %v", err)
	}
}

func TestBoundCandidateOpenRejectsDirectorySwapAndBoundRootSwap(t *testing.T) {
	for _, kind := range []string{"parent-symlink", "parent-inside-symlink", "root-replacement", "root-marker-replacement", "nested-boundary"} {
		t.Run(kind, func(t *testing.T) {
			s, scope, parent, root := candidateOpenFixture(t)
			sub := filepath.Join(root, "sub")
			if err := os.Mkdir(sub, 0700); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(sub, "evidence.txt")
			if err := os.WriteFile(file, []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "parent-symlink", "parent-inside-symlink":
				if err := os.Rename(sub, filepath.Join(root, "saved")); err != nil {
					t.Fatal(err)
				}
				target := t.TempDir()
				if kind == "parent-inside-symlink" {
					target = filepath.Join(root, "saved")
				}
				if err := os.WriteFile(filepath.Join(target, "evidence.txt"), []byte("alternative"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, sub); err != nil {
					t.Fatal(err)
				}
			case "root-replacement":
				if err := os.Rename(root, filepath.Join(parent, "old-root")); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(sub, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, []byte("alternative"), 0600); err != nil {
					t.Fatal(err)
				}
			case "nested-boundary":
				if err := os.Mkdir(filepath.Join(sub, ".recalldock"), 0700); err != nil {
					t.Fatal(err)
				}
			case "root-marker-replacement":
				if err := os.WriteFile(filepath.Join(root, ".recalldock", "root.json"), []byte(`{"version":1,"store_id":"foreign","project_id":"invalid","binding_id":"invalid"}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			tx, err := s.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			f, err := openBoundCandidateFile(tx, scope, file)
			if err == nil {
				f.Close()
				t.Fatalf("%s was accepted", kind)
			}
		})
	}
}

func TestBoundCandidateOpenAllowsRegularNestedFile(t *testing.T) {
	s, scope, _, root := candidateOpenFixture(t)
	dir := filepath.Join(root, "sub")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "evidence.txt")
	if err := os.WriteFile(path, []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	f, err := openBoundCandidateFile(tx, scope, path)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	rootFile := filepath.Join(root, "direct.txt")
	if err := os.WriteFile(rootFile, []byte("direct"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err = openBoundCandidateFile(tx, scope, rootFile)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
}
