package eval

import (
	"strings"
	"testing"
)

// Totals, rates and Compare.
//
// The arithmetic here is four divisions and it is not what these tests are
// about. They are about the DENOMINATOR, and about a table of deltas being read
// as a causal claim when something other than the prompt produced it.

// run builds a summary from a compact spec: each entry is "case:status:cost".
// Turns default to 1 so mean turns is non-zero without every test saying so.
func run(id, suite, sha string, specs ...string) *RunSummary {
	r := &RunSummary{ID: id, Suite: suite, SuiteSHA: sha}
	for _, s := range specs {
		parts := strings.Split(s, ":")
		res := Result{Case: parts[0], Status: CaseStatus(parts[1]), Turns: 1}
		if len(parts) > 2 {
			// A tiny hand parse rather than strconv, so a malformed spec in a
			// test fails loudly at the assertion instead of silently becoming 0.
			var f float64
			for _, ch := range parts[2] {
				if ch == '.' {
					break
				}
				f = f*10 + float64(ch-'0')
			}
			res.CostUSD = f
		}
		r.Results = append(r.Results, res)
	}
	return r
}

func TestPassRateDividesByJudgedAndNotByCases(t *testing.T) {
	// Three cases: two judged (one pass), one errored.
	r := run("e1", "s", "sha1", "a:pass", "b:fail", "c:error")
	tot := r.Totals()

	if tot.Cases != 3 {
		t.Errorf("Cases = %d, want 3", tot.Cases)
	}
	if tot.Judged != 2 {
		t.Errorf("Judged = %d, want 2 (an errored case produced no judgeable answer)", tot.Judged)
	}

	got, ok := tot.PassRate()
	if !ok {
		t.Fatal("PassRate reported nothing to divide")
	}
	if got != 0.5 {
		t.Errorf("PassRate = %.3f, want 0.500\n"+
			"  dividing by Cases would give %.3f, which counts a harness "+
			"failure as a worse prompt", got, 1.0/3.0)
	}
}

// TestAnErroredCaseIsNotAFailure is the distinction the whole four-status enum
// exists for.
//
// A crashed run says nothing about quality. Counting it as a failure means a
// flaky executor is indistinguishable from a regression, and the person reading
// the report goes to look at the prompt.
func TestAnErroredCaseIsNotAFailure(t *testing.T) {
	tot := run("e1", "s", "sha1", "a:pass", "b:error").Totals()
	if tot.Failed != 0 {
		t.Errorf("Failed = %d, want 0: an error is not a failed assertion", tot.Failed)
	}
	if tot.Errored != 1 {
		t.Errorf("Errored = %d, want 1", tot.Errored)
	}
	rate, ok := tot.PassRate()
	if !ok || rate != 1.0 {
		t.Errorf("PassRate = %.2f (ok=%v), want 1.00: the one case that "+
			"produced an answer passed", rate, ok)
	}
	if tot.Complete() {
		t.Error("Complete() is true on a run with an errored case, so nothing " +
			"would tell the reader the rate is over a subset")
	}
}

// TestASkippedCaseIsNeitherPassNorFail: a budget-exhausted suite has a hole in
// its measurement, and a hole is not a result.
func TestASkippedCaseIsNeitherPassNorFail(t *testing.T) {
	tot := run("e1", "s", "sha1", "a:pass", "b:skipped", "c:skipped").Totals()
	if tot.Judged != 1 {
		t.Errorf("Judged = %d, want 1", tot.Judged)
	}
	if tot.Skipped != 2 {
		t.Errorf("Skipped = %d, want 2", tot.Skipped)
	}
	if tot.Complete() {
		t.Error("a run with skipped cases reports itself complete")
	}
}

// TestNoJudgedCasesHasNoPassRate refuses to invent a number.
//
// Zero judged cases is not 0.0 and not 1.0. Returning either invites a
// comparison against it, and the comparison would be against nothing.
func TestNoJudgedCasesHasNoPassRate(t *testing.T) {
	tot := run("e1", "s", "sha1", "a:error", "b:skipped").Totals()
	if _, ok := tot.PassRate(); ok {
		t.Error("PassRate claims a value over zero judged cases")
	}
	if _, ok := tot.MeanCostUSD(); ok {
		t.Error("MeanCostUSD claims a value over zero judged cases")
	}
	if _, ok := tot.MeanTurns(); ok {
		t.Error("MeanTurns claims a value over zero judged cases")
	}
}

