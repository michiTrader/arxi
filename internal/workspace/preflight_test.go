package workspace

import (
	"reflect"
	"strings"
	"testing"
)

// TestPreflightRefusesUnadvertisedSourceLayouts covers worktree only.
//
// It used to cover copy as well, on the grounds that no production
// provisioner existed for either. That is no longer true for copy: the
// git-layout provisioner materializes a tracked-tree snapshot, and its
// behaviour has been audited rather than assumed -- no control plane in the
// root, no inode shared with the source, writes confined to the snapshot and
// discarded on release (ADR-0019 cites the tests).
//
// worktree stays refused for a reason that has nothing to do with a missing
// provisioner: its root carries a gitdir: pointer into the operator's
// repository (ADR-0018). The layout works; what is unadvertised is the
// promise, which is the distinction ADR-0012 exists to keep visible.
func TestPreflightRefusesUnadvertisedSourceLayouts(t *testing.T) {
	_, err := Preflight([]Requirement{{Schema: SchemaV1, Member: "writer", Mode: ModeWorktree,
		FileAccess: FileAccessWrite, RequiresSource: true, ProfileID: DirectFilesProfileID}}, CurrentCapabilities("linux"))
	if err == nil || !strings.Contains(err.Error(), "does not provide") {
		t.Errorf("worktree preflight error = %v: the layout is implemented, but its root holds a "+
			"gitdir: pointer into the operator's repository, so advertising it would promise a "+
			"tracked tree and deliver a control plane as well", err)
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
		{platform: "linux", modes: []Mode{ModeNone, ModeShared, ModeCopy},
			profiles: []string{NoToolsProfileID, DirectFilesReadProfileID, DirectFilesProfileID}},
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
				t.Errorf("Linux read-only profile = %#v: shared availability promises handle-relative, final-link-race-free READ access", read)
			}
			write := capabilities.Profiles[2]
			if write.FileAccess != FileAccessWrite || !write.HandleRelative || !write.FinalLinkRaceFree {
				t.Errorf("Linux write profile = %#v: ADR-0019 advertises write only with the same "+
					"handle-relative, final-link-race-free guarantees the read profile makes; a "+
					"write profile promising less would be a weaker contract under the same name", write)
			}
			for _, mode := range []Mode{ModeShared, ModeCopy} {
				if capabilities.Provisioners[mode] != GitLayoutProvisionerV1 {
					t.Errorf("Linux %s provisioner = %q, want %q: an advertised mode without its real provisioner version is availability with no machinery behind it", mode, capabilities.Provisioners[mode], GitLayoutProvisionerV1)
				}
			}
			// The pairing is the load-bearing part of ADR-0019: both layouts
			// and both file profiles are advertised now, so without it the
			// cross product would offer write over the operator's shared tree.
			if offered := capabilities.Pairs[ModeShared]; len(offered) != 1 || offered[0] != DirectFilesReadProfileID {
				t.Errorf("Linux offers %v with shared, want only %q: advertising the write profile "+
					"for copy must not make the operator's shared tree writable (ADR-0017)",
					offered, DirectFilesReadProfileID)
			}
		}
	}
}

