package main

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// `arxi run result`, exercised as a process against real run directories.
//
// # What is actually worth pinning here
//
// The verb prints one string, which makes it look like the least testable of the
// run verbs. It is the opposite: it is the only one whose output somebody pipes
// into another program, so three things that are invisible in a screenshot are the
// whole contract -- which stream the answer goes to, what the exit code says, and
// whether the text is presented as the agent's answer when it is not.
//
// The last of those is the finding this verb was written around. §20.1 of
// docs/design/20-use-cases.md shows `run result` printing "3 risks found: (1) token
// comparison is not constant-time ...", and no log this binary can produce contains
// a sentence like it. stage.submitted declares no payload at all (spec/events.md:59)
// and the reducer's success summaries are constants ("all stages completed",
// internal/kernel/decide.go:489). So the tests below assert BOTH halves: that the
// recorded text is printed, and that it is never offered as the answer of the member
// who submitted last.
//
// # Fixtures are hand-written logs, and every event was checked against the reducer
//
// runAt (runlist_cli_test.go) already writes a run directory and takes extra log
// lines verbatim, so these reuse it. The statuses here cannot be produced on demand
// by `run start --sim`: a cancelled run needs a run.cancelled that nothing in this
// binary emits yet (`run cancel` is declared and unbuilt), and a failed run needs a
// quiescence nobody observes. Each event below was read out of decide.go rather than
// guessed -- run.cancelled sets Status and reads NO key (:59), which is exactly the
// defect one of these tests exists to cover.

// arxiStreams is arxi(), keeping stdout and stderr apart.
//
// The shared helper uses CombinedOutput on purpose, and for most tests that is the
// better default. It cannot express the assertion this verb needs most: that a run
// with no result yet leaves stdout EMPTY. `arxi run result r1 > answer.txt` on a
// still-running run has to produce an empty file, because the next stage of that
// pipeline reads whatever is in it as the answer -- and an explanation of why there
// is no answer is the worst possible content for that file.
func arxiStreams(t *testing.T, dir string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(buildIash(t), args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running arxi %v: %v", args, err)
	}
	return out.String(), errb.String(), code
}

// succeededAt is the shape a completed run really has, submissions included.
//
// TWO stage.submitted events, from different members, because `result_from:
// last_submit` is a choice between them: a fixture with one submission passes
// against an implementation that prints the first, the last, or any of them.
//
// The submits do NOT make this run succeed on their own, which is worth stating
// because it looks like they should. Meeting quorum makes the reducer EMIT
// run.result as an effect, and effects are not in the log until something appends
// them -- Fold sees only these bytes. So the run.result line below is not
// belt-and-braces, it is the only reason this run is terminal.
func succeededAt(t *testing.T, dir, id string) {
	t.Helper()
	runAt(t, dir, id, "feature-team", 1.0,
		`{"id":"e3","seq":3,"type":"stage.submitted","actor":"backend","payload":{"agent":"backend","stage":"execute"}}
{"id":"e4","seq":4,"type":"stage.submitted","actor":"frontend","payload":{"agent":"frontend","stage":"review"}}
{"id":"e5","seq":5,"type":"run.result","payload":{"summary":"all stages completed","result_from":"last_submit"}}
`)
}

func TestRunResultPrintsTheRecordedResultAndExitsZero(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	succeededAt(t, dir, id)

	out, errb, code := arxiStreams(t, dir, "run", "result", id)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out, errb)
	}
	if !strings.Contains(out, "all stages completed") {
		t.Errorf("the recorded summary is not on stdout:\n%s\n"+
			"  consequence: this is the one run verb whose output is piped. A "+
			"result the command holds and does not print makes `arxi run result "+
			"r1 > answer.txt` write an empty file for a run that finished.", out)
	}

	// The answer must be a line of its own, unindented: whatever prefix this
	// command adds ends up inside the file the user redirected to.
	if !strings.Contains(out, "\nall stages completed\n") {
		t.Errorf("the result is not on a bare line of its own:\n%s", out)
	}
}

