# ADR-0006: The concurrencia is resolves with CAS sobre `seq`; `turn_source` is retira

- Estado: aceptada
- Afecta a: `internal/kernel/config.go` (`Interaction`), `internal/surface/surface.go` (`--if-seq`)
- Depende of: ADR-0002
- Origen: review of IA B

## Contexto

The primer borrador of the blueprint had a field `turn_source`: a declaración of
who had permiso for open turns en a run (the coordinador, cualquier
member, only the human). The intention was prevenir carreras: if only a source
can open turns, not there is two escritores compitiendo.

IA B señaló that that not resolves the problema, and had reason.

`turn_source` responde **who can hablar**. The carrera real is another question:
**two escritores modifican the state that the another leyó**. Son independent. Con
`turn_source: coordinator`, two operadores humans that ambos hablan a través of the
coordinador siguen produciendo the carrera complete: the two leen `seq 40`, the two
mandan a fix, and the segunda is applies sobre a state that ya not is the that
its autor vio.

Peor: `turn_source` da the *sensación* of haber resuelto the concurrencia. Es a
restricción visible en the blueprint, is siente como a garantía, and not lo is.

## Decisión

`turn_source` is **retira**. `Interaction` remains with a only field, `SteerTarget`,
that is lo that really hacía missing: a who le arrives a mensaje when not is
especifica destinatario.

The concurrencia is resolves with **compare-and-swap sobre `seq`**:

```
iash run prompt <run> "..." --if-seq 40
```

Se applies solamente if the run is exactly en `seq 40`. Si en the medio entró
another event, the operación is rechaza and the cliente vuelve a read and decides what
make with the información nueva.

Se combina with `--on-busy` (`reject` | `queue` | `steer`), that is a decision
distinta and ortogonal: `if-seq` protects against write sobre a state old,
`on-busy` decides what make when the destinatario is busy (ADR-0005).

### Por what `seq` and not the turn

`seq` **identifica a version of the state**. Es a entero monótono that the writer
single of the log assigns (ADR-0002), and `State = fold(...until seq N)` is a función
of él. Eso is exactly lo that a CAS needs: a token of version.

A turn not is a version of the state. A turn abarca muchos events and for lo
tanto muchos states; two operaciones that citan the same turn can estar seeing
states completamente distintos. A CAS sobre the turn sería a CAS that a veces
happens when should fail, that is lo peor that can make a CAS.

## Alternativa discarded

**Lockear the run while is writes.** Correcto, and descartado for the modo of
uso: a run live can estar hours waiting a approval humana. A lock
sobre esa ventana convierte cualquier interacción concurrente en a waits
indefinida, and a lock that there is that poder break a mano vuelve a traer the carrera
for the puerta of atrás. The CAS is optimista, that is lo apropiado when the
conflicto is raro and the cost of reintentar is read of new.

## Consecuencias

- `--if-seq` is opcional. Sin él the operación is "last write wins", that is well
  for a human en a terminal and is badly for a script. The surface lo
  declara en `run prompt` and `run steer` for that a cliente programático pueda
  ser correcto.
- A cliente that reciba a rechazo for `if-seq` has that releer. Eso is a
  feature: le arrives the state new before of insistir.
- No there is not declaración en the blueprint sobre who can open turns. The
  control of who can make what is autorización, and va en the capa of
  autorización, not en the model of execution.
- Menos surface of config, and the that remains not promete nothing that not cumpla.

## Cómo is verifies

`surface_test.go` verifies that `run prompt` and `run steer` declaren `if-seq` and
`on-busy`. The comment en `config.go` sobre `Interaction` documenta the retiro
en the punto where someone estaría tentado of reintroducir the field.
