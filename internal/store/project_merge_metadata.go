package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (s *Store) validateProjectMergeTx(tx *sql.Tx, sources []string, target string) error {
	if retired, err := projectKeyRetirementTx(tx, target); err != nil {
		return err
	} else if retired != "" {
		return ErrRetiredProjectKey
	}
	if isRegisteredProjectID(target) {
		var found string
		if err := tx.QueryRow("SELECT id FROM project_identities WHERE id=?", target).Scan(&found); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrUnknownRegisteredProject
			}
			return err
		}
	}
	for _, source := range sources {
		if retired, err := projectKeyRetirementTx(tx, source); err != nil {
			return err
		} else if retired != "" && retired != target {
			return ErrRetiredProjectKey
		}
		if !isRegisteredProjectID(source) {
			continue
		}
		var active int
		if err := tx.QueryRow("SELECT count(*) FROM project_bindings WHERE project_id=? AND state='active'", source).Scan(&active); err != nil {
			return err
		}
		if active > 0 {
			return fmt.Errorf("source %s has active bindings; explicitly rebind/deactivate the selected roots before merging its data", source)
		}
	}
	return nil
}

// Migrate only current ownership. Audit entries and immutable observation
// versions retain their original provenance; a new migration audit links them.
// Old read activations expire rather than gaining rights to the target project.
func (s *Store) mergeProjectMetadataTx(tx *sql.Tx, sources []string, target string, result *MergeResult) error {
	marks := sqlPlaceholders(len(sources))
	args := []any{target}
	for _, source := range sources {
		args = append(args, source)
	}
	if _, err := s.execHook(tx, `INSERT INTO memory_candidate_audit(candidate_sync_id,session_id,project,category,action,reason,created_at)
 SELECT sync_id,session_id,?,category,'project_migrated','explicit_project_mapping',datetime('now') FROM memory_candidates WHERE project IN (`+marks+`)`, args...); err != nil {
		return err
	}
	if isRegisteredProjectID(target) {
		if _, err := s.execHook(tx, `UPDATE project_aliases SET project_id=? WHERE project_id IN (`+marks+`)`, args...); err != nil {
			return fmt.Errorf("migrate project aliases: %w", err)
		}
	} else {
		if _, err := s.execHook(tx, `DELETE FROM project_aliases WHERE project_id IN (`+marks+`)`, args[1:]...); err != nil {
			return fmt.Errorf("drop retired project aliases: %w", err)
		}
	}
	for _, table := range []string{"memory_candidates", "memory_dependencies", "resource_dependencies", "prompt_tombstones", "observation_tombstones"} {
		updated, err := s.execHook(tx, "UPDATE "+table+" SET project=? WHERE project IN ("+marks+")", args...)
		if err != nil {
			return fmt.Errorf("migrate %s ownership: %w", table, err)
		}
		n, err := updated.RowsAffected()
		if err != nil {
			return err
		}
		result.AssociatedRecordsUpdated += n
	}
	expiryArgs := []any{}
	for _, source := range sources {
		expiryArgs = append(expiryArgs, source)
	}
	expired, err := s.execHook(tx, "UPDATE read_budgets SET expires_at='1970-01-01T00:00:00Z' WHERE project IN ("+marks+") AND datetime(expires_at)>datetime('now')", expiryArgs...)
	if err != nil {
		return err
	}
	n, err := expired.RowsAffected()
	if err != nil {
		return err
	}
	result.ActivationsExpired += n
	for _, scope := range []string{"project", "personal", "global"} {
		for _, source := range sources {
			if err := s.rebuildMemoryNodesTx(tx, source, scope); err != nil {
				return err
			}
		}
		if err := s.rebuildMemoryNodesTx(tx, target, scope); err != nil {
			return err
		}
	}
	return s.migrateProjectSyncStateTx(tx, sources, target, result)
}

