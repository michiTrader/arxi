package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for `arxi run unpause`, run as the binary.
//
// These run the real binary because the point of every one of them is what the
// BINARY did. The three defects this file exists to pin were all found by
// walking the command by hand, and none would have shown up in a unit test of
// the functions involved:
//
//   - two lines of one command disagreed about the same ceiling (0.0050 vs 0.01)
//   - `run start` did the same thing in its own banner, rounding a ceiling UP
//   - a failed run left writer.lock behind, so the NEXT command refused
//
// The last is the one a unit test could not have reached at all: it is a
// property of os.Exit skipping deferred calls, which only exists in a real
// process.

// budgetBlockedRun starts a simulated run that genuinely runs out of budget,
// and returns its id.
//
// It is not a hand-written log, unlike blockedRun in inbox_cli_test.go, and the
// difference is deliberate. A hand-written log states what I believe a budget
// block looks like; this one is whatever the reducer actually produces, so a
// change in how blocks are recorded is caught here instead of being encoded
// twice and agreeing with itself.
//
// --budget 0.005 against a fake turn cost of 0.01 blocks on the first turn.
func budgetBlockedRun(t *testing.T, dir string) string {
	t.Helper()

	bp := filepath.Join(dir, "team.yaml")
	if err := os.WriteFile(bp, []byte(exampleBlueprint), 0o644); err != nil {
		t.Fatal(err)
	}

	got := arxi(t, dir, "run", "start", bp, "ship the thing", "--sim", "--budget", "0.005")
	if got.code != 0 {
		t.Fatalf("starting a run to block: exit %d\n%s", got.code, got.out)
	}

	// The id is read off the directory rather than parsed out of the banner:
	// the banner's wording is a thing other tests are entitled to change, and
	// the run's location is not.
	ents, err := os.ReadDir(filepath.Join(dir, "runs"))
	if err != nil || len(ents) != 1 {
		t.Fatalf("expected exactly one run dir under %s: %v (%d entries)", dir, err, len(ents))
	}
	return ents[0].Name()
}

// exampleBlueprint is examples/feature-team.yaml, inline.
//
// Inline and not read from ../../examples: the tests run the binary in a temp
// directory, and a run that reached out of it would be testing this repository's
// layout rather than the command.
const exampleBlueprint = `name: feature-team
members:
  - {name: backend,  role: implementer, tools: [read, write, bash]}
  - {name: frontend, role: implementer, tools: [read, write]}
  - {name: security, role: reviewer, tools: [read], advisory: true}
stages:
  - {name: build,  advance_when: all, timeout_ms: 1800000, on_timeout: escalate}
  - {name: review, advance_when: any}
`

// lastEvent reads the final line of a run's log.
//
// It reads the FILE and does not fold, because these assertions are about what
// the binary wrote. Folding would test the reducer's reading of the event, which
// is what the kernel tests already do -- and would pass even if the payload had
// been written under a key the reducer happened to tolerate.
func lastEvent(t *testing.T, dir, run string) map[string]any {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(dir, "runs", run, "events.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")

	var ev map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &ev); err != nil {
		t.Fatalf("last log line does not parse: %v\n%s", err, lines[len(lines)-1])
	}
	return ev
}

// eventOfType returns the last event of one type in a run's log.
//
// It exists because lastEvent stopped being the right question once a resume
// DRIVES the run: the event a test wants to inspect is no longer at the tip,
// because executing it appended everything it caused. A test that keeps reading
// the tip is then asserting against run.result and reporting the wrong failure
// -- "last event is run.result, want run.unpaused" says nothing about the field
// the test was actually checking.
//
// The last of its type rather than the first, so a test that resumes twice sees
// the resume it just performed.
func eventOfType(t *testing.T, dir, run, typ string) map[string]any {
	t.Helper()

	var found map[string]any
	for _, ev := range allEvents(t, dir, run) {
		if ev["type"] == typ {
			found = ev
		}
	}
	if found == nil {
		t.Fatalf("run %s has no %s event in its log", run, typ)
	}
	return found
}

// countEvents counts events of one type in a run's log.
func countEvents(t *testing.T, dir, run, typ string) int {
	t.Helper()

	n := 0
	for _, ev := range allEvents(t, dir, run) {
		if ev["type"] == typ {
			n++
		}
	}
	return n
}

