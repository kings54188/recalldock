package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxResourceSourceRefLength = 2048

var resourceSourceRefHashPattern = regexp.MustCompile(`(?i)@sha256:[0-9a-f]{64}$`)

// ValidateResourceSourceRef accepts only bounded immutable references with a
// URI scheme and a trailing SHA-256 content digest.
func ValidateResourceSourceRef(sourceRef string) error {
	if sourceRef == "" || sourceRef != strings.TrimSpace(sourceRef) || len(sourceRef) > maxResourceSourceRefLength || !utf8.ValidString(sourceRef) {
		return ErrInvalidResourceSourceRef
	}
	for _, r := range sourceRef {
		if unicode.IsControl(r) {
			return ErrInvalidResourceSourceRef
		}
	}
	parsed, err := url.Parse(sourceRef)
	if err != nil || strings.TrimSpace(parsed.Scheme) == "" || !resourceSourceRefHashPattern.MatchString(sourceRef) {
		return ErrInvalidResourceSourceRef
	}
	return nil
}

func normalizeRequiredResourceProject(project string) (string, error) {
	project, _ = NormalizeProject(strings.TrimSpace(project))
	if project == "" {
		return "", ErrResourceDependencyProjectRequired
	}
	return project, nil
}

// AddResourceDependency binds a memory to an immutable external source
// reference. Resource content remains outside Engram.
func (s *Store) AddResourceDependency(p AddResourceDependencyParams) (*ResourceDependency, error) {
	if err := ValidateResourceSourceRef(p.SourceRef); err != nil {
		return nil, err
	}
	project, err := normalizeRequiredResourceProject(p.Project)
	if err != nil {
		return nil, err
	}

	var dependency *ResourceDependency
	err = s.withTx(func(tx *sql.Tx) error {
		dependent, err := s.getObservationTx(tx, p.DependentObservationID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrObservationNotFound
		}
		if err != nil {
			return err
		}
		dependentProject, _ := NormalizeProject(strings.TrimSpace(derefString(dependent.Project)))
		if dependentProject == "" || dependentProject != project {
			return ErrCrossProjectResourceDependency
		}

		var existingSyncID, existingStatus string
		err = tx.QueryRow(`
			SELECT sync_id, status FROM resource_dependencies
			WHERE source_ref = ? AND dependent_observation_sync_id = ? AND project = ?`,
			p.SourceRef, dependent.SyncID, project,
		).Scan(&existingSyncID, &existingStatus)
		if err == nil && existingStatus == DependencyStatusStale {
			return ErrResourceDependencyRequiresRefresh
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		reason := truncate(stripPrivateTags(strings.TrimSpace(p.Reason)), 512)
		if existingSyncID == "" {
			existingSyncID = newSyncID("rdep")
			if _, err := s.execHook(tx, `
				INSERT INTO resource_dependencies (
					sync_id, source_ref, dependent_observation_sync_id,
					project, reason, status, replacement_source_ref, updated_at
				) VALUES (?, ?, ?, ?, ?, ?, NULL, datetime('now'))`,
				existingSyncID, p.SourceRef, dependent.SyncID, project,
				nullableString(reason), DependencyStatusActive,
			); err != nil {
				return err
			}
		} else if _, err := s.execHook(tx, `
			UPDATE resource_dependencies
			SET reason = ?, updated_at = datetime('now')
			WHERE sync_id = ?`, nullableString(reason), existingSyncID); err != nil {
			return err
		}

		dependency, err = s.getResourceDependencyBySyncIDTx(tx, existingSyncID)
		if err != nil {
			return err
		}
		return s.enqueueSyncMutationTx(tx, SyncEntityResourceDependency, dependency.SyncID, SyncOpUpsert, resourceDependencyPayloadFromResourceDependency(dependency))
	})
	if err != nil {
		return nil, err
	}
	return dependency, nil
}

func (s *Store) ResourceIdentityStatus(project, sourceRef string) (string, error) {
	project, err := normalizeRequiredResourceProject(project)
	if err != nil {
		return "", err
	}
	if err := ValidateResourceSourceRef(sourceRef); err != nil {
		return "", err
	}
	hashStart := resourceSourceRefHashPattern.FindStringIndex(sourceRef)[0]
	suffixLength := len(sourceRef) - hashStart
	var registered, matches int
	err = s.db.QueryRow(`
		SELECT count(*), coalesce(max(lower(substr(source_ref, -?)) = ?), 0)
		FROM resource_dependencies
		WHERE project = ? AND substr(source_ref, 1, length(source_ref) - ?) = ?`,
		suffixLength, strings.ToLower(sourceRef[hashStart:]), project, suffixLength, sourceRef[:hashStart],
	).Scan(&registered, &matches)
	if err != nil {
		return "", err
	}
	if registered == 0 {
		return "unregistered", nil
	}
	if matches != 0 {
		return "unchanged", nil
	}
	return "changed", nil
}

func (s *Store) ListResourceDependencies(opts ResourceDependencyListOptions) ([]ResourceDependency, error) {
	project, _ := NormalizeProject(strings.TrimSpace(opts.Project))
	if opts.SourceRef != "" {
		if err := ValidateResourceSourceRef(opts.SourceRef); err != nil {
			return nil, err
		}
	}
	query := `
		SELECT d.id, d.sync_id, d.source_ref,
		       ifnull(dependent.id, 0), d.dependent_observation_sync_id,
		       d.project, ifnull(d.reason, ''), d.status,
		       d.replacement_source_ref, d.created_at, d.updated_at
		FROM resource_dependencies d
		LEFT JOIN observations dependent ON dependent.sync_id = d.dependent_observation_sync_id
		WHERE 1 = 1`
	args := make([]any, 0, 5)
	if project != "" {
		query += ` AND d.project = ?`
		args = append(args, project)
	}
	if opts.SourceRef != "" {
		query += ` AND d.source_ref = ?`
		args = append(args, opts.SourceRef)
	}
	if value := strings.TrimSpace(opts.DependentObservationSyncID); value != "" {
		query += ` AND d.dependent_observation_sync_id = ?`
		args = append(args, value)
	}
	if value := strings.TrimSpace(opts.Status); value != "" {
		if value != DependencyStatusActive && value != DependencyStatusStale {
			return nil, fmt.Errorf("invalid resource dependency status %q", value)
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
	dependencies := []ResourceDependency{}
	for rows.Next() {
		var dependency ResourceDependency
		if err := scanResourceDependency(rows, &dependency); err != nil {
			return nil, err
		}
		dependencies = append(dependencies, dependency)
	}
	return dependencies, rows.Err()
}

// NotifyResourceChanged marks memories bound to source_ref stale. Replaying the
// same event is a no-op because only active bindings are selected.
func (s *Store) NotifyResourceChanged(p NotifyResourceChangeParams) (*ResourceChangeResult, error) {
	if err := ValidateResourceSourceRef(p.SourceRef); err != nil {
		return nil, err
	}
	project, err := normalizeRequiredResourceProject(p.Project)
	if err != nil {
		return nil, err
	}
	replacement := strings.TrimSpace(p.ReplacementSourceRef)
	if replacement != "" {
		if err := ValidateResourceSourceRef(replacement); err != nil {
			return nil, err
		}
		if replacement == p.SourceRef {
			return nil, ErrInvalidResourceSourceRef
		}
	}

	result := &ResourceChangeResult{}
	err = s.withTx(func(tx *sql.Tx) error {
		rows, err := s.queryItHook(tx, `
			SELECT sync_id FROM resource_dependencies
			WHERE project = ? AND source_ref = ? AND status = ?
			ORDER BY id`, project, p.SourceRef, DependencyStatusActive)
		if err != nil {
			return err
		}
		var syncIDs []string
		for rows.Next() {
			var syncID string
			if err := rows.Scan(&syncID); err != nil {
				return closeRowsWithError(rows, err)
			}
			syncIDs = append(syncIDs, syncID)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		result.Matched = len(syncIDs)
		reason := truncate(stripPrivateTags(strings.TrimSpace(p.Reason)), 512)

		for _, syncID := range syncIDs {
			res, err := s.execHook(tx, `
				UPDATE resource_dependencies
				SET status = ?, replacement_source_ref = ?,
				    reason = CASE WHEN ? = '' THEN reason ELSE ? END,
				    updated_at = datetime('now')
				WHERE sync_id = ? AND status = ?`,
				DependencyStatusStale, nullableString(replacement), reason, reason,
				syncID, DependencyStatusActive)
			if err != nil {
				return err
			}
			changed, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if changed == 0 {
				continue
			}
			result.Staled++
			dependency, err := s.getResourceDependencyBySyncIDTx(tx, syncID)
			if err != nil {
				return err
			}
			if err := s.enqueueSyncMutationTx(tx, SyncEntityResourceDependency, dependency.SyncID, SyncOpUpsert, resourceDependencyPayloadFromResourceDependency(dependency)); err != nil {
				return err
			}

			dependent, err := s.getObservationBySyncIDTx(tx, dependency.DependentObservationSyncID, false)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			observationChanged, err := s.markObservationResourceStaleTx(tx, dependent.SyncID, p.SourceRef)
			if err != nil {
				return err
			}
			if observationChanged {
				dependent, err = s.getObservationBySyncIDTx(tx, dependent.SyncID, false)
				if err != nil {
					return err
				}
				if err := s.enqueueSyncMutationTx(tx, SyncEntityObservation, dependent.SyncID, SyncOpUpsert, observationPayloadFromObservation(dependent)); err != nil {
					return err
				}
			}
			if err := s.invalidateDependentsTx(tx, dependent.SyncID, dependent.SyncID, dependent.RevisionCount, "resource_source_changed:"+truncate(p.SourceRef, 160), true); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// RefreshResourceDependency explicitly rebinds a stale dependency. If no
// source_ref is supplied, the replacement from the change event is used.
func (s *Store) RefreshResourceDependency(p RefreshResourceDependencyParams) (*ResourceDependency, error) {
	project, err := normalizeRequiredResourceProject(p.Project)
	if err != nil {
		return nil, err
	}
	syncID := strings.TrimSpace(p.SyncID)
	if syncID == "" {
		return nil, ErrResourceDependencyNotFound
	}

	var refreshed *ResourceDependency
	err = s.withTx(func(tx *sql.Tx) error {
		dependency, err := s.getResourceDependencyBySyncIDTx(tx, syncID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrResourceDependencyNotFound
		}
		if err != nil {
			return err
		}
		if dependency.Project != project {
			return ErrCrossProjectResourceDependency
		}
		sourceRef := strings.TrimSpace(p.SourceRef)
		if sourceRef == "" && dependency.ReplacementSourceRef != nil {
			sourceRef = *dependency.ReplacementSourceRef
		}
		if err := ValidateResourceSourceRef(sourceRef); err != nil {
			return err
		}
		reason := truncate(stripPrivateTags(strings.TrimSpace(p.Reason)), 512)
		if _, err := s.execHook(tx, `
			UPDATE resource_dependencies
			SET source_ref = ?, status = ?, replacement_source_ref = NULL,
			    reason = CASE WHEN ? = '' THEN reason ELSE ? END,
			    updated_at = datetime('now')
			WHERE sync_id = ? AND project = ?`,
			sourceRef, DependencyStatusActive, reason, reason, syncID, project); err != nil {
			return err
		}
		refreshed, err = s.getResourceDependencyBySyncIDTx(tx, syncID)
		if err != nil {
			return err
		}
		if err := s.enqueueSyncMutationTx(tx, SyncEntityResourceDependency, refreshed.SyncID, SyncOpUpsert, resourceDependencyPayloadFromResourceDependency(refreshed)); err != nil {
			return err
		}
		return s.clearResolvedResourceStaleTx(tx, refreshed.DependentObservationSyncID, true)
	})
	if err != nil {
		return nil, err
	}
	return refreshed, nil
}

func (s *Store) markObservationResourceStaleTx(tx *sql.Tx, dependentSyncID, sourceRef string) (bool, error) {
	res, err := s.execHook(tx, `
		UPDATE observations
		SET stale_at = datetime('now'), stale_reason = ?, updated_at = datetime('now')
		WHERE sync_id = ? AND deleted_at IS NULL AND stale_at IS NULL`,
		"resource:"+sourceRef, dependentSyncID)
	if err != nil {
		return false, err
	}
	changed, err := res.RowsAffected()
	return changed > 0, err
}

func (s *Store) clearResolvedResourceStaleTx(tx *sql.Tx, dependentSyncID string, enqueueSync bool) error {
	valid, err := observationSourcesValidTx(tx, dependentSyncID)
	if err != nil || !valid {
		return err
	}
	res, err := s.execHook(tx, `
		UPDATE observations
		SET stale_at = NULL, stale_reason = NULL, updated_at = datetime('now')
		WHERE sync_id = ? AND stale_at IS NOT NULL AND summary_state != 'stale'`, dependentSyncID)
	if err != nil {
		return err
	}
	changed, err := res.RowsAffected()
	if err != nil || changed == 0 || !enqueueSync {
		return err
	}
	observation, err := s.getObservationBySyncIDTx(tx, dependentSyncID, false)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.enqueueSyncMutationTx(tx, SyncEntityObservation, observation.SyncID, SyncOpUpsert, observationPayloadFromObservation(observation))
}

func (s *Store) getResourceDependencyBySyncIDTx(tx *sql.Tx, syncID string) (*ResourceDependency, error) {
	row := tx.QueryRow(`
		SELECT d.id, d.sync_id, d.source_ref,
		       ifnull(dependent.id, 0), d.dependent_observation_sync_id,
		       d.project, ifnull(d.reason, ''), d.status,
		       d.replacement_source_ref, d.created_at, d.updated_at
		FROM resource_dependencies d
		LEFT JOIN observations dependent ON dependent.sync_id = d.dependent_observation_sync_id
		WHERE d.sync_id = ?`, syncID)
	var dependency ResourceDependency
	if err := scanResourceDependency(row, &dependency); err != nil {
		return nil, err
	}
	return &dependency, nil
}

func scanResourceDependency(scanner observationScanner, dependency *ResourceDependency) error {
	return scanner.Scan(
		&dependency.ID, &dependency.SyncID, &dependency.SourceRef,
		&dependency.DependentObservationID, &dependency.DependentObservationSyncID,
		&dependency.Project, &dependency.Reason, &dependency.Status,
		&dependency.ReplacementSourceRef, &dependency.CreatedAt, &dependency.UpdatedAt,
	)
}

func resourceDependencyPayloadFromResourceDependency(dependency *ResourceDependency) syncResourceDependencyPayload {
	replacement := ""
	if dependency.ReplacementSourceRef != nil {
		replacement = *dependency.ReplacementSourceRef
	}
	return syncResourceDependencyPayload{
		SyncID: dependency.SyncID, SourceRef: dependency.SourceRef,
		DependentObservationSyncID: dependency.DependentObservationSyncID,
		Project:                    dependency.Project, Reason: dependency.Reason, Status: dependency.Status,
		ReplacementSourceRef: replacement, CreatedAt: dependency.CreatedAt, UpdatedAt: dependency.UpdatedAt,
	}
}

func (s *Store) ExportResourceDependencyMutations(project string) ([]SyncMutation, error) {
	project, _ = NormalizeProject(strings.TrimSpace(project))
	query := `
		SELECT d.sync_id, d.source_ref, d.dependent_observation_sync_id,
		       d.project, ifnull(d.reason, ''), d.status,
		       ifnull(d.replacement_source_ref, ''), d.created_at, d.updated_at
		FROM resource_dependencies d
		JOIN observations dependent ON dependent.sync_id = d.dependent_observation_sync_id AND dependent.deleted_at IS NULL
		WHERE 1 = 1`
	args := []any{}
	if project != "" {
		query += ` AND d.project = ?`
		args = append(args, project)
	}
	query += ` ORDER BY d.created_at, d.sync_id`
	rows, err := s.queryItHook(s.db, query, args...)
	if err != nil {
		return nil, fmt.Errorf("export resource dependency mutations: %w", err)
	}
	defer rows.Close()
	mutations := []SyncMutation{}
	for rows.Next() {
		var payload syncResourceDependencyPayload
		if err := rows.Scan(
			&payload.SyncID, &payload.SourceRef, &payload.DependentObservationSyncID,
			&payload.Project, &payload.Reason, &payload.Status,
			&payload.ReplacementSourceRef, &payload.CreatedAt, &payload.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("export resource dependency mutations: scan: %w", err)
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("export resource dependency mutations: marshal %s: %w", payload.SyncID, err)
		}
		mutations = append(mutations, SyncMutation{
			Entity: SyncEntityResourceDependency, EntityKey: payload.SyncID,
			Op: SyncOpUpsert, Payload: string(encoded), Project: payload.Project, OccurredAt: payload.UpdatedAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("export resource dependency mutations: rows: %w", err)
	}
	return mutations, nil
}

func (s *Store) backfillResourceDependencySyncMutationsTx(tx *sql.Tx, project string, fullSnapshot bool) error {
	rows, err := s.queryItHook(tx, `
		SELECT d.sync_id, d.source_ref, d.dependent_observation_sync_id,
		       d.project, ifnull(d.reason, ''), d.status,
		       ifnull(d.replacement_source_ref, ''), d.created_at, d.updated_at
		FROM resource_dependencies d
		WHERE d.project = ?
		  AND NOT EXISTS (
			SELECT 1 FROM sync_mutations sm
			WHERE sm.target_key = ? AND sm.entity = ?
			  AND sm.entity_key = d.sync_id AND sm.source = ?
			  AND ? = 0
		  )
		ORDER BY d.id`, project, DefaultSyncTargetKey, SyncEntityResourceDependency, SyncSourceLocal, fullSnapshot)
	if err != nil {
		return err
	}
	var pending []syncResourceDependencyPayload
	for rows.Next() {
		var payload syncResourceDependencyPayload
		if err := rows.Scan(
			&payload.SyncID, &payload.SourceRef, &payload.DependentObservationSyncID,
			&payload.Project, &payload.Reason, &payload.Status,
			&payload.ReplacementSourceRef, &payload.CreatedAt, &payload.UpdatedAt,
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
		if err := s.enqueueSyncMutationTx(tx, SyncEntityResourceDependency, payload.SyncID, SyncOpUpsert, payload); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) applyResourceDependencyUpsertTx(tx *sql.Tx, payload syncResourceDependencyPayload) error {
	payload.SyncID = strings.TrimSpace(payload.SyncID)
	payload.DependentObservationSyncID = strings.TrimSpace(payload.DependentObservationSyncID)
	if payload.SyncID == "" || payload.DependentObservationSyncID == "" {
		return fmt.Errorf("resource dependency payload missing required identity")
	}
	if err := ValidateResourceSourceRef(payload.SourceRef); err != nil {
		return err
	}
	project, err := normalizeRequiredResourceProject(payload.Project)
	if err != nil {
		return err
	}
	replacement := strings.TrimSpace(payload.ReplacementSourceRef)
	if replacement != "" {
		if err := ValidateResourceSourceRef(replacement); err != nil {
			return err
		}
		if replacement == payload.SourceRef {
			return ErrInvalidResourceSourceRef
		}
	}
	dependent, err := s.getObservationBySyncIDTx(tx, payload.DependentObservationSyncID, false)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrRelationFKMissing
	}
	if err != nil {
		return err
	}
	dependentProject, _ := NormalizeProject(strings.TrimSpace(derefString(dependent.Project)))
	if dependentProject == "" || dependentProject != project {
		return ErrCrossProjectResourceDependency
	}
	status := strings.TrimSpace(payload.Status)
	if status == "" {
		status = DependencyStatusActive
	}
	if status != DependencyStatusActive && status != DependencyStatusStale {
		return fmt.Errorf("invalid resource dependency status %q", status)
	}
	createdAt := strings.TrimSpace(payload.CreatedAt)
	if createdAt == "" {
		createdAt = Now()
	}
	updatedAt := strings.TrimSpace(payload.UpdatedAt)
	if updatedAt == "" {
		updatedAt = createdAt
	}
	if _, err := s.execHook(tx, `
		INSERT INTO resource_dependencies (
			sync_id, source_ref, dependent_observation_sync_id, project,
			reason, status, replacement_source_ref, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(sync_id) DO UPDATE SET
			source_ref = excluded.source_ref,
			dependent_observation_sync_id = excluded.dependent_observation_sync_id,
			project = excluded.project,
			reason = excluded.reason,
			status = excluded.status,
			replacement_source_ref = excluded.replacement_source_ref,
			updated_at = excluded.updated_at`,
		payload.SyncID, payload.SourceRef, dependent.SyncID, project,
		nullableString(truncate(stripPrivateTags(payload.Reason), 512)), status,
		nullableString(replacement), createdAt, updatedAt,
	); err != nil {
		return err
	}
	if status == DependencyStatusStale {
		if _, err := s.markObservationResourceStaleTx(tx, dependent.SyncID, payload.SourceRef); err != nil {
			return err
		}
		if err := s.invalidateDependentsTx(tx, dependent.SyncID, dependent.SyncID, dependent.RevisionCount, "resource_source_changed:"+truncate(payload.SourceRef, 160), false); err != nil {
			return err
		}
	} else if err := s.clearResolvedResourceStaleTx(tx, dependent.SyncID, false); err != nil {
		return err
	}
	_, err = s.execHook(tx, `DELETE FROM sync_apply_deferred WHERE sync_id = ?`, payload.SyncID)
	return err
}
