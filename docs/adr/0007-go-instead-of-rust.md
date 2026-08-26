# ADR-0007: Go en vez of Rust, with tests that cubren lo that the compilador not da

- Estado: aceptada
- Afecta a: everything the project; en particular `internal/kernel/effect.go` and `internal/arch_test.go`

## Contexto

The diseño of iash is a reducer pure sobre a enum of events that returns a
enum of effects (ADR-0001). Descrito so, is a diseño that pide Rust a gritos:

- `enum Effect` with `match` exhaustivo verificado for the compilador
- inmutabilidad for defecto, without `Clone()` a mano and without aliasing accidental
- `Result<T, E>` en vez of convenciones sobre valores zero

Elegir Go is aceptar lose esas three cosas. Hay that ser honesto sobre what is
loses and what is pone en its lugar, or the decision is convierte en a preferencia
disfrazada of argumento.

## Decisión

Go. Y for each garantía that is loses, a mechanism concrete that the reemplaza.

### Lo that is loses: `match` exhaustivo

En Rust, add a variant a `enum Effect` **not compila** until that is
actualiza each `match`. En Go, a `switch` sobre a interfaz is cae silenciosamente
to the `default` — that is exactly the bug that not is nota: a effect new that the
executor descarta without decir nothing.

**The reemplazo** are three cosas juntas:

1. **Interfaz sellada.** `Effect` has a método not exportado, `isEffect()`.
   Ningún paquete outside of `kernel` can implementarla. The conjunto of variants
   is cerrado, igual that a enum.
2. **Registro explícito.** `allEffectVariants` list the 7 variants.
   `EffectVariants()` returns a copy — not the slice direct — for that a test
   not pueda corromper the registry of another.
3. **Test of exhaustividad.** `TestEffectExhaustivo` verifies the conteo against
   a constante, detecta duplicados, and exige that each variant declare a clase
   válida. The mensaje of fails dice what make:

   > *"Si agregaste a variant of Effect, agregala a allEffectVariants and review
   > TODOS the switch sobre Effect (grep 'case SpawnTurn')."*

Es more débil that Rust: fails en `go test`, not en `go build`. Y is more fuerte that
nothing, that is lo that tendría a `switch` without registry.

### Lo that is loses: inmutabilidad

`State` has mapas and slices, and en Go that means aliasing for defecto: a
`State` "copiado" comparte their mapas with the original, so that the reducer podría
mutar the input without darse cuenta and break the fold.

**The reemplazo** is `Clone()` with copy profunda of `Members`, `BlockedOn`,
`PendingCauses`, `Locks` e `Inbox`, more a test that makes two folds of the same log
and compara: if the reducer mutó its input, the second fold da distinto and the test
dice *"two folds of the same log dieron states distintos: replay not works for
nothing"*. También verifies that `Fold` not haya modificado the state of input.

## Lo that Go da and Rust not

Y here is the parte that justifica the elección en vez of only disculparla.

**The grafo of imports is inspeccionable from a test.** `go list -json` returns
the imports and the dependencias of cualquier paquete, so that `internal/arch_test.go`
verifies that the kernel not importe `time`, `net`, `net/http`, `os`, `os/exec`,
`math/rand`, `crypto/rand`, `database/sql`, `io` ni `bufio`. Si someone adds
`time.Now()` to the reducer, the test fails and explica that that breaks the replay and the
clock virtual of `--sim`.

Rust not da this for defecto. The pureza en Rust is a convención sobre the estilo
of the code, and the pureza here is the propiedad of the that depends everything the diseño: if
the kernel is pure, `run`, `--sim`, `replay` and `why` are the same maquinaria; if
leaves of serlo, are four programas and none is entera.

A detalle of implementation relevante: `ownClosure` only walks the dependencias
internas of the project. The first version usaba the clausura complete of `-deps` and
marcaba `os` como violación because the kernel importaba `fmt`. A test of
arquitectura that da falsos positivos is desactiva a the week, so that the
precisión here not is cosmética.

Además, en lo práctico: a CLI of Go is a binario estático without runtime that
instalar, and the tiempos of compilación permiten the ciclo test-arreglo-test that
this diseño uses como método principal of verification.

## Alternativa discarded

**Rust.** Mejores garantías en the tipo. Se descarta because the garantías that Rust
adds are reemplazables for tests with mensajes of fails explícitos, while that
the garantía that Go permite — the verification of the grafo of imports, or sea the
pureza, that is the supuesto central of the diseño — habría quedado without verificar. Es
a intercambio, and is makes en the sentido en that the project lo needs.

## Consecuencias

- A `switch` sobre `Effect` that is olvida of a variant compila. The network that lo
  atrapa is `go test`, so that **the project not can mergear without correr the
  tests**; not is a recomendación, is parte of the diseño.
- The mensajes of fails of the tests are documentation of decisions and there is that
  escribirlos with ese cuidado. A `t.Fatal("mismatch")` not meets the contract of
  this ADR.
- `Clone()` there is that mantenerlo when is adds a field of reference a `State`.
  The test of doble fold is lo that lo detecta.

## Cómo is verifies

- `TestEffectExhaustivo` (conteo, duplicados, clase declared).
- The test of doble fold e inmutabilidad of the input.
- `internal/arch_test.go` for pureza of the kernel and capas.
