package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `arxi state set`, exercised as a process against real run directories.
//
// The store's whole purpose is that one member can tell another something
// without paying for a turn (docs/design/20-use-cases.md §20.8), so the defects
// worth guarding are the ones where the telling silently does not happen:
//
//   - a key written under a name `state get` cannot ask for -- padded, empty, or
//     carrying a newline -- reported as success;
//   - a write that wakes a watcher and does not drive, which leaves a matched
//     watcher that never fired and prints nothing about it;
//   - a write into a halted run, which stores the key legitimately and must NOT
//     claim a turn started -- and must refuse outright in the one case where the
//     reducer's answer is dropped rather than parked;
//   - a write into a terminal run, which the fold ignores entirely;
//   - a --if-seq that does not hold, refused under this command's own name and
//     leaving no writer.lock behind: two members writing one key is what the guard
//     is for, so this is the store's most ordinary failure rather than its rarest;
//   - a header that reaches the wrong audience -- source=runtime skips every
//     watcher, and an actor switches off that member's own watcher on state.*.
//
// emitRunAt (emit_cli_test.go) is the fixture, not runAt: a watcher must name a
// declared member, and this file is about which watcher a state.set matches. The
// logs are hand-written for the reason that file gives -- paused, blocked and
// terminal are not producible on demand by `run start --sim`.

// watchState is the declaration the store exists to serve: somebody who wants to
// be woken when a contract lands.
const watchState = "  - {agent: security, pattern: state.*, action: notify}\n"

// watchStateRunsATool is the one action that cannot survive a halted run.
//
// wakeWatchers returns CallTool unconditionally, so spawnCauses never parks it and
// the next fold discards it. `read` is in security's tool list in emitRunAt, which
// keeps the fixture one a blueprint would accept.
const watchStateRunsATool = "  - {agent: security, pattern: state.*, action: run_tool, tool: read}\n"

// paused is the halt a person caused and can undo in one command.
const pausedAt3 = `{"id":"e3","seq":3,"type":"run.paused","payload":{}}
`

// stateLog is the run's log as bytes, for the assertions that have to prove a
// refusal wrote NOTHING. A refusal that still appends is worse than an acceptance,
// because the log then holds the event the message said it would not hold.
func stateLog(t *testing.T, dir, run string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "runs", run, "events.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestStateSetRefusesAKeyNothingCanReadBack.
//
// Every one of these keys is a key the REDUCER would take, which is what makes the
// refusals worth having: the write would succeed, the command would print a seq,
// and `state get` would answer nothing -- so the failure surfaces days later as a
// member that never reacted to a contract somebody is sure they froze.
func TestStateSetRefusesAKeyNothingCanReadBack(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchState, "", true)

	for _, tc := range []struct{ key, says string }{
		{"", "which key?"},
		{" api.contract", "padded with whitespace"},
		{"api.contract ", "padded with whitespace"},
		{"api\ncontract", "line break or a tab"},
		{"api\tcontract", "line break or a tab"},
	} {
		got := arxi(t, dir, "state", "set", "r1", tc.key, "frozen")
		if got.code != 2 {
			t.Errorf("key %q exited %d, want 2:\n%s\n"+
				"  consequence: the store is an exact lookup, so this records an "+
				"event the command reports as written and `state get` cannot ask "+
				"for -- and every state.* watcher is billed a turn to read it.",
				tc.key, got.code, got.out)
		}
		if !strings.Contains(got.out, tc.says) {
			t.Errorf("refusing %q never says %q:\n%s\n"+
				"  consequence: the key looks correct on screen -- that is the whole "+
				"problem with a padded one -- so the message has to name what is "+
				"wrong with it or the user retypes it identically.",
				tc.key, tc.says, got.out)
		}
	}

	if log := stateLog(t, dir, "r1"); strings.Contains(log, "state.set") {
		t.Errorf("a refused key still reached the log:\n%s\n"+
			"  consequence: the fold applies it anyway, so the refusal was cosmetic.",
			log)
	}
}

