// Package agentstore keeps an agent on disk as a blueprint with one member, and
// a team as the same kind of file with several.
//
// # Why an agent is a blueprint and not a record of its own
//
// `arxi agent create reviewer --model claude-sonnet-4-6 --tools read,grep`
// (§20.1) could have written a small JSON record the way internal/toolstore
// does, and that was the first plan. It is the wrong one, because "an agent" and
// "a member of a blueprint" are the same noun: both carry a name, a role, a
// model, a tool grant and the advisory trait, and there is nothing an agent can
// declare that a member cannot. A second record type would need a second
// loader, a second validator and a second answer to every question the
// blueprint schema already answers, and the two would drift the first time a
// member field is added. That is not hypothetical: `model` was added to
// kernel.MemberConfig after the fact, and its comment there says `agent create
// --model` and `agent show` are promises already made.
//
// Rendering a one-member blueprint instead buys four properties:
//
//   - Create validates through blueprint.Load, so this store cannot write a
//     file that `arxi blueprint validate agents/<name>.yaml` would reject.
//   - `agent show` renders the same resolved config `run start` executes, so it
//     prints what will run rather than a parallel description of it.
//   - `run start reviewer` and `run start ./reviewer.yaml` load the same bytes,
//     so the frozen snapshot and its SHA are identical either way.
//   - `blueprint create` composes agents already in this directory into a team
//     in the same directory, with no migration and no conversion, and
//     `blueprint install` will land there too.
//
// # Why a team is the same file with more members
//
// `arxi blueprint create feature-team --members backend,frontend,security`
// (§20.4) writes agents/feature-team.yaml, and the only difference from the file
// `agent create` writes is the length of the member list. That is the whole
// reason Team below renders through the same quoting, the same atomic publish and
// the same load-before-return check as Record: the two are not two formats.
//
// The members are COPIED, not referenced. A `members: [backend]` pointing at
// agents/backend.yaml would be shorter and is wrong, because `run start` freezes
// the blueprint into runs/<id>/blueprint.snapshot.yaml (ADR-0001/0002) and a
// reference would leave the rules of the run outside the snapshot: editing
// agents/backend.yaml afterwards would silently change what an already-recorded
// run means, and the SHA that is supposed to identify the whole configuration
// would not cover most of it.
//
// The copy is what makes the refusals in Team.Validate necessary rather than
// fussy -- a member copied out of a file can carry a `stages` list naming stages
// this team does not declare, and kernel.participates would then return false for
// that member in every stage, forever.
//
// # Why the rendered file declares one stage
//
// Because without one it cannot run, and it took a hand-run to notice.
//
// applyRunStarted activates the members of the stage it enters, and returns nil
// when the config declares none. A stageless one-member file therefore starts,
// enters nothing, spawns no turn, and records run.quiescent -- "nobody is
// working and nobody can start" -- as the second event, after zero turns and
// zero dollars. Every check passed it: blueprint.Load accepted it, `arxi
// blueprint validate` accepted it, `agent show` printed it, and `agent create`
// finished by printing `run it:` under it. The only symptom was a run that did
// nothing, which is the one failure this store exists to prevent.
//
// The earlier argument against a stage was that advance rules would then be
// evaluated against a stage the author never wrote. They are, and for one member
// `advance_when: all` advances exactly when that member submits -- the only rule
// a lone member can have. `all` rather than the identical-for-one `any` because
// this file is meant to be grown: the moment a second member is added by hand,
// `any` would advance on whichever finished first and ship half the work, where
// `all` waits. The default that survives being grown is the one to write down.
//
// internal/kernel's TestRunStartedWithoutStagesEntersNothing still pins the
// stageless arm of the reducer. What changed is that no stored agent takes it.
//
// # Where the directory lives, and why one file each
//
// In the working directory, beside runs/, policies/, triggers/ and providers/,
// for the reason internal/toolstore argues at greater length: an agent created
// once for a throwaway experiment must not still exist, with its tool grant, in
// every other repository on the machine afterwards.
//
// One file per agent rather than one index, also following toolstore: a crash
// mid-write cannot lose the definition of an agent the command never mentioned,
// and agents/backend.yaml is a path a human can open, diff and commit.
package agentstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/michiTrader/arxi/internal/blueprint"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/tool"
)

