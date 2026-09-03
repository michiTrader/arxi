# 20. Use cases: every command of the final design, in the order you meet it

## 20.0 What this document is for

`arxi surface` lists the 46 declared capabilities. A list is not a design: it
tells you what exists and nothing about whether the set is *coherent*. Two
questions a list cannot answer:

1. **Can a real task be completed with only these commands?** If a use case needs
   a step that no command covers, the surface has a hole. A hole found here costs
   an edit to a table; found after the executor exists it costs a migration.
2. **Does any command exist without a use case?** A capability nobody reaches by
   walking a realistic scenario is a capability we are going to implement, test,
   document and maintain for nobody.

So this document walks **use cases**, not commands, and every command has to fall
out of some scenario naturally. The coverage table in §20.12 closes the loop: it
lists all 46 and names the use case that reaches each one. It is generated from
the registry, not maintained by hand — see `use_cases_test.go`.

**Status of the commands.** `schema`, `surface`, `why`, `version`, `blueprint
validate`, `run start` (`--sim` only) and `serve` run today; the rest are
declared and verified by tests but have no executor (ADR-0001 explains why
declaring first is deliberate). So the outputs below are **specifications of
what the command must print**, not transcripts — except in §20.3, which is real
output from the current binary. Where a scenario depends on behavior the kernel
already decides, the section names the reducer function so the doc can be
checked against the code instead of believed.

This paragraph is easy to leave stale, and staleness here has a direction that
costs: it under-reports what exists, so a contributor reads "no executor" and
reimplements a command that is already in the tree and already tested.

Conventions: `$` is a human shell. `>` is an agent invoking the same capability
as a tool. Run ids are `r1`, `r2`; inbox ids are `inbox-1`.

---

## 20.1 UC-1 — First run: one agent, one objective

The smallest complete path, and the one that decides whether the tool feels
usable. A new user has an API key and a task.

```
$ arxi provider add anthropic --api-key-env ANTHROPIC_API_KEY
provider anthropic registered (key from $ANTHROPIC_API_KEY)

$ arxi model list
NAME                     PROVIDER    STATUS
claude-sonnet-4-6        anthropic   enabled
claude-opus-4-1          anthropic   disabled

$ arxi model enable claude-opus-4-1
model claude-opus-4-1 enabled
```

`provider add` takes `--api-key-env`, the *name of a variable*, not the key. A
key passed as an argument lands in the shell history and in the process table of
every user on the machine. Accepting `--api-key` would make the insecure path the
short one.

```
$ arxi agent create reviewer --model claude-sonnet-4-6 --tools read,grep
agent reviewer created (tools: read, grep — policy: allow)

$ arxi run start reviewer "review the diff in HEAD and list real risks" --budget 2.00
run r1 started (budget 2.00 USD, workspace auto→none)
```

`--budget` is mandatory and has **no default** (`req(p("budget", ...))` in the
registry, enforced by `TestBudgetIsMandatory`). Every other ceiling in the system
has a default; this one cannot, because a default spend ceiling is a number the
user never chose and only discovers on the invoice. Making them type `2.00` is the
only way to be sure they know a ceiling exists.

`--tools read,grep` are read-only, so the workspace resolves to `none`: no
isolation is needed for an agent that cannot write. Compare §20.4, where one
`write` changes that decision.

```
$ arxi run list
ID   ACTOR      STATUS     SPENT   STAGE
r1   reviewer   running    0.31    —

$ arxi run show r1
run r1: running (seq 12)
  reviewer: thinking (turn 3)
  budget: 0.3100 of 2.0000 USD spent in the tree

$ arxi run attach r1
[r1 seq 13] tool.call read src/auth.go
[r1 seq 14] llm.response 0.0900 USD
[r1 seq 15] run.result

$ arxi run result r1
3 risks found: (1) token comparison is not constant-time ...
```

`run attach` is `Protocol`-only — not an agent tool. An agent that could attach
to a live run would sit in a blocking stream burning its own turn while waiting
for another agent, which is a deadlock the budget pays for by the second.

---

## 20.2 UC-2 — The tool that needs permission

The first time the tool is not merely convenient. The agent needs `bash`, which
nobody authorized.

```
$ arxi agent create backend --model claude-sonnet-4-6 --tools read,write,bash
agent backend created (tools: read, write — bash: ask)
```

Note what happened without being asked: `read` and `write` came out `allow`,
`bash` came out `ask`. An undeclared policy is `deny`, and mutating tools are
never `allow` by default (`TestMutatingToolsAreNotAllowByDefault`). A permissive
default turns every oversight into a silent hole, and the person who pays for the
hole is never the person who forgot.

Before starting a run, the effective policy has to be inspectable — otherwise the
first time you learn what an agent may do is when it asks, or worse, when it does
not:

```
$ arxi agent list
NAME       MODEL                TOOLS              ADVISORY
reviewer   claude-sonnet-4-6    read, grep         no
backend    claude-sonnet-4-6    read, write, bash  no

$ arxi agent show backend
agent backend
  model:    claude-sonnet-4-6
  tools:    read (allow), write (allow), bash (ask)
  advisory: no
```

