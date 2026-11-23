package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const defaultDependencyLimit = 200

const maxDependencyVerificationNodes = 256

var ErrDependencySourceInvalid = errors.New("dependency source is not current active knowledge")

// Check only this observation's indexed ancestry; budget exhaustion fails closed.
func validObservationSourcesSQL(alias string) string {
	return `(
		WITH RECURSIVE ancestry(sync_id) AS (
			SELECT ` + alias + `.sync_id
			UNION
			SELECT d.source_observation_sync_id FROM ancestry a
			JOIN memory_dependencies d ON d.dependent_observation_sync_id = a.sync_id
			LIMIT ` + fmt.Sprint(maxDependencyVerificationNodes+1) + `
		)
		SELECT count(*) <= ` + fmt.Sprint(maxDependencyVerificationNodes) + ` AND min(
			NOT EXISTS (
				SELECT 1 FROM memory_dependencies d
				LEFT JOIN observations parent_source ON parent_source.sync_id = d.source_observation_sync_id
				WHERE d.dependent_observation_sync_id = a.sync_id AND (
					d.status != 'active' OR parent_source.id IS NULL
					OR parent_source.deleted_at IS NOT NULL OR parent_source.archived_at IS NOT NULL
					OR parent_source.stale_at IS NOT NULL OR parent_source.summary_state = 'stale'
					OR d.source_revision != parent_source.revision_count
					OR parent_source.project IS NOT ` + alias + `.project OR d.project IS NOT ` + alias + `.project
				)
			)
			AND NOT EXISTS (
				SELECT 1 FROM resource_dependencies d WHERE d.dependent_observation_sync_id = a.sync_id
				AND (d.status != 'active' OR d.project IS NOT ` + alias + `.project)
			)
			AND NOT EXISTS (
				SELECT 1 FROM dependency_invalidation_queue q WHERE q.source_observation_sync_id = a.sync_id
			)
		)
		FROM ancestry a
	)`
}

func observationSourcesValidTx(tx *sql.Tx, syncID string) (bool, error) {
	var valid bool
	err := tx.QueryRow(`SELECT `+validObservationSourcesSQL("o")+` FROM observations o WHERE o.sync_id = ?`, syncID).Scan(&valid)
	return valid, err
}

type dependencyTraversalItem struct {
	sourceSyncID string
	depth        int
}

type dependencyEdge struct {
	dependency      MemoryDependency
	dependentID     int64
	dependentSyncID string
}

// AddMemoryDependency records a directed derived-from edge and binds it to the
// current source revision. Dependency content is never rewritten implicitly.
func (s *Store) AddMemoryDependency(p AddMemoryDependencyParams) (*MemoryDependency, error) {
	if p.SourceObservationID == p.DependentObservationID {
		return nil, ErrDependencySelfReference
	}
	var dependency *MemoryDependency
	err := s.withTx(func(tx *sql.Tx) error {
		source, err := s.getObservationTx(tx, p.SourceObservationID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrObservationNotFound
		}
		if err != nil {
			return err
		}
		dependent, err := s.getObservationTx(tx, p.DependentObservationID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrObservationNotFound
		}
		if err != nil {
			return err
		}
		project := derefString(source.Project)
		if project != derefString(dependent.Project) {
			return ErrCrossProjectDependency
		}
		valid, err := observationSourcesValidTx(tx, source.SyncID)
		if err != nil {
			return err
		}
		if source.State() != ObservationStateActive || source.SummaryState == SummaryStateStale || !valid {
			return ErrDependencySourceInvalid
		}
		cycle, err := s.dependencyWouldCycleTx(tx, source.SyncID, dependent.SyncID)
		if err != nil {
			return err
		}
		if cycle {
			return ErrDependencyCycle
		}

		var syncID string
		err = tx.QueryRow(`
			SELECT sync_id FROM memory_dependencies
			WHERE source_observation_sync_id = ? AND dependent_observation_sync_id = ?`,
			source.SyncID, dependent.SyncID,
		).Scan(&syncID)
		if errors.Is(err, sql.ErrNoRows) {
			syncID = newSyncID("dep")
		} else if err != nil {
			return err
		}
		if _, err := s.execHook(tx, `
			INSERT INTO memory_dependencies (
				sync_id, source_observation_sync_id, dependent_observation_sync_id,
				project, reason, source_revision, status, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, datetime('now'))
			ON CONFLICT(source_observation_sync_id, dependent_observation_sync_id) DO UPDATE SET
				reason = excluded.reason,
				source_revision = excluded.source_revision,
				status = excluded.status,
				updated_at = datetime('now')`,
			syncID, source.SyncID, dependent.SyncID, project,
			nullableString(stripPrivateTags(p.Reason)), source.RevisionCount, DependencyStatusActive,
		); err != nil {
			return err
		}
		dependency, err = s.getMemoryDependencyBySyncIDTx(tx, syncID)
		if err != nil {
			return err
		}
		return s.enqueueSyncMutationTx(tx, SyncEntityDependency, dependency.SyncID, SyncOpUpsert, dependencyPayloadFromMemoryDependency(dependency))
	})
	if err != nil {
		return nil, err
	}
	return dependency, nil
}

