// Package evalstore persists evaluation runs on disk.
//
// It is a separate package from internal/eval for the reason logstore is
// separate from kernel and trigstore from trigger: internal/eval's own
// architecture test forbids it from importing database/sql, and forbids the
// clock and the network besides. A suite loader that reaches the filesystem for
// anything other than reading the suite is one that cannot be tested without
// one. So the pure package decides what a run IS and whether it is meaningful,
// and this package decides where the bytes live.
//
// # Why a store at all
//
// `eval run` reported its numbers and exited, which made `eval compare`
// undeliverable: comparing needs two runs, and there was nowhere for the first
// one to wait. That is the whole motivation, and it is worth stating because it
// bounds what this package should do. It is not a general run database. It is
// the place `compare` reads from.
//
// # One file per run, and runs are never updated
//
// evals/<id>.json, written once. This differs from trigstore in a way that
// matters: a trigger has a Save because pausing one is an edit to a living
// thing, whereas a run is a measurement that has already happened. There is no
// Save here and no overwrite -- Put refuses an id that exists.
//
// That refusal is the point. A run is evidence, and `compare` exists so a
// decision can be defended by pointing at two of them. Silently replacing e1
// with a different e1 would make yesterday's quoted table unreproducible while
// every path and id in it still resolved, which is the worst available failure:
// the citation keeps working and stops being true.
//
// # There is no Delete, and no pruning
//
// Runs accumulate. That is a real cost -- a suite run nightly leaves 365 files
// a year -- and it is accepted rather than solved, because the alternatives are
// worse in ways that are not obvious until they bite. A retention policy
// deletes the baseline somebody is about to cite. A "keep the last N" rule
// deletes the interesting run, since the interesting run is old by definition:
// it is the one from before the change. And the files are small.
//
// If pruning is ever needed it belongs in a command a human runs and reads the
// output of, not in a store method that a writer can call by accident.
package evalstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/michiTrader/arxi/internal/eval"
)

// DefaultDir is where runs live, relative to the working directory.
//
// Beside runs/ and triggers/ rather than in the user's home directory, and for
// the same reason trigstore chose the same: an eval run measures a suite and a
// blueprint that live in a repository, so a run stored in $HOME is a
// measurement of a project separated from the project it measured. Two
// checkouts of the same repository would also share and interleave their runs,
// which is precisely the mistake `compare`'s suite-digest warning exists to
// catch, arranged by the storage layout.
const DefaultDir = "evals"

// ext is the suffix that makes a file a run.
//
// Temp files written during a Put are named <id>.json.tmp-NNNN, which does not
// end in ext -- that is why List can glob for ext and never see a half-written
// run. Load-bearing, not cosmetic, exactly as in trigstore.
const ext = ".json"

// Store is a directory of evaluation runs.
type Store struct {
	dir string

	// maxN bounds the suffix search in FreeID. Zero means the real limit; only
	// tests set it, so that the exhaustion path can be reached without
	// creating a thousand files to reach it.
	maxN int
}

// defaultMaxSuffix is where FreeID gives up. A thousand runs claiming the same
// UTC second is not a busy afternoon, it is a loop.
const defaultMaxSuffix = 1000

func (s *Store) maxSuffix() int {
	if s.maxN > 0 {
		return s.maxN
	}
	return defaultMaxSuffix
}

// Open prepares dir, creating it if necessary.
func Open(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("evalstore: no directory given (default is %q)", DefaultDir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("evalstore: create %s: %w", dir, err)
	}
	return &Store{dir: dir}, nil
}

// Dir is the directory this store reads and writes.
func (s *Store) Dir() string { return s.dir }

// Path is the file a run of this id occupies.
func (s *Store) Path(id string) string { return filepath.Join(s.dir, id+ext) }