`agent show` prints the **resolved** policy per tool, the same way `blueprint
validate` prints resolved defaults in §20.4. `agent list` deliberately does not:
a table that tried to show three policies per row would be unreadable, and the
column that matters at a glance is which agents can write at all.

```
$ arxi run start backend "fix the failing test in ./pkg/auth" --budget 5.00
run r1 started

$ arxi inbox
ID        RUN  AGENT    KIND      QUESTION
inbox-1   r1   backend  approval  run bash: go test ./pkg/auth/...

$ arxi inbox approve inbox-1
approved. backend unblocked (r1 seq 6)
```

The run did not fail. `tool.call_denied` with `policy: ask` is **not an error, it
is a question** (`applyToolDenied` in `decide.go`): it creates an inbox item and
attaches `blocked_ref` so the remedy is derivable. Failing here would mean every
unforeseen tool costs a whole run.

Two other replies exist, and they are different acts:

```
$ arxi inbox reject inbox-1 --reason "do not run the suite, it hits staging"
$ arxi inbox reply  inbox-1 "use the -short flag"
```

`reject` refuses a request and carries a reason that reaches the agent as
context. `reply` answers a *question* (`kind: question`, from `AskHuman`) — there
was nothing to authorize. Collapsing them into one verb would force the agent to
guess whether "no" meant *not allowed* or *not that way*.

If this approval will recur every turn, fix the policy instead of the symptom:

```
$ arxi agent tool policy --agent backend --allow bash
```

That is exactly the second remedy `run why` prints in §20.3. The commands the
diagnosis suggests are commands that exist — which is why the fix strings live
next to the surface and are covered by `surface_test.go`.

---

## 20.3 UC-3 — The run that stops and does not say why

The use case the whole project exists for. This section is **real output**, not a
specification:

```
$ arxi why testdata/scenarios/blocked-on-approval.json
run r1: running
└─ backend: waiting (approval) since seq 5
   └─ waits for approval of the tool "bash" (inbox inbox-1)
   └─ to avoid being asked about this tool again:
└─ budget: 0.4200 of 5.0000 USD spent in the tree

possible remedies:
  $ arxi inbox approve inbox-1
  $ arxi agent tool policy --agent backend --allow bash
```

Two properties of that output are load-bearing.

**It is derived, not hard-coded.** `why.go` walks `blocked_ref` and builds the
remedy from the reference data. There is no `case` per blueprint, so a new
blocking reason gets a diagnosis by bringing its reference — no code change. The
`blocked_ref` contract is in `spec/events.md`, and the fallback exists to catch
violators: an `agent.blocked` with no reference prints *"blocked without a
structured reference: this is a schema violation"* rather than an empty line. A
diagnostic tool that stays silent about its own missing data is worse than no
tool.

**It runs on a JSON file, with no executor.** That is the observable proof that
the reducer is pure (ADR-0001): `run why` is a read of the folded `State` and
needs nothing from the runtime.

Now the harder case — the run nobody flagged, because nothing failed:

```
$ arxi run why r2
run r2: running
└─ nobody is working and nobody can start: the run is quiescent
   └─ stage review advances with quorum:3 and it is not met; everyone who
      could has already submitted: the rule is unsatisfiable with this blueprint

possible remedies:
  $ arxi blueprint validate ./team.yaml
```

Three members, one `advisory`, `quorum:3`. Advisory members never count toward
advance rules (`quorumMet`), so at most two submissions can ever arrive. Everyone
complied. The rule can never hold.

A diagnosis that said only "nobody can start" would leave the user staring at a
blueprint that looks right — this is the case that is hardest to see by eye, so
the diagnosis **always** names the advance rule and distinguishes *waiting for
somebody* from *unsatisfiable*. That distinction is the difference between "wait
longer" and "your config is broken". It is pinned by the `quietCfg` test in
`decide_test.go`, and ADR-0004 is why quiescence is an event you can recover from
rather than a terminal state.

---

## 20.4 UC-4 — A team, and the default nobody asked for

Three agents on the same repository. This is where the defaults stop being
cosmetic.

```yaml
# team.yaml
name: feature-team
members:
  - {name: backend,  role: implementer, tools: [read, write, bash]}
  - {name: frontend, role: implementer, tools: [read, write]}
  - {name: security, role: reviewer,    tools: [read], advisory: true}
stages:
  - {name: build,  advance_when: all,      timeout_ms: 1800000}
  - {name: review, advance_when: quorum:2}
interaction:
  steer_target: coordinator
```

```
$ arxi blueprint validate ./team.yaml
blueprint feature-team is valid (2 stages, 3 members)
  workspace: worktree  (resolved: backend and frontend can write)
  stage build:  advance_when=all      on_timeout=escalate
  stage review: advance_when=quorum:2 on_timeout=escalate
  security is advisory: gives an opinion, does not count toward advance rules
```

`blueprint validate` prints the **resolved** config, not the file back. Three of
those five lines are decisions the user never wrote, and each is a decision they
would need to know to debug the run:

- **`workspace: worktree`** — this is the one the comment in `config.go` calls
  *THE FATAL HOLE*. Two agents that can write, sharing one directory, overwrite
  each other's work; the KV store lock does not prevent it, because the lock
  coordinates *intent* and only the filesystem gives isolation. The trigger is
  narrow and mechanical: any member holding `write`, `bash` or `edit`. So
  `frontend` having `write` is enough, even though `security` is read-only.
