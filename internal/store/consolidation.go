package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"recalldock.local/recalldock/internal/memorypolicy"
)

// ConsolidationError reports sources that could not be absorbed without hiding
// the ones that succeeded; the operator can retry the reported IDs.
type ConsolidationError struct {
	Failed map[int64]string
}

func (e *ConsolidationError) Error() string {
	parts := make([]string, 0, len(e.Failed))
	for id, reason := range e.Failed {
		parts = append(parts, fmt.Sprintf("#%d: %s", id, reason))
	}
	return "consolidation partially applied: " + strings.Join(parts, "; ")
}

// ConsolidateObservations archives source observations and records their
// lineage to the target. The target must already hold the refined text: this
// command never rewrites knowledge, so a merge cannot silently lose detail.
// Sources stay readable by ID; archiving removes them from recall, search and
// the review/forgetting surface.
func (s *Store) ConsolidateObservations(targetID int64, sourceIDs []int64, reason, directory string) ([]int64, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 512 || !utf8.ValidString(reason) || memorypolicy.ContainsSecret(reason) || memorypolicy.ContainsTranscript(reason) {
		return nil, errors.New("bounded consolidation reason required")
	}
	target, err := s.GetObservation(targetID)
	if err != nil {
		return nil, err
	}
	if target.ArchivedAt != nil {
		return nil, errors.New("target is archived; reactivate or choose an active memory")
	}
	targetSnapshot, err := s.ObservationSnapshotMetadata(target)
	if err != nil {
		return nil, err
	}
	absorbed := make([]int64, 0, len(sourceIDs))
	failed := map[int64]string{}
	seen := map[int64]bool{}
	for _, sourceID := range sourceIDs {
		if sourceID == targetID || seen[sourceID] {
			continue
		}
		seen[sourceID] = true
		source, err := s.GetObservation(sourceID)
		if err != nil {
			failed[sourceID] = "not found"
			continue
		}
		if err := s.validateRegisteredWrite(derefString(source.Project), directory, SessionOriginOperator); err != nil {
			failed[sourceID] = err.Error()
			continue
		}
		sourceSnapshot, err := s.ObservationSnapshotMetadata(source)
		if err != nil {
			failed[sourceID] = err.Error()
			continue
		}
		applied := false
		err = s.withTx(func(tx *sql.Tx) error {
			applied = false
			// Reserve SQLite's writer before reading either observation. The scope
			// checks, archival, audit and lineage then see one immutable snapshot.
			locked, err := s.execHook(tx, `UPDATE observations SET id=id WHERE id=? AND deleted_at IS NULL`, targetID)
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
			currentTarget, err := s.getObservationTx(tx, targetID)
			if err != nil {
				return err
			}
			if currentTarget.ArchivedAt != nil {
				return errors.New("target is archived; reactivate or choose an active memory")
			}
			currentSelection, err := selectionTx(tx, currentTarget)
			if err != nil {
				return err
			}
			if currentSelection.SnapshotSHA256 != targetSnapshot.SnapshotSHA256 {
				return ErrObservationSnapshotChanged
			}
			currentSource, err := s.getObservationTx(tx, sourceID)
			if err != nil {
				return err
			}
			// Binding was validated against the prior project outside SQLite.
			// Do not allow a project change to bypass that filesystem check.
			if derefString(currentSource.Project) != derefString(source.Project) {
				return ErrObservationSnapshotChanged
			}
			if derefString(currentSource.Project) != derefString(currentTarget.Project) || normalizeScope(currentSource.Scope) != normalizeScope(currentTarget.Scope) {
				return errors.New("source and target must share project and scope")
			}
			if currentSource.ArchivedAt != nil {
				var count int
				if err := tx.QueryRow(`SELECT COUNT(*) FROM memory_consolidations WHERE consolidated_sync_id=? AND source_sync_id=?`, currentTarget.SyncID, currentSource.SyncID).Scan(&count); err != nil {
					return err
				}
				if count > 0 {
					return nil
				}
				return errors.New("already archived")
			}
			selected, err := selectionTx(tx, currentSource)
			if err != nil {
				return err
			}
			if selected.SnapshotSHA256 != sourceSnapshot.SnapshotSHA256 {
				return ErrObservationSnapshotChanged
			}
			flag := true
			patch, changed, err := s.normalizeCASPatch(currentSource, UpdateObservationParams{Archived: &flag})
			if err != nil {
				return err
			}
			if !changed {
				return errors.New("source was not archived")
			}
			if _, err := s.updateObservationTx(tx, sourceID, patch); err != nil {
				return err
			}
			if _, err := s.execHook(tx, `INSERT INTO observation_edit_audits(observation_id,sequence,expected_snapshot_sha256,operator_reason) SELECT ?,max(sequence),?,? FROM observation_versions WHERE observation_id=?`, sourceID, selected.SnapshotSHA256, reason, sourceID); err != nil {
				return err
			}
			if _, err := s.execHook(tx, `INSERT OR IGNORE INTO memory_consolidations(consolidated_sync_id, source_sync_id, created_at) VALUES(?,?,?)`, currentTarget.SyncID, currentSource.SyncID, time.Now().UTC().Format("2006-01-02 15:04:05")); err != nil {
				return err
			}
			applied = true
			return nil
		})
		if err != nil {
			failed[sourceID] = err.Error()
			continue
		}
		if applied {
			absorbed = append(absorbed, sourceID)
		}
	}
	if len(failed) > 0 {
		return absorbed, &ConsolidationError{Failed: failed}
	}
	return absorbed, nil
}

// ConsolidatedSources lists the archived sources that were folded into target.
func (s *Store) ConsolidatedSources(targetID int64) ([]Observation, error) {
	target, err := s.GetObservation(targetID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT `+observationSelectColumns+` FROM observations WHERE ifnull(sync_id,'') IN
		(SELECT source_sync_id FROM memory_consolidations WHERE consolidated_sync_id=?)
		ORDER BY datetime(created_at) DESC, id DESC`, target.SyncID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Observation
	for rows.Next() {
		var obs Observation
		if err := scanObservationRow(rows, &obs); err != nil {
			return nil, err
		}
		if err != nil {
			return nil, err
		}
		out = append(out, obs)
	}
	return out, rows.Err()
}

// ensureConsolidationTable is idempotent; the table is lineage metadata only.
func (s *Store) ensureConsolidationTable() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS memory_consolidations(
		consolidated_sync_id TEXT NOT NULL,
		source_sync_id TEXT NOT NULL,
		created_at TEXT NOT NULL,
		PRIMARY KEY(consolidated_sync_id, source_sync_id)
	)`)
	return err
}
