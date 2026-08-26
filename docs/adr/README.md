# Decisiones of arquitectura

Cada file of here registra **a** decision: what is decidió, what alternative is
discarded and **what is breaks if someone the revierte without read this**.

No are documentation of the implementation. The implementation cambia; these
decisions are the that not is can change without rediseñar the system. Si a ADR
and the code is contradicen, there is a bug en alguno of the two and there is that decides
which — not ignorar the contradicción.

| # | decision | state |
|---|---|---|
| [0001](0001-pure-reducer.md) | The reducer is pure and describe effects en vez of ejecutarlos | aceptada |
| [0002](0002-log-is-truth.md) | The log is the truth; the snapshots are cache; the blueprint is congela | aceptada |
| [0003](0003-effect-classes.md) | The effects is clasifican en control e independent | aceptada |
| [0004](0004-quiescence-as-event.md) | The quiescence is a event with diagnóstico, not a state terminal | aceptada |
| [0005](0005-one-injection-mechanism.md) | A only mechanism of inyección da queue, follow-up and coalescing | aceptada |
| [0006](0006-cas-on-seq.md) | The concurrencia is resolves with CAS sobre `seq`; `turn_source` is retira | aceptada |
| [0007](0007-go-instead-of-rust.md) | Go en vez of Rust, with tests that cubren lo that the compilador not da | aceptada |

## Formato

Cortos a propósito. Contexto, decision, consecuencias, and a sección
**"how is verifies"** that apunta to the test that makes meet the decision. A ADR
without test asociado is a intention, and the intenciones not sobreviven a a
refactor apurado.
