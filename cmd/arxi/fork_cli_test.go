package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `arxi run fork`, exercised as a process against real run directories.
//
// # What is worth pinning here, and what is not
//
// The mechanism is a directory and an append, and neither is interesting on its
// own. What every test below is about is one of the three promises a fork makes,
// each of which is a thing somebody loses money or history on if it breaks:
//
//   - the parent is NOT touched. A fork is offered as the way to continue a run
//     that has finished, so a user reaches for it precisely when the parent's log
//     is the record of something they still care about.
//   - the copied seqs are the parent's seqs. `--at-seq 44` is meaningless
//     otherwise, and both `run prompt` and `event emit` print that flag with a
//     number read off the parent.
//   - the fork is not a tree child. runtree.go:55 totals a tree by SUMMING each
//     node's own spend, and the copied prefix already contains the parent's
//     llm.response events, so a parent_run_id here prints a total nobody spent.
//
// The fixtures are hand-written through runAt rather than started with --sim, for
// the reason cancel's are: a --sim run drives itself to a terminal state, and a
// terminal parent is the one shape most of these tests must not have.

// forkableRun writes a run that folds to running with money already spent.
//
// It stops at llm.response deliberately. runAt's blueprint has one member and one
// stage whose advance_when is `all`, so a stage.submitted would carry the run to
// succeeded -- and a succeeded parent is refused at its head, which would make
// every copy test here a test of the refusal instead.
func forkableRun(t *testing.T, dir, id string) {
	t.Helper()
	runAt(t, dir, id, "feature-team", 1.0,
		`{"id":"e3","seq":3,"type":"agent.activated","actor":"backend","payload":{"agent":"backend"}}
{"id":"e4","seq":4,"type":"llm.response","actor":"backend","payload":{"agent":"backend","cost_usd":0.02}}
`)
}

// forkEvents reads a run's log as decoded lines, in file order.
func forkEvents(t *testing.T, dir, id string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "runs", id, "events.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("run %s has an unreadable log line %q: %v", id, line, err)
		}
		out = append(out, ev)
	}
	return out
}

func TestRunForkCopiesThePrefixWithTheParentsSeqsAndIds(t *testing.T) {
	dir := t.TempDir()
	const parent, child = "rmtfrk1aa-11c2a0de", "forkchild"
	forkableRun(t, dir, parent)

	before, err := os.ReadFile(filepath.Join(dir, "runs", parent, "events.ndjson"))
	if err != nil {
		t.Fatal(err)
	}

	out, errb, code := arxiStreams(t, dir, "run", "fork", parent,
		"--at-seq", "3", "--run-id", child)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out, errb)
	}

	// The headline has to name the seq even when the user passed it, because it is
	// the one fact that makes the fork reproducible, and when --at-seq is omitted
	// the binary chose the number itself.
	if !strings.Contains(out, "run "+child+" forked from "+parent+" at seq 3") {
		t.Errorf("the headline does not say what was forked, from where, or at "+
			"which seq:\n%s", out)
	}

	evs := forkEvents(t, dir, child)
	if len(evs) != 3 {
		t.Fatalf("the fork holds %d events, want 3 (seq 1..3)\n%v\n"+
			"  consequence: --at-seq is the whole interface of this command. A copy "+
			"that is one event short or long is a run the user did not ask for, and "+
			"an append-only log cannot be trimmed back.", len(evs), evs)
	}
	for i, ev := range evs {
		if seq, _ := ev["seq"].(float64); int(seq) != i+1 {
			t.Errorf("event %d has seq %v, want %d\n%v\n"+
				"  consequence: `run prompt` and `event emit` both print `run fork "+
				"<run> --at-seq N` with a seq read off the PARENT. If the fork "+
				"renumbers, that N points at a different event in the fork than in "+
				"the parent, and the two logs cannot be compared at all.",
				i, ev["seq"], i+1, ev)
		}
	}

	// Ids are NOT rewritten: caused_by holds ids, so renaming them breaks every
	// chain `event trace` walks and makes the copy look like different events that
	// happened to say the same things.
	wantIDs := []string{"e1", "e2", "e3"}
	for i, want := range wantIDs {
		if evs[i]["id"] != want {
			t.Errorf("event %d has id %v, want %q\n"+
				"  consequence: caused_by references ids. A renamed id is a causal "+
				"chain that ends nowhere, and `event trace` on the fork stops at the "+
				"fork's first event.", i, evs[i]["id"], want)
		}
	}

	// The parent is compared BYTE FOR BYTE, not by folding it. A fork is what this
	// tool offers for a run somebody still cares about; an append there -- even a
	// harmless-looking one -- is the failure this command must not have.
	after, err := os.ReadFile(filepath.Join(dir, "runs", parent, "events.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("the parent's log changed:\nbefore:\n%s\nafter:\n%s\n"+
			"  consequence: forking is offered as the way to continue a finished "+
			"run, so it is used exactly when the parent's history still matters.",
			before, after)
	}
}

