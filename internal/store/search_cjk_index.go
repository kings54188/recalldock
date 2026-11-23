package store

import (
	"context"
	"database/sql"
	"strings"
	"unicode"
)

// Synchronize dirty IDs and query the projection within one database snapshot.
func (s *Store) syncCJKIndex(tx *sql.Tx) error {
	_, err := s.execHook(tx, `
		CREATE VIRTUAL TABLE IF NOT EXISTS observations_cjk_fts USING fts5(title, content, topic_key);
		CREATE TABLE IF NOT EXISTS observations_cjk_dirty (id INTEGER PRIMARY KEY);
		CREATE TRIGGER IF NOT EXISTS obs_cjk_insert AFTER INSERT ON observations BEGIN
			INSERT OR IGNORE INTO observations_cjk_dirty VALUES (new.id);
		END;
		CREATE TRIGGER IF NOT EXISTS obs_cjk_update AFTER UPDATE OF title, content, topic_key, deleted_at, archived_at ON observations BEGIN
			INSERT OR IGNORE INTO observations_cjk_dirty VALUES (new.id);
		END;
		CREATE TRIGGER IF NOT EXISTS obs_cjk_delete AFTER DELETE ON observations BEGIN
			INSERT OR IGNORE INTO observations_cjk_dirty VALUES (old.id);
		END;
	`)
	if err != nil {
		return err
	}
	marker, err := s.execHook(tx, `INSERT OR IGNORE INTO store_metadata(key, value) VALUES ('cjk_bm25_v1', 'ready')`)
	if err != nil {
		return err
	}
	if n, err := marker.RowsAffected(); err != nil {
		return err
	} else if n > 0 {
		if _, err := s.execHook(tx, `INSERT OR IGNORE INTO observations_cjk_dirty SELECT id FROM observations`); err != nil {
			return err
		}
	}
	rows, err := s.queryItHook(tx, `SELECT d.id, ifnull(o.title,''), ifnull(o.content,''), ifnull(o.topic_key,''),
		o.id IS NOT NULL AND o.deleted_at IS NULL AND o.archived_at IS NULL
		FROM observations_cjk_dirty d LEFT JOIN observations o ON o.id=d.id`)
	if err != nil {
		return err
	}
	type document struct {
		id                    int64
		title, content, topic string
		active                bool
	}
	var batch []document
	for rows.Next() {
		var d document
		if err := rows.Scan(&d.id, &d.title, &d.content, &d.topic, &d.active); err != nil {
			rows.Close()
			return err
		}
		batch = append(batch, d)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, d := range batch {
		if _, err := s.execHook(tx, `DELETE FROM observations_cjk_fts WHERE rowid=?`, d.id); err != nil {
			return err
		}
		if d.active {
			if _, err := s.execHook(tx, `INSERT INTO observations_cjk_fts(rowid,title,content,topic_key) VALUES(?,?,?,?)`,
				d.id, cjkIndexText(d.title), cjkIndexText(d.content), cjkIndexText(d.topic)); err != nil {
				return err
			}
		}
		if _, err := s.execHook(tx, `DELETE FROM observations_cjk_dirty WHERE id=?`, d.id); err != nil {
			return err
		}
	}
	return nil
}

func cjkIndexText(value string) string {
	var out strings.Builder
	var run []rune
	han := false
	flush := func() {
		if len(run) == 0 {
			return
		}
		if !han || len(run) <= 2 {
			out.WriteString(string(run))
			out.WriteByte(' ')
		} else {
			for n := 2; n <= 3; n++ {
				for i := 0; i+n <= len(run); i++ {
					out.WriteString(string(run[i : i+n]))
					out.WriteByte(' ')
				}
			}
		}
		run = nil
	}
	for _, r := range strings.ToLower(value) {
		isHan := unicode.In(r, unicode.Han)
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			continue
		}
		if len(run) > 0 && isHan != han {
			flush()
		}
		han = isHan
		run = append(run, r)
	}
	flush()
	return out.String()
}

func literalQueryTerms(query string) (terms, ignored []string) {
	seen := map[string]bool{}
	var run []rune
	han := false
	flush := func() {
		if len(run) == 0 {
			return
		}
		term := string(run)
		if !seen[term] {
			seen[term] = true
			switch term {
			case "的", "了", "是", "在", "不", "和":
				ignored = append(ignored, term)
			default:
				terms = append(terms, term)
			}
		}
		run = nil
	}
	for _, r := range strings.ToLower(query) {
		isHan := unicode.In(r, unicode.Han)
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			continue
		}
		if len(run) > 0 && isHan != han {
			flush()
		}
		han = isHan
		run = append(run, r)
	}
	flush()
	return terms, ignored
}

