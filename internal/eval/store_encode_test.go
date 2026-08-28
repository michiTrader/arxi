package eval

import (
	"encoding/json"
	"strings"
	"testing"
)

// ok is a summary that Validate accepts, so each test can break exactly one
// thing. Built by a function rather than a package-level var because a shared
// mutable fixture is how one test's edit becomes another test's mystery.
func ok() *RunSummary {
	return &RunSummary{
		ID:        "e1",
		Suite:     "review-quality",
		SuiteSHA:  "abc123",
		BudgetUSD: 1.00,
		Results: []Result{
			{Case: "one", Status: StatusPass, CostUSD: 0.1, Turns: 1},
			{Case: "two", Status: StatusFail, Why: "missing X", CostUSD: 0.1, Turns: 1},
		},
	}
}

func TestAValidRunEncodesAndDecodesToTheSameThing(t *testing.T) {
	body, err := ok().Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	back, err := DecodeRun(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Compared through the totals rather than field by field, because the
	// totals are what anybody actually reads off a stored run. A round trip
	// that preserves every field but changes the pass rate is not a round trip.
	a, b := ok().Totals(), back.Totals()
	if a != b {
		t.Errorf("the totals changed across a round trip.\n got: %+v\nwant: %+v", b, a)
	}
	if back.ID != "e1" || back.Suite != "review-quality" || back.SuiteSHA != "abc123" {
		t.Errorf("identity fields changed: %+v", back)
	}
}

func TestTheEncodedRunEndsInANewline(t *testing.T) {
	body, err := ok().Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.HasSuffix(string(body), "}\n") {
		t.Errorf("a stored run should end in a newline, so appending to the "+
			"file or catting two of them does not produce one line.\ngot tail: %q",
			string(body[max(0, len(body)-20):]))
	}
	// Indented, because these files are read by eye during exactly the argument
	// they exist to settle.
	if !strings.Contains(string(body), "\n  \"id\": \"e1\"") {
		t.Errorf("a stored run should be indented; got:\n%s", body)
	}
}

func TestARunWithoutASuiteDigestIsRefused(t *testing.T) {
	s := ok()
	s.SuiteSHA = ""
	err := s.Validate()
	if err == nil {
		t.Fatal("a run with no suite digest was accepted.\n" +
			"  warnings() skips the different-suites check SILENTLY when either " +
			"digest is empty, so storing this run disables the most important " +
			"caveat compare prints while the table still looks valid")
	}
	// The message has to say what the digest is FOR. "suite_sha is required" is
	// a schema complaint and teaches nobody why they should care.
	for _, want := range []string{"digest", "different questions"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q, so the reader learns what "+
				"the field protects.\ngot: %v", want, err)
		}
	}
}

func TestARunWithAnUnrecognisedStatusIsRefused(t *testing.T) {
	// "passed" rather than "pass" -- the plausible hand edit, and the one with
	// the quietest consequence.
	s := ok()
	s.Results[0].Status = CaseStatus("passed")

	// First, demonstrate the damage, so this test fails loudly if Judged() ever
	// starts accepting it and the check becomes pointless.
	if s.Totals().Judged != 1 {
		t.Fatalf("expected the bad status to drop out of the judged count; "+
			"totals: %+v", s.Totals())
	}

	err := s.Validate()
	if err == nil {
		t.Fatal(`a status of "passed" was accepted.` + "\n" +
			"  Judged() is false for anything but pass and fail, so this case " +
			"leaves BOTH the numerator and the denominator: the run then " +
			"reports 1.00 over one case instead of 0.50 over two")
	}
	if !strings.Contains(err.Error(), "unjudged") {
		t.Errorf("the refusal should say the case would be counted as "+
			"unjudged, which is the actual harm.\ngot: %v", err)
	}
}

func TestARunWithAnUnnamedCaseIsRefused(t *testing.T) {
	s := ok()
	s.Results[1].Case = "  "
	err := s.Validate()
	if err == nil {
		t.Fatal("a result with a blank case name was accepted.\n" +
			"  compare pairs cases BY NAME, so this one appears as an addition " +
			"in one run and a removal in the other")
	}
	if !strings.Contains(err.Error(), "BY NAME") {
		t.Errorf("the refusal should explain that pairing is by name.\ngot: %v", err)
	}
}

