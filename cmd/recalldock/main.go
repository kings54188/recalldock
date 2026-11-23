// RecallDock — Local selective memory for AI coding agents.
//
// Usage:
//
//	recalldock serve          Start HTTP + MCP server
//	recalldock mcp            Start MCP server only (stdio transport)
//	recalldock search <query> Search memories from CLI
//	recalldock save           Save a memory from CLI
//	recalldock context        Show recent context
//	recalldock stats          Show memory stats
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"recalldock.local/recalldock/internal/candidatehook"
	"recalldock.local/recalldock/internal/cloud/autosync"
	"recalldock.local/recalldock/internal/cloud/constants"
	"recalldock.local/recalldock/internal/cloud/remote"
	"recalldock.local/recalldock/internal/cloud/syncguidance"
	"recalldock.local/recalldock/internal/diagnostic"
	"recalldock.local/recalldock/internal/mcp"
	"recalldock.local/recalldock/internal/obsidian"
	"recalldock.local/recalldock/internal/project"
	"recalldock.local/recalldock/internal/server"
	"recalldock.local/recalldock/internal/setup"
	"recalldock.local/recalldock/internal/store"
	engramsync "recalldock.local/recalldock/internal/sync"
	"recalldock.local/recalldock/internal/timeutil"
	"recalldock.local/recalldock/internal/tui"
	versioncheck "recalldock.local/recalldock/internal/version"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/term"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// version is set via ldflags at build time by goreleaser.
// Falls back to "dev" for local builds; init() tries Go module info first.
var version = "dev"

func init() {
	if version != "dev" {
		return
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		version = strings.TrimPrefix(info.Main.Version, "v")
	}
}

var (
	storeNew      = openCLIStore
	newHTTPServer = server.New
	startHTTP     = (*server.Server).Start

	newMCPServer           = mcp.NewServer
	newMCPServerWithTools  = mcp.NewServerWithTools
	newMCPServerWithConfig = mcp.NewServerWithConfig
	resolveMCPTools        = mcp.ResolveTools
	serveMCP               = mcpserver.ServeStdio

	// detectProject is injectable for testing; wraps project.DetectProject.
	detectProject = project.DetectProject

	newTUIModel         = func(s *store.Store) tui.Model { return tui.New(s, uiDisplayVersion(version)) }
	newTeaProgram       = tea.NewProgram
	runTeaProgram       = (*tea.Program).Run
	interactiveTerminal = func() bool { return term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd()) }

	checkForUpdates = versioncheck.CheckLatest

	setupSupportedAgents        = setup.SupportedAgents
	setupInstallAgent           = setup.Install
	setupAddClaudeCodeAllowlist = setup.AddClaudeCodeAllowlist
	scanInputLine               = fmt.Scanln

	storeSearch = func(s *store.Store, query string, opts store.SearchOptions) ([]store.SearchResult, error) {
		return s.Search(query, opts)
	}
	storeSearchDetailed = func(s *store.Store, query string, opts store.SearchOptions) (*store.SearchResponse, error) {
		return s.SearchDetailed(query, opts)
	}
	storeAddObservation    = func(s *store.Store, p store.AddObservationParams) (int64, error) { return s.AddObservationNew(p) }
	storeDeleteObservation = func(s *store.Store, id int64, hard bool) error { return s.DeleteObservation(id, hard) }
	storeDeleteSession     = func(s *store.Store, id string) error { return s.DeleteSession(id) }
	storeDeletePrompt      = func(s *store.Store, id int64) error { return s.DeletePrompt(id) }
	storeDeleteProject     = func(s *store.Store, name string, hard bool) (*store.DeleteProjectResult, error) {
		return s.DeleteProject(name, hard)
	}
	storeTimeline = func(s *store.Store, observationID int64, before, after int) (*store.TimelineResult, error) {
		return s.Timeline(observationID, before, after)
	}
	storeFormatContext = func(s *store.Store, project, scope string) (string, error) { return s.FormatContext(project, scope) }
	storeStats         = func(s *store.Store) (*store.Stats, error) { return s.Stats() }
	storeExport        = func(s *store.Store) (*store.ExportData, error) { return s.Export() }
	jsonMarshalIndent  = json.MarshalIndent
	runDiagnostics     = func(ctx context.Context, s *store.Store, project, check string) (diagnostic.Report, error) {
		runner := diagnostic.NewRunner()
		scope := diagnostic.Scope{Store: s, Project: project, Now: time.Now()}
		if strings.TrimSpace(check) != "" {
			return runner.RunOne(ctx, scope, check)
		}
		return runner.RunAll(ctx, scope)
	}

	syncStatus = func(sy *engramsync.Syncer) (localChunks int, remoteChunks int, pendingImport int, err error) {
		return sy.Status()
	}
	syncImport = func(sy *engramsync.Syncer) (*engramsync.ImportResult, error) { return sy.Import() }
	syncExport = func(sy *engramsync.Syncer, createdBy, project string) (*engramsync.SyncResult, error) {
		return sy.Export(createdBy, project)
	}
	newCloudAutosyncManager = func(s *store.Store, _ any) cloudAutosyncManager {
		mgr := autosync.New(s, nil, autosync.DefaultConfig())
		return autosyncManagerAdapter{manager: mgr}
	}

	// newAutosyncManager is the injectable factory used by tryStartAutosync.
	// BR2-3: Returns startableAutosyncManager (not *autosync.Manager) so tests can
	// inject a deterministic fake — preventing racy wg.Add/wg.Wait interleaving.
	newAutosyncManager = func(s *store.Store, transport autosync.CloudTransport, cfg autosync.Config) startableAutosyncManager {
		return autosync.New(s, transport, cfg)
	}

	exitFunc = os.Exit

	stdinScanner = func() *bufio.Scanner { return bufio.NewScanner(os.Stdin) }
	userHomeDir  = os.UserHomeDir

	// newObsidianExporter is injectable for testing.
	newObsidianExporter = obsidian.NewExporter

	// newObsidianWatcher is injectable for testing.
	newObsidianWatcher = obsidian.NewWatcher

	// agentRunnerFactory is injectable for testing. In production it delegates to
	// llm.NewRunner; tests substitute a fake to avoid real CLI invocations.
	agentRunnerFactory = defaultAgentRunnerFactory
)

type cloudSyncStatus struct {
	Phase               string
	LastError           string
	ConsecutiveFailures int
	BackoffUntil        *time.Time
	LastSyncAt          *time.Time
	ReasonCode          string
	ReasonMessage       string
}

type cloudAutosyncManager interface {
	Run(context.Context)
	NotifyDirty()
	Status() cloudSyncStatus
}

// startableAutosyncManager is the interface implemented by *autosync.Manager and used
// by tryStartAutosync. It combines autosyncStatusProvider with Run and Stop so that
// the factory variable newAutosyncManager can be stubbed in tests without spawning
// real goroutines — eliminating the racy wg.Add/wg.Wait interleaving.
// BR2-3: Using an interface return type (not *autosync.Manager) makes the factory
// injectable with deterministic fakes.
type startableAutosyncManager interface {
	autosyncStatusProvider // Status() autosync.Status
	Run(context.Context)
	Stop()
}

type autosyncManagerAdapter struct {
	manager *autosync.Manager
}

func (a autosyncManagerAdapter) Run(ctx context.Context) {
	a.manager.Run(ctx)
}

func (a autosyncManagerAdapter) NotifyDirty() {
	a.manager.NotifyDirty()
}

func (a autosyncManagerAdapter) Status() cloudSyncStatus {
	status := a.manager.Status()
	return cloudSyncStatus{
		Phase:               status.Phase,
		LastError:           status.LastError,
		ConsecutiveFailures: status.ConsecutiveFailures,
		BackoffUntil:        status.BackoffUntil,
		LastSyncAt:          status.LastSyncAt,
		ReasonCode:          status.ReasonCode,
		ReasonMessage:       status.ReasonMessage,
	}
}

// mutationTransportAdapter adapts remote.MutationTransport to autosync.CloudTransport.
// This bridges the type gap between packages without creating a circular import.
type mutationTransportAdapter struct {
	remote *remote.MutationTransport
}

func (a *mutationTransportAdapter) PushMutations(entries []autosync.MutationEntry) (*autosync.PushMutationsResult, error) {
	remoteEntries := make([]remote.MutationEntry, len(entries))
	for i, e := range entries {
		remoteEntries[i] = remote.MutationEntry{
			Project:   e.Project,
			Entity:    e.Entity,
			EntityKey: e.EntityKey,
			Op:        e.Op,
			Payload:   e.Payload,
		}
	}
	seqs, err := a.remote.PushMutations(remoteEntries)
	if err != nil {
		return nil, err
	}
	return &autosync.PushMutationsResult{AcceptedSeqs: seqs}, nil
}

func (a *mutationTransportAdapter) PullMutations(sinceSeq int64, limit int) (*autosync.PullMutationsResponse, error) {
	resp, err := a.remote.PullMutations(sinceSeq, limit)
	if err != nil {
		return nil, err
	}
	mutations := make([]autosync.PulledMutation, len(resp.Mutations))
	for i, m := range resp.Mutations {
		mutations[i] = autosync.PulledMutation{
			Seq:        m.Seq,
			Project:    m.Project,
			Entity:     m.Entity,
			EntityKey:  m.EntityKey,
			Op:         m.Op,
			Payload:    m.Payload,
			OccurredAt: m.OccurredAt,
		}
	}
	return &autosync.PullMutationsResponse{
		Mutations: mutations,
		HasMore:   resp.HasMore,
		LatestSeq: resp.LatestSeq,
	}, nil
}

type storeSyncStatusProvider struct {
	store          *store.Store
	defaultProject string
	cfg            store.Config
}

func (p storeSyncStatusProvider) Status(project string) server.SyncStatus {
	resolvedProject, _ := store.NormalizeProject(project)
	resolvedProject = strings.TrimSpace(resolvedProject)
	if resolvedProject == "" {
		resolvedProject, _ = store.NormalizeProject(p.defaultProject)
		resolvedProject = strings.TrimSpace(resolvedProject)
	}
	upgradeStage, upgradeCode, upgradeMessage := p.upgradeStatus(resolvedProject)
	enabled, disabledCode, disabledMessage := p.cloudSyncEnabled(resolvedProject)
	targetKey := cloudTargetKeyForProject(resolvedProject)
	if !enabled {
		if disabledCode == "cloud_not_configured" && resolvedProject != "" {
			enrolled, err := p.store.IsProjectEnrolled(resolvedProject)
			if err != nil {
				return server.SyncStatus{
					Enabled:              false,
					Phase:                store.SyncLifecycleIdle,
					ReasonCode:           "status_unavailable",
					ReasonMessage:        fmt.Sprintf("cloud enrollment status is unavailable: %v", err),
					UpgradeStage:         upgradeStage,
					UpgradeReasonCode:    upgradeCode,
					UpgradeReasonMessage: upgradeMessage,
				}
			}
			if !enrolled {
				return server.SyncStatus{
					Enabled:              false,
					Phase:                store.SyncLifecycleIdle,
					ReasonCode:           constants.ReasonBlockedUnenrolled,
					ReasonMessage:        fmt.Sprintf("project %q is not enrolled for cloud sync", resolvedProject),
					UpgradeStage:         upgradeStage,
					UpgradeReasonCode:    upgradeCode,
					UpgradeReasonMessage: upgradeMessage,
				}
			}
			state, err := p.store.GetSyncState(targetKey)
			if err == nil && hasMeaningfulSyncState(state) {
				status := syncStatusFromState(state)
				status.Enabled = true
				status.UpgradeStage = upgradeStage
				status.UpgradeReasonCode = upgradeCode
				status.UpgradeReasonMessage = upgradeMessage
				return status
			}
		}
		return server.SyncStatus{
			Enabled:              false,
			Phase:                store.SyncLifecycleIdle,
			ReasonCode:           disabledCode,
			ReasonMessage:        disabledMessage,
			UpgradeStage:         upgradeStage,
			UpgradeReasonCode:    upgradeCode,
			UpgradeReasonMessage: upgradeMessage,
		}
	}
	state, err := p.store.GetSyncState(targetKey)
	if err != nil {
		reason := "sync state is unavailable"
		lastErr := fmt.Sprintf("read sync state: %v", err)
		return server.SyncStatus{
			Enabled:              true,
			Phase:                store.SyncLifecycleDegraded,
			ReasonCode:           "status_unavailable",
			ReasonMessage:        reason,
			LastError:            lastErr,
			UpgradeStage:         upgradeStage,
			UpgradeReasonCode:    upgradeCode,
			UpgradeReasonMessage: upgradeMessage,
		}
	}
	status := syncStatusFromState(state)
	status.Enabled = true
	status.UpgradeStage = upgradeStage
	status.UpgradeReasonCode = upgradeCode
	status.UpgradeReasonMessage = upgradeMessage
	return status
}

func (p storeSyncStatusProvider) upgradeStatus(project string) (string, string, string) {
	project = strings.TrimSpace(project)
	if project == "" {
		return "", "", ""
	}
	state, err := p.store.GetCloudUpgradeState(project)
	if err != nil {
		return "", "upgrade_status_unavailable", fmt.Sprintf("cloud upgrade status is unavailable: %v", err)
	}
	if state == nil {
		return "", "", ""
	}
	return state.Stage, strings.TrimSpace(state.LastErrorCode), strings.TrimSpace(state.LastErrorMessage)
}

func (p storeSyncStatusProvider) cloudSyncEnabled(project string) (bool, string, string) {
	cc, err := resolveCloudRuntimeConfig(p.cfg)
	if err != nil {
		return false, "cloud_config_error", fmt.Sprintf("cloud config error: %v", err)
	}
	if cc == nil || strings.TrimSpace(cc.ServerURL) == "" {
		return false, "cloud_not_configured", "cloud sync is not configured"
	}
	if _, err := validateCloudServerURL(cc.ServerURL); err != nil {
		return false, "cloud_config_error", fmt.Sprintf("cloud config error: invalid cloud runtime server URL: %v", err)
	}
	if strings.TrimSpace(project) == "" {
		return false, "project_required", "cloud sync status requires an explicit project scope"
	}
	enrolled, err := p.store.IsProjectEnrolled(project)
	if err != nil {
		return false, "status_unavailable", fmt.Sprintf("cloud enrollment status is unavailable: %v", err)
	}
	if !enrolled {
		return false, constants.ReasonBlockedUnenrolled, fmt.Sprintf("project %q is not enrolled for cloud sync", project)
	}
	return true, "", ""
}

func syncStatusFromState(state *store.SyncState) server.SyncStatus {
	var lastSyncAt *time.Time
	if state != nil && state.Lifecycle == store.SyncLifecycleHealthy {
		lastSyncAt = parseSyncStateTimestamp(state.UpdatedAt)
	}
	return server.SyncStatus{
		Phase:               state.Lifecycle,
		LastError:           derefString(state.LastError),
		ConsecutiveFailures: state.ConsecutiveFailures,
		BackoffUntil:        parseRFC3339Ptr(state.BackoffUntil),
		LastSyncAt:          lastSyncAt,
		ReasonCode:          derefString(state.ReasonCode),
		ReasonMessage:       derefString(state.ReasonMessage),
	}
}

func hasMeaningfulSyncState(state *store.SyncState) bool {
	if state == nil {
		return false
	}
	if state.Lifecycle != "" && state.Lifecycle != store.SyncLifecycleIdle {
		return true
	}
	if state.LastEnqueuedSeq > 0 || state.LastAckedSeq > 0 || state.LastPulledSeq > 0 {
		return true
	}
	if state.ConsecutiveFailures > 0 {
		return true
	}
	if state.BackoffUntil != nil || state.LeaseOwner != nil || state.LeaseUntil != nil {
		return true
	}
	if state.ReasonCode != nil || state.ReasonMessage != nil || state.LastError != nil {
		return true
	}
	return false
}

func parseSyncStateTimestamp(value string) *time.Time {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	if parsed, err := time.Parse(time.RFC3339, trimmed); err == nil {
		return &parsed
	}
	if parsed, err := time.ParseInLocation("2006-01-02 15:04:05", trimmed, time.UTC); err == nil {
		return &parsed
	}
	return nil
}

