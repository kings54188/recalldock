package store

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"recalldock.local/recalldock/internal/memorypolicy"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaxCandidatesPerCommit = 8

	CandidateCategoryUserPreference    = "user_preference"
	CandidateCategoryUserFact          = "user_fact"
	CandidateCategoryProjectConstraint = "project_constraint"
	CandidateCategoryDecision          = "decision"
	CandidateCategoryProcedure         = "procedure"
	CandidateCategoryToolBehavior      = "tool_behavior"
	CandidateCategoryFailureAvoidance  = "failure_avoidance"
	CandidateCategoryVerifiedOutcome   = "verified_outcome"

	CandidateStatusReview   = "review"
	CandidateStatusPromoted = "promoted"
	CandidateStatusRejected = "rejected"
	CandidateStatusExpired  = "expired"
	// CandidateStatusDuplicate is a result-entry label only: an identical
	// candidate already exists (same project+category+normalized hash) and no
	// new row was created. It is never persisted as a candidate status.
	CandidateStatusDuplicate = "duplicate"

	EvidenceKindTest           = "test"
	EvidenceKindCommand        = "command"
	EvidenceKindFile           = "file"
	EvidenceKindSource         = "source"
	EvidenceKindObservation    = "observation"
	EvidenceKindUserCorrection = "user_correction"
)

const (
	defaultCandidateTTL           = 7 * 24 * time.Hour
	defaultCandidateCapacity      = 256
	defaultCandidateAuditCapacity = 1024
	maxCandidateStatementBytes    = 2000
	maxEvidenceRefs               = 8
	maxEvidenceRefBytes           = 512
)

var (
	ErrSessionNotStarted     = errors.New("session_not_started: await successful session registration before retrying")
	knownCandidateCategories = map[string]struct{}{
		CandidateCategoryUserPreference: {}, CandidateCategoryUserFact: {},
		CandidateCategoryProjectConstraint: {}, CandidateCategoryDecision: {},
		CandidateCategoryProcedure: {}, CandidateCategoryToolBehavior: {},
		CandidateCategoryFailureAvoidance: {}, CandidateCategoryVerifiedOutcome: {},
	}
	knownEvidenceKinds = map[string]struct{}{
		EvidenceKindTest: {}, EvidenceKindCommand: {}, EvidenceKindFile: {},
		EvidenceKindSource: {}, EvidenceKindObservation: {}, EvidenceKindUserCorrection: {},
	}
	candidateRoutinePattern     = regexp.MustCompile(`(?i)^(?:tests? (?:passed|failed)|task (?:completed|finished)|completed task|ran (?:tests?|commands?)|已(?:完成|运行|执行|通过)|测试(?:通过|完成)|进度[:：])`)
	candidateSpeculationPattern = regexp.MustCompile(`(?i)^(?:maybe|probably|i think|i guess|possibly|可能|猜测|也许|推测)`)
	candidateLogPattern         = regexp.MustCompile(`(?m)^(?:\$ |Traceback|\[?(?:DEBUG|INFO|ERROR)\]?[: ])`)
)

type CandidateEvidenceRef struct {
	Kind       string `json:"kind"`
	Ref        string `json:"ref"`
	Digest     string `json:"digest,omitempty"`
	ObservedAt string `json:"observed_at,omitempty"`
}

type MemoryCandidateInput struct {
	Category         string                 `json:"category"`
	Statement        string                 `json:"statement"`
	EvidenceRefs     []CandidateEvidenceRef `json:"evidence_refs,omitempty"`
	Confidence       float64                `json:"confidence"`
	MemoryPath       string                 `json:"memory_path,omitempty"`
	ProposedTopicKey string                 `json:"proposed_topic_key,omitempty"`
}

type CandidateAdmissionPolicy struct {
	CapacityTotalPending    int
	Now                     time.Time
	TTL                     time.Duration
	CapacityPerProject      int
	AuditCapacityPerProject int
}

type CommitMemoryCandidatesParams struct {
	SessionID  string                   `json:"session_id"`
	Project    string                   `json:"project"`
	Candidates []MemoryCandidateInput   `json:"candidates"`
	Policy     CandidateAdmissionPolicy `json:"-"`
}

