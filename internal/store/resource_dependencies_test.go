package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const (
	testResourceRefA = "git:0123456789abcdef0123456789abcdef01234567:docs/source.md#L1-L8@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testResourceRefB = "git:89abcdef0123456789abcdef0123456789abcdef:docs/source.md#L1-L9@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestResourceIdentityStatusUsesExactSourceAndProject(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("resource-identity", "alpha", "/tmp/alpha"); err != nil {
		t.Fatal(err)
	}
	id := addDependencyObservation(t, s, "resource-identity", "alpha", "Resource identity")
	ref := "file:///tmp/资源@branch.md@sha256:" + strings.Repeat("a", 64)
	if _, err := s.AddResourceDependency(AddResourceDependencyParams{SourceRef: ref, DependentObservationID: id, Project: "alpha"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ project, ref, want string }{
		{"alpha", ref, "unchanged"},
		{" ALPHA ", ref, "unchanged"},
		{"beta", ref, "unregistered"},
		{"alpha", "file:///tmp/资源@sha256:" + strings.Repeat("a", 64), "unregistered"},
		{"alpha", "file:///tmp/资源@branch.md@sha256:" + strings.Repeat("b", 64), "changed"},
		{"alpha", "file:///tmp/资源@branch.md@SHA256:" + strings.Repeat("A", 64), "unchanged"},
	} {
		got, err := s.ResourceIdentityStatus(tc.project, tc.ref)
		if err != nil || got != tc.want {
			t.Fatalf("project=%q ref=%q got=%q want=%q err=%v", tc.project, tc.ref, got, tc.want, err)
		}
	}
	if _, err := s.ResourceIdentityStatus(" ", ref); !errors.Is(err, ErrResourceDependencyProjectRequired) {
		t.Fatalf("empty project: %v", err)
	}
	if _, err := s.ResourceIdentityStatus("alpha", "file:///tmp/source.md"); !errors.Is(err, ErrInvalidResourceSourceRef) {
		t.Fatalf("invalid source: %v", err)
	}
}