// FreeID returns want, or the first want-2, want-3, ... that no stored run
// occupies.
//
// This exists because run ids are clock timestamps at second resolution, and
// two runs of a fast suite inside one second are not a hypothetical: the CLI
// test that runs a suite twice to have something to compare hit it on the
// first try. The failure that produced was the expensive kind -- the second
// eval executed, spent its money, and only then discovered it had nowhere to
// go, because the name was minted before the work and checked after it.
//
// The alternative was a finer clock. Nanoseconds would collide less, but they
// would also make the id unreadable and unretypeable, and `compare` takes two
// ids a human copies by eye. Resolving the collision keeps the id a timestamp
// that means what it says, and adds a suffix only in the rare case that says
// something true as well: this is the second run of that second.
//
// Suffixed with -2 rather than -1 so the unsuffixed id is not retroactively
// implied to be the first of a series. It usually is the only one.
//
// This is a suggestion, not a reservation. Two processes calling FreeID at the
// same moment can be handed the same name, and the loser's Put still fails --
// which is correct, because Put refusing to overwrite is the guarantee, and a
// helper that made that refusal look impossible would be lying about it.
func (s *Store) FreeID(want string) (string, error) {
	taken, err := s.ids()
	if err != nil {
		return "", err
	}
	// Case-insensitively, matching Put: on a case-insensitive filesystem an id
	// that differs only in case is not free, and offering it here would just
	// route the caller into Put's collision refusal.
	used := make(map[string]bool, len(taken))
	for _, id := range taken {
		used[strings.ToLower(id)] = true
	}
	if !used[strings.ToLower(want)] {
		return want, nil
	}
	// Bounded. An unbounded loop here would spin forever on a directory that
	// somehow contains every suffix, and a run that cannot be named is a
	// failure worth reporting rather than a process worth hanging.
	//
	// maxSuffix is named rather than inlined because it is the one number here
	// a test can reach. Mutation testing flipped `n <= maxSuffix` to `n <` and
	// no test noticed -- correctly, since covering the boundary meant storing
	// a thousand runs to move the loop one step. Naming it lets a test set it
	// low and exercise the exhaustion path in three files instead.
	for n := 2; n <= s.maxSuffix(); n++ {
		cand := fmt.Sprintf("%s-%d", want, n)
		if !used[strings.ToLower(cand)] {
			return cand, nil
		}
	}
	return "", fmt.Errorf("no free run id near %q: every suffix up to -%d is "+
		"already taken in %s, which means a thousand runs started in the same "+
		"second -- something is generating runs in a loop",
		want, s.maxSuffix(), s.dir)
}

// Put writes a run and refuses to replace an existing one.
//
// The refusal is the reason there is no Save alongside it. See the package
// comment: a run is evidence, and an id that silently changes meaning breaks a
// citation while leaving it resolvable.
func (s *Store) Put(sum *eval.RunSummary) error {
	// Validated by the pure package before anything touches the disk, so an
	// unmeaningful run cannot become a file at all.
	//
	// This call is redundant and stays anyway. Mutation testing deleted it and
	// every test still passed, which is the correct result: Encode validates
	// too, so an invalid run is refused either way and no caller can tell the
	// difference in behaviour.
	//
	// What the mutation DID disprove was the comment that used to sit here,
	// which claimed the call existed "so the error arrives before a temp file
	// is created". That was already true without it -- write calls Encode
	// first and returns before CreateTemp -- so the sentence was a
	// justification for something that justified itself. Removing the call
	// would also be defensible; keeping it means the refusal does not depend
	// on the internal ordering of write(), which is a detail Put should not
	// have to know.
	if err := sum.Validate(); err != nil {
		return err
	}

	// Case-insensitive collision check, because macOS and Windows filesystems
	// are. Without it, storing "E1" beside an existing "e1" succeeds here and
	// overwrites on a laptop -- the worst kind of difference, since the machine
	// where it destroys evidence is the one with no tests running on it.
	existing, err := s.ids()
	if err != nil {
		return err
	}
	for _, id := range existing {
		if !strings.EqualFold(id, sum.ID) {
			continue
		}
		if id == sum.ID {
			return fmt.Errorf("run %q already exists (%s).\n"+
				"  a stored run is not updated, because `compare` cites runs by "+
				"id and a citation that keeps resolving while its numbers change "+
				"is worse than one that breaks.\n"+
				// Points at a command that exists. This line said
				// `arxi eval show <id>`, which is not a capability the
				// registry declares -- written from what the store wished
				// were true rather than from the surface. An error message
				// that prescribes a nonexistent command costs the reader the
				// one thing they came here with, which is trust that the
				// tool knows its own shape.
				"  the stored run is readable at %s, and `arxi eval list` "+
				"shows every run", sum.ID, s.Path(id), s.Path(id))
		}
		return fmt.Errorf("run %q collides with the existing %q (%s).\n"+
			"  run ids become filenames, and on macOS and Windows those two are "+
			"the same file -- storing this one would overwrite %q there while "+
			"appearing to work here", sum.ID, id, s.Path(id), id)
	}
	return s.write(sum)
}