// TestStateSetSeparatesAnAbsentValueFromAnEmptyOne.
//
// spec/events.md gives the store no delete, on the grounds that "a key that
// vanished from the fold could not be told from a key nobody ever set". So `""` is
// the nearest thing to emptying a key and has to be sayable -- while `state set r1
// api.contract` is an unfinished command, and accepting it would store content the
// caller never chose and wake every state.* watcher to read it.
//
// The refusal comes out of parseInvocation, because pos() marks the value Required.
// This test does not care which layer answers; it cares that the two spellings do
// not get the same answer, and that the one that IS legal survives to the log.
func TestStateSetSeparatesAnAbsentValueFromAnEmptyOne(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchSomethingElse, "", true)

	got := arxi(t, dir, "state", "set", "r1", "api.contract")
	if got.code != 2 {
		t.Fatalf("a missing value exited %d, want 2:\n%s\n"+
			"  consequence: the key is stored with content nobody chose, and every "+
			"state.* watcher pays a turn to go and read it.", got.code, got.out)
	}
	if !strings.Contains(got.out, "value") {
		t.Errorf("the refusal never names the missing parameter:\n%s", got.out)
	}
	if !strings.Contains(got.out, `"" is a value`) {
		t.Errorf("the refusal does not say an empty value is legal:\n%s\n"+
			"  consequence: this is the exact moment a user wants to empty a key, "+
			"and there is no delete to find instead -- so if the usage printed here "+
			"does not say `\"\"`, they conclude the store cannot express it.", got.out)
	}

	if got := arxi(t, dir, "state", "set", "r1", "api.contract", ""); got.code != 0 {
		t.Fatalf("an explicit empty value was refused with %d:\n%s\n"+
			"  consequence: emptying a key is the only thing this store has in place "+
			"of a delete, and refusing it leaves no way to say it at all.",
			got.code, got.out)
	}
	ev := eventOfType(t, dir, "r1", "state.set")
	pay, _ := ev["payload"].(map[string]any)
	if pay["key"] != "api.contract" {
		t.Errorf("the recorded key is %v, want api.contract verbatim:\n%v", pay["key"], ev)
	}
	if v, ok := pay["value"]; !ok || v != "" {
		t.Errorf("the recorded value is %v (present=%v), want an empty string:\n%v\n"+
			"  consequence: the reducer stores what is in the payload, so a value "+
			"key that is absent rather than empty is a key whose emptiness was lost "+
			"between the shell and the fold.", v, ok, ev)
	}
}

// TestStateSetDrivesTheWatcherItWakes is the defect this verb would otherwise
// ship, and it is invisible from the command's own output.
//
// wakeWatchers returns SpawnTurn from Decide, and Decide's return value lives only
// as long as the call. A command that appends the key and exits leaves a log in
// which a watcher matched and nothing happened -- and it looks completely correct,
// because the seq line prints and the exit code is 0. The only observable
// difference is whether events appear AFTER the state.set.
func TestStateSetDrivesTheWatcherItWakes(t *testing.T) {
	dir := t.TempDir()
	// simulated: true, so the turn is taken by the fake executor. Without it this
	// test would call a model and charge for it.
	emitRunAt(t, dir, "r1", watchState, "", true)

	got := arxi(t, dir, "state", "set", "r1", "api.contract", "frozen")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.out)
	}
	// The value is echoed QUOTED, which is what makes an empty value visible as an
	// empty value rather than as an argument that went missing.
	if !strings.Contains(got.out, `set api.contract = "frozen"`) {
		t.Errorf("the write is not echoed with the value it stored:\n%s", got.out)
	}
	if !strings.Contains(got.out, "opens a turn for security") {
		t.Errorf("the output does not say a turn is being opened:\n%s\n"+
			"  consequence: this write and one nothing watches both exit 0, and only "+
			"this one is about to spend money.", got.out)
	}
	if !strings.Contains(got.out, "arxi state get r1 api.contract") {
		t.Errorf("the output does not say how to read the key back:\n%s\n"+
			"  consequence: the whole point of the store is the read on the other "+
			"side, and the command that just wrote the key knows both halves of it.",
			got.out)
	}

	// The proof is in the log, not the message: the watcher's agent has to have
	// been activated, and after the key rather than before it.
	log := arxi(t, dir, "event", "log", "r1")
	if !strings.Contains(log.out, "agent.activated") {
		t.Errorf("no turn was ever opened:\n%s\n"+
			"  consequence: the write recorded the cause and discarded the effect, "+
			"so the run holds a matched watcher that never fired -- and every "+
			"printed line said otherwise.", log.out)
	}
	if strings.Index(log.out, "state.set") > strings.Index(log.out, "agent.activated") {
		t.Errorf("the activation precedes the key that caused it:\n%s\n"+
			"  consequence: the log's order is its causality, and a reader would "+
			"attribute this turn to something else entirely.", log.out)
	}
}

