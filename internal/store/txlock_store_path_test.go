package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"modernc.org/sqlite"
)

// The contention tests in txlock_contention_test.go use raw sql.Open calls,
// which proves the DSN option works but not that Store actually opens the
// database that way. These tests go through the real New() path so a future
// refactor that drops the parameter cannot pass CI.
//
// _txlock=immediate is observable without racing: with it every transaction
// takes the write lock at BEGIN, so a second connection cannot even begin a
// deferred read while the first transaction is open — its BEGIN blocks or
// fails instead of succeeding and failing later at the upgrade.

// openStoreWithExistingDB creates a real Store over a database file, exercising
// the RequireExisting branch that carries the _txlock=immediate setting.
func openStoreWithExistingDB(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	// First open creates the schema.
	cfg := mustDefaultConfig(t)
	cfg.DataDir = dir
	first, err := New(cfg)
	if err != nil {
		t.Fatalf("initial New: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}
	// Second open is the RequireExisting path the fix targets.
	reopen := mustDefaultConfig(t)
	reopen.DataDir = dir
	reopen.RequireExisting = true
	s, err := New(reopen)
	if err != nil {
		t.Fatalf("reopen New(RequireExisting): %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	// Insert probes reference this session, and observations.session_id is a
	// foreign key, so create it through the store API.
	if err := s.CreateSession("ses-txlock", "proj", dir); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return s, filepath.Join(dir, "recalldock.db")
}

// TestStoreReopenUsesImmediateTransactions proves the option reaches the real
// connection: the store's own transaction must hold the write lock from BEGIN,
// so an independent connection's write is blocked for as long as it is open.
func TestStoreReopenUsesImmediateTransactions(t *testing.T) {
	s, dbPath := openStoreWithExistingDB(t)

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin store transaction: %v", err)
	}
	// A read is enough to establish the transaction is open; under
	// _txlock=immediate the write lock is already held at this point.
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM observations`).Scan(&n); err != nil {
		_ = tx.Rollback()
		t.Fatalf("read inside store transaction: %v", err)
	}

	// An independent connection with no busy timeout must not be able to write.
	other, err := sql.Open("sqlite", sqliteFileURI(dbPath, "mode=rw&_txlock=immediate&_pragma=busy_timeout(0)"))
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("open second connection: %v", err)
	}
	defer other.Close()
	other.SetMaxOpenConns(1)

	// BEGIN IMMEDIATE on the second connection must fail while the store holds
	// the write lock. If the store were DEFERRED this BEGIN would succeed,
	// because a read lock does not block another connection's BEGIN.
	otx, beginErr := other.Begin()
	if beginErr == nil {
		// The begin succeeded, which is only consistent with the store not
		// holding the write lock. Confirm by attempting the write.
		_, writeErr := otx.Exec(`INSERT INTO observations (session_id, title, content, tool_name, type, project)
			VALUES ('ses-txlock', 'probe', 'blocked write probe', 'cli', 'note', 'proj')`)
		_ = otx.Rollback()
		_ = tx.Rollback()
		if writeErr == nil {
			t.Fatal("store transaction did not hold the write lock: a concurrent writer succeeded")
		}
		if !isBusyError(writeErr) {
			t.Fatalf("concurrent write failed for an unexpected reason: %v", writeErr)
		}
		return
	}
	_ = tx.Rollback()
	if !isBusyError(beginErr) {
		t.Fatalf("second BEGIN failed for an unexpected reason: %v", beginErr)
	}
}

// TestStoreReopenReleasesWriteLockOnCommit proves the immediate lock is released
// properly, so the setting cannot deadlock normal sequential use.
func TestStoreReopenReleasesWriteLockOnCommit(t *testing.T) {
	s, dbPath := openStoreWithExistingDB(t)

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit empty transaction: %v", err)
	}

	other, err := sql.Open("sqlite", sqliteFileURI(dbPath, "mode=rw&_txlock=immediate&_pragma=busy_timeout(1000)"))
	if err != nil {
		t.Fatalf("open second connection: %v", err)
	}
	defer other.Close()
	other.SetMaxOpenConns(1)

	otx, err := other.Begin()
	if err != nil {
		t.Fatalf("write lock was not released after commit: %v", err)
	}
	if _, err := otx.Exec(`INSERT INTO observations (session_id, title, content, tool_name, type, project)
		VALUES ('ses-txlock', 'probe', 'after commit', 'cli', 'note', 'proj')`); err != nil {
		_ = otx.Rollback()
		t.Fatalf("write after commit failed: %v", err)
	}
	if err := otx.Commit(); err != nil {
		t.Fatalf("commit on second connection: %v", err)
	}
}

// TestStoreReopenTransactionRollbackLeavesDataIntact proves a failed transaction
// neither commits partial work nor corrupts the database, which the lock mode
// change must not alter.
func TestStoreReopenTransactionRollbackLeavesDataIntact(t *testing.T) {
	s, _ := openStoreWithExistingDB(t)

	var before int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM observations`).Scan(&before); err != nil {
		t.Fatalf("count before: %v", err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO observations (session_id, title, content, tool_name, type, project)
		VALUES ('ses-txlock', 'probe', 'rolled back', 'cli', 'note', 'proj')`); err != nil {
		_ = tx.Rollback()
		t.Fatalf("insert: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	var after int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM observations`).Scan(&after); err != nil {
		t.Fatalf("count after: %v", err)
	}
	if after != before {
		t.Fatalf("rollback did not discard the row: %d -> %d", before, after)
	}
	var integrity string
	if err := s.db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if integrity != "ok" {
		t.Fatalf("database integrity after rollback: %s", integrity)
	}
}

// TestStoreLockWaitIsBounded proves a blocked writer gives up within its
// configured busy timeout instead of hanging forever. This does not claim the
// setting eliminates SQLITE_BUSY; it bounds how long the wait lasts.
func TestStoreLockWaitIsBounded(t *testing.T) {
	_, dbPath := openStoreWithExistingDB(t)

	holder, err := sql.Open("sqlite", sqliteFileURI(dbPath, "mode=rw&_txlock=immediate&_pragma=busy_timeout(0)"))
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	defer holder.Close()
	holder.SetMaxOpenConns(1)
	htx, err := holder.Begin()
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	if _, err := htx.Exec(`INSERT INTO observations (session_id, title, content, tool_name, type, project)
		VALUES ('ses-txlock', 'probe', 'holder', 'cli', 'note', 'proj')`); err != nil {
		_ = htx.Rollback()
		t.Fatalf("holder insert: %v", err)
	}

	waiter, err := sql.Open("sqlite", sqliteFileURI(dbPath, "mode=rw&_txlock=immediate&_pragma=busy_timeout(300)"))
	if err != nil {
		_ = htx.Rollback()
		t.Fatalf("open waiter: %v", err)
	}
	defer waiter.Close()
	waiter.SetMaxOpenConns(1)

	start := time.Now()
	_, beginErr := waiter.Begin()
	elapsed := time.Since(start)
	_ = htx.Rollback()

	if beginErr == nil {
		t.Fatal("waiter acquired a lock the holder retained")
	}
	if !isBusyError(beginErr) {
		t.Fatalf("waiter failed for an unexpected reason: %v", beginErr)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("waiter blocked for %v; busy_timeout(300ms) was not honoured", elapsed)
	}
}

// isBusyError reports whether err is SQLITE_BUSY or SQLITE_LOCKED.
func isBusyError(err error) bool {
	if err == nil {
		return false
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		code := se.Code() & 0xff
		return code == 5 || code == 6
	}
	return false
}
