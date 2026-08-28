package blueprint

import (
	"reflect"
	"strings"
	"testing"
)

// TestParsesTheBlueprintFromTheDesignDoc protects the one input that must work:
// the exact team.yaml printed in docs/design/20-use-cases.md §20.4. If the
// parser cannot read the file the documentation tells users to write, the
// subset is not a subset, it is a different language.
func TestParsesTheBlueprintFromTheDesignDoc(t *testing.T) {
	src := `# team.yaml
name: feature-team
members:
  - {name: backend,  role: implementer, tools: [read, write, bash]}
  - {name: frontend, role: implementer, tools: [read, write]}
  - {name: security, role: reviewer,    tools: [read], advisory: true}
stages:
  - {name: build,  advance_when: all,      timeout_ms: 1800000}
  - {name: review, advance_when: quorum:2}
interaction:
  steer_target: coordinator
`
	got, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("the blueprint from the design doc does not parse: %v\n"+
			"the documented example is the contract; fix the parser, not the doc", err)
	}
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("top level is %T, expected a mapping: every blueprint is a mapping of keys", got)
	}
	if m["name"] != "feature-team" {
		t.Errorf("name = %v, expected feature-team", m["name"])
	}

	members, ok := m["members"].([]any)
	if !ok || len(members) != 3 {
		t.Fatalf("members = %#v, expected 3 items", m["members"])
	}
	backend := members[0].(map[string]any)
	if backend["role"] != "implementer" {
		t.Errorf("backend.role = %v, expected implementer", backend["role"])
	}
	if !reflect.DeepEqual(backend["tools"], []any{"read", "write", "bash"}) {
		t.Errorf("backend.tools = %#v, expected [read write bash]: an inline list that "+
			"loses items would silently strip a permission the user granted", backend["tools"])
	}
	security := members[2].(map[string]any)
	if security["advisory"] != true {
		t.Errorf("security.advisory = %#v, expected the boolean true; if it parses as the "+
			"string \"true\" the member counts toward the quorum and the advance rule changes",
			security["advisory"])
	}

	stages := m["stages"].([]any)
	build := stages[0].(map[string]any)
	if build["timeout_ms"] != int64(1800000) {
		t.Errorf("build.timeout_ms = %#v (%T), expected int64 1800000: a timeout parsed as "+
			"a string arms no timer at all", build["timeout_ms"], build["timeout_ms"])
	}
	review := stages[1].(map[string]any)
	if review["advance_when"] != "quorum:2" {
		t.Errorf("review.advance_when = %#v, expected \"quorum:2\": splitting on the inner "+
			"colon turns a quorum rule into something the reducer does not recognise",
			review["advance_when"])
	}
}

// TestNestedBlockSyntaxParsesLikeInlineSyntax protects that the two ways of
// writing the same blueprint agree. Users mix both styles, and a parser that
// treats them differently produces two runs with different rules from what the
// author believes is one blueprint.
func TestNestedBlockSyntaxParsesLikeInlineSyntax(t *testing.T) {
	inline := "members:\n  - {name: backend, tools: [read, write], advisory: false}\n"
	block := "members:\n  - name: backend\n    tools:\n      - read\n      - write\n    advisory: false\n"

	a, err := Parse([]byte(inline))
	if err != nil {
		t.Fatalf("inline form does not parse: %v", err)
	}
	b, err := Parse([]byte(block))
	if err != nil {
		t.Fatalf("block form does not parse: %v", err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("the same blueprint written two ways parsed differently:\n inline: %#v\n block:  %#v\n"+
			"users mix both styles; if they disagree the file on screen is not the run that executes", a, b)
	}
}

func TestScalarTypes(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want any
	}{
		{"integer stays an integer", "v: 42", int64(42)},
		{"negative integer", "v: -7", int64(-7)},
		{"float stays a float", "v: 0.8", 0.8},
		{"true is a boolean", "v: true", true},
		{"false is a boolean", "v: false", false},
		{"null is nil", "v: null", nil},
		{"tilde is nil", "v: ~", nil},
		{"bare word is a string", "v: coordinator", "coordinator"},
		{"quoted number stays a string", `v: "42"`, "42"},
		{"quoted keeps inner spaces", `v: "  padded  "`, "  padded  "},
		{"single quotes escape by doubling", `v: 'it''s'`, "it's"},
		{"colon inside a value survives", "v: quorum:2", "quorum:2"},
		{"time-like value is not split", "v: 12:30", "12:30"},
		{"hash inside quotes is not a comment", `v: "a # b"`, "a # b"},
		{"hash after a space is a comment", "v: plain # note", "plain"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse([]byte(tc.src))
			if err != nil {
				t.Fatalf("%q did not parse: %v", tc.src, err)
			}
			v := got.(map[string]any)["v"]
			if !reflect.DeepEqual(v, tc.want) {
				t.Fatalf("%q parsed to %#v (%T), expected %#v (%T)", tc.src, v, v, tc.want, tc.want)
			}
		})
	}
}

