package main

import (
	"fmt"
	"strings"
	"testing"
)

// `arxi state unlock`, exercised as a process against real run directories.
//
// The reducer's release arm is `releaseLock(&out, e.Str("key"))` and nothing
// more: it does not check the holder, deliberately, so that a key claimed by an
// agent that crashed mid-turn can be reclaimed at all. Every question about who
// is ENTITLED to release a lock is therefore answered here, in the CLI, and the
// answer is written into the log as a `reason`. That makes the defects worth
// guarding a different set from `state lock`'s:
//
//   - a release recorded under the wrong reason. The log is the only record of
//     whether a key was handed back, reclaimed from a lapsed lease or taken from
//     a holder still working, and `event log --type 'lock.*'` is where somebody
//     asks. A wrong reason there is not a stale display, it is a permanent
//     misstatement of what happened;
//   - `expired_at` on a forced release, which would record a lapse that nobody
//     judged and no clock supports -- the exact judgement --force exists to say
//     was not made;
//   - a refusal that still appends. A release the message said did not happen
//     frees the key in every later fold, which is worse than the collision the
//     lock exists to prevent, because the log then contradicts the terminal;
//   - the wrong exit code. 3 means "ask again later" across this binary, and
//     nothing here ever becomes possible by waiting;
//   - claiming the member waiting on the key is now free to move. It is not, and
//     that is the one thing this command must not imply.
//
// # Fixtures are shared with statelock_cli_test.go on purpose
//
// liveBackendLock, lapsedBackendLock and eternalBackendLock are the three shapes
// of somebody else's lock, and they are the same three this command arbitrates
// over -- one refused, one reclaimed, one forced. A private copy here would drift
// from the file that documents what each shape means.
//
// The clock is not pinned, for the reason that file gives: these tests exec the
// binary as a subprocess, so nothing in this process reaches nowFunc. An expiry in
// 2020 is lapsed at every real now and one in 2099 is live for longer than this
// code will run.

// ownLiveLock is a lock held by a shell, which is what "our own" means here.
//
// It carries `source: human` and NO actor, because that is how lockHolder reads a
// lock this binary took: Actor first, falling back to the source. Written by hand
// rather than by running `arxi state lock`, per emit_cli_test.go -- a fixture built
// with the code under test cannot distinguish a bug in the fixture from a bug in
// the thing being measured.
//
// The turn_done at seq 5 is the same one the three locks in statelock_cli_test.go
// carry, and for the reason given there: the acquire matches a lock.* watcher, so
// it commissions security's turn, and a fixture that leaves it open answers every
// release with "queued: security is mid-turn".
const ownLiveLock = `{"id":"e4","seq":4,"type":"lock.acquired","source":"human","payload":{"key":"migrations/","expires_at":"2099-01-01T00:00:00Z"}}
{"id":"e5","seq":5,"type":"agent.turn_done","actor":"security"}
`

// The paused pairs put the halt AFTER the lock, at seq 6.
//
// pausedAt4 cannot be composed with the lock fixtures: it is seq 4 and so is every
// acquire, and a log with two seq 4 rows is not one this binary would write --
// logstore would refuse the second. Built by hand so the sequence stays honest.
const ownLiveLockThenPaused = ownLiveLock + `{"id":"e6","seq":6,"type":"run.paused","payload":{}}
`

const liveBackendLockThenPaused = liveBackendLock + `{"id":"e6","seq":6,"type":"run.paused","payload":{}}
`

// blockedOnMigrations is the member this command must not claim to have freed.
//
// `blocked_on: lock` and `blocked_ref: {key: ...}` are what applyBlocked copies into
// Member.Detail and Member.BlockedOn, and they are exactly the two fields lockWaiter
// gates on. `security` needs no prior activation -- members are seeded from the
// blueprint at run.started -- and agent.blocked does not halt the run, which is
// budget.exceeded's job, so the run below is running and the outlook this drives is
// the ordinary one.
const blockedOnMigrations = `{"id":"e6","seq":6,"type":"agent.blocked","actor":"security","payload":{"blocked_on":"lock","blocked_ref":{"key":"migrations/"}}}
`

