package opsstore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"recalldock.local/recalldock/internal/worklog"
)

func fixture() Checkpoint {
	return Checkpoint{
		Checkpoint: worklog.Checkpoint{Goal: "Synthetic task", Summary: "Checked current files", Next: []string{"Run the selected test"}, Details: "Selected detail, not long-term knowledge"},
		Status:     "active",
	}
}

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir
}

func TestCatalogExactToolSelectionPreservesServerIsolation(t *testing.T) {
	s, _ := openTestStore(t)
	for _, server := range []string{"first", "second"} {
		tool := ToolRecord{Server: server, Name: "selected", ConfigHash: strings.Repeat("a", 64), SchemaHash: strings.Repeat("b", 64), Summary: server + " selected schema", SchemaJSON: `{"name":"selected","inputSchema":{"type":"object"}}`}
		other := tool
		other.Name, other.SchemaJSON = "unselected", strings.Repeat(" ", 60<<10)
		if err := s.SaveCatalog(server, tool.ConfigHash, []ToolRecord{tool, other}); err != nil {
			t.Fatal(err)
		}
		selected, err := s.LoadCatalogTool(server, "selected")
		if err != nil || selected != tool {
			t.Fatalf("exact catalog selection changed the tool: %+v %v", selected, err)
		}
	}
	for _, scope := range [][2]string{{"first", "missing"}, {"missing", "selected"}, {"first", "selected' OR 1=1"}} {
		if _, err := s.LoadCatalogTool(scope[0], scope[1]); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("catalog selection widened: %v", err)
		}
	}
}

func TestIndependentStoreAndThreeLevels(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	if _, err := Open(dir, false); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read should not initialize: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("read created a directory")
	}
	s, dir := openTestStore(t)
	root := t.TempDir()
	cp := fixture()
	cp.Details = strings.Repeat("selected-detail ", 400)
	snap, err := s.Save(root, "lane", 0, cp)
	if err != nil || snap.Generation != 1 {
		t.Fatalf("save: %+v %v", snap, err)
	}
	for _, mode := range []string{"l0", "l1", "l2"} {
		body, err := EncodeSnapshot(snap, mode)
		if err != nil {
			t.Fatal(err)
		}
		limit := SummaryLimit
		if mode == "l2" {
			limit = DetailLimit
		}
		if len(body) > limit || !json.Valid(body) {
			t.Fatalf("invalid %s envelope: %d bytes", mode, len(body))
		}
		if (mode == "l2") != strings.Contains(string(body), "selected-detail") {
			t.Fatalf("details leaked or missing at %s", mode)
		}
		if mode == "l0" && strings.Contains(string(body), cp.Summary) {
			t.Fatal("summary leaked into index")
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "recalldock.db")); !os.IsNotExist(err) {
		t.Fatal("operations created long-term database")
	}
	if _, err := s.Read(root, "other", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-lane read: %v", err)
	}
	if _, err := s.Read(t.TempDir(), "lane", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-root read: %v", err)
	}
}

