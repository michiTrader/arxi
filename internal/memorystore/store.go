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

// claimExt is the suffix of a supersession claim: a marker that some version
// has already taken a given predecessor as the one it supersedes.
//
// A distinct suffix rather than a subdirectory so that Versions' existing
// filter — "a file is a version if it ends in .json" — keeps claims out of the
// record set without needing to know they exist. A claim in the record set
// would fail to decode and take the whole store's read down with it.
const claimExt = ".claim"

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
	// The claim is taken before the version file is written, so a refused
	// supersession leaves nothing behind. Ordering it after the write would
	// persist the losing version and then report failure, which is the forked
	// state this exists to prevent, reached by the code preventing it.
	//
	// `mine` reports whether this call created the claim, which is what makes
	// the rollback below safe: O_EXCL means only the creator can be inside
	// that window, so releasing a claim this call created cannot release one
	// somebody else is relying on.
	mine, err := s.claimSupersession(sealed)
	if err != nil {
		return Record{}, err
	}
	// Every failure from here on abandons the write, and an abandoned write
	// must not leave its exclusion behind. Before this, a claim outlived the
	// version it was taken for and permanently froze a record whose chain was
	// perfectly healthy -- see releaseClaimAfterFailedWrite.
	fail := func(err error) (Record, error) {
		return Record{}, s.releaseClaimAfterFailedWrite(sealed, mine, err)
	}
	body, err := json.MarshalIndent(sealed, "", "  ")
	if err != nil {
		return fail(fmt.Errorf("encode memory record %s: %w", sealed.VersionID, err))
	}
	path := filepath.Join(s.dir, sealed.VersionID+ext)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			// The version already exists. If its bytes match, the claim is
			// fulfilled by that file and must stay; if they do not, this write
			// is refused and the claim goes back.
			existing, verifyErr := s.verifyExisting(path, sealed)
			if verifyErr != nil {
				return fail(verifyErr)
			}
			return existing, nil
		}
		return fail(fmt.Errorf("create memory version %s: %w", sealed.VersionID, err))
	}
	if _, err := f.Write(append(body, '\n')); err != nil {
		f.Close()
		return fail(fmt.Errorf("write memory version %s: %w", sealed.VersionID, err))
	}
	// Sync the file before the directory, then the directory, so that a crash
	// cannot leave a directory entry pointing at a file with no contents. The
	// same order every other store in this project uses.
	if err := f.Sync(); err != nil {
		f.Close()
		return fail(fmt.Errorf("sync memory version %s: %w", sealed.VersionID, err))
	}
	if err := f.Close(); err != nil {
		return fail(fmt.Errorf("close memory version %s: %w", sealed.VersionID, err))
	}
	if err := fsdurability.SyncDirectory(s.dir); err != nil {
		return fail(err)
	}
	return sealed, nil
}

