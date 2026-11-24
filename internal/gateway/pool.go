package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"recalldock.local/recalldock/internal/filelock"
	"recalldock.local/recalldock/internal/opsstore"
)

type Pool struct {
	RegistryPath  string
	Ops           *opsstore.Store
	IdleTTL       time.Duration
	Host          string // trusted launcher configuration, never a model argument
	HostSessionID string
	Trust         string // "agent" (local host delegate) or "strict" (exact grants)
	mu            sync.Mutex
	leases        map[string]*lease
}

type lease struct {
	mu                           sync.Mutex
	token, server, root, session string
	configHash                   string
	conn                         *connection
	refs                         int
	released                     bool
	timer                        *time.Timer
	timerEpoch                   uint64
}

type CallRequest struct {
	Server, Tool         string
	Arguments            any
	Root, Session, Lease string
	ExpectedSchemaHash   string
	Meta                 *mcp.Meta
	Notify               func(mcp.JSONRPCNotification)
}

func NewPool(registryPath string, ops *opsstore.Store) *Pool {
	return &Pool{RegistryPath: registryPath, Ops: ops, IdleTTL: 5 * time.Minute, Trust: "agent", leases: map[string]*lease{}}
}

func randomToken() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buf)
}

func canonicalPath(path string) string {
	if path == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		cleaned := filepath.Clean(resolved)
		if runtime.GOOS == "windows" && len(cleaned) >= 2 && cleaned[1] == ':' && cleaned[0] >= 'a' && cleaned[0] <= 'z' {
			cleaned = strings.ToUpper(cleaned[:1]) + cleaned[1:]
		}
		return cleaned
	}
	cleaned := filepath.Clean(path)
	if runtime.GOOS == "windows" && len(cleaned) >= 2 && cleaned[1] == ':' && cleaned[0] >= 'a' && cleaned[0] <= 'z' {
		cleaned = strings.ToUpper(cleaned[:1]) + cleaned[1:]
	}
	return cleaned
}

func (p *Pool) registry() (Registry, error) { return ReadRegistry(p.RegistryPath) }

func writeRegistry(path string, reg Registry) error {
	if !filepath.IsAbs(path) {
		return errors.New("registry path must be absolute")
	}
	body, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".gateway-*.json")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := temp.Chmod(0600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(body); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (p *Pool) Update(fn func(*Registry) error) error {
	// An OS advisory lock is released by the kernel when the holder dies, so a
	// crashed gateway cannot wedge later registry writes. The lock file stays on
	// disk; its existence never means "locked".
	lock, err := filelock.Acquire(p.RegistryPath + ".lock")
	if err != nil {
		if errors.Is(err, filelock.ErrUnavailable) {
			return errors.New("registry lock unavailable; another recalldock process is updating it")
		}
		return err
	}
	defer lock.Release()
	reg, err := p.registry()
	if err != nil {
		return err
	}
	if reg.Servers == nil {
		reg.Servers = map[string]Server{}
	}
	if err := fn(&reg); err != nil {
		return err
	}
	if err := ValidateRegistry(reg); err != nil {
		return err
	}
	return writeRegistry(p.RegistryPath, reg)
}

func backendCWD(runtime Runtime) string {
	if runtime.CWD != "" {
		return runtime.CWD
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return cwd
}

func (p *Pool) discoverWith(serverID string) (Server, Runtime, string, *connection, []opsstore.ToolRecord, error) {
	reg, err := p.registry()
	if err != nil {
		return Server{}, Runtime{}, "", nil, nil, err
	}
	server, ok := reg.Servers[serverID]
	if !ok {
		return Server{}, Runtime{}, "", nil, nil, fmt.Errorf("unknown backend %q", serverID)
	}
	if p.Host != "" && server.Host != "" && p.Host != server.Host {
		return Server{}, Runtime{}, "", nil, nil, ErrApproval
	}
	if !server.Enabled || !server.StartAllowed {
		return Server{}, Runtime{}, "", nil, nil, ErrApproval
	}
	runtime, hash, err := server.Source.Read()
	if err != nil {
		return Server{}, Runtime{}, "", nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), server.Timeout())
	defer cancel()
	conn, err := connect(ctx, runtime, backendCWD(runtime))
	if err != nil {
		return Server{}, Runtime{}, "", nil, nil, err
	}
	tools, err := conn.Tools(ctx)
	if err != nil {
		conn.Close()
		return Server{}, Runtime{}, "", nil, nil, err
	}
	records := make([]opsstore.ToolRecord, 0, len(tools))
	for _, tool := range tools {
		schema, err := json.Marshal(tool)
		if err != nil {
			conn.Close()
			return Server{}, Runtime{}, "", nil, nil, errors.New("tool definition is not encodable")
		}
		records = append(records, opsstore.ToolRecord{Server: serverID, Name: tool.Name, ConfigHash: hash, SchemaHash: Fingerprint(tool), Summary: clipText(tool.Description, 300), SchemaJSON: string(schema)})
	}
	return server, runtime, hash, conn, records, nil
}

func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && (s[n]&0xc0) == 0x80 {
		n--
	}
	return s[:n] + "…"
}

