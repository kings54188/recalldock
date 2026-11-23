package obsidian

import (
	"strings"
	"testing"

	"recalldock.local/recalldock/internal/store"
)

func strPtr(s string) *string { return &s }

func TestObservationToMarkdown(t *testing.T) {
	t.Run("all fields populated — frontmatter, body, and wikilinks", func(t *testing.T) {
		topicKey := "auth/jwt"
		sessionID := "abc123"
		project := "eng"
		obs := store.Observation{
			ID:            1,
			Type:          "bugfix",
			Title:         "Fixed the bug",
			Content:       "The fix was simple.",
			Project:       &project,
			Scope:         "project",
			TopicKey:      &topicKey,
			SessionID:     sessionID,
			RevisionCount: 2,
			CreatedAt:     "2026-01-01T10:00:00Z",
			UpdatedAt:     "2026-01-02T10:00:00Z",
		}

		got := ObservationToMarkdown(obs)

		// Frontmatter block must open and close with ---
		if !strings.HasPrefix(got, "---\n") {
			t.Errorf("expected markdown to start with YAML frontmatter delimiter, got: %q", got[:min(len(got), 20)])
		}

		// All required frontmatter keys must be present
		for _, key := range []string{"id:", "type:", "project:", "scope:", "topic_key:", "session_id:", "created_at:", "updated_at:", "revision_count:"} {
			if !strings.Contains(got, key) {
				t.Errorf("frontmatter missing key %q", key)
			}
		}

		// Specific frontmatter values
		if !strings.Contains(got, "type: \"bugfix\"") {
			t.Errorf("frontmatter must contain 'type: bugfix'")
		}
		if !strings.Contains(got, "topic_key: \"auth/jwt\"") {
			t.Errorf("frontmatter must contain 'topic_key: auth/jwt'")
		}
		if !strings.Contains(got, "session_id: \"abc123\"") {
			t.Errorf("frontmatter must contain 'session_id: abc123'")
		}
		if !strings.Contains(got, "revision_count: 2") {
			t.Errorf("frontmatter must contain 'revision_count: 2'")
		}

		// Title as H1 heading
		if !strings.Contains(got, "# Fixed the bug") {
			t.Errorf("expected H1 heading with title, not found in output")
		}

		// Content body
		if !strings.Contains(got, "The fix was simple.") {
			t.Errorf("expected content body in output")
		}

		// Session wikilink
		if !strings.Contains(got, "[[recalldock/_sessions/abc123]]") {
			t.Errorf("expected session wikilink [[recalldock/_sessions/abc123]], not found")
		}

		// Topic wikilink — prefix = "auth" (first segment of "auth/jwt")
		if !strings.Contains(got, "[[recalldock/_topics/auth]]") {
			t.Errorf("expected topic wikilink [[recalldock/_topics/auth]], not found")
		}
	})

	t.Run("no topic_key — topic_key empty in frontmatter, no topic wikilink", func(t *testing.T) {
		project := "eng"
		obs := store.Observation{
			ID:        2,
			Type:      "decision",
			Title:     "Chose SQLite",
			Content:   "SQLite is the right choice.",
			Project:   &project,
			Scope:     "project",
			TopicKey:  nil,
			SessionID: "sess-001",
			CreatedAt: "2026-02-01T00:00:00Z",
			UpdatedAt: "2026-02-01T00:00:00Z",
		}

		got := ObservationToMarkdown(obs)

		// topic_key must appear empty in frontmatter
		if !strings.Contains(got, `topic_key: ""`) {
			t.Errorf("expected 'topic_key: \"\"' in frontmatter for nil topic_key, got output:\n%s", got)
		}

		// No topic wikilink should be emitted
		if strings.Contains(got, "[[recalldock/_topics/") {
			t.Errorf("expected no topic wikilink when topic_key is nil, but found one")
		}

		// Session wikilink must still be present
		if !strings.Contains(got, "[[recalldock/_sessions/sess-001]]") {
			t.Errorf("expected session wikilink [[recalldock/_sessions/sess-001]]")
		}
	})

	t.Run("no session_id — no session wikilink", func(t *testing.T) {
		topicKey := "arch/db"
		project := "core"
		obs := store.Observation{
			ID:        3,
			Type:      "architecture",
			Title:     "DB Schema decision",
			Content:   "We chose normalized schema.",
			Project:   &project,
			Scope:     "project",
			TopicKey:  &topicKey,
			SessionID: "",
			CreatedAt: "2026-03-01T00:00:00Z",
			UpdatedAt: "2026-03-01T00:00:00Z",
		}

		got := ObservationToMarkdown(obs)

		// No session wikilink when session_id is empty
		if strings.Contains(got, "[[recalldock/_sessions/]]") {
			t.Errorf("expected no empty session wikilink, but found [[recalldock/_sessions/]]")
		}

		// Topic wikilink must still be present — prefix = "arch"
		if !strings.Contains(got, "[[recalldock/_topics/arch]]") {
			t.Errorf("expected topic wikilink [[recalldock/_topics/arch]]")
		}
	})

	t.Run("multi-segment topic_key — prefix uses last slash part", func(t *testing.T) {
		topicKey := "sdd/obsidian-plugin/explore"
		project := "engram"
		obs := store.Observation{
			ID:        4,
			Type:      "architecture",
			Title:     "SDD Explore",
			Content:   "Content here.",
			Project:   &project,
			Scope:     "project",
			TopicKey:  &topicKey,
			SessionID: "s-99",
			CreatedAt: "2026-04-01T00:00:00Z",
			UpdatedAt: "2026-04-01T00:00:00Z",
		}

		got := ObservationToMarkdown(obs)

		// Design says: wikilink prefix = topic_key split on LAST "/"
		// "sdd/obsidian-plugin/explore" → last segment = "explore"
		// But design also says prefix for _topics/ uses -- instead of /
		// Looking at design section 4: [[recalldock/_topics/sdd--obsidian-plugin]] where prefix = topic_key split on last "/"
		// Re-reading: "[[recalldock/_topics/{prefix}]] where prefix = topic_key split on last '/'"
		// For "sdd/obsidian-plugin/explore" → split on last "/" → prefix = "sdd/obsidian-plugin"
		// In the wikilink, "/" → "--" → [[recalldock/_topics/sdd--obsidian-plugin]]
		if !strings.Contains(got, "[[recalldock/_topics/"+hubStem("sdd/obsidian-plugin")+"]]") {
			t.Errorf("expected topic wikilink [[recalldock/_topics/sdd--obsidian-plugin]] for topic_key=%q, got:\n%s", topicKey, got)
		}
	})
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
