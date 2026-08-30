package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/eval"
	"github.com/michiTrader/arxi/internal/surface"
)

// The eval CLI, exercised as a process.
//
// # Why these tests exist at all, stated bluntly
//
// cmd/arxi/eval.go shipped in a state where EVERY possible invocation exited 2.
// It gated the command on --sim, read that flag out of surface.Registry, and
// nobody had declared it there — so the parser refused the only flag that gets
// past the gate. The command could not be made to run at all.
//
// It built cleanly. It vetted cleanly. A full `go test ./...` was green. Nothing
// in this repository could observe the bug, because everything that could
// observe it is what this file is. The bug was found by typing the command,
// which is a method that does not survive the next change.
//
// So the standard here is higher than "the functions return the right values":
// every test below runs the binary and asserts on what a person sees. The
// package's in-process tests cannot, because these paths call os.Exit, and
// os.Exit in a test binary kills the test.
//
// # What is asserted, and why each one is not decoration
//
//   - The exit codes, because that is what CI reads. A truncated run printing a
//     high pass rate and exiting 0 is the single worst output this command can
//     produce, and it is invisible to anybody eyeballing the text.
//   - The ORDER of notes and numbers, because a caveat under a table is read
//     after the conclusion has been drawn.
//   - That absent numbers are absent, in both projections. "0.00" is a value a
//     reader compares against; a missing pass_rate key is one a program must
//     handle.
//
// The harness (TestMain, buildIash, arxi, workdir) lives in trigger_cli_test.go
// and is shared deliberately: one binary, built once per package run.
//
// # The mutation that is left, and why it is not a gap
//
// 27 mutations of eval.go: 26 caught, 1 not killable, 0 invalid.
//
// The survivor deletes the declared-but-unbuilt fallback in cmdEval — the
// branch that hands a subcommand the surface advertises to main's precise
// answer instead of "not an eval command". It changes nothing today because
// both declared eval subcommands are built, so the switch never reaches its
// default with a declared name.
//
// It is the same survivor the trigger CLI documents, for the same reason, and
// the same conclusion holds: the branch is unreachable TODAY and load-bearing
// on the day a third eval subcommand is declared, which is precisely when it
// stops being unreachable. TestADeclaredButUnbuiltEvalSubcommandIsNotCalledUnknown
// is what starts failing then. Deleting the guard to raise a mutation score
// would trade a real future protection for a number.
//
// One other mutation — removing the errors.Is(err, fs.ErrNotExist) check —
// failed to compile, because dropping it orphans two imports. It was rewritten
// to compile and then CAUGHT, rather than counted as invalid: a mutation that
// does not build measures nothing, and is indistinguishable from one that was
// caught if the only thing read is an exit code.
//
// Two mutations survived the FIRST version of this file and are worth naming,
// because both were real gaps rather than equivalences: deleting the "unjudged:
// N errored, N skipped" line, and moving the notes to print after the numbers
// in one specific arrangement. The first is now covered by two tests (skipped
// and errored halves separately, since the line is one condition over two
// counts); the second by an offset assertion.

// evalSuite writes a suite file, and the blueprint its cases name, and returns
// the suite's path.
//
// The blueprint is written because --sim loads it. That is not incidental: a
// suite naming a blueprint that does not exist is the mistake --sim is for, so
// a fixture that omitted the file would exercise the error path in every test
// and the happy path in none.
//
// The cases are written so the simulator's answer decides the verdict: simCases
// echoes the objective, so a `contains` naming a word from the objective passes
// and one naming anything else fails. That is what makes a fixture here
// predictable without pretending the simulator is a model.
func evalSuite(t *testing.T, dir, name string, body string) string {
	t.Helper()
	bp := filepath.Join(dir, "bp.yaml")
	if err := os.WriteFile(bp,
		[]byte("name: solo\nmembers:\n  - {name: a, tools: [read]}\n"), 0o644); err != nil {
		t.Fatalf("writing the blueprint the suite names: %v", err)
	}
	// The suite's `blueprint:` is resolved relative to the SUITE file, so the
	// fixtures name bp.yaml and put it beside the suite.
	//
	// Note what this helper hid for the whole of this step: it writes both
	// files into dir and the binary is then run with dir as its working
	// directory, so "relative to the suite" and "relative to the process"
	// pointed at the same place and a bug between them was untestable here.
	// TestASuiteIsRunnableFromSomewhereElse is deliberately not built on this
	// helper for that reason.
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("writing the suite fixture: %v", err)
	}
	return p
}

// passingSuite is two cases the simulator satisfies.
const passingSuite = `name: passes
blueprint: bp.yaml
cases:
  - name: alpha
    objective: simulated alpha
    expect: {contains: ["simulated"]}
  - name: beta
    objective: simulated beta
    expect: {contains: ["simulated"]}
`

// mixedSuite is one the simulator satisfies and one it cannot.
const mixedSuite = `name: mixed
blueprint: bp.yaml
cases:
  - name: alpha
    objective: simulated alpha
    expect: {contains: ["simulated"]}
  - name: beta
    objective: find the injection
    expect: {contains: ["SQL injection"]}
`

// unjudgeableSuite is a suite no case of which can be judged, because every
// case names a blueprint that is not there.
//
// It exists for the pass-rate-over-nothing path, and reaching that path from
// outside the package is only possible because --sim loads the blueprint. With
// a simulator that ignored it, every case would produce an answer, every answer
// would be judged, and "no case produced a judgeable answer" would be a branch
// no test could enter through the CLI at all.
const unjudgeableSuite = `name: unjudgeable
blueprint: nowhere.yaml
cases:
  - name: alpha
    objective: simulated alpha
    expect: {contains: ["simulated"]}
`

