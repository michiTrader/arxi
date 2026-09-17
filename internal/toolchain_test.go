package internal_test

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The project states its required Go version in three places: go.mod, the
// AGENTS.md environment recipe, and the AGENTS.md build section. Nothing kept
// them agreeing with each other, and nothing checked that the toolchain
// actually running the suite is the one being promised.
//
// That gap was not theoretical. The version in go.mod is 1.22; the suite was
// verified for ten consecutive sessions on 1.23.4, because the recovery step
// after each sandbox reset was typed from memory instead of read from
// scripts/bootstrap.sh -- which had the right version all along. Every "suite
// green" in that stretch was a statement about a toolchain the project does
// not declare.
//
// It happened to be fine: the suite passes on 1.22.5 too, which was measured
// before writing this. But "it happened to be fine" is the thing AGENTS.md
// names directly under "Verify, do not assume" -- the project requires
// measuring a claim before reporting it, and the toolchain is the one claim
// every other measurement rests on.
//
// A newer Go passing is weak evidence for an older Go passing, and the
// direction that matters is the one nobody tests: a contributor on the
// declared 1.22 hitting a failure that the author never saw because they were
// a minor version ahead.

// declaredGoVersionPattern extracts the `go 1.x` line from go.mod. go.mod is
// the authority here rather than the prose: it is what the toolchain itself
// enforces, so a disagreement means the documents are wrong, not the module.
var declaredGoVersionPattern = regexp.MustCompile(`(?m)^go\s+(\d+)\.(\d+)`)

func declaredGoVersion(t *testing.T) (major, minor int) {
	t.Helper()
	raw, err := os.ReadFile("../go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	match := declaredGoVersionPattern.FindStringSubmatch(string(raw))
	if match == nil {
		t.Fatalf("go.mod declares no `go 1.x` directive:\n%s", raw)
	}
	major, _ = strconv.Atoi(match[1])
	minor, _ = strconv.Atoi(match[2])
	return major, minor
}

// There is deliberately NO test here asserting that the running toolchain is
// at least the one go.mod declares. One was written and then deleted, because
// mutation proved it could never fail:
//
//   - with GOTOOLCHAIN=auto (the default), raising the floor in go.mod makes
//     the go command download and re-exec the newer toolchain, so by the time
//     any test runs, runtime.Version() already satisfies the floor;
//   - with GOTOOLCHAIN=local, the go command refuses outright --
//     "go.mod requires go >= 1.25 (running go 1.22.5)" -- and the package
//     never builds, so no assertion inside it is ever reached.
//
// Both were measured, not reasoned about. The invariant is real and it is
// already enforced by the toolchain itself, which makes a Go-level test for it
// decoration: it would pass forever and be mistaken for protection. What is
// left below is the part Go does NOT enforce -- the prose and the install
// script, which can name any version at all without the compiler caring.

// docsStatingTheGoVersion are the places a human is told which toolchain to
// install. scripts/bootstrap.sh is included because it is the one that is
// EXECUTED: a script that installs a version the module does not declare is a
// worse failure than prose saying so, since nobody reads what it did.
var docsStatingTheGoVersion = []string{
	"../AGENTS.md",
	"../scripts/bootstrap.sh",
}

// TestEveryDocumentStatingTheGoVersionAgreesWithGoMod holds the install
// instructions to the module's own declaration.
//
// The failure this prevents is specific and already happened in a milder form:
// bootstrap.sh pins 1.22.5 while the AGENTS.md recipe pins 1.22.5 and go.mod
// says 1.22 -- consistent today, and nothing would have complained if they
// were not. A contributor following a stale recipe installs a toolchain that
// cannot build the module and has no way to tell that the document, not their
// machine, is at fault.
//
// Only the major.minor is compared. The patch level in an install recipe is a
// pin for reproducibility, not a module requirement, and demanding that go.mod
// track every patch bump would make this test a chore that gets deleted.
func TestEveryDocumentStatingTheGoVersionAgreesWithGoMod(t *testing.T) {
	wantMajor, wantMinor := declaredGoVersion(t)
	want := strconv.Itoa(wantMajor) + "." + strconv.Itoa(wantMinor)

	// Matches the version inside a download URL or an install instruction:
	// go1.22.5.linux-amd64.tar.gz, GO_VERSION="1.22.5", "Requires Go 1.22".
	mention := regexp.MustCompile(`(?i)(?:go|GO_VERSION=")\s*(\d+)\.(\d+)(?:\.\d+)?`)

	for _, name := range docsStatingTheGoVersion {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Errorf("cannot read %s: %v: an unreadable document is an unchecked document", name, err)
			continue
		}
		body := string(raw)

		found := false
		for _, m := range mention.FindAllStringSubmatch(body, -1) {
			got := m[1] + "." + m[2]
			// Skip incidental numbers that are not toolchain versions at all
			// (a "go 1.0"-style match inside unrelated prose would otherwise
			// produce noise). Only same-major mentions are treated as claims
			// about the toolchain.
			if m[1] != strconv.Itoa(wantMajor) {
				continue
			}
			found = true
			if got != want {
				t.Errorf("%s tells a reader to use Go %s but go.mod declares go%s.\n"+
					"  Someone following this document installs a toolchain the module does not "+
					"declare, and the resulting build failure looks like their machine is broken.\n"+
					"  Full mention: %q", name, got, want, strings.TrimSpace(m[0]))
			}
		}
		if !found {
			t.Errorf("%s states no Go %d.x version at all.\n"+
				"  It is pinned here because it is where a contributor learns which toolchain to "+
				"install; if that moved elsewhere, update this list rather than leaving a pin "+
				"that silently checks nothing", name, wantMajor)
		}
	}
}
