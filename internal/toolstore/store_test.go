package toolstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/surface"
	"github.com/michiTrader/arxi/internal/tool"
)

// open makes a store in a disposable directory.
func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), DefaultDir))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

// TestAnOverrideReachesTheResolverThatAlwaysHadASlotForIt is the point of the
// whole package.
//
// It asserts against tool.Resolve rather than against the stored bytes, because
// what matters is not that the file says "allow" -- it is that the resolver
// which decides whether a run stops to ask now returns allow. A test that
// checked only the JSON would pass just as well if nothing ever read it, which
// is exactly the state this package was written to end.
func TestAnOverrideReachesTheResolverThatAlwaysHadASlotForIt(t *testing.T) {
	s := open(t)

	granted := []string{"read", "bash"}

	// Before: bash mutates, so the default is ask, and a run stops.
	if got := tool.Resolve(granted, nil, "bash"); got != surface.PolicyAsk {
		t.Fatalf("bash resolves to %q without an override, want ask; the "+
			"premise of this test is wrong", got)
	}

	if _, _, err := s.Set("backend", "bash", surface.PolicyAllow); err != nil {
		t.Fatalf("Set: %v", err)
	}

	all, err := s.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if got := tool.Resolve(granted, all["backend"], "bash"); got != surface.PolicyAllow {
		t.Errorf("bash resolves to %q after --allow bash, want allow\n"+
			"  consequence: the approval loop has no exit -- approving respawns "+
			"the turn, the policy still says ask, and the model is asked again", got)
	}
}

// TestAnOverrideDoesNotGrantATheAgentNeverHad: the grant list is not decorative.
//
// tool.Resolve checks the grant list first, so this is really a test that the
// store does not try to work around that ordering. It is worth having because
// the tempting shortcut -- "the operator said allow, so allow" -- would make the
// blueprint's tools: list advisory, and the blueprint is the reviewed artifact.
func TestAnOverrideDoesNotGrantAToolTheAgentNeverHad(t *testing.T) {
	s := open(t)
	if _, _, err := s.Set("backend", "bash", surface.PolicyAllow); err != nil {
		t.Fatalf("Set: %v", err)
	}
	all, err := s.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	// backend was granted read only. The override names bash.
	if got := tool.Resolve([]string{"read"}, all["backend"], "bash"); got != surface.PolicyDeny {
		t.Errorf("an ungranted tool resolves to %q, want deny\n"+
			"  consequence: an override that grants makes the blueprint's tools "+
			"list decorative, and the blueprint is the thing under review", got)
	}
}

// TestSettingOneToolDoesNotForgetAnother guards the read-modify-write.
//
// The DENY is the one that matters. A replace-instead-of-merge bug loses it, and
// losing a deny widens an agent's reach as a side effect of a command that was
// narrowing it somewhere else -- a change nobody would think to look for.
func TestSettingOneToolDoesNotForgetAnother(t *testing.T) {
	s := open(t)
	if _, _, err := s.Set("backend", "write", surface.PolicyDeny); err != nil {
		t.Fatalf("Set write=deny: %v", err)
	}
	if _, _, err := s.Set("backend", "bash", surface.PolicyAllow); err != nil {
		t.Fatalf("Set bash=allow: %v", err)
	}

	rec, err := s.Load("backend")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := rec.Tools["write"]; got != surface.PolicyDeny {
		t.Errorf("write = %q after setting bash, want deny\n"+
			"  consequence: a deny was lost by a command about a different "+
			"tool, which widens an agent quietly", got)
	}
	if got := rec.Tools["bash"]; got != surface.PolicyAllow {
		t.Errorf("bash = %q, want allow", got)
	}
}