// TestEveryDeclaredFlagOfEvalRunIsAccepted is the test that would have caught
// the shipped bug, and it is first because of that.
//
// It reads the flags out of the registry rather than listing them, for the same
// reason the parser does: a test that hand-lists --suite, --budget, --sim and
// --json stops covering the one added next, and "declared but not accepted" is
// exactly the failure being guarded against. A hand-written list would also
// have been written from the same wrong belief that produced the bug.
func TestEveryDeclaredFlagOfEvalRunIsAccepted(t *testing.T) {
	dir := workdir(t)
	suite := evalSuite(t, dir, "s.yaml", passingSuite)

	c := surface.Lookup("eval", "run")
	if c == nil {
		t.Fatal("eval run is not in the surface")
	}
	for _, pp := range c.Params {
		if pp.Positional {
			continue
		}
		// Every non-positional flag is offered alongside a complete, valid
		// invocation. The point is not that the flag does something; it is
		// that the parser recognises the name the registry advertises.
		args := []string{"eval", "run", suite, "--budget", "1.00", "--sim"}
		if pp.Name != "budget" && pp.Name != "sim" {
			if pp.Type == "bool" {
				args = append(args, "--"+pp.Name)
			} else {
				args = append(args, "--"+pp.Name, "0")
			}
		}
		got := arxi(t, dir, args...)
		if strings.Contains(got.out, "is not a flag of") {
			t.Errorf("eval run declares --%s and the parser refuses it:\n%s\n\n"+
				"This is the bug this file was written for. A flag in "+
				"surface.Registry that parseInvocation rejects is not a "+
				"harmless mismatch: eval run gates itself on --sim, so an "+
				"undeclared --sim made every invocation exit 2, and `arxi "+
				"schema` advertised a parameter an agent could not use.",
				pp.Name, got.out)
		}
	}
}

// TestARunWithoutSimIsRefusedAndSaysWhatToTypeInstead.
//
// Exit 2 and not 1: asking for a live executor that does not exist is misuse of
// the interface, not an operational failure of the run. The distinction is what
// a CI job branches on.
func TestARunWithoutSimIsRefusedAndSaysWhatToTypeInstead(t *testing.T) {
	dir := workdir(t)
	suite := evalSuite(t, dir, "s.yaml", passingSuite)

	got := arxi(t, dir, "eval", "run", suite, "--budget", "1.00")
	if got.code != 2 {
		t.Fatalf("a run with no executor exited %d, want 2 (misuse, not an "+
			"operational failure):\n%s", got.code, got.out)
	}
	// The refusal must contain the invocation that works. A message that
	// explains a limitation without naming the way round it makes the reader
	// guess at a flag, and the guess they make is to drop the suite.
	if !strings.Contains(got.out, "--sim") {
		t.Errorf("the refusal does not mention --sim:\n%s", got.out)
	}
	if !strings.Contains(got.out, "what works today: arxi eval run "+suite) {
		t.Errorf("the refusal does not spell out the working invocation with "+
			"the user's own suite path:\n%s", got.out)
	}
	// And it must not report one. The refusal DISCUSSES pass rates — that is
	// its argument — so the assertion is on the reporting line's shape and not
	// on the phrase, which is the distinction the first version of this test
	// got wrong.
	if strings.Contains(got.out, "pass rate:") {
		t.Errorf("a refused run printed a pass rate line:\n%s", got.out)
	}
	if strings.Contains(got.out, "cases,") {
		t.Errorf("a refused run printed a run header:\n%s", got.out)
	}
}

// TestACompleteRunReportsWhatItMeasuredAndExitsZero walks the §20.11 shape.
func TestACompleteRunReportsWhatItMeasuredAndExitsZero(t *testing.T) {
	dir := workdir(t)
	suite := evalSuite(t, dir, "s.yaml", mixedSuite)

	got := arxi(t, dir, "eval", "run", suite, "--budget", "1.00", "--sim")
	if got.code != 0 {
		t.Fatalf("a complete run exited %d, want 0:\n%s", got.code, got.out)
	}
	for _, want := range []string{
		"2 cases, 2 judged",
		"pass rate: 0.50 (1 passed, 1 failed)",
		"mean cost: 0.0100 USD per judged case",
	} {
		if !strings.Contains(got.out, want) {
			t.Errorf("the report is missing %q:\n%s", want, got.out)
		}
	}
	// "judged" and not "completed". §20.11's line says "20 completed", which
	// says nothing about whether anything was judgeable — and the judged count
	// is the denominator of the number on the next line.
	if !strings.Contains(got.out, "judged") {
		t.Errorf("the header does not name the judged count, which is the "+
			"denominator the pass rate is over:\n%s", got.out)
	}
}

// TestAFailingCaseIsNamedNotCounted.
//
// "1 failed" tells somebody that something is wrong without telling them what
// to look at, and the reason a case failed is the whole reason the eval was run.
func TestAFailingCaseIsNamedNotCounted(t *testing.T) {
	dir := workdir(t)
	suite := evalSuite(t, dir, "s.yaml", mixedSuite)

	got := arxi(t, dir, "eval", "run", suite, "--budget", "1.00", "--sim")
	if !strings.Contains(got.out, "fail: beta") {
		t.Errorf("the failing case is not named:\n%s", got.out)
	}
	if !strings.Contains(got.out, `result does not contain "SQL injection"`) {
		t.Errorf("the failing case is named without its reason, which is the "+
			"thing a failing eval is read for:\n%s", got.out)
	}
	// The passing case is NOT listed. A report that names everything is a log,
	// and the failures stop standing out in it.
	if strings.Contains(got.out, "fail: alpha") {
		t.Errorf("a passing case was listed as a failure:\n%s", got.out)
	}
}

// TestATruncatedRunExitsNonZeroEvenWithAPerfectPassRate.
//
// This is the most important assertion in the file. The fixture stops after 2
// of 3 cases and every case that ran passed, so the text says 1.00 — and a CI
// job that read only the exit code of a command like this would record a
// perfect eval for a suite that was two thirds measured.
func TestATruncatedRunExitsNonZeroEvenWithAPerfectPassRate(t *testing.T) {
	dir := workdir(t)
	suite := evalSuite(t, dir, "s.yaml", `name: three
blueprint: bp.yaml
cases:
  - name: a1
    objective: simulated one
    expect: {contains: ["simulated"]}
  - name: a2
    objective: simulated two
    expect: {contains: ["simulated"]}
  - name: a3
    objective: simulated three
    expect: {contains: ["simulated"]}
`)

	// 0.02 buys two cases at the simulator's 0.01, and the reserve then blocks
	// the third.
	got := arxi(t, dir, "eval", "run", suite, "--budget", "0.02", "--sim")

	// The fixture must actually be the dangerous case, or this test passes
	// while proving nothing.
	if !strings.Contains(got.out, "pass rate: 1.00") {
		t.Fatalf("the fixture is no longer the dangerous shape: it must report "+
			"a PERFECT pass rate over a truncated run, since that is the "+
			"output a reader is least likely to question. Got:\n%s", got.out)
	}
	if got.code == 0 {
		t.Errorf("a run that stopped on budget exited 0 while printing a pass "+
			"rate of 1.00:\n%s\n\nA CI job reads the exit code. A suite that "+
			"was one third unmeasured must not be recordable as a clean pass.",
			got.out)
	}
	if !strings.Contains(got.out, "1 case(s) never ran (a3)") {
		t.Errorf("the skipped case is not named:\n%s", got.out)
	}
	// The unjudged tally, next to the pass rate rather than only in the notes.
	// A mutation deleting this line survived the first version of this file,
	// and it is the line that makes "1.00" and "2 judged of 3" appear in the
	// same glance: the pass rate is only interpretable beside the count of what
	// it could not measure.
	if !strings.Contains(got.out, "unjudged:  0 errored, 1 skipped") {
		t.Errorf("the unjudged tally is missing:\n%s\n\nA pass rate needs the "+
			"count of what it could NOT measure printed next to it, not only "+
			"in a note.", got.out)
	}
}

