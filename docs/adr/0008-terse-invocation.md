# ADR-0008: A terse invocation is a declared alias, not a second code path

- Status: accepted
- Affects: `internal/surface/surface.go` (`CLIAlias`, `LookupCLI`), `cmd/arxi/main.go` (dispatch), `docs/design/20-use-cases.md`

## Context

The long form is the honest form, and it is also the tiring one:

```
arxi run start backend "fix the failing test" --budget 2
```

Four words and a number before the sentence you actually meant. A person using
this twenty times a day types `run start` twenty times and `--budget` twenty
times. The competing terminal agents are one word and a string, and the
comparison is not unfair — ergonomics is a real property of a tool, not a
cosmetic one. A correct agent nobody reaches for has the same value as a broken
one.

So the wanted form is roughly:

```
arxi -p "fix the failing test"
```

Two facts make this harder than it looks, and both are deliberate:

1. **`--budget` is mandatory and has no default.** The registry says why, in the
   declaration itself: *"An invisible default is a surprise bill; making the
   user type the number is the only way for them to know it exists."* A terse
   form that quietly picks a ceiling is the exact failure that comment exists to
   prevent.
2. **`-p` is already `--prompt`**, globally, and `run start` takes `actor` and
   `prompt` positionally. So the letter is not free to redefine — but it is
   already the right letter, which is luck worth using rather than fighting.

## Decision

**Yes, and it is sugar with no engine of its own.**

A terse invocation is a **declared alternative spelling of an existing command**,
resolved into that command's parameters before anything runs. One reducer, one
log shape, one budget check, one policy gate. `arxi -p "x"` and
`arxi run start <actor> "x" --budget N` converge on the same effect stream
before the first effect is emitted.

Three parts, in this order:

### 1. The alias is declared, not switched on

The dispatcher today is a hardcoded `switch args[0]`, and `arxi why` proves what
that costs: `why` is implemented, advertised on the usage screen, and **absent
from the registry**. `surface.Lookup("why")` is nil. The consequence was not
theoretical — `TestTheUsageScreenListsWhatIsActuallyImplemented` parses the
usage block by asking the registry which leading words it recognises, so the
`why` line matched nothing and was **silently skipped**. The guard checked ten
of the eleven commands it appeared to check, and reported success.

An undeclared spelling is invisible to every mechanism that keeps this surface
honest: the guard, `arxi surface`, `schema`, the protocol. So a terse form must
be a **field on the command it abbreviates** (`CLIAlias`), which makes it one
fact in one place that every consumer already reads.

This is why the alias mechanism comes first and is worth doing even with no
terse form at all: it closes a live hole in the guard. Adding the line-count
assertion that closes it immediately found two more undeclared-but-advertised
commands, `surface` and `version` — three skipped lines, not one.

### 2. The budget default is written down by the user, once

`--budget` keeps its rationale. What changes is *where the number can come
from*, not *whether the user chose it*:

- A **persisted default the user sets explicitly** — a number they typed once,
  stored, and can read back — is still a written decision. The surface comment
  forbids an *invisible* default, and a value the user wrote and can print is
  not invisible.
- A **built-in default** stays forbidden. It is precisely the surprise bill.

With no default ever set, the terse form **refuses and says how to fix it in one
line**. It does not guess, and it does not fall back to the long form silently:

```
arxi -p "fix the test": no default budget is set, and there is no built-in one.
  a default nobody chose is a surprise bill.
  set it once:  arxi config set budget 2
  or say it here: arxi -p "fix the test" --budget 2
```

The refusal is the feature. It happens once per machine, and after that the
terse form works forever.

### 3. The actor default is the same shape

`run start` needs an `actor`. The terse form resolves it from the same persisted
config, and refuses the same way when there is none. An agent picked by the tool
rather than the user is the same class of surprise as a budget picked by the
tool — smaller bill, larger blast radius.

## Discarded alternatives

**A second dispatch path that builds a run directly.** Rejected: it is a second
place where budget ceilings, policy gates and log writing can drift. The whole
value of this project is that the log explains the run; a fast path that writes
a slightly different log is a fast path to an unexplainable run. The terse form
must be unable to do anything the long form cannot.

**A built-in `--budget` default (say $1).** Rejected by ADR-level reasoning
already recorded in the registry. It also fails quietly: the user who did not
know the flag existed learns the number from an invoice.

**Inferring the budget from the prompt's apparent size.** Rejected: it is a
guess wearing the costume of a measurement, and it is wrong in the expensive
direction exactly when the task turns out harder than it read.

**Making `-p` a bare positional (`arxi "fix the test"`).** Rejected for now.
It collides with every future top-level word: `arxi why` would become ambiguous
the day somebody has a file named `why`. The flag keeps the grammar unambiguous
for the price of two characters.

## Consequences

- The terse form cannot exist meaningfully **before the tool runner**. Today
  `allow` still refuses, so `arxi -p "fix the test"` would be a fast path to a
  refusal — worse than no shortcut, because the shortcut is the thing that gets
  advertised. Sequencing is therefore: tool runner, then inbox, then this.
- The alias field is worth landing early and separately, because it fixes the
  `why` guard gap now.
- `arxi config set budget` is a new command and therefore a new promise. It gets
  declared with its own `Since:` before it is built, like everything else here.
- The differentiation does not come from the shortcut. Startup is ~6 ms measured,
  which is parity, not an advantage. What the other terminal agents cannot show
  you is `arxi run why`, a replayable log, a spend ceiling that binds, and a
  policy gate that refuses instead of asking forgiveness. The terse form removes
  a reason not to use those; it is not itself the reason to.

## How it is verified

`TestTheUsageScreenListsWhatIsActuallyImplemented` in `cmd/arxi` now resolves
aliases and asserts that **every** line of the block resolved, so an advertised
spelling that no longer runs fails the build instead of being skipped.
`TestEveryCLIAliasResolvesToADeclaredCommand` and
`TestNoCLIAliasCollidesWithATopLevelWord` in `internal/surface` keep an alias
from shadowing a real command or pointing at nothing — the two ways a second
spelling turns into a second meaning.