// TestStateUnlockRefusesAKeyThatCouldNotMatchTheLockItNames.
//
// The same exactness rule as `state lock`, with the failure running the other way. A
// padded key matches no lock, so the release would be refused as UNHELD while the key
// the caller meant is held -- and the message they would get names a lock that is not
// there. checkUnlockKey's wording is what stops that reading: it says the padded key
// is a different key and prints the one they probably meant, quoted, because the two
// are indistinguishable unquoted.
//
// The empty key is the worse half and is the reason this is a refusal rather than a
// trim: releaseLock drops a lock.released with no key, so the event would land in the
// log, free nothing, and be reported as a release. The lock in the fixture makes that
// concrete -- there is a real `migrations/` here for the caller to believe they freed.
func TestStateUnlockRefusesAKeyThatCouldNotMatchTheLockItNames(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchLock, eternalBackendLock, true)

	for _, tc := range []struct{ key, says string }{
		{"", "which key?"},
		{"  ", "which key?"},
		{"migrations/ ", "did you mean"},
		{"migrations/\n", "did you mean"},
	} {
		got := arxi(t, dir, "state", "unlock", "r1", tc.key)
		if got.code != 2 {
			t.Errorf("releasing %q exited %d, want 2:\n%s\n"+
				"  consequence: an empty key writes a release that frees nothing and says "+
				"it did; a padded one is refused as unheld while the key the caller meant "+
				"is still held, and they go looking for a release that never happened.",
				tc.key, got.code, got.out)
		}
		if !strings.Contains(got.out, tc.says) {
			t.Errorf("the refusal of %q does not say %q:\n%s\n"+
				"  consequence: %q and %q print identically, so a refusal that does not "+
				"quote them reads as this command failing to find a lock that is right "+
				"there.", tc.key, tc.says, got.out, tc.key, strings.TrimSpace(tc.key))
		}
		if !strings.Contains(got.out, "arxi state unlock") {
			t.Errorf("the refusal of %q does not print the usage:\n%s", tc.key, got.out)
		}
	}

	if log := stateLog(t, dir, "r1"); strings.Contains(log, "lock.released") {
		t.Errorf("a refused release reached the log anyway:\n%s\n"+
			"  consequence: the fold applies it, so migrations/ is free to every later "+
			"reader while the terminal said the command was refused -- the log and the "+
			"screen disagree about who may edit.", log)
	}
}

// TestReleasingOurOwnLockIsRecordedWithoutCeremony is the ordinary case, and the
// assertions are about the payload rather than the prose.
//
// `reason: released` is the only one of the three that claims nothing about a clock or
// about somebody losing a key, so it is the one that must not carry expired_at: a
// release of our own lease that recorded an expiry would tell every later reader the
// lock had run out, when what happened is that the holder handed it back.
//
// SourceHuman and no actor, per lockEvent. Runtime would be the natural-looking choice
// -- bookkeeping, like the steal in `state lock` -- and it would be wrong here:
// wakeWatchers is skipped outright for runtime, so a member declared to watch lock.*
// would wait for a key that is already free.
func TestReleasingOurOwnLockIsRecordedWithoutCeremony(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchSomethingElse, ownLiveLock, true)

	got := arxi(t, dir, "state", "unlock", "r1", "migrations/")
	if got.code != 0 {
		t.Fatalf("releasing our own lock exited %d, want 0:\n%s\n"+
			"  consequence: the one release that needs no judgement is the one a shell "+
			"cannot perform, so a key claimed by hand is held until the run ends.",
			got.code, got.out)
	}
	// seq 6: the fixture runs to seq 5, so the release this command appends is the
	// sixth event. Spelled out rather than computed, because a seq the test derives
	// from the log it is checking would agree with any number the binary printed.
	for _, want := range []string{"released migrations/ (seq 6)", "it was held by human"} {
		if !strings.Contains(got.out, want) {
			t.Errorf("the release does not print %q:\n%s\n"+
				"  consequence: the seq is what `event show` is given to check this against "+
				"the log, and the holder is the only confirmation the caller freed the lock "+
				"they meant rather than one of somebody else's.", want, got.out)
		}
	}

	rel := eventOfType(t, dir, "r1", "lock.released")
	if rel["source"] != "human" || rel["actor"] != nil {
		t.Errorf("the release is source %v actor %v, want human and no actor:\n%v\n"+
			"  consequence: runtime skips wakeWatchers entirely, so a member watching "+
			"lock.* is never told the key came free; an actor switches off that member's "+
			"own watcher and makes the fold say a member released a lock a shell released.",
			rel["source"], rel["actor"], rel)
	}
	pay, _ := rel["payload"].(map[string]any)
	for k, want := range map[string]any{
		"key": "migrations/", "reason": "released", "previous_holder": "human",
	} {
		if pay[k] != want {
			t.Errorf("the release records %s = %v, want %v:\n%v\n"+
				"  consequence: the reason is what `event log --type lock.*` is read for, "+
				"and this one says the holder handed the key back. Recorded as expired or "+
				"forced it accuses somebody of losing a lease they gave up.",
				k, pay[k], want, pay)
		}
	}
	if _, ok := pay["expired_at"]; ok {
		t.Errorf("a voluntary release recorded an expiry: %v\n"+
			"  consequence: it reads as a lapse nobody judged, and the lease in this "+
			"fixture runs to 2099 -- so the log would state an instant that has not "+
			"arrived as the moment the lock ran out.", pay)
	}

	if n := strings.Count(stateLog(t, dir, "r1"), "lock.released"); n != 1 {
		t.Errorf("the log holds %d releases, want 1", n)
	}
	if show := arxi(t, dir, "run", "show", "r1"); strings.Contains(show.out, "locks (") {
		t.Errorf("the key is still held after the release:\n%s\n"+
			"  consequence: the release exited 0 and the fold disagrees, which is the one "+
			"failure a caller cannot detect from this command's own output.", show.out)
	}
}

