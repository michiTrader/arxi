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

// `arxi run cancel <run> [--reason <why>]` -- end a run for good, and say what
// that costs.
//
// # The same one append as pause, and a very different act
//
// Mechanically this is `run pause` again: `case RunCancelled` sets Status and
// returns no effects (internal/kernel/decide.go:59), so there is no loop to
// drive and the command is finished when the byte lands. Everything that makes
// it a different command is in what the status MEANS.
//
// StatusCancelled is terminal (internal/kernel/state.go:29), and terminal is
// enforced at the top of the reducer: an event arriving at a terminal run is
// folded into nothing (decide.go:32). So this append does not stop the run the
// way a pause does -- it makes every later event in this log inert. There is no
// `run uncancel` in the surface and there should not be one: the log is
// append-only, and a status that could be taken back would not be terminal.
//
// # Why --reason is optional in the surface and asked for here
//
// docs/design/20-use-cases.md:407 is explicit: the reason "lands in the log, so
// `run list` six weeks later distinguishes a run that was abandoned from one that
// failed. Both look identical without it." The surface declares it optional, so
// this command cannot require it -- but it can say, once, at the moment it is
// still possible to supply, that the log cannot be edited afterwards.
//
// The payload key is `reason`, which is not this command's invention twice over:
// spec/events.md:41 declares `run.cancelled` with `reason?`, and
// cancelReason (runresult.go:261) already reads that key. Writing anything else
// would produce a cancelled run whose `run result` has nothing to show.
const runCancelUsage = "usage: arxi run cancel <run> [--reason <why>]\n" +
	"  it is FINAL: a cancelled run cannot be resumed, and every later event\n" +
	"  in its log is ignored by the reducer\n" +
	"  a turn already open is not aborted; money already spent is not refunded\n" +
	"  to stop spending and keep the run: arxi run pause <run>\n"

func cmdRunCancel(args []string) {
	c := surface.Lookup("run", "cancel")
	vals, err := parseInvocation(c, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi run cancel: %v\n\n%s", err, runCancelUsage)
		os.Exit(2)
	}

	// Trimmed and checked separately from the missing-positional case, for the
	// reason pause documents: `arxi run cancel "$RUN"` with RUN unset satisfies
	// the parser, and without this it reaches resolveRunDir and reports the runs
	// directory itself as an unreadable run.
	runArg := strings.TrimSpace(vals["run"])
	if runArg == "" {
		fmt.Fprint(os.Stderr, "arxi run cancel: which run?\n\n"+runCancelUsage)
		os.Exit(2)
	}

	// Trimmed too, and for a sharper reason: `--reason " "` would put a payload
	// key holding a space into the log, and `run result` would print that space
	// as the run's result. An empty reason and a blank one are the same absence.
	reason := strings.TrimSpace(vals["reason"])

	dir := resolveRunDir(runArg)

	// Folded BEFORE logstore.Open, which MkdirAll's its directory: opening first
	// answers a typo'd run id by creating runs/<typo>/ and leaving it behind for
	// `run list` to show. See pause.go for the same ordering and the test that
	// pins it.
	pre, _, simulated, events, err := foldRunDirEvents(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi run cancel: %v\n", err)
		os.Exit(1)
	}

	id := pre.RunID
	if id == "" {
		id = filepath.Base(dir)
	}

	if pre.Status.Terminal() {
		printCancelRefusal(pre, id, events)
		os.Exit(1)
	}

	store, err := logstore.Open(dir)
	if err != nil {
		var locked *logstore.LockedError
		if errors.As(err, &locked) {
			// Deliberately not locked.Error(): its advice is to remove
			// writer.lock by hand, which is right for a lock left by a dead
			// process and dangerous here, where the refusal happens BECAUSE the
			// writer is alive. Two writers produce duplicate seq and an
			// unfoldable log.
			fmt.Fprintf(os.Stderr, "arxi run cancel: run %s is being driven right "+
				"now by %s, so it cannot be cancelled from here.\n"+
				"  one writer per log: the process holding it is the only thing "+
				"that may append, and this command is not it.\n"+
				"  stop that process (Ctrl-C) -- it stops at the next quiescence, "+
				"and then this command works.\n"+
				"  do NOT delete %s: two writers produce duplicate seq and a log "+
				"that no longer folds.\n",
				id, locked.Owner, filepath.Join(dir, "writer.lock"))
			os.Exit(1)
		}
		fatal(err)
	}
	defer store.Close()
	atExit(func() { store.Close() })

	payload := map[string]any{}
	if reason != "" {
		payload["reason"] = reason
	}

	ev := kernel.Event{
		ID:   "cancel-" + strconv.FormatInt(store.Head()+1, 10),
		Type: kernel.RunCancelled,
		// A cancel is somebody's decision, and an audit that cannot tell it from
		// a run the runtime failed cannot answer why the work stopped.
		Source: kernel.SourceHuman,
		Scope:  "run:" + id,
		// Stamped here because nothing else will: this append does not go through
		// the effect runner. Measured omission -- inbox replies landed with
		// "ts":"" for exactly this reason.
		Ts: nowFunc().UTC().Format(time.RFC3339),
		// reason? per spec/events.md:41, and omitted entirely when absent rather
		// than written as "". `run result` treats a non-empty reason as the run's
		// result text (runresult.go:199), so an empty string would give a
		// cancelled run a result that is one blank line.
		Payload: payload,
	}

	// AppendIfSeq, not Append: the terminal-status refusal above was decided from
	// a state folded at pre.Seq, and this makes the append conditional on the log
	// still being there. Without the CAS, a cancel racing a run that is finishing
	// records a human cancelling a run that had already succeeded -- and the
	// reducer, having reached terminal first, ignores it. The log would then say
	// the run was cancelled and `run result` would say it succeeded.
	written, err := store.AppendIfSeq(pre.Seq, []kernel.Event{ev})
	if err != nil {
		var cas *logstore.CASError
		if errors.As(err, &cas) {
			fmt.Fprintf(os.Stderr, "arxi run cancel: run %s moved while this "+
				"command was deciding (it was at seq %d, the log is at seq %d).\n"+
				"  nothing was written, and this is the case worth re-reading: the "+
				"run may have finished on its own.\n"+
				"  what it is now: arxi run show %s\n", id, cas.Expected, cas.Actual, id)
			os.Exit(1)
		}
		fatal(fmt.Errorf("record run.cancelled: %w", err))
	}

	sim := ""
	if simulated {
		sim = "  [simulated]"
	}
	// The headline docs/design/20-use-cases.md:406 promises, verbatim: "run r1
	// cancelled at seq 61". Everything else is indented beneath it.
	fmt.Printf("run %s cancelled at seq %d%s\n", id, written[0].Seq, sim)

	printCancelEffect(pre, id, reason)
}

