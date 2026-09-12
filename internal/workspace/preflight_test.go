package workspace

import (
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
	for _, platform := range []string{"linux", "windows"} {
		capabilities := CurrentCapabilities(platform)
		if err := ValidateCapabilities(capabilities); err != nil {
			t.Fatalf("%s current capabilities are internally invalid: an adapter cannot make an honest preflight decision: %v", platform, err)
		}
		if len(capabilities.Modes) != 1 || capabilities.Modes[0] != ModeNone {
			t.Errorf("%s modes = %v: shared cannot claim a verified source view yet, and copy/worktree must stay unadvertised until their real provisioners land", platform, capabilities.Modes)
		}
		if platform == "linux" {
			if len(capabilities.Profiles) != 2 || !capabilities.Profiles[1].FinalLinkRaceFree {
				t.Errorf("Linux direct-file profile = %#v: handle-relative openat plus O_NOFOLLOW must be the advertised strong guarantee", capabilities.Profiles)
			}
		} else if len(capabilities.Profiles) != 1 {
			t.Errorf("Windows profiles = %#v: no direct-file profile may be advertised until reparse-safe handle traversal exists", capabilities.Profiles)
		}
	}
}
