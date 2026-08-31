package toolrun

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestASuccessfulCommandReportsItsOutputAndZero(t *testing.T) {
	w := ws(t)
	res, err := w.Bash(context.Background(), "echo hello", 0)
	if err != nil {
		t.Fatalf("Bash: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if strings.TrimSpace(res.Output) != "hello" {
		t.Errorf("Output = %q, want %q", res.Output, "hello")
	}
	if res.TimedOut || res.Truncated {
		t.Errorf("TimedOut = %v, Truncated = %v, want both false", res.TimedOut, res.Truncated)
	}
}

func TestAFailingCommandIsNotARunnerError(t *testing.T) {
	w := ws(t)
	res, err := w.Bash(context.Background(), "exit 3", 0)
	if err != nil {
		t.Fatalf("Bash returned an error for a non-zero exit: %v\n"+
			"  a command that fails is a fact about the child, not a broken runner; "+
			"collapsing them makes \"the tests found a bug\" look like \"the tool "+
			"runner is broken\" in the log the user is asked to trust", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}
}

func TestTheCommandRunsInsideTheWorkspace(t *testing.T) {
	w := ws(t)
	res, err := w.Bash(context.Background(), "pwd", 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := filepath.EvalSymlinks(strings.TrimSpace(res.Output))
	if err != nil {
		t.Fatal(err)
	}
	if got != w.Root {
		t.Errorf("cwd = %q, want %q\n"+
			"  a command that starts in the caller's directory writes its files "+
			"wherever arxi happened to be launched, which is the confinement the "+
			"rest of this package spends its effort establishing", got, w.Root)
	}
}

func TestStdoutAndStderrArriveInOneStream(t *testing.T) {
	w := ws(t)
	res, err := w.Bash(context.Background(), "echo out; echo err 1>&2", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "out") || !strings.Contains(res.Output, "err") {
		t.Errorf("Output = %q, want both streams\n"+
			"  a build failure is a stderr message about a stdout line, and dropping "+
			"either half leaves the reader — usually a model — with half the evidence",
			res.Output)
	}
}

func TestATimeoutIsReportedAsATimeoutRatherThanAFailure(t *testing.T) {
	w := ws(t)
	start := time.Now()
	res, err := w.Bash(context.Background(), "sleep 30", 150*time.Millisecond)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Bash returned an error on timeout: %v\n"+
			"  the blueprint's on_timeout is escalate, not fail: a timeout almost "+
			"never means impossible, it means something got stuck", err)
	}
	if !res.TimedOut {
		t.Error("TimedOut = false after a command that outlived its deadline\n" +
			"  the reducer needs to tell \"never answered\" from \"answered no\", " +
			"because escalating and failing are different responses")
	}
	if elapsed > 5*time.Second {
		t.Errorf("the call took %v for a 150ms timeout, so the deadline is not "+
			"actually enforced", elapsed)
	}
}

func TestATimeoutDoesNotLeaveTheChildRunning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are a no-op on windows, and killGroup says so")
	}
	w := ws(t)

	// The grandchild writes AFTER the parent shell has exited. If only the direct
	// child is killed, the marker appears in a workspace belonging to a run that
	// has already been recorded as finished.
	script := `(sleep 0.4; echo leaked > orphan.txt) & echo started`
	res, err := w.Bash(context.Background(), script, 150*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut {
		t.Fatalf("expected a timeout, got exit %d", res.ExitCode)
	}

	// Wait past the point where the orphan would have written.
	time.Sleep(1200 * time.Millisecond)

	if _, err := os.Lstat(filepath.Join(w.Root, "orphan.txt")); !os.IsNotExist(err) {
		t.Error("a backgrounded grandchild survived the timeout and wrote into the " +
			"workspace\n" +
			"  exec.CommandContext kills only the direct child, so the group has to " +
			"be killed explicitly; otherwise the run is over and the writes continue")
	}
}

