package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestCreateSessionRejectsOwnershipCollisionWithoutSyncWrites(t *testing.T) {
	for _, changed := range []string{"project", "directory"} {
		t.Run(changed, func(t *testing.T) {
			s := newTestStore(t)
			if err := s.EnrollProject("legacy-one"); err != nil {
				t.Fatal(err)
			}
			if err := s.EnrollProject("legacy-two"); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			if err := s.CreateSession("shared-id", "legacy-one", root); err != nil {
				t.Fatal(err)
			}
			before, err := s.GetSession("shared-id")
			if err != nil {
				t.Fatal(err)
			}
			var count int
			if err := s.db.QueryRow("SELECT count(*) FROM sync_mutations").Scan(&count); err != nil {
				t.Fatal(err)
			}
			project, directory := "legacy-one", root
			if changed == "project" {
				project = "legacy-two"
			} else {
				directory = t.TempDir()
			}
			if err := s.CreateSession("shared-id", project, directory); err == nil {
				t.Error("session ownership collision accepted")
			}
			after, err := s.GetSession("shared-id")
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("session changed: %+v %v", after, err)
			}
			var afterCount int
			if err := s.db.QueryRow("SELECT count(*) FROM sync_mutations").Scan(&afterCount); err != nil {
				t.Fatal(err)
			}
			if count != afterCount {
				t.Fatalf("rejected registration wrote sync data: %d -> %d", count, afterCount)
			}
		})
	}
}

func TestCreateSessionConcurrentOwnersHaveOneWinner(t *testing.T) {
	s := newTestStore(t)
	other, err := New(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	for _, project := range []string{"legacy-one", "legacy-two"} {
		if err := s.EnrollProject(project); err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	root := t.TempDir()
	for i, instance := range []*Store{s, other} {
		project := []string{"legacy-one", "legacy-two"}[i]
		go func() {
			<-start
			results <- instance.CreateSession("racing-id", project, root)
		}()
	}
	close(start)
	winners := 0
	for range 2 {
		if err := <-results; err == nil {
			winners++
		} else if !errors.Is(err, ErrSessionScopeMismatch) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent session owners accepted: %d", winners)
	}
	session, err := s.GetSession("racing-id")
	if err != nil {
		t.Fatal(err)
	}
	var foreign int
	if err := s.db.QueryRow("SELECT count(*) FROM sync_mutations WHERE entity_key=? AND project!=?", session.ID, session.Project).Scan(&foreign); err != nil {
		t.Fatal(err)
	}
	if foreign != 0 {
		t.Fatalf("journal contains %d records for losing owner", foreign)
	}
}

func TestCreateSessionReplayPreservesEndedStateInSync(t *testing.T) {
	s := newTestStore(t)
	if err := s.EnrollProject("legacy-one"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := s.CreateSession("ended-id", "legacy-one", root); err != nil {
		t.Fatal(err)
	}
	if err := s.EndSession("ended-id", "operator summary"); err != nil {
		t.Fatal(err)
	}
	before, err := s.GetSession("ended-id")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession("ended-id", "legacy-one", root); err != nil {
		t.Fatal(err)
	}
	after, err := s.GetSession("ended-id")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("replay reopened or changed session: %+v %v", after, err)
	}
	var mutation SyncMutation
	if err := s.db.QueryRow("SELECT entity,entity_key,op,payload,project FROM sync_mutations WHERE entity=? AND entity_key=? ORDER BY seq DESC LIMIT 1", SyncEntitySession, "ended-id").Scan(&mutation.Entity, &mutation.EntityKey, &mutation.Op, &mutation.Payload, &mutation.Project); err != nil {
		t.Fatal(err)
	}
	var payload syncSessionPayload
	if err := json.Unmarshal([]byte(mutation.Payload), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.EndedAt == nil || *payload.EndedAt != *before.EndedAt || payload.Summary == nil || *payload.Summary != *before.Summary {
		t.Errorf("replayed registration lost lifecycle sync metadata: %+v", payload)
	}
	dest := newTestStore(t)
	if err := dest.withTx(func(tx *sql.Tx) error { return dest.applyPulledMutationTx(tx, mutation) }); err != nil {
		t.Fatal(err)
	}
	received, err := dest.GetSession("ended-id")
	if err != nil || !reflect.DeepEqual(before, received) {
		t.Fatalf("receiver diverged after replay: %+v %v", received, err)
	}
}

func TestCreateSessionPartialMetadataSyncUsesStoredValues(t *testing.T) {
	s := newTestStore(t)
	if err := s.EnrollProject("legacy-one"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := s.CreateSession("partial-id", "legacy-one", root); err != nil {
		t.Fatal(err)
	}
	// Omitted legacy metadata preserves the established owner and directory.
	if err := s.CreateSession("partial-id", "", ""); err != nil {
		t.Fatal(err)
	}
	var payload syncSessionPayload
	var raw, owner string
	if err := s.db.QueryRow("SELECT payload,project FROM sync_mutations WHERE entity_key=? ORDER BY seq DESC LIMIT 1", "partial-id").Scan(&raw, &owner); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatal(err)
	}
	if owner != "legacy-one" || payload.Project != owner || payload.Directory != root {
		t.Fatalf("journal used empty request metadata: owner=%s payload=%+v", owner, payload)
	}
}

func TestCreateSessionRetiredKeyPrecedesOwnershipConflict(t *testing.T) {
	s := newTestStore(t)
	root := t.TempDir()
	if err := s.CreateSession("retired-session", "legacy-source", root); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MergeProjects([]string{"legacy-source"}, "canonical-target"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession("retired-session", "legacy-source", root); !errors.Is(err, ErrRetiredProjectKey) {
		t.Fatalf("retired writer must be directed to an explicit current selection: %v", err)
	}
	session, err := s.GetSession("retired-session")
	if err != nil || session.Project != "canonical-target" {
		t.Fatalf("retired registration changed owner: %+v %v", session, err)
	}
}