// write publishes a run atomically: a crash leaves either no file or a complete
// one, never a truncated run that fails to parse.
func (s *Store) write(sum *eval.RunSummary) error {
	body, err := sum.Encode()
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(s.dir, sum.ID+ext+".tmp-*")
	if err != nil {
		return fmt.Errorf("evalstore: create temp file for run %q: %w", sum.ID, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // a no-op once the rename has succeeded

	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("evalstore: write run %q: %w", sum.ID, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("evalstore: fsync run %q: %w", sum.ID, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("evalstore: close temp file for run %q: %w", sum.ID, err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("evalstore: chmod run %q: %w", sum.ID, err)
	}

	// O_EXCL semantics are not available through Rename, and the collision
	// check in Put is therefore not a lock: two processes storing the same id
	// at once can both pass it. That race is accepted here and named rather
	// than hidden, because the id is a UTC timestamp to the second and the
	// losing writer would have to be a second-identical run of the same suite.
	// Taking logstore's O_EXCL lock for this would serialise every run in the
	// directory to prevent a collision between two runs that are already
	// indistinguishable.
	if err := os.Rename(tmpName, s.Path(sum.ID)); err != nil {
		return fmt.Errorf("evalstore: publish run %q: %w", sum.ID, err)
	}
	// The rename is a directory operation. Fsyncing the file does not make the
	// entry that names it durable, so the directory needs its own sync.
	return fsyncDir(s.dir)
}

// Load reads one run and validates it.
//
// Validation happens on the way IN as well as on the way out, because these are
// text files a human can edit and will: the temptation to fix a status by hand
// is exactly what Validate's status check exists for, and a run edited to say
// "passed" instead of "pass" would otherwise drop that case out of the pass
// rate's denominator without saying so.
func (s *Store) Load(id string) (*eval.RunSummary, error) {
	path := s.Path(id)
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("there is no run %q in %s.\n"+
				"  see what there is: arxi eval list", id, s.dir)
		}
		return nil, fmt.Errorf("evalstore: read %s: %w", path, err)
	}
	sum, err := eval.DecodeRun(body)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	// The filename is what `eval compare e1 e2` looks up, so a run whose id
	// field disagrees with it is addressable by one name and reports another --
	// and `compare` would print a table headed by ids that are not the ones
	// typed, which is a citation pointing at the wrong evidence.
	if want := strings.TrimSuffix(filepath.Base(path), ext); sum.ID != want {
		return nil, fmt.Errorf("%s holds a run with id %q.\n"+
			"  the filename is what `arxi eval compare` looks up, so this run "+
			"answers to %q and reports itself as %q, and a comparison would be "+
			"headed by ids that are not the ones asked for", path, sum.ID, want, sum.ID)
	}
	return sum, nil
}

// List returns every run, newest first.
//
// Newest first, and this is the one ordering decision that differs from
// trigstore's. Triggers are sorted by name because an operator rereads that
// list hunting one row, and a list whose order shifts as triggers are added
// moves the row being watched. Runs are the opposite: they accumulate without
// bound, and the question asked of the list is almost always "what did I just
// run, and what should I compare it against" -- which is the top two rows.
//
// It sorts by ID rather than by mtime. The ids `eval run` mints are UTC
// timestamps in a format that sorts lexically in chronological order, so this
// needs no stat calls and, more importantly, cannot be reordered by a file
// being copied: mtime describes the file, and the id describes the run.
//
// An unreadable file fails the whole listing instead of being skipped, for the
// reason trigstore gives: a list quietly missing a row looks exactly like a
// list of everything that exists, and refusing names the file while omitting
// names nothing.
func (s *Store) List() ([]*eval.RunSummary, error) {
	ids, err := s.ids()
	if err != nil {
		return nil, err
	}
	sortIDs(ids)
	out := make([]*eval.RunSummary, 0, len(ids))
	for _, id := range ids {
		sum, err := s.Load(id)
		if err != nil {
			return nil, err
		}
		out = append(out, sum)
	}
	return out, nil
}

// IDs lists the run ids present, newest first, without parsing the files.
//
// Separate from List because the CLI's most common question -- "what is the
// most recent run of this suite" -- should not require decoding a year of
// nightly runs to answer. List is for when the contents are wanted; this is for
// when the names are.
func (s *Store) IDs() ([]string, error) {
	ids, err := s.ids()
	if err != nil {
		return nil, err
	}
	sortIDs(ids)
	return ids, nil
}

// ids lists the run ids present, in directory order.
func (s *Store) ids() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil // no directory is no runs, not a failure
		}
		return nil, fmt.Errorf("evalstore: read directory %s: %w", s.dir, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ext) {
			continue
		}
		out = append(out, strings.TrimSuffix(e.Name(), ext))
	}
	return out, nil
}

// sortIDs orders ids newest first. See List.
func sortIDs(ids []string) {
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
}

// fsyncDir makes a directory's own metadata durable. Creating and renaming a
// file are directory operations, and fsyncing the file does not make the entry
// that names it durable.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("evalstore: open directory for fsync: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("evalstore: fsync directory %s: %w", dir, err)
	}
	return nil
}
