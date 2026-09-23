# ADR-0041: The atomic-rename publish defers removing its temp, a derived family invariant

- Status: accepted
- Affects: `internal/store_temp_cleanup_test.go`
- Depends on: ADR-0026, ADR-0034, ADR-0036, ADR-0039, ADR-0040

## Context

Every store in this project that publishes a record by renaming a temp over its
final name registers `defer os.Remove(tmpName)` the instant it has the temp's
name — before the write, the fsync, the close, the chmod or the rename, any of
which can return early. Every rename store, and `logstore`'s snapshot, carries it
with the same comment: "a no-op once the rename has succeeded". A publish that
fails part-way runs the defer and removes the temp; a publish that succeeds
leaves nothing for the defer to find, because the rename consumed the temp name.

Probed at its widest point — every store that publishes through
`CreateTemp`+`Rename` — the cleanup is present everywhere. But nothing guarded
it, and dropping it breaks no test. ADR-0036 guarantees the temp name can never
end in the globbed extension, so an orphan is never *offered* as a real record,
and every listing-based test stays green. That is exactly what makes the
accumulation invisible: one stray `agent.yaml.tmp-4817231` per interrupted write,
unseen by the store's own listing but present on disk — and committed by a user
who tracks `roles/`, `agents/` and `triggers/` in git, since those are
team-visible files the project's own comments describe as "expected to read and
commit". The defer is what keeps a transient failure from leaving a permanent
artifact in a version-controlled directory. This is the shape ADR-0035 through
ADR-0040 recorded on the six turns before it: a family-wide property carried by
the shape of a call and pinned by no derived guard.

## Decision

**The deferred cleanup is enforced by reading each store's own `defer os.Remove`
of its temp and tying it to that temp, not by trusting the comment.** A single
guard parses every `internal/*store` package; for each function that both creates
a temp and renames it, it reads the variable `os.CreateTemp` is assigned to and
the name variable bound from that variable's `.Name()`, and requires a `defer
os.Remove` of that name — or of `<tmp>.Name()` directly.

Tying the deferred remove to the temp is the point: a rename-publish with no
deferred remove, or one whose deferred remove targets something the guard cannot
trace back to the temp, fails closed, so an unrelated `os.Remove` that happened
to be present cannot stand in for the cleanup. The subject is derived for the
reason ADR-0026, ADR-0036, ADR-0039 and ADR-0040 give: the property holds across
the family and is expressed in every store's own defer, so trusting the comment
is one more thing to keep in step by hand. Reading the defer ties the guard to
the code it guards, so a dropped cleanup fails the first time the suite runs.

**The corpus is the rename-publishers, the same measured boundary as ADR-0039 and
ADR-0040:** every store that publishes through `CreateTemp`+`Rename`. That
boundary includes `logstore`'s snapshot and excludes `memorystore` (an `O_EXCL`
content-addressed create with no rename, ADR-0034) and `jobstore` (an
append-only journal, no `CreateTemp`+`Rename`). The boundary is stated as what
the guard measures, not enumerated by hand.

## Consequences

The last step of the family's atomic write left underived is now derived. A store
added later, or an existing publish whose defer is dropped or repointed away from
the temp, is caught with a message naming the file, the function, the consequence
(an orphaned temp accumulating in a git-tracked directory, invisible to the
listing) and the remedy (defer `os.Remove` of the temp's name right after
creating it). Together with ADR-0037 (fsync order), ADR-0039 (the temp's
directory) and ADR-0040 (close before rename), the whole publish sequence —
create, write, fsync, close, chmod, rename, and clean up on failure — is now read
from the source rather than trusted to the comments that describe it.

The check is exactly as strong as "a deferred `os.Remove` of the temp's name is
present in source", and no stronger. It does not prove the defer is registered
before the first fallible operation (in every store it is, immediately after
`tmp.Name()`), nor that `os.Remove` succeeds at runtime — a remove that fails is
itself a no-op for correctness, since the file is either already gone or will be
overwritten by the next write of the same name. It closes the gap that mattered:
a publish that silently stopped cleaning up after itself.

This ADR adds no production code. It records that the deferred temp cleanup —
present in seven publishes, commented identically, and tested nowhere — was
carried only by the shape of each store's write, and replaces that with a guard
derived from the defer itself: the seventh derived family invariant over these
stores, after locality (ADR-0035), temp-suffix atomicity (ADR-0036), durability
order (ADR-0037), the file-mode floor (ADR-0038), the same-directory rename
(ADR-0039) and close-before-rename (ADR-0040).

## Discarded alternatives

**Add a per-store test asserting the deferred remove in each publish.** Seven
edits that would still leave the next store free to omit it — the exact state
ADR-0036 found for the temp suffix and ADR-0038 for the mode. The cleanup is
identical across the family, so it is enforced once, over the family, from the
source.

**Accept any deferred `os.Remove` without tying it to the temp.** Simpler, but it
would let a `defer os.Remove(someUnrelatedPath)` satisfy the guard while the temp
leaked — the vacuity ADR this project's guidance warns against, a check
indistinguishable from asserting nothing. Tracing the argument back to the
`CreateTemp` result is what makes the guard mean what it says.

**Require the defer to precede the first fallible call.** Tighter, and true of
every store today, but it adds ordering machinery for a refinement: a defer
placed after the write still cleans up every failure from that point on, and the
regression worth catching — the defer removed entirely, or pointed elsewhere — is
caught by presence and binding alone. Position is left to ADR-0040's kind of
ordering guard, which exists for a step whose order is load-bearing; this one's is
not, so long as it is present.

**Trust the comment.** That is the state this ADR corrects. "A no-op once the
rename has succeeded", repeated in seven files, is documentation of an intent that
no test held, and a guard that would pass over a publish that dropped the defer is
indistinguishable from one asserting nothing.

## How it is verified

`internal/store_temp_cleanup_test.go`:

- `TestEveryStoreRenamePublishDefersRemovingItsTemp` — parses every
  `CreateTemp`+`Rename` publish in every store and requires a deferred
  `os.Remove` of the temp's name. It proves its own analyzer against four
  fixtures before trusting the corpus — a deferred remove of the bound name
  variable and of `tmp.Name()` directly must pass, a publish with no deferred
  remove must fail, and a deferred remove of something other than the temp must
  fail closed — and fails closed over the whole corpus if no `CreateTemp`+`Rename`
  publish is found at all, the case where the write moved and the scan silently
  went empty.

Confirmed to fail closed by mutation: removing `rolestore`'s `defer
os.Remove(tmpName)` entirely, and repointing `toolstore`'s deferred remove at
`s.dir` instead of the temp, each fail the guard naming the offending file and
function — the silent-orphan regression no listing-based test would catch.
