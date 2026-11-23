package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"recalldock.local/recalldock/internal/project"
	"recalldock.local/recalldock/internal/store"
)

type cliReadScope struct {
	Project string
	All     bool
	Bound   bool
	Label   string
}

func openCLIStore(cfg store.Config) (*store.Store, error) {
	s, err := store.New(cfg)
	if err != nil {
		return nil, err
	}
	if cfg.RequireExisting || len(os.Args) > 1 && requiresExistingStore(os.Args[1:]) {
		args := os.Args[2:]
		for i := 0; i < len(args); i++ {
			if args[i] == "--" {
				break
			}
			key, value, inline := strings.Cut(args[i], "=")
			if key != "--project" {
				continue
			}
			if !inline {
				if i+1 >= len(args) {
					s.Close()
					return nil, fmt.Errorf("--project requires a value")
				}
				i++
				value = args[i]
			}
			if _, err := s.ResolveExistingProject(value); err != nil {
				s.Close()
				return nil, err
			}
		}
	}
	return s, nil
}

// Only an explicit local marker/config can activate history automatically.
// Git names, a reused basename or a child repository are discovery hints, not identities.
func resolveCLIReadScope(s *store.Store, explicit string, all bool) (cliReadScope, error) {
	if all && explicit != "" {
		return cliReadScope{}, fmt.Errorf("--project and --all are mutually exclusive")
	}
	if all {
		return cliReadScope{All: true, Bound: true, Label: "all"}, nil
	}
	if explicit != "" {
		explicit, _ = store.NormalizeProject(explicit)
		canonical, err := s.ResolveExistingProject(explicit)
		if err != nil {
			return cliReadScope{}, err
		}
		explicit = canonical
		return cliReadScope{Project: explicit, Bound: true, Label: "project:" + explicit}, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return cliReadScope{}, err
	}
	detected := project.DetectExplicitWithRegistry(cwd, s)
	if detected.Source == project.SourceRegistered || detected.Source == project.SourceConfig {
		if detected.Error != nil {
			return cliReadScope{}, detected.Error
		}
		if detected.Project != "" {
			return cliReadScope{Project: detected.Project, Bound: true, Label: "project:" + detected.Project}, nil
		}
	}
	return cliReadScope{Label: "unbound cwd (0 memories; use --project PROJECT or --all explicitly)"}, nil
}

func writeCLIJSON(value any) error {
	body, err := jsonMarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(body))
	return nil
}

type cliOptions struct {
	Values map[string]string
	Flags  map[string]bool
}

func parseCLIOptions(args []string, valueFlags, boolFlags []string) (cliOptions, error) {
	p := cliOptions{Values: map[string]string{}, Flags: map[string]bool{}}
	allowed := map[string]bool{}
	for _, key := range valueFlags {
		allowed[key] = true
	}
	for _, key := range boolFlags {
		allowed[key] = false
	}
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			if i+1 != len(args) {
				return p, fmt.Errorf("unexpected positional argument %q after --", args[i+1])
			}
			break
		}
		key, value, inline := strings.Cut(args[i], "=")
		takesValue, ok := allowed[key]
		if !ok {
			return p, fmt.Errorf("unknown option %q", args[i])
		}
		if seen[key] {
			return p, fmt.Errorf("duplicate option %q", key)
		}
		seen[key] = true
		if takesValue {
			value, err := cliFlagValue(args, &i, key, value, inline)
			if err != nil {
				return p, err
			}
			p.Values[key] = value
		} else {
			if inline {
				return p, fmt.Errorf("%s takes no value", key)
			}
			p.Flags[key] = true
		}
	}
	return p, nil
}

