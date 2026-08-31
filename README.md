# arxi

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

That restriction is not aesthetic. It is what makes four features be one fold
over the same reducer, differing only in what is plugged into it:

| feature | what it is | state |
|---|---|---|
| `arxi run --sim` | fold + fake executor | **works** |
| `arxi why` | read the `State` that came out of the fold | **works** |
| `arxi run start` | fold + real executor | fold works, executor absent |
| `run replay` | fold over an old log, with no executor | declared, not built |

The `state` column was added after this table was caught overclaiming. It listed
all four as features; `arxi run replay` and `arxi run why` both answer *"declared
in the surface but not implemented yet"*, and `Replay` appears nowhere in the
source except inside a comment. What is real is `arxi why` — a different command,
taking a state file rather than a run id — and the fold itself, which `--sim`
drives end to end.

The architectural claim survives that correction, and is worth separating from
the delivery claim: in a design where the reducer calls the network, `replay` is
a second program that reimplements the logic of the first, always out of date and
nobody notices until the moment they need it. Here it is a fold with the executor
left out, which is why it is a small job that has not been done rather than a
parallel implementation that has to be kept honest.

And the purity is **verified, not promised**: `internal/arch_test.go` runs
`go list` over the package and fails if the kernel imports `time`, `net`, `os`
or `math/rand`.

## What makes it different

**It detects silence.** The most expensive failure mode is not the one that
screams: it is the run that does not fail, does not finish and does not advance.
`arxi` detects it and emits `run.quiescent` with a diagnosis that names the
advance rule that is not met — including the hard case, where everybody
submitted and the rule is still unsatisfiable (ADR-0004).

**The diagnosis is derived, not hard-coded.** `run why` reads structured
references (`blocked_ref`) and produces executable remedies. There is no `case`
per blueprint:

```
$ arxi why runs/r1/state.json
run r1: running
└─ backend: waiting (approval) since seq 5
   └─ waits for approval of the tool "bash" (inbox inbox-1)
└─ budget: 0.4200 of 5.0000 USD spent in the tree

possible remedies:
  $ arxi inbox approve inbox-1
  $ arxi agent tool policy --agent backend --allow bash
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
| `["run","start"]` | `run start` | `arxi_run_start` | `run.start` |

There are **49 declared capabilities**, of which **33 are exposed as tools** to
the agents. The difference is not an oversight: there are things a human can do
from the terminal that an agent should not be able to do to itself. `arxi
surface` shows all of them; `arxi schema` emits the manifest an agent consumes.

## Status

What runs today:

```
arxi run start <bp> <prompt> --budget N         run a blueprint, calling real models
arxi run start ... --sim                        the same run with no model calls
arxi provider add <name> --base-url URL         register a provider
arxi model list | enable | disable              see and gate what may be called
arxi serve [--listen ADDR]      speak the NDJSON protocol; stdio without --listen
arxi schema                     emit the surface manifest (JSON)
arxi surface                    see the whole surface, human readable
arxi why <file>                 explain why a run is not advancing
arxi blueprint validate <file>  check a blueprint and print the resolved config
arxi version                    version of the binary and of the surface
```

Short flags exist, and they are the surface's, not each command's:

```
$ arxi run start -a ./examples/feature-team.yaml -p "add rate limiting" -b 2.00 -S
$ arxi surface --flags        # the whole assignment, and what each letter reaches
  -b  --budget         4 commands
  -r  --run            13 commands
  -p  --prompt         run start
  -f  --path           blueprint validate
