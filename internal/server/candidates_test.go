package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"recalldock.local/recalldock/internal/store"
)

func TestCommitSessionCandidatesRouteReturnsBoundedEnvelope(t *testing.T) {
	st := newServerTestStore(t)
	if err := st.CreateSession("hook-session", "engram", t.TempDir()); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	statement := "HTTP candidate evidence is admitted"
	body := `{"project":"engram","candidates":[{"category":"decision","statement":"` + statement + `","confidence":0.95,"evidence_refs":[{"kind":"test","ref":"go-test/server-candidate","digest":"` + strings.Repeat("d", 64) + `"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/sessions/hook-session/candidates", strings.NewReader(body))
	rec := httptest.NewRecorder()
	New(st, 0).Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), statement) || strings.Contains(rec.Body.String(), "evidence_refs") {
		t.Fatalf("response leaked candidate body: %s", rec.Body.String())
	}
	var result struct {
		Promoted     int      `json:"promoted"`
		CandidateIDs []string `json:"candidate_ids"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result.Promoted != 0 || len(result.CandidateIDs) != 1 {
		t.Fatalf("unexpected response: %+v", result)
	}
	candidate, err := st.GetMemoryCandidate(result.CandidateIDs[0])
	if err != nil {
		t.Fatalf("GetMemoryCandidate: %v", err)
	}
	if candidate.Status != store.CandidateStatusReview {
		t.Fatalf("candidate=%+v", candidate)
	}
}

func TestCommitSessionCandidatesRouteRejectsProjectMismatch(t *testing.T) {
	st := newServerTestStore(t)
	if err := st.CreateSession("hook-session", "engram", t.TempDir()); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/sessions/hook-session/candidates", strings.NewReader(`{"project":"other","candidates":[{"category":"decision","statement":"x","confidence":0.5}]}`))
	rec := httptest.NewRecorder()
	New(st, 0).Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
