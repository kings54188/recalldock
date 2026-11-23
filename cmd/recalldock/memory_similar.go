package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"

	"recalldock.local/recalldock/internal/store"
)

// cmdMemorySimilar prints read-only consolidation hints. It never edits or
// merges anything: the operator reviews the group and runs memory consolidate.
func cmdMemorySimilar(cfg store.Config) {
	project, threshold, limit := "", 0.6, 20
	asJSON := false
	for i := 3; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--project":
			if i+1 >= len(os.Args) {
				fatal(errors.New("--project requires a value"))
				return
			}
			project = os.Args[i+1]
			i++
		case "--threshold":
			if i+1 >= len(os.Args) {
				fatal(errors.New("--threshold requires a value"))
				return
			}
			value, err := strconv.ParseFloat(os.Args[i+1], 64)
			if err != nil || math.IsNaN(value) || value <= 0 || value >= 1 {
				fatal(errors.New("--threshold must be between 0 and 1"))
				return
			}
			threshold = value
			i++
		case "--limit":
			if i+1 >= len(os.Args) {
				fatal(errors.New("--limit requires a value"))
				return
			}
			value, err := strconv.Atoi(os.Args[i+1])
			if err != nil || value < 1 || value > 50 {
				fatal(errors.New("--limit must be 1..50"))
				return
			}
			limit = value
			i++
		case "--json":
			asJSON = true
		default:
			fatal(fmt.Errorf("unknown memory similar option %s", os.Args[i]))
			return
		}
	}
	if project == "" {
		fatal(errors.New("memory similar requires --project PROJECT (explicit history scan)"))
		return
	}
	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
		return
	}
	defer s.Close()
	groups, err := s.SimilarMemoryGroups(project, threshold, limit)
	if err != nil {
		fatal(err)
		return
	}
	if asJSON {
		type item struct {
			Score           float64  `json:"score"`
			SuggestedTarget int64    `json:"suggested_target"`
			IDs             []int64  `json:"ids"`
			Titles          []string `json:"titles"`
		}
		items := make([]item, 0, len(groups))
		for _, group := range groups {
			entry := item{Score: group.Score, SuggestedTarget: group.SuggestedTarget}
			for _, observation := range group.Observations {
				entry.IDs = append(entry.IDs, observation.ID)
				entry.Titles = append(entry.Titles, observation.Title)
			}
			items = append(items, entry)
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"project": project, "threshold": threshold, "groups": items})
		return
	}
	if len(groups) == 0 {
		fmt.Printf("No similar groups above threshold %.2f in %s\n", threshold, project)
		return
	}
	for index, group := range groups {
		fmt.Printf("Group %d score=%.2f suggested_target=#%d (%d memories)\n", index+1, group.Score, group.SuggestedTarget, len(group.Observations))
		for _, observation := range group.Observations {
			marker := " "
			if observation.ID == group.SuggestedTarget {
				marker = "*"
			}
			fmt.Printf("  %s #%d [%s] %s\n", marker, observation.ID, observation.Type, observation.Title)
		}
	}
	fmt.Println("Read-only hint: refine the target first (memory update), then memory consolidate TARGET --source ID ... --reason TEXT")
}
