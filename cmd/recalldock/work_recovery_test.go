package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"recalldock.local/recalldock/internal/opsstore"
	"recalldock.local/recalldock/internal/worklog"
)

type workRecoveryPage struct {
	Data struct {
		Tasks   []opsstore.Task `json:"tasks"`
		After   string          `json:"after"`
		Before  int64           `json:"before"`
		HasMore *bool           `json:"has_more"`
		Total   *int            `json:"total"`
		Order   string          `json:"order"`
	} `json:"data"`
}

func workRecoveryCall(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var err error
	stdout, _ := captureOutput(t, func() { err = runWork(args) })
	return stdout, err
}

func workRecoveryFind(t *testing.T, args ...string) workRecoveryPage {
	t.Helper()
	body, err := workRecoveryCall(t, args...)
	if err != nil {
		t.Fatal(err)
	}
	var page workRecoveryPage
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatal(err)
	}
	if page.Data.Tasks == nil || page.Data.HasMore == nil || page.Data.Total == nil {
		t.Fatalf("incomplete page metadata: %s", body)
	}
	return page
}

func TestWorkFindOptionsAndMissingStore(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(t.TempDir(), "absent")
	t.Setenv("RECALLDOCK_DATA_DIR", data)
	for _, order := range []string{"recent", "lane"} {
		page := workRecoveryFind(t, "find", "--root", root, "--order", order, "--status", "all", "--limit", "20")
		if len(page.Data.Tasks) != 0 || *page.Data.HasMore || *page.Data.Total != 0 || page.Data.Order != order || page.Data.After != "" {
			t.Fatalf("unexpected empty page: %+v", page.Data)
		}
	}
	page := workRecoveryFind(t, "find", "--root", root)
	if page.Data.Order != "recent" {
		t.Fatalf("default order = %q", page.Data.Order)
	}
	for _, path := range []string{data, filepath.Join(root, ".recalldock")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("read initialized %s: %v", path, err)
		}
	}
}

func TestWorkFindRejectsInvalidOptionsBeforeOpeningStore(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(t.TempDir(), "absent")
	t.Setenv("RECALLDOCK_DATA_DIR", data)
	for _, tail := range [][]string{
		{"--order", "updated"}, {"--order", ""}, {"--status", "delivered"}, {"--status", ""},
		{"--limit", "0"}, {"--limit", "21"}, {"--limit", "1.5"},
		{"--order", "recent", "--order", "lane"}, {"--status", "active", "--status", "all"},
		{"--after", "recent:invalid"}, {"--after", "a"}, {"--order", "lane", "--after", "recent:1"},
		{"--query", strings.Repeat("a", 201)}, {"--event"}, {"--before", "1"},
	} {
		args := append([]string{"find", "--root", root}, tail...)
		if _, err := workRecoveryCall(t, args...); err == nil {
			t.Fatalf("invalid input accepted: %v", args)
		}
	}
	if _, err := workRecoveryCall(t, "read", "--root", root, "--lane", "task", "--order", "recent"); err == nil {
		t.Fatal("read accepted find-only option")
	}
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		t.Fatalf("invalid input initialized data: %v", err)
	}
}