func (s *Store) ListMemoryDependencies(opts MemoryDependencyListOptions) ([]MemoryDependency, error) {
	project, _ := NormalizeProject(opts.Project)
	query := `
		SELECT d.id, d.sync_id,
		       ifnull(source.id, 0), d.source_observation_sync_id,
		       ifnull(dependent.id, 0), d.dependent_observation_sync_id,
		       d.project, ifnull(d.reason, ''), d.source_revision, d.status,
		       d.created_at, d.updated_at
		FROM memory_dependencies d
		LEFT JOIN observations source ON source.sync_id = d.source_observation_sync_id
		LEFT JOIN observations dependent ON dependent.sync_id = d.dependent_observation_sync_id
		WHERE 1 = 1`
	args := make([]any, 0, 5)
	if project != "" {
		query += ` AND d.project = ?`
		args = append(args, project)
	}
	if value := strings.TrimSpace(opts.SourceObservationSyncID); value != "" {
		query += ` AND d.source_observation_sync_id = ?`
		args = append(args, value)
	}
	if value := strings.TrimSpace(opts.DependentObservationSyncID); value != "" {
		query += ` AND d.dependent_observation_sync_id = ?`
		args = append(args, value)
	}
	if value := strings.TrimSpace(opts.Status); value != "" {
		if value != DependencyStatusActive && value != DependencyStatusStale {
			return nil, fmt.Errorf("invalid dependency status %q", value)
		}
		query += ` AND d.status = ?`
		args = append(args, value)
	}
	limit := opts.Limit
	if limit <= 0 || limit > defaultDependencyLimit {
		limit = defaultDependencyLimit
	}
	query += ` ORDER BY datetime(d.updated_at) DESC, d.id ASC LIMIT ?`
	args = append(args, limit)

	rows, err := s.queryItHook(s.db, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	dependencies := []MemoryDependency{}
	for rows.Next() {
		var dependency MemoryDependency
		if err := scanMemoryDependency(rows, &dependency); err != nil {
			return nil, err
		}
		dependencies = append(dependencies, dependency)
	}
	return dependencies, rows.Err()
}

func (s *Store) PendingDependencyInvalidationCount() (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT count(*) FROM dependency_invalidation_queue`).Scan(&count)
	return count, err
}

func (s *Store) backfillDependencySyncMutationsTx(tx *sql.Tx, project string, fullSnapshot bool) error {
	rows, err := s.queryItHook(tx, `
		SELECT sync_id, source_observation_sync_id, dependent_observation_sync_id,
		       project, ifnull(reason, ''), source_revision, status, created_at, updated_at
		FROM memory_dependencies d
		WHERE project = ?
		  AND NOT EXISTS (
			SELECT 1 FROM sync_mutations sm
			WHERE sm.target_key = ? AND sm.entity = ?
			  AND sm.entity_key = d.sync_id AND sm.source = ?
			  AND ? = 0
		  )
		ORDER BY d.id`, project, DefaultSyncTargetKey, SyncEntityDependency, SyncSourceLocal, fullSnapshot)
	if err != nil {
		return err
	}
	var pending []syncDependencyPayload
	for rows.Next() {
		var payload syncDependencyPayload
		if err := rows.Scan(
			&payload.SyncID, &payload.SourceObservationSyncID,
			&payload.DependentObservationSyncID, &payload.Project, &payload.Reason,
			&payload.SourceRevision, &payload.Status, &payload.CreatedAt, &payload.UpdatedAt,
		); err != nil {
			return closeRowsWithError(rows, err)
		}
		pending = append(pending, payload)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, payload := range pending {
		if err := s.enqueueSyncMutationTx(tx, SyncEntityDependency, payload.SyncID, SyncOpUpsert, payload); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ProcessDependencyInvalidations(limit int) (*DependencyInvalidationResult, error) {
	if limit <= 0 || limit > defaultDependencyLimit {
		limit = defaultDependencyLimit
	}
	result := &DependencyInvalidationResult{}
	err := s.withTx(func(tx *sql.Tx) error {
		rows, err := s.queryItHook(tx, `
			SELECT id, source_observation_sync_id, root_source_sync_id,
			       root_source_revision, reason
			FROM dependency_invalidation_queue
			ORDER BY id ASC LIMIT ?`, limit)
		if err != nil {
			return err
		}
		type pending struct {
			id, rootRevision                 int64
			sourceSyncID, rootSyncID, reason string
		}
		var queue []pending
		for rows.Next() {
			var item pending
			if err := rows.Scan(&item.id, &item.sourceSyncID, &item.rootSyncID, &item.rootRevision, &item.reason); err != nil {
				return closeRowsWithError(rows, err)
			}
			queue = append(queue, item)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, item := range queue {
			if _, err := s.execHook(tx, `DELETE FROM dependency_invalidation_queue WHERE id = ?`, item.id); err != nil {
				return err
			}
			if err := s.invalidateDependentsTx(tx, item.sourceSyncID, item.rootSyncID, int(item.rootRevision), item.reason, true); err != nil {
				return err
			}
			result.Processed++
		}
		return tx.QueryRow(`SELECT count(*) FROM dependency_invalidation_queue`).Scan(&result.Remaining)
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) dependencyWouldCycleTx(tx *sql.Tx, sourceSyncID, dependentSyncID string) (bool, error) {
	var found int
	err := tx.QueryRow(`
		WITH RECURSIVE descendants(sync_id) AS (
			SELECT dependent_observation_sync_id
			FROM memory_dependencies
			WHERE source_observation_sync_id = ?
			UNION
			SELECT d.dependent_observation_sync_id
			FROM memory_dependencies d
			JOIN descendants p ON p.sync_id = d.source_observation_sync_id
		)
		SELECT EXISTS(SELECT 1 FROM descendants WHERE sync_id = ?)`, dependentSyncID, sourceSyncID).Scan(&found)
	return found != 0, err
}

func (s *Store) invalidateDependentsTx(tx *sql.Tx, sourceSyncID, rootSyncID string, rootRevision int, reason string, enqueueSync bool) error {
	maxDepth := s.cfg.DependencyMaxDepth
	if maxDepth <= 0 {
		maxDepth = 16
	}
	rootSyncID = strings.TrimSpace(rootSyncID)
	if rootSyncID == "" {
		rootSyncID = sourceSyncID
	}
	staleReason := fmt.Sprintf("dependency:%s:revision:%d", rootSyncID, rootRevision)
	if value := strings.TrimSpace(reason); value != "" {
		staleReason += ":" + truncate(value, 160)
	}
	queue := []dependencyTraversalItem{{sourceSyncID: sourceSyncID}}
	visited := map[string]bool{}
	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]
		if visited[item.sourceSyncID] {
			continue
		}
		visited[item.sourceSyncID] = true
		edges, err := s.outgoingDependencyEdgesTx(tx, item.sourceSyncID)
		if err != nil {
			return err
		}
		for _, edge := range edges {
			res, err := s.execHook(tx, `
				UPDATE memory_dependencies
				SET status = ?, updated_at = datetime('now')
				WHERE id = ? AND status != ?`, DependencyStatusStale, edge.dependency.ID, DependencyStatusStale)
			if err != nil {
				return err
			}
			changed, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if changed > 0 && enqueueSync {
				updatedDependency, err := s.getMemoryDependencyBySyncIDTx(tx, edge.dependency.SyncID)
				if err != nil {
					return err
				}
				if err := s.enqueueSyncMutationTx(tx, SyncEntityDependency, updatedDependency.SyncID, SyncOpUpsert, dependencyPayloadFromMemoryDependency(updatedDependency)); err != nil {
					return err
				}
			}

			res, err = s.execHook(tx, `
				UPDATE observations
				SET stale_at = datetime('now'), stale_reason = ?, updated_at = datetime('now')
				WHERE sync_id = ? AND deleted_at IS NULL AND stale_at IS NULL`, staleReason, edge.dependentSyncID)
			if err != nil {
				return err
			}
			observationChanged, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if observationChanged > 0 && enqueueSync {
				observation, err := s.getObservationBySyncIDTx(tx, edge.dependentSyncID, false)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return err
				}
				if observation != nil {
					if err := s.enqueueSyncMutationTx(tx, SyncEntityObservation, observation.SyncID, SyncOpUpsert, observationPayloadFromObservation(observation)); err != nil {
						return err
					}
				}
			}

			hasChildren, err := s.dependencySourceHasChildrenTx(tx, edge.dependentSyncID)
			if err != nil {
				return err
			}
			if !hasChildren {
				continue
			}
			if item.depth+1 < maxDepth {
				queue = append(queue, dependencyTraversalItem{sourceSyncID: edge.dependentSyncID, depth: item.depth + 1})
				continue
			}
			if _, err := s.execHook(tx, `
				INSERT INTO dependency_invalidation_queue (
					source_observation_sync_id, root_source_sync_id, root_source_revision, reason
				) VALUES (?, ?, ?, ?)
				ON CONFLICT(source_observation_sync_id, root_source_sync_id) DO UPDATE SET
					root_source_revision = excluded.root_source_revision,
					reason = excluded.reason`, edge.dependentSyncID, rootSyncID, rootRevision, reason); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) rebindIncomingDependenciesTx(tx *sql.Tx, dependentSyncID string, enqueueSync bool) error {
	rows, err := s.queryItHook(tx, `
		SELECT d.sync_id, source.revision_count
		FROM memory_dependencies d
		JOIN observations source ON source.sync_id = d.source_observation_sync_id
		JOIN observations dependent ON dependent.sync_id = d.dependent_observation_sync_id
		WHERE d.dependent_observation_sync_id = ?
		  AND source.deleted_at IS NULL AND source.archived_at IS NULL AND source.stale_at IS NULL
		  AND source.summary_state != 'stale' AND source.project IS dependent.project
		  AND d.project IS dependent.project AND `+validObservationSourcesSQL("source")+`
		ORDER BY d.id`, dependentSyncID)
	if err != nil {
		return err
	}
	type binding struct {
		syncID   string
		revision int
	}
	var bindings []binding
	for rows.Next() {
		var item binding
		if err := rows.Scan(&item.syncID, &item.revision); err != nil {
			return closeRowsWithError(rows, err)
		}
		bindings = append(bindings, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, item := range bindings {
		if _, err := s.execHook(tx, `
			UPDATE memory_dependencies
			SET source_revision = ?, status = ?, updated_at = datetime('now')
			WHERE sync_id = ?`, item.revision, DependencyStatusActive, item.syncID); err != nil {
			return err
		}
		if enqueueSync {
			dependency, err := s.getMemoryDependencyBySyncIDTx(tx, item.syncID)
			if err != nil {
				return err
			}
			if err := s.enqueueSyncMutationTx(tx, SyncEntityDependency, dependency.SyncID, SyncOpUpsert, dependencyPayloadFromMemoryDependency(dependency)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) outgoingDependencyEdgesTx(tx *sql.Tx, sourceSyncID string) ([]dependencyEdge, error) {
	rows, err := s.queryItHook(tx, `
		SELECT d.id, d.sync_id, ifnull(source.id, 0), d.source_observation_sync_id,
		       ifnull(dependent.id, 0), d.dependent_observation_sync_id,
		       d.project, ifnull(d.reason, ''), d.source_revision, d.status,
		       d.created_at, d.updated_at,
		       ifnull(dependent.id, 0), d.dependent_observation_sync_id
		FROM memory_dependencies d
		LEFT JOIN observations source ON source.sync_id = d.source_observation_sync_id
		LEFT JOIN observations dependent ON dependent.sync_id = d.dependent_observation_sync_id
		WHERE d.source_observation_sync_id = ?
		ORDER BY d.id`, sourceSyncID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var edges []dependencyEdge
	for rows.Next() {
		var edge dependencyEdge
		if err := rows.Scan(
			&edge.dependency.ID, &edge.dependency.SyncID,
			&edge.dependency.SourceObservationID, &edge.dependency.SourceObservationSyncID,
			&edge.dependency.DependentObservationID, &edge.dependency.DependentObservationSyncID,
			&edge.dependency.Project, &edge.dependency.Reason, &edge.dependency.SourceRevision,
			&edge.dependency.Status, &edge.dependency.CreatedAt, &edge.dependency.UpdatedAt,
			&edge.dependentID, &edge.dependentSyncID,
		); err != nil {
			return nil, err
		}
		edges = append(edges, edge)
	}
	return edges, rows.Err()
}

func (s *Store) dependencySourceHasChildrenTx(tx *sql.Tx, sourceSyncID string) (bool, error) {
	var found int
	err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM memory_dependencies WHERE source_observation_sync_id = ?)`, sourceSyncID).Scan(&found)
	return found != 0, err
}

