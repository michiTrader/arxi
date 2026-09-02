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

That restriction is not aesthetic. It is what makes five features be one fold
over the same reducer, differing only in what is plugged into it:

| feature | what it is | state |
|---|---|---|
| `arxi run --sim` | fold + fake executor | **works** |
| `arxi why` | read the `State` that came out of the fold | **works** |
| `arxi run start` | fold + real executor | **works** |
| `run replay` | fold over an old log, with no executor | **works** |
| `run attach` | fold over a log somebody else is still appending to | **works** |

The `state` column was added after this table was caught overclaiming: it listed
all four as features while `arxi run replay` and `arxi run why` both answered
*"declared in the surface but not implemented yet"*, and `Replay` appeared
nowhere in the source except inside a comment. Both are built now, so every row
reads **works** — and the column stays, because a table that has only ever said
"works" is a table no reader can catch. It also rotted in the *other* direction,
which is the better argument for keeping it: the `run start` row still said
"executor absent" long after the live executor had landed, and was found only
when wiring `run replay` made somebody read all four rows at once.

The fifth row arrived with `run attach`, and it is the row that makes the claim
falsifiable rather than decorative. If the fold were not pure, following a live
run would need a second implementation of it — a streaming reducer, kept in step
with the batch one by hand. It needed neither: `cmd/arxi/attach.go` folds to the
join point and then calls the same `kernel.Decide` once per arriving event, and
the only thing it adds is a byte offset.


What the correction bought is that the architectural claim was left standing as a
**prediction**, separate from the delivery claim, and the prediction can now be
checked. In a design where the reducer calls the network, `replay` is a second
program reimplementing the logic of the first, always out of date, and nobody
notices until the moment they need it. Here it was supposed to be a fold with the
executor left out. It is: `cmd/arxi/replay.go` folds with `kernel.Decide`, the
same function the run loop calls, and needed no new kernel entry point to do it.
The one thing it adds is a counter — the effects the fold produced and nobody
executed. That the executor really is absent is **measured rather than asserted**:
the run directory is hashed before and after, so an executor sneaking back in
turns the suite red.

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
  -r  --run            14 commands
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
that parameter: `-r` is `--run` on the fourteen commands that take a run id and
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

Underneath, every package is done and tested — **1207 tests, no dependencies**.
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
| `internal/kernel` | the pure reducer: `Decide`, `State`, `Effect`, `Explain` | 50 |
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
| `internal/tool` | what an agent may do: allow, ask or deny, resolved per tool | 13 |
| `internal/toolrun` | where a tool may do it: the workspace boundary, `grep` and `edit`, and `bash` under a deadline | 83 |
| `internal/inbox` | questions a run is waiting on: listing is a fold, answering is an append | 23 |
| `internal/toolstore` | per-agent policy overrides on disk: one file each, written atomically | 20 |
| `cmd/arxi` | the CLI, the short flags and the NDJSON protocol server | 383 |
| `internal` (arch) | that the kernel stays pure, and that no effect is unhandled | 18 |

Those cells add up to the total, and that is now the point of printing them: an
earlier version of this table did **not** sum to the figure above it — the total
was corrected each time it was measured and the nineteen cells were not, so they
drifted 108 cases behind while looking authoritative. Per-package numbers nobody
adds up are nineteen more places for a stale figure to hide, so they are only
worth keeping if the sum is checked. `cmd/arxi` is where the drift had collected;
it was the row that grew every time a verb was wired.

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

**40 of 49 declared capabilities are wired — 81.6%.** That figure is measured,
not estimated, and as of this step it is measured *by the suite* rather than by
hand: `TestTheReadmeCapabilityCountIsWhatTheBinaryActuallyDoes` walks
`surface.Registry`, invokes every declared path against the built binary, and
counts the ones that do not answer *"is declared in the surface but not
implemented yet"*. It reads the fraction above out of this file, so the sentence
you are reading cannot go stale without a test failing. That is not a
hypothetical: wiring `run list` moved this figure from 24 to 25, `run show`
moved it to 26, `run why` to 27, `run prompt` to 28, `run tree` to 29,
`run result` to 30, `run pause` to 31, `run cancel` to 32, `event log` to 33 and
`event emit` to 34, `run fork` to 35, `run replay` to 36, `run attach` to 37,
`run steer` to 38, `event trace` to 39 and `state set` to 40, and every time the
number above was
corrected because **the suite failed**, not because anybody remembered to check.
It is deliberately unflattering — the 9 that remain are `agent list` / `create`
/ `show`, `role define`, `blueprint create` / `install`, `state get` /
`lock` and `design`, which want an agent store, a keyed store or a generator
rather than another way to read what a run already wrote. The implemented
forty are
`provider add`, `model list` /
`enable` / `disable`, `run start`, `run list`, `run show`, `run why`, `run tree`, `run prompt`, `run steer`, `run result`, `run pause`, `run unpause`, `run cancel`, `run fork`, `run replay`, `run attach`, `agent tool policy`,
`blueprint validate`, `state set`, `event emit`, `event log`, `event trace`, `trigger create` /
`list` / `show` / `pause` / `run`, `inbox` / `approve` / `reject` / `reply`,
`eval run` / `list` / `compare`, `schema`, `serve`, `surface` and `version`.



That probe is worth *running* rather than trusting, and two botched runs of it
are the reason it is now a test instead of a shell loop.

The first reported **22 / 47**, because the loop let `serve` read the paths list
off stdin and swallow two lines. A measurement that quietly loses part of its
own denominator still looks like a result, so the test asserts the number of
paths it walked against `len(surface.Registry)` and closes stdin on every
invocation.

The second was worse, and it was caused by this document. The paragraph above
used to describe the probe as counting paths that do not answer *"declared but
not implemented"* — a paraphrase. The binary says *"is declared in the surface
but not implemented yet"*. Rebuilding the probe from the README's wording, which
is exactly what checking the claim from the outside looks like, produces a
pattern that matches nothing at all: all 49 paths fall into the "implemented"
bucket and the probe reports **49 / 49 — 100.0%**. It was caught only because 25
unwired commands do not appear in an afternoon.

The lesson generalises past this one number. **A verification tool that cannot
fail reports total success**, and it reports it in the flattering direction. So
the test does not merely count — it first probes a path known to be unwired and
*requires* the sentinel to appear. If that phrase ever stops matching the
binary, the suite says so instead of quietly certifying that everything works.

### One number is not enough

81.6% is the CLI surface, and quoting it alone would be misleading in **both**
directions. Four things are being built, and they are at very different stages:

