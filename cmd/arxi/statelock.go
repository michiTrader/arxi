package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/logstore"
	"github.com/michiTrader/arxi/internal/surface"
)

// `arxi state lock <run> <key> [--ttl <d>]` -- take a cooperative lock, the third
// and last verb of the run's shared state.
//
// # What it buys
//
// docs/design/20-use-cases.md §20.8: two members told to work in parallel will
// both edit migrations/ unless something says who has it. The lock is that
// something. It is COOPERATIVE and not a filesystem lock -- it coordinates intent,
// and real isolation comes from the workspace -- so its whole value is that every
// reader of the fold agrees on one holder. That invariant is enforced in the
// reducer (acquireLock), not here.
//
// # The lease is the design, and it is why --ttl defaults
//
// §20.8 names the failure this command exists to avoid: "a lock with no expiry,
// held by an agent that crashed mid-turn, stalls the run until a human notices".
// Nothing in this binary writes lock.released except this command, stealing a lock
// whose lease has run out -- so a lock with no expiry is held until the run ends.
// A caller who says nothing therefore gets a lease, not eternity.
//
// # Refused with exit 3, so a script can wait
//
//	until arxi state lock r1 migrations/; do sleep 5; done
//
// terminates because a held key exits 3 and 1 stays reserved for "the run could
// not be read" -- the loop above would spin forever on a run id with a typo in it
// if the two shared a code. It is the same 3 as `run result`'s "not yet" and
// `state get`'s "not set", deliberately: one number means "ask again later"
// across the whole binary.
const stateLockUsage = "usage: arxi state lock <run> <key> [--ttl <d>]\n" +
	"  <key>      what is being claimed -- a path prefix like migrations/\n" +
	"  --ttl      how long the claim lasts; default 10m, 0 for no expiry\n" +
	"  short: -r run · -k key\n" +
	"  exit 3     somebody else holds it and their lease has not run out\n" +
	"  who holds what: arxi run show <run>\n" +
	"  its history:    arxi event log <run> --type 'lock.*'\n"

// defaultLockTTL is the lease a caller who says nothing gets.
//
// Ten minutes is chosen against the thing on the other end of the lock: a turn.
// It has to outlast an ordinary one, or a member would lose the key while still
// editing under it, and it has to be short enough that a crashed holder is
// reclaimed by the next claimant rather than by a human reading the log. A member
// whose work legitimately runs longer says so -- `--ttl 1h` -- and one that is
// still working re-acquires, which extends the lease it already holds.
const defaultLockTTL = 10 * time.Minute

// exitLockHeld is `run result`'s "not yet", shared for the reason
// exitStateKeyUnset shares it: a caller polling for a key and a caller polling for
// a lock are the same caller, and one number keeps the two from drifting apart.
const exitLockHeld = exitResultNotYet

// cliLockHolder is the name the fold gives a lock taken from a shell.
//
// It is not a choice this file makes, it is a consequence: the event names no
// actor (see the Event below), so lockHolder in the reducer falls back to the
// source, and the source of a command a person typed is human. Written as a
// constant off kernel.SourceHuman rather than as "human" so the two cannot
// disagree -- the day the reducer's fallback changes, this stops compiling instead
// of silently reporting every lock as free.
//
// The consequence worth stating out loud: two shells are ONE holder. The log
// cannot tell them apart, so a second `state lock` on a key this command already
// holds extends the lease instead of being refused. That is the honest reading of
// what the log records, and it is why the refusal below is about somebody ELSE's
// lock.
const cliLockHolder = string(kernel.SourceHuman)

