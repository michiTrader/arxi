package workspace

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// docsThatDescribeTheAdvertisement are the prose files that tell a reader what
// a native platform will accept. They are not reference material: an operator
// decides whether to attempt a file-using run by reading them, so a stale claim
// here sends someone to debug a preflight refusal that the code never intended,
// or — worse, in the flattering direction — promises a capability that fails
// closed at acceptance.
var docsThatDescribeTheAdvertisement = []string{
	"../../README.md",
	"../../docs/roadmap.md",
	"../../docs/design/10-execution.md",
	"../../docs/design/20-use-cases.md",
	"../../spec/workspaces.md",
	"../../docs/adr/0017-shared-readonly-linux.md",
}

// TestTheDocumentedLinuxAdvertisementStaysTrue holds the prose to the capability
// matrix rather than to a second copy of the decision.
//
// ADR-0017 moved Linux from "advertises direct-files, no source-backed mode" to
// "advertises shared paired exclusively with direct-files-read". The ADR, the
// spec and the code moved together; four other documents kept asserting the old
// fact for a full release cycle, because nothing failed when they disagreed.
// docs/adr/README.md says an ADR and the code contradicting each other is a bug
// in one of the two — this test is what makes that bug loud.
//
// The pin deliberately reads the advertisement out of CurrentCapabilities
// instead of hardcoding profile names: a list here would be a third copy that
// agrees with itself while the code moves underneath it. What is asserted is
// the relationship — if Linux advertises the read-only profile, no document may
// still say Linux advertises no source-backed mode.
func TestTheDocumentedLinuxAdvertisementStaysTrue(t *testing.T) {
	linux := CurrentCapabilities("linux")

	advertisesShared := false
	for _, mode := range linux.Modes {
		if mode == ModeShared {
			advertisesShared = true
		}
	}
	readOnly, writable := false, false
	for _, profile := range linux.Profiles {
		switch profile.ID {
		case DirectFilesReadProfileID:
			readOnly = true
		case DirectFilesProfileID:
			writable = true
		}
	}

	// If this ever flips, ADR-0017 was reverted or superseded. That is allowed
	// — but then the documents below must move with it, and the guard clause
	// keeps this test from silently passing against prose it no longer matches.
	if !advertisesShared || !readOnly {
		t.Skipf("Linux no longer advertises shared+%s (shared=%v read-only=%v): "+
			"ADR-0017 was reverted or superseded, so re-derive the claims this test pins "+
			"before trusting it again", DirectFilesReadProfileID, advertisesShared, readOnly)
	}
	if writable {
		t.Errorf("Linux advertises the write-capable %s profile: ADR-0017 removed it so that "+
			"no accepted Linux combination can write, and every document pinned below still "+
			"tells the reader that writable file-using runs fail preflight", DirectFilesProfileID)
	}

	// Claims that were true before ADR-0017 and are false now. Each is matched
	// loosely enough to survive rewording but tightly enough to name the
	// specific falsehood, so a failure tells the author which sentence rotted.
	stale := []struct {
		pattern *regexp.Regexp
		why     string
	}{
		{
			regexp.MustCompile(`(?i)no native source-backed mode`),
			"Linux advertises shared, which is source-backed; a reader is told a read/grep run cannot be accepted when it can",
		},
		{
			regexp.MustCompile(`(?i)no (native )?source-backed mode is currently advertised`),
			"the shared read-only combination is advertised on Linux since ADR-0017",
		},
		{
			regexp.MustCompile(`(?i)cannot yet form an accepted file-using combination`),
			"read/grep over shared+direct-files-read is exactly the accepted file-using combination on Linux",
		},
		{
			regexp.MustCompile("(?i)native `?shared`?, `?copy`?,? (and )?`?worktree`?[^.]*not advertised"),
			"shared is advertised on Linux; only copy and worktree remain unadvertised",
		},
	}

	for _, name := range docsThatDescribeTheAdvertisement {
		raw, err := os.ReadFile(filepath.FromSlash(name))
		if err != nil {
			// A moved document must not quietly stop being checked.
			t.Errorf("cannot read %s: %v: if it moved, update this list; an unreadable "+
				"document is an unchecked document", name, err)
			continue
		}
		doc := string(raw)
		for _, claim := range stale {
			if loc := claim.pattern.FindStringIndex(doc); loc != nil {
				t.Errorf("%s still asserts a pre-ADR-0017 fact:\n  %q\n  %s\n"+
					"the capability matrix in model.go advertises shared+%s on Linux; "+
					"fix the prose or revert the advertisement, but they cannot disagree",
					name, strings.TrimSpace(doc[loc[0]:loc[1]]), claim.why, DirectFilesReadProfileID)
			}
		}
	}
}

