# ADR-0043: A memory record is approved for a purpose, authorized before it is ranked

- Status: accepted
- Affects: `internal/memorystore`
- Depends on: ADR-0022, ADR-0027, ADR-0034, ADR-0042
- Enables: Phase 7 (read-only governed memory)

## Context

ADR-0042 added the first of Phase 7's item-7 record dimensions, sensitivity, and
added it as an authorization applied before ranking rather than as a struct
field. It named the rest in the same breath: "Evidence class, purpose and valid
time are the other authorizing dimensions Phase 7 names, and confidence and
retention are record attributes that feed ranking and lifecycle rather than
authorization," and it fixed the rule the rest must follow — "the scope
authorization seam and this clearance seam are now the two worked examples the
rest follow." Each remaining dimension arrives with the check that fails on it,
or it is decoration a later reader trusts.

This record adds purpose, and it is chosen next for the reason sensitivity was
chosen first: of the dimensions left, purpose is the one whose omission is a
disclosure. A record the user approved for one use — a travel date approved so
the agent can book a trip — surfaced under another — a proactive recommendation
feed — is the same leak the exit evidence already tests for, "cross-user and
cross-project leakage tests return zero records," arriving through purpose rather
than through scope or clearance. The store exists to prevent silent disclosure;
a purpose it does not enforce is purpose creep it cannot see.

Purpose is not sensitivity, and the difference is the point of this record.
Sensitivity is ranked: a clearance admits every level at or below it, so
authorization is a ceiling. Purpose has no order. `operate` is not more or less
restrictive than `recommend`; they are incomparable uses, and a record approved
for one is simply not approved for the other. So the authorization relation here
is **membership**, exactly the shape scope already uses — a set the query holds,
a single value the record carries — not the ceiling clearance uses. This matters
beyond purpose: a reader adding the next dimension must pick the relation the
dimension actually has, and assuming everything is ranked like sensitivity would
invent an order the domain does not have, which is ADR-0023's failure direction.
So this record is the second worked example of the scope relation, carried with
the identity discipline ADR-0042 established for sensitivity.

## Decision

**A record is approved for a `Purpose` from a closed vocabulary, and retrieval
refuses a record whose purpose is not among the query's authorized purposes, in
the authorization step before ranking.**

The vocabulary is three uses, a closed set enumerated once as a map, the same
shape as the scope `principals` and the sensitivity `sensitivities` vocabularies
and for the same reason ADR-0022 gives — two lists drift, and a value valid in
one place and unknown in another leaks by omission rather than by decision. The
map value is not a rank: purposes are incomparable, so the set carries no order
and authorization is membership, never a threshold. The three:

- `operate` — used to carry out the caller's current task.
- `personalize` — used to tailor output to the holder's stated preferences.
- `recommend` — used to proactively suggest content or actions the caller did
  not ask for.

An unknown purpose reports "not a purpose" rather than being admitted, because a
purpose nobody enumerated must get no standing — the same closed-vocabulary rule
the scope principal and the sensitivity level obey.

**Purpose is required, not defaulted.** `Validate` refuses a record with no
purpose or an unrecognized one, exactly as it refuses an empty `Sensitivity` or
an unknown `Kind`. The fail-open direction is starker here than for sensitivity:
sensitivity at least has a least-privilege member — `public` — that a
misguided default could reach for, but purpose has none, because the members are
incomparable. Treating an unstated purpose as "any purpose" would surface the
record for every use, which is purpose creep by construction, not a narrow leak.
Requiring the field means every writer states the use they are approving; that
cost is the decision, not a side effect of it. A purpose a caller may skip is
the field nothing fails on.

**Purpose is part of the version identity.** It is added to the `identity`
struct that determines the content-addressed version ID, the deliberate act
ADR-0034 pins. Two consequences follow for free, the same two sensitivity earned.
Re-purposing a record is a new version that supersedes the old one, never an
in-place edit, so a receipt naming a version still names the purpose that version
was approved under. And the digest-immutability guard in `Versions` and
`verifyExisting` already refuses a file whose purpose was altered after it was
written, because the altered field no longer digests to the version's own name;
no new integrity check is needed.

**Authorized purposes are a property of the query, and their floor is empty.** A
`Query` carries `Purposes`, the set of uses the caller is authorized to serve; a
record is authorized only if its purpose is in that set. An empty set authorizes
nothing — a caller that names no purpose is authorized for no purpose — which is
the fail-closed default and the honest one for an unranked dimension: there is no
floor member to fall back to, so forgetting to state a purpose discloses nothing
rather than everything. This is the scope rule exactly, where an empty scope set
authorizes no holder, and deliberately not the clearance rule, where an empty
clearance is the `public` floor: clearance can name a floor because it is ranked,
purpose cannot because it is not. A named purpose outside the vocabulary is
refused, the same closed-vocabulary rule the record's own purpose obeys.