// TestCostIncludesErroredCasesButTheMeanDoesNot states the trade-off both ways.
//
// An errored case can burn most of a budget before failing, so the TOTAL must
// include it — that is the number the next --budget is chosen against. The MEAN
// must not, or it stops being comparable with the pass rate beside it. The two
// therefore disagree, and Complete() is what tells the reader to look.
func TestCostIncludesErroredCasesButTheMeanDoesNot(t *testing.T) {
	r := run("e1", "s", "sha1", "a:pass:2", "b:error:8")
	tot := r.Totals()

	if tot.CostUSD != 10 {
		t.Errorf("CostUSD = %.2f, want 10.00: money spent on an errored case "+
			"is still spent", tot.CostUSD)
	}
	mean, ok := tot.MeanCostUSD()
	if !ok {
		t.Fatal("no mean")
	}
	if mean != 2 {
		t.Errorf("MeanCostUSD = %.2f, want 2.00 (over the 1 judged case)", mean)
	}
	// And the disagreement is detectable, which is the only reason it is
	// acceptable.
	if tot.Complete() {
		t.Error("the run reports complete, so nothing signals that total/cases " +
			"and the mean disagree")
	}
}

// TestTheMeanCostAndPassRateShareADenominator is what makes §20.11's table
// readable as a trade-off.
//
// A mean cost over attempted cases beside a pass rate over judged ones are two
// numbers from different populations, and the "cost per point of quality" a
// reader computes from them is wrong.
func TestTheMeanCostAndPassRateShareADenominator(t *testing.T) {
	r := run("e1", "s", "sha1", "a:pass:4", "b:fail:6", "c:error:100")
	tot := r.Totals()
	mean, _ := tot.MeanCostUSD()
	if mean != 5 {
		t.Errorf("MeanCostUSD = %.2f, want 5.00 (10 over 2 judged)", mean)
	}
	rate, _ := tot.PassRate()
	if rate != 0.5 {
		t.Errorf("PassRate = %.2f, want 0.50", rate)
	}
	// Both over 2. If either used 3, the pair would be incomparable.
}

// TestMeanTurnsAlsoExcludesUnjudgedCases exists because mutation testing found
// it missing, and the gap is worth naming: the cost mean was tested, the turns
// mean was assumed to follow.
//
// It does not follow — it is a separate numerator and a separate line in
// §20.11's table, and the two means must be over the same population for the
// three rows to be read together. An errored case that burned turns before
// failing inflates a turns mean drawn from the all-cases sum, in the direction
// that makes a change look chattier rather than broken.
func TestMeanTurnsAlsoExcludesUnjudgedCases(t *testing.T) {
	// Three cases, one turn each: two judged, one errored.
	r := run("e1", "s", "sha1", "a:pass", "b:fail", "c:error")
	tot := r.Totals()

	if tot.Turns != 3 {
		t.Errorf("Turns = %d, want 3: turns spent on an errored case were "+
			"still spent", tot.Turns)
	}
	mean, ok := tot.MeanTurns()
	if !ok {
		t.Fatal("no mean turns")
	}
	if mean != 1 {
		t.Errorf("MeanTurns = %.2f, want 1.00 (2 turns over 2 judged cases)\n"+
			"  dividing the all-cases turn count by the judged count gives "+
			"%.2f, an average of nothing", mean, 1.5)
	}
}

// TestCasesArePairedByName is the pairing the suite loader's duplicate-name
// check protects.
func TestCasesArePairedByName(t *testing.T) {
	base := run("e1", "s", "sha1", "a:pass", "b:fail")
	cand := run("e2", "s", "sha1", "b:pass", "a:fail") // reordered on purpose
	c := Compare(base, cand)

	if len(c.Cases) != 2 {
		t.Fatalf("got %d paired cases, want 2", len(c.Cases))
	}
	byName := map[string]CaseDelta{}
	for _, d := range c.Cases {
		byName[d.Case] = d
	}
	if d := byName["a"]; d.BaseStatus != StatusPass || d.CandStatus != StatusFail {
		t.Errorf("case a: %v -> %v, want pass -> fail (order must not matter)",
			d.BaseStatus, d.CandStatus)
	}
	if d := byName["b"]; d.BaseStatus != StatusFail || d.CandStatus != StatusPass {
		t.Errorf("case b: %v -> %v, want fail -> pass", d.BaseStatus, d.CandStatus)
	}
}

