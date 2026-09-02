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

// injection is the whole difference between `run prompt` and `run steer`: a
// noun, a participle, and the event type that lands in the log.
//
// # Why the two verbs share a body
//
// ADR-0005 has one injection mechanism, and that is not a figure of speech: in
// the reducer, run.prompt, agent.steered and agent.notified reach the same
// applyInjection, accumulate in the same PendingCauses, and drain into the same
// coalesced SpawnTurn. A CLI that implemented them twice would be free to
// disagree with that in ways no reducer test can see -- one verb refusing a
// paused run and the other parking a cause silently, one driving the run and
// the other leaving the cause to evaporate.
//
// The cost of copying is measured rather than hypothetical. `run prompt`
// shipped four defects that were found by running it: a cause appended and then
// discarded, a closing line advising a command that refuses, a wrong outlook on
// a halted run, and a refused CAS that exited holding the writer lock. A
// duplicated body would have carried all four into a second file, where each
// would have to be found a second time.
//
// # What genuinely differs
//
// Provenance. `prompt` records "here is something new to do"; `steer` records
// "what you are doing needs correcting". The reducer treats them identically on
// purpose, and the log keeps them apart so `event trace` can say which one
// caused a turn -- a change of direction, or a new requirement. That question is
// worth two event types even where the behaviour is one.
type injection struct {
	verb string           // the CLI verb: "prompt", "steer"
	typ  kernel.EventType // what is appended, and so what `event trace` shows
	past string           // the banner's participle: "prompted", "steered"

	// noun and is complete "a <noun> is <is>" in the refusal of an empty
	// message, which has to name what the caller failed to supply. "the message
	// is empty" on its own tells somebody who typed the wrong verb nothing.
	noun string
	is   string

	// example stands in for a message in the usage line: what a caller who got
	// the arguments wrong most likely meant to say.
	example string
}

