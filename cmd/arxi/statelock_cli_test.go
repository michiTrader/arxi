package main

import (
	"strings"
	"testing"
	"time"
)

// `arxi state lock`, exercised as a process against real run directories.
//
// The lock is cooperative: it coordinates intent, and its entire value is that
// every reader of the fold agrees on ONE holder (docs/design/20-use-cases.md
// §20.8). So the defects worth guarding here are the ones where the command
// reports a claim that is not what the fold ends up holding:
//
//   - a key no other claimant would type the same way -- padded, empty -- which
//     guards nothing while reporting that it does, and both members carry on
//     editing the files;
//   - a lease the caller never chose: a malformed or negative --ttl clamped into
//     something plausible, or a default granted silently;
//   - a refusal with the wrong exit code. 3 means "ask again later", and the loop
//     in the usage line only terminates if a live lease exits 3 while a lock that
//     can NEVER lapse exits 1;
//   - a steal that is not atomic. The release and the acquire are one batch, and
//     any seq between them is a seq at which the key reads as free;
//   - an acquire that wakes a watcher and does not drive, or one that claims a
//     turn started when the run is halted and the cause is parked.
//
// # The clock is not pinned, and does not need to be
//
// nowFunc is a var so a test can pin it, but these tests exec the built binary as
// a subprocess, so nothing in this process reaches it. The lapse paths are driven
// from the fixture instead: an expiry in 2020 is lapsed at every real now, and one
// in 2099 is live for longer than this code will run. That is deterministic
// without injecting anything, and it exercises the same comparison a user hits.
//
// emitRunAt (emit_cli_test.go) is the fixture rather than runAt, per state_cli_test.go:
// a watcher must name a declared member, and the logs are hand-written because
// paused, blocked and terminal are not producible on demand by `run start --sim`.

// watchLock is the declaration a member makes to hear that a key changed hands.
const watchLock = "  - {agent: security, pattern: lock.*, action: notify}\n"

// watchLockRunsATool is the one action a halted run destroys rather than parks.
const watchLockRunsATool = "  - {agent: security, pattern: lock.*, action: run_tool, tool: read}\n"

// The three shapes of a lock somebody ELSE holds, which is the only case the CLI
// arbitrates: two shells are one holder, so a lock held by "human" is this
// command's own. The holder is named with `actor`, which is what lockHolder reads
// first -- these are the locks an agent's tool call takes, not a shell's.
const liveBackendLock = `{"id":"e3","seq":3,"type":"lock.acquired","actor":"backend","payload":{"key":"migrations/","expires_at":"2099-01-01T00:00:00Z"}}
`

const lapsedBackendLock = `{"id":"e3","seq":3,"type":"lock.acquired","actor":"backend","payload":{"key":"migrations/","expires_at":"2020-01-01T00:00:00Z"}}
`

const eternalBackendLock = `{"id":"e3","seq":3,"type":"lock.acquired","actor":"backend","payload":{"key":"migrations/"}}
`

// TestStateLockRefusesAKeyNoClaimantWouldTypeTheSameWay.
//
// A cooperative lock works only because two members ask for the same string and
// the fold compares exactly. `migrations/ ` collides with nothing, so the acquire
// would succeed, this command would print a seq, and the next member to claim
// `migrations/` would be told the key is free -- which is the whole failure the
// type exists to prevent, reached through a command that reported success.
//
// The empty key is worse still: acquireLock drops it, so the event lands in the
// log and no fold of it holds a lock at all.
func TestStateLockRefusesAKeyNoClaimantWouldTypeTheSameWay(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchLock, "", true)

	for _, key := range []string{"", "  ", "migrations/ ", "migrations/\n"} {
		got := arxi(t, dir, "state", "lock", "r1", key)
		if got.code != 2 {
			t.Errorf("locking %q exited %d, want 2:\n%s\n"+
				"  consequence: the reducer takes this key or drops it, and either way "+
				"the next claimant of the key the caller MEANT finds it free -- so two "+
				"members edit under one lock and each believes it is theirs.",
				key, got.code, got.out)
		}
		if !strings.Contains(got.out, "arxi state lock") {
			t.Errorf("the refusal of %q does not print the usage:\n%s", key, got.out)
		}
	}

	if log := stateLog(t, dir, "r1"); strings.Contains(log, "lock.acquired") {
		t.Errorf("a refused acquire reached the log anyway:\n%s\n"+
			"  consequence: every reader of the fold now sees a lock whose key nothing "+
			"will ever ask for again, and no command in this binary releases it.", log)
	}
}

