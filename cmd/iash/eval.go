package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/michiTrader/iash/internal/blueprint"
	"github.com/michiTrader/iash/internal/eval"
	"github.com/michiTrader/iash/internal/evalstore"
	"github.com/michiTrader/iash/internal/surface"
)

// The eval CLI.
//
// `eval run` is twenty runs and `eval compare` is the table people quote in
// decisions, so the printing here carries as much weight as the arithmetic
// behind it. Two rules the rest of this file follows:
//
// Warnings print BEFORE the numbers. A caveat under a table is read after the
// conclusion has already been drawn, and the conclusion is the thing the caveat
// was supposed to prevent.
//
// A number that does not exist is not printed as zero. A pass rate over zero
// judged cases is not 0.00; it is absent, and "0.00" is a value a reader will
// compare against.

func cmdEval(args []string) {
	if len(args) == 0 {
		evalUsage()
		os.Exit(2)
	}
	switch args[0] {
	case "run":
		cmdEvalRun(args[1:])
	case "list":
		cmdEvalList(args[1:])
	case "compare":
		cmdEvalCompare(args[1:])
	default:
		// Same reasoning as cmdTrigger: a declared-but-unbuilt eval subcommand
		// must get main's precise answer, not "unknown", or the user goes
		// hunting for a typo they never made.
		if surface.Lookup("eval", args[0]) != nil {
			notImplemented(append([]string{"eval"}, args...))
		}
		fmt.Fprintf(os.Stderr, "iash eval: %q is not an eval command.\n", args[0])
		evalUsage()
		os.Exit(2)
	}
}

func evalUsage() {
	fmt.Fprint(os.Stderr, `usage: iash eval <command>

  run <suite.yaml> --budget <usd>   run a suite (a 20-case suite is 20 runs)
  list                              the runs that have been stored, newest first
  compare <baseline> <candidate>    compare two runs, with what cannot be
                                    attributed to the change

The suites live wherever you keep them; there is no registry.
`)
}

