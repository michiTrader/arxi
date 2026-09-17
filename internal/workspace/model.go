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

func MixedProfileIdentity(decisions []PlatformDecision) string {
	body, err := json.Marshal(decisions)
	if err != nil {
		panic("workspace platform decisions contain only JSON values: " + err.Error())
	}
	sum := sha256.Sum256(body)
	return "arxi.workspace/mixed-v1:" + hex.EncodeToString(sum[:])
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
		// ADR-0017 advertises shared with the read-only profile; ADR-0019
		// adds copy with the write-capable one. The pairing below is what
		// keeps those two facts from combining into a third nobody decided:
		// shared+write is not offered, so the read-only-ness of the
		// operator's tree is a rule rather than a side effect of the write
		// profile being absent, which is what it was before Pairs existed.
		//
		// What stays unadvertised is the load-bearing half:
		//
		//   - worktree, whose root carries a gitdir: pointer into the
		//     operator's repository (ADR-0018). A file-only writer resolves
		//     to copy, so nothing needs it;
		//   - contained-process, because every process guarantee is still
		//     "unavailable": descendants, filesystem, environment, network.
		//     A bash member resolves to worktree AND that profile, so it is
		//     refused twice over.
		//
		// A member with write or edit is accepted; a member with bash is not.
		// That asymmetry is the decision, not an oversight: the file
		// guarantees are enforced and audited, the process ones are not.
		capabilities.Modes = append(capabilities.Modes, ModeShared, ModeCopy)
		capabilities.Provisioners[ModeShared] = GitLayoutProvisionerV1
		capabilities.Provisioners[ModeCopy] = GitLayoutProvisionerV1
		capabilities.Profiles = append(capabilities.Profiles,
			Profile{Schema: ProfileSchemaV1, ID: DirectFilesReadProfileID,
				FileAccess: FileAccessRead, HandleRelative: true, FinalLinkRaceFree: true,
				Process: ProcessProfile{Descendants: "unavailable", Filesystem: "unavailable", Environment: "unavailable", Network: "unavailable"}},
			Profile{Schema: ProfileSchemaV1, ID: DirectFilesProfileID,
				FileAccess: FileAccessWrite, HandleRelative: true, FinalLinkRaceFree: true,
				Process: ProcessProfile{Descendants: "unavailable", Filesystem: "unavailable", Environment: "unavailable", Network: "unavailable"}})
		// copy carries the read-only profile too, so a reader that declares
		// `workspace: copy` is accepted rather than refused for asking for
		// less than the layout offers.
		capabilities.Pairs = map[Mode][]string{
			ModeNone:   {NoToolsProfileID},
			ModeShared: {DirectFilesReadProfileID},
			ModeCopy:   {DirectFilesProfileID, DirectFilesReadProfileID},
		}
	}
	if platform == "simulation" {
		capabilities.Modes = []Mode{ModeNone, ModeShared, ModeCopy, ModeWorktree}
		capabilities.Provisioners = map[Mode]string{
			ModeNone: "arxi.workspace.none/v1", ModeShared: "arxi.workspace.simulated/v1",
			ModeCopy: "arxi.workspace.simulated/v1", ModeWorktree: "arxi.workspace.simulated/v1",
		}
		// Resolution is platform-neutral, so readers resolve to the read-only
		// profile everywhere; simulation must therefore carry it too, or every
		// simulated read/grep member would fail a preflight that production
		// Linux passes.
		capabilities.Profiles = append(capabilities.Profiles,
			Profile{Schema: ProfileSchemaV1, ID: DirectFilesReadProfileID, FileAccess: FileAccessRead,
				HandleRelative: true, FinalLinkRaceFree: true,
				Process: ProcessProfile{Descendants: "unavailable", Filesystem: "unavailable", Environment: "unavailable", Network: "unavailable"}},
			Profile{Schema: ProfileSchemaV1, ID: DirectFilesProfileID, FileAccess: FileAccessWrite,
				HandleRelative: true, FinalLinkRaceFree: true,
				Process: ProcessProfile{Descendants: "unavailable", Filesystem: "unavailable", Environment: "unavailable", Network: "unavailable"}},
			Profile{Schema: ProfileSchemaV1, ID: ContainedProcessProfileID, FileAccess: FileAccessWrite,
				HandleRelative: true, FinalLinkRaceFree: true,
				Process: ProcessProfile{Descendants: "contained", Filesystem: "workspace-only", Environment: "allowlist", Network: "denied"},
				Command: &CommandProfile{Schema: CommandSchemaV1, RunnerVersion: "arxi.command.simulated/v1", Executable: "simulated",
					EnvironmentVersion: EnvironmentAllowlistV1, Descendants: "contained", Filesystem: "workspace-only", Network: "denied", OutputLimitBytes: 256 << 10}})
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

	// Pairs names which profiles are offered with which layout. It exists
	// because Modes and Profiles are two independent lists, and without a
	// third statement relating them an advertisement means their CROSS
	// PRODUCT: every advertised profile usable on every advertised layout.
	//
	// That is not what ADR-0017 says. "Linux advertises shared only with the
	// read-only profile" was true only because Linux advertised one layout
	// and one profile, so the product was 1x1 and there was no second pairing
	// to get wrong. The guarantee rested on a count.
	//
	// Nil means the cross product, deliberately. Every existing advertisement
	// -- including the ones hosts declare through the public
	// arxi.host.workspace-capabilities/v1 type -- keeps the exact behaviour it
	// had, so this field adds an expressible restriction without silently
	// changing what anyone already deployed. A non-nil map is a promise that
	// the listed pairs are the only ones offered.
	Pairs map[Mode][]string `json:"pairs,omitempty"`
}

// offers reports whether profileID may be used with mode.
//
// The nil case is the compatibility hinge: an advertisement that never
// mentions pairs behaves exactly as it did before pairs existed. An
// advertisement that does mention them is taken at its word, including when
// it omits a mode entirely -- an explicit Pairs map that says nothing about a
// layout is saying that layout carries no file profile, not that it carries
// all of them. Treating a missing key as "anything goes" would make the
// restriction unstatable for the one case most worth stating.
func (c Capabilities) offers(mode Mode, profileID string) bool {
	if c.Pairs == nil {
		return true
	}
	for _, offered := range c.Pairs[mode] {
		if offered == profileID {
			return true
		}
	}
	return false
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
