package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMemoryDependencyPropagationChainDiamondAndReview(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("dependency-graph", "engram", "/tmp/engram"); err != nil {
		t.Fatal(err)
	}
	a := addDependencyObservation(t, s, "dependency-graph", "engram", "Source fact")
	b := addDependencyObservation(t, s, "dependency-graph", "engram", "Derived B")
	c := addDependencyObservation(t, s, "dependency-graph", "engram", "Derived C")
	d := addDependencyObservation(t, s, "dependency-graph", "engram", "Diamond result")

	for _, edge := range [][2]int64{{a, b}, {a, c}, {b, d}, {c, d}} {
		dependency, err := s.AddMemoryDependency(AddMemoryDependencyParams{
			SourceObservationID: edge[0], DependentObservationID: edge[1], Reason: "derived test fact",
		})
		if err != nil {
			t.Fatalf("AddMemoryDependency(%v): %v", edge, err)
		}
		source, err := s.GetObservation(edge[0])
		if err != nil {
			t.Fatal(err)
		}
		if dependency.SourceRevision != source.RevisionCount || dependency.Status != DependencyStatusActive {
			t.Fatalf("dependency=%+v source revision=%d", dependency, source.RevisionCount)
		}
	}

	changed := "The source fact changed and all derived memories need review."
	updatedSource, err := s.UpdateObservation(a, UpdateObservationParams{Content: &changed})
	if err != nil {
		t.Fatalf("UpdateObservation: %v", err)
	}
	for _, id := range []int64{b, c, d} {
		obs, err := s.GetObservation(id)
		if err != nil {
			t.Fatal(err)
		}
		if obs.StaleAt == nil || obs.StaleReason == nil || !strings.Contains(*obs.StaleReason, updatedSource.SyncID) {
			t.Fatalf("observation %d was not marked stale from %s: %+v", id, updatedSource.SyncID, obs)
		}
		if obs.State() != ObservationStateNeedsReview {
			t.Fatalf("observation %d state=%q", id, obs.State())
		}
	}

	review, err := s.ObservationsNeedingReview("engram", 20, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{b, c, d} {
		if !observationsContainID(review, id) {
			t.Fatalf("review list missed stale observation %d: %+v", id, review)
		}
	}

	filtered, err := s.SearchDetailed("diamond result", SearchOptions{
		Project: "engram", Strategy: SearchStrategyFlat, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if searchResponseContains(filtered, d) {
		t.Fatalf("default detailed search returned stale result: %+v", filtered.Results)
	}
	withStale, err := s.SearchDetailed("diamond result", SearchOptions{
		Project: "engram", Strategy: SearchStrategyFlat, Limit: 10, IncludeStale: true,
	})
	if err != nil || !searchResponseContains(withStale, d) {
		t.Fatalf("include_stale result=%+v err=%v", withStale, err)
	}

	dependencies, err := s.ListMemoryDependencies(MemoryDependencyListOptions{Project: "engram"})
	if err != nil {
		t.Fatal(err)
	}
	if len(dependencies) != 4 {
		t.Fatalf("dependencies=%+v", dependencies)
	}
	for _, dependency := range dependencies {
		if dependency.Status != DependencyStatusStale {
			t.Fatalf("dependency did not become stale: %+v", dependency)
		}
	}

	refreshed := "Derived B was recomputed from the changed source."
	if _, err := s.UpdateObservation(b, UpdateObservationParams{Content: &refreshed}); err != nil {
		t.Fatalf("refresh dependent: %v", err)
	}
	refreshedB, err := s.GetObservation(b)
	if err != nil {
		t.Fatal(err)
	}
	if refreshedB.StaleAt != nil || refreshedB.State() != ObservationStateActive {
		t.Fatalf("refreshed dependent remained stale: %+v", refreshedB)
	}
	incoming, err := s.ListMemoryDependencies(MemoryDependencyListOptions{DependentObservationSyncID: refreshedB.SyncID})
	if err != nil || len(incoming) != 1 {
		t.Fatalf("incoming=%+v err=%v", incoming, err)
	}
	if incoming[0].Status != DependencyStatusActive || incoming[0].SourceRevision != updatedSource.RevisionCount {
		t.Fatalf("incoming dependency was not rebound: %+v", incoming[0])
	}
}

func TestMemoryDependencyRejectsSelfCycleAndCrossProject(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("dependency-a", "alpha", "/tmp/alpha"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession("dependency-b", "beta", "/tmp/beta"); err != nil {
		t.Fatal(err)
	}
	a := addDependencyObservation(t, s, "dependency-a", "alpha", "A")
	b := addDependencyObservation(t, s, "dependency-a", "alpha", "B")
	foreign := addDependencyObservation(t, s, "dependency-b", "beta", "Foreign")

	if _, err := s.AddMemoryDependency(AddMemoryDependencyParams{SourceObservationID: a, DependentObservationID: a}); !errors.Is(err, ErrDependencySelfReference) {
		t.Fatalf("self reference err=%v", err)
	}
	if _, err := s.AddMemoryDependency(AddMemoryDependencyParams{SourceObservationID: a, DependentObservationID: foreign}); !errors.Is(err, ErrCrossProjectDependency) {
		t.Fatalf("cross-project err=%v", err)
	}
	if _, err := s.AddMemoryDependency(AddMemoryDependencyParams{SourceObservationID: a, DependentObservationID: b}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddMemoryDependency(AddMemoryDependencyParams{SourceObservationID: b, DependentObservationID: a}); !errors.Is(err, ErrDependencyCycle) {
		t.Fatalf("cycle err=%v", err)
	}
}

func TestMemoryDependencyDeletePropagationAndBoundedQueue(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("dependency-limit", "engram", "/tmp/engram"); err != nil {
		t.Fatal(err)
	}
	root := addDependencyObservation(t, s, "dependency-limit", "engram", "Root")
	child := addDependencyObservation(t, s, "dependency-limit", "engram", "Child")
	grandchild := addDependencyObservation(t, s, "dependency-limit", "engram", "Grandchild")
	if _, err := s.AddMemoryDependency(AddMemoryDependencyParams{SourceObservationID: root, DependentObservationID: child}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddMemoryDependency(AddMemoryDependencyParams{SourceObservationID: child, DependentObservationID: grandchild}); err != nil {
		t.Fatal(err)
	}

	s.cfg.DependencyMaxDepth = 1
	if err := s.DeleteObservation(root, false); err != nil {
		t.Fatalf("DeleteObservation: %v", err)
	}
	childObs, err := s.GetObservation(child)
	if err != nil || childObs.StaleAt == nil {
		t.Fatalf("child=%+v err=%v", childObs, err)
	}
	grandchildObs, err := s.GetObservation(grandchild)
	if err != nil {
		t.Fatal(err)
	}
	if grandchildObs.StaleAt != nil {
		t.Fatalf("grandchild should be queued before drain: %+v", grandchildObs)
	}
	pending, err := s.PendingDependencyInvalidationCount()
	if err != nil || pending == 0 {
		t.Fatalf("pending=%d err=%v", pending, err)
	}

	s.cfg.DependencyMaxDepth = 16
	result, err := s.ProcessDependencyInvalidations(20)
	if err != nil {
		t.Fatalf("ProcessDependencyInvalidations: %v", err)
	}
	if result.Processed == 0 || result.Remaining != 0 {
		t.Fatalf("drain result=%+v", result)
	}
	grandchildObs, err = s.GetObservation(grandchild)
	if err != nil || grandchildObs.StaleAt == nil {
		t.Fatalf("grandchild after drain=%+v err=%v", grandchildObs, err)
	}
}

func TestMemoryDependencySyncReplayIsIdempotent(t *testing.T) {
	sourceStore := newTestStore(t)
	if err := sourceStore.CreateSession("dependency-sync", "engram", "/tmp/engram"); err != nil {
		t.Fatal(err)
	}
	sourceID := addDependencyObservation(t, sourceStore, "dependency-sync", "engram", "Source")
	dependentID := addDependencyObservation(t, sourceStore, "dependency-sync", "engram", "Dependent")
	dependency, err := sourceStore.AddMemoryDependency(AddMemoryDependencyParams{
		SourceObservationID: sourceID, DependentObservationID: dependentID, Reason: "sync provenance",
	})
	if err != nil {
		t.Fatal(err)
	}
	source, _ := sourceStore.GetObservation(sourceID)
	dependent, _ := sourceStore.GetObservation(dependentID)

	replica := newTestStore(t)
	if err := replica.CreateSession("dependency-sync", "engram", "/tmp/engram"); err != nil {
		t.Fatal(err)
	}
	applyObservationPayloadForTest(t, replica, 1, source)
	applyObservationPayloadForTest(t, replica, 2, dependent)
	encoded, err := json.Marshal(dependencyPayloadFromMemoryDependency(dependency))
	if err != nil {
		t.Fatal(err)
	}
	for _, seq := range []int64{3, 4} {
		if err := replica.ApplyPulledMutation(DefaultSyncTargetKey, SyncMutation{
			Seq: seq, Entity: SyncEntityDependency, EntityKey: dependency.SyncID,
			Op: SyncOpUpsert, Payload: string(encoded), Project: "engram",
		}); err != nil {
			t.Fatalf("apply dependency seq=%d: %v", seq, err)
		}
	}
	dependencies, err := replica.ListMemoryDependencies(MemoryDependencyListOptions{Project: "engram"})
	if err != nil || len(dependencies) != 1 {
		t.Fatalf("dependencies=%+v err=%v", dependencies, err)
	}

	changed := "Source changed on the remote replica."
	updated, err := sourceStore.UpdateObservation(sourceID, UpdateObservationParams{Content: &changed})
	if err != nil {
		t.Fatal(err)
	}
	applyObservationPayloadForTest(t, replica, 5, updated)
	replicaDependent, err := replica.GetObservationBySyncID(dependent.SyncID)
	if err != nil || replicaDependent.StaleAt == nil {
		t.Fatalf("replica dependent=%+v err=%v", replicaDependent, err)
	}
}

func TestReplayDeferredDependencyRestoresMutationEnvelope(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("dependency-replay", "alpha", "/tmp/alpha"); err != nil {
		t.Fatal(err)
	}
	sourceID := addDependencyObservation(t, s, "dependency-replay", "alpha", "Source")
	dependentID := addDependencyObservation(t, s, "dependency-replay", "alpha", "Dependent")
	source, err := s.GetObservation(sourceID)
	if err != nil {
		t.Fatal(err)
	}
	dependent, err := s.GetObservation(dependentID)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(syncDependencyPayload{
		SyncID: "dep-replay", SourceObservationSyncID: source.SyncID,
		DependentObservationSyncID: dependent.SyncID, Project: "alpha",
		SourceRevision: source.RevisionCount, Status: DependencyStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	insertDeferredRow(t, s, "dep-replay", SyncEntityDependency, string(payload), 0, "deferred")

	result, err := s.ReplayDeferred()
	if err != nil {
		t.Fatalf("ReplayDeferred: %v", err)
	}
	if result.Succeeded != 1 || result.Dead != 0 || countDeferredRows(t, s, "dep-replay") != 0 {
		t.Fatalf("unexpected replay result=%+v", result)
	}
	dependencies, err := s.ListMemoryDependencies(MemoryDependencyListOptions{Project: "alpha"})
	if err != nil || len(dependencies) != 1 || dependencies[0].SyncID != "dep-replay" {
		t.Fatalf("dependencies=%+v err=%v", dependencies, err)
	}
}

func TestReplayDeferredResourceDependencyRemovesQueueRow(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("resource-replay", "alpha", "/tmp/alpha"); err != nil {
		t.Fatal(err)
	}
	dependentID := addDependencyObservation(t, s, "resource-replay", "alpha", "Dependent")
	dependent, err := s.GetObservation(dependentID)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(syncResourceDependencyPayload{
		SyncID: "rdep-replay", SourceRef: testResourceRefA,
		DependentObservationSyncID: dependent.SyncID, Project: "alpha",
		Status: DependencyStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	insertDeferredRow(t, s, "rdep-replay", SyncEntityResourceDependency, string(payload), 0, "deferred")

	result, err := s.ReplayDeferred()
	if err != nil {
		t.Fatalf("ReplayDeferred: %v", err)
	}
	if result.Succeeded != 1 || result.Dead != 0 || countDeferredRows(t, s, "rdep-replay") != 0 {
		t.Fatalf("unexpected replay result=%+v deferred=%d", result, countDeferredRows(t, s, "rdep-replay"))
	}
	dependencies, err := s.ListResourceDependencies(ResourceDependencyListOptions{Project: "alpha"})
	if err != nil || len(dependencies) != 1 || dependencies[0].SyncID != "rdep-replay" {
		t.Fatalf("dependencies=%+v err=%v", dependencies, err)
	}
}

func TestPulledDependencyMutationsRejectUntrustedEnvelope(t *testing.T) {
	tests := []struct {
		name           string
		entity         string
		entityKey      string
		op             string
		project        string
		payloadID      string
		payloadProject string
	}{
		{name: "dependency project mismatch", entity: SyncEntityDependency, entityKey: "dep-1", op: SyncOpUpsert, project: "beta", payloadID: "dep-1", payloadProject: "alpha"},
		{name: "dependency identity mismatch", entity: SyncEntityDependency, entityKey: "dep-outer", op: SyncOpUpsert, project: "alpha", payloadID: "dep-inner", payloadProject: "alpha"},
		{name: "dependency delete unsupported", entity: SyncEntityDependency, entityKey: "dep-1", op: SyncOpDelete, project: "alpha", payloadID: "dep-1", payloadProject: "alpha"},
		{name: "resource project mismatch", entity: SyncEntityResourceDependency, entityKey: "rdep-1", op: SyncOpUpsert, project: "beta", payloadID: "rdep-1", payloadProject: "alpha"},
		{name: "resource identity mismatch", entity: SyncEntityResourceDependency, entityKey: "rdep-outer", op: SyncOpUpsert, project: "alpha", payloadID: "rdep-inner", payloadProject: "alpha"},
		{name: "resource delete unsupported", entity: SyncEntityResourceDependency, entityKey: "rdep-1", op: SyncOpDelete, project: "alpha", payloadID: "rdep-1", payloadProject: "alpha"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			if err := s.CreateSession("dependency-envelope", "alpha", "/tmp/alpha"); err != nil {
				t.Fatal(err)
			}
			sourceID := addDependencyObservation(t, s, "dependency-envelope", "alpha", "Source")
			dependentID := addDependencyObservation(t, s, "dependency-envelope", "alpha", "Dependent")
			source, err := s.GetObservation(sourceID)
			if err != nil {
				t.Fatal(err)
			}
			dependent, err := s.GetObservation(dependentID)
			if err != nil {
				t.Fatal(err)
			}

			var payload []byte
			if tc.entity == SyncEntityDependency {
				payload, err = json.Marshal(syncDependencyPayload{
					SyncID: tc.payloadID, SourceObservationSyncID: source.SyncID,
					DependentObservationSyncID: dependent.SyncID, Project: tc.payloadProject,
					SourceRevision: source.RevisionCount, Status: DependencyStatusActive,
				})
			} else {
				payload, err = json.Marshal(syncResourceDependencyPayload{
					SyncID: tc.payloadID, SourceRef: testResourceRefA,
					DependentObservationSyncID: dependent.SyncID, Project: tc.payloadProject,
					Status: DependencyStatusActive,
				})
			}
			if err != nil {
				t.Fatal(err)
			}

			mutation := SyncMutation{
				Seq: 1, TargetKey: DefaultSyncTargetKey, Entity: tc.entity,
				EntityKey: tc.entityKey, Op: tc.op, Payload: string(payload),
				Source: SyncSourceRemote, Project: tc.project,
			}
			if err := s.ApplyPulledMutation(DefaultSyncTargetKey, mutation); err != nil {
				t.Fatalf("ApplyPulledMutation should quarantine permanent envelope errors: %v", err)
			}

			table := "memory_dependencies"
			if tc.entity == SyncEntityResourceDependency {
				table = "resource_dependencies"
			}
			var stored int
			if err := s.db.QueryRow("SELECT count(*) FROM "+table+" WHERE sync_id = ?", tc.payloadID).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if stored != 0 {
				t.Fatalf("untrusted mutation created %d rows in %s", stored, table)
			}
			var status string
			if err := s.db.QueryRow(`SELECT apply_status FROM sync_apply_deferred WHERE sync_id = ?`, tc.entityKey).Scan(&status); err != nil {
				t.Fatalf("expected dead quarantine row: %v", err)
			}
			if status != "dead" {
				t.Fatalf("expected dead quarantine, got %q", status)
			}
		})
	}
}

func TestPulledDependencyMutationsQuarantineObservationProjectMismatch(t *testing.T) {
	tests := []struct {
		name   string
		entity string
	}{
		{name: "memory dependency", entity: SyncEntityDependency},
		{name: "resource dependency", entity: SyncEntityResourceDependency},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			if err := s.CreateSession("dependency-project", "alpha", "/tmp/alpha"); err != nil {
				t.Fatal(err)
			}
			sourceID := addDependencyObservation(t, s, "dependency-project", "alpha", "Source")
			dependentID := addDependencyObservation(t, s, "dependency-project", "alpha", "Dependent")
			source, _ := s.GetObservation(sourceID)
			dependent, _ := s.GetObservation(dependentID)
			entityKey := "dep-project"
			var payload []byte
			var err error
			if tc.entity == SyncEntityDependency {
				payload, err = json.Marshal(syncDependencyPayload{
					SyncID: entityKey, SourceObservationSyncID: source.SyncID,
					DependentObservationSyncID: dependent.SyncID, Project: "beta",
					SourceRevision: source.RevisionCount, Status: DependencyStatusActive,
				})
			} else {
				payload, err = json.Marshal(syncResourceDependencyPayload{
					SyncID: entityKey, SourceRef: testResourceRefA,
					DependentObservationSyncID: dependent.SyncID, Project: "beta",
					Status: DependencyStatusActive,
				})
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := s.ApplyPulledMutation(DefaultSyncTargetKey, SyncMutation{
				Seq: 1, Entity: tc.entity, EntityKey: entityKey, Op: SyncOpUpsert,
				Payload: string(payload), Source: SyncSourceRemote, Project: "beta",
			}); err != nil {
				t.Fatalf("permanent cross-project mutation should be quarantined: %v", err)
			}
			var status string
			if err := s.db.QueryRow(`SELECT apply_status FROM sync_apply_deferred WHERE sync_id = ?`, entityKey).Scan(&status); err != nil || status != "dead" {
				t.Fatalf("status=%q err=%v", status, err)
			}
		})
	}
}

func TestLifecycleQueuedInvalidationIsNotTrustworthy(t *testing.T) {
	s := newCandidateTestStore(t)
	depth := s.cfg.DependencyMaxDepth
	if depth <= 0 {
		depth = 16
	}
	ids := dependencyChain(t, s, depth+1)
	changed := "A revised root invalidates the previously derived chain"
	if _, err := s.UpdateObservation(ids[0], UpdateObservationParams{Content: &changed}); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingDependencyInvalidationCount()
	if err != nil || pending == 0 {
		t.Fatalf("no deferred work at depth %d: %d %v", depth, pending, err)
	}
	tail, err := s.GetObservation(ids[len(ids)-1])
	if err != nil {
		t.Fatal(err)
	}
	search, err := s.SearchDetailed("queuedtailmarker", SearchOptions{Project: "candidate-project", Strategy: SearchStrategyFlat, Detail: SearchDetailL0, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetCandidateAdmissionMode("tiered"); err != nil {
		t.Fatal(err)
	}
	admitted := submitLifecycleCandidate(t, s, tail.ID, "Require pending ancestor invalidation to settle before admitting additional derived decisions")
	t.Logf("depth=%d pending=%d tail_state=%s search_has_tail=%v promoted=%d", depth, pending, tail.State(), searchResponseContains(search, tail.ID), admitted.Promoted)
	if searchResponseContains(search, tail.ID) {
		t.Error("default search returned a descendant with known pending ancestor invalidation")
	}
	if admitted.Promoted != 0 {
		t.Error("tiered-v4 trusted a queued ancestor's descendant")
	}
	if _, err := s.ProcessDependencyInvalidations(20); err != nil {
		t.Fatal(err)
	}
	after, err := s.GetObservation(tail.ID)
	if err != nil || after.StaleAt == nil {
		t.Fatalf("explicit queue drain did not invalidate tail: %+v %v", after, err)
	}
}

func TestLifecycleChainMetadataAndVerifiedRecomputation(t *testing.T) {
	s := newCandidateTestStore(t)
	ids := dependencyChain(t, s, 3)
	changed := "Revised root source"
	if _, err := s.UpdateObservation(ids[0], UpdateObservationParams{Content: &changed}); err != nil {
		t.Fatal(err)
	}
	patches := []UpdateObservationParams{}
	topic, path, title, typ, scope := "lifecycle/edited-topic", "project/lifecycle/metadata", "Display title", "learning", "personal"
	abstract, overview := "Display abstract", "Display overview"
	patches = append(patches, UpdateObservationParams{TopicKey: &topic}, UpdateObservationParams{MemoryPath: &path}, UpdateObservationParams{Title: &title}, UpdateObservationParams{Type: &typ}, UpdateObservationParams{Scope: &scope}, UpdateObservationParams{Abstract: &abstract, Overview: &overview})
	for _, patch := range patches {
		before, err := s.SelectObservationForEdit(ids[3])
		if err != nil {
			t.Fatal(err)
		}
		updated, err := s.UpdateObservationCAS(ids[3], before.SnapshotSHA256, patch, "Review display metadata without verifying sources", "")
		if err != nil || updated.State.StaleAt == nil {
			t.Fatalf("metadata cleared stale: %+v %v", updated, err)
		}
	}
	content := "Recompute while an intermediate source is still invalid"
	if _, err := s.UpdateObservation(ids[3], UpdateObservationParams{Content: &content, Abstract: &abstract, Overview: &overview}); err != nil {
		t.Fatal(err)
	}
	last, err := s.GetObservation(ids[3])
	if err != nil || last.StaleAt == nil {
		t.Fatalf("invalid parent allowed rebind: %+v %v", last, err)
	}
	for i := 1; i <= 3; i++ {
		content = fmt.Sprintf("Explicitly recomputed node %d from its current verified source", i)
		if _, err := s.UpdateObservation(ids[i], UpdateObservationParams{Content: &content, Abstract: &abstract, Overview: &overview}); err != nil {
			t.Fatal(err)
		}
		obs, err := s.GetObservation(ids[i])
		if err != nil || obs.StaleAt != nil {
			t.Fatalf("verified recomputation failed: %+v %v", obs, err)
		}
	}
}

func TestLifecycleDependencyBindingRejectsInvalidSources(t *testing.T) {
	for _, kind := range []string{"archived", "deleted", "stale", "invalid-ancestor", "pinned"} {
		t.Run(kind, func(t *testing.T) {
			s := newCandidateTestStore(t)
			ids := dependencyChain(t, s, 2)
			sourceID := ids[0]
			switch kind {
			case "archived":
				flag := true
				if _, err := s.UpdateObservation(sourceID, UpdateObservationParams{Archived: &flag}); err != nil {
					t.Fatal(err)
				}
			case "deleted":
				if err := s.DeleteObservation(sourceID, false); err != nil {
					t.Fatal(err)
				}
			case "stale", "invalid-ancestor":
				if _, err := s.AddResourceDependency(AddResourceDependencyParams{SourceRef: testResourceRefA, DependentObservationID: sourceID, Project: "candidate-project"}); err != nil {
					t.Fatal(err)
				}
				if _, err := s.NotifyResourceChanged(NotifyResourceChangeParams{Project: "candidate-project", SourceRef: testResourceRefA}); err != nil {
					t.Fatal(err)
				}
				if kind == "invalid-ancestor" {
					sourceID = ids[1]
					if _, err := s.db.Exec(`UPDATE observations SET stale_at=NULL,stale_reason=NULL WHERE id=?`, sourceID); err != nil {
						t.Fatal(err)
					}
				}
			case "pinned":
				if err := s.PinObservation(sourceID); err != nil {
					t.Fatal(err)
				}
			}
			target := addDependencyObservation(t, s, "candidate-session", "candidate-project", "New dependent")
			_, err := s.AddMemoryDependency(AddMemoryDependencyParams{SourceObservationID: sourceID, DependentObservationID: target})
			if kind == "pinned" && err != nil {
				t.Fatalf("valid pinned source rejected: %v", err)
			}
			if kind != "pinned" && err == nil {
				t.Fatal("invalid source was bound active")
			}
			if kind != "pinned" {
				content := "Attempt to recompute from an invalid source"
				if _, err := s.UpdateObservation(ids[1], UpdateObservationParams{Content: &content}); err != nil {
					t.Fatal(err)
				}
				obs, err := s.GetObservation(ids[1])
				if err != nil || obs.StaleAt == nil {
					t.Fatalf("invalid source rebind cleared stale: %+v %v", obs, err)
				}
			}
		})
	}
}

func TestLifecycleReadersRejectLegacyActiveAndRetainDiagnostics(t *testing.T) {
	for _, cause := range []string{"memory", "archived", "deleted", "resource", "queued", "revision-mismatch"} {
		t.Run(cause, func(t *testing.T) {
			s := newCandidateTestStore(t)
			edges := 3
			if cause == "queued" {
				edges = 33
			}
			ids := dependencyChain(t, s, edges)
			tailID := ids[len(ids)-1]
			title, topic, path := "queuedtailmarker 来源生命周期 alpha beta", "lifecycle/tail", "project/lifecycle/tail"
			if _, err := s.UpdateObservation(tailID, UpdateObservationParams{Title: &title, TopicKey: &topic, MemoryPath: &path}); err != nil {
				t.Fatal(err)
			}
			switch cause {
			case "archived":
				flag := true
				if _, err := s.UpdateObservation(ids[0], UpdateObservationParams{Archived: &flag}); err != nil {
					t.Fatal(err)
				}
			case "deleted":
				if err := s.DeleteObservation(ids[0], false); err != nil {
					t.Fatal(err)
				}
			case "resource":
				if _, err := s.AddResourceDependency(AddResourceDependencyParams{SourceRef: testResourceRefA, DependentObservationID: ids[0], Project: "candidate-project"}); err != nil {
					t.Fatal(err)
				}
				if _, err := s.NotifyResourceChanged(NotifyResourceChangeParams{Project: "candidate-project", SourceRef: testResourceRefA}); err != nil {
					t.Fatal(err)
				}
			default:
				content := "An invalidating root change"
				if _, err := s.UpdateObservation(ids[0], UpdateObservationParams{Content: &content}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.db.Exec(`UPDATE observations SET stale_at=NULL,stale_reason=NULL WHERE id!=?`, ids[0]); err != nil {
				t.Fatal(err)
			}
			if cause != "memory" {
				if _, err := s.db.Exec(`UPDATE memory_dependencies SET status='active'`); err != nil {
					t.Fatal(err)
				}
			}
			if cause == "queued" || cause == "resource" {
				if _, err := s.db.Exec(`UPDATE memory_dependencies SET source_revision=(SELECT revision_count FROM observations WHERE sync_id=memory_dependencies.source_observation_sync_id)`); err != nil {
					t.Fatal(err)
				}
				if cause == "resource" {
					if _, err := s.db.Exec(`UPDATE observations SET stale_at=NULL,stale_reason=NULL`); err != nil {
						t.Fatal(err)
					}
				}
			}
			other, err := New(s.cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			for _, query := range []string{"queuedtailmarker", "lifecycle/tail", "来源生命周期", "alpha-beta"} {
				for _, strategy := range []string{"", SearchStrategyFlat, SearchStrategyHierarchical, SearchStrategyAuto} {
					found, err := other.SearchDetailed(query, SearchOptions{Project: "candidate-project", Strategy: strategy, Limit: 10})
					if err != nil {
						t.Fatal(err)
					}
					if searchResponseContains(found, tailID) {
						t.Errorf("unsafe result: cause=%s query=%s strategy=%s", cause, query, strategy)
					}
				}
				flat, err := other.SearchPreview(query, SearchOptions{Project: "candidate-project", Limit: 10})
				if err != nil {
					t.Fatal(err)
				}
				for _, r := range flat {
					if r.ID == tailID {
						t.Errorf("unsafe flat preview: %s %s", cause, query)
					}
				}
			}
			context, err := other.HostContext("candidate-project", 4000)
			if err != nil || strings.Contains(context.Text, "queuedtailmarker") {
				t.Fatalf("unsafe host index: %+v %v", context, err)
			}
			historical, err := other.SearchDetailed("queuedtailmarker", SearchOptions{Project: "candidate-project", IncludeStale: true})
			if err != nil || !searchResponseContains(historical, tailID) {
				t.Fatalf("diagnostic search unavailable: %+v %v", historical, err)
			}
			versions, err := other.ObservationHistory(tailID, false, 20, 0)
			if err != nil || len(versions) == 0 {
				t.Fatalf("history unavailable: %+v %v", versions, err)
			}
			if err := other.SetCandidateAdmissionMode("tiered"); err != nil {
				t.Fatal(err)
			}
			admitted := submitLifecycleCandidate(t, other, tailID, "Reject inconsistent legacy sources while retaining explicit historical inspection")
			if admitted.Promoted != 0 || admitted.Review != 1 {
				t.Fatalf("unsafe legacy admission: %+v", admitted)
			}
		})
	}
}

func TestLifecyclePendingMultipleRootsRollbackAndReplay(t *testing.T) {
	s := newCandidateTestStore(t)
	ids := dependencyChain(t, s, 33)
	otherRoot := addDependencyObservation(t, s, "candidate-session", "candidate-project", "Second independent source")
	if _, err := s.AddMemoryDependency(AddMemoryDependencyParams{SourceObservationID: otherRoot, DependentObservationID: ids[1]}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{ids[0], otherRoot} {
		content := "Revised independent source"
		if _, err := s.UpdateObservation(id, UpdateObservationParams{Content: &content}); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := s.PendingDependencyInvalidationCount()
	if err != nil || pending != 2 {
		t.Fatalf("pending=%d %v", pending, err)
	}
	original := s.hooks.commit
	s.hooks.commit = func(tx *sql.Tx) error { tx.Rollback(); return errors.New("synthetic invalidation commit failure") }
	if _, err := s.ProcessDependencyInvalidations(1); err == nil {
		t.Fatal("queue failure hidden")
	}
	s.hooks.commit = original
	after, err := s.PendingDependencyInvalidationCount()
	if err != nil || after != pending {
		t.Fatalf("failed process lost queue: %d %v", after, err)
	}
	for {
		result, err := s.ProcessDependencyInvalidations(1)
		if err != nil {
			t.Fatal(err)
		}
		if result.Remaining == 0 {
			break
		}
	}
	replayed, err := s.ProcessDependencyInvalidations(1)
	if err != nil || replayed.Processed != 0 || replayed.Remaining != 0 {
		t.Fatalf("non-idempotent drain: %+v %v", replayed, err)
	}
	tail, err := s.GetObservation(ids[len(ids)-1])
	if err != nil || tail.StaleAt == nil {
		t.Fatalf("tail not invalidated: %+v %v", tail, err)
	}
}

func TestLifecycleConcurrentAdmissionAndInvalidation(t *testing.T) {
	s := newCandidateTestStore(t)
	ids := dependencyChain(t, s, 17)
	other, err := New(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := s.SetCandidateAdmissionMode("tiered"); err != nil {
		t.Fatal(err)
	}
	tail, err := s.GetObservation(ids[len(ids)-1])
	if err != nil {
		t.Fatal(err)
	}
	selected, err := s.SelectObservationForEdit(tail.ID)
	if err != nil {
		t.Fatal(err)
	}
	root, err := other.SelectObservationForEdit(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	var admitted *CommitMemoryCandidatesResult
	var admissionErr, invalidationErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		admitted, admissionErr = s.CommitMemoryCandidates(CommitMemoryCandidatesParams{SessionID: "candidate-session", Project: "candidate-project", Candidates: []MemoryCandidateInput{{Category: CandidateCategoryDecision, Statement: "Concurrent admission must share an authoritative source snapshot with invalidation", Confidence: 1, EvidenceRefs: []CandidateEvidenceRef{{Kind: EvidenceKindObservation, Ref: tail.SyncID, Digest: selected.SnapshotSHA256}}}}})
	}()
	go func() {
		defer wg.Done()
		<-start
		content := "Concurrent revised root"
		_, invalidationErr = other.UpdateObservationCAS(ids[0], root.SnapshotSHA256, UpdateObservationParams{Content: &content}, "Invalidate the changed source", "")
	}()
	close(start)
	wg.Wait()
	if admissionErr != nil || invalidationErr != nil {
		t.Fatalf("admission=%v invalidation=%v", admissionErr, invalidationErr)
	}
	if admitted.Promoted == 1 {
		candidate, err := s.GetMemoryCandidate(admitted.CandidateIDs[0])
		if err != nil {
			t.Fatal(err)
		}
		obs, err := s.GetObservationBySyncID(candidate.PromotedObservationSyncID)
		if err != nil {
			t.Fatal(err)
		}
		if found, err := s.SearchDetailed("Concurrent admission", SearchOptions{Project: "candidate-project"}); err != nil || searchResponseContains(found, obs.ID) {
			t.Fatalf("concurrently invalidated knowledge visible: %+v %v", found, err)
		}
	}
	if found, err := s.SearchDetailed("queuedtailmarker", SearchOptions{Project: "candidate-project"}); err != nil || searchResponseContains(found, tail.ID) {
		t.Fatalf("concurrent invalidation not observed: %+v %v", found, err)
	}
}

func TestLifecycleVerificationBudgetFailsClosed(t *testing.T) {
	s := newCandidateTestStore(t)
	ids := dependencyChain(t, s, 255)
	if err := s.SetCandidateAdmissionMode("tiered"); err != nil {
		t.Fatal(err)
	}
	atLimit := submitLifecycleCandidate(t, s, ids[len(ids)-2], "Verified bounded deep sources remain eligible for automatic admission")
	if atLimit.Promoted != 1 {
		t.Fatalf("bounded valid source rejected: %+v", atLimit)
	}
	id := addDependencyObservation(t, s, "candidate-session", "candidate-project", "budgettailmarker")
	if _, err := s.AddMemoryDependency(AddMemoryDependencyParams{SourceObservationID: ids[len(ids)-1], DependentObservationID: id}); err != nil {
		t.Fatal(err)
	}
	found, err := s.SearchDetailed("budgettailmarker", SearchOptions{Project: "candidate-project"})
	if err != nil || searchResponseContains(found, id) {
		t.Fatalf("exhausted ancestry budget trusted: %+v %v", found, err)
	}
	history, err := s.SearchDetailed("budgettailmarker", SearchOptions{Project: "candidate-project", IncludeStale: true})
	if err != nil || !searchResponseContains(history, id) {
		t.Fatalf("over-budget diagnostic unavailable: %+v %v", history, err)
	}
	admitted := submitLifecycleCandidate(t, s, id, "Do not infer validity when a bounded source check cannot cover its ancestry")
	if admitted.Promoted != 0 {
		t.Fatalf("over-budget admission: %+v", admitted)
	}
}

func TestLifecycleCASRollbackAndNoopPreserveDependencyState(t *testing.T) {
	s := newCandidateTestStore(t)
	ids := dependencyChain(t, s, 2)
	old, err := s.SelectObservationForEdit(ids[1])
	if err != nil {
		t.Fatal(err)
	}
	content := "Invalidate a source before a selected edit"
	if _, err := s.UpdateObservation(ids[0], UpdateObservationParams{Content: &content}); err != nil {
		t.Fatal(err)
	}
	topic := "lifecycle/cas-label"
	if _, err := s.UpdateObservationCAS(ids[1], old.SnapshotSHA256, UpdateObservationParams{TopicKey: &topic}, "Reject a stale selected snapshot", ""); !errors.Is(err, ErrObservationSnapshotChanged) {
		t.Fatalf("stale CAS accepted: %v", err)
	}
	before, err := s.SelectObservationForEdit(ids[1])
	if err != nil {
		t.Fatal(err)
	}
	history, err := s.ObservationHistory(ids[1], false, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	original := s.hooks.commit
	s.hooks.commit = func(tx *sql.Tx) error { tx.Rollback(); return errors.New("synthetic CAS recomputation failure") }
	content = "Verified recomputation that must roll back with its edge"
	if _, err := s.UpdateObservationCAS(ids[1], before.SnapshotSHA256, UpdateObservationParams{Content: &content}, "Recompute from the current source", ""); err == nil {
		t.Fatal("CAS commit failure hidden")
	}
	s.hooks.commit = original
	after, err := s.SelectObservationForEdit(ids[1])
	if err != nil || after.SnapshotSHA256 != before.SnapshotSHA256 {
		t.Fatalf("failed CAS changed observation: %+v %v", after, err)
	}
	versions, err := s.ObservationHistory(ids[1], false, 20, 0)
	if err != nil || len(versions) != len(history) {
		t.Fatalf("failed CAS changed history: %+v %v", versions, err)
	}
	edges, err := s.ListMemoryDependencies(MemoryDependencyListOptions{DependentObservationSyncID: before.SyncID})
	if err != nil || len(edges) != 1 || edges[0].Status != DependencyStatusStale {
		t.Fatalf("failed CAS rebound edge: %+v %v", edges, err)
	}
	updated, err := s.UpdateObservationCAS(ids[1], before.SnapshotSHA256, UpdateObservationParams{TopicKey: &topic}, "Only edit display metadata", "")
	if err != nil || updated.State.StaleAt == nil {
		t.Fatalf("metadata CAS cleared stale: %+v %v", updated, err)
	}
	replay, err := s.UpdateObservationCAS(ids[1], updated.SnapshotSHA256, UpdateObservationParams{TopicKey: &topic}, "Replay the same metadata patch", "")
	if err != nil || replay.SnapshotSHA256 != updated.SnapshotSHA256 || replay.RevisionCount != updated.RevisionCount {
		t.Fatalf("non-idempotent metadata replay: %+v %v", replay, err)
	}
	versions, err = s.ObservationHistory(ids[1], false, 20, 0)
	if err != nil || len(versions) != len(history)+1 {
		t.Fatalf("metadata changed extra versions: %+v %v", versions, err)
	}
}

func TestLifecycleSyncedMetadataCannotRebindOrClearStale(t *testing.T) {
	s := newCandidateTestStore(t)
	if err := s.CreateSession("dependency-sync", "engram", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	a := addDependencyObservation(t, s, "dependency-sync", "engram", "Root")
	b := addDependencyObservation(t, s, "dependency-sync", "engram", "syncedtailmarker")
	if _, err := s.AddMemoryDependency(AddMemoryDependencyParams{SourceObservationID: a, DependentObservationID: b}); err != nil {
		t.Fatal(err)
	}
	content := "Changed root for a synced metadata replay"
	if _, err := s.UpdateObservation(a, UpdateObservationParams{Content: &content}); err != nil {
		t.Fatal(err)
	}
	obs, err := s.GetObservation(b)
	if err != nil {
		t.Fatal(err)
	}
	payload := *obs
	topic := "lifecycle/synced-label"
	payload.TopicKey = &topic
	payload.Abstract, payload.Overview, payload.SummaryState = "Display abstract", "Display overview", SummaryStateFresh
	payload.RevisionCount++
	payload.StaleAt, payload.StaleReason = nil, nil
	applyObservationPayloadForTest(t, s, 100, &payload)
	applyObservationPayloadForTest(t, s, 101, &payload)
	updated, err := s.GetObservation(b)
	if err != nil || updated.StaleAt == nil {
		t.Fatalf("synced metadata cleared unresolved stale: %+v %v", updated, err)
	}
	edges, err := s.ListMemoryDependencies(MemoryDependencyListOptions{DependentObservationSyncID: obs.SyncID})
	if err != nil || len(edges) != 1 || edges[0].Status != DependencyStatusStale {
		t.Fatalf("synced metadata rebound source: %+v %v", edges, err)
	}
}

func TestLifecycleQueryCost(t *testing.T) {
	for _, mode := range []string{"ordinary", "ordinary-pending", "deep-valid", "deep-pending"} {
		t.Run(mode, func(t *testing.T) {
			s := newCandidateTestStore(t)
			ids := dependencyChain(t, s, 65)
			id := ids[len(ids)-1]
			if strings.HasPrefix(mode, "ordinary") {
				id = addDependencyObservation(t, s, "candidate-session", "candidate-project", "ordinarymarker")
			}
			if strings.HasSuffix(mode, "pending") {
				content := "Revised performance source"
				if _, err := s.UpdateObservation(ids[0], UpdateObservationParams{Content: &content}); err != nil {
					t.Fatal(err)
				}
			}
			unrelated := 200
			if os.Getenv("RECALLDOCK_LIFECYCLE_COST") == "1" {
				unrelated = 10000
			}
			if _, err := s.db.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<?) INSERT INTO observations(sync_id,session_id,type,memory_class,title,content,project,scope,normalized_hash) SELECT 'perf-'||i,'candidate-session','learning','project','unrelated document '||i,'unrelated content '||i,'candidate-project','project','perf-hash-'||i FROM n`, unrelated); err != nil {
				t.Fatal(err)
			}
			query := "queuedtailmarker"
			if strings.HasPrefix(mode, "ordinary") {
				query = "ordinarymarker"
			}
			original := s.hooks.queryIt
			queries, retrievalQueries := 0, 0
			var retrievalSQL string
			var retrievalArgs []any
			s.hooks.queryIt = func(db queryer, query string, args ...any) (rowScanner, error) {
				queries++
				if strings.Contains(query, "FROM observations_fts fts") {
					retrievalQueries++
					retrievalSQL, retrievalArgs = query, args
				}
				return original(db, query, args...)
			}
			started := time.Now()
			for i := 0; i < 100; i++ {
				found, err := s.SearchPreview(query, SearchOptions{Project: "candidate-project", Limit: 5})
				if err != nil {
					t.Fatal(err)
				}
				want := mode != "deep-pending"
				if (len(found) == 1 && found[0].ID == id) != want {
					t.Fatalf("mode=%s results=%+v", mode, found)
				}
			}
			s.hooks.queryIt = original
			elapsed := time.Since(started)
			if queries != 200 || retrievalQueries != 100 {
				t.Fatalf("query count scales with ancestry: %d", queries)
			}
			if err := s.SetCandidateAdmissionMode("tiered"); err != nil {
				t.Fatal(err)
			}
			started = time.Now()
			admitted := submitLifecycleCandidate(t, s, id, "Automatically admit current sources without trusting unresolved propagation")
			admissionElapsed := time.Since(started)
			if (admitted.Promoted == 1) != (mode != "deep-pending") {
				t.Fatalf("unexpected admission: %+v", admitted)
			}
			t.Logf("mode=%s unrelated=%d edges=65 searches=100 retrieval_sql=%d hooked_sql=%d search_mean_us=%.1f admission_us=%d promoted=%d", mode, unrelated, retrievalQueries, queries, float64(elapsed.Microseconds())/100, admissionElapsed.Microseconds(), admitted.Promoted)
			plan, err := s.db.Query(`EXPLAIN QUERY PLAN `+retrievalSQL, retrievalArgs...)
			if err != nil {
				t.Fatal(err)
			}
			ftsFirst := false
			for plan.Next() {
				var id, parent, unused int
				var detail string
				if err := plan.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				t.Log(detail)
				if strings.HasPrefix(detail, "SCAN fts VIRTUAL TABLE") {
					ftsFirst = true
				}
				if strings.HasPrefix(detail, "SEARCH o ") && !ftsFirst {
					t.Errorf("ancestry evaluated before lexical match: %s", detail)
				}
				if strings.HasPrefix(detail, "SCAN observations") || detail == "SCAN parent_source" || detail == "SCAN d" || detail == "SCAN q" {
					t.Errorf("unbounded source scan: %s", detail)
				}
			}
			if err := plan.Err(); err != nil {
				t.Fatal(err)
			}
			plan.Close()
		})
	}
}

func dependencyChain(t *testing.T, s *Store, edges int) []int64 {
	t.Helper()
	ids := make([]int64, 0, edges+1)
	for i := 0; i <= edges; i++ {
		title := fmt.Sprintf("Chain source %d", i)
		if i == edges {
			title = "queuedtailmarker"
		}
		id := addDependencyObservation(t, s, "candidate-session", "candidate-project", title)
		ids = append(ids, id)
		if i > 0 {
			if _, err := s.AddMemoryDependency(AddMemoryDependencyParams{SourceObservationID: ids[i-1], DependentObservationID: id}); err != nil {
				t.Fatal(err)
			}
		}
	}
	return ids
}

func addDependencyObservation(t *testing.T, s *Store, sessionID, project, title string) int64 {
	t.Helper()
	id, err := s.AddObservation(AddObservationParams{
		SessionID: sessionID, Type: "decision", Title: title,
		Content: title + " content", Project: project, Scope: "project",
	})
	if err != nil {
		t.Fatalf("AddObservation(%s): %v", title, err)
	}
	return id
}

func observationsContainID(observations []Observation, id int64) bool {
	for _, observation := range observations {
		if observation.ID == id {
			return true
		}
	}
	return false
}

func applyObservationPayloadForTest(t *testing.T, s *Store, seq int64, observation *Observation) {
	t.Helper()
	encoded, err := json.Marshal(observationPayloadFromObservation(observation))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyPulledMutation(DefaultSyncTargetKey, SyncMutation{
		Seq: seq, Entity: SyncEntityObservation, EntityKey: observation.SyncID,
		Op: SyncOpUpsert, Payload: string(encoded), Project: "engram",
	}); err != nil {
		t.Fatalf("apply observation %s: %v", observation.SyncID, err)
	}
}
