package eval

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// The fold over cases.
//
// These tests are almost entirely about money and about the sentence a truncated
// run prints. The fold is ten lines and needs three tests; the budget logic can
// silently overspend or silently bias a pass rate, and needs the rest.

// fakeRunner answers each case from a script keyed by case name.
//
// A map rather than a slice so a test cannot accidentally depend on call order
// while claiming to test something else, and so a case absent from the script is
// an obvious mistake rather than a zero-cost pass.
type fakeRunner struct {
	answer map[string]string  // case name -> result text
	cost   map[string]float64 // case name -> what it spends
	fail   map[string]string  // case name -> error message
	turns  int

	// calls records what was asked for, including the budget each case was
	// offered. The budget argument is the part most likely to be wrong in a way
	// no other assertion sees.
	calls   []string
	offered []float64
}

func newFake() *fakeRunner {
	return &fakeRunner{
		answer: map[string]string{},
		cost:   map[string]float64{},
		fail:   map[string]string{},
		turns:  1,
	}
}

func (f *fakeRunner) RunCase(ctx context.Context, c Case, budgetUSD float64) (CaseOutcome, error) {
	f.calls = append(f.calls, c.Name)
	f.offered = append(f.offered, budgetUSD)
	out := CaseOutcome{
		RunID:   "r-" + c.Name,
		Result:  f.answer[c.Name],
		CostUSD: f.cost[c.Name],
		Turns:   f.turns,
	}
	if msg, ok := f.fail[c.Name]; ok {
		// A failing case still reports its cost: that is the behaviour the
		// budget arithmetic depends on.
		return out, fmt.Errorf("%s", msg)
	}
	return out, nil
}

// suiteOf builds a Suite in code, so these tests do not go through YAML.
//
// Every case expects to contain "ok", which makes the answer text the only
// thing deciding pass or fail and keeps the assertions about the fold.
func suiteOf(names ...string) *Suite {
	s := &Suite{Name: "review-quality", SHA: "sha1"}
	for _, n := range names {
		s.Cases = append(s.Cases, Case{
			Name:      n,
			Blueprint: "bp",
			Objective: "do the thing",
			Expect:    Expectation{Contains: []string{"ok"}},
		})
	}
	return s
}

func mustRun(t *testing.T, r *Runner) *RunSummary {
	t.Helper()
	sum, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return sum
}

// note reports whether any note contains all the fragments.
func note(s *RunSummary, fragments ...string) (string, bool) {
	for _, n := range s.Notes() {
		all := true
		for _, f := range fragments {
			if !strings.Contains(n, f) {
				all = false
				break
			}
		}
		if all {
			return n, true
		}
	}
	return "", false
}

func TestEveryCaseIsRunAndJudged(t *testing.T) {
	f := newFake()
	f.answer["a"] = "this is ok"
	f.answer["b"] = "this is wrong"
	f.answer["c"] = "ok too"
	for _, n := range []string{"a", "b", "c"} {
		f.cost[n] = 1
	}

	sum := mustRun(t, &Runner{Suite: suiteOf("a", "b", "c"), Cases: f, BudgetUSD: 10, ID: "e1"})

	if len(sum.Results) != 3 {
		t.Fatalf("got %d results, want 3", len(sum.Results))
	}
	tot := sum.Totals()
	if tot.Passed != 2 || tot.Failed != 1 {
		t.Errorf("passed=%d failed=%d, want 2 and 1", tot.Passed, tot.Failed)
	}
	if rate, _ := tot.PassRate(); rate < 0.66 || rate > 0.67 {
		t.Errorf("pass rate = %.4f, want 2/3", rate)
	}
	// The run identifies its suite by digest, which is what Compare needs to
	// notice a suite that changed under the same name.
	if sum.Suite != "review-quality" || sum.SuiteSHA != "sha1" {
		t.Errorf("suite identity lost: %q %q", sum.Suite, sum.SuiteSHA)
	}
}