// TestErroredCasesAreTalliedBesideThePassRate is the errored half of the above.
//
// Both halves are asserted because the line is one condition over two counts,
// and a version that reported only skipped cases would look correct in every
// budget-truncated test in this file while saying nothing about a suite whose
// cases crashed.
func TestErroredCasesAreTalliedBesideThePassRate(t *testing.T) {
	dir := workdir(t)
	suite := evalSuite(t, dir, "s.yaml", `name: half-broken
blueprint: bp.yaml
cases:
  - name: works
    objective: simulated works
    expect: {contains: ["simulated"]}
  - name: broken
    blueprint: nowhere.yaml
    objective: simulated broken
    expect: {contains: ["simulated"]}
`)
	got := arxi(t, dir, "eval", "run", suite, "--budget", "1.00", "--sim")

	if !strings.Contains(got.out, "unjudged:  1 errored, 0 skipped") {
		t.Errorf("the errored tally is missing:\n%s", got.out)
	}
	// And the pass rate is over 1 case, not 2. This is the denominator claim
	// the whole package rests on: dividing by declared cases would report 0.50
	// here and treat a broken blueprint as a worse prompt.
	if !strings.Contains(got.out, "2 cases, 1 judged") {
		t.Errorf("the header does not distinguish declared from judged:\n%s", got.out)
	}
	if !strings.Contains(got.out, "pass rate: 1.00") {
		t.Errorf("the pass rate is not over the JUDGED case:\n%s\n\nDividing "+
			"by declared cases would report 0.50 and blame the prompt for a "+
			"blueprint that could not be loaded.", got.out)
	}
}

// TestTheCaveatsPrintBeforeTheNumbers.
//
// Not cosmetic. A reader who reaches "pass rate: 1.00" first has drawn the
// conclusion the note exists to prevent, and reading the note afterwards asks
// them to un-draw it. Byte offsets, because that is the only way to state it.
func TestTheCaveatsPrintBeforeTheNumbers(t *testing.T) {
	dir := workdir(t)
	suite := evalSuite(t, dir, "s.yaml", `name: two
blueprint: bp.yaml
cases:
  - name: a1
    objective: simulated one
    expect: {contains: ["simulated"]}
  - name: a2
    objective: simulated two
    expect: {contains: ["simulated"]}
`)
	got := arxi(t, dir, "eval", "run", suite, "--budget", "0.01", "--sim")

	note := strings.Index(got.out, "note:")
	rate := strings.Index(got.out, "pass rate")
	if note < 0 {
		t.Fatalf("no note was printed for a run that stopped on budget:\n%s", got.out)
	}
	if rate < 0 {
		t.Fatalf("no pass rate line at all:\n%s", got.out)
	}
	if note > rate {
		t.Errorf("the note about the run's validity printed AFTER the pass "+
			"rate (offsets %d and %d):\n%s\n\nA caveat under a table is read "+
			"after the conclusion has been drawn.", note, rate, got.out)
	}
}

// TestABudgetTruncatedRunSaysWhichWayTheBiasRuns.
//
// The direction is the part that cannot be guessed. Cases run in file order, so
// a truncated run measured a PREFIX, and hard-cases-last — the natural way to
// write a suite — makes the reported rate too high, and higher the earlier the
// money runs out.
func TestABudgetTruncatedRunSaysWhichWayTheBiasRuns(t *testing.T) {
	dir := workdir(t)
	suite := evalSuite(t, dir, "s.yaml", `name: three
blueprint: bp.yaml
cases:
  - name: a1
    objective: simulated one
    expect: {contains: ["simulated"]}
  - name: a2
    objective: simulated two
    expect: {contains: ["simulated"]}
  - name: a3
    objective: simulated three
    expect: {contains: ["simulated"]}
`)
	got := arxi(t, dir, "eval", "run", suite, "--budget", "0.02", "--sim")

	for _, want := range []string{
		"the first ones in the file, not a sample of the suite",
		"over a prefix",
		"raise --budget before reading the pass rate as a result",
	} {
		if !strings.Contains(got.out, want) {
			t.Errorf("the budget note is missing %q:\n%s", want, got.out)
		}
	}
}

// TestAPassRateOverNothingIsAbsentNotZero.
//
// 0.00 parses. It is a value a reader compares against and a program does
// arithmetic on, and "the worst possible result" and "no result" are opposite
// facts that must not share a representation.
func TestAPassRateOverNothingIsAbsentNotZero(t *testing.T) {
	dir := workdir(t)
	suite := evalSuite(t, dir, "s.yaml", unjudgeableSuite)

	// The case errors on its missing blueprint, so nothing is judged even
	// though the budget was ample.
	got := arxi(t, dir, "eval", "run", suite, "--budget", "1.00", "--sim")

	if strings.Contains(got.out, "pass rate: 0.00") {
		t.Fatalf("a run that judged nothing reported a pass rate of 0.00:\n%s\n\n"+
			"That is a measurement of total failure. The run measured "+
			"nothing at all, which is a different fact.", got.out)
	}
	if !strings.Contains(got.out, "pass rate: none") {
		t.Errorf("the absent pass rate is not reported as absent:\n%s", got.out)
	}
	if !strings.Contains(got.out, "no case produced a judgeable answer") {
		t.Errorf("the report does not say WHY there is no pass rate:\n%s", got.out)
	}
	if got.code == 0 {
		t.Errorf("a run that judged nothing exited 0:\n%s", got.out)
	}
}

