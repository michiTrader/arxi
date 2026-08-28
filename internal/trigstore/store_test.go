package trigstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michiTrader/iash/internal/trigger"
)

// nightly is the trigger from docs/design/20-use-cases.md §20.10.
func nightly() trigger.Record {
	return trigger.Record{
		Name:         "nightly-audit",
		On:           "cron:0 3 * * *",
		Then:         "run start security-team 'audit dependencies for new CVEs'",
		Budget:       5.00,
		BudgetPeriod: trigger.PeriodDay,
		OnMissed:     trigger.MissedSkip,
		Overlap:      trigger.OverlapSkip,
		Status:       trigger.StatusActive,
		CreatedAt:    "2026-08-26T12:00:00Z",
	}
}

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), DefaultDir))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func TestATriggerSurvivesTheRoundTrip(t *testing.T) {
	s := open(t)
	want := nightly()
	if err := s.Create(want); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := s.Load(want.Name)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != want {
		t.Errorf("the record changed on disk:\n got %+v\nwant %+v", got, want)
	}
}

// The `on` and `then` text is what `trigger show` echoes, so it has to come
// back byte-identical: a store that normalised "0 3 * * *" into its parsed form
// would make the displayed schedule differ from the one the user typed.
func TestTheUsersOwnTextComesBackUnchanged(t *testing.T) {
	s := open(t)
	r := nightly()
	r.On = "cron:0  3 * * *" // the extra space is the user's
	if err := s.Create(r); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := s.Load(r.Name)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.On != r.On {
		t.Errorf("stored on = %q, want the typed %q", got.On, r.On)
	}
	if got.Then != r.Then {
		t.Errorf("stored then = %q, want the typed %q", got.Then, r.Then)
	}
}

func TestCreateRefusesToReplaceARunningSchedule(t *testing.T) {
	s := open(t)
	if err := s.Create(nightly()); err != nil {
		t.Fatalf("first Create: %v", err)
	}

	second := nightly()
	second.Then = "run start security-team 'something else entirely'"
	second.Budget = 500.00

	err := s.Create(second)
	if err == nil {
		t.Fatal("Create overwrote an existing trigger.\n" +
			"  the old --then is the part that spends money, and replacing it " +
			"silently leaves no record that it existed")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("the error does not say the trigger exists: %v", err)
	}
	// And the original must be untouched, not half-replaced.
	got, err := s.Load("nightly-audit")
	if err != nil {
		t.Fatalf("Load after refused Create: %v", err)
	}
	if got != nightly() {
		t.Errorf("the refused Create modified the original:\n got %+v\nwant %+v",
			got, nightly())
	}
}

