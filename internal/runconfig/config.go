// Package runconfig stores the complete, immutable execution configuration of a run.
package runconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/model"
	"github.com/michiTrader/arxi/internal/surface"
	"github.com/michiTrader/arxi/internal/workspace"
)

const (
	FileName         = "effective-config.v1.json"
	Schema           = "arxi.effective-config/v1"
	SimulationLegacy = 1
	SimulationNative = 2

	DefaultToolSchemaVersion  = "arxi.tools/v1"
	DefaultPolicyVersion      = "arxi.tool-policy/v1"
	DefaultWorkspaceProfileID = "arxi.workspace/legacy-v1"
	DefaultAuthorizationTTLMS = int64(30 * 60 * 1000)
)

type Contracts struct {
	Kernel  int `json:"kernel"`
	Effects int `json:"effects"`
	Surface int `json:"surface"`
}

type Route struct {
	Ref       string      `json:"ref"`
	Provider  string      `json:"provider"`
	Protocol  string      `json:"protocol"`
	Model     string      `json:"model"`
	BaseURL   string      `json:"base_url"`
	APIKeyEnv string      `json:"api_key_env"`
	Price     model.Price `json:"price"`
}

type WorkspaceContract struct {
	Schema       string                       `json:"schema"`
	Source       workspace.SourceIdentity     `json:"source"`
	Requirements []workspace.Requirement      `json:"requirements"`
	Decisions    []workspace.PlatformDecision `json:"platform_decisions"`
}

type Artifact struct {
	Schema              string                               `json:"schema"`
	RunID               string                               `json:"run_id"`
	Mode                string                               `json:"mode"`
	BlueprintSHA        string                               `json:"blueprint_sha"`
	Config              kernel.Config                        `json:"config"`
	Prompt              string                               `json:"prompt"`
	DefaultModel        string                               `json:"default_model,omitempty"`
	Routes              []Route                              `json:"routes,omitempty"`
	ToolPolicy          map[string]map[string]surface.Policy `json:"tool_policy,omitempty"`
	SimVersion          int                                  `json:"sim_version,omitempty"`
	ToolSchemaVersion   string                               `json:"tool_schema_version,omitempty"`
	PolicyVersion       string                               `json:"policy_version,omitempty"`
	WorkspaceProfileID  string                               `json:"workspace_profile_id,omitempty"`
	WorkspaceContract   *WorkspaceContract                   `json:"workspace_contract,omitempty"`
	AuthorizationTTLMS  int64                                `json:"authorization_ttl_ms,omitempty"`
	Contracts           Contracts                            `json:"contracts"`
	legacyAuthorization bool
}

func New(runID, mode, blueprintSHA, prompt, defaultModel string, cfg kernel.Config,
	routes []Route, policy map[string]map[string]surface.Policy) Artifact {
	return Artifact{
		Schema: Schema, RunID: runID, Mode: mode, BlueprintSHA: blueprintSHA,
		Config: cfg, Prompt: prompt, DefaultModel: defaultModel,
		Routes: routes, ToolPolicy: policy, SimVersion: SimulationNative,
		ToolSchemaVersion: DefaultToolSchemaVersion, PolicyVersion: DefaultPolicyVersion,
		WorkspaceProfileID: DefaultWorkspaceProfileID, AuthorizationTTLMS: DefaultAuthorizationTTLMS,
		Contracts: Contracts{Kernel: 1, Effects: 1, Surface: surface.SurfaceVersion},
	}
}

// Encode returns the exact canonical bytes whose digest is recorded by run.started.
func Encode(a Artifact) ([]byte, string, error) {
	if err := Validate(a); err != nil {
		return nil, "", err
	}
	body, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return nil, "", fmt.Errorf("encode effective config: %w", err)
	}
	body = append(body, '\n')
	sum := sha256.Sum256(body)
	return body, hex.EncodeToString(sum[:]), nil
}

