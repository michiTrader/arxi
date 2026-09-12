package workspace

import (
	"fmt"
	"sort"
	"strings"
)

const DefaultProfileID = "arxi.workspace/direct-files-v1"

func Resolve(input ResolutionInput) ([]Requirement, error) {
	if input.TopLevel != "" {
		if err := input.TopLevel.Validate(); err != nil {
			return nil, fmt.Errorf("top-level declaration: %w", err)
		}
	}
	stageNames := map[string]bool{}
	for _, stage := range input.Stages {
		if strings.TrimSpace(stage.Name) == "" {
			return nil, fmt.Errorf("workspace stage name is required")
		}
		if stageNames[stage.Name] {
			return nil, fmt.Errorf("workspace stage %q is duplicated", stage.Name)
		}
		stageNames[stage.Name] = true
		if stage.Mode != "" {
			if err := stage.Mode.Validate(); err != nil {
				return nil, fmt.Errorf("stage %q declaration: %w", stage.Name, err)
			}
		}
	}

	out := make([]Requirement, 0, len(input.Members))
	seenMembers := map[string]bool{}
	for _, member := range input.Members {
		if strings.TrimSpace(member.Name) == "" {
			return nil, fmt.Errorf("workspace member name is required")
		}
		if seenMembers[member.Name] {
			return nil, fmt.Errorf("workspace member %q is duplicated", member.Name)
		}
		seenMembers[member.Name] = true
		access, bash := accessFor(member.Tools)
		selected := input.TopLevel
		if selected == "" {
			if access == FileAccessWrite || bash {
				selected = ModeWorktree
			} else if access == FileAccessRead {
				selected = ModeShared
			} else {
				selected = ModeNone
			}
		}
		for _, stage := range input.Stages {
			if stage.Mode == "" || !participates(member, stage.Name) {
				continue
			}
			var err error
			selected, err = combine(selected, stage.Mode)
			if err != nil {
				return nil, fmt.Errorf("member %q stage %q: %w", member.Name, stage.Name, err)
			}
		}
		requirement := Requirement{Schema: SchemaV1, Member: member.Name, Mode: selected,
			FileAccess: access, RequiresSource: access != FileAccessNone || bash,
			RequiresBash: bash, ProfileID: DefaultProfileID}
		if err := ValidateRequirement(requirement); err != nil {
			return nil, err
		}
		out = append(out, requirement)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Member < out[j].Member })
	return out, nil
}

func ValidateRequirement(requirement Requirement) error {
	if requirement.Schema != SchemaV1 {
		return fmt.Errorf("member %q workspace requirement schema %q is unsupported", requirement.Member, requirement.Schema)
	}
	if strings.TrimSpace(requirement.Member) == "" {
		return fmt.Errorf("workspace requirement member is required")
	}
	if err := requirement.Mode.Validate(); err != nil {
		return fmt.Errorf("member %q: %w", requirement.Member, err)
	}
	switch requirement.FileAccess {
	case FileAccessNone, FileAccessRead, FileAccessWrite:
	default:
		return fmt.Errorf("member %q file access %q is unsupported", requirement.Member, requirement.FileAccess)
	}
	if requirement.ProfileID == "" {
		return fmt.Errorf("member %q workspace profile is required", requirement.Member)
	}
	if requirement.Mode == ModeNone && (requirement.RequiresSource || requirement.FileAccess != FileAccessNone || requirement.RequiresBash) {
		return fmt.Errorf("member %q requests workspace none but requires source, file, or process access", requirement.Member)
	}
	if requirement.Mode != ModeNone && !requirement.RequiresSource {
		return fmt.Errorf("member %q requests workspace %s without a source requirement; use none for text-only work", requirement.Member, requirement.Mode)
	}
	return nil
}

func combine(left, right Mode) (Mode, error) {
	if left == right {
		return left, nil
	}
	if left == ModeNone {
		return right, nil
	}
	if right == ModeNone {
		return left, nil
	}
	if left == ModeShared {
		return right, nil
	}
	if right == ModeShared {
		return left, nil
	}
	return "", fmt.Errorf("workspace modes %s and %s are distinct source layouts with no honest strength ordering; choose one mode for all stages", left, right)
}

func accessFor(tools []string) (FileAccess, bool) {
	access := FileAccessNone
	bash := false
	for _, tool := range tools {
		switch tool {
		case "bash":
			bash = true
			access = FileAccessWrite
		case "write", "edit":
			access = FileAccessWrite
		case "read", "grep":
			if access == FileAccessNone {
				access = FileAccessRead
			}
		}
	}
	return access, bash
}

func participates(member Member, stage string) bool {
	if len(member.Stages) == 0 {
		return true
	}
	for _, declared := range member.Stages {
		if declared == stage {
			return true
		}
	}
	return false
}