// TestStateLockTakesAFreeKeyOnALeaseItSaysItChose.
//
// The lease is the design: §20.8 names "a lock with no expiry, held by an agent
// that crashed mid-turn, stalls the run until a human notices" as the failure this
// verb exists to avoid, so a caller who says nothing gets a lease rather than
// eternity -- and is TOLD, because a default that decides how long a claim lasts
// and never mentions itself is one the caller cannot correct.
//
// The expiry has to be absolute in the payload. "10m" there would make the fold
// depend on when it ran: the same log would report the lock live in the morning
// and lapsed in the afternoon, and replay fidelity is what the design is for.
func TestStateLockTakesAFreeKeyOnALeaseItSaysItChose(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchSomethingElse, "", true)

	got := arxi(t, dir, "state", "lock", "r1", "migrations/")
	if got.code != 0 {
		t.Fatalf("taking a free key exited %d, want 0:\n%s", got.code, got.out)
	}
	for _, want := range []string{"took migrations/", "held by human until", "--ttl was not given"} {
		if !strings.Contains(got.out, want) {
			t.Errorf("the acquire does not print %q:\n%s\n"+
				"  consequence: a claim whose holder and lease are not both on screen is "+
				"one the caller cannot check against `run show`, which is where the same "+
				"two facts appear.", want, got.out)
		}
	}

	pay, _ := eventOfType(t, dir, "r1", "lock.acquired")["payload"].(map[string]any)
	exp, _ := pay["expires_at"].(string)
	until, err := time.Parse(time.RFC3339, exp)
	if err != nil {
		t.Fatalf("the recorded expiry %q is not an RFC3339 instant: %v\n"+
			"  consequence: nothing can judge this lease lapsed -- lockLapsed answers "+
			"false on an unparseable expiry -- so the key is held until the run ends "+
			"and the default lease silently became eternity.", exp, err)
	}
	if d := time.Until(until); d <= 0 || d > defaultLockTTL {
		t.Errorf("the default lease runs for %s, want at most %s and still ahead:\n%v\n"+
			"  consequence: a lease already spent is stolen by the next claimant while "+
			"this command reports having taken the key.", d, defaultLockTTL, pay)
	}
}

