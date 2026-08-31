package toolrun

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func ws(t *testing.T) *Workspace {
	t.Helper()
	w, err := OpenWorkspace(filepath.Join(t.TempDir(), "work"), "backend")
	if err != nil {
		t.Fatalf("OpenWorkspace: %v", err)
	}
	return w
}

func TestAPlainRelativePathResolvesInsideTheWorkspace(t *testing.T) {
	w := ws(t)
	got, err := w.Resolve("main.go")
	if err != nil {
		t.Fatalf("Resolve(main.go): %v", err)
	}
	if want := filepath.Join(w.Root, "main.go"); got != want {
		t.Errorf("Resolve(main.go) = %q, want %q", got, want)
	}
}

// TestTheRootItselfIsInside guards the boundary case of the boundary check.
//
// An off-by-one in contains() that excluded the root would refuse every path
// whose parent is the root -- which is most of them -- and the natural "fix"
// under time pressure is to loosen the check rather than find the off-by-one.
func TestTheRootItselfIsInside(t *testing.T) {
	w := ws(t)
	if !w.contains(w.Root) {
		t.Error("the workspace root is not considered inside its own workspace\n" +
			"  consequence: every write directly into the workspace would be refused, " +
			"and the pressure would be to weaken the check instead of fixing the bound")
	}
}

func TestAPathClimbingOutIsRefused(t *testing.T) {
	w := ws(t)
	for _, p := range []string{
		"../escape.go",
		"../../escape.go",
		"sub/../../escape.go",
		"./../escape.go",
	} {
		if got, err := w.Resolve(p); err == nil {
			t.Errorf("Resolve(%q) succeeded with %q; a path that climbs out of the "+
				"workspace must be refused\n"+
				"  consequence: the tool writes outside the workspace, the call "+
				"completes, the stage advances, and the log records a correct-looking "+
				"history of an incident", p, got)
		}
	}
}

func TestAnAbsolutePathOutsideIsRefused(t *testing.T) {
	w := ws(t)
	for _, p := range []string{"/etc/hosts", "/tmp", "/"} {
		if got, err := w.Resolve(p); err == nil {
			t.Errorf("Resolve(%q) succeeded with %q; an absolute path outside the "+
				"workspace must be refused. A model reasoning about a different "+
				"machine produces exactly this, and it is not an attack -- which is "+
				"why intent cannot be the test", p, got)
		}
	}
}

// TestAnAbsolutePathInsideIsAllowed is the inverse, and it earns its place.
//
// The cheap way to pass the test above is to refuse every absolute path. That
// also breaks the legitimate case of a tool echoing back a path this package
// itself returned, which is what a second tool call on the same file looks like.
func TestAnAbsolutePathInsideIsAllowed(t *testing.T) {
	w := ws(t)
	inside := filepath.Join(w.Root, "pkg", "auth")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(inside, "auth.go")
	got, err := w.Resolve(want)
	if err != nil {
		t.Fatalf("Resolve(%q): %v\n"+
			"  an absolute path INSIDE the workspace has to work, or the runner "+
			"cannot accept a path it produced itself", want, err)
	}
	if got != want {
		t.Errorf("Resolve(%q) = %q", want, got)
	}
}

// TestASiblingSharingANamePrefixIsRefused is the one a string prefix gets wrong.
//
// "/tmp/x/work" is not the parent of "/tmp/x/work-2", but HasPrefix without a
// separator says it is. The failure is silent: a legitimate-looking path in a
// DIFFERENT directory is accepted, and nothing in the run ever reveals it.
func TestASiblingSharingANamePrefixIsRefused(t *testing.T) {
	base := t.TempDir()
	w, err := OpenWorkspace(filepath.Join(base, "work"), "backend")
	if err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(base, "work-2")
	if err := os.MkdirAll(sibling, 0o700); err != nil {
		t.Fatal(err)
	}
	if w.contains(sibling) {
		t.Errorf("contains(%q) said yes for workspace %q\n"+
			"  those are different directories that merely share a name prefix, "+
			"and the wrong answer here is a silent yes: a path in somebody else's "+
			"workspace is accepted and nothing ever reveals it", sibling, w.Root)
	}
	if _, err := w.Resolve(sibling); err == nil {
		t.Error("Resolve accepted a sibling directory sharing a name prefix")
	}
}

// TestASymlinkedDirectoryPointingOutIsRefused is why the parent is resolved
// rather than merely cleaned.
//
// filepath.Clean is pure string arithmetic: it has no idea that "link" is a
// symlink to /etc. A confinement check built on Clean alone passes every test
// written with literal "../" and fails the one case that actually escapes.
func TestASymlinkedDirectoryPointingOutIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	base := t.TempDir()
	w, err := OpenWorkspace(filepath.Join(base, "work"), "backend")
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(w.Root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	got, err := w.Resolve("link/loot.txt")
	if err == nil {
		t.Errorf("Resolve(link/loot.txt) succeeded with %q\n"+
			"  the path contains no \"..\" at all, so a check built on "+
			"filepath.Clean accepts it. That is the case that actually escapes, "+
			"and it is the one a string-only check cannot see", got)
	}
}

