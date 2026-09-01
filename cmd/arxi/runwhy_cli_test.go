package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Process-level guards for `arxi run why`.
//
// These run the built binary against run directories on disk, because that is
// the only level at which the bug this command was built to fix is visible. The
// gap was never in kernel.Explain, which had been tested since the kernel was
// written: it was that no caller existed, so `arxi run why <id>` -- the exact
// string three other commands print as advice -- answered "declared in the
// surface but not implemented yet". A unit test on Explain passed throughout.

// The remedy the binary prints must be a command the binary accepts.
//
// THE DEFECT: printRunSummary, printRunShow and cmdRunUnpause all told the user
// to run `arxi run why <id>`. Typing it answered:
//
//	arxi run why is declared in the surface but not implemented yet.
//
// This asserts on the class rather than on one caller: it takes the advice out
// of `run show`'s own output and runs it, so the two can never drift apart. If
// a future change makes run show suggest a different spelling, this test
// follows it there instead of pinning the string it happened to print today.
// The fixture is a plain RUNNING run, and that is not incidental. run show
// picks its closing line by what is true of the run: a blocked run with a
// pending question is sent to `arxi inbox`, a paused one to `run unpause`, and
// only an unfinished run with neither gets `run why`. A budget-blocked fixture
// here made this test fail while the code was correct -- run show was right to
// suggest the inbox -- so the fixture is chosen to reach the arm under test.
func TestRunWhyAnswersTheCommandTheBinaryToldYouToRun(t *testing.T) {
	dir := workdir(t)
	runAt(t, dir, "advice-000000001", "team", 5,
		`{"id":"e3","seq":3,"type":"agent.activated","actor":"backend","payload":{"agent":"backend"}}
`)

	shown := arxi(t, dir, "run", "show", "advice-000000001")
	advice := ""
	for _, line := range strings.Split(shown.out, "\n") {
		if i := strings.Index(line, "arxi run why"); i >= 0 {
			advice = strings.TrimSpace(line[i:])
			break
		}
	}
	if advice == "" {
		t.Fatalf("run show no longer suggests `arxi run why`, so this guard is "+
			"pointing at nothing and must be re-aimed:\n%s", shown.out)
	}

	// The advice is executed as written, minus the binary name.
	args := strings.Fields(advice)[1:]
	got := arxi(t, dir, args...)
	if got.code != 0 {
		t.Fatalf("the binary printed %q as the way to diagnose a stopped run, and "+
			"then refused it (exit %d):\n%s", advice, got.code, got.out)
	}
	if strings.Contains(got.out, "not implemented yet") {
		t.Fatalf("following the tool's own advice reports the command missing:\n%s", got.out)
	}
}

// A diagnosis with no remedy fails the only promise the command makes.
//
// THE DEFECT, found by running it and not by reading it: on a real run stopped
// by its budget, `run why` printed a cause tree and an EMPTY remedy list, while
// `run show` on the same run said "1 pending question / it is waiting on you".
//
// The cause is worth stating because it is the same shape as the run show bug:
// Explain built remedies only from members in MemberWaiting, and a run halted by
// budget has nobody waiting -- spawnCauses parks causes while spendingHalted, so
// every member sits idle. The wait graph was empty and the run was still stuck.
// The surface declares this command as "explain why a run is not advancing, and
// how to unblock it"; without the second half it is a pretty-printer.
func TestRunWhyOffersAWayOutOfABudgetBlockedRun(t *testing.T) {
	dir := workdir(t)
	showBlocked(t, dir, "no-remedy-0000001")

	got := arxi(t, dir, "run", "why", "no-remedy-0000001")
	if got.code != 0 {
		t.Fatalf("exit %d:\n%s", got.code, got.out)
	}
	if !strings.Contains(got.out, "possible remedies:") {
		t.Fatalf("a stopped run was diagnosed with no way out offered. The command "+
			"declares itself as \"and how to unblock it\":\n%s", got.out)
	}
	// The pending question is the actionable thing here, so it must be named
	// and not merely counted.
	if !strings.Contains(got.out, "inbox-1") {
		t.Errorf("the pending question is not named, so the user cannot act on it:\n%s", got.out)
	}
	if !strings.Contains(got.out, "arxi inbox approve inbox-1") {
		t.Errorf("the remedy is not an executable command:\n%s", got.out)
	}
}

