package store

import (
	"database/sql"
	"errors"
	"fmt"
)

var ErrRetiredProjectKey = errors.New("project key retired; select a current project explicitly")

// This is a write fence, not an alias resolver. Historical versions/audits and
// outbound mutation envelopes keep their original provenance. Database triggers
// also fence writers running an older binary on an already-open connection.
func (s *Store) installProjectRetirement() error {
	return s.withTx(func(tx *sql.Tx) error {
		if _, err := s.execHook(tx, `CREATE TABLE IF NOT EXISTS project_key_retirements (
 source TEXT PRIMARY KEY, target TEXT NOT NULL, retired_at TEXT NOT NULL DEFAULT (datetime('now')),
 CHECK(source <> target))`); err != nil {
			return err
		}
		for _, entry := range []struct{ table, column string }{
			{"sessions", "project"}, {"observations", "project"}, {"user_prompts", "project"},
			{"prompt_tombstones", "project"}, {"observation_tombstones", "project"}, {"sync_enrolled_projects", "project"},
			{"memory_candidates", "project"}, {"memory_nodes", "project"},
			{"memory_dependencies", "project"}, {"resource_dependencies", "project"},
			{"read_budgets", "project"}, {"project_bindings", "project_id"},
			{"sync_mutations", "project"}, {"cloud_upgrade_state", "project"},
		} {
			for _, operation := range []struct{ suffix, clause string }{{"insert", "INSERT"}, {"update", "UPDATE OF " + entry.column}} {
				// Identifiers are fixed compile-time constants, never caller input.
				query := fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS project_retired_%s_%s
 BEFORE %s ON %s WHEN EXISTS(SELECT 1 FROM project_key_retirements WHERE source=NEW.%s)
 BEGIN SELECT RAISE(ABORT, 'project key retired; select a current project explicitly'); END;`, entry.table, operation.suffix, operation.clause, entry.table, entry.column)
				if _, err := s.execHook(tx, query); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func projectKeyRetirementTx(tx *sql.Tx, key string) (string, error) {
	var target string
	err := tx.QueryRow(`SELECT target FROM project_key_retirements WHERE source=?`, key).Scan(&target)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return target, err
}

func (s *Store) requireCurrentProjectKey(key string) error {
	var exists int
	if err := s.db.QueryRow(`SELECT count(*) FROM project_key_retirements WHERE source=?`, key).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return ErrRetiredProjectKey
	}
	return nil
}

func (s *Store) retireMergedKeysTx(tx *sql.Tx, sources []string, target string, changed bool) (int64, error) {
	if !changed {
		args := make([]any, len(sources))
		for i, source := range sources {
			args[i] = source
		}
		var registered int
		if err := tx.QueryRow(`SELECT count(*) FROM project_identities WHERE id IN (`+sqlPlaceholders(len(sources))+`)`, args...).Scan(&registered); err != nil {
			return 0, err
		}
		if registered == 0 {
			return 0, nil
		} // A nonexistent typo is not reserved.
	}
	var count int64
	for _, source := range sources {
		result, err := s.execHook(tx, `INSERT OR IGNORE INTO project_key_retirements(source,target) VALUES(?,?)`, source, target)
		if err != nil {
			return 0, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		count += n
	}
	return count, nil
}