| dimension | measured | how |
|---|---|---|
| the engine — event types the reducer folds | **33 / 33 — 100%** | every `EventType` constant appears in a `Decide` switch arm |
| effects dispatched by the run loop | **7 / 7 — 100%** | every `kernel.Effect` has a case in `internal/exec` |
| effects a **real** executor performs | **3 / 3 — 100%** | `SpawnTurn` calls models; `CallTool` runs tools in a confined workspace; `AskHuman` writes the question to the log |
| the CLI surface | **40 / 49 — 81.6%** | every declared path probed against the built binary, by a test that also verifies its own sentinel |

Read together they say something a single percentage cannot: **the core is
finished and the edges are not.** The reducer, the log, the fold, the budget
arithmetic and the trigger/eval/model layers are complete and heavily tested —
that is where most of the 1207 tests live. What is missing is almost entirely
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
does *not* mean a run drives itself to completion after an approval — but a
blocked run can now be picked back up from the CLI, which is what `run unpause`
does and what this paragraph used to name as the next thing worth building.

That shape is also why 81.6% understates and 100% overstates. The `run` group is
now **14 / 14** and the `event` group **3 / 3**: every verb a person needs to
inspect, redirect or end a run in flight is wired, and none of the 9 that remain
is one of them. The list here has been rewritten four times and each rewrite was
the same admission — it named `replay`, then `attach`, then `steer` as the next
thing worth building, and each one got built.

`state set` is the one just finished, and it is the first verb that writes to the
run's **shared store**: what one member wants another to know without paying for a
turn to say it. A member that has frozen an API contract writes the key; whoever
needs it reads it back. The store is not a file beside the log, and that is the
whole design — a KV file would make `state = fold(Decide, State0, events)` false in
the direction hardest to notice, since the fold would rebuild every member, lock
and inbox item from August and then read **today's** value for a key an agent set
last Tuesday. Nothing is lost by keeping it in the log either: the history of a key
*is* `arxi event log <run> --type state.set`, and a second copy inside the state
would be a copy that can disagree with it.

The write **drives the run**, because `state.set` is deliberately not
runtime-derived and deliberately outside `isWatcherDispatched`: a blueprint that
declares `watchers: [{agent: backend, pattern: state.*}]` gets a turn when the
contract it was waiting for lands. Those effects live only in `Decide`'s return
value, so appending and returning would leave a declared watcher unfired and
indistinguishable from a pattern that never matched — the same conclusion
`event emit` and `run prompt` reached, the second one after shipping the bug.

Where it *departs* from `event emit` is the paused run, and the asymmetry is the
judgement rather than an inconsistency. `event emit` refuses one outright: waking
somebody is the event's only purpose, so a parked cause is pure loss. A
`state.set` has a second purpose that lands whatever the status is — the key is
stored and `state get` will read it — so refusing would block a good write and
teach the user to unpause a run, which resumes spending, in order to leave a note
in it. The **one** exception is a `run_tool` watcher, and it is refused: a notify
or activate cause is parked in `PendingCauses`, which is part of `State` and comes
back when the halt clears, while `wakeWatchers` returns `CallTool`
unconditionally, nothing parks it, and the next drive folds this event into its
starting state and keeps no effects from it. That tool call would be dropped in
silence, and nothing has been written yet when the refusal happens.

Wiring it also found a **registry** defect that was systematic rather than a slip.
All three `state` verbs were declared with a key and no run — they had been
written from the agent's side, where the run is ambient, and a shell has no
ambient run. `state get` is where it bites hardest: it reads, so nothing can be
undone by getting it wrong, which is exactly what makes it dangerous, because with
no run to name the honest implementation had to invent one and a read that
silently answers from the wrong run is a read whose caller has no reason to doubt
it. Every entry declared before its CLI existed is a candidate for the same fix.

`event trace` came just before it, and it is the first verb here that reads
the log for its **shape** rather than its contents: it takes one event id and
prints the causal chain around it, ancestors above and descendants below, with
the event asked about marked. `caused_by` has been written by the reducer since
the first commit and nothing until now could follow it, so the fields existed and
the question "what caused this" had no answer at the CLI.

Reading a tree out of an append-only file is where the wiring got interesting,
because the file can hold shapes a tree cannot: two events naming the same cause
from different branches, a `caused_by` naming an id that is not in the file, an
id that appears twice, an event with no id at all, a depth stamp that disagrees
with the rebuilt tree, and — although appending in order cannot write one — a
cycle. Every one of them is **reported in the footer rather than repaired
silently**, because each is evidence about a producer, and a walker that quietly
smoothed them over would make the log look healthier than it is. The walk itself
carries a visited set, so a cycle terminates the print instead of the process.

What the verb also documented was a gap on the **producer** side, and writing
this reader is what made the gap measurable — so it is now closed. `caused_by`,
`correlation_id` and `depth` were written by `kernel.derived` and **not** by
`exec.stamp`, so every event the executor minted — turns, tool calls, their
results — carried no cause at all. On a real 21-event `--sim` log: **5 events
carried a cause and 16 did not**, so the log held sixteen causal threads instead
of one, an agent turn was a hole in the chain, every correlation group was rooted
at an executor event rather than at `run.started`, and depth 0 on all sixteen
cleared the `MaxDepth` brake in `wakeWatchers` as if each were a root cause. The
same run today — same blueprint, same 21 events — has **20 of 21 carrying a
cause**, all correlating to `run.started`, and `arxi event trace` prints a
six-event chain from the run's start down to `run.result`. The one event that
records no cause is `run.started`, which is the root.

The fix is one function — `exec.attribute` — and its placement was the whole
decision. Not in the `Executor` interface, because then every implementation has
to remember: the Fake, the live provider one, and whichever comes next. Copying
three fields is not a choice an executor should get to make differently, and the
one that forgot would produce a log that traces correctly under `--sim` and not
in production, which is the worst place for the difference to live. Not in
`stamp` either, which fills identity, takes a bare `[]kernel.Event` and is called
from three places — one of them, `Loop.appendTicks`, has no effect at all. So it
sits in the sequential tail of `runIndependent`, the last point where the effect
is still in hand.

Two decisions inside it are load-bearing. All the events of one effect get the
**same** cause, flat rather than chained — not `activated` causing `llm.response`
causing `turn_done`. One effect is one causal step; chaining would read prettier
and would **triple the depth every turn adds**, so cascades a blueprint legitimately
asked for would start dying two generations early for a reason nothing prints.
And a **timer tick is a root**: no cause, depth 0. The `SetTimer` that armed it
ran in an earlier step and the clock that delivers the id does not remember who
armed it, so reconstructing the link would need a timer-to-cause map in the
`Runner` — a map that is not in the log, which a resumed run would rebuild empty,
writing uncaused ticks where a fresh run wrote caused ones. `run`, `--sim`,
resume and replay would stop being one fold over the same bytes. It is also true
to what a tick is: time passing is not an event's consequence.