func TestARunWithNoResultsIsRefused(t *testing.T) {
	s := ok()
	s.Results = nil
	if err := s.Validate(); err == nil {
		t.Fatal("a run with no results was accepted, and it would compare as a " +
			"table of em dashes that looks like a comparison")
	}
}

func TestARunIDCannotEscapeTheRunDirectory(t *testing.T) {
	for _, id := range []string{"../../etc/passwd", "a/b", `a\b`, ".", ".."} {
		s := ok()
		s.ID = id
		if err := s.Validate(); err == nil {
			t.Errorf("id %q was accepted; ids become filenames, so this one "+
				"writes outside the run directory", id)
		}
	}
}

func TestARunWithNoIDIsRefusedBecauseCompareTakesIDs(t *testing.T) {
	s := ok()
	s.ID = ""
	if err := s.Validate(); err == nil {
		t.Fatal("a run with no id was accepted; `eval compare` takes two ids, " +
			"so this run could be stored and never be one of them")
	}
}

func TestANilRunIsRefusedRatherThanPanicking(t *testing.T) {
	var s *RunSummary
	if err := s.Validate(); err == nil {
		t.Fatal("Validate on a nil run should refuse, not succeed")
	}
}

func TestAHandAddedPassRateIsRefusedNotIgnored(t *testing.T) {
	// The specific field somebody will add, because it is the one they want to
	// read. Every rate is DERIVED from Results; none is stored.
	raw := `{"id":"e1","suite":"s","suite_sha":"abc","budget_usd":1,` +
		`"simulated":false,"pass_rate":0.9,` +
		`"results":[{"case":"one","status":"pass","cost_usd":0.1,"turns":1}]}`
	_, err := DecodeRun([]byte(raw))
	if err == nil {
		t.Fatal(`a file carrying "pass_rate": 0.9 was accepted.` + "\n" +
			"  the rate is computed from results, so ignoring the field yields " +
			"a run that reports a different number than the file plainly states")
	}
	if !strings.Contains(err.Error(), "pass_rate") {
		t.Errorf("the refusal should name the offending field so it can be "+
			"deleted.\ngot: %v", err)
	}
}

func TestATruncatedRunFileIsRefusedWithAHintAboutHandEditing(t *testing.T) {
	body, _ := ok().Encode()
	_, err := DecodeRun(body[:len(body)/2])
	if err == nil {
		t.Fatal("half a run file was accepted")
	}
	if !strings.Contains(err.Error(), "edited by hand") {
		t.Errorf("the refusal should suggest comparing against another run, "+
			"since a hand edit is the likely cause.\ngot: %v", err)
	}
}

func TestSimulatedIsAlwaysPresentInTheEncodedRun(t *testing.T) {
	// The whole point of leaving omitempty off. A reader deciding whether to
	// quote a number must not have to know that a missing field means real.
	body, err := ok().Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	v, present := m["simulated"]
	if !present {
		t.Fatal(`"simulated" is absent from a real run's document.` + "\n" +
			"  absence and false then look identical, and the field exists " +
			"precisely so a reader can tell fabricated numbers from measured " +
			"ones without knowing this file format's defaults")
	}
	if v != false {
		t.Errorf(`want simulated=false on a real run, got %v`, v)
	}
}