// Publish writes the config once. A hard-link publication is atomic and cannot replace an existing artifact.
func Publish(dir string, a Artifact) (string, error) {
	body, digest, err := Encode(a)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create run directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".effective-config-*")
	if err != nil {
		return "", fmt.Errorf("create effective config temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return "", fmt.Errorf("protect effective config: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		return "", fmt.Errorf("write effective config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return "", fmt.Errorf("sync effective config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close effective config: %w", err)
	}
	path := filepath.Join(dir, FileName)
	if err := os.Link(tmpName, path); err != nil {
		if os.IsExist(err) {
			return "", fmt.Errorf("effective config already exists at %s; refusing to replace the execution contract", path)
		}
		return "", fmt.Errorf("publish effective config: %w", err)
	}
	ok = true
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return digest, nil
}

func Load(dir string) (Artifact, string, error) {
	path := filepath.Join(dir, FileName)
	body, err := os.ReadFile(path)
	if err != nil {
		return Artifact{}, "", err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var a Artifact
	if err := dec.Decode(&a); err != nil {
		return Artifact{}, "", fmt.Errorf("decode %s: %w", path, err)
	}
	if err := requireEOF(dec); err != nil {
		return Artifact{}, "", fmt.Errorf("decode %s: %w", path, err)
	}
	if a.ToolSchemaVersion == "" && a.PolicyVersion == "" && a.WorkspaceProfileID == "" && a.AuthorizationTTLMS == 0 {
		a.legacyAuthorization = true
		a.ToolSchemaVersion = DefaultToolSchemaVersion
		a.PolicyVersion = DefaultPolicyVersion
		a.WorkspaceProfileID = DefaultWorkspaceProfileID
		a.AuthorizationTTLMS = DefaultAuthorizationTTLMS
	}
	if err := Validate(a); err != nil {
		return Artifact{}, "", fmt.Errorf("validate %s: %w", path, err)
	}
	sum := sha256.Sum256(body)
	return a, hex.EncodeToString(sum[:]), nil
}

// VerifyBinding proves that the immutable execution artifacts still match the
// confirmed run.started contract before any executor is constructed.
func VerifyBinding(dir, runID string, events []kernel.Event) (Artifact, error) {
	a, digest, err := Load(dir)
	if err != nil {
		return Artifact{}, err
	}
	if a.RunID != runID {
		return Artifact{}, fmt.Errorf("effective config belongs to run %q, not %q", a.RunID, runID)
	}
	var started *kernel.Event
	for i := range events {
		if events[i].Type == kernel.RunStarted {
			started = &events[i]
			break
		}
	}
	if started == nil {
		return Artifact{}, fmt.Errorf("run %s has no run.started event", runID)
	}
	if got := started.Str("effective_config_schema"); got != Schema {
		return Artifact{}, fmt.Errorf("run.started effective_config_schema %q disagrees with %q", got, Schema)
	}
	if got := started.Str("effective_config_path"); got != FileName {
		return Artifact{}, fmt.Errorf("run.started effective_config_path %q disagrees with %q", got, FileName)
	}
	if got := started.Str("effective_config_sha"); got != digest {
		return Artifact{}, fmt.Errorf("effective config digest %s disagrees with run.started %s", digest, got)
	}
	if got := started.Str("blueprint_sha"); got != a.BlueprintSHA {
		return Artifact{}, fmt.Errorf("effective config blueprint_sha %s disagrees with run.started %s", a.BlueprintSHA, got)
	}
	snapshot, err := os.ReadFile(filepath.Join(dir, "blueprint.snapshot.yaml"))
	if err != nil {
		return Artifact{}, fmt.Errorf("read the frozen blueprint: %w", err)
	}
	sum := sha256.Sum256(snapshot)
	if got := hex.EncodeToString(sum[:]); got != a.BlueprintSHA {
		return Artifact{}, fmt.Errorf("frozen blueprint digest %s disagrees with effective config %s", got, a.BlueprintSHA)
	}
	return a, nil
}

// SupportsExactAuthorization reports whether these exact bindings were present
// in the persisted artifact. Legacy artifacts decode with defaults for replay,
// but those defaults cannot retroactively authorize an old unanswered action.
func (a Artifact) SupportsExactAuthorization() bool { return !a.legacyAuthorization }

func (a Artifact) SupportsWorkspaceContract() bool { return a.WorkspaceContract != nil }

func requireEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("more than one JSON value")
		}
		return err
	}
	return nil
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func Validate(a Artifact) error {
	if a.Schema != Schema {
		return fmt.Errorf("unsupported schema %q (want %q)", a.Schema, Schema)
	}
	if strings.TrimSpace(a.RunID) == "" {
		return fmt.Errorf("run_id is required")
	}
	if a.Mode != "live" && a.Mode != "sim" {
		return fmt.Errorf("mode %q is not live or sim", a.Mode)
	}
	if a.Mode == "sim" && a.SimVersion != SimulationLegacy && a.SimVersion != SimulationNative {
		return fmt.Errorf("unsupported simulation version %d", a.SimVersion)
	}
	if len(a.BlueprintSHA) != sha256.Size*2 {
		return fmt.Errorf("blueprint_sha is not a SHA-256 digest")
	}
	if _, err := hex.DecodeString(a.BlueprintSHA); err != nil {
		return fmt.Errorf("blueprint_sha is not a SHA-256 digest")
	}
	if a.Contracts.Kernel != 1 || a.Contracts.Effects != 1 || a.Contracts.Surface != surface.SurfaceVersion {
		return fmt.Errorf("unsupported contracts kernel=%d effects=%d surface=%d",
			a.Contracts.Kernel, a.Contracts.Effects, a.Contracts.Surface)
	}
	if a.ToolSchemaVersion == "" || a.PolicyVersion == "" || a.WorkspaceProfileID == "" || a.AuthorizationTTLMS <= 0 {
		return fmt.Errorf("authorization defaults require tool schema, policy, workspace profile, and positive ttl")
	}
	if a.WorkspaceContract != nil {
		if a.WorkspaceContract.Schema != workspace.SchemaV1 {
			return fmt.Errorf("unsupported workspace contract schema %q", a.WorkspaceContract.Schema)
		}
		if a.WorkspaceContract.Source.Schema != workspace.SchemaV1 || a.WorkspaceContract.Source.Kind == "" {
			return fmt.Errorf("workspace contract requires a versioned source identity")
		}
		if len(a.WorkspaceContract.Requirements) == 0 || len(a.WorkspaceContract.Decisions) != len(a.WorkspaceContract.Requirements) {
			return fmt.Errorf("workspace contract requires one platform decision per member requirement")
		}
		for i, requirement := range a.WorkspaceContract.Requirements {
			if err := workspace.ValidateRequirement(requirement); err != nil {
				return err
			}
			if decision := a.WorkspaceContract.Decisions[i]; decision.Schema != workspace.SchemaV1 || decision.Member != requirement.Member || decision.ProfileID != requirement.ProfileID || decision.Platform == "" || decision.CapabilityVersion == "" || decision.ProvisionerVersion == "" {
				return fmt.Errorf("workspace platform decision for member %q does not bind its frozen requirement", requirement.Member)
			}
		}
	}
	seen := map[string]bool{}
	for _, r := range a.Routes {
		if r.Ref == "" || seen[r.Ref] {
			return fmt.Errorf("route ref %q is empty or duplicated", r.Ref)
		}
		seen[r.Ref] = true
		if r.Protocol != model.ProtocolOpenAIChatCompletions && r.Protocol != model.ProtocolAnthropicMessages {
			return fmt.Errorf("route %q uses unsupported protocol %q", r.Ref, r.Protocol)
		}
		u, err := url.Parse(r.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("route %q has invalid base_url", r.Ref)
		}
		if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("route %q base_url may not contain userinfo, query, or fragment", r.Ref)
		}
		if r.APIKeyEnv != "" && !envName.MatchString(r.APIKeyEnv) {
			return fmt.Errorf("route %q api_key_env is not an environment-variable name", r.Ref)
		}
	}
	if a.Mode == "live" && len(a.Routes) == 0 {
		return fmt.Errorf("live config has no resolved routes")
	}
	return nil
}
