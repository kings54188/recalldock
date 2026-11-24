package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"
)

// The database file is opened with _txlock=immediate so every transaction takes
// the write lock at BEGIN. Without it a transaction is DEFERRED: it acquires a
// read lock on its first read and only upgrades when it first writes, and SQLite
// does not consult the busy handler for that upgrade — the writer gets
// SQLITE_BUSY even with busy_timeout set.
//
// These tests establish the two facts the change is justified by:
//
//  1. A DEFERRED read-then-write transaction fails with SQLITE_BUSY when another
//     writer is active, despite busy_timeout.
//  2. The same workload under _txlock=immediate succeeds, because the write lock
//     is taken up front where the busy handler applies.
//
// They use raw connections rather than s.db, because the store pins its own pool
// to one connection and therefore cannot exhibit cross-connection contention.

func openRawDB(t *testing.T, path, query string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteFileURI(path, query))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func initRawDB(t *testing.T, path string) {
	t.Helper()
	db := openRawDB(t, path, "mode=rwc")
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS t (id INTEGER PRIMARY KEY AUTOINCREMENT, v TEXT)`,
		`INSERT INTO t (v) VALUES ('seed')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("init: %v", err)
		}
	}
}

func isBusy(err error) bool {
	if err == nil {
		return false
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		code := se.Code() & 0xff
		return code == 5 || code == 6 // SQLITE_BUSY, SQLITE_LOCKED
	}
	return false
}

// A DEFERRED read-then-write transaction must be shown to fail under contention.
// If this test ever stops failing, the justification for _txlock=immediate has
// changed and the setting should be re-evaluated rather than carried forward.
func TestDeferredTransactionGetsBusyUnderContention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	initRawDB(t, path)

	// Hold the write lock from a second connection for long enough that the
	// deferred transaction below reaches its upgrade while the lock is taken.
	holder := openRawDB(t, path, "mode=rwc&_txlock=immediate&_pragma=busy_timeout(0)")
	tx, err := holder.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE t SET v='held' WHERE id=1`); err != nil {
		t.Fatal(err)
	}

	reader := openRawDB(t, path, "mode=rwc&_pragma=busy_timeout(2000)")
	dtx, err := reader.Begin() // DEFERRED
	if err != nil {
		t.Fatal(err)
	}
	// Read first: this takes a read lock, not a write lock.
	var v string
	_ = dtx.QueryRow(`SELECT v FROM t WHERE id=1`).Scan(&v)
	// Now write: this must upgrade, and the upgrade must not wait on busy_timeout.
	_, upgradeErr := dtx.Exec(`INSERT INTO t (v) VALUES ('upgraded')`)
	_ = dtx.Rollback()
	_ = tx.Rollback()

	if !isBusy(upgradeErr) {
		t.Fatalf("deferred upgrade error = %v; expected SQLITE_BUSY/LOCKED to justify _txlock=immediate", upgradeErr)
	}
}

// The same contention with _txlock=immediate waits on the busy handler instead
// of failing, which is the behaviour the change buys.
func TestImmediateTransactionWaitsInsteadOfFailing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "i.db")
	initRawDB(t, path)

	holder := openRawDB(t, path, "mode=rwc&_txlock=immediate&_pragma=busy_timeout(0)")
	tx, err := holder.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE t SET v='held' WHERE id=1`); err != nil {
		t.Fatal(err)
	}

	waiter := openRawDB(t, path, "mode=rwc&_txlock=immediate&_pragma=busy_timeout(3000)")
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { _ = tx.Rollback() }) }
	defer release()

	// Release the holder while the waiter is blocked, so the waiter can proceed.
	go func() {
		time.Sleep(300 * time.Millisecond)
		release()
	}()

	start := time.Now()
	itx, err := waiter.Begin() // IMMEDIATE: takes the write lock here
	if err != nil {
		t.Fatalf("immediate begin should wait for the lock, got: %v", err)
	}
	elapsed := time.Since(start)
	if _, err := itx.Exec(`INSERT INTO t (v) VALUES ('waited')`); err != nil {
		t.Fatalf("insert after waiting: %v", err)
	}
	_ = itx.Commit()

	if elapsed < 200*time.Millisecond {
		t.Fatalf("immediate begin returned after %v; expected it to block until the holder released", elapsed)
	}
}

// A bounded sanity check that the immediate setting cannot livelock a serial
// workload: many short transactions from several goroutines all complete.
func TestImmediateTransactionsCompleteUnderSerializedWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")
	initRawDB(t, path)

	var wg sync.WaitGroup
	var failures atomic.Int32
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			db := openRawDB(t, path, "mode=rwc&_txlock=immediate&_pragma=busy_timeout(5000)")
			for i := 0; i < 25; i++ {
				err := func() error {
					tx, err := db.Begin()
					if err != nil {
						return err
					}
					defer tx.Rollback()
					var n int
					if err := tx.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n); err != nil {
						return err
					}
					if _, err := tx.Exec(`INSERT INTO t (v) VALUES ('w')`); err != nil {
						return err
					}
					return tx.Commit()
				}()
				if err != nil {
					failures.Add(1)
				}
			}
		}(w)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("serialized immediate transactions did not complete in 60s")
	}
	if n := failures.Load(); n > 0 {
		t.Fatalf("%d immediate transactions failed; busy_timeout should have covered them", n)
	}
}
