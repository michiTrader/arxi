# ADR-0004: La quiescencia es un evento con diagnóstico, no un estado terminal

- Estado: aceptada
- Afecta a: `internal/kernel/decide.go` (`checkQuiescence`), `internal/kernel/state.go` (`RunStatus`)

## Contexto

Los modos de falla de un orquestador de agentes que todo el mundo especifica son
los que gritan: un agente falla, se agota el presupuesto, expira un timeout. Son
fáciles de detectar porque algo pasa.

El modo de falla que nadie especifica es el que **no** grita: el run no falla, no
termina, no avanza. Simplemente deja de pasar cosas. Nadie está pensando, nadie
puede empezar, no hay timer armado, no hay pregunta pendiente. El sistema está
callado.

Es el más caro de los dos. Un run que falla se descubre en el momento; un run
callado se descubre a la mañana siguiente, después de haber gastado el
presupuesto en los turnos que sí corrieron y de haber consumido el día de trabajo
de la persona que lo esperaba.

## Decisión

El reducer detecta el silencio y lo emite como un **evento**, `run.quiescent`,
con un campo `diagnosis` obligatorio.

**No es un estado terminal.** `RunStatus` deliberadamente no tiene un valor
`quiescent`; `Terminal()` cubre `succeeded`, `failed`, `cancelled` y `expired`, y
nada más.

La razón es que la quiescencia es *recuperable por definición*: un `run prompt`
la rompe, un `inbox approve` la rompe, cambiar una política de herramienta la
rompe. Si fuera un estado terminal, el usuario tendría que hacer `fork` para
seguir un run que solo necesitaba un empujón, y perdería la continuidad del log
justo en el momento en que más la necesita para entender qué pasó.

### La detección es conservadora

No se emite si hay **cualquier** razón para creer que algo va a pasar:

- algún efecto pendiente que vaya a producir un evento (`SpawnTurn`, `CallTool`,
  `SetTimer`, `AskHuman`, `Emit`)
- alguien ocupado (`anyBusy`) o alguien despertable (`anyRunnable`)
- un timer armado (`ActiveTimer`)
- un ítem de inbox sin responder

El sesgo es intencional. Un falso positivo — decirle "estás trabado" a un run que
iba a seguir — destruye la confianza en la señal, y una señal en la que no se
confía es peor que no tenerla porque se ignora justo cuando es verdadera.
`QuiescentEmitted` además garantiza que se emita una sola vez.

### El diagnóstico siempre nombra la regla de avance

Esto salió de un test que falló y tenía razón. La primera versión decía "nadie
está trabajando y nadie puede empezar", que es cierto e inútil.

El caso difícil es este: la etapa avanza con `quorum:3` y solo hay dos miembros
que puedan entregar. Los dos entregaron. Nadie falta. La regla no se cumple y no
se va a cumplir nunca. Un diagnóstico que solo dijera "nadie puede empezar" deja
al usuario mirando un blueprint que a simple vista está bien.

Así que el diagnóstico nombra la regla, y distingue los dos casos:

```
etapa review avanza con quorum:3 y no se cumple; falta el submit de: backend
etapa review avanza con quorum:3 y no se cumple; ya entregaron todos los que
podían: la regla es insatisfacible con este blueprint
```

La segunda línea es un diagnóstico que apunta al blueprint, no al run. Es la
diferencia entre "esperá un rato más" y "esto nunca va a terminar, arreglá la
config".

## Alternativa descartada

**Un timeout global de inactividad en el ejecutor.** Más simple: si no pasó nada
en N minutos, avisar. Se descarta por dos razones. Primero, requiere el reloj, y
el reloj no existe dentro del reducer (ADR-0001), así que la detección quedaría
fuera del replay: no podrías reproducir el diagnóstico. Segundo, un timeout no
sabe **por qué** está callado; te da la alarma sin la causa, y la causa es todo
el valor.

## Consecuencias

- La detección es sincrónica con el evento que produjo el silencio, así que
  `replay` reproduce el `run.quiescent` en el mismo punto del log. El
  diagnóstico es reproducible, no una observación de una corrida.
- El run sigue vivo y aceptando inyecciones después de la quiescencia.
- Un blueprint con una regla insatisfacible se denuncia como tal en lugar de
  presentarse como un run lento.

## Cómo se verifica

`decide_test.go` tiene el caso difícil construido a propósito: `quietCfg` arma
un `quorum:3` con solo dos miembros elegibles, todos entregan, y el test exige
que el diagnóstico contenga la regla de avance y la palabra `insatisfacible`.
Otros tests verifican que no se emita mientras haya un efecto pendiente, un
timer armado o un inbox sin responder.
