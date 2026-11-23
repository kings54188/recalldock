package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
)

var (
	ErrInvalidSearchOptions = errors.New("invalid_arguments")
	ErrInvalidQuery         = errors.New("invalid_query: invalid raw FTS syntax; correct the expression or omit raw_fts for literal search")
	ErrQueryRequired        = errors.New("query_required: provide a non-empty search query")
	ErrQueryHasNoTerms      = errors.New("query_has_no_terms: query contains only ignored standalone stopwords; provide meaningful keywords")
	ErrBudgetTooSmall       = errors.New("budget_too_small")
)

const (
	SearchStrategyFlat         = "flat"
	SearchStrategyHierarchical = "hierarchical"
	SearchStrategyAuto         = "auto"

	SearchDetailL0 = "l0"
	SearchDetailL1 = "l1"
	searchDetailL2 = "l2"

	SearchTokenEstimator = "engram-conservative-v1"

	defaultSearchMaxDepth     = 4
	defaultSearchTimeBudget   = 100 * time.Millisecond
	maxHierarchyNodes         = 256
	maxHierarchyObservations  = 512
	maxSelectedHierarchyNodes = 8
)

// SearchBudget reports deterministic context packing costs. Tokens are a
// conservative estimate, not a model-specific tokenizer count.
type SearchBudget struct {
	Limit      int    `json:"limit"`
	Used       int    `json:"used"`
	Candidates int    `json:"candidates"`
	Estimator  string `json:"estimator"`
	Truncated  bool   `json:"truncated"`
}

// SearchTraceEntry is deliberately content-free so traces remain safe to log.
type SearchTraceEntry struct {
	Stage       string  `json:"stage"`
	CandidateID int64   `json:"candidate_id,omitempty"`
	Path        string  `json:"path,omitempty"`
	Score       float64 `json:"score,omitempty"`
	ScoreSource string  `json:"score_source,omitempty"`
	Reason      string  `json:"reason"`
	Selected    bool    `json:"selected,omitempty"`
}

// SearchRecallSummary is the low-cost counterpart of a full trace.
type SearchRecallSummary struct {
	Flat           int `json:"flat"`
	HierarchyAdded int `json:"hierarchy_added"`
	BudgetDropped  int `json:"budget_dropped"`
	LimitDropped   int `json:"limit_dropped"`
	ScoreTies      int `json:"score_ties"`
	TiedAtCutoff   int `json:"tied_at_cutoff"`
}

type SearchResponse struct {
	QueryGroups      [][]string              `json:"query_groups,omitempty"`
	QueryTerms       []string                `json:"query_terms,omitempty"`
	IgnoredTerms     []string                `json:"ignored_terms,omitempty"`
	Results          []SearchResult          `json:"results"`
	StrategyUsed     string                  `json:"strategy_used"`
	FallbackReason   string                  `json:"fallback_reason,omitempty"`
	EmptyReason      string                  `json:"empty_reason,omitempty"`
	DidYouMean       *SearchSuggestion       `json:"did_you_mean,omitempty"`
	EmptyExplanation *SearchEmptyExplanation `json:"empty_explanation,omitempty"`
	RecallSummary    SearchRecallSummary     `json:"recall_summary"`
	LatencyMS        float64                 `json:"latency_ms"`
	Budget           SearchBudget            `json:"budget"`
	Trace            []SearchTraceEntry      `json:"trace,omitempty"`
}

type hierarchyNodeCandidate struct {
	node  MemoryNode
	score float64
}

type fusedSearchCandidate struct {
	result         SearchResult
	score          float64
	flatRank       int
	hierarchyRank  int
	hierarchyScore float64
	nodeOnly       bool
}

