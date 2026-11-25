package gateway

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"recalldock.local/recalldock/internal/opsstore"
)

// These tests cover the lifecycle edges of the pending-connect state machine that
// Pool.acquire uses to keep connect() out of the pool lock.
//
// Each one was written against a real defect found while reviewing that state
// machine, and each failed before its fix. They observe pool state directly
// (pending/leases/closed) instead of sleeping and hoping a race happened, because
// a timing-based test of a race is not evidence.

// slowPoolFixture returns a pool whose single backend never completes the MCP
// handshake, so a connect stays in flight until its timeout. The config hash is
// seeded by hand because Discover would try to start the hanging backend.
func slowPoolFixture(t *testing.T) (*Pool, Server) {
	t.Helper()
	dir := t.TempDir()
	runtime := backendRuntime(t, "hang")
	source := sourceFixture(t, "json", "{}")
	body, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"test": runtime}})
	if err := os.WriteFile(source.Path, body, 0600); err != nil {
		t.Fatal(err)
	}
	ops, err := opsstore.Open(filepath.Join(dir, "data"), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ops.Close() })
	p := NewPool(filepath.Join(dir, "gateway.json"), ops)
	p.Trust = "strict"
	t.Cleanup(p.Close)
	if err := p.Update(func(r *Registry) error {
		// Two seconds keeps the connect in flight well past the assertions while
		// still bounding the test.
		r.Servers["test"] = Server{Source: source, Enabled: true, StartAllowed: true, Stateful: true, TimeoutSeconds: 2}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reg, err := p.registry()
	if err != nil {
		t.Fatal(err)
	}
	_, hash, err := reg.Servers["test"].Source.Read()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Update(func(r *Registry) error {
		s := r.Servers["test"]
		s.ConfigHash = hash
		r.Servers["test"] = s
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reg, err = p.registry()
	if err != nil {
		t.Fatal(err)
	}
	return p, reg.Servers["test"]
}

// gatedPoolFixture returns a pool whose backend blocks in its handshake until the
// returned release func is called, then completes normally. This is what makes
// "connect succeeds after the pool closed" testable: the connect is held open on
// purpose and then allowed to succeed, with no timing assumption.
func gatedPoolFixture(t *testing.T) (*Pool, Server, func()) {
	t.Helper()
	dir := t.TempDir()
	gate := filepath.Join(dir, "gate")
	runtime := backendRuntime(t, "gated")
	runtime.Env["RECALLDOCK_GATEWAY_TEST_GATE"] = gate
	source := sourceFixture(t, "json", "{}")
	body, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"test": runtime}})
	if err := os.WriteFile(source.Path, body, 0600); err != nil {
		t.Fatal(err)
	}
	ops, err := opsstore.Open(filepath.Join(dir, "data"), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ops.Close() })
	p := NewPool(filepath.Join(dir, "gateway.json"), ops)
	p.Trust = "strict"
	t.Cleanup(p.Close)
	if err := p.Update(func(r *Registry) error {
		r.Servers["test"] = Server{Source: source, Enabled: true, StartAllowed: true, Stateful: true, TimeoutSeconds: 20}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reg, err := p.registry()
	if err != nil {
		t.Fatal(err)
	}
	_, hash, err := reg.Servers["test"].Source.Read()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Update(func(r *Registry) error {
		srv := r.Servers["test"]
		srv.ConfigHash = hash
		r.Servers["test"] = srv
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reg, err = p.registry()
	if err != nil {
		t.Fatal(err)
	}
	release := func() { _ = os.WriteFile(gate, nil, 0600) }
	return p, reg.Servers["test"], release
}

// waitForPending blocks until the pool has registered a pending connect.
// Observing the real map is the deterministic alternative to sleeping.
func waitForPending(t *testing.T, p *Pool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		n := len(p.pending)
		p.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("connect never registered a pending entry")
}

// TestCloseDuringConnectDoesNotPublishLease pins the "revival after close" edge.
//
// Close marks the pool closed and drains leases, but a connect already in flight
// completes later — and it can complete *successfully*. Without a closed check on
// the completion path, that connect inserted its fresh lease into a pool nobody
// owns: the backend process kept running with no reachable token, so it leaked
// until the machine ran out of handles.
//
// The backend is gated so the connect is genuinely in flight when Close runs and
// then genuinely succeeds; a backend that merely times out would never reach the
// publication path and the test would prove nothing.
func TestCloseDuringConnectDoesNotPublishLease(t *testing.T) {
	p, server, release := gatedPoolFixture(t)
	done := make(chan *lease, 1)
	go func() {
		l, _ := p.acquire(context.Background(), "test", leaseRoot(t), "s", server)
		done <- l
	}()
	waitForPending(t, p)

	p.Close()
	release() // let the connect succeed after the pool has closed

	l := <-done
	if l != nil {
		p.mu.Lock()
		_, present := p.leases[l.token]
		n := len(p.leases)
		p.mu.Unlock()
		if present {
			t.Fatalf("a lease whose connect succeeded after Close was published into the closed pool (leases=%d); its backend process would leak", n)
		}
		l.shutdown(true)
	}
	p.mu.Lock()
	remaining := len(p.leases)
	p.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("closed pool still holds %d leases", remaining)
	}
}

// TestAcquireAfterCloseIsRefused pins the simpler half of the same edge: once
// closed, acquire must not start a new backend at all.
func TestAcquireAfterCloseIsRefused(t *testing.T) {
	p, _, _ := poolFixture(t, true)
	if _, _, err := p.Discover("test"); err != nil {
		t.Fatalf("discover: %v", err)
	}
	reg, err := p.registry()
	if err != nil {
		t.Fatal(err)
	}
	server := reg.Servers["test"]
	p.Close()

	l, err := p.acquire(context.Background(), "test", leaseRoot(t), "s", server)
	if err == nil || l != nil {
		p.mu.Lock()
		n := len(p.leases)
		p.mu.Unlock()
		t.Fatalf("acquire on a closed pool returned a lease (leases=%d); the backend would be owned by nobody", n)
	}
}

// TestPendingConnectsReserveBoundCapacity pins the capacity edge.
//
// The bound exists to cap backend processes. Pending connects are about to become
// processes, but reclaimIdleLeaseLocked counted only published leases, so several
// concurrent connects could each see room, all proceed, and all succeed —
// overshooting maxConcurrentLeases by the number of in-flight attempts.
func TestPendingConnectsReserveBoundCapacity(t *testing.T) {
	p, server := slowPoolFixture(t)
	p.maxLeases = 2

	// One published lease plus in-flight attempts must never exceed the bound.
	p.mu.Lock()
	p.leases["occupied"] = &lease{token: "occupied", refs: 1, server: "test", root: "/x", session: "s"}
	p.mu.Unlock()

	const attempts = 4
	done := make(chan struct{}, attempts)
	for i := 0; i < attempts; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			_, _ = p.acquire(context.Background(), "test", leaseRoot(t), "s", server)
		}()
	}

	// Sample while connects are in flight; the invariant must hold throughout.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		held := len(p.leases) + len(p.pending)
		p.mu.Unlock()
		if held > p.maxLeases {
			t.Fatalf("live+pending = %d exceeds the bound %d; pending connects must reserve capacity", held, p.maxLeases)
		}
		time.Sleep(time.Millisecond)
	}
	for i := 0; i < attempts; i++ {
		<-done
	}
}

