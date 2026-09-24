package memorystore

import (
	"fmt"
	"sort"
	"strings"
)

// EvidenceClass is the kind of evidence a record is backed by, and the fourth
// dimension retrieval authorizes on, beside the scope principal, the sensitivity
// level and the purpose.
//
// A named type rather than a bare string so the vocabulary is a closed set the
// compiler participates in, the same shape as Principal, Sensitivity and Purpose
// and for the same reason ADR-0022 gives: two lists of values drift, and a value
// valid in one place and unknown in another leaks by omission rather than by
// decision. It is added now, with the store that authorizes on it, and not
// before -- a class with no check that fails on it is the field nothing fails on
// that ADR-0044 exists to avoid.
//
// # Evidence class is a membership dimension, not a ranked one
//
// It is tempting to rank evidence by strength -- to call one class "stronger"
// than another and let a query authorize everything at or above a floor, the
// mirror of the ceiling sensitivity uses. The domain forbids it. The trust order
// of these classes is not fixed: for a user's stated preference, `stated` is
// authoritative and an `observed` inference is the weaker guess; for an external
// fact, `imported` from a system of record outranks whatever the user `stated`
// from memory. The order flips with the question asked, so no single rank is
// honest, and inventing one would let a caller authorized for the "stronger"
// class receive a record of a class it never accepted because it "sorts below" --
// a disclosure through an order the domain does not have, which is ADR-0023's and
// ADR-0043's failure direction. So authorization here is set membership, the
// shape scope and purpose already use: a set the query holds and a single value
// the record carries, and this type deliberately carries no rank.
type EvidenceClass string

// The canonical evidence-class vocabulary. ADR-0044 fixed these three. A record
// is authorized only when its class is among the query's accepted classes, so the
// set is authorization, not decoration.
const (
	// Stated is memory asserted directly by a user or an operator: a preference
	// they declared, a fact they gave. Its evidence is the assertion itself.
	Stated EvidenceClass = "stated"
	// Observed is memory derived from execution the run witnessed: a tool result,
	// a system event, a value read from the environment rather than declared.
	Observed EvidenceClass = "observed"
	// Imported is memory brought in from an external system of record during an
	// import: its evidence is that external source, not this system.
	Imported EvidenceClass = "imported"
)

// evidenceClasses enumerates the vocabulary once, as a set, for the same reason
// principals, sensitivities and purposes are maps: two lists would let a class be
// valid in one place and unknown in the other by omission rather than by
// decision.
//
// The value is a presence marker, not a rank, because evidence classes are
// incomparable: like purposes and unlike sensitivities this map carries no
// ordering, and authorization is membership in the query's set rather than a
// threshold. A ranked value here would be an order the domain does not have.
var evidenceClasses = map[EvidenceClass]bool{
	Stated:   true,
	Observed: true,
	Imported: true,
}

// Known reports whether the evidence class is part of the vocabulary at all.
//
// An unknown class reports false rather than being admitted, the same
// fail-closed direction Purpose.Known and Sensitivity.Rank take. A class nobody
// enumerated gets no standing: given to a record it would let unclassified
// evidence be retrieved, and given to a query it would accept evidence the caller
// never named. Material of a class nobody recognizes gets no standing, not the
// standing of whatever it is mistaken for.
func (e EvidenceClass) Known() bool {
	return evidenceClasses[e]
}

// Validate refuses a record evidence class that cannot authorize anything.
//
// The empty case is refused rather than defaulted, and that is the load-bearing
// choice of ADR-0044. The fail-open direction is the same one purpose has and
// starker than sensitivity's: there is no least-privilege member to reach for,
// because the members are incomparable, so a default could only pick one arbitrary
// class and admit the record to every query that accepts it. Treating an unstated
// class as "any evidence" would surface an inference wherever an assertion was
// asked for, which is the ungoverned influence Phase 7 exists to stop. Requiring
// the field means every writer states what backs the record; that cost is the
// decision, not a side effect of it. A class a caller may skip is the field
// nothing fails on.
func (e EvidenceClass) Validate() error {
	if e == "" {
		return fmt.Errorf("memory record has no evidence class: unstated evidence is unknown, and "+
			"treating unknown as any class would surface the record wherever any evidence is accepted "+
			"-- with no least-privilege member to fall back to, because the classes are incomparable "+
			"-- so record it as one of %s", evidenceClassVocabulary())
	}
	if !evidenceClasses[e] {
		return fmt.Errorf("memory record evidence class %q is not one of %s: the vocabulary is "+
			"closed, so an unrecognized class gets no standing rather than the standing of whatever "+
			"it is mistaken for", e, evidenceClassVocabulary())
	}
	return nil
}

// evidenceClassVocabulary renders the closed set for error messages. Sorted
// alphabetically rather than by rank, because evidence classes are unranked and
// any order the message implied would be an order the domain does not have.
func evidenceClassVocabulary() string {
	names := make([]string, 0, len(evidenceClasses))
	for e := range evidenceClasses {
		names = append(names, string(e))
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