Closing the gap then **inverted the footer notes**, which is the part worth
recording. They had been written to accuse the producer, and on a healthy log the
accusation was now false: the six-event trace printed *"1 of these 21 events
carry no cause"* about `run.started`. An event with no cause is a **root** — the
run's own start, a timer firing, or something fed in from outside the fold — not
a defect. Both notes were reframed and gated at *more than one* root, so they are
silent on every healthy log and still loud where it matters: sixteen roots in a
21-event log reads as obviously wrong, which is how the next regression in
attribution gets seen. The fixtures that produce the old shape were kept rather
than updated, for two reasons — logs written before the fix are still on disk and
must still render, and they are what keeps the root-count note reachable in the
suite.

`run steer` came before that, and it is worth recording what it actually
cost, because this paragraph predicted the wrong thing. The prediction was that
it "needs the writer lock this binary hands to one process at a time — a
different problem from reading". True, and already solved: `run prompt` had paid
for the lock, the CAS on `seq` and the drive, so steering was an **extraction**
rather than new machinery. `prompt.go` went from 416 lines to 40 and the shared
body became `inject.go`, which is why the two verbs cannot drift — a test refuses
an unknown recipient through both and requires the same words back.

What the wiring did expose was a defect the surface had been carrying quietly:
`steer` declared `--on-busy` with a default of `steer`, `parseInvocation` applies
declared defaults before it checks anything, and the executor refuses any mode
the reducer does not implement. Each of those three is right on its own. Together
the verb would have refused **every** plain invocation with exit 2, quoting a
flag back at a caller who typed none.

The honest reason the remaining 10 will not fall as fast is that none of them is
a variation on something already here. `agent *` and `role define` want an agent
store; `blueprint create` / `install` want generation rather than validation;
`state get` / `set` / `lock` want a keyed store, and `lock` specifically wants a
**lease** — a named key held across processes until it expires, with nobody
running to expire it, which nothing in this tree has. `writer.lock` is not a
counter-example: it is one file per run directory holding a pid, taken and
dropped inside a single command.

`run attach` was built two verbs earlier, and it is the only verb here that reads
a log while somebody else is writing to it. It joins at the head — the events that
already exist are *not* reprinted, because `event log`, `run replay` and
`run show` each print history better, and backfilling would scroll the arriving
events out of view. Then it prints each event as it lands, folds it with
`kernel.Decide`, and stops on one of two things: the run reaching a terminal
state, exit 0, or the writer lock going away, exit 3.

**It takes no lock and opens nothing for writing**, which is what makes it safe
to point at a run that is mid-turn. It also never signals the writer, and does
not probe whether that pid is alive — deliberately, since a viewer that could
reap the process it is watching is a viewer that can break the run.

Following a file another process is appending to is where the interesting
correctness lives, and two cases are easy to get wrong in the flattering
direction, because both look fine until the day they do not:

**A read can land mid-write.** Bytes are made durable by the writer's `fsync`,
not atomic against a concurrent reader, so a read can return half a JSON object.
Decoding that half is a parse error, and the follow would die on a log that is
perfectly healthy. So only whole lines — up to the last newline — are ever
decoded, and the remainder is held for the next read.

**A batch that was never committed must not be shown.** `logstore` writes
`pending.commit`, then the events, then removes the marker; the next `Open` rolls
an uncommitted tail back. A follower that printed those bytes would report an
event that a later `arxi event log` on the same run does not have — the follower
would be its only witness. So `logstore.BatchInFlight` is consulted **after** each
read, which is what makes the answer sound: no marker *now* proves the bytes just
read are durable. When the writer dies mid-batch, the footer names the byte count
being withheld and says the next command will roll it back, rather than printing
lines that are about to stop existing or going quiet, which reads as the log
simply stopping.

The lock is sampled **before** each read, and the order is the whole argument for
why the tail cannot be lost: if the lock is gone at time T, nothing appends after
T, so a read at any later time has already seen everything there is.

**Nothing is writing to it** is a symptom, not an answer, so the two exits that
are not a finished run end with the same diagnosis `run show` would give: paused,
with `run unpause`; blocked on a question, with `arxi inbox`; a log with no
`run.started`, with `run start`. And when the run says *running* but no process
holds its lock, it says exactly that — the driver is gone — and pointedly does
**not** suggest `run unpause`, because the run was never paused and unpausing it
would not put a writer back.

`run replay` came before it, and it is the verb the whole arrangement was
for: fold the log again, at `--until-seq`, with the executor left out. What it
prints is the state at that seq, the event that produced it, and a count of the
effects the fold returned and nobody ran.

It folds **event by event** with `kernel.Decide` rather than calling
`logstore.Fold`, and that is not a style preference. `Fold` returns the final
state only, which throws away both things this verb exists to show: the event
*at* the target seq, which goes on the headline, and the effects, which are the
whole point of a fold with no executor.

**"No executor" is measured, not asserted.** The run directory is hashed file by
file before and after, and the two trees must be byte-identical — a stray
`os.WriteFile` in the command turns the suite red. That guard is why the verb can
also be pointed at a *live* run: it takes no writer lock and opens nothing for
writing, so a stuck run can be replayed while it is still stuck, which is exactly
when somebody wants to.

Running it settled four things that reading it would not have:

**Two spend figures, not one.** §20.9's own transcript prints a single
`spend: 0.0000 USD`, which conflates what the *replay* spent — always zero, by
construction — with what the *run* recorded. On a 21-event sim log the recorded
figure is 0.02 at seq 11 and 0.04 at seq 21. One number would have reported zero
for a run that had a bill, so both are printed, labelled, with the reason on the
line: `replay spend: 0.0000 USD (replay does not execute effects)`.

**The closing suggestions named the wrong run.** They echoed the *resolved* run
id, so replaying a directory copied aside printed `fold all of it: arxi run
replay rmtizyc28-ba2261a1` — naming the pristine original rather than the copy
the reader had just been looking at. Every suggestion now echoes the argument as
typed, and a test asserts it by replaying a copy under a different name.

**A missing frozen blueprint is not a small problem.** Delete
`blueprint.snapshot.yaml` and the fold still produces a plausible-looking state —
but the member roster comes back empty and the effect tally drops from 11 to 2,
because with no config there is nobody to spawn a turn for. Quietly folding on
would print a confident, wrong answer, so the file is named in a warning and the
fold continues anyway, which is what makes the state still worth reading.

