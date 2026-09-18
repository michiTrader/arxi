// Package memorystore persists governed memory records and answers retrieval
// with authorization applied before ranking.
//
// # Why this package exists at all
//
// Six ADRs (0020-0025) decided how retrieved memory is presented, identified,
// scoped, enumerated and validated. Every one of them describes a property of a
// `contextprep.MemoryReceipt`. A probe of the production build found what that
// leaves: `KindApprovedMemoryRecord` has **zero** construction sites outside
// tests, `RecordID` is assigned in production **zero** times, and the only
// receipt any code path emits is `frozen_context_memory` — a field on a frozen
// blueprint.
//
// So the channel was a finished pipe with nothing flowing through it. The rules
// about record versions (ADR-0021), principals (ADR-0022) and authority
// (ADR-0023) were reachable only from tests, because nothing in the system could
// produce a governed record for them to govern. This package is the producer,
// and it is deliberately the last of the memory work rather than the first: the
// presentation shape is the expensive thing to change once prepared contexts are
// committed against it, which is why Phase 7 settled that first.
//
// # An agent forgets between runs because nothing survives a run directory
//
// A run's truth is its event log (ADR-0002), and a log is per-run by
// construction. `ContextSpec.Memory` is frozen blueprint prose, identical for
// every run of that blueprint and therefore incapable of carrying anything a
// run learned. `ContextSpec.Shared` is within-run team material. `run fork`
// copies a parent's prefix, which is continuation of one history rather than
// recall across histories — and cmd/arxi/fork.go documents why it must not even
// be reported as lineage, because the copied prefix already contains the
// parent's `llm.response` events and summing them would double-count spend.
//
// None of those is a memory. Cross-run recall needs a store outside every run
// directory, keyed by a principal rather than by a run, which is what this is.
package memorystore

import (
	"fmt"
	"sort"
	"strings"
)

// Principal is one dimension of isolation from ADR-0022's vocabulary.
//
// A named type rather than a bare string so that the vocabulary is a closed set
// the compiler participates in. ADR-0022 deliberately added no scope field to
// any struct, on the grounds that "a scope type with no store to authorize would
// be a field nothing fails on" — the exact defect ADR-0021 was written about.
// The store is here now, so the type is here now, and not before.
type Principal string

// The canonical scope vocabulary. ADR-0022 fixed these seven and removed
// `subject` from the list rather than defining it, because `subject` already
// denotes the subject *agent* in five committed artifact schemas: a reader
// implementing `subject` scoping would have wired it to the field that already
// exists and shipped a store whose subject scope was its agent scope, silently.
const (
	// Tenant is the trust boundary. No retrieval ever crosses it.
	Tenant Principal = "tenant"
	// User is a person within a tenant.
	User Principal = "user"
	// Application is an external product adapter; Asha is one.
	Application Principal = "application"
	// Project is a body of work.
	Project Principal = "project"
	// Team is a blueprint with several members.
	Team Principal = "team"
	// Agent is one member of a blueprint.
	Agent Principal = "agent"
	// Run is a single execution.
	Run Principal = "run"
)

// principals enumerates the vocabulary once, as a map, for the same reason
// contextprep.memoryKindPresentable is a map rather than a switch: two lists
// would let a principal be valid in one place and unknown in the other by
// omission rather than by decision.
//
// The value is the containment rank, and it is not decoration. Tenant is the
// widest isolation and Run the narrowest, so a record scoped to a tenant is
// visible to more callers than one scoped to a run. Ranking retrieval by it
// makes the narrower, more specific memory win a tie, which is the direction a
// reader expects: what this run learned outranks what the tenant believes in
// general.
var principals = map[Principal]int{
	Tenant:      0,
	User:        1,
	Application: 2,
	Project:     3,
	Team:        4,
	Agent:       5,
	Run:         6,
}

// Specificity reports the containment rank of the principal, and whether it is
// part of the vocabulary at all.
//
// Unknown principals report false rather than a default rank. A rank invented
// for an unrecognized principal would sort it against real ones and let it be
// retrieved, which is ADR-0023's failure direction inverted: material nobody
// enumerated must get no standing, not the standing of whatever it sorts next
// to.
func (p Principal) Specificity() (int, bool) {
	rank, known := principals[p]
	return rank, known
}

// Scope binds a principal to the identity of that principal. Both halves are
// required: `user` alone authorizes nothing, because it does not say which
// user, and a store that treated the bare principal as a wildcard would make
// every cross-user leakage test pass by accident and leak in production.
type Scope struct {
	Principal Principal `json:"principal"`
	ID        string    `json:"id"`
}

// ErrScope is returned for every malformed scope so callers can distinguish a
// vocabulary error from an I/O error without string matching.
type ErrScope struct{ Err error }

func (e *ErrScope) Error() string { return e.Err.Error() }

func (e *ErrScope) Unwrap() error { return e.Err }

// Validate refuses a scope that cannot authorize anything.
//
// The `subject` case is named explicitly instead of falling into the generic
// unknown-principal message. ADR-0022 records that all three places this
// project named scopes disagreed, and that the roadmap itself said `subject`;
// anyone implementing against a stale copy of that list will type it. A generic
// "unknown principal" would read as a typo and invite a retry with the same
// word, so the refusal says what to use instead.
func (s Scope) Validate() error {
	if s.Principal == "" {
		return &ErrScope{fmt.Errorf("memory scope has no principal: a record that names no " +
			"principal cannot be authorized, and authorization runs before ranking")}
	}
	if s.Principal == "subject" {
		return &ErrScope{fmt.Errorf("memory scope principal %q is not in the vocabulary: "+
			"`subject` already denotes the subject agent in five committed artifact schemas, "+
			"so scoping by it would authorize the agent dimension while reading as a user or "+
			"tenant one -- use `user` for a person or `agent` for one member of a blueprint",
			s.Principal)}
	}
	if _, known := principals[s.Principal]; !known {
		return &ErrScope{fmt.Errorf("memory scope principal %q is not one of %s: the "+
			"vocabulary is closed, so an unrecognized principal gets no standing rather than "+
			"the standing of whatever it sorts beside", s.Principal, vocabulary())}
	}
	if strings.TrimSpace(s.ID) == "" {
		return &ErrScope{fmt.Errorf("memory scope %q has no id: the bare principal names a "+
			"dimension, not a holder, and treating it as a wildcard would authorize every "+
			"holder in that dimension", s.Principal)}
	}
	return nil
}

// String renders the scope as `principal:id`, the form the run log already uses
// for its own scope field (kernel.Event.Scope holds `run:<id>`). Reusing that
// shape means an operator reading a receipt beside a log sees one notation.
func (s Scope) String() string { return string(s.Principal) + ":" + s.ID }

// vocabulary renders the closed set in containment order for error messages.
// Sorted by rank rather than alphabetically so the message also teaches the
// widest-to-narrowest ordering that retrieval ranks by.
func vocabulary() string {
	names := make([]Principal, 0, len(principals))
	for p := range principals {
		names = append(names, p)
	}
	sort.Slice(names, func(i, j int) bool { return principals[names[i]] < principals[names[j]] })
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, string(n))
	}
	return strings.Join(parts, ", ")
}
