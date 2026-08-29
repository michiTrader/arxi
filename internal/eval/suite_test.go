package eval

import (
	"strings"
	"testing"
)

// The suite loader.
//
// Most of these tests are about a suite that PARSES and means something other
// than what it says, because that is the failure mode worth spending tests on:
// a suite that does not parse costs a minute, and a suite that passes vacuously
// costs a decision made on a number that measured nothing.

// good is a suite that should load without complaint. Cases are built from it
// so a test about one defect is not also asserting the shape of everything else.
const good = `
name: review-quality
blueprint: ./teams/review.yaml
cases:
  - name: finds-sql-injection
    objective: review the login handler
    expect:
      contains:
        - SQL injection
  - name: no-false-positives
    objective: review the formatter
    expect:
      not_contains:
        - vulnerability
`

func mustLoad(t *testing.T, src string) *Suite {
	t.Helper()
	s, err := Load([]byte(src))
	if err != nil {
		t.Fatalf("Load: %v\nsource:%s", err, src)
	}
	return s
}

func mustFail(t *testing.T, src string) *ValidationError {
	t.Helper()
	_, err := Load([]byte(src))
	if err == nil {
		t.Fatalf("this loaded and should not have:%s", src)
	}
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("error is %T, want *ValidationError: %v", err, err)
	}
	return ve
}

// problem reports whether any problem mentions every one of the fragments.
func problem(ve *ValidationError, fragments ...string) bool {
	for _, p := range ve.Problems {
		all := true
		for _, f := range fragments {
			if !strings.Contains(p, f) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

func TestADocumentedSuiteLoads(t *testing.T) {
	s := mustLoad(t, good)
	if s.Name != "review-quality" {
		t.Errorf("name = %q", s.Name)
	}
	if len(s.Cases) != 2 {
		t.Fatalf("got %d cases, want 2", len(s.Cases))
	}
	if s.Cases[0].Name != "finds-sql-injection" {
		t.Errorf("cases[0].name = %q", s.Cases[0].Name)
	}
	if s.Cases[0].Objective != "review the login handler" {
		t.Errorf("cases[0].objective = %q", s.Cases[0].Objective)
	}
	if s.SHA == "" {
		t.Error("no SHA: compare cannot tell a prompt change from a suite change without it")
	}
}

// TestTheSuiteBlueprintIsResolvedOntoEveryCase is the reason the default is
// applied at load time.
//
// A Case whose Blueprint is empty must be impossible downstream, so nothing
// that reads a Case has to know a suite-level default ever existed. The
// alternative puts the same fallback in the runner, the reporter and anything
// else that touches a case, and one of them will forget.
func TestTheSuiteBlueprintIsResolvedOntoEveryCase(t *testing.T) {
	s := mustLoad(t, good)
	for i, c := range s.Cases {
		if c.Blueprint != "./teams/review.yaml" {
			t.Errorf("cases[%d].blueprint = %q, want the suite default", i, c.Blueprint)
		}
	}

	// And a case may override it, which is the shape that makes a suite useful
	// for comparing two team designs on one question.
	s = mustLoad(t, `
name: two-designs
blueprint: ./a.yaml
cases:
  - name: default-team
    objective: fix the bug
    expect: {contains: done}
  - name: other-team
    blueprint: ./b.yaml
    objective: fix the bug
    expect: {contains: done}
`)
	if s.Cases[0].Blueprint != "./a.yaml" {
		t.Errorf("cases[0] = %q, want the default", s.Cases[0].Blueprint)
	}
	if s.Cases[1].Blueprint != "./b.yaml" {
		t.Errorf("cases[1] = %q, want the override", s.Cases[1].Blueprint)
	}
}

// TestACaseWithNoExpectationIsRefused is the most important test in this file.
//
// An empty `expect` passes on every possible output — including an error
// message — so it is a guaranteed +1 to the pass rate that measures nothing. A
// suite of them reports 1.00 and reads as a clean run, which is the one result
// that gets acted on without being questioned.
func TestACaseWithNoExpectationIsRefused(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"expect omitted", `
name: s
blueprint: ./b.yaml
cases:
  - name: c
    objective: o
`},
		{"expect present but empty", `
name: s
blueprint: ./b.yaml
cases:
  - name: c
    objective: o
    expect: {}
`},
		{"conditions all absent", `
name: s
blueprint: ./b.yaml
cases:
  - name: c
    objective: o
    expect:
      contains: []
`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ve := mustFail(t, tc.src)
			// Asserting on the REASON, not merely on failure. The three inputs
			// above are refused by more than one rule apiece, so a test that
			// only checked "did it fail" passes with the expect check deleted
			// entirely — which is exactly what a mutation proved: removing the
			// "expect is required" branch left the suite green, because the
			// "no conditions" branch caught the same file one step later.
			//
			// A guard that is only load-bearing when another guard is absent is
			// still load-bearing; `expect: {contains: []}` reaches one branch
			// and an omitted `expect` reaches the other, and the messages point
			// at different fixes.
			if !problem(ve, "passes unconditionally") &&
				!problem(ve, "satisfied by every output") {
				t.Errorf("the problem does not say WHY an empty expectation is "+
					"refused, so the test would pass with the check removed: %v",
					ve.Problems)
			}
		})
	}

	// And the two branches are distinguishable, which is the point of having
	// both: one tells the author to write an expect, the other tells them the
	// one they wrote is empty.
	omitted := mustFail(t, "name: s\nblueprint: ./b.yaml\ncases:\n  - name: c\n    objective: o\n")
	if !problem(omitted, "expect is required") {
		t.Errorf("an omitted expect should say it is required: %v", omitted.Problems)
	}
	empty := mustFail(t, `
name: s
blueprint: ./b.yaml
cases:
  - name: c
    objective: o
    expect:
      contains: []
`)
	if !problem(empty, "no conditions") {
		t.Errorf("an empty expect should say it has no conditions: %v", empty.Problems)
	}
}

