package store

import (
	"sort"
	"strings"
	"unicode"
)

const maxSubstringFallbackCandidates = 24

type substringFallbackPlan struct {
	Candidates []string
	MinMatches int
	// Required keeps the user's whole terms for coverage scoring. CJK queries
	// may return a useful partial match instead of FTS5's all-or-nothing AND.
	Required []string
}

func containsHan(value string) bool {
	for _, r := range value {
		if unicode.In(r, unicode.Han) {
			return true
		}
	}
	return false
}

func buildSubstringFallbackPlan(query string) substringFallbackPlan {
	query = strings.TrimSpace(strings.ToLower(query))
	if query == "" {
		return substringFallbackPlan{}
	}
	if containsHan(query) {
		return buildCJKFallbackPlan(query)
	}

	hasConnector := false
	parts := strings.FieldsFunc(query, func(r rune) bool {
		connector := unicode.IsSpace(r) || r == '-' || r == '_' || r == '/' || r == ':' || unicode.In(r, unicode.Dash)
		if connector && !unicode.IsSpace(r) {
			hasConnector = true
		}
		return connector
	})
	if hasConnector {
		candidates := uniqueFallbackCandidates(parts)
		if len(candidates) >= 2 {
			return substringFallbackPlan{Candidates: candidates, MinMatches: len(candidates)}
		}
	}
	// Plain ASCII queries keep strict AND semantics; broadening them would
	// turn every two-word query into an OR search.
	return substringFallbackPlan{}
}

// buildCJKFallbackPlan emits overlapping 3-gram substrings for Han runs and
// keeps whole tokens for coverage. unicode61 indexes a contiguous Han run as
// one token, so a natural Chinese phrase otherwise cannot match a longer run.
func buildCJKFallbackPlan(query string) substringFallbackPlan {
	terms := uniqueFallbackCandidates(strings.FieldsFunc(query, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.In(r, unicode.Dash)
	}))
	candidates := make([]string, 0, maxSubstringFallbackCandidates)
	appendCandidate := func(value string) {
		value = strings.ToLower(strings.TrimSpace(value))
		if len([]rune(value)) < 2 || len(candidates) >= maxSubstringFallbackCandidates {
			return
		}
		for _, existing := range candidates {
			if existing == value {
				return
			}
		}
		candidates = append(candidates, value)
	}

	for _, term := range terms {
		appendCandidate(term)
		var run []rune
		han := false
		flush := func() {
			if len(run) >= 2 {
				if !han || len(run) == 2 {
					appendCandidate(string(run))
				} else {
					for i := 0; i+3 <= len(run); i++ {
						appendCandidate(string(run[i : i+3]))
					}
					appendCandidate(string(run))
				}
			}
			run = nil
		}
		for _, r := range term {
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
	}
	candidates = uniqueFallbackCandidates(candidates)
	minimum := 1
	if len(terms) == 1 && len(candidates) > 1 {
		// One long Han run should match at least two overlapping trigrams,
		// which avoids matching a single common bigram such as "项目".
		minimum = 2
	}
	return substringFallbackPlan{Candidates: candidates, MinMatches: minimum, Required: terms}
}

func uniqueFallbackCandidates(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if len([]rune(value)) < 2 {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
		if len(result) == maxSubstringFallbackCandidates {
			break
		}
	}
	return result
}

func (s *Store) searchSubstringFallback(query string, opts SearchOptions, limit int, seen map[int64]bool) ([]SearchResult, error) {
	plan := buildSubstringFallbackPlan(query)
	if len(plan.Candidates) == 0 || plan.MinMatches <= 0 || limit <= 0 {
		return nil, nil
	}

	haystack := `lower(o.title || char(10) || o.content || char(10) || ifnull(o.topic_key, ''))`
	matchParts := make([]string, 0, len(plan.Candidates))
	args := make([]any, 0, len(plan.Candidates)+6)
	for _, candidate := range plan.Candidates {
		matchParts = append(matchParts, `CASE WHEN instr(`+haystack+`, lower(?)) > 0 THEN 1 ELSE 0 END`)
		args = append(args, candidate)
	}

	sqlQ := `SELECT ` + observationSelectColumns + ` FROM observations o WHERE o.deleted_at IS NULL AND o.archived_at IS NULL AND (` + strings.Join(matchParts, " + ") + `) >= ?`
	args = append(args, plan.MinMatches)
	if opts.Type != "" {
		sqlQ += " AND o.type = ?"
		args = append(args, opts.Type)
	}
	if opts.Project != "" {
		sqlQ += " AND lower(o.project) = ?"
		args = append(args, opts.Project)
	}
	if opts.Scope != "" {
		sqlQ += " AND o.scope = ?"
		args = append(args, normalizeScope(opts.Scope))
	}
	sqlQ += memoryClassProfileClause(opts.Profile, "o.memory_class")
	if opts.Since != "" {
		sqlQ += " AND o.created_at >= ?"
		args = append(args, opts.Since)
	}
	if opts.Until != "" {
		sqlQ += " AND o.created_at < ?"
		args = append(args, opts.Until)
	}
	if opts.RootPath != "" {
		sqlQ += " AND (o.memory_path = ? OR o.memory_path LIKE ? ESCAPE '\\')"
		args = append(args, opts.RootPath, searchPathPattern(opts.RootPath))
	}
	if !opts.IncludeStale {
		sqlQ += " AND o.stale_at IS NULL AND o.summary_state != 'stale' AND " + validObservationSourcesSQL("o")
	}
	queryLimit := limit + len(seen)
	if len(plan.Required) > 0 {
		// Coverage ranking needs more than the final page so a weak recent
		// match cannot hide a stronger older result.
		queryLimit = limit*5 + len(seen)
	}
	if queryLimit > s.cfg.MaxSearchResults {
		queryLimit = s.cfg.MaxSearchResults
	}
	sqlQ += " ORDER BY o.updated_at DESC, o.id DESC LIMIT ? OFFSET ?"
	args = append(args, queryLimit, maxSearchOffset(opts.Offset))

	rows, err := s.queryItHook(s.db, sqlQ, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type scoredResult struct {
		result  SearchResult
		matched int
	}
	scored := make([]scoredResult, 0, limit)
	for rows.Next() {
		result, scanErr := scanSearchObservation(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		if seen[result.ID] {
			continue
		}
		matched := 0
		if len(plan.Required) > 0 {
			text := strings.ToLower(result.Title + "\n" + result.Content + "\n" + derefString(result.TopicKey))
			for _, candidate := range plan.Candidates {
				if strings.Contains(text, candidate) {
					matched++
				}
			}
			if matched < plan.MinMatches {
				continue
			}
		}
		result.Rank = 1000 - float64(matched)
		result.ScoreSource = "substring_match"
		scored = append(scored, scoredResult{result: result, matched: matched})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(plan.Required) > 0 {
		sort.SliceStable(scored, func(i, j int) bool {
			if scored[i].matched != scored[j].matched {
				return scored[i].matched > scored[j].matched
			}
			return scored[i].result.UpdatedAt > scored[j].result.UpdatedAt
		})
	}
	results := make([]SearchResult, 0, limit)
	for _, item := range scored {
		if len(results) == limit {
			break
		}
		results = append(results, item.result)
		seen[item.result.ID] = true
	}
	return results, nil
}
