# ADR-0019: Linux advertises the copy snapshot as writable, and stops there

- Status: accepted
- Affects: `internal/workspace`, `internal/workspacefs`, `spec/workspaces.md`
- Depends on: ADR-0012, ADR-0017, ADR-0018

## Context

ADR-0017 left Linux advertising exactly one source-backed combination:
`shared` with the read-only `direct-files-read` profile. No accepted
combination could write. That was the honest position — not because writing
was impossible, but because nothing had proven what a writable workspace would
contain.

Proving it took four audits, and each one found something:

- **The repository control plane** (ADR-0018). A `worktree` root holds a
  `.git` file pointing into the operator's repository. Writing it redirects
  every Git operation performed from that root; reading it discloses the
  operator's absolute path and, through the common config, potentially a
  credential. `copy` was then audited against the same standard and found
  structurally immune: it materializes tracked blobs only, and Git refuses to
  track a path named `.git`.
- **Path confinement versus inodes.** A hardlink is a second name for one
  inode, not a path pointing elsewhere, so path confinement cannot refuse it.
  Verified that a write through a hardlinked name does change a file outside
  the workspace — and separately that `copy` does not expose that, because
  `copyTrackedTree` writes fresh blobs rather than linking.
- **What a member's writes actually do.** Writes never reach the operator's
  repository, for an overwritten tracked path or a newly created file alike.
  Recovery adopts the snapshot in place, so a resumed run keeps its earlier
  work. `Release` then removes the snapshot root, and nothing publishes it
  first.
- **How an advertisement is interpreted.** Modes and profiles were two
  independent lists and acceptance validated them separately, so an
  advertisement meant their *cross product*: advertising the write profile for
  `copy` would also have made the operator's `shared` tree writable.
  ADR-0017's "shared only with the read-only profile" was true because the
  advertisement was one layout by one profile — a count, not a rule.
  `Capabilities.Pairs` now states the pairing and preflight enforces it.

The last of these is why this decision could not have been taken earlier
without shipping a regression. The first three are why it can be taken now.

## Decision

Linux advertises `copy` paired with the write-capable `direct-files-v1`
profile, and with `direct-files-read-v1` so a reader that declares
`workspace: copy` is not refused for asking for less than the layout offers.

`shared` keeps its exclusive pairing with the read-only profile. The
operator's frozen tree stays unwritable, now by rule rather than by the write
profile being absent from the advertisement.

**The line is file access, not write access.** A member whose tools are
`read`, `grep`, `write` or `edit` is accepted. A member with `bash` is not,
and is refused twice over: it resolves to `worktree`, which stays
unadvertised, and to `contained-process`, whose four process guarantees —
descendants, filesystem, environment, network — are all still `"unavailable"`.

That asymmetry is the decision, not an omission. The file guarantees are
enforced by mechanism and checked by adversarial tests; the process ones are
not enforced at all. ADR-0012 permits advertising only what an adapter can
prove, and the file half is provable today.

## Consequences

A writing member gets a **scratch tree**. Writes are real for the duration of
the run and survive recovery, they never touch the operator's repository, and
on a successful run they are discarded with the snapshot. Nothing publishes
them anywhere first.

This is worth stating plainly, because "the agent can write" is ordinarily
read as "the agent can change my files". On `copy` it cannot. The layout is
therefore useful for work whose product is a *conclusion* — analysis,
verification, review, running a check over a modified tree — and not for work
whose product is a *delivered artifact*. Delivering work out of a snapshot
would be an egress path needing its own decision and its own threat model; it
is deliberately not smuggled in here.

`worktree` and `contained-process` remain unadvertised on every platform.
Windows still advertises only `none` with `no-tools`.

## Discarded alternatives

**Advertise `worktree` instead of `copy`.** ADR-0018 refuses the `.git`
pointer at the tool boundary, so a worktree is defensible. But its root still
*contains* a control plane, so the guarantee needs a clause: "the tracked
tree, plus a pointer you may not touch." `copy`'s root holds tracked files and
nothing else, and a member without `bash` cannot invoke Git anyway, so the
repository a worktree serves buys it nothing. A guarantee with no exceptions
is worth more than a stronger-sounding one with a caveat.

**Advertise `copy` with `bash` as well.** This is the request most likely to
arrive next, and the one this ADR exists to refuse. `copy` isolates a *tree*;
it says nothing about processes. Granting `bash` on a layout audited only for
file behaviour would let four unproven process guarantees ride in under a name
earned by different evidence — exactly the confusion ADR-0012 was written to
prevent.

**Wait for the process guarantees and advertise everything at once.** It
would keep file-only writers blocked indefinitely on work that is genuinely
harder and separately auditable. The audits already done pin a real boundary;
withholding their result until an unrelated one lands wastes them.

**Advertise the write profile without the pairing.** The cheapest option, and
the one a careless implementation produces: since acceptance validated modes
and profiles independently, adding the write profile would have made `shared`
writable too. It contradicts ADR-0017, and nobody would have decided it.

## How it is verified

`internal/workspace/preflight_test.go` pins the advertised matrix — modes,
profile IDs, provisioner versions, and that the write profile carries the same
handle-relative, final-link-race-free guarantees the read profile does. It
also pins the boundaries this decision does not move, including `shared`+write,
which is refused *only* by the pairing now that both halves are advertised.

`TestEveryToolConfigurationLandsOnTheSideOfTheLineADR0019Drew` drives real
tool lists through `Resolve` and asserts which side of the line each lands on.
It replaces a test that asserted every write was refused; driving tools
through resolution rather than hand-building requirements is what keeps it
from being satisfied by a stale assumption about which layout a writer picks.

`internal/workspacefs/probe_test.go` repeats the boundary checks against the
*probed* capabilities. `Probe` rewrites `Provisioners`, so it is a second
construction of the advertisement and can drift from the one the preflight
tests reason about.

`internal/workspace/pairing_rule_test.go` pins that the operator's shared tree
stays read-only by rule: both file profiles are advertised now, so deleting
the pairing re-opens `shared`+write and that test fails.

The audits this decision rests on are pinned where they were found:
`internal/toolrun/gitcontrol_test.go` (control plane refused for read and
write), `internal/workspacefs/copy_audit_test.go` (no control plane in a
snapshot), `internal/toolrun/hardlink_test.go` with
`internal/workspacefs/inode_isolation_test.go` (path confinement versus
inodes, both sides), and `internal/workspacefs/write_lifecycle_test.go`
(writes confined, discarded on release, preserved across recovery).
