package memory

import "fmt"

// Version is one governed memory version: a record identity, an immutable
// version identity, and the bitemporal validity of that version.
//
// RecordID is stable across every version of the same fact; VersionID is unique
// to this one. The pair is the identity ADR-0021 added to the presentation
// receipt, here on the stored thing the receipt names a version of. This struct
// deliberately carries no authority kind, provenance or evidence class yet: the
// kind enumeration lives on the receipt in internal/contextprep, and pulling it
// into this pure leaf is a package move worth its own decision, not a side
// effect of settling lineage. Validity is the part settled by ADR-0028; this
// file settles how two versions of one record chain.
type Version struct {
	RecordID  string   `json:"record_id"`
	VersionID string   `json:"version_id"`
	Validity  Validity `json:"validity"`
}

// Validate refuses a version missing either identity or carrying an ill-formed
// validity. A version with no record identity cannot be joined to its lineage,
// and one with no version identity cannot be distinguished from the correction
// that supersedes it -- which is the exact guarantee ADR-0021 says a receipt
// owes Phase 7.
func (v Version) Validate() error {
	if v.RecordID == "" {
		return fmt.Errorf("memory version has no record_id: a version with no record identity cannot " +
			"be joined to the other versions of its fact, so no lineage can be formed and no " +
			"correction can find what it supersedes")
	}
	if v.VersionID == "" {
		return fmt.Errorf("memory version for record %q has no version_id: without it a correction "+
			"cannot name the version it replaces, which is the identity ADR-0021 requires", v.RecordID)
	}
	return nil
}

// Supersede corrects this version, returning the closed predecessor and the new
// current successor. The correction is an operation rather than two hand-built
// validities precisely because ADR-0028 asserted the tiling boundary on
// literals: here the predecessor's belief interval is closed at the same instant
// the successor's opens, by construction, so a caller cannot produce a gap or an
// overlap on the decision axis. The predecessor's fact bytes are untouched --
// only its SupersededAt is set -- because a correction is a new fact appended,
// not an edit of the old one (ADR-0002, ADR-0028).
//
// It refuses to supersede a version that is already superseded: that would fork
// the history into two chains claiming the same predecessor, and a lineage with
// a fork has no single answer to "what was believed after this". The instant of
// the correction must not precede the version being corrected, or the closed
// belief interval would run backwards.
func (v Version) Supersede(newVersionID, at, validFrom, validTo string) (Version, Version, error) {
	if err := v.Validate(); err != nil {
		return Version{}, Version{}, fmt.Errorf("cannot supersede an invalid version: %w", err)
	}
	if err := v.Validity.Validate(); err != nil {
		return Version{}, Version{}, fmt.Errorf("cannot supersede a version with invalid validity: %w", err)
	}
	if !v.Validity.Current() {
		return Version{}, Version{}, fmt.Errorf("memory version %q of record %q is already superseded at "+
			"%q: superseding it again would fork the lineage into two chains claiming the same "+
			"predecessor, and a fork has no single answer to what was believed next",
			v.VersionID, v.RecordID, v.Validity.SupersededAt)
	}
	if newVersionID == "" {
		return Version{}, Version{}, fmt.Errorf("supersede of record %q was given no new version_id: the "+
			"successor must be a distinct, nameable version or the correction cannot be told apart "+
			"from the version it replaces", v.RecordID)
	}
	if newVersionID == v.VersionID {
		return Version{}, Version{}, fmt.Errorf("supersede of record %q reuses version_id %q for the "+
			"successor: a version identity is immutable, so a correction is a new version, never the "+
			"same one re-pointed", v.RecordID, v.VersionID)
	}
	correctedAt, err := parseInstant("superseded_at", at)
	if err != nil {
		return Version{}, Version{}, err
	}
	recordedAt, _ := parseInstant("recorded_at", v.Validity.RecordedAt) // parsed clean above
	if correctedAt.Before(recordedAt) {
		return Version{}, Version{}, fmt.Errorf("supersede of version %q instant %q precedes its recorded_at "+
			"%q: a version cannot be corrected before it was recorded, and a backwards belief interval "+
			"would hide it from the query meant to prove it was once current",
			v.VersionID, at, v.Validity.RecordedAt)
	}

	closed := v
	closed.Validity.SupersededAt = at
	successor := Version{
		RecordID:  v.RecordID,
		VersionID: newVersionID,
		Validity:  Validity{ValidFrom: validFrom, ValidTo: validTo, RecordedAt: at},
	}
	if err := closed.Validity.Validate(); err != nil {
		return Version{}, Version{}, fmt.Errorf("closing the predecessor produced an invalid validity: %w", err)
	}
	if err := successor.Validate(); err != nil {
		return Version{}, Version{}, fmt.Errorf("the successor is not a valid version: %w", err)
	}
	if err := successor.Validity.Validate(); err != nil {
		return Version{}, Version{}, fmt.Errorf("the successor validity is invalid: %w", err)
	}
	return closed, successor, nil
}