// DefaultDir is where agents live, relative to the working directory.
const DefaultDir = "agents"

// ext is the extension of a stored agent.
//
// `.yaml` rather than `.agent.yaml`: the file *is* a blueprint, and somebody who
// copies one out of agents/ to grow it into a team should not have to rename it
// for `blueprint validate` to believe it.
//
// Load-bearing for the same reason as in toolstore: the temp files below are
// named <name>.yaml.tmp-NNNN, so names() can select on this exact suffix and can
// never hand a half-written file to a reader.
const ext = ".yaml"

// ErrExists and ErrNotExist are the two outcomes a caller must handle
// differently from an I/O failure.
//
// Sentinels rather than string matching, because the CLI answers each of the
// three with a different exit code and a different sentence: an existing agent
// means "your edits are safe, choose another name" (exit 2, the invocation is
// wrong), a missing one means "there is nothing to show" (exit 1, the invocation
// was fine), and a permission error is neither and must not be reported as
// either.
var (
	ErrExists   = errors.New("agent already exists")
	ErrNotExist = errors.New("no such agent")
)

// Record is what `agent create` was told, before it becomes a file.
//
// These five fields are exactly the five params the surface declares for
// `agent create`, and deliberately not a copy of kernel.MemberConfig. A struct
// mirroring MemberConfig would invite rendering `activation` and `stages` --
// fields the command has no flag for and nothing to fill them from -- and the
// file would then carry declarations nobody made.
type Record struct {
	Name     string
	Model    string
	Role     string
	Tools    []string
	Advisory bool
}

// Validate refuses a Record that must not become a file.
//
// The tool grant is checked here rather than only in the CLI so that every
// writer passes through it, including the agent-facing `arxi_agent_create` that
// serve will project from the same surface entry. tool.ValidateGrants reports
// every bad name at once, which is the behaviour internal/tool/policy.go
// promises in its own comment: `--tools reed` should say so now, not grant
// nothing and fail halfway through a paid run.
func (r Record) Validate() error {
	if err := validName("agent", r.Name); err != nil {
		return err
	}
	return tool.ValidateGrants(r.Tools)
}

