# ADR-0048: Valid time's carry-forward is guarded at every derived verb, because it is the one dimension Validate does not cover

- Status: accepted
- Affects: `internal/memorystore`
- Depends on: ADR-0034, ADR-0046, ADR-0047
- Enables: Phase 7 (read-only governed memory)

## Context

ADR-0047 added valid time and stated, in its Decision, that **the derived verbs
carry the window forward**: "`Correct`, `Delete`, `Promote` and `Resolve` carry
the tip's window forward verbatim, exactly as they already carry its scope,
sensitivity, purpose, evidence class, confidence and retention." The code does
exactly that — all four verbs copy `tip.Validity` (or `keep.Validity`) into the
version they append. But the record's verification section lists a single
carry-forward guard, `TestCorrectionCarriesValidityForward`, and that test
exercises `Correct` alone. The property is asserted for four verbs and proven for
one.

That is the exact failure shape this repository keeps finding and `AGENTS.md`
names outright: "a claim verified once, in one place, and then generalised to a
whole capability." The claim "every derived verb carries validity forward" was
generalised from the one verb a test covers to the three it does not.

Probing it confirmed the gap is live, not cosmetic. Removing `Validity:
tip.Validity` from `Delete`, from `Promote`, or `Validity: keep.Validity` from
`Resolve` — each in isolation — leaves the whole `internal/memorystore` suite
green. A mutation that silently re-dates a record through three of its four
derived verbs passes every check.

**Why valid time alone is exposed, when the same three verbs carry six other
dimensions the same way.** The other identity dimensions have an incidental
guard that valid time does not: they are *required*. Dropping the carry-forward
of scope, sensitivity, purpose, evidence class, confidence or retention leaves
that field at its zero value, and `Record.Validate` refuses a record whose
sensitivity, purpose, evidence class, confidence or retention is empty — so the
verb's own write fails, and any test that drives the verb fails with it. The
mutation is caught, but by `Validate`'s required-field check rather than by a
carry-forward test: the coverage is real for those dimensions and accidental in
its source. Valid time is the single identity dimension whose zero value is a
*legal* value. `Validity{}` is the timeless record, which ADR-0047 deliberately
admits as an honest always-valid assertion. So a dropped valid-time
carry-forward produces a well-formed timeless record that `Validate` accepts, and
the incidental cover that protects the other six dimensions is absent for exactly
this one. The dimension ADR-0047 made optional on purpose is the dimension whose
carry-forward nothing guards.

## Decision

**Each derived verb that carries valid time forward gets its own guard, so the
carry-forward is proven where it happens rather than generalised from `Correct`.**
`TestPromotionCarriesValidityForward`, `TestDeletionCarriesValidityForward` and
`TestResolutionCarriesValidityForward` join the existing
`TestCorrectionCarriesValidityForward`, one per verb that appends a version from a
tip. No production code changes: the four verbs already carry the window forward
correctly, and probing found no verb that drops it. This record closes a
verification gap, in the manner of ADR-0034 — which added no runtime guard and
instead pinned an assumption with a test that fails the moment the assumption is
withdrawn.

**The guard shape follows the consequence, and the consequence differs by verb.**
`Promote` and `Resolve` append a *live, retrievable* record, so a dropped window
is a disclosure with the same shape as the one ADR-0047's headline leakage test
prevents: a fact true only in a past interval becomes timeless — valid at every
as-of — and is presented as current. Each of these two guards therefore asserts
through retrieval at an as-of the original window excludes, exactly as
`TestCorrectionCarriesValidityForward` does, so the test fails the way a real leak
would surface rather than only on a struct-field comparison. `Delete` appends a
*tombstone*, which retrieval never returns, so its dropped window is not a
disclosure but a lineage loss: a tombstone carries the tip's classification
forward precisely so an audit reading the deletion can still see what the record
was when it was removed — the reason `Record.Validate` already requires every
other dimension on a tombstone. Its guard reads the window off the returned
tombstone version directly, because there is no retrieval to read it through.

