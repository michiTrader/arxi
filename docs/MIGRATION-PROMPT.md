# Prompt: Spanish → English migration pass

> **Status: completed.** This pass has been carried out; the repository is
> English-only. The document is kept as the record of what was asked for and
> what the acceptance criteria were, because the ADRs reference decisions that
> were re-examined during the migration.
>
> The Spanish fragments below are **deliberate**: they are the "before" side of
> the before/after examples. They are the one place in the repo where Spanish is
> not a violation of `AGENTS.md`, because quoting the original is the whole point
> of a translation example. Do not "fix" them — replacing them with English
> would leave two identical snippets and destroy the instruction.

Copy everything below the line into the other AI. It is written to be
self-contained.

---

## Task

Translate the entire `arxi` repository (`github.com/michiTrader/arxi`, Go 1.22)
from Spanish to English. This is a **dedicated, mechanical migration pass**: not
behavior changes, not refactors, not design improvements, not "while I'm here"
fixes. Translation only.

Read `AGENTS.md` first — it states the English-only policy this pass exists to
satisfy. The end state is a fully English repository with zero Spanish content.

## Context you need before starting

`arxi` is an agent orchestration system built on one pure reducer:

```go
Decide(State, Event, Config) -> (State', []Effect)
```

The codebase is ~3,500 lines of Go plus ~700 lines of Markdown. There are 45
tests across three packages and they all pass right now. **They must all still
pass when you are done.** That is the acceptance criterion.

Scale, measured — so you can size the job rather than guess:

- **777 lines currently contain accented Spanish characters** across `.go` and
  `.md` files. That is the floor, not the total: plenty of Spanish has not
  accents.
- **8 references to `docs/design/10-execution.md`** need updating when the file
  is renamed.
- 45 test names, all Spanish.
- Largest single file: `internal/kernel/decide.go`, ~700 lines, comment-dense.

This is a multi-hour job. Do not attempt it in one pass — follow the ordered
steps at the bottom and commit after each.

Verify the baseline before changing anything:

```bash
go build -o /tmp/arxi ./cmd/arxi
go test -count=1 ./...
```

If Go is missing:

```bash
cd /tmp && curl -sSLO https://go.dev/dl/go1.22.5.linux-amd64.tar.gz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.22.5.linux-amd64.tar.gz
export PATH=$PATH:/usr/local/go/bin
```

## What to translate

### 1. Comments — the bulk of the work

Nearly every comment is Spanish, and they are dense. They do not describe what
the code does; they argue **why a decision is right and what breaks otherwise**.
Preserve that argumentative force. A comment that explains a costly trap must
still read as a warning in English, not as a flat restatement.

Example — this is the register to match:

```go
// SUBTILEZA CARA: only MemberIdle. The tentación is incluir MemberSubmitted
// ... that breaks the detección of quiescence of the peor way
```

```go
// EXPENSIVE SUBTLETY: MemberIdle only. The temptation is to include
// MemberSubmitted ... that breaks quiescence detection in the worst way
```

Do not soften, shorten or summarize. If the Spanish spends four lines on why an
alternative is wrong, the English spends four lines on it. These comments are the
main defense against someone "simplifying" a load-bearing decision later.

Terminology to keep consistent throughout:

| Spanish | English |
|---|---|
| stage | stage |
| member | member |
| turn | turn |
| effect | effect |
| budget | budget |
| quiescence | quiescence |
| block / blocked | block / blocked |
| stalled | stuck |
| observador | watcher |
| bandeja (of input) | inbox |
| approval | approval |
| executor | executor |
| reducer | reducer (keep) |
| surface | surface |
| tree (of runs) | tree |

### 2. Identifiers

Most are already English (`applyStageEntered`, `checkQuiescence`,
`wakeWatchers`). A few are Spanish or mixed and must be renamed:

- `hastaQuiescencia` → `untilQuiescent` (test helper in `decide_test.go`)
- `quietCfg` → keep (already English)

Scan for others. Use `gofmt`-safe renames — rename every reference, not just the
declaration. `go build ./...` catches misses.

### 3. Test names — all 45