// TestCaseOrderIsBaselineFirstThenAdditions keeps the table readable beside the
// baseline it is compared against, with new cases visible as additions.
func TestCaseOrderIsBaselineFirstThenAdditions(t *testing.T) {
	base := run("e1", "s", "sha1", "zulu:pass", "alpha:pass")
	cand := run("e2", "s", "sha1", "alpha:pass", "zulu:pass", "added:pass")
	c := Compare(base, cand)

	want := []string{"zulu", "alpha", "added"}
	if len(c.Cases) != len(want) {
		t.Fatalf("got %d cases, want %d", len(c.Cases), len(want))
	}
	for i, w := range want {
		if c.Cases[i].Case != w {
			t.Errorf("cases[%d] = %q, want %q (baseline order, then additions)",
				i, c.Cases[i].Case, w)
		}
	}
}

// TestDifferentSuitesAreTheFirstWarning is the failure that makes an eval tool
// worse than no eval tool.
//
// Comparing "review-quality" against "latency" produces a table of numbers that
// reads as a quality delta and is a change of subject. It must be the first
// warning, because it invalidates everything below it and a caveat printed
// after the conclusion has been drawn is not a caveat.
func TestDifferentSuitesAreTheFirstWarning(t *testing.T) {
	base := run("e1", "review-quality", "sha1", "a:pass")
	cand := run("e2", "latency", "sha2", "a:fail")
	c := Compare(base, cand)

	if len(c.Warnings) == 0 {
		t.Fatal("comparing two different suites produced no warning at all")
	}
	first := c.Warnings[0]
	if !strings.Contains(first, "DIFFERENT SUITES") {
		t.Errorf("the first warning is not about the suites differing: %q", first)
	}
	if !strings.Contains(first, "review-quality") || !strings.Contains(first, "latency") {
		t.Errorf("the warning does not name both suites: %q", first)
	}
}

// TestASuiteThatChangedUnderTheSameNameIsCaught is the sneakier version.
//
// Same name, edited file. Nothing on screen looks wrong, and the delta mixes a
// prompt change with a question change. The digest is the only thing that can
// notice, which is why Suite carries one.
func TestASuiteThatChangedUnderTheSameNameIsCaught(t *testing.T) {
	// The digests are built so a truncated one and a whole one are
	// distinguishable: the first twelve characters are shared with the tail
	// that must NOT be printed. Asserting only the prefix would pass whether
	// or not the digest was shortened at all.
	base := run("e1", "review-quality", "aaaaaaaaaaaaTAILBASE", "a:pass")
	cand := run("e2", "review-quality", "bbbbbbbbbbbbTAILCAND", "a:pass")
	c := Compare(base, cand)

	if len(c.Warnings) == 0 {
		t.Fatal("an edited suite under the same name produced no warning")
	}
	w := c.Warnings[0]
	if !strings.Contains(w, "FILE CHANGED") {
		t.Errorf("the first warning does not say the file changed: %q", w)
	}
	// It shows enough of each digest to be checkable...
	if !strings.Contains(w, "aaaaaaaaaaaa") || !strings.Contains(w, "bbbbbbbbbbbb") {
		t.Errorf("the warning does not show both digests: %q", w)
	}
	// ...and no more than that. A warning is only read if it is short, and a
	// full digest is a wall of hex that pushes the sentence off the line. This
	// half of the assertion is the half that mutation testing found missing.
	if strings.Contains(w, "TAILBASE") || strings.Contains(w, "TAILCAND") {
		t.Errorf("the warning prints whole digests instead of short ones: %q", w)
	}
}

// TestTheSameSuiteProducesNoSuiteWarning: the check must not fire on the normal
// case, or it becomes noise that gets skipped, taking the real warnings with it.
func TestTheSameSuiteProducesNoSuiteWarning(t *testing.T) {
	base := run("e1", "s", "sha1", "a:pass", "b:pass", "c:pass", "d:pass")
	cand := run("e2", "s", "sha1", "a:pass", "b:pass", "c:pass", "d:pass")
	c := Compare(base, cand)

	for _, w := range c.Warnings {
		if strings.Contains(w, "SUITE") || strings.Contains(w, "FILE CHANGED") {
			t.Errorf("identical suites warned about the suite: %q", w)
		}
	}
}

