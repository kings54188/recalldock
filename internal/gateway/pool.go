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
	"recalldock.local/recalldock/internal/opsstore"
)

type Pool struct {
	RegistryPath  string
	Ops           *opsstore.Store
	IdleTTL       time.Duration
	Host          string // trusted launcher configuration, never a model argument
	HostSessionID string
	Trust         string // "agent" (local host delegate) or "strict" (exact grants)
	// maxLeases overrides maxConcurrentLeases when positive. It is unexported on
	// purpose: the bound is a fixed production constant, and an exported field
	// would let any caller (or a future config parser) raise or lift it. Only
	// tests in this package can lower it, and only to reach the boundary with a
	// few synthetic leases instead of 512 real backend processes.
	maxLeases int
	mu        sync.Mutex
	leases    map[string]*lease
	// closed is set by Close. It exists because connecting happens outside p.mu
	// (see acquire): without it, a connect that finishes after Close would insert
	// its lease into a pool nobody owns, so the backend process would leak with no
	// way to release it. Acquire refuses once closed, and the connect completion
	// path checks the flag before publishing.
	closed bool
	// pending holds in-flight connection attempts keyed by leaseKey. It exists
	// so connecting happens outside p.mu (see acquire) without letting two
	// callers race into duplicate backends for the same server/root/session.
	// Each entry is closed when its attempt finishes, success or failure.
	//
	// A pending entry also reserves a lease slot: every pending attempt will own
	// one backend if it succeeds, so capacity is counted as
	// len(leases)+len(pending). Counting only len(leases) let several concurrent
	// connects pass the bound check and then all succeed, overshooting
	// maxConcurrentLeases.
	pending map[leaseKey]*pendingConnect
}

// leaseKey is the identity a lease is keyed by. It is a struct rather than a
// concatenated string so that a server ID or session containing the separator
// cannot alias a different key.
type leaseKey struct {
	server, root, session string
}

// pendingConnect is one in-flight connection attempt. Callers that want the
// same key wait on ready instead of starting a second backend.
type pendingConnect struct {
	ready chan struct{}
	lease *lease
	err   error
	// cancel aborts the owner's connect. Pool.Close uses it so a shutdown does not
	// have to wait out a backend's full startup timeout before returning.
	cancel context.CancelFunc
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
	return &Pool{RegistryPath: registryPath, Ops: ops, IdleTTL: 5 * time.Minute, Trust: "agent", leases: map[string]*lease{}, pending: map[leaseKey]*pendingConnect{}}
}

// maxConcurrentLeases bounds live backend processes (not registered servers).
// It matches the per-server grant cap so a gateway cannot hold more child
// processes than the registry can meaningfully describe.
const maxConcurrentLeases = 512

// leaseLimit is the effective bound for this pool. It defaults to
// maxConcurrentLeases and exists as an unexported field so tests in this package
// can exercise the boundary with a handful of synthetic leases instead of
// spawning 512 real backends. Production code never sets it, and nothing outside
// this package can: the bound is not operator-configurable.
func (p *Pool) leaseLimit() int {
	if p.maxLeases > 0 {
		return p.maxLeases
	}
	return maxConcurrentLeases
}

