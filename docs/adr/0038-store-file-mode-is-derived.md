# ADR-0038: The published file-mode integrity floor is a derived family invariant, not a per-store comment

- Status: accepted
- Affects: `internal/store_file_mode_test.go`
- Depends on: ADR-0026, ADR-0035, ADR-0036, ADR-0037

## Context

Every content store in this project stamps a permission mode on the record it
publishes, and each decides that mode by hand and explains it in a comment that
references the others:

- `rolestore/store.go` — "CreateTemp makes the file 0600. A role is not a
  secret ... leaving it unreadable would make it behave differently from every
  other file in the tree." Chmods `0644`.
- `agentstore/store.go` — "An agent definition is not a secret -- it is a file
  the whole team is expected to read and commit." Chmods `0644`.
- `modelstore/store.go` — "0600 and not 0644, unlike a trigger. This file names
  the variable holding an API key and the endpoint it is sent to: that is a map
  to a credential, and there is no reason for another user on the machine to
  read it." Chmods `0600`.

Probed at its widest point — every content store in the family: `rolestore`,
`agentstore`, `toolstore`, `trigstore` and `evalstore` publish `0644`;
`modelstore` publishes `0600`; `memorystore`'s `O_EXCL` create asks for `0644` —
the behaviour is correct everywhere.

But the family invariant was guarded unevenly, which is worse than not at all
because it looks covered. Four stores asserted their own published mode in their
own test (`agentstore`, `trigstore`, `modelstore`, `rolestore`); `toolstore`,
`evalstore` and `memorystore` asserted nothing, and no test derived the rule
over the corpus. So a store added later, or one whose chmod was widened to `0666`
or dropped for an umask-honouring `os.Create`, was caught by no test at all —
and the consequence is not cosmetic: a tool policy or an agent definition
writable by another local account is a file that account can rewrite behind the
owner's back, changing what an agent is allowed to do. This is the shape
ADR-0035, ADR-0036 and ADR-0037 recorded on the three turns before it: a
family-wide property argued in comments, generalised to N packages, and pinned
by a hand list that goes stale precisely in the store nobody added to it.

## Decision

**The published mode is enforced by reading the mode each store's own publish
stamps on the file and holding it to an integrity floor, not by trusting the
comments' claim about each other.** A single guard parses every `internal/*store`
package that declares `const ext`, reads the mode set by `os.Chmod` (its second
argument), `os.WriteFile` (its third) and `os.OpenFile` when its flags contain
`O_CREATE` (its third), and requires that mode to clear the floor: the owner can
read and write it, nobody can execute it, and neither group nor other can write
it.

`os.OpenFile` is counted only when it creates the file, because without
`O_CREATE` the kernel ignores the perm argument — reading a read path's `0` as a
mode would flag it for a mode it never sets. `os.MkdirAll` and `os.Mkdir` are
deliberately not matched: `0755` is correct for a directory and would fail the
no-execute test written for files. Calls are classified by selector name for the
reason ADR-0037 gives: the receiver varies but the operation does not.

The subject is derived for the reason ADR-0026, ADR-0035, ADR-0036 and ADR-0037
give: the property holds across the family and is restated in every store, so a
hand-listed copy of it in a test is one more thing to keep in step by hand —
which is the failure, not the fix. Reading the mode the store actually sets ties
the guard to the code it guards, so a widened mode fails the first time the suite
runs, and a store whose mode-setting call moved or was renamed yields no mode and
**fails closed** naming that store, rather than passing over an empty set.

**The floor guards integrity, not secrecy, and that scope is deliberate.** It
does not separate `0600` from `0644`, so it does not by itself hold
`modelstore`'s credential file to `0600`; reverting that file to `0644` clears
the floor and this guard stays green. That read-secrecy choice is a per-store
decision guarded by `modelstore`'s own test. What every store shares regardless
of the `0644`-vs-`0600` split — and what this derives over the family — is that a
stored record is data the owner writes, never a program and never a file another
local account may rewrite. Stating the narrower subject the guard actually
measures is the point, per this project's rule to check a claim at the width it
holds, not to generalise it.