// On macOS and Windows `Nightly-Audit.json` and `nightly-audit.json` are one
// file. Allowing the pair through means the create is destructive on exactly
// the machines where nobody is running the test suite.
func TestANameDifferingOnlyInCaseIsRefused(t *testing.T) {
	s := open(t)
	if err := s.Create(nightly()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	clash := nightly()
	clash.Name = "Nightly-Audit"

	err := s.Create(clash)
	if err == nil {
		t.Fatal("a name differing only in case was accepted.\n" +
			"  on a case-insensitive filesystem this overwrites the existing " +
			"trigger while appearing to create a new one")
	}
	if !strings.Contains(err.Error(), "collides") {
		t.Errorf("the error does not explain the collision: %v", err)
	}
}

func TestSaveReplacesButWillNotInventATrigger(t *testing.T) {
	s := open(t)
	if err := s.Create(nightly()); err != nil {
		t.Fatalf("Create: %v", err)
	}

	paused := nightly()
	paused.Status = trigger.StatusPaused
	if err := s.Save(paused); err != nil {
		t.Fatalf("Save over an existing trigger: %v", err)
	}
	got, err := s.Load(paused.Name)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Status != trigger.StatusPaused {
		t.Errorf("status = %q after Save, want paused", got.Status)
	}

	typo := nightly()
	typo.Name = "nightley-audit"
	err = s.Save(typo)
	if err == nil {
		t.Fatal("Save created a trigger that did not exist.\n" +
			"  `trigger pause nightley-audit` would then report success while " +
			"the real trigger kept firing")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("the error does not say the name is unknown: %v", err)
	}
}

func TestAMissingTriggerSaysSoAndSaysWhereToLook(t *testing.T) {
	s := open(t)
	_, err := s.Load("nope")
	if err == nil {
		t.Fatal("Load of a missing trigger succeeded")
	}
	if !strings.Contains(err.Error(), "does not exist") ||
		!strings.Contains(err.Error(), "trigger list") {
		t.Errorf("the error should name the problem and the way out, got: %v", err)
	}
}

// A stored trigger is text a human can edit, and the schedule is stored as the
// text they typed. So the file has to be re-validated on the way in: the point
// of catching `cron:0 3 30 2 *` here is that February the 30th never arrives,
// so the trigger would simply never fire and never explain why.
func TestAHandEditedFileIsRefusedWhenItIsRead(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"an impossible date", `{"name":"x","on":"cron:0 3 30 2 *",` +
			`"then":"run start t 'o'","budget":5,"budget_period":"day",` +
			`"on_missed":"skip","overlap":"skip","status":"active",` +
			`"created_at":"2026-08-26T12:00:00Z"}`, "30"},
		{"a period nothing enforces", `{"name":"x","on":"cron:0 3 * * *",` +
			`"then":"run start t 'o'","budget":5,"budget_period":"fortnight",` +
			`"on_missed":"skip","overlap":"skip","status":"active",` +
			`"created_at":"2026-08-26T12:00:00Z"}`, "fortnight"},
		{"a command withheld from agents", `{"name":"x","on":"cron:0 3 * * *",` +
			`"then":"inbox approve 7","budget":5,"budget_period":"day",` +
			`"on_missed":"skip","overlap":"skip","status":"active",` +
			`"created_at":"2026-08-26T12:00:00Z"}`, "unattended"},
		{"a budget of zero", `{"name":"x","on":"cron:0 3 * * *",` +
			`"then":"run start t 'o'","budget":0,"budget_period":"day",` +
			`"on_missed":"skip","overlap":"skip","status":"active",` +
			`"created_at":"2026-08-26T12:00:00Z"}`, "budget"},
		{"a status the scheduler cannot compare", `{"name":"x","on":"cron:0 3 * * *",` +
			`"then":"run start t 'o'","budget":5,"budget_period":"day",` +
			`"on_missed":"skip","overlap":"skip","status":"disabled",` +
			`"created_at":"2026-08-26T12:00:00Z"}`, "disabled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := open(t)
			path := s.Path("x")
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			_, err := s.Load("x")
			if err == nil {
				t.Fatalf("Load accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the error does not mention %q: %v", tc.want, err)
			}
		})
	}
}

