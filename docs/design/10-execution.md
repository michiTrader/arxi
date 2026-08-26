# 10. The model of execution

## 10.1 A sola función decides

Todo iash gira alrededor of a función:

```go
Decide(State, Event, Config) -> (State', []Effect)
```

Pura. No mira the clock, not toca the network, not writes nothing. Todo lo that quiere that
happen en the world lo **describe** como a `Effect` and returns; another lo executes.

Esa restricción not is estética. Es lo that makes that four features sean the same
feature en vez of four programas that there is that keep sincronizados:

| feature | what is |
|---|---|
| `iash run` | fold + executor real |
| `iash run --sim` | fold + executor fake |
| `iash run replay` | fold sobre a log old, without executor |
| `iash run why` | read the `State` that salió of the fold |

En a diseño where the reducer llama a the network, `replay` is a programa aparte that
reimplementa the logic of the principal. Siempre is desactualizado and nadie is da
cuenta until that makes missing.

The pureza is verificada, not prometida: `internal/arch_test.go` runs `go list`
sobre the paquete and fails if the kernel importa `time`, `net`, `os`, `math/rand` or
cualquier cosa of esa familia. The mensaje of error explica for what is badly and what
make en its lugar.

## 10.2 The log is the truth

```
State = fold(Decide, State0, events)
```

The snapshots are cache. Si a snapshot and the log not coinciden, **gana the log**.

Dos consecuencias with dientes:

**The blueprint is congela to the start.** The run stores `blueprint_sha` and the
reducer never lee the file live, sino the copy en
`runs/<id>/blueprint.snapshot.yaml`. Sin this, replay a run of the week
pasada usaría the config of today and daría another resultado — that is lo same that not
tener replay.

**The reducer not assigns `seq`.** The events that returns vía `Emit` carry
`seq: 0`. The number of secuencia lo pone the writer single of the log. The reducer not
knows en what order global va a caer lo that emite, and pretender that yes lo knows is
inventar a carrera.

## 10.3 Dos clases of effect

Si `Decide` returns `[]Effect`, ¿the executor the runs en order or en parallel?

Ninguna of the two answers simple works. "Todos en parallel" breaks:
`Emit` and `SetTimer` cambian lo that the rest of the system va a see. "Todos en
order" serializa three turns of agente that not is conocen between yes, and ahí is
loses the reason of ser of the tool.

Así that each effect declara its clase:

- **`ClassControl`** — cambia the state observable of the run or the clock.
  `Emit`, `SetTimer`, `CancelTimer`, `Snapshot`. Se ejecutan **en the order exact
  of the list**, one after of another.
- **`ClassIndependent`** — only is afecta a yes same. `SpawnTurn`, `CallTool`,
  `AskHuman`. Se can ejecutar **en parallel** between ellos.

`Decide` ordena the list before of devolverla (`orderEffects`), so that the
executor only needs a rule: correr the prefijo of control secuencialmente,
after the rest concurrente.

The order uses `sort.SliceStable`, not `sort.Slice`. The order relative between the
`Emit` is semántico — `stage.advanced` has that ir before of `stage.entered` — and
a sort inestable lo rompería of way intermitente, that is the peor way of
romperse.

## 10.4 Quiescencia: the modo of fails that nadie especifica

The modo of fails more expensive of a system multi-agente not is the crash. Es the
silence: the run not fails, not termina, not advances. Deja of pasar cosas. The usuario
is entera a the tomorrow siguiente of that gastó cuarenta dollars en nothing.

`Decide` chequea quiescence **to the final of each paso**. Si nadie is busy,
nadie is despertable, not there is timer armado, not there is questions without responder and
not effect pendiente va a generar a event, then emite `run.quiescent`.

Tres decisions that importan:

**Es a event, not a state terminal.** Convertirlo en `StatusQuiescent` was the
tentación obvia and habría sido the peor error of the diseño. The event wakes to the
observador and the run is recupera. Solo if **nadie** lo observa the run fails — and
fails arrastrando the diagnóstico.

**Trae diagnóstico required.** "The run is still" not le works a nadie. The
payload dice what rule not is meets:

```
stage only advances with quorum:3 and not is meets; ya submitted all the that
could: the rule is unsatisfiable with this blueprint
```

