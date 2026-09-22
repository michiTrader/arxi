// Package memory holds the pure contract for governed memory records: the
// vocabulary a Phase 7 store, its retrieval and its deletion lineage all build
// on, with no store, clock, network or filesystem of its own.
//
// It is a pure leaf in the sense internal/job and internal/workspace are: it may
// represent instants with time.Time and parse them, but it must never acquire an
// instant or perform I/O. Every judgment it makes is a function of values handed
// in, so the same record yields the same answer during a live run and during
// replay. internal/arch_test.go pins that, for the reason it pins it for
// coordination and workspaces.
package memory

import (
	"fmt"
	"time"
)

// instantLayout is the one representation an instant takes on the wire, matching
// the RFC3339Nano strings the rest of the system already stores (event Ts,
// authorization ExpiresAt). A record's times are data carried in events, not a
// time.Time a store minted, so the field is a string and this package only
// parses it.
const instantLayout = time.RFC3339Nano

// Validity is the bitemporal validity of one governed memory version.
//
// Two independent time axes, because one timestamp conflates two different
// facts and Phase 7's exit evidence needs them apart. Valid time answers "when
// does the world this record describes hold" -- a person's address is true from
// the day they moved, regardless of when we were told. Decision time answers
// "when did the store believe this version" -- it opens when the version is
// recorded and closes when a correction supersedes it. A correction that
// retracts a belief we recorded in error is a decision-time event; an address
// that genuinely changed is a valid-time event. Collapsing them makes
// "contradictions preserve temporal history" and "stale versions stop appearing
// after correction" unprovable, because there is no axis on which the old
// version both stops being current and stays historically visible.
//
// Both axes are half-open intervals [from, to): the successor's `from` equals
// the predecessor's `to`, so adjacent versions tile the timeline with neither a
// gap nor an overlap. Closed-closed intervals would make the switchover instant
// belong to two versions at once, and a query at exactly that instant would see
// both the superseded record and the one that replaced it -- which is the
// leakage the deletion-lineage exit evidence forbids.
type Validity struct {
	// ValidFrom is the instant the described fact begins to hold in the world.
	// Required: a fact with no beginning cannot be placed on the valid-time axis
	// at all, so it could never be excluded by an as-of query.
	ValidFrom string `json:"valid_from"`
	// ValidTo is the instant the fact stops holding, exclusive. Empty means the
	// fact is open-ended -- still true with no known end -- which is distinct
	// from a fact whose end is recorded.
	ValidTo string `json:"valid_to,omitempty"`

	// RecordedAt is the decision-time instant this version entered the store.
	// Required and immutable: it is the lower bound of the belief interval, and a
	// version with no recorded instant cannot be ordered against the correction
	// that supersedes it.
	RecordedAt string `json:"recorded_at"`
	// SupersededAt is the decision-time instant this version stopped being the
	// current belief, exclusive. Empty means it is still current. It is set once
	// and never cleared: a correction appends a new version and closes this one's
	// belief interval, and the fact bytes of a superseded version are never
	// mutated. That append-only rule is what lets a historical as-of query
	// reproduce exactly what was believed then, the same reason ADR-0002 makes
	// the log the truth rather than a mutable snapshot.
	SupersededAt string `json:"superseded_at,omitempty"`
}

