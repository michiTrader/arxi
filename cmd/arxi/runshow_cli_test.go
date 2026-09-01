package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// `arxi run show`, exercised as a process.
//
// Every test here is a defect that existed. Both of the real ones were found by
// pointing the built binary at a run produced by `run start --sim` and reading
// the output, not by inspecting the code -- which compiled, read correctly, and
// was wrong about the two things a person consults this view to learn: whether
// work is about to happen, and how much has been spent.
//
// runAt (from runlist_cli_test.go) is reused rather than reimplemented. A
// second fixture builder would drift from the first, and the first thing to
// drift would be the roster, which is exactly what these tests read.

// showBlocked writes a run stopped by its budget, with work parked on a member.
//
// The shape is copied from a REAL run: `arxi run start feature-team.yaml
// --budget 0.001 --sim` produces a run that spent 0.02 against 0.001, blocked,
// with pending causes on the members who were mid-stage. Reproducing the shape
// by hand keeps the test independent of the executor while still describing a
// state the system genuinely reaches.
//
// llm.response is what carries cost, NOT budget.exceeded. Checked against the
// reducer rather than assumed: an earlier version of this fixture used
// budget.exceeded alone and produced a run that was blocked while having spent
// nothing, which would have made the overspend test below unable to fail.
func showBlocked(t *testing.T, dir, id string) {
	t.Helper()
	runAt(t, dir, id, "team", 0.001,
		`{"id":"e3","seq":3,"type":"agent.activated","actor":"backend","payload":{"agent":"backend"}}
{"id":"e4","seq":4,"type":"llm.response","actor":"backend","payload":{"cost_usd":0.02}}
{"id":"e5","seq":5,"type":"budget.exceeded","payload":{"spent_usd":0.02}}
{"id":"e6","seq":6,"type":"inbox.created","payload":{"inbox_id":"inbox-1","kind":"budget","question":"budget exhausted. raise or cancel?","on_timeout":"fail"}}
{"id":"e7","seq":7,"type":"stage.entered","payload":{"stage":"execute","index":0}}
`)
}

// A blocked run must not describe withheld work as work about to happen.
//
// THE DEFECT: this printed "(a turn is queued: ev-000005)" for two members of a
// run that had broken its budget and stopped. Member.Runnable() was reporting
// truthfully about the member -- a cause is waiting -- but spawnCauses parks
// causes instead of opening turns while spending is halted, so nothing was
// coming. The user most likely to run this command is the one whose run has
// stopped, and this told them to wait for a turn that could not arrive.
func TestRunShowDoesNotPromiseTurnsOnAHaltedRun(t *testing.T) {
	dir := workdir(t)
	showBlocked(t, dir, "held-back-00000001")

	got := arxi(t, dir, "run", "show", "held-back-00000001")
	if got.code != 0 {
		t.Fatalf("exit %d, want 0\n%s", got.code, got.out)
	}

	if strings.Contains(got.out, "a turn is queued") {
		t.Errorf("a blocked run says a turn is queued.\n%s\n"+
			"  the run has broken its budget and stopped; spawnCauses PARKS the "+
			"cause instead of opening a turn while spending is halted. Reporting "+
			"it as queued tells the one person who came here because the run is "+
			"stuck that work is on its way.", got.out)
	}
	if !strings.Contains(got.out, "work held back") {
		t.Errorf("a blocked run with parked causes does not say the work is held back.\n%s\n"+
			"  consequence: the member reads as plain \"idle\", which is the same "+
			"word this view uses for a member that has finished. The state that "+
			"needs a decision looks identical to the state that needs nothing.",
			got.out)
	}
}