// TestRunResultNeverPassesTheSummaryOffAsTheAgentsAnswer is the finding this
// command exists to be honest about.
//
// "all stages completed" is a constant in the reducer (internal/kernel/decide.go:489).
// A user who delegated a code review and reads that line as the review has been
// misled by their tooling. Naming the member whose answer it would have been is
// what makes the output actionable rather than merely truthful: it says who to go
// and read.
func TestRunResultNeverPassesTheSummaryOffAsTheAgentsAnswer(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	succeededAt(t, dir, id)

	out, _, _ := arxiStreams(t, dir, "run", "result", id)

	// last_submit is frontend at seq 4, not backend at seq 3.
	if !strings.Contains(out, "frontend at seq 4 (stage review)") {
		t.Errorf("the provenance line does not name the last submission:\n%s", out)
	}
	if strings.Contains(out, "backend at seq 3") {
		t.Errorf("result_from: last_submit resolved to the FIRST submission:\n%s\n"+
			"  consequence: the run is credited to a member who submitted earlier, "+
			"and the reader goes to read the wrong agent's work.", out)
	}
	if !strings.Contains(out, "not frontend's answer") {
		t.Errorf("nothing says the text is not the agent's answer:\n%s\n"+
			"  consequence: stage.submitted declares no payload (spec/events.md), "+
			"so no submission in this log carries text. Printing a reducer "+
			"constant with a member's name beside it and no caveat reads as that "+
			"member's deliverable.", out)
	}
}

// TestRunResultLeavesStdoutEmptyWhenThereIsNoResultYet pins the pipeline contract.
//
// `arxi run result r1 > answer.txt` on a run that is still working must leave
// answer.txt empty. A file containing "run r1 has no result yet" is worse than an
// empty one: the next step reads it as the answer, and an explanation of why there
// is no answer is the most convincing wrong content it could receive.
//
// Exit 3 rather than 1, because 1 already means the command could not do its job.
// A poller that cannot tell "not yet" from "no such run" either gives up on a live
// run or spins forever on a typo.
func TestRunResultLeavesStdoutEmptyWhenThereIsNoResultYet(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	// No extra events: run.started + stage.entered folds to running.
	runAt(t, dir, id, "feature-team", 1.0, "")

	out, errb, code := arxiStreams(t, dir, "run", "result", id)
	if code != exitResultNotYet {
		t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s",
			code, exitResultNotYet, out, errb)
	}
	if out != "" {
		t.Errorf("stdout is not empty for a run with no result:\n%q\n"+
			"  consequence: `arxi run result %s > answer.txt` writes this text "+
			"into the file a pipeline reads as the deliverable.", out, id)
	}
	if !strings.Contains(errb, "no result yet") {
		t.Errorf("stderr does not explain the empty answer:\n%s", errb)
	}
	if !strings.Contains(errb, "arxi run why "+id) {
		t.Errorf("stderr does not point at the command that says what it is "+
			"waiting on:\n%s", errb)
	}
}

// TestRunResultQuotesTheCancelReasonTheReducerDrops is a defect guard, not a
// feature test.
//
// spec/events.md:41 declares run.cancelled with `reason?`, and applyEvent sets
// Status and reads no key from it (internal/kernel/decide.go:59). So a cancelled
// run has an EMPTY State.Result while the reason sits in the log one line above the
// status being printed. Reading the State alone here would print "no result
// recorded" for a run whose own log says why it ended -- a claim the file the
// command just read disproves.
func TestRunResultQuotesTheCancelReasonTheReducerDrops(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	runAt(t, dir, id, "feature-team", 1.0,
		`{"id":"e3","seq":3,"type":"run.cancelled","payload":{"reason":"operator stopped it: wrong branch"}}
`)

	out, errb, code := arxiStreams(t, dir, "run", "result", id)
	if code != exitResultUnsuccessful {
		t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s",
			code, exitResultUnsuccessful, out, errb)
	}
	if !strings.Contains(out, "operator stopped it: wrong branch") {
		t.Errorf("the cancel reason is missing:\n%s\n"+
			"  consequence: the reducer drops it, so this command is the only "+
			"place it can surface. Without it the run reports \"cancelled\" and "+
			"\"no result recorded\" while the log says exactly why.", out)
	}
	if !strings.Contains(out, "recorded in: run.cancelled") {
		t.Errorf("the text is printed without saying which record it came out "+
			"of:\n%s\n  consequence: a cancel reason and an agent summary read "+
			"identically, and they mean opposite things.", out)
	}
	if strings.Contains(out, "no result recorded") {
		t.Errorf("a run with a reason in its log is reported as having none:\n%s", out)
	}
}

