package eval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Result is what one case produced.
//
// Status is separate from Pass because "failed the assertion" and "never got an
// answer" are different facts, and a boolean cannot hold both. A case whose run
// crashed is not a case whose output was wrong: the first says nothing about
// quality, and counting it as a failure is how a broken harness gets read as a
// worse prompt.
type Result struct {
	Case   string     `json:"case"`
	Status CaseStatus `json:"status"`

	// Why is the reason, and it is present for every non-pass outcome. A
	// failure with no reason sends somebody to a transcript for something the
	// judgement already knew.
	Why string `json:"why,omitempty"`

	RunID    string  `json:"run_id,omitempty"`
	CostUSD  float64 `json:"cost_usd"`
	Turns    int     `json:"turns"`
	Result   string  `json:"result,omitempty"`
	Duration int64   `json:"duration_ms,omitempty"`
}

// CaseStatus is how a case ended.
//
// Four outcomes and not two, because the three non-passing ones need different
// actions: a failed assertion is a finding, an error is a broken harness, and a
// skipped case is a hole in the measurement. Collapsing them into `pass: false`
// makes all three look like the prompt got worse.
type CaseStatus string

const (
	// StatusPass: the case ran and satisfied its expectation.
	StatusPass CaseStatus = "pass"
	// StatusFail: the case ran and did not satisfy its expectation. This is
	// the only status that carries information about quality.
	StatusFail CaseStatus = "fail"
	// StatusError: the case did not produce a judgeable answer — the run
	// crashed, the blueprint would not load, the executor refused. Judging
	// this as a failure attributes a harness problem to the prompt.
	StatusError CaseStatus = "error"
	// StatusSkipped: the case was not attempted. The suite budget was
	// exhausted, or the operator stopped early. A skipped case is not a
	// passing case and not a failing one; it is a measurement that does not
	// exist.
	StatusSkipped CaseStatus = "skipped"
)

// Judged reports whether this outcome says anything about quality.
//
// Only pass and fail do. This is the predicate the pass rate is computed over,
// and having it as a named function rather than an inline condition is
// deliberate: the denominator of a pass rate is the whole argument of this file,
// and it must be stated in exactly one place.
func (s CaseStatus) Judged() bool { return s == StatusPass || s == StatusFail }

// RunSummary is a completed (or abandoned) evaluation run.
//
// SuiteSHA is stored, not just the suite name. Two runs of "review-quality" can
// be two different sets of questions, and a comparison that reports a delta
// between them is reporting a change of subject as a change of quality. The
// digest is what makes that detectable.
type RunSummary struct {
	ID        string  `json:"id"`
	Suite     string  `json:"suite"`
	SuiteSHA  string  `json:"suite_sha"`
	BudgetUSD float64 `json:"budget_usd"`
	StartedAt string  `json:"started_at,omitempty"`

	// Simulated records that these numbers were produced by a fake executor
	// and are not measurements of anything.
	//
	// This field did not exist while runs were thrown away at the end of the
	// process, and it did not need to: a simulated run was on the screen of
	// the person who asked for it. Persisting runs is what creates the
	// problem — once a run is a file, `eval compare` will happily pair a
	// simulated baseline against a real candidate and report a pass-rate
	// delta between fabricated numbers and measured ones. That is the same
	// class of error as comparing two different suites, except worse, because
	// a suite mismatch is at least a comparison of two real things.
	//
	// Deliberately NOT `omitempty`. A field that means "these numbers are
	// fake" must never be absent from the document, because absence and false
	// then look identical to every reader — including a reader that is
	// deciding whether to quote the number in a decision.
	Simulated bool `json:"simulated"`

	Results []Result `json:"results"`
}

// Totals is the arithmetic of one run.
//
// Cases is the total the suite declared; Judged is how many produced an
// answer that could be judged. They differ whenever anything errored or was
// skipped, and every rate below divides by Judged — never by Cases.
type Totals struct {
	Cases   int
	Judged  int
	Passed  int
	Failed  int
	Errored int
	Skipped int

	// CostUSD and Turns are over EVERYTHING that ran, errors included. This is
	// the spend, and it is what the next --budget is chosen against.
	CostUSD float64
	Turns   int

	// JudgedCostUSD and JudgedTurns are over judged cases only, and exist
	// because a mean needs its numerator and denominator drawn from the same
	// population. Dividing the all-cases cost by the judged count produces a
	// "mean" that can exceed every individual case: 4 + 6 + 100 over 2 judged
	// is 55, which is not the average of anything. These are unexported from
	// the printed table on purpose — they are inputs to the means, not numbers
	// a reader should be offered beside the spend.
	JudgedCostUSD float64
	JudgedTurns   int
}

