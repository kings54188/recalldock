package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrCandidateEvidenceUnverified = errors.New("candidate evidence is not independently verified; keep pending for operator review")

type verifiedCandidateEvidence struct {
	observations []*Observation
	resources    []string
}

// A current same-project observation snapshot is a verifiable local source.
// It verifies source identity/state, not the truth of a new proposed statement.
// Claimed test/command/user/URL evidence has no trusted receipt and stays manual.
func (s *Store) verifyCandidateEvidenceTx(tx *sql.Tx, c *MemoryCandidate) (*verifiedCandidateEvidence, error) {
	if len(c.EvidenceRefs) == 0 {
		return nil, ErrCandidateEvidenceUnverified
	}
	sources := []*Observation{}
	resources := []string{}
	seen := map[string]bool{}
	for _, ref := range c.EvidenceRefs {
		if ref.Kind == EvidenceKindFile {
			if err := s.verifyCandidateFileEvidenceTx(tx, c.Project, ref); err != nil {
				return nil, err
			}
			if !seen[ref.Ref] {
				seen[ref.Ref] = true
				resources = append(resources, ref.Ref)
			}
			continue
		}
		if ref.Kind != EvidenceKindObservation || len(ref.Digest) != 64 {
			return nil, ErrCandidateEvidenceUnverified
		}
		obs, err := s.getObservationBySyncIDTx(tx, ref.Ref, false)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, ErrCandidateEvidenceUnverified
			}
			return nil, err
		}
		if derefString(obs.Project) != c.Project || obs.State() != "active" || obs.SummaryState == SummaryStateStale {
			return nil, ErrCandidateEvidenceUnverified
		}
		valid, err := observationSourcesValidTx(tx, obs.SyncID)
		if err != nil {
			return nil, err
		}
		if !valid {
			return nil, ErrCandidateEvidenceUnverified
		}
		snapshot, err := selectionTx(tx, obs)
		if err != nil {
			return nil, err
		}
		if snapshot.SnapshotSHA256 != strings.ToLower(ref.Digest) {
			return nil, ErrCandidateEvidenceUnverified
		}
		if !seen[obs.SyncID] {
			seen[obs.SyncID] = true
			sources = append(sources, obs)
		}
	}
	return &verifiedCandidateEvidence{observations: sources, resources: resources}, nil
}

func (s *Store) linkCandidateEvidenceTx(tx *sql.Tx, evidence *verifiedCandidateEvidence, target *Observation) error {
	for _, source := range evidence.observations {
		if source.SyncID == target.SyncID {
			continue
		}
		var existing string
		err := tx.QueryRow(`SELECT sync_id FROM memory_dependencies WHERE source_observation_sync_id=? AND dependent_observation_sync_id=?`, source.SyncID, target.SyncID).Scan(&existing)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		cycle, err := s.dependencyWouldCycleTx(tx, source.SyncID, target.SyncID)
		if err != nil {
			return err
		}
		if cycle {
			return ErrDependencyCycle
		}
		id := newSyncID("dep")
		if _, err := s.execHook(tx, `INSERT INTO memory_dependencies(sync_id,source_observation_sync_id,dependent_observation_sync_id,project,reason,source_revision,status,updated_at) VALUES(?,?,?,?,?,?,'active',datetime('now'))`, id, source.SyncID, target.SyncID, derefString(target.Project), "checked candidate observation evidence", source.RevisionCount); err != nil {
			return err
		}
		dependency, err := s.getMemoryDependencyBySyncIDTx(tx, id)
		if err != nil {
			return err
		}
		if err := s.enqueueSyncMutationTx(tx, SyncEntityDependency, id, SyncOpUpsert, dependencyPayloadFromMemoryDependency(dependency)); err != nil {
			return err
		}
	}
	for _, ref := range evidence.resources {
		var existing string
		err := tx.QueryRow(`SELECT sync_id FROM resource_dependencies WHERE source_ref=? AND dependent_observation_sync_id=? AND project=?`, ref, target.SyncID, derefString(target.Project)).Scan(&existing)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		id := newSyncID("rdep")
		if _, err := s.execHook(tx, `INSERT INTO resource_dependencies(sync_id,source_ref,dependent_observation_sync_id,project,reason,status,updated_at) VALUES(?,?,?,?,?,'active',datetime('now'))`, id, ref, target.SyncID, derefString(target.Project), "checked candidate file snapshot"); err != nil {
			return err
		}
		dep, err := s.getResourceDependencyBySyncIDTx(tx, id)
		if err != nil {
			return err
		}
		if err := s.enqueueSyncMutationTx(tx, SyncEntityResourceDependency, id, SyncOpUpsert, resourceDependencyPayloadFromResourceDependency(dep)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) auditCandidateEvidenceTx(tx *sql.Tx, c *MemoryCandidate, evidence *verifiedCandidateEvidence) error {
	return insertCandidateAuditTx(s, tx, c.SyncID, c.SessionID, c.Project, c.Category, "evidence_checked", fmt.Sprintf("current_local_snapshot_v1: %d observation(s), %d file(s); source identity/state only", len(evidence.observations), len(evidence.resources)), time.Now().UTC())
}
