package opsstore

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"recalldock.local/recalldock/internal/worklog"
)

func TestFirstSaveFailureDoesNotPoisonWorkspace(t *testing.T) {
	for _, stage := range []string{"workspace", "checkpoint", "commit"} {
		t.Run(stage, func(t *testing.T) {
			s, _ := openTestStore(t)
			root := t.TempDir()
			trigger := "CREATE TRIGGER write_failure BEFORE INSERT ON workspaces BEGIN SELECT RAISE(ABORT,'injected failure'); END"
			if stage == "checkpoint" {
				trigger = "CREATE TRIGGER write_failure BEFORE INSERT ON checkpoints BEGIN SELECT RAISE(ABORT,'injected failure'); END"
			}
			if stage == "commit" {
				if _, err := s.db.Exec("CREATE TABLE deferred_failure (workspace TEXT REFERENCES workspaces(id) DEFERRABLE INITIALLY DEFERRED)"); err != nil {
					t.Fatal(err)
				}
				trigger = "CREATE TRIGGER write_failure AFTER INSERT ON checkpoints BEGIN INSERT INTO deferred_failure VALUES('missing'); END"
			}
			if _, err := s.db.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Save(root, "lane", 0, fixture()); err == nil {
				t.Fatal("injected failure did not fire")
			}
			marker := filepath.Join(root, ".recalldock", "ops-root.json")
			if _, err := os.Lstat(marker); !os.IsNotExist(err) {
				t.Fatalf("failed %s retained an orphan marker: %v", stage, err)
			}
			if info, err := os.Stat(filepath.Dir(marker)); err != nil || !info.IsDir() {
				t.Fatalf("cleanup removed the user directory: %v", err)
			}
			for _, table := range []string{"workspaces", "tasks", "checkpoints"} {
				var count int
				if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
					t.Fatalf("failed %s retained %s rows: %d %v", stage, table, count, err)
				}
			}
			if _, err := s.db.Exec("DROP TRIGGER write_failure"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Save(root, "lane", 0, fixture()); err != nil {
				t.Fatalf("valid retry failed after %s: %v", stage, err)
			}
		})
	}
}

func longDeliveryRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	var parts []string
	if runtime.GOOS == "windows" {
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			root = resolved
		}
		parts = []string{strings.Repeat("&", 80) + strings.Repeat("a", 120), strings.Repeat("b", 200), strings.Repeat("c", 180), strings.Repeat("d", 180)}
	} else {
		parts = []string{strings.Repeat(`"`, 200), strings.Repeat(`"`, 200), strings.Repeat("a", 180), strings.Repeat("b", 180)}
	}
	for _, part := range parts {
		root = filepath.Join(root, part)
	}
	root = filepath.Join(root, strings.Repeat("c", 1000-len(root)-1))
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestUndeliverableWriterRejectedBeforePersistence(t *testing.T) {
	s, _ := openTestStore(t)
	root := longDeliveryRoot(t)
	id := "codex-" + strings.Repeat("h", 186)
	if err := s.RegisterHostSession(id, "codex", root); err != nil {
		t.Fatal(err)
	}
	bound := s.WithWriter(Writer{Host: "codex", HostSessionID: id, GatewaySessionID: "gateway-" + strings.Repeat("f", 32)})
	cp := Checkpoint{Checkpoint: worklog.Checkpoint{Goal: strings.Repeat("g", 300)}, Status: "active"}
	lane := strings.Repeat("l", 80)
	for _, selected := range []string{root, filepath.Join(t.TempDir(), "alias")} {
		if selected != root {
			if err := os.Symlink(root, selected); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := bound.Save(selected, lane, 0, cp); !errors.Is(err, ErrBudget) {
			t.Fatalf("unreadable writer accepted through %q: %v", selected, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, ".recalldock")); !os.IsNotExist(err) {
		t.Fatalf("budget rejection created workspace: %v", err)
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM checkpoints").Scan(&count); err != nil || count != 0 {
		t.Fatalf("budget rejection persisted checkpoint: %d %v", count, err)
	}
}

func TestAppendDeliveryBudgetFailurePreservesCurrentState(t *testing.T) {
	s, _ := openTestStore(t)
	root := longDeliveryRoot(t)
	cp := Checkpoint{Checkpoint: worklog.Checkpoint{Goal: strings.Repeat("g", 300)}, Status: "active"}
	lane := strings.Repeat("l", 80)
	first, err := s.Save(root, lane, 0, cp)
	if err != nil {
		t.Fatal(err)
	}
	id := "codex-" + strings.Repeat("h", 186)
	if err := s.RegisterHostSession(id, "codex", root); err != nil {
		t.Fatal(err)
	}
	bound := s.WithWriter(Writer{Host: "codex", HostSessionID: id, GatewaySessionID: "gateway-" + strings.Repeat("f", 32)})
	event := Event{ID: "attempt", InputFingerprint: "input", Status: "reported", Summary: "attempt"}
	if _, err := bound.Append(root, lane, 1, event, Patch{}); !errors.Is(err, ErrBudget) {
		t.Fatalf("unreadable append accepted: %v", err)
	}
	current, err := s.Read(root, lane, 0)
	if err != nil || current.Generation != first.Generation || current.Writer != nil {
		t.Fatalf("rejected append changed state: %+v %v", current, err)
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM events").Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected append retained event: %d %v", count, err)
	}
}

func TestEventEnvelopeBudgetRejectsBeforePersistence(t *testing.T) {
	for _, escaped := range []string{"\t", "\n", "\\", `"`} {
		t.Run(escaped, func(t *testing.T) {
			s, _ := openTestStore(t)
			root := t.TempDir()
			if _, err := s.Save(root, "lane", 0, fixture()); err != nil {
				t.Fatal(err)
			}
			event := Event{ID: "escaped", InputFingerprint: "escaped-v1", Status: "started", Summary: strings.Repeat("s", 600), Details: strings.Repeat(escaped, 6000)}
			if err := validateEvent(event); err != nil {
				t.Fatalf("fixture violates field limits: %v", err)
			}
			if _, err := s.Append(root, "lane", 1, event, Patch{}); !errors.Is(err, ErrBudget) {
				t.Fatalf("unreadable event accepted: %v", err)
			}
			current, err := s.Read(root, "lane", 0)
			if err != nil || current.Generation != 1 || current.Task().NeedsCheck {
				t.Fatalf("rejected event changed state: %+v %v", current, err)
			}
			for _, table := range []string{"events", "operations"} {
				var count int
				if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
					t.Fatalf("rejection retained %s: %d %v", table, count, err)
				}
			}
			var count int
			if err := s.db.QueryRow("SELECT count(*) FROM checkpoints").Scan(&count); err != nil || count != 1 {
				t.Fatalf("rejection persisted a checkpoint: %d %v", count, err)
			}
		})
	}
}