// Totals computes the counts and the spend.
func (r *RunSummary) Totals() Totals {
	var t Totals
	t.Cases = len(r.Results)
	for _, res := range r.Results {
		switch res.Status {
		case StatusPass:
			t.Passed++
		case StatusFail:
			t.Failed++
		case StatusError:
			t.Errored++
		case StatusSkipped:
			t.Skipped++
		}
		if res.Status.Judged() {
			t.Judged++
		}
		// Cost and turns are summed over everything that ran, INCLUDING errors.
		// An errored case can burn most of a budget before it fails, and a cost
		// total that omitted it would understate what the suite actually spent —
		// which is the number the next --budget has to be chosen against.
		t.CostUSD += res.CostUSD
		t.Turns += res.Turns
		if res.Status.Judged() {
			t.JudgedCostUSD += res.CostUSD
			t.JudgedTurns += res.Turns
		}
	}
	return t
}

// PassRate is passed over JUDGED, and the denominator is the point.
//
// Dividing by the declared case count treats an errored case as a failure, so a
// harness problem reads as a worse prompt. Dividing by attempted-including-
// errors does the same thing more quietly. Dividing by judged answers the
// question actually being asked — "of the cases that produced an answer, how
// many were right" — and leaves the unjudged ones to be reported separately,
// which is what Complete() is for.
//
// ok is false when nothing was judged. A pass rate over zero cases is not 0.0
// and not 1.0; it does not exist, and returning either number invites a
// comparison against it.
func (t Totals) PassRate() (rate float64, ok bool) {
	if t.Judged == 0 {
		return 0, false
	}
	return float64(t.Passed) / float64(t.Judged), true
}

// MeanCostUSD is per JUDGED case, for comparability.
//
// §20.11 puts mean cost beside the pass rate so a change can be read as a
// trade-off. That only works if both are over the same denominator: a mean cost
// averaged over attempted cases and a pass rate over judged ones are two
// numbers from different populations, and the "cost per point of quality" a
// reader computes from them is wrong.
//
// Sharing the denominator also means sharing the NUMERATOR's population: the
// cost of judged cases divided by their count. Using Totals.CostUSD here — the
// all-cases spend — over the judged count is the mistake that looks like a
// rounding difference and is not one. One errored case that burned 100 USD
// before failing turns a mean of 5.00 into 55.00, a figure larger than any case
// actually cost, and it moves in the direction that makes a change look
// expensive rather than broken.
//
// The consequence is stated rather than hidden: money spent on errored cases is
// real, appears in Totals.CostUSD, and is therefore NOT in this mean, so the
// mean here can be lower than total/cases. Complete() is what tells the reader
// to look.
func (t Totals) MeanCostUSD() (mean float64, ok bool) {
	if t.Judged == 0 {
		return 0, false
	}
	return t.JudgedCostUSD / float64(t.Judged), true
}

// MeanTurns is per judged case, same reasoning, same paired numerator.
func (t Totals) MeanTurns() (mean float64, ok bool) {
	if t.Judged == 0 {
		return 0, false
	}
	return float64(t.JudgedTurns) / float64(t.Judged), true
}

// Complete reports whether every case was judged.
//
// A run with errored or skipped cases is a partial measurement, and every rate
// derived from it is a rate over a subset the reader did not choose. This is
// what `compare` prints beside the table instead of silently narrowing the
// population.
func (t Totals) Complete() bool { return t.Judged == t.Cases }

// ---------------------------------------------------------------------------
// Comparison
// ---------------------------------------------------------------------------

// Comparison is the answer to "did the change help".
//
// Warnings is not a decoration and not a log. It carries the reasons a delta in
// this table may not mean what it looks like, and the CLI prints them with the
// numbers rather than after them — a caveat below a table gets read after the
// conclusion has already been drawn.
type Comparison struct {
	Baseline  *RunSummary
	Candidate *RunSummary

	BaseTotals Totals
	CandTotals Totals

	// Cases is every case in either run, paired by name.
	Cases []CaseDelta

	Warnings []string
}

