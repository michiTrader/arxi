package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/michiTrader/arxi/internal/logstore"
)

// `arxi run attach`, exercised as a process against a log the test is appending to.
//
// # Why this file needed machinery the other CLI tests did not
//
// Every other run verb is a function of bytes that are already on disk: write a
// fixture, run the binary, read what it printed. This one is a function of bytes
// that arrive WHILE it runs, and three of its four promises are unobservable if the
// test writes the whole log first:
//
//   - it joins at the head, so the twelve events that already exist are NOT printed
//   - an event split across two writes is printed once, whole
//   - a batch that was never committed is not printed at all
//
// A fixture written before the process starts turns all three into the same trivial
// case. So the tests below start the binary, wait for its join header, and only then
// append -- which is also the only ordering under which "printed only what arrived
// after it" means anything.
//
// # Every invocation is bounded, and that is an assertion
//
// The follow loop has exactly two exits: the run reaches a terminal state, or the
// writer lock goes away. A defect in either check does not produce a wrong answer,
// it produces no answer -- and an unbounded exec in a Go test does not fail, it
// hangs the whole package until the 10-minute timeout and reports a goroutine dump
// with no test name (scheduler_cli_test.go documents that happening). So there is no
// plain exec here at all: startAttach kills the process group at a deadline and
// finish() fails with both streams, naming the exit that did not happen.
//
// # The fixtures hand-write writer.lock, and nothing checks the pid
//
// A follower decides "somebody is appending" by the presence of the lock file and
// names its contents so the user knows who they are waiting on. It does not signal
// the pid, and does not ask the OS whether it is alive -- deliberately, since a
// viewer that probed or reaped the writer would be a viewer that can break the run
// it is watching. That is what makes these fixtures possible: `pid 4242` is a lock
// held by nobody, which is precisely a live writer as far as this verb can tell.
//
// The lock path comes from logstore.LockPath rather than the string "writer.lock",
// so the fixture and the binary read one constant. pending.commit is spelled out
// (the package exports no path for it) and the fixture immediately asserts
// logstore.BatchInFlight agrees -- otherwise a rename would leave that test quietly
// exercising nothing, which is the failure mode surface_coverage_test.go exists for.

// attachTestDeadline is how long any single invocation below may take.
//
// Generous rather than tight: these tests append on a 120 ms poll interval and run
// alongside the rest of the package, so a slow machine must not fail them. It is a
// backstop against a hang, not a performance assertion.
const attachTestDeadline = 20 * time.Second

// syncBuf is a buffer safe to read while a child process is writing into it.
//
// os/exec copies a child's output on its own goroutine when Stdout is not an
// *os.File, and these tests read stderr while the child is still running -- that is
// how they learn the follower has joined. A bytes.Buffer there is a data race, and
// one the race detector reports as a failure in os/exec rather than here, which is a
// confusing place to start reading.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// attachSession is a running `arxi run attach`, with its streams kept apart.
//
// Separate streams and not CombinedOutput, for the reason arxiStreams gives about
// `run result` and doubly so here: this verb promises that stdout is only ever event
// rows, so `arxi run attach r1 --json > live.ndjson` produces a parseable file. A
// combined-output test cannot tell a diagnostic on stderr from the same sentence
// printed into the middle of somebody's NDJSON.
type attachSession struct {
	t      *testing.T
	cmd    *exec.Cmd
	ctx    context.Context
	out    *syncBuf
	errb   *syncBuf
	done   chan error
	waited bool
	err    error
}

