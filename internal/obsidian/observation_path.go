package obsidian

import "recalldock.local/recalldock/internal/store"

// Stable sync identity keeps filenames unchanged across title edits and JSON
// transfers. Legacy fixtures without identity retain the old local-ID fallback.
func observationStem(obs store.Observation) string {
	if obs.SyncID != "" {
		return "memory-" + hubStem(obs.SyncID)
	}
	return Slugify(obs.Title, obs.ID)
}
