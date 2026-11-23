package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"recalldock.local/recalldock/internal/gateway"
	"recalldock.local/recalldock/internal/opsstore"
)

type scannedGatewayServer struct {
	Host         string   `json:"host"`
	Name         string   `json:"name"`
	SourcePath   string   `json:"source_path"`
	SourceFormat string   `json:"source_format"`
	SourceKeys   []string `json:"source_keys"`
	Transport    string   `json:"transport"`
	Mode         string   `json:"mode"`
}

func sortStringsSlice(s []string) {
	sort.Strings(s)
}

func parseGatewayAddDefinition(values map[string]string, flags map[string]bool) (gateway.Server, error) {
	var def gateway.Server
	inputPath, hasInput := values["--input"]
	sourceFile, hasSource := values["--source-file"]
	_, hasFormat := values["--format"]
	keysRaw, hasKeys := values["--keys"]
	if hasInput && (hasSource || hasFormat || hasKeys || flags["--enable"] || flags["--stateful"] || values["--description"] != "") {
		return def, errors.New("use either --input or direct --source-file/--keys flags")
	}
	if hasInput {
		if err := readGatewayJSON(inputPath, &def); err != nil {
			return def, err
		}
		if len(def.Grants) > 0 || def.ConfigHash != "" {
			return def, errors.New("add cannot import grants; use explicit grant after discovery")
		}
		return def, nil
	}
	if !hasSource || strings.TrimSpace(sourceFile) == "" {
		return def, errors.New("add requires --input ABS_JSON or --source-file ABS_PATH --keys k1,k2")
	}
	if !filepath.IsAbs(sourceFile) {
		return def, errors.New("add --source-file must be an absolute path")
	}
	format := strings.ToLower(strings.TrimSpace(values["--format"]))
	if format == "" {
		switch strings.ToLower(filepath.Ext(sourceFile)) {
		case ".json":
			format = "json"
		case ".toml":
			format = "toml"
		case ".yaml", ".yml":
			format = "yaml"
		default:
			return def, errors.New("add requires --format json|toml|yaml")
		}
	}
	var keys []string
	for _, part := range strings.Split(keysRaw, ",") {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			keys = append(keys, trimmed)
		}
	}
	if len(keys) == 0 {
		return def, errors.New("add requires comma-separated --keys matching the MCP server object path")
	}
	enable := flags["--enable"]
	return gateway.Server{
		Host:        strings.TrimSpace(values["--host"]),
		Description: strings.TrimSpace(values["--description"]),
		Source: gateway.Source{
			Path:   filepath.Clean(sourceFile),
			Format: format,
			Keys:   keys,
		},
		Enabled:      enable,
		StartAllowed: enable,
		Stateful:     flags["--stateful"],
	}, nil
}

func sameScanPath(a, b string) bool {
	ca, cb := filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(ca, cb)
	}
	return ca == cb
}

func inferEntryTransport(entry map[string]any) string {
	for _, key := range []string{"type", "transport"} {
		if v, ok := entry[key].(string); ok {
			t := strings.ToLower(strings.TrimSpace(v))
			switch t {
			case "streamable-http", "streamable_http", "http":
				return "http"
			case "stdio", "sse":
				return t
			}
		}
	}
	if cmd, ok := entry["command"].(string); ok && strings.TrimSpace(cmd) != "" {
		return "stdio"
	}
	if u, ok := entry["url"].(string); ok && strings.TrimSpace(u) != "" {
		return "http"
	}
	return "unknown"
}

func hostEntryDisabled(entry map[string]any) bool {
	if enabled, ok := entry["enabled"].(bool); ok && !enabled {
		return true
	}
	if disabled, ok := entry["disabled"].(bool); ok && disabled {
		return true
	}
	return false
}

func readSafeConfigMap(path, format string) (map[string]any, bool) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > gateway.ConfigLimit {
		return nil, false
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, gateway.ConfigLimit+1))
	if err != nil || int64(len(body)) > gateway.ConfigLimit {
		return nil, false
	}
	var raw map[string]any
	switch format {
	case "json":
		if json.Unmarshal(body, &raw) != nil {
			return nil, false
		}
	case "toml":
		if toml.Unmarshal(body, &raw) != nil {
			return nil, false
		}
	default:
		return nil, false
	}
	return raw, true
}

func isOwnGatewayEntry(name string, entry map[string]any) bool {
	if name == "recalldock_gateway" {
		return true
	}
	if rawArgs, ok := entry["args"].([]any); ok && len(rawArgs) >= 2 {
		a0, _ := rawArgs[0].(string)
		a1, _ := rawArgs[1].(string)
		if a0 == "gateway" && a1 == "serve" {
			return true
		}
	}
	return false
}