// Discover connects once to an operator-approved backend, caches bounded tool
// metadata and closes the connection. Discovery never starts all servers.
func (p *Pool) Discover(serverID string) (Server, []opsstore.ToolRecord, error) {
	server, _, hash, conn, records, err := p.discoverWith(serverID)
	if err != nil {
		return server, nil, err
	}
	defer conn.Close()
	if err := p.Ops.SaveCatalog(serverID, hash, records); err != nil {
		return server, nil, err
	}
	// Keep the registry identity aligned with the catalog this discovery just
	// created. Without it a successful discover could still leave Call with a
	// stale config_hash and reject every agent-mode call.
	if err := p.Update(func(reg *Registry) error {
		current := reg.Servers[serverID]
		if current.ConfigHash != hash {
			current.ConfigHash = hash
			current.Grants = nil
		}
		reg.Servers[serverID] = current
		return nil
	}); err != nil {
		return server, nil, err
	}
	return server, records, nil
}

type GrantOptions struct {
	Tool      string
	ReadOnly  bool
	Roots     []string
	Arguments any
	Session   string
	ExpiresAt string
}

func (p *Pool) Grant(serverID string, opts GrantOptions) (Grant, error) {
	reg, err := p.registry()
	if err != nil {
		return Grant{}, err
	}
	if _, ok := reg.Servers[serverID]; !ok {
		return Grant{}, fmt.Errorf("unknown backend %q", serverID)
	}
	catalog, err := p.Ops.LoadCatalog(serverID)
	if err != nil {
		return Grant{}, err
	}
	var record *opsstore.ToolRecord
	for i := range catalog {
		if catalog[i].Name == opts.Tool {
			record = &catalog[i]
			break
		}
	}
	if record == nil {
		return Grant{}, fmt.Errorf("tool %q not found in cached catalog; discover first", opts.Tool)
	}
	server := reg.Servers[serverID]
	_, currentHash, err := server.Source.Read()
	if err != nil {
		return Grant{}, err
	}
	if currentHash != record.ConfigHash {
		return Grant{}, ErrChanged
	}
	if len(opts.Roots) == 0 {
		return Grant{}, errors.New("explicit --root is required for every grant")
	}
	roots := make([]string, 0, len(opts.Roots))
	for _, root := range opts.Roots {
		if root == "*" {
			if !opts.ReadOnly {
				return Grant{}, errors.New("wildcard roots are allowed for read-only grants only")
			}
			roots = append(roots, root)
			continue
		}
		if !filepath.IsAbs(root) {
			return Grant{}, errors.New("grant roots must be absolute")
		}
		roots = append(roots, canonicalPath(root))
	}
	sort.Strings(roots)
	grant := Grant{ID: "g-" + strings.TrimPrefix(randomToken(), "lease-"), Tool: record.Name, SchemaHash: record.SchemaHash, Roots: roots, ReadOnly: opts.ReadOnly}
	if !opts.ReadOnly {
		if opts.Session == "" || opts.ExpiresAt == "" || opts.Arguments == nil {
			return Grant{}, errors.New("side-effecting grants require --session, --expires and --arguments (exact JSON)")
		}
		until, err := time.Parse(time.RFC3339, opts.ExpiresAt)
		if err != nil || !until.After(time.Now()) {
			return Grant{}, errors.New("--expires must be a future RFC3339 time")
		}
		grant.Session, grant.ExpiresAt, grant.ArgumentsHash = opts.Session, until.UTC().Format(time.RFC3339), Fingerprint(opts.Arguments)
	}
	err = p.Update(func(reg *Registry) error {
		server := reg.Servers[serverID]
		if server.ConfigHash != currentHash {
			server.Grants = nil
		}
		server.ConfigHash = currentHash
		if len(server.Grants) >= 512 {
			return errors.New("grant limit reached")
		}
		server.Grants = append(server.Grants, grant)
		reg.Servers[serverID] = server
		return nil
	})
	return grant, err
}