**The root verbs take a purpose; the derived verbs carry it forward.** `Approve`
and `Propose` create a record and so must be told the use it is approved for.
`Correct`, `Delete`, `Promote` and `Resolve` carry the tip's purpose forward
verbatim, exactly as they already carry its scope and sensitivity, because
purpose is a property of the record set once at creation, not a decision re-made
on every correction. Carrying it forward is also what stops a correction from
silently re-purposing a record by omission.

## Consequences

The store now authorizes across three dimensions instead of two — scope,
sensitivity and purpose — all before ranking. A cross-purpose leakage test has a
witness that fails when the purpose filter is removed: a record approved for
`recommend`, queried by a caller authorized only for `operate`, must return
nothing while `Considered` proves it was present to be leaked — the same shape as
the cross-scope and cross-clearance tests, one dimension over.

The retrieval evidence records the purposes the query was authorized for and each
selected record's purpose, so an audit can answer not only which records were
presented and under what clearance but for which use each was approved — the
"every influence identifies its source" requirement, widened once more.

There are now three worked examples of the pre-ranking authorization seam, and
between them they demonstrate both relation shapes the remaining dimensions will
need: sensitivity is ranked and authorized against a ceiling, scope and purpose
are unranked and authorized by set membership. Evidence class and valid time are
the authorizing dimensions Phase 7 still names; confidence and retention are
record attributes that feed ranking and lifecycle rather than authorization.
Each remains future work and each must arrive the same way — with the check that
fails on it, and with the relation the dimension actually has — rather than as a
field added because the list said so.

## Discarded alternatives

**Default an unstated purpose to a wildcard.** It would spare every existing
writer the edit, and it is purpose creep by construction: a record with no stated
use, treated as usable for any, surfaces for every purpose the store knows. The
churn of requiring the field is the evidence that the field is load-bearing — the
same argument ADR-0042 made for refusing an empty sensitivity, sharper here
because purpose has no least-privilege member a default could safely pick.

**Make purpose ranked, with a floor, like sensitivity.** It would let an empty
query purpose fall back to a floor and spare the migration, and it is a false
model of the domain. Purposes are incomparable: a rank would declare, say,
`recommend` "below" `operate`, and a caller authorized to `operate` would then
receive a record approved only to `recommend` because it "sorts below" — a
disclosure for a use the record was never approved for, an order invented where
the domain has none. ADR-0023's failure direction: material gets the standing of
whatever it sorts beside instead of no standing at all.

**Model purpose as a set on the record and a single purpose on the query.** This
is the GDPR shape — data collected for a set of compatible purposes, processed
for one — and it is defensible, but it diverges from the uniform shape scope
established, where the record carries one authorization value per dimension and
the query holds the authorized set. A set-valued record field would also widen
the identity digest to cover a collection whose ordering would have to be
normalized before it could be content-addressed, for no authorization gain over
the membership check the single-valued field already provides.

**Filter by purpose after ranking.** Simpler to bolt on, and it is the exact
defect ADR-0027 refused for scope and ADR-0042 refused for clearance: the ranker
sees the record, so it has already been treated as a candidate and scored against
the others before being dropped. Authorization precedes ranking or it is not
authorization.

## How it is verified

`internal/memorystore/purpose_test.go`:

- `TestARecordOutsideTheQueryPurposeIsWithheld` — a record approved for
  `recommend` in the caller's own scope is not returned to a query authorized
  only for `operate`, and the retrieval evidence reports it as considered but not
  authorized, so a contained refusal is told apart from an absent record.
- `TestAQueryAuthorizesOnlyItsDeclaredPurposes` — a query authorized for
  `operate` and `personalize` returns the records approved for those and
  withholds the `recommend` one, pinning membership rather than a single case.
- `TestAQueryWithNoPurposeAuthorizesNothing` — an empty purpose set returns
  nothing while the record is considered, pinning the fail-closed floor of an
  unranked dimension so nobody later gives purpose a wildcard default.
- `TestAnUnclassifiedOrUnknownPurposeIsRefusedAtValidate` — an empty and a
  garbage purpose each fail `Validate` with a message naming the record and the
  purpose-creep consequence, not a bare "invalid".
- `TestAnUnknownQueryPurposeIsRefused` — a query naming a purpose outside the
  vocabulary is refused rather than silently dropped from the authorized set.
- `TestPurposeIsPartOfTheVersionIdentity` — two records identical but for their
  purpose seal to different version IDs, so the field cannot leave the identity
  without the test failing, the guarantee ADR-0034 pins applied here.
- `TestCorrectionCarriesPurposeForward` — correcting a record approved for
  `personalize` yields a `personalize` tip, so a correction cannot re-purpose by
  omission.

Confirmed to fail closed by mutation: removing the purpose filter in `Retrieve`
makes the withholding test return the `recommend` record; defaulting an empty
purpose to a wildcard in `Validate` makes the validation test admit it; dropping
`Purpose` from the `identity` struct collapses the two version IDs in the
identity test; dropping the carry-forward in `Correct` re-purposes the tip. Each
names the offending seam and the leak it reopens.
