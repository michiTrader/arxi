# iash

Sistemas de agentes que se pueden depurar.

La tesis del proyecto cabe en una línea:

> **Una sola función pura produce `run`, `--sim`, `replay` y `why` como la misma
> maquinaria.**

Todo lo demás es consecuencia de eso.

## El núcleo

```go
Decide(State, Event, Config) -> (State', []Effect)
```

Pura. No mira el reloj, no toca la red, no escribe nada. Todo lo que quiere que
pase en el mundo lo **describe** como un `Effect` y lo devuelve; otro lo ejecuta.

Esa restricción no es estética. Es lo que hace que cuatro features sean una:

| feature | qué es |
|---|---|
| `iash run` | fold + ejecutor real |
| `iash run --sim` | fold + ejecutor falso |
| `iash run replay` | fold sobre un log viejo, sin ejecutor |
| `iash run why` | leer el `State` que salió del fold |

En un diseño donde el reducer llama a la red, `replay` es un segundo programa que
reimplementa la lógica del primero. Siempre está desactualizado y nadie se da
cuenta hasta que hace falta.

Y la pureza está **verificada, no prometida**: `internal/arch_test.go` corre
`go list` sobre el paquete y falla si el kernel importa `time`, `net`, `os` o
`math/rand`.

## Qué hace distinto

**Detecta el silencio.** El modo de falla más caro no es el que grita: es el run
que no falla, no termina y no avanza. `iash` lo detecta y emite `run.quiescent`
con un diagnóstico que nombra la regla de avance que no se cumple — incluso en el
caso difícil, donde todos entregaron y la regla igual es insatisfacible
(ADR-0004).

**El diagnóstico es derivado, no hard-codeado.** `run why` lee referencias
estructuradas (`blocked_ref`) y produce remedios ejecutables. No hay un `case`
por blueprint:

```
$ iash why runs/r1/state.json
run r1: running
└─ backend: waiting (approval) desde seq 5
   └─ espera aprobación de la tool "bash" (inbox inbox-1)
└─ presupuesto: 0.4200 de 5.0000 USD gastados en el árbol

posibles remedios:
  $ iash inbox approve inbox-1
  $ iash agent tool policy --agent backend --allow bash
```

**Los defaults son decisiones de seguridad.** Si algún miembro puede escribir
archivos, el workspace pasa a `worktree` solo: dos agentes en un mismo directorio
se pisan y el resultado es basura difícil de atribuir. Un timeout de etapa
escala, no falla — fallar por defecto entrena al usuario a poner timeouts
absurdos. Las herramientas mutantes no se autorizan solas.

**El gasto es auditable.** El presupuesto es del árbol (`TreeSpentUSD`), así que
un spawn anidado no puede multiplicar el techo de la raíz. Y cuando N causas se
fusionan en un turno, las N quedan registradas en `SpawnTurn.Coalesced`: un
ahorro que no se puede auditar es un ahorro que nadie va a creer.

**Una declaración, tres superficies.** Cada capacidad se declara una sola vez y
se proyecta por puro join de strings — sin tabla de traducción que se
desincronice:

| declaración | CLI | tool | protocolo |
|---|---|---|---|
| `["run","start"]` | `run start` | `iash_run_start` | `run.start` |

Son **45 capacidades declaradas**, de las cuales **32 se exponen como tools** a
los agentes. La diferencia no es un descuido: hay cosas que un humano puede hacer
desde la terminal y un agente no debería poder hacerse a sí mismo. `iash surface`
muestra las 32; `iash schema` emite el manifiesto que consume un agente.

## Estado

Lo que corre hoy:

```
iash schema        emitir el manifiesto de la superficie (JSON)
iash surface       ver la superficie completa, legible
iash why <archivo> explicar por qué un run no avanza
iash version       versión del binario y de la superficie
```

El resto de la superficie está **declarada y verificada por tests, pero sin
ejecutor**. La CLI es honesta al respecto: para un comando declarado y no
implementado te dice eso, con su nombre de tool y su tipo de protocolo, en vez de
mentir con "unknown command".

Falta: el ejecutor (`internal/exec`), carga de blueprints, `serve` (protocolo
NDJSON), triggers y eval.

## Compilar y testear

Requiere Go 1.22.

```bash
go build -o iash ./cmd/iash
go test ./...
```

Los tests **no son opcionales**. Go no da `match` exhaustivo, así que un `switch`
sobre `Effect` al que le falta una variante compila igual; la red que lo atrapa es
`go test` (ADR-0007). Por la misma razón, cada test protege una decisión y su
mensaje de falla nombra la consecuencia y el remedio — un `t.Fatal("mismatch")`
no cumple el contrato del proyecto.

Para regenerar los golden:

```bash
UPDATE_GOLDEN=1 go test ./internal/kernel
```

## Dónde leer

| ruta | qué hay |
|---|---|
| [`docs/design/10-ejecucion.md`](docs/design/10-ejecucion.md) | el modelo de ejecución completo |
| [`docs/adr/`](docs/adr/) | por qué cada decisión, y qué se rompe si se revierte |
| [`spec/events.md`](spec/events.md) | el catálogo de eventos y el contrato de `blocked_ref` |

Los ADR son el mejor punto de entrada: cada uno dice qué se decidió, qué
alternativa se descartó y **cuál es el test que hace cumplir la decisión**.

## Organización del código

```
internal/kernel/    el reducer puro: Decide, State, Event, Effect, Explain
internal/surface/   la superficie declarada una vez y sus tres proyecciones
internal/arch_test  los límites de arquitectura, verificados con go list
cmd/iash/           la CLI
spec/               contratos de eventos
```

El kernel no importa nada del resto del proyecto. Es la única capa que tiene que
seguir siendo pura, y hay un test que lo hace cumplir.
