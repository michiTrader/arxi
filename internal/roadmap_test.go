package internal_test

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// phaseHeading matches a roadmap phase heading, capturing its number.
var phaseHeading = regexp.MustCompile(`(?m)^## Phase (\d+) `)

// deepestImplementedPhase is the highest-numbered phase with an unmistakable
// implementation witness in the tree. The witness is a load-bearing symbol
// rather than a file name: a package can exist as a stub, but an event constant
// the reducer and the spec both depend on cannot.
//
// One witness is enough because the roadmap's own ordering claim does the rest
// (see TestEveryImplementedPhaseSaysSo).
var deepestImplementedPhase = struct {
	number  int
	witness string
	inFile  string
	why     string
}{
	number:  6,
	witness: `ContextPrepareFailed`,
	inFile:  "../internal/kernel/event.go",
	why:     "compaction failure is a terminal recorded outcome, which presupposes the barrier",
}

// TestEveryImplementedPhaseSaysSo holds the roadmap's status markers to the
// code, in the one direction that actually goes wrong.
//
// The roadmap is a planning document, so most of it is allowed to describe
// work that does not exist. What is NOT allowed is the reverse: a phase whose
// machinery is present, wired and enforced, described as though it were still
// ahead. That reading cost real time here — Phase 5 was fully implemented,
// including its exit evidence, while the roadmap said nothing about it, and
// Phase 6 claimed "implemented" while depending on it. Someone planning from
// that document would have set out to build a barrier that already existed.
//
// The check is deliberately narrow. It does not judge completeness — that is
// what the exit evidence in each phase is for. It asserts only that an
// implemented phase carries a status line, because the ABSENCE of one is what
// reads as "not started".
//
// # Why this is derived rather than listed
//
// This test used to carry a hand-written table of two phases, 5 and 6. It
// passed while Phases 0, 1 and 2 were fully implemented and carried no status
// line at all — Phase 1 is `host/v1` with its nine capabilities, Phase 2 is
// `internal/turn`'s canonical request. So the test described the defect
// precisely, said in its own comment that it had already cost real time, and
// covered two of the seven phases where it could occur. The other five were
// exactly as exposed as before it was written.
//
// That is the seventh instance of one shape in this corpus, and the second in
// this file's immediate neighbourhood: a guard whose subject is enumerated by
// hand goes stale in the cases nobody remembered to enumerate. ADR-0026 fixed
// the same thing one document over by deriving the ADR-to-phase pairs from the
// corpus on every run, and that rule caught its own ADR on the first run.
//
// The derivation here rests on a claim the roadmap makes about itself, on line
// 10: "The order is deliberate". A sequenced plan cannot have phase 6 built on
// nothing, so a single deepest witness implies every phase below it. One pin
// therefore covers 0 through 6, and moving the frontier forward is one edit to
// deepestImplementedPhase rather than a new table row that the next author must
// remember to add.
func TestEveryImplementedPhaseSaysSo(t *testing.T) {
	raw, err := os.ReadFile("../docs/roadmap.md")
	if err != nil {
		t.Fatalf("cannot read the roadmap: %v: an unreadable document is an unchecked document", err)
	}
	roadmap := string(raw)

	code, err := os.ReadFile(deepestImplementedPhase.inFile)
	if err != nil {
		t.Fatalf("cannot read %s: %v", deepestImplementedPhase.inFile, err)
	}
	if !strings.Contains(string(code), deepestImplementedPhase.witness) {
		// The implementation went away. That is a larger problem than a stale
		// roadmap, and not one this test should paper over.
		t.Fatalf("%s no longer contains %q.\n"+
			"  Either Phase %d was reverted — in which case its status line must go too — "+
			"or the witness moved and this pin needs re-deriving.\n"+
			"  Consequence of leaving it: the frontier below is computed from a symbol that "+
			"no longer exists, so every phase passes vacuously.",
			deepestImplementedPhase.inFile, deepestImplementedPhase.witness,
			deepestImplementedPhase.number)
	}

	// Every phase heading the document declares, in the order it declares them.
	headings := phaseHeading.FindAllStringSubmatchIndex(roadmap, -1)
	if len(headings) == 0 {
		t.Fatal("the roadmap declares no \"## Phase N\" headings.\n" +
			"  Consequence: the loop below runs over an empty set and this test reports " +
			"success while checking nothing. Remedy: re-derive phaseHeading from the " +
			"heading format the roadmap now uses")
	}

	found := 0
	for _, at := range headings {
		number, err := strconv.Atoi(roadmap[at[2]:at[3]])
		if err != nil {
			t.Errorf("cannot parse the phase number in %q: %v",
				roadmap[at[0]:at[1]], err)
			continue
		}
		if number > deepestImplementedPhase.number {
			continue // Unbuilt work: the roadmap may describe it however it likes.
		}
		found++

		heading := strings.TrimSpace(roadmap[at[0]:at[1]])
		body, ok := phaseBody(roadmap, roadmap[at[0]:at[1]])
		if !ok {
			t.Errorf("cannot isolate the body of %q", heading)
			continue
		}
		if !strings.Contains(body, "**Status:**") {
			t.Errorf("%s is implemented but carries no status line.\n"+
				"  Phase %d is implemented (%s in %s: %s), and the roadmap orders its phases "+
				"deliberately, so every phase at or below %d is built.\n"+
				"  A phase with no marker reads as \"not started\", so a reader plans to build "+
				"what is already there.\n"+
				"  Remedy: state what exists in a **Status:** line, or correct "+
				"deepestImplementedPhase if this phase genuinely is not built.",
				strings.TrimPrefix(heading, "## "),
				deepestImplementedPhase.number, deepestImplementedPhase.witness,
				strings.TrimPrefix(deepestImplementedPhase.inFile, "../"),
				deepestImplementedPhase.why, deepestImplementedPhase.number)
		}
	}

	// Guard against a heading-format change that silently empties the frontier.
	if found <= 1 {
		t.Errorf("only %d phase at or below %d was found in the roadmap.\n"+
			"  A sequenced plan whose deepest built phase is %d has %d phases to check. "+
			"Consequence: the assertion above held over almost nothing. Remedy: re-derive "+
			"phaseHeading, or correct deepestImplementedPhase",
			found, deepestImplementedPhase.number,
			deepestImplementedPhase.number, deepestImplementedPhase.number+1)
	}
}

// phaseBody returns the text between a phase heading and the next one.
func phaseBody(roadmap, heading string) (string, bool) {
	start := strings.Index(roadmap, heading)
	if start < 0 {
		return "", false
	}
	rest := roadmap[start+len(heading):]
	if next := regexp.MustCompile(`(?m)^## `).FindStringIndex(rest); next != nil {
		return rest[:next[0]], true
	}
	return rest, true
}