// allEvents decodes a run's whole log.
func allEvents(t *testing.T, dir, run string) []map[string]any {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(dir, "runs", run, "events.ndjson"))
	if err != nil {
		t.Fatal(err)
	}

	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("run %s has an unreadable log line: %v\n%s", run, err, line)
		}
		out = append(out, ev)
	}
	return out
}

// TestOneCeilingIsPrintedAsOneNumber is the first walk defect.
//
// A run started with --budget 0.005 had its ceiling reported three ways: the
// start banner said 0.01, the block summary said 0.0050, and the raise said
// "0.01 -> 10.00". The raise is the worst of the three, because it puts BOTH
// ceilings on one line: the same command contradicted itself about a number it
// had just read out of a single field.
//
// 0.01 is also the wrong direction. Rounding a spend ceiling UP shows the reader
// more headroom than the run has, so the block that follows looks premature and
// the command that reported it looks broken.
func TestOneCeilingIsPrintedAsOneNumber(t *testing.T) {
	dir := workdir(t)

	// The banner, first: this is where the third instance lived.
	bp := filepath.Join(dir, "team.yaml")
	if err := os.WriteFile(bp, []byte(exampleBlueprint), 0o644); err != nil {
		t.Fatal(err)
	}
	start := arxi(t, dir, "run", "start", bp, "ship it", "--sim", "--budget", "0.005")
	if start.code != 0 {
		t.Fatalf("exit %d: %s", start.code, start.out)
	}
	if strings.Contains(start.out, "budget 0.01 USD") {
		t.Errorf("the start banner rounded a ceiling of 0.005 UP to 0.01:\n%s\n\n"+
			"The summary four lines below says 0.0050. One command, one field, "+
			"two numbers -- and the banner's is the one that promises headroom "+
			"the run does not have.", start.out)
	}
	if !strings.Contains(start.out, "0.0050") {
		t.Errorf("the start banner does not show the ceiling at a precision "+
			"that can represent it:\n%s", start.out)
	}

	ents, err := os.ReadDir(filepath.Join(dir, "runs"))
	if err != nil || len(ents) != 1 {
		t.Fatalf("expected one run dir: %v", err)
	}
	run := ents[0].Name()

	// And the raise, which prints two ceilings on one line.
	got := arxi(t, dir, "run", "unpause", run, "--budget", "10")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.out)
	}
	if strings.Contains(got.out, "0.01 ->") {
		t.Errorf("the raise reports the old ceiling as 0.01, which is not the "+
			"0.0050 the run was blocked against:\n%s\n\nBoth numbers are on "+
			"one line here, so this is the command disagreeing with itself.",
			got.out)
	}
	if !strings.Contains(got.out, "0.0050 -> 10.00") {
		t.Errorf("the raise does not name both ceilings exactly:\n%s\n"+
			"want \"0.0050 -> 10.00\"", got.out)
	}
}

// TestASuccessfulResumeReleasesTheWriterLock is the easy half.
//
// Exactly one writer per log is required, so a lock that outlives its holder
// locks the run out. The ordinary path releases it via defer; this only checks
// that the ordinary path is ordinary.
//
// The interesting half is TestAFatalUnderTheLockDoesNotLockOutTheNextCommand.
func TestASuccessfulResumeReleasesTheWriterLock(t *testing.T) {
	dir := workdir(t)
	run := budgetBlockedRun(t, dir)

	if got := arxi(t, dir, "run", "unpause", run, "--budget", "5"); got.code != 0 {
		t.Fatalf("resume: exit %d\n%s", got.code, got.out)
	}
	if _, err := os.Stat(filepath.Join(dir, "runs", run, "writer.lock")); err == nil {
		t.Errorf("writer.lock survived the command that took it")
	}

	// The next command reaches the run rather than being turned away at the
	// door. It is REFUSED -- the raise let the rehearsal finish, and a finished
	// run has no work to hand back -- and that refusal is the proof: it is a
	// judgement about the run's status, which can only be made by a command that
	// got far enough to read the log.
	//
	// This used to assert exit 0, back when a simulated resume appended and
	// stopped so the run stayed resumable forever. Asserting the exit code alone
	// would now pass for the wrong reason if the lock DID survive, since a lock
	// refusal also exits non-zero -- so the message is what is checked.
	got := arxi(t, dir, "run", "unpause", run, "--budget", "9")
	if strings.Contains(got.out, "writer.lock") ||
		strings.Contains(got.out, "open for writing") {
		t.Fatalf("the next command was refused by a leftover lock: exit %d\n%s",
			got.code, got.out)
	}
	if !strings.Contains(got.out, "final") {
		t.Errorf("the next command did not reach the run's state: exit %d\n%s",
			got.code, got.out)
	}
}