func cmdStateLock(args []string) {
	c := surface.Lookup("state", "lock")
	vals, err := parseInvocation(c, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi state lock: %v\n\n%s", err, stateLockUsage)
		os.Exit(2)
	}

	// Everything the caller typed is checked before the run is touched, per
	// cmdStateSet: an append-only log cannot take a write back, so a command that
	// validates late is a command whose mistakes are permanent.
	runArg := strings.TrimSpace(vals["run"])
	if runArg == "" {
		fmt.Fprint(os.Stderr, "arxi state lock: which run?\n\n"+stateLockUsage)
		os.Exit(2)
	}

	key := vals["key"]
	if err := checkLockKey(key); err != nil {
		fmt.Fprintf(os.Stderr, "arxi state lock: %v\n\n%s", err, stateLockUsage)
		os.Exit(2)
	}

	ttl, given, err := parseLockTTL(vals["ttl"])
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi state lock: %v\n\n%s", err, stateLockUsage)
		os.Exit(2)
	}

	dir := resolveRunDir(runArg)

	// Folded twice, and the SECOND fold is the one every decision below is taken
	// from. This one answers "is that a run at all" without having taken the writer
	// lock, and it is not redundant: logstore.Open calls MkdirAll, so a run id with a
	// typo in it would otherwise leave an empty directory behind for `run ls` to list
	// as a run.
	if _, _, _, err := foldRunDir(dir); err != nil {
		fmt.Fprintf(os.Stderr, "arxi state lock: %v\n", err)
		os.Exit(1)
	}

	// The writer lock is taken BEFORE the fold that arbitrates, which is a departure
	// from `state set` -- fold, then open -- and what a lock is for is the reason.
	//
	// This command is a read-decide-write: it reads who holds the key, concludes the
	// key is free, and writes an acquire. Two shells racing for one key both fold,
	// both see it free, and only then queue at Open -- so a conclusion reached before
	// the writer lock is a conclusion that can be stale by the time it is acted on,
	// and the second shell would append an acquire for a key the first had just
	// taken. One key with two claimants is the exact outcome the type exists to
	// prevent, so the fold that decides happens where the log cannot move under it.
	store, err := logstore.Open(dir)
	if err != nil {
		fatal(err)
	}
	// Both, and not redundant: the defer covers ordinary returns and atExit covers
	// os.Exit, which runs no defers. Every refusal below this line exits, so without
	// the second half a refused acquire would leave writer.lock holding a dead pid --
	// measured on `run unpause`, where it bricked the run.
	defer store.Close()
	atExit(func() { store.Close() })

	pre, cfg, simulated, err := foldRunDir(dir)
	if err != nil {
		// Unreachable in practice: the fold above just succeeded on this directory.
		// Handled anyway rather than ignored, because carrying on would decide who
		// holds the key from a zero State, in which every key reads as free.
		fatal(err)
	}

	// A terminal run is refused, and here it is the LOCK that would be lost rather
	// than merely a watcher: Decide returns before the switch on a terminal status,
	// so the acquire would be appended and the arm never reached.
	if pre.Status.Terminal() {
		fmt.Fprintf(os.Stderr, "arxi state lock: run %s is %s, which is final.\n"+
			"  the reducer ignores every event after a terminal status, so the acquire "+
			"would be recorded in the log and hold nothing -- `arxi run show %s` would "+
			"list no lock on %s.\n"+
			"  nothing needs one either: a finished run has no member left to edit "+
			"under it.\n", pre.RunID, pre.Status, pre.RunID, key)
		os.Exit(1)
	}

	// One clock read for the whole command. The instant that decides a lease has
	// lapsed is the same instant the new lease is measured from and the same one
	// stamped on the events, so nothing this command prints can disagree with what it
	// wrote -- which two calls to time.Now would eventually allow.
	now := nowFunc().UTC()

	// The arbitration, and the CLI's half of it is only about a lock somebody ELSE
	// holds. Two callers of this command are one holder (see cliLockHolder), and the
	// reducer refuses a second holder on its own; what the reducer cannot do is
	// decide a lease has run out, because it has no clock. So that judgement is
	// taken here and written down.
	stolen, verb := (*kernel.Lock)(nil), "took"
	if held := heldLock(pre, key); held != nil {
		switch {
		case held.Holder == cliLockHolder:
			// An extension is the same event as an acquire -- acquireLock replaces the
			// expiry when the holder matches -- so a lapsed lock of our own needs no
			// release first. Only the word for it changes, and it changes because
			// "extended" would be a lie about a lease that had already run out.
			verb = "extended"
			if lockLapsed(*held, now) {
				verb = "re-took"
			}
		case lockLapsed(*held, now):
			stolen = held
		default:
			refuseHeldLock(pre, *held, key, now)
		}
	}

	watch := emitWatcherOutcomes(pre, cfg, string(kernel.LockAcquired))
	acts := false
	var tools []string
	for _, o := range watch {
		if o.acts {
			acts = true
		}
		if o.tool != "" {
			tools = append(tools, o.tool+" for "+o.agent)
		}
	}

	// The same asymmetry `state set` documents: a paused run is not refused
	// wholesale, because taking the key is worth something by itself -- it is held,
	// `run show` says so, and whoever unpauses inherits that. The exception is a
	// run_tool watcher, the one outcome a halt destroys: wakeWatchers returns CallTool
	// unconditionally, so nothing parks it, and nothing re-decides this event later,
	// because the next drive folds it into a starting state and keeps no effects. The
	// call would be dropped in silence. Nothing is written yet, so clearing the halt
	// and repeating the command costs nothing.
	if spendingHalted(pre) && len(tools) > 0 {
		fmt.Fprintf(os.Stderr, "arxi state lock: run %s is %s, and a watcher on %s "+
			"would run a tool: %s.\n"+
			"  a queued turn survives the halt, a tool call does not: it is returned by "+
			"the reducer and dropped, because the next drive folds this event into its "+
			"starting state and keeps no effects from it.\n"+
			"  nothing was written, so %s stands as it did. clear the halt, then take "+
			"it:\n    %s\n    arxi state lock %s %s%s\n",
			pre.RunID, pre.Status, kernel.LockAcquired, strings.Join(tools, ", "),
			key, haltRemedy(pre), pre.RunID, key, ttlArg(given, ttl))
		os.Exit(1)
	}

	head := store.Head()
	var batch []kernel.Event

	// A steal is a release RECORDED before the acquire, and not a special case in the
	// reducer. That is what lets ExpiresAt be a string the fold never compares to a
	// clock: judging a lease lapsed needs a now, this command has one, and writing the
	// judgement down leaves it in the log for the next fold to reproduce without a
	// clock of its own.
	//
	// SourceRuntime, alone in this file, because a lapse is bookkeeping rather than a
	// decision somebody took -- and wakeWatchers is skipped outright for runtime
	// events. A member watching lock.* wants to hear that the key changed hands, which
	// the acquire below tells it; waking it twice would bill two turns for one fact.
	if stolen != nil {
		batch = append(batch, lockEvent(kernel.LockReleased, kernel.SourceRuntime,
			pre.RunID, head+1, now, map[string]any{
				"key":             key,
				"previous_holder": stolen.Holder,
				"reason":          "expired",
				"expired_at":      stolen.ExpiresAt,
			}))
	}

	// The expiry is computed here and not in the reducer, and it is absolute by the
	// time it reaches the log. A payload carrying "10m" would make the fold depend on
	// when it ran -- the same log would report the lock live in the morning and lapsed
	// in the afternoon -- and replay fidelity is what the design is for.
	payload := map[string]any{"key": key}
	var expires string
	if ttl > 0 {
		expires = now.Add(ttl).Format(time.RFC3339)
		payload["expires_at"] = expires
	}
	batch = append(batch, lockEvent(kernel.LockAcquired, kernel.SourceHuman,
		pre.RunID, head+int64(len(batch))+1, now, payload))

	// ONE Append for both, which is what makes a steal safe. A batch is atomic --
	// logstore rolls back a partial one on the next Open -- so there is no seq at
	// which the key reads as released-but-not-taken, and no crash that can leave it
	// there. Appending twice would open exactly that window, and a third claimant
	// folding the log inside it would find the key free and take it.
	written, err := store.Append(batch)
	if err != nil {
		fatal(fmt.Errorf("record %s: %w", kernel.LockAcquired, err))
	}
	at := written[len(written)-1].Seq

	fmt.Printf("run %s %s %s (seq %d)\n", pre.RunID, verb, key, at)
	if expires == "" {
		fmt.Printf("  held by %s with no expiry\n", cliLockHolder)
	} else {
		fmt.Printf("  held by %s until %s\n", cliLockHolder, expires)
	}
	if stolen != nil {
		fmt.Printf("  taken from %s, whose lease lapsed at %s (released at seq %d)\n",
			stolen.Holder, stolen.ExpiresAt, written[0].Seq)
	}
	if !given {
		fmt.Printf("  --ttl was not given, so the lease is %s: a lock with no expiry, "+
			"held by an agent that crashed mid-turn, stalls the run until a human "+
			"notices (docs/design/20-use-cases.md §20.8).\n", defaultLockTTL)
	}

	// On stderr, unlike every other note here, because the callers of this command
	// are scripts and a script writes `arxi state lock r1 k >/dev/null`. The one
	// consequence a caller must not be able to discard is having created a lock
	// nothing can reclaim.
	if ttl == 0 {
		fmt.Fprintf(os.Stderr, "warning: %s is held with no expiry, and the only "+
			"lock.released this binary writes is the one above -- a steal of a LAPSED "+
			"lease. Nothing releases a lock that cannot lapse, so this one is held "+
			"until the run ends.\n", key)
	}

	printStateLockOutlook(pre, key, watch, acts)

	// Two different reasons not to drive, per `state set`. No watcher acts: driving
	// would run the loop from the tip, find nothing pending and print a summary
	// implying the acquire started something. The run is halted: the cause is parked
	// in the state, and taking a lock must not resume spending as a side effect.
	if !acts || spendingHalted(pre) {
		return
	}
	if simulated {
		fmt.Printf("  this run was started with --sim, so the turn is taken by the " +
			"same fake executor: no model is called and no money is spent.\n")
	}
	driveResumedRun(dir, cfg, store, pre.RunID, simulated)
}