// startAttach starts `arxi run attach <args...>` and returns without waiting.
//
// The process group and the group-wide SIGKILL are arxiBounded's, for its reason:
// killing only the parent of a process that has children leaves them writing into a
// t.TempDir() that is being removed. attach spawns nothing today, and inheriting the
// pattern costs one line rather than a future debugging session.
func startAttach(t *testing.T, dir string, args ...string) *attachSession {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), attachTestDeadline)
	cmd := exec.CommandContext(ctx, buildIash(t), append([]string{"run", "attach"}, args...)...)
	cmd.Dir = dir

	s := &attachSession{t: t, cmd: cmd, ctx: ctx, out: &syncBuf{}, errb: &syncBuf{}, done: make(chan error, 1)}
	cmd.Stdout, cmd.Stderr = s.out, s.errb
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	// Wait blocks until the output copiers finish as well as the process, so a
	// descendant holding the pipe open would hang the test after the kill. Nothing
	// here has descendants; the delay makes that assumption survive one that does.
	cmd.WaitDelay = 5 * time.Second

	// stdin is closed, which every invocation in this package gets and which one
	// hand-run probe once got wrong: a command that inherits the test runner's stdin
	// can consume input meant for something else. attach reads none, so this is
	// hygiene rather than a fix.
	cmd.Stdin = strings.NewReader("")

	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("starting arxi run attach %v: %v", args, err)
	}
	go func() { s.done <- cmd.Wait() }()

	// Cleanup runs on the test goroutine after the body returns, so the waited flag
	// needs no lock. Draining done matters even for a test that already finished:
	// leaving the goroutine blocked on a send would leak it for the whole package.
	t.Cleanup(func() {
		cancel()
		if !s.waited {
			s.err = <-s.done
			s.waited = true
		}
	})
	return s
}

// attachJoinHeader is the first thing a following attach prints, on stderr.
//
// The tests wait for it before appending anything, and that wait is not politeness:
// an event appended before the binary has read the log is an event it folds into its
// join state, so it is legitimately NOT printed. Without this handshake the central
// test would be racing its own fixture and would fail, occasionally, for the one
// behaviour it is asserting is correct.
const attachJoinHeader = "attached to"

// await polls one of the child's streams for a substring, and stops early if the
// child exits.
//
// The exit check is not an optimisation. Without it, a follower that refused at the
// pre-flight -- a one-line write and an immediate return -- would be polled for the
// whole eventually budget, and the failure would blame a timeout rather than the
// refusal already sitting in stderr. The error is kept because finish() still needs
// it to report the real exit code.
func (s *attachSession) await(b *syncBuf, sub string) bool {
	s.t.Helper()

	eventually(s.t, func() bool {
		if strings.Contains(b.String(), sub) {
			return true
		}
		select {
		case err := <-s.done:
			s.err, s.waited = err, true
			return true
		default:
			return false
		}
	})
	return strings.Contains(b.String(), sub)
}

// waitForJoin blocks until the follower has joined, or fails saying it never did.
func (s *attachSession) waitForJoin() {
	s.t.Helper()

	if s.await(s.errb, attachJoinHeader) {
		return
	}
	s.t.Fatalf("arxi run attach %v never printed %q, so it never joined.\nstdout:\n%s\nstderr:\n%s\n"+
		"  consequence: the appends this test makes next would land before the "+
		"follower read the log, which is the one case where NOT printing them "+
		"is correct -- so every assertion after this point would be measuring "+
		"the fixture's timing instead of the binary.",
		s.cmd.Args[1:], attachJoinHeader, s.out.String(), s.errb.String())
}

// waitForRow blocks until a row reaches stdout.
//
// Used to pin down where the follower's read offset is, which is the only way to
// write a torn-line test that is not a race: a row on stdout proves the read that
// produced it consumed through that line's newline and no further, so anything
// written after the newline in the same call is provably still held back.
func (s *attachSession) waitForRow(sub string) {
	s.t.Helper()

	if s.await(s.out, sub) {
		return
	}
	s.t.Fatalf("arxi run attach %v never printed a row containing %q.\nstdout:\n%s\nstderr:\n%s\n"+
		"  consequence: this test uses that row to learn the follower's read "+
		"offset. Without it the next append cannot be placed relative to the "+
		"reader, and the case being tested is not the case being run.",
		s.cmd.Args[1:], sub, s.out.String(), s.errb.String())
}

// finish waits for the process and reports what a caller saw.
func (s *attachSession) finish() (string, string, int) {
	s.t.Helper()

	if !s.waited {
		s.err = <-s.done
		s.waited = true
	}
	out, errb := s.out.String(), s.errb.String()

	if s.ctx.Err() != nil {
		s.t.Fatalf("arxi run attach %v did not exit within %s and was killed.\nstdout:\n%s\nstderr:\n%s\n"+
			"  consequence: the follow loop has exactly two exits -- the run reaching "+
			"a terminal state, and the writer lock going away. If neither fired here, "+
			"a real user is holding Ctrl-C on a run that already finished.",
			s.cmd.Args[1:], attachTestDeadline, out, errb)
	}

	code := 0
	if ee, ok := s.err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if s.err != nil {
		s.t.Fatalf("running arxi run attach %v: %v\nstdout:\n%s\nstderr:\n%s", s.cmd.Args[1:], s.err, out, errb)
	}
	return out, errb, code
}