// TestAFatalUnderTheLockDoesNotLockOutTheNextCommand is the second walk defect,
// and the version of it that actually catches the bug.
//
// The first version of this test resumed a --sim run twice and asserted no lock
// was left. It passed. It also passed with the fix REVERTED, which is how the
// mistake was found: a --sim resume returns before the stopped-early branch, so
// the os.Exit path being guarded was never reached. A guard that cannot fail is
// not a guard, and the only way to know is to break the code and watch.
//
// So this reaches a path that IS taken. An unparseable policy file makes
// openPolicies fatal, and openPolicies is called AFTER the lock is taken and
// AFTER run.unpaused is appended. Measured before the fix: writer.lock survived
// holding pid 5871, and the next command refused with
//
//	already open for writing by pid 5871 ... remove writer.lock by hand
//
// for a run whose log says it was successfully resumed.
//
// The advice is also dangerous to generalise: an operator who learns to delete
// writer.lock after a crash will eventually delete a live one.
func TestAFatalUnderTheLockDoesNotLockOutTheNextCommand(t *testing.T) {
	dir := workdir(t)
	run := budgetBlockedRun(t, dir)

	// The run must not be simulated, or the command returns before it wires up
	// the executor and the fatal is never reached. Patching the log is the
	// honest way to say "pretend this was live": the flag is read off
	// run.started, which is exactly the fact being overridden.
	log := filepath.Join(dir, "runs", run, "events.ndjson")
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	patched := strings.Replace(string(raw), `"simulated":true`, `"simulated":false`, 1)
	if patched == string(raw) {
		t.Fatalf("run.started does not carry simulated:true, so this test is "+
			"no longer reaching the drive path:\n%s", raw)
	}
	if err := os.WriteFile(log, []byte(patched), 0o644); err != nil {
		t.Fatal(err)
	}

	// policies/ is relative to the working directory, so this is the real file
	// the real command reads.
	if err := os.MkdirAll(filepath.Join(dir, "policies"), 0o755); err != nil {
		t.Fatal(err)
	}
	pol := filepath.Join(dir, "policies", "backend.json")
	if err := os.WriteFile(pol, []byte("NOT JSON {{{\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := arxi(t, dir, "run", "unpause", run, "--budget", "5")
	if !strings.Contains(got.out, "backend.json") {
		t.Fatalf("the bad policy file was not reached, so this test is not "+
			"exercising a fatal under the lock:\n%s", got.out)
	}

	if _, err := os.Stat(filepath.Join(dir, "runs", run, "writer.lock")); err == nil {
		t.Errorf("writer.lock survived a fatal exit.\n\n" +
			"os.Exit skips defers, so the lock outlived the process holding " +
			"it. The run's log already says it was resumed, and the next " +
			"command on it will refuse with a dead pid.")
	}

	// The symptom, from the user's side: the next command must work once the
	// bad file is gone. This is what actually got reported before the fix.
	if err := os.Remove(pol); err != nil {
		t.Fatal(err)
	}
	next := arxi(t, dir, "run", "unpause", run, "--budget", "9")
	if strings.Contains(next.out, "already open for writing") {
		t.Errorf("the next command hit a stale writer lock:\n%s\n\n"+
			"The previous process is gone and the run is fine; only the lock "+
			"file disagrees.", next.out)
	}
	if next.code != 0 {
		t.Errorf("the next command failed: exit %d\n%s", next.code, next.out)
	}
}

// TestTheRaiseReachesTheLogWhereTheReducerReadsIt checks the two halves agree.
//
// The reducer honours budget_usd on run.unpaused; this test is the other side of
// that contract, that the CLI writes the field under that name. A private
// convention here and a correct reducer there is a gap neither test would find
// alone.
func TestTheRaiseReachesTheLogWhereTheReducerReadsIt(t *testing.T) {
	dir := workdir(t)
	run := budgetBlockedRun(t, dir)

	if got := arxi(t, dir, "run", "unpause", run, "--budget", "5"); got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.out)
	}

	// Looked up by type rather than taken off the tip: the resume DRIVES the
	// run, so the events it caused sit above the run.unpaused it wrote.
	ev := eventOfType(t, dir, run, "run.unpaused")
	// SourceHuman is load-bearing: raising a ceiling is an authorisation, and an
	// audit that cannot say whether a human or the runtime raised a budget
	// cannot answer the only question anybody asks about a bill.
	if ev["source"] != "human" {
		t.Errorf("run.unpaused is attributed to %v, want human", ev["source"])
	}
	// Ts is stamped by the command, because nothing else will: this append does
	// not go through the effect runner. Inbox replies landed with "ts":"" for
	// exactly this reason.
	if ts, _ := ev["ts"].(string); ts == "" {
		t.Errorf("run.unpaused has no timestamp:\n%v\n\nNothing else will stamp "+
			"it -- a human typed this command, so it does not pass through the "+
			"effect runner.", ev)
	}

	pl, _ := ev["payload"].(map[string]any)
	if pl == nil {
		t.Fatalf("run.unpaused carries no payload, so the raise never reached "+
			"the reducer:\n%v", ev)
	}
	if got, ok := pl["budget_usd"].(float64); !ok || got != 5 {
		t.Errorf("payload budget_usd is %v, want 5 -- this is the exact key the "+
			"reducer reads, and run.started writes", pl["budget_usd"])
	}
}

