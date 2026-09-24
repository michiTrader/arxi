# ADR-0047: A memory record carries a valid-time interval, authorized against a query as-of instant the caller supplies

- Status: accepted
- Affects: `internal/memorystore`
- Depends on: ADR-0022, ADR-0027, ADR-0034, ADR-0042, ADR-0043, ADR-0044, ADR-0045, ADR-0046
- Enables: Phase 7 (read-only governed memory)

## Context

ADR-0042 through ADR-0046 added five of Phase 7 item 7's record dimensions —
sensitivity, purpose, evidence class (all authorizing), confidence (ranking) and
retention (lifecycle) — each with the check that fails on it and the relation the
dimension actually has. One authorizing dimension of that item is still missing,
and the roadmap has named it last from the start: valid time, the period during
which the fact a record asserts is true in the modeled world.

Valid time was left for its own record because it is the one dimension with a
clock in it. ADR-0046 settled the near version of that problem for retention by
making retention a policy the caller times rather than a duration the store
counts, so `Expire` takes no as-of instant. Valid time cannot dodge the clock the
same way, because an as-of query is a clock read *by nature*: to ask "which
records are true now" something must know what "now" is. The store must not — it
reads no clock, and ADR-0042 through ADR-0046 all rest on retrieval being a pure
function of its inputs. So the question this record answers is: how does a
clock-free store authorize on time at all.

The answer is the same move the reducer makes for every other clock value in this
project. The reducer never calls `time.Now()`; the clock arrives as an event. Here
the as-of instant arrives as a query parameter. The caller — which does have a
clock — reads it once and passes the instant in, and the store compares each
record's interval against that supplied instant without ever acquiring one of its
own. The clock read that the query needs by nature happens in the one place
allowed to have a clock, and the store stays pure. This is why valid time waited
until after retention: retention taught the store to treat timing as an input
rather than a capability, and valid time is that lesson applied to an authorizing
dimension instead of a lifecycle one.

Valid time is also the store's *second* time axis, and naming the first is what
makes the second legible. The version chain already records transaction time —
when each version was written and when a correction superseded it — which answers
"what did the store believe, and since when". It cannot answer "when is the
asserted fact true in the world": a headcount true for all of 2025 can be recorded
in 2026 and corrected in 2027 without changing when it was true. Carrying both
axes is what makes the store bitemporal, the shape Phase 7 item 7 named with the
word "bitemporal" and the last piece of it to land.

## Decision

**A record carries a `Validity` — a half-open interval `[From, To)` in valid time
— and retrieval withholds a record whose interval does not contain the query's
`AsOf` instant, in the same pre-ranking step that filters by scope, clearance,
purpose and evidence class. The store never reads a clock: the as-of instant is
supplied by the caller.**

An instant is a canonical RFC 3339 timestamp in UTC at seconds precision
(`2006-01-02T15:04:05Z`). The empty instant is the unbounded end of an interval,
not a zero time: an empty `From` means "true since before any instant this store
records", an empty `To` means "true with no known end". The interval is half-open
so adjacent windows tile without overlap or gap — a fact true for 2025 is
`[2025-01-01T00:00:00Z, 2026-01-01T00:00:00Z)` and the fact that replaces it may
begin exactly at that upper bound with no instant belonging to both windows or to
neither.

**Valid time authorizes; it does not rank, and its check is a leakage test.**
Unlike confidence and retention, which are on the ranking and lifecycle sides of
ADR-0043's line, valid time decides whether a record may be seen — a fact true
only in the past is withheld from a caller asking about the present, exactly as a
record outside the caller's scope is. So its witness is the authorization seam's:
a record outside the as-of window is counted in `Considered` and not in
`Authorized`, the same contained-refusal measurement a cross-scope record leaves,
one dimension over. Remove the filter and a fact true only in 2025 is presented as
current in 2026.

**The as-of instant lives on the query, and the store reads no clock to get it.**
`Query` gains an `AsOf Instant`, supplied by the caller. This is the load-bearing
line of the whole record: the clock read an as-of query needs by nature happens in
the caller, and the store only compares canonical strings, which sort
chronologically because every stored and queried instant is forced into the one
fixed-width UTC spelling. An empty as-of is the **timeless floor**, not a wildcard:
a caller naming no as-of asks no temporal question, so it receives only records
that declared no validity window and is withheld from any record carrying one,
because it supplied no instant to evaluate that record against. This mirrors the
clearance floor (empty clearance is the public floor) rather than the purpose
empty-set (which authorizes nothing): as-of is a point with a natural floor — a
timeless record needs no as-of, as public material needs no clearance — where
purpose is a set with no floor member.

