# ADR-0040: The atomic-rename publish closes the temp before renaming it, a derived family invariant

- Status: accepted
- Affects: `internal/store_temp_close_test.go`
- Depends on: ADR-0026, ADR-0034, ADR-0037, ADR-0039

## Context

Every store in this project that publishes a record by renaming a temp over its
final name writes the same sequence: `os.CreateTemp`, write, `tmp.Sync()`,
`tmp.Close()`, `os.Chmod`, `os.Rename`. `agentstore`'s comment calls that
sequence "toolstore's, for the same reasons" — a cross-package claim about the
whole family.

One step in it is not a durability choice but a portability one: the temp is
**closed before it is renamed**. On Windows `os.Rename` refuses a file that is
still open and fails with a sharing violation
(`ERROR_SHARING_VIOLATION`, `syscall.Errno(32)`). This is not hypothetical here:
`logstore/pending_remove_windows.go` already retries around exactly that error
(`windowsSharingViolation = syscall.Errno(32)`) when removing a pending marker,
so the constraint is demonstrated and live in this repo. Rename the temp while
its handle is open and the publish never completes on Windows — while every test
on a POSIX runner, where renaming an open file is fine, stays green.

Probed at its widest point — every store that publishes through
`CreateTemp`+`Rename`, plus `logstore`'s snapshot — the close-before-rename step
is correct everywhere. But nothing guarded it. Moving the rename ahead of the
close changes no durability property, so ADR-0037's fsync-order guard stays
green, and on Linux CI the behaviour itself is fine, so the whole suite stays
green. The failure surfaces only on the user's Windows machine, as a role or an
agent that cannot be saved. This is the shape ADR-0035 through ADR-0039 recorded
on the five turns before it: a family-wide property carried by the shape of a
call and pinned by no derived guard.

## Decision

**The close-before-rename order is enforced by reading each store's own `Close`
and `os.Rename` and holding the last close before the first rename, not by
trusting the sequence to stay right.** A single guard parses every
`internal/*store` package; for each function that both creates a temp and renames
it, it collects the positions of every `.Close()` call and every `os.Rename`
call and requires the last close to precede the first rename.

The last close is the right bound because every close in a correct publish sits
before the rename: the error-path closes after a failed `Write` or `Sync`, and
the final close on the success path. Requiring `max(close) < min(rename)` holds
the whole function to "closed before renamed" and flags a rename moved ahead of
the close, while the error-path closes that precede the success-path close do not
trip it. A rename-publish with no close at all fails closed, naming the function,
rather than passing over a form the analyzer did not recognise. Calls are
classified by selector name for the reason ADR-0037 gives: the receiver varies
(`tmp`, `f`) but the operation does not.

The subject is derived for the reason ADR-0026, ADR-0037 and ADR-0039 give: the
property holds across the family and is expressed in every store's own two calls,
so trusting the sequence to stay right is one more thing to keep in step by hand.
Reading the calls ties the guard to the code it guards, so a rename moved ahead
of the close fails the first time the suite runs — including on the Linux CI
where the runtime itself would not fail.

**The corpus is the rename-publishers, the same measured boundary as ADR-0039:**
every store that publishes through `CreateTemp`+`Rename`. That boundary includes
`logstore`'s snapshot and excludes `memorystore` (an `O_EXCL` content-addressed
create with no rename, ADR-0034) and `jobstore` (an append-only journal, no
`CreateTemp`+`Rename`). The boundary is stated as what the guard measures, not
enumerated by hand.

## Consequences

The one portability step in the family's atomic write no longer rests on the
shape of a call nobody checks. A store added later, or an existing publish whose
rename is moved ahead of its close, is caught with a message naming the file, the
function, the consequence (on Windows `os.Rename` fails with a sharing violation
on the open handle and the publish never completes) and the remedy (close the
temp, checking the error, before `os.Rename`). Crucially, it is caught on the
Linux CI this project runs on, not left to surface on a user's Windows machine.

The check is exactly as strong as "the last `.Close()` call precedes the first
`os.Rename` call in source", and no stronger. It does not prove the closed handle
is the temp's rather than some unrelated file's — in these publish functions the
only file is the temp — and it does not execute the store to observe the rename;
it reads the order the source states, which is where the regression it guards
would appear. It closes the last step of the publish sequence that ADR-0037 and
ADR-0039 left underived: ADR-0037 the fsync order, ADR-0039 the temp's directory,
and this the close before the rename.

This ADR adds no production code. It records that the close-before-rename
step — a Windows correctness requirement, not a durability one, and the reason a
publish completes at all on that platform — was carried only by the shape of each
store's write, and replaces that with a guard derived from the calls themselves:
the sixth derived family invariant over these stores, after locality (ADR-0035),
temp-suffix atomicity (ADR-0036), durability order (ADR-0037), the file-mode
floor (ADR-0038) and the same-directory rename (ADR-0039).

## Discarded alternatives

**Add a per-store test asserting the close order in each publish.** Seven edits
that would still leave the next store free to omit it — the exact state ADR-0036
found for the temp suffix and ADR-0038 for the mode. The order is identical
across the family, so it is enforced once, over the family, from the source.

**Rely on a Windows CI run to catch it.** There is no CI at all here — the
GitHub App cannot write `.github/workflows/`, so `scripts/verify.sh` is run by
hand, and in practice on whatever platform the contributor has. Even with CI, a
Linux-only run would never see this failure, and a Windows run would catch it
only after the fact, as a red build rather than a named consequence at the line
that caused it. A static guard names the defect on every platform.

**Fold it into ADR-0037's durability-order guard.** Tempting, since both read the
publish sequence, but they guard different properties for different reasons:
ADR-0037 is about surviving a crash (fsync order), this is about running at all
on Windows (handle released before rename). Merging them would blur two decisions
whose rationales and failure modes are distinct, and this project keeps one
decision per record so a later reader can see exactly what each protects.

**Trust the sequence.** That is the state this ADR corrects. The order held in
seven publishes with no test anywhere, and a guard that would pass over a rename
issued on an open handle is indistinguishable from one asserting nothing.

## How it is verified

`internal/store_temp_close_test.go`:

- `TestEveryStoreRenamePublishClosesTheTempBeforeRenaming` — parses every
  `CreateTemp`+`Rename` publish in every store and requires the last `.Close()`
  to precede the first `os.Rename`. It proves its own analyzer against fixtures
  before trusting the corpus — a correct close-then-rename (with error-path
  closes before the success-path one) must pass, a rename-before-close must fail,
  and a rename with no close must fail closed — and fails closed over the whole
  corpus if no `CreateTemp`+`Rename` publish is found at all, the case where the
  write moved and the scan silently went empty.

Confirmed to fail closed by mutation: moving the rename ahead of the close in
`agentstore`'s `write` and in `modelstore`'s `write` (the credential store) each
fail the guard naming the offending file and function — the Windows
sharing-violation regression that no test, on the Linux runner this project uses,
would otherwise catch.