// CaseDelta is one case across the two runs.
//
// Both statuses are kept rather than a single "changed" flag: pass→fail and
// pass→error are both regressions in a summary and completely different
// problems, and the second one is not about the prompt at all.
type CaseDelta struct {
	Case string

	InBaseline  bool
	InCandidate bool

	BaseStatus CaseStatus
	CandStatus CaseStatus

	BaseCostUSD float64
	CandCostUSD float64

	// Why carries the candidate's reason when it failed, since that is the
	// thing a reader chases. The baseline's reason is available on the Result.
	Why string
}

// Regressed reports a case that used to pass and now does not.
//
// Deliberately narrow: it requires the baseline to have PASSED, so a case that
// errored in both runs is not a regression, and a case that was already failing
// is not one either. A looser definition inflates the count with things that
// did not change.
//
// InCandidate is required too, and that clause was MISSING until `compare` was
// RUN against two real stored runs of an edited suite. A case DELETED from the
// suite has BaseStatus pass and CandStatus "" — which is not StatusPass — so it
// was reported as a regression and printed as `finds-xss  pass → ` with an
// empty arrow. That is the worst available reading of a suite edit: deleting a
// case looked exactly like breaking one, and the count a reader trusts to mean
// "this change broke things" was inflated by cases nobody ran. The removal is
// already reported by the warning about cases present in only one run, which is
// where it belongs.
//
// Note what made this invisible before persistence: comparing required two
// stored runs, so until this step there was no way to compare a suite against
// an edited version of itself at all.
func (d CaseDelta) Regressed() bool {
	return d.InCandidate && d.BaseStatus == StatusPass && d.CandStatus != StatusPass
}

// Fixed is the mirror: it failed and now passes.
//
// InBaseline for the same reason, and here the false positive is the flattering
// one, which makes it the more dangerous of the two: a case ADDED to the suite
// that passes on its first run would be counted as something the change fixed.
// It never failed. Crediting a prompt change with a case that did not exist
// before it is how a suite edit gets read as an improvement — and unlike a
// spurious regression, nobody investigates good news.
func (d CaseDelta) Fixed() bool {
	return d.InBaseline && d.BaseStatus == StatusFail && d.CandStatus == StatusPass
}

// Compare pairs two runs and reports what changed, with what cannot be
// attributed.
//
// This function's real job is the warnings. The arithmetic is four divisions;
// the difficulty is that a table of deltas is read as a causal claim — "the
// prompt got 15% better" — and there are several ways for the same table to be
// produced by something other than the prompt. Each one is detected here and
// said out loud, because a reader who is not told cannot know.
func Compare(baseline, candidate *RunSummary) *Comparison {
	c := &Comparison{
		Baseline:   baseline,
		Candidate:  candidate,
		BaseTotals: baseline.Totals(),
		CandTotals: candidate.Totals(),
	}

	byName := map[string]*CaseDelta{}
	var order []string
	get := func(name string) *CaseDelta {
		d, ok := byName[name]
		if !ok {
			d = &CaseDelta{Case: name}
			byName[name] = d
			order = append(order, name)
		}
		return d
	}
	for _, r := range baseline.Results {
		d := get(r.Case)
		d.InBaseline, d.BaseStatus, d.BaseCostUSD = true, r.Status, r.CostUSD
	}
	for _, r := range candidate.Results {
		d := get(r.Case)
		d.InCandidate, d.CandStatus, d.CandCostUSD = true, r.Status, r.CostUSD
		d.Why = r.Why
	}
	// Baseline order first, then cases only the candidate has, which is the
	// order somebody reading the baseline alongside expects. Cases added by the
	// candidate land at the end where they are visible as additions.
	for _, name := range order {
		c.Cases = append(c.Cases, *byName[name])
	}

	c.Warnings = warnings(c)
	return c
}