// TestCasesInOnlyOneRunAreWarnedAbout catches the partial re-run.
//
// Running 5 of 20 cases and comparing against the full 20 produces a pass rate
// over a different population, and it is the easy mistake to make while
// iterating on one failing case.
func TestCasesInOnlyOneRunAreWarnedAbout(t *testing.T) {
	base := run("e1", "s", "sha1", "a:pass", "b:fail", "c:pass")
	cand := run("e2", "s", "sha1", "b:pass")
	c := Compare(base, cand)

	var found string
	for _, w := range c.Warnings {
		if strings.Contains(w, "baseline and not in the candidate") {
			found = w
		}
	}
	if found == "" {
		t.Fatalf("no warning about the missing cases: %v", c.Warnings)
	}
	if !strings.Contains(found, "a") || !strings.Contains(found, "c") {
		t.Errorf("the warning does not name the missing cases: %q", found)
	}

	// The reverse direction too: a case the candidate added.
	c = Compare(run("e1", "s", "sha1", "a:pass"),
		run("e2", "s", "sha1", "a:pass", "new:pass"))
	var rev string
	for _, w := range c.Warnings {
		if strings.Contains(w, "candidate and not in the baseline") {
			rev = w
		}
	}
	if rev == "" {
		t.Errorf("no warning about a case only the candidate ran: %v", c.Warnings)
	}
}

// TestAnIncompleteRunIsWarnedAboutWithItsNumbers.
//
// Every rate is over the judged subset, so a run with errors or skips has rates
// over a population the reader did not choose — and the warning has to say
// which run and how many, or it is not actionable.
func TestAnIncompleteRunIsWarnedAboutWithItsNumbers(t *testing.T) {
	base := run("e1", "s", "sha1", "a:pass", "b:pass", "c:error", "d:skipped")
	cand := run("e2", "s", "sha1", "a:pass", "b:pass", "c:pass", "d:pass")
	c := Compare(base, cand)

	var found string
	for _, w := range c.Warnings {
		if strings.Contains(w, "baseline run judged") {
			found = w
		}
	}
	if found == "" {
		t.Fatalf("no warning about the incomplete baseline: %v", c.Warnings)
	}
	for _, want := range []string{"2 of 4", "1 errored", "1 skipped"} {
		if !strings.Contains(found, want) {
			t.Errorf("the warning is missing %q: %q", want, found)
		}
	}
	// The complete candidate must not be warned about.
	for _, w := range c.Warnings {
		if strings.Contains(w, "candidate run judged") {
			t.Errorf("a complete run was warned about: %q", w)
		}
	}
}

// TestASmallDeltaIsFlaggedAsWithinNoise is the limitation stated where it is
// actually read.
//
// §20.11's example celebrates +0.15 over 20 cases, which is three cases. With
// one sample per case that is inside the variation of running the same prompt
// twice, and a reader who is not told cannot know.
func TestASmallDeltaIsFlaggedAsWithinNoise(t *testing.T) {
	// 10 judged cases: one case is worth 0.10. A one-case move must be flagged.
	base := run("e1", "s", "sha1", "a:pass", "b:pass", "c:pass", "d:pass", "e:pass",
		"f:fail", "g:fail", "h:fail", "i:fail", "j:fail")
	cand := run("e2", "s", "sha1", "a:pass", "b:pass", "c:pass", "d:pass", "e:pass",
		"f:pass", "g:fail", "h:fail", "i:fail", "j:fail")
	c := Compare(base, cand)

	var found string
	for _, w := range c.Warnings {
		if strings.Contains(w, "within the noise") {
			found = w
		}
	}
	if found == "" {
		t.Fatalf("a one-case delta was not flagged as noise: %v", c.Warnings)
	}
	// The arithmetic is in the message, so it cannot be dismissed as
	// boilerplate: the band on 5/10 against 6/10 is ±0.40, four times the delta.
	if !strings.Contains(found, "±0.40") {
		t.Errorf("the warning does not show the band it judged against: %q", found)
	}
	if !strings.Contains(found, "10 and 10 judged cases") {
		t.Errorf("the warning does not show the sample sizes: %q", found)
	}
}