// TestTheJSONOmitsAPassRateItDidNotMeasure is the machine half of the above,
// and the failure mode is worse there: a human reading "0.00" might wonder,
// where a program will not.
func TestTheJSONOmitsAPassRateItDidNotMeasure(t *testing.T) {
	dir := workdir(t)
	suite := evalSuite(t, dir, "s.yaml", unjudgeableSuite)

	got := arxi(t, dir, "eval", "run", suite, "--budget", "1.00", "--sim", "--json")

	var m map[string]any
	if err := json.Unmarshal([]byte(got.out), &m); err != nil {
		t.Fatalf("--json did not emit one JSON document: %v\n%s", err, got.out)
	}
	if _, present := m["pass_rate"]; present {
		t.Errorf("pass_rate is present with value %v after judging nothing.\n"+
			"A machine field must not carry a number that was not measured; "+
			"0.0 is worse than a human string here, because it parses.",
			m["pass_rate"])
	}
	if _, present := m["pass_rate_absent"]; !present {
		t.Errorf("neither pass_rate nor pass_rate_absent is present, so a "+
			"client cannot tell a missing measurement from a missing "+
			"field:\n%s", got.out)
	}
	// The spend IS a fact even when nothing was judged, and must be reported:
	// it is what the next --budget is chosen against.
	if _, present := m["cost_usd"]; !present {
		t.Errorf("cost_usd is missing; the spend is real even when the pass "+
			"rate is not:\n%s", got.out)
	}
}

// TestASuiteNamingAMissingBlueprintIsNotAPerfectRun.
//
// The failure this prevents is the quietest one available: --sim's answer does
// not depend on the blueprint, so a simulator that skipped loading it would
// judge every case, satisfy every `contains` naming a word from the objective,
// and report 1.00 over a suite whose every case points at a file that is not
// there. The author would then pay for 20 real runs to be told.
func TestASuiteNamingAMissingBlueprintIsNotAPerfectRun(t *testing.T) {
	dir := workdir(t)
	suite := evalSuite(t, dir, "s.yaml", `name: broken
blueprint: nowhere.yaml
cases:
  - name: alpha
    objective: simulated alpha
    expect: {contains: ["simulated"]}
  - name: beta
    objective: simulated beta
    expect: {contains: ["simulated"]}
`)
	got := arxi(t, dir, "eval", "run", suite, "--budget", "1.00", "--sim")

	if strings.Contains(got.out, "pass rate: 1.00") {
		t.Fatalf("a suite whose every case names a missing blueprint reported "+
			"a perfect pass rate:\n%s\n\n--sim exists to catch exactly this "+
			"before 20 real runs are paid for.", got.out)
	}
	if got.code == 0 {
		t.Errorf("the run exited 0:\n%s", got.out)
	}
	// Both cases are reported, not just the first. "every case names a file
	// that is not there" and "case 7 has a typo" have the same symptom in one
	// case and different causes.
	for _, want := range []string{"error: alpha", "error: beta"} {
		if !strings.Contains(got.out, want) {
			t.Errorf("%q is not reported:\n%s", want, got.out)
		}
	}
	if !strings.Contains(got.out, `blueprint "nowhere.yaml" could not be loaded`) {
		t.Errorf("the reason does not name the blueprint that is missing:\n%s", got.out)
	}
	// And nothing was charged for a case that never started.
	if !strings.Contains(got.out, "0.00 USD") {
		t.Errorf("a suite where no case ran reported a nonzero spend:\n%s", got.out)
	}
}

// TestTheSimulatorDoesNotSpendMoreThanItWasOffered.
//
// Found by a test, not by reading: the simulator ignored its budgetUSD argument
// and charged its flat 0.01 against a --budget of 0.001, a tenfold overspend by
// the one component whose stated purpose is letting an author check budget
// behaviour before paying to discover it. A simulator that overruns its ceiling
// cannot demonstrate the ceiling.
func TestTheSimulatorDoesNotSpendMoreThanItWasOffered(t *testing.T) {
	dir := workdir(t)
	suite := evalSuite(t, dir, "s.yaml", passingSuite)

	got := arxi(t, dir, "eval", "run", suite, "--budget", "0.001", "--sim", "--json")
	var m map[string]any
	if err := json.Unmarshal([]byte(got.out), &m); err != nil {
		t.Fatalf("--json did not emit one JSON document: %v\n%s", err, got.out)
	}
	spent, _ := m["cost_usd"].(float64)
	if spent > 0.001 {
		t.Errorf("the run spent %v against a budget of 0.001:\n%s\n\n"+
			"The fold offers each case what is LEFT; a runner that ignores "+
			"the offer makes --sim's budget arithmetic decorative.",
			spent, got.out)
	}
}

// TestABudgetTooSmallToRoundIsNotPrintedAsZero.
//
// `0.01 USD of 0.00` is what %.2f produced for --budget 0.001: a ceiling the
// user typed, rounded to the one value that means "no budget at all", printed
// next to a spend that appears to exceed it infinitely. Two decimals are right
// for a bill and wrong for a threshold.
func TestABudgetTooSmallToRoundIsNotPrintedAsZero(t *testing.T) {
	dir := workdir(t)
	suite := evalSuite(t, dir, "s.yaml", passingSuite)

	got := arxi(t, dir, "eval", "run", suite, "--budget", "0.001", "--sim")
	if strings.Contains(got.out, "of 0.00\n") {
		t.Errorf("a budget of 0.001 printed as 0.00:\n%s\n\nThe reader typed "+
			"the number; showing it back as the value that means \"none\" "+
			"makes them doubt the invocation rather than the precision.",
			got.out)
	}
	if !strings.Contains(got.out, "0.0010") {
		t.Errorf("the budget is not shown at a precision that can represent "+
			"it:\n%s", got.out)
	}
	// And the ordinary case keeps two decimals, because §20.11's line does.
	got = arxi(t, dir, "eval", "run", suite, "--budget", "11.30", "--sim")
	if !strings.Contains(got.out, "of 11.30") {
		t.Errorf("an ordinary budget lost its two-decimal form:\n%s", got.out)
	}
}

