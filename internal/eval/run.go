package eval

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// The fold over cases.
//
// `arxi eval run ./suites/review-quality.yaml --budget 12.00` is twenty runs,
// not one, and almost everything difficult about it follows from that: twenty
// runs share one ceiling, any of them can fail without the others being wrong,
// and the suite can run out of money before it runs out of cases.
//
// The fold itself is ten lines. What earns the rest of this file is the
// arithmetic of stopping early, because a suite that stops early still prints a
// pass rate, and that pass rate is the most misleading number this package can
// produce. See budgetStop.

// CaseRunner executes one case and reports what it cost.
//
// This is the seam. `internal/eval` does not import `internal/exec` and does not
// know what a turn is, for the same reason `internal/kernel` does not know what
// a file is: the fold has to be testable without an executor, a clock or a
// provider account. A fake CaseRunner that returns fixed costs exercises every
// branch of the budget logic in microseconds, and the budget logic is the part
// that can silently lose money.
//
// The budget passed in is what is LEFT, not the suite total. A case cannot be
// allowed to spend the whole suite ceiling just because it is first in the file.
type CaseRunner interface {
	RunCase(ctx context.Context, c Case, budgetUSD float64) (CaseOutcome, error)
}

// CaseOutcome is one case's execution, before it has been judged.
//
// Judging is deliberately not the runner's job: the runner reports what the
// agent produced and what it cost, and Expectation.Judge decides whether that
// is a pass. Keeping them apart is what makes it possible to re-judge a stored
// run against a corrected expectation without paying for it twice — and a suite
// whose expectations were wrong is a suite you very much want to re-judge.
type CaseOutcome struct {
	RunID string

	// Result is the text the expectation is matched against. Empty is a real
	// answer, not a missing one: an agent that produced nothing fails a
	// `contains` and passes a `not_contains`, which is correct in both cases.
	Result string

	CostUSD    float64
	Turns      int
	DurationMs int64
}

// Runner folds a suite into a RunSummary.
type Runner struct {
	Suite *Suite
	Cases CaseRunner

	// BudgetUSD is the ceiling for the WHOLE suite, and it is required by the
	// declaration (§20.11: "eval run requires --budget for the same reason run
	// start does -- more so: a 20-case suite is 20 runs"). Twenty chances to
	// spend more than intended, and the mistake is only visible on the invoice.
	BudgetUSD float64

	// ID names this run so `eval compare e1 e2` has something to name.
	ID string

	// Reserve is the fraction of the budget that must remain for a case to be
	// started at all. See reserveFor.
	Reserve float64

	StartedAt string
}

// DefaultReserve is the share of the suite budget a case must have available
// before it is started.
//
// A case is started with whatever is left, and a case that is started with 0.02
// USD remaining will not fail cleanly -- it will run until the budget stops it
// and produce a truncated, unjudgeable answer that nonetheless gets judged. It
// then counts as a FAIL, which is a lie: the prompt did not get that case wrong,
// the suite ran out of money. A fail is indistinguishable from a genuine one on
// every report, whereas a skip is visible everywhere.
//
// So a case is only started if a plausible amount remains. One twentieth is the
// suite's own arithmetic -- §20.11's 20 cases against 12.00 USD -- which makes
// the floor scale with the ceiling the user actually chose rather than with a
// constant in dollars that would be absurd at both ends.
const DefaultReserve = 0.05

// ErrNoCases means the suite loaded but had nothing to run.
//
// Distinct from a validation failure: the loader refuses an empty `cases:` list,
// so this is reachable only through a Suite built in code, and it must not be
// reported as success over zero cases.
var ErrNoCases = fmt.Errorf("the suite has no cases to run")

// Run executes every case, in file order, until the suite finishes or the money
// does.
//
// File order rather than sorted or shuffled: a suite is read top to bottom, and
// a partial run should have covered the cases the author put first. Shuffling
// would make a budget-exhausted run a random sample -- statistically better and
// operationally awful, because two runs of the same suite would then cover
// different cases and `compare` would have nothing stable to pair.
//
// The error return is for failures of the FOLD, not of a case. A case that
// errored is data (StatusError) and the suite carries on; there is no such thing
// as one case's failure invalidating the others, and stopping would throw away
// the cases already paid for.
func (r *Runner) Run(ctx context.Context) (*RunSummary, error) {
	if r.Suite == nil {
		return nil, fmt.Errorf("no suite to run")
	}
	if len(r.Suite.Cases) == 0 {
		return nil, ErrNoCases
	}
	if r.Cases == nil {
		return nil, fmt.Errorf("no case runner: eval run cannot execute anything")
	}
	if r.BudgetUSD <= 0 {
		// Refused rather than defaulted. A default budget is a number nobody
		// chose being spent twenty times.
		return nil, fmt.Errorf("--budget must be greater than zero: a %d-case "+
			"suite is %d runs, and an unbounded ceiling is not a ceiling",
			len(r.Suite.Cases), len(r.Suite.Cases))
	}

	sum := &RunSummary{
		ID:        r.ID,
		Suite:     r.Suite.Name,
		SuiteSHA:  r.Suite.SHA,
		BudgetUSD: r.BudgetUSD,
		StartedAt: r.StartedAt,
	}

	reserve := reserveFor(r.BudgetUSD, r.Reserve)
	var spent float64

	for _, c := range r.Suite.Cases {
		remaining := r.BudgetUSD - spent

		// Cancellation and exhaustion are both "this case did not run", and both
		// are recorded as skipped with the reason attached rather than dropped.
		// A case missing from the results is indistinguishable from a case that
		// was never in the suite, which is exactly the confusion Compare's
		// case-set warning exists to catch -- and it should not have to catch a
		// confusion this package created itself.
		if err := ctx.Err(); err != nil {
			sum.Results = append(sum.Results, Result{
				Case:   c.Name,
				Status: StatusSkipped,
				Why:    "the run was cancelled before this case started",
			})
			continue
		}
		if remaining < reserve {
			sum.Results = append(sum.Results, Result{
				Case:   c.Name,
				Status: StatusSkipped,
				Why: fmt.Sprintf("only %.4f USD of the %.2f budget was left, "+
					"below the %.4f a case needs to produce a judgeable answer",
					remaining, r.BudgetUSD, reserve),
			})
			continue
		}

		out, err := r.Cases.RunCase(ctx, c, remaining)

		// The cost is banked BEFORE the error is examined, and the order is the
		// whole point: a case that failed halfway still spent what it spent.
		// Treating an error as costless is how a suite overruns its ceiling --
		// the failures are free, so the fold keeps going, and the invoice
		// disagrees with the report.
		spent += out.CostUSD

		res := Result{
			Case:     c.Name,
			RunID:    out.RunID,
			CostUSD:  out.CostUSD,
			Turns:    out.Turns,
			Duration: out.DurationMs,
		}
		if err != nil {
			res.Status = StatusError
			res.Why = err.Error()
			sum.Results = append(sum.Results, res)
			continue
		}

		res.Result = out.Result
		ok, why := c.Expect.Judge(out.Result)
		if ok {
			res.Status = StatusPass
		} else {
			res.Status = StatusFail
			res.Why = why
		}
		sum.Results = append(sum.Results, res)
	}

	return sum, nil
}

