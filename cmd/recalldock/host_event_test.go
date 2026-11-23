package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"recalldock.local/recalldock/internal/opsstore"
	"recalldock.local/recalldock/internal/store"
)

func TestHostEventCodexSubagentStartUsesNativeAgentID(t *testing.T) {
	cfg := testConfig(t)
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	s, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateRegisteredProject("subagent-host")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindProjectRoot(p.ID, root); err != nil {
		t.Fatal(err)
	}
	s.Close()
	var out bytes.Buffer
	parent, _ := json.Marshal(map[string]string{"session_id": "synthetic-parent", "cwd": root, "hook_event_name": "SessionStart"})
	if err := runHostEvent(cfg, []string{"codex"}, bytes.NewReader(parent), &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	event, _ := json.Marshal(map[string]string{"session_id": "synthetic-parent", "agent_id": "synthetic-child", "cwd": root, "hook_event_name": "SubagentStart", "prompt": "DO_NOT_STORE_SUBTASK"})
	if err := runHostEvent(cfg, []string{"codex"}, bytes.NewReader(event), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"hookEventName":"SubagentStart"`) || !strings.Contains(out.String(), "codex-synthetic-child") || strings.Contains(out.String(), "DO_NOT_STORE") {
		t.Fatalf("subagent context: %s", out.String())
	}
	s, err = store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	child, err := s.GetSession("codex-synthetic-child")
	if err != nil || child.Project != p.ID || child.Directory != root || child.Origin != store.SessionOriginHost || child.Summary != nil {
		t.Fatalf("child lifecycle: %+v %v", child, err)
	}
	parentSession, err := s.GetSession("codex-synthetic-parent")
	if err != nil || parentSession.EndedAt != nil || parentSession.Summary != nil {
		t.Fatalf("parent was changed: %+v %v", parentSession, err)
	}
	ops, err := opsstore.Open(cfg.DataDir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer ops.Close()
	if err := ops.ValidateHostSession("codex-synthetic-child", "codex"); err != nil {
		t.Fatalf("child Work lifecycle: %v", err)
	}
}

func TestHostEventCodexSubagentInvalidOrUnboundDoesNotCreateData(t *testing.T) {
	cfg := testConfig(t)
	cfg.DataDir = filepath.Join(cfg.DataDir, "absent")
	root := t.TempDir()
	for _, agent := range []any{"", 42, map[string]string{"id": "forged"}} {
		event, _ := json.Marshal(map[string]any{"session_id": "synthetic-parent", "agent_id": agent, "cwd": root, "hook_event_name": "SubagentStart"})
		if err := runHostEvent(cfg, []string{"codex"}, bytes.NewReader(event), &bytes.Buffer{}); err == nil {
			t.Fatalf("invalid agent identity accepted: %v", agent)
		}
	}
	event, _ := json.Marshal(map[string]string{"session_id": "synthetic-parent", "agent_id": "synthetic-child", "cwd": root, "hook_event_name": "SubagentStart"})
	if err := runHostEvent(cfg, []string{"codex", "--auto-project"}, bytes.NewReader(event), &bytes.Buffer{}); err == nil {
		t.Fatal("subagent auto-project accepted")
	}
	if err := runHostEvent(cfg, []string{"claude-code"}, bytes.NewReader(event), &bytes.Buffer{}); err == nil {
		t.Fatal("Codex subagent event accepted for another host")
	}
	var out bytes.Buffer
	if err := runHostEvent(cfg, []string{"codex"}, bytes.NewReader(event), &out); err != nil || out.Len() != 0 {
		t.Fatalf("unregistered subagent: %v %s", err, out.String())
	}
	if _, err := os.Stat(cfg.DataDir); !os.IsNotExist(err) {
		t.Fatalf("subagent created a knowledge store: %v", err)
	}
}

func TestHostEventMetadataOnly(t *testing.T) {
	cfg := testConfig(t)
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateRegisteredProject("host")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindProjectRoot(p.ID, root); err != nil {
		t.Fatal(err)
	}
	s.Close()
	event := func(name, cwd string) []byte {
		b, _ := json.Marshal(map[string]string{"session_id": "synthetic-test", "cwd": cwd, "hook_event_name": name, "prompt": "DO_NOT_STORE_PRIVATE_PROMPT", "transcript_path": "DO_NOT_STORE_TRANSCRIPT"})
		return b
	}
	var out bytes.Buffer
	if err := runHostEvent(cfg, []string{"codex"}, bytes.NewReader(event("SessionStart", nested)), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() > 512 || strings.Contains(out.String(), "DO_NOT_STORE") || !strings.Contains(out.String(), "codex-synthetic-test") {
		t.Fatalf("bad context: %s", out.String())
	}
	out.Reset()
	if err := runHostEvent(cfg, []string{"codex"}, bytes.NewReader(event("SessionStart", nested)), &out); err != nil {
		t.Fatal(err)
	}
	s, err = store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.GetSession("codex-synthetic-test")
	s.Close()
	if err != nil || session.Project != p.ID || session.Directory != root || session.Summary != nil {
		t.Fatalf("bad session: %+v %v", session, err)
	}
	out.Reset()
	if err := runHostEvent(cfg, []string{"codex"}, bytes.NewReader(event("SessionEnd", nested)), &out); err != nil || out.Len() != 0 {
		t.Fatalf("end: %v %s", err, out.String())
	}
	s, err = store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	session, err = s.GetSession("codex-synthetic-test")
	s.Close()
	if err != nil || session.EndedAt == nil || session.Summary != nil {
		t.Fatalf("bad end: %+v %v", session, err)
	}
	out.Reset()
	if err := runHostEvent(cfg, []string{"codex"}, bytes.NewReader(event("SessionStart", nested)), &out); err != nil {
		t.Fatal(err)
	}
	s, err = store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	session, err = s.GetSession("codex-synthetic-test")
	s.Close()
	if err != nil || session.EndedAt != nil {
		t.Fatalf("resume failed: %+v %v", session, err)
	}
}

func TestHostEventUnboundAndInvalid(t *testing.T) {
	cfg := testConfig(t)
	cfg.DataDir = filepath.Join(cfg.DataDir, "absent")
	var out bytes.Buffer
	valid, _ := json.Marshal(map[string]string{"session_id": "synthetic", "cwd": t.TempDir(), "hook_event_name": "SessionStart"})
	if err := runHostEvent(cfg, []string{"claude-code"}, bytes.NewReader(valid), &out); err != nil || out.Len() != 0 {
		t.Fatalf("unbound: %v", err)
	}
	if _, err := os.Stat(cfg.DataDir); !os.IsNotExist(err) {
		t.Fatalf("created data: %v", err)
	}
	if err := runHostEvent(cfg, []string{"codex"}, strings.NewReader(strings.Repeat("x", 16385)), &out); err == nil {
		t.Fatal("oversized accepted")
	}
	if err := runHostEvent(cfg, []string{"other"}, bytes.NewReader(valid), &out); err == nil {
		t.Fatal("unknown host accepted")
	}
}

func TestHostEventCommandReturnsFailureForInvalidInput(t *testing.T) {
	withArgs(t, "recalldock", "host-event", "unsupported-host")
	stubExitWithPanic(t)
	_, stderr, stopped := captureOutputAndRecover(t, main)
	if stopped != exitCode(1) || !strings.Contains(stderr, "unsupported host") {
		t.Fatalf("failed hook must return a failure status; got %v, %q", stopped, stderr)
	}
}

func TestHostEventLeavesMemoryRetrievalToAgent(t *testing.T) {
	cfg := testConfig(t)
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	s, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateRegisteredProject("host-context")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindProjectRoot(p.ID, root); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession("host-context-seed", p.ID, root); err != nil {
		t.Fatal(err)
	}
	for _, title := range []string{"bounded decision alpha", "bounded decision beta"} {
		if _, err := s.AddObservation(store.AddObservationParams{
			SessionID: "host-context-seed", Type: "decision", Title: title, Content: "body", Project: p.ID,
		}); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()

	event, _ := json.Marshal(map[string]string{"session_id": "context-test", "cwd": root, "hook_event_name": "SessionStart"})
	var out bytes.Buffer
	if err := runHostEvent(cfg, []string{"claude-code"}, bytes.NewReader(event), &out); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	context := payload.HookSpecificOutput.AdditionalContext
	if strings.Contains(context, "bounded decision alpha") || strings.Contains(context, "bounded decision beta") {
		t.Fatalf("startup loaded knowledge without a task: %s", context)
	}
	if !strings.Contains(context, "Work root "+root) || !strings.Contains(context, "only when useful") {
		t.Fatalf("startup omitted exact scope or optional-use guidance: %s", context)
	}
	if strings.Contains(context, "DO_NOT_STORE") {
		t.Fatal("hook leaked private prompt data")
	}
	if len(context) > 1024 {
		t.Fatalf("metadata context is not bounded: %d bytes", len(context))
	}
	s, err = store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	selected, err := s.HostContext(p.ID, store.HostContextBudget)
	if err != nil || !strings.Contains(selected.Text, "bounded decision alpha") {
		t.Fatalf("explicit bounded context unavailable: %+v %v", selected, err)
	}
}

func nativeStartHookArgs(t *testing.T, host string) []string {
	t.Helper()
	path := filepath.Join(repoRootFromCaller(t), "integrations/host-plugins/plugins", host, "hooks/hooks.json")
	key := "hooks"
	if host == "antigravity" {
		path = filepath.Join(repoRootFromCaller(t), "integrations/host-plugins/plugins", host, "hooks.json")
		key = "recalldock-session"
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal(body, &config); err != nil {
		t.Fatal(err)
	}
	var groups map[string][]struct {
		Command string `json:"command"`
		Hooks   []struct {
			Command string `json:"command"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(config[key], &groups); err != nil {
		t.Fatal(err)
	}
	command := groups["SessionStart"][0].Command
	if host != "antigravity" {
		command = groups["SessionStart"][0].Hooks[0].Command
	}
	at := strings.Index(command, "host-event ")
	if at < 0 {
		t.Fatalf("missing lifecycle command: %s", command)
	}
	args := strings.Fields(command[at+len("host-event "):])
	if len(args) == 0 || args[0] != host {
		t.Fatalf("wrong host command: %s", command)
	}
	return args
}

func TestNativeDefaultHooksDoNotProvisionUnregisteredConversations(t *testing.T) {
	for _, host := range []string{"codex", "claude-code", "antigravity"} {
		for _, message := range []string{"知道了", "今天随便聊聊", "仅检查连接是否正常", "收到回复ok"} {
			for _, initialized := range []bool{false, true} {
				t.Run(host+"/"+message+map[bool]string{false: "/new-store", true: "/existing-store"}[initialized], func(t *testing.T) {
					cfg := testConfig(t)
					root := t.TempDir()
					if initialized {
						s, err := store.New(cfg)
						if err != nil {
							t.Fatal(err)
						}
						s.Close()
					}
					payload := map[string]any{"session_id": "ordinary", "cwd": root, "hook_event_name": "SessionStart", "prompt": message}
					if host == "antigravity" {
						payload = map[string]any{"conversationId": "ordinary", "workspacePaths": []string{root}, "lastUserInput": message}
					}
					body, _ := json.Marshal(payload)
					var out bytes.Buffer
					if err := runHostEvent(cfg, nativeStartHookArgs(t, host), bytes.NewReader(body), &out); err != nil || out.Len() != 0 {
						t.Fatalf("unregistered ordinary conversation did not remain unbound: %v %s", err, out.String())
					}
					for _, marker := range []string{"root.json", "ops-root.json"} {
						if _, err := os.Lstat(filepath.Join(root, ".recalldock", marker)); !os.IsNotExist(err) {
							t.Fatalf("startup created %s: %v", marker, err)
						}
					}
					if !initialized {
						if _, err := os.Stat(filepath.Join(cfg.DataDir, "recalldock.db")); !os.IsNotExist(err) {
							t.Fatalf("startup initialized knowledge: %v", err)
						}
					} else {
						s, err := store.New(cfg)
						if err != nil {
							t.Fatal(err)
						}
						defer s.Close()
						projects, err := s.ListRegisteredProjects()
						if err != nil || len(projects) != 0 {
							t.Fatalf("startup created a project: %+v %v", projects, err)
						}
					}
					if _, err := os.Stat(filepath.Join(cfg.DataDir, "operations.db")); !os.IsNotExist(err) {
						t.Fatalf("startup created task state: %v", err)
					}
				})
			}
		}
	}
}

func TestHostEventSynchronizesTrustedOperationsLifecycle(t *testing.T) {
	cfg := testConfig(t)
	root := t.TempDir()
	id, host := "claude-code-trusted", "claude-code"
	event := func(name string) *bytes.Reader {
		body, _ := json.Marshal(map[string]string{"session_id": "trusted", "cwd": root, "hook_event_name": name})
		return bytes.NewReader(body)
	}
	var out bytes.Buffer
	if err := runHostEvent(cfg, []string{host, "--auto-project"}, event("SessionStart"), &out); err != nil {
		t.Fatal(err)
	}
	ops, err := opsstore.Open(cfg.DataDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := ops.ValidateHostSession(id, host); err != nil {
		t.Fatalf("hook omitted operations registration: %v", err)
	}
	ops.Close()
	if _, err := os.Stat(filepath.Join(root, ".recalldock", "ops-root.json")); !os.IsNotExist(err) {
		t.Fatal("host registration created task binding")
	}
	out.Reset()
	if err := runHostEvent(cfg, []string{host, "--auto-project"}, event("SessionEnd"), &out); err != nil || out.Len() != 0 {
		t.Fatalf("end hook failed: %v %s", err, out.String())
	}
	ops, err = opsstore.Open(cfg.DataDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := ops.ValidateHostSession(id, host); err == nil {
		t.Fatal("end hook left operations session active")
	}
	ops.Close()
	if err := runHostEvent(cfg, []string{host, "--auto-project"}, event("SessionStart"), &out); err != nil {
		t.Fatal(err)
	}
	ops, err = opsstore.Open(cfg.DataDir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer ops.Close()
	if err := ops.ValidateHostSession(id, host); err != nil {
		t.Fatalf("resume hook did not reactivate operations registration: %v", err)
	}
}

func TestHostEventCannotClaimExistingNonHostSession(t *testing.T) {
	for _, origin := range []string{store.SessionOriginModel, store.SessionOriginOperator, store.SessionOriginUnknown} {
		t.Run(origin, func(t *testing.T) {
			cfg := testConfig(t)
			root := t.TempDir()
			s, err := store.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			p, err := s.CreateRegisteredProject("origin")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.BindProjectRoot(p.ID, root); err != nil {
				t.Fatal(err)
			}
			if err := s.CreateSessionWithOrigin("codex-existing", p.ID, root, origin); err != nil {
				t.Fatal(err)
			}
			s.Close()
			for _, name := range []string{"SessionStart", "SessionEnd"} {
				body, _ := json.Marshal(map[string]string{"session_id": "existing", "cwd": root, "hook_event_name": name})
				var out bytes.Buffer
				if err := runHostEvent(cfg, []string{"codex"}, bytes.NewReader(body), &out); err == nil || out.Len() != 0 {
					t.Fatalf("non-host session claimed via %s: %v %s", name, err, out.String())
				}
			}
			if _, err := os.Stat(filepath.Join(cfg.DataDir, "operations.db")); !os.IsNotExist(err) {
				t.Fatal("rejected origin created operations database")
			}
			s, err = store.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			session, err := s.GetSession("codex-existing")
			if err != nil || session.Origin != origin || session.EndedAt != nil {
				t.Fatalf("rejected hook changed session provenance: %+v %v", session, err)
			}
		})
	}
}

func TestHostEventIgnoresUnrelatedOtherHostFields(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		args []string
		body map[string]any
	}{
		{[]string{"codex"}, map[string]any{"session_id": "native", "cwd": root, "hook_event_name": "SessionStart", "workspacePaths": 17, "conversationId": false}},
		{[]string{"antigravity", "--event", "SessionStart"}, map[string]any{"conversationId": "native", "workspacePaths": []string{root}, "hook_event_name": 17, "cwd": false, "session_id": 17}},
	}
	for _, tc := range cases {
		cfg := testConfig(t)
		body, err := json.Marshal(tc.body)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := runHostEvent(cfg, tc.args, bytes.NewReader(body), &out); err != nil {
			t.Fatalf("unrelated host fields blocked lifecycle: %v", err)
		}
		if out.Len() != 0 {
			t.Fatal("unconfigured host unexpectedly injected context")
		}
		if _, err := os.Stat(filepath.Join(cfg.DataDir, "recalldock.db")); !os.IsNotExist(err) {
			t.Fatal("unconfigured host initialized a database")
		}
	}
}

func TestAntigravityLifecycleRejectsAmbiguousOrUnsupportedMetadataBeforeWrites(t *testing.T) {
	root, other := t.TempDir(), t.TempDir()
	cases := []struct {
		args []string
		body map[string]any
	}{
		{[]string{"antigravity", "--auto-project"}, map[string]any{"conversationId": "native", "workspacePaths": []string{root}, "hookEventName": "SessionStart"}},
		{[]string{"antigravity", "--auto-project", "--event", "SessionEnd"}, map[string]any{"conversationId": "native", "workspacePaths": []string{root}}},
		{[]string{"antigravity", "--auto-project", "--event", "SessionStart"}, map[string]any{"conversationId": "native", "workspacePaths": []string{root, other}}},
		{[]string{"antigravity", "--auto-project", "--event", "SessionStart"}, map[string]any{"conversationId": "native", "workspacePaths": []string{}}},
		{[]string{"antigravity", "--auto-project", "--event", "SessionStart"}, map[string]any{"conversationId": "native", "workspacePaths": []string{"relative"}}},
		{[]string{"antigravity", "--auto-project", "--event", "SessionStart"}, map[string]any{"session_id": "invented", "workspacePaths": []string{root}}},
		{[]string{"antigravity", "--event", "SessionStart", "--event", "SessionStart"}, map[string]any{"conversationId": "native", "workspacePaths": []string{root}}},
		{[]string{"codex", "--event", "SessionStart"}, map[string]any{"session_id": "native", "cwd": root, "hook_event_name": "SessionStart"}},
	}
	for _, tc := range cases {
		cfg := testConfig(t)
		body, err := json.Marshal(tc.body)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := runHostEvent(cfg, tc.args, bytes.NewReader(body), &out); err == nil {
			t.Fatalf("unsupported lifecycle accepted: %+v", tc)
		}
		for _, path := range []string{filepath.Join(cfg.DataDir, "recalldock.db"), filepath.Join(cfg.DataDir, "operations.db"), filepath.Join(root, ".recalldock"), filepath.Join(other, ".recalldock")} {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("invalid lifecycle initialized %s: %v", path, err)
			}
		}
		if out.Len() != 0 {
			t.Fatal("invalid lifecycle injected a context")
		}
	}
}

func TestHostEventAntigravitySessionStart(t *testing.T) {
	cfg := testConfig(t)
	root := t.TempDir()
	s, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateRegisteredProject("agy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindProjectRoot(p.ID, root); err != nil {
		t.Fatal(err)
	}
	s.Close()

	body, _ := json.Marshal(map[string]any{
		"conversationId": "conv-123",
		"workspacePaths": []string{root},
	})
	var out bytes.Buffer
	if err := runHostEvent(cfg, []string{"antigravity", "--auto-project", "--event", "SessionStart"}, bytes.NewReader(body), &out); err != nil {
		t.Fatalf("antigravity host-event failed: %v", err)
	}
	var resp struct {
		InjectSteps []struct {
			SystemMessage struct {
				SystemMessage string `json:"systemMessage"`
			} `json:"systemMessage"`
		} `json:"injectSteps"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("invalid injectSteps json: %v (%s)", err, out.String())
	}
	if len(resp.InjectSteps) != 1 || !strings.Contains(resp.InjectSteps[0].SystemMessage.SystemMessage, "antigravity-conv-123") {
		t.Fatalf("unexpected injectSteps output: %s", out.String())
	}
}