// runDirOf is where runAt and its friends put a run.
func runDirOf(dir, id string) string { return filepath.Join(dir, "runs", id) }

// holdWriterLock makes a run look like one somebody is driving.
//
// The body is what logstore.acquireLock writes and what its refusal quotes back, so
// the header under test names the same thing a real conflict would. See the file
// header for why a pid that belongs to nobody is enough.
func holdWriterLock(t *testing.T, dir, id string) {
	t.Helper()
	if err := os.WriteFile(logstore.LockPath(runDirOf(dir, id)), []byte("pid 4242\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// releaseWriterLock is a writer going away, which is one of the two ways a follow ends.
func releaseWriterLock(t *testing.T, dir, id string) {
	t.Helper()
	if err := os.Remove(logstore.LockPath(runDirOf(dir, id))); err != nil {
		t.Fatal(err)
	}
}

// appendToRunLog appends complete lines, the way a writer that committed them does.
//
// One open per call rather than a handle held across the test, so each call is a
// separate arrival the follower has to notice on its own. A single buffered writer
// would let several "appends" reach the file as one, which is the shape these tests
// are trying to take apart.
func appendToRunLog(t *testing.T, dir, id string, lines ...string) {
	t.Helper()
	appendRawToRunLog(t, dir, id, strings.Join(lines, "\n")+"\n")
}

// appendRawToRunLog appends exactly these bytes, terminated or not.
//
// The unterminated case is the point of having it: a real writer's bytes reach the
// file when they reach it, so the last line of a follower's read is routinely half an
// event. Nothing else in this package can produce that.
func appendRawToRunLog(t *testing.T, dir, id, raw string) {
	t.Helper()

	f, err := os.OpenFile(logstore.EventsPath(runDirOf(dir, id)), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(raw); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// attachRows is the event rows on stdout, blank lines dropped.
func attachRows(stdout string) []string {
	var rows []string
	for _, line := range strings.Split(stdout, "\n") {
		if strings.TrimSpace(line) != "" {
			rows = append(rows, line)
		}
	}
	return rows
}

// The three events §20.1 shows arriving, in the payload shapes the executors write.
//
// Copied from the doc rather than invented, because the row format is the thing being
// checked and the doc is where it is promised. The keys were read out of
// internal/exec/fake.go and internal/provider/executor.go: `tool` and `args` on
// tool.call, `agent` and `cost_usd` on llm.response, `summary` on run.result.
const (
	attachToolCall = `{"id":"e3","seq":3,"type":"tool.call","actor":"backend",` +
		`"payload":{"agent":"backend","tool":"read","args":"src/auth.go"}}`
	attachLLMResponse = `{"id":"e4","seq":4,"type":"llm.response","actor":"backend",` +
		`"payload":{"agent":"backend","cost_usd":0.09,"model":"claude-sonnet-5"}}`
	attachRunResult = `{"id":"e5","seq":5,"type":"run.result",` +
		`"payload":{"summary":"all stages completed","result_from":"last_submit"}}`
)

// TestRunAttachOnARunThatAlreadyEndedSaysSoAndExitsZero is the case with no waiting.
//
// Exit 0 and not 3, which is a deliberate choice worth pinning: the run ended, so
// there is nothing wrong and nothing to wait for. A script that attaches after a run
// finished has not hit an error, and 3 -- shared with `run result`'s "not terminal
// yet" -- would tell it the opposite of the truth about a completed run.
func TestRunAttachOnARunThatAlreadyEndedSaysSoAndExitsZero(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	succeededAt(t, dir, id)

	out, errb, code := startAttach(t, dir, id).finish()
	if code != 0 {
		t.Errorf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s\n"+
			"  consequence: attaching to a run that finished is not a failure. A "+
			"non-zero code here makes `arxi run attach r1 && deploy` refuse to "+
			"deploy a run that succeeded.", code, out, errb)
	}
	if out != "" {
		t.Errorf("stdout is not empty:\n%s\n"+
			"  consequence: stdout is event rows and nothing else, so an "+
			"explanation printed there lands in the middle of a redirected "+
			"stream -- and under --json it is a line that does not parse.", out)
	}
	for _, want := range []string{"already ended", "arxi event log " + id, "arxi run result " + id} {
		if !strings.Contains(errb, want) {
			t.Errorf("stderr does not mention %q:\n%s\n"+
				"  consequence: the user asked to watch a run and there is nothing "+
				"to watch. Naming the two verbs that DO answer for a finished run is "+
				"the whole of the useful reply.", want, errb)
		}
	}
}

// TestRunAttachOnARunNobodyIsDrivingDoesNotWaitInSilence is the pre-flight refusal.
//
// The surface gives this verb a run id and nothing else -- no --timeout to declare --
// so a run with no writer has to be answered rather than waited out. The failure this
// pins is not a wrong message, it is an indefinite one: a viewer sitting silent on a
// dead run looks exactly like a viewer watching a run that is thinking, and the user
// finds out only by giving up.
func TestRunAttachOnARunNobodyIsDrivingDoesNotWaitInSilence(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	runAt(t, dir, id, "feature-team", 1.0, "")

	out, errb, code := startAttach(t, dir, id).finish()
	if code != exitResultNotYet {
		t.Errorf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s\n"+
			"  consequence: 3 is `run result`'s \"not terminal yet\", shared on "+
			"purpose so a script polling either verb reads one number. 1 would "+
			"make a run waiting on a person look like a broken log.",
			code, exitResultNotYet, out, errb)
	}
	if out != "" {
		t.Errorf("stdout is not empty:\n%s", out)
	}
	for _, want := range []string{
		"nothing is writing to " + id,
		"no process holds its writer lock",
		"arxi run why " + id,
	} {
		if !strings.Contains(errb, want) {
			t.Errorf("stderr does not mention %q:\n%s\n"+
				"  consequence: \"nothing is writing to r1\" is the symptom. That "+
				"the log claims a running run while no process holds the lock is "+
				"the cause, and it is the sentence that tells the user their "+
				"driver died rather than that they mistyped an id.", want, errb)
		}
	}

	// `run unpause` must NOT be suggested. It refuses a run that is already running
	// -- "there is nothing to resume" -- and points at `run why` itself, so naming it
	// costs the user one command to arrive where this message already sent them.
	if strings.Contains(errb, "run unpause") {
		t.Errorf("stderr suggests `run unpause` for a running run:\n%s\n"+
			"  consequence: unpause.go refuses exactly this case, so the remedy "+
			"printed here becomes the user's second problem.", errb)
	}
}

// TestRunAttachOnAStuckRunSendsTheReaderWhereRunShowWould covers the two diagnoses
// that are not "the driver died".
//
// Both are subtests of one test because the requirement is a comparison: a blocked run
// and a paused run must be told apart, and told apart in the words the rest of the
// binary already uses. `run show` closes with the same switch, and two verbs looking at
// one stuck run that send the reader to different places is worse than either being
// slightly wrong -- it makes the user decide which of the two to believe.
func TestRunAttachOnAStuckRunSendsTheReaderWhereRunShowWould(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(t *testing.T, dir, id string)
		wants []string
	}{
		{
			// blockedAt is budget.exceeded plus the inbox.created it mints, so the
			// question is real and unanswered: pendingAsks counts 1, and the remedy
			// is the inbox rather than a diagnosis of the run.
			name: "blocked on a question",
			build: func(t *testing.T, dir, id string) {
				blockedAt(t, dir, id, "feature-team", 1.0)
			},
			wants: []string{"blocked on 1 question(s)", "arxi inbox"},
		},
		{
			name: "paused",
			build: func(t *testing.T, dir, id string) {
				runAt(t, dir, id, "feature-team", 1.0,
					`{"id":"e3","seq":3,"type":"run.paused","payload":{"reason":"operator"}}`+"\n")
			},
			wants: []string{"it is paused at seq 3", "arxi run unpause "},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			const id = "rmthws2dz-93381f43"
			tc.build(t, dir, id)

			out, errb, code := startAttach(t, dir, id).finish()
			if code != exitResultNotYet {
				t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitResultNotYet, out, errb)
			}
			for _, want := range tc.wants {
				if !strings.Contains(errb, want) {
					t.Errorf("stderr does not mention %q:\n%s\n"+
						"  consequence: the run is not going to produce another "+
						"event until somebody acts. A follower that stops without "+
						"naming the action leaves the user to run three more verbs "+
						"to learn what this one already knew.", want, errb)
				}
			}
		})
	}
}

