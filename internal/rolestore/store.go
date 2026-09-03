// Package rolestore persists reusable role defaults on disk.
//
// # A role is a fragment of a member, not a document
//
// internal/agentstore renders an agent as a one-member blueprint, on the grounds
// that "an agent" and "a member of a blueprint" are the same noun. A role is not
// the same noun as either. It carries a tool grant and the advisory trait and
// nothing else: no member name, no model, no stage, no objective. There is
// nothing to run, so there is nothing for blueprint.Load to check and no
// `run start` that could execute it -- and a one-member blueprint standing in for
// a role would have to invent the member name, which is the one thing a role must
// leave open.
//
// So this is a JSON record beside toolstore's, and roles/<name>.json is a file a
// human can read without knowing the blueprint schema.
//
// # Applied when an agent is created, never consulted at run time
//
// `agent create --role reviewer` copies this record's fields into the agent's
// rendered YAML. The rendered file records the outcome; it carries no reference
// back here.
//
// That is the load-bearing decision in this package. `run start` freezes
// blueprint.snapshot.yaml so that a run is judged by rules which cannot change
// underneath it, and a `role:` meaning "look up roles/reviewer.json when the run
// starts" would put part of those rules outside the snapshot: redefining a role
// would then silently change every agent that named it, including agents somebody
// reviewed and approved months earlier. Copying makes redefinition safe by making
// it inert -- a role reaches the agents created after it and no others.
//
// The second reason is that `role:` already means something. kernel.Decide picks
// the steer target by Role == "coordinator" and builds a member's Identity as
// "name (role)", and the blueprints in this tree write roles that were never
// defined anywhere. A free-form string cannot become a reference without changing
// what those files mean.
//
// # A role grants nothing by itself
//
// Defining a role creates no agent, touches no existing one, and widens nobody's
// tools. It is a default for a flag the user did not type: `agent create --tools
// read` after `role define auditor --tools bash` grants read, because an explicit
// flag wins over a file written last week.
//
// # One file per role, in the working directory
//
// roles/<name>.json, for the reasons trigstore, modelstore and agentstore give:
// defining one role rewrites one file, so a crash cannot lose a role the command
// never mentioned; and the store sits beside runs/ rather than in $HOME, so a
// role defined for one repository cannot quietly widen an agent's reach in the
// next one.
package rolestore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/michiTrader/arxi/internal/tool"
)

// DefaultDir is where roles live, relative to the working directory.
const DefaultDir = "roles"

// ext is the suffix that makes a file a role.
//
// Load-bearing for the reason it is in toolstore and agentstore: temp files are
// written as <name>.json.tmp-NNNN, which does not end in ext, so Names can select
// on this exact suffix and can never offer a half-written role.
const ext = ".json"

// ErrExists and ErrNotExist are the two outcomes a caller answers differently
// from an I/O failure.
//
// Sentinels rather than string matching, because each gets its own exit code and
// its own sentence: an existing role means "your definition is safe, choose
// another name" (exit 2, the invocation is wrong), a missing one means the name
// resolves to nothing, and a permission error is neither and must not be reported
// as either.
var (
	ErrExists   = errors.New("role already exists")
	ErrNotExist = errors.New("no such role")
)

// Record is one role: the member fields a role is allowed to carry.
//
// Exactly the two params `role define` declares beside the name. Model is
// deliberately absent -- the surface gives the verb no --model flag, and a role
// that pinned a model would decide for `agent create` the one field most likely
// to differ between two agents doing the same job.
type Record struct {
	Name     string   `json:"name"`
	Advisory bool     `json:"advisory"`
	Tools    []string `json:"tools,omitempty"`
}