// TestCasesRunInFileOrder pins the order, which is load-bearing rather than
// incidental.
//
// A partial run must have covered the cases the author put FIRST. Shuffling
// would make a budget-exhausted run a random sample — better statistically and
// worse operationally, because two runs of the same suite would then cover
// different cases and `compare` would have nothing stable to pair.
func TestCasesRunInFileOrder(t *testing.T) {
	f := newFake()
	sum := mustRun(t, &Runner{Suite: suiteOf("zulu", "alpha", "mike"), Cases: f, BudgetUSD: 10})

	want := "zulu,alpha,mike"
	if got := strings.Join(f.calls, ","); got != want {
		t.Errorf("cases ran %s, want %s (file order)", got, want)
	}
	var names []string
	for _, r := range sum.Results {
		names = append(names, r.Case)
	}
	if got := strings.Join(names, ","); got != want {
		t.Errorf("results are in %s, want %s", got, want)
	}
}

// TestACaseIsOfferedWhatIsLeftNotTheWholeBudget is the arithmetic that stops the
// first case in the file spending the suite.
func TestACaseIsOfferedWhatIsLeftNotTheWholeBudget(t *testing.T) {
	f := newFake()
	f.cost["a"] = 4
	f.cost["b"] = 3
	mustRun(t, &Runner{Suite: suiteOf("a", "b", "c"), Cases: f, BudgetUSD: 12})

	want := []float64{12, 8, 5}
	if len(f.offered) != 3 {
		t.Fatalf("got %d calls, want 3", len(f.offered))
	}
	for i, w := range want {
		if f.offered[i] != w {
			t.Errorf("case %d was offered %.2f, want %.2f (the remaining "+
				"budget, not the suite total)", i, f.offered[i], w)
		}
	}
}

// TestAFailedCaseStillCostsWhatItSpent is the ordering bug that overruns a
// ceiling.
//
// If the cost of an errored case is not banked, the failures are free, the fold
// keeps going, and the invoice disagrees with the report. This asserts on the
// BUDGET OFFERED to the next case, because that is the number the mistake
// actually corrupts — asserting only on the total would pass an implementation
// that banked the cost after deciding whether to continue.
func TestAFailedCaseStillCostsWhatItSpent(t *testing.T) {
	f := newFake()
	f.cost["a"] = 5
	f.fail["a"] = "the provider hung up"
	f.cost["b"] = 1
	f.answer["b"] = "ok"

	sum := mustRun(t, &Runner{Suite: suiteOf("a", "b"), Cases: f, BudgetUSD: 10})

	if f.offered[1] != 5 {
		t.Errorf("the second case was offered %.2f, want 5.00: the errored "+
			"case spent 5.00 and a free failure is how a suite overruns its "+
			"ceiling", f.offered[1])
	}
	tot := sum.Totals()
	if tot.CostUSD != 6 {
		t.Errorf("CostUSD = %.2f, want 6.00 (5 errored + 1 judged)", tot.CostUSD)
	}
	if tot.Errored != 1 {
		t.Errorf("Errored = %d, want 1", tot.Errored)
	}
	// And the error text is kept: it is the thing somebody chases.
	if sum.Results[0].Why != "the provider hung up" {
		t.Errorf("Why = %q, want the provider's message", sum.Results[0].Why)
	}
}

// TestAnErroredCaseDoesNotStopTheSuite: one case failing does not make the
// others wrong, and stopping would throw away cases already paid for.
func TestAnErroredCaseDoesNotStopTheSuite(t *testing.T) {
	f := newFake()
	f.fail["a"] = "boom"
	f.answer["b"] = "ok"
	f.answer["c"] = "ok"

	sum := mustRun(t, &Runner{Suite: suiteOf("a", "b", "c"), Cases: f, BudgetUSD: 10})

	if len(f.calls) != 3 {
		t.Fatalf("only %d cases ran; an errored case stopped the suite", len(f.calls))
	}
	if tot := sum.Totals(); tot.Passed != 2 {
		t.Errorf("passed = %d, want 2: the cases after the error still count",
			tot.Passed)
	}
}