// TestStateLockWarnsAboutNoExpiryWhereStdoutCannotHideIt.
//
// `--ttl 0` is a real request -- a key one member owns for the whole run -- so it
// is accepted. What follows from it is not obvious and cannot be discovered from
// the log: no reader ever judges such a lease lapsed, so neither the next claimant
// nor this command's own steal reclaims the key. Somebody has to hand it back by
// name with `arxi state unlock`, or it stands until the run ends.
//
// That last clause is what the warning gained when `state unlock` landed, and it is
// asserted here rather than left to the prose: before the verb existed the sentence
// described a dead end, and a warning about a dead end teaches the caller there is
// nothing to be done.
//
// The warning is on STDERR because the callers of this verb are scripts, and a
// script writes `arxi state lock r1 k >/dev/null`. The one consequence a caller
// must not be able to redirect away is having created a lock nothing reclaims on
// its own.
func TestStateLockWarnsAboutNoExpiryWhereStdoutCannotHideIt(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchSomethingElse, "", true)

	out, errOut, code := arxiStreams(t, dir, "state", "lock", "r1", "migrations/", "--ttl", "0")
	if code != 0 {
		t.Fatalf("--ttl 0 exited %d, want 0:\n%s\n%s\n"+
			"  consequence: there would be no way to say \"this member owns the key for "+
			"the whole run\", which is a claim a blueprint legitimately makes.",
			code, out, errOut)
	}
	if !strings.Contains(out, "held by human with no expiry") {
		t.Errorf("stdout does not say the lock has no expiry:\n%s", out)
	}
	if !strings.Contains(errOut, "warning:") || !strings.Contains(errOut, "until the run ends") {
		t.Errorf("the no-expiry warning is not on stderr:\n%s\n"+
			"  consequence: `arxi state lock r1 k >/dev/null` is how a script calls this, "+
			"so a warning on stdout is a warning the caller who most needs it never "+
			"sees -- and the lock it describes is one nothing reclaims on its own.",
			errOut)
	}
	if !strings.Contains(errOut, "arxi state unlock r1 migrations/") {
		t.Errorf("the warning does not name the release that frees this key:\n%s\n"+
			"  consequence: the warning states a consequence and no remedy, which reads "+
			"as \"you have done something irreversible\" -- and the reader who believes it "+
			"leaves the key held for the life of the run.", errOut)
	}

	pay, _ := eventOfType(t, dir, "r1", "lock.acquired")["payload"].(map[string]any)
	if _, ok := pay["expires_at"]; ok {
		t.Errorf("--ttl 0 recorded an expiry anyway: %v\n"+
			"  consequence: the lease the caller refused is granted, so the next "+
			"claimant steals a key its holder was promised for the whole run.", pay)
	}
}

// TestStateLockRefusesATTLItCannotHonourRatherThanClampingIt.
//
// `1d` is how everybody writes a day and is not a Go duration, so ParseDuration's
// own "unknown unit d" is accurate and tells the caller nothing about what to type.
// A negative ttl is refused rather than clamped, per scheduler.go's --interval:
// clamping to 0 grants the eternal lock nobody asked for, clamping to the default
// grants a lease nobody asked for, and both report success for something the caller
// did not type.
//
// Both are checked BEFORE the run is opened, so neither can leave a write behind.
func TestStateLockRefusesATTLItCannotHonourRatherThanClampingIt(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchSomethingElse, "", true)

	bad := arxi(t, dir, "state", "lock", "r1", "migrations/", "--ttl", "1d")
	if bad.code != 2 {
		t.Errorf("--ttl 1d exited %d, want 2:\n%s", bad.code, bad.out)
	}
	if !strings.Contains(bad.out, "a day is 24h") {
		t.Errorf("the refusal does not say what to type instead:\n%s\n"+
			"  consequence: \"unknown unit d\" is a fact about Go's parser, and the "+
			"caller's next guess is 1day.", bad.out)
	}

	// --ttl=-5m and not --ttl -5m: expandShort reads a leading dash as a short flag,
	// and this test is about the duration rather than about the parser.
	neg := arxi(t, dir, "state", "lock", "r1", "migrations/", "--ttl=-5m")
	if neg.code != 2 {
		t.Errorf("a negative --ttl exited %d, want 2:\n%s\n"+
			"  consequence: clamped either way it grants a lease the caller never typed "+
			"-- and taken literally the lock is lapsed the instant it is written, so the "+
			"next claimant steals it while this command reports success.", neg.code, neg.out)
	}

	if log := stateLog(t, dir, "r1"); strings.Contains(log, "lock.acquired") {
		t.Errorf("a refused --ttl still wrote an acquire:\n%s\n"+
			"  consequence: an append-only log cannot be asked to take it back, which is "+
			"why every argument is checked before the run is opened.", log)
	}
}