// SearchDetailed adds bounded hierarchical recall and context packing while
// keeping Search as the permanent flat compatibility path.
func (s *Store) SearchDetailed(query string, opts SearchOptions) (*SearchResponse, error) {
	started := time.Now()
	query = NormalizeSearchQuery(query)
	if strings.TrimSpace(query) == "" {
		return nil, ErrQueryRequired
	}
	opts.Project = s.canonicalProjectOrRaw(opts.Project)
	if err := NormalizeSearchOptions(&opts); err != nil {
		return nil, err
	}
	strategy := opts.Strategy
	if strategy == "" {
		strategy = SearchStrategyFlat
	}
	if opts.RawFTS {
		strategy = SearchStrategyFlat
	}
	if hasFTSSyntax(query) && !opts.RawFTS {
		// Operator-shaped text must not pull unrelated descendants through a node term.
		strategy = SearchStrategyFlat
	}
	detail, rootPath := opts.Detail, opts.RootPath
	legacyRequest := strings.TrimSpace(opts.Strategy) == "" && strings.TrimSpace(opts.Detail) == "" &&
		opts.TokenBudget == 0 && rootPath == "" && opts.MaxDepth == 0 && !opts.Trace && !opts.IncludeStale

	limit := opts.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > s.cfg.MaxSearchResults {
		limit = s.cfg.MaxSearchResults
	}
	if opts.Offset != 0 {
		return nil, fmt.Errorf("%w: progressive search does not support offset pagination", ErrInvalidSearchOptions)
	}
	candidateOpts := opts
	candidateOpts.Limit = s.cfg.MaxSearchResults
	candidateOpts.Strategy = ""
	candidateOpts.Detail = SearchDetailL0
	candidateOpts.IncludeStale = opts.IncludeStale
	candidateOpts.TokenBudget = 0
	candidateOpts.RootPath = rootPath
	candidateOpts.MaxDepth = 0
	candidateOpts.Trace = false

	flatResults, err := s.search(query, candidateOpts)
	if err != nil {
		return nil, err
	}
	rawFlatCount := len(flatResults)
	flatResults = filterDetailedResults(flatResults, rootPath, opts.IncludeStale)
	pathFiltered := opts.RootPath != "" && rawFlatCount > 0 && len(flatResults) == 0
	if opts.RootPath != "" && rawFlatCount == 0 {
		probe := candidateOpts
		probe.RootPath, probe.Limit = "", 1
		if outside, probeErr := s.search(query, probe); probeErr == nil && len(outside) > 0 {
			pathFiltered = true
		}
	}
	filterOpts := opts
	filterOpts.IncludeStale = opts.IncludeStale
	beforeOptions := len(flatResults)
	flatResults = filterSearchOptions(flatResults, filterOpts)
	optionsFiltered := beforeOptions > 0 && len(flatResults) == 0

	response := &SearchResponse{
		StrategyUsed: strategy,
		Budget: SearchBudget{
			Limit: opts.TokenBudget, Estimator: SearchTokenEstimator,
		},
	}
	if opts.RawFTS && opts.Strategy != "" && opts.Strategy != SearchStrategyFlat {
		response.FallbackReason = "raw_fts_flat_only"
	}
	if !opts.RawFTS && !hasFTSSyntax(query) {
		response.QueryTerms, response.IgnoredTerms = literalQueryTerms(query)
		response.QueryGroups = literalQueryGroups(query)
	}
	trace := make([]SearchTraceEntry, 0, len(flatResults)+8)
	for _, candidate := range fuseSearchCandidates(query, flatResults, nil, nil) {
		trace = append(trace, SearchTraceEntry{
			Stage: "flat_recall", CandidateID: candidate.result.ID, Path: candidate.result.MemoryPath,
			Score: candidate.score, ScoreSource: candidate.result.ScoreSource, Reason: "fts_match",
		})
	}

	var hierarchyResults []SearchResult
	var candidates []fusedSearchCandidate
	if strategy == SearchStrategyFlat {
		candidates = fuseSearchCandidates(query, flatResults, nil, nil)
	} else {
		maxDepth := opts.MaxDepth
		if maxDepth == 0 {
			maxDepth = defaultSearchMaxDepth
		}
		nodes, nodeTrace, nodeErr := s.findHierarchyNodes(query, opts, maxDepth, defaultSearchTimeBudget)
		if nodeErr != nil {
			return nil, nodeErr
		}
		trace = append(trace, nodeTrace...)
		if strategy == SearchStrategyAuto && len(nodes) == 0 {
			response.StrategyUsed = SearchStrategyFlat
			response.FallbackReason = "no_matching_hierarchy"
			trace = append(trace, SearchTraceEntry{Stage: "strategy", Reason: response.FallbackReason})
			candidates = fuseSearchCandidates(query, flatResults, nil, nil)
		} else {
			var hierarchyScores map[int64]float64
			var hierarchyTrace []SearchTraceEntry
			var hierarchyErr error
			hierarchyResults, hierarchyScores, hierarchyTrace, hierarchyErr = s.findHierarchyObservations(opts, nodes, maxDepth, defaultSearchTimeBudget)
			if hierarchyErr != nil {
				return nil, hierarchyErr
			}
			hierarchyResults = filterSearchOptions(hierarchyResults, filterOpts)
			if opts.MatchMode != "any" {
				hierarchyResults = filterAllQueryTerms(hierarchyResults, response.QueryTerms)
			}
			trace = append(trace, hierarchyTrace...)
			candidates = fuseSearchCandidates(query, flatResults, hierarchyResults, hierarchyScores)
		}
	}

	response.Budget.Candidates = len(candidates)
	requestedDetail := detail
	if requestedDetail == "" {
		if legacyRequest {
			requestedDetail = searchDetailL2
		} else {
			requestedDetail = SearchDetailL0
		}
	}
	response.Results, response.Budget.Used, response.Budget.Truncated, trace = packSearchCandidates(
		candidates, limit, requestedDetail, opts.TokenBudget, trace,
	)
	response.RecallSummary.Flat = len(flatResults)
	response.RecallSummary.HierarchyAdded = countHierarchyOnly(flatResults, hierarchyResults)
	for _, entry := range trace {
		if entry.Stage == "budget" && entry.Reason == "token_budget" {
			response.RecallSummary.BudgetDropped++
		}
		if entry.Stage == "budget" && entry.Reason == "result_limit" {
			response.RecallSummary.LimitDropped++
		}
	}
	for start := 0; start < len(candidates); {
		end := start + 1
		for end < len(candidates) && candidates[end].score == candidates[start].score {
			end++
		}
		if end-start > 1 {
			response.RecallSummary.ScoreTies += end - start
			selected := 0
			for _, result := range response.Results {
				if result.Score == candidates[start].score {
					selected++
				}
			}
			if selected > 0 && selected < end-start {
				response.RecallSummary.TiedAtCutoff = end - start
			}
		}
		start = end
	}
	if len(response.Results) == 0 {
		if len(candidates) == 0 {
			response.EmptyReason = "no_match"
			if pathFiltered {
				response.EmptyReason = "path_out_of_scope"
			} else if optionsFiltered || filteredByOptionsProbe(s, query, opts) {
				response.EmptyReason = "filtered_out"
			}
		} else if opts.TokenBudget > 0 {
			response.EmptyReason = "budget_too_small"
		} else {
			response.EmptyReason = "filtered"
		}
	}
	if len(response.Results) == 0 && len(candidates) > 0 && opts.TokenBudget > 0 {
		smallest := smallestCandidateTokens(candidates, requestedDetail)
		return nil, fmt.Errorf("%w: smallest result needs %d estimated tokens; increase token_budget or omit it", ErrBudgetTooSmall, smallest)
	}
	if len(response.Results) == 0 {
		if response.EmptyReason == "no_match" {
			response.DidYouMean, err = s.suggestSearchSpelling(query, filterOpts)
			if err != nil {
				return nil, err
			}
		}
		if opts.ExplainEmpty {
			response.EmptyExplanation, err = s.explainEmptySearch(query, filterOpts)
			if err != nil {
				return nil, err
			}
		}
	}
	if opts.Trace || opts.ExplainEmpty {
		response.Trace = trace
	}
	response.LatencyMS = float64(time.Since(started).Microseconds()) / 1000
	if opts.TokenBudget > 0 {
		for {
			raw, err := json.MarshalIndent(response, "", "  ")
			if err != nil {
				return nil, err
			}
			cost := EstimateReadTokens(string(raw)) + 16
			if cost <= opts.TokenBudget {
				response.Budget.Used = cost
				break
			}
			if len(response.Results) == 0 {
				return nil, fmt.Errorf("%w: response metadata alone needs %d estimated tokens; increase token_budget or omit it", ErrBudgetTooSmall, cost)
			}
			last := len(response.Results) - 1
			if response.Results[last].SelectedDetail == SearchDetailL1 {
				response.Results[last].SelectedDetail = SearchDetailL0
				response.Results[last].SelectedContent = searchDetailContent(response.Results[last].Observation, SearchDetailL0)
				response.Results[last].DetailDowngraded = &SearchDowngrade{Requested: requestedDetail, Applied: SearchDetailL0, Reason: "budget"}
			} else {
				if len(response.Results) == 1 {
					return nil, fmt.Errorf("%w: smallest response needs %d estimated tokens; increase token_budget or omit it", ErrBudgetTooSmall, cost)
				}
				response.Results = response.Results[:last]
				response.RecallSummary.BudgetDropped++
			}
			response.Budget.Truncated = true
		}
	}
	if !opts.DeferRecallTracking {
		s.RecordSearchRecalls(response.Results)
	}
	return response, nil
}

