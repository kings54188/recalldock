package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

var storedTimestampFields = map[string]bool{
	"created_at": true, "updated_at": true, "started_at": true, "ended_at": true,
	"last_seen_at": true, "review_after": true, "last_recall_at": true,
	"stale_at": true, "deleted_at": true, "archived_at": true, "occurred_at": true,
	"reviewed_at": true, "expires_at": true,
}

func normalizeTimestampJSON(raw []byte) ([]byte, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("expected exactly one JSON value")
	}
	var normalize func(any) error
	normalize = func(value any) error {
		switch node := value.(type) {
		case map[string]any:
			for key, child := range node {
				if storedTimestampFields[key] && child != nil {
					text, ok := child.(string)
					if !ok {
						return fmt.Errorf("%s must be a timestamp string", key)
					}
					if strings.TrimSpace(text) == "" {
						continue
					}
					parsed, err := parseObservationTime(text)
					if err != nil {
						return fmt.Errorf("invalid %s timestamp", key)
					}
					node[key] = parsed.UTC().Format("2006-01-02 15:04:05")
				} else if err := normalize(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range node {
				if err := normalize(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := normalize(value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func normalizeExchangeTimestamps(data *ExportData) (*ExportData, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	raw, err = normalizeTimestampJSON(raw)
	if err != nil {
		return nil, err
	}
	var normalized ExportData
	if err := json.Unmarshal(raw, &normalized); err != nil {
		return nil, err
	}
	return &normalized, nil
}

func (s *Store) normalizeStoredTimestamps() error {
	return s.withTx(func(tx *sql.Tx) error {
		var marker string
		err := tx.QueryRow(`SELECT value FROM store_metadata WHERE key='migration.utc_timestamps.v1'`).Scan(&marker)
		if err == nil {
			return nil
		}
		if err != sql.ErrNoRows {
			return err
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO observation_tombstones(sync_id,session_id,project,deleted_at)
			SELECT sync_id,session_id,project,deleted_at FROM observations WHERE deleted_at IS NOT NULL AND ifnull(sync_id,'')!=''`); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO observation_tombstones(sync_id,session_id,project,deleted_at)
			SELECT entity_key,json_extract(payload,'$.session_id'),project,coalesce(json_extract(payload,'$.deleted_at'),occurred_at)
			FROM sync_mutations WHERE entity='observation' AND op='delete' AND json_valid(payload)`); err != nil {
			return err
		}
		for _, table := range []string{"observations", "sessions", "user_prompts", "memory_relations", "memory_dependencies", "resource_dependencies", "prompt_tombstones", "observation_tombstones", "project_identities"} {
			rows, err := tx.Query("PRAGMA table_info(" + table + ")")
			if err != nil {
				return err
			}
			var columns []string
			for rows.Next() {
				var cid, notNull, primary int
				var name, kind string
				var defaultValue any
				if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primary); err != nil {
					rows.Close()
					return err
				}
				if storedTimestampFields[name] {
					columns = append(columns, name)
				}
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return err
			}
			if err := rows.Close(); err != nil {
				return err
			}
			for _, column := range columns {
				if _, err := tx.Exec("UPDATE " + table + " SET " + column + "=strftime('%Y-%m-%d %H:%M:%S'," + column + ") WHERE " + column + " IS NOT NULL AND strftime('%Y-%m-%d %H:%M:%S'," + column + ") IS NOT NULL AND " + column + "!=strftime('%Y-%m-%d %H:%M:%S'," + column + ")"); err != nil {
					return err
				}
			}
		}
		_, err = tx.Exec(`INSERT INTO store_metadata(key,value) VALUES('migration.utc_timestamps.v1',?)`, time.Now().UTC().Format(time.RFC3339))
		return err
	})
}
