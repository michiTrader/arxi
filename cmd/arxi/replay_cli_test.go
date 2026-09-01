package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Tests for `arxi run replay`, run as the binary.
//
// This verb makes a claim no other verb in the tool makes: that running it
// changes nothing. Every other command here is tested by looking at what it
// wrote; this one is tested by proving it wrote nothing, which is a different
// kind of assertion and needs a different kind of guard --
// TestReplayLeavesTheRunDirectoryByteForByteIdentical below reads the whole
// directory before and after rather than trusting the absence of a banner.
//
// The defect that running it found is worth stating, because it is invisible on
// the happy path and the happy path is what a reader checks. On a run directory
// whose NAME differs from the run id recorded inside its log -- which is what a
// copy made for inspection is -- the closing suggestion read
//
//	fold all of it: arxi run replay rmtizyc28-ba2261a1
//
// naming the pristine original rather than the damaged copy on screen. Following
// it silently answers a question about a different log.
// TestTheCommandsReplaySuggestsNameTheRunTheUserAsked is the guard, and it needs
// a directory whose name and recorded id disagree to see anything at all.
//
// stagedRun writes a run whose log crosses a stage boundary, and returns its id.
//
// Hand-written, unlike most fixtures in this package, and for one reason: the seq
// numbers have to be known to the test. §20.9's contract is about the state at a
// PARTICULAR seq, and a log produced by `run start` renumbers itself whenever the
// reducer emits one more derived event -- so a test written against a real run
// would assert on seq 11 today and silently be asserting on the wrong event after
// any change to what the loop records.
//
// Seq 7 is stage.advanced from build to review, which is the exact event
// docs/design/20-use-cases.md:521 puts on the replay headline. That is why this
// fixture has two stages and a submission: with one stage there is no advance to
// land on, and the arrow rendering -- the one piece of formatting replay does not
// inherit from `event log` -- would go unexercised.
//
// The cost on seq 4 is 0.25 against a ceiling of 10, deliberately far from both
// zero and the ceiling. A fixture that spent nothing cannot tell the two spend
// lines apart, which is the whole point of TestTheTwoSpendFiguresAreNotTheSame.
func stagedRun(t *testing.T, dir string) string {
	t.Helper()

	const id = "r1"
	run := filepath.Join(dir, "runs", id)
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatal(err)
	}

	bp := "name: feature-team\n" +
		"members:\n" +
		"  - {name: backend,  role: implementer, tools: [read, write]}\n" +
		"  - {name: frontend, role: implementer, tools: [read]}\n" +
		"stages:\n" +
		"  - {name: build, advance_when: all}\n" +
		"  - {name: review, advance_when: any}\n"
	if err := os.WriteFile(filepath.Join(run, "blueprint.snapshot.yaml"),
		[]byte(bp), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run, "events.ndjson"),
		[]byte(stagedLog), 0o644); err != nil {
		t.Fatal(err)
	}
	return id
}

// stagedLog is stagedRun's log, kept apart so the fixtures below can damage a
// copy of it without rebuilding the whole thing.
//
// Eight events, seq 1..8, no gaps. The head is 8 and the run is still running
// there, which matters: a fixture that ended would make the not-at-the-head note
// read "where the run is succeeded" and hide the case where a replay stops short
// of a run that is still going.
const stagedLog = `{"id":"e1","seq":1,"ts":"2026-01-01T00:00:00Z","type":"run.started","source":"human","payload":{"actor":"feature-team","run_id":"r1","budget_usd":10,"simulated":true}}
{"id":"e2","seq":2,"ts":"2026-01-01T00:00:01Z","type":"stage.entered","source":"system","payload":{"stage":"build","index":0}}
{"id":"e3","seq":3,"ts":"2026-01-01T00:00:02Z","type":"agent.activated","source":"system","payload":{"agent":"backend"}}
{"id":"e4","seq":4,"ts":"2026-01-01T00:00:03Z","type":"llm.response","source":"model","payload":{"agent":"backend","cost_usd":0.25,"simulated":true}}
{"id":"e5","seq":5,"ts":"2026-01-01T00:00:04Z","type":"agent.turn_done","source":"system","payload":{"agent":"backend"}}
{"id":"e6","seq":6,"ts":"2026-01-01T00:00:05Z","type":"stage.submitted","source":"agent","payload":{"agent":"backend","stage":"build"}}
{"id":"e7","seq":7,"ts":"2026-01-01T00:00:06Z","type":"stage.advanced","source":"runtime","payload":{"from":"build","to":"review","to_index":1}}
{"id":"e8","seq":8,"ts":"2026-01-01T00:00:07Z","type":"stage.entered","source":"system","payload":{"stage":"review","index":1}}
`