// releaseClaimAfterFailedWrite undoes a claim whose version was never written.
//
// # Why a failed write must not keep its exclusion
//
// The claim is taken first so that a refused supersession leaves nothing
// behind, and ADR-0028 justified preferring it to a lock on the grounds that it
// "needs no release because it is the durable record of a fact that does not
// expire: that predecessor now has a successor". A probe of that claim found
// the gap in the sentence. When the write that the claim was taken for fails,
// the fact it records never became true -- no successor exists -- and the claim
// that outlives it is precisely the stale lock the ADR rejected locking to
// avoid, reintroduced under another name.
//
// The consequence was measured, not supposed. An ordinary I/O failure on the
// version write -- no crash, no tampering, no hostile replica -- left a claim
// naming a version that does not exist. `Correct` and `Delete` both resolve a
// tip and then supersede it, so both were refused from then on, permanently,
// for a record whose supersession chain was completely healthy and whose
// retrieval kept serving the pre-correction body as current. Worse, the refusal
// told the user the predecessor "is already superseded by" a version ID that is
// on no disk anywhere, and no verb in the package could see the claim at all:
// Versions, Retrieve and Forks were all blind to it, because a fork is two
// versions and this is zero.
//
// Releasing it is safe here and only here. O_EXCL means the creator of a claim
// is its exclusive holder, so a claim this call created is one no other writer
// can be acting on; `mine` carries that distinction, and a claim found already
// held is never released, because it belongs to somebody else's write.
//
// A crash between the claim and the write still strands one -- no in-process
// rollback can cover a process that stops existing -- and that residue is now
// visible through Claims and resolvable through ReleaseClaim rather than being
// permanent and invisible.
func (s *Store) releaseClaimAfterFailedWrite(sealed Record, mine bool, cause error) error {
	if !mine || sealed.Supersedes == "" {
		return cause
	}
	path := filepath.Join(s.dir, sealed.Supersedes+claimExt)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		// Reported together with the cause rather than swallowed: the write
		// failed AND the record is now frozen, and an operator who is told only
		// the first will not know to run ReleaseClaim.
		return fmt.Errorf("%w -- and the supersession claim on %s could not be released "+
			"afterwards (%v), so memory record %q now refuses correction and deletion until "+
			"an operator releases it", cause, sealed.Supersedes, err, sealed.RecordID)
	}
	if err := fsdurability.SyncDirectory(s.dir); err != nil {
		return fmt.Errorf("%w -- and the directory holding the released supersession claim on "+
			"%s could not be synced (%v)", cause, sealed.Supersedes, err)
	}
	return cause
}

