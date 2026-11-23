package store

import (
	"fmt"
	"strings"

	"recalldock.local/recalldock/internal/timeutil"
)

// ContextMode controls how much persisted context is returned. Full is the
// legacy formatter; the other modes are bounded progressive-disclosure views.
const (
	ContextModeFull  = "full"
	ContextModeIndex = "index"
	ContextModeL0    = "l0"
	ContextModeL1    = "l1"
)

const (
	defaultContextIndexBudget = 1200
	defaultContextL0Budget    = 1800
	defaultContextL1Budget    = 3000
)

// ContextOptions describes a bounded context read. A zero TokenBudget uses a
// conservative mode-specific default for progressive-disclosure modes.
type ContextOptions struct {
	Project     string `json:"project,omitempty"`
	Scope       string `json:"scope,omitempty"`
	Mode        string `json:"mode,omitempty"`
	TokenBudget int    `json:"token_budget,omitempty"`
}

// ContextBudget is content-free accounting for the returned view.
type ContextBudget struct {
	Limit         int    `json:"limit"`
	Used          int    `json:"used"`
	Entries       int    `json:"entries"`
	Estimator     string `json:"estimator"`
	Truncated     bool   `json:"truncated"`
	BudgetLimited bool   `json:"budget_limited,omitempty"`
}

// ContextResponse is the bounded context view returned to MCP callers.
type ContextResponse struct {
	Text        string        `json:"text"`
	Mode        string        `json:"mode"`
	EmptyReason string        `json:"empty_reason,omitempty"`
	Budget      ContextBudget `json:"budget"`
}

// FormatContextBounded returns an explicit progressive-disclosure view. It
// deliberately leaves FormatContext unchanged for legacy callers.
func (s *Store) FormatContextBounded(opts ContextOptions) (ContextResponse, error) {
	var err error
	opts.Scope, err = NormalizeReadScope(opts.Scope)
	if err != nil {
		return ContextResponse{}, err
	}
	mode := strings.ToLower(strings.TrimSpace(opts.Mode))
	if mode == "" {
		mode = ContextModeFull
	}
	switch mode {
	case ContextModeFull, ContextModeIndex, ContextModeL0, ContextModeL1:
	default:
		return ContextResponse{}, fmt.Errorf("invalid context mode %q: must be full, index, l0, or l1", opts.Mode)
	}
	if opts.TokenBudget < 0 {
		return ContextResponse{}, fmt.Errorf("token_budget must be non-negative")
	}
	if mode == ContextModeFull {
		if opts.TokenBudget > 0 {
			return ContextResponse{}, fmt.Errorf("full context does not support token_budget; use mode=index, l0, or l1")
		}
		text, err := s.FormatContext(opts.Project, opts.Scope)
		if err != nil {
			return ContextResponse{}, err
		}
		return ContextResponse{
			Text: text,
			Mode: mode,
			Budget: ContextBudget{
				Used: estimateSearchTokens(text), Entries: countContextLines(text),
				Estimator: SearchTokenEstimator,
			},
		}, nil
	}

	budget := opts.TokenBudget
	if budget == 0 {
		switch mode {
		case ContextModeIndex:
			budget = defaultContextIndexBudget
		case ContextModeL0:
			budget = defaultContextL0Budget
		case ContextModeL1:
			budget = defaultContextL1Budget
		}
	}

	sessions, err := s.RecentSessions(opts.Project, 5)
	if err != nil {
		return ContextResponse{}, err
	}
	pinned, err := s.pinnedObservations(opts.Project, opts.Scope, s.cfg.MaxContextResults)
	if err != nil {
		return ContextResponse{}, err
	}
	observations, err := s.recentUnpinnedObservations(opts.Project, opts.Scope, s.cfg.MaxContextResults)
	if err != nil {
		return ContextResponse{}, err
	}
	prompts, err := s.RecentPrompts(opts.Project, 10)
	if err != nil {
		return ContextResponse{}, err
	}

	lines := []string{fmt.Sprintf("## Memory Context (mode=%s)", mode)}
	if len(sessions) > 0 {
		lines = append(lines, "### Recent Sessions")
		for _, session := range sessions {
			lines = append(lines, fmt.Sprintf("- session_id=%s project=%s started=%s observations=%d", session.ID, session.Project, timeutil.FormatLocal(session.StartedAt), session.ObservationCount))
		}
	}
	if len(prompts) > 0 {
		lines = append(lines, "### Recent Prompt Index")
		for _, prompt := range prompts {
			// Prompt bodies are intentionally omitted from the index. A later
			// search or exact observation read can retrieve them when needed.
			lines = append(lines, fmt.Sprintf("- prompt_id=%d project=%s created=%s", prompt.ID, prompt.Project, timeutil.FormatLocal(prompt.CreatedAt)))
		}
	}
	if len(pinned) > 0 {
		lines = append(lines, "### Pinned Memory Index")
		for _, observation := range pinned {
			lines = append(lines, formatBoundedObservation(observation, mode))
		}
	}
	if len(observations) > 0 {
		lines = append(lines, "### Recent Memory Index")
		for _, observation := range observations {
			lines = append(lines, formatBoundedObservation(observation, mode))
		}
	}

	if len(lines) == 1 {
		return ContextResponse{Mode: mode, EmptyReason: "no_data", Budget: ContextBudget{Limit: budget, Estimator: SearchTokenEstimator}}, nil
	}
	// Soft budget: always return at least one content line. A hard budget that
	// turns a populated project into "No previous memories" is worse than a
	// marked over-budget response.
	text := lines[0] + "\n"
	entries := 0
	truncated := false
	limited := estimateSearchTokens(text) > budget
	for _, line := range lines[1:] {
		entry := strings.HasPrefix(line, "- ")
		candidate := text + line + "\n"
		if estimateSearchTokens(candidate) > budget {
			truncated = true
			if entries == 0 && entry {
				// No metadata block fits; still return one real memory line.
				text = line + "\n"
				entries = 1
				limited = true
			}
			continue
		}
		text = candidate
		if entry {
			entries++
		}
	}
	used := estimateSearchTokens(text)
	if truncated {
		candidate := text + "[context truncated: use mem_search or mem_get_observation for bounded follow-up]\n"
		if estimateSearchTokens(candidate) <= budget {
			text = candidate
			used = estimateSearchTokens(text)
		}
	}
	if used > budget {
		limited = true
	}
	return ContextResponse{
		Text: text, Mode: mode,
		Budget: ContextBudget{Limit: budget, Used: used, Entries: entries, Estimator: SearchTokenEstimator, Truncated: truncated, BudgetLimited: limited},
	}, nil
}

