package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/michiTrader/arxi/internal/inbox"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/surface"
)

// `arxi run list` -- the first of the twelve run verbs, and a projection.
//
// # Why this is small
//
// The README calls the twelve unwired `run *` verbs "twelve readings of one
// finished mechanism", and this command is the test of that claim. It builds
// nothing: the log already exists, kernel.Fold already turns it into a
// kernel.State, and inbox.OpenRun already does both without taking a lock. All
// that was missing was a caller that asks for every run instead of one.
//
// If this file had needed a new store, a new index, or a `runs.json` listing
// what exists, the claim would have been wrong and the right response would
// have been to say so rather than build the index. It did not.
//
// # Why it does not use logstore.Open
//
// logstore.Open takes the writer lock and holds it for the lifetime of the
// Store, because exactly one process may write a log. A read-only listing that
// took that lock would fail on precisely the runs a user most wants listed --
// the ones currently executing -- and reporting "someone else is writing" for
// `run list` would be absurd. Worse, a crash mid-listing would strand a lock
// file naming a pid that no longer exists, which is a failure this project has
// already met once and does not need reintroduced by a read.
//
// inbox.OpenRun is the read path: it reads events.ndjson, folds it against the
// run's own frozen config, and never writes. `arxi inbox` has scanned every run
// this way since it was built, so this is the established shape rather than a
// new one.
//
// # Why one bad directory is a warning and not an exit
//
// Copied deliberately from cmdInboxList, and for a stronger reason here. If a
// single unreadable run aborted the listing, one corrupt directory would hide
// every healthy run -- and the output would look identical to having no runs at
// all. "I have no runs" and "I have nine runs and one is damaged" must not print
// the same thing.

// runListRow is one line of the table, already projected.
//
// The projection happens once, up front, so sorting and filtering cannot
// disagree with what is printed. A version that re-derived the status while
// rendering would be one edit away from filtering on one value and displaying
// another.
type runListRow struct {
	id     string
	dir    string
	actor  string
	status kernel.RunStatus
	stage  string
	seq    int64
	spent  float64
	budget float64
	turns  int
	asks   int
}

func cmdRunList(args []string) {
	c := surface.Lookup("run", "list")
	vals, err := parseInvocation(c, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi run list: %v\n", err)
		os.Exit(2)
	}

	want := strings.TrimSpace(vals["status"])

	// An unknown --status is refused rather than silently matching nothing.
	//
	// This is the difference between a filter and a trap. `--status done` is a
	// reasonable guess and is not one of the eight; matching it against nothing
	// would print "no runs" to a user whose runs had all succeeded, and the
	// answer they would take away is the opposite of the truth. The valid set
	// is listed because a rejection that does not say what IS allowed just
	// moves the guessing one step later.
	if want != "" && !isKnownRunStatus(want) {
		fmt.Fprintf(os.Stderr, "arxi run list: %q is not a run status.\n"+
			"  it accepts: %s\n"+
			"  refusing rather than matching nothing: an unknown filter would "+
			"print \"no runs\" for a directory full of them, and that reads as "+
			"an answer rather than a mistake.\n",
			want, strings.Join(knownRunStatuses(), ", "))
		os.Exit(2)
	}

	runs, err := discoverRuns()
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi run list: %v\n", err)
		os.Exit(1)
	}

	var rows []runListRow
	var unreadable []string

	for _, dir := range runs {
		r, err := inbox.OpenRun(dir)
		if err != nil {
			unreadable = append(unreadable, fmt.Sprintf("%s: %v", dir, err))
			continue
		}
		st := r.State()

		if want != "" && string(st.Status) != want {
			continue
		}

		// The id comes from the log, and the directory name is only a fallback.
		// They are normally the same, and when they are not the log is the one
		// that is true -- a directory can be renamed by hand, and a listing
		// that believed the filename would report an id that no `run show`
		// would ever accept.
		id := st.RunID
		if id == "" {
			id = filepath.Base(dir)
		}

		rows = append(rows, runListRow{
			id: id, dir: dir, actor: st.Actor, status: st.Status,
			stage: st.Stage, seq: st.Seq, spent: st.SpentUSD,
			budget: st.BudgetUSD, turns: st.Turns, asks: pendingAsks(st),
		})
	}

	// Newest-looking last is wrong for a listing you scan top-down looking for
	// what needs attention, so the sort puts the runs that are still live
	// first, then falls back to the id for a stable order. Without the
	// tie-break the order would depend on directory iteration, and a listing
	// that reshuffles between two identical invocations looks broken even when
	// it is correct.
	sort.SliceStable(rows, func(i, j int) bool {
		li, lj := liveRank(rows[i].status), liveRank(rows[j].status)
		if li != lj {
			return li < lj
		}
		return rows[i].id < rows[j].id
	})

	if vals["json"] == "true" {
		out := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			out = append(out, map[string]any{
				"run": r.id, "dir": r.dir, "actor": r.actor,
				"status": string(r.status), "stage": r.stage, "seq": r.seq,
				"spent_usd": r.spent, "budget_usd": r.budget,
				"turns": r.turns, "pending_asks": r.asks,
			})
		}
		payload := map[string]any{"runs": out}
		if len(unreadable) > 0 {
			payload["unreadable"] = unreadable
		}
		emitJSON(payload)
		return
	}

	if len(rows) == 0 {
		// What was looked at matters most when the answer is "nothing", the
		// same argument `arxi inbox` makes. With a filter the two ways of
		// finding nothing are different problems -- no runs at all, versus no
		// runs in that state -- and they have different next steps.
		if want != "" {
			fmt.Printf("no runs with status %q.\n", want)
			fmt.Printf("  looked in %s (%d run%s, none matching)\n",
				runsDir, len(runs), plural(len(runs)))
		} else {
			fmt.Println("no runs.")
			fmt.Printf("  looked in %s\n", runsDir)
		}
	} else {
		// The id column is sized to the widest id actually present, and never
		// truncated. Every other column may be elided; this one may not.
		//
		// Found by running it. A fixed width of 14 looked generous and is not:
		// real ids are 18 characters, so EVERY row printed "rmthws2dz-933…", and
		// pasting that into the command this listing exists to feed answers
		// "runs/rmthws2dz-933… holds no event log". A listing whose purpose is to
		// hand the user an id to act on must not print ids that cannot be used.
		width := len("RUN")
		for _, r := range rows {
			if len(r.id) > width {
				width = len(r.id)
			}
		}

		fmt.Printf("%-*s %-10s %-10s %-12s %6s %12s %6s\n",
			width, "RUN", "ACTOR", "STATUS", "STAGE", "SEQ", "SPENT", "ASKS")
		for _, r := range rows {
			fmt.Printf("%-*s %-10s %-10s %-12s %6d %12s %6s\n",
				width, r.id, truncateCol(r.actor, 10),
				truncateCol(string(r.status), 10), truncateCol(dashIfEmpty(r.stage), 12),
				r.seq, formatSpend(r.spent, r.budget), dashIfZero(r.asks))
		}
	}

	for _, u := range unreadable {
		fmt.Fprintf(os.Stderr, "warning: %s\n", u)
	}
}