// TestTheDocumentedSuccessCaseIsFlaggedAsNoise is the test that found the bug,
// and the finding is worth stating plainly: §20.11's worked example is not
// evidence of an improvement.
//
// The doc celebrates 0.65 → 0.80 over 20 cases. The 95% band on a difference
// between 13/20 and 16/20 is ±0.27, nearly twice the delta. The first version of
// this warning used a hand-chosen threshold — "within two cases' worth", 0.10
// here — and stayed SILENT on exactly this table, the one table in the
// repository a reader is most likely to imitate.
//
// A threshold picked by hand cannot know how many samples it is looking at.
// That is the whole reason it was replaced by an interval.
func TestTheDocumentedSuccessCaseIsFlaggedAsNoise(t *testing.T) {
	var baseSpecs, candSpecs []string
	for i := 0; i < 20; i++ {
		name := string(rune('a' + i))
		bs, cs := "fail", "fail"
		if i < 13 { // 13/20 = 0.65
			bs = "pass"
		}
		if i < 16 { // 16/20 = 0.80
			cs = "pass"
		}
		baseSpecs = append(baseSpecs, name+":"+bs)
		candSpecs = append(candSpecs, name+":"+cs)
	}
	base := run("e1", "review-quality", "sha1", baseSpecs...)
	cand := run("e2", "review-quality", "sha1", candSpecs...)

	// Fail loudly if the fixture drifts from the documented table, since the
	// whole point is that these are §20.11's numbers and not example numbers.
	if r, _ := base.Totals().PassRate(); r != 0.65 {
		t.Fatalf("fixture is not §20.11's table: baseline rate %.2f, want 0.65", r)
	}
	if r, _ := cand.Totals().PassRate(); r != 0.80 {
		t.Fatalf("fixture is not §20.11's table: candidate rate %.2f, want 0.80", r)
	}

	c := Compare(base, cand)
	var found string
	for _, w := range c.Warnings {
		if strings.Contains(w, "within the noise") {
			found = w
		}
	}
	if found == "" {
		t.Fatalf("§20.11's own +0.15 over 20 cases was reported with no "+
			"sample-size warning; the band is ±0.27, nearly twice the delta: %v",
			c.Warnings)
	}
	if !strings.Contains(found, "+0.15") {
		t.Errorf("the warning does not state the delta it is about: %q", found)
	}
}

// TestTheSameDeltaOverEnoughCasesIsNotNoise is the pair to the test above, and
// it is what makes an interval worth having instead of a blanket caveat.
//
// An identical +0.15 over 100 cases has a band of ±0.12 and IS evidence. A
// warning that fired on both would be telling the reader that sample size does
// not matter, which is the opposite of the point.
func TestTheSameDeltaOverEnoughCasesIsNotNoise(t *testing.T) {
	var baseSpecs, candSpecs []string
	for i := 0; i < 100; i++ {
		name := "c" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		bs, cs := "fail", "fail"
		if i < 65 {
			bs = "pass"
		}
		if i < 80 {
			cs = "pass"
		}
		baseSpecs = append(baseSpecs, name+":"+bs)
		candSpecs = append(candSpecs, name+":"+cs)
	}
	c := Compare(run("e1", "s", "sha1", baseSpecs...), run("e2", "s", "sha1", candSpecs...))
	for _, w := range c.Warnings {
		if strings.Contains(w, "within the noise") {
			t.Errorf("+0.15 over 100 cases (band ±0.12) was dismissed as noise: %q", w)
		}
	}
}

// TestAPerfectRunDoesNotClaimCertainty is why the interval is Agresti-Caffo and
// not the textbook two-proportion one.
//
// The naive standard error is p(1-p)/n, which is ZERO when p is 0 or 1. Two
// small runs going 4/4 and then 3/4 would be compared against a band of ±0.00
// and the difference declared real — certainty manufactured by the formula out
// of four samples. Adding one success and one failure to each arm is what stops
// the band collapsing.
func TestAPerfectRunDoesNotClaimCertainty(t *testing.T) {
	base := run("e1", "s", "sha1", "a:pass", "b:pass", "c:pass", "d:pass")
	cand := run("e2", "s", "sha1", "a:pass", "b:pass", "c:pass", "d:fail")

	if band := passRateBand(base.Totals(), cand.Totals()); band < 0.2 {
		t.Errorf("band on 4/4 against 3/4 is ±%.2f; a naive interval gives ±0.00 "+
			"here and calls four samples conclusive", band)
	}
	c := Compare(base, cand)
	var found bool
	for _, w := range c.Warnings {
		if strings.Contains(w, "within the noise") {
			found = true
		}
	}
	if !found {
		t.Errorf("a one-case move out of four was reported as a real change: %v",
			c.Warnings)
	}
}

