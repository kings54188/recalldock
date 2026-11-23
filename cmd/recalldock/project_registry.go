package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"recalldock.local/recalldock/internal/store"
)

// Registered identity commands never resolve by display-name similarity and do
// not change a global active project. Historical activation is a read selector.
func cmdProjectRegistry(cfg store.Config) {
	sub := os.Args[2]
	var name, directory, id, projectID string
	budget := store.DefaultReadBudget
	budgetExplicit := false
	confirm := false
	for i := 3; i < len(os.Args); i++ {
		arg := os.Args[i]
		switch arg {
		case "--json":
		case "--confirm":
			confirm = true
		case "--name", "--directory", "--token-budget", "--project":
			if i+1 >= len(os.Args) {
				fatal(fmt.Errorf("%s requires a value", arg))
				return
			}
			i++
			if arg == "--token-budget" {
				value, err := strconv.Atoi(os.Args[i])
				if err != nil || value < 1 || value > store.MaxReadBudget {
					fatal(errors.New("activation token budget must be 1..32000"))
					return
				}
				budget = value
				budgetExplicit = true
			} else if arg == "--name" {
				name = os.Args[i]
			} else if arg == "--project" {
				projectID = os.Args[i]
			} else {
				directory = os.Args[i]
			}
		default:
			if strings.HasPrefix(arg, "-") || id != "" {
				fatal(fmt.Errorf("unknown project registry option %s", arg))
				return
			}
			id = arg
		}
	}
	if sub == "delete" && projectID != "" && !confirm {
		fatal(errors.New("projects delete requires --confirm"))
		return
	}
	if (budgetExplicit && sub != "activate") || (sub == "create" && (name == "" || id != "" || directory != "")) || (sub == "adopt" && (name == "" || id != "" || directory != "")) || (sub == "bind" && (id == "" || directory == "" || name != "")) || (sub == "activate" && (id == "" || directory != "" || name != "")) || (sub == "budget" && (id == "" || name != "" || directory != "")) || (sub == "registry" && (id != "" || name != "" || directory != "")) || (sub == "delete" && (projectID == "" || name != "" || directory != "" || confirm == false)) {
		fatal(errors.New("use projects create --name NAME; projects adopt --name LEGACY_NAME; projects bind ID --directory ABS_PATH; projects activate ID; projects delete --project ID --confirm"))
		return
	}
	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
		return
	}
	defer s.Close()
	var result any
	switch sub {
	case "create":
		result, err = s.CreateRegisteredProject(name)
	case "adopt":
		result, err = s.AdoptLegacyProject(name)
	case "delete":
		err = s.DeleteRegisteredProject(projectID, confirm)
		result = map[string]any{"deleted": projectID}
	case "bind":
		result, err = s.BindProjectRoot(id, directory)
	case "activate":
		result, err = s.ActivateProjectReadOnly(id, budget)
	case "registry":
		var identities []store.RegisteredProject
		identities, err = s.ListRegisteredProjects()
		result = identities
		if err == nil {
			fmt.Fprintf(os.Stderr, "Project registry (%d): registered identities, including projects without sessions or observations.\n", len(identities))
		}
	case "budget":
		result, err = s.ReadBudget(id)
	}
	if err != nil {
		fatal(err)
		return
	}
	body, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fatal(err)
		return
	}
	fmt.Println(string(body))
}
