package memorystore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/michiTrader/arxi/internal/contextprep"
)

// Schema names the persisted record format. Versioned from the first commit
// because a record outlives the binary that wrote it: this is the one store in
// the project whose whole purpose is to be read by a later run.
const Schema = "arxi.memory-record/v1"

// Kind mirrors the authority enumeration contextprep already owns, rather than
// redeclaring it.
//
// Importing the constants is the decision. A parallel enumeration here would be
// the defect ADR-0023 was written about, one layer up: two lists of authority
// kinds drift, and the drift shows up as a record this store considers approved
// and the preparer refuses — or worse, the reverse. contextprep is the package
// that enforces presentability at the barrier, so it owns the vocabulary and
// this store spends it.
const (
	Approved  = contextprep.KindApprovedMemoryRecord
	Candidate = contextprep.KindProposedMemoryCandidate
)

// Record is one immutable version of one governed memory record.
//
// # Versions are values, not mutations
//
// ADR-0021 decided a receipt names the record version it presented, so that a
// correction has something to supersede. That is only true if a version is
// immutable once written: if editing a record rewrote its body in place, a
// receipt naming version 2 would describe whatever version 2 says *now*, and
// correction propagation — the thing Phase 7's exit evidence requires — would be
// unverifiable against it. So `Correct` appends a new version and supersedes the
// old one; nothing here is ever edited.
//
// # Provenance is recorded, never inferred
//
// Phase 7 opens with a containment rule: model material "may propose candidates
// but cannot create active memory". `Kind` is how that is represented, and it is
// set by the caller that has the authority to know — a user approval, an
// operator import, or a model proposal — never derived from the content. Content
// cannot be evidence of its own provenance, which is the whole argument of
// ADR-0020: memory is data, and data does not get to declare its own authority.
type Record struct {
	Schema string `json:"schema"`
	// RecordID is the stable identity across every version of this record.
	RecordID string `json:"record_id"`
	// VersionID is the immutable identity of this particular version. It is
	// content-addressed, so two stores given the same writes produce the same
	// version IDs and a receipt is portable between them.
	VersionID string `json:"version_id"`
	// Supersedes names the version this one corrects, empty for a first
	// version. It makes the correction chain walkable from either end, which
	// is what lets retrieval prove it returned a tip rather than trusting a
	// flag on the record.
	Supersedes string `json:"supersedes,omitempty"`
	// Retires names the other current versions this one supersedes at once, and
	// it exists for exactly one operation: resolving a fork. Supersession is
	// one-parent by design, and a one-parent append can never reduce a record's
	// current-version count -- it turns one head into a non-head and adds a new
	// head, a net change of zero. So no chain of Corrects can bring a forked
	// record back to a single current version, which is why Resolve names the
	// losing heads here and tips() treats a retired version as not current.
	// Empty for every write that is not a fork resolution, so it never touches
	// the identity of an ordinary version.
	Retires []string `json:"retires,omitempty"`

	Scope Scope  `json:"scope"`
	Kind  string `json:"kind"`
	// Sensitivity is the classification retrieval authorizes on beside the
	// scope. It is required (ADR-0042): an unclassified record's level is
	// unknown, and treating unknown as public is the fail-open direction. Set
	// once at creation and carried forward by every correction, like the scope,
	// because a correction that dropped it would silently declassify the record.
	Sensitivity Sensitivity `json:"sensitivity"`
	// Purpose is the use the record is approved for, the third dimension
	// retrieval authorizes on beside the scope and the sensitivity. It is
	// required (ADR-0043): an unstated use is unknown, and treating unknown as
	// any purpose would surface the record for every use -- purpose creep by
	// construction. Set once at creation and carried forward by every
	// correction, like the scope and the sensitivity, because a correction that
	// dropped it would silently re-purpose the record.
	Purpose Purpose `json:"purpose"`
	// EvidenceClass is the kind of evidence backing the record, the fourth
	// dimension retrieval authorizes on beside the scope, the sensitivity and the
	// purpose. It is required (ADR-0044): unstated evidence is unknown, and
	// treating unknown as any class would surface an inference wherever an
	// assertion was asked for. Set once at creation and carried forward by every
	// correction, like the other three, because a correction that dropped it would
	// silently reclassify what the record is evidence of.
	EvidenceClass EvidenceClass `json:"evidence_class"`
	// Confidence is how far the writer vouches for the record, the first dimension
	// that feeds ranking rather than authorization (ADR-0045). It is required: an
	// unstated confidence is no assessment, and defaulting it would launder a
	// missing judgment into a stated one. Set once at creation and carried forward
	// by every correction, like the four authorization dimensions, because a
	// correction that dropped it would silently restate how much to trust the
	// record -- and unlike them it never withholds a record, it only orders it.
	Confidence Confidence `json:"confidence"`
	// Retention is the lifecycle policy the record is kept under, the first
	// dimension that feeds neither authorization nor ranking but expiry
	// (ADR-0046). It is required: an unstated retention is no lifecycle decision,
	// and defaulting it would either hoard a record meant to be transient or expire
	// one meant to be kept. Set once at creation and carried forward by every
	// correction, like the five dimensions above, because a correction that dropped
	// it would silently re-tier the record -- and unlike the authorization
	// dimensions it never withholds a record and unlike confidence it never orders
	// one; it only decides whether an automated sweep may expire it.
	Retention Retention `json:"retention"`
	Body      string    `json:"body"`

	// Origin records who or what produced this version: an authenticated
	// principal, an import name, or the run that proposed it. The exit
	// evidence requires that "every influence identifies its source", and the
	// scope says which holder the record belongs to, not who wrote it.
	Origin string `json:"origin"`
	// CreatedSeq and CreatedRun bind a version to the execution that produced
	// it when there was one. Empty for user and operator writes, which happen
	// outside any run; a synthetic run ID for those would be indistinguishable
	// downstream from a real one, the same argument ADR-0021 makes for leaving
	// RecordID empty on frozen configuration memory.
	CreatedRun string `json:"created_run,omitempty"`
	CreatedSeq int64  `json:"created_seq,omitempty"`

	// Deleted marks a tombstone. Deletion is a version like any other, for
	// the reason the phase's exit evidence gives: deletion must propagate
	// "without resurrection", and a record removed by truncating a file can be
	// resurrected by any replica or backup that still holds the old bytes.
	// A tombstone is a positive assertion that travels with the data.
	Deleted bool `json:"deleted,omitempty"`

	ContentDigest string `json:"content_digest"`
}