// cmdEvalRun loads a suite, runs it, and reports what the numbers are over.
func cmdEvalRun(args []string) {
	c := surface.Lookup("eval", "run")
	vals, err := parseInvocation(c, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "iash eval run: %v\n\n"+
			"usage: iash eval run <suite.yaml> --budget <usd> [--sim]\n"+
			"short: -b budget  -S sim\n", err)
		os.Exit(2)
	}

	budget, err := strconv.ParseFloat(vals["budget"], 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "iash eval run: --budget %q is not a number\n", vals["budget"])
		os.Exit(2)
	}

	suite, err := eval.LoadFile(vals["suite"])
	if err != nil {
		// A missing file and an invalid one are different mistakes and get
		// different sentences. "the suite is not valid" over `open nope.yaml:
		// no such file or directory` sends the reader to look for a syntax
		// error in a file they have not written yet, or — worse, and more
		// common — into a file at a path they did believe existed, when the
		// actual answer is that they are in the wrong directory.
		if errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "there is no suite at %s.\n\n"+
				"  the path is relative to where you are now: %s\n",
				vals["suite"], cwdOr())
			os.Exit(1)
		}
		// Exit 1: the invocation was right, the suite file is what is wrong.
		fmt.Fprintf(os.Stderr, "the suite is not valid.\n\n%v\n", err)
		os.Exit(1)
	}

	// --sim is still required, and the REASON has changed even though the gate
	// has not. It used to be that no LLM-backed Executor existed anywhere in
	// the build; one now does, and `run start` uses it. What eval still lacks is
	// its own wiring to it: simCases below is the only case runner, and it
	// answers from the objective rather than from a model.
	//
	// The gate stays because removing it would be the worst possible way to
	// close that gap. A "real" eval run today would spend nothing, produce
	// nothing, and report a pass rate anyway -- a wrong number wearing the
	// clothes of a measurement, which is the specific output this whole package
	// exists to prevent. It would be doubly indefensible now, because the user
	// has every reason to believe a live executor means a live eval.
	if vals["sim"] != "true" {
		fmt.Fprintf(os.Stderr,
			"iash eval run is not wired to the live executor yet: `run start` "+
				"calls real models, but every eval case is still answered by the "+
				"simulator, so a real run would spend nothing and produce "+
				"nothing while reporting a pass rate.\n\n"+
				"  what works today: iash eval run %s --budget %.2f --sim\n\n"+
				"--sim runs the same fold, the same budget arithmetic and the "+
				"same judging; only the executor is fake. The pass rate it "+
				"reports measures the simulator, and it says so.\n",
			vals["suite"], budget)
		os.Exit(2)
	}

	// The id comes from the clock, and there is no --run-id to override it.
	// There was a read of one here — of a flag the registry does not declare —
	// so it was always the empty string: a knob that looked like a feature and
	// could not be turned. §20.11 labels its runs e1 and e2, which needs a store
	// to count against; a timestamp is what can be generated without one.
	// Resolved against the store BEFORE the suite runs, because the failure
	// mode of doing it afterwards is the expensive one: the eval executes,
	// spends its budget, and then cannot be stored because a run in the same
	// second already owns the name. Two runs inside one second is what the
	// test that runs a suite twice does, and what any scripted loop does.
	store := openEvalStore()
	runID, idErr := store.FreeID("e" + time.Now().UTC().Format("20060102T150405"))
	if idErr != nil {
		fmt.Fprintf(os.Stderr, "iash eval run: %v\n", idErr)
		os.Exit(1)
	}

	r := &eval.Runner{
		Suite: suite,
		Cases: &simCases{
			dir: filepath.Join("evals", runID),
			// Blueprint paths in the suite are relative to the suite, not to
			// wherever the operator happens to be standing.
			base: filepath.Dir(vals["suite"]),
		},
		BudgetUSD: budget,
		ID:        runID,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}

	sum, err := r.Run(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "iash eval run: %v\n", err)
		os.Exit(1)
	}

	// Marked simulated BEFORE it is stored, and this is the only line in the
	// CLI that sets the field. Every eval run this build can produce is still
	// simulated, because the --sim gate above is impassable otherwise — so it
	// would be equally correct today to hardcode true here. It is set from the
	// flag instead, so that the day eval is wired to the live executor the flag
	// is already the thing that decides.
	//
	// That day arrived for `run start` and the constant there was NOT found:
	// run.started logged `"simulated": true` on real runs that had really called
	// a real server, and it took reading the log of one to notice. This line is
	// what that mistake looks like when it is avoided in advance.
	sum.Simulated = vals["sim"] == "true"

	// Stored before it is printed, and that order is deliberate. A run that
	// prints and then fails to store has spent the money and shown the numbers,
	// and the user has no reason to suspect the file is missing until they try
	// to compare against it — by which time the evidence is gone and only the
	// terminal scrollback remembers. Storing first means a failure to persist
	// is reported instead of discovered.
	//
	// The cost is that a full disk turns a completed run into a nonzero exit.
	// That is the right trade: the numbers are still on stderr's neighbour, and
	// an eval whose result was not recorded genuinely did not finish.
	if err := store.Put(sum); err != nil {
		fmt.Fprintf(os.Stderr, "the eval ran but could not be stored.\n\n%v\n\n"+
			"  the numbers below are real; there is just no file to compare "+
			"against later.\n", err)
		printEvalRun(sum)
		os.Exit(1)
	}

	if vals["json"] == "true" {
		emitJSON(evalRunJSON(sum))
		return
	}
	printEvalRun(sum)
	// Where it went, so `compare` is reachable without knowing the layout.
	fmt.Printf("  stored:    %s\n", store.Path(sum.ID))

	// A suite where anything failed to run is exit 1: a CI job asking "did the
	// eval pass" must not read a truncated run as a success. The pass rate
	// being high is not the same as the suite having been measured.
	if t := sum.Totals(); !t.Complete() {
		os.Exit(1)
	}
}