// TestAPlainResumeWritesNoCeiling guards the direction that costs money.
//
// §20.6's first example is a bare `arxi run unpause r1`. If that wrote
// budget_usd: 0, the reducer would read an absent ceiling as a limit of zero --
// and while raiseBudget refuses to lower, a command that emits a field it does
// not mean is one reducer change away from setting every plain resume to an
// unsatisfiable ceiling.
func TestAPlainResumeWritesNoCeiling(t *testing.T) {
	dir := workdir(t)
	run := budgetBlockedRun(t, dir)

	if got := arxi(t, dir, "run", "unpause", run); got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.out)
	}

	ev := lastEvent(t, dir, run)
	pl, _ := ev["payload"].(map[string]any)
	if _, present := pl["budget_usd"]; present {
		t.Errorf("a plain resume wrote budget_usd anyway: %v\n\nNo ceiling was "+
			"given, so none should be recorded: the field means \"raise to\", "+
			"and 0 is not a ceiling any run can spend under.", pl)
	}
}

// TestResumingABudgetBlockWithoutARaiseSaysWhatWillHappen.
//
// It is allowed, because it is occasionally what somebody means. It is warned
// about because the common case is somebody who read the remedy and dropped the
// flag, and a run that blocks again immediately with no explanation looks like
// the command failed.
func TestResumingABudgetBlockWithoutARaiseSaysWhatWillHappen(t *testing.T) {
	dir := workdir(t)
	run := budgetBlockedRun(t, dir)

	got := arxi(t, dir, "run", "unpause", run)
	if got.code != 0 {
		t.Fatalf("a resume without a raise should be allowed: exit %d\n%s",
			got.code, got.out)
	}
	for _, want := range []string{"warning", "budget ran out", "block again", "--budget"} {
		if !strings.Contains(got.out, want) {
			t.Errorf("the warning does not mention %q:\n%s", want, got.out)
		}
	}
	// The remedy is offered as a whole command, not as prose about a flag: the
	// reader is already stuck, and this is the moment to be copy-pasteable.
	if !strings.Contains(got.out, "arxi run unpause "+run+" --budget") {
		t.Errorf("the warning does not print the remedy as a runnable line:\n%s",
			got.out)
	}
}

