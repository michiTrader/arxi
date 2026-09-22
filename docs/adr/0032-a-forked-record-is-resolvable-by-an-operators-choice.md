# ADR-0032: A forked record is resolvable by an operator's choice, not permanently contained

- Status: accepted
- Affects: `internal/memorystore`
- Depends on: ADR-0027, ADR-0028, ADR-0031
- Enables: Phase 7 (read-only governed memory)

## Context

ADR-0028 and ADR-0031 made a fork *contained and visible*: retrieval excludes a
forked record, `Forks()` names it, and `tip` refuses it rather than choosing a
version silently. That is the right containment. What none of those records
provided is a way back: how a contained fork returns to a single current version
so the record is usable again.

Probing that gap, the way each memory turn has probed the last, found that there
was no way back at all, and that the refusal message actively named one that does
not work. `Fork.err` told the operator to "resolve it ... by superseding the ones
that are wrong". A probe carried out that instruction and measured the result,
not supposed it:

```text
Approve("r1","body A"); import a second root "body B"     -> r1 forked
Correct("r1", ...)  -> refused (tip refuses a forked record)
Delete("r1", ...)   -> refused (tip refuses a forked record)
Put superseding one of the two heads by hand -> SUCCEEDS, and r1 is STILL forked
```

Two facts make the fork permanent. First, `Correct`, `Delete` and `Promote` all
resolve the tip through `tip`, which refuses a forked record outright, so none of
the store's own verbs will touch one. Second — and this is the load-bearing one —
**supersession has exactly one parent, so appending a version can never reduce a
record's head count.** It turns one head into a non-head and adds a new head: a
net change of zero. A record with two heads has two heads after any number of
hand supersessions. The remedy the error printed was not merely awkward; it was
impossible, and following it left the operator with the same fork and one more
version on disk.

This is the same shape this corpus keeps finding in its own text: a remediation
string that names a repair the code cannot perform. ADR-0029 found it in the
claim message that named an absent successor; this is it in the fork message that
names a supersession that cannot heal.

## Decision

**A fork is resolved by an operator naming the one current version to keep; the
store retires the rest in a single append.** `Resolve(recordID, keepVersionID,
origin)` appends one version that supersedes the kept head and names every other
current head of the record in a new `Retires []string` field. A retired version
is no longer current even though nothing supersedes it, so the head count drops
from many to one and the record reads like any healthy one.

`Retires` is the multi-head edge that `Supersedes` cannot be. Supersession stays
one-parent — it is the correction chain, and ADR-0021's version identity and
ADR-0028's per-predecessor claim both rest on a version having exactly one
predecessor. Resolution is the only operation that retires more than one head,
and it is rare and operator-initiated, so it gets its own additive field rather
than pluralizing the edge every correction walks. The field is `omitempty` and
empty on every non-resolution write, so it never enters the identity of an
ordinary version and no existing version ID changes.

The definition of "current" now lives in one place, `headsByRecord`: a version is
a head when nothing supersedes it **and** no resolution retired it. Both `tips`
(which detects forks) and `Resolve` (which must enumerate the heads to retire)
read heads from that one function, so detection and resolution cannot disagree
about whether a retired version still counts.

**The store never picks the survivor.** Which version to keep is exactly the fact
ADR-0027 and ADR-0031 refused to guess — every tiebreak on version ID, mtime or
file order is deterministic and unrelated to which version the user meant, and
choosing one silently is the loss this store exists to prevent. So the survivor
is a required argument. `Resolve` carries the kept head's body, scope, kind and
deletion state forward verbatim: it decides which version wins, never what it
says. Keeping a tombstone is allowed, so an operator may resolve a fork by
deciding the record stays deleted, and doing so does not resurrect it.

## Consequences

A contained fork is no longer terminal. `Correct`, `Delete` and `Promote` still
refuse a forked record — that containment is unchanged — but `Resolve` now brings
it back to one head, after which those verbs work again. `Fork.err` names
`Resolve` and states why superseding by hand cannot heal a fork, so the
remediation matches what the API can do.

Resolution is not repeatable. Once a fork is healed the record has one head, so a
second `Resolve` finds no fork and is refused rather than appending a redundant
version — which is what stops a double resolution from re-forking what it just
healed. A survivor that is not a current head, a record that is not forked, and a
record that does not exist are each refused with a message naming the reason.

Two operators resolving the same fork toward **different** survivors
concurrently each supersede a different head, so both writes land and the record
re-forks — contained at read exactly as two concurrent first writes are
(ADR-0031), and resolvable again. The supersession claim serializes the common
case, two resolutions toward the **same** survivor: the second finds the survivor
already claimed, and content-addressing makes the identical resolution version a
no-op rather than a second one. Preventing the different-survivor race would mean
claiming every retired head as well, which is left out deliberately: it is more
machinery for a two-operator race that read-side containment already catches, and
the containment is the backstop ADR-0028 argued a single process cannot replace.

The retired versions stay on disk. Resolution retires a head by naming it, never
by unlinking it, so a receipt already issued for a retired version still resolves
to the bytes it named — the append-only immutability ADR-0021 rests on is not
weakened to clean up a fork.

## Discarded alternatives

**Let `Correct` operate on a forked record by superseding its "latest" head.**
Reintroduces the silent tiebreak ADR-0031 removed — "latest" is an mtime or
version-ID order unrelated to intent — and cannot reach one head anyway, because
a one-parent supersession leaves the head count unchanged. The whole reason a new
verb exists is that the correction path structurally cannot do this.

**Resolve by physically deleting the losing version files.** Removes the fork
from this copy of the store and from nothing else, the exact argument ADR-0027's
tombstone reasoning makes against unlinking: a replica or backup that still holds
the bytes reintroduces the head on the next read, and a receipt issued for the
deleted version no longer resolves. A retirement that travels with the data as a
positive assertion is the same shape as a tombstone, and for the same reason.

**Pluralize `Supersedes` into a multi-parent edge instead of adding `Retires`.**
Every correction walks the supersession edge, and ADR-0028's claim reserves one
predecessor for one successor; making the edge many-parent would change the
identity of every version and the meaning of the claim for the sake of one rare
operation. A separate, additive field keeps the common path and its invariants
untouched.

## How it is verified

`internal/memorystore/resolve_test.go`:

- `TestResolveHealsARootFork` — a root fork is brought to one current version by
  keeping the named head; the record reads healthy afterward, keeps the
  survivor's body, and the retired head remains on disk for its receipts.
- `TestResolveHealsASupersessionFork` — the multi-successor fork ADR-0028 first
  saw is resolved the same way.
- `TestResolveRefusesAVersionThatIsNotAHead` — a survivor that is not a current
  head is refused, so resolution cannot report success while leaving the fork.
- `TestResolveRefusesAHealthyRecord` — a record with one head has nothing to
  resolve, and the message names `Correct`.
- `TestResolveRefusesAnUnknownRecord` — a missing record is `ErrNotFound`.
- `TestResolvingAnAlreadyResolvedForkIsRefused` — a second resolution is refused,
  so a double resolution cannot re-fork a healed record.
- `TestResolveKeepingATombstoneLeavesRecordDeleted` — keeping a tombstone
  resolves the fork without resurrecting the record.

The retired-exclusion in `headsByRecord` was confirmed to fail closed by
mutation: removing it leaves the record forked after `Resolve`, and the heal
tests fail naming the still-forked record.
