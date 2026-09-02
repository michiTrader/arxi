package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/surface"
)

// The README's "N of 49 declared capabilities are wired" figure, measured.
//
// The figure is deliberately not written into this comment. It moves every
// time a verb is wired, and a comment stating it would be one more copy to
// forget -- which is the failure this whole file exists to prevent.
//
// # Why this file exists
//
// The number was prose, arrived at by a shell probe run by hand and then typed
// into the README in four places. Nothing in the suite read it. A figure that
// only a human maintains is a figure that goes stale the first time somebody
// wires a command and forgets, and "the README overstates what works" is the
// one class of staleness a reader cannot detect from the inside.
//
// It went wrong in a worse way than staleness, though, and that is the actual
// reason for this file. The README described the probe as counting the paths
// that "do not answer *declared but not implemented*". The binary does not say
// that. It says:
//
//	arxi run replay is declared in the surface but not implemented yet.
//
// Rebuilding the probe from the README's wording -- which is what a reader
// checking the claim would do, and is exactly what happened -- produces a
// pattern that matches nothing, so every one of the 49 paths falls into the
// "implemented" bucket and the probe reports 49/49, 100.0%. A verification tool
// that cannot fail reports total success. The 100% was believable enough to
// stop and diagnose only because 25 unwired commands do not appear in an
// afternoon.
//
// So the guard below does two separate jobs, and the second is the one the
// hand-run probe could never do:
//
//  1. Count the wired capabilities and compare against the README.
//  2. Assert the sentinel this count depends on is really the binary's wording,
//     by checking a path known to be unimplemented actually prints it.
//
// Without (2), (1) is the same trap one layer up: a sentinel that stops
// matching turns this test into one that certifies 100% and passes.
//
// # Why the count is "did not refuse" rather than "succeeded"
//
// A wired command invoked with no arguments mostly fails -- `arxi trigger show`
// wants a name, `arxi eval run` wants two flags. Those failures come from the
// command's own flag checking, which means the executor ran, which is the thing
// being counted. Requiring success would need a valid invocation for all 49 and
// would measure fixture-building rather than wiring. The distinction that
// matters to a user is only ever "does this verb exist yet", and the binary
// answers exactly that question in one sentence.
//
// Every one of the 24 was read by eye once against this rule, and none was a
// false positive: 20 printed their own usage or flag error, and `model list`,
// `trigger list`, `inbox` and `eval list` printed real (empty) output.

// notImplementedSentinel is the phrase the binary prints for a declared command
// with no executor behind it, copied from cmd/arxi/main.go's notImplemented.
//
// Not a shortened paraphrase. The README's paraphrase is what broke the probe,
// and a sentinel is the one string in a test that must never be approximated:
// shortening it cannot make the test fail loudly, only make it stop finding
// anything, and this test's failure mode from over-matching is silence.
const notImplementedSentinel = "is declared in the surface but not implemented yet"

// blockingPaths are declared paths that do not return on their own.
//
// `trigger run` is a watcher: it prints "watching 0 trigger(s)" and then sleeps
// until interrupted, so probing it costs the whole timeout. It is counted as
// implemented by inspection rather than by invocation, and the count below says
// so out loud instead of quietly skipping it -- a skipped entry is how a probe
// silently loses part of its denominator.
//
// `serve` is NOT here, and that is deliberate: it terminates on EOF, and every
// invocation in this file gets a closed stdin. An earlier hand-run probe let
// serve inherit the shell's stdin, where it read the list of paths being probed
// and swallowed two of them, reporting 22/47 against a registry of 49. Closing
// stdin per invocation is what makes serve safe to probe, so the fix lives in
// the runner rather than in an exception list.
var blockingPaths = map[string]bool{
	"trigger run": true,
}

