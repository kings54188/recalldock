package store

import "fmt"

func (s *Store) ResolveExistingProject(project string) (string, error) {
	key, err := s.CanonicalProjectKey(project)
	if err != nil {
		return "", err
	}
	exists, err := s.ProjectExists(key)
	if err != nil {
		return "", err
	}
	if key == "" || !exists {
		return "", fmt.Errorf("unknown_project: project %q is not in this store", QueryPreview(project))
	}
	return key, nil
}

// Resource ownership lookups deliberately avoid loading knowledge bodies,
// summaries or updating recall counters. Public transports use them before
// fetching an exact resource under an explicit/pinned project activation.
func (s *Store) ObservationProject(id int64) (string, error) {
	var project string
	err := s.db.QueryRow("SELECT ifnull(project,'') FROM observations WHERE id=? AND deleted_at IS NULL", id).Scan(&project)
	project, _ = NormalizeProject(project)
	return project, err
}

func (s *Store) SessionProject(id string) (string, error) {
	var project string
	err := s.db.QueryRow("SELECT project FROM sessions WHERE id=?", id).Scan(&project)
	project, _ = NormalizeProject(project)
	return project, err
}

func (s *Store) ObservationProjectIncludingDeleted(id int64) (string, error) {
	var project string
	err := s.db.QueryRow(`SELECT coalesce(nullif(o.project, ''), s.project, '')
		FROM observations o LEFT JOIN sessions s ON s.id=o.session_id WHERE o.id=?`, id).Scan(&project)
	project, _ = NormalizeProject(project)
	return project, err
}

func (s *Store) ObservationProjectBySyncID(syncID string) (string, error) {
	var project string
	err := s.db.QueryRow(`SELECT coalesce(nullif(o.project, ''), s.project, '')
		FROM observations o LEFT JOIN sessions s ON s.id=o.session_id WHERE o.sync_id=? AND o.deleted_at IS NULL`, syncID).Scan(&project)
	project, _ = NormalizeProject(project)
	return project, err
}

func (s *Store) PromptProject(id int64) (string, error) {
	var project string
	err := s.db.QueryRow(`SELECT coalesce(nullif(p.project, ''), s.project, '')
		FROM user_prompts p LEFT JOIN sessions s ON s.id=p.session_id WHERE p.id=?`, id).Scan(&project)
	project, _ = NormalizeProject(project)
	return project, err
}
