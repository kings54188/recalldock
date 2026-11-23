package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
	"recalldock.local/recalldock/internal/store"
)

func pendingMutationCount(t *testing.T, cfg store.Config, project string) int {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(cfg.DataDir, "recalldock.db"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM sync_mutations WHERE project = ? AND acked_at IS NULL`, project).Scan(&count); err != nil {
		t.Fatalf("count pending mutations: %v", err)
	}
	return count
}

func decodeSyncCleanupResult(t *testing.T, output string) map[string]any {
	t.Helper()
	var result map[string]any
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("invalid cleanup json: %v\n%s", err, output)
	}
	return result
}

func TestCmdSyncCleanupNonEnrolledDryRunAndApply(t *testing.T) {
	cfg := testConfig(t)
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if err := s.EnrollProject("protected"); err != nil {
		t.Fatalf("EnrollProject: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	seedDoctorPendingMutation(t, cfg, "orphaned", store.SyncEntityPrompt, "p-orphaned", store.SyncOpUpsert, `{"sync_id":"p-orphaned","session_id":"s","content":"payload"}`)
	seedDoctorPendingMutation(t, cfg, "protected", store.SyncEntityPrompt, "p-protected", store.SyncOpUpsert, `{"sync_id":"p-protected","session_id":"s","content":"payload"}`)
	seedDoctorPendingMutation(t, cfg, "", store.SyncEntityPrompt, "p-global", store.SyncOpUpsert, `{"sync_id":"p-global","session_id":"s","content":"payload"}`)

	withArgs(t, "engram", "sync", "cleanup", "--non-enrolled", "--dry-run", "--json")
	dryOut, dryErr := captureOutput(t, func() { cmdSync(cfg) })
	if dryErr != "" {
		t.Fatalf("dry-run stderr=%q", dryErr)
	}
	dry := decodeSyncCleanupResult(t, dryOut)
	if dry["status"] != "dry_run" || dry["total_count"] != float64(1) || dry["total_payload_bytes"].(float64) <= 0 {
		t.Fatalf("dry-run result=%v", dry)
	}
	if pendingMutationCount(t, cfg, "orphaned") != 1 {
		t.Fatal("dry-run must not acknowledge mutations")
	}

	withArgs(t, "engram", "sync", "cleanup", "--non-enrolled", "--apply", "--all", "--json")
	applyOut, applyErr := captureOutput(t, func() { cmdSync(cfg) })
	if applyErr != "" {
		t.Fatalf("apply stderr=%q", applyErr)
	}
	applied := decodeSyncCleanupResult(t, applyOut)
	if applied["status"] != "applied" || applied["acked_count"] != float64(1) {
		t.Fatalf("apply result=%v", applied)
	}
	backupPath, _ := applied["backup_path"].(string)
	if backupPath == "" {
		t.Fatal("apply must report an online backup path")
	}
	if _, err := os.Stat(backupPath); err != nil {
		t.Fatalf("backup missing: %v", err)
	}
	if pendingMutationCount(t, cfg, "orphaned") != 0 || pendingMutationCount(t, cfg, "protected") != 1 || pendingMutationCount(t, cfg, "") != 1 {
		t.Fatal("cleanup must only acknowledge non-enrolled, non-empty projects")
	}
}
