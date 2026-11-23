package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"recalldock.local/recalldock/internal/memorypolicy"
)

var ErrObservationSnapshotChanged = errors.New("observation snapshot changed; reread the selected current state before editing")

type ObservationSelection struct {
	ID             int64                     `json:"id"`
	StoreID        string                    `json:"store_id"`
	SyncID         string                    `json:"sync_id"`
	RevisionCount  int                       `json:"revision_count"`
	LifecycleState string                    `json:"lifecycle_state"`
	SnapshotSHA256 string                    `json:"snapshot_sha256"`
	State          *ObservationStateSnapshot `json:"state,omitempty"`
}

func snapshotFromObservation(obs *Observation) (ObservationStateSnapshot, error) {
	var state ObservationStateSnapshot
	for _, field := range []string{obs.SyncID, obs.SessionID, obs.Type, obs.MemoryClass, obs.MemoryPath, obs.Abstract, obs.Overview, obs.Title, obs.Content, derefString(obs.ToolName), derefString(obs.Project), obs.Scope, derefString(obs.TopicKey), derefString(obs.StaleReason)} {
		if !utf8.ValidString(field) {
			return state, errors.New("stored knowledge has invalid UTF-8; inspect an isolated copy before repair")
		}
	}
	encoded, err := json.Marshal(obs)
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(encoded, &state); err != nil {
		return state, err
	}
	state.Pinned = obs.Pinned
	return state, nil
}
func selectionTx(tx *sql.Tx, obs *Observation) (ObservationSelection, error) {
	var selection ObservationSelection
	state, err := snapshotFromObservation(obs)
	if err != nil {
		return selection, err
	}
	if err := tx.QueryRow("SELECT value FROM store_metadata WHERE key='store_id'").Scan(&selection.StoreID); err != nil {
		return selection, err
	}
	selection.ID = obs.ID
	selection.SyncID = obs.SyncID
	selection.RevisionCount = obs.RevisionCount
	selection.LifecycleState = obs.State()
	selection.State = &state
	encoded, err := json.Marshal(struct {
		StoreID string                   `json:"store_id"`
		ID      int64                    `json:"id"`
		State   ObservationStateSnapshot `json:"state"`
	}{selection.StoreID, obs.ID, state})
	if err != nil {
		return selection, err
	}
	sum := sha256.Sum256(encoded)
	selection.SnapshotSHA256 = hex.EncodeToString(sum[:])
	return selection, nil
}

// SelectObservationForEdit is explicit selected state, not a broad memory load.
func (s *Store) SelectObservationForEdit(id int64) (ObservationSelection, error) {
	var selected ObservationSelection
	tx, err := s.db.Begin()
	if err != nil {
		return selected, err
	}
	defer tx.Rollback()
	obs, err := s.getObservationTx(tx, id)
	if err != nil {
		return selected, err
	}
	selected, err = selectionTx(tx, obs)
	if err != nil {
		return selected, err
	}
	if err := tx.Commit(); err != nil {
		return selected, err
	}
	return selected, nil
}