// TestALiveLeaseIsRefusedWithTheCodeAScriptCanWaitOn.
//
// The usage line promises `until arxi state lock r1 migrations/; do sleep 5; done`,
// and that loop only terminates because a held key exits 3 while 1 stays reserved
// for "the run could not be read" -- otherwise a run id with a typo in it spins
// forever. It is the same 3 as `run result`'s "not yet" and `state get`'s "not set".
//
// The refusal must also leave the log alone. A refused claimant that appends
// anything is worse than one that succeeds, because the holder's lease would then
// be extended or replaced by the member it just turned away.
func TestALiveLeaseIsRefusedWithTheCodeAScriptCanWaitOn(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchLock, liveBackendLock, true)

	out, errOut, code := arxiStreams(t, dir, "state", "lock", "r1", "migrations/")
	if code != exitLockHeld {
		t.Fatalf("a key held on a live lease exited %d, want %d:\n%s%s\n"+
			"  consequence: 1 makes the documented wait loop indistinguishable from a "+
			"typo in the run id, and 0 would report a lock this caller does not hold.",
			code, exitLockHeld, out, errOut)
	}
	if out != "" {
		t.Errorf("the refusal wrote to stdout:\n%s\n"+
			"  consequence: a script reads stdout for the claim it got; a refusal there "+
			"is one a pipeline treats as an acquire.", out)
	}
	for _, want := range []string{"held by backend", "until arxi state lock r1 migrations/"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("the refusal does not print %q:\n%s\n"+
				"  consequence: the two things the caller came for are who has it and how "+
				"to wait for it.", want, errOut)
		}
	}

	if log := stateLog(t, dir, "r1"); strings.Count(log, "lock.acquired") != 1 {
		t.Errorf("the refused claimant touched the log:\n%s\n"+
			"  consequence: the holder's lease was rewritten by the member it turned "+
			"away, so the lock now expires when the LOSER asked rather than the winner.",
			log)
	}
}

// TestALockThatCannotLapseIsRefusedWithACodeNoPollerSpinsOn is the other half of
// the exit-code contract, and it is the half that is easy to get wrong: this is
// still "somebody else holds it", and it still must not be 3.
//
// A lock with no expiry never lapses, and this command steals only a LAPSED lease,
// so nothing reclaims the key on its own: not the next claimant, and not this
// refusal. A caller polling on 3 would spin until the run ended.
//
// `state unlock --force` ends such a lease, and that does NOT make 3 right. 3 says
// waiting alone gets the key, and here it never does -- what frees it is somebody
// deciding to, which is a command to run rather than a wait to sit through. So the
// refusal has to say which of the two situations this is, because "held by backend"
// alone reads as something that will clear, and then name the decision.
func TestALockThatCannotLapseIsRefusedWithACodeNoPollerSpinsOn(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchLock, eternalBackendLock, true)

	out, errOut, code := arxiStreams(t, dir, "state", "lock", "r1", "migrations/")
	if code != 1 {
		t.Fatalf("a lock with no expiry exited %d, want 1:\n%s%s\n"+
			"  consequence: %d is \"ask again later\", so the documented wait loop spins "+
			"until the run ends against a lease that cannot run out.",
			code, out, errOut, exitLockHeld)
	}
	if !strings.Contains(errOut, "with no expiry") ||
		!strings.Contains(errOut, "nothing reclaims the key on its own") {
		t.Errorf("the refusal does not distinguish itself from a lease that will "+
			"clear:\n%s\n"+
			"  consequence: the caller waits, and waiting is the one thing that cannot "+
			"work here.", errOut)
	}
	if !strings.Contains(errOut, "no amount of asking changes this one") {
		t.Errorf("the refusal does not say why it is not exit %d:\n%s\n"+
			"  consequence: a reader who knows 3 means \"not yet\" reads 1 as a bug in "+
			"this command rather than as a fact about the lock.", exitLockHeld, errOut)
	}
	if !strings.Contains(errOut, "arxi state unlock r1 migrations/ --force") {
		t.Errorf("the refusal names no way out:\n%s\n"+
			"  consequence: exit 1 plus \"nothing reclaims it\" is a dead end, so the "+
			"caller's remaining options are to abandon the key or to invent a way of "+
			"freeing it -- and the one that exists takes --force for a reason worth "+
			"reading before it is typed.", errOut)
	}

	if log := stateLog(t, dir, "r1"); strings.Contains(log, "lock.released") {
		t.Errorf("the refusal released a lock it could not judge lapsed:\n%s\n"+
			"  consequence: a key is taken from a holder still editing under it, which "+
			"is the collision the whole type exists to prevent.", log)
	}
}