// TestForceOnOurOwnLockIsAllowedAndSaysItWasUnnecessary.
//
// A caller who has read `run show`, seen a holder and reached for --force is being
// careful, and refusing them would be a lesson in this binary's own bookkeeping: the
// holder they saw is "human", which is what every shell's lock folds to, so it was
// theirs all along. Accepting it costs nothing.
//
// What the flag must not do is change what is written. --force means "end a lease that
// had not run out", and there is no lease being ended here -- so `reason` stays
// "released", and the note explaining that goes to stderr, where it reaches a caller
// running `arxi state unlock r1 k >/dev/null` and cannot be mistaken for part of the
// result.
func TestForceOnOurOwnLockIsAllowedAndSaysItWasUnnecessary(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchSomethingElse, ownLiveLock, true)

	out, errOut, code := arxiStreams(t, dir, "state", "unlock", "r1", "migrations/", "--force")
	if code != 0 {
		t.Fatalf("--force on our own lock exited %d, want 0:\n%s%s\n"+
			"  consequence: the careful caller is refused for being careful, and the fix "+
			"they are sent to find is that the holder printed by `run show` was themselves.",
			code, out, errOut)
	}
	if !strings.Contains(errOut, "--force was not needed") ||
		!strings.Contains(errOut, "breaks nobody's lease") {
		t.Errorf("the unnecessary --force is not called out on stderr:\n%s\n"+
			"  consequence: the caller walks away believing they broke another member's "+
			"lease, which is something false about the run -- and the next thing they do "+
			"is go looking for whoever they interrupted.", errOut)
	}

	pay, _ := eventOfType(t, dir, "r1", "lock.released")["payload"].(map[string]any)
	if pay["reason"] != "released" {
		t.Errorf("the release records reason %v, want released:\n%v\n"+
			"  consequence: \"forced\" is a permanent claim that a live lease was ended "+
			"over its holder's head, and here there was no other holder -- so a flag the "+
			"note itself calls unnecessary would have rewritten what happened.",
			pay["reason"], pay)
	}
	if strings.Contains(out, "had not run out") {
		t.Errorf("stdout describes a lease that was ended:\n%s\n"+
			"  consequence: the two sentences contradict each other -- stderr says the "+
			"flag was not needed and stdout says a lease was cut short.", out)
	}
}

// TestALapsedForeignLeaseIsReleasedWithoutForceAndCarriesTheEvidence.
//
// The lease is what makes a foreign lock reclaimable without a decision: past the
// instant its holder was promised, taking it back is arithmetic rather than a
// judgement, so no flag is asked for. `state lock` already steals such a lease to take
// the key; this is the same reclaim for somebody who wants the key FREE rather than
// theirs -- unblocking a run they are not about to edit.
//
// expired_at is the evidence and it is load-bearing. The fold has no clock, so it
// cannot re-derive that the lease had run out; the instant goes into the payload so the
// next fold reaches the same conclusion without one. Its absence would not be a display
// bug, it would be a release whose reason no later reader can check.
//
// SourceHuman, where the steal in `state lock` is SourceRuntime, and the asymmetry is
// deliberate: there the batched lock.acquired carries the wake, so the release must not
// wake anything twice. Here the release is the only event, so runtime would mean no
// watcher on lock.* ever hears that the key came free.
func TestALapsedForeignLeaseIsReleasedWithoutForceAndCarriesTheEvidence(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchSomethingElse, lapsedBackendLock, true)

	got := arxi(t, dir, "state", "unlock", "r1", "migrations/")
	if got.code != 0 {
		t.Fatalf("releasing a lapsed lease exited %d, want 0:\n%s\n"+
			"  consequence: a key whose holder crashed mid-turn needs --force to free, so "+
			"the flag that means \"end a live lease over its holder's head\" becomes the "+
			"flag everybody types, and it stops meaning anything.", got.code, got.out)
	}
	for _, want := range []string{
		"taken from backend, whose lease lapsed at 2020-01-01T00:00:00Z",
		`recorded as "expired"`,
	} {
		if !strings.Contains(got.out, want) {
			t.Errorf("the release does not print %q:\n%s\n"+
				"  consequence: this and a forced release both free the key and exit 0, and "+
				"only one of them took it from a holder that may still be working.",
				want, got.out)
		}
	}

	rel := eventOfType(t, dir, "r1", "lock.released")
	if rel["source"] != "human" {
		t.Errorf("the release is source %v, want human:\n%v\n"+
			"  consequence: wakeWatchers is skipped for runtime, and this release is the "+
			"only event written -- so a member declared to watch lock.* waits for a key "+
			"that came free without it.", rel["source"], rel)
	}
	pay, _ := rel["payload"].(map[string]any)
	for k, want := range map[string]any{
		"key": "migrations/", "reason": "expired", "previous_holder": "backend",
		"expired_at": "2020-01-01T00:00:00Z",
	} {
		if pay[k] != want {
			t.Errorf("the release records %s = %v, want %v:\n%v\n"+
				"  consequence: without the instant it lapsed, \"expired\" is a claim the "+
				"log cannot support -- the fold has no clock, so no later reader can check "+
				"that this key was reclaimed rather than taken.", k, pay[k], want, pay)
		}
	}
}

