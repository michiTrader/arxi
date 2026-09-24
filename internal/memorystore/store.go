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
// Beside runs/ rather than in $HOME: a store in $HOME would let memory written
// while working in one repository influence an agent's answers in the next one,
// silently. Cross-run recall is the feature; cross-repository recall is a
// leakage the user never asked for. trigstore/store.go argues the same trade-off
// for triggers; the other stores state the location without the reasoning, so
// the project-local property is held across the whole family by a derived test
// (internal/store_locality_test.go) rather than trusted store by store — see
// ADR-0035 for why the guarantee is pinned rather than restated in each comment.
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
	// A root write — one that supersedes nothing — must be the record's first
	// version. If the record already has a different version, a second root would
	// give it two current versions with no supersession between them: the root
	// fork tips() now contains, but far better refused at the source with the verb
	// that avoids it named. Correct supersedes the tip; Approve and Propose start
	// a record, and starting one that already exists is the mistake this catches.
	//
	// The claim mechanism cannot cover this the way it covers a supersession race:
	// a root has no predecessor to claim, so two concurrent first writes for one
	// record still both succeed and are contained at read, exactly as an imported
	// fork is. This guard removes the sequential mistake, which is the common one;
	// the read-side containment remains the backstop for the concurrent one.
	if sealed.Supersedes == "" {
		if err := s.refuseSecondRoot(sealed); err != nil {
			return Record{}, err
		}
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

// refuseSecondRoot rejects a root write for a record that already has a
// different version.
//
// An identical re-write is allowed through: version IDs are content-addressed,
// so a re-Approve of the same body resolves to the same version, and Put is
// idempotent by design — refusing it would break every retry above it. Only a
// version with the same record ID and a *different* version ID is a genuine
// second root, and that is what forks the record.
func (s *Store) refuseSecondRoot(sealed Record) error {
	versions, err := s.Versions()
	if err != nil {
		return err
	}
	for _, v := range versions {
		if v.RecordID != sealed.RecordID || v.VersionID == sealed.VersionID {
			continue
		}
		return fmt.Errorf("memory record %q already has version %s, so a new first version cannot be "+
			"created for it: two versions that supersede nothing would both be current, and no rule "+
			"could say which the user meant. Use Correct to change a live record; a deleted one is "+
			"terminal by design and its identity is not reused", sealed.RecordID, v.VersionID)
	}
	return nil
}

// Approve stores a new first version of a record approved by a principal.
// Model-generated material must use Propose: the two entry points exist so the
// containment rule Phase 7 opens with is a function signature rather than a
// field the caller is trusted to set correctly.
func (s *Store) Approve(recordID string, scope Scope, sensitivity Sensitivity, purpose Purpose, evidenceClass EvidenceClass, confidence Confidence, retention Retention, body, origin string) (Record, error) {
	return s.Put(Record{RecordID: recordID, Scope: scope, Sensitivity: sensitivity, Purpose: purpose, EvidenceClass: evidenceClass, Confidence: confidence, Retention: retention, Kind: Approved, Body: body, Origin: origin})
}

// Propose stores model-generated material as a candidate.
//
// A candidate is stored, inspectable and promotable, and never retrieved for
// presentation: Retrieve filters it out and contextprep refuses a candidate
// receipt at the barrier. Two independent refusals for one rule, because this
// is the containment the phase names first and a single point of enforcement
// would be a single point of regression.
func (s *Store) Propose(recordID string, scope Scope, sensitivity Sensitivity, purpose Purpose, evidenceClass EvidenceClass, confidence Confidence, retention Retention, body, origin, runID string, seq int64) (Record, error) {
	return s.Put(Record{RecordID: recordID, Scope: scope, Sensitivity: sensitivity, Purpose: purpose, EvidenceClass: evidenceClass, Confidence: confidence, Retention: retention, Kind: Candidate, Body: body,
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
		Kind: tip.Kind, Sensitivity: tip.Sensitivity, Purpose: tip.Purpose, EvidenceClass: tip.EvidenceClass, Confidence: tip.Confidence, Retention: tip.Retention, Body: body, Origin: origin})
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
		Kind: Approved, Sensitivity: tip.Sensitivity, Purpose: tip.Purpose, EvidenceClass: tip.EvidenceClass, Confidence: tip.Confidence, Retention: tip.Retention, Body: tip.Body, Origin: origin})
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
		Kind: tip.Kind, Sensitivity: tip.Sensitivity, Purpose: tip.Purpose, EvidenceClass: tip.EvidenceClass, Confidence: tip.Confidence, Retention: tip.Retention, Origin: origin, Deleted: true})
}

