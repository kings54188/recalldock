package opsstore

import (
	"database/sql"
	"errors"
	"regexp"
	"strings"
)

var serverNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,79}$`)

// ToolRecord is the discoverable surface of one backend tool. SchemaJSON is
// cached so describe/approve need no live connection; it is never executable
// truth without a matching live schema hash.
type ToolRecord struct {
	Server     string `json:"server"`
	Name       string `json:"name"`
	ConfigHash string `json:"config_hash"`
	SchemaHash string `json:"schema_hash"`
	Summary    string `json:"summary"`
	SchemaJSON string `json:"schema_json,omitempty"`
}

func validToolName(name string) bool {
	if name == "" || len(name) > 200 {
		return false
	}
	for _, r := range name {
		if r <= ' ' || r == '/' || r == '\\' || r > 0x7e {
			return false
		}
	}
	return true
}

func validateToolRecord(t ToolRecord) error {
	if !serverNamePattern.MatchString(t.Server) || !validToolName(t.Name) || len(t.SchemaHash) != 64 || len(t.ConfigHash) != 64 {
		return errors.New("invalid tool record identity")
	}
	if !safeText(t.Summary, 600) || len(t.SchemaJSON) > 64<<10 {
		return errors.New("tool record exceeds bounded metadata")
	}
	return nil
}

func (s *Store) SaveCatalog(server, configHash string, tools []ToolRecord) error {
	if !serverNamePattern.MatchString(server) || len(configHash) != 64 || len(tools) > 1000 {
		return errors.New("invalid catalog payload")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM tool_catalog WHERE server=?", server); err != nil {
		return err
	}
	for _, t := range tools {
		if t.Server != server || t.ConfigHash != configHash {
			return errors.New("catalog entry does not match its server identity")
		}
		if err := validateToolRecord(t); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO tool_catalog(server,name,config_hash,schema_hash,summary,schema_json,updated_at)
			VALUES(?,?,?,?,?,?,datetime('now'))`, t.Server, t.Name, t.ConfigHash, t.SchemaHash, t.Summary, t.SchemaJSON); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) LoadCatalogTool(server, name string) (ToolRecord, error) {
	var tool ToolRecord
	err := s.db.QueryRow(`SELECT server,name,config_hash,schema_hash,summary,schema_json FROM tool_catalog WHERE server=? AND name=?`, server, name).Scan(&tool.Server, &tool.Name, &tool.ConfigHash, &tool.SchemaHash, &tool.Summary, &tool.SchemaJSON)
	return tool, err
}

func (s *Store) LoadCatalog(server string) ([]ToolRecord, error) {
	rows, err := s.db.Query(`SELECT server,name,config_hash,schema_hash,summary,schema_json FROM tool_catalog WHERE server=? ORDER BY name LIMIT 1000`, server)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ToolRecord{}
	for rows.Next() {
		var t ToolRecord
		if err := rows.Scan(&t.Server, &t.Name, &t.ConfigHash, &t.SchemaHash, &t.Summary, &t.SchemaJSON); err != nil {
			return nil, err
		}
		result = append(result, t)
	}
	return result, rows.Err()
}

type ToolSummary struct {
	Server     string `json:"server"`
	Name       string `json:"name"`
	Summary    string `json:"summary"`
	SchemaHash string `json:"schema_hash"`
}

func (s *Store) SearchCatalog(query, after string, limit int, servers []string) ([]ToolSummary, error) {
	if !safeText(query, 200) || len(after) > 300 {
		return nil, errors.New("invalid bounded catalog query")
	}
	afterServer, afterName := "", ""
	if after != "" {
		var ok bool
		afterServer, afterName, ok = strings.Cut(after, "\x00")
		if !ok || !serverNamePattern.MatchString(afterServer) || !validToolName(afterName) {
			return nil, errors.New("invalid catalog cursor")
		}
	}
	if len(servers) == 0 {
		return []ToolSummary{}, nil
	}
	placeholders := make([]string, len(servers))
	args := []any{after, afterServer, afterServer, afterName, query, query}
	for i, server := range servers {
		if !serverNamePattern.MatchString(server) {
			return nil, errors.New("invalid visible catalog server")
		}
		placeholders[i] = "?"
		args = append(args, server)
	}
	args = append(args, pageLimit(limit))
	rows, err := s.db.Query(`SELECT server,name,summary,schema_hash FROM tool_catalog
		WHERE (?='' OR server>? OR (server=? AND name>?))
		  AND (?='' OR instr(lower(server||' '||name||' '||summary),lower(?))>0)
		  AND server IN (`+strings.Join(placeholders, ",")+`)
		ORDER BY server,name LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ToolSummary{}
	for rows.Next() {
		var t ToolSummary
		if err := rows.Scan(&t.Server, &t.Name, &t.Summary, &t.SchemaHash); err != nil {
			return nil, err
		}
		result = append(result, t)
	}
	return result, rows.Err()
}

func (s *Store) RemoveServerCatalog(server string) error {
	if !serverNamePattern.MatchString(server) {
		return errors.New("invalid server name")
	}
	_, err := s.db.Exec("DELETE FROM tool_catalog WHERE server=?", server)
	return err
}

func (s *Store) CatalogServerCount() (int, error) {
	var n int
	err := s.db.QueryRow("SELECT count(DISTINCT server) FROM tool_catalog").Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}
