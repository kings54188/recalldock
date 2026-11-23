package opsstore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"recalldock.local/recalldock/internal/memorypolicy"
)

var ErrHostSession = errors.New("host session must be registered and active for the selected host and directory identity")

func (s *Store) WithWriter(writer Writer) *Store {
	clone := *s
	clone.ownsDB = false
	clone.writer = nil
	if writer != (Writer{}) {
		clone.writer = &writer
	}
	return &clone
}

func validateHostSessionID(id, host string) error {
	switch host {
	case "claude-code", "codex", "dsh", "antigravity":
	default:
		return fmt.Errorf("invalid host session metadata: %w", ErrHostSession)
	}
	if !strings.HasPrefix(id, host+"-") || len(id) <= len(host)+1 || memorypolicy.ValidateSessionID(id) != nil {
		return fmt.Errorf("host session ID must retain its trusted host prefix: %w", ErrHostSession)
	}
	return nil
}

func (s *Store) RegisterHostSession(id, host, root string) error {
	if err := validateHostSessionID(id, host); err != nil {
		return err
	}
	root, identity, err := canonicalRoot(root)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var storedHost, storedRoot, storedIdentity string
	var ended sql.NullString
	err = tx.QueryRow("SELECT host,root,identity,ended_at FROM host_sessions WHERE id=?", id).Scan(&storedHost, &storedRoot, &storedIdentity, &ended)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.Exec("INSERT INTO host_sessions(id,host,root,identity,started_at) VALUES(?,?,?,?,?)", id, host, root, identity, time.Now().UTC().Format(time.RFC3339Nano))
	} else if err == nil {
		if storedHost != host || storedRoot != root || storedIdentity != identity {
			return ErrHostSession
		}
		if ended.Valid {
			_, err = tx.Exec("UPDATE host_sessions SET started_at=?,ended_at=NULL WHERE id=?", time.Now().UTC().Format(time.RFC3339Nano), id)
		}
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) EndHostSession(id, host, root string) error {
	if err := validateHostSessionID(id, host); err != nil {
		return err
	}
	root, identity, err := canonicalRoot(root)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var storedHost, storedRoot, storedIdentity string
	err = tx.QueryRow("SELECT host,root,identity FROM host_sessions WHERE id=?", id).Scan(&storedHost, &storedRoot, &storedIdentity)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if storedHost != host || storedRoot != root || storedIdentity != identity {
		return ErrHostSession
	}
	if _, err := tx.Exec("UPDATE host_sessions SET ended_at=? WHERE id=? AND ended_at IS NULL", time.Now().UTC().Format(time.RFC3339Nano), id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ValidateHostSession(id, host string) error {
	if err := validateHostSessionID(id, host); err != nil {
		return err
	}
	var storedHost, root, storedIdentity string
	var ended sql.NullString
	err := s.db.QueryRow("SELECT host,root,identity,ended_at FROM host_sessions WHERE id=?", id).Scan(&storedHost, &root, &storedIdentity, &ended)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrHostSession
	}
	if err != nil {
		return err
	}
	if storedHost != host || ended.Valid {
		return ErrHostSession
	}
	canonical, identity, err := canonicalRoot(root)
	if err != nil {
		return err
	}
	if canonical != root || identity != storedIdentity {
		return ErrHostSession
	}
	return nil
}

func (s *Store) validateWriter(q queryer, root, identity string) error {
	if s.writer == nil {
		return nil
	}
	writer := s.writer
	if writer.Host != "" && writer.Host != "claude-code" && writer.Host != "codex" && writer.Host != "dsh" && writer.Host != "antigravity" {
		return errors.New("invalid trusted writer host")
	}
	if writer.GatewaySessionID != "" && (!strings.HasPrefix(writer.GatewaySessionID, "gateway-") || len(writer.GatewaySessionID) <= len("gateway-") || memorypolicy.ValidateSessionID(writer.GatewaySessionID) != nil) {
		return errors.New("invalid trusted gateway writer ID")
	}
	if writer.HostSessionID == "" {
		return nil
	}
	if err := validateHostSessionID(writer.HostSessionID, writer.Host); err != nil {
		return err
	}
	var storedHost, storedRoot, storedIdentity string
	var ended sql.NullString
	err := q.QueryRow("SELECT host,root,identity,ended_at FROM host_sessions WHERE id=?", writer.HostSessionID).Scan(&storedHost, &storedRoot, &storedIdentity, &ended)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrHostSession
	}
	if err != nil {
		return err
	}
	if storedHost != writer.Host || storedRoot != root || storedIdentity != identity || ended.Valid {
		return ErrHostSession
	}
	return nil
}

func (s *Store) checkpointWriter() *Writer {
	if s.writer == nil {
		return nil
	}
	writer := *s.writer
	return &writer
}

func (s *Store) validateCheckpoint(root, lane string, cp Checkpoint) error {
	if err := ValidateCheckpoint(root, lane, cp); err != nil {
		return err
	}
	if s.writer == nil {
		return nil
	}
	return validateDelivery(Snapshot{Root: root, Lane: lane, Checkpoint: cp, Writer: s.checkpointWriter()})
}

func (s *Store) writerSQL(prefix string) string {
	if !s.hasWriterColumn {
		return "NULL"
	}
	if prefix == "" {
		return "writer"
	}
	return prefix + ".writer"
}

func decodeWriter(value sql.NullString) (*Writer, error) {
	if !value.Valid {
		return nil, nil
	}
	var writer *Writer
	err := json.Unmarshal([]byte(value.String), &writer)
	return writer, err
}

func migrateWriterSchema(tx *sql.Tx) error {
	var count int
	if err := tx.QueryRow("SELECT count(*) FROM pragma_table_info('checkpoints') WHERE name='writer'").Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		if _, err := tx.Exec("ALTER TABLE checkpoints ADD COLUMN writer TEXT"); err != nil {
			return err
		}
	}
	_, err := tx.Exec("UPDATE ops_metadata SET value='2' WHERE key='schema_version'")
	return err
}