func literalQueryGroups(query string) [][]string {
	var groups [][]string
	for _, fragment := range strings.Fields(query) {
		terms, _ := literalQueryTerms(fragment)
		if len(terms) > 0 {
			groups = append(groups, terms)
		}
	}
	return groups
}

func matchesQueryGroups(text string, groups [][]string, mode string) bool {
	for _, group := range groups {
		matched := true
		for _, term := range group {
			if !containsSearchTerm(text, term) {
				matched = false
				break
			}
		}
		if mode == "any" && matched {
			return true
		}
		if mode != "any" && !matched {
			return false
		}
	}
	return mode != "any" && len(groups) > 0
}

func cjkSearchExpression(groups [][]string, matchMode string) string {
	alternatives := make([]string, 0, len(groups))
	for _, group := range groups {
		phrases := make([]string, 0, len(group))
		for _, term := range group {
			runes := []rune(term)
			if containsHan(term) && len(runes) > 2 {
				grams := make([]string, 0, len(runes)-1)
				for i := 0; i+2 <= len(runes); i++ {
					grams = append(grams, string(runes[i:i+2]))
				}
				term = strings.Join(grams, " ")
			}
			phrases = append(phrases, `"`+strings.ReplaceAll(term, `"`, `""`)+`"`)
		}
		alternatives = append(alternatives, "("+strings.Join(phrases, " AND ")+")")
	}
	joiner := " AND "
	if matchMode == "any" {
		joiner = " OR "
	}
	return strings.Join(alternatives, joiner)
}

