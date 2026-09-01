package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/logstore"
	"github.com/michiTrader/arxi/internal/surface"
)

// `arxi run pause <run>` -- stop opening paid turns, and be exact about what
// that does not stop.
//
// # One append, and no loop to drive
//
// This is the shortest mutating verb in the binary, and the reason is in the
// reducer: `case RunPaused` sets Status and returns NO effects
// (internal/kernel/decide.go:46). There is nothing to hand to the effect runner,
// so unlike `run unpause` -- which appends and then drives the loop to the next
// quiescence -- pause is finished the moment the byte hits the log.
//
// What makes that one byte worth anything is spendingHalted
// (internal/kernel/decide.go:1176), which is true for Blocked AND Paused. Every
// site that would open a turn asks it first and parks the cause instead, and
// unpause's drainParked hands the parked causes back. Its doc comment states the
// requirement this verb exists to meet: "a `run pause` that keeps paying for
// turns is not a pause".
//
// # What it cannot do, measured rather than promised
//
// A pause does not reach into a turn that is already open. There is no daemon:
// a turn runs inside whichever process is driving the loop, and this process is
// not that one. That is not a gap this command papers over -- it is printed,
// because "paused" that a reader takes to mean "stopped mid-call" is the reading
// that costs them money they thought they had saved.
//
// It also cannot pause a run that is being driven right now. The driver holds
// the run's writer.lock (one writer per log, ADR-0006), so logstore.Open fails
// with *LockedError and this command refuses. The refusal is translated rather
// than printed raw, and that is a safety fix, not a style one: LockedError's own
// text ends with "remove <dir>/writer.lock by hand after confirming no process
// is running", which is sound advice for a stale lock and dangerous advice here,
// where the writer is alive by definition -- the pause failed BECAUSE it is
// alive. An operator who deletes a live lock gets two writers, duplicate seq,
// and a log that no longer folds.
const runPauseUsage = "usage: arxi run pause <run>\n" +
	"  it stops the NEXT turn being opened; a turn already open is not aborted\n" +
	"  resume it later: arxi run unpause <run>\n" +
	"  see what exists: arxi run list\n"

