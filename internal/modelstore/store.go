// Package modelstore persists providers on disk.
//
// Separate from internal/model for the reason every pure package in this tree
// has a store beside it: internal/model is forbidden by arch_test from touching
// os, so the rule about which models may be called can be tested without a
// filesystem. This package decides only where the bytes live.
//
// # One file per provider
//
// providers/<name>.json, not one index. `model enable` then rewrites the record
// of the provider that owns the model and nothing else, so a crash mid-write
// cannot lose the credentials of a provider the command never mentioned. Same
// reasoning as trigstore, with a sharper consequence: the thing at risk here is
// the pointer to a user's API key.
//
// # Not in the home directory
//
// Providers sit beside runs/ and triggers/ in the working directory. That is a
// real trade-off and it follows trigstore's: a home-directory store would make
// a provider follow the user between projects, which sounds convenient until a
// run in one repository is billed to the credentials of another. Keeping the
// provider next to the runs it pays for means the two move together.
package modelstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/michiTrader/arxi/internal/model"
)

// DefaultDir is where providers live, relative to the working directory.
const DefaultDir = "providers"

// ext is the suffix that makes a file a provider. Temp files are written as
// <name>.json.tmp-NNNN, which does not end in ext, which is why names() can
// glob for ext and never see a half-written provider. Load-bearing.
const ext = ".json"

// Store is a directory of providers.
type Store struct{ dir string }

// Open prepares dir, creating it if necessary.
func Open(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("modelstore: no directory given (default is %q)", DefaultDir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("modelstore: create %s: %w", dir, err)
	}
	return &Store{dir: dir}, nil
}

// Dir is the directory this store reads and writes.
func (s *Store) Dir() string { return s.dir }

// Path is the file a provider of this name occupies.
func (s *Store) Path(name string) string { return filepath.Join(s.dir, name+ext) }

// Add writes a new provider and refuses to replace one that exists.
//
// Refusing rather than overwriting, because `provider add anthropic` on a name
// already registered is not an update — it is a user who has forgotten. An
// overwrite would silently repoint every agent using that provider at a new
// endpoint and a new credential, and reset the enabled flags an operator chose.
func (s *Store) Add(p model.Provider) error {
	if err := p.Validate(); err != nil {
		return err
	}
	existing, err := s.names()
	if err != nil {
		return err
	}
	for _, n := range existing {
		// Case-insensitive, because macOS and Windows filesystems are. Without
		// it, adding `Anthropic` beside `anthropic` succeeds here and destroys
		// the credential pointer on a laptop — the machine with no tests on it.
		if !strings.EqualFold(n, p.Name) {
			continue
		}
		return fmt.Errorf("provider %q is already registered (%s).\n"+
			"  `provider add` will not replace it: replacing would repoint every "+
			"agent using it at a new endpoint and forget which models were "+
			"enabled.\n  see what it offers: arxi model list", n, s.Path(n))
	}
	return s.write(p)
}

// Save replaces a provider that already exists, for `model enable` and
// `model disable`.
//
// It refuses a name that does not exist, so a typo reports the typo instead of
// quietly creating a second provider with no models that no run can use.
func (s *Store) Save(p model.Provider) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if _, err := os.Stat(s.Path(p.Name)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("provider %q is not registered, so there is nothing "+
				"to update.\n  see what is: arxi model list", p.Name)
		}
		return fmt.Errorf("modelstore: stat %s: %w", s.Path(p.Name), err)
	}
	return s.write(p)
}

// write publishes atomically: a crash leaves the old file or the new one, never
// a truncated provider whose credential pointer fails to parse.
func (s *Store) write(p model.Provider) error {
	body, err := p.Encode()
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(s.dir, p.Name+ext+".tmp-*")
	if err != nil {
		return fmt.Errorf("modelstore: create temp file for provider %q: %w", p.Name, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // a no-op once the rename has succeeded

	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("modelstore: write provider %q: %w", p.Name, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("modelstore: fsync provider %q: %w", p.Name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("modelstore: close temp file for provider %q: %w", p.Name, err)
	}
	// 0600 and not 0644, unlike a trigger. This file names the variable holding
	// an API key and the endpoint it is sent to: that is a map to a credential,
	// and there is no reason for another user on the machine to read it.
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("modelstore: chmod provider %q: %w", p.Name, err)
	}
	if err := os.Rename(tmpName, s.Path(p.Name)); err != nil {
		return fmt.Errorf("modelstore: publish provider %q: %w", p.Name, err)
	}
	// The rename is a directory operation; fsyncing the file does not make the
	// entry that names it durable.
	return fsyncDir(s.dir)
}

// Load reads one provider and validates it.
//
// Validated on the way in as well as out, because this file is text a human
// edits: somebody will change api_key_env by hand and paste the key itself, and
// that has to be caught on read rather than sent to a provider.
func (s *Store) Load(name string) (model.Provider, error) {
	path := s.Path(name)
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return model.Provider{}, fmt.Errorf("provider %q is not registered.\n"+
				"  register it: arxi provider add %s", name, name)
		}
		return model.Provider{}, fmt.Errorf("modelstore: read %s: %w", path, err)
	}
	return decode(path, body)
}

