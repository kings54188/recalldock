package store

import (
	"database/sql"
	"encoding/json"
	"testing"
)

func TestSessionOriginRecordedAndUpgraded(t *testing.T) {
	s := newTestStore(t)
	dir := t.TempDir()

	if err := s.CreateSessionWithOrigin("sess-model", "origin-proj", dir, SessionOriginOperator); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSession("sess-model")
	if err != nil {
		t.Fatal(err)
	}
	if got.Origin != SessionOriginOperator {
		t.Fatalf("origin=%q want operator", got.Origin)
	}

	// A weaker later registration must not downgrade recorded provenance.
	if err := s.CreateSessionWithOrigin("sess-model", "origin-proj", dir, SessionOriginUnknown); err != nil {
		t.Fatal(err)
	}
	if got, err = s.GetSession("sess-model"); err != nil {
		t.Fatal(err)
	}
	if got.Origin != SessionOriginOperator {
		t.Fatalf("unknown registration downgraded origin to %q", got.Origin)
	}

	// A host hook re-registering the same id is stronger evidence and upgrades.
	if err := s.CreateSessionWithOrigin("sess-model", "origin-proj", dir, SessionOriginHost); err != nil {
		t.Fatal(err)
	}
	if got, err = s.GetSession("sess-model"); err != nil {
		t.Fatal(err)
	}
	if got.Origin != SessionOriginHost {
		t.Fatalf("host registration did not upgrade origin: %q", got.Origin)
	}

	// Legacy CreateSession leaves the explicit unknown marker.
	if err := s.CreateSession("sess-legacy", "origin-proj", dir); err != nil {
		t.Fatal(err)
	}
	if got, err = s.GetSession("sess-legacy"); err != nil {
		t.Fatal(err)
	}
	if got.Origin != SessionOriginUnknown {
		t.Fatalf("legacy origin=%q want unknown", got.Origin)
	}
}

func TestSessionOriginStatsAndValidation(t *testing.T) {
	s := newTestStore(t)
	dir := t.TempDir()
	// A model session needs an established binding, so this fixture binds the
	// directory and uses its registered id for the model-origin row.
	proj, err := s.CreateRegisteredProject("origin stats")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindProjectRoot(proj.ID, dir); err != nil {
		t.Fatal(err)
	}
	for id, origin := range map[string]string{
		"a": SessionOriginHost, "b": SessionOriginModel, "c": SessionOriginOperator, "d": "",
	} {
		project := proj.ID
		if origin != SessionOriginModel {
			project = "origin-stats"
		}
		if err := s.CreateSessionWithOrigin(id, project, dir, origin); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := s.SessionOriginStats("origin-stats")
	if err != nil {
		t.Fatal(err)
	}
	modelStats, err := s.SessionOriginStats(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stats[SessionOriginHost] != 1 || stats[SessionOriginOperator] != 1 || stats[SessionOriginUnknown] != 1 {
		t.Fatalf("stats=%v", stats)
	}
	if modelStats[SessionOriginModel] != 1 {
		t.Fatalf("model stats=%v", modelStats)
	}
	if NormalizeSessionOrigin(" HOST ") != SessionOriginHost {
		t.Fatal("normalization failed")
	}
	if NormalizeSessionOrigin("root") != SessionOriginUnknown {
		t.Fatal("unexpected origin accepted as trusted")
	}
}

func TestSessionOriginTravelsInSyncPayload(t *testing.T) {
	s := newTestStore(t)
	dir := t.TempDir()
	// Sessions are only replicated once their project is enrolled for sync.
	if err := s.EnrollProject("origin-sync"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSessionWithOrigin("sync-origin", "origin-sync", dir, SessionOriginHost); err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.Query(`SELECT payload FROM sync_mutations WHERE entity = 'session' AND entity_key = 'sync-origin' ORDER BY seq DESC LIMIT 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no session sync mutation enqueued")
	}
	var raw string
	if err := rows.Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var payload syncSessionPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Origin != SessionOriginHost {
		t.Fatalf("sync payload origin=%q", payload.Origin)
	}
}

func TestApplySessionPayloadKeepsOriginPrecedence(t *testing.T) {
	s := newTestStore(t)
	dir := t.TempDir()
	if err := s.CreateSessionWithOrigin("apply-origin", "origin-apply", dir, SessionOriginOperator); err != nil {
		t.Fatal(err)
	}
	// Applying a host payload through the sync apply path upgrades the origin.
	payload := syncSessionPayload{ID: "apply-origin", Project: "origin-apply", Directory: dir, Origin: SessionOriginHost}
	if err := s.withTx(func(tx *sql.Tx) error { return s.applySessionPayloadTx(tx, payload) }); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSession("apply-origin")
	if err != nil {
		t.Fatal(err)
	}
	if got.Origin != SessionOriginHost {
		t.Fatalf("apply did not upgrade origin: %q", got.Origin)
	}
	// A later model payload must not downgrade the host attestation.
	payload.Origin = SessionOriginModel
	if err := s.withTx(func(tx *sql.Tx) error { return s.applySessionPayloadTx(tx, payload) }); err != nil {
		t.Fatal(err)
	}
	if got, err = s.GetSession("apply-origin"); err != nil {
		t.Fatal(err)
	}
	if got.Origin != SessionOriginHost {
		t.Fatalf("apply downgraded origin: %q", got.Origin)
	}
}
