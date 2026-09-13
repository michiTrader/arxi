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
	return ProbeResult{Source: identity, Capabilities: caps}, nil
}

// Verify proves repository identity and frozen object availability. The operator
// checkout may advance after acceptance; its current HEAD is not run identity.
func Verify(ctx context.Context, frozen workspace.SourceIdentity) (ProbeResult, error) {
	if frozen.Kind != "git" || frozen.CanonicalRoot == "" || frozen.CommonGitDir == "" || frozen.Commit == "" || frozen.Tree == "" {
		return ProbeResult{}, fmt.Errorf("frozen source kind %q is not a complete provisionable Git source", frozen.Kind)
	}
	observed, err := Probe(ctx, frozen.CanonicalRoot)
	if err != nil {
		return ProbeResult{}, err
	}
	if observed.Source.CanonicalRoot != frozen.CanonicalRoot || observed.Source.CommonGitDir != frozen.CommonGitDir {
		return ProbeResult{}, fmt.Errorf("current Git repository identity does not match the frozen effective configuration")
	}
	commit, err := gitOutput(ctx, frozen.CanonicalRoot, "rev-parse", frozen.Commit+"^{commit}")
	if err != nil || strings.TrimSpace(commit) != frozen.Commit {
		return ProbeResult{}, fmt.Errorf("frozen Git commit %s is unavailable: %w", frozen.Commit, err)
	}
	tree, err := gitOutput(ctx, frozen.CanonicalRoot, "rev-parse", frozen.Commit+"^{tree}")
	if err != nil || strings.TrimSpace(tree) != frozen.Tree {
		return ProbeResult{}, fmt.Errorf("frozen Git tree %s does not match commit %s", frozen.Tree, frozen.Commit)
	}
	observed.Source = frozen
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
	body, err := gitCommand(ctx, dir, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(body)))
	}
	return string(body), nil
}

func gitCommand(ctx context.Context, dir string, args ...string) *exec.Cmd {
	gitArgs := append([]string{
		"-c", "core.hooksPath=" + filepath.ToSlash(os.DevNull),
		"-c", "core.attributesFile=" + filepath.ToSlash(os.DevNull),
	}, args...)
	cmd := exec.CommandContext(ctx, "git", gitArgs...)
	cmd.Dir = dir
	cmd.Env = gitEnvironment()
	return cmd
}

func gitEnvironment() []string {
	names := []string{"PATH", "SystemRoot", "WINDIR", "COMSPEC", "PATHEXT", "TMP", "TEMP", "TMPDIR"}
	env := make([]string, 0, len(names)+6)
	for _, name := range names {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return append(env,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=0",
	)
}