Every test is named in Spanish and every one must be renamed. Examples:

| current | rename to |
|---|---|
| `TestSubmittedNoEsRunnable` | `TestSubmittedIsNotRunnable` |
| `TestQuiescenciaSeDetectaConDiagnostico` | `TestQuiescenceIsDetectedWithDiagnosis` |
| `TestPresupuestoAvisaYAgotaSobreElArbol` | `TestBudgetWarnsAndExhaustsAcrossTree` |
| `TestSteerAgenteOcupadoSeEncolaNoAbreTurno` | `TestSteerToBusyAgentQueuesInsteadOfOpeningTurn` |
| `TestToolDenegadaCreaInboxConReferenciaEstructurada` | `TestDeniedToolCreatesInboxWithStructuredRef` |
| `TestStageTimeoutEscalaNoFalla` | `TestStageTimeoutEscalatesInsteadOfFailing` |
| `TestAdvisoryNoCuentaParaElQuorum` | `TestAdvisoryDoesNotCountTowardQuorum` |
| `TestFoldEsDeterministaYNoMutaLaEntrada` | `TestFoldIsDeterministicAndDoesNotMutateInput` |
| `TestKernelEsPuro` | `TestKernelIsPure` |
| `TestNoExisteAbort` | `TestAbortDoesNotExist` |
| `TestWorkspaceWorktreePorDefault` | `TestWorkspaceDefaultsToWorktree` |

The name must keep stating **the invariant being protected**, not just the
subject. `TestSubmittedIsNotRunnable` is right; `TestMemberState` is not — it
loses the claim.

### 4. Test failure messages

Every `t.Fatalf` / `t.Errorf` message. These are load-bearing: each one names
the **consequence** and the **remedy**. Keep that structure.

```go
t.Fatal("two folds of the same log dieron states distintos: replay not works for nothing")
```

```go
t.Fatal("two folds of the same log produced different states: replay is worthless")
```

Do not reduce any of these to `t.Fatal("mismatch")`. That is explicitly
forbidden by `AGENTS.md`.

### 5. User-facing strings

CLI usage text in `cmd/arxi/main.go`, all capability descriptions in
`internal/surface/surface.go`, the `Why` output lines in
`internal/kernel/why.go`, and the reducer's diagnostic strings in
`internal/kernel/decide.go`.

The reducer diagnostics are the highest-value strings in the project — they are
what a user reads when a run is stuck. Translate with care:

```
"budget agotado (%.4f of %.4f USD en the tree). ¿subir or cancel?"
"budget exhausted (%.4f of %.4f USD across the tree). raise or cancel?"

"; ya submitted all the that could: the rule is unsatisfiable with this blueprint"
"; everyone who could have submitted already did: the rule is unsatisfiable with this blueprint"

"stage " + st.Name + " advances with " + st.AdvanceWhen + " and not is meets"
"stage " + st.Name + " advances on " + st.AdvanceWhen + " and that is not met"
```

Keep format verbs (`%.4f`, `%s`, `%d`) and concatenation structure intact.

### 6. Documentation

`README.md`, `AGENTS.md` (Spanish parts only — the policy sections are already
English), `docs/adr/*.md` (all 7 plus its README), `docs/design/10-execution.md`,
`spec/events.md`.

**Rename `docs/design/10-execution.md` → `docs/design/10-execution.md`** and
update every reference to it. It is cited in at least: `cmd/arxi/main.go` usage
text, `internal/arch_test.go` failure message, `README.md`, and several ADRs.
Grep for `10-ejecucion` to catch them all (the file is now `10-execution.md`).

ADR filenames are Spanish too. Rename them and update the index and all
cross-references:

```
0001-pure-reducer.md            → 0001-pure-reducer.md
0002-log-is-truth.md        → 0002-log-is-truth.md
0003-effect-classes.md        → 0003-effect-classes.md
0004-quiescence-as-event.md → 0004-quiescence-as-event.md
0005-one-injection-mechanism.md → 0005-one-injection-mechanism.md
0006-cas-on-seq.md           → 0006-cas-on-seq.md
0007-go-instead-of-rust.md       → 0007-go-instead-of-rust.md
```

