# ADR-0012: Workspace names describe provisioned, platform-verified guarantees

- Status: accepted
- Affects: `internal/runconfig`, `internal/toolrun`, `internal/supervisor`, `cmd/arxi`, `host/v1`, `spec/workspaces.md`

## Context

Arxi accepts `none`, `shared`, `copy` and `worktree`, but the current runner
reduces them to one Boolean. `shared` uses one empty directory; every other name
uses one empty directory per member. No mode supplies project source, `copy`
copies nothing and `worktree` creates no Git worktree.

The direct file tools reject ordinary traversal and symlink escapes, while
`bash` is a program with the authority of the Arxi process. It can use absolute
paths, inherits provider credentials and network access, and has different
process-descendant behavior on Unix and Windows. A directory name cannot turn
those facts into isolation.

## Decision

**A workspace mode names source layout. Separately versioned execution profiles
name containment guarantees. Arxi advertises and accepts only combinations that
a platform adapter proves before the run starts.**

### Source identity

Every workspace-backed run freezes:

- canonical repository root and source kind;
- selected commit and tree identity;
- dirty, untracked, ignored, submodule, symlink and special-file policy;
- workspace mode and provisioner version;
- execution-profile and policy versions;
- platform capability decision.

Resume verifies the provisioned workspace against those fields. A directory at
the expected path is not evidence that it belongs to the job or still contains
the frozen source.

### Workspace modes

`none` provides no filesystem workspace. File and process tools are unavailable.
It is valid for text-only members and cannot silently become a private directory.

`shared` provides one verified source tree to all members. Writes are deliberately
visible across members. It makes no cross-member isolation claim.

`copy` provides one deterministic source snapshot per writing member. A snapshot
contains the frozen tracked tree. Dirty, untracked and ignored content is excluded
unless a later versioned policy explicitly includes it. Symlinks are reproduced
only when their recorded target remains relative and resolves within the snapshot;
special files are refused.

`worktree` provides one detached Git worktree per writing member at the frozen
commit. The provisioner verifies Git common-directory identity, registered
worktree metadata, HEAD, job and member ownership. A Git worktree separates
working trees; it does not sandbox a process.

Read-only members use the run's verified source view unless a stage requires a
stronger mode. Stage declarations resolve before acceptance to one effective
mode for each member. Arxi does not replace a member's filesystem between stages,
because doing so would require a separate durable transfer contract.

### Execution profiles

A workspace handle controls built-in file operations. Those operations traverse
relative to an opened root, reject symlink or reparse-point traversal at every
component, and never accept an absolute path. A platform that cannot provide the
required handle-relative checks does not advertise the profile.

A process profile separately declares:

- descendant ownership and termination;
- filesystem reach;
- inherited environment;
- network reach.

Child environments are constructed from an allowlist. Provider credentials and
undeclared variables are absent by default. Network and filesystem denial are
claimed only when the platform adapter can enforce them for the whole process
tree. Git layout alone never satisfies either claim.

The built-in runner refuses `bash` when the selected profile requires a guarantee
that its platform adapter cannot enforce. It does not fall back to an inherited,
unrestricted process. An adapter may advertise a deliberately unrestricted
operator profile only when that profile is explicitly selected; it is not an
isolation mode and cannot satisfy an isolation requirement.

### Capability preflight

Capability probing and provisioning perform I/O outside the kernel. A pure
comparison checks the requested requirement against the probe result. Failure
occurs before `run.started`, before provider dispatch and before a workspace is
reported as provisioned.

Git executable availability, repository identity, operating-system primitives
and required privileges are runtime capabilities, not Go dependencies. The
standard-library-only build policy remains unchanged.

### Durable lifecycle

Provisioning uses stable job/member identities and records prepared, started and
finished boundaries. Recovery verifies existing external state before adopting
it. It never assumes that a failed local command means Git or the filesystem was
unchanged.

Release is idempotent and ownership-checked. Successful runs may release managed
workspaces according to retention policy. Failed, cancelled or unknown work keeps
diagnostic evidence unless an explicit operator action removes it. One job can
never release another job's workspace.

The public `host/v1.Workspace` remains opaque. A configured provisioner returns
the handle supplied to `ToolInvocation`; Arxi does not turn it into a public path
or invent stronger guarantees than the host declared.

## Discarded alternatives

**Keep interpreting every non-shared mode as a member directory.** Rejected: the
names promise source and isolation behavior that the implementation does not
provide.

**Treat a Git worktree as a sandbox.** Rejected: a process in a worktree can read
absolute paths, inherit secrets, use the network and outlive its parent.

**Implement one lowest-common-denominator fallback.** Rejected: silently weaker
Windows behavior under the same profile makes the profile a label rather than a
contract.

**Copy the caller's live directory.** Rejected: ignored secrets, build artifacts,
pipes and a changing source tree would enter the run without durable identity.

**Probe after acceptance.** Rejected: an accepted job that can never obtain its
promised workspace violates the durable acceptance boundary and may already have
spent a provider call.

**Delete every workspace on exit.** Rejected: failed and unknown outcomes need
their filesystem evidence, and an ownership bug would make cleanup destructive.

## Consequences

- Existing mode names become honest and may fail where they previously produced
an empty directory.
- `worktree` requires Git at runtime; `copy` remains available without adding a Go
dependency.
- Strong process isolation is platform-specific. Unsupported combinations are
visible errors, not weaker execution.
- Frozen source and profile identity increase effective-configuration and
provisioning records.
- Direct file safety, process containment, environment filtering and network
containment are tested and reported independently.

## How it is verified

Contract tests run every advertised mode against source identity, member
visibility, restart, corruption and ownership cases. Worktree tests inspect Git
metadata and the frozen commit; copy tests exclude dirty, ignored and special
content. Filesystem tests cover traversal, symlinks, junctions, reparse points,
parent swaps and cross-member access. Process tests use sentinel secrets, a local
network listener and descendants that detach or keep output handles open. Each
supported operating system runs its negative suite. A platform missing a required
primitive must pass a preflight-refusal test before it may pass no execution test.
Host tests require the exact opaque provisioner handle to reach the tool executor.