func TestWorkRecoverySearchPagesEventsAndReplay(t *testing.T) {
	root := t.TempDir()
	data := t.TempDir()
	t.Setenv("RECALLDOCK_DATA_DIR", data)
	s, err := opsstore.Open(data, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	cp := opsstore.Checkpoint{Checkpoint: worklog.Checkpoint{Goal: "Alpha task", Summary: "26/26 selected checks", Details: "detail-only needle"}, Status: "active"}
	if _, err := s.Save(root, "a", 0, cp); err != nil {
		t.Fatal(err)
	}
	event := opsstore.Event{ID: "attempt", InputFingerprint: "input-v1", Kind: "attempt", Status: "started", Summary: "Check the uncertain outcome", Details: "selected event detail"}
	if _, err := s.Append(root, "a", 1, event, opsstore.Patch{}); err != nil {
		t.Fatal(err)
	}
	cp.Goal, cp.Summary, cp.Details = "Zulu task", "Other checks", "other details"
	if _, err := s.Save(root, "z", 0, cp); err != nil {
		t.Fatal(err)
	}
	cp.Goal, cp.Status = "Archived probe", "archived"
	if _, err := s.Save(root, "probe", 0, cp); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"26/26", "detail-only"} {
		page := workRecoveryFind(t, "find", "--root", root, "--query", query)
		if len(page.Data.Tasks) != 1 || page.Data.Tasks[0].Lane != "a" || *page.Data.Total != 1 || *page.Data.HasMore {
			t.Fatalf("missing body search hit %q: %+v", query, page.Data)
		}
	}
	for _, tc := range []struct {
		order string
		want  []string
	}{{"recent", []string{"z", "a"}}, {"lane", []string{"a", "z"}}} {
		first := workRecoveryFind(t, "find", "--root", root, "--order", tc.order, "--limit", "1")
		if len(first.Data.Tasks) != 1 || first.Data.Tasks[0].Lane != tc.want[0] || !*first.Data.HasMore || *first.Data.Total != 2 || first.Data.After == "" {
			t.Fatalf("first %s page: %+v", tc.order, first.Data)
		}
		last := workRecoveryFind(t, "find", "--root", root, "--order", tc.order, "--limit", "1", "--after", first.Data.After)
		if len(last.Data.Tasks) != 1 || last.Data.Tasks[0].Lane != tc.want[1] || *last.Data.HasMore || *last.Data.Total != 2 || last.Data.After != "" {
			t.Fatalf("last %s page: %+v", tc.order, last.Data)
		}
	}
	archived := workRecoveryFind(t, "find", "--root", root, "--status", "archived")
	if len(archived.Data.Tasks) != 1 || archived.Data.Tasks[0].Lane != "probe" || *archived.Data.Total != 1 {
		t.Fatalf("archive filter: %+v", archived.Data)
	}
	all := workRecoveryFind(t, "find", "--root", root, "--status", "all", "--limit", "20")
	if len(all.Data.Tasks) != 3 || *all.Data.Total != 3 || *all.Data.HasMore {
		t.Fatalf("all status: %+v", all.Data)
	}
	history := workRecoveryFind(t, "history", "--root", root, "--lane", "a", "--limit", "1")
	if len(history.Data.Tasks) != 1 || history.Data.Tasks[0].Generation != 2 || !*history.Data.HasMore || *history.Data.Total != 2 || history.Data.Before != 2 {
		t.Fatalf("history: %+v", history.Data)
	}
	older := workRecoveryFind(t, "history", "--root", root, "--lane", "a", "--limit", "1", "--before", "2")
	if len(older.Data.Tasks) != 1 || older.Data.Tasks[0].Generation != 1 || *older.Data.HasMore || older.Data.Before != 0 {
		t.Fatalf("last history page: %+v", older.Data)
	}
	body, err := workRecoveryCall(t, "read", "--root", root, "--lane", "a", "--mode", "l2", "--generation", "2", "--event")
	if err != nil || !strings.Contains(body, event.Details) || !strings.Contains(body, event.Summary) || !strings.Contains(body, `"kind":"attempt"`) {
		t.Fatalf("event read: %s %v", body, err)
	}
	input := filepath.Join(t.TempDir(), "event.json")
	content, err := json.Marshal(struct {
		Event opsstore.Event `json:"event"`
	}{event})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(input, content, 0600); err != nil {
		t.Fatal(err)
	}
	body, err = workRecoveryCall(t, "append", "--root", root, "--lane", "a", "--expected", "1", "--input", input)
	if err != nil || !strings.Contains(body, `"replayed":true`) || !strings.Contains(body, `"generation":2`) {
		t.Fatalf("retry response: %s %v", body, err)
	}
	if _, err := os.Stat(filepath.Join(data, "recalldock.db")); !os.IsNotExist(err) {
		t.Fatalf("work opened knowledge database: %v", err)
	}
}

func TestWorkEventSelectionRejectsInvalidModeBeforeOpeningStore(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(t.TempDir(), "absent")
	t.Setenv("RECALLDOCK_DATA_DIR", data)
	for _, tail := range [][]string{
		{"--event"}, {"--event", "--mode", "l1", "--generation", "2"},
		{"--event", "--mode", "l2", "--generation", "0"},
	} {
		args := append([]string{"read", "--root", root, "--lane", "task"}, tail...)
		if _, err := workRecoveryCall(t, args...); err == nil || !strings.Contains(err.Error(), "selected event requires") {
			t.Fatalf("invalid event selection accepted: %v %v", args, err)
		}
	}
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		t.Fatalf("invalid event read opened data: %v", err)
	}
}
