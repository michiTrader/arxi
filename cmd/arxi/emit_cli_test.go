package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `arxi event emit`, exercised as a process against real run directories.
//
// This verb is the only one in the binary that lets something outside a run put
// a cause INTO it, so the defects worth guarding are not about what it computes.
// They are about the three ways it can lie to a person:
//
//   - by recording an event nobody watches while sounding like it started work
//     (the reverse of `run prompt`, which always starts work);
//   - by accepting `custom.*` -- the literal string `event log --type` taught
//     the user to type -- and minting an event whose name is a glob;
//   - by parking a cause in a halted run and exiting 0, which looks identical to
//     the command having worked.
//
// Every fixture below is a hand-written log, for the reason runresult's are: the
// statuses that matter here (paused, blocked, terminal, a member waiting) are
// not producible on demand by `run start --sim`, and a fixture built with the
// code under test cannot tell "the run was never there" from "the command
// failed to read it".

// emitRunAt writes a run whose blueprint declares watchers.
//
// runAt (runlist_cli_test.go) is reused everywhere else in this package, and is
// not reused here: its blueprint declares one member and NO watchers, and the
// whole subject of this file is which watcher a type matches. The roster below
// has two members because a watcher must name a declared member -- blueprint
// validation refuses `agent: qa` outright, which is how the "watcher names a
// member that does not exist" branch turned out to be unreachable.
func emitRunAt(t *testing.T, dir, id, watchers, extra string, simulated bool) {
	t.Helper()

	run := filepath.Join(dir, "runs", id)
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatal(err)
	}
	bp := "name: feature-team\n" +
		"members:\n" +
		"  - name: backend\n" +
		"    tools: [read, write, bash]\n" +
		"  - name: security\n" +
		"    tools: [read]\n" +
		"stages:\n" +
		"  - name: execute\n" +
		"    advance_when: all\n"
	if watchers != "" {
		bp += "watchers:\n" + watchers
	}
	if err := os.WriteFile(filepath.Join(run, "blueprint.snapshot.yaml"),
		[]byte(bp), 0o644); err != nil {
		t.Fatal(err)
	}

	sim := "false"
	if simulated {
		sim = "true"
	}
	log := `{"id":"e1","seq":1,"type":"run.started","payload":{"actor":"feature-team","run_id":"` +
		id + `","budget_usd":1,"simulated":` + sim + `}}
{"id":"e2","seq":2,"type":"stage.entered","payload":{"stage":"execute","index":0}}
` + extra
	if err := os.WriteFile(filepath.Join(run, "events.ndjson"), []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
}

// watchCustom is the watcher this file spends most of its time against.
const watchCustom = "  - {agent: security, pattern: custom.*, action: notify}\n"

// watchSomethingElse is a run that watches, and does not watch custom.*.
//
// It is not the same fixture as "declares no watchers", and the difference is
// the point of one of the tests below: a person who emits into this run needs to
// be told what IS watched, because the likeliest cause of their surprise is a
// pattern one character off from the type they typed.
const watchSomethingElse = "  - {agent: security, pattern: run.quiescent, action: notify}\n"

// lastEvent and eventOfType are NOT defined here.
//
// unpause_cli_test.go already has both, and its lastEvent makes the same
// judgement this file needs: read the FILE rather than fold it, because these
// assertions are about what the binary wrote, and a fold would pass even if a
// payload had been written under a key the reducer happens to tolerate. A second
// copy read the same bytes and would drift.
//
// eventOfType matters for the driving test below for the reason its own comment
// gives: once an emit drives the run, the event a test wants to inspect is no
// longer at the tip, because executing it appended everything it caused.

// TestEventEmitRefusesEveryTypeOutsideCustom is the invariant test, not a
// validation test.
//
// stage.advanced is the example on purpose: the reducer reads its to_index to
// decide which stage comes next, and a run whose blueprint says advance_when: all
// would be walked past its own quorum by one hand-written line. The refusal is
// what keeps `custom.*` from being a suggestion.
func TestEventEmitRefusesEveryTypeOutsideCustom(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchCustom, "", true)

	for _, typ := range []string{"stage.advanced", "agent.blocked", "run.started", "note"} {
		got := arxi(t, dir, "event", "emit", "r1", typ)
		if got.code != 2 {
			t.Errorf("emitting %s exited %d, want 2:\n%s\n"+
				"  consequence: a caller who can write stage.advanced can walk a "+
				"run past the quorum its blueprint declares, and the log will "+
				"look like the stage advanced legitimately.", typ, got.code, got.out)
		}
		if !strings.Contains(got.out, "custom.") {
			t.Errorf("refusing %s never names the namespace that IS allowed:\n%s\n"+
				"  consequence: the user is told no and not told what to type "+
				"instead, so the next attempt is a guess.", typ, got.out)
		}
	}

	// Nothing was written. A refusal that still appends is worse than an
	// acceptance, because the log then holds the event the message said it
	// would not hold.
	raw, err := os.ReadFile(filepath.Join(dir, "runs", "r1", "events.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "stage.advanced") {
		t.Errorf("a refused emit still reached the log:\n%s\n"+
			"  consequence: the reducer folds it anyway, so the refusal was "+
			"cosmetic.", raw)
	}
}

