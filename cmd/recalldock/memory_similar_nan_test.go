package main

import (
	"strings"
	"testing"
)

func TestMemorySimilarRejectsNaNBeforeOpeningStore(t *testing.T) {
	withArgs(t, "recalldock", "memory", "similar", "--project", "test", "--threshold", "NaN", "--json")
	stubExitWithPanic(t)
	_, stderr, recovered := captureOutputAndRecover(t, func() { cmdMemorySimilar(testConfig(t)) })
	if recovered == nil || !strings.Contains(stderr, "--threshold must be between 0 and 1") {
		t.Fatalf("expected invalid threshold, panic=%v stderr=%q", recovered, stderr)
	}
}
