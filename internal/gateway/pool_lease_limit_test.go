package gateway

import (
	"strings"
	"testing"
)

// Leases are keyed by (server, root, session), so a caller that varies root can
// start one backend process per root. Grants were already capped at 512 per
// server; leases had no bound at all. These tests pin reclaim-then-refuse
// without spawning hundreds of real processes.
func TestReclaimIdleLeasesLockedDropsReleasedLeases(t *testing.T) {
	p := NewPool(t.TempDir()+"/gw.json", nil)
	p.mu.Lock()

	for i := 0; i < maxConcurrentLeases; i++ {
		token := "lease-released-" + itoa(i)
		p.leases[token] = &lease{token: token, released: true}
	}
	closing, err := p.reclaimIdleLeasesLocked()
	p.mu.Unlock()
	for _, l := range closing {
		l.shutdown(true)
	}
	if err != nil {
		t.Fatalf("reclaim of released leases failed: %v", err)
	}
	if len(p.leases) != 0 {
		t.Fatalf("released leases not reclaimed, %d remain", len(p.leases))
	}
}

func TestReclaimIdleLeasesLockedRefusesWhenAllReferenced(t *testing.T) {
	p := NewPool(t.TempDir()+"/gw.json", nil)
	p.mu.Lock()

	// Every lease is actively referenced, so none may be dropped: dropping one
	// would break an in-flight tools_call. The pool must refuse instead.
	for i := 0; i < maxConcurrentLeases; i++ {
		token := "lease-busy-" + itoa(i)
		p.leases[token] = &lease{token: token, refs: 1}
	}
	closing, err := p.reclaimIdleLeasesLocked()
	p.mu.Unlock()
	for _, l := range closing {
		l.shutdown(true)
	}
	if err == nil {
		t.Fatal("pool at its bound with all leases referenced did not refuse")
	}
	if !strings.Contains(err.Error(), "lease_limit") {
		t.Fatalf("error lacks lease_limit marker: %v", err)
	}
	if len(p.leases) != maxConcurrentLeases {
		t.Fatalf("referenced leases were dropped: %d remain", len(p.leases))
	}
}

func TestReclaimIdleLeasesLockedNoopBelowBound(t *testing.T) {
	p := NewPool(t.TempDir()+"/gw.json", nil)
	p.mu.Lock()
	p.leases["lease-one"] = &lease{token: "lease-one", refs: 1}
	closing, err := p.reclaimIdleLeasesLocked()
	p.mu.Unlock()
	for _, l := range closing {
		l.shutdown(true)
	}
	if err != nil {
		t.Fatalf("reclaim below bound failed: %v", err)
	}
	if len(p.leases) != 1 {
		t.Fatalf("reclaim below bound changed the pool: %d", len(p.leases))
	}
}

func TestLeaseShutdownToleratesNilConnection(t *testing.T) {
	// A lease that never finished acquiring has no connection. Shutdown used to
	// dereference it and panic.
	l := &lease{token: "lease-nil"}
	l.shutdown(true)
	l.done()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