// validName refuses a name that must not become a filename in this store.
//
// Shared by Record and Team because both become agents/<name>.yaml and both are
// resolved by `run start <name>`, so a rule that held for one and not the other
// would mean `blueprint create` could write a file `agent create` is forbidden to
// write -- into the same directory, read by the same commands. noun is the word
// the message uses, so the refusal says "agent" or "blueprint" rather than
// picking one and being wrong half the time.
func validName(noun, name string) error {
	switch {
	case strings.TrimSpace(name) == "":
		return fmt.Errorf("an %s needs a name", noun)
	case strings.TrimSpace(name) != name:
		// Refused rather than trimmed. The name is the word `run start` is given
		// and the word `run steer` addresses, so an agent whose name carries an
		// invisible space would fail to be addressed for a reason that is not
		// visible anywhere on screen.
		return fmt.Errorf("%s name %q has surrounding whitespace; "+
			"the name is what `run start` and `run steer` are given, and they compare it exactly", noun, name)
	case strings.IndexFunc(name, unicode.IsControl) >= 0:
		// A control character in the name would break all three places the name
		// is printed: the comment at the top of the rendered file, the filename
		// itself (a newline is legal in one on unix), and the aligned columns of
		// `agent list`. None of those failures would name the cause.
		return fmt.Errorf("%s name %q contains a control character", noun, name)
	case strings.ContainsAny(name, `/\`) || name == "." || name == "..":
		// The name becomes a filename. Refused and not sanitised: somebody who
		// typed a slash meant something by it, and quietly renaming their agent
		// is a worse answer than saying it cannot be spelled that way.
		return fmt.Errorf("%s name %q cannot contain a path separator; "+
			"it becomes the filename %s/%s%s", noun, name, DefaultDir, name, ext)
	}
	return nil
}

// Render turns a Record into the bytes that will be written, and proves they
// load before returning them.
//
// The load is the point of rendering rather than templating. This store must be
// incapable of producing a file that `arxi blueprint validate` rejects, because
// the file is the agent's definition and a definition that fails its own
// validator is a worse outcome than a refused command: the refusal is visible
// now, the broken file is discovered later by `run start`. The quoting rules of
// the YAML subset are the concrete way it could happen -- see yamlScalar.
func (r Record) Render() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# %s -- an arxi agent: a blueprint with one member.\n", r.Name)
	b.WriteString("#\n")
	b.WriteString("# `arxi run start <name> \"<objective>\" --budget <usd>` resolves an agent by\n")
	b.WriteString("# name. A path still wins over a name, so ./<file>.yaml never stops meaning\n")
	b.WriteString("# the file it says.\n")
	b.WriteString("#\n")
	b.WriteString("# Editing this by hand is expected: it is an ordinary blueprint, so\n")
	b.WriteString("# `arxi blueprint validate` checks it, and adding members or stages grows it\n")
	b.WriteString("# into a team without moving it.\n")
	fmt.Fprintf(&b, "name: %s\n\n", yamlScalar(r.Name))
	b.WriteString("members:\n")
	fmt.Fprintf(&b, "  - name: %s\n", yamlScalar(r.Name))
	if r.Role != "" {
		fmt.Fprintf(&b, "    role: %s\n", yamlScalar(r.Role))
	}
	if r.Model != "" {
		fmt.Fprintf(&b, "    model: %s\n", yamlScalar(r.Model))
	}
	if len(r.Tools) > 0 {
		fmt.Fprintf(&b, "    tools: [%s]\n", yamlList(r.Tools))
	}
	// advisory is written even when false, unlike the fields above. It is the one
	// member field that changes whether a stage can advance, `agent list` prints
	// a column for it on every row, and a knob that only appears in the file once
	// somebody has already used it is a knob nobody discovers.
	fmt.Fprintf(&b, "    advisory: %t\n", r.Advisory)

	// One stage, and it is not decoration: without it the agent does not run.
	//
	// kernel.applyRunStarted emits stage.entered only when the blueprint declares
	// a stage and returns no effect at all for an empty list, so a stageless file
	// starts, activates nobody, and records run.quiescent -- "nobody is working
	// and nobody can start" -- as the second event, after zero turns. The file
	// loads, `blueprint validate` passes it, and the only symptom is a run that
	// does nothing, which is the failure this store exists to make impossible.
	//
	// `all` rather than `any`, which for one member are the same rule. They stop
	// being the same the moment somebody adds a second member, and that edit is
	// the one this file invites: `any` would then advance on whichever member
	// finished first, shipping half the work, where `all` waits. The default that
	// survives being grown is the right one to write.
	b.WriteString("\nstages:\n")
	b.WriteString("  - {name: work, advance_when: all}\n")

	raw := []byte(b.String())
	if _, err := blueprint.Load(raw); err != nil {
		return nil, fmt.Errorf("agentstore: agent %q does not render to a valid blueprint: %w\n%s", r.Name, err, raw)
	}
	return raw, nil
}

// Team is what `blueprint create` was told, before it becomes a file.
//
// Members are kernel.MemberConfig and not Record, unlike `agent create`. Record
// exists because `agent create` has five flags and must not render fields nobody
// filled in; a composed member is the opposite case, since it is copied out of a
// file that already exists and may already declare `activation` or a per-member
// `stages` list -- both hand-editable, neither reachable from any flag. Narrowing
// it to Record's five fields on the way through would drop those declarations, and
// the composed member would then behave differently from the agent whose name it
// carries.
//
// Stages are names only. Everything else a stage can declare -- timeout_ms,
// on_timeout, workspace, on_conflict -- is a decision about this team's process
// that no member can supply, and inventing values for them would put rules in the
// file that nobody chose. They are a hand edit away in a file designed for hand
// edits.
type Team struct {
	Name    string
	Members []kernel.MemberConfig
	Stages  []string
}

// Validate refuses a Team that must not become a file.
//
// Every check here is one that internal/blueprint would NOT make, and that
// division is the point. blueprint.Load is the schema of a file a human typed, so
// it accepts a memberless blueprint ("a run with no members is caught when the
// run starts") and does not cross-check a member's `stages` list against the
// declared stages. Both are defensible for a hand-written file whose author can
// see what they wrote. Neither is defensible for a file this package composes out
// of arguments, because the author never sees it before it exists: the refusal
// has to arrive while they are still looking at the command.
func (t Team) Validate() error {
	if err := validName("blueprint", t.Name); err != nil {
		return err
	}
	if len(t.Members) == 0 {
		// The surface marks --members required, so the CLI refuses this first.
		// Repeated here because Validate is what every writer passes through,
		// including a `blueprint create` reached through the protocol.
		return errors.New("a blueprint needs at least one member; " +
			"a file with none loads, and then records run.quiescent after zero turns")
	}
	if err := t.checkStageNames(); err != nil {
		return err
	}

	first := map[string]int{}
	for i, m := range t.Members {
		if err := checkMemberName(m.Name); err != nil {
			return fmt.Errorf("members[%d]: %w", i, err)
		}
		if prev, dup := first[m.Name]; dup {
			// blueprint.Load refuses duplicates too, but it would report
			// "members[N]" of a file the user never typed. Refused here so the
			// message can be about the arguments that are still on screen.
			return fmt.Errorf("members[%d] and members[%d] are both called %q; "+
				"a run addresses a member by name, so `run steer` could not say which one it meant",
				prev, i, m.Name)
		}
		first[m.Name] = i
		if err := tool.ValidateGrants(m.Tools); err != nil {
			return fmt.Errorf("member %q: %w", m.Name, err)
		}
		if err := t.checkMemberStages(m); err != nil {
			return err
		}
	}
	return nil
}

// stageNames is the stage list this team will actually declare.
//
// One method rather than a default applied in Render, so that the stages
// Validate checks a member against are the stages the file will contain. A
// default filled in on the way out is a default the checks never saw.
//
// `work` when none are named, for the reason the package doc gives at length: a
// stageless file starts, activates nobody and records run.quiescent after zero
// turns.
func (t Team) stageNames() []string {
	if len(t.Stages) == 0 {
		return []string{"work"}
	}
	return t.Stages
}

// checkStageNames refuses a stage list that would not survive being written.
func (t Team) checkStageNames() error {
	first := map[string]int{}
	for i, s := range t.Stages {
		switch {
		case strings.TrimSpace(s) == "":
			return fmt.Errorf("stages[%d] has no name; a stage is addressed by name "+
				"in `run advance`, in stage.entered and in every member's stage list", i)
		case strings.TrimSpace(s) != s:
			return fmt.Errorf("stage name %q has surrounding whitespace; "+
				"the reducer compares stage names exactly", s)
		case strings.IndexFunc(s, unicode.IsControl) >= 0:
			return fmt.Errorf("stage name %q contains a control character", s)
		}
		if prev, dup := first[s]; dup {
			return fmt.Errorf("stages[%d] and stages[%d] are both called %q; "+
				"a run advances from one stage to the next by name", prev, i, s)
		}
		first[s] = i
	}
	return nil
}

// checkMemberStages refuses a member that could never take a turn.
//
// This is the check that only matters because members are COPIED. A stored agent
// may carry `stages: [build]` from a hand edit, and composing it into a team whose
// stages are `review` and `ship` produces a member that kernel.participates
// answers false for in every stage -- so it is activated never, and the file that
// names it looks complete.
//
// internal/blueprint does not make this check, deliberately: in a hand-written
// file the author can see both lists. Here the author sees neither, because the
// file does not exist yet.
//
// An empty list is the normal case and always passes: participates treats a
// member with no stages as a member of every stage, which is the shape §20.4's
// team.yaml uses and what `agent create` writes.
func (t Team) checkMemberStages(m kernel.MemberConfig) error {
	if len(m.Stages) == 0 {
		return nil
	}
	declared := t.stageNames()
	for _, want := range m.Stages {
		for _, have := range declared {
			if want == have {
				return nil
			}
		}
	}
	return fmt.Errorf("member %q only takes part in stages [%s], and this blueprint declares [%s]; "+
		"it would be activated in no stage at all, so it could never take a turn",
		m.Name, strings.Join(m.Stages, ", "), strings.Join(declared, ", "))
}

// checkMemberName refuses a member name that could not be addressed.
//
// Not validName above, because a member name does not become a filename: telling
// somebody that "backend" becomes agents/backend.yaml when it is a member of
// feature-team.yaml would name a consequence that is not real. What does carry
// over is exactness -- `run steer <member>` and the reducer compare the name
// literally, so an invisible character produces a member that cannot be addressed
// and whose name looks right on screen.
func checkMemberName(name string) error {
	switch {
	case strings.TrimSpace(name) == "":
		return errors.New("a member needs a name")
	case strings.TrimSpace(name) != name:
		return fmt.Errorf("member name %q has surrounding whitespace; "+
			"`run steer` and the reducer compare it exactly", name)
	case strings.IndexFunc(name, unicode.IsControl) >= 0:
		return fmt.Errorf("member name %q contains a control character", name)
	}
	return nil
}

// Render turns a Team into the bytes that will be written, and proves they load
// before returning them.
//
// Same contract as Record.Render for the same reason: this package must be
// incapable of writing a file that `arxi blueprint validate` rejects. It matters
// more here, not less -- a team is composed from names the user typed rather than
// from a file they wrote, so a rendering bug would produce a broken file that
// nobody had the chance to read first.
func (t Team) Render() ([]byte, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	stages := t.stageNames()

	var b strings.Builder
	fmt.Fprintf(&b, "# %s -- an arxi blueprint composed by `arxi blueprint create`.\n", t.Name)
	b.WriteString("#\n")
	fmt.Fprintf(&b, "# Its %d members were COPIED out of %s/ at creation time, not referenced.\n", len(t.Members), DefaultDir)
	b.WriteString("# `run start` freezes a blueprint into runs/<id>/blueprint.snapshot.yaml, so a\n")
	b.WriteString("# reference would leave the rules of the run outside the snapshot that is meant\n")
	b.WriteString("# to be the whole of it. Editing a member here changes this team and nothing\n")
	b.WriteString("# else; editing the agent it came from changes that agent and not this team.\n")
	b.WriteString("#\n")
	b.WriteString("# `arxi run start <name> \"<objective>\" --budget <usd>` resolves this file by\n")
	b.WriteString("# name, and `arxi blueprint validate` checks it after a hand edit.\n")
	fmt.Fprintf(&b, "name: %s\n\n", yamlScalar(t.Name))

	b.WriteString("members:\n")
	for _, m := range t.Members {
		fmt.Fprintf(&b, "  - name: %s\n", yamlScalar(m.Name))
		if m.Role != "" {
			fmt.Fprintf(&b, "    role: %s\n", yamlScalar(m.Role))
		}
		if m.Model != "" {
			fmt.Fprintf(&b, "    model: %s\n", yamlScalar(m.Model))
		}
		if len(m.Tools) > 0 {
			fmt.Fprintf(&b, "    tools: [%s]\n", yamlList(m.Tools))
		}
		if m.Activation != "" {
			// Copied when the source agent had it, omitted otherwise. Writing the
			// kernel's default ("coalesce") explicitly would pin this team to a
			// value nobody chose and stop it from following a later change of
			// default.
			fmt.Fprintf(&b, "    activation: %s\n", yamlScalar(m.Activation))
		}
		if len(m.Stages) > 0 {
			// Never generated, only carried over: a member with no stages takes
			// part in every stage (kernel.participates), which is the shape §20.4
			// documents. Validate has already checked that this list names at
			// least one stage this file declares.
			fmt.Fprintf(&b, "    stages: [%s]\n", yamlList(m.Stages))
		}
		// advisory on every member even when false, as in Record.Render: it is the
		// one member field that changes whether a stage can advance, and in a team
		// -- where `advance_when: quorum:N` is the reason to have one -- reading it
		// off the file must not require knowing the default.
		fmt.Fprintf(&b, "    advisory: %t\n", m.Advisory)
	}

	// advance_when: all on every stage, and no timeouts.
	//
	// `all` is Record.Render's choice for its one member and the right one for
	// several: it waits for everybody. `any` would advance on whichever member
	// finished first and ship a stage's worth of half-done work, and `quorum:N`
	// would be a number this command was never given. §20.4's review stage uses
	// quorum:2 -- that is a hand edit in a file written to be edited, not
	// something to guess from a member count.
	b.WriteString("\nstages:\n")
	for _, s := range stages {
		fmt.Fprintf(&b, "  - {name: %s, advance_when: all}\n", yamlScalar(s))
	}

	raw := []byte(b.String())
	if _, err := blueprint.Load(raw); err != nil {
		return nil, fmt.Errorf("agentstore: blueprint %q does not render to a valid blueprint: %w\n%s", t.Name, err, raw)
	}
	return raw, nil
}

// yamlList renders a flow sequence of scalars, each quoted by the rule above.
//
// Extracted from Record.Render, where it was inline, once a second and third
// caller appeared (a member's tools and its stage list). Three copies of a
// quoting loop is three places for the subset's rules to be applied to two of
// them.
func yamlList(xs []string) string {
	quoted := make([]string, len(xs))
	for i, x := range xs {
		quoted[i] = yamlScalar(x)
	}
	return strings.Join(quoted, ", ")
}

// yamlScalar renders a string so that the parser in internal/blueprint reads
// back that exact string.
//
// Quoting here is not cosmetic. The subset parser refuses `yes`, `no`, `on` and
// `off` in every case variant *by name* -- YAML 1.1 reads them as booleans and
// 1.2 as strings, so rather than pick a side it makes the author choose -- which
// means `arxi agent create no` would otherwise write a file that the very next
// command cannot read at all. `true`, `false`, `null`, `~` and anything that
// parses as a number are the same problem one step further along: they would
// load, as the wrong Go type, and validation would then report
// "members[0]: name must be text" about a file the user never wrote.
//
// Double quotes rather than single, because the parser unquotes them with
// strconv.Unquote, which makes strconv.Quote its exact inverse. Single quotes
// would need YAML's own rule of doubling an inner quote, correct but with no
// matching function in the standard library to be checked against.
func yamlScalar(s string) string {
	if plainSafe(s) {
		return s
	}
	return strconv.Quote(s)
}

// plainSafe reports whether s survives a round trip unquoted.
//
// An allowlist, not a list of dangerous characters. A denylist of the subset's
// specials (#, :, comma, brackets, braces, quotes, &, *, !, |, >, %, @) is one
// omission away from writing a file that misparses into something plausible, and
// the cost of being too strict is a pair of quotes nobody minds. Everything a
// name, a role, a tool or a model id is actually spelled with -- including the
// `/` of `anthropic/claude-sonnet-4-6` and the dots and dashes of a version --
// is inside the set.
func plainSafe(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '/':
		case (r == '-' || r == '.') && i > 0:
			// Leading `-` or `.` are refused: `- x` is a sequence item and `.` is
			// a path, and neither should depend on what follows.
		default:
			return false
		}
	}
	// Number-like and boolean-like plain scalars parse as non-strings, so they
	// have to be quoted even though every character in them is safe.
	if _, err := strconv.ParseInt(s, 10, 64); err == nil {
		return false
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return false
	}
	switch strings.ToLower(s) {
	case "true", "false", "null", "yes", "no", "on", "off":
		return false
	}
	return true
}

