package memory

import "fmt"

// Selection is one memory version a retrieval chose, and why. It is the unit of
// "every influence identifies its source and version": a selection names the
// record and the exact version it drew from, the excerpt it actually carried
// forward, and the reason it ranked in. Without the version, a later correction
// cannot say whether the influence was the version it superseded or the one it
// replaced -- which is the propagation guarantee ADR-0021 exists to make
// possible.
type Selection struct {
	RecordID  string `json:"record_id"`
	VersionID string `json:"version_id"`
	// Excerpt is the exact text the retrieval carried forward. Optional: a
	// selection may use a whole record and record no narrower excerpt. It is not
	// an identity, so it is not required; the identity is the record and version.
	Excerpt string `json:"excerpt,omitempty"`
	// Reason is why this version ranked in. Required: an influence with no reason
	// is an influence nobody can audit, and the exit evidence names reasons
	// beside versions.
	Reason string `json:"reason"`
}

// RetrievalReceipt records exactly what one retrieval selected, on whose behalf,
// and under which ranking. It is the audit trail Phase 7's exit evidence
// requires -- "retrieval receipts record exact selected versions and excerpts,
// ranking/index versions and reasons" -- and it is the retrieval-side companion
// to the presentation receipt in internal/contextprep: that one records what was
// shown to a model, this one records what the store chose to show it.
//
// A receipt with no selections is valid and meaningful: a retrieval that matched
// nothing -- including one refused across a tenant boundary -- produces a receipt
// that proves nothing influenced the turn, which is itself the evidence the
// leakage tests assert.
type RetrievalReceipt struct {
	// Principal is the scope the retrieval was authorized against. It must name a
	// tenant, because a retrieval with no trust boundary is not an authorized
	// retrieval (ADR-0033).
	Principal Scope `json:"principal"`
	// RankingVersion identifies the ranking/index that produced this ordering.
	// Required: a selection is only reproducible against the ranking that made
	// it, and the exit evidence names the ranking/index version explicitly.
	RankingVersion string      `json:"ranking_version"`
	Selections     []Selection `json:"selections"`
}

// Validate refuses a receipt that cannot account for what it selected: a
// principal with no tenant, no ranking version, a selection that does not name
// its record and version, an unexplained selection, or the same version claimed
// twice.
//
// This is the assertion that keeps "every influence identifies its source and
// version" from being a comment. A selection missing its version would encode,
// marshal and read exactly like one that named it, letting a store report an
// influence a correction can never find -- the same shape ADR-0021 caught on the
// presentation receipt, here on the retrieval side.
func (r RetrievalReceipt) Validate() error {
	if err := r.Principal.Validate(); err != nil {
		return fmt.Errorf("retrieval receipt principal is not authorizable: %w", err)
	}
	if r.RankingVersion == "" {
		return fmt.Errorf("retrieval receipt has no ranking_version: a selection is only reproducible " +
			"against the ranking that produced it, so a receipt that does not name the ranking cannot " +
			"be audited or replayed")
	}
	seen := make(map[string]bool, len(r.Selections))
	for i, s := range r.Selections {
		if s.RecordID == "" {
			return fmt.Errorf("selection %d of %d has no record_id: an influence that does not name its "+
				"source cannot be traced, corrected or deleted", i+1, len(r.Selections))
		}
		if s.VersionID == "" {
			return fmt.Errorf("selection %d of %d for record %q has no version_id: without it a correction "+
				"cannot say whether the influence was the version it superseded or the one that replaced "+
				"it, which is the guarantee Phase 7 owes", i+1, len(r.Selections), s.RecordID)
		}
		if s.Reason == "" {
			return fmt.Errorf("selection %d of %d (%q) has no reason: an influence nobody can explain is "+
				"an influence nobody can review, and the exit evidence names reasons beside versions",
				i+1, len(r.Selections), s.VersionID)
		}
		if seen[s.VersionID] {
			return fmt.Errorf("selection %d of %d repeats version_id %q: a version selected twice is a "+
				"double-counted influence, and the receipt would overstate what one version contributed",
				i+1, len(r.Selections), s.VersionID)
		}
		seen[s.VersionID] = true
	}
	return nil
}