// TestTheBandIsOverJudgedCasesNotDeclaredOnes pins the denominator inside the
// noise warning itself.
//
// The warning's persuasive force is the arithmetic it shows. If its sample
// sizes are the DECLARED case counts while the pass rates it judges are over
// judged ones, the sentence is doing the exact thing this file exists to
// prevent — mixing two populations — inside the warning that complains about it.
//
// The other noise tests cannot see this, because every case in them is judged
// and the two counts are therefore equal. This one errors a case in each run so
// they differ (4 judged of 5 declared), and asserts the reported sizes are the
// judged ones.
func TestTheBandIsOverJudgedCasesNotDeclaredOnes(t *testing.T) {
	base := run("e1", "s", "sha1", "a:pass", "b:pass", "c:fail", "d:fail", "e:error")
	cand := run("e2", "s", "sha1", "a:pass", "b:pass", "c:pass", "d:fail", "e:error")
	c := Compare(base, cand)

	var found string
	for _, w := range c.Warnings {
		if strings.Contains(w, "within the noise") {
			found = w
		}
	}
	if found == "" {
		t.Fatalf("a one-case delta was not flagged as noise: %v", c.Warnings)
	}
	if !strings.Contains(found, "4 and 4 judged cases") {
		t.Errorf("the warning reports declared cases, not judged ones: %q\n"+
			"  5 is the declared count, but the rates beside it are over the "+
			"4 that produced an answer", found)
	}
	// And the band itself must use the same 4, not 5.
	want := passRateBand(Totals{Passed: 2, Judged: 4}, Totals{Passed: 3, Judged: 4})
	if got := passRateBand(base.Totals(), cand.Totals()); got != want {
		t.Errorf("band = %.4f, want %.4f (over judged cases)", got, want)
	}
}

// TestALargeDeltaIsNotFlaggedAsNoise is the other half.
//
// A warning that fires on every comparison is a warning nobody reads, and it
// would take the real ones with it.
func TestALargeDeltaIsNotFlaggedAsNoise(t *testing.T) {
	base := run("e1", "s", "sha1", "a:fail", "b:fail", "c:fail", "d:fail", "e:fail",
		"f:fail", "g:fail", "h:fail", "i:fail", "j:fail")
	cand := run("e2", "s", "sha1", "a:pass", "b:pass", "c:pass", "d:pass", "e:pass",
		"f:pass", "g:pass", "h:pass", "i:fail", "j:fail")
	c := Compare(base, cand)

	for _, w := range c.Warnings {
		if strings.Contains(w, "within the noise") {
			t.Errorf("a 0.80 delta was called noise: %q", w)
		}
	}
}

// TestAnIdenticalResultIsNotFlaggedAsNoise: zero delta is not a small delta,
// and "your unchanged result might be noise" is a sentence with no meaning.
func TestAnIdenticalResultIsNotFlaggedAsNoise(t *testing.T) {
	base := run("e1", "s", "sha1", "a:pass", "b:fail", "c:pass", "d:fail")
	cand := run("e2", "s", "sha1", "a:pass", "b:fail", "c:pass", "d:fail")
	c := Compare(base, cand)

	for _, w := range c.Warnings {
		if strings.Contains(w, "within the noise") {
			t.Errorf("an unchanged pass rate was flagged as noise: %q", w)
		}
	}
}

// TestARunWithNothingJudgedIsReportedRatherThanShownAsZero.
//
// 0.00 across the board is a table that looks like a result.
func TestARunWithNothingJudgedIsReportedRatherThanShownAsZero(t *testing.T) {
	base := run("e1", "s", "sha1", "a:error", "b:error")
	cand := run("e2", "s", "sha1", "a:pass", "b:pass")
	c := Compare(base, cand)

	var found bool
	for _, w := range c.Warnings {
		if strings.Contains(w, "no pass rate to compare") {
			found = true
		}
	}
	if !found {
		t.Errorf("a run with nothing judged produced no warning: %v", c.Warnings)
	}
}

