package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ObservationStateSnapshot contains formal knowledge state, not read/hit counters,
// task logs or complete conversations. Local numeric IDs are outside this state.
type ObservationStateSnapshot struct {
	SyncID          string  `json:"sync_id"`
	SessionID       string  `json:"session_id"`
	Type            string  `json:"type"`
	MemoryClass     string  `json:"memory_class"`
	MemoryPath      string  `json:"memory_path"`
	Abstract        string  `json:"abstract"`
	Overview        string  `json:"overview"`
	SummaryState    string  `json:"summary_state"`
	SummaryRevision int     `json:"summary_revision"`
	StaleAt         *string `json:"stale_at"`
	StaleReason     *string `json:"stale_reason"`
	Title           string  `json:"title"`
	Content         string  `json:"content"`
	ToolName        *string `json:"tool_name"`
	Project         *string `json:"project"`
	Scope           string  `json:"scope"`
	TopicKey        *string `json:"topic_key"`
	RevisionCount   int     `json:"revision_count"`
	Pinned          bool    `json:"pinned"`
	CreatedAt       string  `json:"created_at"`
	DeletedAt       *string `json:"deleted_at"`
	ArchivedAt      *string `json:"archived_at"`
}

type ObservationVersionSummary struct {
	Sequence        int64  `json:"sequence"`
	SyncID          string `json:"sync_id"`
	ContentRevision int    `json:"content_revision"`
	Kind            string `json:"kind"`
	Title           string `json:"title"`
	StateBytes      int    `json:"state_bytes"`
	CapturedAt      string `json:"captured_at"`
}

type ObservationVersion struct {
	ObservationVersionSummary
	State ObservationStateSnapshot `json:"state"`
}

// Keep this list aligned with the typed state above. UPDATE WHEN compares only
// durable fields: duplicate_count/last_seen_at/review scheduling do not create versions.
var observationVersionColumns = []string{"sync_id", "session_id", "type", "memory_class", "memory_path", "abstract", "overview", "summary_state", "summary_revision", "stale_at", "stale_reason", "title", "content", "tool_name", "project", "scope", "topic_key", "revision_count", "pinned", "created_at", "deleted_at", "archived_at"}

func observationSnapshotSQL(alias string) string {
	pairs := make([]string, 0, len(observationVersionColumns)*2)
	for _, column := range observationVersionColumns {
		value := alias + "." + column
		switch column {
		case "memory_path", "abstract", "overview":
			value = "ifnull(" + value + ",'')"
		case "pinned":
			value = "json(CASE WHEN " + value + "=1 THEN 'true' ELSE 'false' END)"
		}
		pairs = append(pairs, "'"+column+"'", value)
	}
	return "json_object(" + strings.Join(pairs, ",") + ")"
}

func (s *Store) installObservationHistory() error {
	changes := make([]string, 0, len(observationVersionColumns))
	for _, column := range observationVersionColumns {
		changes = append(changes, "OLD."+column+" IS NOT NEW."+column)
	}
	changed := strings.Join(changes, " OR ")
	snapshot := func(alias, kind string) string {
		return "INSERT INTO observation_versions(observation_id,sequence,sync_id,content_revision,kind,state) SELECT " + alias + ".id,coalesce((SELECT max(sequence) FROM observation_versions WHERE observation_id=" + alias + ".id),0)+1," + "ifnull(" + alias + ".sync_id,'')," + alias + ".revision_count,'" + kind + "'," + observationSnapshotSQL(alias)
	}
	ddl := `DROP TRIGGER IF EXISTS observation_version_insert_v1;
 DROP TRIGGER IF EXISTS observation_version_baseline_v1;
 DROP TRIGGER IF EXISTS observation_version_change_v1;
 CREATE TABLE IF NOT EXISTS observation_versions (
 observation_id INTEGER NOT NULL REFERENCES observations(id) ON DELETE CASCADE,
 sequence INTEGER NOT NULL CHECK(sequence>0),
 sync_id TEXT NOT NULL,
 content_revision INTEGER NOT NULL,
 kind TEXT NOT NULL CHECK(kind IN ('created','change','legacy_baseline')),
 state TEXT NOT NULL CHECK(json_valid(state)),
 captured_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
 PRIMARY KEY(observation_id,sequence)
 ) WITHOUT ROWID;
 CREATE INDEX IF NOT EXISTS idx_observation_versions_sync ON observation_versions(sync_id,sequence);
 CREATE TABLE IF NOT EXISTS observation_edit_audits (
 observation_id INTEGER NOT NULL,
 sequence INTEGER NOT NULL,
 expected_snapshot_sha256 TEXT NOT NULL CHECK(length(expected_snapshot_sha256)=64),
 operator_reason TEXT NOT NULL CHECK(length(operator_reason)>0 AND length(operator_reason)<=512),
 created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
 PRIMARY KEY(observation_id,sequence),
 FOREIGN KEY(observation_id,sequence) REFERENCES observation_versions(observation_id,sequence) ON DELETE CASCADE
 ) WITHOUT ROWID;
 CREATE TRIGGER IF NOT EXISTS observation_version_insert_v2 AFTER INSERT ON observations BEGIN ` + snapshot("NEW", "created") + `; END;
 CREATE TRIGGER IF NOT EXISTS observation_version_baseline_v2 BEFORE UPDATE ON observations
 WHEN (` + changed + `) AND NOT EXISTS(SELECT 1 FROM observation_versions WHERE observation_id=OLD.id)
 BEGIN ` + snapshot("OLD", "legacy_baseline") + `; END;
 CREATE TRIGGER IF NOT EXISTS observation_version_change_v2 AFTER UPDATE ON observations
 WHEN ` + changed + ` BEGIN ` + snapshot("NEW", "change") + `; END;`
	return s.withTx(func(tx *sql.Tx) error { _, err := s.execHook(tx, ddl); return err })
}

