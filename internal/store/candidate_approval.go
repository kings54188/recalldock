package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"recalldock.local/recalldock/internal/memorypolicy"
	"strings"
	"time"
)

var ErrCandidateChanged = errors.New("candidate changed; reread the selected snapshot")

var ErrCandidateNotFound = errors.New("candidate not found")

// The digest binds operator approval to all proposed content, scope, evidence
// claims and expiry, but not mutable review status. It grants no authority by itself.
func MemoryCandidateDigest(c *MemoryCandidate) (string, error) {
	value := struct {
		ID, Session, Project, Category, Statement string
		Evidence                                  []CandidateEvidenceRef
		Confidence                                float64
		Path, Topic, Created, Expires             string
	}{c.SyncID, c.SessionID, c.Project, c.Category, c.Statement, c.EvidenceRefs, c.Confidence, c.MemoryPath, c.ProposedTopicKey, c.CreatedAt, c.ExpiresAt}
	body, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(body)), nil
}

const candidateSelect = `SELECT c.sync_id,c.session_id,c.project,c.category,c.statement,c.evidence_refs,c.confidence,ifnull(c.memory_path,''),ifnull(c.proposed_topic_key,''),c.status,ifnull(c.rejection_reason,''),ifnull(c.promoted_observation_sync_id,''),c.created_at,c.expires_at,c.updated_at,ifnull(s.origin,'unknown') FROM memory_candidates c LEFT JOIN sessions s ON s.id=c.session_id WHERE c.sync_id=?`

// ApproveMemoryCandidate is a local operator trust boundary, not an agent input.
// Candidate state, observation/FTS/nodes, sync journal and audit commit together.
// It never silently upserts an existing topic: that requires revision-aware review.
func (s *Store) ApproveMemoryCandidate(id, expectedDigest, reason string) (*Observation, error) {
	return s.approveMemoryCandidate(id, expectedDigest, reason, "")
}