func normalizeSearchRootPath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	root := strings.Split(value, "/")[0]
	switch root {
	case MemoryClassOperational, MemoryClassUser, MemoryClassAgent, MemoryClassProject:
	default:
		return "", fmt.Errorf("invalid root_path %q: first segment must be a memory class", value)
	}
	normalized, err := NormalizeMemoryPath(root, value)
	if err != nil {
		return "", fmt.Errorf("invalid root_path: %w", err)
	}
	return normalized, nil
}

func filterDetailedResults(results []SearchResult, rootPath string, includeStale bool) []SearchResult {
	filtered := make([]SearchResult, 0, len(results))
	for _, result := range results {
		if rootPath != "" && !pathWithin(result.MemoryPath, rootPath) {
			continue
		}
		if !includeStale && result.StaleAt != nil {
			continue
		}
		filtered = append(filtered, result)
	}
	return filtered
}

func searchPathPattern(root string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(root) + "/%"
}

func pathWithin(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+"/")
}

// filterSearchOptions is a final guard shared by flat and hierarchy paths. A
// candidate that violates an explicit selector must not enter fusion merely
// because another recall path found it.
func filterSearchOptions(results []SearchResult, opts SearchOptions) []SearchResult {
	project := strings.ToLower(strings.TrimSpace(opts.Project))
	scope := normalizeScope(opts.Scope)
	profile := strings.ToLower(strings.TrimSpace(opts.Profile))
	filtered := make([]SearchResult, 0, len(results))
	for _, result := range results {
		if opts.Type != "" && result.Type != opts.Type {
			continue
		}
		if opts.Scope != "" && result.Scope != scope {
			continue
		}
		if project != "" && strings.ToLower(derefString(result.Project)) != project {
			continue
		}
		if profile == SearchProfileSemantic && !isSemanticMemoryClass(result.MemoryClass) {
			continue
		}
		if profile == SearchProfileOperational && result.MemoryClass != MemoryClassOperational {
			continue
		}
		if opts.RootPath != "" && !pathWithin(result.MemoryPath, opts.RootPath) {
			continue
		}
		if !opts.IncludeStale && result.StaleAt != nil {
			continue
		}
		if opts.Since != "" && result.CreatedAt < opts.Since {
			continue
		}
		if opts.Until != "" && result.CreatedAt >= opts.Until {
			continue
		}
		filtered = append(filtered, result)
	}
	return filtered
}