// TestTheDocumentedResolutionOfWritersStaysTrue closes a blind spot the pin
// above had, found by reading the roadmap rather than by any test failing.
//
// TestTheDocumentedLinuxAdvertisementStaysTrue pins what Linux ADVERTISES. It
// says nothing about what a writer RESOLVES to, so when ADR-0018 moved
// file-only writers from `worktree` to `copy`, docs/roadmap.md kept asserting
// `worktree` and the suite stayed green. That is the same failure ADR-0017
// produced and the same one the pin above was written to end — reappearing one
// layer over, because the pin was built around a single decision instead of
// around the class of claim.
//
// So this derives the resolution from Resolve itself. A document may describe
// the rule, but it may not name a layout the resolver does not choose.
//
// docs/adr/0017-shared-readonly-linux.md is deliberately NOT exempt even
// though its body still says `worktree`: it carries a dated later-note
// correcting exactly that sentence, which is how docs/adr/README.md says an
// accepted ADR records a change. The check below is satisfied by the note, so
// an ADR that superseded a claim passes while a document that merely went
// stale fails.
func TestTheDocumentedResolutionOfWritersStaysTrue(t *testing.T) {
	resolveMode := func(tools ...string) Mode {
		t.Helper()
		requirements, err := Resolve(ResolutionInput{Members: []Member{{Name: "w", Tools: tools}}})
		if err != nil {
			t.Fatalf("Resolve(%v): %v", tools, err)
		}
		return requirements[0].Mode
	}

	fileOnlyWriter := resolveMode("write")
	bashUser := resolveMode("bash")

	// Guard: if these ever coincide, the distinction the prose draws is gone
	// and the assertions below would pin a sentence that no longer means
	// anything.
	if fileOnlyWriter == bashUser {
		t.Skipf("a file-only writer and a bash user both resolve to %q: the documented "+
			"distinction no longer exists, so re-derive these claims before trusting them",
			fileOnlyWriter)
	}

	// The claim is about which layout the prose attributes to a file-only
	// writer. Matched near the word that scopes it, so a document describing
	// bash's worktree correctly is not flagged.
	wrongLayout := regexp.MustCompile("(?i)(file-only )?writers resolve to `?" + string(bashUser) + "`?")

	for _, name := range docsThatDescribeTheAdvertisement {
		raw, err := os.ReadFile(filepath.FromSlash(name))
		if err != nil {
			t.Errorf("cannot read %s: %v", name, err)
			continue
		}
		doc := string(raw)
		loc := wrongLayout.FindStringIndex(doc)
		if loc == nil {
			continue
		}
		// An ADR that recorded the change in a later note has not gone stale;
		// its body is the historical decision, which is the point of an ADR.
		if strings.Contains(doc, "Later note") &&
			strings.Contains(doc, "resolve to `"+string(fileOnlyWriter)+"`") {
			continue
		}
		t.Errorf("%s says:\n  %q\nbut a file-only writer resolves to %q, not %q.\n"+
			"  Resolve is the authority here; the prose is a copy of it that drifted. "+
			"Either fix the sentence or record the change in a later note, as ADR-0017 does",
			name, strings.TrimSpace(doc[loc[0]:loc[1]]), fileOnlyWriter, bashUser)
	}
}

// TestTheAdvertisementPinNamesDocumentsThatExist keeps the list above honest.
// A pin over a file that no longer exists is worse than no pin: it reports
// success for a document nobody is checking.
func TestTheAdvertisementPinNamesDocumentsThatExist(t *testing.T) {
	for _, name := range docsThatDescribeTheAdvertisement {
		if _, err := os.Stat(filepath.FromSlash(name)); err != nil {
			t.Errorf("%s is pinned but missing: %v", name, err)
		}
	}
}
