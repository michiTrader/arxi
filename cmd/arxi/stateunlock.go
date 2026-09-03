package main

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/logstore"
	"github.com/michiTrader/arxi/internal/surface"
)

// `arxi state unlock <run> <key> [--force]` -- hand a cooperative lock back.
//
// # It was promised before it existed
//
// internal/kernel/why.go's `lock` arm has handed the user `arxi state unlock` as
// the remedy for a member blocked on a key since long before any registry entry
// declared the verb, and `state lock` sharpened that hole rather than filling it:
// the only lock.released it writes is a STEAL of a lapsed lease. So a holder that
// finished its work early had no way to say so, and the key stayed claimed until
// its lease ran out -- which for `--ttl 0` means until the run ends.
//
// # The three shapes, and why they are three
//
// The lock is cooperative, so who may release it is a CLI judgement and not a
// reducer one: releaseLock deliberately does not check the holder, because a
// release that only its holder could write could never reclaim a key from an agent
// that crashed mid-turn. That leaves this command to distinguish the cases, which
// it does by reason, all three of them values in the payload row spec/events.md
// already declares:
//
//   - our OWN lock -> reason "released". The ordinary case, no ceremony.
//   - somebody else's LAPSED lease -> reason "expired", carrying previous_holder
//     and expired_at, exactly as statelock.go's steal records it. The evidence for
//     the judgement goes in the log because the fold has no clock to re-take it.
//   - somebody else's LIVE lease -> reason "forced", and it needs --force. This is
//     the one that ends work in flight: the holder may be editing the files the key
//     guards, and it has been promised the key until an instant that has not
//     arrived.
//
// # Why --force is not asked for in the other two
//
// Not for our own lock, obviously. And deliberately not for a lapsed foreign lease
// either, even though that one takes the key from another name: `arxi state lock`
// already steals a lapsed lease with no flag at all, so
//
//	arxi state lock r1 migrations/ && arxi state unlock r1 migrations/
//
// frees it in two steps that nobody has to justify. Demanding ceremony on the
// one-step path would make the route that reclaims the key WITHOUT taking it
// first -- the safer of the two -- look like the more dangerous one.
//
// # SourceHuman, unlike the steal in statelock.go
//
// There the release was SourceRuntime because the lock.acquired batched behind it
// carried the wake, and waking a lock.* watcher twice bills two turns for one
// handover. Here the release is the whole event. Stamping it runtime would skip
// wakeWatchers outright, so a member declared to watch lock.* would not hear that
// the key it was waiting for is free -- which is the only news this command has.
const stateUnlockUsage = "usage: arxi state unlock <run> <key> [--force]\n" +
	"  <key>      what is being handed back -- the same string it was claimed by\n" +
	"  --force    release a lease that has NOT run out, ending work in flight\n" +
	"  short: -r run · -k key\n" +
	"  exit 1     the key is not held, or is held live and --force was not given\n" +
	"  who holds what: arxi run show <run>\n" +
	"  its history:    arxi event log <run> --type 'lock.*'\n"

// unlockKeysListed caps the "what this run does hold" listing on a miss, per
// stateGetKeysListed. A run with forty locks would otherwise answer a mistyped key
// with forty lines, and the answer to the question -- is my key in here -- is lost
// in them. The overflow line names the command that prints all of them.
const unlockKeysListed = 10