The corpus is the same family as ADR-0035, ADR-0036 and ADR-0037: the
ext-declaring content stores. `jobstore` and `logstore` do not declare `const
ext` and publish through a different shape; they are out of this guard's scope by
that boundary, stated as a measured limit rather than an exemption.

## Consequences

The floor no longer rests on the stores' comments about each other. A store
added later, or an existing publish widened, is caught with a message naming the
file, the function, the consequence (an executable record, or one another local
account may rewrite behind the owner's back) and the remedy (publish `0644`, or
`0600` where the file maps a credential). The three stores that had no mode test
at all — `toolstore`, `evalstore`, `memorystore` — are now covered by the same
derivation as the rest.

The check is exactly as strong as "owner read/write, no execute, no group/other
write", and no stronger. It does not restate `modelstore`'s `0600`, it does not
prove the mode survives the runtime umask, and it does not prove the OS enforces
the bits — those are properties of the platform and of `modelstore`'s own test,
not of the mode literal in the source. It closes the one gap that spanned the
whole family with no derived guard: the integrity floor, which the stores' own
comments treated as obvious and which three of them held with no test.

This ADR adds no production code. It records that a family-wide file-mode floor,
asserted across the stores in comments and tested in only four of seven, was
carried mostly by prose, and replaces that with a guard derived from the writes
themselves — the fourth derived family invariant over this same corpus, after
locality (ADR-0035), temp-suffix atomicity (ADR-0036) and durability order
(ADR-0037).

## Discarded alternatives

**Add the missing per-store mode test to `toolstore`, `evalstore` and
`memorystore`.** Three edits that would raise the count from four to seven and
still leave the eighth store free to omit it — the exact state ADR-0036 found for
the temp suffix. The floor is identical across the family, so it is enforced
once, over the family, from the source.

**Derive which store holds a credential and require `0600` there.** This would
let the guard restate `modelstore`'s secrecy rather than only its integrity, but
"holds a credential" is not a property the source states structurally — it would
mean matching field names or comment text against a hand-tuned list of words like
`token` or `key`, the fragile heuristic this project's guidance warns against,
and it would misfire on a field like `TokenBudget`. The secrecy choice stays
`modelstore`'s own test's job; the family guard measures the floor it can derive
honestly.

**Enforce the exact set `{0600, 0644}` the family happens to use.** Tighter, but
it rejects a future `0640` that clears the real floor, turning a legitimate
choice into a failure and inviting the guard to be relaxed rather than a document
updated. The floor is the property the family actually needs; the guard checks
that property, and an unrecognized mode fails only when it breaks it.

**Trust the comments.** That is the state this ADR corrects. A floor asserted
across the stores and tested in four of seven is not guarded for the other
three, and a guard that would pass over a store publishing `0666` is
indistinguishable from one asserting nothing.

## How it is verified

`internal/store_file_mode_test.go`:

- `TestEveryStorePublishSetsAnIntegrityPreservingFileMode` — parses every
  file-mode-setting call in every store declaring `const ext` and requires the
  mode to clear the integrity floor (owner read/write, no execute, no
  group/other write). It proves its own analyzer against known-bad and known-good
  fixtures — a group/other-writable chmod must fail, a `0644` chmod and an
  `O_CREATE` `OpenFile` at `0644` must pass, and a read-only `OpenFile` must
  contribute no mode so its ignored perm is never read as a violation — before
  trusting the corpus, and fails closed per store when a store declaring `const
  ext` yields no mode at all, the case where the chmod moved and the scan
  silently went empty.

Confirmed to fail closed by mutation: setting a rename store's mode to `0755`
(executable), widening `memorystore`'s `O_EXCL` create to `0666` (group/other
writable), and dropping `agentstore`'s chmod entirely each fail the guard naming
the offending file and function — the file-mode regressions no test caught before
this existed.