func filterAllQueryTerms(results []SearchResult, terms []string) []SearchResult {
	filtered := make([]SearchResult, 0, len(results))
	for _, result := range results {
		text := strings.ToLower(result.Title + "\n" + result.Content + "\n" + result.Abstract + "\n" + result.Overview + "\n" + derefString(result.TopicKey))
		matched := true
		for _, term := range terms {
			if !containsSearchTerm(text, term) {
				matched = false
				break
			}
		}
		if matched {
			filtered = append(filtered, result)
		}
	}
	return filtered
}

func isSemanticMemoryClass(class string) bool {
	switch class {
	case MemoryClassUser, MemoryClassAgent, MemoryClassProject:
		return true
	}
	return false
}

func (s *Store) findHierarchyNodes(query string, opts SearchOptions, maxDepth int, timeBudget time.Duration) ([]hierarchyNodeCandidate, []SearchTraceEntry, error) {
	started := time.Now()
	queryTerms := searchTerms(query)
	queryGroups := literalQueryGroups(query)
	if len(queryTerms) == 0 {
		return nil, nil, nil
	}
	project, _ := NormalizeProject(opts.Project)
	sqlQ := `SELECT sync_id, project, scope, path, ifnull(parent_path, ''), ifnull(abstract, ''), ifnull(overview, ''),
		content_revision, summary_revision, summary_state, created_at, updated_at, deleted_at
		FROM memory_nodes WHERE deleted_at IS NULL`
	args := make([]any, 0, 5)
	if project != "" {
		sqlQ += " AND LOWER(project) = ?"
		args = append(args, project)
	}
	if opts.Scope != "" {
		sqlQ += " AND scope = ?"
		args = append(args, normalizeScope(opts.Scope))
	}
	if opts.RootPath != "" {
		sqlQ += " AND (path = ? OR path LIKE ? ESCAPE '\\')"
		args = append(args, opts.RootPath, searchPathPattern(opts.RootPath))
	}
	sqlQ += " ORDER BY updated_at DESC LIMIT ?"
	args = append(args, maxHierarchyNodes)
	rows, err := s.queryItHook(s.db, sqlQ, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	baseDepth := pathDepth(opts.RootPath)
	candidates := make([]hierarchyNodeCandidate, 0, 16)
	trace := make([]SearchTraceEntry, 0, 16)
	for rows.Next() {
		if time.Since(started) > timeBudget {
			trace = append(trace, SearchTraceEntry{Stage: "hierarchy_nodes", Reason: "time_budget_exhausted"})
			break
		}
		var node MemoryNode
		if err := rows.Scan(
			&node.SyncID, &node.Project, &node.Scope, &node.Path, &node.ParentPath,
			&node.Abstract, &node.Overview, &node.ContentRevision, &node.SummaryRevision,
			&node.SummaryState, &node.CreatedAt, &node.UpdatedAt, &node.DeletedAt,
		); err != nil {
			return nil, nil, err
		}
		depth := pathDepth(node.Path)
		if baseDepth > 0 && depth-baseDepth > maxDepth || baseDepth == 0 && depth > maxDepth+1 {
			trace = append(trace, SearchTraceEntry{Stage: "hierarchy_nodes", Path: node.Path, Reason: "max_depth"})
			continue
		}
		if !matchesQueryGroups(strings.ToLower(node.Path+"\n"+node.Abstract+"\n"+node.Overview), queryGroups, opts.MatchMode) {
			continue
		}
		score := hierarchyTextScore(queryTerms, node.Path, node.Abstract, node.Overview)
		if score <= 0 {
			continue
		}
		score += float64(depth) * 0.01
		candidates = append(candidates, hierarchyNodeCandidate{node: node, score: score})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score == candidates[j].score {
			return candidates[i].node.Path < candidates[j].node.Path
		}
		return candidates[i].score > candidates[j].score
	})
	candidates = pruneHierarchyAncestors(candidates)
	if len(candidates) > maxSelectedHierarchyNodes {
		candidates = candidates[:maxSelectedHierarchyNodes]
	}
	maxScore := 0.0
	if len(candidates) > 0 {
		maxScore = candidates[0].score
	}
	for _, candidate := range candidates {
		score := 0.0
		if maxScore > 0 {
			score = candidate.score / maxScore
		}
		trace = append(trace, SearchTraceEntry{
			Stage: "hierarchy_nodes", Path: candidate.node.Path, Score: score,
			ScoreSource: "node_match", Reason: "node_match", Selected: true,
		})
	}
	return candidates, trace, nil
}

