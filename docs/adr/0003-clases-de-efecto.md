# ADR-0003: Los efectos se clasifican en control e independientes

- Estado: aceptada
- Afecta a: `internal/kernel/effect.go` (`EffectClass`), `internal/kernel/decide.go` (`orderEffects`)
- Depende de: ADR-0001
- Origen: revisión de IA A

## Contexto

ADR-0001 dice que `Decide` devuelve `[]Effect`. IA A señaló el hueco obvio que
eso deja abierto: **el ejecutor recibe una lista, ¿la corre en orden o en
paralelo?**

No es una pregunta de rendimiento. Las dos respuestas simples rompen cosas
distintas:

- **Todo en paralelo** rompe la corrección. `Emit` y `SetTimer` cambian lo que el
  resto del sistema va a ver. Si un `Emit` de `stage.advanced` corre después de
  un `SpawnTurn`, el turno arranca leyendo una etapa que ya no es la actual.
- **Todo en orden** rompe la razón de ser de la herramienta. Tres turnos de
  agente que no se conocen entre sí se serializan, y entonces orquestar un
  equipo cuesta lo mismo que correrlos de a uno a mano.

Un tercer camino tentador es que el ejecutor decida caso por caso con un
`switch` sobre el tipo concreto. Eso mueve la política al ejecutor y garantiza
que la próxima variante de `Effect` se ejecute con la política por defecto que
le toque, sin que nadie lo haya pensado.

## Decisión

La clase es una propiedad **del efecto**, declarada en el kernel:

```go
type Effect interface {
	isEffect()
	Class() EffectClass
}
```

Dos clases, y la política es una sola regla:

| clase | variantes | cómo se ejecuta |
|---|---|---|
| `ClassControl` | `Emit`, `SetTimer`, `CancelTimer`, `Snapshot` | secuencial, en el orden exacto de la lista |
| `ClassIndependent` | `SpawnTurn`, `CallTool`, `AskHuman` | concurrente entre sí |

`Decide` ordena la lista antes de devolverla (`orderEffects`), así que el
ejecutor solo necesita saber esto: **correr el prefijo de control uno por uno,
después el resto en paralelo.** No hay ningún `switch` sobre tipos concretos en
el ejecutor, y una variante nueva tiene que declarar su clase para compilar.

### Por qué `SliceStable` y no `Slice`

```go
sort.SliceStable(fx, func(i, j int) bool { return fx[i].Class() < fx[j].Class() })
```

El orden relativo **entre** los `Emit` es semántico: `stage.advanced` tiene que
salir antes que `stage.entered`, porque el segundo describe el estado que dejó
el primero. Un sort inestable los reordena según el tamaño de la entrada y la
implementación del pivote — o sea, se rompe de forma intermitente y no
reproducible, que es la peor forma de romperse posible en un sistema cuyo
argumento de venta es el replay fiel.

## Alternativa descartada

**Un DAG de dependencias explícitas entre efectos.** Más expresivo: permitiría
paralelizar dos `Emit` que realmente no se tocan. Se descarta porque el costo no
se paga con lo que se gana: hay que declarar la dependencia en cada sitio de
emisión, y una dependencia olvidada produce exactamente el bug de carrera que
queríamos evitar, ahora con más ceremonia. Dos clases son gruesas y son
correctas por construcción.

## Consecuencias

- Un `Emit` nunca corre en paralelo con otro `Emit`, aunque a veces podría. Se
  acepta: los `Emit` son baratos, lo caro son los `SpawnTurn` y esos sí van en
  paralelo.
- Agregar una variante de `Effect` obliga a decidir su clase (es un método de la
  interfaz). No hay default silencioso.
- El ejecutor no tiene política propia sobre orden. Toda la decisión vive en el
  kernel, donde es pura y testeable.

## Cómo se verifica

- `TestEffectExhaustivo` recorre `EffectVariants()` y falla si una variante no
  está registrada — ver ADR-0007.
- Los tests de orden en `decide_test.go` verifican que los efectos de control
  vengan antes que los independientes y que el orden relativo de los `Emit` se
  preserve.
- El golden de `testdata/scenarios/` fija la lista completa de efectos: un
  cambio de orden accidental hace fallar la comparación.
