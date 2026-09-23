# ADR-0039: The atomic-rename publish writes its temp into the destination's directory, a derived family invariant

- Status: accepted
- Affects: `internal/store_rename_dir_test.go`
- Depends on: ADR-0026, ADR-0034, ADR-0035, ADR-0036, ADR-0037, ADR-0038

## Context

Every store in this project that publishes a record by renaming a temp file over
its final name writes the same shape:

- the rename stores — `os.CreateTemp(s.dir, name+ext+".tmp-*")`, then
  `os.Rename(tmp.Name(), s.Path(name))`, where `Path` is
  `filepath.Join(s.dir, name+ext)`.
- `logstore`'s snapshot — `os.CreateTemp(s.dir, snapshotFileName+".tmp-*")`, then
  `os.Rename(tmp.Name(), filepath.Join(s.dir, snapshotFileName))`.

In both, the temp is created in the directory the rename targets. That is not an
incidental detail: `os.Rename` is atomic only within a single filesystem, and
the family gets that guarantee precisely by creating the temp beside its
destination. ADR-0036 (the temp name can never end in the globbed extension, so
a write interrupted before the rename leaves nothing a reader offers) and
ADR-0037 (fsync the file, rename, then fsync the directory) both rest on the
rename being atomic. Neither derives it.

Probed at its widest point — every store that publishes through
`CreateTemp`+`Rename` — the behaviour is correct: all six rename stores and
`logstore`'s snapshot create the temp in the destination's own directory. But
nothing guarded it. Routing a temp through `os.TempDir()` — a plausible "use the
system temp directory" cleanup — leaves every store's suite green while turning
the publish into a cross-filesystem rename: `EXDEV` on Linux, where the publish
fails outright, and elsewhere a non-atomic copy-then-delete, where a reader can
observe a half-written record. That is the exact torn read ADR-0036's suffix
rule exists to prevent, reintroduced one layer beneath it. This is the shape
ADR-0035 through ADR-0038 recorded on the four turns before it: a family-wide
property carried by the shape of a call and pinned by no guard.

## Decision

**The same-directory publish is enforced by reading each store's own `CreateTemp`
and `Rename` and requiring the rename destination's directory to equal the
temp's, not by trusting the shape of the call to stay right.** A single guard
parses every `internal/*store` package; for each function that both creates a
temp and renames it, it resolves the destination directory from source and holds
it equal to the `CreateTemp` directory argument.

The destination directory is resolved without evaluating it, in the two forms
the family uses. A destination of `filepath.Join(D, ...)` has directory `D`
(logstore's snapshot). A destination of `s.Path(...)` has the directory that
`Path`'s own `filepath.Join` joins on — which the analyzer reads from the same
file, since `Path` and the publish live together in each store's `store.go`. A
destination in neither form is **not** assumed same-directory; it is reported,
so an unrecognized publish fails closed rather than passing as safe, the way
ADR-0036 and ADR-0038 treat a form they cannot read.

The subject is derived for the reason ADR-0026, ADR-0035, ADR-0036, ADR-0037 and
ADR-0038 give: the property holds across the family and is expressed in every
store's own two calls, so trusting the shape to stay right is one more thing to
keep in step by hand. Reading the calls ties the guard to the code it guards, so
a temp moved off the destination's directory fails the first time the suite
runs, and a publish whose destination the analyzer cannot resolve fails closed
naming the function rather than passing over a form it did not understand.

**The corpus is the rename-publishers, a measured boundary of its own.** Unlike
ADR-0038, whose corpus is the `const ext` stores, this guard's subject is every
store that publishes through `CreateTemp`+`Rename`. That boundary *includes*
`logstore`'s snapshot — which declares no `const ext`, and so is outside
ADR-0038's scope, yet renames a temp into place exactly as the ext stores do —
and *excludes* `memorystore` (an `O_EXCL` content-addressed create with no
rename, ADR-0034) and `jobstore` (an append-only journal, with no
`CreateTemp`+`Rename` pair). The boundary is stated as what the guard measures,
not enumerated by hand.

## Consequences

The atomicity of the family's publish no longer rests on the shape of a call
nobody checks. A store added later, or an existing publish whose temp is routed
off the destination's directory, is caught with a message naming the file, the
function, the consequence (a cross-filesystem rename that is not atomic, or fails
outright, so a reader can observe a half-written record) and the remedy (create
the temp in the directory the rename targets). `logstore`'s snapshot, outside
ADR-0038's `const ext` corpus, is covered here by the boundary that fits this
property.

The check is exactly as strong as "the temp's directory expression equals the
destination's directory expression, read from source", and no stronger. It does
not prove the two are on the same physical device at runtime — a directory
cannot span filesystems, so same directory is the source-visible sufficient
condition for the atomicity the rename needs, and it is the strongest thing
derivable without executing the store. It closes the last unguarded assumption
under the family's atomic write: ADR-0036 guards the temp's name, ADR-0037 its
fsync order, ADR-0038 its mode, and this its location.

This ADR adds no production code. It records that the same-directory publish —
the property that makes `os.Rename` atomic and on which ADR-0036 and ADR-0037
both depend — was carried only by the shape of each store's `CreateTemp` call,
and replaces that with a guard derived from the calls themselves: the fifth
derived family invariant over these stores, after locality (ADR-0035),
temp-suffix atomicity (ADR-0036), durability order (ADR-0037) and the file-mode
floor (ADR-0038).

## Discarded alternatives

**Add a per-store test asserting the temp directory in each publish.** Six or
seven edits that would still leave the next store free to omit it — the exact
state ADR-0036 found for the temp suffix and ADR-0038 for the mode. The property
is identical across the family, so it is enforced once, over the family, from the
source.

**Assert the temp and destination resolve to the same device at runtime.**
Tighter in principle, but "same filesystem" is not a property the source states:
it would mean opening the store, creating files and comparing `Stat` device
numbers — a runtime, platform-dependent test of the OS rather than of the code,
and one the kernel purity boundary and this project's static-guard style both
push away from. Same directory is the condition the source can show and the one
rename atomicity actually needs.

**Resolve the destination directory by evaluating `Path`.** Running the method
would follow it through `filepath.Join`, but it would also drag the guard toward
executing store code to read a property that is plain in the source. Reading the
directory `Path`'s own `filepath.Join` joins on — present in the same file —
gives the same answer statically, and a destination in no recognized form fails
closed instead of being guessed.

**Trust the shape of the call.** That is the state this ADR corrects. The
same-directory publish held in six stores and a seventh snapshot with no test
anywhere, and a guard that would pass over a temp created in `os.TempDir()` is
indistinguishable from one asserting nothing.

## How it is verified

`internal/store_rename_dir_test.go`:

- `TestEveryStoreRenamePublishTargetsItsTempDirectory` — parses every
  `CreateTemp`+`Rename` publish in every store and requires the rename
  destination's directory to equal the temp's directory. It proves its own
  analyzer against fixtures before trusting the corpus — a cross-directory
  publish must fail; the two same-directory forms the family uses (`filepath.Join`
  directly, and a resolved `s.Path(...)`) must pass; and a destination in neither
  form must fail closed — and fails closed over the whole corpus if no
  `CreateTemp`+`Rename` publish is found at all, the case where the write moved
  and the scan silently went empty.

Confirmed to fail closed by mutation: routing `rolestore`'s temp through
`os.TempDir()` (the `s.Path(...)` destination form) and routing `logstore`'s
snapshot temp through `os.TempDir()` (the `filepath.Join` destination form) each
fail the guard naming the offending file and function (`write` and
`WriteSnapshot`) — the cross-filesystem-rename regression no test caught before
this existed.
