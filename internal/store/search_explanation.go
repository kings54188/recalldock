package store

type SearchFilterStage struct {
	Stage     string `json:"stage"`
	Input     int    `json:"input"`
	Dropped   int    `json:"dropped"`
	Remaining int    `json:"remaining"`
}

type SearchRelaxation struct {
	Parameters  map[string]any `json:"parameters"`
	CandidateID int64          `json:"candidate_id"`
}

type SearchEmptyExplanation struct {
	SampleLimit int                 `json:"sample_limit"`
	Sampled     bool                `json:"sampled"`
	Stages      []SearchFilterStage `json:"stages"`
	Relaxations []SearchRelaxation  `json:"relaxations,omitempty"`
}

func (s *Store) explainEmptySearch(query string, opts SearchOptions) (*SearchEmptyExplanation, error) {
	probe := opts
	probe.Type, probe.Scope, probe.Profile, probe.RootPath, probe.Since, probe.Until = "", "", "", "", "", ""
	probe.IncludeStale = true
	probe.Detail = SearchDetailL0
	probe.Limit = s.cfg.MaxSearchResults
	probe.Offset = 0
	probe.TokenBudget = 0
	sample, err := s.search(query, probe)
	if err != nil {
		return nil, err
	}
	explanation := &SearchEmptyExplanation{SampleLimit: probe.Limit, Sampled: true}
	explanation.Stages = append(explanation.Stages, SearchFilterStage{Stage: "lexical_sample", Input: len(sample), Remaining: len(sample)})
	current := probe
	type trial struct {
		opts       SearchOptions
		parameters map[string]any
	}
	var trials []trial
	apply := func(stage string, relaxed SearchOptions, parameters map[string]any) {
		filtered := filterSearchOptions(sample, current)
		dropped := len(sample) - len(filtered)
		explanation.Stages = append(explanation.Stages, SearchFilterStage{Stage: stage, Input: len(sample), Dropped: dropped, Remaining: len(filtered)})
		if dropped > 0 {
			trials = append(trials, trial{relaxed, parameters})
		}
		sample = filtered
	}
	current.RootPath = opts.RootPath
	relaxed := opts
	relaxed.RootPath = ""
	apply("root_path", relaxed, map[string]any{"root_path": ""})
	current.Type = opts.Type
	relaxed = opts
	relaxed.Type = ""
	apply("type", relaxed, map[string]any{"type": ""})
	current.Scope = opts.Scope
	relaxed = opts
	relaxed.Scope = ""
	apply("scope", relaxed, map[string]any{"scope": ""})
	current.Profile = opts.Profile
	relaxed = opts
	relaxed.Profile = SearchProfileAll
	apply("profile", relaxed, map[string]any{"profile": SearchProfileAll})
	current.Since, current.Until = opts.Since, opts.Until
	relaxed = opts
	relaxed.Since, relaxed.Until = "", ""
	apply("time", relaxed, map[string]any{"since": "", "until": ""})
	current.IncludeStale = opts.IncludeStale
	relaxed = opts
	relaxed.IncludeStale = true
	apply("stale", relaxed, map[string]any{"include_stale": true})
	terms, _ := literalQueryTerms(query)
	if len(terms) > 1 && opts.MatchMode != "any" && !opts.RawFTS && !hasFTSSyntax(query) {
		relaxed = opts
		relaxed.MatchMode = "any"
		trials = append(trials, trial{relaxed, map[string]any{"match_mode": "any"}})
	}
	for _, candidate := range trials {
		candidate.opts.Limit = 1
		candidate.opts.Offset = 0
		candidate.opts.TokenBudget = 0
		results, err := s.search(query, candidate.opts)
		if err != nil {
			return nil, err
		}
		if len(results) > 0 {
			explanation.Relaxations = append(explanation.Relaxations, SearchRelaxation{Parameters: candidate.parameters, CandidateID: results[0].ID})
		}
	}
	return explanation, nil
}