func parseRFC3339Ptr(value *string) *time.Time {
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, *value)
	if err != nil {
		return nil
	}
	return &parsed
}

func derefString(ptr *string) string {
	if ptr == nil {
		return ""
	}
	return *ptr
}

func envBool(key string) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func resolveCloudRuntimeConfig(cfg store.Config) (*cloudConfig, error) {
	cc, err := loadCloudConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("read cloud config: %w", err)
	}
	if cc == nil {
		cc = &cloudConfig{}
	}
	// RECALLDOCK_CLOUD_TOKEN overrides any token stored in cloud.json.
	// When the env var is absent, the persisted token from cloud.json is used
	// as a fallback so that `recalldock sync --cloud` works without requiring the
	// env var to be set in every shell session (fix for issue #343).
	if v := strings.TrimSpace(os.Getenv("RECALLDOCK_CLOUD_SERVER")); v != "" {
		cc.ServerURL = v
	}
	if v := strings.TrimSpace(os.Getenv("RECALLDOCK_CLOUD_TOKEN")); v != "" {
		cc.Token = v
	}
	return cc, nil
}

func preflightCloudSync(s *store.Store, cfg store.Config, project string, mutateState bool) (*cloudConfig, error) {
	project = strings.TrimSpace(project)
	if project != "" {
		project, _ = store.NormalizeProject(project)
	}
	targetKey := cloudTargetKeyForProject(project)

	cc, err := resolveCloudRuntimeConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("cloud sync config error: %w", err)
	}
	hasServer := strings.TrimSpace(cc.ServerURL) != ""
	if !hasServer {
		message := "cloud server is missing: configure server URL with `recalldock cloud config --server <url>`"
		if mutateState {
			_ = s.MarkSyncBlocked(targetKey, constants.ReasonCloudConfigError, message)
		}
		return nil, fmt.Errorf("cloud sync %s: %s", constants.ReasonCloudConfigError, message)
	}
	if _, err := validateCloudServerURL(cc.ServerURL); err != nil {
		message := fmt.Sprintf("invalid cloud runtime server URL: %v", err)
		if mutateState {
			_ = s.MarkSyncBlocked(targetKey, constants.ReasonCloudConfigError, message)
		}
		return nil, fmt.Errorf("cloud sync %s: %s", constants.ReasonCloudConfigError, message)
	}
	if project != "" {
		enrolled, err := s.IsProjectEnrolled(project)
		if err != nil {
			return nil, fmt.Errorf("cloud sync enrollment check: %w", err)
		}
		if !enrolled {
			message := fmt.Sprintf("project %q is not enrolled for cloud sync", project)
			if mutateState {
				_ = s.MarkSyncBlocked(targetKey, constants.ReasonBlockedUnenrolled, message)
			}
			return nil, fmt.Errorf("cloud sync blocked_unenrolled: %s", message)
		}
		if err := preflightCloudSyncLegacyMutations(s, project, targetKey, mutateState); err != nil {
			return nil, err
		}
	}
	return cc, nil
}

func preflightCloudSyncLegacyMutations(s *store.Store, project, targetKey string, mutateState bool) error {
	report, err := s.DiagnoseCloudUpgradeLegacyMutations(project)
	if err != nil {
		return fmt.Errorf("cloud sync legacy mutation preflight: %w", err)
	}
	if report.BlockedCount == 0 && report.RepairableCount == 0 {
		return nil
	}

	reasonCode := store.UpgradeReasonRepairableLegacyMutationPayload
	message := fmt.Sprintf(
		"legacy mutation payloads require repair before cloud sync for project %q: run `recalldock cloud upgrade doctor --project %s` then `recalldock cloud upgrade repair --project %s --apply`",
		project, project, project,
	)
	if report.BlockedCount > 0 {
		reasonCode = store.UpgradeReasonBlockedLegacyMutationManual
		first := firstBlockedLegacyMutationFinding(report)
		message = fmt.Sprintf(
			"legacy mutation payloads require manual action before cloud sync for project %q (seq=%d entity=%s op=%s): %s; inspect with `recalldock cloud upgrade doctor --project %s` and run `recalldock cloud upgrade repair --project %s --apply` for deterministic repairs",
			project, first.Seq, first.Entity, first.Op, first.Message, project, project,
		)
	}
	if mutateState {
		_ = s.MarkSyncBlocked(targetKey, reasonCode, message)
	}
	return fmt.Errorf("cloud sync %s: %s", reasonCode, message)
}

func firstBlockedLegacyMutationFinding(report store.CloudUpgradeLegacyMutationReport) store.CloudUpgradeLegacyMutationFinding {
	for _, finding := range report.Findings {
		if !finding.Repairable {
			return finding
		}
	}
	if len(report.Findings) > 0 {
		return report.Findings[0]
	}
	return store.CloudUpgradeLegacyMutationFinding{}
}

func cloudTargetKeyForProject(project string) string {
	project = strings.TrimSpace(project)
	if project == "" {
		return constants.TargetKeyCloud
	}
	project, _ = store.NormalizeProject(project)
	if strings.TrimSpace(project) == "" {
		return constants.TargetKeyCloud
	}
	return fmt.Sprintf("%s:%s", constants.TargetKeyCloud, project)
}

func markCloudSyncFailure(s *store.Store, targetKey string, syncErr error) {
	if syncErr == nil {
		return
	}
	message := cloudSyncFailureMessage(syncguidance.ProjectFromTargetKey(targetKey), syncErr)
	var statusErr *remote.HTTPStatusError
	if errors.As(syncErr, &statusErr) {
		switch {
		case statusErr.IsAuthFailure():
			_ = s.MarkSyncAuthRequired(targetKey, message)
			return
		case statusErr.IsPolicyFailure():
			_ = s.MarkSyncBlocked(targetKey, constants.ReasonPolicyForbidden, message)
			return
		}
	}
	_ = s.MarkSyncFailure(targetKey, message, time.Now().UTC().Add(30*time.Second))
}

func cloudSyncFailureMessage(project string, syncErr error) string {
	if syncErr == nil {
		return ""
	}
	return syncguidance.AppendGuidance(syncErr.Error(), project, syncErr)
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		exitFunc(1)
	}

	if handleHelpCommand(os.Args[1:]) {
		return
	}
	if reason := localOnlyCommandError(os.Args[1:]); reason != nil {
		fatal(reason)
		return
	}
	if os.Args[1] == "tui" && !interactiveTerminal() {
		fmt.Fprintln(os.Stderr, "RecallDock tui requires an interactive terminal on stdin and stdout; open a terminal and run recalldock tui. For scripts use search/context/stats.")
		exitFunc(1)
		return
	}
	if shouldCheckForUpdates(os.Args[1:]) {
		printUpdateCheckResult(checkForUpdates(version))
	}
	if handleConfigFreeCommand(os.Args[1:]) {
		return
	}
	if os.Args[1] == "setup" {
		if err := runLocalSetup(os.Args[2:]); err != nil {
			fatal(err)
		}
		return
	}

	cfg, cfgErr := store.DefaultConfig()
	if cfgErr != nil {
		// Fallback: try to resolve home directory from environment variables
		// that os.UserHomeDir() might have missed (e.g. MCP subprocesses on
		// Windows where %USERPROFILE% is not propagated).
		if home := resolveHomeFallback(); home != "" {
			log.Printf("[recalldock] UserHomeDir failed, using fallback: %s", home)
			cfg = store.FallbackConfig(store.DefaultDataDirForHome(home))
		} else {
			fatal(cfgErr)
		}
	}

	// Allow overriding data dir via env
	if dir := os.Getenv("RECALLDOCK_DATA_DIR"); dir != "" {
		cfg.DataDir = dir
	}
	if expectedStoreID := strings.TrimSpace(os.Getenv("RECALLDOCK_EXPECT_STORE_ID")); expectedStoreID != "" {
		cfg.ExpectedStoreID = expectedStoreID
	}
	if os.Args[1] == "candidate-hook" {
		cmdCandidateHook(cfg)
		return
	}
	if os.Args[1] == "host-event" {
		if err := runHostEvent(cfg, os.Args[2:], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "RecallDock host event:", err)
			exitFunc(1)
		}
		return
	}

	cfg.RequireExisting = requiresExistingStore(os.Args[1:])
	// Never move discovered databases on ordinary command startup. An operator
	// must explicitly export/import a selected backed-up source.

	switch os.Args[1] {
	case "candidates":
		cmdCandidates(cfg)
	case "serve":
		cmdServe(cfg)
	case "mcp":
		cmdMCP(cfg)
	case "tui":
		cmdTUI(cfg)
	case "ui":
		cmdUI(cfg)
	case "search":
		cmdSearch(cfg)
	case "save":
		cmdSave(cfg)
	case "delete":
		cmdDelete(cfg)
	case "timeline":
		cmdTimeline(cfg)
	case "conflicts":
		cmdConflicts(cfg)
	case "review":
		cmdReview(cfg)
	case "doctor":
		cmdDoctor(cfg)
	case "context":
		cmdContext(cfg)
	case "stats":
		cmdStats(cfg)
	case "memory":
		cmdMemory(cfg)
	case "resources":
		cmdResources(cfg)
	case "backup":
		cmdBackup(cfg)
	case "export":
		cmdExport(cfg)
	case "import":
		cmdImport(cfg)
	case "sync":
		cmdSync(cfg)
	case "cloud":
		cmdCloud(cfg)
	case "obsidian-export":
		cmdObsidianExport(cfg)
	case "projects":
		cmdProjects(cfg)
	case "store":
		cmdStore(cfg)
	case "setup":
		cmdSetup(cfg)
	case "protocol-mode":
		cmdProtocolMode(cfg)
	case "version", "--version", "-v":
		fmt.Printf("recalldock %s\n", version)
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		printUsage()
		exitFunc(1)
	}
}

// Local-only production commands never perform release network requests, even
// if stale legacy release configuration remains in the environment.
func shouldCheckForUpdates(_ []string) bool { return false }

func handleConfigFreeCommand(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(args[0])) {
	case "gateway":
		cmdGateway(args[1:])
		return true
	case "work":
		cmdWorklog(args[1:])
		return true
	case "version", "--version", "-v":
		fmt.Printf("recalldock %s\n", version)
		return true
	case "help", "--help", "-h":
		printUsage()
		return true
	case "cloud":
		if len(args) >= 2 {
			subcommand := strings.ToLower(strings.TrimSpace(args[1]))
			if subcommand == "--help" || subcommand == "-h" || subcommand == "help" {
				cmdCloud(store.Config{})
				return true
			}
		}
	}
	return false
}

func printUpdateCheckResult(result versioncheck.CheckResult) {
	if result.Status != versioncheck.StatusUpToDate && result.Message != "" {
		fmt.Fprintln(os.Stderr, result.Message)
		fmt.Fprintln(os.Stderr)
	}
}

func cmdCandidateHook(cfg store.Config) {
	input, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		return
	}
	endpoint := strings.TrimSpace(os.Getenv("RECALLDOCK_URL"))
	if endpoint == "" {
		port := strings.TrimSpace(os.Getenv("RECALLDOCK_PORT"))
		if port == "" {
			port = "7437"
		}
		endpoint = "http://127.0.0.1:" + port
	}
	err = candidatehook.Run(context.Background(), input, candidatehook.Config{
		Endpoint: endpoint, OutboxPath: filepath.Join(cfg.DataDir, "candidate-outbox.json"),
		Timeout: 2 * time.Second, TTL: 7 * 24 * time.Hour, Capacity: 32,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "recalldock: candidate hook fallback failed")
	}
}

// ─── Commands ────────────────────────────────────────────────────────────────

func cmdServe(cfg store.Config) {
	port := 7437
	portValue := os.Getenv("RECALLDOCK_PORT")
	if len(os.Args) > 3 {
		fatal(errors.New("usage: recalldock serve [port]"))
		return
	}
	if len(os.Args) > 2 {
		portValue = os.Args[2]
	}
	if portValue != "" {
		n, err := strconv.Atoi(portValue)
		if err != nil || n < 1 || n > 65535 {
			fatal(errors.New("serve port must be 1..65535"))
			return
		}
		port = n
	}

	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
	}
	defer s.Close()

	srv := newHTTPServer(s, port)
	srv.Version = version

	// No cloud transports, runner subprocesses or saved cloud config are consulted.
	srv.SetSyncStatus(localOnlySyncStatus{})
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-sigCh:
			log.Println("[recalldock] shutting down...")
			exitFunc(0)
		case <-done:
			return
		}
	}()

	if err := startHTTP(srv); err != nil {
		fatal(err)
	}
}

func resolveServeSyncStatusProject() string {
	projectName := strings.TrimSpace(os.Getenv("RECALLDOCK_PROJECT"))
	if projectName == "" {
		if cwd, err := os.Getwd(); err == nil {
			projectName = detectProject(cwd)
		}
	}
	projectName, _ = store.NormalizeProject(projectName)
	return strings.TrimSpace(projectName)
}

// tryStartAutosync starts the autosync Manager if RECALLDOCK_CLOUD_AUTOSYNC=1 and
// both RECALLDOCK_CLOUD_TOKEN and RECALLDOCK_CLOUD_SERVER are present.
// REQ-210: only exact "1" is accepted. REQ-211: missing token/server → log+skip.
// Never fatal — autosync is optional.
// BW7: Returns (status provider, stop func) so the caller can invoke stop
// before os.Exit to ensure the Manager releases its sync lease.
func tryStartAutosync(ctx context.Context, s *store.Store, cfg store.Config) (autosyncStatusProvider, func()) {
	// REQ-210: opt-in requires exact "1".
	if strings.TrimSpace(os.Getenv("RECALLDOCK_CLOUD_AUTOSYNC")) != "1" {
		return nil, nil
	}

	cc, err := resolveCloudRuntimeConfig(cfg)
	if err != nil {
		log.Printf("[autosync] ERROR: cannot read cloud config: %v", err)
		return nil, nil
	}

	token := strings.TrimSpace(cc.Token)
	serverURL := strings.TrimSpace(cc.ServerURL)

	// REQ-211: token required. The token is resolved from cloud.json first and
	// overridden by RECALLDOCK_CLOUD_TOKEN when set, so both sources are tried.
	// On Windows (Task Scheduler), the env var is often absent — the file path
	// is the expected source (issue #421).
	if token == "" {
		log.Printf("[autosync] ERROR: cloud token is not configured (set RECALLDOCK_CLOUD_TOKEN or store token in cloud.json via `recalldock cloud config`); autosync disabled")
		return nil, nil
	}
	// REQ-211: server URL required. Resolved from cloud.json or RECALLDOCK_CLOUD_SERVER.
	if serverURL == "" {
		log.Printf("[autosync] ERROR: cloud server URL is not configured (set RECALLDOCK_CLOUD_SERVER or run `recalldock cloud config --server <url>`); autosync disabled")
		return nil, nil
	}

	remoteMT, err := remote.NewMutationTransport(serverURL, token)
	if err != nil {
		log.Printf("[autosync] ERROR: invalid server URL %q: %v; autosync disabled", serverURL, err)
		return nil, nil
	}
	transport := &mutationTransportAdapter{remote: remoteMT}
	mgrCfg := autosync.DefaultConfig()
	// BR2-3: Call newAutosyncManager (injectable) instead of autosync.New directly,
	// so tests can stub the factory and avoid real goroutine/network side effects.
	mgr := newAutosyncManager(s, transport, mgrCfg)

	go mgr.Run(ctx)
	log.Printf("[autosync] started (server=%s)", serverURL)
	return mgr, mgr.Stop
}