**The reason valid time needs the explicit guard is recorded, not just the
guard.** The load-bearing fact is that valid time has no incidental `Validate`
cover because its empty value is legal, and that fact is stated in this record and
in the test comments. Without it, a later reader has two ways to reintroduce the
hole: believe the other dimensions share it (they do not, and adding six more
carry-forward tests to chase a gap that only exists for one would be cargo-culting
the fix), or make some other dimension optional the way valid time is optional and
inherit the same silent exposure without noticing. Naming the mechanism is what
turns "valid time happened to be untested here" into "valid time is the case that
must be tested here, and here is why the others need not be."

## Consequences

The carry-forward of valid time is now verified at all four verbs that perform it,
so a mutation dropping it from any one of them fails a test that names the verb and
the disclosure it reopens, instead of passing silently. ADR-0047's Decision and its
verification section now agree: the four-verb claim has four guards.

No behavior changes. The store carried valid time forward correctly before this
record and carries it forward correctly after; what changed is that the property is
now defended where the previous coverage was accidental. The other six dimensions
remain guarded by `Validate`'s required-field rejection, and this record documents
that as the reason they do not each need a matching per-verb carry-forward test —
so the asymmetry is a recorded decision rather than an omission a future pass
"corrects" by adding eighteen tests that assert what `Validate` already enforces.

## Discarded alternatives

**Leave it: the code is correct, so the missing tests cost nothing.** This is the
argument that lost every prior time it was made in this corpus. A carry-forward
that is correct today and untested is one refactor away from being incorrect and
still untested — and the whole point of the test suite here is that Go gives no
exhaustiveness or immutability guarantee, so a `Put` that quietly stops passing one
field compiles and ships. The three mutations that pass the green suite today are
the proof the cost is not zero; it is deferred to whoever next edits `Delete`,
`Promote` or `Resolve`.

**Add per-verb carry-forward tests for all six dimensions, not just valid time.**
It would make the coverage uniform and stop relying on `Validate`'s rejection as an
incidental guard. It chases a gap that does not exist for those dimensions: their
carry-forward *is* caught, by `Validate`, and a test asserting what `Validate`
already enforces on every write is the decoration `AGENTS.md` warns against — a
check that has only ever passed and cannot distinguish a real regression from the
`Validate` failure that would fire first. The honest scope is the dimension that is
actually unguarded, with the asymmetry explained rather than papered over. If a
future dimension is made optional the way valid time is, this record is the place
that says it then needs the same explicit guard.

**Make `Validity{}` illegal so valid time gets the same incidental cover as the
others.** It would collapse the asymmetry at the source: require both bounds, and a
dropped carry-forward would leave an empty-and-now-illegal window that `Validate`
rejects, exactly as it rejects an empty sensitivity. It reverses ADR-0047's central
judgment to buy a test guarantee. An empty bound is a stated claim — "no known end"
— not the unknown an empty sensitivity is, and forcing a `To` onto a standing
preference fabricates an expiry nobody knows. Weakening a correct domain decision to
make a missing test unnecessary is the "correct the test, not the code" inversion
turned inside out: here it would be corrupting the *model* to avoid writing the
test. The test is cheaper and truthful.

## How it is verified

`internal/memorystore/validtime_test.go`:

- `TestCorrectionCarriesValidityForward` — unchanged; `Correct` keeps the window,
  checked through the tip and an as-of the original window excludes.
- `TestPromotionCarriesValidityForward` — a candidate approved with a bounded
  window is promoted, and the promoted (now retrievable) record is withheld at an
  as-of outside that window; dropping `Validity` from `Promote` presents a
  past-only fact as current.
- `TestDeletionCarriesValidityForward` — a deleted record's tombstone carries the
  window forward, read off the returned tombstone version, so an audit can still
  see when the record was valid at the moment it was removed; dropping `Validity`
  from `Delete` makes the tombstone timeless and loses that lineage.
- `TestResolutionCarriesValidityForward` — resolving a fork toward a survivor with
  a bounded window keeps that window on the resolved (retrievable) record, checked
  through an as-of the window excludes; dropping `Validity` from `Resolve` re-dates
  the survivor to always-valid.

Confirmed to fail closed by mutation: removing `Validity: tip.Validity` from
`Delete`, removing it from `Promote`, and removing `Validity: keep.Validity` from
`Resolve` each fail the matching new guard, where before this record all three left
the suite green. Each message names the verb and states whether the reopened harm
is a stale-fact disclosure (Promote, Resolve) or a lineage loss on the tombstone
(Delete).
