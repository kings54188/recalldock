package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResourceMissingPreservesErrorChainAndCLIStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.txt")
	_, err := hashResourceFile(path)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lost missing cause: %v", err)
	}
	withArgs(t, "recalldock", "resources", "check", "--file", path)
	stdout, stderr := captureOutput(t, func() { cmdResourcesCheck(testConfig(t)) })
	if stderr != "" || !strings.Contains(stdout, `"status": "missing"`) {
		t.Fatalf("stdout=%s stderr=%s", stdout, stderr)
	}
}