func pruneHierarchyAncestors(candidates []hierarchyNodeCandidate) []hierarchyNodeCandidate {
	result := make([]hierarchyNodeCandidate, 0, len(candidates))
	for i, candidate := range candidates {
		isBroadAncestor := false
		for j, other := range candidates {
			if i == j || other.score < candidate.score*0.8 {
				continue
			}
			if other.node.Path != candidate.node.Path && pathWithin(other.node.Path, candidate.node.Path) {
				isBroadAncestor = true
				break
			}
		}
		if !isBroadAncestor {
			result = append(result, candidate)
		}
	}
	return result
}

func (s *Store) findHierarchyObservations(opts SearchOptions, nodes []hierarchyNodeCandidate, maxDepth int, timeBudget time.Duration) ([]SearchResult, map[int64]float64, []SearchTraceEntry, error) {
	if len(nodes) == 0 {
		return nil, nil, nil, nil
	}
	started := time.Now()
	project, _ := NormalizeProject(opts.Project)
	sqlQ := `SELECT ` + observationSelectColumns + ` FROM observations WHERE deleted_at IS NULL AND archived_at IS NULL AND memory_path != ''`
	args := make([]any, 0, 6)
	if opts.Type != "" {
		sqlQ += " AND type = ?"
		args = append(args, opts.Type)
	}
	if project != "" {
		sqlQ += " AND LOWER(project) = ?"
		args = append(args, project)
	}
	if opts.Scope != "" {
		sqlQ += " AND scope = ?"
		args = append(args, normalizeScope(opts.Scope))
	}
	sqlQ += memoryClassProfileClause(strings.ToLower(strings.TrimSpace(opts.Profile)), "memory_class")
	if opts.Since != "" {
		sqlQ += " AND created_at >= ?"
		args = append(args, opts.Since)
	}
	if opts.Until != "" {
		sqlQ += " AND created_at < ?"
		args = append(args, opts.Until)
	}
	if opts.RootPath != "" {
		sqlQ += " AND (memory_path = ? OR memory_path LIKE ? ESCAPE '\\')"
		args = append(args, opts.RootPath, searchPathPattern(opts.RootPath))
	}
	paths := make([]string, 0, len(nodes))
	for _, node := range nodes {
		paths = append(paths, "(memory_path = ? OR memory_path LIKE ? ESCAPE '\\')")
		args = append(args, node.node.Path, searchPathPattern(node.node.Path))
	}
	sqlQ += " AND (" + strings.Join(paths, " OR ") + ")"
	if !opts.IncludeStale {
		sqlQ += " AND stale_at IS NULL AND summary_state != 'stale' AND " + validObservationSourcesSQL("observations")
	}
	sqlQ += " ORDER BY updated_at DESC LIMIT ?"
	args = append(args, maxHierarchyObservations)
	rows, err := s.queryItHook(s.db, sqlQ, args...)
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()

	results := make([]SearchResult, 0, 32)
	scores := make(map[int64]float64)
	trace := make([]SearchTraceEntry, 0, 32)
	for rows.Next() {
		if time.Since(started) > timeBudget {
			trace = append(trace, SearchTraceEntry{Stage: "hierarchy_observations", Reason: "time_budget_exhausted"})
			break
		}
		result, scanErr := scanSearchObservation(rows)
		if scanErr != nil {
			return nil, nil, nil, scanErr
		}
		if !opts.IncludeStale && result.StaleAt != nil {
			trace = append(trace, SearchTraceEntry{Stage: "hierarchy_observations", CandidateID: result.ID, Path: result.MemoryPath, Reason: "stale_filtered"})
			continue
		}
		bestScore := 0.0
		maxNodeScore := 0.0
		for _, node := range nodes {
			if node.score > maxNodeScore {
				maxNodeScore = node.score
			}
		}
		for _, node := range nodes {
			if !pathWithin(result.MemoryPath, node.node.Path) {
				continue
			}
			distance := pathDepth(result.MemoryPath) - pathDepth(node.node.Path)
			if distance > maxDepth {
				continue
			}
			score := node.score / float64(distance+1)
			if score > bestScore {
				bestScore = score
			}
		}
		if bestScore <= 0 {
			continue
		}
		results = append(results, result)
		scores[result.ID] = bestScore
		normalized := 0.0
		if maxNodeScore > 0 {
			normalized = bestScore / maxNodeScore
		}
		trace = append(trace, SearchTraceEntry{
			Stage: "hierarchy_observations", CandidateID: result.ID, Path: result.MemoryPath,
			Score: normalized, ScoreSource: "node_match", Reason: "node_descendant",
		})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, err
	}
	sort.SliceStable(results, func(i, j int) bool {
		if scores[results[i].ID] == scores[results[j].ID] {
			return results[i].ID < results[j].ID
		}
		return scores[results[i].ID] > scores[results[j].ID]
	})
	return results, scores, trace, nil
}

