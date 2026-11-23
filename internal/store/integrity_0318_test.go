package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestSessionAndProjectDeletionRejectForeignPromptCascade(t *testing.T) {
	for _, projectDelete := range []bool{false, true} {
		t.Run(fmt.Sprint(projectDelete), func(t *testing.T) {
			s := newTestStore(t)
			data := &ExportData{Sessions: []Session{{ID: "mixed", Project: "alpha", Directory: t.TempDir(), StartedAt: Now()}}, Prompts: []Prompt{
				{SyncID: "owned-prompt", SessionID: "mixed", Project: "alpha", Content: "owned", CreatedAt: Now()},
				{SyncID: "foreign-prompt", SessionID: "mixed", Project: "beta", Content: "foreign", CreatedAt: Now()},
			}}
			if _, err := s.Import(data); err != nil {
				t.Fatal(err)
			}
			var err error
			if projectDelete {
				_, err = s.DeleteProject("alpha", true)
			} else {
				err = s.DeleteSession("mixed")
			}
			if err == nil || !strings.Contains(err.Error(), "project_scope_mismatch") {
				t.Fatalf("foreign cascade not rejected: %v", err)
			}
			var prompts, sessions, tombstones int
			if err := s.db.QueryRow("SELECT count(*) FROM user_prompts").Scan(&prompts); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRow("SELECT count(*) FROM sessions").Scan(&sessions); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRow("SELECT count(*) FROM prompt_tombstones").Scan(&tombstones); err != nil {
				t.Fatal(err)
			}
			if prompts != 2 || sessions != 1 || tombstones != 0 {
				t.Fatalf("rejected deletion partially changed state: %d %d %d", prompts, sessions, tombstones)
			}
		})
	}
}

func TestJSONImportDoesNotResurrectDeletedPrompts(t *testing.T) {
	for _, sessionDelete := range []bool{false, true} {
		t.Run(fmt.Sprint(sessionDelete), func(t *testing.T) {
			s := newTestStore(t)
			data := importReplayFixture(t)
			data.Observations = nil
			if _, err := s.Import(data); err != nil {
				t.Fatal(err)
			}
			if sessionDelete {
				if err := s.DeleteSession(data.Sessions[0].ID); err != nil {
					t.Fatal(err)
				}
			} else {
				var id int64
				if err := s.db.QueryRow("SELECT id FROM user_prompts").Scan(&id); err != nil {
					t.Fatal(err)
				}
				if err := s.DeletePrompt(id); err != nil {
					t.Fatal(err)
				}
			}
			replay, err := s.Import(data)
			if err != nil || replay.PromptsImported != 0 || replay.PromptsSkipped != 1 {
				t.Fatalf("deleted prompt resurrected: %+v %v", replay, err)
			}
			var count int
			if err := s.db.QueryRow("SELECT count(*) FROM user_prompts").Scan(&count); err != nil || count != 0 {
				t.Fatalf("prompt came back: %d %v", count, err)
			}
		})
	}
}

func TestDeletesKeepTombstonesAndClearProjectDerivatives(t *testing.T) {
	for _, hard := range []bool{false, true} {
		t.Run(fmt.Sprint(hard), func(t *testing.T) {
			s := newTestStore(t)
			if err := s.CreateSession("delete-session", "delete-project", t.TempDir()); err != nil {
				t.Fatal(err)
			}
			var ids []int64
			for _, title := range []string{"source", "dependent"} {
				id, err := s.AddObservation(AddObservationParams{SessionID: "delete-session", Project: "delete-project", Type: "decision", Title: title, Content: title, MemoryPath: "project/secret", Abstract: "private synthetic abstract"})
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, id)
			}
			if _, err := s.AddMemoryDependency(AddMemoryDependencyParams{SourceObservationID: ids[0], DependentObservationID: ids[1]}); err != nil {
				t.Fatal(err)
			}
			obs, err := s.GetObservation(ids[0])
			if err != nil {
				t.Fatal(err)
			}
			payload := observationPayloadFromObservation(obs)
			if _, err := s.db.Exec(`INSERT INTO sync_enrolled_projects(project) VALUES('delete-project')`); err != nil {
				t.Fatal(err)
			}
			if _, err := s.DeleteProject("delete-project", hard); err != nil {
				t.Fatal(err)
			}
			for _, table := range []string{"memory_nodes", "memory_dependencies", "resource_dependencies"} {
				var n int
				if err := s.db.QueryRow("SELECT count(*) FROM " + table + " WHERE project='delete-project'").Scan(&n); err != nil || n != 0 {
					t.Fatalf("derivatives left in %s: %d %v", table, n, err)
				}
			}
			var deletes int
			if err := s.db.QueryRow(`SELECT count(*) FROM sync_mutations WHERE entity='observation' AND op='delete'`).Scan(&deletes); err != nil || deletes != 2 {
				t.Fatalf("delete journal: %d %v", deletes, err)
			}
			if hard {
				if err := s.CreateSession("delete-session", "delete-project", t.TempDir()); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.withTx(func(tx *sql.Tx) error { return s.applyObservationUpsertTx(tx, payload) }); err != nil {
				t.Fatal(err)
			}
			if _, err := s.GetObservation(ids[0]); err == nil {
				t.Fatal("sync revived deleted memory")
			}
			var active int
			if err := s.db.QueryRow(`SELECT count(*) FROM observations WHERE sync_id=? AND deleted_at IS NULL`, obs.SyncID).Scan(&active); err != nil || active != 0 {
				t.Fatalf("delete resurrected: %d %v", active, err)
			}
		})
	}
}

