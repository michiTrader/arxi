# ADR-0044: A memory record carries an evidence class, authorized before it is ranked

- Status: accepted
- Affects: `internal/memorystore`
- Depends on: ADR-0022, ADR-0027, ADR-0034, ADR-0042, ADR-0043
- Enables: Phase 7 (read-only governed memory)

## Context

ADR-0042 added the first of Phase 7's item-7 record dimensions, sensitivity, as
an authorization applied before ranking rather than as a struct field; ADR-0043
added the second, purpose, and named the shape the rest must take: "Evidence
class and valid time are the authorizing dimensions Phase 7 still names;
confidence and retention are record attributes that feed ranking and lifecycle
rather than authorization. Each remains future work and each must arrive the same
way — with the check that fails on it, and with the relation the dimension
actually has — rather than as a field added because the list said so."

This record adds evidence class, and it is chosen next over valid time for two
reasons. Its omission is a disclosure of the kind the exit evidence already
tests: Phase 7 opens with a governance rule — model material "may propose
candidates but cannot create active memory" — and evidence class is the finer
grain of that same concern one level down. `Kind` says whether a record was
approved or proposed; evidence class says what the record rests on once approved.
A record backed only by an inference the run `observed`, surfaced to a caller
that asked only for what a user `stated`, is an ungoverned influence presented as
an authorized one — the leak Phase 7 exists to prevent, arriving through evidence
rather than through scope, clearance or purpose. And it is the self-contained
choice: valid time is bitemporal and its as-of instant is a clock value this
store deliberately does not read (it binds versions to a run and a sequence, not
to a wall clock), so it is rightly the last authorizer, added once the simpler
relations are all in place.

Evidence class is not sensitivity, and it is not quite purpose either. It is
unranked, so — like purpose and scope, unlike sensitivity — it is authorized by
set membership, not against a ceiling. But it earns its own record because the
temptation to rank it is real and wrong, and refusing that temptation is the
decision. This is the third worked example of the membership relation, and the
first whose whole argument is *why it is not the ranked one it looks like*.

## Decision

**A record carries an `EvidenceClass` from a closed vocabulary, and retrieval
refuses a record whose class is not among the query's accepted classes, in the
authorization step before ranking.**

The vocabulary is three kinds, a closed set enumerated once as a map, the same
shape as the scope `principals`, the sensitivity `sensitivities` and the purpose
`purposes` vocabularies and for the same reason ADR-0022 gives — two lists drift,
and a value valid in one place and unknown in another leaks by omission rather
than by decision. The map value is not a rank: evidence classes are incomparable,
so the set carries no order and authorization is membership, never a threshold.
The three:

- `stated` — asserted directly by a user or an operator: a preference they
  declared, a fact they gave. Its evidence is the assertion itself.
- `observed` — derived from execution the run witnessed: a tool result, a system
  event, a value read from the environment rather than declared.
- `imported` — brought in from an external system of record during an import: its
  evidence is that external source, not this system.

Evidence class is orthogonal to `Kind`, and that separation is deliberate. `Kind`
is authority — who has standing to create the record (a user approval, an
operator import, a model proposal). Evidence class is what the record rests on,
and a record of any authority can rest on any evidence: a model may `Propose` a
candidate it `observed` from a tool result, an operator may `Approve` a record
they `imported`. Collapsing the two would lose exactly the audit answer Phase 7
asks for — not only who approved a record but what it is evidence of.

An unknown class reports "not an evidence class" rather than being admitted,
because a class nobody enumerated must get no standing — the same closed-vocabulary
rule the scope principal, the sensitivity level and the purpose obey.

**Evidence class is required, not defaulted.** `Validate` refuses a record with
no class or an unrecognized one, exactly as it refuses an empty `Sensitivity`, an
empty `Purpose` or an unknown `Kind`. The fail-open direction is purpose's, not
sensitivity's: there is no least-privilege member a default could reach for,
because the members are incomparable, so a default could only pick one arbitrary
class and admit the record to every query that accepts it. Treating an unstated
class as "any evidence" would surface an inference wherever an assertion was
asked for. Requiring the field means every writer states what backs the record;
that cost is the decision, not a side effect of it. A class a caller may skip is
the field nothing fails on.

**Evidence class is part of the version identity.** It is added to the `identity`
struct that determines the content-addressed version ID, the deliberate act
ADR-0034 pins. The same two consequences sensitivity and purpose earned follow
for free. Reclassifying the evidence of a record is a new version that supersedes
the old one, never an in-place edit, so a receipt naming a version still names
the class that version was recorded under. And the digest-immutability guard in
`Versions` and `verifyExisting` already refuses a file whose class was altered
after it was written, because the altered field no longer digests to the
version's own name; no new integrity check is needed.

**Accepted classes are a property of the query, and their floor is empty.** A
`Query` carries `EvidenceClasses`, the set of evidence kinds the caller will act
on; a record is authorized only if its class is in that set. An empty set accepts
nothing — a caller that names no class accepts no evidence — which is the
fail-closed default and the honest one for an unranked dimension: there is no
floor member to fall back to, so forgetting to state a class discloses nothing
rather than everything. This is the scope and purpose rule exactly, and
deliberately not the clearance rule, where an empty clearance is the `public`
floor: clearance can name a floor because it is ranked, evidence class cannot
because it is not. A named class outside the vocabulary is refused, the same
closed-vocabulary rule the record's own class obeys.

