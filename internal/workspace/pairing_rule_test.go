package workspace

import "testing"

// These tests cover the Pairs field added to close the gap #63 and #64
// recorded: modes and profiles were two independent lists, so an
// advertisement meant their cross product and "shared is offered only with
// the read-only profile" was true by arithmetic rather than by rule.
//
// The existing tests in pairing_test.go still pass and are still correct:
// they describe an advertisement that sets no pairs, and that case is
// deliberately unchanged. Nil means the cross product, so no existing
// advertisement -- native or host-declared -- changes behaviour.

func twoLayoutCapabilities(pairs map[Mode][]string) Capabilities {
	return Capabilities{
		Schema: SchemaV1, CapabilityVersion: CapabilityVersionInitialV1, Platform: "hypothetical",
		Modes: []Mode{ModeNone, ModeShared, ModeCopy},
		Profiles: []Profile{
			{Schema: ProfileSchemaV1, ID: NoToolsProfileID, FileAccess: FileAccessNone,
				Process: ProcessProfile{Descendants: "unavailable", Filesystem: "unavailable", Environment: "unavailable", Network: "unavailable"}},
			{Schema: ProfileSchemaV1, ID: DirectFilesReadProfileID, FileAccess: FileAccessRead,
				HandleRelative: true, FinalLinkRaceFree: true,
				Process: ProcessProfile{Descendants: "unavailable", Filesystem: "unavailable", Environment: "unavailable", Network: "unavailable"}},
			{Schema: ProfileSchemaV1, ID: DirectFilesProfileID, FileAccess: FileAccessWrite,
				HandleRelative: true, FinalLinkRaceFree: true,
				Process: ProcessProfile{Descendants: "unavailable", Filesystem: "unavailable", Environment: "unavailable", Network: "unavailable"}},
		},
		Provisioners: map[Mode]string{
			ModeNone: "arxi.workspace.none/v1", ModeShared: GitLayoutProvisionerV1, ModeCopy: GitLayoutProvisionerV1,
		},
		Pairs: pairs,
	}
}

func requirement(mode Mode, access FileAccess, profileID string) []Requirement {
	return []Requirement{{Schema: SchemaV1, Member: "w", Mode: mode, FileAccess: access,
		RequiresSource: true, ProfileID: profileID}}
}

// TestPairsMakeTheIntendedAdvertisementStatable is the whole point of the
// field: the platform that means "writes land in copy, shared is read-only"
// can now say exactly that, and be held to it.
//
// Before this, the same advertisement also accepted writing shared, because
// both the layout and the profile were advertised and nothing related them.
func TestPairsMakeTheIntendedAdvertisementStatable(t *testing.T) {
	capabilities := twoLayoutCapabilities(map[Mode][]string{
		ModeNone:   {NoToolsProfileID},
		ModeShared: {DirectFilesReadProfileID},
		ModeCopy:   {DirectFilesProfileID, DirectFilesReadProfileID},
	})

	if _, err := Preflight(requirement(ModeCopy, FileAccessWrite, DirectFilesProfileID), capabilities); err != nil {
		t.Fatalf("writing copy was refused, which is the combination this advertisement exists to offer: %v", err)
	}
	if _, err := Preflight(requirement(ModeShared, FileAccessRead, DirectFilesReadProfileID), capabilities); err != nil {
		t.Fatalf("reading shared was refused: %v", err)
	}

	err := Preflight2Err(t, capabilities, requirement(ModeShared, FileAccessWrite, DirectFilesProfileID))
	if err == nil {
		t.Fatal("writing SHARED was accepted under an advertisement that pairs shared with the " +
			"read-only profile only.\n" +
			"  This is the combination #63 and #64 found accepted by accident: for a platform " +
			"whose shared layout is the operator's own checkout, it is the difference between " +
			"editing a snapshot and editing the operator's files")
	}
}

// Preflight2Err runs Preflight and returns only the error, keeping the
// assertion above readable.
func Preflight2Err(t *testing.T, capabilities Capabilities, requirements []Requirement) error {
	t.Helper()
	_, err := Preflight(requirements, capabilities)
	return err
}

// TestAnAbsentModeInAnExplicitPairingOffersNothing pins the choice that a mode
// missing from a non-nil map carries no profile, rather than all of them.
//
// The opposite reading is tempting because it looks forgiving, and it is the
// one that makes the field useless: the restriction most worth stating is
// "this layout is not offered with any file profile", and a missing-key-means-
// everything rule cannot state it. It would also make a typo in a mode name
// silently widen the advertisement instead of narrowing it.
func TestAnAbsentModeInAnExplicitPairingOffersNothing(t *testing.T) {
	capabilities := twoLayoutCapabilities(map[Mode][]string{
		ModeNone: {NoToolsProfileID},
		ModeCopy: {DirectFilesProfileID},
	})

	if err := Preflight2Err(t, capabilities, requirement(ModeShared, FileAccessRead, DirectFilesReadProfileID)); err == nil {
		t.Fatal("a mode absent from an explicit pairing accepted a profile anyway.\n" +
			"  Then a pairing could never express \"this layout carries no file profile\", which " +
			"is the restriction it most needs to express, and a mistyped mode name would widen " +
			"the advertisement rather than narrow it")
	}
}

