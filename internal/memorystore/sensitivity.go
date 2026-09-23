package memorystore

import (
	"fmt"
	"sort"
	"strings"
)

// Sensitivity is the classification dimension retrieval authorizes on, beside
// the scope principal.
//
// A named type rather than a bare string so the vocabulary is a closed set the
// compiler participates in, the same shape as Principal and for the same reason
// ADR-0022 gives: two lists of levels drift, and a level valid in one place and
// unknown in another leaks by omission rather than by decision. It is added now,
// with the store that authorizes on it, and not before -- a classification with
// no check that fails on it is the field nothing fails on that ADR-0042 exists
// to avoid.
type Sensitivity string

// The canonical sensitivity vocabulary, least to most restrictive. ADR-0042
// fixed these four. A record is retrieved only when its level is at or below the
// caller's clearance, so the ordering is authorization, not decoration.
const (
	// Public needs no clearance: material safe to present to any authorized
	// caller. It is the clearance floor, so a query naming no clearance sees
	// exactly this level and nothing above it.
	Public Sensitivity = "public"
	// Internal is ordinary material within a holder, presentable to a caller
	// cleared for the holder but not to be treated as shareable.
	Internal Sensitivity = "internal"
	// Confidential is restricted material that a caller must be explicitly
	// cleared for.
	Confidential Sensitivity = "confidential"
	// Secret is the most restricted material, presented only under the top
	// clearance and withheld from everything below it.
	Secret Sensitivity = "secret"
)

// sensitivities enumerates the vocabulary once, as a map, for the same reason
// principals is a map: two lists would let a level be valid in one place and
// unknown in the other by omission rather than by decision.
//
// The value is the restriction rank. A higher rank is more restrictive, so a
// record is admitted when its rank does not exceed the clearance's rank. Public
// is the floor at 0 because it is the level that needs no clearance at all.
var sensitivities = map[Sensitivity]int{
	Public:       0,
	Internal:     1,
	Confidential: 2,
	Secret:       3,
}

// Rank reports the restriction rank of the level, and whether it is part of the
// vocabulary at all.
//
// An unknown level reports false rather than a default rank, the same
// fail-closed direction Principal.Specificity takes. A rank invented for an
// unrecognized level would sort it against real ones: given to a record it would
// let unclassified material be retrieved, and given to a clearance it would admit
// levels the caller was never cleared for. Material nobody classified gets no
// standing, not the standing of whatever it sorts beside.
func (s Sensitivity) Rank() (int, bool) {
	rank, known := sensitivities[s]
	return rank, known
}

// Validate refuses a record sensitivity that cannot authorize anything.
//
// The empty case is refused rather than defaulted, and that is the load-bearing
// choice of ADR-0042. An unclassified record's sensitivity is unknown, and
// treating unknown as Public is fail-open: the most sensitive material is the
// most likely to be written in a hurry, and the hurry is when the level is
// forgotten. Refusing it at the write names the mistake where the caller can fix
// it, instead of shipping a record that either discloses too much or vanishes.
func (s Sensitivity) Validate() error {
	if s == "" {
		return fmt.Errorf("memory record has no sensitivity: an unclassified record's level is "+
			"unknown, and treating unknown as %q would disclose material that was never cleared -- "+
			"classify it as one of %s", Public, sensitivityVocabulary())
	}
	if _, known := sensitivities[s]; !known {
		return fmt.Errorf("memory record sensitivity %q is not one of %s: the vocabulary is closed, "+
			"so an unrecognized level gets no standing rather than the standing of whatever it sorts "+
			"beside", s, sensitivityVocabulary())
	}
	return nil
}

// admits reports whether a caller holding this clearance may receive a record at
// the given level.
//
// The receiver is the clearance and the argument is the record. An empty
// clearance is the Public floor: a caller that named no clearance receives only
// material that needs none, which is the fail-closed default -- forgetting to
// state a clearance discloses the minimum, not the maximum. A record whose level
// is not in the vocabulary is never admitted, so a file that somehow carries a
// garbage level is withheld rather than compared against a default rank.
func (clearance Sensitivity) admits(record Sensitivity) bool {
	clearanceRank := 0
	if clearance != "" {
		rank, known := clearance.Rank()
		if !known {
			return false
		}
		clearanceRank = rank
	}
	recordRank, known := record.Rank()
	if !known {
		return false
	}
	return recordRank <= clearanceRank
}

// sensitivityVocabulary renders the closed set in restriction order for error
// messages. Sorted by rank rather than alphabetically so the message also teaches
// the least-to-most-restrictive ordering that clearance is checked against.
func sensitivityVocabulary() string {
	levels := make([]Sensitivity, 0, len(sensitivities))
	for s := range sensitivities {
		levels = append(levels, s)
	}
	sort.Slice(levels, func(i, j int) bool { return sensitivities[levels[i]] < sensitivities[levels[j]] })
	parts := make([]string, 0, len(levels))
	for _, l := range levels {
		parts = append(parts, string(l))
	}
	return strings.Join(parts, ", ")
}