// heldLock finds the row the fold left for a key, or nil if the key is free.
//
// An EXACT key match, because acquireLock compares keys exactly. A prefix match
// here would be a different command from the one the reducer implements: this
// would refuse `migrations/2026_add_users.sql` as covered by a lock on
// `migrations/`, then the reducer would append a second row for it, and the fold
// would hold two locks the CLI believes are one.
//
// That is a real limit of the type rather than something to paper over in the CLI,
// and it is why the usage line calls a key "a path prefix like migrations/": the
// convention that makes cooperative locks work is that members claim the same
// string, not that the tree understands containment.
func heldLock(st kernel.State, key string) *kernel.Lock {
	for i := range st.Locks {
		if st.Locks[i].Key == key {
			return &st.Locks[i]
		}
	}
	return nil
}

// lockLapsed reports whether a lease has run out by the clock passed in.
//
// Two cases answer false that a "has it expired" reading might expect to answer
// true, and both are deliberate:
//
// An EMPTY ExpiresAt is no expiry, which is not the same as expired (see
// kernel.Lock). A lock taken with --ttl 0 is held until something releases it, and
// treating it as lapsed would make every such lock stealable by the next caller --
// exactly inverting what its holder asked for.
//
// An UNPARSEABLE ExpiresAt is not lapsed either. It can only come from an agent's
// tool call writing something that is not RFC3339, and the choice is between
// stealing a lock nobody can time and refusing to. Refusing is recoverable: the
// caller is told the expiry cannot be read (see refuseHeldLock) and a human can
// look. Stealing on unreadable input would mean that malformed payload silently
// hands the key to whoever asks next.
//
// After, not !Before: at the exact instant of expiry the lock is still held. The
// boundary has to fall one way, and holding it there matches the lease the holder
// was promised -- "until T" includes T.
func lockLapsed(l kernel.Lock, now time.Time) bool {
	if l.ExpiresAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, l.ExpiresAt)
	if err != nil {
		return false
	}
	return now.After(t)
}