// TestStateSetDoesNotCallAnUnwatchedKeyUnobserved is the sentence `event emit`
// gets right and this verb must NOT copy.
//
// "recorded, and observed by nobody" is true of a custom.* emit, whose only
// purpose is to be heard. It is false of a state.set: the key is in the state
// whether or not anybody watches, and `state get` answers with it. Printing it here
// would read as a failed write, and the user's next move is to write it again.
func TestStateSetDoesNotCallAnUnwatchedKeyUnobserved(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchSomethingElse, "", true)

	got := arxi(t, dir, "state", "set", "r1", "api.contract", "frozen")
	if got.code != 0 {
		t.Fatalf("a write nothing watches exited %d, want 0:\n%s\n"+
			"  consequence: leaving a note for a member who reads it later is the "+
			"store's primary use, not a degenerate one.", got.code, got.out)
	}
	if strings.Contains(got.out, "observed by nobody") {
		t.Errorf("an unwatched key is called unobserved:\n%s\n"+
			"  consequence: the key IS stored and `state get` will answer with it, so "+
			"this reads as a failed write and the user writes it again.", got.out)
	}
	if !strings.Contains(got.out, "stored") {
		t.Errorf("the output does not say the key was stored:\n%s\n"+
			"  consequence: the only other thing on screen is a seq, and a seq is "+
			"what an event got, not what a reader gets back.", got.out)
	}
	if !strings.Contains(got.out, "pattern: state.*") {
		t.Errorf("the output does not show how a watcher on this is declared:\n%s\n"+
			"  consequence: the actionable half of \"nothing was waiting\" is the "+
			"line the user is missing from their blueprint.", got.out)
	}

	// And the key really is in the log, which is the claim the paragraph above
	// rests on.
	if pay, _ := eventOfType(t, dir, "r1", "state.set")["payload"].(map[string]any); pay["value"] != "frozen" {
		t.Errorf("the unwatched key is not in the log: %v", pay)
	}
	if strings.Contains(got.out, "agent.activated") {
		t.Errorf("the run was driven anyway:\n%s", got.out)
	}
}

// TestStateSetStillWritesIntoAPausedRunAndSaysTheTurnIsParked is the asymmetry with
// `event emit`, which refuses this outright.
//
// An emit into a halted run whose watcher matches is pure loss: waking somebody is
// the event's only purpose. A state.set has a second purpose that lands regardless
// of the status -- the key is stored and `state get` reads it back -- so refusing
// would block a good write and teach the user to unpause a run, which resumes
// spending, in order to leave a note in it.
//
// What must not happen is the write claiming a turn started. spawnCauses parks the
// cause, and the only thing that can say so is this line.
func TestStateSetStillWritesIntoAPausedRunAndSaysTheTurnIsParked(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchState, pausedAt3, true)

	got := arxi(t, dir, "state", "set", "r1", "api.contract", "frozen")
	if got.code != 0 {
		t.Fatalf("writing a key into a paused run exited %d, want 0:\n%s\n"+
			"  consequence: paused is the state in which a person is most likely to "+
			"be recording what they found, and refusing here makes them unpause -- "+
			"which resumes spending -- to write it down.", got.code, got.out)
	}
	if !strings.Contains(got.out, "parked") {
		t.Errorf("the parked turn is not mentioned:\n%s\n"+
			"  consequence: the write and the wake-up print the same way, so the "+
			"user waits for a member who will not move until the run is unpaused.",
			got.out)
	}
	if !strings.Contains(got.out, "arxi run unpause r1") {
		t.Errorf("the output does not name the command that clears the halt:\n%s\n"+
			"  consequence: paused is the one halted status the user themselves "+
			"caused, so the remedy is one command and it should be printed.", got.out)
	}

	// Stored, which is the half that justifies not refusing.
	if pay, _ := eventOfType(t, dir, "r1", "state.set")["payload"].(map[string]any); pay["value"] != "frozen" {
		t.Errorf("the key was not written into the paused run: %v", pay)
	}
	// And not driven: writing a key must not resume a run somebody paused.
	if strings.Contains(stateLog(t, dir, "r1"), "agent.activated") {
		t.Errorf("a paused run was driven by a state write:\n%s\n"+
			"  consequence: `state set` becomes a way to resume spending, which is "+
			"what `run unpause` exists to be asked for explicitly.",
			stateLog(t, dir, "r1"))
	}
}