// injectCause implements `arxi run <verb> <run> <text> [--to X] [--if-seq N]`.
//
// # This is a writer, and the first kind built here that is not a resume
//
// `run list`, `run show` and `run why` were projections: they read a log and
// could not corrupt it. This appends. The difference shows up in what has to be
// checked BEFORE the write, because after it the run has already been told
// something and an append-only log offers no way to unsay it.
//
// # Why the CAS is offered and not imposed
//
// --if-seq is ADR-0006's compare-and-swap. It is optional here, and defaulting
// it to "whatever the log is at right now" would be worse than leaving it off:
// that value is read microseconds before the append, so it would pass virtually
// always and give a caller the feeling of a guard without the guard. A CAS is
// only meaningful when the expected seq comes from the state the CALLER actually
// looked at -- the seq printed by `run show` or `run why`, minutes ago, before
// they decided what to say.
//
// So: no flag means "append regardless", which is what a human at a terminal
// almost always means. A flag means "only if nothing has happened since I
// looked", which is what a script or an agent means.
func injectCause(in injection, args []string) {
	c := surface.Lookup("run", in.verb)
	vals, err := parseInvocation(c, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi run %s: %v\n\n"+
			"usage: arxi run %s <run> <text> [--to <member>] [--if-seq <n>]\n",
			in.verb, err, in.verb)
		os.Exit(2)
	}

	runArg := strings.TrimSpace(vals["run"])
	text := vals["text"]
	if runArg == "" {
		fmt.Fprintf(os.Stderr, "arxi run %s: which run?\n"+
			"  usage: arxi run %s <run> <text>\n"+
			"  see what exists: arxi run list\n", in.verb, in.verb)
		os.Exit(2)
	}

	// An empty message is refused rather than appended. The reducer would take
	// it happily -- applyInjection keys off the event ID, not the text -- and
	// spawn a paid turn whose new context is a blank string. That is money spent
	// to tell an agent nothing, and the log would record a human intervening
	// with no content, which is unreadable afterwards.
	if strings.TrimSpace(text) == "" {
		fmt.Fprintf(os.Stderr, "arxi run %s: the message is empty.\n"+
			"  a %s is %s, so an empty one buys a turn that has nothing to read.\n"+
			"  usage: arxi run %s %s %q\n",
			in.verb, in.noun, in.is, in.verb, runArg, in.example)
		os.Exit(2)
	}

	// --if-seq is parsed BEFORE the run is opened, so a malformed guard is
	// reported without having touched the log. The same ordering as unpause's
	// --budget, and for the same reason: refusing a write after making it is not
	// an option an append-only log offers.
	var ifSeq int64 = -1
	if raw, ok := vals["if-seq"]; ok && raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "arxi run %s: --if-seq %q is not a seq.\n"+
				"  it is the seq you last saw, so the write is refused if the run "+
				"moved since: e.g. --if-seq 14\n"+
				"  the current seq is printed by: arxi run show %s\n",
				in.verb, raw, runArg)
			os.Exit(2)
		}
		if n < 0 {
			fmt.Fprintf(os.Stderr, "arxi run %s: --if-seq %d is not a seq a log "+
				"can be at.\n  omit the flag to append regardless of where the run "+
				"is.\n", in.verb, n)
			os.Exit(2)
		}
		ifSeq = n
	}

	dir := resolveRunDir(runArg)

	// The state is read BEFORE the append, because every refusal below depends
	// on what the run was, and afterwards it has already been told something.
	pre, cfg, simulated, err := foldRunDir(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi run %s: %v\n", in.verb, err)
		os.Exit(1)
	}

	// A finished run is refused. The reducer would accept the event and
	// applyInjection would even spawn a turn, because it does not consult
	// Status -- so this is a real guard and not a formality. Appending here
	// would resurrect a run that had reached a terminal state, and `event trace`
	// would show work happening after the result was recorded.
	if pre.Status.Terminal() {
		fmt.Fprintf(os.Stderr, "arxi run %s: run %s is %s, which is final.\n"+
			"  a finished run cannot take a new cause: its result is already "+
			"recorded, and work after that point would contradict it.\n"+
			"  to continue from here: arxi run fork %s --at-seq %d\n",
			in.verb, pre.RunID, pre.Status, pre.RunID, pre.Seq)
		os.Exit(1)
	}

	// A paused run is refused, with the command that fixes it. This one is worth
	// stating precisely: the append would succeed and the cause would NOT be
	// lost -- spendingHalted parks it and drainParked hands it back on unpause.
	// But the run would sit there having been told something and doing nothing,
	// which looks exactly like this command failing. Saying so up front is
	// cheaper than a silent no-op.
	if pre.Status == kernel.StatusPaused {
		fmt.Fprintf(os.Stderr, "arxi run %s: run %s is paused, so a new cause "+
			"would be parked rather than acted on.\n"+
			"  resume it first: arxi run unpause %s\n",
			in.verb, pre.RunID, pre.RunID)
		os.Exit(1)
	}

	// The recipient is resolved and CHECKED here, against the members the run
	// actually has. The reducer cannot do this: applyInjection loops over
	// members and skips the ones that do not match, so a --to naming a member
	// who does not exist is a silent no-op that appends an event, prints success,
	// and does nothing at all. That is the worst available outcome, because the
	// user believes they have steered the run.
	if to := strings.TrimSpace(vals["to"]); to != "" && to != "*" {
		if pre.Member(to) == nil {
			names := make([]string, 0, len(pre.Members))
			for _, m := range pre.Members {
				names = append(names, m.Name)
			}
			fmt.Fprintf(os.Stderr, "arxi run %s: run %s has no member %q.\n"+
				"  it has: %s\n"+
				"  omit --to to use the blueprint's steer target, or --to '*' to "+
				"reach everybody who is participating.\n",
				in.verb, pre.RunID, to, strings.Join(names, ", "))
			os.Exit(1)
		}
	}

	// --on-busy is declared and NOT honoured, so saying so is the only honest
	// option. The reducer implements exactly one behaviour: applyInjection
	// appends to PendingCauses when the recipient is busy, which is `queue`.
	// `reject` and `steer` are in the surface and nothing reads them.
	//
	// Accepting the flag silently is the specific failure serve.go's
	// validateParams was written to prevent -- "a misspelled guard that is
	// silently dropped makes a request the client believed was safe unsafe". A
	// dropped `reject` is worse than a misspelling: the caller asked NOT to
	// disturb a busy member and would be told it worked.
	if ob := strings.TrimSpace(vals["on-busy"]); ob != "" && ob != "queue" {
		fmt.Fprintf(os.Stderr, "arxi run %s: --on-busy %q is declared in the "+
			"surface but not implemented: the reducer always queues.\n"+
			"  a busy member accumulates the cause in pending_causes and drains it "+
			"when its turn finishes, so nothing is lost -- but nothing is rejected "+
			"either.\n", in.verb, ob)
		if ob == "steer" {
			// `steer` is not merely unbuilt, and the distinction earns its two
			// lines: it is ADR-0005's discarded alternative, word for word --
			// "interrupt the running turn and restart it with the new context",
			// discarded "because it throws away work already paid for". Told only
			// "not implemented", a reader would wait for it.
			fmt.Fprintf(os.Stderr, "  this one is not a gap waiting to be filled: "+
				"interrupting a running turn and restarting it with the new context "+
				"is the alternative ADR-0005 discarded, because it throws away work "+
				"that has already been paid for.\n")
		}
		fmt.Fprintf(os.Stderr, "  re-run with --on-busy queue to say that "+
			"explicitly, or omit the flag.\n")
		os.Exit(2)
	}

	store, err := logstore.Open(dir)
	if err != nil {
		fatal(err)
	}
	// Both, and not redundant: the defer covers ordinary returns, atExit covers
	// os.Exit, which does not run defers -- and there are exit paths below this
	// line. Same reasoning as unpause, where a stale writer.lock was measured.
	defer store.Close()
	atExit(func() { store.Close() })

	payload := map[string]any{"text": text}
	if to := strings.TrimSpace(vals["to"]); to != "" {
		payload["to"] = to
	}

	ev := kernel.Event{
		// The seq is not known yet, so the id is built from the head. Same scheme
		// as unpause's "unpause-<n>", and the verb is in it so a log that carries
		// both kinds can be read without consulting the type column.
		ID:   in.verb + "-" + strconv.FormatInt(store.Head()+1, 10),
		Type: in.typ,
		// SourceHuman, and load-bearing. A new cause injected from outside is the
		// one kind of event that has no antecedent in the log, so an audit that
		// cannot say a human put it there cannot explain why the run changed
		// direction.
		Source: kernel.SourceHuman,
		Scope:  "run:" + pre.RunID,
		// Ts is stamped here because nothing else will: this append does not go
		// through the effect runner. Measured on real logs for inbox replies,
		// which landed with "ts":"".
		Ts:      nowFunc().UTC().Format(time.RFC3339),
		Payload: payload,
	}

	var written []kernel.Event
	if ifSeq >= 0 {
		written, err = store.AppendIfSeq(ifSeq, []kernel.Event{ev})
	} else {
		written, err = store.Append([]kernel.Event{ev})
	}
	if err != nil {
		// A rejected CAS is not a failure of the disk, and saying so is the whole
		// reason CASError is a distinct type carrying Actual. The caller is told
		// what it missed and how to look at it, because re-reading is the correct
		// response and retrying blindly is not.
		var cas *logstore.CASError
		if errors.As(err, &cas) {
			fmt.Fprintf(os.Stderr, "arxi run %s: not appended -- the run moved.\n"+
				"  you guarded on seq %d and run %s is at seq %d, so %d event(s) "+
				"happened since you looked.\n"+
				"  nothing was written. read what changed and decide again:\n"+
				"    arxi run show %s\n"+
				// --since-seq, not --from-seq: the registry declares since-seq on
				// `event log`, and the flag is INCLUSIVE (event.go:208 keeps
				// e.Seq >= sinceSeq), so Expected+1 is the first event the caller
				// has not seen. Printing a flag the recommended command refuses is
				// the same defect as the `run fork --from-seq` hints, and it is
				// worse here because it appears in a message whose whole purpose is
				// to tell somebody how to catch up.
				"    arxi event log %s --since-seq %d\n",
				in.verb, cas.Expected, pre.RunID, cas.Actual,
				cas.Actual-cas.Expected, pre.RunID, pre.RunID, cas.Expected+1)

			// Closed explicitly before exiting, and this was MEASURED as a defect,
			// not added defensively. os.Exit runs neither the deferred Close above
			// nor the atExit hooks -- only fatal() calls runExitHooks, and this
			// branch does not go through fatal because a rejected CAS is not an
			// internal error.
			//
			// The consequence, walked by hand: one `run prompt --if-seq <stale>`
			// left writer.lock holding a dead pid, and EVERY later command on that
			// run was refused with advice to delete a lock file by hand -- for a run
			// that had merely been guarded correctly. A CAS miss is the most
			// ordinary failure this command has; it must not be the one that bricks
			// the run.
			store.Close()
			os.Exit(1)
		}
		fatal(fmt.Errorf("record %s: %w", in.typ, err))
	}
	at := written[0].Seq

	target := kernelSteerTargetFor(pre, cfg, vals["to"])
	fmt.Printf("run %s %s (seq %d), to %s\n", pre.RunID, in.past, at, target)

	// What happens NEXT is stated, because it differs by the recipient's state
	// and the difference is the whole behaviour of this command. A user who
	// injects a cause into a busy member and sees nothing happen has no way to
	// tell "queued" from "ignored" -- and those are the two outcomes the declared
	// --on-busy is about.
	if note := causeOutlook(pre, target); note != "" {
		fmt.Printf("  %s\n", note)
	}

	// The run is DRIVEN here. This reverses the decision the first draft of `run
	// prompt` shipped with, and the reversal was forced by measuring what that
	// draft actually did.
	//
	// # What the first draft got wrong
	//
	// It appended the cause and returned, on the reasoning that `arxi inbox` sets
	// the precedent: answering a question is not the same act as paying for the
	// turns the answer unblocks. That reasoning is sound for the inbox and false
	// here, and the difference is what the append LEAVES BEHIND.
	//
	// An inbox reply is durable in the state: the fold sets Replied=true, so a
	// user who answers and stops can see that they answered, and the next drive
	// finds the answer waiting. A cause injected into a member who is neither
	// busy nor waiting leaves NOTHING. applyInjection sends it straight to
	// spawnCauses, and spawnCauses parks it only when spendingHalted -- which is
	// false on a running run. On every other path it returns a transient
	// SpawnTurn effect that lives in Decide's return value and is discarded if
	// nobody is executing effects.
	//
	// Measured, on the exact run `run why` was pointing at:
	//
	//	$ arxi run prompt <run> "please submit what you have"
	//	run <run> prompted (seq 16), to backend
	//	$ arxi run why <run>
	//	└─ nobody is working and nobody can start: the run is quiescent
	//
	// A prompted run and an un-prompted one were indistinguishable, and `run why`
	// went on printing the very remedy the user had just followed. That is worse
	// than the gap it was written to close: a refusal at least tells the truth.
	//
	// # Why the closing line could not fix it either
	//
	// The draft ended with "drive it: arxi run unpause <run>". That command
	// REFUSES a running run -- "already running, so there is nothing to resume",
	// exit 1 -- and quiescence is a running state. So the advice was itself a
	// printed-and-refused gap, printed by the command written to close the fifth
	// one. There is no other verb that drives. Not driving here does not defer the
	// decision to the user; it removes it.
	//
	// # What driving costs, and why that is the right price
	//
	// It spends money, which is exactly what the inbox reasoning was protecting
	// against. The protection is kept where it belongs: this command refuses
	// paused and terminal runs up front, and a budget-exhausted run gets a
	// BudgetSlice of 0 from spawnFor, so the loop stops on the ceiling rather than
	// spending past it. What a user cannot do is ask for work and then be unable
	// to get it.
	if simulated {
		fmt.Printf("  this run was started with --sim, so the turn is taken by the " +
			"same fake executor: no model is called and no money is spent.\n")
	}
	driveResumedRun(dir, cfg, store, pre.RunID, simulated)
}

