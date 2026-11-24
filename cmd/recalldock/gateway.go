package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/mark3labs/mcp-go/server"
	"recalldock.local/recalldock/internal/gateway"
	"recalldock.local/recalldock/internal/memorypolicy"
	"recalldock.local/recalldock/internal/opsstore"
)

func gatewayRegistryPath(explicit string) (string, error) {
	path := explicit
	if path == "" {
		path = os.Getenv("RECALLDOCK_GATEWAY_REGISTRY")
	}
	if path == "" {
		base, err := opsstore.DefaultBaseDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(base, "gateway.json")
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("absolute gateway registry path required: %q", path)
	}
	return filepath.Clean(path), nil
}

func cmdGateway(args []string) {
	if err := runGateway(args); err != nil {
		fatal(err)
	}
}
func gatewayUsage() {
	fmt.Fprintln(os.Stderr, `usage: recalldock gateway <subcommand> [options]

Subcommands:
  serve [--registry PATH] [--host HOST] [--host-session ID] [--trust agent|strict]
  list [--registry PATH] [--trust agent|strict]
  scan [--registry PATH] [--home PATH] [--json]
  check [--registry PATH]
  doctor [--registry PATH]
  add --server ID (--input ABS_JSON | --source-file ABS_PATH --keys k1,k2 [--format json|toml|yaml]) [--host HOST] [--description TEXT] [--enable] [--stateful] [--registry PATH]
  discover --server ID [--registry PATH]
  grant --server ID --tool TOOL --root PATH [--read-only | --arguments ABS_JSON --session ID --expires RFC3339] [--registry PATH]
  revoke --server ID --tool TOOL [--registry PATH]
  enable|disable|remove --server ID [--registry PATH]`)
}

