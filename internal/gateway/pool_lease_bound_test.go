package gateway

import (
	"context"
	"errors"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The contributor's tests build synthetic lease structs and call the reclaim
// helper directly. These tests go through Pool.acquire with real backend
// connections at a small bound, so they cover the path a caller actually takes:
// connection creation, reuse, refusal, and clean-up.
//
// Pool.maxLeases exists so the boundary can be reached with three backends
// instead of 512. TestProductionLeaseLimitIsNotConfigurable pins both the real
// constant and the fact that no exported field can raise it.

// TestProductionLeaseLimitIsFixed guards the production bound. The override is
// unexported, so nothing outside this package can change it; if this fails,
// someone made the bound operator-configurable and the DoS reasoning changes.
func TestProductionLeaseLimitIsFixed(t *testing.T) {
	if maxConcurrentLeases != 512 {
		t.Fatalf("maxConcurrentLeases changed to %d; re-check the grant-cap parity argument", maxConcurrentLeases)
	}
	p, _, _ := poolFixture(t, false)
	if p.maxLeases != 0 {
		t.Fatalf("NewPool set maxLeases=%d; production pools must use the constant", p.maxLeases)
	}
	if got := p.leaseLimit(); got != maxConcurrentLeases {
		t.Fatalf("default leaseLimit() = %d, want the production constant %d", got, maxConcurrentLeases)
	}
}

// TestProductionLeaseLimitIsNotConfigurable is the structural half of the guard
// above: it inspects Pool's type rather than an instance. The lease bound must
// stay unexported so no caller, config decoder or future option can lift it —
// the DoS argument depends on the limit being a constant, not a suggestion.
func TestProductionLeaseLimitIsNotConfigurable(t *testing.T) {
	typ := reflect.TypeOf(Pool{})
	var exported []string
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.IsExported() {
			exported = append(exported, f.Name)
		}
	}
	sort.Strings(exported)
	for _, name := range exported {
		lower := strings.ToLower(name)
		if strings.Contains(lower, "lease") || strings.Contains(lower, "limit") || strings.Contains(lower, "bound") {
			t.Fatalf("Pool exposes exported field %q that could widen the lease bound; the limit must stay unexported", name)
		}
	}
	// A future rename of the unexported field would silently drop the test
	// override while production behaviour stayed correct; catch that here.
	if _, ok := typ.FieldByName("maxLeases"); !ok {
		t.Fatal("Pool.maxLeases is gone; the test-only override no longer exists and these boundary tests are not reaching it")
	}
	if _, ok := typ.FieldByName("MaxLeases"); ok {
		t.Fatal("Pool.MaxLeases is exported again; callers could raise the bound")
	}
}

// TestAcquireRefusesAtLeaseLimitReachingRealBackends drives acquire directly
// until the bound is hit and asserts the refusal is a lease_limit error rather
// than a spawn past the cap.
//
// acquire is used rather than Call because Call releases its lease when the
// tool returns (done + arm), so sequential calls never accumulate leases and
// could not reach a bound. The bound protects concurrent holders, which is what
// this test models by keeping each lease referenced.
//
// Teardown is deterministic: connection.Close() blocks until the child process
// is reaped (it waits on the cmd.Wait goroutine), and the backend's working
// directory is owned by backendWorkDir rather than the testing TempDir, so no
// step here depends on guessing how long Windows takes to drop a handle.
func TestAcquireRefusesAtLeaseLimitReachingRealBackends(t *testing.T) {
	p, _, _ := poolFixture(t, false)
	p.maxLeases = 2
	if _, _, err := p.Discover("test"); err != nil {
		t.Fatalf("discover: %v", err)
	}
	// Read back after Discover: it aligns the registry config_hash, and acquire
	// compares against that value.
	reg, err := p.registry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	server := reg.Servers["test"]

	ctx := context.Background()
	for i := 0; i < 2; i++ {
		l, err := p.acquire(ctx, "test", leaseRoot(t), "s", server)
		if err != nil {
			t.Fatalf("lease %d should have been granted: %v", i, err)
		}
		// Force-close deterministically. Release would not do it here: acquire
		// leaves refs==1, and shutdown(false) only closes an unreferenced lease.
		// shutdown(true) closes now and Close blocks until the child is reaped,
		// so no part of teardown depends on a sleep.
		t.Cleanup(func() { l.shutdown(true) })
	}
	if got := len(p.leases); got != 2 {
		t.Fatalf("expected to reach the bound, have %d leases", got)
	}

	// The third distinct root must be refused with a lease_limit error.
	_, err = p.acquire(ctx, "test", leaseRoot(t), "s", server)
	if err == nil {
		t.Fatal("acquire past the lease bound succeeded")
	}
	if !strings.Contains(err.Error(), "lease_limit") {
		t.Fatalf("refusal lacks lease_limit marker: %v", err)
	}
	if got := len(p.leases); got > 2 {
		t.Fatalf("pool exceeded its bound: %d leases", got)
	}
}

