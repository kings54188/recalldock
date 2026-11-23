package opsstore

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"recalldock.local/recalldock/internal/worklog"
)

type LedgerImport struct {
	SHA256       string     `json:"sha256"`
	RecordedRoot string     `json:"recorded_root"`
	Generation   int64      `json:"generation"`
	Checkpoint   Checkpoint `json:"checkpoint"`
}

func ReadInput(path string, value any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > DetailLimit {
		return ErrBudget
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, DetailLimit+1))
	if err != nil {
		return err
	}
	if len(body) > DetailLimit {
		return ErrBudget
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		return errors.New("invalid input JSON schema")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return errors.New("trailing JSON input")
	}
	return nil
}

func InspectLedger(path, recordedRoot string) (LedgerImport, error) {
	var result LedgerImport
	info, err := os.Lstat(path)
	if err != nil {
		return result, err
	}
	if !info.Mode().IsRegular() || info.Size() > worklog.FileLimit {
		return result, ErrBudget
	}
	f, err := os.Open(path)
	if err != nil {
		return result, err
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, worklog.FileLimit+1))
	if err != nil {
		return result, err
	}
	cp, generation, err := worklog.DecodeLedger(body, recordedRoot)
	if err != nil {
		return result, err
	}
	result = LedgerImport{SHA256: digest(body), RecordedRoot: recordedRoot, Generation: generation, Checkpoint: Checkpoint{Checkpoint: cp, Status: "active"}}
	return result, nil
}

func (s *Store) ImportLedger(root, lane string, ledger LedgerImport, expectedSHA256 string) (Snapshot, error) {
	if len(expectedSHA256) != 64 || expectedSHA256 != ledger.SHA256 || ledger.Generation < 1 {
		return Snapshot{}, errors.New("ledger digest changed or missing explicit expected SHA256")
	}
	return s.save(root, lane, 0, ledger.Generation, ledger.Checkpoint)
}

func (s *Store) ExportLedger(root, lane, output string) (string, error) {
	if !filepath.IsAbs(output) {
		return "", errors.New("absolute export path required")
	}
	snap, err := s.Read(root, lane, 0)
	if err != nil {
		return "", err
	}
	body, err := worklog.RenderLedger(snap.Root, snap.Generation, snap.Checkpoint.Checkpoint)
	if err != nil {
		return "", err
	}
	// Exports are create-only: replacing a hand-edited projection is never implicit.
	f, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("refusing to overwrite existing ledger %q (exports are create-only; choose a new path)", output)
		}
		return "", fmt.Errorf("create ledger %q: %w", output, err)
	}
	_, err = f.Write(body)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	if err := syncDir(filepath.Dir(output)); err != nil {
		return "", err
	}
	return digest(body), nil
}

func EncodeTasks(page TaskPage, history bool) ([]byte, error) {
	for n := len(page.Tasks); n >= 0; n-- {
		if n == 0 && len(page.Tasks) > 0 {
			return nil, ErrBudget
		}
		selected := page
		selected.Tasks = page.Tasks[:n]
		selected.HasMore = page.HasMore || n < len(page.Tasks)
		selected.After, selected.Before = "", 0
		if selected.HasMore && n > 0 {
			if history {
				selected.Before = selected.Tasks[n-1].Generation
			} else {
				selected.After = taskCursor(selected.Tasks[n-1], selected.Order)
			}
		}
		body, err := encode(selected, "l0", SummaryLimit, n < len(page.Tasks))
		if err == nil {
			return body, nil
		}
		if !errors.Is(err, ErrBudget) {
			return nil, err
		}
	}
	return nil, ErrBudget
}

func IsMissing(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrNotFound) || errors.Is(err, sql.ErrNoRows)
}

func ValidateSelection(root, lane string) error {
	if !namePattern.MatchString(lane) {
		return errors.New("exact safe lane required")
	}
	_, _, err := canonicalRoot(strings.TrimSpace(root))
	return err
}
