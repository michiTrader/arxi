package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `arxi run cancel`, exercised as a process against real run directories.
//
// # What is actually being tested
//
// The append is one event and the reducer arm is two lines, so nothing here is
// about whether the status changes. What is worth pinning is the set of claims
// the command makes around it, because each one is a thing a user acts on:
//
//   - it is FINAL, and the log agrees: a second cancel is refused, not recorded,
//     because cancelReason (runresult.go:261) quotes the FIRST run.cancelled and
//     the reducer ignores every event after a terminal one. A second cancel with
//     a better reason would be in the file and in no reading of it.
//   - --reason lands under the key `run result` reads, which is the difference
//     between an annotated abandonment and a run indistinguishable from a failure
//     (docs/design/20-use-cases.md:407)
//   - a question outstanding at the moment of the cancel becomes unanswerable,
//     and `arxi inbox` used to print "backend unblocked" for it anyway

// cancellableRun writes a run whose log folds to running, and returns nothing:
// the id is the caller's, as with pausableRun. Hand-written via runAt rather than
// started with --sim, because a --sim run drives itself to a terminal state and a
// terminal run is the one thing this command refuses.
func cancellableRun(t *testing.T, dir, id string) {
	t.Helper()
	runAt(t, dir, id, "feature-team", 1.0, "")
}

func TestRunCancelRecordsRunCancelledWithTheReason(t *testing.T) {
	dir := t.TempDir()
	const id = "rmtcnl4kq-71c2a0de"
	cancellableRun(t, dir, id)

	const why = "requirement changed, rate limiting is deferred"
	out, errb, code := arxiStreams(t, dir, "run", "cancel", id, "--reason", why)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out, errb)
	}

	// The headline docs/design/20-use-cases.md:406 promises, including the seq:
	// the fixture's log ends at seq 2, so the cancel is seq 3. The seq is the
	// handle a reader uses to find this event, and a headline naming the wrong one
	// sends them to somebody else's event.
	if !strings.Contains(out, "run "+id+" cancelled at seq 3") {
		t.Errorf("the headline does not say what was cancelled, or where:\n%s", out)
	}
	if !strings.Contains(out, "final") {
		t.Errorf("nothing says the cancel cannot be taken back:\n%s\n"+
			"  consequence: a user who reads this as a pause waits for a run that "+
			"will never move again.", out)
	}

	ev := lastEvent(t, dir, id)
	if ev["type"] != "run.cancelled" {
		t.Fatalf("the last event is %v, not run.cancelled:\n%v", ev["type"], ev)
	}
	if ev["source"] != "human" {
		t.Errorf("source = %v, want human\n%v\n"+
			"  consequence: an audit that cannot tell a person's cancel from a run "+
			"the runtime failed cannot answer why the work stopped.", ev["source"], ev)
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

	// The payload key is the one cancelReason reads. A cancel that wrote
	// `why` or `message` would be a run whose reason is in the log and in no
	// reading of it.
	payload, _ := ev["payload"].(map[string]any)
	if payload["reason"] != why {
		t.Errorf("payload reason = %v, want %q\n%v\n"+
			"  consequence: spec/events.md:41 declares run.cancelled with reason?, "+
			"and runresult.go:261 reads that key -- any other name and `run result` "+
			"shows nothing.", payload["reason"], why, ev)
	}
}

// TestRunCancelReasonIsWhatRunResultShows is the cross-command half.
//
// resultText (runresult.go:199) uses the cancel reason as the run's result when
// nothing else recorded one, so these two commands are one feature: the reason is
// written here and read there. Testing them separately would let the key drift
// and both suites keep passing.
func TestRunCancelReasonIsWhatRunResultShows(t *testing.T) {
	dir := t.TempDir()
	const id = "rmtcnl4kq-71c2a0de"
	cancellableRun(t, dir, id)

	const why = "duplicate of the earlier run"
	if _, errb, code := arxiStreams(t, dir, "run", "cancel", id, "--reason", why); code != 0 {
		t.Fatalf("cancelling: exit %d\n%s", code, errb)
	}

	out, errb, code := arxiStreams(t, dir, "run", "result", id)
	// exitResultUnsuccessful: a cancelled run is finished and did not succeed,
	// which is what a poller gates on.
	if code == 0 {
		t.Errorf("`run result` on a cancelled run exited 0\nstdout:\n%s", out)
	}
	if !strings.Contains(out, why) {
		t.Errorf("the reason does not reach `run result`:\n%s\nstderr:\n%s\n"+
			"  consequence: the reason is the only thing distinguishing this run "+
			"from one that failed, and this is the command that reads it.",
			out, errb)
	}
}

