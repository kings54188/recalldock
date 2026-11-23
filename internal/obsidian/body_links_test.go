package obsidian

import (
	"strings"
	"testing"
)

func TestStableBodyMemoryLinksRespectCodeAndScope(t *testing.T) {
	targets := map[string]string{"obs-one": "p/decision/memory-obs-one"}
	input := "See [[memory:obs-one#Details|source]]. Keep [[User note]] and [external](https://example.invalid).\n`[[memory:missing]]`\n```text\n[[memory:missing]]\n```\n\\[[memory:missing]]\n"
	out, err := rewriteBodyMemoryLinks(input, targets)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"[[recalldock/p/decision/memory-obs-one#Details|source]]", "[[User note]]", "`[[memory:missing]]`", "\\[[memory:missing]]"} {
		if !strings.Contains(out, part) {
			t.Fatalf("missing %q: %s", part, out)
		}
	}
	if _, err := rewriteBodyMemoryLinks("[[memory:outside-project]]", targets); err == nil {
		t.Fatal("unselected or absent endpoint accepted")
	}
	if input == out {
		t.Fatal("explicit stable link not rewritten")
	}
}
