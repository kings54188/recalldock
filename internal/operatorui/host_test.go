package operatorui

import "testing"

// TestLoopbackHost pins the Host gate: only literal loopback names pass, which
// is what prevents DNS-rebinding requests from reaching the operator surface.
func TestLoopbackHost(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "127.0.0.1:7438", "localhost", "localhost:7438", "[::1]:7438", "::1"} {
		if !loopbackHost(host) {
			t.Fatalf("loopback host rejected: %q", host)
		}
	}
	for _, host := range []string{"", "10.0.0.1:7438", "example.com", "127.0.0.1.example.com:80", "127.0.0.1@evil:80", "0.0.0.0:7438"} {
		if loopbackHost(host) {
			t.Fatalf("non-loopback host accepted: %q", host)
		}
	}
}
