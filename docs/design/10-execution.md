# 10. El modelo de ejecución

## 10.1 Una sola función decide

Todo iash gira alrededor de una función:

```go
Decide(State, Event, Config) -> (State', []Effect)
```

Pura. No mira el reloj, no toca la red, no escribe nada. Todo lo que quiere que
pase en el mundo lo **describe** como un `Effect` y devuelve; otro lo ejecuta.

Esa restricción no es estética. Es lo que hace que cuatro features sean la misma
feature en vez de cuatro programas que hay que mantener sincronizados:

| feature | qué es |
|---|---|
| `iash run` | fold + ejecutor real |
| `iash run --sim` | fold + ejecutor falso |
| `iash run replay` | fold sobre un log viejo, sin ejecutor |
| `iash run why` | leer el `State` que salió del fold |

En un diseño donde el reducer llama a la red, `replay` es un programa aparte que
reimplementa la lógica del principal. Siempre está desactualizado y nadie se da
cuenta hasta que hace falta.

La pureza está verificada, no prometida: `internal/arch_test.go` corre `go list`
sobre el paquete y falla si el kernel importa `time`, `net`, `os`, `math/rand` o
cualquier cosa de esa familia. El mensaje de error explica por qué está mal y qué
hacer en su lugar.

## 10.2 El log es la verdad

```
State = fold(Decide, State0, events)
```

Los snapshots son caché. Si un snapshot y el log no coinciden, **gana el log**.

Dos consecuencias con dientes:

**El blueprint se congela al arrancar.** El run guarda `blueprint_sha` y el
reducer nunca lee el archivo vivo, sino la copia en
`runs/<id>/blueprint.snapshot.yaml`. Sin esto, reproducir un run de la semana
pasada usaría la config de hoy y daría otro resultado — que es lo mismo que no
tener replay.

**El reducer no asigna `seq`.** Los eventos que devuelve vía `Emit` llevan
`seq: 0`. El número de secuencia lo pone el escritor único del log. El reducer no
sabe en qué orden global va a caer lo que emite, y pretender que sí lo sabe es
inventar una carrera.

## 10.3 Dos clases de efecto

Si `Decide` devuelve `[]Effect`, ¿el ejecutor los corre en orden o en paralelo?

Ninguna de las dos respuestas simples funciona. "Todos en paralelo" rompe:
`Emit` y `SetTimer` cambian lo que el resto del sistema va a ver. "Todos en
orden" serializa tres turnos de agente que no se conocen entre sí, y ahí se
pierde la razón de ser de la herramienta.

Así que cada efecto declara su clase:

- **`ClassControl`** — cambia el estado observable del run o el reloj.
  `Emit`, `SetTimer`, `CancelTimer`, `Snapshot`. Se ejecutan **en el orden exacto
  de la lista**, uno después de otro.
- **`ClassIndependent`** — solo se afecta a sí mismo. `SpawnTurn`, `CallTool`,
  `AskHuman`. Se pueden ejecutar **en paralelo** entre ellos.

`Decide` ordena la lista antes de devolverla (`orderEffects`), así que el
ejecutor solo necesita una regla: correr el prefijo de control secuencialmente,
después el resto concurrente.

El orden usa `sort.SliceStable`, no `sort.Slice`. El orden relativo entre los
`Emit` es semántico — `stage.advanced` tiene que ir antes de `stage.entered` — y
un sort inestable lo rompería de forma intermitente, que es la peor forma de
romperse.

## 10.4 Quiescencia: el modo de falla que nadie especifica

El modo de falla más caro de un sistema multi-agente no es el crash. Es el
silencio: el run no falla, no termina, no avanza. Deja de pasar cosas. El usuario
se entera a la mañana siguiente de que gastó cuarenta dólares en nada.

`Decide` chequea quiescencia **al final de cada paso**. Si nadie está ocupado,
nadie es despertable, no hay timer armado, no hay preguntas sin responder y
ningún efecto pendiente va a generar un evento, entonces emite `run.quiescent`.

Tres decisiones que importan:

**Es un evento, no un estado terminal.** Convertirlo en `StatusQuiescent` fue la
tentación obvia y habría sido el peor error del diseño. El evento despierta al
observador y el run se recupera. Solo si **nadie** lo observa el run falla — y
falla arrastrando el diagnóstico.

**Trae diagnóstico obligatorio.** "El run está quieto" no le sirve a nadie. El
payload dice qué regla no se cumple:

```
etapa solo avanza con quorum:3 y no se cumple; ya entregaron todos los que
podían: la regla es insatisfacible con este blueprint
```

