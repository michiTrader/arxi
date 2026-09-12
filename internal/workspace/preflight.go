package workspace

import (
	"fmt"
	"sort"
)

func ValidateCapabilities(capabilities Capabilities) error {
	if capabilities.Schema != SchemaV1 {
		return fmt.Errorf("workspace capability schema %q is unsupported", capabilities.Schema)
	}
	if capabilities.CapabilityVersion == "" || capabilities.Platform == "" {
		return fmt.Errorf("workspace capabilities require platform and capability version")
	}
	seenModes := map[Mode]bool{}
	for _, mode := range capabilities.Modes {
		if err := mode.Validate(); err != nil {
			return err
		}
		if seenModes[mode] {
			return fmt.Errorf("workspace capability mode %q is duplicated", mode)
		}
		seenModes[mode] = true
		if capabilities.Provisioners[mode] == "" {
			return fmt.Errorf("workspace mode %q has no provisioner version", mode)
		}
	}
	seenProfiles := map[string]bool{}
	for _, profile := range capabilities.Profiles {
		if profile.Schema != ProfileSchemaV1 || profile.ID == "" {
			return fmt.Errorf("workspace capability contains an invalid profile")
		}
		if _, err := profile.Identity(); err != nil {
			return err
		}
		if profile.Command != nil {
			command := profile.Command
			if command.Schema != CommandSchemaV1 || command.RunnerVersion == "" || command.Executable == "" ||
				command.EnvironmentVersion == "" || command.Descendants == "" || command.Filesystem == "" ||
				command.Network == "" || command.OutputLimitBytes <= 0 {
				return fmt.Errorf("workspace profile %q contains an invalid command profile", profile.ID)
			}
		}
		if seenProfiles[profile.ID] {
			return fmt.Errorf("workspace profile %q is duplicated", profile.ID)
		}
		seenProfiles[profile.ID] = true
	}
	return nil
}

func Preflight(requirements []Requirement, capabilities Capabilities) ([]PlatformDecision, error) {
	if err := ValidateCapabilities(capabilities); err != nil {
		return nil, err
	}
	modes := map[Mode]bool{}
	for _, mode := range capabilities.Modes {
		modes[mode] = true
	}
	profiles := map[string]Profile{}
	for _, profile := range capabilities.Profiles {
		profiles[profile.ID] = profile
	}
	out := make([]PlatformDecision, 0, len(requirements))
	for _, requirement := range requirements {
		if err := ValidateRequirement(requirement); err != nil {
			return nil, err
		}
		if !modes[requirement.Mode] {
			return nil, fmt.Errorf("member %q requests workspace mode %s, but platform %s does not provide it", requirement.Member, requirement.Mode, capabilities.Platform)
		}
		profile, ok := profiles[requirement.ProfileID]
		if !ok {
			return nil, fmt.Errorf("member %q requests workspace profile %q, but platform %s does not provide it", requirement.Member, requirement.ProfileID, capabilities.Platform)
		}
		if requirement.FileAccess == FileAccessWrite && profile.FileAccess != FileAccessWrite {
			return nil, fmt.Errorf("member %q requires write access, but workspace profile %q provides %s", requirement.Member, profile.ID, profile.FileAccess)
		}
		if requirement.FileAccess == FileAccessRead && profile.FileAccess == FileAccessNone {
			return nil, fmt.Errorf("member %q requires read access, but workspace profile %q provides none", requirement.Member, profile.ID)
		}
		if requirement.RequiresBash {
			if profile.Command == nil {
				return nil, fmt.Errorf("member %q requires bash, but profile %q has no platform command runner", requirement.Member, profile.ID)
			}
			command := profile.Command
			if command.Descendants != "contained" {
				return nil, fmt.Errorf("member %q requires contained process descendants, but profile %q provides %s", requirement.Member, profile.ID, command.Descendants)
			}
			if command.Filesystem != "workspace-only" {
				return nil, fmt.Errorf("member %q requires workspace-only process filesystem reach, but profile %q provides %s", requirement.Member, profile.ID, command.Filesystem)
			}
			if command.EnvironmentVersion != EnvironmentAllowlistV1 {
				return nil, fmt.Errorf("member %q requires allowlisted process environment, but profile %q provides %s", requirement.Member, profile.ID, command.EnvironmentVersion)
			}
			if command.Network != "denied" {
				return nil, fmt.Errorf("member %q requires denied process network reach, but profile %q provides %s", requirement.Member, profile.ID, command.Network)
			}
		}
		identity, err := profile.Identity()
		if err != nil {
			return nil, err
		}
		out = append(out, PlatformDecision{Schema: SchemaV1, Member: requirement.Member, Platform: capabilities.Platform,
			CapabilityVersion: capabilities.CapabilityVersion, ProfileID: profile.ID, ProfileIdentity: identity,
			ProvisionerVersion: capabilities.Provisioners[requirement.Mode], Command: profile.Command})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Member < out[j].Member })
	return out, nil
}