func cmdRunPause(args []string) {
	c := surface.Lookup("run", "pause")
	vals, err := parseInvocation(c, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi run pause: %v\n\n%s", err, runPauseUsage)
		os.Exit(2)
	}

	// Trimmed, and checked separately from the missing-positional case that
	// parseInvocation catches. `arxi run pause "$RUN"` with RUN unset DID pass a
	// value, so the parser is satisfied; without this it reaches resolveRunDir
	// and reports the runs directory itself as an unreadable run.
	runArg := strings.TrimSpace(vals["run"])
	if runArg == "" {
		fmt.Fprint(os.Stderr, "arxi run pause: which run?\n\n"+runPauseUsage)
		os.Exit(2)
	}

	dir := resolveRunDir(runArg)

	// The state is read BEFORE the store is opened, and the order is deliberate
	// in one specific way: logstore.Open MkdirAll's its directory, so opening
	// first would answer a typo'd run id by silently creating runs/<typo>/ and
	// leaving it behind. Folding first means a name that is not a run is
	// reported as one, not manufactured into one.
	pre, _, simulated, err := foldRunDir(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi run pause: %v\n", err)
		os.Exit(1)
	}

	id := pre.RunID
	if id == "" {
		id = filepath.Base(dir)
	}

	// Refusals, by the same reasoning cmdRunUnpause refuses a run that is
	// already running: a second run.paused is harmless to the reducer and
	// dishonest in the log. It records a human intervening at a moment when
	// nothing changed, and `event trace` would show a decision that decided
	// nothing.
	switch {
	case pre.Status.Terminal():
		fmt.Fprintf(os.Stderr, "arxi run pause: run %s is %s, which is final.\n"+
			"  a finished run is not spending anything, so there is nothing to "+
			"pause. what it ended with: arxi run result %s\n", id, pre.Status, id)
		os.Exit(1)
	case pre.Status == kernel.StatusPaused:
		fmt.Fprintf(os.Stderr, "arxi run pause: run %s is already paused "+
			"(seq %d), so nothing would change.\n"+
			"  pick it back up: arxi run unpause %s\n", id, pre.Seq, id)
		os.Exit(1)
	}

	store, err := logstore.Open(dir)
	if err != nil {
		var locked *logstore.LockedError
		if errors.As(err, &locked) {
			// Deliberately NOT locked.Error(): see the note above about the
			// "remove writer.lock by hand" advice, which is exactly wrong for a
			// writer that is alive.
			fmt.Fprintf(os.Stderr, "arxi run pause: run %s is being driven right "+
				"now by %s, so it cannot be paused from here.\n"+
				"  one writer per log: the process holding it is the only thing "+
				"that may append, and this command is not it.\n"+
				"  stop that process (Ctrl-C) -- it stops at the next quiescence "+
				"and the run stays resumable.\n"+
				"  do NOT delete %s: two writers produce duplicate seq and a log "+
				"that no longer folds.\n",
				id, locked.Owner, filepath.Join(dir, "writer.lock"))
			os.Exit(1)
		}
		fatal(err)
	}
	// Both, and not redundant: the defer covers the ordinary return, atExit
	// covers os.Exit, which does not run defers. Close is idempotent, so
	// whichever fires first is the one that matters.
	defer store.Close()
	atExit(func() { store.Close() })

	ev := kernel.Event{
		ID:   "pause-" + strconv.FormatInt(store.Head()+1, 10),
		Type: kernel.RunPaused,
		// SourceHuman: a pause is an instruction, and an audit that cannot tell
		// a human's pause from the runtime blocking a run cannot answer why the
		// bill stopped growing.
		Source: kernel.SourceHuman,
		Scope:  "run:" + id,
		// Stamped here because nothing else will -- this append does not go
		// through the effect runner. Measured omission: inbox replies landed
		// with "ts":"" for exactly this reason.
		Ts: nowFunc().UTC().Format(time.RFC3339),
		// No payload, and none may be invented: spec/events.md:39 declares
		// run.paused with "—". A --reason flag would need a key no producer
		// writes and no consumer reads, which is a private convention wearing
		// the catalogue's clothes.
		Payload: map[string]any{},
	}

	// AppendIfSeq, not Append. The refusals above were decided from a state
	// folded at pre.Seq, and this makes the append conditional on the log still
	// being there. Without the CAS, two `run pause` invocations racing on the
	// same run both read "running" and both append, and the log records two
	// human pauses of a run that was only paused once. State.Seq is the last
	// folded event's seq (decide.go:28), which is the head of a complete log.
	written, err := store.AppendIfSeq(pre.Seq, []kernel.Event{ev})
	if err != nil {
		var cas *logstore.CASError
		if errors.As(err, &cas) {
			fmt.Fprintf(os.Stderr, "arxi run pause: run %s moved while this "+
				"command was deciding (it was at seq %d, the log is at seq %d).\n"+
				"  nothing was written. the state the refusals were checked "+
				"against is stale, so run it again: arxi run pause %s\n",
				id, cas.Expected, cas.Actual, id)
			os.Exit(1)
		}
		fatal(fmt.Errorf("record run.paused: %w", err))
	}

	sim := ""
	if simulated {
		sim = "  [simulated]"
	}
	fmt.Printf("run %s paused at seq %d (%s)%s\n", id, written[0].Seq,
		pauseTurnsPhrase(pre), sim)

	printPauseEffect(pre, id)

	// Last, and only on a pause that actually happened. An earlier draft warned
	// before opening the store, which printed a paragraph about what the pause
	// had masked on runs where the pause was then refused for a held lock.
	if pre.Status == kernel.StatusBlocked {
		printPauseMasking(pre, id)
	}
}

// openTurns names the members whose turn is recorded as still open.
//
// Members rather than the run-level counter, because "how many" is not the
// useful half: a reader who is told one turn is open wants to know whose, since
// that is the member they have to go and look at.
func openTurns(st kernel.State) []string {
	var open []string
	for _, m := range st.Members {
		if m.TurnOpen {
			open = append(open, m.Name)
		}
	}
	return open
}

