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

// existenceClaim matches a sentence asserting that machinery is now present.
//
// These are the affirmative phrasings this roadmap actually uses when a phase
// reports something built. Enumerated rather than inferred for the reason
// ADR-0023 gives about receipt kinds: a check that matches only what it knows
// cannot tell a missing claim from one spelled differently, so the vacuity
// guard below fails closed when nothing matches at all.
var existenceClaim = regexp.MustCompile(`(?i)\b(now exists|is now implemented|now holds|store now exists)\b`)

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

// nonexistenceClaim matches a status line asserting that machinery is absent.
//
// Scoped to the status line's own sentence rather than the whole phase body,
// because the body is narration and is allowed to discuss absence freely --
// Phase 7 legitimately says a run "does not exist yet" while explaining why a
// per-run log cannot carry memory. The claim under test is the one a reader
// takes as the phase's headline verdict, which is the status line.
var nonexistenceClaim = regexp.MustCompile(`(?i)\b(not started|do(es)? not exist|is missing|unbuilt|nothing exists)\b`)

// statusLine returns the sentence following "**Status:**" in a phase body.
//
// A sentence, not the paragraph: the paragraph is where a phase narrates what
// changed, and Phase 7's narration correctly reported the store as built while
// its opening verdict said the opposite. Comparing the paragraph against itself
// would find no contradiction, because the contradiction IS the paragraph.
func statusLine(body string) (string, bool) {
	at := strings.Index(body, "**Status:**")
	if at < 0 {
		return "", false
	}
	rest := body[at+len("**Status:**"):]
	// The first sentence, allowing the line breaks a wrapped document inserts.
	if end := strings.Index(rest, ". "); end >= 0 {
		return strings.TrimSpace(rest[:end]), true
	}
	if end := strings.Index(rest, "\n\n"); end >= 0 {
		return strings.TrimSpace(rest[:end]), true
	}
	return strings.TrimSpace(rest), true
}

