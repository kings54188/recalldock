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
	// testPendingTransition provides deterministic gates around pending publication.
	testPendingTransition func(string, *lease)
	mu                    sync.Mutex
	leases                map[string]*lease
	retiring              map[*lease]struct{}
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
	pending          map[leaseKey]*pendingConnect
	transient        map[*transientConnect]struct{}
	serverGeneration map[string]uint64
	serverConfigHash map[string]string
	discoveryLocks   map[string]*sync.Mutex
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
	done  chan struct{}
	lease *lease
	err   error
	// waiters counts callers sharing this attempt. It is protected by p.mu.
	waiters       int
	ownerCanceled bool
	ownerReleased bool
	// invalidated prevents CloseServer, Remove, or Close from publishing a late
	// connection after they have revoked this attempt.
	invalidated bool
	// cancel aborts the owner's connect. Pool.Close uses it so a shutdown does not
	// have to wait out a backend's full startup timeout before returning.
	cancel context.CancelFunc
}

type transientConnect struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type lease struct {
	mu                           sync.Mutex
	token, server, root, session string
	configHash                   string
	conn                         *connection
	pool                         *Pool
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
	return &Pool{
		RegistryPath:     registryPath,
		Ops:              ops,
		IdleTTL:          5 * time.Minute,
		Trust:            "agent",
		leases:           map[string]*lease{},
		retiring:         map[*lease]struct{}{},
		pending:          map[leaseKey]*pendingConnect{},
		transient:        map[*transientConnect]struct{}{},
		serverGeneration: map[string]uint64{},
		serverConfigHash: map[string]string{},
		discoveryLocks:   map[string]*sync.Mutex{},
	}
}

func (p *Pool) beginTransient(parent context.Context) (context.Context, *transientConnect, error) {
	ctx, cancel := context.WithCancel(parent)
	op := &transientConnect{cancel: cancel, done: make(chan struct{})}
	for {
		if parent.Err() != nil {
			cancel()
			return nil, nil, ErrUnavailable
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			cancel()
			return nil, nil, ErrUnavailable
		}
		reclaimed, reclaimErr := p.reclaimIdleLeasesLocked()
		if len(reclaimed) > 0 {
			p.mu.Unlock()
			for _, l := range reclaimed {
				l.shutdown(true)
			}
			continue
		}
		if reclaimErr != nil {
			p.mu.Unlock()
			cancel()
			return nil, nil, reclaimErr
		}
		p.transient[op] = struct{}{}
		p.mu.Unlock()
		return ctx, op, nil
	}
}

func (p *Pool) endTransient(op *transientConnect) {
	p.mu.Lock()
	delete(p.transient, op)
	p.mu.Unlock()
	op.cancel()
	close(op.done)
}

func (p *Pool) finishRetiring(l *lease) {
	p.mu.Lock()
	delete(p.retiring, l)
	p.mu.Unlock()
}

func (p *Pool) releasePendingOwner(pending *pendingConnect) {
	p.mu.Lock()
	lease := pending.lease
	release := lease != nil && !pending.ownerReleased
	retire := false
	if release {
		pending.ownerReleased = true
		if pending.waiters == 0 && p.leases[lease.token] == lease {
			delete(p.leases, lease.token)
			lease.mu.Lock()
			lease.released = true
			lease.pool = p
			lease.mu.Unlock()
			p.retiring[lease] = struct{}{}
			retire = true
		}
	}
	p.mu.Unlock()
	if release {
		lease.done()
		if !retire {
			lease.arm(p)
		}
	}
}

