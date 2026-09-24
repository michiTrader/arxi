package memorystore

import (
	"fmt"
	"sort"
	"strings"
)

// Retention is the lifecycle policy a record is kept under, and the first
// dimension that feeds neither authorization nor ranking but expiry.
//
// A named type rather than a bare string so the vocabulary is a closed set the
// compiler participates in, the same shape as Principal, Sensitivity, Purpose,
// EvidenceClass and Confidence and for the same reason ADR-0022 gives: two lists
// of values drift, and a value valid in one place and unknown in another leaks
// by omission rather than by decision. It is added now, with the sweep that acts
// on it, and not before -- a retention with nothing that expires on it is the
// field-nothing-fails-on defect this corpus has recorded, one lifecycle rank
// below the confidence the same defect nearly reached.
//
// # It expires; it does not authorize and it does not rank
//
// The four dimensions before confidence -- scope, sensitivity, purpose, evidence
// class -- each decide whether a record may be seen at all, and confidence
// decides where it ranks among the records already authorized. Retention decides
// none of that. A record's retention never changes whether it is retrieved or
// where it orders: an ephemeral record the caller is authorized for is returned
// exactly as a permanent one is, in the same position, right up until a lifecycle
// sweep expires it. So retention has no query-side set to match against and no
// clause in the ranking comparator; its whole mechanism is Expire, and the guard
// that keeps it from being decoration is that removing the permanent check there
// tombstones a record a user committed to keep.
//
// # A named type over a bool, even at two members
//
// The vocabulary is only two levels today, and a bool `Expirable` would seem to
// carry the same information. It would not. A bool has no unstated state: its zero
// value is false, so a record whose writer never made a lifecycle decision would
// read as "keep forever" with nothing to say the choice was never made -- the
// exact default this dimension is required in order to refuse. A closed string
// vocabulary makes the unstated case a value outside the set, so Validate can
// refuse it, and it leaves room for a third policy to fail closed rather than be
// mistaken for one of the two the domain already knows.
type Retention string

// The canonical retention vocabulary. ADR-0046 fixed these two. The store reads
// no clock, so a policy here is not a duration but the lifecycle a sweep applies:
// a record is either kept until an explicit deletion, or eligible for an
// automated expiry.
const (
	// Permanent is a record kept until an explicit operator deletion. An automated
	// Expire sweep never removes it. This is material a user or operator
	// deliberately committed to keep, and it is the policy that must never be
	// reached by defaulting an unstated field, because defaulting to it would hoard
	// records a writer meant to be transient.
	Permanent Retention = "permanent"
	// Ephemeral is a record an automated Expire sweep may tombstone. This is
	// transient material -- a proposed candidate, a low-value observation -- that
	// should not accumulate indefinitely. Expiry is a tombstone like any deletion,
	// so an expired ephemeral record does not resurrect on a replica.
	Ephemeral Retention = "ephemeral"
)

// retentions enumerates the vocabulary once, as a map, for the same reason
// sensitivities and evidenceClasses are maps: two lists would let a policy be
// valid in one place and unknown in the other by omission rather than by
// decision.
//
// The value is whether an automated sweep may expire the record. It is not a
// rank -- permanent and ephemeral are not ordered, they are different lifecycles
// -- so unlike the confidence map this one carries a lifecycle predicate rather
// than an ordering, the shape the domain actually has.
var retentions = map[Retention]bool{
	Permanent: false,
	Ephemeral: true,
}

// Expirable reports whether an automated Expire sweep may tombstone a record
// held under this retention, and whether the retention is part of the vocabulary
// at all.
//
// An unknown retention reports false for both, the same fail-closed direction
// Sensitivity.Rank, Purpose.Known and Confidence.Rank take. A retention nobody
// defined does not become expirable by default: expiring a record on the
// strength of a policy the vocabulary does not recognize would be a deletion
// justified by a value that means nothing, so an unrecognized policy gets no
// standing and a sweep leaves it alone.
func (r Retention) Expirable() (expirable bool, known bool) {
	expirable, known = retentions[r]
	return expirable, known
}

// Validate refuses a record retention that governs no lifecycle.
//
// The empty case is refused rather than defaulted, and that is the load-bearing
// choice of ADR-0046. Both defaults are dishonest and in opposite directions,
// which is the argument for requiring the field rather than picking one. Defaulting
// an unstated retention to Permanent looks safe -- it never loses data -- but it
// hoards a record a writer meant to be transient, so material a user asked to be
// forgotten lives forever, which is the "deletion without resurrection" guarantee
// failing through lifecycle instead of through a missed tombstone. Defaulting to
// Ephemeral is worse: it silently makes a durable record collectable, so the next
// sweep tombstones a record nobody chose to expire. An unstated retention is not a
// lifecycle decision at all; recording it as either policy fabricates a choice no
// one made, so the field is required.
func (r Retention) Validate() error {
	if r == "" {
		return fmt.Errorf("memory record has no retention: an unstated retention is no lifecycle "+
			"decision, and defaulting it either hoards a record meant to be transient or expires one "+
			"meant to be kept -- state it as one of %s", retentionVocabulary())
	}
	if _, known := retentions[r]; !known {
		return fmt.Errorf("memory record retention %q is not one of %s: the vocabulary is closed, so "+
			"an unrecognized policy gets no standing rather than being expired on the strength of a "+
			"value the sweep does not recognize", r, retentionVocabulary())
	}
	return nil
}

// retentionVocabulary renders the closed set for error messages. Sorted
// alphabetically rather than by any rank, because retention policies are
// unordered and any order the message implied would be a lifecycle relation the
// domain does not have.
func retentionVocabulary() string {
	names := make([]string, 0, len(retentions))
	for r := range retentions {
		names = append(names, string(r))
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
