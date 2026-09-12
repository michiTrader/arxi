package workspace

import (
	"strings"
	"testing"
)

func currentCapabilities(platform string) Capabilities {
	return Capabilities{
		Schema: SchemaV1, CapabilityVersion: "arxi.workspace-capabilities/initial-v1", Platform: platform,
		Modes: []Mode{ModeNone, ModeShared},
		Profiles: []Profile{
			{Schema: ProfileSchemaV1, ID: NoToolsProfileID, FileAccess: FileAccessNone,
				Process: ProcessProfile{Descendants: "unavailable", Filesystem: "unavailable", Environment: "unavailable", Network: "unavailable"}},
			{Schema: ProfileSchemaV1, ID: DirectFilesProfileID, FileAccess: FileAccessWrite,
				HandleRelative: true, FinalLinkRaceFree: platform != "windows",
				Process: ProcessProfile{Descendants: "unavailable", Filesystem: "unavailable", Environment: "unavailable", Network: "unavailable"}},
		},
		Provisioners: map[Mode]string{ModeNone: "arxi.workspace.none/v1", ModeShared: "arxi.workspace.shared-current-directory/v1"},
	}
}

func TestPreflightRefusesUnimplementedSourceProvisioners(t *testing.T) {
	for _, mode := range []Mode{ModeCopy, ModeWorktree} {
		_, err := Preflight([]Requirement{{Schema: SchemaV1, Member: "writer", Mode: mode,
			FileAccess: FileAccessWrite, RequiresSource: true, ProfileID: DirectFilesProfileID}}, currentCapabilities("linux"))
		if err == nil || !strings.Contains(err.Error(), "does not provide") {
			t.Errorf("%s preflight error = %v: a nominal per-member directory is not a source snapshot or Git worktree; refuse until that provisioner exists", mode, err)
		}
	}
}

func TestPreflightRefusesUnsupportedProcessGuarantees(t *testing.T) {
	requirement := Requirement{Schema: SchemaV1, Member: "shell", Mode: ModeShared,
		FileAccess: FileAccessWrite, RequiresSource: true, RequiresBash: true, ProfileID: ContainedProcessProfileID}
	_, err := Preflight([]Requirement{requirement}, currentCapabilities("linux"))
	if err == nil || !strings.Contains(err.Error(), "does not provide") {
		t.Fatalf("strong process profile preflight error = %v: current bash inherits process authority, environment and network; never downgrade that request to unrestricted execution", err)
	}
}

func TestCurrentLinuxAndWindowsCapabilitiesReportOnlyProvenGuarantees(t *testing.T) {
	for _, platform := range []string{"linux", "windows"} {
		capabilities := currentCapabilities(platform)
		if err := ValidateCapabilities(capabilities); err != nil {
			t.Fatalf("%s current capabilities are internally invalid: an adapter cannot make an honest preflight decision: %v", platform, err)
		}
		if len(capabilities.Modes) != 2 || capabilities.Modes[0] != ModeNone || capabilities.Modes[1] != ModeShared {
			t.Errorf("%s modes = %v: copy and worktree must stay unadvertised until their real provisioners land", platform, capabilities.Modes)
		}
		if capabilities.Profiles[1].FinalLinkRaceFree != (platform != "windows") {
			t.Errorf("%s final-link race-free capability = %v: Windows must report its check/open gap while Linux reports O_NOFOLLOW", platform, capabilities.Profiles[1].FinalLinkRaceFree)
		}
	}
}