func scanHostMCPConfigs(reg gateway.Registry, registryPath, homeOverride string, homeExplicit bool) ([]scannedGatewayServer, error) {
	home := strings.TrimSpace(homeOverride)
	if homeExplicit {
		if !filepath.IsAbs(home) || (runtime.GOOS == "windows" && strings.HasPrefix(filepath.Clean(home), `\\`)) {
			return nil, errors.New("scan --home must be an absolute local directory path")
		}
		home = filepath.Clean(home)
	} else {
		var err error
		home, err = os.UserHomeDir()
		if err != nil || !filepath.IsAbs(home) {
			return nil, errors.New("cannot resolve home directory; specify --home")
		}
		home = filepath.Clean(home)
	}

	type configTarget struct {
		host     string
		path     string
		format   string
		topKey   string
	}
	targets := []configTarget{
		{"codex", filepath.Join(home, ".codex", "config.toml"), "toml", "mcp_servers"},
		{"claude-code", filepath.Join(home, ".claude.json"), "json", "mcpServers"},
		{"claude-code", filepath.Join(home, ".recalldock", "claude-gateway.json"), "json", "mcpServers"},
		{"antigravity", filepath.Join(home, ".gemini", "antigravity-cli", "mcp_config.json"), "json", "mcpServers"},
	}
	regDirClaude := filepath.Join(filepath.Dir(registryPath), "claude-gateway.json")
	if !sameScanPath(regDirClaude, filepath.Join(home, ".recalldock", "claude-gateway.json")) {
		targets = append(targets, configTarget{"claude-code", regDirClaude, "json", "mcpServers"})
	}
	if !homeExplicit {
		if base, err := opsstore.DefaultBaseDir(); err == nil {
			cand := filepath.Join(base, "claude-gateway.json")
			dup := false
			for _, t := range targets {
				if sameScanPath(t.path, cand) {
					dup = true
					break
				}
			}
			if !dup {
				targets = append(targets, configTarget{"claude-code", cand, "json", "mcpServers"})
			}
		}
	}
	agPlugins, _ := filepath.Glob(filepath.Join(home, ".gemini", "config", "plugins", "*", "mcp_config.json"))
	sort.Strings(agPlugins)
	for _, p := range agPlugins {
		targets = append(targets, configTarget{"antigravity", p, "json", "mcpServers"})
	}

	matchedGatewayIDs := map[string]bool{}
	items := []scannedGatewayServer{}

	for _, target := range targets {
		raw, ok := readSafeConfigMap(target.path, target.format)
		if !ok {
			continue
		}
		table, ok := raw[target.topKey].(map[string]any)
		if !ok {
			continue
		}
		names := make([]string, 0, len(table))
		for name := range table {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			entry, ok := table[name].(map[string]any)
			if !ok || isOwnGatewayEntry(name, entry) {
				continue
			}
			keys := []string{target.topKey, name}
			inGateway := false
			for regID, srv := range reg.Servers {
				if sameScanPath(srv.Source.Path, target.path) && slices.Equal(srv.Source.Keys, keys) {
					matchedGatewayIDs[regID] = true
					if srv.Enabled {
						inGateway = true
					}
				}
			}
			hostActive := !hostEntryDisabled(entry)
			var mode string
			switch {
			case hostActive && inGateway:
				mode = "both"
			case !hostActive && inGateway:
				mode = "gateway_managed"
			case hostActive && !inGateway:
				mode = "host_direct"
			default:
				continue
			}
			items = append(items, scannedGatewayServer{
				Host:         target.host,
				Name:         name,
				SourcePath:   target.path,
				SourceFormat: target.format,
				SourceKeys:   keys,
				Transport:    inferEntryTransport(entry),
				Mode:         mode,
			})
		}
	}

	regIDs := make([]string, 0, len(reg.Servers))
	for id := range reg.Servers {
		regIDs = append(regIDs, id)
	}
	sort.Strings(regIDs)
	for _, id := range regIDs {
		if matchedGatewayIDs[id] {
			continue
		}
		srv := reg.Servers[id]
		transport := "unknown"
		if len(srv.Source.Keys) > 0 && (srv.Source.Format == "json" || srv.Source.Format == "toml") {
			if raw, ok := readSafeConfigMap(srv.Source.Path, srv.Source.Format); ok {
				var cur any = raw
				for _, k := range srv.Source.Keys {
					m, mok := cur.(map[string]any)
					if !mok {
						cur = nil
						break
					}
					cur = m[k]
				}
				if entry, eok := cur.(map[string]any); eok {
					transport = inferEntryTransport(entry)
				}
			}
		}
		items = append(items, scannedGatewayServer{
			Host:         srv.Host,
			Name:         id,
			SourcePath:   srv.Source.Path,
			SourceFormat: srv.Source.Format,
			SourceKeys:   srv.Source.Keys,
			Transport:    transport,
			Mode:         "gateway_managed",
		})
	}
	return items, nil
}
