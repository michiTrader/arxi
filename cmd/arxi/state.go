package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/logstore"
	"github.com/michiTrader/arxi/internal/surface"
)

// cmdState routes the run's shared key/value store.
//
// Only `set` is wired; `get` and `lock` fall through to notImplemented, which
// reads the group from the registry. A hand-written list of the group's verbs
// here would be a second copy, and the copy is the one that would forget the next
// verb to land.
func cmdState(args []string) {
	if len(args) == 0 {
		notImplemented([]string{"state"})
		return
	}
	switch args[0] {
	case "set":
		cmdStateSet(args[1:])
		return
	}
	notImplemented(append([]string{"state"}, args...))
}

// `arxi state set <run> <key> <value> [--if-seq N]` -- the write half of the run's
// shared key/value store.
//
// # What the store is for
//
// docs/design/20-use-cases.md §20.8: what one member wants another to know
// without paying for a turn to say it. The member that froze an API contract
// writes the key; whoever needs it reads it back, and nobody buys a turn to relay
// the message.
//
// # Why the write is an event and not a file beside the log
//
// Because `state = fold(Decide, State0, events)` has to stay true, and a KV file
// beside the log would make it false in the direction that is hardest to notice:
// the fold would rebuild every member, lock and inbox item from August and then
// read TODAY's value for a key an agent set last Tuesday, so a replay would not
// be a replay (spec/events.md §Shared state). The key's history is not lost by
// living in the log either -- it IS the log: `arxi event log <run> --type
// state.set`.
//
// # It drives the run, and that is the difference from writing a config file
//
// state.set is deliberately outside isWatcherDispatched and is not SourceRuntime,
// so a blueprint declaring `watchers: [{agent: backend, pattern: state.*}]` gets a
// turn when the contract it was waiting for lands. Those effects live only in
// Decide's return value, so appending and returning would leave a declared watcher
// unfired and indistinguishable from a pattern that never matched -- the same
// conclusion `event emit` and `run prompt` reached, the second one after shipping
// the bug.
const stateSetUsage = "usage: arxi state set <run> <key> <value> [--if-seq <n>]\n" +
	"  <key>      the name another member reads it back by -- e.g. api.contract\n" +
	"  <value>    any string; \"\" is a value, and there is no delete\n" +
	"  --if-seq   write only if the run is still at this seq\n" +
	"  short: -r run · -k key · -v value\n" +
	"  read it back: arxi state get <run> <key>\n" +
	"  its history:  arxi event log <run> --type state.set\n"

