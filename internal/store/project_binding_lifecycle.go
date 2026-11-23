package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
)

type ProjectBindingState struct {
	ProjectBinding
	State string `json:"state"`
}

// ListProjectBindings is a bounded metadata index, including retired bindings.
func (s *Store) ListProjectBindings(id string, limit int, after string) ([]ProjectBindingState, error) {
	if limit < 1 || limit > 20 {
		return nil, errors.New("binding index limit must be 1..20")
	}
	if _, err := s.RegisteredProject(id); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT id,project_id,root_path,state FROM project_bindings WHERE project_id=? AND id>? ORDER BY id LIMIT ?`, id, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ProjectBindingState{}
	for rows.Next() {
		var b ProjectBindingState
		if err := rows.Scan(&b.ID, &b.ProjectID, &b.RootPath, &b.State); err != nil {
			return nil, err
		}
		result = append(result, b)
	}
	return result, rows.Err()
}

// DeactivateProjectBinding changes only the explicitly selected database row.
// The old marker remains a fail-closed fence against inheriting a parent root.
// A missing/moved root is selectable by its recorded absolute path and ID.
func (s *Store) DeactivateProjectBinding(id, root, expected string) (ProjectBindingState, error) {
	var b ProjectBindingState
	if !filepath.IsAbs(root) || strings.TrimSpace(expected) == "" {
		return b, errors.New("unbind requires an absolute recorded path and expected binding ID")
	}
	root = resolvePathPrefix(root)
	err := s.withTx(func(tx *sql.Tx) error {
		err := tx.QueryRow(`SELECT id,project_id,root_path,state FROM project_bindings WHERE id=? AND project_id=?`, expected, id).Scan(&b.ID, &b.ProjectID, &b.RootPath, &b.State)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !samePath(b.RootPath, root)) {
			return errors.New("binding selection mismatch; inspect projects bindings before retrying")
		}
		if err != nil {
			return err
		}
		if b.State == "superseded" {
			return nil
		}
		if b.State != "active" {
			return errors.New("unsupported binding state")
		}
		if _, err = tx.Exec(`UPDATE project_bindings SET state='superseded' WHERE id=? AND state='active'`, expected); err != nil {
			return err
		}
		b.State = "superseded"
		return nil
	})
	return b, err
}
