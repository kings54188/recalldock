package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"recalldock.local/recalldock/internal/store"
)

// Explicit local operator surface, not an MCP/HTTP approval capability.
func cmdMemory(cfg store.Config) {
	if len(os.Args) >= 3 && os.Args[2] == "similar" {
		cmdMemorySimilar(cfg)
		return
	}
	if len(os.Args) >= 3 && os.Args[2] == "consolidate" {
		cmdMemoryConsolidate(cfg)
		return
	}
	if len(os.Args) >= 3 && os.Args[2] == "sources" {
		cmdMemorySources(cfg)
		return
	}
	if len(os.Args) >= 3 && (os.Args[2] == "archive" || os.Args[2] == "reactivate" || os.Args[2] == "archived") {
		cmdMemoryLifecycle(cfg)
		return
	}
	if len(os.Args) >= 3 && os.Args[2] == "cooldown" {
		cmdMemoryCooldown(cfg)
		return
	}
	if len(os.Args) < 4 {
		fatal(errors.New("usage: recalldock memory <show|history|version|update|cooldown|similar|consolidate|sources> ID [options]"))
		return
	}
	sub := os.Args[2]
	id, err := strconv.ParseInt(os.Args[3], 10, 64)
	if err != nil || id < 1 {
		fatal(errors.New("positive observation ID required"))
		return
	}
	var sequence, before int64
	limit := 5
	full, deleted := false, false
	expected, input, reason := "", "", ""
	seen := map[string]bool{}
	start := 4
	if sub == "version" {
		if len(os.Args) < 5 {
			fatal(errors.New("version requires a positive sequence"))
			return
		}
		sequence, err = strconv.ParseInt(os.Args[4], 10, 64)
		if err != nil || sequence < 1 {
			fatal(errors.New("positive sequence required"))
			return
		}
		start = 5
	}
	if sub != "show" && sub != "history" && sub != "version" && sub != "update" {
		fatal(errors.New("unknown memory subcommand"))
		return
	}
	for i := start; i < len(os.Args); i++ {
		key := os.Args[i]
		if seen[key] {
			fatal(fmt.Errorf("duplicate memory option %s", key))
			return
		}
		seen[key] = true
		switch key {
		case "--json": // JSON is the bounded automation-friendly default.
		case "--full":
			if sub != "show" && sub != "version" {
				fatal(errors.New("--full requires a selected show/version"))
				return
			}
			full = true
		case "--include-deleted":
			if sub != "history" && sub != "version" {
				fatal(errors.New("--include-deleted is history/version only"))
				return
			}
			deleted = true
		case "--expected", "--input", "--reason", "--limit", "--before":
			value, e := cliFlagValue(os.Args, &i, key, "", false)
			if e != nil {
				fatal(e)
				return
			}
			if key == "--limit" || key == "--before" {
				if sub != "history" {
					fatal(errors.New("limit/before are history only"))
					return
				}
				n, e := strconv.ParseInt(value, 10, 64)
				if e != nil || n < 1 {
					fatal(errors.New("positive history limit/cursor required"))
					return
				}
				if key == "--limit" {
					if n > 20 {
						fatal(errors.New("history limit must be 1..20"))
						return
					}
					limit = int(n)
				} else {
					before = n
				}
			} else {
				if sub != "update" {
					fatal(errors.New("expected/input/reason are update only"))
					return
				}
				switch key {
				case "--expected":
					expected = value
				case "--input":
					input = value
				case "--reason":
					reason = value
				}
			}
		default:
			fatal(fmt.Errorf("unknown memory option %s", key))
			return
		}
	}
	var patch store.UpdateObservationParams
	if sub == "update" {
		if expected == "" || input == "" || strings.TrimSpace(reason) == "" {
			fatal(errors.New("memory update requires --expected SNAPSHOT_SHA256 --input PATCH_JSON --reason TEXT"))
			return
		}
		info, err := os.Lstat(input)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 {
			fatal(errors.New("edit input must be a regular JSON file <=64KiB"))
			return
		}
		f, err := os.Open(input)
		if err != nil {
			fatal(err)
			return
		}
		body, err := io.ReadAll(io.LimitReader(f, 64<<10+1))
		f.Close()
		if err != nil || len(body) > 64<<10 {
			fatal(errors.New("edit input exceeds size limit"))
			return
		}
		trimmed := bytes.TrimSpace(body)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			fatal(errors.New("edit input must be a JSON object"))
			return
		}
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&patch) != nil {
			fatal(errors.New("invalid or unsupported edit patch fields"))
			return
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			fatal(errors.New("edit input must contain exactly one JSON object"))
			return
		}
	}
	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
		return
	}
	defer s.Close()
	var result any
	switch sub {
	case "show":
		selected, e := s.SelectObservationForEdit(id)
		err = e
		if !full {
			selected.State = nil
		}
		result = selected
	case "history":
		result, err = s.ObservationHistory(id, deleted, limit, before)
	case "version":
		version, e := s.ObservationVersion(id, sequence, deleted)
		err = e
		if full {
			result = version
		} else {
			result = version.ObservationVersionSummary
		}
	case "update":
		directory, e := requireCLIObservationWrite(s, id)
		if e != nil {
			fatal(e)
			return
		}
		selected, e := s.UpdateObservationCAS(id, expected, patch, reason, directory)
		err = e
		selected.State = nil
		result = selected
	}
	if err != nil {
		if errors.Is(err, store.ErrObservationNotFound) {
			fatal(fmt.Errorf("observation #%d not found", id))
			return
		}
		if errors.Is(err, store.ErrObservationVersionNotFound) {
			fatal(fmt.Errorf("observation #%d has no history version %d", id, sequence))
			return
		}
		fatal(err)
		return
	}
	encoded, err := jsonMarshalIndent(result, "", "  ")
	if err != nil {
		fatal(err)
		return
	}
	fmt.Println(string(encoded))
}