func TestOversizedOutputIsTruncatedRatherThanFailing(t *testing.T) {
	w := ws(t)
	// Print well past the cap, then exit 0.
	res, err := w.Bash(context.Background(),
		"for i in $(seq 1 20000); do echo aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa; done", 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0\n"+
			"  a suite that passes while printing too much did pass: failing it "+
			"would be a lie about the code under test, and a short Write would kill "+
			"the child with a broken pipe", res.ExitCode)
	}
	if !res.Truncated {
		t.Error("Truncated = false on output past the cap\n" +
			"  without the flag a reader cannot tell a command that printed nothing " +
			"more from one whose evidence was discarded")
	}
	if len(res.Output) > maxOutputBytes+512 {
		t.Errorf("Output is %d bytes, cap is %d: the limit is not enforced",
			len(res.Output), maxOutputBytes)
	}
	if !strings.Contains(res.Output, "truncated") {
		t.Error("the truncated output does not say it was truncated")
	}
}

func TestTruncationKeepsTheBeginning(t *testing.T) {
	w := ws(t)
	res, err := w.Bash(context.Background(),
		"echo FIRST_LINE_MARKER; for i in $(seq 1 20000); do echo bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb; done", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "FIRST_LINE_MARKER") {
		t.Error("the first line was discarded by truncation\n" +
			"  for a failing build the first error is the cause and everything after " +
			"is its consequences; keeping the tail shows a screen of symptoms")
	}
}

func TestAnEmptyScriptIsRefusedRatherThanSucceeding(t *testing.T) {
	w := ws(t)
	for _, s := range []string{"", "   ", "\n\t"} {
		if _, err := w.Bash(context.Background(), s, 0); err == nil {
			t.Errorf("Bash(%q) succeeded\n"+
				"  an empty command exits 0, and a success nobody asked for advances "+
				"the stage on evidence that does not exist", s)
		}
	}
}

func TestACancelledContextStopsTheCommandWithoutClaimingATimeout(t *testing.T) {
	w := ws(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	res, err := w.Bash(ctx, "sleep 30", 30*time.Second)
	if err != nil {
		t.Fatalf("Bash: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("cancelling the context did not stop the command")
	}
	if res.TimedOut {
		t.Error("TimedOut = true after a CANCELLATION\n" +
			"  the user pressed ctrl-c; recording that as the command's own timeout " +
			"would send the reader to escalate a stage that was never stuck")
	}
}

func TestTheDurationIsMeasuredRatherThanZero(t *testing.T) {
	w := ws(t)
	res, err := w.Bash(context.Background(), "sleep 0.2", 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Duration < 150*time.Millisecond {
		t.Errorf("Duration = %v for a 200ms sleep\n"+
			"  a run log that cannot show where the time went makes the executor "+
			"guess, and a guessed number in an audit trail is worse than none",
			res.Duration)
	}
}

func TestACommandCanWriteInsideTheWorkspace(t *testing.T) {
	w := ws(t)
	if _, err := w.Bash(context.Background(), "echo built > out.txt", 0); err != nil {
		t.Fatal(err)
	}
	got, err := w.ReadFile("out.txt")
	if err != nil {
		t.Fatalf("the command's own output file is not readable through the "+
			"workspace: %v\n"+
			"  the inverse of every refusal: a sandbox that blocks the legitimate "+
			"case is one somebody switches off", err)
	}
	if strings.TrimSpace(string(got)) != "built" {
		t.Errorf("out.txt = %q, want %q", got, "built")
	}
}

func TestAMissingTimeoutFallsBackToTheDefaultRatherThanForever(t *testing.T) {
	// Asserted as a property of the constant, because exercising it honestly
	// would mean blocking a test for two minutes. What matters is that zero is
	// never treated as "no deadline": a child that never exits holds the run
	// open, which is the state the event log exists to make impossible.
	if DefaultTimeout <= 0 {
		t.Fatal("DefaultTimeout is not positive, so a caller passing 0 gets no deadline")
	}
	w := ws(t)
	res, err := w.Bash(context.Background(), "echo x", 0)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("a zero timeout should mean the default, not a refusal: %v", err)
	}
}