func TestCheckpointCASAndImmutableHistory(t *testing.T) {
	s, dir := openTestStore(t)
	root := t.TempDir()
	first, err := s.Save(root, "lane", 0, fixture())
	if err != nil {
		t.Fatal(err)
	}
	var wins atomic.Int32
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			other, err := Open(dir, true)
			if err != nil {
				t.Error(err)
				return
			}
			defer other.Close()
			cp := fixture()
			cp.Summary = "Second checkpoint"
			_, err = other.Save(root, "lane", 1, cp)
			if err == nil {
				wins.Add(1)
			} else if !errors.Is(err, ErrGeneration) {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	if wins.Load() != 1 {
		t.Fatalf("expected one writer, got %d", wins.Load())
	}
	old, err := s.Read(root, "lane", first.Generation)
	if err != nil || old.Checkpoint.Summary != fixture().Summary {
		t.Fatalf("immutable selection changed: %+v %v", old, err)
	}
	history, err := s.History(root, "lane", 0, 1)
	if err != nil || len(history.Tasks) != 1 || history.Tasks[0].Generation != 2 {
		t.Fatalf("history: %+v %v", history, err)
	}
	older, err := s.History(root, "lane", history.Tasks[0].Generation, 1)
	if err != nil || len(older.Tasks) != 1 || older.Tasks[0].Generation != 1 {
		t.Fatalf("history page: %+v %v", older, err)
	}
}

func TestDirectoryIdentityRejectsCopyMoveRecreationAndForeignStore(t *testing.T) {
	for _, scenario := range []string{"move", "copy", "recreate", "foreign"} {
		t.Run(scenario, func(t *testing.T) {
			s, _ := openTestStore(t)
			root := filepath.Join(t.TempDir(), "root")
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Save(root, "lane", 0, fixture()); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(filepath.Dir(root), "other")
			switch scenario {
			case "move":
				if err := os.Rename(root, target); err != nil {
					t.Fatal(err)
				}
			case "copy":
				if err := os.MkdirAll(filepath.Join(target, ".recalldock"), 0700); err != nil {
					t.Fatal(err)
				}
				body, err := os.ReadFile(filepath.Join(root, ".recalldock", "ops-root.json"))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(target, ".recalldock", "ops-root.json"), body, 0600); err != nil {
					t.Fatal(err)
				}
			case "recreate":
				if err := os.Rename(root, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
				target = root
			case "foreign":
				s, _ = openTestStore(t)
				target = root
			}
			if _, err := s.Read(target, "lane", 0); !errors.Is(err, ErrIdentity) {
				t.Fatalf("unsafe identity accepted: %v", err)
			}
			if _, err := s.Save(target, "new-lane", 0, fixture()); !errors.Is(err, ErrIdentity) {
				t.Fatalf("unsafe write accepted: %v", err)
			}
		})
	}
}

func TestAppendIdempotencyAndUncertainOperations(t *testing.T) {
	s, _ := openTestStore(t)
	root := t.TempDir()
	if _, err := s.Save(root, "lane", 0, fixture()); err != nil {
		t.Fatal(err)
	}
	event := Event{ID: "test-run", InputFingerprint: "input-v1", Status: "started", Summary: "Starting synthetic validation"}
	started, err := s.Append(root, "lane", 1, event, Patch{})
	if err != nil || started.Generation != 2 {
		t.Fatalf("append: %+v %v", started, err)
	}
	replayed, err := s.Append(root, "lane", 1, event, Patch{})
	if err != nil || replayed.Generation != 2 {
		t.Fatalf("retry duplicated event: %+v %v", replayed, err)
	}
	body, err := EncodeSnapshot(started, "l0")
	if err != nil || !strings.Contains(string(body), "needs_check") {
		t.Fatalf("unknown work silently replayable: %s %v", body, err)
	}
	event.InputFingerprint = "different-input"
	if _, err := s.Append(root, "lane", 2, event, Patch{}); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("operation input silently changed: %v", err)
	}
	event.InputFingerprint = "input-v1"
	event.Status = "verified"
	event.Evidence = "synthetic-report.json"
	event.Summary = "Synthetic validation passed"
	cp, err := s.Append(root, "lane", 2, event, Patch{})
	if err != nil || cp.Generation != 3 {
		t.Fatalf("completion: %+v %v", cp, err)
	}
	if cp.Checkpoint.Operations[0].Status != "verified" {
		t.Fatal("current operation not updated")
	}
}

func TestOperationalContentAndBackup(t *testing.T) {
	s, _ := openTestStore(t)
	root := t.TempDir()
	cp := fixture()
	cp.Status = "blocked"
	cp.Blockers = []string{"Dependency unavailable"}
	cp.Details = "Hypothesis: the failed attempt may be caused by a version mismatch. Not verified."
	if _, err := s.Save(root, "lane", 0, cp); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"-----BEGIN PRIVATE KEY-----", "user: full conversation\nassistant: raw answer", strings.Repeat("x", DetailLimit+1)} {
		bad := fixture()
		bad.Details = text
		if _, err := s.Save(root, "lane", 1, bad); err == nil {
			t.Fatal("unsafe/oversized progress accepted")
		}
	}
	backup, err := s.Backup()
	if err != nil || backup.Integrity != "ok" || backup.StoreID == "" || backup.SHA256 == "" {
		t.Fatalf("backup: %+v %v", backup, err)
	}
}