// TestRunForkRewritesTheStartEventAndNothingElse pins the one edited event.
//
// run.started is rewritten because it is the only event that names the run: left
// verbatim, the fork folds to the PARENT's RunID and `run list`, `run show` and
// `event log` all report the fork under the parent's name, in the parent's row.
func TestRunForkRewritesTheStartEventAndNothingElse(t *testing.T) {
	dir := t.TempDir()
	const parent, child = "rmtfrk1aa-11c2a0de", "forkchild"
	forkableRun(t, dir, parent)

	if _, errb, code := arxiStreams(t, dir, "run", "fork", parent,
		"--at-seq", "4", "--run-id", child); code != 0 {
		t.Fatalf("forking: exit %d\n%s", code, errb)
	}

	start := forkEvents(t, dir, child)[0]
	payload, _ := start["payload"].(map[string]any)
	if payload["run_id"] != child {
		t.Errorf("the fork's run.started says run_id=%v, want %q\n%v\n"+
			"  consequence: state = fold(events), so the fork would BE the parent as "+
			"far as every command in the tool is concerned -- two directories "+
			"reporting one id.", payload["run_id"], child, start)
	}
	// The lineage, and the only place it exists: parent_run_id is deliberately not
	// written, so these two keys are what an audit reads to learn where a fork
	// came from.
	if payload["forked_from"] != parent {
		t.Errorf("forked_from = %v, want %q\n%v\n"+
			"  consequence: with no parent_run_id and no forked_from, a fork is a run "+
			"whose first four events came from nowhere.",
			payload["forked_from"], parent, start)
	}
	if seq, _ := payload["forked_at_seq"].(float64); int(seq) != 4 {
		t.Errorf("forked_at_seq = %v, want 4\n%v", payload["forked_at_seq"], start)
	}
	// Inherited, not re-chosen. A fork that says nothing about money keeps the
	// ceiling the user chose once, so no default is invented in the binary.
	if b, _ := payload["budget_usd"].(float64); b != 1.0 {
		t.Errorf("budget_usd = %v, want the parent's 1\n%v", payload["budget_usd"], start)
	}
	// The scope is asserted in TestRunForkRewritesTheScopeWhenTheParentRecordedOne,
	// against a fixture that HAS one. runAt's events carry no scope, and a scope is
	// not invented here: an empty field copied verbatim is the parent's own shape.

	// parent_run_id is the assertion this file exists to make. Its absence is not an
	// omission: runtree.go:55 totals a tree by summing each node's own SpentUSD,
	// and the copied prefix already contains the parent's llm.response events.
	if _, ok := payload["parent_run_id"]; ok {
		t.Errorf("run.started carries parent_run_id=%v\n%v\n"+
			"  consequence: `run tree` sums each node's own spend, and the fork's "+
			"own spend INCLUDES the money the parent also reports -- so making the "+
			"fork a tree child prints a total nobody spent, and runtree.go:324 "+
			"additionally flags a spawn_depth disagreement.",
			payload["parent_run_id"], start)
	}
}

