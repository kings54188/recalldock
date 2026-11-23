package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ─── Recall tracking (cooldown-aware retrieval ordering) ──────────────────────
//
// Design contract (docs/NEXT-ARCHITECTURE.md §5): cold/warm state must
// influence recall ordering, never fact validity; protected memories
// (pinned, safety constraints) must not sink; a read alone never counts as
// verified reuse, so recorded recalls are ordering signals only and never
// extend any expiry or promote anything.

// RecallStats is the bounded read model for one observation's warm state.
type RecallStats struct {
	SyncID          string  `json:"sync_id"`
	ID              int64   `json:"id"`
	RecallCount     int     `json:"recall_count"`
	LastRecallAt    *string `json:"last_recall_at,omitempty"`
	RecencyBoost    float64 `json:"recency_boost"`
	CooldownPenalty float64 `json:"cooldown_penalty"`
}

// RecordSearchRecalls updates recall counters for the returned observation IDs.
// Failure is non-fatal: recall signals degrade to zero instead of failing the
// search read path.
func (s *Store) RecordSearchRecalls(results []SearchResult) {
	if len(results) == 0 {
		return
	}
	ids := make([]any, 0, len(results))
	for _, r := range results {
		ids = append(ids, r.ID)
	}
	q := `UPDATE observations SET recall_count = recall_count + 1, last_recall_at = datetime('now') WHERE id IN (` + placeholders(len(ids)) + `)`
	if _, err := s.execHook(s.db, q, ids...); err != nil {
		// Ordering signal only; never fail the read path on counter errors.
		_ = err
	}
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	out := make([]byte, 0, 2*n)
	for i := 0; i < n; i++ {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, '?')
	}
	return string(out)
}

// recallOrderWeight combines FTS rank with bounded warm-state signals.
// A recall_count above threshold (stale, unreviewed repetition) demotes an
// observation behind fresher equally-relevant ones, but pinned observations
// and protected classes never receive the demotion. Recency within the
// cooldown window adds a small bounded boost so freshly-used memories stay
// reachable. All values are ordering-only: nothing here changes fact
// validity, review state or expiry.
func recallOrderWeight(rank float64, pinned bool, protectedClass bool, recallCount int, lastRecall *string, now time.Time) float64 {
	w := -rank // FTS rank is negative-better; weight is positive-better
	if pinned || protectedClass {
		// Protected memories keep full relevance; no cooldown demotion.
		if lastRecall != nil {
			w += recencyBoost(lastRecall, now)
		}
		return w
	}
	if recallCount > recallCooldownThreshold {
		w -= cooldownPenalty(recallCount)
	}
	if lastRecall != nil {
		w += recencyBoost(lastRecall, now)
	}
	return w
}

const (
	recallCooldownThreshold  = 8   // recalls beyond this start cooling down
	recallCooldownMaxPenalty = 4.0 // bounded: cooling never buries a result
	recallCooldownStep       = 0.25
	recencyBoostMax          = 0.5
	recencyWindow            = 24 * time.Hour
)

func cooldownPenalty(recallCount int) float64 {
	p := float64(recallCount-recallCooldownThreshold) * recallCooldownStep
	if p > recallCooldownMaxPenalty {
		p = recallCooldownMaxPenalty
	}
	return p
}

func recencyBoost(lastRecall *string, now time.Time) float64 {
	if lastRecall == nil {
		return 0
	}
	t, err := time.Parse("2006-01-02 15:04:05", *lastRecall)
	if err != nil {
		return 0
	}
	age := now.Sub(t)
	if age <= 0 {
		return recencyBoostMax
	}
	if age >= recencyWindow {
		return 0
	}
	return recencyBoostMax * (1 - float64(age)/float64(recencyWindow))
}

// isProtectedMemoryClass marks classes that must not sink under cooldown:
// user preferences/facts and operational (safety/protocol) memories.
func isProtectedMemoryClass(memoryClass string) bool {
	switch memoryClass {
	case MemoryClassUser, "operational":
		return true
	}
	return false
}

// ─── Cooldown archive (operator command surface) ──────────────────────────────