// TestRunResultDoesNotCallAFailedRunSucceeded separates the two terminal answers.
//
// The failure here is the one the kernel reaches with no help from any command:
// run.quiescent that nobody watches (decide.go:167) sets StatusFailed and writes the
// diagnosis into Result itself -- no run.result event exists, so this also pins that
// the provenance says "reducer" rather than claiming an event that is not there.
func TestRunResultDoesNotCallAFailedRunSucceeded(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	runAt(t, dir, id, "feature-team", 1.0,
		`{"id":"e3","seq":3,"type":"run.quiescent","payload":{"diagnosis":"no member is scheduled and no timer is pending","stage":"execute"}}
`)

	out, errb, code := arxiStreams(t, dir, "run", "result", id)
	if code != exitResultUnsuccessful {
		t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s",
			code, exitResultUnsuccessful, out, errb)
	}
	if !strings.Contains(out, "failed") {
		t.Errorf("the status is not printed beside the result:\n%s", out)
	}
	if !strings.Contains(out, "no member is scheduled") {
		t.Errorf("the diagnosis the reducer wrote into Result is missing:\n%s", out)
	}
	if !strings.Contains(out, "recorded in: reducer") {
		t.Errorf("a result with no run.result event should not be attributed to "+
			"one:\n%s", out)
	}
	if strings.Contains(out, "not ") && strings.Contains(out, "'s answer") {
		t.Errorf("the misattribution note appears on a run with no submission:\n%s\n"+
			"  consequence: with nothing being misattributed the note is a "+
			"lecture, and a note readers learn to skip is skipped on the run "+
			"where it mattered.", out)
	}
}

// TestRunResultAnswersInJSONWhenTheAnswerIsNegative.
//
// A machine that asked in JSON and got English on stderr has to parse prose to
// learn that it should retry -- and the retry is the whole reason it asked. So the
// human path's "stdout stays empty" rule is deliberately inverted here: with --json
// the negative answer IS the document.
func TestRunResultAnswersInJSONWhenTheAnswerIsNegative(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	runAt(t, dir, id, "feature-team", 1.0, "")

	out, errb, code := arxiStreams(t, dir, "run", "result", id, "--json")
	if code != exitResultNotYet {
		t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s",
			code, exitResultNotYet, out, errb)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s\n"+
			"  consequence: the caller asked for a document and got prose, so it "+
			"cannot tell \"poll again\" from \"this run is broken\".", err, out)
	}
	if got["has_result"] != false {
		t.Errorf("has_result = %v, want false: %s", got["has_result"], out)
	}
	if got["status"] != "running" {
		t.Errorf("status = %v, want running: %s", got["status"], out)
	}
	// has_result is a field of its own rather than an empty result string, because
	// "" answers two different questions: not finished, or finished with nothing.
	if got["result"] != "" {
		t.Errorf("result = %q, want empty: %s", got["result"], out)
	}
}

// TestRunResultJSONSaysTheResultIsNotFromAnAgent.
//
// result_is_from_agent is false on every run this binary can currently produce,
// which makes it look like dead weight. It is the one field that stops a consumer
// treating "all stages completed" as a deliverable, and the day a submission
// carries text it becomes true without that consumer changing a line.
func TestRunResultJSONSaysTheResultIsNotFromAnAgent(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	succeededAt(t, dir, id)

	out, errb, code := arxiStreams(t, dir, "run", "result", id, "--json")
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out, errb)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	if got["result_is_from_agent"] != false {
		t.Errorf("result_is_from_agent = %v, want false\n%s\n"+
			"  consequence: nothing in the event catalogue carries submitted text, "+
			"so claiming true would hand a reducer constant to a consumer as a "+
			"member's answer.", got["result_is_from_agent"], out)
	}
	if got["has_result"] != true {
		t.Errorf("has_result = %v, want true: %s", got["has_result"], out)
	}
	last, ok := got["last_submit"].(map[string]any)
	if !ok {
		t.Fatalf("last_submit is missing, so the consumer cannot find whose "+
			"submission result_from selected:\n%s", out)
	}
	if last["agent"] != "frontend" {
		t.Errorf("last_submit.agent = %v, want frontend: %s", last["agent"], out)
	}
}

