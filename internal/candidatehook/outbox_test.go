package candidatehook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"recalldock.local/recalldock/internal/store"
)

func candidateHookPayload(statement string) []byte {
	payload, _ := json.Marshal(map[string]any{
		"session_id":             "hook-session",
		"project":                "hook-project",
		"last_assistant_message": "transcript-secret-that-must-not-persist",
		"memory_candidates": []any{map[string]any{
			"category":   store.CandidateCategoryVerifiedOutcome,
			"statement":  statement,
			"confidence": 0.95,
			"evidence_refs": []any{map[string]any{
				"kind": "test", "ref": "go-test/hook", "digest": strings.Repeat("c", 64),
			}},
		}},
	})
	return payload
}

func TestRunQueuesBoundedRedactedPayloadOnTimeoutAndReplays(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	slow := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(100 * time.Millisecond)
	}))
	defer slow.Close()
	outboxPath := filepath.Join(t.TempDir(), "candidate-outbox.json")
	cfg := Config{Endpoint: slow.URL, OutboxPath: outboxPath, Timeout: 10 * time.Millisecond, TTL: time.Hour, Capacity: 2, Now: func() time.Time { return now }}

	if err := Run(context.Background(), candidateHookPayload("verified hook outcome"), cfg); err != nil {
		t.Fatalf("Run timeout fallback: %v", err)
	}
	data, err := os.ReadFile(outboxPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(data), "last_assistant_message") || strings.Contains(string(data), "transcript-secret") {
		t.Fatalf("raw hook transcript leaked into outbox: %s", data)
	}

	requests := 0
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"promoted":1,"review":0,"rejected":0,"candidate_ids":["cand-1"]}`))
	}))
	defer fast.Close()
	cfg.Endpoint = fast.URL
	cfg.Timeout = time.Second
	if err := Run(context.Background(), candidateHookPayload("verified hook outcome"), cfg); err != nil {
		t.Fatalf("Run replay: %v", err)
	}
	if requests != 1 {
		t.Fatalf("dedupe should replay one queued record, got %d requests", requests)
	}
	if _, err := os.Stat(outboxPath); !os.IsNotExist(err) {
		t.Fatalf("successful replay should remove empty outbox, stat err=%v", err)
	}
}

func TestOutboxDedupeTTLAndCapacity(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "candidate-outbox.json")
	box := NewOutbox(path, 2, time.Hour, func() time.Time { return now })

	a := CommitRequest{SessionID: "s", Project: "p", Candidates: []store.MemoryCandidateInput{{Category: store.CandidateCategoryDecision, Statement: "a", Confidence: 0.2}}}
	b := CommitRequest{SessionID: "s", Project: "p", Candidates: []store.MemoryCandidateInput{{Category: store.CandidateCategoryDecision, Statement: "b", Confidence: 0.2}}}
	c := CommitRequest{SessionID: "s", Project: "p", Candidates: []store.MemoryCandidateInput{{Category: store.CandidateCategoryDecision, Statement: "c", Confidence: 0.2}}}
	if err := box.Enqueue(a); err != nil {
		t.Fatalf("enqueue a: %v", err)
	}
	if err := box.Enqueue(a); err != nil {
		t.Fatalf("dedupe a: %v", err)
	}
	if err := box.Enqueue(b); err != nil {
		t.Fatalf("enqueue b: %v", err)
	}
	if err := box.Enqueue(c); err != nil {
		t.Fatalf("enqueue c: %v", err)
	}
	records, err := box.Load()
	if err != nil {
		t.Fatalf("load bounded box: %v", err)
	}
	if len(records) != 2 || records[0].Request.Candidates[0].Statement != "b" || records[1].Request.Candidates[0].Statement != "c" {
		t.Fatalf("capacity should keep newest distinct records: %+v", records)
	}

	box.now = func() time.Time { return now.Add(2 * time.Hour) }
	records, err = box.Load()
	if err != nil {
		t.Fatalf("load expired box: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("expired records retained: %+v", records)
	}
}

func TestRunDropsSecretCandidateInsteadOfWritingOutbox(t *testing.T) {
	path := filepath.Join(t.TempDir(), "candidate-outbox.json")
	cfg := Config{Endpoint: "http://127.0.0.1:1", OutboxPath: path, Timeout: time.Millisecond, TTL: time.Hour, Capacity: 4}
	if err := Run(context.Background(), candidateHookPayload("api_key=sk-test-abcdefghijklmnopqrstuvwxyz123456"), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("secret candidate must not reach outbox, stat err=%v", err)
	}
}