// A PAUSED run holds work back too, and this pins the duplicated rule.
//
// printRunShow cannot call kernel's own spendingHalted -- it is unexported, and
// exporting a reducer predicate so a printer may borrow it would widen the
// frozen surface for a display concern. So the rule is restated in this package
// and the duplication is the risk: the copy covers blocked AND paused today,
// and the way it rots is by covering only the one somebody happened to test.
//
// This test is the second status. It replaced an earlier one of mine that tried
// to assert the opposite case -- a RUNNING run with parked causes -- which does
// not fire because that state is unreachable: applyUnpaused and the inbox reply
// path both call drainParked, so the reducer never leaves a live run with
// causes withheld. A guard aimed at an impossible state cannot fail, which is
// the same trap as the 100% coverage probe, one layer down. Diagnosed against
// the reducer rather than patched.
func TestRunShowSaysWorkIsHeldBackOnAPausedRunToo(t *testing.T) {
	dir := workdir(t)
	runAt(t, dir, "paused-park-000001", "team", 5.0,
		`{"id":"e3","seq":3,"type":"run.paused","payload":{}}
{"id":"e4","seq":4,"type":"stage.entered","payload":{"stage":"execute","index":0}}
`)

	got := arxi(t, dir, "run", "show", "paused-park-000001")
	if !strings.Contains(got.out, "paused") {
		t.Fatalf("the fixture is not paused, so this proves nothing.\n%s", got.out)
	}
	if strings.Contains(got.out, "a turn is queued") {
		t.Errorf("a paused run says a turn is queued.\n%s\n"+
			"  pause halts spending exactly as blocked does -- a `run pause` that "+
			"kept paying for turns would not be a pause. The local copy of that "+
			"rule has drifted to cover only the blocked case.", got.out)
	}
}

// An overspend must be stated, not left to be inferred by comparing two numbers.
//
// THE DEFECT: a run twenty times over its ceiling printed "0.02 of 0.001 USD in
// the tree". Nothing in that line says the budget was breached; reading it
// correctly requires noticing the left number is larger, and the two differ in
// magnitude and digit count exactly when it matters most. The inbox of this
// same run words it properly, so the detail view was the vaguer of two
// descriptions of one fact.
func TestRunShowSaysWhenTheBudgetWasBreached(t *testing.T) {
	dir := workdir(t)
	showBlocked(t, dir, "over-budget-000001")

	got := arxi(t, dir, "run", "show", "over-budget-000001")
	if !strings.Contains(got.out, "OVER by") {
		t.Errorf("a run 20x over its budget does not say it is over.\n%s\n"+
			"  the line reads like ordinary progress. This is the number a user "+
			"is billed for, and the same run's inbox says \"budget exhausted\" "+
			"in plain words -- the detail view should not be the vaguer of the two.",
			got.out)
	}
}

// A budget below a cent must not be rounded into a claim that none exists.
//
// The sibling of the defect `run list` shipped with. Two decimals is right for
// dollars and wrong for a deliberately tiny ceiling: "0.00" is how these
// commands say "unlimited".
func TestRunShowDoesNotRoundASmallBudgetAway(t *testing.T) {
	dir := workdir(t)
	showBlocked(t, dir, "small-budget-00001")

	got := arxi(t, dir, "run", "show", "small-budget-00001")
	if strings.Contains(got.out, "of 0 USD") || strings.Contains(got.out, "of 0.00 USD") {
		t.Errorf("a budget of 0.001 renders as zero.\n%s\n"+
			"  consequence: zero is how this view says \"no ceiling\", so a real "+
			"limit is displayed as its own absence.", got.out)
	}
	if !strings.Contains(got.out, "0.001") {
		t.Errorf("the budget 0.001 does not appear at all.\n%s", got.out)
	}
}