func (p *Pool) Revoke(serverID, tool string) error {
	return p.Update(func(reg *Registry) error {
		server, ok := reg.Servers[serverID]
		if !ok {
			return fmt.Errorf("unknown backend %q", serverID)
		}
		kept := server.Grants[:0]
		for _, g := range server.Grants {
			if g.Tool != tool {
				kept = append(kept, g)
			}
		}
		server.Grants = kept
		reg.Servers[serverID] = server
		return nil
	})
}

// Remove deletes the backend definition and its cached catalog, denies new
// calls immediately and defers closing live leases until in-flight calls end.
func (p *Pool) Remove(serverID string) error {
	err := p.Update(func(reg *Registry) error {
		if _, ok := reg.Servers[serverID]; !ok {
			return fmt.Errorf("unknown backend %q", serverID)
		}
		delete(reg.Servers, serverID)
		return nil
	})
	if err != nil {
		return err
	}
	if err := p.Ops.RemoveServerCatalog(serverID); err != nil {
		return err
	}
	p.CloseServer(serverID)
	return nil
}

func (p *Pool) CloseServer(serverID string) {
	p.mu.Lock()
	var closing []*lease
	for token, l := range p.leases {
		if l.server == serverID {
			closing = append(closing, l)
			delete(p.leases, token)
		}
	}
	p.mu.Unlock()
	for _, l := range closing {
		l.shutdown(false)
	}
}

func (l *lease) shutdown(force bool) {
	l.mu.Lock()
	l.released = true
	if l.timer != nil {
		l.timer.Stop()
	}
	closeNow := l.refs == 0 || force
	l.mu.Unlock()
	if closeNow {
		l.conn.Close()
	}
}

func (l *lease) done() {
	l.mu.Lock()
	if l.refs > 0 {
		l.refs--
	}
	closeNow := l.refs == 0 && l.released
	l.mu.Unlock()
	if closeNow {
		l.conn.Close()
	}
}

func (l *lease) arm(p *Pool) {
	l.mu.Lock()
	if l.released {
		l.mu.Unlock()
		return
	}
	if l.timer != nil {
		l.timer.Stop()
	}
	l.timerEpoch++
	epoch := l.timerEpoch
	l.timer = time.AfterFunc(p.IdleTTL, func() {
		p.mu.Lock()
		l.mu.Lock()
		idle := l.timerEpoch == epoch && l.refs == 0 && !l.released
		if idle {
			l.released = true
			delete(p.leases, l.token)
		}
		l.mu.Unlock()
		p.mu.Unlock()
		if idle {
			l.conn.Close()
		}
	})
	l.mu.Unlock()
}

