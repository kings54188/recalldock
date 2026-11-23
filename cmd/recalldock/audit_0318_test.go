package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"recalldock.local/recalldock/internal/diagnostic"
	"recalldock.local/recalldock/internal/store"
)

func declareAuditProject(t *testing.T, root, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".recalldock"), 0700); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"project_name": name})
	if err := os.WriteFile(filepath.Join(root, ".recalldock", "config.json"), body, 0600); err != nil {
		t.Fatal(err)
	}
}

func withAuditProject(t *testing.T, name string) {
	t.Helper()
	root := t.TempDir()
	withCwd(t, root)
	declareAuditProject(t, root, name)
}

func TestAudit0318CLIProcess(t *testing.T) {
	if os.Getenv("RECALLDOCK_AUDIT_CLI_HELPER") == "1" {
		if err := os.Chdir(os.Getenv("RECALLDOCK_AUDIT_CLI_ROOT")); err != nil {
			os.Exit(2)
		}
		index := 0
		for i, arg := range os.Args {
			if arg == "--" {
				index = i + 1
				break
			}
		}
		if index == 0 {
			os.Exit(2)
		}
		os.Args = append([]string{"recalldock"}, os.Args[index:]...)
		main()
		os.Exit(0)
	}
	root := t.TempDir()
	cfg := testConfig(t)
	alpha := mustSeedObservation(t, cfg, "process-alpha", "alpha", "decision", "alpha title", "synthetic alpha", "project")
	beta := mustSeedObservation(t, cfg, "process-beta", "beta", "decision", "beta title", "synthetic beta", "project")
	s, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		name string
		id   int64
	}{{"alpha", alpha}, {"beta", beta}} {
		ref := "file:///synthetic/" + item.name + "@sha256:" + strings.Repeat("a", 64)
		if _, err := s.AddResourceDependency(store.AddResourceDependencyParams{Project: item.name, SourceRef: ref, DependentObservationID: item.id}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.NotifyResourceChanged(store.NotifyResourceChangeParams{Project: item.name, SourceRef: ref}); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	run := func(args ...string) (string, string, int) {
		t.Helper()
		command := exec.Command(os.Args[0], append([]string{"-test.run=^TestAudit0318CLIProcess$", "--"}, args...)...)
		command.Dir = root
		command.Env = append(os.Environ(), "RECALLDOCK_AUDIT_CLI_HELPER=1", "RECALLDOCK_AUDIT_CLI_ROOT="+root, "RECALLDOCK_DATA_DIR="+cfg.DataDir)
		var stdout, stderr strings.Builder
		command.Stdout = &stdout
		command.Stderr = &stderr
		err := command.Run()
		code := 0
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatal(err)
			}
			code = exit.ExitCode()
		}
		return stdout.String(), stderr.String(), code
	}
	out, stderr, code := run("review", "list", "--json")
	if code != 0 || !json.Valid([]byte(out)) || strings.Contains(out, "alpha") || !strings.Contains(stderr, "unbound") {
		t.Fatalf("unbound process scope: %d %q %q", code, out, stderr)
	}
	declareAuditProject(t, root, "beta")
	out, stderr, code = run("review", "list", "--json")
	if code != 0 || !json.Valid([]byte(out)) || strings.Contains(out, "alpha") || !strings.Contains(out, "beta") || stderr != "" {
		t.Fatalf("bound process scope: %d %q %q", code, out, stderr)
	}
	out, stderr, code = run("delete", fmt.Sprint(alpha))
	if code != 1 || out != "" || !strings.Contains(stderr, "trusted project") {
		t.Fatalf("cross project process write: %d %q %q", code, out, stderr)
	}
	for _, args := range [][]string{{"search", "alpha", "--limit=5", "--all", "--json"}, {"memory", "cooldown", "--project=beta", "--dry-run", "--json"}, {"review", "list", "--all", "--json"}} {
		out, stderr, code = run(args...)
		if code != 0 || !json.Valid([]byte(out)) {
			t.Fatalf("JSON subprocess: %v %d %q %q", args, code, out, stderr)
		}
	}
	for _, args := range [][]string{{"export", "--json"}, {"mcp", "--tools=typo"}, {"serve", "abc"}, {"search", "alpha", "--json", "--json"}} {
		out, stderr, code = run(args...)
		if code != 1 || out != "" || stderr == "" {
			t.Fatalf("strict subprocess: %v %d %q %q", args, code, out, stderr)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "--json")); !os.IsNotExist(err) {
		t.Fatal("export --json created a file")
	}
}