// reserveFor is the minimum a case must have available to be started.
func reserveFor(budget, frac float64) float64 {
	if frac <= 0 {
		frac = DefaultReserve
	}
	return budget * frac
}

// budgetStop reports whether this run was cut short, and by what.
//
// This exists because of the one number a truncated eval run gets wrong in a way
// nothing else catches. Totals.PassRate divides by judged cases, which is right
// for an errored case -- a harness crash is not a worse prompt, and the cases
// that DID produce answers are still a fair sample of the suite.
//
// Budget exhaustion is not like that. The cases that ran are the first N in file
// order, and the ones that did not are the tail. That is not a sample of the
// suite, it is a PREFIX of it, and a suite's cases are not interchangeable: an
// author who puts the easy cases first and the adversarial ones last -- which is
// the natural way to write a file -- gets a pass rate that is systematically too
// high, and it rises further the earlier the money runs out. A prompt change
// that makes each case more expensive can then improve the reported pass rate by
// running out of money sooner.
//
// So this is reported separately from ordinary incompleteness, in the sentence
// that says which way the bias runs. Saying "17 of 20 cases judged" is true and
// would let a reader assume the missing three were random.
func (s *RunSummary) budgetStop() (skipped []string, exhausted bool) {
	for _, r := range s.Results {
		if r.Status != StatusSkipped {
			continue
		}
		skipped = append(skipped, r.Case)
		if strings.Contains(r.Why, "budget was left") {
			exhausted = true
		}
	}
	return skipped, exhausted
}

// Notes are the things a reader of THIS run needs told, independent of any
// comparison.
//
// Compare has its own warnings for reading two runs against each other; these
// are about one run being misread on its own. They are separate because a
// single-run report is where a pass rate is first quoted, and the caveats must
// not wait for somebody to run `compare`.
func (s *RunSummary) Notes() []string {
	var out []string
	t := s.Totals()

	skipped, exhausted := s.budgetStop()
	if exhausted {
		out = append(out, fmt.Sprintf(
			"the budget ran out after %d of %d cases, so %d case(s) never ran "+
				"(%s) — the cases that DID run are the first ones in the file, "+
				"not a sample of the suite, so this pass rate is over a prefix "+
				"and carries whatever bias the file's ordering has",
			t.Judged+t.Errored, t.Cases, len(skipped),
			strings.Join(truncate(skipped, 5), ", ")))
		out = append(out, fmt.Sprintf(
			"spent %.4f of the %.2f USD budget; a run that stops on budget is "+
				"a measurement of cost, not of quality — raise --budget before "+
				"reading the pass rate as a result",
			t.CostUSD, s.BudgetUSD))
	} else if len(skipped) > 0 {
		out = append(out, fmt.Sprintf(
			"%d case(s) did not run (%s); every rate below is over the %d that did",
			len(skipped), strings.Join(truncate(skipped, 5), ", "), t.Judged))
	}

	if t.Errored > 0 {
		out = append(out, fmt.Sprintf(
			"%d case(s) errored and produced no judgeable answer; they are in "+
				"the cost total (money was spent) and out of the pass rate (a "+
				"harness failure is not a worse prompt)", t.Errored))
	}

	// One sample per case, said on the single-run report too. A reader who never
	// runs `compare` still quotes this number, and "0.80" invites a precision
	// the measurement does not have.
	if _, ok := t.PassRate(); ok && t.Judged > 0 {
		band := passRateBand(t, t)
		out = append(out, fmt.Sprintf(
			"one sample per case over %d judged cases: a difference smaller "+
				"than about %.2f between two runs of this suite is not "+
				"distinguishable from noise",
			t.Judged, band))
	}
	return out
}

// truncate keeps a list of names readable.
//
// A warning that prints all 400 skipped case names is a warning that gets
// scrolled past, taking the sentence at the top with it.
func truncate(names []string, max int) []string {
	sort.Strings(names)
	if len(names) <= max {
		return names
	}
	out := append([]string{}, names[:max]...)
	return append(out, fmt.Sprintf("and %d more", len(names)-max))
}