Ese caso — la regla pide tres entregas y solo existen dos miembros que puedan
entregar — es el más difícil de depurar a ojo, porque todos "cumplieron" y el
blueprint se ve correcto.

**`submitted` no es `runnable`.** La sutileza que hizo fallar la primera
implementación. Un miembro que ya entregó parece disponible (no piensa, no
espera) pero no tiene nada que hacer. Contarlo como runnable hace que la
quiescencia **nunca** se detecte, y el bug es invisible: el sistema se ve
eternamente sano mientras está trabado para siempre.

## 10.5 Un mecanismo, tres features

`run.prompt`, `agent.steered` y `agent.notified` son el mismo mecanismo con
distinta procedencia: llega texto para alguien.

Si el destinatario está ocupado, el texto no se pierde ni abre un turno paralelo.
Se acumula en `PendingCauses` y se drena **todo junto** en el próximo turno.

Eso ES `on_busy: queue`. Y `queue` ES follow-up. Y el drenado ES coalescing. Tres
features del documento de requisitos, una sola máquina de veinte líneas.

El ahorro es directo: si cinco eventos despiertan al mismo agente mientras está
ocupado, se abre **un** turno con cinco causas en el contexto, no cinco turnos.
Literalmente 5x en la factura. El campo `Coalesced` del efecto registra cuántas
se fusionaron, para que el ahorro sea auditable y no una afirmación de marketing.

## 10.6 Los dos filtros que corren antes de gastar

Un watcher sobre `agent.*` que reacciona a sus propios eventos es un bucle
infinito con tarjeta de crédito. Dos filtros baratos corren **antes** de generar
un solo efecto caro:

1. **Auto-exclusión** (`include_self: false` por defecto). Un watcher no se
   despierta con sus propios eventos. Es un default, no una prohibición: hay
   patrones legítimos que necesitan verse a sí mismos, y `include_self: true`
   los habilita explícitamente.
2. **Límite de profundidad** (`max_depth: 12`). Cada evento derivado incrementa
   `depth`. Sin esto, un watcher que reacciona a lo que otro watcher causó no
   tiene fondo.

## 10.7 El presupuesto es del árbol, no del run

`State.TreeSpentUSD` acumula el gasto del subárbol completo. Un run hijo consume
del pool del padre.

Sin esto, N niveles de spawn multiplican el techo por N y el `--budget` del run
raíz es decorativo. Con esto, `--budget 10` significa diez dólares, sin importar
cuántos niveles de delegación aparezcan.

Y cuando se agota: **bloquear y preguntar, no matar**. El trabajo hecho hasta ahí
vale dinero real que ya se gastó. El humano decide si sube el techo o corta.

## 10.8 Los defaults son decisiones de seguridad

| default | por qué |
|---|---|
| `workspace: worktree` si alguien tiene `write`/`bash` | Dos agentes escribiendo el mismo directorio se pisan, y el lock del KV store no lo impide. El lock coordina *intención*; el aislamiento real lo da el filesystem. |
| `on_timeout: escalate` | Un timeout casi nunca significa "imposible", significa "algo se trabó, mirá". Fallar por defecto entrena al usuario a poner timeouts absurdamente largos, que es peor que no tenerlos. |
| `activation: coalesce` | La alternativa multiplica la factura a cambio de nada. |
| `include_self: false` | Ver §10.6. |
| política de tool sin declarar → `deny` | Un default permisivo convierte cada olvido en un agujero silencioso. |
| `--budget` sin default, obligatorio | Un techo invisible es una factura sorpresa. Que el usuario escriba el número es la única forma de que sepa que existe. |

## 10.9 Qué garantiza el compilador y qué garantizan los tests

Go no tiene enums exhaustivos. `Effect` es una interfaz sellada (método no
exportado `isEffect()`), así que nadie de afuera puede agregar variantes, pero el
compilador no obliga a que un `switch` las cubra todas.

El reemplazo es mecánico: `allEffectVariants` registra las siete, y
`TestEffectExhaustivo` falla si el número no coincide — con un mensaje que dice
exactamente qué hacer:

```
variantes registradas = 8, se esperaban 7.
Si agregaste una variante de Effect, agregala a allEffectVariants
y revisá TODOS los switch sobre Effect (grep 'case SpawnTurn').
```

Es peor que un `match` de Rust. La compensación está en la otra dirección:
`go list` permite verificar el grafo de imports, y `TestKernelEsPuro` convierte
"el kernel es puro" en algo que el CI comprueba en vez de algo que se recuerda.
Ver ADR-0007.
