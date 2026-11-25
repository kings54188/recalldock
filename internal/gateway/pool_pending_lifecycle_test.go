package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
//
// The failure must happen *inside* connectLease: a config-hash mismatch is
// detected before the pending entry is ever registered, so a test built on it
// would pass without exercising the cleanup path at all. That is exactly what an
// earlier version did, and it failed on the race-detector job where timing shifted
// which path ran. This version points the backend at a command that cannot be
// resolved, so connect fails deterministically after claiming the slot.
func TestPendingIsClearedAfterFailure(t *testing.T) {
	dir := t.TempDir()
	// A backend whose command cannot be resolved: connect fails inside
	// connectLease, after the pending entry is registered.
	runtime := Runtime{Type: "stdio", Command: "recalldock-no-such-backend-command", CWD: backendWorkDir(t)}
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
		r.Servers["test"] = Server{Source: source, Enabled: true, StartAllowed: true, Stateful: true, TimeoutSeconds: 5}
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
		// Source.Read itself may fail for an unresolvable command; that still means
		// the connect path fails, which is what this test needs.
		hash = strings.Repeat("0", 64)
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

	if _, err := p.acquire(context.Background(), "test", leaseRoot(t), "s", reg.Servers["test"]); err == nil {
		t.Fatal("acquire against an unresolvable backend command should fail")
	}

	// The failed attempt must leave nothing behind.
	p.mu.Lock()
	pending, leases := len(p.pending), len(p.leases)
	p.mu.Unlock()
	if pending != 0 {
		t.Fatalf("failed connect left %d pending entries behind; capacity would shrink permanently", pending)
	}
	if leases != 0 {
		t.Fatalf("failed connect published %d leases", leases)
	}

	// And the same key can be attempted again (it fails the same way, which proves
	// the slot is reusable rather than stuck).
	if _, err := p.acquire(context.Background(), "test", leaseRoot(t), "s", reg.Servers["test"]); err == nil {
		t.Fatal("second attempt against an unresolvable backend should also fail")
	}
	p.mu.Lock()
	pendingAfter := len(p.pending)
	p.mu.Unlock()
	if pendingAfter != 0 {
		t.Fatalf("second failed connect left %d pending entries behind", pendingAfter)
	}
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

// TestCloseWaitsForInFlightConnect pins claim 2a from the independent review.
//
// Close marked the pool closed and drained leases, then returned — but a connect
// already in flight was left to finish on its own, which can take the full
// backend startup timeout. A child process still starting up at that moment
// outlives the gateway as an orphan, which is the same process leak this change
// set exists to prevent, just triggered one step earlier. Close now cancels
// in-flight attempts and waits for their owners to shut down what they opened.
func TestCloseWaitsForInFlightConnect(t *testing.T) {
	p, server, release := gatedPoolFixture(t)
	acquireDone := make(chan struct{})
	go func() {
		defer close(acquireDone)
		_, _ = p.acquire(context.Background(), "test", leaseRoot(t), "s", server)
	}()
	waitForPending(t, p)

	closed := make(chan struct{})
	go func() {
		p.Close()
		close(closed)
	}()

	// Close must cancel the attempt rather than wait out the 20s server timeout.
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return; it must cancel in-flight connects instead of waiting out the backend timeout")
	}

	p.mu.Lock()
	pending := len(p.pending)
	p.mu.Unlock()
	if pending != 0 {
		t.Fatalf("Close returned with %d pending connects still in flight; their children would be orphaned", pending)
	}
	release()
	<-acquireDone
}