// TestALowerCeilingIsRefusedAndSaysWhy.
//
// A ceiling under the spend re-breaches on the next event, which is the loop a
// raise exists to end -- and "unpause" is not the verb for tightening a budget.
// Refusing in the CLI rather than the reducer is the division of labour the
// project already uses: the reducer ignores what it will not honour, because it
// has nobody to talk to, and the CLI is where a person is told why.
func TestALowerCeilingIsRefusedAndSaysWhy(t *testing.T) {
	dir := workdir(t)

	// The run's OWN ceiling is what a lower one is compared against, so no
	// preparatory raise is needed. There used to be one -- a raise to 5, then a
	// refused lower to 1 -- which stopped working when a simulated resume began
	// driving: the raise let the rehearsal finish, and the second command was
	// then refused for being terminal, which is a different refusal than the one
	// under test. Comparing against the ceiling the run started with tests the
	// same rule with nothing in between to go wrong.
	run := budgetBlockedRun(t, dir) // started with --budget 0.005

	got := arxi(t, dir, "run", "unpause", run, "--budget", "0.001")
	if got.code != 2 {
		t.Fatalf("lowering a ceiling exited %d, want 2 (misuse):\n%s",
			got.code, got.out)
	}
	// Both numbers, so the user can see the comparison that was made rather
	// than being told their number was wrong.
	for _, want := range []string{"0.0010", "0.0050", "not above"} {
		if !strings.Contains(got.out, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, got.out)
		}
	}
	// And the alternative, because somebody lowering a budget usually wants to
	// stop paying, which is a different command.
	if !strings.Contains(got.out, "run cancel") {
		t.Errorf("the refusal does not point at the command that does stop "+
			"the spending:\n%s", got.out)
	}
	// Nothing was written: the refusal happens before the append.
	if ev := lastEvent(t, dir, run); ev["type"] == "run.unpaused" {
		if pl, _ := ev["payload"].(map[string]any); pl != nil {
			if v, _ := pl["budget_usd"].(float64); v == 1 {
				t.Errorf("the refused ceiling reached the log anyway: %v", ev)
			}
		}
	}
}

// TestAFinishedRunIsNotResumed.
//
// A terminal run has no work to hand back, and appending run.unpaused to it
// would record a human intervening in a run that had already ended -- which is
// the sort of thing `event trace` is read to rule out.
func TestAFinishedRunIsNotResumed(t *testing.T) {
	dir := workdir(t)
	run := budgetBlockedRun(t, dir)

	// Cancelled is terminal and reachable without inventing a log by hand.
	log := filepath.Join(dir, "runs", run, "events.ndjson")
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte(`{"seq":99,"id":"c1","ts":"2026-01-01T00:00:00Z",`+
		`"type":"run.cancelled","source":"human","payload":{"reason":"done"}}`+"\n")...)
	if err := os.WriteFile(log, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	got := arxi(t, dir, "run", "unpause", run)
	if got.code != 1 {
		t.Fatalf("resuming a cancelled run exited %d, want 1:\n%s", got.code, got.out)
	}
	for _, want := range []string{"cancelled", "final"} {
		if !strings.Contains(got.out, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, got.out)
		}
	}
	// The log is unchanged: a refusal that appended would be the exact dishonesty
	// this guard exists to prevent.
	if ev := lastEvent(t, dir, run); ev["type"] != "run.cancelled" {
		t.Errorf("the refusal appended to the log anyway: last event is %v",
			ev["type"])
	}
}

