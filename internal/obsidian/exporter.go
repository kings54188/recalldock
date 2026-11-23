package obsidian

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"recalldock.local/recalldock/internal/store"
)

// ExportConfig holds all CLI flags for the obsidian-export command.
type ExportConfig struct {
	VaultPath      string          // --vault (required): path to the Obsidian vault root
	Project        string          // --project (optional): filter export to a single project
	Limit          int             // --limit (0 = no limit)
	Since          time.Time       // retained for CLI compatibility; graph membership is always reconciled
	Force          bool            // --force: ignore state, full re-export
	GraphConfig    GraphConfigMode // --graph-config: preserve|force|skip (empty string = skip for backward compat)
	AttachmentRoot string          // explicit opt-in; registered hash-verified local files only
}

// StoreReader is the read-only interface the exporter needs.
// Keeps the dependency narrow — easy to mock in tests.
type StoreReader interface {
	Export() (*store.ExportData, error)
	Stats() *store.Stats
}

// Exporter reads from the store and writes markdown files to a vault.
type Exporter struct {
	store  StoreReader
	config ExportConfig
}

// NewExporter constructs an Exporter. Validation happens in Export().
func NewExporter(s StoreReader, cfg ExportConfig) *Exporter {
	return &Exporter{store: s, config: cfg}
}

// sanitizePathComponent strips path separators and dot-dot sequences from a
// single path component (project name or observation type), preventing path
// traversal attacks when the value is used inside filepath.Join.
// Any slash, backslash, or ".." element is replaced with "_".
func sanitizePathComponent(s string) string {
	// Replace OS separators with underscore.
	s = strings.ReplaceAll(s, string(filepath.Separator), "_")
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")

	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || strings.ContainsRune("[]#|^:", r) {
			return '_'
		}
		return r
	}, s)

	// Replace ".." with "__" to neutralise traversal.
	s = strings.ReplaceAll(s, "..", "__")

	// filepath.Clean on a single component that contains no separators is a
	// no-op, but run it anyway to normalise any remaining oddities (e.g. trailing
	// dots on Windows). We re-apply the separator guard afterwards because Clean
	// can introduce a leading "/" on absolute-looking inputs on Unix.
	clean := filepath.Clean(s)
	clean = strings.ReplaceAll(clean, string(filepath.Separator), "_")
	clean = strings.ReplaceAll(clean, "/", "_")
	if clean == "" || clean == "." {
		clean = "_"
	}
	return clean
}

// SetGraphConfig sets the GraphConfig mode on this exporter's config.
// This is used by Watcher to force GraphConfigSkip on subsequent cycles (REQ-WATCH-06).
func (e *Exporter) SetGraphConfig(mode GraphConfigMode) {
	e.config.GraphConfig = mode
}

// GraphConfig returns the current GraphConfig mode.
func (e *Exporter) GraphConfig() GraphConfigMode {
	return e.config.GraphConfig
}

// vaultRoot returns the {vault}/recalldock/ root path.
func (e *Exporter) vaultRoot() string {
	return filepath.Join(e.config.VaultPath, "recalldock")
}

// stateFilePath returns the path to the sync state JSON file.
func (e *Exporter) stateFilePath() string {
	return filepath.Join(e.vaultRoot(), ".recalldock-sync-state.json")
}