// TestAnAdvertisementWithoutPairsIsUnchanged is the compatibility guarantee,
// asserted rather than asserted in prose.
//
// Every advertisement that existed before this field -- including any a host
// declares through the public type -- has a nil Pairs, and must keep behaving
// exactly as it did. If this ever fails, the field stopped being additive and
// became a breaking change to a versioned public surface.
func TestAnAdvertisementWithoutPairsIsUnchanged(t *testing.T) {
	capabilities := twoLayoutCapabilities(nil)

	for _, c := range []struct {
		mode    Mode
		access  FileAccess
		profile string
	}{
		{ModeShared, FileAccessRead, DirectFilesReadProfileID},
		{ModeShared, FileAccessWrite, DirectFilesProfileID},
		{ModeCopy, FileAccessWrite, DirectFilesProfileID},
		{ModeCopy, FileAccessRead, DirectFilesReadProfileID},
	} {
		if err := Preflight2Err(t, capabilities, requirement(c.mode, c.access, c.profile)); err != nil {
			t.Errorf("%s + %s was refused under an advertisement with no pairs: %v\n"+
				"  Nil must mean the cross product. Any other reading silently narrows every "+
				"advertisement written before this field existed", c.mode, c.profile, err)
		}
	}
}

// TestAPairingThatNamesSomethingUnadvertisedIsRefused keeps a typo loud.
//
// A pairing naming a profile that is not advertised would silently offer
// nothing for that layout, and the symptom -- every run refused for what reads
// like a platform limitation -- points nowhere near the typo.
func TestAPairingThatNamesSomethingUnadvertisedIsRefused(t *testing.T) {
	if err := ValidateCapabilities(twoLayoutCapabilities(map[Mode][]string{
		ModeCopy: {"arxi.workspace/direct-files-v2"},
	})); err == nil {
		t.Error("a pairing naming an unadvertised profile validated: a typo would narrow the " +
			"advertisement to nothing and report it as a platform limitation")
	}

	if err := ValidateCapabilities(twoLayoutCapabilities(map[Mode][]string{
		ModeWorktree: {DirectFilesProfileID},
	})); err == nil {
		t.Error("a pairing naming an unadvertised mode validated: the same typo problem, on the " +
			"other key")
	}
}

// TestLinuxNowPairsByRuleRatherThanByArithmetic is the native half.
//
// ADR-0017's "shared only with the read-only profile" was previously true
// because the advertisement was 1x1. It is now stated. The distinction is only
// observable by widening the lists, which is what this does.
func TestLinuxNowPairsByRuleRatherThanByArithmetic(t *testing.T) {
	linux := CurrentCapabilities("linux")
	if linux.Pairs == nil {
		t.Fatal("the Linux advertisement states no pairs, so shared+read-only is still true only " +
			"because the product happens to be 1x1")
	}

	// Widen exactly as the pending writer decision would, WITHOUT extending
	// the pairing. The new profile must not become available on shared.
	linux.Modes = append(linux.Modes, ModeCopy)
	linux.Provisioners[ModeCopy] = GitLayoutProvisionerV1
	linux.Profiles = append(linux.Profiles, Profile{Schema: ProfileSchemaV1, ID: DirectFilesProfileID,
		FileAccess: FileAccessWrite, HandleRelative: true, FinalLinkRaceFree: true,
		Process: ProcessProfile{Descendants: "unavailable", Filesystem: "unavailable", Environment: "unavailable", Network: "unavailable"}})
	linux.Pairs[ModeCopy] = []string{DirectFilesProfileID}

	if _, err := Preflight(requirement(ModeCopy, FileAccessWrite, DirectFilesProfileID), linux); err != nil {
		t.Fatalf("the widened advertisement refuses the combination it was widened for: %v", err)
	}
	if err := Preflight2Err(t, linux, requirement(ModeShared, FileAccessWrite, DirectFilesProfileID)); err == nil {
		t.Fatal("advertising copy+write also made shared writable.\n" +
			"  This is the exact regression the writer decision would have shipped before pairs " +
			"existed: ADR-0017 removed the write-capable profile from Linux so that no accepted " +
			"combination could write, and re-adding it for copy would have re-opened shared")
	}
}