func (s *Store) searchCJKPreview(query string, opts SearchOptions, limit int) ([]SearchResult, error) {
	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	index, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	defer index.Close()
	index.SetMaxOpenConns(1)
	if _, err := index.Exec(`CREATE VIRTUAL TABLE documents USING fts5(title, content, topic_key);
		CREATE TABLE ordering(id INTEGER PRIMARY KEY, pinned INTEGER, last_recall_at TEXT, created_at TEXT)`); err != nil {
		return nil, err
	}
	querySQL := `SELECT o.id, o.title, o.content, ifnull(o.topic_key,''), o.pinned, o.last_recall_at, o.created_at
		FROM observations o WHERE o.deleted_at IS NULL AND o.archived_at IS NULL`
	args := []any{}
	for _, filter := range []struct{ column, value string }{
		{"project", opts.Project}, {"type", opts.Type}, {"scope", opts.Scope},
	} {
		if filter.value != "" {
			querySQL += " AND o." + filter.column + "=?"
			args = append(args, filter.value)
		}
	}
	querySQL += memoryClassProfileClause(opts.Profile, "o.memory_class")
	if !opts.IncludeStale {
		querySQL += " AND o.stale_at IS NULL AND o.summary_state != 'stale'"
	}
	if opts.Since != "" {
		querySQL += " AND o.created_at>=?"
		args = append(args, opts.Since)
	}
	if opts.Until != "" {
		querySQL += " AND o.created_at<?"
		args = append(args, opts.Until)
	}
	if opts.RootPath != "" {
		querySQL += " AND (o.memory_path=? OR o.memory_path LIKE ? ESCAPE '\\')"
		args = append(args, opts.RootPath, searchPathPattern(opts.RootPath))
	}
	rows, err := tx.Query(querySQL, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var title, content, topic, created string
		var recalled *string
		var pinned bool
		if err := rows.Scan(&id, &title, &content, &topic, &pinned, &recalled, &created); err != nil {
			rows.Close()
			return nil, err
		}
		if _, err := index.Exec(`INSERT INTO documents(rowid,title,content,topic_key) VALUES(?,?,?,?)`, id, cjkIndexText(title), cjkIndexText(content), cjkIndexText(topic)); err != nil {
			rows.Close()
			return nil, err
		}
		if _, err := index.Exec(`INSERT INTO ordering VALUES(?,?,?,?)`, id, pinned, recalled, created); err != nil {
			rows.Close()
			return nil, err
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	candidateLimit := limit
	if !opts.IncludeStale {
		candidateLimit = s.cfg.MaxSearchResults
	}
	ranked, err := index.Query(`SELECT d.rowid, bm25(documents,3.0,1.0,2.0) FROM documents d
		JOIN ordering o ON o.id=d.rowid WHERE documents MATCH ?
		ORDER BY (d.topic_key=?) DESC, bm25(documents,3.0,1.0,2.0), o.pinned DESC, o.last_recall_at DESC, o.created_at DESC, o.id DESC LIMIT ? OFFSET ?`, cjkSearchExpression(literalQueryGroups(query), opts.MatchMode), cjkIndexText(query), candidateLimit, maxSearchOffset(opts.Offset))
	if err != nil {
		return nil, err
	}
	defer ranked.Close()
	var results []SearchResult
	for ranked.Next() {
		var id int64
		var rank float64
		if err := ranked.Scan(&id, &rank); err != nil {
			return nil, err
		}
		obs, err := s.getObservationTx(tx, id)
		if err != nil {
			return nil, err
		}
		if !opts.IncludeStale {
			valid, err := observationSourcesValidTx(tx, obs.SyncID)
			if err != nil {
				return nil, err
			}
			if !valid {
				continue
			}
		}
		if obs.TopicKey != nil && *obs.TopicKey == query {
			rank = -1000
		}
		results = append(results, SearchResult{Observation: *obs, Rank: rank, ScoreSource: "fts5_bm25"})
		if len(results) == limit {
			break
		}
	}
	return results, ranked.Err()
}

func (s *Store) searchCJKRanked(query string, opts SearchOptions, limit int) ([]SearchResult, error) {
	terms, _ := literalQueryTerms(query)
	if len(terms) == 0 {
		return nil, ErrQueryHasNoTerms
	}
	if opts.readOnlyProjection {
		return s.searchCJKPreview(query, opts, limit)
	}
	tx, err := s.beginTxHook()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Acquire the writer before reading dirty IDs to avoid a read-to-write upgrade.
	if _, err := s.execHook(tx, `UPDATE store_metadata SET value=value WHERE key='cjk_bm25_v1'`); err != nil {
		return nil, err
	}
	if err := s.syncCJKIndex(tx); err != nil {
		return nil, err
	}
	sqlQ := `SELECT ` + observationSelectColumns + `, f.cjk_rank
		FROM (
			SELECT rowid AS fts_id, bm25(observations_cjk_fts,3.0,1.0,2.0) AS cjk_rank
			FROM observations_cjk_fts WHERE observations_cjk_fts MATCH ?
		) f CROSS JOIN observations o ON o.id=f.fts_id
		WHERE o.deleted_at IS NULL AND o.archived_at IS NULL`
	args := []any{cjkSearchExpression(literalQueryGroups(query), opts.MatchMode)}
	if opts.Project != "" {
		sqlQ += " AND lower(o.project)=?"
		args = append(args, opts.Project)
	}
	if opts.Type != "" {
		sqlQ += " AND o.type=?"
		args = append(args, opts.Type)
	}
	if opts.Scope != "" {
		sqlQ += " AND o.scope=?"
		args = append(args, normalizeScope(opts.Scope))
	}
	sqlQ += memoryClassProfileClause(opts.Profile, "o.memory_class")
	if opts.Since != "" {
		sqlQ += " AND o.created_at>=?"
		args = append(args, opts.Since)
	}
	if opts.Until != "" {
		sqlQ += " AND o.created_at<?"
		args = append(args, opts.Until)
	}
	if opts.RootPath != "" {
		sqlQ += " AND (o.memory_path=? OR o.memory_path LIKE ? ESCAPE '\\')"
		args = append(args, opts.RootPath, searchPathPattern(opts.RootPath))
	}
	if !opts.IncludeStale {
		sqlQ += " AND o.stale_at IS NULL AND o.summary_state != 'stale' AND " + validObservationSourcesSQL("o")
	}
	sqlQ += " ORDER BY (o.topic_key=?) DESC, f.cjk_rank, o.pinned DESC, o.last_recall_at DESC, o.created_at DESC, o.id DESC LIMIT ? OFFSET ?"
	args = append(args, query, limit, maxSearchOffset(opts.Offset))
	rows, err := s.queryItHook(tx, sqlQ, args...)
	if err != nil {
		return nil, err
	}
	var results []SearchResult
	for rows.Next() {
		var rank float64
		r, err := scanSearchObservation(rows, &rank)
		if err != nil {
			rows.Close()
			return nil, err
		}
		r.Rank = rank
		r.ScoreSource = "fts5_bm25"
		if r.TopicKey != nil && *r.TopicKey == query {
			r.Rank = -1000
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := s.commitHook(tx); err != nil {
		return nil, err
	}
	return results, nil
}
