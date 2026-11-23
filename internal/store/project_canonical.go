package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	projectpkg "recalldock.local/recalldock/internal/project"
)

// projectKeyCanonicalizationMarker records the one-time repair that maps legacy
// name-keyed rows onto their already registered project identity. It never
// creates identities or bindings: a deleted project directory is irrelevant,
// because the mapping comes only from project_identities.name.
const projectKeyCanonicalizationMarker = "migration.project_key_canonicalization.v1"

// CanonicalProjectKey resolves an operator/agent supplied project name to the
// single registered project whose display name matches it. It deliberately
// does not consult the filesystem: historical and deleted projects keep their
// identity row, while a name that is missing or ambiguous stays untouched.
func (s *Store) CanonicalProjectKey(project string) (string, error) {
	project, _ = NormalizeProject(project)
	project = strings.TrimSpace(project)
	if project == "" || projectpkg.IsProjectID(project) {
		return project, nil
	}
	if alias, ok, err := s.projectAlias(project); err != nil {
		return "", err
	} else if ok {
		return alias, nil
	}
	rows, err := s.queryItHook(s.db, `SELECT id FROM project_identities WHERE lower(name)=? ORDER BY id`, project)
	if err != nil {
		return "", fmt.Errorf("resolve project name: %w", err)
	}
	defer rows.Close()
	var matches []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		matches = append(matches, id)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	switch len(matches) {
	case 0:
		return project, nil
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("project name %q matches %d registered identities; use the project ID explicitly", project, len(matches))
	}
}

// canonicalProjectOrRaw is for read paths: an ambiguous name keeps its raw key
// instead of failing a lookup the caller can still scope by ID.
func (s *Store) projectAlias(alias string) (string, bool, error) {
	var id string
	err := s.db.QueryRow(`SELECT project_id FROM project_aliases WHERE alias=?`, alias).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return "", false, err
}

func (s *Store) canonicalProjectOrRaw(project string) string {
	canonical, err := s.CanonicalProjectKey(project)
	if err != nil {
		project, _ = NormalizeProject(project)
		return strings.TrimSpace(project)
	}
	return canonical
}

// refreshProjectAliasTx keeps the alias index consistent with the unique
// display name. Duplicate names deliberately have no alias: callers must use
// the project ID, so a guess can never select the wrong project.
func refreshProjectAliasTx(tx *sql.Tx, name, id string) error {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return nil
	}
	var count int
	if err := tx.QueryRow(`SELECT count(*) FROM project_identities WHERE lower(name)=?`, name).Scan(&count); err != nil {
		return err
	}
	if count == 1 {
		if _, err := tx.Exec(`INSERT INTO project_aliases(alias,project_id,source) VALUES(?,?, 'identity-name')
			ON CONFLICT(alias) DO UPDATE SET project_id=excluded.project_id, source=excluded.source`, name, id); err != nil {
			return err
		}
		return nil
	}
	_, err := tx.Exec(`DELETE FROM project_aliases WHERE alias=?`, name)
	return err
}

type canonicalizationCounts map[string]int

func (s *Store) canonicalizeLegacyProjectKeys() error {
	tx, err := s.beginTxHook()
	if err != nil {
		return fmt.Errorf("project key canonicalization: begin: %w", err)
	}
	defer tx.Rollback()

	var marker string
	err = tx.QueryRow(`SELECT value FROM store_metadata WHERE key=?`, projectKeyCanonicalizationMarker).Scan(&marker)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("project key canonicalization: read marker: %w", err)
	}

	rows, err := tx.Query(`SELECT lower(name), id FROM project_identities ORDER BY lower(name), id`)
	if err != nil {
		return fmt.Errorf("project key canonicalization: list identities: %w", err)
	}
	aliases := map[string]string{}
	ambiguous := map[string]bool{}
	for rows.Next() {
		var name, id string
		if err := rows.Scan(&name, &id); err != nil {
			_ = rows.Close()
			return err
		}
		name = strings.TrimSpace(name)
		if name == "" || projectpkg.IsProjectID(name) {
			continue
		}
		if _, seen := aliases[name]; seen {
			ambiguous[name] = true
			delete(aliases, name)
			continue
		}
		if !ambiguous[name] {
			aliases[name] = id
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}

	counts := canonicalizationCounts{}
	for alias, target := range aliases {
		if _, err := tx.Exec(`INSERT INTO project_aliases(alias,project_id,source) VALUES(?,?, 'identity-name')
			ON CONFLICT(alias) DO UPDATE SET project_id=excluded.project_id, source=excluded.source`, alias, target); err != nil {
			return fmt.Errorf("project key canonicalization alias %q: %w", alias, err)
		}
		updated, err := canonicalizeProjectAliasTx(tx, alias, target)
		if err != nil {
			return fmt.Errorf("project key canonicalization %q: %w", alias, err)
		}
		for table, n := range updated {
			counts[table] += n
		}
	}
	body, _ := json.Marshal(counts)
	if _, err := tx.Exec(`INSERT INTO store_metadata (key,value) VALUES (?,?)`, projectKeyCanonicalizationMarker, string(body)); err != nil {
		return fmt.Errorf("project key canonicalization: write marker: %w", err)
	}
	if err := s.commitHook(tx); err != nil {
		return fmt.Errorf("project key canonicalization: commit: %w", err)
	}
	return nil
}

