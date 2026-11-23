package opsstore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestAntigravityWriterUsesRegisteredConversationAndRejectsForeignRoot(t *testing.T) {
	s, _ := openTestStore(t)
	root, other := t.TempDir(), t.TempDir()
	id := "antigravity-conversation"
	if err := s.RegisterHostSession(id, "antigravity", root); err != nil {
		t.Fatal(err)
	}
	writer := Writer{Host: "antigravity", HostSessionID: id, GatewaySessionID: "gateway-antigravity"}
	bound := s.WithWriter(writer)
	if _, err := bound.Save(other, "lane", 0, fixture()); !errors.Is(err, ErrHostSession) {
		t.Fatalf("foreign root accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(other, ".recalldock")); !os.IsNotExist(err) {
		t.Fatal("rejected writer initialized foreign root")
	}
	snap, err := bound.Save(root, "lane", 0, fixture())
	if err != nil || snap.Writer == nil || *snap.Writer != writer {
		t.Fatalf("registered Antigravity writer lost: %+v %v", snap, err)
	}
	if err := s.EndHostSession(id, "antigravity", root); err != nil {
		t.Fatal(err)
	}
	if _, err := bound.Save(root, "lane", 1, fixture()); !errors.Is(err, ErrHostSession) {
		t.Fatalf("ended conversation still writes: %v", err)
	}
}

func TestHostSessionMissingRootErrorKeepsInput(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	_, _, err := canonicalRoot(missing)
	if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), missing) || strings.Contains(err.Error(), "lstat") {
		t.Fatalf("missing-root error lost the selected path or exposed OS details: %v", err)
	}
}

func TestHostSessionLifecycleIsIdempotentAndIndependent(t *testing.T) {
	s, dir := openTestStore(t)
	root := t.TempDir()
	id, host := "claude-code-real-session", "claude-code"
	if err := s.RegisterHostSession(id, host, root); err != nil {
		t.Fatal(err)
	}
	var started string
	if err := s.db.QueryRow("SELECT started_at FROM host_sessions WHERE id=?", id).Scan(&started); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterHostSession(id, host, root); err != nil {
		t.Fatal(err)
	}
	var repeated string
	if err := s.db.QueryRow("SELECT started_at FROM host_sessions WHERE id=?", id).Scan(&repeated); err != nil || repeated != started {
		t.Fatalf("start retry changed metadata: %q %q %v", started, repeated, err)
	}
	if err := s.ValidateHostSession(id, host); err != nil {
		t.Fatal(err)
	}
	if err := s.EndHostSession("claude-code-missing", host, root); err != nil {
		t.Fatal(err)
	}
	var sessions, workspaces int
	if err := s.db.QueryRow("SELECT (SELECT count(*) FROM host_sessions),(SELECT count(*) FROM workspaces)").Scan(&sessions, &workspaces); err != nil || sessions != 1 || workspaces != 0 {
		t.Fatalf("registration initialized task state: %d %d %v", sessions, workspaces, err)
	}
	for _, path := range []string{filepath.Join(root, ".recalldock"), filepath.Join(dir, "recalldock.db")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("registration created unrelated state: %s %v", path, err)
		}
	}
	if err := s.EndHostSession(id, host, root); err != nil {
		t.Fatal(err)
	}
	var ended string
	if err := s.db.QueryRow("SELECT ended_at FROM host_sessions WHERE id=?", id).Scan(&ended); err != nil {
		t.Fatal(err)
	}
	if err := s.EndHostSession(id, host, root); err != nil {
		t.Fatal(err)
	}
	var endedAgain string
	if err := s.db.QueryRow("SELECT ended_at FROM host_sessions WHERE id=?", id).Scan(&endedAgain); err != nil || endedAgain != ended {
		t.Fatalf("end retry changed metadata: %q %q %v", ended, endedAgain, err)
	}
	if err := s.ValidateHostSession(id, host); err == nil {
		t.Fatal("ended host session remained active")
	}
	if err := s.RegisterHostSession(id, host, root); err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateHostSession(id, host); err != nil {
		t.Fatalf("host resume failed: %v", err)
	}
}