func cmdMCP(cfg store.Config) {
	rawArgs := os.Args[2:]
	autoProject := false
	filtered := make([]string, 0, len(rawArgs))
	for _, arg := range rawArgs {
		if arg == "--auto-project" {
			if autoProject {
				fatal(fmt.Errorf("--auto-project specified more than once"))
				return
			}
			autoProject = true
		} else {
			filtered = append(filtered, arg)
		}
	}
	args, required, parseErr := extractRequiredMCPScope(filtered)
	if parseErr != nil {
		fatal(parseErr)
		return
	}
	toolsFilter, projectOverride, parseErr := parseMCPArguments(args)
	if parseErr != nil {
		fatal(parseErr)
		return
	}
	if autoProject && (required || projectOverride != "") {
		fatal(fmt.Errorf("--auto-project cannot be combined with explicit project selection or --require-project"))
		return
	}

	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
	}
	defer s.Close()
	if autoProject {
		cwd, cwdErr := os.Getwd()
		if cwdErr != nil {
			fatal(cwdErr)
			return
		}
		scope, scopeErr := ensureHostProject(s, cfg.DataDir, cwd)
		if scopeErr != nil {
			fatal(scopeErr)
			return
		}
		projectOverride = scope.Project
	}
	if required {
		scope, scopeErr := resolveCLIReadScope(s, projectOverride, false)
		if scopeErr != nil {
			fatal(scopeErr)
			return
		}
		if !scope.Bound {
			fatal(fmt.Errorf("RecallDock project is not bound; bind this workspace or explicitly activate a historical project before restarting the host session"))
			return
		}
		projectOverride = scope.Project
	}

	// Stdio is local IPC. Legacy cloud environment cannot enable autosync here.

	mcpCfg := mcp.MCPConfig{DefaultProject: projectOverride, Version: version, ReadBudgetID: strings.TrimSpace(os.Getenv("RECALLDOCK_READ_BUDGET_ID")), HostOwnsLifecycle: required || autoProject}
	allowlist := resolveMCPTools(toolsFilter)
	mcpSrv := newMCPServerWithConfig(s, mcpCfg, allowlist)

	if err := serveMCP(mcpSrv); err != nil {
		fatal(err)
	}
}

func cmdTUI(cfg store.Config) {
	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
	}
	defer s.Close()

	model := newTUIModel(s)
	p := newTeaProgram(model)
	if _, err := runTeaProgram(p); err != nil {
		fatal(err)
	}
}

func cmdSearch(cfg store.Config) {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: recalldock search <query> [--type TYPE] [--project PROJECT] [--scope SCOPE] [--limit N] [--since DATE] [--until DATE] [--strategy flat|hierarchical|auto] [--detail l0|l1] [--token-budget N] [--root-path PATH] [--max-depth N] [--trace] [--explain-empty] [--include-stale] [--json]")
		exitFunc(1)
	}

	allProjects, parseErr := validateSearchArguments(os.Args[2:])
	if parseErr != nil {
		fatal(parseErr)
		return
	}
	options, queryParts, parseErr := parseSearchArguments(os.Args[2:])
	if parseErr != nil {
		fatal(parseErr)
		return
	}
	opts := store.SearchOptions{Limit: 10, DeferRecallTracking: true}
	opts.Type, opts.Project = options.Values["--type"], options.Values["--project"]
	opts.Scope, opts.MatchMode = options.Values["--scope"], options.Values["--match-mode"]
	opts.Profile, opts.Strategy = options.Values["--profile"], options.Values["--strategy"]
	opts.Detail, opts.RootPath = options.Values["--detail"], options.Values["--root-path"]
	opts.Since, opts.Until = options.Values["--since"], options.Values["--until"]
	opts.Limit, parseErr = options.integer("--limit", 10, 1, 20)
	if parseErr != nil {
		fatal(parseErr)
		return
	}
	opts.TokenBudget, parseErr = options.integer("--token-budget", 0, 1, 32000)
	if parseErr != nil {
		fatal(parseErr)
		return
	}
	opts.MaxDepth, parseErr = options.integer("--max-depth", 0, 0, 64)
	if parseErr != nil {
		fatal(parseErr)
		return
	}
	opts.RawFTS, opts.Trace = options.Flags["--raw-fts"], options.Flags["--trace"]
	opts.ExplainEmpty, opts.IncludeStale = options.Flags["--explain-empty"], options.Flags["--include-stale"]
	jsonOutput := options.Flags["--json"]
	detailedRequested := jsonOutput || opts.RawFTS || opts.Trace || opts.ExplainEmpty || opts.IncludeStale
	for _, key := range []string{"--strategy", "--detail", "--token-budget", "--root-path", "--max-depth"} {
		if _, ok := options.Values[key]; ok {
			detailedRequested = true
		}
	}

	query := strings.Join(queryParts, " ")
	if query == "" {
		fmt.Fprintln(os.Stderr, "error: search query is required")
		exitFunc(1)
		return
	}
	if err := store.NormalizeSearchOptions(&opts); err != nil {
		fatal(err)
		return
	}

	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
		return
	}
	defer s.Close()

	selected, scopeErr := resolveCLIReadScope(s, opts.Project, allProjects)
	if scopeErr != nil {
		fatal(scopeErr)
		return
	}
	opts.Project = selected.Project
	if !selected.Bound && opts.Type != "" && !store.IsBuiltinObservationType(opts.Type) {
		fatal(fmt.Errorf("%w: select --project or --all to validate a stored custom type %q", store.ErrInvalidType, store.QueryPreview(opts.Type)))
		return
	}
	if !jsonOutput {
		fmt.Printf("Scope: %s\n", selected.Label)
	}
	var results []store.SearchResult
	var detailed *store.SearchResponse
	if !selected.Bound {
		detailed = &store.SearchResponse{Results: []store.SearchResult{}, StrategyUsed: "none", FallbackReason: "unbound_cwd"}
	} else if detailedRequested {
		detailed, err = storeSearchDetailed(s, query, opts)
		if err == nil {
			results = detailed.Results
		}
	} else {
		results, err = storeSearch(s, query, opts)
	}
	if err != nil {
		fatal(err)
		return
	}

	if jsonOutput {
		compactResults := make([]map[string]any, 0, len(results))
		for _, result := range results {
			entry := map[string]any{
				"id": result.ID, "sync_id": result.SyncID, "type": result.Type,
				"memory_class": result.MemoryClass, "memory_path": result.MemoryPath,
				"selected_detail": result.SelectedDetail, "estimated_tokens": result.EstimatedTokens,
				"selected_content": result.SelectedContent,
				"score":            result.Score, "score_source": result.ScoreSource, "selection_reason": result.SelectionReason, "state": result.State(),
			}
			if result.RawBM25 != nil {
				entry["raw_bm25"] = *result.RawBM25
			}
			if result.DetailDowngraded != nil {
				entry["detail_downgraded"] = result.DetailDowngraded
			}
			if result.StaleAt != nil {
				entry["stale_at"] = *result.StaleAt
			}
			if result.StaleReason != nil {
				entry["stale_reason"] = *result.StaleReason
			}
			compactResults = append(compactResults, entry)
		}
		payload := map[string]any{
			"query": store.QueryPreview(query), "query_truncated": query != store.QueryPreview(query), "scope": selected.Label, "project": selected.Project, "results": compactResults, "strategy_used": detailed.StrategyUsed,
			"fallback_reason": detailed.FallbackReason, "latency_ms": detailed.LatencyMS,
			"budget": detailed.Budget, "trace": detailed.Trace,
			"recall_summary": detailed.RecallSummary,
			"empty_reason":   detailed.EmptyReason, "did_you_mean": detailed.DidYouMean,
			"empty_explanation": detailed.EmptyExplanation,
			"query_groups":      detailed.QueryGroups,
			"query_terms":       detailed.QueryTerms, "ignored_terms": detailed.IgnoredTerms,
		}
		var encoded []byte
		for {
			payload["results"], payload["budget"], payload["recall_summary"] = compactResults, detailed.Budget, detailed.RecallSummary
			var marshalErr error
			encoded, marshalErr = jsonMarshalIndent(payload, "", "  ")
			if marshalErr != nil {
				fatal(marshalErr)
				return
			}
			cost := store.EstimateReadTokens(string(encoded) + "\n")
			if detailed.Budget.Used != cost {
				detailed.Budget.Used = cost
				continue
			}
			if opts.TokenBudget == 0 || cost <= opts.TokenBudget {
				break
			}
			if len(results) <= 1 {
				fatal(fmt.Errorf("%w: CLI response needs %d estimated tokens; increase token_budget or omit it", store.ErrBudgetTooSmall, cost))
				return
			}
			results = results[:len(results)-1]
			compactResults = compactResults[:len(compactResults)-1]
			detailed.Budget.Truncated = true
			detailed.RecallSummary.BudgetDropped++
		}
		if _, err := fmt.Fprintln(os.Stdout, string(encoded)); err != nil {
			fatal(err)
			return
		}
		s.RecordSearchRecalls(results)
		return
	}

	if len(results) == 0 {
		fmt.Printf("No memories found for: %q\n", store.QueryPreview(query))
		return
	}

	var output strings.Builder
	fmt.Fprintf(&output, "Found %d memories:\n\n", len(results))
	for i, r := range results {
		project := ""
		if r.Project != nil {
			project = fmt.Sprintf(" | project: %s", *r.Project)
		}
		content := r.SelectedContent
		if content == "" && !detailedRequested {
			content = truncate(r.Content, 300)
		}
		state := ""
		if r.State() == store.ObservationStateNeedsReview {
			state = " | state: needs_review"
		}
		if r.StaleReason != nil {
			state += " | stale_reason: " + *r.StaleReason
		}
		fmt.Fprintf(&output, "[%d] #%d (%s) — %s\n    %s\n    %s%s | scope: %s%s\n\n",
			i+1, r.ID, r.Type, r.Title,
			truncate(content, 300),
			timeutil.FormatLocal(r.CreatedAt), project, r.Scope, state)
	}
	if _, err := fmt.Fprint(os.Stdout, output.String()); err != nil {
		fatal(err)
		return
	}
	s.RecordSearchRecalls(results)
}

func cmdReview(cfg store.Config) {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: recalldock review <list|mark|link|dependencies|process|resource-link|resource-dependencies|resource-changed|resource-refresh> [options]")
		exitFunc(1)
		return
	}
	options, err := parseReviewOptions(os.Args[2:])
	if err != nil {
		fatal(err)
		return
	}
	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
		return
	}
	defer s.Close()
	jsonOutput := options.Flags["--json"]
	printJSON := func(value any) {
		encoded, marshalErr := jsonMarshalIndent(value, "", "  ")
		if marshalErr != nil {
			fatal(marshalErr)
			return
		}
		fmt.Println(string(encoded))
	}

	switch os.Args[2] {
	case "list":
		selected, err := resolveCLIReadScope(s, options.Values["--project"], options.Flags["--all"])
		if err != nil {
			fatal(err)
			return
		}
		if !selected.Bound {
			fmt.Fprintln(os.Stderr, selected.Label)
			if jsonOutput {
				printJSON(map[string]any{"observations": []any{}})
			} else {
				fmt.Println("No memories need review.")
			}
			return
		}
		projectName := selected.Project
		limit, err := options.integer("--limit", 10, 1, 20)
		if err != nil {
			fatal(err)
			return
		}
		observations, err := s.ObservationsNeedingReview(projectName, limit, options.Values["--status"])
		if err != nil {
			fatal(err)
			return
		}
		items := make([]map[string]any, 0, len(observations))
		for _, obs := range observations {
			item := map[string]any{"id": obs.ID, "sync_id": obs.SyncID, "type": obs.Type, "title": obs.Title, "state": obs.State()}
			if obs.Project != nil {
				item["project"] = *obs.Project
			}
			if obs.ReviewAfter != nil {
				item["review_after"] = *obs.ReviewAfter
			}
			if obs.StaleAt != nil {
				item["stale_at"] = *obs.StaleAt
			}
			if obs.StaleReason != nil {
				item["stale_reason"] = *obs.StaleReason
			}
			dependencies, depErr := s.ListMemoryDependencies(store.MemoryDependencyListOptions{Project: projectName, DependentObservationSyncID: obs.SyncID, Limit: 50})
			if depErr != nil {
				fatal(depErr)
				return
			}
			item["dependencies"] = dependencies
			resourceDependencies, resourceErr := s.ListResourceDependencies(store.ResourceDependencyListOptions{Project: projectName, DependentObservationSyncID: obs.SyncID, Limit: 50})
			if resourceErr != nil {
				fatal(resourceErr)
				return
			}
			item["resource_dependencies"] = resourceDependencies
			items = append(items, item)
		}
		if jsonOutput {
			printJSON(map[string]any{"observations": items})
			return
		}
		if len(items) == 0 {
			fmt.Println("No memories need review.")
			return
		}
		for _, item := range items {
			fmt.Printf("#%d (%s) %s | state=%s", item["id"], item["type"], item["title"], item["state"])
			if reason, ok := item["stale_reason"]; ok {
				fmt.Printf(" | stale_reason=%s", reason)
			}
			fmt.Println()
		}

	case "mark":
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "usage: recalldock review mark <observation_id> [--json]")
			exitFunc(1)
			return
		}
		id, parseErr := strconv.ParseInt(os.Args[3], 10, 64)
		if parseErr != nil || id <= 0 {
			fatal(fmt.Errorf("invalid observation id %q", os.Args[3]))
			return
		}
		jsonOutput = hasArg(os.Args[4:], "--json")
		if _, err := requireCLIObservationWrite(s, id); err != nil {
			fatal(err)
			return
		}
		if err := s.MarkReviewed(id); err != nil {
			fatal(err)
			return
		}
		obs, err := s.GetObservation(id)
		if err != nil {
			fatal(err)
			return
		}
		if jsonOutput {
			printJSON(map[string]any{"id": obs.ID, "sync_id": obs.SyncID, "state": obs.State(), "stale_at": obs.StaleAt, "stale_reason": obs.StaleReason})
			return
		}
		fmt.Printf("Memory #%d review cycle reset; state=%s\n", obs.ID, obs.State())

	case "link":
		if len(os.Args) < 5 {
			fmt.Fprintln(os.Stderr, "usage: recalldock review link <source_observation_id> <dependent_observation_id> [--reason TEXT] [--json]")
			exitFunc(1)
			return
		}
		sourceID, sourceErr := strconv.ParseInt(os.Args[3], 10, 64)
		dependentID, dependentErr := strconv.ParseInt(os.Args[4], 10, 64)
		if sourceErr != nil || dependentErr != nil || sourceID <= 0 || dependentID <= 0 {
			fatal(errors.New("source and dependent observation ids must be positive integers"))
			return
		}
		if _, err := requireCLIObservationWrite(s, sourceID); err != nil {
			fatal(err)
			return
		}
		if _, err := requireCLIObservationWrite(s, dependentID); err != nil {
			fatal(err)
			return
		}
		reason := options.Values["--reason"]
		dependency, err := s.AddMemoryDependency(store.AddMemoryDependencyParams{SourceObservationID: sourceID, DependentObservationID: dependentID, Reason: reason})
		if err != nil {
			fatal(err)
			return
		}
		if jsonOutput {
			printJSON(dependency)
			return
		}
		fmt.Printf("Dependency %s linked: #%d -> #%d at source revision %d\n", dependency.SyncID, sourceID, dependentID, dependency.SourceRevision)

	case "dependencies":
		selected, err := resolveCLIReadScope(s, options.Values["--project"], options.Flags["--all"])
		if err != nil {
			fatal(err)
			return
		}
		if !selected.Bound {
			fmt.Fprintln(os.Stderr, selected.Label)
			if jsonOutput {
				printJSON(map[string]any{"dependencies": []any{}})
			} else {
				fmt.Println("No memory dependencies found.")
			}
			return
		}
		limit, err := options.integer("--limit", 50, 1, 500)
		if err != nil {
			fatal(err)
			return
		}
		opts := store.MemoryDependencyListOptions{
			Project: selected.Project, Status: options.Values["--status"],
			SourceObservationSyncID:    options.Values["--source"],
			DependentObservationSyncID: options.Values["--dependent"], Limit: limit,
		}
		dependencies, err := s.ListMemoryDependencies(opts)
		if err != nil {
			fatal(err)
			return
		}
		if jsonOutput {
			printJSON(map[string]any{"dependencies": dependencies})
			return
		}
		if len(dependencies) == 0 {
			fmt.Println("No memory dependencies found.")
			return
		}
		for _, dependency := range dependencies {
			fmt.Printf("%s | #%d -> #%d | status=%s | source_revision=%d\n", dependency.SyncID, dependency.SourceObservationID, dependency.DependentObservationID, dependency.Status, dependency.SourceRevision)
		}

	case "resource-link":
		if len(os.Args) < 5 {
			fmt.Fprintln(os.Stderr, "usage: recalldock review resource-link <source_ref> <dependent_observation_id> --project PROJECT [--reason TEXT] [--json]")
			exitFunc(1)
			return
		}
		dependentID, parseErr := strconv.ParseInt(os.Args[4], 10, 64)
		if parseErr != nil || dependentID <= 0 {
			fatal(fmt.Errorf("invalid dependent observation id %q", os.Args[4]))
			return
		}
		projectName, reason := options.Values["--project"], options.Values["--reason"]
		if strings.TrimSpace(projectName) == "" {
			fatal(errors.New("--project is required for resource-link"))
			return
		}
		projectName, err = s.ResolveExistingProject(projectName)
		if err != nil {
			fatal(err)
			return
		}
		if _, err := resolveCLIWriteOwner(s, projectName); err != nil {
			fatal(err)
			return
		}
		if _, err := requireCLIObservationWrite(s, dependentID); err != nil {
			fatal(err)
			return
		}
		dependency, err := s.AddResourceDependency(store.AddResourceDependencyParams{
			SourceRef: os.Args[3], DependentObservationID: dependentID, Project: projectName, Reason: reason,
		})
		if err != nil {
			fatal(err)
			return
		}
		if jsonOutput {
			printJSON(dependency)
			return
		}
		fmt.Printf("Resource dependency %s linked to memory #%d; resource content remains outside the memory store.\n", dependency.SyncID, dependentID)

	case "resource-dependencies":
		selected, err := resolveCLIReadScope(s, options.Values["--project"], options.Flags["--all"])
		if err != nil {
			fatal(err)
			return
		}
		if !selected.Bound {
			fmt.Fprintln(os.Stderr, selected.Label)
			if jsonOutput {
				printJSON(map[string]any{"resource_dependencies": []any{}})
			} else {
				fmt.Println("No resource dependencies found.")
			}
			return
		}
		limit, err := options.integer("--limit", 50, 1, 500)
		if err != nil {
			fatal(err)
			return
		}
		opts := store.ResourceDependencyListOptions{
			Project: selected.Project, Status: options.Values["--status"],
			SourceRef:                  options.Values["--source-ref"],
			DependentObservationSyncID: options.Values["--dependent"], Limit: limit,
		}
		dependencies, err := s.ListResourceDependencies(opts)
		if err != nil {
			fatal(err)
			return
		}
		if jsonOutput {
			printJSON(map[string]any{"resource_dependencies": dependencies})
			return
		}
		if len(dependencies) == 0 {
			fmt.Println("No resource dependencies found.")
			return
		}
		for _, dependency := range dependencies {
			fmt.Printf("%s | %s -> #%d | project=%s | status=%s\n", dependency.SyncID, dependency.SourceRef, dependency.DependentObservationID, dependency.Project, dependency.Status)
		}

	case "resource-changed":
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "usage: recalldock review resource-changed <source_ref> --project PROJECT [--replacement-source-ref REF] [--reason TEXT] [--json]")
			exitFunc(1)
			return
		}
		projectName, replacement, reason := options.Values["--project"], options.Values["--replacement-source-ref"], options.Values["--reason"]
		if strings.TrimSpace(projectName) == "" {
			fatal(errors.New("--project is required for resource-changed"))
			return
		}
		projectName, err = s.ResolveExistingProject(projectName)
		if err != nil {
			fatal(err)
			return
		}
		if _, err := resolveCLIWriteOwner(s, projectName); err != nil {
			fatal(err)
			return
		}
		result, err := s.NotifyResourceChanged(store.NotifyResourceChangeParams{
			Project: projectName, SourceRef: os.Args[3], ReplacementSourceRef: replacement, Reason: reason,
		})
		if err != nil {
			fatal(err)
			return
		}
		if jsonOutput {
			printJSON(result)
			return
		}
		fmt.Printf("Marked %d of %d resource dependencies stale; memory content was not rewritten.\n", result.Staled, result.Matched)

	case "resource-refresh":
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "usage: recalldock review resource-refresh <resource_dependency_sync_id> --project PROJECT [--source-ref REF] [--reason TEXT] [--json]")
			exitFunc(1)
			return
		}
		projectName, sourceRef, reason := options.Values["--project"], options.Values["--source-ref"], options.Values["--reason"]
		if strings.TrimSpace(projectName) == "" {
			fatal(errors.New("--project is required for resource-refresh"))
			return
		}
		projectName, err = s.ResolveExistingProject(projectName)
		if err != nil {
			fatal(err)
			return
		}
		if _, err := resolveCLIWriteOwner(s, projectName); err != nil {
			fatal(err)
			return
		}
		dependency, err := s.RefreshResourceDependency(store.RefreshResourceDependencyParams{
			SyncID: os.Args[3], Project: projectName, SourceRef: sourceRef, Reason: reason,
		})
		if err != nil {
			fatal(err)
			return
		}
		if jsonOutput {
			printJSON(dependency)
			return
		}
		fmt.Printf("Resource dependency %s refreshed to %s.\n", dependency.SyncID, dependency.SourceRef)

	case "process":
		limit, err := options.integer("--limit", 50, 1, 500)
		if err != nil {
			fatal(err)
			return
		}
		result, err := s.ProcessDependencyInvalidations(limit)
		if err != nil {
			fatal(err)
			return
		}
		if jsonOutput {
			printJSON(result)
			return
		}
		fmt.Printf("Processed %d dependency invalidations; %d remain.\n", result.Processed, result.Remaining)

	default:
		fmt.Fprintf(os.Stderr, "unknown review subcommand: %s\n", os.Args[2])
		exitFunc(1)
	}
}

