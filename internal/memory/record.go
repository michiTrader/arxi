package memory

import "fmt"

// Record is one governed memory record: a scope and the append-only lineage of
// its versions. It is the unit a store holds and authorizes -- the place the
// pieces built separately compose. Identity, authority and validity live on each
// Version (ADR-0021/0032, ADR-0028); the correction/deletion chain is the
// lineage (ADR-0029/0030); the scope is here, once, because every version of a
// record shares its trust boundary and principals -- a correction does not move
// a record to another tenant or user.
//
// This is where ADR-0022's deferred scope field finally lands, and on the
// aggregate rather than the version for that reason: scope is a property of the
// record, not of each correction to it.
type Record struct {
	Scope    Scope     `json:"scope"`
	Versions []Version `json:"versions"`
}

// Validate refuses a record whose scope is unsafe or whose lineage is not an
// append-only history. It is the composition of the two checks built for the
// parts: a store that accepts a Record has accepted a tenant-scoped record and a
// contiguous, resurrection-proof chain, in one call.
func (r Record) Validate() error {
	if err := r.Scope.Validate(); err != nil {
		return fmt.Errorf("memory record scope is invalid: %w", err)
	}
	if err := ValidateLineage(r.Versions); err != nil {
		return fmt.Errorf("memory record lineage is invalid: %w", err)
	}
	return nil
}

// RecordID is the identity every version of this record shares. Valid only on a
// validated record; ValidateLineage guarantees the versions agree on it and that
// there is at least one.
func (r Record) RecordID() string {
	if len(r.Versions) == 0 {
		return ""
	}
	return r.Versions[0].RecordID
}

// VisibleTo is the record-level retrieval primitive: it authorizes the principal
// against the record's scope, then selects the version believed as-of
// decisionAsOf that holds at validAt. It returns that version, whether one was
// found, and an error only for a malformed record or query.
//
// This is authorization *before* selection, in that order, which is the order
// Phase 7 states: a principal not authorized for the record sees nothing, with
// no error -- an unauthorized retrieval is empty, not a failure -- and the
// bitemporal selection runs only within a record the principal may see. At most
// one version is live on the decision axis at a time (the lineage tiles), so the
// scan returns the single version in force. A record whose current version was
// retracted returns nothing for a present query while still returning the older
// version for a historical one, exactly as deletion and correction require.
func (r Record) VisibleTo(principal Scope, decisionAsOf, validAt string) (Version, bool, error) {
	if err := r.Validate(); err != nil {
		return Version{}, false, err
	}
	authorized, err := r.Scope.AuthorizedFor(principal)
	if err != nil {
		return Version{}, false, err
	}
	if !authorized {
		return Version{}, false, nil
	}
	for _, v := range r.Versions {
		live, err := v.Validity.LiveAt(decisionAsOf, validAt)
		if err != nil {
			return Version{}, false, err
		}
		if live {
			return v, true, nil
		}
	}
	return Version{}, false, nil
}