// pauseTurnsPhrase is the parenthetical in the headline.
//
// docs/design/20-use-cases.md §20.6 promises this line as "run r1 paused at seq
// 52 (2 turns finished, none interrupted)", and getting there needs one
// subtraction that is easy to miss.
//
// MEASURED in the reducer: State.Turns is incremented by applyActivated
// (internal/kernel/decide.go:296) at the moment a turn OPENS, and nothing
// decrements it -- agent.turn_done only clears Member.TurnOpen (:701). So Turns
// counts turns STARTED, and printing it as "turns finished" credits the run with
// completing work that is still in flight. On the fixture with one activation and
// no turn_done it read "1 turn finished, backend still open", which is a single
// line contradicting itself.
//
// Finished is therefore Turns minus the turns still open. "none interrupted" is
// kept only for the case it is true of: printed as a constant beside an open
// turn it would be the one false sentence in the output, and the one a reader
// would rely on to mean the call stopped.
func pauseTurnsPhrase(st kernel.State) string {
	open := openTurns(st)

	done := st.Turns - len(open)
	if done < 0 {
		// Not reachable from a log this binary writes, and clamped rather than
		// trusted: a negative count in a headline reads as a broken command, and
		// the honest floor for "how many finished" is none.
		done = 0
	}

	turns := fmt.Sprintf("%d turns finished", done)
	switch done {
	case 0:
		turns = "no turns finished"
	case 1:
		turns = "1 turn finished"
	}

	if len(open) == 0 {
		return turns + ", none interrupted"
	}
	return fmt.Sprintf("%s, %s still open and NOT interrupted",
		turns, strings.Join(open, ", "))
}

// printPauseEffect says what the pause bought and what it left alone.
//
// Separate from the headline because it is the part a reader needs on the run
// where they were wrong about what pause means. It does not moralise and it does
// not repeat the headline: each line is a fact the headline does not carry.
func printPauseEffect(st kernel.State, id string) {
	fmt.Print("  no new turn will be opened: pause halts spending the same way " +
		"a block does\n")

	if open := openTurns(st); len(open) > 0 {
		// This command holds the writer lock, which is how it can say this at
		// all: if nothing else may append, nothing else is driving, so the open
		// turn is not work in progress somewhere. It is a member waiting, or the
		// process that opened it stopped without closing it.
		fmt.Printf("  %s's turn stays open -- a pause does not abort one, and "+
			"nothing here can:\n"+
			"    no process was driving this run (pausing it required its writer "+
			"lock), so no call is being cut off.\n"+
			"    who owes what: arxi run why %s\n",
			strings.Join(open, ", "), id)
	}

	if n := pendingAsks(st); n > 0 {
		// Worth its own line because a paused run with an open question looks
		// idle for two different reasons at once, and answering the question
		// will not restart it.
		fmt.Printf("  %d unanswered question(s) are still waiting: answering one "+
			"does not resume the run, arxi run unpause %s does\n", n, id)
	}

	fmt.Printf("  resume it: arxi run unpause %s\n", id)
}

// printPauseMasking warns that pausing a blocked run hides why it was stuck.
//
// The block does not go away -- the exhausted budget, the unanswered question
// and the parked causes are all still in the state -- but Status is overwritten,
// and Status is what `run why` branches on first (internal/kernel/why.go:44
// returns early for StatusPaused). So the diagnosis is printed HERE, quoting the
// state as it was one event ago, because this is the last moment anything in the
// binary can still name it.
//
// The sibling half of this defect was in unpause: its exhausted-budget warning
// was gated on Status == Blocked, so a run that had been paused while blocked
// resumed with no warning at all and re-blocked on the next cost.
func printPauseMasking(st kernel.State, id string) {
	fmt.Printf("warning: run %s was blocked, and the pause has replaced that "+
		"status.\n"+
		"  arxi run why %s now reports \"paused by explicit request\" and stops, "+
		"rather than naming the block.\n", id, id)

	switch {
	case budgetIsExhausted(st):
		fmt.Printf("  what it was blocked on, as of the event before the pause: "+
			"the budget ran out, %s of %s USD in the tree.\n"+
			"  a pause does not raise it: arxi run unpause %s --budget <higher>\n",
			usd(st.TreeSpentUSD), usd(st.BudgetUSD), id)
	case pendingAsks(st) > 0:
		fmt.Printf("  what it was blocked on, as of the event before the pause: "+
			"%d unanswered question(s), see arxi inbox\n", pendingAsks(st))
	default:
		fmt.Printf("  what it was blocked on is in the log rather than in this "+
			"message: arxi run show %s\n", id)
	}
}