func cmdStateUnlock(args []string) {
	c := surface.Lookup("state", "unlock")
	vals, err := parseInvocation(c, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi state unlock: %v\n\n%s", err, stateUnlockUsage)
		os.Exit(2)
	}

	// Everything the caller typed is checked before the run is touched, per
	// cmdStateLock: an append-only log cannot take a write back, so a command that
	// validates late is a command whose mistakes are permanent.
	runArg := strings.TrimSpace(vals["run"])
	if runArg == "" {
		fmt.Fprint(os.Stderr, "arxi state unlock: which run?\n\n"+stateUnlockUsage)
		os.Exit(2)
	}

	key := vals["key"]
	if err := checkUnlockKey(key); err != nil {
		fmt.Fprintf(os.Stderr, "arxi state unlock: %v\n\n%s", err, stateUnlockUsage)
		os.Exit(2)
	}
	force := vals["force"] == "true"

	dir := resolveRunDir(runArg)

	// Folded twice, per cmdStateLock, and for the same reason: this one answers "is
	// that a run at all" without having taken the writer lock, because logstore.Open
	// calls MkdirAll and a run id with a typo in it would otherwise leave an empty
	// directory behind for `run list` to show as a run.
	if _, _, _, err := foldRunDir(dir); err != nil {
		fmt.Fprintf(os.Stderr, "arxi state unlock: %v\n", err)
		os.Exit(1)
	}

	// The writer lock is taken BEFORE the fold that decides, exactly as `state lock`
	// does, and this command is the mirror image of the same race. It is a
	// read-decide-write: it reads who holds the key, concludes it may release it, and
	// writes. Two shells racing -- one forcing, one whose holder is renewing -- would
	// both fold, both see the state they decided from, and only then queue at Open, so
	// a conclusion reached before the writer lock can be stale by the time it is acted
	// on. Here the stale conclusion is worse than a duplicate row: a --force that
	// arbitrates against a lease which has since been RENEWED would break a lock whose
	// holder had just proved it is alive.
	store, err := logstore.Open(dir)
	if err != nil {
		fatal(err)
	}
	// Both, and not redundant: the defer covers ordinary returns and atExit covers
	// os.Exit, which runs no defers. Every refusal below this line exits, so without
	// the second half a refused release would leave writer.lock holding a dead pid --
	// measured on `run unpause`, where it bricked the run.
	defer store.Close()
	atExit(func() { store.Close() })

	pre, cfg, simulated, err := foldRunDir(dir)
	if err != nil {
		// Unreachable in practice: the fold above just succeeded on this directory.
		// Handled anyway rather than ignored, because carrying on would decide who holds
		// the key from a zero State, in which every key reads as free -- and this command
		// refuses a free key, so the failure would present as "no such lock".
		fatal(err)
	}

	// A terminal run is refused, and here what would be lost is the whole event: Decide
	// returns before the switch on a terminal status, so the release would be appended
	// and the LockReleased arm never reached. The key would read as held in every fold
	// of a log that contains its release.
	//
	// Unlike `state lock`, there is a second half to say: nothing is holding anything
	// up either. A finished run has no member left waiting on the key, so there is no
	// remedy to offer beyond the truth.
	if pre.Status.Terminal() {
		fmt.Fprintf(os.Stderr, "arxi state unlock: run %s is %s, which is final.\n"+
			"  the reducer ignores every event after a terminal status, so the release "+
			"would be recorded in the log and free nothing -- `arxi run show %s` would "+
			"still list %s as held.\n"+
			"  nothing is waiting on it either: a finished run has no member left to "+
			"take it.\n", pre.RunID, pre.Status, pre.RunID, key)
		os.Exit(1)
	}

	// One clock read for the whole command, per cmdStateLock: the instant that decides
	// a lease has lapsed is the same one stamped on the event, so nothing this command
	// prints can disagree with what it wrote.
	now := nowFunc().UTC()

	held := heldLock(pre, key)
	if held == nil {
		refuseUnheldLock(pre, key)
	}

	// The arbitration. Three outcomes, and the reason is the part of the payload that
	// distinguishes them for every later reader -- `event log --type lock.*` is where
	// somebody asks why a key changed hands.
	reason := "released"
	switch {
	case held.Holder == cliLockHolder:
		// Our own, by the only definition the log has: Actor is empty on a lock this
		// binary takes, so lockHolder falls back to the source and every shell is the one
		// holder "human" (see cliLockHolder). Two shells are therefore one holder here as
		// well, which is the honest reading of what was recorded -- and it is why --force
		// is not wanted for this arm even though the shell releasing may not be the shell
		// that claimed.
		if force {
			// Not an error. --force on a lock that needs none is a caller being careful, and
			// refusing it would send them to read the holder out of `run show` to find out
			// they were entitled all along. Said out loud, though, because a caller who
			// believes they broke somebody's lease has learned something false about the run.
			fmt.Fprintf(os.Stderr, "note: --force was not needed. %s is held by %s, which "+
				"is what every lock taken from a shell is held by, so this is your own "+
				"lock and releasing it breaks nobody's lease.\n", key, held.Holder)
		}
	case lockLapsed(*held, now):
		// A lapsed foreign lease, recorded the way statelock.go's steal records it: the
		// judgement needs a clock, the fold has none, so the evidence goes in the log
		// where the next fold reproduces the conclusion without one.
		reason = "expired"
	case !force:
		refuseLiveLock(pre, *held, key, now)
	default:
		reason = "forced"
	}

	watch := emitWatcherOutcomes(pre, cfg, string(kernel.LockReleased))
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

	// The same asymmetry `state set` and `state lock` document, and here the case for
	// writing anyway is the strongest of the three: the key is FREED whether or not
	// the run is spending, and a held lock on a paused run is exactly the thing an
	// operator is trying to clear. Refusing would teach them to unpause a run -- which
	// resumes spending -- in order to hand a key back.
	//
	// The exception is a run_tool watcher, the one outcome a halt destroys:
	// wakeWatchers returns CallTool unconditionally, so spawnCauses never sees it and
	// nothing parks it, and nothing re-decides this event later -- the next drive folds
	// it into a starting state and keeps no effects. The call would be dropped in
	// silence. Nothing is written yet, so clearing the halt and repeating costs
	// nothing but the order of two commands.
	if spendingHalted(pre) && len(tools) > 0 {
		fmt.Fprintf(os.Stderr, "arxi state unlock: run %s is %s, and a watcher on %s "+
			"would run a tool: %s.\n"+
			"  a queued turn survives the halt, a tool call does not: it is returned by "+
			"the reducer and dropped, because the next drive folds this event into its "+
			"starting state and keeps no effects from it.\n"+
			"  nothing was written, so %s is still held by %s. clear the halt, then "+
			"release it:\n    %s\n    arxi state unlock %s %s%s\n",
			pre.RunID, pre.Status, kernel.LockReleased, strings.Join(tools, ", "),
			key, held.Holder, haltRemedy(pre), pre.RunID, key, forceArg(reason))
		os.Exit(1)
	}

	payload := map[string]any{"key": key, "reason": reason}

	// previous_holder on every release, and not only on the two that take a key from
	// another name. spec/events.md marks it optional, but the field is what makes the
	// log answer "who had it" without a reader folding the whole run to find the
	// matching acquire -- and on a release of our own it is the only record that the
	// holder was a shell rather than a member.
	payload["previous_holder"] = held.Holder

	// expired_at ONLY when the lease actually lapsed, matching statelock.go's steal.
	// A forced release has no expiry to point at -- that is the whole difference
	// between the two -- and writing the not-yet-reached instant into a field named
	// expired_at would record a lapse that never happened, which is precisely the
	// judgement --force exists to say nobody made.
	if reason == "expired" {
		payload["expired_at"] = held.ExpiresAt
	}

	head := store.Head()

	// One event, so no batch and no atomicity question: the release is complete on its
	// own. That is the structural difference from the steal in `state lock`, where the
	// release without the acquire behind it would leave a seq at which the key reads
	// as free to a third claimant.
	//
	// SourceHuman, unlike that steal, because here the release IS the news:
	// wakeWatchers is skipped outright for SourceRuntime, so a runtime release would
	// leave a member declared to watch lock.* waiting for a key that is already free.
	// Actor stays empty, per lockEvent, so a member's own lock.* watcher is not
	// silently disabled by naming it.
	ev := lockEvent(kernel.LockReleased, kernel.SourceHuman, pre.RunID, head+1, now, payload)

	written, err := store.Append([]kernel.Event{ev})
	if err != nil {
		fatal(fmt.Errorf("record %s: %w", kernel.LockReleased, err))
	}
	at := written[0].Seq

	fmt.Printf("run %s released %s (seq %d)\n", pre.RunID, key, at)
	switch reason {
	case "expired":
		fmt.Printf("  taken from %s, whose lease lapsed at %s -- recorded as %s so the "+
			"next fold reaches the same conclusion without a clock\n",
			held.Holder, held.ExpiresAt, strconv.Quote("expired"))
	case "forced":
		fmt.Printf("  taken from %s, whose lease ran to %s and had not run out: "+
			"recorded as %s, with previous_holder naming them. The event does not "+
			"record who ended it -- every release from a shell folds to holder %s -- "+
			"so if that matters here, say it somewhere the log keeps\n",
			held.Holder, held.ExpiresAt, strconv.Quote("forced"),
			strconv.Quote("human"))
	default:
		fmt.Printf("  it was held by %s\n", held.Holder)
	}

	printStateUnlockOutlook(pre, key, watch, acts)

	// Two different reasons not to drive, per `state set` and `state lock`. No watcher
	// acts: driving would run the loop from the tip, find nothing pending and print a
	// summary implying the release started something. The run is halted: the cause is
	// parked in the state, and handing a key back must not resume spending as a side
	// effect -- which is what `run unpause` is for.
	if !acts || spendingHalted(pre) {
		return
	}
	if simulated {
		fmt.Printf("  this run was started with --sim, so the turn is taken by the " +
			"same fake executor: no model is called and no money is spent.\n")
	}
	driveResumedRun(dir, cfg, store, pre.RunID, simulated)
}