func formatBoundedObservation(observation Observation, mode string) string {
	project := ""
	if observation.Project != nil {
		project = *observation.Project
	}
	topic := ""
	if observation.TopicKey != nil {
		topic = *observation.TopicKey
	}
	base := fmt.Sprintf("- observation_id=%d type=%s title=%q project=%s scope=%s path=%s topic=%s created=%s abstract=%q",
		observation.ID, observation.Type, observation.Title, project, observation.Scope,
		observation.MemoryPath, topic, timeutil.FormatLocal(observation.CreatedAt), observation.EffectiveAbstract())
	switch mode {
	case ContextModeL1:
		return base + fmt.Sprintf(" overview=%q", observation.EffectiveOverview(500))
	case ContextModeL0, ContextModeIndex:
		return base
	default:
		return base
	}
}

func countContextLines(text string) int {
	if strings.TrimSpace(text) == "" {
		return 0
	}
	return len(strings.Split(strings.TrimSuffix(text, "\n"), "\n"))
}

func (s *Store) pinnedObservations(project, scope string, limit int) ([]Observation, error) {
	project, _ = NormalizeProject(project)
	if limit <= 0 {
		limit = s.cfg.MaxContextResults
	}
	query := `SELECT ` + observationSelectColumns + ` FROM observations o WHERE o.deleted_at IS NULL AND o.archived_at IS NULL AND o.pinned = 1`
	args := make([]any, 0, 3)
	if project != "" {
		query += " AND LOWER(o.project) = ?"
		args = append(args, project)
	}
	if scope != "" {
		query += " AND o.scope = ?"
		args = append(args, normalizeScope(scope))
	}
	query += " ORDER BY datetime(o.created_at) DESC, o.id DESC LIMIT ?"
	args = append(args, limit)
	return s.queryObservations(query, args...)
}
