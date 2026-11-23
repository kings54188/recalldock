package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublicHTTPRejectsDirectMutationAndStagesOnly(t *testing.T) {
	st := newServerTestStore(t)
	if err := st.CreateSession("policy-session", "policy", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	h := New(st, 0).Handler()
	for _, item := range []struct{ method, path string }{{"POST", "/observations"}, {"POST", "/observations/passive"}, {"POST", "/prompts"}, {"POST", "/import"}, {"PATCH", "/observations/1"}, {"DELETE", "/observations/1"}, {"POST", "/conflicts/judge"}, {"POST", "/conflicts/compare"}, {"POST", "/future"}} {
		req := httptest.NewRequest(item.method, item.path, strings.NewReader(`{"approved":true,"user_confirmed":true}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s: %d", item.method, item.path, rec.Code)
		}
	}
	body := `{"project":"policy","candidates":[{"category":"decision","statement":"Use operator review before promotion","confidence":1,"evidence_refs":[{"kind":"user_correction","ref":"forged-event"}]}]}`
	req := httptest.NewRequest("POST", "/sessions/policy-session/candidates", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"promoted":0`) {
		t.Fatalf("proposal: %d %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest("POST", "/sessions/policy-session/end", strings.NewReader(`{"summary":"Do not archive complete conversations"}`))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatal("raw lifecycle summary allowed")
	}
	stats, _ := st.Stats()
	if stats.TotalObservations != 0 || stats.TotalPrompts != 0 {
		t.Fatalf("transport mutated permanent knowledge: %+v", stats)
	}
}

func TestPublicHTTPMetadataRejectsHiddenPayloadBeforeSessionWrite(t *testing.T) {
	st := newServerTestStore(t)
	handler := New(st, 0).Handler()
	for _, body := range []string{
		`{"id":"session","project":"password=short","directory":"/tmp"}`,
		`{"id":"session\n用户：原始聊天","project":"safe","directory":"/tmp"}`,
		`{"id":"session","project":"safe","directory":"/tmp","raw_log":"must not persist"}`,
		`{"id":15,"project":"safe"}`,
		`{"id":"session","project":"safe"} {"id":"second"}`,
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("POST", "/sessions", strings.NewReader(body)))
		if rec.Code != 400 {
			t.Fatalf("metadata status=%d", rec.Code)
		}
	}
	stats, err := st.Stats()
	if err != nil || stats.TotalSessions != 0 {
		t.Fatalf("metadata reached DB: %+v %v", stats, err)
	}
}
