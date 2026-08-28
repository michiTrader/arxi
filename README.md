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
iash run start <bp> <prompt> --budget N --sim   run a blueprint to completion
iash serve [--listen ADDR]      speak the NDJSON protocol; stdio without --listen
iash schema                     emit the surface manifest (JSON)
iash surface                    see the whole surface, human readable
iash why <file>                 explain why a run is not advancing
iash blueprint validate <file>  check a blueprint and print the resolved config
iash version                    version of the binary and of the surface
```

Underneath, every package is done and tested — **249 tests, no dependencies**.
The count is of **cases reported by `go test -v`, subtests included**, which is
what `go test -run` can address individually:

```bash
go test -v ./... 2>&1 | grep -c '^=== RUN'
```

The convention is stated because it is the only reason the number is checkable.
An earlier version of this table counted subtests for two packages and top-level
functions for the rest, and the total it summed to was a number no command could
reproduce — a figure like that cannot be shown to be wrong, so it drifts.

| package | what it owns | tests |
|---|---|---|
| `internal/kernel` | the pure reducer: `Decide`, `State`, `Effect`, `Explain` | 41 |
| `internal/exec` | the run loop, the effect runner, the fake executor, the clock | 57 |
| `internal/logstore` | the append-only log, `seq` assignment, CAS on `seq` | 33 |
| `internal/blueprint` | YAML loading, validation, and freezing by digest | 59 |
| `internal/surface` | the capability manifest every command is checked against | 20 |
| `cmd/iash` | the CLI and the NDJSON protocol server | 33 |
| `internal` (arch) | that the kernel stays pure, and that no effect is unhandled | 6 |

`blueprint validate` prints the config **as resolved**, not the file read back.
Most of what it shows the user never wrote:

```
$ iash blueprint validate ./examples/feature-team.yaml
blueprint feature-team is valid (2 stages, 3 members)
  workspace: worktree  (resolved: backend and frontend can write)
  stage build: advance_when=all on_timeout=escalate timeout=30m
  stage review: advance_when=any on_timeout=escalate
  security is advisory: gives an opinion, does not count toward advance rules
  watcher security on run.quiescent: notify
  sha: 44c08e284a9c
```

That is deliberate: a default you cannot see is indistinguishable from a bug
when it fires. And it refuses blueprints that would fail *silently* — a
`quorum:5` over three members does not make the run fail, it makes it go quiet
after everybody has already been paid for.

The YAML parser is written by hand, over a deliberately small subset, so the
binary keeps shipping with no runtime and no dependencies. It refuses anything
outside that subset **by name**, with the line and the fix, because a parser
that guesses returns a config that looks plausible and is not what the file
says.

That blueprint is in the repo, at `examples/feature-team.yaml`, and it is the
one `run start` is invoked with below. An example that is not executable drifts
away from the code and nobody notices until somebody copies it.

`run start` is where the kernel, the log, the executor and the blueprint meet.
It loads and freezes the
blueprint, appends `run.started`, and then folds the log forward one step at a
time until the run reaches a terminal status or goes quiet:

```
$ iash run start ./examples/feature-team.yaml "add rate limiting" --budget 2.00 --sim
run rmtbqzimz-f393e8d7 started (budget 2.00 USD, workspace auto→worktree)
run rmtbqzimz-f393e8d7 succeeded (seq 21, stopped by reaching a terminal status)
  stage:  review
  turns:  4
  spent:  0.0400 of 2.0000 USD in the tree
  cursor: 21 (resume from here)
  log:    /tmp/.../events.ndjson
