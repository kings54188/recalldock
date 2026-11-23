package store

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testFileSourceRef(path, digest string) string {
	slashed := filepath.ToSlash(filepath.Clean(path))
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	return "file://" + slashed + "@sha256:" + digest
}

func TestCheckedFileEvidenceRequiresRegisteredCurrentScopedBytes(t *testing.T) {
	for _, kind := range []string{"valid", "unregistered", "changed", "outside-root", "symlink", "wrong-digest", "stale-registration"} {
		t.Run(kind, func(t *testing.T) {
			s := newTestStore(t)
			root := t.TempDir()
			p, err := s.CreateRegisteredProject("File evidence fixture")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.BindProjectRoot(p.ID, root); err != nil {
				t.Fatal(err)
			}
			if err := s.CreateSession("file-session", p.ID, root); err != nil {
				t.Fatal(err)
			}
			owner, err := s.AddObservation(AddObservationParams{SessionID: "file-session", Project: p.ID, Type: "decision", Title: "Registration owner", Content: "Resource registration fixture"})
			if err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(root, "evidence.txt")
			if kind == "outside-root" {
				source = filepath.Join(t.TempDir(), "evidence.txt")
			}
			body := []byte("Synthetic immutable source bytes")
			if err := os.WriteFile(source, body, 0600); err != nil {
				t.Fatal(err)
			}
			if kind == "symlink" {
				link := filepath.Join(root, "linked.txt")
				if err := os.Symlink(source, link); err != nil {
					t.Fatal(err)
				}
				source = link
			}
			digest := fmt.Sprintf("%x", sha256.Sum256(body))
			ref := testFileSourceRef(source, digest)
			if kind != "unregistered" {
				if _, err := s.AddResourceDependency(AddResourceDependencyParams{SourceRef: ref, DependentObservationID: owner, Project: p.ID}); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "changed" {
				if err := os.WriteFile(source, []byte("Changed source bytes"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "stale-registration" {
				if _, err := s.db.Exec("UPDATE resource_dependencies SET status='stale' WHERE source_ref=?", ref); err != nil {
					t.Fatal(err)
				}
			}
			claimDigest := digest
			if kind == "wrong-digest" {
				claimDigest = strings.Repeat("0", 64)
			}
			if err := s.SetCandidateAdmissionMode("checked"); err != nil {
				t.Fatal(err)
			}
			r, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{SessionID: "file-session", Project: p.ID, Candidates: []MemoryCandidateInput{{Category: CandidateCategoryDecision, Statement: "Preserve exact source snapshots for bounded verification", Confidence: 0.9, EvidenceRefs: []CandidateEvidenceRef{{Kind: EvidenceKindFile, Ref: ref, Digest: claimDigest}}}}})
			if err != nil {
				t.Fatal(err)
			}
			if kind != "valid" {
				if r.Promoted != 0 || r.Review != 1 {
					t.Fatalf("invalid file evidence admitted: %+v", r)
				}
				return
			}
			if r.Promoted != 1 {
				t.Fatalf("valid registered file evidence refused: %+v", r)
			}
			c, err := s.GetMemoryCandidate(r.CandidateIDs[0])
			if err != nil {
				t.Fatal(err)
			}
			dependencies, err := s.ListResourceDependencies(ResourceDependencyListOptions{Project: p.ID})
			if err != nil || len(dependencies) != 2 {
				t.Fatalf("new fact lost source dependency: %+v %v", dependencies, err)
			}
			linked := false
			for _, dep := range dependencies {
				linked = linked || (dep.SourceRef == ref && dep.DependentObservationSyncID == c.PromotedObservationSyncID)
			}
			if !linked {
				t.Fatal("verified file not bound to admitted fact")
			}
		})
	}
}
