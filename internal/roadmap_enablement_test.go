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

// headerField matches any "- Field:" line in an ADR's header block.
var headerField = regexp.MustCompile(`(?m)^- ([A-Za-z][A-Za-z ]*):`)

// firstSection marks the end of the header block: the first "## " heading.
//
// Scoping matters and was measured. Applied to the whole document, headerField
// matches prose bullets such as "- Existing runs are unaffected: ..." in the
// body of ADR-0017, so an unscoped vocabulary check reports a dozen false
// violations and would have to be weakened to pass -- which is how a guard
// stops guarding.
var firstSection = regexp.MustCompile(`(?m)^## `)

// adrHeaderBlock returns the text above an ADR's first "## " section, where the
// status, affects, depends-on and enables fields live.
func adrHeaderBlock(body string) string {
	if at := firstSection.FindStringIndex(body); at != nil {
		return body[:at[0]]
	}
	return body
}

// adrHeaderVocabulary enumerates every field name an ADR header may use.
//
// Enumerated rather than inferred, for the reason ADR-0023 gives about receipt
// kinds: a check that looks only for the fields it knows cannot tell a missing
// claim from a claim spelled differently. This was measured, not supposed —
// renaming "Enables" to "Unblocks" in one ADR made that ADR invisible to the
// enablement check above and the whole test passed, because a header nobody
// matches is indistinguishable from a header nobody wrote.
//
// So an unrecognized field fails closed. The cost is that adding a legitimate
// new field requires editing this set, which is the intended trade: the edit is
// a deliberate decision recorded in one place, whereas the silence it replaces
// was undetectable.
//
// The set is meant to be the measured vocabulary of the corpus, and
// TestHeaderVocabularyMatchesTheCorpus derives that vocabulary on every run and
// asserts this map equals it exactly. Frozen per-field counts are deliberately
// not written here: an earlier version of this comment stated "all 25 records
// ... Depends on in 16, Enables in 6" and every figure was wrong even when it
// was typed and drifted further with each record added (ADR-0030). A count kept
// by hand drifts precisely because nothing fails when it does, so it is derived
// below rather than narrated here.
var adrHeaderVocabulary = map[string]bool{
	"Status":     true,
	"Affects":    true,
	"Depends on": true,
	"Enables":    true,
	"Origin":     true,
}

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
		// Refuse an unknown header field before trusting the absence of an
		// "Enables" line. Without this, a renamed header is silently read as
		// "this ADR enables nothing" and the citation check below holds
		// vacuously for it.
		for _, match := range headerField.FindAllStringSubmatch(adrHeaderBlock(string(body)), -1) {
			if !adrHeaderVocabulary[match[1]] {
				t.Errorf("ADR %s carries header field %q, which is not in the header vocabulary.\n"+
					"  Consequence: a field nobody matches is indistinguishable from a field "+
					"nobody wrote, so renaming \"Enables\" silently exempts this ADR from the "+
					"citation check below.\n"+
					"  Remedy: use an enumerated field name, or add this one to "+
					"adrHeaderVocabulary as a deliberate decision.", entry.Name(), match[1])
			}
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

// TestHeaderVocabularyMatchesTheCorpus derives the header field vocabulary from
// the ADR corpus on every run and asserts adrHeaderVocabulary equals it exactly,
// in both directions.
//
// It is the same shape as the defects the memory-decision turns kept finding: a
// guard with no caller, an assertion with no subject, a narration with no
// source. This is a count with no derivation. ADR-0026 introduced
// adrHeaderVocabulary and justified it in prose as "the measured vocabulary of
// all 25 records -- Depends on in 16, Enables in 6", and every figure was wrong
// even when it was written and drifted further with each record added. The same
// off-by-more that TestEveryDocumentStatingTheADRCountAgreesWithTheCorpus pins
// for the total, one field-set deeper and left unchecked.
//
// The citation test above already fails closed on a corpus field the map does
// not list, which pins map >= corpus. The missing direction is corpus >= map: a
// key the map lists that no ADR uses makes "the measured vocabulary" false with
// nothing failing, exactly the vacuity ADR-0026 warned about one level up.
// Exact equality pins both, so the map is a checked projection of the corpus
// rather than a list kept in step with it by hand.
func TestHeaderVocabularyMatchesTheCorpus(t *testing.T) {
	entries, err := os.ReadDir("../docs/adr")
	if err != nil {
		t.Fatalf("cannot read the ADR directory: %v: the corpus is this test's only source of truth", err)
	}

	observed := map[string]int{}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		if adrFilename.FindStringSubmatch(entry.Name()) == nil {
			continue // README.md and anything else not numbered.
		}
		body, err := os.ReadFile(filepath.Join("../docs/adr", entry.Name()))
		if err != nil {
			t.Fatalf("cannot read ADR %s: %v", entry.Name(), err)
		}
		for _, match := range headerField.FindAllStringSubmatch(adrHeaderBlock(string(body)), -1) {
			observed[match[1]]++
		}
	}

	if len(observed) == 0 {
		// Fail rather than pass vacuously: with no fields observed, the equality
		// below would hold only if the map were empty too, and an empty
		// vocabulary makes the citation test's fail-closed guard match nothing.
		t.Fatal("no header fields found in any ADR: the corpus lost its header blocks or the " +
			"heading format changed, so this test compares two empty sets and stops guarding the " +
			"vocabulary. Remedy: re-derive headerField and firstSection from the format the ADRs now use")
	}

	for field, count := range observed {
		if !adrHeaderVocabulary[field] {
			t.Errorf("ADRs use header field %q (%d times) and adrHeaderVocabulary does not list it.\n"+
				"  Consequence: the citation test reads an unlisted field as an unknown one and fails "+
				"closed, so a legitimate field blocks the suite until it is enumerated.\n"+
				"  Remedy: add %q to adrHeaderVocabulary as a deliberate decision, per ADR-0030.",
				field, count, field)
		}
	}
	for field := range adrHeaderVocabulary {
		if observed[field] == 0 {
			t.Errorf("adrHeaderVocabulary lists header field %q and no ADR uses it.\n"+
				"  Consequence: the map claims to be the measured vocabulary of the corpus while "+
				"carrying a field the corpus does not contain, so \"the measured vocabulary\" is "+
				"false with nothing failing -- the vacuity ADR-0026 warned about, one level up.\n"+
				"  Remedy: remove %q from adrHeaderVocabulary, or add the ADR field it was added for.",
				field, field)
		}
	}
}

