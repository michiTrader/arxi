# ADR-0017: Shared on Linux is read-only because a mechanism refuses writes, not because none were granted

- Status: accepted
- Affects: `internal/workspace`, `internal/workspacefs`, `internal/toolrun`, `cmd/arxi/runtime.go`, `spec/workspaces.md`
- Depends on: ADR-0012

## Context

The first milestone requires a model to request one read-only tool (`read`,
`grep`) and execute it against the run's source. Every platform refuses that
today: `read`/`grep` resolve to workspace mode `shared`, no platform advertises
a source-backed mode, and preflight fails the run before acceptance.

The tempting shortcut is to add `shared` to the advertised Linux modes and
call it read-only. The map of the current implementation says why that would
be a lie:

- `shared` is a **copy of the tracked tree**, one per job, published
  atomically — and it is writable at the filesystem level.
- The profile readers resolve to is the same write-capable `direct-files`
  profile writers use; there is no read-only profile.
- Nothing below the granted tool list enforces read-only: a member granted
  `write` alongside a shared requirement would mutate the shared view, and
  preflight would accept it (write-capable profile satisfies write access).

Under ADR-0012, availability means the platform promises guarantees that
adversarial tests can verify. "Read-only" enforced only by the absence of a
grant is an accident of configuration, not a guarantee.

## Decision

**Linux advertises workspace mode `shared` paired exclusively with a new
read-only profile, and the runtime refuses mutating tools against a read-only
session. Read-only-ness becomes a mechanism at two boundaries, so the promise
holds even when configuration drifts.**

### The read-only profile

`arxi.workspace/direct-files-read-v1` carries `file_access: read` with the
same handle-relative, final-link-race-free file guarantees as its write-capable
sibling. Requirement resolution assigns it to members whose tools only ever
read (`read`, `grep`); the write-capable `direct-files` profile remains for
writers and for the simulation platform.

Linux advertises `shared` **only with this profile**. The write-capable
`direct-files` profile leaves the Linux advertisement, so no accepted
combination on Linux can write: a write requirement over the read-only profile
fails preflight, and writers resolve to `worktree`, which stays unadvertised.
The only source-backed combination Linux accepts is `shared` + read-only —
the milestone's shape, exactly.

### Runtime enforcement

`toolrun.Workspace` carries the session's file access, plumbed from the frozen
requirement through the provisioned session, and `write`/`edit` refuse unless
the session's access is `write` — before any path is touched. This is
deliberately redundant with preflight: preflight refuses the configuration at
acceptance time; the session refuses the dispatch at execution time. A future
path that grants a mutating tool to a read-only session fails closed at the
point of use instead of silently succeeding because nothing checked.

Unplumbed access refuses writes too. A missing value is not a promise of
write; making it one would return read-only-ness to accident status.

### What the advertisement promises

For an accepted Linux run with read tools: one verified frozen copy of the
tracked tree (commit and tree recorded, dirty/untracked/ignored content
excluded, submodules and special files refused, symlinks internal-relative
only), confined handle-relative access that cannot escape the tree or follow
a final-component symlink out, read-size and search caps, mutating tools
refused at the session, and the pre-acceptance provisioning lifecycle with
crash recovery. It promises **no** cross-member isolation story beyond "one
shared view", no process guarantees, and no immutability of the underlying
operator checkout (the copy is frozen; the checkout may advance).

## Discarded alternatives

**Advertise `shared` as-is and document that grants govern mutation.**
Rejected: the spec would then advertise a read-only milestone on a mechanism
that is the absence of a grant. ADR-0012 exists precisely to keep names and
guarantees from diverging like that.

**Advertise the write-capable profile too, for operators who want a writable
shared view.** Rejected for this decision: it reopens the exact combination
(shared + write) that makes "read-only" unprovable, and no consumer needs it
yet. When one does, it is its own platform decision with its own contract and
tests.

**A new `read-only` mode instead of a profile.** Rejected: modes describe
source layouts (how the tree is materialized); access describes what may be
done to it. Read-only copy and read-only worktree are the same access over
different layouts; making access a mode would entangle the two axes and
multiply the matrix.

**Serve the operator checkout directly instead of copying.** Rejected: a live
checkout changes under the run, and the frozen-tree copy is what makes
"verified frozen source tree" a checkable statement rather than a hope.

## Consequences

- A Linux run whose members only read can be accepted and can execute `read`
  and `grep` against the frozen tracked tree — the milestone unblocks.
- Writers and process users remain unaccepted on every platform; their
  platform decisions are still open.
- Windows remains text-only; its file access is not handle-relative and no
  source-backed promise is made there yet.
- Existing runs are unaffected: the advertisement is consulted at acceptance,
  and accepted runs replay under their frozen contracts.
- The Phase 4 pins that asserted no source-backed advertisement now assert
  the narrower fact they existed to protect: only combinations whose full
  contract is proven are advertised.

## How it is verified

Resolution tests pin that readers resolve to the read-only profile and writers
to the write-capable one. Preflight tests pin that a write requirement over
the read-only profile is refused, that the Linux advertisement accepts a
read/grep requirement and refuses every write combination, and that the
capability matrix is exactly the advertised set. Toolrun tests pin that
`write`/`edit` against a read-only session refuse before any filesystem
effect, and that unplumbed access refuses too. The existing confinement suite
(escape, symlink swap, TOCTOU, reserved paths, size caps) continues to hold
for the reads themselves, and the provisioning lifecycle tests continue to
hold for the shared copy.
