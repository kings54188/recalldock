package setup

import (
	"errors"
	"testing"
)

// The retained installers hardcode an upstream Engram payload. They are
// unreachable from the CLI today only because cmd/recalldock/main.go routes
// "setup" to runLocalSetup and returns. This pins the defence in depth: even if
// that dispatcher is reordered, Install refuses rather than fetching
// Gentleman-Programming/engram or npm:gentle-engram.
func TestInstallRefusesLegacyPathByDefault(t *testing.T) {
	previous := legacyInstallerEnabled
	legacyInstallerEnabled = false
	t.Cleanup(func() { legacyInstallerEnabled = previous })

	for _, agent := range supportedSlugs() {
		result, err := Install(agent)
		if !errors.Is(err, ErrLegacyInstallerDisabled) {
			t.Fatalf("Install(%q) error = %v, want ErrLegacyInstallerDisabled", agent, err)
		}
		if result != nil {
			t.Fatalf("Install(%q) returned a result despite refusing", agent)
		}
	}
}

func TestInstallRefusesUnknownAgentOnceEnabled(t *testing.T) {
	previous := legacyInstallerEnabled
	legacyInstallerEnabled = true
	t.Cleanup(func() { legacyInstallerEnabled = previous })

	if _, err := Install("no-such-agent"); err == nil {
		t.Fatal("unknown agent accepted")
	} else if errors.Is(err, ErrLegacyInstallerDisabled) {
		t.Fatal("unknown agent reported as the disabled-installer error")
	}
}

// Guards the constants so a rename cannot silently re-point the legacy payload
// at something else without a reviewer noticing.
func TestLegacyUpstreamPayloadConstants(t *testing.T) {
	if claudeCodeMarketplace != "Gentleman-Programming/engram" {
		t.Skipf("upstream marketplace constant changed to %q; update this guard", claudeCodeMarketplace)
	}
	if codexMarketplace != claudeCodeMarketplace {
		t.Fatalf("codex marketplace %q diverged from claude marketplace %q", codexMarketplace, claudeCodeMarketplace)
	}
	if piGentleEngramPackage == "" {
		t.Fatal("pi legacy package constant unexpectedly empty")
	}
}
