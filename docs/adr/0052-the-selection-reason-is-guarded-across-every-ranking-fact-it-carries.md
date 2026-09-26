# ADR-0052: The selection reason is guarded across every ranking fact it carries, because the receipt's one free-text field named by the exit evidence was witnessed at a single substring

- Status: accepted
- Affects: `internal/memorystore`
- Depends on: ADR-0049, ADR-0050, ADR-0051
- Enables: Phase 7 (read-only governed memory)

## Context

Phase 7's exit evidence requires retrieval receipts to record "ranking/index
versions and reasons". ADR-0051 closed the *versions* half at the header. This
record closes the *reasons* half: the free-text `Reason` on every `Selection`,
the one receipt field the exit evidence names by that word.

`Retrieve` builds each reason to carry four facts at once:

```go
Reason: fmt.Sprintf("scope specificity %d (%s), confidence %s, no semantic ranking in %s",
    rank, r.Scope.Principal, r.Confidence, RetrievalVersion),
```

the scope-specificity rank the record ranked at, the principal it belongs to, the
confidence that broke the tie, and the ranker version that produced the ordering.
retrieve.go states the load-bearing claim outright — "The reason string on every
selection says exactly which rule applied, which is what the exit evidence asks
for" — so an audit reading it must be able to trust all four, not one.

ADR-0049 guarded the structured `Selection` fields and deliberately stopped at
them, recording that `confidence_test` "checks the free-text Reason string" and
`store_test` checks only that it is non-empty. That framing treated the reason as
already covered. It is not: it is covered at one of its four facts, and only as a
substring.

Probing confirmed the gap is live and confirmed its width. With `confidence high`
left intact so `confidence_test` still passes, three separate mutations of the
reason each left the whole `internal/memorystore` suite green:

- crediting the ordering to a ranker that never ran (`wrong.ranker/v0`);
- printing the wrong specificity number (`rank+999`);
- naming a principal the record does not belong to.

So three of the reason's four facts were the field nothing fails on, and the
fourth — confidence — was witnessed only as the substring `confidence_test` pins.
It is the last unwitnessed content on the retrieval receipt: ADR-0049 closed the
selection's structured fields, ADR-0050 the authorization envelope, ADR-0051 the
provenance header, and this the one free-text field they all left aside.

**Why this is a different gap from the ones ADR-0049 through ADR-0051 closed.**
Those three guarded machine-readable fields, each a single value asserted for
equality. The reason is a composite human-readable string that names four facts
in one line; a guard on any one of them — the confidence substring that already
exists — is the verified-at-its-narrowest-point mistake `AGENTS.md` names, one
field over. The mutation shows the other three facts are undefended, so the honest
subject is every fact the reason claims to carry, not the one that happened to be
checked.

## Decision

**One guard pins every fact the reason names, and it derives each expected value
from the returned record or the package's exported ranker constant rather than
from a hand-copied template.**
`TestRetrievalReasonNamesEveryRankingFactItClaimsToCarry` approves a record the
query authorizes, retrieves it so a real reason is built, then asserts the reason
names: the specificity rank read back from `rec.Scope.Principal.Specificity()`,
the principal `rec.Scope.Principal`, the confidence `rec.Confidence`, and the
ranker `memorystore.RetrievalVersion`.

The property under test is that the reason names the record it explains, not that
it matches a literal a reword could drift from — the same
derive-the-subject-from-the-corpus discipline ADR-0049 applied to the structured
fields, adapted to a composite string. So the guard checks that each derived value
is *present* in the reason rather than pinning the exact format template: rewording
the connective prose leaves it green, while corrupting any of the four values fails
it. Two of the checks carry a delimiter for the same reason ADR-0050's set checks
deduplicate — to defeat a false pass. The specificity fragment carries a trailing
space so a wrong rank sharing a leading digit (`1` versus `1000`) cannot pass by
prefix, and the principal carries its parentheses so a wrong principal cannot pass
by a substring appearing elsewhere in the line.

No production code changes. `Retrieve` already builds the reason from the correct
record fields and constant, and probing found no case where it mis-sources them;
this record closes a verification gap in the manner of ADR-0049 through ADR-0051 —
a test that fails the moment an assumption is withdrawn, added where the previous
coverage was a single substring.

**The guard's subject is the reason's content, not its shape.** The prior
`Reason != ""` check in `store_test` asserts a string was written; the
`confidence high` substring in `confidence_test` asserts one of its four facts is
present. Neither asserts the reason names the rank, the principal or the ranker
that actually produced the ordering. The distinction matters because the reason is
committed into a prepared-context artifact and read later by whoever audits *why*
a record was placed where it was: a reason crediting the ordering to a ranker that
never ran, or naming a rank the record did not hold, reads as a coherent
explanation of a ranking that never happened. It passes every shape check and
misleads the only reader that matters.

## Consequences

The free-text reason on every selection is now verified at its content, so a
`Retrieve` that named the wrong specificity, the wrong principal, or a ranker other
than the one that ran fails a test that names the fact and the audit consequence
instead of passing silently. With ADR-0049 (the structured selection fields),
ADR-0050 (the authorization envelope) and ADR-0051 (the provenance header), every
field of the `Retrieval` receipt — structured and free-text alike — is now
defended where before the reason's content was witnessed only at a single
substring.

No behavior changes. The reason carried the same four facts before this record and
carries them after; what changed is that all four are now verified against the
record and constant they must reflect, where three were unwitnessed and the fourth
checked only as a substring.

## Discarded alternatives

**Leave it: `confidence_test` already checks the reason, so the missing assertions
cost nothing.** This is the argument that has lost every prior time it was made in
this corpus, and the mutation is the proof the cost is not zero: a reason crediting
the ordering to a ranker that never ran, naming the wrong specificity, or naming
the wrong principal all ship green today. Because the reason is read to reconstruct
*why* a record ranked where it did, the regression surfaces as an audit that
believes a coherent but false explanation, not as an obvious break.

**Add only the ranker version, since ADR-0051 just made it the topic.** It would
look like the smaller change and would be the same verified-at-its-narrowest
mistake the reason already suffers from — three facts checked out of four instead
of one out of four. The mutation shows the specificity and the principal are
undefended in the reason exactly as the ranker version is; scoping the guard to the
ranker would generalise a green result to facts the mutation proves are unguarded.

**Assert the whole reason against an exact expected string.** It would catch all
three mutations, but by re-implementing the format template in the test it would
turn a benign reword of the connective prose into a failure, making the guard a
change-detector for wording rather than a witness that the facts are named. Checking
that each derived value is present keeps the property — the reason names the record
it explains — independent of how the sentence is phrased.

## How it is verified

`internal/memorystore/reason_witness_test.go`:

- `TestRetrievalReasonNamesEveryRankingFactItClaimsToCarry` — approves a record the
  query authorizes, retrieves it, and asserts the single selection's reason names
  the specificity rank, the principal, the confidence and the ranker version, each
  derived from the returned record or the exported `RetrievalVersion` constant.

Confirmed to fail closed by mutation: with `confidence high` left intact so the
prior coverage still passes, crediting the ordering to a ranker that never ran
fails the ranker-version assertion, printing the wrong specificity number fails the
specificity assertion, and naming the wrong principal fails the principal
assertion — where before this record all three left the suite green; the baseline
passes. Each message names the fact and the audit question the missing witness
defeats — a ranking credited to a ranker that never ran, a rank the record did not
hold, or a holder the record does not belong to.