// TestALapsedLeaseIsStolenAsOneBatchThatRecordsTheJudgement is the reclaim §20.8
// asks for, and every part of it is load-bearing.
//
// The reducer has NO clock, so it cannot decide a lease has run out; this command
// has one, and it writes the judgement down as a lock.released. That is what lets
// ExpiresAt be a string the fold never compares to a clock: the next fold
// reproduces the same locks without needing a now of its own.
//
// The release is SourceRuntime because a lapse is bookkeeping rather than a
// decision somebody took -- and wakeWatchers is skipped outright for runtime, so a
// member watching lock.* is billed one turn for the handover rather than two.
func TestALapsedLeaseIsStolenAsOneBatchThatRecordsTheJudgement(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchSomethingElse, lapsedBackendLock, true)

	got := arxi(t, dir, "state", "lock", "r1", "migrations/")
	if got.code != 0 {
		t.Fatalf("stealing a lapsed lease exited %d, want 0:\n%s\n"+
			"  consequence: a holder that crashed mid-turn stalls the run until a human "+
			"reads the log, which is the failure the lease exists to avoid.",
			got.code, got.out)
	}
	if !strings.Contains(got.out, "taken from backend") {
		t.Errorf("the steal does not name who lost the key:\n%s\n"+
			"  consequence: a lock changing hands silently is indistinguishable from one "+
			"that was free, and the previous holder may still be editing.", got.out)
	}

	// The two events are ADJACENT and in this order. A gap between them is a seq at
	// which the key reads as released-but-not-taken, and a third claimant folding the
	// log there finds it free.
	evs := allEvents(t, dir, "r1")
	rel, acq := evs[len(evs)-2], evs[len(evs)-1]
	if rel["type"] != "lock.released" || acq["type"] != "lock.acquired" {
		t.Fatalf("the log tail is %v then %v, want lock.released then lock.acquired\n"+
			"  consequence: one Append is what makes a steal safe -- logstore rolls back "+
			"a partial batch, so there is no crash that can leave the key free.",
			rel["type"], acq["type"])
	}

	if rel["source"] != "runtime" {
		t.Errorf("the release is source %v, want runtime:\n%v\n"+
			"  consequence: wakeWatchers is skipped for runtime events, so a human or "+
			"agent source here wakes every lock.* watcher a second time -- two turns "+
			"billed for one handover.", rel["source"], rel)
	}
	if acq["source"] != "human" || acq["actor"] != nil {
		t.Errorf("the acquire is source %v actor %v, want human and no actor:\n%v\n"+
			"  consequence: an actor switches off that member's own watcher on lock.*, "+
			"and it makes the fold say a member took a lock a shell took.",
			acq["source"], acq["actor"], acq)
	}
	if rel["id"] == acq["id"] {
		t.Errorf("both events of the batch share the id %v\n"+
			"  consequence: `event show` and `event trace --id` address an event by id, "+
			"so a steal is a pair of rows nothing can point at individually.", rel["id"])
	}

	relPay, _ := rel["payload"].(map[string]any)
	for k, want := range map[string]any{
		"key": "migrations/", "previous_holder": "backend",
		"reason": "expired", "expired_at": "2020-01-01T00:00:00Z",
	} {
		if relPay[k] != want {
			t.Errorf("the release records %s = %v, want %v:\n%v\n"+
				"  consequence: the log is the only record that this key was taken rather "+
				"than handed over, and `run why` reads it to explain a member that lost "+
				"its lock mid-edit.", k, relPay[k], want, relPay)
		}
	}

	// And the fold agrees: one lock, held by the shell that stole it.
	show := arxi(t, dir, "run", "show", "r1")
	if !strings.Contains(show.out, "migrations/  held by human") {
		t.Errorf("after the steal `run show` reads:\n%s\n"+
			"  consequence: two rows for one key means both claimants believe they hold "+
			"it, which is exactly what acquireLock's one-holder rule is for.", show.out)
	}
	if strings.Contains(show.out, "held by backend") {
		t.Errorf("the previous holder still holds the key:\n%s", show.out)
	}
}