// claimSupersession reserves a predecessor for exactly one successor.
//
// # Why the filesystem and not a check
//
// `Correct` reads the current tip and then writes a version superseding it.
// Two callers read the same tip and both write: both succeed, the record has
// two current versions, and the store is unreadable. That is ADR-0006's race
// exactly — two writers modifying state the other one read — and that ADR
// settled this project's answer as compare-and-swap against a version token
// rather than a lock. `Supersedes` is that token: it names one immutable
// version of one record.
//
// A probe raced two concurrent `Correct` calls and bricked the store in three
// runs out of five, with both calls returning nil. Nothing in the suite saw it,
// because the fork guard built its fork from two deliberate `Put` calls and
// asserted only that retrieval then failed — which cannot tell a contained
// refusal from a catastrophic one.
//
// O_EXCL makes the exclusion a property of the filesystem rather than a check
// this code must remember to perform under concurrency, which is the same
// mechanism and the same argument `Put` already uses to refuse overwriting a
// version file. It needs no release, and that is why it is preferred to a lock:
// a lock must be released, so a crash between claim and write strands one, and
// every rule for breaking a stale lock is a guess about whether the holder is
// alive. This file records a fact that does not expire — that predecessor now
// has a successor.
//
// The claim is never read by retrieval. `tips` still derives the current
// version by walking supersession, so this is a write-side exclusion and not a
// second source of truth: a claim file that retrieval trusted would be the
// `current` pointer ADR-0027 rejected, and it could disagree with the chain.
// It reports whether this call created the claim. Only the creator may release
// it on a failed write, and that distinction is what keeps the rollback from
// stealing a claim another writer is mid-flight on.
func (s *Store) claimSupersession(sealed Record) (bool, error) {
	if sealed.Supersedes == "" {
		return false, nil
	}
	path := filepath.Join(s.dir, sealed.Supersedes+claimExt)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if !os.IsExist(err) {
			return false, fmt.Errorf("claim memory version %s for supersession: %w", sealed.Supersedes, err)
		}
		held, readErr := os.ReadFile(path)
		if readErr != nil {
			return false, fmt.Errorf("read supersession claim on memory version %s: %w",
				sealed.Supersedes, readErr)
		}
		holder := strings.TrimSpace(string(held))
		// The identical successor is the benign collision and must stay
		// successful: version IDs are content-addressed, so re-applying the
		// same correction produces the same version, and Put is idempotent by
		// design. Refusing here would break re-Put and every retry above it.
		if holder == sealed.VersionID {
			return false, nil
		}
		// A claim whose successor was never written is refused with the truth
		// rather than with the fork message. The holder does not exist, so
		// saying the predecessor "is already superseded by" it asserts a
		// supersession that never happened and sends the operator looking for
		// a version that is on no disk. This is the residue of a crash between
		// claim and write; the in-process rollback cannot cover that, so the
		// message names the verb that can.
		if _, statErr := os.Stat(filepath.Join(s.dir, holder+ext)); os.IsNotExist(statErr) {
			return false, fmt.Errorf("memory version %s is claimed for supersession by %s, but "+
				"no such version was ever written: the write that took the claim did not "+
				"complete, so the record is frozen behind an exclusion for a successor that "+
				"does not exist. This is not a fork -- there is nothing to choose between. "+
				"Release the claim with ReleaseClaim(%q) after confirming no writer is still "+
				"in flight, then re-apply the correction",
				sealed.Supersedes, holder, sealed.Supersedes)
		}
		return false, fmt.Errorf("memory version %s is already superseded by %s, so %s cannot also "+
			"supersede it: one predecessor has one successor, because two would make both "+
			"current with no rule able to say which correction the user meant. Re-read the "+
			"current version and decide whether this correction still applies to it -- it is "+
			"not retried automatically, because replaying it onto a version its author never "+
			"saw would silently overwrite the correction that won",
			sealed.Supersedes, holder, sealed.VersionID)
	}
	if _, err := f.Write([]byte(sealed.VersionID + "\n")); err != nil {
		f.Close()
		return true, fmt.Errorf("write supersession claim on memory version %s: %w", sealed.Supersedes, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return true, fmt.Errorf("sync supersession claim on memory version %s: %w", sealed.Supersedes, err)
	}
	if err := f.Close(); err != nil {
		return true, fmt.Errorf("close supersession claim on memory version %s: %w", sealed.Supersedes, err)
	}
	// The claim is durable before the version that depends on it is written.
	// Reversed, a crash could leave a version file whose predecessor was never
	// claimed, and the next writer would fork the chain against it.
	if err := fsdurability.SyncDirectory(s.dir); err != nil {
		return true, err
	}
	return true, nil
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
//
// It fails for a forked record and only for a forked record. Before ADR-0028
// it failed whenever ANY record in the store was forked, because tips()
// returned one error for the whole set — which made Correct, Delete and
// Promote unavailable store-wide. Those are the three verbs that could repair a
// fork, so one damaged record permanently disabled the repair of every healthy
// one. Measured with a probe, not supposed.
func (s *Store) tip(recordID string) (Record, error) {
	versions, err := s.Versions()
	if err != nil {
		return Record{}, err
	}
	live, forked := tips(versions)
	for _, t := range live {
		if t.RecordID == recordID {
			return t, nil
		}
	}
	if fork, bad := forked[recordID]; bad {
		return Record{}, fork.err()
	}
	return Record{}, fmt.Errorf("%w: %q", ErrNotFound, recordID)
}

// Fork describes one record whose supersession chain has more than one current
// version, which makes the record unreadable until an operator resolves it.
type Fork struct {
	RecordID string `json:"record_id"`
	// Predecessor is the version two successors both claim.
	Predecessor string `json:"predecessor"`
	// Successors are the competing versions, sorted for a stable message.
	Successors []string `json:"successors"`
}

// err renders the refusal a caller sees when it touches a forked record.
func (f Fork) err() error {
	return fmt.Errorf("memory record %q has a forked supersession chain: versions %s both "+
		"supersede %s, so two versions are current and no rule here can say which correction "+
		"the user intended. Resolve it by inspecting the competing versions; a tiebreak on "+
		"file order, digest or mtime would be deterministic and unrelated to what was meant",
		f.RecordID, strings.Join(f.Successors, " and "), f.Predecessor)
}

// Forks reports every record whose chain has forked, for an operator resolving
// one.
//
// This is the reason containment can exclude a record without losing it. A
// record dropped from retrieval with no trace anywhere is indistinguishable
// from a record that was never written, and silent loss is the failure this
// whole store exists to prevent. Version IDs are disclosed here — an explicit
// local inspection verb — rather than in a retrieval error, which before
// ADR-0028 carried them across the tenant boundary to whoever happened to call
// Retrieve next.
func (s *Store) Forks() ([]Fork, error) {
	versions, err := s.Versions()
	if err != nil {
		return nil, err
	}
	_, forked := tips(versions)
	out := make([]Fork, 0, len(forked))
	for _, f := range forked {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RecordID < out[j].RecordID })
	return out, nil
}