- **`on_timeout: escalate`** — a stage timeout almost never means "the work is
  impossible", it means "something got stuck, go look". Failing by default trains
  users to set absurdly long timeouts, which is worse than having none.
- **`advance_when: all`** and **`activation: coalesce`** — the safe reading and
  the cheap one respectively.

Printing the resolved values is what makes these defaults reviewable instead of
folklore. A default you cannot see is indistinguishable from a bug when it fires.

```
$ arxi run start feature-team "implement rate limiting on /api/login" --budget 20.00 --workspace worktree
run r1 started (3 members, stage build)

$ arxi run show r1
run r1: running (seq 34), stage build
  backend:  thinking (turn 4)
  frontend: submitted
  security: inactive (advisory)
  budget: 6.8100 of 20.0000 USD spent in the tree
```

`security` is `inactive`, not idle: an advisory member costs nothing until
something activates it. And `frontend: submitted` is **not** available for work —
conflating those two states is what made the first implementation never detect
quiescence at all (§10.4). Here that distinction is what tells you the run is
correctly waiting on `backend` rather than stalled.

### The default that IS asked for

`team.yaml` above writes `role: implementer` twice, and beside it almost the same
tool grant twice. `arxi role define` is where that pair stops being retyped:

```
$ arxi role define implementer --tools read,write,bash
role implementer defined (tools: bash (ask), read (allow), write (ask))
  file:   roles/implementer.json
  note:   copied into an agent as it is created, so redefining this role
          later changes nothing that already named it. that is deliberate:
          a run is judged by the rules frozen when it started.
  use it: arxi agent create <name> --role implementer --model <id>

$ arxi agent create backend --model claude-sonnet-4-6 --role implementer
agent backend created (tools: bash (ask), read (allow), write (ask))
  file:   agents/backend.yaml
  role:   implementer supplied the tool grant (roles/implementer.json).
          copied as this agent was written, so redefining the role later
          will not change this one.
  run it: arxi run start backend "<objective>" --budget 5.00
```

**A role is read by exactly one thing: `arxi agent create --role`.** The `role:`
line in `team.yaml` is not affected by any of this, and that is deliberate rather
than unfinished — see the note on copying below. A role holds two fields, the tool
grant and `advisory`, because those are the two that are a house style rather than
a property of the individual. The model is not one of them: `--model` is the field
most likely to differ between two agents that do the same job, and putting it in a
role would make `--role` the wrong place to change it.

Three properties are worth stating, because each is a thing a reader will
otherwise assume the other way round.

**An explicit flag wins, and the command says which fields the role actually
supplied.** A role that was named but had nothing left to contribute is reported,
not passed over in silence — otherwise it is indistinguishable from a role that
was ignored:

```
$ arxi agent create frontend --model claude-sonnet-4-6 --role implementer --tools read,write
agent frontend created (tools: read (allow), write (ask))
  file:   agents/frontend.yaml
  role:   implementer is defined and supplied nothing: the command line already
          named every field it sets, and an explicit flag wins.
  run it: arxi run start frontend "<objective>" --budget 5.00
```

**The defaults are copied into `agents/<name>.yaml` as it is written, and never
read again.** Redefining `implementer` tomorrow changes nothing that already named
it; deleting `roles/` entirely leaves every stored agent runnable. This follows
from ADR-0001: `run start` freezes the blueprint into
`runs/<id>/blueprint.snapshot.yaml`, so a role resolved at run time would put part
of a run's rules outside the snapshot, and a redefinition could silently change an
agent that had already been reviewed and approved. The copy is what makes
redefining safe.

**A role nobody defined is a note, not a refusal.** `role:` is a free-form label
that the reducer reads — it picks the steer target by `role == coordinator` and
builds each member's identity from it — and the blueprints in this tree name roles
nothing defines. Refusing an unknown one would break files that are already
correct, so the name is stored and the roles that do exist are printed beside it:

```
$ arxi agent create qa --model claude-sonnet-4-6 --role reviewr --tools read
agent qa created (tools: read (allow))
  file:   agents/qa.yaml
  role:   reviewr is not defined, so no defaults were applied. that is not
          an error: `role:` is a free-form label, and the blueprints in this
          tree name roles nobody defined. it is how a misspelling shows up.
          defined: implementer, reviewer
  run it: arxi run start qa "<objective>" --budget 5.00
```

That list is the whole value of registering a name that carries no defaults:
`--role reviewr` goes from an accepted string to a visible typo. A role file that
exists and cannot be used — unparseable JSON, or a grant naming a tool that does
not exist — is the opposite case and refuses with exit 1, leaving no agent behind,
because there the user asked for defaults that cannot be applied.

`roles/<name>.json` is also the only way to read a role back: the surface declares
no `role list` and no `role show`. It is written 0644, where `providers/` is 0600 —
a role is a default a team commits and reviews, not a credential.

### Composing the team, instead of writing it