// TestReleasedLeaseFreesCapacity proves a released lease returns its slot, so
// the bound does not become a permanent lockout.
//
// This uses controlled leases rather than real backends on purpose. The
// invariant under test is arithmetic on the lease map, and a real backend would
// add process-lifecycle noise without adding coverage: the real-backend path is
// covered by TestAcquireRefusesAtLeaseLimitReachingRealBackends.
func TestReleasedLeaseFreesCapacity(t *testing.T) {
	p, _, _ := poolFixture(t, false)
	p.maxLeases = 1

	p.mu.Lock()
	p.leases["held"] = &lease{token: "held", refs: 1, server: "test", root: "/held", session: "s"}
	p.mu.Unlock()

	// At the bound, a new lease is refused.
	p.mu.Lock()
	err := p.reclaimIdleLeaseLocked()
	p.mu.Unlock()
	if err == nil {
		t.Fatal("pool at its bound with a referenced lease did not refuse")
	}

	// Releasing the lease makes room again.
	if err := p.Release("held"); err != nil {
		t.Fatalf("release: %v", err)
	}
	p.mu.Lock()
	err = p.reclaimIdleLeaseLocked()
	remaining := len(p.leases)
	p.mu.Unlock()
	if err != nil {
		t.Fatalf("capacity was not restored after release: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("released lease still present: %d", remaining)
	}
}

// TestReclaimKeepsReferencedLeases proves an in-flight lease is never evicted to
// make room: dropping it would break a call that is currently executing.
func TestReclaimKeepsReferencedLeases(t *testing.T) {
	p, _, _ := poolFixture(t, false)
	p.maxLeases = 3
	p.mu.Lock()
	defer p.mu.Unlock()

	// One referenced lease and two idle-but-unreleased ones. Reclaim may drop
	// only the idle ones.
	busy := &lease{token: "busy", refs: 1, server: "test", root: "/busy", session: "s"}
	p.leases[busy.token] = busy
	p.leases["idle-1"] = &lease{token: "idle-1", refs: 0, released: true}
	p.leases["idle-2"] = &lease{token: "idle-2", refs: 0, released: true}

	if err := p.reclaimIdleLeaseLocked(); err != nil {
		t.Fatalf("reclaim should have succeeded after dropping idle leases: %v", err)
	}
	if _, ok := p.leases["busy"]; !ok {
		t.Fatal("reclaim dropped a referenced lease, which would break an in-flight call")
	}
	if len(p.leases) != 1 {
		t.Fatalf("expected only the referenced lease to remain, got %d", len(p.leases))
	}
}

// TestReclaimToleratesNilConnection covers a lease that never finished
// acquiring: shutdown must not panic on a nil connection.
func TestReclaimToleratesNilConnection(t *testing.T) {
	p, _, _ := poolFixture(t, false)
	p.maxLeases = 1
	p.mu.Lock()
	defer p.mu.Unlock()

	// refs==0 with a nil conn is exactly the "acquisition failed" shape.
	p.leases["half"] = &lease{token: "half", refs: 0}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("reclaim panicked on a nil connection: %v", r)
		}
	}()
	if err := p.reclaimIdleLeaseLocked(); err != nil {
		t.Fatalf("reclaim of a half-acquired lease failed: %v", err)
	}
	if len(p.leases) != 0 {
		t.Fatalf("half-acquired lease was not reclaimed, %d remain", len(p.leases))
	}
}

// TestShutdownToleratesNilConnection pins the nil guards added to lease
// shutdown and done.
func TestShutdownToleratesNilConnection(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("shutdown panicked on a nil connection: %v", r)
		}
	}()
	l := &lease{token: "nil-conn"}
	l.shutdown(true)
	l.mu.Lock()
	l.released = true
	l.mu.Unlock()
	l.done()
}

// TestLeaseLimitErrorIsExplainable proves the refusal reaches an MCP caller as a
// readable error rather than a generic failure.
func TestLeaseLimitErrorIsExplainable(t *testing.T) {
	p, _, _ := poolFixture(t, false)
	p.maxLeases = 1
	p.mu.Lock()
	p.leases["busy"] = &lease{token: "busy", refs: 1, server: "test", root: "/x", session: "s"}
	p.mu.Unlock()

	p.mu.Lock()
	err := p.reclaimIdleLeaseLocked()
	p.mu.Unlock()
	if err == nil {
		t.Fatal("expected a lease_limit refusal")
	}
	msg := err.Error()
	for _, want := range []string{"lease_limit", "tools_release"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("refusal %q does not mention %q, so a caller cannot act on it", msg, want)
		}
	}
	if errors.Is(err, ErrApproval) {
		t.Fatal("lease_limit must not be reported as an approval problem")
	}
}

// leaseRoot returns a directory to use as a lease root.
//
// It deliberately does NOT use t.TempDir(). These roots are only lease keys —
// acquire never makes them a working directory — but keeping lease keys out of
// the testing TempDir means a Windows teardown delay can never turn a passing
// assertion into a RemoveAll failure.
func leaseRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "rd-lease-root-")
	if err != nil {
		t.Fatalf("create lease root: %v", err)
	}
	t.Cleanup(func() {
		// Best effort: a straggling handle is not a test failure.
		_ = os.RemoveAll(dir)
	})
	return dir
}