// TestACaseIsSkippedRatherThanStartedOnFumes is the reason the reserve exists.
//
// Started with almost nothing left, a case does not fail cleanly. It produces a
// truncated answer that still gets judged and counts as a FAIL — one that is
// indistinguishable from a real failure on every report, and which makes the
// prompt look worse than it is.
func TestACaseIsSkippedRatherThanStartedOnFumes(t *testing.T) {
	f := newFake()
	f.cost["a"] = 9.8 // leaves 0.20 of a 10.00 budget; the reserve is 0.50
	f.answer["a"] = "ok"
	f.answer["b"] = "ok"

	sum := mustRun(t, &Runner{Suite: suiteOf("a", "b"), Cases: f, BudgetUSD: 10})

	if len(f.calls) != 1 {
		t.Fatalf("the second case was started with 0.20 USD left; calls: %v", f.calls)
	}
	if sum.Results[1].Status != StatusSkipped {
		t.Errorf("case b is %q, want skipped: started on fumes it would be "+
			"judged on a truncated answer and counted as a genuine fail",
			sum.Results[1].Status)
	}
	if !strings.Contains(sum.Results[1].Why, "budget was left") {
		t.Errorf("the skip does not say the budget ran out: %q", sum.Results[1].Why)
	}
	// A skipped case is neither a pass nor a fail, so it is out of the rate.
	if tot := sum.Totals(); tot.Judged != 1 || tot.Skipped != 1 {
		t.Errorf("judged=%d skipped=%d, want 1 and 1", tot.Judged, tot.Skipped)
	}
}

// TestTheSkippedCaseIsRecordedNotDropped closes a confusion this package could
// otherwise create for itself.
//
// A case missing from the results is indistinguishable from a case that was
// never in the suite — which is exactly what Compare's case-set warning exists
// to catch. It should not have to catch something this fold invented.
func TestTheSkippedCaseIsRecordedNotDropped(t *testing.T) {
	f := newFake()
	f.cost["a"] = 9.9
	f.answer["a"] = "ok"

	sum := mustRun(t, &Runner{Suite: suiteOf("a", "b", "c"), Cases: f, BudgetUSD: 10})

	if len(sum.Results) != 3 {
		t.Fatalf("got %d results for a 3-case suite: the unrun cases were "+
			"dropped, which makes them look like cases that never existed",
			len(sum.Results))
	}
	if tot := sum.Totals(); tot.Cases != 3 {
		t.Errorf("Cases = %d, want 3", tot.Cases)
	}
}

// TestTheBudgetNoteSaysWhichWayTheBiasRuns is the most important test in this
// file.
//
// "17 of 20 cases judged" is true and lets a reader assume the missing three
// were random. They are not: they are the TAIL of the file. If the author put
// the hard cases last — the natural way to write a suite — the pass rate is
// systematically too high, and it rises the earlier the money runs out. A prompt
// change that makes each case more expensive can then improve the reported pass
// rate by exhausting the budget sooner.
func TestTheBudgetNoteSaysWhichWayTheBiasRuns(t *testing.T) {
	f := newFake()
	// 4.8 + 4.8 = 9.6 of a 10.00 budget, leaving 0.40 against a 0.50 reserve.
	f.cost["a"], f.cost["b"] = 4.8, 4.8
	f.answer["a"], f.answer["b"] = "ok", "ok"

	sum := mustRun(t, &Runner{Suite: suiteOf("a", "b", "c", "d"), Cases: f, BudgetUSD: 10})

	n, ok := note(sum, "budget ran out")
	if !ok {
		t.Fatalf("a budget-truncated run printed no note about it: %v", sum.Notes())
	}
	// The direction of the bias, not just its existence.
	for _, want := range []string{"first ones in the file", "not a sample"} {
		if !strings.Contains(n, want) {
			t.Errorf("the note does not say which way the bias runs (missing "+
				"%q): %q", want, n)
		}
	}
	// And it names the cases that never ran, so the reader can see what was
	// missed rather than being told a count.
	if !strings.Contains(n, "c") || !strings.Contains(n, "d") {
		t.Errorf("the note does not name the cases that did not run: %q", n)
	}
	// A 100% pass rate over a truncated prefix is exactly the number this note
	// is protecting, so assert it really is that misleading.
	if rate, _ := sum.Totals().PassRate(); rate != 1.0 {
		t.Fatalf("fixture is not the dangerous case: pass rate %.2f, want 1.00", rate)
	}
}