// decode turns stored bytes into a validated provider.
func decode(path string, body []byte) (model.Provider, error) {
	var p model.Provider
	dec := json.NewDecoder(strings.NewReader(string(body)))

	// Unknown fields are refused rather than ignored. A misspelled key —
	// "api_key_evn" — otherwise leaves the field empty, and an empty key env is
	// legal (a local server needs none), so the provider would be registered
	// with no credential and every call would come back unauthorized with
	// nothing in the file to explain it. And `api_key` is the field somebody
	// will add by hand; refusing it is the last place to stop a secret from
	// being stored.
	dec.DisallowUnknownFields()

	if err := dec.Decode(&p); err != nil {
		return model.Provider{}, fmt.Errorf("%s is not a readable provider: %w\n"+
			"  this file is written by `arxi provider add`; if it was edited by "+
			"hand, note that `api_key` is not a field — the key is read from the "+
			"variable named in api_key_env", path, err)
	}
	if err := p.Validate(); err != nil {
		return model.Provider{}, fmt.Errorf("%s: %w", path, err)
	}
	// The filename is what a qualified ref (anthropic/claude-...) looks up, so a
	// record whose name field disagrees is addressable by one name and reports
	// another — and a `model enable` would write a new file under the field's
	// name, leaving this one in place with the old flags.
	if want := strings.TrimSuffix(filepath.Base(path), ext); p.Name != want {
		return model.Provider{}, fmt.Errorf("%s holds a provider named %q.\n"+
			"  the filename is what a model ref like %q resolves through, so this "+
			"provider answers to %q and reports itself as %q; enabling a model "+
			"would write a new file and leave this one in place",
			path, p.Name, want+"/<model>", want, p.Name)
	}
	return p, nil
}

// List returns every provider, ordered as `model list` prints them.
//
// An unreadable file fails the whole listing rather than being skipped, for the
// reason trigstore gives: `model list` is what a user reads to confirm what
// exists, and a list quietly missing a row looks exactly like a complete list.
func (s *Store) List() ([]model.Provider, error) {
	names, err := s.names()
	if err != nil {
		return nil, err
	}
	out := make([]model.Provider, 0, len(names))
	for _, n := range names {
		p, err := s.Load(n)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	model.SortProviders(out)
	return out, nil
}

// Owner finds the provider that offers a model id, for `model enable`.
//
// It refuses an ambiguous id instead of picking one, the same refusal
// model.Resolve makes and for the same reason: `model enable claude-sonnet-4-6`
// when two providers offer it would enable one of them and report success, and
// the run that then failed to resolve would contradict a command that said it
// worked.
func (s *Store) Owner(ref string) (model.Provider, string, error) {
	wantProvider, wantID := model.ParseRef(ref)
	ps, err := s.List()
	if err != nil {
		return model.Provider{}, "", err
	}

	var hits []model.Provider
	for _, p := range ps {
		if wantProvider != "" && p.Name != wantProvider {
			continue
		}
		for _, m := range p.Models {
			if m.ID == wantID {
				hits = append(hits, p)
				break
			}
		}
	}

	switch len(hits) {
	case 0:
		// Resolve builds the good "no such model, did you mean" message, and it
		// is reused rather than duplicated. Resolve can also fail because the
		// model is DISABLED, which is not an error here — `model enable` exists
		// precisely to act on a disabled model — so a resolution that succeeds
		// or fails for that reason is not what this branch is for. Reaching
		// here means no provider offers the id at all.
		if _, rerr := model.Resolve(ps, ref); rerr != nil {
			return model.Provider{}, "", rerr
		}
		return model.Provider{}, "", fmt.Errorf("no provider offers a model %q", ref)
	case 1:
		return hits[0], wantID, nil
	default:
		names := make([]string, 0, len(hits))
		for _, h := range hits {
			names = append(names, h.Name+"/"+wantID)
		}
		return model.Provider{}, "", fmt.Errorf("model %q is offered by %d "+
			"providers (%s), so which one to change is not decided.\n"+
			"  name it in full: arxi model enable %s",
			wantID, len(hits), strings.Join(names, ", "), names[0])
	}
}

// names lists the provider names present, without parsing the files.
func (s *Store) names() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil // no directory is no providers, not a failure
		}
		return nil, fmt.Errorf("modelstore: read directory %s: %w", s.dir, err)
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
		return fmt.Errorf("modelstore: open directory for fsync: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("modelstore: fsync directory %s: %w", dir, err)
	}
	return nil
}