// ObservationSnapshotMetadata describes exactly the already-selected state.
// Concurrent revisions cannot pair a new digest with an older rendered body.
// This digest is a comparison value, never an approval or identity credential.
func (s *Store) ObservationSnapshotMetadata(obs *Observation) (ObservationSelection, error) {
	if obs == nil {
		return ObservationSelection{}, errors.New("selected observation required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return ObservationSelection{}, err
	}
	defer tx.Rollback()
	selected, err := selectionTx(tx, obs)
	selected.State = nil
	return selected, err
}

func (s *Store) normalizeCASPatch(obs *Observation, p UpdateObservationParams) (UpdateObservationParams, bool, error) {
	changed := false
	fields := []struct {
		value   **string
		current string
		max     int
	}{
		{&p.Type, obs.Type, 80}, {&p.Title, obs.Title, 512}, {&p.Content, obs.Content, s.cfg.MaxObservationLength},
		{&p.Project, derefString(obs.Project), 200}, {&p.Scope, obs.Scope, 20}, {&p.TopicKey, derefString(obs.TopicKey), 512},
		{&p.MemoryClass, obs.MemoryClass, 80}, {&p.MemoryPath, obs.MemoryPath, 512},
		{&p.Abstract, obs.Abstract, 2000}, {&p.Overview, obs.Overview, s.cfg.MaxObservationLength},
	}
	provided := p.Archived != nil
	if p.Archived != nil {
		for _, field := range fields {
			if *field.value != nil {
				return p, false, errors.New("archive/reactivate must be a separate reviewed lifecycle change")
			}
		}
		if *p.Archived && (obs.Pinned || isProtectedMemoryClass(obs.MemoryClass)) {
			return p, false, errors.New("protected memory cannot be archived")
		}
		changed = *p.Archived != (obs.ArchivedAt != nil)
	}
	for _, field := range fields {
		if *field.value == nil {
			continue
		}
		provided = true
		value := **field.value
		if !utf8.ValidString(value) || len(value) > field.max || memorypolicy.ContainsSecret(value) || memorypolicy.ContainsTranscript(value) {
			return p, false, fmt.Errorf("edit patch must be bounded UTF-8 knowledge, without secrets or raw transcript")
		}
	}
	if !provided {
		return p, false, errors.New("at least one edit field is required")
	}
	if p.Project != nil {
		normalized, _ := NormalizeProject(*p.Project)
		if normalized != derefString(obs.Project) {
			return p, false, errors.New("CAS edit cannot move a memory across projects; use explicit reviewed project migration")
		}
		p.Project = &normalized
	}
	if p.Scope != nil {
		value := strings.TrimSpace(*p.Scope)
		if value != "project" && value != "personal" && value != "global" {
			return p, false, errors.New("scope must be project, personal or global")
		}
		p.Scope = &value
	}
	if p.TopicKey != nil {
		value := normalizeTopicKey(*p.TopicKey)
		p.TopicKey = &value
	}
	if p.MemoryClass != nil {
		value, err := normalizeMemoryClass(*p.MemoryClass)
		if err != nil {
			return p, false, err
		}
		p.MemoryClass = &value
	}
	contentChanged := (p.Content != nil && *p.Content != obs.Content) || (p.Title != nil && *p.Title != obs.Title) || (p.Type != nil && *p.Type != obs.Type)
	if (p.Abstract != nil || p.Overview != nil) && (contentChanged || obs.SummaryState == SummaryStateStale) {
		if (obs.Abstract != "" && p.Abstract == nil) || (obs.Overview != "" && p.Overview == nil) {
			return p, false, errors.New("review all existing summary fields together, or omit both to keep derived summaries stale")
		}
	}
	// Rebuild the pointer list after normalizing fields; read/hit metadata is not
	// part of the patch or selected digest and cannot make retries create versions.
	comparisons := []struct {
		value   *string
		current string
	}{{p.Type, obs.Type}, {p.Title, obs.Title}, {p.Content, obs.Content}, {p.Project, derefString(obs.Project)}, {p.Scope, obs.Scope}, {p.TopicKey, derefString(obs.TopicKey)}, {p.MemoryClass, obs.MemoryClass}, {p.MemoryPath, obs.MemoryPath}, {p.Abstract, obs.Abstract}, {p.Overview, obs.Overview}}
	for _, field := range comparisons {
		if field.value != nil && *field.value != field.current {
			changed = true
		}
	}
	if (p.Abstract != nil || p.Overview != nil) && (obs.SummaryState != SummaryStateFresh || obs.SummaryRevision != obs.RevisionCount) {
		changed = true
	}
	return p, changed, nil
}

// UpdateObservationCAS is a local operator surface, never a model approval flag.
// Directory validation is point-in-time; SQLite state comparison and all mutation
// side effects/history/audit are one transaction. Filesystem is not a DB transaction.
func (s *Store) UpdateObservationCAS(id int64, expected string, p UpdateObservationParams, reason, directory string) (ObservationSelection, error) {
	var result ObservationSelection
	if decoded, err := hex.DecodeString(expected); err != nil || len(decoded) != 32 {
		return result, errors.New("expected snapshot SHA256 required")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 512 || !utf8.ValidString(reason) || memorypolicy.ContainsSecret(reason) || memorypolicy.ContainsTranscript(reason) {
		return result, errors.New("bounded non-secret operator edit reason required")
	}
	before, err := s.GetObservation(id)
	if err != nil {
		return result, err
	}
	if err := s.validateRegisteredWrite(derefString(before.Project), directory, SessionOriginOperator); err != nil {
		return result, err
	}
	err = s.withTx(func(tx *sql.Tx) error {
		// Reserve the writer before the read to avoid a stale DEFERRED read snapshot
		// racing another process. No meaningful fields change, so no history is added.
		locked, err := s.execHook(tx, "UPDATE observations SET id=id WHERE id=? AND deleted_at IS NULL", id)
		if err != nil {
			return err
		}
		n, err := locked.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrObservationNotFound
		}
		current, err := s.getObservationTx(tx, id)
		if err != nil {
			return err
		}
		selected, err := selectionTx(tx, current)
		if err != nil {
			return err
		}
		if selected.SnapshotSHA256 != expected {
			return ErrObservationSnapshotChanged
		}
		patch, changed, err := s.normalizeCASPatch(current, p)
		if err != nil {
			return err
		}
		if !changed {
			result = selected
			return nil
		}
		updated, err := s.updateObservationTx(tx, id, patch)
		if err != nil {
			return err
		}
		if updated.RevisionCount == current.RevisionCount {
			result = selected
			return nil
		}
		if _, err := s.execHook(tx, `INSERT INTO observation_edit_audits(observation_id,sequence,expected_snapshot_sha256,operator_reason) SELECT ?,max(sequence),?,? FROM observation_versions WHERE observation_id=?`, id, expected, reason, id); err != nil {
			return err
		}
		result, err = selectionTx(tx, updated)
		return err
	})
	if err != nil {
		return ObservationSelection{}, err
	}
	return result, nil
}