// TestTheBlueprintIsReadOncePerSuiteNotOncePerCase is a performance fact with a
// correctness consequence: a --sim that re-reads and re-validates the blueprint
// for all 20 cases is slow enough that authors stop running it, and an unused
// check is not a check.
func TestTheBlueprintIsReadOncePerSuiteNotOncePerCase(t *testing.T) {
	dir := workdir(t)
	suite := evalSuite(t, dir, "s.yaml", passingSuite)

	// Run once so the blueprint is loaded and cached, then remove it. If the
	// cache works, the second case never touched the file; the assertion is on
	// the run having completed, which is only possible with one read.
	if got := arxi(t, dir, "eval", "run", suite, "--budget", "1.00", "--sim"); got.code != 0 {
		t.Fatalf("the baseline run failed: %s", got.out)
	}
	// This is a same-process claim, so it is checked in-process instead.
	s := &simCases{dir: dir}
	c := eval.Case{Name: "x", Blueprint: filepath.Join(dir, "bp.yaml"), Objective: "simulated x"}
	if _, err := s.RunCase(context.Background(), c, 1.0); err != nil {
		t.Fatalf("the first case failed: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "bp.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunCase(context.Background(), c, 1.0); err != nil {
		t.Errorf("the second case re-read the blueprint and failed: %v\n\n"+
			"A 20-case suite naming one blueprint must validate it once; "+
			"otherwise --sim costs the file's size times the case count.", err)
	}
}

// TestTheJSONCarriesTheNotesAndTheJudgedDenominator.
//
// A client that never received the caveats was not given the choice to ignore
// them, and a pass rate without its denominator is not interpretable: 1.00 over
// 2 judged of 20 declared is not the same claim as 1.00 over 20.
func TestTheJSONCarriesTheNotesAndTheJudgedDenominator(t *testing.T) {
	dir := workdir(t)
	suite := evalSuite(t, dir, "s.yaml", mixedSuite)

	got := arxi(t, dir, "eval", "run", suite, "--budget", "1.00", "--sim", "--json")
	var m map[string]any
	if err := json.Unmarshal([]byte(got.out), &m); err != nil {
		t.Fatalf("--json did not emit one JSON document: %v\n%s", err, got.out)
	}
	for _, k := range []string{"pass_rate", "judged", "cases", "notes", "suite_sha", "complete"} {
		if _, present := m[k]; !present {
			t.Errorf("the JSON has no %q:\n%s", k, got.out)
		}
	}
	if notes, _ := m["notes"].([]any); len(notes) == 0 {
		t.Errorf("the JSON carries no notes, so a machine client is not told "+
			"what the number is over:\n%s", got.out)
	}
	// suite_sha is what `compare` uses to notice two runs that measured
	// different questions, so its presence is load-bearing rather than
	// informational.
	if sha, _ := m["suite_sha"].(string); len(sha) != 64 {
		t.Errorf("suite_sha is %q, want a full sha256; it is what tells "+
			"\"the prompt improved\" from \"the questions changed\"", sha)
	}
}

// TestCompareRefusesInsteadOfPrintingATableOfZeroes.
//
// The dangerous alternative is not an error: it is a clean-looking table. Two
// empty summaries compare without complaint and print zeroes and em dashes, a
// comparison of nothing that has the shape of a comparison.
func TestCompareOfARunThatDoesNotExistNamesItAndSaysHowToLook(t *testing.T) {
	// This test used to assert that compare refused BECAUSE runs were not
	// persisted, and it asserted the exact words "are not persisted yet". It
	// was right to be specific: the moment persistence landed it failed, which
	// is precisely what it was for. A test written vaguely enough to survive
	// the feature it describes would have left the stub's obsolete refusal
	// sitting in the binary.
	dir := workdir(t)
	got := arxi(t, dir, "eval", "compare", "e1", "e2")

	if got.code != 1 {
		t.Errorf("compare exited %d, want 1 (the invocation was fine; the "+
			"runs do not exist):\n%s", got.code, got.out)
	}
	if strings.Contains(got.out, "pass rate") {
		t.Errorf("compare printed a table for runs it could not load:\n%s", got.out)
	}
	// The id it could not find, and the way to find out what ids exist. Run
	// ids are timestamps, so "no such run" without a listing hint leaves the
	// reader with no next move except reading the directory.
	for _, want := range []string{`"e1"`, "eval list"} {
		if !strings.Contains(got.out, want) {
			t.Errorf("compare's refusal is missing %q:\n%s", want, got.out)
		}
	}
	// And it must name the BASELINE specifically, since it is the first of two
	// arguments and the reader needs to know which one is wrong.
	if !strings.Contains(got.out, "baseline") {
		t.Errorf("the refusal should say which of the two arguments failed to "+
			"resolve.\n%s", got.out)
	}
}

func TestARunIsStoredAndCanThenBeCompared(t *testing.T) {
	// The end-to-end claim of this whole step, and the test that would have
	// failed at every earlier point in the file's history.
	dir := workdir(t)
	suite := evalSuite(t, dir, "s.yaml", passingSuite)

	first := arxi(t, dir, "eval", "run", suite, "--budget", "1.00", "--sim")
	if first.code != 0 {
		t.Fatalf("the first run failed: %s", first.out)
	}
	// The receipt has to name where it went, or `compare` is reachable only by
	// reading the storage layout.
	if !strings.Contains(first.out, "stored:") {
		t.Errorf("the run did not say where it was stored:\n%s", first.out)
	}

	ids := storedIDs(t, dir)
	if len(ids) != 1 {
		t.Fatalf("want 1 stored run, got %v", ids)
	}

	// A second run of the SAME suite. The ids are timestamps to the second, so
	// this is also the test that would catch an id scheme too coarse to
	// distinguish two runs -- Put refuses a duplicate, so the second run would
	// fail rather than silently overwrite the first.
	second := arxi(t, dir, "eval", "run", suite, "--budget", "1.00", "--sim")
	if second.code != 0 {
		t.Fatalf("the second run failed, which means two runs cannot be told "+
			"apart: %s", second.out)
	}
	ids = storedIDs(t, dir)
	if len(ids) != 2 {
		t.Fatalf("want 2 stored runs, got %v", ids)
	}

	cmp := arxi(t, dir, "eval", "compare", ids[1], ids[0])
	if cmp.code != 0 {
		t.Fatalf("comparing two runs that exist failed: %s", cmp.out)
	}
	if !strings.Contains(cmp.out, "pass rate") {
		t.Errorf("compare printed no table:\n%s", cmp.out)
	}
}

func TestASimulatedRunIsMarkedSimulatedEverywhereItIsReported(t *testing.T) {
	// The field exists because runs persist: without it, `compare` pairs a
	// --sim baseline against a real candidate and reports the difference
	// between a fake executor and a real one as a change in quality.
	dir := workdir(t)
	suite := evalSuite(t, dir, "s.yaml", passingSuite)
	if got := arxi(t, dir, "eval", "run", suite, "--budget", "1.00", "--sim"); got.code != 0 {
		t.Fatalf("the run failed: %s", got.out)
	}
	arxi(t, dir, "eval", "run", suite, "--budget", "1.00", "--sim")

	// In the listing, in the row, where a reader scanning rates will see it.
	list := arxi(t, dir, "eval", "list")
	if !strings.Contains(list.out, "sim") {
		t.Errorf("`eval list` does not mark a simulated run, so a rate of 1.00 "+
			"from a fake executor is indistinguishable from a measurement:\n%s",
			list.out)
	}

	// In the JSON, always present rather than omitted when false.
	var payload struct {
		Runs []map[string]any `json:"runs"`
	}
	lj := arxi(t, dir, "eval", "list", "--json")
	if err := json.Unmarshal([]byte(lj.out), &payload); err != nil {
		t.Fatalf("eval list --json is not JSON: %v\n%s", err, lj.out)
	}
	if len(payload.Runs) == 0 {
		t.Fatal("no runs in the JSON listing")
	}
	if v, ok := payload.Runs[0]["simulated"]; !ok || v != true {
		t.Errorf("want simulated=true in the JSON row, got %v (present=%v)", v, ok)
	}

	// And in compare's warnings, first, ahead of everything else.
	ids := storedIDs(t, dir)
	if len(ids) < 2 {
		t.Fatalf("want 2 runs, got %v", ids)
	}
	cmp := arxi(t, dir, "eval", "compare", ids[1], ids[0])
	if !strings.Contains(cmp.out, "SIMULATED") {
		t.Errorf("comparing two simulated runs printed no warning that the "+
			"table measures nothing:\n%s", cmp.out)
	}
	if i := strings.Index(cmp.out, "SIMULATED"); i > strings.Index(cmp.out, "pass rate") {
		t.Errorf("the simulation warning printed AFTER the table, which is "+
			"after the conclusion has been drawn:\n%s", cmp.out)
	}
}

func TestAnEmptyListSaysSoRatherThanPrintingABareHeader(t *testing.T) {
	dir := workdir(t)
	got := arxi(t, dir, "eval", "list")
	if got.code != 0 {
		t.Fatalf("listing an empty store failed: %s", got.out)
	}
	if strings.Contains(got.out, "PASS") {
		t.Errorf("an empty store printed a header row, which is the output "+
			"that makes a user wonder whether the command worked:\n%s", got.out)
	}
	if !strings.Contains(got.out, "no eval runs") {
		t.Errorf("an empty listing should say there are no runs:\n%s", got.out)
	}
	if !strings.Contains(got.out, "eval run") {
		t.Errorf("an empty listing should say what to type to get one:\n%s", got.out)
	}
}

func TestAListWithOneRunDoesNotSuggestComparingItWithItself(t *testing.T) {
	// Found by running the command. `compare X X` prints a table of +0.00
	// deltas and answers nothing, and this is the state every user is in
	// exactly once -- immediately after their first run, when they most need
	// the suggested next command to be real.
	dir := workdir(t)
	suite := evalSuite(t, dir, "s.yaml", passingSuite)
	if got := arxi(t, dir, "eval", "run", suite, "--budget", "1.00", "--sim"); got.code != 0 {
		t.Fatalf("the run failed: %s", got.out)
	}

	got := arxi(t, dir, "eval", "list")
	ids := storedIDs(t, dir)
	if len(ids) != 1 {
		t.Fatalf("want 1 run, got %v", ids)
	}
	if strings.Contains(got.out, "compare "+ids[0]+" "+ids[0]) {
		t.Errorf("the listing suggested comparing a run against itself:\n%s", got.out)
	}
	if !strings.Contains(got.out, "again") {
		t.Errorf("with one run stored, the listing should say to run the suite "+
			"again rather than offering an invocation that answers nothing:\n%s",
			got.out)
	}
}

func TestTheCompareTableColumnsLineUpWithRealRunIDs(t *testing.T) {
	// The bug this catches was invisible to every other test in this file
	// because they use hand-written ids -- e1, e2 -- and a real id is a
	// 16-character timestamp. The header was %9s, so the two run columns ran
	// into each other and sat left of the numbers beneath them, in the one
	// table in this repository most likely to be copied into a decision.
	dir := workdir(t)
	suite := evalSuite(t, dir, "s.yaml", passingSuite)
	arxi(t, dir, "eval", "run", suite, "--budget", "1.00", "--sim")
	arxi(t, dir, "eval", "run", suite, "--budget", "1.00", "--sim")
	ids := storedIDs(t, dir)
	if len(ids) < 2 {
		t.Fatalf("want 2 runs, got %v", ids)
	}

	out := arxi(t, dir, "eval", "compare", ids[1], ids[0]).out
	var header, passRow string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, ids[0]) && strings.Contains(line, "delta") {
			header = line
		}
		if strings.HasPrefix(line, "pass rate") {
			passRow = line
		}
	}
	if header == "" || passRow == "" {
		t.Fatalf("could not find the header and the pass-rate row:\n%s", out)
	}
	// The candidate id must END where the number under it ends. That is what
	// "the columns line up" means for a right-aligned table, and it is
	// checkable without hardcoding a width.
	wantEnd := len(strings.TrimRight(passRow[:strings.LastIndex(passRow, " ")], " "))
	gotEnd := strings.Index(header, ids[0]) + len(ids[0])
	if gotEnd != wantEnd {
		t.Errorf("the candidate column header ends at %d but its number ends "+
			"at %d, so the table does not line up:\n%s\n%s",
			gotEnd, wantEnd, header, passRow)
	}
	// And the two ids must not be touching.
	if strings.Contains(header, ids[1]+ids[0]) {
		t.Errorf("the two run ids ran into each other in the header:\n%s", header)
	}
}

