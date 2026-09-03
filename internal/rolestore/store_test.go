package rolestore

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// open makes a store in a disposable directory, under the name a real repository
// would use.
//
// filepath.Join(TempDir, DefaultDir) rather than the temp dir itself, so every
// test goes through the same Path() the CLI does. A store rooted at the bare temp
// dir would keep passing while Path was wrong.
func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), DefaultDir))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

// TestADefinedRoleReadsBackAsTheFieldsItWasGiven is the point of the package.
//
// Both fields are checked, and advisory is the one worth naming: it is the only
// field here that changes whether a stage can advance, so a role that lost it
// would produce agents that count toward a quorum their author excluded them
// from. Nothing later in the pipeline can notice -- the rendered agent is a valid
// blueprint either way.
func TestADefinedRoleReadsBackAsTheFieldsItWasGiven(t *testing.T) {
	s := open(t)

	path, err := s.Create(Record{Name: "auditor", Advisory: true, Tools: []string{"read", "grep"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if want := s.Path("auditor"); path != want {
		t.Errorf("Create returned %q, want %q\n"+
			"  consequence: `role define` prints this path, and the surface declares "+
			"no `role show`, so a wrong path leaves the user nothing to open.\n"+
			"  fix: return s.Path(r.Name), the same join Load uses.", path, want)
	}

	got, err := s.Load("auditor")
	if err != nil {
		t.Fatalf("Load right after Create: %v\n"+
			"  consequence: the store wrote a file it cannot read back, so every "+
			"`agent create --role auditor` afterwards fails on bytes this package "+
			"produced and the user cannot correct.", err)
	}
	if got.Name != "auditor" {
		t.Errorf("role name is %q, want %q\n"+
			"  consequence: this string becomes the agent's `role:` line, which "+
			"kernel.Decide compares against \"coordinator\" and prints as the "+
			"member's identity.", got.Name, "auditor")
	}
	if !got.Advisory {
		t.Errorf("advisory came back false, want true\n" +
			"  consequence: advisory is the one field here that changes whether a " +
			"stage advances. Losing it makes an agent count toward a quorum its " +
			"author excluded it from, and the rendered blueprint is valid either way.")
	}
	if joined := strings.Join(got.Tools, ","); joined != "read,grep" {
		t.Errorf("role tools are %q, want %q\n"+
			"  consequence: these are copied into the agent's grant list, which "+
			"tool.Resolve checks first, so a lost tool is a refusal mid-run for a "+
			"tool the operator granted.", joined, "read,grep")
	}
}

// TestARoleWithNoDefaultsIsStillWorthDefining pins a deliberate acceptance.
//
// `role define reviewer` with neither flag records a name and nothing else, which
// looks like an empty file with no purpose. It has one: `agent create` notes a
// --role nothing has defined, so this file is what makes `--role reviewr` visible
// as a typo. Refusing it -- the obvious "a role must define something" guard --
// would remove the only spelling check the verb offers.
func TestARoleWithNoDefaultsIsStillWorthDefining(t *testing.T) {
	s := open(t)

	if _, err := s.Create(Record{Name: "reviewer"}); err != nil {
		t.Fatalf("Create of a role with no defaults: %v\n"+
			"  consequence: registering a name is the whole spelling check behind "+
			"`agent create --role`; refusing it removes it.", err)
	}

	names, err := s.Names()
	if err != nil {
		t.Fatalf("Names: %v", err)
	}
	if len(names) != 1 || names[0] != "reviewer" {
		t.Errorf("Names is %v, want [reviewer]\n"+
			"  consequence: `agent create` lists the defined roles when --role names "+
			"none of them. A role missing from this list is reported as undefined "+
			"immediately after being defined.", names)
	}

	rec, err := s.Load("reviewer")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if rec.Advisory || len(rec.Tools) != 0 {
		t.Errorf("empty role loaded as {advisory:%t tools:%v}, want {false []}\n"+
			"  consequence: a default nobody typed would be applied to every agent "+
			"created with this role, which is the one thing --role must not do.",
			rec.Advisory, rec.Tools)
	}
}

// TestTheRoleFileIsJSONAHumanCanReadAndEdit.
//
// The surface declares no `role list` and no `role show`, so this file is the only
// way to read a role back or change one. That makes its shape part of the
// interface rather than an implementation detail: the keys are the flag names, and
// the indentation is what makes a hand edit reviewable in a diff.
func TestTheRoleFileIsJSONAHumanCanReadAndEdit(t *testing.T) {
	s := open(t)
	if _, err := s.Create(Record{Name: "auditor", Advisory: true, Tools: []string{"read"}}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	raw, err := os.ReadFile(s.Path("auditor"))
	if err != nil {
		t.Fatalf("read the file back: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("the stored role is not JSON: %v\n%s", err, raw)
	}
	for _, key := range []string{"name", "advisory", "tools"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("the stored role has no %q key: %s\n"+
				"  consequence: the keys are the flag names. A field spelled "+
				"differently on disk cannot be edited by somebody who has only seen "+
				"`role define --help`.", key, raw)
		}
	}
	if !strings.Contains(string(raw), "\n  \"") {
		t.Errorf("the stored role is not indented: %s\n"+
			"  consequence: this file is the only way to read a role back, and a "+
			"single-line record makes a one-field hand edit an unreadable diff.", raw)
	}
	if !strings.HasSuffix(string(raw), "\n") {
		t.Errorf("the stored role does not end in a newline: %q\n"+
			"  consequence: it is a text file in a git repository; a missing final "+
			"newline shows up as a change in the next diff that touches it.", raw)
	}
	if runtime.GOOS != "windows" {
		// Windows reports a mode CreateTemp's 0600 never had, so the assertion
		// would be about the platform rather than about this package.
		info, err := os.Stat(s.Path("auditor"))
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if info.Mode().Perm() != 0o644 {
			t.Errorf("the stored role is mode %v, want 0644\n"+
				"  consequence: CreateTemp makes files 0600. A role is a default the "+
				"team commits and reads, like the agents it is applied to, so leaving "+
				"it owner-only makes it behave differently from every other file in "+
				"the tree.", info.Mode().Perm())
		}
	}
}

// TestDefineRefusesToReplaceARoleAndLeavesTheFileAlone.
//
// `role define` is Mutates and not Idempotent in the registry, so a second call
// with the same name is not required to be a no-op -- and the only alternative to
// refusing is overwriting. The bytes are compared before and after because a
// refusal that had already truncated the file would be the worst of both answers.
func TestDefineRefusesToReplaceARoleAndLeavesTheFileAlone(t *testing.T) {
	s := open(t)
	if _, err := s.Create(Record{Name: "auditor", Tools: []string{"read"}}); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	before, err := os.ReadFile(s.Path("auditor"))
	if err != nil {
		t.Fatalf("read the first definition: %v", err)
	}

	_, err = s.Create(Record{Name: "auditor", Advisory: true, Tools: []string{"bash"}})
	if !errors.Is(err, ErrExists) {
		t.Fatalf("second Create returned %v, want ErrExists\n"+
			"  consequence: the CLI answers ErrExists with exit 2 and \"nothing was "+
			"written\". Any other error is reported as an I/O failure, and no error "+
			"at all silently replaces a definition the agents created from it "+
			"already copied.", err)
	}

	after, err := os.ReadFile(s.Path("auditor"))
	if err != nil {
		t.Fatalf("read the definition after the refusal: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("the refused define changed the file:\n--- before\n%s\n--- after\n%s\n"+
			"  consequence: the refusal promises the existing definition is intact. "+
			"A partially rewritten role would grant bash to every agent created next.",
			before, after)
	}
}

// TestAnUnknownToolNeverBecomesARole.
//
// tool.ValidateGrants reports every bad name at once, and the point of calling it
// in Create rather than only in the CLI is that a bad grant must not reach disk:
// `--tools reed` typed once would otherwise be copied into every agent defined
// with that role, and each of those agents would be refused later by
// agentstore -- for a name the message would blame on their own --tools flag.
func TestAnUnknownToolNeverBecomesARole(t *testing.T) {
	s := open(t)

	_, err := s.Create(Record{Name: "auditor", Tools: []string{"read", "reed"}})
	if err == nil {
		t.Fatal("Create accepted --tools read,reed\n" +
			"  consequence: the grant is copied into every agent created with this " +
			"role, and tool.Resolve denies what it cannot recognise, so the failure " +
			"arrives mid-run instead of at the typo.")
	}
	if !strings.Contains(err.Error(), "reed") {
		t.Errorf("the refusal does not name the bad tool: %v\n"+
			"  consequence: the operator has to guess which of their tools was "+
			"rejected.", err)
	}
	if _, statErr := os.Stat(s.Path("auditor")); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("a file exists after the refusal (%v), want none\n"+
			"  consequence: a refused command that wrote anyway makes the next "+
			"`role define auditor` fail with ErrExists, naming a role the operator "+
			"was told did not get created.", statErr)
	}
}

// TestAToolAddedByHandIsRefusedAtTheReadAndNamesTheFile pins the asymmetry with
// agentstore, which deliberately does NOT re-check tool names when it loads.
//
// The two are right for opposite reasons. A hand-added tool in an agent file is
// run by `run start`, so a reader that refused it would be stricter than the thing
// that executes it. A hand-added tool in a role file is never run: it is copied
// into an agent, and agentstore then refuses `agent create` with "unknown tool(s):
// reed" -- blaming a --tools flag the user never typed, for a name that came from
// a file the message does not mention. So the path must appear in the refusal, and
// that is what is asserted here rather than merely that it failed.
func TestAToolAddedByHandIsRefusedAtTheReadAndNamesTheFile(t *testing.T) {
	s := open(t)
	if _, err := s.Create(Record{Name: "auditor", Tools: []string{"read"}}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	edited := `{"name":"auditor","advisory":false,"tools":["read","reed"]}` + "\n"
	if err := os.WriteFile(s.Path("auditor"), []byte(edited), 0o644); err != nil {
		t.Fatalf("hand-edit the role: %v", err)
	}

	_, err := s.Load("auditor")
	if err == nil {
		t.Fatal("Load accepted a role granting an unknown tool\n" +
			"  consequence: it is copied into the next agent, and the refusal that " +
			"follows names the agent's --tools flag instead of this file.")
	}
	if !strings.Contains(err.Error(), s.Path("auditor")) {
		t.Errorf("the refusal does not name the file: %v\n"+
			"  consequence: pointing at the real file is the entire reason this "+
			"validates on read at all. Without the path the message is worse than "+
			"agentstore's, not better.", err)
	}
	if !strings.Contains(err.Error(), "reed") {
		t.Errorf("the refusal does not name the bad tool: %v\n"+
			"  consequence: the operator has to diff the file against the known "+
			"tools by hand.", err)
	}
}

// TestABrokenRoleFileSaysItCanBeFixedByHand.
//
// Load's error on unparseable JSON is the one message that must not read like an
// internal failure: the file is one a human is invited to edit, so the remedy is
// theirs. The alternative -- "rolestore: parse ...: unexpected end of JSON input"
// alone -- describes the symptom and leaves the reader unsure whether the store
// is corrupt or their edit is.
func TestABrokenRoleFileSaysItCanBeFixedByHand(t *testing.T) {
	s := open(t)
	if err := os.MkdirAll(s.Dir(), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(s.Path("auditor"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write a broken role: %v", err)
	}

	_, err := s.Load("auditor")
	if err == nil {
		t.Fatal("Load accepted a file that is not JSON")
	}
	if !strings.Contains(err.Error(), s.Path("auditor")) || !strings.Contains(err.Error(), "edit") {
		t.Errorf("the refusal is %v\n"+
			"  want the path and the fact that the file is editable\n"+
			"  consequence: the reader cannot tell whether to fix their own edit or "+
			"report a bug in the store.", err)
	}
}

// TestANameThatWouldEscapeTheStoreIsRefused, on the way in and on the way out.
//
// Both directions matter and they fail differently. A write with a separator would
// put the file outside roles/, where nothing looks for it; a read with one is a
// path traversal, and `agent create --role ../../etc/passwd` is a plain string
// the CLI passes straight through.
func TestANameThatWouldEscapeTheStoreIsRefused(t *testing.T) {
	s := open(t)

	for _, name := range []string{"team/auditor", `team\auditor`, "..", "."} {
		if _, err := s.Create(Record{Name: name}); err == nil {
			t.Errorf("Create accepted the name %q\n"+
				"  consequence: the name becomes the filename, so the role lands "+
				"outside roles/ and Names never offers it again.", name)
		}
	}

	if _, err := s.Load("../../etc/passwd"); !errors.Is(err, ErrNotExist) {
		t.Errorf("Load(%q) returned %v, want ErrNotExist\n"+
			"  consequence: --role is a free string on the command line, so a read "+
			"that followed the separator would open a file outside the store and "+
			"report its parse error.", "../../etc/passwd", err)
	}
}

// TestANameWithInvisibleCharactersIsRefusedRatherThanTrimmed.
//
// Trimming is the tempting fix and it is the wrong one: `agent create --role` is
// compared exactly, so a silently renamed role would be reported as undefined
// right after being defined. The control-character case is worse than cosmetic --
// the name is written into the agent's `role:` line, so it would corrupt the YAML
// of a different file from the one this command wrote.
func TestANameWithInvisibleCharactersIsRefusedRatherThanTrimmed(t *testing.T) {
	s := open(t)

	for _, name := range []string{" auditor", "auditor ", "aud\nitor", "aud\titor"} {
		if _, err := s.Create(Record{Name: name}); err == nil {
			t.Errorf("Create accepted the name %q\n"+
				"  consequence: whitespace makes the role unfindable by the name the "+
				"user typed; a control character breaks the rendered YAML of every "+
				"agent created with it.", name)
		}
	}
}

// TestReadingRolesNeverCreatesTheDirectory is the reason At exists.
//
// `agent create --role X` reads this store on every invocation, and the common
// case by a wide margin is a repository where no role was ever defined. Through
// Open that read would leave an empty roles/ behind -- a directory the user did
// not ask for, which git then reports -- and in a checkout they cannot write it
// would fail with "create roles: permission denied", an error about a directory
// the command was never asked to make.
func TestReadingRolesNeverCreatesTheDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), DefaultDir)
	s, err := At(dir)
	if err != nil {
		t.Fatalf("At: %v", err)
	}

	names, err := s.Names()
	if err != nil {
		t.Fatalf("Names with no directory: %v\n"+
			"  consequence: a fresh repository cannot list what is defined, so the "+
			"note `agent create` prints about an undefined role fails instead.", err)
	}
	if len(names) != 0 {
		t.Errorf("Names returned %v, want none", names)
	}

	if _, err := s.Load("auditor"); !errors.Is(err, ErrNotExist) {
		t.Errorf("Load with no directory returned %v, want ErrNotExist\n"+
			"  consequence: the caller distinguishes \"not defined\" from an I/O "+
			"failure by this sentinel, and answers them with different sentences.", err)
	}

	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s exists after two reads (%v), want it untouched\n"+
			"  consequence: every `agent create` in a repository with no roles would "+
			"leave an empty directory behind, and would fail outright where the "+
			"checkout is read-only.", dir, err)
	}
}

// TestAMissingRoleSaysWhereItLookedFor.
//
// There is no `role list` to fall back on, so the directory has to be in the
// message: a user whose role is missing is very often in the wrong working
// directory, and "no such role: auditor" alone cannot tell them that.
func TestAMissingRoleSaysWhereItLookedFor(t *testing.T) {
	s := open(t)

	_, err := s.Load("auditor")
	if !errors.Is(err, ErrNotExist) {
		t.Fatalf("Load of a missing role returned %v, want ErrNotExist", err)
	}
	if !strings.Contains(err.Error(), s.Dir()) {
		t.Errorf("the refusal does not name the directory: %v\n"+
			"  consequence: roles are per-repository on purpose, so the most common "+
			"cause of a missing one is being in the wrong directory -- the one thing "+
			"the message would not mention.", err)
	}
}

// TestNamesOffersOnlyRoles, and in particular never a half-written one.
//
// The temp file is the case worth pinning. write publishes through
// <name>.json.tmp-NNNN, and Names selects on the exact ".json" suffix, so a define
// interrupted between CreateTemp and Rename leaves nothing Names will offer -- and
// nothing `agent create` will then fail to parse.
func TestNamesOffersOnlyRoles(t *testing.T) {
	s := open(t)
	if _, err := s.Create(Record{Name: "auditor"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, name := range []string{"auditor.json.tmp-4711", "README.md", "notes.yaml"} {
		if err := os.WriteFile(filepath.Join(s.Dir(), name), []byte("{}"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(s.Dir(), "archive.json"), 0o755); err != nil {
		t.Fatalf("mkdir archive.json: %v", err)
	}

	names, err := s.Names()
	if err != nil {
		t.Fatalf("Names: %v", err)
	}
	if len(names) != 1 || names[0] != "auditor" {
		t.Errorf("Names is %v, want [auditor]\n"+
			"  consequence: a temp file offered as a role is a name `agent create` "+
			"reports as defined and then fails to parse; a directory named "+
			"archive.json is a role Load can never read.", names)
	}
}

// TestTheFilenameIsAuthoritativeOverTheNameField.
//
// They can only disagree if somebody copied roles/auditor.json to
// roles/reviewer.json to start a second role from the first, which is exactly what
// a store with no `role show` and no `role edit` invites. The name the user typed
// wins, because that name is what gets written into the agent's `role:` line: the
// alternative silently creates agents whose role says "auditor" while the operator
// asked for "reviewer", and kernel.Decide reads that string.
func TestTheFilenameIsAuthoritativeOverTheNameField(t *testing.T) {
	s := open(t)
	if _, err := s.Create(Record{Name: "auditor", Tools: []string{"read"}}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	raw, err := os.ReadFile(s.Path("auditor"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := os.WriteFile(s.Path("reviewer"), raw, 0o644); err != nil {
		t.Fatalf("copy the role: %v", err)
	}

	rec, err := s.Load("reviewer")
	if err != nil {
		t.Fatalf("Load the copy: %v", err)
	}
	if rec.Name != "reviewer" {
		t.Errorf("the copied role loaded as %q, want %q\n"+
			"  consequence: this string is written into the agent's `role:`, so the "+
			"agent would carry a role the operator did not ask for -- and "+
			"kernel.Decide compares that field against \"coordinator\".", rec.Name, "reviewer")
	}
}

// TestBothConstructorsRefuseAnEmptyDirectory.
//
// A store rooted at "" is a store rooted at the working directory, where Names
// would offer every *.json in the repository as a role and `role define reviewer`
// would write reviewer.json beside go.mod. At needs the guard as much as Open even
// though it creates nothing, because the reading path is the one that would
// silently succeed with the wrong answer.
func TestBothConstructorsRefuseAnEmptyDirectory(t *testing.T) {
	if _, err := Open("  "); err == nil {
		t.Error("Open(\"  \") succeeded\n" +
			"  consequence: roles would be written into the working directory.")
	}
	if _, err := At(""); err == nil {
		t.Error("At(\"\") succeeded\n" +
			"  consequence: every *.json in the repository would be offered as a role.")
	}
}
