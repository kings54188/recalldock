package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

func validateSessionPromptOwnersTx(tx *sql.Tx, project, session string) error {
	var foreign int
	if err := tx.QueryRow(`SELECT count(*) FROM user_prompts p
		WHERE p.session_id IN (SELECT id FROM sessions WHERE project=? AND (?='' OR id=?))
		AND coalesce(nullif(p.project,''),?)!=?`, project, session, session, project, project).Scan(&foreign); err != nil {
		return err
	}
	if foreign != 0 {
		return fmt.Errorf("project_scope_mismatch: session contains prompts owned by another project")
	}
	return nil
}

func (s *Store) ExportDeletionMutations(project string) ([]SyncMutation, error) {
	project = s.canonicalProjectOrRaw(project)
	mutations := []SyncMutation{}
	for _, table := range []struct{ name, entity string }{{"observation_tombstones", SyncEntityObservation}, {"prompt_tombstones", SyncEntityPrompt}} {
		query := "SELECT sync_id,ifnull(session_id,''),ifnull(project,''),deleted_at FROM " + table.name
		args := []any{}
		if project != "" {
			query += " WHERE project=?"
			args = append(args, project)
		}
		query += " ORDER BY deleted_at,sync_id"
		rows, err := s.db.Query(query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, session, owner, deleted string
			if err := rows.Scan(&id, &session, &owner, &deleted); err != nil {
				rows.Close()
				return nil, err
			}
			payload := map[string]any{"sync_id": id, "session_id": session, "project": owner, "deleted": true, "deleted_at": deleted}
			raw, err := json.Marshal(payload)
			if err != nil {
				rows.Close()
				return nil, err
			}
			mutations = append(mutations, SyncMutation{Entity: table.entity, EntityKey: id, Project: owner, Op: SyncOpDelete, OccurredAt: deleted, Payload: string(raw)})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return mutations, nil
}
func (s *Store) recordProjectDeletesTx(tx *sql.Tx, project string, hard bool) error {
	rows, err := tx.Query(`SELECT sync_id,session_id,project FROM observations WHERE project=?`, project)
	if err != nil {
		return err
	}
	var observations []syncObservationPayload
	now := Now()
	for rows.Next() {
		var p syncObservationPayload
		if err := rows.Scan(&p.SyncID, &p.SessionID, &p.Project); err != nil {
			rows.Close()
			return err
		}
		p.Deleted, p.HardDelete, p.DeletedAt = true, hard, &now
		observations = append(observations, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, p := range observations {
		if err := s.invalidateDependentsTx(tx, p.SyncID, p.SyncID, 0, "source_deleted", true); err != nil {
			return err
		}
		if _, err := s.execHook(tx, `INSERT INTO observation_tombstones(sync_id,session_id,project,deleted_at)
			VALUES(?,?,?,?) ON CONFLICT(sync_id) DO UPDATE SET deleted_at=excluded.deleted_at`, p.SyncID, p.SessionID, p.Project, now); err != nil {
			return err
		}
		if err := s.enqueueSyncMutationTx(tx, SyncEntityObservation, p.SyncID, SyncOpDelete, p); err != nil {
			return err
		}
	}
	rows, err = tx.Query(`SELECT sync_id,session_id,project FROM user_prompts WHERE project=?`, project)
	if err != nil {
		return err
	}
	var prompts []syncPromptPayload
	for rows.Next() {
		var p syncPromptPayload
		if err := rows.Scan(&p.SyncID, &p.SessionID, &p.Project); err != nil {
			rows.Close()
			return err
		}
		p.Deleted, p.HardDelete, p.DeletedAt = true, true, &now
		prompts = append(prompts, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, p := range prompts {
		if _, err := s.execHook(tx, `INSERT INTO prompt_tombstones(sync_id,session_id,project,deleted_at)
			VALUES(?,?,?,?) ON CONFLICT(sync_id) DO UPDATE SET deleted_at=excluded.deleted_at`, p.SyncID, p.SessionID, p.Project, now); err != nil {
			return err
		}
		if err := s.enqueueSyncMutationTx(tx, SyncEntityPrompt, p.SyncID, SyncOpDelete, p); err != nil {
			return err
		}
	}
	if hard {
		rows, err = tx.Query(`SELECT id FROM sessions WHERE project=?`, project)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, id := range ids {
			if err := s.enqueueSyncMutationTx(tx, SyncEntitySession, id, SyncOpDelete, syncSessionPayload{ID: id, Project: project, Deleted: true, HardDelete: true, DeletedAt: &now}); err != nil {
				return err
			}
		}
	}
	if _, err := s.execHook(tx, `DELETE FROM memory_nodes WHERE project=?`, project); err != nil {
		return err
	}
	if _, err := s.execHook(tx, `DELETE FROM memory_dependencies WHERE project=?`, project); err != nil {
		return err
	}
	if _, err := s.execHook(tx, `DELETE FROM resource_dependencies WHERE project=?`, project); err != nil {
		return err
	}
	_, err = s.execHook(tx, `DELETE FROM dependency_invalidation_queue WHERE source_observation_sync_id IN (SELECT sync_id FROM observations WHERE project=?)`, project)
	return err
}