func (p cliOptions) integer(key string, fallback, minimum, maximum int) (int, error) {
	value, ok := p.Values[key]
	if !ok {
		return fallback, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < minimum || n > maximum {
		return 0, fmt.Errorf("%s must be %d..%d", key, minimum, maximum)
	}
	return n, nil
}

func parseReviewOptions(args []string) (cliOptions, error) {
	positionals := 0
	var values []string
	switch args[0] {
	case "list":
		values = []string{"--project", "--status", "--limit"}
	case "mark":
		positionals = 1
	case "link":
		positionals, values = 2, []string{"--reason"}
	case "dependencies":
		values = []string{"--project", "--status", "--source", "--dependent", "--limit"}
	case "resource-link":
		positionals, values = 2, []string{"--project", "--reason"}
	case "resource-dependencies":
		values = []string{"--project", "--status", "--source-ref", "--dependent", "--limit"}
	case "resource-changed":
		positionals, values = 1, []string{"--project", "--replacement-source-ref", "--reason"}
	case "resource-refresh":
		positionals, values = 1, []string{"--project", "--source-ref", "--reason"}
	case "process":
		values = []string{"--limit"}
	default:
		return cliOptions{}, fmt.Errorf("unknown review command %q", args[0])
	}
	if len(args) < 1+positionals {
		return cliOptions{}, fmt.Errorf("review %s requires %d positional arguments", args[0], positionals)
	}
	bools := []string{"--json"}
	if args[0] == "list" || args[0] == "dependencies" || args[0] == "resource-dependencies" || args[0] == "process" {
		bools = append(bools, "--all")
	}
	p, err := parseCLIOptions(args[1+positionals:], values, bools)
	if err != nil {
		return p, err
	}
	if p.Flags["--all"] && p.Values["--project"] != "" {
		return p, fmt.Errorf("--project and --all are mutually exclusive")
	}
	if args[0] == "process" && !p.Flags["--all"] {
		return p, fmt.Errorf("review process requires --all; dependency invalidations are store-wide")
	}
	if status := p.Values["--status"]; status != "" && status != "active" && status != "stale" {
		return p, fmt.Errorf("--status must be active or stale")
	}
	maxLimit := 500
	if args[0] == "list" {
		maxLimit = 20
	}
	_, err = p.integer("--limit", 10, 1, maxLimit)
	return p, err
}

type cliReadArguments struct {
	Project     string
	All, JSON   bool
	Scope, Mode string
	TokenBudget int
}

func cliFlagValue(args []string, index *int, key, inline string, hasInline bool) (string, error) {
	if !hasInline {
		if *index+1 >= len(args) || strings.HasPrefix(args[*index+1], "--") {
			return "", fmt.Errorf("%s requires a value", key)
		}
		*index++
		inline = args[*index]
	}
	inline = strings.TrimSpace(inline)
	if inline == "" {
		return "", fmt.Errorf("%s requires a value", key)
	}
	return inline, nil
}

func parseCLIReadArguments(args []string, context bool) (cliReadArguments, error) {
	p := cliReadArguments{Mode: store.ContextModeIndex}
	for i := 0; i < len(args); i++ {
		key, val, inline := strings.Cut(args[i], "=")
		switch key {
		case "--all":
			if inline {
				return p, fmt.Errorf("--all takes no value")
			}
			p.All = true
		case "--json":
			if inline || context {
				return p, fmt.Errorf("unsupported option %q", args[i])
			}
			p.JSON = true
		case "--project", "--scope", "--mode", "--token-budget":
			if key != "--project" && !context {
				return p, fmt.Errorf("unknown stats option %q", key)
			}
			v, err := cliFlagValue(args, &i, key, val, inline)
			if err != nil {
				return p, err
			}
			switch key {
			case "--project":
				if p.Project != "" {
					return p, fmt.Errorf("project specified more than once")
				}
				p.Project = v
			case "--scope":
				var err error
				p.Scope, err = store.NormalizeReadScope(v)
				if err != nil {
					return p, err
				}
			case "--mode":
				if v != "index" && v != "l0" && v != "l1" && v != "full" {
					return p, fmt.Errorf("invalid context mode")
				}
				p.Mode = v
			case "--token-budget":
				n, err := strconv.Atoi(v)
				if err != nil || n < 1 || n > 32000 {
					return p, fmt.Errorf("--token-budget must be 1..32000")
				}
				p.TokenBudget = n
			}
		default:
			if !context || strings.HasPrefix(key, "-") || p.Project != "" {
				return p, fmt.Errorf("unknown argument %q", args[i])
			}
			p.Project = args[i]
		}
	}
	if p.All && p.Project != "" {
		return p, fmt.Errorf("--project and --all are mutually exclusive")
	}
	if p.Mode == "full" && p.TokenBudget > 0 {
		return p, fmt.Errorf("full context does not support --token-budget")
	}
	return p, nil
}

func resolveCLIWriteOwner(s *store.Store, owner string) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	detected := project.DetectExplicitWithRegistry(cwd, s)
	if detected.Error != nil {
		return "", detected.Error
	}
	if detected.Project == "" || (detected.ProjectID == "" && detected.Source != project.SourceConfig) {
		return "", fmt.Errorf("unbound cwd is not write authority; bind the target project or declare matching local project config")
	}
	selected, err := s.ResolveExistingProject(detected.Project)
	if err != nil {
		return "", err
	}
	canonical, err := s.ResolveExistingProject(owner)
	if err != nil {
		return "", err
	}
	if selected != canonical {
		return "", fmt.Errorf("target project %q differs from current trusted project %q", canonical, selected)
	}
	if project.IsProjectID(canonical) && detected.ProjectID != canonical {
		return "", fmt.Errorf("registered project write requires a matching bound directory; a read selection or config alias cannot grant it")
	}
	return detected.Path, nil
}