// TestStateSetPointsABlockedRunAtRunWhy.
//
// Blocked and paused are both halted and the remedy is not the same command.
// `run unpause` on a blocked run either refuses or immediately re-breaches,
// because whatever blocked it is still true; `run why` is the verb that walks to
// the thing that needs answering. haltRemedy branches on the status for that
// reason, and this is the half of the branch the paused test cannot reach.
func TestStateSetPointsABlockedRunAtRunWhy(t *testing.T) {
	dir := t.TempDir()
	// budget.exceeded, not tool.call_denied: decide.go:128 is what actually sets
	// StatusBlocked, and the other one looks like it should and does not.
	emitRunAt(t, dir, "r1", watchState,
		`{"id":"e3","seq":3,"type":"budget.exceeded","payload":{"spent_usd":1.2}}
`, true)

	got := arxi(t, dir, "state", "set", "r1", "api.contract", "frozen")
	if got.code != 0 {
		t.Fatalf("writing a key into a blocked run exited %d, want 0:\n%s\n"+
			"  consequence: a run stops on a full budget, and recording what the "+
			"next member needs to know is exactly what a person does next.",
			got.code, got.out)
	}
	if !strings.Contains(got.out, "arxi run why r1") {
		t.Errorf("a blocked run is not pointed at `run why`:\n%s\n"+
			"  consequence: printing `run unpause` here sends the user to a command "+
			"that reports success and leaves the run just as blocked.", got.out)
	}
	if strings.Contains(got.out, "opens a turn") {
		t.Errorf("a blocked run's write claims a turn was opened:\n%s\n"+
			"  consequence: spawnCauses consults the status before it looks at the "+
			"member, so the per-watcher note is wrong on a halted run -- and the "+
			"user waits for a member the reducer never activated.", got.out)
	}
	if pay, _ := eventOfType(t, dir, "r1", "state.set")["payload"].(map[string]any); pay["value"] != "frozen" {
		t.Errorf("the key was not written into the blocked run: %v", pay)
	}
}

