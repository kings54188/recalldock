package store

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
)

func TestCheckedAdmissionRejectsStaleForeignAndMixedEvidence(t *testing.T) {
	for _, kind := range []string{"wrong-digest", "changed", "foreign", "archived", "deleted", "mixed"} {
		t.Run(kind, func(t *testing.T) {
			s := newCandidateTestStore(t)
			p, sid := "candidate-project", "candidate-session"
			if kind == "foreign" {
				p, sid = "other-project", "other-session"
				if err := s.CreateSession(sid, p, t.TempDir()); err != nil {
					t.Fatal(err)
				}
			}
			id, err := s.AddObservation(AddObservationParams{SessionID: sid, Project: p, Type: "decision", Title: "Source fixture", Content: "Reviewed current source"})
			if err != nil {
				t.Fatal(err)
			}
			obs, err := s.GetObservation(id)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := s.SelectObservationForEdit(id)
			if err != nil {
				t.Fatal(err)
			}
			ref := CandidateEvidenceRef{Kind: EvidenceKindObservation, Ref: obs.SyncID, Digest: snapshot.SnapshotSHA256}
			switch kind {
			case "wrong-digest":
				ref.Digest = strings.Repeat("0", 64)
			case "changed":
				content := "New source revision"
				if _, err := s.UpdateObservation(id, UpdateObservationParams{Content: &content}); err != nil {
					t.Fatal(err)
				}
			case "archived":
				archived := true
				if _, err := s.UpdateObservation(id, UpdateObservationParams{Archived: &archived}); err != nil {
					t.Fatal(err)
				}
				current, err := s.SelectObservationForEdit(id)
				if err != nil {
					t.Fatal(err)
				}
				ref.Digest = current.SnapshotSHA256
			case "deleted":
				if err := s.DeleteObservation(id, false); err != nil {
					t.Fatal(err)
				}
			}
			refs := []CandidateEvidenceRef{ref}
			if kind == "mixed" {
				refs = append(refs, CandidateEvidenceRef{Kind: EvidenceKindTest, Ref: "forged-test-receipt", Digest: strings.Repeat("a", 64)})
			}
			if err := s.SetCandidateAdmissionMode("checked"); err != nil {
				t.Fatal(err)
			}
			r, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{SessionID: "candidate-session", Project: "candidate-project", Candidates: []MemoryCandidateInput{{Category: CandidateCategoryDecision, Statement: "Use explicit provenance before persisting a durable conclusion", Confidence: 0.9, EvidenceRefs: refs}}})
			if err != nil || r.Promoted != 0 || r.Review != 1 || r.Results[0].Reason != "evidence_not_verified" {
				t.Fatalf("unsafe source admitted: %+v %v", r, err)
			}
		})
	}
}

func TestTieredConfidenceNeverOverridesEvidence(t *testing.T) {
	for _, kind := range []string{"wrong-digest", "changed", "foreign", "archived", "deleted", "mixed", "claimed-test"} {
		t.Run(kind, func(t *testing.T) {
			s := newCandidateTestStore(t)
			project, session := "candidate-project", "candidate-session"
			if kind == "foreign" {
				project, session = "foreign-project", "foreign-session"
				if err := s.CreateSession(session, project, t.TempDir()); err != nil {
					t.Fatal(err)
				}
			}
			id, err := s.AddObservation(AddObservationParams{SessionID: session, Project: project, Type: "decision", Title: "Source", Content: "Current reviewed source"})
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
			ref := CandidateEvidenceRef{Kind: EvidenceKindObservation, Ref: source.SyncID, Digest: snapshot.SnapshotSHA256}
			switch kind {
			case "wrong-digest":
				ref.Digest = strings.Repeat("0", 64)
			case "changed":
				content := "Changed source"
				if _, err := s.UpdateObservation(id, UpdateObservationParams{Content: &content}); err != nil {
					t.Fatal(err)
				}
			case "archived":
				archived := true
				if _, err := s.UpdateObservation(id, UpdateObservationParams{Archived: &archived}); err != nil {
					t.Fatal(err)
				}
			case "deleted":
				if err := s.DeleteObservation(id, false); err != nil {
					t.Fatal(err)
				}
			case "claimed-test":
				ref = CandidateEvidenceRef{Kind: EvidenceKindTest, Ref: "self-reported-receipt", Digest: strings.Repeat("a", 64)}
			}
			refs := []CandidateEvidenceRef{ref}
			if kind == "mixed" {
				refs = append(refs, CandidateEvidenceRef{Kind: EvidenceKindSource, Ref: "unverified historical claim"})
			}
			if err := s.SetCandidateAdmissionMode("tiered"); err != nil {
				t.Fatal(err)
			}
			result, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{SessionID: "candidate-session", Project: "candidate-project", Candidates: []MemoryCandidateInput{{Category: CandidateCategoryDecision, Statement: "Preserve verified evidence as a mandatory admission condition", Confidence: 1, EvidenceRefs: refs}}})
			if err != nil || result.Promoted != 0 || result.Review != 1 || result.Results[0].Reason != "evidence_not_verified" {
				t.Fatalf("confidence overrode a hard evidence condition: %+v %v", result, err)
			}
		})
	}
}

