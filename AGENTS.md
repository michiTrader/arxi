# iash

Agent orchestration system in Go. The whole project rests on one thesis:

> **A single pure reducer produces run, simulation, replay and diagnosis as the
> same machinery.**

```go
Decide(State, Event, Config) -> (State', []Effect)
```

Pure: no clock, no network, no filesystem. Everything it wants to happen in the
world it **describes** as an `Effect` and returns; something else carries it out.
That constraint is why `iash run`, `iash run --sim`, `iash run replay` and
`iash run why` are one body of logic instead of four programs that drift apart.

Before touching code, read `docs/adr/`. Seven records, each one stating what was
decided, what alternative was rejected, and **what breaks if someone reverts it
without reading**. `docs/design/10-execution.md` has the execution model;
`spec/events.md` has the event catalog and the `blocked_ref` contract.

---

## Language policy

**EVERYTHING is written in English. No exceptions. No Spanish anywhere.**

The user of this project speaks Spanish and will often address you in Spanish.
**That changes nothing about what goes into files.** Conversation with the user
may be in Spanish; the repository is English-only. This is the single most
common way this rule gets broken — the language of the request leaks into the
language of the artifact.

This covers, without exception:

- **Identifiers** — variables, functions, types, constants, struct fields,
  packages, file names, directory names.
- **Comments** — including doc comments (godoc) and inline notes.
- **Test names and table-driven test case descriptions**, plus every `t.Fatalf`
  / `t.Errorf` failure message.
- **User-facing strings** — CLI output, usage text, error messages, diagnostics
  emitted by the reducer (`diagnosis` payloads, `why` output, remediation
  hints).
- **JSON field names and event type names** in the wire format and payloads.
- **Commit messages**, branch names, PR titles and PR descriptions.
- **All documentation** — ADRs, design docs, specs, README, and this file.

Before writing a single line, re-read this rule. If a comment, identifier,
string or commit message you are about to write would naturally come out in
Spanish, translate it **before it goes in the file**. Never write it in Spanish
"to fix later" — that is how the mixed state got here in the first place.

### Migration status

**The migration is done.** The repository was originally written in Spanish; a
dedicated pass translated all of it — code comments, diagnostic strings, CLI
usage text, test names and failure messages, ADRs, design docs and the README.
There is no grandfathered Spanish content and no file is exempt.

So the rule is no longer transitional, it is simply the steady state: **every
line you add or edit is English**. If you find Spanish anywhere, it is a
regression — report it and fix it in a focused commit.

One warning drawn from how that pass went wrong the first time around, because
it is the failure mode to watch for. Translation must be **read and rewritten**,
not substituted word by word. A mechanical pass produced two classes of damage
that compiled cleanly and kept every test green, so nothing caught it:

- **Comments replaced by a placeholder.** 540 argued comments were collapsed
  into `// Implementation note.` — the entire body of reasoning about why each
  decision is right and what breaks otherwise was deleted. Those comments are
  the primary defense against someone later "simplifying" a load-bearing
  decision. Losing them is worse than leaving them in Spanish.
- **Word-for-word substitution.** `no` became `not` and `es` became `is`
  regardless of context, yielding text like "with not runtime" and "nadie is
  working and nadie can empezar". Even flag names were corrupted:
  `go build -o` turned into `go build -or`.

If you are ever asked to translate anything here again: read the paragraph,
understand the argument, and write the English that makes the same argument with
the same force. Never map token to token, and never drop content you have not
replaced with an equivalent.

Do not translate unrelated content as a side effect of some other task — that
turns a small reviewable change into an unreviewable one.

### Why this matters beyond style

The failure messages in this project are load-bearing. Go does not give
exhaustive `match`, so a `switch` over `Effect` missing a variant compiles fine
and the test suite is the only net that catches it (ADR-0007). Those test
messages are the current documentation of the decision they protect — they must
name the consequence and the remedy, and they must be readable by every
contributor and every tool that ingests them.

---

## Comment policy

Comments explain **why a decision is right and what breaks otherwise**. Never
what the code does — the code already says that.

```go
// BAD: increments the counter
// GOOD: Only MemberIdle counts as runnable. Including MemberSubmitted here
// would break quiescence detection in the worst way: a member that already
// delivered would look wakeable forever, so the run would never be reported
// as stuck and the user would wait all night for nothing.
```

