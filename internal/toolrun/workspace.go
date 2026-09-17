// Package toolrun runs the tools an agent is allowed to use.
//
// It is deliberately a package of its own rather than more code inside
// internal/provider. That package is about MODELS: wire formats, token prices,
// HTTP. A tool runner is about the filesystem and child processes, and the two
// share nothing but the effect vocabulary. Putting them together would mean the
// package that holds an API key also holds the code that starts `bash`.
//
// # What this package protects against
//
// The permission layer (internal/tool) already decided WHETHER a tool may run.
// This package decides WHERE it runs and WHAT it can reach, and those are a
// different kind of question with a different failure mode.
//
// A policy bug is visible: the tool is refused, or it is asked about, and
// somebody notices. A confinement bug is invisible: the tool succeeds, the stage
// advances, the log records a completed call — and the write landed outside the
// workspace. Nothing in the run looks wrong. The log, which is the thing this
// project asks people to trust, records a correct-looking history of an incident.
//
// So the rule here is the opposite of convenience: every path is resolved and
// checked against the root before it is used, and anything that cannot be proven
// inside is refused.
//
// # What this package does NOT protect against
//
// A source layout is not process confinement. Command execution is available
// only through an explicit, frozen command profile. That profile states its
// filesystem, network, environment and descendant guarantees independently;
// unsupported strong profiles are refused before this package starts a child.
// An explicitly named operator profile may say unrestricted, but that is not an
// isolation mode and cannot satisfy the default bash requirement.
package toolrun

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/michiTrader/arxi/internal/workspace"
)

// Workspace is the directory a member's tools may touch, and nothing above it.
type Workspace struct {
	// Root is an absolute, symlink-resolved path. Both properties are
	// established once in OpenWorkspace so that every later check is a plain
	// string comparison against a value that cannot lie.
	Root string

	// Member is whose workspace this is, carried so a refusal can name it.
	// A message that says "path escapes the workspace" without saying whose
	// sends the reader to the wrong blueprint.
	Member string

	rootHandle *os.File
	command    *workspace.CommandProfile
	fileAccess workspace.FileAccess
}

// WithFileAccess freezes the file access the workspace's tools may exercise.
//
// It is optional only for tests that predate ADR-0017; production callers get
// it from the provisioned session, whose access was validated at the request
// boundary. The zero value means FileAccessNone, so a caller that forgets to
// plumb it fails closed: mutating tools refuse rather than silently running.
func WithFileAccess(access workspace.FileAccess) func(*Workspace) {
	return func(w *Workspace) { w.fileAccess = access }
}

// Close releases the root capability. Idempotence lets terminal cleanup and
// process shutdown converge without leaking a directory handle.
func (w *Workspace) Close() error {
	if w == nil || w.rootHandle == nil {
		return nil
	}
	err := w.rootHandle.Close()
	w.rootHandle = nil
	return err
}

// OpenWorkspace prepares dir as the root for member's tools.
//
// EvalSymlinks is called on the root itself, and that is load-bearing rather
// than tidiness. On macOS /tmp is a symlink to /private/tmp, so a root captured
// literally as "/tmp/x" would compare unequal to every resolved path beneath it,
// and Resolve would refuse everything — a confinement check that rejects the
// legitimate case gets switched off by the next person in a hurry.
//
// The same call is what makes the check sound in the other direction: comparing
// a resolved child against an unresolved root is comparing two different
// namespaces, and a mismatch there is exactly the gap an attacker needs.
func OpenWorkspace(dir, member string, options ...func(*Workspace)) (*Workspace, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("toolrun: no workspace directory given for %q", member)
	}
	if strings.TrimSpace(member) == "" {
		return nil, fmt.Errorf("toolrun: no member given for workspace %q\n"+
			"  the member is not decoration: a refusal has to name whose workspace "+
			"was escaped, or the reader cannot tell which blueprint entry is wrong", dir)
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("toolrun: absolute path of %q: %w", dir, err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("toolrun: create workspace %s: %w", abs, err)
	}

	// 0o700 above, not 0o755. A workspace can hold whatever a model decided to
	// write, which on a shared machine is not something other users should be
	// able to read.

	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("toolrun: resolve workspace %s: %w", abs, err)
	}
	rootHandle, err := openWorkspaceRoot(real)
	if err != nil {
		return nil, fmt.Errorf("toolrun: open workspace root %s: %w", real, err)
	}
	w := &Workspace{Root: real, Member: member, rootHandle: rootHandle}
	for _, option := range options {
		option(w)
	}
	return w, nil
}

