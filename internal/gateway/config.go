package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"
)

var (
	ErrApproval    = errors.New("approval_required: an operator must approve this exact server/configuration/tool; model confirmation is not permission")
	ErrChanged     = errors.New("configuration_or_schema_changed: rediscover and obtain a new operator approval; do not retry the call")
	ErrUnavailable = errors.New("backend_unavailable: check the original configuration or authentication; no automatic retry")
	ErrLease       = errors.New("lease_not_available: it expired, was released, belongs to another gateway session, or was issued for a different server/root; retry without lease to acquire a fresh one")
	ErrUnknown     = errors.New("outcome_unknown: the request may have executed; verify external state before any retry")
	serverName     = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,79}$`)
	envName        = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

const ConfigLimit = 2 << 20

type Source struct {
	Path       string   `json:"path"`
	Format     string   `json:"format"`
	Keys       []string `json:"keys"`
	PluginRoot string   `json:"plugin_root,omitempty"`
}

type Grant struct {
	ID         string   `json:"id"`
	Tool       string   `json:"tool"`
	SchemaHash string   `json:"schema_hash"`
	Roots      []string `json:"roots"`
	// Recursive makes this grant also cover descendants of each root. It is
	// additive and defaults to false, so a grant recorded before this field
	// existed keeps its exact-match meaning: widening existing grants
	// automatically would extend access to subdirectories the operator never
	// named, and the old record carries no evidence of that consent.
	// Operators opt in per grant with `gateway grant --recursive`.
	Recursive     bool   `json:"recursive,omitempty"`
	ArgumentsHash string `json:"arguments_hash,omitempty"`
	Session       string `json:"session,omitempty"`
	ExpiresAt     string `json:"expires_at,omitempty"`
	ReadOnly      bool   `json:"read_only"`
}

type Server struct {
	Host           string  `json:"host,omitempty"`
	Description    string  `json:"description"`
	Source         Source  `json:"source"`
	Enabled        bool    `json:"enabled"`
	StartAllowed   bool    `json:"start_allowed"`
	ConfigHash     string  `json:"config_hash,omitempty"`
	Stateful       bool    `json:"stateful"`
	TimeoutSeconds int     `json:"timeout_seconds,omitempty"`
	Grants         []Grant `json:"grants,omitempty"`
}

type Registry struct {
	Version int               `json:"version"`
	Servers map[string]Server `json:"servers"`
}

type Runtime struct {
	Type              string            `json:"type"`
	Transport         string            `json:"transport,omitempty"`
	Command           string            `json:"command"`
	Args              []string          `json:"args"`
	CWD               string            `json:"cwd"`
	Env               map[string]string `json:"env"`
	EnvVars           []string          `json:"env_vars"`
	URL               string            `json:"url"`
	Headers           map[string]string `json:"headers"`
	HTTPHeaders       map[string]string `json:"http_headers"`
	BearerTokenEnvVar string            `json:"bearer_token_env_var"`
}

func Fingerprint(v any) string {
	body, _ := json.Marshal(v)
	hash := sha256.Sum256(body)
	return hex.EncodeToString(hash[:])
}

func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("configuration must be a bounded regular file")
	}
	body, err := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(body)) > limit {
		return nil, errors.New("configuration exceeds size limit")
	}
	return body, err
}

func ReadRegistry(path string) (Registry, error) {
	var cfg Registry
	if !filepath.IsAbs(path) {
		return cfg, errors.New("absolute operator registry path required")
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return Registry{Version: 1, Servers: map[string]Server{}}, nil
	}
	if err != nil {
		return cfg, err
	}
	if !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0022 != 0) {
		return cfg, errors.New("operator registry cannot be a symlink or group/world-writable")
	}
	body, err := readBounded(path, ConfigLimit)
	if err != nil {
		return cfg, err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, errors.New("invalid operator registry")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF || cfg.Version != 1 || len(cfg.Servers) > 200 {
		return cfg, errors.New("unsupported or oversized operator registry")
	}
	if err := ValidateRegistry(cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func ValidateRegistry(cfg Registry) error {
	if cfg.Version != 1 || len(cfg.Servers) > 200 {
		return errors.New("unsupported or oversized operator registry")
	}
	for id, server := range cfg.Servers {
		if !serverName.MatchString(id) || len(server.Description) > 600 || len(server.Grants) > 512 || server.TimeoutSeconds < 0 || server.TimeoutSeconds > 600 {
			return errors.New("invalid server metadata")
		}
		if !filepath.IsAbs(server.Source.Path) || len(server.Source.Keys) == 0 || len(server.Source.Keys) > 12 {
			return errors.New("invalid original configuration reference")
		}
		if server.Source.Format != "json" && server.Source.Format != "toml" && server.Source.Format != "yaml" {
			return errors.New("unsupported source format")
		}
		if server.Host != "" && server.Host != "claude-code" && server.Host != "codex" && server.Host != "dsh" && server.Host != "antigravity" {
			return errors.New("invalid configured host")
		}
		ids := map[string]bool{}
		for _, g := range server.Grants {
			if !serverName.MatchString(g.ID) || ids[g.ID] || g.Tool == "" || strings.Contains(g.Tool, "*") || len(g.SchemaHash) != 64 || len(g.Roots) == 0 {
				return errors.New("invalid exact tool grant")
			}
			ids[g.ID] = true
			if !g.ReadOnly && (len(g.ArgumentsHash) != 64 || g.Session == "" || g.ExpiresAt == "" || slices.Contains(g.Roots, "*")) {
				return errors.New("side-effecting grants require exact arguments, root, session and expiry")
			}
			for _, root := range g.Roots {
				if root != "*" && !filepath.IsAbs(root) {
					return errors.New("grant root must be absolute")
				}
			}
			if g.ExpiresAt != "" {
				if _, err := time.Parse(time.RFC3339, g.ExpiresAt); err != nil {
					return errors.New("invalid grant expiry")
				}
			}
		}
	}
	return nil
}

func (s Source) Read() (Runtime, string, error) {
	var runtime Runtime
	body, err := readBounded(s.Path, ConfigLimit)
	if err != nil {
		return runtime, "", ErrUnavailable
	}
	var raw any
	switch s.Format {
	case "json":
		err = json.Unmarshal(body, &raw)
	case "toml":
		err = toml.Unmarshal(body, &raw)
	case "yaml":
		err = yaml.Unmarshal(body, &raw)
	default:
		return runtime, "", errors.New("unsupported source format")
	}
	if err != nil {
		return runtime, "", errors.New("cannot parse original configuration")
	}
	var selected any = raw
	for _, key := range s.Keys {
		switch m := selected.(type) {
		case map[string]any:
			var ok bool
			selected, ok = m[key]
			if !ok {
				return runtime, "", ErrUnavailable
			}
		case []any:
			index, err := strconv.Atoi(key)
			if err != nil || index < 0 || index >= len(m) {
				return runtime, "", ErrUnavailable
			}
			selected = m[index]
		default:
			return runtime, "", ErrUnavailable
		}
	}
	entry, ok := selected.(map[string]any)
	if !ok {
		return runtime, "", ErrUnavailable
	}
	if _, ok := entry["oauth"]; ok {
		return runtime, "", errors.New("interactive_auth_required: preserve host OAuth and use its native connector until supported")
	}
	for _, key := range []string{"auth", "env_http_headers", "http_headers_helper", "experimental_environment"} {
		if _, ok := entry[key]; ok {
			return runtime, "", errors.New("unsupported_auth_or_execution: use the original host connector; no silent fallback")
		}
	}
	// Host autostart state is not a gateway grant. It changes during migration.
	delete(entry, "enabled")
	delete(entry, "disabled")
	encoded, err := json.Marshal(entry)
	if err != nil || json.Unmarshal(encoded, &runtime) != nil {
		return runtime, "", errors.New("unsupported original server configuration")
	}
	if runtime.Type == "" {
		runtime.Type = runtime.Transport
	}
	if runtime.Type == "" {
		if runtime.Command != "" {
			runtime.Type = "stdio"
		} else {
			runtime.Type = "http"
		}
	}
	if runtime.Type == "streamable-http" || runtime.Type == "streamable_http" {
		runtime.Type = "http"
	}
	if runtime.Type != "stdio" && runtime.Type != "http" && runtime.Type != "sse" {
		return runtime, "", errors.New("unsupported transport; no silent fallback")
	}
	if runtime.Type == "stdio" && runtime.Command == "" {
		return runtime, "", ErrUnavailable
	}
	if runtime.Type != "stdio" && runtime.URL == "" {
		return runtime, "", ErrUnavailable
	}
	var missing bool
	variable := regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)
	expand := func(value string) string {
		return variable.ReplaceAllStringFunc(value, func(expression string) string {
			parts := variable.FindStringSubmatch(expression)
			key := parts[1]
			if key == "CLAUDE_PLUGIN_ROOT" || key == "CODEX_PLUGIN_ROOT" {
				if s.PluginRoot == "" {
					missing = true
				}
				return s.PluginRoot
			}
			v, ok := os.LookupEnv(key)
			if (!ok || v == "") && strings.Contains(expression, ":-") {
				return parts[2]
			}
			if !ok {
				missing = true
			}
			return v
		})
	}
	runtime.Command = expand(runtime.Command)
	for i := range runtime.Args {
		runtime.Args[i] = expand(runtime.Args[i])
	}
	runtime.CWD = expand(runtime.CWD)
	runtime.URL = expand(runtime.URL)
	for key, value := range runtime.Env {
		if !envName.MatchString(key) {
			return runtime, "", ErrUnavailable
		}
		runtime.Env[key] = expand(value)
	}
	if runtime.Headers == nil {
		runtime.Headers = map[string]string{}
	}
	for key, value := range runtime.HTTPHeaders {
		runtime.Headers[key] = value
	}
	for key, value := range runtime.Headers {
		runtime.Headers[key] = expand(value)
	}
	if runtime.BearerTokenEnvVar != "" {
		value, ok := os.LookupEnv(runtime.BearerTokenEnvVar)
		if !ok || !envName.MatchString(runtime.BearerTokenEnvVar) {
			missing = true
		} else {
			runtime.Headers["Authorization"] = "Bearer " + value
		}
	}
	if missing {
		return Runtime{}, "", errors.New("required_configuration_missing: set credentials in the original host or environment; never send secrets as tool arguments")
	}
	if runtime.Type == "stdio" && runtime.Command == "" || runtime.Type != "stdio" && runtime.URL == "" {
		return Runtime{}, "", ErrUnavailable
	}
	// Bind approvals to resolved execution semantics, not template strings.
	// Declared env may change code loading (e.g. NODE_OPTIONS), so do not guess
	// which values are credentials. Values are hashed here, never persisted.
	forwarded := map[string]string{}
	names := slices.Clone(runtime.EnvVars)
	slices.Sort(names)
	names = slices.Compact(names)
	for _, key := range names {
		if !envName.MatchString(key) {
			return Runtime{}, "", errors.New("invalid declared environment name")
		}
		if value, ok := os.LookupEnv(key); ok {
			forwarded[key] = value
		}
	}
	headers := make(map[string]string, len(runtime.Headers))
	for key, value := range runtime.Headers {
		// A routing/tenant header can redirect a call without changing URL or
		// schema. Only the explicit Authorization credential value is rotatable.
		if strings.EqualFold(key, "Authorization") {
			value = "<authorization>"
		}
		headers[key] = value
	}
	searchPath := ""
	if runtime.Type == "stdio" && !filepath.IsAbs(runtime.Command) {
		searchPath = os.Getenv("PATH")
		if stdlibRuntimeGOOS() == "windows" {
			for k, path := range runtime.Env {
				if strings.EqualFold(k, "PATH") {
					searchPath = path
					break
				}
			}
		} else if path, ok := runtime.Env["PATH"]; ok {
			searchPath = path
		}
	}
	hash := Fingerprint(struct {
		Version           int
		Source            Source
		Type, Command     string
		Args              []string
		URL, CWD          string
		Env               map[string]string
		EnvVars           []string
		Forwarded         map[string]string
		SearchPath        string
		Headers           map[string]string
		BearerTokenEnvVar string
	}{Version: 2, Source: s, Type: runtime.Type, Command: runtime.Command, Args: runtime.Args, URL: runtime.URL, CWD: runtime.CWD,
		Env: runtime.Env, EnvVars: names, Forwarded: forwarded, SearchPath: searchPath, Headers: headers, BearerTokenEnvVar: runtime.BearerTokenEnvVar})
	return runtime, hash, nil
}

func stdlibRuntimeGOOS() string { return runtime.GOOS }

func (r Runtime) Environment() ([]string, error) {
	values := map[string]string{}
	baseVars := []string{"PATH", "HOME", "LANG", "LC_ALL", "TMPDIR", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "DISPLAY", "WAYLAND_DISPLAY", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"}
	if runtime.GOOS == "windows" {
		baseVars = append(baseVars, "PATHEXT", "USERPROFILE", "LOCALAPPDATA", "APPDATA", "SYSTEMROOT", "WINDIR", "COMSPEC", "TEMP", "TMP", "HOMEDRIVE", "HOMEPATH")
	}
	for _, key := range append(baseVars, r.EnvVars...) {
		if !envName.MatchString(key) {
			return nil, errors.New("invalid declared environment name")
		}
		if value, ok := os.LookupEnv(key); ok {
			if runtime.GOOS == "windows" {
				for existing := range values {
					if strings.EqualFold(existing, key) {
						delete(values, existing)
					}
				}
			}
			values[key] = value
		}
	}
	for key, value := range r.Env {
		if runtime.GOOS == "windows" {
			for existing := range values {
				if strings.EqualFold(existing, key) {
					delete(values, existing)
				}
			}
		}
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result, nil
}

func (s Server) Timeout() time.Duration {
	if s.TimeoutSeconds == 0 {
		return 120 * time.Second
	}
	return time.Duration(s.TimeoutSeconds) * time.Second
}

func (s Server) Allowed(tool, schemaHash, root, session string, arguments any) (Grant, bool) {
	for _, g := range s.Grants {
		if g.Tool != tool || g.SchemaHash != schemaHash || !g.coversRoot(root) {
			continue
		}
		if g.Session != "" && g.Session != session {
			continue
		}
		if g.ArgumentsHash != "" && g.ArgumentsHash != Fingerprint(arguments) {
			continue
		}
		if g.ExpiresAt != "" {
			until, err := time.Parse(time.RFC3339, g.ExpiresAt)
			if err != nil || !time.Now().Before(until) {
				continue
			}
		}
		return g, true
	}
	return Grant{}, false
}

// coversRoot reports whether g authorizes a call at root. Both paths are already
// canonical (absolute, symlink-resolved, cleaned) before this runs.
//
// Coverage is exact unless the grant opted into recursion. Recursive matching
// stops at a path-separator boundary, so a grant on "/work/proj" covers
// "/work/proj/sub" but never "/work/project-other"; a bare strings.HasPrefix
// would have matched that sibling.
func (g Grant) coversRoot(root string) bool {
	if root == "" {
		return false
	}
	for _, granted := range g.Roots {
		if granted == "*" || granted == root {
			return true
		}
		if g.Recursive && withinRoot(root, granted) {
			return true
		}
	}
	return false
}

// withinRoot reports whether path is a strict descendant of root, compared by
// whole path segments. Windows compares case-insensitively to match the
// canonicalPath normalization applied before this check.
func withinRoot(path, root string) bool {
	if root == "" || path == root {
		return false
	}
	cleanPath := filepath.Clean(path)
	cleanRoot := filepath.Clean(root)
	if runtime.GOOS == "windows" {
		cleanPath = strings.ToLower(cleanPath)
		cleanRoot = strings.ToLower(cleanRoot)
	}
	if runtime.GOOS != "windows" && !strings.HasPrefix(cleanRoot, string(filepath.Separator)) {
		// Relative roots are never valid grants; refuse rather than guess.
		return false
	}
	prefix := cleanRoot
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(cleanPath, prefix)
}