func hasArg(args []string, target string) bool {
	for _, arg := range args {
		if arg == target {
			return true
		}
	}
	return false
}

func cmdSave(cfg store.Config) {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: recalldock save <title> <content> [--type TYPE] [--project PROJECT] [--scope SCOPE] [--topic TOPIC_KEY] [--memory-class CLASS] [--memory-path PATH] [--abstract TEXT] [--overview TEXT]")
		exitFunc(1)
		return
	}

	if err := validateSaveArguments(os.Args[4:]); err != nil {
		fatal(err)
		return
	}

	title := os.Args[2]
	content := os.Args[3]
	typ := "manual"
	project := ""
	scope := "project"
	topicKey := ""
	memoryClass := ""
	memoryPath := ""
	abstract := ""
	overview := ""

	for i := 4; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--type":
			if i+1 < len(os.Args) {
				typ = os.Args[i+1]
				i++
			}
		case "--project":
			if i+1 < len(os.Args) {
				project = os.Args[i+1]
				i++
			}
		case "--scope":
			if i+1 < len(os.Args) {
				scope = os.Args[i+1]
				i++
			}
		case "--topic":
			if i+1 < len(os.Args) {
				topicKey = os.Args[i+1]
				i++
			}
		case "--memory-class":
			if i+1 < len(os.Args) {
				memoryClass = os.Args[i+1]
				i++
			}
		case "--memory-path":
			if i+1 < len(os.Args) {
				memoryPath = os.Args[i+1]
				i++
			}
		case "--abstract":
			if i+1 < len(os.Args) {
				abstract = os.Args[i+1]
				i++
			}
		case "--overview":
			if i+1 < len(os.Args) {
				overview = os.Args[i+1]
				i++
			}
		}
	}

	if project != "" {
		normalizedProject, warning := store.NormalizeProject(project)
		project = normalizedProject
		if warning != "" {
			fmt.Fprintln(os.Stderr, warning)
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		fatal(err)
		return
	}
	if err := preflightCLISaveScope(cwd, project); err != nil {
		fatal(err)
		return
	}
	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
		return
	}
	defer s.Close()
	selected, err := resolveCLISaveScope(s, cwd, project)
	if err != nil {
		fatal(err)
		return
	}
	project, cwd, sessionID := selected.Project, selected.Directory, selected.SessionID

	if err := s.CreateSessionWithOrigin(sessionID, project, cwd, store.SessionOriginOperator); err != nil {
		fatal(err)
	}
	id, err := storeAddObservation(s, store.AddObservationParams{
		SessionID: sessionID,
		Type:      typ,
		Title:     title,
		Content:   content,
		Project:   project,
		Scope:     scope,
		TopicKey:  topicKey, MemoryClass: memoryClass, MemoryPath: memoryPath,
		Abstract: abstract, Overview: overview,
	})
	if err != nil {
		fatal(err)
	}

	fmt.Printf("Memory saved: #%d %q (%s)\n", id, title, typ)
	fmt.Printf("Project: %s | scope: %s\n", project, scope)
}

func cmdDelete(cfg store.Config) {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: recalldock delete <observation_id> [--hard]")
		fmt.Fprintln(os.Stderr, "       recalldock delete session  <id>")
		fmt.Fprintln(os.Stderr, "       recalldock delete prompt   <id>")
		fmt.Fprintln(os.Stderr, "       recalldock delete project  <name> [--hard]")
		exitFunc(1)
		return
	}

	sub := os.Args[2]
	switch sub {
	case "session":
		cmdDeleteSession(cfg)
	case "prompt":
		cmdDeletePrompt(cfg)
	case "project":
		cmdDeleteProject(cfg)
	default:
		// Backward-compat: treat the second arg as a numeric observation ID.
		cmdDeleteObservation(cfg)
	}
}

func cmdDeleteObservation(cfg store.Config) {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: recalldock delete <observation_id> [--hard]")
		exitFunc(1)
		return
	}

	id, err := strconv.ParseInt(os.Args[2], 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: invalid observation id %q\n", os.Args[2])
		exitFunc(1)
		return
	}

	options, err := parseCLIOptions(os.Args[3:], nil, []string{"--hard"})
	if err != nil {
		fatal(err)
		return
	}
	hard := options.Flags["--hard"]

	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
		return
	}
	defer s.Close()

	if _, err := requireCLIObservationWrite(s, id); err != nil {
		fatal(err)
		return
	}
	if err := storeDeleteObservation(s, id, hard); err != nil {
		fatal(err)
		return
	}

	kind := "soft-deleted"
	if hard {
		kind = "hard-deleted"
	}
	fmt.Printf("Observation #%d %s\n", id, kind)
}

func cmdDeleteSession(cfg store.Config) {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: recalldock delete session <id>")
		exitFunc(1)
		return
	}

	id := os.Args[3]
	if _, err := parseCLIOptions(os.Args[4:], nil, nil); err != nil {
		fatal(err)
		return
	}

	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
		return
	}
	defer s.Close()

	owner, err := s.SessionProject(id)
	if err != nil {
		fatal(fmt.Errorf("session %q not found: %w", id, err))
		return
	}
	if _, err := resolveCLIWriteOwner(s, owner); err != nil {
		fatal(err)
		return
	}
	if err := storeDeleteSession(s, id); err != nil {
		fatal(err)
		return
	}
	fmt.Printf("Session %q deleted\n", id)
}

func cmdDeletePrompt(cfg store.Config) {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: recalldock delete prompt <id>")
		exitFunc(1)
		return
	}

	id, err := strconv.ParseInt(os.Args[3], 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: invalid prompt id %q\n", os.Args[3])
		exitFunc(1)
		return
	}

	if _, err := parseCLIOptions(os.Args[4:], nil, nil); err != nil {
		fatal(err)
		return
	}
	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
		return
	}
	defer s.Close()

	owner, err := s.PromptProject(id)
	if err != nil {
		fatal(fmt.Errorf("prompt %d not found: %w", id, err))
		return
	}
	if _, err := resolveCLIWriteOwner(s, owner); err != nil {
		fatal(err)
		return
	}
	if err := storeDeletePrompt(s, id); err != nil {
		fatal(err)
		return
	}
	fmt.Printf("Prompt #%d deleted\n", id)
}

func cmdDeleteProject(cfg store.Config) {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: recalldock delete project <name> [--hard]")
		exitFunc(1)
		return
	}

	name := os.Args[3]
	options, err := parseCLIOptions(os.Args[4:], nil, []string{"--hard", "--confirm"})
	if err != nil {
		fatal(err)
		return
	}
	hard := options.Flags["--hard"]
	if hard && !options.Flags["--confirm"] {
		fatal(errors.New("hard project deletion requires --confirm"))
		return
	}

	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
		return
	}
	defer s.Close()

	name, err = s.ResolveExistingProject(name)
	if err != nil {
		fatal(err)
		return
	}
	if _, err := resolveCLIWriteOwner(s, name); err != nil {
		fatal(err)
		return
	}
	result, err := storeDeleteProject(s, name, hard)
	if err != nil {
		fatal(err)
		return
	}

	kind := "soft-deleted"
	if hard {
		kind = "hard-deleted"
	}
	fmt.Printf("Project %q %s: %d observation(s), %d prompt(s), %d session(s)\n",
		result.Project, kind, result.ObservationsDeleted, result.PromptsDeleted, result.SessionsDeleted)
}

func cmdTimeline(cfg store.Config) {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: recalldock timeline <observation_id> [--before N] [--after N]")
		exitFunc(1)
	}

	obsID, err := strconv.ParseInt(os.Args[2], 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: invalid observation id %q\n", os.Args[2])
		exitFunc(1)
	}

	options, err := parseCLIOptions(os.Args[3:], []string{"--before", "--after"}, []string{"--json"})
	if err != nil {
		fatal(err)
		return
	}
	before, err := options.integer("--before", 5, 0, 50)
	if err != nil {
		fatal(err)
		return
	}
	after, err := options.integer("--after", 5, 0, 50)
	if err != nil {
		fatal(err)
		return
	}

	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
	}
	defer s.Close()

	result, err := storeTimeline(s, obsID, before, after)
	if err != nil {
		fatal(err)
		return
	}
	if options.Flags["--json"] {
		if err := writeCLIJSON(result); err != nil {
			fatal(err)
		}
		return
	}

	// Session header
	if result.SessionInfo != nil {
		summary := ""
		if result.SessionInfo.Summary != nil {
			summary = fmt.Sprintf(" — %s", truncate(*result.SessionInfo.Summary, 100))
		}
		fmt.Printf("Session: %s (%s)%s\n", result.SessionInfo.Project, result.SessionInfo.StartedAt, summary)
		fmt.Printf("Total observations in session: %d\n\n", result.TotalInRange)
	}

	// Before
	if len(result.Before) > 0 {
		fmt.Println("─── Before ───")
		for _, e := range result.Before {
			fmt.Printf("  #%d [%s] %s — %s\n", e.ID, e.Type, e.Title, truncate(e.Content, 150))
		}
		fmt.Println()
	}

	// Focus
	fmt.Printf(">>> #%d [%s] %s <<<\n", result.Focus.ID, result.Focus.Type, result.Focus.Title)
	fmt.Printf("    %s\n", truncate(result.Focus.Content, 500))
	fmt.Printf("    %s\n\n", timeutil.FormatLocal(result.Focus.CreatedAt))

	// After
	if len(result.After) > 0 {
		fmt.Println("─── After ───")
		for _, e := range result.After {
			fmt.Printf("  #%d [%s] %s — %s\n", e.ID, e.Type, e.Title, truncate(e.Content, 150))
		}
	}
}

func cmdContext(cfg store.Config) {
	args, err := parseCLIReadArguments(os.Args[2:], true)
	if err != nil {
		fatal(err)
		return
	}
	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
		return
	}
	defer s.Close()
	selected, err := resolveCLIReadScope(s, args.Project, args.All)
	if err != nil {
		fatal(err)
		return
	}
	fmt.Printf("Scope: %s\n", selected.Label)
	if !selected.Bound {
		fmt.Println("No previous session memories found.")
		return
	}
	var text string
	if args.Mode == store.ContextModeFull {
		text, err = storeFormatContext(s, selected.Project, args.Scope)
	} else {
		var response store.ContextResponse
		response, err = s.FormatContextBounded(store.ContextOptions{Project: selected.Project, Scope: args.Scope, Mode: args.Mode, TokenBudget: args.TokenBudget})
		text = response.Text
	}
	if err != nil {
		fatal(err)
		return
	}
	if text == "" {
		fmt.Println("No previous session memories found.")
		return
	}
	fmt.Print(text)
}