// TestEventEmitRefusesTheGlobEventLogTaughtYouToType covers the one invalid type
// that passes the namespace check.
//
// `arxi event log <run> --type 'custom.*'` is the command this file's own
// documentation recommends for seeing what an emit wrote, so `custom.*` is the
// string a user has most recently had in their hands. It starts with `custom.`
// and would mint an event whose literal type contains a `*` -- selectable by no
// pattern anyone would write.
func TestEventEmitRefusesTheGlobEventLogTaughtYouToType(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchCustom, "", true)

	got := arxi(t, dir, "event", "emit", "r1", "custom.*")
	if got.code != 2 {
		t.Fatalf("emitting custom.* exited %d, want 2:\n%s\n"+
			"  consequence: the log gains an event named `custom.*`, and the "+
			"user's next `--type 'custom.*'` matches it by accident while "+
			"matching nothing they meant.", got.code, got.out)
	}
	if !strings.Contains(got.out, "pattern") {
		t.Errorf("the refusal does not say it was read as a pattern:\n%s\n"+
			"  consequence: it reads as \"custom.* is not in the custom.* "+
			"namespace\", which is nonsense to the person holding it.", got.out)
	}
}

// TestEventEmitRefusesAPayloadThatIsNotAnObject.
//
// `null` is in the table because it is the case that fails silently: it
// unmarshals into a nil map with NO error, so a check written as "does it
// decode" accepts it and the event is recorded with no payload while the user
// believes they sent one.
func TestEventEmitRefusesAPayloadThatIsNotAnObject(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchSomethingElse, "", true)

	for _, p := range []string{"42", `"done"`, "null", "[1,2]", `{"v":2`} {
		got := arxi(t, dir, "event", "emit", "r1", "custom.note", "--payload", p)
		if got.code != 2 {
			t.Errorf("--payload %s exited %d, want 2:\n%s\n"+
				"  consequence: Event.Payload is a map, so this either records an "+
				"event with an empty payload the user thinks they filled, or "+
				"fails deeper down where the message no longer names --payload.",
				p, got.code, got.out)
		}
	}

	// And the object form is accepted, which is the half that proves the check
	// discriminates rather than refuses.
	got := arxi(t, dir, "event", "emit", "r1", "custom.note", "--payload", `{"v":2}`)
	if got.code != 0 {
		t.Fatalf("a JSON object payload was refused with %d:\n%s", got.code, got.out)
	}
	ev := lastEvent(t, dir, "r1")
	pay, _ := ev["payload"].(map[string]any)
	if pay["v"] != float64(2) {
		t.Errorf("the payload in the log is %v, want v=2:\n%v\n"+
			"  consequence: a watcher receives this map verbatim as its tool "+
			"arguments, so a dropped field is a tool called with the wrong input.",
			pay, ev)
	}
}

// TestEventEmitSaysNobodyIsWatchingAndNamesWhatIs is the test this verb most
// needs, because both outcomes exit 0.
//
// An emit that matches a watcher and an emit that matches nothing are both
// successes: one is a cause, the other is a record. The ONLY thing distinguishing
// "nothing is watching custom.deploy" from "custom.deploy woke qa" is this
// paragraph, so a silent success here is indistinguishable from work starting.
func TestEventEmitSaysNobodyIsWatchingAndNamesWhatIs(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchSomethingElse, "", true)

	got := arxi(t, dir, "event", "emit", "r1", "custom.deploy")
	if got.code != 0 {
		t.Fatalf("exit %d: %s\n"+
			"  an emit nothing watches is a legitimate record, not an error.",
			got.code, got.out)
	}
	if !strings.Contains(got.out, "observed by nobody") {
		t.Errorf("the output does not say nothing observed it:\n%s\n"+
			"  consequence: the user reads a bare success line and waits for work "+
			"that no watcher was ever going to start.", got.out)
	}
	if !strings.Contains(got.out, "run.quiescent") {
		t.Errorf("the output does not name the patterns this run DOES watch:\n%s\n"+
			"  consequence: the likeliest cause of the surprise is a pattern one "+
			"character off from the type typed, and that is exactly what printing "+
			"the declared patterns lets the user see.", got.out)
	}

	// Recorded, not discarded: the run's log holds it and `event log` finds it.
	log := arxi(t, dir, "event", "log", "r1", "--type", "custom.deploy")
	if log.code != 0 || !strings.Contains(log.out, "custom.deploy") {
		t.Errorf("the emitted event is not in the log (exit %d):\n%s\n"+
			"  consequence: the command claimed to have recorded it.",
			log.code, log.out)
	}
}

