package gateway

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Recursive grants widen a grant from one directory to its whole subtree. These
// tests pin the boundaries that keep that widening from becoming a privilege
// escalation: exact-match stays the default, matching stops at a path separator,
// a case-insensitive filesystem cannot be used to slip past, and recursion is
// read-only at every entry point.

// TestRecursiveDefaultsOffKeepsExactMatching proves an old-style grant does not
// quietly start covering descendants.
func TestRecursiveDefaultsOffKeepsExactMatching(t *testing.T) {
	root := canonicalPath(t.TempDir())
	g := Grant{Tool: "mem_search", Roots: []string{root}}
	p, ok := g.coversRootTest(root)
	if !ok || !p {
		t.Fatalf("exact root was not covered: covered=%v ok=%v", p, ok)
	}
	if covered, _ := g.coversRootTest(filepath.Join(root, "child")); covered {
		t.Fatal("a non-recursive grant covered a descendant; existing grants would widen on upgrade")
	}
	if covered, _ := g.coversRootTest(filepath.Join(root, "a", "b")); covered {
		t.Fatal("a non-recursive grant covered a deep descendant")
	}
}

// TestRecursiveCoversDescendantsButNotSiblings is the prefix-matching hazard: a
// naive strings.HasPrefix would treat /work/proj-other as inside /work/proj.
func TestRecursiveCoversDescendantsButNotSiblings(t *testing.T) {
	base := canonicalPath(t.TempDir())
	root := filepath.Join(base, "proj")
	g := Grant{Recursive: true, ReadOnly: true, Tool: "mem_search", Roots: []string{root}}

	if covered, _ := g.coversRootTest(filepath.Join(root, "inner")); !covered {
		t.Fatal("recursive grant did not cover a direct descendant")
	}
	if covered, _ := g.coversRootTest(filepath.Join(root, "a", "b", "c")); !covered {
		t.Fatal("recursive grant did not cover a deep descendant")
	}
	// The classic escape: a sibling whose name shares the root as a text prefix.
	sibling := filepath.Join(base, "proj-other")
	if covered, _ := g.coversRootTest(sibling); covered {
		t.Fatalf("recursive grant covered sibling %q; prefix matching is not a boundary", sibling)
	}
	// And the parent, which must never be covered by a grant on a child.
	if covered, _ := g.coversRootTest(base); covered {
		t.Fatal("recursive grant covered its own parent directory")
	}
	if covered, _ := g.coversRootTest("/"); covered {
		t.Fatal(`recursive grant covered "/"`)
	}
}

// TestRecursiveRejectsTraversalShapedRoots proves "..", empty and relative roots
// are refused rather than resolved into something wider.
func TestRecursiveRejectsTraversalShapedRoots(t *testing.T) {
	base := canonicalPath(t.TempDir())
	root := filepath.Join(base, "proj")
	g := Grant{Recursive: true, ReadOnly: true, Tool: "mem_search", Roots: []string{root}}

	for _, probe := range []string{
		"",
		".",
		"relative/path",
		filepath.Join(base, "proj", "..", "proj-other"),
	} {
		if covered, _ := g.coversRootTest(probe); covered {
			t.Fatalf("recursive grant covered %q", probe)
		}
	}
}

// TestRecursiveIsCaseInsensitiveOnWindows only asserts the Windows rule, because
// Linux paths are case-sensitive and lowering them would be wrong there.
func TestRecursiveIsCaseInsensitiveOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("case-insensitive matching is a Windows rule")
	}
	base := canonicalPath(t.TempDir())
	g := Grant{Recursive: true, ReadOnly: true, Roots: []string{filepath.Join(base, "proj")}}
	if covered, _ := g.coversRootTest(filepath.Join(base, "PROJ", "Inner")); !covered {
		t.Fatal("recursive grant did not match a case variant on Windows")
	}
}