func (p *Pool) acquire(ctx context.Context, serverID, root, session string, server Server) (*lease, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ctx.Err() != nil {
		return nil, ErrUnavailable
	}
	for _, l := range p.leases {
		if l.server == serverID && l.root == root && l.session == session {
			if l.configHash != server.ConfigHash {
				delete(p.leases, l.token)
				l.shutdown(false)
				continue
			}
			l.mu.Lock()
			usable := !l.released && l.conn.Alive()
			l.refs++
			l.mu.Unlock()
			if usable {
				return l, nil
			}
			l.mu.Lock()
			l.refs--
			l.mu.Unlock()
			// Do not leave a dead lease addressable by its old token: a later
			// getLease would otherwise hand out a connection that cannot serve
			// the call, then the next call would acquire a different lease.
			delete(p.leases, l.token)
			l.shutdown(true)
		}
	}
	if !server.StartAllowed {
		return nil, ErrApproval
	}
	runtime, hash, err := server.Source.Read()
	if err != nil {
		return nil, err
	}
	if hash != server.ConfigHash {
		return nil, ErrChanged
	}
	ctx, cancel := context.WithTimeout(ctx, server.Timeout())
	defer cancel()
	conn, err := connect(ctx, runtime, backendCWD(runtime))
	if err != nil {
		return nil, err
	}
	l := &lease{token: randomToken(), server: serverID, root: root, session: session, configHash: hash, conn: conn, refs: 1}
	p.leases[l.token] = l
	l.arm(p)
	return l, nil
}