**`--until-seq` refuses more than it accepts.** It is inclusive; `0` means the
same as absent; a seq past the head is **refused rather than clamped**, because
silently folding the whole log answers a question nobody asked; a negative is
refused rather than clamped; a malformed value is refused *before the log is
opened*, so a typo cannot look like a slow command; and a gap is reported —
`this log holds no seq 5, so the fold stopped at seq 4` — rather than passed off
as the seq requested. One wrinkle is worth writing down: `expandShort` claims any
leading-dash token first, so `--until-seq -1` is answered by the short-flag
expander and only `--until-seq=-1` reaches the negative guard. Both are errors;
they are just different errors, and the second is the one the guard is for.

Fourteen deliberate mutations were introduced to check the tests were worth
having — the `→` in the headline turned to `->`, `tally.add(effs)` replaced with
`_ = effs`, the past-the-head refusal disabled, the missing-snapshot warning
disabled, `head_seq` in the JSON pointed at the reached seq, `os.Getpid()`
printed. All fourteen were caught; none survived.

`run fork` came before it, and it is the only wired verb that is **not** a
projection and not an append to a run either: it writes a *new* run directory
holding a frozen `blueprint.snapshot.yaml` and a re-appended copy of the parent's
events up to `--at-seq`. There is no `run.forked` event type and none was added —
a fork is not something that happens to the parent, and recording it there would
put a row in an append-only log describing a decision made outside it.

It was built because two commands already printed it as *the* remedy for their
own refusals. `run result` and `run prompt` both refuse a finished run and both
answer `arxi run fork <run> --at-seq <n>`, which until this step replied *"is
declared in the surface but not implemented yet"*. That is the fifth instance of
the same defect class in this project — a remedy the tool recommends and the
binary declines — and it is the worst kind, because the refusal is otherwise
correct and the user has nowhere left to go.

**The copied events keep the parent's seqs and the parent's ids**, and that falls
out of `logstore` rather than being arranged: `Append` requires `Seq == 0` and
assigns sequences itself, so a *contiguous prefix* starting at 1 necessarily
comes back numbered 1..N — the same numbers. Ids are copied verbatim, which keeps
`caused_by` chains walkable across the fork boundary; renaming them would have
produced a history that references antecedents no log contains. Exactly one event
is rewritten, `run.started`, gaining `run_id`, `forked_from` and `forked_at_seq`,
and its payload map is **copied rather than mutated** — the caller folds the same
decoded slice, so editing in place would alter the parent's history mid-fold.

**Nothing is driven, and that is the one design decision worth arguing with.**
Every other writer in this binary drives, and `run prompt`'s first draft shipped
the opposite mistake. The asymmetry is the reducer's own guarantee: `Decide` is
pure, so a fork whose events are the parent's events reaches the parent's
decisions. Driving immediately would re-run history at full model price to arrive
where the parent already is. So the closing line names what makes a fork differ —
`arxi run prompt <fork> "what you want instead"` — and branches instead when the
inherited state cannot proceed on its own: `run unpause` for an inherited pause,
`run unpause --budget <more than the ceiling>` for an inherited exhausted budget.
Each of those was **followed by hand** and does what it says, which is the check
this verb exists to earn the right to skip.

Running it also settled two things reading could not. A refusal must happen
**before `MkdirAll`**, so the prefix is folded in memory first and no rejected
fork leaves a half-built `runs/<id>` behind. And the parent's `workspace/` is
deliberately *not* copied, with a line saying so: those are its files as they are
**now**, not as they were at the fork point, so copying them would hand the fork
a future it never had.

`event emit` came before it, and it is the only verb in the binary that
lets something outside a run put a **cause** into it. Every other wired verb
either reads the log or asks the run's own machinery to act; this one appends a
row and lets `wakeWatchers` decide what that row means. It exists because a
blueprint could already declare `{pattern: custom.*, action: notify}` and
nothing in the system could produce a `custom.*` event — the reaction was
declarable and unreachable.

Wiring it found a defect in the **declaration**, not in the command, and it is
the most interesting thing this step produced. `event emit` declared `type` and
`payload` and no `run`, because the entry had been written from the agent's point
of view, where the run is *ambient*: a tool call arrives inside a turn and the
turn already knows which log it belongs to. The same declaration is projected to
the CLI, and a shell has no ambient run — `resolveRunDir` deliberately has no
"the latest one" default, because guessing which run a write lands in is the one
guess an append-only log cannot take back. So `arxi event emit custom.x` had no
run to write to at all. The fix was in the registry rather than in the command:
`run` is now the first positional, matching the fourteen other run-taking
commands, and `-r` went from thirteen commands to fourteen. A parameter that is
positional on thirteen commands and a flag on the fourteenth is precisely the
per-command dialect `shortFlags` exists to prevent.

**`custom.*` is enforced against people too**, not only against agents, and that
is a decision rather than an inherited restriction. Each of the other 32 event
types has a verb that maintains the fields the reducer indexes —
`stage.advanced`'s `to_index`, `agent.blocked`'s `blocked_ref` — so a
hand-written one produces a log its own readers disagree about; a caller who can
write `stage.advanced` can walk a run straight past the quorum its blueprint
declares, and the log will look like it advanced legitimately. `custom.*` is the
one namespace with no invariant, which is exactly why it is the one an outsider
may write in. The refusal that matters most is not `stage.advanced`, though: it
is **`custom.*` itself**, which passes the prefix check and is the string the
user most recently had in their hands, because this command's own output
recommends `arxi event log <run> --type 'custom.*'` for seeing what was written.
Emitting it would mint one event whose literal type contains a `*` and which no
readable pattern matches.

The halted guard is **asymmetric on purpose**, and copying `run prompt` here
would have been wrong. A prompt always spends money, so a paused run refuses it
unconditionally. An emit has two legitimate purposes: it is a cause when a
watcher matches and a *record* when none does. So it refuses only when something
would have acted — a parked cause exits 0 and starts nothing, which is
indistinguishable from the command failing — and it records freely into a paused
run when nothing is watching. Refusing that second case would teach people to
unpause a run in order to write a note in it, which is the opposite of what
pausing is for. The message branches on *which* halt: `run unpause` for paused,
`run why` for blocked, because unpausing a blocked run reports success and leaves
it just as blocked.

Running it also proved that **driving is load-bearing**, which is the same defect
`run prompt` shipped in its first draft. Effects are transient: `wakeWatchers`
returns `SpawnTurn` inside `Decide`'s return value, and a command that appends
the cause and exits has produced a log in which a watcher matched and nothing
happened — while printing a success line and exiting 0. Emitting
`custom.contract_frozen` into a live run whose blueprint watches `custom.*` wrote
seq 11 and then seq 12–15 (`agent.activated`, `llm.response`, `stage.submitted`,
`agent.turn_done`, all `security`): four events that exist only because
`driveResumedRun` executed the effect. And when nothing matches, the output
spends the *most* words, because both outcomes exit 0 and the only thing
distinguishing "nothing is watching `custom.deploy`" from "`custom.deploy` woke
`qa`" is that paragraph — so it names the patterns the run really declares, or
says the run declares none, since "your pattern did not match" and "this run
reacts to nothing at all" send a reader to different files.