**Both bounds are optional, and an empty bound is a claim, not an omission.** This
is the one place valid time departs from the vocabulary dimensions, and the
departure is deliberate. For sensitivity, purpose, evidence class, confidence and
retention, an empty value is an *unknown* to be refused, because defaulting an
unknown is fail-open. An empty bound here is not unknown: an empty `To` is the
stated claim "true with no known end", the honest shape of a standing preference.
Requiring a `To` would force a writer to invent an expiry nobody knows — the exact
fabrication ADR-0045 and ADR-0046 refuse in confidence and retention, here in
time. So a record may be timeless (both bounds empty), which the as-of filter reads
as always-valid, and `Validate` refuses only a bound that is not a canonical
instant or an interval whose `From` is not strictly before its `To`.

**The check that fails on valid time is the as-of filter, not a required field.**
An always-valid record is a legitimate assertion, so "a bound is present" cannot be
the requirement without refusing honest timeless records. The guard that keeps
valid time from being decoration is instead the filter in `Retrieve`: it fails
closed the moment a record *has* a bound, because removing it leaks a fact outside
its window. An empty *interval* — `From` at or after `To` — is refused at
`Validate`, because it contains no instant and the record could never be retrieved
for any as-of: the interval-shaped version of the field-nothing-fails-on defect,
dead weight nothing would ever fail on, caught at the write.

**Valid time is part of the version identity.** `Validity` is added to the
`identity` struct that determines the content-addressed version ID, the deliberate
act ADR-0034 pins. Re-dating when a fact is true is a new version a receipt can
name, never an in-place edit — and this matters as much here as it did for
retention. If a window could be edited in place, a record a receipt named as true
through 2027 could be silently narrowed to 2026, so a retrieval that correctly
withheld it at a 2027 as-of would start returning it with no version to show the
window had moved. Making it identity forces a re-dating to be a visible new
version. The digest-immutability guard in `Versions` and `verifyExisting` already
refuses a file whose window was altered after it was written, because the altered
field no longer digests to the version's own name; no new integrity check is
needed.

**The root verbs take a validity; the derived verbs carry it forward.** `Approve`
and `Propose` create a record and so must be told when it is true. `Correct`,
`Delete`, `Promote` and `Resolve` carry the tip's window forward verbatim, exactly
as they already carry its scope, sensitivity, purpose, evidence class, confidence
and retention, because the window is a property of the record set once at creation,
not a decision re-made on every correction. Carrying it forward is what stops a
correction from silently re-dating a record by omission — and unlike dropping an
authorization dimension the record still discloses, dropping the window would move
*when* the fact is true, so a later as-of query returns or withholds it against a
window nobody set.

**A validity is a pair, so it is passed as one value.** `Approve` and `Propose`
take a `Validity{From, To}` rather than two adjacent instant arguments. Two
same-typed string parameters next to each other are the shape a caller silently
swaps; a struct with named fields makes the swap a compile error rather than an
inverted interval discovered at retrieval.

## Consequences

The store now carries six record dimensions: five that authorize before ranking
(scope, sensitivity, purpose, evidence class, valid time) and one that ranks the
survivors (confidence), with retention deciding lifecycle beside them. Retrieval's
pre-ranking filter gains a fifth clause, checked against an instant the query
carries rather than one the store reads. The ranker ADR-0027 kept non-semantic is
untouched: valid time withholds, it does not reorder.

The retrieval evidence records the as-of the retrieval was authorized against and
each selected record's window, so an audit can see that a record was returned
because its window contained the as-of and not in spite of it — the "every
influence identifies its source" requirement widened to the moment the influence
was current.

This is the last of Phase 7 item 7's record dimensions. With it the store is
bitemporal: transaction time is the version chain, valid time is this interval, and
the two are independent. What remains of Phase 7 is not another dimension but the
wiring — retrieval is still not called from `internal/exec`, because which
principals a run is authorized for is an identity question the roadmap assigns to
Asha, and inventing a principal from whatever the run happens to know is the class
of guess ADR-0022 exists to stop.

## Discarded alternatives

**Read the clock in the store and default the as-of to "now".** It would spare
every caller the parameter and read the way a user expects — "what is true now" with
no argument. It is the one thing this store must never do. A `time.Now()` inside
retrieval makes the same query return different records on two runs, so a replay
disagrees with the original fold and a receipt naming a version becomes
unreproducible — the exact property ADR-0013 and the kernel's purity guard rest on.
The as-of is the caller's clock read, passed in, precisely so the store stays a
pure function of its inputs.