// TestRunAttachPrintsWhatArrivesAndNotWhatAlreadyHappened is the verb's contract.
//
// The rows are asserted verbatim, which is unusual in this package and right here:
// docs/design/20-use-cases.md §20.1 publishes `[r1 seq 13] tool.call read src/auth.go`
// as the format, and a Contains check on "tool.call" would pass against a row that
// dropped the run id, the seq, or the argument -- the three things that make a
// follower's output attributable when two of them share a terminal.
//
// The join point is checked as an absence: nothing from seq 1 or 2 may appear. That is
// the half of the specification a test is most likely to leave uncovered, because a
// backfilling implementation looks better in a screenshot -- the user sees history
// immediately -- and is wrong for the reason the file header gives: three other verbs
// already print history, and the interesting part scrolls away.
func TestRunAttachPrintsWhatArrivesAndNotWhatAlreadyHappened(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	runAt(t, dir, id, "feature-team", 1.0, "")
	holdWriterLock(t, dir, id)

	s := startAttach(t, dir, id)
	s.waitForJoin()

	// Three separate appends, so the follower has to notice three arrivals rather
	// than one 3-line write.
	appendToRunLog(t, dir, id, attachToolCall)
	appendToRunLog(t, dir, id, attachLLMResponse)
	appendToRunLog(t, dir, id, attachRunResult)

	out, errb, code := s.finish()
	if code != 0 {
		t.Fatalf("exit %d, want 0 (the run reached a terminal state while attached)\nstdout:\n%s\nstderr:\n%s",
			code, out, errb)
	}

	rows := attachRows(out)
	if len(rows) != 3 {
		t.Fatalf("%d rows on stdout, want 3:\n%s\nstderr:\n%s\n"+
			"  consequence: fewer means an appended event was never shown, which is "+
			"the one thing this verb exists to do. More means history was replayed, "+
			"or a row was printed twice.", len(rows), out, errb)
	}

	// The doc's own rows, character for character.
	for i, want := range []string{
		"[" + id + " seq 3] tool.call read src/auth.go",
		"[" + id + " seq 4] llm.response backend 0.09 USD",
	} {
		if rows[i] != want {
			t.Errorf("row %d is\n  %q\nwant\n  %q\n"+
				"  consequence: this format is published in "+
				"docs/design/20-use-cases.md §20.1. A row missing the run id cannot "+
				"be attributed once two attaches share a terminal, and one missing "+
				"the seq cannot be looked up with `arxi event log`.", i, rows[i], want)
		}
	}
	if !strings.HasPrefix(rows[2], "["+id+" seq 5] run.result") {
		t.Errorf("the last row is %q, want it to start with the run.result header\n"+
			"  consequence: the event that ended the run is the one row a reader "+
			"scrolls back to find.", rows[2])
	}

	// The join point, as an absence.
	for _, before := range []string{"run.started", "stage.entered", "seq 1", "seq 2"} {
		if strings.Contains(out, before) {
			t.Errorf("stdout contains %q, which happened before the attach:\n%s\n"+
				"  consequence: attach joins at the head -- §20.1's first line is "+
				"seq 13, not seq 1. Backfilling makes the events the user is "+
				"waiting for scroll past behind history that `arxi event log`, "+
				"`run replay` and `run show` all print better.", before, out)
		}
	}

	for _, want := range []string{
		attachJoinHeader + " " + id + " at seq 2",
		"pid 4242 is writing",
		id + " succeeded at seq 5",
		"arxi run result " + id,
	} {
		if !strings.Contains(errb, want) {
			t.Errorf("stderr does not mention %q:\n%s\n"+
				"  consequence: the header says who is being waited on and the "+
				"footer says why the waiting stopped. Without them a stream that "+
				"ends is indistinguishable from one that was interrupted.", want, errb)
		}
	}
}

