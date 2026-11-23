package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"recalldock.local/recalldock/internal/store"
)

// File resource identity: an immutable content-hash reference in the
// documented resource-dependency format `file://<rel-or-abs-path>@sha256:<64hex>`.
// The hash binds identity to content: same path with different content is a
// different resource version, not "the same file that changed" — matches
// docs/NEXT-ARCHITECTURE.md §1 (文件资源由资源身份与不可变版本引用关联).
func cmdResources(cfg store.Config) {
	if len(os.Args) < 3 {
		fatal(errors.New("usage: recalldock resources <register|check> (local operator only)"))
		return
	}
	sub := os.Args[2]
	switch sub {
	case "register":
		cmdResourcesRegister(cfg)
	case "check":
		cmdResourcesCheck(cfg)
	default:
		fatal(errors.New("unknown resources subcommand"))
		return
	}
}

// ResourceFileIdentity is the computed identity for one file version.
type ResourceFileIdentity struct {
	SourceRef    string `json:"source_ref"` // file://path@sha256:<hex>
	AbsolutePath string `json:"absolute_path"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
}

// hashResourceFile computes the bounded content identity of a regular file.
// Symlinks are resolved first so a moved/copied file with identical content
// keeps one identity.
func hashResourceFile(path string) (*ResourceFileIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("resource file %q does not exist: %w", path, err)
		}
		return nil, fmt.Errorf("resource file %q is not readable: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file; directories are not resource identities")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return nil, err
	}
	digest := hex.EncodeToString(h.Sum(nil))
	slashed := filepath.ToSlash(abs)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	ref := "file://" + slashed + "@sha256:" + digest
	return &ResourceFileIdentity{SourceRef: ref, AbsolutePath: abs, Size: size, SHA256: digest}, nil
}

func cmdResourcesRegister(cfg store.Config) {
	var project, observationIDRaw string
	var path string
	for i := 3; i < len(os.Args); i++ {
		key := os.Args[i]
		if i+1 >= len(os.Args) {
			fatal(fmt.Errorf("%s requires a value", key))
			return
		}
		i++
		switch key {
		case "--project":
			project = strings.TrimSpace(os.Args[i])
			if project == "" {
				fatal(store.ErrResourceDependencyProjectRequired)
				return
			}
		case "--observation-id":
			observationIDRaw = os.Args[i]
		case "--file":
			path = os.Args[i]
		default:
			fatal(fmt.Errorf("unknown resources register option %s", key))
			return
		}
	}
	if project == "" || observationIDRaw == "" || path == "" {
		fatal(errors.New("resources register requires --project PROJECT --observation-id ID --file PATH"))
		return
	}
	observationID, err := strconv.ParseInt(observationIDRaw, 10, 64)
	if err != nil || observationID < 1 {
		fatal(errors.New("positive observation ID required"))
		return
	}
	identity, err := hashResourceFile(path)
	if err != nil {
		fatal(err)
		return
	}
	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
		return
	}
	defer s.Close()
	project, err = s.ResolveExistingProject(project)
	if err != nil {
		fatal(err)
		return
	}
	if _, err := resolveCLIWriteOwner(s, project); err != nil {
		fatal(err)
		return
	}
	if _, err := requireCLIObservationWrite(s, observationID); err != nil {
		fatal(err)
		return
	}
	dep, err := s.AddResourceDependency(store.AddResourceDependencyParams{
		Project:                project,
		SourceRef:              identity.SourceRef,
		DependentObservationID: observationID,
		Reason:                 "operator file identity registration",
	})
	if err != nil {
		fatal(err)
		return
	}
	out, err := jsonMarshalIndent(map[string]any{
		"identity": identity, "dependency": dep,
	}, "", "  ")
	if err != nil {
		fatal(err)
		return
	}
	fmt.Println(string(out))
}

// ResourceCheckResult reports one file's current identity against its
// registered dependency source_ref.
type ResourceCheckResult struct {
	Path          string `json:"path"`
	CurrentSHA256 string `json:"current_sha256"`
	Status        string `json:"status"` // unchanged | changed | missing
	Note          string `json:"note,omitempty"`
}

func cmdResourcesCheck(cfg store.Config) {
	var project, path string
	for i := 3; i < len(os.Args); i++ {
		key := os.Args[i]
		if i+1 >= len(os.Args) {
			fatal(fmt.Errorf("%s requires a value", key))
			return
		}
		i++
		switch key {
		case "--project":
			project = strings.TrimSpace(os.Args[i])
			if project == "" {
				fatal(store.ErrResourceDependencyProjectRequired)
				return
			}
		case "--file":
			path = os.Args[i]
		default:
			fatal(fmt.Errorf("unknown resources check option %s", key))
			return
		}
	}
	if path == "" {
		fatal(errors.New("resources check requires --file PATH [--project PROJECT]"))
		return
	}
	identity, err := hashResourceFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			encoded, _ := json.MarshalIndent(map[string]any{"path": path, "status": "missing"}, "", "  ")
			fmt.Println(string(encoded))
			return
		}
		fatal(err)
		return
	}
	result := ResourceCheckResult{Path: identity.AbsolutePath, CurrentSHA256: identity.SHA256, Status: "unknown"}
	if project != "" {
		s, err := storeNew(cfg)
		if err != nil {
			fatal(err)
			return
		}
		defer s.Close()
		result.Status, err = s.ResourceIdentityStatus(project, identity.SourceRef)
		if err != nil {
			fatal(err)
			return
		}
		switch result.Status {
		case "unregistered":
			result.Note = "no resource dependency references this path in the project"
		case "changed":
			result.Note = "content differs from the registered identity version; use recalldock review resource-changed / resource-refresh"
		}
	} else {
		result.Status = "identity-only"
		result.Note = "pass --project to compare against registered dependencies"
	}
	encoded, err := jsonMarshalIndent(result, "", "  ")
	if err != nil {
		fatal(err)
		return
	}
	fmt.Println(string(encoded))
}
