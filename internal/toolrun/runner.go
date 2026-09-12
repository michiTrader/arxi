package toolrun

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/michiTrader/arxi/internal/workspacefs"
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

// Runner performs tools only through provisioner-verified sessions.
type Runner struct {
	Sessions workspacefs.Provisioner
	Requests map[string]workspacefs.Request
	Timeout  time.Duration

	mu     sync.Mutex
	spaces map[string]*Workspace
}

// workspaceFor obtains the stable session and opens its verified root once.
func (r *Runner) workspaceFor(member string) (*Workspace, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if w, ok := r.spaces[member]; ok {
		return w, nil
	}
	if r.Sessions == nil {
		return nil, fmt.Errorf("toolrun: no workspace provisioner is configured for %q", member)
	}
	req, ok := r.Requests[member]
	if !ok {
		return nil, fmt.Errorf("toolrun: member %q has no frozen workspace request", member)
	}
	session, err := r.Sessions.Provision(context.Background(), req)
	if err != nil {
		return nil, fmt.Errorf("toolrun: provision workspace for %q: %w", member, err)
	}
	root, available := session.WorkspaceRoot()
	if !available {
		return nil, fmt.Errorf("toolrun: member %q uses workspace mode none; file and process tools have no filesystem root", member)
	}
	w, err := OpenWorkspace(root, member)
	if err != nil {
		return nil, err
	}
	if command, ok := session.CommandProfile(); ok {
		copy := *command
		w.command = &copy
	}
	if r.spaces == nil {
		r.spaces = map[string]*Workspace{}
	}
	r.spaces[member] = w
	return w, nil
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

	case "grep":
		pattern, err := stringArg(args, "pattern", "query", "regex")
		if err != nil {
			return "", fmt.Errorf("toolrun: %s calling grep: %w", member, err)
		}
		// An absent path means the whole workspace, which is what a model that
		// says only `pattern` means. It is not an error, unlike an absent
		// pattern: there is an obvious default for where to look and none for
		// what to look for.
		sub, _ := stringArg(args, "path", "dir", "directory")
		matches, truncated, err := w.Grep(pattern, sub)
		if err != nil {
			return "", err
		}
		return formatGrep(matches, truncated, pattern), nil

	case "edit":
		path, err := stringArg(args, "path", "file")
		if err != nil {
			return "", fmt.Errorf("toolrun: %s calling edit: %w", member, err)
		}
		old, err := stringArg(args, "old", "old_string", "find")
		if err != nil {
			return "", fmt.Errorf("toolrun: %s calling edit: %w", member, err)
		}
		// A missing replacement is a DELETION, not an error, and for the same
		// reason a missing `content` is an empty write: "remove this line" is a
		// real instruction, and absent and empty are the same request.
		replacement, _ := stringArg(args, "new", "new_string", "replace")
		all := boolArg(args, "all", "replace_all", "global")

		n, err := w.Edit(path, old, replacement, all)
		if err != nil {
			return "", err
		}
		// The COUNT is reported, not just success. An edit that replaced 7
		// occurrences when the caller expected 1 is a fact it can act on, and
		// the only place it can learn it.
		if n == 1 {
			return fmt.Sprintf("edited %s (1 replacement)", path), nil
		}
		return fmt.Sprintf("edited %s (%d replacements)", path, n), nil

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

// boolArg pulls a boolean out of args, defaulting to false.
//
// Absent is false and there is no error case, which is deliberate: every caller
// of this is a flag that widens what a tool does, so a value nobody supplied has
// to mean the narrower behaviour. An unparseable value is also false, for the
// same reason -- "all": "yes" turning into a global replace because a string is
// truthy somewhere is how a model's loose vocabulary becomes an edit it did not
// ask for.
//
// A JSON string "true" IS accepted, though, because that is not looseness: a
// model emitting arguments as text is a normal thing that happens, and refusing
// it would fail a turn over quoting rather than over intent.
func boolArg(args map[string]any, names ...string) bool {
	for _, n := range names {
		v, ok := args[n]
		if !ok {
			continue
		}
		switch t := v.(type) {
		case bool:
			return t
		case string:
			return t == "true"
		}
		return false
	}
	return false
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

// Release explicitly releases owned managed sessions. Callers must use it only
// after the durable run outcome makes cleanup safe.
func (r *Runner) Release(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var releaseErr error
	for member, req := range r.Requests {
		session, err := r.Sessions.Provision(ctx, req)
		if err == nil {
			err = r.Sessions.Release(ctx, req, session)
		}
		if err != nil {
			releaseErr = errors.Join(releaseErr, fmt.Errorf("release workspace for %s: %w", member, err))
		}
	}
	if releaseErr == nil {
		r.spaces = nil
	}
	return releaseErr
}
