// Package workspace defines pure workspace requirements and platform capabilities.
package workspace

import "fmt"

const (
	SchemaV1        = "arxi.workspace/v1"
	ProfileSchemaV1 = "arxi.workspace-profile/v1"
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

type Profile struct {
	Schema            string         `json:"schema"`
	ID                string         `json:"id"`
	FileAccess        FileAccess     `json:"file_access"`
	HandleRelative    bool           `json:"handle_relative"`
	FinalLinkRaceFree bool           `json:"final_link_race_free"`
	Process           ProcessProfile `json:"process"`
}

func CurrentCapabilities(platform string) Capabilities {
	return Capabilities{
		Schema: SchemaV1, CapabilityVersion: "arxi.workspace-capabilities/initial-v1", Platform: platform,
		Modes: []Mode{ModeNone},
		Profiles: []Profile{
			{Schema: ProfileSchemaV1, ID: NoToolsProfileID, FileAccess: FileAccessNone,
				Process: ProcessProfile{Descendants: "unavailable", Filesystem: "unavailable", Environment: "unavailable", Network: "unavailable"}},
			{Schema: ProfileSchemaV1, ID: DirectFilesProfileID, FileAccess: FileAccessWrite,
				HandleRelative: true, FinalLinkRaceFree: platform != "windows",
				Process: ProcessProfile{Descendants: "unavailable", Filesystem: "unavailable", Environment: "unavailable", Network: "unavailable"}},
		},
		Provisioners: map[Mode]string{ModeNone: "arxi.workspace.none/v1"},
	}
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
	Schema             string `json:"schema"`
	Member             string `json:"member"`
	Platform           string `json:"platform"`
	CapabilityVersion  string `json:"capability_version"`
	ProfileID          string `json:"profile_id"`
	ProvisionerVersion string `json:"provisioner_version"`
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
