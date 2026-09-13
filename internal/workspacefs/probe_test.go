package workspacefs

import (
	"context"
	"runtime"
	"testing"

	"github.com/michiTrader/arxi/internal/workspace"
)

// TestProbeDoesNotAdvertiseUnprovenSourceModes pins the production capability
// decision. The internal provisioners can provision shared, copy and worktree
// layouts, and that evidence lives in the returned provisioner versions, but
// Modes is what preflight consults: widening it here would let a real run
// accept a source-backed workspace before any platform decision promised its
// full source, lifecycle and containment contract.
func TestProbeDoesNotAdvertiseUnprovenSourceModes(t *testing.T) {
	probe, err := Probe(context.Background(), ".")
	if err != nil {
		t.Skipf("probe requires a Git checkout: %v", err)
	}
	caps := probe.Capabilities
	if err := workspace.ValidateCapabilities(caps); err != nil {
		t.Fatalf("probed capabilities are internally invalid: %v", err)
	}
	advertised := map[workspace.Mode]bool{}
	for _, mode := range caps.Modes {
		advertised[mode] = true
	}
	for _, mode := range []workspace.Mode{workspace.ModeShared, workspace.ModeCopy, workspace.ModeWorktree} {
		if advertised[mode] {
			t.Errorf("%s is advertised by the %s probe: internal provisioner evidence must not become production availability before the platform decision guarantees the contract", mode, runtime.GOOS)
		}
	}
	if _, err := workspace.Preflight([]workspace.Requirement{{Schema: workspace.SchemaV1, Member: "writer",
		Mode: workspace.ModeWorktree, FileAccess: workspace.FileAccessWrite, RequiresSource: true,
		ProfileID: workspace.DirectFilesProfileID}}, caps); err == nil {
		t.Fatal("preflight accepted a source-backed requirement against probed capabilities: a live run would start with a workspace no platform decision promised")
	}
}
