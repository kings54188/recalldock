package obsidian

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"

	"recalldock.local/recalldock/internal/store"
)

func observationProjectionPath(obs store.Observation) string {
	project := "unknown"
	if obs.Project != nil && *obs.Project != "" {
		project = sanitizePathComponent(*obs.Project)
	}
	return filepath.ToSlash(filepath.Join(project, sanitizePathComponent(obs.Type), observationStem(obs)))
}

// Links only join endpoints actually selected for this projection. Directions,
// vocabulary and judgment status remain visible; a stored claim is not proof.
func knowledgeGraphLinks(reader StoreReader, data *store.ExportData, project string) (map[int64][]string, error) {
	bySync := map[string]store.Observation{}
	byID := map[int64]store.Observation{}
	for _, obs := range data.Observations {
		if obs.DeletedAt != nil || (project != "" && (obs.Project == nil || *obs.Project != project)) {
			continue
		}
		byID[obs.ID] = obs
		if obs.SyncID != "" {
			bySync[obs.SyncID] = obs
		}
	}
	result := map[int64][]string{}
	add := func(from, to store.Observation, label string) {
		result[from.ID] = append(result[from.ID], fmt.Sprintf("- %s: [[recalldock/%s]]", label, observationProjectionPath(to)))
	}
	if relations, ok := reader.(interface {
		ExportRelationMutations(string) ([]store.SyncMutation, error)
	}); ok {
		rows, err := relations.ExportRelationMutations(project)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			var rel store.Relation
			if err := json.Unmarshal([]byte(row.Payload), &rel); err != nil {
				return nil, fmt.Errorf("invalid relation projection payload: %w", err)
			}
			source, hasSource := bySync[rel.SourceID]
			target, hasTarget := bySync[rel.TargetID]
			if !hasSource || !hasTarget || rel.JudgmentStatus == store.JudgmentStatusIgnored || rel.JudgmentStatus == store.JudgmentStatusOrphaned {
				continue
			}
			switch rel.Relation {
			case store.RelationPending, store.RelationRelated, store.RelationCompatible, store.RelationScoped, store.RelationConflictsWith, store.RelationSupersedes, store.RelationNotConflict:
			default:
				return nil, fmt.Errorf("unsupported relation vocabulary in projection")
			}
			if rel.JudgmentStatus != store.JudgmentStatusJudged && rel.JudgmentStatus != store.JudgmentStatusPending {
				return nil, fmt.Errorf("unsupported relation judgment in projection")
			}
			label := rel.Relation + " (" + rel.JudgmentStatus + ")"
			add(source, target, label)
			add(target, source, "incoming "+label)
		}
	}
	for _, dep := range data.Dependencies {
		source, hasSource := bySync[dep.SourceObservationSyncID]
		target, hasTarget := bySync[dep.DependentObservationSyncID]
		if dep.SourceObservationSyncID == "" {
			source, hasSource = byID[dep.SourceObservationID]
		}
		if dep.DependentObservationSyncID == "" {
			target, hasTarget = byID[dep.DependentObservationID]
		}
		if !hasSource || !hasTarget {
			continue
		}
		status := dep.Status
		if status == "" {
			status = "active"
		}
		if status != "active" && status != "stale" {
			return nil, fmt.Errorf("unsupported dependency status in projection")
		}
		add(source, target, "required_by ("+status+")")
		add(target, source, "depends_on ("+status+")")
	}
	for id, links := range result {
		sort.Strings(links)
		unique := links[:0]
		for _, link := range links {
			if len(unique) == 0 || unique[len(unique)-1] != link {
				unique = append(unique, link)
			}
		}
		result[id] = unique
	}
	return result, nil
}
