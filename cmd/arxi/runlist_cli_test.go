package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `arxi run list`, exercised as a process against real run directories.
//
// The two defects this file guards were both invisible to inspection and
// obvious the first time the command met a real run: an id column too narrow
// for the ids the system actually mints, and a sub-cent budget rounded into a
// claim that no budget existed. Neither is a logic error a package test would
// have caught, because neither is about what the code computes -- both are
// about what it tells a person, and one of them told a person the opposite of
// the truth about money.

// runAt writes a run directory whose log folds to the requested shape.
//
// Written by hand rather than by calling `arxi run start`, for the same reason
// toolrun's fixtures avoid the code under test: a listing built with the thing
// being tested cannot tell "the run was never there" from "the listing failed
// to read it". Hand-written logs also make statuses reachable that a simulated
// run would not produce on demand.
func runAt(t *testing.T, dir, id, actor string, budget float64, extra string) {
	t.Helper()

	run := filepath.Join(dir, "runs", id)
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run, "blueprint.snapshot.yaml"), []byte(
		"name: "+actor+"\n"+
			"members:\n"+
			"  - name: backend\n"+
			"    tools: [read, write, bash]\n"+
			"stages:\n"+
			"  - name: execute\n"+
			"    advance_when: all\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	log := `{"id":"e1","seq":1,"type":"run.started","payload":{"actor":"` + actor +
		`","run_id":"` + id + `","budget_usd":` + trimFloat(budget) + `}}
{"id":"e2","seq":2,"type":"stage.entered","payload":{"stage":"execute","index":0}}
` + extra

	if err := os.WriteFile(filepath.Join(run, "events.ndjson"), []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
}

func trimFloat(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// blockedAt is a run the reducer really marks blocked.
//
// budget.exceeded, and not tool.call_denied, which was the first guess and is
// wrong: a denied tool call mints an inbox item but leaves the run RUNNING, so
// a fixture built on it silently tested the wrong status and the filter test
// failed for a reason that had nothing to do with the filter. Checked against
// decide.go rather than assumed a second time -- BudgetExceeded sets
// StatusBlocked at :128, which is also what the probed run did.
func blockedAt(t *testing.T, dir, id, actor string, budget float64) {
	t.Helper()
	runAt(t, dir, id, actor, budget,
		`{"id":"e3","seq":3,"type":"agent.activated","actor":"backend","payload":{"agent":"backend"}}
{"id":"e4","seq":4,"type":"budget.exceeded","payload":{"spent_usd":0.02}}
{"id":"e5","seq":5,"type":"inbox.created","payload":{"inbox_id":"inbox-1","kind":"budget","question":"budget exhausted. raise or cancel?","on_timeout":"fail"}}
`)
}

func TestRunListPrintsAnIdThatCanBeUsed(t *testing.T) {
	dir := t.TempDir()

	// The real generator mints 18-character ids, and the first version of this
	// command used a 14-wide column -- so every row was truncated and no
	// printed id worked. The id here is that length on purpose: a fixture with
	// a short id would have passed against the broken code.
	const id = "rmthws2dz-93381f43"
	blockedAt(t, dir, id, "feature-team", 1.0)

	got := arxi(t, dir, "run", "list")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.out)
	}

	if !strings.Contains(got.out, id) {
		t.Errorf("the listing does not contain the full run id %q:\n%s\n"+
			"  consequence: this listing exists to hand the user an id to act "+
			"on. A truncated one pasted into `arxi run unpause` answers \"holds "+
			"no event log\", which blames the user for a mangling this command did.",
			id, got.out)
	}

	// The ellipsis is what truncation looks like, and asserting on the id alone
	// would still pass if the id were printed AND a truncated copy appeared
	// somewhere. Checking the id's own column is the specific claim.
	for _, line := range strings.Split(got.out, "\n") {
		if strings.Contains(line, "rmthws2dz") && strings.Contains(line, "…") {
			cols := strings.Fields(line)
			if len(cols) > 0 && strings.Contains(cols[0], "…") {
				t.Errorf("the id column is truncated: %q\n"+
					"  every other column may be elided; this one may not, "+
					"because it is the only one the user has to retype.", cols[0])
			}
		}
	}
}

func TestRunListDoesNotRoundASmallBudgetIntoNoBudget(t *testing.T) {
	dir := t.TempDir()

	// 0.001 is not a contrived number: it is what a person types to cap a
	// smoke test, and the run that produced this defect really did block on
	// "budget exhausted (0.0200 of 0.0010 USD)" while the listing said 0.00.
	blockedAt(t, dir, "tiny-budget-run00", "feature-team", 0.001)

	got := arxi(t, dir, "run", "list")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.out)
	}

	// The check is for the exact two-decimal rendering, anchored so it cannot
	// match the correct "/0.0010". A first version tested Contains("/0.00"),
	// which is a prefix of the right answer and failed against output that was
	// already correct -- a test that cannot pass is as useless as one that
	// cannot fail, and this one would have sent the next reader to fix
	// formatSpend a second time.
	if strings.Contains(got.out, "/0.00 ") || strings.HasSuffix(strings.TrimRight(got.out, "\n"), "/0.00") {
		t.Errorf("a budget of 0.001 is rendered as /0.00:\n%s\n"+
			"  consequence: /0.00 is how this command says \"no budget set\", so "+
			"a run with a real ceiling -- one the inbox describes as exhausted "+
			"-- reads as unlimited. That is not a rounding nit: it is the "+
			"opposite of the truth about money.", got.out)
	}
	if !strings.Contains(got.out, "0.0010") {
		t.Errorf("the listing never shows the budget 0.0010:\n%s\n"+
			"  a budget deliberately set below a cent has to survive being "+
			"printed, or setting it was pointless.", got.out)
	}
}

func TestRunListSaysWhereItLookedWhenThereAreNoRuns(t *testing.T) {
	dir := t.TempDir()

	got := arxi(t, dir, "run", "list")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.out)
	}
	for _, want := range []string{"no runs", "runs"} {
		if !strings.Contains(got.out, want) {
			t.Errorf("an empty listing does not mention %q:\n%s\n"+
				"  consequence: the user cannot tell \"nothing has run\" from "+
				"\"you are in the wrong directory\", and those have different "+
				"next steps.", want, got.out)
		}
	}
}

func TestRunListRefusesAStatusThatIsNotAStatus(t *testing.T) {
	dir := t.TempDir()
	blockedAt(t, dir, "some-run-000000000", "feature-team", 1.0)

	// "done" is the plausible wrong guess: the real value is "succeeded".
	got := arxi(t, dir, "run", "list", "--status", "done")
	if got.code == 0 {
		t.Fatalf("an unknown --status was accepted:\n%s\n"+
			"  consequence: it matches nothing, so the command prints \"no runs\" "+
			"to somebody whose runs all succeeded. That reads as an answer "+
			"rather than a mistake, and the answer is the opposite of the truth.",
			got.out)
	}
	if !strings.Contains(got.out, "succeeded") {
		t.Errorf("the refusal does not list the statuses that ARE accepted:\n%s\n"+
			"  a rejection that does not say what is allowed just moves the "+
			"guessing one step later.", got.out)
	}
}

func TestRunListFiltersByStatus(t *testing.T) {
	dir := t.TempDir()
	blockedAt(t, dir, "blocked-run-00001", "feature-team", 1.0)
	runAt(t, dir, "queued-run-000001", "other-team", 1.0, "")

	got := arxi(t, dir, "run", "list", "--status", "blocked")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.out)
	}
	if !strings.Contains(got.out, "blocked-run-00001") {
		t.Errorf("--status blocked omitted the blocked run:\n%s", got.out)
	}
	if strings.Contains(got.out, "queued-run-000001") {
		t.Errorf("--status blocked included a run that is not blocked:\n%s\n"+
			"  a filter that does not filter is worse than none: it invites "+
			"the user to trust a narrowing that never happened.", got.out)
	}
}

func TestRunListPutsBlockedRunsFirst(t *testing.T) {
	dir := t.TempDir()

	// Alphabetically "aaa" precedes "zzz", so a listing that merely sorted by
	// id would put the blocked run last. The blocked run is the only one that
	// cannot move without a person, and burying it is the one ordering mistake
	// with a cost.
	// run.result, not run.succeeded. The latter is not an event type the
	// reducer knows, so it folded to "running" and this fixture was silently
	// comparing two live runs -- the test still failed when the ranking was
	// removed, but for the wrong reason, and it would have kept passing if
	// terminal runs had been mis-ranked. Found by reading the failure output of
	// a deliberately broken sort instead of only its pass/fail.
	runAt(t, dir, "aaa-succeeded-0001", "team", 1.0,
		`{"id":"e3","seq":3,"type":"run.result","payload":{"summary":"ok"}}
`)
	blockedAt(t, dir, "zzz-blocked-00001", "team", 1.0)

	got := arxi(t, dir, "run", "list")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.out)
	}
	iBlocked := strings.Index(got.out, "zzz-blocked-00001")
	iOther := strings.Index(got.out, "aaa-succeeded-0001")
	if iBlocked < 0 || iOther < 0 {
		t.Fatalf("both runs should be listed:\n%s", got.out)
	}
	if iBlocked > iOther {
		t.Errorf("a blocked run is listed below a finished one:\n%s\n"+
			"  consequence: the only row that is asking for help is the one "+
			"scrolled past. Ordering by id alone does this whenever the blocked "+
			"run sorts late.", got.out)
	}
}

func TestRunListDoesNotLetOneBadDirectoryHideTheRest(t *testing.T) {
	dir := t.TempDir()
	blockedAt(t, dir, "healthy-run-00001", "feature-team", 1.0)

	// A directory that looks like a run and is not readable as one.
	bad := filepath.Join(dir, "runs", "corrupt-run-00001")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "events.ndjson"),
		[]byte("this is not ndjson\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := arxi(t, dir, "run", "list")
	if !strings.Contains(got.out, "healthy-run-00001") {
		t.Errorf("one unreadable run hid a healthy one:\n%s\n"+
			"  consequence: a single corrupt directory makes the output "+
			"indistinguishable from having no runs at all, and the user goes "+
			"looking for why nothing started.", got.out)
	}
	if !strings.Contains(got.out, "corrupt-run-00001") {
		t.Errorf("the unreadable run is not reported at all:\n%s\n"+
			"  skipping it silently is the other half of the same bug: the "+
			"listing would be quietly incomplete and look total.", got.out)
	}
}

func TestRunListJSONCarriesWhatTheTableElides(t *testing.T) {
	dir := t.TempDir()
	blockedAt(t, dir, "json-run-00000001", "feature-team", 0.001)

	got := arxi(t, dir, "run", "list", "--json")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.out)
	}

	var payload struct {
		Runs []struct {
			Run    string  `json:"run"`
			Dir    string  `json:"dir"`
			Actor  string  `json:"actor"`
			Status string  `json:"status"`
			Budget float64 `json:"budget_usd"`
			Asks   int     `json:"pending_asks"`
		} `json:"runs"`
	}
	if err := json.Unmarshal([]byte(got.out), &payload); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, got.out)
	}
	if len(payload.Runs) != 1 {
		t.Fatalf("expected 1 run, got %d:\n%s", len(payload.Runs), got.out)
	}

	r := payload.Runs[0]
	if r.Run != "json-run-00000001" {
		t.Errorf("run id = %q, want json-run-00000001", r.Run)
	}
	if r.Status != "blocked" {
		t.Errorf("status = %q, want blocked", r.Status)
	}
	// The budget is asserted as a NUMBER, which the table cannot do. This is the
	// reason --json exists: the text column is a rendering and this is the value.
	if r.Budget != 0.001 {
		t.Errorf("budget_usd = %v, want 0.001\n"+
			"  the JSON output must not inherit the table's rounding: a machine "+
			"reading this has no way to notice a budget that was formatted away.",
			r.Budget)
	}
	if r.Asks != 1 {
		t.Errorf("pending_asks = %d, want 1 -- the run is blocked on an approval", r.Asks)
	}
	if r.Dir == "" {
		t.Error("dir is empty: a caller acting on this run needs to know where it is")
	}
}