func TestAudit0318SyncImportScope(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root)
	declareAuditProject(t, root, "beta")
	sourceCfg, targetCfg := testConfig(t), testConfig(t)
	mustSeedObservation(t, sourceCfg, "scoped-sync-alpha", "alpha", "decision", "alpha exchange", "alpha exchange body", "project")
	mustSeedObservation(t, sourceCfg, "scoped-sync-beta", "beta", "decision", "beta exchange", "beta exchange body", "project")
	withArgs(t, "recalldock", "sync", "--all")
	out, stderr := captureOutput(t, func() { cmdSync(sourceCfg) })
	if stderr != "" || !strings.Contains(out, "Created chunk") {
		t.Fatalf("source export: %q %q", out, stderr)
	}
	s, err := store.New(targetCfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession("target-beta", "beta", root); err != nil {
		t.Fatal(err)
	}
	s.Close()
	withArgs(t, "recalldock", "sync", "--import")
	out, stderr = captureOutput(t, func() { cmdSync(targetCfg) })
	if stderr != "" {
		t.Fatalf("scoped import: %q %q", out, stderr)
	}
	s, err = store.New(targetCfg)
	if err != nil {
		t.Fatal(err)
	}
	data, err := s.Export()
	s.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Observations) != 1 || data.Observations[0].Project == nil || *data.Observations[0].Project != "beta" {
		t.Fatalf("scoped import wrote foreign project: %+v", data.Observations)
	}
	withArgs(t, "recalldock", "sync", "--import", "--all")
	out, stderr = captureOutput(t, func() { cmdSync(targetCfg) })
	if stderr != "" {
		t.Fatalf("remaining all import: %q %q", out, stderr)
	}
	s, err = store.New(targetCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	data, err = s.Export()
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Observations) != 2 {
		t.Fatalf("partial chunk marked globally completed: count=%d", len(data.Observations))
	}
}

func TestAudit0318ReviewScope(t *testing.T) {
	cfg := testConfig(t)
	root := t.TempDir()
	withCwd(t, root)
	s, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha", "beta"} {
		s.Close()
		id := mustSeedObservation(t, cfg, "review-"+name, name, "decision", name+" secret title", "synthetic body", "project")
		s, err = store.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		ref := "file:///synthetic/" + name + "@sha256:" + strings.Repeat("a", 64)
		if _, err := s.AddResourceDependency(store.AddResourceDependencyParams{Project: name, SourceRef: ref, DependentObservationID: id}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.NotifyResourceChanged(store.NotifyResourceChangeParams{Project: name, SourceRef: ref}); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	for _, sub := range []string{"list", "dependencies", "resource-dependencies"} {
		withArgs(t, "recalldock", "review", sub, "--json")
		out, stderr := captureOutput(t, func() { cmdReview(cfg) })
		if !json.Valid([]byte(out)) || strings.Contains(out, "alpha") || strings.Contains(out, "beta") || !strings.Contains(stderr, "unbound") {
			t.Fatalf("%s leaked unbound: %q %q", sub, out, stderr)
		}
	}
	declareAuditProject(t, root, "beta")
	for _, sub := range []string{"list", "dependencies", "resource-dependencies"} {
		withArgs(t, "recalldock", "review", sub, "--json")
		out, stderr := captureOutput(t, func() { cmdReview(cfg) })
		if !json.Valid([]byte(out)) || strings.Contains(out, "alpha") || stderr != "" {
			t.Fatalf("%s crossed binding: %q %q", sub, out, stderr)
		}
	}
	withArgs(t, "recalldock", "review", "list", "--all", "--json")
	out, stderr := captureOutput(t, func() { cmdReview(cfg) })
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "beta") || stderr != "" {
		t.Fatalf("explicit all: %q %q", out, stderr)
	}
}