func canonicalizeProjectAliasTx(tx *sql.Tx, alias, target string) (canonicalizationCounts, error) {
	counts := canonicalizationCounts{}
	simple := []string{
		"observations", "sessions", "user_prompts", "read_budgets",
		"memory_candidates", "prompt_tombstones", "memory_dependencies",
	}
	for _, table := range simple {
		res, err := tx.Exec(`UPDATE `+table+` SET project=? WHERE lower(project)=? AND project<>?`, target, alias, target)
		if err != nil {
			return counts, err
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			counts[table] += int(n)
		}
	}

	// Synced project enrollment is keyed by project: merge the alias into the
	// canonical row instead of violating the primary key.
	if _, err := tx.Exec(`INSERT OR IGNORE INTO sync_enrolled_projects(project,enrolled_at)
		SELECT ?, enrolled_at FROM sync_enrolled_projects WHERE lower(project)=?`, target, alias); err != nil {
		return counts, err
	}
	res, err := tx.Exec(`DELETE FROM sync_enrolled_projects WHERE lower(project)=? AND project<>?`, alias, target)
	if err != nil {
		return counts, err
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		counts["sync_enrolled_projects"] += int(n)
	}

	// Cloud upgrade state is also keyed by project. Keep an existing canonical
	// state authoritative and drop only the shadow alias row.
	var canonicalState int
	if err := tx.QueryRow(`SELECT count(*) FROM cloud_upgrade_state WHERE project=?`, target).Scan(&canonicalState); err != nil {
		return counts, err
	}
	if canonicalState > 0 {
		res, err = tx.Exec(`DELETE FROM cloud_upgrade_state WHERE lower(project)=? AND project<>?`, alias, target)
	} else {
		res, err = tx.Exec(`UPDATE cloud_upgrade_state SET project=? WHERE lower(project)=? AND project<>?`, target, alias, target)
	}
	if err != nil {
		return counts, err
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		counts["cloud_upgrade_state"] += int(n)
	}

	// Memory nodes have a (project,scope,path) uniqueness constraint. Merge
	// duplicate paths conservatively by deleting only the alias duplicate.
	if _, err := tx.Exec(`UPDATE OR IGNORE memory_nodes SET project=? WHERE lower(project)=? AND project<>?`, target, alias, target); err != nil {
		return counts, err
	}
	res, err = tx.Exec(`DELETE FROM memory_nodes WHERE lower(project)=? AND project<>?
		AND EXISTS (SELECT 1 FROM memory_nodes canonical WHERE canonical.project=? AND canonical.scope=memory_nodes.scope AND canonical.path=memory_nodes.path)`,
		alias, target, target)
	if err != nil {
		return counts, err
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		counts["memory_nodes"] += int(n)
	}
	// Any remaining alias nodes have no canonical conflict and can be renamed.
	res, err = tx.Exec(`UPDATE memory_nodes SET project=? WHERE lower(project)=? AND project<>?`, target, alias, target)
	if err != nil {
		return counts, err
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		counts["memory_nodes"] += int(n)
	}
	return counts, nil
}

// AdoptLegacyProject registers a historical name-keyed bucket as an identity
// without inventing a directory binding. It is explicit operator input: the
// caller names the bucket, and only rows already under that exact key move.
func (s *Store) AdoptLegacyProject(name string) (RegisteredProject, error) {
	name, _ = NormalizeProject(name)
	name = strings.TrimSpace(name)
	if name == "" || projectpkg.IsProjectID(name) || strings.ContainsAny(name, "\x00\r\n") || len(name) > 200 {
		return RegisteredProject{}, errors.New("legacy project adoption requires a nonempty single-line name, not a project ID")
	}
	var existing string
	err := s.db.QueryRow(`SELECT id FROM project_identities WHERE lower(name)=?`, name).Scan(&existing)
	if err == nil {
		return RegisteredProject{}, fmt.Errorf("project name %q is already registered as %s", name, existing)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return RegisteredProject{}, err
	}
	tx, err := s.beginTxHook()
	if err != nil {
		return RegisteredProject{}, err
	}
	defer tx.Rollback()
	var rows int
	if err := tx.QueryRow(`SELECT (SELECT count(*) FROM observations WHERE lower(project)=?)+(SELECT count(*) FROM sessions WHERE lower(project)=?)`, name, name).Scan(&rows); err != nil {
		return RegisteredProject{}, err
	}
	if rows == 0 {
		return RegisteredProject{}, fmt.Errorf("legacy project %q has no observations or sessions to adopt", name)
	}
	id, err := opaqueID("prj-")
	if err != nil {
		return RegisteredProject{}, err
	}
	p := RegisteredProject{ID: id, Name: name, CreatedAt: Now()}
	if _, err := tx.Exec(`INSERT INTO project_identities(id,name,created_at) VALUES(?,?,?)`, p.ID, p.Name, p.CreatedAt); err != nil {
		return RegisteredProject{}, err
	}
	if err := refreshProjectAliasTx(tx, p.Name, p.ID); err != nil {
		return RegisteredProject{}, err
	}
	if _, err := canonicalizeProjectAliasTx(tx, name, p.ID); err != nil {
		return RegisteredProject{}, err
	}
	if err := s.commitHook(tx); err != nil {
		return RegisteredProject{}, err
	}
	return p, nil
}
