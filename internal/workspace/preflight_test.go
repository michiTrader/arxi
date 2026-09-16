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

func TestCurrentCapabilitiesAdvertiseExactlyTheADR0017Matrix(t *testing.T) {
	for _, tc := range []struct {
		platform string
		modes    []Mode
		profiles []string
	}{
		{platform: "windows", modes: []Mode{ModeNone}, profiles: []string{NoToolsProfileID}},
		{platform: "linux", modes: []Mode{ModeNone, ModeShared}, profiles: []string{NoToolsProfileID, DirectFilesReadProfileID}},
		{platform: "simulation", modes: []Mode{ModeNone, ModeShared, ModeCopy, ModeWorktree},
			profiles: []string{NoToolsProfileID, DirectFilesReadProfileID, DirectFilesProfileID, ContainedProcessProfileID}},
	} {
		capabilities := CurrentCapabilities(tc.platform)
		if err := ValidateCapabilities(capabilities); err != nil {
			t.Fatalf("%s current capabilities are internally invalid: an adapter cannot make an honest preflight decision: %v", tc.platform, err)
		}
		if !reflect.DeepEqual(capabilities.Modes, tc.modes) {
			t.Errorf("%s modes = %v, want %v: the advertised mode set is the platform promise itself, so it must match ADR-0017 exactly", tc.platform, capabilities.Modes, tc.modes)
		}
		gotProfiles := make([]string, len(capabilities.Profiles))
		for i, profile := range capabilities.Profiles {
			gotProfiles[i] = profile.ID
			if profile.Command != nil && tc.platform != "simulation" {
				t.Errorf("%s profile %q advertises command containment: no native platform may expose bash until descendant, filesystem, environment, and network guarantees are all enforced", tc.platform, profile.ID)
			}
		}
		if !reflect.DeepEqual(gotProfiles, tc.profiles) {
			t.Errorf("%s profile IDs = %v, want %v: platform documentation and preflight must describe the exact advertised profiles", tc.platform, gotProfiles, tc.profiles)
		}
		if tc.platform == "linux" {
			read := capabilities.Profiles[1]
			if read.FileAccess != FileAccessRead || !read.HandleRelative || !read.FinalLinkRaceFree {
				t.Errorf("Linux read-only profile = %#v: shared availability promises handle-relative, final-link-race-free READ access; anything stronger reopens shared+write", read)
			}
			if capabilities.Provisioners[ModeShared] != GitLayoutProvisionerV1 {
				t.Errorf("Linux shared provisioner = %q, want %q: an advertised mode without its real provisioner version is availability with no machinery behind it", capabilities.Provisioners[ModeShared], GitLayoutProvisionerV1)
			}
		}
	}
}

func TestLinuxPreflightAcceptsReadersAndRefusesEveryWriteCombination(t *testing.T) {
	caps := CurrentCapabilities("linux")

	requirements, err := Resolve(ResolutionInput{Members: []Member{{Name: "reader", Tools: []string{"read", "grep"}}}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	decisions, err := Preflight(requirements, caps)
	if err != nil {
		t.Fatalf("read/grep requirement refused against Linux capabilities: %v\n"+
			"  ADR-0017 exists to make this the one accepted source-backed combination; "+
			"refusing it here blocks the read-only milestone on every platform", err)
	}
	if decisions[0].ProfileID != DirectFilesReadProfileID {
		t.Fatalf("reader decision profile = %q, want %q: a reader accepted with the write-capable profile would make read-only a grant accident", decisions[0].ProfileID, DirectFilesReadProfileID)
	}

	for _, tc := range []struct {
		name        string
		requirement Requirement
		fragment    string
	}{
		{name: "write requirement over the read-only profile",
			requirement: Requirement{Schema: SchemaV1, Member: "writer", Mode: ModeShared, FileAccess: FileAccessWrite, RequiresSource: true, ProfileID: DirectFilesReadProfileID},
			fragment:    "provides read"},
		{name: "shared writer resolves to the unadvertised write-capable profile",
			requirement: Requirement{Schema: SchemaV1, Member: "writer", Mode: ModeShared, FileAccess: FileAccessWrite, RequiresSource: true, ProfileID: DirectFilesProfileID},
			fragment:    "does not provide"},
		{name: "worktree writer mode is unadvertised",
			requirement: Requirement{Schema: SchemaV1, Member: "writer", Mode: ModeWorktree, FileAccess: FileAccessWrite, RequiresSource: true, ProfileID: DirectFilesProfileID},
			fragment:    "does not provide"},
	} {
		_, err := Preflight([]Requirement{tc.requirement}, caps)
		if err == nil || !strings.Contains(err.Error(), tc.fragment) {
			t.Errorf("%s: preflight error = %v\n"+
				"  every path to a writable view on Linux must be refused at acceptance; "+
				"the read-only promise only holds if no accepted combination can write", tc.name, err)
		}
	}
}

func TestSimulationStillAcceptsWritersWhileLinuxDoesNot(t *testing.T) {
	requirements, err := Resolve(ResolutionInput{Members: []Member{
		{Name: "reader", Tools: []string{"read"}},
		{Name: "writer", Tools: []string{"write"}},
		{Name: "shell", Tools: []string{"bash"}},
	}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, err := Preflight(requirements, CurrentCapabilities("simulation")); err != nil {
		t.Fatalf("simulation refused a routine reader/writer/shell blueprint: %v\n"+
			"  simulation must keep exercising every mode and profile, or the pure "+
			"machinery drifts behind whatever production currently dares to promise", err)
	}
}