// storedIDs is the run ids on disk, oldest last, as `eval list --json` reports
// them. Read through the CLI rather than by globbing the directory, so a test
// that passes is evidence the listing works and not just the store.
func storedIDs(t *testing.T, dir string) []string {
	t.Helper()
	got := arxi(t, dir, "eval", "list", "--json")
	if got.code != 0 {
		t.Fatalf("eval list --json failed: %s", got.out)
	}
	var payload struct {
		Runs []struct{ ID string } `json:"runs"`
	}
	if err := json.Unmarshal([]byte(got.out), &payload); err != nil {
		t.Fatalf("eval list --json is not JSON: %v\n%s", err, got.out)
	}
	out := make([]string, 0, len(payload.Runs))
	for _, r := range payload.Runs {
		out = append(out, r.ID)
	}
	return out
}

// TestAMissingSuiteIsNotReportedAsAnInvalidOne.
//
// Different mistakes, different sentences. "the suite is not valid" over `open
// nope.yaml: no such file or directory` sends the reader hunting for a syntax
// error in a file that is not there, when the actual cause is almost always
// being one directory away from where the path is right.
func TestAMissingSuiteIsNotReportedAsAnInvalidOne(t *testing.T) {
	dir := workdir(t)
	got := arxi(t, dir, "eval", "run", "nope.yaml", "--budget", "1.00", "--sim")

	if got.code != 1 {
		t.Errorf("a missing suite exited %d, want 1:\n%s", got.code, got.out)
	}
	if strings.Contains(got.out, "not valid") {
		t.Errorf("a missing file is reported as an invalid suite:\n%s\n\n"+
			"That sends the reader looking for a syntax error in a file that "+
			"does not exist.", got.out)
	}
	if !strings.Contains(got.out, "there is no suite at nope.yaml") {
		t.Errorf("the message does not say the file is missing:\n%s", got.out)
	}
	// The directory the relative path was resolved against, because that is
	// the actual cause more often than the spelling.
	if !strings.Contains(got.out, dir) {
		t.Errorf("the message does not name the directory the path was "+
			"resolved against (%s):\n%s", dir, got.out)
	}
}