func TestExchangeUTCAndRecallMetadataRoundTrip(t *testing.T) {
	data := importReplayFixture(t)
	data.Sessions[0].StartedAt = "2026-11-15T11:00:00.123456789+08:00"
	data.Observations[0].CreatedAt = "2026-11-15T11:00:00+08:00"
	data.Observations[0].UpdatedAt = "2026-11-15T11:00:00+08:00"
	recalled := "2026-11-15T11:01:00+08:00"
	data.Observations[0].RecallCount, data.Observations[0].LastRecallAt = 17, &recalled
	s := newTestStore(t)
	if _, err := s.Import(data); err != nil {
		t.Fatal(err)
	}
	var created, last string
	var recalls int
	if err := s.db.QueryRow(`SELECT created_at,recall_count,last_recall_at FROM observations`).Scan(&created, &recalls, &last); err != nil {
		t.Fatal(err)
	}
	if created != "2026-11-15 03:00:00" || recalls != 17 || last != "2026-11-15 03:01:00" {
		t.Fatalf("exchange metadata changed: %s %d %s", created, recalls, last)
	}
	results, err := s.Search("Replay contract", SearchOptions{Project: "replay", Since: "2026-11-15 05:00:00"})
	if err != nil || len(results) != 0 {
		t.Fatalf("offset timestamp passed wrong since: %d %v", len(results), err)
	}
	if _, err := s.Import(data); err != nil {
		t.Fatal(err)
	}
	if data.Observations[0].CreatedAt != "2026-11-15T11:00:00+08:00" {
		t.Fatal("import modified caller data")
	}
	if _, err := normalizeTimestampJSON([]byte(`{"id":9007199254740993,"created_at":"2026-11-15T03:00:00Z"}`)); err != nil {
		t.Fatal(err)
	}
	encoded, err := normalizeTimestampJSON([]byte(`{"id":9007199254740993,"created_at":"2026-11-15T03:00:00Z"}`))
	if err != nil || !strings.Contains(string(encoded), "9007199254740993") {
		t.Fatal("timestamp normalization rounded integer identity")
	}
}

func TestSearchPreviewWorksWithQueryOnlyDatabase(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("readonly", "readonly", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	id, err := s.AddObservation(AddObservationParams{SessionID: "readonly", Project: "readonly", Type: "decision", Title: "中文只读检索", Content: "中文只读检索内容"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`PRAGMA query_only=ON`); err != nil {
		t.Fatal(err)
	}
	defer s.db.Exec(`PRAGMA query_only=OFF`)
	results, err := s.SearchPreview("中文只读", SearchOptions{Project: "readonly"})
	if err != nil || len(results) != 1 || results[0].ID != id {
		t.Fatalf("read-only preview: %v %+v", err, results)
	}
}

func TestSerializedSearchResponseFitsEstimatedBudget(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("budget", "budget", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, err := s.AddObservation(AddObservationParams{SessionID: "budget", Project: "budget", Type: "decision", Title: fmt.Sprintf("预算 budget marker %d", i), Content: strings.Repeat("中文正文 budget ", 50), Abstract: "短摘要", Overview: strings.Repeat("概览内容 ", 100)}); err != nil {
			t.Fatal(err)
		}
	}
	const budget = 1800
	r, err := s.SearchDetailed("budget marker", SearchOptions{Project: "budget", Detail: SearchDetailL1, TokenBudget: budget, Limit: 8})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if EstimateReadTokens(string(raw)) > budget || r.Budget.Used > budget {
		t.Fatalf("response exceeds estimated budget: actual=%d reported=%d", EstimateReadTokens(string(raw)), r.Budget.Used)
	}
	if EstimateReadTokens("中文") < 2 {
		t.Fatal("CJK estimator is not conservative")
	}
	_, err = s.SearchDetailed("budget marker", SearchOptions{Project: "budget", Detail: SearchDetailL1, TokenBudget: 1})
	if !errors.Is(err, ErrBudgetTooSmall) {
		t.Fatalf("too-small response: %v", err)
	}
}

func TestScopedSyncCannotRelabelForeignIdentity(t *testing.T) {
	s := newTestStore(t)
	for _, p := range []string{"alpha", "beta"} {
		if err := s.CreateSession(p, p, t.TempDir()); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddObservation(AddObservationParams{SessionID: p, Project: p, Type: "decision", Title: p, Content: p}); err != nil {
			t.Fatal(err)
		}
	}
	var foreignID string
	if err := s.db.QueryRow(`SELECT sync_id FROM observations WHERE project='beta'`).Scan(&foreignID); err != nil {
		t.Fatal(err)
	}
	p := syncObservationPayload{SyncID: foreignID, SessionID: "alpha", Project: nullableString("alpha"), Type: "decision", Scope: "project", Title: "changed", Content: "changed"}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{foreignID, "obs-made-up"} {
		if err := s.ApplyPulledChunkForProject("local:alpha", key, []SyncMutation{{Entity: SyncEntityObservation, EntityKey: key, Project: "alpha", Op: SyncOpUpsert, Payload: string(raw)}}, "alpha"); err == nil {
			t.Fatal("scoped chunk relabeled foreign identity")
		}
	}
	var body string
	if err := s.db.QueryRow(`SELECT content FROM observations WHERE sync_id=?`, foreignID).Scan(&body); err != nil || body != "beta" {
		t.Fatal("foreign memory modified")
	}
}
