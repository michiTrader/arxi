package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `arxi run pause`, exercised as a process against real run directories.
//
// # Why a verb this small needs this many tests
//
// The command appends one event with no payload and prints four lines, and
// almost none of what matters about it is in that append. What matters is what
// it REFUSES and what it says while doing it, and every one of those is a
// property of the binary in a real directory:
//
//   - a typo'd run id must not create runs/<typo>/, and logstore.Open MkdirAll's
//     its directory, so the only thing standing between a typo and a junk
//     directory is the order of two calls in this file
//   - a run that is being driven cannot be paused, and the raw lock error's
//     advice ("remove writer.lock by hand") is dangerous in exactly this case
//   - pausing a BLOCKED run overwrites the block in Status, and `run why`
//     branches on Status first (internal/kernel/why.go:44), so the diagnosis
//     stops being reachable one event after this command runs
//
// The last of those has a sibling in `run unpause`, and it is pinned here rather
// than there on purpose: the defect only exists on a run that was paused, so the
// test that finds it has to pause one.

// pausableRun writes a run whose log folds to running, and returns its id.
//
// Hand-written via runAt (runlist_cli_test.go), because a run started with
// `run start --sim` drives itself to a terminal state and a terminal run is the
// one thing this command refuses. A run that is merely running is not a state
// the simulator will hold still in.
func pausableRun(t *testing.T, dir, id string) {
	t.Helper()
	runAt(t, dir, id, "feature-team", 1.0, "")
}

func TestRunPauseRecordsRunPausedAndSaysWhatItStopped(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	pausableRun(t, dir, id)

	out, errb, code := arxiStreams(t, dir, "run", "pause", id)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out, errb)
	}

	// seq 3: the fixture's log ends at seq 2, and the pause is the next event.
	// Asserted as a number because the seq is the handle a reader uses to find
	// this event in the log, and a headline naming the wrong one sends them to
	// somebody else's event.
	if !strings.Contains(out, "run "+id+" paused at seq 3") {
		t.Errorf("the headline does not say what was paused, or where:\n%s", out)
	}
	if !strings.Contains(out, "arxi run unpause "+id) {
		t.Errorf("the way back is not printed as a runnable line:\n%s\n"+
			"  consequence: a pause with no resume command beside it is a run the "+
			"user has to go and look up how to restart.", out)
	}

	ev := lastEvent(t, dir, id)
	if ev["type"] != "run.paused" {
		t.Fatalf("the last event is %v, not run.paused:\n%v", ev["type"], ev)
	}
	if ev["source"] != "human" {
		t.Errorf("source = %v, want human\n%v\n"+
			"  consequence: an audit that cannot tell a person's pause from the "+
			"runtime blocking a run cannot answer why the bill stopped growing.",
			ev["source"], ev)
	}
	if ev["scope"] != "run:"+id {
		t.Errorf("scope = %v, want run:%s\n%v", ev["scope"], id, ev)
	}
	if ts, _ := ev["ts"].(string); ts == "" {
		t.Errorf("ts is empty\n%v\n"+
			"  consequence: this append does not go through the effect runner, so "+
			"nothing else stamps it -- measured on real logs for inbox replies, "+
			"which landed with \"ts\":\"\".", ev)
	}
}