// TestASecondLockFromAShellExtendsTheOneItAlreadyHolds.
//
// Two shells are ONE holder: the events name no actor, so lockHolder falls back to
// the source and both fold to "human". The log cannot tell them apart, so the
// refusal above is about somebody ELSE's lock and this case must not reach it.
//
// It is an extension rather than a second row because acquireLock replaces the
// expiry when the holder matches -- which is also how a member whose turn outlasts
// its lease keeps the key: it re-acquires. The word changes because "took" would
// hide that the caller already had it, and no release is written because there is
// nothing to reclaim from.
func TestASecondLockFromAShellExtendsTheOneItAlreadyHolds(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchSomethingElse, "", true)

	if first := arxi(t, dir, "state", "lock", "r1", "migrations/"); first.code != 0 {
		t.Fatalf("the first acquire exited %d:\n%s", first.code, first.out)
	}
	got := arxi(t, dir, "state", "lock", "r1", "migrations/", "--ttl", "1h")
	if got.code != 0 {
		t.Fatalf("re-locking a key this shell holds exited %d, want 0:\n%s\n"+
			"  consequence: a member whose work outlasts its lease has no way to keep the "+
			"key, so the lock lapses under it and is stolen mid-edit.", got.code, got.out)
	}
	if !strings.Contains(got.out, "extended migrations/") {
		t.Errorf("the renewal is not called an extension:\n%s\n"+
			"  consequence: \"took\" reads as a fresh claim, so a caller who has lost "+
			"track of its own lock believes it just won a race it never entered.",
			got.out)
	}

	if log := stateLog(t, dir, "r1"); strings.Contains(log, "lock.released") {
		t.Errorf("extending a lock released it first:\n%s\n"+
			"  consequence: there is a seq between the two at which the key reads as "+
			"free, and the reducer needs no release to move an expiry it owns.", log)
	}
	if n := strings.Count(stateLog(t, dir, "r1"), "lock.acquired"); n != 2 {
		t.Errorf("the log holds %d acquires, want 2 -- the first claim and the renewal", n)
	}
	if show := arxi(t, dir, "run", "show", "r1"); !strings.Contains(show.out, "locks (1)") {
		t.Errorf("the renewal did not fold to one lock:\n%s\n"+
			"  consequence: the holder appears twice, and one release frees only whichever "+
			"row the fold happens to have listed first.", show.out)
	}
}