// TestLinuxPreflightAcceptsFileWorkAndStillRefusesTheSharedTreeAndProcesses
// replaces the ADR-0017-era test that asserted EVERY write was refused.
//
// That assertion was correct until ADR-0019 and is now false by decision: a
// file-only writer over copy is accepted. What survives unchanged is what
// ADR-0017 actually protects -- the operator's shared tree stays read-only --
// plus the process boundary, which no platform decision has moved.
func TestLinuxPreflightAcceptsFileWorkAndStillRefusesTheSharedTreeAndProcesses(t *testing.T) {
	caps := CurrentCapabilities("linux")

	requirements, err := Resolve(ResolutionInput{Members: []Member{{Name: "reader", Tools: []string{"read", "grep"}}}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	decisions, err := Preflight(requirements, caps)
	if err != nil {
		t.Fatalf("read/grep requirement refused against Linux capabilities: %v", err)
	}
	if decisions[0].ProfileID != DirectFilesReadProfileID {
		t.Fatalf("reader decision profile = %q, want %q: a reader accepted with the write-capable "+
			"profile would make read-only a grant accident", decisions[0].ProfileID, DirectFilesReadProfileID)
	}

	for _, tc := range []struct {
		name        string
		requirement Requirement
		fragment    string
	}{
		{name: "write requirement over the read-only profile",
			requirement: Requirement{Schema: SchemaV1, Member: "writer", Mode: ModeShared, FileAccess: FileAccessWrite, RequiresSource: true, ProfileID: DirectFilesReadProfileID},
			fragment:    "provides read"},
		// The case that matters most after ADR-0019. Both the shared layout
		// and the write profile are advertised now, so ONLY the pairing
		// refuses this. Before Pairs existed it would have been accepted.
		{name: "the operator's shared tree is still not writable",
			requirement: Requirement{Schema: SchemaV1, Member: "writer", Mode: ModeShared, FileAccess: FileAccessWrite, RequiresSource: true, ProfileID: DirectFilesProfileID},
			fragment:    "does not offer that combination"},
		{name: "worktree remains unadvertised, so its gitdir: pointer is unreachable",
			requirement: Requirement{Schema: SchemaV1, Member: "writer", Mode: ModeWorktree, FileAccess: FileAccessWrite, RequiresSource: true, ProfileID: DirectFilesProfileID},
			fragment:    "does not provide"},
		{name: "contained-process remains unadvertised",
			requirement: Requirement{Schema: SchemaV1, Member: "runner", Mode: ModeCopy, FileAccess: FileAccessWrite, RequiresSource: true, RequiresBash: true, ProfileID: ContainedProcessProfileID},
			fragment:    "does not provide"},
	} {
		_, err := Preflight([]Requirement{tc.requirement}, caps)
		if err == nil || !strings.Contains(err.Error(), tc.fragment) {
			t.Errorf("%s: preflight error = %v\n"+
				"  ADR-0019 widened Linux to file-only writes over copy and nothing else; each "+
				"case here is a boundary that widening must not have moved", tc.name, err)
		}
	}

	// And the combination the decision exists to offer.
	accepted, err := Resolve(ResolutionInput{Members: []Member{{Name: "writer", Tools: []string{"read", "write", "edit"}}}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, err := Preflight(accepted, caps); err != nil {
		t.Fatalf("a file-only writer was refused on Linux: %v\n"+
			"  ADR-0019 advertises copy with the write-capable profile precisely so this is "+
			"accepted; if it is refused the decision is not in effect", err)
	}
}

// TestEveryToolConfigurationLandsOnTheSideOfTheLineADR0019Drew walks real
// tool lists through Resolve and checks each against the boundary, rather
// than against a remembered assumption about which layout resolution picks.
//
// Its ancestor asserted that EVERY write configuration was refused. That was
// right under ADR-0017 and is now wrong by decision, but the reason it
// existed survives: when file-only writers moved from worktree to copy,
// nothing failed, because the guarantee was held by copy happening to be
// unadvertised rather than by a check. Driving real tool lists through
// Resolve is what makes this insensitive to that mapping.
//
// The line ADR-0019 draws is file access, not write access: a member that
// only touches files is accepted, a member that can start a process is not,
// because every process guarantee is still "unavailable".
func TestEveryToolConfigurationLandsOnTheSideOfTheLineADR0019Drew(t *testing.T) {
	caps := CurrentCapabilities("linux")
	for _, tc := range []struct {
		tools    []string
		accepted bool
	}{
		{tools: []string{"read"}, accepted: true},
		{tools: []string{"read", "grep"}, accepted: true},
		{tools: []string{"write"}, accepted: true},
		{tools: []string{"edit"}, accepted: true},
		{tools: []string{"write", "edit"}, accepted: true},
		{tools: []string{"read", "write"}, accepted: true},
		{tools: []string{"grep", "edit"}, accepted: true},

		// bash resolves to worktree AND the contained-process profile, so it
		// is refused twice over. Neither refusal is incidental: the layout
		// carries the operator's gitdir: pointer, and the profile claims
		// descendant, filesystem, environment and network guarantees that no
		// adapter enforces.
		{tools: []string{"bash"}, accepted: false},
		{tools: []string{"read", "bash"}, accepted: false},
		{tools: []string{"write", "bash"}, accepted: false},
	} {
		requirements, err := Resolve(ResolutionInput{Members: []Member{{Name: "w", Tools: tc.tools}}})
		if err != nil {
			if tc.accepted {
				t.Errorf("tools %v were refused by Resolve: %v", tc.tools, err)
			}
			continue
		}
		_, err = Preflight(requirements, caps)
		if tc.accepted && err != nil {
			t.Errorf("tools %v resolved to mode %q with profile %q and were REFUSED: %v\n"+
				"  ADR-0019 accepts file-only work on Linux; refusing it means the decision is "+
				"not in effect", tc.tools, requirements[0].Mode, requirements[0].ProfileID, err)
		}
		if !tc.accepted && err == nil {
			t.Errorf("tools %v resolved to mode %q with profile %q and were ACCEPTED.\n"+
				"  ADR-0019 deliberately stops at files: this configuration can start a process, "+
				"and descendants, filesystem, environment and network are all still "+
				"\"unavailable\", so nothing has proven what it would be contained by",
				tc.tools, requirements[0].Mode, requirements[0].ProfileID)
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
