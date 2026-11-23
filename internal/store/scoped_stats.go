package store

import (
	"fmt"
	"strings"
)

// StatsForProject counts exactly one project; an empty project is never global.
// Keep Stats as the explicitly global trusted/operator API.
func (s *Store) StatsForProject(project string) (*Stats, error) {
	project = s.canonicalProjectOrRaw(project)
	project = strings.TrimSpace(project)
	if project == "" {
		return nil, fmt.Errorf("project required for scoped stats")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	result := &Stats{Projects: []string{}}
	queries := []struct {
		query  string
		target *int
	}{
		{"SELECT COUNT(*) FROM sessions WHERE project = ?", &result.TotalSessions},
		{"SELECT COUNT(*) FROM observations WHERE project = ? AND deleted_at IS NULL", &result.TotalObservations},
		{"SELECT COUNT(*) FROM user_prompts WHERE project = ?", &result.TotalPrompts},
	}
	for _, q := range queries {
		if err := tx.QueryRow(q.query, project).Scan(q.target); err != nil {
			return nil, err
		}
	}
	if result.TotalObservations > 0 {
		result.Projects = append(result.Projects, project)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}
