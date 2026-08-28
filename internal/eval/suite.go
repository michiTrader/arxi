// Package eval loads evaluation suites and compares evaluation runs.
//
// §20.11's premise: "prompt changes get judged by anecdote unless something
// measures them". This package is the measuring part, and it is separated from
// the running part for the same reason internal/trigger is separated from
// internal/trigstore — a suite that is wrong is wrong before anything executes,
// and 20 runs is an expensive place to discover a typo.
//
// # What eval gets for free, and what it does not
//
// The registry comment says "eval comes for free from the pure reducer: it is
// the same fold over fixed cases", and that is true of the EXECUTION. It is not
// true of the measurement, which is where the mistakes live:
//
//   - A pass rate computed over a suite that did not finish is a number with no
//     meaning, and there are two ways to get it wrong in opposite directions.
//   - A comparison between two runs of DIFFERENT case sets reads as a quality
//     delta when it is a change of question.
//
// Both are handled here, and both are the reason this is a package and not a
// pair of helpers in cmd/iash.
//
// # The limitation this package does not hide
//
// One sample per case measures the sampler as much as the prompt. Model output
// varies between identical invocations, so a case that passes at 0.6
// probability contributes a coin flip to the pass rate, and a 20-case suite
// with no repetition can move ±0.15 between runs of the SAME prompt. That is
// the size of the improvement §20.11's example celebrates.
//
// There is no `repeat:` key yet, and adding an unused one would be worse than
// its absence — a field the runner ignores is a promise the file appears to
// make. What exists instead is this paragraph, and a `compare` that refuses to
// report a delta it cannot attribute. Whoever builds the runner should read
// this first.
package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/michiTrader/iash/internal/blueprint"
)

// Suite is a validated evaluation suite.
//
// SHA pins the file the cases came from, and it is not decoration: `eval
// compare` uses it to tell "the prompt improved" from "the questions changed".
// Two runs of different suites can be compared case by case only where the
// cases agree, and without a digest there is nothing to notice the disagreement
// with.
type Suite struct {
	Name  string
	Raw   []byte
	SHA   string
	Cases []Case
}

// Case is one question the suite asks.
//
// Blueprint is resolved per case rather than looked up at run time: a suite
// whose cases each name their own blueprint is the useful shape (the same
// objective against two team designs), and a suite-level default is a
// convenience on top of it, applied here so nothing downstream has to know the
// default existed.
type Case struct {
	Name      string
	Blueprint string
	Objective string
	Expect    Expectation
}

// Expectation is how a case is judged.
//
// Every listed condition must hold. There is no `any_of`, because a case that
// passes on either of two unrelated outputs is two cases, and merging them
// hides which one actually happened — the pass rate goes up and the reason
// disappears.
//
// Matching is CASE-SENSITIVE and substring-based, stated because the
// alternative is a silent one: an eval that quietly lowercased everything would
// pass a case asserting `contains: "SQL injection"` on output saying "sql
// injection", and the day that distinction matters is the day somebody is
// asserting on an identifier.
type Expectation struct {
	Contains    []string
	NotContains []string
	Equals      string
	hasEquals   bool // `equals: ""` is a real assertion; absence is not
}

// HasEquals reports whether `equals` was written at all.
//
// Needed because `equals: ""` asserts "the result is empty" and an omitted
// `equals` asserts nothing, and the zero value cannot tell them apart. A case
// asserting emptiness is unusual but legitimate — "this stage must produce no
// findings" — and silently treating it as no assertion would make the case pass
// on every possible output.
func (e Expectation) HasEquals() bool { return e.hasEquals }

// Judge decides whether one result satisfies the expectation, and says why not.
//
// The reason is returned rather than logged because it is the thing a failing
// eval is read for. "case 7 failed" sends somebody to the transcript; "case 7:
// result does not contain \"SQL injection\"" is usually the whole answer.
func (e Expectation) Judge(result string) (ok bool, why string) {
	if e.hasEquals && result != e.Equals {
		return false, fmt.Sprintf("result is not exactly %q", e.Equals)
	}
	for _, want := range e.Contains {
		if !strings.Contains(result, want) {
			return false, fmt.Sprintf("result does not contain %q", want)
		}
	}
	for _, unwanted := range e.NotContains {
		if strings.Contains(result, unwanted) {
			return false, fmt.Sprintf("result contains %q, which it must not", unwanted)
		}
	}
	return true, ""
}

// ValidationError collects every problem in the suite at once.
//
// Same reasoning as blueprint's, and the same type shape rather than a shared
// one: a suite problem and a blueprint problem are reported by different
// commands about different files, and the day one grows a field the other does
// not need is the day a shared type starts carrying it anyway.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string {
	if len(e.Problems) == 1 {
		return e.Problems[0]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d problems in the suite:", len(e.Problems))
	for _, p := range e.Problems {
		b.WriteString("\n  - ")
		b.WriteString(p)
	}
	return b.String()
}

