package store

import (
	"database/sql"
	"strings"
	"testing"
)

// downgradeFTSIndexToLegacy rewrites the live FTS index into its pre-topic_key
// shape so a migration test can exercise the real rebuild path against a
// database whose remaining schema is current.
func downgradeFTSIndexToLegacy(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, stmt := range []string{
		`DROP TRIGGER IF EXISTS obs_fts_insert`,
		`DROP TRIGGER IF EXISTS obs_fts_update`,
		`DROP TRIGGER IF EXISTS obs_fts_delete`,
		`DROP TABLE IF EXISTS observations_fts`,
		`CREATE VIRTUAL TABLE observations_fts USING fts5(
			title, content, tool_name, type, project,
			content='observations', content_rowid='id')`,
		`INSERT INTO observations_fts(rowid, title, content, tool_name, type, project)
		 SELECT id, title, content, tool_name, type, project
		 FROM observations WHERE deleted_at IS NULL`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("downgrade fts index: %v (%s)", err, stmt)
		}
	}
}

// seedFTSTestObservations inserts two live observations under one session.
func seedFTSTestObservations(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.DB().Exec(
		`INSERT INTO sessions (id, project, directory) VALUES ('ses-fts-mig', 'engram', '/tmp')`); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	for _, row := range []struct{ title, content, topic string }{
		{"kept", "surviving memory", "topic-a"},
		{"second", "another memory", "topic-b"},
	} {
		if _, err := s.DB().Exec(
			`INSERT INTO observations (session_id, title, content, tool_name, type, project, topic_key)
			 VALUES ('ses-fts-mig', ?, ?, 'cli', 'note', 'engram', ?)`,
			row.title, row.content, row.topic); err != nil {
			t.Fatalf("seed observation: %v", err)
		}
	}
}

func ftsTableExists(t *testing.T, db *sql.DB) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='observations_fts'`).Scan(&n); err != nil {
		t.Fatalf("probe fts table: %v", err)
	}
	return n > 0
}

func ftsTopicKeyColumns(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_xinfo('observations_fts') WHERE name='topic_key'`).Scan(&n); err != nil {
		t.Fatalf("probe topic_key column: %v", err)
	}
	return n
}

func ftsTriggerCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name IN
		 ('obs_fts_insert','obs_fts_update','obs_fts_delete')`).Scan(&n); err != nil {
		t.Fatalf("probe fts triggers: %v", err)
	}
	return n
}

func ftsIndexedRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM observations_fts`).Scan(&n); err != nil {
		t.Fatalf("count indexed rows: %v", err)
	}
	return n
}

func liveObservationRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM observations WHERE deleted_at IS NULL`).Scan(&n); err != nil {
		t.Fatalf("count observations: %v", err)
	}
	return n
}

// TestMigrateFTSTopicKeyRepairsIntermediateState is the regression test for the
// migration's completion check.
//
// The pre-fix probe asked only whether the topic_key column was present. A run
// that was interrupted after CREATE VIRTUAL TABLE but before the repopulating
// INSERT and the three trigger definitions therefore looked finished: the
// column existed, so the probe returned nil and never repaired the missing
// rows or triggers. Retrieval would then silently miss rows, and writes would
// not be indexed at all, with no error surfaced anywhere.
func TestMigrateFTSTopicKeyRepairsIntermediateState(t *testing.T) {
	s := newTestStore(t)
	seedFTSTestObservations(t, s)
	downgradeFTSIndexToLegacy(t, s.DB())

	// Reproduce the exact state an interrupted rebuild can leave: the new
	// column layout exists, but the index is empty and no trigger is defined.
	for _, stmt := range []string{
		`DROP TRIGGER IF EXISTS obs_fts_insert`,
		`DROP TRIGGER IF EXISTS obs_fts_update`,
		`DROP TRIGGER IF EXISTS obs_fts_delete`,
		`DROP TABLE IF EXISTS observations_fts`,
		`CREATE VIRTUAL TABLE observations_fts USING fts5(
			title, content, tool_name, type, project, topic_key,
			content='observations', content_rowid='id')`,
	} {
		if _, err := s.DB().Exec(stmt); err != nil {
			t.Fatalf("build intermediate state: %v", err)
		}
	}
	if got := ftsTopicKeyColumns(t, s.DB()); got != 1 {
		t.Fatalf("fixture must present the topic_key column, got %d", got)
	}
	if got := ftsTriggerCount(t, s.DB()); got != 0 {
		t.Fatalf("fixture must have no fts triggers, got %d", got)
	}

	if err := s.migrateFTSTopicKey(); err != nil {
		t.Fatalf("migration failed on intermediate state: %v", err)
	}

	if got := ftsTriggerCount(t, s.DB()); got != 3 {
		t.Fatalf("migration left %d/3 fts triggers: writes would not be indexed", got)
	}
	if indexed, live := ftsIndexedRows(t, s.DB()), liveObservationRows(t, s.DB()); indexed < live {
		t.Fatalf("migration left the index missing live rows: indexed %d live %d", indexed, live)
	}
}

// TestMigrateFTSTopicKeyRebuildsWhenIndexMissing covers a database where an
// interrupted run already dropped the index entirely.
func TestMigrateFTSTopicKeyRebuildsWhenIndexMissing(t *testing.T) {
	s := newTestStore(t)
	seedFTSTestObservations(t, s)
	downgradeFTSIndexToLegacy(t, s.DB())

	for _, stmt := range []string{
		`DROP TRIGGER IF EXISTS obs_fts_insert`,
		`DROP TRIGGER IF EXISTS obs_fts_update`,
		`DROP TRIGGER IF EXISTS obs_fts_delete`,
		`DROP TABLE IF EXISTS observations_fts`,
	} {
		if _, err := s.DB().Exec(stmt); err != nil {
			t.Fatalf("prepare missing index: %v", err)
		}
	}

	if err := s.migrateFTSTopicKey(); err != nil {
		t.Fatalf("migration with missing index failed: %v", err)
	}
	if got := ftsTopicKeyColumns(t, s.DB()); got != 1 {
		t.Fatal("missing index was not rebuilt")
	}
	if got := ftsTriggerCount(t, s.DB()); got != 3 {
		t.Fatalf("rebuilt index has %d/3 triggers", got)
	}
	// The rebuilt index must contain every live row. It may retain soft-deleted
	// rows, because the projection keeps a row until the delete trigger removes
	// it, so only the lower bound is asserted.
	if indexed, live := ftsIndexedRows(t, s.DB()), liveObservationRows(t, s.DB()); indexed < live {
		t.Fatalf("rebuilt index is missing live rows: indexed %d live %d", indexed, live)
	}
}

// TestMigrateFTSTopicKeyFailureRollsBack asserts the rebuild is atomic: a
// failure after the DROP statements must not commit them.
//
// SQLite does not make a multi-statement Exec atomic. The failure here is
// injected by appending an impossible statement to the genuine rebuild SQL, so
// every preceding statement really executes before the error is raised.
func TestMigrateFTSTopicKeyFailureRollsBack(t *testing.T) {
	s := newTestStore(t)
	seedFTSTestObservations(t, s)
	downgradeFTSIndexToLegacy(t, s.DB())

	beforeIndexed := ftsIndexedRows(t, s.DB())
	if beforeIndexed != 2 {
		t.Fatalf("fixture index has %d rows, want 2", beforeIndexed)
	}

	original := s.hooks.exec
	fired := 0
	s.hooks.exec = func(db execer, query string, args ...any) (sql.Result, error) {
		if strings.Contains(query, "CREATE VIRTUAL TABLE observations_fts") {
			fired++
			return original(db, query+"\nSELECT * FROM absent_table_after_rebuild;", args...)
		}
		return original(db, query, args...)
	}
	err := s.migrateFTSTopicKey()
	s.hooks.exec = original
	if fired != 1 {
		t.Fatalf("fault injection did not reach the rebuild (fired %d times)", fired)
	}
	if err == nil {
		t.Fatal("interrupted migration reported success")
	}

	// The index must still exist with its rows: a failed migration may not
	// commit the drops.
	if !ftsTableExists(t, s.DB()) {
		t.Fatal("failed migration committed the index drop")
	}
	if got := liveObservationRows(t, s.DB()); got != 2 {
		t.Fatalf("failed migration lost observations: got %d want 2", got)
	}

	// A retry must still be able to finish the migration.
	if err := s.migrateFTSTopicKey(); err != nil {
		t.Fatalf("retry after failure failed: %v", err)
	}
	if got := ftsTopicKeyColumns(t, s.DB()); got != 1 {
		t.Fatalf("retry did not produce a topic_key index: %d columns", got)
	}
	if got := ftsTriggerCount(t, s.DB()); got != 3 {
		t.Fatalf("retry left %d/3 triggers", got)
	}
}

// TestMigrateFTSTopicKeyIsIdempotent runs the migration twice on a current
// database and asserts the second run leaves the index untouched.
func TestMigrateFTSTopicKeyIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	seedFTSTestObservations(t, s)

	if err := s.migrateFTSTopicKey(); err != nil {
		t.Fatalf("first migration: %v", err)
	}
	firstCols := ftsTopicKeyColumns(t, s.DB())
	firstTriggers := ftsTriggerCount(t, s.DB())

	if err := s.migrateFTSTopicKey(); err != nil {
		t.Fatalf("second migration: %v", err)
	}
	if got := ftsTopicKeyColumns(t, s.DB()); got != firstCols {
		t.Fatalf("repeat migration changed index shape: %d -> %d", firstCols, got)
	}
	if got := ftsTriggerCount(t, s.DB()); got != firstTriggers {
		t.Fatalf("repeat migration changed triggers: %d -> %d", firstTriggers, got)
	}
	if indexed, live := ftsIndexedRows(t, s.DB()), liveObservationRows(t, s.DB()); indexed < live {
		t.Fatalf("index missing live rows after repeat: indexed %d live %d", indexed, live)
	}
}

// TestMigrateFTSTopicKeyIgnoresSoftDeletedRows guards the completion probe
// against a row-parity criterion.
//
// The index is an external-content projection: a soft delete keeps the indexed
// row and only clears it through the delete trigger, so `indexed == live` is
// false after any soft delete. Treating that as "migration incomplete" rebuilt
// the whole index on every open, which is both a performance regression and a
// behaviour change on a healthy database.
func TestMigrateFTSTopicKeyIgnoresSoftDeletedRows(t *testing.T) {
	s := newTestStore(t)
	seedFTSTestObservations(t, s)

	// Soft delete one row the way DeleteObservation does.
	if _, err := s.DB().Exec(
		`UPDATE observations SET deleted_at = datetime('now') WHERE title = 'second'`); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	done, err := s.ftsTopicKeyMigrationComplete()
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !done {
		t.Fatal("a healthy index with a soft-deleted row is reported incomplete")
	}

	// Running the migration must therefore be a no-op that keeps the triggers.
	before := ftsTriggerCount(t, s.DB())
	if err := s.migrateFTSTopicKey(); err != nil {
		t.Fatalf("migration: %v", err)
	}
	if after := ftsTriggerCount(t, s.DB()); after != before {
		t.Fatalf("no-op migration changed triggers: %d -> %d", before, after)
	}
}