// TestALiveForeignLeaseIsRefusedAndPricesTheWait.
//
// The holder was promised the key until an instant that has not arrived, and it may be
// editing what the key guards right now. So this is the one arm that refuses, and the
// refusal has to hand the caller both ways forward -- the flag, and the clock -- because
// each is right for a different caller: somebody whose colleague went home needs the
// flag, and somebody who arrived four minutes early needs to be told so.
//
// Exit 1 and not exitLockHeld, which is the tempting one here because waiting really
// would work. 3 means the same command succeeds later, unchanged, and this one does not:
// past the expiry the arbitration lands on "expired", which writes a different reason
// into the log. A caller polling this is hoping somebody else's work finishes, and
// `state lock` is the command that waits.
//
// Nothing on stdout, per `state lock`: a script reads stdout for what it got, and a
// refusal there is one a pipeline treats as a release.
func TestALiveForeignLeaseIsRefusedAndPricesTheWait(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchLock, liveBackendLock, true)

	out, errOut, code := arxiStreams(t, dir, "state", "unlock", "r1", "migrations/")
	if code != 1 {
		t.Fatalf("a live foreign lease exited %d, want 1:\n%s%s\n"+
			"  consequence: 0 would take a key from a holder that may be mid-edit, and %d "+
			"would tell a script to wait -- but the command that succeeds after the expiry "+
			"records \"expired\", which is a different answer than the one being polled for.",
			code, out, errOut, exitLockHeld)
	}
	if out != "" {
		t.Errorf("the refusal wrote to stdout:\n%s\n"+
			"  consequence: a pipeline reads stdout for the release it asked for.", out)
	}
	for _, want := range []string{
		"is held by backend until 2099-01-01T00:00:00Z",
		"from now",
		"arxi state unlock r1 migrations/ --force",
		"or wait:",
		`reason "expired"`,
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("the refusal does not say %q:\n%s\n"+
				"  consequence: the caller is told no and given neither of the two things "+
				"that follow -- how long the lease has left, and the flag that ends it "+
				"anyway. One of those is right for them, and only this refusal knows "+
				"which facts decide it.", want, errOut)
		}
	}

	if log := stateLog(t, dir, "r1"); strings.Contains(log, "lock.released") {
		t.Errorf("the refusal released the lock anyway:\n%s\n"+
			"  consequence: the key is taken from a holder still working under it and the "+
			"terminal said it was not, so nobody knows to re-take it -- the collision the "+
			"lock exists to prevent, reached through a message that denied it.", log)
	}
}

// TestALockThatCannotLapseIsTheStallForceExistsFor.
//
// A lock with no expiry is the failure docs/design/20-use-cases.md §20.8 names: nothing
// reclaims it, because every reclaim in this tree is a lease judged to have run out and
// there is no lease. `state lock` refuses it, `state lock` on the key never steals it,
// and before this verb existed the run stalled until somebody edited a log by hand.
//
// So this refusal is the same shape as the one above and means something different, and
// the difference is the sentence that must be there: waiting costs nothing here because
// there is no instant at which the key frees itself. A refusal that only said "held by
// backend" would send the caller away to poll something that will never change.
//
// It also names what --force writes, quoted, before the caller types it. The flag ends
// somebody's claim, and the case for typing it has to be made before the fact: the log
// records whose lease ended and that a decision rather than a clock ended it. It does
// not record whose decision -- see the release arm of cmdStateUnlock -- so the refusal
// promises previous_holder and nothing more.
func TestALockThatCannotLapseIsTheStallForceExistsFor(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchLock, eternalBackendLock, true)

	out, errOut, code := arxiStreams(t, dir, "state", "unlock", "r1", "migrations/")
	if code != 1 {
		t.Fatalf("a lock with no expiry exited %d, want 1:\n%s%s\n"+
			"  consequence: %d is \"ask again later\", and this is the one lock for which "+
			"later never comes.", code, out, errOut, exitLockHeld)
	}
	for _, want := range []string{
		"with no expiry",
		"§20.8",
		"nothing is lost by waiting instead",
		"arxi state unlock r1 migrations/ --force",
		`previous_holder "backend"`,
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("the refusal does not say %q:\n%s\n"+
				"  consequence: the caller cannot tell this from a lease about to run out, "+
				"so they wait -- and waiting is the one thing that cannot work here. --force "+
				"is the answer, and it is one somebody should read the consequences of "+
				"before typing.", want, errOut)
		}
	}

	if log := stateLog(t, dir, "r1"); strings.Contains(log, "lock.released") {
		t.Errorf("the refusal released the lock anyway:\n%s\n"+
			"  consequence: --force is the whole point of this arm, and a refusal that "+
			"frees the key regardless means the flag decides nothing.", log)
	}
}

