# ADR-0018: The repository control plane is not workspace content

- Status: accepted
- Affects: `internal/toolrun`, `internal/workspacefs`, `spec/workspaces.md`
- Depends on: ADR-0012, ADR-0017

## Context

A workspace mode names a source layout (ADR-0012). Two of the three layouts
materialize the tracked tree and nothing else. The third does not:
`git worktree add` places a `.git` **file** at the root of the worktree whose
content is a pointer:

```text
gitdir: /path/to/operator/repo/.git/worktrees/<name>
```

That file lands inside the tool-visible tree. Before this decision, toolrun's
reserved-path list covered `.arxi-workspace.json` and the ownership metadata
directory, but not `.git`, so the pointer was an ordinary file to the built-in
tools. Two consequences were verified against real Git rather than reasoned
about:

- **Write.** Rewriting the pointer at a repository the member controls moves
  `rev-parse --git-common-dir` to that repository while `--show-toplevel` still
  reports the worktree. Every Git operation performed from that root then
  resolves against a repository the member chose.
- **Read.** The same pointer names the operator's repository by absolute path.
  Following it reaches the common `config`, where a remote URL can carry an
  embedded credential — so reading a file inside the workspace discloses both
  where the operator's checkout lives and, potentially, a secret nobody
  granted.

Neither is an escape of the confined file API. The bytes are inside the
workspace, which is exactly where the profile says writes may land. That is
what makes it a naming problem rather than a containment bug, and therefore an
ADR-0012 problem: the mode promises *the tracked tree*, and the tracked tree is
not what the member reached.

This blocked the pending writer decision. An advertisement of a writable
source-backed mode had to say which guarantee it made — "a writer cannot
redirect it" or "a redirect is detected afterwards" — and only the weaker one
was true.

## Decision

**A repository control plane is never workspace content. `.git` is a reserved
path in every layout, refused for read and for write, before any path is
resolved.**

The rule is a first-path-component match in `validateToolPath`, which both the
read and the write paths already traverse.

### Read is refused, not only write

Write is the obvious half. Read is refused because the disclosure — operator
path, and a config that may carry credentials — is a real loss on its own, and
because a rule that refuses writes while permitting reads invites the reading
half to be treated as harmless and re-enabled later.

### The rule does not depend on the mode

`.git` is refused in `shared` and `copy` too, where the provisioner creates no
such entry. A reserved name whose meaning depends on the layout is a rule
nobody can apply from the path alone. All three layouts promise the tracked
tree; none of them promises a control directory.

### The rule is the first component only

`vendor/dep/.git` is not refused. The source policy already refuses submodules
at provisioning time, so a live control directory below the root should not
exist in a provisioned workspace. Widening the match to every component would
also refuse ordinary tracked files like `src/.gitignore`, and a rule that
breaks real repositories gets deleted rather than narrowed.

### Ownership verification stays

`verifyWorktree` continues to compare the observed common directory against the
frozen source and refuse the release on a mismatch. It is now the second layer,
answering "what if something other than the built-in tools mutated the tree",
and its failure mode is unchanged: the release fails closed and the worktree
remains registered in the operator's repository for a human to remove.

## What this does and does not settle for writers

`copy` was audited against the same standard. It carries no control plane, and
the reason is structural: it materializes only blobs named by the tracked-entry
walk, and Git refuses to track a path named `.git` at all, so a repository
cannot smuggle one in as content.

This decision removes the redirect objection from the writer platform decision.
It does **not** advertise a writable mode. Descendant, environment, filesystem
and network guarantees for writers remain unproven, and `copy` and `worktree`
stay unadvertised on every platform until one decision can promise the complete
contract (ADR-0012).

## Discarded alternatives

**Document the weakness and advertise the weaker promise.** Rejected: the
disclosure half makes it worse than a cleanup nuisance, and a three-line
reserved-path entry buys the stronger promise. Documenting a gap that is cheap
to close is how a gap becomes permanent.

**Refuse `.git` only in the worktree layout.** Rejected: a reserved name that
depends on the mode cannot be applied from the path alone, and the
tracked-tree promise is identical in all three layouts.

**Rely on ownership verification alone.** Rejected: it runs at release, after
the run has already executed against a redirected repository, and it does
nothing at all about read disclosure.

**Move the worktree `.git` pointer outside the root.** Rejected: its location
is Git's, not ours. Fighting it would mean maintaining a layout Git does not
produce, and every Git invocation inside the workspace would become the thing
that breaks.

## Consequences

- A member using the built-in file tools cannot read or write `.git` in any
  layout; the refusal names what it protects so it is not mistaken for a typo.
- The worktree redirect surface is closed at the point of use, and still
  detected at release if something else mutates the tree.
- The writer platform decision loses one blocking objection and keeps the
  others.
- A tool that wanted to inspect a repository's own `.git` is refused. None
  does today, and a future one would need an explicit capability rather than
  ambient reach through the workspace.

## How it is verified

`internal/toolrun/gitcontrol_test.go` pins that `.git`, `.GIT` and paths
beneath them are refused for `Resolve`, `WriteFile` and `ReadFile`; that the
rule does **not** refuse `.gitignore`, `.gitattributes`, `src/.gitignore` or
`gitconfig`, since over-refusal is the failure mode that gets rules deleted;
and that the refusal message names what it protects. Validated by mutation:
disabling the check fails those tests.

`internal/workspacefs/worktree_gitfile_test.go` pins the second layer — a
direct write bypassing toolrun is still caught by ownership verification, the
release refuses, and the worktree stays registered. Validated by mutation:
neutralizing `verifyWorktree` fails it.

`internal/workspacefs/copy_audit_test.go` pins that a `copy` snapshot contains
no `.git` entry, and separately that Git refuses to track a path named `.git` —
the upstream invariant the copy audit depends on, pinned so that its loss is
loud rather than silent.