```

One letter means one thing everywhere, or it does not exist. The two obvious
implementations both fail, and for the same reason the protocol's message list is
derived rather than written down.

Per-command aliases would let `-p` be prompt here and something else there.
That does not break loudly: `-p` keeps working, it just stops meaning what the
reader learned it meant.

Deriving the letter from the first character is worse. This surface has `budget`
and `base-url`, `to`/`tools`/`text`/`type`/`ttl`, `on`/`on-busy`/`on-missed`.
Auto-assignment gives the letter to whichever entry the registry lists first, so
`-b 5` is a spend ceiling today and a provider URL after somebody sorts the
file. A short flag whose meaning depends on the order of a slice does not fail
when it breaks — the script silently does something else with the number.

So there is one global name→letter map, and one expander that rewrites `-p` into
`--prompt` before any parser runs. A letter is only valid on a command that *has*
that parameter: `-r` is `--run` on the thirteen commands that take a run id and
an **error** on the rest, because binding it to something else would discard the
value while the command reported success. The refusal says so rather than
guessing:

```
$ arxi blueprint validate -r r1
-r is --run elsewhere in the surface, but blueprint validate has no run
parameter, so there is nothing for it to abbreviate here.
it accepts: -f (--path), -J (--json)
```

Booleans group (`-SJ`); a flag that takes a value does not. `-Sb 2` is refused
rather than guessed, because inside a group nothing says which letter the `2`
belongs to, and a parser that guesses about a spend ceiling is the failure
`--budget`'s mandatory-ness exists to prevent.

The NDJSON protocol has **no** short flags. A machine has no fingers to save, and
`{"b": 5}` in a log is a puzzle where `{"budget": 5}` is a fact.

Underneath, every package is done and tested — **940 tests, no dependencies**.
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
| `internal/kernel` | the pure reducer: `Decide`, `State`, `Effect`, `Explain` | 42 |
| `internal/exec` | the run loop, the effect runner, the fake executor, the clock | 57 |
| `internal/logstore` | the append-only log, `seq` assignment, CAS on `seq` | 33 |
| `internal/blueprint` | YAML loading, validation, and freezing by digest | 65 |
| `internal/surface` | the capability manifest every command is checked against | 31 |
| `internal/trigger` | schedules, what a trigger may invoke, and both halves of the firing decision | 152 |
| `internal/trigstore` | triggers on disk: one file each, written atomically | 27 |
| `internal/scheduler` | the tick: reads the store, asks `trigger`, starts and records | 31 |
| `internal/eval` | suite files, the fold over cases, and the denominators a pass rate is read over | 106 |
| `internal/evalstore` | runs on disk: never rewritten, never pruned, newest first by id | 28 |
| `internal/model` | which models may be called: exist, unambiguous, enabled — and what a turn costs | 44 |
| `internal/modelstore` | providers on disk: one file each, `0600`, written atomically | 19 |
| `internal/provider` | the live executor: the wire format, the HTTP call, and what it costs | 24 |
| `internal/tool` | what an agent may do: allow, ask or deny, resolved per tool | 12 |
| `internal/toolrun` | where a tool may do it: the workspace boundary, and `bash` under a deadline | 55 |
| `internal/inbox` | questions a run is waiting on: listing is a fold, answering is an append | 21 |
| `cmd/arxi` | the CLI, the short flags and the NDJSON protocol server | 175 |
| `internal` (arch) | that the kernel stays pure, and that no effect is unhandled | 18 |

`blueprint validate` prints the config **as resolved**, not the file read back.
Most of what it shows the user never wrote:

```
$ arxi blueprint validate ./examples/feature-team.yaml
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
$ arxi run start ./examples/feature-team.yaml "add rate limiting" --budget 2.00 --sim
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

Without `--sim` the command now runs for real: it resolves each member's model
against the registered providers, calls the endpoint over HTTP, and charges the
budget from the token counts the provider reports. `--sim` drives the same
reducer, the same log and the same loop; only the executor, the clock and the
timekeeper differ, so the run you get is the run you would get minus the model
calls. That is also what makes `replay` and `run why` work on a simulated log:
the log is the truth, and nothing in the reducer knows which executor produced
it.

`--model` sets the default for members that declare none. It has no default of
its own, deliberately — a default here would be a spend decision taken inside the
binary, and upgrading `arxi` could then change what an unchanged command costs.

```
$ arxi provider add local --base-url http://127.0.0.1:11434/v1
provider local registered (http://127.0.0.1:11434/v1, no credential)
  see what it offers: arxi model list

$ arxi run start ./bp.yaml "fix the failing test in ./pkg/auth" --budget 5.00
run rmtg75ex6-ac02323d started (budget 5.00 USD, workspace auto→none)
run rmtg75ex6-ac02323d failed (seq 6, stopped by reaching a terminal status)
  stage:  build
  turns:  1
  spent:  0.0000 of 5.0000 USD in the tree
```

That transcript is a real run against a real HTTP server, and two details in it
are the point. **`no credential`**: a loopback provider is assigned no key
variable at all, which was found by running the binary rather than reading it —
`provider add local` used to assign `$LOCAL_API_KEY`, and the run then refused
because it was empty, making the one provider that can be exercised with no key
and no bill the only one that could not be used without inventing a meaningless
secret. **`spent 0.0000`**: `llama3.1` is priced at zero *knowingly*, which is
not the same as being unpriced, and the code keeps those two apart because
collapsing them silently disables `--budget`.

`serve` is the same surface with a different mouth. One request per line, one
response per line, NDJSON, in order:

```
$ arxi serve
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
real capability (`arxi design`) and is not exposed to the protocol. That is
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
surface, and the first time someone flags a new capability `Protocol`, `arxi
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

