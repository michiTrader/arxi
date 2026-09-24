# ADR-0046: A memory record carries a retention, which expires it rather than authorizing it

- Status: accepted
- Affects: `internal/memorystore`
- Depends on: ADR-0022, ADR-0027, ADR-0034, ADR-0042, ADR-0043, ADR-0044, ADR-0045
- Enables: Phase 7 (read-only governed memory)

## Context

ADR-0042 through ADR-0045 added four of Phase 7's item-7 record dimensions.
Sensitivity, purpose and evidence class each authorize a record before it is
ranked; confidence ranks the survivors without authorizing any of them. ADR-0043
drew the line the ranking side of the corpus now lives on: "confidence and
retention are record attributes that feed ranking and lifecycle rather than
authorization. Each remains future work and each must arrive the same way — with
the check that fails on it, and with the relation the dimension actually has."

Confidence crossed onto the ranking side. This record adds retention, and it is
the first dimension on the *lifecycle* side of that line — the third distinct
relation the store now holds. The four authorization dimensions decide whether a
record may be seen; confidence decides where a visible record ranks; retention
decides neither. It decides when a record stops being kept at all. So the question
this ADR must answer is again "what is the check that fails on it", and again the
answer is not a leakage test — no query withholds a record for its retention — but
this time it is not a reordering test either, because retention does not touch the
order. It is an *expiry* test: a retention nothing expires on is the
field-nothing-fails-on defect the roadmap has recorded, one lifecycle rank below
the confidence that nearly reached it.

Retention also has to answer a constraint the other five dimensions did not face:
this store reads no clock, and it holds no wall-clock creation instant to count a
duration from. That is the same constraint that still defers valid time, and it is
why retention here is not a duration but a lifecycle policy the store honors when
something else decides it is time to sweep. The store decides *what* may expire;
the caller decides *when*, and the clock that triggers it stays outside the store,
which is the pure-reducer discipline applied to lifecycle.

## Decision

**A record carries a `Retention` from a closed lifecycle vocabulary, and an
`Expire` sweep tombstones the records whose retention marks them expirable while
leaving the rest untouched. Retention never withholds a record from retrieval and
never changes its rank.**

The vocabulary is two levels, a closed set enumerated once as a map, the same
shape as the sensitivity and evidence-class vocabularies and for the reason
ADR-0022 gives — two lists drift, and a value valid in one place and unknown in
another leaks by omission rather than by decision. The map's value is not a rank
but a lifecycle predicate — whether an automated sweep may expire the record —
because retention has no order the way confidence does: permanent and ephemeral
are not more and less of one thing, they are different lifecycles. The two:

- `permanent` — kept until an explicit operator deletion; an automated `Expire`
  sweep never removes it. Material a user or operator deliberately committed to
  keep.
- `ephemeral` — an automated `Expire` sweep may tombstone it. Transient material —
  a proposed candidate, a low-value observation — that should not accumulate
  indefinitely.

**A closed vocabulary of two, not a `bool`.** Two members look like they could be
one boolean `Expirable`, and the difference is the whole point of this dimension.
A `bool` has no unstated state: its zero value is `false`, so a record whose writer
never made a lifecycle decision would read as "keep forever" with nothing to
distinguish the decision from its absence — the exact default the field is required
in order to refuse. A closed string vocabulary makes the unstated case a value
outside the set, so `Validate` can refuse it, and it digests as a stable string
that leaves room for a third policy to fail closed rather than be mistaken for one
of the two the domain already knows.

**Retention expires; it does not authorize and it does not rank, and that is the
load-bearing distinction.** Its only mechanism is the `Expire` sweep. A record's
retention never changes whether it is retrieved or where it orders: before a sweep,
an ephemeral record the caller is authorized for is returned in exactly the
position a permanent one would hold. So a `Query` carries no retention set — a
caller does not ask for "only durable records" the way it names the scopes,
purposes and classes it is authorized for — and this is a stronger version of the
reason confidence carries none: confidence at least changes the order, while
retention does not touch retrieval at all. A query-side retention filter would
withhold a record the caller is entitled to merely because it is expirable,
conflating "may be swept" with "may not be read". A caller that wants only durable
records is asking a lifecycle question the sweep answers, not a retrieval one the
query answers.