// Expire tombstones every live record whose retention marks it expirable and
// leaves the permanent ones untouched. It is the mechanism that fails on
// retention, and its existence is what keeps retention from being the
// field-nothing-fails-on defect: a lifecycle dimension with no sweep acting on it
// would decide nothing, exactly as a confidence the ranker never read would order
// nothing (ADR-0046).
//
// # The store decides what may expire; the caller decides when
//
// There is no clock here and there is no as-of argument, because this store
// deliberately reads no clock (the same reason valid time is still deferred).
// Retention is therefore not a duration the store counts down but a policy the
// store honors: an ephemeral record is one a sweep may collect, a permanent one is
// not. Deciding when to sweep -- at the end of a session, on a schedule -- is the
// caller's, and the clock that triggers it lives outside this package, which is
// the pure-reducer discipline applied to lifecycle: the policy is data the store
// holds, the timing is an input the store is given.
//
// # Expiry is a tombstone, never an unlink
//
// Each expiry is an ordinary Delete: a tombstone superseding the tip, not a file
// removed. Unlinking would drop the record from this copy and from nothing else,
// so any replica or backup still holding the bytes would resurrect it -- the exact
// argument Delete makes for the explicit case, and it holds identically when the
// deletion is driven by a sweep rather than an operator. So an expired ephemeral
// record inherits the whole no-resurrection guarantee, and a receipt that named an
// earlier version of it still resolves against the versions left on disk.
//
// A forked record is skipped rather than expired: it has no single tip to
// supersede, is already withheld from retrieval, and needs an operator's Resolve
// before any verb can act on it. Expiring one head would deepen the fork, not
// clear it.
func (s *Store) Expire(origin string) ([]string, error) {
	versions, err := s.Versions()
	if err != nil {
		return nil, err
	}
	live, _ := tips(versions)
	// Collect the expirable record IDs from one snapshot before writing any
	// tombstone. Each Delete appends a version, so re-reading mid-loop would see
	// the tombstones this sweep is still writing; taking the set first keeps the
	// sweep a function of the store as it stood when Expire was called.
	var expirable []string
	for _, r := range live {
		// A tombstone is already the absence of a record; expiring it again would
		// be a no-op tombstone superseding a tombstone. Skip it so the returned set
		// names records this call actually removed.
		if r.Deleted {
			continue
		}
		// Expirability is read through the vocabulary, not compared against the
		// Ephemeral literal, so the one place that decides what a sweep may remove is
		// the retention map. A record whose retention is unknown reports not
		// expirable and is left alone -- a deletion justified by a policy the
		// vocabulary does not recognize is the fail-open direction Validate already
		// closes on the way in, and Expire closes it again here so a value that
		// somehow reached disk is still not swept.
		if expire, _ := r.Retention.Expirable(); !expire {
			continue
		}
		expirable = append(expirable, r.RecordID)
	}
	sort.Strings(expirable)
	expired := make([]string, 0, len(expirable))
	for _, recordID := range expirable {
		if _, err := s.Delete(recordID, origin); err != nil {
			return expired, fmt.Errorf("expire memory record %q: %w", recordID, err)
		}
		expired = append(expired, recordID)
	}
	return expired, nil
}