**22 of 49 declared capabilities are wired — 44.9%.** That figure is measured,
not estimated: a probe walks `surface.Registry`, invokes every declared path
against the built binary, and counts the ones that do not answer *"declared but
not implemented"*. It is the honest denominator, and it is deliberately
unflattering — the 27 that remain are mostly `run *`, `agent *`, `state *`,
`event *` and `inbox *`, which now want the inbox and a way to read the log
rather than the executor or the tool runner. The implemented eighteen are
`provider add`, `model list` /
`enable` / `disable`, `run start`, `blueprint validate`, `trigger create` /
`list` / `show` / `pause` / `run`, `eval run` / `list` / `compare`, `schema`,
`serve`, `surface` and `version`.

### One number is not enough

44.9% is the CLI surface, and quoting it alone would be misleading in **both**
directions. Four things are being built, and they are at very different stages:

| dimension | measured | how |
|---|---|---|
| the engine — event types the reducer folds | **32 / 32 — 100%** | every `EventType` constant appears in a `Decide` switch arm |
| effects dispatched by the run loop | **7 / 7 — 100%** | every `kernel.Effect` has a case in `internal/exec` |
| effects a **real** executor performs | **3 / 3 — 100%** | `SpawnTurn` calls models; `CallTool` runs tools in a confined workspace; `AskHuman` writes the question to the log |
| the CLI surface | **22 / 49 — 44.9%** | every declared path probed against the built binary |

Read together they say something a single percentage cannot: **the core is
finished and the edges are not.** The reducer, the log, the fold, the budget
arithmetic and the trigger/eval/model layers are complete and heavily tested —
that is where most of the 940 tests live. What is missing is almost entirely
*the last mile*: CLI verbs that would read state the runners already produce.

That row moved from **1 / 3** to **2 / 3** when `CallTool` was connected to
`internal/toolrun`, and not when the runner was written. The runner existed,
fully tested, for two commits while the number stayed at 1 / 3, because a
count that rose on code being authored rather than reached would measure this
project's effort instead of its user's capability.

It reached **3 / 3** when `AskHuman` stopped refusing. That refusal's stated
reason was that an inbox needs somewhere to persist a question outliving the
process, and the reason turned out to be satisfiable without building anything
to persist it: the question is already an event. `applyToolDenied` mints the id
and records the item, `AskHuman` returns `inbox.created`, `kernel.Fold` rebuilds
`State.Inbox` from it, and `arxi inbox` folds the same log. **Listing is a fold,
answering is an append, and there is no third thing to keep in sync** — see
`internal/inbox`'s package doc for why a second store would be a liability
rather than a convenience.

So the honest summary of that row is smaller than 3 / 3 sounds: every effect a
run can emit now reaches something real, and **no effect fakes a result**. It
does *not* mean a run drives itself to completion after an approval — nothing
yet resumes a blocked run from the CLI, which is the next thing worth building.

That shape is also why 44.9% understates and 100% overstates. Twelve of the
27 unwired capabilities are `run *` verbs — `list`, `show`, `tree`, `result`,
`pause`, `cancel`, `fork`, `replay` — and each is a **projection of a log that
already exists and is already correct**. They are not twelve features; they
are twelve readings of one finished mechanism.

The one number worth committing to, if only one is wanted: **the system can
reason about a run end to end, can do the work inside one, and cannot yet pick
a run back up after a human has answered it.**

Two entries in that unwired list are worth naming, because they are remedies
this README itself recommends. `agent tool policy` is what stops an approval
loop — approving a tool call re-spawns the turn while the policy still says
`ask`, so the model asks again — and it is declared but not implemented. And
`run unpause` is what would drive a run forward once an inbox reply has landed.
A remedy a document names and a binary refuses is the worst kind of gap,
because it is discovered by the person already in trouble.

Both surface numbers moved for an unglamorous reason worth recording: `surface`
and `version` were always implemented, were always on the first screen, and were
never *declared*. They were absent from the denominator and the numerator at
once. The count changed because the registry stopped being wrong, not because
anything new was built.

The count did not move when the live executor landed, and that is the honest
result: `run start` already counted as wired when only `--sim` worked, so making
it real improved the *thing* without improving the *number*. A percentage that
had gone up here would have been measuring the wrong property.

The probe was itself wrong once, in the flattering direction, which is worth
recording. It truncated the three-word `agent tool policy` to `agent tool` — a
path that does not exist, so `--help` answered *"does not exist in the surface"*
rather than *"not implemented yet"*, and the command counted as done. It read
36.2%. A probe that miscounts in the direction you would like is worse than no
probe, so it now handles three-word paths and refuses to count any path the
surface rejects.