**Retention is required, not defaulted, and both defaults are dishonest in
opposite directions.** `Validate` refuses a record with no retention or an
unrecognized one, exactly as it refuses an empty `Sensitivity`, `Purpose`,
`EvidenceClass` or `Confidence`. Defaulting an unstated retention to `permanent`
looks safe — it never loses data — but it hoards a record a writer meant to be
transient, so material a user asked to be forgotten lives forever, which is the
"deletion propagates without resurrection" guarantee failing through lifecycle
instead of through a missed tombstone. Defaulting to `ephemeral` is worse: it
silently makes a durable record collectable, so the next sweep tombstones a record
nobody chose to expire. An unstated retention is not a lifecycle decision at all;
recording it as either policy fabricates a choice no one made, so the field is
required.

**Retention is part of the version identity.** It is added to the `identity`
struct that determines the content-addressed version ID, the deliberate act
ADR-0034 pins. It matters more here than for the dimensions before it: if retention
could be edited in place, a permanent version a receipt named could be flipped to
ephemeral and tombstoned by the next sweep, expiring a version somebody was told
would be kept. Making it identity forces a re-tiering to be a visible new version
that supersedes the old one, and the digest-immutability guard in `Versions` and
`verifyExisting` already refuses a file whose retention was altered after it was
written, because the altered field no longer digests to the version's own name; no
new integrity check is needed.

**The root verbs take a retention; the derived verbs carry it forward.** `Approve`
and `Propose` create a record and so must be told its lifecycle. `Correct`,
`Delete`, `Promote` and `Resolve` carry the tip's retention forward verbatim,
exactly as they already carry its scope, sensitivity, purpose, evidence class and
confidence, because the retention is a property set once at creation, not a decision
re-made on every correction. Carrying it forward is what stops a correction from
silently re-tiering a record by omission — and unlike a dropped confidence, which
would only misorder, a dropped retention would let the correction verb, rather than
the operator, decide the record's lifetime: a corrected permanent record that came
back ephemeral would be tombstoned by the next sweep.

**Expiry is a tombstone, never an unlink.** `Expire` removes an ephemeral record
the same way `Delete` removes an explicit one — by appending a tombstone that
supersedes the tip — not by unlinking its files. Unlinking would drop the record
from this copy of the store and from nothing else, so any replica or backup still
holding the bytes would resurrect it on the next read; that is the argument `Delete`
already makes, and it holds identically when the deletion is driven by a sweep
rather than an operator. An expired ephemeral record therefore inherits the whole
no-resurrection guarantee, and a receipt that named an earlier version still
resolves against the versions left on disk. A forked record is skipped rather than
expired: it has no single tip to supersede, is already withheld from retrieval, and
needs an operator's `Resolve` before any verb can act on it.

## Consequences

The store now carries six record dimensions with three distinct relations: four
that authorize before ranking (scope, sensitivity, purpose, evidence class), one
that ranks the survivors (confidence), and one that governs lifecycle (retention).
Retrieval is unchanged by the sixth — the same records come back in the same order
— because retention acts only through `Expire`, which is a write, not a read.

`Expire` is the first verb whose timing lives outside the store. It takes no clock
and no as-of instant, because the store reads no clock; it honors the policy on the
records as they stand and tombstones the expirable ones when called. Deciding when
to call it — at the end of a session, on a schedule — belongs to whatever drives the
store, and the clock that triggers it is that caller's, kept out of this package on
the same reasoning that keeps it out of the reducer.

This is the first of the two lifecycle/authorization attributes ADR-0043 and
ADR-0045 set aside. Valid time remains the last authorizing dimension, still left
for its own record because it is bitemporal and its as-of instant is a clock value
this store does not read — the same clock constraint retention met by being a
policy the caller times rather than a duration the store counts, which valid time
cannot do because an as-of query is a clock read by nature.

## Discarded alternatives

**Model retention as a duration or TTL counted from creation.** It reads as the
natural shape of "keep for ninety days", and this store cannot honor it. Counting a
duration needs a creation instant to count from, and this store holds none: it reads
no clock at write time, and minting one would break the content-addressed identity
ADR-0034 rests on, because two stores given the same write at different wall-clock
moments would stamp different creation instants and mint different version IDs. A
lifecycle class digests as a stable string, needs no clock, and moves the one clock
that lifecycle genuinely requires — *when to sweep* — to the caller that has one,
which is where every other clock in this system already lives.

