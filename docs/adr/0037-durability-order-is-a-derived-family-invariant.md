# ADR-0037: The file-before-directory fsync order is a derived family invariant, not a per-store comment

- Status: accepted
- Affects: `internal/store_durability_order_test.go`
- Depends on: ADR-0026, ADR-0035, ADR-0036

## Context

Every content store in this project makes a write durable in the same order: it
fsyncs the file so its bytes are on disk, then (for the stores that publish by
rename) renames the temp over the final name, then fsyncs the directory so the
entry that now names those bytes is itself durable. The order is the whole
point. Reversed — the directory fsynced before the file — a crash can leave a
directory entry pointing at a file whose contents never reached the platter,
which is exactly the torn read the sequence exists to prevent. Fsyncing the file
does not make the entry that names it durable, so the directory needs its own
fsync, and it must come last.

Two comments state this as a property of the whole family:

- `memorystore/store.go:172` — "Sync the file before the directory, then the
  directory ... The same order every other store in this project uses."
- `agentstore/store.go:844` — "The sequence is toolstore's, for the same
  reasons: ... fsync before the rename ... and fsync of the directory after."

Both are cross-package claims: one store's comment asserting a property of six
others. Probed at its widest point — every content store in the family:
`rolestore`, `modelstore`, `agentstore`, `toolstore`, `trigstore`, `evalstore`
each fsync the temp, rename, then fsync the directory; `memorystore` fsyncs its
`O_EXCL` file then the directory — the behaviour is correct everywhere.

But nothing guarded it. There was no test anywhere asserting the order. This is
weaker than the temp-suffix convention ADR-0036 corrected: that at least had six
per-store tests, decoupled but present. Here the order lived only in prose, so
reversing `memorystore` to fsync the directory before the file, or moving a
rename store's directory fsync ahead of its rename, changed no test result. This
is the shape ADR-0035 and ADR-0036 recorded on the two turns before it, and the
one ADR-0026 recorded before them: a family-wide property argued in one place,
generalised to N packages, and pinned in none — which goes stale precisely in
the store nobody re-checks.

## Decision

**The order is enforced by reading each store's own write from source and holding
the calls to the file-before-directory order, not by trusting one comment's
claim about the others.** A single guard parses every `internal/*store` package
that declares `const ext`, and for each function that both writes file content
and fsyncs a directory it requires: a file fsync exists, it precedes the
directory fsync, and — when the function renames — a file fsync precedes the
rename and the rename precedes the directory fsync.

Calls are classified by selector name, because the receiver varies (`tmp`, `f`,
`file`) but the operation does not: `.Sync()` is a file fsync,
`fsdurability.SyncDirectory(...)` a directory fsync, `os.Rename` the atomic
publish, and `os.CreateTemp` / `.Write(...)` the evidence that the function
writes file content at all. A function that fsyncs a directory but writes no
content is a delete or tombstone — it makes the removal of an entry durable and
has no file of its own to fsync — and is correctly not held to an order it has
no file for. Keying the requirement on the content write, not on the directory
fsync, is what makes a publish that *dropped* its file fsync fail here rather
than pass as if it were a delete.

The subject is derived for the reason ADR-0026, ADR-0035 and ADR-0036 give: the
order is identical across the family and duplicated in every store, so a
hand-listed or hand-restated copy of it in a test is one more thing to keep in
step by hand — which is the failure, not the fix. Reading the calls the store
actually makes ties the guard to the code it guards, so a write reordered so the
directory fsync moves ahead of the file fsync or the rename fails the first time
the suite runs.

The corpus is the same family as ADR-0035 and ADR-0036: the ext-declaring
content stores, which all route directory durability through a direct
`fsdurability.SyncDirectory` call. `jobstore` and `logstore` reach the same
order through a `syncDir` helper and an ops seam rather than a direct call, so
the direct-call analyzer does not see them; they are out of this guard's scope
by that boundary, stated as a measured limit rather than an exemption. (Spot
checked: `jobstore`'s `commit` and `writePending` both fsync the file before
`syncDir`, so the order holds there too — through a shape this guard does not
reach.)

## Consequences

The guarantee no longer rests on two comments' claim about six packages. A store
added later, or an existing write reordered, is caught with a message naming the
file, the function, the consequence (a crash can leave the entry naming bytes
that never reached disk) and the remedy (fsync the file before the rename, the
directory only after).

The check is exactly as strong as "some file fsync precedes the directory fsync,
and the rename sits between them", and no stronger. It does not prove the fsyncs
succeed at runtime, that the platform honours them, or that the rename is atomic
on every filesystem — those are properties of the OS and `fsdurability`, not of
call order. It closes the one gap that had no guard at all: the order itself,
which the family's own comments called load-bearing and no test held.

This ADR adds no production code. It records that a family-wide durability order,
asserted for six stores in two comments, was carried by prose alone, and replaces
that with a guard derived from the writes themselves — the third derived family
invariant over this same corpus, after locality (ADR-0035) and temp-suffix
atomicity (ADR-0036).

## Discarded alternatives

**Add a per-store test asserting the order in each package.** Seven edits, each
re-establishing the same coupling in a slightly different local way, and nothing
stopping the eighth store from omitting it — the exact state ADR-0036 found for
the temp suffix. The order is identical across the family, so it is enforced
once, over the family, from the source.

**Crash the process between the file fsync and the directory fsync and assert
the recovery.** Closest to the runtime truth, but there is no portable hook to
stop a process at that point, and a timing-based attempt would be a flake, not a
guard. The order is a static property of the call sequence, so it is checked
statically.

**Extend the guard to jobstore and logstore by following the `syncDir` helper.**
Tempting, since they hold the same order, but resolving one level of indirection
per store re-introduces exactly the hand-maintained mapping this family of ADRs
exists to remove, and it would go stale the moment a store wrapped its sync
differently. The direct-call boundary is stated instead of papered over.

**Trust the comments.** That is the state this ADR corrects. A property asserted
for six stores in two comments and pinned in none is decoration, and a test that
would pass over a reversed store is indistinguishable from one asserting nothing.

## How it is verified

`internal/store_durability_order_test.go`:

- `TestEveryStorePublishFsyncsTheFileBeforeTheDirectory` — parses every durable
  publish function in every store declaring `const ext` and requires the file
  fsync before the directory fsync, with the rename between them where one is
  present. It proves its own analyzer against a known-bad fixture (directory
  fsync moved ahead of the file fsync) and a known-good one before trusting the
  corpus, and fails closed if it finds no publish function at all, the case where
  the write moved and the scan silently went empty.

Confirmed to fail closed by mutation: moving a rename store's directory fsync
ahead of the rename, reversing `memorystore`'s file and directory fsyncs, and
dropping a rename store's file fsync entirely each fail the guard naming the
offending file and function — the reorders no test caught before this existed.