The `team.yaml` at the top of this section was typed by hand, and its three members
are the three agents the previous subsection just created. `arxi blueprint create`
is what turns the second thing into the first:

```
$ arxi blueprint create feature-team --members backend,frontend,security
blueprint feature-team created: a team of 3
  file:   agents/feature-team.yaml
  - backend: tools: bash (ask), read (allow), write (ask)
      claude-sonnet-4-6, implementer, counts toward advance
      copied from agents/backend.yaml
  - frontend: tools: read (allow), write (ask)
      claude-sonnet-4-6, implementer, counts toward advance
      copied from agents/frontend.yaml
  - security: tools: read (allow)
      claude-sonnet-4-6, reviewer, advisory
      copied from agents/security.yaml
  stages: work  (the default -- no --stages, so one stage and everybody in it)
  note:   security is advisory: no turn at stage entry and no vote in the
          advance rule, so each stage advances when backend and frontend have submitted.
  run it: arxi run start feature-team "<objective>" --budget 5.00
  edit it: it is an ordinary blueprint -- add a watcher, a timeout, or
           advance_when: quorum:2, then arxi blueprint validate agents/feature-team.yaml
```

The team lands in `agents/`, beside the files it was composed from, and is a file
of the same kind: `agent list` shows it, `run start feature-team` runs it,
`blueprint validate` checks it after an edit. A one-member blueprint was always a
blueprint; this is the same shape with three members in it.

Three lines of that screen are the ones worth their width.

**The grants are resolved, per member.** `--tools read,write,bash` looks like three
equal grants and two of them resolve to `ask`. One agent's worth of that surprise is
survivable; three members composed on three different days is the same surprise
three times over, and this is the first screen where all of them stand together.

**Every line names the file its member came from.** That is the one thing the screen
knows which the new file does not record, because members are **copied, not
referenced** — the same decision as roles above, for the same reason: `run start`
freezes the blueprint into `runs/<id>/blueprint.snapshot.yaml` and hashes it
(ADR-0001, ADR-0002), so a reference would leave part of a run's rules outside the
sha that is meant to be the whole of it. Editing `agents/backend.yaml` tomorrow
changes that agent and no team already composed from it.

**The note does the advance arithmetic.** `advance_when: all` over three members one
of whom is advisory does not mean three, it means backend and frontend. The rule is
in the file; the count it resolves to is not, so the count is printed.

Copying is also what makes the refusals matter: nothing revisits
`agents/backend.yaml` afterwards, so composition is the only moment a team can be
checked against the files it is made of. A member that is itself a team is refused
rather than spliced in, and the refusal prints the `--members` line that names its
members instead. Two members resolving to one name is refused naming both files,
because the name inside a file need not match the filename and the collision is
invisible on the command line. A member whose own `stages:` list names none of the
team's is refused before anything is written — it would otherwise be a team that
runs, spends, and activates that member in no stage at all. All three exit 2 and
leave `agents/` untouched; `cmd/arxi/blueprint_cli_test.go` walks each one at
process level.

What comes out is not quite the `team.yaml` this section opened with. Set them side
by side: the members are the same and the stages are not.

```
$ arxi blueprint validate agents/feature-team.yaml
blueprint feature-team is valid (1 stages, 3 members)
  workspace: worktree  (resolved: backend and frontend can write)
  stage work: advance_when=all on_timeout=escalate
  security is advisory: gives an opinion, does not count toward advance rules
  sha: 70db35843708
```

`--stages build,review` closes half the distance: two stages instead of one, in
order, every member in both. What no flag renders is `timeout_ms`, `quorum:2` or
`interaction.steer_target` — and that is a decision, not an unfinished command. A
flag per blueprint field is a second, worse YAML with a `--` in front of every key,
and the file it would be competing with is already on disk. So the screen's last
line is `edit it:` and it names the verb that checks the edit.

The `workspace:` line is what composing buys over writing. Nobody typed it: the run
of the hand-written team above passed `--workspace worktree` on the command line,
and here it is resolved from `backend` and `frontend` holding `write` — the default
this section has already argued is not optional. Started as the screen suggests, the
team runs and stops where the rule says:

```
$ arxi run start feature-team "implement rate limiting on /api/login" \
    --budget 20.00 --sim --run-id r1
run r1 started (budget 20.00 USD, workspace auto→worktree)
run r1 succeeded (seq 11, stopped by reaching a terminal status)
  stage:  work
  turns:  2
  spent:  0.0200 of 20.0000 USD in the tree
```

Two turns, not three, and `run show r1` says which member did not take one:

```
  backend   submitted  (submitted)   role implementer, 1 turn, 0.01 USD
  frontend  submitted  (submitted)   role implementer, 1 turn, 0.01 USD
  security  inactive   (advisory)    role reviewer
```

Which is the reading this section began with, three transcripts earlier and on a
file nobody wrote by hand: `inactive` is not idle, and `submitted` is not available.

---

## 20.5 UC-5 — Steering a run without restarting it

The objective was right and incomplete. The user learns something while the team
is mid-flight.

```
$ arxi run steer r1 "rate-limit by API key, not by IP — we are behind a CDN"
run r1 steered (seq 42), to coordinator
  it is queued: coordinator is mid-turn, and the cause is drained when that turn finishes
```

