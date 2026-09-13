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
- `arxi.workspace/direct-files-v1` permits only built-in relative file access;
- `arxi.workspace/contained-process-v1` additionally requires declared process
  containment.

A profile identity is the digest of its complete versioned guarantee, not its
display name. Authorization and resume bind that identity.

## Pure requirement resolution

Resolution performs no probing or provisioning. Text-only members resolve to
`none` and `no-tools`. `read` and `grep` require a shared source view and
read-only direct files. `write` and `edit` require a per-writer source view and
write-capable direct files. `bash` requires a per-writer source view and the
contained-process profile.

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
| Linux | `none` | `no-tools`, `direct-files` | none |

Linux direct-file operations can provide handle-relative, final-link-race-free
access, but no native source-backed mode is advertised with which that profile
can form an accepted combination. Neither platform advertises
`contained-process`. Simulation may exercise every mode and profile, but that is
simulation behavior and not a production isolation claim.

Internal `shared`, `copy` and `worktree` provisioners remain evidence toward the
contract. A production probe must not add them until one platform decision can
prove source identity, lifecycle, file access and any requested process
containment together.

## Mode guarantees

`none` has no root. File and process tools are unavailable, and the runtime must
not silently create an empty directory and call it isolation.

If advertised, `shared` is one verified frozen source tree. Writes are visible
between members and it makes no cross-member isolation claim.

If advertised, `copy` is one deterministic tracked-tree snapshot per writing
member. Dirty, untracked and ignored content is excluded. Submodules and special
files are refused. A symlink is reproduced only when its recorded relative
target remains inside the snapshot.

If advertised, `worktree` is one registered detached Git worktree per writing
member at the frozen commit. Recovery verifies the common Git directory,
registration, HEAD and ownership. A worktree separates working trees; it is not
a process sandbox.

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