// adrNumberWord spells the record counts this project's prose actually uses.
// Only values near the current corpus size are listed: a number far outside
// that range is a symptom of something other than a stale count.
var adrNumberWord = map[int]string{
	24: "Twenty-four", 25: "Twenty-five", 26: "Twenty-six",
	27: "Twenty-seven", 28: "Twenty-eight", 29: "Twenty-nine", 30: "Thirty",
	31: "Thirty-one", 32: "Thirty-two", 33: "Thirty-three", 34: "Thirty-four",
	35: "Thirty-five", 36: "Thirty-six",
}

// TestEveryDocumentStatingTheADRCountAgreesWithTheCorpus holds the prose count
// of decision records to the number of records on disk.
//
// Found by the same probe that produced ADR-0026, and it is the same defect one
// document over: AGENTS.md opened with "Thirteen records" while docs/adr/ held
// twenty-six. The sentence containing that number instructs the reader to read
// them all before touching code, so the count is not decoration -- it is how a
// reader decides whether they have finished. Off by thirteen, it invites
// stopping at half the corpus, and the omitted half is every memory decision
// Phase 7 rests on.
//
// It also sat directly beneath this project's own "verify, do not assume" rule,
// which is the argument for pinning rather than correcting: a hand-maintained
// count drifts precisely because nothing fails when it does.
//
// The index table and the prose count are checked separately, because they go
// stale independently -- adding a record without a table row and adding one
// without updating the count are different omissions.
func TestEveryDocumentStatingTheADRCountAgreesWithTheCorpus(t *testing.T) {
	entries, err := os.ReadDir("../docs/adr")
	if err != nil {
		t.Fatalf("cannot read the ADR directory: %v", err)
	}
	var numbers []string
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		if match := adrFilename.FindStringSubmatch(entry.Name()); match != nil {
			numbers = append(numbers, match[1])
		}
	}
	sort.Strings(numbers)
	count := len(numbers)
	if count == 0 {
		t.Fatal("no numbered ADR files found: this test's only source of truth is empty, " +
			"so every assertion below would hold vacuously")
	}

	word, ok := adrNumberWord[count]
	if !ok {
		t.Fatalf("the corpus holds %d records and adrNumberWord has no spelling for it.\n"+
			"  Remedy: add it. Consequence of skipping: the prose-count check below stops "+
			"running and the number is free to drift again", count)
	}

	agents, err := os.ReadFile("../AGENTS.md")
	if err != nil {
		t.Fatalf("cannot read AGENTS.md: %v", err)
	}
	if !strings.Contains(string(agents), word+" records") {
		t.Errorf("AGENTS.md does not state %q for the %d records in docs/adr/.\n"+
			"  That line tells a reader to read every record before touching code, so the "+
			"count is how they know when they are done. Understated, it invites stopping "+
			"early -- it read \"Thirteen records\" at twenty-six, omitting every memory "+
			"decision Phase 7 rests on.\n"+
			"  Remedy: update the count in AGENTS.md.", word, count)
	}

	index, err := os.ReadFile("../docs/adr/README.md")
	if err != nil {
		t.Fatalf("cannot read the ADR index: %v", err)
	}
	for _, number := range numbers {
		if !strings.Contains(string(index), "("+number+"-") {
			t.Errorf("ADR %s has no row in docs/adr/README.md.\n"+
				"  The index is the only list of decisions a reader sees before opening "+
				"files, so an unlisted record is an invisible one. Remedy: add its row.",
				number)
		}
	}
}
