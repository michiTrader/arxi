# ADR-0007: Go en vez de Rust, con tests que cubren lo que el compilador no da

- Estado: aceptada
- Afecta a: todo el proyecto; en particular `internal/kernel/effect.go` y `internal/arch_test.go`

## Contexto

El diseño de iash es un reducer puro sobre un enum de eventos que devuelve un
enum de efectos (ADR-0001). Descrito así, es un diseño que pide Rust a gritos:

- `enum Effect` con `match` exhaustivo verificado por el compilador
- inmutabilidad por defecto, sin `Clone()` a mano y sin aliasing accidental
- `Result<T, E>` en vez de convenciones sobre valores cero

Elegir Go es aceptar perder esas tres cosas. Hay que ser honesto sobre qué se
pierde y qué se pone en su lugar, o la decisión se convierte en una preferencia
disfrazada de argumento.

## Decisión

Go. Y por cada garantía que se pierde, un mecanismo concreto que la reemplaza.

### Lo que se pierde: `match` exhaustivo

En Rust, agregar una variante a `enum Effect` **no compila** hasta que se
actualiza cada `match`. En Go, un `switch` sobre una interfaz se cae silenciosamente
al `default` — que es exactamente el bug que no se nota: un efecto nuevo que el
ejecutor descarta sin decir nada.

**El reemplazo** son tres cosas juntas:

1. **Interfaz sellada.** `Effect` tiene un método no exportado, `isEffect()`.
   Ningún paquete fuera de `kernel` puede implementarla. El conjunto de variantes
   es cerrado, igual que un enum.
2. **Registro explícito.** `allEffectVariants` lista las 7 variantes.
   `EffectVariants()` devuelve una copia — no el slice directo — para que un test
   no pueda corromper el registro de otro.
3. **Test de exhaustividad.** `TestEffectExhaustivo` verifica el conteo contra
   una constante, detecta duplicados, y exige que cada variante declare una clase
   válida. El mensaje de falla dice qué hacer:

   > *"Si agregaste una variante de Effect, agregala a allEffectVariants y revisá
   > TODOS los switch sobre Effect (grep 'case SpawnTurn')."*

Es más débil que Rust: falla en `go test`, no en `go build`. Y es más fuerte que
nada, que es lo que tendría un `switch` sin registro.

### Lo que se pierde: inmutabilidad

`State` tiene mapas y slices, y en Go eso significa aliasing por defecto: un
`State` "copiado" comparte sus mapas con el original, así que el reducer podría
mutar la entrada sin darse cuenta y romper el fold.

**El reemplazo** es `Clone()` con copia profunda de `Members`, `BlockedOn`,
`PendingCauses`, `Locks` e `Inbox`, más un test que hace dos folds del mismo log
y compara: si el reducer mutó su entrada, el segundo fold da distinto y el test
dice *"dos folds del mismo log dieron estados distintos: replay no sirve para
nada"*. También verifica que `Fold` no haya modificado el estado de entrada.

## Lo que Go da y Rust no

Y acá está la parte que justifica la elección en vez de solo disculparla.

**El grafo de imports es inspeccionable desde un test.** `go list -json` devuelve
los imports y las dependencias de cualquier paquete, así que `internal/arch_test.go`
verifica que el kernel no importe `time`, `net`, `net/http`, `os`, `os/exec`,
`math/rand`, `crypto/rand`, `database/sql`, `io` ni `bufio`. Si alguien agrega
`time.Now()` al reducer, el test falla y explica que eso rompe el replay y el
reloj virtual de `--sim`.

Rust no da esto por defecto. La pureza en Rust es una convención sobre el estilo
del código, y la pureza acá es la propiedad de la que depende todo el diseño: si
el kernel es puro, `run`, `--sim`, `replay` y `why` son la misma maquinaria; si
deja de serlo, son cuatro programas y ninguno se entera.

Un detalle de implementación relevante: `ownClosure` solo camina las dependencias
internas del proyecto. La primera versión usaba la clausura completa de `-deps` y
marcaba `os` como violación porque el kernel importaba `fmt`. Un test de
arquitectura que da falsos positivos se desactiva a la semana, así que la
precisión acá no es cosmética.

Además, en lo práctico: una CLI de Go es un binario estático sin runtime que
instalar, y los tiempos de compilación permiten el ciclo test-arreglo-test que
este diseño usa como método principal de verificación.

## Alternativa descartada

**Rust.** Mejores garantías en el tipo. Se descarta porque las garantías que Rust
agrega son reemplazables por tests con mensajes de falla explícitos, mientras que
la garantía que Go permite — la verificación del grafo de imports, o sea la
pureza, que es el supuesto central del diseño — habría quedado sin verificar. Es
un intercambio, y se hace en el sentido en que el proyecto lo necesita.

## Consecuencias

- Un `switch` sobre `Effect` que se olvida de una variante compila. La red que lo
  atrapa es `go test`, así que **el proyecto no puede mergear sin correr los
  tests**; no es una recomendación, es parte del diseño.
- Los mensajes de falla de los tests son documentación de decisiones y hay que
  escribirlos con ese cuidado. Un `t.Fatal("mismatch")` no cumple el contrato de
  este ADR.
- `Clone()` hay que mantenerlo cuando se agrega un campo de referencia a `State`.
  El test de doble fold es lo que lo detecta.

## Cómo se verifica

- `TestEffectExhaustivo` (conteo, duplicados, clase declarada).
- El test de doble fold e inmutabilidad de la entrada.
- `internal/arch_test.go` para pureza del kernel y capas.
