# ADR-0003: The effects is clasifican en control e independent

- Estado: aceptada
- Afecta a: `internal/kernel/effect.go` (`EffectClass`), `internal/kernel/decides.go` (`orderEffects`)
- Depende of: ADR-0001
- Origen: review of IA A

## Contexto

ADR-0001 dice that `Decide` returns `[]Effect`. IA A señaló the gap obvious that
that leaves abierto: **the executor recibe a list, ¿the runs en order or en
parallel?**

No is a question of rendimiento. The two answers simple rompen cosas
distintas:

- **Todo en parallel** breaks the fix. `Emit` and `SetTimer` cambian lo that the
  rest of the system va a see. Si a `Emit` of `stage.advanced` runs after of
  a `SpawnTurn`, the turn starts leyendo a stage that ya not is the current.
- **Todo en order** breaks the reason of ser of the tool. Tres turns of
  agente that not is conocen between yes is serializan, and then orquestar a
  equipo cuesta lo same that correrlos of a one a mano.

A tercer camino tentador is that the executor decida case for case with a
`switch` sobre the tipo concrete. Eso mueve the política to the executor and garantiza
that the próxima variant of `Effect` is ejecute with the política for defecto that
le toque, without that nadie lo haya pensado.

## Decisión

The clase is a propiedad **of the effect**, declared en the kernel:

```go
type Effect interface {
	isEffect()
	Class() EffectClass
}
```

Dos clases, and the política is a sola rule:

| clase | variants | how is executes |
|---|---|---|
| `ClassControl` | `Emit`, `SetTimer`, `CancelTimer`, `Snapshot` | secuencial, en the order exact of the list |
| `ClassIndependent` | `SpawnTurn`, `CallTool`, `AskHuman` | concurrente between yes |

`Decide` ordena the list before of devolverla (`orderEffects`), so that the
executor only needs know this: **correr the prefijo of control one for one,
after the rest en parallel.** No there is not `switch` sobre tipos concretos en
the executor, and a variant nueva has that declarar its clase for build.

### Por what `SliceStable` and not `Slice`

```go
sort.SliceStable(fx, func(i, j int) bool { return fx[i].Class() < fx[j].Class() })
```

The order relative **between** the `Emit` is semántico: `stage.advanced` has that
salir before that `stage.entered`, because the second describe the state that left
the first. A sort inestable the reordena according to the tamaño of the input and the
implementation of the pivote — or sea, is breaks of way intermitente and not
reproducible, that is the peor way of romperse posible en a system cuyo
argumento of venta is the replay fiel.

## Alternativa discarded

**A DAG of dependencias explícitas between effects.** Más expresivo: permitiría
paralelizar two `Emit` that really not is tocan. Se descarta because the cost not
is pays with lo that is gana: there is that declarar the dependencia en each sitio of
emisión, and a dependencia olvidada produce exactly the bug of carrera that
wanted evitar, now with more ceremonia. Dos clases are gruesas and are
correctas for construcción.

## Consecuencias

- A `Emit` never runs en parallel with another `Emit`, aunque a veces podría. Se
  acepta: the `Emit` are baratos, lo expensive are the `SpawnTurn` and esos yes van en
  parallel.
- Agregar a variant of `Effect` requires a decides its clase (is a método of the
  interfaz). No there is default silencioso.
- The executor not has política propia sobre order. Toda the decision vive en the
  kernel, where is pure and testeable.

## Cómo is verifies

- `TestEffectExhaustivo` recorre `EffectVariants()` and fails if a variant not
  is registrada — see ADR-0007.
- The tests of order en `decide_test.go` verifican that the effects of control
  vengan before that the independent and that the order relative of the `Emit` is
  preserve.
- The golden of `testdata/scenarios/` fixes the list complete of effects: a
  change of order accidental makes fail the comparación.