// checkUnlockKey refuses the keys no lock could be held under.
//
// It DECIDES with checkStateKey and checkLockKey -- one predicate, three wordings --
// and the wording differs because what the caller has to do differs. On the lock
// side a padded key silently guards nothing; here it cannot match anything, because
// heldLock compares exactly, so the answer would be "no such lock" for a key that is
// held and one space away. That is the misdiagnosis worth naming: the caller would
// go looking for a release that already happened.
func checkUnlockKey(k string) error {
	if checkStateKey(k) == nil {
		return nil
	}
	if strings.TrimSpace(k) == "" {
		return fmt.Errorf("which key? a release names the string the lock was claimed "+
			"by -- a path prefix like migrations/ -- and the reducer drops a %s with an "+
			"empty key, so this would report freeing a lock it left held",
			kernel.LockReleased)
	}
	// %q on the key and Quote on the suggestion, since unquoted the two look
	// identical and the message reads as a bug in this command.
	return fmt.Errorf("the key %q carries padding or a line break, and the fold compares "+
		"keys exactly -- so this would be refused as an unheld key even while %s is held, "+
		"and the caller would go looking for a release that had already happened. did you "+
		"mean %s?",
		k, strconv.Quote(strings.TrimSpace(k)), strconv.Quote(strings.TrimSpace(k)))
}