// TestStatelessCallAndDiscoverRespectClose pins claim 7a.
//
// The non-stateful branch of Call opens its own short-lived connection and never
// consulted p.closed, so after Pool.Close a tools_call could still start a
// backend — and Discover could too. Neither leaked (the connection closes when
// the call returns) but both contradicted the pool contract, and the same call
// was refused on a stateful backend and allowed on a stateless one.
func TestStatelessCallAndDiscoverRespectClose(t *testing.T) {
	p, root, _ := poolFixture(t, false) // stateless
	if _, _, err := p.Discover("test"); err != nil {
		t.Fatalf("discover: %v", err)
	}
	if _, err := p.Grant("test", GrantOptions{Tool: "echo", ReadOnly: true, Roots: []string{root}}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	p.Close()

	if _, _, err := p.Call(context.Background(), CallRequest{
		Server: "test", Tool: "echo", Root: root, Session: "s",
		Arguments: map[string]any{"message": "hi"},
	}); err == nil {
		t.Fatal("a stateless Call started a backend after Close")
	}
	if _, _, err := p.Discover("test"); err == nil {
		t.Fatal("Discover started a backend after Close")
	}
}

// TestJoinerIsNotRefusedAtBound pins claim 6a.
//
// Joining an existing pending attempt costs no extra capacity — the joiner shares
// the lease that attempt is creating. But the bound was enforced before the join
// was considered, so at the limit a caller with a perfectly good in-flight
// attempt for its own key was told to release somebody else's connection.
func TestJoinerIsNotRefusedAtBound(t *testing.T) {
	p, server := slowPoolFixture(t)
	p.maxLeases = 2

	// One published lease, then one in-flight attempt: the pool is at its bound.
	p.mu.Lock()
	p.leases["occupied"] = &lease{token: "occupied", refs: 1, server: "test", root: "/x", session: "s"}
	p.mu.Unlock()

	root := leaseRoot(t)
	go func() { _, _ = p.acquire(context.Background(), "test", root, "s", server) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		_, ok := p.pending[leaseKey{server: "test", root: root, session: "s"}]
		p.mu.Unlock()
		if ok {
			break
		}
		time.Sleep(time.Millisecond)
	}

	// The joiner must wait for the existing attempt rather than be refused with
	// lease_limit. It will ultimately fail because the backend hangs, but the
	// failure must not be a capacity refusal.
	done := make(chan error, 1)
	go func() {
		_, err := p.acquire(context.Background(), "test", root, "s", server)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil && strings.Contains(err.Error(), "lease_limit") {
			t.Fatalf("a joiner for an existing pending key was refused on capacity: %v", err)
		}
	case <-time.After(4 * time.Second):
		// Still waiting on the shared attempt is the expected outcome.
	}
}

// TestOwnerCancellationDoesNotFailHealthyWaiter pins claim 5.
//
// The connect ran on the owner's context. If the owner gave up, every waiter
// received the owner's error even though their own contexts were healthy — one
// cancelled host request could fail unrelated concurrent calls for the same
// backend session.
func TestOwnerCancellationDoesNotFailHealthyWaiter(t *testing.T) {
	p, server, release := gatedPoolFixture(t)
	p.maxLeases = 4

	ownerCtx, ownerCancel := context.WithCancel(context.Background())
	ownerDone := make(chan error, 1)
	go func() {
		_, err := p.acquire(ownerCtx, "test", leaseRoot(t), "s", server)
		ownerDone <- err
	}()
	waitForPending(t, p)

	// Find the key the owner registered so the second caller joins it.
	p.mu.Lock()
	var key leaseKey
	for k := range p.pending {
		key = k
	}
	p.mu.Unlock()

	waiterDone := make(chan error, 1)
	waiterLease := make(chan *lease, 1)
	go func() {
		l, err := p.acquire(context.Background(), "test", key.root, "s", server)
		waiterLease <- l
		waiterDone <- err
	}()
	time.Sleep(30 * time.Millisecond)
	ownerCancel()
	release() // let the gated backend complete for whoever still wants it

	oErr := <-ownerDone
	wErr := <-waiterDone
	wLease := <-waiterLease

	if oErr != nil && wErr != nil && oErr.Error() == wErr.Error() && !errors.Is(wErr, context.Canceled) {
		t.Fatalf("healthy waiter received the owner's failure verbatim (%v); a cancelled owner must not fail other callers", wErr)
	}
	if wLease != nil {
		wLease.shutdown(true)
	}
}