func TestHostSessionRejectsForeignIdentityAndInvalidMetadata(t *testing.T) {
	s, _ := openTestStore(t)
	root, other := t.TempDir(), t.TempDir()
	id, host := "codex-session", "codex"
	for _, invalid := range []struct{ id, host, root string }{
		{"", host, root}, {"codex-", host, root}, {"claude-code-session", host, root},
		{id, "unknown", root}, {id, host, "relative"}, {"codex-bad\nvalue", host, root},
	} {
		if err := s.RegisterHostSession(invalid.id, invalid.host, invalid.root); err == nil {
			t.Fatalf("invalid registration accepted: %+v", invalid)
		}
	}
	if err := s.RegisterHostSession(id, host, root); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterHostSession(id, host, other); err == nil {
		t.Fatal("session moved to foreign root")
	}
	if err := s.EndHostSession(id, host, other); err == nil {
		t.Fatal("foreign root ended session")
	}
	if err := s.ValidateHostSession(id, "claude-code"); err == nil {
		t.Fatal("foreign host validated session")
	}
	if err := s.ValidateHostSession(id, host); err != nil {
		t.Fatalf("rejected calls changed session: %v", err)
	}
	moved := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterHostSession(id, host, root); err == nil {
		t.Fatal("recreated directory reclaimed host session")
	}
	if err := s.ValidateHostSession(id, host); err == nil {
		t.Fatal("startup validation accepted recreated directory")
	}
}

func TestWriterContextsRemainIsolatedAndHistoryRetainsOrigin(t *testing.T) {
	s, _ := openTestStore(t)
	root := t.TempDir()
	first := Writer{Host: "claude-code", GatewaySessionID: "gateway-first"}
	second := Writer{Host: "codex", GatewaySessionID: "gateway-second"}
	one, two := s.WithWriter(first), s.WithWriter(second)
	first.GatewaySessionID = "gateway-modified-input"
	var group sync.WaitGroup
	for lane, context := range map[string]*Store{"first": one, "second": two} {
		group.Add(1)
		go func(lane string, context *Store) {
			defer group.Done()
			if _, err := context.Save(root, lane, 0, fixture()); err != nil {
				t.Error(err)
			}
		}(lane, context)
	}
	group.Wait()
	for lane, expected := range map[string]Writer{"first": {Host: "claude-code", GatewaySessionID: "gateway-first"}, "second": second} {
		snap, err := s.Read(root, lane, 1)
		if err != nil || snap.Writer == nil || *snap.Writer != expected {
			t.Fatalf("writer context leaked: %+v %v", snap, err)
		}
		for _, mode := range []string{"l0", "l1", "l2"} {
			body, err := EncodeSnapshot(snap, mode)
			if err != nil || !strings.Contains(string(body), expected.GatewaySessionID) {
				t.Fatalf("writer missing from %s: %s %v", mode, body, err)
			}
		}
		history, err := s.History(root, lane, 0, 5)
		if err != nil || len(history.Tasks) != 1 || !reflect.DeepEqual(history.Tasks[0].Writer, snap.Writer) {
			t.Fatalf("history omitted writer: %+v %v", history, err)
		}
		snap.Writer.GatewaySessionID = "gateway-modified-output"
	}
	page, err := s.Find(root, FindOptions{Limit: 20})
	if err != nil || len(page.Tasks) != 2 || page.Tasks[0].Writer == nil || page.Tasks[1].Writer == nil {
		t.Fatalf("index omitted writers: %+v %v", page, err)
	}
	snap, err := one.Save(root, "first", 1, fixture())
	if err != nil || snap.Writer == nil || snap.Writer.GatewaySessionID != "gateway-first" {
		t.Fatalf("snapshot mutation changed context: %+v %v", snap, err)
	}
	unknown, err := s.Save(root, "unbound", 0, fixture())
	if err != nil || unknown.Writer != nil {
		t.Fatalf("shared store inherited gateway writer: %+v %v", unknown, err)
	}
}