// migrateProjectSyncStateTx moves current cloud-sync ownership from the merged
// sources to the canonical project.
//
// Enrollment, pending outbound mutations and cloud upgrade checkpoints are
// current ownership, so they follow the project. Mutation payloads keep their
// original provenance: the payload project is rewritten at export time, and
// rewriting stored envelopes would forge history. Audit rows are untouched.
func (s *Store) migrateProjectSyncStateTx(tx *sql.Tx, sources []string, target string, result *MergeResult) error {
	marks := sqlPlaceholders(len(sources))
	sourceArgs := make([]any, 0, len(sources))
	for _, source := range sources {
		sourceArgs = append(sourceArgs, source)
	}
	targetArgs := append([]any{target}, sourceArgs...)

	// 1. Pending outbound mutations: the ownership column moves so the
	//    canonical project still selects them for export. The payload keeps
	//    its original provenance.
	movedMutations, err := s.execHook(tx, "UPDATE sync_mutations SET project=? WHERE project IN ("+marks+")", targetArgs...)
	if err != nil {
		return fmt.Errorf("migrate sync_mutations ownership: %w", err)
	}
	movedMutationCount, _ := movedMutations.RowsAffected()
	result.SyncRecordsUpdated += movedMutationCount

	// 2. Enrollment is current ownership: the canonical project inherits it
	//    with the earliest enrollment time, and the retired source loses it.
	var enrolledSources int
	if err := tx.QueryRow("SELECT count(*) FROM sync_enrolled_projects WHERE project IN ("+marks+")", sourceArgs...).Scan(&enrolledSources); err != nil {
		return err
	}
	if enrolledSources > 0 {
		var earliest sql.NullString
		if err := tx.QueryRow("SELECT min(enrolled_at) FROM sync_enrolled_projects WHERE project IN ("+marks+")", sourceArgs...).Scan(&earliest); err != nil {
			return err
		}
		if _, err := s.execHook(tx, "INSERT OR IGNORE INTO sync_enrolled_projects(project, enrolled_at) VALUES(?, datetime('now'))", target); err != nil {
			return fmt.Errorf("migrate sync enrollment: %w", err)
		}
		if earliest.Valid {
			if _, err := s.execHook(tx, "UPDATE sync_enrolled_projects SET enrolled_at=? WHERE project=? AND enrolled_at>?", earliest.String, target, earliest.String); err != nil {
				return fmt.Errorf("migrate sync enrollment time: %w", err)
			}
		}
		res, err := s.execHook(tx, "DELETE FROM sync_enrolled_projects WHERE project IN ("+marks+")", sourceArgs...)
		if err != nil {
			return fmt.Errorf("retire sync enrollment: %w", err)
		}
		if n, err := res.RowsAffected(); err == nil {
			result.SyncRecordsUpdated += n
		}
	}

	// 3. Cloud upgrade checkpoints are current state, not audit history. The
	//    canonical project keeps its own checkpoint; otherwise the oldest
	//    source checkpoint is adopted.
	var upgradeSources int
	if err := tx.QueryRow("SELECT count(*) FROM cloud_upgrade_state WHERE project IN ("+marks+")", sourceArgs...).Scan(&upgradeSources); err != nil {
		return err
	}
	if upgradeSources > 0 {
		var targetCheckpoints int
		if err := tx.QueryRow("SELECT count(*) FROM cloud_upgrade_state WHERE project=?", target).Scan(&targetCheckpoints); err != nil {
			return err
		}
		if targetCheckpoints == 0 {
			res, err := s.execHook(tx, "UPDATE cloud_upgrade_state SET project=? WHERE project=(SELECT project FROM cloud_upgrade_state WHERE project IN ("+marks+") ORDER BY updated_at ASC, project ASC LIMIT 1)", targetArgs...)
			if err != nil {
				return fmt.Errorf("migrate cloud upgrade state: %w", err)
			}
			if n, err := res.RowsAffected(); err == nil {
				result.SyncRecordsUpdated += n
			}
		}
		res, err := s.execHook(tx, "DELETE FROM cloud_upgrade_state WHERE project IN ("+marks+")", sourceArgs...)
		if err != nil {
			return fmt.Errorf("retire cloud upgrade state: %w", err)
		}
		if n, err := res.RowsAffected(); err == nil {
			result.SyncRecordsUpdated += n
		}
	}

	// 4. The per-project sync cursor is current state too. The canonical
	//    cursor is refreshed from the mutations it now owns, and a retired
	//    source cursor is dropped unless a live sync lease still holds it.
	if movedMutationCount > 0 || enrolledSources > 0 {
		if err := s.refreshProjectSyncStateTx(tx, target); err != nil {
			return fmt.Errorf("refresh canonical sync state: %w", err)
		}
		for _, source := range sources {
			if err := s.dropRetiredProjectSyncStateTx(tx, source); err != nil {
				return err
			}
		}
	}

	// 5. Chunk tracking is per-project export dedup state. Moving it keeps the
	//    canonical project from re-exporting chunks the retired project
	//    already exported.
	for _, source := range sources {
		sourceTargetKey := syncTargetKeyForProject(source)
		canonicalTargetKey := syncTargetKeyForProject(target)
		if sourceTargetKey == canonicalTargetKey {
			continue
		}
		res, err := s.execHook(tx, "UPDATE OR IGNORE sync_chunks SET target_key=? WHERE target_key=?", canonicalTargetKey, sourceTargetKey)
		if err != nil {
			return fmt.Errorf("migrate sync chunk tracking: %w", err)
		}
		if n, err := res.RowsAffected(); err == nil {
			result.SyncRecordsUpdated += n
		}
		if _, err := s.execHook(tx, "DELETE FROM sync_chunks WHERE target_key=?", sourceTargetKey); err != nil {
			return fmt.Errorf("retire sync chunk tracking: %w", err)
		}
	}
	return nil
}

// dropRetiredProjectSyncStateTx removes the per-project sync cursor of a
// retired project once its ownership moved to the canonical project. A cursor
// that still carries a live lease is left untouched so an in-flight sync is
// never orphaned.
func (s *Store) dropRetiredProjectSyncStateTx(tx *sql.Tx, source string) error {
	targetKey := syncTargetKeyForProject(source)
	var leaseOwner sql.NullString
	var leaseUntil sql.NullString
	err := tx.QueryRow("SELECT lease_owner, lease_until FROM sync_state WHERE target_key=?", targetKey).Scan(&leaseOwner, &leaseUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if strings.TrimSpace(leaseOwner.String) != "" {
		if until, parseErr := time.Parse(time.RFC3339, strings.TrimSpace(leaseUntil.String)); parseErr == nil && until.After(time.Now().UTC()) {
			return nil
		}
	}
	if _, err := s.execHook(tx, "DELETE FROM sync_state WHERE target_key=?", targetKey); err != nil {
		return fmt.Errorf("drop retired sync state: %w", err)
	}
	return nil
}