// TestAMissingCasesKeyIsDistinguishedFromAnEmptyList closes the same kind of
// gap one level up.
//
// `cases:` absent and `cases: []` are different mistakes — one is an unfinished
// file, the other is a file that says "measure nothing" — and a mutation
// removing the absent-key branch survived because the empty-list branch caught
// the same input afterwards. Both messages exist; only one was tested.
func TestAMissingCasesKeyIsDistinguishedFromAnEmptyList(t *testing.T) {
	absent := mustFail(t, "name: s\nblueprint: ./b.yaml\n")
	if !problem(absent, "cases is required") {
		t.Errorf("an absent cases key should say it is required: %v", absent.Problems)
	}

	empty := mustFail(t, "name: s\nblueprint: ./b.yaml\ncases: []\n")
	if !problem(empty, "the list is empty") {
		t.Errorf("an empty cases list should say so: %v", empty.Problems)
	}

	// Both explain the consequence rather than only naming the key, because
	// "cases is required" invites adding `cases: []` to satisfy it.
	for _, ve := range []*ValidationError{absent, empty} {
		if !problem(ve, "pass rate") && !problem(ve, "passes vacuously") {
			t.Errorf("the message does not say what an empty suite reports: %v",
				ve.Problems)
		}
	}
}

// TestAnEmptyStringConditionIsRefused covers the vacuous condition one level
// down from an empty expect.
//
// Every string contains "", so `contains: [""]` always holds and
// `not_contains: [""]` never does. Both are easy to produce with a trailing
// `- ` in a list, and neither announces itself: one inflates the pass rate and
// the other deflates it by a fixed amount that looks like a real regression.
func TestAnEmptyStringConditionIsRefused(t *testing.T) {
	t.Run("contains", func(t *testing.T) {
		ve := mustFail(t, `
name: s
blueprint: ./b.yaml
cases:
  - name: c
    objective: o
    expect:
      contains: ["", ok]
`)
		if !problem(ve, "always holds") {
			t.Errorf("problems: %v", ve.Problems)
		}
	})
	t.Run("not_contains", func(t *testing.T) {
		ve := mustFail(t, `
name: s
blueprint: ./b.yaml
cases:
  - name: c
    objective: o
    expect:
      not_contains: [""]
`)
		if !problem(ve, "no output can satisfy") {
			t.Errorf("problems: %v", ve.Problems)
		}
	})
}

// TestAContradictoryExpectationIsRefused catches a case that fails for a reason
// in the suite rather than in the system under test.
//
// It fails on every run, so it subtracts a fixed amount from the pass rate that
// reads as a persistent quality problem — and looking for the cause in the
// prompt is looking in the wrong file.
func TestAContradictoryExpectationIsRefused(t *testing.T) {
	ve := mustFail(t, `
name: s
blueprint: ./b.yaml
cases:
  - name: c
    objective: o
    expect:
      contains: [findings]
      not_contains: [findings]
`)
	if !problem(ve, "both contains and not_contains") {
		t.Errorf("problems: %v", ve.Problems)
	}
}