If a comment would still be true after the surrounding logic was rewritten
differently, it is describing behavior, not defending a decision. Rewrite it or
delete it.

## Test policy

Every test protects a decision, and its failure message names the
**consequence** and the **remedy**. `t.Fatal("mismatch")` does not satisfy this
contract.

```go
t.Fatal("two folds of the same log produced different states: replay is worthless")
```

Because the type system does not cover exhaustiveness or immutability here, the
test suite is part of the architecture, not a recommendation. **Nothing merges
without `go test ./...` passing.**

## Architectural boundaries

The kernel (`internal/kernel`) must stay pure. `internal/arch_test.go` runs
`go list` and fails if it imports `time`, `net`, `net/http`, `os`, `os/exec`,
`math/rand`, `crypto/rand`, `database/sql`, `io` or `bufio`. Do not work around
that test — it guards the assumption the entire design rests on. If you need the
clock, the answer is an event, not an import.

---

## Commit policy — MANDATORY, not exceptions

**Commit after every file you create or modify. Push after every commit.**

This is not process hygiene, it is data-loss prevention with a track record.
**This sandbox has already been reset three times on this project.** The first
time destroyed an entire turn of work that was committed locally but never
pushed. The times after that cost nothing, because the work was on the remote.
A commit that exists only in the local working copy is exactly as fragile as an
uncommitted change.

Concretely:

1. After every `Write`/`Edit`/`MultiEdit` call — or a very small, tightly
   coupled group of files forming one atomic change (a source file and the
   fixture it needs) — immediately `git add` + `git commit` with a descriptive
   conventional-commit message **in English**.
2. **Push after every commit**, or at worst after every small batch, so the
   remote branch is always close to current.
3. Do not batch a whole feature into one commit at the end. If something goes
   wrong mid-task, the last pushed commit is the only thing that survives.
4. Open the pull request **early** — as soon as there is one meaningful commit —
   and keep updating it. Do not wait for the whole step to be done.
5. Before ending a turn, verify `git status` is clean and that
   `git log origin/<branch>..HEAD` shows nothing unpushed.

### Recovering from a sandbox reset

If the working tree looks empty or reverted, the work is not lost — it is on the
remote. Do not rebuild from scratch:

```bash
git fetch origin
git checkout -B genspark_ai_developer origin/genspark_ai_developer
```

Go is **not** preinstalled and does not survive a reset:

```bash
cd /tmp && curl -sSLO https://go.dev/dl/go1.22.5.linux-amd64.tar.gz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.22.5.linux-amd64.tar.gz
export PATH=$PATH:/usr/local/go/bin
```

---

## Working rules

**One step at a time, in order.** Do not start the executor while blueprint
loading is half done.

**No new dependencies without justification.** The project is standard library
only. A CLI that ships as a single static binary with no runtime is a feature,
and every dependency is a claim against it. Justify it against that or leave it
out.

**Respect the frozen surface.** `internal/surface` declares 47 capabilities;
33 are exposed as agent tools. That gap is deliberate — some things a human may
do from a terminal an agent must not be able to do to itself. Adding a command
means implementing something already promised, not inventing a new promise.

The clearest example is `trigger run`, and it is worth knowing because it is the
only **transitive** exclusion: it is withheld from agents not for what it does
directly, but because it starts whatever every stored trigger's `--then` names.
Granting it would grant the union of every scheduled action.

These two numbers are checked by `TestEveryDocumentStatingTheSplitIsCurrent`.
They were stale for two capabilities before that test existed.

**Verify, do not assume.** Before reporting a number, measure it. Before saying
tests pass, run them. Counts, file contents and git state have all been wrong
from memory on this project.

**Correct the code, not the test.** When a test fails, the default assumption is
that the test is right. Three real reducer bugs were found this way, including a
broadcast steer that opened a billed turn for an advisory member nobody had
activated. Weakening the test would have hidden a bug that costs real money.

## Build and test

Requires Go 1.22.

```bash
go build -o iash ./cmd/iash
go vet ./... && gofmt -l .
go test -count=1 ./...
UPDATE_GOLDEN=1 go test ./internal/kernel   # regenerate golden fixtures
```