**Put a retention floor in the query and filter by it.** It would let a caller say
"only show me durable records" and reuse the authorization seam. It is the category
error this ADR exists to prevent, and a starker version of the one ADR-0045 refused
for confidence: retention does not authorize *and does not rank*, so filtering by it
would withhold a record the caller is entitled to see merely because it is
expirable — conflating "may be swept" with "may not be read". The caller that wants
only durable records is asking the sweep a lifecycle question, not asking retrieval
an authorization one.

**Add the field and let a sweep read it "when it matters".** The minimal change: a
`Retention` field, no `Validate` requirement, no identity membership, and a sweep
that consults it if present. It is the field-nothing-fails-on defect in full —
nothing would fail if a writer omitted it, if a correction dropped it, or if no
sweep ever ran — so the attribute would rot into decoration and the first regression
would be silent. It is the same defect ADR-0042 through ADR-0045 refused for their
dimensions, one lifecycle rank down.

**Default an unstated retention to `permanent`.** It would spare every existing
writer the edit and looks conservative — never lose data. But an unstated retention
is the absence of a lifecycle decision, not a decision to keep forever, and
recording it as `permanent` hoards a record a writer meant to be transient: material
a user asked to be forgotten would live indefinitely, which is the deletion
guarantee failing through lifecycle. Defaulting to `ephemeral` is worse still — it
would let the next sweep tombstone a record nobody chose to expire. Neither default
is honest, so the field is required, and the churn of requiring it is the evidence
that it is load-bearing.

**Expire by unlinking the version files.** It is the obvious way to "remove" an
expired record and it is the one the deletion lineage already refused for explicit
deletion. Unlinking removes the record from this copy and from nothing else: a
replica, backup or synced directory still holding the bytes reintroduces it on the
next read, with nothing in the data saying it was expired. Expiry must be a
tombstone for the same reason `Delete` is — an assertion that travels with the data
— so that a store which receives it stops presenting the record even if it still
holds every earlier version.

## How it is verified

`internal/memorystore/retention_test.go`:

- `TestExpireTombstonesEphemeralAndKeepsPermanent` — an ephemeral and a permanent
  record share a scope; `Expire` reports and tombstones the ephemeral one and only
  it, and the permanent one is still retrieved afterward. This is the lifecycle
  mechanism, and the reordering-analogue witness: removing the permanent check
  expires a record a user committed to keep.
- `TestRetentionNeverWithholdsARecord` — a lone ephemeral record the caller is
  authorized for is returned and counted as authorized before any sweep, so
  retention is shown to act only through `Expire` and never to filter retrieval —
  the distinction from the authorization dimensions and from confidence, made a
  test.
- `TestAnUnstatedOrUnknownRetentionIsRefusedAtValidate` — an empty and a garbage
  retention each fail `Validate`, the empty one with a message naming the field and
  the remedy rather than a bare "invalid".
- `TestRetentionIsPartOfTheVersionIdentity` — two records identical but for their
  retention seal to different version IDs, so the field cannot leave the identity —
  and a re-tiering cannot become an in-place edit — without this test failing, the
  guarantee ADR-0034 pins applied here.
- `TestCorrectionCarriesRetentionForward` — correcting a record recorded as
  `permanent` yields a `permanent` tip, so a correction cannot re-tier a record by
  omission and hand its lifetime to the next sweep.
- `TestExpiredEphemeralRecordLeavesATombstoneNotAHole` — after `Expire`, the
  record's chain carries a `Deleted` tip in `Versions`, not merely an absence, so an
  unlink-based expiry that would pass a naive "is it gone" check fails here — the
  no-resurrection guarantee reached through a sweep.

Confirmed to fail closed by mutation: making `Expire` ignore the permanent guard
tombstones the permanent record; adding a retention filter to `Retrieve` withholds
the ephemeral record and makes the never-withholds test count zero authorized;
defaulting an empty retention in `Validate` admits it and drops the remedy from the
message; dropping `Retention` from the `identity` struct collapses the two version
IDs; dropping the carry-forward in `Correct` refuses the correction outright rather
than re-tiering it silently; and a sweep that writes no tombstone leaves the chain
with no `Deleted` tip. Each names the offending seam and the expiry or admission it
reopens.
