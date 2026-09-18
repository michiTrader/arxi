package internal_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

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
// what the exit evidence in each phase is for. It asserts only that a phase
// with an unmistakable implementation witness in the tree carries a status
// line, because the ABSENCE of one is what reads as "not started".
func TestEveryImplementedPhaseSaysSo(t *testing.T) {
	raw, err := os.ReadFile("../docs/roadmap.md")
	if err != nil {
		t.Fatalf("cannot read the roadmap: %v: an unreadable document is an unchecked document", err)
	}
	roadmap := string(raw)

	// Each entry pairs a phase heading with a fact about the tree that only
	// holds once that phase is real. The witnesses are load-bearing symbols
	// rather than file names: a package can exist as a stub, but an event
	// constant the reducer and the spec both depend on cannot.
	for _, phase := range []struct {
		heading string
		witness string
		inFile  string
		why     string
	}{
		{
			heading: "## Phase 5 — Canonical transcript and prepared context",
			witness: `ContextPrepared         EventType = "context.prepared"`,
			inFile:  "../internal/kernel/event.go",
			why: "the preparation barrier is a committed event the executor refuses to start a " +
				"model child without, not a plan",
		},
		{
			heading: "## Phase 6 — Measured context compaction",
			witness: `ContextPrepareFailed`,
			inFile:  "../internal/kernel/event.go",
			why:     "compaction failure is a terminal recorded outcome, which presupposes the barrier",
		},
	} {
		body, ok := phaseBody(roadmap, phase.heading)
		if !ok {
			t.Errorf("the roadmap has no heading %q.\n"+
				"  If phases were renumbered or renamed, update this list: a pin over a heading "+
				"that no longer exists silently checks nothing", phase.heading)
			continue
		}

		code, err := os.ReadFile(phase.inFile)
		if err != nil {
			t.Errorf("cannot read %s: %v", phase.inFile, err)
			continue
		}
		if !strings.Contains(string(code), phase.witness) {
			// The implementation went away. That is a larger problem than a
			// stale roadmap, and not one this test should paper over.
			t.Errorf("%s no longer contains %q.\n"+
				"  Either the phase was reverted — in which case its status line must go too — "+
				"or the witness moved and this pin needs re-deriving",
				phase.inFile, phase.witness)
			continue
		}

		if !strings.Contains(body, "**Status:**") {
			t.Errorf("%s is implemented but carries no status line.\n"+
				"  %s\n"+
				"  A phase with no marker reads as \"not started\", so a reader plans to build "+
				"what is already there. Phases 3, 4 and 6 state their status; this one must too",
				strings.TrimPrefix(phase.heading, "## "), phase.why)
		}
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