// TestRunAttachPrintsAnEventSplitAcrossTwoWritesOnceAndWhole is the reason this file
// starts the follower before the bytes exist.
//
// A log being appended to by another process can be read at any instant, including the
// instant halfway through a write. Nothing in the commit protocol prevents that: the
// writer's fsync makes bytes durable, not atomic against a concurrent reader. So the
// follower must hold a partial line rather than decode it, and the failure it is
// avoiding is loud -- decodeAttachLine on half a JSON object errors and the process
// exits 1, killing a follow that was working fine.
//
// The torn state is produced deterministically rather than with a sleep. One write
// carries a whole line plus the first half of the next; waiting for the whole line's
// row proves the read that printed it stopped at that newline, which is exactly the
// offset that leaves the half-line held. Then the remainder arrives.
func TestRunAttachPrintsAnEventSplitAcrossTwoWritesOnceAndWhole(t *testing.T) {
	dir := t.TempDir()
	const id = "rmtorn7qk-5c0be118"
	runAt(t, dir, id, "feature-team", 1.0, "")
	holdWriterLock(t, dir, id)

	s := startAttach(t, dir, id)
	s.waitForJoin()

	// The split lands inside the payload object, so neither half is valid JSON --
	// which is what makes this a test of holding rather than of forgiving parsers.
	half := len(attachLLMResponse) / 2
	appendRawToRunLog(t, dir, id, attachToolCall+"\n"+attachLLMResponse[:half])
	s.waitForRow("seq 3")

	appendRawToRunLog(t, dir, id, attachLLMResponse[half:]+"\n")
	appendToRunLog(t, dir, id, attachRunResult)

	out, errb, code := s.finish()
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s\n"+
			"  consequence: exit 1 here means the torn line was decoded instead of "+
			"held, and a reader whose only mistake was watching at the wrong "+
			"microsecond had their follow killed.", code, out, errb)
	}

	rows := attachRows(out)
	if len(rows) != 3 {
		t.Fatalf("%d rows, want 3:\n%s\n"+
			"  consequence: 2 means the reassembled event was dropped; 4 means the "+
			"two halves were each printed, so the log a user reads has an event "+
			"that never happened.", len(rows), out)
	}
	if want := "[" + id + " seq 4] llm.response backend 0.09 USD"; rows[1] != want {
		t.Errorf("the reassembled row is\n  %q\nwant\n  %q\n"+
			"  consequence: whole means whole. A row rebuilt from a partial read "+
			"that lost or duplicated bytes is worse than a missing one, because it "+
			"reads as fact.", rows[1], want)
	}
}