// reclaimIdleLeaseLocked frees leases that are provably safe to drop and then
// fails when the pool is still at its bound. Callers must hold p.mu.
//
// Three distinct quantities matter here, and conflating them was the mistake
// behind Issue #6:
//
//   - live leases: every entry in p.leases, each owning a backend process.
//     This is what the bound limits.
//   - in-flight references (l.refs): calls currently executing on a lease.
//   - idle but retained: a live lease with refs == 0 that was not released.
//     Pool.Call does done()+arm(), which returns refs to zero but deliberately
//     keeps the connection so a stateful backend keeps its state and its
//     tools_release token stays valid.
//
// Because idle-but-retained leases are still live leases, *serial* calls to
// distinct roots or sessions do accumulate against the bound. They are not
// reclaimed here: a healthy idle connection is a retained stateful session, and
// evicting it would silently drop backend state the caller may still need. Only
// leases that are already released, never connected, or whose process has died
// are dropped. A caller at the bound is refused and told to release explicitly.
//
// Pending connects reserve capacity too. Each pending attempt will own one
// backend process if it succeeds, so it counts against the bound exactly like a
// live lease. Without that, several concurrent connects could each observe
// len(leases) < limit, all proceed, and then all succeed — overshooting the
// bound that exists to bound backend processes.
func (p *Pool) reclaimIdleLeaseLocked() error {
	limit := p.leaseLimit()
	// releasedOrDead leases free capacity; pending ones hold a reservation.
	held := len(p.leases) + len(p.pending)
	if held < limit {
		return nil
	}
	for token, l := range p.leases {
		l.mu.Lock()
		conn := l.conn
		drop := l.refs == 0 && (l.released || conn == nil || !conn.Alive())
		l.mu.Unlock()
		if drop {
			delete(p.leases, token)
			l.shutdown(true)
		}
	}
	if held = len(p.leases) + len(p.pending); held >= limit {
		return fmt.Errorf("lease_limit: %d backend connections are held (bound %d); calls in flight are never evicted, and idle stateful connections stay until released, so release ones you no longer need with tools_release and retry", held, limit)
	}
	return nil
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
	// Create-only lock fails closed after a crashed writer; never steal by age.
	lockPath := p.RegistryPath + ".lock"
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("registry lock unavailable; inspect before recovery")
	}
	defer func() { lock.Close(); os.Remove(lockPath) }()
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
	// Discovery starts a backend to read its tool list, so it must respect the
	// closed flag like acquire does.
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return Server{}, nil, ErrUnavailable
	}
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
			// Same ordering rule as Release: mark released before the token stops
			// resolving, so a concurrent Call cannot publish a token that is already
			// gone from the pool.
			l.mu.Lock()
			l.released = true
			l.mu.Unlock()
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
	conn := l.conn
	l.mu.Unlock()
	// A lease that never finished acquiring has no connection; shutdown is
	// still the right call for it, so tolerate the nil rather than panic.
	if closeNow && conn != nil {
		conn.Close()
	}
}

