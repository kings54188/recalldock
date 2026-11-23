package store

import (
	"errors"
	"strings"
)

type ArchivedMemorySummary struct {
	ID         int64  `json:"id"`
	SyncID     string `json:"sync_id"`
	Project    string `json:"project"`
	Title      string `json:"title"`
	ArchivedAt string `json:"archived_at"`
	Revision   int    `json:"revision_count"`
}

// ArchivedMemories is a bounded operator index, not a second knowledge store.
// Archive state remains in the original observation, versions and backups.
func (s *Store) ArchivedMemories(project string, before int64, limit int) ([]ArchivedMemorySummary, error) {
	project, _ = NormalizeProject(project)
	if strings.TrimSpace(project) == "" || before < 0 {
		return nil, errors.New("archived index requires an explicit project and nonnegative cursor")
	}
	if limit < 1 {
		limit = 5
	}
	if limit > 20 {
		limit = 20
	}
	rows, err := s.db.Query(`SELECT id,ifnull(sync_id,''),ifnull(project,''),title,archived_at,revision_count FROM observations
	 WHERE project=? AND deleted_at IS NULL AND archived_at IS NOT NULL AND (?=0 OR id<?) ORDER BY id DESC LIMIT ?`, project, before, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []ArchivedMemorySummary{}
	for rows.Next() {
		var item ArchivedMemorySummary
		if err := rows.Scan(&item.ID, &item.SyncID, &item.Project, &item.Title, &item.ArchivedAt, &item.Revision); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