// printEvalRun reports one run.
//
// §20.11's line is `eval e1: 20 cases, 20 completed, 11.30 USD`, and it is kept
// because it is the shape people scan. What is added is everything that line
// leaves room to misread: "20 completed" says nothing about whether the cases
// were judged, and a truncated run prints the same shape as a whole one.
func printEvalRun(s *eval.RunSummary) {
	t := s.Totals()

	// Notes first. See the file comment.
	for _, n := range s.Notes() {
		fmt.Printf("note: %s\n", n)
	}
	if len(s.Notes()) > 0 {
		fmt.Println()
	}

	// usd, not %.2f, on both figures. `0.01 USD of 0.00` is what %.2f printed
	// for a budget of 0.001 — a ceiling the user typed, rounded to the value
	// that means "none", next to a spend that appears to exceed it infinitely.
	// Two pennies is the natural precision for a bill and the wrong precision
	// for a threshold.
	fmt.Printf("eval %s: %d cases, %d judged, %s USD of %s\n",
		s.ID, t.Cases, t.Judged, usd(t.CostUSD), usd(s.BudgetUSD))

	if rate, ok := t.PassRate(); ok {
		fmt.Printf("  pass rate: %.2f (%d passed, %d failed)\n", rate, t.Passed, t.Failed)
	} else {
		// Not 0.00. See the file comment.
		fmt.Printf("  pass rate: none — no case produced a judgeable answer\n")
	}
	if t.Errored > 0 || t.Skipped > 0 {
		fmt.Printf("  unjudged:  %d errored, %d skipped\n", t.Errored, t.Skipped)
	}
	if mean, ok := t.MeanCostUSD(); ok {
		fmt.Printf("  mean cost: %.4f USD per judged case\n", mean)
	}

	// Failures are named, not counted. "6 failed" tells somebody something is
	// wrong without telling them what to look at.
	for _, r := range s.Results {
		if r.Status == eval.StatusFail {
			fmt.Printf("  fail: %-24s %s\n", r.Case, r.Why)
		}
	}
	for _, r := range s.Results {
		if r.Status == eval.StatusError {
			fmt.Printf("  error: %-23s %s\n", r.Case, r.Why)
		}
	}
}

// cmdEvalCompare prints §20.11's table, with what cannot be attributed to the
// change.
func cmdEvalCompare(args []string) {
	c := surface.Lookup("eval", "compare")
	vals, err := parseInvocation(c, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "iash eval compare: %v\n\n"+
			"usage: iash eval compare <baseline> <candidate>\n", err)
		os.Exit(2)
	}

	base, err := loadEvalRun(vals["baseline"])
	if err != nil {
		fmt.Fprintf(os.Stderr, "iash eval compare: baseline: %v\n", err)
		os.Exit(1)
	}
	cand, err := loadEvalRun(vals["candidate"])
	if err != nil {
		fmt.Fprintf(os.Stderr, "iash eval compare: candidate: %v\n", err)
		os.Exit(1)
	}

	cmp := eval.Compare(base, cand)

	if vals["json"] == "true" {
		emitJSON(evalCompareJSON(cmp))
		return
	}
	printEvalCompare(cmp)
}

func printEvalCompare(cmp *eval.Comparison) {
	// Warnings first, and with a blank line after, so the table cannot be read
	// before them. This is the single most important formatting decision in the
	// file: `compare`'s output is quoted in decisions, and a delta of +0.15
	// between two different suites is a change of question presented as a
	// change of quality.
	for _, w := range cmp.Warnings {
		fmt.Printf("warning: %s\n", w)
	}
	if len(cmp.Warnings) > 0 {
		fmt.Println()
	}

	bt, ct := cmp.BaseTotals, cmp.CandTotals
	bl, cl := cmp.Baseline.ID, cmp.Candidate.ID

	// The ids are 16 characters (e20260828T220223) and the columns were 9, so
	// the header ran into itself and sat two characters left of the numbers
	// beneath it. Caught by looking at real output rather than at the format
	// string: with the hand-written ids of the tests -- e1, e2 -- every column
	// fitted and the misalignment was invisible.
	//
	// Widened to fit an id rather than truncating one. A truncated id is not a
	// label a reader can retype into the next command, and this table is
	// copied into decisions.
	//
	// MEASURED, not constant. This was `const w = 17`, chosen to fit the
	// 16-character timestamp, and it broke the very next day: collision
	// resolution can hand out e20260828T223406-2, which is 18. A literal
	// width is a claim about the longest id the program can produce, made in
	// the one file that does not mint them -- so it is wrong every time that
	// claim changes elsewhere, and it was already wrong twice. Measuring is
	// what makes the table correct for ids nobody has thought of yet.
	w := 9
	for _, id := range []string{bl, cl} {
		if len(id) > w {
			w = len(id)
		}
	}
	fmt.Printf("%-16s %*s %*s %*s\n", "", w, bl, w, cl, 9, "delta")
	row := func(label string, b, c float64, bok, cok bool, prec int) {
		if !bok || !cok {
			// A delta against a number that does not exist is not a number.
			fmt.Printf("%-16s %*s %*s %9s\n", label,
				w, existsOr(b, bok, prec), w, existsOr(c, cok, prec), "—")
			return
		}
		fmt.Printf("%-16s %*.*f %*.*f %+9.*f\n", label,
			w, prec, b, w, prec, c, prec, c-b)
	}

	brate, bok := bt.PassRate()
	crate, cok := ct.PassRate()
	row("pass rate", brate, crate, bok, cok, 2)

	bcost, bok2 := bt.MeanCostUSD()
	ccost, cok2 := ct.MeanCostUSD()
	row("mean cost USD", bcost, ccost, bok2, cok2, 3)

	bturns, bok3 := bt.MeanTurns()
	cturns, cok3 := ct.MeanTurns()
	row("mean turns", bturns, cturns, bok3, cok3, 1)

	// The spend is not a mean and belongs on its own line: it includes errored
	// cases, which the means above deliberately exclude.
	fmt.Printf("%-16s %*.2f %*.2f %+9.2f\n", "total USD",
		w, bt.CostUSD, w, ct.CostUSD, ct.CostUSD-bt.CostUSD)

	// Regressions before fixes. A reader scanning a diff for what broke should
	// not have to read past the good news.
	if regs := cmp.Regressions(); len(regs) > 0 {
		fmt.Printf("\nregressed (%d):\n", len(regs))
		for _, d := range regs {
			fmt.Printf("  %-24s %s → %s  %s\n", d.Case, d.BaseStatus, d.CandStatus, d.Why)
		}
	}
	if fx := cmp.Fixes(); len(fx) > 0 {
		fmt.Printf("\nfixed (%d):\n", len(fx))
		for _, d := range fx {
			fmt.Printf("  %-24s %s → %s\n", d.Case, d.BaseStatus, d.CandStatus)
		}
	}
}