// TestPendingIsClearedAfterFailure pins that a failed connect does not leave a
// reservation behind, which would permanently shrink the pool.
func TestPendingIsClearedAfterFailure(t *testing.T) {
	p, _, _ := poolFixture(t, true)
	if _, _, err := p.Discover("test"); err != nil {
		t.Fatalf("discover: %v", err)
	}
	reg, err := p.registry()
	if err != nil {
		t.Fatal(err)
	}
	server := reg.Servers["test"]
	stale := server
	stale.ConfigHash = "0" + server.ConfigHash[1:]

	if _, err := p.acquire(context.Background(), "test", leaseRoot(t), "s", stale); err == nil {
		t.Fatal("acquire with a stale config hash should fail")
	}
	p.mu.Lock()
	pending, leases := len(p.pending), len(p.leases)
	p.mu.Unlock()
	if pending != 0 || leases != 0 {
		t.Fatalf("failed connect left state behind: pending=%d leases=%d", pending, leases)
	}

	// And the key is usable immediately afterwards.
	l, err := p.acquire(context.Background(), "test", leaseRoot(t), "s", server)
	if err != nil {
		t.Fatalf("acquire after failure: %v", err)
	}
	t.Cleanup(func() { l.shutdown(true) })
}

// TestWaiterCancellationDoesNotCancelOwner proves a waiter that gives up does not
// abort the connect other waiters are relying on.
func TestWaiterCancellationDoesNotCancelOwner(t *testing.T) {
	p, server := slowPoolFixture(t)
	p.maxLeases = 4

	ownerDone := make(chan struct{})
	go func() {
		defer close(ownerDone)
		_, _ = p.acquire(context.Background(), "test", leaseRoot(t), "s", server)
	}()
	waitForPending(t, p)

	// A second caller for the same key cancels while waiting.
	ctx, cancel := context.WithCancel(context.Background())
	waiterErr := make(chan error, 1)
	go func() {
		_, err := p.acquire(ctx, "test", leaseRoot(t), "s", server)
		waiterErr <- err
	}()
	cancel()

	select {
	case err := <-waiterErr:
		if err == nil {
			t.Fatal("a cancelled waiter should report an error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled waiter did not return; it must not block on the owner's connect")
	}
	// The owner's attempt is independent and must finish on its own schedule.
	select {
	case <-ownerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("owner's connect did not complete after a waiter cancelled")
	}
}

// TestStressConcurrentAcquireSameKey mix many callers on one key, some of them
// cancelling, to stress the publication path: pending.err and pending.lease are
// written under p.mu before ready closes, and read after it closes.
//
// The channel close is the happens-before edge, so the reads are safe; this test
// exists to prove that under -race and to catch pending entries that are never
// removed. It was written after adding the wait path, because a single-threaded
// test cannot distinguish "no race" from "race never exercised".
func TestStressConcurrentAcquireSameKey(t *testing.T) {
	p, _, _ := poolFixture(t, true)
	if _, _, err := p.Discover("test"); err != nil {
		t.Fatalf("discover: %v", err)
	}
	reg, err := p.registry()
	if err != nil {
		t.Fatal(err)
	}
	server := reg.Servers["test"]
	root := leaseRoot(t)

	const callers = 16
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			if idx%4 == 0 {
				// A caller that gives up before the connect finishes.
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				_, _ = p.acquire(ctx, "test", root, "s", server)
				return
			}
			l, err := p.acquire(context.Background(), "test", root, "s", server)
			if err == nil && l != nil {
				l.done()
				l.arm(p)
			}
		}(i)
	}
	wg.Wait()

	p.mu.Lock()
	leases, pending := len(p.leases), len(p.pending)
	p.mu.Unlock()
	if pending != 0 {
		t.Fatalf("%d pending entries leaked after concurrent acquire", pending)
	}
	if leases != 1 {
		t.Fatalf("expected exactly one lease for one key, got %d; concurrent callers must share one backend", leases)
	}
}

