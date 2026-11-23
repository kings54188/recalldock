package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var ErrImportIdentityConflict = errors.New("import identity conflict")

// Legacy JSON may lack sync IDs. A deterministic, domain-separated fallback
// makes an identical record replayable without pretending that later edited
// legacy records have a provable shared identity.
func importSyncID(existing, prefix string, payload any) (string, error) {
	if strings.TrimSpace(existing) != "" {
		return existing, nil
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode legacy import identity: %w", err)
	}
	digest := sha256.Sum256(append([]byte(prefix+"\x00legacy-json-import-v1\x00"), data...))
	return prefix + "-" + hex.EncodeToString(digest[:16]), nil
}

func equalImportNullable(a, b *string) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func sameImportedObservation(a, b Observation) bool {
	return a.SessionID == b.SessionID && a.Type == b.Type &&
		a.MemoryClass == b.MemoryClass && a.MemoryPath == b.MemoryPath &&
		a.Abstract == b.Abstract && a.Overview == b.Overview &&
		a.SummaryState == b.SummaryState && a.SummaryRevision == b.SummaryRevision &&
		a.Title == b.Title && a.Content == b.Content &&
		derefString(a.Project) == derefString(b.Project) && a.Scope == b.Scope &&
		derefString(a.TopicKey) == derefString(b.TopicKey) &&
		derefString(a.ToolName) == derefString(b.ToolName) &&
		a.RevisionCount == b.RevisionCount && a.CreatedAt == b.CreatedAt &&
		equalImportNullable(a.DeletedAt, b.DeletedAt) &&
		equalImportNullable(a.ArchivedAt, b.ArchivedAt) &&
		equalImportNullable(a.StaleAt, b.StaleAt) &&
		equalImportNullable(a.StaleReason, b.StaleReason) &&
		equalImportNullable(a.ReviewAfter, b.ReviewAfter)
}

func (s *Store) importedObservationExistsTx(tx *sql.Tx, incoming Observation) (bool, error) {
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM observations WHERE sync_id = ?`, incoming.SyncID).Scan(&count); err != nil {
		return false, err
	}
	if count == 0 {
		return false, nil
	}
	if count != 1 {
		return false, fmt.Errorf("%w: observation identity is already ambiguous", ErrImportIdentityConflict)
	}
	existing, err := s.getObservationBySyncIDTx(tx, incoming.SyncID, true)
	if err != nil {
		return false, err
	}
	if !sameImportedObservation(*existing, incoming) {
		return false, fmt.Errorf("%w: observation differs; compare revisions before importing", ErrImportIdentityConflict)
	}
	return true, nil
}

func importedPromptExistsTx(tx *sql.Tx, incoming Prompt) (bool, error) {
	var tombstones int
	if err := tx.QueryRow(`SELECT count(*) FROM prompt_tombstones WHERE sync_id=?`, incoming.SyncID).Scan(&tombstones); err != nil {
		return false, err
	}
	if tombstones != 0 {
		return true, nil
	}
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM user_prompts WHERE sync_id = ?`, incoming.SyncID).Scan(&count); err != nil {
		return false, err
	}
	if count == 0 {
		return false, nil
	}
	if count != 1 {
		return false, fmt.Errorf("%w: prompt identity is already ambiguous", ErrImportIdentityConflict)
	}
	var session, content, project, created string
	if err := tx.QueryRow(`SELECT session_id, content, ifnull(project, ''), created_at FROM user_prompts WHERE sync_id = ?`, incoming.SyncID).Scan(&session, &content, &project, &created); err != nil {
		return false, err
	}
	if session != incoming.SessionID || content != incoming.Content || project != incoming.Project || created != incoming.CreatedAt {
		return false, fmt.Errorf("%w: prompt differs", ErrImportIdentityConflict)
	}
	return true, nil
}
