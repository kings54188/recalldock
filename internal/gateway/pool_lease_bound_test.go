package gateway

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// The contributor's tests build synthetic lease structs and call the reclaim
// helper directly. These tests go through Pool.acquire with real backend
// connections at a small bound, so they cover the path a caller actually takes:
// connection creation, reuse, refusal, and clean-up.
//
// Pool.MaxLeases exists so the boundary can be reached with three backends
// instead of 512. TestProductionLeaseLimitIsFixed pins the real constant.

// TestProductionLeaseLimitIsFixed guards the production bound. MaxLeases is a
// test escape hatch, so nothing in production code may set it; if this fails,
// someone made the bound operator-configurable and the DoS reasoning changes.
func TestProductionLeaseLimitIsFixed(t *testing.T) {
	if maxConcurrentLeases != 512 {
		t.Fatalf("maxConcurrentLeases changed to %d; re-check the grant-cap parity argument", maxConcurrentLeases)
	}
	p, _, _ := poolFixture(t, false)
	if p.MaxLeases != 0 {
		t.Fatalf("NewPool set MaxLeases=%d; production pools must use the constant", p.MaxLeases)
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
func TestAcquireRefusesAtLeaseLimitReachingRealBackends(t *testing.T) {
	// Real backends are used to prove the refusal comes from acquire itself, but
	// the pool is drained before the test returns and cleanup does not depend on
	// a fixture temp directory being removable.
	//
	// An earlier version failed the windows-2025 run repeatedly on teardown
	// (RemoveAll colliding with a terminating child's cwd handle) after all
	// assertions had passed. Ordering the shutdown before TempDir cleanup is not
	// sufficient on Windows, so this test now shuts down through the pool and the
	// pool's own Close, and adds a settle delay as a best-effort guard rather than
	// a correctness assumption.
	p, _, _ := poolFixture(t, false)
	p.MaxLeases = 2
	t.Cleanup(func() {
		p.Close()
		time.Sleep(500 * time.Millisecond)
	})
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
		if _, err := p.acquire(ctx, "test", leaseRoot(t), "s", server); err != nil {
			t.Fatalf("lease %d should have been granted: %v", i, err)
		}
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
func TestReleasedLeaseFreesCapacity(t *testing.T) {
	// This test uses controlled leases rather than real backends.
	//
	// The equivalent real-backend version was removed after it failed the
	// windows-2025 run four times with:
	//   TempDir RemoveAll cleanup: unlinkat ...\002: The process cannot access
	//   the file because it is being used by another process.
	// poolFixture starts a backend whose cwd is its own t.TempDir; on Windows a
	// terminating child keeps that handle briefly, and testing.TempDir turns the
	// collision into a failure after every assertion had already passed. That is
	// teardown noise from the fixture, not behaviour of the bound, so the real
	// backend path is covered by TestAcquireRefusesAtLeaseLimitReachingRealBackends
	// and this test pins the release/free invariant deterministically.
	p, _, _ := poolFixture(t, false)
	p.MaxLeases = 1
	t.Cleanup(p.Close)

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
	p.MaxLeases = 3
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
	p.MaxLeases = 1
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
	p.MaxLeases = 1
	p.mu.Lock()
	p.leases["busy"] = &lease{token: "busy", refs: 1, server: "test", root: "/x", session: "s"}
	p.mu.Unlock()

	err := p.reclaimIdleLeaseLocked
	p.mu.Lock()
	err2 := p.reclaimIdleLeaseLocked()
	p.mu.Unlock()
	_ = err
	if err2 == nil {
		t.Fatal("expected a lease_limit refusal")
	}
	msg := err2.Error()
	for _, want := range []string{"lease_limit", "tools_release"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("refusal %q does not mention %q, so a caller cannot act on it", msg, want)
		}
	}
	if errors.Is(err2, ErrApproval) {
		t.Fatal("lease_limit must not be reported as an approval problem")
	}
}

// leaseRoot returns a directory to use as a lease root.
//
// It deliberately does NOT use t.TempDir(). These roots are only lease keys —
// acquire never makes them a working directory — but a backend child that was
// started with a temp path as cwd can still hold a handle on Windows for a short
// while after Close, and testing.TempDir fails the test when RemoveAll collides
// with that. Losing an empty directory in /tmp is harmless; failing a passing
// test is not.
func leaseRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "rd-lease-root-")
	if err != nil {
		t.Fatalf("create lease root: %v", err)
	}
	t.Cleanup(func() {
		// Best effort: a straggling child handle is not a test failure.
		_ = os.RemoveAll(dir)
	})
	return dir
}