func cmdStats(cfg store.Config) {
	args, err := parseCLIReadArguments(os.Args[2:], false)
	if err != nil {
		fatal(err)
		return
	}
	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
		return
	}
	defer s.Close()
	selected, err := resolveCLIReadScope(s, args.Project, args.All)
	if err != nil {
		fatal(err)
		return
	}
	stats := &store.Stats{Projects: []string{}}
	if selected.All {
		stats, err = storeStats(s)
	} else if selected.Bound {
		stats, err = s.StatsForProject(selected.Project)
	}
	if err != nil {
		fatal(err)
		return
	}
	if args.JSON {
		b, e := jsonMarshalIndent(map[string]any{"scope": selected.Label, "project": selected.Project, "stats": stats, "projects_basis": "projects with non-deleted observations in the selected scope; includes archived observations"}, "", "  ")
		if e != nil {
			fatal(e)
			return
		}
		fmt.Println(string(b))
		return
	}
	projects := "none yet"
	if len(stats.Projects) > 0 {
		projects = strings.Join(stats.Projects, ", ")
	}
	fmt.Printf("RecallDock Memory Stats\n  Scope:        %s\n", selected.Label)
	fmt.Printf("  Sessions:     %d\n  Observations: %d\n  Prompts:      %d\n  Projects:     %s\n  Projects basis: non-deleted observations in this scope (includes archived); not registered identities\n  Database:     %s/recalldock.db\n", stats.TotalSessions, stats.TotalObservations, stats.TotalPrompts, projects, cfg.DataDir)
}

func cmdExport(cfg store.Config) {
	optionArgs, positionals, err := splitCLIPositionals(os.Args[2:], []string{"--project"})
	if err != nil {
		fatal(err)
		return
	}
	options, err := parseCLIOptions(optionArgs, []string{"--project"}, nil)
	if err != nil {
		fatal(err)
		return
	}
	if len(positionals) > 1 {
		fatal(errors.New("export accepts at most one output file"))
		return
	}
	outFile := "recalldock-export.json"
	if len(positionals) == 1 {
		outFile = positionals[0]
	}
	projectFilter := options.Values["--project"]

	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
	}
	defer s.Close()

	var data *store.ExportData
	if projectFilter != "" {
		data, err = s.ExportProject(projectFilter)
	} else {
		data, err = storeExport(s)
	}
	if err != nil {
		fatal(err)
	}

	out, err := jsonMarshalIndent(data, "", "  ")
	if err != nil {
		fatal(err)
	}

	if err := os.WriteFile(outFile, out, 0644); err != nil {
		fatal(err)
	}

	fmt.Printf("Exported to %s\n", outFile)
	fmt.Printf("  Sessions:     %d\n", len(data.Sessions))
	fmt.Printf("  Observations: %d\n", len(data.Observations))
	fmt.Printf("  Prompts:      %d\n", len(data.Prompts))
}

func cmdImport(cfg store.Config) {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: recalldock import <file.json>")
		exitFunc(1)
	}

	inFile := os.Args[2]
	raw, err := os.ReadFile(inFile)
	if err != nil {
		fatal(fmt.Errorf("read %s: %w", inFile, err))
	}

	var data store.ExportData
	if err := json.Unmarshal(raw, &data); err != nil {
		fatal(fmt.Errorf("parse %s: %w", inFile, err))
	}

	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
	}
	defer s.Close()

	result, err := s.Import(&data)
	if err != nil {
		fatal(err)
	}

	fmt.Printf("Imported from %s\n", inFile)
	fmt.Printf("  Sessions:     %d\n", result.SessionsImported)
	fmt.Printf("  Observations: %d\n", result.ObservationsImported)
	fmt.Printf("  Prompts:      %d\n", result.PromptsImported)
	if result.ObservationsSkipped > 0 || result.PromptsSkipped > 0 {
		fmt.Printf("  Skipped existing: %d observations, %d prompts\n", result.ObservationsSkipped, result.PromptsSkipped)
	}
	if result.SessionsImported+result.ObservationsImported+result.PromptsImported == 0 {
		fmt.Println("  No new rows: the same IDs were already present and left unchanged.")
	}
}

func cmdSync(cfg store.Config) {
	if len(os.Args) == 3 && (os.Args[2] == "--help" || os.Args[2] == "-h" || os.Args[2] == "help") {
		printSyncUsage()
		return
	}
	if len(os.Args) >= 3 && os.Args[2] == "cleanup" {
		cmdSyncCleanup(cfg)
		return
	}

	options, err := parseCLIOptions(os.Args[2:], []string{"--project"}, []string{"--import", "--status", "--all", "--cloud"})
	if err != nil {
		fatal(err)
		return
	}
	doImport, doStatus := options.Flags["--import"], options.Flags["--status"]
	doAll, doCloud := options.Flags["--all"], options.Flags["--cloud"]
	projectName := options.Values["--project"]
	projectProvided := projectName != ""
	if doAll && projectProvided {
		fatal(errors.New("--project and --all are mutually exclusive"))
		return
	}
	if doImport && doStatus {
		fatal(errors.New("--import and --status are mutually exclusive"))
		return
	}
	project := projectName

	syncDir := ".recalldock"

	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
	}
	defer s.Close()

	cloudEnabled := doCloud || envBool("RECALLDOCK_CLOUD_SYNC")
	if cloudEnabled {
		if doAll {
			fatal(errors.New("cloud sync requires a single explicit --project scope; --all is not supported"))
			return
		}
		if !projectProvided {
			fatal(errors.New("cloud sync requires an explicit non-empty --project value"))
			return
		}
	}
	selected, err := resolveCLIReadScope(s, project, doAll)
	if err != nil {
		fatal(err)
		return
	}
	if !selected.Bound {
		fatal(errors.New("sync requires a bound project, --project PROJECT or explicit --all; unbound cwd has no sync scope"))
		return
	}
	project = selected.Project
	if doImport && !doAll {
		if _, err := resolveCLIWriteOwner(s, project); err != nil {
			fatal(err)
			return
		}
	}
	cloudTargetKey := cloudTargetKeyForProject(project)
	var sy *engramsync.Syncer

	markCloudHealthy := func() {
		if !cloudEnabled {
			return
		}
		if err := s.MarkSyncHealthy(cloudTargetKey); err != nil {
			fatal(fmt.Errorf("cloud sync health update: %w", err))
		}
	}

	markCloudSyncOutcome := func() {
		if !cloudEnabled {
			return
		}
		hasPending, err := s.HasPendingSyncMutationsForProject(project)
		if err != nil {
			fatal(fmt.Errorf("cloud sync state update: %w", err))
		}
		pendingImports := 0
		remoteStatusVerified := false
		if _, _, pending, statusErr := syncStatus(sy); statusErr == nil {
			pendingImports = pending
			remoteStatusVerified = true
		}
		if hasPending || (remoteStatusVerified && pendingImports > 0) {
			if err := s.MarkSyncPending(cloudTargetKey); err != nil {
				fatal(fmt.Errorf("cloud sync pending-state update: %w", err))
			}
			return
		}
		if !remoteStatusVerified {
			return
		}
		markCloudHealthy()
	}

	sy = engramsync.NewLocal(s, syncDir)
	if cloudEnabled {
		cc, err := preflightCloudSync(s, cfg, project, !doStatus)
		if err != nil {
			fatal(err)
		}
		transport, err := remote.NewRemoteTransport(cc.ServerURL, cc.Token, project)
		if err != nil {
			if !doStatus {
				markCloudSyncFailure(s, cloudTargetKey, err)
			}
			fatal(errors.New(cloudSyncFailureMessage(project, err)))
		}
		sy = engramsync.NewCloudWithTransport(s, transport, project)
	}

	if doStatus {
		local, remote, pending, err := syncStatus(sy)
		if err != nil {
			fatal(err)
		}
		if cloudEnabled {
			fmt.Printf("Cloud sync status (project=%q):\n", project)
			fmt.Printf("  Local chunks:    %d\n", local)
			fmt.Printf("  Remote chunks:   %d\n", remote)
			fmt.Printf("  Pending import:  %d\n", pending)
			return
		}
		fmt.Printf("Sync status:\n")
		fmt.Printf("  Local chunks:    %d\n", local)
		fmt.Printf("  Remote chunks:   %d\n", remote)
		fmt.Printf("  Pending import:  %d\n", pending)
		return
	}

	if doImport {
		var result *engramsync.ImportResult
		var err error
		if doAll || cloudEnabled {
			result, err = syncImport(sy)
		} else {
			result, err = sy.ImportProject(selected.Project)
		}
		if err != nil {
			if cloudEnabled {
				markCloudSyncFailure(s, cloudTargetKey, err)
			}
			if cloudEnabled {
				fatal(errors.New(cloudSyncFailureMessage(project, err)))
			}
			fatal(err)
		}
		markCloudSyncOutcome()

		if result.ChunksImported == 0 {
			fmt.Println("No new chunks to import.")
			if result.ChunksSkipped > 0 {
				fmt.Printf("  (%d chunks already imported)\n", result.ChunksSkipped)
			}
			return
		}

		if cloudEnabled {
			fmt.Printf("Imported %d new remote chunk(s) for project %q\n", result.ChunksImported, project)
		} else {
			fmt.Printf("Imported %d new chunk(s) from .recalldock/\n", result.ChunksImported)
		}
		fmt.Printf("  Sessions:     %d\n", result.SessionsImported)
		fmt.Printf("  Observations: %d\n", result.ObservationsImported)
		fmt.Printf("  Prompts:      %d\n", result.PromptsImported)
		if result.ChunksSkipped > 0 {
			fmt.Printf("  Skipped:      %d (already imported)\n", result.ChunksSkipped)
		}
		return
	}

	// Export: DB → new chunk
	username := engramsync.GetUsername()
	if doAll {
		fmt.Println("Exporting ALL memories (all projects)...")
	} else {
		if cloudEnabled {
			fmt.Printf("Exporting memories for project %q to cloud...\n", project)
		} else {
			fmt.Printf("Exporting memories for project %q...\n", project)
		}
	}
	result, err := syncExport(sy, username, project)
	if err != nil {
		if cloudEnabled {
			markCloudSyncFailure(s, cloudTargetKey, err)
			fatal(errors.New(cloudSyncFailureMessage(project, err)))
		}
		fatal(err)
	}
	markCloudSyncOutcome()

	if result.IsEmpty {
		if doAll {
			fmt.Println("Nothing new to sync — all memories already exported.")
		} else {
			fmt.Printf("Nothing new to sync for project %q — all memories already exported.\n", project)
		}
		return
	}

	fmt.Printf("Created chunk %s\n", result.ChunkID)
	fmt.Printf("  Sessions:     %d\n", result.SessionsExported)
	fmt.Printf("  Observations: %d\n", result.ObservationsExported)
	fmt.Printf("  Prompts:      %d\n", result.PromptsExported)
	if result.MutationsExported > 0 {
		fmt.Printf("  Mutations:    %d\n", result.MutationsExported)
	}
	if cloudEnabled {
		fmt.Printf("Cloud sync complete for project %q.\n", project)
		return
	}
	fmt.Println()
	fmt.Println("Add to git:")
	fmt.Printf("  git add .recalldock/ && git commit -m \"sync recalldock memories\"\n")
}

func cmdSyncCleanup(cfg store.Config) {
	if len(os.Args) == 4 && (os.Args[3] == "--help" || os.Args[3] == "-h" || os.Args[3] == "help") {
		printSyncCleanupUsage()
		return
	}
	options, err := parseCLIOptions(os.Args[3:], nil, []string{"--non-enrolled", "--dry-run", "--apply", "--json", "--all"})
	if err != nil {
		fatal(err)
		return
	}
	nonEnrolled, apply := options.Flags["--non-enrolled"], options.Flags["--apply"]
	jsonOutput := options.Flags["--json"]
	if apply && options.Flags["--dry-run"] {
		fatal(errors.New("--apply and --dry-run are mutually exclusive"))
		return
	}
	if apply && !options.Flags["--all"] {
		fatal(errors.New("sync cleanup --apply requires --all; cleanup acknowledges non-enrolled projects store-wide"))
		return
	}
	if !nonEnrolled {
		fatal(errors.New("sync cleanup requires --non-enrolled"))
		return
	}

	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
		return
	}
	defer s.Close()

	projects, err := s.CountPendingNonEnrolledSyncMutations(store.DefaultSyncTargetKey)
	if err != nil {
		fatal(err)
		return
	}
	var totalCount, totalPayloadBytes, snapshotMaxSeq int64
	for _, project := range projects {
		totalCount += project.Count
		totalPayloadBytes += project.PayloadBytes
		if project.MaxSeq > snapshotMaxSeq {
			snapshotMaxSeq = project.MaxSeq
		}
	}

	result := map[string]any{
		"status":              "dry_run",
		"target_key":          store.DefaultSyncTargetKey,
		"projects":            projects,
		"total_count":         totalCount,
		"total_payload_bytes": totalPayloadBytes,
		"snapshot_max_seq":    snapshotMaxSeq,
		"acked_count":         int64(0),
	}
	if apply {
		backupPath, backupErr := s.BackupSQLite()
		if backupErr != nil {
			fatal(backupErr)
			return
		}
		ackedCount, ackErr := s.SkipAckNonEnrolledMutationsThrough(store.DefaultSyncTargetKey, snapshotMaxSeq)
		if ackErr != nil {
			fatal(ackErr)
			return
		}
		result["status"] = "applied"
		result["backup_path"] = backupPath
		result["acked_count"] = ackedCount
	}

	if jsonOutput {
		encoded, marshalErr := jsonMarshalIndent(result, "", "  ")
		if marshalErr != nil {
			fatal(marshalErr)
			return
		}
		fmt.Println(string(encoded))
		return
	}
	if !apply {
		fmt.Printf("Dry run: %d pending mutation(s), %d payload byte(s), across %d non-enrolled project(s).\n", totalCount, totalPayloadBytes, len(projects))
		fmt.Println("Review the result, then rerun with --apply to create an online backup and acknowledge this snapshot.")
		return
	}
	fmt.Printf("Acknowledged %d pending mutation(s) through sequence %d.\n", result["acked_count"], snapshotMaxSeq)
	fmt.Printf("Online backup: %s\n", result["backup_path"])
}

func printSyncCleanupUsage() {
	fmt.Println("usage: recalldock sync cleanup --non-enrolled [--dry-run | --apply --all] [--json]")
	fmt.Println("Dry-run is the default. Apply creates an online SQLite backup before acknowledging the inspected snapshot.")
}

func printSyncUsage() {
	fmt.Println("usage: recalldock sync [--import | --status] [--project PROJECT | --all] (local chunks only)")
	fmt.Println("       recalldock sync cleanup --non-enrolled [--dry-run | --apply --all] [--json]")
	fmt.Println("Local sync exports project-scoped chunks to .recalldock/ by default.")
	fmt.Println("Cloud transport is disabled in this local-only build.")
}

// storeAdapter wraps *store.Store to satisfy obsidian.StoreReader.
// The real store.Stats() returns (*store.Stats, error); the interface expects *store.Stats.
type storeAdapter struct{ s *store.Store }

func (a *storeAdapter) Export() (*store.ExportData, error) { return a.s.Export() }
func (a *storeAdapter) ExportProject(project string) (*store.ExportData, error) {
	return a.s.ExportProject(project)
}
func (a *storeAdapter) ExportRelationMutations(project string) ([]store.SyncMutation, error) {
	return a.s.ExportRelationMutations(project)
}
func (a *storeAdapter) Identity() (store.StoreIdentity, error) { return a.s.Identity() }

func (a *storeAdapter) Stats() *store.Stats {
	st, _ := a.s.Stats()
	return st
}

