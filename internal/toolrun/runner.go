package toolrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrUnknownTool and ErrNotImplemented distinguish a name nobody declared from
// a declared name with no body behind it.
//
// Sentinels rather than distinguishable prose, and the difference is not
// stylistic. The first version said "it is not an unknown tool" in the
// not-implemented message, so anything matching on the phrase "unknown tool" —
// a caller, a log filter, the test that found this — classified the two as the
// same thing. Wording that CONTAINS the other case's phrase cannot be told
// apart by anyone reading text, and text is what an error is.
//
// They matter separately because the remedies are opposite: an unknown tool
// means a model invented a capability and the prompt or the grant is wrong,
// while a declared-but-unimplemented tool means arxi promised something it has
// not built. Sending a user to fix their blueprint over the second would waste
// their time on a bug that is ours.
var (
	ErrUnknownTool    = errors.New("no such tool")
	ErrNotImplemented = errors.New("declared but not implemented in this build")
)

// Runner performs tools for the members of one run.
//
// It owns the mapping from member name to workspace, which is the part the
// executor deliberately does not know. `workspace: worktree` means each member
// gets a directory of its own, and the reason is in docs/design/10-execution.md:
// two agents writing the same directory overwrite each other, and the KV lock
// does not prevent it — the lock coordinates INTENT, while real isolation comes
// from the filesystem.
type Runner struct {
	// Root is the run's directory. Member workspaces live beneath it.
	Root string

	// Shared, when true, gives every member the same workspace: the
	// `workspace: shared` blueprint setting. Off by default, because the
	// default that silently lets two agents corrupt each other's work is not
	// the one to get for free.
	Shared bool

	// Timeout bounds a single bash call. Zero means DefaultTimeout.
	Timeout time.Duration

	// mu guards spaces. Independent effects run in PARALLEL, so two members can
	// ask for their workspace at the same moment, and OpenWorkspace creates
	// directories — without the lock, two goroutines race on the same mkdir and
	// on the map itself.
	mu     sync.Mutex
	spaces map[string]*Workspace
}

// workspaceFor returns member's workspace, creating it once.
func (r *Runner) workspaceFor(member string) (*Workspace, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if w, ok := r.spaces[member]; ok {
		return w, nil
	}

	dir := r.Root
	if !r.Shared {
		// The member name is a path component here, and it arrives from a
		// blueprint a human wrote. "backend/../../etc" would place a workspace
		// wherever it liked, so it is validated rather than trusted: the whole
		// package exists to stop paths from escaping, and letting one in through
		// the front door would be an odd place to start.
		if err := validMemberName(member); err != nil {
			return nil, err
		}
		dir = filepath.Join(r.Root, member)
	}

	w, err := OpenWorkspace(dir, member)
	if err != nil {
		return nil, err
	}
	if r.spaces == nil {
		r.spaces = map[string]*Workspace{}
	}
	r.spaces[member] = w
	return w, nil
}