func (s *Store) getMemoryDependencyBySyncIDTx(tx *sql.Tx, syncID string) (*MemoryDependency, error) {
	row := tx.QueryRow(`
		SELECT d.id, d.sync_id,
		       ifnull(source.id, 0), d.source_observation_sync_id,
		       ifnull(dependent.id, 0), d.dependent_observation_sync_id,
		       d.project, ifnull(d.reason, ''), d.source_revision, d.status,
		       d.created_at, d.updated_at
		FROM memory_dependencies d
		LEFT JOIN observations source ON source.sync_id = d.source_observation_sync_id
		LEFT JOIN observations dependent ON dependent.sync_id = d.dependent_observation_sync_id
		WHERE d.sync_id = ?`, syncID)
	var dependency MemoryDependency
	if err := scanMemoryDependency(row, &dependency); err != nil {
		return nil, err
	}
	return &dependency, nil
}

func scanMemoryDependency(scanner observationScanner, dependency *MemoryDependency) error {
	return scanner.Scan(
		&dependency.ID, &dependency.SyncID,
		&dependency.SourceObservationID, &dependency.SourceObservationSyncID,
		&dependency.DependentObservationID, &dependency.DependentObservationSyncID,
		&dependency.Project, &dependency.Reason, &dependency.SourceRevision,
		&dependency.Status, &dependency.CreatedAt, &dependency.UpdatedAt,
	)
}