func cmdObsidianExport(cfg store.Config) {
	// Parse flags
	var (
		vault          string
		project        string
		limit          int
		since          string
		force          bool
		graphConfig    string
		watch          bool
		interval       string
		attachmentRoot string
	)

	seen := map[string]bool{}
	for i := 2; i < len(os.Args); i++ {
		key := os.Args[i]
		if seen[key] {
			fatal(fmt.Errorf("duplicate obsidian-export option %s", key))
			return
		}
		seen[key] = true
		switch os.Args[i] {
		case "--attachment-root":
			value, err := cliFlagValue(os.Args, &i, "--attachment-root", "", false)
			if err != nil {
				fatal(err)
				return
			}
			attachmentRoot = value
		case "--vault":
			if i+1 < len(os.Args) {
				vault = os.Args[i+1]
				i++
			}
		case "--project":
			value, err := cliFlagValue(os.Args, &i, "--project", "", false)
			if err != nil {
				fatal(err)
				return
			}
			project, _ = store.NormalizeProject(value)
			if strings.TrimSpace(project) == "" {
				fatal(errors.New("obsidian-export --project requires a nonempty selector"))
				return
			}
		case "--limit":
			value, err := cliFlagValue(os.Args, &i, "--limit", "", false)
			if err != nil {
				fatal(err)
				return
			}
			limit, err = strconv.Atoi(value)
			if err != nil || limit < 0 {
				fatal(errors.New("obsidian-export --limit requires a nonnegative integer"))
				return
			}
		case "--since":
			if i+1 < len(os.Args) {
				since = os.Args[i+1]
				i++
			}
		case "--force":
			force = true
		case "--graph-config":
			if i+1 < len(os.Args) {
				graphConfig = os.Args[i+1]
				i++
			}
		case "--watch":
			watch = true
		case "--interval":
			if i+1 < len(os.Args) {
				interval = os.Args[i+1]
				i++
			}
		default:
			fmt.Fprintf(os.Stderr, "recalldock: unknown flag: %s\n", os.Args[i])
			exitFunc(1)
		}
	}

	if vault == "" {
		fmt.Fprintln(os.Stderr, "error: flag --vault is required")
		exitFunc(1)
	}

	// Default --graph-config to "preserve"
	if graphConfig == "" {
		graphConfig = string(obsidian.GraphConfigPreserve)
	}

	graphMode, err := obsidian.ParseGraphConfigMode(graphConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: invalid --graph-config value: %s (accepted: preserve, force, skip)\n", graphConfig)
		exitFunc(1)
	}

	// Validate --interval requires --watch
	if interval != "" && !watch {
		fmt.Fprintln(os.Stderr, "error: --interval requires --watch")
		exitFunc(1)
	}

	// Parse and validate --interval (default 10m when --watch is set)
	var watchInterval time.Duration
	if watch {
		intervalStr := interval
		if intervalStr == "" {
			intervalStr = "10m"
		}
		d, parseErr := time.ParseDuration(intervalStr)
		if parseErr != nil {
			fmt.Fprintf(os.Stderr, "error: invalid --interval value %q: %v\n", intervalStr, parseErr)
			exitFunc(1)
		}
		if d < time.Minute {
			fmt.Fprintf(os.Stderr, "error: --interval must be at least 1m (minimum), got %v\n", d)
			exitFunc(1)
		}
		watchInterval = d
	}

	exportCfg := obsidian.ExportConfig{
		VaultPath:      vault,
		Project:        project,
		Limit:          limit,
		Force:          force,
		GraphConfig:    graphMode,
		AttachmentRoot: attachmentRoot,
	}
	if err := obsidian.ValidateAttachmentRoot(attachmentRoot); err != nil {
		fatal(err)
		return
	}

	if since != "" {
		// Try common date formats: full RFC3339, date-only (YYYY-MM-DD)
		var sinceTime time.Time
		var parseErr error
		for _, layout := range []string{time.RFC3339, "2006-01-02"} {
			sinceTime, parseErr = time.Parse(layout, since)
			if parseErr == nil {
				break
			}
		}
		if parseErr != nil {
			fmt.Fprintf(os.Stderr, "error: invalid --since value %q (expected YYYY-MM-DD or RFC3339)\n", since)
			exitFunc(1)
		}
		exportCfg.Since = sinceTime
	}

	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
	}
	defer s.Close()

	exp := newObsidianExporter(&storeAdapter{s: s}, exportCfg)

	if watch {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		w := newObsidianWatcher(obsidian.WatcherConfig{
			Exporter: exp,
			Interval: watchInterval,
			Logf:     log.Printf,
		})

		if w != nil {
			if runErr := w.Run(ctx); runErr != nil {
				log.Printf("[recalldock] shutting down watch mode: %v", runErr)
			} else {
				log.Printf("[recalldock] shutting down watch mode")
			}
		}
		exitFunc(0)
		return
	}

	result, err := exp.Export()
	if err != nil {
		fatal(err)
	}

	fmt.Printf("Obsidian export result\n")
	fmt.Printf("  Created: %d\n", result.Created)
	fmt.Printf("  Updated: %d\n", result.Updated)
	fmt.Printf("  Deleted: %d\n", result.Deleted)
	fmt.Printf("  Skipped: %d\n", result.Skipped)
	fmt.Printf("  Hubs:    %d\n", result.HubsCreated)
	if len(result.Errors) > 0 {
		fmt.Fprintf(os.Stderr, "  Errors: %d\n", len(result.Errors))
		for _, e := range result.Errors {
			fmt.Fprintf(os.Stderr, "    - %v\n", e)
		}
		exitFunc(1)
		return
	}
}

func cmdProjects(cfg store.Config) {
	// Route: recalldock projects current | list | consolidate | prune
	subCmd := "list"
	if len(os.Args) > 2 {
		subCmd = os.Args[2]
	}
	switch subCmd {
	case "bindings", "unbind":
		cmdProjectBindings(cfg)
	case "create", "adopt", "delete", "bind", "activate", "registry", "budget":
		cmdProjectRegistry(cfg)
	case "current":
		cmdCurrentHandshake(cfg)
	case "consolidate":
		cmdProjectsConsolidate(cfg)
	case "prune":
		cmdProjectsPrune(cfg)
	case "list", "":
		cmdProjectsList(cfg)
	default:
		fmt.Fprintf(os.Stderr, "unknown projects subcommand: %s\n", subCmd)
		fmt.Fprintln(os.Stderr, "usage: recalldock projects current [--directory PATH] [--json]")
		fmt.Fprintln(os.Stderr, "usage: recalldock projects list")
		fmt.Fprintln(os.Stderr, "       recalldock projects consolidate [--all] [--dry-run]")
		fmt.Fprintln(os.Stderr, "       recalldock projects consolidate --mapping FILE [--apply]")
		fmt.Fprintln(os.Stderr, "       recalldock projects prune [--dry-run]")
		exitFunc(1)
	}
}

func cmdStore(cfg store.Config) {
	subCmd := "current"
	if len(os.Args) > 2 {
		subCmd = os.Args[2]
	}
	if subCmd == "init" {
		for _, arg := range os.Args[3:] {
			if arg != "--json" {
				fatal(fmt.Errorf("unknown store init option %q", arg))
				return
			}
		}
		cfg.RequireExisting = false
		s, err := storeNew(cfg)
		if err != nil {
			fatal(err)
			return
		}
		defer s.Close()
		identity, err := s.Identity()
		if err != nil {
			fatal(err)
			return
		}
		if hasArg(os.Args[3:], "--json") {
			body, _ := json.Marshal(identity)
			fmt.Println(string(body))
		} else {
			fmt.Printf("Store initialized: %s\n", identity.DatabasePath)
		}
		return
	}
	if subCmd != "current" {
		fmt.Fprintf(os.Stderr, "unknown store subcommand: %s\n", subCmd)
		fmt.Fprintln(os.Stderr, "usage: recalldock store current [--directory PATH] [--json]")
		exitFunc(1)
		return
	}
	cmdCurrentHandshake(cfg)
}

type currentHandshake struct {
	ProjectID         string   `json:"project_id,omitempty"`
	ProjectName       string   `json:"project_name,omitempty"`
	BindingID         string   `json:"binding_id,omitempty"`
	StoreID           string   `json:"store_id"`
	DataDir           string   `json:"data_dir"`
	DatabasePath      string   `json:"database_path"`
	Project           string   `json:"project"`
	ProjectSource     string   `json:"project_source"`
	ProjectPath       string   `json:"project_path"`
	CWD               string   `json:"cwd"`
	AvailableProjects []string `json:"available_projects,omitempty"`
	ScopeState        string   `json:"scope_state"`
	Warning           string   `json:"warning,omitempty"`
	ErrorHint         string   `json:"error_hint,omitempty"`
}

func cmdCurrentHandshake(cfg store.Config) {
	directory := ""
	jsonOutput := false
	for i := 3; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--json":
			jsonOutput = true
		case "--directory":
			if i+1 >= len(os.Args) {
				fatal(errors.New("--directory requires a path"))
				return
			}
			directory = os.Args[i+1]
			i++
		default:
			fatal(fmt.Errorf("unknown current option: %s", os.Args[i]))
			return
		}
	}

	if directory == "" {
		var err error
		directory, err = os.Getwd()
		if err != nil {
			fatal(err)
			return
		}
	}
	absDirectory, err := filepath.Abs(directory)
	if err != nil {
		fatal(fmt.Errorf("resolve current directory: %w", err))
		return
	}

	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
		return
	}
	defer s.Close()
	identity, err := s.Identity()
	if err != nil {
		fatal(err)
		return
	}
	resolved := s.DetectProject(absDirectory)
	handshake := currentHandshake{
		ProjectID: resolved.ProjectID, ProjectName: resolved.ProjectName, BindingID: resolved.BindingID,
		StoreID:           identity.StoreID,
		DataDir:           identity.DataDir,
		DatabasePath:      identity.DatabasePath,
		Project:           resolved.Project,
		ProjectSource:     resolved.Source,
		ProjectPath:       resolved.Path,
		CWD:               absDirectory,
		AvailableProjects: resolved.AvailableProjects,
		ScopeState:        handshakeScopeState(resolved.Source),
		Warning:           resolved.Warning,
	}
	if resolved.Error != nil {
		handshake.ErrorHint = resolved.Error.Error()
	}

	if jsonOutput {
		out, err := jsonMarshalIndent(handshake, "", "  ")
		if err != nil {
			fatal(err)
			return
		}
		fmt.Println(string(out))
	} else {
		fmt.Printf("Store:   %s\n", handshake.StoreID)
		fmt.Printf("Database: %s\n", handshake.DatabasePath)
		fmt.Printf("Project:  %s (%s)\n", handshake.Project, handshake.ProjectSource)
		fmt.Printf("Path:     %s\n", handshake.ProjectPath)
		fmt.Printf("Scope:    %s\n", scopeStateText(handshake.ScopeState))
		if handshake.Warning != "" {
			fmt.Printf("Warning:  %s\n", handshake.Warning)
		}
	}

	if resolved.Error != nil {
		fmt.Fprintf(os.Stderr, "project resolution failed: %v\n", resolved.Error)
		exitFunc(2)
	}
}

// handshakeScopeState separates a writable project scope from a read-only
// detection hint: a git remote or repository name tells the operator where a
// directory sits, but a registered binding or declared config is what makes
// writes and model sessions legal.
func handshakeScopeState(source string) string {
	switch source {
	case "registered_binding":
		return "bound"
	case "worktree_main":
		return "bound"
	case "config":
		return "configured"
	case "git_root", "git_remote", "git_child":
		return "hint_only"
	default:
		return "unbound"
	}
}

func scopeStateText(state string) string {
	switch state {
	case "bound":
		return "bound (writes and model sessions are allowed for this project)"
	case "configured":
		return "configured (declared .recalldock/config.json; writes allowed for this directory)"
	case "hint_only":
		return "hint_only — detected from git only; bind the project or declare .recalldock/config.json before writing"
	default:
		return "unbound — reads return an empty scope; bind a root or declare config to use this directory"
	}
}

func cmdProjectsList(cfg store.Config) {
	args := os.Args[2:]
	if len(args) > 0 && args[0] == "list" {
		args = args[1:]
	}
	options, err := parseCLIOptions(args, nil, []string{"--json"})
	if err != nil {
		fatal(err)
		return
	}
	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
		return
	}
	defer s.Close()

	projects, err := s.ListProjectsWithStats()
	if err != nil {
		fatal(err)
		return
	}
	registered, err := s.ListRegisteredProjects()
	if err != nil {
		fatal(err)
		return
	}
	names := map[string]string{}
	for _, p := range registered {
		names[p.ID] = p.Name
	}
	type projectItem struct {
		ID string `json:"id"`
		store.ProjectStats
	}
	items := make([]projectItem, 0, len(projects))
	withObservations := 0
	for _, p := range projects {
		key := p.Name
		if name, ok := names[key]; ok {
			p.Name = name
		}
		items = append(items, projectItem{ID: key, ProjectStats: p})
		if p.ObservationCount > 0 {
			withObservations++
		}
	}
	const basis = "projects with sessions, observations or prompts; excludes deleted observations"
	if options.Flags["--json"] {
		body, err := jsonMarshalIndent(map[string]any{"projects": items, "count": len(items), "count_basis": basis, "registered_count": len(registered), "with_observations_count": withObservations}, "", "  ")
		if err != nil {
			fatal(err)
			return
		}
		fmt.Println(string(body))
		return
	}
	fmt.Printf("Projects (%d) — %s; %d registered, %d with observations\n", len(items), basis, len(registered), withObservations)
	if len(items) == 0 {
		fmt.Println("No projects found.")
		return
	}
	for _, item := range items {
		p := item.ProjectStats
		name := p.Name
		if item.ID != name {
			name += " (" + item.ID + ")"
		}
		sessionWord := "sessions"
		if p.SessionCount == 1 {
			sessionWord = "session"
		}
		promptWord := "prompts"
		if p.PromptCount == 1 {
			promptWord = "prompt"
		}
		fmt.Printf("  %-30s %4d obs   %3d %-9s  %3d %s\n", name, p.ObservationCount, p.SessionCount, sessionWord, p.PromptCount, promptWord)
	}
}

// projectGroup represents a set of project names that should be merged.
type projectGroup struct {
	Names     []string
	Canonical string // suggested canonical (most observations)
}

// groupSimilarProjects groups projects by name similarity and shared directories.
// Uses a simple union-find approach.
func groupSimilarProjects(projects []store.ProjectStats, displayByKey map[string]string, registered map[string]bool) []projectGroup {
	n := len(projects)
	if n == 0 {
		return nil
	}
	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(x int) int {
		if parent[x] != x {
			parent[x] = find(parent[x])
		}
		return parent[x]
	}
	union := func(x, y int) {
		rx, ry := find(x), find(y)
		if rx != ry {
			parent[rx] = ry
		}
	}

	// Similarity uses display names, never opaque IDs. An ID like prj-929… and
	// prj-ceb… must not become a merge group just because both start with prj-.
	display := func(key string) string {
		if name := displayByKey[key]; name != "" {
			return name
		}
		return key
	}
	names := make([]string, n)
	nameToIndices := make(map[string][]int, n)
	for i, p := range projects {
		names[i] = display(p.Name)
		nameToIndices[names[i]] = append(nameToIndices[names[i]], i)
	}
	for i := 0; i < n; i++ {
		for _, sm := range project.FindSimilar(names[i], names, 3) {
			for _, j := range nameToIndices[sm.Name] {
				union(i, j)
			}
		}
	}

	// Shared historical session directories are not current ownership. Skip
	// pairing two registered identities by an old checkout path.
	dirToProjects := make(map[string][]int)
	for i, p := range projects {
		for _, dir := range p.Directories {
			if dir != "" {
				dirToProjects[dir] = append(dirToProjects[dir], i)
			}
		}
	}
	for _, idxs := range dirToProjects {
		for k := 1; k < len(idxs); k++ {
			if registered[projects[idxs[0]].Name] && registered[projects[idxs[k]].Name] {
				continue
			}
			union(idxs[0], idxs[k])
		}
	}

	components := make(map[int][]int)
	for i := 0; i < n; i++ {
		root := find(i)
		components[root] = append(components[root], i)
	}
	var groups []projectGroup
	for _, idxs := range components {
		if len(idxs) < 2 {
			continue
		}
		bestIdx := idxs[0]
		for _, idx := range idxs[1:] {
			if projects[idx].ObservationCount > projects[bestIdx].ObservationCount {
				bestIdx = idx
			}
		}
		grpNames := make([]string, len(idxs))
		for k, idx := range idxs {
			grpNames[k] = projects[idx].Name
		}
		sort.Strings(grpNames)
		groups = append(groups, projectGroup{Names: grpNames, Canonical: projects[bestIdx].Name})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Canonical < groups[j].Canonical })
	return groups
}