// TestASimulatedRunIsResumedByTheSameFakeExecutor.
//
// A log whose run.started says simulated:true was produced by exec.Fake.
// Driving it with a LIVE executor would call real models and charge real money
// for the continuation of a rehearsal. That is the danger, and it is what this
// guard is about.
//
// The flag comes off the LOG and not from a --sim on this command, because the
// run already answered the question when it started. Re-asking would let the two
// answers differ, and the direction that costs money is the one a user hits by
// forgetting a flag.
//
// # Why this test was rewritten
//
// It used to be TestASimulatedRunIsNotDrivenByAResume, and it asserted that the
// resume stopped at the append: no turns, nothing driven. That assertion was
// wider than the reason above supports. The reason forbids a LIVE executor; the
// assertion forbade driving at all, and the two are only the same thing if the
// fake executor is not an option -- which it is, because `run start --sim`
// already uses it.
//
// The gap that overreach left was measured, not theorised. Nothing else in the
// binary drives a run: `run unpause` was the only verb that did, and it refused
// to for a rehearsal. So a simulated run could never be advanced after `run
// start` returned, and `run prompt` on one appended a cause that was silently
// discarded -- while `run why` went on recommending the prompt the user had
// just sent. A rehearsal that cannot be rehearsed past its first stop is not a
// rehearsal of the thing it claims to simulate.
//
// So the property pinned here is the one the rationale actually states: the
// continuation is taken by the fake, no model is called, and the run advances.
func TestASimulatedRunIsResumedByTheSameFakeExecutor(t *testing.T) {
	dir := workdir(t)
	run := budgetBlockedRun(t, dir)

	got := arxi(t, dir, "run", "unpause", run, "--budget", "5")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.out)
	}
	if !strings.Contains(got.out, "--sim") {
		t.Errorf("the resume does not say which executor continued it:\n%s\n\n"+
			"A user who is not told cannot know whether that output cost money.",
			got.out)
	}
	// It says the thing that makes the drive safe, in the terms a user cares
	// about. "fake executor" alone is jargon; the money is the point.
	if !strings.Contains(got.out, "no money is spent") {
		t.Errorf("the resume does not say the continuation is free:\n%s", got.out)
	}
	// The resume reached the log AND was acted on. Asserting only the event
	// would pass against the defect this rewrite exists to fix, because the
	// broken version appended too.
	if !strings.Contains(got.out, "turns:") {
		t.Errorf("the simulated run was not driven, so the resume resumed "+
			"nothing:\n%s\n\nNothing else in the binary drives a run, so a "+
			"rehearsal that stops here can never be advanced.", got.out)
	}

	// And it really moved: the log has turns the resume produced, not just the
	// run.unpaused it wrote. Read off the log rather than the banner, because
	// the banner's wording is a thing other tests may change.
	if n := countEvents(t, dir, run, "agent.turn_done"); n < 3 {
		t.Errorf("the resumed rehearsal produced %d turns in total, want more "+
			"than the 2 the blocked run already had", n)
	}
	// Every response came from the fake. A live provider is unreachable in the
	// test environment anyway, but that is an accident of the sandbox and not a
	// guarantee, so the log is checked instead.
	if n := countEvents(t, dir, run, "llm.response"); n < 3 {
		t.Errorf("expected simulated responses from the fake executor, got %d", n)
	}
}

// TestUnpauseNeedsARunAndSaysSo.
func TestUnpauseNeedsARunAndSaysSo(t *testing.T) {
	got := arxi(t, workdir(t), "run", "unpause")
	if got.code != 2 {
		t.Fatalf("a missing run id exited %d, want 2:\n%s", got.code, got.out)
	}
	if !strings.Contains(got.out, "run") {
		t.Errorf("the error does not name what is missing:\n%s", got.out)
	}
}

// TestAnUnknownRunIsNotAnInternalError.
//
// The failure mode being guarded is a stack trace or a bare ENOENT for a
// mistyped id. The user's next question is "where does it look?", so the answer
// includes the directory it searched.
func TestAnUnknownRunIsNotAnInternalError(t *testing.T) {
	got := arxi(t, workdir(t), "run", "unpause", "no-such-run")
	if got.code != 1 {
		t.Fatalf("an unknown run exited %d, want 1:\n%s", got.code, got.out)
	}
	for _, want := range []string{"no-such-run", "runs"} {
		if !strings.Contains(got.out, want) {
			t.Errorf("the error does not mention %q:\n%s", want, got.out)
		}
	}
	if strings.Contains(got.out, "panic") || strings.Contains(got.out, "goroutine") {
		t.Errorf("a mistyped run id produced a crash:\n%s", got.out)
	}
}

// TestAGarbledBudgetIsRefusedBeforeTheLogIsTouched.
//
// A resume is a write. Refusing the number after appending would leave a run
// resumed against a ceiling the user never got to see rejected -- so the parse
// happens before the run is even opened, and this test proves the ordering by
// pointing at a run that does not exist. A command that opened the run first
// would complain about the run instead.
func TestAGarbledBudgetIsRefusedBeforeTheLogIsTouched(t *testing.T) {
	got := arxi(t, workdir(t), "run", "unpause", "no-such-run", "--budget", "banana")
	if got.code != 2 {
		t.Fatalf("a garbled budget exited %d, want 2:\n%s", got.code, got.out)
	}
	if !strings.Contains(got.out, "budget") {
		t.Errorf("the error does not say which flag was wrong:\n%s", got.out)
	}
	if strings.Contains(got.out, "holds no event log") {
		t.Errorf("the run was opened before the number was checked:\n%s\n\n"+
			"The ordering matters: the parse must fail before anything is "+
			"appended.", got.out)
	}
}

