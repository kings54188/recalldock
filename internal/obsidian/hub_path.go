package obsidian

import (
	"crypto/sha256"
	"fmt"
	"regexp"
)

var plainHubStem = regexp.MustCompile(`^[A-Za-z0-9_-]{1,120}$`)

// hubStem is shared by both link rendering and filenames. Hashing unsafe/raw
// identifiers prevents traversal, wikilink injection and sanitization collisions.
func hubStem(raw string) string {
	if plainHubStem.MatchString(raw) {
		return raw
	}
	digest := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("hub-%x", digest[:16])
}
