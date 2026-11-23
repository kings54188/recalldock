//go:build !linux && !windows

package store

import (
	"database/sql"
	"os"
	projectpkg "recalldock.local/recalldock/internal/project"
)

// Verified bindings and race-safe file evidence are currently Linux-only.
func openBoundCandidateFile(_ *sql.Tx, _ projectpkg.DetectionResult, _ string) (*os.File, error) {
	return nil, ErrCandidateEvidenceUnverified
}
