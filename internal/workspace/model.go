// Package workspace defines pure workspace requirements and platform capabilities.
package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

const (
	SchemaV1                   = "arxi.workspace/v1"
	ProfileSchemaV1            = "arxi.workspace-profile/v1"
	CommandSchemaV1            = "arxi.command-spec/v1"
	EnvironmentAllowlistV1     = "arxi.command-environment/allowlist-v1"
	CapabilityVersionInitialV1 = "arxi.workspace-capabilities/initial-v1"
)

type Mode string

const (
	ModeNone     Mode = "none"
	ModeShared   Mode = "shared"
	ModeCopy     Mode = "copy"
	ModeWorktree Mode = "worktree"
)

func ParseMode(value string) (Mode, error) {
	mode := Mode(value)
	if err := mode.Validate(); err != nil {
		return "", err
	}
	return mode, nil
}

func (m Mode) Validate() error {
	switch m {
	case ModeNone, ModeShared, ModeCopy, ModeWorktree:
		return nil
	case "":
		return fmt.Errorf("workspace mode is required")
	default:
		return fmt.Errorf("workspace mode %q is unsupported; supported modes are none, shared, copy, worktree", m)
	}
}

type FileAccess string

const (
	FileAccessNone  FileAccess = "none"
	FileAccessRead  FileAccess = "read"
	FileAccessWrite FileAccess = "write"
)

type SourceIdentity struct {
	Schema            string `json:"schema"`
	Kind              string `json:"kind"`
	CanonicalRoot     string `json:"canonical_root,omitempty"`
	CommonGitDir      string `json:"common_git_dir,omitempty"`
	Commit            string `json:"commit,omitempty"`
	Tree              string `json:"tree,omitempty"`
	DirtyPolicy       string `json:"dirty_policy"`
	UntrackedPolicy   string `json:"untracked_policy"`
	IgnoredPolicy     string `json:"ignored_policy"`
	SubmodulePolicy   string `json:"submodule_policy"`
	SymlinkPolicy     string `json:"symlink_policy"`
	SpecialFilePolicy string `json:"special_file_policy"`
}

type ProcessProfile struct {
	Descendants string `json:"descendants"`
	Filesystem  string `json:"filesystem"`
	Environment string `json:"environment"`
	Network     string `json:"network"`
}

type CommandProfile struct {
	Schema             string `json:"schema"`
	RunnerVersion      string `json:"runner_version"`
	Executable         string `json:"executable"`
	EnvironmentVersion string `json:"environment_version"`
	Descendants        string `json:"descendants"`
	Filesystem         string `json:"filesystem"`
	Network            string `json:"network"`
	OutputLimitBytes   int    `json:"output_limit_bytes"`
}

type Profile struct {
	Schema            string          `json:"schema"`
	ID                string          `json:"id"`
	FileAccess        FileAccess      `json:"file_access"`
	HandleRelative    bool            `json:"handle_relative"`
	FinalLinkRaceFree bool            `json:"final_link_race_free"`
	Process           ProcessProfile  `json:"process"`
	Command           *CommandProfile `json:"command,omitempty"`
}

func (p Profile) Identity() (string, error) {
	if p.Schema != ProfileSchemaV1 || p.ID == "" {
		return "", fmt.Errorf("workspace profile identity requires a versioned profile")
	}
	body, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("encode workspace profile identity: %w", err)
	}
	sum := sha256.Sum256(body)
	return p.ID + ":" + hex.EncodeToString(sum[:]), nil
}

func CurrentCapabilities(platform string) Capabilities {
	capabilities := Capabilities{
		Schema: SchemaV1, CapabilityVersion: CapabilityVersionInitialV1, Platform: platform,
		Modes: []Mode{ModeNone},
		Profiles: []Profile{
			{Schema: ProfileSchemaV1, ID: NoToolsProfileID, FileAccess: FileAccessNone,
				Process: ProcessProfile{Descendants: "unavailable", Filesystem: "unavailable", Environment: "unavailable", Network: "unavailable"}},
		},
		Provisioners: map[Mode]string{ModeNone: "arxi.workspace.none/v1"},
	}
	if platform == "linux" {
		capabilities.Profiles = append(capabilities.Profiles, Profile{Schema: ProfileSchemaV1, ID: DirectFilesProfileID,
			FileAccess: FileAccessWrite, HandleRelative: true, FinalLinkRaceFree: true,
			Process: ProcessProfile{Descendants: "unavailable", Filesystem: "unavailable", Environment: "unavailable", Network: "unavailable"}})
	}
	if platform == "simulation" {
		capabilities.Modes = []Mode{ModeNone, ModeShared, ModeCopy, ModeWorktree}
		capabilities.Provisioners = map[Mode]string{
			ModeNone: "arxi.workspace.none/v1", ModeShared: "arxi.workspace.simulated/v1",
			ModeCopy: "arxi.workspace.simulated/v1", ModeWorktree: "arxi.workspace.simulated/v1",
		}
		capabilities.Profiles = append(capabilities.Profiles, Profile{Schema: ProfileSchemaV1,
			ID: ContainedProcessProfileID, FileAccess: FileAccessWrite, HandleRelative: true, FinalLinkRaceFree: true,
			Process: ProcessProfile{Descendants: "contained", Filesystem: "workspace-only", Environment: "allowlist", Network: "denied"}})
	}
	return capabilities
}

type Requirement struct {
	Schema         string     `json:"schema"`
	Member         string     `json:"member"`
	Mode           Mode       `json:"mode"`
	FileAccess     FileAccess `json:"file_access"`
	RequiresSource bool       `json:"requires_source"`
	RequiresBash   bool       `json:"requires_bash"`
	ProfileID      string     `json:"profile_id"`
}

type Capabilities struct {
	Schema            string          `json:"schema"`
	CapabilityVersion string          `json:"capability_version"`
	Platform          string          `json:"platform"`
	Modes             []Mode          `json:"modes"`
	SourceKinds       []string        `json:"source_kinds"`
	Profiles          []Profile       `json:"profiles"`
	Provisioners      map[Mode]string `json:"provisioners"`
}

type PlatformDecision struct {
	Schema             string          `json:"schema"`
	Member             string          `json:"member"`
	Platform           string          `json:"platform"`
	CapabilityVersion  string          `json:"capability_version"`
	ProfileID          string          `json:"profile_id"`
	ProfileIdentity    string          `json:"profile_identity"`
	ProvisionerVersion string          `json:"provisioner_version"`
	Command            *CommandProfile `json:"command,omitempty"`
}

type Member struct {
	Name   string
	Tools  []string
	Stages []string
}

type Stage struct {
	Name string
	Mode Mode
}

type ResolutionInput struct {
	TopLevel Mode
	Members  []Member
	Stages   []Stage
}