func TestTheReadmeCapabilityCountIsWhatTheBinaryActuallyDoes(t *testing.T) {
	// Job 2 first, because job 1's answer is meaningless if it fails.
	//
	// `state lock` is used as the canary because it is declared, has no executor,
	// and is the unwired verb whose implementation is least like the work already
	// done here: a cooperative lock with a --ttl is a LEASE, and nothing in this
	// tree has one. writer.lock is not a counter-example -- it is a per-run-dir
	// file holding a pid, taken and dropped inside a single command -- whereas
	// this verb has to hold a named key across processes until it expires, with
	// nobody running to expire it. This repository wires verbs one per change, so
	// the canary that survives longest is the one that is not a variation on
	// something already built.
	//
	// It has moved three times, from `run replay` to `run attach` to `run steer`
	// to here, and every move happened because the verb got built. The test failed
	// exactly as designed each time, with the instruction below naming the fix,
	// which is the right amount of noise: a demand to re-verify the sentinel
	// against a path that is genuinely unwired, not a bug.
	dir := workdir(t)
	canary := arxi(t, dir, "state", "lock")
	if !strings.Contains(canary.out, notImplementedSentinel) {
		t.Fatalf("the sentinel this test counts with does not appear for a path known to be unimplemented.\n"+
			"  probed:   arxi state lock\n  expected: %q\n  got:\n%s\n"+
			"  consequence: if the sentinel is wrong, NOTHING matches it, every "+
			"declared path counts as implemented, and this test certifies 100%% "+
			"coverage while passing. That already happened once with the README's "+
			"paraphrase of this phrase.\n"+
			"  fix: copy the wording from notImplemented in main.go -- or, if "+
			"state lock is now built, point this canary at another unimplemented path.",
			notImplementedSentinel, canary.out)
	}

	implemented, notImplemented, walked := probeSurface(t, dir)

	// The denominator is asserted against the registry, not assumed. A probe
	// that walks fewer paths than exist reports a percentage of a number it
	// never states, which is how 22/47 got written down as a result.
	if walked != len(surfaceRegistryPaths()) {
		t.Fatalf("probe walked %d paths but the registry declares %d\n"+
			"  consequence: the percentage below is a fraction of the wrong "+
			"denominator, and looks like a measurement either way.",
			walked, len(surfaceRegistryPaths()))
	}

	got := len(implemented)
	total := walked

	wantImpl, wantTotal := readmeCapabilityClaim(t)

	if got != wantImpl || total != wantTotal {
		t.Errorf("the README claims %d of %d declared capabilities are wired; the binary does %d of %d.\n"+
			"  wired:     %v\n  not wired: %v\n"+
			"  consequence: the first number a reader meets is wrong. If the "+
			"README overstates, it promises commands that refuse them; if it "+
			"understates, finished work is invisible.\n"+
			"  fix: update every occurrence in README.md (the figure appears "+
			"more than once, including a table row and the percentage in prose).",
			wantImpl, wantTotal, got, total, implemented, notImplemented)
	}

	// The percentage is derived here rather than trusted, because the README
	// spells it out separately from the fraction and the two can disagree.
	if pct := percentString(got, total); !readmeMentions(t, pct) {
		t.Errorf("the measured coverage is %s but README.md never says so\n"+
			"  consequence: the fraction and the percentage are written down "+
			"separately, so one can be corrected while the other keeps the old "+
			"claim -- and a stale percentage reads as authoritative.", pct)
	}
}

// TestEveryDeclaredPathAnswersAboutItselfOneWayOrTheOther is the other half.
//
// The count above says how many are wired. It says nothing about what the other
// 25 do, and "unknown command" for a command `arxi surface` publishes is the
// worst answer the binary can give: it sends the user hunting for a typo they
// did not make. That promise is made in the README, in the usage screen, and in
// main.go's comment, so it is worth one test that no declared path can break it.
func TestEveryDeclaredPathAnswersAboutItselfOneWayOrTheOther(t *testing.T) {
	dir := workdir(t)
	for _, path := range surfaceRegistryPaths() {
		joined := strings.Join(path, " ")
		if blockingPaths[joined] {
			continue
		}
		r := arxi(t, dir, path...)
		for _, bad := range []string{
			"does not exist in the surface",
			"unknown command",
		} {
			if strings.Contains(strings.ToLower(r.out), bad) {
				t.Errorf("arxi %s is declared in the registry but the binary answers %q:\n%s\n"+
					"  consequence: the user is told the command does not exist "+
					"when `arxi surface` lists it. They will look for a spelling "+
					"mistake instead of learning it is not built yet.",
					joined, bad, r.out)
			}
		}
	}
}

// probeSurface invokes every declared path and sorts it into wired or not.
func probeSurface(t *testing.T, dir string) (implemented, notImplemented []string, walked int) {
	t.Helper()

	for _, path := range surfaceRegistryPaths() {
		walked++
		joined := strings.Join(path, " ")

		// A path that never returns is counted from the exception list, and it
		// is counted -- not skipped -- so the denominator stays whole.
		if blockingPaths[joined] {
			implemented = append(implemented, joined)
			continue
		}

		r := arxi(t, dir, path...)
		if strings.Contains(r.out, notImplementedSentinel) {
			notImplemented = append(notImplemented, joined)
			continue
		}
		implemented = append(implemented, joined)
	}
	return implemented, notImplemented, walked
}