// ValidateLineage refuses any ordered chain of versions of one record that is
// not an append-only, contiguous correction history.
//
// The chain is what makes "contradictions preserve temporal history" checkable:
// every version but the last is closed, each closed interval ends exactly where
// the next begins, and at most one version -- the last -- is current. A gap
// would leave an instant with no belief; an overlap would leave one with two; a
// non-last current version would be a fork. A lineage may end closed (a fully
// retracted fact with no successor), which is the shape a later deletion-lineage
// decision builds on -- this function pins the chain, not the meaning of a
// closed tail, because deletion semantics remain undecided (roadmap item 7).
//
// The slice is taken in recorded order rather than sorted: the caller holds the
// history, and re-sorting here would hide a chain that was stored out of order,
// which is itself a defect worth failing on.
func ValidateLineage(versions []Version) error {
	if len(versions) == 0 {
		return fmt.Errorf("memory lineage has no versions: an empty chain is not a history, and a " +
			"correction query over it can only answer vacuously")
	}
	recordID := versions[0].RecordID
	seen := make(map[string]bool, len(versions))
	for i, v := range versions {
		if err := v.Validate(); err != nil {
			return fmt.Errorf("version %d of %d is invalid: %w", i+1, len(versions), err)
		}
		if err := v.Validity.Validate(); err != nil {
			return fmt.Errorf("version %d of %d (%q) has invalid validity: %w", i+1, len(versions), v.VersionID, err)
		}
		if v.RecordID != recordID {
			return fmt.Errorf("version %d of %d belongs to record %q, not %q: a lineage is the history of "+
				"one fact, and mixing records into it would let a correction to one silently answer a "+
				"query about another", i+1, len(versions), v.RecordID, recordID)
		}
		if seen[v.VersionID] {
			return fmt.Errorf("version_id %q appears twice in the lineage of record %q: a version identity "+
				"is unique, and a repeated one makes the chain ambiguous about which correction came next",
				v.VersionID, recordID)
		}
		seen[v.VersionID] = true
	}
	for i := 0; i < len(versions)-1; i++ {
		cur, next := versions[i], versions[i+1]
		if cur.Validity.Current() {
			return fmt.Errorf("version %q of record %q is not the last in the lineage yet is still current: "+
				"a non-final current version is a fork, and the history then has two answers to what was "+
				"believed after it", cur.VersionID, recordID)
		}
		if cur.Validity.SupersededAt != next.Validity.RecordedAt {
			return fmt.Errorf("version %q is superseded at %q but its successor %q was recorded at %q: the "+
				"belief intervals must tile, so a gap leaves an instant with no belief and an overlap "+
				"leaves one with two", cur.VersionID, cur.Validity.SupersededAt, next.VersionID,
				next.Validity.RecordedAt)
		}
	}
	return nil
}
