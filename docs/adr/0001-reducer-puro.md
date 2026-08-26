# ADR-0001: El reducer es puro y describe efectos en vez de ejecutarlos

- Estado: aceptada
- Afecta a: `internal/kernel/decide.go`, `internal/kernel/effect.go`, `internal/arch_test.go`

## Contexto

Un orquestador de agentes tiene que hacer cuatro cosas que parecen distintas:
correr un equipo de verdad, simularlo sin gastar plata, reproducir un run viejo
para entender qué pasó, y explicar por qué un run está trabado ahora mismo.

La forma obvia de escribirlo es un loop que decide y actúa en el mismo lugar:
mira el estado, llama al modelo, escribe el archivo, actualiza el estado. Es la
forma más corta de llegar a la primera demo.

El problema aparece después. `--sim` necesita el mismo loop sin las llamadas
reales, así que se agrega un flag y un `if` en cada punto de contacto con el
mundo. `replay` necesita el mismo loop sin *ningún* efecto, así que se agrega
otro flag y otro `if`. Cada punto de contacto nuevo hay que acordarse de
condicionarlo en tres lugares. El que se olvida de uno produce un `replay` que
manda mails.

## Decisión

El núcleo es una única función:

```go
Decide(State, Event, Config) -> (State', []Effect)
```

Pura: no consulta el reloj, no abre sockets, no lee ni escribe archivos, no usa
aleatoriedad. Todo lo que quiere que pase en el mundo lo **describe** como un
valor de tipo `Effect` y lo devuelve. Quien la llama decide qué hace con esa
lista.

Las cuatro features dejan de ser cuatro programas:

| feature | qué es |
|---|---|
| `iash run` | fold + ejecutor real |
| `iash run --sim` | fold + ejecutor falso |
| `iash run replay` | fold, sin ejecutor |
| `iash run why` | leer el `State` que salió del fold |

La lógica de decisión existe **una vez**. No hay ninguna manera de que `replay`
se desincronice de `run`, porque no hay dos implementaciones que puedan
divergir.

## Alternativa descartada

**Un reducer que puede llamar al mundo, con inyección de dependencias.** Pasar
un `Clock` y un `HTTPClient` como interfaces y mockearlos en test. Es la
solución habitual y es peor por dos razones concretas:

1. La pureza deja de ser verificable. Con inyección, "es puro" depende de que
   nadie llame `time.Now()` directo. Eso no lo chequea nada; se descubre cuando
   un replay da distinto.
2. El orden de los efectos queda escondido en el flujo de control. Cuando los
   efectos son datos devueltos, el orden es una lista que se puede inspeccionar,
   testear y comparar contra un golden. Cuando son llamadas, el orden es "lo
   que pasó" y hay que leer todo el reducer para saberlo.

## Consecuencias

- El reducer no puede tomar decisiones que dependan del tiempo real. Los
  timeouts entran como eventos (`timer.fired`), no como comparaciones contra el
  reloj. Esto es lo que hace posible el reloj virtual de `--sim`.
- El reducer no asigna `seq` a los eventos que emite (ver ADR-0002).
- Hay un ejecutor que sí es impuro y que hay que testear aparte. Es más código
  total, pero el código impuro queda concentrado en un lugar chico en vez de
  repartido por toda la lógica.
- Un efecto que nadie implementa en el ejecutor no rompe el reducer: se decide y
  se descarta. Eso permite declarar la superficie antes de implementarla.

## Cómo se verifica

`internal/arch_test.go` corre `go list -json` sobre el paquete `kernel` y falla
si aparece `time`, `net`, `net/http`, `os`, `os/exec`, `math/rand`,
`crypto/rand`, `database/sql`, `io` o `bufio` en su clausura de imports propios.
El mensaje de error nombra la consecuencia: *"time.Now() dentro del reducer
rompe replay y el reloj virtual de --sim"*.

Es una garantía sobre el grafo de imports, no sobre la disciplina del próximo
commit.