// surfaceRegistryPaths returns every declared path, in registry order.
//
// Copied out of the registry rather than hand-listed, and it returns fresh
// slices rather than the registry's own: handing back c.Path would let a caller
// append to the registry's backing array, and one caller below does append to
// build an argv. That is a real bug and not a theoretical one -- it is the same
// aliasing that any `append(c.Path, "--help")` invites.
func surfaceRegistryPaths() [][]string {
	out := make([][]string, 0, len(surface.Registry))
	for i := range surface.Registry {
		p := make([]string, len(surface.Registry[i].Path))
		copy(p, surface.Registry[i].Path)
		out = append(out, p)
	}
	return out
}

// readmeClaimRe finds "<impl> of <total>" or "<impl> / <total>" wherever the
// README states it.
var readmeClaimRe = regexp.MustCompile(`(\d+)\s*(?:of|/)\s*(\d+) declared capabilities`)

// readmeCapabilityClaim reads the claim out of README.md.
//
// Reading the document rather than hardcoding the pair is the point: a
// hardcoded expectation makes this test agree with itself while the README says
// something else, which is the exact failure it exists to catch.
func readmeCapabilityClaim(t *testing.T) (impl, total int) {
	t.Helper()

	all := readmeClaimRe.FindAllStringSubmatch(readmeText(t), -1)
	if len(all) == 0 {
		t.Fatalf("README.md no longer states the capability count in a form this test can read (%q)\n"+
			"  the claim is expected to read like \"<impl> of <total> declared capabilities\".\n"+
			"  consequence: if the sentence was reworded, this guard stops "+
			"checking it and the figure is unverified again -- silently, which "+
			"is how it went stale before. Update the pattern with the wording.",
			readmeClaimRe)
	}

	// Every statement of the claim must agree, and the pattern is matched
	// ALL-wise rather than first-wise to find out.
	//
	// FindStringSubmatch would return the first and say nothing about the rest,
	// which is a coin flip dressed as a measurement: the figure is written in
	// several places in this document, and the failure being guarded against is
	// precisely that one of them gets corrected and another does not. Checking
	// the first match would then pass or fail depending on which one happens to
	// appear earlier in the file.
	//
	// Today there is exactly one match, and README.md line 93 -- "There are
	// **49 declared capabilities**" -- is one small edit away from being a
	// second. Refusing an ambiguity rather than resolving it by position is the
	// same rule toolrun.Edit applies to a string that occurs more than once,
	// and for the same reason: "the first one" is a choice the reader cannot
	// see.
	m := all[0]
	for _, other := range all[1:] {
		if other[1] != m[1] || other[2] != m[2] {
			t.Fatalf("README.md states the capability count more than once and the statements disagree: %q vs %q\n"+
				"  consequence: whichever one this test happened to read first "+
				"would decide whether it passes. One of them is stale; both "+
				"need the measured figure.",
				m[0], other[0])
		}
	}

	impl, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("reading the implemented count from README.md: %v", err)
	}
	total, err = strconv.Atoi(m[2])
	if err != nil {
		t.Fatalf("reading the declared total from README.md: %v", err)
	}
	return impl, total
}

func readmeMentions(t *testing.T, s string) bool {
	t.Helper()
	return strings.Contains(readmeText(t), s)
}

// readmeText reads README.md from the repository root.
//
// The test binary runs in cmd/arxi, so the path is relative to there. It is
// checked with an explicit error rather than ignored, because a missing README
// must not read as an empty one -- that would make every "does the README say
// X" assertion below answer no, for a reason that has nothing to do with X.
func readmeText(t *testing.T) string {
	t.Helper()

	path := filepath.Join("..", "..", "README.md")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v\n  this test verifies the README's measured "+
			"claims against the binary, and cannot do that without it.", path, err)
	}
	return string(b)
}

// percentString formats coverage the way the README writes it: one decimal.
func percentString(n, total int) string {
	if total == 0 {
		return "0.0%"
	}
	return strconv.FormatFloat(100*float64(n)/float64(total), 'f', 1, 64) + "%"
}