func scanSearchObservation(row rowScanner, extra ...any) (SearchResult, error) {
	var result SearchResult
	columns := []any{
		&result.ID, &result.SyncID, &result.SessionID, &result.Type, &result.MemoryClass,
		&result.MemoryPath, &result.Abstract, &result.Overview, &result.SummaryState, &result.SummaryRevision,
		&result.StaleAt, &result.StaleReason, &result.Title, &result.Content, &result.ToolName,
		&result.Project, &result.Scope, &result.TopicKey, &result.RevisionCount, &result.DuplicateCount,
		&result.LastSeenAt, &result.ReviewAfter, &result.RecallCount, &result.LastRecallAt,
		&result.Pinned, &result.CreatedAt, &result.UpdatedAt, &result.DeletedAt, &result.ArchivedAt,
	}
	err := row.Scan(append(columns, extra...)...)
	return result, err
}

func fuseSearchCandidates(query string, flat, hierarchy []SearchResult, hierarchyScores map[int64]float64) []fusedSearchCandidate {
	byID := make(map[int64]*fusedSearchCandidate, len(flat)+len(hierarchy))
	maxRank := 0.0
	for _, result := range flat {
		maxRank = math.Max(maxRank, -result.Rank)
	}
	maxNodeScore := 0.0
	for _, score := range hierarchyScores {
		maxNodeScore = math.Max(maxNodeScore, score)
	}
	for index, result := range flat {
		if result.TopicKey != nil && *result.TopicKey == query {
			result.ScoreSource = "topic_key_exact"
		} else if result.ScoreSource == "" {
			result.ScoreSource = "fts5_bm25"
		}
		score := 0.5
		if maxRank > 0 {
			score += 0.5 * math.Max(0, -result.Rank) / maxRank
		}
		byID[result.ID] = &fusedSearchCandidate{result: result, flatRank: index + 1, score: score}
	}
	for index, result := range hierarchy {
		coverage, matched := lexicalCoverage(query, result)
		nodeScore := hierarchyScores[result.ID]
		if maxNodeScore > 0 {
			nodeScore /= maxNodeScore
		}
		if matched == 0 && nodeScore <= 0 {
			continue
		}
		candidate, ok := byID[result.ID]
		if !ok {
			score := 0.25 * coverage
			if matched == 0 {
				score = 0.1 * nodeScore
			}
			candidate = &fusedSearchCandidate{result: result, score: score, nodeOnly: matched == 0}
			byID[result.ID] = candidate
		}
		candidate.hierarchyRank = index + 1
		candidate.hierarchyScore = nodeScore
	}
	result := make([]fusedSearchCandidate, 0, len(byID))
	for _, candidate := range byID {
		candidate.result.Score = candidate.score
		if candidate.flatRank > 0 && candidate.result.ScoreSource == "fts5_bm25" {
			rank := candidate.result.Rank
			candidate.result.RawBM25 = &rank
		}
		switch {
		case candidate.flatRank > 0:
			candidate.result.SelectionReason = "flat"
			if candidate.hierarchyRank > 0 {
				candidate.result.SelectionReason = "flat_and_hierarchy"
			}
		case candidate.nodeOnly:
			candidate.result.ScoreSource = "node_match"
			candidate.result.SelectionReason = "hierarchy"
		default:
			candidate.result.ScoreSource = "hierarchy_keyword_coverage"
			candidate.result.SelectionReason = "hierarchy"
		}
		result = append(result, *candidate)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].score != result[j].score {
			return result[i].score > result[j].score
		}
		return searchRecencyLess(result[i].result, result[j].result)
	})
	return result
}