`event log` came before it, and it is the verb every other verb has been
quoting. `run show`, `run why`, `run tree` and `run result` are all folds of the
same bytes; this is the bytes. It computes nothing, which makes it look like the
one command where a defect cannot matter, and the opposite is true: it is what a
person opens when they have stopped believing the summaries, so a column it
drops is evidence that exists on disk and cannot be found. That is the whole
design rule — **values are elided, keys never are** — and every elision prints a
note naming `--json`, which marshals `kernel.Event` itself so the wire shape *is*
the file's shape.

Running it against a real 21-event `--sim` log decided four things that no amount
of reading the spec would have. There is **no TS column**, because there is no
usable timestamp: `run.started` carries `"ts":""` and every later event carries
`1970-01-01T00:00:00Z`, so a simulated run has an order and no times, and a
column of identical epoch strings would be six wasted characters per row
pretending to be information. **Selection is by directory and never by scope**,
because `scope` is *empty on every executor event* — `agent.activated`,
`llm.response`, `stage.submitted` and `agent.turn_done` all leave it unset, and
only reducer events carry `scope: "run:<id>"`. A `--scope` filter would have
silently dropped the majority of a real log while looking like it worked. And
`DEPTH` is a column because the chain **was** broken through the executor when
this view was built: reducer events carried `correlation_id`, `caused_by` and
depth 1; executor events carried none of the three and depth 0. The footer named
that gap instead of apologising for the column, which is what made it measurable
and then fixed — see `event trace` above. The column stayed, and now carries real
values on every row: it is how a cascade's distance from its root is read, and how
the note would come back if a producer ever stopped attributing.

The two defects running it found were both in what it *said*, not in what it
computed. `dashIfZero` — right for every other optional column in this binary —
turned all seventeen executor events into `-` while the footer directly
underneath announced a chain "broken at depth 0", a note about a value that
appeared nowhere in the table above it; 0 is not a missing depth, it is the depth
of a root cause. And the timestamp note printed **"1 are empty"** — the *common*
branch, since exactly one event in a real log is written before the clock exists.
A footer whose only job is to be trusted about what the table omitted cannot get
that count's grammar wrong in the same breath. A third landed from the test
rather than the run: on a log where *nothing* carries a ts, the note offered
`--json` as the place to find them, which is a false promise and exactly how a
reader concludes the view is hiding something.

Two smaller decisions are worth stating because both are the less obvious
option. `--since-seq` is **inclusive** and `0` means *no bound*, because the
intended use is a tailer resuming from the last seq it saw, and a script
computing that bound off an empty log passes 0 and must still get everything. A
filter matching nothing **exits 0 with stdout empty** and diagnoses on stderr,
listing the types the log really holds: nothing found is an answer, so
`arxi event log r1 --type 'tool.*' && deploy` must not refuse to deploy a run
that merely used no tools, and the likeliest cause of a miss is a typo the reader
can only fix if they are shown the real spellings. `kernel.MatchEventType` is now
exported and the reducer's `matchPattern` delegates to it, so a watcher and
`--type` cannot come to disagree about what `stage.*` selects.

`run cancel` came before it, and it is the smallest verb in the binary in
the same way `run pause` is — `case RunCancelled` sets `Status` and returns **no
effects** — with one difference that changes everything about the command around
it: cancelling is *final*. An append-only log has no way to unsay it, and the
reducer folds every event arriving at a terminal run into nothing
(`internal/kernel/decide.go`), so a second `run.cancelled` carrying a better
reason would sit in the file where no reading of the log will ever show it. The
command therefore refuses the second cancel and quotes the reason already on
record, and it says at the one moment `--reason` can still be supplied that it
cannot be supplied later.

What running it found was not in this command at all. Cancel a run with a
question outstanding and the question is still in `State.Inbox`, still unanswered,
and now unanswerable — and `arxi inbox approve` printed
*"approved. backend unblocked (r1 seq 7)"* for it, a sentence in which only the
seq was true. Nothing folded that reply, so the member was not working and
anybody waiting on it waited forever. `internal/inbox` now has `ErrRunOver`,
checked **under the writer lock** against the state `Answer` folds itself rather
than the one the caller listed from — because the run can reach a terminal status
between a human reading `arxi inbox` and answering it, which is precisely the race
a cancel creates. The listing gained the other half: a question whose run has
ended is still shown, because the log is not edited, and it is marked
`[run cancelled -- not answerable]` with `answerable: false` in the JSON. A row
that looks exactly like actionable work and is not is how a human's attention
gets spent on nothing.

`run pause` came before it, and it is the smallest verb in the binary:
`case RunPaused` sets `Status` and returns **no effects**, so unlike `run unpause`
there is no loop to drive and the command is finished the moment one payload-less
event hits the log. What that byte buys is `spendingHalted`, which is true for
paused as well as blocked, so every site that would open a turn parks its cause
instead. What it does *not* buy is the reading most likely to cost somebody
money: a pause cannot reach into a turn that is already open, because a turn runs
inside whichever process drives the loop and this is not that process. So the
output says which member's turn survives, by name, and refuses to print "none
interrupted" as a constant.

Running it found two things. The turn count in the promised headline —
§20.6's "2 turns finished, none interrupted" — cannot be `State.Turns`:
`applyActivated` increments that when a turn *opens* and nothing decrements it,
so the fixture with one activation and no `turn_done` printed **"1 turn finished,
backend still open"**, a line contradicting itself in nine words. Finished is
`Turns` minus the turns still open. The second was in `run unpause`: its
exhausted-budget warning was gated on `Status == blocked`, and a pause overwrites
`Status`, so the sequence a user actually performs — block on money, pause to
think, resume — skipped the warning entirely and re-blocked on the next cost with
nothing said. Pausing a blocked run also costs the diagnosis, since
`kernel.Explain` returns early for a paused run, so the pause prints what the run
was stuck on as of the event before it: this is the last moment anything in the
binary can still name it.

`run result` was the verb before it, and it is the sharpest counter-example to
"already correct" this project has produced. §20.1 of the design shows the verb
printing a code review's findings; the log cannot supply them. `stage.submitted`
declares **no payload at all** (`spec/events.md`), the only producer writes
`{agent, stage, simulated}`, and both success paths in the reducer write
*constants* — `"all stages completed"`. So `result_from: last_submit` resolves to
a real event that carries no text, and there is no field anywhere in a run
directory for an agent-authored answer. The verb therefore prints what is
recorded, names the member whose answer it would have been, and says in one line
that the summary is the run's own record rather than that member's work. Printing
"all stages completed" alone and letting it pass for a delegated review is the
failure §10.7 calls the worst kind: a sentence that is still displayed and is
simply untrue. Reading it also found that the reducer sets `StatusCancelled`
without reading the `reason` `spec/events.md` declares, so a cancelled run's
reason survives only in the log — this command is the only place it surfaces.

