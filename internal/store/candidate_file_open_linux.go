//go:build linux

package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
	projectpkg "recalldock.local/recalldock/internal/project"
)

// openBoundCandidateFile pins the registered root and opens every path element
// without following symlinks or crossing mount points. On kernels lacking
// openat2, evidence is refused rather than using a race-prone fallback.
func openBoundCandidateFile(tx *sql.Tx, scope projectpkg.DetectionResult, path string) (*os.File, error) {
	var registeredPath, device, inode, state string
	if err := tx.QueryRow(`SELECT root_path,device,inode,state FROM project_bindings WHERE id=? AND project_id=?`, scope.BindingID, scope.ProjectID).Scan(&registeredPath, &device, &inode, &state); err != nil {
		return nil, err
	}
	if state != "active" || filepath.Clean(scope.Path) != registeredPath {
		return nil, ErrCandidateEvidenceUnverified
	}
	relative, err := filepath.Rel(registeredPath, path)
	if err != nil || relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, ErrCandidateEvidenceUnverified
	}
	parts := strings.Split(relative, string(filepath.Separator))
	rootFD, err := unix.Open(registeredPath, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(rootFD)
	var st unix.Stat_t
	if err = unix.Fstat(rootFD, &st); err != nil {
		return nil, err
	}
	if fmt.Sprint(st.Dev) != device || fmt.Sprint(st.Ino) != inode {
		return nil, ErrCandidateEvidenceUnverified
	}
	resolve := candidateResolveFlags
	if err := verifyCandidateRootMarker(tx, rootFD, scope, resolve); err != nil {
		return nil, err
	}
	current := rootFD
	for _, part := range parts[:len(parts)-1] {
		if part == "" || part == "." || part == ".." {
			if current != rootFD {
				unix.Close(current)
			}
			return nil, ErrCandidateEvidenceUnverified
		}
		child, openErr := openCandidateDirectory(current, part)
		if current != rootFD {
			unix.Close(current)
		}
		if openErr != nil {
			return nil, openErr
		}
		current = child
		// A nested repository or RecallDock declaration is a separate scope. Fail
		// closed if one was introduced after the initial path-based detection.
		for _, name := range []string{".git", ".recalldock"} {
			var entry unix.Stat_t
			e := unix.Fstatat(current, name, &entry, unix.AT_SYMLINK_NOFOLLOW)
			if e == nil {
				unix.Close(current)
				return nil, ErrCandidateEvidenceUnverified
			}
			if !errors.Is(e, unix.ENOENT) {
				unix.Close(current)
				return nil, e
			}
		}
	}
	if current != rootFD {
		defer unix.Close(current)
	}
	name := parts[len(parts)-1]
	if name == "" || name == "." || name == ".." {
		return nil, ErrCandidateEvidenceUnverified
	}
	fd, err := unix.Openat2(current, name, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_NONBLOCK | unix.O_CLOEXEC, Resolve: resolve})
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

const candidateResolveFlags = uint64(unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV)

func openCandidateDirectory(parentFD int, name string) (int, error) {
	return unix.Openat2(parentFD, name, &unix.OpenHow{Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: candidateResolveFlags})
}

// Read the marker through the same pinned directory used for the evidence;
// path-based detection alone could have observed a different root after a swap.
func verifyCandidateRootMarker(tx *sql.Tx, rootFD int, scope projectpkg.DetectionResult, resolve uint64) error {
	fd, err := unix.Openat2(rootFD, ".recalldock/root.json", &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_NONBLOCK | unix.O_CLOEXEC, Resolve: resolve})
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), "project root marker")
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
		return ErrCandidateEvidenceUnverified
	}
	var marker projectpkg.RootMarker
	decoder := json.NewDecoder(io.LimitReader(f, 4097))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&marker); err != nil {
		return ErrCandidateEvidenceUnverified
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF || marker.Version != 1 {
		return ErrCandidateEvidenceUnverified
	}
	var storeID string
	if err := tx.QueryRow("SELECT value FROM store_metadata WHERE key='store_id'").Scan(&storeID); err != nil {
		return err
	}
	if marker.StoreID != storeID || marker.ProjectID != scope.ProjectID || marker.BindingID != scope.BindingID {
		return ErrCandidateEvidenceUnverified
	}
	return nil
}