Use `git mv` so history is preserved.

## What NOT to translate — read this carefully

### The wire contract

Event type strings, JSON field names and enum **values** are protocol, not prose.
They are already English and must stay byte-identical:

- Event types: `run.started`, `stage.entered`, `agent.steered`, `tool.call`,
  `run.quiescent`, etc.
- JSON tags: `run_id`, `blueprint_sha`, `budget_usd`, `if_seq`, `blocked_ref`,
  `cost_usd`, `diagnosis`, etc.
- Status/state values: `"running"`, `"idle"`, `"thinking"`, `"waiting"`,
  `"succeeded"`, `"failed"`
- Config values: `"worktree"`, `"escalate"`, `"coalesce"`, `"all"`, `"quorum:N"`,
  `"queue"`, `"steer"`, `"reject"`
- Surface paths and derived names: `run start` / `arxi_run_start` / `run.start`

Changing any of these is a **breaking protocol change**, not a translation.

### The golden fixture — this will bite you

`testdata/scenarios/blocked-on-approval.json` contains Spanish **output
strings**, because it is a recorded snapshot of `Why` output:

```
"waits approval of the tool \"bash\" (inbox inbox-1)"
"budget: 0.4200 of 5.0000 USD spent en the tree"
"aprobar bash for backend?"
```

When you translate the strings in `why.go` and `decide.go`, `TestGolden` will
fail. **That failure is correct and expected.** Do not edit the JSON by hand and
do not weaken the test. Regenerate it:

```bash
UPDATE_GOLDEN=1 go test ./internal/kernel
```

Then inspect the diff and confirm the only changes are the translated strings —
if any structure, ordering, or numeric value changed, you altered behavior
somewhere and must find it.

### Anything else

No logic changes. No signature changes. No reordering. No dependency changes
(the project is standard library only, deliberately). If you believe you found a
bug, **do not fix it** — note it separately and leave the code alone. Mixing a
fix into a 4,000-line rename makes both unreviewable.

## Order of work, and commit discipline

The sandbox for this project has been reset three times and destroyed a full
turn of work once. **Commit and push after each step.** A commit that exists only
locally is as fragile as not commit.

Work package by package so each step is independently verifiable:

1. `internal/kernel/event.go`, `effect.go`, `state.go` — comments and identifiers
2. `internal/kernel/config.go`
3. `internal/kernel/decide.go` — the biggest file (~700 lines), comments + diagnostic strings
4. `internal/kernel/why.go` — comments + output strings
5. `internal/kernel/decide_test.go` — test names, messages, helper renames
6. `internal/surface/surface.go` + `surface_test.go`
7. `internal/arch_test.go` — includes the doc path in its failure message
8. `cmd/arxi/main.go` — comments + usage text
9. Docs: README, ADRs (with `git mv`), design doc (with `git mv`), spec
10. Regenerate the golden, verify the diff

After every step:

```bash
gofmt -l .          # must print nothing
go vet ./...
go test -count=1 ./...
git add -A && git commit -m "i18n: translate <area> to English" && git push
```

## Acceptance criteria

- [ ] `go build ./cmd/arxi` succeeds
- [ ] `go vet ./...` clean, `gofmt -l .` prints nothing
- [ ] `go test -count=1 ./...` — all 45 tests pass in all three packages
- [ ] `arxi why testdata/scenarios/blocked-on-approval.json` prints an English
      wait tree with English remediation commands
- [ ] Zero Spanish remains. Verify:
      `grep -rniE "[áéíóúñ¿¡]" --include=*.go --include=*.md .` returns nothing,
      then read through for accent-free Spanish (`stage`, `budget`,
      `member`, `turn`, `block`, `nadie`, `this`, `for`, `from`, `because`)
- [ ] No wire-contract string changed. Confirm with
      `git diff origin/main -- internal/kernel/event.go` that event type
      constants and JSON tags are untouched
- [ ] The golden diff contains only translated strings — not structural change
- [ ] Every doc cross-reference resolves (not link to `10-execution.md` or to an
      old ADR filename survives)