func dependencyPayloadFromMemoryDependency(dependency *MemoryDependency) syncDependencyPayload {
	return syncDependencyPayload{
		SyncID:                     dependency.SyncID,
		SourceObservationSyncID:    dependency.SourceObservationSyncID,
		DependentObservationSyncID: dependency.DependentObservationSyncID,
		Project:                    dependency.Project,
		Reason:                     dependency.Reason,
		SourceRevision:             dependency.SourceRevision,
		Status:                     dependency.Status,
		CreatedAt:                  dependency.CreatedAt,
		UpdatedAt:                  dependency.UpdatedAt,
	}
}

// ExportDependencyMutations returns portable dependency upserts whose source
// and dependent observations are both present locally.
func (s *Store) ExportDependencyMutations(project string) ([]SyncMutation, error) {
	normalizedProject, _ := NormalizeProject(project)
	normalizedProject = strings.TrimSpace(normalizedProject)
	query := `
		SELECT d.sync_id, d.source_observation_sync_id, d.dependent_observation_sync_id,
		       d.project, ifnull(d.reason, ''), d.source_revision, d.status, d.created_at, d.updated_at
		FROM memory_dependencies d
		JOIN observations src ON src.sync_id = d.source_observation_sync_id AND src.deleted_at IS NULL
		JOIN observations dep ON dep.sync_id = d.dependent_observation_sync_id AND dep.deleted_at IS NULL
		WHERE 1 = 1`
	args := []any{}
	if normalizedProject != "" {
		query += ` AND d.project = ?`
		args = append(args, normalizedProject)
	}
	query += ` ORDER BY d.created_at, d.sync_id`
	rows, err := s.queryItHook(s.db, query, args...)
	if err != nil {
		return nil, fmt.Errorf("export dependency mutations: %w", err)
	}
	defer rows.Close()
	mutations := []SyncMutation{}
	for rows.Next() {
		var p syncDependencyPayload
		if err := rows.Scan(&p.SyncID, &p.SourceObservationSyncID, &p.DependentObservationSyncID,
			&p.Project, &p.Reason, &p.SourceRevision, &p.Status, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("export dependency mutations: scan: %w", err)
		}
		payload, err := json.Marshal(p)
		if err != nil {
			return nil, fmt.Errorf("export dependency mutations: marshal %s: %w", p.SyncID, err)
		}
		mutations = append(mutations, SyncMutation{Entity: SyncEntityDependency, EntityKey: p.SyncID, Op: SyncOpUpsert, Payload: string(payload), Project: p.Project, OccurredAt: p.UpdatedAt})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("export dependency mutations: rows: %w", err)
	}
	return mutations, nil
}

