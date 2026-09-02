// Package agentstore keeps an agent on disk as a blueprint with one member.
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
//   - `blueprint create` and `blueprint install`, both still unwired, land in
//     this same directory with no migration and no conversion.
//
// # Why the rendered file declares no stages
//
// A single-agent run has nothing to advance between, and the reducer has an
// explicit arm for it: applyRunStarted emits no stage.entered when the config
// declares no stages, pinned by TestRunStartedWithoutStagesEntersNothing.
// Rendering a synthetic stage so the file resembled examples/feature-team.yaml
// would put a stage in the state of every single-agent run that the author never
// declared, and advance rules would then be evaluated against it.
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
	switch {
	case strings.TrimSpace(r.Name) == "":
		return errors.New("an agent needs a name")
	case strings.TrimSpace(r.Name) != r.Name:
		// Refused rather than trimmed. The name is the word `run start` is given
		// and the word `run steer` addresses, so an agent whose name carries an
		// invisible space would fail to be addressed for a reason that is not
		// visible anywhere on screen.
		return fmt.Errorf("agent name %q has surrounding whitespace; "+
			"the name is what `run start` and `run steer` are given, and they compare it exactly", r.Name)
	case strings.IndexFunc(r.Name, unicode.IsControl) >= 0:
		// A control character in the name would break all three places the name
		// is printed: the comment at the top of the rendered file, the filename
		// itself (a newline is legal in one on unix), and the aligned columns of
		// `agent list`. None of those failures would name the cause.
		return fmt.Errorf("agent name %q contains a control character", r.Name)
	case strings.ContainsAny(r.Name, `/\`) || r.Name == "." || r.Name == "..":
		// The name becomes a filename. Refused and not sanitised: somebody who
		// typed a slash meant something by it, and quietly renaming their agent
		// is a worse answer than saying it cannot be spelled that way.
		return fmt.Errorf("agent name %q cannot contain a path separator; "+
			"it becomes the filename agents/%s%s", r.Name, r.Name, ext)
	}
	return tool.ValidateGrants(r.Tools)
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
	b.WriteString("# into a team without moving it. There are no stages here because one member\n")
	b.WriteString("# has nothing to advance between, and the reducer enters no stage when a\n")
	b.WriteString("# blueprint declares none.\n")
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
		quoted := make([]string, len(r.Tools))
		for i, t := range r.Tools {
			quoted[i] = yamlScalar(t)
		}
		fmt.Fprintf(&b, "    tools: [%s]\n", strings.Join(quoted, ", "))
	}
	// advisory is written even when false, unlike the fields above. It is the one
	// member field that changes whether a stage can advance, `agent list` prints
	// a column for it on every row, and a knob that only appears in the file once
	// somebody has already used it is a knob nobody discovers.
	fmt.Fprintf(&b, "    advisory: %t\n", r.Advisory)

	raw := []byte(b.String())
	if _, err := blueprint.Load(raw); err != nil {
		return nil, fmt.Errorf("agentstore: agent %q does not render to a valid blueprint: %w\n%s", r.Name, err, raw)
	}
	return raw, nil
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
func Open(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("agentstore: no directory given (default is %q)", DefaultDir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("agentstore: create %s: %w", dir, err)
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
	path := s.Path(r.Name)
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("%w: %s", ErrExists, path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("agentstore: stat %s: %w", path, err)
	}
	if err := s.write(r.Name, raw); err != nil {
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