// TestRegressedIsNarrowOnPurpose.
//
// It requires the baseline to have PASSED. A case that errored in both runs did
// not regress, and a case that was already failing did not either — counting
// them inflates the number with things that did not change.
func TestRegressedIsNarrowOnPurpose(t *testing.T) {
	cases := []struct {
		name       string
		base, cand CaseStatus
		want       bool
	}{
		{"pass to fail", StatusPass, StatusFail, true},
		{"pass to error", StatusPass, StatusError, true},
		{"pass to skipped", StatusPass, StatusSkipped, true},
		{"pass to pass", StatusPass, StatusPass, false},
		{"fail to fail", StatusFail, StatusFail, false},
		{"error to error", StatusError, StatusError, false},
		{"error to fail", StatusError, StatusFail, false},
		{"fail to error", StatusFail, StatusError, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Present in both runs: this table is about STATUS
			// transitions, and the presence flags are what the two
			// tests below are for. Before Regressed required
			// InCandidate these were zero and it did not matter,
			// which is exactly how a case deleted from the suite
			// got counted as a regression.
			d := CaseDelta{InBaseline: true, InCandidate: true,
				BaseStatus: tc.base, CandStatus: tc.cand}
			if got := d.Regressed(); got != tc.want {
				t.Errorf("Regressed() = %v, want %v for %s -> %s",
					got, tc.want, tc.base, tc.cand)
			}
		})
	}
}

// TestFixedRequiresARealFailureBefore: error -> pass is a harness that started
// working, not a prompt that got better, and counting it as a fix attributes a
// repair to the wrong change.
func TestFixedRequiresARealFailureBefore(t *testing.T) {
	both := func(b, c CaseStatus) CaseDelta {
		return CaseDelta{InBaseline: true, InCandidate: true, BaseStatus: b, CandStatus: c}
	}
	if both(StatusError, StatusPass).Fixed() {
		t.Error("error -> pass counted as a fix: that is a harness that " +
			"started working, not a better prompt")
	}
	if !both(StatusFail, StatusPass).Fixed() {
		t.Error("fail -> pass is not counted as a fix")
	}
}

// TestACaseDeletedFromTheSuiteIsNotARegression.
//
// Found by RUNNING `eval compare` against two stored runs of an edited suite,
// which was not possible until runs persisted. A deleted case has BaseStatus
// pass and CandStatus "", so the old condition -- CandStatus != StatusPass --
// was satisfied, and the regression list printed `finds-xss  pass → ` with an
// empty arrow.
//
// Deleting a case must not look like breaking one. The count under "regressed"
// is the number a reader treats as "what this change broke", and inflating it
// with cases nobody ran sends somebody investigating a failure that does not
// exist. The removal IS reported -- by the warning about cases present in only
// one run -- which is the honest place for it, because it is a fact about the
// suite and not about the prompt.
func TestACaseDeletedFromTheSuiteIsNotARegression(t *testing.T) {
	d := CaseDelta{Case: "finds-xss", InBaseline: true, InCandidate: false,
		BaseStatus: StatusPass, CandStatus: ""}
	if d.Regressed() {
		t.Error("a case deleted from the suite was counted as a regression.\n" +
			"  it would print as `finds-xss  pass → ` with an empty arrow, and " +
			"the regression COUNT -- which a reader treats as what this change " +
			"broke -- would include a case nobody ran")
	}
}

// TestACaseAddedToTheSuiteIsNotAFix.
//
// The mirror, and the more dangerous of the two, because nobody investigates
// good news. A case added to the suite that passes on its first run never
// failed, so the change did not fix it. Counting it credits a prompt change
// with work it did not do, and the way that lands is: somebody edits a suite,
// sees "fixed (3)", and ships.
func TestACaseAddedToTheSuiteIsNotAFix(t *testing.T) {
	d := CaseDelta{Case: "finds-xss-v2", InBaseline: false, InCandidate: true,
		BaseStatus: "", CandStatus: StatusPass}
	if d.Fixed() {
		t.Error("a case added to the suite was counted as a fix.\n" +
			"  it never failed, so nothing about it was repaired -- and unlike " +
			"a spurious regression, nobody goes looking to disprove a fix")
	}
	// And it must not be a regression either, in the direction where the
	// baseline is the one missing the case.
	if d.Regressed() {
		t.Error("a case added to the suite was counted as a regression")
	}
}