func requireCLIObservationWrite(s *store.Store, id int64) (string, error) {
	owner, err := s.ObservationProjectIncludingDeleted(id)
	if err != nil {
		return "", fmt.Errorf("observation %d not found: %w", id, err)
	}
	return resolveCLIWriteOwner(s, owner)
}

type cliSaveScope struct{ Project, Directory, SessionID string }

// Only reject DB-free obvious ambiguity here. A registry marker is deliberately
// not accepted as identity until resolveCLISaveScope verifies it against SQLite.
func preflightCLISaveScope(directory, explicit string) error {
	detected := project.DetectExplicitWithRegistry(directory, nil)
	if detected.Source == project.SourceRegistered {
		return nil
	}
	if detected.Error != nil {
		return detected.Error
	}
	selected := explicit
	if selected == "" && detected.Source == project.SourceConfig {
		selected = detected.Project
	}
	if project.IsProjectID(selected) {
		return fmt.Errorf("registered project write requires a matching bound directory; a config alias or historical read selection cannot grant it")
	}
	if explicit == "" && (detected.Source != project.SourceConfig || detected.Project == "") {
		return fmt.Errorf("save requires an explicit project: bind a registered root, declare local project config, or pass --project PROJECT; an unbound cwd never creates an empty project bucket")
	}
	return nil
}

func resolveCLISaveScope(s *store.Store, directory, explicit string) (cliSaveScope, error) {
	detected := project.DetectExplicitWithRegistry(directory, s)
	if detected.Error != nil {
		return cliSaveScope{}, detected.Error
	}
	if detected.ProjectID != "" {
		if explicit != "" && explicit != detected.ProjectID {
			return cliSaveScope{}, fmt.Errorf("explicit project differs from registered directory binding")
		}
		return cliSaveScope{Project: detected.ProjectID, Directory: detected.Path, SessionID: "manual-save-" + detected.BindingID}, nil
	}
	selected := explicit
	if selected == "" && detected.Source == project.SourceConfig {
		selected = detected.Project
		directory = detected.Path
	}
	if selected == "" {
		return cliSaveScope{}, fmt.Errorf("save requires an explicit project; unbound cwd is not write authority")
	}
	if project.IsProjectID(selected) {
		return cliSaveScope{}, fmt.Errorf("registered project write requires a matching bound directory; a config alias or historical read selection cannot grant it")
	}
	return cliSaveScope{Project: selected, Directory: directory, SessionID: "manual-save-" + selected}, nil
}