func TestASimulatedRunSaysSoWhenEncoded(t *testing.T) {
	s := ok()
	s.Simulated = true
	body, err := s.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	back, err := DecodeRun(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !back.Simulated {
		t.Error("a simulated run decoded as a real one, which is how fake " +
			"numbers get compared against measured ones")
	}
}

func TestComparingASimulatedRunAgainstARealOneIsWarnedAboutFirst(t *testing.T) {
	base, cand := ok(), ok()
	base.Simulated = true
	// A suite mismatch too, so the ORDERING is what is under test and not just
	// the presence of the warning.
	cand.Suite, cand.SuiteSHA = "other", "def456"

	cmp := Compare(base, cand)
	if len(cmp.Warnings) < 2 {
		t.Fatalf("expected both a simulation warning and a suite warning, got %v",
			cmp.Warnings)
	}
	if !strings.Contains(cmp.Warnings[0], "SIMULATED") {
		t.Errorf("the simulation warning must come FIRST, ahead of the suite "+
			"mismatch: comparing two different suites still compares two "+
			"measurements, so a reader who ignores that caveat learns "+
			"something real, whereas no reading of a simulated table is "+
			"partially correct.\ngot first: %q\nall: %v",
			cmp.Warnings[0], cmp.Warnings)
	}
	if !strings.Contains(cmp.Warnings[0], "baseline") {
		t.Errorf("the warning should say WHICH run was simulated, since the "+
			"fix differs.\ngot: %q", cmp.Warnings[0])
	}
}

func TestTwoSimulatedRunsAreReportedAsMeasuringNothing(t *testing.T) {
	base, cand := ok(), ok()
	base.Simulated, cand.Simulated = true, true
	cmp := Compare(base, cand)
	if len(cmp.Warnings) == 0 || !strings.Contains(cmp.Warnings[0], "BOTH") {
		t.Fatalf("two simulated runs should be reported as comparing two fake "+
			"executors, not as one run being odd.\ngot: %v", cmp.Warnings)
	}
	if !strings.Contains(cmp.Warnings[0], "measures nothing") {
		t.Errorf("the warning should say plainly that nothing was measured, "+
			"rather than hedging.\ngot: %q", cmp.Warnings[0])
	}
}

func TestTwoRealRunsAreNotWarnedAboutSimulation(t *testing.T) {
	// The guard against a warning that always fires, which is a warning nobody
	// reads.
	cmp := Compare(ok(), ok())
	for _, w := range cmp.Warnings {
		if strings.Contains(w, "SIMULATED") {
			t.Errorf("two real runs were warned about simulation: %q", w)
		}
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// TestOnlyTheCandidateBeingSimulatedIsAlsoWarnedAbout closes a gap that
// mutation testing found: replacing the `case c.Candidate.Simulated:` arm with
// `case false:` did not fail a single test. Two of the three arms were covered
// and the third was decoration.
//
// It is the arm that matters most in practice, too. The natural mistake is to
// have a real baseline from last week and then run the candidate with --sim
// while iterating -- at which point the table shows the simulator beating the
// real system, which reads as a large improvement.
func TestOnlyTheCandidateBeingSimulatedIsAlsoWarnedAbout(t *testing.T) {
	base, cand := ok(), ok()
	cand.Simulated = true

	cmp := Compare(base, cand)
	if len(cmp.Warnings) == 0 {
		t.Fatal("a real baseline against a simulated candidate produced no warning")
	}
	if !strings.Contains(cmp.Warnings[0], "SIMULATED") {
		t.Fatalf("want a simulation warning first, got %q", cmp.Warnings[0])
	}
	// Which run, because the fix differs: a simulated candidate means rerun
	// without --sim, a simulated baseline means the baseline is worthless.
	if !strings.Contains(cmp.Warnings[0], "candidate") {
		t.Errorf("the warning must name the candidate as the simulated run, "+
			"since that is the one to rerun.\ngot: %q", cmp.Warnings[0])
	}
}

// TestFixedTrustsPresenceOverStatusWhenTheTwoDisagree is the test that makes
// the InBaseline guard on Fixed() load-bearing rather than decorative.
//
// Mutation testing removed that guard and nothing failed, because every
// realistic absent case also has an empty BaseStatus, so the status check
// alone happened to be sufficient. That is an argument that the guard is
// currently redundant -- not that it is wrong. This test pins the intended
// precedence directly: when presence and status disagree, presence wins,
// because presence is a fact about the suite and status is a fact about a run
// that did not happen.
func TestFixedTrustsPresenceOverStatusWhenTheTwoDisagree(t *testing.T) {
	d := CaseDelta{Case: "ghost", InBaseline: false, InCandidate: true,
		BaseStatus: StatusFail, CandStatus: StatusPass}
	if d.Fixed() {
		t.Error("a case absent from the baseline was called fixed on the " +
			"strength of a baseline status it cannot have.\n" +
			"  presence must win: a case that was not in the baseline did not " +
			"fail there, whatever the status field says")
	}
}

// TestRegressedTrustsPresenceOverStatusWhenTheTwoDisagree is the mirror, and
// the one whose absence caused a real bug: a deleted case printed as
// `finds-xss  pass → ` and inflated the regression count.
func TestRegressedTrustsPresenceOverStatusWhenTheTwoDisagree(t *testing.T) {
	d := CaseDelta{Case: "ghost", InBaseline: true, InCandidate: false,
		BaseStatus: StatusPass, CandStatus: StatusFail}
	if d.Regressed() {
		t.Error("a case absent from the candidate was called a regression on " +
			"the strength of a candidate status it cannot have")
	}
}
