package kernel

import (
	"encoding/json"
	"strings"
	"testing"
)

// ----------------------------------------------------------------- locks
//
// A cooperative lock is how two members avoid editing the same thing at once
// (docs/design/20-use-cases.md §20.8). These tests exist because the arm that
// takes one was an unconditional append for the entire life of the event type:
// nothing in the tree emitted lock.acquired, so nothing had ever folded two of
// them, and what came out of that fold was one key with two holders.

// TestALockHasOneHolder is the invariant the type exists for. With two holders in
// the state both members read the fold, both believe they may edit migrations/,
// and the coordination cost a turn to achieve nothing.
func TestALockHasOneHolder(t *testing.T) {
	c := bp()
	s := started(c)

	s, _ = Decide(s, ev(LockAcquired, "backend", map[string]any{
		"key": "migrations/", "expires_at": "2026-09-02T07:00:00Z"}), c)
	s, _ = Decide(s, ev(LockAcquired, "frontend", map[string]any{
		"key": "migrations/", "expires_at": "2026-09-02T09:00:00Z"}), c)

	if len(s.Locks) != 1 {
		t.Fatalf("two members acquiring one key left %d locks: %+v\n"+
			"  consequence: both of them see themselves holding migrations/, so the "+
			"lock has bought a turn's worth of coordination and delivered none of it.\n"+
			"  fix: acquireLock in decide.go", len(s.Locks), s.Locks)
	}
	if h := s.Locks[0].Holder; h != "backend" {
		t.Errorf("the key ended up held by %q; backend took it first.\n"+
			"  consequence: the later claimant wins, so a lock can be taken out from "+
			"under a member already editing the files it guards, and the fold then "+
			"contradicts work in flight.", h)
	}
	if x := s.Locks[0].ExpiresAt; x != "2026-09-02T07:00:00Z" {
		t.Errorf("the holder's expiry is %q, want the one it acquired with.\n"+
			"  consequence: a refused claimant extends the holder's lease, so the "+
			"lock outlives what its holder asked for.", x)
	}
}

// TestTheHolderExtendsItsOwnLock is the other half of "one holder": refusing a
// second acquire must not refuse the holder's own renewal. A long turn keeps its
// lock alive by re-acquiring, and there is no other way to say so.
func TestTheHolderExtendsItsOwnLock(t *testing.T) {
	c := bp()
	s := started(c)

	s, _ = Decide(s, ev(LockAcquired, "backend", map[string]any{
		"key": "migrations/", "expires_at": "2026-09-02T07:00:00Z"}), c)
	s, _ = Decide(s, ev(LockAcquired, "backend", map[string]any{
		"key": "migrations/", "expires_at": "2026-09-02T07:10:00Z"}), c)

	if len(s.Locks) != 1 {
		t.Fatalf("a holder renewing its own lock left %d rows: %+v\n"+
			"  consequence: the holder now appears twice, and one release frees only "+
			"what the fold happens to have listed first.", len(s.Locks), s.Locks)
	}
	if x := s.Locks[0].ExpiresAt; x != "2026-09-02T07:10:00Z" {
		t.Errorf("after the renewal the expiry is still %q.\n"+
			"  consequence: a renewal that does not move the expiry is not a renewal, "+
			"so a member whose turn outlasts its ttl has the lock lapse under itself "+
			"and stolen mid-edit -- by which point it is already writing the files.\n"+
			"  fix: acquireLock in decide.go", x)
	}
}

// TestALockTakenFromAShellIsHeldByItsSource pins the holder of a lock nobody
// signed for: `arxi state lock` leaves Actor empty on purpose, because
// wakeWatchers skips a watcher whose agent equals the actor and naming a member
// there would silently disable that member's own watcher on lock.*.
func TestALockTakenFromAShellIsHeldByItsSource(t *testing.T) {
	c := bp()
	s := started(c)

	e := ev(LockAcquired, "", map[string]any{"key": "migrations/"})
	e.Source = SourceHuman
	s, _ = Decide(s, e, c)

	if len(s.Locks) != 1 {
		t.Fatalf("an unsigned acquire folded to %+v, want the key held", s.Locks)
	}
	if h := s.Locks[0].Holder; h != "human" {
		t.Errorf("a lock taken from a shell is held by %q, want %q from its source.\n"+
			"  consequence: with the empty string there `arxi run show` prints "+
			"\"held by \" and `arxi run why` says `waits for the lock \"migrations/\" "+
			"held by ` -- the sentence stops exactly where the reader needs a name.\n"+
			"  fix: lockHolder in decide.go", h, SourceHuman)
	}
}

// TestALockWithNoKeyIsNotTaken drops the acquire the CLI refuses to write but an
// agent's tool call can still submit. Same judgement as StateSet and as the
// `if id == ""` in InboxItem: a row keyed on nothing is one no release can name.
func TestALockWithNoKeyIsNotTaken(t *testing.T) {
	c := bp()
	s := started(c)

	s, _ = Decide(s, ev(LockAcquired, "backend", map[string]any{
		"expires_at": "2026-09-02T07:00:00Z"}), c)

	if len(s.Locks) != 0 {
		t.Errorf("an acquire with no key stored %+v\n"+
			"  consequence: the state carries a lock whose key is \"\", which nothing "+
			"can release and every reader prints as a blank line, and the next such "+
			"event adds another.", s.Locks)
	}
}

