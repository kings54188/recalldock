package opsstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindSearchesRecoveryContentAndHidesArchived(t *testing.T) {
	s, _ := openTestStore(t)
	root := t.TempDir()
	cp := fixture()
	cp.Summary = "Validation 26/26 passed"
	cp.Details = "Selected detail needle"
	cp.Next = []string{"next-step marker"}
	cp.Blockers = []string{"blocker marker"}
	if _, err := s.Save(root, "lane", 0, cp); err != nil {
		t.Fatal(err)
	}
	event := Event{ID: "attempt", InputFingerprint: "input", Status: "reported", Summary: "event summary marker", Details: "event detail marker"}
	if _, err := s.Append(root, "lane", 1, event, Patch{}); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"26/26", "detail needle", "next-step", "blocker marker", "event summary", "event detail", "LANE"} {
		page, err := s.Find(root, FindOptions{Query: query})
		if err != nil || len(page.Tasks) != 1 || page.Total != 1 {
			t.Fatalf("search %q: %+v %v", query, page, err)
		}
		body, err := EncodeTasks(page, false)
		if err != nil || strings.Contains(string(body), "event detail") || strings.Contains(string(body), "26/26") {
			t.Fatalf("search leaked recovery body: %s %v", body, err)
		}
	}
	cp.Status = "archived"
	if _, err := s.Save(root, "archived", 0, cp); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		status string
		count  int
	}{{"", 1}, {"active", 1}, {"archived", 1}, {"all", 2}, {"blocked", 0}} {
		page, err := s.Find(root, FindOptions{Status: tc.status})
		if err != nil || len(page.Tasks) != tc.count || page.Total != tc.count || page.HasMore || page.After != "" {
			t.Fatalf("status %q: %+v %v", tc.status, page, err)
		}
	}
	page, err := s.Find(t.TempDir(), FindOptions{Query: "26/26"})
	if err != nil || len(page.Tasks) != 0 || page.Total != 0 {
		t.Fatalf("scope isolation: %+v %v", page, err)
	}
}

func TestRecentAndLanePagesAreTransparent(t *testing.T) {
	s, _ := openTestStore(t)
	root := t.TempDir()
	for _, lane := range []string{"a", "b", "c"} {
		if _, err := s.Save(root, lane, 0, fixture()); err != nil {
			t.Fatal(err)
		}
	}
	for _, order := range []string{"recent", "lane"} {
		opts := FindOptions{Order: order, Limit: 1}
		var lanes []string
		for i := 0; i < 3; i++ {
			page, err := s.Find(root, opts)
			if err != nil || len(page.Tasks) != 1 || page.Total != 3 || page.HasMore != (i < 2) {
				t.Fatalf("%s page %d: %+v %v", order, i, page, err)
			}
			lanes = append(lanes, page.Tasks[0].Lane)
			if (page.After != "") != page.HasMore {
				t.Fatalf("terminal page has an unnecessary cursor: %+v", page)
			}
			opts.After = page.After
		}
		want := "c,b,a"
		if order == "lane" {
			want = "a,b,c"
		}
		if strings.Join(lanes, ",") != want {
			t.Fatalf("order=%s: %v", order, lanes)
		}
	}
	if _, err := s.Save(root, "a", 1, fixture()); err != nil {
		t.Fatal(err)
	}
	page, err := s.Find(root, FindOptions{Limit: 1})
	if err != nil || page.Tasks[0].Lane != "a" {
		t.Fatalf("updated task is not recent: %+v %v", page, err)
	}
	for _, opts := range []FindOptions{{Order: "bad"}, {Status: "bad"}, {Order: "recent", After: "a"}, {Order: "lane", After: "recent:1"}, {Limit: 21}, {Limit: -1}} {
		if _, err := s.Find(root, opts); err == nil {
			t.Fatalf("invalid options accepted: %+v", opts)
		}
	}
}