// lockEvent builds one of the two events this command writes.
//
// Shared by both because they must agree on everything except type, source and
// payload. The seq is passed in rather than read from the store: a steal writes
// TWO events in one batch, and an id built from store.Head()+1 twice would give
// them the same one -- `event trace` and `event show` both address an event by id,
// so two rows sharing one is a log you cannot point at.
//
// The `now` is passed in for the same reason: one clock read for the whole command
// (see cmdStateLock), so the release that judges a lease lapsed and the acquire
// that replaces it carry the same instant. Two calls to time.Now would let the
// release be stamped after the acquire it precedes in the log.
func lockEvent(t kernel.EventType, src kernel.Source, runID string, seq int64,
	now time.Time, payload map[string]any) kernel.Event {
	return kernel.Event{
		// The same scheme as "state-set-<n>" and "emit-<n>", with the dot flattened
		// because the id appears in `event trace --id` and a dot there reads as part of
		// a type: "lock-acquired-7", not "lock.acquired-7".
		ID:   strings.ReplaceAll(string(t), ".", "-") + "-" + strconv.FormatInt(seq, 10),
		Type: t,
		// Passed in, and it is the one field the two events disagree about on purpose.
		// The acquire is SourceHuman: somebody typed this, and wakeWatchers is skipped
		// outright for SourceRuntime, so a runtime acquire would provably wake nobody.
		// The release is SourceRuntime: a lapse is bookkeeping rather than a decision,
		// and waking a lock.* watcher for the release AND the acquire would bill two
		// turns for one handover.
		Source: src,
		Scope:  "run:" + runID,
		// Actor is left EMPTY, deliberately, and here it decides who the fold says holds
		// the key. wakeWatchers skips a watcher whose agent equals the actor unless
		// include_self, so naming a member would silently disable that member's own
		// watcher on lock.*; with Actor empty, lockHolder falls back to the source and
		// the lock is held by "human" (see cliLockHolder). No member took this lock, a
		// shell did.
		Ts:      now.Format(time.RFC3339),
		Payload: payload,
	}
}

