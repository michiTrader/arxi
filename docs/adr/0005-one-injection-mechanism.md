# ADR-0005: A only mechanism of inyección da queue, follow-up and coalescing

- Estado: aceptada
- Afecta a: `internal/kernel/decides.go` (`applyInjection`, `applyTurnDone`), `internal/kernel/state.go` (`PendingCauses`)

## Contexto

Tres requerimientos that arrived for separado and parecen three features:

1. **`on_busy: queue`** — if le send something a a agente that is thinking, the
   text not is loses ni lo interrumpe: waits.
2. **Follow-up** — mandarle a segunda instrucción a a agente that ya is
   working en the first.
3. **Coalescing** — if arrive five causes of wake while is busy, not
   open five turns.

The way natural of implementarlas is a for a: a cola for (1), a field of
continuación for (2), a contador with ventana for (3). Tres estructuras that
interactúan, and the interacciones are where viven the bugs: ¿the follow-up entra a
the cola or the saltea? ¿the coalescing cuenta the that are en cola?

## Decisión

A only field of state: `PendingCauses`.

Tres tipos of event (`run.prompt`, `agent.steered`, `agent.notified`) entran for
the **same** función, `applyInjection`, because are the same done with distinta
procedencia: *llegó text for someone*. Si the destinatario is busy, the
text is acumula en `PendingCauses`. Cuando the turn current termina,
`applyTurnDone` drena the list en **a** `SpawnTurn`.

The three features caen solas:

- Acumular en vez of lose **is** `on_busy: queue`.
- `queue` sobre someone that ya works **is** follow-up.
- Drenar N causes en a `SpawnTurn` **is** coalescing.

No there is interacción between features that pueda tener bugs, because not there is three
features. Hay a mechanism.

### The coalescing is audita

`SpawnTurn.Coalesced` carries the causes that is fusionaron.

Esto not is telemetría opcional. N causes fusionadas en 1 turn is a
**multiplicador direct of facturación**: is the diferencia between pay five
turns and pay one. Si the number not remains en the log, nadie can verificar that the
coalescing hizo lo that dice, ni explicar a factura, ni notar that left of
work after of a refactor. A saving that not is can auditar is a saving
that nadie va a creer.

### The broadcast not wakes a the inactive

A bug that encontró a test. A `steer` with destinatario `*` abría a turn for
a member `advisory` that nadie había activado still:

```go
if target == "*" && m.State == MemberInactive { continue }
```

A broadcast le habla a who is participando, not a who still not was
invocado. Sin this filtro, each fix to the equipo pays a turn for each
opinador that nadie called — and the advisory existen justamente for not costar
plata until that is the needs.

## Alternativa discarded

**Interrumpir the turn en curso and reiniciarlo with the contexto new.** Es lo that
parece more receptivo. Se descarta because tira a the basura the work ya pagado:
the turn interrumpido ya consumió tokens. Con inyecciones frecuentes, the agente
never termina a turn and the spending crece without producir nothing. Esperar the fin of the
turn is more lento and estrictamente more barato.

## Consecuencias

- A inyección a someone busy tarda until the fin of the turn en tener effect.
  Es the cost aceptado a change of not quemar work pagado.
- `PendingCauses` is parte of the state, so that is reconstruye for fold and
  `run why` can reportar "there is 3 causes waiting that termine the turn".
- The three tipos of event share camino, so that a bug en the logic of
  encolado is manifiesta en the three a the vez — more visible, and with a only lugar
  for arreglarlo.

## Cómo is verifies

`decide_test.go` cubre: that a inyección sobre someone busy not genere
`SpawnTurn` en the momento, that to the terminar the turn is genere exactly one with
all the causes en `Coalesced`, and that a broadcast not toque a the members
`MemberInactive`. Ese último test is the that encontró the bug.
