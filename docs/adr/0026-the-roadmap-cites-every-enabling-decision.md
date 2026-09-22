# ADR-0026: The roadmap cites every decision that claims to enable it

- Status: accepted
- Affects: `docs/roadmap.md`, `internal/roadmap_enablement_test.go`, `docs/adr/`
- Depends on: ADR-0020, ADR-0025
- Enables: Phase 7 (read-only governed memory)

## Context

ADR-0025 corrected the memory channel in the two assemblers that had never
adopted ADR-0020, and closed with the store still unbuilt. Before starting it,
the same probe that produced the previous five findings was pointed at the
artifact the store work would actually be planned from: not the code, but the
document a reader opens to decide what Phase 7 still needs.

`docs/roadmap.md` narrated five settled prerequisites and cited ADR-0020 through
ADR-0024. The ADR corpus declared **six** records with `Enables: Phase 7`.
ADR-0025 was not among the citations.

The count is not the defect. The defect is what the uncited narration says. Its
first paragraph reads:

> ADR-0020 decided the channel retrieved memory arrives on, and the preparer
> presents it as a user-role message rather than folding it into the system
> message.

That sentence describes one assembler of three, and ADR-0025 exists precisely
because believing it is what let `provider.buildMessages` and
`host/v1.textSystem` fold memory into the system message through four
subsequent ADRs. So the roadmap did not merely omit a decision — it preserved,
as current planning guidance, the exact belief under which the defect survived.
A reader designing retrieval from that document would conclude the channel was a
solved single-site property and wire a store to one presentation path of three.

### Why nothing caught it

`internal/roadmap_test.go` holds the roadmap to the code in one direction: a
phase with an implementation witness in the tree must carry a status line. That
check is sound and it is deliberately narrow — it asserts the *presence* of a
marker, never the *content* of one. A status line that is present, detailed,
argued and wrong satisfies it completely.

Nothing connected an ADR's `Enables:` header to the phase it names. The header
was prose pointing one way, and the roadmap was prose pointing the other, with
no assertion in between. Both documents were individually well-formed.

This is the third instance of one shape, and the shape is now worth naming:

- **ADR-0024** — a guard with no caller. `MemoryReceipt.Validate` existed and
  nothing invoked it.
- **ADR-0025** — an assertion with no subject. The channel was asserted at the
  wire against hand-built `turn.Request` literals, never against the code that
  assembles messages in production.
- **ADR-0026** — a narration with no source. The status text was written by hand
  and nothing tied it to the decisions it summarizes.

In all three the decision was published and correct; what failed was the link
between the guarantee and the evidence for it. Each was found by measuring the
output of the previous turn rather than reading it.

## Decision

**A phase must cite every ADR that declares it enables that phase.** An ADR
asserting `Enables: Phase N` is a claim on Phase N's plan, and a plan that does
not mention the claim is planning without it.

`internal/roadmap_enablement_test.go` derives the pairs on every run — ADR
numbers and their `Enables:` headers from `docs/adr/`, phase bodies from the
roadmap headings — and fails when a phase body does not reference an ADR that
named it. Derivation is the point: a hardcoded table of pairs would have to be
updated by the same author who forgot the citation, so it would encode the
omission rather than detect it.

Phase 7's status gains a sixth prerequisite paragraph recording what ADR-0025
settled, and **correcting the first paragraph's claim**. It also records the two
facts the store work must not rediscover:

- The channel is a property of **every assembler**, not of the preparer. Routing
  the legacy paths through `internal/contextprep` is the right long-term answer
  and is deliberately not done (ADR-0025), so a retrieval design must assume
  more than one presentation path exists.
- Receipt guarantees reach **only the durable path**. `turn.Request` has no
  receipt field, so the exit evidence that "every influence identifies its
  source and version" holds for prepared contexts and has no representation on
  `SpawnTurn` at all.

The check asserts citation, not correctness. It cannot tell a true summary from a
false one — the roadmap's first paragraph proves a false one is possible. What it
can guarantee is that no decision goes **unmentioned**, and that is the failure
mode that matters here: a wrong summary is visible to a reader comparing it with
the ADR, while a missing one gives the reader nothing to compare.

## Consequences

Writing an ADR with an `Enables:` header now obliges a roadmap edit in the same
change. That coupling is the intent — the alternative is what just happened,
where the decision landed and the plan silently aged.

The ADR header block acquires a checked vocabulary: `Status`, `Affects`,
`Depends on`, `Enables`, `Origin`. Adding a field means adding it there. This is
a real constraint accepted for a measured reason, recorded below.

Phase 7's status section is now long. It is kept in full rather than compressed
because each paragraph records a defect found by measuring the previous one, and
the compressed version of that history is exactly the sentence that turned out to
be wrong.

## Discarded alternatives

**Assert the roadmap summary is accurate, not merely present.** The stronger and
more useful check, and not expressible: "the preparer presents it as a user-role
message" is false in a way no string comparison can detect. Attempting it would
produce a test that appears to verify accuracy while verifying wording, which is
worse than a narrow check that is honest about its scope.

**Hardcode the ADR-to-phase pairs.** Simpler and self-defeating, per the
derivation argument above.

**Check bidirectionally — every ADR a phase cites must declare it enables that
phase.** Rejected: phases legitimately reference decisions they build on without
those decisions being about them. Phase 7 cites ADR-0013's barrier as context.
The one-way direction is the one where silence is indistinguishable from absence.

**Drop `Enables:` from the ADR template instead.** Removes the inconsistency by
removing the claim. The header is the only place an ADR states what its decision
unblocks, and ADR-0020 through ADR-0025 are all prerequisite work for an unbuilt
phase — deleting it would discard the connective tissue of the entire memory
sequence to avoid checking it.

## How it is verified

`TestEveryEnablingDecisionIsCitedByThePhaseItEnables` derives every
(ADR, phase) pair from the corpus and asserts the citation, failing with the
consequence and the remedy named.

Mutation-verified five times, all five failing: removing the ADR-0025 citation
from Phase 7 fails; pointing an `Enables:` header at a nonexistent phase fails;
stripping the header from all six memory ADRs fails the anti-vacuity guard
rather than passing over an empty set; and moving the citation into the
neighbouring Phase 8 body fails, so proximity in the document is not accepted as
citation.

The first version of this test **survived** a mutation, and the fix is the
substance of the header-vocabulary constraint above. Renaming `Enables` to
`Unblocks` in one ADR made that ADR invisible to the check and the whole test
passed — a header nobody matches is indistinguishable from a header nobody
wrote, which is ADR-0025's surviving-mutation shape reproduced inside the test
written to catch that shape. The vocabulary is now enumerated and an
unrecognized field fails closed, on the same reasoning ADR-0023 gives for
receipt kinds.

Scoping that scan was itself measured, not assumed: applied to whole documents
it matched prose bullets such as ADR-0017's "- Existing runs are unaffected:"
and reported a dozen false violations. A check that noisy gets weakened, so it
reads only the block above the first `## ` heading. The enumerated set is the
measured vocabulary of the corpus — `Status`, `Affects`, `Depends on`, `Enables`
and `Origin` — rather than the four fields this author expected to find. The
per-field frequencies this sentence originally froze were wrong when written and
drifted further with each record added; they are now derived on every run
instead of narrated here, per ADR-0030 and `TestHeaderVocabularyMatchesTheCorpus`.