// parseLockTTL reads --ttl, and reports whether the caller gave one.
//
// The bool is why this returns three values. "10m because you said 10m" and "10m
// because you said nothing" are the same lease and different facts, and the second
// one earns the note that tells the caller a default was chosen for them -- printed
// once, where it can still change their mind, rather than on every acquire.
//
// # Zero is accepted, negative is refused
//
// `--ttl 0` is a real request: a lock held until the run ends is the right answer
// for a key one member owns for the whole run, and refusing it would leave no way
// to say so. It is answered on stderr instead, because what follows from it -- that
// nothing in this binary can reclaim it -- is a consequence the caller has to hear
// even with stdout redirected.
//
// A NEGATIVE ttl is refused rather than clamped to either of those, per
// scheduler.go's refusal of a non-positive --interval. Clamping to 0 would grant
// the eternal lock the caller did not ask for; clamping to the default would grant
// a lease they did not ask for either. Both would report success for something they
// did not type. What they meant is unknowable, so the command says so.
func parseLockTTL(raw string) (time.Duration, bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultLockTTL, false, nil
	}

	d, err := time.ParseDuration(raw)
	if err != nil {
		// The examples are the whole value of this message. "1d" is the mistake worth
		// naming: it is how everybody writes a day and it is not a Go duration, so
		// ParseDuration's own "unknown unit" is technically accurate and tells the
		// caller nothing about what to type instead.
		return 0, false, fmt.Errorf("--ttl %q is not a duration: %v\n"+
			"  examples: 30s, 10m, 1h, 24h -- there is no d unit, so a day is 24h\n"+
			"  --ttl 0 means no expiry, which is held until the run ends", raw, err)
	}
	if d < 0 {
		return 0, false, fmt.Errorf("--ttl %s is negative, so the lease would already "+
			"have run out when it was written: the next caller would steal the key and "+
			"this command would still have reported taking it.\n"+
			"  a lease that outlasts a turn: --ttl 10m (the default)\n"+
			"  no expiry at all:             --ttl 0", raw)
	}
	return d, true, nil
}