// Validate refuses a Record that must not become a file.
//
// The name rules are agentstore's, because a role name lands in the same two
// places a name does there: it becomes a filename, and it becomes a YAML scalar --
// the `role:` line of every agent created with it. A control character would
// therefore corrupt a *different* file from the one this command writes, and the
// resulting failure would be reported against an agent whose definition looks
// fine on screen.
//
// An empty role -- no tools, not advisory -- is accepted, and that is a decision
// rather than an oversight. The name alone is worth recording, because `agent
// create` notes a --role that nothing has defined: defining `reviewer` is exactly
// what makes `--role reviewr` visible as a typo.
func (r Record) Validate() error {
	switch {
	case strings.TrimSpace(r.Name) == "":
		return errors.New("a role needs a name")
	case strings.TrimSpace(r.Name) != r.Name:
		// Refused rather than trimmed, for agentstore's reason: the name is what
		// `agent create --role` is compared against, so a role whose name carries
		// an invisible space would fail to be found for a reason not visible
		// anywhere on screen.
		return fmt.Errorf("role name %q has surrounding whitespace; "+
			"`agent create --role` compares the name exactly", r.Name)
	case strings.IndexFunc(r.Name, unicode.IsControl) >= 0:
		return fmt.Errorf("role name %q contains a control character", r.Name)
	case strings.ContainsAny(r.Name, `/\`) || r.Name == "." || r.Name == "..":
		// The name becomes a filename. Refused and not sanitised: somebody who
		// typed a slash meant something by it, and quietly renaming their role is
		// a worse answer than saying it cannot be spelled that way.
		return fmt.Errorf("role name %q cannot contain a path separator; "+
			"it becomes the filename %s/%s%s", r.Name, DefaultDir, r.Name, ext)
	}
	return tool.ValidateGrants(r.Tools)
}

// Encode renders a record as the bytes on disk.
func (r Record) Encode() ([]byte, error) {
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("rolestore: encode role %q: %w", r.Name, err)
	}
	return append(body, '\n'), nil
}

// Store is a directory of roles.
type Store struct{ dir string }

// Open prepares dir, creating it if necessary. Writers use this.
//
// The blank-directory guard matters: a store rooted at the working directory would
// make every *.json in the repository a role, and `role define reviewer` would
// then write reviewer.json next to go.mod.
func Open(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("rolestore: no directory given (default is %q)", DefaultDir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("rolestore: create %s: %w", dir, err)
	}
	return &Store{dir: dir}, nil
}

// At names a store without touching the filesystem. Readers use this.
//
// The split is agentstore's and it is not symmetry for its own sake: `agent
// create --role reviewer` reads this store on every invocation, including the
// overwhelmingly common one where no role has ever been defined. Through Open
// that would leave an empty roles/ behind in a repository that has never had one,
// and in a checkout the user cannot write to it would fail with "create roles:
// permission denied" -- an error about a directory the command was never asked to
// make, in place of the answer "that role is not defined".
//
// Nothing needs preparing for a read: Names reads a missing directory as no
// names, and Load reports ErrNotExist. Create is the only operation that needs
// the directory to exist, and Open is what it takes.
func At(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("rolestore: no directory given (default is %q)", DefaultDir)
	}
	return &Store{dir: dir}, nil
}

// Dir is the directory this store reads and writes.
func (s *Store) Dir() string { return s.dir }

// Path is the file a role occupies, whether or not it exists.
//
// Exported because the CLI prints it. The surface declares no `role list` and no
// `role show`, so the file is the only way to read a role back or change one, and
// a verb that wrote a file without saying where would leave the user nothing to
// open.
func (s *Store) Path(name string) string { return filepath.Join(s.dir, name+ext) }

// Create writes a new role and refuses to replace one that is already there.
//
// What the surface declares, not caution: `role define` is marked Mutates and is
// not marked Idempotent, so a second call with the same name need not be a no-op
// -- and the only alternative to refusing is overwriting, which turns a repeated
// command into a destructive one. There is no --force to offer, either:
// parseInvocation accepts only declared params, and the declaration has three.
//
// Overwriting would be the wrong default even if it were spelled. A role is a
// default for agents *not yet created*, so replacing it in place cannot be
// reviewed against the agents that inherited the old one -- they already recorded
// their own copy, and nothing on disk says which definition they came from.
//
// The Stat is a guard against a repeated command and not a lock. Two defines of
// the same name racing each other can both pass it, and then one rename wins: the
// surviving file is one complete record, never a mixture, because the publish
// below is atomic.
func (s *Store) Create(r Record) (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	body, err := r.Encode()
	if err != nil {
		return "", err
	}
	path := s.Path(r.Name)
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("%w: %s", ErrExists, path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("rolestore: stat %s: %w", path, err)
	}
	if err := s.write(r.Name, body); err != nil {
		return "", err
	}
	return path, nil
}

// Load reads one role, and validates it on the way in.
//
// A missing role is an error here, where a missing policy file is not one in
// toolstore. The difference is what absence means: no policy file means "no
// override", a default the resolver can act on, while no role file means the name
// the user typed defines nothing, and the caller has to decide what to say about
// that.
//
// Validating on the way in is the opposite of agentstore's choice, on purpose.
// agentstore does not re-check tool names when it loads, because the blueprint
// schema accepts any string list and a reader stricter than the thing that runs
// the file is the wrong way round. Here the fault has nowhere else to surface: a
// hand-edited "tools": ["reed"] would be copied into an agent, and
// agentstore.Record.Validate would then refuse `agent create` with "unknown
// tool(s): reed" -- blaming a --tools flag the user never typed, for a name that
// came from a file the message does not mention. Refusing at the read, where the
// role's path is known, is the only place a message can point at the real file.
func (s *Store) Load(name string) (Record, error) {
	if strings.TrimSpace(name) == "" {
		return Record{}, errors.New("rolestore: no role name given")
	}
	if strings.ContainsAny(name, `/\`) {
		// Not merely invalid: roles/../../etc/passwd would read outside the store.
		// Create refuses to write such a name, so nothing here can have one.
		return Record{}, fmt.Errorf("%w: %q is not a role name (it contains a path separator)", ErrNotExist, name)
	}

	raw, err := os.ReadFile(s.Path(name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Record{}, fmt.Errorf("%w: %q (looked in %s)", ErrNotExist, name, s.dir)
		}
		return Record{}, fmt.Errorf("rolestore: read role %q: %w", name, err)
	}

	var rec Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return Record{}, fmt.Errorf("rolestore: parse %s: %w\n"+
			"  this file is text a human can edit; fix it or delete it", s.Path(name), err)
	}
	// The filename is authoritative over the field. They can only disagree if the
	// file was moved or copied by hand, and then the name the user typed is the one
	// they meant -- and the one that must be written into the agent's `role:`.
	rec.Name = name
	if err := rec.Validate(); err != nil {
		return Record{}, fmt.Errorf("rolestore: %s: %w", s.Path(name), err)
	}
	return rec, nil
}

// Names lists the defined roles in sorted order.
//
// A directory that is not there yields no names and no error, as in agentstore:
// asking what is defined in a fresh repository is a legitimate question with the
// answer "nothing".
//
// This exists for `agent create`, not for a `role list` -- the surface declares
// none. When --role names something undefined, the command says so and lists what
// is defined, which is the whole mechanism that turns `--role reviewr` from a
// silently accepted string into a visible typo.
func (s *Store) Names() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("rolestore: read %s: %w", s.dir, err)
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

// write publishes body as the role's file, atomically.
//
// The sequence is toolstore's and agentstore's, for the same reasons: a temp file
// in the same directory (so the rename cannot cross a filesystem), fsync before
// the rename (so the bytes are durable before anything names them), and fsync of
// the directory after (so the name itself survives a crash -- syncing the file
// does not make the entry pointing at it durable).
//
// What it buys here is that `agent create --role X` never reads a truncated role.
// Load refuses a file it cannot parse, so a half-written one would stop agents
// from being created, and the message would name a file the user did not write.
func (s *Store) write(name string, body []byte) error {
	tmp, err := os.CreateTemp(s.dir, name+ext+".tmp-*")
	if err != nil {
		return fmt.Errorf("rolestore: create temp file for role %q: %w", name, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // a no-op once the rename has succeeded

	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("rolestore: write role %q: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("rolestore: fsync role %q: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("rolestore: close temp file for role %q: %w", name, err)
	}
	// CreateTemp makes the file 0600. A role is not a secret -- it is a default the
	// whole team is expected to read and commit, like the agents it is applied to --
	// and leaving it unreadable would make it behave differently from every other
	// file in the tree.
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("rolestore: chmod role %q: %w", name, err)
	}
	if err := os.Rename(tmpName, s.Path(name)); err != nil {
		return fmt.Errorf("rolestore: publish role %q: %w", name, err)
	}
	return fsyncDir(s.dir)
}

func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("rolestore: open %s to sync: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("rolestore: sync %s: %w", dir, err)
	}
	return nil
}
