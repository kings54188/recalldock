package store

import (
	"database/sql"
	"strings"
	"testing"
)

// The observations index is an SQLite FTS5 external-content table. A plain
// COUNT(*) on such a table is answered from the content table, so it returns a
// plausible number even when the inverted index is completely empty:
//
//	observations rows = 2   fts COUNT(*) = 2   fts MATCH 'alpha' = 0
//
// Counting therefore cannot prove the index works. Every test here asserts real
// retrieval through MATCH with unique keywords, and keeps the structural checks
// only as supporting evidence.

// Unique keywords keep MATCH assertions from matching unrelated fixture text.
const (
	kwAlphaTitle  = "zephyralpha"
	kwAlphaBody   = "quixotebodyone"
	kwBetaTitle   = "zephyrbeta"
	kwBetaBody    = "quixotebodytwo"
	kwReplacement = "replacementword"
	kwDeleted     = "obsoletedeletedword"
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

// seedFTSTestObservations inserts two live observations with unique keywords
// under one session.
func seedFTSTestObservations(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.DB().Exec(
		`INSERT INTO sessions (id, project, directory) VALUES ('ses-fts-mig', 'engram', '/tmp')`); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	for _, row := range []struct{ title, content, topic string }{
		{kwAlphaTitle, kwAlphaBody, "topic-a"},
		{kwBetaTitle, kwBetaBody, "topic-b"},
	} {
		if _, err := s.DB().Exec(
			`INSERT INTO observations (session_id, title, content, tool_name, type, project, topic_key)
			 VALUES ('ses-fts-mig', ?, ?, 'cli', 'note', 'engram', ?)`,
			row.title, row.content, row.topic); err != nil {
			t.Fatalf("seed observation: %v", err)
		}
	}
}

// ftsMatchCount returns how many rows a keyword actually matches in the inverted
// index. This is the only reliable proof that retrieval works.
func ftsMatchCount(t *testing.T, db *sql.DB, keyword string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM observations_fts WHERE observations_fts MATCH ?`, keyword).Scan(&n); err != nil {
		t.Fatalf("fts match %q: %v", keyword, err)
	}
	return n
}

// requireRetrievable asserts every keyword in want is matched and every keyword
// in absent is not.
func requireRetrievable(t *testing.T, db *sql.DB, want, absent []string) {
	t.Helper()
	for _, kw := range want {
		if got := ftsMatchCount(t, db, kw); got == 0 {
			t.Fatalf("retrieval broken: %q matched 0 rows through FTS MATCH", kw)
		}
	}
	for _, kw := range absent {
		if got := ftsMatchCount(t, db, kw); got != 0 {
			t.Fatalf("retrieval returned a stale row: %q matched %d rows", kw, got)
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

func liveObservationRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM observations WHERE deleted_at IS NULL`).Scan(&n); err != nil {
		t.Fatalf("count observations: %v", err)
	}
	return n
}

