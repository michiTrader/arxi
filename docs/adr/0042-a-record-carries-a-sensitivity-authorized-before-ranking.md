# ADR-0042: A memory record carries a sensitivity, authorized before it is ranked

- Status: accepted
- Affects: `internal/memorystore`
- Depends on: ADR-0021, ADR-0022, ADR-0023, ADR-0027, ADR-0034
- Enables: Phase 7 (read-only governed memory)

## Context

Phase 7 says a governed record carries "evidence class, authority, confidence,
sensitivity, purpose, retention, lifecycle and bitemporal validity", and that
"authorization occurs before semantic ranking across tenant, user, application,
project, team, agent, run, evidence class, sensitivity, purpose and valid time".
The store built by ADR-0027 authorizes across exactly one of those dimensions —
the scope principal — and none of the record vocabulary exists. The phase names
the rest as its own deferred item and warns, in the same breath, that "adding
them as struct fields with nothing authorizing them would be the field nothing
fails on defect this corpus has now recorded four times".

So the vocabulary cannot be added as a block of fields. Each dimension has to
arrive with the thing that fails on it, or it is decoration that a later reader
trusts. This record adds the first of them, and it is chosen first for a
specific reason: of the dimensions the phase lists as authorizing, sensitivity
is the one whose omission is a disclosure. A record scoped to the right user but
classified secret, handed to a caller cleared only for ordinary material, is the
same leak the exit evidence already tests for — "cross-user and cross-project
leakage tests return zero records" — arriving through clearance rather than
through scope. The store exists to prevent silent disclosure; a sensitivity it
does not enforce is a disclosure it cannot see.

The scope dimension is the model to follow, not a thing to bolt beside. ADR-0027
made authorization strictly precede ranking because "a relevance score computed
across tenants is itself a cross-tenant inference even when the record is dropped
afterwards". The identical argument holds one dimension over: a ranker that has
seen a secret record has already treated it as a candidate, so the exclusion has
to happen in the authorization step, before ranking, on the same reasoning and
at the same seam.

## Decision

**A record carries a `Sensitivity` from a closed, ranked vocabulary, and
retrieval refuses a record whose sensitivity outranks the caller's clearance,
in the authorization step before ranking.**

The vocabulary is four levels, ranked least to most restrictive: `public`,
`internal`, `confidential`, `secret`. It is a closed set enumerated once as a
map, the same shape as the scope `principals` vocabulary and for the same reason
ADR-0022 gives — two lists of levels drift, and a level valid in one place and
unknown in another leaks by omission rather than by decision. An unknown level
reports "not a level" rather than a default rank, because a rank invented for an
unrecognized level would sort it against real ones and let it be retrieved, which
is ADR-0023's failure direction: material nobody classified must get no standing,
not the standing of whatever it sorts beside.

**Sensitivity is required, not defaulted.** `Validate` refuses a record with no
sensitivity or an unrecognized one, exactly as it already refuses an empty or
unknown `Kind`. The reason is the fail-closed direction: an unclassified record's
sensitivity is unknown, and treating unknown as `public` is fail-open — the most
sensitive material is the most likely to be written in a hurry, and the hurry is
when the level is forgotten. Requiring it means every writer classifies; that
cost is the decision, not a side effect of it. A sensitivity a caller may skip is
the field nothing fails on.

**Sensitivity is part of the version identity.** It is added to the `identity`
struct that determines the content-addressed version ID, the deliberate act
ADR-0034 pins. Two consequences follow for free. A reclassification is a new
version that supersedes the old one, never an in-place edit, so a receipt naming
a version still names the sensitivity that version was presented under —
ADR-0021's guarantee, extended to the new field. And the digest-immutability
guard in `Versions` and `verifyExisting` already refuses a file whose sensitivity
was altered after it was written, because the altered field no longer digests to
the version's own name; no new integrity check is needed.

**Clearance is a property of the query, and its floor is `public`.** A `Query`
carries a `Clearance`; a record is authorized only if its sensitivity rank does
not exceed the clearance rank. An empty clearance is the public floor — a caller
that names no clearance receives only material that needs none — which is the
fail-closed default: forgetting to state a clearance discloses the minimum, not
the maximum. A non-empty clearance that is not a known level is refused, the same
closed-vocabulary rule the record's own level obeys.

