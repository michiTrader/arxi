package toolrun

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"
)

// maxOutputBytes caps what one command may contribute to the run log.
//
// Same reason as maxReadBytes and a different number: a build that prints 40 MB
// of compiler noise has technically succeeded, and has also made the run
// unreadable. The output is truncated rather than the command failed, because a
// test suite that passes while printing too much did pass, and reporting failure
// there would be a lie about the code under test.
const maxOutputBytes = 256 << 10 // 256 KiB

// DefaultTimeout applies when a caller supplies none.
//
// Not zero-means-forever. A model that writes `npm install` on a machine with no
// network produces a child that never exits, and a run loop waiting on it is a
// run that neither finishes nor fails — the state the whole event log exists to
// make impossible. Two minutes is short enough that a human notices and long
// enough for a real test suite.
const DefaultTimeout = 2 * time.Minute

// BashResult is what a finished command contributes to the log.
type BashResult struct {
	// Output is stdout and stderr interleaved, in the order written.
	//
	// Kept as one stream on purpose. A build failure is a compiler message on
	// stderr about a line printed to stdout, and separating them destroys the
	// ordering that makes the pair legible — which matters because the reader is
	// usually a model deciding what to do next.
	Output string

	// ExitCode is the child's status. 0 means success and nothing else does.
	ExitCode int

	// TimedOut distinguishes "the command failed" from "the command never
	// answered". The blueprint's on_timeout is escalate, not fail, precisely
	// because those need different responses, and a runner that collapsed them
	// into a non-zero exit would take that decision away from the reducer.
	TimedOut bool

	// Truncated says the output was cut. Without it, a reader cannot tell a
	// command that printed nothing more from one whose evidence was discarded.
	Truncated bool

	// Duration is measured, not estimated, so a run log can show where the time
	// went without the executor having to guess.
	Duration time.Duration
}

// Bash runs script with the workspace as its working directory.
//
// # What is deliberately not done here
//
// This does not decide whether bash is allowed — internal/tool already did — and
// it does not decide whether the result is an error. It returns what happened.
// A non-zero exit is a fact about the child, not a failure of the runner, and
// collapsing the two would make "the tests found a bug" indistinguishable from
// "the tool runner is broken" in the one log the user is asked to trust.
func (w *Workspace) Bash(ctx context.Context, script string, timeout time.Duration) (BashResult, error) {
	if strings.TrimSpace(script) == "" {
		return BashResult{}, fmt.Errorf("toolrun: %s asked to run an empty script\n"+
			"  an empty command would exit 0, and a success nobody asked for is worse "+
			"than a refusal: the stage advances on evidence that does not exist", w.Member)
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if w.command == nil {
		return BashResult{}, fmt.Errorf("toolrun: %s has no preflighted command profile; bash is refused", w.Member)
	}
	profile := *w.command
	if profile.Schema != "arxi.command-spec/v1" || profile.RunnerVersion == "" || profile.Executable == "" || profile.OutputLimitBytes <= 0 {
		return BashResult{}, fmt.Errorf("toolrun: %s has an invalid command profile; bash is refused", w.Member)
	}
	env, err := commandEnvironment(profile, w.Root)
	if err != nil {
		return BashResult{}, err
	}
	runner, err := platformCommandRunner(profile.Descendants)
	if err != nil {
		return BashResult{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	buf := &cappedBuffer{limit: profile.OutputLimitBytes}
	spec := CommandSpec{Executable: profile.Executable, Argv: []string{"-c", script}, Script: script, Root: w.Root, Env: env}
	start := time.Now()
	exitCode, runErr := runner.Run(ctx, spec, buf)
	elapsed := time.Since(start)
	res := BashResult{Output: buf.String(), ExitCode: exitCode, Duration: elapsed, Truncated: buf.truncated}
	if ctx.Err() != nil {
		res.ExitCode = -1
		res.TimedOut = ctx.Err() == context.DeadlineExceeded
		return res, nil
	}
	if runErr != nil {
		return res, fmt.Errorf("toolrun: %s could not run command: %w; the script did not produce an exit status", w.Member, runErr)
	}
	return res, nil
}

// cappedBuffer accumulates output up to a limit and remembers that it stopped.
//
// It keeps the FIRST bytes rather than the last. For a failing build the first
// error is the one that caused the rest, and a tail would show a screen of
// consequences with the cause discarded.
type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	room := c.limit - c.buf.Len()
	if room <= 0 {
		c.truncated = true
		// The full length is reported as written even though it was discarded.
		// Returning a short count makes io treat it as ErrShortWrite, which kills
		// the child with a broken pipe — so a command would fail for printing too
		// much instead of succeeding with truncated output.
		return len(p), nil
	}
	if len(p) > room {
		c.buf.Write(p[:room])
		c.truncated = true
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *cappedBuffer) String() string {
	s := c.buf.String()
	if c.truncated {
		s += fmt.Sprintf("\n\n[truncated at %d bytes: the beginning is kept because "+
			"the first error is the cause and the rest are its consequences]", c.limit)
	}
	return s
}
