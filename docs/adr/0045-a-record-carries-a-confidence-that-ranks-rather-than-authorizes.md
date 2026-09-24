# ADR-0045: A memory record carries a confidence, which ranks it rather than authorizing it

- Status: accepted
- Affects: `internal/memorystore`
- Depends on: ADR-0022, ADR-0027, ADR-0034, ADR-0042, ADR-0043, ADR-0044
- Enables: Phase 7 (read-only governed memory)

## Context

ADR-0042, ADR-0043 and ADR-0044 added the first three of Phase 7's item-7 record
dimensions — sensitivity, purpose and evidence class — each as an authorization
applied before ranking rather than as a struct field, and each with the check
that fails on it. ADR-0043 named the shape the rest must take and, in doing so,
drew the line this record is the first to cross: "confidence and retention are
record attributes that feed ranking and lifecycle rather than authorization. Each
remains future work and each must arrive the same way — with the check that fails
on it, and with the relation the dimension actually has."

This record adds confidence, and it is the first dimension on the ranking side of
that line. The four before it each decide whether a record may be seen at all;
confidence decides none of that. So the question this ADR must answer is not "how
is it authorized" but "what is the check that fails on it", because a ranking
attribute cannot borrow the authorization seam's witness — no query withholds a
record for its confidence, so there is no cross-confidence leakage test to write.
The roadmap names confidence explicitly as a place the field-nothing-fails-on
defect could recur, precisely because the obvious mechanism — a struct field the
ranker may or may not read — is one nothing fails on.

Confidence is also the honest counterweight to evidence class. ADR-0044's whole
argument was that evidence class *looks* rankable and is not: the trust order of
`stated`, `observed` and `imported` flips with the question asked, so any rank
there would be an order the domain does not have. Confidence is the opposite case,
and stating the two together is the point. It is a self-contained degree of belief
in the record's own correctness, and that belief does not invert with the
question: a record its writer was sure of outranks one they guessed at, whether
the caller is asking about a preference or an external fact. So confidence carries
a rank honestly, where evidence class could not — the two are the worked examples
of when a rank is earned and when it is invented.

## Decision

**A record carries a `Confidence` from a closed ranked vocabulary, and retrieval
orders the authorized set by it — highest first — after scope specificity and
before the record-ID tie-break. Confidence never withholds a record.**

The vocabulary is three levels, a closed set enumerated once as a map, the same
shape as the sensitivity `sensitivities` vocabulary and for the same reason
ADR-0022 gives — two lists drift, and a value valid in one place and unknown in
another leaks by omission rather than by decision. Unlike the evidence-class map,
which is a presence set carrying no order, this map's value is a rank, because
confidence is the dimension whose order the domain actually has. The three, least
to most vouched-for:

- `low` — offered with little assurance: a weak inference, a guess worth keeping
  but not worth trusting over anything better. It is the rank floor.
- `medium` — an ordinary assertion the writer stands behind without special
  corroboration.
- `high` — vouched for strongly: a corroborated fact, a preference the user stated
  outright.

**Confidence ranks; it does not authorize, and that is the load-bearing
distinction.** Its only mechanism is the ranking comparator in `Retrieve`. A
low-confidence record the caller is authorized for is still returned; it is simply
ordered below a high-confidence one. So a `Query` carries no confidence set: a
caller does not ask for "records of at least medium confidence" the way it names
the scopes, purposes and classes it is authorized for. Adding a query-side
confidence floor would turn a ranking dimension into an authorization one and
withhold material the caller may see merely because the writer was unsure — which
is not what "unsure" means and is the exact confusion this dimension is the worked
example against. The check that fails on confidence is therefore not a leakage
test but a *reordering* test: remove the confidence clause from the comparator and
two records tied on scope specificity fall back to record-ID order, presenting a
guess ahead of a vouched-for fact.

**Confidence is required, not defaulted.** `Validate` refuses a record with no
confidence or an unrecognized one, exactly as it refuses an empty `Sensitivity`,
`Purpose` or `EvidenceClass`. The temptation here is specific to a ranked
dimension: because confidence has a floor, one could treat an unstated confidence
as `low` and bury it, the way sensitivity nearly justifies a `public` default.
That is the defect, not the safe path. A confidence nobody stated is not low
confidence; it is *no assessment*, and ranking it as `low` launders a missing
judgment into a stated one — the record reads as "the writer judged this weak"
when the truth is "nobody judged it". Defaulting the other way, to `high`, is
worse: it promotes unvetted material over records a writer deliberately rated.
Neither default is honest, so the field is required, and a confidence a caller may
skip is the field nothing fails on.

**Confidence is part of the version identity.** It is added to the `identity`
struct that determines the content-addressed version ID, the deliberate act
ADR-0034 pins. It never authorizes, but it is still identity: two records alike in
everything but confidence are two different assertions about trust, and a
re-rating is a new version that supersedes the old one, never an in-place edit —
so a receipt naming a version still names the confidence that version was ranked
under. The digest-immutability guard in `Versions` and `verifyExisting` already
refuses a file whose confidence was altered after it was written, because the
altered field no longer digests to the version's own name; no new integrity check
is needed.

