package toolrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
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

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// exec.Command, not exec.CommandContext, and the cancellation is done below
	// by hand. CommandContext looks like exactly the right tool and is not, for a
	// reason worth recording because it cost a failing test to find:
	//
	// its watchdog does `select { case resultc <- ctxResult{}: return; case
	// <-ctx.Done(): }`, so if the child is reaped before the deadline it returns
	// WITHOUT ever calling Cancel. `bash -c "work & echo started"` exits in
	// milliseconds, so the shell is always reaped first, and the cancel hook that
	// was supposed to kill the process group is never invoked. The grandchild then
	// runs to completion while Wait sits on a pipe the orphan still holds open.
	//
	// Two mechanisms racing to own the deadline is also just one too many. One
	// goroutine, watching one context, killing one group.
	//
	// -c, not a temporary script file: a file would have to be written somewhere,
	// and the only place this package may write is the workspace the command can
	// itself modify — so the script could be rewritten between creation and
	// execution by the very command it launches.
	cmd := exec.Command("bash", "-c", script)
	cmd.Dir = w.Root

	// A bounded buffer shared by both streams, so interleaving is preserved and
	// the cap applies to the total rather than to each half.
	buf := &cappedBuffer{limit: maxOutputBytes}
	cmd.Stdout = buf
	cmd.Stderr = buf

	// Nil Stdin gives the child /dev/null. Inheriting the parent's stdin would
	// let a command that prompts for input hang forever holding the run open,
	// waiting for a human who is not watching a terminal.
	cmd.Stdin = nil

	// The environment is inherited, and that is a real decision rather than an
	// omission: a build needs PATH, HOME and the toolchain's own variables, and a
	// runner that stripped them would fail on every real project and be worked
	// around immediately. It does mean a command can read the process
	// environment, including provider API keys, so the confinement here is of the
	// filesystem and not of secrets. Saying so is the point; a boundary that is
	// believed to be wider than it is gets trusted with the wrong things.

	setProcessGroup(cmd)

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return BashResult{}, fmt.Errorf("toolrun: %s could not start bash: %w\n"+
			"  this is the runner failing, not the command: the script never ran", w.Member, err)
	}

	// One goroutine owns the deadline. It kills the GROUP, not the process:
	// killing the process reaches the shell and nothing the shell started, so a
	// backgrounded grandchild keeps writing into the workspace of a run that has
	// already been recorded as finished.
	//
	// It also has to happen here rather than after Wait returns, which is the
	// version that looks correct. Stdout is a buffer, so exec copies through an
	// os.Pipe, and Wait does not return until the write end is closed by EVERY
	// process holding it — including the orphan. A kill placed after Wait would
	// therefore fire only once the orphan had finished doing whatever the timeout
	// existed to stop.
	// The pgid is captured HERE, not looked up at the deadline, and that is the
	// difference between this working and silently doing nothing. Setpgid made the
	// child its own group leader, so pgid == pid. For `work & echo done` the shell
	// is reaped within milliseconds while Wait still blocks on the pipe the orphan
	// holds; a Getpgid at the deadline would then ask about a leader that no longer
	// exists and get ESRCH, while the group — orphan included — is very much alive.
	pgid := cmd.Process.Pid

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			killGroup(pgid)
		case <-done:
		}
	}()

	err := cmd.Wait()
	close(done)
	elapsed := time.Since(start)

	res := BashResult{
		Output:    buf.String(),
		Duration:  elapsed,
		Truncated: buf.truncated,
	}

	// ctx.Err is consulted rather than the shape of err, because a killed child
	// reports a signal and a signal is indistinguishable from one the script sent
	// itself. The deadline is the only authority on whether time ran out.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		res.TimedOut = true
		res.ExitCode = -1
		return res, nil
	}

	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
			return res, nil
		}
		// Not an exit status: bash is missing, or the workspace vanished. This one
		// IS a runner failure, and must not be reported as a command that failed —
		// the remedy is completely different and the log should not conflate them.
		return res, fmt.Errorf("toolrun: %s could not run bash: %w\n"+
			"  this is the runner failing, not the command: the script never "+
			"produced an exit status", w.Member, err)
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
