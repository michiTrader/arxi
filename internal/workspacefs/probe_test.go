package workspacefs

import (
	"context"
	"runtime"
	"testing"

	"github.com/michiTrader/arxi/internal/workspace"
)

// TestProbePinsTheADR0017CapabilityMatrix pins the production capability
// decision. The internal provisioners can materialize shared, copy and worktree
// layouts, and that evidence lives in the returned provisioner versions, but
// Modes is what preflight consults. Before ADR-0017 the pin refused shared on
// every platform because no platform decision promised its contract. Linux now
// announces shared read-only (ADR-0017), so the pin measures the contract each
// platform actually announced: shared must appear on Linux and nowhere else,
// and copy and worktree must not appear anywhere.
func TestProbePinsTheADR0017CapabilityMatrix(t *testing.T) {
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
	// Copy and worktree stay unadvertised on every production platform: their
	// provisioners are internal evidence only, and no platform decision has
	// promised their lifecycle or isolation contract (ADR-0012).
	for _, mode := range []workspace.Mode{workspace.ModeCopy, workspace.ModeWorktree} {
		if advertised[mode] {
			t.Errorf("%s is advertised by the %s probe: internal provisioner evidence must not become production availability before the platform decision guarantees the contract", mode, runtime.GOOS)
		}
	}
	if runtime.GOOS == "linux" && !advertised[workspace.ModeShared] {
		t.Error("the Linux probe does not advertise shared: ADR-0017 promised the shared read-only combination, so a read/grep run would be refused at preflight and the milestone would be blocked")
	}
	if runtime.GOOS != "linux" && advertised[workspace.ModeShared] {
		t.Errorf("shared is advertised on %s: only the Linux platform decision (ADR-0017) has proven the shared read-only contract", runtime.GOOS)
	}
	requirements, err := workspace.Resolve(workspace.ResolutionInput{Members: []workspace.Member{{Name: "reader", Tools: []string{"read", "grep"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "linux" {
		if _, err := workspace.Preflight(requirements, caps); err != nil {
			t.Errorf("preflight refused a read/grep requirement against probed Linux capabilities: %v: the frozen-tree shared read-only contract is exactly what ADR-0017 promised, and refusing it here would block the milestone on the platform it was decided for", err)
		}
	} else if _, err := workspace.Preflight(requirements, caps); err == nil {
		t.Errorf("preflight accepted a read/grep requirement on %s: no platform decision promises a source-backed combination there yet", runtime.GOOS)
	}
	if _, err := workspace.Preflight([]workspace.Requirement{{Schema: workspace.SchemaV1, Member: "writer",
		Mode: workspace.ModeWorktree, FileAccess: workspace.FileAccessWrite, RequiresSource: true,
		ProfileID: workspace.DirectFilesProfileID}}, caps); err == nil {
		t.Fatal("preflight accepted a source-backed write requirement against probed capabilities: a live run would start with a workspace no platform decision promised")
	}
}