// TestASymlinkPointingBackInsideIsAllowed keeps the fix from overshooting.
//
// Refusing every symlink would pass the test above and break a workspace that
// legitimately contains one -- a vendored directory, a build cache. The rule is
// about WHERE the link lands, not that it is a link.
func TestASymlinkPointingBackInsideIsAllowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	w := ws(t)
	real := filepath.Join(w.Root, "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(w.Root, "alias")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	got, err := w.Resolve("alias/file.txt")
	if err != nil {
		t.Fatalf("Resolve(alias/file.txt): %v\n"+
			"  a symlink that stays inside the workspace is legitimate; refusing "+
			"every link passes the escape test by breaking real workspaces", err)
	}
	if want := filepath.Join(real, "file.txt"); got != want {
		t.Errorf("Resolve(alias/file.txt) = %q, want %q (the resolved location, "+
			"not the alias, so two names for one file cannot desynchronise)", got, want)
	}
}

func TestAnEmptyPathIsRefusedRatherThanTreatedAsTheRoot(t *testing.T) {
	w := ws(t)
	got, err := w.Resolve("   ")
	if err == nil {
		t.Errorf("Resolve(\"   \") succeeded with %q\n"+
			"  an empty path is not the workspace root. Treating it as the root is "+
			"how a delete of \"\" becomes a delete of everything", got)
	}
}

func TestAPathWithANULByteIsRefused(t *testing.T) {
	w := ws(t)
	got, err := w.Resolve("safe.txt\x00/../../etc/passwd")
	if err == nil {
		t.Errorf("Resolve accepted a path containing a NUL byte, returning %q\n"+
			"  a NUL truncates the path inside the syscall, so the string that was "+
			"checked and the file that gets opened are different things", got)
	}
	if err != nil && !strings.Contains(err.Error(), "NUL") {
		t.Errorf("the refusal does not mention the NUL byte: %v\n"+
			"  surfacing this as a generic \"invalid argument\" makes the reader "+
			"hunt for a filesystem problem that does not exist", err)
	}
}

// TestAMissingParentIsRefusedRatherThanCreated pins a decision, not a mechanism.
//
// mkdir -p on the way to a write is the convenient choice and the wrong one: the
// caller most likely to supply a wrong path is a model that misjudged its depth,
// and that is exactly the caller who would silently materialise a tree.
func TestAMissingParentIsRefusedRatherThanCreated(t *testing.T) {
	w := ws(t)
	got, err := w.Resolve("a/b/c/deep.go")
	if err == nil {
		t.Errorf("Resolve created or accepted a path with a missing parent: %q", got)
	}
	if _, statErr := os.Stat(filepath.Join(w.Root, "a")); statErr == nil {
		t.Error("Resolve created the missing parent directory\n" +
			"  a tool that builds directory trees on the way to a write turns one " +
			"wrong path into a mess nobody asked for")
	}
}

func TestOpenWorkspaceRefusesAnEmptyDirectoryOrMember(t *testing.T) {
	if _, err := OpenWorkspace("", "backend"); err == nil {
		t.Error("OpenWorkspace accepted an empty directory")
	}
	if _, err := OpenWorkspace(filepath.Join(t.TempDir(), "w"), "  "); err == nil {
		t.Error("OpenWorkspace accepted an empty member\n" +
			"  a refusal that cannot name whose workspace was escaped sends the " +
			"reader to the wrong blueprint entry")
	}
}

// TestTheRootIsSymlinkResolvedOnOpen is the test that would have caught the
// macOS failure before it happened.
//
// If the root is stored unresolved, every resolved child compares unequal to it
// and Resolve refuses everything. A confinement check that rejects the
// legitimate case does not survive contact with a deadline: it gets disabled.
func TestTheRootIsSymlinkResolvedOnOpen(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	w, err := OpenWorkspace(link, "backend")
	if err != nil {
		t.Fatalf("OpenWorkspace(%q): %v", link, err)
	}
	if w.Root == link {
		t.Errorf("Root is the symlink %q rather than its target\n"+
			"  consequence: every resolved child compares unequal to the root and "+
			"Resolve refuses everything. This is what /tmp being a symlink to "+
			"/private/tmp does on macOS", link)
	}
	if _, err := w.Resolve("file.txt"); err != nil {
		t.Errorf("Resolve failed inside a workspace opened through a symlink: %v", err)
	}
}

func TestTheWorkspaceIsNotReadableByOtherUsers(t *testing.T) {
	w := ws(t)
	fi, err := os.Stat(w.Root)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("workspace %s has mode %o; group and other bits must be clear\n"+
			"  a workspace holds whatever a model decided to write, which on a "+
			"shared machine is not something other users should be able to read",
			w.Root, perm)
	}
}