// TestDuplicateCaseNamesAreRefused protects the pairing that `compare` does.
//
// Comparison pairs cases by name. Two cases sharing one makes the pairing pick
// whichever came first in each run, so a regression in the second is reported
// as no change — and the suite appears to cover one more case than it measures.
func TestDuplicateCaseNamesAreRefused(t *testing.T) {
	ve := mustFail(t, `
name: s
blueprint: ./b.yaml
cases:
  - name: same
    objective: o
    expect: {contains: ok}
  - name: same
    objective: p
    expect: {contains: ok}
`)
	if !problem(ve, "duplicate case") {
		t.Errorf("problems: %v", ve.Problems)
	}
	if !problem(ve, "pairs cases by name") {
		t.Errorf("the message does not say why it matters: %v", ve.Problems)
	}
}

// TestAnEmptySuiteIsRefused covers the suite that passes vacuously as a whole.
//
// Zero cases completes instantly and reports a pass rate over nothing. Whether
// that is 1.00 or 0.00 or NaN is an implementation detail, and all three are
// wrong answers to "did the change help".
func TestAnEmptySuiteIsRefused(t *testing.T) {
	for _, src := range []string{
		"name: s\nblueprint: ./b.yaml\n",
		"name: s\nblueprint: ./b.yaml\ncases: []\n",
	} {
		ve := mustFail(t, src)
		if !problem(ve, "cases") {
			t.Errorf("problems do not mention cases: %v", ve.Problems)
		}
	}
}

// TestAMissingBlueprintIsRefusedAtLoadTime keeps the failure at the cheap end.
//
// A case with no blueprint cannot run, and discovering that after nineteen
// other cases have spent real money is the expensive way to learn it.
func TestAMissingBlueprintIsRefusedAtLoadTime(t *testing.T) {
	ve := mustFail(t, `
name: s
cases:
  - name: c
    objective: o
    expect: {contains: ok}
`)
	if !problem(ve, "no blueprint") {
		t.Errorf("problems: %v", ve.Problems)
	}
}

// TestAMisspelledKeyIsCorrectedRatherThanDropped is the whole reason the loader
// works over map[string]any instead of decoding into a struct.
//
// A dropped `not_contains` does not fail: the case runs, the condition is
// simply absent, and the case passes on output the author meant to forbid. The
// suite on screen says something that never took effect.
func TestAMisspelledKeyIsCorrectedRatherThanDropped(t *testing.T) {
	cases := []struct {
		name, src, typo, want string
	}{
		{"a case key", `
name: s
blueprint: ./b.yaml
cases:
  - name: c
    objectiv: o
    expect: {contains: ok}
`, "objectiv", "objective"},
		{"an expect key", `
name: s
blueprint: ./b.yaml
cases:
  - name: c
    objective: o
    expect: {contain: ok}
`, "contain", "contains"},
		{"a suite key", `
name: s
bluprint: ./b.yaml
cases:
  - name: c
    objective: o
    expect: {contains: ok}
`, "bluprint", "blueprint"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ve := mustFail(t, tc.src)
			// "did you mean" is part of the assertion, and it is not
			// decoration. Without it this test passes on the FALLBACK message
			// — `unknown key "objectiv" (valid: name, blueprint, objective,
			// expect)` contains both the typo and the intended key, because the
			// intended key is in the valid list. A mutation disabling the
			// suggestion entirely survived on exactly that coincidence.
			//
			// The two messages are not interchangeable: one hands over the fix,
			// the other hands over a list to search. For a one-character typo
			// the difference is the whole value of the check.
			if !problem(ve, tc.typo, tc.want, "did you mean") {
				t.Errorf("a misspelled %q was not corrected to %q with a "+
					"suggestion: %v", tc.typo, tc.want, ve.Problems)
			}
		})
	}
}

// TestAKeyThatIsNotAMisspellingListsTheValidOnes covers the other half of
// nearest.
//
// Suggesting a correction for a word that is not a misspelling of anything
// sends the reader to fix the wrong thing, so an unrecognisable key gets the
// list instead of a guess.
func TestAKeyThatIsNotAMisspellingListsTheValidOnes(t *testing.T) {
	ve := mustFail(t, `
name: s
blueprint: ./b.yaml
retries: 4
cases:
  - name: c
    objective: o
    expect: {contains: ok}
`)
	if !problem(ve, "retries", "valid:") {
		t.Errorf("an unrecognisable key should be answered with the valid set: %v",
			ve.Problems)
	}
	if problem(ve, "did you mean") {
		t.Errorf("%q was corrected to something: %v", "retries", ve.Problems)
	}
}