// forceArg renders --force for a command line printed back to the caller, so the
// suggestion they are handed is one that will actually work. Derived from the
// arbitrated reason rather than from what they typed: the halt refusal above is
// reached AFTER arbitration, so "forced" means --force was given and accepted, and
// a suggestion that dropped it would be refused for a different reason on the retry.
func forceArg(reason string) string {
	if reason == "forced" {
		return " --force"
	}
	return ""
}

// refuseUnheldLock declines a key nothing holds, and lists what the run does hold.
//
// Exit 1 and NOT exitLockHeld, which is the one code choice in this file worth
// arguing. 3 means "ask again later" across the whole binary -- `run result`'s no
// result yet, `state get`'s unset key, `state lock`'s live lease -- and it is what
// makes `until arxi state lock r1 k; do sleep 5; done` terminate. Nobody polls an
// unlock: a key that is not held will not become held by waiting, and if it did,
// waiting for that in order to release it is not a thing anybody wants. So this is
// a mistake to be corrected now, which is 1.
//
// The locks are listed for the reason printKeyUnset lists the store's keys: the
// overwhelmingly likely cause is a name that does not match, and the fold is the
// only place the real one exists. Sorted, because output that reshuffles between
// two identical invocations looks broken even when it is right. Quoted, because a
// key differing by padding or by an invisible character is exactly what a listing
// has to make visible.
func refuseUnheldLock(st kernel.State, key string) {
	fmt.Fprintf(os.Stderr, "arxi state unlock: run %s holds no lock on %s (seq %d).\n",
		st.RunID, key, st.Seq)

	if len(st.Locks) == 0 {
		fmt.Fprintf(os.Stderr, "  it holds no locks at all: no %s has been folded into "+
			"this run, or every one of them has been released already.\n"+
			"  every lock this run ever took: arxi event log %s --type %s\n",
			kernel.LockAcquired, st.RunID, kernel.LockAcquired)
		exitUnheldLock(st)
	}

	locks := make([]kernel.Lock, len(st.Locks))
	copy(locks, st.Locks)
	sort.Slice(locks, func(i, j int) bool { return locks[i].Key < locks[j].Key })

	noun := "locks"
	if len(locks) == 1 {
		noun = "lock"
	}
	fmt.Fprintf(os.Stderr, "  it holds %d %s:\n", len(locks), noun)
	for i, l := range locks {
		if i == unlockKeysListed {
			fmt.Fprintf(os.Stderr, "    ... and %d more, all of them in: arxi run show %s\n",
				len(locks)-i, st.RunID)
			break
		}
		until := "no expiry"
		if l.ExpiresAt != "" {
			until = "until " + l.ExpiresAt
		}
		fmt.Fprintf(os.Stderr, "    %s held by %s, %s\n", strconv.Quote(l.Key), l.Holder, until)
	}
	fmt.Fprintf(os.Stderr, "  a lock is an exact string, so a name differing by one "+
		"character or by padding is a different key.\n")
	exitUnheldLock(st)
}

