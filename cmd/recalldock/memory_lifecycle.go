package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"recalldock.local/recalldock/internal/store"
)

func cmdMemoryLifecycle(cfg store.Config) {
	if err := runMemoryLifecycle(cfg, os.Args[2:]); err != nil {
		fatal(err)
	}
}

func runMemoryLifecycle(cfg store.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("memory archive/reactivate requires a selected ID; archived requires --project")
	}
	sub := args[0]
	if sub != "archive" && sub != "reactivate" && sub != "archived" {
		return errors.New("unknown memory lifecycle command")
	}
	var id, before int64
	limit := 5
	var expected, reason, project string
	start := 1
	if sub != "archived" {
		if len(args) < 2 {
			return errors.New("positive observation ID required")
		}
		var err error
		id, err = strconv.ParseInt(args[1], 10, 64)
		if err != nil || id < 1 {
			return errors.New("positive observation ID required")
		}
		start = 2
	}
	seen := map[string]bool{}
	for i := start; i < len(args); i++ {
		key := args[i]
		if seen[key] {
			return fmt.Errorf("duplicate lifecycle option %s", key)
		}
		seen[key] = true
		if key == "--json" {
			continue
		}
		if key != "--expected" && key != "--reason" && key != "--project" && key != "--limit" && key != "--before" {
			return fmt.Errorf("unknown lifecycle option %s", key)
		}
		value, err := cliFlagValue(args, &i, key, "", false)
		if err != nil {
			return err
		}
		if sub == "archived" {
			switch key {
			case "--project":
				project = value
			case "--limit":
				n, e := strconv.Atoi(value)
				if e != nil || n < 1 || n > 20 {
					return errors.New("limit must be 1..20")
				}
				limit = n
			case "--before":
				n, e := strconv.ParseInt(value, 10, 64)
				if e != nil || n < 1 {
					return errors.New("before must be positive")
				}
				before = n
			default:
				return errors.New("archived accepts project/limit/before only")
			}
		} else {
			switch key {
			case "--expected":
				expected = value
			case "--reason":
				reason = value
			default:
				return errors.New("archive/reactivate accepts expected/reason only")
			}
		}
	}
	if sub == "archived" && strings.TrimSpace(project) == "" {
		return errors.New("archived requires --project PROJECT")
	}
	if sub != "archived" && (len(expected) != 64 || strings.TrimSpace(reason) == "") {
		return errors.New("archive/reactivate requires --expected SNAPSHOT_SHA256 --reason TEXT")
	}
	if sub != "archived" {
		if _, err := hex.DecodeString(expected); err != nil {
			return errors.New("expected snapshot must be a SHA256 hex digest")
		}
		if !utf8.ValidString(reason) || len(reason) > 512 {
			return errors.New("reason must be bounded UTF-8 text")
		}
	}
	s, err := storeNew(cfg)
	if err != nil {
		return err
	}
	defer s.Close()
	var result any
	if sub == "archived" {
		result, err = s.ArchivedMemories(project, before, limit)
	} else {
		dir, e := os.Getwd()
		if e != nil {
			return e
		}
		if _, e := requireCLIObservationWrite(s, id); e != nil {
			return e
		}
		archive := sub == "archive"
		selected, e := s.UpdateObservationCAS(id, expected, store.UpdateObservationParams{Archived: &archive}, reason, dir)
		err = e
		selected.State = nil
		result = selected
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}
