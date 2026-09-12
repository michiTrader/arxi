package authorization

import (
	"strings"
	"testing"
)

func validActionInput() ActionInput {
	return ActionInput{
		JobID:                 "job-17",
		RunID:                 "run-29",
		RequesterPrincipal:    "agent:backend",
		SuspendedParentWorkID: "work-parent-5",
		ProviderCallID:        "provider-call-11",
		ToolName:              "write_file",
		ArgumentDigest:        strings.Repeat("a", 64),
		ToolSchemaVersion:     "arxi.tool.write-file/v3",
		PolicyVersion:         "policy-2026-09-11",
		WorkspaceProfileID:    "workspace-profile-41",
	}
}

func mustAction(t *testing.T, input ActionInput) Action {
	t.Helper()
	action, err := NewAction(input)
	if err != nil {
		t.Fatalf("a valid exact action was rejected: authorization cannot protect tool dispatch; accept every complete, well-formed binding: %v", err)
	}
	return action
}

func TestActionDigestIsDeterministicAndUnambiguous(t *testing.T) {
	input := validActionInput()
	first := mustAction(t, input)
	second := mustAction(t, input)
	if first.Digest() != second.Digest() {
		t.Fatalf("the same exact action produced different digests %q and %q: a persisted grant could not authorize its own suspended call; use one deterministic canonical field order", first.Digest(), second.Digest())
	}
	if len(first.Digest()) != 64 || first.Digest() != strings.ToLower(first.Digest()) {
		t.Fatalf("action digest %q is not lowercase SHA-256: adapters could compare distinct encodings inconsistently; return the full 64-character lowercase hexadecimal digest", first.Digest())
	}

	left := input
	left.JobID, left.RunID = "ab", "c"
	right := input
	right.JobID, right.RunID = "a", "bc"
	if mustAction(t, left).Digest() == mustAction(t, right).Digest() {
		t.Fatal("different field boundaries produced one action digest: a grant could authorize a different job/run pair; length-frame every field in a fixed order")
	}
}

func TestChangingEveryBoundFieldChangesActionDigest(t *testing.T) {
	base := validActionInput()
	baseDigest := mustAction(t, base).Digest()
	tests := []struct {
		name   string
		change func(*ActionInput)
	}{
		{"job id", func(v *ActionInput) { v.JobID = "job-other" }},
		{"run id", func(v *ActionInput) { v.RunID = "run-other" }},
		{"requester principal", func(v *ActionInput) { v.RequesterPrincipal = "agent:reviewer" }},
		{"parent work id", func(v *ActionInput) { v.SuspendedParentWorkID = "work-parent-other" }},
		{"provider call id", func(v *ActionInput) { v.ProviderCallID = "provider-call-other" }},
		{"tool name", func(v *ActionInput) { v.ToolName = "delete_file" }},
		{"argument digest", func(v *ActionInput) { v.ArgumentDigest = strings.Repeat("b", 64) }},
		{"tool schema version", func(v *ActionInput) { v.ToolSchemaVersion = "arxi.tool.write-file/v4" }},
		{"policy version", func(v *ActionInput) { v.PolicyVersion = "policy-other" }},
		{"workspace profile id", func(v *ActionInput) { v.WorkspaceProfileID = "workspace-profile-other" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := base
			tt.change(&changed)
			if got := mustAction(t, changed).Digest(); got == baseDigest {
				t.Fatalf("changing %s left action digest %q unchanged: a grant could authorize work outside its exact binding; include this field in the versioned canonical digest", tt.name, got)
			}
		})
	}
}

func TestNewActionRejectsEveryEmptyRequiredField(t *testing.T) {
	tests := []struct {
		name  string
		empty func(*ActionInput)
	}{
		{"job id", func(v *ActionInput) { v.JobID = "" }},
		{"run id", func(v *ActionInput) { v.RunID = "" }},
		{"requester principal", func(v *ActionInput) { v.RequesterPrincipal = "" }},
		{"parent work id", func(v *ActionInput) { v.SuspendedParentWorkID = "" }},
		{"provider call id", func(v *ActionInput) { v.ProviderCallID = "" }},
		{"tool name", func(v *ActionInput) { v.ToolName = "" }},
		{"argument digest", func(v *ActionInput) { v.ArgumentDigest = "" }},
		{"tool schema version", func(v *ActionInput) { v.ToolSchemaVersion = "" }},
		{"policy version", func(v *ActionInput) { v.PolicyVersion = "" }},
		{"workspace profile id", func(v *ActionInput) { v.WorkspaceProfileID = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := validActionInput()
			tt.empty(&input)
			if _, err := NewAction(input); err == nil {
				t.Fatalf("an action with empty %s was accepted: an incomplete grant could match unrelated work; reject the binding before hashing", tt.name)
			}
		})
	}
}

func TestNewActionRejectsMalformedArgumentDigests(t *testing.T) {
	tests := []struct {
		name   string
		digest string
	}{
		{"short", strings.Repeat("a", 63)},
		{"long", strings.Repeat("a", 65)},
		{"non hexadecimal", strings.Repeat("g", 64)},
		{"uppercase", strings.Repeat("A", 64)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := validActionInput()
			input.ArgumentDigest = tt.digest
			if _, err := NewAction(input); err == nil {
				t.Fatalf("a %s argument digest was accepted: authorization identity could diverge across producers; require exactly 64 lowercase hexadecimal characters before hashing", tt.name)
			}
		})
	}
}

func TestInputMutationCannotChangeComputedActionDigest(t *testing.T) {
	input := validActionInput()
	action := mustAction(t, input)
	want := action.Digest()

	input.JobID = "mutated-job"
	input.RunID = "mutated-run"
	input.RequesterPrincipal = "agent:mutated"
	input.SuspendedParentWorkID = "mutated-parent"
	input.ProviderCallID = "mutated-call"
	input.ToolName = "mutated-tool"
	input.ArgumentDigest = strings.Repeat("f", 64)
	input.ToolSchemaVersion = "mutated-schema"
	input.PolicyVersion = "mutated-policy"
	input.WorkspaceProfileID = "mutated-workspace"

	if got := action.Digest(); got != want {
		t.Fatalf("mutating constructor input changed an existing action digest from %q to %q: a stored grant would no longer name immutable work; copy every binding into value-owned state and cache its digest", want, got)
	}
}
