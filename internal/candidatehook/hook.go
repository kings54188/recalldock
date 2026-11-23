package candidatehook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"recalldock.local/recalldock/internal/memorypolicy"
	"recalldock.local/recalldock/internal/store"
)

type Config struct {
	Endpoint   string
	OutboxPath string
	Timeout    time.Duration
	TTL        time.Duration
	Capacity   int
	Now        func() time.Time
	Client     *http.Client
}

type hookInput struct {
	SessionID        string                       `json:"session_id"`
	Project          string                       `json:"project,omitempty"`
	MemoryCandidates []store.MemoryCandidateInput `json:"memory_candidates"`
}

func Run(ctx context.Context, input []byte, cfg Config) error {
	if strings.TrimSpace(cfg.Endpoint) != "" {
		if err := validateLocalEndpoint(cfg.Endpoint); err != nil {
			return err
		}
	}
	if len(input) > 64<<10 {
		return fmt.Errorf("candidate hook input exceeds local size limit")
	}
	var hook hookInput
	if err := json.Unmarshal(input, &hook); err != nil {
		return nil
	}
	hook.SessionID = strings.TrimSpace(hook.SessionID)
	if hook.SessionID == "" || len(hook.MemoryCandidates) == 0 {
		return nil
	}
	if err := memorypolicy.ValidateProposalMetadata(map[string]any{"session_id": hook.SessionID, "project": hook.Project}); err != nil {
		return err
	}
	if len(hook.MemoryCandidates) > store.MaxCandidatesPerCommit {
		hook.MemoryCandidates = hook.MemoryCandidates[:store.MaxCandidatesPerCommit]
	}
	safe := make([]store.MemoryCandidateInput, 0, len(hook.MemoryCandidates))
	for _, candidate := range hook.MemoryCandidates {
		if store.CandidateSafeForOutbox(candidate) {
			safe = append(safe, candidate)
		}
	}
	if len(safe) == 0 {
		return nil
	}
	request := CommitRequest{SessionID: hook.SessionID, Project: strings.TrimSpace(hook.Project), Candidates: safe}
	box := NewOutbox(cfg.OutboxPath, cfg.Capacity, cfg.TTL, cfg.Now)
	if err := box.Enqueue(request); err != nil {
		return err
	}
	records, err := box.Load()
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	client := localHTTPClient(cfg.Client, timeout)
	defer client.CloseIdleConnections()
	completed := make(map[string]struct{})
	for _, record := range records {
		requestCtx, cancel := context.WithTimeout(ctx, timeout)
		err := postCommit(requestCtx, client, cfg.Endpoint, record.Request)
		cancel()
		if err != nil {
			break
		}
		completed[record.ID] = struct{}{}
	}
	return box.Remove(completed)
}

func postCommit(ctx context.Context, client *http.Client, endpoint string, request CommitRequest) error {
	if err := validateLocalEndpoint(endpoint); err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"project": request.Project, "candidates": request.Candidates})
	if err != nil {
		return err
	}
	endpoint = strings.TrimRight(endpoint, "/") + "/sessions/" + url.PathEscape(request.SessionID) + "/candidates"
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	response, err := client.Do(httpRequest)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("candidate commit returned HTTP %d", response.StatusCode)
	}
	return nil
}
