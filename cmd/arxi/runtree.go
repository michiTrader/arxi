package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/surface"
)

// `arxi run tree <run>` -- the view whose subject is the tree.
//
// # Why this is a separate verb at all
//
// docs/design/20-use-cases.md §20.7 answers that directly: "the honest figure is
// a property of the tree, so there has to be a view whose subject *is* the tree."
// `run show` describes one run and cannot; `run list` has one line per run and so
// cannot show who spawned whom. Delegation is the case where a per-run reading is
// not merely incomplete but wrong -- a root sitting comfortably at 14.20 of 20.00
// while its children have burned another 3.50 is a number that is still displayed
// and is simply untrue, which §10.7 calls the worst kind of failure.
//
// # It is a projection, like its siblings
//
// Nothing new is built or stored. discoverRuns finds the directories, foldRunDir
// folds each log, and the edges come from each child's own ParentRunID -- which
// the reducer sets from the run.started payload and nothing else writes
// (internal/kernel/decide.go:232). There is no index, no runs.json, no parent
// pointer file. If this command had needed one, the README's claim that the run
// verbs are "readings of one finished mechanism" would have been wrong, and the
// right response would have been to say so rather than to build the index.
//
// foldRunDir is used rather than inbox.OpenRun (which `run list` uses) for one
// reason: it also returns whether the run was simulated, and the State does not
// carry that. Every figure this view prints is money, and a total that silently
// mixes a rehearsal with a bill is worse than no total.
//
// # MEASURED: the design's central claim is not true of the code yet
//
// §20.7 says "TreeSpentUSD accumulates the whole subtree, so r3 spending money
// moves the root figure". It does not. applyCost does
//
//	out.SpentUSD += cost
//	out.TreeSpentUSD += cost
//
// (internal/kernel/decide.go:958) and there is no cross-run rollup anywhere in
// the tree -- grepped, not assumed. So a parent's TreeSpentUSD equals its own
// SpentUSD, each run enforces only its own ceiling, and N levels of delegation do
// multiply the root's budget by N.
//
// This command therefore computes the total by SUMMING each node's own SpentUSD
// rather than by printing the root's TreeSpentUSD, and when the two disagree it
// says so out loud. A view that exists to be honest about the tree must not be
// the place where that particular lie gets laundered into a nice number.
//
// # MEASURED: nothing writes parent_run_id yet either
//
// `run start` has no --parent, its run.started payload carries only
// {run_id, actor, blueprint_sha, budget_usd, max_turns, prompt, workspace,
// simulated} (cmd/arxi/run.go:227), and no spawn effect creates a child run. So
// every tree this binary can currently produce on its own is a tree of one.
//
// That is stated in the output rather than hidden, because the failure it
// prevents is specific: a user delegates, runs `run tree`, sees a single node and
// concludes their children were lost. Being told "nothing writes the parent link
// yet" sends them to the roadmap; being shown a lonely root sends them hunting
// for missing data. The walk itself is written against the real field, so it
// starts working the day spawning lands -- and the tests below exercise real
// multi-level trees, which is what keeps that promise checkable.

// treeNode is one run, already folded, plus its place in the tree.
//
// depth is the STRUCTURAL depth measured by the walk, deliberately separate from
// st.SpawnDepth which comes from the log. The two can disagree -- a log is a
// file, and spawn_depth is written by whoever appended run.started -- and nothing
// in the reducer validates it (kernel.Config.MaxDepth limits event cascade depth,
// not run nesting). A disagreement is reported rather than resolved: this view
// cannot know which of the two is the lie.
type treeNode struct {
	id        string
	dir       string
	st        kernel.State
	simulated bool
	depth     int
	children  []*treeNode
}