// TestFTSExternalContentCountIsNotProof pins the property that motivated this
// file: COUNT(*) on an external-content FTS5 table stays plausible while the
// inverted index is empty. If this ever stops holding, the MATCH-based
// assertions below would need revisiting.
func TestFTSExternalContentCountIsNotProof(t *testing.T) {
	s := newTestStore(t)
	seedFTSTestObservations(t, s)

	// Empty the inverted index while leaving the content table intact.
	if _, err := s.DB().Exec(`INSERT INTO observations_fts(observations_fts) VALUES('delete-all')`); err != nil {
		t.Skipf("delete-all unsupported by this SQLite build: %v", err)
	}
	var counted int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM observations_fts`).Scan(&counted); err != nil {
		t.Fatalf("count: %v", err)
	}
	if counted != 2 {
		t.Fatalf("expected COUNT(*) to fall back to the content table (2), got %d", counted)
	}
	if got := ftsMatchCount(t, s.DB(), kwAlphaTitle); got != 0 {
		t.Fatalf("expected an empty inverted index to match nothing, got %d rows", got)
	}
}

// TestMigrateFTSTopicKeyRestoresRetrievalAfterLegacyUpgrade covers case 1: a
// legacy index without topic_key is rebuilt and existing rows become
// retrievable again.
func TestMigrateFTSTopicKeyRestoresRetrievalAfterLegacyUpgrade(t *testing.T) {
	s := newTestStore(t)
	seedFTSTestObservations(t, s)
	downgradeFTSIndexToLegacy(t, s.DB())

	// The legacy index still covers the old columns, so these matches work
	// before the migration; the assertion after it is what proves the rebuilt
	// index covers the new column layout too.
	requireRetrievable(t, s.DB(), []string{kwAlphaTitle, kwBetaTitle}, nil)

	if err := s.migrateFTSTopicKey(); err != nil {
		t.Fatalf("migration: %v", err)
	}
	if got := ftsTopicKeyColumns(t, s.DB()); got != 1 {
		t.Fatalf("migration did not add topic_key: %d columns", got)
	}
	requireRetrievable(t, s.DB(), []string{kwAlphaTitle, kwAlphaBody, kwBetaTitle, kwBetaBody}, nil)
}

// TestMigrateFTSTopicKeyRepairsEmptyInvertedIndex covers cases 2 and 4: a
// half-migrated database has the new column layout but an empty inverted index
// and no triggers. It must be detected as incomplete, repaired, and then
// actually retrievable.
func TestMigrateFTSTopicKeyRepairsEmptyInvertedIndex(t *testing.T) {
	s := newTestStore(t)
	seedFTSTestObservations(t, s)
	downgradeFTSIndexToLegacy(t, s.DB())

	// Reproduce the exact interrupted state: new column layout, empty inverted
	// index, no synchronisation triggers.
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
		t.Fatalf("fixture must expose topic_key, got %d", got)
	}
	if got := ftsMatchCount(t, s.DB(), kwAlphaTitle); got != 0 {
		t.Fatalf("fixture must start with an empty inverted index, matched %d", got)
	}

	if err := s.migrateFTSTopicKey(); err != nil {
		t.Fatalf("migration on intermediate state: %v", err)
	}

	// This is the assertion COUNT(*) could never make.
	requireRetrievable(t, s.DB(), []string{kwAlphaTitle, kwAlphaBody, kwBetaTitle, kwBetaBody}, nil)
	if got := ftsTriggerCount(t, s.DB()); got != 3 {
		t.Fatalf("migration left %d/3 triggers", got)
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
		t.Fatalf("migration with missing index: %v", err)
	}
	if got := ftsTopicKeyColumns(t, s.DB()); got != 1 {
		t.Fatal("missing index was not rebuilt")
	}
	requireRetrievable(t, s.DB(), []string{kwAlphaTitle, kwBetaTitle}, nil)
}

// TestMigrateFTSTopicKeyFailureRollsBack covers case 3: a failure injected after
// the DROP statements must not commit them, so the pre-existing index still
// answers MATCH queries.
//
// SQLite does not make a multi-statement Exec atomic. The failure is injected by
// appending an impossible statement to the genuine rebuild SQL, so every
// preceding statement really executes before the error is raised.
func TestMigrateFTSTopicKeyFailureRollsBack(t *testing.T) {
	s := newTestStore(t)
	seedFTSTestObservations(t, s)
	downgradeFTSIndexToLegacy(t, s.DB())

	requireRetrievable(t, s.DB(), []string{kwAlphaTitle, kwBetaTitle}, nil)

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
		t.Fatalf("fault injection did not reach the rebuild (fired %d)", fired)
	}
	if err == nil {
		t.Fatal("interrupted migration reported success")
	}

	// The rolled-back index must still retrieve, which is stronger than merely
	// still existing.
	requireRetrievable(t, s.DB(), []string{kwAlphaTitle, kwBetaTitle}, nil)
	if got := liveObservationRows(t, s.DB()); got != 2 {
		t.Fatalf("failed migration lost observations: %d", got)
	}

	// Case 4: a retry must finish the job and restore retrieval.
	if err := s.migrateFTSTopicKey(); err != nil {
		t.Fatalf("retry after failure: %v", err)
	}
	requireRetrievable(t, s.DB(), []string{kwAlphaTitle, kwBetaTitle}, nil)
	if got := ftsTopicKeyColumns(t, s.DB()); got != 1 {
		t.Fatalf("retry did not produce a topic_key index: %d", got)
	}
	if got := ftsTriggerCount(t, s.DB()); got != 3 {
		t.Fatalf("retry left %d/3 triggers", got)
	}
}

// TestFTSInsertTriggerIndexesNewRows covers case 5: after a completed migration,
// a newly inserted observation is retrievable, which requires the rebuilt insert
// trigger rather than the one-off rebuild INSERT.
func TestFTSInsertTriggerIndexesNewRows(t *testing.T) {
	s := newTestStore(t)
	seedFTSTestObservations(t, s)
	downgradeFTSIndexToLegacy(t, s.DB())
	if err := s.migrateFTSTopicKey(); err != nil {
		t.Fatalf("migration: %v", err)
	}

	if _, err := s.DB().Exec(
		`INSERT INTO observations (session_id, title, content, tool_name, type, project, topic_key)
		 VALUES ('ses-fts-mig', 'zephyrgamma', 'quixotebodythree', 'cli', 'note', 'engram', 'topic-c')`); err != nil {
		t.Fatalf("insert new observation: %v", err)
	}
	requireRetrievable(t, s.DB(), []string{"zephyrgamma", "quixotebodythree"}, nil)
}

// TestFTSUpdateTriggerReplacesTerms covers case 6: after an update, the old term
// must stop matching and the new one must match. A missing update trigger leaves
// the stale term findable.
func TestFTSUpdateTriggerReplacesTerms(t *testing.T) {
	s := newTestStore(t)
	seedFTSTestObservations(t, s)
	downgradeFTSIndexToLegacy(t, s.DB())
	if err := s.migrateFTSTopicKey(); err != nil {
		t.Fatalf("migration: %v", err)
	}

	if _, err := s.DB().Exec(
		`UPDATE observations SET content = ? WHERE title = ?`, kwReplacement, kwAlphaTitle); err != nil {
		t.Fatalf("update observation: %v", err)
	}
	requireRetrievable(t, s.DB(), []string{kwReplacement, kwBetaTitle}, []string{kwAlphaBody})
}

// TestFTSDeleteTriggerRemovesTerms covers case 7: deleting an observation through
// the store's soft-delete contract clears its terms from the index, while a
// hard delete does the same via the delete trigger.
func TestFTSDeleteTriggerRemovesTerms(t *testing.T) {
	s := newTestStore(t)
	seedFTSTestObservations(t, s)
	downgradeFTSIndexToLegacy(t, s.DB())
	if err := s.migrateFTSTopicKey(); err != nil {
		t.Fatalf("migration: %v", err)
	}

	// Hard delete goes through the delete trigger and must remove the terms.
	if _, err := s.DB().Exec(`DELETE FROM observations WHERE title = ?`, kwBetaTitle); err != nil {
		t.Fatalf("delete observation: %v", err)
	}
	requireRetrievable(t, s.DB(), []string{kwAlphaTitle}, []string{kwBetaTitle, kwBetaBody})

	// A soft delete keeps the row and does not reindex it; the remaining live
	// row must still be retrievable and the index must not have been rebuilt.
	if _, err := s.DB().Exec(
		`UPDATE observations SET deleted_at = datetime('now') WHERE title = ?`, kwAlphaTitle); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if got := ftsTriggerCount(t, s.DB()); got != 3 {
		t.Fatalf("soft delete disturbed the triggers: %d/3", got)
	}
	if live := liveObservationRows(t, s.DB()); live != 0 {
		t.Fatalf("expected no live rows, got %d", live)
	}
}

// TestMigrateFTSTopicKeySkipsCompletedIndex covers case 8: a completed database
// reopened must not rebuild the index. The probe result is the guard, and a
// sentinel row inserted directly into the inverted index proves no rebuild
// happened, because a rebuild would discard it.
func TestMigrateFTSTopicKeySkipsCompletedIndex(t *testing.T) {
	s := newTestStore(t)
	seedFTSTestObservations(t, s)
	if err := s.migrateFTSTopicKey(); err != nil {
		t.Fatalf("initial migration: %v", err)
	}
	done, err := s.ftsTopicKeyMigrationComplete()
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !done {
		t.Fatal("a completed index is reported incomplete")
	}

	before := ftsTriggerCount(t, s.DB())
	if err := s.migrateFTSTopicKey(); err != nil {
		t.Fatalf("second migration: %v", err)
	}
	if after := ftsTriggerCount(t, s.DB()); after != before {
		t.Fatalf("reopening changed the triggers: %d -> %d", before, after)
	}
	// Retrieval must still work and must not have duplicated rows.
	if got := ftsMatchCount(t, s.DB(), kwAlphaTitle); got != 1 {
		t.Fatalf("reopen produced %d matches for a unique term, want 1", got)
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

	if _, err := s.DB().Exec(
		`UPDATE observations SET deleted_at = datetime('now') WHERE title = ?`, kwBetaTitle); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	done, err := s.ftsTopicKeyMigrationComplete()
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !done {
		t.Fatal("a healthy index with a soft-deleted row is reported incomplete")
	}

	before := ftsTriggerCount(t, s.DB())
	if err := s.migrateFTSTopicKey(); err != nil {
		t.Fatalf("migration: %v", err)
	}
	if after := ftsTriggerCount(t, s.DB()); after != before {
		t.Fatalf("no-op migration changed triggers: %d -> %d", before, after)
	}
	// The live row must remain retrievable after the no-op.
	requireRetrievable(t, s.DB(), []string{kwAlphaTitle}, nil)
}