// TestStateLockDrivesTheWatcherItWakes is the effect that lives only in Decide's
// return value, and its absence is invisible from this command's own output.
//
// wakeWatchers returns SpawnTurn, which exists for as long as the call. A command
// that appends the acquire and exits leaves a log in which a watcher on lock.*
// matched and nothing happened -- printing a seq and exiting 0 the whole way. The
// one observable difference is whether events appear AFTER the lock.acquired.
func TestStateLockDrivesTheWatcherItWakes(t *testing.T) {
	dir := t.TempDir()
	// simulated: true, so the turn is taken by the fake executor. Without it this
	// test would call a model and charge for it.
	emitRunAt(t, dir, "r1", watchLock, "", true)

	got := arxi(t, dir, "state", "lock", "r1", "migrations/")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.out)
	}
	if !strings.Contains(got.out, "opens a turn for security now") {
		t.Errorf("the output does not say a turn is being opened:\n%s\n"+
			"  consequence: this acquire and one nothing watches both exit 0, and only "+
			"this one is about to spend money.", got.out)
	}

	// The proof is in the log rather than in the message: the watcher's agent has to
	// have been activated, and after the claim rather than before it.
	log := stateLog(t, dir, "r1")
	if !strings.Contains(log, "agent.activated") {
		t.Errorf("no turn was ever opened:\n%s\n"+
			"  consequence: the acquire recorded the cause and discarded the effect, so "+
			"the run holds a matched watcher that provably never fired -- and every "+
			"line the command printed said otherwise.", log)
	}
	if strings.Index(log, "lock.acquired") > strings.Index(log, "agent.activated") {
		t.Errorf("the activation precedes the claim that caused it:\n%s\n"+
			"  consequence: the log's order is its causality, so a reader attributes "+
			"this turn to something else entirely.", log)
	}
}

// TestALockIsStillTakenInAPausedRunAndSaysTheTurnIsParked is the asymmetry with
// `event emit`, which refuses exactly this.
//
// An emit into a halted run whose watcher matches is pure loss: being heard is the
// event's only purpose. An acquire has a second purpose that lands whatever the
// status -- the key is held, `run show` says so, the next claimant is refused, and
// whoever unpauses inherits that. Refusing would make a person unpause a run, which
// resumes spending, in order to claim the files they are about to edit by hand.
//
// What must not happen is claiming the turn started. spawnCauses parks the cause,
// and this line is the only thing that can say so.
func TestALockIsStillTakenInAPausedRunAndSaysTheTurnIsParked(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchLock, pausedAt3, true)

	got := arxi(t, dir, "state", "lock", "r1", "migrations/")
	if got.code != 0 {
		t.Fatalf("taking a lock in a paused run exited %d, want 0:\n%s\n"+
			"  consequence: paused is when a person is most likely to be claiming what "+
			"they are about to edit themselves, and refusing here makes them unpause -- "+
			"which resumes spending -- to do it.", got.code, got.out)
	}
	if !strings.Contains(got.out, "parked") {
		t.Errorf("the parked turn is not mentioned:\n%s\n"+
			"  consequence: this and an acquire on a live run print the same way, so the "+
			"user waits for a member that will not move until the halt clears.", got.out)
	}
	if !strings.Contains(got.out, "arxi run unpause r1") {
		t.Errorf("the output does not name the command that clears the halt:\n%s\n"+
			"  consequence: paused is the one halted status the user themselves caused, "+
			"so the remedy is one command and it should be on screen.", got.out)
	}

	// Held, which is the half that justifies not refusing.
	show := arxi(t, dir, "run", "show", "r1")
	if !strings.Contains(show.out, "migrations/  held by human") {
		t.Errorf("the key is not held in the paused run:\n%s\n"+
			"  consequence: the acquire's standalone value is that the fold lists it and "+
			"the next claimant is refused, and that is what a halt must not cost.",
			show.out)
	}
	// And not driven: taking a lock must not resume a run somebody paused.
	if log := stateLog(t, dir, "r1"); strings.Contains(log, "agent.activated") {
		t.Errorf("a paused run was driven by an acquire:\n%s\n"+
			"  consequence: `state lock` becomes a way to resume spending, which is what "+
			"`run unpause` exists to be asked for explicitly.", log)
	}
}