// A breached budget must be stated as a breach.
//
// The line "budget: 0.0200 of 0.0010 USD spent in the tree" is only alarming to
// a reader who compares the two numbers, and 20x over budget is not a detail to
// leave to the reader's arithmetic. run show needed the same correction, which
// is why this is a separate guard: the two projections read the same field and
// were wrong about it in the same way.
func TestRunWhySaysTheBudgetIsExhaustedRatherThanLeavingArithmetic(t *testing.T) {
	dir := workdir(t)
	showBlocked(t, dir, "over-budget-00001")

	got := arxi(t, dir, "run", "why", "over-budget-00001")
	if !strings.Contains(got.out, "exhausted") {
		t.Errorf("a run 20x over budget does not say the budget is exhausted; the "+
			"breach is left implicit in two numbers:\n%s", got.out)
	}
	if !strings.Contains(got.out, "--budget") {
		t.Errorf("nothing tells the user how to raise the budget that stopped them:\n%s", got.out)
	}
}

// The remedy must name the run, not a placeholder.
//
// walkCause's budget arm returns the literal "arxi run unpause <run> --budget
// <higher>", because a Member does not know its run id. A user copying that gets
// a shell error. The run-level remedy substitutes the real id, and this pins it:
// the point of the remedy list is that its lines can be pasted.
func TestRunWhyRemedyNamesTheRunSoItCanBePasted(t *testing.T) {
	dir := workdir(t)
	showBlocked(t, dir, "paste-me-0000001")

	got := arxi(t, dir, "run", "why", "paste-me-0000001")
	for _, line := range strings.Split(got.out, "\n") {
		if !strings.Contains(line, "run unpause") {
			continue
		}
		if strings.Contains(line, "<run>") {
			t.Errorf("the remedy still contains a placeholder where the run id "+
				"belongs, so pasting it fails: %q", strings.TrimSpace(line))
		}
		if !strings.Contains(line, "paste-me-0000001") {
			t.Errorf("the remedy does not name the run it is about: %q", strings.TrimSpace(line))
		}
	}
}

// An answered question must stop being offered as the fix.
//
// The log is append-only, so replying to a question does not remove it from
// Inbox: the reducer sets Replied and leaves the item. Counting the whole slice
// is exactly the bug `run list` had, where an answered approval printed ASKS=1
// while `arxi inbox` said there was nothing pending. Here the cost is higher
// than a wrong count: the user is told to go approve something they already
// approved.
func TestRunWhyStopsOfferingAQuestionOnceItIsAnswered(t *testing.T) {
	dir := workdir(t)
	runAt(t, dir, "answered-00000001", "team", 5,
		`{"id":"e3","seq":3,"type":"inbox.created","payload":{"inbox_id":"inbox-1","kind":"tool_approval","question":"approve bash?","agent":"backend","on_timeout":"deny"}}
{"id":"e4","seq":4,"type":"inbox.replied","payload":{"inbox_id":"inbox-1","reply":"approve"}}
`)

	got := arxi(t, dir, "run", "why", "answered-00000001")
	if strings.Contains(got.out, "arxi inbox approve inbox-1") {
		t.Errorf("why offers to approve a question that was already answered, "+
			"sending the user to redo settled work:\n%s", got.out)
	}
	if strings.Contains(got.out, "waits on you") {
		t.Errorf("an answered question is still described as waiting on the user:\n%s", got.out)
	}
}

