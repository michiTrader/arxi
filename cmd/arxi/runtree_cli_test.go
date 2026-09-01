package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// `arxi run tree`, exercised as a process against real run directories.
//
// The verb exists for one number -- what a TREE of runs cost -- and the measured
// reality is that no state in the system holds it. applyCost
// (internal/kernel/decide.go:958) adds a cost to the spending run's own SpentUSD
// and TreeSpentUSD and never rolls it up to the parent. So most of what is
// asserted here is the difference between what the tree really cost and what any
// single run claims, plus the sentences that stop a reader taking one for the
// other.
//
// # Why the fixtures write parent_run_id by hand
//
// Nothing in this binary writes it yet: `run start` has no --parent, no effect
// spawns a run, and `run fork` is declared and unbuilt. Building the fixtures
// with the code under test is therefore not merely inadvisable here -- it is
// impossible, because there is no code that produces a tree. Hand-written logs
// are how the shape this view exists for gets exercised BEFORE spawning lands,
// and the day it lands these tests already say what the output must be.
//
// The one thing that must not slip: a fixture has to describe a state the
// reducer really reaches. Each event type below was checked against decide.go
// rather than guessed, the way `run show`'s fixtures were.

// treeRun describes one run directory.
//
// A struct and not eight positional parameters, because half of them are numbers
// and a call site reading `..., "", 1, 20, 14.2, false, ""` is one no reader can
// check against the assertion under it.
type treeRun struct {
	id     string  // the run id INSIDE the log, which is what the tree links on
	at     string  // the directory under runs/; defaults to id
	actor  string  // defaults to "team"
	parent string  // parent_run_id, written only when set
	depth  int     // spawn_depth: the number the reducer stores and never checks
	budget float64 // 0 is "no ceiling", which is how this view reads it
	spent  float64 // charged with one llm.response
	sim    bool    // run.started's simulated flag, which foldRunDir reads
	extra  string  // further log lines, verbatim
}

// writeTreeRun lays down one run whose log folds to the requested shape.
//
// runAt (runlist_cli_test.go) writes the directory and the frozen blueprint, and
// then the log is REPLACED rather than patched. runAt's run.started carries no
// parent_run_id, and editing a parent into an existing JSON line by string
// surgery is how a fixture starts passing for the wrong reason -- one malformed
// line and foldRunDir reports a broken log, which several tests below treat as a
// result rather than as an accident.
//
// id and at are separate so that two directories can claim one run id, which is
// what copying a run directory as a backup produces.
func writeTreeRun(t *testing.T, dir string, f treeRun) {
	t.Helper()

	if f.at == "" {
		f.at = f.id
	}
	if f.actor == "" {
		f.actor = "team"
	}
	runAt(t, dir, f.at, f.actor, f.budget, "")

	payload := `"actor":"` + f.actor + `","run_id":"` + f.id +
		`","budget_usd":` + trimFloat(f.budget)
	if f.parent != "" {
		payload += `,"parent_run_id":"` + f.parent + `"`
	}
	if f.depth != 0 || f.parent != "" {
		payload += `,"spawn_depth":` + strconv.Itoa(f.depth)
	}
	if f.sim {
		payload += `,"simulated":true`
	}

	log := `{"id":"e1","seq":1,"type":"run.started","payload":{` + payload + `}}
{"id":"e2","seq":2,"type":"stage.entered","payload":{"stage":"execute","index":0}}
`
	// llm.response, because that is the case that reaches applyCost
	// (decide.go:108) and applyCost charges SpentUSD whatever the actor is -- so
	// this is the smallest event that gives a run a real spend, with no
	// agent.activated needed. Checked against the reducer rather than assumed: a
	// fixture that spent nothing would make every total below agree by accident.
	if f.spent != 0 {
		log += `{"id":"e3","seq":3,"type":"llm.response","actor":"backend","payload":{"cost_usd":` +
			trimFloat(f.spent) + `}}
`
	}

	if err := os.WriteFile(filepath.Join(dir, "runs", f.at, "events.ndjson"),
		[]byte(log+f.extra), 0o644); err != nil {
		t.Fatal(err)
	}
}

// treeLine returns the FIRST output line mentioning needle.
//
// First and not last, deliberately: the rows are printed before the notes, so
// the first mention of a run id is its row. That is what makes the box-drawing
// prefix assertable at all -- the notes mention the same ids with no prefix, and
// a search that found one of those would pass whatever the tree looked like.
func treeLine(t *testing.T, out, needle string) string {
	t.Helper()
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, needle) {
			return ln
		}
	}
	t.Fatalf("no line of the output mentions %q\n%s", needle, out)
	return ""
}

