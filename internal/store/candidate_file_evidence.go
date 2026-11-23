package store

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	projectpkg "recalldock.local/recalldock/internal/project"
)

const candidateEvidenceFileLimit = 10 << 20

type candidateEvidenceRegistry struct {
	tx      *sql.Tx
	storeID string
}

func (r candidateEvidenceRegistry) ResolveProjectBinding(marker projectpkg.RootMarker, root string) (projectpkg.DetectionResult, error) {
	return resolveProjectBindingRows(r.tx, r.storeID, marker, root)
}

func (s *Store) verifyCandidateFileEvidenceTx(tx *sql.Tx, project string, ref CandidateEvidenceRef) error {
	if !strings.HasPrefix(ref.Ref, "file://") || len(ref.Digest) != 64 {
		return ErrCandidateEvidenceUnverified
	}
	cut := strings.LastIndex(ref.Ref, "@sha256:")
	if cut < 0 {
		return ErrCandidateEvidenceUnverified
	}
	expected := strings.ToLower(ref.Ref[cut+len("@sha256:"):])
	if len(expected) != 64 || expected != strings.ToLower(ref.Digest) {
		return ErrCandidateEvidenceUnverified
	}
	// Registration is operator-owned metadata. Query it before any filesystem
	// access, so an arbitrary model-supplied path cannot become a file read.
	var found int
	err := tx.QueryRow(`SELECT 1 FROM resource_dependencies d JOIN observations o ON o.sync_id=d.dependent_observation_sync_id WHERE d.project=? AND d.source_ref=? AND d.status='active' AND o.project=d.project AND o.deleted_at IS NULL AND o.archived_at IS NULL AND o.stale_at IS NULL AND o.summary_state != 'stale' AND `+validObservationSourcesSQL("o")+` LIMIT 1`, project, ref.Ref).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrCandidateEvidenceUnverified
	}
	if err != nil {
		return err
	}
	path, ok := parseFileSourceRefPath(ref.Ref[:cut])
	if !ok {
		return ErrCandidateEvidenceUnverified
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil || !samePath(real, path) {
		return ErrCandidateEvidenceUnverified
	}
	var storeID string
	if err := tx.QueryRow("SELECT value FROM store_metadata WHERE key='store_id'").Scan(&storeID); err != nil {
		return err
	}
	scope := projectpkg.DetectExplicitWithRegistry(filepath.Dir(real), candidateEvidenceRegistry{tx: tx, storeID: storeID})
	if scope.Error != nil || scope.Source != projectpkg.SourceRegistered || scope.ProjectID != project {
		return ErrCandidateEvidenceUnverified
	}
	f, err := openBoundCandidateFile(tx, scope, real)
	if err != nil {
		return ErrCandidateEvidenceUnverified
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Size() > candidateEvidenceFileLimit {
		return ErrCandidateEvidenceUnverified
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(f, candidateEvidenceFileLimit+1))
	if err != nil || n > candidateEvidenceFileLimit || fmt.Sprintf("%x", hash.Sum(nil)) != expected {
		return ErrCandidateEvidenceUnverified
	}
	return nil
}
