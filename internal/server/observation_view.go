package server

import "recalldock.local/recalldock/internal/store"

func observationIndexView(obs store.Observation, detail string) map[string]any {
	view := map[string]any{"id": obs.ID, "sync_id": obs.SyncID, "project": obs.Project, "title": obs.Title, "memory_class": obs.MemoryClass,
		"revision_count": obs.RevisionCount, "summary_state": obs.SummaryState, "summary_revision": obs.SummaryRevision, "state": obs.State(), "detail": detail, "abstract": obs.EffectiveAbstract()}
	if detail == "l1" {
		view["overview"] = obs.EffectiveOverview(2000)
	}
	return view
}