// TestRegressionsAndFixesAreListedByCase is why the delta table is not the
// whole output.
//
// "-0.05" is a number; "case finds-sql-injection now fails because the result
// does not contain X" is a thing to go and fix.
func TestRegressionsAndFixesAreListedByCase(t *testing.T) {
	base := run("e1", "s", "sha1", "keeps:pass", "breaks:pass", "heals:fail")
	cand := run("e2", "s", "sha1", "keeps:pass", "breaks:fail", "heals:pass")
	cand.Results[1].Why = `result does not contain "SQL injection"`
	c := Compare(base, cand)

	regs := c.Regressions()
	if len(regs) != 1 || regs[0].Case != "breaks" {
		t.Fatalf("Regressions() = %v, want just [breaks]", regs)
	}
	if !strings.Contains(regs[0].Why, "SQL injection") {
		t.Errorf("the regression carries no reason: %q\n"+
			"  without it the reader goes to a transcript for something the "+
			"judgement already knew", regs[0].Why)
	}

	fixes := c.Fixes()
	if len(fixes) != 1 || fixes[0].Case != "heals" {
		t.Errorf("Fixes() = %v, want just [heals]", fixes)
	}
}

// TestWarningsAreOrderedMostInvalidatingFirst.
//
// The CLI prints them in order and the first is the one that gets read. A note
// about sample size above "these are different suites" buries the fact that
// nothing below it means anything.
func TestWarningsAreOrderedMostInvalidatingFirst(t *testing.T) {
	// A comparison that trips several warnings at once: different suites,
	// missing cases, an incomplete run, and a small delta.
	base := run("e1", "alpha", "sha1", "a:pass", "b:fail", "c:error", "gone:pass")
	cand := run("e2", "beta", "sha2", "a:pass", "b:pass", "c:pass")
	c := Compare(base, cand)

	if len(c.Warnings) < 3 {
		t.Fatalf("expected several warnings, got %v", c.Warnings)
	}
	if !strings.Contains(c.Warnings[0], "DIFFERENT SUITES") {
		t.Errorf("warnings[0] = %q, want the suite mismatch first", c.Warnings[0])
	}
	// And the noise note, if present, is not above the structural ones.
	for i, w := range c.Warnings {
		if strings.Contains(w, "within the noise") && i == 0 {
			t.Error("the sample-size note is first, above warnings that " +
				"invalidate the comparison entirely")
		}
	}
}

// TestAMissingDigestDoesNotFabricateAWarning.
//
// An older run with no recorded digest is unknown, not different. Warning that
// the suite changed when there is nothing to compare would be a false alarm,
// and false alarms are how the real warnings stop being read.
func TestAMissingDigestDoesNotFabricateAWarning(t *testing.T) {
	base := run("e1", "s", "", "a:pass", "b:pass", "c:pass", "d:pass")
	cand := run("e2", "s", "sha2", "a:pass", "b:pass", "c:pass", "d:pass")
	c := Compare(base, cand)

	for _, w := range c.Warnings {
		if strings.Contains(w, "FILE CHANGED") || strings.Contains(w, "DIFFERENT SUITES") {
			t.Errorf("a missing digest was treated as a different suite: %q", w)
		}
	}
}

// TestACleanComparisonHasNoWarnings is the case that makes the others worth
// having.
//
// Same suite, same cases, both complete, a delta too large to be noise. If this
// produced warnings they would all be noise, and the reader would learn to skip
// them.
func TestACleanComparisonHasNoWarnings(t *testing.T) {
	base := run("e1", "s", "sha1", "a:fail", "b:fail", "c:fail", "d:fail")
	cand := run("e2", "s", "sha1", "a:pass", "b:pass", "c:pass", "d:fail")
	c := Compare(base, cand)

	if len(c.Warnings) != 0 {
		t.Errorf("a clean comparison produced warnings:\n  %s",
			strings.Join(c.Warnings, "\n  "))
	}
}
