package store

import (
	"strings"
	"testing"
)

// TestMergedProjectMovesSyncOwnershipToCanonical pins the contract that cloud
// sync ownership is current state: enrollment, pending outbound mutations,
// chunk dedup tracking and cloud upgrade checkpoints follow the project that
// absorbed the data. The retired key must not keep any of them.
func TestMergedProjectMovesSyncOwnershipToCanonical(t *testing.T) {
	s := newTestStore(t)
	const source = "legacy-sync-source"
	const target = "canonical-sync-target"

	if err := s.CreateSession("legacy-session", source, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := s.EnrollProject(source); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCloudUpgradeState(CloudUpgradeState{
		Project: source,
		Stage:   UpgradeStageBootstrapEnrolled,
		Snapshot: CloudUpgradeSnapshot{
			CloudConfigPresent: true,
			ProjectEnrolled:    true,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSyncedChunkForTarget(syncTargetKeyForProject(source), "chunk-1"); err != nil {
		t.Fatal(err)
	}

	enrolledBefore, err := s.IsProjectEnrolled(source)
	if err != nil || !enrolledBefore {
		t.Fatalf("source not enrolled before merge: %v %v", enrolledBefore, err)
	}
	var mutationCount int
	if err := s.db.QueryRow(`SELECT count(*) FROM sync_mutations WHERE project=?`, source).Scan(&mutationCount); err != nil || mutationCount == 0 {
		t.Fatalf("source has no outbound mutation before merge: %d %v", mutationCount, err)
	}

	result, err := s.MergeProjects([]string{source}, target)
	if err != nil {
		t.Fatal(err)
	}
	if result.SyncRecordsUpdated == 0 {
		t.Fatal("merge reported no sync ownership movement")
	}

	if enrolled, err := s.IsProjectEnrolled(source); err != nil || enrolled {
		t.Fatalf("retired source kept enrollment: %v %v", enrolled, err)
	}
	if enrolled, err := s.IsProjectEnrolled(target); err != nil || !enrolled {
		t.Fatalf("canonical target not enrolled after merge: %v %v", enrolled, err)
	}

	var sourceMutations, targetMutations int
	if err := s.db.QueryRow(`SELECT count(*) FROM sync_mutations WHERE project=?`, source).Scan(&sourceMutations); err != nil || sourceMutations != 0 {
		t.Fatalf("retired source kept outbound mutations: %d %v", sourceMutations, err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM sync_mutations WHERE project=?`, target).Scan(&targetMutations); err != nil || targetMutations == 0 {
		t.Fatalf("canonical target did not inherit outbound mutations: %d %v", targetMutations, err)
	}

	var sourceCheckpoints, targetCheckpoints int
	if err := s.db.QueryRow(`SELECT count(*) FROM cloud_upgrade_state WHERE project=?`, source).Scan(&sourceCheckpoints); err != nil || sourceCheckpoints != 0 {
		t.Fatalf("retired source kept cloud upgrade checkpoint: %d %v", sourceCheckpoints, err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM cloud_upgrade_state WHERE project=?`, target).Scan(&targetCheckpoints); err != nil || targetCheckpoints != 1 {
		t.Fatalf("canonical target did not inherit cloud upgrade checkpoint: %d %v", targetCheckpoints, err)
	}
	state, err := s.GetCloudUpgradeState(target)
	if err != nil || state == nil || state.Stage != UpgradeStageBootstrapEnrolled {
		t.Fatalf("cloud upgrade checkpoint content lost: %+v %v", state, err)
	}

	var sourceChunks, targetChunks int
	if err := s.db.QueryRow(`SELECT count(*) FROM sync_chunks WHERE target_key=?`, syncTargetKeyForProject(source)).Scan(&sourceChunks); err != nil || sourceChunks != 0 {
		t.Fatalf("retired source kept chunk tracking: %d %v", sourceChunks, err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM sync_chunks WHERE target_key=?`, syncTargetKeyForProject(target)).Scan(&targetChunks); err != nil || targetChunks != 1 {
		t.Fatalf("canonical target did not inherit chunk tracking: %d %v", targetChunks, err)
	}

	// The retired key is fenced for every write path it used to own.
	if err := s.EnrollProject(source); err == nil {
		t.Fatal("retired source was re-enrolled")
	}
	if err := s.SaveCloudUpgradeState(CloudUpgradeState{Project: source, Stage: UpgradeStageBootstrapEnrolled}); err == nil {
		t.Fatal("retired source wrote a cloud upgrade checkpoint")
	}
	if _, err := s.db.Exec(
		`INSERT INTO sync_mutations (target_key, entity, entity_key, op, payload, source, project) VALUES (?,?,?,?,?,?,?)`,
		DefaultSyncTargetKey, SyncEntityObservation, "obs-stale", SyncOpUpsert, `{"sync_id":"obs-stale"}`, SyncSourceLocal, source,
	); err == nil || !strings.Contains(err.Error(), "project key retired") {
		t.Fatalf("retired source wrote an outbound mutation: %v", err)
	}
}

// TestRetiredProjectKeyFencesSyncStateWrites checks the write fence on the
// two sync-owned tables that were missing from the trigger list. A stale
// writer on an already-open connection must not resurrect a retired key.
func TestRetiredProjectKeyFencesSyncStateWrites(t *testing.T) {
	s := newTestStore(t)
	// The checkpoint row exists before the key is retired, so the fence is
	// exercised against a live row rather than a rejected insert.
	if _, err := s.db.Exec(`INSERT INTO cloud_upgrade_state(project,stage) VALUES('old-key','planned')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO project_key_retirements(source,target) VALUES('old-key','current-key')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO sync_mutations (target_key, entity, entity_key, op, payload, source, project) VALUES (?,?,?,?,?,?,?)`,
		DefaultSyncTargetKey, SyncEntityObservation, "obs-old", SyncOpUpsert, `{"sync_id":"obs-old"}`, SyncSourceLocal, "old-key",
	); err == nil || !strings.Contains(err.Error(), "project key retired") {
		t.Fatalf("stale connection wrote a retired sync mutation: %v", err)
	}
	if err := s.SaveCloudUpgradeState(CloudUpgradeState{Project: "old-key", Stage: UpgradeStageBootstrapEnrolled}); err == nil {
		t.Fatal("stale connection upserted a retired cloud upgrade checkpoint")
	}
	if err := s.EnrollProject("old-key"); err == nil {
		t.Fatal("stale connection re-enrolled a retired project")
	}
	// A live key keeps working on the same tables.
	if err := s.SaveCloudUpgradeState(CloudUpgradeState{Project: "live-key", Stage: UpgradeStageBootstrapEnrolled}); err != nil {
		t.Fatal("live key was fenced: ", err)
	}
	if err := s.EnrollProject("live-key"); err != nil {
		t.Fatal("live key enrollment was fenced: ", err)
	}
}

func TestMergeSnapshotPreservesAcknowledgedMutationHistory(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("prior", "source", "/synthetic"); err != nil {
		t.Fatal(err)
	}
	if err := s.EnrollProject("source"); err != nil {
		t.Fatal(err)
	}
	var seq int64
	var payload string
	if err := s.db.QueryRow(`SELECT seq,payload FROM sync_mutations WHERE project='source' ORDER BY seq DESC LIMIT 1`).Scan(&seq, &payload); err != nil {
		t.Fatal(err)
	}
	if err := s.AckSyncMutations(DefaultSyncTargetKey, seq); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MergeProjects([]string{"source"}, "target"); err != nil {
		t.Fatal(err)
	}
	var historicalPayload string
	var acknowledged bool
	if err := s.db.QueryRow(`SELECT payload,acked_at IS NOT NULL FROM sync_mutations WHERE seq=?`, seq).Scan(&historicalPayload, &acknowledged); err != nil || historicalPayload != payload || !acknowledged {
		t.Fatalf("merge rewrote original envelope or acknowledgement: %v", err)
	}
	var pending int
	if err := s.db.QueryRow(`SELECT count(*) FROM sync_mutations WHERE seq>? AND project='target' AND acked_at IS NULL AND json_extract(payload,'$.project')='target'`, seq).Scan(&pending); err != nil || pending == 0 {
		t.Fatalf("merge failed to enqueue current canonical snapshot: %d %v", pending, err)
	}
	if _, err := s.MergeProjects([]string{"source"}, "target"); err != nil {
		t.Fatal(err)
	}
	var afterReplay int
	if err := s.db.QueryRow(`SELECT count(*) FROM sync_mutations WHERE seq>? AND project='target' AND acked_at IS NULL`, seq).Scan(&afterReplay); err != nil || afterReplay != pending {
		t.Fatalf("mapping replay requeued the snapshot: %d => %d %v", pending, afterReplay, err)
	}
}

func TestMergeSnapshotFailureRollsBackOwnershipAndRetirement(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("prior", "source", "/synthetic"); err != nil {
		t.Fatal(err)
	}
	if err := s.EnrollProject("source"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_transfer_snapshot BEFORE INSERT ON sync_mutations WHEN NEW.project='target' BEGIN SELECT RAISE(ABORT,'synthetic snapshot failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MergeProjects([]string{"source"}, "target"); err == nil {
		t.Fatal("merge committed without a canonical snapshot")
	}
	session, err := s.GetSession("prior")
	if err != nil || session.Project != "source" {
		t.Fatalf("snapshot failure changed ownership: %+v %v", session, err)
	}
	if err := s.requireCurrentProjectKey("source"); err != nil {
		t.Fatalf("snapshot failure left a retired source: %v", err)
	}
	if enrolled, err := s.IsProjectEnrolled("source"); err != nil || !enrolled {
		t.Fatalf("snapshot failure lost enrollment: %v %v", enrolled, err)
	}
	var moved int
	if err := s.db.QueryRow(`SELECT count(*) FROM sync_mutations WHERE project='target'`).Scan(&moved); err != nil || moved != 0 {
		t.Fatalf("snapshot failure partially moved journal: %d %v", moved, err)
	}
}