type MemoryCandidate struct {
	SyncID                    string                 `json:"sync_id"`
	SessionID                 string                 `json:"session_id"`
	SessionOrigin             string                 `json:"session_origin,omitempty"`
	Project                   string                 `json:"project"`
	Category                  string                 `json:"category"`
	Statement                 string                 `json:"statement,omitempty"`
	EvidenceRefs              []CandidateEvidenceRef `json:"evidence_refs,omitempty"`
	Confidence                float64                `json:"confidence"`
	MemoryPath                string                 `json:"memory_path,omitempty"`
	ProposedTopicKey          string                 `json:"proposed_topic_key,omitempty"`
	Status                    string                 `json:"status"`
	RejectionReason           string                 `json:"rejection_reason,omitempty"`
	PromotedObservationSyncID string                 `json:"promoted_observation_sync_id,omitempty"`
	CreatedAt                 string                 `json:"created_at"`
	ExpiresAt                 string                 `json:"expires_at"`
	UpdatedAt                 string                 `json:"updated_at"`
}

type CandidateAdmissionResult struct {
	CandidateID string `json:"candidate_id"`
	Status      string `json:"status"`
	Reason      string `json:"reason,omitempty"`
}

type CommitMemoryCandidatesResult struct {
	Promoted     int                        `json:"promoted"`
	Review       int                        `json:"review"`
	Rejected     int                        `json:"rejected"`
	Duplicate    int                        `json:"duplicate"`
	Swept        int                        `json:"swept,omitempty"`
	CandidateIDs []string                   `json:"candidate_ids"`
	Results      []CandidateAdmissionResult `json:"-"`
}

type preparedCandidate struct {
	input          MemoryCandidateInput
	category       string
	statement      string
	evidenceJSON   string
	normalizedHash string
	status         string
	reason         string
	redacted       bool
}

func normalizeCandidatePolicy(p CandidateAdmissionPolicy) CandidateAdmissionPolicy {
	if p.Now.IsZero() {
		p.Now = time.Now().UTC()
	} else {
		p.Now = p.Now.UTC()
	}
	if p.TTL <= 0 {
		p.TTL = defaultCandidateTTL
	}
	if p.CapacityPerProject <= 0 {
		p.CapacityPerProject = defaultCandidateCapacity
	}
	if p.CapacityTotalPending <= 0 || p.CapacityTotalPending > 2048 {
		p.CapacityTotalPending = 2048
	}
	if p.AuditCapacityPerProject <= 0 {
		p.AuditCapacityPerProject = defaultCandidateAuditCapacity
	}

	return p
}

func candidateTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05")
}

func containsCandidateSecret(value string) bool { return memorypolicy.ContainsSecret(value) }

// CandidateSafeForOutbox applies the no-secret/no-transcript boundary before a
// hook writes retry state. Full admission still happens inside the Store.
func CandidateSafeForOutbox(input MemoryCandidateInput) bool {
	prepared := prepareMemoryCandidate(input, normalizeCandidatePolicy(CandidateAdmissionPolicy{}))
	return !prepared.redacted && prepared.reason != "invalid_category" && prepared.reason != "invalid_statement"
}

func normalizeCandidateStatement(value string) string {
	// A carriage return in an imported statement is encoding damage, not a
	// line break. Delete it instead of turning (0.1.7) into (0 .1 .7).
	value = strings.ReplaceAll(value, "\r", "")
	return strings.TrimSpace(value)
}