// TestAZeroCeilingIsRefused.
//
// --budget 0 reads as "let it spend nothing", which is not a resume. The user
// most likely wants to stop the run, and that has its own verb.
func TestAZeroCeilingIsRefused(t *testing.T) {
	dir := workdir(t)
	run := budgetBlockedRun(t, dir)

	got := arxi(t, dir, "run", "unpause", run, "--budget", "0")
	if got.code != 2 {
		t.Fatalf("--budget 0 exited %d, want 2:\n%s", got.code, got.out)
	}
	if !strings.Contains(got.out, "run cancel") {
		t.Errorf("the refusal does not point at the command that stops a run:\n%s",
			got.out)
	}
}

// TestARunCanBeNamedByItsDirectory.
//
// --dir exists on `run start`, so a run started elsewhere cannot be resumed by
// id at all. Deciding by "does this hold an event log" rather than by "does it
// look like a path" means the answer does not depend on whether the user typed
// a slash.
func TestARunCanBeNamedByItsDirectory(t *testing.T) {
	dir := workdir(t)
	run := budgetBlockedRun(t, dir)

	got := arxi(t, dir, "run", "unpause", filepath.Join("runs", run), "--budget", "5")
	if got.code != 0 {
		t.Fatalf("naming a run by its directory failed: exit %d\n%s", got.code, got.out)
	}
	// By type, not off the tip: the resume drives, so run.unpaused is followed
	// by everything it caused.
	if ev := eventOfType(t, dir, run, "run.unpaused"); ev["source"] != "human" {
		t.Errorf("the resume did not reach the run named by path: %v", ev)
	}
}

// TestTheRemedyTheToolRecommendsIsImplemented closes the loop on the whole step.
//
// `run why` prints a command for a budget block. The gap this step existed to
// close was that the binary refused the command it recommends, which is the
// worst kind of gap because it is found by the person already in trouble. So the
// remedy is taken from why's own output and run verbatim.
func TestTheRemedyTheToolRecommendsIsImplemented(t *testing.T) {
	dir := workdir(t)
	run := budgetBlockedRun(t, dir)

	// why reads a state file, not a log, so the recommendation is checked
	// against the command the reducer's explainer emits.
	if got := arxi(t, dir, "run", "unpause", run, "--budget", "5"); got.code != 0 {
		t.Fatalf("the recommended remedy was refused: exit %d\n%s\n\n"+
			"This is the gap the step closed: a command the tool prints and "+
			"the binary will not run.", got.code, got.out)
	}

	// And it did the thing it claims. This used to be checked by raising again
	// to a lower figure and reading the refusal, which stopped working once a
	// simulated resume drove the run: the first raise now lets the rehearsal
	// FINISH, so the second command is refused for being terminal instead.
	//
	// Finishing is a better proof than the refusal ever was. The remedy exists
	// to end a budget block, and a run that reached a terminal status did more
	// than accept the number -- it spent under it and got somewhere. The old
	// check only established that a second command remembered the figure.
	st := arxi(t, dir, "run", "show", run)
	if st.code != 0 {
		t.Fatalf("run show after the remedy: exit %d\n%s", st.code, st.out)
	}
	if strings.Contains(st.out, "blocked") {
		t.Errorf("the remedy was accepted but the run is still blocked:\n%s\n\n"+
			"Raising the ceiling that stopped it is supposed to let it continue.",
			st.out)
	}
	// The ceiling in the log is the raised one, read where the reducer reads it.
	pl, _ := eventOfType(t, dir, run, "run.unpaused")["payload"].(map[string]any)
	if pl == nil || pl["budget_usd"] != float64(5) {
		t.Errorf("the raise did not reach the log as 5: %v", pl)
	}
}
