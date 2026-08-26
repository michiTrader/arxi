# ADR-0005: Un solo mecanismo de inyección da queue, follow-up y coalescing

- Estado: aceptada
- Afecta a: `internal/kernel/decide.go` (`applyInjection`, `applyTurnDone`), `internal/kernel/state.go` (`PendingCauses`)

## Contexto

Tres requerimientos que llegaron por separado y parecen tres features:

1. **`on_busy: queue`** — si le mandás algo a un agente que está pensando, el
   texto no se pierde ni lo interrumpe: espera.
2. **Follow-up** — mandarle una segunda instrucción a un agente que ya está
   trabajando en la primera.
3. **Coalescing** — si llegan cinco causas de despertar mientras está ocupado, no
   abrir cinco turnos.

La forma natural de implementarlas es una por una: una cola para (1), un campo de
continuación para (2), un contador con ventana para (3). Tres estructuras que
interactúan, y las interacciones son donde viven los bugs: ¿el follow-up entra a
la cola o la saltea? ¿el coalescing cuenta los que están en cola?

## Decisión

Un solo campo de estado: `PendingCauses`.

Tres tipos de evento (`run.prompt`, `agent.steered`, `agent.notified`) entran por
la **misma** función, `applyInjection`, porque son el mismo hecho con distinta
procedencia: *llegó texto para alguien*. Si el destinatario está ocupado, el
texto se acumula en `PendingCauses`. Cuando el turno actual termina,
`applyTurnDone` drena la lista en **un** `SpawnTurn`.

Las tres features caen solas:

- Acumular en vez de perder **es** `on_busy: queue`.
- `queue` sobre alguien que ya trabaja **es** follow-up.
- Drenar N causas en un `SpawnTurn` **es** coalescing.

No hay interacción entre features que pueda tener bugs, porque no hay tres
features. Hay un mecanismo.

### El coalescing se audita

`SpawnTurn.Coalesced` lleva las causas que se fusionaron.

Esto no es telemetría opcional. N causas fusionadas en 1 turno es un
**multiplicador directo de facturación**: es la diferencia entre pagar cinco
turnos y pagar uno. Si el número no queda en el log, nadie puede verificar que el
coalescing hizo lo que dice, ni explicar una factura, ni notar que dejó de
funcionar después de un refactor. Un ahorro que no se puede auditar es un ahorro
que nadie va a creer.

### El broadcast no despierta a los inactivos

Un bug que encontró un test. Un `steer` con destinatario `*` abría un turno para
un miembro `advisory` que nadie había activado todavía:

```go
if target == "*" && m.State == MemberInactive { continue }
```

Un broadcast le habla a quien está participando, no a quien todavía no fue
invocado. Sin este filtro, cada corrección al equipo paga un turno por cada
opinador que nadie llamó — y los advisory existen justamente para no costar
plata hasta que se los necesita.

## Alternativa descartada

**Interrumpir el turno en curso y reiniciarlo con el contexto nuevo.** Es lo que
parece más receptivo. Se descarta porque tira a la basura el trabajo ya pagado:
el turno interrumpido ya consumió tokens. Con inyecciones frecuentes, el agente
nunca termina un turno y el gasto crece sin producir nada. Esperar el fin del
turno es más lento y estrictamente más barato.

## Consecuencias

- Una inyección a alguien ocupado tarda hasta el fin del turno en tener efecto.
  Es el costo aceptado a cambio de no quemar trabajo pagado.
- `PendingCauses` es parte del estado, así que se reconstruye por fold y
  `run why` puede reportar "hay 3 causas esperando que termine el turno".
- Los tres tipos de evento comparten camino, así que un bug en la lógica de
  encolado se manifiesta en los tres a la vez — más visible, y con un solo lugar
  para arreglarlo.

## Cómo se verifica

`decide_test.go` cubre: que una inyección sobre alguien ocupado no genere
`SpawnTurn` en el momento, que al terminar el turno se genere exactamente uno con
todas las causas en `Coalesced`, y que un broadcast no toque a los miembros
`MemberInactive`. Ese último test es el que encontró el bug.
