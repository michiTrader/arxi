# Workspace guarantees

This specification defines the source-layout and execution-profile guarantees
that may be frozen into an accepted run. It implements ADR-0012. Availability
is a platform capability decision: implementation code or tests for a
provisioner do not by themselves make a mode available in production.

## Vocabulary

The workspace contract schema is `arxi.workspace/v1`.

A **requirement** is the pure result of resolving one member's tools, stages and
workspace declarations. A **capability decision** compares that requirement with
a platform probe before acceptance. The accepted run freezes both values in its
effective configuration.

Workspace modes describe source layout:

- `none` provides no filesystem workspace;
- `shared` provides one verified source view to all members;
- `copy` provides a deterministic source snapshot per writing member;
- `worktree` provides a detached Git worktree per writing member.

Execution profiles separately describe authority:

- `arxi.workspace/no-tools-v1` provides neither file nor process tools;
- `arxi.workspace/direct-files-read-v1` permits built-in relative file reads
  only: mutating tools are refused at the session boundary, not by the absence
  of a grant (ADR-0017);
- `arxi.workspace/direct-files-v1` permits only built-in relative file access;
- `arxi.workspace/contained-process-v1` additionally requires declared process
  containment.

A profile identity is the digest of its complete versioned guarantee, not its
display name. Authorization and resume bind that identity.

## Pure requirement resolution

Resolution performs no probing or provisioning. Text-only members resolve to
`none` and `no-tools`. `read` and `grep` require a shared source view and
read-only direct files. `write` and `edit` require a per-writer source view and
write-capable direct files, and when no mode is declared they resolve to `copy`:
both per-writer layouts isolate one tree per member, and `copy` is the one whose
root holds tracked files and nothing else, while a `worktree` root also holds a
`gitdir:` pointer (ADR-0018). A member without `bash` cannot invoke Git, so the
repository a worktree serves buys it nothing. `bash` requires a per-writer
source view and the contained-process profile, and resolves to `worktree`,
because a contained command is expected to be able to run Git.

Stage declarations are combined before acceptance. An explicit `none` conflicts
with source or tool requirements. `shared` may yield to an explicitly stronger
layout, but `copy` and `worktree` are different contracts rather than ordered
strengths and therefore conflict. Resolution returns one stable requirement per
member in member order.

## Native availability

Production capability probes currently advertise only guarantees that the
complete native path can enforce:

| platform | modes | profiles | accepted source-backed combination |
|---|---|---|---|
| Windows | `none` | `no-tools` | none |
| Linux | `none`, `shared` | `no-tools`, `direct-files-read` | `shared` + `direct-files-read` (read-only) |

Linux direct-file operations can provide handle-relative, final-link-race-free
access. Since ADR-0017 Linux advertises `shared` paired exclusively with the
read-only `direct-files-read` profile: a read/grep requirement forms the one
accepted source-backed combination, file-only write requirements resolve to
`copy` and `bash` to `worktree` — both unadvertised — and no accepted
combination on Linux can write. The write-capable `direct-files` profile stays
off the Linux advertisement; it remains available to writers and simulation.
Neither platform advertises `contained-process`. Simulation may exercise every
mode and profile, but that is simulation behavior and not a production
isolation claim.

An advertisement is a set of layouts and a set of profiles, and acceptance
checks a requirement's layout and its profile independently. It does not check
the pair: no capability field expresses "this profile is offered with that
layout". "Paired exclusively" above is therefore true by arithmetic rather
than by rule — Linux advertises one source-backed layout and one file profile,
so the only pair available is the intended one. Advertising a second layout or
a second profile makes every combination of them acceptable, so any widening
must either accept the full cross product deliberately or introduce pair
validation first.

Internal `shared`, `copy` and `worktree` provisioners remain evidence toward the
contract. A production probe must not add them until one platform decision can
prove source identity, lifecycle, file access and any requested process
containment together.

## Mode guarantees

`none` has no root. File and process tools are unavailable, and the runtime must
not silently create an empty directory and call it isolation.

If advertised, `shared` is one verified frozen source tree. Where a platform
advertises it read-only (Linux, ADR-0017), the session refuses mutating tools
and writes never land; where a platform advertises a writable shared view, the
specification makes no cross-member isolation claim beyond one shared view.