// TestAnInvalidSuiteIsRefusedBeforeAnythingIsSpent.
//
// A suite is 20 runs, so its validation belongs before the first one. The
// specific case here is the empty `expect`, which passes on every possible
// output and contributes a guaranteed +1 to the pass rate.
func TestAnInvalidSuiteIsRefusedBeforeAnythingIsSpent(t *testing.T) {
	dir := workdir(t)
	suite := evalSuite(t, dir, "bad.yaml", `name: bad
blueprint: bp.yaml
cases:
  - name: alpha
    objective: simulated alpha
    expect: {}
`)
	got := arxi(t, dir, "eval", "run", suite, "--budget", "1.00", "--sim")

	if got.code != 1 {
		t.Errorf("an invalid suite exited %d, want 1:\n%s", got.code, got.out)
	}
	if !strings.Contains(got.out, "not valid") {
		t.Errorf("the suite was not reported as invalid:\n%s", got.out)
	}
	if strings.Contains(got.out, "pass rate") || strings.Contains(got.out, "USD") {
		t.Errorf("a run was reported for a suite that never should have "+
			"started:\n%s", got.out)
	}
}

// TestABudgetThatIsNotANumberIsMisuse.
//
// Exit 2, and it must not be confused with a budget of zero: `--budget abc`
// parsing to 0.0 would be read by the fold as a refused run, reported as an
// operational failure, and the typo never mentioned.
func TestABudgetThatIsNotANumberIsMisuse(t *testing.T) {
	dir := workdir(t)
	suite := evalSuite(t, dir, "s.yaml", passingSuite)

	got := arxi(t, dir, "eval", "run", suite, "--budget", "abc", "--sim")
	if got.code != 2 {
		t.Errorf("--budget abc exited %d, want 2 (misuse):\n%s", got.code, got.out)
	}
	if !strings.Contains(got.out, `--budget "abc" is not a number`) {
		t.Errorf("the message does not quote back what was typed:\n%s", got.out)
	}
}

// TestABudgetOfZeroIsRefusedRatherThanTreatedAsUnlimitedByTheCLI checks that
// the fold's refusal reaches the user rather than being swallowed.
func TestABudgetOfZeroIsRefusedRatherThanTreatedAsUnlimitedByTheCLI(t *testing.T) {
	dir := workdir(t)
	suite := evalSuite(t, dir, "s.yaml", passingSuite)

	got := arxi(t, dir, "eval", "run", suite, "--budget", "0", "--sim")
	if got.code == 0 {
		t.Errorf("--budget 0 exited 0:\n%s", got.out)
	}
	if !strings.Contains(got.out, "--budget") {
		t.Errorf("the refusal does not name the flag that was wrong:\n%s", got.out)
	}
	if strings.Contains(got.out, "pass rate") {
		t.Errorf("a pass rate was printed for a run with no budget:\n%s", got.out)
	}
}

// TestTheShortFlagsReachTheEvalRun.
//
// -b, -S and -J are declared in the surface's shortFlags table, and a shorthand
// that exists in the help and not in the parser is worse than none.
func TestTheShortFlagsReachTheEvalRun(t *testing.T) {
	dir := workdir(t)
	suite := evalSuite(t, dir, "s.yaml", passingSuite)

	got := arxi(t, dir, "eval", "run", suite, "-b", "1.00", "-S", "-J")
	if got.code != 0 {
		t.Fatalf("the short-flag invocation exited %d:\n%s", got.code, got.out)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(got.out), &m); err != nil {
		t.Fatalf("-J did not produce JSON: %v\n%s", err, got.out)
	}
	if m["judged"] != float64(2) {
		t.Errorf("-b/-S/-J parsed but the run judged %v cases, want 2:\n%s",
			m["judged"], got.out)
	}
}

// TestADeclaredButUnbuiltEvalSubcommandIsNotCalledUnknown.
//
// Same reasoning as the trigger CLI's: a subcommand the surface advertises and
// the binary has not built must get main's precise answer, or the user goes
// hunting for a typo they never made. Both declared eval subcommands are built
// today, so this reads the registry rather than naming one — the day a third is
// declared is the day this starts covering it.
func TestADeclaredButUnbuiltEvalSubcommandIsNotCalledUnknown(t *testing.T) {
	dir := workdir(t)
	for i := range surface.Registry {
		c := &surface.Registry[i]
		if len(c.Path) != 2 || c.Path[0] != "eval" {
			continue
		}
		got := arxi(t, dir, "eval", c.Path[1])
		if strings.Contains(got.out, "is not an eval command") {
			t.Errorf("`eval %s` is declared in the surface and the CLI calls "+
				"it unknown:\n%s", c.Path[1], got.out)
		}
	}
}

// TestAnUnknownEvalSubcommandIsNamedAndTheUsagePrinted.
func TestAnUnknownEvalSubcommandIsNamedAndTheUsagePrinted(t *testing.T) {
	dir := workdir(t)
	got := arxi(t, dir, "eval", "frobnicate")

	if got.code != 2 {
		t.Errorf("an unknown subcommand exited %d, want 2:\n%s", got.code, got.out)
	}
	if !strings.Contains(got.out, `"frobnicate" is not an eval command`) {
		t.Errorf("the unknown subcommand is not quoted back:\n%s", got.out)
	}
	if !strings.Contains(got.out, "usage: arxi eval") {
		t.Errorf("no usage was offered:\n%s", got.out)
	}
}

