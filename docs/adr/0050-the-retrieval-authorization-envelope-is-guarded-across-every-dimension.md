# ADR-0050: The retrieval authorization envelope is guarded across every dimension, because the receipt header's authorization sets are the field nothing fails on

- Status: accepted
- Affects: `internal/memorystore`
- Depends on: ADR-0027, ADR-0042, ADR-0043, ADR-0044, ADR-0047, ADR-0049
- Enables: Phase 7 (read-only governed memory)

## Context

Phase 7's exit evidence requires retrieval receipts to record "ranking/index
versions and reasons" and that "every influence identifies its source". ADR-0049
proved the per-record half of that: `Retrieval.Selection`, one entry per returned
record, is now witnessed across every dimension. The other half is the receipt
*header* — the `Retrieval` struct's own fields, which record the authorization
envelope the whole retrieval ran under: `authorized_scopes`, `clearance`,
`authorized_purposes`, `authorized_evidence_classes` and `as_of`. Five records
built that envelope field by field: ADR-0027 fixed the scope set as the one
boundary retrieval never crosses, ADR-0042 added the clearance ceiling, ADR-0043
the authorized purposes, ADR-0044 the accepted evidence classes, ADR-0047 the
as-of instant — and each justified its field in the same words: "recorded so an
audit can answer not only which records were presented but under what clearance,
for which use, on what kind of evidence, as of when each was authorized".

Almost none of that header is asserted. `sensitivity_test.go` checks `clearance`
in exactly one case — that an empty query resolves to the public floor — and
`validtime_test.go` checks `as_of` in one case. `authorized_scopes`,
`authorized_purposes` and `authorized_evidence_classes` are asserted nowhere. So
three of the five authorization dimensions in the audit envelope have zero
coverage, and a fourth is covered only at its floor.

Probing it confirmed the gap is live and confirmed its width. Dropping
`authorized_scopes`, `authorized_purposes` and `authorized_evidence_classes` to
`nil` leaves the whole `internal/memorystore` suite green; cross-wiring them to
each other's values — scopes reporting the class list, purposes reporting the
class list, classes reporting the purpose list — passes too. Forcing `clearance`
to the public floor regardless of the query passes, because the only assertion
covers the floor case a floored bug would satisfy. Only `as_of` is genuinely
defended: forcing it empty fails `validtime_test.go`. A `Retrieve` that recorded
the wrong scopes, purposes or classes in its audit envelope — or narrowed a
confidential retrieval's clearance to public in the receipt — would ship silently.

**Why this is a different gap from the one ADR-0049 closed.** ADR-0049 witnessed
each *returned record* — the per-selection fields describing what was presented.
This is the *query envelope* — one level up, describing the authorization the
retrieval ran under, produced once per retrieval rather than once per record. The
two are separate artifacts with separate mutation evidence, and the header's gap
is wider: ADR-0049 found the selection fields unasserted, and here three of five
envelope dimensions are unasserted while a fourth is covered only at its narrowest
point — the "verified once, in one place, and generalised to a whole capability"
shape `AGENTS.md` names outright. A guard scoped to just the three silent fields
would repeat it, leaving the floored-clearance defect live. The honest subject is
the whole envelope, so the guard is written against the whole envelope.

## Decision

**One guard pins the entire authorization envelope, under a query built so every
dimension is non-default and every set is multi-valued.**
`TestRetrievalEnvelopeWitnessesTheQueryItAuthorizedUnder` retrieves under a query
naming several scopes, a non-public clearance, several purposes and several
evidence classes — with a duplicate in each set — then asserts each header field
equals the authorized set derived from those same query values.

