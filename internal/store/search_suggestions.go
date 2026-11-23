package store

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

type SearchSuggestion struct {
	Query string `json:"query"`
	ID    int64  `json:"id"`
}

func spellingPatterns(term string) []string {
	if len(term) < 3 || len(term) > 32 {
		return nil
	}
	hasLetter := false
	for _, r := range term {
		if r >= 'a' && r <= 'z' {
			hasLetter = true
			continue
		}
		if r < '0' || r > '9' {
			return nil
		}
	}
	if !hasLetter {
		return nil
	}
	patterns := make([]string, 0, 3*len(term)+1)
	for i := 0; i < len(term); i++ {
		patterns = append(patterns, term[:i]+"?"+term[i+1:], term[:i]+term[i+1:])
	}
	for i := 0; i <= len(term); i++ {
		patterns = append(patterns, term[:i]+"?"+term[i:])
	}
	return patterns
}

func (s *Store) spellingCandidates(term string, opts SearchOptions) ([]string, error) {
	patterns := spellingPatterns(term)
	if len(patterns) == 0 {
		return nil, nil
	}
	if _, err := s.execHook(s.db, `CREATE VIRTUAL TABLE IF NOT EXISTS temp.observations_spelling_vocab USING fts5vocab(main, observations_fts, instance)`); err != nil {
		return nil, err
	}
	matches := make([]string, 0, len(patterns))
	args := make([]any, 0, len(patterns)+8)
	for _, pattern := range patterns {
		matches = append(matches, "v.term GLOB ?")
		args = append(args, pattern)
	}
	sqlQ := `SELECT DISTINCT v.term FROM temp.observations_spelling_vocab v
		JOIN observations o ON o.id=v.doc
		WHERE o.deleted_at IS NULL AND o.archived_at IS NULL
		AND v.col IN ('title','content','topic_key') AND (` + strings.Join(matches, " OR ") + `)`
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
	if opts.RootPath != "" {
		sqlQ += " AND (o.memory_path=? OR o.memory_path LIKE ? ESCAPE '\\')"
		args = append(args, opts.RootPath, searchPathPattern(opts.RootPath))
	}
	if !opts.IncludeStale {
		sqlQ += " AND o.stale_at IS NULL AND o.summary_state != 'stale' AND " + validObservationSourcesSQL("o")
	}
	if opts.Since != "" {
		sqlQ += " AND o.created_at>=?"
		args = append(args, opts.Since)
	}
	if opts.Until != "" {
		sqlQ += " AND o.created_at<?"
		args = append(args, opts.Until)
	}
	sqlQ += " ORDER BY v.term LIMIT 64"
	rows, err := s.queryItHook(s.db, sqlQ, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var candidates []string
	for rows.Next() {
		var candidate string
		if err := rows.Scan(&candidate); err != nil {
			return nil, err
		}
		if candidate != term {
			candidates = append(candidates, candidate)
		}
	}
	return candidates, rows.Err()
}

func replaceSearchTerm(query, term, replacement string) string {
	query = strings.ToLower(query)
	continuation := func(r rune) bool {
		return (unicode.IsLetter(r) || unicode.IsDigit(r)) && !unicode.In(r, unicode.Han)
	}
	var out strings.Builder
	written := 0
	for cursor := 0; cursor < len(query); {
		at := strings.Index(query[cursor:], term)
		if at < 0 {
			break
		}
		at += cursor
		end := at + len(term)
		before, _ := utf8.DecodeLastRuneInString(query[:at])
		after, _ := utf8.DecodeRuneInString(query[end:])
		cursor = end
		if at > 0 && continuation(before) || end < len(query) && continuation(after) {
			continue
		}
		out.WriteString(query[written:at])
		out.WriteString(replacement)
		written = end
	}
	out.WriteString(query[written:])
	return out.String()
}

func (s *Store) suggestSearchSpelling(query string, opts SearchOptions) (*SearchSuggestion, error) {
	if opts.RawFTS || hasFTSSyntax(query) {
		return nil, nil
	}
	terms, _ := literalQueryTerms(query)
	if len(terms) > 4 {
		return nil, nil
	}
	var best *SearchSuggestion
	bestRank := 0.0
	for _, term := range terms {
		candidates, err := s.spellingCandidates(term, opts)
		if err != nil {
			return nil, err
		}
		for _, candidate := range candidates {
			suggestedQuery := replaceSearchTerm(query, term, candidate)
			probe := opts
			probe.Limit = 1
			probe.Offset = 0
			probe.TokenBudget = 0
			results, err := s.search(suggestedQuery, probe)
			if err != nil {
				return nil, err
			}
			if len(results) == 0 {
				continue
			}
			if best == nil || results[0].Rank < bestRank {
				best = &SearchSuggestion{Query: suggestedQuery, ID: results[0].ID}
				bestRank = results[0].Rank
			}
		}
	}
	return best, nil
}