// TestRunPauseSaysAnOpenTurnIsNotInterrupted is the claim most likely to be
// misread, so it is the one pinned hardest.
//
// A pause withholds the NEXT turn: spendingHalted (internal/kernel/decide.go:1176)
// is consulted where a turn would be opened, and nothing anywhere reaches into
// one already open. "paused" read as "stopped mid-call" is the reading that costs
// somebody money they thought they had saved, so the output has to say which it
// is -- and it must not print "none interrupted" as a constant, because on a run
// with an open turn that sentence is the only false line in the output.
func TestRunPauseSaysAnOpenTurnIsNotInterrupted(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	// agent.activated opens backend's turn and leaves it open: nothing here
	// closes it, which is exactly the shape a run has while a member is working.
	runAt(t, dir, id, "feature-team", 1.0,
		`{"id":"e3","seq":3,"type":"agent.activated","actor":"backend","payload":{"agent":"backend"}}
`)

	out, errb, code := arxiStreams(t, dir, "run", "pause", id)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out, errb)
	}
	if !strings.Contains(out, "backend") {
		t.Errorf("the member holding the open turn is not named:\n%s\n"+
			"  consequence: the reader is told a turn is open and not which one, "+
			"so they cannot go and look at it.", out)
	}
	if !strings.Contains(out, "NOT interrupted") {
		t.Errorf("nothing says the open turn survives the pause:\n%s", out)
	}
	if strings.Contains(out, "none interrupted") {
		t.Errorf("a run with an open turn reports that none was interrupted:\n%s\n"+
			"  consequence: it is a constant, and on this run it is false -- the "+
			"one sentence a reader would rely on to mean the call stopped.", out)
	}

	// The turn count is the second half of the same sentence, and it was wrong
	// for the same reason. State.Turns is incremented when a turn OPENS
	// (internal/kernel/decide.go:296) and nothing decrements it, so this fixture
	// -- one activation, no turn_done -- has Turns == 1 with that turn still
	// open. Printing it as finished made the line contradict itself in nine
	// words: "1 turn finished, backend still open".
	if strings.Contains(out, "1 turn finished") {
		t.Errorf("the open turn is counted as finished:\n%s\n"+
			"  consequence: State.Turns counts turns started, so a run whose only "+
			"turn is still open is credited with having completed it.", out)
	}
	if !strings.Contains(out, "no turns finished") {
		t.Errorf("the finished-turn count is not reported honestly:\n%s", out)
	}
}

// TestRunPauseRefusesATerminalRun.
//
// A finished run is not spending anything, so a pause changes nothing about it.
// Appending run.paused anyway would be worse than useless: the reducer ignores
// every event after a terminal one, so the log would carry a human instruction
// that provably did nothing, and `event trace` would show an intervention at a
// moment when there was nothing to intervene in.
func TestRunPauseRefusesATerminalRun(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	succeededAt(t, dir, id)

	out, errb, code := arxiStreams(t, dir, "run", "pause", id)
	if code != 1 {
		t.Fatalf("exit %d, want 1\nstdout:\n%s\nstderr:\n%s", code, out, errb)
	}
	if !strings.Contains(errb, "final") {
		t.Errorf("the refusal does not say the run is over:\n%s", errb)
	}
	if !strings.Contains(errb, "arxi run result "+id) {
		t.Errorf("the refusal does not point at what the run ended with:\n%s\n"+
			"  consequence: somebody pausing a finished run wants to know what it "+
			"did, and this is the command that says.", errb)
	}
	for _, ev := range allEvents(t, dir, id) {
		if ev["type"] == "run.paused" {
			t.Fatalf("a refused pause appended to the log anyway:\n%v", ev)
		}
	}
}

// TestRunPauseTwiceRecordsOnePause.
//
// The reducer would tolerate the second one -- Status is already paused and the
// event sets it again -- which is exactly why the refusal has to live here. A log
// with two run.paused events says a human paused a run twice, and one of those
// two statements is false.
func TestRunPauseTwiceRecordsOnePause(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	pausableRun(t, dir, id)

	if _, errb, code := arxiStreams(t, dir, "run", "pause", id); code != 0 {
		t.Fatalf("the first pause failed: exit %d\n%s", code, errb)
	}

	out, errb, code := arxiStreams(t, dir, "run", "pause", id)
	if code != 1 {
		t.Fatalf("exit %d, want 1\nstdout:\n%s\nstderr:\n%s", code, out, errb)
	}
	if !strings.Contains(errb, "already paused") {
		t.Errorf("the refusal does not say the run is already paused:\n%s", errb)
	}
	if !strings.Contains(errb, "arxi run unpause "+id) {
		t.Errorf("the refusal does not offer the command that would change "+
			"something:\n%s", errb)
	}

	n := 0
	for _, ev := range allEvents(t, dir, id) {
		if ev["type"] == "run.paused" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the log holds %d run.paused events, want 1\n"+
			"  consequence: a second one records a human pausing a run that was "+
			"already stopped, and an audit cannot tell which pause did the work.", n)
	}
}