// TestForcingALiveLeaseRecordsWhoLostItAndClaimsNoLapse is the assertion this whole
// file is arranged around.
//
// A forced release and an expired one free the same key and print the same seq. The
// difference lives entirely in the payload, and it is the difference between "its lease
// ran out" and "somebody decided": the second is a claim about a person and the log is
// the only place it is recorded. `event log --type 'lock.*'` is where the member that
// lost the key finds out which happened.
//
// WhoLostIt, not who ended it, and the name is precise because the older one was not.
// previous_holder is the only name in the payload; Actor is empty by lockEvent's design,
// so nothing here identifies the caller and this test never asserted that it did. A
// reader who takes the old name at face value goes looking for an accountability field,
// finds source: human, and concludes it names somebody.
//
// So expired_at must be ABSENT. The instant is right there in held.ExpiresAt and writing
// it would be one line -- and it would record a lapse at an instant that has not
// arrived, which is exactly the judgement --force exists to say nobody made. That is the
// only defect in this command that no message on screen could reveal: both releases
// print correctly and only the log is wrong, permanently.
func TestForcingALiveLeaseRecordsWhoLostItAndClaimsNoLapse(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchSomethingElse, liveBackendLock, true)

	got := arxi(t, dir, "state", "unlock", "r1", "migrations/", "--force")
	if got.code != 0 {
		t.Fatalf("--force on a live foreign lease exited %d, want 0:\n%s\n"+
			"  consequence: a lease only its own holder can end means every agent lock "+
			"runs to expiry, and a lock with no expiry runs to the end of the run -- the "+
			"§20.8 stall, with no way out of it.", got.code, got.out)
	}
	for _, want := range []string{
		"taken from backend, whose lease ran to 2099-01-01T00:00:00Z and had not run out",
		`recorded as "forced"`,
	} {
		if !strings.Contains(got.out, want) {
			t.Errorf("the release does not print %q:\n%s\n"+
				"  consequence: the caller is not told they ended a lease that was still "+
				"running, so the one release worth mentioning to its previous holder looks "+
				"like the routine one.", want, got.out)
		}
	}

	pay, _ := eventOfType(t, dir, "r1", "lock.released")["payload"].(map[string]any)
	for k, want := range map[string]any{
		"key": "migrations/", "reason": "forced", "previous_holder": "backend",
	} {
		if pay[k] != want {
			t.Errorf("the release records %s = %v, want %v:\n%v\n"+
				"  consequence: previous_holder is how the member that lost the key learns "+
				"it was taken, and \"forced\" is the only record that a live lease was ended "+
				"by a decision rather than by a clock.", k, pay[k], want, pay)
		}
	}
	if _, ok := pay["expired_at"]; ok {
		t.Errorf("a forced release recorded an expiry: %v\n"+
			"  consequence: it records a lapse at an instant in 2099 that has not arrived "+
			"and no clock supports -- the exact judgement --force exists to say nobody "+
			"made. Every later reader is told the lease ran out on its own, and the log is "+
			"append-only, so nothing corrects it.", pay)
	}
}

