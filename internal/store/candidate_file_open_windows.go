//go:build windows

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

	"golang.org/x/sys/windows"
	projectpkg "recalldock.local/recalldock/internal/project"
)

func openRegularNoReparseWindows(path string) (*os.File, windows.ByHandleFileInformation, error) {
	var info windows.ByHandleFileInformation
	if isUNCPath(path) {
		return nil, info, ErrCandidateEvidenceUnverified
	}
	ptr, err := windows.UTF16PtrFromString(extendedWinPath(path))
	if err != nil {
		return nil, info, err
	}
	handle, err := windows.CreateFile(
		ptr,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, info, err
	}
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, info, err
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		_ = windows.CloseHandle(handle)
		return nil, info, ErrCandidateEvidenceUnverified
	}
	return os.NewFile(uintptr(handle), path), info, nil
}

func openBoundCandidateFile(tx *sql.Tx, scope projectpkg.DetectionResult, path string) (*os.File, error) {
	var registeredPath, device, inode, state string
	if err := tx.QueryRow(`SELECT root_path,device,inode,state FROM project_bindings WHERE id=? AND project_id=?`, scope.BindingID, scope.ProjectID).Scan(&registeredPath, &device, &inode, &state); err != nil {
		return nil, err
	}
	if state != "active" || !samePath(scope.Path, registeredPath) || isUNCPath(registeredPath) || isUNCPath(path) {
		return nil, ErrCandidateEvidenceUnverified
	}
	relative, err := filepath.Rel(registeredPath, path)
	if err != nil || relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, ErrCandidateEvidenceUnverified
	}
	rootDev, rootInode, err := directoryIdentity(registeredPath)
	if err != nil || rootDev != device || rootInode != inode {
		return nil, ErrCandidateEvidenceUnverified
	}
	if err := verifyCandidateRootMarkerWindows(tx, registeredPath, scope, device); err != nil {
		return nil, err
	}
	parts := strings.Split(relative, string(filepath.Separator))
	current := registeredPath
	for _, part := range parts[:len(parts)-1] {
		if part == "" || part == "." || part == ".." {
			return nil, ErrCandidateEvidenceUnverified
		}
		current = filepath.Join(current, part)
		dirInfo, err := winDirHandleInfo(current)
		if err != nil || fmt.Sprintf("vol:%08x", dirInfo.VolumeSerialNumber) != device {
			return nil, ErrCandidateEvidenceUnverified
		}
		for _, name := range []string{".git", ".recalldock"} {
			if _, statErr := os.Lstat(filepath.Join(current, name)); statErr == nil {
				return nil, ErrCandidateEvidenceUnverified
			} else if !errors.Is(statErr, os.ErrNotExist) {
				return nil, statErr
			}
		}
	}
	name := parts[len(parts)-1]
	if name == "" || name == "." || name == ".." {
		return nil, ErrCandidateEvidenceUnverified
	}
	f, fileInfo, err := openRegularNoReparseWindows(filepath.Join(current, name))
	if err != nil {
		return nil, err
	}
	if fmt.Sprintf("vol:%08x", fileInfo.VolumeSerialNumber) != device {
		_ = f.Close()
		return nil, ErrCandidateEvidenceUnverified
	}
	return f, nil
}

func verifyCandidateRootMarkerWindows(tx *sql.Tx, registeredPath string, scope projectpkg.DetectionResult, expectedVol string) error {
	hidden := filepath.Join(registeredPath, ".recalldock")
	dirInfo, err := winDirHandleInfo(hidden)
	if err != nil || fmt.Sprintf("vol:%08x", dirInfo.VolumeSerialNumber) != expectedVol {
		return ErrCandidateEvidenceUnverified
	}
	f, fileInfo, err := openRegularNoReparseWindows(filepath.Join(hidden, "root.json"))
	if err != nil {
		return ErrCandidateEvidenceUnverified
	}
	defer f.Close()
	if fmt.Sprintf("vol:%08x", fileInfo.VolumeSerialNumber) != expectedVol {
		return ErrCandidateEvidenceUnverified
	}
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() > 4096 {
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
