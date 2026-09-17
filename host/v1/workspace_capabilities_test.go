package v1

import (
	"testing"

	"github.com/michiTrader/arxi/internal/workspace"
)

// #63 recorded that preflight validates a requirement's mode and its profile
// independently and never their pair, so an advertisement is the cross product
// of its layouts and its profiles. It framed that as latent: today's native
// Linux advertisement is one source-backed layout by one file profile, and a
// 1x1 product is 1.
//
// That framing was incomplete. WorkspaceCapabilitiesV1 is a PUBLIC type on the
// host surface, and Options.WorkspaceCapabilities lets an embedder declare its
// own. The native advertisement is not the only one that reaches preflight, so
// the cross product is reachable today by any host that declares two layouts
// and two profiles -- no change to internal/workspace required.
//
// These tests pin that at the boundary where it is actually reachable. They
// assert current behaviour, including the part nobody chose, because an
// embedder deciding what to declare needs the real rule rather than the one
// the field names imply.

// twoByTwoDeclaration is the shape an embedder writes when it means "copy may
// be written, shared may only be read". That intent cannot be expressed: the
// declaration has a list of modes and a list of profiles and no way to say
// which go together.
func twoByTwoDeclaration() *WorkspaceCapabilitiesV1 {
	return &WorkspaceCapabilitiesV1{
		Schema:            WorkspaceCapabilitiesSchemaV1,
		CapabilityVersion: "embedder-caps/v1",
		Platform:          "linux",
		Modes:             []string{"none", "shared", "copy"},
		SourceKinds:       []string{"git"},
		Provisioners: map[string]string{
			"none": "arxi.workspace.none/v1", "shared": "embedder.layout/v1", "copy": "embedder.layout/v1",
		},
		Profiles: []WorkspaceProfileV1{
			{Schema: WorkspaceProfileSchemaV1, ID: "embedder/no-tools", FileAccess: "none",
				Process: WorkspaceProcessProfileV1{Descendants: "unavailable", Filesystem: "unavailable",
					Environment: "unavailable", Network: "unavailable"}},
			{Schema: WorkspaceProfileSchemaV1, ID: "embedder/read", FileAccess: "read",
				HandleRelative: true, FinalLinkRaceFree: true,
				Process: WorkspaceProcessProfileV1{Descendants: "unavailable", Filesystem: "unavailable",
					Environment: "unavailable", Network: "unavailable"}},
			{Schema: WorkspaceProfileSchemaV1, ID: "embedder/write", FileAccess: "write",
				HandleRelative: true, FinalLinkRaceFree: true,
				Process: WorkspaceProcessProfileV1{Descendants: "unavailable", Filesystem: "unavailable",
					Environment: "unavailable", Network: "unavailable"}},
		},
	}
}

// TestAHostDeclaredAdvertisementIsAcceptedAsItsCrossProduct is the reachable
// half of the gap #63 described.
//
// The declaration above is written to mean "writes land in copy". Preflight
// accepts writing SHARED under it, because the write profile is advertised and
// the shared layout is advertised and nothing relates the two.
//
// For a host whose shared layout is the operator's own checkout, that is the
// difference between a snapshot being modified and the operator's tree being
// modified -- decided by a rule the embedder never wrote.
func TestAHostDeclaredAdvertisementIsAcceptedAsItsCrossProduct(t *testing.T) {
	capabilities, err := hostWorkspaceCapabilities(twoByTwoDeclaration())
	if err != nil {
		t.Fatalf("convert host declaration: %v", err)
	}

	writeOverShared := []workspace.Requirement{{
		Schema: workspace.SchemaV1, Member: "w", Mode: workspace.ModeShared,
		FileAccess: workspace.FileAccessWrite, RequiresSource: true, ProfileID: "embedder/write",
	}}

	if _, err := workspace.Preflight(writeOverShared, capabilities); err != nil {
		t.Fatalf("writing the shared layout was refused: %v\n"+
			"  If this now fails, layout/profile pairing became expressible and enforced. That is "+
			"the fix this test exists to provoke: delete it and record the new rule", err)
	}

	t.Log("a host declaring {shared, copy} x {read, write} has also declared shared+write, " +
		"whether or not it meant to: modes and profiles are separate lists and preflight checks " +
		"them separately")
}

// TestTheHostDeclarationCannotExpressAPairing pins the cause rather than the
// symptom, so the gap cannot be read as a preflight bug alone.
//
// Even a perfect preflight could not enforce a pairing the wire type has no
// field for. Any fix has to change WorkspaceCapabilitiesV1 -- which is public
// and versioned, so it is a compatibility decision, not a local one. That is
// worth knowing before someone attempts the narrow fix.
func TestTheHostDeclarationCannotExpressAPairing(t *testing.T) {
	declaration := twoByTwoDeclaration()

	// Provisioners is keyed by mode, so it can say "this layout exists" but
	// nothing about which profiles accompany it. If a pairing field is ever
	// added, this test should fail and be replaced by one asserting the
	// pairing is honoured.
	if len(declaration.Provisioners) != len(declaration.Modes) {
		t.Fatalf("Provisioners no longer maps exactly one version per mode (%d vs %d modes): "+
			"the declaration's shape changed and this test's claim about it must be re-derived",
			len(declaration.Provisioners), len(declaration.Modes))
	}

	converted, err := hostWorkspaceCapabilities(declaration)
	if err != nil {
		t.Fatalf("convert host declaration: %v", err)
	}
	// The converted form is likewise two independent lists.
	if len(converted.Modes) != 3 || len(converted.Profiles) != 3 {
		t.Fatalf("expected 3 modes and 3 profiles, got %d and %d", len(converted.Modes), len(converted.Profiles))
	}

	// Every advertised profile is usable with every advertised source-backed
	// layout. Asserted by exhaustion rather than by claim.
	for _, mode := range []workspace.Mode{workspace.ModeShared, workspace.ModeCopy} {
		for _, profile := range converted.Profiles {
			if profile.FileAccess == workspace.FileAccessNone {
				continue
			}
			requirement := []workspace.Requirement{{
				Schema: workspace.SchemaV1, Member: "w", Mode: mode,
				FileAccess: profile.FileAccess, RequiresSource: true, ProfileID: profile.ID,
			}}
			if _, err := workspace.Preflight(requirement, converted); err != nil {
				t.Errorf("%s + %s was refused (%v): the cross product is no longer complete, so the "+
					"rule this test documents has changed and should be re-derived", mode, profile.ID, err)
			}
		}
	}
}