func (l *lease) done() {
	l.mu.Lock()
	if l.refs > 0 {
		l.refs--
	}
	closeNow := l.refs == 0 && l.released
	conn := l.conn
	l.mu.Unlock()
	if closeNow && conn != nil {
		conn.Close()
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

// acquire returns a referenced lease for (serverID, root, session), starting a
// backend if none is live.
//
// Connecting a backend can take as long as the backend's startup timeout. This
// used to happen while holding p.mu, so one slow or hanging backend blocked
// every other caller — including tools_release, which needs no connection at
// all (audit D5). The connection is now established outside the lock.
//
// Doing that naively would let two concurrent callers for the same key spawn
// two backends for one logical lease, so attempts are registered in p.pending
// first: the winner connects, the others wait on its result and share it. That
// keeps "one backend per server/root/session" without serialising unrelated
// keys.
func (p *Pool) acquire(ctx context.Context, serverID, root, session string, server Server) (*lease, error) {
	if ctx.Err() != nil {
		return nil, ErrUnavailable
	}
	key := leaseKey{server: serverID, root: root, session: session}

	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, ErrUnavailable
		}
		// Leases removed here are shut down after p.mu is released: shutdown can
		// terminate a process, and doing that while holding the pool lock would
		// reintroduce the stall this change set removed (a backend that ignores
		// SIGTERM blocks the pool for its whole kill timeout).
		var toClose []*lease
		// Reuse a live lease for this key, exactly as before.
		for _, l := range p.leases {
			if l.server != serverID || l.root != root || l.session != session {
				continue
			}
			if l.configHash != server.ConfigHash {
				delete(p.leases, l.token)
				// Mark released under p.mu, before shutdown, so a concurrent Call
				// cannot publish a token this removal just invalidated.
				l.mu.Lock()
				l.released = true
				l.mu.Unlock()
				toClose = append(toClose, l)
				continue
			}
			l.mu.Lock()
			usable := !l.released && l.conn.Alive()
			l.refs++
			l.mu.Unlock()
			if usable {
				p.mu.Unlock()
				return l, nil
			}
			l.mu.Lock()
			l.refs--
			l.mu.Unlock()
			// Do not leave a dead lease addressable by its old token.
			delete(p.leases, l.token)
			l.mu.Lock()
			l.released = true
			l.mu.Unlock()
			toClose = append(toClose, l)
		}
		if len(toClose) > 0 {
			p.mu.Unlock()
			for _, l := range toClose {
				l.shutdown(true)
			}
			// Re-acquire and re-evaluate: the pool changed while the lock was
			// released, so the reuse scan must run again rather than continuing on a
			// stale view.
			p.mu.Lock()
			continue
		}

		if !server.StartAllowed {
			p.mu.Unlock()
			return nil, ErrApproval
		}
		// A caller that joins an existing pending attempt consumes no extra capacity:
		// it shares the lease that attempt is already creating. So the join must be
		// checked before the bound is enforced, otherwise a caller at the limit with a
		// perfectly good in-flight attempt for its own key is told to go release
		// somebody else's connection.
		// Another caller is already connecting this exact key. Wait for it rather
		// than opening a second backend, then re-evaluate: its result may be
		// reusable, or it may have failed and we start our own attempt.
		if pending, ok := p.pending[key]; ok {
			// Note the attempt we joined. If it ends in failure we compare against
			// this entry so we only retry for a genuinely finished attempt, not one
			// that is still the same pending record.
			joined := pending
			p.mu.Unlock()
			select {
			case <-pending.ready:
			case <-ctx.Done():
				return nil, ErrUnavailable
			}
			if joined.err != nil {
				// The attempt we joined failed. That failure belongs to its owner: the
				// owner's context may have been cancelled, or its connect may have hit a
				// transient problem, while this caller is healthy and still wants a
				// connection. Returning the owner's error here would fail a caller that
				// did nothing wrong and could have succeeded on its own attempt.
				//
				// Retry once from the top, where a fresh attempt is made (or an existing
				// live lease is reused). A caller whose own context is cancelled is still
				// handled above.
				if ctx.Err() == nil {
					continue
				}
				return nil, joined.err
			}
			// Fall through to the reuse path with the finished lease available.
			continue
		}

		if err := p.reclaimIdleLeaseLocked(); err != nil {
			p.mu.Unlock()
			return nil, err
		}

		// Claim the connect. Everything from here to the store is outside p.mu.
		//
		// The connect runs on a context derived from the caller's but detached from
		// its cancellation, because the result is shared: if the caller that happens
		// to own the attempt gives up, every waiter would receive that caller's
		// cancellation error even though their own contexts are healthy. Waiters
		// still return early on their own ctx (below); the connect itself is
		// cancelled only by Pool.Close via pending.cancel, bounded by the server
		// timeout inside connectLease.
		connectCtx, cancelConnect := context.WithCancel(context.WithoutCancel(ctx))
		pending := &pendingConnect{ready: make(chan struct{}), cancel: cancelConnect}
		p.pending[key] = pending
		p.mu.Unlock()

		lease, err := p.connectLease(connectCtx, key, server)
		cancelConnect()

		p.mu.Lock()
		delete(p.pending, key)
		pending.err = err
		// Publish only if the pool is still open. A connect that finishes after
		// Close has a live backend process and no owner: inserting it would leak
		// the process, since nothing would ever release a lease the caller is
		// about to be told does not exist.
		if err == nil && lease != nil {
			if p.closed {
				pending.lease, pending.err = nil, ErrUnavailable
			} else {
				pending.lease = lease
				p.leases[lease.token] = lease
			}
		}
		close(pending.ready)
		p.mu.Unlock()

		if err != nil {
			return nil, err
		}
		if pending.lease == nil {
			// Lost the race with Close: close the connection we opened rather than
			// leaving the child process running.
			lease.shutdown(true)
			return nil, ErrUnavailable
		}
		// Arm the idle timer outside p.mu: arm only takes l.mu, and the timer
		// callback acquires p.mu, so calling it under the pool lock would deadlock
		// if the TTL were short enough to fire immediately.
		lease.arm(p)
		return lease, nil
	}
}

