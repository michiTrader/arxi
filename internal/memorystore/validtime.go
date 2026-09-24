package memorystore

import (
	"fmt"
	"time"
)

// Instant is a point in valid time, written as a canonical RFC 3339 timestamp in
// UTC with seconds precision -- "2006-01-02T15:04:05Z". The empty Instant is the
// unbounded end of an interval, not a zero time: an empty From means "true since
// before any instant this store records", an empty To means "true with no known
// end".
//
// # Why a canonical string and not a time.Time
//
// The record's identity is content-addressed (ADR-0034): two stores given the same
// write must mint the same version ID, or a receipt stops being portable between
// them. A time.Time cannot carry that guarantee, and for the same reason ADR-0045
// rejected a float confidence -- its encoding is not canonical. The same instant
// marshals as "...Z" or "...+00:00", with or without a monotonic reading, in one
// location or another, so two writers recording the same moment would digest to
// different version IDs. One canonical spelling -- parsed, required to be UTC,
// required to re-render to itself -- digests to stable bytes; and because every
// stored instant is then the same fixed width in the same zone, it sorts
// chronologically under a plain string comparison, so the as-of filter orders
// instants without importing a clock at all.
//
// # Why this store may parse an instant but must never read one
//
// Parsing an instant the caller supplied is not reading a clock; calling
// time.Now() is. The store authorizes on valid time by comparing a record's
// interval against an as-of instant the caller passes in, exactly as the reducer
// receives the clock as an event rather than acquiring one. The clock read that an
// as-of query needs by nature happens in the caller -- the one move that lets a
// clock-free store answer a temporal question, and the reason valid time waited for
// its own record while the store learned through retention (ADR-0046) to treat
// timing as an input rather than a capability. time is used below only to reject a
// string that is not a real calendar instant, never to acquire one.
type Instant string

// canonical reports the error for an instant that is not the store's one spelling
// of a moment.
//
// The empty instant is the unbounded bound and is allowed. A non-empty instant must
// parse as RFC 3339 and must equal its own UTC re-rendering, which rejects three
// things at once: a real-but-non-UTC offset ("...+05:00"), a sub-second precision
// the digest could not round-trip, and a fixed-width string that is not a real date
// ("2026-13-40T..."). Refused rather than normalized, on the same reasoning
// Retrieve refuses an unknown clearance instead of narrowing it: silently rewriting
// "+00:00" to "Z" would let two spellings of one write look different on the way in
// and identical on disk, and the writer would never learn which form the store kept
// -- the closed-vocabulary rule applied to the one dimension whose values are not a
// closed set.
func (i Instant) canonical() error {
	if i == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, string(i))
	if err != nil {
		return fmt.Errorf("memory record valid-time instant %q is not an RFC 3339 timestamp: %w -- "+
			"state it as a UTC instant like 2026-01-02T15:04:05Z", i, err)
	}
	if canon := t.UTC().Format(time.RFC3339); canon != string(i) {
		return fmt.Errorf("memory record valid-time instant %q is not canonical: an instant is stored "+
			"as UTC at seconds precision so the same moment digests to one version ID on every store "+
			"(ADR-0034) -- state it as %q", i, canon)
	}
	return nil
}

// Validity is the interval in valid time during which a record's assertion is true
// in the modeled world, half-open as [From, To): From is included, To is excluded.
//
// # Valid time is the second time axis, and the store already had the first
//
// The version chain records transaction time -- when each version was written and
// when a correction superseded it -- which answers "what did the store believe, and
// since when". Valid time answers a question the chain cannot: "when is the asserted
// fact true in the world". The two are independent: a headcount true for all of 2025
// can be recorded in 2026 and corrected in 2027 without changing when it was true.
// Carrying both is what makes the store bitemporal, the last of Phase 7's record
// dimensions and the reason this dimension needed its own record rather than a
// struct field bolted onto the others.
//
// # Why half-open
//
// Adjacent intervals tile without overlap or gap: a fact true for 2025 is
// [2025-01-01T00:00:00Z, 2026-01-01T00:00:00Z), and the fact that replaces it can
// begin exactly at that upper bound with no instant falling in both windows or in
// neither. A closed interval would make the boundary instant belong to two versions
// at once, which is the ambiguity the as-of filter exists to remove.
//
// # An empty bound is a claim, not an omission
//
// Unlike the vocabulary dimensions, where an empty value is an unknown to be
// refused, an empty bound here is a stated claim: an empty To means "true with no
// known end", the honest shape of a standing preference. Requiring a To would force
// a writer to invent an expiry nobody knows -- the fabrication ADR-0045 and ADR-0046
// refuse in confidence and retention, here in time. So both bounds are optional and
// a record may be timeless (both empty), which the as-of filter reads as
// always-valid. The check that fails on valid time is therefore not "a bound is
// present" but the as-of filter withholding a bounded record outside its window.
type Validity struct {
	From Instant `json:"from,omitempty"`
	To   Instant `json:"to,omitempty"`
}

// Validate refuses a validity that cannot be evaluated or that no instant satisfies.
//
// A bound that is not a canonical instant is refused, so a garbage timestamp fails
// at the write rather than sorting against real ones at retrieval. An interval whose
// From is not strictly before its To is refused: [t, t) and [later, earlier) contain
// no instant, so a record carrying one could never be retrieved for any as-of -- it
// is dead weight that nothing would ever fail on, the interval-shaped version of the
// field-nothing-fails-on defect this corpus keeps recording. A writer who states a
// window states one an instant can fall inside.
//
// Both bounds empty is allowed and is not that defect: a timeless record is a
// legitimate always-valid assertion, not an unstated one, and the guard that keeps
// valid time from being decoration is the as-of filter in Retrieve, not a
// non-empty-bound requirement here.
func (v Validity) Validate() error {
	if err := v.From.canonical(); err != nil {
		return err
	}
	if err := v.To.canonical(); err != nil {
		return err
	}
	if v.From != "" && v.To != "" && v.From >= v.To {
		return fmt.Errorf("memory record valid-time interval [%s, %s) is empty: From must be strictly "+
			"before To, because a half-open interval whose bounds are equal or inverted contains no "+
			"instant, so the record could never be retrieved for any as-of", v.From, v.To)
	}
	return nil
}

// contains reports whether an as-of instant falls inside the interval under the
// half-open [From, To) rule: at or after From, strictly before To. An empty From is
// the unbounded past and an empty To the unbounded future, so a timeless interval
// (both empty) contains every as-of.
//
// # An empty as-of is the timeless floor
//
// A query that names no as-of asks no temporal question, so it admits only a record
// that makes no temporal claim -- both bounds empty. Any bound, upper or lower, is a
// claim the caller gave no instant to evaluate, so it fails closed and the record is
// withheld. This mirrors clearance, whose empty case is the public floor rather than
// a refusal: as-of is a point with a natural floor (the timeless record needs no
// as-of, as public needs no clearance), unlike purpose, whose empty set authorizes
// nothing because it has no floor member.
//
// The comparison is a plain string comparison, which is chronological here because
// canonical() has already forced every stored and queried instant into the one
// fixed-width UTC spelling; comparing them as time.Time would import the clock this
// store refuses, for no gain. The as-of is assumed canonical because Retrieve
// validates it before ranking, the same place and for the same reason it validates
// the query's clearance.
func (v Validity) contains(asOf Instant) bool {
	if asOf == "" {
		return v.From == "" && v.To == ""
	}
	if v.From != "" && asOf < v.From {
		return false
	}
	if v.To != "" && asOf >= v.To {
		return false
	}
	return true
}