// TestEventEmitSaysWhenARunDeclaresNoWatchersAtAll separates the two silences.
//
// "your pattern did not match" and "this run reacts to nothing at all" call for
// different fixes -- correct the type, or edit the blueprint -- and a single
// message covering both sends half the users to the wrong file.
func TestEventEmitSaysWhenARunDeclaresNoWatchersAtAll(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", "", "", true)

	got := arxi(t, dir, "event", "emit", "r1", "custom.deploy")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.out)
	}
	if !strings.Contains(got.out, "no watchers at all") {
		t.Errorf("a run with no watchers is not told so:\n%s\n"+
			"  consequence: the user retypes the event type looking for a "+
			"spelling mistake, when no type whatsoever would start work here.",
			got.out)
	}
}

// TestEventEmitRefusesWhenAPausedRunWouldParkTheCause.
//
// The refusal is what `run prompt` does too, and the reason is the same: a
// halted run parks causes rather than dropping them, so the emit "works", exits
// 0, and starts nothing -- which is indistinguishable from the command failing.
func TestEventEmitRefusesWhenAPausedRunWouldParkTheCause(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchCustom,
		`{"id":"e3","seq":3,"type":"run.paused","payload":{}}
`, true)

	got := arxi(t, dir, "event", "emit", "r1", "custom.deploy")
	if got.code != 1 {
		t.Fatalf("emitting into a paused run with a matching watcher exited %d, "+
			"want 1:\n%s\n"+
			"  consequence: the cause is parked until somebody unpauses, and "+
			"nothing on screen said so.", got.code, got.out)
	}
	if !strings.Contains(got.out, "arxi run unpause r1") {
		t.Errorf("the refusal does not name the command that clears it:\n%s\n"+
			"  consequence: paused is the one halted status the user themselves "+
			"caused, so the remedy is one command and it should be printed.",
			got.out)
	}
	if !strings.Contains(got.out, "not lost") {
		t.Errorf("the refusal does not say the cause would be kept:\n%s\n"+
			"  consequence: the user assumes parking means losing and re-emits "+
			"after unpausing, doubling the event.", got.out)
	}
}

// TestEventEmitStillRecordsIntoAPausedRunWhenNothingWatches is the asymmetry.
//
// `run prompt` refuses on a paused run unconditionally, and copying that here
// would have been wrong: an emit has two legitimate purposes, and only one of
// them spends money. Refusing the other would teach people to unpause a run in
// order to write a note in it -- which is the opposite of what pausing is for.
func TestEventEmitStillRecordsIntoAPausedRunWhenNothingWatches(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchSomethingElse,
		`{"id":"e3","seq":3,"type":"run.paused","payload":{}}
`, true)

	got := arxi(t, dir, "event", "emit", "r1", "custom.note")
	if got.code != 0 {
		t.Fatalf("emitting a record into a paused run exited %d, want 0:\n%s\n"+
			"  consequence: annotating a paused run -- the state in which a person "+
			"is most likely to be writing notes about why they paused it -- "+
			"requires unpausing first, which resumes spending.", got.code, got.out)
	}
	if ev := lastEvent(t, dir, "r1"); ev["type"] != "custom.note" {
		t.Errorf("the last event in the log is %v, want custom.note", ev["type"])
	}
}