**Model the interval as `time.Time` bounds.** It reads as the natural Go type and
composes with real date arithmetic, and it breaks the content-addressed identity
for the same reason ADR-0045 rejected a float confidence: a `time.Time` has no
canonical encoding. The same instant marshals as `...Z` or `...+00:00`, with or
without a monotonic reading, in one location or another, so two stores given the
same write could mint different version IDs and a receipt would stop being portable
between them. A canonical UTC string digests to stable bytes, sorts
chronologically under a plain comparison, and is refused at `Validate` when it is
not a real instant instead of admitting a zero time that silently means "the
beginning of 1 CE".

**Require both bounds, refusing an empty one as the other dimensions refuse an
empty value.** It would make valid time uniform with sensitivity, purpose and the
rest — every field required, nothing skippable. It is dishonest here in a way it is
not there. An empty bound is not an unknown the way an empty sensitivity is; it is
the stated claim "no known start" or "no known end". Forcing a writer to supply a
`To` for a standing preference makes them invent an expiry nobody knows, fabricating
a fact exactly as defaulting an unstated confidence to `low` would — the churn is
not evidence the field is load-bearing, it is evidence the requirement is wrong. The
guard that keeps valid time honest is the as-of filter, which fails on a *bounded*
record, not a rule that every record be bounded.

**Put a valid-time range on the query and return everything overlapping it.** It
would let a caller ask "everything true at any point in 2026" in one call. It
answers a different question than the one authorization asks. Authorization is "may
this caller see this record *now-as-the-caller-defines-now*", a single instant; a
range asks "what was ever true across a span", which is a reporting query a caller
builds from instants, not an authorization the store owes. Worse, an overlapping-range
filter would admit a record true for one day of a year-long query range as readily
as one true throughout it, conflating "was true at some point you asked about" with
"is true at the point you are asking about" — the stale-fact disclosure the single
as-of exists to prevent.

**Add the field and let the ranker or filter read it "when an as-of is present".**
The minimal change: a `Validity` field, no `Validate` requirement, and a filter that
runs only when the query happens to carry an as-of. It is the field-nothing-fails-on
defect in full, the one the roadmap warns each of these dimensions invites. A query
that forgot the as-of would silently see every record regardless of window, so the
first caller wired without an as-of would leak stale facts with nothing failing. The
timeless floor closes exactly this: an absent as-of is a defined, fail-closed value
(timeless-only), not an off switch for the filter.

## How it is verified

`internal/memorystore/validtime_test.go`:

- `TestValidTimeWithholdsARecordOutsideTheAsOfWindow` — two records true in adjacent
  years are both considered, and only the one whose window contains the as-of is
  authorized; the evidence records the as-of. The authorization witness, the same
  shape as the cross-scope leakage test one dimension over.
- `TestTheAsOfBoundaryIsHalfOpen` — a record is returned at exactly its `From`
  instant and withheld at exactly its `To` instant, pinning `[From, To)` at both
  edges so a closed-interval refactor fails here rather than presenting a fact one
  instant after it stopped being true.
- `TestATimelessRecordIsValidAtEveryAsOf` — a record with neither bound is returned
  at a concrete as-of and at the empty one, proving an empty bound is the unbounded
  end of the interval rather than an edge no instant clears.
- `TestAnEmptyAsOfSeesOnlyTimelessRecords` — a query naming no as-of returns the
  timeless record and withholds the bounded one, counting the bounded one as
  considered-not-authorized, so the floor is fail-closed and measurable rather than
  a wildcard.
- `TestAMisspelledQueryAsOfIsRefusedNotNarrowed` — a non-canonical as-of is refused
  with a message naming the expected form, not silently narrowed to the timeless
  floor, the closed-form rule the record's own instants obey applied to the query.
- `TestAnUnparseableOrInvertedValidityIsRefusedAtValidate` — an un-parseable bound, a
  non-UTC bound and an inverted interval each fail `Validate` with a message naming
  the field and the remedy; a timeless record is not refused.
- `TestValidTimeIsPartOfTheVersionIdentity` — two records identical but for their
  window seal to different version IDs, so the window cannot leave the identity
  without the test failing, the guarantee ADR-0034 pins applied here.
- `TestCorrectionCarriesValidityForward` — correcting a record keeps its window, so a
  correction cannot re-date a record by omission — checked through both the tip and
  an as-of the original window excludes.

Confirmed to fail closed by mutation: making the as-of filter always admit makes the
window and empty-floor tests leak the out-of-window record; dropping `Validity` from
the `identity` struct collapses the two version IDs in the identity test; dropping
the carry-forward in `Correct` re-dates the tip; making the `To` bound inclusive
fails the boundary test; dropping the query's as-of validation admits the misspelled
instant; and letting an empty as-of admit everything fails the timeless-floor test.
Each names the offending seam and the disclosure or admission it reopens.