// identity is the subset of a record that determines its version ID. Declared
// as its own type so the digest cannot silently change meaning when a field is
// added to Record: a new field is not part of the identity until someone adds
// it here on purpose, and an accidentally-included field would change every
// existing version ID, invalidating every receipt already committed.
type identity struct {
	RecordID      string        `json:"record_id"`
	Supersedes    string        `json:"supersedes,omitempty"`
	Retires       []string      `json:"retires,omitempty"`
	Scope         Scope         `json:"scope"`
	Kind          string        `json:"kind"`
	Sensitivity   Sensitivity   `json:"sensitivity"`
	Purpose       Purpose       `json:"purpose"`
	EvidenceClass EvidenceClass `json:"evidence_class"`
	// Confidence is part of the identity for the same reason the four authorization
	// dimensions are (ADR-0034): a re-rating is a new version a receipt can name,
	// not an in-place edit that would silently change how a version already cited
	// was ranked. It never authorizes, but it is still identity -- two records
	// alike in everything but confidence are two different assertions about trust.
	Confidence Confidence `json:"confidence"`
	// Retention is part of the identity for the same reason the dimensions above
	// are (ADR-0034): a re-tiering is a new version a receipt can name, not an
	// in-place edit. It matters more here than elsewhere, because if retention
	// could be edited in place a permanent record a receipt named could be flipped
	// to ephemeral and tombstoned by the next sweep, expiring a version somebody
	// was told would be kept. Making it identity forces a re-tier to be a visible
	// new version.
	Retention  Retention `json:"retention"`
	Body       string    `json:"body"`
	Origin     string    `json:"origin"`
	CreatedRun string    `json:"created_run,omitempty"`
	CreatedSeq int64     `json:"created_seq,omitempty"`
	Deleted    bool      `json:"deleted,omitempty"`
}

// Seal computes the content digest and the content-addressed version ID.
//
// The version ID is derived from the content rather than assigned from a
// counter, and that is a durability decision rather than an aesthetic one. A
// counter needs a writer with exclusive state to allocate from, so two
// processes writing concurrently either serialize on a lock or mint colliding
// IDs; the store's whole point is to be read and written by many runs. A
// content-addressed ID also makes a correction that restates an earlier body
// resolve to that earlier version, which is honest: it is the same assertion.
func (r Record) Seal() (Record, error) {
	body, err := json.Marshal(identity{
		RecordID: r.RecordID, Supersedes: r.Supersedes, Retires: r.Retires, Scope: r.Scope,
		Kind: r.Kind, Sensitivity: r.Sensitivity, Purpose: r.Purpose, EvidenceClass: r.EvidenceClass,
		Confidence: r.Confidence, Retention: r.Retention, Body: r.Body, Origin: r.Origin, CreatedRun: r.CreatedRun, CreatedSeq: r.CreatedSeq, Deleted: r.Deleted,
	})
	if err != nil {
		return Record{}, fmt.Errorf("encode memory record identity: %w", err)
	}
	out := r
	out.Schema = Schema
	out.ContentDigest = digest("arxi.memory-record/v1", body)
	out.VersionID = "mv-" + out.ContentDigest[:24]
	return out, nil
}