// Store is a directory of agents.
type Store struct{ dir string }

// Open prepares dir, creating it if necessary.
//
// For writers. Every reader should take At below, which prepares nothing.
func Open(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("agentstore: no directory given (default is %q)", DefaultDir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("agentstore: create %s: %w", dir, err)
	}
	return &Store{dir: dir}, nil
}

// At names a store without touching the filesystem.
//
// Reporting on a directory must not create it. `arxi agent list` through Open
// would leave an empty agents/ behind in a repository that has never had one,
// and in a checkout the user cannot write to it would fail with "create agents:
// permission denied" -- an error about a directory the command was never asked
// to make, in place of the answer "no agents yet".
//
// Nothing needs preparing for a read: Names already reads a missing directory as
// no names, and Load already reports ErrNotExist. Create is the only operation
// that requires the directory to be there, and Open is what it takes.
//
// The blank-directory guard is Open's, for Open's reason: a store rooted at the
// working directory would make every *.yaml in the repository an agent.
func At(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("agentstore: no directory given (default is %q)", DefaultDir)
	}
	return &Store{dir: dir}, nil
}

// Dir is the directory this store reads and writes.
func (s *Store) Dir() string { return s.dir }

// Path is the file an agent occupies, whether or not it exists.
//
// Exported because every command that mentions an agent prints this path: `agent
// create` so the next step is a file the user can open, `agent show` so a hand
// edit has somewhere to go, and the error from a broken one so it can be fixed
// rather than guessed at.
func (s *Store) Path(name string) string { return filepath.Join(s.dir, name+ext) }