The probe has to bound each invocation, and that is worth writing down because
it cost two minutes of wall clock to rediscover: `trigger run` loops by design
and `serve` blocks on stdin, so a walk that simply waits for each command hangs
on the first one. It kills the **process group**, since a child that outlives
the probe outlives the directory it was writing into.

**Providers and models are wired as of this change**, and they come before the
executor rather than after it for a reason that is not sequencing preference: a
run needs an endpoint, a credential and a model id, and until now none of the
three could exist. `docs/design` §20.1 puts `provider add`, `model list` and
`model enable` exactly there — the first three commands a new user types, ahead
of the first run.

The rule about which models may be called lives in `internal/model` and is the
gate the executor will consult before spending money. It refuses three things,
each of which is a distinct way a run could otherwise cost money and produce
nothing: a model that does not exist (with a suggestion, bounded to three edits
so it never claims a Claude id and a GPT id are interchangeable), a model that
**two providers offer** (refused rather than resolved by sort order — picking one
means that registering a provider months later silently reroutes every existing
agent to a different price), and a model an operator **disabled**, since
`model disable` is listed in §20.11 as a cost decision and a resolver that
ignored it would make the command decorative.

**The key is never stored.** A provider holds the *name* of an environment
variable, and `--api-key-env sk-ant-…` — one keystroke of misunderstanding from
the correct command — is refused by shape rather than by prefix, because prefix
matching alone passes every vendor that does not use one and the cost of a miss
is a published secret. The check runs *before* the file is created: an error
printed after the write is not protection. The stored file is `0600`, not `0644`,
because it is a map to a credential, and an `api_key` field added by hand is
refused on read rather than ignored.

Plain `http` is accepted **only** to loopback. Anywhere else the API key would
cross the network in clear text; loopback is the one case with no network to read
it off, and it is also the only provider that can be exercised end to end with no
credential and no bill. A host that merely *begins* with `localhost` is remote,
because `localhost.evil.com` resolves to whatever its owner points it at.

**The live executor exists as of this change**, so `run start` without `--sim`
calls a real model over HTTP and charges what it costs. `internal/provider` owns
it, and it is a separate package for a reason worth stating: an architecture test
holds `internal/exec` to exactly one internal dependency, the kernel, and a
thing that speaks HTTP and resolves models cannot live there without breaking
it. Go satisfies interfaces structurally, so the CLI hands one in and the runner
never learns which kind it got.

**Sim and live differ in exactly three objects** — the clock, the timekeeper and
the executor — and share the reducer, the log, the loop and the effect ordering.
That is the whole reason `--sim` is worth trusting, and it is also why the
timekeeper is not hardcoded: `VirtualTime` *jumps* to the next deadline while
`RealTime` *waits* for it, so a live run wired to the virtual one would skip
every timeout it exists to honour.

Because a simulated log is meant to be indistinguishable in every other respect,
the `simulated` field on `run.started` carries the entire distinction. It was a
hardcoded `true`, and the bug was found by reading the log of a real run — one
that had really called a real server — and finding it describe itself as a
simulation. Nothing failed; the costs were right and the summary was accurate.
Only the durable artefact lied, which is the copy that outlives the terminal.

**A turn is refused before it spends, in three ways, and each is a distinct way
money could otherwise buy nothing.** A member with no model and no `--model`
default is refused *before the run directory is created*, so a refusal leaves no
debris to clean up. A model nobody has priced is refused rather than billed at
zero: an unknown price is not a price of zero, and reporting it as zero would
leave `--budget 5.00` enforced against a total that never moves — not a wrong
number but a disabled safety mechanism, discovered on the invoice. And a model an
operator **disabled** is refused, which required resolving through `model.Resolve`
rather than the store's `Owner`: `Owner` deliberately finds disabled models,
because `model enable` has to act on one, so a run built on it would make `model
disable` decorative while looking entirely correct.

**Prices live in the binary, not in the provider file.** A price is a fact about
the world that changes without asking us; the provider file records what the
operator chose. A past run is unaffected either way, because it wrote the dollars
it actually spent into its own log. They are stated per **million** tokens, the
unit vendors publish, and input and output are separate fields because output
costs four to five times more everywhere — one blended rate misprices `--budget`
by a multiple, in a direction that depends on whether a turn mostly reads or
mostly writes.

**The prompt is assembled in the order that keeps the provider's prefix cache
warm**: identity, situation, memory, shared, causes — most stable to most
volatile. Stable sections go in the *system* message, because a cache keys on a
byte-identical prefix; volatile ones go in the *user* message. Putting causes
early turns a cache hit into a full-price prompt on every single turn.
`stream` stays off, since streaming delivers the usage block last and a cost
that arrives after the turn cannot be charged as the turn happens, and
`max_tokens` is always sent, because an unbounded reply is an unbounded bill and
the budget is only charged once the response is already back.