// kernelSteerTargetFor answers "who will actually hear this".
//
// The blueprint's steer_target is resolved rather than echoed, because "" is
// what the user typed and it is not an answer: it could mean the coordinator, a
// named slot, or a broadcast. Printing the resolved name is what lets somebody
// notice they addressed the wrong member before paying for the turn.
//
// The resolution rule is DUPLICATED from kernel.resolveSteerTarget, which is
// unexported. The alternative is exporting a reducer helper so a printer may
// borrow it, which widens the frozen surface for a display concern -- the same
// trade taken for spendingHalted in runshow.go. The risk is that the two drift;
// it is named here and pinned by a test that injects a cause into a run with a
// coordinator and checks the printed name.
func kernelSteerTargetFor(st kernel.State, c kernel.Config, explicit string) string {
	if e := strings.TrimSpace(explicit); e != "" {
		return e
	}
	t := c.Inter.SteerTarget
	switch {
	case t == "broadcast":
		return "*"
	case strings.HasPrefix(t, "slot:"):
		return strings.TrimPrefix(t, "slot:")
	case t == "coordinator":
		for _, m := range st.Members {
			if m.Role == "coordinator" {
				return m.Name
			}
		}
		for _, m := range st.Members {
			if !m.Advisory {
				return m.Name
			}
		}
	}
	return t
}