// liveRank orders statuses by how much they want a human.
//
// blocked first, because a blocked run is the only one that cannot progress
// without somebody doing something. Then the ones still moving, then the ones
// that are over. A plain alphabetical sort would put "cancelled" above
// "blocked", which buries the only row that is asking for help.
func liveRank(s kernel.RunStatus) int {
	switch s {
	case kernel.StatusBlocked:
		return 0
	case kernel.StatusRunning:
		return 1
	case kernel.StatusPaused:
		return 2
	case kernel.StatusQueued:
		return 3
	}
	return 4
}

// allRunStatuses is the vocabulary, taken from the kernel rather than retyped.
//
// Retyping it is how a filter comes to accept seven of eight values: the eighth
// is added to the kernel, nothing here fails, and `--status expired` starts
// answering "not a run status" about a status the reducer can produce. Deriving
// it means the only way to break that is to remove a constant.
var allRunStatuses = []kernel.RunStatus{
	kernel.StatusQueued, kernel.StatusRunning, kernel.StatusBlocked,
	kernel.StatusPaused, kernel.StatusSucceeded, kernel.StatusFailed,
	kernel.StatusCancelled, kernel.StatusExpired,
}

func isKnownRunStatus(s string) bool {
	for _, k := range allRunStatuses {
		if string(k) == s {
			return true
		}
	}
	return false
}

func knownRunStatuses() []string {
	out := make([]string, 0, len(allRunStatuses))
	for _, k := range allRunStatuses {
		out = append(out, string(k))
	}
	return out
}

// formatSpend shows the budget only when there is one, without rounding it away.
//
// "0.12/1.00" is useful and "0.12/0.00" is a lie about an unlimited run, so the
// denominator appears only when it was set. The rounding half of this was found
// by running it: a run with --budget 0.001 printed "0.02/0.00", which claims no
// budget for a run that is TWENTY TIMES over the one it has -- the same run the
// inbox correctly describes as "budget exhausted (0.0200 of 0.0010 USD)". Two
// decimals is right for dollars and wrong for a budget deliberately set below a
// cent, so a small budget keeps the precision that makes it meaningful rather
// than being rounded into a claim that it does not exist.
func formatSpend(spent, budget float64) string {
	if budget <= 0 {
		return fmt.Sprintf("%.2f", spent)
	}
	if budget < 0.01 {
		return fmt.Sprintf("%.4f/%.4f", spent, budget)
	}
	return fmt.Sprintf("%.2f/%.2f", spent, budget)
}

// pendingAsks counts the questions still waiting on a human.
//
// It is not len(st.Inbox), and that difference is a defect this command shipped
// with. An answered question STAYS in the inbox with Replied set -- the log is
// append-only and the reducer marks rather than removes, which is correct and
// is what makes `event trace` able to show that somebody answered. Counting the
// slice therefore counted history as work outstanding.
//
// Found by running the binary, not by reading it: a run whose single approval
// had been answered printed ASKS=1 while `arxi inbox`, in the same binary,
// printed "no pending questions". Two commands contradicting each other about
// the same run is worse than either being wrong alone, because whichever the
// user happens to trust, the other one teaches them the tool is unreliable.
//
// printRunSummary already filters on !Replied; this is the same rule, applied
// where it was missed. The JSON field was called "pending_asks" all along,
// which is the intent it was failing to honour.
func pendingAsks(st kernel.State) int {
	n := 0
	for _, it := range st.Inbox {
		if !it.Replied {
			n++
		}
	}
	return n
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func dashIfZero(n int) string {
	if n == 0 {
		return "-"
	}
	return fmt.Sprintf("%d", n)
}