// TestAnUnheldKeyIsRefusedWithTheCodeNoPollerWaitsOn covers both branches of the
// refusal, in two runs, because they are two messages with one exit code and the code
// is what a script acts on.
//
// A release of a key nobody holds is almost always a typo or padding, so the refusal
// lists what IS held, quoted -- an invisible character is exactly what a listing has to
// make visible, and "no lock on migrations/" while `run show` displays migrations/ reads
// as a bug in this command rather than as a difference in the string.
//
// Exit 1 rather than exitLockHeld, on the same ground as the eternal lock: 3 means the
// same command works later, and an unheld key does not become held by waiting. The
// refusal says so, because a reader who knows 3 means "not yet" reads 1 as a defect.
func TestAnUnheldKeyIsRefusedWithTheCodeNoPollerWaitsOn(t *testing.T) {
	notYet := fmt.Sprintf("exit 1 and not %d", exitLockHeld)

	// A run holding one lock, asked for a different key.
	held := t.TempDir()
	emitRunAt(t, held, "r1", watchLock, eternalBackendLock, true)

	got := arxi(t, held, "state", "unlock", "r1", "docs/")
	if got.code != 1 {
		t.Fatalf("releasing an unheld key exited %d, want 1:\n%s\n"+
			"  consequence: 0 would report freeing a lock that was never there, and the "+
			"caller would stop looking for the key they actually meant.", got.code, got.out)
	}
	for _, want := range []string{
		"holds no lock on docs/",
		"it holds 1 lock:",
		`"migrations/" held by backend, no expiry`,
		"a lock is an exact string",
		notYet,
	} {
		if !strings.Contains(got.out, want) {
			t.Errorf("the refusal does not say %q:\n%s\n"+
				"  consequence: the caller is told a key is not held and not told which "+
				"ones are, so a release that failed on one character of padding is "+
				"indistinguishable from one that failed because the lock was already gone.",
				want, got.out)
		}
	}

	// And a run holding nothing, which is the branch that returns before the listing.
	// Separate because the two exits are written in different functions and a refusal
	// that exits 1 here and 3 there is one no script can act on.
	empty := t.TempDir()
	emitRunAt(t, empty, "r2", watchLock, "", true)

	none := arxi(t, empty, "state", "unlock", "r2", "migrations/")
	if none.code != 1 {
		t.Errorf("releasing a key in a run with no locks exited %d, want 1:\n%s",
			none.code, none.out)
	}
	for _, want := range []string{"it holds no locks at all", notYet} {
		if !strings.Contains(none.out, want) {
			t.Errorf("the empty-locks refusal does not say %q:\n%s\n"+
				"  consequence: the listing branch is skipped here, so this is the only "+
				"place the code is stated -- and \"no locks at all\" is what tells the "+
				"caller the release they are chasing has already happened.", want, none.out)
		}
	}
}

// TestStateUnlockRefusesATerminalRunWhereTheReleaseWouldFreeNothing.
//
// Decide returns before its switch once the status is terminal, so the release would be
// appended and the LockReleased arm never reached: the key would read as held in every
// fold of a log that contains its release. That is the worst disagreement in this file,
// because both halves are permanent -- the row is in the log forever and the lock is in
// the state forever.
//
// The refusal also says nothing is waiting on the key, which `state lock`'s equivalent
// has no reason to. Handing a key back is something people do to unblock a member, and
// in a finished run there is no member left to unblock -- so "it is final" alone would
// leave the caller hunting for the flag that overrides it.
func TestStateUnlockRefusesATerminalRunWhereTheReleaseWouldFreeNothing(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchLock, ownLiveLock+
		`{"id":"e6","seq":6,"type":"stage.submitted","actor":"backend","payload":{"agent":"backend","stage":"execute"}}
{"id":"e7","seq":7,"type":"run.result","payload":{"summary":"done"}}
`, true)

	got := arxi(t, dir, "state", "unlock", "r1", "migrations/")
	if got.code != 1 {
		t.Fatalf("releasing a lock in a terminal run exited %d, want 1:\n%s\n"+
			"  consequence: the release is in the log and in no fold of it, so `run show` "+
			"lists migrations/ as held forever while the command reported freeing it.",
			got.code, got.out)
	}
	for _, want := range []string{
		"which is final",
		"would still list migrations/ as held",
		"no member left to take it",
	} {
		if !strings.Contains(got.out, want) {
			t.Errorf("the refusal does not say %q:\n%s\n"+
				"  consequence: a refusal that only says the run is final reads as a policy, "+
				"and the caller goes looking for the flag around it. What is true is that "+
				"the release would be unobservable and that nothing needs the key.",
				want, got.out)
		}
	}

	if log := stateLog(t, dir, "r1"); strings.Contains(log, "lock.released") {
		t.Errorf("the refused release reached the log anyway:\n%s\n"+
			"  consequence: this is precisely the row that can never be folded and can "+
			"never be removed -- a log that records a release the state does not have.",
			log)
	}
}