// usd renders money at two decimals, and at more when two would round a real
// amount to zero.
//
// The rule is narrow on purpose: 11.30 stays 11.30, because that is how a bill
// is read and §20.11's line depends on it. But a figure that is not zero must
// never print as "0.00", and a budget is the field where that matters — the
// reader typed the number, and being shown it back as the value that means
// "none" makes them doubt the invocation rather than the precision.
func usd(v float64) string {
	if v != 0 && v < 0.005 {
		// Four places, which reaches the reserve's scale. Below that a figure
		// is not a budget anybody chose, and %g would print exponents.
		return strconv.FormatFloat(v, 'f', 4, 64)
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// cwdOr names the directory a relative suite path was resolved against.
//
// It is printed with the "no suite here" message because that message's most
// likely cause is not a typo in the path; it is being one directory away from
// where the path is correct. An error that says only "nope.yaml does not exist"
// leaves the reader checking the spelling of a name that is spelled right.
func cwdOr() string {
	wd, err := os.Getwd()
	if err != nil {
		// Not fatal, and not a lie either: the suite is still missing, which
		// is the thing being reported. Saying so is better than inventing a
		// directory name to complete a sentence with.
		return "(the working directory could not be determined)"
	}
	return wd
}

// existsOr renders a number, or an em dash when there is none.
func existsOr(v float64, ok bool, prec int) string {
	if !ok {
		return "—"
	}
	return strconv.FormatFloat(v, 'f', prec, 64)
}

// ---------------------------------------------------------------------------
// JSON projections
// ---------------------------------------------------------------------------

// evalRunJSON is the machine view of one run.
//
// pass_rate is omitted rather than zero when nothing was judged, for the same
// reason the trigger CLI omits `next` rather than writing "(paused)": a machine
// field with a human's apology in it is a parse failure for whoever assumed a
// number, and 0.0 is worse still because it parses.
func evalRunJSON(s *eval.RunSummary) map[string]any {
	t := s.Totals()
	m := map[string]any{
		"id":         s.ID,
		"suite":      s.Suite,
		"suite_sha":  s.SuiteSHA,
		"budget_usd": s.BudgetUSD,
		"cases":      t.Cases,
		"judged":     t.Judged,
		"passed":     t.Passed,
		"failed":     t.Failed,
		"errored":    t.Errored,
		"skipped":    t.Skipped,
		"cost_usd":   t.CostUSD,
		"complete":   t.Complete(),
		"results":    s.Results,
		"notes":      s.Notes(),
	}
	if rate, ok := t.PassRate(); ok {
		m["pass_rate"] = rate
	} else {
		m["pass_rate_absent"] = "no case produced a judgeable answer"
	}
	if mean, ok := t.MeanCostUSD(); ok {
		m["mean_cost_usd"] = mean
	}
	if mean, ok := t.MeanTurns(); ok {
		m["mean_turns"] = mean
	}
	return m
}

func evalCompareJSON(c *eval.Comparison) map[string]any {
	m := map[string]any{
		"baseline":  c.Baseline.ID,
		"candidate": c.Candidate.ID,
		"warnings":  c.Warnings,
		"cases":     c.Cases,
	}
	// Warnings are in the JSON too, and first in the map's declaration order
	// for whatever reads it as a document. A machine client that ignores them
	// is making a choice; one that never received them was not given one.
	add := func(prefix string, t eval.Totals) {
		m[prefix+"_cost_usd"] = t.CostUSD
		m[prefix+"_judged"] = t.Judged
		m[prefix+"_cases"] = t.Cases
		if r, ok := t.PassRate(); ok {
			m[prefix+"_pass_rate"] = r
		}
		if v, ok := t.MeanCostUSD(); ok {
			m[prefix+"_mean_cost_usd"] = v
		}
	}
	add("baseline", c.BaseTotals)
	add("candidate", c.CandTotals)
	return m
}

// ---------------------------------------------------------------------------
// The simulated case runner
// ---------------------------------------------------------------------------

// simCases is the --sim executor: it drives the real fold with a fake agent.
//
// It deliberately does NOT try to look like a language model. The answer it
// returns is derived from the objective, so a suite's expectations either match
// it or do not, deterministically — which makes `--sim` useful for exactly one
// thing: checking that a suite's cases, expectations and budget behave the way
// the author intended, before spending money finding out they did not.
//
// A suite that passes under --sim has not been validated as a measure of
// quality. It has been validated as a FILE. That is a smaller claim than it
// looks and it is the claim actually worth making here.
//
// # The blueprint IS loaded, and that is the whole value of --sim
//
// It would be easier not to. The simulated answer does not depend on the
// blueprint, so loading it changes no output on the happy path — which is
// exactly why the first version skipped it, and why that version could not do
// the one job --sim has.
//
// A suite names a blueprint per case. A typo in that name, or a blueprint that
// has since become invalid, is a failure every case will hit — and the cheapest
// possible moment to hear about it is now, not after 19 real runs have been
// paid for. So the load happens per case, its error is returned as a case
// error, and a suite pointing at a missing blueprint reports 0 judged cases
// with a named reason rather than a pass rate of 1.00 over answers no agent
// produced.
//
// It also makes an errored case reachable through the CLI at all, which is the
// only reason `pass rate: none` and `pass_rate_absent` can be observed from
// outside this package.
type simCases struct {
	dir string
	n   int

	// base is the directory the suite file was read from, and blueprint paths
	// are resolved against it.
	//
	// They used to resolve against the process's working directory, which made
	// a suite runnable from exactly one place. `iash eval run
	// --suite ./suites/review-quality.yaml` failed for a suite saying
	// `blueprint: bp.yaml` next to it, and succeeded for one saying
	// `blueprint: suites/bp.yaml` -- so the path in the file had to be written
	// relative to wherever the operator would later stand. Found while checking
	// a README example, because the doc naturally put the suite in ./suites/
	// and the example did not work.
	//
	// Resolving against the suite means the two files that reference each other
	// can sit together and move together, which is what `blueprint: bp.yaml`
	// looks like it already promises.
	base string
	// seen caches blueprint loads by path. A 20-case suite naming one
	// blueprint should read and validate it once: repeating the work would make
	// --sim's cost the file's size times the case count, and a slow --sim is
	// one nobody runs before spending money.
	seen map[string]error
}

// resolve turns a blueprint path from the suite file into a path this process
// can open, by reading it relative to the suite's own directory.
//
// An absolute path is left alone: somebody who wrote one meant it, and
// rewriting it relative to the suite would produce a path that exists nowhere.
func (s *simCases) resolve(bp string) string {
	if bp == "" || filepath.IsAbs(bp) || s.base == "" {
		return bp
	}
	return filepath.Join(s.base, bp)
}

func (s *simCases) RunCase(ctx context.Context, c eval.Case, budgetUSD float64) (eval.CaseOutcome, error) {
	s.n++

	// The blueprint first, and before anything is charged. See the type's
	// comment: this is the check --sim exists to perform, and a case whose
	// blueprint cannot be loaded has not run, so it costs nothing.
	if s.seen == nil {
		s.seen = map[string]error{}
	}
	err, cached := s.seen[c.Blueprint]
	if !cached {
		_, err = blueprint.LoadFile(s.resolve(c.Blueprint))
		s.seen[c.Blueprint] = err
	}
	if err != nil {
		// Returned as an error, so the fold records StatusError with this
		// reason and carries on to the next case rather than stopping. Every
		// case naming the same broken blueprint should be reported, because
		// "all 20 name a file that is not there" and "case 7 has a typo" are
		// different problems with the same symptom in a single case.
		return eval.CaseOutcome{}, fmt.Errorf(
			"blueprint %q could not be loaded, so this case did not run: %v",
			c.Blueprint, err)
	}
	// A fixed, deliberately modest cost. It is not a guess at what a model
	// charges — pretending to know that would make --sim's cost column look
	// like a forecast, and somebody would budget against it.
	const costPerCase = 0.01

	// It never charges more than it was offered, and this parameter was ignored
	// until a test ran the command with --budget 0.001 and watched the
	// simulator spend 0.01: a tenfold overspend, from the one component whose
	// stated purpose is letting an author check budget behaviour before paying
	// to discover it.
	//
	// The fold offers each case what is LEFT, so honouring it is what makes the
	// simulated run's arithmetic resemble the real one's. A simulator that
	// ignores its ceiling cannot demonstrate the behaviour --sim exists to
	// demonstrate; it can only demonstrate that the ceiling is decorative.
	//
	// Truncating rather than erroring is the honest option of the two. A real
	// executor handed less money than a turn costs produces a short, bad answer
	// and bills for it — that is the case-started-on-fumes shape the reserve
	// exists to avoid — so the simulator does the same, and the reserve keeps
	// it rare.
	cost := costPerCase
	if budgetUSD < cost {
		cost = budgetUSD
	}

	// The objective is echoed so a `contains` expectation naming words from the
	// objective passes, and one naming a real answer's substance fails. That
	// asymmetry is the useful part: it exercises both branches of Judge without
	// claiming to be right.
	return eval.CaseOutcome{
		RunID:      fmt.Sprintf("%s-c%d", filepath.Base(s.dir), s.n),
		Result:     fmt.Sprintf("[simulated] %s", c.Objective),
		CostUSD:    cost,
		Turns:      1,
		DurationMs: 0,
	}, nil
}

// evalDir is where runs are stored, and it is a var so tests can point it
// somewhere disposable — the same seam triggerDir uses.
var evalDir = evalstore.DefaultDir

// openEvalStore prepares the run store or gives up loudly.
//
// Failing here rather than returning an error to each caller: every command in
// this file needs the store, and a directory that cannot be created is not a
// condition any of them can do anything useful about.
func openEvalStore() *evalstore.Store {
	s, err := evalstore.Open(evalDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "iash eval: %v\n", err)
		os.Exit(1)
	}
	return s
}

// loadEvalRun resolves a run reference to a stored summary.
//
// This was an always-erroring stub, and its refusal named exactly what was
// missing: a store, because `compare` must read the suite digest of each run to
// notice two runs that measured different questions. That store now exists.
func loadEvalRun(ref string) (*eval.RunSummary, error) {
	// A reference with a .json on it is what somebody types after looking in
	// the directory, or after a shell completed a filename for them. Trimming
	// it is not laziness about validation: without this, `eval compare
	// evals/e1.json ...` resolves to evals/evals/e1.json.json and reports that
	// there is no such run, which is a true sentence about a path the user did
	// not type.
	ref = strings.TrimSuffix(filepath.Base(ref), ".json")
	return openEvalStore().Load(ref)
}

// cmdEvalList prints the stored runs, newest first.
//
// It exists because run ids are timestamps. `compare` takes two of them, and
// without a listing those are two arguments a user has no way to discover — the
// workflow degrades to `ls evals/`, which is a user reading the storage layout
// because the tool declined to tell them.
func cmdEvalList(args []string) {
	c := surface.Lookup("eval", "list")
	vals, err := parseInvocation(c, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "iash eval list: %v\n", err)
		os.Exit(2)
	}

	runs, err := openEvalStore().List()
	if err != nil {
		fmt.Fprintf(os.Stderr, "iash eval list: %v\n", err)
		os.Exit(1)
	}

	if vals["json"] == "true" {
		out := make([]map[string]any, 0, len(runs))
		for _, r := range runs {
			out = append(out, evalRunRowJSON(r))
		}
		emitJSON(map[string]any{"runs": out})
		return
	}

	// An empty list says so, and says what to type. A bare header row is the
	// output that makes a user wonder whether the command worked.
	if len(runs) == 0 {
		fmt.Printf("no eval runs in %s/\n", evalDir)
		fmt.Println("  run a suite: iash eval run SUITE.yaml --budget 1.00 --sim")
		return
	}

	rows := [][6]string{{"ID", "SUITE", "PASS", "JUDGED", "COST", "NOTE"}}
	for _, r := range runs {
		t := r.Totals()
		rate := "none"
		if v, ok := t.PassRate(); ok {
			rate = fmt.Sprintf("%.2f", v)
		}
		// The NOTE column carries the two facts that make a row's pass rate
		// mean less than it appears to, and it is here rather than in a
		// footnote because a table is read one row at a time. "sim" beside a
		// rate of 1.00 is the difference between a result and a rehearsal.
		var note []string
		if r.Simulated {
			note = append(note, "sim")
		}
		if !t.Complete() {
			note = append(note, "incomplete")
		}
		rows = append(rows, [6]string{
			r.ID, r.Suite, rate,
			fmt.Sprintf("%d/%d", t.Judged, t.Cases),
			usd(t.CostUSD),
			strings.Join(note, ","),
		})
	}

	// Widths measured, not fixed, for the reason `trigger list` measures them:
	// a hardcoded width turns ragged the first time a suite has a longer name,
	// and the column a reader scans down is the one that stops lining up.
	var w [6]int
	for _, row := range rows {
		for i, cell := range row {
			if len(cell) > w[i] {
				w[i] = len(cell)
			}
		}
	}
	for _, row := range rows {
		var b strings.Builder
		for i, cell := range row {
			if i == len(row)-1 {
				b.WriteString(cell) // no trailing padding on the last column
				break
			}
			fmt.Fprintf(&b, "%-*s  ", w[i], cell)
		}
		fmt.Println(strings.TrimRight(b.String(), " "))
	}
	// The hint needs two DIFFERENT runs, and with one stored it suggested
	// comparing a run against itself -- an invocation that prints a table of
	// +0.00 deltas and answers nothing. Caught by running the command after
	// the first run, which is the state every user is in exactly once and the
	// state in which they most need the hint to be right.
	//
	// Oldest against newest, so the direction matches what `compare` means by
	// baseline and candidate: the older run is what the newer one is judged
	// against.
	if len(runs) < 2 {
		fmt.Printf("\nrun the suite again to have something to compare against.\n")
		return
	}
	fmt.Printf("\ncompare two: iash eval compare %s %s\n",
		runs[len(runs)-1].ID, runs[0].ID)
}