// TestStateSetRefusesAHaltedRunWhenAWatcherWouldRunATool is the one exception to
// the two tests above, and the reason it is an exception is a difference in the
// reducer rather than a difference in politeness.
//
// A notify or activate cause is parked into Member.PendingCauses, which is part of
// State: it survives in the log's own fold and drainParked hands it back when the
// halt clears. A CallTool effect is returned by wakeWatchers UNCONDITIONALLY --
// spawnCauses never sees it, so nothing parks it -- and no later drive re-decides
// this event, because the next drive folds everything below its cursor into a
// starting state and keeps no effects. So the tool call is dropped in silence.
//
// Refusing costs nothing here: nothing has been written, and clearing the halt and
// repeating the command is a whole recovery. Writing costs the tool call.
func TestStateSetRefusesAHaltedRunWhenAWatcherWouldRunATool(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchStateRunsATool, pausedAt3, true)

	got := arxi(t, dir, "state", "set", "r1", "api.contract", "frozen")
	if got.code != 1 {
		t.Fatalf("a halted run whose watcher runs a tool exited %d, want 1:\n%s\n"+
			"  consequence: the key is stored and the tool call is discarded by the "+
			"next fold, so the run holds a matched run_tool watcher that provably "+
			"never ran -- and exit 0 said the opposite.", got.code, got.out)
	}
	if !strings.Contains(got.out, "read for security") {
		t.Errorf("the refusal does not name the tool and the member:\n%s\n"+
			"  consequence: the user has to find which of their watchers is a "+
			"run_tool before they can decide whether to clear the halt.", got.out)
	}
	if !strings.Contains(got.out, "nothing was written") {
		t.Errorf("the refusal does not say the write did not happen:\n%s\n"+
			"  consequence: an append-only log cannot be asked, so a user who "+
			"assumes the key landed will not repeat the command after unpausing.",
			got.out)
	}
	for _, want := range []string{"arxi run unpause r1", `arxi state set r1 api.contract "frozen"`} {
		if !strings.Contains(got.out, want) {
			t.Errorf("the refusal does not print %q:\n%s\n"+
				"  consequence: the recovery is two commands in order, and the second "+
				"one is the command that was just refused -- retyping it is where the "+
				"value gets mistyped.", want, got.out)
		}
	}

	// The claim the refusal makes about itself, checked against the bytes.
	if log := stateLog(t, dir, "r1"); strings.Contains(log, "state.set") {
		t.Errorf("the refused write reached the log anyway:\n%s\n"+
			"  consequence: the message says nothing was written, so the user does "+
			"not repeat it -- and the key is in the log with its watcher unfired.",
			log)
	}
}

// TestStateSetRefusesATerminalRun, where it is the WRITE that would be lost rather
// than merely the watcher.
//
// Decide returns before its switch when the status is terminal, so the StateSet arm
// is never reached: the key would be appended to the log and absent from every fold
// of it. `state get` would answer nothing for a write this command had just
// reported as done, and the log would hold the row that proves it was asked for.
func TestStateSetRefusesATerminalRun(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchState,
		`{"id":"e3","seq":3,"type":"stage.submitted","actor":"backend","payload":{"agent":"backend","stage":"execute"}}
{"id":"e4","seq":4,"type":"run.result","payload":{"summary":"done"}}
`, true)

	got := arxi(t, dir, "state", "set", "r1", "api.contract", "frozen")
	if got.code != 1 {
		t.Fatalf("writing a key into a terminal run exited %d, want 1:\n%s\n"+
			"  consequence: the key is in the log and in no fold of it, so the store "+
			"the command reported writing to does not have it.", got.code, got.out)
	}
	if !strings.Contains(got.out, "read back by nobody") {
		t.Errorf("the refusal does not say the key would be unreadable:\n%s\n"+
			"  consequence: \"the run is finished\" reads as a rule, and a user who "+
			"thinks it is only a rule looks for a flag to override it.", got.out)
	}
	for _, want := range []string{"arxi run fork r1", "--at-seq"} {
		if !strings.Contains(got.out, want) {
			t.Errorf("the refusal does not print %q:\n%s\n"+
				"  consequence: there IS a way to carry on from a finished run, and a "+
				"refusal that does not name it reads as a dead end.", want, got.out)
		}
	}

	if log := stateLog(t, dir, "r1"); strings.Contains(log, "state.set") {
		t.Errorf("the refused write reached the log anyway:\n%s\n"+
			"  consequence: this is precisely the unobservable row the refusal exists "+
			"to keep out of the log.", log)
	}
}