// LoadFile reads and validates a suite.
func LoadFile(path string) (*Suite, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, err := Load(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Load parses and validates a suite held in memory.
//
// It uses blueprint.Parse, and reusing that parser is the point: a second YAML
// reader in this repository would be a second set of rules about indentation,
// comments and quoting, and the file that parses in one and not the other is
// the bug nobody can reproduce. The suite and the blueprint it names are edited
// by the same person in the same afternoon.
func Load(raw []byte) (*Suite, error) {
	doc, err := blueprint.Parse(raw)
	if err != nil {
		return nil, err
	}
	root, ok := doc.(map[string]any)
	if !ok {
		return nil, &ValidationError{Problems: []string{
			"a suite is a mapping of keys at the top level (name:, blueprint:, cases:)"}}
	}

	v := &validator{}
	name, cases := v.suite(root)
	if len(v.problems) > 0 {
		sort.Strings(v.problems)
		return nil, &ValidationError{Problems: v.problems}
	}

	sum := sha256.Sum256(raw)
	return &Suite{
		Name:  name,
		Raw:   append([]byte(nil), raw...),
		SHA:   hex.EncodeToString(sum[:]),
		Cases: cases,
	}, nil
}

// CaseNames returns the case names in file order.
//
// File order and not sorted: it is the order the author chose and the order the
// runner will report progress in, and a suite whose output is alphabetised
// stops matching the file the author is reading alongside it.
func (s *Suite) CaseNames() []string {
	out := make([]string, 0, len(s.Cases))
	for _, c := range s.Cases {
		out = append(out, c.Name)
	}
	return out
}

// validator accumulates problems instead of stopping at the first.
type validator struct{ problems []string }

func (v *validator) errf(format string, a ...any) {
	v.problems = append(v.problems, fmt.Sprintf(format, a...))
}

func (v *validator) known(where string, m map[string]any, allowed ...string) {
	for k := range m {
		if contains(allowed, k) {
			continue
		}
		if near := nearest(k, allowed); near != "" {
			v.errf("%s: unknown key %q, did you mean %q?", where, k, near)
		} else {
			v.errf("%s: unknown key %q (valid: %s)", where, k, strings.Join(allowed, ", "))
		}
	}
}

func (v *validator) suite(root map[string]any) (name string, cases []Case) {
	v.known("suite", root, "name", "blueprint", "cases")

	name = v.str("suite", root, "name")
	if name == "" {
		// The name is what `eval compare` prints and what a stored run is
		// identified by in a report. An unnamed suite produces a comparison
		// table whose columns say nothing about what was compared.
		v.errf("suite: name is required; it is what a comparison of two runs " +
			"is labelled with, and an unnamed suite makes every report ambiguous")
	}
	// Resolved onto each case below rather than returned, so nothing
	// downstream has to know a suite-level default existed. A Case whose
	// Blueprint is empty is a bug, not a case that inherits at run time.
	defBlueprint := v.str("suite", root, "blueprint")

	raw, ok := root["cases"]
	if !ok || raw == nil {
		v.errf("suite: cases is required; a suite with no cases completes " +
			"instantly, reports a pass rate of 1.00 over nothing, and looks " +
			"like a clean run")
		return name, nil
	}
	items, ok := raw.([]any)
	if !ok {
		v.errf("cases: must be a list, found %T", raw)
		return name, nil
	}
	if len(items) == 0 {
		v.errf("cases: the list is empty; see above — an empty suite passes " +
			"vacuously, which is the one result that cannot be acted on")
		return name, nil
	}

	seen := map[string]bool{}
	for i, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			v.errf("cases[%d]: each case is a mapping with a name, an objective and an expect", i)
			continue
		}
		where := fmt.Sprintf("cases[%d]", i)
		v.known(where, m, "name", "blueprint", "objective", "expect")

		c := Case{
			Name:      v.str(where, m, "name"),
			Blueprint: v.str(where, m, "blueprint"),
			Objective: v.str(where, m, "objective"),
		}
		if c.Blueprint == "" {
			c.Blueprint = defBlueprint
		}
		c.Expect = v.expect(where, m)

		switch {
		case c.Name == "":
			v.errf("%s: a case needs a name; it is how `eval compare` pairs "+
				"this case across two runs, and an unnamed case cannot be "+
				"compared with anything", where)
		case seen[c.Name]:
			// Compare pairs cases BY NAME. Two cases sharing one name makes
			// the pairing pick whichever came first in each run, so a
			// regression in the second is reported as no change at all — and
			// the suite looks like it covers one more case than it measures.
			v.errf("%s: duplicate case %q; comparison pairs cases by name, so "+
				"two cannot share one", where, c.Name)
		default:
			seen[c.Name] = true
		}
		if c.Objective == "" {
			v.errf("%s: objective is required; it is the prompt the case "+
				"actually runs, and a case with none measures the blueprint's "+
				"behaviour on an empty instruction", where)
		}
		if c.Blueprint == "" {
			v.errf("%s: no blueprint, and the suite declares no default; "+
				"write `blueprint:` at the top level or on this case", where)
		}
		cases = append(cases, c)
	}
	return name, cases
}