// Resolve turns a tool-supplied path into an absolute one inside the workspace,
// or refuses.
//
// The argument comes from a language model, which means it is neither trusted
// nor adversarial in the usual sense: it is CARELESS. The paths that show up in
// practice are "../fix.go" because the model believed it was one directory
// deeper, and "/etc/hosts" because it was reasoning about a different machine.
// Neither is an attack, and both must fail, because the damage does not depend
// on the intent.
//
// # Why the parent directory is resolved rather than the path itself
//
// EvalSymlinks fails on a path that does not exist yet, and "does not exist yet"
// is the normal case for a write. Resolving the parent and joining the base
// gives a checkable answer for a file about to be created, while still catching
// the case that matters: a symlinked DIRECTORY component pointing out of the
// tree.
//
// The remaining gap is honest to state: if the final component is itself an
// existing symlink pointing outside, this returns a path inside the root that
// resolves outside it. That is why Resolve is not the only defence — the open
// itself passes O_NOFOLLOW (see openNoFollow), so the two together close what
// neither closes alone. A single check that looks sufficient is worse than two
// that admit their limits.
//
// Which means Resolve is not the confinement and must not be used as if it
// were. Callers get WriteFile and ReadFile, never a bare resolved path, because
// a caller holding one half and forgetting the other has written exactly the
// invisible bug described above.
func (w *Workspace) Resolve(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("toolrun: %s gave an empty path\n"+
			"  an empty path is not the workspace root: treating it as the root is "+
			"how a delete of \"\" becomes a delete of everything", w.Member)
	}

	// A NUL byte truncates the path in any syscall that receives it, so
	// "safe.txt\x00/../../etc/passwd" passes a string check and opens something
	// else. Go's os package rejects it, but rejecting it HERE means the refusal
	// names the member and the reason instead of surfacing as "invalid argument".
	if strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("toolrun: %s gave a path containing a NUL byte: %q\n"+
			"  a NUL truncates the path inside the syscall, so the string checked "+
			"and the file opened are different things", w.Member, p)
	}

	full := p
	if !filepath.IsAbs(full) {
		full = filepath.Join(w.Root, full)
	}
	full = filepath.Clean(full)

	dir := filepath.Dir(full)
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("toolrun: resolve %s: %w", dir, err)
		}
		// The parent does not exist. Refuse rather than create it: a tool that
		// can bring whole directory trees into being on the way to a write turns
		// one typo into a mess nobody asked for, and the model that got the
		// depth wrong is exactly the caller that would trigger it.
		return "", fmt.Errorf("toolrun: %s: the directory %s does not exist\n"+
			"  refusing to create parent directories on the way to a write: one "+
			"wrong path would build a tree nobody asked for", w.Member, dir)
	}

	resolved := filepath.Join(realDir, filepath.Base(full))
	if err := w.validateToolPath(resolved); err != nil {
		return "", err
	}
	if !w.contains(resolved) {
		return "", fmt.Errorf("toolrun: %s tried to reach %q, which is outside its "+
			"workspace %s\n"+
			"  resolved to: %s\n"+
			"  this is refused even when it looks accidental, because the damage "+
			"does not depend on the intent",
			w.Member, p, w.Root, resolved)
	}
	return resolved, nil
}

func (w *Workspace) validateToolPath(path string) error {
	relative, err := filepath.Rel(w.Root, path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil
	}
	first := relative
	if index := strings.IndexRune(first, filepath.Separator); index >= 0 {
		first = first[:index]
	}
	if strings.EqualFold(first, ".arxi-workspace.json") || strings.EqualFold(first, metadataDirName) {
		return fmt.Errorf("toolrun: %s: reserved workspace metadata path %q is not agent-writable", w.Member, relative)
	}
	if strings.EqualFold(first, gitControlName) {
		return fmt.Errorf("toolrun: %s: reserved source-control path %q is not agent-accessible\n"+
			"  in a worktree layout this is a gitdir: pointer into the operator's repository. "+
			"Writing it redirects every Git operation performed from this root at a repository "+
			"the member chose; reading it discloses the operator's absolute repository path, and "+
			"through it a config that may carry credentials. The workspace promise is the tracked "+
			"tree, never the control plane that produced it", w.Member, relative)
	}
	return nil
}

const metadataDirName = ".arxi-workspace-metadata"

// gitControlName is refused for READ as well as write, which is the part worth
// spelling out because it is not symmetric with the metadata paths above.
//
// Write is the obvious half: in a worktree layout the workspace root holds a
// `.git` FILE containing `gitdir: <common>/worktrees/<name>`, and rewriting it
// repoints every Git operation run from that root (verified against real Git:
// rev-parse --git-common-dir follows the rewrite).
//
// Read is the half that is easy to miss. That same pointer names the
// operator's repository by absolute path, and following it reaches the common
// config -- where a remote URL can carry an embedded token. A member that can
// read it learns both where the operator's checkout lives and, potentially, a
// credential nobody granted it.
//
// Refusing the name in every layout, including `copy` and `shared` where the
// provisioner creates no such entry, is deliberate. A reserved name whose
// meaning depends on the mode is a rule nobody can apply from the path alone,
// and the tracked tree is what the workspace promises in all three layouts; a
// repository control directory never is.
const gitControlName = ".git"

// contains reports whether p is the root or beneath it.
//
// The separator on the prefix is the whole reason this is a method and not an
// inlined strings.HasPrefix. Without it "/tmp/work" contains "/tmp/workspace2",
// which is a different directory that merely shares a name prefix — and it is a
// silent yes, so nothing would ever reveal the mistake.
func (w *Workspace) contains(p string) bool {
	if p == w.Root {
		return true
	}
	return strings.HasPrefix(p, w.Root+string(filepath.Separator))
}
