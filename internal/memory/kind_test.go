package memory

import "testing"

// TestEachEnumeratedKindReportsItsAuthority pins the authority table at its new
// home (ADR-0031). It is the same table the receipt asserted in
// internal/contextprep before the enumeration moved here; keeping it means the
// pure leaf, not its caller, is where a mis-set flag fails.
func TestEachEnumeratedKindReportsItsAuthority(t *testing.T) {
	cases := []struct {
		kind            Kind
		wantKnown       bool
		wantPresentable bool
		wantGoverned    bool
	}{
		{KindFrozenContextMemory, true, true, false},
		{KindApprovedMemoryRecord, true, true, true},
		{KindProposedMemoryCandidate, true, false, true},
		{Kind("frozen_context_memry"), false, false, false}, // a typo is not authority
		{Kind(""), false, false, false},
	}
	for _, c := range cases {
		if got := c.kind.Known(); got != c.wantKnown {
			t.Errorf("Known() = %v for kind %q, want %v: an unknown kind must fail closed, or a "+
				"typo inherits the authority of a record somebody approved", got, string(c.kind), c.wantKnown)
		}
		if got := c.kind.Presentable(); got != c.wantPresentable {
			t.Errorf("Presentable() = %v for kind %q, want %v: a candidate that can be presented is "+
				"not a candidate, and an unknown kind gets no authority", got, string(c.kind), c.wantPresentable)
		}
		if got := c.kind.Governed(); got != c.wantGoverned {
			t.Errorf("Governed() = %v for kind %q, want %v: frozen configuration memory is not a "+
				"stored record and an unknown kind is not governed", got, string(c.kind), c.wantGoverned)
		}
	}
}

func TestKindValidateRefusesTheUnenumerated(t *testing.T) {
	for _, k := range []Kind{KindFrozenContextMemory, KindApprovedMemoryRecord, KindProposedMemoryCandidate} {
		if err := k.Validate(); err != nil {
			t.Errorf("Validate refused an enumerated kind %q: %v: a legitimate kind cannot be used", string(k), err)
		}
	}
	if err := Kind("").Validate(); err == nil {
		t.Fatal("Validate accepted an empty kind: a record must say what authority it carries")
	}
	if err := Kind("approved_memory_recrd").Validate(); err == nil {
		t.Fatal("Validate accepted an unenumerated kind: a kind nobody enumerated is not authority, " +
			"and accepting it lets a typo carry the weight of an approval")
	}
}