// Create writes a new agent and refuses to replace one that is already there.
//
// Refusing is not caution, it is what the surface declares: `agent create` is
// marked Mutates and is *not* marked Idempotent, so a second call with the same
// name is not required to be a no-op -- and the only alternative to refusing is
// overwriting, which turns a repeated command into a destructive one. The file is
// designed to be edited by hand, so what an overwrite would destroy is a member
// list somebody grew, tools they added, and a stage they wrote.
//
// The Stat is a guard against a repeated command and not a lock. Two creates of
// the same name racing each other can both pass it, and then one rename wins:
// the surviving file is a complete render of one of the two invocations, never a
// mixture, because the publish below is atomic. That is the correct outcome for
// a race between two humans typing the same command, and a lock file would be
// machinery for a case that cannot corrupt anything.
func (s *Store) Create(r Record) (string, error) {
	raw, err := r.Render()
	if err != nil {
		return "", err
	}
	return s.createRaw(r.Name, raw)
}

// CreateTeam writes a new composed blueprint, under exactly Create's rules.
//
// Separate method rather than an interface both records satisfy, because the two
// have one caller each and an interface would be a seam with nothing on the other
// side of it. What they do share -- the existence guard, the atomic publish, the
// 0644 -- is createRaw below, and that is the part where a divergence would
// matter: a `blueprint create` that overwrote where `agent create` refuses would
// be a destructive command hiding behind a familiar name, in the same directory,
// read by the same `agent list`.
func (s *Store) CreateTeam(t Team) (string, error) {
	raw, err := t.Render()
	if err != nil {
		return "", err
	}
	return s.createRaw(t.Name, raw)
}

