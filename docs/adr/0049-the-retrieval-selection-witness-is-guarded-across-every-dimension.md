# ADR-0049: The retrieval selection witness is guarded across every dimension, because the audit trail's structured fields are the field nothing fails on

- Status: accepted
- Affects: `internal/memorystore`
- Depends on: ADR-0034, ADR-0042, ADR-0043, ADR-0044, ADR-0045, ADR-0047, ADR-0048
- Enables: Phase 7 (read-only governed memory)

## Context

Phase 7's exit evidence requires retrieval receipts to record "ranking/index
versions and reasons" and that "every influence identifies its source". The
artifact that carries that in the store is `Retrieval.Selection`: one entry per
returned record, holding the record's identity — `RecordID`, `VersionID`, `Scope`
— and the full classification it was admitted under — `Sensitivity`, `Purpose`,
`EvidenceClass`, `Confidence`, and the `ValidFrom`/`ValidTo` bounds of its valid-time
interval. Five records built that struct field by field: ADR-0042 added the
sensitivity witness, ADR-0043 the purpose, ADR-0044 the evidence class, ADR-0045 the
confidence, ADR-0047 the valid-time bounds, and each justified its field in the same
words — "recorded so an audit can answer ... beside the retrieval's clearance".

No test asserts any of those structured fields. `store_test.go` checks only that the
number of selections matches the number of returned records and that each `Reason`
string is non-empty; `confidence_test.go` checks that `Selections[0].Reason` contains
the substring `confidence high`. `Reason` is the human-readable free-text line, and
ADR-0020 already ruled that such prose is legibility, not a boundary. The
machine-readable fields an audit tool actually parses — the ones every ADR from 0042
to 0047 describes as the witness — are asserted nowhere.

Probing it confirmed the gap is live, not cosmetic, and confirmed its width. Blanking
every structured field of `Selection` at once — `RecordID`, `VersionID`, `Scope`,
`Sensitivity`, `Purpose`, `EvidenceClass`, `Confidence`, `ValidFrom`, `ValidTo`, each
set to the empty string — leaves the whole `internal/memorystore` suite green. Each
field also survives dropping in isolation. A `Retrieve` that stopped populating the
receipt, or populated it from the wrong record, would ship silently, and the artifact
Phase 7's exit evidence rests on would be unverified at its content.

**Why this is a different gap from the one ADR-0048 closed, and why it must not be
framed as valid time's.** ADR-0048 was about the record's own valid-time window being
carried forward at each derived verb — a property of the stored record. This is the
retrieval evidence — a different artifact, produced at read rather than write — and
the gap is not specific to valid time. Every witness dimension is unasserted, so a
valid-time-only guard here would be a fresh instance of the exact
"verified once, in one place, and generalised to a whole capability" failure this
corpus keeps finding, and `AGENTS.md` names outright. The honest subject is the whole
`Selection`, so the guard is written against the whole `Selection`.

## Decision

**One guard pins the entire selection witness, and it reads the expected values off
the record the selection names rather than from hand-copied literals.**
`TestRetrievalSelectionWitnessesTheRecordItNames` retrieves a record whose every
witnessed dimension is set to a distinct value, then asserts each `Selection` field
equals the corresponding field of the returned record. Deriving the expectation from
the record is the point: the property under test is that the receipt *describes the
record it names*, not that either happens to match a constant a later edit might
forget to update — the derive-the-subject-from-the-corpus discipline `AGENTS.md`
prescribes for guards whose subject would otherwise go stale. Distinct values across
dimensions mean a witness populated from the wrong field is caught as well as one
dropped.

No production code changes. `Retrieve` already builds the witness correctly, and
probing found no dimension it omits or mis-sources; this record closes a verification
gap in the manner of ADR-0034 and ADR-0048 — a test that fails the moment an
assumption is withdrawn, added where the previous coverage was absent.

**The guard's subject is the receipt's content, not its shape.** A test that only
counted selections or checked `Reason` was non-empty — the coverage that existed —
asserts the receipt has the right number of rows and that a string was written, never
that the rows say anything true. The distinction matters because the receipt is not
read by a human at the moment it is written; it is committed into a prepared-context
artifact and parsed later by whatever audits the presentation. A witness whose fields
are blank or transposed passes every shape check and fails the only reader that
matters.

## Consequences

The structured content of the retrieval receipt is now verified at every dimension it
carries, so a `Retrieve` that dropped a witness field, or filled it from the wrong
record, fails a test that names the dimension and the audit consequence it defeats,
instead of passing silently. The five records that each added a witness field now have
their claim — "recorded so an audit can answer ..." — proven rather than asserted.

No behavior changes. The receipt carried the same fields before this record and carries
them after; what changed is that its content is now defended where only its shape was.
The free-text `Reason` remains checked for presence, not pinned to an exact string:
that line is deliberately prose an audit does not parse, and pinning it would churn
every time the ranker's wording changes while adding nothing the structured assertions
do not already give.

## Discarded alternatives

**Leave it: the receipt is built correctly, so the missing assertions cost nothing.**
This is the argument that has lost every prior time it was made in this corpus, and
the mutation is the proof the cost is not zero. A witness that is correct today and
unasserted is one refactor of `Retrieve` away from being blank or transposed and still
unasserted — and because the receipt is committed into an artifact and read by a tool,
not a person, the regression would surface as an audit that silently answers the wrong
question, not as an obvious break. The nine field drops that pass the green suite today
are the deferred cost, handed to whoever next edits the selection loop.

**Assert only the valid-time witness, mirroring ADR-0048.** It would look like the
natural next step after ADR-0048 and would be the same mistake ADR-0048 itself warns
against, one artifact over. The mutation shows every dimension is unguarded, not just
valid time, so scoping the guard to valid time would verify the capability at its
narrowest point and generalise a green result to the eight fields it never touched —
the precise shape `AGENTS.md` says to fail closed against.

**Pin the `Reason` string's full content instead of the fields.** It would appear to
cover the same ground with one assertion. It pins the wrong thing: `Reason` is
human-readable prose that ADR-0020 classes as legibility rather than a boundary, it
omits sensitivity, purpose, evidence class and the valid-time bounds entirely, and it
changes whenever the ranker's explanation is reworded. An audit parses the structured
fields; those are the stable contract and the thing the exit evidence is about, so they
are what the guard asserts.

## How it is verified

`internal/memorystore/selection_witness_test.go`:

- `TestRetrievalSelectionWitnessesTheRecordItNames` — approves a record with a distinct
  value in every witnessed dimension (a confidential, personalize, observed, high,
  bounded-window record), retrieves it under a query authorized for exactly that
  classification, and asserts each `Selection` field — `RecordID`, `VersionID`,
  `Scope`, `Sensitivity`, `Purpose`, `EvidenceClass`, `Confidence`, `ValidFrom`,
  `ValidTo` — equals the corresponding field of the returned record.

Confirmed to fail closed by mutation: blanking each of the nine structured fields in
the selection loop in turn fails the matching assertion, where before this record all
nine left the suite green; the baseline passes. Each message names the dimension and
the audit question the missing witness defeats — an unrecorded clearance, an invisible
purpose creep, an inference presented as an assertion with no trace, a ranking that
cannot be told from a withholding, or a stale fact whose window the receipt cannot show.
