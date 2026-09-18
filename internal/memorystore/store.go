package memorystore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/michiTrader/arxi/internal/fsdurability"
)

// DefaultDir is where memory lives, relative to the working directory.
//
// Beside runs/ rather than in $HOME, for the reason rolestore, trigstore,
// modelstore and agentstore all give: a store in $HOME would let memory written
// while working in one repository influence an agent's answers in the next one,
// silently. Cross-run recall is the feature; cross-repository recall is a
// leakage the user never asked for.
const DefaultDir = "memory"

// ext is the suffix that makes a file a memory version.
const ext = ".json"

// ErrNotFound reports that no live version of a record is visible.
var ErrNotFound = errors.New("memory record not found")

// Store is an append-only set of record versions on disk.
//
// # One file per version, never per record
//
// Every version is its own file, named by its version ID. A file per *record*
// holding the current body would make a write a rewrite, and ADR-0021's whole
// guarantee is that a receipt naming version 2 describes what version 2 said —
// which is false the moment version 2's file can be overwritten. Append-only
// files also mean a crash mid-write loses at most the version being written,
// never a version somebody already received a receipt for.
//
// The cost is that reading a record means reading its versions and walking the
// supersession chain. That is the right trade at this size and the honest one:
// a `current` pointer file would be a cache, and ADR-0002 already decided this
// project's position on caches that can disagree with the truth.
type Store struct{ dir string }

// Open creates the directory if absent and returns a store over it.
func Open(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		dir = DefaultDir
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create memory store %s: %w", dir, err)
	}
	return &Store{dir: dir}, nil
}

// At returns a store over an existing directory without creating it. Used by
// readers so that inspecting memory in a workspace that has none reports
// nothing rather than quietly creating an empty store there.
func At(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		dir = DefaultDir
	}
	if _, err := os.Stat(dir); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// Dir reports the directory backing the store.
func (s *Store) Dir() string { return s.dir }

// Put validates, seals and persists one version.
//
// It refuses to overwrite an existing version file. Because version IDs are
// content-addressed, an existing file with the same ID should hold
// byte-identical content, so the write is a no-op. What must never happen is
// the other case: a different body under an ID somebody already holds a receipt
// for. O_EXCL makes that a filesystem guarantee rather than a check this code
// has to remember to perform.
//
// "Should" is doing real work in that paragraph, and the existing file is
// verified rather than trusted. A mutation replacing O_EXCL with O_TRUNC
// survived the whole suite, and probing why found this: if the file on disk was
// modified after it was written, its name no longer describes its content, so
// the collision is not the harmless one this function assumed. Before the
// check, a re-Put over a tampered file returned a valid-looking record and
// success while the disk still held the forged body — the caller received a
// receipt for content the store does not have, which is the one outcome every
// ADR from 0021 onward exists to prevent. Measured with a probe, not supposed.
func (s *Store) Put(r Record) (Record, error) {
	if err := r.Validate(); err != nil {
		return Record{}, err
	}
	sealed, err := r.Seal()
	if err != nil {
		return Record{}, err
	}
	body, err := json.MarshalIndent(sealed, "", "  ")
	if err != nil {
		return Record{}, fmt.Errorf("encode memory record %s: %w", sealed.VersionID, err)
	}
	path := filepath.Join(s.dir, sealed.VersionID+ext)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return s.verifyExisting(path, sealed)
		}
		return Record{}, fmt.Errorf("create memory version %s: %w", sealed.VersionID, err)
	}
	if _, err := f.Write(append(body, '\n')); err != nil {
		f.Close()
		return Record{}, fmt.Errorf("write memory version %s: %w", sealed.VersionID, err)
	}
	// Sync the file before the directory, then the directory, so that a crash
	// cannot leave a directory entry pointing at a file with no contents. The
	// same order every other store in this project uses.
	if err := f.Sync(); err != nil {
		f.Close()
		return Record{}, fmt.Errorf("sync memory version %s: %w", sealed.VersionID, err)
	}
	if err := f.Close(); err != nil {
		return Record{}, fmt.Errorf("close memory version %s: %w", sealed.VersionID, err)
	}
	if err := fsdurability.SyncDirectory(s.dir); err != nil {
		return Record{}, err
	}
	return sealed, nil
}