// Install writes a blueprint this machine did not author, under Create's rules.
//
// It takes bytes rather than a Record or a Team because with `blueprint install`
// the bytes ARE the artifact. The five fields a Record holds cannot carry a stage
// list, a watcher, a timeout, an advance rule or a context spec, so rendering the
// fetched file through Render would install something quietly smaller than what
// was fetched -- and the digest the caller records as provenance would then
// describe bytes that are not the ones on disk.
//
// The load is done here even though `blueprint install` loads too, to report on
// what it got. The first promise in this package's doc is that the store cannot
// write a file `arxi blueprint validate agents/<name>.yaml` would reject, and
// Create and CreateTeam keep it through Render's load-before-return. Install is
// handed bytes nothing in this process composed -- the least trustworthy input the
// store has, from a URL in the general case -- so a writer that skipped the check
// would be the single path able to break that promise, reached by the single input
// most likely to break it.
//
// The name is validated here and not only in the caller, for validName's own
// reason: it becomes agents/<name>.yaml. `--as ../../../etc/cron.d/x` has to be
// refused by the thing that does the writing, because the next caller -- the
// agent-facing projection of some other verb -- will not remember to check.
func (s *Store) Install(name string, raw []byte) (string, error) {
	if err := validName("blueprint", name); err != nil {
		return "", err
	}
	if _, err := blueprint.Load(raw); err != nil {
		return "", err
	}
	return s.createRaw(name, raw)
}

