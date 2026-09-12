package workspace

import (
	"reflect"
	"strings"
	"testing"
)

func TestPreflightRefusesUnimplementedSourceProvisioners(t *testing.T) {
	for _, mode := range []Mode{ModeCopy, ModeWorktree} {
		_, err := Preflight([]Requirement{{Schema: SchemaV1, Member: "writer", Mode: mode,
			FileAccess: FileAccessWrite, RequiresSource: true, ProfileID: DirectFilesProfileID}}, CurrentCapabilities("linux"))
		if err == nil || !strings.Contains(err.Error(), "does not provide") {
			t.Errorf("%s preflight error = %v: a nominal per-member directory is not a source snapshot or Git worktree; refuse until that provisioner exists", mode, err)
		}
	}
}

func TestPreflightRefusesUnsupportedProcessGuarantees(t *testing.T) {
	requirement := Requirement{Schema: SchemaV1, Member: "shell", Mode: ModeShared,
		FileAccess: FileAccessWrite, RequiresSource: true, RequiresBash: true, ProfileID: ContainedProcessProfileID}
	_, err := Preflight([]Requirement{requirement}, CurrentCapabilities("linux"))
	if err == nil || !strings.Contains(err.Error(), "does not provide") {
		t.Fatalf("strong process profile preflight error = %v: current bash inherits process authority, environment and network; never downgrade that request to unrestricted execution", err)
	}
}

func TestCurrentLinuxAndWindowsCapabilitiesReportOnlyProvenGuarantees(t *testing.T) {
	for _, tc := range []struct {
		platform string
		profiles []string
	}{
		{platform: "windows", profiles: []string{NoToolsProfileID}},
		{platform: "linux", profiles: []string{NoToolsProfileID, DirectFilesProfileID}},
	} {
		capabilities := CurrentCapabilities(tc.platform)
		if err := ValidateCapabilities(capabilities); err != nil {
			t.Fatalf("%s current capabilities are internally invalid: an adapter cannot make an honest preflight decision: %v", tc.platform, err)
		}
		if len(capabilities.Modes) != 1 || capabilities.Modes[0] != ModeNone {
			t.Errorf("%s modes = %v: shared cannot claim a verified source view yet, and copy/worktree must stay unadvertised until their production provisioners guarantee the full contract", tc.platform, capabilities.Modes)
		}
		gotProfiles := make([]string, len(capabilities.Profiles))
		for i, profile := range capabilities.Profiles {
			gotProfiles[i] = profile.ID
			if profile.Command != nil || profile.ID == ContainedProcessProfileID {
				t.Errorf("%s profile %q advertises command containment: no native platform may expose bash until descendant, filesystem, environment, and network guarantees are all enforced", tc.platform, profile.ID)
			}
		}
		if !reflect.DeepEqual(gotProfiles, tc.profiles) {
			t.Errorf("%s profile IDs = %v, want %v: platform documentation and preflight must describe the exact advertised profiles", tc.platform, gotProfiles, tc.profiles)
		}
		if tc.platform == "linux" && !capabilities.Profiles[1].HandleRelative || tc.platform == "linux" && !capabilities.Profiles[1].FinalLinkRaceFree {
			t.Errorf("Linux direct-file profile = %#v: handle-relative openat plus O_NOFOLLOW must be the advertised strong guarantee", capabilities.Profiles[1])
		}
	}
}
