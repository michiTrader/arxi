# Catálogo de eventos

El log es la fuente de verdad. Este documento es el contrato de lo que puede
aparecer en él.

## Forma común

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

| campo | para qué |
|---|---|
| `seq` | Orden dentro del run. Lo asigna el escritor único, **nunca** el reducer. |
| `type` | Namespace jerárquico con punto. Los watchers matchean por prefijo (`stage.*`), así que el punto no es cosmético. |
| `source` | `human`, `agent`, `runtime`, `trigger`. Los eventos `runtime` (derivados) **no** vuelven a disparar watchers: si lo hicieran, un watcher sobre `stage.*` entraría en bucle con los `stage.advanced` que él mismo causó. |
| `correlation_id` | Agrupa la cadena causal completa desde la causa raíz. |
| `caused_by` | Padres directos. Junto con `correlation_id` permite que `event trace` reconstruya el árbol. |
| `depth` | Profundidad causal. Es el freno de la cascada de watchers (§`max_depth`). |

## Ciclo de vida del run

| tipo | payload | notas |
|---|---|---|
| `run.started` | `run_id`, `actor`, `budget_usd`, `blueprint_sha`, `parent_run_id?`, `spawn_depth?` | `blueprint_sha` congela la config: sin él, un replay usaría la config de hoy. |
| `run.prompt` | `text`, `to?` | Inyecta una causa nueva en un run vivo. |
| `run.paused` / `run.unpaused` | — | |
| `run.cancelled` | `reason?` | |
| `run.expired` | — | |
| `run.quiescent` | **`diagnosis`** (obligatorio), `stage` | Ver abajo. |
| `run.result` | `summary`, `result_from?` | |

### `run.quiescent`

No es un estado terminal, es un aviso. `diagnosis` es **obligatorio**: un evento
que solo dice "el run está quieto" no le sirve a nadie. Tiene que nombrar la
causa concreta — la regla de avance que no se cumple, o quién está esperando qué.

Si nadie observa el evento, el run falla arrastrando el diagnóstico en `result`.

## Etapas

| tipo | payload |
|---|---|
| `stage.entered` | `stage`, `index` |
| `stage.submitted` | — (el actor es quien entregó) |
| `stage.advanced` | `from`, `to`, `to_index` |
| `stage.timeout` | `stage` |

`stage.advanced` **siempre** precede al `stage.entered` correspondiente. El orden
entre ellos es semántico, y por eso `orderEffects` usa un sort estable.

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

### La regla de `blocked_ref`

**Todo `agent.blocked` debe traer `blocked_ref`: un objeto con los datos
necesarios para desbloquear.**

Esta es la regla que hace que `run why` no tenga casos cableados. En vez de una
lista de `if` por cada situación conocida, `why` camina la referencia y arma el
comando concreto:

| `blocked_on` | `blocked_ref` | remedio derivado |
|---|---|---|
| `approval` | `{inbox_id, tool, policy}` | `iash inbox approve <inbox_id>` |
| `lock` | `{key, holder}` | `iash state unlock <key>` |
| `peer` | `{peer}` | (informativo: espera en cadena) |
| `budget` | `{}` | `iash run unpause <run> --budget <mayor>` |
| `timer` | `{timer_id}` | (informativo) |
| `tool` | `{tool}` | (informativo) |
| `workspace` | `{path}` | `iash run show <run> --workspace` |

Cuando aparezca una razón nueva de bloqueo, trae su referencia y `why` la muestra
sin cambios de código. Si alguien emite un bloqueo sin referencia, `why` lo
delata explícitamente en vez de mostrar una línea vacía:

> bloqueado sin referencia estructurada: es una violación del schema, todo
> waiting:* debe traer blocked_ref

## Herramientas y modelo

| tipo | payload |
|---|---|
| `tool.call` | `tool`, `args?` |
| `tool.call_completed` | `tool`, `result?` |
| `tool.call_denied` | `tool`, `policy` |
| `llm.response` | `cost_usd`, `tokens_in?`, `tokens_out?`, `model?` |

`tool.call_denied` con `policy: "ask"` **no es un error**: es una pregunta. Crea
un item de inbox y deja `blocked_ref` para que el remedio sea automático.

## Recursos

| tipo | payload |
|---|---|
| `lock.acquired` / `lock.released` | `key` |
| `resource.conflict` | `path`, `agents?` |

`resource.conflict` no falla el run. Despierta a quien lo observe; si nadie
observa, queda registrado y la quiescencia lo detecta más tarde. Fallar acá haría
que un merge conflict trivial mate media hora de trabajo.

## Presupuesto

| tipo | payload |
|---|---|
| `budget.warning` | `tree_spent_usd`, `budget_usd`, `pct` |
| `budget.exceeded` | `tree_spent_usd`, `budget_usd` |

Los dos reportan el gasto del **árbol**, no del run: con spawn anidado, el gasto
del run solo es una fracción engañosa.

`budget.warning` se emite **una vez** (marcado en `State.BudgetWarned`). Un aviso
que se repite en cada llamada es un aviso que el usuario aprende a ignorar.

## Humano en el bucle

| tipo | payload |
|---|---|
| `inbox.created` | `inbox_id`, `kind`, `question`, `agent?`, `on_timeout` |
| `inbox.replied` | `inbox_id`, `text` |
| `inbox.timeout` | `inbox_id` |

`on_timeout` se decide **cuando se crea la pregunta**, no cuando expira: en el
momento del timeout ya no hay nadie mirando.

## Reloj

| tipo | payload |
|---|---|
| `timer.tick` | `timer_id` |

Los timers se arman con offsets relativos en milisegundos, no timestamps
absolutos. Eso es lo que permite que el reloj virtual de `--sim` ejecute el mismo
fold sin esperar media hora de verdad.

## Eventos de usuario

`custom.*` está reservado para eventos emitidos por agentes vía
`iash event emit`. Los agentes **solo** pueden emitir en ese namespace: si
pudieran emitir `stage.advanced`, podrían saltarse la regla de avance de su
propio blueprint.