func TestWriterCASAndReplayPreserveOriginalAttribution(t *testing.T) {
	s, _ := openTestStore(t)
	root := t.TempDir()
	first := Writer{Host: "codex", GatewaySessionID: "gateway-first"}
	second := Writer{Host: "codex", GatewaySessionID: "gateway-second"}
	one, two := s.WithWriter(first), s.WithWriter(second)
	if _, err := one.Save(root, "lane", 0, fixture()); err != nil {
		t.Fatal(err)
	}
	event := Event{ID: "operation", InputFingerprint: "fp", Status: "started", Summary: "Synthetic operation"}
	applied, err := one.Append(root, "lane", 1, event, Patch{})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := two.Append(root, "lane", 1, event, Patch{})
	if err != nil || !replay.Replayed || replay.Generation != applied.Generation || replay.Writer == nil || *replay.Writer != first {
		t.Fatalf("retry replaced original writer: %+v %v", replay, err)
	}
	if _, err := two.Save(root, "lane", 1, fixture()); !errors.Is(err, ErrGeneration) {
		t.Fatalf("attribution bypassed CAS: %v", err)
	}
	event.InputFingerprint = "other"
	if _, err := two.Append(root, "lane", 2, event, Patch{}); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("attribution bypassed fingerprint conflict: %v", err)
	}
	old, err := s.Read(root, "lane", 2)
	if err != nil || old.Writer == nil || *old.Writer != first {
		t.Fatalf("rejected writes modified history: %+v %v", old, err)
	}
}

func TestBoundWriterValidatesEveryWriteWithoutSideEffects(t *testing.T) {
	s, _ := openTestStore(t)
	root, other := t.TempDir(), t.TempDir()
	writer := Writer{Host: "claude-code", HostSessionID: "claude-code-session", GatewaySessionID: "gateway-one"}
	bound := s.WithWriter(writer)
	if _, err := bound.Save(root, "lane", 0, fixture()); err == nil {
		t.Fatal("unregistered session wrote checkpoint")
	}
	if _, err := os.Stat(filepath.Join(root, ".recalldock")); !os.IsNotExist(err) {
		t.Fatal("rejected writer created root marker")
	}
	if err := s.RegisterHostSession(writer.HostSessionID, writer.Host, root); err != nil {
		t.Fatal(err)
	}
	if _, err := bound.Save(other, "lane", 0, fixture()); err == nil {
		t.Fatal("bound writer wrote foreign root")
	}
	if _, err := os.Stat(filepath.Join(other, ".recalldock")); !os.IsNotExist(err) {
		t.Fatal("foreign write created root marker")
	}
	if _, err := bound.Save(root, "lane", 0, fixture()); err != nil {
		t.Fatal(err)
	}
	event := Event{ID: "operation", InputFingerprint: "fp", Status: "started", Summary: "Synthetic operation"}
	if _, err := bound.Append(root, "lane", 1, event, Patch{}); err != nil {
		t.Fatal(err)
	}
	if err := s.EndHostSession(writer.HostSessionID, writer.Host, root); err != nil {
		t.Fatal(err)
	}
	if _, err := bound.Save(root, "lane", 2, fixture()); err == nil {
		t.Fatal("ended host wrote checkpoint")
	}
	if _, err := bound.Append(root, "lane", 1, event, Patch{}); err == nil {
		t.Fatal("ended host replay bypassed binding validation")
	}
	snap, err := s.Read(root, "lane", 0)
	if err != nil || snap.Generation != 2 || snap.Writer == nil || *snap.Writer != writer {
		t.Fatalf("ended writes changed checkpoint: %+v %v", snap, err)
	}
}