// nearly compares money the way a sum of floats has to be compared.
//
// 14.2 + 2.9 + 0.6 is not 17.7 in binary floating point, and == would fail on a
// total that is right to every decimal a bill has. The rendered output is
// asserted as a string, where trimUSD has already rounded to four places; this is
// only for the JSON, which carries the raw number on purpose.
func nearly(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// A tree of one must say the link cannot be written yet, not go quiet.
//
// THE DEFECT this pins is the difference between "no children" and "children not
// found". A view that printed one run and stopped would read as a run whose
// children were lost, and the reader would go looking for a bug in the walk. The
// truth is that no code writes parent_run_id yet, and saying so -- with the
// number of directories that were searched -- is the whole answer.
func TestRunTreeSaysALonelyRootCannotHaveChildrenYet(t *testing.T) {
	dir := workdir(t)
	writeTreeRun(t, dir, treeRun{id: "solo-000000000001", budget: 5, spent: 0.4})

	got := arxi(t, dir, "run", "tree", "solo-000000000001")
	if got.code != 0 {
		t.Fatalf("exit %d, want 0\n%s", got.code, got.out)
	}
	if !strings.Contains(got.out, "cannot have any yet") {
		t.Errorf("a childless root does not say why it is childless.\n%s\n"+
			"  consequence: a tree of one looks like a tree whose children this "+
			"view failed to find, and the reader debugs the walk instead of "+
			"learning that nothing writes parent_run_id yet.", got.out)
	}
	if !strings.Contains(got.out, "scanned 1 run directory under runs/") {
		t.Errorf("the search is not reported, or is reported in the plural for one directory.\n%s\n"+
			"  consequence: \"no children\" with no count is unfalsifiable -- the "+
			"reader cannot tell whether anything was looked at.", got.out)
	}
	if !strings.Contains(got.out, "tree total: 0.4 of 5 USD across 1 run\n") {
		t.Errorf("the total of a one-run tree is wrong or pluralised.\n%s", got.out)
	}
	if strings.Contains(got.out, "warning:") {
		t.Errorf("a healthy run produces a warning.\n%s\n"+
			"  consequence: warnings on a sound run teach the reader to ignore "+
			"them, and the cycle and spawn_depth warnings below are the ones that "+
			"matter.", got.out)
	}
}

// The total is MEASURED, and the disagreement with the root is stated.
//
// The §20.7 shape: a root that has spent 14.20 of 20.00 with two descendants at
// 2.90 and 0.60. The root's own tree_spent_usd says 14.2, because applyCost never
// rolled the children up, so a view that printed that field would tell a user
// their tree cost 14.20 when it cost 17.70.
//
// THE DEFECT this pins is the reason the verb was written: reading the number off
// the root is the obvious implementation, it type-checks, it agrees with
// `run show`, and it understates the bill by 3.50.
func TestRunTreeMeasuresTheTotalItselfInsteadOfTrustingTheRoot(t *testing.T) {
	dir := workdir(t)
	writeTreeRun(t, dir, treeRun{id: "tree-root-00000001", budget: 20, spent: 14.2})
	writeTreeRun(t, dir, treeRun{id: "tree-mid-000000001",
		parent: "tree-root-00000001", depth: 1, spent: 2.9})
	writeTreeRun(t, dir, treeRun{id: "tree-leaf-00000001",
		parent: "tree-mid-000000001", depth: 2, spent: 0.6})

	got := arxi(t, dir, "run", "tree", "tree-root-00000001")
	if got.code != 0 {
		t.Fatalf("exit %d, want 0\n%s", got.code, got.out)
	}

	if !strings.Contains(got.out, "tree total: 17.7 of 20 USD across 3 runs") {
		t.Errorf("the tree total is not the sum of the three runs.\n%s\n"+
			"  consequence: 14.2 is what the root's own state claims and is the "+
			"number this command exists to correct. Printing it makes the view "+
			"agree with `run show` and understate the bill by 3.5.", got.out)
	}
	if !strings.Contains(got.out, "records tree_spent_usd 14.2, but its subtree has spent 17.7") {
		t.Errorf("the gap between the root's figure and the measured one is not named.\n%s\n"+
			"  consequence: a reader who has seen `run show` on the root now has "+
			"two numbers and no way to tell which is wrong -- and will assume this "+
			"view added up badly, since a state is easier to trust than a sum.",
			got.out)
	}
	if !strings.Contains(got.out, "understate the bill by 3.5") {
		t.Errorf("the size of the gap is not stated.\n%s", got.out)
	}

	if ln := treeLine(t, got.out, "tree-mid-000000001"); !strings.HasPrefix(ln, "└─ ") {
		t.Errorf("the only child of the root is not drawn as a last child: %q\n%s", ln, got.out)
	}
	if ln := treeLine(t, got.out, "tree-leaf-00000001"); !strings.HasPrefix(ln, "   └─ ") {
		t.Errorf("the grandchild is not indented under its parent: %q\n%s\n"+
			"  consequence: depth is the one thing this view adds over run list. A "+
			"grandchild drawn at the child's indent describes a flat roster.",
			ln, got.out)
	}

	if strings.Contains(got.out, "has no children") {
		t.Errorf("a tree of three says it has no children.\n%s", got.out)
	}
	if strings.Contains(got.out, "warning:") {
		t.Errorf("a well-formed tree produces a warning.\n%s", got.out)
	}
}

// A tree can be over the root's ceiling with every run inside its own.
//
// 0.60 under a ceiling of 1.00, plus a child that spent 0.90 under no ceiling at
// all. The tree cost 1.50 against a root ceiling of 1.00 and NOTHING blocked,
// because applyCost charges the spending run and each run enforces only its own
// BudgetUSD.
//
// THE DEFECT this pins is a view that reports the breach in the vocabulary of
// enforcement. The absent string is the load-bearing assertion: "OVER by" is the
// per-run mark, and if it appeared on either row this view would be claiming a
// run is over a ceiling that the reducer never compared it against.
func TestRunTreeSaysTheCeilingStoppedNothing(t *testing.T) {
	dir := workdir(t)
	writeTreeRun(t, dir, treeRun{id: "cap-root-000000001", budget: 1, spent: 0.6})
	writeTreeRun(t, dir, treeRun{id: "cap-kid-0000000001",
		parent: "cap-root-000000001", depth: 1, spent: 0.9})

	got := arxi(t, dir, "run", "tree", "cap-root-000000001")
	if got.code != 0 {
		t.Fatalf("exit %d, want 0\n%s", got.code, got.out)
	}

	if !strings.Contains(got.out, "the tree is OVER the root's ceiling by 0.5 USD") {
		t.Errorf("a tree that cost 1.5 under a ceiling of 1 does not say so.\n%s\n"+
			"  consequence: the one number this view exists for is past the only "+
			"limit anybody set, and the output reads as an ordinary tree.", got.out)
	}
	if strings.Contains(got.out, "OVER by") {
		t.Errorf("a run is marked OVER its own ceiling when neither run is.\n%s\n"+
			"  consequence: this is the enforcement claim §10.7 makes and the code "+
			"does not. The root spent 0.6 of 1 and the child has no ceiling, so a "+
			"row marked OVER sends the reader to `run unpause --budget` for a run "+
			"that is not blocked and will not become unblocked.", got.out)
	}
	if !strings.Contains(got.out, "running") {
		t.Errorf("neither run is reported as running, though nothing blocked.\n%s\n"+
			"  consequence: the breach above is only readable as harmless if the "+
			"statuses next to it say the runs kept going.", got.out)
	}
}

// A cycle in the logs terminates, and is reported rather than drawn.
//
// Two runs each naming the other as parent. No code writes parent_run_id yet, so
// this is not a state the reducer can currently reach -- but the field is read off
// a log, a log is a file, and a walk that follows it must not depend on the file
// being sane. A recursive walk over this shape without a seen-set does not print a
// bad tree, it exhausts the stack.
//
// THE DEFECT this pins is therefore twofold, and the exit code is half of it: the
// command has to come back at all, and having come back it must say the log is
// impossible instead of quietly drawing one of the two orderings as if it were
// the truth.
func TestRunTreeReportsACycleInsteadOfWalkingIt(t *testing.T) {
	dir := workdir(t)
	writeTreeRun(t, dir, treeRun{id: "loop-a-000000001",
		parent: "loop-b-000000001", spent: 1})
	writeTreeRun(t, dir, treeRun{id: "loop-b-000000001",
		parent: "loop-a-000000001", depth: 1, spent: 2})

	got := arxi(t, dir, "run", "tree", "loop-a-000000001")
	if got.code != 0 {
		t.Fatalf("exit %d, want 0 -- a cycle is a fact about the log, not a "+
			"failure of the command\n%s", got.code, got.out)
	}

	if !strings.Contains(got.out,
		"run loop-a-000000001 names loop-b-000000001 as its parent but is already in this tree") {
		t.Errorf("the run that closes the cycle is not named.\n%s\n"+
			"  consequence: without both ids the reader cannot tell which link to "+
			"go and look at, and the two logs are the only evidence.", got.out)
	}
	if !strings.Contains(got.out, "the log describes a cycle") {
		t.Errorf("the shape is not called a cycle.\n%s", got.out)
	}
	if !strings.Contains(got.out, "across 2 runs") {
		t.Errorf("the cycle is counted twice, or not at all.\n%s\n"+
			"  consequence: counting loop-a again where the cycle closes doubles "+
			"its spend into the total, and the number this view exists for becomes "+
			"a function of where the walk happened to start.", got.out)
	}
}

// A run that is its own parent is the same defect with one node.
//
// Worth its own test because the seen-set is what makes it terminate, and a
// membership check written as "is this child already among MY children" would
// pass the two-node case above and still recurse forever here.
func TestRunTreeReportsASelfParentingRun(t *testing.T) {
	dir := workdir(t)
	writeTreeRun(t, dir, treeRun{id: "self-000000000001",
		parent: "self-000000000001", spent: 1})

	got := arxi(t, dir, "run", "tree", "self-000000000001")
	if got.code != 0 {
		t.Fatalf("exit %d, want 0\n%s", got.code, got.out)
	}
	if !strings.Contains(got.out,
		"run self-000000000001 names self-000000000001 as its parent but is already in this tree") {
		t.Errorf("a run parented to itself is not reported.\n%s", got.out)
	}
	if !strings.Contains(got.out, "across 1 run") {
		t.Errorf("the self-parenting run is counted more than once.\n%s\n"+
			"  consequence: its spend would be added twice for a tree that holds "+
			"exactly one run.", got.out)
	}
}

// spawn_depth is stored and never checked, so the two depths can disagree.
//
// applyRunStarted (decide.go:225-245) copies spawn_depth into the state and
// nothing anywhere compares it to anything -- not to the parent's depth, not to a
// maximum. A child written at depth 5 directly under the root is a state the
// reducer accepts without complaint.
//
// THE DEFECT this pins is a view that prints st.SpawnDepth as the indent. That
// reads as a tree five levels deep containing two runs, and every conclusion drawn
// from the shape is then wrong. The walk's own depth is the structural one; the
// log's number is reported as a disagreement, because one of the two is a bug and
// this view cannot tell which.
func TestRunTreeReportsASpawnDepthThatDisagreesWithTheWalk(t *testing.T) {
	dir := workdir(t)
	writeTreeRun(t, dir, treeRun{id: "depth-root-0000001", budget: 5, spent: 0.1})
	writeTreeRun(t, dir, treeRun{id: "depth-kid-00000001",
		parent: "depth-root-0000001", depth: 5, spent: 0.2})

	got := arxi(t, dir, "run", "tree", "depth-root-0000001")
	if got.code != 0 {
		t.Fatalf("exit %d, want 0 -- an unvalidated field disagreeing is not a "+
			"reason to refuse the tree\n%s", got.code, got.out)
	}
	if !strings.Contains(got.out, "records spawn_depth 5 but sits at depth 1 in this tree") {
		t.Errorf("the disagreement between the logged depth and the walked one is not reported.\n%s\n"+
			"  consequence: both numbers exist and nothing in the reducer keeps "+
			"them in step. A view that shows one and hides the other lets a reader "+
			"believe the field is maintained.", got.out)
	}
	if ln := treeLine(t, got.out, "depth-kid-00000001"); !strings.HasPrefix(ln, "└─ ") {
		t.Errorf("the child is not drawn at the depth the walk found: %q\n%s\n"+
			"  consequence: indenting by spawn_depth draws a five-deep tree out of "+
			"two runs, from a number no code validates.", ln, got.out)
	}
}

// One unreadable directory must not cost the reader the whole tree.
//
// decodeRunEvents is deliberately strict -- any line but a truncated last one is
// fatal, because the log is the run's only history. That is right for the run being
// folded and wrong for a SIBLING: this walk reads every directory under runs/ to
// find children, so one corrupt log elsewhere would take out a view of a tree it
// has nothing to do with.
//
// THE DEFECT this pins is exit 1 on a healthy root. The broken directory is named,
// as a warning, and the total still prints -- with the caveat that the total is now
// a floor rather than a fact, which is why the count of scanned directories
// (2) and the count of runs in the tree (1) differ on purpose.
func TestRunTreeWarnsAboutAnUnreadableSiblingInsteadOfAborting(t *testing.T) {
	dir := workdir(t)
	writeTreeRun(t, dir, treeRun{id: "intact-0000000001", budget: 2, spent: 0.5})

	broken := filepath.Join(dir, "runs", "broken-0000000001")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatal(err)
	}
	// A whole line of nonsense, not a truncated one: a truncated final line is
	// SKIPPED by decodeRunEvents on purpose (a log being appended to right now
	// ends mid-line), so a fixture built that way would produce no error at all
	// and this test would pass while asserting nothing.
	if err := os.WriteFile(filepath.Join(broken, "events.ndjson"),
		[]byte("{not json at all}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := arxi(t, dir, "run", "tree", "intact-0000000001")
	if got.code != 0 {
		t.Fatalf("exit %d, want 0 -- a corrupt log in an unrelated directory "+
			"must not deny the reader this tree\n%s", got.code, got.out)
	}
	if !strings.Contains(got.out, "warning:") || !strings.Contains(got.out, "does not parse") {
		t.Errorf("the unreadable directory is skipped silently.\n%s\n"+
			"  consequence: if broken-0000000001 WERE a child, the tree would be "+
			"missing a run and its spend with no sign that anything was left out.",
			got.out)
	}
	if !strings.Contains(got.out, "tree total: 0.5 of 2 USD across 1 run\n") {
		t.Errorf("the total of the readable part is not printed.\n%s", got.out)
	}
	if !strings.Contains(got.out, "scanned 2 run directories under runs/") {
		t.Errorf("the unreadable directory is not counted among those scanned.\n%s\n"+
			"  consequence: scanned is what makes \"no children\" falsifiable. A "+
			"directory that was looked at and could not be read still has to be "+
			"in that number, or the two counts silently agree and hide the skip.",
			got.out)
	}
}

// Two directories claiming one run id are named, and counted once.
//
// The tree links on the run id INSIDE the log, not on the directory name, and
// nothing enforces that the two agree. Copying a run directory as a backup --
// `cp -r runs/x runs/x.bak`, which is a thing an operator does before trying
// something -- produces exactly this: two logs, one id.
//
// THE DEFECT this pins is a tree that joins twice under one name and counts the
// spend twice. The total would then depend on how many backups happen to be lying
// around, which is the one property a bill must not have.
func TestRunTreeNamesTwoDirectoriesClaimingOneRunId(t *testing.T) {
	dir := workdir(t)
	writeTreeRun(t, dir, treeRun{id: "shared-000000001", at: "copy-a", budget: 3, spent: 0.7})
	writeTreeRun(t, dir, treeRun{id: "shared-000000001", at: "copy-b", budget: 3, spent: 0.7})

	got := arxi(t, dir, "run", "tree", "copy-a")
	if got.code != 0 {
		t.Fatalf("exit %d, want 0\n%s", got.code, got.out)
	}
	if !strings.Contains(got.out, "both claim run id shared-000000001") {
		t.Errorf("two directories claiming one run id are not reported.\n%s\n"+
			"  consequence: whichever the walk indexed last silently decides what "+
			"the tree links on, and the reader has no way to learn that a stale "+
			"copy is the one being read.", got.out)
	}
	if !strings.Contains(got.out, "tree total: 0.7 of 3 USD across 1 run\n") {
		t.Errorf("the duplicate is counted as a second run.\n%s\n"+
			"  consequence: 1.4 for a run that spent 0.7, because a backup "+
			"directory exists.", got.out)
	}
}

// A wholly simulated tree says the total is a rehearsal.
//
// --sim drives the same reducer, the same loop and the same log, which is what
// makes a simulated log worth reading -- and is exactly why its numbers are
// indistinguishable from a real bill once they are in the log. kernel.State does
// not carry the flag at all; it lives only on run.started, which is why this view
// reads it off the event.
//
// THE DEFECT this pins is a rehearsal presented as money. "1.5 of 4 USD" with no
// qualifier is a sentence a reader takes to their finance channel.
func TestRunTreeSaysASimulatedTotalIsARehearsal(t *testing.T) {
	dir := workdir(t)
	writeTreeRun(t, dir, treeRun{id: "sim-root-000000001", budget: 4, spent: 0.2, sim: true})
	writeTreeRun(t, dir, treeRun{id: "sim-kid-0000000001",
		parent: "sim-root-000000001", depth: 1, spent: 0.3, sim: true})

	got := arxi(t, dir, "run", "tree", "sim-root-000000001")
	if got.code != 0 {
		t.Fatalf("exit %d, want 0\n%s", got.code, got.out)
	}
	if !strings.Contains(got.out, "[sim]") {
		t.Errorf("a simulated run is not marked on its own row.\n%s\n"+
			"  consequence: the note below applies to the whole tree; the mark is "+
			"what lets a reader see WHICH run it came from once a tree is mixed.",
			got.out)
	}
	if !strings.Contains(got.out, "every run here was started with --sim") {
		t.Errorf("a wholly simulated total is not called a rehearsal.\n%s", got.out)
	}
}

// A mixed tree is the dangerous one, and it says which runs are the rehearsal.
//
// THE DEFECT this pins is the note above being reused here. "every run here was
// started with --sim" on a tree containing a real run tells the reader the 0.2 the
// root really spent was imaginary -- and it is the more likely implementation,
// because a single boolean over the tree is the obvious way to carry this and
// answers "any" and "all" with the same field.
func TestRunTreeSaysWhenATreeMixesRehearsalWithBill(t *testing.T) {
	dir := workdir(t)
	writeTreeRun(t, dir, treeRun{id: "mix-root-000000001", budget: 4, spent: 0.2})
	writeTreeRun(t, dir, treeRun{id: "mix-kid-000000001",
		parent: "mix-root-000000001", depth: 1, spent: 0.3, sim: true})

	got := arxi(t, dir, "run", "tree", "mix-root-000000001")
	if got.code != 0 {
		t.Fatalf("exit %d, want 0\n%s", got.code, got.out)
	}
	if !strings.Contains(got.out, "mixes simulated runs with real ones") {
		t.Errorf("a part-rehearsal total is presented as a whole one.\n%s", got.out)
	}
	if !strings.Contains(got.out, "Simulated: mix-kid-000000001.") {
		t.Errorf("the simulated runs are not named.\n%s\n"+
			"  consequence: knowing a tree is mixed without knowing which part is "+
			"which makes the total unusable rather than merely qualified.", got.out)
	}
	if strings.Contains(got.out, "every run here was started with --sim") {
		t.Errorf("a mixed tree claims every run was simulated.\n%s\n"+
			"  consequence: the root spent real money and is being reported as a "+
			"rehearsal, which is the direction of this error that costs money.",
			got.out)
	}
}

// A sub-cent ceiling must not be rounded into nonsense.
//
// A ceiling of 0.001 with 0.002 spent. Money in this tool is USD as a float and
// the amounts a token-priced model produces are routinely in the fourth decimal, so
// formatting with %.2f -- the obvious choice for currency -- prints "0.00 of 0.00
// USD" for a run that is at twice its ceiling.
//
// THE DEFECT this pins is arithmetic that is right and output that is unreadable.
// This is also the one place the per-run OVER mark IS asserted: here a single run
// really is past its own BudgetUSD, which is the comparison the reducer makes,
// unlike the tree-wide breach above.
func TestRunTreeDoesNotRoundASubCentBudgetAway(t *testing.T) {
	dir := workdir(t)
	writeTreeRun(t, dir, treeRun{id: "tiny-000000000001", budget: 0.001, spent: 0.002})

	got := arxi(t, dir, "run", "tree", "tiny-000000000001")
	if got.code != 0 {
		t.Fatalf("exit %d, want 0\n%s", got.code, got.out)
	}
	for _, bad := range []string{"of 0 USD", "of 0.00 USD"} {
		if strings.Contains(got.out, bad) {
			t.Errorf("a ceiling of 0.001 is printed as %q.\n%s\n"+
				"  consequence: \"of 0 USD\" is how this view says there is NO "+
				"ceiling, so rounding turns a run at twice its limit into a run "+
				"with no limit -- the opposite of what the log says.", bad, got.out)
		}
	}
	if !strings.Contains(got.out, "0.001") {
		t.Errorf("the ceiling does not appear anywhere in the output.\n%s", got.out)
	}
	if !strings.Contains(got.out, "OVER by 0.001") {
		t.Errorf("a run past its own ceiling is not marked, or the overage is rounded.\n%s\n"+
			"  consequence: this run IS over the limit the reducer compares it "+
			"against, and the mark is the only thing on the row that says so.",
			got.out)
	}
}

// Asking about a child says the total is not the whole tree's.
//
// A subtree total is a correct answer to a question nobody asked. Somebody who
// copies a run id out of `run list` -- where children and roots look identical --
// gets the cost of a branch and reads it as the cost of the tree.
//
// THE DEFECT this pins is silence about the root's own parent, which is the one
// piece of information that makes the number interpretable, and which this view
// already has in hand because it folded the run.
func TestRunTreeSaysWhenTheRootIsOnlyASubtree(t *testing.T) {
	dir := workdir(t)
	writeTreeRun(t, dir, treeRun{id: "par-000000000001", budget: 9, spent: 1})
	writeTreeRun(t, dir, treeRun{id: "sub-000000000001",
		parent: "par-000000000001", depth: 1, spent: 2})

	got := arxi(t, dir, "run", "tree", "sub-000000000001")
	if got.code != 0 {
		t.Fatalf("exit %d, want 0\n%s", got.code, got.out)
	}
	if !strings.Contains(got.out, "this is a SUBTREE") {
		t.Errorf("a run with a parent does not say its tree is a branch.\n%s\n"+
			"  consequence: 2 USD is read as the cost of the tree, when the tree "+
			"cost 3.", got.out)
	}
	if !strings.Contains(got.out, "arxi run tree par-000000000001") {
		t.Errorf("the command for the real root is not offered.\n%s\n"+
			"  consequence: the reader has to know that the parent id is in "+
			"`run show` to ask the question they meant to ask.", got.out)
	}
	if !strings.Contains(got.out, "tree total: 2 USD across 1 run, no ceiling on the root") {
		t.Errorf("the subtree total, or the absence of a ceiling on the child, is wrong.\n%s\n"+
			"  consequence: the child has no budget_usd of its own -- the ceiling "+
			"is the parent's -- and printing \"of 0 USD\" would read as a ceiling "+
			"of nothing rather than no ceiling at all.", got.out)
	}
}

// When other runs DO name a parent, the childless note must not claim otherwise.
//
// The note in the first test says nothing writes parent_run_id yet, which is a fact
// about this binary. Here two hand-written logs contradict it -- and the day
// `run fork` lands, every log will.
//
// THE DEFECT this pins is the unconditional sentence: a command that had just read
// a run carrying parent_run_id telling its reader the field is never written. That
// is worse than saying nothing, because it is the kind of confident wrongness that
// stops the reader looking.
func TestRunTreePointsAtTheRunsThatDoNameAParent(t *testing.T) {
	dir := workdir(t)
	writeTreeRun(t, dir, treeRun{id: "alone-00000000001", budget: 5, spent: 0.3})
	writeTreeRun(t, dir, treeRun{id: "other-p-000000001", budget: 5, spent: 0.1})
	writeTreeRun(t, dir, treeRun{id: "other-k-000000001",
		parent: "other-p-000000001", depth: 1, spent: 0.2})

	got := arxi(t, dir, "run", "tree", "alone-00000000001")
	if got.code != 0 {
		t.Fatalf("exit %d, want 0\n%s", got.code, got.out)
	}
	if strings.Contains(got.out, "cannot have any yet") {
		t.Errorf("the view says parent_run_id is never written while two logs write it.\n%s\n"+
			"  consequence: a reader who has just built a tree by hand, or who is "+
			"using `run fork` the day it lands, is told the mechanism does not "+
			"exist -- by the command whose whole job is to read it.", got.out)
	}
	if !strings.Contains(got.out, "has no children in this tree") {
		t.Errorf("the childlessness of this root is not stated.\n%s", got.out)
	}
	if !strings.Contains(got.out, "1 run under runs/ names a parent") {
		t.Errorf("the runs that do name a parent are not counted, or are counted in the plural.\n%s\n"+
			"  consequence: \"runs ... name a parent\" for one run reads as "+
			"carelessness in the sentence whose job is to be trusted about what is "+
			"on disk.", got.out)
	}
	if !strings.Contains(got.out, "(other-k-000000001)") {
		t.Errorf("the run that names a parent is not named.\n%s\n"+
			"  consequence: a count with no ids cannot be checked, and the reader "+
			"has to grep the logs to find out whether one of them meant to be a "+
			"child of this root.", got.out)
	}

	asJSON := arxi(t, dir, "run", "tree", "alone-00000000001", "--json")
	if asJSON.code != 0 {
		t.Fatalf("exit %d, want 0\n%s", asJSON.code, asJSON.out)
	}
	if !strings.Contains(asJSON.out, "parented_elsewhere") ||
		!strings.Contains(asJSON.out, "other-k-000000001") {
		t.Errorf("--json drops what the human output says about runs parented elsewhere.\n%s\n"+
			"  consequence: the two renderings answer differently about the same "+
			"logs, and the machine one is the one nobody re-reads.", asJSON.out)
	}
}

// treeJSONNode is the shape a caller decodes, declared with only the fields the
// assertions read.
//
// Deliberately partial: a struct listing every key would fail on any addition,
// which is the wrong thing to guard. What must not change is the NESTING and the
// two totals, so those are what this describes.
type treeJSONNode struct {
	Run        string         `json:"run"`
	Depth      int            `json:"depth"`
	SpawnDepth int            `json:"spawn_depth"`
	SpentUSD   float64        `json:"spent_usd"`
	Children   []treeJSONNode `json:"children"`
}

type treeJSONDoc struct {
	Root      string       `json:"root"`
	Runs      int          `json:"runs"`
	Scanned   int          `json:"runs_scanned"`
	BudgetUSD float64      `json:"budget_usd"`
	Total     float64      `json:"tree_spent_usd"`
	RootClaim float64      `json:"root_tree_spent_usd"`
	Missing   float64      `json:"rollup_missing_usd"`
	Tree      treeJSONNode `json:"tree"`
}

// --json carries the nesting and BOTH totals, unrounded.
//
// The human rendering draws depth with box characters, which a program cannot read.
// The flat list `run list --json` already provides is not a substitute: it is
// exactly the thing this verb was written because it could not answer.
//
// THE DEFECT this pins is a payload that carries one total. Whichever one it were,
// the caller could not detect the discrepancy this command exists to report -- and
// a caller reading tree_spent_usd out of THIS payload would reasonably assume it
// meant what the same key means in `run show`, where it is the root's own field.
func TestRunTreeJSONCarriesTheNestingAndBothTotals(t *testing.T) {
	dir := workdir(t)
	writeTreeRun(t, dir, treeRun{id: "js-root-000000001", budget: 20, spent: 14.2})
	writeTreeRun(t, dir, treeRun{id: "js-mid-0000000001",
		parent: "js-root-000000001", depth: 1, spent: 2.9})
	writeTreeRun(t, dir, treeRun{id: "js-leaf-000000001",
		parent: "js-mid-0000000001", depth: 2, spent: 0.6})

	got := arxi(t, dir, "run", "tree", "js-root-000000001", "--json")
	if got.code != 0 {
		t.Fatalf("exit %d, want 0\n%s", got.code, got.out)
	}

	var doc treeJSONDoc
	if err := json.Unmarshal([]byte(got.out), &doc); err != nil {
		t.Fatalf("--json did not produce a single JSON document: %v\n%s\n"+
			"  consequence: anything printed alongside it -- a warning, a note, a "+
			"stray line -- makes the output undecodable, and the caller of a "+
			"--json flag has no way to recover the part that was valid.", err, got.out)
	}

	if doc.Root != "js-root-000000001" || doc.Runs != 3 || doc.Scanned != 3 {
		t.Errorf("root/runs/runs_scanned = %q/%d/%d, want js-root-000000001/3/3\n%s",
			doc.Root, doc.Runs, doc.Scanned, got.out)
	}
	if !nearly(doc.Total, 17.7) {
		t.Errorf("tree_spent_usd = %v, want the measured 17.7\n%s\n"+
			"  consequence: 14.2 here is the root's own field, and a caller "+
			"budgeting against it is 3.5 short.", doc.Total, got.out)
	}
	if doc.RootClaim != 14.2 {
		t.Errorf("root_tree_spent_usd = %v, want the root's own 14.2\n%s\n"+
			"  consequence: without the claimed figure alongside the measured one, "+
			"a caller cannot see the gap at all -- only this command's word for it.",
			doc.RootClaim, got.out)
	}
	if !nearly(doc.Missing, 3.5) {
		t.Errorf("rollup_missing_usd = %v, want 3.5\n%s", doc.Missing, got.out)
	}
	if doc.Total == doc.RootClaim {
		t.Errorf("the measured total and the root's claim are the same number (%v)\n%s\n"+
			"  consequence: this fixture exists BECAUSE applyCost does not roll up. "+
			"If these agree, either the payload reports one field twice or the "+
			"fixture stopped describing the state it was built for.",
			doc.Total, got.out)
	}
	if doc.BudgetUSD != 20 {
		t.Errorf("budget_usd = %v, want the root's ceiling 20\n%s", doc.BudgetUSD, got.out)
	}

	// The nesting, which is the whole reason this payload is not a list.
	if len(doc.Tree.Children) != 1 {
		t.Fatalf("the root has %d children in the payload, want 1\n%s",
			len(doc.Tree.Children), got.out)
	}
	mid := doc.Tree.Children[0]
	if mid.Run != "js-mid-0000000001" || len(mid.Children) != 1 {
		t.Fatalf("the root's child is %q with %d children, want js-mid-0000000001 with 1\n%s",
			mid.Run, len(mid.Children), got.out)
	}
	leaf := mid.Children[0]
	if leaf.Run != "js-leaf-000000001" {
		t.Errorf("the grandchild is %q, want js-leaf-000000001\n%s\n"+
			"  consequence: a leaf hoisted to the root's children describes a tree "+
			"two levels deep instead of three, and any cost attributed by branch "+
			"is then attributed to the wrong branch.", leaf.Run, got.out)
	}
	if doc.Tree.Depth != 0 || mid.Depth != 1 || leaf.Depth != 2 {
		t.Errorf("depths are %d/%d/%d, want 0/1/2\n%s",
			doc.Tree.Depth, mid.Depth, leaf.Depth, got.out)
	}
	if !nearly(leaf.SpentUSD, 0.6) {
		t.Errorf("the leaf's own spend is %v, want 0.6\n%s", leaf.SpentUSD, got.out)
	}
}

// A run id that is not on disk fails, and says what was looked for.
//
// The read path is foldRunDir's, so the message is the one `run unpause` gives:
// "holds no event log, so it is not a run directory", with where runs live. Pinned
// here because a projection is the command most likely to grow its own softer
// answer -- printing an empty tree for a run that does not exist is a real
// temptation, and it is indistinguishable from a run that exists and spent nothing.
func TestRunTreeRefusesARunThatIsNotThere(t *testing.T) {
	dir := workdir(t)
	writeTreeRun(t, dir, treeRun{id: "present-000000001", budget: 2, spent: 0.1})

	got := arxi(t, dir, "run", "tree", "absent-0000000001")
	if got.code == 0 {
		t.Errorf("a run that does not exist produced a tree and exit 0.\n%s\n"+
			"  consequence: a mistyped id renders as a tree of one that cost "+
			"nothing, which is exactly what a real quiescent run looks like.",
			got.out)
	}
	if !strings.Contains(got.out, "no event log") {
		t.Errorf("the failure does not say the directory holds no log.\n%s\n"+
			"  consequence: the reader cannot tell a typo from a run whose "+
			"directory was moved.", got.out)
	}
}

// With no argument, the usage says the argument is the ROOT.
//
// THE DEFECT this pins is a usage line reading "arxi run tree <run>" and stopping.
// `run list` prints children and roots identically -- nothing marks one -- so the
// id a user has to hand is as likely to be a child as a root, and passing a child
// returns a smaller number with no error. Saying so in the usage is cheaper than
// the note that has to catch it afterwards.
func TestRunTreeAsksWhichRunAndSaysAChildIsNotARoot(t *testing.T) {
	dir := workdir(t)
	got := arxi(t, dir, "run", "tree")
	if got.code != 2 {
		t.Errorf("exit %d, want 2 for a missing argument\n%s", got.code, got.out)
	}
	if !strings.Contains(got.out, "the argument is the ROOT") {
		t.Errorf("the usage does not distinguish a root from a child.\n%s\n"+
			"  consequence: the user passes the id they have, which `run list` "+
			"gave them without saying whether it is a root, and reads a branch's "+
			"cost as the tree's.", got.out)
	}

	// An empty id is a second arrival at the same question, and it goes through a
	// different branch: parseInvocation is satisfied by an argument that is there
	// and empty, so the check inside the command is what catches it. Asserted here
	// because the two branches printing different advice is precisely what
	// happened -- the ROOT sentence was in the one the common case cannot reach.
	blank := arxi(t, dir, "run", "tree", "")
	if blank.code != 2 {
		t.Errorf("exit %d, want 2 for an empty run id\n%s", blank.code, blank.out)
	}
	if !strings.Contains(blank.out, "the argument is the ROOT") {
		t.Errorf("an empty run id gets different advice from a missing one.\n%s\n"+
			"  consequence: whichever branch a user lands in decides whether they "+
			"are told the argument is the root, and that is not something they can "+
			"choose.", blank.out)
	}
}

// A sibling with a nephew is the shape that needs the vertical bar.
//
// Every other test here builds a chain, and a chain only ever needs "└─ " and the
// three spaces under it. Two children of one root are what produce "├─ " for the
// first, and a child UNDER that first sibling is the only thing that needs "│  ":
// the line has to be carried down past the grandchild so the reader can see that
// the last row belongs to the root and not to its sibling.
//
// THE DEFECT this pins is indentation with spaces where the bar belongs. It looks
// almost right, and it makes a nephew and an uncle indistinguishable -- which in a
// view about who spawned whom is the only distinction it has.
func TestRunTreeCarriesTheVerticalBarPastANephew(t *testing.T) {
	dir := workdir(t)
	writeTreeRun(t, dir, treeRun{id: "mb-root-000000001", budget: 20, spent: 1})
	writeTreeRun(t, dir, treeRun{id: "mb-a-00000000001",
		parent: "mb-root-000000001", depth: 1, spent: 2})
	writeTreeRun(t, dir, treeRun{id: "mb-a-kid-00000001",
		parent: "mb-a-00000000001", depth: 2, spent: 4})
	writeTreeRun(t, dir, treeRun{id: "mb-b-00000000001",
		parent: "mb-root-000000001", depth: 1, spent: 3})

	got := arxi(t, dir, "run", "tree", "mb-root-000000001")
	if got.code != 0 {
		t.Fatalf("exit %d, want 0\n%s", got.code, got.out)
	}

	for _, want := range []struct{ id, prefix string }{
		{"mb-a-00000000001", "├─ "},
		{"mb-a-kid-00000001", "│  └─ "},
		{"mb-b-00000000001", "└─ "},
	} {
		if ln := treeLine(t, got.out, want.id); !strings.HasPrefix(ln, want.prefix) {
			t.Errorf("%s is drawn %q, want the prefix %q\n%s",
				want.id, ln, want.prefix, got.out)
		}
	}
	if !strings.Contains(got.out, "tree total: 10 of 20 USD across 4 runs") {
		t.Errorf("the four-run total is wrong.\n%s", got.out)
	}
}