// warnings is the list of reasons this comparison may not mean what it looks
// like.
//
// Ordered most-invalidating first, because the CLI prints them in order and the
// first one is the one that gets read. "These runs measured different suites"
// makes every number below it meaningless, and it must not appear after a note
// about sample size.
func warnings(c *Comparison) []string {
	var out []string

	// 0. Simulated numbers. Ahead of the suite mismatch, and that ordering is
	// an argument: a comparison of two different suites is at least a
	// comparison of two measurements, and the reader who ignores the caveat
	// still learns something real. A simulated run measures nothing at all, so
	// there is no reading of the table below that is partially correct.
	//
	// Stated as a hard "this is not a measurement" rather than as a hedge,
	// because the phrasing of the strongest warning is what decides whether it
	// survives being skimmed.
	switch {
	case c.Baseline.Simulated && c.Candidate.Simulated:
		out = append(out, "BOTH runs were SIMULATED (--sim), so this table "+
			"compares two fake executors and measures nothing about any "+
			"prompt, model or blueprint")
	case c.Baseline.Simulated:
		out = append(out, "the baseline was SIMULATED (--sim) and the "+
			"candidate was not, so every delta below is the difference "+
			"between a fake executor and a real one — it is not a change in "+
			"quality")
	case c.Candidate.Simulated:
		out = append(out, "the candidate was SIMULATED (--sim) and the "+
			"baseline was not, so every delta below is the difference "+
			"between a real executor and a fake one — it is not a change in "+
			"quality")
	}

	// 1. Different suites. The strongest possible caveat among runs that
	// actually measured something: the delta is a change of question, and
	// reporting it as a change of quality is the one failure that makes an
	// eval tool worse than no eval tool.
	if c.Baseline.SuiteSHA != "" && c.Candidate.SuiteSHA != "" &&
		c.Baseline.SuiteSHA != c.Candidate.SuiteSHA {
		if c.Baseline.Suite != c.Candidate.Suite {
			out = append(out, fmt.Sprintf(
				"these runs used DIFFERENT SUITES (%q and %q), so every delta "+
					"below is a change of question, not a change of quality",
				c.Baseline.Suite, c.Candidate.Suite))
		} else {
			out = append(out, fmt.Sprintf(
				"both runs are named %q but the suite FILE CHANGED between them "+
					"(%s vs %s), so the cases are not the same questions and the "+
					"deltas mix a prompt change with a suite change",
				c.Baseline.Suite, shortSHA(c.Baseline.SuiteSHA),
				shortSHA(c.Candidate.SuiteSHA)))
		}
	}

	// 2. Cases that exist in only one run. A pass rate over a different case
	// set is a different measurement even when the suite name matches, and this
	// catches the partial re-run: `eval run` over 5 of 20 cases compared
	// against the full 20 looks like a quality jump.
	var onlyBase, onlyCand []string
	for _, d := range c.Cases {
		switch {
		case !d.InCandidate:
			onlyBase = append(onlyBase, d.Case)
		case !d.InBaseline:
			onlyCand = append(onlyCand, d.Case)
		}
	}
	sort.Strings(onlyBase)
	sort.Strings(onlyCand)
	if len(onlyBase) > 0 {
		out = append(out, fmt.Sprintf(
			"%d case(s) ran in the baseline and not in the candidate (%s): the "+
				"rates are over different case sets",
			len(onlyBase), strings.Join(onlyBase, ", ")))
	}
	if len(onlyCand) > 0 {
		out = append(out, fmt.Sprintf(
			"%d case(s) ran in the candidate and not in the baseline (%s): the "+
				"rates are over different case sets",
			len(onlyCand), strings.Join(onlyCand, ", ")))
	}

	// 3. Incomplete runs. Every rate is over the judged subset, so a run with
	// errors or skips has rates over a population the reader did not choose.
	for _, r := range []struct {
		label string
		t     Totals
	}{{"baseline", c.BaseTotals}, {"candidate", c.CandTotals}} {
		if r.t.Complete() {
			continue
		}
		var parts []string
		if r.t.Errored > 0 {
			parts = append(parts, fmt.Sprintf("%d errored", r.t.Errored))
		}
		if r.t.Skipped > 0 {
			parts = append(parts, fmt.Sprintf("%d skipped", r.t.Skipped))
		}
		out = append(out, fmt.Sprintf(
			"the %s run judged %d of %d cases (%s); its rates are over that "+
				"subset, and money spent on unjudged cases is in the total but "+
				"not in the mean",
			r.label, r.t.Judged, r.t.Cases, strings.Join(parts, ", ")))
	}

	// 4. Sample size. The limitation the package comment refuses to hide, said
	// where the delta is actually read.
	//
	// The threshold is a confidence interval, not a case count. It WAS a case
	// count — "within two cases' worth" — and that number was chosen by hand,
	// which is the kind of guess this file exists to be suspicious of. It let
	// through §20.11's own worked example: 0.65 → 0.80 over 20 cases, a delta
	// of 0.15 against a 95% band of ±0.27. The documented success case is not
	// distinguishable from noise, and the warning meant to say so said nothing
	// about the one table in this repository a reader is most likely to copy.
	//
	// Agresti-Caffo (add one success and one failure to each arm before the
	// usual two-proportion interval) is used instead of the textbook Wald
	// interval, because Wald's standard error collapses to zero as a rate
	// approaches 0 or 1: 20/20 against 20/20 would report a band of ±0.00 and
	// claim certainty from four samples' worth of evidence. The adjustment is
	// two lines and removes the case where the check is confidently wrong.
	//
	// What this does NOT model is the thing the package comment already admits:
	// with one sample per case, a case that flips is not an independent draw
	// from a stable process. The interval is a floor on the uncertainty, not an
	// estimate of it.
	bp, bok := c.BaseTotals.PassRate()
	cp, cok := c.CandTotals.PassRate()
	if bok && cok {
		delta := math.Abs(cp - bp)
		band := passRateBand(c.BaseTotals, c.CandTotals)
		if delta > 0 && delta <= band {
			out = append(out, fmt.Sprintf(
				"the pass-rate delta is %+.2f but the 95%% band on a "+
					"difference this size is ±%.2f (%d and %d judged cases, "+
					"one sample each), so this delta is within the noise of "+
					"running the SAME prompt twice — it is not evidence of an "+
					"improvement",
				cp-bp, band, c.BaseTotals.Judged, c.CandTotals.Judged))
		}
	}

	// 5. Nothing to compare. Reported rather than shown as 0.00 across the
	// board, which is a table that looks like a result.
	if !bok || !cok {
		out = append(out, "at least one run judged no cases at all, so there "+
			"is no pass rate to compare")
	}

	return out
}