// CooldownArchiveResult reports the outcome of `memory cooldown`:
// cooled observations are soft-archived (kept searchable with
// include_stale/cooldown filters, never physically deleted), pinned and
// protected-class memories are always skipped.
type CooldownArchiveResult struct {
	Examined         int      `json:"examined"`
	Cooled           int      `json:"cooled"`
	SkippedPinned    int      `json:"skipped_pinned"`
	SkippedProtected int      `json:"skipped_protected"`
	CooledSyncIDs    []string `json:"cooled_sync_ids,omitempty"`
}

var errCooldownNoProject = errors.New("cooldown requires an explicit --project (or --all for the operator audit)")

// CooldownArchive demotes rarely-recalled project memories into the cooled
// bucket. It is an explicit operator action: no automatic time-based delete
// exists anywhere else in this codebase and this command does not delete
// either — it only sets review_after markers so cooled items surface for
// review before any future compaction decision.
func (s *Store) CooldownArchive(project string, all bool, minAge time.Duration, dryRun bool) (*CooldownArchiveResult, error) {
	if all {
		project = ""
	} else if p, _ := NormalizeProject(project); p == "" {
		return nil, errCooldownNoProject
	}
	cutoff := time.Now().UTC().Add(-minAge).Format("2006-01-02 15:04:05")
	result := &CooldownArchiveResult{CooledSyncIDs: []string{}}
	// Project scope filter: observations carry a project column; match the
	// canonical project name (and, for compatibility, the raw argument).
	var whereProject string
	var args []any
	args = append(args, recallCooldownThreshold, cutoff)
	if project != "" && !all {
		canonical, _ := NormalizeProject(project)
		whereProject = " AND (project = ? OR project = ?)"
		args = append(args, canonical, project)
	}
	// A row is cooldown-eligible when nothing has recalled it recently AND no
	// future review is already scheduled. The decay pipeline stamps
	// review_after = creation + N months on promoted observations, so the
	// scan must accept rows whose review date is in the past, not only NULL.
	rows, err := s.queryItHook(s.db, `
		SELECT id, sync_id, pinned, memory_class, revision_count, updated_at
		FROM observations
		WHERE deleted_at IS NULL AND archived_at IS NULL
		  AND recall_count < ?
		  AND datetime(updated_at) < datetime(?)
		  AND (review_after IS NULL OR datetime(review_after) < datetime('now'))`+whereProject,
		args...)
	if err != nil {
		return nil, fmt.Errorf("cooldown scan: %w", err)
	}
	type candidate struct {
		id        int64
		syncID    string
		pinned    bool
		class     string
		revision  int
		updatedAt string
	}
	var batch []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.syncID, &c.pinned, &c.class, &c.revision, &c.updatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		batch = append(batch, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	for _, c := range batch {
		result.Examined++
		switch {
		case c.pinned:
			result.SkippedPinned++
		case isProtectedMemoryClass(c.class):
			result.SkippedProtected++
		default:
			if dryRun {
				result.Cooled++
				result.CooledSyncIDs = append(result.CooledSyncIDs, c.syncID)
				continue
			}
			// The scan is advisory. Recheck the current row atomically and bind
			// its revision, even when an edit has the same second timestamp.
			applyArgs := []any{c.id, c.revision, c.updatedAt, recallCooldownThreshold, cutoff}
			applyArgs = append(applyArgs, args[2:]...)
			res, err := s.execHook(s.db,
				`UPDATE observations SET review_after = datetime('now', '+7 days'), updated_at = datetime('now')
                 WHERE id = ? AND revision_count = ? AND updated_at = ?
                   AND deleted_at IS NULL AND archived_at IS NULL AND pinned = 0
                   AND memory_class NOT IN ('user', 'operational')
                   AND recall_count < ? AND datetime(updated_at) < datetime(?)
                   AND (review_after IS NULL OR datetime(review_after) < datetime('now'))`+whereProject,
				applyArgs...)
			if err != nil {
				return nil, fmt.Errorf("cooldown apply %d: %w", c.id, err)
			}
			if n, _ := res.RowsAffected(); n == 0 {
				continue // lost a race: another writer already rescheduled
			}
			result.Cooled++
			result.CooledSyncIDs = append(result.CooledSyncIDs, c.syncID)
		}
	}
	return result, nil
}

var _ = sql.ErrNoRows // keep database/sql import for future tx use