// runTreeUsage is the usage block, shared by both ways of arriving with no run id.
//
// Shared rather than written twice because the two arrivals differ only in their
// first line and must not differ in their advice. MEASURED, which is why this
// exists: `arxi run tree` with no argument does not reach the empty-runArg branch
// below at all -- parseInvocation rejects the invocation first ("run tree needs 1
// more flag(s)") -- so the sentence explaining that the argument is the root lived
// in a branch only `arxi run tree ""` can enter. Correct words in an unreachable
// branch are the same defect as a test asserting an impossible state: nothing is
// wrong with them and nobody is ever shown them.
//
// The sentence is worth showing at all because `run list` prints roots and children
// identically -- nothing marks one -- so the id a user has to hand is as likely to
// be a child as a root, and passing a child succeeds, quietly, with a smaller
// number.
const runTreeUsage = "usage: arxi run tree <run>\n" +
	"  the argument is the ROOT of the tree, not a child: a child prints only\n" +
	"  its own subtree, which is what makes a spend look small.\n" +
	"  see what exists: arxi run list\n"

func cmdRunTree(args []string) {
	c := surface.Lookup("run", "tree")
	vals, err := parseInvocation(c, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi run tree: %v\n\n%s", err, runTreeUsage)
		os.Exit(2)
	}

	runArg := strings.TrimSpace(vals["run"])
	if runArg == "" {
		fmt.Fprint(os.Stderr, "arxi run tree: which run?\n\n"+runTreeUsage)
		os.Exit(2)
	}

	dir := resolveRunDir(runArg)
	rootSt, _, rootSim, err := foldRunDir(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi run tree: %v\n", err)
		os.Exit(1)
	}

	rootID := rootSt.RunID
	if rootID == "" {
		rootID = filepath.Base(dir)
	}
	root := &treeNode{id: rootID, dir: dir, st: rootSt, simulated: rootSim}

	index, scanned, unreadable := collectRunNodes(root)
	warnings := linkRunTree(root, index)

	nodes := flattenRunTree(root)
	sc := treeScan{
		scanned:    scanned,
		parented:   parentedOutsideTree(nodes, index),
		unreadable: unreadable,
		warnings:   warnings,
	}

	if vals["json"] == "true" {
		emitJSON(runTreePayload(root, nodes, sc))
		return
	}

	printRunTree(root, nodes, sc)
	for _, w := range sc.warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
	for _, u := range sc.unreadable {
		fmt.Fprintf(os.Stderr, "warning: %s\n", u)
	}
}

// treeScan is what the walk learned that is not in the tree itself.
//
// Grouped into one value rather than passed as four parameters because every one
// of them is consumed by the same closing notes, and the printer's job is to turn
// "what could not be placed" into a sentence. A tree view that only carries the
// tree cannot tell the difference between a run with no children and a run whose
// children it failed to place, which is the distinction these fields exist for.
type treeScan struct {
	scanned    int
	parented   []string
	unreadable []string
	warnings   []string
}

