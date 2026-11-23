package sync

import (
	"encoding/json"
	"testing"

	"recalldock.local/recalldock/internal/store"
)

func TestSynthesizeMutationsCarriesMemoryClassForLegacyChunk(t *testing.T) {
	project := "engram"
	mutations := synthesizeMutationsFromChunk(ChunkData{Observations: []store.Observation{{
		SyncID: "obs-memory-class", SessionID: "session-memory-class", Type: "learning", Title: "learning",
		Content: "legacy chunk without explicit memory class", Project: &project, Scope: "project",
		MemoryPath: "agent/procedures/sync", Abstract: "sync L0", Overview: "sync L1",
		SummaryState: store.SummaryStateFresh, SummaryRevision: 2,
	}}})
	if len(mutations) != 1 {
		t.Fatalf("mutations=%+v", mutations)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(mutations[0].Payload), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload["memory_class"] != store.MemoryClassAgent {
		t.Fatalf("memory_class=%#v, want %q", payload["memory_class"], store.MemoryClassAgent)
	}
	if payload["memory_path"] != "agent/procedures/sync" || payload["abstract"] != "sync L0" || payload["overview"] != "sync L1" {
		t.Fatalf("hierarchy payload=%#v", payload)
	}
	if payload["summary_state"] != store.SummaryStateFresh || payload["summary_revision"] != float64(2) {
		t.Fatalf("summary payload=%#v", payload)
	}
}