// verifyExisting handles the O_EXCL collision: a file already carries this
// version ID.
//
// The benign case is a re-Put of identical content, which content addressing
// makes the expected collision, and returning the record is correct. The other
// case is a file whose content no longer digests to its own name — tampered,
// truncated by a failed write, or corrupted — and it is refused loudly. It
// cannot be repaired by rewriting: this function has the body the caller
// *intended*, and silently replacing the divergent file would destroy the
// evidence that anything was ever wrong, which is exactly what an audit needs.
func (s *Store) verifyExisting(path string, sealed Record) (Record, error) {
	existing, err := os.ReadFile(path)
	if err != nil {
		return Record{}, fmt.Errorf("read existing memory version %s: %w", sealed.VersionID, err)
	}
	var held Record
	if err := json.Unmarshal(existing, &held); err != nil {
		return Record{}, fmt.Errorf("existing memory version %s does not decode: %w -- a version "+
			"file is immutable once written, so this file was damaged or replaced after the "+
			"receipts naming it were issued", sealed.VersionID, err)
	}
	resealed, err := held.Seal()
	if err != nil {
		return Record{}, err
	}
	if resealed.ContentDigest != sealed.ContentDigest {
		return Record{}, fmt.Errorf("memory version %s already exists holding content that "+
			"digests to %q rather than %q: the file was modified after it was written, so its "+
			"name no longer describes its content. Refused rather than overwritten, because "+
			"rewriting it would erase the only evidence that a version somebody holds a "+
			"receipt for was tampered with", sealed.VersionID, resealed.ContentDigest,
			sealed.ContentDigest)
	}
	return sealed, nil
}

// Approve stores a new first version of a record approved by a principal.
// Model-generated material must use Propose: the two entry points exist so the
// containment rule Phase 7 opens with is a function signature rather than a
// field the caller is trusted to set correctly.
func (s *Store) Approve(recordID string, scope Scope, body, origin string) (Record, error) {
	return s.Put(Record{RecordID: recordID, Scope: scope, Kind: Approved, Body: body, Origin: origin})
}

// Propose stores model-generated material as a candidate.
//
// A candidate is stored, inspectable and promotable, and never retrieved for
// presentation: Retrieve filters it out and contextprep refuses a candidate
// receipt at the barrier. Two independent refusals for one rule, because this
// is the containment the phase names first and a single point of enforcement
// would be a single point of regression.
func (s *Store) Propose(recordID string, scope Scope, body, origin, runID string, seq int64) (Record, error) {
	return s.Put(Record{RecordID: recordID, Scope: scope, Kind: Candidate, Body: body,
		Origin: origin, CreatedRun: runID, CreatedSeq: seq})
}

// Correct appends a version that supersedes the current tip of a record.
//
// It resolves the tip itself rather than taking a version ID from the caller.
// A caller-supplied predecessor would let two concurrent corrections both
// supersede version 1, producing two tips and no way to say which is current —
// and the exit evidence requires that "stale versions stop appearing after
// correction", which a forked chain cannot satisfy.
func (s *Store) Correct(recordID string, body, origin string) (Record, error) {
	tip, err := s.tip(recordID)
	if err != nil {
		return Record{}, err
	}
	if tip.Deleted {
		return Record{}, fmt.Errorf("memory record %q was deleted at version %s: correcting a "+
			"tombstone would resurrect the record under a new version, which is the "+
			"resurrection the deletion guarantee forbids", recordID, tip.VersionID)
	}
	return s.Put(Record{RecordID: recordID, Supersedes: tip.VersionID, Scope: tip.Scope,
		Kind: tip.Kind, Body: body, Origin: origin})
}

// Promote turns a candidate into an approved record by appending an approved
// version that supersedes it. The candidate version stays on disk: an audit
// asking whether a presented record was originally model-generated must be able
// to answer yes, and deleting the candidate would erase exactly that.
func (s *Store) Promote(recordID, origin string) (Record, error) {
	tip, err := s.tip(recordID)
	if err != nil {
		return Record{}, err
	}
	if tip.Kind != Candidate {
		return Record{}, fmt.Errorf("memory record %q is already of kind %q at version %s: "+
			"promotion is the transition from proposed to approved and has no meaning for a "+
			"record that never was a candidate", recordID, tip.Kind, tip.VersionID)
	}
	return s.Put(Record{RecordID: recordID, Supersedes: tip.VersionID, Scope: tip.Scope,
		Kind: Approved, Body: tip.Body, Origin: origin})
}