// Claim is one supersession claim held against a predecessor version.
type Claim struct {
	// Predecessor is the version that has been claimed for supersession.
	Predecessor string `json:"predecessor"`
	// Successor is the version ID recorded in the claim.
	Successor string `json:"successor"`
	// RecordID is the record the predecessor belongs to, empty when the
	// predecessor's own version file is not in this store.
	RecordID string `json:"record_id,omitempty"`
	// Fulfilled reports whether the successor was actually written. An
	// unfulfilled claim is the residue of a write that did not complete, and
	// it freezes its record until it is released.
	Fulfilled bool `json:"fulfilled"`
}

// Claims reports every supersession claim and whether its successor exists.
//
// This is the verb that was missing, and its absence is what made an
// unfulfilled claim permanent. A fork is two versions and is reported by
// Forks; an unfulfilled claim is *zero* versions, so Forks cannot see it,
// Versions skips it because it does not end in .json, and Retrieve keeps
// serving the pre-correction body as though nothing were wrong. A record could
// therefore be frozen against correction and deletion with no verb in the
// package able to say why. ADR-0028 argued that excluding a record with no
// trace makes a lost correction indistinguishable from one never written; an
// exclusion with no trace is that same failure one layer down.
func (s *Store) Claims() ([]Claim, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read memory store %s: %w", s.dir, err)
	}
	owner := make(map[string]string)
	versions, err := s.Versions()
	if err != nil {
		return nil, err
	}
	for _, v := range versions {
		owner[v.VersionID] = v.RecordID
	}
	out := make([]Claim, 0)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), claimExt) {
			continue
		}
		predecessor := strings.TrimSuffix(e.Name(), claimExt)
		held, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read supersession claim %s: %w", e.Name(), err)
		}
		successor := strings.TrimSpace(string(held))
		_, statErr := os.Stat(filepath.Join(s.dir, successor+ext))
		out = append(out, Claim{Predecessor: predecessor, Successor: successor,
			RecordID: owner[predecessor], Fulfilled: statErr == nil})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Predecessor < out[j].Predecessor })
	return out, nil
}

// ReleaseClaim removes an unfulfilled supersession claim so the record it
// froze can be corrected again.
//
// # Why this refuses to release a fulfilled claim
//
// A claim whose successor exists is load-bearing: it is the exclusion that
// makes one predecessor have one successor, which is the whole of ADR-0028.
// Releasing it would let a second successor be written against a predecessor
// that already has one, recreating by hand exactly the fork that ADR made
// unreachable. So this verb is scoped to the case where the successor was
// never written, and that condition is checked here rather than trusted to the
// caller.
//
// # Why it is explicit rather than automatic
//
// A claim with no successor file is *usually* abandoned, and sometimes it is a
// writer a few microseconds into its own Put. Nothing on disk distinguishes
// the two, so an automatic sweep -- "no file, take the claim" -- would race the
// legitimate in-flight window and let two writers supersede one predecessor,
// which is the fork this store refuses to resolve. A probe confirmed the naive
// rule reclaims a live in-flight claim. That is why recovery is an operator
// act with the evidence from Claims in hand, on the same reasoning ADR-0028
// gives for not retrying a losing correction automatically: the machine cannot
// know the fact, and guessing it silently overwrites somebody's work.
func (s *Store) ReleaseClaim(predecessor string) error {
	if strings.TrimSpace(predecessor) == "" {
		return fmt.Errorf("release supersession claim: no predecessor version named")
	}
	path := filepath.Join(s.dir, predecessor+claimExt)
	held, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no supersession claim is held on memory version %s: releasing "+
				"a claim that does not exist would report a repair that did not happen",
				predecessor)
		}
		return fmt.Errorf("read supersession claim on memory version %s: %w", predecessor, err)
	}
	successor := strings.TrimSpace(string(held))
	if _, err := os.Stat(filepath.Join(s.dir, successor+ext)); err == nil {
		return fmt.Errorf("the supersession claim on memory version %s is fulfilled by %s, "+
			"which exists: releasing it would allow a second version to supersede %s, and two "+
			"successors is the fork that has no rule able to say which correction the user "+
			"meant. Only a claim whose successor was never written may be released",
			predecessor, successor, predecessor)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat claimed successor %s of memory version %s: %w",
			successor, predecessor, err)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("release supersession claim on memory version %s: %w", predecessor, err)
	}
	return fsdurability.SyncDirectory(s.dir)
}

