package store

import (
	"database/sql"
	"errors"
	"os"
	"recalldock.local/recalldock/internal/project"
)

// PruneMissingRegisteredProject only removes empty identities whose bound roots
// no longer exist. Observations (including deleted ones) and candidates protect it.
func (s *Store) PruneMissingRegisteredProject(id string, apply bool) error {
	if !project.IsProjectID(id) {
		return errors.New("exact registered project ID required")
	}
	return s.withTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE project_identities SET name=name WHERE id=?`, id); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM project_identities WHERE id=?`, id).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return ErrUnknownRegisteredProject
		}
		if err := tx.QueryRow(`SELECT (SELECT COUNT(*) FROM observations WHERE project=?)+(SELECT COUNT(*) FROM memory_candidates WHERE project=?)`, id, id).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return errors.New("project contains observations or candidates")
		}
		rows, err := tx.Query(`SELECT root_path FROM project_bindings WHERE project_id=?`, id)
		if err != nil {
			return err
		}
		roots := []string{}
		for rows.Next() {
			var path string
			if err = rows.Scan(&path); err != nil {
				rows.Close()
				return err
			}
			roots = append(roots, path)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(roots) == 0 {
			return errors.New("project has no binding; use `projects delete --project ID --confirm` for an empty unbound identity")
		}
		for _, path := range roots {
			_, err := os.Lstat(path)
			if !os.IsNotExist(err) {
				return errors.New("bound root exists or cannot be verified missing")
			}
		}
		if !apply {
			return nil
		}
		for _, q := range []string{`DELETE FROM user_prompts WHERE project=?`, `DELETE FROM sessions WHERE project=?`, `DELETE FROM read_budgets WHERE project=?`, `DELETE FROM project_aliases WHERE project_id=?`, `DELETE FROM project_bindings WHERE project_id=?`, `DELETE FROM project_identities WHERE id=?`} {
			if _, err := tx.Exec(q, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteRegisteredProject removes a never-bound, empty identity. It is explicit
// operator input: any binding, observation, session, prompt or candidate blocks
// the delete so data cannot be orphaned by a typo.
func (s *Store) DeleteRegisteredProject(id string, confirm bool) error {
	if !confirm {
		return errors.New("project delete requires --confirm")
	}
	if !project.IsProjectID(id) {
		return errors.New("exact registered project ID required")
	}
	return s.withTx(func(tx *sql.Tx) error {
		var name string
		if err := tx.QueryRow(`SELECT name FROM project_identities WHERE id=?`, id).Scan(&name); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrUnknownRegisteredProject
			}
			return err
		}
		var bindings int
		if err := tx.QueryRow(`SELECT count(*) FROM project_bindings WHERE project_id=?`, id).Scan(&bindings); err != nil {
			return err
		}
		if bindings != 0 {
			return errors.New("project still has bindings; unbind and prune them first")
		}
		var records int
		if err := tx.QueryRow(`SELECT
			(SELECT count(*) FROM observations WHERE project=?)+
			(SELECT count(*) FROM sessions WHERE project=?)+
			(SELECT count(*) FROM user_prompts WHERE project=?)+
			(SELECT count(*) FROM memory_candidates WHERE project=?)`, id, id, id, id).Scan(&records); err != nil {
			return err
		}
		if records != 0 {
			return errors.New("project still contains observations/sessions/prompts/candidates; refusing delete")
		}
		if _, err := tx.Exec(`DELETE FROM project_aliases WHERE project_id=?`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM project_identities WHERE id=?`, id); err != nil {
			return err
		}
		_ = name
		return nil
	})
}