// exitUnheldLock is the shared tail of refuseUnheldLock: the code, and why it is not
// the other one. Factored out because the empty-run branch returns early and the two
// exits must not drift -- a refusal that exits 1 in one branch and 3 in the other is
// a refusal no script can act on.
func exitUnheldLock(st kernel.State) {
	fmt.Fprintf(os.Stderr, "  exit 1 and not %d: %d means ask again later, and an unheld "+
		"key does not become held by waiting.\n"+
		"  who holds what: arxi run show %s\n", exitLockHeld, exitLockHeld, st.RunID)
	os.Exit(1)
}

// refuseLiveLock declines somebody else's lease that has not run out.
//
// Reached only from the arbitration in cmdStateUnlock, so the holder is not this
// shell, lockLapsed answered false, and --force was not given. Two shapes reach it
// and they need different sentences, because what the caller should do differs:
//
//   - a LIVE lease says for how long, so waiting is a real option and the message
//     prices it. Breaking it costs the holder work in flight.
//   - NO expiry, or an unreadable one, cannot lapse, so waiting is not an option at
//     all -- this is the stall docs/design/20-use-cases.md §20.8 names, and --force
//     is now the answer to it. That is the sentence `state lock` could not write
//     before this verb existed.
//
// Exit 1 throughout, not exitLockHeld, and the live case is the one where 3 is
// tempting: waiting WOULD get the key. But 3 is for a command that will succeed
// unchanged later, and this one will not -- past the expiry the arbitration lands on
// "expired", which is a different command's answer, and before it the caller has a
// decision to take rather than a wait to sit through. A poller here is a caller
// hoping somebody else's work finishes, which is what `state lock` is for.
func refuseLiveLock(st kernel.State, held kernel.Lock, key string, now time.Time) {
	fmt.Fprintf(os.Stderr, "arxi state unlock: %s is held by %s", key, held.Holder)

	forced := fmt.Sprintf("  end it anyway: arxi state unlock %s %s --force\n"+
		"    that records %s with reason %s and previous_holder %s, so the log says "+
		"who ended the lease and whose it was.\n",
		st.RunID, key, kernel.LockReleased, strconv.Quote("forced"),
		strconv.Quote(held.Holder))

	if held.ExpiresAt == "" {
		fmt.Fprintf(os.Stderr, " with no expiry, so it cannot lapse and nothing reclaims "+
			"it on its own -- this is the lock that stalls a run until a human notices "+
			"(docs/design/20-use-cases.md §20.8).\n%s"+
			"  nothing is lost by waiting instead, either: there is no instant at which "+
			"this frees itself.\n"+
			"  when it was taken: arxi event log %s --type %s\n",
			forced, st.RunID, kernel.LockAcquired)
		os.Exit(1)
	}

	until, err := time.Parse(time.RFC3339, held.ExpiresAt)
	if err != nil {
		// Only an agent's tool call can produce this: `arxi state lock` computes an
		// absolute instant from --ttl. It is the no-expiry case with a worse cause -- no
		// reader can judge the lease lapsed, so nothing steals it either.
		fmt.Fprintf(os.Stderr, " until %q, which is not an RFC3339 instant: %v\n"+
			"  no reader can judge that lease lapsed, so nothing reclaims the key on its "+
			"own -- `arxi state lock` included. an expiry like that comes from an agent "+
			"writing the payload by hand.\n%s"+
			"  the event that wrote it: arxi event log %s --type %s\n",
			held.ExpiresAt, err, forced, st.RunID, kernel.LockAcquired)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, " until %s, which is %s from now.\n"+
		"  that holder may be editing what %s guards, and it was promised the key until "+
		"that instant -- so this command will not take it back without being told to.\n"+
		"%s"+
		"  or wait: past that instant this releases it as a lapsed lease, with no flag "+
		"and reason %s, and so does the next `arxi state lock` on the key.\n"+
		"  who holds what: arxi run show %s\n",
		held.ExpiresAt, until.Sub(now).Round(time.Second), key, forced,
		strconv.Quote("expired"), st.RunID)
	os.Exit(1)
}