func TestAudit0318MutationOwnerBoundary(t *testing.T) {
	for _, declared := range []string{"", "alpha", "beta"} {
		t.Run("binding-"+declared, func(t *testing.T) {
			cfg := testConfig(t)
			root := t.TempDir()
			withCwd(t, root)
			if declared != "" {
				declareAuditProject(t, root, declared)
			}
			id := mustSeedObservation(t, cfg, "write-beta", "beta", "decision", "safe title", "safe body", "project")
			stubExitWithPanic(t)
			withArgs(t, "recalldock", "delete", fmt.Sprint(id))
			out, stderr, p := captureOutputAndRecover(t, func() { cmdDelete(cfg) })
			if declared == "beta" {
				if p != nil || !strings.Contains(out, "soft-deleted") || stderr != "" {
					t.Fatalf("matching scope failed: %q %q %v", out, stderr, p)
				}
			} else {
				if p == nil || stderr == "" {
					t.Fatalf("mutation crossed boundary: %q %q", out, stderr)
				}
				s, err := store.New(cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				if _, err := s.GetObservation(id); err != nil {
					t.Fatalf("denied delete wrote: %v", err)
				}
			}
		})
	}
}

func TestAudit0318RegisteredMutationGuards(t *testing.T) {
	cfg := testConfig(t)
	unbound, alphaRoot, betaRoot := t.TempDir(), t.TempDir(), t.TempDir()
	withCwd(t, unbound)
	s, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	alpha, err := s.CreateRegisteredProject("alpha")
	if err != nil {
		t.Fatal(err)
	}
	beta, err := s.CreateRegisteredProject("beta")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindProjectRoot(alpha.ID, alphaRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindProjectRoot(beta.ID, betaRoot); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSessionWithOrigin("registered-beta", beta.ID, betaRoot, store.SessionOriginOperator); err != nil {
		t.Fatal(err)
	}
	id, err := s.AddObservation(store.AddObservationParams{SessionID: "registered-beta", Project: beta.ID, Type: "decision", Title: "synthetic target", Content: "synthetic target body"})
	if err != nil {
		t.Fatal(err)
	}
	promptID, err := s.AddPrompt(store.AddPromptParams{SessionID: "registered-beta", Project: beta.ID, Content: "synthetic prompt"})
	if err != nil {
		t.Fatal(err)
	}
	ref := "file:///synthetic/source@sha256:" + strings.Repeat("a", 64)
	dep, err := s.AddResourceDependency(store.AddResourceDependencyParams{Project: beta.ID, SourceRef: ref, DependentObservationID: id})
	if err != nil {
		t.Fatal(err)
	}
	committed, err := s.CommitMemoryCandidates(store.CommitMemoryCandidatesParams{SessionID: "registered-beta", Project: beta.ID, Candidates: []store.MemoryCandidateInput{{Category: store.CandidateCategoryDecision, Statement: "Synthetic durable proposal", Confidence: 0.8}}})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := s.GetMemoryCandidate(committed.CandidateIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	digest, err := store.MemoryCandidateDigest(candidate)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := s.SelectObservationForEdit(id)
	if err != nil {
		t.Fatal(err)
	}
	patch := filepath.Join(t.TempDir(), "patch.json")
	if err := os.WriteFile(patch, []byte(`{"title":"changed"}`), 0600); err != nil {
		t.Fatal(err)
	}
	s.Close()
	commands := [][]string{
		{"delete", fmt.Sprint(id), "--hard"}, {"delete", "prompt", fmt.Sprint(promptID)}, {"delete", "session", "registered-beta"}, {"delete", "project", "beta", "--hard", "--confirm"},
		{"review", "mark", fmt.Sprint(id)}, {"review", "link", fmt.Sprint(id), fmt.Sprint(id)}, {"review", "resource-link", ref, fmt.Sprint(id), "--project", beta.ID}, {"review", "resource-changed", ref, "--project", beta.ID}, {"review", "resource-refresh", dep.SyncID, "--project", beta.ID},
		{"candidates", "approve", candidate.SyncID, "--expected", digest, "--reason", "operator"}, {"candidates", "reject", candidate.SyncID, "--expected", digest, "--reason", "operator"}, {"candidates", "revive", candidate.SyncID, "--expected", digest, "--reason", "operator"},
		{"memory", "archive", fmt.Sprint(id), "--expected", selected.SnapshotSHA256, "--reason", "operator"}, {"memory", "reactivate", fmt.Sprint(id), "--expected", selected.SnapshotSHA256, "--reason", "operator"}, {"memory", "update", fmt.Sprint(id), "--expected", selected.SnapshotSHA256, "--input", patch, "--reason", "operator"}, {"memory", "consolidate", fmt.Sprint(id), "--source", fmt.Sprint(id), "--reason", "operator"},
	}
	for _, root := range []string{unbound, alphaRoot} {
		for _, command := range commands {
			t.Run(filepath.Base(root)+"-"+strings.Join(command[:2], "-"), func(t *testing.T) {
				withCwd(t, root)
				stubExitWithPanic(t)
				withArgs(t, append([]string{"recalldock"}, command...)...)
				out, stderr, p := captureOutputAndRecover(t, func() {
					switch command[0] {
					case "delete":
						cmdDelete(cfg)
					case "review":
						cmdReview(cfg)
					case "candidates":
						cmdCandidates(cfg)
					case "memory":
						cmdMemory(cfg)
					}
				})
				if p != exitCode(1) || out != "" || (!strings.Contains(stderr, "write authority") && !strings.Contains(stderr, "trusted project")) {
					t.Fatalf("write guard absent: %q %q %v", out, stderr, p)
				}
			})
		}
	}
	s, err = store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	current, err := s.SelectObservationForEdit(id)
	if err != nil || current.SnapshotSHA256 != selected.SnapshotSHA256 {
		t.Fatalf("denied mutation changed observation: %v", err)
	}
	currentCandidate, err := s.GetMemoryCandidate(candidate.SyncID)
	if err != nil {
		t.Fatal(err)
	}
	currentDigest, err := store.MemoryCandidateDigest(currentCandidate)
	if err != nil || currentDigest != digest {
		t.Fatalf("denied mutation changed candidate: %v", err)
	}
}

func TestAudit0318SearchEnvelopeAndRecalls(t *testing.T) {
	cfg := testConfig(t)
	withAuditProject(t, "alpha")
	var originals []store.SearchResult
	for i := 0; i < 3; i++ {
		id := mustSeedObservation(t, cfg, fmt.Sprintf("budget-%d", i), "alpha", "decision", fmt.Sprintf("Synthetic index %d", i), "UNSELECTED_PRIVATE_BODY", "project")
		s, err := store.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		obs, err := s.GetObservation(id)
		s.Close()
		if err != nil {
			t.Fatal(err)
		}
		originals = append(originals, store.SearchResult{Observation: *obs, SelectedDetail: store.SearchDetailL0, SelectedContent: strings.Repeat("selected index ", 100), EstimatedTokens: 1})
	}
	old := storeSearchDetailed
	storeSearchDetailed = func(_ *store.Store, _ string, opts store.SearchOptions) (*store.SearchResponse, error) {
		if !opts.DeferRecallTracking {
			t.Fatal("query tracked probes before output")
		}
		return &store.SearchResponse{Results: append([]store.SearchResult(nil), originals...), Budget: store.SearchBudget{Limit: opts.TokenBudget, Candidates: 3, Estimator: store.SearchTokenEstimator}}, nil
	}
	t.Cleanup(func() { storeSearchDetailed = old })
	withArgs(t, "recalldock", "search", "synthetic", "--project=alpha", "--json", "--token-budget=1400")
	out, stderr := captureOutput(t, func() { cmdSearch(cfg) })
	var result struct {
		Results []struct {
			ID int64 `json:"id"`
		} `json:"results"`
		Budget        store.SearchBudget        `json:"budget"`
		RecallSummary store.SearchRecallSummary `json:"recall_summary"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil || stderr != "" {
		t.Fatalf("invalid envelope: %q %q %v", out, stderr, err)
	}
	cost := store.EstimateReadTokens(out)
	if cost > 1400 || result.Budget.Used != cost || len(result.Results) == 0 || len(result.Results) >= 3 || !result.Budget.Truncated || result.RecallSummary.BudgetDropped != 3-len(result.Results) {
		t.Fatalf("unbounded envelope: cost=%d budget=%+v count=%d", cost, result.Budget, len(result.Results))
	}
	if strings.Contains(out, "UNSELECTED_PRIVATE_BODY") {
		t.Fatal("JSON exposed unselected body")
	}
	selected := map[int64]bool{}
	for _, row := range result.Results {
		selected[row.ID] = true
	}
	s, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range originals {
		obs, err := s.GetObservation(row.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		if selected[row.ID] {
			want = 1
		}
		if obs.RecallCount != want {
			t.Fatalf("undelivered probe counted: id=%d count=%d want=%d", row.ID, obs.RecallCount, want)
		}
	}
	s.Close()
	stubExitWithPanic(t)
	withArgs(t, "recalldock", "search", "synthetic", "--project=alpha", "--json", "--token-budget=1")
	out, stderr, p := captureOutputAndRecover(t, func() { cmdSearch(cfg) })
	if p != exitCode(1) || out != "" || !strings.Contains(stderr, "CLI response needs") {
		t.Fatalf("too small budget accepted: %q %q %v", out, stderr, p)
	}
	s, err = store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, row := range originals {
		obs, err := s.GetObservation(row.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		if selected[row.ID] {
			want = 1
		}
		if obs.RecallCount != want {
			t.Fatalf("denied response counted recall for %d", row.ID)
		}
	}
}

func TestAudit0318StructuredOutput(t *testing.T) {
	cfg := testConfig(t)
	withAuditProject(t, "alpha")
	id := mustSeedObservation(t, cfg, "output-alpha", "alpha", "decision", "Synthetic", "Synthetic body", "project")
	withArgs(t, "recalldock", "memory", "sources", fmt.Sprint(id), "--json")
	out, stderr := captureOutput(t, func() { cmdMemorySources(cfg) })
	var sources struct {
		Sources []store.Observation `json:"sources"`
	}
	if json.Unmarshal([]byte(out), &sources) != nil || sources.Sources == nil || stderr != "" {
		t.Fatalf("empty sources not array: %q %q", out, stderr)
	}
	withArgs(t, "recalldock", "memory", "cooldown", "--project=alpha", "--dry-run", "--json")
	out, stderr = captureOutput(t, func() { cmdMemoryCooldown(cfg) })
	if !json.Valid([]byte(out)) || !strings.Contains(stderr, "[dry-run]") {
		t.Fatalf("dry-run polluted JSON: %q %q", out, stderr)
	}
	stubExitWithPanic(t)
	withArgs(t, "recalldock", "conflicts", "show", "99999", "--json")
	out, stderr, p := captureOutputAndRecover(t, func() { cmdConflictsShow(cfg) })
	if p != exitCode(1) || out != "" || !json.Valid([]byte(stderr)) {
		t.Fatalf("error not structured stderr: %q %q %v", out, stderr, p)
	}
}

func TestAudit0318DeleteSoftTombstoneAndConfirmation(t *testing.T) {
	cfg := testConfig(t)
	withAuditProject(t, "alpha")
	id := mustSeedObservation(t, cfg, "delete-soft", "alpha", "decision", "Synthetic", "Synthetic body", "project")
	for _, tail := range [][]string{nil, {"--hard"}} {
		withArgs(t, append([]string{"recalldock", "delete", fmt.Sprint(id)}, tail...)...)
		out, stderr := captureOutput(t, func() { cmdDelete(cfg) })
		if !strings.Contains(out, "deleted") || stderr != "" {
			t.Fatalf("soft/hard tombstone delete: %q %q", out, stderr)
		}
	}
	mustSeedObservation(t, cfg, "delete-confirm", "alpha", "decision", "Second synthetic", "Second body", "project")
	stubExitWithPanic(t)
	withArgs(t, "recalldock", "delete", "project", "alpha", "--hard")
	out, stderr, p := captureOutputAndRecover(t, func() { cmdDelete(cfg) })
	if p != exitCode(1) || out != "" || !strings.Contains(stderr, "--confirm") {
		t.Fatalf("hard project did not confirm: %q %q %v", out, stderr, p)
	}
}

func TestAudit0318ParserContracts(t *testing.T) {
	for _, args := range [][]string{{"needle", "--json", "--json"}, {"needle", "--all", "--all"}, {"needle", "--limit=5", "--limit", "6"}} {
		if _, err := validateSearchArguments(args); err == nil {
			t.Fatalf("duplicate accepted: %v", args)
		}
	}
	if _, err := validateSearchArguments([]string{"needle", "--limit=5", "--json"}); err != nil {
		t.Fatal(err)
	}
	for _, selection := range []string{"agent", "admin", "all", "agent,admin", "mem_search,mem_stats"} {
		if _, _, err := parseMCPArguments([]string{"--tools=" + selection}); err != nil {
			t.Fatalf("valid selection rejected: %s %v", selection, err)
		}
	}
	if _, _, err := parseMCPArguments([]string{"--tools=typo"}); err == nil {
		t.Fatal("unknown MCP profile accepted")
	}
	if _, err := parseCLIOptions([]string{"--", "--json"}, nil, []string{"--json"}); err == nil {
		t.Fatal("unconsumed positional accepted")
	}
}

func TestAudit0318StrictCommandsBeforeStore(t *testing.T) {
	for _, command := range [][]string{{"export", "--json"}, {"delete", "1", "--hrad"}, {"projects", "prune", "--bogus"}, {"memory", "sources", "1", "--bogus"}, {"serve", "abc"}, {"serve", "0"}, {"conflicts", "list", "--since", "bad"}, {"conflicts", "deferred", "--status", "bogus"}, {"sync", "--project", "a", "--all"}, {"candidates", "policy", "--mode", "manual"}, {"candidates", "sweep"}, {"review", "process"}, {"candidates", "list", "--all", "--all"}, {"conflicts", "deferred", "--replay"}, {"sync", "cleanup", "--non-enrolled", "--apply"}} {
		t.Run(strings.Join(command, "_"), func(t *testing.T) {
			stubExitWithPanic(t)
			cfg := testConfig(t)
			cfg.DataDir = filepath.Join(cfg.DataDir, "absent")
			withArgs(t, append([]string{"recalldock"}, command...)...)
			_, stderr, p := captureOutputAndRecover(t, func() {
				switch command[0] {
				case "export":
					cmdExport(cfg)
				case "delete":
					cmdDelete(cfg)
				case "projects":
					cmdProjects(cfg)
				case "memory":
					cmdMemory(cfg)
				case "serve":
					cmdServe(cfg)
				case "conflicts":
					cmdConflicts(cfg)
				case "sync":
					cmdSync(cfg)
				case "review":
					cmdReview(cfg)
				case "candidates":
					cmdCandidates(cfg)
				}
			})
			if p == nil || stderr == "" {
				t.Fatalf("invalid invocation accepted: %v %q", p, stderr)
			}
			if _, err := os.Stat(cfg.DataDir); !os.IsNotExist(err) {
				t.Fatalf("invalid arguments opened store: %v", err)
			}
		})
	}
}

func TestAudit0318DoctorExitStatus(t *testing.T) {
	for _, status := range []string{"ok", "warning", "blocked", "error", "failed"} {
		for _, asJSON := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s-%t", status, asJSON), func(t *testing.T) {
				cfg := testConfig(t)
				old := runDiagnostics
				runDiagnostics = func(context.Context, *store.Store, string, string) (diagnostic.Report, error) {
					return diagnostic.Report{Status: status}, nil
				}
				t.Cleanup(func() { runDiagnostics = old })
				stubExitWithPanic(t)
				args := []string{"recalldock", "doctor"}
				if asJSON {
					args = append(args, "--json")
				}
				withArgs(t, args...)
				out, _, p := captureOutputAndRecover(t, func() { cmdDoctor(cfg) })
				wantFailure := status == "blocked" || status == "error" || status == "failed"
				if (p != nil) != wantFailure || !strings.Contains(out, status) || asJSON && !json.Valid([]byte(out)) {
					t.Fatalf("status lost: out=%q panic=%v", out, p)
				}
			})
		}
	}
	t.Run("runner-error", func(t *testing.T) {
		cfg := testConfig(t)
		old := runDiagnostics
		runDiagnostics = func(context.Context, *store.Store, string, string) (diagnostic.Report, error) {
			return diagnostic.Report{}, errors.New("synthetic failure")
		}
		t.Cleanup(func() { runDiagnostics = old })
		stubExitWithPanic(t)
		withArgs(t, "recalldock", "doctor", "--json")
		out, _, p := captureOutputAndRecover(t, func() { cmdDoctor(cfg) })
		if p == nil || !json.Valid([]byte(out)) {
			t.Fatalf("runner failure not signaled: %q %v", out, p)
		}
	})
}
