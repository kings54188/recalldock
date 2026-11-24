package setup

import (
	"os"
	"testing"
)

// TestMain enables the retained legacy installer for this package's tests.
//
// The installers are unreachable in production (Install returns
// ErrLegacyInstallerDisabled) because they still carry an upstream Engram
// payload, but their content-generation logic — TOML/JSON/JSONC patching,
// marker blocks, idempotency — is worth keeping under test: those helpers are
// what a future RecallDock-branded installer would reuse, and breaking them
// silently is the likelier regression.
func TestMain(m *testing.M) {
	legacyInstallerEnabled = true
	os.Exit(m.Run())
}