// TestABudgetStopIsDistinguishedFromCasesMerelyNotRunning: both are
// incompleteness, and only one is biased in a known direction. Reporting them
// with the same sentence would waste the distinction.
func TestABudgetStopIsDistinguishedFromCasesMerelyNotRunning(t *testing.T) {
	// Cancelled before anything ran: incomplete, but not budget-driven.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := newFake()
	sum, err := (&Runner{Suite: suiteOf("a", "b"), Cases: f, BudgetUSD: 10}).Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("a cancelled context still ran %d cases", len(f.calls))
	}
	if _, ok := note(sum, "budget ran out"); ok {
		t.Errorf("a cancelled run was reported as a budget stop: %v", sum.Notes())
	}
	if _, ok := note(sum, "did not run"); !ok {
		t.Errorf("a cancelled run said nothing about the cases that did not "+
			"run: %v", sum.Notes())
	}
	if sum.Results[0].Status != StatusSkipped {
		t.Errorf("case a is %q, want skipped", sum.Results[0].Status)
	}
	if !strings.Contains(sum.Results[0].Why, "cancelled") {
		t.Errorf("the skip does not say it was cancelled: %q", sum.Results[0].Why)
	}
}

// TestTheSpendIsReportedAgainstTheCeiling: a truncated run's cost is the number
// the next --budget is chosen against, so it belongs in the note that tells the
// reader to raise it.
func TestTheSpendIsReportedAgainstTheCeiling(t *testing.T) {
	f := newFake()
	f.cost["a"], f.cost["b"] = 4.8, 4.8
	f.answer["a"], f.answer["b"] = "ok", "ok"
	sum := mustRun(t, &Runner{Suite: suiteOf("a", "b", "c"), Cases: f, BudgetUSD: 10})

	n, ok := note(sum, "9.6000", "10.00")
	if !ok {
		t.Fatalf("no note reports the spend against the ceiling: %v", sum.Notes())
	}
	if !strings.Contains(n, "raise --budget") {
		t.Errorf("the note does not say what to do about it: %q", n)
	}
}

// TestErroredCasesAreExplainedInTheNotes states the total/mean disagreement
// where it is first read, rather than leaving it to `compare`.
func TestErroredCasesAreExplainedInTheNotes(t *testing.T) {
	f := newFake()
	f.fail["a"] = "boom"
	f.cost["a"] = 2
	f.answer["b"] = "ok"
	sum := mustRun(t, &Runner{Suite: suiteOf("a", "b"), Cases: f, BudgetUSD: 10})

	n, ok := note(sum, "errored")
	if !ok {
		t.Fatalf("an errored case produced no note: %v", sum.Notes())
	}
	if !strings.Contains(n, "cost total") || !strings.Contains(n, "pass rate") {
		t.Errorf("the note does not explain that the cost total and the pass "+
			"rate treat the error differently: %q", n)
	}
}

// TestTheSingleRunReportAdmitsItsSampleSize: a reader who never runs `compare`
// still quotes the pass rate, and "0.80" invites a precision one sample per case
// does not have.
func TestTheSingleRunReportAdmitsItsSampleSize(t *testing.T) {
	f := newFake()
	names := []string{"a", "b", "c", "d"}
	for _, n := range names {
		f.answer[n] = "ok"
	}
	sum := mustRun(t, &Runner{Suite: suiteOf(names...), Cases: f, BudgetUSD: 10})

	if _, ok := note(sum, "one sample per case", "4 judged cases"); !ok {
		t.Errorf("the report does not admit its sample size: %v", sum.Notes())
	}
}

// TestACleanCompleteRunIsNotCluttered is the other half of every note above.
//
// A report that carries caveats when nothing is wrong trains the reader to skip
// them, and takes the real ones with it.
func TestACleanCompleteRunIsNotCluttered(t *testing.T) {
	f := newFake()
	for _, n := range []string{"a", "b"} {
		f.answer[n] = "ok"
		f.cost[n] = 1
	}
	sum := mustRun(t, &Runner{Suite: suiteOf("a", "b"), Cases: f, BudgetUSD: 10})

	for _, n := range sum.Notes() {
		if strings.Contains(n, "budget ran out") ||
			strings.Contains(n, "did not run") ||
			strings.Contains(n, "errored") {
			t.Errorf("a clean run carries a caveat about nothing: %q", n)
		}
	}
	if !sum.Totals().Complete() {
		t.Error("a run with every case judged reports itself incomplete")
	}
}