// causeOutlook says what the recipient will do with this, in their own state.
//
// The RUN's state is consulted before the member's, and that order was measured
// rather than reasoned: prompting a budget-blocked run printed "it opens a turn
// for backend now" and then parked the cause instead, because spawnCauses checks
// spendingHalted before it looks at the member at all. A member who is idle by
// every visible sign still cannot be given a turn while the run is halted, so a
// line that reads the member alone describes a decision the reducer never makes.
func causeOutlook(st kernel.State, target string) string {
	// Ahead of every member rule below, including the broadcast: spendingHalted
	// is the reducer's first question and nothing downstream of it can override
	// the answer.
	if spendingHalted(st) {
		return "it is parked, not started: the run is " + string(st.Status) +
			", so no turn opens until that clears -- the cause is handed back then"
	}
	if target == "*" {
		return "it reaches everybody participating; whoever is busy queues it until " +
			"their turn ends"
	}
	m := st.Member(target)
	if m == nil {
		// Reachable when the blueprint's steer_target names nobody in this run.
		// Not an error here: the event is already written, and the reducer will
		// simply match no member. Saying so is better than printing success and
		// leaving the user to discover the silence.
		return "warning: no member of this run is named " + target + ", so nothing " +
			"will act on it. check steer_target in the blueprint, or use --to"
	}
	switch {
	case m.Busy():
		return "it is queued: " + target + " is mid-turn, and the cause is drained " +
			"when that turn finishes"
	case m.State == kernel.MemberWaiting:
		return "it is queued: " + target + " is blocked (" + m.Detail + "), and the " +
			"cause is drained once that clears"
	case m.State == kernel.MemberFailed:
		return "warning: " + target + " has failed, and a failed member takes no new " +
			"causes. name somebody else with --to"
	}
	return "it opens a turn for " + target + " now"
}
