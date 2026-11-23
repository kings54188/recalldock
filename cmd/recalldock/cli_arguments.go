package main

import (
	"fmt"
	"os"
	"strings"

	"recalldock.local/recalldock/internal/mcp"
	"recalldock.local/recalldock/internal/store"
)

func requiresExistingStore(args []string) bool {
	if len(args) == 0 {
		return true
	}
	switch args[0] {
	case "save", "import":
		return false
	case "projects":
		return len(args) < 2 || args[1] != "create"
	case "store":
		return len(args) < 2 || args[1] != "init"
	default:
		return true
	}
}

// extractRequiredMCPScope is opt-in for host adapters: an unbound working
// directory must not accidentally expose another project's memories.
func extractRequiredMCPScope(args []string) ([]string, bool, error) {
	filtered := make([]string, 0, len(args))
	required := false
	for _, arg := range args {
		if arg == "--require-project" {
			if required {
				return nil, false, fmt.Errorf("--require-project specified more than once")
			}
			required = true
			continue
		}
		filtered = append(filtered, arg)
	}
	return filtered, required, nil
}

func parseMCPArguments(args []string) (tools, project string, err error) {
	tools = "agent"
	project = strings.TrimSpace(os.Getenv("RECALLDOCK_PROJECT"))
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		key, value, inline := strings.Cut(args[i], "=")
		if key != "--tools" && key != "--project" {
			return "", "", fmt.Errorf("unknown mcp argument %q", key)
		}
		if seen[key] {
			return "", "", fmt.Errorf("duplicate mcp option %q", key)
		}
		seen[key] = true
		if !inline {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				return "", "", fmt.Errorf("%s requires a value", key)
			}
			i++
			value = args[i]
		}
		value = strings.TrimSpace(value)
		if value == "" {
			return "", "", fmt.Errorf("%s requires a value", key)
		}
		if key == "--tools" {
			tools = value
		} else {
			project = value
		}
	}
	if err := mcp.ValidateToolsSelection(tools); err != nil {
		return "", "", err
	}
	return tools, project, nil
}

// Validate before opening a database. A misspelled flag is never a query;
// use -- to search literal option-shaped words.
func validateSearchArguments(args []string) (bool, error) {
	options, _, err := parseSearchArguments(args)
	if err != nil {
		return false, err
	}
	var opts store.SearchOptions
	opts.Scope, opts.MatchMode = options.Values["--scope"], options.Values["--match-mode"]
	opts.Profile, opts.Strategy = options.Values["--profile"], options.Values["--strategy"]
	opts.Detail, opts.RootPath = options.Values["--detail"], options.Values["--root-path"]
	opts.Since, opts.Until = options.Values["--since"], options.Values["--until"]
	for _, spec := range []struct {
		key      string
		min, max int
	}{{"--limit", 1, 20}, {"--token-budget", 1, 32000}, {"--max-depth", 0, 64}} {
		if _, err := options.integer(spec.key, 0, spec.min, spec.max); err != nil {
			return false, err
		}
	}
	if err := store.NormalizeSearchOptions(&opts); err != nil {
		return false, err
	}
	return options.Flags["--all"], nil
}

func parseSearchArguments(args []string) (cliOptions, []string, error) {
	values := []string{"--type", "--project", "--limit", "--scope", "--match-mode", "--profile", "--strategy", "--detail", "--token-budget", "--root-path", "--max-depth", "--since", "--until"}
	bools := []string{"--all", "--trace", "--explain-empty", "--include-stale", "--json", "--raw-fts"}
	optionArgs, positionals, err := splitCLIPositionals(args, values)
	if err != nil {
		return cliOptions{}, nil, err
	}
	options, err := parseCLIOptions(optionArgs, values, bools)
	if err == nil && options.Flags["--all"] && options.Values["--project"] != "" {
		err = fmt.Errorf("--project and --all are mutually exclusive")
	}
	return options, positionals, err
}

func splitCLIPositionals(args, valueFlags []string) ([]string, []string, error) {
	values := map[string]bool{}
	for _, key := range valueFlags {
		values[key] = true
	}
	var options, positionals []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			positionals = append(positionals, args[i+1:]...)
			break
		}
		key, _, inline := strings.Cut(args[i], "=")
		if !strings.HasPrefix(key, "-") {
			positionals = append(positionals, args[i])
			continue
		}
		options = append(options, args[i])
		if values[key] && !inline {
			value, err := cliFlagValue(args, &i, key, "", false)
			if err != nil {
				return nil, nil, err
			}
			options = append(options, value)
		}
	}
	return options, positionals, nil
}

func validateSaveArguments(args []string) error {
	known := map[string]bool{"--type": true, "--project": true, "--scope": true, "--topic": true, "--memory-class": true, "--memory-path": true, "--abstract": true, "--overview": true}
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		key := args[i]
		if !known[key] {
			return fmt.Errorf("unknown save option %q", key)
		}
		if seen[key] {
			return fmt.Errorf("save option %s specified more than once", key)
		}
		seen[key] = true
		if _, err := cliFlagValue(args, &i, key, "", false); err != nil {
			return err
		}
	}
	return nil
}