// TestEventEmitPointsABlockedRunAtRunWhy.
//
// Blocked and paused are both halted and the remedy differs: `run unpause` on a
// blocked run does nothing useful, because whatever blocked it is still true.
// `run why` is the verb that walks the reference graph to the thing that needs
// answering, which is why the message branches on the status rather than
// printing one remedy for both.
func TestEventEmitPointsABlockedRunAtRunWhy(t *testing.T) {
	dir := t.TempDir()
	// budget.exceeded, checked against decide.go:128 -- it is what actually sets
	// StatusBlocked. tool.call_denied looks like it should and does not.
	emitRunAt(t, dir, "r1", watchCustom,
		`{"id":"e3","seq":3,"type":"budget.exceeded","payload":{"spent_usd":1.2}}
`, true)

	got := arxi(t, dir, "event", "emit", "r1", "custom.deploy")
	if got.code != 1 {
		t.Fatalf("emitting into a blocked run with a matching watcher exited %d, "+
			"want 1:\n%s", got.code, got.out)
	}
	if !strings.Contains(got.out, "arxi run why r1") {
		t.Errorf("a blocked run is not pointed at `run why`:\n%s\n"+
			"  consequence: printing `run unpause` here sends the user to a "+
			"command that reports success and leaves the run just as blocked.",
			got.out)
	}
}

// TestEventEmitRefusesATerminalRun.
//
// Decide returns immediately on a terminal status, so an append here is folded by
// nothing: the bytes land in the log and no state, no watcher and no reader ever
// sees them. That is the one case where recording is worse than refusing, because
// the log is the source of truth and this would put an unobservable row in it.
func TestEventEmitRefusesATerminalRun(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchCustom,
		`{"id":"e3","seq":3,"type":"stage.submitted","actor":"backend","payload":{"agent":"backend","stage":"execute"}}
{"id":"e4","seq":4,"type":"run.result","payload":{"summary":"done"}}
`, true)

	got := arxi(t, dir, "event", "emit", "r1", "custom.deploy")
	if got.code != 1 {
		t.Fatalf("emitting into a terminal run exited %d, want 1:\n%s\n"+
			"  consequence: the event is written and folded by nobody, so the log "+
			"holds a row that no state, watcher or reader will ever reflect.",
			got.code, got.out)
	}
	if !strings.Contains(got.out, "arxi run fork r1") {
		t.Errorf("the refusal does not offer the way to carry on:\n%s\n"+
			"  consequence: a finished run is not a mistake to be undone, it is a "+
			"point to branch from, and fork is the only verb that does that.",
			got.out)
	}
	if !strings.Contains(got.out, "--at-seq") {
		t.Errorf("the fork hint omits --at-seq:\n%s\n"+
			"  consequence: the user has to look up which seq to fork at, and the "+
			"command that just folded the whole log already knows.\n"+
			"  it is --at-seq and not --from-seq: the registry declares at-seq, "+
			"and a printed flag that does not exist is worse than no hint, "+
			"because it is refused by the very command being recommended.", got.out)
	}
}

// TestEventEmitDrivesTheRunItWakes is the test that would have caught the defect
// this verb shipped with in its first draft.
//
// Effects are transient: wakeWatchers returns SpawnTurn from Decide, and Decide's
// return value lives only as long as the call. A command that appends the cause
// and exits has produced a log in which a watcher matched and nothing happened --
// and it looks completely correct, because the emit line prints and the exit code
// is 0. The only observable difference is whether events appear AFTER the one that
// was emitted.
func TestEventEmitDrivesTheRunItWakes(t *testing.T) {
	dir := t.TempDir()
	// simulated: true, so the turn is taken by the fake executor. Without it this
	// test would call a model and charge for it.
	emitRunAt(t, dir, "r1", watchCustom, "", true)

	got := arxi(t, dir, "event", "emit", "r1", "custom.deploy")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.out)
	}
	if !strings.Contains(got.out, "opens a turn for security") {
		t.Errorf("the output does not say a turn is being opened:\n%s\n"+
			"  consequence: the user cannot tell this emit from one nothing "+
			"watched, and this one is about to spend money.", got.out)
	}

	// The proof is in the log, not the message: the watcher's agent has to have
	// actually been activated after the emitted event.
	log := arxi(t, dir, "event", "log", "r1")
	if !strings.Contains(log.out, "agent.activated") {
		t.Errorf("no turn was ever opened:\n%s\n"+
			"  consequence: the emit recorded a cause and discarded the effect, so "+
			"the run holds a matched watcher that never fired -- and every "+
			"printed line said otherwise.", log.out)
	}
	if strings.Index(log.out, "custom.deploy") > strings.Index(log.out, "agent.activated") {
		t.Errorf("the activation precedes the emit that caused it:\n%s\n"+
			"  consequence: the log's order is its causality, and a reader would "+
			"attribute this turn to something else entirely.", log.out)
	}
}