// printStateUnlockOutlook says what the release did BEYOND freeing the key.
//
// The same shape as printStateLockOutlook, with one thing it must NOT say. A member
// blocked on this key is still blocked: Decide's LockReleased arm is
// `releaseLock(&out, e.Str("key"))` and nothing more, and nothing in the tree emits
// agent.unblocked, so freeing a key does not clear the member waiting on it. A line
// implying otherwise would send the caller away believing the run is moving; instead
// the no-watcher case names the declaration that WOULD make it move.
func printStateUnlockOutlook(st kernel.State, key string, watch []emitWatcherOutcome, acts bool) {
	holders := fmt.Sprintf("  who holds what: arxi run show %s\n", st.RunID)

	// Asked BEFORE the per-watcher notes, per printStateLockOutlook: spawnCauses
	// consults spendingHalted before it looks at the member, so a line computed from
	// the member alone would announce a turn the reducer never opens.
	if spendingHalted(st) {
		if acts {
			fmt.Printf("  freed, but run %s is %s, so the turn a watcher on %s would open "+
				"is parked rather than started -- it is handed back when that clears.\n",
				st.RunID, st.Status, kernel.LockReleased)
			fmt.Printf("  clear it: %s\n", haltRemedy(st))
		} else {
			fmt.Printf("  freed. run %s is %s, and nothing was watching %s anyway -- %s is "+
				"claimable either way, and whoever unpauses inherits that.\n",
				st.RunID, st.Status, kernel.LockReleased, key)
		}
		fmt.Print(holders)
		return
	}

	if len(watch) == 0 {
		// The type is fixed at lock.released, so the run's other patterns are not listed
		// the way `event emit` lists them: there is nothing here the caller could have
		// mistyped, and a list of patterns would invite the guess that the KEY has to
		// match one.
		fmt.Printf("  freed, and nobody was told: this run's blueprint declares no watcher "+
			"matching %s, so no turn opens.\n", kernel.LockReleased)
		// The blocked member is named explicitly, because this is the case where the
		// caller is most likely to expect the release to have done it. It has not: the
		// reducer's release arm frees the key and stops there, and no event in the tree
		// clears a waiting member.
		if waiter := lockWaiter(st, key); waiter != "" {
			fmt.Printf("  %s is blocked on %s and is NOT woken by this: the reducer's %s "+
				"arm frees the key and nothing more, so the member stays waiting until a "+
				"turn is opened for it.\n", waiter, key, kernel.LockReleased)
			fmt.Printf("  what it is waiting for: arxi run why %s\n", st.RunID)
		}
		fmt.Print("  a member that should react to a key coming free is declared with: " +
			"watchers: [{agent: <name>, pattern: lock.*}]\n")
		fmt.Print(holders)
		return
	}
	for _, o := range watch {
		fmt.Printf("  %s\n", o.note)
	}
	if !acts {
		fmt.Printf("  so nothing starts now: %s is free and the run is left where it "+
			"was.\n", key)
	}
	fmt.Print(holders)
}

// lockWaiter names a member blocked on this key, or "" if none is.
//
// Read off the blocked_ref the event catalogue requires of every agent.blocked
// (spec/events.md §The blocked_ref rule) via exactly the fields walkCause reads:
// Member.Detail is the `blocked_on` string and Member.BlockedOn is the reference
// map -- the two names are the other way round from what they describe, which is
// worth stating here because the plausible reading of the code is the wrong one.
// Going through the same pair means this reports a waiter exactly when `run why`
// would name one, instead of inventing a second notion of waiting.
//
// The state gate matches why.go's too. A member that is not MemberWaiting is not
// held up by this key even if it carries a stale reference to it, and reporting one
// would name a member that is working as blocked.
//
// The FIRST match is returned rather than all of them. One key has one holder, but
// any number of members can be waiting on it, and a release frees the key for
// whichever the run opens a turn for first. Naming one as an example is honest;
// naming one as the winner would not be, which is why the sentence around this says
// only that it is not woken.
func lockWaiter(st kernel.State, key string) string {
	for _, m := range st.Members {
		if m.State != kernel.MemberWaiting || m.Detail != "lock" {
			continue
		}
		if k, _ := m.BlockedOn["key"].(string); k == key {
			return m.Name
		}
	}
	return ""
}