func historyObservationExists(tx *sql.Tx, id int64, includeDeleted bool) error {
	query := "SELECT id FROM observations WHERE id=?"
	if !includeDeleted {
		query += " AND deleted_at IS NULL"
	}
	var found int64
	if err := tx.QueryRow(query, id).Scan(&found); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrObservationNotFound
		}
		return err
	}
	return nil
}

// ObservationHistory is L0 only, bounded and newest first. beforeSequence=0
// starts at the latest state; a positive cursor is exclusive and monotone.
func (s *Store) ObservationHistory(id int64, includeDeleted bool, limit int, beforeSequence int64) ([]ObservationVersionSummary, error) {
	if limit < 1 {
		limit = 5
	}
	if limit > 20 || beforeSequence < 0 {
		return nil, fmt.Errorf("history limit must be 1..20 and cursor nonnegative")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := historyObservationExists(tx, id, includeDeleted); err != nil {
		return nil, err
	}
	query := `SELECT sequence,sync_id,content_revision,kind,substr(json_extract(state,'$.title'),1,64),length(CAST(state AS BLOB)),captured_at FROM observation_versions WHERE observation_id=?`
	args := []any{id}
	if beforeSequence > 0 {
		query += " AND sequence<?"
		args = append(args, beforeSequence)
	}
	query += " ORDER BY sequence DESC LIMIT ?"
	args = append(args, limit)
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ObservationVersionSummary{}
	for rows.Next() {
		var entry ObservationVersionSummary
		if err := rows.Scan(&entry.Sequence, &entry.SyncID, &entry.ContentRevision, &entry.Kind, &entry.Title, &entry.StateBytes, &entry.CapturedAt); err != nil {
			return nil, err
		}
		result = append(result, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

// ObservationVersion explicitly selects one complete state. History never
// participates in current FTS/search/context/Markdown projection.
func (s *Store) ObservationVersion(id, sequence int64, includeDeleted bool) (ObservationVersion, error) {
	var version ObservationVersion
	if sequence < 1 {
		return version, fmt.Errorf("positive history sequence required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return version, err
	}
	defer tx.Rollback()
	if err := historyObservationExists(tx, id, includeDeleted); err != nil {
		return version, err
	}
	var encoded string
	err = tx.QueryRow(`SELECT sequence,sync_id,content_revision,kind,substr(json_extract(state,'$.title'),1,64),length(CAST(state AS BLOB)),captured_at,state FROM observation_versions WHERE observation_id=? AND sequence=?`, id, sequence).Scan(&version.Sequence, &version.SyncID, &version.ContentRevision, &version.Kind, &version.Title, &version.StateBytes, &version.CapturedAt, &encoded)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return version, ErrObservationVersionNotFound
		}
		return version, err
	}
	if !utf8.ValidString(encoded) {
		return version, fmt.Errorf("stored historical state has invalid UTF-8")
	}
	if err := json.Unmarshal([]byte(encoded), &version.State); err != nil {
		return version, fmt.Errorf("invalid stored observation version")
	}
	if err := tx.Commit(); err != nil {
		return version, err
	}
	return version, nil
}