```

It prints the **cursor** because resuming is not a matter of reopening the log:
the loop's starting point has to be *given*. `Head()` would assume every event
was already acted upon, and `0` would assume none was — the first silently skips
the effect a stage is waiting for, the second silently pays for the same turns
twice. The cursor is also why the loop writes it *after* the step succeeds and
not before: lagging costs a re-executed turn, leading costs the whole run, quietly.

Without `--sim` the command **refuses**, and says why: there is no LLM-backed
`Executor` in this build, so a real run would print a plausible run id, a
plausible cost and a plausible result for work nobody did. `--sim` drives the
same reducer, the same log and the same loop; only the executor and the clock
are fake, so the run you get is the run you would get minus the model calls.
That is also what makes `replay` and `run why` work on a simulated log: the log
is the truth, and nothing in the reducer knows which executor produced it.

`serve` is the same surface with a different mouth. One request per line, one
response per line, NDJSON, in order:

```
$ iash serve
{"type":"hello","version":"0.0.1-spec","surface_version":1,"types":["agent.create",...],"implemented":["blueprint.validate","schema"]}
{"id":"1","type":"blueprint.validate","params":{"path":"./examples/feature-team.yaml"}}
{"id":"1","ok":true,"result":{"name":"feature-team","sha":"44c08e284a9c...","workspace":"worktree", ...}}
```

Without `--listen` it speaks over stdio, which is why the transcript above is
something you can pipe, `tee` and read with `cat`. A framed binary protocol
would need a tool written for it before anybody could see what went wrong, and a
protocol only its own client can read is a protocol nobody debugs.

Requests are answered **strictly in order**, one connection at a time. Handling
them concurrently would let a `state get` overtake the `state set` issued before
it, and the lost update would have been authored by the server, not by the
client that took care to serialize its own writes.

Two decisions in there are worth defending, because both look like extra work
until the day they are not.

**`unknown_type` and `not_implemented` are different codes.** The client has to
tell "your bug, retrying never helps" from "this build is behind, retrying after
an upgrade helps":

```
{"id":"2","type":"run.list"}   → not_implemented  "declared in surface v1 and this build has
                                                   no executor for it. The request was well
                                                   formed; retrying will not help until the
                                                   capability lands."
{"id":"3","type":"run.abort"}  → unknown_type     "not a message type in surface v1. The type
                                                   is the CLI path with dots: `run why` is
                                                   `run.why`."
```

Collapse those into one code and every client picks one wrong behaviour for
both: retry a typo forever, or abandon a capability that ships next week. A
capability deliberately held off the wire says so too — `design` answers "is a
real capability (`iash design`) and is not exposed to the protocol. That is
deliberate, not missing", because "unknown" would send the reader looking for a
misspelling that isn't there.

**Any address that is not `unix://` is refused.** There is no handshake and no
token, so the socket's file permissions *are* the authentication — it is created
`0700`, and a connected client can start runs and spend the budget. `--listen
tcp://0.0.0.0:9000` therefore does not bind; it explains that it would hand that
ability to whoever reaches the port. The check is an allow-list rather than a
list of refused schemes, for the same reason an undeclared `ToolPolicy` defaults
to deny: a scheme nobody has thought of yet is then refused by default instead of
by enumeration. This one was rewritten after mutation testing — the deny-list it
replaced had a `tcp://` clause that could be deleted with no test noticing,
since every `tcp://…` contains the colon the next clause already caught. Dead
code in a security guard is worse than redundant: it reads as two conditions
being enforced when one does all the work, so a later edit to the load-bearing
one looks harmless.

The dispatchable set is not a list inside the server. It is `ProtocolCommands()`,
filtering the registry on `Kind&Protocol`. A hand-kept list would be a second
surface, and the first time someone flags a new capability `Protocol`, `iash
schema` would advertise a type the server answers `unknown_type` to — the client
lied to by the one document it was told to trust.

Unknown and malformed parameters are refused, never ignored and never coerced.
`{"reasn":"x"}` on `run.cancel` is an error naming both what was sent and what
is accepted, because a misspelled `if_seq` that is silently dropped turns a
compare-and-swap into last-write-wins while the client still believes its write
was conditional. `{"budget":"2.00"}` is refused rather than coerced, since
coercion makes it `0` and the most cautious-looking request becomes the most
dangerous one.

The rest of the surface is **declared and verified by tests, but not wired to a
command**. The CLI is honest about it: for a command that is declared and not
implemented it tells you so, with its tool name and its protocol type, instead
of lying with "unknown command".

Missing: the live executor behind `run start` (the loop, the log and the
reducer are done and driven end to end by `--sim`; what is absent is the thing
that actually calls a model), triggers and eval.

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
