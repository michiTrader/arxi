package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/surface"
)

// `arxi run show <run>` -- the second reading of the same fold.
//
// # What this adds over `run list`
//
// `run list` answers "which runs exist and which one needs me". This answers
// "what is true of THIS one", and the difference is entirely in what may be
// left out. A table has to elide: it has one line per run, so members, locks
// and the text of a question cannot appear in it. A detail view has the room,
// and its whole justification is spending that room on what the table dropped.
//
// So this is deliberately not a one-run table. If it printed the same seven
// columns for a single id it would be a worse `run list --status`, and the verb
// would exist only to make the surface count go up.
//
// # It reuses the resolver rather than re-deriving it
//
// resolveRunDir and foldRunDir already exist, written for `run unpause`. Using
// them means `run show <id>` and `run unpause <id>` cannot disagree about which
// directory an id names, and that they accept a path in the same cases. A
// second resolver would be a second set of rules to keep in sync, and the first
// symptom of drift would be `run show` describing a run that `run unpause`
// then says does not exist.
//
// foldRunDir also carries the simulated flag, which the State does not: the
// reducer has no use for the distinction, so run.started is the only place it
// survives. That matters here more than anywhere else, because the number this
// view puts on screen is a dollar amount, and "$0.42" from a simulated run is
// not a bill.

func cmdRunShow(args []string) {
	c := surface.Lookup("run", "show")
	vals, err := parseInvocation(c, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi run show: %v\n\n"+
			"usage: arxi run show <run>\n", err)
		os.Exit(2)
	}

	runArg := strings.TrimSpace(vals["run"])
	if runArg == "" {
		// The suggestion is `run list` and not `inbox`, unlike unpause's. This
		// verb reads any run, including finished ones, and the inbox only
		// knows about runs with an unanswered question -- pointing there would
		// show nothing at all to somebody whose runs have all succeeded.
		fmt.Fprintf(os.Stderr, "arxi run show: which run?\n"+
			"  usage: arxi run show <run>\n"+
			"  see what exists: arxi run list\n")
		os.Exit(2)
	}

	dir := resolveRunDir(runArg)
	st, cfg, simulated, err := foldRunDir(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi run show: %v\n", err)
		os.Exit(1)
	}

	id := st.RunID
	if id == "" {
		id = filepath.Base(dir)
	}

	if vals["json"] == "true" {
		emitJSON(runShowPayload(id, dir, st, cfg, simulated))
		return
	}

	printRunShow(id, dir, st, simulated)
}

// runShowPayload is the machine reading.
//
// It carries the whole state rather than the fields the table happens to show,
// because the reason to add --json to a detail view is that somebody wants
// something the human view left out. Members and locks are included as
// structures for the same reason: a caller that has to parse "backend
// (thinking) 2 turns" out of a line is a caller that will break the first time
// a column is widened.
func runShowPayload(id, dir string, st kernel.State, cfg kernel.Config, simulated bool) map[string]any {
	members := make([]map[string]any, 0, len(st.Members))
	for _, m := range st.Members {
		e := map[string]any{
			"name": m.Name, "state": string(m.State),
			"turns": m.Turns, "spent_usd": m.SpentUSD,
			"since_seq": m.SinceSeq,
			// Busy and Runnable are computed, not stored, and the rules behind
			// them are subtle enough that internal/kernel documents them at
			// length. Exporting the answers means a caller does not have to
			// reimplement "submitted is neither busy nor runnable" and get it
			// wrong in the direction that hides a stuck run.
			"busy": m.Busy(), "runnable": m.Runnable(),
		}
		if m.Role != "" {
			e["role"] = m.Role
		}
		if m.Detail != "" {
			e["detail"] = m.Detail
		}
		if m.Advisory {
			e["advisory"] = true
		}
		if m.Submitted {
			e["submitted"] = true
		}
		if len(m.PendingCauses) > 0 {
			e["pending_causes"] = m.PendingCauses
		}
		members = append(members, e)
	}

	asks := make([]map[string]any, 0, len(st.Inbox))
	for _, it := range st.Inbox {
		a := map[string]any{
			"id": it.ID, "kind": it.Kind,
			"question": it.Question, "replied": it.Replied,
		}
		if it.Agent != "" {
			a["agent"] = it.Agent
		}
		if it.OnTimeout != "" {
			a["on_timeout"] = it.OnTimeout
		}
		asks = append(asks, a)
	}

	locks := make([]map[string]any, 0, len(st.Locks))
	for _, l := range st.Locks {
		lk := map[string]any{"key": l.Key, "holder": l.Holder}
		// Omitted rather than "" when there is no expiry, the way the Lock's own JSON
		// tag omits it and for the same reason: absent means "held until released" and a
		// present empty string invites a consumer to print it as an instant. A reader
		// deciding whether a lock is stealable tests presence first, so the two cases
		// have to stay distinguishable here as well.
		if l.ExpiresAt != "" {
			lk["expires_at"] = l.ExpiresAt
		}
		locks = append(locks, lk)
	}

	out := map[string]any{
		"run": id, "dir": dir, "actor": st.Actor,
		"status": string(st.Status), "terminal": st.Status.Terminal(),
		"seq": st.Seq, "stage": st.Stage, "stage_index": st.StageIndex,
		"turns": st.Turns, "max_turns": st.MaxTurns,
		"spent_usd": st.SpentUSD, "budget_usd": st.BudgetUSD,
		"tree_spent_usd": st.TreeSpentUSD,
		"spawn_depth":    st.SpawnDepth,
		"simulated":      simulated,
		"pending_asks":   pendingAsks(st),
		"members":        members,
		"asks":           asks,
		"locks":          locks,
		"log":            filepath.Join(dir, "events.ndjson"),
	}
	if st.ParentRunID != "" {
		out["parent_run_id"] = st.ParentRunID
	}
	if st.BlueprintSHA != "" {
		out["blueprint_sha"] = st.BlueprintSHA
	}
	if st.ActiveTimer != "" {
		out["active_timer"] = st.ActiveTimer
	}
	if st.Result != "" {
		out["result"] = st.Result
	}
	return out
}