// TestALockIsRefusedWhenAHaltedRunWouldRunAToolForIt is the one exception to the
// test above, and the reason is a difference in the reducer rather than in manners.
//
// A notify or activate cause is parked into Member.PendingCauses, which is part of
// State: it survives in the fold and drainParked hands it back when the halt clears.
// A CallTool effect is returned by wakeWatchers UNCONDITIONALLY -- spawnCauses never
// sees it, so nothing parks it -- and no later drive re-decides this event, because
// the next drive folds everything below its cursor into a starting state and keeps no
// effects from it. The call is dropped in silence.
//
// Refusing costs nothing here: nothing has been written, so the key stands as it did,
// and clearing the halt and repeating the command is a whole recovery.
func TestALockIsRefusedWhenAHaltedRunWouldRunAToolForIt(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchLockRunsATool, pausedAt3, true)

	got := arxi(t, dir, "state", "lock", "r1", "migrations/", "--ttl", "1h")
	if got.code != 1 {
		t.Fatalf("a halted run whose watcher runs a tool exited %d, want 1:\n%s\n"+
			"  consequence: the key is claimed and the tool call is discarded by the next "+
			"fold, so the run holds a matched run_tool watcher that provably never ran -- "+
			"and exit 0 said the opposite.", got.code, got.out)
	}
	for _, want := range []string{
		"read for security", "nothing was written", "arxi run unpause r1",
		// The lease the caller chose is carried into the suggestion, not dropped: the
		// second half of the recovery is the command just refused, and a retry that
		// silently falls back to the default grants a lock they did not ask for.
		"arxi state lock r1 migrations/ --ttl 1h0m0s",
	} {
		if !strings.Contains(got.out, want) {
			t.Errorf("the refusal does not say %q:\n%s\n"+
				"  consequence: the recovery is two commands in order, and the user has to "+
				"be told which of their watchers is a run_tool before they can decide "+
				"whether clearing the halt is what they want.", want, got.out)
		}
	}

	if log := stateLog(t, dir, "r1"); strings.Contains(log, "lock.acquired") {
		t.Errorf("the refused acquire reached the log anyway:\n%s\n"+
			"  consequence: the message says nothing was written, so the user does not "+
			"repeat it after unpausing -- and the key is held with its watcher unfired.",
			log)
	}
}

// TestALockIsRefusedInATerminalRunWhereItWouldHoldNothing.
//
// Decide returns before its switch on a terminal status, so the acquire arm is never
// reached: the event would be appended to the log and be absent from every fold of
// it. `run show` would list no lock on the key, so the next claimant would be told
// it is free -- by a command that had just reported taking it.
//
// The refusal also says that nothing needs a lock in a finished run, because "it is
// final" on its own reads as a rule to look for a flag around.
func TestALockIsRefusedInATerminalRunWhereItWouldHoldNothing(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchLock,
		`{"id":"e3","seq":3,"type":"stage.submitted","actor":"backend","payload":{"agent":"backend","stage":"execute"}}
{"id":"e4","seq":4,"type":"run.result","payload":{"summary":"done"}}
`, true)

	got := arxi(t, dir, "state", "lock", "r1", "migrations/")
	if got.code != 1 {
		t.Fatalf("taking a lock in a terminal run exited %d, want 1:\n%s\n"+
			"  consequence: the acquire is in the log and in no fold of it, so the key "+
			"the command reported holding is free to the next claimant that asks.",
			got.code, got.out)
	}
	for _, want := range []string{
		"which is final", "hold nothing", "no member left to edit",
	} {
		if !strings.Contains(got.out, want) {
			t.Errorf("the refusal does not say %q:\n%s\n"+
				"  consequence: a user who reads this as a policy goes looking for the "+
				"flag that overrides it; what is true is that the lock would guard nothing "+
				"and that nothing is left to guard against.", want, got.out)
		}
	}

	if log := stateLog(t, dir, "r1"); strings.Contains(log, "lock.acquired") {
		t.Errorf("the refused acquire reached the log anyway:\n%s\n"+
			"  consequence: this is exactly the unobservable row the refusal exists to "+
			"keep out of the log.", log)
	}
}