A **domain** failure — the provider answered and refused — comes back as events,
because it happened and the log is the record. A **returned error** means the
failure could not be turned into a fact at all: the transport broke, or the
context was cancelled. The distinction is load-bearing rather than tidy.
Emitting `agent.activated` and *then* returning an error would leave a member the
reducer believes is thinking, holding a turn that can never close.

**Tool policy is enforced, and an allowed tool now runs.** Policy came first
rather than as a halfway measure, because two of the three outcomes are
decisions rather than work — and they are the two where being wrong is
unrecoverable:

| outcome | what happens |
|---|---|
| `deny` | `tool.call_denied`. Nothing runs, and the log records why. |
| `ask` | `tool.call_denied` with `policy: ask`, which the reducer turns into an inbox item plus a `blocked_ref`. Per `spec/events.md` this is **not an error, it is a question**. |
| `allow` | **runs**, in `internal/toolrun`: a per-member workspace, `bash` under a deadline, output bounded. `tool.call_completed` carries the result. |

The `allow` row is narrower than it sounds, and the reason is worth stating.
Because a granted *mutating* tool resolves to `ask`, `bash` and `write` do not
reach the runner unattended — they reach a human first. What runs unattended
is `read`. That ordering is deliberate: the confinement in `internal/toolrun`
stops careless **paths**, and it cannot stop a **script**, because a script is a
program rather than an argument. The package doc says so in as many words,
under a heading naming what it does not protect against, and a test reads the
`ask` claim back through `tool.Resolve` so the prose cannot quietly become
reassuring and wrong.

The rules are the ones the design already committed to: a tool not granted is
`deny`, a granted mutating tool is `ask`, a granted reading tool is `allow`. An
undeclared policy is `deny` because a permissive default turns every oversight
into a silent hole, and the person who pays for the hole is never the person who
forgot.

A denial is emitted as an **event, not an error**. A denial is a domain fact: it
happened, it is the correct outcome, and the run should continue knowing it.
Returning an error would abort the effect and leave no trace, so the one case
where the policy did its job would look identical to a broken executor.

`tool.Mutating` is a named set rather than two literals in two packages, because
`internal/kernel` already had it — deciding that a blueprint whose members can
write needs a worktree instead of a shared directory. If the two drift, neither
failure is visible: a tool that mutates for isolation but not for policy gets a
private directory to scribble in and no approval gate on the scribbling, and the
reverse gets an approval gate and a shared directory to corrupt.
`TestTheMutatingSetAgreesWithTheKernel` reads the kernel's set through the
behaviour it drives, not by copying it — a copy would agree with itself forever.

Missing still: the **inbox** — somewhere a question can outlive the process.
`AskHuman` refuses entirely, which is why the run above still goes quiet
instead of finishing: submitting work is a tool call, and a call that resolves
to `ask` has nowhere to wait. A fake result would be indistinguishable in the
log from a real one, and the log is the truth. Refusing loudly is the only
option that keeps that sentence honest.

The trigger **scheduler** exists as of this change, and is worth describing
precisely because the split is the interesting part. `internal/trigger` decides,
purely: `Due` says whether a slot has arrived and how many runs are owed after an
outage, `Admit` says what to do about that given what is already running.
`internal/scheduler` is the caller that owns a clock, a store and a subprocess,
and its `Tick(now)` takes the instant as a **parameter** — which is why a
four-day outage, an execution that outlives three of its own slots, and a run
that ignores its own cancellation are all ordinary table entries rather than
tests that wait.

One finding from building it is worth stating, because it is what makes the loop
small: **the tick interval is a latency knob, not a correctness one.** Dueness is
derived from `last_fired_at` on every pass, so a slot that is not acted on stays
due. A scheduler that oversleeps runs the trigger late and says so; one that
oversleeps for a week reports six missed firings and applies `--on-missed` to
them. There is no drift correction and no catch-up queue, because the store
already is the queue — durably, across restarts.

`arxi trigger run` is the command that starts it. `--once` checks and exits,
which is what a systemd timer or a cron entry wants; without it the process
loops on `--interval` until interrupted. `--dry-run --once` reports what would
fire and starts nothing:

```bash
$ arxi trigger run --dry-run --once
  would run: schema
nightly-audit            started      first firing, due at 2026-08-29T02:09:51Z

$ arxi trigger run --interval 30s
watching 1 trigger(s), checking every 30s
nightly-audit            waiting      not due until 2026-08-29T03:00:00Z
```