func prepareMemoryCandidate(input MemoryCandidateInput, policy CandidateAdmissionPolicy) preparedCandidate {
	category := strings.ToLower(strings.TrimSpace(input.Category))
	statement := normalizeCandidateStatement(input.Statement)
	prepared := preparedCandidate{input: input, category: category, statement: statement, status: CandidateStatusReview}
	if _, ok := knownCandidateCategories[category]; !ok {
		prepared.category = "invalid"
		prepared.status, prepared.reason, prepared.redacted = CandidateStatusRejected, "invalid_category", true
		return prepared
	}
	if statement == "" {
		prepared.status, prepared.reason, prepared.redacted = CandidateStatusRejected, "invalid_statement", true
		return prepared
	}
	if len(statement) > maxCandidateStatementBytes {
		// Size-limit rejection is distinct from transcript detection: the statement
		// may be durable knowledge that simply exceeds maxCandidateStatementBytes.
		prepared.status, prepared.reason, prepared.redacted = CandidateStatusRejected, "statement_too_long", true
		return prepared
	}
	if memorypolicy.ContainsTranscript(statement) {
		prepared.status, prepared.reason, prepared.redacted = CandidateStatusRejected, "raw_transcript", true
		return prepared
	}
	if containsCandidateSecret(statement) {
		prepared.status, prepared.reason, prepared.redacted = CandidateStatusRejected, "secret_detected", true
		return prepared
	}
	for _, field := range []string{input.MemoryPath, input.ProposedTopicKey} {
		if len(field) > maxEvidenceRefBytes || containsCandidateSecret(field) || memorypolicy.ContainsTranscript(field) {
			prepared.status, prepared.reason, prepared.redacted = CandidateStatusRejected, "unsafe_metadata", true
			return prepared
		}
	}
	if candidateRoutinePattern.MatchString(statement) || candidateSpeculationPattern.MatchString(statement) || candidateLogPattern.MatchString(statement) {
		prepared.status, prepared.reason, prepared.redacted = CandidateStatusRejected, "non_durable_or_raw", true
		return prepared
	}
	if math.IsNaN(input.Confidence) || math.IsInf(input.Confidence, 0) || input.Confidence < 0 || input.Confidence > 1 {
		prepared.status, prepared.reason, prepared.redacted = CandidateStatusRejected, "invalid_confidence", true
		return prepared
	}
	if len(input.EvidenceRefs) > maxEvidenceRefs {
		prepared.status, prepared.reason, prepared.redacted = CandidateStatusRejected, "too_many_evidence_refs", true
		return prepared
	}

	refs := append([]CandidateEvidenceRef(nil), input.EvidenceRefs...)
	for i := range refs {
		refs[i].Kind = strings.ToLower(strings.TrimSpace(refs[i].Kind))
		refs[i].Ref = strings.TrimSpace(refs[i].Ref)
		refs[i].Digest = strings.ToLower(strings.TrimSpace(refs[i].Digest))
		refs[i].ObservedAt = strings.TrimSpace(refs[i].ObservedAt)
		if _, ok := knownEvidenceKinds[refs[i].Kind]; !ok || refs[i].Ref == "" || len(refs[i].Ref) > maxEvidenceRefBytes {
			prepared.status, prepared.reason, prepared.redacted = CandidateStatusRejected, "invalid_evidence", true
			return prepared
		}
		if containsCandidateSecret(refs[i].Ref) || containsCandidateSecret(refs[i].Digest) || memorypolicy.ContainsTranscript(refs[i].Ref) {
			prepared.status, prepared.reason, prepared.redacted = CandidateStatusRejected, "secret_or_transcript_evidence", true
			return prepared
		}
		if len(refs[i].Digest) > 64 || len(refs[i].ObservedAt) > 64 {
			prepared.status, prepared.reason, prepared.redacted = CandidateStatusRejected, "invalid_evidence_metadata", true
			return prepared
		}
		if refs[i].Digest != "" {
			if decoded, err := hex.DecodeString(refs[i].Digest); err != nil || len(decoded) != 32 {
				prepared.status, prepared.reason, prepared.redacted = CandidateStatusRejected, "invalid_evidence_digest", true
				return prepared
			}
		}
		if refs[i].ObservedAt != "" {
			if _, err := time.Parse(time.RFC3339, refs[i].ObservedAt); err != nil {
				prepared.status, prepared.reason, prepared.redacted = CandidateStatusRejected, "invalid_evidence_time", true
				return prepared
			}
		}

	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Kind != refs[j].Kind {
			return refs[i].Kind < refs[j].Kind
		}
		return refs[i].Ref < refs[j].Ref
	})
	encoded, _ := json.Marshal(refs)
	prepared.input.Category = category
	prepared.input.Statement = statement
	prepared.input.EvidenceRefs = refs
	prepared.evidenceJSON = string(encoded)
	prepared.normalizedHash = hashNormalized(category + "\n" + statement)
	// Model claims never authenticate themselves. Promotion is an operator action.
	if len(refs) == 0 {
		prepared.reason = "evidence_required"
	} else {
		prepared.reason = "operator_review_required"
	}

	return prepared
}