// TestRunCancelWithoutAReasonSaysWhatWasLost.
//
// --reason is optional in the surface, so this command may not require it. What it
// can do is say, at the one moment it is still possible to supply, that it cannot
// be supplied later: the log is append-only and a second run.cancelled is ignored.
func TestRunCancelWithoutAReasonSaysWhatWasLost(t *testing.T) {
	dir := t.TempDir()
	const id = "rmtcnl4kq-71c2a0de"
	cancellableRun(t, dir, id)

	out, errb, code := arxiStreams(t, dir, "run", "cancel", id)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out, errb)
	}
	for _, want := range []string{"no reason", "--reason", "arxi run list"} {
		if !strings.Contains(out, want) {
			t.Errorf("a reasonless cancel does not mention %q:\n%s\n"+
				"  consequence: six weeks later `run list` cannot tell this run "+
				"from one that failed, and by then nothing can add the reason.",
				want, out)
		}
	}

	// And the absence is an absent key, not an empty one: resultText treats a
	// non-empty reason as the run's result, so "" would give the run a result
	// that is one blank line.
	ev := lastEvent(t, dir, id)
	payload, _ := ev["payload"].(map[string]any)
	if _, present := payload["reason"]; present {
		t.Errorf("payload carries a reason key with no reason: %v\n"+
			"  consequence: runresult.go prints a non-empty reason as the result, "+
			"so an empty string is a result that is one blank line.", ev)
	}
}

// TestRunCancelTwiceIsRefusedAndTheFirstReasonSurvives.
//
// The reducer would tolerate the second event -- it ignores everything after a
// terminal status -- and that tolerance is exactly the danger. cancelReason quotes
// the FIRST run.cancelled on purpose, so a second cancel carrying a better reason
// writes it where no reading of the log will ever show it. Refusing is the only
// honest answer, and it has to name the reason already on record so the user can
// see what they were about to shadow.
func TestRunCancelTwiceIsRefusedAndTheFirstReasonSurvives(t *testing.T) {
	dir := t.TempDir()
	const id = "rmtcnl4kq-71c2a0de"
	cancellableRun(t, dir, id)

	const first = "abandoned: the requirement moved"
	if _, errb, code := arxiStreams(t, dir, "run", "cancel", id, "--reason", first); code != 0 {
		t.Fatalf("the first cancel failed: exit %d\n%s", code, errb)
	}

	out, errb, code := arxiStreams(t, dir, "run", "cancel", id, "--reason", "second thoughts")
	if code != 1 {
		t.Fatalf("exit %d, want 1\nstdout:\n%s\nstderr:\n%s", code, out, errb)
	}
	if !strings.Contains(errb, "already cancelled") {
		t.Errorf("the refusal does not say the run is already cancelled:\n%s", errb)
	}
	if !strings.Contains(errb, first) {
		t.Errorf("the refusal does not quote the reason on record:\n%s\n"+
			"  consequence: the user cannot see which of the two reasons the log "+
			"will report, and it is not the one they just typed.", errb)
	}

	n := 0
	for _, ev := range allEvents(t, dir, id) {
		if ev["type"] == "run.cancelled" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the log holds %d run.cancelled events, want 1\n"+
			"  consequence: the reducer ignores the second, and cancelReason quotes "+
			"the first, so the extra event is a human decision recorded where "+
			"nothing reads it.", n)
	}
}

// TestRunCancelRefusesARunThatAlreadyFinished.
//
// Different from cancelling twice, and worth its own message: a succeeded run has
// a result, and appending run.cancelled would put a status in the file that the
// run never had. The reducer ignores it, so `run result` would keep saying
// succeeded while the log's last event says cancelled -- two answers to "how did
// this end", from the same directory.
func TestRunCancelRefusesARunThatAlreadyFinished(t *testing.T) {
	dir := t.TempDir()
	const id = "rmtcnl4kq-71c2a0de"
	succeededAt(t, dir, id)

	out, errb, code := arxiStreams(t, dir, "run", "cancel", id)
	if code != 1 {
		t.Fatalf("exit %d, want 1\nstdout:\n%s\nstderr:\n%s", code, out, errb)
	}
	if !strings.Contains(errb, "final") {
		t.Errorf("the refusal does not say the run is over:\n%s", errb)
	}
	if !strings.Contains(errb, "arxi run result "+id) {
		t.Errorf("the refusal does not point at what the run ended with:\n%s", errb)
	}
	for _, ev := range allEvents(t, dir, id) {
		if ev["type"] == "run.cancelled" {
			t.Fatalf("a refused cancel appended to the log anyway:\n%v", ev)
		}
	}
}

