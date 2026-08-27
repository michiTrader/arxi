# iash

Agent systems you can actually debug.

The thesis of the project fits in one line:

> **A single pure function produces `run`, `--sim`, `replay` and `why` as the
> same machinery.**

Everything else is a consequence of that.

## The core

```go
Decide(State, Event, Config) -> (State', []Effect)
```

Pure. It does not look at the clock, does not touch the network, does not write
anything. Everything it wants to happen in the world it **describes** as an
`Effect` and returns; somebody else runs it.

That restriction is not aesthetic. It is what makes four features be one:

| feature | what it is |
|---|---|
| `iash run` | fold + real executor |
| `iash run --sim` | fold + fake executor |
| `iash run replay` | fold over an old log, with no executor |
| `iash run why` | read the `State` that came out of the fold |

In a design where the reducer calls the network, `replay` is a second program
that reimplements the logic of the first. It is always out of date and nobody
notices until the moment they need it.

And the purity is **verified, not promised**: `internal/arch_test.go` runs
`go list` over the package and fails if the kernel imports `time`, `net`, `os`
or `math/rand`.

## What makes it different

**It detects silence.** The most expensive failure mode is not the one that
screams: it is the run that does not fail, does not finish and does not advance.
`iash` detects it and emits `run.quiescent` with a diagnosis that names the
advance rule that is not met — including the hard case, where everybody
submitted and the rule is still unsatisfiable (ADR-0004).

**The diagnosis is derived, not hard-coded.** `run why` reads structured
references (`blocked_ref`) and produces executable remedies. There is no `case`
per blueprint:

```
$ iash why runs/r1/state.json
run r1: running
└─ backend: waiting (approval) since seq 5
   └─ waits for approval of the tool "bash" (inbox inbox-1)
└─ budget: 0.4200 of 5.0000 USD spent in the tree

possible remedies:
  $ iash inbox approve inbox-1
  $ iash agent tool policy --agent backend --allow bash
```

**The defaults are security decisions.** If any member can write files, the
workspace becomes a `worktree` per member: two agents in the same directory
overwrite each other and the result is garbage that is hard to attribute. A
stage timeout escalates, it does not fail — failing by default trains the user
to set absurd timeouts. Mutating tools do not authorize themselves.

**Spending is auditable.** The budget belongs to the tree (`TreeSpentUSD`), so a
nested spawn cannot multiply the ceiling of the root. And when N causes are
merged into one turn, those N stay recorded in `SpawnTurn.Coalesced`: a saving
you cannot audit is a saving nobody is going to believe.

**One declaration, three surfaces.** Every capability is declared once and
projected by a pure string join — with no translation table to drift out of
sync:

| declaration | CLI | tool | protocol |
|---|---|---|---|
| `["run","start"]` | `run start` | `iash_run_start` | `run.start` |

There are **45 declared capabilities**, of which **32 are exposed as tools** to
the agents. The difference is not an oversight: there are things a human can do
from the terminal that an agent should not be able to do to itself. `iash
surface` shows all of them; `iash schema` emits the manifest an agent consumes.

## Status

What runs today:

```
iash schema        emit the surface manifest (JSON)
iash surface       see the whole surface, human readable
iash why <file>    explain why a run is not advancing
iash version       version of the binary and of the surface
```

The rest of the surface is **declared and verified by tests, but has no
executor**. The CLI is honest about it: for a command that is declared and not
implemented it tells you so, with its tool name and its protocol type, instead
of lying with "unknown command".

Missing: the executor (`internal/exec`), blueprint loading, `serve` (NDJSON
protocol), triggers and eval.

## Build and test

Requires Go 1.22.

```bash
go build -o iash ./cmd/iash
go test ./...
```

The tests are **not optional**. Go does not give exhaustive `match`, so a
`switch` over `Effect` that is missing a variant still compiles; the net that
catches it is `go test` (ADR-0007). For the same reason, every test protects a
decision and its failure message names the consequence and the remedy — a
`t.Fatal("mismatch")` does not meet the contract of this project.

To regenerate the golden:

```bash
UPDATE_GOLDEN=1 go test ./internal/kernel
```

## Where to read

| path | what is there |
|---|---|
| [`docs/design/20-use-cases.md`](docs/design/20-use-cases.md) | every command, reached by walking eleven realistic scenarios |
| [`docs/design/10-execution.md`](docs/design/10-execution.md) | the full execution model |
| [`docs/adr/`](docs/adr/) | why each decision, and what breaks if it is reverted |
| [`spec/events.md`](spec/events.md) | the event catalogue and the `blocked_ref` contract |

Start with the use cases if you want to know what the tool *does*, and with the
ADRs if you want to know why it is built this way. The use-case document is
enforced by tests: a capability no scenario reaches, or an example using a verb
that does not exist, fails the build.

The ADRs are the best entry point: each one says what was decided, which
alternative was discarded and **which test enforces the decision**.

## Code layout

```
internal/kernel/    the pure reducer: Decide, State, Event, Effect, Explain
internal/surface/   the surface declared once, and its three projections
internal/arch_test  the architectural boundaries, verified with go list
cmd/iash/           the CLI
spec/               event contracts
docs/design/        the execution model and the use cases
docs/adr/           one file per decision that cannot change quietly
```

The kernel imports nothing from the rest of the project. It is the only layer
that has to stay pure, and there is a test that enforces it.
