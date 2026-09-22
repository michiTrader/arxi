package memory

import "fmt"

// Kind is the authority class of a piece of memory: what kind of thing it is,
// and therefore whether it may be presented to a model and whether it is a
// governed stored record. ADR-0023 decided authority is enumerated, not
// inferred -- a kind outside this set is not authority, it is invalid -- and
// ADR-0031 moves the enumeration here, to the pure leaf, so the stored record
// and the presentation receipt read one vocabulary rather than each keeping its
// own. Two enumerations would drift, which is the exact failure ADR-0023 names.
type Kind string

const (
	// KindFrozenContextMemory is the static prose an operator froze on a
	// blueprint (Phase 5). It is presentable but not a governed record: a
	// configuration field has no record identity to correct, so it lives on the
	// receipt, never in the store.
	KindFrozenContextMemory Kind = "frozen_context_memory"
	// KindApprovedMemoryRecord is a stored record supplied or explicitly approved
	// by a user, operator or import. It is governed and presentable.
	KindApprovedMemoryRecord Kind = "approved_memory_record"
	// KindProposedMemoryCandidate is model-generated material. It is a governed
	// stored record -- it may be inspected and promoted -- but it is never
	// presented: the roadmap requires that model material "may propose candidates
	// but cannot create active memory", and a candidate that can be presented is
	// not a candidate.
	KindProposedMemoryCandidate Kind = "proposed_memory_candidate"
)

// kindPresentable maps every enumerated kind to whether it may be presented to a
// model. A map rather than a switch so the unknown-kind refusal and the
// presentable answer come from one table: two lists would let a kind be known
// and un-presentable by omission rather than by decision.
var kindPresentable = map[Kind]bool{
	KindFrozenContextMemory:     true,
	KindApprovedMemoryRecord:    true,
	KindProposedMemoryCandidate: false,
}

// Known reports whether the kind is enumerated at all. It is the fail-closed
// gate: material nobody enumerated is not authority, so an unknown kind is
// refused rather than trusted (ADR-0023).
func (k Kind) Known() bool {
	_, known := kindPresentable[k]
	return known
}

// Governed reports whether the kind is a stored governed record rather than
// frozen configuration prose. Governed records must name their version
// (ADR-0021).
//
// Derived from the enumeration, not from `!= KindFrozenContextMemory`: the
// negation form was a blocklist with one entry, so a typo'd kind reported
// governed and validated as authority. An unknown kind is not governed.
func (k Kind) Governed() bool {
	return k.Known() && k != KindFrozenContextMemory
}

// Presentable reports whether a memory of this kind may appear in a prepared
// context. False for an unknown kind, which is the inverted failure direction
// ADR-0023 decided: material nobody enumerated gets no authority rather than
// authority by default.
func (k Kind) Presentable() bool {
	return kindPresentable[k]
}

// Validate refuses a kind that is not enumerated. The presentation-specific rule
// -- that a candidate must never be presented -- stays with the receipt, because
// a candidate is a valid stored kind and only an invalid presentation; the store
// layer refuses only an unknown kind.
func (k Kind) Validate() error {
	if k == "" {
		return fmt.Errorf("memory kind is empty: a record must say what kind of authority it carries")
	}
	if !k.Known() {
		return fmt.Errorf("memory kind %q is not enumerated: authority is enumerated, so an "+
			"unrecognized kind fails closed rather than inheriting the authority of a record "+
			"somebody approved", string(k))
	}
	return nil
}