// TestRejectsUnsupportedConstructs is the core of the design: every construct
// outside the subset must fail loudly. A parser that quietly ignores what it
// does not understand hands back a config missing the very rule the user was
// relying on, and the run then behaves in a way the file does not explain.
func TestRejectsUnsupportedConstructs(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		wantSub string
	}{
		{"tab indentation", "members:\n\t- name: a\n", "tab"},
		{"anchor", "base: &b\n  x: 1\n", "anchors"},
		{"alias", "a: *b\n", "anchors"},
		{"type tag", "v: !!str 3\n", "tags"},
		{"block scalar", "prompt: |\n  hello\n", "block scalars"},
		{"folded scalar", "prompt: >\n  hello\n", "block scalars"},
		{"multi document", "a: 1\n---\nb: 2\n", "multi-document"},
		{"duplicate key", "a: 1\na: 2\n", "duplicate"},
		{"duplicate key inline", "m: {a: 1, a: 2}\n", "duplicate"},
		{"unterminated quote", `v: "open`, "unterminated"},
		{"unclosed flow sequence", "v: [a, b\n", "closing"},
		{"unclosed flow mapping", "v: {a: 1\n", "closing"},
		{"yaml 1.1 yes", "advisory: yes\n", "ambiguous"},
		{"yaml 1.1 no", "advisory: no\n", "ambiguous"},
		{"yaml 1.1 on", "advisory: on\n", "ambiguous"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.src))
			if err == nil {
				t.Fatalf("%q parsed without error; an unsupported construct that is silently "+
					"accepted produces a config that does not match the file the user wrote", tc.src)
			}
			if !strings.Contains(strings.ToLower(err.Error()), tc.wantSub) {
				t.Fatalf("error for %q was %q; it must mention %q so the user knows the "+
					"construct is unsupported rather than their file being malformed",
					tc.src, err.Error(), tc.wantSub)
			}
		})
	}
}

// TestErrorsCarryTheLineNumber protects that a rejection is actionable. An
// error with no line sends the user hunting through a file the parser already
// located the problem in.
func TestErrorsCarryTheLineNumber(t *testing.T) {
	src := "name: ok\nmembers:\n  - name: a\nprompt: |\n  text\n"
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatal("a block scalar must be rejected: it is outside the supported subset")
	}
	se, ok := err.(*SyntaxError)
	if !ok {
		t.Fatalf("error is %T, expected *SyntaxError: callers need the line to point at the problem", err)
	}
	if se.Line != 4 {
		t.Errorf("line = %d, expected 4 (where the `|` is); a wrong line is worse than none, "+
			"it sends the user to correct something that is fine", se.Line)
	}
	if se.Hint == "" {
		t.Error("the error carries no hint; rejecting a construct without saying what to write " +
			"instead leaves the user assuming their YAML is invalid when it is merely unsupported")
	}
}

// TestYesNoAmbiguityIsRejectedNotGuessed protects the specific trap that has a
// name in the YAML world. A member called `no` becoming the boolean false, or
// `advisory: yes` staying the string "yes", are both defensible readings of
// different YAML versions and both silently change who counts toward a quorum.
func TestYesNoAmbiguityIsRejectedNotGuessed(t *testing.T) {
	if _, err := Parse([]byte("advisory: yes\n")); err == nil {
		t.Fatal("`yes` was accepted: YAML 1.1 reads it as true and YAML 1.2 as the string " +
			"\"yes\", and either choice silently changes whether the member counts toward the quorum")
	}
	got, err := Parse([]byte(`advisory: "yes"`))
	if err != nil {
		t.Fatalf("quoted \"yes\" must be accepted as a string: quoting is how the user " +
			"resolves the ambiguity we asked them to resolve")
	}
	if v := got.(map[string]any)["advisory"]; v != "yes" {
		t.Errorf("quoted \"yes\" parsed to %#v, expected the string \"yes\"", v)
	}
}

func TestEmptyAndCommentOnlyInputAreEmptyMappings(t *testing.T) {
	for _, src := range []string{"", "\n\n", "# just a comment\n", "   \n# another\n"} {
		got, err := Parse([]byte(src))
		if err != nil {
			t.Fatalf("%q did not parse: %v; an empty file is empty, not malformed", src, err)
		}
		m, ok := got.(map[string]any)
		if !ok || len(m) != 0 {
			t.Fatalf("%q parsed to %#v, expected an empty mapping", src, got)
		}
	}
}