// Validate refuses a validity whose instants do not parse or whose intervals
// run backwards.
//
// It does not invent bounds or reorder them: an ill-formed validity is a defect
// in whatever produced the record, and silently repairing it would let a store
// commit a version whose temporal position nobody can trust. The messages name
// the consequence because these are the invariants every as-of query below
// assumes without re-checking.
func (v Validity) Validate() error {
	validFrom, err := parseRequired("valid_from", v.ValidFrom)
	if err != nil {
		return err
	}
	recordedAt, err := parseRequired("recorded_at", v.RecordedAt)
	if err != nil {
		return err
	}
	if v.ValidTo != "" {
		validTo, err := parseInstant("valid_to", v.ValidTo)
		if err != nil {
			return err
		}
		if validTo.Before(validFrom) {
			return fmt.Errorf("memory validity has valid_to %q before valid_from %q: a fact cannot "+
				"stop holding before it starts, and a backwards valid interval would make every "+
				"world-time query answer as if the fact never held", v.ValidTo, v.ValidFrom)
		}
	}
	if v.SupersededAt != "" {
		supersededAt, err := parseInstant("superseded_at", v.SupersededAt)
		if err != nil {
			return err
		}
		if supersededAt.Before(recordedAt) {
			return fmt.Errorf("memory validity has superseded_at %q before recorded_at %q: a version "+
				"cannot stop being believed before it was recorded, and a backwards belief interval "+
				"would hide the version from the very as-of query meant to prove it was once current",
				v.SupersededAt, v.RecordedAt)
		}
	}
	return nil
}

// Current reports whether this version is the store's present belief: its belief
// interval is still open. This is the cheap gate a retrieval runs before the
// full bitemporal query, and it is exactly "has this been superseded" phrased so
// that an empty SupersededAt -- the common case -- needs no parsing.
func (v Validity) Current() bool { return v.SupersededAt == "" }

// LiveAt reports whether this version is the belief held as-of decisionAsOf
// about the world at validAt: the decision instant lies in the half-open belief
// interval [RecordedAt, SupersededAt) and the world instant lies in the
// half-open valid interval [ValidFrom, ValidTo).
//
// This is where correction propagation is actually decided. A version corrected
// at T has its belief interval closed at T, so a query as-of any instant at or
// after T no longer selects it -- the stale version stops appearing -- while a
// query as-of an instant before T still selects it, so the history of what was
// believed is preserved rather than rewritten. Both query instants are required
// and must parse: an as-of query with no `as-of` is not a bitemporal query, and
// answering it by defaulting to now would read the wall clock this package is
// forbidden to touch.
func (v Validity) LiveAt(decisionAsOf, validAt string) (bool, error) {
	if err := v.Validate(); err != nil {
		return false, fmt.Errorf("cannot place an invalid validity on either axis: %w", err)
	}
	decision, err := parseRequired("decision_as_of", decisionAsOf)
	if err != nil {
		return false, err
	}
	world, err := parseRequired("valid_at", validAt)
	if err != nil {
		return false, err
	}
	return v.believedAt(decision) && v.holdsAt(world), nil
}

// believedAt answers the decision axis: is decision in [RecordedAt, SupersededAt)?
// The caller has already validated, so the bounds parse.
func (v Validity) believedAt(decision time.Time) bool {
	recordedAt, _ := parseInstant("recorded_at", v.RecordedAt)
	if decision.Before(recordedAt) {
		return false
	}
	if v.SupersededAt == "" {
		return true
	}
	supersededAt, _ := parseInstant("superseded_at", v.SupersededAt)
	return decision.Before(supersededAt)
}

// holdsAt answers the valid axis: is world in [ValidFrom, ValidTo)?
func (v Validity) holdsAt(world time.Time) bool {
	validFrom, _ := parseInstant("valid_from", v.ValidFrom)
	if world.Before(validFrom) {
		return false
	}
	if v.ValidTo == "" {
		return true
	}
	validTo, _ := parseInstant("valid_to", v.ValidTo)
	return world.Before(validTo)
}

func parseRequired(field, value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, fmt.Errorf("memory validity has no %s: an instant that is absent cannot be "+
			"ordered against any other, so a version carrying it can neither be excluded by an as-of "+
			"query nor superseded by a correction", field)
	}
	return parseInstant(field, value)
}

func parseInstant(field, value string) (time.Time, error) {
	t, err := time.Parse(instantLayout, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("memory validity %s %q is not an RFC3339 instant: %w: an "+
			"unparseable instant compares equal to nothing, so it would silently fall outside every "+
			"as-of window instead of failing where it was written", field, value, err)
	}
	return t, nil
}