// TestEventEmitNamesAWaitingAgentWithoutAnEmptyParenthetical.
//
// A member blocked without a `blocked_on` key printed "security is blocked ()",
// which reads as a detail that was lost rather than one that was never there.
// Detail is optional -- applyBlocked copies it straight out of the payload -- so
// the parenthetical has to be conditional on having something to say.
func TestEventEmitNamesAWaitingAgentWithoutAnEmptyParenthetical(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchCustom,
		`{"id":"e3","seq":3,"type":"agent.blocked","actor":"security","payload":{"agent":"security"}}
`, true)

	got := arxi(t, dir, "event", "emit", "r1", "custom.deploy")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.out)
	}
	if strings.Contains(got.out, "()") {
		t.Errorf("an empty parenthetical was printed:\n%s\n"+
			"  consequence: it reads as a truncated string, so the user looks for "+
			"the missing detail instead of reading the sentence.", got.out)
	}
	if !strings.Contains(got.out, "queued") {
		t.Errorf("a cause parked behind a waiting member is not called queued:\n%s\n"+
			"  consequence: this emit starts nothing now and is not lost either, "+
			"and only one word covers both.", got.out)
	}
	// And it did NOT drive: a waiting member takes the cause onto its pending
	// list rather than opening a turn.
	if strings.Contains(got.out, "agent.activated") {
		t.Errorf("the run was driven anyway:\n%s", got.out)
	}
}

// TestEventEmitRecordsAnUnattributedHumanEvent.
//
// Actor is left EMPTY deliberately, and it is the sort of field somebody fills in
// later to be helpful. wakeWatchers excludes a watcher whose Agent equals the
// event's Actor unless include_self is set, so naming an agent here would silently
// switch off that agent's own watcher -- a CLI emit that works for every member
// except the one it was attributed to.
func TestEventEmitRecordsAnUnattributedHumanEvent(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchSomethingElse, "", true)

	if got := arxi(t, dir, "event", "emit", "r1", "custom.contract_frozen"); got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.out)
	}
	ev := lastEvent(t, dir, "r1")

	if ev["type"] != "custom.contract_frozen" {
		t.Errorf("the recorded type is %v, want custom.contract_frozen verbatim:\n%v\n"+
			"  consequence: watcher patterns match on this string, so any "+
			"normalisation makes the type the user was shown differ from the one "+
			"they must write a pattern against.", ev["type"], ev)
	}
	if ev["source"] != "human" {
		t.Errorf("source is %v, want human:\n%v\n"+
			"  consequence: Decide skips wakeWatchers entirely for "+
			"source=runtime, so a mislabelled source means the emit wakes nobody "+
			"and every message about which watcher matched was a lie.",
			ev["source"], ev)
	}
	if a, ok := ev["actor"]; ok && a != "" {
		t.Errorf("actor is %q, want absent:\n%v\n"+
			"  consequence: wakeWatchers skips a watcher whose agent equals the "+
			"actor unless include_self is set, so attributing this event to an "+
			"agent silently disables that agent's own watcher.", a, ev)
	}
}

// TestEventEmitWithNoArgumentsExplainsTheNamespace.
//
// The usage is where a first-time caller learns that `custom.` is mandatory, and
// it is cheaper to read it there than to be refused twice.
func TestEventEmitWithNoArgumentsExplainsTheNamespace(t *testing.T) {
	dir := t.TempDir()

	got := arxi(t, dir, "event", "emit")
	if got.code != 2 {
		t.Fatalf("`event emit` with no arguments exited %d, want 2:\n%s",
			got.code, got.out)
	}
	for _, want := range []string{"usage: arxi event emit", "custom.", "arxi run list"} {
		if !strings.Contains(got.out, want) {
			t.Errorf("the usage omits %q:\n%s\n"+
				"  consequence: this verb needs a run id it cannot default and a "+
				"namespace it cannot infer, and both have to be in the usage or "+
				"the user learns them by being refused.", want, got.out)
		}
	}
}

// TestEventEmitNeedsARunItCanFind.
//
// There is deliberately no "the latest run" default: guessing which run a write
// lands in is the one guess an append-only log cannot take back. So a bad id has
// to fail loudly rather than resolve to something plausible.
func TestEventEmitNeedsARunItCanFind(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchCustom, "", true)

	got := arxi(t, dir, "event", "emit", "r2", "custom.deploy")
	if got.code == 0 {
		t.Fatalf("emitting into a run that does not exist succeeded:\n%s\n"+
			"  consequence: if this ever resolves to some other run, the event is "+
			"in a log it cannot be removed from.", got.out)
	}
	if strings.Contains(got.out, "r1") {
		t.Errorf("the failure mentions another run:\n%s\n"+
			"  consequence: it reads as though r1 were used instead.", got.out)
	}
}
