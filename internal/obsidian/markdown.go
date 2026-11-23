package obsidian

import (
	"fmt"
	"strings"

	"recalldock.local/recalldock/internal/store"
)

// ObservationToMarkdown converts a store.Observation into an Obsidian-compatible
// markdown string with YAML frontmatter, an H1 title, the content body,
// and a wikilinks footer section.
func ObservationToMarkdown(obs store.Observation) string {
	return observationToMarkdown(obs, true)
}

// observationToMarkdown omits topic edges when no topic hub is materialized.
func observationToMarkdown(obs store.Observation, includeTopic bool) string {
	var sb strings.Builder

	topicKey := ""
	if obs.TopicKey != nil {
		topicKey = *obs.TopicKey
	}
	project := ""
	if obs.Project != nil {
		project = *obs.Project
	}

	// ── YAML Frontmatter ──────────────────────────────────────────────────────
	sb.WriteString("---\n")
	fmt.Fprintf(&sb, "id: %d\n", obs.ID)
	fmt.Fprintf(&sb, "sync_id: %q\n", obs.SyncID)
	sb.WriteString("export_format: recalldock-v1\n")
	fmt.Fprintf(&sb, "type: %q\n", obs.Type)
	fmt.Fprintf(&sb, "project: %q\n", project)
	fmt.Fprintf(&sb, "scope: %q\n", obs.Scope)
	if topicKey == "" {
		sb.WriteString("topic_key: \"\"\n")
	} else {
		fmt.Fprintf(&sb, "topic_key: %q\n", topicKey)
	}
	fmt.Fprintf(&sb, "session_id: %q\n", obs.SessionID)
	fmt.Fprintf(&sb, "created_at: %q\n", obs.CreatedAt)
	fmt.Fprintf(&sb, "updated_at: %q\n", obs.UpdatedAt)
	fmt.Fprintf(&sb, "revision_count: %d\n", obs.RevisionCount)
	fmt.Fprintf(&sb, "lifecycle_state: %s\n", obs.State())
	if obs.ArchivedAt != nil {
		fmt.Fprintf(&sb, "archived_at: %q\n", *obs.ArchivedAt)
	}
	fmt.Fprintf(&sb, "tags:\n  - %q\n", project)
	if obs.Type != "" {
		fmt.Fprintf(&sb, "  - %q\n", obs.Type)
	}
	fmt.Fprintf(&sb, "aliases:\n  - %q\n", obs.Title)
	sb.WriteString("---\n")

	// ── Title as H1 ───────────────────────────────────────────────────────────
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "# %s\n", obs.Title)
	sb.WriteString("\n")

	// ── Content Body ──────────────────────────────────────────────────────────
	if obs.ArchivedAt != nil {
		sb.WriteString("> [!warning] Archived\n> Preserved for history; excluded from default memory retrieval.\n\n")
	}
	sb.WriteString(obs.Content)
	sb.WriteString("\n")

	// ── Wikilinks Footer ──────────────────────────────────────────────────────
	if !includeTopic {
		topicKey = ""
	}
	wikilinks := buildWikilinks(obs.SessionID, topicKey)
	if len(wikilinks) > 0 {
		sb.WriteString("\n---\n")
		for _, link := range wikilinks {
			sb.WriteString(link)
			sb.WriteString("\n")
		}
	}

	return sb.String()
}

// buildWikilinks returns the wikilink lines for a given session ID and topic key.
// Targets are vault-relative and share the exact filename mapping used by Exporter.
func buildWikilinks(sessionID, topicKey string) []string {
	var links []string

	if sessionID != "" {
		links = append(links, fmt.Sprintf("*Session*: [[recalldock/_sessions/%s]]", hubStem(sessionID)))
	}

	if topicKey != "" {
		prefix := topicPrefix(topicKey)
		prefixSafe := hubStem(prefix)
		links = append(links, fmt.Sprintf("*Topic*: [[recalldock/_topics/%s]]", prefixSafe))
	}

	return links
}

// topicPrefix extracts the prefix from a topic_key.
// For "auth/jwt" → "auth"
// For "sdd/obsidian-plugin/explore" → "sdd/obsidian-plugin"
// For "standalone" → "standalone"
func topicPrefix(topicKey string) string {
	idx := strings.LastIndex(topicKey, "/")
	if idx < 0 {
		return topicKey
	}
	return topicKey[:idx]
}