// createRaw is the publish half of Create: refuse if taken, then write atomically.
func (s *Store) createRaw(name string, raw []byte) (string, error) {
	path := s.Path(name)
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("%w: %s", ErrExists, path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("agentstore: stat %s: %w", path, err)
	}
	if err := s.write(name, raw); err != nil {
		return "", err
	}
	return path, nil
}

// Load reads one agent and validates it on the way out.
//
// It returns a *blueprint.Blueprint rather than a Record, and that asymmetry with
// Create is deliberate. The file is authoritative, not the Record that happened
// to render it: somebody may have added a second member, a stage or a watcher,
// and reconstructing a Record would have to throw all of that away to fit five
// fields. Handing back the loaded blueprint means `agent show` describes what is
// there and `run start` executes the same bytes.
//
// A missing agent is an error here, where a missing policy file is not one in
// toolstore. The difference is what absence means: no policy file means "no
// override", a meaningful default the resolver can act on, while no agent file
// means the name does not exist and there is nothing to show or run.
//
// Tool names are NOT checked here, though Create refuses unknown ones. The
// blueprint schema accepts any string list for `tools`, so a stored agent with a
// hand-added tool loads for `run start` -- and a reader that refused it would be
// stricter than the thing that runs it, which is the wrong way round for the two
// to disagree.
func (s *Store) Load(name string) (*blueprint.Blueprint, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("agentstore: no agent name given")
	}
	if strings.ContainsAny(name, `/\`) {
		// Not merely invalid: agents/../../etc/passwd would read outside the
		// store. Create refuses to write such a name, so nothing here can have
		// one, and a read that accepted it would be a path traversal reachable
		// from `arxi_agent_show` with an agent-supplied argument.
		return nil, fmt.Errorf("%w: %q is not an agent name (it contains a path separator)", ErrNotExist, name)
	}

	bp, err := blueprint.LoadFile(s.Path(name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %q (looked in %s)", ErrNotExist, name, s.dir)
		}
		return nil, err
	}
	return bp, nil
}

// Names lists the stored agents in sorted order.
//
// A directory that is not there yields no names and no error: `agent list` in a
// fresh repository is a legitimate question with the answer "none", and the
// precedent is already set by `model list`, `trigger list` and `inbox`, which
// print real empty output rather than complaining about a missing store.
func (s *Store) Names() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("agentstore: read %s: %w", s.dir, err)
	}

	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ext) {
			continue
		}
		out = append(out, strings.TrimSuffix(e.Name(), ext))
	}
	sort.Strings(out)
	return out, nil
}

// Entry is one agent as the directory presents it: loaded, or explained.
//
// Err travels with the entry instead of aborting the walk, because one file that
// does not parse must not hide the agents that do. A `agent list` that gave up on
// the first bad file would answer a question about the whole directory with a
// complaint about one member of it, and the operator would not learn that their
// other six agents are fine.
type Entry struct {
	Name      string
	Path      string
	Blueprint *blueprint.Blueprint // nil exactly when Err is non-nil
	Err       error
}

// List loads every stored agent. Its error is only about the directory itself.
func (s *Store) List() ([]Entry, error) {
	names, err := s.Names()
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(names))
	for _, name := range names {
		e := Entry{Name: name, Path: s.Path(name)}
		e.Blueprint, e.Err = s.Load(name)
		out = append(out, e)
	}
	return out, nil
}

// write publishes body as the agent's file, atomically.
//
// The sequence is toolstore's, for the same reasons: a temp file in the same
// directory (so the rename cannot cross a filesystem), fsync before the rename
// (so the bytes are durable before anything names them), and fsync of the
// directory after (so the name itself survives a crash -- syncing the file does
// not make the entry pointing at it durable).
//
// What it buys here is that `run start <name>` and `agent list` never see a
// partial blueprint. A truncated YAML file would be reported as a validation
// error against a file the user did not write and cannot correct, which is the
// most confusing failure this package could produce.
func (s *Store) write(name string, body []byte) error {
	tmp, err := os.CreateTemp(s.dir, name+ext+".tmp-*")
	if err != nil {
		return fmt.Errorf("agentstore: create temp file for agent %q: %w", name, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // a no-op once the rename has succeeded

	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("agentstore: write agent %q: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("agentstore: fsync agent %q: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("agentstore: close temp file for agent %q: %w", name, err)
	}
	// CreateTemp makes the file 0600. An agent definition is not a secret -- it
	// is a file the whole team is expected to read and commit -- and leaving it
	// unreadable would make it behave differently from every other blueprint.
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("agentstore: chmod agent %q: %w", name, err)
	}
	if err := os.Rename(tmpName, s.Path(name)); err != nil {
		return fmt.Errorf("agentstore: publish agent %q: %w", name, err)
	}
	return fsyncDir(s.dir)
}

func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("agentstore: open %s to sync: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("agentstore: sync %s: %w", dir, err)
	}
	return nil
}
