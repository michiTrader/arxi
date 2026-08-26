# iash

Sistemas of agentes that is can depurar.

The tesis of the project cabe en a línea:

> **A sola función pure produce `run`, `--sim`, `replay` and `why` como the same
> maquinaria.**

Todo lo demás is consequence of that.

## The núcleo

```go
Decide(State, Event, Config) -> (State', []Effect)
```

Pura. No mira the clock, not toca the network, not writes nothing. Todo lo that quiere that
happen en the world lo **describe** como a `Effect` and lo returns; another lo executes.

Esa restricción not is estética. Es lo that makes that four features sean a:

| feature | what is |
|---|---|
| `iash run` | fold + executor real |
| `iash run --sim` | fold + executor fake |
| `iash run replay` | fold sobre a log old, without executor |
| `iash run why` | read the `State` that salió of the fold |

En a diseño where the reducer llama a the network, `replay` is a second programa that
reimplementa the logic of the first. Siempre is desactualizado and nadie is da
cuenta until that makes missing.

Y the pureza is **verificada, not prometida**: `internal/arch_test.go` runs
`go list` sobre the paquete and fails if the kernel importa `time`, `net`, `os` or
`math/rand`.

## Qué makes distinto

**Detecta the silence.** The modo of fails more expensive not is the that grita: is the run
that not fails, not termina and not advances. `iash` lo detecta and emite `run.quiescent`
with a diagnóstico that nombra the rule of avance that not is meets — incluso en the
case difficult, where all submitted and the rule igual is unsatisfiable
(ADR-0004).

**The diagnóstico is derived, not hard-codeado.** `run why` lee references
estructuradas (`blocked_ref`) and produce remedies ejecutables. No there is a `case`
for blueprint:

```
$ iash why runs/r1/state.json
run r1: running
└─ backend: waiting (approval) from seq 5
   └─ waits approval of the tool "bash" (inbox inbox-1)
└─ budget: 0.4200 of 5.0000 USD spent en the tree

posibles remedies:
  $ iash inbox approve inbox-1
  $ iash agent tool policy --agent backend --allow bash
```

**The defaults are decisions of security.** Si some member can write
files, the workspace happens a `worktree` only: two agentes en a same directory
is pisan and the resultado is basura difficult of atribuir. A timeout of stage
escala, not fails — fail for defecto entrena to the usuario a poner timeouts
absurdos. The tools mutantes not is autorizan solas.

**The spending is auditable.** The budget is of the tree (`TreeSpentUSD`), so that
a spawn anidado not can multiplicar the techo of the raíz. Y when N causes is
fusionan en a turn, the N quedan registradas en `SpawnTurn.Coalesced`: a
saving that not is can auditar is a saving that nadie va a creer.

**A declaración, three superficies.** Cada capability is declara a sola vez and
is proyecta for pure join of strings — without tabla of traducción that is
desincronice:

| declaración | CLI | tool | protocolo |
|---|---|---|---|
| `["run","start"]` | `run start` | `iash_run_start` | `run.start` |

Son **45 capabilities declared**, of the cuales **32 is expose como tools** a
the agentes. The diferencia not is a descuido: there is cosas that a human can make
from the terminal and a agente not should poder hacerse a yes same. `iash surface`
shows the 32; `iash schema` emite the manifest that consumes a agente.

## Estado

Lo that runs today:

```
iash schema        emitir the manifest of the surface (JSON)
iash surface       see the surface complete, readable
iash why <file> explicar for what a run not advances
iash version       version of the binario and of the surface
```

The rest of the surface is **declared and verificada for tests, pero without
executor**. The CLI is honesta to the respecto: for a command declared and not
implemented te dice that, with its name of tool and its tipo of protocolo, en vez of
mentir with "unknown command".

Falta: the executor (`internal/exec`), load of blueprints, `serve` (protocolo
NDJSON), triggers and eval.

## Compilar and test

Requiere Go 1.22.

```bash
go build -or iash ./cmd/iash
go test ./...
```

The tests **not are opcionales**. Go not da `match` exhaustivo, so that a `switch`
sobre `Effect` to the that le missing a variant compila igual; the network that lo atrapa is
`go test` (ADR-0007). Por the same reason, each test protects a decision and its
mensaje of fails nombra the consequence and the remedy — a `t.Fatal("mismatch")`
not meets the contract of the project.

Para regenerar the golden:

```bash
UPDATE_GOLDEN=1 go test ./internal/kernel
```

## Dónde read

| ruta | what there is |
|---|---|
| [`docs/design/10-execution.md`](docs/design/10-execution.md) | the model of execution complete |
| [`docs/adr/`](docs/adr/) | for what each decision, and what is breaks if is revierte |
| [`spec/events.md`](spec/events.md) | the catálogo of events and the contract of `blocked_ref` |

The ADR are the mejor punto of input: each one dice what is decidió, what
alternative is discarded and **which is the test that makes meet the decision**.

## Organización of the code

```
internal/kernel/    the reducer pure: Decide, State, Event, Effect, Explain
internal/surface/   the surface declared a vez and their three proyecciones
internal/arch_test  the límites of arquitectura, verificados with go list
cmd/iash/           the CLI
spec/               contracts of events
```

The kernel not importa nothing of the rest of the project. Es the single capa that has that
continue siendo pure, and there is a test that lo makes meet.
