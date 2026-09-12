// Package workspacefs probes Git source identity and provisions verified source layouts.
package workspacefs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/michiTrader/arxi/internal/workspace"
)

const (
	capabilityVersion  = "arxi.workspace-capabilities/git-v1"
	provisionerVersion = "arxi.workspace.git-layout/v1"
)

// ProbeResult is the observed source identity and the guarantees available for it.
type ProbeResult struct {
	Source       workspace.SourceIdentity
	Capabilities workspace.Capabilities
}

// Probe verifies that source belongs to a Git repository and freezes its HEAD tree.
func Probe(ctx context.Context, source string) (ProbeResult, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return ProbeResult{}, fmt.Errorf("workspace source requires git in PATH: %w", err)
	}
	if strings.TrimSpace(source) == "" {
		source = "."
	}
	rootText, err := gitOutput(ctx, source, "rev-parse", "--show-toplevel")
	if err != nil {
		return ProbeResult{}, fmt.Errorf("workspace source is not a Git repository: %w", err)
	}
	root, err := canonical(strings.TrimSpace(rootText))
	if err != nil {
		return ProbeResult{}, fmt.Errorf("canonicalize repository root: %w", err)
	}
	commonText, err := gitOutput(ctx, root, "rev-parse", "--git-common-dir")
	if err != nil {
		return ProbeResult{}, fmt.Errorf("resolve Git common directory: %w", err)
	}
	commonPath := strings.TrimSpace(commonText)
	if !filepath.IsAbs(commonPath) {
		commonPath = filepath.Join(root, commonPath)
	}
	common, err := canonical(commonPath)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("canonicalize Git common directory: %w", err)
	}
	commit, err := gitOutput(ctx, root, "rev-parse", "HEAD^{commit}")
	if err != nil {
		return ProbeResult{}, fmt.Errorf("freeze source commit: %w", err)
	}
	tree, err := gitOutput(ctx, root, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return ProbeResult{}, fmt.Errorf("freeze source tree: %w", err)
	}
	identity := workspace.SourceIdentity{
		Schema: workspace.SchemaV1, Kind: "git", CanonicalRoot: root,
		CommonGitDir: common, Commit: strings.TrimSpace(commit), Tree: strings.TrimSpace(tree),
		DirtyPolicy: "tracked-frozen-tree", UntrackedPolicy: "excluded", IgnoredPolicy: "excluded",
		SubmodulePolicy: "refused", SymlinkPolicy: "internal-relative-only", SpecialFilePolicy: "refused",
	}
	caps := workspace.CurrentCapabilities(runtime.GOOS)
	caps.CapabilityVersion = capabilityVersion
	caps.Modes = []workspace.Mode{workspace.ModeNone, workspace.ModeShared, workspace.ModeCopy, workspace.ModeWorktree}
	caps.SourceKinds = []string{"git"}
	caps.Provisioners = map[workspace.Mode]string{
		workspace.ModeNone: "arxi.workspace.none/v1", workspace.ModeShared: provisionerVersion,
		workspace.ModeCopy: provisionerVersion, workspace.ModeWorktree: provisionerVersion,
	}
	return ProbeResult{Source: identity, Capabilities: caps}, nil
}

// Verify re-probes the source and rejects drift from the frozen repository identity.
func Verify(ctx context.Context, frozen workspace.SourceIdentity) (ProbeResult, error) {
	if frozen.Kind != "git" || frozen.CanonicalRoot == "" {
		return ProbeResult{}, fmt.Errorf("frozen source kind %q is not a provisionable Git source", frozen.Kind)
	}
	observed, err := Probe(ctx, frozen.CanonicalRoot)
	if err != nil {
		return ProbeResult{}, err
	}
	if observed.Source != frozen {
		return ProbeResult{}, fmt.Errorf("current Git source identity does not match the frozen effective configuration")
	}
	return observed, nil
}

func canonical(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	body, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(body)))
	}
	return string(body), nil
}