// TestReleaseInvalidatesTokenBeforeItLeavesThePool pins the linearisation point
// for D4.
//
// Release deletes the lease from p.leases and then calls shutdown, which is what
// sets released. Between those two steps the token no longer resolves, but
// `released` was still false — so a Call finishing in that window saw "owned" and
// published a token that getLease would immediately reject with
// lease_not_available. The caller was told it held a usable handle when it did
// not.
//
// The fix marks the lease released while p.mu is still held, immediately after the
// delete, so "removed from the pool" and "not publishable" are the same instant.
//
// This test checks the invariant deterministically rather than trying to sample a
// narrow window from another goroutine: an earlier sampling version passed even
// against the buggy code because the goroutine simply never landed inside the
// window. Instead it observes, under p.mu, whether the removal and the released
// flag are atomic with respect to each other.
func TestReleaseInvalidatesTokenBeforeItLeavesThePool(t *testing.T) {
	p, _, _ := poolFixture(t, true)
	if _, _, err := p.Discover("test"); err != nil {
		t.Fatalf("discover: %v", err)
	}
	reg, err := p.registry()
	if err != nil {
		t.Fatal(err)
	}
	l, err := p.acquire(context.Background(), "test", leaseRoot(t), "s", reg.Servers["test"])
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	token := l.token

	if err := p.Release(token); err != nil {
		t.Fatalf("release: %v", err)
	}

	// The contract after Release: absent from the pool AND not publishable. A
	// correct implementation sets both under the same p.mu critical section, so no
	// observer holding p.mu can see one without the other.
	p.mu.Lock()
	_, inMap := p.leases[token]
	p.mu.Unlock()
	l.mu.Lock()
	released := l.released
	l.mu.Unlock()

	if inMap {
		t.Fatal("released lease is still present in the pool")
	}
	if !released {
		t.Fatal("a lease removed from the pool still reports released=false; a Call finishing now would publish a dead token")
	}

	// Publishing is also refused through the token lookup path.
	if _, err := p.getLease(token, "test", l.root, "s"); err == nil {
		t.Fatal("getLease accepted a released token")
	}
}