func printRunShow(id, dir string, st kernel.State, simulated bool) {
	// The simulated marker rides on the headline rather than sitting in a
	// footnote, because every money figure below it is fake and a reader who
	// misses that fact misreads all of them. It is the one qualifier that
	// changes the meaning of the rest of the output.
	sim := ""
	if simulated {
		sim = "  [simulated]"
	}
	fmt.Printf("run %s  %s%s\n", id, st.Status, sim)

	if st.Actor != "" {
		fmt.Printf("  actor:  %s\n", st.Actor)
	}
	if st.Stage != "" {
		fmt.Printf("  stage:  %s (index %d)\n", st.Stage, st.StageIndex)
	}
	fmt.Printf("  seq:    %d\n", st.Seq)

	turns := fmt.Sprintf("%d", st.Turns)
	if st.MaxTurns > 0 {
		turns = fmt.Sprintf("%d of %d", st.Turns, st.MaxTurns)
	}
	fmt.Printf("  turns:  %s\n", turns)
	fmt.Printf("  spent:  %s\n", showSpend(st))

	if st.ParentRunID != "" {
		fmt.Printf("  parent: %s (depth %d)\n", st.ParentRunID, st.SpawnDepth)
	}
	if st.ActiveTimer != "" {
		fmt.Printf("  timer:  %s\n", st.ActiveTimer)
	}
	fmt.Printf("  log:    %s\n", filepath.Join(dir, "events.ndjson"))

	printShowMembers(st)
	printShowLocks(st)
	printShowAsks(st)

	if st.Result != "" {
		fmt.Printf("\nresult:\n  %s\n", st.Result)
	}

	// The closing line is the next command, chosen by what is actually true of
	// this run rather than printed unconditionally. A terminal run gets
	// nothing: suggesting `run why` for a run that succeeded invites the user
	// to go looking for a problem that does not exist.
	switch {
	case st.Status == kernel.StatusBlocked && pendingAsks(st) > 0:
		fmt.Printf("\nit is waiting on you:  arxi inbox\n")
	case st.Status == kernel.StatusPaused:
		fmt.Printf("\nresume it:  arxi run unpause %s\n", id)
	case !st.Status.Terminal():
		fmt.Printf("\nwhy is it not finished?  arxi run why %s\n", id)
	}
}

