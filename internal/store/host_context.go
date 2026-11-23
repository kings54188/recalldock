package store

import (
	"fmt"
	"strings"

	"recalldock.local/recalldock/internal/timeutil"
)

// HostContextBudget is the default token budget for a session-start index. It
// is deliberately small: the host injects it into every new session, so it must
// stay a hint list rather than a memory dump.
const HostContextBudget = 600

// HostContext returns a bounded, project-scoped index for host session start.
// It excludes archived and stale observations, but a review clock running out
// is not invalidity: months of inactivity must not empty the index.
func (s *Store) HostContext(project string, tokenBudget int) (ContextResponse, error) {
	project = s.canonicalProjectOrRaw(project)
	project = strings.TrimSpace(project)
	if project == "" {
		return ContextResponse{}, nil
	}
	if tokenBudget <= 0 {
		tokenBudget = HostContextBudget
	}
	if tokenBudget > 4000 {
		tokenBudget = 4000
	}

	// Apply pin priority before the bound: otherwise older pinned memories are
	// discarded by the recent-only limit before they can be prioritized.
	recent, err := s.queryObservations(`SELECT `+observationSelectColumns+` FROM observations o
		WHERE o.deleted_at IS NULL AND o.archived_at IS NULL AND LOWER(o.project)=?
		AND (o.stale_at IS NULL OR trim(o.stale_at)='') AND o.summary_state != 'stale'
		AND `+validObservationSourcesSQL("o")+`
		ORDER BY o.pinned DESC, datetime(o.created_at) DESC, o.id DESC LIMIT 41`, project)
	if err != nil {
		return ContextResponse{}, err
	}
	moreAvailable := len(recent) > 40
	if moreAvailable {
		recent = recent[:40]
	}
	pinned := make([]Observation, 0, len(recent))
	active := make([]Observation, 0, len(recent))
	for _, observation := range recent {
		if observation.ArchivedAt != nil {
			continue
		}
		if observation.StaleAt != nil && strings.TrimSpace(*observation.StaleAt) != "" {
			continue // source invalidation: do not inject
		}
		if observation.Pinned {
			pinned = append(pinned, observation)
		} else {
			active = append(active, observation)
		}
	}

	lines := []string{fmt.Sprintf("## RecallDock context (project=%s; bounded index, verify before use)", project)}
	entries := 0
	truncated := moreAvailable
	for _, group := range [][]Observation{pinned, active} {
		for _, observation := range group {
			title := clipRunes(observation.Title, 120)
			abstract := clipRunes(observation.EffectiveAbstract(), 160)
			pin := ""
			if observation.Pinned {
				pin = ", pinned"
			}
			created := timeutil.FormatLocal(observation.CreatedAt)
			if len(created) > 10 {
				created = created[:10]
			}
			line := fmt.Sprintf("- [%s] %s (id=%d, %s%s)", observation.Type, title,
				observation.ID, created, pin)
			if abstract != "" && abstract != title {
				line += " — " + abstract
			}
			candidate := strings.Join(lines, "\n") + "\n" + line + "\n"
			if EstimateTokens(candidate) > tokenBudget {
				truncated = true
				continue
			}
			lines = append(lines, line)
			entries++
		}
	}
	text := ""
	if entries > 0 {
		text = strings.Join(lines, "\n")
		if truncated {
			withNotice := text + "\n- [more omitted by limit or budget: use mem_search or mem_context]\n"
			if EstimateTokens(withNotice) <= tokenBudget {
				text = withNotice
			}
		}
	}
	used := 0
	if text != "" {
		used = EstimateTokens(text)
	}
	return ContextResponse{
		Text: text, Mode: ContextModeIndex,
		Budget: ContextBudget{Limit: tokenBudget, Used: used, Entries: entries, Estimator: SearchTokenEstimator, Truncated: truncated},
	}, nil
}

func clipRunes(value string, limit int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= limit {
		return string(runes)
	}
	return string(runes[:limit]) + "…"
}