Queued, not applied, and not interrupting. Interrupting the turn would throw away
tokens already paid for, and with frequent steering the agent never finishes a
turn while the bill keeps growing (ADR-0005). Waiting is slower and strictly
cheaper.

The addressee came from `steer_target: coordinator` in the blueprint, which is
the one field left in `Interaction` after `turn_source` was retired.

`run prompt` is the sibling verb and the distinction is real:

```
$ arxi run prompt r1 "also add a metrics counter" --to backend
```

`steer` **corrects the course** of work in flight; `prompt` **injects a new
cause**. Their defaults do *not* differ: both default to `--on-busy=queue`,
because queueing is the only mode `applyInjection` implements and
`--on-busy=steer` names precisely the alternative ADR-0005 discarded. `steer`
therefore refuses that mode and cites the ADR, rather than accepting a flag it
would ignore. Internally they are the same mechanism (`applyInjection`), which is
the point of ADR-0005: `on_busy: queue`, follow-up and coalescing are one
mechanism, not three features. But the user is stating a different intent, and
the log records which one (`agent.steered` against `run.prompt`), so `event
trace` can later show whether a change of direction or a new requirement caused a
turn.

For a script rather than a human, the write must be conditional:

```
$ arxi run steer r1 "use a sliding window" --if-seq 41
arxi run steer: not appended -- the run moved.
  you guarded on seq 41 and run r1 is at seq 47, so 6 event(s) happened since you looked.
  nothing was written. read what changed and decide again:
    arxi run show r1
    arxi event log r1 --since-seq 42
```

CAS on `seq`, not on the turn: `seq` identifies a *version of the state*, a turn
spans many states (ADR-0006). A CAS on the turn would sometimes pass when it
should fail, which is the worst thing a CAS can do. Without `--if-seq` the
semantics are last-write-wins, which is fine for a human at a terminal and wrong
for automation.

A guard the run has passed and a guard it never reached are different mistakes,
and the refusals say so differently. Above, six events landed and re-reading the
tail is the fix. A guard *ahead* of the head cannot be caught up to at all — the
seq came from another run or was mistyped — so that refusal says the run never
reached that seq, and offers the whole log rather than a `--since-seq` slice past
the end, which would print nothing.

Five steers arriving while `backend` is busy produce **one** turn carrying five
causes, and `SpawnTurn.Coalesced` records that it was five. That number is a
direct billing multiplier — 5x — and a saving that is not in the log is a saving
nobody can audit or defend.

---

## 20.6 UC-6 — Pause, budget exhaustion, and resuming

The ceiling exists to be hit; what matters is what happens then.

```
$ arxi run pause r1
run r1 paused at seq 52 (2 turns finished, none interrupted)

$ arxi run unpause r1
run r1 resumed
```

Now the ceiling:

```
$ arxi run show r1
run r1: paused (seq 61)
  backend: waiting (budget)
  budget: 20.0000 of 20.0000 USD spent in the tree

$ arxi run why r1
run r1: paused
└─ paused by explicit request
└─ backend: waiting (budget) since seq 60
   └─ the budget of the tree ran out

possible remedies:
  $ arxi run unpause r1 --budget <higher>
```

**Block and ask, do not kill.** The work already done is worth money already
spent; killing the run throws away the asset and the audit trail together. The
human decides whether to raise the ceiling or stop — which is why `run unpause`
carries an optional `--budget`, making "raise and continue" one action rather
than a fork.

`budget: 20.0000 of 20.0000` is the **tree**, not this run. With nested spawn the
figure for one run alone is a misleading fraction (§20.7).

Cancelling is the other outcome, and it takes a reason:

```
$ arxi run cancel r1 --reason "requirement changed, rate limiting is deferred"
run r1 cancelled at seq 61
```

`--reason` is optional in the surface and worth writing anyway: it lands in the
log, so `run list` six weeks later distinguishes a run that was abandoned from
one that failed. Both look identical without it.

---

## 20.7 UC-7 — Delegation, and why the budget belongs to the tree

An agent decides the task is too big and delegates. This is the scenario where a
naive budget design silently fails.

```
> arxi_run_start {"actor": "researcher", "prompt": "survey rate limiting
  algorithms", "budget": 3.00}
run r2 started (parent r1, spawn_depth 1)
```

`run start` is an `AgentTool` with `ToolPolicy: ask`, so by default this
delegation is an inbox question, not an automatic spend. An agent that can spawn
children unattended can multiply the bill without any human in the loop.

```
$ arxi run tree r1
r1  feature-team   running     14.20 / 20.00 USD
└─ r2  researcher  succeeded    2.90
   └─ r3  fetcher   succeeded    0.60
tree total: 17.70 of 20.00 USD
```

`--budget 3.00` on `r2` is a **sub-ceiling inside the parent's pool**, not a new
allowance. `TreeSpentUSD` accumulates the whole subtree, so `r3` spending money
moves the root figure. Without this, N levels of delegation multiply the ceiling
by N and the `--budget 20.00` the user typed in §20.4 becomes decorative — the
worst kind of failure, because the number is still displayed and is simply
untrue.