`run tree` came before it, and it makes the "already correct" half of that
sentence exactly as sharp as it deserves to be. The tree is a projection and
nothing had to be added to the engine to draw it — but the projection is only as
true as what it reads, and reading it found that `applyCost` charges each run's
own ceiling and never rolls a child's spend up into its parent. So the verb
prints the total it measures itself, by summing each run's own spend, and says
out loud when the root's own `tree_spent_usd` disagrees. A projection cannot
corrupt a log; it can still be the first thing to notice that the log does not
say what the design documents claim.

`run prompt` is the exception that proves the rule, and it is why the sentence
above says *projection* rather than *command*. It is the first `run *` verb
built here that **writes**, and the difference showed up immediately: a
projection cannot corrupt a log, so `run list`, `run show` and `run why` could
be wrong only on screen. `run prompt` appends, so every refusal has to happen
*before* the write — an append-only log offers no way to unsay something.

`run list`, `run show` and `run why` were the first three, and building them is the evidence
for that claim rather than a restatement of it: neither needed a new store, an
index, or a `runs.json`. `inbox.OpenRun` already read the log and folded it without taking
the writer lock, and the whole command is a caller asking for every run instead
of one. Had it needed an index, the claim would have been wrong and the right
response would have been to say so rather than build the index.

What the projections cost instead is *judgement about what the state means*, and
that is not free. All three commands compiled, read correctly, and were wrong in
ways only visible by pointing them at a real run that had gone wrong. `run list`
counted answered questions as pending, so it contradicted `arxi inbox` about the
same run. `run show` announced "a turn is queued" for members on a run whose
budget had broken — true of `Member.Runnable()`, false of the run, because the
reducer parks those causes instead of spawning them — and printed a twentyfold
overspend as the ordinary-looking fraction `0.02 of 0.001`. `run why` — whose
declared purpose is "explain why a run is not advancing, **and how to unblock
it**" — printed a cause tree with an empty remedy list on exactly the runs that
needed one, because `Explain` built remedies only from members in
`MemberWaiting` and a run halted by its budget has nobody waiting: the reducer
parks the causes and every member sits idle. The wait graph was empty while the
run was still stuck, so the command answered the one question it exists to
answer with silence. A projection is easy to build and easy to make say
something false; the log being correct does not make the reading of it correct.

That last one is the sharpest version of the lesson, because the missing half
was the half named in the declaration. A test on `Explain` had existed since the
kernel was written and passed throughout — it asserted remedies on a run with a
member blocked on approval, which is a state where the wait graph *is* populated.
The defect lived in the states that test never described.

The one number worth committing to, if only one is wanted: **the system can
reason about a run end to end, can do the work inside one, and can now be
picked back up after a human has answered it.**

That last clause is new, and earning it is what the section below is about.
There were five gaps of the worst kind — a remedy this project *prints* and the
binary *refuses* — and they are now zero: `AskHuman`, then `agent tool policy`,
then `run unpause`, then `run why`, then **`run prompt`**. A gap of that kind is
discovered by the person already in trouble, which is why closing them came
before building anything new.

The fifth was **created by closing the fourth**, and that is worth stating
plainly because it is the pattern rather than the accident. Wiring `run why`
made `kernel.Explain`'s quiescence branch reachable for the first time, and the
top line of the advice it prints there is `arxi run prompt <run> "..."` — which
the binary refused. Closing a gap does not only remove a gap; it *runs code that
nothing had run before*, and that code has its own advice.

Quiescence is also the sharpest of the five, because it is the one state with
no other way out. Every member is idle, no cause is pending, and the advance
rule cannot be met by anybody left. `run unpause` does not apply — the run is
not paused. The inbox does not apply — there is no question. Until this step the
tool could diagnose that state precisely and then offer nothing that worked.

The fourth is worth its own sentence, because it says something about how these
are found. `arxi run why <id>` was printed as advice by *three* separate
commands — including `run show`, added in the step immediately before — and
following that advice answered `arxi run why is declared in the surface but not
implemented yet`. Nothing in the suite noticed, because every test that
mentioned the string printed it rather than ran it. It was found by typing what
the tool said to type. The guard that now covers it lifts the suggestion out of
`run show`'s own output and executes it, so the advice and the implementation
cannot drift apart again.

### The command that printed success and did nothing

Closing the fifth gap produced the most useful defect of the project so far, and
it is a new shape. The previous lessons were *a passing test can describe only
the states where the code works* and *100% coverage of a switch says nothing
about the arms nobody reaches*. This one is one layer further out:

> **A command that reports what it INTENDED cannot be caught by reading its
> output. It has to be caught by reading the state.**

`arxi run prompt <run> "please submit what you have"` answered
`run <run> prompted (seq 16), to backend`. The event was in the log, the seq was
real, the recipient was correctly resolved. Every word of it was true, and the
run was **bit-for-bit as quiescent as before**: `run why` went on printing the
very remedy the user had just followed.

The cause is a single line in the reducer. `applyInjection` hands an idle
member's cause to `spawnCauses`, which *parks* it only when `spendingHalted` —
false on a running run. On every other path it returns a transient `SpawnTurn`
that lives in `Decide`'s return value and is **discarded if nobody is executing
effects**. The first draft of the command deliberately did not drive, on the
reasoning that `arxi inbox` sets the precedent: answering a question is not the
same act as paying for the turns it unblocks.

That reasoning is sound for the inbox and false here, and the difference is what
the append *leaves behind*. An inbox reply is durable in the state — the fold
sets `Replied=true`, so a user who answers and stops can see that they answered.
A prompt to an idle member on a running run leaves **nothing at all**.

The closing line could not rescue it either. It said
`drive it: arxi run unpause <run>` — and `run unpause` refuses a running run
outright, exit 1. So the command written to close the fifth printed-and-refused
gap *printed a sixth*. Not driving did not defer the decision to the user; it
removed it.

Three more defects fell out of fixing that one, all measured rather than
reasoned:

- **A rehearsal could not be rehearsed.** `driveResumedRun` built a live
  executor unconditionally, so every caller had to check `simulated` and print
  *"not driven here"* instead. That protected the user's money and cost them the
  run: nothing else drives, so a `--sim` run could never be advanced after
  `run start` returned. It now drives with the same fake executor `run start`
  uses — no model called, no money spent, same loop, same reducer, same log.