// TestBareEvalPrintsTheUsageAndIsMisuse.
func TestBareEvalPrintsTheUsageAndIsMisuse(t *testing.T) {
	dir := workdir(t)
	got := arxi(t, dir, "eval")

	if got.code != 2 {
		t.Errorf("bare `arxi eval` exited %d, want 2:\n%s", got.code, got.out)
	}
	for _, want := range []string{"run <suite.yaml>", "compare <baseline> <candidate>"} {
		if !strings.Contains(got.out, want) {
			t.Errorf("the usage is missing %q:\n%s", want, got.out)
		}
	}
}

// TestTheRunWritesNothing.
//
// Recorded as a fact rather than a preference: `eval run` has no store behind
// it, which is WHY `eval compare` refuses. If a run ever starts leaving a
// directory here, compare's refusal becomes a lie and this test is where that
// is noticed.
func TestTheRunWritesExactlyOneRunFileAndNothingElse(t *testing.T) {
	// This replaces TestTheRunWritesNothing, which recorded that `eval run`
	// had no store behind it -- the fact that made `eval compare` refuse. It
	// failed the moment persistence landed, which is what it was written to
	// do, and it is replaced rather than deleted because the interesting
	// question survived: a run must leave the ONE file it claims to and not
	// scatter anything else through the user's directory.
	dir := workdir(t)
	suite := evalSuite(t, dir, "s.yaml", passingSuite)

	before := map[string]bool{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the work directory: %v", err)
	}
	for _, e := range entries {
		before[e.Name()] = true
	}

	if got := arxi(t, dir, "eval", "run", suite, "--budget", "1.00", "--sim"); got.code != 0 {
		t.Fatalf("the run failed: %s", got.out)
	}

	var added []string
	entries, err = os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the work directory: %v", err)
	}
	for _, e := range entries {
		if !before[e.Name()] {
			added = append(added, e.Name())
		}
	}
	if len(added) != 1 || added[0] != "evals" {
		t.Fatalf("the run added %v; it should add exactly the evals/ "+
			"directory and nothing else", added)
	}

	// And exactly one file inside it -- no temp file left behind, which is the
	// failure that makes every later `eval list` fail on a file nobody can
	// explain.
	inner, err := os.ReadDir(filepath.Join(dir, "evals"))
	if err != nil {
		t.Fatalf("reading evals/: %v", err)
	}
	if len(inner) != 1 {
		var names []string
		for _, e := range inner {
			names = append(names, e.Name())
		}
		t.Errorf("one run left %d files in evals/ (%s); a leftover temp file "+
			"is what makes a later `eval list` fail on a file nobody can "+
			"explain", len(inner), strings.Join(names, ", "))
	}
	if len(inner) == 1 && !strings.HasSuffix(inner[0].Name(), ".json") {
		t.Errorf("the stored run is not a .json file: %s", inner[0].Name())
	}
}

// TestASuiteIsRunnableFromSomewhereElse is a directory-layout test, and it
// exists because every other test in this file was blind to the bug.
//
// A suite's `blueprint:` used to be opened relative to the process's working
// directory, so `arxi eval run --suite ./suites/s.yaml` could only find
// `blueprint: bp.yaml` if bp.yaml sat next to the OPERATOR, not next to the
// suite. The path written in the file therefore had to anticipate where
// somebody would later be standing, and the same suite worked or errored
// depending on which directory it was invoked from.
//
// The whole existing suite missed it because the fixtures write the suite and
// its blueprint into the same directory the binary runs in, which makes the two
// interpretations identical. This test separates them: the suite and blueprint
// live in a subdirectory, and the command runs from the parent.
//
// It was found by checking a README example against the binary rather than by
// reading code -- the doc naturally put the suite under ./suites/, and the
// example did not work.
func TestASuiteIsRunnableFromSomewhereElse(t *testing.T) {
	dir := workdir(t)
	sub := filepath.Join(dir, "suites")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, "bp.yaml"),
		[]byte("name: solo\nmembers:\n  - {name: a, tools: [read]}\n"), 0o644); err != nil {
		t.Fatalf("blueprint: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, "s.yaml"), []byte(passingSuite), 0o644); err != nil {
		t.Fatalf("suite: %v", err)
	}

	// Run from the PARENT, naming the suite by a relative path. The blueprint
	// is beside the suite, which is the only place a suite author can
	// reasonably be expected to put it.
	res := arxi(t, dir, "eval", "run", "suites/s.yaml", "--budget", "1.00", "--sim")
	if res.code != 0 {
		t.Fatalf("a suite in a subdirectory could not be run from its parent, "+
			"which means the path in `blueprint:` has to be written relative "+
			"to wherever the operator stands:\n%s", res.out)
	}
	if strings.Contains(res.out, "could not be loaded") {
		t.Fatalf("the blueprint beside the suite was not found:\n%s", res.out)
	}
	// And the cases must actually have been judged -- an errored case still
	// exits 0 in some paths, so a run that "succeeded" while measuring nothing
	// would pass a weaker assertion than this one.
	if !strings.Contains(res.out, "2 judged") {
		t.Errorf("expected both cases to be judged, got:\n%s", res.out)
	}
}

// TestAnAbsoluteBlueprintPathIsNotRewritten guards the other half of the
// resolution rule. Joining the suite's directory onto an absolute path would
// produce something like /tmp/x/suites/tmp/x/bp.yaml, which exists nowhere.
func TestAnAbsoluteBlueprintPathIsNotRewritten(t *testing.T) {
	dir := workdir(t)
	sub := filepath.Join(dir, "suites")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	bp := filepath.Join(dir, "elsewhere.yaml")
	if err := os.WriteFile(bp,
		[]byte("name: solo\nmembers:\n  - {name: a, tools: [read]}\n"), 0o644); err != nil {
		t.Fatalf("blueprint: %v", err)
	}
	body := strings.Replace(passingSuite, "blueprint: bp.yaml", "blueprint: "+bp, 1)
	if err := os.WriteFile(filepath.Join(sub, "s.yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("suite: %v", err)
	}

	res := arxi(t, dir, "eval", "run", "suites/s.yaml", "--budget", "1.00", "--sim")
	if res.code != 0 || strings.Contains(res.out, "could not be loaded") {
		t.Fatalf("an absolute blueprint path was not honoured as written:\n%s", res.out)
	}
}