func TestHistoryHasMoreAndBoundedPagesRetainContinuation(t *testing.T) {
	s, _ := openTestStore(t)
	root := t.TempDir()
	for i := int64(0); i < 3; i++ {
		if _, err := s.Save(root, "lane", i, fixture()); err != nil {
			t.Fatal(err)
		}
	}
	before := int64(0)
	for i := 0; i < 3; i++ {
		page, err := s.History(root, "lane", before, 1)
		if err != nil || len(page.Tasks) != 1 || page.Total != 3 || page.HasMore != (i < 2) {
			t.Fatalf("history %d: %+v %v", i, page, err)
		}
		if page.Tasks[0].Generation != int64(3-i) || (page.Before != 0) != page.HasMore {
			t.Fatalf("history cursor: %+v", page)
		}
		before = page.Before
	}
	longRoot := "/" + strings.Repeat("r", 800)
	tasks := []Task{}
	for _, lane := range []string{"a", "b", "c"} {
		tasks = append(tasks, Task{Root: longRoot, Lane: lane, ID: int64(len(tasks) + 1), Goal: strings.Repeat("g", 200)})
	}
	body, err := EncodeTasks(TaskPage{Tasks: tasks, Total: 3, Order: "lane"}, false)
	if err != nil || len(body) > SummaryLimit {
		t.Fatalf("bounded encode: %d %v", len(body), err)
	}
	var envelope struct {
		Truncated bool     `json:"truncated"`
		Data      TaskPage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.Truncated || !envelope.Data.HasMore || envelope.Data.After != envelope.Data.Tasks[len(envelope.Data.Tasks)-1].Lane || envelope.Data.Total != 3 {
		t.Fatalf("budget truncation lost continuation: %s", body)
	}
}

func TestUnresolvedOperationsSurviveWindowAndCheckpointReplacement(t *testing.T) {
	for _, status := range []string{"started", "applied", "needs_check"} {
		t.Run(status, func(t *testing.T) {
			s, _ := openTestStore(t)
			root := t.TempDir()
			if _, err := s.Save(root, "lane", 0, fixture()); err != nil {
				t.Fatal(err)
			}
			pending := Event{ID: "pending", InputFingerprint: "pending-v1", Status: status, Summary: "Reconcile the pending outcome"}
			if _, err := s.Append(root, "lane", 1, pending, Patch{}); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 12; i++ {
				event := Event{ID: fmt.Sprintf("report-%d", i), InputFingerprint: fmt.Sprintf("input-%d", i), Status: "reported", Summary: "Unrelated progress"}
				if _, err := s.Append(root, "lane", int64(i+2), event, Patch{}); err != nil {
					t.Fatal(err)
				}
			}
			check := func(generation int64) Snapshot {
				t.Helper()
				snap, err := s.Read(root, "lane", generation)
				if err != nil || !snap.Task().NeedsCheck {
					t.Fatalf("pending operation disappeared at generation %d: %+v %v", generation, snap.Task(), err)
				}
				for _, mode := range []string{"l0", "l1", "l2"} {
					body, err := EncodeSnapshot(snap, mode)
					if err != nil || !strings.Contains(string(body), `"needs_check":true`) {
						t.Fatalf("%s lost pending state: %s %v", mode, body, err)
					}
				}
				return snap
			}
			old := check(14)
			if len(old.Checkpoint.Operations) > 12 {
				t.Fatal("operation display exceeded its bounded window")
			}
			page, err := s.Find(root, FindOptions{})
			if err != nil || len(page.Tasks) != 1 || !page.Tasks[0].NeedsCheck {
				t.Fatalf("recovery index lost pending state: %+v %v", page, err)
			}
			if _, err := s.Save(root, "lane", 14, fixture()); err != nil {
				t.Fatal(err)
			}
			check(15)
			pending.Status, pending.Evidence = "verified", "selected-evidence.json"
			resolved, err := s.Append(root, "lane", 15, pending, Patch{})
			if err != nil || resolved.Task().NeedsCheck {
				t.Fatalf("explicit resolution retained pending state: %+v %v", resolved.Task(), err)
			}
			current, err := s.Read(root, "lane", 0)
			if err != nil || current.Task().NeedsCheck {
				t.Fatalf("current state disagrees with resolution: %+v %v", current.Task(), err)
			}
			check(14)
			pending.Status, pending.Evidence = status, ""
			replay, err := s.Append(root, "lane", 1, pending, Patch{})
			if err != nil || !replay.Replayed || replay.Generation != 2 || !replay.Task().NeedsCheck {
				t.Fatalf("retry rewrote historical pending state: %+v %v", replay, err)
			}
			history, err := s.History(root, "lane", 0, 20)
			if err != nil || history.Tasks[0].NeedsCheck || !history.Tasks[1].NeedsCheck {
				t.Fatalf("history does not preserve per-generation state: %+v %v", history, err)
			}
		})
	}
}

func TestLegacyCurrentPendingStateDoesNotRewriteHistory(t *testing.T) {
	s, _ := openTestStore(t)
	root := t.TempDir()
	if _, err := s.Save(root, "lane", 0, fixture()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(root, "lane", 1, Event{ID: "pending", InputFingerprint: "input", Status: "started", Summary: "Unknown outcome"}, Patch{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(root, "lane", 2, fixture()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE checkpoints SET needs_check=0 WHERE generation=3"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE tasks SET needs_check=0"); err != nil {
		t.Fatal(err)
	}
	var before string
	if err := s.db.QueryRow("SELECT body FROM checkpoints WHERE generation=3").Scan(&before); err != nil {
		t.Fatal(err)
	}
	current, err := s.Read(root, "lane", 0)
	if err != nil || !current.Task().NeedsCheck {
		t.Fatalf("legacy current state lost its pending operation: %+v %v", current, err)
	}
	page, err := s.Find(root, FindOptions{})
	if err != nil || !page.Tasks[0].NeedsCheck {
		t.Fatalf("legacy recovery index lost its pending operation: %+v %v", page, err)
	}
	historical, err := s.Read(root, "lane", 3)
	if err != nil || historical.Task().NeedsCheck {
		t.Fatalf("historical report was silently reinterpreted: %+v %v", historical, err)
	}
	var after string
	var needsCheck bool
	if err := s.db.QueryRow("SELECT body,needs_check FROM checkpoints WHERE generation=3").Scan(&after, &needsCheck); err != nil || before != after || needsCheck {
		t.Fatalf("read repaired immutable history: %v", err)
	}
}

func TestReplayAndActionableErrors(t *testing.T) {
	s, _ := openTestStore(t)
	root := t.TempDir()
	if _, err := s.Save(root, "lane", 0, fixture()); err != nil {
		t.Fatal(err)
	}
	event := Event{ID: "test", InputFingerprint: "input", Status: "reported", Summary: "summary"}
	if _, err := s.Append(root, "lane", 1, event, Patch{}); err != nil {
		t.Fatal(err)
	}
	replay, err := s.Append(root, "lane", 1, event, Patch{})
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"l0", "l1", "l2"} {
		body, err := EncodeSnapshot(replay, mode)
		if err != nil || !strings.Contains(string(body), `"replayed":true`) {
			t.Fatalf("replay marker missing at %s: %s %v", mode, body, err)
		}
	}
	for _, appendEvent := range []bool{false, true} {
		var err error
		if appendEvent {
			event.ID = "other"
			_, err = s.Append(root, "lane", 1, event, Patch{})
		} else {
			_, err = s.Save(root, "lane", 1, fixture())
		}
		if !errors.Is(err, ErrGeneration) || !strings.Contains(err.Error(), "expected=1") || !strings.Contains(err.Error(), "current=2") {
			t.Fatalf("unhelpful CAS conflict: %v", err)
		}
	}
	if _, err := s.Read(root, "missing", 0); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "work_find") {
		t.Fatalf("missing lane has no recovery action: %v", err)
	}
	if _, err := s.Find(filepath.Join(root, "absent"), FindOptions{}); err == nil || !strings.Contains(err.Error(), "root does not exist") {
		t.Fatalf("missing root error: %v", err)
	}
}
