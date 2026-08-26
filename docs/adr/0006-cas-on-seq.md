# ADR-0006: La concurrencia se resuelve con CAS sobre `seq`; `turn_source` se retira

- Estado: aceptada
- Afecta a: `internal/kernel/config.go` (`Interaction`), `internal/surface/surface.go` (`--if-seq`)
- Depende de: ADR-0002
- Origen: revisión de IA B

## Contexto

El primer borrador del blueprint tenía un campo `turn_source`: una declaración de
quién tenía permiso para abrir turnos en un run (el coordinador, cualquier
miembro, solo el humano). La intención era prevenir carreras: si solo una fuente
puede abrir turnos, no hay dos escritores compitiendo.

IA B señaló que eso no resuelve el problema, y tenía razón.

`turn_source` responde **quién puede hablar**. La carrera real es otra pregunta:
**dos escritores modifican el estado que el otro leyó**. Son independientes. Con
`turn_source: coordinator`, dos operadores humanos que ambos hablan a través del
coordinador siguen produciendo la carrera completa: los dos leen `seq 40`, los dos
mandan una corrección, y la segunda se aplica sobre un estado que ya no es el que
su autor vio.

Peor: `turn_source` da la *sensación* de haber resuelto la concurrencia. Es una
restricción visible en el blueprint, se siente como una garantía, y no lo es.

## Decisión

`turn_source` se **retira**. `Interaction` queda con un solo campo, `SteerTarget`,
que es lo que realmente hacía falta: a quién le llega un mensaje cuando no se
especifica destinatario.

La concurrencia se resuelve con **compare-and-swap sobre `seq`**:

```
iash run prompt <run> "..." --if-seq 40
```

Se aplica solamente si el run está exactamente en `seq 40`. Si en el medio entró
otro evento, la operación se rechaza y el cliente vuelve a leer y decide qué
hacer con la información nueva.

Se combina con `--on-busy` (`reject` | `queue` | `steer`), que es una decisión
distinta y ortogonal: `if-seq` protege contra escribir sobre un estado viejo,
`on-busy` decide qué hacer cuando el destinatario está ocupado (ADR-0005).

### Por qué `seq` y no el turno

`seq` **identifica una versión del estado**. Es un entero monótono que el escritor
único del log asigna (ADR-0002), y `State = fold(...hasta seq N)` es una función
de él. Eso es exactamente lo que un CAS necesita: un token de versión.

Un turno no es una versión del estado. Un turno abarca muchos eventos y por lo
tanto muchos estados; dos operaciones que citan el mismo turno pueden estar viendo
estados completamente distintos. Un CAS sobre el turno sería un CAS que a veces
pasa cuando debería fallar, que es lo peor que puede hacer un CAS.

## Alternativa descartada

**Lockear el run mientras se escribe.** Correcto, y descartado por el modo de
uso: un run vivo puede estar horas esperando una aprobación humana. Un lock
sobre esa ventana convierte cualquier interacción concurrente en una espera
indefinida, y un lock que hay que poder romper a mano vuelve a traer la carrera
por la puerta de atrás. El CAS es optimista, que es lo apropiado cuando el
conflicto es raro y el costo de reintentar es leer de nuevo.

## Consecuencias

- `--if-seq` es opcional. Sin él la operación es "last write wins", que está bien
  para un humano en una terminal y está mal para un script. La superficie lo
  declara en `run prompt` y `run steer` para que un cliente programático pueda
  ser correcto.
- Un cliente que reciba un rechazo por `if-seq` tiene que releer. Eso es una
  feature: le llega el estado nuevo antes de insistir.
- No hay ninguna declaración en el blueprint sobre quién puede abrir turnos. El
  control de quién puede hacer qué es autorización, y va en la capa de
  autorización, no en el modelo de ejecución.
- Menos superficie de config, y la que queda no promete nada que no cumpla.

## Cómo se verifica

`surface_test.go` verifica que `run prompt` y `run steer` declaren `if-seq` y
`on-busy`. El comentario en `config.go` sobre `Interaction` documenta el retiro
en el punto donde alguien estaría tentado de reintroducir el campo.