// TestNoPhaseDeclaresMachineryAbsentThatItAlsoDescribesAsBuilt holds a phase's
// headline verdict to the rest of its own status block.
//
// # The defect this was written for
//
// Phase 7's status opened with "not started; the store, retrieval and deletion
// lineage do not exist" while, one hundred lines below in the same block, it
// said "The store now exists" and described `internal/memorystore` building
// all three: `Put`/`Approve`/`Correct` for records, `Retrieve` authorizing
// before ranking, and `Delete` appending a tombstone so deletion survives
// replication. Every one of the three named absences was present in the tree.
//
// # Why neither existing guard could see it
//
// This is the sixth time in this corpus that a stale claim survived because the
// check nearest to it was scoped just short of the claim:
//
//   - TestEveryImplementedPhaseSaysSo skips every phase above
//     deepestImplementedPhase, and Phase 7 is above it. Worse, it asserts only
//     that a "**Status:**" marker EXISTS, never what it says -- its own comment
//     scopes it that way deliberately, because absence was the defect it was
//     built for.
//   - TestEveryEnablingDecisionIsCitedByThePhaseItEnables passed, and correctly:
//     Phase 7 does cite ADR-0027. It cites it in the very paragraph that
//     contradicts the phase's opening line. A citation check cannot notice that
//     the citation refutes the headline above it.
//
// So the two guards between them checked that a status line exists and that it
// cites the right decisions, and neither checked whether it was true.
//
// # Why the subject is derived
//
// The witness is a symbol looked up in the tree, not a phrase listed here. A
// hand-listed subject goes stale in exactly the cases nobody remembered to
// list, which is the finding behind ADR-0026 and the reason
// TestEveryImplementedPhaseSaysSo was rewritten to derive its frontier. A
// status line claiming absence must therefore survive a search for the thing it
// says is absent.
func TestNoPhaseDeclaresMachineryAbsentThatItAlsoDescribesAsBuilt(t *testing.T) {
	raw, err := os.ReadFile("../docs/roadmap.md")
	if err != nil {
		t.Fatalf("cannot read the roadmap: %v: an unreadable document is an unchecked document", err)
	}
	roadmap := string(raw)

	headings := phaseHeading.FindAllStringSubmatchIndex(roadmap, -1)
	if len(headings) == 0 {
		t.Fatal("the roadmap declares no \"## Phase N\" headings.\n" +
			"  Consequence: this test holds over an empty set and reports success while " +
			"checking nothing. Remedy: re-derive phaseHeading from the heading format " +
			"the roadmap now uses")
	}

	checked := 0
	for _, at := range headings {
		heading := strings.TrimSpace(roadmap[at[0]:at[1]])
		body, ok := phaseBody(roadmap, roadmap[at[0]:at[1]])
		if !ok {
			t.Errorf("cannot isolate the body of %q", heading)
			continue
		}
		status, ok := statusLine(body)
		if !ok {
			continue // Covered by TestEveryImplementedPhaseSaysSo.
		}
		if !nonexistenceClaim.MatchString(status) {
			continue
		}
		checked++

		// The phase's headline says something is absent. If the same block
		// later reports it built, the two cannot both be current, and the
		// headline is the one a reader acts on.
		//
		// Compared as text rather than by stat'ing packages, because a package
		// existing is not the same claim: Phase 7's retrieval is deliberately
		// unwired from `internal/exec`, so the presence of that directory says
		// nothing about whether the phase overstates itself. The defect is the
		// block disagreeing with itself, so that is what is measured.
		contradiction := existenceClaim.FindString(body)
		if contradiction == "" {
			continue
		}
		t.Errorf("%s opens by claiming %q, and the same status block then says %q.\n"+
			"  Status line: %q\n"+
			"  Both cannot be current, and the opening verdict is the one a reader acts "+
			"on: they plan to build what is already built. That misreading has cost real "+
			"time here twice -- Phase 5 was fully implemented while the roadmap said "+
			"nothing, and Phase 7 shipped a store while its headline said no store "+
			"existed.\n"+
			"  Remedy: rewrite the status line to state what exists, keeping the gaps "+
			"that remain as gaps. Do not delete the narration -- it is the evidence.",
			strings.TrimPrefix(heading, "## "),
			strings.ToLower(nonexistenceClaim.FindString(status)),
			strings.ToLower(contradiction), status)
	}

	// Vacuity is guarded by proving the detector still fires, NOT by requiring
	// the roadmap to contain a defect.
	//
	// The distinction is the whole reason this block is not an "if checked ==
	// 0" error. Once Phase 7's line is corrected, no status line claims absence
	// and none should -- demanding one would make a clean document fail, so the
	// next author would relax the test rather than the document. But a stale
	// `nonexistenceClaim` or a `statusLine` that stopped finding the sentence
	// would also produce zero matches, and that must not read as health.
	//
	// So the mechanism is exercised against a fixture that is known to be
	// contradictory. If this stops failing, the detector is broken regardless
	// of what the real document says.
	const contradictory = "**Status:** not started; the store does not exist.\n" +
		"Later in the same block: the store now exists, built last turn.\n"
	probe, ok := statusLine(contradictory)
	if !ok {
		t.Fatal("statusLine found no status sentence in a fixture that opens with " +
			"\"**Status:**\".\n" +
			"  Consequence: every phase above was skipped and this test reported success " +
			"while checking nothing. Remedy: re-derive statusLine from the status format " +
			"the roadmap now uses.")
	}
	if !nonexistenceClaim.MatchString(probe) {
		t.Errorf("nonexistenceClaim does not match %q.\n"+
			"  Consequence: a phase claiming absence is invisible to this test, so the "+
			"loop above holds vacuously. Remedy: re-derive nonexistenceClaim from the "+
			"phrasing the roadmap now uses.", probe)
	}
	if !existenceClaim.MatchString(contradictory) {
		t.Errorf("existenceClaim does not match the fixture's %q.\n"+
			"  Consequence: the contradiction check cannot fire, so a phase may claim "+
			"absence and report the same machinery built. Remedy: re-derive "+
			"existenceClaim.", "the store now exists")
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
