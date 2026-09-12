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
		if requirement.RequiresBash && (profile.Process.Descendants != "contained" || profile.Process.Filesystem != "workspace-only" || profile.Process.Environment != "allowlist" || profile.Process.Network != "denied") {
			return nil, fmt.Errorf("member %q requires contained process descendants, workspace-only filesystem, allowlisted environment, and denied network; profile %q does not provide all four guarantees", requirement.Member, profile.ID)
		}
		out = append(out, PlatformDecision{Schema: SchemaV1, Member: requirement.Member, Platform: capabilities.Platform,
			CapabilityVersion: capabilities.CapabilityVersion, ProfileID: profile.ID,
			ProvisionerVersion: capabilities.Provisioners[requirement.Mode]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Member < out[j].Member })
	return out, nil
}