func TestCheckedAdmissionEvidenceAndKnowledgeRollbackTogether(t *testing.T) {
	s := newCandidateTestStore(t)
	id, err := s.AddObservation(AddObservationParams{SessionID: "candidate-session", Project: "candidate-project", Type: "decision", Title: "Source", Content: "Reviewed source fixture"})
	if err != nil {
		t.Fatal(err)
	}
	obs, err := s.GetObservation(id)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.SelectObservationForEdit(id)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{SessionID: "candidate-session", Project: "candidate-project", Candidates: []MemoryCandidateInput{{Category: CandidateCategoryDecision, Statement: "Retain provenance edges alongside admitted facts", Confidence: 0.9, EvidenceRefs: []CandidateEvidenceRef{{Kind: EvidenceKindObservation, Ref: obs.SyncID, Digest: snapshot.SnapshotSHA256}}}}})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := s.GetMemoryCandidate(r.CandidateIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	digest, err := MemoryCandidateDigest(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetCandidateAdmissionMode("checked"); err != nil {
		t.Fatal(err)
	}
	original := s.hooks.commit
	s.hooks.commit = func(tx *sql.Tx) error { tx.Rollback(); return errors.New("synthetic commit failure") }
	if _, err := s.approveMemoryCandidate(candidate.SyncID, digest, "Synthetic checked review", "checked"); err == nil {
		t.Fatal("commit failure hidden")
	}
	s.hooks.commit = original
	candidate, err = s.GetMemoryCandidate(candidate.SyncID)
	if err != nil || candidate.Status != CandidateStatusReview {
		t.Fatal("failed transaction promoted candidate")
	}
	stats, err := s.Stats()
	if err != nil || stats.TotalObservations != 1 {
		t.Fatal("failed transaction left knowledge")
	}
	for _, table := range []string{"memory_dependencies", "memory_candidate_audit"} {
		query := "SELECT count(*) FROM " + table
		if table == "memory_candidate_audit" {
			query += " WHERE action IN ('evidence_checked','promoted')"
		}
		var count int
		if err := s.db.QueryRow(query).Scan(&count); err != nil || count != 0 {
			t.Fatal("partial evidence/dependency commit", table, count, err)
		}
	}
}

func TestSelectedSnapshotMetadataRemainsConsistentAcrossRevisions(t *testing.T) {
	s := newCandidateTestStore(t)
	id, err := s.AddObservation(AddObservationParams{SessionID: "candidate-session", Project: "candidate-project", Type: "decision", Title: "Source", Content: "Old selected body"})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := s.GetObservation(id)
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.SelectObservationForEdit(id)
	if err != nil {
		t.Fatal(err)
	}
	content := "Newer source body"
	if _, err := s.UpdateObservation(id, UpdateObservationParams{Content: &content}); err != nil {
		t.Fatal(err)
	}
	metadata, err := s.ObservationSnapshotMetadata(selected)
	if err != nil {
		t.Fatal(err)
	}
	after, err := s.SelectObservationForEdit(id)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.State != nil || metadata.SnapshotSHA256 != before.SnapshotSHA256 || metadata.SnapshotSHA256 == after.SnapshotSHA256 {
		t.Fatal("digest mismatched the state already selected/rendered")
	}
}

func TestAutomaticDuplicatesCannotCreateAfterExistingFactDisappears(t *testing.T) {
	s := newCandidateTestStore(t)
	const statement = "Keep exact existing facts as duplicates instead of creating new knowledge"
	r, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{SessionID: "candidate-session", Project: "candidate-project", Candidates: []MemoryCandidateInput{verifiedCandidate(CandidateCategoryDecision, statement)}})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := s.GetMemoryCandidate(r.CandidateIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	digest, err := MemoryCandidateDigest(candidate)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.AddObservation(AddObservationParams{SessionID: "candidate-session", Project: "candidate-project", Type: "decision", Title: "Existing fact", Content: statement})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetCandidateAdmissionMode("duplicates"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteObservation(id, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.approveMemoryCandidate(candidate.SyncID, digest, "Synthetic automatic duplicate", "duplicates"); err == nil {
		t.Fatal("automatic duplicate policy created replacement knowledge")
	}
	stats, err := s.Stats()
	if err != nil || stats.TotalObservations != 0 {
		t.Fatal("unrequested knowledge created after source disappeared")
	}
}

func TestCheckedAdmissionRejectsModelAssertionsAsVerification(t *testing.T) {
	for _, ref := range []CandidateEvidenceRef{{Kind: EvidenceKindTest, Ref: "go-test/claimed-passed", Digest: strings.Repeat("a", 64)}, {Kind: EvidenceKindCommand, Ref: "claimed-command-success", Digest: strings.Repeat("b", 64)}, {Kind: EvidenceKindObservation, Ref: "obs-missing", Digest: strings.Repeat("c", 64)}, {Kind: EvidenceKindSource, Ref: "https://example.invalid/claimed-evidence", Digest: strings.Repeat("d", 64)}} {
		t.Run(ref.Kind, func(t *testing.T) {
			s := newCandidateTestStore(t)
			if err := s.SetCandidateAdmissionMode("checked"); err != nil {
				t.Fatal(err)
			}
			r, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{SessionID: "candidate-session", Project: "candidate-project", Candidates: []MemoryCandidateInput{{Category: CandidateCategoryDecision, Statement: "Use isolated database snapshots for reproducible restore validation", Confidence: 0.9, EvidenceRefs: []CandidateEvidenceRef{ref}}}})
			if err != nil || r.Promoted != 0 || r.Review != 1 {
				t.Fatalf("unverified assertion became knowledge: %+v %v", r, err)
			}
		})
	}
}

func TestCheckedAdmissionBindsCurrentObservationEvidenceAndDependency(t *testing.T) {
	s := newCandidateTestStore(t)
	id, err := s.AddObservation(AddObservationParams{SessionID: "candidate-session", Project: "candidate-project", Type: "decision", Title: "Reviewed baseline", Content: "Use an isolated backup copy for validation"})
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
	if err := s.SetCandidateAdmissionMode("checked"); err != nil {
		t.Fatal(err)
	}
	r, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{SessionID: "candidate-session", Project: "candidate-project", Candidates: []MemoryCandidateInput{{Category: CandidateCategoryDecision, Statement: "Keep restore validation separate from the active database", Confidence: 0.9, EvidenceRefs: []CandidateEvidenceRef{{Kind: EvidenceKindObservation, Ref: source.SyncID, Digest: snapshot.SnapshotSHA256}}}}})
	if err != nil || r.Promoted != 1 {
		t.Fatalf("current selected evidence refused: %+v %v", r, err)
	}
	candidate, err := s.GetMemoryCandidate(r.CandidateIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	derived, err := s.GetObservationBySyncID(candidate.PromotedObservationSyncID)
	if err != nil {
		t.Fatal(err)
	}
	edges, err := s.ListMemoryDependencies(MemoryDependencyListOptions{Project: "candidate-project"})
	if err != nil || len(edges) != 1 || edges[0].SourceObservationSyncID != source.SyncID || edges[0].DependentObservationSyncID != derived.SyncID {
		t.Fatalf("verified evidence lost dependency: %+v %v", edges, err)
	}
	content := "Baseline is no longer valid"
	if _, err := s.UpdateObservation(id, UpdateObservationParams{Content: &content}); err != nil {
		t.Fatal(err)
	}
	current, err := s.GetObservation(derived.ID)
	if err != nil || current.StaleAt == nil {
		t.Fatal("source change did not invalidate admitted derivative")
	}
}

func submitLifecycleCandidate(t *testing.T, s *Store, sourceID int64, statement string) *CommitMemoryCandidatesResult {
	t.Helper()
	source, err := s.GetObservation(sourceID)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := s.SelectObservationForEdit(sourceID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{SessionID: "candidate-session", Project: "candidate-project", Candidates: []MemoryCandidateInput{{Category: CandidateCategoryDecision, Statement: statement, Confidence: 1, EvidenceRefs: []CandidateEvidenceRef{{Kind: EvidenceKindObservation, Ref: source.SyncID, Digest: selected.SnapshotSHA256}}}}})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestLifecycleMetadataAndSummaryKeepUnresolvedStale(t *testing.T) {
	for _, kind := range []string{"stale-memory-parent", "archived-memory-parent", "stale-resource-summary"} {
		t.Run(kind, func(t *testing.T) {
			s := newCandidateTestStore(t)
			a := addDependencyObservation(t, s, "candidate-session", "candidate-project", "Root supporting source")
			parent := a
			if kind == "stale-memory-parent" {
				parent = addDependencyObservation(t, s, "candidate-session", "candidate-project", "Intermediate supporting source")
				if _, err := s.AddMemoryDependency(AddMemoryDependencyParams{SourceObservationID: a, DependentObservationID: parent}); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.SetCandidateAdmissionMode("tiered"); err != nil {
				t.Fatal(err)
			}
			first := submitLifecycleCandidate(t, s, parent, "Keep the reviewed supporting source linked to durable derived knowledge")
			if first.Promoted != 1 {
				t.Fatalf("initial admission failed: %+v", first)
			}
			candidate, err := s.GetMemoryCandidate(first.CandidateIDs[0])
			if err != nil {
				t.Fatal(err)
			}
			derived, err := s.GetObservationBySyncID(candidate.PromotedObservationSyncID)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.PinObservation(derived.ID); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "stale-resource-summary":
				if _, err := s.AddResourceDependency(AddResourceDependencyParams{SourceRef: testResourceRefA, DependentObservationID: derived.ID, Project: "candidate-project"}); err != nil {
					t.Fatal(err)
				}
				if _, err := s.NotifyResourceChanged(NotifyResourceChangeParams{Project: "candidate-project", SourceRef: testResourceRefA, ReplacementSourceRef: testResourceRefB}); err != nil {
					t.Fatal(err)
				}
			case "archived-memory-parent":
				flag := true
				if _, err := s.UpdateObservation(a, UpdateObservationParams{Archived: &flag}); err != nil {
					t.Fatal(err)
				}
			default:
				changed := "The root source was revised and previous derived conclusions need verification"
				if _, err := s.UpdateObservation(a, UpdateObservationParams{Content: &changed}); err != nil {
					t.Fatal(err)
				}
			}
			before, err := s.GetObservation(derived.ID)
			if err != nil || before.StaleAt == nil {
				t.Fatalf("invalidation missing before edit: %+v %v", before, err)
			}
			selected, err := s.SelectObservationForEdit(derived.ID)
			if err != nil {
				t.Fatal(err)
			}
			topic := "lifecycle/cosmetic-label"
			patch := UpdateObservationParams{TopicKey: &topic}
			if kind == "stale-resource-summary" {
				abstract, overview := "A replacement display abstract", "A replacement display overview"
				patch = UpdateObservationParams{Abstract: &abstract, Overview: &overview}
			}
			if _, err := s.UpdateObservationCAS(derived.ID, selected.SnapshotSHA256, patch, "Only change display metadata, not source verification", ""); err != nil {
				t.Fatal(err)
			}
			after, err := s.GetObservation(derived.ID)
			if err != nil {
				t.Fatal(err)
			}
			edges, err := s.ListMemoryDependencies(MemoryDependencyListOptions{DependentObservationSyncID: after.SyncID})
			if err != nil {
				t.Fatal(err)
			}
			resources, err := s.ListResourceDependencies(ResourceDependencyListOptions{Project: "candidate-project", DependentObservationSyncID: after.SyncID})
			if err != nil {
				t.Fatal(err)
			}
			admitted := submitLifecycleCandidate(t, s, after.ID, "Only derive fresh decisions from sources whose invalidation has actually been resolved")
			t.Logf("kind=%s pinned=%v stale_before=%v stale_after=%v memory_edges=%+v resource_edges=%+v promoted=%d", kind, after.Pinned, before.StaleAt, after.StaleAt, edges, resources, admitted.Promoted)
			if after.StaleAt == nil {
				t.Error("cosmetic metadata/summary edit cleared unresolved stale")
			}
			if admitted.Promoted != 0 {
				t.Error("tiered-v4 admitted knowledge from an unresolved dependency source")
			}
			for _, edge := range edges {
				if kind != "stale-resource-summary" && edge.Status != DependencyStatusStale {
					t.Error("cosmetic edit rebound an invalid memory source")
				}
			}
			if !after.Pinned || after.RevisionCount != before.RevisionCount+1 || after.ArchivedAt != nil {
				t.Error("edit changed pin/archive/revision boundaries")
			}
		})
	}
}
