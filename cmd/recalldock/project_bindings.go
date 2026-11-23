package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"recalldock.local/recalldock/internal/store"
)

func cmdProjectBindings(cfg store.Config) {
	if err := runProjectBindings(cfg, os.Args[2:]); err != nil {
		fatal(err)
	}
}

func runProjectBindings(cfg store.Config, args []string) error {
	if len(args) < 2 || (args[0] != "bindings" && args[0] != "unbind") || strings.HasPrefix(args[1], "-") {
		return errors.New("use projects bindings PROJECT_ID [--limit 1..20] [--after BINDING_ID]; projects unbind PROJECT_ID --directory ABS_PATH --binding EXPECTED_ID")
	}
	sub, id := args[0], args[1]
	limit := 5
	var after, root, expected string
	seen := map[string]bool{}
	for i := 2; i < len(args); i++ {
		key := args[i]
		if seen[key] {
			return fmt.Errorf("duplicate option %s", key)
		}
		seen[key] = true
		if (sub == "bindings" && key != "--limit" && key != "--after") || (sub == "unbind" && key != "--directory" && key != "--binding") {
			return fmt.Errorf("unknown %s option %s", sub, key)
		}
		value, err := cliFlagValue(args, &i, key, "", false)
		if err != nil {
			return err
		}
		switch key {
		case "--limit":
			limit, err = strconv.Atoi(value)
			if err != nil || limit < 1 || limit > 20 {
				return errors.New("limit must be 1..20")
			}
		case "--after":
			after = value
		case "--directory":
			root = value
		case "--binding":
			expected = value
		}
	}
	if sub == "unbind" && (!filepath.IsAbs(root) || strings.TrimSpace(expected) == "") {
		return errors.New("unbind requires an absolute recorded directory and exact binding ID")
	}
	s, err := storeNew(cfg)
	if err != nil {
		return err
	}
	defer s.Close()
	var result any
	if sub == "bindings" {
		result, err = s.ListProjectBindings(id, limit, after)
	} else {
		result, err = s.DeactivateProjectBinding(id, root, expected)
	}
	if err != nil {
		return err
	}
	body, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(body))
	return nil
}