func (s *Store) applyDependencyUpsertTx(tx *sql.Tx, payload syncDependencyPayload) error {
	payload.SyncID = strings.TrimSpace(payload.SyncID)
	payload.SourceObservationSyncID = strings.TrimSpace(payload.SourceObservationSyncID)
	payload.DependentObservationSyncID = strings.TrimSpace(payload.DependentObservationSyncID)
	if payload.SyncID == "" || payload.SourceObservationSyncID == "" || payload.DependentObservationSyncID == "" {
		return fmt.Errorf("dependency payload missing required identity")
	}
	if payload.SourceObservationSyncID == payload.DependentObservationSyncID {
		return ErrDependencySelfReference
	}
	source, err := s.getObservationBySyncIDTx(tx, payload.SourceObservationSyncID, false)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrRelationFKMissing
	}
	if err != nil {
		return err
	}
	dependent, err := s.getObservationBySyncIDTx(tx, payload.DependentObservationSyncID, false)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrRelationFKMissing
	}
	if err != nil {
		return err
	}
	project := derefString(source.Project)
	if project != derefString(dependent.Project) {
		return ErrCrossProjectDependency
	}
	payloadProject, _ := NormalizeProject(payload.Project)
	if payloadProject == "" || payloadProject != project {
		return ErrCrossProjectDependency
	}
	cycle, err := s.dependencyWouldCycleTx(tx, source.SyncID, dependent.SyncID)
	if err != nil {
		return err
	}
	if cycle {
		return ErrDependencyCycle
	}
	status := strings.TrimSpace(payload.Status)
	if status == "" {
		status = DependencyStatusActive
	}
	if status != DependencyStatusActive && status != DependencyStatusStale {
		return fmt.Errorf("invalid dependency status %q", status)
	}
	createdAt := strings.TrimSpace(payload.CreatedAt)
	if createdAt == "" {
		createdAt = Now()
	}
	updatedAt := strings.TrimSpace(payload.UpdatedAt)
	if updatedAt == "" {
		updatedAt = createdAt
	}
	if payload.SourceRevision <= 0 {
		payload.SourceRevision = source.RevisionCount
	}
	if _, err := s.execHook(tx, `
		INSERT INTO memory_dependencies (
			sync_id, source_observation_sync_id, dependent_observation_sync_id,
			project, reason, source_revision, status, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(sync_id) DO UPDATE SET
			source_observation_sync_id = excluded.source_observation_sync_id,
			dependent_observation_sync_id = excluded.dependent_observation_sync_id,
			project = excluded.project,
			reason = excluded.reason,
			source_revision = excluded.source_revision,
			status = excluded.status,
			updated_at = excluded.updated_at`,
		payload.SyncID, source.SyncID, dependent.SyncID, project,
		nullableString(stripPrivateTags(payload.Reason)), payload.SourceRevision,
		status, createdAt, updatedAt,
	); err != nil {
		return err
	}
	if status == DependencyStatusStale {
		if err := s.invalidateDependentsTx(tx, source.SyncID, source.SyncID, source.RevisionCount, payload.Reason, false); err != nil {
			return err
		}
	}
	_, err = s.execHook(tx, `DELETE FROM sync_apply_deferred WHERE sync_id = ?`, payload.SyncID)
	return err
}