// TestRunPauseOnADrivenRunDoesNotAdviseDeletingTheLock is a safety test.
//
// A run being driven holds its writer.lock, so the pause fails at
// logstore.Open. LockedError's own text ends with "remove <dir>/writer.lock by
// hand after confirming no process is running", which is sound for a lock left
// by a dead process and dangerous here: this failure happens BECAUSE the writer
// is alive. An operator who learns to delete the lock gets two writers,
// duplicate seq, and a log that no longer folds -- so the message is translated
// rather than passed through.
//
// The lock is written by hand with a pid that is not this test's, because the
// alternative is racing a real driver, and a test that has to win a race to
// assert a message is a test that fails for the wrong reason.
func TestRunPauseOnADrivenRunDoesNotAdviseDeletingTheLock(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	pausableRun(t, dir, id)

	lock := filepath.Join(dir, "runs", id, "writer.lock")
	if err := os.WriteFile(lock, []byte("pid 424242\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, errb, code := arxiStreams(t, dir, "run", "pause", id)
	if code != 1 {
		t.Fatalf("exit %d, want 1\nstdout:\n%s\nstderr:\n%s", code, out, errb)
	}
	if !strings.Contains(errb, "pid 424242") {
		t.Errorf("the refusal does not name the process holding the run:\n%s", errb)
	}
	if strings.Contains(errb, "by hand") {
		t.Errorf("the raw lock error reached the user:\n%s\n"+
			"  consequence: it advises removing writer.lock after confirming no "+
			"process is running, and this refusal happens precisely because one "+
			"is. Two writers produce duplicate seq and an unfoldable log.", errb)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Errorf("the lock of a live writer was removed: %v", err)
	}
	for _, ev := range allEvents(t, dir, id) {
		if ev["type"] == "run.paused" {
			t.Fatalf("a run that could not be locked was paused anyway:\n%v", ev)
		}
	}
}

// TestPausingABlockedRunSaysWhatThePauseHides.
//
// A blocked run can be paused, and doing so costs the user the diagnosis:
// kernel.Explain branches on Status and returns early for StatusPaused
// (internal/kernel/why.go:44), so one event later `arxi run why` says "paused by
// explicit request" and stops naming the block. The block is still in the state;
// the command that reports it no longer looks.
//
// So this is the last moment anything in the binary can say what the run was
// stuck on, and the fixture is blocked with an unanswered question rather than an
// exhausted ceiling (blockedAt spends 0.02 of 1.00) to pin the arm that does not
// involve money.
func TestPausingABlockedRunSaysWhatThePauseHides(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	blockedAt(t, dir, id, "feature-team", 1.0)

	out, errb, code := arxiStreams(t, dir, "run", "pause", id)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out, errb)
	}
	if !strings.Contains(out, "warning") {
		t.Errorf("pausing a blocked run passes without comment:\n%s", out)
	}
	if !strings.Contains(out, "arxi run why "+id) {
		t.Errorf("the warning does not name the command whose answer changes:\n%s", out)
	}
	if !strings.Contains(out, "unanswered question") {
		t.Errorf("the block being masked is not described:\n%s\n"+
			"  consequence: after this event `run why` reports the pause and stops, "+
			"so a reader who did not run it first has no way back to the reason.", out)
	}
}

// TestPausingABudgetBlockStillWarnsWhenItIsResumed is the defect `run pause`
// found in `run unpause`.
//
// unpause's exhausted-budget warning was gated on `pre.Status ==
// kernel.StatusBlocked`, and a pause overwrites Status. So the sequence a user
// naturally performs -- the run blocks on money, they pause it to think, then
// resume it -- skipped the warning entirely, and the run re-blocked on the next
// cost with nothing said. The condition now asks budgetIsExhausted, which reads
// TreeSpentUSD against BudgetUSD and is true whatever the status says.
//
// This runs a real --sim run rather than a hand-written log, because the point is
// the state the reducer actually produces at each of the three steps.
func TestPausingABudgetBlockStillWarnsWhenItIsResumed(t *testing.T) {
	dir := workdir(t)
	run := budgetBlockedRun(t, dir)

	paused := arxi(t, dir, "run", "pause", run)
	if paused.code != 0 {
		t.Fatalf("pausing a budget-blocked run: exit %d\n%s", paused.code, paused.out)
	}
	if !strings.Contains(paused.out, "budget ran out") {
		t.Errorf("the pause does not say the run was blocked on money:\n%s", paused.out)
	}

	got := arxi(t, dir, "run", "unpause", run)
	if got.code != 0 {
		t.Fatalf("resuming a paused run: exit %d\n%s", got.code, got.out)
	}
	for _, want := range []string{"warning", "budget ran out", "--budget"} {
		if !strings.Contains(got.out, want) {
			t.Errorf("the resume warning does not mention %q:\n%s\n"+
				"  consequence: the ceiling is still exhausted, so the run resumes "+
				"and blocks again on the next cost. Gating this on Status == blocked "+
				"dropped it for every run that was paused first.", want, got.out)
		}
	}
}