- **The outlook line read the wrong actor.** It said *"it opens a turn for
  backend now"* on a budget-blocked run and then parked the cause instead,
  because `spawnCauses` asks `spendingHalted` **before** it looks at the member
  at all. A line that reads the member alone describes a decision the reducer
  never makes.
- **A refused CAS bricked the run.** The refusal exits 1 directly rather than
  through `fatal()`, so it ran neither the deferred `Close` nor the `atExit`
  hooks. One mis-guarded `--if-seq` left `writer.lock` holding a dead pid, and
  every later command on that run was refused with advice to delete a lock file
  by hand — for a run that had merely been guarded *correctly*. A CAS miss is
  the most ordinary failure this command has; it must not be the one that
  bricks the run.

One test had to be **rewritten rather than fixed**, and the distinction matters.
`TestASimulatedRunIsNotDrivenByAResume` asserted that a simulated resume stopped
at the append. Its own stated rationale forbade a **live** executor; the
assertion forbade driving **at all**. The two are the same thing only if the
fake executor is not an option — and it is, because `run start --sim` already
uses it. The test was pinning more than its reason supported, and that surplus
was the thing keeping a rehearsal un-advanceable. It now pins what the rationale
actually says: the continuation is taken by the fake, and the run moves.

Ten guards cover this, and all ten were confirmed to fail against a deliberate
mutation of the line each one documents — including the two that matter most:
removing the drive, and reverting the executor choice.

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
Because a granted *mutating* tool resolves to `ask`, `bash`, `write` and `edit`
do not reach the runner unattended — they reach a human first. What runs
unattended is `read` and `grep`, the two that only look. Read back through
`tool.Resolve` rather than asserted here:

```
granted read  -> allow      granted write -> ask
granted grep  -> allow      granted edit  -> ask
                            granted bash  -> ask
```

That ordering is deliberate: the confinement in `internal/toolrun`
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

### The approval loop, and the way out

`ask` has a consequence that only appears once you use it. Approving a tool call
respawns the turn — that is what an approval *is* — but the policy still says
`ask`, so the model calls the tool again and is asked again. Approving your way
out does not work; it is a loop with a per-turn price.

```bash
arxi agent tool policy --agent backend --allow bash
```

That is the exit, and `run why` prints it as the second remedy. Four things
about it are deliberate:

**It is CLI-only, and that is a security boundary rather than an omission.** An
agent that can widen its own tool policy does not have a policy. The registry
declares it `CLIOnly`, so it is never exposed as a tool and never reachable over
the protocol — the same line the three `inbox` reply verbs sit on.

**An override cannot grant.** `tool.Resolve` checks the grant list first, so
`--allow bash` on an agent that was never given `bash` changes nothing. An
override that could grant would make the blueprint's `tools:` list decorative,
and the blueprint is the reviewed artifact. The command says this in its own
output, because succeeding while changing nothing observable is the most
confusing thing it could do.

**It is stored, not logged.** Every other durable decision here is an event in a
run's log; this one is a file in `policies/`. The surface declares it with
`--agent` and no `--run`: it is an operator decision about an agent *across*
runs, so writing it into one run's log would mean the next run could not see it.
Overrides also sit deliberately outside the frozen `blueprint.snapshot.yaml`,
because they are a standing answer to "stop asking me about this" rather than a
property of one run.

**It is not retroactive, and the command says so.** The policy is read at
`run start` and copied into that run's executor, so a run already waiting on an
approval is *not* unblocked by changing the policy — answer that one with
`arxi inbox`. Re-reading policy mid-run would mean the rules a run is judged by
could change between two of its own turns. The person running this command is
usually looking at exactly that blocked run, so the limitation is printed in the
output rather than left in a doc comment where only the people who did not need
it would find it.

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

### Picking a run back up

`arxi run unpause <run> [--budget <usd>]` is the third of the three remedies
this project printed and could not honour. `run why` emitted the exact line
`arxi run unpause <run> --budget <higher>` for an exhausted budget,
`spec/events.md` listed it as **the** remedy for a budget block, and §20.6 built
its narrative on "raise and continue as one action rather than a fork". The
binary answered *"declared but not implemented"*.

The interesting part was not writing the command. It was that **the reducer was
the thing that was wrong.** `--budget` was declared on the surface as a "new
spend ceiling", promised by the spec, printed by `why` — and only `run.started`
had ever written `BudgetUSD`. So `run.unpaused` now raises it, and the project's
own rule decided that: *correct the code, not the test*. The promise lives in
the surface and the spec, which makes the reducer the thing that disagrees.

Raising the number is the easy half. Two flags decide whether the raise is a
ceiling or a blank cheque:

- **`BudgetBlocked` must be cleared**, because `applyCost` returns early while
  it is set. A raise that left it would let the run spend straight past the
  *new* ceiling in silence — **raising a budget would buy an unlimited one.**
  `applyCost` already clears that flag when spend falls under the ceiling; a
  raise is the same fact arriving from the other direction.
- **`BudgetWarned` must be cleared**, because 80% of the old ceiling is not 80%
  of the new one. Leaving it set spends the only pre-stop notice on a ceiling
  nobody is measured against.
- **A lower ceiling is refused, not applied.** One under the current spend
  re-breaches on the next event, which is the loop a raise exists to end — and
  "unpause" is not the verb for tightening a budget. The refusal is in the CLI,
  not the reducer: the reducer has nobody to talk to, so it ignores what it will
  not honour, and the CLI is where a person is told why.
- **A missing `budget_usd` leaves the ceiling alone**, because §20.6's first
  example is a bare `arxi run unpause r1`. Reading an absent field as a limit of
  zero would give every plain resume an unsatisfiable ceiling.

Resuming is then **two acts**: append `run.unpaused`, and *drive the loop*. The
split is deliberate and `arxi inbox` does only the first half on purpose —
answering a question is not the same act as paying for the turns the answer
unblocks. Unpause is the other half.

Driving needs a cursor, and **there isn't one to have.** `exec.Loop`'s own doc
rules out both guesses: `Head()` skips the effects of anything a previous pass
had not reached, and zero re-spawns every turn the run already paid for.
`run start` prints the cursor as "resume from here" and **nothing persists it** —
measured, not assumed: a run directory holds only `blueprint.snapshot.yaml`,
`events.ndjson`, `state.snapshot.json` and `writer.lock`. A fresh process
therefore cannot know it, and the choice is which failure to take. `Head()` may
strand a crashed pass's effects, on a run that is likely stuck anyway; zero
re-charges the whole bill on every healthy run. **The tip wins**, and this is a
limitation the command reports rather than hides.