type projectMappingManifest struct {
	Version  int                    `json:"version"`
	StoreID  string                 `json:"store_id"`
	Mappings []store.ProjectMapping `json:"mappings"`
}

func cmdProjectsConsolidate(cfg store.Config) {
	mappingPath := ""
	apply := false
	doAll := false
	for i := 3; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--mapping":
			if i+1 >= len(os.Args) {
				fatal(errors.New("--mapping requires a JSON file path"))
				return
			}
			mappingPath = os.Args[i+1]
			i++
		case "--apply":
			apply = true
		case "--all":
			doAll = true
		case "--dry-run":
			// Retained as a compatibility spelling; dry-run is now the default.
		default:
			fatal(fmt.Errorf("unknown consolidate option: %s", os.Args[i]))
			return
		}
	}

	if mappingPath == "" {
		if apply {
			fatal(errors.New("project consolidation requires --mapping FILE; inferred candidates can only be previewed"))
			return
		}
		originalArgs := os.Args
		if !stringSliceContains(os.Args[3:], "--dry-run") {
			os.Args = append(append([]string(nil), os.Args...), "--dry-run")
		}
		defer func() { os.Args = originalArgs }()
		fmt.Println("[dry-run] Inferred candidates are advisory only; apply changes with a reviewed --mapping FILE bound to store_id.")
		cmdProjectsConsolidateDiscovery(cfg)
		return
	}
	if doAll {
		fatal(errors.New("--all cannot be combined with --mapping; mappings must enumerate every approved source and target"))
		return
	}

	raw, err := os.ReadFile(mappingPath)
	if err != nil {
		fatal(fmt.Errorf("read project mapping %s: %w", mappingPath, err))
		return
	}
	var manifest projectMappingManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		fatal(fmt.Errorf("parse project mapping %s: %w", mappingPath, err))
		return
	}
	if manifest.Version != 1 {
		fatal(fmt.Errorf("project mapping version must be 1, got %d", manifest.Version))
		return
	}

	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
		return
	}
	defer s.Close()
	identity, err := s.Identity()
	if err != nil {
		fatal(err)
		return
	}
	if strings.TrimSpace(manifest.StoreID) == "" || manifest.StoreID != identity.StoreID {
		fatal(&store.StoreIdentityMismatchError{
			Expected: strings.TrimSpace(manifest.StoreID),
			Actual:   identity.StoreID,
			Path:     identity.DatabasePath,
		})
		return
	}
	if len(manifest.Mappings) == 0 {
		fatal(fmt.Errorf("%w: mappings must not be empty", store.ErrInvalidProjectMapping))
		return
	}
	normalizedMappings, err := s.ValidateProjectMappings(manifest.StoreID, manifest.Mappings)
	if err != nil {
		fatal(err)
		return
	}

	if !apply {
		fmt.Printf("[dry-run] Reviewed mapping manifest targets store %s:\n", identity.StoreID)
		for _, mapping := range normalizedMappings {
			fmt.Printf("  %q -> %q\n", mapping.Source, mapping.Target)
		}
		fmt.Println("No changes applied. Re-run with --apply after reviewing the exact mapping and backup.")
		return
	}

	result, err := s.ApplyProjectMappings(manifest.StoreID, normalizedMappings)
	if err != nil {
		fatal(err)
		return
	}
	fmt.Printf("Applied %d explicit project mapping(s) to store %s.\n", result.MappingsApplied, identity.StoreID)
	fmt.Printf("  Observations: %d\n", result.ObservationsUpdated)
	fmt.Printf("  Sessions:     %d\n", result.SessionsUpdated)
	fmt.Printf("  Prompts:      %d\n", result.PromptsUpdated)
	fmt.Printf("  Sync records: %d\n", result.SyncRecordsUpdated)
	fmt.Printf("  Keys retired: %d (stale writes rejected; no automatic redirect)\n", result.KeysRetired)
}

func stringSliceContains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func cmdProjectsConsolidateDiscovery(cfg store.Config) {
	doAll := false
	for i := 3; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--all":
			doAll = true
		}
	}

	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
	}
	defer s.Close()

	if !doAll {
		// Single-project mode: detect canonical project for cwd, find variants
		cwd, err := os.Getwd()
		if err != nil {
			fatal(err)
		}
		detected := s.DetectExplicitProject(cwd)
		if detected.Error != nil {
			fatal(detected.Error)
			return
		}
		canonical := strings.TrimSpace(detected.Project)
		if canonical == "" {
			// Legacy name/git detection is only a dry-run hint, and only when
			// it names a project that actually exists. "unknown" or an
			// unrelated cwd must fail instead of becoming a merge candidate.
			hint := strings.TrimSpace(detectProject(cwd))
			if hint != "" && hint != "unknown" {
				exists, err := s.ProjectExists(hint)
				if err != nil {
					fatal(err)
					return
				}
				if exists {
					canonical = hint
				}
			}
		}
		if canonical == "" {
			fatal(errors.New("current directory is not bound to a project; use --all for a read-only candidate scan, or run inside a bound project"))
			return
		}

		allNames, err := s.ListProjectNames()
		if err != nil {
			fatal(err)
		}
		// Similarity must run on display names, never on opaque prj-* IDs:
		// unrelated IDs share a prefix and otherwise become false merge groups.
		displayByKey := map[string]string{}
		if registered, regErr := s.ListRegisteredProjects(); regErr == nil {
			for _, p := range registered {
				displayByKey[p.ID] = p.Name
			}
		}
		displayNames := make([]string, 0, len(allNames))
		seenDisplay := map[string]bool{}
		for _, key := range allNames {
			name := displayByKey[key]
			if name == "" {
				name = key
			}
			if !seenDisplay[name] {
				seenDisplay[name] = true
				displayNames = append(displayNames, name)
			}
		}
		canonicalDisplay := displayByKey[canonical]
		if canonicalDisplay == "" {
			canonicalDisplay = canonical
		}

		// Check if the detected canonical actually exists in the DB.
		canonicalExists := false
		for _, n := range allNames {
			if n == canonical {
				canonicalExists = true
				break
			}
		}
		if !canonicalExists {
			fmt.Printf("Note: %q has no existing memories. Merging will move memories into this new project name.\n", canonical)
		}

		// Find candidates by display-name similarity, then map back to keys.
		displayMatches := project.FindSimilar(canonicalDisplay, displayNames, 3)
		similar := make([]project.ProjectMatch, 0, len(displayMatches))
		for _, dm := range displayMatches {
			for key, name := range displayByKey {
				if name == dm.Name {
					dm.Name = key
					similar = append(similar, dm)
					break
				}
			}
			if dm.Name != canonical {
				// Raw/name-keyed projects have no display entry; FindSimilar
				// already returned the key itself, so keep it.
				found := false
				for _, existing := range similar {
					if existing.Name == dm.Name {
						found = true
						break
					}
				}
				if !found {
					similar = append(similar, dm)
				}
			}
		}

		// Also find candidates by shared directory (catches renames like sdd-agent-team → agent-teams-lite)
		allStats, _ := s.ListProjectsWithStats()
		statsMap := make(map[string]store.ProjectStats)
		var cwdDirs []string // directories for the canonical project
		for _, ps := range allStats {
			statsMap[ps.Name] = ps
			if ps.Name == canonical {
				cwdDirs = ps.Directories
			}
		}
		// If canonical has no stats yet, use cwd as its directory
		if len(cwdDirs) == 0 {
			cwdDirs = []string{cwd}
		}
		// Find projects sharing a directory with the canonical
		similarNames := make(map[string]bool)
		for _, sm := range similar {
			similarNames[sm.Name] = true
		}
		for _, ps := range allStats {
			if ps.Name == canonical || similarNames[ps.Name] {
				continue
			}
			// Historical session directories are not current ownership. Two
			// distinct registered IDs must be merged only by an explicit
			// mapping, never because an old session reused a checkout path.
			if strings.HasPrefix(canonical, "prj-") && strings.HasPrefix(ps.Name, "prj-") {
				continue
			}
			for _, d := range ps.Directories {
				for _, cd := range cwdDirs {
					if d == cd {
						similar = append(similar, project.ProjectMatch{
							Name:      ps.Name,
							MatchType: "shared directory",
						})
						similarNames[ps.Name] = true
					}
				}
			}
		}

		if len(similar) == 0 {
			fmt.Printf("No similar project names found for %q. Nothing to consolidate.\n", canonical)
			return
		}

		fmt.Printf("Detected project: %q\n\n", canonical)
		fmt.Printf("Found similar project names:\n")
		for i, sm := range similar {
			obs := 0
			if ps, ok := statsMap[sm.Name]; ok {
				obs = ps.ObservationCount
			}
			fmt.Printf("  [%d] %-30s %3d obs  (%s)\n", i+1, sm.Name, obs, sm.MatchType)
		}

		fmt.Printf("\n[dry-run] Candidate set contains %d project(s) for %q\n", len(similar), canonical)
		return
	}

	// --all mode: group all projects by similarity + shared directories
	projects, err := s.ListProjectsWithStats()
	if err != nil {
		fatal(err)
	}
	displayByKey := map[string]string{}
	registeredKeys := map[string]bool{}
	if registered, regErr := s.ListRegisteredProjects(); regErr == nil {
		for _, p := range registered {
			displayByKey[p.ID] = p.Name
			registeredKeys[p.ID] = true
		}
	}
	groups := groupSimilarProjects(projects, displayByKey, registeredKeys)

	if len(groups) == 0 {
		fmt.Println("No similar project name groups found.")
		return
	}

	fmt.Printf("Found %d group(s) of similar project names:\n\n", len(groups))

	// Build stats map for obs counts
	projectStatsMap := make(map[string]store.ProjectStats)
	for _, p := range projects {
		projectStatsMap[p.Name] = p
	}

	for i, g := range groups {
		fmt.Printf("Group %d:\n", i+1)
		for j, name := range g.Names {
			obs := 0
			if ps, ok := projectStatsMap[name]; ok {
				obs = ps.ObservationCount
			}
			marker := "  "
			if name == g.Canonical {
				marker = "→ "
			}
			label := name
			if display := displayByKey[name]; display != "" && display != name {
				label = display + " (" + name + ")"
			}
			fmt.Printf("  %s[%d] %-30s %3d obs\n", marker, j+1, label, obs)
		}
		fmt.Printf("  Suggested canonical: %q (→)\n", g.Canonical)

		fmt.Printf("  [dry-run] Candidate canonical: %q\n\n", g.Canonical)
	}
}

func cmdProjectsPrune(cfg store.Config) {
	options, err := parseCLIOptions(os.Args[3:], []string{"--project"}, []string{"--apply", "--dry-run", "--all"})
	if err != nil {
		fatal(err)
		return
	}
	projectName := options.Values["--project"]
	apply, dryRun := options.Flags["--apply"], options.Flags["--dry-run"]
	if apply && dryRun {
		fatal(errors.New("--apply and --dry-run are mutually exclusive"))
		return
	}
	if projectName != "" {
		if options.Flags["--all"] {
			fatal(errors.New("--project and --all are mutually exclusive"))
			return
		}
		s, err := storeNew(cfg)
		if err != nil {
			fatal(err)
			return
		}
		defer s.Close()
		if err = s.PruneMissingRegisteredProject(projectName, apply); err != nil {
			fatal(err)
			return
		}
		fmt.Printf("Missing empty registered project %s verified; applied=%t\n", projectName, apply)
		return
	}
	if apply || !options.Flags["--all"] {
		fatal(errors.New("interactive projects prune requires --all [--dry-run]; --apply requires --project ID"))
		return
	}

	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
	}
	defer s.Close()

	allStats, err := s.ListProjectsWithStats()
	if err != nil {
		fatal(err)
	}

	// Find projects with 0 observations
	var candidates []store.ProjectStats
	for _, ps := range allStats {
		if ps.ObservationCount == 0 {
			candidates = append(candidates, ps)
		}
	}

	// Registered identities (id or display name) are never prunable from the
	// interactive name-key path: their data may live under several keys and only
	// the missing-root verified `--project ID` path is safe for them.
	registeredKeys := map[string]string{}
	if identities, idErr := s.ListRegisteredProjects(); idErr == nil {
		for _, p := range identities {
			registeredKeys[p.ID] = p.ID
			registeredKeys[p.Name] = p.ID
		}
	}
	var protected, selectable []store.ProjectStats
	for _, ps := range candidates {
		if _, hit := registeredKeys[ps.Name]; hit {
			protected = append(protected, ps)
			continue
		}
		selectable = append(selectable, ps)
	}

	if len(candidates) == 0 {
		fmt.Println("No empty projects to prune.")
		return
	}

	fmt.Printf("Found %d project(s) with 0 observations:\n\n", len(candidates))
	for _, ps := range protected {
		fmt.Printf("  [protected] %-30s %3d sessions  %3d prompts  (registered identity; use projects prune --project <ID> --apply)\n", ps.Name, ps.SessionCount, ps.PromptCount)
	}
	for i, ps := range selectable {
		fmt.Printf("  [%d] %-30s %3d sessions  %3d prompts\n", i+1, ps.Name, ps.SessionCount, ps.PromptCount)
	}
	if len(selectable) == 0 {
		fmt.Println("\nNothing selectable: every empty project is a registered identity.")
		return
	}

	if dryRun {
		fmt.Printf("\n[dry-run] Would prune %d project(s) (%d protected as registered identities)\n", len(selectable), len(protected))
		return
	}

	fmt.Printf("\nSelect which to prune (comma-separated numbers, 'all', or 'none'): ")
	var answer string
	scanInputLine(&answer)
	answer = strings.TrimSpace(strings.ToLower(answer))

	if answer == "none" || answer == "n" || answer == "" {
		fmt.Println("Cancelled.")
		return
	}

	var selected []store.ProjectStats
	if answer == "all" || answer == "a" {
		selected = selectable
	} else {
		for _, part := range strings.Split(answer, ",") {
			part = strings.TrimSpace(part)
			idx := 0
			if _, err := fmt.Sscanf(part, "%d", &idx); err != nil || idx < 1 || idx > len(selectable) {
				fmt.Fprintf(os.Stderr, "Invalid selection: %q (expected 1-%d)\n", part, len(selectable))
				return
			}
			selected = append(selected, selectable[idx-1])
		}
	}

	if len(selected) == 0 {
		fmt.Println("Nothing selected.")
		return
	}

	totalSessions := int64(0)
	totalPrompts := int64(0)
	for _, ps := range selected {
		result, err := s.PruneProject(ps.Name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error pruning %q: %v\n", ps.Name, err)
			continue
		}
		totalSessions += result.SessionsDeleted
		totalPrompts += result.PromptsDeleted
	}

	fmt.Printf("\nPruned %d project(s): %d sessions, %d prompts removed.\n", len(selected), totalSessions, totalPrompts)
}

