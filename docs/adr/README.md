# Decisiones de arquitectura

Cada archivo de acá registra **una** decisión: qué se decidió, qué alternativa se
descartó y **qué se rompe si alguien la revierte sin leer esto**.

No son documentación de la implementación. La implementación cambia; estas
decisiones son las que no se pueden cambiar sin rediseñar el sistema. Si un ADR
y el código se contradicen, hay un bug en alguno de los dos y hay que decidir
cuál — no ignorar la contradicción.

| # | decisión | estado |
|---|---|---|
| [0001](0001-reducer-puro.md) | El reducer es puro y describe efectos en vez de ejecutarlos | aceptada |
| [0002](0002-log-es-la-verdad.md) | El log es la verdad; los snapshots son caché; el blueprint se congela | aceptada |
| [0003](0003-clases-de-efecto.md) | Los efectos se clasifican en control e independientes | aceptada |
| [0004](0004-quiescencia-como-evento.md) | La quiescencia es un evento con diagnóstico, no un estado terminal | aceptada |
| [0005](0005-un-mecanismo-de-inyeccion.md) | Un solo mecanismo de inyección da queue, follow-up y coalescing | aceptada |
| [0006](0006-cas-sobre-seq.md) | La concurrencia se resuelve con CAS sobre `seq`; `turn_source` se retira | aceptada |
| [0007](0007-go-en-vez-de-rust.md) | Go en vez de Rust, con tests que cubren lo que el compilador no da | aceptada |

## Formato

Cortos a propósito. Contexto, decisión, consecuencias, y una sección
**"cómo se verifica"** que apunta al test que hace cumplir la decisión. Un ADR
sin test asociado es una intención, y las intenciones no sobreviven a un
refactor apurado.