// validMemberName rejects a member name that cannot be a single directory.
func validMemberName(m string) error {
	if strings.TrimSpace(m) == "" {
		return fmt.Errorf("toolrun: empty member name")
	}
	if m != filepath.Base(m) || m == "." || m == ".." ||
		strings.ContainsAny(m, `/\`) || strings.ContainsRune(m, 0) {
		return fmt.Errorf("toolrun: %q cannot be a workspace directory name\n"+
			"  a member name becomes a path component, so one containing a separator "+
			"or \"..\" would put the workspace outside the run's own directory", m)
	}
	return nil
}

// RunTool performs name for member and returns what the next turn should read.
//
// The dispatch is a closed switch rather than a registry, and an unknown tool is
// refused. internal/tool.Known lists what exists; a default branch that tried
// something anyway would let a model invent a tool and get a plausible answer.
func (r *Runner) RunTool(ctx context.Context, member, name string, args map[string]any) (string, error) {
	w, err := r.workspaceFor(member)
	if err != nil {
		return "", err
	}

	switch name {
	case "bash":
		script, err := stringArg(args, "command", "script")
		if err != nil {
			return "", fmt.Errorf("toolrun: %s calling bash: %w", member, err)
		}
		res, err := w.Bash(ctx, script, r.Timeout)
		if err != nil {
			return "", err
		}
		return formatBash(res), nil

	case "read":
		path, err := stringArg(args, "path", "file")
		if err != nil {
			return "", fmt.Errorf("toolrun: %s calling read: %w", member, err)
		}
		data, err := w.ReadFile(path)
		if err != nil {
			return "", err
		}
		return string(data), nil

	case "write":
		path, err := stringArg(args, "path", "file")
		if err != nil {
			return "", fmt.Errorf("toolrun: %s calling write: %w", member, err)
		}
		// Missing content is an empty write, not an error: "create this file"
		// is a real instruction. Absent and empty are the same request here, and
		// distinguishing them would refuse a legitimate one.
		content, _ := stringArg(args, "content", "text")
		if err := w.WriteFile(path, []byte(content)); err != nil {
			return "", err
		}
		return fmt.Sprintf("wrote %d bytes to %s", len(content), path), nil

	case "grep", "edit":
		// Declared in internal/tool.Known and not implemented here. Named
		// explicitly rather than falling into the default, so the refusal says
		// "not built yet" instead of "no such tool" — those send the reader to
		// completely different places.
		return "", fmt.Errorf("toolrun: %s: the %q tool is granted and permitted, but "+
			"this runner has no implementation for it: %w\n"+
			"  it is declared in internal/tool.Known, so this is a missing body and "+
			"not a mistyped name", member, name, ErrNotImplemented)

	default:
		return "", fmt.Errorf("toolrun: %s asked for %q: %w\n"+
			"  refusing rather than improvising: a model that invents a tool and "+
			"receives a plausible answer will keep using it", member, name, ErrUnknownTool)
	}
}

// stringArg pulls the first present name out of args.
//
// Aliases are accepted because the caller is a model choosing between "command"
// and "script" on its own. Refusing one spelling would fail a turn over
// vocabulary, and the run would record a tool error where the intent was clear.
func stringArg(args map[string]any, names ...string) (string, error) {
	for _, n := range names {
		v, ok := args[n]
		if !ok {
			continue
		}
		s, ok := v.(string)
		if !ok {
			return "", fmt.Errorf("argument %q is %T, not a string", n, v)
		}
		return s, nil
	}

	// The error lists what WAS supplied, sorted. A message that only says what
	// is missing leaves the reader guessing whether the key was absent or
	// misspelled, and sorting keeps it reproducible: Go randomises map order, so
	// an unsorted list would make the same failure read differently each run.
	got := make([]string, 0, len(args))
	for k := range args {
		got = append(got, k)
	}
	sort.Strings(got)
	return "", fmt.Errorf("no %s argument (got: %v)", strings.Join(names, " or "), got)
}

// formatBash renders a result as the text a model will read next.
//
// The exit code is stated in words rather than left implicit in the output. A
// model shown only stdout cannot tell a passing test run from a failing one
// whose output happens to look similar, and the entire point of giving it bash
// is that it finds out.
func formatBash(res BashResult) string {
	var b strings.Builder
	switch {
	case res.TimedOut:
		b.WriteString("TIMED OUT: the command did not finish within its deadline. " +
			"It was killed, along with anything it started. This is not the same as " +
			"failing: nothing was learned about whether it would have succeeded.\n")
	case res.ExitCode == 0:
		b.WriteString("exit 0 (success)\n")
	default:
		fmt.Fprintf(&b, "exit %d (failure)\n", res.ExitCode)
	}
	fmt.Fprintf(&b, "took %s\n", res.Duration.Round(time.Millisecond))
	if res.Output == "" {
		b.WriteString("\n(no output)\n")
		return b.String()
	}
	b.WriteString("\n")
	b.WriteString(res.Output)
	return b.String()
}

// Cleanup removes the run's workspaces.
//
// Called explicitly, never from a finaliser, and never automatically at the end
// of a run. A failed run's workspace is the evidence: `run why` sends the user
// to look at what the agent actually produced, and a runner that tidied up on
// its way out would delete the one artefact worth having.
func (r *Runner) Cleanup() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spaces = nil
	return os.RemoveAll(r.Root)
}