`run tree` exists as a separate verb from `run show` for this reason: the honest
figure is a property of the tree, so there has to be a view whose subject *is* the
tree.

```
$ arxi run result r2
sliding window over Redis is the best fit: ...
```

`result_from` defaults to `last_submit`, so the result of a run is the last
submission rather than a concatenation of everything said. A dump of the whole
transcript is not a result; it moves the summarizing work back to the human who
delegated precisely to avoid it.

---

## 20.8 UC-8 — Coordinating without stepping on each other

Two agents that can both write need to share facts and avoid collisions. All five
of these capabilities are agent tools with `allow` policy — they are cheap,
non-destructive and needed constantly, so requiring approval would make
coordination the most expensive thing in the run.

```
> arxi_state_set {"key": "api_contract", "value": "POST /login {key, ts}"}
> arxi_state_get {"key": "api_contract"}
POST /login {key, ts}
```

`state set` takes `--if-seq` too: the same CAS as §20.5, for the same reason.
Two agents doing read-modify-write on one key is exactly the lost-update race,
and the fix is a version token, not a lock.

```
> arxi_state_lock {"key": "migrations/", "ttl": "10m"}
lock acquired (holder: backend, expires in 10m)
```

The lock is **cooperative** and `--ttl` is effectively required in practice: a
lock with no expiry, held by an agent that crashed mid-turn, stalls the run until
a human notices — which is the quiescence of §20.3 caused by the very mechanism
meant to prevent conflicts. And note what the lock does *not* do: it does not give
filesystem isolation. That is why `workspace: worktree` is a separate default
(§20.4). The lock coordinates intent; the filesystem provides separation.

```
> arxi_state_unlock {"key": "migrations/"}
lock migrations/ released (was held by backend)
```

The hand-back is what makes the `--ttl` above a **lease** rather than a deadline.
`backend` asked for ten minutes and was done in two; without this call the
frontend waiting on `migrations/` sits out the other eight for no reason, and the
run pays for the safety margin of every lock in it. The expiry bounds the cost of
a crash; the release is how the normal case stays fast.

```
$ arxi state unlock r1 migrations/ --force
run r1 released migrations/ (seq 47)
  taken from backend, whose lease ran to 2026-08-26T14:11:00Z and had not run
  out: recorded as "forced", and the log names this shell as the one that ended it
  frontend is blocked on migrations/ and is NOT woken by this: ...
```

`--force` is needed in exactly one case: a lease that has **not** lapsed and
belongs to somebody else. Ending work in flight is a decision, so it has to be
spelled and it is recorded as one — the release carries
`previous_holder: "backend"` and `reason: "forced"`, and `arxi event log r1
--type lock.*` says who ended it. A lease that has already lapsed needs no flag
and is recorded as `expired`, with `expired_at` as the evidence for the judgement;
the holder releasing its own key is neither.

The reducer does not check who releases, and that is the design rather than a gap:
a release honoured only from its own holder could never reclaim the key of an
agent that crashed mid-turn, which is the stall this section opened with, and the
only way round it would be a shell writing an event that claims to be that agent.
So who *may* release is the writer's judgement, and `reason` is where the
judgement is kept. Freeing the key does not by itself wake the member waiting for
it: that member moves when a `lock.*` watcher opens a turn, which is why a
blueprint coordinating on locks declares one.

```
> arxi_event_emit {"type": "custom.contract_frozen", "payload": "{\"v\":2}"}
```

Agents can emit **only** in `custom.*`. If they could emit `stage.advanced`, they
could skip the advance rule of their own blueprint — an agent voting itself past
the quorum that was supposed to constrain it. The namespace restriction is the
whole enforcement.

```
$ arxi event log r1 --type stage.* --since-seq 40
seq 41  stage.submitted   frontend
seq 44  stage.advanced    build → review
seq 45  stage.entered     review

$ arxi event trace e44
e44 stage.advanced (depth 2)
└─ caused_by e41 stage.submitted (frontend)
   └─ caused_by e12 run.prompt (human)
```

`event log` filters; `event trace` follows causality. They answer different
questions — *what happened around then* versus *why did this happen* — and only
the second can attribute a spend to the decision that caused it. `stage.advanced`
appearing before `stage.entered` is the semantic ordering that forces
`sort.SliceStable` in `orderEffects` (§10.3).

---

## 20.9 UC-9 — Understanding a finished run: replay and fork

A run from last week produced something wrong. Nothing is live any more.

```
$ arxi run replay r1 --until-seq 44
[replay] seq 44 stage.advanced build → review
state at seq 44: stage review, backend idle, frontend submitted
  spend: 0.0000 USD (replay does not execute effects)
```

Spend is exactly zero because `replay` is the fold with no executor. Same
function as `run`, same code path — not a reimplementation that drifts (ADR-0001).

This is also where ADR-0002 earns its keep: the reducer reads the frozen
`blueprint.snapshot.yaml`, never the live file. If the blueprint had been edited
since — say `quorum:2` became `all` — a replay against the current file would
advance where the original run stalled, and report a state that never existed. It
would not error; it would just be confidently wrong, which is the failure mode a
debugging tool can least afford.