Each firing is a **child process**, not a goroutine. `--then "trigger run"` is
a legal trigger, and in one process that is a stack overflow rather than a
visible pile of subprocesses; `cancel-previous` has to be able to stop work
that is not cooperating, and a goroutine cannot be killed; and a scheduled run
that panics must not take an unattended scheduler with it.

Two findings from wiring it are worth recording, because both were invisible
until the binary was actually run twice in a row.

**`--dry-run` was consuming the slot it previewed.** A firing has two effects
and only one had been faked: the child process was suppressed, and the store
write was not. `Tick` records `last_fired_at` for every firing it admits — that
is what makes a slot stop being due — so the preview advanced the schedule and
the real run a second later answered "not due until". The safest-looking flag
in the command was the only one that could silently skip a scheduled run. None
of the scheduler's own 31 tests could have caught it: they use a fake store and
correctly assert that `Tick` *does* save. The CLI is where the two fakes are
chosen, so the CLI is the only layer where the omission existed.

**Declaring the capability hung the test suite.** The test that checks no
declared subcommand is called "unknown" reads the registry instead of
hand-listing subcommands — the right design, and exactly why it reached
`trigger run` and invoked it with no arguments, which loops forever on purpose.
One package sat in `os/exec` until the timeout while the other eleven stayed
green. A registry-derived test cannot assume the commands it discovers return.

Eval runs now persist. `eval run` writes one file per run, `eval list` shows
what exists, and `eval compare` reads two of them:

```bash
$ arxi eval run --suite ./suites/review-quality.yaml --budget 1.00 --sim
eval e20260828T223546: 2 cases, 2 judged, 0.02 USD of 1.00
  pass rate: 1.00 (2 passed, 0 failed)
  stored:    evals/e20260828T223546.json

$ arxi eval list
ID                  SUITE           PASS  JUDGED  COST  NOTE
e20260828T223546-2  review-quality  1.00  2/2     0.02  sim
e20260828T223546    review-quality  1.00  2/2     0.02  sim

compare two: arxi eval compare e20260828T223546 e20260828T223546-2
```

Three things in that output are deliberate. The `sim` note is a stored field, so
a fake run stays labelled as one forever and `compare` refuses to read a
simulated baseline against a real candidate without saying so first — a caveat
printed **above** the table, because one printed below is read after the
conclusion has been drawn. The `-2` suffix is there because two runs can start
in the same UTC second, which is what a scripted loop does; the id is resolved
before the suite runs, so a collision cannot be discovered after the money is
spent. And a run is never rewritten or pruned: `compare` cites runs by id, and a
citation that keeps resolving while its numbers change is worse than one that
breaks.

Triggers are **most of the way done, and it is worth being precise about which
part is not**. Schedules parse, firings compute, the action boundary is
enforced, triggers persist, and `arxi trigger create/list/show/pause` works:

```bash
$ arxi trigger create nightly-audit \
    --on "cron:0 3 * * *" \
    --then "run start security-team 'audit dependencies for new CVEs'" \
    --budget 5.00 --budget-period day
trigger nightly-audit created (next: 2026-08-29 03:00Z)

$ arxi trigger list
NAME           ON              STATUS  LAST   NEXT
nightly-audit  cron:0 3 * * *  active  never  2026-08-29 03:00Z
```

**Nothing fires yet.** There is no process watching the clock, so `NEXT` is a
prediction and `LAST` will say `never` forever. That is the last piece, and it
is last on purpose: the order was chosen so the parts that are wrong *silently*
came first. A scheduler built on a parser that quietly disagreed with crontab
would fire on the wrong days and nothing would report it — whereas a correct
parser with no scheduler is a system that visibly does nothing, which is the
failure that gets noticed in a second.

There is also no `trigger delete`, and that is a decision rather than an
omission — `pause` keeps the configuration and the history, and the reason a
trigger was stopped is usually the thing you want to read later. Asking for
`delete` gets told so, rather than being told it is not a command.

```
cron:0 3 * * *        every:15m        at:2026-09-01T03:00:00Z
webhook:/deploy       file:./src       event:stage.failed
```

Everything is **UTC**, and that costs something visible: "3am" is 3am UTC, not
3am where you live. The alternative is worse — a local 02:30 daily trigger fires
twice or not at all on a DST transition, on the one night nobody is watching,
which is the entire premise of a nightly job.

The cron parser takes **numbers only**. `MON` is refused with the number to
write, because implementations disagree on whether the week starts at Sunday=0
or Monday=0, and a job running on the wrong day of the week goes unnoticed for
weeks. It implements cron's genuinely counter-intuitive day rule — when *both*
day-of-month and day-of-week are restricted, **either** matching fires — because
people paste crontab lines, and a line that means something narrower here would
fire less often than the user has already watched it fire elsewhere, with no
error to point at.