// expect validates the judgement, and refuses a case that asserts nothing.
//
// This is the suite file's silently-wrong key. A case with an empty `expect`
// passes on every possible output, including an error message and an empty
// string, so it contributes a guaranteed +1 to the pass rate while testing
// nothing at all. A suite of them reports 1.00 and reads as a clean run.
//
// It is also the easiest mistake to make: `expect:` with the body deleted while
// editing, or a misspelled `contains` that `known` catches only because this
// function is strict about the keys.
func (v *validator) expect(where string, m map[string]any) Expectation {
	raw, ok := m["expect"]
	if !ok || raw == nil {
		v.errf("%s: expect is required; a case with no expectation passes "+
			"unconditionally, so it raises the pass rate while measuring "+
			"nothing (valid: contains, not_contains, equals)", where)
		return Expectation{}
	}
	em, ok := raw.(map[string]any)
	if !ok {
		v.errf("%s: expect must be a mapping of conditions, found %T", where, raw)
		return Expectation{}
	}
	v.known(where+".expect", em, "contains", "not_contains", "equals")

	var e Expectation
	e.Contains = v.strList(where+".expect", em, "contains")
	e.NotContains = v.strList(where+".expect", em, "not_contains")
	if eq, present := em["equals"]; present {
		s, ok := eq.(string)
		if !ok {
			v.errf("%s.expect: equals must be text, found %v", where, eq)
		} else {
			e.Equals, e.hasEquals = s, true
		}
	}

	if len(e.Contains) == 0 && len(e.NotContains) == 0 && !e.hasEquals {
		v.errf("%s.expect: no conditions; see above — an expectation that "+
			"asserts nothing is satisfied by every output, error messages "+
			"included", where)
		return e
	}

	// A condition that is both required and forbidden can never be satisfied,
	// so the case fails for a reason that is in the suite rather than in the
	// system under test — and it fails on EVERY run, dragging the pass rate
	// down by a fixed amount that looks like a persistent quality problem.
	for _, want := range e.Contains {
		if contains(e.NotContains, want) {
			v.errf("%s.expect: %q is in both contains and not_contains, so no "+
				"output can satisfy this case", where, want)
		}
	}
	// An empty string is contained in everything, which makes the condition
	// vacuous — the same defect as an empty expect, one level down, and easy to
	// produce with a trailing `- ` in the list.
	for i, want := range e.Contains {
		if want == "" {
			v.errf("%s.expect: contains[%d] is empty, and every string "+
				"contains the empty string, so this condition always holds", where, i)
		}
	}
	// And forbidding the empty string can never be satisfied, for the same
	// reason in the other direction.
	for i, unwanted := range e.NotContains {
		if unwanted == "" {
			v.errf("%s.expect: not_contains[%d] is empty, and every string "+
				"contains the empty string, so no output can satisfy this", where, i)
		}
	}
	return e
}

func (v *validator) str(where string, m map[string]any, key string) string {
	raw, ok := m[key]
	if !ok || raw == nil {
		return ""
	}
	s, ok := raw.(string)
	if !ok {
		v.errf("%s: %s must be text, found %v", where, key, raw)
		return ""
	}
	return s
}

func (v *validator) strList(where string, m map[string]any, key string) []string {
	raw, ok := m[key]
	if !ok || raw == nil {
		return nil
	}
	// A single string where a list belongs is accepted, because `contains:
	// "SQL injection"` is what everybody writes for one condition and the list
	// form is the ceremony. Refusing it would be a rule with no reason behind
	// it; the shapes are unambiguous.
	if s, ok := raw.(string); ok {
		return []string{s}
	}
	items, ok := raw.([]any)
	if !ok {
		v.errf("%s: %s must be text or a list of text, found %v", where, key, raw)
		return nil
	}
	out := make([]string, 0, len(items))
	for i, it := range items {
		s, ok := it.(string)
		if !ok {
			v.errf("%s: %s[%d] must be text, found %v", where, key, i, it)
			continue
		}
		out = append(out, s)
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// nearest suggests the closest known key, or "" when nothing is close enough.
//
// The threshold matters: suggesting a correction for a word that is not a
// misspelling of anything sends the reader to fix the wrong thing. Copied in
// shape from blueprint's, because a suggestion that behaved differently between
// two files edited together would be its own small confusion.
func nearest(got string, candidates []string) string {
	best, bestD := "", 1<<30
	for _, c := range candidates {
		d := editDistance(got, c)
		if d < bestD {
			best, bestD = c, d
		}
	}
	limit := len(got) / 2
	if limit < 1 {
		limit = 1
	}
	if bestD > limit {
		return ""
	}
	return best
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		copy(prev, cur)
	}
	return prev[len(b)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}
