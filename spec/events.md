# Catálogo of events

The log is the source of truth. Este document is the contract of lo that can
aparecer en él.

## Forma common

```json
{
  "seq": 42,
  "id": "e42",
  "ts": "2026-08-26T14:03:11Z",
  "type": "agent.blocked",
  "scope": "run:r1",
  "source": "runtime",
  "actor": "backend",
  "correlation_id": "e7",
  "caused_by": ["e41"],
  "depth": 2,
  "payload": {}
}
```

| field | for what |
|---|---|
| `seq` | Orden inside of the run. Lo assigns the writer single, **never** the reducer. |
| `type` | Namespace jerárquico with punto. The watchers matchean for prefijo (`stage.*`), so that the punto not is cosmético. |
| `source` | `human`, `agent`, `runtime`, `trigger`. The events `runtime` (derived) **not** vuelven a disparar watchers: if lo hicieran, a watcher sobre `stage.*` entraría en bucle with the `stage.advanced` that él same causó. |
| `correlation_id` | Agrupa the cadena causal complete from the cause raíz. |
| `caused_by` | Padres directos. Junto with `correlation_id` permite that `event trace` reconstruya the tree. |
| `depth` | Profundidad causal. Es the freno of the cascada of watchers (§`max_depth`). |

## Ciclo of vida of the run

| tipo | payload | notas |
|---|---|---|
| `run.started` | `run_id`, `actor`, `budget_usd`, `blueprint_sha`, `parent_run_id?`, `spawn_depth?` | `blueprint_sha` congela the config: without él, a replay usaría the config of today. |
| `run.prompt` | `text`, `to?` | Inyecta a cause nueva en a run live. |
| `run.paused` / `run.unpaused` | — | |
| `run.cancelled` | `reason?` | |
| `run.expired` | — | |
| `run.quiescent` | **`diagnosis`** (required), `stage` | Ver abajo. |
| `run.result` | `summary`, `result_from?` | |

### `run.quiescent`

No is a state terminal, is a aviso. `diagnosis` is **required**: a event
that only dice "the run is still" not le works a nadie. Tiene that nombrar the
cause concrete — the rule of avance that not is meets, or who is waiting what.

Si nadie observa the event, the run fails arrastrando the diagnóstico en `result`.

## Etapas

| tipo | payload |
|---|---|
| `stage.entered` | `stage`, `index` |
| `stage.submitted` | — (the actor is who entregó) |
| `stage.advanced` | `from`, `to`, `to_index` |
| `stage.timeout` | `stage` |

`stage.advanced` **always** precede to the `stage.entered` correspondiente. The order
between ellos is semántico, and for that `orderEffects` uses a sort estable.

## Agentes

| tipo | payload |
|---|---|
| `agent.activated` | — |
| `agent.steered` | `text`, `to?` |
| `agent.notified` | `text`, `to?` |
| `agent.turn_done` | — |
| `agent.blocked` | `blocked_on`, **`blocked_ref`** |
| `agent.unblocked` | — |
| `agent.failed` | `error` |

### The rule of `blocked_ref`

**Todo `agent.blocked` must traer `blocked_ref`: a objeto with the datos
necesarios for unblock.**

Esta is the rule that makes that `run why` not tenga cases cableados. En vez of a
list of `if` for each situación conocida, `why` walks the reference and builds the
command concrete:

| `blocked_on` | `blocked_ref` | remedy derived |
|---|---|---|
| `approval` | `{inbox_id, tool, policy}` | `iash inbox approve <inbox_id>` |
| `lock` | `{key, holder}` | `iash state unlock <key>` |
| `peer` | `{peer}` | (informativo: waits en cadena) |
| `budget` | `{}` | `iash run unpause <run> --budget <mayor>` |
| `timer` | `{timer_id}` | (informativo) |
| `tool` | `{tool}` | (informativo) |
| `workspace` | `{path}` | `iash run show <run> --workspace` |

Cuando aparezca a reason nueva of block, trae its reference and `why` the shows
without cambios of code. Si someone emite a block without reference, `why` lo
delata explícitamente en vez of show a línea vacía:

> blocked without reference structured: is a violación of the schema, everything
> waiting:* must traer blocked_ref

## Herramientas and model

| tipo | payload |
|---|---|
| `tool.call` | `tool`, `args?` |
| `tool.call_completed` | `tool`, `result?` |
| `tool.call_denied` | `tool`, `policy` |
| `llm.response` | `cost_usd`, `tokens_in?`, `tokens_out?`, `model?` |

`tool.call_denied` with `policy: "ask"` **not is a error**: is a question. Crea
a item of inbox and leaves `blocked_ref` for that the remedy sea automático.

## Recursos

| tipo | payload |
|---|---|
| `lock.acquired` / `lock.released` | `key` |
| `resource.conflict` | `path`, `agents?` |

`resource.conflict` not fails the run. Despierta a who lo observe; if nadie
observa, remains registrado and the quiescence lo detecta more tarde. Fallar here haría
that a merge conflict trivial mate media hour of work.

## Presupuesto

| tipo | payload |
|---|---|
| `budget.warning` | `tree_spent_usd`, `budget_usd`, `pct` |
| `budget.exceeded` | `tree_spent_usd`, `budget_usd` |

The two reportan the spending of the **tree**, not of the run: with spawn anidado, the spending
of the run only is a fracción engañosa.

`budget.warning` is emite **a vez** (marcado en `State.BudgetWarned`). A aviso
that is repeats en each call is a aviso that the usuario aprende a ignorar.

## Humano en the bucle

| tipo | payload |
|---|---|
| `inbox.created` | `inbox_id`, `kind`, `question`, `agent?`, `on_timeout` |
| `inbox.replied` | `inbox_id`, `text` |
| `inbox.timeout` | `inbox_id` |

`on_timeout` is decides **when is creates the question**, not when expires: en the
momento of the timeout ya not there is nadie mirando.

## Reloj

| tipo | payload |
|---|---|
| `timer.tick` | `timer_id` |

The timers is arman with offsets relativos en milisegundos, not timestamps
absolutos. Eso is lo that permite that the clock virtual of `--sim` ejecute the same
fold without wait media hour of truth.

## Eventos of usuario

`custom.*` is reservado for events emitidos for agentes vía
`iash event emit`. The agentes **only** can emitir en ese namespace: if
pudieran emitir `stage.advanced`, podrían saltarse the rule of avance of its
own blueprint.