`cron:0 3 30 2 *` (February 30th: five valid fields, an instant that never
happens) is **refused at create time**, not stored. Stored, it would sit in
`trigger list` marked active and never once run.

`--then` takes **an arxi command**, with no prefix:

```
--then "run start security-team 'audit dependencies for new CVEs'"
```

There is no action vocabulary, and that is the point. It was declared as
`run:|emit:|notify:` — a hand-written list of things a trigger may do, sitting
inside the file whose whole purpose is that there is only one such list — and it
had already drifted: **`notify` is not a command in arxi**. Naming the surface
instead of copying part of it means every verb arxi gains is triggerable the day
it lands.

Which commands are legal is derived from the same flag that decides what an
agent may call. A trigger fires unattended, so a trigger is not a human, and
`--then "inbox approve i1"` is refused: a trigger that approves inbox items
turns every `ask` policy in the system into `allow`, on a schedule, at 3am. That
is checked in the parser rather than left to the human approving `trigger
create`, because nobody reconstructs the security table from memory while
reading a one-line diff.

The **next firing is never stored**, only computed. A persisted `NEXT` is a
second copy of a derivable fact and it rots by existing: after four days of
downtime it is a timestamp in the past, indistinguishable from a broken
schedule. A paused trigger reports no next firing at all, even though its
schedule still names one — an operator scanning that column for "is my
automation running" reads a future timestamp as *yes*.

**Eval** runs, under `--sim`. Suite files load and are validated, the fold over
cases executes them against a budget, and the result is reported together with
what it is a measurement *of*:

```
$ arxi eval run ./suites/review-quality.yaml --budget 12.00 --sim
note: one sample per case over 3 judged cases: a difference smaller than about
0.50 between two runs of this suite is not distinguishable from noise

eval e20260828T214104: 3 cases, 3 judged, 0.03 USD of 12.00
  pass rate: 1.00 (3 passed, 0 failed)
  mean cost: 0.0100 USD per judged case
```

`3 cases, 3 judged`, and not §20.11's `20 completed`: "completed" says nothing
about whether anything was **judgeable**, and the judged count is the denominator
of the number on the next line.

Run the same suite against a budget that cannot finish it, and the report changes
shape rather than just its numbers:

```
$ arxi eval run ./suites/review-quality.yaml --budget 0.02 --sim
note: the budget ran out after 2 of 3 cases, so 1 case(s) never ran
(rejects-hardcoded-secret) — the cases that DID run are the first ones in the
file, not a sample of the suite, so this pass rate is over a prefix and carries
whatever bias the file's ordering has
note: spent 0.0200 of the 0.02 USD budget; a run that stops on budget is a
measurement of cost, not of quality — raise --budget before reading the pass
rate as a result

eval e20260828T214104: 3 cases, 2 judged, 0.02 USD of 0.02
  pass rate: 1.00 (2 passed, 0 failed)
  unjudged:  0 errored, 1 skipped
  mean cost: 0.0100 USD per judged case

$ echo $?
1
```

That says **1.00**, and it is the most dangerous number this tool can print. Two
thirds of the suite was measured, every case that ran passed, and a reader
scanning for a pass rate finds a perfect one. Three things are arranged against
it: the exit code is **1**, so CI cannot record a truncated run as a clean pass;
the notes print **before** the numbers; and the note names *which way the bias
runs*. Cases execute in file order, so a truncated run measured a **prefix**, and
hard-cases-last — the natural way to write a suite — makes the reported rate too
high, and higher the earlier the money runs out. A prompt change that makes each
case more expensive can therefore *raise* the reported pass rate.

Cost is banked **before** a case's error is examined, because money spent by a
case that then fell over is still spent, and a suite that treated failures as
free would overrun its ceiling. A case is also **skipped rather than started** on
what is left below a reserve: an agent handed less than one turn's budget does
not fail cleanly, it produces a truncated answer that still gets judged and
counts as a genuine `FAIL`.

`--sim` is required. `run start` calls real models now, but `eval` is not yet
wired to the same executor: every case is still answered by the simulator, so a
"real" eval run would spend nothing and report a pass rate anyway. The gate stays
for that reason rather than from inertia, and it is more necessary than before,
not less: a user who has just watched `run start` call a live model has every
reason to assume `eval run` does too.

It also **loads each case's blueprint**. That check is most of why `--sim` is worth
running at all. The simulated answer does not depend on the blueprint, so
skipping the load would change no output on the happy path; a suite naming a
blueprint that does not exist would then judge every case, satisfy every
`contains`, and report a healthy pass rate over answers no agent produced. The
author would find out after paying for nineteen real runs.