func TestWriterSchemaMigrationKeepsOldReadonlyHistoryUnknown(t *testing.T) {
	dir, root := t.TempDir(), t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "operations.db"))
	if err != nil {
		t.Fatal(err)
	}
	legacy := strings.Replace(schema, "body TEXT NOT NULL, writer TEXT, created_at", "body TEXT NOT NULL, created_at", 1)
	if _, err := db.Exec(legacy); err != nil {
		t.Fatal(err)
	}
	identity, err := directoryIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(fixture())
	if _, err := db.Exec("INSERT INTO workspaces VALUES(?,?,?)", "ws-legacy", root, identity); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"INSERT INTO ops_metadata VALUES('kind','operations'),('schema_version','1'),('store_id','ops-legacy')",
		"INSERT INTO tasks VALUES('ws-legacy','lane',1,1,'Synthetic task','active',0,'2000-01-01T00:00:00Z')",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec("INSERT INTO checkpoints(id,workspace,lane,generation,goal,status,needs_check,body,created_at) VALUES(1,'ws-legacy','lane',1,'Synthetic task','active',0,?,'2000-01-01T00:00:00Z')", string(body)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".recalldock"), 0700); err != nil {
		t.Fatal(err)
	}
	marker, _ := json.Marshal(rootMarker{1, "ops-legacy", "ws-legacy", root, identity})
	if err := os.WriteFile(filepath.Join(root, ".recalldock", "ops-root.json"), marker, 0600); err != nil {
		t.Fatal(err)
	}
	old, err := Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = old.Close() })
	for _, mode := range []string{"read", "find", "history"} {
		switch mode {
		case "read":
			snap, err := old.Read(root, "lane", 1)
			if err != nil || snap.Writer != nil {
				t.Fatalf("readonly history acquired writer: %+v %v", snap, err)
			}
		case "find":
			page, err := old.Find(root, FindOptions{})
			if err != nil || len(page.Tasks) != 1 || page.Tasks[0].Writer != nil {
				t.Fatalf("readonly index: %+v %v", page, err)
			}
		case "history":
			page, err := old.History(root, "lane", 0, 5)
			if err != nil || len(page.Tasks) != 1 || page.Tasks[0].Writer != nil {
				t.Fatalf("readonly timeline: %+v %v", page, err)
			}
		}
	}
	var columns int
	if err := old.db.QueryRow("SELECT count(*) FROM pragma_table_info('checkpoints') WHERE name='writer'").Scan(&columns); err != nil || columns != 0 {
		t.Fatalf("readonly open migrated schema: %d %v", columns, err)
	}
	old.Close()
	upgraded, err := Open(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	var version, oldBody string
	if err := upgraded.db.QueryRow("SELECT value FROM ops_metadata WHERE key='schema_version'").Scan(&version); err != nil || version != "2" {
		t.Fatalf("writer migration version: %q %v", version, err)
	}
	if err := upgraded.db.QueryRow("SELECT body FROM checkpoints WHERE id=1").Scan(&oldBody); err != nil || oldBody != string(body) {
		t.Fatalf("migration rewrote checkpoint: %v", err)
	}
	writer := Writer{Host: "codex", GatewaySessionID: "gateway-new"}
	if _, err := upgraded.WithWriter(writer).Save(root, "lane", 1, fixture()); err != nil {
		t.Fatal(err)
	}
	snap, err := upgraded.Read(root, "lane", 1)
	if err != nil || snap.Writer != nil {
		t.Fatalf("migration backfilled historical writer: %+v %v", snap, err)
	}
	latest, err := upgraded.Read(root, "lane", 0)
	if err != nil || latest.Writer == nil || *latest.Writer != writer {
		t.Fatalf("new generation missing writer: %+v %v", latest, err)
	}
}

func TestWriterIncludedInCheckpointBudget(t *testing.T) {
	s, _ := openTestStore(t)
	root := t.TempDir()
	writer := Writer{Host: "claude-code", HostSessionID: "claude-code-" + strings.Repeat("a", 180), GatewaySessionID: "gateway-" + strings.Repeat("b", 184)}
	if err := s.RegisterHostSession(writer.HostSessionID, writer.Host, root); err != nil {
		t.Fatal(err)
	}
	cp := fixture()
	found := false
	for n := DetailLimit; n > DetailLimit-2000; n-- {
		cp.Details = strings.Repeat("x", n)
		if ValidateCheckpoint(root, "lane", cp) != nil {
			continue
		}
		_, err := EncodeSnapshot(Snapshot{Root: root, Lane: "lane", ID: 9223372036854775807, Generation: 9223372036854775807, UpdatedAt: "2000-01-01T00:00:00.000000000Z", Checkpoint: cp, Writer: &writer}, "l2")
		if errors.Is(err, ErrBudget) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("failed to construct attribution budget boundary")
	}
	if _, err := s.WithWriter(writer).Save(root, "lane", 0, cp); !errors.Is(err, ErrBudget) {
		t.Fatalf("writer metadata exceeded L2 budget: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".recalldock")); !os.IsNotExist(err) {
		t.Fatal("over-budget writer created workspace marker")
	}
}