// TestEveryProblemIsReportedAtOnce is the same argument blueprint makes: a
// validator that reports one problem per run is a validator people stop running.
func TestEveryProblemIsReportedAtOnce(t *testing.T) {
	ve := mustFail(t, `
name: s
cases:
  - name: c
    expect: {}
  - name: c
    objective: o
    expect:
      contains: [x]
      not_contains: [x]
`)
	// missing objective, missing blueprint x2, empty expect, duplicate name,
	// contradiction: at least five distinct complaints.
	if len(ve.Problems) < 5 {
		t.Errorf("got %d problems, want at least 5:\n%s", len(ve.Problems),
			strings.Join(ve.Problems, "\n"))
	}
	// And the report names all of them, not just a count.
	msg := ve.Error()
	for _, want := range []string{"objective", "blueprint", "expect", "duplicate"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Error() does not mention %q:\n%s", want, msg)
		}
	}
}

// TestASingleConditionNeedsNoListSyntax accepts what everybody actually writes.
//
// `contains: "SQL injection"` for one condition and the list form for several.
// Refusing the scalar would be a rule with no reason behind it, and the two
// shapes are unambiguous.
func TestASingleConditionNeedsNoListSyntax(t *testing.T) {
	s := mustLoad(t, `
name: s
blueprint: ./b.yaml
cases:
  - name: c
    objective: o
    expect:
      contains: SQL injection
`)
	got := s.Cases[0].Expect.Contains
	if len(got) != 1 || got[0] != "SQL injection" {
		t.Errorf("contains = %#v, want one condition", got)
	}
}

// TestJudgeExplainsTheFailureRatherThanReportingIt is why Judge returns a
// reason.
//
// "case 7 failed" sends somebody to a transcript. "case 7: result does not
// contain \"SQL injection\"" is usually the whole answer, and the transcript
// stays unopened.
func TestJudgeExplainsTheFailureRatherThanReportingIt(t *testing.T) {
	e := Expectation{Contains: []string{"SQL injection"}, NotContains: []string{"TODO"}}

	if ok, why := e.Judge("found a SQL injection in the login handler"); !ok {
		t.Errorf("a satisfying result was judged a failure: %s", why)
	}

	ok, why := e.Judge("looks fine to me")
	if ok {
		t.Fatal("a result missing the required text passed")
	}
	if !strings.Contains(why, "SQL injection") {
		t.Errorf("the reason does not name the missing condition: %q", why)
	}

	ok, why = e.Judge("found a SQL injection; TODO check the rest")
	if ok {
		t.Fatal("a result containing forbidden text passed")
	}
	if !strings.Contains(why, "TODO") {
		t.Errorf("the reason does not name the forbidden text: %q", why)
	}
}

// TestJudgeIsCaseSensitive states the decision rather than leaving it to be
// discovered.
//
// An eval that quietly lowercased everything would pass a case asserting
// `contains: "SQL injection"` against output saying "sql injection". That is
// convenient until the assertion is about an identifier, and then it is a case
// that cannot be written.
func TestJudgeIsCaseSensitive(t *testing.T) {
	e := Expectation{Contains: []string{"SQL injection"}}
	if ok, _ := e.Judge("found a sql injection"); ok {
		t.Error("matching is case-insensitive; a case asserting on an " +
			"identifier could not be written")
	}
}

// TestAnEmptyEqualsIsAnAssertionAndAnAbsentOneIsNot is why hasEquals exists.
//
// `equals: ""` asserts "the result is empty" — unusual but legitimate, as in "this
// stage must produce no findings". An omitted `equals` asserts nothing. The zero
// value cannot tell them apart, and collapsing them makes the first case pass on
// every possible output.
func TestAnEmptyEqualsIsAnAssertionAndAnAbsentOneIsNot(t *testing.T) {
	s := mustLoad(t, `
name: s
blueprint: ./b.yaml
cases:
  - name: must-be-silent
    objective: o
    expect:
      equals: ""
`)
	e := s.Cases[0].Expect
	if !e.HasEquals() {
		t.Fatal("`equals: \"\"` was read as no assertion at all")
	}
	if ok, _ := e.Judge(""); !ok {
		t.Error("an empty result should satisfy `equals: \"\"`")
	}
	if ok, why := e.Judge("something"); ok {
		t.Errorf("a non-empty result satisfied `equals: \"\"` (%s)", why)
	}

	// And with equals absent, a non-empty result is not judged against it.
	plain := mustLoad(t, good).Cases[0].Expect
	if plain.HasEquals() {
		t.Error("an absent equals was read as an assertion")
	}
}