// evalRunRowJSON is one row of `eval list --json`.
//
// Deliberately NOT evalRunJSON. That document carries every case result, and a
// listing of a year of nightly runs would be megabytes of case detail nobody
// asked for. The fields here are the ones the table shows, plus the two that
// decide whether a rate should be trusted at all.
//
// pass_rate follows the same rule it does everywhere else in this repository:
// absent when nothing was judged, never 0.0. A machine field that parses is
// worse than a human string, because 0.0 is a number a caller will average.
func evalRunRowJSON(r *eval.RunSummary) map[string]any {
	t := r.Totals()
	m := map[string]any{
		"id":         r.ID,
		"suite":      r.Suite,
		"suite_sha":  r.SuiteSHA,
		"simulated":  r.Simulated,
		"cases":      t.Cases,
		"judged":     t.Judged,
		"passed":     t.Passed,
		"failed":     t.Failed,
		"cost_usd":   t.CostUSD,
		"complete":   t.Complete(),
		"budget_usd": r.BudgetUSD,
	}
	if r.StartedAt != "" {
		m["started_at"] = r.StartedAt
	}
	if rate, ok := t.PassRate(); ok {
		m["pass_rate"] = rate
	} else {
		m["pass_rate_absent"] = "no case produced a judgeable answer"
	}
	return m
}