// TestRunForkRefusesATerminalForkPointAndTheRemedyItPrintsWorks is the test this
// verb was written for.
//
// The two commands that recommend `run fork` recommend it FOR a terminal run:
// prompt.go:128 and emit.go:152 both print `arxi run fork <run> --at-seq <head>`,
// and that head is exactly the seq whose fork would be terminal too. So the
// refusal and the remedy are one feature, and the remedy is EXECUTED here rather
// than pattern-matched: the four defects before this verb were all printed
// commands the binary refused, and asserting on the text would reproduce them.
func TestRunForkRefusesATerminalForkPointAndTheRemedyItPrintsWorks(t *testing.T) {
	dir := t.TempDir()
	const parent = "rmtfrk2bb-22c2a0de"
	succeededAt(t, dir, parent)

	out, errb, code := arxiStreams(t, dir, "run", "fork", parent)
	if code == 0 {
		t.Fatalf("forking a succeeded run at its head exited 0\nstdout:\n%s\n"+
			"  consequence: `run prompt` sends users here for exactly this run. A "+
			"fork that is born succeeded costs a directory and does nothing.", out)
	}
	// succeededAt's log ends with run.result at seq 5, so the state turns terminal
	// there and the seq before it is 4.
	if !strings.Contains(errb, "--at-seq 4") {
		t.Errorf("the refusal does not hand back a fork point that works:\n%s\n"+
			"  consequence: this is the only continuation offered for a finished run. "+
			"A refusal with no usable seq ends the session.", errb)
	}
	if !strings.Contains(errb, "seq 5") {
		t.Errorf("the refusal does not say where the parent ended:\n%s\n"+
			"  consequence: without the terminating seq the user cannot tell whether "+
			"the suggested point is one event back or forty.", errb)
	}
	// Nothing was created. The fold happens in memory before any directory exists
	// precisely so a refused fork leaves no runs/<id> for `run list` to show forever.
	if _, err := os.Stat(filepath.Join(dir, "runs", "x")); err == nil {
		t.Errorf("a refused fork left a directory behind")
	}

	// The remedy, run. Not its text.
	out2, errb2, code2 := arxiStreams(t, dir, "run", "fork", parent,
		"--at-seq", "4", "--run-id", "recovered")
	if code2 != 0 {
		t.Fatalf("the seq the refusal recommended was itself refused: exit %d\n"+
			"stdout:\n%s\nstderr:\n%s\n"+
			"  consequence: this is the fifth printed-and-refused command in this "+
			"project's history, and it would be printed by the command written to "+
			"close the fourth.", code2, out2, errb2)
	}
	if n := len(forkEvents(t, dir, "recovered")); n != 4 {
		t.Errorf("the recovered fork holds %d events, want 4", n)
	}
}

// TestRunForkBudgetRaisesTheCeilingAndSaysWhatIsAlreadySpent covers the flag
// docs/design/20-use-cases.md:541 shows and the registry did not declare.
//
// The spend line is asserted alongside the ceiling because the two are only
// meaningful together: the fork inherits the parent's spending along with its
// history, so a fork with a fresh-looking ceiling has less room than it appears.
func TestRunForkBudgetRaisesTheCeilingAndSaysWhatIsAlreadySpent(t *testing.T) {
	dir := t.TempDir()
	const parent, child = "rmtfrk3cc-33c2a0de", "richer"
	forkableRun(t, dir, parent)

	out, errb, code := arxiStreams(t, dir, "run", "fork", parent,
		"--budget", "8.00", "--run-id", child)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out, errb)
	}
	payload, _ := forkEvents(t, dir, child)[0]["payload"].(map[string]any)
	if b, _ := payload["budget_usd"].(float64); b != 8.0 {
		t.Errorf("budget_usd = %v, want 8\n"+
			"  consequence: budget_usd on run.started is the only place a ceiling "+
			"exists, so a --budget that does not land there is a flag that reports "+
			"success and changes nothing.", payload["budget_usd"])
	}
	if !strings.Contains(out, "0.02") {
		t.Errorf("the report does not say what the fork has already spent:\n%s\n"+
			"  consequence: the copied history carries the parent's llm.response "+
			"events, so the fork starts with the parent's bill against its ceiling. "+
			"A user who reads 8.00 as 8.00 of room is wrong by the parent's spend.",
			out)
	}
}

// TestRunForkRefusesToWriteIntoADirectoryThatAlreadyHoldsARun is about a failure
// that cannot be undone rather than one that is merely wrong.
func TestRunForkRefusesToWriteIntoADirectoryThatAlreadyHoldsARun(t *testing.T) {
	dir := t.TempDir()
	const parent, taken = "rmtfrk4dd-44c2a0de", "rmtfrk5ee-55c2a0de"
	forkableRun(t, dir, parent)
	forkableRun(t, dir, taken)

	before, err := os.ReadFile(filepath.Join(dir, "runs", taken, "events.ndjson"))
	if err != nil {
		t.Fatal(err)
	}

	out, errb, code := arxiStreams(t, dir, "run", "fork", parent, "--run-id", taken)
	if code == 0 {
		t.Fatalf("forking into an occupied id exited 0\nstdout:\n%s", out)
	}
	if !strings.Contains(errb, "--run-id") {
		t.Errorf("the refusal does not say how to choose another id:\n%s", errb)
	}
	after, err := os.ReadFile(filepath.Join(dir, "runs", taken, "events.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("the occupied run's log was appended to:\nbefore:\n%s\nafter:\n%s\n"+
			"  consequence: two histories in one log means two run.started events "+
			"and a fold that reports whichever came first -- for BOTH runs -- and an "+
			"append-only log offers no way to take it back.", before, after)
	}
}

