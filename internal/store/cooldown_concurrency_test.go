package store

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

// Pause at the first apply after scanning; a different writer changes the row.
func TestCooldownRechecksEligibilityAfterConcurrentWrite(t *testing.T) {
	for name, mutation := range map[string]string{
		"pin": "pinned=1", "protect": "memory_class='operational'", "user": "memory_class='user'",
		"archive": "archived_at=datetime('now')", "delete": "deleted_at=datetime('now')",
		"revision": "revision_count=revision_count+1", "recent": "updated_at=datetime('now')",
		"recall": "recall_count=20", "reschedule": "review_after=datetime('now','+1 day')",
		"project": "project='other'",
	} {
		t.Run(name, func(t *testing.T) {
			s := newTestStore(t)
			if err := s.CreateSession("cooldown-race", "cooldown", "/tmp/cooldown"); err != nil {
				t.Fatal(err)
			}
			id, err := s.AddObservation(AddObservationParams{SessionID: "cooldown-race", Project: "cooldown", Type: "discovery", Title: "Cold fixture", Content: "A verified project implementation detail", MemoryClass: MemoryClassProject})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.db.Exec("UPDATE observations SET updated_at='2000-01-01 00:00:00', review_after=NULL WHERE id=?", id); err != nil {
				t.Fatal(err)
			}
			scanned, release := make(chan struct{}), make(chan struct{})
			original := s.hooks.exec
			s.hooks.exec = func(db execer, q string, args ...any) (sql.Result, error) {
				if strings.Contains(q, "UPDATE observations SET review_after") {
					close(scanned)
					<-release
				}
				return original(db, q, args...)
			}
			type outcome struct {
				r   *CooldownArchiveResult
				err error
			}
			done := make(chan outcome, 1)
			go func() { r, e := s.CooldownArchive("cooldown", false, 24*time.Hour, false); done <- outcome{r, e} }()
			select {
			case <-scanned:
			case <-time.After(5 * time.Second):
				t.Fatal("apply barrier not reached")
			}
			_, err = s.db.Exec("UPDATE observations SET "+mutation+" WHERE id=?", id)
			close(release)
			got := <-done
			if err != nil {
				t.Fatal(err)
			}
			if got.err != nil || got.r.Cooled != 0 {
				t.Fatalf("concurrently changed row cooled: result=%+v err=%v", got.r, got.err)
			}
			var review sql.NullString
			if err = s.db.QueryRow("SELECT review_after FROM observations WHERE id=?", id).Scan(&review); err != nil {
				t.Fatal(err)
			}
			if name != "reschedule" && review.Valid {
				t.Fatalf("review marker overwritten: %s", review.String)
			}
		})
	}
}

func TestCooldownStillSchedulesEligibleRow(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("cold", "cooldown", "/tmp/cooldown"); err != nil {
		t.Fatal(err)
	}
	id, err := s.AddObservation(AddObservationParams{SessionID: "cold", Project: "cooldown", Type: "discovery", Title: "Eligible", Content: "A reusable implementation detail"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("UPDATE observations SET updated_at='2000-01-01', review_after=NULL WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	r, err := s.CooldownArchive("cooldown", false, time.Hour, false)
	if err != nil || r.Cooled != 1 {
		t.Fatalf("result=%+v err=%v", r, err)
	}
}
