package workspace

import (
	"reflect"
	"strings"
	"testing"
)

func TestModeValidationUsesTheExactFrozenSet(t *testing.T) {
	for _, value := range []string{"none", "shared", "copy", "worktree"} {
		if _, err := ParseMode(value); err != nil {
			t.Errorf("supported workspace mode %q was refused: the frozen surface and ADR-0012 would disagree; keep parsing on the exact four-mode contract: %v", value, err)
		}
	}
	for _, value := range []string{"", "auto", "private", "COPY"} {
		if _, err := ParseMode(value); err == nil {
			t.Errorf("unsupported workspace mode %q was accepted: a misspelling could silently select weaker source layout; reject values outside the exact contract", value)
		}
	}
}

func TestResolveUsesVerifiedSourceForReadersAndSeparationForWriters(t *testing.T) {
	got, err := Resolve(ResolutionInput{Members: []Member{
		{Name: "text"},
		{Name: "reader", Tools: []string{"read"}},
		{Name: "writer", Tools: []string{"write"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := []Requirement{
		{Schema: SchemaV1, Member: "reader", Mode: ModeShared, FileAccess: FileAccessRead, RequiresSource: true, ProfileID: DirectFilesReadProfileID},
		{Schema: SchemaV1, Member: "text", Mode: ModeNone, FileAccess: FileAccessNone, ProfileID: NoToolsProfileID},
		{Schema: SchemaV1, Member: "writer", Mode: ModeCopy, FileAccess: FileAccessWrite, RequiresSource: true, ProfileID: DirectFilesProfileID},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolved workspace requirements = %#v, want %#v: text-only work must stay filesystem-free, readers must resolve to the read-only profile so read-only-ness is carried by the workspace contract and not by the absence of a write grant, and a file-only writer must resolve to the layout whose root holds tracked files and nothing else", got, want)
	}
}

// TestAFileOnlyWriterGetsNoRepositoryControlPlaneAndABashUserDoes pins the
// distinction the resolution above rests on.
//
// This test exists because the assertion it protects reads like an arbitrary
// preference. It is not. `copy` and `worktree` are, per combine(), "distinct
// source layouts with no honest strength ordering" -- both isolate one tree per
// writing member, so neither is the stronger separation. What differs is what
// else sits in the root: a worktree root also holds a `.git` pointer into the
// operator's repository, which ADR-0018 has to refuse at the tool boundary for
// read and write alike.
//
// A member without `bash` cannot invoke Git at all, so the repository a
// worktree serves buys it nothing while adding a surface the writer platform
// decision would have to make promises about. A `bash` member is the opposite:
// a contained command is expected to be able to run Git, which needs the
// repository worktree provides and copy does not.
//
// If a future change routes file-only writers back to worktree, the writer
// advertisement inherits a control-plane clause it does not need, and this test
// is where that has to be argued rather than assumed.
func TestAFileOnlyWriterGetsNoRepositoryControlPlaneAndABashUserDoes(t *testing.T) {
	fileOnly, err := Resolve(ResolutionInput{Members: []Member{{Name: "w", Tools: []string{"write", "edit"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if fileOnly[0].Mode != ModeCopy {
		t.Errorf("a file-only writer resolved to %q, want copy: a worktree root carries a gitdir: pointer into the operator's repository, and a member that cannot run Git gains nothing from the repository that pointer serves", fileOnly[0].Mode)
	}

	withBash, err := Resolve(ResolutionInput{Members: []Member{{Name: "w", Tools: []string{"write", "bash"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if withBash[0].Mode != ModeWorktree {
		t.Errorf("a bash user resolved to %q, want worktree: a contained command is expected to be able to run Git, and copy provides no repository to run it against", withBash[0].Mode)
	}
}

func TestResolveFreezesOneModeAcrossParticipatingStages(t *testing.T) {
	got, err := Resolve(ResolutionInput{
		TopLevel: ModeShared,
		Members:  []Member{{Name: "writer", Tools: []string{"write"}, Stages: []string{"build"}}},
		Stages:   []Stage{{Name: "build", Mode: ModeCopy}, {Name: "review", Mode: ModeWorktree}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Mode != ModeCopy {
		t.Fatalf("effective mode = %q, want copy: declarations for stages a member never enters must not replace that member's frozen workspace", got[0].Mode)
	}
}

func TestResolveRejectsAmbiguousCopyAndWorktreeStages(t *testing.T) {
	_, err := Resolve(ResolutionInput{
		Members: []Member{{Name: "writer", Tools: []string{"write"}}},
		Stages:  []Stage{{Name: "build", Mode: ModeCopy}, {Name: "review", Mode: ModeWorktree}},
	})
	if err == nil || !strings.Contains(err.Error(), "no honest strength ordering") {
		t.Fatalf("copy/worktree ambiguity error = %v: choosing either layout would discard an explicit declaration and swap source semantics between stages; reject instead of guessing", err)
	}
}

func TestValidateRequirementRejectsNoneForFilesystemOrProcessWork(t *testing.T) {
	for _, requirement := range []Requirement{
		{Schema: SchemaV1, Member: "reader", Mode: ModeNone, FileAccess: FileAccessRead, RequiresSource: true, ProfileID: DirectFilesReadProfileID},
		{Schema: SchemaV1, Member: "shell", Mode: ModeNone, FileAccess: FileAccessWrite, RequiresSource: true, RequiresBash: true, ProfileID: ContainedProcessProfileID},
	} {
		if err := ValidateRequirement(requirement); err == nil {
			t.Errorf("invalid none requirement %#v was accepted: the run would advertise no filesystem while dispatching work that needs one; fail before acceptance", requirement)
		}
	}
}
