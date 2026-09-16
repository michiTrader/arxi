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
