# ADR-0051: The retrieval receipt's provenance is guarded across schema and ranking version, because the evidence's own identity is the field nothing fails on

- Status: accepted
- Affects: `internal/memorystore`
- Depends on: ADR-0049, ADR-0050
- Enables: Phase 7 (read-only governed memory)

## Context

Phase 7's exit evidence requires retrieval receipts to record "ranking/index
versions and reasons". The `Retrieval` header carries two fields that are the
provenance of the receipt itself rather than of any record in it: `Schema`
(`schema`), the tag an audit tool parses the receipt by, and `RetrievalVersion`
(`retrieval_version`), the identity of the ranker that produced the result. The
const's own comment states the requirement outright — "the identity of the ranker
is part of the evidence, not a build detail" — so `RetrievalVersion` is the
"ranking version" the exit evidence names, made explicit.

ADR-0049 guarded the per-record `Selection` witness and ADR-0050 guarded the
authorization envelope (`authorized_scopes`, `clearance`, `authorized_purposes`,
`authorized_evidence_classes`, `as_of`). Those two records closed the content of
the receipt's records and the query it ran under. The provenance pair is what
remained: the receipt's statement of *what produced it*.

No test asserts that content. `store_test.go` checks only that
`RetrievalVersion` is non-empty; `Schema` on the `Retrieval` header is asserted
nowhere. The non-empty check is a shape check of exactly the kind ADR-0049
identified for `Reason` — it proves a string was written, never that the string
is true.

Probing confirmed the gap is live and confirmed its width. Blanking `Schema` in
the header leaves the whole `internal/memorystore` suite green — zero coverage.
Setting `RetrievalVersion` to a wrong non-empty value (`wrong.ranker/v0`) also
leaves the suite green, because the only assertion checks that it is not empty;
only forcing it to the empty string is caught, at `store_test.go:356`. So one
provenance field is unwitnessed entirely and the other is witnessed only at its
floor.

**Why this is a different gap from the ones ADR-0049 and ADR-0050 closed.**
ADR-0049 was the per-record selection witness; ADR-0050 was the authorization
envelope. This is neither — it is the receipt's own identity, the fields that say
which ranker and which receipt schema an audit is looking at, independent of any
record or any query dimension. A guard scoped to `Schema` alone, or one that kept
`RetrievalVersion` at its non-empty floor as before, would be a fresh instance of
the "verified once, in one place, and generalised to a whole capability" failure
this corpus keeps finding and `AGENTS.md` names outright: the mutation shows both
provenance fields are undefended at their content, so the honest subject is both.

## Decision

**One guard pins both provenance fields at their content, and it reads the
expected values off the package's own exported constants rather than from
hand-copied literals.** `TestRetrievalProvenanceWitnessesTheRankerAndSchemaThatProducedIt`
approves a record the query authorizes, retrieves it so a real receipt is
stamped, then asserts `evidence.RetrievalVersion == memorystore.RetrievalVersion`
and `evidence.Schema == memorystore.Schema`.

Deriving the expectation from the constants is the point, and the same
derive-the-subject-from-the-corpus discipline ADR-0049 applied to the `Selection`.
The property under test is that the receipt names the ranker and schema this build
actually is — not that either matches a string a later const bump would forget to
update. A const rename correctly changes both the production output and the
guard's expectation together; what the guard catches is a `Retrieve` that stamped
a value diverging from the const it should have carried: a blank schema, or a
ranker name that names a ranker that never ran.

No production code changes. `Retrieve` already stamps both fields from the
correct constants, and probing found no case where it omits or mis-sources them;
this record closes a verification gap in the manner of ADR-0049 and ADR-0050 — a
test that fails the moment an assumption is withdrawn, added where the previous
coverage was absent or floor-only.

**The guard's subject is the receipt's content, not its shape.** The prior
`RetrievalVersion != ""` check asserts a string was written, never that it names
the ranker that ran; the missing `Schema` check asserts nothing at all. The
distinction matters because the receipt is committed into a prepared-context
artifact and parsed later by whatever audits the presentation: a receipt tagged
with a blank or wrong schema is unparseable or read against the wrong shape, and
one naming the wrong ranker attributes the ranking to code that never produced
it. Both pass every shape check and fail the only reader that matters.

## Consequences

The provenance of the retrieval receipt is now verified at its content, so a
`Retrieve` that stopped stamping the schema, or named a ranker other than the one
that ran, fails a test that names the field and the audit consequence instead of
passing silently. With ADR-0049 (the selection witness) and ADR-0050 (the
authorization envelope), every content field of the `Retrieval` receipt — the
records it selected, the query it authorized, and the identity of the evidence
itself — is now defended where before only shape and counts were.

No behavior changes. The receipt carried the same schema and ranking version
before this record and carries them after; what changed is that both are now
verified against the constants they must reflect, where one was checked only for
non-emptiness and the other not at all.

## Discarded alternatives

**Leave it: the receipt is stamped correctly, so the missing assertions cost
nothing.** This is the argument that has lost every prior time it was made in this
corpus, and the mutation is the proof the cost is not zero: a blank `Schema` and a
wrong `RetrievalVersion` both ship green today. Because the receipt is read by a
tool and not a person, the regression surfaces as an audit that silently parses
the wrong shape or credits the wrong ranker, not as an obvious break.

**Keep `RetrievalVersion` at its non-empty floor and add only `Schema`.** It
would look like the smaller change and would be the same verified-at-its-narrowest
mistake one field over. The mutation shows a wrong non-empty ranker name passes
the floor check, so the ranker's identity is undefended at its content exactly as
`Schema` is; scoping the guard to `Schema` would generalise a green floor result
to a field the mutation proves is unguarded.

**Assert the fields against hand-copied literals rather than the constants.** It
would pin the same values today and drift the moment the ranker or schema const is
bumped, forcing a test edit that has nothing to do with the property under test
and inviting the stale-literal failure `AGENTS.md` warns against. Reading the
expectation off the constant makes the guard track the build's actual identity,
which is what an audit reads the field to learn.

## How it is verified

`internal/memorystore/provenance_witness_test.go`:

- `TestRetrievalProvenanceWitnessesTheRankerAndSchemaThatProducedIt` — approves a
  record the query authorizes, retrieves it, and asserts `evidence.RetrievalVersion`
  and `evidence.Schema` each equal the package constant the receipt must carry.

Confirmed to fail closed by mutation: blanking `Schema` in the header fails the
schema assertion, and setting `RetrievalVersion` to a wrong non-empty value fails
the ranking-version assertion, where before this record blanking `Schema` left the
suite green and only an empty `RetrievalVersion` was caught; the baseline passes.
Each message names the field and the audit question the missing witness defeats —
a receipt parsed against the wrong shape, or a ranking credited to a ranker that
never ran.
