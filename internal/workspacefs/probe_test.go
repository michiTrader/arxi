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
	// worktree stays unadvertised everywhere. Its provisioner works, so the
	// refusal is not about missing machinery: the root carries a gitdir:
	// pointer into the operator's repository (ADR-0018), and a layout that
	// promises the tracked tree must not deliver the control plane as well.
	if advertised[workspace.ModeWorktree] {
		t.Errorf("worktree is advertised by the %s probe: no platform decision has promised a "+
			"layout whose root exposes a pointer into the operator's repository", runtime.GOOS)
	}
	// copy is advertised on Linux only, and only since ADR-0019. Its file
	// guarantees were audited rather than assumed before that decision: no
	// control plane in the root, no inode shared with the source, writes
	// confined to the snapshot and discarded on release.
	if advertised[workspace.ModeCopy] != (runtime.GOOS == "linux") {
		t.Errorf("copy advertised = %v on %s: ADR-0019 promised the copy snapshot on Linux and "+
			"nowhere else, so advertising it elsewhere claims an audit that platform never had, "+
			"and withholding it on Linux means the decision is not in effect",
			advertised[workspace.ModeCopy], runtime.GOOS)
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
		t.Fatal("preflight accepted a worktree write requirement against probed capabilities: a live run would start with a workspace no platform decision promised")
	}

	// The two boundaries ADR-0019 did NOT move, checked against the PROBED
	// capabilities rather than CurrentCapabilities. Probe rewrites
	// Provisioners, so it is a second construction of the advertisement and
	// can drift from the one the preflight tests reason about.
	if _, err := workspace.Preflight([]workspace.Requirement{{Schema: workspace.SchemaV1, Member: "writer",
		Mode: workspace.ModeShared, FileAccess: workspace.FileAccessWrite, RequiresSource: true,
		ProfileID: workspace.DirectFilesProfileID}}, caps); err == nil {
		t.Fatal("the probed capabilities accept writing the SHARED layout: that is the operator's " +
			"own frozen tree, kept read-only by ADR-0017 and left read-only by ADR-0019, which " +
			"advertises the write profile for the copy snapshot only")
	}
	if _, err := workspace.Preflight([]workspace.Requirement{{Schema: workspace.SchemaV1, Member: "runner",
		Mode: workspace.ModeCopy, FileAccess: workspace.FileAccessWrite, RequiresSource: true,
		RequiresBash: true, ProfileID: workspace.ContainedProcessProfileID}}, caps); err == nil {
		t.Fatal("the probed capabilities accept a contained-process requirement: descendants, " +
			"filesystem, environment and network are all still \"unavailable\", so nothing has " +
			"proven what such a process would be contained by")
	}

	// And the combination the decision exists to offer, on the platform it
	// was decided for.
	writer, err := workspace.Resolve(workspace.ResolutionInput{Members: []workspace.Member{
		{Name: "writer", Tools: []string{"read", "write", "edit"}}}})
	if err != nil {
		t.Fatal(err)
	}
	_, writerErr := workspace.Preflight(writer, caps)
	if runtime.GOOS == "linux" && writerErr != nil {
		t.Errorf("the probed Linux capabilities refuse a file-only writer: %v\n"+
			"  ADR-0019 advertises copy with the write-capable profile precisely so this is "+
			"accepted", writerErr)
	}
	if runtime.GOOS != "linux" && writerErr == nil {
		t.Errorf("a file-only writer was accepted on %s: ADR-0019 audited the copy snapshot on "+
			"Linux only", runtime.GOOS)
	}
}
