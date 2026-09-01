package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for `arxi run prompt`, run as the binary.
//
// They run the real binary because every defect this file pins was found by
// typing the command and looking at what happened afterwards -- and in three of
// the four cases the command had already printed success by then. That is the
// shape worth stating plainly, because it is the same one the last two steps
// hit:
//
//	A command that reports what it INTENDED rather than what it DID cannot be
//	caught by reading its output. It has to be caught by reading the state.
//
// The four defects, all measured on real --sim runs:
//
//   - the prompt was appended and then silently discarded, because nothing
//     drove the run and an un-parked cause lives only in Decide's return value
//   - the closing line advised `run unpause`, which REFUSES a running run --
//     so the command written to close a printed-and-refused gap printed one
//   - a budget-blocked run was told "it opens a turn now" and parked instead
//   - a refused CAS exited while holding writer.lock, bricking the run for
//     every later command
//
// The first is the one that matters most, and it is the reason the guards below
// look at the log and the folded state rather than at the banner.

// promptableRun starts a simulated run that stops on its budget, and returns
// its id. The run is genuinely produced by the reducer rather than hand-written,
// so a change in how a stop is recorded is caught here instead of being encoded
// twice and agreeing with itself.
//
// It stops BLOCKED, which is the state most of these guards want: the refusals
// and the CAS do not care how the run stopped, and the parking guard needs a
// halted one specifically. The tests that need a live, quiescent run use
// quiescentRun instead.
func promptableRun(t *testing.T, dir string) string {
	t.Helper()

	bp := filepath.Join(dir, "team.yaml")
	if err := os.WriteFile(bp, []byte(exampleBlueprint), 0o644); err != nil {
		t.Fatal(err)
	}

	got := arxi(t, dir, "run", "start", bp, "ship the thing", "--sim", "--budget", "0.005")
	if got.code != 0 {
		t.Fatalf("starting a run to prompt: exit %d\n%s", got.code, got.out)
	}

	ents, err := os.ReadDir(filepath.Join(dir, "runs"))
	if err != nil || len(ents) != 1 {
		t.Fatalf("expected exactly one run dir under %s: %v (%d entries)",
			dir, err, len(ents))
	}
	return ents[0].Name()
}

