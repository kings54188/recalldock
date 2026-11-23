package store

import (
	"database/sql"
	"errors"
	"fmt"
)

func (s *Store) validatePulledMutationProjectTx(tx *sql.Tx, mutation SyncMutation, project string) error {
	if outer, _ := NormalizeProject(mutation.Project); outer != project {
		return errors.New("project_scope_mismatch: mutation belongs to another project")
	}
	var payload struct {
		ID                         string `json:"id"`
		SyncID                     string `json:"sync_id"`
		Project                    string `json:"project"`
		SessionID                  string `json:"session_id"`
		SourceID                   string `json:"source_id"`
		TargetID                   string `json:"target_id"`
		SourceObservationSyncID    string `json:"source_observation_sync_id"`
		DependentObservationSyncID string `json:"dependent_observation_sync_id"`
	}
	if err := decodeSyncPayload([]byte(mutation.Payload), &payload); err != nil {
		return err
	}
	identity := payload.SyncID
	if mutation.Entity == SyncEntitySession {
		identity = payload.ID
	}
	if identity == "" || identity != mutation.EntityKey {
		return errors.New("project_scope_mismatch: payload identity must match mutation entity_key")
	}
	if inner, _ := NormalizeProject(payload.Project); inner != project {
		return errors.New("project_scope_mismatch: payload belongs to another project")
	}
	check := func(query string, key string) error {
		if key == "" {
			return nil
		}
		var owner string
		err := tx.QueryRow(query, key).Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if owner != project {
			return fmt.Errorf("project_scope_mismatch: existing resource belongs to another project")
		}
		return nil
	}
	observationOwner := `SELECT coalesce(nullif(o.project,''),s.project,'') FROM observations o LEFT JOIN sessions s ON s.id=o.session_id WHERE o.sync_id=?`
	for _, key := range []string{payload.SourceID, payload.TargetID, payload.SourceObservationSyncID, payload.DependentObservationSyncID} {
		if err := check(observationOwner, key); err != nil {
			return err
		}
	}
	if err := check(`SELECT project FROM sessions WHERE id=?`, payload.SessionID); err != nil {
		return err
	}
	switch mutation.Entity {
	case SyncEntityObservation:
		if err := check(observationOwner, mutation.EntityKey); err != nil {
			return err
		}
		return check(`SELECT ifnull(project,'') FROM observation_tombstones WHERE sync_id=?`, mutation.EntityKey)
	case SyncEntityPrompt:
		if err := check(`SELECT coalesce(nullif(p.project,''),s.project,'') FROM user_prompts p LEFT JOIN sessions s ON s.id=p.session_id WHERE p.sync_id=?`, mutation.EntityKey); err != nil {
			return err
		}
		return check(`SELECT ifnull(project,'') FROM prompt_tombstones WHERE sync_id=?`, mutation.EntityKey)
	case SyncEntitySession:
		if err := check(`SELECT project FROM sessions WHERE id=?`, mutation.EntityKey); err != nil {
			return err
		}
		var foreign int
		if err := tx.QueryRow(`SELECT (SELECT count(*) FROM observations WHERE session_id=? AND ifnull(project,'')!=?)+(SELECT count(*) FROM user_prompts WHERE session_id=? AND ifnull(project,'')!=?)`, mutation.EntityKey, project, mutation.EntityKey, project).Scan(&foreign); err != nil {
			return err
		}
		if foreign != 0 {
			return errors.New("project_scope_mismatch: session has foreign resources")
		}
	case SyncEntityRelation:
		var p syncRelationPayload
		if err := decodeSyncPayload([]byte(mutation.Payload), &p); err != nil {
			return err
		}
		return check(`SELECT ifnull(o.project,'') FROM memory_relations r LEFT JOIN observations o ON o.sync_id=r.source_id WHERE r.sync_id=?`, mutation.EntityKey)
	case SyncEntityDependency:
		return check(`SELECT project FROM memory_dependencies WHERE sync_id=?`, mutation.EntityKey)
	case SyncEntityResourceDependency:
		return check(`SELECT project FROM resource_dependencies WHERE sync_id=?`, mutation.EntityKey)
	default:
		return errors.New("unsupported scoped sync entity")
	}
	return nil
}