func TestResourceDependencyValidationAndProjectIsolation(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("resource-alpha", "alpha", "/tmp/alpha"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession("resource-beta", "beta", "/tmp/beta"); err != nil {
		t.Fatal(err)
	}
	alpha := addDependencyObservation(t, s, "resource-alpha", "alpha", "Alpha memory")
	beta := addDependencyObservation(t, s, "resource-beta", "beta", "Beta memory")

	invalidRefs := []string{
		"docs/source.md@sha256:" + strings.Repeat("a", 64),
		"git:ref:docs/source.md",
		"git:ref:docs/\nsource.md@sha256:" + strings.Repeat("a", 64),
		"git:" + strings.Repeat("x", maxResourceSourceRefLength) + "@sha256:" + strings.Repeat("a", 64),
	}
	for _, sourceRef := range invalidRefs {
		if _, err := s.AddResourceDependency(AddResourceDependencyParams{
			SourceRef: sourceRef, DependentObservationID: alpha, Project: "alpha",
		}); !errors.Is(err, ErrInvalidResourceSourceRef) {
			t.Fatalf("source_ref %q err=%v", sourceRef, err)
		}
	}
	if _, err := s.AddResourceDependency(AddResourceDependencyParams{
		SourceRef: testResourceRefA, DependentObservationID: alpha,
	}); !errors.Is(err, ErrResourceDependencyProjectRequired) {
		t.Fatalf("missing project err=%v", err)
	}
	if _, err := s.AddResourceDependency(AddResourceDependencyParams{
		SourceRef: testResourceRefA, DependentObservationID: beta, Project: "alpha",
	}); !errors.Is(err, ErrCrossProjectResourceDependency) {
		t.Fatalf("cross-project err=%v", err)
	}

	dependency, err := s.AddResourceDependency(AddResourceDependencyParams{
		SourceRef: testResourceRefA, DependentObservationID: alpha,
		Project: "alpha", Reason: "derived from the pinned source",
	})
	if err != nil {
		t.Fatal(err)
	}
	if dependency.Status != DependencyStatusActive || dependency.SourceRef != testResourceRefA || dependency.Project != "alpha" {
		t.Fatalf("dependency=%+v", dependency)
	}
	listed, err := s.ListResourceDependencies(ResourceDependencyListOptions{
		Project: "alpha", SourceRef: testResourceRefA, Status: DependencyStatusActive,
	})
	if err != nil || len(listed) != 1 || listed[0].SyncID != dependency.SyncID {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
}

func TestResourceChangeMarksDirectAndMemoryDependentsStaleUntilExplicitRefresh(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("resource-change", "engram", "/tmp/engram"); err != nil {
		t.Fatal(err)
	}
	direct := addDependencyObservation(t, s, "resource-change", "engram", "Resource-derived memory")
	downstream := addDependencyObservation(t, s, "resource-change", "engram", "Downstream memory")
	if _, err := s.AddMemoryDependency(AddMemoryDependencyParams{
		SourceObservationID: direct, DependentObservationID: downstream, Reason: "derived from direct memory",
	}); err != nil {
		t.Fatal(err)
	}
	resourceDependency, err := s.AddResourceDependency(AddResourceDependencyParams{
		SourceRef: testResourceRefA, DependentObservationID: direct,
		Project: "engram", Reason: "resource provenance",
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := s.NotifyResourceChanged(NotifyResourceChangeParams{
		Project: "engram", SourceRef: testResourceRefA,
		ReplacementSourceRef: testResourceRefB, Reason: "pinned source changed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Matched != 1 || result.Staled != 1 {
		t.Fatalf("result=%+v", result)
	}
	for _, id := range []int64{direct, downstream} {
		observation, getErr := s.GetObservation(id)
		if getErr != nil || observation.StaleAt == nil || observation.StaleReason == nil {
			t.Fatalf("observation %d=%+v err=%v", id, observation, getErr)
		}
	}
	directObservation, _ := s.GetObservation(direct)
	if !strings.HasPrefix(*directObservation.StaleReason, "resource:") || !strings.Contains(*directObservation.StaleReason, testResourceRefA) {
		t.Fatalf("direct stale reason=%q", *directObservation.StaleReason)
	}

	updatedContent := "Recomputed content from the replacement source."
	if _, err := s.UpdateObservation(direct, UpdateObservationParams{Content: &updatedContent}); err != nil {
		t.Fatal(err)
	}
	directObservation, _ = s.GetObservation(direct)
	if directObservation.StaleAt == nil {
		t.Fatal("ordinary observation update must not clear resource stale state")
	}

	refreshed, err := s.RefreshResourceDependency(RefreshResourceDependencyParams{
		SyncID: resourceDependency.SyncID, Project: "engram",
	})
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.SourceRef != testResourceRefB || refreshed.ReplacementSourceRef != nil || refreshed.Status != DependencyStatusActive {
		t.Fatalf("refreshed=%+v", refreshed)
	}
	directObservation, _ = s.GetObservation(direct)
	if directObservation.StaleAt != nil || directObservation.StaleReason != nil {
		t.Fatalf("explicit refresh did not clear resource stale state: %+v", directObservation)
	}
	downstreamObservation, _ := s.GetObservation(downstream)
	if downstreamObservation.StaleAt == nil {
		t.Fatal("refreshing the direct memory must not silently refresh downstream memories")
	}

	replayed, err := s.NotifyResourceChanged(NotifyResourceChangeParams{
		Project: "engram", SourceRef: testResourceRefA,
		ReplacementSourceRef: testResourceRefB, Reason: "replayed event",
	})
	if err != nil || replayed.Matched != 0 || replayed.Staled != 0 {
		t.Fatalf("replayed=%+v err=%v", replayed, err)
	}
}

func TestResourceDependencySyncReplayIsIdempotent(t *testing.T) {
	sourceStore := newTestStore(t)
	if err := sourceStore.CreateSession("resource-sync", "engram", "/tmp/engram"); err != nil {
		t.Fatal(err)
	}
	dependentID := addDependencyObservation(t, sourceStore, "resource-sync", "engram", "Resource dependent")
	dependency, err := sourceStore.AddResourceDependency(AddResourceDependencyParams{
		SourceRef: testResourceRefA, DependentObservationID: dependentID,
		Project: "engram", Reason: "sync provenance",
	})
	if err != nil {
		t.Fatal(err)
	}
	dependent, _ := sourceStore.GetObservation(dependentID)

	replica := newTestStore(t)
	if err := replica.CreateSession("resource-sync", "engram", "/tmp/engram"); err != nil {
		t.Fatal(err)
	}
	applyObservationPayloadForTest(t, replica, 1, dependent)
	encoded, err := json.Marshal(resourceDependencyPayloadFromResourceDependency(dependency))
	if err != nil {
		t.Fatal(err)
	}
	for _, seq := range []int64{2, 3} {
		if err := replica.ApplyPulledMutation(DefaultSyncTargetKey, SyncMutation{
			Seq: seq, Entity: SyncEntityResourceDependency, EntityKey: dependency.SyncID,
			Op: SyncOpUpsert, Payload: string(encoded), Project: "engram",
		}); err != nil {
			t.Fatalf("apply resource dependency seq=%d: %v", seq, err)
		}
	}
	listed, err := replica.ListResourceDependencies(ResourceDependencyListOptions{Project: "engram"})
	if err != nil || len(listed) != 1 {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}

	changedPayload := resourceDependencyPayloadFromResourceDependency(dependency)
	changedPayload.Status = DependencyStatusStale
	changedPayload.ReplacementSourceRef = testResourceRefB
	changedPayload.UpdatedAt = Now()
	changedEncoded, _ := json.Marshal(changedPayload)
	if err := replica.ApplyPulledMutation(DefaultSyncTargetKey, SyncMutation{
		Seq: 4, Entity: SyncEntityResourceDependency, EntityKey: dependency.SyncID,
		Op: SyncOpUpsert, Payload: string(changedEncoded), Project: "engram",
	}); err != nil {
		t.Fatal(err)
	}
	replicaDependent, err := replica.GetObservationBySyncID(dependent.SyncID)
	if err != nil || replicaDependent.StaleAt == nil || replicaDependent.StaleReason == nil || !strings.HasPrefix(*replicaDependent.StaleReason, "resource:") {
		t.Fatalf("replica dependent=%+v err=%v", replicaDependent, err)
	}
}

func TestLifecycleMultipleMemoryAndResourceInvalidations(t *testing.T) {
	for _, order := range []string{"memory-first", "resources-first"} {
		t.Run(order, func(t *testing.T) {
			s := newCandidateTestStore(t)
			ids := dependencyChain(t, s, 1)
			a, err := s.AddResourceDependency(AddResourceDependencyParams{SourceRef: testResourceRefA, DependentObservationID: ids[1], Project: "candidate-project"})
			if err != nil {
				t.Fatal(err)
			}
			refC := "file:///tmp/independent-source@sha256:" + strings.Repeat("c", 64)
			refD := "file:///tmp/independent-source@sha256:" + strings.Repeat("d", 64)
			b, err := s.AddResourceDependency(AddResourceDependencyParams{SourceRef: refC, DependentObservationID: ids[1], Project: "candidate-project"})
			if err != nil {
				t.Fatal(err)
			}
			invalidateMemory := func() {
				content := "Updated memory provenance"
				if _, err := s.UpdateObservation(ids[0], UpdateObservationParams{Content: &content}); err != nil {
					t.Fatal(err)
				}
			}
			if order == "memory-first" {
				invalidateMemory()
			}
			for _, p := range []NotifyResourceChangeParams{{Project: "candidate-project", SourceRef: testResourceRefA, ReplacementSourceRef: testResourceRefB}, {Project: "candidate-project", SourceRef: refC, ReplacementSourceRef: refD}} {
				first, err := s.NotifyResourceChanged(p)
				if err != nil || first.Staled != 1 {
					t.Fatalf("notify: %+v %v", first, err)
				}
				replay, err := s.NotifyResourceChanged(p)
				if err != nil || replay.Staled != 0 {
					t.Fatalf("duplicate notify: %+v %v", replay, err)
				}
			}
			if order == "resources-first" {
				invalidateMemory()
			}
			content, abstract, overview := "Recomputed content from verified memory", "Reviewed abstract", "Reviewed overview"
			if order == "memory-first" {
				if _, err := s.UpdateObservation(ids[1], UpdateObservationParams{Content: &content, Abstract: &abstract, Overview: &overview}); err != nil {
					t.Fatal(err)
				}
			}
			for _, dependency := range []*ResourceDependency{a, b} {
				if _, err := s.RefreshResourceDependency(RefreshResourceDependencyParams{SyncID: dependency.SyncID, Project: "candidate-project"}); err != nil {
					t.Fatal(err)
				}
				obs, err := s.GetObservation(ids[1])
				if err != nil {
					t.Fatal(err)
				}
				if dependency == a || order == "resources-first" {
					if obs.StaleAt == nil {
						t.Fatal("one resolved cause cleared another invalidation")
					}
				}
			}
			if order == "resources-first" {
				if _, err := s.UpdateObservation(ids[1], UpdateObservationParams{Content: &content, Abstract: &abstract, Overview: &overview}); err != nil {
					t.Fatal(err)
				}
			}
			obs, err := s.GetObservation(ids[1])
			if err != nil || obs.StaleAt != nil {
				t.Fatalf("all verified sources remained stale: %+v %v", obs, err)
			}
			if err := s.SetCandidateAdmissionMode("tiered"); err != nil {
				t.Fatal(err)
			}
			admitted := submitLifecycleCandidate(t, s, ids[1], "Normal admission resumes after all independent invalidation causes are resolved")
			if admitted.Promoted != 1 {
				t.Fatalf("normal automatic admission failed: %+v", admitted)
			}
		})
	}
}

func TestLifecycleResourceRefreshPreservesSummaryInvalidity(t *testing.T) {
	s := newCandidateTestStore(t)
	id, err := s.AddObservation(AddObservationParams{SessionID: "candidate-session", Project: "candidate-project", Type: "decision", Title: "Source derived summary", Content: "Original content", Abstract: "Original abstract", Overview: "Original overview"})
	if err != nil {
		t.Fatal(err)
	}
	edge, err := s.AddResourceDependency(AddResourceDependencyParams{SourceRef: testResourceRefA, DependentObservationID: id, Project: "candidate-project"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.NotifyResourceChanged(NotifyResourceChangeParams{Project: "candidate-project", SourceRef: testResourceRefA, ReplacementSourceRef: testResourceRefB}); err != nil {
		t.Fatal(err)
	}
	content := "Reviewed replacement content with obsolete display summaries"
	if _, err := s.UpdateObservation(id, UpdateObservationParams{Content: &content}); err != nil {
		t.Fatal(err)
	}
	original := s.hooks.commit
	s.hooks.commit = func(tx *sql.Tx) error { tx.Rollback(); return errors.New("synthetic resource refresh commit failure") }
	if _, err := s.RefreshResourceDependency(RefreshResourceDependencyParams{SyncID: edge.SyncID, Project: "candidate-project"}); err == nil {
		t.Fatal("refresh failure hidden")
	}
	s.hooks.commit = original
	stored, err := s.ListResourceDependencies(ResourceDependencyListOptions{Project: "candidate-project"})
	if err != nil || len(stored) != 1 || stored[0].Status != DependencyStatusStale || stored[0].SourceRef != testResourceRefA {
		t.Fatalf("failed refresh committed: %+v %v", stored, err)
	}
	if _, err := s.RefreshResourceDependency(RefreshResourceDependencyParams{SyncID: edge.SyncID, Project: "candidate-project"}); err != nil {
		t.Fatal(err)
	}
	obs, err := s.GetObservation(id)
	if err != nil || obs.StaleAt == nil || obs.SummaryState != SummaryStateStale {
		t.Fatalf("resource refresh repaired an obsolete summary: %+v %v", obs, err)
	}
	abstract, overview := "Reviewed replacement abstract", "Reviewed replacement overview"
	if _, err := s.UpdateObservation(id, UpdateObservationParams{Abstract: &abstract, Overview: &overview}); err != nil {
		t.Fatal(err)
	}
	obs, err = s.GetObservation(id)
	if err != nil || obs.StaleAt != nil || obs.SummaryState != SummaryStateFresh {
		t.Fatalf("verified summary did not recover: %+v %v", obs, err)
	}
}

func TestResourceDependencyExportImportAndProjectMigration(t *testing.T) {
	source := newTestStore(t)
	if err := source.CreateSession("resource-export", "old-project", "/tmp/old-project"); err != nil {
		t.Fatal(err)
	}
	dependentID := addDependencyObservation(t, source, "resource-export", "old-project", "Exported resource memory")
	dependency, err := source.AddResourceDependency(AddResourceDependencyParams{
		SourceRef: testResourceRefA, DependentObservationID: dependentID,
		Project: "old-project", Reason: "portable metadata",
	})
	if err != nil {
		t.Fatal(err)
	}

	exported, err := source.ExportProject("old-project")
	if err != nil {
		t.Fatal(err)
	}
	if len(exported.ResourceDependencies) != 1 || exported.ResourceDependencies[0].SyncID != dependency.SyncID {
		t.Fatalf("resource dependencies=%+v", exported.ResourceDependencies)
	}
	replica := newTestStore(t)
	result, err := replica.Import(exported)
	if err != nil {
		t.Fatal(err)
	}
	if result.ResourceDependenciesImported != 1 {
		t.Fatalf("import result=%+v", result)
	}
	listed, err := replica.ListResourceDependencies(ResourceDependencyListOptions{Project: "old-project"})
	if err != nil || len(listed) != 1 || listed[0].SourceRef != testResourceRefA {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}

	migrated, err := replica.MigrateProject("old-project", "new-project")
	if err != nil {
		t.Fatal(err)
	}
	if migrated.ResourceDependenciesUpdated != 1 {
		t.Fatalf("migrate result=%+v", migrated)
	}
	listed, err = replica.ListResourceDependencies(ResourceDependencyListOptions{Project: "new-project"})
	if err != nil || len(listed) != 1 || listed[0].Project != "new-project" {
		t.Fatalf("migrated dependencies=%+v err=%v", listed, err)
	}
}