**The root verbs take a confidence; the derived verbs carry it forward.**
`Approve` and `Propose` create a record and so must be told how far to vouch for
it. `Correct`, `Delete`, `Promote` and `Resolve` carry the tip's confidence
forward verbatim, exactly as they already carry its scope, sensitivity, purpose
and evidence class, because the confidence is a property of the record set once at
creation, not a decision re-made on every correction. Carrying it forward is also
what stops a correction from silently re-rating a record by omission — and unlike
the four authorization dimensions, dropping it would not disclose or hide a
record, only misorder it, which is precisely why it needs the reordering witness
rather than a leakage one.

## Consequences

The store now carries five record dimensions: four that authorize before ranking
(scope, sensitivity, purpose, evidence class) and one that ranks the survivors
(confidence). The ranker, which ADR-0027 deliberately kept non-semantic — scope
specificity, then record ID — gains a middle key: scope specificity, then
confidence, then record ID. What a run or an agent learned still outranks what the
tenant believes in general; among records at the same specificity, the one its
writer vouched for now leads.

The retrieval evidence records each selected record's confidence and names it in
the selection reason, so an audit can see that a record placed below another was
placed there for its confidence and not withheld — which is the whole difference
between a ranking dimension and an authorization one, made legible in the receipt.

This is the first of the two ranking/lifecycle attributes ADR-0043 set aside.
Retention remains, and it feeds lifecycle rather than ranking, so it must arrive
with its own relation and its own failing check — a retention nothing expires on
is the same defect one rank lower. Valid time remains the last authorizing
dimension, still left for its own record because it is bitemporal and its as-of
instant is a clock value this store does not read.

## Discarded alternatives

**Model confidence as a float in `[0,1]`.** It reads as more precise and composes
with a future semantic score, and it is the wrong precision for this store. A
content-addressed identity digests the field, and a float's JSON encoding is not
canonical — `0.5`, `0.50` and `5e-1` are the same number and different bytes — so
two stores given the same write could mint different version IDs, breaking the
portability ADR-0034 rests on. A closed ranked vocabulary digests as a stable
string, sorts through a map the domain owns, and refuses a garbage value at
`Validate` instead of admitting `NaN` or `1.7`. The precision a float offers is
also unearned: no writer in Phase 7 can honestly distinguish 0.62 from 0.63.

**Put a confidence floor in the query and filter by it.** It would let a caller
say "only show me records you are sure of" and reuse the authorization seam. It is
the category error this ADR exists to prevent: confidence does not authorize, so
filtering by it would withhold a record the caller is entitled to see because the
*writer* was unsure — conflating "the caller may not have this" with "the writer
did not vouch for this". The caller that wants only high-confidence records reads
the ranked result and stops early; the store does not hide the rest.

**Add the field and let the ranker read it "when it matters".** The minimal
change: a `Confidence` field, no `Validate` requirement, no identity membership,
and a ranker that consults it if present. It is the field-nothing-fails-on defect
in full — a field with no failing check, exactly what ADR-0042, ADR-0043 and
ADR-0044 refused for their dimensions and what the roadmap warns confidence
invites. Nothing would fail if a writer omitted it, if a correction dropped it, or
if the ranker ignored it, so the attribute would rot into decoration and the first
regression would be silent.

**Default an unstated confidence to `low`.** It would spare every existing writer
the edit and looks conservative — bury the unjudged. But an unstated confidence is
the absence of a judgment, not a low one, and recording it as `low` fabricates an
assessment no one made; a later reader auditing why a record ranked last would be
told the writer rated it weak. The churn of requiring the field is the evidence
that the field is load-bearing.

## How it is verified

`internal/memorystore/confidence_test.go`:

- `TestConfidenceOrdersRetrievalAtTheSameSpecificity` — two records in the same
  scope, one `high` and one `low`, are both returned (proving confidence does not
  withhold) and the `high` one ranks first; the selection reason names the
  confidence that placed it.
- `TestConfidenceNeverWithholdsARecord` — a `low`-confidence record is returned to
  a query that authorizes it, with `Authorized` counting it, so confidence is
  shown to rank and never to filter — the distinction from the four authorization
  dimensions, made a test.
- `TestAnUnstatedOrUnknownConfidenceIsRefusedAtValidate` — an empty and a garbage
  confidence each fail `Validate` with a message naming the record and the
  consequence, not a bare "invalid".
- `TestConfidenceIsPartOfTheVersionIdentity` — two records identical but for their
  confidence seal to different version IDs, so the field cannot leave the identity
  without the test failing, the guarantee ADR-0034 pins applied here.
- `TestCorrectionCarriesConfidenceForward` — correcting a record recorded as
  `high` yields a `high` tip, so a correction cannot re-rate a record by omission.
- `TestRankOrderReadsTheVocabularyNotTheString` — `high` outranks `low` even
  though it sorts after it alphabetically, pinning that the comparator reads the
  confidence rank and not the raw string, so a refactor to string comparison
  fails here rather than in production.

Confirmed to fail closed by mutation: removing the confidence clause from the
`Retrieve` comparator makes the ordering test present the `low` record first;
defaulting an empty confidence to a member in `Validate` makes the validation test
admit it; dropping `Confidence` from the `identity` struct collapses the two
version IDs in the identity test; dropping the carry-forward in `Correct` re-rates
the tip; and comparing confidence as a string instead of through `Rank` puts `low`
ahead of `high`. Each names the offending seam and the misordering or admission it
reopens.
