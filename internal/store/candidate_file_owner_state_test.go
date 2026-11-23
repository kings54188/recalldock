package store

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestAutomaticFileEvidenceRequiresCurrentRegistrationOwner(t *testing.T) {
	for _, mode := range []string{"checked", "tiered"} {
		for _, tc := range []struct {
			name, review string
			stale        bool
			wantPromoted int
		}{{"overdue", "2000-01-01 00:00:00", false, 1}, {"malformed", "not-a-date", false, 1}, {"future", "2999-01-01 00:00:00", false, 1}, {"stale", "", true, 0}} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				s := newTestStore(t)
				root := t.TempDir()
				p, err := s.CreateRegisteredProject("evidence owner fixture")
				if err != nil {
					t.Fatal(err)
				}
				if _, err = s.BindProjectRoot(p.ID, root); err != nil {
					t.Fatal(err)
				}
				if err = s.CreateSession("file-owner-session", p.ID, root); err != nil {
					t.Fatal(err)
				}
				owner, err := s.AddObservation(AddObservationParams{SessionID: "file-owner-session", Project: p.ID, Type: "decision", Title: "Evidence owner", Content: "A registered resource owner"})
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(root, "evidence.txt")
				body := []byte("verified local synthetic source")
				if err = os.WriteFile(path, body, 0600); err != nil {
					t.Fatal(err)
				}
				digest := fmt.Sprintf("%x", sha256.Sum256(body))
				ref := testFileSourceRef(path, digest)
				if _, err = s.AddResourceDependency(AddResourceDependencyParams{SourceRef: ref, DependentObservationID: owner, Project: p.ID}); err != nil {
					t.Fatal(err)
				}
				if tc.stale {
					if _, err = s.db.Exec(`UPDATE observations SET stale_at=datetime('now') WHERE id=?`, owner); err != nil {
						t.Fatal(err)
					}
				} else if _, err = s.db.Exec(`UPDATE observations SET review_after=? WHERE id=?`, tc.review, owner); err != nil {
					t.Fatal(err)
				}
				if err = s.SetCandidateAdmissionMode(mode); err != nil {
					t.Fatal(err)
				}
				r, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{SessionID: "file-owner-session", Project: p.ID, Candidates: []MemoryCandidateInput{{Category: CandidateCategoryDecision, Statement: "Use the currently reviewed source for this decision", Confidence: 0.9, EvidenceRefs: []CandidateEvidenceRef{{Kind: EvidenceKindFile, Ref: ref, Digest: digest}}}}})
				if err != nil {
					t.Fatal(err)
				}
				if r.Promoted != tc.wantPromoted || r.Review != 1-tc.wantPromoted {
					t.Fatalf("owner %s unexpectedly handled: %+v", tc.name, r)
				}
			})
		}
	}
}
