package obsidian

import (
	"fmt"
	"strings"

	"recalldock.local/recalldock/internal/store"
)

func projectedBodies(data *store.ExportData, project string) (map[int64]string, error) {
	targets := map[string]string{}
	for _, obs := range data.Observations {
		if obs.SyncID != "" && obs.DeletedAt == nil && (project == "" || (obs.Project != nil && *obs.Project == project)) {
			targets[obs.SyncID] = observationProjectionPath(obs)
		}
	}
	result := map[int64]string{}
	for _, obs := range data.Observations {
		if obs.DeletedAt != nil || (project != "" && (obs.Project == nil || *obs.Project != project)) {
			continue
		}
		body, err := rewriteBodyMemoryLinks(obs.Content, targets)
		if err != nil {
			return nil, fmt.Errorf("observation %d body link: %w", obs.ID, err)
		}
		result[obs.ID] = body
	}
	return result, nil
}

// Only explicit stable memory references are projected. Native vault/external
// links, escaped text and code stay byte-for-byte unchanged. No title guesses.
func rewriteBodyMemoryLinks(body string, targets map[string]string) (string, error) {
	var out strings.Builder
	var fence byte
	fenceLength, inlineLength := 0, 0
	for _, line := range strings.SplitAfter(body, "\n") {
		if fence == 0 && (strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t")) {
			out.WriteString(line)
			continue
		}
		trimmed := strings.TrimLeft(line, " \t")
		for strings.HasPrefix(trimmed, ">") {
			trimmed = strings.TrimLeft(strings.TrimPrefix(trimmed, ">"), " \t")
		}
		if inlineLength == 0 && len(trimmed) > 0 && (trimmed[0] == '`' || trimmed[0] == '~') {
			count := 0
			for count < len(trimmed) && trimmed[count] == trimmed[0] {
				count++
			}
			if count >= 3 {
				if fence == 0 {
					fence = trimmed[0]
					fenceLength = count
				} else if fence == trimmed[0] && count >= fenceLength && strings.TrimSpace(trimmed[count:]) == "" {
					fence = 0
					fenceLength = 0
				}
				out.WriteString(line)
				continue
			}
		}
		if fence != 0 {
			out.WriteString(line)
			continue
		}
		for i := 0; i < len(line); {
			if line[i] == '\\' && i+1 < len(line) {
				out.WriteString(line[i : i+2])
				i += 2
				continue
			}
			if line[i] == '`' {
				n := 1
				for i+n < len(line) && line[i+n] == '`' {
					n++
				}
				if inlineLength == 0 {
					inlineLength = n
				} else if inlineLength == n {
					inlineLength = 0
				}
				out.WriteString(line[i : i+n])
				i += n
				continue
			}
			if inlineLength == 0 && strings.HasPrefix(line[i:], "[[memory:") {
				end := strings.Index(line[i:], "]]")
				if end >= 0 {
					inside := line[i+len("[[memory:") : i+end]
					target, label, hasLabel := strings.Cut(inside, "|")
					id, fragment, hasFragment := strings.Cut(target, "#")
					path, ok := targets[id]
					if !ok {
						return "", fmt.Errorf("stable memory endpoint %q is missing or outside selected projection", id)
					}
					out.WriteString("[[recalldock/")
					out.WriteString(path)
					if hasFragment {
						out.WriteByte('#')
						out.WriteString(fragment)
					}
					if hasLabel {
						out.WriteByte('|')
						out.WriteString(label)
					}
					out.WriteString("]]")
					i += end + 2
					continue
				}
			}
			out.WriteByte(line[i])
			i++
		}
	}
	return out.String(), nil
}