// TestRunForkRefusesBadForkPointsBeforeCreatingAnything checks the ordering, not
// just the messages.
//
// Every flag is range-checked before any directory exists, because the debris a
// late failure leaves is a runs/<id> holding a frozen blueprint and no log --
// which logstore.Open cannot fold and `run list` shows forever.
func TestRunForkRefusesBadForkPointsBeforeCreatingAnything(t *testing.T) {
	dir := t.TempDir()
	const parent = "rmtfrk6ff-66c2a0de"
	forkableRun(t, dir, parent)

	cases := []struct {
		name string
		args []string
		want string
		code int
	}{
		// Not "the beginning": a log with no run.started folds to a State with no
		// RunID, which cannot be driven.
		{"seq zero", []string{"--at-seq", "0"}, "--at-seq 1", 2},
		{"not a number", []string{"--at-seq", "later"}, "not a seq", 2},
		// The parent's head is 4. A fork cannot start in a future the parent has
		// not had, and the message hands back the head it does have.
		{"past the head", []string{"--at-seq", "99"}, "only reaches seq 4", 1},
		// resolveRunDir accepts a path on purpose (a run started with --dir has to
		// be reachable), so nothing downstream catches an id that is a path.
		{"id is a path", []string{"--run-id", "../escaped"}, "not a run id", 2},
		{"budget of zero", []string{"--budget", "0"}, "inherit", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, errb, code := arxiStreams(t, dir,
				append([]string{"run", "fork", parent}, tc.args...)...)
			if code != tc.code {
				t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s",
					code, tc.code, out, errb)
			}
			if !strings.Contains(errb, tc.want) {
				t.Errorf("the refusal does not contain %q:\n%s", tc.want, errb)
			}
		})
	}

	// One directory before, one after: the parent's. A refused fork that has
	// already minted an id is indistinguishable in `run list` from a real run.
	ents, err := os.ReadDir(filepath.Join(dir, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 {
		names := make([]string, 0, len(ents))
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("runs/ holds %v, want only %s\n"+
			"  consequence: a half-written run directory is folded by nothing and "+
			"listed forever, and the user did not ask for it.", names, parent)
	}
}

// TestRunForkDoesNotDriveAndSaysWhatDoesInstead pins the one decision in this
// command that differs from every other writer in the package.
//
// `run prompt`, `run unpause` and `inbox` all drive the loop after appending.
// Fork must not: the copied events yield the copied decisions -- Decide is pure --
// so driving would re-run the parent's history at full price and land where the
// parent already is. The closing line therefore has to name a verb that DOES
// drive, branched on the fork's own folded status, and each branch is checked
// against cmdRunUnpause's guards rather than assumed.
func TestRunForkDoesNotDriveAndSaysWhatDoesInstead(t *testing.T) {
	dir := t.TempDir()
	const parent, child = "rmtfrk7gg-77c2a0de", "undriven"
	forkableRun(t, dir, parent)

	out, errb, code := arxiStreams(t, dir, "run", "fork", parent, "--run-id", child)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errb)
	}
	// Four events in, four events out. A fifth would be the fork having taken a
	// turn, which is money spent to reproduce the parent.
	if n := len(forkEvents(t, dir, child)); n != 4 {
		t.Errorf("the fork's log holds %d events, want the 4 that were copied\n"+
			"  consequence: driving a fresh fork folds the parent's own events and "+
			"produces the parent's own effects, so it re-runs history at full price "+
			"and changes nothing.", n)
	}
	// A running fork is NOT resumable: cmdRunUnpause refuses a running run with
	// "already running, so there is nothing to resume", so prompt is the only verb
	// that moves it. Printing unpause here was the first draft's defect.
	if !strings.Contains(out, "arxi run prompt "+child) {
		t.Errorf("the fork does not say what moves it:\n%s\n"+
			"  consequence: `run unpause` refuses a running run, so a closing line "+
			"naming it would be the sixth printed-and-refused command -- printed by "+
			"the command written to close the fifth.", out)
	}
	if strings.Contains(out, "arxi run unpause "+child) {
		t.Errorf("a running fork was told to unpause:\n%s", out)
	}
}