// TestAStaleStateSetCASRefusesUnderThisCommandsOwnName.
//
// The store is the one place in the surface where two members write the same key,
// so --if-seq is not decoration here: it is how "freeze the contract only if
// nobody has moved it since I read it" gets said. Three things have to be true of
// the refusal, and each has been wrong in this binary before:
//
//   - it names `state set`, not the command refuseStaleCAS was written for. cmd is
//     a parameter for that reason, and a caller passing the wrong string sends the
//     user to another verb's docs;
//   - it says nothing was written, because an append-only log cannot be asked;
//   - it leaves no writer.lock. The refusal exits 1 from inside refuseStaleCAS,
//     which runs no defers, so this path is exactly the one that bricked a run on
//     `run prompt`.
func TestAStaleStateSetCASRefusesUnderThisCommandsOwnName(t *testing.T) {
	dir := t.TempDir()
	// The fixture's head is seq 2, so guarding on 1 is a run that moved by one.
	emitRunAt(t, dir, "r1", watchSomethingElse, "", true)

	got := arxi(t, dir, "state", "set", "r1", "api.contract", "frozen", "--if-seq", "1")
	if got.code != 1 {
		t.Fatalf("a stale --if-seq exited %d, want 1:\n%s\n"+
			"  consequence: the guard is what makes a concurrent write safe, and one "+
			"that does not refuse overwrites the value it was told to check.",
			got.code, got.out)
	}
	for _, want := range []string{
		"arxi state set:", "the run moved", "nothing was written",
		"arxi event log r1 --since-seq 2",
	} {
		if !strings.Contains(got.out, want) {
			t.Errorf("the CAS refusal does not say %q:\n%s\n"+
				"  consequence: the user has to be told which command refused, that "+
				"the log is unchanged, and how to read what happened instead -- the "+
				"last one with a flag `event log` actually accepts.", want, got.out)
		}
	}
	if log := stateLog(t, dir, "r1"); strings.Contains(log, "state.set") {
		t.Errorf("a refused CAS wrote the key anyway:\n%s", log)
	}

	// The lock, and the proof that matters more than the lock: the next command.
	if _, err := os.Stat(filepath.Join(dir, "runs", "r1", "writer.lock")); err == nil {
		t.Errorf("writer.lock survived a refused CAS\n" +
			"  consequence: it does not merely linger -- every later command on this " +
			"run is refused with advice to delete a lock file by hand, for a run that " +
			"only hit a stale guard.")
	}
	if next := arxi(t, dir, "run", "show", "r1"); strings.Contains(next.out, "open for writing") {
		t.Fatalf("a refused CAS bricked the run for the next command:\n%s", next.out)
	}
}

// TestAStateSetCASAheadOfTheHeadIsNotCalledMovement is the other branch of the
// same refusal, and it exists because the arithmetic used to produce nonsense.
//
// A seq the run has not reached yet means the caller read some OTHER run, or
// mistyped. "The run moved" is false, and Actual-Expected is negative, so the
// message said "-7 event(s) happened since you looked" -- and `event log
// --since-seq 10` past the head prints nothing, which is the worst possible advice
// in a message about catching up.
func TestAStateSetCASAheadOfTheHeadIsNotCalledMovement(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchSomethingElse, "", true)

	got := arxi(t, dir, "state", "set", "r1", "api.contract", "frozen", "--if-seq", "9")
	if got.code != 1 {
		t.Fatalf("guarding on a seq the run never reached exited %d, want 1:\n%s",
			got.code, got.out)
	}
	if !strings.Contains(got.out, "never reached seq 9") {
		t.Errorf("the refusal does not say the run never reached that seq:\n%s\n"+
			"  consequence: the alternative wording claims the run moved, which sends "+
			"the user to read a diff that does not exist instead of noticing they are "+
			"holding a seq from another run.", got.out)
	}
	if strings.Contains(got.out, "-") && strings.Contains(got.out, "event(s) happened") {
		t.Errorf("the refusal counts a negative number of events:\n%s", got.out)
	}
}

// TestAGuardedStateSetAppendsWhenTheRunHasNotMoved is the discriminating half: a
// guard that refused everything would pass both tests above and be useless.
func TestAGuardedStateSetAppendsWhenTheRunHasNotMoved(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchSomethingElse, "", true)

	// Taken from the log rather than assumed, so this stays a statement about the
	// guard and not about how many events the fixture happens to carry.
	head := len(allEvents(t, dir, "r1"))

	got := arxi(t, dir, "state", "set", "r1", "api.contract", "frozen", "--if-seq", itoa(head))
	if got.code != 0 {
		t.Fatalf("a correct --if-seq was refused: exit %d\n%s\n"+
			"  the run was at seq %d and nothing had touched it since.\n"+
			"  consequence: the guard becomes unusable, and the way people work "+
			"around an unusable guard is to stop passing it.", got.code, got.out, head)
	}
	if pay, _ := eventOfType(t, dir, "r1", "state.set")["payload"].(map[string]any); pay["value"] != "frozen" {
		t.Errorf("the guarded write did not reach the log: %v", pay)
	}
}