**The root verbs take a sensitivity; the derived verbs carry it forward.**
`Approve` and `Propose` create a record and so must be told its level. `Correct`,
`Delete`, `Promote` and `Resolve` carry the tip's sensitivity forward verbatim,
exactly as they already carry its scope, because sensitivity is a property of the
record set once at creation, not a decision re-made on every correction. Carrying
it forward is also what stops a correction from silently declassifying a secret
record by omission.

## Consequences

The store now authorizes across two dimensions instead of one, both before
ranking. A cross-clearance leakage test has a witness that fails when the
clearance filter is removed: a secret record in the caller's own scope, queried
under a public clearance, must return nothing while `Considered` proves it was
present to be leaked — the same shape as the cross-scope test, one dimension over.

The retrieval evidence records the clearance the query was authorized under and
each selected record's sensitivity, so an audit can answer not only which records
were presented but under what clearance and at what classification — the
"every influence identifies its source" requirement, widened to say at what level
that influence was cleared.

This is one dimension, not the vocabulary. Evidence class, purpose and valid time
are the other authorizing dimensions Phase 7 names, and confidence and retention
are record attributes that feed ranking and lifecycle rather than authorization.
Each remains future work and each must arrive the same way this one did — with
the check that fails on it — rather than as a field added because the list said
so. The scope authorization seam and this clearance seam are now the two worked
examples the rest follow.

## Discarded alternatives

**Default an unclassified record to `public`.** It would spare every existing
writer the edit, and it is precisely the fail-open the phase warns about: the
level most often left unset is the sensitive one, and a default of `public`
discloses exactly the records that most needed a classification. The churn of
requiring the field is the evidence that the field is load-bearing.

**Default an unclassified record to `secret`.** Fail-closed, but silently: a
forgotten classification becomes an invisible record rather than a loud refusal,
and a store full of accidentally-secret material reads as a retrieval bug, not as
the classification mistake it is. `Validate` refusing the empty level names the
mistake at the write, where the caller can fix it.

**Filter by clearance after ranking.** Simpler to bolt on, and it is the exact
defect ADR-0027 refused for scope: the ranker sees the secret record, so it has
already been treated as a candidate and scored against the others before being
dropped. Authorization precedes ranking or it is not authorization.

**Keep sensitivity out of the version identity.** It would let a record be
reclassified in place without minting a new version, which reads as less churn
until a receipt names version 2 and version 2's sensitivity has since changed
underneath it — the mutable-record defect ADR-0021 exists to forbid, reintroduced
for one field.

## How it is verified

`internal/memorystore/sensitivity_test.go`:

- `TestARecordOverTheCallersClearanceIsWithheld` — a secret record in the
  caller's own scope is not returned under a public clearance, and the retrieval
  evidence reports it as considered but not authorized, so a contained refusal is
  told apart from an absent record.
- `TestClearanceAdmitsAtOrBelowItself` — a confidential clearance returns public,
  internal and confidential records and withholds the secret one, pinning the
  ranking direction rather than a single boundary.
- `TestAnUnclassifiedOrUnknownRecordIsRefusedAtValidate` — an empty and a
  garbage sensitivity each fail `Validate` with a message naming the record and
  the fail-open consequence, not a bare "invalid".
- `TestAnUnknownClearanceIsRefused` — a query naming a level outside the
  vocabulary is refused rather than silently treated as the floor.
- `TestSensitivityIsPartOfTheVersionIdentity` — two records identical but for
  their sensitivity seal to different version IDs, so the field cannot leave the
  identity without the test failing, the guarantee ADR-0034 pins for the edge
  fields applied here.
- `TestCorrectionCarriesSensitivityForward` — correcting a confidential record
  yields a confidential tip, so a correction cannot declassify by omission.

Confirmed to fail closed by mutation: removing the clearance filter in
`Retrieve` makes the withholding test return the secret record; defaulting an
empty sensitivity to `public` in `Validate` makes the validation test admit it;
dropping `Sensitivity` from the `identity` struct collapses the two version IDs
in the identity test. Each names the offending seam and the leak it reopens.