```
$ arxi run fork r1 --at-seq 44 --budget 8.00
run r4 forked from r1 at seq 44 (blueprint: ./team.yaml, re-read)
```

Fork is how you change your mind. Editing a live run's blueprint is impossible by
design; `fork` branches with the new config while `r1` stays intact and
reproducible. That is what makes "try it the other way" a cheap experiment
instead of a destructive edit — and why forking is the honest answer to the
unsatisfiable `quorum:3` of §20.3 once the blueprint has been fixed.

---

## 20.10 UC-10 — Scheduled work, and the failure mode of automation

The team runs nightly. Nobody is watching, which changes every default.

```
$ arxi trigger create nightly-audit \
    --on "cron:0 3 * * *" \
    --then "run start security-team 'audit dependencies for new CVEs'" \
    --budget 5.00 --budget-period day \
    --on-missed skip --overlap skip
trigger nightly-audit created (next: 2026-08-27 03:00Z)
```

Four required flags, which is unusual for this surface, and each covers a way
unattended automation becomes expensive:

- **`--budget` + `--budget-period`** — a per-invocation ceiling is not a ceiling
  for something that fires 365 times a year. The period is what makes the number
  mean anything, so neither is optional.
- **`--on-missed=skip`** — the machine was asleep for four days. `catchup` would
  fire four runs at once, at 3am, with nobody watching. Skipping is the only safe
  default for missed *scheduled* work: nightly audits are not a queue to drain.
- **`--overlap=skip`** — last night's audit is still running. Starting a second
  gives two agents writing the same repo, which is §20.4's fatal hole reached by a
  different road.

`trigger` is also the name that was chosen over `schedule`
(`TestScheduleDoesNotExist`): triggers fire on event patterns as well as cron, and
naming the concept after only one of its two modes would make the event-driven
half look like an afterthought.

```
$ arxi trigger list
NAME            ON              STATUS   LAST      NEXT
nightly-audit   cron:0 3 * * *  active   ok        2026-08-27 03:00Z

$ arxi trigger show nightly-audit
$ arxi trigger pause nightly-audit
```

`pause`, not delete — the same reasoning as `run pause`. Silencing a noisy
trigger while investigating should not destroy its configuration and history.

### Something has to be watching the clock

The four commands above are the whole of what a user *configures*, and none of
them fire anything. `trigger create` writes a file; `trigger list` reads it and
computes what the next firing *would* be. Until something checks the clock, the
`NEXT` column is a prediction about a thing that never happens and `LAST` says
`never` forever.

```
$ arxi trigger run --once
nightly-audit    started      due at 2026-08-27T03:00:00Z
weekly-report    not due      next at 2026-08-31T09:00:00Z
stale-cleanup    skipped      due at 2026-08-27T03:00:00Z; dropped because 1
                              execution still in flight (overlap: skip)
```

```
$ arxi trigger run --interval 30s
```

`trigger run`, not `scheduler run`: the noun already exists, and a second
top-level verb would present the scheduler as a separate subsystem rather than
as the thing that makes the other four commands mean anything.

**It is not an agent tool**, and the reason is different from every other
exclusion in §20.12. Those are withheld for what they do directly. This one is
withheld for what it does *transitively*: the scheduler starts whatever `--then`
names, for every stored trigger, unattended, until stopped. An agent permitted to
start it is an agent permitted to run the union of everything anybody ever
scheduled — and a human approving "may I run the scheduler?" is not going to
reconstruct that from the request. `trigger create` is `ask` for a related
reason; this is the same hole reached from the other side.

`--once` exists because the useful half should be scriptable. A cron entry, a
systemd timer or a CI step wants one pass and an exit code, not a process to
supervise — and a single tick is also the only shape of this command that a test
can drive end to end without waiting on a wall clock.

`--dry-run` reports what *would* fire and starts nothing. The case it is for is
the one where automation has been silently broken for a month: it prints the
missed count and the reason for every trigger, which is how an operator finds out
that "skipped 4 nightly audits" is the actual state of their health dashboard.

---

## 20.11 UC-11 — Did the change help? Eval

Prompt changes get judged by anecdote unless something measures them.

```
$ arxi eval run ./suites/review-quality.yaml --budget 12.00
eval e20260828T223546: 20 cases, 20 judged, 11.30 USD of 12.00
  pass rate: 0.65 (13 passed, 7 failed)
  stored:    evals/e20260828T223546.json

... edit the prompt, run it again ...

$ arxi eval list
ID                  SUITE           PASS  JUDGED  COST   NOTE
e20260828T224101-2  review-quality  0.80  20/20   14.04
e20260828T223546    review-quality  0.65  20/20   11.30

compare two: arxi eval compare e20260828T223546 e20260828T224101-2

$ arxi eval compare e20260828T223546 e20260828T224101-2
                   e20260828T223546  e20260828T224101-2     delta
pass rate                      0.65                0.80     +0.15
mean cost USD                 0.565               0.702    +0.137
mean turns                      3.2                 4.1      +0.9
```

`eval list` is not a convenience beside `compare`; it is what makes `compare`
reachable. A run id is the UTC second it started, because there is no counter to
mint `e1` and `e2` from without reading every existing run first -- and nobody
retypes `e20260828T224101-2` from memory. Without a listing, `compare` takes two
arguments the user has no way to discover, and the real workflow becomes
`ls evals/`: a person reading the storage layout because the tool declined to
tell them. The `-2` on the second id is there because two runs can start inside
one second, which is what a scripted loop does.

