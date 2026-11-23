package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"recalldock.local/recalldock/internal/memorypolicy"
)

// AddObservationNew is the operator create-only surface. The trusted legacy
// AddObservation upsert remains for compatibility, but is not this CLI contract.
func (s *Store) AddObservationNew(p AddObservationParams) (int64, error) {
	for _, value := range []string{p.Type, p.Project, p.Scope, p.Title, p.Content, p.Abstract, p.Overview, p.MemoryPath, p.TopicKey} {
		if !utf8.ValidString(value) || memorypolicy.ContainsSecret(value) || memorypolicy.ContainsTranscript(value) {
			return 0, errors.New("new knowledge must be valid UTF-8 without secrets or raw transcript")
		}
	}
	if strings.TrimSpace(stripPrivateTags(p.Title)) == "" || strings.TrimSpace(stripPrivateTags(p.Content)) == "" {
		return 0, errors.New("nonempty durable title and content required")
	}
	if len(p.Content) > s.cfg.MaxObservationLength || len(p.Overview) > s.cfg.MaxObservationLength {
		return 0, errors.New("new knowledge exceeds observation length limit")
	}
	if strings.TrimSpace(p.Type) == "" || len(p.Type) > 80 || strings.ContainsFunc(p.Type, unicode.IsControl) || len(p.Project) > 200 || strings.ContainsFunc(p.Project, unicode.IsControl) {
		return 0, errors.New("bounded single-line type and project required")
	}
	if scope := strings.TrimSpace(strings.ToLower(p.Scope)); scope != "" && scope != "project" && scope != "personal" && scope != "global" {
		return 0, errors.New("scope must be project, personal or global")
	}
	if p.MemoryClass != "" {
		value, err := normalizeMemoryClass(p.MemoryClass)
		if err != nil {
			return 0, err
		}
		p.MemoryClass = value
	}
	mutation, err := s.prepareObservationMutation(p, true)
	if err != nil {
		return 0, err
	}
	p.Project, _ = NormalizeProject(p.Project)
	scope := normalizeScope(p.Scope)
	topic := normalizeTopicKey(p.TopicKey)
	var id int64
	err = s.withTx(func(tx *sql.Tx) error {
		if _, err := s.execHook(tx, "UPDATE sessions SET id=id WHERE id=?", p.SessionID); err != nil {
			return err
		}
		var existingID int64
		err := tx.QueryRow(`SELECT id FROM observations WHERE title=? AND content=? AND type=? AND ifnull(project,'')=? AND scope=? AND ifnull(topic_key,'')=? AND deleted_at IS NULL ORDER BY id LIMIT 1`, stripPrivateTags(p.Title), stripPrivateTags(p.Content), p.Type, p.Project, scope, topic).Scan(&existingID)
		if err == nil {
			existing, err := s.getObservationTx(tx, existingID)
			if err != nil {
				return err
			}
			if (p.MemoryClass != "" && p.MemoryClass != existing.MemoryClass) || (p.MemoryPath != "" && p.MemoryPath != existing.MemoryPath) || (p.Abstract != "" && stripPrivateTags(p.Abstract) != existing.Abstract) || (p.Overview != "" && stripPrivateTags(p.Overview) != existing.Overview) {
				return fmt.Errorf("memory already exists with different metadata; use snapshot-checked memory update")
			}
			if existing.ArchivedAt != nil {
				return fmt.Errorf("memory already exists archived; use an explicit snapshot-checked reactivate")
			}
			id = existingID
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		id, err = mutation(tx)
		return err
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}