func cmdStateSet(args []string) {
	c := surface.Lookup("state", "set")
	vals, err := parseInvocation(c, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi state set: %v\n\n%s", err, stateSetUsage)
		os.Exit(2)
	}

	// Everything the caller typed is checked BEFORE the run is opened. An
	// append-only log has no way to take a write back, so a command that validates
	// late is a command whose mistakes are permanent.
	runArg := strings.TrimSpace(vals["run"])
	if runArg == "" {
		fmt.Fprint(os.Stderr, "arxi state set: which run?\n\n"+stateSetUsage)
		os.Exit(2)
	}

	key := vals["key"]
	if err := checkStateKey(key); err != nil {
		fmt.Fprintf(os.Stderr, "arxi state set: %v\n\n%s", err, stateSetUsage)
		os.Exit(2)
	}

	// An explicit "" is accepted, and there is no guard above this line for the
	// absent case on purpose: pos() marks all three positionals Required, so
	// parseInvocation has already refused `arxi state set r1 api.contract` by the
	// time control reaches here, naming `value` and printing stateSetUsage with it.
	// A second check here was written first and measured to be unreachable; the
	// reason it was wanted is worth keeping even though the code is not, so:
	//
	// Accepting an unsupplied value would record a key whose content the caller
	// never chose and then wake every state.* watcher to go and read it -- a
	// message with no message, billed a turn. Whereas an explicit "" is a value:
	// spec/events.md has no delete, on the grounds that "a key that vanished from
	// the fold could not be told from a key nobody ever set", so emptying a key is
	// the nearest thing there is and it has to be sayable. That is the distinction
	// the usage names in one line, and it is the reachable half that the tests pin.
	value := vals["value"]

	// Parsed here, still before the run is opened, for the reason above: a
	// malformed guard is reported without having touched the log.
	var ifSeq int64 = -1
	if raw, ok := vals["if-seq"]; ok && raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "arxi state set: --if-seq %q is not a seq.\n"+
				"  it is the seq you last read, and the write is refused if the run "+
				"moved since: --if-seq 14\n"+
				"  the run's current seq is printed by: arxi run show %s\n", raw, runArg)
			os.Exit(2)
		}
		if n < 0 {
			fmt.Fprintf(os.Stderr, "arxi state set: --if-seq %d is not a seq a log "+
				"can be at.\n"+
				"  omit the flag to write wherever the run happens to be.\n", n)
			os.Exit(2)
		}
		ifSeq = n
	}

	dir := resolveRunDir(runArg)

	pre, cfg, simulated, err := foldRunDir(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi state set: %v\n", err)
		os.Exit(1)
	}

	// A terminal run is refused, and here it is the WRITE that would be lost, not
	// merely the watcher. Decide's first line returns before the switch when the
	// status is terminal, so the event would be appended and the StateSet arm never
	// reached: the key would sit in the log and be absent from every fold of it, so
	// `state get` would answer nothing for a write this command had just reported
	// as done.
	if pre.Status.Terminal() {
		fmt.Fprintf(os.Stderr, "arxi state set: run %s is %s, which is final.\n"+
			"  the reducer ignores every event after a terminal status, so the key "+
			"would be recorded in the log and read back by nobody.\n"+
			"  to carry on from where it ended: arxi run fork %s --at-seq %d\n",
			pre.RunID, pre.Status, pre.RunID, pre.Seq)
		os.Exit(1)
	}

	watch := emitWatcherOutcomes(pre, cfg, string(kernel.StateSet))
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

	// A paused or budget-blocked run is NOT refused wholesale, and the asymmetry
	// with `event emit` -- which refuses exactly this -- is the judgement in this
	// guard rather than an inconsistency with it.
	//
	// An emit on a halted run whose watcher matches is pure loss: waking somebody is
	// the event's ONLY purpose, and the cause gets parked. A state.set has a second
	// purpose that lands regardless of the status: the StateSet arm runs, the key is
	// stored, and `state get` reads it back. Refusing here would block a perfectly
	// good write and teach the user to unpause a run -- which resumes spending -- in
	// order to leave a note in it.
	//
	// The one exception is a run_tool watcher, and it is refused because it is the
	// one outcome that is unrecoverable. A notify or activate cause is parked into
	// PendingCauses, which is part of State, so drainParked hands it back when the
	// halt clears. wakeWatchers returns CallTool UNCONDITIONALLY -- spawnCauses
	// never sees it, so nothing parks it -- and nothing re-decides this event later:
	// the next drive folds everything below its cursor into the starting state, and
	// that fold discards effects. So the tool call would be dropped in silence.
	// Nothing has been written yet, so clearing the halt and repeating the command
	// costs nothing.
	if spendingHalted(pre) && len(tools) > 0 {
		fmt.Fprintf(os.Stderr, "arxi state set: run %s is %s, and a watcher on this "+
			"key would run a tool: %s.\n"+
			"  a queued turn survives the pause, a tool call does not: it is returned "+
			"by the reducer and dropped, because the next drive folds this event into "+
			"its starting state and keeps no effects from it.\n"+
			"  nothing was written. clear the halt, then write the key:\n"+
			"    %s\n"+
			"    arxi state set %s %s %s\n",
			pre.RunID, pre.Status, strings.Join(tools, ", "),
			haltRemedy(pre), pre.RunID, key, strconv.Quote(value))
		os.Exit(1)
	}

	store, err := logstore.Open(dir)
	if err != nil {
		fatal(err)
	}
	// Both, and not redundant: the defer covers ordinary returns and atExit covers
	// os.Exit, which runs no defers. Measured on `run unpause`, where the missing
	// half left writer.lock holding a dead pid and bricked the run.
	defer store.Close()
	atExit(func() { store.Close() })

	ev := kernel.Event{
		// The seq is assigned by the writer, so the id is built off the head -- the
		// same scheme as "emit-<n>" and "prompt-<n>".
		ID:   "state-set-" + strconv.FormatInt(store.Head()+1, 10),
		Type: kernel.StateSet,
		// SourceHuman, and load-bearing twice over. wakeWatchers is skipped outright
		// for SourceRuntime -- stamping this runtime would make the command append a
		// key that provably wakes nobody -- and an audit that cannot say a person
		// set this value cannot explain why the run turned.
		Source: kernel.SourceHuman,
		Scope:  "run:" + pre.RunID,
		// Actor is left EMPTY, deliberately. wakeWatchers skips a watcher whose agent
		// equals the actor unless include_self, so claiming an actor here would
		// silently disable that agent's own watcher on state.*. No member set this
		// key; a shell did.
		Ts:      nowFunc().UTC().Format(time.RFC3339),
		Payload: map[string]any{"key": key, "value": value},
	}

	var written []kernel.Event
	if ifSeq >= 0 {
		written, err = store.AppendIfSeq(ifSeq, []kernel.Event{ev})
	} else {
		written, err = store.Append([]kernel.Event{ev})
	}
	if err != nil {
		var cas *logstore.CASError
		if errors.As(err, &cas) {
			refuseStaleCAS("state set", pre.RunID, cas, store)
		}
		fatal(fmt.Errorf("record %s: %w", kernel.StateSet, err))
	}
	at := written[0].Seq

	// The value is echoed, quoted. It is the one way to see that an empty value
	// landed as an empty value rather than as a lost argument, and %q also keeps a
	// value containing a newline from breaking the line it is printed on.
	fmt.Printf("run %s set %s = %s (seq %d)\n",
		pre.RunID, key, strconv.Quote(value), at)
	printStateSetOutlook(pre, key, watch, acts)

	// Two reasons not to drive, and they are different. No watcher acts: driving
	// would run the loop from the tip, find no pending work and print a summary
	// suggesting the write did something. The run is halted: the cause is parked in
	// the state, and driving a paused run is precisely what `run unpause` is for --
	// this command must not resume spending as a side effect of storing a key.
	if !acts || spendingHalted(pre) {
		return
	}
	if simulated {
		fmt.Printf("  this run was started with --sim, so the turn is taken by the " +
			"same fake executor: no model is called and no money is spent.\n")
	}
	driveResumedRun(dir, cfg, store, pre.RunID, simulated)
}

