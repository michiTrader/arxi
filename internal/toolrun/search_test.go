package toolrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// put writes a file inside the workspace, creating parents.
//
// Deliberately NOT w.WriteFile: a fixture built with the code under test cannot
// distinguish "the fixture was never created" from "the tool refused to read
// it", and the symlink and binary cases below need files WriteFile would refuse
// to make.
func put(t *testing.T, w *Workspace, rel, content string) string {
	t.Helper()
	full := filepath.Join(w.Root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", rel, err)
	}
	return full
}

func TestGrepMatchesARegularExpressionRatherThanASubstring(t *testing.T) {
	w := ws(t)
	put(t, w, "a.go", "package a\n\nfunc Hello() {}\nfunc Bye() {}\n")

	got, _, err := w.Grep(`func \w+\(`, "")
	if err != nil {
		t.Fatalf(`Grep("func \\w+\\("): %v`, err)
	}
	if len(got) != 2 {
		t.Fatalf("a regexp pattern found %d matches, want 2\n"+
			"  a model told it has grep will send `func \\w+\\(` sooner or later; "+
			"matching that as a literal substring finds nothing, reports success, "+
			"and teaches the model the file does not contain what it does contain\n"+
			"  got: %v", len(got), got)
	}

	// The other half of the same guarantee: the LITERAL text of that pattern is
	// not in the file. Without this, an implementation that tried substring
	// first and regexp second would still pass the assertion above.
	none, _, err := w.Grep(`func \\w\+\\\(`, "")
	if err != nil {
		t.Fatalf("Grep(escaped): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("the escaped pattern matched %d lines, want 0: %v", len(none), none)
	}
}

func TestAPatternThatDoesNotCompileIsRefusedBeforeTheWalk(t *testing.T) {
	w := ws(t)
	put(t, w, "a.go", "func Hello() {}\n")

	_, _, err := w.Grep("func((", "")
	if err == nil {
		t.Fatal("an uncompilable pattern was accepted\n" +
			"  reported as \"no matches\" it is a wrong answer delivered confidently: " +
			"the caller cannot tell a broken pattern from an absent string")
	}
	if !strings.Contains(err.Error(), "regular expression") {
		t.Errorf("refusal does not say the pattern is a regular expression: %v", err)
	}
	// The escaped form is the fix for the caller that meant the text literally,
	// and it is the only thing in the message it can act on.
	if !strings.Contains(err.Error(), `func\(\(`) {
		t.Errorf("refusal does not show the escaped form: %v", err)
	}
}

func TestAnEmptyPatternIsRefusedRatherThanMatchingEveryLine(t *testing.T) {
	w := ws(t)
	put(t, w, "a.go", "func Hello() {}\n")

	if _, _, err := w.Grep("   ", ""); err == nil {
		t.Fatal("a blank pattern was accepted\n" +
			"  it matches every line of every file, which dumps the workspace into " +
			"the run log instead of searching it")
	}
}

// One of the two defects found by probing the first implementation.
//
// Resolve permits a final component that does not exist yet, which is right for
// a WRITE and wrong for a search; WalkDir on a missing root calls the callback
// once with an error, which Grep deliberately swallows so one unreadable subtree
// cannot fail a whole grep. Together they turned a search that never happened
// into "no matches for func".
func TestASearchOfAPathThatDoesNotExistIsRefusedRatherThanReportedAsNoMatches(t *testing.T) {
	w := ws(t)
	// Asserted setup: the pattern really IS present in the workspace, so a
	// scoped search reporting nothing cannot be dismissed as a true negative.
	// Without this the test would also pass against an empty workspace.
	put(t, w, "a.go", "func Hello() {}\n")
	if found, _, err := w.Grep("func", ""); err != nil || len(found) != 1 {
		t.Fatalf("setup: unscoped Grep found %d matches (err %v), want 1", len(found), err)
	}

	got, _, err := w.Grep("func", "nope-dir")
	if err == nil {
		t.Fatalf("searching a directory that does not exist succeeded with %d matches\n"+
			"  \"no matches\" is a claim about a directory that was never opened, and "+
			"the caller will conclude the string is absent from it", len(got))
	}
	if !strings.Contains(err.Error(), "nope-dir") {
		t.Errorf("refusal does not name the path: %v", err)
	}
}

// The second probed defect. A symlink is refused rather than followed OR
// skipped: following puts whatever it points at in scope, skipping is the
// silent empty answer again.
func TestASearchPathThatIsASymlinkIsRefusedRatherThanFollowedOrSkipped(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("PASSWORD=hunter2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	w := ws(t)
	if err := os.Symlink(outside, filepath.Join(w.Root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// Asserted setup, read OUTSIDE the code under test: the target really does
	// contain the pattern. If this tool ever starts following the link, the
	// assertion below fails on a real leak rather than on an empty directory.
	raw, err := os.ReadFile(filepath.Join(outside, "secret.txt"))
	if err != nil || !strings.Contains(string(raw), "PASSWORD") {
		t.Fatalf("setup: target file does not contain the pattern (err %v)", err)
	}

	got, _, err := w.Grep("PASSWORD", "escape")
	if err == nil {
		t.Fatalf("searching through a symlink succeeded with %d matches\n"+
			"  either it followed the link out of the workspace, which is the "+
			"boundary this package exists to hold, or it skipped it and called "+
			"that an answer", len(got))
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("refusal does not say the reason is a symlink: %v", err)
	}
}

// TestASymlinkedDirectoryInsideTheWorkspaceIsNotWalkedOutOfIt guards the
// outcome, and deliberately does not care which layer produces it.
//
// Breaking Grep's own symlink skip does NOT make this fail, and that is not a
// weakness in the test -- it is a fact about the code that the test found. Two
// measurements explain it: filepath.WalkDir never descends into a symlinked
// directory (it yields the link as a single non-directory entry), and ReadFile
// refuses that entry in the kernel with O_NOFOLLOW. Grep's skip is an
// optimisation on top of both.
//
// The finding is recorded here because the first version of Grep's doc comment
// credited the skip with holding the boundary. A test written to fail when the
// skip is removed would have agreed with that comment and pinned an
// optimisation as though it were a control, pointing the next reader at the
// wrong line to be careful with. What must never change is that PASSWORD from
// outside does not appear in the result, and that is what is asserted.
func TestASymlinkedDirectoryInsideTheWorkspaceIsNotWalkedOutOfIt(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("PASSWORD=hunter2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	w := ws(t)
	put(t, w, "own.txt", "PASSWORD=mine\n")
	if err := os.Symlink(outside, filepath.Join(w.Root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// A whole-workspace grep must not be made useless by one stray link -- which
	// is why the walk SKIPS here where the named-path case refuses -- and must
	// not report what is behind it either.
	got, _, err := w.Grep("PASSWORD", "")
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	if len(got) != 1 || got[0].Path != "own.txt" {
		t.Fatalf("whole-workspace grep returned %v\n"+
			"  want exactly own.txt: a link to a tree outside the workspace must "+
			"not put that tree in scope, and must not stop the rest of the search", got)
	}
}

func TestGrepScopesToASubdirectoryWhenOneIsNamed(t *testing.T) {
	w := ws(t)
	put(t, w, "a.go", "func Top() {}\n")
	put(t, w, "sub/b.go", "func Deep() {}\n")

	got, _, err := w.Grep("func", "sub")
	if err != nil {
		t.Fatalf("Grep(sub): %v", err)
	}
	if len(got) != 1 || got[0].Path != filepath.Join("sub", "b.go") {
		t.Fatalf("scoped grep returned %v, want only sub/b.go", got)
	}
	// Paths are reported relative to the WORKSPACE, not to the scoped root: a
	// caller that greps `sub` and then edits `b.go` would be editing a file at
	// the wrong level.
	if got[0].Line != 1 {
		t.Errorf("line number %d, want 1", got[0].Line)
	}
}

func TestASingleFileIsASearchableRoot(t *testing.T) {
	w := ws(t)
	put(t, w, "a.go", "func Hello() {}\n")
	put(t, w, "b.go", "func Other() {}\n")

	// `grep pattern path=a.go` has one obvious reading, and refusing it because
	// the path is not a directory fails a turn over a distinction the caller has
	// no reason to make.
	got, _, err := w.Grep("func", "a.go")
	if err != nil {
		t.Fatalf("Grep(a.go): %v", err)
	}
	if len(got) != 1 || got[0].Path != "a.go" {
		t.Errorf("single-file grep returned %v, want only a.go", got)
	}
}

func TestAPathClimbingOutOfTheWorkspaceIsRefused(t *testing.T) {
	w := ws(t)
	put(t, w, "a.go", "func Hello() {}\n")

	if _, _, err := w.Grep("func", filepath.Join("..", "..")); err == nil {
		t.Fatal("a search path outside the workspace was accepted\n" +
			"  the scoped path goes through the same gate as a file path, or the " +
			"confinement has a second door")
	}
}

func TestABinaryFileIsSkippedRatherThanMatched(t *testing.T) {
	w := ws(t)
	put(t, w, "readable.txt", "func Hello\n")
	put(t, w, "blob.bin", "func \x00\x00 func Hello\n")

	got, _, err := w.Grep("func", "")
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	for _, m := range got {
		if m.Path == "blob.bin" {
			t.Fatalf("matched inside a binary file: %v\n"+
				"  the match text becomes an event in the run log, and bytes nobody "+
				"can read are the one artefact this project must not produce", m)
		}
	}
	if len(got) != 1 {
		t.Errorf("got %d matches, want 1 (readable.txt only): %v", len(got), got)
	}
}

func TestTheBinaryTestIsTheFirstEightKilobytesAndSaysSo(t *testing.T) {
	// The heuristic is git's: a NUL in the first 8 KiB. Pinning the boundary
	// makes a later "scan the whole file" change a visible decision rather than
	// a silent one.
	if isBinary([]byte(strings.Repeat("a", 9<<10) + "\x00")) {
		t.Error("a NUL past 8 KiB was treated as binary, but the documented test " +
			"is the first 8 KiB")
	}
	if !isBinary([]byte("ab\x00cd")) {
		t.Error("a NUL in the first bytes was not treated as binary")
	}
}

func TestGrepStopsAtTheMatchCapAndSaysSo(t *testing.T) {
	w := ws(t)
	put(t, w, "many.txt", strings.Repeat("needle\n", maxMatches+50))

	got, truncated, err := w.Grep("needle", "")
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	if len(got) > maxMatches {
		t.Errorf("returned %d matches, above the cap of %d", len(got), maxMatches)
	}
	if !truncated {
		t.Fatalf("hit the cap at %d matches without reporting truncation\n"+
			"  a caller shown 200 of 250 matches and told nothing reads the list "+
			"as complete", len(got))
	}
	if out := formatGrep(got, truncated, "needle"); !strings.Contains(out, "stopped at") {
		t.Errorf("rendered output does not mention stopping:\n%s", out)
	}
}

func TestAGrepThatMatchesNothingStillStopsWalking(t *testing.T) {
	// maxGrepFiles exists because maxMatches cannot prevent this: a pattern that
	// matches nothing produces no matches to cap, and would walk every file in
	// a workspace somebody unpacked a dependency tree into.
	w := ws(t)
	for i := 0; i < maxGrepFiles+100; i++ {
		put(t, w, fmt.Sprintf("d%04d/f.txt", i), "haystack\n")
	}

	got, truncated, err := w.Grep("needle", "")
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d matches for a pattern present in no file", len(got))
	}
	if !truncated {
		t.Error("walked past maxGrepFiles without reporting a stop\n" +
			"  a cap on matches alone lets a no-match grep spend unbounded time")
	}
}

func TestGrepResultsAreInTheSameOrderEveryTime(t *testing.T) {
	w := ws(t)
	put(t, w, "b/2.txt", "needle\nneedle\n")
	put(t, w, "a/1.txt", "needle\n")
	put(t, w, "c.txt", "needle\n")

	found, _, err := w.Grep("needle", "")
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	var got []string
	for _, m := range found {
		got = append(got, fmt.Sprintf("%s:%d", filepath.ToSlash(m.Path), m.Line))
	}
	want := []string{"a/1.txt:1", "b/2.txt:1", "b/2.txt:2", "c.txt:1"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v\n"+
			"  a model diffing two greps needs the same input to give the same "+
			"output, and WalkDir's cross-directory interleaving is not that", got, want)
	}
}

func TestNoMatchesIsStatedAsAResultRatherThanAsSilence(t *testing.T) {
	out := formatGrep(nil, false, "needle")
	if strings.TrimSpace(out) == "" {
		t.Fatal("a grep with no matches rendered as an empty string\n" +
			"  a model cannot tell that apart from a tool that failed to run, and " +
			"will retry a search that worked")
	}
	if !strings.Contains(out, "needle") {
		t.Errorf("the result does not repeat the pattern it answers for: %q", out)
	}
}

func TestGrepOutputUsesGrepsOwnFormatAndKeepsIndentation(t *testing.T) {
	out := formatGrep([]grepMatch{{Path: "a.go", Line: 7, Text: "\tif err != nil {   "}}, false, "err")
	if !strings.Contains(out, "a.go:7:\tif err != nil {") {
		t.Errorf("output is not path:line:text with leading indentation intact:\n%q\n"+
			"  that indentation is exactly what a later edit has to reproduce to "+
			"match", out)
	}
	if strings.Contains(out, "{   \n") {
		t.Error("trailing whitespace survived the render")
	}
}

func TestEditReplacesOneOccurrenceAndReportsTheCount(t *testing.T) {
	w := ws(t)
	put(t, w, "a.go", "package a\n\nfunc Hello() {}\n")

	n, err := w.Edit("a.go", "Hello", "Greet", false)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if n != 1 {
		t.Errorf("count = %d, want 1", n)
	}
	got, err := w.ReadFile("a.go")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if want := "package a\n\nfunc Greet() {}\n"; string(got) != want {
		t.Errorf("file = %q, want %q", got, want)
	}
}

func TestAnEditThatMatchesNothingIsRefusedAndLeavesTheFileAlone(t *testing.T) {
	w := ws(t)
	const before = "package a\n\nfunc Hello() {}\n"
	put(t, w, "a.go", before)

	_, err := w.Edit("a.go", "Goodbye", "Greet", false)
	if err == nil {
		t.Fatal("an edit that matched nothing reported success\n" +
			"  the caller then believes the file says something it does not, and " +
			"every later step is built on that belief")
	}
	// The refusal quotes what was not found because the usual cause is
	// whitespace: a caller that rebuilt the line from earlier output has the
	// indentation wrong, and "not found" alone sends it to re-read the file
	// rather than to look at what it sent.
	if !strings.Contains(err.Error(), `"Goodbye"`) {
		t.Errorf("refusal does not quote the string that was not found: %v", err)
	}
	if !strings.Contains(err.Error(), "whitespace") {
		t.Errorf("refusal does not point at the usual cause: %v", err)
	}

	after, rerr := w.ReadFile("a.go")
	if rerr != nil {
		t.Fatalf("ReadFile: %v", rerr)
	}
	if string(after) != before {
		t.Errorf("a refused edit still changed the file: %q", after)
	}
}

func TestAnAmbiguousEditIsRefusedAndSaysHowMany(t *testing.T) {
	w := ws(t)
	const before = "func A() {}\nfunc B() {}\nfunc C() {}\n"
	put(t, w, "a.go", before)

	_, err := w.Edit("a.go", "func", "FUNC", false)
	if err == nil {
		t.Fatal("an ambiguous edit was applied\n" +
			"  \"the first one\" is a position the caller cannot see, so which " +
			"occurrence got edited would be luck")
	}
	// The COUNT is the fact the caller needs in order to choose between
	// narrowing the match and asking for all of them. A refusal without it just
	// says no.
	if !strings.Contains(err.Error(), "3 occurrences") {
		t.Errorf("refusal does not report how many were found: %v", err)
	}
	if !strings.Contains(err.Error(), "all=true") {
		t.Errorf("refusal does not name the way to proceed: %v", err)
	}

	after, _ := w.ReadFile("a.go")
	if string(after) != before {
		t.Errorf("a refused edit still changed the file: %q", after)
	}
}

func TestAnAmbiguousEditProceedsWhenAllIsAsked(t *testing.T) {
	w := ws(t)
	put(t, w, "a.go", "func A() {}\nfunc B() {}\nfunc C() {}\n")

	n, err := w.Edit("a.go", "func", "FUNC", true)
	if err != nil {
		t.Fatalf("Edit(all): %v", err)
	}
	if n != 3 {
		t.Errorf("count = %d, want 3", n)
	}
	after, _ := w.ReadFile("a.go")
	if strings.Contains(string(after), "func") {
		t.Errorf("all=true left an occurrence behind: %q", after)
	}
}

func TestAMissingReplacementIsADeletionRatherThanAnError(t *testing.T) {
	w := ws(t)
	put(t, w, "a.go", "keep\nDELETE ME\nkeep\n")

	// Same reasoning as write's missing content: "remove this" is a real
	// instruction, and absent and empty are the same request.
	if _, err := w.Edit("a.go", "DELETE ME\n", "", false); err != nil {
		t.Fatalf("Edit with an empty replacement: %v", err)
	}
	got, _ := w.ReadFile("a.go")
	if want := "keep\nkeep\n"; string(got) != want {
		t.Errorf("file = %q, want %q", got, want)
	}
}

// TestAnEmptyStringToReplaceIsRefused covers all=true as well as all=false, and
// the second case is the one that matters.
//
// The first version of this test only passed all=false, and removing the empty-
// old guard did NOT make it fail: with all=false the ambiguity check fires
// first, because an empty string "occurs" at every position and n > 1. The
// guard looked tested and was not.
//
// With all=true nothing else stands in the way. Measured with the guard
// removed: Edit("abc\n", old="", new="X", all=true) returned 5 replacements,
// no error, and left the file as "XaXbXcX\nX" -- every character separated by
// an insertion, reported as a success.
func TestAnEmptyStringToReplaceIsRefused(t *testing.T) {
	for _, all := range []bool{false, true} {
		w := ws(t)
		const before = "abc\n"
		put(t, w, "a.go", before)

		if _, err := w.Edit("a.go", "", "X", all); err == nil {
			t.Errorf("an empty match was accepted with all=%v\n"+
				"  it is at every position in the file, so the result is an "+
				"insertion between every character rather than an edit", all)
		}
		if after, _ := w.ReadFile("a.go"); string(after) != before {
			t.Errorf("all=%v: the file was rewritten to %q", all, after)
		}
	}
}

func TestEditingAMissingFileFailsWithoutCreatingIt(t *testing.T) {
	w := ws(t)

	if _, err := w.Edit("gone.go", "a", "b", false); err == nil {
		t.Fatal("editing a file that does not exist succeeded")
	}
	if _, err := os.Lstat(filepath.Join(w.Root, "gone.go")); err == nil {
		t.Error("a failed edit created the file\n" +
			"  edit replaces text in a file the caller believes exists; creating it " +
			"is write's job, and doing both hides a wrong path")
	}
}

func TestEditOutsideTheWorkspaceIsRefusedAndLandsNowhere(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "victim.txt")
	if err := os.WriteFile(outside, []byte("original\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	w := ws(t)
	if _, err := w.Edit(outside, "original", "tampered", false); err == nil {
		t.Fatal("an edit outside the workspace was accepted")
	}
	if got, _ := os.ReadFile(outside); string(got) != "original\n" {
		t.Errorf("the file outside the workspace changed: %q", got)
	}
}

func TestGrepAndEditReachTheirImplementationsThroughTheRunner(t *testing.T) {
	r := runner(t)
	if _, err := r.RunTool(context.Background(), "backend", "write",
		map[string]any{"path": "a.go", "content": "func A() {}\nfunc B() {}\n"}); err != nil {
		t.Fatalf("write: %v", err)
	}

	out, err := r.RunTool(context.Background(), "backend", "grep", map[string]any{"pattern": `func \w`})
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if !strings.Contains(out, "a.go:1:func A() {}") {
		t.Errorf("grep through the runner returned:\n%s", out)
	}

	out, err = r.RunTool(context.Background(), "backend", "edit",
		map[string]any{"path": "a.go", "old": "func A", "new": "func Z"})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	// The count is in the reply, not just "ok": an edit that touched 7 lines
	// when the caller expected 1 is a fact, and this is the only place it can
	// learn it.
	if !strings.Contains(out, "1 replacement") {
		t.Errorf("edit reply does not report the count: %q", out)
	}
}

func TestTheRunnerAcceptsTheOtherSpellingsOfTheseArguments(t *testing.T) {
	r := runner(t)
	if _, err := r.RunTool(context.Background(), "backend", "write",
		map[string]any{"path": "a.go", "content": "alpha\n"}); err != nil {
		t.Fatalf("write: %v", err)
	}

	// A model emitting `query` for grep or `old_string` for edit is emitting a
	// spelling some other tool taught it. Refusing it fails a turn over
	// vocabulary rather than over intent.
	if _, err := r.RunTool(context.Background(), "backend", "grep",
		map[string]any{"query": "alpha", "directory": "."}); err != nil {
		t.Errorf("grep with query/directory: %v", err)
	}
	if _, err := r.RunTool(context.Background(), "backend", "edit",
		map[string]any{"file": "a.go", "old_string": "alpha", "new_string": "beta"}); err != nil {
		t.Errorf("edit with file/old_string/new_string: %v", err)
	}
}

func TestAllIsAcceptedAsATrueStringButNotAsLooserVocabulary(t *testing.T) {
	r := runner(t)
	reset := func() {
		t.Helper()
		if _, err := r.RunTool(context.Background(), "backend", "write",
			map[string]any{"path": "a.go", "content": "x\nx\n"}); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	// A model that emits arguments as text is normal, so "true" is honoured.
	reset()
	out, err := r.RunTool(context.Background(), "backend", "edit",
		map[string]any{"path": "a.go", "old": "x", "new": "y", "all": "true"})
	if err != nil {
		t.Fatalf(`all:"true": %v`, err)
	}
	if !strings.Contains(out, "2 replacements") {
		t.Errorf(`all:"true" did not go global: %q`, out)
	}

	// "yes" is NOT honoured, and the tell is that the ambiguity refusal fires.
	// Anything looser than true turns vague vocabulary into an edit nobody
	// asked for.
	reset()
	if _, err := r.RunTool(context.Background(), "backend", "edit",
		map[string]any{"path": "a.go", "old": "x", "new": "y", "all": "yes"}); err == nil {
		t.Error("all:\"yes\" was read as true\n" +
			"  an ambiguous edit went ahead on a word the caller never defined")
	}

	// A real bool is the primary spelling and must still work.
	reset()
	if out, err := r.RunTool(context.Background(), "backend", "edit",
		map[string]any{"path": "a.go", "old": "x", "new": "y", "all": true}); err != nil {
		t.Errorf("all:true (bool): %v", err)
	} else if !strings.Contains(out, "2 replacements") {
		t.Errorf("all:true (bool) did not go global: %q", out)
	}
}

func TestGrepWithoutAPatternIsRefusedByTheRunner(t *testing.T) {
	r := runner(t)
	_, err := r.RunTool(context.Background(), "backend", "grep", map[string]any{"path": "."})
	if err == nil {
		t.Fatal("grep with no pattern succeeded\n" +
			"  there is an obvious default for WHERE to look and none for WHAT to " +
			"look for")
	}
	if !strings.Contains(err.Error(), "grep") {
		t.Errorf("refusal does not name the tool: %v", err)
	}
}