func lexicalCoverage(query string, result SearchResult) (float64, int) {
	terms := searchTerms(query)
	if len(terms) == 0 {
		return 0, 0
	}
	text := strings.ToLower(result.Title + "\n" + result.Content + "\n" +
		result.Abstract + "\n" + result.Overview + "\n" + derefString(result.TopicKey))
	matched := 0
	for _, term := range terms {
		if containsSearchTerm(text, term) {
			matched++
			continue
		}
		runes := []rune(term)
		if containsHan(term) && len(runes) >= 3 {
			for i := 0; i+3 <= len(runes); i++ {
				if strings.Contains(text, string(runes[i:i+3])) {
					matched++
					break
				}
			}
		}
	}
	return float64(matched) / float64(len(terms)), matched
}

// filteredByOptionsProbe distinguishes "the query never matched" from "it
// matched but type/scope/profile removed every result". The flat SQL already
// applies those filters, so a probe without them is the only way to know.
func filteredByOptionsProbe(s *Store, query string, opts SearchOptions) bool {
	if opts.Type == "" && opts.Scope == "" && strings.TrimSpace(opts.Profile) == "" {
		return false
	}
	probe := opts
	probe.Type, probe.Scope, probe.Profile = "", "", ""
	probe.Limit, probe.TokenBudget, probe.RootPath, probe.MaxDepth = 1, 0, "", 0
	results, err := s.search(query, probe)
	return err == nil && len(results) > 0
}

func countHierarchyOnly(flat, hierarchy []SearchResult) int {
	seen := make(map[int64]bool, len(flat))
	for _, result := range flat {
		seen[result.ID] = true
	}
	added := 0
	for _, result := range hierarchy {
		if !seen[result.ID] {
			added++
			seen[result.ID] = true
		}
	}
	return added
}

func smallestCandidateTokens(candidates []fusedSearchCandidate, detail string) int {
	smallest := 0
	for _, candidate := range candidates {
		result := candidate.result
		result.SelectedDetail = detail
		result.SelectedContent = searchDetailContent(result.Observation, detail)
		estimated := estimateSearchResultTokens(result)
		if smallest == 0 || estimated < smallest {
			smallest = estimated
		}
	}
	return smallest
}

