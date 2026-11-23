package diagnostic

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"recalldock.local/recalldock/internal/store"
)

func TestSessionOriginCheckReportsProvenanceCounts(t *testing.T) {
	s := newDiagnosticTestStore(t)
	dir := t.TempDir()
	if err := s.CreateSessionWithOrigin("host-session", "engram", dir, store.SessionOriginHost); err != nil {
		t.Fatal(err)
	}
	// A model session needs an established binding, so bind this fixture dir
	// and record the model-origin row under the registered id.
	proj, err := s.CreateRegisteredProject("diagnostic origin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindProjectRoot(proj.ID, dir); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSessionWithOrigin("model-session", proj.ID, dir, store.SessionOriginModel); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSessionWithOrigin("legacy-session", "engram", dir, ""); err != nil {
		t.Fatal(err)
	}
	report, err := NewRunner().RunOne(context.Background(), Scope{Store: s, Now: time.Now()}, CheckSessionOrigin)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != StatusOK {
		t.Fatalf("status=%s", report.Status)
	}
	var evidence struct {
		Total    int `json:"total"`
		Host     int `json:"host"`
		Model    int `json:"model"`
		Unknown  int `json:"unknown"`
		Operator int `json:"operator"`
	}
	if err := json.Unmarshal(report.Checks[0].Evidence, &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence.Total != 3 || evidence.Host != 1 || evidence.Model != 1 || evidence.Unknown != 1 {
		t.Fatalf("evidence=%+v", evidence)
	}
}

func TestNetworkBoundaryCheckNeverDialsButReportsConfig(t *testing.T) {
	s := newDiagnosticTestStore(t)
	t.Setenv("RECALLDOCK_CLOUD_URL", "https://example.invalid")
	report, err := NewRunner().RunOne(context.Background(), Scope{Store: s, Now: time.Now()}, CheckNetworkBoundary)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != StatusOK {
		t.Fatalf("status=%s", report.Status)
	}
	var evidence struct {
		CloudEnv []string `json:"cloud_env_present"`
		Update   string   `json:"update_check"`
		Note     string   `json:"note"`
	}
	if err := json.Unmarshal(report.Checks[0].Evidence, &evidence); err != nil {
		t.Fatal(err)
	}
	if len(evidence.CloudEnv) != 1 || evidence.CloudEnv[0] != "RECALLDOCK_CLOUD_URL" {
		t.Fatalf("cloud env=%v", evidence.CloudEnv)
	}
	if evidence.Note == "" {
		t.Fatal("expected the application-boundary note")
	}
}