func (s *Store) CommitMemoryCandidates(p CommitMemoryCandidatesParams) (*CommitMemoryCandidatesResult, error) {
	if len(p.Candidates) == 0 {
		return nil, errors.New("at least one memory candidate is required")
	}
	if len(p.Candidates) > MaxCandidatesPerCommit {
		return nil, fmt.Errorf("memory candidate batch exceeds maximum of %d", MaxCandidatesPerCommit)
	}
	p.SessionID = strings.TrimSpace(p.SessionID)
	if p.SessionID == "" {
		return nil, errors.New("session_id is required")
	}
	session, err := s.GetSession(p.SessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSessionNotStarted
	}
	if err != nil {
		return nil, fmt.Errorf("resolve candidate session: %w", err)
	}
	project, err := s.CanonicalProjectKey(p.Project)
	if err != nil {
		return nil, err
	}
	if project == "" {
		project, _ = s.CanonicalProjectKey(session.Project)
	}
	sessionProject, _ := s.CanonicalProjectKey(session.Project)
	if project != sessionProject {
		return nil, fmt.Errorf("candidate project %q does not match session project %q", project, sessionProject)
	}
	policy := normalizeCandidatePolicy(p.Policy)
	prepared := make([]preparedCandidate, len(p.Candidates))
	for i := range p.Candidates {
		prepared[i] = prepareMemoryCandidate(p.Candidates[i], policy)

		if !prepared[i].redacted && strings.TrimSpace(prepared[i].input.MemoryPath) != "" {
			class, _, _ := candidateObservationShape(prepared[i].category)
			if _, pathErr := NormalizeMemoryPath(class, prepared[i].input.MemoryPath); pathErr != nil {
				prepared[i].status, prepared[i].reason, prepared[i].redacted = CandidateStatusRejected, "invalid_memory_path", true
			}
		}
	}

	result := &CommitMemoryCandidatesResult{CandidateIDs: make([]string, 0, len(prepared)), Results: make([]CandidateAdmissionResult, 0, len(prepared))}

	err = s.withTx(func(tx *sql.Tx) error {
		// Review candidates are never silently destroyed on a timer. TTL is a
		// review SLA: an overdue proposal stays readable, and automatic
		// admission may still promote it when the operator policy allows.
		for _, candidate := range prepared {
			candidateID := newSyncID("cand")
			if candidate.normalizedHash != "" {
				var existingID string
				err := tx.QueryRow(`
					SELECT sync_id FROM memory_candidates
					WHERE project = ? AND category = ? AND normalized_hash = ?
					  AND status IN ('review', 'promoted') AND datetime(expires_at) > datetime(?)
					ORDER BY datetime(created_at) DESC LIMIT 1
				`, project, candidate.category, candidate.normalizedHash, candidateTime(policy.Now)).Scan(&existingID)
				if err == nil {
					// Same candidate already exists; nothing was rejected or
					// created. Report it as an explicit duplicate outcome so
					// callers do not misread it as a policy rejection.
					result.Duplicate++
					result.CandidateIDs = append(result.CandidateIDs, existingID)
					result.Results = append(result.Results, CandidateAdmissionResult{CandidateID: existingID, Status: CandidateStatusDuplicate, Reason: "duplicate"})
					if err := insertCandidateAuditTx(s, tx, existingID, p.SessionID, project, candidate.category, "duplicate", "duplicate", policy.Now); err != nil {
						return err
					}
					continue
				}
				if err != sql.ErrNoRows {
					return err
				}
			}

			// Rejected rows contain no statement or evidence and are disposable
			// audit envelopes. Evict the oldest before they can starve review.
			if _, err := s.execHook(tx, `
				DELETE FROM memory_candidates WHERE sync_id IN (
					SELECT sync_id FROM memory_candidates
					WHERE project = ? AND status = 'rejected'
					ORDER BY datetime(created_at), sync_id LIMIT 1
				) AND (SELECT count(*) FROM memory_candidates WHERE project = ?) >= ?
			`, project, project, policy.CapacityPerProject); err != nil {
				return err
			}
			var activeCount int
			if err := tx.QueryRow(`SELECT count(*) FROM memory_candidates WHERE project = ? AND status != 'expired'`, project).Scan(&activeCount); err != nil {
				return err
			}
			var totalPending int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM memory_candidates WHERE status='review'`).Scan(&totalPending); err != nil {
				return err
			}
			if activeCount >= policy.CapacityPerProject || totalPending >= policy.CapacityTotalPending {
				result.Rejected++
				result.Results = append(result.Results, CandidateAdmissionResult{CandidateID: candidateID, Status: CandidateStatusRejected, Reason: "capacity_exceeded"})
				if err := insertCandidateAuditTx(s, tx, candidateID, p.SessionID, project, candidate.category, "rejected", "capacity_exceeded", policy.Now); err != nil {
					return err
				}
				continue
			}

			statement, evidenceJSON, normalizedHash := candidate.statement, candidate.evidenceJSON, candidate.normalizedHash
			confidence := candidate.input.Confidence
			memoryPath, topicKey := strings.TrimSpace(candidate.input.MemoryPath), normalizeTopicKey(candidate.input.ProposedTopicKey)
			if candidate.redacted {
				statement, evidenceJSON, normalizedHash, memoryPath, topicKey = "", "[]", "", "", ""
				confidence = 0
			}
			status := candidate.status
			if _, err := s.execHook(tx, `
				INSERT INTO memory_candidates (
					sync_id, session_id, project, category, statement, evidence_refs,
					confidence, memory_path, proposed_topic_key, normalized_hash, status,
					rejection_reason, created_at, expires_at, updated_at
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			`, candidateID, p.SessionID, project, candidate.category, statement, evidenceJSON,
				confidence, nullableString(memoryPath), nullableString(topicKey), nullableString(normalizedHash), status,
				nullableString(candidate.reason), candidateTime(policy.Now), candidateTime(policy.Now.Add(policy.TTL)), candidateTime(policy.Now)); err != nil {
				return err
			}
			result.CandidateIDs = append(result.CandidateIDs, candidateID)
			if status == CandidateStatusRejected {
				result.Rejected++
				result.Results = append(result.Results, CandidateAdmissionResult{CandidateID: candidateID, Status: status, Reason: candidate.reason})
				if err := insertCandidateAuditTx(s, tx, candidateID, p.SessionID, project, candidate.category, "rejected", candidate.reason, policy.Now); err != nil {
					return err
				}
				continue
			}
			result.Review++
			result.Results = append(result.Results, CandidateAdmissionResult{CandidateID: candidateID, Status: CandidateStatusReview, Reason: candidate.reason})
			if err := insertCandidateAuditTx(s, tx, candidateID, p.SessionID, project, candidate.category, "admitted", candidate.reason, policy.Now); err != nil {
				return err
			}
		}
		if _, err := s.execHook(tx, `DELETE FROM memory_candidates WHERE sync_id IN (SELECT sync_id FROM memory_candidates WHERE status IN ('rejected','expired') ORDER BY datetime(created_at) DESC,sync_id DESC LIMIT -1 OFFSET 2048)`); err != nil {
			return err
		}
		if _, err := s.execHook(tx, `DELETE FROM memory_candidate_audit WHERE id IN (SELECT id FROM memory_candidate_audit ORDER BY id DESC LIMIT -1 OFFSET 8192)`); err != nil {
			return err
		}
		return pruneCandidateAuditTx(s, tx, project, policy.AuditCapacityPerProject)
	})
	if err != nil {
		return nil, err
	}

	s.autoAdmitCandidates(result)
	if swept, sweepErr := s.admitPendingMemoryCandidates(project); sweepErr == nil {
		result.Swept = swept.Promoted
	}
	return result, nil
}

func candidateObservationShape(category string) (memoryClass, observationType, scope string) {
	switch category {
	case CandidateCategoryUserPreference:
		return MemoryClassUser, "preference", "personal"
	case CandidateCategoryUserFact:
		return MemoryClassUser, "user_fact", "personal"
	case CandidateCategoryProjectConstraint:
		return MemoryClassProject, "policy", "project"
	case CandidateCategoryDecision:
		return MemoryClassProject, "decision", "project"
	case CandidateCategoryProcedure:
		return MemoryClassAgent, "procedure", "project"
	case CandidateCategoryToolBehavior:
		return MemoryClassAgent, "pattern", "project"
	case CandidateCategoryFailureAvoidance:
		return MemoryClassAgent, "learning", "project"
	default:
		return MemoryClassAgent, "experience", "project"
	}
}

func candidateTitle(statement string) string {
	statement = strings.Join(strings.Fields(statement), " ")
	if utf8.RuneCountInString(statement) <= 120 {
		return statement
	}
	runes := []rune(statement)
	return string(runes[:117]) + "..."
}

func candidateEvidenceOverview(refs []CandidateEvidenceRef) string {
	if len(refs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(refs))
	for _, ref := range refs {
		part := ref.Kind + ":" + ref.Ref
		if ref.Digest != "" {
			part += "@sha256:" + ref.Digest
		}
		parts = append(parts, part)
	}
	return "Claimed evidence (not host-attested): " + strings.Join(parts, "; ")
}

// PromoteMemoryCandidate is a trusted in-process operator compatibility API.
// Agent transports cannot call it. New operator callers supply a reviewed digest.
func (s *Store) PromoteMemoryCandidate(id, reason string) (*Observation, error) {
	candidate, err := s.GetMemoryCandidate(id)
	if err != nil {
		return nil, err
	}
	digest, err := MemoryCandidateDigest(candidate)
	if err != nil {
		return nil, err
	}
	return s.ApproveMemoryCandidate(id, digest, reason)
}

func (s *Store) RejectMemoryCandidate(id, reason string) error {
	candidate, err := s.GetMemoryCandidate(id)
	if err != nil {
		return err
	}
	digest, err := MemoryCandidateDigest(candidate)
	if err != nil {
		return err
	}
	return s.RejectMemoryCandidateChecked(id, digest, reason)
}

func (s *Store) GetMemoryCandidate(syncID string) (*MemoryCandidate, error) {
	row := s.db.QueryRow(`
		SELECT c.sync_id, c.session_id, c.project, c.category, c.statement, c.evidence_refs, c.confidence,
		       ifnull(c.memory_path, ''), ifnull(c.proposed_topic_key, ''), c.status,
		       ifnull(c.rejection_reason, ''), ifnull(c.promoted_observation_sync_id, ''),
		       c.created_at, c.expires_at, c.updated_at, ifnull(s.origin, 'unknown')
		FROM memory_candidates c LEFT JOIN sessions s ON s.id = c.session_id WHERE c.sync_id = ?
	`, strings.TrimSpace(syncID))
	candidate, err := scanMemoryCandidate(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrCandidateNotFound
	}
	return candidate, err
}

func scanMemoryCandidate(row scannable) (*MemoryCandidate, error) {
	var candidate MemoryCandidate
	var evidenceJSON string
	if err := row.Scan(&candidate.SyncID, &candidate.SessionID, &candidate.Project, &candidate.Category,
		&candidate.Statement, &evidenceJSON, &candidate.Confidence, &candidate.MemoryPath,
		&candidate.ProposedTopicKey, &candidate.Status, &candidate.RejectionReason,
		&candidate.PromotedObservationSyncID, &candidate.CreatedAt, &candidate.ExpiresAt, &candidate.UpdatedAt,
		&candidate.SessionOrigin); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrCandidateNotFound
		}
		return nil, err
	}
	if err := json.Unmarshal([]byte(evidenceJSON), &candidate.EvidenceRefs); err != nil {
		return nil, fmt.Errorf("decode candidate evidence: %w", err)
	}
	return &candidate, nil
}

type CandidateReviewStats struct {
	PendingTotal    int
	DueWithin24h    int
	OverdueTotal    int
	OldestReviewDue string
}

func (s *Store) CandidateReviewStats(project string, now time.Time) (CandidateReviewStats, error) {
	project = s.canonicalProjectOrRaw(project)
	query := `SELECT count(*),
		ifnull(sum(CASE WHEN datetime(expires_at)>datetime(?) AND datetime(expires_at)<datetime(?) THEN 1 ELSE 0 END),0),
		ifnull(sum(CASE WHEN datetime(expires_at)<=datetime(?) THEN 1 ELSE 0 END),0),
		ifnull(min(expires_at),'') FROM memory_candidates WHERE status=?`
	args := []any{candidateTime(now), candidateTime(now.Add(24 * time.Hour)), candidateTime(now), CandidateStatusReview}
	if project != "" {
		query += " AND project=?"
		args = append(args, project)
	}
	var stats CandidateReviewStats
	err := s.db.QueryRow(query, args...).Scan(&stats.PendingTotal, &stats.DueWithin24h, &stats.OverdueTotal, &stats.OldestReviewDue)
	return stats, err
}

func (s *Store) ListMemoryCandidates(project, status string, limit int) ([]MemoryCandidate, error) {
	project = s.canonicalProjectOrRaw(project)
	status = strings.ToLower(strings.TrimSpace(status))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := `
		SELECT c.sync_id, c.session_id, c.project, c.category, c.statement, c.evidence_refs, c.confidence,
		       ifnull(c.memory_path, ''), ifnull(c.proposed_topic_key, ''), c.status,
		       ifnull(c.rejection_reason, ''), ifnull(c.promoted_observation_sync_id, ''),
		       c.created_at, c.expires_at, c.updated_at, ifnull(s.origin, 'unknown')
		FROM memory_candidates c
		LEFT JOIN sessions s ON s.id = c.session_id
		WHERE 1=1`
	args := []any{}
	if project != "" {
		query += ` AND c.project = ?`
		args = append(args, project)
	}
	if status != "" {
		query += ` AND c.status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY datetime(c.created_at) DESC, c.sync_id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.queryItHook(s.db, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]MemoryCandidate, 0)
	for rows.Next() {
		candidate, err := scanMemoryCandidate(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *candidate)
	}
	return result, rows.Err()
}

func (s *Store) ExpireMemoryCandidates(now time.Time) (int, error) {
	var count int
	err := s.withTx(func(tx *sql.Tx) error {
		var err error
		count, err = expireMemoryCandidatesTx(s, tx, now.UTC())
		return err
	})
	return count, err
}

func expireMemoryCandidatesTx(s *Store, tx *sql.Tx, now time.Time) (int, error) {
	rows, err := s.queryItHook(tx, `
		SELECT sync_id, session_id, project, category FROM memory_candidates
		WHERE status IN ('review','rejected') AND datetime(expires_at) <= datetime(?)
	`, candidateTime(now))
	if err != nil {
		return 0, err
	}
	type expiredRow struct{ id, session, project, category string }
	var expired []expiredRow
	for rows.Next() {
		var row expiredRow
		if err := rows.Scan(&row.id, &row.session, &row.project, &row.category); err != nil {
			_ = rows.Close()
			return 0, err
		}
		expired = append(expired, row)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if len(expired) == 0 {
		return 0, nil
	}
	// Expiry is an explicit operator transition now; it keeps the proposal body
	// and evidence so a mistaken or late review can be revived. Only audit
	// envelopes are disposable.
	if _, err := s.execHook(tx, `
		UPDATE memory_candidates SET status = 'expired',
			rejection_reason = 'ttl_expired', updated_at = ?
		WHERE status IN ('review','rejected') AND datetime(expires_at) <= datetime(?)
	`, candidateTime(now), candidateTime(now)); err != nil {
		return 0, err
	}
	for _, row := range expired {
		if err := insertCandidateAuditTx(s, tx, row.id, row.session, row.project, row.category, "expired", "ttl_expired", now); err != nil {
			return 0, err
		}
	}
	return len(expired), nil
}

func insertCandidateAuditTx(s *Store, tx *sql.Tx, candidateID, sessionID, project, category, action, reason string, now time.Time) error {
	_, err := s.execHook(tx, `
		INSERT INTO memory_candidate_audit (candidate_sync_id, session_id, project, category, action, reason, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, candidateID, sessionID, project, category, action, nullableString(strings.TrimSpace(reason)), candidateTime(now))
	return err
}

func pruneCandidateAuditTx(s *Store, tx *sql.Tx, project string, capacity int) error {
	if capacity <= 0 {
		capacity = defaultCandidateAuditCapacity
	}
	_, err := s.execHook(tx, `
		DELETE FROM memory_candidate_audit
		WHERE project = ? AND id NOT IN (
			SELECT id FROM memory_candidate_audit WHERE project = ? ORDER BY id DESC LIMIT ?
		)
	`, project, project, capacity)
	return err
}