// TestSetReportsWhatItReplaced: the caller can tell a change from a no-op.
//
// Without this the CLI would have to read before writing to print anything
// truthful, and two reads around a write is where a race lives.
func TestSetReportsWhatItReplaced(t *testing.T) {
	s := open(t)

	prev, had, err := s.Set("backend", "bash", surface.PolicyAllow)
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if had {
		t.Errorf("the first Set reported a previous value %q; there was none", prev)
	}

	prev, had, err = s.Set("backend", "bash", surface.PolicyDeny)
	if err != nil {
		t.Fatalf("Set again: %v", err)
	}
	if !had {
		t.Error("the second Set reported no previous value; there was one")
	}
	if prev != surface.PolicyAllow {
		t.Errorf("previous = %q, want allow", prev)
	}
}

// TestNoOverridesIsNotAnError, and it is the common case.
//
// Every run start reads the policy of every member, and by a wide margin no
// override exists. If a missing file were an error, a run could not start until
// somebody had configured something they had no reason to configure.
func TestNoOverridesIsNotAnError(t *testing.T) {
	s := open(t)

	rec, err := s.Load("backend")
	if err != nil {
		t.Fatalf("Load of an unconfigured agent failed (%v)\n"+
			"  consequence: no run could start until every member had been "+
			"given a policy nobody wanted to change", err)
	}
	if len(rec.Tools) != 0 {
		t.Errorf("an unconfigured agent has overrides: %v", rec.Tools)
	}
	if rec.Tools == nil {
		t.Error("Tools is nil rather than empty; a caller indexing it is fine, " +
			"but a caller assigning into it panics")
	}

	all, err := s.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll on an empty store: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("LoadAll on an empty store returned %v", all)
	}
}

// TestClearingTheLastOverrideLeavesNothingBehind.
//
// An empty {"tools":{}} on disk reads as "this agent has been configured", and
// the next person to look would spend time working out what was decided.
func TestClearingTheLastOverrideLeavesNothingBehind(t *testing.T) {
	s := open(t)
	if _, _, err := s.Set("backend", "bash", surface.PolicyAllow); err != nil {
		t.Fatalf("Set: %v", err)
	}

	had, err := s.Clear("backend", "bash")
	if err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if !had {
		t.Error("Clear reported nothing to remove")
	}

	if _, err := os.Stat(s.Path("backend")); !os.IsNotExist(err) {
		t.Errorf("the policy file still exists after the last override was "+
			"cleared (%v)\n  consequence: an empty record reads as a decision, "+
			"and somebody will try to work out what it was", err)
	}

	// And the tool is back to its default, not stuck at what it was.
	all, _ := s.LoadAll()
	if got := tool.Resolve([]string{"bash"}, all["backend"], "bash"); got != surface.PolicyAsk {
		t.Errorf("bash resolves to %q after the override was cleared, want the "+
			"default ask", got)
	}
}

// TestClearingOneOverrideKeepsTheOthers.
func TestClearingOneOverrideKeepsTheOthers(t *testing.T) {
	s := open(t)
	s.Set("backend", "bash", surface.PolicyAllow)
	s.Set("backend", "write", surface.PolicyDeny)

	if _, err := s.Clear("backend", "bash"); err != nil {
		t.Fatalf("Clear: %v", err)
	}

	rec, err := s.Load("backend")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, still := rec.Tools["bash"]; still {
		t.Error("bash survived being cleared")
	}
	if got := rec.Tools["write"]; got != surface.PolicyDeny {
		t.Errorf("write = %q after clearing bash, want deny", got)
	}
}

// TestClearingSomethingThatWasNeverSetSaysSo rather than reporting success.
//
// `agent tool policy --agent backend --reset bahs` is a typo, and a silent
// success would leave the operator believing they had changed something.
func TestClearingSomethingThatWasNeverSetSaysSo(t *testing.T) {
	s := open(t)
	had, err := s.Clear("backend", "bash")
	if err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if had {
		t.Error("Clear reported it removed an override that never existed; the " +
			"operator would believe a typo had taken effect")
	}
}