// TestStateSetRefusesAMalformedIfSeqBeforeItOpensTheRun.
//
// parseInvocation hands --if-seq through as a string, so a typo is this command's
// to catch. Two things are being pinned. The exit code is 2 and not 1, because a
// misspelled flag value is misuse rather than a rejected write -- a script that
// distinguishes them would otherwise retry a typo forever as though the run had
// moved. And nothing is opened: the parse happens above logstore.Open, so a bad
// guard cannot leave a lock or a partial write behind.
func TestStateSetRefusesAMalformedIfSeqBeforeItOpensTheRun(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchSomethingElse, "", true)

	got := arxi(t, dir, "state", "set", "r1", "api.contract", "frozen", "--if-seq", "abc")
	if got.code != 2 {
		t.Fatalf("a malformed --if-seq exited %d, want 2:\n%s\n"+
			"  consequence: exit 1 is what a refused CAS returns, so a caller that "+
			"retries on 1 retries a typo that cannot ever succeed.", got.code, got.out)
	}
	if !strings.Contains(got.out, "arxi run show r1") {
		t.Errorf("the refusal does not say where the run's seq comes from:\n%s\n"+
			"  consequence: the value this flag wants is a number the user has to "+
			"read off something, and the command that prints it is one line away.",
			got.out)
	}
	if log := stateLog(t, dir, "r1"); strings.Contains(log, "state.set") {
		t.Errorf("a malformed guard still wrote the key:\n%s\n"+
			"  consequence: the guard was the whole reason for the command, so writing "+
			"unguarded is the one outcome the caller did not ask for.", log)
	}
	if _, err := os.Stat(filepath.Join(dir, "runs", "r1", "writer.lock")); err == nil {
		t.Errorf("writer.lock exists after a refusal that never opened the run")
	}
}

// TestStateSetRecordsAnUnattributedHumanEvent pins the three header fields, and
// each of them changes who the write reaches.
//
// source=human, because Decide skips wakeWatchers entirely for source=runtime: a
// key stamped runtime provably wakes nobody, and every line the command printed
// about which watcher matched would be a lie. It is also the only thing in the log
// that can later answer "who froze this contract" with "a person did".
//
// actor absent, which is the field it is most tempting to fill in. wakeWatchers
// skips a watcher whose agent equals the actor unless include_self is set, so
// naming a member here silently switches off that member's own watcher on state.*
// -- a write that works for everybody except the one it was attributed to. No
// member set this key; a shell did.
//
// And the payload verbatim, because the reducer stores exactly these two fields
// and `state get` is an exact lookup on the first of them.
func TestStateSetRecordsAnUnattributedHumanEvent(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchSomethingElse, "", true)

	if got := arxi(t, dir, "state", "set", "r1", "api.contract", "v2 frozen"); got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.out)
	}
	ev := lastEvent(t, dir, "r1")

	if ev["type"] != "state.set" {
		t.Fatalf("the recorded type is %v, want state.set:\n%v", ev["type"], ev)
	}
	if ev["source"] != "human" {
		t.Errorf("source is %v, want human:\n%v\n"+
			"  consequence: Decide skips wakeWatchers outright for source=runtime, so "+
			"a mislabelled source appends a key that wakes nobody -- and the output "+
			"named a watcher it would wake.", ev["source"], ev)
	}
	if a, ok := ev["actor"]; ok && a != "" {
		t.Errorf("actor is %q, want absent:\n%v\n"+
			"  consequence: wakeWatchers skips a watcher whose agent equals the actor "+
			"unless include_self is set, so attributing this key to a member disables "+
			"exactly that member's watcher on it.", a, ev)
	}
	pay, _ := ev["payload"].(map[string]any)
	if pay["key"] != "api.contract" || pay["value"] != "v2 frozen" {
		t.Errorf("the payload is %v, want key and value verbatim:\n%v\n"+
			"  consequence: the store is an exact lookup, so anything done to either "+
			"string here is a key `state get` cannot ask for or a value it answers "+
			"wrongly with.", pay, ev)
	}
}