// TestReleasingAKeyLetsTheNextHolderTakeIt walks the reclaim end to end, with the
// release stamped the way the CLI stamps the one it writes before stealing a
// lapsed lock: no actor, and not the holder. A reducer that honoured a release
// only from its holder would refuse exactly this, and the only way around it
// would be for the CLI to write an event claiming to be the crashed agent.
func TestReleasingAKeyLetsTheNextHolderTakeIt(t *testing.T) {
	c := bp()
	s := started(c)

	s, _ = Decide(s, ev(LockAcquired, "backend", map[string]any{
		"key": "migrations/", "expires_at": "2026-09-02T07:00:00Z"}), c)

	rel := ev(LockReleased, "", map[string]any{"key": "migrations/"})
	rel.Source = SourceRuntime
	s, _ = Decide(s, rel, c)

	if len(s.Locks) != 0 {
		t.Fatalf("the key is still held after a release by a non-holder: %+v\n"+
			"  consequence: a lock whose holder crashed mid-turn can never be "+
			"reclaimed, so the run stalls until a human forks it.", s.Locks)
	}

	s, _ = Decide(s, ev(LockAcquired, "frontend", map[string]any{
		"key": "migrations/", "expires_at": "2026-09-02T09:00:00Z"}), c)

	if len(s.Locks) != 1 || s.Locks[0].Holder != "frontend" {
		t.Errorf("after the release the fold holds %+v, want frontend holding the key.\n"+
			"  consequence: the key is unusable rather than free -- a release that "+
			"leaves something behind blocks the acquire it exists to permit.", s.Locks)
	}
}

// TestExtendingALockDoesNotWriteThroughToTheStateItWasHanded guards the Clone arm
// against a hazard a renewal introduced: it is the first thing in this reducer to
// mutate a Lock in place rather than append one.
func TestExtendingALockDoesNotWriteThroughToTheStateItWasHanded(t *testing.T) {
	c := bp()
	s := started(c)
	s, _ = Decide(s, ev(LockAcquired, "backend", map[string]any{
		"key": "migrations/", "expires_at": "2026-09-02T07:00:00Z"}), c)

	before := s
	_, _ = Decide(s, ev(LockAcquired, "backend", map[string]any{
		"key": "migrations/", "expires_at": "2026-09-02T09:00:00Z"}), c)

	if x := before.Locks[0].ExpiresAt; x != "2026-09-02T07:00:00Z" {
		t.Errorf("the state handed to Decide now reads %q, the expiry the NEXT event "+
			"carried.\n"+
			"  `out := s` copies a slice HEADER, so writing out.Locks[i] writes "+
			"s.Locks[i] too, and appending never showed it.\n"+
			"  consequence: the fold stops being a function of the events before it, so "+
			"a replay to seq N reports what seq N+1 did. It surfaces only where an "+
			"earlier state is kept -- a replay, an --at-seq, a golden.\n"+
			"  fix: the Locks arm in State.Clone", x)
	}
}

// TestTheReducerDoesNotExpireALockItself is a test that nothing happens, and the
// thing that must not happen is the reducer growing a clock.
func TestTheReducerDoesNotExpireALockItself(t *testing.T) {
	c := bp()
	s := started(c)
	s, _ = Decide(s, ev(LockAcquired, "backend", map[string]any{
		"key": "migrations/", "expires_at": "2020-01-01T00:00:00Z"}), c)

	if len(s.Locks) != 1 {
		t.Errorf("a lock whose expiry is long past folded to %+v, want it still held.\n"+
			"  dropping it here would make the same log answer differently in the "+
			"morning and in the afternoon.\n"+
			"  consequence: replay stops being reproducible, which is the property the "+
			"rest of the design is built on. Judging a lock lapsed is a reading and "+
			"needs a now, so `arxi state lock` records a lock.released and then "+
			"acquires -- which leaves the judgement in the log where the next fold "+
			"reproduces it without a clock.", s.Locks)
	}
}

// TestALockKeepsItsExpiryInTheSnapshot pins the wire name, because state.json is
// written by one command and read by the next: a field that does not survive that
// trip is a field the CLI can only see within a single process.
func TestALockKeepsItsExpiryInTheSnapshot(t *testing.T) {
	raw, err := json.Marshal(State{Locks: []Lock{
		{Key: "migrations/", Holder: "backend", ExpiresAt: "2026-09-02T07:00:00Z"}}})
	if err != nil {
		t.Fatalf("a state holding one lock does not marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"expires_at":"2026-09-02T07:00:00Z"`) {
		t.Errorf("the expiry is not in the snapshot as expires_at: %s\n"+
			"  consequence: every lock comes back from disk with no expiry, which is "+
			"indistinguishable from one deliberately taken forever -- so nothing is "+
			"ever stealable and a crashed holder stalls the run.", raw)
	}

	var back State
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("the snapshot does not read back: %v\n  %s", err, raw)
	}
	if x := back.Locks[0].ExpiresAt; x != "2026-09-02T07:00:00Z" {
		t.Errorf("the expiry read back as %q; it went out as the instant above.\n"+
			"  consequence: the round trip is lossy, so a resumed run disagrees with "+
			"the one that wrote the file about when the lock lapses.", x)
	}

	// No expiry omits the field rather than carrying "". Presence is what a reader
	// tests to tell "held until released" from "expires at T", and an empty string
	// there invites `run show --json` consumers to print an expiry that is not a time.
	raw, err = json.Marshal(State{Locks: []Lock{{Key: "migrations/", Holder: "backend"}}})
	if err != nil {
		t.Fatalf("a lock with no expiry does not marshal: %v", err)
	}
	if strings.Contains(string(raw), "expires_at") {
		t.Errorf("a lock with no expiry writes the field anyway: %s\n"+
			"  consequence: absent and empty stop being distinguishable, so a reader "+
			"cannot tell a lock held until released from one whose expiry was lost.", raw)
	}
}
