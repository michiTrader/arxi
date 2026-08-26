# ADR-0002: El log es la verdad; los snapshots son caché; el blueprint se congela

- Estado: aceptada
- Afecta a: `internal/kernel/state.go` (`BlueprintSHA`), `internal/kernel/decide.go`
- Depende de: ADR-0001

## Contexto

Con un reducer puro, el estado se puede reconstruir:

```
State = fold(Decide, State0, events)
```

Eso abre una pregunta que hay que contestar de una vez y no caso por caso:
cuando el snapshot guardado y el log no coinciden, ¿quién gana?

La tentación es "gana el snapshot, es más rápido". Y es más rápido. También es
la forma de tener un estado que nadie puede explicar: un `State` que dice
`status: failed` sin ningún evento de falla que lo justifique, porque el
snapshot se escribió mal una vez hace tres semanas.

## Decisión

**Gana el log, siempre.** Los snapshots son exclusivamente una optimización de
lectura. Se pueden borrar todos y el sistema no pierde información — pierde
velocidad de arranque.

Dos consecuencias que tienen dientes:

### El blueprint se congela al arrancar

El run guarda un `blueprint_sha` en su estado (`State.BlueprintSHA`) y el
reducer **nunca** lee el archivo de blueprint vivo. Lee la copia congelada en
`runs/<id>/blueprint.snapshot.yaml`.

Sin esto, reproducir un run de la semana pasada usaría la config de hoy. Si en
el medio alguien cambió `advance_when` de `all` a `quorum:2`, el replay avanza
donde el run original se trabó, y el diagnóstico que te da es una ficción. Un
replay que no reproduce no sirve para nada, y lo peor es que no lo grita: te da
una respuesta plausible y equivocada.

Esta corrección vino de la revisión de IA A.

### El reducer no asigna `seq`

Los eventos que el reducer devuelve vía `Emit` llevan `seq: 0`. El número de
secuencia lo pone el escritor único del log, en el momento de escribir.

El reducer no sabe en qué orden global va a caer lo que emite: entre que decide
y que se escribe puede haber entrado otro evento de otra fuente. Pretender que
sí lo sabe es inventar una carrera que después se manifiesta como un log con
`seq` duplicados, que es exactamente lo que rompe el CAS de ADR-0006.

## Alternativa descartada

**Snapshots autoritativos con el log como auditoría.** Más rápido, y es lo que
hace la mayoría de los sistemas de workflow. Se descarta porque convierte el
diagnóstico en arqueología: cuando el estado y el log difieren no hay ninguna
regla para decidir qué pasó realmente, y `iash run why` — que es la razón de ser
de la herramienta — pasa a responder sobre datos que no se pueden justificar.

## Consecuencias

- Arrancar un run largo desde cero cuesta un fold completo. Aceptable: el fold
  es puro y no hace I/O por evento.
- Cambiar el blueprint de un run vivo es imposible por diseño. Para eso está
  `run fork --at-seq`: bifurcás con la config nueva y el run original queda
  intacto y reproducible.
- Un log corrupto es un problema irrecuperable, así que el log tiene que ser
  append-only y escrito por un único escritor.

## Cómo se verifica

- El golden de `testdata/scenarios/` resetea `seq = 0` antes de comparar,
  justamente para que el test no dependa del orden de asignación — si el reducer
  empezara a asignar `seq`, la comparación fallaría.
- Los tests de `decide_test.go` construyen el `Config` explícitamente y lo pasan
  a `Decide`; no hay ninguna ruta por la que el reducer pueda alcanzar un
  archivo.
- `internal/arch_test.go` prohíbe `os` e `io` en el kernel, que es justo lo que
  haría falta para leer el blueprint vivo.