// One remedy must not be printed twice.
//
// Remedies now come from two places: the member wait graph and the inbox. A
// member blocked on approval is blocked BY a pending question, so both produce
// `arxi inbox approve inbox-1`. Two identical lines under "possible remedies"
// read as two separate steps to take.
func TestRunWhyDoesNotPrintTheSameRemedyTwice(t *testing.T) {
	dir := workdir(t)
	// tool.call_denied, with an underscore. Spelling it "tool.call.denied" by
	// analogy with the other dotted names produced a fixture the reducer
	// ignored entirely: the member stayed `thinking`, no inbox item was
	// created, and the test failed against correct code. Checked against
	// kernel.ToolCallDenied rather than guessed a second time.
	runAt(t, dir, "dedup-0000000001", "team", 5,
		`{"id":"e3","seq":3,"type":"agent.activated","actor":"backend","payload":{"agent":"backend"}}
{"id":"e4","seq":4,"type":"tool.call_denied","actor":"backend","payload":{"tool":"bash","policy":"ask"}}
`)

	got := arxi(t, dir, "run", "why", "dedup-0000000001", "--json")
	var w struct {
		Fix []string `json:"fix"`
	}
	if err := json.Unmarshal([]byte(got.out), &w); err != nil {
		t.Fatalf("--json is not parseable: %v\n%s", err, got.out)
	}
	seen := map[string]int{}
	for _, f := range w.Fix {
		seen[f]++
	}
	for f, n := range seen {
		if n > 1 {
			t.Errorf("the remedy %q is listed %d times, which reads as %d things "+
				"to do:\n%v", f, n, n, w.Fix)
		}
	}
	if len(w.Fix) == 0 {
		t.Error("a member blocked on approval produced no remedy at all")
	}
}

// A run id must not be reported as a missing file.
//
// THE DEFECT: `arxi why <id>` answered "open <id>: no such file or directory".
// Every message in this binary that mentions why prints a RUN ID after it, so
// that is the argument users arrive with -- and the answer described their stuck
// run as a missing file, sending them to look for one that was never meant to
// exist. The alias must resolve runs like the long spelling does.
func TestWhyAliasResolvesARunAndNotJustAFile(t *testing.T) {
	dir := workdir(t)
	showBlocked(t, dir, "alias-0000000001")

	got := arxi(t, dir, "why", "alias-0000000001")
	if got.code != 0 {
		t.Fatalf("`arxi why <run>` failed with exit %d:\n%s", got.code, got.out)
	}
	if strings.Contains(got.out, "no such file") {
		t.Fatalf("a run id is reported as a missing file:\n%s", got.out)
	}
	if !strings.Contains(got.out, "alias-0000000001") {
		t.Errorf("the diagnosis does not name the run:\n%s", got.out)
	}
}

// Both spellings must say the same thing.
//
// `run why` and `why` share a resolver and a renderer precisely so they cannot
// diverge. If they ever do, the first thing to diverge is the remedy list --
// the part a user copies and runs -- and no test would notice, because each
// spelling would still look correct on its own.
func TestWhyAndRunWhyDoNotDisagree(t *testing.T) {
	dir := workdir(t)
	showBlocked(t, dir, "same-00000000001")

	long := arxi(t, dir, "run", "why", "same-00000000001")
	short := arxi(t, dir, "why", "same-00000000001")
	if long.out != short.out {
		t.Errorf("the two spellings of one command disagree.\n--- run why ---\n%s\n--- why ---\n%s",
			long.out, short.out)
	}
}

