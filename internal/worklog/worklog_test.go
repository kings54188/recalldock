package worklog

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func checkpointFixture() Checkpoint {
	return Checkpoint{Goal: "Synthetic task", Next: []string{"Verify current files"}, Summary: "One bounded milestone", Details: "Only selected facts and evidence references, not a raw log."}
}

func TestCheckpointCASAndBoundedReads(t *testing.T) {
	root := t.TempDir()
	cp := checkpointFixture()
	if _, err := Save(root, "lane", 0, cp); err != nil {
		t.Fatal(err)
	}
	if _, err := Save(root, "lane", 0, cp); !errors.Is(err, ErrGeneration) {
		t.Fatalf("expected stale generation: %v", err)
	}
	summary, err := Read(root, "lane", false)
	if err != nil || len(summary) > SummaryLimit {
		t.Fatalf("summary: %d %v", len(summary), err)
	}
	if strings.Contains(summary, cp.Details) {
		t.Fatal("default read loaded details")
	}
	full, err := Read(root, "lane", true)
	if err != nil || !strings.Contains(full, cp.Details) {
		t.Fatal("selected full read missing detail")
	}
	if next, err := Save(root, "lane", 1, cp); err != nil || next != 2 {
		t.Fatalf("revision: %d %v", next, err)
	}
	cp.Details = strings.Repeat("界", FileLimit)
	if _, err := Save(root, "lane", 2, cp); !errors.Is(err, ErrBudget) {
		t.Fatalf("oversize must fail: %v", err)
	}
	full, _ = Read(root, "lane", true)
	if strings.Contains(full, "generation: 3") {
		t.Fatal("oversize write changed checkpoint")
	}
}

func TestCheckpointReadRejectsRelocatedRoot(t *testing.T) {
	original := filepath.Join(t.TempDir(), "original")
	if err := os.Mkdir(original, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Save(original, "lane", 0, checkpointFixture()); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(filepath.Dir(original), "moved")
	if err := os.Rename(original, moved); err != nil {
		t.Fatal(err)
	}
	for _, full := range []bool{false, true} {
		body, err := Read(moved, "lane", full)
		if err == nil || body != "" || !strings.Contains(err.Error(), "canonical root differs") {
			t.Fatalf("relocated root returned history: full=%v body=%q err=%v", full, body, err)
		}
	}
}

func TestCheckpointReadResolvesRootSymlink(t *testing.T) {
	root := t.TempDir()
	if _, err := Save(root, "lane", 0, checkpointFixture()); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(alias, "lane", false); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentCheckpointWritersDoNotLoseUpdates(t *testing.T) {
	root := t.TempDir()
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := Save(root, "race", 0, checkpointFixture()); err == nil {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("successful writers: %d", successes.Load())
	}
}

func TestCheckpointRejectsTraversalSymlinksAndLegacyOverwrite(t *testing.T) {
	root := t.TempDir()
	if _, err := Save(root, "../escape", 0, checkpointFixture()); err == nil {
		t.Fatal("unsafe lane")
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, ".recalldock")); err != nil {
		t.Fatal(err)
	}
	if _, err := Save(root, "lane", 0, checkpointFixture()); err == nil {
		t.Fatal("symlink metadata")
	}
	root = t.TempDir()
	dir := filepath.Join(root, ".recalldock/work/lane")
	os.MkdirAll(dir, 0700)
	path := filepath.Join(dir, "current.md")
	os.WriteFile(path, []byte("manual progress to preserve"), 0600)
	if _, err := Save(root, "lane", 0, checkpointFixture()); err == nil {
		t.Fatal("must not adopt/overwrite an unrelated manual ledger")
	}
	body, _ := os.ReadFile(path)
	if string(body) != "manual progress to preserve" {
		t.Fatal("manual progress changed")
	}
}

func TestCheckpointRejectsSecretsAndTranscriptsWithoutChangingGeneration(t *testing.T) {
	root := t.TempDir()
	safe := checkpointFixture()
	if _, err := Save(root, "safe-lane", 0, safe); err != nil {
		t.Fatal(err)
	}
	before, _ := Read(root, "safe-lane", true)
	for _, bad := range []string{"password=short", "DEEPSEEK_API_KEY=small", "用户：全部对话\n助手：完整回答", "unsafe\x1b[31mcontrol"} {
		cp := safe
		cp.Details = bad
		if _, err := Save(root, "safe-lane", 1, cp); err == nil {
			t.Fatal("unsafe checkpoint accepted")
		}
		after, _ := Read(root, "safe-lane", true)
		if after != before {
			t.Fatal("rejected content overwrote recovery point")
		}
	}
}