// Export performs a full or incremental export from the store to the vault.
// It returns an ExportResult summarizing what happened.
func (e *Exporter) Export() (*ExportResult, error) {
	// ── Validate config ───────────────────────────────────────────────────────
	if e.config.VaultPath == "" {
		return nil, fmt.Errorf("obsidian: --vault path is required")
	}
	if err := ValidateAttachmentRoot(e.config.AttachmentRoot); err != nil {
		return nil, err
	}

	if e.config.Limit != 0 || !e.config.Since.IsZero() {
		return nil, fmt.Errorf("partial graph export (--limit/--since) is not supported safely; export a complete --project to a dedicated vault")
	}

	// ── Create vault namespace directory ─────────────────────────────────────
	engRoot := e.vaultRoot()
	if _, err := managedPath(engRoot, "."); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(engRoot, 0755); err != nil {
		return nil, fmt.Errorf("obsidian: create vault dir %q: %w", engRoot, err)
	}
	// Create hub directories up front
	sessionsDir := filepath.Join(engRoot, "_sessions")
	topicsDir := filepath.Join(engRoot, "_topics")
	for _, d := range []string{sessionsDir, topicsDir} {
		if _, err := managedPath(engRoot, filepath.Base(d)); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(d, 0755); err != nil {
			return nil, fmt.Errorf("obsidian: create dir %q: %w", d, err)
		}
	}

	lockPath, err := managedPath(engRoot, ".export.lock")
	if err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("export lock unavailable (running export or stale lock; inspect before removal): %w", err)
	}
	defer func() { lock.Close(); os.Remove(lockPath) }()

	// ── Read incremental state ────────────────────────────────────────────────
	if _, err := managedPath(engRoot, filepath.Base(e.stateFilePath())); err != nil {
		return nil, err
	}
	state, err := ReadState(e.stateFilePath())
	if err != nil {
		return nil, fmt.Errorf("obsidian: read state: %w", err)
	}
	// A force refresh never discards provenance or manual-edit protection.
	if identified, ok := e.store.(interface {
		Identity() (store.StoreIdentity, error)
	}); ok {
		identity, err := identified.Identity()
		if err != nil {
			return nil, err
		}
		if identity.StoreID == "" {
			return nil, fmt.Errorf("empty source store identity")
		}
		if state.SourceStoreID != "" && state.SourceStoreID != identity.StoreID {
			return nil, fmt.Errorf("vault projection belongs to a different database; choose a separate vault")
		}
		if state.SourceStoreID == "" && (len(state.Files) != 0 || len(state.SessionHubs) != 0 || len(state.TopicHubs) != 0) {
			return nil, fmt.Errorf("legacy projection has no database provenance; use a fresh vault, do not guess ownership")
		}
		state.SourceStoreID = identity.StoreID
	}

	if state.Version != 0 && state.ProjectFilter != e.config.Project {
		return nil, fmt.Errorf("vault projection scope differs; choose a separate vault for this project filter")
	}
	state.ProjectFilter = e.config.Project

	// ── Write graph config (first cycle only, controlled by caller) ───────────
	// Empty string means skip — preserves backward compatibility with existing
	// code that doesn't set GraphConfig. Watcher sets it to GraphConfigSkip on
	// subsequent cycles to avoid clobbering user edits.
	graphMode := e.config.GraphConfig
	if graphMode == "" {
		graphMode = GraphConfigSkip
	}
	if err := WriteGraphConfig(e.config.VaultPath, graphMode); err != nil {
		return nil, fmt.Errorf("obsidian: write graph config: %w", err)
	}

	// ── Fetch data from store ─────────────────────────────────────────────────
	var data *store.ExportData
	if scoped, ok := e.store.(interface {
		ExportProject(string) (*store.ExportData, error)
	}); ok && e.config.Project != "" {
		data, err = scoped.ExportProject(e.config.Project)
	} else {
		data, err = e.store.Export()
	}
	if err != nil {
		return nil, fmt.Errorf("obsidian: store export: %w", err)
	}

	result := &ExportResult{}
	graphLinks, err := knowledgeGraphLinks(e.store, data, e.config.Project)
	if err != nil {
		return nil, fmt.Errorf("obsidian: project graph: %w", err)
	}
	bodies, err := projectedBodies(data, e.config.Project)
	if err != nil {
		return nil, err
	}
	attachmentLinks, err := e.exportAttachments(data, &state)
	if err != nil {
		return nil, fmt.Errorf("obsidian: attachments: %w", err)
	}

	// ── Handle deleted observations: clean up files ───────────────────────────
	for _, obs := range data.Observations {
		if obs.DeletedAt == nil {
			continue
		}
		relPath, tracked := state.Files[obs.ID]
		if !tracked {
			continue
		}
		if err := removeManagedNote(engRoot, relPath, &state); err != nil {
			result.Errors = append(result.Errors, err)
		} else {
			result.Deleted++
			delete(state.Files, obs.ID)
		}
	}

	// ── Filter and export observations ────────────────────────────────────────
	// sessionObsRefs tracks obs per session for hub generation.
	sessionObsRefs := make(map[string][]ObsRef)
	// topicObsRefs tracks obs per topic prefix for hub generation.
	topicObsRefs := make(map[string][]ObsRef)

	topicCounts := make(map[string]int)
	for _, obs := range data.Observations {
		if obs.DeletedAt != nil || (e.config.Project != "" && (obs.Project == nil || *obs.Project != e.config.Project)) {
			continue
		}
		if prefix := obsTopicPrefix(obs); prefix != "" {
			topicCounts[prefix]++
		}
	}

	for _, obs := range data.Observations {
		// Skip deleted
		if obs.DeletedAt != nil {
			continue
		}

		// Project filter
		if e.config.Project != "" {
			proj := ""
			if obs.Project != nil {
				proj = *obs.Project
			}
			if proj != e.config.Project {
				continue
			}
		}

		// Always render current membership: another record can create/remove a hub
		// without changing this observation. Byte comparison below avoids redundant writes.

		// Determine target file path — sanitize project and type to prevent
		// path traversal (issue #180): strip separators and dot-dot sequences,
		// then verify the resolved absolute path stays inside engRoot.
		project := "unknown"
		if obs.Project != nil && *obs.Project != "" {
			project = sanitizePathComponent(*obs.Project)
		}
		obsType := sanitizePathComponent(obs.Type)
		slug := observationStem(obs)
		relPath := filepath.Join(project, obsType, slug+".md")
		absDir := filepath.Join(engRoot, project, obsType)
		absPath, err := managedPath(engRoot, relPath)
		if err != nil {
			result.Errors = append(result.Errors, err)
			continue
		}

		// Containment check: reject any path that escapes engRoot after cleaning.
		cleanRoot := filepath.Clean(engRoot)
		cleanPath := filepath.Clean(absPath)
		if !strings.HasPrefix(cleanPath, cleanRoot+string(filepath.Separator)) {
			result.Errors = append(result.Errors, fmt.Errorf("unsafe path rejected (would escape export root): %s", cleanPath))
			continue
		}

		// Create directory
		if err := os.MkdirAll(absDir, 0755); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("mkdir %s: %w", absDir, err))
			continue
		}

		// Generate markdown content
		obs.Content = bodies[obs.ID]
		content := observationToMarkdown(obs, ShouldCreateTopicHub(topicCounts[obsTopicPrefix(obs)]))
		if links := graphLinks[obs.ID]; len(links) > 0 {
			content += "\n## Knowledge relationships\n\n" + strings.Join(links, "\n") + "\n"
		}
		if links := attachmentLinks[obs.ID]; len(links) > 0 {
			content += "\n## Registered attachments\n\n" + strings.Join(links, "\n") + "\n"
		}

		status, err := writeManagedNote(engRoot, relPath, []byte(content), &state)
		if err != nil {
			result.Errors = append(result.Errors, err)
			continue
		}
		switch status {
		case "created":
			result.Created++
		case "updated":
			result.Updated++
		case "skipped":
			result.Skipped++
		}
		if previous := state.Files[obs.ID]; previous != "" && previous != relPath {
			if err := removeManagedNote(engRoot, previous, &state); err != nil {
				result.Errors = append(result.Errors, err)
				continue
			}
		}
		state.Files[obs.ID] = relPath

		// Collect for hub generation
		ref := obsToRef(obs)
		if obs.SessionID != "" {
			sessionObsRefs[obs.SessionID] = append(sessionObsRefs[obs.SessionID], ref)
		}
		if prefix := obsTopicPrefix(obs); prefix != "" {
			topicObsRefs[prefix] = append(topicObsRefs[prefix], ref)
		}
	}

	// ── Generate session hub notes ────────────────────────────────────────────
	for sessionID, refs := range sessionObsRefs {
		if len(refs) == 0 {
			continue
		}
		hubPath, err := managedPath(engRoot, filepath.Join("_sessions", hubStem(sessionID)+".md"))
		if err != nil {
			result.Errors = append(result.Errors, err)
			continue
		}
		content := SessionHubMarkdown(sessionID, refs)
		hubRel, _ := filepath.Rel(engRoot, hubPath)
		if _, err := writeManagedNote(engRoot, hubRel, []byte(content), &state); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("write session hub %s: %w", hubPath, err))
			continue
		}
		state.SessionHubs[sessionID] = filepath.Join("_sessions", hubStem(sessionID)+".md")
		result.HubsCreated++
	}

	// ── Generate topic hub notes ──────────────────────────────────────────────
	for prefix, refs := range topicObsRefs {
		if !ShouldCreateTopicHub(len(refs)) {
			continue
		}
		safeName := hubStem(prefix)
		hubPath, err := managedPath(engRoot, filepath.Join("_topics", safeName+".md"))
		if err != nil {
			result.Errors = append(result.Errors, err)
			continue
		}
		content := TopicHubMarkdown(prefix, refs)
		hubRel, _ := filepath.Rel(engRoot, hubPath)
		if _, err := writeManagedNote(engRoot, hubRel, []byte(content), &state); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("write topic hub %s: %w", hubPath, err))
			continue
		}
		state.TopicHubs[prefix] = filepath.Join("_topics", safeName+".md")
		result.HubsCreated++
	}

	// The recorded source and project filter fix ownership of this complete projection.
	{
		for id, rel := range state.SessionHubs {
			if len(sessionObsRefs[id]) != 0 {
				continue
			}
			if err := removeManagedNote(engRoot, rel, &state); err != nil {
				result.Errors = append(result.Errors, err)
			} else {
				delete(state.SessionHubs, id)
			}
		}
		for prefix, rel := range state.TopicHubs {
			if ShouldCreateTopicHub(len(topicObsRefs[prefix])) {
				continue
			}
			if err := removeManagedNote(engRoot, rel, &state); err != nil {
				result.Errors = append(result.Errors, err)
			} else {
				delete(state.TopicHubs, prefix)
			}
		}
	}

	// ── Persist updated state ─────────────────────────────────────────────────
	state.LastExportAt = time.Now().UTC().Format(time.RFC3339)
	state.Version = 2
	if err := WriteState(e.stateFilePath(), state); err != nil {
		return nil, fmt.Errorf("obsidian: write state: %w", err)
	}

	return result, nil
}

// obsToRef converts a store.Observation to a lightweight ObsRef for hub building.
func obsToRef(obs store.Observation) ObsRef {
	topicKey := ""
	if obs.TopicKey != nil {
		topicKey = *obs.TopicKey
	}
	project := "unknown"
	if obs.Project != nil && *obs.Project != "" {
		project = sanitizePathComponent(*obs.Project)
	}
	return ObsRef{
		LinkTarget: "recalldock/" + filepath.ToSlash(filepath.Join(project, sanitizePathComponent(obs.Type), observationStem(obs))),
		Slug:       observationStem(obs),
		Title:      obs.Title,
		TopicKey:   topicKey,
		Type:       obs.Type,
	}
}

// obsTopicPrefix returns the topic prefix for an observation (for hub grouping).
// Returns "" if the observation has no topic_key.
func obsTopicPrefix(obs store.Observation) string {
	if obs.TopicKey == nil || *obs.TopicKey == "" {
		return ""
	}
	return topicPrefix(*obs.TopicKey)
}