// cmdMemoryCooldown implements `memory cooldown`: an explicit operator
// decision surface for protected cool-down archiving. It never deletes and
// never touches pinned/protected memories; it only marks rarely-recalled
// observations for review within a bounded horizon (docs/NEXT-ARCHITECTURE.md
// §5: 冷暖影响召回与复核,不改变事实真假,也不直接删除).
//
//	usage: recalldock memory cooldown --project PROJECT [--min-age 30d] [--dry-run]
//	       recalldock memory cooldown --all [--min-age 30d] [--dry-run]
func cmdMemoryCooldown(cfg store.Config) {
	options, err := parseCLIOptions(os.Args[3:], []string{"--project", "--min-age"}, []string{"--all", "--dry-run", "--json"})
	if err != nil {
		fatal(err)
		return
	}
	project := options.Values["--project"]
	all, dryRun := options.Flags["--all"], options.Flags["--dry-run"]
	if all && project != "" {
		fatal(errors.New("--project and --all are mutually exclusive"))
		return
	}
	minAge := 30 * 24 * time.Hour
	if value := options.Values["--min-age"]; value != "" {
		d, err := parseDurationDays(value)
		if err != nil || d < 24*time.Hour {
			fatal(errors.New("--min-age must be at least 1d (format: 30d)"))
			return
		}
		minAge = d
	}
	if !all && project == "" {
		fatal(errors.New("memory cooldown requires --project PROJECT or --all"))
		return
	}
	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
		return
	}
	defer s.Close()
	if !all {
		project, err = s.ResolveExistingProject(project)
		if err != nil {
			fatal(err)
			return
		}
		if _, err := resolveCLIWriteOwner(s, project); err != nil {
			fatal(err)
			return
		}
	}
	result, err := s.CooldownArchive(project, all, minAge, dryRun)
	if err != nil {
		fatal(err)
		return
	}
	if dryRun {
		fmt.Fprintln(os.Stderr, "[dry-run] no changes applied")
	}
	encoded, err := jsonMarshalIndent(result, "", "  ")
	if err != nil {
		fatal(err)
		return
	}
	fmt.Println(string(encoded))
}

// parseDurationDays accepts "30d", "12h" style bounded values.
func parseDurationDays(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if len(v) < 2 {
		return 0, errors.New("invalid duration")
	}
	unit := v[len(v)-1]
	num, err := strconv.Atoi(v[:len(v)-1])
	if err != nil || num <= 0 {
		return 0, errors.New("invalid duration")
	}
	switch unit {
	case 'd':
		return time.Duration(num) * 24 * time.Hour, nil
	case 'h':
		return time.Duration(num) * time.Hour, nil
	default:
		return 0, errors.New("duration unit must be d or h")
	}
}