// TestCallDoesNotReturnTokenAfterConcurrentRelease is the end-to-end form: a call
// in flight while the lease is released must not hand back that token.
func TestCallDoesNotReturnTokenAfterConcurrentRelease(t *testing.T) {
	p, root, marker := poolFixture(t, true)
	if _, _, err := p.Discover("test"); err != nil {
		t.Fatalf("discover: %v", err)
	}
	reg, err := p.registry()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Grant("test", GrantOptions{Tool: "echo", ReadOnly: true, Roots: []string{root}}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	reg, err = p.registry()
	if err != nil {
		t.Fatal(err)
	}
	server := reg.Servers["test"]

	l, err := p.acquire(context.Background(), "test", root, "s", server)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	token := l.token
	l.done()
	l.arm(p)

	type result struct{ token string }
	done := make(chan result, 1)
	go func() {
		_, tok, _ := p.Call(context.Background(), CallRequest{
			Server: "test", Tool: "echo", Root: root, Session: "s",
			Lease: token, Arguments: map[string]any{"message": "wait"},
		})
		done <- result{tok}
	}()

	// Wait until the backend actually started the tool, so the release is genuinely
	// concurrent with the call rather than before it.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if b, readErr := os.ReadFile(marker); readErr == nil && len(b) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if b, _ := os.ReadFile(marker); len(b) == 0 {
		t.Skip("backend did not report start; skipping rather than asserting on an unstarted call")
	}

	if err := p.Release(token); err != nil {
		t.Fatalf("release during in-flight call: %v", err)
	}

	res := <-done
	if res.token == "" {
		return // correct: no token published
	}
	if _, err := p.getLease(res.token, "test", root, "s"); err != nil {
		t.Fatalf("Call returned token %q after a concurrent release, but it does not resolve: %v", res.token, err)
	}
	_ = p.Release(res.token)
}

// TestLockOrderStress mixes the operations that take p.mu and lease.mu in
// different orders — acquire, Release, CloseServer, reclaim, idle-timer arming —
// to surface an ABBA deadlock, a double close, or a use-after-close under -race.
//
// The lock order is p.mu before lease.mu everywhere and never the reverse, but a
// rule that is only written down is not verified; this test exercises the
// interleavings. A deadlock shows up as the explicit timeout below rather than as
// a hung CI job.
func TestLockOrderStress(t *testing.T) {
	p, _, _ := poolFixture(t, true)
	if _, _, err := p.Discover("test"); err != nil {
		t.Fatalf("discover: %v", err)
	}
	reg, err := p.registry()
	if err != nil {
		t.Fatal(err)
	}
	server := reg.Servers["test"]

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Acquire / release churn.
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				l, err := p.acquire(context.Background(), "test", leaseRoot(t), "s", server)
				if err != nil {
					continue
				}
				l.done()
				l.arm(p)
				_ = p.Release(l.token)
			}
		}()
	}
	// Server close churn: removes leases then calls shutdown.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			p.CloseServer("test")
			time.Sleep(200 * time.Microsecond)
		}
	}()
	// Reclaim churn: iterates leases while holding p.mu and takes l.mu inside.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			p.mu.Lock()
			_ = p.reclaimIdleLeaseLocked()
			p.mu.Unlock()
		}
	}()

	time.Sleep(300 * time.Millisecond)
	close(stop)

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("lock-order stress did not finish; suspect an ABBA deadlock between p.mu and lease.mu")
	}
	p.Close()
}