// TestABudgetOfZeroIsRefusedRatherThanTreatedAsUnlimited.
//
// §20.11 requires --budget precisely because a 20-case suite is 20 runs. A
// missing ceiling defaulting to "no ceiling" is twenty chances to spend money
// nobody agreed to.
func TestABudgetOfZeroIsRefusedRatherThanTreatedAsUnlimited(t *testing.T) {
	f := newFake()
	for _, b := range []float64{0, -1} {
		_, err := (&Runner{Suite: suiteOf("a"), Cases: f, BudgetUSD: b}).Run(context.Background())
		if err == nil {
			t.Fatalf("a budget of %.2f was accepted", b)
		}
		if !strings.Contains(err.Error(), "--budget") {
			t.Errorf("the error does not name the flag to fix: %v", err)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("a refused budget still ran %d cases", len(f.calls))
	}
}

// TestAnEmptySuiteIsAnErrorNotASuccessOverZeroCases.
//
// "0 of 0 cases passed" is a green result that measured nothing, and it would be
// reported beside real ones.
func TestAnEmptySuiteIsAnErrorNotASuccessOverZeroCases(t *testing.T) {
	_, err := (&Runner{Suite: &Suite{Name: "s"}, Cases: newFake(), BudgetUSD: 5}).Run(context.Background())
	if err != ErrNoCases {
		t.Errorf("err = %v, want ErrNoCases", err)
	}
}

// TestAMissingCaseRunnerIsRefusedBeforeAnythingIsReported: without an executor
// the fold would report every case as unrun, which looks like a result.
func TestAMissingCaseRunnerIsRefusedBeforeAnythingIsReported(t *testing.T) {
	_, err := (&Runner{Suite: suiteOf("a"), BudgetUSD: 5}).Run(context.Background())
	if err == nil {
		t.Fatal("a Runner with no CaseRunner produced a report")
	}
	if !strings.Contains(err.Error(), "execute") {
		t.Errorf("the error does not say what is missing: %v", err)
	}
}

// TestTheJudgementUsesTheCaseExpectationAndKeepsItsReason.
//
// The runner reports what the agent produced; Expectation.Judge decides. Keeping
// them apart is what lets a stored run be re-judged against a corrected
// expectation without paying for it again.
func TestTheJudgementUsesTheCaseExpectationAndKeepsItsReason(t *testing.T) {
	s := suiteOf("a")
	s.Cases[0].Expect = Expectation{
		Contains:    []string{"approved"},
		NotContains: []string{"TODO"},
	}
	f := newFake()
	f.answer["a"] = "approved, but there is a TODO left"

	sum := mustRun(t, &Runner{Suite: s, Cases: f, BudgetUSD: 5})

	if sum.Results[0].Status != StatusFail {
		t.Fatalf("status = %q, want fail (the answer contains TODO)",
			sum.Results[0].Status)
	}
	if !strings.Contains(sum.Results[0].Why, "TODO") {
		t.Errorf("Why does not name the condition that failed: %q",
			sum.Results[0].Why)
	}
	// The result text is kept so the run can be re-judged later.
	if sum.Results[0].Result == "" {
		t.Error("the answer was discarded, so this run can never be re-judged")
	}
}

// TestTheReserveScalesWithTheBudget: a floor in dollars would be absurd at both
// ends — meaningless against a 500 USD suite and larger than the whole ceiling
// of a 0.50 USD one.
func TestTheReserveScalesWithTheBudget(t *testing.T) {
	for _, tc := range []struct {
		budget float64
		want   float64
	}{{10, 0.5}, {100, 5}, {1, 0.05}} {
		if got := reserveFor(tc.budget, 0); got != tc.want {
			t.Errorf("reserveFor(%.2f) = %.4f, want %.4f", tc.budget, got, tc.want)
		}
	}
	// An explicit reserve overrides the default.
	if got := reserveFor(10, 0.5); got != 5 {
		t.Errorf("an explicit reserve was ignored: %.2f", got)
	}
}