// connectLease performs the slow part of acquire with p.mu released.
func (p *Pool) connectLease(ctx context.Context, key leaseKey, server Server) (*lease, error) {
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
	return &lease{token: randomToken(), server: key.server, root: key.root, session: key.session, configHash: hash, conn: conn, refs: 1}, nil
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
		// Mark the lease released while still holding p.mu, before the token stops
		// resolving. Release used to delete first and set the flag afterwards, which
		// left a window where a concurrent Call saw released==false on a lease the
		// pool no longer held: it published a token that getLease would immediately
		// reject with lease_not_available. Setting the flag here makes "removed from
		// the pool" and "not publishable" the same instant.
		l.mu.Lock()
		l.released = true
		l.mu.Unlock()
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
	//
	// A lease that a concurrent tools_release removed while this call was running
	// is the exception: its token no longer resolves, so publishing it would hand
	// the caller a dead handle (audit D4).
	defer func() {
		if l == nil {
			return
		}
		l.mu.Lock()
		owned := !l.released
		l.mu.Unlock()
		if owned {
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
		// A non-stateful call opens its own short-lived connection, which used to
		// bypass the closed check entirely: after Pool.Close a tools_call could still
		// start a backend. This does not leak (the connection is closed when the call
		// returns) but it contradicts the pool's contract and behaves differently
		// from the stateful path for the same closed pool.
		p.mu.Lock()
		closed := p.closed
		p.mu.Unlock()
		if closed {
			return nil, "", ErrUnavailable
		}
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
	// The deferred block above publishes l.token when the lease is still owned.
	// Re-arming schedules the idle TTL; it must happen even if the lease was
	// released concurrently, because arm() is a no-op for a released lease.
	if l != nil {
		l.arm(p)
	}
	return result, token, nil
}

// Close terminates every backend process/connection owned by this gateway and
// waits for in-flight connection attempts to finish terminating theirs.
//
// It marks the pool closed first so no connect can publish a lease afterwards.
// Draining leases and cancelling pending attempts both happen under p.mu, and
// released is set there rather than in a later shutdown: a Call finishing during
// the drain must not observe "not released" for a lease the pool already dropped,
// or it would publish a token that getLease immediately rejects (D4).
//
// Pending connects are cancelled and waited for. Leaving them to finish on their
// own meant Close could return while a child process was still starting up, and
// that child would outlive the gateway as an orphan — the same process leak this
// change set is meant to prevent, just triggered one step earlier.
func (p *Pool) Close() {
	p.mu.Lock()
	p.closed = true
	leases := p.leases
	p.leases = map[string]*lease{}
	// Mark released inside the same critical section as the drain.
	for _, l := range leases {
		l.mu.Lock()
		l.released = true
		l.mu.Unlock()
	}
	pending := make([]*pendingConnect, 0, len(p.pending))
	for _, pc := range p.pending {
		pending = append(pending, pc)
	}
	p.mu.Unlock()

	for _, l := range leases {
		l.shutdown(true)
	}
	// Cancel in-flight connects so their owners stop waiting on a backend that is
	// about to belong to a closed pool, then wait for them to shut their own
	// children down. Each owner observes p.closed and closes what it opened.
	for _, pc := range pending {
		if pc.cancel != nil {
			pc.cancel()
		}
	}
	for _, pc := range pending {
		<-pc.ready
	}
}