// Answered questions are shown as answered, and counted separately.
//
// THE DEFECT this pins is `run list`'s, found while building this command: an
// answered question stayed in st.Inbox with Replied set, and len(st.Inbox)
// counted it as outstanding. `arxi inbox` said "no pending questions" for the
// same run in the same binary. Two commands contradicting each other about one
// fact is worse than either being wrong alone.
func TestRunShowSeparatesAnsweredQuestionsFromPendingOnes(t *testing.T) {
	dir := workdir(t)
	runAt(t, dir, "answered-00000001", "team", 1.0,
		`{"id":"e3","seq":3,"type":"inbox.created","payload":{"inbox_id":"inbox-1","kind":"approval","question":"may i write?","on_timeout":"fail"}}
{"id":"e4","seq":4,"type":"inbox.replied","payload":{"inbox_id":"inbox-1","text":"go ahead"}}
`)

	got := arxi(t, dir, "run", "show", "answered-00000001")
	if !strings.Contains(got.out, "0 pending") {
		t.Errorf("an answered question is still counted as pending.\n%s\n"+
			"  the reducer marks Replied and LEAVES the item in the inbox, "+
			"because the log is append-only. Counting the slice counts history "+
			"as outstanding work, and `arxi inbox` says \"no pending questions\" "+
			"about this very run.", got.out)
	}
	if !strings.Contains(got.out, "answered") {
		t.Errorf("an answered question is not marked as answered.\n%s\n"+
			"  it is shown rather than hidden on purpose -- this is the detail "+
			"view and the decision is history worth seeing -- so it has to be "+
			"distinguishable from one still waiting.", got.out)
	}
}

// The same fix, in `run list`'s column.
//
// Guarded here rather than in runlist_cli_test.go because this is where the
// contradiction was found, and keeping the two assertions together is what
// makes it obvious they are one bug.
func TestRunListDoesNotCountAnsweredQuestionsAsPending(t *testing.T) {
	dir := workdir(t)
	runAt(t, dir, "answered-00000002", "team", 1.0,
		`{"id":"e3","seq":3,"type":"inbox.created","payload":{"inbox_id":"inbox-1","kind":"approval","question":"may i write?","on_timeout":"fail"}}
{"id":"e4","seq":4,"type":"inbox.replied","payload":{"inbox_id":"inbox-1","text":"go ahead"}}
`)

	got := arxi(t, dir, "run", "list", "--json")
	var payload struct {
		Runs []struct {
			Asks int `json:"pending_asks"`
		} `json:"runs"`
	}
	if err := json.Unmarshal([]byte(got.out), &payload); err != nil {
		t.Fatalf("run list --json is not JSON: %v\n%s", err, got.out)
	}
	if len(payload.Runs) != 1 {
		t.Fatalf("expected 1 run, got %d\n%s", len(payload.Runs), got.out)
	}
	if payload.Runs[0].Asks != 0 {
		t.Errorf("pending_asks = %d for a run whose only question was answered, want 0.\n"+
			"  the field is NAMED pending_asks, and `arxi inbox` reports no "+
			"pending questions for this run. The listing contradicted the inbox.",
			payload.Runs[0].Asks)
	}
}

// A run id that does not exist must be refused, not described as empty.
func TestRunShowRefusesARunThatIsNotThere(t *testing.T) {
	dir := workdir(t)
	got := arxi(t, dir, "run", "show", "no-such-run-0001")

	if got.code == 0 {
		t.Errorf("exit 0 for a run that does not exist.\n%s\n"+
			"  consequence: a typo in an id prints a blank-looking run and the "+
			"user concludes the run lost its state, rather than that they "+
			"mistyped it.", got.out)
	}
	if !strings.Contains(got.out, "no event log") {
		t.Errorf("the refusal does not say what was missing.\n%s", got.out)
	}
}

// The human view must name the members, which the table could not.
//
// This is the justification for the verb existing. If `run show` printed only
// what `run list` prints, it would be a worse `run list --status` and the only
// thing it would add is a higher number in the surface count.
func TestRunShowNamesWhatTheTableHadToElide(t *testing.T) {
	dir := workdir(t)
	showBlocked(t, dir, "detail-0000000001")

	got := arxi(t, dir, "run", "show", "detail-0000000001")

	for _, want := range []string{
		"backend",          // the roster, absent from the table
		"budget exhausted", // the question's TEXT, not a count
		"members",          // the section exists at all
	} {
		if !strings.Contains(got.out, want) {
			t.Errorf("run show omits %q, which is the kind of thing it exists to add.\n%s",
				want, got.out)
		}
	}
}

