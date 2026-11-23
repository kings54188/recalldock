package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestPublicHTTPRejectsSessionOwnershipCollision(t *testing.T) {
	s := newServerTestStore(t)
	handler := New(s, 0).Handler()
	post := func(project, directory string) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(map[string]string{"id": "http-session", "project": project, "directory": directory})
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRecorder()
		handler.ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(string(body))))
		return r
	}
	root := t.TempDir()
	proj, err := s.CreateRegisteredProject("http session owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindProjectRoot(proj.ID, root); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if r := post(proj.ID, root); r.Code != http.StatusCreated {
			t.Fatalf("matching registration: %d %s", r.Code, r.Body.String())
		}
	}
	before, err := s.GetSession("http-session")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateRegisteredProject("http session other")
	if err != nil {
		t.Fatal(err)
	}
	otherRoot := t.TempDir()
	if _, err := s.BindProjectRoot(other.ID, otherRoot); err != nil {
		t.Fatal(err)
	}
	for _, owner := range [][2]string{{other.ID, otherRoot}, {proj.ID, t.TempDir()}} {
		r := post(owner[0], owner[1])
		if r.Code != http.StatusConflict {
			t.Errorf("collision not actionable: %d %s", r.Code, r.Body.String())
			continue
		}
		if !strings.Contains(r.Body.String(), "session_scope_mismatch") && !strings.Contains(r.Body.String(), "binding_unverified") {
			t.Errorf("collision code unexpected: %d %s", r.Code, r.Body.String())
		}
	}
	after, err := s.GetSession("http-session")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("collision changed session: %+v %v", after, err)
	}
}
