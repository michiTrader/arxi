package memorystore

import (
	"fmt"
	"sort"
	"strings"
)

// Purpose is the use a record is approved for, and the third dimension
// retrieval authorizes on, beside the scope principal and the sensitivity level.
//
// A named type rather than a bare string so the vocabulary is a closed set the
// compiler participates in, the same shape as Principal and Sensitivity and for
// the same reason ADR-0022 gives: two lists of values drift, and a value valid
// in one place and unknown in another leaks by omission rather than by decision.
// It is added now, with the store that authorizes on it, and not before -- a use
// with no check that fails on it is the field nothing fails on that ADR-0043
// exists to avoid.
//
// # Purpose is a membership dimension, not a ranked one
//
// Sensitivity is ranked: a clearance admits every level at or below it, so its
// authorization is a ceiling. Purpose has no order. `operate` is neither more nor
// less restrictive than `recommend`; they are incomparable uses, and a record
// approved for one is simply not approved for the other. So authorization here is
// set membership -- the shape scope already uses, a set the query holds and a
// single value the record carries -- and this type deliberately carries no rank.
// Inventing one would let a caller authorized for one use receive a record
// approved only for another because it "sorts below", which is a disclosure for a
// use the record was never approved for (ADR-0043).
type Purpose string

// The canonical purpose vocabulary. ADR-0043 fixed these three. A record is
// authorized only when its purpose is among the query's authorized purposes, so
// the set is authorization, not decoration.
const (
	// Operate is memory used to carry out the caller's current task.
	Operate Purpose = "operate"
	// Personalize is memory used to tailor output to the holder's stated
	// preferences.
	Personalize Purpose = "personalize"
	// Recommend is memory used to proactively suggest content or actions the
	// caller did not ask for.
	Recommend Purpose = "recommend"
)

// purposes enumerates the vocabulary once, as a set, for the same reason
// principals and sensitivities are maps: two lists would let a purpose be valid
// in one place and unknown in the other by omission rather than by decision.
//
// The value is a presence marker, not a rank, because purposes are incomparable:
// unlike sensitivities this map carries no ordering, and authorization is
// membership in the query's set rather than a threshold. A ranked value here
// would be an order the domain does not have.
var purposes = map[Purpose]bool{
	Operate:     true,
	Personalize: true,
	Recommend:   true,
}

// Known reports whether the purpose is part of the vocabulary at all.
//
// An unknown purpose reports false rather than being admitted, the same
// fail-closed direction Principal.Specificity and Sensitivity.Rank take. A
// purpose nobody enumerated gets no standing: given to a record it would let an
// unclassified use be retrieved, and given to a query it would authorize a use
// the caller never named. Material nobody approved for a known use gets no
// standing, not the standing of whatever it is mistaken for.
func (p Purpose) Known() bool {
	return purposes[p]
}

// Validate refuses a record purpose that cannot authorize anything.
//
// The empty case is refused rather than defaulted, and that is the load-bearing
// choice of ADR-0043. The fail-open direction is starker than for sensitivity:
// sensitivity has a least-privilege member, public, that a misguided default
// could reach for, but purpose has none because its members are incomparable.
// Treating an unstated purpose as "any purpose" would surface the record for
// every use, which is purpose creep by construction. Refusing it at the write
// names the mistake where the caller can fix it.
func (p Purpose) Validate() error {
	if p == "" {
		return fmt.Errorf("memory record has no purpose: an unstated use is unknown, and treating "+
			"unknown as any purpose would surface the record for every use -- purpose creep by "+
			"construction, with no least-privilege member to fall back to -- so approve it for one "+
			"of %s", purposeVocabulary())
	}
	if !purposes[p] {
		return fmt.Errorf("memory record purpose %q is not one of %s: the vocabulary is closed, so "+
			"an unrecognized use gets no standing rather than the standing of whatever it is mistaken "+
			"for", p, purposeVocabulary())
	}
	return nil
}

// purposeVocabulary renders the closed set for error messages. Sorted
// alphabetically rather than by rank, because purposes are unranked and any
// order the message implied would be an order the domain does not have.
func purposeVocabulary() string {
	names := make([]string, 0, len(purposes))
	for p := range purposes {
		names = append(names, string(p))
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
