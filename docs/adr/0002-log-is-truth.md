# ADR-0002: The log is the truth; the snapshots are cache; the blueprint is congela

- Estado: aceptada
- Afecta a: `internal/kernel/state.go` (`BlueprintSHA`), `internal/kernel/decides.go`
- Depende of: ADR-0001

## Contexto

Con a reducer pure, the state is can reconstruir:

```
State = fold(Decide, State0, events)
```

Eso opens a question that there is that answer of a vez and not case for case:
when the snapshot guardado and the log not coinciden, ¿who gana?

The tentación is "gana the snapshot, is more rápido". Y is more rápido. También is
the way of tener a state that nadie can explicar: a `State` that dice
`status: failed` without not event of fails that lo justifique, because the
snapshot is wrote badly a vez makes three semanas.

## Decisión

**Gana the log, always.** The snapshots are exclusivamente a optimización of
reading. Se can delete all and the system not loses información — loses
velocidad of arranque.

Dos consecuencias that have dientes:

### The blueprint is congela to the start

The run stores a `blueprint_sha` en its state (`State.BlueprintSHA`) and the
reducer **never** lee the file of blueprint live. Lee the copy frozen en
`runs/<id>/blueprint.snapshot.yaml`.

Sin this, replay a run of the week pasada usaría the config of today. Si en
the medio someone cambió `advance_when` of `all` a `quorum:2`, the replay advances
where the run original is trabó, and the diagnóstico that te da is a ficción. A
replay that not reproduces not works for nothing, and lo peor is that not lo grita: te da
a answer plausible and equivocada.

Esta fix vino of the review of IA A.

### The reducer not assigns `seq`

The events that the reducer returns vía `Emit` carry `seq: 0`. The number of
secuencia lo pone the writer single of the log, en the momento of write.

The reducer not knows en what order global va a caer lo that emite: between that decides
and that is writes can haber entrado another event of another source. Pretender that
yes lo knows is inventar a carrera that after is manifiesta como a log with
`seq` duplicados, that is exactly lo that breaks the CAS of ADR-0006.

## Alternativa discarded

**Snapshots autoritativos with the log como auditoría.** Más rápido, and is lo that
makes the mayoría of the systems of workflow. Se descarta because convierte the
diagnóstico en arqueología: when the state and the log difieren not there is not
rule for decides what happened really, and `iash run why` — that is the reason of ser
of the tool — happens a responder sobre datos that not is can justificar.

## Consecuencias

- Arrancar a run largo from zero cuesta a fold complete. Aceptable: the fold
  is pure and not makes I/O for event.
- Cambiar the blueprint of a run live is imposible for diseño. Para that is
  `run fork --at-seq`: bifurcás with the config nueva and the run original remains
  intacto and reproducible.
- A log corrupto is a problema irrecuperable, so that the log has that ser
  append-only and escrito for a single writer.

## Cómo is verifies

- The golden of `testdata/scenarios/` resetea `seq = 0` before of comparar,
  justamente for that the test not dependa of the order of assignment — if the reducer
  empezara a asignar `seq`, the comparación fallaría.
- The tests of `decide_test.go` build the `Config` explícitamente and lo pasan
  a `Decide`; not there is not ruta for the that the reducer pueda alcanzar a
  file.
- `internal/arch_test.go` prohíbe `os` e `io` en the kernel, that is justo lo that
  haría missing for read the blueprint live.