// pendingCommitPath is logstore's uncommitted-batch marker.
//
// Spelled out because logstore exports no path for it -- and then self-checked with
// logstore.BatchInFlight in the test below, so a rename cannot leave this test writing
// a file nobody reads. Without that check, the marker would silently stop existing,
// the follower would print the tail it is supposed to hold, and the test would pass
// for the opposite of the reason it was written.
func pendingCommitPath(dir, id string) string {
	return filepath.Join(runDirOf(dir, id), "pending.commit")
}

// TestRunAttachHoldsBackABatchThatWasNeverCommitted covers the writer dying mid-batch.
//
// This is the case the commit protocol exists for, seen from the reading side. Bytes
// are on disk and syntactically complete, but pending.commit says they are part of a
// batch that never landed, so the next command to open the run truncates them away.
// Printing them would show a user an event, and then a later `arxi event log` on the
// same run would not have it -- the follower would be the only thing that ever claimed
// it happened.
//
// The ordering needs no sleep. The append completes before the lock is released, and
// the loop samples the lock before each read, so the iteration that first sees the
// lock gone has already read those bytes: n > 0 sends it round again, and only the
// following read returns 0 and takes the writer-gone exit with the tail still held.
func TestRunAttachHoldsBackABatchThatWasNeverCommitted(t *testing.T) {
	dir := t.TempDir()
	const id = "rmtunc4bx-7a19d052"
	runAt(t, dir, id, "feature-team", 1.0, "")
	holdWriterLock(t, dir, id)

	if err := os.WriteFile(pendingCommitPath(dir, id), []byte("12\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !logstore.BatchInFlight(runDirOf(dir, id)) {
		t.Fatalf("this test wrote %s but logstore.BatchInFlight says no batch is in flight\n"+
			"  consequence: the marker has been renamed or moved, so the follower "+
			"would treat the tail below as committed and print it. The assertions "+
			"would then pass only because nothing was held back -- the exact "+
			"opposite of what they claim to prove.", pendingCommitPath(dir, id))
	}

	s := startAttach(t, dir, id)
	s.waitForJoin()

	appendToRunLog(t, dir, id, attachToolCall)
	releaseWriterLock(t, dir, id)

	out, errb, code := s.finish()
	if code != exitResultNotYet {
		t.Fatalf("exit %d, want %d (the writer went away and the run had not ended)\nstdout:\n%s\nstderr:\n%s",
			code, exitResultNotYet, out, errb)
	}
	if rows := attachRows(out); len(rows) != 0 {
		t.Errorf("stdout has %d row(s) from an uncommitted batch:\n%s\n"+
			"  consequence: those bytes are rolled back by the next command that "+
			"opens this run, so the follower would be the only witness to an event "+
			"that, afterwards, never existed.", len(rows), out)
	}

	// The count is asserted, not just the sentence. It is the only thing telling the
	// reader how much was withheld, and a wrong number is a claim about their data.
	if want := strconv.Itoa(len(attachToolCall)+1) + " byte(s)"; !strings.Contains(errb, want) {
		t.Errorf("stderr does not say %q:\n%s\n"+
			"  consequence: silence here looks like the log simply stopped, and a "+
			"wrong count misdescribes what is sitting at the end of their log.", want, errb)
	}
	if !strings.Contains(errb, "never committed") {
		t.Errorf("stderr does not explain that the tail was never committed:\n%s", errb)
	}
}

// TestRunAttachJSONIsOneEventPerLineAndNothingElse pins the machine-readable stream.
//
// Two separate promises, and the second is the one that breaks in practice: the rows
// must be NDJSON rather than indented objects, and the human framing -- the join
// header, the footer, the simulated notice -- must stay on stderr. A single line of
// prose on stdout stops `arxi run attach --json | jq` mid-pipeline, and the pipeline is
// the only reason --json exists.
func TestRunAttachJSONIsOneEventPerLineAndNothingElse(t *testing.T) {
	dir := t.TempDir()
	const id = "rmtjson8ec-2f7d4a10"
	runAt(t, dir, id, "feature-team", 1.0, "")
	holdWriterLock(t, dir, id)

	s := startAttach(t, dir, id, "--json")
	s.waitForJoin()

	appendToRunLog(t, dir, id, attachToolCall)
	appendToRunLog(t, dir, id, attachRunResult)

	out, errb, code := s.finish()
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out, errb)
	}

	rows := attachRows(out)
	if len(rows) != 2 {
		t.Fatalf("%d line(s) on stdout, want 2 -- one JSON object per event:\n%s\n"+
			"  consequence: more lines than events means the objects were indented, "+
			"so no line is a complete document and a streaming reader gets a parse "+
			"error on the first one.", len(rows), out)
	}

	var first map[string]any
	for i, row := range rows {
		var e map[string]any
		if err := json.Unmarshal([]byte(row), &e); err != nil {
			t.Fatalf("stdout line %d is not JSON: %v\n  %s\n"+
				"  consequence: with --json every line is a document. One that is "+
				"not ends the consumer's stream, whatever came before it.", i, err, row)
		}
		if i == 0 {
			first = e
		}
	}

	if got, ok := first["seq"].(float64); !ok || int64(got) != 3 {
		t.Errorf("the first object's seq is %v, want 3\n  %s\n"+
			"  consequence: the seq is how a consumer resumes with "+
			"`run replay --until-seq` after a dropped connection.", first["seq"], rows[0])
	}
	if first["type"] != "tool.call" {
		t.Errorf("the first object's type is %v, want tool.call\n  %s", first["type"], rows[0])
	}
	// The payload is the part a human row summarises and JSON must not: emitting the
	// rendered detail string instead of the object would make --json a worse text mode.
	payload, _ := first["payload"].(map[string]any)
	if payload["tool"] != "read" {
		t.Errorf("the first object's payload does not carry the event's own fields: %v\n  %s\n"+
			"  consequence: --json exists so a consumer can read fields the text row "+
			"only summarises. A summarised payload leaves it with less than the text.",
			first["payload"], rows[0])
	}

	for _, human := range []string{attachJoinHeader, "the result:", "Ctrl-C"} {
		if strings.Contains(out, human) {
			t.Errorf("stdout contains the human framing %q:\n%s\n"+
				"  consequence: it belongs on stderr. On stdout it is a line that "+
				"is not an event, which is precisely what a pipeline cannot survive.",
				human, out)
		}
	}
	if !strings.Contains(errb, attachJoinHeader) {
		t.Errorf("with --json the join header vanished entirely instead of moving to stderr:\n%s\n"+
			"  consequence: a person watching a --json follow still needs to know it "+
			"attached, and to what.", errb)
	}
}