// parentedOutsideTree names the runs that DO declare a parent and are not here.
//
// It exists to stop the closing note from lying. "This run has no children, and
// cannot have any yet -- nothing writes parent_run_id" is true of the binary
// today and becomes false the moment anything on disk names a parent, whether
// that is `run fork` landing or a log edited by hand. The note then asserts the
// link is never written while a run three directories away is writing it, and
// the reader stops looking for the reason their child is missing.
//
// Runs whose parent exists but belongs to a DIFFERENT tree are included here on
// purpose and are only mentioned when this tree has no children at all: once
// spawning works, most runs under runs/ will legitimately belong to other trees,
// and naming them on every invocation would be noise.
func parentedOutsideTree(nodes []*treeNode, index map[string]*treeNode) []string {
	inTree := map[string]bool{}
	for _, n := range nodes {
		inTree[n.id] = true
	}
	var out []string
	for id, n := range index {
		if !inTree[id] && n.st.ParentRunID != "" {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// collectRunNodes folds every run under runsDir, keyed by the id in its log.
//
// The root is seeded first and never replaced, because it may have been named by
// path and live outside runsDir -- `run start --dir` puts it wherever the user
// said. Replacing it with a same-id directory found by the scan would make
// `run tree ./somewhere/r1` silently describe a different r1.
//
// One unreadable directory is a warning and not an exit, copied from `run list`
// and for the same reason, which is stronger here: a tree that aborted on a
// damaged sibling would print nothing at all, and "the root has no children"
// and "I could not read them" must not look the same.
func collectRunNodes(root *treeNode) (index map[string]*treeNode, scanned int, unreadable []string) {
	index = map[string]*treeNode{root.id: root}

	dirs, err := discoverRuns()
	if err != nil {
		// Not fatal: the root has already been folded from an explicit path, so
		// there is still a one-node tree to print. Losing the scan costs the
		// children, and saying so is better than refusing to describe the run
		// the user asked about.
		return index, 0, []string{fmt.Sprintf("%v -- children cannot be found without it", err)}
	}
	scanned = len(dirs)

	for _, d := range dirs {
		st, _, sim, err := foldRunDir(d)
		if err != nil {
			unreadable = append(unreadable, fmt.Sprintf("%s: %v", d, err))
			continue
		}
		id := st.RunID
		if id == "" {
			id = filepath.Base(d)
		}

		if prev, dup := index[id]; dup {
			// Two directories claiming one id is not hypothetical: copying a run
			// directory as a backup produces exactly that, and the log inside
			// still says the original id. Silently keeping one would make the
			// tree depend on directory order, so the collision is named and the
			// first (sorted) directory wins -- the root always keeping its own.
			if prev.dir != d {
				unreadable = append(unreadable, fmt.Sprintf(
					"%s and %s both claim run id %s; using %s. "+
						"the id in the log is what this tree links on, so a copied "+
						"run directory joins the tree twice under one name",
					prev.dir, d, id, prev.dir))
			}
			continue
		}
		index[id] = &treeNode{id: id, dir: d, st: st, simulated: sim}
	}
	return index, scanned, unreadable
}

// linkRunTree attaches children to parents and walks down from the root.
//
// # The cycle guard is reachable, and it took a rewrite to make it so
//
// Each run names exactly ONE parent, so a node lands in exactly one parent's
// child list and the descendants of the root cannot form a cycle among
// themselves -- reached by induction, not by hope: visiting a node twice would
// require it to appear under two parents.
//
// The one node a cycle CAN close on is the root, because the root is the only
// node the walk visits without having found it in a child list: root.parent = x
// with x.parent = root makes the root a child of its own child. An earlier
// version of this function skipped the root while building the child lists,
// which made that impossible -- and therefore made this guard dead code that
// silently truncated the loop instead of reporting it. The skip protected
// nothing else: a parent outside the subtree is never visited anyway, since the
// walk only ever descends.
//
// So the skip is gone and the guard fires. A log is a hand-editable file, and
// this is the first command somebody points at a log they repaired by hand; a
// self-parenting run (parent = own id) takes the same path.
//
// A node already visited is dropped from the walk and reported, rather than
// dropped quietly: it is spend that exists on disk, and its place in the tree is
// a claim two logs disagree about.
func linkRunTree(root *treeNode, index map[string]*treeNode) []string {
	var warnings []string

	byParent := map[string][]*treeNode{}
	for _, n := range index {
		if p := n.st.ParentRunID; p != "" {
			byParent[p] = append(byParent[p], n)
		}
	}
	for p := range byParent {
		sort.Slice(byParent[p], func(i, j int) bool { return byParent[p][i].id < byParent[p][j].id })
	}

	seen := map[string]bool{}
	var walk func(n *treeNode, depth int)
	walk = func(n *treeNode, depth int) {
		seen[n.id] = true
		n.depth = depth
		n.children = nil
		for _, ch := range byParent[n.id] {
			if seen[ch.id] {
				warnings = append(warnings, fmt.Sprintf(
					"run %s names %s as its parent but is already in this tree, so "+
						"the log describes a cycle; it is shown once and its spend "+
						"counted once", ch.id, n.id))
				continue
			}
			n.children = append(n.children, ch)
			walk(ch, depth+1)
		}
	}
	walk(root, 0)

	// A declared spawn_depth that disagrees with the shape of the tree is worth
	// one line, because nothing else in the system checks it: the reducer stores
	// the number and never reads it, so this view is the only place the two
	// readings of "how deep is this" ever meet.
	for _, n := range flattenRunTree(root) {
		if n.st.SpawnDepth != n.depth {
			warnings = append(warnings, fmt.Sprintf(
				"run %s records spawn_depth %d but sits at depth %d in this tree; "+
					"the number in the log is not validated by the reducer, so one "+
					"of the two is wrong",
				n.id, n.st.SpawnDepth, n.depth))
		}
	}
	return warnings
}

// flattenRunTree returns the nodes in the order they are printed.
func flattenRunTree(root *treeNode) []*treeNode {
	out := []*treeNode{root}
	for _, ch := range root.children {
		out = append(out, flattenRunTree(ch)...)
	}
	return out
}

// treeOwnSpend is the measured total: every node's own SpentUSD, summed.
//
// SpentUSD and not TreeSpentUSD, and that choice is the whole point of this file.
// Summing TreeSpentUSD would double-count the moment a rollup is implemented, and
// today -- with no rollup -- it produces the same number by accident. Summing the
// per-run figure is correct in both worlds, which is the property worth having in
// the one line of output the user is most likely to quote.
func treeOwnSpend(nodes []*treeNode) float64 {
	total := 0.0
	for _, n := range nodes {
		total += n.st.SpentUSD
	}
	return total
}

// treeRow is a node plus the box-drawing prefix that puts it in its place.
type treeRow struct {
	prefix string
	n      *treeNode
}

// appendTreeRows lays out the branches.
//
// The prefix is built by passing down what the CHILD's indent must be, rather
// than by repeating a string depth times, because the two are not the same: a
// node under a middle sibling needs "│  " at that level and a node under the last
// sibling needs three spaces. Multiplying one indent by the depth draws a
// vertical bar down past the last child, which is the classic wrong tree.
func appendTreeRows(n *treeNode, prefix, childIndent string, out []treeRow) []treeRow {
	out = append(out, treeRow{prefix: prefix, n: n})
	for i, ch := range n.children {
		branch, cont := "├─ ", "│  "
		if i == len(n.children)-1 {
			branch, cont = "└─ ", "   "
		}
		out = appendTreeRows(ch, childIndent+branch, childIndent+cont, out)
	}
	return out
}

// colWidth measures a column in runes, not bytes.
//
// The prefixes are box-drawing characters: "└─ " is nine bytes and three
// columns. Sizing with len() pads every branch line by six spaces per level,
// which bends the tree to the right as it deepens -- measured on the first run,
// and the only reason the tree looked wrong before it looked right.
func colWidth(s string) int { return utf8.RuneCountInString(s) }

func pad(s string, w int) string {
	if n := w - colWidth(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// printRunTree renders §20.7's shape, then the figure no single state can give.
//
// # No header row, unlike `run list`, and that is a decision rather than an omission
//
// The two views were compared side by side on the same run before this was
// written. `run list` prints RUN ACTOR STATUS STAGE SEQ SPENT ASKS and needs to:
// SEQ and ASKS are bare integers, and a column of small numbers with no label
// above it says nothing at all -- a reader cannot tell 7 turns from event 7.
//
// Nothing here is a bare number. The spend column carries its own units and its
// own ceiling ("0.04 of 5 USD"), the status is an English word, and `[sim]` names
// itself. A header would buy no meaning, and it would cost the one thing this
// view has that the list does not: the first column is indented box-drawing whose
// width changes with the depth of the tree, so a heading over it would sit above
// the drawing rather than above the ids.
//
// # Widths are measured from the content, and nothing is truncated
//
// `run list` shortens a long actor, status or stage with an ellipsis, which is
// right for a view whose job is to fit many unrelated runs on a screen. It never
// shortens an id, and its own comment records why: a fixed width of 14 printed
// "rmthws2dz-933…" for every real 18-character id, and pasting that into the
// command the listing exists to feed answered "holds no event log". This view
// elides nothing at all, because the actor here is the roster name a reader
// matches against the blueprint and the ids are what they type into `arxi run
// show`, which the closing line tells them to. A wide tree therefore wraps, which
// is a cost the reader can see and recover from.
func printRunTree(root *treeNode, nodes []*treeNode, sc treeScan) {
	rows := appendTreeRows(root, "", "", nil)

	idW, actorW, statusW := 0, 0, 0
	for _, r := range rows {
		if w := colWidth(r.prefix + r.n.id); w > idW {
			idW = w
		}
		if w := colWidth(r.n.st.Actor); w > actorW {
			actorW = w
		}
		if w := colWidth(string(r.n.st.Status)); w > statusW {
			statusW = w
		}
	}

	for _, r := range rows {
		fmt.Printf("%s  %s  %s  %s",
			pad(r.prefix+r.n.id, idW),
			pad(r.n.st.Actor, actorW),
			pad(string(r.n.st.Status), statusW),
			nodeSpend(r.n))
		if m := nodeMarks(r.n); m != "" {
			fmt.Printf("  %s", m)
		}
		fmt.Println()
	}

	printTreeTotal(root, nodes, sc)
}

// nodeSpend is the run's OWN spend, which is what the total below sums.
//
// The ceiling beside it is the run's own too, because a child's --budget is a
// sub-ceiling inside the parent's pool (§20.7) and eliding it would hide the
// number that stopped that particular run. TreeSpentUSD is printed only when it
// differs from SpentUSD -- today it never does, and saying "0.6 USD (tree says
// 0.6)" on every line would be noise that trains the reader to skip the column
// that matters the day a rollup lands.
func nodeSpend(n *treeNode) string {
	own := trimUSD(n.st.SpentUSD)
	s := own + " USD"
	if n.st.BudgetUSD > 0 {
		s = own + " of " + trimUSD(n.st.BudgetUSD) + " USD"
	}
	if n.st.TreeSpentUSD != n.st.SpentUSD {
		s += " (subtree " + trimUSD(n.st.TreeSpentUSD) + ")"
	}
	return s
}

// nodeMarks says what is wrong with a node, in the fewest words that still act.
//
// "sim" is first because it invalidates every other figure on the line: a
// rehearsal's spend is not money. The overspend is stated rather than left to be
// inferred from the two numbers beside it, which is the defect `run show` shipped
// with -- "0.02 of 0.001 USD" reads as ordinary progress -- and it compares
// TreeSpentUSD, because that is the figure the reducer actually enforces.
func nodeMarks(n *treeNode) string {
	var marks []string
	if n.simulated {
		marks = append(marks, "sim")
	}
	if n.st.BudgetUSD > 0 && n.st.TreeSpentUSD > n.st.BudgetUSD {
		marks = append(marks, "OVER by "+trimUSD(n.st.TreeSpentUSD-n.st.BudgetUSD))
	}
	if a := pendingAsks(n.st); a > 0 {
		marks = append(marks, fmt.Sprintf("%d question%s waiting", a, plural(a)))
	}
	if len(marks) == 0 {
		return ""
	}
	return "[" + strings.Join(marks, ", ") + "]"
}

// printTreeTotal is the line this whole verb exists for, and the notes that keep
// it from being read as more certain than it is.
func printTreeTotal(root *treeNode, nodes []*treeNode, sc treeScan) {
	total := treeOwnSpend(nodes)

	fmt.Println()
	if root.st.BudgetUSD > 0 {
		fmt.Printf("tree total: %s of %s USD across %d run%s\n",
			trimUSD(total), trimUSD(root.st.BudgetUSD), len(nodes), plural(len(nodes)))
	} else {
		fmt.Printf("tree total: %s USD across %d run%s, no ceiling on the root\n",
			trimUSD(total), len(nodes), plural(len(nodes)))
	}
	fmt.Printf("  measured by summing each run's own spend, not read off the root\n")

	printTreeNotes(root, nodes, total, sc)
}

// printTreeNotes says the things a tree of numbers cannot say for itself.
//
// Each of these is a specific wrong conclusion a reader would otherwise reach,
// and none of them is decoration:
//
//   - a total over the ceiling with nothing blocked looks like the ceiling works
//   - a root figure lower than the total looks like this view added something up
//     wrong, when it is the state that does not roll up
//   - a lonely root looks like lost children
//   - a lonely root next to runs that DO name a parent looks like a broken link
//     when the truth is they belong to another tree
//   - a rehearsal total looks like a bill
//   - a subtree total looks like a whole-tree total
func printTreeNotes(root *treeNode, nodes []*treeNode, total float64, sc treeScan) {
	if root.st.BudgetUSD > 0 && total > root.st.BudgetUSD {
		fmt.Printf("\nthe tree is OVER the root's ceiling by %s USD.\n"+
			"  no run is necessarily blocked by that: applyCost adds a cost to the\n"+
			"  spending run's own SpentUSD and TreeSpentUSD and never rolls it up to\n"+
			"  the parent, so each run enforces its own budget alone. This is the\n"+
			"  failure docs/design/10-execution.md §10.7 names -- the ceiling is\n"+
			"  still displayed and is simply untrue -- and it is a gap in the\n"+
			"  reducer, not in this reading.\n",
			trimUSD(total-root.st.BudgetUSD))
	}

	if len(nodes) > 1 && root.st.TreeSpentUSD != total {
		fmt.Printf("\nrun %s records tree_spent_usd %s, but its subtree has spent %s.\n"+
			"  the field is named for the subtree and holds only this run's own\n"+
			"  costs; the total above is measured here instead. Trusting the field\n"+
			"  would understate the bill by %s USD.\n",
			root.id, trimUSD(root.st.TreeSpentUSD), trimUSD(total),
			trimUSD(total-root.st.TreeSpentUSD))
	}

	if len(nodes) == 1 {
		// Two different sentences, chosen by what is on disk. The first is a fact
		// about this binary and the second is a fact about these runs, and saying
		// the first while the second is true is how a view loses its reader: they
		// would be told the link is never written by a command that had just read
		// a run writing it.
		if n := len(sc.parented); n > 0 {
			verb := "name"
			if n == 1 {
				verb = "names"
			}
			fmt.Printf("\nrun %s has no children in this tree.\n"+
				"  parent_run_id IS written somewhere: %d run%s under %s/ %s a parent\n"+
				"  that is not %s (%s). A run meant to be a child of %s names a\n"+
				"  different one.\n",
				root.id, n, plural(n), runsDir, verb, root.id,
				strings.Join(sc.parented, ", "), root.id)
		} else {
			dirs := "directories"
			if sc.scanned == 1 {
				dirs = "directory"
			}
			fmt.Printf("\nrun %s has no children, and cannot have any yet.\n"+
				"  nothing in this binary writes parent_run_id: `run start` has no\n"+
				"  --parent and no effect spawns a run, so a tree of one is the only\n"+
				"  tree it can produce. `run fork` is built and is NOT delegation: it\n"+
				"  makes a sibling with a shared past, and withholds parent_run_id on\n"+
				"  purpose, because a fork's copied prefix already reports the parent's\n"+
				"  spend and summing it as a child would bill it twice (fork.go:72).\n"+
				"  This walk reads the real field, so it starts describing\n"+
				"  real trees the day spawning lands.\n"+
				"  scanned %d run %s under %s/ for children.\n",
				root.id, sc.scanned, dirs, runsDir)
		}
	}

	// A rehearsal total and a bill must not read the same, and a tree that mixes
	// them is the case neither word covers. Counting rather than checking the
	// root's own flag is deliberate: the root is the node whose flag a reader is
	// least likely to doubt, so a real child under a simulated root would
	// otherwise be summed into a total labelled "no money was spent".
	sims := 0
	for _, n := range nodes {
		if n.simulated {
			sims++
		}
	}
	switch {
	case sims == len(nodes):
		fmt.Printf("\nevery run here was started with --sim, so the total above is a\n" +
			"  rehearsal: no model was called and no money was spent.\n")
	case sims > 0:
		var names []string
		for _, n := range nodes {
			if n.simulated {
				names = append(names, n.id)
			}
		}
		fmt.Printf("\nthis tree mixes simulated runs with real ones, so the total is part\n"+
			"  rehearsal and part bill. Simulated: %s.\n", strings.Join(names, ", "))
	}

	// A subtree total is the easiest figure in this file to misread, because it
	// is correct and answers a question the reader did not ask.
	if root.st.ParentRunID != "" {
		fmt.Printf("\nrun %s has a parent (%s), so this is a SUBTREE and its total is not\n"+
			"  the whole tree's: arxi run tree %s\n",
			root.id, root.st.ParentRunID, root.st.ParentRunID)
	}

	fmt.Printf("\none run in detail: arxi run show %s\n", root.id)
}

// runTreePayload is the machine reading, and it carries BOTH totals.
//
// tree_spent_usd is the measured sum; root_tree_spent_usd is what the root's
// state claims. A caller given only one of them cannot detect the gap this file
// exists to report, and a caller given only the measured figure would silently
// disagree with `run show` on the same run. Naming both, with rollup_missing
// saying which way they differ, is what lets a script assert the invariant
// instead of trusting whichever number it happened to read.
func runTreePayload(root *treeNode, nodes []*treeNode, sc treeScan) map[string]any {
	total := treeOwnSpend(nodes)

	out := map[string]any{
		"root":                root.id,
		"runs":                len(nodes),
		"runs_scanned":        sc.scanned,
		"budget_usd":          root.st.BudgetUSD,
		"tree_spent_usd":      total,
		"root_tree_spent_usd": root.st.TreeSpentUSD,
		"tree":                runTreeNodePayload(root),
	}
	if root.st.ParentRunID != "" {
		// A subtree is flagged in the payload as well as in the prose, because a
		// caller summing this total as "the whole tree" is the same mistake a
		// reader makes, with nobody to notice it.
		out["parent_run_id"] = root.st.ParentRunID
		out["subtree"] = true
	}
	if root.st.BudgetUSD > 0 && total > root.st.BudgetUSD {
		out["over_budget_usd"] = total - root.st.BudgetUSD
	}
	if root.st.TreeSpentUSD != total {
		out["rollup_missing_usd"] = total - root.st.TreeSpentUSD
	}
	if len(sc.unreadable) > 0 {
		// Same key and same reason as `run list`: a consumer that cannot tell "no
		// children" from "I could not read them" will report a small total with
		// full confidence.
		out["unreadable"] = sc.unreadable
	}
	if len(sc.warnings) > 0 {
		out["warnings"] = sc.warnings
	}
	if len(sc.parented) > 0 {
		// Named in the payload too, because the caller that most needs it is a
		// script asserting "this total is the whole bill": a run naming a parent
		// that is not in this tree is the one thing that can make that false
		// without anything looking wrong.
		out["parented_elsewhere"] = sc.parented
	}
	return out
}

// runTreeNodePayload is one node, nested -- the shape IS the answer here.
//
// A flat list with parent ids would make every consumer rebuild the walk,
// including the cycle protection, and the first one to skip it would hang. The
// nesting is the thing this verb computed.
//
// depth and spawn_depth are both present and are different fields: the first is
// measured by the walk, the second is copied from the log and validated by
// nothing. Collapsing them would destroy the only evidence that they disagree.
func runTreeNodePayload(n *treeNode) map[string]any {
	out := map[string]any{
		"run":            n.id,
		"dir":            n.dir,
		"actor":          n.st.Actor,
		"status":         string(n.st.Status),
		"terminal":       n.st.Status.Terminal(),
		"seq":            n.st.Seq,
		"turns":          n.st.Turns,
		"spent_usd":      n.st.SpentUSD,
		"tree_spent_usd": n.st.TreeSpentUSD,
		"budget_usd":     n.st.BudgetUSD,
		"depth":          n.depth,
		"spawn_depth":    n.st.SpawnDepth,
		"simulated":      n.simulated,
		"pending_asks":   pendingAsks(n.st),
	}
	if n.st.Stage != "" {
		out["stage"] = n.st.Stage
	}
	if n.st.Result != "" {
		out["result"] = n.st.Result
	}
	if len(n.children) > 0 {
		kids := make([]map[string]any, 0, len(n.children))
		for _, ch := range n.children {
			kids = append(kids, runTreeNodePayload(ch))
		}
		out["children"] = kids
	}
	return out
}
