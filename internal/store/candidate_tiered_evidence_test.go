package store

import "testing"

func TestTieredAdmissionPersistsEvidenceAndRollsBackOnDependencyFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "verified"
		if fail {
			name = "dependency_failure"
		}
		t.Run(name, func(t *testing.T) {
			s := newCandidateTestStore(t)
			id, err := s.AddObservation(AddObservationParams{SessionID: "candidate-session", Project: "candidate-project", Type: "decision", Title: "Reviewed baseline", Content: "Original supporting knowledge"})
			if err != nil {
				t.Fatal(err)
			}
			source, err := s.GetObservation(id)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := s.SelectObservationForEdit(id)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.SetCandidateAdmissionMode("tiered"); err != nil {
				t.Fatal(err)
			}
			if fail {
				if _, err := s.db.Exec(`CREATE TRIGGER fail_tiered_dependency BEFORE INSERT ON memory_dependencies BEGIN SELECT RAISE(ABORT, 'dependency failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			result, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{SessionID: "candidate-session", Project: "candidate-project", Candidates: []MemoryCandidateInput{{Category: CandidateCategoryDecision, Statement: "Use the verified source when validating recovery", Confidence: 0.9, EvidenceRefs: []CandidateEvidenceRef{{Kind: EvidenceKindObservation, Ref: source.SyncID, Digest: snapshot.SnapshotSHA256}}}}})
			if err != nil {
				t.Fatal(err)
			}
			candidate, err := s.GetMemoryCandidate(result.CandidateIDs[0])
			if err != nil {
				t.Fatal(err)
			}
			var auditCount int
			if err := s.db.QueryRow(`SELECT count(*) FROM memory_candidate_audit WHERE candidate_sync_id=? AND action='evidence_checked'`, candidate.SyncID).Scan(&auditCount); err != nil {
				t.Fatal(err)
			}
			edges, err := s.ListMemoryDependencies(MemoryDependencyListOptions{Project: "candidate-project"})
			if err != nil {
				t.Fatal(err)
			}
			if fail {
				if result.Promoted != 0 || candidate.Status != CandidateStatusReview || len(edges) != 0 || auditCount != 0 {
					t.Fatalf("failed transaction leaked: result=%+v candidate=%+v dependencies=%d audits=%d", result, candidate, len(edges), auditCount)
				}
				stats, err := s.Stats()
				if err != nil || stats.TotalObservations != 1 {
					t.Fatalf("failed transaction left observation: %+v %v", stats, err)
				}
				return
			}
			if result.Promoted != 1 || auditCount != 1 || len(edges) != 1 || edges[0].SourceObservationSyncID != source.SyncID || edges[0].DependentObservationSyncID != candidate.PromotedObservationSyncID {
				t.Fatalf("missing evidence: result=%+v candidate=%+v dependencies=%+v audits=%d", result, candidate, edges, auditCount)
			}
			derived, err := s.GetObservationBySyncID(candidate.PromotedObservationSyncID)
			if err != nil {
				t.Fatal(err)
			}
			changed := "Updated supporting knowledge"
			if _, err := s.UpdateObservation(id, UpdateObservationParams{Content: &changed}); err != nil {
				t.Fatal(err)
			}
			derived, err = s.GetObservation(derived.ID)
			if err != nil || derived.StaleAt == nil {
				t.Fatalf("source change did not mark derived knowledge stale: %+v %v", derived, err)
			}
		})
	}
}