func TestEventEnvelopeBoundaryIncludesPatchAndReplays(t *testing.T) {
	s, _ := openTestStore(t)
	root := t.TempDir()
	if _, err := s.Save(root, "lane", 0, fixture()); err != nil {
		t.Fatal(err)
	}
	summary := strings.Repeat("p", 700)
	patch := Patch{Summary: &summary}
	event := Event{ID: "boundary", InputFingerprint: "boundary-v1", Status: "reported", Summary: strings.Repeat("s", 600)}
	encoded := func(n int) json.RawMessage {
		event.Details = strings.Repeat("\t", n)
		body, err := json.Marshal(struct {
			Event Event `json:"event"`
			Patch Patch `json:"patch"`
		}{event, patch})
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	low, high := 0, 6000
	for low < high {
		mid := (low + high + 1) / 2
		if _, err := Encode(encoded(mid), "l2", DetailLimit); err == nil {
			low = mid
		} else if errors.Is(err, ErrBudget) {
			high = mid - 1
		} else {
			t.Fatal(err)
		}
	}
	valid := encoded(low)
	if _, err := s.Append(root, "lane", 1, event, patch); err != nil {
		t.Fatalf("readable boundary rejected: %v", err)
	}
	selected, err := s.Event(root, "lane", 2)
	if err != nil || string(selected) != string(valid) {
		t.Fatalf("event changed on readback: %v", err)
	}
	if _, err := Encode(selected, "l2", DetailLimit); err != nil {
		t.Fatal(err)
	}
	if replay, err := s.Append(root, "lane", 1, event, patch); err != nil || !replay.Replayed || replay.Generation != 2 {
		t.Fatalf("boundary replay failed: %+v %v", replay, err)
	}
	encoded(low + 1)
	if _, err := s.Append(root, "lane", 2, event, patch); !errors.Is(err, ErrBudget) {
		t.Fatalf("oversized boundary accepted: %v", err)
	}
	current, err := s.Read(root, "lane", 0)
	if err != nil || current.Generation != 2 {
		t.Fatalf("budget rejection advanced generation: %+v %v", current, err)
	}
}

func TestEventInsertFailureRollsBackAllState(t *testing.T) {
	s, _ := openTestStore(t)
	root := t.TempDir()
	if _, err := s.Save(root, "lane", 0, fixture()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_event BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT,'event failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(root, "lane", 1, Event{ID: "pending", InputFingerprint: "input", Status: "started", Summary: "Uncertain operation"}, Patch{}); err == nil {
		t.Fatal("injected event failure did not fire")
	}
	for _, table := range []string{"events", "operations"} {
		var count int
		if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("failed event retained %s: %d %v", table, count, err)
		}
	}
	current, err := s.Read(root, "lane", 0)
	if err != nil || current.Generation != 1 || current.Task().NeedsCheck {
		t.Fatalf("failed event advanced current state: %+v %v", current, err)
	}
}