// Resolve heals a forked record by keeping one current version the operator
// chose and retiring the rest.
//
// # Why a verb exists at all, and why superseding by hand cannot replace it
//
// ADR-0028 and ADR-0031 made a fork contained and visible: retrieval excludes
// it, Forks names it, tip refuses it. What neither provided was a way back to a
// single current version, and the refusal message told the operator to
// "supersede the ones that are wrong" — advice a probe proved false. A
// supersession has one parent, so appending one turns a single head into a
// non-head and adds a single new head: the number of current versions is
// unchanged. No sequence of Corrects can bring a two-headed record to one head,
// and Correct, Delete and Promote all resolve through tip and so refuse a forked
// record outright. The fork was permanent, and the remediation named a remedy
// the API did not have.
//
// Resolve is the missing edge. It appends one version that supersedes the head
// the operator keeps and names every other head in Retires, which headsByRecord
// treats as no longer current. One append, many heads retired: the head count
// drops to one and the record is readable again.
//
// # Why the operator names the survivor, and the store never picks
//
// The version to keep is exactly the fact ADR-0027 and ADR-0031 refused to
// guess. A tiebreak on version ID, mtime or file order is deterministic and
// unrelated to which correction the user meant, and picking one silently is the
// loss this store exists to prevent. So the survivor is a required argument: the
// store retires the losers the operator did not choose and keeps the body of the
// one they did, but it never decides which that is.
//
// Two operators resolving the same fork toward different survivors concurrently
// each supersede a different head, so both writes land and the record re-forks —
// contained at read exactly as two concurrent first writes are (ADR-0031). The
// supersession claim serializes the common case, two resolutions toward the same
// survivor: the second finds the survivor already claimed and is refused.
func (s *Store) Resolve(recordID, keepVersionID, origin string) (Record, error) {
	versions, err := s.Versions()
	if err != nil {
		return Record{}, err
	}
	heads := headsByRecord(versions)[recordID]
	if len(heads) <= 1 {
		// Not forked. Distinguish the two harmless cases so the message tells the
		// operator which one they hit rather than a bare "cannot resolve".
		if len(heads) == 1 {
			return Record{}, fmt.Errorf("memory record %q is not forked: it has one current version "+
				"%s, so there is nothing to resolve. Use Correct to change a healthy record",
				recordID, heads[0].VersionID)
		}
		return Record{}, fmt.Errorf("%w: %q, so there is no fork to resolve", ErrNotFound, recordID)
	}

	var keep Record
	found := false
	others := make([]string, 0, len(heads)-1)
	for _, h := range heads {
		if h.VersionID == keepVersionID {
			keep = h
			found = true
			continue
		}
		others = append(others, h.VersionID)
	}
	if !found {
		ids := make([]string, 0, len(heads))
		for _, h := range heads {
			ids = append(ids, h.VersionID)
		}
		sort.Strings(ids)
		return Record{}, fmt.Errorf("memory version %q is not one of the current versions of record "+
			"%q (%s): Resolve keeps a version that is actually competing and retires the others, so "+
			"the survivor must be one of the heads. A version that was already superseded is not a "+
			"candidate to keep", keepVersionID, recordID, strings.Join(ids, ", "))
	}
	sort.Strings(others)

	// The survivor's body, scope, kind, sensitivity, purpose, evidence class,
	// confidence, retention and deletion state are carried forward verbatim:
	// Resolve chooses which version wins, not what it says. Keeping a tombstone is
	// allowed -- an operator may resolve a fork by deciding the record is deleted --
	// so Deleted is copied rather than forced false.
	return s.Put(Record{RecordID: recordID, Supersedes: keep.VersionID, Retires: others,
		Scope: keep.Scope, Kind: keep.Kind, Sensitivity: keep.Sensitivity, Purpose: keep.Purpose,
		EvidenceClass: keep.EvidenceClass, Confidence: keep.Confidence, Retention: keep.Retention, Body: keep.Body, Deleted: keep.Deleted, Origin: origin})
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
//
// Two shapes, told apart by whether the competing versions share a predecessor.
// A supersession fork names the version they split from; a root fork has none to
// name, so it says so rather than printing an empty predecessor as though a
// version were missing.
func (f Fork) err() error {
	if f.Predecessor != "" {
		return fmt.Errorf("memory record %q has a forked supersession chain: versions %s both "+
			"supersede %s, so two versions are current and no rule here can say which correction "+
			"the user intended. Inspect the competing versions and call Resolve(%q, <the version "+
			"to keep>, origin) to retire the rest; a tiebreak on file order, digest or mtime would "+
			"be deterministic and unrelated to what was meant",
			f.RecordID, strings.Join(f.Successors, " and "), f.Predecessor, f.RecordID)
	}
	return fmt.Errorf("memory record %q has %d current versions (%s) that supersede nothing, so it "+
		"forked at its root: more than one first version was created for the record without one "+
		"superseding the other, and no rule here can say which is current. This is what two "+
		"Approve or Propose calls for one record produce. Inspect the competing versions and call "+
		"Resolve(%q, <the version to keep>, origin) to retire the rest; superseding one by hand "+
		"cannot heal it, because a one-parent supersession leaves the number of current versions "+
		"unchanged",
		f.RecordID, len(f.Successors), strings.Join(f.Successors, " and "), f.RecordID)
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

// headsByRecord groups the current versions of each record.
//
// A version is current — a head — when nothing supersedes it and no resolution
// retired it. The two conditions are separate because they close different
// gaps. `Supersedes` is the one-parent correction edge; `Retires` is the
// multi-head edge a fork resolution writes, and it exists because a one-parent
// supersession can never reduce a record's head count: it turns one head into a
// non-head and adds a new head, net zero, so no chain of Corrects heals a fork.
// Both tips (for detection) and Resolve (for the heads it must retire) read the
// heads from here, so the definition of "current" lives in one place rather
// than in two predicates that could disagree about whether a retired version
// still counts.
func headsByRecord(versions []Record) map[string][]Record {
	recordOf := make(map[string]string, len(versions))
	for _, v := range versions {
		recordOf[v.VersionID] = v.RecordID
	}
	superseded := make(map[string]bool, len(versions))
	retired := make(map[string]bool)
	for _, v := range versions {
		// An edge is honored only within one record (ADR-0033). A supersede or
		// retire whose target belongs to a different record is ignored, so an
		// imported, replicated or corrupt version cannot remove another record's
		// head -- and cannot reach across a scope boundary to do it. Without this
		// check a version of one record naming another record's head in Supersedes
		// or Retires silently dropped that head, leaving the victim with zero
		// current versions: unreadable, with no fork and no tombstone, which is
		// exactly the silent loss ADR-0031 and ADR-0028 name. The target keeps its
		// head; the version carrying the foreign edge stays a head of its own
		// record, where a second head is reported as a fork rather than swallowed.
		for _, r := range v.Retires {
			if recordOf[r] == v.RecordID {
				retired[r] = true
			}
		}
		if v.Supersedes != "" && recordOf[v.Supersedes] == v.RecordID {
			superseded[v.Supersedes] = true
		}
	}
	out := make(map[string][]Record)
	for _, v := range versions {
		if !superseded[v.VersionID] && !retired[v.VersionID] {
			out[v.RecordID] = append(out[v.RecordID], v)
		}
	}
	return out
}

// tips reduces a set of versions to the current version of each healthy record,
// and separately reports the records it could not resolve.
//
// A version is current when nothing supersedes it and no resolution retired it.
// Derived from the chain on every read rather than tracked by a flag, for the
// reason the package comment gives: a flag is a cache, and a cache that
// disagrees with the chain would let a superseded version be presented while the
// correction sat on disk — the exact failure the exit evidence names.
//
// # The invariant is one current version per record, not one successor per predecessor
//
// A record is forked when it has more than one current version, and that is the
// whole test — regardless of how the second one arose. ADR-0028 detected the
// case it had seen, two versions superseding the same predecessor, by counting
// successors per predecessor. A probe found the case it had not: two *root*
// versions of one record, each with an empty `Supersedes`, share no predecessor,
// so the successor count never rose above one and both were reported current.
// Two `Approve` calls for the same record with different bodies, or an `Approve`
// of a record that already had a `Propose`, produced two live versions that
// Retrieve returned together and `tip` chose between silently — the silent loss
// this store exists to prevent, reached through the gap in the fork test rather
// than through a race the claim mechanism covers.
//
// So the detection is phrased against the invariant directly: group the current
// versions by record, and any record holding two or more is forked. This
// subsumes the multi-successor case — two successors of one predecessor are both
// current — and catches the multi-root case the successor count could not see.
//
// # A fork is contained to its own record, not raised for the whole store
//
// Refused rather than resolved by a tiebreak (ADR-0027): every available
// tiebreak — file order, digest order, mtime — would pick a winner
// deterministically while having no relationship to which version the user
// meant. Contained per record rather than for the whole store (ADR-0028): the
// two return values let a caller refuse the one record without refusing the
// store, and name which record is affected instead of leaking version IDs across
// a scope boundary in an error string. Resolving it is an operator act (ADR-0032)
// rather than a rule here, for the same reason: which version to keep is a fact
// the store does not hold.
func tips(versions []Record) ([]Record, map[string]Fork) {
	currentByRecord := headsByRecord(versions)

	forked := make(map[string]Fork)
	var out []Record
	for recordID, current := range currentByRecord {
		if len(current) == 1 {
			out = append(out, current[0])
			continue
		}
		ids := make([]string, 0, len(current))
		for _, c := range current {
			ids = append(ids, c.VersionID)
		}
		sort.Strings(ids)
		forked[recordID] = Fork{RecordID: recordID, Predecessor: sharedPredecessor(current), Successors: ids}
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

// sharedPredecessor returns the one version every current version supersedes, or
// empty when they do not agree on one. A supersession fork has it — both
// successors name the same predecessor — and the message can point at the
// version the correction split from. A root fork does not: the competing
// versions supersede nothing, so there is no predecessor to name, and the empty
// string is what tells Fork.err which story to tell.
func sharedPredecessor(current []Record) string {
	pred := ""
	for _, c := range current {
		if c.Supersedes == "" {
			return ""
		}
		if pred == "" {
			pred = c.Supersedes
		} else if pred != c.Supersedes {
			return ""
		}
	}
	return pred
}