// TestRunAttachSuggestsTheRunTheWayTheUserTypedIt is a fix kept from `run replay`.
//
// Every refusal ends in another command. When the user reached the run by path, the
// suggestions have to name that path: `arxi run why r7` does not work from a directory
// where `runs/` is not where the run lives, and the user has no way to tell that the id
// in the message is not the argument they gave. attachView carries both the folded id
// and the typed argument for this reason, and this test is what keeps them from being
// collapsed back into one field by somebody tidying up.
func TestRunAttachSuggestsTheRunTheWayTheUserTypedIt(t *testing.T) {
	dir := t.TempDir()
	const id = "rmtpath3jy-88ae06c1"
	runAt(t, dir, id, "feature-team", 1.0, "")

	arg := "runs/" + id
	out, errb, code := startAttach(t, dir, arg).finish()
	if code != exitResultNotYet {
		t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitResultNotYet, out, errb)
	}
	if want := "arxi run why " + arg; !strings.Contains(errb, want) {
		t.Errorf("stderr does not suggest %q:\n%s\n"+
			"  consequence: the suggestion names an id the user never typed. Run "+
			"from anywhere but the run root it does not resolve, and the reader "+
			"cannot see why -- the two spellings look interchangeable.", want, errb)
	}
}

// TestRunAttachTakesTheRunByShortFlag checks -r reaches this command too.
//
// The letters are global by design -- -r is --run on every command that has a run
// parameter -- and the parser derives them from the registry, so this cannot break
// alone. It can break together, which is why one command per verb asserts it: a
// wrong-command error for -r would otherwise only show up in whichever verb somebody
// happened to try by hand.
func TestRunAttachTakesTheRunByShortFlag(t *testing.T) {
	dir := t.TempDir()
	const id = "rmtshort5ku-13c0fa27"
	succeededAt(t, dir, id)

	out, errb, code := startAttach(t, dir, "-r", id).finish()
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s\n"+
			"  consequence: -r is documented as --run everywhere it exists. A "+
			"command that rejects it teaches the user the letters are per-command "+
			"guesswork.", code, out, errb)
	}
	if !strings.Contains(errb, "already ended") {
		t.Errorf("-r %s did not resolve to the run: stderr is\n%s", id, errb)
	}
}