func (p *Pool) getLease(token, serverID, root, session string) (*lease, error) {
	p.mu.Lock()
	l, ok := p.leases[token]
	p.mu.Unlock()
	if !ok {
		return nil, ErrLease
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released || l.server != serverID || l.root != root || l.session != session || !l.conn.Alive() {
		return nil, ErrLease
	}
	l.refs++
	return l, nil
}

func (p *Pool) Release(token string) error {
	p.mu.Lock()
	l, ok := p.leases[token]
	if ok {
		delete(p.leases, token)
	}
	p.mu.Unlock()
	if !ok {
		return ErrLease
	}
	l.shutdown(false)
	return nil
}

// StaleGrant reports a schema mismatch requiring an explicit operator re-grant.
type StaleGrant struct {
	Server       string `json:"server"`
	Tool         string `json:"tool"`
	GrantHash    string `json:"grant_schema_hash"`
	CurrentHash  string `json:"current_schema_hash"`
	ReadOnly     bool   `json:"read_only"`
	ArgumentsSet bool   `json:"arguments_bound"`
}

func (p *Pool) StaleGrants() ([]StaleGrant, error) {
	reg, err := p.registry()
	if err != nil {
		return nil, err
	}
	var stale []StaleGrant
	for id, server := range reg.Servers {
		if len(server.Grants) == 0 {
			continue
		}
		catalog, err := p.Ops.LoadCatalog(id)
		if err != nil || len(catalog) == 0 {
			continue
		}
		current := make(map[string]string, len(catalog))
		for _, record := range catalog {
			current[record.Name] = record.SchemaHash
		}
		for _, grant := range server.Grants {
			hash, ok := current[grant.Tool]
			if ok && hash == grant.SchemaHash {
				continue
			}
			stale = append(stale, StaleGrant{
				Server: id, Tool: grant.Tool, GrantHash: grant.SchemaHash,
				CurrentHash: hash, ReadOnly: grant.ReadOnly, ArgumentsSet: grant.ArgumentsHash != "",
			})
		}
	}
	sort.Slice(stale, func(i, j int) bool {
		if stale[i].Server == stale[j].Server {
			return stale[i].Tool < stale[j].Tool
		}
		return stale[i].Server < stale[j].Server
	})
	return stale, nil
}

// Call enforces operator grants against live identity: configuration changes
// and changed tool definitions invalidate old approvals instead of reusing them.
func (p *Pool) Call(ctx context.Context, req CallRequest) (result *mcp.CallToolResult, token string, err error) {
	if ctx.Err() != nil {
		return nil, "", ErrUnavailable
	}
	reg, err := p.registry()
	if err != nil {
		return nil, "", err
	}
	server, ok := reg.Servers[req.Server]
	if !ok {
		p.CloseServer(req.Server)
		return nil, "", ErrApproval
	}
	if p.Host != "" && server.Host != "" && p.Host != server.Host {
		return nil, "", ErrApproval
	}
	if !server.Enabled || !server.StartAllowed {
		p.CloseServer(req.Server)
		return nil, "", ErrApproval
	}
	if !filepath.IsAbs(req.Root) || req.Session == "" {
		return nil, "", ErrApproval
	}
	root := canonicalPath(req.Root)
	record, err := p.Ops.LoadCatalogTool(req.Server, req.Tool)
	if err != nil {
		return nil, "", errors.New("backend_not_discovered: refresh this server to cache the selected tool first")
	}
	selected := &record
	runtime, hash, err := server.Source.Read()
	if err != nil {
		return nil, "", err
	}
	if hash != selected.ConfigHash {
		p.CloseServer(req.Server)
		return nil, "", ErrChanged
	}
	if p.Trust != "agent" && server.ConfigHash != "" && hash != server.ConfigHash {
		return nil, "", ErrApproval
	}
	if p.Trust != "agent" && req.ExpectedSchemaHash != "" && req.ExpectedSchemaHash != selected.SchemaHash {
		return nil, "", ErrChanged
	}
	if p.Trust != "agent" {
		if _, ok := server.Allowed(req.Tool, selected.SchemaHash, root, req.Session, req.Arguments); !ok {
			return nil, "", ErrApproval
		}
	}
	var conn *connection
	var l *lease
	// Once a stateful connection is owned, expose its control token even if a
	// later schema check or backend call fails. An unknown outcome must never
	// force the caller to wait for TTL merely to release its own connection.
	defer func() {
		if l != nil {
			token = l.token
		}
	}()
	if req.Lease != "" {
		leaseConn, err := p.getLease(req.Lease, req.Server, root, req.Session)
		if err != nil {
			return nil, "", err
		}
		l = leaseConn
		conn = leaseConn.conn
		defer func() { l.done(); l.arm(p) }()
		if l.configHash != hash {
			p.CloseServer(req.Server)
			return nil, "", ErrChanged
		}
	} else if server.Stateful {
		created, err := p.acquire(ctx, req.Server, root, req.Session, server)
		if err != nil {
			return nil, "", err
		}
		l = created
		conn = created.conn
		defer func() { l.done(); l.arm(p) }()
	} else {
		cctx, cancel := context.WithTimeout(ctx, server.Timeout())
		defer cancel()
		fresh, err := connect(cctx, runtime, backendCWD(runtime))
		if err != nil {
			return nil, "", err
		}
		conn = fresh
		defer conn.Close()
	}
	callCtx, cancel := context.WithTimeout(ctx, server.Timeout())
	defer cancel()
	tools, err := conn.Tools(callCtx)
	if err != nil {
		return nil, "", err
	}
	var live *mcp.Tool
	for i := range tools {
		if tools[i].Name == req.Tool {
			live = &tools[i]
			break
		}
	}
	if live == nil {
		return nil, "", ErrChanged
	}
	schemaHash := Fingerprint(*live)
	if schemaHash != selected.SchemaHash {
		return nil, "", ErrChanged
	}
	if p.Trust != "agent" {
		if _, ok := server.Allowed(req.Tool, schemaHash, root, req.Session, req.Arguments); !ok {
			return nil, "", ErrApproval
		}
	}
	// Arbitrary request metadata is outside the granted argument digest and may
	// affect backend routing or behavior. Forward only the MCP progress token,
	// which CallWithProgress remaps to an independent backend token.
	var meta *mcp.Meta
	if req.Meta != nil && req.Meta.ProgressToken != nil && req.Notify != nil {
		meta = &mcp.Meta{ProgressToken: req.Meta.ProgressToken}
	}
	result, err = conn.CallWithProgress(callCtx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: req.Tool, Arguments: req.Arguments, Meta: meta}}, req.Notify)
	if err != nil {
		return nil, "", ErrUnknown
	}
	token = ""
	if l != nil {
		token = l.token
		l.arm(p)
	}
	return result, token, nil
}

// Close only terminates processes/connections owned by this gateway.
func (p *Pool) Close() {
	p.mu.Lock()
	leases := p.leases
	p.leases = map[string]*lease{}
	p.mu.Unlock()
	for _, l := range leases {
		l.shutdown(true)
	}
}
