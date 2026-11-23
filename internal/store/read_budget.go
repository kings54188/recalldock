package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// DefaultReadBudget matches MaxReadBudget: a single local host's 24h window must
// cover normal tool use (~30-40 bounded reads plus the startup catalog reserve).
// The old 8000 default exhausted after 5-7 reads with no in-window recovery.
const DefaultReadBudget = 32000
const MaxReadBudget = 32000
const ReadBudgetEstimator = "recalldock-response-estimate-v1"

var ErrReadBudget = errors.New("shared read budget unavailable, expired, out of scope or exhausted")

type ReadBudgetState struct {
	ID        string `json:"id"`
	Project   string `json:"project"`
	Limit     int    `json:"limit"`
	Used      int    `json:"used"`
	ExpiresAt string `json:"expires_at"`
	Duplicate bool   `json:"duplicate,omitempty"`
}

func EstimateReadTokens(text string) int { return estimateSearchTokens(text) }

// CreateReadBudget is a host/operator operation, never a model tool argument.
// The reference is accounting state, not proof of a human or an authentication token.
func (s *Store) CreateReadBudget(project string, limit int) (ReadBudgetState, error) {
	if limit == 0 {
		limit = DefaultReadBudget
	}
	if limit < 1 || limit > MaxReadBudget {
		return ReadBudgetState{}, fmt.Errorf("read budget must be 1..%d", MaxReadBudget)
	}
	project, _ = NormalizeProject(project)
	id, err := opaqueID("read-")
	if err != nil {
		return ReadBudgetState{}, err
	}
	state := ReadBudgetState{ID: id, Project: project, Limit: limit, ExpiresAt: candidateTime(time.Now().UTC().Add(24 * time.Hour))}
	err = s.withTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM read_budgets WHERE datetime(expires_at)<=datetime('now')`); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM read_budgets`).Scan(&count); err != nil {
			return err
		}
		if count >= 4096 {
			return errors.New("too many live read activations; inspect host lifecycle")
		}
		_, err := tx.Exec(`INSERT INTO read_budgets(id,project,token_limit,used,expires_at) VALUES(?,?,?,0,?)`, id, project, limit, state.ExpiresAt)
		return err
	})
	return state, err
}

func readBudgetRow(row scannable) (ReadBudgetState, error) {
	var state ReadBudgetState
	err := row.Scan(&state.ID, &state.Project, &state.Limit, &state.Used, &state.ExpiresAt)
	if err != nil {
		return state, ErrReadBudget
	}
	expires, err := time.Parse("2006-01-02 15:04:05", state.ExpiresAt)
	if err != nil || !expires.After(time.Now().UTC()) {
		return state, ErrReadBudget
	}
	return state, nil
}

func (s *Store) ReadBudget(id string) (ReadBudgetState, error) {
	return readBudgetRow(s.db.QueryRow(`SELECT id,project,token_limit,used,expires_at FROM read_budgets WHERE id=?`, id))
}

// ConsumeReadBudget serializes accounting and selected receipt deduplication in
// SQLite so cooperating MCP/HTTP processes cannot independently overspend.
// A failed reservation never records a receipt or consumes the payload budget.
func (s *Store) ConsumeReadBudget(id, project string, cost int, key string) (ReadBudgetState, error) {
	if cost < 1 || len(key) > 256 {
		return ReadBudgetState{}, ErrReadBudget
	}
	var state ReadBudgetState
	err := s.withTx(func(tx *sql.Tx) error {
		// Garbage-collect expired activations on every consume (not only on create)
		// so long-lived hosts cannot accumulate hundreds of dead budget rows.
		if _, err := tx.Exec(`DELETE FROM read_budgets WHERE id<>? AND datetime(expires_at)<=datetime('now')`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE read_budgets SET used=used WHERE id=?`, id); err != nil {
			return err
		}
		var err error
		state, err = readBudgetRow(tx.QueryRow(`SELECT id,project,token_limit,used,expires_at FROM read_budgets WHERE id=?`, id))
		if err != nil {
			return err
		}
		if state.Project != "" && project != state.Project {
			return ErrReadBudget
		}
		if key != "" {
			var exists int
			err := tx.QueryRow(`SELECT 1 FROM read_budget_receipts WHERE budget_id=? AND receipt_key=?`, id, key).Scan(&exists)
			if err == nil {
				state.Duplicate = true
				return nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if cost > state.Limit-state.Used {
			return ErrReadBudget
		}
		if _, err := tx.Exec(`UPDATE read_budgets SET used=used+? WHERE id=?`, cost, id); err != nil {
			return err
		}
		if key != "" {
			if _, err := tx.Exec(`INSERT INTO read_budget_receipts(budget_id,receipt_key) VALUES(?,?)`, id, key); err != nil {
				return err
			}
		}
		state.Used += cost
		return nil
	})
	return state, err
}

const readBudgetScopeMarker = "migration.read_budget_scope.v1"

// purgeUnscopedReadBudgets removes legacy host-wide buckets created before
// project-scoped accounting. A new read creates a bucket for its actual
// project; carrying an empty project bucket forward would silently mix
// unrelated workspaces.
func (s *Store) purgeUnscopedReadBudgets() error {
	tx, err := s.beginTxHook()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var marker string
	err = tx.QueryRow(`SELECT value FROM store_metadata WHERE key=?`, readBudgetScopeMarker).Scan(&marker)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM read_budgets WHERE project=''`); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO store_metadata(key,value) VALUES(?,'done')`, readBudgetScopeMarker); err != nil {
		return err
	}
	return s.commitHook(tx)
}
