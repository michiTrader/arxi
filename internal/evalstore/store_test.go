package evalstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michiTrader/iash/internal/eval"
)

// run builds a storable summary. Each test varies only what it is about.
func run(id, suite, sha string) *eval.RunSummary {
	return &eval.RunSummary{
		ID:        id,
		Suite:     suite,
		SuiteSHA:  sha,
		BudgetUSD: 1.00,
		StartedAt: "2026-01-01T00:00:00Z",
		Results: []eval.Result{
			{Case: "one", Status: eval.StatusPass, CostUSD: 0.1, Turns: 1},
			{Case: "two", Status: eval.StatusFail, Why: "missing X", CostUSD: 0.1, Turns: 1},
		},
	}
}

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), DefaultDir))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return s
}

func TestARunSurvivesBeingWrittenAndReadBack(t *testing.T) {
	s := open(t)
	want := run("e1", "review-quality", "abc123")
	if err := s.Put(want); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.Load("e1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Through the totals, because the totals are what gets read off a run. A
	// round trip preserving every field but changing the pass rate is not one.
	if got.Totals() != want.Totals() {
		t.Errorf("the totals changed on disk.\n got: %+v\nwant: %+v",
			got.Totals(), want.Totals())
	}
	if got.SuiteSHA != "abc123" {
		t.Errorf("the suite digest did not survive storage, and it is the whole "+
			"reason compare can tell two questions apart; got %q", got.SuiteSHA)
	}
}

func TestAStoredRunIsNotReplaced(t *testing.T) {
	s := open(t)
	first := run("e1", "review-quality", "abc123")
	if err := s.Put(first); err != nil {
		t.Fatalf("put: %v", err)
	}

	// A DIFFERENT run under the same id: same suite, opposite results. This is
	// the shape of the accident -- rerunning a suite twice in the same second,
	// or a future writer that mints ids from something coarser than it thinks.
	second := run("e1", "review-quality", "abc123")
	second.Results[0].Status = eval.StatusFail
	second.Results[1].Status = eval.StatusFail

	err := s.Put(second)
	if err == nil {
		t.Fatal("a stored run was silently replaced.\n" +
			"  yesterday's quoted table is now unreproducible while every id in " +
			"it still resolves: the citation keeps working and stops being true")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("the refusal should say the run already exists.\ngot: %v", err)
	}

	// And the original must be intact, not half-overwritten by the attempt.
	back, err := s.Load("e1")
	if err != nil {
		t.Fatalf("load after refused put: %v", err)
	}
	if rate, ok := back.Totals().PassRate(); !ok || rate != 0.5 {
		t.Errorf("the refused write damaged the stored run: pass rate %v (ok=%v), "+
			"want 0.50", rate, ok)
	}
}

func TestARunIDThatCollidesOnlyOnACaseInsensitiveFilesystemIsRefused(t *testing.T) {
	s := open(t)
	if err := s.Put(run("e1", "s", "abc")); err != nil {
		t.Fatalf("put: %v", err)
	}
	err := s.Put(run("E1", "s", "abc"))
	if err == nil {
		t.Fatal(`storing "E1" beside "e1" was accepted.` + "\n" +
			"  macOS and Windows filesystems are case-insensitive, so this " +
			"succeeds here and DESTROYS EVIDENCE on a laptop -- the worst kind " +
			"of difference, since the machine where it is destructive is the " +
			"one with no tests running on it")
	}
	if !strings.Contains(err.Error(), "collides") {
		t.Errorf("the refusal should name the collision and the existing id.\ngot: %v", err)
	}
}

func TestAMeaninglessRunNeverBecomesAFile(t *testing.T) {
	s := open(t)
	bad := run("e1", "s", "") // no suite digest
	if err := s.Put(bad); err == nil {
		t.Fatal("a run with no suite digest was stored")
	}
	// The point: it was rejected BEFORE anything was written, so a failed Put
	// does not leave a temp file or a partial run behind for List to trip over.
	ids, err := s.IDs()
	if err != nil {
		t.Fatalf("ids: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("a refused Put left something in the directory: %v", ids)
	}
	entries, _ := os.ReadDir(s.Dir())
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("a refused Put left files behind: %v", names)
	}
}

func TestARunThatIsNotThereIsNamedAlongWithHowToSeeWhatIs(t *testing.T) {
	s := open(t)
	_, err := s.Load("e9")
	if err == nil {
		t.Fatal("loading a run that does not exist succeeded")
	}
	// The id AND the recovery. "no such file or directory" sends the reader
	// hunting a path; the useful reply is what runs there actually are.
	if !strings.Contains(err.Error(), "e9") {
		t.Errorf("the error should name the run asked for.\ngot: %v", err)
	}
	if !strings.Contains(err.Error(), "eval list") {
		t.Errorf("the error should say how to see what runs exist, which is "+
			"nearly always the next thing wanted.\ngot: %v", err)
	}
}

func TestAFileWhoseIDDisagreesWithItsNameIsRefused(t *testing.T) {
	s := open(t)
	// e1.json holding a run that calls itself e2. Reachable by copying a run
	// file to a new name, which is exactly what somebody does to "keep a
	// baseline".
	body, err := run("e2", "s", "abc").Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := os.WriteFile(s.Path("e1"), body, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err = s.Load("e1")
	if err == nil {
		t.Fatal("a run addressable as e1 that reports itself as e2 was accepted.\n" +
			"  `eval compare e1 e2` would print a table headed by ids that are " +
			"not the ones asked for, which is a citation pointing at the wrong " +
			"evidence")
	}
	if !strings.Contains(err.Error(), "e2") || !strings.Contains(err.Error(), "e1") {
		t.Errorf("the error should name both the id asked for and the id "+
			"found.\ngot: %v", err)
	}
}

func TestListIsNewestFirst(t *testing.T) {
	s := open(t)
	// Stored out of order, so the ordering cannot come from insertion order.
	for _, id := range []string{
		"e20260101T000000", "e20260301T000000", "e20260201T000000",
	} {
		if err := s.Put(run(id, "s", "abc")); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
	}
	got, err := s.IDs()
	if err != nil {
		t.Fatalf("ids: %v", err)
	}
	want := []string{"e20260301T000000", "e20260201T000000", "e20260101T000000"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("runs should be listed newest first, because they "+
				"accumulate without bound and the question asked of the list is "+
				"\"what did I just run, and what do I compare it against\".\n"+
				" got: %v\nwant: %v", got, want)
		}
	}
}

func TestListDoesNotReorderWhenAFileIsTouched(t *testing.T) {
	s := open(t)
	for _, id := range []string{"e20260101T000000", "e20260301T000000"} {
		if err := s.Put(run(id, "s", "abc")); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	// Rewriting the OLDER file makes it the most recently modified. If the
	// ordering came from mtime, copying a run into place -- or restoring a
	// backup -- would silently reorder the history.
	old := s.Path("e20260101T000000")
	body, err := os.ReadFile(old)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := os.WriteFile(old, body, 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	got, err := s.IDs()
	if err != nil {
		t.Fatalf("ids: %v", err)
	}
	if got[0] != "e20260301T000000" {
		t.Errorf("touching the older file reordered the list, so the order is "+
			"coming from mtime. mtime describes the FILE; the id describes the "+
			"RUN.\ngot: %v", got)
	}
}

func TestAnEmptyStoreListsNothingRatherThanFailing(t *testing.T) {
	// Not via open(), so the directory has never been created: the state on a
	// machine where `eval compare` is the first eval command ever typed.
	s := &Store{dir: filepath.Join(t.TempDir(), "never-created")}
	ids, err := s.IDs()
	if err != nil {
		t.Fatalf("a store with no directory should be no runs, not a failure: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("want no runs, got %v", ids)
	}
	runs, err := s.List()
	if err != nil {
		t.Fatalf("List on an absent directory: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("want no runs, got %d", len(runs))
	}
}

func TestAnUnreadableRunFailsTheWholeListing(t *testing.T) {
	s := open(t)
	if err := s.Put(run("e1", "s", "abc")); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := os.WriteFile(s.Path("e2"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := s.List()
	if err == nil {
		t.Fatal("a corrupt run was skipped and the listing succeeded.\n" +
			"  a list quietly missing a row looks exactly like a list of " +
			"everything that exists; refusing names the file, omitting names " +
			"nothing")
	}
	if !strings.Contains(err.Error(), "e2") {
		t.Errorf("the failure should name the file that could not be read.\ngot: %v", err)
	}
}

func TestAHalfWrittenRunIsNeverVisibleToTheListing(t *testing.T) {
	s := open(t)
	// A temp file of the shape Put's interrupted write leaves behind. It must
	// not end in .json, or a crash mid-run would make every subsequent
	// `eval list` fail on a file nobody can explain.
	tmp := filepath.Join(s.Dir(), "e1.json.tmp-12345")
	if err := os.WriteFile(tmp, []byte("{half"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	ids, err := s.IDs()
	if err != nil {
		t.Fatalf("a leftover temp file broke the listing: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("a temp file was listed as a run: %v", ids)
	}
}

func TestADirectoryEndingInJSONIsNotMistakenForARun(t *testing.T) {
	s := open(t)
	if err := os.Mkdir(filepath.Join(s.Dir(), "e1.json"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	ids, err := s.IDs()
	if err != nil {
		t.Fatalf("ids: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("a directory was listed as a run: %v", ids)
	}
}

func TestNonRunFilesInTheDirectoryAreIgnored(t *testing.T) {
	s := open(t)
	if err := s.Put(run("e1", "s", "abc")); err != nil {
		t.Fatalf("put: %v", err)
	}
	// A README beside the runs is a reasonable thing for somebody to add.
	for _, name := range []string{"README.md", ".DS_Store", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(s.Dir(), name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	ids, err := s.IDs()
	if err != nil {
		t.Fatalf("ids: %v", err)
	}
	if len(ids) != 1 || ids[0] != "e1" {
		t.Errorf("want just e1, got %v", ids)
	}
}

func TestListReturnsTheRunsThemselvesNotJustTheirNames(t *testing.T) {
	s := open(t)
	if err := s.Put(run("e1", "review-quality", "abc")); err != nil {
		t.Fatalf("put: %v", err)
	}
	runs, err := s.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("want 1 run, got %d", len(runs))
	}
	if runs[0].Suite != "review-quality" {
		t.Errorf("want the suite name carried through, got %q", runs[0].Suite)
	}
	if rate, ok := runs[0].Totals().PassRate(); !ok || rate != 0.5 {
		t.Errorf("want a readable pass rate off a listed run, got %v (ok=%v)", rate, ok)
	}
}

func TestOpenRefusesAnEmptyDirectoryAndNamesTheDefault(t *testing.T) {
	if _, err := Open("   "); err == nil {
		t.Fatal("Open accepted an empty directory, which would write runs into " +
			"whatever the process's working directory happens to be")
	} else if !strings.Contains(err.Error(), DefaultDir) {
		t.Errorf("the refusal should name the default so it can be used.\ngot: %v", err)
	}
}

func TestOpenCreatesTheDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b", "evals")
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.Put(run("e1", "s", "abc")); err != nil {
		t.Fatalf("put into a freshly created directory: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the directory was not created: %v", err)
	}
}

func TestAStoredRunIsReadableByAHuman(t *testing.T) {
	// Not cosmetic. These files are read by eye during exactly the argument
	// they exist to settle -- "did this change help" -- and a one-line document
	// is one a reader diffs by eye and gets wrong.
	s := open(t)
	if err := s.Put(run("e1", "s", "abc")); err != nil {
		t.Fatalf("put: %v", err)
	}
	body, err := os.ReadFile(s.Path("e1"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n := strings.Count(string(body), "\n"); n < 10 {
		t.Errorf("a stored run should be indented across many lines, got %d "+
			"newlines:\n%s", n, body)
	}
}

func TestASimulatedRunIsStoredAsSimulated(t *testing.T) {
	// The field exists BECAUSE runs persist. If it did not survive the round
	// trip, `compare` would pair fabricated numbers against measured ones and
	// report the difference as a change in quality.
	s := open(t)
	sim := run("e1", "s", "abc")
	sim.Simulated = true
	if err := s.Put(sim); err != nil {
		t.Fatalf("put: %v", err)
	}
	back, err := s.Load("e1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !back.Simulated {
		t.Fatal("a simulated run was stored and came back looking real")
	}
	// And it is visible in the file, for anyone reading it without the tool.
	body, _ := os.ReadFile(s.Path("e1"))
	if !strings.Contains(string(body), `"simulated": true`) {
		t.Errorf("the file should say plainly that it is simulated:\n%s", body)
	}
}

func TestPathAndDirAreWhatTheStoreActuallyUses(t *testing.T) {
	// Guards against Path drifting from where write() puts things, which would
	// make every error message in this package point at the wrong file.
	s := open(t)
	if err := s.Put(run("e1", "s", "abc")); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := os.Stat(s.Path("e1")); err != nil {
		t.Errorf("Path does not name the file Put wrote: %v", err)
	}
	if filepath.Dir(s.Path("e1")) != s.Dir() {
		t.Errorf("Path is not inside Dir: %q vs %q", s.Path("e1"), s.Dir())
	}
}

// --- FreeID ---------------------------------------------------------------
//
// FreeID exists because of a bug that a green test suite could not see: run
// ids are second-resolution timestamps, the CLI minted one before running the
// suite and only discovered the collision when storing afterwards, so the
// second eval in a second spent its whole budget and then had nowhere to go.

func TestAFreeIDIsHandedBackUnchanged(t *testing.T) {
	s := open(t)
	got, err := s.FreeID("e20260828T223406")
	if err != nil {
		t.Fatalf("free id: %v", err)
	}
	// Unchanged, because suffixing an id that nothing occupies would imply a
	// series to every later reader of the filename. Almost every run is the
	// only run of its second.
	if got != "e20260828T223406" {
		t.Fatalf("an unused id was renamed to %q", got)
	}
}

func TestATakenIDIsResolvedRatherThanRefused(t *testing.T) {
	s := open(t)
	if err := s.Put(run("e20260828T223406", "s", "sha")); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.FreeID("e20260828T223406")
	if err != nil {
		t.Fatalf("free id: %v", err)
	}
	// -2, not -1: the first run of that second is unsuffixed, so the second
	// one is the second, and the number says so.
	if got != "e20260828T223406-2" {
		t.Fatalf("want e20260828T223406-2, got %q", got)
	}
	// And the resolved name must actually be storable -- a helper that hands
	// out a name Put then rejects has moved the bug rather than fixed it.
	if err := s.Put(run(got, "s", "sha")); err != nil {
		t.Fatalf("the id FreeID offered was refused by Put: %v", err)
	}
}

func TestTheThirdRunOfASecondGetsTheThirdSuffix(t *testing.T) {
	s := open(t)
	for _, id := range []string{"e1", "e1-2"} {
		if err := s.Put(run(id, "s", "sha")); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
	}
	got, err := s.FreeID("e1")
	if err != nil {
		t.Fatalf("free id: %v", err)
	}
	// Not e1-2 again. This is the test that fails if FreeID checks only the
	// unsuffixed name and then returns a fixed suffix.
	if got != "e1-3" {
		t.Fatalf("want e1-3, got %q", got)
	}
}

func TestAnIDTakenOnlyInAnotherCaseIsNotOfferedAsFree(t *testing.T) {
	s := open(t)
	if err := s.Put(run("E1", "s", "sha")); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.FreeID("e1")
	if err != nil {
		t.Fatalf("free id: %v", err)
	}
	// "e1" and "E1" are one file on macOS and Windows, so offering "e1" here
	// would hand back a name that Put refuses -- routing the caller into the
	// collision error this function exists to prevent. Matching Put's own
	// case-insensitive check is what keeps the two consistent.
	if strings.EqualFold(got, "e1") {
		t.Fatalf("FreeID offered %q while %q is stored; on a case-insensitive "+
			"filesystem those are the same file", got, "E1")
	}
	if got != "e1-2" {
		t.Fatalf("want e1-2, got %q", got)
	}
}

func TestFreeIDDoesNotCreateAnything(t *testing.T) {
	s := open(t)
	if _, err := s.FreeID("e1"); err != nil {
		t.Fatalf("free id: %v", err)
	}
	// FreeID names a run; it does not reserve one. If it wrote a placeholder,
	// a run that was prepared and then abandoned -- a suite that fails to
	// load, a budget that refuses -- would leave an empty run in the listing,
	// and `eval list` would report evals that never happened.
	ids, err := s.IDs()
	if err != nil {
		t.Fatalf("ids: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("FreeID left %v behind; it is a suggestion, not a reservation", ids)
	}
}

func TestFreeIDOnADirectoryThatDoesNotExistYetStillAnswers(t *testing.T) {
	// The very first run of a fresh checkout calls FreeID before anything has
	// ever been stored, and Open creating the directory must not be the thing
	// that makes this work -- so this store points at a path that is not
	// there at all.
	s := &Store{dir: filepath.Join(t.TempDir(), "nope", "evals")}
	got, err := s.FreeID("e1")
	if err != nil {
		t.Fatalf("an empty store could not name the first run: %v", err)
	}
	if got != "e1" {
		t.Fatalf("want e1, got %q", got)
	}
}

// TestFreeIDLowercasesTheCandidateItChecksToo closes a gap mutation testing
// found: dropping the strings.ToLower around the SUFFIXED candidate killed no
// test, because every id the tests used was already lowercase, so lowering it
// was a no-op.
//
// It stops being a no-op the moment an id has an uppercase letter in it, and
// then the bug is the one FreeID exists to prevent: a name handed out that Put
// immediately refuses.
func TestFreeIDLowercasesTheCandidateItChecksToo(t *testing.T) {
	s := open(t)
	// Both the plain id and its -2 form are taken, and the stored -2 differs
	// from the generated candidate only in case.
	for _, id := range []string{"E1", "E1-2"} {
		if err := s.Put(run(id, "s", "sha")); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
	}
	got, err := s.FreeID("E1")
	if err != nil {
		t.Fatalf("free id: %v", err)
	}
	if strings.EqualFold(got, "E1-2") {
		t.Fatalf("FreeID offered %q while %q is stored: on a case-insensitive "+
			"filesystem that is the same file, and Put will refuse it", got, "E1-2")
	}
	// And the proof that it is usable, which is the whole point of the helper.
	if err := s.Put(run(got, "s", "sha")); err != nil {
		t.Fatalf("the id FreeID offered (%q) was refused by Put: %v", got, err)
	}
}

// TestFreeIDGivesUpRatherThanSpinning covers the exhaustion boundary that
// mutation testing exposed: flipping `n <= maxSuffix()` to `n <` killed no
// test, because reaching the limit meant storing a thousand runs. The limit is
// injectable for exactly this, so the path costs three files to exercise.
func TestFreeIDGivesUpRatherThanSpinning(t *testing.T) {
	s := open(t)
	s.maxN = 3 // so -2 and -3 exhaust the range
	for _, id := range []string{"e1", "e1-2", "e1-3"} {
		if err := s.Put(run(id, "s", "sha")); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
	}
	got, err := s.FreeID("e1")
	if err == nil {
		t.Fatalf("FreeID returned %q past its own limit; the bound is what "+
			"stops this from looping forever", got)
	}
	// The message has to say what is actually happening, because the true
	// cause is never "the store is full" -- it is a script generating runs.
	if !strings.Contains(err.Error(), "e1") {
		t.Errorf("the error should name the id it could not place: %v", err)
	}
}

// TestTheLastSuffixInRangeIsStillOffered is the other half of the boundary: an
// off-by-one that gave up one early would be silent, since the caller only
// sees "no free id" either way.
func TestTheLastSuffixInRangeIsStillOffered(t *testing.T) {
	s := open(t)
	s.maxN = 3
	for _, id := range []string{"e1", "e1-2"} {
		if err := s.Put(run(id, "s", "sha")); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
	}
	got, err := s.FreeID("e1")
	if err != nil {
		t.Fatalf("FreeID gave up while -3 was still free: %v", err)
	}
	if got != "e1-3" {
		t.Fatalf("want e1-3, the last suffix in range, got %q", got)
	}
}