**The root verbs take an evidence class; the derived verbs carry it forward.**
`Approve` and `Propose` create a record and so must be told what it is evidence
of. `Correct`, `Delete`, `Promote` and `Resolve` carry the tip's class forward
verbatim, exactly as they already carry its scope, sensitivity and purpose,
because the evidence class is a property of the record set once at creation, not
a decision re-made on every correction. Carrying it forward is also what stops a
correction from silently reclassifying a record's evidence by omission.

## Consequences

The store now authorizes across four dimensions instead of three — scope,
sensitivity, purpose and evidence class — all before ranking. A cross-class
leakage test has a witness that fails when the class filter is removed: a record
backed by `observed` evidence, queried by a caller accepting only `stated`, must
return nothing while `Considered` proves it was present to be leaked — the same
shape as the cross-scope, cross-clearance and cross-purpose tests, one dimension
over.

The retrieval evidence records the classes the query accepted and each selected
record's class, so an audit can answer not only which records were presented,
under what clearance and for which use, but on what kind of evidence each rested
— the "every influence identifies its source" requirement, widened once more.

There are now four worked examples of the pre-ranking authorization seam. Three
of them — scope, purpose and evidence class — are the membership relation, and
this one is the example that shows membership is a choice about the domain and
not a default: an evidence class looks rankable and is not, and ranking it would
be the order-inventing failure ADR-0023 and ADR-0043 name. Valid time is the last
authorizing dimension Phase 7 names, and it is left for its own record because it
is bitemporal and its as-of instant is a clock value this store does not read;
confidence and retention feed ranking and lifecycle rather than authorization.
Each remaining piece must arrive the same way — with the check that fails on it,
and with the relation the dimension actually has.

## Discarded alternatives

**Rank evidence class, with a floor, like sensitivity.** It would let an empty
query accept everything at or above a floor and spare the caller from enumerating
classes, and it is a false model of the domain. The trust order of these classes
is not fixed: for a user's stated preference, `stated` is authoritative and an
`observed` inference is the weaker guess; for an external fact, `imported` from a
system of record outranks whatever the user `stated` from memory. The order flips
with the question, so any rank would declare, say, `observed` "above" `stated`,
and a caller that accepts `stated` would then receive an inference it never asked
for because it "sorts above" — ADR-0023's failure direction, material getting the
standing of whatever it sorts beside instead of no standing at all.

**Default an unstated class to a wildcard.** It would spare every existing writer
the edit, and it is the same fail-open ADR-0042 refused for sensitivity and
ADR-0043 refused for purpose, sharper here because — as with purpose — there is
no least-privilege member a default could safely pick. A record with no stated
evidence, treated as usable on any, surfaces wherever any evidence is accepted.
The churn of requiring the field is the evidence that the field is load-bearing.

**Fold evidence class into `Kind`.** `Kind` already carries provenance authority,
so a reader might extend it — `observed_candidate`, `imported_approval` — rather
than add a dimension. It conflates two independent facts: a record's authority
(who may create it) and its evidence (what it rests on), which vary independently,
and the product of the two vocabularies grows multiplicatively while each stays
separately closed and separately authorized. It would also break the `Kind`
enumeration `contextprep` owns (ADR-0023), which this store spends rather than
redeclares, so a compound kind would be unknown at the barrier and fail closed —
the right failure for the wrong reason.

**Filter by evidence class after ranking.** Simpler to bolt on, and it is the
exact defect ADR-0027 refused for scope, ADR-0042 for clearance and ADR-0043 for
purpose: the ranker sees the record, so it has already been treated as a
candidate and scored against the others before being dropped. Authorization
precedes ranking or it is not authorization.

## How it is verified

`internal/memorystore/evidence_class_test.go`:

- `TestARecordOutsideTheQueryEvidenceClassesIsWithheld` — a record backed by
  `observed` evidence in the caller's own scope is not returned to a query that
  accepts only `stated`, and the retrieval evidence reports it as considered but
  not authorized, so a contained refusal is told apart from an absent record.
- `TestAQueryAcceptsOnlyItsDeclaredEvidenceClasses` — a query accepting `stated`
  and `imported` returns the records of those classes and withholds the
  `observed` one, pinning membership rather than a single case.
- `TestAQueryWithNoEvidenceClassAcceptsNothing` — an empty class set returns
  nothing while the record is considered, pinning the fail-closed floor of an
  unranked dimension so nobody later gives evidence class a wildcard default.
- `TestAnUnclassifiedOrUnknownEvidenceClassIsRefusedAtValidate` — an empty and a
  garbage class each fail `Validate` with a message naming the record and the
  consequence, not a bare "invalid".
- `TestAnUnknownQueryEvidenceClassIsRefused` — a query naming a class outside the
  vocabulary is refused rather than silently dropped from the accepted set.
- `TestEvidenceClassIsPartOfTheVersionIdentity` — two records identical but for
  their evidence class seal to different version IDs, so the field cannot leave
  the identity without the test failing, the guarantee ADR-0034 pins applied here.
- `TestCorrectionCarriesEvidenceClassForward` — correcting a record recorded as
  `imported` yields an `imported` tip, so a correction cannot reclassify the
  evidence by omission.

Confirmed to fail closed by mutation: removing the class filter in `Retrieve`
makes the withholding test return the `observed` record; defaulting an empty
class to a wildcard in `Validate` makes the validation test admit it; dropping
`EvidenceClass` from the `identity` struct collapses the two version IDs in the
identity test; dropping the carry-forward in `Correct` reclassifies the tip; and
admitting an unknown query class answers a misspelled query. Each names the
offending seam and the leak it reopens.