Ese case — the rule pide three entregas and only existen two members that puedan
submit — is the more difficult of depurar a ojo, because all "cumplieron" and the
blueprint is ve correcto.

**`submitted` not is `runnable`.** The sutileza that hizo fail the first
implementation. A member that ya entregó parece disponible (not thinks, not
waits) pero not has nothing that make. Contarlo como runnable makes that the
quiescence **never** is detecte, and the bug is invisible: the system is ve
eternamente sano while is stalled for always.

## 10.5 A mechanism, three features

`run.prompt`, `agent.steered` and `agent.notified` are the same mechanism with
distinta procedencia: arrives text for someone.

Si the destinatario is busy, the text not is loses ni opens a turn parallel.
Se acumula en `PendingCauses` and is drena **everything junto** en the próximo turn.

Eso ES `on_busy: queue`. Y `queue` ES follow-up. Y the drenado ES coalescing. Tres
features of the document of requisitos, a sola máquina of veinte líneas.

The saving is direct: if five events despiertan to the same agente while is
busy, is opens **a** turn with five causes en the contexto, not five turns.
Literalmente 5x en the factura. The field `Coalesced` of the effect registra cuántas
is fusionaron, for that the saving sea auditable and not a afirmación of marketing.

## 10.6 The two filtros that corren before of gastar

A watcher sobre `agent.*` that reacts a their own events is a bucle
infinito with tarjeta of crédito. Dos filtros baratos corren **before** of generar
a only effect expensive:

1. **Auto-exclusión** (`include_self: false` for defecto). A watcher not is
   wakes with their own events. Es a default, not a prohibición: there is
   patrones legítimos that necesitan verse a yes mismos, and `include_self: true`
   the habilita explícitamente.
2. **Límite of depth** (`max_depth: 12`). Cada event derived incrementa
   `depth`. Sin this, a watcher that reacts a lo that another watcher causó not
   has depth.

## 10.7 The budget is of the tree, not of the run

`State.TreeSpentUSD` acumula the spending of the subárbol complete. A run hijo consumes
of the pool of the padre.

Sin this, N niveles of spawn multiplican the techo for N and the `--budget` of the run
raíz is decorativo. Con this, `--budget 10` means diez dollars, without importar
cuántos niveles of delegación aparezcan.

Y when is agota: **bloquear and ask, not kill**. The work done until ahí
vale money real that ya is gastó. The human decides if sube the techo or corta.

## 10.8 The defaults are decisions of security

| default | for what |
|---|---|
| `workspace: worktree` if someone has `write`/`bash` | Dos agentes escribiendo the same directory is pisan, and the lock of the KV store not lo impide. The lock coordina *intention*; the aislamiento real lo da the filesystem. |
| `on_timeout: escalate` | A timeout casi never means "imposible", means "something is trabó, mirá". Fallar for defecto entrena to the usuario a poner timeouts absurdamente largos, that is peor that not tenerlos. |
| `activation: coalesce` | The alternative multiplica the factura a change of nothing. |
| `include_self: false` | Ver §10.6. |
| política of tool without declarar → `deny` | A default permisivo convierte each olvido en a hole silencioso. |
| `--budget` without default, required | A techo invisible is a factura sorpresa. Que the usuario escriba the number is the single way of that sepa that exists. |

## 10.9 Qué garantiza the compilador and what garantizan the tests

Go not has enums exhaustivos. `Effect` is a interfaz sellada (método not
exportado `isEffect()`), so that nadie of outside can add variants, pero the
compilador not requires a that a `switch` the cubra all.

The reemplazo is mecánico: `allEffectVariants` registra the siete, and
`TestEffectExhaustivo` fails if the number not coincide — with a mensaje that dice
exactly what make:

```
variants registradas = 8, is esperaban 7.
Si agregaste a variant of Effect, agregala a allEffectVariants
and review TODOS the switch sobre Effect (grep 'case SpawnTurn').
```

Es peor that a `match` of Rust. The compensación is en the another dirección:
`go list` permite verificar the grafo of imports, and `TestKernelEsPuro` convierte
"the kernel is pure" en something that the CI comprueba en vez of something that is recuerda.
Ver ADR-0007.
