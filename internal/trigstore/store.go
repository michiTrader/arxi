// Package trigstore persists triggers on disk.
//
// It is a separate package from internal/trigger for the same reason logstore
// is separate from kernel: internal/trigger's own architecture test forbids it
// from importing os, because a schedule parser that reads the filesystem is a
// schedule parser that cannot be tested without one. So the rule is the same
// one the kernel follows — the pure package decides what a trigger IS, and a
// package one layer out decides where the bytes live.
//
// The alternative was to put these functions in cmd/arxi next to the flag
// parsing. That fails the first time anything other than the CLI needs to read
// a trigger, which is the scheduler — the very next step. Persistence reachable
// only from a main package is persistence that gets copy-pasted.
//
// # One file per trigger
//
// Triggers are stored as triggers/<name>.json, not as rows in one index file.
// An index means `trigger pause nightly-audit` rewrites the record of every
// other trigger too: a crash mid-write loses schedules that had nothing to do
// with the command, and two processes writing different triggers lose one of
// them entirely. With one file per trigger, the blast radius of any write is
// the trigger named in the command.
//
// # There is no Delete
//
// The surface declares create, list, show and pause, and pause exists BECAUSE
// delete does not (docs/design/20-use-cases.md §20.10: a deleted trigger takes
// its history with it, and the reason it was stopped is usually the thing you
// want to read later). A store method with no caller is the one that acquires a
// caller by accident, so it is not written.
package trigstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/michiTrader/arxi/internal/trigger"
)

// DefaultDir is where triggers live, relative to the working directory.
//
// It sits next to runs/ rather than in the user's home directory, and that is a
// real trade-off worth naming: a home-directory store would make triggers
// follow the user between projects, which sounds convenient right up to the
// point where a trigger created while working on one repository fires an agent
// against whatever is checked out later. Triggers name a --then that runs in a
// workspace; keeping them beside the runs they produce means the schedule and
// the thing it schedules move together.
const DefaultDir = "triggers"

// ext is the suffix that makes a file a trigger.
//
// Temp files written during a save are named <name>.json.tmp-NNNN, which does
// NOT end in ext — that is why List can glob for ext and never see a
// half-written file. It is load-bearing, not cosmetic.
const ext = ".json"

// Store is a directory of triggers.
type Store struct{ dir string }

// Open prepares dir, creating it if necessary.
func Open(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("trigstore: no directory given (default is %q)", DefaultDir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("trigstore: create %s: %w", dir, err)
	}
	return &Store{dir: dir}, nil
}

// Dir is the directory this store reads and writes.
func (s *Store) Dir() string { return s.dir }

// Path is the file a trigger of this name occupies.
func (s *Store) Path(name string) string { return filepath.Join(s.dir, name+ext) }

// Create writes a new trigger and refuses to replace an existing one.
//
// `trigger create` on a name already in use is not an update, it is a user who
// has forgotten what they already scheduled. Overwriting silently would replace
// a schedule that is currently being relied on, and the old --then — the part
// that spends money — would be gone with no record that it ever existed.
func (s *Store) Create(r trigger.Record) error {
	if err := r.Validate(); err != nil {
		return err
	}
	// Compared case-insensitively because macOS and Windows filesystems are.
	// Without this, `trigger create Nightly` beside an existing `nightly`
	// succeeds on Linux and overwrites on a laptop, which is the worst kind of
	// difference: the machine where it is destructive is the one with no tests
	// running on it.
	existing, err := s.names()
	if err != nil {
		return err
	}
	for _, n := range existing {
		if !strings.EqualFold(n, r.Name) {
			continue
		}
		if n == r.Name {
			return fmt.Errorf("trigger %q already exists (%s).\n"+
				"  `trigger create` will not replace a schedule that is already "+
				"running: inspect it with `arxi trigger show %s`, and stop it with "+
				"`arxi trigger pause %s`", r.Name, s.Path(n), n, n)
		}
		return fmt.Errorf("trigger %q collides with the existing %q (%s).\n"+
			"  trigger names become filenames, and on macOS and Windows those two "+
			"are the same file — creating this one would overwrite %q there while "+
			"appearing to work here", r.Name, n, s.Path(n), n)
	}
	return s.write(r)
}

// Save replaces an existing trigger, for pause and for recording a firing.
//
// Unlike Create it does not refuse an existing name — replacing is the whole
// point — but it does refuse a name that does not exist yet, so a typo in
// `trigger pause nightley-audit` reports the typo instead of quietly creating a
// second trigger that has never fired and never will.
func (s *Store) Save(r trigger.Record) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if _, err := os.Stat(s.Path(r.Name)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("trigger %q does not exist, so there is nothing to "+
				"update.\n  see what does: arxi trigger list", r.Name)
		}
		return fmt.Errorf("trigstore: stat %s: %w", s.Path(r.Name), err)
	}
	return s.write(r)
}