func (s *Store) approveMemoryCandidate(id, expectedDigest, reason, automaticMode string) (*Observation, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 512 || containsCandidateSecret(reason) || memorypolicy.ContainsTranscript(reason) {
		return nil, errors.New("bounded non-secret operator review reason required")
	}
	candidate, err := s.GetMemoryCandidate(id)
	if err != nil {
		return nil, err
	}
	digest, err := MemoryCandidateDigest(candidate)
	if err != nil {
		return nil, err
	}
	if digest != expectedDigest {
		return nil, ErrCandidateChanged
	}
	class, typ, scope := candidateObservationShape(candidate.Category)
	var mutation func(*sql.Tx) (int64, error)
	if candidate.Status == CandidateStatusReview {
		mutation, err = s.prepareHistoricalObservationMutation(AddObservationParams{SessionID: candidate.SessionID, Project: candidate.Project, Type: typ, Scope: scope, Title: candidateTitle(candidate.Statement), Content: candidate.Statement, TopicKey: candidate.ProposedTopicKey, MemoryClass: class, MemoryPath: candidate.MemoryPath, Abstract: candidateTitle(candidate.Statement), Overview: candidate.Statement + "\n" + candidateEvidenceOverview(candidate.EvidenceRefs)}, true)
		if err != nil {
			return nil, err
		}
	}

	var result *Observation
	err = s.withTx(func(tx *sql.Tx) error {
		// Acquire a write lock before reading mutable review state, across processes.
		if _, err := tx.Exec(`UPDATE memory_candidates SET updated_at=updated_at WHERE sync_id=?`, id); err != nil {
			return err
		}
		current, err := scanMemoryCandidate(tx.QueryRow(candidateSelect, id))
		if err != nil {
			return err
		}
		currentDigest, err := MemoryCandidateDigest(current)
		if err != nil {
			return err
		}
		if currentDigest != expectedDigest {
			return ErrCandidateChanged
		}
		if current.Status == CandidateStatusPromoted && current.PromotedObservationSyncID != "" {
			result, err = s.getObservationBySyncIDTx(tx, current.PromotedObservationSyncID, false)
			return err
		}
		if current.Status != CandidateStatusReview {
			return errors.New("candidate is not pending review")
		}
		// TTL is a review SLA, not a destruction deadline. Overdue candidates
		// remain reviewable and auto-admittable under the operator policy.
		if automaticMode != "" && s.candidateAdmissionMode() != automaticMode {
			return errors.New("automatic admission policy changed; keep pending")
		}
		var evidence *verifiedCandidateEvidence
		if automaticMode == "checked" || automaticMode == "tiered" {
			evidence, err = s.verifyCandidateEvidenceTx(tx, current)
			if err != nil {
				return err
			}
		}
		var existingID int64
		var available int
		duplicateErr := tx.QueryRow(`SELECT id, CASE WHEN archived_at IS NULL AND (stale_at IS NULL OR trim(stale_at)='') AND summary_state != 'stale' AND `+validObservationSourcesSQL("observations")+` THEN 1 ELSE 0 END AS available FROM observations WHERE content=? AND ifnull(project,'')=? AND scope=? AND deleted_at IS NULL ORDER BY available DESC, id LIMIT 1`, current.Statement, current.Project, scope).Scan(&existingID, &available)
		if duplicateErr != nil && !errors.Is(duplicateErr, sql.ErrNoRows) {
			return duplicateErr
		}
		if duplicateErr == nil && available == 0 {
			return errors.New("matching knowledge is archived, stale or due for review; resolve it explicitly before approving this candidate")
		}
		if automaticMode == "duplicates" && duplicateErr != nil {
			return errors.New("automatic duplicates policy requires a current exact existing fact")
		}
		if topic := normalizeTopicKey(current.ProposedTopicKey); duplicateErr != nil && topic != "" {
			var count int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM observations WHERE topic_key=? AND ifnull(project,'')=? AND scope=? AND deleted_at IS NULL`, topic, current.Project, scope).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return errors.New("topic already exists; explicit revision-aware update required")
			}
		}
		observationID := existingID
		if duplicateErr != nil {
			if mutation == nil {
				return ErrCandidateChanged
			}
			observationID, err = mutation(tx)
			if err != nil {
				return err
			}
		}
		result, err = s.getObservationTx(tx, observationID)
		if err != nil {
			return err
		}
		if automaticMode == "checked" || automaticMode == "tiered" {
			if err := s.linkCandidateEvidenceTx(tx, evidence, result); err != nil {
				return err
			}
			valid, err := observationSourcesValidTx(tx, result.SyncID)
			if err != nil {
				return err
			}
			if !valid {
				return ErrCandidateEvidenceUnverified
			}
			if err := s.auditCandidateEvidenceTx(tx, current, evidence); err != nil {
				return err
			}
		}
		if _, err := s.execHook(tx, `UPDATE memory_candidates SET status='promoted',rejection_reason=NULL,promoted_observation_sync_id=?,updated_at=? WHERE sync_id=? AND status='review'`, result.SyncID, candidateTime(time.Now().UTC()), id); err != nil {
			return err
		}
		return insertCandidateAuditTx(s, tx, id, current.SessionID, current.Project, current.Category, "promoted", reason, time.Now().UTC())
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) RejectMemoryCandidateChecked(id, expected, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 512 || containsCandidateSecret(reason) || memorypolicy.ContainsTranscript(reason) {
		return errors.New("bounded non-secret operator review reason required")
	}
	return s.withTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE memory_candidates SET updated_at=updated_at WHERE sync_id=?`, id); err != nil {
			return err
		}
		candidate, err := scanMemoryCandidate(tx.QueryRow(candidateSelect, id))
		if err != nil {
			return err
		}
		digest, err := MemoryCandidateDigest(candidate)
		if err != nil {
			return err
		}
		if digest != expected {
			return ErrCandidateChanged
		}
		if candidate.Status != CandidateStatusReview {
			return errors.New("candidate is not pending review")
		}
		if _, err := s.execHook(tx, `UPDATE memory_candidates SET status='rejected',statement='',evidence_refs='[]',memory_path=NULL,proposed_topic_key=NULL,normalized_hash=NULL,rejection_reason=?,updated_at=? WHERE sync_id=? AND status='review'`, reason, candidateTime(time.Now().UTC()), id); err != nil {
			return err
		}
		return insertCandidateAuditTx(s, tx, id, candidate.SessionID, candidate.Project, candidate.Category, "rejected", reason, time.Now().UTC())
	})
}