// showSpend states the tree total alongside the run's own.
//
// A run that spawned children has spent more than its own SpentUSD says, and
// the budget it is measured against is the TREE's -- that is the whole point of
// the sub-pool. Printing "0.10 of 1.00" for a run whose children burned 0.90
// would show a comfortable margin at the moment the ceiling is hit. So the
// tree figure is the one compared to the budget, and the run's own is shown
// beside it only when they differ, since on the common case of a childless run
// repeating the same number twice is noise.
func showSpend(st kernel.State) string {
	var b strings.Builder
	if st.BudgetUSD > 0 {
		fmt.Fprintf(&b, "%s of %s USD in the tree",
			trimUSD(st.TreeSpentUSD), trimUSD(st.BudgetUSD))
	} else {
		fmt.Fprintf(&b, "%s USD in the tree, no ceiling", trimUSD(st.TreeSpentUSD))
	}
	if st.TreeSpentUSD != st.SpentUSD {
		fmt.Fprintf(&b, " (%s of it this run)", trimUSD(st.SpentUSD))
	}
	// An overspend is SAID, not left to be inferred by comparing two numbers.
	//
	// Found by running it: a run stopped dead by its ceiling printed
	// "0.02 of 0.001 USD in the tree", which is a breach of twenty times the
	// budget rendered as an ordinary progress fraction. Reading it correctly
	// requires noticing that the left number exceeds the right one, and the
	// two are different lengths and different magnitudes precisely when it
	// matters most. The inbox already words this properly; the detail view of
	// the same run should not be the vaguer of the two.
	if st.BudgetUSD > 0 && st.TreeSpentUSD > st.BudgetUSD {
		fmt.Fprintf(&b, " -- OVER by %s", trimUSD(st.TreeSpentUSD-st.BudgetUSD))
	}
	return b.String()
}

// trimUSD prints a dollar figure without rounding a real amount to zero.
//
// Four decimals, not two, and for the reason `run list` had to learn by being
// run: a budget of 0.001 shown as "0.00" reads as no budget at all. Here the
// same rounding would report a run that has spent money as having spent
// nothing, which is the more expensive direction of the same lie. Trailing
// zeroes are trimmed so the common case still reads as "0.42" rather than
// "0.4200".
func trimUSD(v float64) string {
	s := fmt.Sprintf("%.4f", v)
	s = strings.TrimRight(s, "0")
	s = strings.TrimSuffix(s, ".")
	if s == "" || s == "-" {
		return "0"
	}
	return s
}

// printShowMembers is the main thing the table could not show.
//
// Members are printed in the roster's own order and NOT sorted. The order a
// blueprint declares them in is meaningful -- it is the order stages activate
// them -- and re-sorting alphabetically would destroy information the user
// could not recover from this view.
func printShowMembers(st kernel.State) {
	if len(st.Members) == 0 {
		return
	}
	fmt.Printf("\nmembers (%d):\n", len(st.Members))

	width := 0
	for _, m := range st.Members {
		if len(m.Name) > width {
			width = len(m.Name)
		}
	}

	halted := spendingHalted(st)

	for _, m := range st.Members {
		// The state word alone is not enough to act on: "waiting" does not say
		// what is being waited for, and "idle" is the same word for a member
		// that has finished and one with work queued. The note after it is
		// what makes the difference visible.
		note := memberNote(m, halted)
		line := fmt.Sprintf("  %-*s  %-10s", width, m.Name, m.State)
		if note != "" {
			line += "  " + note
		}
		fmt.Println(line)

		detail := []string{}
		if m.Role != "" {
			detail = append(detail, "role "+m.Role)
		}
		if m.Turns > 0 {
			detail = append(detail, fmt.Sprintf("%d turn%s", m.Turns, plural(m.Turns)))
		}
		if m.SpentUSD > 0 {
			detail = append(detail, trimUSD(m.SpentUSD)+" USD")
		}
		if m.Detail != "" {
			detail = append(detail, m.Detail)
		}
		if len(detail) > 0 {
			fmt.Printf("  %-*s  %s\n", width, "", strings.Join(detail, ", "))
		}
	}
}

// memberNote names what the state word leaves ambiguous.
//
// halted changes what a queued cause MEANS, and getting that wrong was a defect
// this command shipped in its first build. kernel.Member.Runnable() answers "a
// turn is coming", and it is right about the member: there is a cause waiting
// to be drained. But when the run is blocked or paused, spawnCauses parks those
// causes instead of opening the turn -- so on a blocked run the honest reading
// is the opposite one, that this member's work is being WITHHELD pending the
// user's decision.
//
// Printing "a turn is queued" there told a user staring at a run that had
// stopped dead that work was on its way. The run had breached its budget and
// was waiting to be told whether to keep going; the one thing that was not
// about to happen was a turn.
func memberNote(m kernel.Member, halted bool) string {
	switch {
	case halted && len(m.PendingCauses) > 0:
		return fmt.Sprintf("(work held back: %s)", strings.Join(m.PendingCauses, ", "))
	case m.Advisory && m.Runnable():
		return "(advisory, a turn is queued)"
	case m.Runnable():
		return fmt.Sprintf("(a turn is queued: %s)", strings.Join(m.PendingCauses, ", "))
	case m.State == kernel.MemberWaiting && len(m.BlockedOn) > 0:
		return "(blocked on " + describeBlockedOn(m.BlockedOn) + ")"
	case m.Busy():
		return "(working)"
	case m.Advisory:
		return "(advisory)"
	case m.Submitted:
		return "(submitted)"
	}
	return ""
}