// checkLockKey refuses the keys a lock could not be taken on.
//
// It DECIDES with checkStateKey, per checkStateGetKey's "one predicate, two
// wordings". The reducer drops an acquire with an empty key (acquireLock's
// `if key == ""`), and a padded or line-broken key is a key no other member will
// type the same way -- which for a COOPERATIVE lock is the whole failure: the point
// is that two claimants collide on one string, and `migrations/ ` collides with
// nothing.
//
// The wording differs from the KV side because the damage differs. A padded KV key
// is a value that cannot be read back; a padded lock key is a lock that guards
// nothing while reporting that it does, and both members carry on editing.
func checkLockKey(k string) error {
	if checkStateKey(k) == nil {
		return nil
	}
	if strings.TrimSpace(k) == "" {
		return fmt.Errorf("what is being claimed? a lock is a name two members agree "+
			"to check -- a path prefix like migrations/ -- and the reducer drops an %s "+
			"with an empty key, so this would report a lock it did not take",
			kernel.LockAcquired)
	}
	// %q on the key and Quote on the suggestion, since unquoted the two look
	// identical and the message reads as a bug in this command.
	return fmt.Errorf("the key %q carries padding or a line break. a cooperative lock "+
		"works only because every claimant asks for the SAME string, and the fold "+
		"compares keys exactly -- so the next member to claim %s would be told the key "+
		"is free and would take a second lock on it. did you mean %s?",
		k, strconv.Quote(strings.TrimSpace(k)), strconv.Quote(strings.TrimSpace(k)))
}

// ttlArg renders the --ttl the caller gave, for the command line printed back to
// them by a refusal. Empty when they gave none, so the suggestion they are handed
// is the command they typed rather than one that pins a default they never chose --
// and repeating it verbatim is what makes the suggestion copy-pasteable.
func ttlArg(given bool, ttl time.Duration) string {
	if !given {
		return ""
	}
	return " --ttl " + ttl.String()
}

// refuseHeldLock declines a key somebody else is holding, and the exit code is
// half the message: 3 says waiting will get you the key, 1 says it will not.
//
// It is reached only from the default arm of the arbitration in cmdStateLock, so
// the lock is somebody ELSE's and lockLapsed answered false. That leaves exactly
// three shapes, and they do not deserve the same answer:
//
//   - a LIVE lease. Waiting works and the lease says for how long, so this is exit
//     3 -- the same "not yet" as `run result` and `state get`, which is what makes
//     `until arxi state lock r1 k; do sleep 5; done` terminate.
//   - NO expiry. Waiting cannot work: `event emit` is gated to custom.*, so the
//     only lock.released this binary writes is this command stealing a LAPSED
//     lease, and a lock with no expiry never lapses. Exit 1, because a poller on 3
//     would spin until the run ended.
//   - an UNREADABLE expiry, which only an agent's tool call can produce. No reader
//     can judge that lease lapsed, so it is the case above with a worse cause.
func refuseHeldLock(st kernel.State, held kernel.Lock, key string, now time.Time) {
	fmt.Fprintf(os.Stderr, "arxi state lock: %s is held by %s", key, held.Holder)

	if held.ExpiresAt == "" {
		fmt.Fprintf(os.Stderr, " with no expiry.\n"+
			"  nothing here can reclaim it. `event emit` is gated to custom.*, so the "+
			"only %s this binary writes is this command stealing a LAPSED lease -- and "+
			"this lease cannot lapse, so the key is held until the run ends.\n"+
			"  exit 1 and not %d for that reason: %d means ask again later, and no "+
			"amount of asking changes this one.\n"+
			"  who holds what:    arxi run show %s\n"+
			"  when it was taken: arxi event log %s --type %s\n",
			kernel.LockReleased, exitLockHeld, exitLockHeld, st.RunID, st.RunID,
			kernel.LockAcquired)
		os.Exit(1)
	}

	until, err := time.Parse(time.RFC3339, held.ExpiresAt)
	if err != nil {
		fmt.Fprintf(os.Stderr, " until %q, which is not an RFC3339 instant: %v\n"+
			"  no reader can judge that lease lapsed, so nothing steals the key -- this "+
			"command included -- and it is held until the run ends. an expiry like that "+
			"comes from an agent's tool call writing the payload by hand; `arxi state "+
			"lock` computes an absolute instant from --ttl.\n"+
			"  the event that wrote it: arxi event log %s --type %s\n",
			held.ExpiresAt, err, st.RunID, kernel.LockAcquired)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, " until %s, which is %s from now.\n"+
		"  exit %d, which is \"ask again later\" across this whole binary, so a script "+
		"can wait for it:\n"+
		"    until arxi state lock %s %s; do sleep 5; done\n"+
		"  past that instant the next claimant takes it: this command records a %s and "+
		"then acquires, so a holder that crashed mid-turn costs one lease rather than a "+
		"human noticing.\n"+
		"  who holds what: arxi run show %s\n",
		held.ExpiresAt, until.Sub(now).Round(time.Second), exitLockHeld, st.RunID, key,
		kernel.LockReleased, st.RunID)
	os.Exit(exitLockHeld)
}