// Delete appends a tombstone superseding the current tip.
//
// Deletion is an append rather than an unlink, and this is the phase's
// "without resurrection" requirement taken literally. Unlinking the files
// removes the record from this copy of the store and from nothing else: any
// replica, backup or synced directory that still holds the bytes reintroduces
// it on the next read, and nothing in the data says it was deleted. A tombstone
// is an assertion that travels with the data, so a store that receives it
// stops presenting the record even if it still holds every earlier version.
func (s *Store) Delete(recordID, origin string) (Record, error) {
	tip, err := s.tip(recordID)
	if err != nil {
		return Record{}, err
	}
	if tip.Deleted {
		return tip, nil
	}
	return s.Put(Record{RecordID: recordID, Supersedes: tip.VersionID, Scope: tip.Scope,
		Kind: tip.Kind, Origin: origin, Deleted: true})
}

// Versions returns every stored version, in no meaningful order.
func (s *Store) Versions() ([]Record, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read memory store %s: %w", s.dir, err)
	}
	var out []Record
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ext) {
			continue
		}
		body, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read memory version %s: %w", e.Name(), err)
		}
		var r Record
		if err := json.Unmarshal(body, &r); err != nil {
			return nil, fmt.Errorf("decode memory version %s: %w", e.Name(), err)
		}
		// A version whose recomputed digest disagrees with the stored one is
		// refused rather than skipped. Skipping would present a partial view
		// of a record's chain as if it were complete, which is how a
		// superseded version comes back to life: drop the tip and the
		// predecessor becomes the tip.
		resealed, err := r.Seal()
		if err != nil {
			return nil, err
		}
		if resealed.ContentDigest != r.ContentDigest {
			return nil, fmt.Errorf("memory version %s has content digest %q but its contents "+
				"digest to %q: a version is immutable once written, so this file was modified "+
				"after the receipts naming it were issued", r.VersionID, r.ContentDigest,
				resealed.ContentDigest)
		}
		out = append(out, r)
	}
	return out, nil
}

// tip resolves the current version of one record by walking supersession.
func (s *Store) tip(recordID string) (Record, error) {
	versions, err := s.Versions()
	if err != nil {
		return Record{}, err
	}
	tips, err := tips(versions)
	if err != nil {
		return Record{}, err
	}
	for _, t := range tips {
		if t.RecordID == recordID {
			return t, nil
		}
	}
	return Record{}, fmt.Errorf("%w: %q", ErrNotFound, recordID)
}

// tips reduces a set of versions to the current version of each record.
//
// A version is current when nothing supersedes it. Derived from the chain on
// every read rather than tracked by a flag, for the reason the package comment
// gives: a flag is a cache, and a cache that disagrees with the chain would let
// a superseded version be presented while the correction sat on disk — the exact
// failure the exit evidence names.
func tips(versions []Record) ([]Record, error) {
	superseded := make(map[string]string, len(versions))
	byID := make(map[string]Record, len(versions))
	for _, v := range versions {
		byID[v.VersionID] = v
		if v.Supersedes == "" {
			continue
		}
		// Two versions superseding the same predecessor is a forked chain: two
		// versions are current and nothing can say which. Refused rather than
		// resolved by a tiebreak, because every available tiebreak — file
		// order, digest order, mtime — would pick a winner deterministically
		// while having no relationship to which correction the user meant.
		if prior, dup := superseded[v.Supersedes]; dup {
			return nil, fmt.Errorf("memory versions %s and %s both supersede %s: the "+
				"supersession chain has forked, so two versions are current and no rule here "+
				"can say which correction the user intended", prior, v.VersionID, v.Supersedes)
		}
		superseded[v.Supersedes] = v.VersionID
	}
	var out []Record
	for _, v := range versions {
		if _, dead := superseded[v.VersionID]; !dead {
			out = append(out, v)
		}
	}
	// Sorted by record then version so that retrieval and inspection see one
	// stable order: a store whose output depends on directory iteration order
	// would produce a different prepared context from the same records, and
	// ADR-0013's reproducibility argument rests on the presentation being a
	// pure function of its inputs.
	sort.Slice(out, func(i, j int) bool {
		if out[i].RecordID != out[j].RecordID {
			return out[i].RecordID < out[j].RecordID
		}
		return out[i].VersionID < out[j].VersionID
	})
	return out, nil
}