// TestAnUnknownToolIsRefusedRatherThanStored.
//
// The dangerous thing about a typo here is not that it widens anything -- it
// cannot -- but that it produces an override which will never match a tool and
// therefore never fires. The agent keeps asking while the file says it should
// not, and the operator has no reason to suspect the file.
func TestAnUnknownToolIsRefusedRatherThanStored(t *testing.T) {
	s := open(t)
	_, _, err := s.Set("backend", "bahs", surface.PolicyAllow)
	if err == nil {
		t.Fatal("an unknown tool name was accepted; the override can never " +
			"match, so the agent keeps asking while the file says otherwise")
	}
	if !strings.Contains(err.Error(), "known tools") {
		t.Errorf("error %q does not say what the known tools are", err)
	}
	if _, statErr := os.Stat(s.Path("backend")); statErr == nil {
		t.Error("a file was written for a refused override")
	}
}

// TestAnUnknownPolicyWordIsRefused.
func TestAnUnknownPolicyWordIsRefused(t *testing.T) {
	s := open(t)
	_, _, err := s.Set("backend", "bash", surface.Policy("alow"))
	if err == nil {
		t.Fatal("an unknown policy word was accepted")
	}
	if !strings.Contains(err.Error(), "allow, ask, deny") {
		t.Errorf("error %q does not list the policies that exist", err)
	}
}

// TestAPathSeparatorInAnAgentNameIsRefused.
//
// The agent name becomes a filename. Refused rather than sanitised: an operator
// who typed a slash meant something, and quietly renaming their agent is worse
// than saying it cannot be spelled that way.
func TestAPathSeparatorInAnAgentNameIsRefused(t *testing.T) {
	s := open(t)
	for _, name := range []string{"../escape", "a/b", ".", ".."} {
		if _, _, err := s.Set(name, "bash", surface.PolicyAllow); err == nil {
			t.Errorf("agent name %q was accepted; it becomes a filename", name)
		}
	}
}