// cmdSetup classifies os.Args[2:] with a two-pass, order-independent
// algorithm (see openspec/changes/setup-protocol-flag/proposal.md,
// Approach; JD-014 residual fix). The FIRST pass scans every token and only
// accumulates classification state — it never dispatches mid-loop. This
// guarantees a token like --protocol=<v> is always parsed regardless of
// what precedes it (e.g. an earlier unrecognized hyphen-prefixed token no
// longer short-circuits the loop before later tokens are read). The SECOND
// pass dispatches once, in a fixed priority order, using the fully
// accumulated state: helpSeen > extraBareSeen > unknownFlagSeen > slug
// present > protocol-only > no args.
func cmdSetup(cfg store.Config) {
	args := os.Args[2:]

	var (
		helpSeen        bool
		protocolRaw     string
		protocolFlag    bool
		slug            string
		slugSeen        bool
		extraBareSeen   bool
		unknownFlagSeen bool
	)

	for i := 0; i < len(args); i++ {
		token := args[i]
		switch {
		case token == "--help" || token == "-h" || token == "help":
			helpSeen = true
		case token == "--protocol":
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				protocolRaw = args[i+1]
				i++
			} else {
				// Dangling --protocol: either it's the last token, or the
				// next token is itself a flag (e.g. `--protocol --help`).
				// Do NOT consume the next token as the value — leave it to
				// be classified normally on the next iteration so
				// `--protocol --help` still shows usage.
				protocolRaw = ""
			}
			protocolFlag = true
		case strings.HasPrefix(token, "--protocol="):
			protocolRaw = strings.TrimPrefix(token, "--protocol=")
			protocolFlag = true
		case strings.HasPrefix(token, "-"):
			// Unrecognized hyphen-prefixed token: record it but keep
			// scanning so a --protocol appearing later is still parsed
			// (JD-014 residual).
			unknownFlagSeen = true
		default:
			if slugSeen {
				extraBareSeen = true
			} else {
				slug = token
				slugSeen = true
			}
		}
	}

	switch {
	case helpSeen:
		printSetupUsage()
		return
	case extraBareSeen:
		fmt.Fprintln(os.Stderr, "usage: recalldock setup [<agent>] [--protocol=slim|full]")
		exitFunc(1)
		return
	case unknownFlagSeen:
		// Preserve the legacy fallback to the interactive menu (keeps
		// TestCmdSetupHyphenArgFallsBackToInteractive green), but forward
		// the already-parsed --protocol mode (if any) instead of dropping
		// it (JD-014), regardless of the unknown flag's position.
		mode := ""
		if protocolFlag {
			mode = resolveProtocolModeFlag(protocolRaw)
		}
		cmdSetupInteractive(cfg, mode)
		return
	case slugSeen:
		result, err := setupInstallAgent(slug)
		if err != nil {
			fatal(err)
		}
		if protocolFlag {
			applyProtocolMode(cfg, slug, resolveProtocolModeFlag(protocolRaw))
		}
		fmt.Printf("✓ Installed %s plugin (%d files)\n", result.Agent, result.Files)
		fmt.Printf("  → %s\n", result.Destination)
		printPostInstall(result)
	default:
		// No slug: interactive menu. Mode (if any) applies to whichever
		// slug the user selects.
		mode := ""
		if protocolFlag {
			mode = resolveProtocolModeFlag(protocolRaw)
		}
		cmdSetupInteractive(cfg, mode)
	}
}

// cmdSetupInteractive renders the agent picker and installs the chosen
// agent. mode is the already-resolved --protocol value ("slim"/"full") from
// a slug-less invocation, or "" when --protocol was not given at all.
func cmdSetupInteractive(cfg store.Config, mode string) {
	agents := setupSupportedAgents()

	fmt.Println("recalldock setup — Install agent plugin")
	fmt.Println()
	fmt.Println("Which agent do you want to set up?")
	fmt.Println()

	for i, a := range agents {
		fmt.Printf("  [%d] %s\n", i+1, a.Description)
		fmt.Printf("      Install to: %s\n\n", a.InstallDir)
	}

	fmt.Print("Enter choice (1-", len(agents), "): ")
	var input string
	scanInputLine(&input)

	choice, err := strconv.Atoi(strings.TrimSpace(input))
	if err != nil || choice < 1 || choice > len(agents) {
		fmt.Fprintln(os.Stderr, "Invalid choice.")
		exitFunc(1)
	}

	selected := agents[choice-1]
	fmt.Printf("\nInstalling %s plugin...\n", selected.Name)

	result, err := setupInstallAgent(selected.Name)
	if err != nil {
		fatal(err)
	}
	if mode != "" {
		applyProtocolMode(cfg, selected.Name, mode)
	}

	fmt.Printf("✓ Installed %s plugin (%d files)\n", result.Agent, result.Files)
	fmt.Printf("  → %s\n", result.Destination)
	printPostInstall(result)
}

// printSetupUsage prints `recalldock setup --help` output. Its Flags section
// MUST contain the literal "--protocol" (Guarantee 1); it must never read
// stdin (Guarantee 2 — safe under a detached/non-TTY stdin).
func printSetupUsage() {
	fmt.Println("usage: recalldock setup [<agent>] [--protocol=slim|full]")
	fmt.Println()
	fmt.Println("Install an agent plugin (claude-code, opencode, codex, ...).")
	fmt.Println("Without <agent>, shows an interactive menu.")
	fmt.Println()
	fmt.Println("Flags:")
	fmt.Println("  --protocol=<slim|full>  Set the session-start protocol verbosity for the")
	fmt.Println("                          installed agent slug (default: full). Unknown or")
	fmt.Println("                          missing values fall back to full with a warning.")
	fmt.Println("                          slim currently only takes effect for claude-code,")
	fmt.Println("                          and only when the installed recalldock is >= 1.4.0.")
	fmt.Println("  --help, -h              Show this help and exit.")
}

// resolveProtocolModeFlag normalizes a --protocol value to "slim" or "full".
// Unknown or empty values fall back to "full" with a non-fatal stderr
// warning — an invalid --protocol value never fails `recalldock setup`.
func resolveProtocolModeFlag(raw string) string {
	v := strings.ToLower(strings.TrimSpace(raw))
	switch v {
	case setup.ProtocolModeSlim:
		return setup.ProtocolModeSlim
	case setup.ProtocolModeFull:
		return setup.ProtocolModeFull
	default:
		fmt.Fprintf(os.Stderr, "warning: unknown --protocol value %q, defaulting to full\n", raw)
		return setup.ProtocolModeFull
	}
}

// applyProtocolMode persists the resolved protocol mode for slug, using the
// SAME cfg.DataDir main() resolved (RECALLDOCK_DATA_DIR override included) so the
// `protocol-mode` subcommand's read path matches this write path (JD-005). A
// write failure is reported as a non-fatal warning — it never fails setup.
func applyProtocolMode(cfg store.Config, slug, mode string) {
	if err := setup.WriteProtocolMode(cfg.DataDir, slug, mode); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not persist protocol mode: %v\n", err)
	}
}

// cmdProtocolMode implements `recalldock protocol-mode <slug>`: prints "slim" to
// stdout ONLY when the persisted mode for slug is "slim" AND the running
// binary's version meets the slim floor (>= 1.4.0); any other case
// (unrecognized slug, missing/corrupted mode file, version below floor,
// unparseable version) prints "full". All branching lives here in Go so it
// runs under `go test` — the Claude Code hook scripts only read this single
// line of stdout.
func cmdProtocolMode(cfg store.Config) {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: recalldock protocol-mode <slug>")
		exitFunc(1)
		return
	}
	slug := os.Args[2]

	mode := setup.ReadProtocolMode(cfg.DataDir, slug)
	if mode == setup.ProtocolModeSlim && meetsProtocolVersionFloor(version) {
		fmt.Println(setup.ProtocolModeSlim)
		return
	}
	fmt.Println(setup.ProtocolModeFull)
}

// protocolVersionFloor is the minimum recalldock version required to honor a
// persisted "slim" protocol-mode: the slim status block relies on the
// MCP serverInstructions duplication fix shipped in this release.
var protocolVersionFloor = [3]int{1, 4, 0}

// meetsProtocolVersionFloor reports whether v (e.g. "1.4.0", "v1.5.2", or the
// build-time "dev" placeholder) is >= protocolVersionFloor. Any unparseable
// or empty value returns false — the caller then falls back to "full".
func meetsProtocolVersionFloor(v string) bool {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if v == "" || v == "dev" {
		return false
	}

	segments := strings.SplitN(v, ".", 3)
	var parts [3]int
	for i, s := range segments {
		if i >= 3 {
			break
		}
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			return false
		}
		parts[i] = n
	}

	for i := 0; i < 3; i++ {
		if parts[i] != protocolVersionFloor[i] {
			return parts[i] > protocolVersionFloor[i]
		}
	}
	return true
}

func printPostInstall(result *setup.Result) {
	switch result.Agent {
	case "opencode":
		fmt.Println("\nNext steps:")
		fmt.Println("  1. Restart OpenCode — plugin + MCP server are ready")
		fmt.Println("  2. The plugin auto-starts the Engram HTTP server when needed")
		if result.TUIPluginEnabled {
			fmt.Println("\nAlso enabled: opencode-subagent-statusline in tui.json — sub-agent activity in the sidebar/footer.")
		}
	case "pi":
		fmt.Println("\nNext steps:")
		fmt.Println("  1. Restart Pi so packages and MCP config are reloaded")
		fmt.Println("  2. Verify with: pi list")
	case "claude-code":
		// Offer to add recalldock tools to the permissions allowlist
		fmt.Print("\nAdd recalldock tools to ~/.claude/settings.json allowlist?\n")
		fmt.Print("This prevents Claude Code from asking permission on every tool call.\n")
		fmt.Print("Add to allowlist? (y/N): ")
		var answer string
		scanInputLine(&answer)
		answer = strings.TrimSpace(strings.ToLower(answer))
		if answer == "y" || answer == "yes" {
			if err := setupAddClaudeCodeAllowlist(); err != nil {
				fmt.Fprintf(os.Stderr, "  warning: could not update allowlist: %v\n", err)
				fmt.Fprintln(os.Stderr, "  You can add them manually to permissions.allow in ~/.claude/settings.json")
			} else {
				fmt.Println("  ✓ RecallDock tools added to allowlist")
			}
		} else {
			fmt.Println("  Skipped. You can add them later to permissions.allow in ~/.claude/settings.json")
		}

		fmt.Println("\nNext steps:")
		fmt.Println("  1. Restart Claude Code — the plugin is active immediately")
		fmt.Println("  2. Verify with: claude plugin list")
		fmt.Println("  3. MCP config written to ~/.claude/mcp/engram.json using absolute binary path")
		fmt.Println("     (survives plugin auto-updates; re-run 'recalldock setup claude-code' if you move the binary)")
	default:
		// Every other agent's "next steps" are declared as data in the registry,
		// so the message is rendered generically instead of one case per agent.
		printNextSteps(setup.PostInstallSteps(result.Agent))
	}
}

// printNextSteps renders a numbered "Next steps" list, or nothing when empty.
func printNextSteps(steps []string) {
	if len(steps) == 0 {
		return
	}
	fmt.Println("\nNext steps:")
	for i, step := range steps {
		fmt.Printf("  %d. %s\n", i+1, step)
	}
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func printUsage() {
	fmt.Printf(`recalldock v%s — Local-only selective memory

Usage: recalldock <command> [arguments]

Commands:
  serve [port]       Local HTTP on 127.0.0.1 (default 7437), no cloud autosync
  mcp [--tools=agent|admin|all|TOOL[,PROFILE|TOOL...]] [--project PROJECT]
                     Stdio MCP; agent default; admin deferred read tools; profiles never grant writes
  tui                Interactive terminal only; use CLI in scripts
  ui [--port 7438] [--project P] [--open]
                     Loopback operator web UI; one-time token from this terminal
  search <query>     Current explicit project; --project PROJECT or --all for history
  context [project]  Default bounded index; --mode index|l0|l1|full; --token-budget N
  stats              Current explicit project; --project PROJECT | --all; --json
  save <title> <msg>  Local operator write; not exposed as agent write permission
  delete <id>        Soft-delete observation; --hard explicitly permanent
  delete session <id> Refuses all observations including soft-deleted; no cascade
  delete prompt <id> Permanent operator action
  delete project <name> [--hard --confirm] Matching trusted project cascade; back up first
  timeline <id>      Chronological selected observation context
  memory             Operator show/history/version/update with snapshot-checked edits
  candidates         Operator list/show/approve/reject with snapshot digest
  work               Independent operations.db; absolute --root + exact --lane
  gateway            7-tool on-demand MCP gateway (up to 200 backends); list/scan/add/discover/remove
  projects           create/bind/activate/budget/registry/current/list/consolidate/prune
  store current      Store/project handshake; --directory PATH; --json
  review             Lifecycle/dependency/resource staleness inspection
  conflicts          Local conflict inspection/lexical scan; external --semantic disabled
  doctor             Read-only diagnostics; --json; --project PROJECT; --check CODE
  backup [--json]    Verified private online SQLite snapshot (WAL-safe; no restore)
  export [file]      Operator JSON interchange (default recalldock-export.json)
  import <file>      Operator JSON import; never automatic discovery/migration
  sync               Local .recalldock chunks only; --import/--status/--project/--all
  obsidian-export    SQLite to protected Markdown projection; --vault PATH
  candidate-hook     Bounded proposal outbox; literal loopback HTTP delivery only
  cloud              Disabled in this local-only build (legacy source retained)
  resources          Operator file identity register/check; content stays outside memory
  protocol-mode <slug> Select the local protocol profile
  setup              Offline host adapter preview/apply/rollback; verified release required
  version/help       No network or database side effects

Use recalldock <command> --help for exact supported options.
Unbound cwd returns zero; --project and --all are mutually exclusive.
Use search -- --help for a literal option-shaped query.

Environment:
  (local production surface)
  RECALLDOCK_DATA_DIR       Selected local data directory (default ~/.recalldock)
  RECALLDOCK_PROJECT        Trusted MCP process project override
  RECALLDOCK_EXPECT_STORE_ID Store identity handshake; wrong store fails closed
  RECALLDOCK_READ_BUDGET_ID Trusted shared activation budget; no model reset
  RECALLDOCK_PORT           Local HTTP port
  RECALLDOCK_TIMEZONE       Display timezone
Legacy cloud/release/runner environment cannot enable outbound services here.
SQLite is authoritative; Markdown is interchange/projection, not a second master.
`, version)
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "recalldock: %s\n", store.ErrorMessage(err))
	exitFunc(1)
}

// resolveHomeFallback tries platform-specific environment variables to find
// a home directory when os.UserHomeDir() fails. This commonly happens on
// Windows when recalldock is launched as an MCP subprocess without full env
// propagation.
func resolveHomeFallback() string {
	// Windows: try common env vars that might be set even when
	// %USERPROFILE% is missing.
	for _, env := range []string{"USERPROFILE", "HOME", "LOCALAPPDATA"} {
		if v := os.Getenv(env); v != "" {
			if env == "LOCALAPPDATA" {
				// LOCALAPPDATA is C:\Users\<user>\AppData\Local — go up two levels.
				parent := filepath.Dir(filepath.Dir(v))
				if parent != "." && parent != v {
					return parent
				}
			}
			return v
		}
	}

	// Unix: $HOME should always work, but try passwd-style fallback.
	if v := os.Getenv("HOME"); v != "" {
		return v
	}

	return ""
}

// migrateOrphanedDB checks for recalldock databases that ended up in wrong
// locations (e.g. drive root on Windows when UserHomeDir failed silently)
// and moves them to the correct location if the correct location has no DB.
func migrateOrphanedDB(correctDir string) {
	correctDB := filepath.Join(correctDir, "recalldock.db")

	// If the correct DB already exists, nothing to migrate.
	if _, err := os.Stat(correctDB); err == nil {
		return
	}

	// Known wrong locations: relative ".recalldock" resolved from common roots.
	// On Windows this typically ends up at C:\.recalldock or D:\.recalldock.
	candidates := []string{
		filepath.Join(string(filepath.Separator), ".recalldock", "recalldock.db"),
	}

	// On Windows, check all drive letter roots.
	if filepath.Separator == '\\' {
		for _, drive := range "CDEFGHIJ" {
			candidates = append(candidates,
				filepath.Join(string(drive)+":\\", ".recalldock", "recalldock.db"),
			)
		}
	}

	for _, candidate := range candidates {
		if candidate == correctDB {
			continue
		}
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() {
			continue
		}

		// Found an orphaned DB — migrate it.
		log.Printf("[recalldock] found orphaned database at %s, migrating to %s", candidate, correctDB)

		if err := os.MkdirAll(correctDir, 0755); err != nil {
			log.Printf("[recalldock] migration failed (create dir): %v", err)
			return
		}

		// Move DB and WAL/SHM files if they exist.
		for _, suffix := range []string{"", "-wal", "-shm"} {
			src := candidate + suffix
			dst := correctDB + suffix
			if _, statErr := os.Stat(src); statErr != nil {
				continue
			}
			if renameErr := os.Rename(src, dst); renameErr != nil {
				log.Printf("[recalldock] migration failed (move %s): %v", filepath.Base(src), renameErr)
				return
			}
		}

		// Clean up empty orphaned directory.
		orphanDir := filepath.Dir(candidate)
		entries, _ := os.ReadDir(orphanDir)
		if len(entries) == 0 {
			os.Remove(orphanDir)
		}

		log.Printf("[recalldock] migration complete — memories recovered")
		return
	}
}

func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "..."
}