func TestOldOversizedEventReplaysWithoutRewritingHistory(t *testing.T) {
	s, _ := openTestStore(t)
	root := t.TempDir()
	if _, err := s.Save(root, "lane", 0, fixture()); err != nil {
		t.Fatal(err)
	}
	event := Event{ID: "legacy", InputFingerprint: "legacy-v1", Status: "reported", Summary: "Legacy event"}
	if _, err := s.Append(root, "lane", 1, event, Patch{}); err != nil {
		t.Fatal(err)
	}
	event.Summary, event.Details = strings.Repeat("s", 600), strings.Repeat("\t", 6000)
	body, _ := json.Marshal(struct {
		Event Event `json:"event"`
		Patch Patch `json:"patch"`
	}{event, Patch{}})
	if _, err := s.db.Exec("UPDATE events SET body=?,digest=? WHERE id='legacy'", string(body), digest(body)); err != nil {
		t.Fatal(err)
	}
	replay, err := s.Append(root, "lane", 1, event, Patch{})
	if err != nil || !replay.Replayed || replay.Generation != 2 {
		t.Fatalf("old committed event was treated as a new write: %+v %v", replay, err)
	}
	selected, err := s.Event(root, "lane", 2)
	if err != nil || string(selected) != string(body) {
		t.Fatalf("legacy event was rewritten: %v", err)
	}
	if _, err := Encode(selected, "l2", DetailLimit); !errors.Is(err, ErrBudget) {
		t.Fatalf("legacy limit silently changed: %v", err)
	}
}

func TestNewWritesReserveDeliverySpaceForExactReplay(t *testing.T) {
	s, _ := openTestStore(t)
	root := t.TempDir()
	bound := s.WithWriter(Writer{Host: "codex", GatewaySessionID: "gateway-synthetic"})
	if _, err := bound.Save(root, "lane", 0, fixture()); err != nil {
		t.Fatal(err)
	}
	event := Event{ID: "attempt", InputFingerprint: "input", Status: "reported", Summary: "attempt"}
	applied, err := bound.Append(root, "lane", 1, event, Patch{})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := bound.Append(root, "lane", 1, event, Patch{})
	if err != nil || !replay.Replayed || replay.Generation != applied.Generation {
		t.Fatalf("exact retry: %+v %v", replay, err)
	}
	for _, mode := range []string{"l0", "l1", "l2"} {
		if body, err := EncodeSnapshot(replay, mode); err != nil || !json.Valid(body) {
			t.Fatalf("replay is undeliverable at %s: %v", mode, err)
		}
	}
}

func TestWorkspaceMarkerCleanupPreservesChanges(t *testing.T) {
	for _, change := range []string{"content", "replacement", "directory", "mode", "symlink", "committed"} {
		t.Run(change, func(t *testing.T) {
			s, _ := openTestStore(t)
			root := t.TempDir()
			identity, err := directoryIdentity(root)
			if err != nil {
				t.Fatal(err)
			}
			conn, err := s.db.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			tx, err := conn.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			_, marker, err := s.createWorkspace(tx, root, identity)
			if err != nil {
				t.Fatal(err)
			}
			if change == "committed" {
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			} else if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, ".recalldock", "ops-root.json")
			body := marker.body
			switch change {
			case "content":
				body = []byte("later manual content")
				if err := os.WriteFile(path, body, 0600); err != nil {
					t.Fatal(err)
				}
			case "replacement", "symlink":
				if err := os.Rename(path, path+".original"); err != nil {
					t.Fatal(err)
				}
				if change == "replacement" {
					if err := os.WriteFile(path, body, 0600); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Symlink(path+".original", path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				dir := filepath.Dir(path)
				if err := os.Rename(dir, dir+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, body, 0600); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(path, 0400); err != nil {
					t.Fatal(err)
				}
			}
			if err := marker.cleanup(conn); err != nil {
				t.Fatal(err)
			}
			actual, err := os.ReadFile(path)
			if err != nil || string(actual) != string(body) {
				t.Fatalf("cleanup deleted %s marker: %v", change, err)
			}
		})
	}
}

func TestWorkspaceMarkerWriteFailureCanCleanOwnedEmptyFile(t *testing.T) {
	s, _ := openTestStore(t)
	root := t.TempDir()
	identity, err := directoryIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ".recalldock")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ops-root.json")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	fileInfo, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	directoryInfo, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	marker := &workspaceMarker{root: root, identity: identity, id: "ws-synthetic", fileInfo: fileInfo, directoryInfo: directoryInfo}
	f.Close()
	if err := marker.write(f, []byte("synthetic body")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("write failure did not fire: %v", err)
	}
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := marker.cleanup(conn); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("owned failed write was not cleaned: %v", err)
	}
}