// TestALockIsStillReleasedInAPausedRunAndSaysTheTurnIsParked.
//
// A pause is not a refusal here, and that asymmetry with the terminal case is the whole
// point of the test. Releasing a key in a paused run is a coherent thing to want: the
// fold accepts it, the state changes, and the run resumes later with the key already
// free. Refusing it would leave the caller with a lock they cannot hand back until they
// unpause -- and unpausing to release a lock is the wrong order for anyone who paused
// deliberately.
//
// What must not happen is the release quietly implying a turn opens. The watcher on
// lock.* is real and matches, but a halted run is not driven, so the member the caller
// expects to move does not move. The command says so and names the one command that
// changes it.
func TestALockIsStillReleasedInAPausedRunAndSaysTheTurnIsParked(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchLock, ownLiveLockThenPaused, true)

	got := arxi(t, dir, "state", "unlock", "r1", "migrations/")
	if got.code != 0 {
		t.Fatalf("releasing our own lock in a paused run exited %d, want 0:\n%s\n"+
			"  consequence: the caller cannot hand a key back without unpausing first, "+
			"which is the opposite order from the one anybody who paused on purpose wants.",
			got.code, got.out)
	}
	for _, want := range []string{
		"freed, but run r1 is paused",
		"clear it: arxi run unpause r1",
	} {
		if !strings.Contains(got.out, want) {
			t.Errorf("the release does not say %q:\n%s\n"+
				"  consequence: the key is free and the watcher matched, so the caller has "+
				"every reason to expect a turn. Nothing runs until the pause is cleared, and "+
				"a release that does not say so reads as a member that hung.", want, got.out)
		}
	}

	log := stateLog(t, dir, "r1")
	if strings.Contains(log, "agent.activated") {
		t.Errorf("the paused run drove a watcher:\n%s\n"+
			"  consequence: a pause that still opens turns is not a pause, and the note "+
			"this command just printed is false.", log)
	}

	if show := arxi(t, dir, "run", "show", "r1"); strings.Contains(show.out, "locks (") {
		t.Errorf("run show still lists a locks section after the release:\n%s\n"+
			"  consequence: the release reported success and the state disagrees.", show.out)
	}
}

// TestAReleaseIsRefusedWhenAHaltedRunWouldRunAToolForIt.
//
// The test above releases in a paused run because its watcher only opens a turn. This one
// refuses, because the watcher spends: freeing the key would run `read` for security, and
// the run is halted precisely so that nothing spends. Doing it anyway would launder a
// tool call through a lock release, past the pause that was supposed to stop it.
//
// Both branches exist to pin the retry line, and it is the reason this test needs two
// runs. The suggestion has to carry --force exactly when the original invocation did:
// print it always and the caller is told to force a lease that is their own, print it
// never and the retry they were handed refuses the moment they unpause. Nothing else in
// the file distinguishes forceArg("released") from forceArg("forced").
func TestAReleaseIsRefusedWhenAHaltedRunWouldRunAToolForIt(t *testing.T) {
	own := t.TempDir()
	emitRunAt(t, own, "r1", watchLockRunsATool, ownLiveLockThenPaused, true)

	plain := arxi(t, own, "state", "unlock", "r1", "migrations/")
	if plain.code != 1 {
		t.Fatalf("releasing a lock a halted run would spend on exited %d, want 1:\n%s\n"+
			"  consequence: a pause that a lock release can spend past is not a pause.",
			plain.code, plain.out)
	}
	for _, want := range []string{
		"would run a tool: read for security",
		"nothing was written, so migrations/ is still held by human",
		"arxi run unpause r1",
		"arxi state unlock r1 migrations/",
	} {
		if !strings.Contains(plain.out, want) {
			t.Errorf("the halt refusal does not say %q:\n%s\n"+
				"  consequence: the caller has to learn three things -- what would have run, "+
				"that the key is still theirs, and the two commands that finish the job. A "+
				"refusal missing any of them reads as a lock that cannot be handed back.",
				want, plain.out)
		}
	}
	if strings.Contains(plain.out, "--force") {
		t.Errorf("the refusal suggests --force for a lock we already hold:\n%s\n"+
			"  consequence: --force means ending somebody else's live lease. Suggesting it "+
			"for our own teaches the flag as boilerplate, which is how it gets typed at a "+
			"lease that is not ours.", plain.out)
	}

	// The same refusal, reached with --force, must hand back a command that still
	// carries it: backend's lease runs to 2099, so the retry without --force would be
	// refused for a different reason the moment the run is unpaused.
	forced := t.TempDir()
	emitRunAt(t, forced, "r1", watchLockRunsATool, liveBackendLockThenPaused, true)

	steal := arxi(t, forced, "state", "unlock", "r1", "migrations/", "--force")
	if steal.code != 1 {
		t.Fatalf("forcing a release a halted run would spend on exited %d, want 1:\n%s\n"+
			"  consequence: --force overrides who holds the lease, not the halt. A run that "+
			"spends because a flag was passed has no pause at all.", steal.code, steal.out)
	}
	if want := "arxi state unlock r1 migrations/ --force"; !strings.Contains(steal.out, want) {
		t.Errorf("the refusal does not offer %q:\n%s\n"+
			"  consequence: the retry the caller was handed drops the flag that made the "+
			"release legal, so following the instructions verbatim earns a second refusal "+
			"-- and this one would say the lease is held until 2099.", want, steal.out)
	}

	for _, tc := range []struct{ name, dir string }{{"own", own}, {"forced", forced}} {
		if log := stateLog(t, tc.dir, "r1"); strings.Contains(log, "lock.released") {
			t.Errorf("the refused %s release was appended anyway:\n%s\n"+
				"  consequence: the message says nothing was written. If a row is there, the "+
				"key is free, the caller believes it is held, and the tool the halt was "+
				"protecting runs on the next drive.", tc.name, log)
		}
	}
}

