// Package memorypolicy is the shared fail-closed agent transport boundary.
// Local operator CLI actions are a separate trust surface, never JSON flags.
package memorypolicy

import "strings"

type Decision string

const (
	Read      Decision = "read"
	Lifecycle Decision = "lifecycle_metadata"
	Propose   Decision = "propose_only"
	Deny      Decision = "operator_review_required"
)

func MCP(tool, action string) Decision {
	switch tool {
	case "mem_search", "mem_context", "mem_get_observation", "mem_current_project", "mem_suggest_topic_key", "mem_stats", "mem_timeline", "mem_doctor":
		return Read
	case "mem_commit_session":
		return Propose
	case "mem_session_start", "mem_session_end":
		return Lifecycle
	case "mem_review":
		if strings.TrimSpace(action) == "list" {
			return Read
		}
	}
	return Deny
}

func HTTP(method, path string) Decision {
	if path == "/export" {
		return Deny
	} // bulk interchange belongs to the local operator CLI
	switch method {
	case "GET", "HEAD", "OPTIONS":
		return Read
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if method == "POST" {
		if len(parts) == 1 && parts[0] == "sessions" {
			return Lifecycle
		}
		if len(parts) == 3 && parts[0] == "sessions" && parts[1] != "" {
			if parts[2] == "candidates" {
				return Propose
			}
			if parts[2] == "end" {
				return Lifecycle
			}
		}
	}
	return Deny
}
