package opsstore

import (
	"strings"
	"testing"
)

// TestExportLedgerRefusesOverwriteWithActionableMessage keeps the create-only
// guarantee while telling the operator how to proceed. A hand-edited
// projection must never be replaced implicitly.
func TestExportLedgerRefusesOverwriteWithActionableMessage(t *testing.T) {
	s, _ := openTestStore(t)
	root := t.TempDir()
	if _, err := s.Save(root, "lane", 0, fixture()); err != nil {
		t.Fatal(err)
	}
	out := root + "/ledger.md"
	if _, err := s.ExportLedger(root, "lane", out); err != nil {
		t.Fatal(err)
	}
	_, err := s.ExportLedger(root, "lane", out)
	if err == nil {
		t.Fatal("second export must refuse to overwrite")
	}
	if !strings.Contains(err.Error(), "refusing to overwrite") || !strings.Contains(err.Error(), "create-only") {
		t.Fatalf("unexpected message: %v", err)
	}
}
