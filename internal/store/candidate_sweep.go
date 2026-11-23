package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CandidateSweepResult reports a bounded pass of the operator's configured admission policy.
type CandidateSweepResult struct {
	Scanned     int      `json:"scanned"`
	Promoted    int      `json:"promoted"`
	Remaining   int      `json:"remaining"`
	Failed      int      `json:"failed"`
	Skipped     int      `json:"skipped"`
	Failures    []string `json:"failures,omitempty"`
	PromotedIDs []string `json:"promoted_ids,omitempty"`
}

// AdmitPendingMemoryCandidates applies the configured policy to existing
// review entries. It exists so high-confidence knowledge is not left waiting
// only because it arrived before the current policy or before a review batch.
func (s *Store) AdmitPendingMemoryCandidates() (CandidateSweepResult, error) {
	return s.admitPendingMemoryCandidates("")
}

func (s *Store) admitPendingMemoryCandidates(project string) (CandidateSweepResult, error) {
	result := CandidateSweepResult{}
	mode := s.candidateAdmissionMode()
	if mode == "manual" {
		return result, nil
	}
	candidates, err := s.ListMemoryCandidates(project, CandidateStatusReview, 200)
	if err != nil {
		return result, err
	}
	result.Scanned = len(candidates)
	for i := range candidates {
		c := &candidates[i]
		if !candidateEligibleForAutoAdmission(c, mode) {
			result.Skipped++
			continue
		}
		digest, err := MemoryCandidateDigest(c)
		if err != nil {
			result.Failed++
			continue
		}
		if _, err := s.approveMemoryCandidate(c.SyncID, digest, "Automatic operator policy: "+mode, mode); err != nil {
			result.Failed++
			if len(result.Failures) < 8 {
				result.Failures = append(result.Failures, c.SyncID+": "+err.Error())
			}
			continue
		}
		result.Promoted++
		result.PromotedIDs = append(result.PromotedIDs, c.SyncID)
	}
	result.Remaining = result.Scanned - result.Promoted
	return result, nil
}

func candidateEligibleForAutoAdmission(c *MemoryCandidate, mode string) bool {
	if c.Status != CandidateStatusReview || strings.TrimSpace(c.Statement) == "" {
		return false
	}
	switch mode {
	case "duplicates":
		return true
	case "checked", "tiered":
		if len(c.EvidenceRefs) == 0 {
			return false
		}
		if mode == "tiered" && !tieredAutoAdmit(c) {
			return false
		}
		return true
	default:
		return false
	}
}

// ReviveMemoryCandidateChecked makes an expired or overdue review entry current
// again. The proposal body is retained across expiry, so revival is a TTL
// decision rather than a new model submission.
func (s *Store) ReviveMemoryCandidateChecked(id, expected, reason string) (*MemoryCandidate, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 512 || containsCandidateSecret(reason) || strings.ContainsAny(reason, "\r\n") {
		return nil, errors.New("bounded non-secret revive reason required")
	}
	var revived *MemoryCandidate
	err := s.withTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE memory_candidates SET updated_at=updated_at WHERE sync_id=?`, id); err != nil {
			return err
		}
		current, err := scanMemoryCandidate(tx.QueryRow(candidateSelect, id))
		if err != nil {
			return err
		}
		digest, err := MemoryCandidateDigest(current)
		if err != nil {
			return err
		}
		if digest != expected {
			return ErrCandidateChanged
		}
		if strings.TrimSpace(current.Statement) == "" {
			return errors.New("candidate body was already discarded; it cannot be revived")
		}
		if current.Status != CandidateStatusExpired && current.Status != CandidateStatusReview {
			return fmt.Errorf("candidate status %q cannot be revived", current.Status)
		}
		now := time.Now().UTC()
		if _, err := tx.Exec(`UPDATE memory_candidates SET status='review', rejection_reason=NULL, expires_at=?, updated_at=? WHERE sync_id=? AND status IN ('review','expired')`,
			candidateTime(now.Add(defaultCandidateTTL)), candidateTime(now), id); err != nil {
			return err
		}
		if err := insertCandidateAuditTx(s, tx, id, current.SessionID, current.Project, current.Category, "revived", reason, now); err != nil {
			return err
		}
		revived, err = scanMemoryCandidate(tx.QueryRow(candidateSelect, id))
		return err
	})
	return revived, err
}

const candidateLineEndingMarker = "migration.candidate_line_endings.v2"

// repairCandidateLineEndings deletes legacy carriage returns. A CR is encoding
// damage here, not a line break; replacing it with a newline broke version
// strings such as (0.1.7).
func (s *Store) repairCandidateLineEndings() error {
	tx, err := s.beginTxHook()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var marker string
	err = tx.QueryRow(`SELECT value FROM store_metadata WHERE key=?`, candidateLineEndingMarker).Scan(&marker)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	rows, err := tx.Query(`SELECT sync_id, category, statement FROM memory_candidates WHERE instr(statement, char(13)) > 0`)
	if err != nil {
		return err
	}
	type candidateRow struct{ id, category, statement string }
	var fixes []candidateRow
	for rows.Next() {
		var row candidateRow
		if err := rows.Scan(&row.id, &row.category, &row.statement); err != nil {
			_ = rows.Close()
			return err
		}
		fixes = append(fixes, row)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, row := range fixes {
		statement := normalizeCandidateStatement(row.statement)
		if statement == row.statement {
			continue
		}
		if _, err := tx.Exec(`UPDATE memory_candidates SET statement=?, normalized_hash=? WHERE sync_id=?`,
			statement, hashNormalized(row.category+"\n"+statement), row.id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO store_metadata(key,value) VALUES(?,'done')`, candidateLineEndingMarker); err != nil {
		return err
	}
	return s.commitHook(tx)
}