// The file form must keep working, because it is the proof of the reducer.
//
// `run why` needs no executor, no store and no run directory -- only a State.
// The scenario file is how that is demonstrated, and accepting run ids must not
// have cost it. This reads the golden the kernel maintains, so the two cannot
// drift.
func TestRunWhyStillExplainsAStateFileWithNoRunDirectory(t *testing.T) {
	dir := workdir(t)
	golden, err := os.ReadFile(filepath.Join("..", "..", "testdata", "scenarios", "blocked-on-approval.json"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "scenario.json")
	if err := os.WriteFile(path, golden, 0o644); err != nil {
		t.Fatal(err)
	}

	got := arxi(t, dir, "run", "why", "scenario.json")
	if got.code != 0 {
		t.Fatalf("a state file with no run directory was refused (exit %d):\n%s", got.code, got.out)
	}
	if !strings.Contains(got.out, "possible remedies:") {
		t.Errorf("the file form lost its remedies:\n%s", got.out)
	}
}

// A wrong argument must say what was tried, in both readings.
//
// The argument is either a run or a file, and the tool cannot know which the
// user meant. Reporting only "no such file" sends somebody who typed a run id
// looking for a file; reporting only "no such run" hides a typo in a path. Both
// are named, and so is the way to find the right one.
func TestRunWhyExplainsBothWaysItFailedToResolve(t *testing.T) {
	dir := workdir(t)

	got := arxi(t, dir, "run", "why", "not-a-thing-0001")
	if got.code == 0 {
		t.Fatalf("a nonexistent run exited 0:\n%s", got.out)
	}
	for _, want := range []string{"as a run:", "as a file:", "arxi run list"} {
		if !strings.Contains(got.out, want) {
			t.Errorf("the failure does not mention %q, so the user cannot tell which "+
				"reading was attempted:\n%s", want, got.out)
		}
	}
}

// A finished run must not be handed remedies for a problem it does not have.
//
// A succeeded run is not stuck, and the whole cause tree is inapplicable to it.
// Offering "raise the budget" or "approve inbox-1" on a run that completed would
// invent a problem.
func TestRunWhyDoesNotDiagnoseAFinishedRun(t *testing.T) {
	dir := workdir(t)
	runAt(t, dir, "finished-00000001", "team", 5,
		`{"id":"e3","seq":3,"type":"llm.response","actor":"backend","payload":{"cost_usd":0.01}}
{"id":"e4","seq":4,"type":"run.result","payload":{"status":"succeeded","result":"all stages completed"}}
`)

	got := arxi(t, dir, "run", "why", "finished-00000001")
	if got.code != 0 {
		t.Fatalf("exit %d:\n%s", got.code, got.out)
	}
	if !strings.Contains(got.out, "succeeded") {
		t.Errorf("a finished run is not reported as finished:\n%s", got.out)
	}
	if strings.Contains(got.out, "possible remedies:") {
		t.Errorf("a run that completed is offered remedies, which invents a "+
			"problem it does not have:\n%s", got.out)
	}
}

// --json must carry the structure, not the rendering.
//
// The text form is for a human and the JSON is for a program, and the reason to
// pin it is that the tree is the interesting part: depth is what makes the cause
// chain machine-readable rather than a list of sentences.
func TestRunWhyJSONCarriesTheCauseTree(t *testing.T) {
	dir := workdir(t)
	showBlocked(t, dir, "json-00000000001")

	got := arxi(t, dir, "run", "why", "json-00000000001", "--json")
	var w struct {
		RunID string `json:"run_id"`
		Lines []struct {
			Depth int    `json:"depth"`
			Text  string `json:"text"`
		} `json:"lines"`
		Fix []string `json:"fix"`
	}
	if err := json.Unmarshal([]byte(got.out), &w); err != nil {
		t.Fatalf("--json is not parseable: %v\n%s", err, got.out)
	}
	if w.RunID != "json-00000000001" {
		t.Errorf("run_id is %q", w.RunID)
	}
	if len(w.Lines) == 0 {
		t.Fatal("the cause tree is empty")
	}
	if len(w.Fix) == 0 {
		t.Error("the remedies are missing from the JSON, so a program reading this " +
			"cannot act on the diagnosis")
	}
	// The tree must actually be a tree: a nested cause exists.
	deep := false
	for _, l := range w.Lines {
		if l.Depth > 0 {
			deep = true
		}
	}
	if !deep {
		t.Error("every line is at depth 0, so the cause chain is flat and the " +
			"structure carries nothing the text did not")
	}
	// The text form must not leak into the JSON.
	if strings.Contains(got.out, "└─") {
		t.Error("the JSON contains the tree-drawing characters of the text form")
	}
}