// TestRunResultDoesNotDrawAnArrowItCannotResolve.
//
// This test is why printResultProvenance has three arms. run.result carries the
// result_from that was in force (decide.go:490 writes it), and NOTHING reads that
// value back anywhere in the tree -- no code selects a submission by it. The first
// version of this command printed `result_from: <whatever> → <the last submission>`
// regardless, so a run recorded under first_submit named the member who submitted
// LAST as the source of its result. Both halves of that line are true separately,
// which is what made it unnoticeable.
func TestRunResultDoesNotDrawAnArrowItCannotResolve(t *testing.T) {
	dir := t.TempDir()
	const id = "rmthws2dz-93381f43"
	runAt(t, dir, id, "feature-team", 1.0,
		`{"id":"e3","seq":3,"type":"stage.submitted","actor":"backend","payload":{"agent":"backend","stage":"execute"}}
{"id":"e4","seq":4,"type":"stage.submitted","actor":"frontend","payload":{"agent":"frontend","stage":"review"}}
{"id":"e5","seq":5,"type":"run.result","payload":{"summary":"all stages completed","result_from":"first_submit"}}
`)

	out, errb, code := arxiStreams(t, dir, "run", "result", id)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out, errb)
	}

	// The logged value wins over the blueprint, which records no result_from and
	// therefore normalises to last_submit (internal/kernel/config.go:121). What was
	// in force when the run finished is the fact worth printing.
	if !strings.Contains(out, "result_from: first_submit") {
		t.Errorf("the result_from on run.result was not preferred over the "+
			"blueprint's default:\n%s", out)
	}
	if strings.Contains(out, "first_submit → ") {
		t.Errorf("an unresolvable result_from is drawn as a resolved selection:\n%s\n"+
			"  consequence: the arrow points at the LAST submission, so a run "+
			"recorded under first_submit credits the wrong member.", out)
	}
	if !strings.Contains(out, "frontend at seq 4") {
		t.Errorf("the last submission is not reported at all:\n%s\n"+
			"  consequence: dropping it leaves the reader with a summary and "+
			"nobody to go and ask about it.", out)
	}
}

// TestRunResultSeparatesTheFourNegativeAnswers.
//
// Four ways for this command not to print a result, and a script has to be able to
// tell them apart from the exit code alone: still working (3), ended badly (4), the
// run cannot be read (1), the invocation was wrong (2). Collapsing any pair is what
// makes a poller either give up on a live run or spin forever on a typo, so the
// codes are asserted together rather than one per test.
func TestRunResultSeparatesTheFourNegativeAnswers(t *testing.T) {
	dir := t.TempDir()
	runAt(t, dir, "rmthws2dz-93381f43", "feature-team", 1.0, "")
	succeededAt(t, dir, "rmthws2dz-00000001")

	cases := []struct {
		name string
		args []string
		code int
		says string
	}{
		{"still working", []string{"run", "result", "rmthws2dz-93381f43"}, exitResultNotYet, "no result yet"},
		{"succeeded", []string{"run", "result", "rmthws2dz-00000001"}, 0, "all stages completed"},
		{"no such run", []string{"run", "result", "rmthws2dz-deadbeef"}, 1, "rmthws2dz-deadbeef"},
		// Two different wrong invocations, and they are both exit 2 by different
		// routes: the missing positional is caught by parseInvocation from the
		// registry's declaration, while a blank one gets past it (a value WAS given)
		// and is stopped by this command. The second is not hypothetical -- `arxi run
		// result "$RUN"` with RUN unset is exactly it, and without the trim check it
		// reaches resolveRunDir and reports the runs directory itself as unreadable.
		{"no run id", []string{"run", "result"}, 2, "needs 1 more flag"},
		{"blank run id", []string{"run", "result", "  "}, 2, "which run?"},
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

// TestRunResultUsageNamesTheExitCodes.
//
// The exit code is half of this verb's contract and it is invisible: nothing in the
// output tells a reader that 3 means "poll again". The usage line is the only place
// it can be discovered without reading the source, which is why it is asserted --
// documentation nobody tests is documentation that drifts.
func TestRunResultUsageNamesTheExitCodes(t *testing.T) {
	dir := t.TempDir()
	_, errb, code := arxiStreams(t, dir, "run", "result")
	if code != 2 {
		t.Fatalf("exit %d, want 2: %s", code, errb)
	}
	for _, want := range []string{"exit 0", "3 no result yet", "4 it ended without succeeding"} {
		if !strings.Contains(errb, want) {
			t.Errorf("the usage does not mention %q:\n%s", want, errb)
		}
	}
}
