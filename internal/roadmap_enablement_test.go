package internal_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// enablesLine matches the "Enables:" header an ADR uses to name the phase its
// decision unblocks, capturing the phase number.
//
// Anchored to the header block form the corpus actually uses ("- Enables: Phase
// 7 (...)"), so a mention of a phase in prose is not mistaken for a claim of
// enablement. The distinction matters: prose discusses phases freely, while the
// header is the ADR asserting that a phase now depends on it.
var enablesLine = regexp.MustCompile(`(?m)^- Enables: Phase (\d+)`)

// adrCitation matches a reference to an ADR by number anywhere in prose.
var adrCitation = regexp.MustCompile(`ADR-(\d{4})`)

// adrFilename extracts the number an ADR file is named with.
var adrFilename = regexp.MustCompile(`^(\d{4})-`)

// TestEveryEnablingDecisionIsCitedByThePhaseItEnables closes the gap between an
// ADR claiming to unblock a phase and that phase's status narration admitting
// it exists.
//
// This is the defect ADR-0026 was written for, and it is the third instance of
// one shape. ADR-0024 found a guard with no caller; ADR-0025 found an assertion
// with no subject; this is a narration with no source. In each case a decision
// was published and something that was supposed to carry it did not, and the
// suite stayed green because nothing connected the two.
//
// Concretely: ADR-0025 corrected the memory channel in the two assemblers that
// never adopted it, declared "Enables: Phase 7", and the roadmap's Phase 7
// status went on listing five settled prerequisites and citing ADR-0020 through
// ADR-0024. A reader planning the store from that document would have read
// "the preparer presents it as a user-role message" and concluded the channel
// was a solved, single-site property — which is the precise belief ADR-0025
// exists to refute, and the belief under which the defect survived four ADRs.
//
// The direction of the check is the point. It does not require the roadmap to
// discuss unbuilt work, and it does not judge whether a prerequisite is truly
// settled — the exit evidence in each phase is for that. It requires only that
// a phase cannot silently omit a decision that named it. An omission reads as
// absence of the decision, and absence is what gets designed around.
//
// It is derived rather than listed: the pairs come from the ADR corpus and the
// roadmap headings on every run. A hardcoded table would have to be edited by
// the same author who forgot the citation, which is the failure it is meant to
// catch.
func TestEveryEnablingDecisionIsCitedByThePhaseItEnables(t *testing.T) {
	raw, err := os.ReadFile("../docs/roadmap.md")
	if err != nil {
		t.Fatalf("cannot read the roadmap: %v: an unreadable document is an unchecked document", err)
	}
	roadmap := string(raw)

	entries, err := os.ReadDir("../docs/adr")
	if err != nil {
		t.Fatalf("cannot read the ADR directory: %v: the corpus is this test's only source of truth", err)
	}

	// phase number -> the ADR numbers claiming to enable it.
	claimed := map[string][]string{}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		number := adrFilename.FindStringSubmatch(entry.Name())
		if number == nil {
			continue // README.md and anything else not numbered.
		}
		body, err := os.ReadFile(filepath.Join("../docs/adr", entry.Name()))
		if err != nil {
			t.Fatalf("cannot read ADR %s: %v", entry.Name(), err)
		}
		for _, match := range enablesLine.FindAllStringSubmatch(string(body), -1) {
			claimed[match[1]] = append(claimed[match[1]], number[1])
		}
	}

	if len(claimed) == 0 {
		// Fail rather than pass vacuously. If the header were renamed, every
		// assertion below would hold over an empty set and this test would
		// report success while checking nothing -- the exact shape of defect
		// ADR-0025 recorded as a surviving mutation.
		t.Fatal("no ADR declares an \"- Enables: Phase N\" header.\n" +
			"  Either the corpus lost the header or its format changed. Consequence: this test " +
			"passes over an empty set and stops connecting decisions to the phases they " +
			"unblock. Remedy: re-derive enablesLine from the format the ADRs now use")
	}

	phases := make([]string, 0, len(claimed))
	for phase := range claimed {
		phases = append(phases, phase)
	}
	sort.Strings(phases)

	for _, phase := range phases {
		heading := "## Phase " + phase + " "
		start := strings.Index(roadmap, heading)
		if start < 0 {
			t.Errorf("ADRs %s declare they enable Phase %s, and the roadmap has no such phase.\n"+
				"  Consequence: decisions point at a plan nobody can find. Remedy: either the "+
				"phase was renumbered and the ADR headers need updating, or the heading format "+
				"changed and this pin needs re-deriving",
				strings.Join(claimed[phase], ", "), phase)
			continue
		}
		body := roadmap[start:]
		if next := regexp.MustCompile(`(?m)^## `).FindStringIndex(body[len(heading):]); next != nil {
			body = body[:len(heading)+next[0]]
		}

		cited := map[string]bool{}
		for _, match := range adrCitation.FindAllStringSubmatch(body, -1) {
			cited[match[1]] = true
		}

		var missing []string
		for _, adr := range claimed[phase] {
			if !cited[adr] {
				missing = append(missing, "ADR-"+adr)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("Phase %s does not cite %s, which declare they enable it.\n"+
				"  An uncited decision reads as a decision that was never made, so a reader "+
				"plans around the defect it corrected. That is how the memory channel survived "+
				"four ADRs in two of the three packages ADR-0020 named.\n"+
				"  Remedy: state in Phase %s's status what the decision settled, or remove the "+
				"\"Enables: Phase %s\" header if it no longer does.",
				phase, strings.Join(missing, ", "), phase, phase)
		}
	}
}
