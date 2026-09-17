package workspace

import "testing"

// The tests in this file record a structural property of Preflight that was
// found while preparing the ADR that would advertise `copy` with the
// write-capable profile on Linux.
//
// Preflight validates a requirement's mode and its profile INDEPENDENTLY: the
// mode must appear in Modes, the profile must appear in Profiles, and the
// profile's access must cover the requirement's access. Nothing checks the
// PAIR. Capabilities has no field expressing "this profile is offered with
// that layout", so an advertisement is a set of layouts and a set of profiles,
// and preflight accepts their cross product.
//
// Today that is harmless, because Linux advertises exactly one source-backed
// layout and exactly one file profile, and a cross product of one by one is
// one. ADR-0017 reads as though `shared` and `direct-files-read` were bound
// together — "Linux advertises shared ONLY with this profile" — but nothing in
// the code binds them. They coincide.
//
// It stops being harmless the moment a second layout or a second profile is
// advertised, which is precisely what the pending writer decision proposes.
// These tests exist so that decision is taken with the property visible
// instead of discovered afterwards.

// TestPreflightDoesNotBindAProfileToALayout states the mechanism plainly.
//
// A member declaring `workspace: shared` with a write tool resolves to the
// write-capable profile over the shared layout. Add that profile to the Linux
// advertisement for any reason and the pair is accepted, even though nothing
// ever decided that shared may be written.
//
// The test asserts the CURRENT behaviour, including the part that is only safe
// by arithmetic. If someone later adds pair validation, this test fails and
// should be deleted — its job is to make the absence visible, not to defend
// it.
func TestPreflightDoesNotBindAProfileToALayout(t *testing.T) {
	// A hypothetical platform advertising two layouts and two profiles. This
	// is not Linux today; it is the shape the writer decision would create.
	capabilities := Capabilities{
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
	}

	// The intent behind such an advertisement would be "copy may be written,
	// shared may only be read". Preflight cannot express that, and the
	// assertion below is what proves it rather than asserting it.
	shared := []Requirement{{
		Schema: SchemaV1, Member: "w", Mode: ModeShared, FileAccess: FileAccessWrite,
		RequiresSource: true, ProfileID: DirectFilesProfileID,
	}}
	if _, err := Preflight(shared, capabilities); err != nil {
		t.Fatalf("writing the SHARED layout was refused: %v\n"+
			"  If this now fails, preflight learned to validate layout/profile pairs. That is a "+
			"real improvement and this test has done its job: delete it and record the new rule",
			err)
	}

	// Said explicitly so the failure above is not read as an endorsement:
	// this combination is accepted because no rule forbids it, not because
	// any platform decided a shared tree may be mutated.
	t.Log("preflight accepts shared+write whenever the write-capable profile is advertised at all: " +
		"modes and profiles are validated independently, so an advertisement is their cross product")
}

// TestTodaysLinuxAdvertisementIsSafeByArithmeticNotByRule is the other half,
// and the reason the gap above has never bitten.
//
// ADR-0017 says Linux advertises shared "only with" the read-only profile.
// That sentence describes an intent no mechanism enforces — what makes it true
// is that only ONE file profile is advertised, so there is no second pairing
// to get wrong.
//
// Pinning the arithmetic makes the dependency explicit: the moment Linux
// advertises a second file profile, this fails, and whoever is widening the
// advertisement is told that ADR-0017's guarantee rested on a count.
func TestTodaysLinuxAdvertisementIsSafeByArithmeticNotByRule(t *testing.T) {
	linux := CurrentCapabilities("linux")

	sourceBacked := 0
	for _, mode := range linux.Modes {
		if mode != ModeNone {
			sourceBacked++
		}
	}
	fileProfiles := 0
	for _, profile := range linux.Profiles {
		if profile.FileAccess != FileAccessNone {
			fileProfiles++
		}
	}

	if sourceBacked != 1 || fileProfiles != 1 {
		t.Fatalf("Linux advertises %d source-backed layout(s) and %d file profile(s).\n"+
			"  ADR-0017's \"shared only with the read-only profile\" is not enforced by any rule: "+
			"preflight validates modes and profiles independently, so an advertisement is their "+
			"cross product and the guarantee held only while that product was 1x1.\n"+
			"  Widening it means deciding which layout/profile PAIRS are offered, and teaching "+
			"preflight to check pairs — otherwise every advertised profile becomes available on "+
			"every advertised layout, including ones nobody chose.\n"+
			"  See TestPreflightDoesNotBindAProfileToALayout for the mechanism",
			sourceBacked, fileProfiles)
	}
}
