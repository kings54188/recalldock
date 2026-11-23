package sync

import (
	"os"
	"path/filepath"
	"testing"
)

// Private historical data is opt-in and is never embedded in the repository.
// Import only into newTestStore's temporary database; New reads the fixture
// directory and records import state in that temporary database, not the files.
func TestPrivateLegacyChunksImportIsIdempotent(t *testing.T) {
	dir := os.Getenv("RECALLDOCK_LEGACY_FIXTURE_DIR")
	if dir == "" {
		t.Skip("set RECALLDOCK_LEGACY_FIXTURE_DIR to a private manifest/chunks directory")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal("cannot resolve private fixture directory")
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
		t.Fatal("private fixture manifest is unavailable")
	}
	s := newTestStore(t)
	sy := New(s, dir)
	first, err := sy.Import()
	if err != nil {
		t.Fatal("private legacy import failed; inspect locally without publishing record contents")
	}
	if first.ChunksImported != 5 || first.ObservationsImported == 0 {
		t.Fatalf("expected five nonempty legacy chunks; counts=%+v", first)
	}
	before, err := s.Stats()
	if err != nil {
		t.Fatal("cannot read temporary-store counts")
	}
	second, err := sy.Import()
	if err != nil {
		t.Fatal("private legacy replay failed")
	}
	after, err := s.Stats()
	if err != nil {
		t.Fatal("cannot read replay counts")
	}
	if second.ChunksImported != 0 || second.ChunksSkipped != 5 || second.ObservationsImported != 0 {
		t.Fatalf("replay must skip all five chunks; counts=%+v", second)
	}
	if before.TotalObservations != after.TotalObservations || before.TotalSessions != after.TotalSessions || before.TotalPrompts != after.TotalPrompts {
		t.Fatal("private legacy replay changed stored record counts")
	}
	t.Logf("legacy fixture: chunks=%d observations=%d sessions=%d prompts=%d; replay skipped=%d", first.ChunksImported, before.TotalObservations, before.TotalSessions, before.TotalPrompts, second.ChunksSkipped)
}
