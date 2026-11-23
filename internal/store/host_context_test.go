package store

import (
	"fmt"
	"strings"
	"testing"
)

func TestHostContextIsBoundedAndSkipsStaleAndArchived(t *testing.T) {
	cfg, err := DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = t.TempDir()
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.CreateSession("host-ctx-session", "ctx-proj", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	add := func(title, content string) int64 {
		id, err := s.AddObservation(AddObservationParams{SessionID: "host-ctx-session", Type: "decision", Title: title, Content: content, Project: "ctx-proj"})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	pinnedID := add("pinned durable decision", "body")
	if err := s.PinObservation(pinnedID); err != nil {
		t.Fatal(err)
	}
	add("active reviewed decision", "body")
	staleID := add("stale decision that must not load", "body")
	if _, err := s.db.Exec(`UPDATE observations SET stale_at = ? WHERE id = ?`, "2000-01-01 00:00:00", staleID); err != nil {
		t.Fatal(err)
	}
	overdueID := add("overdue but still valid decision", "body")
	if _, err := s.db.Exec(`UPDATE observations SET review_after = ? WHERE id = ?`, "2000-01-01 00:00:00", overdueID); err != nil {
		t.Fatal(err)
	}
	archivedID := add("archived decision that must not load", "body")
	selected, err := s.SelectObservationForEdit(archivedID)
	if err != nil {
		t.Fatal(err)
	}
	flag := true
	if _, err := s.UpdateObservationCAS(archivedID, selected.SnapshotSHA256, UpdateObservationParams{Archived: &flag}, "host context test", ""); err != nil {
		t.Fatal(err)
	}

	resp, err := s.HostContext("ctx-proj", 300)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Budget.Used > resp.Budget.Limit {
		t.Fatalf("budget exceeded: %+v", resp.Budget)
	}
	if resp.Budget.Entries < 2 {
		t.Fatalf("expected both active memories, got %d", resp.Budget.Entries)
	}
	for _, want := range []string{"pinned durable decision", "active reviewed decision", "overdue but still valid decision", "bounded index"} {
		if !strings.Contains(resp.Text, want) {
			t.Fatalf("context missing %q: %s", want, resp.Text)
		}
	}
	for _, banned := range []string{"stale decision", "archived decision"} {
		if strings.Contains(resp.Text, banned) {
			t.Fatalf("context leaked %q: %s", banned, resp.Text)
		}
	}
	if strings.Index(resp.Text, "pinned durable decision") > strings.Index(resp.Text, "active reviewed decision") {
		t.Fatal("pinned memory should be listed before recent ones")
	}

	small, err := s.HostContext("ctx-proj", 40)
	if err != nil {
		t.Fatal(err)
	}
	if small.Budget.Used > small.Budget.Limit {
		t.Fatalf("small budget exceeded: %+v", small.Budget)
	}
	for budget := 1; budget <= 300; budget++ {
		result, err := s.HostContext("ctx-proj", budget)
		if err != nil {
			t.Fatal(err)
		}
		if result.Budget.Used > result.Budget.Limit {
			t.Fatalf("budget %d exceeded: %+v", budget, result.Budget)
		}
	}
	if empty, err := s.HostContext("", 300); err != nil || empty.Text != "" {
		t.Fatalf("empty project should return no context: %+v %v", empty, err)
	}
}

func TestHostContextIncludesOlderPinnedMemory(t *testing.T) {
	s, pinned, _, _ := refineFixture(t)
	if err := s.PinObservation(pinned); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 41; i++ {
		_, err := s.AddObservation(AddObservationParams{SessionID: "refine-session", Type: "decision", Title: fmt.Sprintf("recent item %d", i), Content: "recent detail", Project: "refine-proj"})
		if err != nil {
			t.Fatal(err)
		}
	}
	result, err := s.HostContext("refine-proj", 600)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "WAL 切换顺序") {
		t.Fatalf("older pinned memory missing: %s", result.Text)
	}
	if !result.Budget.Truncated {
		t.Fatal("more than 40 memories must report truncation")
	}
}

func TestHostContextDoesNotLetStaleEntriesHideActiveMemory(t *testing.T) {
	s, target, _, _ := refineFixture(t)
	for i := 0; i < 41; i++ {
		id, err := s.AddObservation(AddObservationParams{SessionID: "refine-session", Type: "decision", Title: fmt.Sprintf("stale item %d", i), Content: "stale detail", Project: "refine-proj"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE observations SET stale_at='2000-01-01 00:00:00' WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
	}
	result, err := s.HostContext("refine-proj", 600)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, fmt.Sprintf("id=%d", target)) {
		t.Fatalf("active memory hidden by stale entries: %s", result.Text)
	}
}