// write publishes a record atomically: a crash leaves either the old file or
// the new one, never a truncated schedule that fails to parse.
func (s *Store) write(r trigger.Record) error {
	body, err := r.Encode()
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(s.dir, r.Name+ext+".tmp-*")
	if err != nil {
		return fmt.Errorf("trigstore: create temp file for trigger %q: %w", r.Name, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // a no-op once the rename has succeeded

	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("trigstore: write trigger %q: %w", r.Name, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("trigstore: fsync trigger %q: %w", r.Name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("trigstore: close temp file for trigger %q: %w", r.Name, err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("trigstore: chmod trigger %q: %w", r.Name, err)
	}
	if err := os.Rename(tmpName, s.Path(r.Name)); err != nil {
		return fmt.Errorf("trigstore: publish trigger %q: %w", r.Name, err)
	}
	// The rename is a directory operation. Fsyncing the file does not make the
	// entry that names it durable, so the directory needs its own sync.
	return fsyncDir(s.dir)
}

// Load reads one trigger and validates it.
//
// Validation happens on the way IN as well as on the way out, because the file
// is text a human can edit and does edit: `on` and `then` are stored exactly as
// typed, so a file hand-changed to `cron:0 3 30 2 *` has to be caught when it
// is read rather than at 03:00 on a February the 30th that never comes.
func (s *Store) Load(name string) (trigger.Record, error) {
	path := s.Path(name)
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return trigger.Record{}, fmt.Errorf("trigger %q does not exist.\n"+
				"  see what does: arxi trigger list", name)
		}
		return trigger.Record{}, fmt.Errorf("trigstore: read %s: %w", path, err)
	}
	return decode(path, body)
}

// decode turns stored bytes into a validated record.
func decode(path string, body []byte) (trigger.Record, error) {
	var r trigger.Record
	dec := json.NewDecoder(strings.NewReader(string(body)))

	// Unknown fields are refused rather than ignored, for two different
	// reasons. A misspelled key — "budget_perid" — otherwise leaves the field
	// at its zero value, and a zero value that happens to be legal is a
	// setting the user believes they changed. And `next` is the specific field
	// somebody will add by hand, expecting the scheduler to honour it; it is
	// derived on purpose (ADR-0002), so accepting the file while ignoring the
	// field would make it say one thing and do another.
	dec.DisallowUnknownFields()

	if err := dec.Decode(&r); err != nil {
		return trigger.Record{}, fmt.Errorf("%s is not a readable trigger: %w\n"+
			"  this file is written by `arxi trigger create`; if it was edited by "+
			"hand, compare it with another one in the same directory", path, err)
	}
	if err := r.Validate(); err != nil {
		return trigger.Record{}, fmt.Errorf("%s: %w", path, err)
	}
	// The filename is what `trigger show NAME` and `trigger pause NAME` look
	// up, so a record whose name field disagrees with it is addressable by one
	// name and reports another — and a pause would write the record back under
	// the field's name, leaving the original file behind, still firing.
	if want := strings.TrimSuffix(filepath.Base(path), ext); r.Name != want {
		return trigger.Record{}, fmt.Errorf("%s holds a trigger named %q.\n"+
			"  the filename is what `arxi trigger show` and `arxi trigger pause` "+
			"look up, so this trigger answers to %q and reports itself as %q; "+
			"pausing it would write a new file and leave this one firing",
			path, r.Name, want, r.Name)
	}
	return r, nil
}

// List returns every trigger, ordered as `trigger list` prints them.
//
// An unreadable file fails the whole listing instead of being skipped. That is
// the deliberate choice: `trigger list` is what a user reads to confirm a
// trigger exists, and a list quietly missing one row looks exactly like a list
// of everything that exists. Refusing names the file; omitting names nothing.
func (s *Store) List() ([]trigger.Record, error) {
	names, err := s.names()
	if err != nil {
		return nil, err
	}
	out := make([]trigger.Record, 0, len(names))
	for _, n := range names {
		r, err := s.Load(n)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	trigger.SortRecords(out)
	return out, nil
}

// names lists the trigger names present, without parsing the files.
func (s *Store) names() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil // no directory is no triggers, not a failure
		}
		return nil, fmt.Errorf("trigstore: read directory %s: %w", s.dir, err)
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

// fsyncDir makes a directory's own metadata durable. Creating and renaming a
// file are directory operations, and fsyncing the file does not make the entry
// that names it durable.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("trigstore: open directory for fsync: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("trigstore: fsync directory %s: %w", dir, err)
	}
	return nil
}