// TestRunPauseOnANameThatIsNotARunCreatesNothing.
//
// logstore.Open MkdirAll's its directory, so a command that opened the store
// before checking would answer a mistyped id by creating runs/<typo>/ and leaving
// it there -- and the next `arxi run list` would show a run that never existed,
// with an unreadable log. The fold happens first for exactly this reason, and the
// assertion is on the filesystem rather than on the message, because the message
// was already right when the directory was being created.
func TestRunPauseOnANameThatIsNotARunCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	pausableRun(t, dir, id)

	out, errb, code := arxiStreams(t, dir, "run", "pause", "rmthws2dz-deadbeef")
	if code != 1 {
		t.Fatalf("exit %d, want 1\nstdout:\n%s\nstderr:\n%s", code, out, errb)
	}
	if !strings.Contains(errb, "rmthws2dz-deadbeef") {
		t.Errorf("the error does not name what could not be found:\n%s", errb)
	}

	ents, err := os.ReadDir(filepath.Join(dir, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != id {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("the runs directory now holds %v, want only %s\n"+
			"  consequence: logstore.Open creates its directory, so a typo turns "+
			"into a run that `arxi run list` shows and nothing can read.",
			names, id)
	}
}

// TestRunPauseSeparatesTheWrongInvocationsFromTheImpossibleOnes.
//
// Exit 2 is "you typed it wrong", exit 1 is "the run cannot be paused". They are
// asserted together because collapsing them is what makes a script retry a typo
// forever or give up on a run that was merely finished.
//
// The blank id is not hypothetical: `arxi run pause "$RUN"` with RUN unset passes
// a value, so parseInvocation is satisfied, and without the trim check it reaches
// resolveRunDir and reports the runs directory itself as an unreadable run.
//
// --json is an error rather than being ignored, and that is the surface's rule
// rather than this command's opinion: pause declares Mutates, so parseInvocation
// synthesises no --json for it. A mutating verb that accepted the flag and printed
// prose would be worse than one that refuses it -- the caller would parse the
// output and find nothing.
func TestRunPauseSeparatesTheWrongInvocationsFromTheImpossibleOnes(t *testing.T) {
	dir := t.TempDir()
	pausableRun(t, dir, "rmthws2dz-93381f43")

	cases := []struct {
		name string
		args []string
		code int
		says string
	}{
		{"no run id", []string{"run", "pause"}, 2, "needs 1 more flag"},
		{"blank run id", []string{"run", "pause", "  "}, 2, "which run?"},
		{"json on a mutating verb", []string{"run", "pause", "rmthws2dz-93381f43", "--json"}, 2, "json"},
		{"no such run", []string{"run", "pause", "rmthws2dz-deadbeef"}, 1, "rmthws2dz-deadbeef"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, errb, code := arxiStreams(t, dir, tc.args...)
			if code != tc.code {
				t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s",
					code, tc.code, out, errb)
			}
			if !strings.Contains(out+errb, tc.says) {
				t.Errorf("neither stream mentions %q\nstdout:\n%s\nstderr:\n%s",
					tc.says, out, errb)
			}
		})
	}
}

// TestRunPauseUsageSaysWhatAPauseDoesNotDo.
//
// The usage line is where somebody looks after the command refused them, and the
// misreading worth pre-empting there is the expensive one: pause withholds the
// next turn and does not abort one already open. Documentation nobody tests is
// documentation that drifts, so it is asserted.
func TestRunPauseUsageSaysWhatAPauseDoesNotDo(t *testing.T) {
	dir := t.TempDir()
	_, errb, code := arxiStreams(t, dir, "run", "pause")
	if code != 2 {
		t.Fatalf("exit %d, want 2: %s", code, errb)
	}
	for _, want := range []string{"usage: arxi run pause <run>", "not aborted",
		"arxi run unpause <run>"} {
		if !strings.Contains(errb, want) {
			t.Errorf("the usage does not mention %q:\n%s", want, errb)
		}
	}
}