// quiescentRun writes a run that is RUNNING, has budget left, and cannot move:
// one member submitted, the stage needs all of them, and nobody is working. It
// returns the run id.
//
// This one is hand-written, and the reason is that the reducer cannot be asked
// for it. Quiescence with headroom is precisely the state the running loop
// drives its way out of, so any run produced by `run start` has either finished
// or stopped on a ceiling by the time the command returns. The state exists in
// real logs -- it is the one `kernel.Explain` has a dedicated branch for, and
// the only one whose remedy is `run prompt` -- but it is reached by a log that
// stopped mid-flight, not by a run that ran to a standstill in one process.
//
// simulated:true is on run.started deliberately. Without it the drive builds a
// LIVE executor, which on a machine with no configured model spawns the turn
// and produces nothing -- so the test would report "the prompt opened no turn"
// for a reason that has nothing to do with the prompt.
func quiescentRun(t *testing.T, dir string) string {
	t.Helper()

	const id = "q1"
	run := filepath.Join(dir, "runs", id)
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatal(err)
	}

	bp := "name: feature-team\n" +
		"members:\n" +
		"  - {name: backend,  role: implementer, tools: [read, write]}\n" +
		"  - {name: frontend, role: implementer, tools: [read]}\n" +
		"stages:\n" +
		"  - {name: build, advance_when: all}\n"
	if err := os.WriteFile(filepath.Join(run, "blueprint.snapshot.yaml"),
		[]byte(bp), 0o644); err != nil {
		t.Fatal(err)
	}

	log := `{"id":"e1","seq":1,"ts":"2026-01-01T00:00:00Z","type":"run.started","source":"human","payload":{"actor":"feature-team","run_id":"q1","budget_usd":10,"simulated":true}}
{"id":"e2","seq":2,"ts":"2026-01-01T00:00:01Z","type":"stage.entered","source":"system","payload":{"stage":"build","index":0}}
{"id":"e3","seq":3,"ts":"2026-01-01T00:00:02Z","type":"agent.activated","source":"system","payload":{"agent":"backend"}}
{"id":"e4","seq":4,"ts":"2026-01-01T00:00:03Z","type":"agent.turn_done","source":"system","payload":{"agent":"backend"}}
{"id":"e5","seq":5,"ts":"2026-01-01T00:00:04Z","type":"stage.submitted","source":"agent","payload":{"agent":"backend","stage":"build"}}
`
	if err := os.WriteFile(filepath.Join(run, "events.ndjson"),
		[]byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestAPromptIsActedOnAndNotJustRecorded is the defect this whole step exists
// for, and the only test here whose failure means the command does nothing.
//
// The first draft appended the event, printed "prompted (seq 16)", and returned.
// The cause reached the log and then evaporated: applyInjection hands an idle
// member's cause to spawnCauses, which parks it only when spendingHalted, and
// otherwise returns a transient SpawnTurn that nobody executes. So the run was
// bit-for-bit as quiescent as before, and `run why` kept recommending the very
// prompt that had just been sent.
//
// Asserting on the output cannot catch that -- the output was already correct.
// This asserts on the log.
func TestAPromptIsActedOnAndNotJustRecorded(t *testing.T) {
	dir := workdir(t)
	// Quiescent and not merely stopped: a halted run PARKS the cause, which is
	// correct behaviour and would hide the defect. The bug lives exactly where
	// the run is free to work and nobody makes it.
	run := quiescentRun(t, dir)

	before := countEvents(t, dir, run, "agent.turn_done")

	got := arxi(t, dir, "run", "prompt", run, "please submit what you have")
	if got.code != 0 {
		t.Fatalf("prompting exited %d:\n%s", got.code, got.out)
	}

	// The event reached the log. This is the half that always worked.
	ev := eventOfType(t, dir, run, "run.prompt")
	if ev["source"] != "human" {
		t.Errorf("run.prompt is attributed to %v, want human -- an injected "+
			"cause has no antecedent in the log, so an audit that cannot say a "+
			"human put it there cannot explain the change of direction",
			ev["source"])
	}
	if ts, _ := ev["ts"].(string); ts == "" {
		t.Errorf("run.prompt has no timestamp: nothing else will stamp it, "+
			"because a human typed this command and it does not pass through "+
			"the effect runner:\n%v", ev)
	}

	// And the half that did not: the run MOVED. Without this the command is a
	// no-op that reports success.
	if after := countEvents(t, dir, run, "agent.turn_done"); after <= before {
		t.Errorf("the prompt opened no turn: %d turns before, %d after.\n\n"+
			"The cause reached the log and was discarded -- spawnCauses returns "+
			"a transient effect for an idle member on a running run, so a "+
			"prompt that is not driven changes nothing at all.", before, after)
	}
}

// TestThePromptTellsTheTruthAboutWhatItDid.
//
// The banner is the only thing most users read, so it must not describe an
// intention. The first draft ended with "the cause is in the log. drive it:
// arxi run unpause <run>" -- advice for work the command had not done, naming a
// command that refuses.
func TestThePromptTellsTheTruthAboutWhatItDid(t *testing.T) {
	dir := workdir(t)
	run := quiescentRun(t, dir)

	got := arxi(t, dir, "run", "prompt", run, "carry on")
	if got.code != 0 {
		t.Fatalf("exit %d:\n%s", got.code, got.out)
	}
	// The summary of a driven run, which is what proves it drove.
	if !strings.Contains(got.out, "turns:") {
		t.Errorf("the prompt printed no run summary, so it did not drive:\n%s",
			got.out)
	}
	// It does not tell the user to go and do it themselves.
	if strings.Contains(got.out, "drive it:") {
		t.Errorf("the prompt still defers the work to the user:\n%s\n\n"+
			"Nothing else in the binary drives a run: `run unpause` refuses a "+
			"running one, and quiescence is a running state.", got.out)
	}
	// A simulated run says so, in the terms that matter.
	if !strings.Contains(got.out, "no money is spent") {
		t.Errorf("a simulated prompt does not say the turn was free:\n%s", got.out)
	}
}

// TestTheAdviceRunWhyGivesForAQuiescentRunIsImplemented closes the loop on the
// whole step, the same way the unpause step closed its own.
//
// `run why` prints `arxi run prompt <run> "..."` as the FIRST remedy for a
// quiescent run, and quiescence is the one state with no other way out: the run
// is not paused so unpause does not apply, and there is no question so the inbox
// does not apply. The remedy is lifted out of why's own output and run, rather
// than being retyped here, so the two cannot drift.
func TestTheAdviceRunWhyGivesForAQuiescentRunIsImplemented(t *testing.T) {
	dir := workdir(t)
	run := quiescentRun(t, dir)

	why := arxi(t, dir, "run", "why", run)
	if why.code != 0 {
		t.Fatalf("run why: exit %d\n%s", why.code, why.out)
	}
	// The diagnosis and its remedy, both. If why stops calling this state
	// quiescent the fixture has drifted, and that is worth failing on rather
	// than skipping past.
	if !strings.Contains(why.out, "quiescent") {
		t.Fatalf("the fixture is not quiescent any more, so this test no longer "+
			"guards what it claims:\n%s", why.out)
	}
	if !strings.Contains(why.out, "run prompt "+run) {
		t.Fatalf("run why no longer recommends a prompt for a quiescent run:\n%s",
			why.out)
	}

	before := countEvents(t, dir, run, "agent.turn_done")

	got := arxi(t, dir, "run", "prompt", run, "please submit what you have")
	if got.code != 0 {
		t.Fatalf("the recommended remedy was refused: exit %d\n%s\n\n"+
			"This is the gap the step closed: a command the tool prints and the "+
			"binary will not run.", got.code, got.out)
	}

	// And it did something. A remedy that is accepted and changes nothing is
	// the same gap wearing a different exit code -- which is exactly what the
	// first draft shipped, and why the loop is only closed by the state.
	if after := countEvents(t, dir, run, "agent.turn_done"); after <= before {
		t.Errorf("the remedy was accepted and the run did not move: %d turns "+
			"before, %d after", before, after)
	}
}

// TestAHaltedRunIsToldItsCauseIsParkedAndNotStarted.
//
// promptOutlook read the MEMBER's state and printed "it opens a turn for
// backend now". On a budget-blocked run that is false: spawnCauses asks
// spendingHalted BEFORE it looks at the member, so an idle member on a halted
// run gets its cause parked. The line described a decision the reducer never
// makes.
func TestAHaltedRunIsToldItsCauseIsParkedAndNotStarted(t *testing.T) {
	dir := workdir(t)
	run := promptableRun(t, dir) // stops blocked, on the budget

	got := arxi(t, dir, "run", "prompt", run, "hurry up")
	if got.code != 0 {
		t.Fatalf("exit %d:\n%s", got.code, got.out)
	}
	if !strings.Contains(got.out, "parked") {
		t.Errorf("a blocked run's prompt does not say the cause is parked:\n%s\n\n"+
			"spendingHalted is the reducer's first question, so no turn opens "+
			"however idle the member looks.", got.out)
	}
	if strings.Contains(got.out, "opens a turn") {
		t.Errorf("a blocked run's prompt claims a turn was opened:\n%s", got.out)
	}

	// And the state agrees: the cause is held, not dropped. Raising the ceiling
	// must give it back, which is the whole reason spawnCauses parks rather
	// than discarding.
	show := arxi(t, dir, "run", "show", run)
	if !strings.Contains(show.out, "held back") {
		t.Errorf("the parked cause is not visible in the run's state:\n%s", show.out)
	}
}

// TestARefusedCASLeavesNoLockBehind is the fourth defect, and the nastiest,
// because a CAS miss is the most ORDINARY failure this command has.
//
// The refusal exits 1 directly rather than through fatal(), so it ran neither
// the deferred Close nor the atExit hooks. One mis-guarded prompt left
// writer.lock holding a dead pid, and every later command on that run was
// refused with advice to delete a lock file by hand.
func TestARefusedCASLeavesNoLockBehind(t *testing.T) {
	dir := workdir(t)
	run := promptableRun(t, dir)

	got := arxi(t, dir, "run", "prompt", run, "hi", "--if-seq", "3")
	if got.code != 1 {
		t.Fatalf("a stale --if-seq exited %d, want 1:\n%s", got.code, got.out)
	}
	// The refusal is informative: what was expected, what is true, and that
	// nothing was written.
	for _, want := range []string{"the run moved", "nothing was written"} {
		if !strings.Contains(got.out, want) {
			t.Errorf("the CAS refusal does not say %q:\n%s", want, got.out)
		}
	}

	if _, err := os.Stat(filepath.Join(dir, "runs", run, "writer.lock")); err == nil {
		t.Errorf("writer.lock survived a refused CAS")
	}

	// The proof that matters: the next command still works. A leftover lock
	// does not merely linger, it refuses everything afterwards.
	next := arxi(t, dir, "run", "show", run)
	if strings.Contains(next.out, "open for writing") {
		t.Fatalf("a refused CAS bricked the run for the next command:\n%s", next.out)
	}
}

// TestAGuardedPromptAppendsWhenTheRunHasNotMoved is the other half of the CAS.
//
// A guard that always refused would pass the test above and be useless.
func TestAGuardedPromptAppendsWhenTheRunHasNotMoved(t *testing.T) {
	dir := workdir(t)
	run := promptableRun(t, dir)

	// The seq the caller last saw, taken from the run itself rather than
	// assumed: hard-coding it would make this test a statement about how many
	// events a blocked run happens to have.
	head := len(allEvents(t, dir, run))

	got := arxi(t, dir, "run", "prompt", run, "go on", "--if-seq", itoa(head))
	if got.code != 0 {
		t.Fatalf("a correct --if-seq was refused: exit %d\n%s\n\n"+
			"The run was at seq %d and nothing had touched it since.",
			got.code, got.out, head)
	}
	if countEvents(t, dir, run, "run.prompt") != 1 {
		t.Errorf("the guarded prompt did not reach the log")
	}
}

// TestAnEmptyPromptIsRefusedBeforeItIsWritten.
//
// The reducer would take it: applyInjection keys off the event ID, not the
// text. So an empty prompt buys a turn whose new context is a blank string --
// money spent to tell an agent nothing -- and leaves a log recording a human
// intervening with no content.
func TestAnEmptyPromptIsRefusedBeforeItIsWritten(t *testing.T) {
	dir := workdir(t)
	run := promptableRun(t, dir)

	got := arxi(t, dir, "run", "prompt", run, "   ")
	if got.code != 2 {
		t.Fatalf("an empty message exited %d, want 2 (misuse):\n%s",
			got.code, got.out)
	}
	// Nothing was written, which is the point of refusing BEFORE the append: an
	// append-only log offers no way to unsay it.
	for _, ev := range allEvents(t, dir, run) {
		if ev["type"] == "run.prompt" {
			t.Fatalf("the refusal appended anyway: %v", ev)
		}
	}
}

// TestAPromptToANonMemberIsRefusedRatherThanDroppedSilently.
//
// The reducer cannot catch this: applyInjection loops over members and skips
// the ones that do not match, so a --to naming nobody appends an event, changes
// nothing, and looks like success. That is the worst available outcome, because
// the user believes they steered the run.
func TestAPromptToANonMemberIsRefusedRatherThanDroppedSilently(t *testing.T) {
	dir := workdir(t)
	run := promptableRun(t, dir)

	got := arxi(t, dir, "run", "prompt", run, "hi", "--to", "nobody")
	if got.code != 1 {
		t.Fatalf("an unknown recipient exited %d, want 1:\n%s", got.code, got.out)
	}
	// The members it DOES have, so the user can fix it without a second command.
	if !strings.Contains(got.out, "backend") {
		t.Errorf("the refusal does not list the members that exist:\n%s", got.out)
	}
	for _, ev := range allEvents(t, dir, run) {
		if ev["type"] == "run.prompt" {
			t.Fatalf("the refusal appended anyway: %v", ev)
		}
	}
}

// TestAnUnimplementedOnBusyIsRefusedRatherThanIgnored.
//
// --on-busy is declared in the surface and nothing reads it: applyInjection
// implements exactly one behaviour, which is `queue`. Accepting `reject`
// silently is the specific failure serve.go's validateParams exists to prevent,
// and it is worse here than a misspelling: the caller asked NOT to disturb a
// busy member and would be told it worked.
func TestAnUnimplementedOnBusyIsRefusedRatherThanIgnored(t *testing.T) {
	dir := workdir(t)
	run := promptableRun(t, dir)

	for _, mode := range []string{"reject", "steer"} {
		got := arxi(t, dir, "run", "prompt", run, "hi", "--on-busy", mode)
		if got.code != 2 {
			t.Errorf("--on-busy %s exited %d, want 2:\n%s", mode, got.code, got.out)
			continue
		}
		if !strings.Contains(got.out, "not implemented") {
			t.Errorf("--on-busy %s is refused without saying why:\n%s", mode, got.out)
		}
	}

	// The one that IS implemented is accepted, so the refusal above is about
	// the behaviour and not about the flag existing.
	if got := arxi(t, dir, "run", "prompt", run, "hi", "--on-busy", "queue"); got.code != 0 {
		t.Errorf("--on-busy queue was refused, but queueing is what the reducer "+
			"does: exit %d\n%s", got.code, got.out)
	}
}

// TestAFinishedRunIsNotPrompted.
//
// The reducer would accept it -- applyInjection does not consult Status -- so
// this is a real guard and not a formality. Appending would resurrect a run
// whose result is already recorded, and `event trace` would show work happening
// after it.
func TestAFinishedRunIsNotPrompted(t *testing.T) {
	dir := workdir(t)
	run := promptableRun(t, dir)

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

	got := arxi(t, dir, "run", "prompt", run, "one more thing")
	if got.code != 1 {
		t.Fatalf("prompting a cancelled run exited %d, want 1:\n%s",
			got.code, got.out)
	}
	if !strings.Contains(got.out, "final") {
		t.Errorf("the refusal does not say the run is final:\n%s", got.out)
	}
	// The way forward, because somebody prompting a finished run wants to
	// continue the work, not to be told no.
	if !strings.Contains(got.out, "run fork") {
		t.Errorf("the refusal does not offer a way to continue from here:\n%s",
			got.out)
	}
}

// itoa avoids pulling strconv in for one call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
