# ADR-0036: The temp-suffix atomicity guard is derived from the write, not a hand-built name

- Status: accepted
- Affects: `internal/store_temp_atomicity_test.go`
- Depends on: ADR-0026, ADR-0035

## Context

Every content store that publishes a named file does it the same way: write the
bytes to a temp file in the same directory, fsync, then rename over the final
name. The rename is atomic, so a reader listing the directory never sees a
partial file — *provided the temp file is not itself offered by the listing*.
The stores select records by extension (`Names`/`List` skip any entry that does
not end in `ext`), so the temp must not end in `ext`. They achieve that with
`os.CreateTemp(dir, name+ext+".tmp-*")`: `CreateTemp` substitutes its random
digits for the trailing `*`, so the temp is `name.json.tmp-01234567`, which the
`ext` glob skips.

Six stores — `rolestore`, `modelstore`, `agentstore`, `toolstore`, `trigstore`,
`evalstore` — carry a comment calling this convention "load-bearing, not
cosmetic", and each has a test named for it: "a half-written X is never visible".

Probed at its widest point — all six — not one of those tests guarded the
convention. Each models it in the test instead of reading it from the write:
`rolestore`, `modelstore` and `evalstore` write a hand-built temp name
(`auditor.json.tmp-4711`, `x.json.tmp-1234`, `e1.json.tmp-12345`) and check the
listing skips it; `agentstore` and `toolstore` assert that a hand-rebuilt
`"backend"+ext+".tmp-123"` does not end in `ext`; `trigstore` re-types the whole
pattern into the test. All three shapes verify a string the test itself
constructs, decoupled from the `os.CreateTemp` call in the store's `write`.

So the mutation the comments explicitly warn against — moving `ext` after the
wildcard, `CreateTemp(dir, name+".tmp-*"+ext)`, which produces
`name.tmp-<n>.json`, an entry the `ext` glob *does* offer — leaves all six test
suites green. Measured, not supposed:

```text
mutate each store's write: name+ext+".tmp-*"  ->  name+".tmp-*"+ext
go test ./internal/<store>/   ->  ok   (all six)
```

`agentstore`'s test even says its suffix assertion "is the one that survives a
refactor". It does not survive the refactor it names, because it re-derives the
convention by hand rather than from the code it protects.

The behaviour is sound today; every store's real pattern still ends in the
wildcard. What was wrong is that the guarantee rested on six comments and six
tests that would pass over a broken store. This is the shape ADR-0035 recorded
one turn earlier and the shape ADR-0026 recorded before it: a claim verified by
a hand-maintained restatement of itself, which goes stale in exactly the case
nobody re-checks.

## Decision

**The convention is enforced by reading each store's own `os.CreateTemp` pattern
from source and holding it to the property, not by restating the pattern in a
test.** A single guard discovers every `internal/*store` package that declares
`const ext`, extracts every `os.CreateTemp(...)` argument list by balanced-paren
scan, and requires the pattern to end in a `"*"` wildcard literal — the
sufficient, auditable condition for "the temp name ends in random digits, so it
can never end in `ext`". An unrecognized form (ext after the wildcard, a
`fmt.Sprintf`, a pattern with no `*`) is refused rather than guessed safe, and
the scan fails closed if it finds no `CreateTemp` call at all.

The subject is derived for the reason ADR-0026 and ADR-0035 give: the convention
is duplicated across six packages, and a hand-listed or hand-rebuilt copy of it
in the test is one more thing to keep in step by hand — which is the failure, not
the fix. Reading the call the store actually makes ties the guard to the code it
guards, so a pattern "cleaned up" to give the temp its natural extension for
tooling fails the first time the suite runs.

`memorystore` declares `ext` but has no `CreateTemp`: it writes each version
under an `O_EXCL` content-addressed name and reseals every file on read
(ADR-0034), so content addressing — not the file name — is what makes a
half-written version unreadable. It contributes nothing to this guard, correctly.

## Consequences

The guarantee no longer rests on six decoupled tests. A store added later, or an
existing pattern reordered, is caught with a message naming the file, the
consequence (a truncated record offered to a reader at run start) and the remedy
(keep the wildcard last). The existing per-store tests are kept: they still
verify that the listing skips a `.tmp-` entry, which is the other half of the
contract; this guard adds the half they omitted — that the write produces such an
entry in the first place.

The check is exactly as strong as "the pattern ends in the wildcard", and no
stronger. It does not prove the rename is atomic or the fsync ordering correct —
those are separate properties with their own tests. It closes the one gap the
mutation exposed: a temp name that could collide with the extension the listing
selects on.

This ADR adds no production code. It records that a family-wide invariant six
comments called load-bearing was carried by tests that reconstructed it by hand,
and replaces that with a guard derived from the writes themselves.

## Discarded alternatives

**Fix each store's test to derive the temp name from its own write.** Six edits,
each re-establishing the same coupling in a slightly different local way, and
nothing stopping the seventh store from omitting it. The invariant is identical
across the family, so it is enforced once, over the family, from the source.

**Interrupt a real write mid-rename and assert the listing skips the temp.**
Closest to the runtime truth, but there is no portable hook between `CreateTemp`
and `Rename` to interrupt, and a timing-based attempt would be a flake, not a
guard. The pattern is a static property of the call, so it is checked statically.

**Trust the comments and the existing tests.** That is the state this ADR
corrects. A property called load-bearing in six places and pinned in none is
decoration, and a test that has only ever passed is indistinguishable from one
asserting nothing — which mutation proved this one to be.

## How it is verified

`internal/store_temp_atomicity_test.go`:

- `TestEveryStoreTempFileCannotEndInItsGlobbedExtension` — reads every
  `os.CreateTemp` pattern in every store declaring `const ext` and fails on any
  that does not end in the `"*"` wildcard. It proves its own detector against a
  known-bad fixture (the ext-last form) before trusting the corpus, and fails
  closed if it finds no `CreateTemp` call, the case where the write moved and the
  scan silently went empty.

Confirmed to fail closed by mutation: moving `ext` after the wildcard in a
store's write, and the no-star form, each fail the guard naming the offending
file — the mutation that left all six per-store suites green before this existed.