If advertised, `copy` is one deterministic tracked-tree snapshot per writing
member. Dirty, untracked and ignored content is excluded. Submodules and special
files are refused. A symlink is reproduced only when its recorded relative
target remains inside the snapshot. Every entry is materialized by writing the
tracked blob, so no snapshot path shares an inode with the source tree or with
anything outside the workspace. That property is load-bearing rather than
incidental: the confined file API restricts paths, and a hardlink is a second
name for one inode rather than a path pointing elsewhere, so it cannot be
refused by path confinement. A snapshot that linked or reflinked its entries
would let a writing member mutate the operator's tree through a path nothing
has reason to refuse.

If advertised, `worktree` is one registered detached Git worktree per writing
member at the frozen commit. Recovery verifies the common Git directory,
registration, HEAD and ownership. A worktree separates working trees; it is not
a process sandbox.

A `worktree` root also contains a `.git` file — a `gitdir:` pointer into the
common repository — inside the tool-visible tree. Since ADR-0018 the repository
control plane is not workspace content: `.git` is a reserved path in every
layout, refused for read and for write before any path is resolved, so the
built-in tools can neither redirect where Git operations from that root resolve
nor read the operator's repository path and, through it, a config that may
carry credentials. Ownership verification remains as a second layer for
mutations that do not go through those tools: it detects the redirect and
refuses the release, which fails closed but leaves the worktree registered in
the operator's repository.

A `copy` snapshot carries no control plane. It materializes only tracked blobs,
and Git refuses to track a path named `.git`, so the redirect constraint is
specific to the worktree layout.

A writing member's snapshot is scratch. Writes are real for the duration of the
run — a later tool call reads back what an earlier one wrote, and re-provisioning
during recovery adopts the snapshot as it stands rather than rebuilding it, so a
resumed run keeps the work it had already done. They are also confined: nothing a
member writes or creates appears in the operator's repository. Release then
removes the snapshot root, and no path publishes it anywhere first. The
consequence is worth stating plainly because "the member can write" is ordinarily
read as "the member can change the operator's files": on `copy` it cannot, and on
a successful run its output is discarded. Delivering work out of a snapshot would
be an egress path and needs its own decision.

## Execution-profile guarantees

Built-in direct-file operations accept relative paths under an opaque opened
root. They refuse absolute paths and traversal through symlinks or reparse
points. A platform advertises a particular race-resistance guarantee only when
its adapter enforces it for every path component and final target.

A contained command profile must bind executable and arguments, constructed
environment, workspace reach, network reach, descendant ownership and
termination, deadline behavior, runner version and one shared output bound.
Child environments are allowlisted; provider credentials and undeclared values
are absent. Environment names compare case-insensitively on Windows. The runtime
refuses `bash` when any requested containment guarantee is unavailable; it never
falls back to an inherited unrestricted process.

## Pre-acceptance lifecycle

Capability probing and provisioning are external work. They finish before
`run.started`, provider dispatch, or any claim that a workspace exists. The
effective configuration freezes source kind and canonical root, commit and tree,
content policies, requirements, platform decisions, provisioner versions and
profile identities.

Provisioning records ordered `prepared`, `started` and `finished` evidence under
stable job and member identities. Recovery verifies existing external state
before adopting it; path existence alone is never ownership or source evidence.
A failed preparation performs ownership-checked cleanup and publishes no
accepted run.

Resume verifies the frozen source and provisioned session before constructing an
executor. Release is idempotent and ownership-checked. Successful work may be
released according to retention policy; failed, cancelled or unknown work keeps
diagnostic evidence unless an explicit operator action removes it. One job may
never release another job's workspace.

## Host boundary

`host/v1.Workspace` is opaque. A provisioner returns the exact handle supplied
to a tool invocation, and recovery preserves both session identity and handle.
Arxi does not turn the handle into a public path or infer a guarantee the host
did not declare.

## Non-guarantees

- A directory name is not source identity or isolation.
- A Git worktree is not process, filesystem, environment or network containment.
- Internal adapters and passing negative tests are not production availability.
- The local coordination journal does not claim distributed or network-filesystem
  coordination.
- Unsupported guarantees fail before acceptance; they never degrade silently.

## Verification

Resolution tests pin the four-mode vocabulary, defaults and conflicts. Platform
preflight tests pin the exact advertised native profiles and refusal of missing
source and process guarantees. Acceptance tests require preflight before
publication or provider dispatch. Provisioning tests cover source identity,
visibility, restart, corruption, ownership and idempotent release. Filesystem
and process tests cover traversal, links, parent swaps, sentinel secrets,
network access and descendants. Host tests require the exact opaque provisioner
handle to reach the tool executor.