// spendingHalted mirrors the reducer's rule about when a turn may open.
//
// It is deliberately the same two statuses kernel.spendingHalted uses, and the
// duplication is the uncomfortable part: that function is unexported, so this
// view cannot call the thing it must agree with. The alternative -- exporting a
// reducer predicate so a printer can borrow it -- widens the kernel's surface
// for a display concern, which the frozen-surface rule exists to prevent.
//
// So the risk is accepted and named instead: if a third status ever halts
// spending, this view will describe withheld work as imminent until this line
// is updated. A test pins the two that exist today.
func spendingHalted(st kernel.State) bool {
	return st.Status == kernel.StatusBlocked || st.Status == kernel.StatusPaused
}

// describeBlockedOn renders the wait reason in a stable order.
//
// BlockedOn is a map, so ranging it directly would print the keys in a
// different order on different runs of the same command. A detail view that
// reshuffles between two identical invocations looks broken even when it is
// correct -- the same argument `run list`'s tie-break makes.
func describeBlockedOn(on map[string]any) string {
	keys := make([]string, 0, len(on))
	for k := range on {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %v", k, on[k]))
	}
	return strings.Join(parts, ", ")
}

// printShowLocks prints who holds what, and whether the claim still stands.
//
// The expiry is here because this is the command every lock message in the binary
// points at: `state lock`'s refusals say "who holds what: arxi run show <id>", and
// a holder without a lease answers half of what the reader came for -- whether to
// wait or to steal.
//
// It reads a clock, alone among the show renderings, and the alternative was worse.
// The annotation comes from lockLapsed, the SAME predicate `state lock` arbitrates
// with, so the two cannot disagree; printing the instant and leaving the comparison
// to the reader would let this view call a lock live that the next acquire would
// steal out from under it. The clock is nowFunc, so a test can pin it.
//
// The JSON half deliberately does not carry the annotation: expires_at is a fact of
// the fold, "lapsed" is a reading of it against a now, and a machine consumer has
// its own clock.
func printShowLocks(st kernel.State) {
	if len(st.Locks) == 0 {
		return
	}
	now := nowFunc().UTC()
	fmt.Printf("\nlocks (%d):\n", len(st.Locks))
	for _, l := range st.Locks {
		fmt.Printf("  %s  held by %s%s\n", l.Key, l.Holder, showLockLease(l, now))
	}
}

// showLockLease renders the lease half of a lock line.
//
// Three outcomes, because a lock has three states worth telling apart and only one
// of them is "held until T". No expiry is NOT expired -- it is held until the run
// ends, since the only lock.released this binary writes is `state lock` stealing a
// lapsed lease -- and an expiry no reader can parse is the same dead end reached by
// a worse route, so both say what follows rather than showing a raw field.
func showLockLease(l kernel.Lock, now time.Time) string {
	if l.ExpiresAt == "" {
		return "  (no expiry: held until the run ends)"
	}
	if _, err := time.Parse(time.RFC3339, l.ExpiresAt); err != nil {
		return fmt.Sprintf("  (expires_at %q is not an instant, so nothing can judge it "+
			"lapsed)", l.ExpiresAt)
	}
	if lockLapsed(l, now) {
		return fmt.Sprintf("  (lapsed at %s: the next `arxi state lock` takes it)",
			l.ExpiresAt)
	}
	return "  until " + l.ExpiresAt
}

// printShowAsks prints questions, with the unanswered ones first.
//
// Answered questions are shown rather than hidden, because this is the detail
// view and "what was asked and what was decided" is history the user came here
// for. They are marked and sorted below the live ones so the distinction is
// never in doubt -- which is the same distinction `run list`'s ASKS column got
// wrong by counting them together.
func printShowAsks(st kernel.State) {
	if len(st.Inbox) == 0 {
		return
	}
	pending := pendingAsks(st)
	fmt.Printf("\nquestions (%d, %d pending):\n", len(st.Inbox), pending)

	items := make([]kernel.InboxItem, len(st.Inbox))
	copy(items, st.Inbox)
	sort.SliceStable(items, func(i, j int) bool {
		return !items[i].Replied && items[j].Replied
	})

	for _, it := range items {
		mark := "?"
		if it.Replied {
			mark = "answered"
		}
		fmt.Printf("  %s [%s] %s\n", it.ID, it.Kind, it.Question)
		who := ""
		if it.Agent != "" {
			who = ", asked by " + it.Agent
		}
		timeout := ""
		if it.OnTimeout != "" {
			timeout = ", on timeout " + it.OnTimeout
		}
		fmt.Printf("      %s%s%s\n", mark, who, timeout)
	}
}
