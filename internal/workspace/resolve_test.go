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
		{Schema: SchemaV1, Member: "reader", Mode: ModeShared, FileAccess: FileAccessRead, RequiresSource: true, ProfileID: DirectFilesProfileID},
		{Schema: SchemaV1, Member: "text", Mode: ModeNone, FileAccess: FileAccessNone, ProfileID: NoToolsProfileID},
		{Schema: SchemaV1, Member: "writer", Mode: ModeWorktree, FileAccess: FileAccessWrite, RequiresSource: true, ProfileID: DirectFilesProfileID},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolved workspace requirements = %#v, want %#v: text-only work must stay filesystem-free, readers need a verified source view, and writers need the strongest default separation", got, want)
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
		{Schema: SchemaV1, Member: "reader", Mode: ModeNone, FileAccess: FileAccessRead, RequiresSource: true, ProfileID: DirectFilesProfileID},
		{Schema: SchemaV1, Member: "shell", Mode: ModeNone, FileAccess: FileAccessWrite, RequiresSource: true, RequiresBash: true, ProfileID: ContainedProcessProfileID},
	} {
		if err := ValidateRequirement(requirement); err == nil {
			t.Errorf("invalid none requirement %#v was accepted: the run would advertise no filesystem while dispatching work that needs one; fail before acceptance", requirement)
		}
	}
}
