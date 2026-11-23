package sync

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"recalldock.local/recalldock/internal/store"
)

const regressionJSONLimit = 8 << 20

type regressionCountingReader struct {
	remaining int
	read      int
}

func (r *regressionCountingReader) Read(p []byte) (int, error) {
	n := min(len(p), r.remaining)
	clear(p[:n])
	r.remaining -= n
	r.read += n
	return n, nil
}

func TestSyncJSONLimitExactBoundaryAndBoundedRead(t *testing.T) {
	reader := &regressionCountingReader{remaining: 4 * regressionJSONLimit}
	if _, err := readLimitedJSON(reader); err == nil {
		t.Fatal("oversized reader accepted")
	}
	if reader.read != regressionJSONLimit+1 {
		t.Fatalf("read %d bytes, want limit+1", reader.read)
	}
	payload := append([]byte(`{}`), bytes.Repeat([]byte(" "), regressionJSONLimit-2)...)
	ft := NewFileTransport(t.TempDir())
	if err := ft.WriteChunk("boundary", payload, ChunkEntry{}); err != nil {
		t.Fatal(err)
	}
	got, err := ft.ReadChunk("boundary")
	if err != nil || !bytes.Equal(payload, got) {
		t.Fatalf("exact-limit chunk rejected: len=%d, %v", len(got), err)
	}
	if err := os.WriteFile(filepath.Join(ft.syncDir, "manifest.json"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ft.ReadManifest(); err != nil {
		t.Fatalf("exact-limit manifest rejected: %v", err)
	}
}

func TestFileTransportManifestWriteBoundaries(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	outsideFile := filepath.Join(outside, "manifest.json")
	if err := os.WriteFile(outsideFile, []byte(`{"version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(root, "manifest.json")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	ft := NewFileTransport(root)
	if _, err := ft.ReadManifest(); err == nil {
		t.Error("manifest read escaped root")
	}
	if err := ft.WriteManifest(&Manifest{Version: 2}); err == nil {
		t.Error("manifest write escaped root")
	}
	got, err := os.ReadFile(outsideFile)
	if err != nil || string(got) != `{"version":1}` {
		t.Fatalf("outside manifest changed: %v", err)
	}
}

func TestSyncRejectsConcatenatedGzipOverLimit(t *testing.T) {
	var compressed bytes.Buffer
	for i := 0; i < 2; i++ {
		gz := gzip.NewWriter(&compressed)
		if _, err := gz.Write(bytes.Repeat([]byte(" "), regressionJSONLimit/2+1)); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := readGzipStream(&compressed); err == nil {
		t.Fatal("concatenated gzip bypassed decompression limit")
	}
}

func TestFileTransportRejectsUnsafeChunkIDs(t *testing.T) {
	for _, id := range []string{"", ".", "..", "../escape", "nested/chunk", `nested\chunk`, "/escape", "with space", "记忆", "bad\x00id"} {
		t.Run(id, func(t *testing.T) {
			ft := NewFileTransport(t.TempDir())
			if err := ft.WriteChunk(id, []byte(`{}`), ChunkEntry{}); err == nil {
				t.Errorf("WriteChunk accepted unsafe ID %q", id)
			}
			if _, err := ft.ReadChunk(id); err == nil || errors.Is(err, ErrChunkNotFound) {
				t.Errorf("ReadChunk did not reject unsafe ID %q: %v", id, err)
			}
		})
	}
	for _, id := range []string{"aabbccdd", "chunk-1_2.v3", ".legacy"} {
		t.Run("valid-"+id, func(t *testing.T) {
			ft := NewFileTransport(t.TempDir())
			if err := ft.WriteChunk(id, []byte(`{}`), ChunkEntry{}); err != nil {
				t.Fatal(err)
			}
			if _, err := ft.ReadChunk(id); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFileTransportRejectsSymlinkEscape(t *testing.T) {
	for _, linkDirectory := range []bool{false, true} {
		t.Run(map[bool]string{false: "chunk-file", true: "chunks-directory"}[linkDirectory], func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			outsideFile := filepath.Join(outside, "safe.jsonl.gz")
			if err := writeGzip(outsideFile, []byte(`{"outside":true}`)); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(outsideFile)
			if err != nil {
				t.Fatal(err)
			}
			if linkDirectory {
				err = os.Symlink(outside, filepath.Join(root, "chunks"))
			} else {
				if err := os.Mkdir(filepath.Join(root, "chunks"), 0o755); err != nil {
					t.Fatal(err)
				}
				err = os.Symlink(outsideFile, filepath.Join(root, "chunks", "safe.jsonl.gz"))
			}
			if err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}
			ft := NewFileTransport(root)
			if _, err := ft.ReadChunk("safe"); err == nil || errors.Is(err, ErrChunkNotFound) {
				t.Errorf("read symlink escape was not rejected: %v", err)
			}
			if err := ft.WriteChunk("safe", []byte(`{"changed":true}`), ChunkEntry{}); err == nil {
				t.Error("write symlink escape was not rejected")
			}
			after, err := os.ReadFile(outsideFile)
			if err != nil || !bytes.Equal(before, after) {
				t.Errorf("outside file changed: %v", err)
			}
		})
	}
}

func TestSyncRejectsOversizedJSON(t *testing.T) {
	payload := append([]byte(`{}`), bytes.Repeat([]byte(" "), regressionJSONLimit-1)...)
	t.Run("gzip read", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "oversized.gz")
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		gz := gzip.NewWriter(f)
		if _, err := gz.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := readGzip(path); err == nil || !strings.Contains(err.Error(), "limit") {
			t.Fatalf("expected decompressed size limit error, got %v", err)
		}
	})
	t.Run("gzip write", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "oversized.gz")
		if err := writeGzip(path, payload); err == nil {
			t.Fatal("oversized gzip write accepted")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("oversized write created a file: %v", err)
		}
	})
	t.Run("manifest read", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "manifest.json"), payload, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := NewFileTransport(root).ReadManifest(); err == nil || !strings.Contains(err.Error(), "limit") {
			t.Fatalf("expected manifest size limit error, got %v", err)
		}
	})
	t.Run("manifest write", func(t *testing.T) {
		ft := NewFileTransport(t.TempDir())
		m := &Manifest{Version: 1, Chunks: []ChunkEntry{{ID: "large", CreatedBy: strings.Repeat("x", regressionJSONLimit)}}}
		if err := ft.WriteManifest(m); err == nil || !strings.Contains(err.Error(), "limit") {
			t.Fatalf("expected manifest write size limit error, got %v", err)
		}
		if _, err := os.Stat(filepath.Join(ft.syncDir, "manifest.json")); !os.IsNotExist(err) {
			t.Fatalf("oversized manifest write created a file: %v", err)
		}
	})
	t.Run("raw transport manifest", func(t *testing.T) {
		tr := newFakeCloudTransport()
		tr.manifest.Chunks = []ChunkEntry{{ID: "large", CreatedBy: strings.Repeat("x", regressionJSONLimit)}}
		if _, err := NewWithTransport(newTestStore(t), tr).Import(); err == nil || !strings.Contains(err.Error(), "limit") {
			t.Fatalf("expected raw manifest size limit error, got %v", err)
		}
	})
	t.Run("raw transport scan", func(t *testing.T) {
		tr := newFakeCloudTransport()
		tr.manifest.Chunks = []ChunkEntry{{ID: "oversized"}}
		tr.chunks["oversized"] = payload
		if _, err := NewWithTransport(newTestStore(t), tr).Export("test", ""); err == nil || !strings.Contains(err.Error(), "limit") {
			t.Fatalf("expected raw scan size limit error, got %v", err)
		}
	})
	t.Run("raw transport import", func(t *testing.T) {
		tr := newFakeCloudTransport()
		tr.manifest.Chunks = []ChunkEntry{{ID: "oversized"}}
		tr.chunks["oversized"] = payload
		if _, err := NewWithTransport(newTestStore(t), tr).Import(); err == nil || !strings.Contains(err.Error(), "limit") {
			t.Fatalf("expected raw chunk size limit error, got %v", err)
		}
	})
	t.Run("raw transport export", func(t *testing.T) {
		resetSyncTestHooks(t)
		jsonMarshalChunk = func(any) ([]byte, error) { return payload, nil }
		storeExportData = func(*store.Store) (*store.ExportData, error) {
			return &store.ExportData{Sessions: []store.Session{{ID: "synthetic"}}}, nil
		}
		tr := newFakeCloudTransport()
		if _, err := NewWithTransport(newTestStore(t), tr).Export("test", ""); err == nil || !strings.Contains(err.Error(), "limit") {
			t.Fatalf("expected raw chunk write size limit error, got %v", err)
		}
		if tr.writeChunkCalls != 0 {
			t.Fatal("oversized payload reached transport")
		}
	})
}

func TestLocalExportDeletionTombstonesWithoutEnrollment(t *testing.T) {
	for _, hard := range []bool{false, true} {
		t.Run(map[bool]string{false: "soft", true: "hard"}[hard], func(t *testing.T) {
			source, target := newTestStore(t), newTestStore(t)
			if err := source.CreateSession("delete-session", "proj-a", "/synthetic/a"); err != nil {
				t.Fatal(err)
			}
			obsID, err := source.AddObservation(store.AddObservationParams{SessionID: "delete-session", Project: "proj-a", Type: "decision", Title: "delete", Content: "delete", Scope: "project"})
			if err != nil {
				t.Fatal(err)
			}
			obs, err := source.GetObservation(obsID)
			if err != nil {
				t.Fatal(err)
			}
			promptID, err := source.AddPrompt(store.AddPromptParams{SessionID: "delete-session", Project: "proj-a", Content: "delete"})
			if err != nil {
				t.Fatal(err)
			}
			tr := newFakeCloudTransport()
			sourceSync, targetSync := NewWithTransport(source, tr), NewWithTransport(target, tr)
			if _, err := sourceSync.Export("test", ""); err != nil {
				t.Fatal(err)
			}
			if _, err := targetSync.Import(); err != nil {
				t.Fatal(err)
			}
			if err := source.DeleteObservation(obsID, hard); err != nil {
				t.Fatal(err)
			}
			if err := source.DeletePrompt(promptID); err != nil {
				t.Fatal(err)
			}
			tr.manifest.Chunks[0].CreatedAt = time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
			deleted, err := sourceSync.Export("test", "proj-a")
			if err != nil || deleted.IsEmpty || deleted.MutationsExported < 2 {
				t.Fatalf("unenrolled tombstones not exported: %+v, %v", deleted, err)
			}
			if _, err := targetSync.Import(); err != nil {
				t.Fatal(err)
			}
			if _, err := target.GetObservationBySyncID(obs.SyncID); err == nil {
				t.Fatal("deleted observation remained active on target")
			}
			prompts, err := target.RecentPrompts("proj-a", 10)
			if err != nil || len(prompts) != 0 {
				t.Fatalf("deleted prompt remained on target: %+v, %v", prompts, err)
			}
			again, err := sourceSync.Export("test", "")
			if err != nil || !again.IsEmpty {
				t.Fatalf("unchanged tombstones re-exported: %+v, %v", again, err)
			}
		})
	}
}

func TestLocalExportSameSecondIncrementalAndIdempotent(t *testing.T) {
	resetSyncTestHooks(t)
	cutoff := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	timestamp := cutoff.Format("2006-01-02 15:04:05")
	data := &store.ExportData{
		Sessions:     []store.Session{{ID: "s1", StartedAt: timestamp}},
		Observations: []store.Observation{{ID: 1, SyncID: "o1", SessionID: "s1", Title: "first", Content: "original", RevisionCount: 1, CreatedAt: timestamp, UpdatedAt: timestamp}},
		Prompts:      []store.Prompt{{ID: 1, SyncID: "p1", SessionID: "s1", Content: "first", CreatedAt: timestamp}},
	}
	storeExportData = func(*store.Store) (*store.ExportData, error) { return data, nil }
	mutations := map[string][]store.SyncMutation{}
	for _, entity := range []string{store.SyncEntityRelation, store.SyncEntityDependency, store.SyncEntityResourceDependency} {
		mutations[entity] = []store.SyncMutation{{Entity: entity, EntityKey: entity + "-1", Op: store.SyncOpUpsert, Payload: `{"revision":1}`, OccurredAt: timestamp}}
	}
	storeExportRelations = func(*store.Store, string) ([]store.SyncMutation, error) {
		return mutations[store.SyncEntityRelation], nil
	}
	storeExportDependencies = func(*store.Store, string) ([]store.SyncMutation, error) {
		return mutations[store.SyncEntityDependency], nil
	}
	storeExportResourceDependencies = func(*store.Store, string) ([]store.SyncMutation, error) {
		return mutations[store.SyncEntityResourceDependency], nil
	}
	tr := newFakeCloudTransport()
	sy := NewWithTransport(newTestStore(t), tr)
	first, err := sy.Export("test", "")
	if err != nil || first.IsEmpty {
		t.Fatalf("first export: %+v, %v", first, err)
	}
	tr.manifest.Chunks[0].CreatedAt = cutoff.Format(time.RFC3339)
	data.Sessions = append(data.Sessions, store.Session{ID: "s2", StartedAt: timestamp})
	data.Observations[0].Content = "same-second update"
	data.Observations[0].RevisionCount = 2
	data.Observations = append(data.Observations, store.Observation{ID: 2, SyncID: "o2", SessionID: "s2", CreatedAt: timestamp, UpdatedAt: timestamp})
	data.Prompts = append(data.Prompts, store.Prompt{ID: 2, SyncID: "p2", SessionID: "s2", CreatedAt: timestamp})
	for entity := range mutations {
		mutations[entity][0].Payload = `{"revision":2}`
	}
	second, err := sy.Export("test", "")
	if err != nil {
		t.Fatal(err)
	}
	if second.IsEmpty || second.SessionsExported != 1 || second.ObservationsExported != 2 || second.PromptsExported != 1 || second.MutationsExported != 3 {
		t.Fatalf("same-second additions/updates lost or duplicates included: %+v", second)
	}
	var chunk ChunkData
	if err := json.Unmarshal(tr.chunks[second.ChunkID], &chunk); err != nil {
		t.Fatal(err)
	}
	if chunk.Observations[0].SyncID != "o1" || chunk.Observations[0].Content != "same-second update" {
		t.Fatalf("same-ID revision did not propagate: %+v", chunk.Observations)
	}
	tr.manifest.Chunks[1].CreatedAt = cutoff.Format(time.RFC3339)
	for i := range data.Observations {
		data.Observations[i].ID += 100
	}
	for i := range data.Prompts {
		data.Prompts[i].ID += 100
	}
	third, err := sy.Export("test", "")
	if err != nil || !third.IsEmpty || len(tr.manifest.Chunks) != 2 || tr.writeChunkCalls != 2 {
		t.Fatalf("unchanged boundary rows were re-exported: %+v, %v, chunks=%d", third, err, len(tr.manifest.Chunks))
	}
	data.Observations[0].Content = "original"
	data.Observations[0].RevisionCount = 3
	summary := "same-second session summary"
	data.Sessions[0].Summary = &summary
	data.Prompts[0].Content = "same-second prompt update"
	for entity := range mutations {
		mutations[entity][0].Payload = `{"revision":1}`
	}
	fourth, err := sy.Export("test", "")
	if err != nil || fourth.IsEmpty || fourth.SessionsExported != 1 || fourth.ObservationsExported != 1 || fourth.PromptsExported != 1 || fourth.MutationsExported != 3 {
		t.Fatalf("returning to earlier content was mistaken for latest exported state: %+v, %v", fourth, err)
	}
	tr.manifest.Chunks[2].CreatedAt = cutoff.Format(time.RFC3339)
	fifth, err := sy.Export("test", "")
	if err != nil || !fifth.IsEmpty || tr.writeChunkCalls != 3 {
		t.Fatalf("returned state was not idempotent: %+v, %v", fifth, err)
	}
}

func TestLocalExportIgnoresLocalOnlyRecallAndReviewMetadata(t *testing.T) {
	source, target := newTestStore(t), newTestStore(t)
	if err := source.CreateSession("signals", "signals", "/synthetic/signals"); err != nil {
		t.Fatal(err)
	}
	if _, err := source.AddObservation(store.AddObservationParams{SessionID: "signals", Project: "signals", Type: "decision", Title: "signal marker", Content: "signal marker"}); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Search("signal marker", store.SearchOptions{Project: "signals"}); err != nil {
		t.Fatal(err)
	}
	tr := newFakeCloudTransport()
	origin, replica := NewWithTransport(source, tr), NewWithTransport(target, tr)
	if _, err := origin.Export("source", "signals"); err != nil {
		t.Fatal(err)
	}
	if _, err := replica.Import(); err != nil {
		t.Fatal(err)
	}
	if result, err := replica.Export("target", "signals"); err != nil || !result.IsEmpty {
		t.Fatalf("imported unchanged content was echoed because local-only fields differ: %+v %v", result, err)
	}
	if _, err := source.Search("signal marker", store.SearchOptions{Project: "signals"}); err != nil {
		t.Fatal(err)
	}
	if result, err := origin.Export("source", "signals"); err != nil || !result.IsEmpty {
		t.Fatalf("local recall metadata created a sync chunk: %+v %v", result, err)
	}
}

func TestLocalExportTracksStateRegardlessOfClock(t *testing.T) {
	resetSyncTestHooks(t)
	const timestamp = "2000-01-01 00:00:00"
	data := &store.ExportData{
		Sessions:     []store.Session{{ID: "s1", StartedAt: timestamp}},
		Observations: []store.Observation{{SyncID: "o1", SessionID: "s1", Content: "original", CreatedAt: timestamp, UpdatedAt: timestamp}},
		Prompts:      []store.Prompt{{SyncID: "p1", SessionID: "s1", Content: "original", CreatedAt: timestamp}},
	}
	storeExportData = func(*store.Store) (*store.ExportData, error) { return data, nil }
	mutations := []store.SyncMutation{{Entity: store.SyncEntityRelation, EntityKey: "r1", Op: store.SyncOpUpsert, Payload: `{"revision":1}`, OccurredAt: timestamp}}
	storeExportRelations = func(*store.Store, string) ([]store.SyncMutation, error) { return mutations, nil }
	tr := newFakeCloudTransport()
	sy := NewWithTransport(newTestStore(t), tr)
	if first, err := sy.Export("test", ""); err != nil || first.IsEmpty {
		t.Fatalf("first export: %+v, %v", first, err)
	}
	tr.manifest.Chunks[0].CreatedAt = "2099-01-01T00:00:00Z"
	ended := "2000-01-02 00:00:00"
	data.Sessions[0].EndedAt = &ended
	data.Sessions = append(data.Sessions, store.Session{ID: "s2", StartedAt: timestamp})
	data.Observations[0].Content = "changed"
	data.Observations = append(data.Observations, store.Observation{SyncID: "o2", SessionID: "s2", Content: "historical import", CreatedAt: timestamp, UpdatedAt: timestamp})
	data.Prompts[0].Content = "changed"
	data.Prompts = append(data.Prompts, store.Prompt{SyncID: "p2", SessionID: "s2", Content: "historical import", CreatedAt: timestamp})
	mutations[0].Payload = `{"revision":2}`
	second, err := sy.Export("test", "")
	if err != nil || second.IsEmpty || second.SessionsExported != 2 || second.ObservationsExported != 2 || second.PromptsExported != 2 || second.MutationsExported != 1 {
		t.Fatalf("clock skew or old timestamp dropped changed/new state: %+v, %v", second, err)
	}
	third, err := sy.Export("test", "")
	if err != nil || !third.IsEmpty {
		t.Fatalf("unchanged state is not idempotent: %+v, %v", third, err)
	}
}
