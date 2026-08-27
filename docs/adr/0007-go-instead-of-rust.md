# ADR-0007: Go instead of Rust, with tests covering what the compiler does not give

- Status: accepted
- Affects: the whole project; in particular `internal/kernel/effect.go` and `internal/arch_test.go`

## Context

The design of iash is a pure reducer over an enum of events returning an enum of
effects (ADR-0001). Described that way, it is a design that screams for Rust:

- `enum Effect` with exhaustive `match` verified by the compiler
- immutability by default, with no hand-written `Clone()` and no accidental
  aliasing
- `Result<T, E>` instead of conventions about zero values

Choosing Go means accepting the loss of those three things. We have to be honest
about what is lost and what is put in its place, or the decision turns into a
preference disguised as an argument.

## Decision

Go. And for every guarantee that is lost, a concrete mechanism that replaces it.

### What is lost: exhaustive `match`

In Rust, adding a variant to `enum Effect` **does not compile** until every
`match` is updated. In Go, a `switch` over an interface falls silently through to
the `default` — which is exactly the bug you do not notice: a new effect the
executor discards without saying anything.

**The replacement** is three things together:

1. **Sealed interface.** `Effect` has an unexported method, `isEffect()`. No
   package outside `kernel` can implement it. The set of variants is closed, just
   like an enum.
2. **Explicit registry.** `allEffectVariants` lists the 7 variants.
   `EffectVariants()` returns a copy — not the slice itself — so one test cannot
   corrupt the registry for another.
3. **Exhaustiveness test.** `TestEffectExhaustive` checks the count against a
   constant, detects duplicates, and requires every variant to declare a valid
   class. The failure message says what to do:

   > *"If you added an Effect variant, add it to allEffectVariants and review ALL
   > the switches over Effect (grep 'case SpawnTurn')."*

It is weaker than Rust: it fails at `go test`, not at `go build`. And it is
stronger than nothing, which is what a `switch` with no registry would have.

### What is lost: immutability

`State` has maps and slices, and in Go that means aliasing by default: a "copied"
`State` shares its maps with the original, so the reducer could mutate its input
without noticing and break the fold.

**The replacement** is `Clone()` with a deep copy of `Members`, `BlockedOn`,
`PendingCauses`, `Locks` and `Inbox`, plus a test that folds the same log twice
and compares: if the reducer mutated its input, the second fold comes out
different and the test says *"two folds of the same log gave different states:
replay is worthless"*. It also verifies that `Fold` did not modify the input
state.

## What Go gives and Rust does not

And here is the part that justifies the choice rather than merely apologizing for
it.

**The import graph is inspectable from a test.** `go list -json` returns the
imports and dependencies of any package, so `internal/arch_test.go` verifies that
the kernel does not import `time`, `net`, `net/http`, `os`, `os/exec`,
`math/rand`, `crypto/rand`, `database/sql`, `io` or `bufio`. If somebody adds
`time.Now()` to the reducer, the test fails and explains that this breaks replay
and the virtual clock of `--sim`.

Rust does not give this by default. Purity in Rust is a convention about code
style, and purity here is the property the whole design depends on: if the kernel
is pure, `run`, `--sim`, `replay` and `why` are the same machinery; if it stops
being pure, they are four programs and none of them is complete.

One relevant implementation detail: `ownClosure` only walks the project-internal
dependencies. The first version used the full `-deps` closure and flagged `os` as
a violation because the kernel imported `fmt`. An architecture test that produces
false positives gets disabled within a week, so the precision here is not
cosmetic.

Also, practically: a Go CLI is a static binary with no runtime to install, and
compile times allow the test-fix-test cycle this design uses as its main
verification method.

## Discarded alternative

**Rust.** Better guarantees in the type system. Discarded because the guarantees
Rust adds are replaceable by tests with explicit failure messages, whereas the
guarantee Go enables — verifying the import graph, that is, purity, which is the
central assumption of the design — would have gone unverified. It is a trade, and
it is made in the direction the project needs.

## Consequences

- A `switch` over `Effect` that forgets a variant still compiles. The net that
  catches it is `go test`, so **the project cannot merge without running the
  tests**; that is not a recommendation, it is part of the design.
- The failure messages of the tests are documentation of decisions and have to be
  written with that care. A `t.Fatal("mismatch")` does not meet the contract of
  this ADR.
- `Clone()` has to be maintained whenever a reference field is added to `State`.
  The double-fold test is what detects it.

## How it is verified

- `TestEffectExhaustive` (count, duplicates, declared class).
- The double-fold and input-immutability test.
- `internal/arch_test.go` for kernel purity and layering.
