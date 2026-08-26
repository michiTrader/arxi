# ADR-0001: The reducer is pure and describe effects en vez of ejecutarlos

- Estado: aceptada
- Afecta a: `internal/kernel/decides.go`, `internal/kernel/effect.go`, `internal/arch_test.go`

## Contexto

A orquestador of agentes has that make four cosas that parecen distintas:
correr a equipo of truth, simularlo without gastar plata, replay a run old
for entender what happened, and explicar for what a run is stalled now same.

The way obvia of escribirlo is a loop that decides and actúa en the same lugar:
mira the state, llama to the model, writes the file, actualiza the state. Es the
way more corta of llegar a the first demo.

The problema aparece after. `--sim` needs the same loop without the calls
real, so that is adds a flag and a `if` en each punto of contacto with the
world. `replay` needs the same loop without *not* effect, so that is adds
another flag and another `if`. Cada punto of contacto new there is that acordarse of
condicionarlo en three lugares. The that is olvida of one produce a `replay` that
sends mails.

## Decisión

The núcleo is a single función:

```go
Decide(State, Event, Config) -> (State', []Effect)
```

Pura: not consulta the clock, not opens sockets, not lee ni writes files, not uses
aleatoriedad. Todo lo that quiere that happen en the world lo **describe** como a
valor of tipo `Effect` and lo returns. Quien the llama decides what makes with esa
list.

The four features dejan of ser four programas:

| feature | what is |
|---|---|
| `iash run` | fold + executor real |
| `iash run --sim` | fold + executor fake |
| `iash run replay` | fold, without executor |
| `iash run why` | read the `State` that salió of the fold |

The logic of decision exists **a vez**. No there is not way of that `replay`
is desincronice of `run`, because not there is two implementaciones that puedan
divergir.

## Alternativa discarded

**A reducer that can call to the world, with inyección of dependencias.** Pasar
a `Clock` and a `HTTPClient` como interfaces and mockearlos en test. Es the
solución habitual and is peor for two reasons concretas:

1. The pureza leaves of ser verificable. Con inyección, "is pure" depends of that
   nadie llame `time.Now()` direct. Eso not lo chequea nothing; is descubre when
   a replay da distinto.
2. The order of the effects remains escondido en the flujo of control. Cuando the
   effects are datos devueltos, the order is a list that is can inspeccionar,
   test and comparar against a golden. Cuando are calls, the order is "lo
   that happened" and there is that read everything the reducer for saberlo.

## Consecuencias

- The reducer not can tomar decisions that dependan of the time real. The
  timeouts entran como events (`timer.fired`), not como comparaciones against the
  clock. Esto is lo that makes posible the clock virtual of `--sim`.
- The reducer not assigns `seq` a the events that emite (see ADR-0002).
- Hay a executor that yes is impuro and that there is that test aparte. Es more code
  total, pero the code impuro remains concentrado en a lugar chico en vez of
  repartido for toda the logic.
- A effect that nadie implementa en the executor not breaks the reducer: is decides and
  is descarta. Eso permite declarar the surface before of implementarla.

## Cómo is verifies

`internal/arch_test.go` runs `go list -json` sobre the paquete `kernel` and fails
if aparece `time`, `net`, `net/http`, `os`, `os/exec`, `math/rand`,
`crypto/rand`, `database/sql`, `io` or `bufio` en its clausura of imports own.
The mensaje of error nombra the consequence: *"time.Now() inside of the reducer
breaks replay and the clock virtual of --sim"*.

Es a garantía sobre the grafo of imports, not sobre the disciplina of the próximo
commit.