// --json must carry the structures, not the rendered strings.
//
// A caller that has to parse "backend (thinking) 2 turns" out of a line is a
// caller that breaks the first time a column is widened.
func TestRunShowJSONCarriesTheStructures(t *testing.T) {
	dir := workdir(t)
	showBlocked(t, dir, "json-000000000001")

	got := arxi(t, dir, "run", "show", "json-000000000001", "--json")

	var payload struct {
		Run       string  `json:"run"`
		Status    string  `json:"status"`
		Terminal  bool    `json:"terminal"`
		Budget    float64 `json:"budget_usd"`
		TreeSpent float64 `json:"tree_spent_usd"`
		Asks      int     `json:"pending_asks"`
		Members   []struct {
			Name     string   `json:"name"`
			Runnable bool     `json:"runnable"`
			Causes   []string `json:"pending_causes"`
		} `json:"members"`
		Questions []struct {
			ID       string `json:"id"`
			Question string `json:"question"`
			Replied  bool   `json:"replied"`
		} `json:"asks"`
	}
	if err := json.Unmarshal([]byte(got.out), &payload); err != nil {
		t.Fatalf("run show --json is not JSON: %v\n%s", err, got.out)
	}

	if payload.Run != "json-000000000001" || payload.Status != "blocked" {
		t.Errorf("run/status = %q/%q, want json-000000000001/blocked",
			payload.Run, payload.Status)
	}
	if payload.Terminal {
		t.Errorf("terminal = true for a blocked run.\n" +
			"  blocked is recoverable -- that is the entire reason `run unpause` " +
			"exists -- and a caller that treats it as final would give up on a " +
			"run that only needs an answer.")
	}
	// The budget must survive as a NUMBER, not be rounded by the renderer.
	if payload.Budget != 0.001 {
		t.Errorf("budget_usd = %v, want 0.001 -- the JSON was rounded like the table",
			payload.Budget)
	}
	if payload.TreeSpent <= payload.Budget {
		t.Errorf("tree_spent_usd %v is not over budget_usd %v; the fixture no "+
			"longer describes an overspend and the test above proves nothing",
			payload.TreeSpent, payload.Budget)
	}
	if len(payload.Members) == 0 {
		t.Errorf("no members in the JSON.\n%s\n"+
			"  the roster is the main thing the table could not show; a machine "+
			"reading that omits it has no reason to prefer this over run list.",
			got.out)
	}
	if len(payload.Questions) != 1 || payload.Questions[0].Question == "" {
		t.Errorf("the question text is missing from the JSON: %+v", payload.Questions)
	}
	if payload.Asks != 1 {
		t.Errorf("pending_asks = %d, want 1", payload.Asks)
	}
}

// A terminal run must not be told to go looking for a problem.
//
// The closing line is chosen by what is true of the run. Printing "why is it
// not finished?" under a run that succeeded sends the user to diagnose a
// non-existent fault, and `run why` would then have to talk them back out of it.
//
// run.result, not run.succeeded: the latter is not an event type at all, and
// using it produced a run that folded to "running" -- a mistake made once
// already in runlist_cli_test.go and diagnosed against the reducer.
func TestRunShowDoesNotAskWhyASucceededRunIsUnfinished(t *testing.T) {
	dir := workdir(t)
	runAt(t, dir, "succeeded-0000001", "team", 1.0,
		`{"id":"e3","seq":3,"type":"run.result","payload":{"summary":"all stages completed"}}
`)

	got := arxi(t, dir, "run", "show", "succeeded-0000001")
	if strings.Contains(got.out, "why is it not finished") {
		t.Errorf("a succeeded run is asked why it is not finished.\n%s", got.out)
	}
	if !strings.Contains(got.out, "all stages completed") {
		t.Errorf("the result of a finished run is not shown.\n%s\n"+
			"  the result IS the answer for a terminal run; omitting it means "+
			"the detail view has nothing to say about the runs that worked.",
			got.out)
	}
}