func (p *Pool) discoveryLock(serverID string) *sync.Mutex {
	p.mu.Lock()
	lock := p.discoveryLocks[serverID]
	if lock == nil {
		lock = &sync.Mutex{}
		p.discoveryLocks[serverID] = lock
	}
	p.mu.Unlock()
	return lock
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

// reclaimIdleLeasesLocked removes leases that are provably safe to drop and
// returns them for shutdown after p.mu is released. Callers must hold p.mu.
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
func (p *Pool) reclaimIdleLeasesLocked() ([]*lease, error) {
	limit := p.leaseLimit()
	// releasedOrDead leases free capacity; pending ones hold a reservation.
	held := len(p.leases) + len(p.pending) + len(p.retiring) + len(p.transient)
	if held < limit {
		return nil, nil
	}
	var closing []*lease
	for token, l := range p.leases {
		l.mu.Lock()
		conn := l.conn
		drop := l.refs == 0 && (l.released || conn == nil || !conn.Alive())
		if drop {
			l.released = true
			l.pool = p
		}
		l.mu.Unlock()
		if drop {
			delete(p.leases, token)
			p.retiring[l] = struct{}{}
			closing = append(closing, l)
		}
	}
	held = len(p.leases) + len(p.pending) + len(p.retiring) + len(p.transient)
	if held-len(closing) >= limit {
		return closing, fmt.Errorf("lease_limit: %d backend connections are held (bound %d); calls in flight are never evicted, and idle stateful connections stay until released, so release ones you no longer need with tools_release and retry", held, limit)
	}
	return closing, nil
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

func (p *Pool) discoverWith(ctx context.Context, serverID string) (Server, Runtime, string, *connection, []opsstore.ToolRecord, error) {
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
	ctx, cancel := context.WithTimeout(ctx, server.Timeout())
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
	lock := p.discoveryLock(serverID)
	lock.Lock()
	defer lock.Unlock()

	ctx, operation, err := p.beginTransient(context.Background())
	if err != nil {
		return Server{}, nil, err
	}
	defer p.endTransient(operation)

	server, _, hash, conn, records, err := p.discoverWith(ctx, serverID)
	if err != nil {
		return server, nil, err
	}
	defer conn.Close()
	if ctx.Err() != nil {
		return server, nil, ErrUnavailable
	}
	if _, currentHash, err := server.Source.Read(); err != nil || currentHash != hash {
		return server, nil, ErrChanged
	}
	previous, err := p.Ops.LoadCatalog(serverID)
	if err != nil {
		return server, nil, err
	}
	if err := p.Ops.SaveCatalog(serverID, hash, records); err != nil {
		return server, nil, err
	}
	// Keep the registry identity aligned with the catalog this discovery just
	// created. Without it a successful discover could still leave Call with a
	// stale config_hash and reject every agent-mode call.
	changed := false
	if err := p.Update(func(reg *Registry) error {
		current, ok := reg.Servers[serverID]
		if !ok {
			return ErrUnavailable
		}
		if current.ConfigHash != hash {
			changed = true
			current.ConfigHash = hash
			current.Grants = nil
		}
		reg.Servers[serverID] = current
		return nil
	}); err != nil {
		var rollbackErr error
		if len(previous) == 0 {
			rollbackErr = p.Ops.RemoveServerCatalog(serverID)
		} else {
			rollbackErr = p.Ops.SaveCatalog(serverID, previous[0].ConfigHash, previous)
		}
		if rollbackErr != nil {
			return server, nil, errors.Join(err, fmt.Errorf("restore previous catalog: %w", rollbackErr))
		}
		return server, nil, err
	}
	p.mu.Lock()
	p.serverConfigHash[serverID] = hash
	var invalidated []*pendingConnect
	if changed {
		p.serverGeneration[serverID]++
		for key, pending := range p.pending {
			if key.server == serverID {
				pending.invalidated = true
				invalidated = append(invalidated, pending)
			}
		}
	}
	p.mu.Unlock()
	for _, pending := range invalidated {
		pending.cancel()
	}
	for _, pending := range invalidated {
		<-pending.done
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
	lock := p.discoveryLock(serverID)
	lock.Lock()
	defer lock.Unlock()

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
	// Once the registry no longer contains the server, invalidate its live and
	// pending lifecycle immediately. Catalog cleanup may fail independently, but
	// that must not let a removed backend publish or retain a connection.
	p.CloseServer(serverID)
	return p.Ops.RemoveServerCatalog(serverID)
}

func (p *Pool) CloseServer(serverID string) {
	p.mu.Lock()
	p.serverGeneration[serverID]++
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
			l.pool = p
			l.mu.Unlock()
			p.retiring[l] = struct{}{}
		}
	}
	var pending []*pendingConnect
	for key, pc := range p.pending {
		if key.server == serverID {
			pc.invalidated = true
			pending = append(pending, pc)
		}
	}
	p.mu.Unlock()

	for _, pc := range pending {
		if pc.cancel != nil {
			pc.cancel()
		}
	}
	for _, l := range closing {
		// Server removal and configuration drift retire the lease without
		// interrupting tool calls that already hold a reference.
		l.shutdown(false)
	}
	for _, pc := range pending {
		<-pc.done
	}
}

func (l *lease) shutdown(force bool) {
	l.mu.Lock()
	l.released = true
	if l.timer != nil {
		l.timer.Stop()
	}
	conn := l.conn
	closeNow := l.refs == 0 || force || conn == nil
	pool := l.pool
	l.mu.Unlock()
	// A lease that never finished acquiring has no connection; shutdown is
	// still the right call for it, so tolerate the nil rather than panic.
	if closeNow && conn != nil {
		conn.Close()
	}
	if closeNow && pool != nil {
		pool.finishRetiring(l)
	}
}

func (l *lease) done() {
	l.mu.Lock()
	if l.refs > 0 {
		l.refs--
	}
	closeNow := l.refs == 0 && l.released
	conn := l.conn
	pool := l.pool
	l.mu.Unlock()
	if closeNow && conn != nil {
		conn.Close()
	}
	if closeNow && pool != nil {
		pool.finishRetiring(l)
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
			l.pool = p
			delete(p.leases, l.token)
			p.retiring[l] = struct{}{}
		}
		conn := l.conn
		l.mu.Unlock()
		p.mu.Unlock()
		if idle && conn != nil {
			conn.Close()
		}
		if idle {
			p.finishRetiring(l)
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
	p.mu.Lock()
	generation := p.serverGeneration[serverID]
	knownHash, tracked := p.serverConfigHash[serverID]
	p.mu.Unlock()
	if tracked && knownHash != server.ConfigHash {
		return nil, ErrChanged
	}

	for {
		if ctx.Err() != nil {
			return nil, ErrUnavailable
		}
		p.mu.Lock()
		if p.closed || p.serverGeneration[serverID] != generation {
			p.mu.Unlock()
			return nil, ErrUnavailable
		}
		// Leases removed here are shut down after p.mu is released: shutdown can
		// terminate a process, and doing that while holding the pool lock would
		// reintroduce the stall this change set removed (a backend that ignores
		// SIGTERM blocks the pool for its whole kill timeout).
		var retire []*lease
		var forceClose []*lease
		// Reuse a live lease for this key, exactly as before.
		for _, l := range p.leases {
			if l.server != serverID || l.root != root || l.session != session {
				continue
			}
			if l.configHash != server.ConfigHash {
				delete(p.leases, l.token)
				l.mu.Lock()
				l.released = true
				l.pool = p
				l.mu.Unlock()
				p.retiring[l] = struct{}{}
				retire = append(retire, l)
				continue
			}
			l.mu.Lock()
			conn := l.conn
			usable := !l.released && conn != nil && conn.Alive()
			if usable {
				l.refs++
				l.mu.Unlock()
				p.mu.Unlock()
				return l, nil
			}
			l.released = true
			l.pool = p
			l.mu.Unlock()
			// Do not leave a dead lease addressable by its old token. A dead
			// backend cannot finish an in-flight tool, so its cleanup is forced.
			delete(p.leases, l.token)
			p.retiring[l] = struct{}{}
			forceClose = append(forceClose, l)
		}
		if len(retire) > 0 || len(forceClose) > 0 {
			p.mu.Unlock()
			for _, l := range retire {
				// Configuration drift retires the old connection for new calls but
				// preserves tools already executing on it.
				l.shutdown(false)
			}
			for _, l := range forceClose {
				l.shutdown(true)
			}
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
			joined := pending
			joined.waiters++
			p.mu.Unlock()
			select {
			case <-joined.ready:
			case <-ctx.Done():
			}
			p.mu.Lock()
			joined.waiters--
			joinedErr := joined.err
			invalidated := joined.invalidated
			cancelShared := ctx.Err() != nil && joined.ownerCanceled && joined.waiters == 0
			var abandon *lease
			if cancelShared && joined.ownerReleased && joined.lease != nil && p.leases[joined.lease.token] == joined.lease {
				abandon = joined.lease
				delete(p.leases, abandon.token)
				abandon.mu.Lock()
				abandon.released = true
				abandon.pool = p
				abandon.mu.Unlock()
				p.retiring[abandon] = struct{}{}
			}
			p.mu.Unlock()
			if cancelShared && joined.cancel != nil {
				joined.cancel()
			}
			if abandon != nil {
				abandon.shutdown(false)
			}
			if ctx.Err() != nil {
				return nil, ErrUnavailable
			}
			if joinedErr != nil {
				// A normal connect failure belongs to its owner, so a healthy waiter
				// retries through a fresh shared attempt. Server or pool invalidation
				// is authoritative and must not be retried with the stale Server value.
				if invalidated {
					return nil, joinedErr
				}
				continue
			}
			// Fall through to the reuse path with the finished lease available.
			continue
		}

		reclaimed, reclaimErr := p.reclaimIdleLeasesLocked()
		if len(reclaimed) > 0 {
			p.mu.Unlock()
			for _, l := range reclaimed {
				l.shutdown(true)
			}
			continue
		}
		if reclaimErr != nil {
			p.mu.Unlock()
			return nil, reclaimErr
		}

		// Claim the connect before releasing p.mu so its capacity reservation is
		// visible atomically with the limit check.
		connectCtx, cancelConnect := context.WithCancel(context.WithoutCancel(ctx))
		pending := &pendingConnect{
			ready:  make(chan struct{}),
			done:   make(chan struct{}),
			cancel: cancelConnect,
		}
		p.pending[key] = pending
		p.mu.Unlock()
		go func() {
			select {
			case <-ctx.Done():
				p.mu.Lock()
				pending.ownerCanceled = true
				cancelShared := pending.waiters == 0
				p.mu.Unlock()
				if cancelShared {
					cancelConnect()
				}
			case <-pending.done:
			}
		}()
		for _, l := range reclaimed {
			l.shutdown(true)
		}

		func() {
			defer close(pending.done)
			lease, err := p.connectLease(connectCtx, key, server)
			if err == nil && lease != nil && p.testPendingTransition != nil {
				p.testPendingTransition("connected", lease)
			}
			cancelConnect()

			p.mu.Lock()
			delete(p.pending, key)
			pending.err = err
			ownerCanceled := pending.ownerCanceled || ctx.Err() != nil
			pending.ownerCanceled = ownerCanceled
			if err == nil && lease != nil && p.testPendingTransition != nil {
				p.testPendingTransition("publishing", lease)
			}
			if p.closed || pending.invalidated || p.serverGeneration[serverID] != generation {
				pending.invalidated = true
				pending.err = ErrUnavailable
			} else if err == nil && lease != nil {
				if ownerCanceled && pending.waiters == 0 {
					pending.err = ErrUnavailable
				} else {
					pending.lease = lease
					p.leases[lease.token] = lease
				}
			}
			close(pending.ready)
			p.mu.Unlock()

			if pending.lease == nil {
				if lease != nil {
					if p.testPendingTransition != nil {
						p.testPendingTransition("cleanup", lease)
					}
					lease.shutdown(true)
				}
				return
			}
			if ownerCanceled {
				p.releasePendingOwner(pending)
				return
			}
			lease.arm(p)
		}()

		if ctx.Err() != nil {
			p.releasePendingOwner(pending)
			return nil, ErrUnavailable
		}
		if pending.err != nil {
			return nil, pending.err
		}
		if pending.lease == nil {
			return nil, ErrUnavailable
		}
		return pending.lease, nil
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
	return &lease{token: randomToken(), server: key.server, root: key.root, session: key.session, configHash: hash, conn: conn, pool: p, refs: 1}, nil
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

func (p *Pool) discardBrokenLease(l *lease) {
	if l == nil {
		return
	}
	p.mu.Lock()
	current, present := p.leases[l.token]
	if present && current == l {
		delete(p.leases, l.token)
		l.mu.Lock()
		l.released = true
		l.pool = p
		l.mu.Unlock()
		p.retiring[l] = struct{}{}
	}
	p.mu.Unlock()
	if present && current == l {
		l.shutdown(true)
	}
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
		l.pool = p
		l.mu.Unlock()
		p.retiring[l] = struct{}{}
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
	callBaseCtx := ctx
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
		p.mu.Lock()
		current, present := p.leases[l.token]
		l.mu.Lock()
		conn := l.conn
		owned := present && current == l && !l.released && conn != nil && conn.Alive()
		l.mu.Unlock()
		p.mu.Unlock()
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
		transientCtx, operation, err := p.beginTransient(ctx)
		if err != nil {
			return nil, "", err
		}
		defer p.endTransient(operation)
		callBaseCtx = transientCtx
		cctx, cancel := context.WithTimeout(transientCtx, server.Timeout())
		defer cancel()
		fresh, err := connect(cctx, runtime, backendCWD(runtime))
		if err != nil {
			return nil, "", err
		}
		conn = fresh
		defer conn.Close()
	}
	callCtx, cancel := context.WithTimeout(callBaseCtx, server.Timeout())
	defer cancel()
	tools, err := conn.Tools(callCtx)
	if err != nil {
		if callCtx.Err() == nil {
			p.discardBrokenLease(l)
		}
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
		if callCtx.Err() == nil {
			p.discardBrokenLease(l)
		}
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
	leases := make([]*lease, 0, len(p.leases)+len(p.retiring))
	for _, l := range p.leases {
		leases = append(leases, l)
	}
	for l := range p.retiring {
		leases = append(leases, l)
	}
	p.leases = map[string]*lease{}
	p.retiring = map[*lease]struct{}{}
	// Mark released inside the same critical section as the drain.
	for _, l := range leases {
		l.mu.Lock()
		l.released = true
		l.pool = p
		l.mu.Unlock()
	}
	pending := make([]*pendingConnect, 0, len(p.pending))
	for _, pc := range p.pending {
		pc.invalidated = true
		pending = append(pending, pc)
	}
	transient := make([]*transientConnect, 0, len(p.transient))
	for op := range p.transient {
		transient = append(transient, op)
	}
	p.mu.Unlock()

	// Cancel connects first; process termination for published leases can take its
	// full bounded shutdown interval and must not delay pending cancellation.
	for _, pc := range pending {
		if pc.cancel != nil {
			pc.cancel()
		}
	}
	for _, op := range transient {
		op.cancel()
	}
	for _, l := range leases {
		l.shutdown(true)
	}
	// ready only means callers can observe the result. done closes after a late
	// connection has either been published or fully shut down.
	for _, pc := range pending {
		<-pc.done
	}
	for _, op := range transient {
		<-op.done
	}
}
