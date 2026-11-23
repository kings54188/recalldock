package candidatehook

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"recalldock.local/recalldock/internal/store"
)

const (
	defaultOutboxCapacity = 32
	defaultOutboxTTL      = 7 * 24 * time.Hour
)

var outboxMu sync.Mutex

type CommitRequest struct {
	SessionID  string                       `json:"session_id"`
	Project    string                       `json:"project,omitempty"`
	Candidates []store.MemoryCandidateInput `json:"candidates"`
}

type OutboxRecord struct {
	ID        string        `json:"id"`
	CreatedAt string        `json:"created_at"`
	ExpiresAt string        `json:"expires_at"`
	Request   CommitRequest `json:"request"`
}

type outboxFile struct {
	Schema  string         `json:"schema"`
	Records []OutboxRecord `json:"records"`
}

type Outbox struct {
	path     string
	capacity int
	ttl      time.Duration
	now      func() time.Time
}

func NewOutbox(path string, capacity int, ttl time.Duration, now func() time.Time) *Outbox {
	if capacity <= 0 {
		capacity = defaultOutboxCapacity
	}
	if ttl <= 0 {
		ttl = defaultOutboxTTL
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Outbox{path: path, capacity: capacity, ttl: ttl, now: now}
}

func (o *Outbox) Enqueue(request CommitRequest) error {
	outboxMu.Lock()
	defer outboxMu.Unlock()
	records, err := o.loadUnlocked()
	if err != nil {
		return err
	}
	id, err := requestID(request)
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.ID == id {
			return o.saveUnlocked(records)
		}
	}
	now := o.now().UTC()
	records = append(records, OutboxRecord{
		ID: id, CreatedAt: now.Format(time.RFC3339Nano), ExpiresAt: now.Add(o.ttl).Format(time.RFC3339Nano), Request: request,
	})
	if len(records) > o.capacity {
		records = records[len(records)-o.capacity:]
	}
	return o.saveUnlocked(records)
}

func (o *Outbox) Load() ([]OutboxRecord, error) {
	outboxMu.Lock()
	defer outboxMu.Unlock()
	records, err := o.loadUnlocked()
	if err != nil {
		return nil, err
	}
	if err := o.saveUnlocked(records); err != nil {
		return nil, err
	}
	return records, nil
}

func (o *Outbox) Remove(ids map[string]struct{}) error {
	if len(ids) == 0 {
		return nil
	}
	outboxMu.Lock()
	defer outboxMu.Unlock()
	records, err := o.loadUnlocked()
	if err != nil {
		return err
	}
	kept := records[:0]
	for _, record := range records {
		if _, ok := ids[record.ID]; !ok {
			kept = append(kept, record)
		}
	}
	return o.saveUnlocked(kept)
}

func (o *Outbox) loadUnlocked() ([]OutboxRecord, error) {
	data, err := os.ReadFile(o.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var file outboxFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, err
	}
	now := o.now().UTC()
	records := make([]OutboxRecord, 0, len(file.Records))
	seen := make(map[string]struct{}, len(file.Records))
	for _, record := range file.Records {
		expires, err := time.Parse(time.RFC3339Nano, record.ExpiresAt)
		if err != nil || !expires.After(now) {
			continue
		}
		if _, ok := seen[record.ID]; ok {
			continue
		}
		seen[record.ID] = struct{}{}
		records = append(records, record)
	}
	sort.SliceStable(records, func(i, j int) bool { return records[i].CreatedAt < records[j].CreatedAt })
	if len(records) > o.capacity {
		records = records[len(records)-o.capacity:]
	}
	return records, nil
}

func (o *Outbox) saveUnlocked(records []OutboxRecord) error {
	if len(records) == 0 {
		if err := os.Remove(o.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(o.path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(outboxFile{Schema: "engram-candidate-outbox/v1", Records: records})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(o.path), ".candidate-outbox-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, o.path)
}

func requestID(request CommitRequest) (string, error) {
	data, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return "outbox-" + hex.EncodeToString(digest[:]), nil
}
