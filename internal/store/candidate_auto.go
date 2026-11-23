package store

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// SetCandidateAdmissionMode is operator configuration, never a candidate field.
func (s *Store) SetCandidateAdmissionMode(mode string) error {
	if mode != "manual" && mode != "duplicates" && mode != "checked" && mode != "tiered" {
		return errors.New("mode must be manual, duplicates, checked or tiered")
	}
	version := 2
	wireMode := mode
	if mode != "manual" {
		wireMode += "-v2"
	}
	if mode == "tiered" {
		version = 4
		wireMode = "tiered-v4"
	}
	body, _ := json.Marshal(struct {
		Version int    `json:"version"`
		Mode    string `json:"mode"`
	}{version, wireMode})
	f, err := os.CreateTemp(s.cfg.DataDir, ".candidate-policy-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(body); err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	if err := os.Rename(f.Name(), filepath.Join(s.cfg.DataDir, "candidate-admission.json")); err != nil {
		return err
	}
	return syncDir(s.cfg.DataDir)
}

func (s *Store) candidateAdmissionMode() string {
	path := filepath.Join(s.cfg.DataDir, "candidate-admission.json")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 512 {
		return "manual"
	}
	f, err := os.Open(path)
	if err != nil {
		return "manual"
	}
	defer f.Close()
	var policy struct {
		Version json.RawMessage `json:"version"`
		Mode    string          `json:"mode"`
	}
	dec := json.NewDecoder(io.LimitReader(f, 513))
	dec.DisallowUnknownFields()
	if dec.Decode(&policy) != nil {
		return "manual"
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return "manual"
	}
	if len(policy.Version) > 0 {
		var version int
		if err := json.Unmarshal(policy.Version, &version); err != nil || (version != 2 && version != 3 && version != 4) {
			return "manual"
		}
		switch policy.Mode {
		case "duplicates-v2":
			if version == 2 {
				return "duplicates"
			}
		case "checked-v2":
			if version == 2 {
				return "checked"
			}
		case "tiered-v3":
			if version == 3 {
				return "tiered"
			}
		case "tiered-v4":
			if version == 4 {
				return "tiered"
			}
		}
		return "manual"
	}
	if policy.Mode == "duplicates" || policy.Mode == "checked" {
		return policy.Mode
	}
	return "manual"
}

func (s *Store) autoAdmitCandidates(result *CommitMemoryCandidatesResult) {
	mode := s.candidateAdmissionMode()
	if mode == "manual" {
		return
	}
	for i := range result.Results {
		entry := &result.Results[i]
		if entry.Status != CandidateStatusReview {
			continue
		}
		c, err := s.GetMemoryCandidate(entry.CandidateID)
		if err != nil {
			continue
		}
		// Recheck all existing lexical/metadata guards. Evidence claims never count
		// as an independent verifier in duplicates mode.
		prepared := prepareMemoryCandidate(MemoryCandidateInput{Category: c.Category, Statement: c.Statement, EvidenceRefs: c.EvidenceRefs, Confidence: c.Confidence, MemoryPath: c.MemoryPath, ProposedTopicKey: c.ProposedTopicKey}, normalizeCandidatePolicy(CandidateAdmissionPolicy{}))
		if prepared.status != CandidateStatusReview {
			continue
		}
		if mode == "duplicates" {
			_, _, scope := candidateObservationShape(c.Category)
			var n int
			if s.db.QueryRow(`SELECT COUNT(*) FROM observations WHERE content=? AND project=? AND scope=? AND deleted_at IS NULL`, c.Statement, c.Project, scope).Scan(&n) != nil || n == 0 {
				continue
			}
		}
		if (mode == "checked" || mode == "tiered") && len(c.EvidenceRefs) == 0 {
			// A reference is necessary, not sufficient. Actual local-source
			// verification runs inside the admission transaction below.
			continue
		}
		if mode == "tiered" && !tieredAutoAdmit(c) {
			continue
		}
		digest, err := MemoryCandidateDigest(c)
		if err != nil {
			continue
		}
		if _, err = s.approveMemoryCandidate(c.SyncID, digest, "Automatic operator policy: "+mode, mode); err != nil {
			if errors.Is(err, ErrCandidateEvidenceUnverified) {
				entry.Reason = "evidence_not_verified"
			}
			continue
		}
		entry.Status = CandidateStatusPromoted
		entry.Reason = "automatic_policy_" + mode
		result.Review--
		result.Promoted++
	}
}

func durableTierCategory(category string) bool {
	switch category {
	case CandidateCategoryDecision, CandidateCategoryProcedure,
		CandidateCategoryProjectConstraint, CandidateCategoryFailureAvoidance,
		CandidateCategoryVerifiedOutcome:
		return true
	}
	return false
}

func tieredAutoAdmit(c *MemoryCandidate) bool {
	if c.Confidence < 0.85 {
		return false
	}
	if !durableTierCategory(c.Category) {
		return false
	}
	// A topic-key update may rewrite an existing topic; keep it for review so a
	// merge is an explicit decision instead of an automatic overwrite.
	if strings.TrimSpace(c.ProposedTopicKey) != "" {
		return false
	}
	return true
}

// CandidateAdmissionMode reports the current operator admission policy for
// operator-facing surfaces (CLI list header, diagnostics). Read-only; it
// grants no admission authority to callers.
func (s *Store) CandidateAdmissionMode() string {
	return s.candidateAdmissionMode()
}