func runGateway(args []string) (err error) {
	if len(args) == 0 {
		return errors.New("gateway requires serve, list, scan, check, doctor, add, discover, grant, revoke, enable, disable or remove")
	}
	sub := args[0]
	if sub == "help" || sub == "--help" || sub == "-h" {
		gatewayUsage()
		return nil
	}
	valid := map[string]string{
		"serve":    "--registry --host --host-session --trust",
		"list":     "--registry --trust",
		"scan":     "--registry --home --json",
		"check":    "--registry",
		"doctor":   "--registry",
		"add":      "--registry --server --input --source-file --format --keys --description --host --enable --stateful",
		"discover": "--registry --server",
		"grant":    "--registry --server --tool --root --read-only --recursive --arguments --session --expires",
		"revoke":   "--registry --server --tool",
		"enable":   "--registry --server",
		"disable":  "--registry --server",
		"remove":   "--registry --server",
	}
	allowed, ok := valid[sub]
	if !ok {
		return errors.New("unknown gateway command")
	}
	var valueFlags, boolFlags []string
	for _, key := range strings.Fields(allowed) {
		switch key {
		case "--read-only", "--recursive", "--enable", "--stateful", "--json":
			boolFlags = append(boolFlags, key)
		default:
			valueFlags = append(valueFlags, key)
		}
	}
	options, err := parseCLIOptions(args[1:], valueFlags, boolFlags)
	if err != nil {
		return err
	}
	values, flags := options.Values, options.Flags
	host, hostSession, trust := values["--host"], values["--host-session"], values["--trust"]
	if value, present := values["--host"]; present && value != "codex" && value != "claude-code" && value != "dsh" && value != "antigravity" {
		return errors.New("invalid gateway host")
	}
	if value, present := values["--trust"]; present && value != "agent" && value != "strict" {
		return errors.New("gateway --trust must be agent or strict")
	}
	if _, present := values["--host-session"]; present && (host == "" || !strings.HasPrefix(hostSession, host+"-") || memorypolicy.ValidateSessionID(hostSession) != nil) {
		return errors.New("gateway --host-session requires --host and a real registered host session ID")
	}
	if sub == "list" && trust == "" {
		trust = "agent"
	}
	registry, err := gatewayRegistryPath(values["--registry"])
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = fmt.Errorf("gateway (registry: %s): %w", registry, err)
		}
	}()
	reg, err := gateway.ReadRegistry(registry)
	if err != nil {
		return err
	}
	if sub == "scan" {
		homeVal, homeExplicit := values["--home"]
		items, scanErr := scanHostMCPConfigs(reg, registry, homeVal, homeExplicit)
		if scanErr != nil {
			return scanErr
		}
		return json.NewEncoder(os.Stdout).Encode(items)
	}
	if sub == "check" {
		type item struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		items := []item{}
		failed := false
		for id, s := range reg.Servers {
			status := "configuration_resolved"
			if _, _, err := s.Source.Read(); err != nil {
				status = err.Error()
				failed = true
			}
			items = append(items, item{id, status})
		}
		if err := json.NewEncoder(os.Stdout).Encode(items); err != nil {
			return err
		}
		if failed {
			return errors.New("some original configurations are unresolved; no backend was started")
		}
		return nil
	}
	if sub == "list" {
		type item struct {
			ID           string `json:"id"`
			Host         string `json:"host,omitempty"`
			Description  string `json:"description,omitempty"`
			SourcePath   string `json:"source_path"`
			Stateful     bool   `json:"stateful"`
			Enabled      bool   `json:"enabled"`
			StartAllowed bool   `json:"start_allowed"`
			Trust        string `json:"trust"`
			Grants       int    `json:"grants"`
		}
		ids := make([]string, 0, len(reg.Servers))
		for id := range reg.Servers {
			ids = append(ids, id)
		}
		sortStringsSlice(ids)
		items := []item{}
		for _, id := range ids {
			s := reg.Servers[id]
			items = append(items, item{
				ID:           id,
				Host:         s.Host,
				Description:  s.Description,
				SourcePath:   s.Source.Path,
				Stateful:     s.Stateful,
				Enabled:      s.Enabled,
				StartAllowed: s.StartAllowed,
				Trust:        trust,
				Grants:       len(s.Grants),
			})
		}
		return json.NewEncoder(os.Stdout).Encode(items)
	}
	id := values["--server"]
	if sub != "serve" && sub != "doctor" && id == "" {
		return errors.New("explicit --server required")
	}
	var definition gateway.Server
	if sub == "add" {
		var addErr error
		definition, addErr = parseGatewayAddDefinition(values, flags)
		if addErr != nil {
			return addErr
		}
		candidate := gateway.Registry{Version: 1, Servers: map[string]gateway.Server{id: definition}}
		if err := gateway.ValidateRegistry(candidate); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(registry), 0700); err != nil {
			return err
		}
	}
	dir, err := opsstore.DataDir()
	if err != nil {
		return err
	}
	if hostSession != "" {
		registered, err := opsstore.Open(dir, false)
		if err != nil {
			return fmt.Errorf("registered host session required: %w", err)
		}
		err = registered.ValidateHostSession(hostSession, host)
		closeErr := registered.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	// A bad subcommand/input never creates a database. list/help remain read-only.
	ops, err := opsstore.Open(dir, true)
	if err != nil {
		return err
	}
	defer ops.Close()
	pool := gateway.NewPool(registry, ops)
	defer pool.Close()
	pool.Host, pool.HostSessionID = host, hostSession
	if trust != "" {
		pool.Trust = trust
	}
	switch sub {
	case "doctor":
		stale, err := pool.StaleGrants()
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"stale_grants": stale, "count": len(stale)})
	case "serve":
		if stale, staleErr := pool.StaleGrants(); staleErr == nil {
			for _, grant := range stale {
				fmt.Fprintf(os.Stderr, "recalldock gateway: stale grant %s/%s (grant=%s current=%s read_only=%v); run gateway discover, review the new definition and explicitly re-grant in strict mode\n", grant.Server, grant.Tool, shortHash(grant.GrantHash), shortHash(grant.CurrentHash), grant.ReadOnly)
			}
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		return server.NewStdioServer(gateway.NewServer(pool)).Listen(ctx, os.Stdin, os.Stdout)
	case "add":
		return pool.Update(func(r *gateway.Registry) error {
			if _, ok := r.Servers[id]; ok {
				return errors.New("server already registered; disable/remove before replacing")
			}
			r.Servers[id] = definition
			return nil
		})
	case "discover":
		_, records, err := pool.Discover(id)
		if err != nil {
			return err
		}
		type item struct {
			Name       string `json:"name"`
			SchemaHash string `json:"schema_hash"`
		}
		items := []item{}
		for _, r := range records {
			items = append(items, item{r.Name, r.SchemaHash})
		}
		return json.NewEncoder(os.Stdout).Encode(items)
	case "grant":
		var arguments any
		if values["--arguments"] != "" {
			if err := readGatewayJSON(values["--arguments"], &arguments); err != nil {
				return err
			}
		}
		grant, err := pool.Grant(id, gateway.GrantOptions{Tool: values["--tool"], ReadOnly: flags["--read-only"], Recursive: flags["--recursive"], Roots: []string{values["--root"]}, Arguments: arguments, Session: values["--session"], ExpiresAt: values["--expires"]})
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(grant)
	case "revoke":
		if values["--tool"] == "" {
			return errors.New("explicit --tool required")
		}
		return pool.Revoke(id, values["--tool"])
	case "enable", "disable":
		return pool.Update(func(r *gateway.Registry) error {
			s, ok := r.Servers[id]
			if !ok {
				return errors.New("unknown server")
			}
			s.Enabled = sub == "enable"
			r.Servers[id] = s
			return nil
		})
	case "remove":
		return pool.Remove(id)
	}
	return nil
}

func readGatewayJSON(path string, value any) error {
	if !filepath.IsAbs(path) {
		return errors.New("absolute JSON input file required")
	}
	f, err := os.Open(path)
	if err != nil {
		return errors.New("cannot read selected JSON input")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > gateway.ConfigLimit {
		return errors.New("JSON input must be bounded regular file")
	}
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		return fmt.Errorf("invalid JSON input")
	}
	return nil
}

func shortHash(value string) string {
	if len(value) <= 12 {
		return value
	}
	return value[:12] + "…"
}