func TestExistingWorkspaceMarkerIsNeverRemovedByRejectedWrite(t *testing.T) {
	s, _ := openTestStore(t)
	root := t.TempDir()
	dir := filepath.Join(root, ".recalldock")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ops-root.json")
	body := []byte("existing manual marker")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(root, "lane", 0, fixture()); !errors.Is(err, ErrIdentity) {
		t.Fatalf("existing invalid marker accepted: %v", err)
	}
	actual, err := os.ReadFile(path)
	if err != nil || string(actual) != string(body) {
		t.Fatalf("rejected write removed existing marker: %v", err)
	}
}

func TestOldUndeliverableHistoryIsNotRewritten(t *testing.T) {
	s, _ := openTestStore(t)
	root := longDeliveryRoot(t)
	identity, err := directoryIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	id := "codex-" + strings.Repeat("h", 186)
	if err := s.RegisterHostSession(id, "codex", root); err != nil {
		t.Fatal(err)
	}
	legacy := s.WithWriter(Writer{Host: "codex", HostSessionID: id, GatewaySessionID: "gateway-" + strings.Repeat("f", 32)})
	cp := Checkpoint{Checkpoint: worklog.Checkpoint{Goal: strings.Repeat("g", 300)}, Status: "active"}
	lane := strings.Repeat("l", 80)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	workspace, _, err := s.createWorkspace(tx, root, identity)
	if err != nil {
		t.Fatal(err)
	}
	old, err := legacy.persist(tx, workspace, root, lane, 1, cp)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Save(root, lane, 1, cp); !errors.Is(err, ErrBudget) {
		t.Fatalf("legacy unreadable metadata accepted for a new write: %v", err)
	}
	current, err := s.Read(root, lane, 1)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := EncodeSnapshot(old, "l2")
	after, _ := EncodeSnapshot(current, "l2")
	if string(before) != string(after) {
		t.Fatal("new validation rewrote legacy checkpoint data")
	}
}

func TestBudgetReservesReplayAtL2Boundary(t *testing.T) {
	s, _ := openTestStore(t)
	root := t.TempDir()
	writer := Writer{Host: "codex", GatewaySessionID: "gateway-synthetic"}
	cp := fixture()
	found := false
	for n := DetailLimit; n > DetailLimit-2000; n-- {
		cp.Details = strings.Repeat("x", n)
		if worklog.ValidateCheckpoint(root, cp.Checkpoint) != nil {
			continue
		}
		snap := Snapshot{Root: root, Lane: "lane", ID: 9223372036854775807, Generation: 9223372036854775807, UpdatedAt: "2000-01-01T00:00:00.000000000Z", Checkpoint: cp, Writer: &writer}
		if _, err := EncodeSnapshot(snap, "l2"); err != nil {
			continue
		}
		snap.Replayed = true
		if _, err := EncodeSnapshot(snap, "l2"); errors.Is(err, ErrBudget) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("failed to construct replay budget boundary")
	}
	if _, err := s.WithWriter(writer).Save(root, "lane", 0, cp); !errors.Is(err, ErrBudget) {
		t.Fatalf("replay-undeliverable input accepted: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, ".recalldock")); !os.IsNotExist(err) {
		t.Fatalf("replay budget rejection created a marker: %v", err)
	}
}

func TestWorkspaceCleanupDoesNotDeleteConcurrentWinner(t *testing.T) {
	s, dir := openTestStore(t)
	root := t.TempDir()
	var wins atomic.Int32
	var group sync.WaitGroup
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			other, err := Open(dir, true)
			if err != nil {
				t.Error(err)
				return
			}
			defer other.Close()
			if _, err := other.Save(root, "lane", 0, fixture()); err == nil {
				wins.Add(1)
			} else if !errors.Is(err, ErrGeneration) {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	if wins.Load() != 1 {
		t.Fatalf("concurrent winners: %d", wins.Load())
	}
	if _, err := s.Read(root, "lane", 1); err != nil {
		t.Fatalf("cleanup removed committed winner: %v", err)
	}
}
