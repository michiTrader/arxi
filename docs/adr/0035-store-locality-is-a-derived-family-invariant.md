# ADR-0035: A store's project-local locality is a derived family invariant, not a per-store claim

- Status: accepted
- Affects: `internal/memorystore`, `internal/store_locality_test.go`
- Depends on: ADR-0026, ADR-0027

## Context

Every content store keeps its data beside `runs/`, relative to the working
directory: `memory`, `roles`, `triggers`, `providers`, `agents`, `evals`,
`policies`. The reason is a leak, not a preference. A store rooted in `$HOME`
would follow the user between repositories, so memory, roles or triggers written
while working on one project would silently shape an agent working on the next.
Cross-run recall is the feature; cross-repository recall is a leak nobody asked
for.

`memorystore/store.go` stated that reason and attributed it to the neighbours:
the store sits beside `runs/` "for the reason rolestore, trigstore, modelstore
and agentstore all give". Probed at its widest point — the four packages the
sentence names — the attribution was false. Only `trigstore` argues the
trade-off, in a comment on its own `DefaultDir`. `rolestore`, `modelstore` and
`agentstore` state the location in a single line and make no argument at all. And
only `trigstore` had a test pinning the property (`TestTheDefaultDirectoryIs`
`ProjectLocal`), for `triggers` alone.

So a guarantee asserted for four packages was argued in one and enforced in one.
That is the shape this corpus keeps finding in itself: the memory channel that
was true in one assembler and false in the two others ADR-0020 named (ADR-0025),
the record count that was right for a subset and generalised to the whole
(ADR-0026), the blocker verified at its narrowest point and reported for the
whole capability. A claim checked in one place and generalised to many is the
recurring defect, not the exception.

The behaviour was in fact sound: no store reaches `$HOME`, and every declared
default is a bare relative name. Nothing was mis-rooted. What was wrong was the
prose — a cross-reference to three packages that never made the argument — and
the fact that the property rested on one hand-written test for one store, so a
regression in any other store would have been silent.

## Decision

**The project-local locality of a store is enforced as a family invariant
derived from the tree, not restated in each store's comment.** A single test
discovers every `internal/*store` package by globbing, and holds two properties
over whatever it finds:

- every `DefaultDir` it can read is project-local — not absolute, not
  `$HOME`-anchored, not a `..` escape; and
- no store's source reaches the home directory (`UserHomeDir`, `UserConfigDir`,
  `UserCacheDir`, `Getenv("HOME")`, an `XDG_*` variable) to decide where its
  data lives — which also covers `jobstore` and `logstore`, the two that take
  their directory from the caller and so declare no default to inspect.

`memorystore`'s comment is corrected to state the reason itself, name the one
sibling that shares it, and point at that test rather than at three packages
that never earned the citation.

The subject is derived rather than listed for the reason ADR-0026 gives and this
turn re-proves: a hand-written list of stores goes stale in exactly the store
nobody remembers to add, and a store missing from the list is a store the leak
guard never sees. The glob covers whatever the tree contains, including a store
added after the test was written.

## Consequences

The guarantee no longer depends on three comments that never made the argument,
nor on one test covering one store. A newly added store with an absolute or
home-anchored default fails the suite the first time it runs, with a message
naming the file and the leak it would open. `trigstore`'s own test is kept: it
additionally pins the exact value `"triggers"` and carries the store-specific
reasoning, which the family guard does not check.

The invariant is exactly as strong as "no store roots itself in `$HOME`", and no
stronger. It does not judge whether a store's relative name is the *right* one,
only that it is relative — the same restraint the roadmap guards keep, which
check that a claim is present and consistent, not that it is wise.

This ADR adds no production code. It records that a family-wide property was
being carried by a false cross-reference and one narrow test, and replaces both
with a derived guard, so the safety is enforced where it was previously only
asserted.

## Discarded alternatives

**Paste the reasoning into rolestore, modelstore and agentstore so the original
sentence becomes true.** Four copies of one argument is four places for it to
drift, and this project already treats a hand-kept count or list as a defect
waiting to happen — "two copies of a check list is how the list drifts". The
argument lives once, and a test enforces the property everywhere.

**Correct the comment and stop.** That fixes the false sentence but leaves the
property resting on `trigstore`'s lone test. The next store added, or an existing
one "simplified" to read a home directory, would reopen the leak with nothing
failing. The prose was the visible defect; the missing family guard was the one
that would have cost working code.

**Enumerate the store packages in the test by hand.** That repeats the exact
mistake one layer down: the list is right when written and stale the first time a
store is added without editing it. Deriving the set from the tree is the
correction ADR-0026 already made for the ADR-to-phase pairs, and the reason it
caught its own record on the first run.

## How it is verified

`internal/store_locality_test.go`:

- `TestEveryStoreDefaultDirectoryIsProjectLocal` — reads every `DefaultDir`
  declared under `internal/*store` and fails on any that is absolute,
  home-anchored or escapes the working directory. Fails closed if it finds no
  store package or no default at all, the case where the literal format drifted
  and the check silently stopped seeing any store.
- `TestNoStoreRootsItselfInTheHomeDirectory` — fails if any store's source
  reaches `$HOME` to place its data, covering the two stores that declare no
  default. The home detector is proven against a known-bad fixture before the
  corpus is trusted, so a regex that stopped matching cannot make a negative
  assertion pass vacuously.

Both were confirmed to fail closed by mutation: giving a store an absolute
`DefaultDir` fails the first test naming that store, and injecting a
`UserHomeDir` call fails the second naming that file.
