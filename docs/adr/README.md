# Architecture decisions

Every file here records **one** decision: what was decided, which alternative
was discarded and **what breaks if somebody reverts it without reading this**.

These are not implementation docs. The implementation changes; these decisions
are the ones that cannot change without redesigning the system. If an ADR and
the code contradict each other, there is a bug in one of the two and somebody
has to decide which — not ignore the contradiction.

| # | decision | status |
|---|---|---|
| [0001](0001-pure-reducer.md) | The reducer is pure and describes effects instead of running them | accepted |
| [0002](0002-log-is-truth.md) | The log is the truth; snapshots are cache; the blueprint is frozen | accepted |
| [0003](0003-effect-classes.md) | Effects are classified into control and independent | accepted |
| [0004](0004-quiescence-as-event.md) | Quiescence is an event with a diagnosis, not a terminal state | accepted |
| [0005](0005-one-injection-mechanism.md) | A single injection mechanism gives queue, follow-up and coalescing | accepted |
| [0006](0006-cas-on-seq.md) | Concurrency is resolved with CAS on `seq`; `turn_source` is retired | accepted |
| [0007](0007-go-instead-of-rust.md) | Go instead of Rust, with tests covering what the compiler does not give | accepted |
| [0008](0008-terse-invocation.md) | A terse invocation is a declared alias, not a second code path | accepted |
| [0009](0009-provider-neutral-native-turns.md) | Native model/tool loops use one durable provider-neutral turn contract | accepted |
| [0010](0010-durable-jobs-attempts-and-scheduling.md) | Durable work uses occurrences, fenced attempts and explicit outcomes | accepted |
| [0011](0011-exact-single-use-authorization.md) | Authorization binds and consumes one exact action | accepted |
| [0012](0012-honest-platform-workspaces.md) | Workspace names describe provisioned, platform-verified guarantees | accepted |
| [0013](0013-confirmed-transcript-prepared-context.md) | Confirmed history is projected once and prepared context is immutable before model dispatch | accepted |
| [0014](0014-measured-context-compaction.md) | Context pressure is measured against explicit budgets and compaction is a verified lossy artifact beside an intact transcript | accepted |
| [0015](0015-one-host-lifecycle-projected-across-surfaces.md) | One capability has one implementation and every surface projects it | accepted |
| [0016](0016-protocol-event-streaming.md) | Protocol event streaming carries notifications beside responses | accepted |
| [0017](0017-shared-readonly-linux.md) | Shared on Linux is read-only because a mechanism refuses writes, not because none were granted | accepted |
| [0018](0018-repository-control-plane-is-not-workspace-content.md) | The repository control plane is not workspace content | accepted |
| [0019](0019-copy-is-writable-on-linux.md) | Linux advertises the copy snapshot as writable, and stops at files | accepted |
| [0020](0020-retrieved-memory-is-data-not-instruction.md) | Retrieved memory arrives as data, not as instruction | accepted |
| [0021](0021-memory-records-are-addressable-versions.md) | A memory receipt names the record version it presented | accepted |
| [0022](0022-memory-scope-is-a-principal-not-a-subject.md) | Memory scope names a principal, and `subject` is not one | accepted |
| [0023](0023-memory-authority-is-enumerated-not-inferred.md) | Memory authority is enumerated, and an unknown kind is not authority | accepted |
| [0024](0024-the-preparer-validates-the-receipts-it-emits.md) | The preparer validates the receipts it emits, and evidence names content | accepted |
| [0025](0025-every-assembler-uses-the-memory-channel.md) | Every assembler uses the memory channel, not just the durable one | accepted |
| [0026](0026-the-roadmap-cites-every-enabling-decision.md) | The roadmap cites every decision that claims to enable it | accepted |
| [0027](0027-memory-records-are-a-store-outside-every-run.md) | Governed memory is a store outside every run, and retrieval is authorized before it is ranked | accepted |
| [0028](0028-supersession-is-claimed-exclusively-and-a-fork-is-contained.md) | Superseding a version claims it exclusively, and a fork that still arrives is contained to its own record | accepted |

## Format

Short on purpose. Context, decision, consequences, and a **"how it is verified"**
section pointing at the test that enforces the decision. An ADR with no
associated test is an intention, and intentions do not survive a rushed
refactor.