// passRateBand is the half-width of a 95% confidence interval on the difference
// between the two pass rates, by the Agresti-Caffo adjustment.
//
// A function rather than an inline expression so it can be tested against known
// values: a band is a claim about how much evidence there is, and a wrong one
// is worse than none because it is quoted with authority. It is deliberately
// NOT a column in the printed table — a number offered beside a delta invites
// significance theatre, whereas a sentence saying "this is not evidence" does
// not offer itself for arithmetic.
func passRateBand(base, cand Totals) float64 {
	// Add one success and one failure to each arm. This is what keeps the band
	// from collapsing to zero at a 0% or 100% pass rate, where the naive
	// standard error claims certainty from no evidence at all.
	p1 := (float64(base.Passed) + 1) / (float64(base.Judged) + 2)
	p2 := (float64(cand.Passed) + 1) / (float64(cand.Judged) + 2)
	v1 := p1 * (1 - p1) / (float64(base.Judged) + 2)
	v2 := p2 * (1 - p2) / (float64(cand.Judged) + 2)
	return 1.96 * math.Sqrt(v1+v2)
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// Regressions lists the cases that stopped passing, in file order.
//
// Separate from the table because it is the actionable part: a pass rate that
// moved -0.05 is a number, and "case finds-sql-injection now fails because the
// result does not contain \"SQL injection\"" is a thing to go and fix.
func (c *Comparison) Regressions() []CaseDelta {
	var out []CaseDelta
	for _, d := range c.Cases {
		if d.Regressed() {
			out = append(out, d)
		}
	}
	return out
}

// Fixes is the mirror, and it exists so an improvement can be attributed to
// specific cases rather than to a moved average. "+0.15" over 20 cases is three
// cases, and which three is usually the interesting part.
func (c *Comparison) Fixes() []CaseDelta {
	var out []CaseDelta
	for _, d := range c.Cases {
		if d.Fixed() {
			out = append(out, d)
		}
	}
	return out
}

// Validate reports whether this summary is fit to be stored or compared.
//
// It lives in this package and not in a store, for the same reason Decide lives
// in the kernel: what makes a run meaningful is a fact about runs, not a fact
// about files. A store that owned these rules would let a second writer — the
// scheduler, a future `eval import` — persist a summary that passes no checks
// at all.
//
// The checks are the ones whose absence would produce a document that COMPARES
// cleanly and means nothing. That is the bar, and it is why there is no check
// on, say, StartedAt: a run with no timestamp is annoying, while a run with no
// suite digest is a trap.
func (s *RunSummary) Validate() error {
	if s == nil {
		return fmt.Errorf("there is no run to store")
	}
	if strings.TrimSpace(s.ID) == "" {
		return fmt.Errorf("a run has no id: `eval compare` takes two run ids, " +
			"so an unnamed run can be stored and then never be one of them")
	}
	// The id becomes a filename, so the same path-separator rule the triggers
	// needed applies here for the same reason: an id of "../../etc/x" would
	// write outside the run directory.
	if strings.ContainsAny(s.ID, "/\\") || s.ID == "." || s.ID == ".." {
		return fmt.Errorf("run id %q contains a path separator.\n"+
			"  ids become filenames, so this one would write outside the run "+
			"directory", s.ID)
	}
	if strings.TrimSpace(s.Suite) == "" {
		return fmt.Errorf("run %q does not say which suite it ran.\n"+
			"  `compare` warns when two runs measured different suites, and it "+
			"cannot warn about a suite with no name", s.ID)
	}
	// The digest is required, and this is the check with the most argument
	// behind it. SuiteSHA is what makes "these two runs asked different
	// questions" detectable, and warnings() skips that check entirely when
	// either digest is empty — silently, because an empty string is a legal
	// string. So a stored run without a digest does not fail loudly later; it
	// disables the single most important caveat `compare` prints, and the
	// resulting table looks exactly like a valid one.
	if strings.TrimSpace(s.SuiteSHA) == "" {
		return fmt.Errorf("run %q has no suite digest.\n"+
			"  the digest is what lets `compare` notice that two runs measured "+
			"different questions; without it that warning is silently skipped "+
			"and the comparison looks valid", s.ID)
	}
	if len(s.Results) == 0 {
		return fmt.Errorf("run %q has no case results, so there is nothing in "+
			"it to compare.\n  a suite with no cases is refused by the runner; "+
			"a stored run with none would compare as a table of em dashes", s.ID)
	}
	for i, r := range s.Results {
		if strings.TrimSpace(r.Case) == "" {
			return fmt.Errorf("run %q: result %d has no case name.\n"+
				"  `compare` pairs cases BY NAME, so an unnamed case cannot be "+
				"paired and would appear as an addition in one run and a "+
				"removal in the other", s.ID, i+1)
		}
		switch r.Status {
		case StatusPass, StatusFail, StatusError, StatusSkipped:
		default:
			// An unrecognised status is not merely odd. Judged() returns false
			// for anything that is not pass or fail, so a status of "" or
			// "PASS" or "passed" is counted as unjudged: it vanishes from the
			// numerator AND the denominator of the pass rate, and the run
			// reports a confident rate over a subset nobody chose.
			return fmt.Errorf("run %q: case %q has status %q, which is not one "+
				"of pass, fail, error or skipped.\n"+
				"  an unrecognised status counts as unjudged, so it would leave "+
				"the pass rate silently computed over fewer cases",
				s.ID, r.Case, r.Status)
		}
	}
	return nil
}

// Encode renders a run as the bytes a store writes.
//
// Indented, and with a trailing newline, matching trigger.Record.Encode. These
// files are read by humans during exactly the argument they exist to settle —
// "did this prompt change actually help" — and a one-line document is one a
// reader diffs by eye and gets wrong.
func (s *RunSummary) Encode() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode run %q: %w", s.ID, err)
	}
	return append(b, '\n'), nil
}

// DecodeRun parses stored bytes back into a validated summary.
//
// Here and not in the store because encoding and decoding must be the same
// argument in the same place: a field added to RunSummary should not require
// finding a matching change in another package to be read back.
func DecodeRun(raw []byte) (*RunSummary, error) {
	var s RunSummary
	dec := json.NewDecoder(bytes.NewReader(raw))

	// Unknown fields refused, exactly as trigstore refuses them, and the
	// sharpest case here is `pass_rate`. Every rate is DERIVED from Results by
	// Totals(); none is stored. Someone will eventually add `"pass_rate": 0.9`
	// to one of these files by hand, or a future writer will helpfully include
	// the computed value, and a decoder that ignored it would produce a run
	// that reports a different rate than the file plainly states.
	dec.DisallowUnknownFields()

	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("this is not a readable eval run: %w\n"+
			"  these files are written by `arxi eval run`; if this one was "+
			"edited by hand, compare it with another run in the same directory", err)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}