// TestRunAttachWithNoRunSaysSoRatherThanFollowingNothing covers both empty spellings.
//
// They are refused in different places, and both places matter. Omitting the argument
// is caught by the derived parser, which names the missing parameter from the registry;
// passing an empty one gets past the parser -- it WAS given -- and is caught by the
// command. Without that second guard the empty string reaches resolveRunDir, which
// joins it onto the runs directory and produces the runs directory itself, so the
// follower would report on `runs/` as though it were a run.
//
// Both messages carry the usage block, and the usage block names `arxi event log`,
// because a reader who typed this verb wants events and the sibling verb prints the
// ones that already exist.
func TestRunAttachWithNoRunSaysSoRatherThanFollowingNothing(t *testing.T) {
	dir := t.TempDir()

	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "no argument at all",
			args: nil,
			// From the parser, so the parameter's own name and description appear:
			// the registry is the only place that knows a run id may be positional.
			want: []string{"needs 1 more flag", "run id"},
		},
		{
			name: "an empty argument",
			args: []string{""},
			want: []string{"which run?"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, errb, code := startAttach(t, dir, tc.args...).finish()
			if code != 2 {
				t.Fatalf("exit %d, want 2 (usage)\nstdout:\n%s\nstderr:\n%s\n"+
					"  consequence: anything else means the run id was accepted. "+
					"An empty one resolves to the runs directory, and a follower "+
					"pointed at a directory of runs waits on a log that will "+
					"never be appended to.", code, out, errb)
			}
			for _, want := range append(tc.want,
				"usage: arxi run attach <run>", "arxi event log <run>") {
				if !strings.Contains(errb, want) {
					t.Errorf("stderr does not contain %q:\n%s\n"+
						"  consequence: a follower given no run has nothing to "+
						"follow. Saying which argument is missing, and naming "+
						"the verb that prints the events that already happened, "+
						"is the whole answer.", want, errb)
				}
			}
		})
	}
}
