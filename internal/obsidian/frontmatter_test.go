package obsidian

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"recalldock.local/recalldock/internal/store"
)

func TestFrontmatterPreservesOpaqueValues(t *testing.T) {
	project, topic := "project: [one]\nnot_a_field: bad", "topic/#one"
	obs := store.Observation{ID: 7, SyncID: "obs-stable-one", Project: &project, TopicKey: &topic, SessionID: "s:\nnot_a_field: bad", Type: "true", Scope: "null", Title: "Quoted \"title\"", RevisionCount: 3}
	markdown := ObservationToMarkdown(obs)
	parts := strings.SplitN(markdown, "---\n", 3)
	if len(parts) != 3 {
		t.Fatal("missing frontmatter")
	}
	var got map[string]any
	if err := yaml.Unmarshal([]byte(parts[1]), &got); err != nil {
		t.Fatalf("invalid YAML: %v", err)
	}
	for key, want := range map[string]string{"sync_id": obs.SyncID, "project": project, "topic_key": topic, "session_id": obs.SessionID, "type": obs.Type, "scope": obs.Scope} {
		if got[key] != want {
			t.Errorf("%s: got %#v want %q", key, got[key], want)
		}
	}
	if _, exists := got["not_a_field"]; exists {
		t.Fatal("value injected a frontmatter field")
	}
}