// TestValidateRegistryRejectsHandWrittenRecursiveWrite proves a registry edited
// by hand cannot create the widening the CLI refuses.
func TestValidateRegistryRejectsHandWrittenRecursiveWrite(t *testing.T) {
	base := canonicalPath(t.TempDir())
	build := func(g Grant) Registry {
		return Registry{Version: 1, Servers: map[string]Server{
			"test": {
				Source: Source{Path: filepath.Join(base, "src.json"), Format: "json", Keys: []string{"mcpServers", "test"}},
				Grants: []Grant{g},
			},
		}}
	}
	schema := strings.Repeat("a", 64)

	t.Run("recursive write grant is rejected", func(t *testing.T) {
		err := ValidateRegistry(build(Grant{
			ID: "g1", Tool: "mem_save", SchemaHash: schema, Roots: []string{base},
			Recursive: true, ReadOnly: false,
			ArgumentsHash: schema, Session: "s", ExpiresAt: "2099-01-01T00:00:00Z",
		}))
		if err == nil {
			t.Fatal("registry accepted a recursive non-read-only grant")
		}
		if !strings.Contains(err.Error(), "read_only") {
			t.Fatalf("refusal does not explain the rule: %v", err)
		}
	})

	t.Run("recursive wildcard grant is rejected", func(t *testing.T) {
		err := ValidateRegistry(build(Grant{
			ID: "g2", Tool: "mem_search", SchemaHash: schema, Roots: []string{"*"},
			Recursive: true, ReadOnly: true,
		}))
		if err == nil {
			t.Fatal(`registry accepted a recursive grant on "*"`)
		}
	})

	t.Run("recursive read-only grant is accepted", func(t *testing.T) {
		if err := ValidateRegistry(build(Grant{
			ID: "g3", Tool: "mem_search", SchemaHash: schema, Roots: []string{base},
			Recursive: true, ReadOnly: true,
		})); err != nil {
			t.Fatalf("valid recursive read-only grant was rejected: %v", err)
		}
	})

	t.Run("non-recursive write grant is still accepted", func(t *testing.T) {
		if err := ValidateRegistry(build(Grant{
			ID: "g4", Tool: "mem_save", SchemaHash: schema, Roots: []string{base},
			ReadOnly: false, ArgumentsHash: schema, Session: "s", ExpiresAt: "2099-01-01T00:00:00Z",
		})); err != nil {
			t.Fatalf("existing write-grant shape regressed: %v", err)
		}
	})
}

// TestGrantRejectsRecursiveWithoutReadOnly proves the pool refuses the widening
// even when a caller bypasses the CLI and constructs GrantOptions directly.
func TestGrantRejectsRecursiveWithoutReadOnly(t *testing.T) {
	p, _, _ := poolFixture(t, false)
	if _, _, err := p.Discover("test"); err != nil {
		t.Fatal(err)
	}
	_, err := p.Grant("test", GrantOptions{Tool: "image", Recursive: true, ReadOnly: false, Roots: []string{t.TempDir()}})
	if err == nil {
		t.Fatal("Pool.Grant accepted a recursive write grant")
	}
	if !errors.Is(err, ErrInvalidRecursiveGrant) {
		t.Fatalf("error = %v, want ErrInvalidRecursiveGrant", err)
	}
}

// TestGrantAllowsRecursiveReadOnly proves the permitted shape still works, so
// the guard is not simply blocking the feature.
func TestGrantAllowsRecursiveReadOnly(t *testing.T) {
	p, root, _ := poolFixture(t, false)
	if _, _, err := p.Discover("test"); err != nil {
		t.Fatal(err)
	}
	g, err := p.Grant("test", GrantOptions{Tool: "image", Recursive: true, ReadOnly: true, Roots: []string{root}})
	if err != nil {
		t.Fatalf("recursive read-only grant was refused: %v", err)
	}
	if !g.Recursive || !g.ReadOnly {
		t.Fatalf("grant did not record recursion/read-only: %+v", g)
	}
}

// coversRootTest adapts coversRoot to a two-value result so the tests read
// clearly; coversRoot is the implementation under test.
func (g Grant) coversRootTest(root string) (bool, bool) {
	return g.coversRoot(root), true
}

// TestRecursiveGrantIsNotEscapedBySymlink proves a symlink inside an authorized
// subtree cannot be used to reach a directory outside it.
//
// Coverage is decided on canonicalPath, which resolves symlinks before Allowed
// runs. That is what makes the boundary meaningful: if matching used the literal
// text the caller passed, a link would silently authorize its target.
func TestRecursiveGrantIsNotEscapedBySymlink(t *testing.T) {
	base := canonicalPath(t.TempDir())
	authorized := filepath.Join(base, "authorized")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{authorized, outside} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	link := filepath.Join(authorized, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}

	g := Grant{Recursive: true, ReadOnly: true, Tool: "mem_search", Roots: []string{authorized}}

	// The literal link path is textually inside the grant.
	if covered, _ := g.coversRootTest(link); !covered {
		t.Fatal("literal link path should still be textually covered")
	}
	// But once canonicalized it resolves outside, so Allowed must refuse it.
	if covered, _ := g.coversRootTest(canonicalPath(link)); covered {
		t.Fatal("a symlink let a recursive grant reach a directory outside its root")
	}
	// The real subtree is still reachable.
	inner := filepath.Join(authorized, "inner")
	if err := os.MkdirAll(inner, 0o700); err != nil {
		t.Fatalf("mkdir inner: %v", err)
	}
	if covered, _ := g.coversRootTest(canonicalPath(inner)); !covered {
		t.Fatal("recursive grant lost a legitimate descendant")
	}
}

// TestRecursiveGrantRootItselfThroughSymlink proves a grant recorded on a
// symlinked root still authorizes the resolved directory, so the operator is not
// surprised by a grant that never matches.
func TestRecursiveGrantRootItselfThroughSymlink(t *testing.T) {
	base := canonicalPath(t.TempDir())
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	g := Grant{Recursive: true, ReadOnly: true, Tool: "mem_search", Roots: []string{canonicalPath(link)}}
	if covered, _ := g.coversRootTest(canonicalPath(real)); !covered {
		t.Fatal("grant on a symlinked root did not authorize the resolved directory")
	}
}
