package project

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
)

const SourceRegistered = "registered_binding"

var ErrBinding = errors.New("invalid or unverified project binding")
var opaqueProjectID = regexp.MustCompile(`^prj-[a-f0-9]{32}$`)

func IsProjectID(id string) bool { return opaqueProjectID.MatchString(id) }

type RootMarker struct {
	Version   int    `json:"version"`
	StoreID   string `json:"store_id"`
	ProjectID string `json:"project_id"`
	BindingID string `json:"binding_id"`
}

type BindingRegistry interface {
	ResolveProjectBinding(RootMarker, string) (DetectionResult, error)
}

// DetectProjectFull retains legacy detection, but never trusts a registry marker
// without the database that issued it. Store-aware callers use DetectWithRegistry.
func DetectProjectFull(dir string) DetectionResult { return DetectWithRegistry(dir, nil) }

func DetectWithRegistry(dir string, registry BindingRegistry) DetectionResult {
	marker, root, found, err := findRootMarker(dir)
	if err != nil {
		return DetectionResult{Source: SourceRegistered, Path: root, Error: err}
	}
	if found {
		if registry == nil {
			return DetectionResult{Source: SourceRegistered, Path: root, Error: fmt.Errorf("%w: database registry required", ErrBinding)}
		}
		res, err := registry.ResolveProjectBinding(marker, root)
		if err != nil {
			return DetectionResult{Source: SourceRegistered, Path: root, Error: err}
		}
		return markMainCheckoutInheritance(dir, root, res)
	}
	return detectLegacyProject(dir)
}

// DetectExplicitWithRegistry resolves only operator declarations. Unlike legacy
// discovery, it never scans child repos or derives memory identity from a name.
func DetectExplicitWithRegistry(dir string, registry BindingRegistry) DetectionResult {
	marker, root, found, err := findRootMarker(dir)
	if err != nil {
		return DetectionResult{Source: SourceRegistered, Path: root, Error: err}
	}
	if found {
		if registry == nil {
			return DetectionResult{Source: SourceRegistered, Path: root, Error: ErrBinding}
		}
		result, err := registry.ResolveProjectBinding(marker, root)
		if err != nil {
			return DetectionResult{Source: SourceRegistered, Path: root, Error: err}
		}
		return markMainCheckoutInheritance(dir, root, result)
	}
	if result, ok := detectFromConfig(dir); ok {
		return result
	}
	return DetectionResult{}
}

// markMainCheckoutInheritance relabels a result that resolved through the main
// checkout of a linked git worktree. The binding is still the store-verified
// main-root binding; only the source and the working path are adjusted.
func markMainCheckoutInheritance(dir, markerRoot string, res DetectionResult) DetectionResult {
	linked, mainRoot := IsLinkedWorktree(dir)
	if !linked || mainRoot == "" || !samePath(canonicalizePath(markerRoot), canonicalizePath(mainRoot)) {
		return res
	}
	res.Source = SourceWorktreeMain
	res.Path = canonicalizePath(dir)
	return res
}

// ReadRootMarker only reads bounded binding metadata, not memory content.
func ReadRootMarker(root string) (RootMarker, bool, error) {
	var marker RootMarker
	hidden := filepath.Join(root, ".recalldock")
	if info, err := os.Lstat(hidden); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return marker, true, fmt.Errorf("%w: symlink metadata directory", ErrBinding)
	}
	path := filepath.Join(hidden, "root.json")
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return marker, false, nil
	}
	if err != nil {
		return marker, true, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4096 {
		return marker, true, fmt.Errorf("%w: unsafe marker", ErrBinding)
	}
	f, err := os.Open(path)
	if err != nil {
		return marker, true, err
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, 4097))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&marker); err != nil {
		return marker, true, fmt.Errorf("%w: malformed marker", ErrBinding)
	}
	var trailing any
	if dec.Decode(&trailing) != io.EOF || marker.Version != 1 || !IsProjectID(marker.ProjectID) || marker.StoreID == "" || marker.BindingID == "" {
		return marker, true, fmt.Errorf("%w: malformed marker metadata", ErrBinding)
	}
	return marker, true, nil
}

func hasGitDeclaration(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

var (
	gitAvailableOnce sync.Once
	gitAvailableFlag bool
)

// gitBinaryAvailable keeps the broken-declaration guard limited to environments
// where git can actually verify repository roots. Without git the legacy
// discovery path stays available instead of failing every directory.
func gitBinaryAvailable() bool {
	gitAvailableOnce.Do(func() {
		_, err := exec.LookPath("git")
		gitAvailableFlag = err == nil
	})
	return gitAvailableFlag
}

func findRootMarker(dir string) (RootMarker, string, bool, error) {
	if dir == "" {
		dir = "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return RootMarker{}, dir, false, err
	}
	abs = canonicalizePath(abs)
	info, err := os.Stat(abs)
	if err != nil {
		return RootMarker{}, abs, false, fmt.Errorf("%w: root directory unavailable: %v", ErrBinding, err)
	}
	if !info.IsDir() {
		return RootMarker{}, abs, false, fmt.Errorf("%w: directory required; bind file resources separately", ErrBinding)
	}
	stop := canonicalizePath(detectGitRootDir(abs))
	home, _ := os.UserHomeDir()
	home = canonicalizePath(home)
	for depth := 0; depth < 64; depth++ {
		if samePath(abs, home) || filepath.Dir(abs) == abs {
			return RootMarker{}, abs, false, nil
		}
		marker, found, err := ReadRootMarker(abs)
		if found || err != nil {
			return marker, abs, found, err
		}
		// A nearer explicit legacy project is also an isolation boundary.
		if _, err := os.Lstat(filepath.Join(abs, ".recalldock/config.json")); err == nil {
			return RootMarker{}, abs, false, nil
		} else if !os.IsNotExist(err) {
			return RootMarker{}, abs, false, err
		}
		// A directory that declares its own repository (.git dir or file) must
		// never fall through to an ancestor project binding when that
		// declaration cannot be resolved: a broken checkout or a dangling
		// submodule/worktree pointer would otherwise inherit the parent's
		// identity silently. Fail closed instead.
		if stop == "" && hasGitDeclaration(abs) && gitBinaryAvailable() {
			return RootMarker{}, abs, false, fmt.Errorf("%w: directory declares a git repository that cannot be resolved; repair the checkout or bind the intended project explicitly", ErrBinding)
		}
		if abs == stop {
			// A linked git worktree shares the main checkout's repository but has
			// its own top level. Reuse the main checkout's established binding so
			// one repository does not fragment into several identities. This is
			// binding inheritance, not name/path matching: the main root must own a
			// valid marker that the store registry still verifies.
			if linked, mainRoot := IsLinkedWorktree(abs); linked && mainRoot != "" {
				if marker, found, err := ReadRootMarker(mainRoot); found || err != nil {
					return marker, mainRoot, found, err
				}
			}
			return RootMarker{}, abs, false, nil
		}
		abs = filepath.Dir(abs)
	}
	return RootMarker{}, abs, false, fmt.Errorf("%w: ancestor depth exceeds 64", ErrBinding)
}