// printCancelRefusal explains a cancel that was not needed.
//
// A second run.cancelled is not merely redundant, it is unreadable: the reducer
// ignores every event after a terminal one, and cancelReason (runresult.go:261)
// deliberately quotes the FIRST one. So a second cancel with a better reason
// records that reason nowhere a reader will find it, which is worse than
// refusing -- the user believes they have annotated the run.
func printCancelRefusal(st kernel.State, id string, events []kernel.Event) {
	if st.Status == kernel.StatusCancelled {
		fmt.Fprintf(os.Stderr, "arxi run cancel: run %s was already cancelled "+
			"(seq %d), and cancelling is not something that can be done twice.\n",
			id, st.Seq)
		if r := cancelReason(events); r != "" {
			fmt.Fprintf(os.Stderr, "  the reason on record: %s\n", r)
		} else {
			fmt.Fprintf(os.Stderr, "  no reason was recorded, and this command "+
				"cannot add one: the log is append-only, and a second "+
				"run.cancelled is ignored by the reducer -- the reason would be "+
				"in the file and in no reading of it.\n")
		}
		fmt.Fprintf(os.Stderr, "  what it ended with: arxi run result %s\n", id)
		return
	}

	fmt.Fprintf(os.Stderr, "arxi run cancel: run %s is %s, which is final, so "+
		"there is nothing left to cancel.\n"+
		"  a cancel now would append an event the reducer ignores and change the "+
		"status to something the run never was.\n"+
		"  what it ended with: arxi run result %s\n", id, st.Status, id)
}

// printCancelEffect says what the cancel ended and what it did not undo.
//
// The three facts here are the three ways "cancelled" gets misread, and each is
// printed only on the run it is true of. An open turn is not aborted (the same
// physics as pause: there is no daemon, and this process is not the one running
// the call); the money is spent whatever the status says; and an unanswered
// question is now unanswerable, which matters because `arxi inbox` will keep
// listing it.
func printCancelEffect(st kernel.State, id, reason string) {
	fmt.Print("  it is final: a cancelled run cannot be resumed, and every " +
		"event after this one folds into nothing\n")

	if open := openTurns(st); len(open) > 0 {
		// Sayable with confidence because this command holds the writer lock: if
		// nothing else may append, nothing else is driving this run, so the open
		// turn is not a call being cut off. It is a turn whose process stopped
		// without closing it.
		fmt.Printf("  %s's turn is recorded as open and is NOT aborted -- no "+
			"process was driving this run (cancelling it required its writer "+
			"lock), so no call was interrupted.\n", strings.Join(open, ", "))
	}

	if st.TreeSpentUSD > 0 {
		// The design's own argument for blocking rather than killing
		// (20-use-cases.md:394): the work already done is worth the money already
		// spent. Printing the figure is what makes the trade visible at the
		// moment it is being made.
		fmt.Printf("  %s USD of %s was already spent in the tree and is not "+
			"recovered by this: arxi run show %s\n",
			usd(st.TreeSpentUSD), usd(st.BudgetUSD), id)
	}

	if n := pendingAsks(st); n > 0 {
		// Worth its own line because the question outlives the run in the inbox:
		// State.Inbox still holds it, so `arxi inbox` lists it, and nothing about
		// the row says the run it belongs to is over.
		fmt.Printf("  %d unanswered question(s) can no longer be answered: the "+
			"run is terminal, so a reply would fold into nothing. arxi inbox "+
			"refuses them rather than pretending.\n", n)
	}

	if reason == "" {
		// Printed on the cancel itself rather than in the usage, because this is
		// the last moment it can be acted on -- and it cannot be acted on
		// afterwards at all. A cancel with no reason is indistinguishable from a
		// failure six weeks later (20-use-cases.md:407).
		fmt.Printf("  no reason was recorded, and none can be added later (the " +
			"log is append-only and a second run.cancelled is ignored).\n" +
			"    next time: arxi run cancel <run> --reason \"why\" -- without it, " +
			"arxi run list cannot tell this from a run that failed.\n")
		return
	}
	fmt.Printf("  reason, in the log and in the result: %s\n"+
		"    read it back: arxi run result %s\n", reason, id)
}
