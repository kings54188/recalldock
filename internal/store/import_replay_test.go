package store

import (
	"strings"
	"testing"
)

func importReplayFixture(t *testing.T) *ExportData {
	t.Helper()
	s := newTestStore(t)
	if err := s.CreateSession("json-replay-session", "replay", "/tmp/replay"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddObservation(AddObservationParams{SessionID: "json-replay-session", Project: "replay", Type: "decision", Title: "Replay contract", Content: "A repeated import must not create a second identity."}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddPrompt(AddPromptParams{SessionID: "json-replay-session", Project: "replay", Content: "Synthetic prompt fixture"}); err != nil {
		t.Fatal(err)
	}
	data, err := s.Export()
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestJSONImportReplayPreservesObservationAndPromptIdentity(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "stable_sync_ids", true: "legacy_without_sync_ids"}[legacy], func(t *testing.T) {
			data := importReplayFixture(t)
			if legacy {
				data.Observations[0].SyncID = ""
				data.Prompts[0].SyncID = ""
			}
			s := newTestStore(t)
			first, err := s.Import(data)
			if err != nil || first.ObservationsImported != 1 || first.PromptsImported != 1 {
				t.Fatalf("initial import=%+v err=%v", first, err)
			}
			second, err := s.Import(data)
			if err != nil || second.ObservationsImported != 0 || second.PromptsImported != 0 {
				t.Fatalf("replay must not insert: result=%+v err=%v", second, err)
			}
			stats, err := s.Stats()
			if err != nil || stats.TotalObservations != 1 || stats.TotalPrompts != 1 || stats.TotalSessions != 1 {
				t.Fatalf("replay changed counts: %+v err=%v", stats, err)
			}
		})
	}
}

func TestJSONImportRejectsIdentityConflictAndRollsBack(t *testing.T) {
	for _, kind := range []string{"observation", "prompt", "session_project"} {
		t.Run(kind, func(t *testing.T) {
			data := importReplayFixture(t)
			s := newTestStore(t)
			if _, err := s.Import(data); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "observation":
				data.Observations[0].Content = "Conflicting payload must not overwrite the existing record."
			case "prompt":
				data.Prompts[0].Content = "Conflicting prompt payload"
			case "session_project":
				data.Sessions[0].Project = "different-project"
			}
			data.Sessions = append([]Session{{ID: "must-rollback", Project: "replay", Directory: "/tmp/replay", StartedAt: Now()}}, data.Sessions...)
			if _, err := s.Import(data); err == nil || !strings.Contains(err.Error(), "identity conflict") {
				t.Fatalf("expected explicit identity conflict, got %v", err)
			}
			stats, err := s.Stats()
			if err != nil || stats.TotalObservations != 1 || stats.TotalPrompts != 1 || stats.TotalSessions != 1 {
				t.Fatalf("conflicting import must roll back: %+v err=%v", stats, err)
			}
		})
	}
}