// copyRun clones a run directory under a new id, letting the caller rewrite the
// log, and returns the new id.
//
// A copy rather than an edit in place because the tests that damage a log and the
// tests that read a healthy one run in the same directory, and because the copy
// is itself the condition one guard needs: its NAME is not the run id inside its
// log, which is the case that found the suggestion defect.
func copyRun(t *testing.T, dir, from, to, log string) string {
	t.Helper()

	src, dst := filepath.Join(dir, "runs", from), filepath.Join(dir, "runs", to)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	bp, err := os.ReadFile(filepath.Join(src, "blueprint.snapshot.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "blueprint.snapshot.yaml"), bp, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "events.ndjson"), []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
	return to
}

// dropSeq returns the log with one line removed, by seq.
//
// Used to build the one log shape no healthy run has: a hole. `logstore.Open`
// refuses a log with a gap, and replay does not go through it -- reading bytes
// without the writer lock is what lets a replay run alongside a live run -- so
// this fixture exercises the only reader in the tree that has to cope.
func dropSeq(log string, seq int) string {
	needle := `"seq":` + strconv.Itoa(seq) + `,`
	var kept []string
	for _, line := range strings.Split(strings.TrimRight(log, "\n"), "\n") {
		if !strings.Contains(line, needle) {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n") + "\n"
}

// TestTheHeadlineIsTheEventAtTheSeqAskedFor pins §20.9's transcript.
//
// The headline is the one line a reader of a replay looks at first, and it has to
// name the event that produced the state below it -- not the head, and not the
// event after. §20.9 writes it as `stage.advanced build → review`, and the arrow
// is the part worth pinning: `from=build to=review` carries the same information
// and reads as two unrelated keys, so it is rendered specially, so it can break
// specially.
func TestTheHeadlineIsTheEventAtTheSeqAskedFor(t *testing.T) {
	dir := workdir(t)
	id := stagedRun(t, dir)

	got := arxi(t, dir, "run", "replay", id, "--until-seq", "7")
	if got.code != 0 {
		t.Fatalf("replaying %s to seq 7: exit %d, wanted 0\n%s", id, got.code, got.out)
	}

	head := firstLine(got.out)
	for _, want := range []string{"seq 7 of 8", "stage.advanced", "build → review"} {
		if !strings.Contains(head, want) {
			t.Errorf("the replay headline does not contain %q:\n  %s\n"+
				"  consequence: docs/design/20-use-cases.md:521 shows this line as "+
				"`[replay] seq 44 stage.advanced build → review`, and it is the only "+
				"line saying WHICH event the state below belongs to. Naming a "+
				"different event, or dropping the direction of the transition, makes "+
				"the state underneath unattributable.", want, head)
		}
	}

	// The state has to be the one at seq 7, not the one at the head. Seq 8 enters
	// review too, so the stage cannot tell them apart -- the event on the headline
	// is what does, which is why this assertion is about the headline pairing and
	// the one below is about the seq the state line names.
	if !strings.Contains(got.out, "state at seq 7:") {
		t.Errorf("the state line does not say which seq it is the state at:\n%s\n"+
			"  consequence: a replay prints one state out of a log holding many. "+
			"Without the seq on it, the reader cannot tell whether they are looking "+
			"at the seq they asked for or the head.", got.out)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// TestTheTwoSpendFiguresAreNotTheSame is the defect in §20.9's own transcript.
//
// That transcript prints ONE money line:
//
//	spend: 0.0000 USD (replay does not execute effects)
//
// Zero is the truth about what the replaying process spent, and it is a lie about
// the run being replayed, which had burned 0.25 by seq 7 of this fixture. A
// debugging tool that reports a run's spend as zero is the "confidently wrong"
// failure ADR-0002 is about, and it is worse coming from the tool the operator
// reached for to find out what happened.
//
// So both figures are printed, labelled, and this test fails if they collapse back
// into agreement. The fixture spends 0.25 of 10 specifically so that they CAN
// disagree: on a run that spent nothing, one line and two say the same thing.
func TestTheTwoSpendFiguresAreNotTheSame(t *testing.T) {
	dir := workdir(t)
	id := stagedRun(t, dir)

	got := arxi(t, dir, "run", "replay", id, "--until-seq", "7")
	if got.code != 0 {
		t.Fatalf("replaying %s: exit %d\n%s", id, got.code, got.out)
	}

	replay := lineWith(t, got.out, "replay spend:")
	recorded := lineWith(t, got.out, "recorded spend:")

	if !strings.Contains(replay, "0 USD") {
		t.Errorf("the replay's own spend is not zero: %q\n"+
			"  consequence: this is the claim the verb exists to make -- "+
			"docs/design/20-use-cases.md:530, \"spend is exactly zero because "+
			"replay is the fold with no executor\". A nonzero figure here means "+
			"either something was executed or the number is invented.", replay)
	}
	if !strings.Contains(replay, "replay does not execute effects") {
		t.Errorf("the zero is unexplained: %q\n"+
			"  consequence: a bare 0 next to a run that cost money reads as a bug in "+
			"the accounting. The parenthetical is what makes it a statement rather "+
			"than a suspicious number, and §20.9 writes it out for that reason.", replay)
	}
	if !strings.Contains(recorded, "0.25") {
		t.Errorf("the recorded spend does not report the 0.25 this run really spent: %q\n"+
			"  consequence: the two figures have collapsed into one, which is the "+
			"defect in §20.9's transcript: the operator is told the run spent nothing. "+
			"They are separate lines because they answer separate questions -- what "+
			"this command cost, and what the run cost.", recorded)
	}
}

// lineWith returns the one output line containing needle, failing if there is not
// exactly one.
//
// Exactly one, not the first: two lines matching "spend:" would mean the label a
// test is asserting on no longer identifies a single figure, and picking the first
// would decide the outcome by position.
func lineWith(t *testing.T, out, needle string) string {
	t.Helper()
	var hits []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, needle) {
			hits = append(hits, line)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0]
	case 0:
		t.Fatalf("no output line contains %q:\n%s", needle, out)
	}
	t.Fatalf("%d output lines contain %q, so this assertion no longer names one "+
		"figure:\n%s", len(hits), needle, strings.Join(hits, "\n"))
	return ""
}

// TestReplayLeavesTheRunDirectoryByteForByteIdentical measures "no executor".
//
// Every other guard here reads what the command PRINTED, and printing "replay does
// not execute effects" is not evidence that it did not. This one reads the run
// directory before and after -- every file, its whole contents -- and fails on any
// difference at all.
//
// It is the strongest assertion in the file and the cheapest to get wrong by
// accident: appending an event, refreshing state.snapshot.json, or taking the
// writer lock are all one line of ordinary-looking code, and any of them would
// make a replay of a live run corrupt the run it was reading.
func TestReplayLeavesTheRunDirectoryByteForByteIdentical(t *testing.T) {
	dir := workdir(t)
	id := stagedRun(t, dir)
	run := filepath.Join(dir, "runs", id)

	before := snapshotTree(t, run)
	got := arxi(t, dir, "run", "replay", id)
	if got.code != 0 {
		t.Fatalf("replaying %s: exit %d\n%s", id, got.code, got.out)
	}
	after := snapshotTree(t, run)

	for name, was := range before {
		switch now, still := after[name]; {
		case !still:
			t.Errorf("replay removed %s from the run directory\n"+
				"  consequence: the log is the run's only history (ADR-0002). A "+
				"read-only verb that deletes part of it destroys the thing every "+
				"other command reads.", name)
		case now != was:
			t.Errorf("replay rewrote %s (%d bytes -> %d bytes)\n"+
				"  consequence: `run replay` promises it executes nothing and writes "+
				"nothing, which is what makes it safe to run against a LIVE run "+
				"while another process holds the writer lock. Writing anything here "+
				"means two writers, and the loser's events are gone.",
				name, len(was), len(now))
		}
	}
	for name := range after {
		if _, had := before[name]; !had {
			t.Errorf("replay created %s in the run directory\n"+
				"  consequence: if this is writer.lock, every later command on this "+
				"run refuses with \"already open for writing\" and the operator is "+
				"advised to delete a lock file by hand -- advice that eventually "+
				"gets applied to a live one. Replay takes no lock by design.", name)
		}
	}
}

// snapshotTree reads every file under root, keyed by path relative to it.
//
// Contents and not mtimes: a test comparing timestamps would pass on a filesystem
// with coarse resolution while the bytes underneath had changed, which is the
// wrong direction for a guard whose job is to notice a write.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("reading the run directory %s: %v", root, err)
	}
	if len(out) == 0 {
		t.Fatalf("the run directory %s is empty, so comparing it before and after "+
			"proves nothing", root)
	}
	return out
}

// TestReplayCountsTheEffectsItDidNotRun turns the claim into a number.
//
// The fold returns the effects each event asked for, and replay throws them away.
// Printing the count is what makes "no executor" checkable from the outside: an
// operator can see that four turns and two timers were wanted, and that none of
// them happened. It is also a figure available nowhere else in the tool -- no
// other verb reports what the reducer asked for as distinct from what was done.
//
// The count must be nonzero on a real log, and that is the assertion: a tally that
// silently reported 0 would look like a perfectly clean replay while proving that
// the effects are not being observed at all.
func TestReplayCountsTheEffectsItDidNotRun(t *testing.T) {
	dir := workdir(t)
	id := stagedRun(t, dir)

	got := arxi(t, dir, "run", "replay", id)
	if got.code != 0 {
		t.Fatalf("replaying %s: exit %d\n%s", id, got.code, got.out)
	}

	skipped := lineWith(t, got.out, "effects skipped:")
	if strings.Contains(skipped, "none") {
		t.Errorf("the effect tally reports none on a log the reducer answers: %q\n"+
			"  consequence: zero here is indistinguishable from not looking. The "+
			"tally is the evidence that the fold really did return effects and this "+
			"command really did discard them; reporting none turns the verb's central "+
			"claim into an unverifiable assertion.", skipped)
	}
	if !strings.Contains(skipped, "agent turn") {
		t.Errorf("the tally does not name the agent turns it skipped: %q\n"+
			"  consequence: a bare total says how many effects were dropped and not "+
			"which. Turns are the ones that would have cost money, and separating "+
			"them from timers and derived events is what tells the reader how much a "+
			"real run of this log would have spent.", skipped)
	}
	if !strings.Contains(skipped, "of them spend") {
		t.Errorf("the tally does not say how many of the effects would have cost money: %q\n"+
			"  consequence: \"11 effects skipped\" and \"11 skipped, 4 of them spend\" "+
			"answer different questions, and the second is the one an operator "+
			"deciding whether to re-run the log is asking.", skipped)
	}
}

// TestASeqPastTheHeadIsRefusedRatherThanClamped.
//
// Clamping would print the state at the head under a headline the user did not ask
// for, and they would read it as the state at the seq they typed. Refusing costs
// them one more command and tells them the truth about the log's length.
func TestASeqPastTheHeadIsRefusedRatherThanClamped(t *testing.T) {
	dir := workdir(t)
	id := stagedRun(t, dir)

	got := arxi(t, dir, "run", "replay", id, "--until-seq", "99")
	if got.code != 1 {
		t.Errorf("--until-seq 99 on an 8-event log exited %d, wanted 1\n%s\n"+
			"  consequence: exit 0 means the state at some other seq was printed and "+
			"accepted. A replay that quietly answers a different question than the "+
			"one asked is worse than one that refuses, because the answer looks "+
			"right.", got.code, got.out)
	}
	if !strings.Contains(got.out, "seq 8") {
		t.Errorf("the refusal does not name the head this log actually reaches:\n%s\n"+
			"  consequence: \"no such seq\" leaves the user guessing how far the log "+
			"goes. The head is the one number that makes the next attempt correct.",
			got.out)
	}
	if !strings.Contains(got.out, "event log") {
		t.Errorf("the refusal names no way to find a valid seq:\n%s\n"+
			"  consequence: the seqs are printed by `arxi event log`, and a user who "+
			"guessed wrong once has no reason to know that. Naming it turns a refusal "+
			"into a next step.", got.out)
	}
}

// TestUntilSeqZeroIsTheSameAsNotPassingIt.
//
// Seq numbering starts at 1, so 0 names no event and the two readings of it are
// "the whole log" or an error. It folds everything, because a caller building the
// flag from a variable that defaulted to 0 means "no limit", and refusing that
// would make the flag harder to script than to type.
//
// Byte-for-byte equality with the no-flag output, not a spot check: the two paths
// differ by one assignment inside the command, and anything that made them differ
// visibly would be the bug.
func TestUntilSeqZeroIsTheSameAsNotPassingIt(t *testing.T) {
	dir := workdir(t)
	id := stagedRun(t, dir)

	bare := arxi(t, dir, "run", "replay", id)
	zero := arxi(t, dir, "run", "replay", id, "--until-seq", "0")

	if zero.code != bare.code || zero.out != bare.out {
		t.Errorf("--until-seq 0 (exit %d) does not match no flag at all (exit %d)\n"+
			"  with the flag:\n%s\n  without it:\n%s\n"+
			"  consequence: 0 is not a seq any log has, so it can only mean \"no "+
			"limit\" or be an error. Anything in between -- an empty fold, a state "+
			"before the run started -- is a third answer to a question with two.",
			zero.code, bare.code, zero.out, bare.out)
	}
}

// TestAMalformedUntilSeqIsRefusedBeforeTheLogIsOpened.
//
// The shape of the flag is checked before the run is resolved, so a typo is
// answered without a file read. The observable consequence, and what this test
// uses to prove the ordering, is that a bad seq against a run that does not exist
// complains about the SEQ -- the thing the user got wrong -- rather than about the
// missing run.
func TestAMalformedUntilSeqIsRefusedBeforeTheLogIsOpened(t *testing.T) {
	dir := workdir(t)

	got := arxi(t, dir, "run", "replay", "no-such-run", "--until-seq", "forty-four")
	if got.code != 2 {
		t.Errorf("a non-numeric --until-seq exited %d, wanted 2 (wrong invocation)\n%s\n"+
			"  consequence: exit 1 says the command tried and could not; this never "+
			"tried, and a script distinguishing \"my arguments are wrong\" from \"the "+
			"run is broken\" reads the code.", got.code, got.out)
	}
	if !strings.Contains(got.out, "until-seq") {
		t.Errorf("the refusal does not name the flag that is wrong:\n%s\n"+
			"  consequence: two arguments were passed and one is malformed. Not "+
			"naming it sends the reader to check the run id, which is fine.", got.out)
	}
	if strings.Contains(got.out, "holds no event log") {
		t.Errorf("a malformed --until-seq was reported as a missing run:\n%s\n"+
			"  consequence: the run id is not the problem, and this message sends the "+
			"user to create or find a run in order to be told about the typo they "+
			"already made. The shape check is meant to run first precisely so the "+
			"cheaper, more likely error is the one reported.", got.out)
	}
}

// TestANegativeUntilSeqIsRefusedRatherThanClamped.
//
// Written as --until-seq=-1 and not with a space, because the space-separated form
// never reaches this check: the surface's short-flag expander sees a leading dash
// and answers "-1 is not a short flag in this surface" first. Both refuse with
// exit 2 and both say something true, so the shared parser is left alone -- but the
// guard here is only reachable through the joined spelling, and a test written the
// other way would pass without ever running the code it names.
func TestANegativeUntilSeqIsRefusedRatherThanClamped(t *testing.T) {
	dir := workdir(t)
	id := stagedRun(t, dir)

	got := arxi(t, dir, "run", "replay", id, "--until-seq=-1")
	if got.code != 2 {
		t.Errorf("--until-seq=-1 exited %d, wanted 2\n%s\n"+
			"  consequence: clamping a negative to 1, or to the head, prints a state "+
			"the user did not ask for under a number they did. Exit 0 here means one "+
			"of those happened.", got.code, got.out)
	}
	if !strings.Contains(got.out, "seq 1") {
		t.Errorf("the refusal does not say where a log actually starts:\n%s\n"+
			"  consequence: a user who typed a negative seq is working from the wrong "+
			"model of the numbering. \"below the first seq\" plus \"a run's log starts "+
			"at seq 1\" corrects the model; \"invalid value\" does not.", got.out)
	}
}

// TestAMissingFrozenBlueprintWarnsAndFoldsAnyway.
//
// blueprint.snapshot.yaml is what makes a replay reproducible (ADR-0001): the
// reducer reads the frozen copy, never the live file, so an edited blueprint cannot
// silently re-judge an old run. With the snapshot gone the fold uses an empty
// config, and the state it produces can differ from the one the run was really in
// -- fewer members, no stage advancing, no ceiling.
//
// It warns and continues rather than refusing, unlike `run fork`, and the
// difference is the point of the verb: forking a run with no config would build a
// new run on a guess, while replaying one is an act of reading. A log that is
// damaged is exactly what somebody reaches for this command to look at.
func TestAMissingFrozenBlueprintWarnsAndFoldsAnyway(t *testing.T) {
	dir := workdir(t)
	id := stagedRun(t, dir)
	if err := os.Remove(filepath.Join(dir, "runs", id, "blueprint.snapshot.yaml")); err != nil {
		t.Fatal(err)
	}

	out, errOut, code := arxiStreams(t, dir, "run", "replay", id)
	if code != 0 {
		t.Errorf("replaying a run with no frozen blueprint exited %d, wanted 0\n%s%s\n"+
			"  consequence: refusing here withholds the log from the person trying to "+
			"read it. A missing snapshot makes the fold less trustworthy, not "+
			"unreadable, and saying so is the whole job.", code, out, errOut)
	}
	if !strings.Contains(out, "state at seq") {
		t.Errorf("no state was printed for a run with no frozen blueprint:\n%s\n"+
			"  consequence: the events are still there and folding them is still "+
			"useful. Printing nothing turns a caveat into a refusal.", out)
	}
	if !strings.Contains(errOut, "no frozen blueprint") {
		t.Errorf("no warning was printed about the missing frozen blueprint:\n%s\n"+
			"  consequence: the state above is missing members and cannot advance a "+
			"stage, and nothing says why. The reader concludes the run was in a state "+
			"it was never in -- which is the one failure a debugging tool must not "+
			"produce.", errOut)
	}
	if !strings.Contains(errOut, filepath.Join("runs", id, "blueprint.snapshot.yaml")) {
		t.Errorf("the warning does not name the file that is missing:\n%s\n"+
			"  consequence: the reader's next question is whether it can be restored, "+
			"and that starts with knowing where it should be.", errOut)
	}
	if strings.Contains(out, "no frozen blueprint") {
		t.Errorf("the warning was written to stdout:\n%s\n"+
			"  consequence: `arxi run replay r1 > state.txt` is how this output gets "+
			"kept. The state belongs in the file and the caveat belongs on the "+
			"terminal, where the person running the command is.", out)
	}
}

// TestALogThatDoesNotBeginWithRunStartedNamesWhatItBeginsWith.
//
// A fold starting anywhere but run.started begins from a state no run has been in:
// no roster, no budget, no actor. It is still folded, because a log with its head
// cut off is a thing that happens and reading it is what this verb is for.
//
// The assertion is on the warning NAMING the first type it found. "does not begin
// with run.started" sends the reader to go and look; "begins with stage.entered,
// not run.started" has already told them what happened.
func TestALogThatDoesNotBeginWithRunStartedNamesWhatItBeginsWith(t *testing.T) {
	dir := workdir(t)
	id := stagedRun(t, dir)
	cut := copyRun(t, dir, id, "headless", dropSeq(stagedLog, 1))

	out, errOut, code := arxiStreams(t, dir, "run", "replay", cut)
	if code != 0 {
		t.Errorf("replaying a log with no run.started exited %d, wanted 0\n%s%s\n"+
			"  consequence: this is the log most in need of reading. Refusing it "+
			"leaves the operator with nothing but the raw NDJSON.", code, out, errOut)
	}
	if !strings.Contains(errOut, "stage.entered") || !strings.Contains(errOut, "run.started") {
		t.Errorf("the warning does not name both what the log begins with and what it should:\n%s\n"+
			"  consequence: the reader has to open the log to find out what is wrong "+
			"with it, which is what they were using this command to avoid. Both names "+
			"in one sentence is the difference between a flag and an explanation.",
			errOut)
	}
	if !strings.Contains(out, "state at seq") {
		t.Errorf("no state was printed for a log missing its first event:\n%s\n"+
			"  consequence: seven of the eight events are intact and folding them "+
			"still says something. Withholding the answer makes the warning the whole "+
			"output.", out)
	}
}

// TestAGapInTheLogIsReportedRatherThanPassedOffAsTheSeqAskedFor.
//
// This command reads the log's bytes without taking the writer lock, which is what
// lets it run against a live run -- and means nothing upstream has promised the
// seqs run 1..N with no holes. `logstore.Open` enforces that; this reader does not
// go through it.
//
// So the fold applies everything at or below the target and reports the highest seq
// it actually reached. On a healthy log that is the target, and this test is the
// only place the difference is visible: asking for a seq the log does not contain
// prints the state at the one below it, and says so.
func TestAGapInTheLogIsReportedRatherThanPassedOffAsTheSeqAskedFor(t *testing.T) {
	dir := workdir(t)
	id := stagedRun(t, dir)
	holed := copyRun(t, dir, id, "holed", dropSeq(stagedLog, 7))

	got := arxi(t, dir, "run", "replay", holed, "--until-seq", "7")
	if got.code != 0 {
		t.Fatalf("replaying a log with seq 7 missing: exit %d\n%s", got.code, got.out)
	}
	if !strings.Contains(got.out, "holds no seq 7") {
		t.Errorf("the missing seq is not reported:\n%s\n"+
			"  consequence: the state printed is the one at seq 6, under a command "+
			"that asked for 7. Silence here means the reader believes they are looking "+
			"at seq 7 -- and a replay whose whole value is that it says which state it "+
			"is showing has just shown a different one.", got.out)
	}
	if !strings.Contains(firstLine(got.out), "seq 6") {
		t.Errorf("the headline does not name the seq actually reached:\n  %s\n"+
			"  consequence: the note explains the gap and the headline is what gets "+
			"read. If it still says 7, the correction is in the small print under a "+
			"wrong heading.", firstLine(got.out))
	}
}

// TestTheCommandsReplaySuggestsNameTheRunTheUserAsked is the regression guard for
// the defect running this verb by hand found.
//
// A run directory copied aside for inspection keeps the ORIGINAL run id inside its
// log. The closing suggestion was built from that id, so replaying the copy printed
//
//	fold all of it: arxi run replay rmtizyc28-ba2261a1
//
// pointing at the pristine original -- a different directory, with none of the
// damage on screen. The suggestion has to be built from the argument the user
// typed, which is the only string guaranteed to resolve back to the directory just
// read, because resolveRunDir turned exactly that into it.
//
// Invisible on any run whose directory name matches its recorded id, which is every
// run `run start` makes. It takes a copy to see, and a copy is what an operator
// investigating a broken run makes first.
func TestTheCommandsReplaySuggestsNameTheRunTheUserAsked(t *testing.T) {
	dir := workdir(t)
	id := stagedRun(t, dir)
	const alias = "copied-aside"
	copyRun(t, dir, id, alias, stagedLog)

	// Short of the head, so the "fold all of it" suggestion is printed.
	got := arxi(t, dir, "run", "replay", alias, "--until-seq", "7")
	if got.code != 0 {
		t.Fatalf("replaying %s to seq 7: exit %d\n%s", alias, got.code, got.out)
	}

	suggestion := lineWith(t, got.out, "fold all of it:")
	if !strings.Contains(suggestion, alias) {
		t.Errorf("the suggested command does not name the run the user asked about: %q\n"+
			"  asked about: %s   recorded in the log: %s\n"+
			"  consequence: the command as printed resolves to a DIFFERENT run "+
			"directory. Running it answers a question about another log and reports "+
			"success, and the reader has no way to notice: both outputs are "+
			"well-formed replays.", suggestion, alias, id)
	}
	if strings.Contains(suggestion, "replay "+id) {
		t.Errorf("the suggested command names the id recorded in the log instead of the "+
			"argument given: %q\n"+
			"  consequence: %q is the pristine original this copy was made from. The "+
			"suggestion sends an operator investigating damage to the undamaged copy.",
			suggestion, id)
	}
}

// TestTheJSONAndTheHumanViewAgree.
//
// Two renderings of one replayView, which is the rule the rest of this binary holds
// itself to: `surface` and `schema` render one manifest, and if they diverge it is a
// bug rather than a product decision. The way two views drift is one of them
// growing a value the other does not have, so the numbers are compared across the
// two rather than each being checked against a constant.
func TestTheJSONAndTheHumanViewAgree(t *testing.T) {
	dir := workdir(t)
	id := stagedRun(t, dir)

	human := arxi(t, dir, "run", "replay", id, "--until-seq", "7")
	if human.code != 0 {
		t.Fatalf("replaying %s: exit %d\n%s", id, human.code, human.out)
	}
	raw := arxi(t, dir, "run", "replay", id, "--until-seq", "7", "--json")
	if raw.code != 0 {
		t.Fatalf("replaying %s as JSON: exit %d\n%s", id, raw.code, raw.out)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(raw.out), &got); err != nil {
		t.Fatalf("the --json output does not parse: %v\n%s\n"+
			"  consequence: --json exists so another program can read this. Output "+
			"that only a human can read is the flag failing silently.", err, raw.out)
	}

	for field, want := range map[string]float64{
		"until_seq":        7,
		"at_seq":           7,
		"head_seq":         8,
		"events_applied":   7,
		"replay_spent_usd": 0,
		"spent_usd":        0.25,
	} {
		if n, ok := got[field].(float64); !ok || n != want {
			t.Errorf("--json field %q is %v, wanted %v\n"+
				"  consequence: a caller gating on this reads the wrong number. "+
				"replay_spent_usd in particular is the machine-readable form of the "+
				"claim that nothing was executed.", field, got[field], want)
		}
	}
	if got["at_head"] != false {
		t.Errorf("--json says at_head=%v for a fold that stopped at seq 7 of 8\n"+
			"  consequence: this is the one boolean telling a caller whether the state "+
			"it just read is the run's current state or a historical one. Wrong, it "+
			"makes an old state look live.", got["at_head"])
	}

	// The tally's total has to be the same number the human line reports, because
	// the two are computed from one effectTally and printed by different code.
	disc, ok := got["effects_discarded"].(map[string]any)
	if !ok {
		t.Fatalf("--json has no effects_discarded object: %v\n"+
			"  consequence: the effect count is the evidence for \"no executor\", and "+
			"the human view prints it. A machine reader cannot check the claim.",
			got["effects_discarded"])
	}
	total, _ := disc["total"].(float64)
	tallyLine := lineWith(t, human.out, "effects skipped:")
	if !strings.Contains(tallyLine, strconv.Itoa(int(total))) {
		t.Errorf("--json reports %v effects discarded, the human view says %q\n"+
			"  consequence: two readings of one tally disagree, so at least one is "+
			"wrong and nothing on screen says which. They are rendered by separate "+
			"code from the same struct precisely so this can be checked.", total, tallyLine)
	}
}

// TestReplayingTwiceGivesTheSameAnswer pins what the registry declares.
//
// `run replay` carries Idempotent: true in the surface, which is a promise made to
// an agent: it may re-run this verb without asking whether doing so is safe. The
// promise is not free. It holds only because the fold reads bytes and derives
// everything else, and it would break the moment any part of the answer came from
// the clock, from a counter, or from the state snapshot on disk -- all three of
// which are sitting right there next to the log and are the obvious shortcut for a
// future change.
//
// Byte-for-byte, on purpose. A weaker assertion (same status, same seq) would pass
// while a timestamp or a duration crept onto the output, and a duration is exactly
// the kind of thing somebody adds to a fold to show it was fast.
func TestReplayingTwiceGivesTheSameAnswer(t *testing.T) {
	dir := workdir(t)
	id := stagedRun(t, dir)

	first := arxi(t, dir, "run", "replay", id)
	second := arxi(t, dir, "run", "replay", id)

	if first.code != 0 || second.code != 0 {
		t.Fatalf("replaying %s twice: exit %d then %d\n%s", id, first.code, second.code, first.out)
	}
	if first.out != second.out {
		t.Errorf("two replays of one unchanged log printed different output\n"+
			"  first:\n%s\n  second:\n%s\n"+
			"  consequence: the surface declares this verb Idempotent, which is what "+
			"lets an agent re-run it without asking. Something in the answer is coming "+
			"from outside the log -- the clock, a counter, or the state snapshot -- and "+
			"an agent comparing two replays would conclude the run had changed when "+
			"nothing did.", first.out, second.out)
	}
}