func estimateSearchResultTokens(result SearchResult) int {
	raw, _ := json.Marshal(result)
	return EstimateReadTokens(string(raw))
}

func packSearchCandidates(candidates []fusedSearchCandidate, limit int, detail string, tokenBudget int, trace []SearchTraceEntry) ([]SearchResult, int, bool, []SearchTraceEntry) {
	results := make([]SearchResult, 0, limit)
	used := 0
	truncated := false
	for _, candidate := range candidates {
		if len(results) >= limit {
			truncated = true
			trace = append(trace, SearchTraceEntry{Stage: "budget", CandidateID: candidate.result.ID, Path: candidate.result.MemoryPath, Score: candidate.score, Reason: "result_limit"})
			continue
		}
		result := candidate.result
		result.SelectedDetail = detail
		result.SelectedContent = searchDetailContent(result.Observation, detail)
		estimated := estimateSearchResultTokens(result)
		if tokenBudget > 0 && used+estimated > tokenBudget && detail == SearchDetailL1 {
			result.DetailDowngraded = &SearchDowngrade{Requested: detail, Applied: SearchDetailL0, Reason: "budget"}
			truncated = true
			trace = append(trace, SearchTraceEntry{Stage: "budget", CandidateID: result.ID, Reason: "detail_downgraded"})
			result.SelectedDetail = SearchDetailL0
			result.SelectedContent = searchDetailContent(result.Observation, SearchDetailL0)
			estimated = estimateSearchResultTokens(result)
		}
		if tokenBudget > 0 && used+estimated > tokenBudget {
			truncated = true
			trace = append(trace, SearchTraceEntry{Stage: "budget", CandidateID: result.ID, Path: result.MemoryPath, Score: candidate.score, Reason: "token_budget"})
			continue
		}
		result.EstimatedTokens = estimated
		results = append(results, result)
		used += estimated
		trace = append(trace, SearchTraceEntry{Stage: "budget", CandidateID: result.ID, Path: result.MemoryPath, Score: candidate.score, Reason: "selected_" + result.SelectedDetail, Selected: true})
	}
	return results, used, truncated, trace
}

func searchDetailContent(observation Observation, detail string) string {
	switch detail {
	case SearchDetailL0:
		return observation.EffectiveAbstract()
	case SearchDetailL1:
		return "Abstract: " + observation.EffectiveAbstract() + "\nOverview: " + observation.EffectiveOverview(2000)
	default:
		return observation.Content
	}
}

// EstimateTokens exposes the conservative estimator used by bounded context and
// search responses, so host injectors can enforce the same budget.
func EstimateTokens(values ...string) int {
	return estimateSearchTokens(values...)
}

func estimateSearchTokens(values ...string) int {
	ascii := 0
	nonASCII := 0
	for _, value := range values {
		for _, r := range value {
			if r <= unicode.MaxASCII {
				ascii++
			} else {
				nonASCII++
			}
		}
	}
	return 12 + int(math.Ceil(float64(ascii)/4.0)) + nonASCII
}

func reciprocalRank(index int) float64 {
	return 1.0 / float64(61+index)
}

// containsSearchTerm keeps ASCII matches on token boundaries. Without this,
// "test" matches "attested" inside a node overview and pulls in unrelated
// descendants.
func containsSearchTerm(text, term string) bool {
	term = strings.ToLower(strings.TrimSpace(term))
	if term == "" {
		return false
	}
	if containsHan(term) {
		return strings.Contains(text, term)
	}
	hasASCII := false
	for _, r := range term {
		if r <= unicode.MaxASCII {
			hasASCII = true
			break
		}
	}
	if !hasASCII {
		return strings.Contains(text, term)
	}
	for _, token := range strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if token == term {
			return true
		}
	}
	return false
}

func searchTerms(query string) []string {
	terms, _ := literalQueryTerms(query)
	return terms
}

func hierarchyTextScore(terms []string, path, abstract, overview string) float64 {
	path = strings.ToLower(path)
	abstract = strings.ToLower(abstract)
	overview = strings.ToLower(overview)
	score := 0.0
	for _, term := range terms {
		if containsSearchTerm(path, term) {
			score += 3
		}
		if containsSearchTerm(abstract, term) {
			score += 2
		}
		if containsSearchTerm(overview, term) {
			score++
		}
	}
	return score
}

func pathDepth(path string) int {
	path = strings.Trim(path, "/")
	if path == "" {
		return 0
	}
	return strings.Count(path, "/") + 1
}
