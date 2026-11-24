package gateway

import (
	"path/filepath"
	"runtime"
	"testing"
)

// Recursive coverage is opt-in per grant. A grant recorded before the field
// existed carries no evidence that its operator approved subdirectories, so
// widening it automatically would extend access nobody consented to. These
// tests pin both halves: exact grants stay exact, and --recursive grants cover
// descendants without ever crossing a sibling's path boundary.
func TestGrantCoversRootExactOnlyByDefault(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "proj")
	g := Grant{Roots: []string{root}}

	if !g.coversRoot(root) {
		t.Fatal("exact root not covered by a non-recursive grant")
	}
	if g.coversRoot(filepath.Join(root, "sub")) {
		t.Fatal("non-recursive grant covered a descendant; existing grants would silently widen")
	}
}

func TestGrantCoversRootRecursive(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "proj")
	g := Grant{Roots: []string{root}, Recursive: true}

	if !g.coversRoot(root) {
		t.Fatal("exact root not covered by a recursive grant")
	}
	if !g.coversRoot(filepath.Join(root, "sub")) {
		t.Fatal("direct child not covered by a recursive grant")
	}
	if !g.coversRoot(filepath.Join(root, "a", "b", "c")) {
		t.Fatal("deep descendant not covered by a recursive grant")
	}
}

// The important negative: even a recursive grant on "/work/proj" must not cover
// "/work/project-other". A naive strings.HasPrefix would allow it.
func TestGrantCoversRootRecursiveRejectsSiblingWithSharedPrefix(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "proj")
	sibling := filepath.Join(base, "proj-other")
	g := Grant{Roots: []string{root}, Recursive: true}

	if g.coversRoot(sibling) {
		t.Fatalf("recursive grant on %q wrongly covered sibling %q", root, sibling)
	}
}

func TestGrantCoversRootRecursiveRejectsParent(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "proj")
	g := Grant{Roots: []string{root}, Recursive: true}
	if g.coversRoot(base) {
		t.Fatalf("recursive grant on %q wrongly covered its parent %q", root, base)
	}
}

func TestGrantCoversRootWildcard(t *testing.T) {
	g := Grant{Roots: []string{"*"}}
	if !g.coversRoot(filepath.Join(t.TempDir(), "anything")) {
		t.Fatal("wildcard grant did not cover a path")
	}
}

func TestGrantCoversRootEmptyRootRejected(t *testing.T) {
	if (Grant{Roots: []string{"/work/proj"}, Recursive: true}).coversRoot("") {
		t.Fatal("empty root accepted")
	}
	if (Grant{}).coversRoot("/work/proj") {
		t.Fatal("empty grant list covered a path")
	}
}

func TestServerAllowedUsesRecursiveMatchOnlyWhenGranted(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "proj")

	exact := Server{Grants: []Grant{{Tool: "read", SchemaHash: "h", Roots: []string{root}}}}
	if _, ok := exact.Allowed("read", "h", root, "", nil); !ok {
		t.Fatal("exact root rejected by Allowed")
	}
	if _, ok := exact.Allowed("read", "h", filepath.Join(root, "pkg"), "", nil); ok {
		t.Fatal("non-recursive grant accepted a descendant")
	}

	recursive := Server{Grants: []Grant{{Tool: "read", SchemaHash: "h", Roots: []string{root}, Recursive: true}}}
	if _, ok := recursive.Allowed("read", "h", filepath.Join(root, "pkg"), "", nil); !ok {
		t.Fatal("recursive grant rejected a descendant")
	}
	if _, ok := recursive.Allowed("read", "h", filepath.Join(base, "proj-other"), "", nil); ok {
		t.Fatal("recursive grant accepted a sibling with a shared prefix")
	}
	if _, ok := recursive.Allowed("write", "h", root, "", nil); ok {
		t.Fatal("different tool accepted")
	}
	if _, ok := recursive.Allowed("read", "other-hash", root, "", nil); ok {
		t.Fatal("schema mismatch accepted")
	}
}

func TestWithinRootIsSegmentAware(t *testing.T) {
	sep := string(filepath.Separator)
	root := sep + "work" + sep + "proj"
	if !withinRoot(root+sep+"a", root) {
		t.Fatal("child rejected")
	}
	if withinRoot(root+"-other", root) {
		t.Fatal("prefix sibling accepted")
	}
	if withinRoot(root, root) {
		t.Fatal("root reported as its own strict descendant")
	}
	if !withinRoot(root+sep+"a"+sep, root+sep) {
		t.Fatal("trailing separator changed the verdict")
	}
	if runtime.GOOS == "windows" {
		if !withinRoot(`C:\work\proj\a`, `C:\work\proj`) {
			t.Fatal("windows child rejected")
		}
		if withinRoot(`C:\work\project-other`, `C:\work\proj`) {
			t.Fatal("windows prefix sibling accepted")
		}
	}
}