// TestStateSetWithNoArgumentsExplainsTheStore.
//
// The usage is the only place two facts about this store are written down, and
// both of them are things a caller would otherwise learn by getting them wrong:
// that `""` is a legal value and there is no delete, and that the write has a
// matching read on the other side. A user who does not know the second one has no
// reason to believe the key went anywhere.
func TestStateSetWithNoArgumentsExplainsTheStore(t *testing.T) {
	dir := t.TempDir()

	got := arxi(t, dir, "state", "set")
	if got.code != 2 {
		t.Fatalf("`state set` with no arguments exited %d, want 2:\n%s",
			got.code, got.out)
	}
	for _, want := range []string{
		"usage: arxi state set",
		`"" is a value`,
		"arxi state get",
		"--type state.set",
	} {
		if !strings.Contains(got.out, want) {
			t.Errorf("the usage omits %q:\n%s\n"+
				"  consequence: this verb takes three things it cannot default and "+
				"belongs to a pair, so anything the usage leaves out is learned by "+
				"being refused -- or not learned at all.", want, got.out)
		}
	}
}

// TestTheStateGroupAnswersForVerbsItDoesNotRun.
//
// cmdState routes the verbs it has and sends everything else to notImplemented,
// which reads the group out of the registry rather than keeping its own list. The
// defect that guards against is the one `model` and `trigger` both shipped: a group
// that grows a dispatcher starts answering "unknown command" for capabilities
// `arxi surface` publishes, which sends the user hunting for a typo they never made.
//
// The name of this test is now half a lie -- every verb in the group is wired, so
// there is no capability it answers FOR -- and it is kept because the two shapes it
// pins are the ones that outlive that. `arxi state` one word short and `arxi state
// frobnicate` are still different mistakes with different answers, and both answers
// are still built from the registry, so the fifth verb of this group will be listed
// here on the day it is declared and before anything runs it.
//
// What is NOT pinned here is which verb is unwired. `state get` was next once, then
// `state lock`, then `state unlock`; an assertion naming one would have to be edited
// by the commit that fixed it -- and a test whose failure means "you succeeded"
// trains people to edit tests. surface_coverage_test.go is where that moving fact
// lives, measured against the registry rather than written down.
func TestTheStateGroupAnswersForVerbsItDoesNotRun(t *testing.T) {
	dir := t.TempDir()

	// A path that stops one word short. There is no wrong word to quote, so the
	// whole answer is the list -- and blaming a word the user got right, which is
	// what this used to do, is worse than useless.
	bare := arxi(t, dir, "state")
	if bare.code != 2 {
		t.Fatalf("`arxi state` exited %d, want 2:\n%s", bare.code, bare.out)
	}
	// "unlock" rather than "lock", which every listing of the group would contain by
	// accident: `lock` is a prefix of it, so asserting the shorter string would pass
	// on a listing that had dropped the newer verb entirely.
	for _, want := range []string{"needs a subcommand", "it accepts:", "get", "unlock"} {
		if !strings.Contains(bare.out, want) {
			t.Errorf("`arxi state` does not say %q:\n%s\n"+
				"  consequence: the group is real and the user is one word away, so "+
				"the verbs it accepts are the entire answer.", want, bare.out)
		}
	}

	// A verb that does not exist anywhere. The blame belongs on the word that is
	// actually wrong, and the group has to be confirmed as real in the same breath.
	wrong := arxi(t, dir, "state", "frobnicate")
	if wrong.code != 2 {
		t.Fatalf("`arxi state frobnicate` exited %d, want 2:\n%s", wrong.code, wrong.out)
	}
	for _, want := range []string{`"frobnicate" is not a state command`, "it accepts:"} {
		if !strings.Contains(wrong.out, want) {
			t.Errorf("`arxi state frobnicate` does not say %q:\n%s\n"+
				"  consequence: \"no such command\" means check the spelling of the "+
				"whole thing; this means the group is right and only the verb is wrong, "+
				"and they send the reader to different places.", want, wrong.out)
		}
	}
}