// tips reduces a set of versions to the current version of each healthy record,
// and separately reports the records it could not resolve.
//
// A version is current when nothing supersedes it. Derived from the chain on
// every read rather than tracked by a flag, for the reason the package comment
// gives: a flag is a cache, and a cache that disagrees with the chain would let
// a superseded version be presented while the correction sat on disk — the exact
// failure the exit evidence names.
//
// # A fork is contained to its own record, not raised for the whole store
//
// Two versions superseding the same predecessor is still refused rather than
// resolved by a tiebreak (ADR-0027): every available tiebreak — file order,
// digest order, mtime — would pick a winner deterministically while having no
// relationship to which correction the user meant. What changed in ADR-0028 is
// the blast radius. This function used to return an error for the whole set, so
// Retrieve failed before authorization ran and one corrupt record in one tenant
// denied memory to every tenant in the store — the mirror image of the argument
// ADR-0027 makes for authorizing before ranking, and one it did not make.
//
// So the split is two return values rather than a result and an error. Healthy
// records resolve; forked records are excluded from the live set and handed
// back by identity, so a caller can refuse the one record without refusing the
// store, and can say which record is affected instead of leaking version IDs
// across a scope boundary in an error string.
func tips(versions []Record) ([]Record, map[string]Fork) {
	successors := make(map[string][]string, len(versions))
	for _, v := range versions {
		if v.Supersedes == "" {
			continue
		}
		successors[v.Supersedes] = append(successors[v.Supersedes], v.VersionID)
	}

	// A predecessor with more than one successor condemns its whole record.
	// Keyed by record rather than by version because exclusion is per record:
	// the question a caller asks is "may I present this record", and with two
	// current versions the answer is no regardless of which one it found.
	forked := make(map[string]Fork)
	byVersion := make(map[string]Record, len(versions))
	for _, v := range versions {
		byVersion[v.VersionID] = v
	}
	for predecessor, claimants := range successors {
		if len(claimants) < 2 {
			continue
		}
		recordID := predecessor
		if r, known := byVersion[predecessor]; known {
			recordID = r.RecordID
		} else if r, known := byVersion[claimants[0]]; known {
			// The predecessor's own file may be absent in an imported store
			// that carried only the competing successors. The record ID is
			// still recoverable from a claimant, and naming the record is the
			// whole point of the containment.
			recordID = r.RecordID
		}
		sorted := append([]string(nil), claimants...)
		sort.Strings(sorted)
		forked[recordID] = Fork{RecordID: recordID, Predecessor: predecessor, Successors: sorted}
	}

	var out []Record
	for _, v := range versions {
		if _, bad := forked[v.RecordID]; bad {
			continue
		}
		if len(successors[v.VersionID]) > 0 {
			continue
		}
		out = append(out, v)
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
	return out, forked
}