One more thing is read off the log rather than asked: a run whose `run.started`
says `simulated:true` was produced by `exec.Fake`, so it is **not driven**.
Charging real money to continue a rehearsal is the failure worth designing
against, and a `--sim` flag on this command would let two answers differ — with
the money-costing direction being the one a user hits by forgetting a flag.
`kernel.State` has no `Simulated` field (verified: the reducer has no use for the
distinction, which is exactly what makes `--sim` trustworthy), so it comes from
the event.

#### What only running it could find

Four defects, all found by walking the command by hand and none by a unit test:

1. **One ceiling, three numbers.** A run started with `--budget 0.005` had its
   ceiling printed as `0.01` by the start banner, `0.0050` by the block summary,
   and `0.01 -> 10.00` by the raise. The raise is the worst, because it puts
   both ceilings on one line: **the same command contradicted itself about a
   number it had just read out of a single field.** Every instance rounded *up*,
   which is the bad direction twice over — the reader is shown more headroom than
   the run has, so the block that follows looks premature.

   The fix exposed that `usd()` already existed, solving the same problem with
   the same reasoning, so the duplicate I had written was deleted and the shared
   helper widened. Its rule tested **size** (`v < 0.005`); the right test is
   whether two decimals are **exact**.

2. **A fatal under the lock stranded the writer lock.** `os.Exit` does not run
   deferred calls, so a `run unpause` that appended `run.unpaused` and then hit
   an unparseable `policies/backend.json` left `writer.lock` on disk holding
   pid 5871. The next command on that run refused — *"already open for writing
   by pid 5871 … remove `writer.lock` by hand"* — for a run whose log says it
   was successfully resumed. `run start` had the identical exposure.

   Fixed with an `atExit` registry called by `fatal`, not a `store.Close()` in
   front of each `os.Exit`: there were four exit paths under one lock in
   `run unpause` alone, and fixing them one at a time is how the second one gets
   missed. The manual remedy in that message is for a hard kill, which is the
   only case a process genuinely cannot clean up after.

3. **A test that could not fail.** The first guard for defect 2 passed — and
   passed with the fix reverted, which is how the mistake was caught. It resumed
   a `--sim` run, and a `--sim` resume returns *before* the executor is wired up,
   so the `os.Exit` path it claimed to guard was never reached. **Breaking the
   code is the only way to learn that**, and it is the reason the replacement
   asserts its own setup: it checks that `run.started` really said
   `simulated:true` before patching it, and that the bad policy file was really
   read, so a guard that stops reaching the path fails loudly instead of
   vacuously.

4. **A probe that lost part of its denominator.** The surface re-measure first
   reported **22 / 47** against a registry of **49**, because the shell loop let
   `serve` read the remaining paths off stdin. A measurement that quietly drops
   two of its own cases still looks like a result; the fixed probe redirects
   stdin per invocation and prints the total it walked.

### Searching and editing, without lying about either

`grep` and `edit` were declared in `internal/tool.Known` before they existed —
the runner refused them by name and said which kind of refusal it was. They now
have bodies, and the decisions in them are all versions of one rule: **a tool
must not report an answer it did not obtain.**

**`grep` is a regular expression, because that is what the name promises.** A
model told it has grep will send `func \w+\(` sooner or later. Matching that
literally would find nothing, report success, and teach the model that a file
does not contain what it does contain. The pattern is compiled *before* the walk,
so a bad pattern is a refusal naming the escape (`func\(\(`) rather than an
empty result. Go's `regexp` is RE2 — no backreferences, no lookahead, and no
exponential backtracking — which is why `grep` needs no deadline where `bash`
does.

**Two caps, preventing different failures.** `maxMatches = 200`, because a grep
for `"e"` matches nearly every line and tool output becomes an event in the run
log. `maxGrepFiles = 2000`, because *a pattern that matches nothing still walks
the whole tree*: a cap on matches alone cannot stop a no-match grep in a
workspace somebody unpacked a dependency tree into. Hitting either is reported,
not hidden — a caller shown 200 of 250 matches and told nothing reads the list as
complete.

**`edit` refuses two things a permissive tool would accept.** A replacement
matching **nothing is an error, not a silent no-op**: the caller would believe
the file says something it does not, and every later step would be built on that
belief. A replacement matching **more than once is an error unless `all=true`**:
"the first one" is a position in a file the caller cannot see, so which
occurrence got edited would be luck. The count comes back either way, because
that is the fact the caller needs to choose between narrowing the match and
going global. A *missing* replacement is a deletion rather than an error,
matching how `write` treats missing `content`.

Four defects were found by probing the implementation and by breaking each
guard, and all four are the same shape:

1. **A search that could not happen reported "no matches".** Measured:
   `path: nope-dir` and a `path` naming a symlink both returned *"no matches for
   func"*. Two correct behaviours combined into a wrong one — `Resolve` permits a
   final component that does not exist yet, which is right for a *write*, and the
   walk deliberately swallows `WalkDir`'s error callback so one unreadable
   subtree cannot fail a whole grep. A missing root hits that callback once, gets
   swallowed, and the walk ends empty. Now an `os.Lstat` gate refuses both, and a
   single regular file is still a valid root.

2. **A test that asserted the opposite of the truth, and passed.**
   `TestADeclaredButUnimplementedToolSaysWhichItIs` checked that `grep` and
   `edit` had no bodies. After they got bodies it kept passing, because it called
   them with `{"path": "x"}` — incomplete for both, so both still errored, now on
   a *missing argument*. It is replaced by a test that reads `tool.Known`, drives
   every declared name with arguments that should work, and fails if a name is
   added to `Known` with no way to call it.

3. **A comment that credited the wrong control.** The doc said the walk's
   symlink skip was what stopped a link to `/` putting the filesystem in scope.
   Breaking the skip did **not** make the boundary test fail. Measured:
   `filepath.WalkDir` never descends into a symlinked directory (it yields the
   link as one non-directory entry), and `ReadFile` refuses that entry *in the
   kernel* with `O_NOFOLLOW`. The skip is an optimisation. A doc naming the wrong
   control points the next reader at the wrong line to be careful with, so the
   comment now says which layer holds the boundary — and the test still asserts
   the outcome rather than the mechanism, because one written to fail when the
   skip is removed would have pinned an optimisation as though it were a control.

4. **A guard whose own test could not reach it.** Removing the `old == ""`
   refusal did not fail its test: with `all=false` the ambiguity check fires
   first, since an empty string "occurs" at every position. The test only passed
   `all=false`. With `all=true` nothing is in the way — measured, the guard
   removed: `Edit("abc\n", old="", new="X", all=true)` returned **5 replacements,
   no error**, leaving the file as `"XaXbXcX\nX"`. The test now covers both.

Under the policy table above, this puts `grep` on the unattended side and `edit`
behind a human, without either being special-cased: `internal/tool.Mutating` is
`{write, bash, edit}`, and the resolution falls out of it.

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