// A misspelled key otherwise leaves the field at its zero value, and a zero
// value that happens to be legal is a setting the user believes they changed.
func TestAMisspelledFieldIsRefusedRatherThanIgnored(t *testing.T) {
	s := open(t)
	body := `{"name":"x","on":"cron:0 3 * * *","then":"run start t 'o'",` +
		`"budget":5,"budget_perid":"day","budget_period":"day",` +
		`"on_missed":"skip","overlap":"skip","status":"active",` +
		`"created_at":"2026-08-26T12:00:00Z"}`
	if err := os.WriteFile(s.Path("x"), []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	_, err := s.Load("x")
	if err == nil {
		t.Fatal("a misspelled field was ignored.\n" +
			"  the user believes they set budget_period; nothing told them otherwise")
	}
	if !strings.Contains(err.Error(), "budget_perid") {
		t.Errorf("the error should name the unknown field, got: %v", err)
	}
}

// `next` is derived on purpose (ADR-0002). Somebody will add it by hand
// expecting it to be honoured; accepting the file while ignoring the field
// makes it say one thing and do another.
func TestAHandWrittenNextIsRefusedRatherThanIgnored(t *testing.T) {
	s := open(t)
	body := `{"name":"x","on":"cron:0 3 * * *","then":"run start t 'o'",` +
		`"budget":5,"budget_period":"day","on_missed":"skip","overlap":"skip",` +
		`"status":"active","created_at":"2026-08-26T12:00:00Z",` +
		`"next":"2030-01-01T03:00:00Z"}`
	if err := os.WriteFile(s.Path("x"), []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := s.Load("x"); err == nil {
		t.Fatal("a hand-written `next` was silently ignored.\n" +
			"  the file now claims a firing time the scheduler does not read")
	}
}

// The filename is what `trigger show NAME` looks up. A record whose name field
// disagrees answers to one name and reports another, and pausing it would write
// a second file while the original kept firing.
func TestAFileWhoseNameDisagreesWithItsContentIsRefused(t *testing.T) {
	s := open(t)
	r := nightly()
	body, err := r.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if err := os.WriteFile(s.Path("renamed-by-hand"), body, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	_, err = s.Load("renamed-by-hand")
	if err == nil {
		t.Fatal("a file renamed by hand was accepted.\n" +
			"  pausing it writes nightly-audit.json and leaves " +
			"renamed-by-hand.json firing")
	}
	if !strings.Contains(err.Error(), "nightly-audit") {
		t.Errorf("the error should name both spellings, got: %v", err)
	}
}

func TestListIsEmptyBeforeAnythingIsCreated(t *testing.T) {
	s := open(t)
	got, err := s.List()
	if err != nil {
		t.Fatalf("List on an empty store: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List returned %d triggers from an empty store", len(got))
	}
}

// A missing directory is no triggers, not a failure: `trigger list` before the
// first `trigger create` is the most ordinary thing a new user does.
func TestListOnAMissingDirectoryIsNotAnError(t *testing.T) {
	s := &Store{dir: filepath.Join(t.TempDir(), "never-created")}
	got, err := s.List()
	if err != nil {
		t.Fatalf("List on a missing directory: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List returned %d triggers", len(got))
	}
}

func TestListIsOrderedByName(t *testing.T) {
	s := open(t)
	for _, n := range []string{"weekly-report", "nightly-audit", "hourly-sync"} {
		r := nightly()
		r.Name = n
		if err := s.Create(r); err != nil {
			t.Fatalf("Create %s: %v", n, err)
		}
	}
	got, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"hourly-sync", "nightly-audit", "weekly-report"}
	if len(got) != len(want) {
		t.Fatalf("List returned %d triggers, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Name != want[i] {
			t.Errorf("List[%d] = %q, want %q (a watched row must not move)",
				i, got[i].Name, want[i])
		}
	}
}

// The dot in ".json" sorts before every letter, so os.ReadDir's own ordering is
// NOT the ordering of the names: `nightly-audit.json` comes before
// `nightly.json`, while `nightly` comes before `nightly-audit`.
//
// This test exists because TestListIsOrderedByName does not catch a missing
// sort. Its three names happen to sort the same way as their filenames, so
// deleting SortRecords entirely left it green — the free ordering looked like
// the intended one. Any test asserting an order has to use inputs where the
// order it wants differs from the order it would get for nothing.
func TestListOrderIsTheNameOrderAndNotTheFilenameOrder(t *testing.T) {
	s := open(t)
	for _, n := range []string{"nightly", "nightly-audit"} {
		r := nightly()
		r.Name = n
		if err := s.Create(r); err != nil {
			t.Fatalf("Create %s: %v", n, err)
		}
	}
	got, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"nightly", "nightly-audit"}
	for i := range want {
		if got[i].Name != want[i] {
			t.Fatalf("List[%d] = %q, want %q\n"+
				"  this is filename order, not name order: %q sorts before %q "+
				"because '.' precedes every letter. `trigger list` must be sorted "+
				"by the name it prints, or a row moves for reasons invisible in "+
				"the output",
				i, got[i].Name, want[i], "nightly-audit.json", "nightly.json")
		}
	}
}

// `trigger list` is what a user reads to confirm a trigger exists. A list
// quietly missing one row is indistinguishable from a list of everything.
func TestListRefusesRatherThanSkippingAnUnreadableTrigger(t *testing.T) {
	s := open(t)
	if err := s.Create(nightly()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := os.WriteFile(s.Path("broken"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	_, err := s.List()
	if err == nil {
		t.Fatal("List skipped an unreadable trigger.\n" +
			"  the user sees a complete-looking list with a schedule missing")
	}
	if !strings.Contains(err.Error(), "broken") {
		t.Errorf("the error should name the file, got: %v", err)
	}
}

// Files that are not triggers share the directory (an editor backup, a README).
// Treating them as triggers would make `trigger list` fail on a stray file.
func TestUnrelatedFilesAreNotTriggers(t *testing.T) {
	s := open(t)
	if err := s.Create(nightly()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, n := range []string{"README.md", "nightly-audit.json.bak", ".DS_Store"} {
		if err := os.WriteFile(filepath.Join(s.Dir(), n), []byte("junk"), 0o644); err != nil {
			t.Fatalf("write %s: %v", n, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(s.Dir(), "subdir.json"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	got, err := s.List()
	if err != nil {
		t.Fatalf("List with unrelated files present: %v", err)
	}
	if len(got) != 1 || got[0].Name != "nightly-audit" {
		t.Errorf("List = %+v, want just nightly-audit", got)
	}
}

// The temp file used during a write must not end in .json, or a concurrent
// List can load a file that is still being written.
func TestAHalfWrittenTriggerIsNeverListed(t *testing.T) {
	s := open(t)
	// Simulate the window: a temp file shaped exactly like write() creates.
	tmp, err := os.CreateTemp(s.Dir(), "nightly-audit"+ext+".tmp-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if _, err := tmp.WriteString("{ partially written"); err != nil {
		t.Fatalf("write: %v", err)
	}
	tmp.Close()

	got, err := s.List()
	if err != nil {
		t.Fatalf("List while a write was in flight: %v\n"+
			"  the temp name must not end in %q, or the glob picks it up", err, ext)
	}
	if len(got) != 0 {
		t.Errorf("List returned %d triggers, want 0", len(got))
	}
}

func TestAFailedWriteLeavesNoDebris(t *testing.T) {
	s := open(t)
	bad := nightly()
	bad.On = "cron:not a schedule"
	if err := s.Create(bad); err == nil {
		t.Fatal("Create accepted an invalid schedule")
	}
	entries, err := os.ReadDir(s.Dir())
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("a refused Create left %d files behind: %v", len(entries), entries)
	}
}

func TestTheStoredFileIsReadableAndNotExecutable(t *testing.T) {
	s := open(t)
	if err := s.Create(nightly()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	fi, err := os.Stat(s.Path("nightly-audit"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("mode = %o, want 644 (os.CreateTemp defaults to 600, which "+
			"makes the file unreadable to anyone but its creator)", got)
	}
}

// A store that will not say where it is cannot be reported in an error message,
// and "no directory" has to be refused rather than defaulting to the current
// one, where it would scatter .json files into the user's project root.
func TestAnEmptyDirectoryIsRefused(t *testing.T) {
	if _, err := Open("  "); err == nil {
		t.Fatal("Open accepted a blank directory, which would write triggers " +
			"into whatever the working directory happens to be")
	}
}

func TestOpenCreatesTheDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b", DefaultDir)
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if s.Dir() != dir {
		t.Errorf("Dir() = %q, want %q", s.Dir(), dir)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat after Open: %v", err)
	}
	if !fi.IsDir() {
		t.Errorf("%s is not a directory", dir)
	}
}

// Storing triggers beside runs/ is a decision with a stated reason (a
// home-directory store makes a trigger created in one project fire against
// whatever is checked out later). If the default moves, that reasoning has to
// move with it.
func TestTheDefaultDirectoryIsProjectLocal(t *testing.T) {
	if DefaultDir != "triggers" {
		t.Errorf("DefaultDir = %q, want %q", DefaultDir, "triggers")
	}
	if filepath.IsAbs(DefaultDir) || strings.HasPrefix(DefaultDir, "~") {
		t.Errorf("DefaultDir %q is not project-local: a trigger created while "+
			"working on one repository would fire against another", DefaultDir)
	}
}