// checkStateKey refuses the keys that would be written and never read.
//
// Like checkCustomType, these refuse values the REDUCER would take, so each one
// has to earn it by naming what breaks rather than by stating a rule.
func checkStateKey(k string) error {
	if strings.TrimSpace(k) == "" {
		return fmt.Errorf("which key? the reducer drops a state.set with an empty " +
			"key rather than storing it, so this would append an event, report " +
			"success and leave the store exactly as it was -- and a value filed " +
			"under no name is one `state get` has no way to ask for")
	}
	if k != strings.TrimSpace(k) {
		return fmt.Errorf("the key %q is padded with whitespace, and nothing "+
			"downstream can see that: the store is an exact lookup, so `arxi state "+
			"get <run> %s` would answer nothing for a key this command had just "+
			"reported as written", k, strings.TrimSpace(k))
	}
	if strings.ContainsAny(k, "\n\r\t") {
		return fmt.Errorf("the key %q contains a line break or a tab; every reader "+
			"of the store prints one key per line, so a key that carries a newline "+
			"is a key no output can be read back into", k)
	}
	return nil
}

// haltRemedy is the command that clears a halt, and which one it is depends on
// why the run stopped. Paused is a decision somebody took and can undo; blocked
// is a fact about the run -- a full budget, a held lock, an unanswered question --
// and `run unpause` on it would either be refused or immediately re-breach, so the
// remedy is the command that says what the block is.
func haltRemedy(st kernel.State) string {
	if st.Status == kernel.StatusBlocked {
		return "arxi run why " + st.RunID
	}
	return "arxi run unpause " + st.RunID
}

// printStateSetOutlook says what the write did BEYOND storing the key.
//
// It does not reuse printEmitOutlook, and the no-watcher case is why. An emit
// nothing matches is "recorded, and observed by nobody" -- true there, because a
// custom.* event has no other effect. The same sentence about a state.set would be
// false: the key is in the state whether or not anybody watches, and `state get`
// answers with it. Printing "observed by nobody" would read as a failed write, and
// the user's next move would be to write it again.
func printStateSetOutlook(st kernel.State, key string, watch []emitWatcherOutcome, acts bool) {
	readBack := fmt.Sprintf("  read it back: arxi state get %s %s\n", st.RunID, key)

	// Asked BEFORE the per-watcher notes, and that order is what causeOutlook
	// records having measured: spawnCauses consults spendingHalted before it looks
	// at the member, so a line computed from the member alone announces a turn the
	// reducer never opens. On a halted run every acting watcher is parked,
	// whatever its own note says.
	if spendingHalted(st) {
		if acts {
			fmt.Printf("  stored, but run %s is %s, so the turn is parked rather than "+
				"started -- it is handed back when that clears.\n", st.RunID, st.Status)
			fmt.Printf("  clear it: %s\n", haltRemedy(st))
		} else {
			fmt.Printf("  stored. run %s is %s, and nothing was waiting on this key "+
				"anyway.\n", st.RunID, st.Status)
		}
		fmt.Print(readBack)
		return
	}

	if len(watch) == 0 {
		// The type is fixed at state.set, so the run's other patterns are not listed
		// the way `event emit` lists them: there is no type here for the user to have
		// mistyped, and a list of patterns they cannot act on would only invite the
		// guess that the KEY is what has to match one. What is actionable is the
		// declaration they are missing.
		fmt.Printf("  stored, and nothing was waiting on it: this run's blueprint "+
			"declares no watcher matching %s, so no turn opens.\n", kernel.StateSet)
		fmt.Print("  a member that should react to a key landing is declared with: " +
			"watchers: [{agent: <name>, pattern: state.*}]\n")
		fmt.Print(readBack)
		return
	}
	for _, o := range watch {
		fmt.Printf("  %s\n", o.note)
	}
	if !acts {
		fmt.Print("  so nothing starts now: the key is stored and the run is left " +
			"where it was.\n")
	}
	fmt.Print(readBack)
}