```
$ arxi eval run ./suites/typo.yaml --budget 12.00 --sim
note: 2 case(s) errored and produced no judgeable answer; they are in the cost
total (money was spent) and out of the pass rate (a harness failure is not a
worse prompt)

eval e20260828T214104: 2 cases, 0 judged, 0.00 USD of 12.00
  pass rate: none — no case produced a judgeable answer
  unjudged:  2 errored, 0 skipped
  error: alpha                   blueprint "nowhere.yaml" could not be loaded, so this case did not run: open nowhere.yaml: no such file or directory
  error: beta                    blueprint "nowhere.yaml" could not be loaded, so this case did not run: open nowhere.yaml: no such file or directory
```

`pass rate: none`, not `0.00`. "The worst possible result" and "no result" are
opposite facts and must not share a representation. `--json` omits the
`pass_rate` key entirely and sets `pass_rate_absent` instead, for the same reason
the trigger CLI omits `next` rather than writing `"(paused)"` into it — except
that `0.0` is worse than a human string in a machine field, because it parses.

Both cases are reported, not just the first: "every case names a file that is not
there" and "case 7 has a typo" have the same symptom in a single case and
different causes. Failures are **named with their reason** throughout — `6 failed`
tells somebody that something is wrong without telling them what to look at, and
the reason a case failed is the whole reason the eval was run.

`eval compare` is the half that does not work yet, and it **refuses** rather than
approximating: nothing persists an eval run, so there is nothing to load, and two
empty summaries would compare cleanly against each other and print a table of
zeroes — a comparison of nothing that has the shape of a comparison. The refusal
says which capability is missing and why comparing needs it, because the reader's
first thought is that they mistyped a run id.

The loader **refuses an expectation that cannot fail**. `expect:` with nothing
under it, `contains: [""]`, and the same string in both `contains` and
`not_contains` are all rejected at load time:

```
cases[0]: expect is required; a case with no expectation passes
unconditionally, so it raises the pass rate while measuring nothing
(valid: contains, not_contains, equals)
cases[0].expect: contains[0] is empty, and every string contains the empty
string, so this condition always holds
cases[0].expect: "x" is in both contains and not_contains, so no output can
satisfy this case
```

Each of those produces a case that reports `pass` for every possible output, and
a suite of them reports a healthy pass rate while measuring nothing — the one
failure mode of an eval tool that is worse than having no eval tool, because the
number gets quoted in a decision.

Every rate divides by **judged** cases, never by declared ones. Dividing by
declared treats a harness crash as a worse prompt. The cost *total* does include
errored cases — money spent before a case fell over is still spent, and the
total is what the next `--budget` is chosen against — so the total and the mean
deliberately disagree, and `compare` says which population each number came from
instead of letting the two be read as one.

`eval compare` treats a delta table as **a causal claim it has to defend**,
because "the prompt got 15% better" is what a reader takes from it, and several
things other than the prompt produce the same table: different suites, the same
suite name with an edited file, two runs over different case sets, a run that
judged only some of its cases, and — most often — a delta the size of the noise.
Each is detected and printed **with** the numbers, ordered most-invalidating
first, since a caveat under a table is read after the conclusion has been drawn.

The sample-size check is where this repository had to take its own advice.
`docs/design/20-use-cases.md` §20.11 celebrates a pass rate going **0.65 → 0.80
over 20 cases**, and that result is **not evidence**: the 95% band on a
difference between 13/20 and 16/20 is **±0.27**, nearly twice the delta. The
first implementation compared against a threshold chosen by hand — "within two
cases' worth", 0.10 here — and so said nothing about exactly that table, the one
table in the repository a reader is most likely to imitate. A hand-picked
threshold cannot know how many samples it is looking at, so it was replaced by
an interval; the identical `+0.15` over 100 cases (band ±0.12) is reported
without complaint.

That interval is Agresti-Caffo rather than the textbook one, because the naive
standard error is `p(1-p)/n` and collapses to **zero** at a 0% or 100% pass rate
— 4/4 against 3/4 would be compared against ±0.00 and the difference declared
real, certainty manufactured by the formula out of four samples.

## Build and test

Requires Go 1.22.

```bash
go build -o arxi ./cmd/arxi
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
cmd/arxi/           the CLI
spec/               event contracts
docs/design/        the execution model and the use cases
docs/adr/           one file per decision that cannot change quietly
```

The kernel imports nothing from the rest of the project. It is the only layer
that has to stay pure, and there is a test that enforces it.