// TestAReleaseDrivesTheWatcherItWakes.
//
// The release is SourceHuman rather than SourceRuntime for exactly this reason, and it is
// the one place the choice is observable: wakeWatchers is skipped outright for
// SourceRuntime, so a release recorded as runtime news would free the key and leave the
// member who declared `pattern: lock.*` waiting on a lock that no longer exists. Nothing
// would ever wake it -- the event that would have is the one already in the log.
//
// The order is asserted, not just the presence. An activation folded before the release
// is a turn opened while the key was still held, which is the race the lock is for.
func TestAReleaseDrivesTheWatcherItWakes(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchLock, ownLiveLock, true)

	got := arxi(t, dir, "state", "unlock", "r1", "migrations/")
	if got.code != 0 {
		t.Fatalf("releasing a watched key exited %d, want 0:\n%s", got.code, got.out)
	}
	if want := "opens a turn for security now"; !strings.Contains(got.out, want) {
		t.Errorf("the release does not say %q:\n%s\n"+
			"  consequence: handing a key back is done to let somebody else move. If the "+
			"command does not say who moved, the caller polls `run show` to find out what "+
			"their own command did.", want, got.out)
	}

	log := stateLog(t, dir, "r1")
	rel, act := strings.Index(log, "lock.released"), strings.Index(log, "agent.activated")
	if act < 0 {
		t.Fatalf("no agent.activated after releasing a key a watcher listens for:\n%s\n"+
			"  consequence: the member declared `pattern: lock.*` and the key came free. "+
			"Nothing else will tell it -- the release is the news, and it has been spent.",
			log)
	}
	if act < rel {
		t.Errorf("agent.activated is folded before lock.released:\n%s\n"+
			"  consequence: the turn opens at a seq where migrations/ still reads as held, "+
			"so the woken member sees the lock it was waiting on and blocks again.", log)
	}
}

// TestFreeingAKeyDoesNotWakeTheMemberWaitingOnIt pins the gap, not a feature.
//
// Decide's release arm is `releaseLock(&out, e.Str("key"))` and nothing else. AgentUnblocked
// is handled but no code path in the tree emits it, so a member blocked on migrations/ is
// still MemberWaiting after migrations/ is free. Without a watcher on lock.* nothing opens
// a turn for it either, and the run sits there with a free key and a waiting member.
//
// That is a real limitation and this test does not pretend otherwise -- it asserts the
// command SAYS so. The caller who releases a key is doing it to unstick security; letting
// them walk away believing it worked is the whole failure, and it is worse than the gap,
// because they will come back to a run that has not moved and look for the crash. When
// agent.unblocked is eventually wired, this test fails, and the message it fails with is
// the paragraph that has to be deleted from the outlook.
func TestFreeingAKeyDoesNotWakeTheMemberWaitingOnIt(t *testing.T) {
	dir := t.TempDir()
	emitRunAt(t, dir, "r1", watchSomethingElse, lapsedBackendLock+blockedOnMigrations, true)

	got := arxi(t, dir, "state", "unlock", "r1", "migrations/")
	if got.code != 0 {
		t.Fatalf("releasing a lapsed lease somebody is waiting on exited %d, want 0:\n%s\n"+
			"  consequence: this is the case the command exists for -- a holder that went "+
			"away and a member queued behind it.", got.code, got.out)
	}
	for _, want := range []string{
		"freed, and nobody was told",
		"security is blocked on migrations/ and is NOT woken by this",
		"what it is waiting for: arxi run why r1",
		"watchers: [{agent: <name>, pattern: lock.*}]",
	} {
		if !strings.Contains(got.out, want) {
			t.Errorf("the release does not say %q:\n%s\n"+
				"  consequence: the key is free, security is still waiting, and the command "+
				"reported success. The caller leaves believing the run is moving and comes "+
				"back to one that has not, with nothing on screen having been wrong.",
				want, got.out)
		}
	}

	log := stateLog(t, dir, "r1")
	for _, unexpected := range []string{"agent.unblocked", "agent.activated"} {
		if strings.Contains(log, unexpected) {
			t.Errorf("the release emitted %s:\n%s\n"+
				"  consequence: the sentence above is now false, and it is the sentence the "+
				"caller acts on. If this became true on purpose, delete the paragraph in "+
				"printStateUnlockOutlook that says the member stays waiting.", unexpected, log)
		}
	}

	if show := arxi(t, dir, "run", "show", "r1"); strings.Contains(show.out, "locks (") {
		t.Errorf("run show still lists a locks section after the release:\n%s\n"+
			"  consequence: the release reported success and the state disagrees -- and the "+
			"member waiting on the key would be right to keep waiting.", show.out)
	}
}