// TestAHandEditedFileIsCheckedOnTheWayIn.
//
// This file is text a human can edit, and the point of validating on read is
// that a typo becomes a message instead of an override that silently never
// fires.
func TestAHandEditedFileIsCheckedOnTheWayIn(t *testing.T) {
	s := open(t)
	if err := os.WriteFile(s.Path("backend"),
		[]byte(`{"agent":"backend","tools":{"bash":"alow"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Load("backend"); err == nil {
		t.Fatal("a hand-edited file with an invalid policy word loaded cleanly; " +
			"the override would never match and the operator would not know why")
	}
}

// TestAGarbledFileNamesItselfAndSaysHowToRecover.
//
// Every run start reads these, so an unparseable file stops runs from starting.
// The message has to say which file and what to do, because the person reading
// it is being stopped by something they may not know exists.
func TestAGarbledFileNamesItselfAndSaysHowToRecover(t *testing.T) {
	s := open(t)
	if err := os.WriteFile(s.Path("backend"), []byte(`{not json`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := s.Load("backend")
	if err == nil {
		t.Fatal("a garbled policy file loaded cleanly")
	}
	if !strings.Contains(err.Error(), s.Path("backend")) {
		t.Errorf("error %q does not name the file that has to be fixed", err)
	}
	if !strings.Contains(err.Error(), "delete it") {
		t.Errorf("error %q does not say how to get back to the default policy", err)
	}
}

// TestLoadAllIsTheShapeTheExecutorWants.
//
// provider.Executor.ToolPolicy is map[agent]map[tool]Policy, and this returns
// exactly that so a run start passes it straight through. A conversion loop in
// the caller is a place an agent can be dropped.
func TestLoadAllIsTheShapeTheExecutorWants(t *testing.T) {
	s := open(t)
	s.Set("backend", "bash", surface.PolicyAllow)
	s.Set("frontend", "write", surface.PolicyDeny)

	all, err := s.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("LoadAll returned %d agents, want 2: %v", len(all), all)
	}
	if all["backend"]["bash"] != surface.PolicyAllow {
		t.Errorf("backend/bash = %q", all["backend"]["bash"])
	}
	if all["frontend"]["write"] != surface.PolicyDeny {
		t.Errorf("frontend/write = %q", all["frontend"]["write"])
	}

	// The type is asserted by assignment: if this compiles, the shape matches
	// what the executor's field expects.
	var target map[string]map[string]surface.Policy = all
	_ = target
}

// TestAgentsListsOnlyTheOnesWithOverrides, sorted.
func TestAgentsListsOnlyTheOnesWithOverrides(t *testing.T) {
	s := open(t)
	s.Set("zebra", "bash", surface.PolicyAllow)
	s.Set("alpha", "bash", surface.PolicyAllow)

	got, err := s.Agents()
	if err != nil {
		t.Fatalf("Agents: %v", err)
	}
	if len(got) != 2 || got[0] != "alpha" || got[1] != "zebra" {
		t.Errorf("Agents = %v, want [alpha zebra] sorted", got)
	}
}

// TestAHalfWrittenFileIsNeverVisible.
//
// The temp file must not end in the extension the reader globs for, or a crash
// mid-write would leave something Load refuses -- and since every run start
// reads these, that would stop runs from starting.
func TestAHalfWrittenFileIsNeverVisible(t *testing.T) {
	s := open(t)
	if _, _, err := s.Set("backend", "bash", surface.PolicyAllow); err != nil {
		t.Fatalf("Set: %v", err)
	}

	entries, err := os.ReadDir(s.Dir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("a temp file survived the write: %s", e.Name())
		}
	}

	// And the invariant that makes the glob safe: a temp name does not end in
	// ext, so names() cannot see one.
	if strings.HasSuffix("backend"+ext+".tmp-123", ext) {
		t.Error("the temp file suffix ends in the extension the reader globs " +
			"for; a half-written policy would be loadable")
	}
}

// TestTheStoredFileIsReadableByAHuman.
//
// It is indented JSON with the agent named inside it, because the recovery
// instruction the error message gives is "fix it or delete it", and fixing
// requires reading.
func TestTheStoredFileIsReadableByAHuman(t *testing.T) {
	s := open(t)
	s.Set("backend", "bash", surface.PolicyAllow)

	raw, err := os.ReadFile(s.Path("backend"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "\n") {
		t.Error("the policy is one line; the error message tells a human to fix it")
	}

	var rec Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("the file we wrote does not parse: %v", err)
	}
	if rec.Agent != "backend" {
		t.Errorf("agent = %q; the file does not say who it is about", rec.Agent)
	}
}

// TestTheFilenameWinsOverTheFieldWhenTheyDisagree.
//
// They can only disagree if the file was moved by hand, and then the name the
// operator typed is the one they meant. The alternative is a file called
// backend.json that quietly configures somebody else.
func TestTheFilenameWinsOverTheFieldWhenTheyDisagree(t *testing.T) {
	s := open(t)
	if err := os.WriteFile(s.Path("backend"),
		[]byte(`{"agent":"frontend","tools":{"bash":"allow"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	rec, err := s.Load("backend")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if rec.Agent != "backend" {
		t.Errorf("Agent = %q, want backend (the filename)\n"+
			"  consequence: a file named for one agent would configure another", rec.Agent)
	}
}

// TestEveryKnownToolCanBeOverridden.
//
// Read this as a check on the two lists agreeing rather than on a behaviour.
// tool.Known is the closed set of grantable tools, and an override for one of
// them that Validate refuses would be a tool a blueprint can grant and an
// operator cannot govern.
func TestEveryKnownToolCanBeOverridden(t *testing.T) {
	s := open(t)
	for name := range tool.Known {
		for _, pol := range []surface.Policy{surface.PolicyAllow, surface.PolicyAsk, surface.PolicyDeny} {
			if _, _, err := s.Set("backend", name, pol); err != nil {
				t.Errorf("Set(%s=%s) refused: %v\n  consequence: a tool a "+
					"blueprint can grant that an operator cannot govern", name, pol, err)
			}
		}
	}
}

// TestOpenRefusesAnEmptyDirectory rather than defaulting to the cwd.
//
// A store rooted at "" writes policy files into whatever directory the process
// happens to be in, which is how a config lands somewhere nobody looks.
func TestOpenRefusesAnEmptyDirectory(t *testing.T) {
	if _, err := Open("  "); err == nil {
		t.Fatal("Open accepted an empty directory; policies would land in the cwd")
	}
}
