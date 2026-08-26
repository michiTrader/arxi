# ADR-0004: The quiescence is a event with diagnóstico, not a state terminal

- Estado: aceptada
- Afecta a: `internal/kernel/decides.go` (`checkQuiescence`), `internal/kernel/state.go` (`RunStatus`)

## Contexto

The modos of fails of a orquestador of agentes that everything the world especifica are
the that gritan: a agente fails, is agota the budget, expires a timeout. Son
fáciles of detectar because something happens.

The modo of fails that nadie especifica is the that **not** grita: the run not fails, not
termina, not advances. Simplemente leaves of pasar cosas. Nadie is thinking, nadie
can empezar, not there is timer armado, not there is question pendiente. The system is
silent.

Es the more expensive of the two. A run that fails is descubre en the momento; a run
silent is descubre a the tomorrow siguiente, after of haber gastado the
budget en the turns that yes corrieron and of haber consumido the día of work
of the persona that lo esperaba.

## Decisión

The reducer detecta the silence and lo emite como a **event**, `run.quiescent`,
with a field `diagnosis` required.

**No is a state terminal.** `RunStatus` deliberadamente not has a valor
`quiescent`; `Terminal()` cubre `succeeded`, `failed`, `cancelled` and `expired`, and
nothing more.

The reason is that the quiescence is *recuperable for definición*: a `run prompt`
the breaks, a `inbox approve` the breaks, change a política of tool the
breaks. Si outside a state terminal, the usuario tendría that make `fork` for
continue a run that only necesitaba a empujón, and perdería the continuidad of the log
justo en the momento en that more the needs for entender what happened.

### The detección is conservadora

No is emite if there is **cualquier** reason for creer that something va a pasar:

- some effect pendiente that vaya a producir a event (`SpawnTurn`, `CallTool`,
  `SetTimer`, `AskHuman`, `Emit`)
- someone busy (`anyBusy`) or someone despertable (`anyRunnable`)
- a timer armado (`ActiveTimer`)
- a ítem of inbox without responder

The sesgo is intencional. A fake positivo — decirle "estás stalled" a a run that
iba a continue — destruye the confianza en the señal, and a señal en the that not is
confía is peor that not tenerla because is ignora justo when is verdadera.
`QuiescentEmitted` además garantiza that is emita a sola vez.

### The diagnóstico always nombra the rule of avance

Esto salió of a test that failed and had reason. The first version decía "nadie
is working and nadie can empezar", that is cierto e inútil.

The case difficult is this: the stage advances with `quorum:3` and only there is two members
that puedan submit. The two submitted. Nadie missing. The rule not is meets and not
is va a meet never. A diagnóstico that only dijera "nadie can empezar" leaves
to the usuario mirando a blueprint that a simple vista is well.

Así that the diagnóstico nombra the rule, and distingue the two cases:

```
stage review advances with quorum:3 and not is meets; missing the submit of: backend
stage review advances with quorum:3 and not is meets; ya submitted all the that
could: the rule is unsatisfiable with this blueprint
```

The segunda línea is a diagnóstico that apunta to the blueprint, not to the run. Es the
diferencia between "esperá a rato more" and "this never va a terminar, arreglá the
config".

## Alternativa discarded

**A timeout global of inactividad en the executor.** Más simple: if not happened nothing
en N minutes, avisar. Se descarta for two reasons. Primero, requiere the clock, and
the clock not exists inside of the reducer (ADR-0001), so that the detección quedaría
outside of the replay: not podrías replay the diagnóstico. Segundo, a timeout not
knows **for what** is silent; te da the alarma without the cause, and the cause is everything
the valor.

## Consecuencias

- The detección is sincrónica with the event that produjo the silence, so that
  `replay` reproduces the `run.quiescent` en the same punto of the log. The
  diagnóstico is reproducible, not a observación of a corrida.
- The run continues live and aceptando inyecciones after of the quiescence.
- A blueprint with a rule unsatisfiable is denuncia como tal en lugar of
  presentarse como a run lento.

## Cómo is verifies

`decide_test.go` has the case difficult construido a propósito: `quietCfg` builds
a `quorum:3` with only two members elegibles, all entregan, and the test exige
that the diagnóstico contenga the rule of avance and the palabra `unsatisfiable`.
Otros tests verifican that not is emita while haya a effect pendiente, a
timer armado or a inbox without responder.