// printStateLockOutlook says what the acquire did BEYOND recording the claim.
//
// The same shape as printStateSetOutlook, and it is not shared with
// printEmitOutlook for the same reason that one is not: "recorded, and observed by
// nobody" would be false here. The key is held whether or not a watcher matched --
// `run show` says so, and the next claimant is refused -- so a line implying the
// acquire did nothing would send the caller back to take a lock they already have.
//
// The closing line is `run show` rather than a `state get`-style read-back because
// no verb reads one lock: the fold's Locks are printed by `run show`, which is the
// only place a holder and an expiry appear together.
func printStateLockOutlook(st kernel.State, key string, watch []emitWatcherOutcome, acts bool) {
	holders := fmt.Sprintf("  who holds what: arxi run show %s\n", st.RunID)

	// Asked BEFORE the per-watcher notes, per printStateSetOutlook: spawnCauses
	// consults spendingHalted before it looks at the member, so a line computed from
	// the member alone would announce a turn the reducer never opens.
	if spendingHalted(st) {
		if acts {
			fmt.Printf("  held, but run %s is %s, so the turn a watcher on %s would open "+
				"is parked rather than started -- it is handed back when that clears.\n",
				st.RunID, st.Status, kernel.LockAcquired)
			fmt.Printf("  clear it: %s\n", haltRemedy(st))
		} else {
			fmt.Printf("  held. run %s is %s, and nothing was watching %s anyway -- %s is "+
				"claimed either way, and whoever unpauses inherits that.\n",
				st.RunID, st.Status, kernel.LockAcquired, key)
		}
		fmt.Print(holders)
		return
	}

	if len(watch) == 0 {
		// The type is fixed at lock.acquired, so the run's other patterns are not listed
		// the way `event emit` lists them: there is nothing here the caller could have
		// mistyped, and a list of patterns would invite the guess that the KEY has to
		// match one. What is actionable is the declaration they are missing.
		fmt.Printf("  held, and nothing was waiting on it: this run's blueprint declares "+
			"no watcher matching %s, so no turn opens.\n", kernel.LockAcquired)
		fmt.Print("  a member that should react to a key changing hands is declared " +
			"with: watchers: [{agent: <name>, pattern: lock.*}]\n")
		fmt.Print(holders)
		return
	}
	for _, o := range watch {
		fmt.Printf("  %s\n", o.note)
	}
	if !acts {
		fmt.Printf("  so nothing starts now: %s is held and the run is left where it "+
			"was.\n", key)
	}
	fmt.Print(holders)
}