// TestTheSHAChangesWithTheFileAndNotWithFormatting is what makes `compare` able
// to say "these two runs asked different questions".
func TestTheSHAChangesWithTheFileAndNotWithFormatting(t *testing.T) {
	a := mustLoad(t, good)
	b := mustLoad(t, good)
	if a.SHA != b.SHA {
		t.Error("the same bytes produced two digests")
	}

	changed := strings.Replace(good, "SQL injection", "XSS", 1)
	c := mustLoad(t, changed)
	if c.SHA == a.SHA {
		t.Error("changing a condition did not change the digest, so a " +
			"comparison cannot notice the questions changed")
	}
}

// TestTheRawBytesAreCopiedAndNotAliased guards the snapshot.
//
// Raw is what a stored eval run would keep, for the same reason a run keeps the
// blueprint it used. Aliasing the caller's slice means whoever holds the
// original can rewrite the record of what was measured.
func TestTheRawBytesAreCopiedAndNotAliased(t *testing.T) {
	src := []byte(good)
	s, err := Load(src)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	src[0] = 'X'
	if s.Raw[0] == 'X' {
		t.Error("Raw aliases the caller's slice: the snapshot of what was " +
			"measured can be edited after the fact")
	}
}

// TestCaseNamesAreInFileOrder keeps the report matching the file.
//
// File order and not sorted: it is the order the author chose and reads
// alongside, and an alphabetised progress report stops lining up with the
// suite on screen.
func TestCaseNamesAreInFileOrder(t *testing.T) {
	s := mustLoad(t, `
name: s
blueprint: ./b.yaml
cases:
  - name: zulu
    objective: o
    expect: {contains: ok}
  - name: alpha
    objective: o
    expect: {contains: ok}
`)
	got := s.CaseNames()
	want := []string{"zulu", "alpha"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("CaseNames = %v, want %v (file order, not sorted)", got, want)
		}
	}
}

// TestASuiteWithNoNameIsRefused: the name labels every comparison the suite
// produces, and an unnamed one makes every report ambiguous about what was
// measured.
func TestASuiteWithNoNameIsRefused(t *testing.T) {
	ve := mustFail(t, `
blueprint: ./b.yaml
cases:
  - name: c
    objective: o
    expect: {contains: ok}
`)
	if !problem(ve, "name is required") {
		t.Errorf("problems: %v", ve.Problems)
	}
}

// TestANonMappingSuiteIsRefusedWithTheShape tells the author what to write.
//
// "must be a mapping" alone is a restatement of the failure; naming the keys is
// the fix.
func TestANonMappingSuiteIsRefusedWithTheShape(t *testing.T) {
	ve := mustFail(t, "- just\n- a list\n")
	if !problem(ve, "cases:") {
		t.Errorf("the message does not say what a suite looks like: %v", ve.Problems)
	}
}

// TestWrongTypesAreReportedRatherThanCoerced.
//
// A number where text belongs is a mistake, and coercing it invents a value the
// file does not contain — `objective: 5` becoming "5" is a case that runs the
// prompt "5".
func TestWrongTypesAreReportedRatherThanCoerced(t *testing.T) {
	ve := mustFail(t, `
name: s
blueprint: ./b.yaml
cases:
  - name: c
    objective: 5
    expect: {contains: ok}
`)
	if !problem(ve, "objective", "must be text") {
		t.Errorf("problems: %v", ve.Problems)
	}
}

// TestEveryCaseProblemNamesTheCaseThatCausedIt.
//
// The index is what makes a problem findable in a twenty-case file, and
// "objective is required" with no location is a message that sends the reader
// scanning. A mutation that replaced the index with a constant survived,
// because the only test asserting on `cases[N]` used the not-a-mapping path —
// a different branch from the one that reports every other problem.
//
// So the defect is placed at index 2 here: an assertion that accepts
// `cases[0]` would pass on a hardcoded zero, which is the same blind spot one
// step along.
func TestEveryCaseProblemNamesTheCaseThatCausedIt(t *testing.T) {
	ve := mustFail(t, `
name: s
blueprint: ./b.yaml
cases:
  - name: fine
    objective: o
    expect: {contains: ok}
  - name: also-fine
    objective: o
    expect: {contains: ok}
  - name: broken
    expect: {contains: ok}
`)
	if !problem(ve, "cases[2]", "objective") {
		t.Errorf("the problem does not locate the offending case at its own "+
			"index: %v", ve.Problems)
	}

	// The not-a-mapping path reports an index too, and it is a separate branch.
	ve = mustFail(t, `
name: s
blueprint: ./b.yaml
cases:
  - name: fine
    objective: o
    expect: {contains: ok}
  - just a string
`)
	if !problem(ve, "cases[1]") {
		t.Errorf("a case that is not a mapping is not located: %v", ve.Problems)
	}
}
