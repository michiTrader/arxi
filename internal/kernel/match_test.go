package kernel

import "testing"

// MatchEventType is the pattern language, and it has three implementations that
// must agree.
//
// The rule is deliberately not a glob: exact, `*`, or ONE trailing segment. Two
// other places describe these semantics -- the reducer's matchPattern, which now
// delegates here, and internal/blueprint's validator, which refuses the shapes this
// cannot honour. That makes this function the definition, and a change to it a
// change to what every watcher in every blueprint already selects.
//
// The cases below are the ones where a "reasonable" widening would bite. Each is
// paired with what it would cost: a watcher matching more than its author wrote
// wakes agents nobody expected, and every wakened agent is billed.
func TestMatchEventTypeIsPrefixByWholeSegmentAndNotAGlob(t *testing.T) {
	cases := []struct {
		pattern, typ string
		want         bool
		why          string
	}{
		{"stage.entered", "stage.entered", true, "an exact type is the common case"},
		{"stage.entered", "stage.advanced", false, "an exact type must not match a sibling"},
		{"*", "anything.at.all", true, "* is how a blueprint says every event"},
		{"*", "", true, "* is unconditional, even for a type nothing should have"},

		{"stage.*", "stage.entered", true, "the documented wildcard"},
		{"stage.*", "stage.advanced", true, "and every other member of the segment"},
		{"stage.*", "stage.timeout.retry", true,
			"prefix, so a deeper type under the same segment matches too"},
		{"stage.*", "stage", false,
			"`stage` alone is not IN the stage namespace; matching it would make a " +
				"watcher on stage.* fire for an event with no stage"},
		{"stage.*", "stages.entered", false,
			"the dot is part of the prefix. Without it stage.* would select the " +
				"whole of a differently named namespace -- the exact mistake a " +
				"pluralised type name invites"},
		{"stage.*", "agent.activated", false, "a different namespace entirely"},

		// The shapes blueprint refuses. They are not errors here -- this function
		// reports a match or no match and has no channel for a complaint -- but they
		// must not accidentally work, or the validator would be rejecting patterns
		// that behave perfectly, and users would learn to route around it.
		{"stage*", "stage.entered", false,
			"no trailing dot, so this is an exact type nothing emits. If it matched, " +
				"the validator would be refusing a pattern that works"},
		{"st*ge.entered", "stage.entered", false, "an interior * is not a wildcard"},
		{"*.entered", "stage.entered", false, "a leading * is not a wildcard"},

		{"", "stage.entered", false, "an empty pattern selects nothing"},
		{"", "", true, "and equals itself, which is what exact match means"},
	}

	for _, tc := range cases {
		if got := MatchEventType(tc.pattern, tc.typ); got != tc.want {
			t.Errorf("MatchEventType(%q, %q) = %v, want %v\n  because: %s",
				tc.pattern, tc.typ, got, tc.want, tc.why)
		}
	}
}

// TestTheReducerAndTheCLIMatchWithTheSameFunction is a one-line test guarding a
// one-line delegation, and it earns that because of what the delegation prevents.
//
// matchPattern's body used to live in decide.go, which was fine while the reducer
// was the only caller. `arxi event log --type stage.*` asks the same question, and a
// command with its own copy is a second dialect waiting to happen: the day one gained
// partial globs the other would answer differently about the same log, and nothing
// in the output would say which spelling belonged to which.
func TestTheReducerAndTheCLIMatchWithTheSameFunction(t *testing.T) {
	for _, pattern := range []string{"*", "stage.*", "stage.entered", "stage*", ""} {
		for _, typ := range []string{"stage.entered", "stage", "stages.entered", "run.result"} {
			if matchPattern(pattern, typ) != MatchEventType(pattern, typ) {
				t.Errorf("the reducer and the exported rule disagree about (%q, %q)\n"+
					"  consequence: a watcher and `arxi event log --type` would "+
					"select different events from one log, and the user would have "+
					"no way to tell which answer was which.", pattern, typ)
			}
		}
	}
}
