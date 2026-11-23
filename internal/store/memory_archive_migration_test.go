package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestLegacyPrimaryKeyMigrationPreservesDurableMemoryState(t *testing.T) {
	cfg := mustDefaultConfig(t)
	cfg.DataDir = t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(cfg.DataDir, "recalldock.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE sessions(id TEXT PRIMARY KEY,project TEXT NOT NULL,directory TEXT NOT NULL,started_at TEXT DEFAULT (datetime('now')),ended_at TEXT,summary TEXT);
INSERT INTO sessions(id,project,directory) VALUES('legacy-session','legacy-project','/synthetic');
CREATE TABLE observations(id INTEGER,sync_id TEXT,session_id TEXT,type TEXT,title TEXT,content TEXT,tool_name TEXT,project TEXT,scope TEXT,topic_key TEXT,normalized_hash TEXT,
 revision_count INTEGER,duplicate_count INTEGER,last_seen_at TEXT,created_at TEXT,updated_at TEXT,deleted_at TEXT,pinned INTEGER,memory_class TEXT,memory_path TEXT,
 abstract TEXT,overview TEXT,summary_state TEXT,summary_revision INTEGER,recall_count INTEGER,last_recall_at TEXT,archived_at TEXT,review_after TEXT);
INSERT INTO observations(id,sync_id,session_id,type,title,content,project,scope,revision_count,duplicate_count,created_at,updated_at,pinned,memory_class,memory_path,
 abstract,overview,summary_state,summary_revision,recall_count,last_recall_at,archived_at,review_after)
 VALUES(7,'obs-existing-legacy','legacy-session','decision','Preserved title','Preserved durable body','legacy-project','project',3,1,'2026-01-01 00:00:00','2026-01-01 00:00:00',1,'project','project/design/archive',
 'Preserved abstract','Preserved overview','fresh',3,4,'2026-01-01 00:00:00','2026-02-01 00:00:00','2026-03-01 00:00:00');`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	obs, err := s.GetObservation(7)
	if err != nil {
		t.Fatal(err)
	}
	if obs.SyncID != "obs-existing-legacy" {
		t.Fatal("legacy table rebuild replaced an established resource identity")
	}
	if !obs.Pinned || obs.ArchivedAt == nil || obs.ReviewAfter == nil || obs.RecallCount != 4 || obs.Abstract != "Preserved abstract" || obs.Overview != "Preserved overview" || obs.MemoryPath != "project/design/archive" {
		t.Fatalf("legacy metadata lost: %+v", obs)
	}
	var integrity string
	if err := s.db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatal("migration integrity failed")
	}
}