The query's shape is the point. A non-public clearance (`confidential`) catches a
header that always reports the public floor, the case the prior assertion could
not distinguish. Multi-valued purpose and evidence-class sets with *different*
contents catch a header that reports one where the other belongs. A duplicate in
each set catches a header that forgot to deduplicate, and sets given out of order
catch one that forgot to sort. The expected sets are derived from the query values
the retrieval saw, deduplicated and sorted the same way `Retrieve` builds them,
rather than restated as literals — the derive-the-subject-from-the-corpus
discipline `AGENTS.md` prescribes, so the property under test is that the envelope
describes the query it authorized, not that both match a hand-copied constant.

No production code changes. `Retrieve` already builds the envelope correctly, and
probing found no dimension it omits or mis-sources; this record closes a
verification gap in the manner of ADR-0049 — a test that fails the moment an
assumption is withdrawn, added where the previous coverage was absent.

## Consequences

The authorization envelope of the retrieval receipt is now verified at every
dimension it carries. A `Retrieve` that dropped a header set, filled it from the
wrong query field, stopped deduplicating or sorting it, or narrowed a non-public
clearance to the floor, fails a test that names the dimension and the audit
consequence it defeats, instead of passing silently. The five records that each
added an envelope field now have their claim — "recorded so an audit can answer
..." — proven rather than asserted, on the header as ADR-0049 proved it on the
selections.

No behavior changes. The receipt carried the same header before this record and
carries it after; what changed is that its content is now defended where only two
of its five dimensions were, one of those only at its floor.

## Discarded alternatives

**Leave it: the envelope is built correctly, so the missing assertions cost
nothing.** This is the argument that has lost every prior time it was made in this
corpus, and the mutation is the proof the cost is not zero. An envelope correct
today and unasserted is one refactor of `Retrieve` away from reporting the wrong
authorization set and still unasserted — and because the receipt is committed into
a prepared-context artifact and read by a tool rather than a person, the
regression surfaces as an audit that silently answers the wrong question, not as
an obvious break. The header mutations that pass the green suite today are the
deferred cost, handed to whoever next edits the envelope construction.

**Assert only the three silent fields, leaving clearance to its existing
coverage.** It would look like the minimal patch: `as_of` is defended and
`clearance` has an assertion, so add the three that have none. But the clearance
assertion covers only the empty-query floor, and the mutation shows a header that
always reports the floor passes it. Treating "has an assertion" as "is covered"
is the verified-at-its-narrowest-point error itself — a clearance dimension
witnessed only where a floored bug is invisible. So the guard asserts a non-public
clearance too, and the envelope is covered at its width, not at the one point that
already had a line.

**Trust the shared construction: all five fields are built in one struct literal,
so testing one tests the wiring.** The fields are adjacent in source but
independent in value — each is sourced from a different query input, sorted and
deduplicated separately — and the cross-wiring mutation proves it: swapping two
of them compiles, runs, and passes every test, because no assertion reads what the
literal actually put there. Adjacency in the source is not coverage of the values.

## How it is verified

`internal/memorystore/envelope_witness_test.go`:

- `TestRetrievalEnvelopeWitnessesTheQueryItAuthorizedUnder` — retrieves under a
  query naming several scopes, a non-public (`confidential`) clearance, several
  purposes and several evidence classes, each set carrying a duplicate, and
  asserts `authorized_scopes`, `clearance`, `authorized_purposes`,
  `authorized_evidence_classes` and `as_of` each equal the authorized set derived
  from the query, deduplicated and sorted the way `Retrieve` builds them.

Confirmed to fail closed by mutation: eight mutations of the envelope construction
— dropping each of the three set fields to `nil`, cross-wiring scopes, purposes
and classes to each other's values, forcing clearance to the public floor, and
forcing as-of empty — each fail the matching assertion, where before this record
five of the eight left the suite green; the baseline passes. Each message names
the dimension and the audit question the wrong witness defeats — a scope boundary
crossed with a receipt claiming it never was, an invisible widening of the
permitted uses or accepted evidence, a disclosure cleared too high with no trace,
or a stale-fact retrieval naming a different moment than the one asked about.
