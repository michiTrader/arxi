# ADR-0034: A retrieval receipt names every influence by version and reason

- Status: accepted
- Affects: `internal/memory`
- Depends on: ADR-0021, ADR-0033
- Enables: Phase 7 (read-only governed memory)

## Context

The record side now has identity, authority, validity, correction, deletion and
scope authorization (ADR-0021 through ADR-0033). Phase 7's exit evidence has a
second clause the record contract alone does not satisfy: "retrieval receipts
record exact selected versions and excerpts, ranking/index versions and reasons,"
and "every influence identifies its source and version."

`internal/contextprep` already has a `MemoryReceipt`, but it is the wrong
receipt. It records what was *presented to a model* — the presentation side. What
Phase 7 also requires is a record of what a *retrieval selected from the store* —
which versions, why, and under which ranking — upstream of presentation. Nothing
represented that, so "every influence identifies its source and version" had
nowhere to be asserted on the retrieval path.

## Decision

`internal/memory` gains `RetrievalReceipt` and `Selection`. A `Selection` names
the record and the exact version it drew from, the excerpt it carried forward,
and the reason it ranked in. A `RetrievalReceipt` records the principal scope the
retrieval was authorized against, the ranking/index version that produced the
ordering, and the selections.

`Validate` refuses a receipt that cannot account for what it selected:

- a principal with no tenant — a retrieval with no trust boundary is not an
  authorized retrieval (ADR-0033), so its receipt cannot be trusted to have
  respected one;
- no ranking version — a selection is reproducible only against the ranking that
  produced it, and the exit evidence names the ranking/index version explicitly;
- a selection with no record or no version — the "every influence identifies its
  source and version" clause, made executable; without the version a correction
  cannot say whether the influence was the version it superseded or the one that
  replaced it;
- a selection with no reason — an unexplained influence is one nobody can review,
  and the exit evidence names reasons beside versions;
- the same version selected twice — a double-counted influence overstates what
  one version contributed.

An **empty** receipt — zero selections — is valid and meaningful. A retrieval
that matched nothing, including one refused across a tenant boundary, produces a
receipt that proves nothing influenced the turn, which is itself the evidence the
leakage tests assert.

The `Excerpt` is optional: a selection may use a whole record and record no
narrower excerpt. It is not an identity, so it is not required; the identity is
the record and the version, and those are.

## Consequences

The retrieval path now has the audit artifact the exit evidence names, in the
same pure leaf as the records and the scope it authorizes against. It is the
companion to `contextprep.MemoryReceipt`: that one is the presentation side (what
a model was shown), this one is the retrieval side (what the store chose). Both
must name versions, and now both are refused if they do not.

The receipt does not re-check authorization against each record's scope, because
it carries the selections' identities and reasons, not the records' scopes; the
authorization happened upstream through `Scope.AuthorizedFor`, and the receipt is
its audit trail, not a second authorizer. It does enforce what it can see: a
tenant-less principal is refused, so a receipt cannot claim an unauthorized-shaped
retrieval.

`RankingVersion` is an opaque identifier here. This ADR does not decide how
ranking works — ranking remains undecided (roadmap item 7) — only that a receipt
must name the ranking it was produced under, so a decision about ranking later
has a field to be recorded in rather than a schema to change.

## Discarded alternatives

**Extend `contextprep.MemoryReceipt` to also carry retrieval selections.**
Conflates two receipts with different lifetimes and owners: the presentation
receipt is frozen into a durable prepared-context artifact under ADR-0013's
barrier, while a retrieval receipt is produced by the store before any
presentation exists. One struct serving both would be committed under the barrier
carrying retrieval-time fields that the presentation never had.

**Make `Excerpt` required.** Over-constrains a legitimate case — a selection that
uses a whole record has no narrower excerpt — and would push callers to
fabricate one. The identity is the version; the excerpt is supporting detail.

**Refuse an empty receipt.** Wrong, and it would erase the leakage evidence: a
retrieval that correctly returned nothing must still produce a receipt, because
"nothing was selected" is precisely what the cross-tenant and cross-user tests
need to see recorded.

**Have the receipt re-authorize each selection against its record's scope.** The
receipt does not hold the records' scopes, only the selections' identities;
re-authorizing would require carrying every record's full scope into the receipt,
duplicating state the store already checked. The authorizer is
`Scope.AuthorizedFor`; the receipt is the evidence it ran.

## How it is verified

`internal/memory/retrieval_test.go`:

- `TestRetrievalReceiptAcceptsAWellFormedReceipt` and
  `TestAnEmptyRetrievalIsAValidReceipt` — a populated receipt and a
  matched-nothing receipt both validate.
- `TestRetrievalReceiptRequiresAnAuthorizablePrincipal` and
  `TestRetrievalReceiptRequiresARankingVersion` — the principal must name a
  tenant and the receipt must name its ranking.
- `TestEveryInfluenceIdentifiesItsSourceAndVersion` — a selection missing its
  record or version is refused.
- `TestRetrievalSelectionRequiresAReason` and
  `TestRetrievalReceiptRefusesADoubleCountedVersion` — an unexplained selection
  and a repeated version are refused.