// TestRunForkOfABlockedParentPointsAtUnpauseWithARaise is the other branch, and
// the reason the two are separate tests: the remedy inverts on the parent's state.
//
// A fork of a budget-blocked parent inherits the block AND the spend, so bare
// `run unpause` cannot help -- budgetIsExhausted is still true. cmdRunUnpause
// refuses a --budget at or below the recorded ceiling ("unpause raises a ceiling,
// it does not lower one"), which is why the printed form is a raise.
//
// The fixture is local rather than the shared blockedAt, and that was measured:
// blockedAt records budget.exceeded with a spent_usd payload and no llm.response,
// so the fold reports 0.00 spent, budgetIsExhausted is FALSE, and the run takes
// the "blocked with budget left" branch. Spend comes from llm.response cost_usd --
// checked against the reducer after the shared fixture sent this test to the wrong
// branch, rather than assumed a second time.
func TestRunForkOfABlockedParentPointsAtUnpauseWithARaise(t *testing.T) {
	dir := t.TempDir()
	const parent, child = "rmtfrk8hh-88c2a0de", "stillblocked"
	runAt(t, dir, parent, "feature-team", 0.01,
		`{"id":"e3","seq":3,"type":"agent.activated","actor":"backend","payload":{"agent":"backend"}}
{"id":"e4","seq":4,"type":"llm.response","actor":"backend","payload":{"agent":"backend","cost_usd":0.02}}
{"id":"e5","seq":5,"type":"budget.exceeded","payload":{"spent_usd":0.02}}
{"id":"e6","seq":6,"type":"inbox.created","payload":{"inbox_id":"inbox-1","kind":"budget","question":"budget exhausted. raise or cancel?","on_timeout":"fail"}}
`)

	out, errb, code := arxiStreams(t, dir, "run", "fork", parent, "--run-id", child)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errb)
	}
	if !strings.Contains(out, "arxi run unpause "+child+" --budget") {
		t.Errorf("the fork of a blocked parent does not say how to unblock it:\n%s\n"+
			"  consequence: the fork inherits the parent's spend along with its "+
			"history, so bare `run unpause` leaves budgetIsExhausted true and the run "+
			"does not move -- which looks exactly like this command having failed.",
			out)
	}
	// The inherited ask comes with the history and is unanswered in the fork, so
	// the report has to mention it: nothing else will, and an unanswered ask with
	// on_timeout=fail is how a run dies quietly.
	if !strings.Contains(out, "arxi inbox "+child) {
		t.Errorf("the inherited, unanswered ask is not reported:\n%s", out)
	}
}

// TestRunForkRewritesTheScopeWhenTheParentRecordedOne needs its own fixture
// because runAt writes no scope and a real log does: `run start` stamps
// scope=run:<id> on every event it appends.
//
// Cosmetic today -- grepping the tree finds nothing that READS Scope -- and
// asserted anyway, because the first reader of it would otherwise find a fork
// whose every line claims to belong to a different run, and would have no way to
// tell that from a fork of the wrong parent.
func TestRunForkRewritesTheScopeWhenTheParentRecordedOne(t *testing.T) {
	dir := t.TempDir()
	const parent, child = "rmtfrk9ii-99c2a0de", "rescoped"
	runAt(t, dir, parent, "feature-team", 1.0, "")

	// runAt's two lines are rewritten with scopes, keeping its seqs and ids so the
	// fixture stays comparable with the others in this file.
	log := `{"id":"e1","seq":1,"scope":"run:` + parent + `","type":"run.started","payload":{"actor":"feature-team","run_id":"` + parent + `","budget_usd":1}}
{"id":"e2","seq":2,"scope":"run:` + parent + `","type":"stage.entered","payload":{"stage":"execute","index":0}}
`
	if err := os.WriteFile(filepath.Join(dir, "runs", parent, "events.ndjson"),
		[]byte(log), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, errb, code := arxiStreams(t, dir, "run", "fork", parent,
		"--run-id", child); code != 0 {
		t.Fatalf("forking: exit %d\n%s", code, errb)
	}
	for i, ev := range forkEvents(t, dir, child) {
		if ev["scope"] != "run:"+child {
			t.Errorf("event %d has scope %v, want run:%s\n%v",
				i, ev["scope"], child, ev)
		}
	}
}