// Validate refuses a record that cannot be retrieved safely.
//
// It runs before a write reaches the disk, not after. A record that fails these
// checks on the way out would already be persisted, and every reader would have
// to defend against it forever; ADR-0024 made the same choice for receipts, on
// the same reasoning that a refused artifact must not reach the barrier at all.
func (r Record) Validate() error {
	if err := r.Scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(r.RecordID) == "" {
		return fmt.Errorf("memory record has no record_id: without a stable identity across " +
			"versions, a correction has nothing to attach to and every write is a new record")
	}
	switch r.Kind {
	case Approved, Candidate:
	case contextprep.KindFrozenContextMemory:
		return fmt.Errorf("memory record has kind %q: frozen configuration memory is a field on "+
			"a blueprint and has no record identity, so storing one here would mint a version "+
			"ID that no correction could ever supersede", r.Kind)
	case "":
		return fmt.Errorf("memory record has no kind: authority is enumerated, and a record " +
			"that does not say what it is evidence of cannot be authorized")
	default:
		return fmt.Errorf("memory record has unknown kind %q: authority is enumerated, so an "+
			"unrecognized kind fails closed rather than inheriting the authority of a record "+
			"somebody approved", r.Kind)
	}
	// Sensitivity is validated for every record, tombstone included: a deletion
	// carries the tip's level forward, and a tombstone with no level would be a
	// record whose classification became unknown at the moment it was removed.
	if err := r.Sensitivity.Validate(); err != nil {
		return err
	}
	// Purpose is validated for every record, tombstone included, for the same
	// reason as the sensitivity: a deletion carries the tip's purpose forward,
	// and a tombstone with no purpose would be a record whose approved use became
	// unknown at the moment it was removed.
	if err := r.Purpose.Validate(); err != nil {
		return err
	}
	// Evidence class is validated for every record, tombstone included, for the
	// same reason as the sensitivity and the purpose: a deletion carries the tip's
	// class forward, and a tombstone with no class would be a record whose backing
	// evidence became unknown at the moment it was removed.
	if err := r.EvidenceClass.Validate(); err != nil {
		return err
	}
	// Confidence is validated for every record, tombstone included, for the same
	// reason as the four dimensions above: a deletion carries the tip's confidence
	// forward, and a tombstone with no confidence would be a record whose stated
	// trust became unknown at the moment it was removed. It is required here even
	// though it never authorizes, because a record that cannot be ranked honestly
	// is exactly the field-nothing-fails-on defect ADR-0045 refuses to add.
	if err := r.Confidence.Validate(); err != nil {
		return err
	}
	// Retention is validated for every record, tombstone included, for the same
	// reason as the five dimensions above: a deletion carries the tip's retention
	// forward, and a tombstone with no retention would be a record whose lifecycle
	// became unknown at the moment it was removed. It is required here even though
	// it neither authorizes nor ranks, because a record no sweep can decide whether
	// to expire is exactly the field-nothing-fails-on defect ADR-0046 refuses to
	// add.
	if err := r.Retention.Validate(); err != nil {
		return err
	}
	// A tombstone carries no body by design, so the body check is scoped to
	// live versions. Requiring a body on a deletion would force callers to
	// invent text that retrieval must then remember never to present.
	if !r.Deleted && strings.TrimSpace(r.Body) == "" {
		return fmt.Errorf("memory record %q has an empty body: a record that asserts nothing "+
			"would occupy the context window and the retrieval receipt without influencing "+
			"anything, which is cost with no evidence of benefit", r.RecordID)
	}
	if strings.TrimSpace(r.Origin) == "" {
		return fmt.Errorf("memory record %q has no origin: the exit evidence requires every "+
			"influence to identify its source, and the scope names the holder the record "+
			"belongs to rather than whoever wrote it", r.RecordID)
	}
	if r.CreatedSeq != 0 && r.CreatedRun == "" {
		return fmt.Errorf("memory record %q has created_seq %d and no created_run: a sequence "+
			"number is only meaningful within one log, so a seq without its run points at "+
			"every run and none of them", r.RecordID, r.CreatedSeq)
	}
	return nil
}

// Receipt renders the retrieval evidence for this version in the form the
// preparer already validates.
//
// Constructed here rather than at the call site because this is the first
// production code in the project able to produce a governed receipt at all: a
// probe found `KindApprovedMemoryRecord` with zero construction sites outside
// tests and `RecordID` assigned in production zero times. Building it beside
// the record means the record's own identity fields are what populate it, so
// the receipt cannot describe a version the store does not hold.
//
// EffectiveConfigSHA is filled by the preparer, not here. It identifies the
// blueprint that presented the memory, which is a property of the presentation
// and unknown to a store.
func (r Record) Receipt() contextprep.MemoryReceipt {
	return contextprep.MemoryReceipt{
		Kind:          r.Kind,
		ContentDigest: r.ContentDigest,
		RecordID:      r.RecordID,
		VersionID:     r.VersionID,
	}
}

// digest binds content the way every other persisted digest in this repository
// does: a domain-separated lowercase hex SHA-256. The domain prefix keeps a
// record digest from colliding with a transcript or presentation digest that
// happens to cover the same bytes.
func digest(domain string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(domain))
	h.Write([]byte{0})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}