// TestRunCancelOnADrivenRunDoesNotAdviseDeletingTheLock is the safety test pause
// has, repeated here because the advice is dangerous per command rather than per
// package: LockedError's text ends with "remove <dir>/writer.lock by hand", which
// is sound for a lock left by a dead process and wrong here -- this refusal
// happens BECAUSE the writer is alive. Two writers produce duplicate seq and a log
// that no longer folds.
func TestRunCancelOnADrivenRunDoesNotAdviseDeletingTheLock(t *testing.T) {
	dir := t.TempDir()
	const id = "rmtcnl4kq-71c2a0de"
	cancellableRun(t, dir, id)

	lock := filepath.Join(dir, "runs", id, "writer.lock")
	if err := os.WriteFile(lock, []byte("pid 424242\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, errb, code := arxiStreams(t, dir, "run", "cancel", id)
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
			"is.", errb)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Errorf("the lock of a live writer was removed: %v", err)
	}
	for _, ev := range allEvents(t, dir, id) {
		if ev["type"] == "run.cancelled" {
			t.Fatalf("a run that could not be locked was cancelled anyway:\n%v", ev)
		}
	}
}

// TestRunCancelOnANameThatIsNotARunCreatesNothing.
//
// logstore.Open MkdirAll's its directory, so a command that opened the store
// before folding would answer a mistyped id by creating runs/<typo>/ and leaving
// it there for `run list` to show. The assertion is on the filesystem rather than
// on the message, because the message was already right while the directory was
// being created.
func TestRunCancelOnANameThatIsNotARunCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	const id = "rmtcnl4kq-71c2a0de"
	cancellableRun(t, dir, id)

	out, errb, code := arxiStreams(t, dir, "run", "cancel", "rmtcnl4kq-deadbeef")
	if code != 1 {
		t.Fatalf("exit %d, want 1\nstdout:\n%s\nstderr:\n%s", code, out, errb)
	}
	if !strings.Contains(errb, "rmtcnl4kq-deadbeef") {
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
		t.Errorf("the runs directory now holds %v, want only %s", names, id)
	}
}

// TestRunCancelSeparatesTheWrongInvocationsFromTheImpossibleOnes.
//
// Exit 2 is "you typed it wrong", exit 1 is "the run cannot be cancelled".
// Collapsing them is what makes a script retry a typo forever or give up on a run
// that was merely finished.
//
// The blank id is not hypothetical: `arxi run cancel "$RUN"` with RUN unset passes
// a value, so parseInvocation is satisfied, and without the trim check it reaches
// resolveRunDir and reports the runs directory itself as an unreadable run. The
// blank --reason is the same argument one level down, and its consequence is
// sharper: a space in the payload is a space printed as the run's result.
func TestRunCancelSeparatesTheWrongInvocationsFromTheImpossibleOnes(t *testing.T) {
	dir := t.TempDir()
	cancellableRun(t, dir, "rmtcnl4kq-71c2a0de")

	cases := []struct {
		name string
		args []string
		code int
		says string
	}{
		{"no run id", []string{"run", "cancel"}, 2, "needs 1 more flag"},
		{"blank run id", []string{"run", "cancel", "  "}, 2, "which run?"},
		{"reason with no value", []string{"run", "cancel", "rmtcnl4kq-71c2a0de", "--reason"}, 2, "needs a value"},
		{"json on a mutating verb", []string{"run", "cancel", "rmtcnl4kq-71c2a0de", "--json"}, 2, "json"},
		{"no such run", []string{"run", "cancel", "rmtcnl4kq-deadbeef"}, 1, "rmtcnl4kq-deadbeef"},
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

// TestRunCancelBlankReasonIsNoReason.
//
// `--reason " "` satisfies the parser and would put a payload key holding one
// space into the log, which resultText then prints as the run's result: a
// deliverable that is one blank line, on a run somebody has to explain later.
func TestRunCancelBlankReasonIsNoReason(t *testing.T) {
	dir := t.TempDir()
	const id = "rmtcnl4kq-71c2a0de"
	cancellableRun(t, dir, id)

	out, errb, code := arxiStreams(t, dir, "run", "cancel", id, "--reason", "   ")
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out, errb)
	}
	if !strings.Contains(out, "no reason") {
		t.Errorf("a blank reason is reported as a reason:\n%s", out)
	}
	ev := lastEvent(t, dir, id)
	payload, _ := ev["payload"].(map[string]any)
	if _, present := payload["reason"]; present {
		t.Errorf("a blank reason was written to the log: %v", ev)
	}
}

// TestRunCancelUsageSaysItCannotBeUndone.
//
// The usage line is where somebody looks after the command refused them, and the
// misreading worth pre-empting there is the one that cannot be repaired: cancel is
// final. It names `run pause` for the reader who wanted to stop spending and keep
// the run, because that is what most people reaching for "stop this" actually
// want.
func TestRunCancelUsageSaysItCannotBeUndone(t *testing.T) {
	dir := t.TempDir()
	_, errb, code := arxiStreams(t, dir, "run", "cancel")
	if code != 2 {
		t.Fatalf("exit %d, want 2: %s", code, errb)
	}
	for _, want := range []string{"usage: arxi run cancel <run>", "FINAL",
		"arxi run pause <run>"} {
		if !strings.Contains(errb, want) {
			t.Errorf("the usage does not mention %q:\n%s", want, errb)
		}
	}
}
