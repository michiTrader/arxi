# ADR-0027: The header vocabulary is derived from the corpus, not counted by hand

- Status: accepted
- Affects: `internal/roadmap_enablement_test.go`, `docs/adr/0026-the-roadmap-cites-every-enabling-decision.md`, `docs/roadmap.md`
- Depends on: ADR-0026
- Enables: Phase 7 (read-only governed memory)

## Context

ADR-0026 closed the gap between an ADR claiming to enable a phase and that
phase's status admitting it. Before building the store on top of it, the same
probe that produced the previous six findings was pointed at ADR-0026's own
output rather than at the code: measure the last turn instead of reading it.

ADR-0026 introduced `adrHeaderVocabulary`, an enumerated set of the field names
an ADR header may use, and fails closed on any field outside it. It justified
the set in prose, in both the ADR body and the test comment beside the map:

> The set is the measured vocabulary of all 25 records — `Status` and `Affects`
> in every one, `Depends on` in 16, `Enables` in 6, `Origin` in 2.

Measured against the corpus that sentence describes, every figure that could be
off was off by one. The corpus held **26** records, not 25. `Depends on`
appeared **17** times, not 16. `Enables` appeared **7** times, not 6. Only
`Origin` (2) was right, and only because ADR-0026 — the record making the
claim — carries no `Origin` field.

That is the tell. The counts describe the corpus **without the record stating
them**: the author ran the measurement on 25 files, wrote the numbers, then added
a 26th file carrying `Status`, `Affects`, `Depends on` and `Enables`, and never
re-ran it. It is the identical off-by-one that
`TestEveryDocumentStatingTheADRCountAgreesWithTheCorpus` was written to catch for
the *total* count, reproduced one field-set deeper and left unchecked — inside
the very test file that catches the total-count version.

### Why nothing caught it

`TestEveryDocumentStatingTheADRCountAgreesWithTheCorpus` pins the **total**
record count, because it appears in a fixed phrase (`"Twenty-six records"`) that
a string check can locate. The **per-field** frequencies appear only as free
prose — "`Depends on` in 16" — and nothing derives or checks them.

The citation test does read the header fields, but only in one direction. It
fails closed when the corpus uses a field the map does not list (map ⊇ corpus),
which is what makes a renamed `Enables` header fail instead of silently
exempting its ADR. It never checks the reverse: a field the map lists that no
ADR uses (corpus ⊇ map) passes silently. So the map could drift from the corpus
in the one direction its own justifying prose asserts it does not — "the
measured vocabulary of all N records" — with nothing failing.

This is the fourth instance of one shape, and each was found by measuring the
turn before it:

- **ADR-0024** — a guard with no caller. `MemoryReceipt.Validate` existed and
  nothing invoked it.
- **ADR-0025** — an assertion with no subject. The channel was asserted against
  hand-built literals, never the code that assembles messages.
- **ADR-0026** — a narration with no source. The roadmap status was written by
  hand and nothing tied it to the decisions it summarized.
- **ADR-0027** — a count with no derivation. The vocabulary's frequencies were
  written by hand and nothing tied them to the files they count.

In all four the artifact was published and something meant to keep it true did
not exist. A green suite is what disguised each one.

## Decision

**The header vocabulary is a checked projection of the corpus, and its
frequencies are derived on every run rather than narrated once.**

`TestHeaderVocabularyMatchesTheCorpus` reads every numbered ADR, extracts the
header fields above the first `## ` section, and asserts `adrHeaderVocabulary`
equals the observed set **exactly**, in both directions. The map ⊇ corpus
direction keeps the citation test's fail-closed guard honest; the added corpus ⊇
map direction refuses a vocabulary entry no record uses, which is the vacuity
ADR-0026 warned about one level up. An empty observed set fails rather than
passing over two empty sets.

The frozen per-field counts are **removed** from the test comment and from
ADR-0026's verification section, and replaced with a reference to the derivation.
A count kept by hand drifts precisely because nothing fails when it does; the
remedy is not a fresh correct number — which drifts again on the next ADR — but a
number the test computes.

Correcting ADR-0026's body is a factual repair of a wrong measurement, not a
rewrite of its decision. Its argument, its discarded alternatives and the
decision it records are untouched; only the sentence stating a corpus count is
changed, because leaving a known-wrong count in an accepted record contradicts
this project's "verify, do not assume" rule at the exact site that rule is about.

## Consequences

Adding a legitimate new header field now requires two coordinated edits in one
change: the ADR that first uses it, and the `adrHeaderVocabulary` entry for it.
Doing one without the other fails `TestHeaderVocabularyMatchesTheCorpus` — the
map entry with no user, or the corpus field with no entry. That coupling is the
intent: the vocabulary and the corpus move together or the suite says so.

The per-field frequencies are no longer stated anywhere as a fixed number, so
there is no count left to go stale. The total count remains pinned separately in
`AGENTS.md` and the index, because those are prose a reader relies on and they
drift independently of the field vocabulary.

## Discarded alternatives

**Just correct the numbers to 26 / 17 / 7 / 2.** The obvious fix and the wrong
one: it re-freezes a hand-kept count that is correct only until ADR-0028, which
is the mechanism that produced the defect. The project already learned this for
the total count and pinned it; the per-field counts get derived for the same
reason.

**Pin the prose counts with a string check, like the total count.** The total
count works because it lives in one fixed phrase. The per-field counts appear in
varying prose ("`Depends on` in 16", "Depends on in 16"), and asserting the
accuracy of free prose is the trap ADR-0026 named in its own discarded
alternatives: a check that appears to verify accuracy while verifying wording.
Deriving the map and deleting the prose count avoids needing to parse the prose
at all.

**Leave ADR-0026's body alone as an immutable historical record.** Rejected: the
sentence is a factual measurement, not a decision, and it is wrong. An accepted
record carrying a wrong count is the same regression this project fixes on sight
when it finds a stale number elsewhere; exempting the ADR corpus from that rule
would exempt the one place a reader most trusts.

**Check only corpus ⊇ map and drop the existing map ⊇ corpus guard.** Rejected:
the two directions protect different things. Map ⊇ corpus keeps a renamed
`Enables` header from silently exempting its ADR from the citation check; corpus
⊇ map keeps the map from claiming a field the corpus lacks. Exact equality is
both, and dropping either reopens a hole.

## How it is verified

`TestHeaderVocabularyMatchesTheCorpus` derives the field set on every run and
asserts exact equality with `adrHeaderVocabulary`, failing with the consequence
and the remedy named.

Mutation-verified. Adding a `"Reviewers"` entry the corpus does not use fails the
corpus ⊇ map direction; removing a field the corpus does use (were the map
edited to drop `Enables`) fails the map ⊇ corpus direction; forcing the observed
set empty fails the anti-vacuity guard rather than comparing two empty sets. The
citation test `TestEveryEnablingDecisionIsCitedByThePhaseItEnables` continues to
pass, so the added guard does not weaken the one it complements.
