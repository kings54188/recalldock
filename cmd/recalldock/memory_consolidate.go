package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"

	"recalldock.local/recalldock/internal/store"
)

// cmdMemoryConsolidate folds already-refined sources into a target memory. The
// target must hold the distilled text first; this command never rewrites
// knowledge, only archives sources and records lineage.
func cmdMemoryConsolidate(cfg store.Config) {
	if len(os.Args) < 4 {
		fatal(errors.New("usage: recalldock memory consolidate <target-id> --source <id> [--source <id>] --reason TEXT"))
		return
	}
	targetID, err := strconv.ParseInt(os.Args[3], 10, 64)
	if err != nil || targetID < 1 {
		fatal(errors.New("positive target observation ID required"))
		return
	}
	var sources []int64
	reason := ""
	for i := 4; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--source":
			if i+1 >= len(os.Args) {
				fatal(errors.New("--source requires an observation ID"))
				return
			}
			id, err := strconv.ParseInt(os.Args[i+1], 10, 64)
			if err != nil || id < 1 {
				fatal(errors.New("positive source observation ID required"))
				return
			}
			sources = append(sources, id)
			i++
		case "--reason":
			if i+1 >= len(os.Args) {
				fatal(errors.New("--reason requires text"))
				return
			}
			reason = os.Args[i+1]
			i++
		default:
			fatal(fmt.Errorf("unknown memory consolidate option %s", os.Args[i]))
			return
		}
	}
	if len(sources) == 0 {
		fatal(errors.New("at least one --source is required"))
		return
	}
	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
		return
	}
	defer s.Close()
	directory, err := requireCLIObservationWrite(s, targetID)
	if err != nil {
		fatal(err)
		return
	}
	for _, id := range sources {
		if _, err := requireCLIObservationWrite(s, id); err != nil {
			fatal(err)
			return
		}
	}
	absorbed, err := s.ConsolidateObservations(targetID, sources, reason, directory)
	if err != nil {
		var partial *store.ConsolidationError
		if errors.As(err, &partial) {
			fmt.Fprintf(os.Stderr, "absorbed: %v\n", absorbed)
			fatal(err)
			return
		}
		fatal(err)
		return
	}
	fmt.Printf("Consolidated into #%d: absorbed %d source(s) %v\n", targetID, len(absorbed), absorbed)
	fmt.Println("Sources are archived (out of recall/search/review) but remain readable by ID: recalldock memory show <id>")
}

func cmdMemorySources(cfg store.Config) {
	if len(os.Args) < 4 {
		fatal(errors.New("usage: recalldock memory sources <id> [--json]"))
		return
	}
	id, err := strconv.ParseInt(os.Args[3], 10, 64)
	if err != nil || id < 1 {
		fatal(errors.New("positive observation ID required"))
		return
	}
	options, err := parseCLIOptions(os.Args[4:], nil, []string{"--json"})
	if err != nil {
		fatal(err)
		return
	}
	asJSON := options.Flags["--json"]
	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
		return
	}
	defer s.Close()
	sources, err := s.ConsolidatedSources(id)
	if err != nil {
		fatal(err)
		return
	}
	if sources == nil {
		sources = []store.Observation{}
	}
	if asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"target": id, "sources": sources})
		return
	}
	if len(sources) == 0 {
		fmt.Printf("No consolidated sources for #%d\n", id)
		return
	}
	for _, source := range sources {
		fmt.Printf("#%d %q (archived=%v) sync=%s\n", source.ID, source.Title, source.ArchivedAt != nil, source.SyncID)
	}
}