A run is never rewritten. `compare` cites runs by id, and a citation that keeps
resolving while its numbers change is worse than one that breaks, so `eval run`
refuses an id that already exists rather than replacing it. There is no pruning
either: "keep the last N" deletes the interesting run, which is old by
definition, and a retention policy eventually deletes the baseline somebody is
about to cite.

`eval compare` reports cost next to quality on purpose. A prompt change that
improves the pass rate by 15% while raising cost 24% is a trade-off, not a win,
and a tool that showed only the pass rate would present it as one. The system
tracks spend everywhere else (§20.6, §20.7); the evaluation view has to as well,
or the one place where you deliberately compare alternatives is the one place the
cost is hidden.

`eval run` requires `--budget` for the same reason `run start` does — more so: a
20-case suite is 20 runs.

---

## 20.12 The agent-side surface

Everything above was a human at a terminal. An agent reaches the same
capabilities through one declaration:

```
$ arxi schema
{"surface_version": 1, "tools": [{"name": "arxi_run_why", ...}]}

$ arxi serve --listen unix:///tmp/arxi.sock
```

`run why` on the CLI, `arxi_run_why` as a tool name, and `run.why` as a protocol
message type are three **mechanical projections of one registry entry** —
`strings.Join(c.Path, ".")` and friends, not a translation table
(`TestOneSingleSurface`). That is why the verb is `cancel` and not `abort`
(`TestAbortDoesNotExist`): the CLI verb name *is* the protocol message name, so a
synonym anywhere would fork the vocabulary and require a hand-maintained mapping
forever.

Of 50 declared capabilities, **34 are exposed as agent tools**. The 16 that are
not are a security boundary, not an oversight:

| not an agent tool | why an agent must not have it |
|---|---|
| `provider add`, `model enable`, `model disable` | credentials and model availability are operator decisions; an agent that can enable models can route itself to a more expensive one |
| `agent tool policy` | an agent that can widen its own tool policy does not have a policy |
| `role define`, `blueprint create`, `blueprint install` | these define what agents *are* and how they are judged; installing a blueprint is closer to installing code than to doing work |
| `inbox approve`, `inbox reject`, `inbox reply` | these are the human's side of the conversation. An agent that could approve its own inbox item turns `ToolPolicy: ask` into `allow` |
| `run attach` | a blocking stream that burns a turn while waiting (§20.1) |
| `design`, `serve` | operator surface: an interactive designer and a socket server |
| `surface`, `version` | operator surface, and redundant besides: `schema` already gives an agent the same capability list in a form it can parse, so exposing the human-readable rendering adds a second answer to one question. `version` describes the binary the agent is already running inside, which is a fact it cannot act on |
| `trigger run` | the only **transitive** exclusion: it starts whatever every stored trigger's `--then` names, unattended. An agent granted this one verb is granted the union of every action anybody ever scheduled, which no human reading the request can reconstruct |

Note three that *are* tools and might look like they should not be. `agent create`
is exposed, but with `ToolPolicy: ask` — building a sub-team is legitimate and
costs a human approval. `schema` is exposed with `allow`, because an agent reading
the list of its own capabilities is the least dangerous operation in the system.

`state unlock` is the third and the one that deserves its own sentence, because
its `--force` lets one agent end a lease another agent still holds — authority
nothing else on the `allow` list has. The param is agent-visible as a consequence
rather than a preference: every declared param is projected into the tool schema,
so there is no way to declare a flag the CLI has and an agent does not. It is
`allow` anyway for the reason §20.8 opens with — a release only a human can
perform means every agent lock runs to its full expiry, so the crashed-holder
stall returns for every early finish — and what makes that tolerable is that the
authority is **not deniable**: the release records `previous_holder` and
`reason: "forced"`, so `arxi event log <run> --type lock.*` names who broke whose
lease. `ask` was the alternative and was rejected because the common case,
releasing a lock you hold yourself, would then cost a human approval every turn.

The general rule: an agent may do things **inside** a run, and may not change the
rules the run is judged by. `state set` is a tool and `agent tool policy` is not,
because the first is work and the second is self-authorization. The three `inbox`
reply verbs are on the far side of that line for the same reason: they are how a
human answers, and a system where the asker can also answer has no approval
mechanism at all.

---

## 20.13 Coverage

Two claims have to hold, and neither should be checked by reading:

1. Every declared capability appears in some use case above. Otherwise we are
   maintaining something nobody needs.
2. Every command named in a use case exists in the registry. Otherwise this
   document promises a CLI we do not have — the same failure as the stale doc
   paths found during the translation pass.

Both are enforced by `internal/surface/use_cases_test.go`, which parses this file
and cross-checks it against `Registry`. If you add a capability and no scenario
reaches it, the test fails and names it. If you write an example using a verb that
does not exist, the test fails and names that too.

That is the difference between a document that describes the design and a document
that constrains it. Prose drifts from code silently; this one cannot drift without
turning the build red.
