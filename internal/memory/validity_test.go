package memory

import "testing"

// Instants used across the table. Spaced so "before", "at" and "after" each
// boundary are distinct, and chosen on the RFC3339Nano layout the store uses.
const (
	t1 = "2026-01-01T00:00:00Z" // v1 recorded / fact begins
	t2 = "2026-06-01T00:00:00Z" // correction: v1 superseded, v2 recorded
	t3 = "2026-09-01T00:00:00Z" // "now" for a present-belief query
	t0 = "2025-01-01T00:00:00Z" // before anything
)

func TestValidateRefusesIllFormedValidity(t *testing.T) {
	cases := []struct {
		name    string
		v       Validity
		wantErr bool
	}{
		{
			name:    "open-ended current version is valid",
			v:       Validity{ValidFrom: t1, RecordedAt: t1},
			wantErr: false,
		},
		{
			name:    "fully bounded superseded version is valid",
			v:       Validity{ValidFrom: t1, ValidTo: t3, RecordedAt: t1, SupersededAt: t2},
			wantErr: false,
		},
		{
			name:    "missing valid_from is refused",
			v:       Validity{RecordedAt: t1},
			wantErr: true,
		},
		{
			name:    "missing recorded_at is refused",
			v:       Validity{ValidFrom: t1},
			wantErr: true,
		},
		{
			name:    "valid_to before valid_from is refused",
			v:       Validity{ValidFrom: t2, ValidTo: t1, RecordedAt: t1},
			wantErr: true,
		},
		{
			name:    "superseded_at before recorded_at is refused",
			v:       Validity{ValidFrom: t1, RecordedAt: t2, SupersededAt: t1},
			wantErr: true,
		},
		{
			name:    "unparseable instant is refused, not treated as absent",
			v:       Validity{ValidFrom: "2026-01-01", RecordedAt: t1},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.v.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("Validate accepted an ill-formed validity (%+v): a store would commit a version "+
					"whose temporal position no as-of query can trust, and the leakage surfaces only when "+
					"a correction fails to hide it", tc.v)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate rejected a well-formed validity (%+v): %v: a legitimate record cannot "+
					"enter the store, so correction and history cannot be exercised at all", tc.v, err)
			}
		})
	}
}

// TestCorrectionHidesTheStaleVersionButKeepsTheHistory is the scenario Phase 7's
// exit evidence names: a stale version stops appearing after correction, and the
// history of what was believed is preserved rather than rewritten.
//
// Two versions of one record. v1 was recorded at t1 and superseded at t2; v2 was
// recorded at t2 and is current. The correction closed v1's belief interval; it
// did not mutate v1's fact. So a present-belief query (as-of t3) must see only
// v2, and a historical query (as-of t1) must still see v1 -- which is the whole
// point of a decision axis separate from the valid axis.
func TestCorrectionHidesTheStaleVersionButKeepsTheHistory(t *testing.T) {
	v1 := Validity{ValidFrom: t1, RecordedAt: t1, SupersededAt: t2}
	v2 := Validity{ValidFrom: t1, RecordedAt: t2}

	// Present belief, as-of "now": the corrected version is gone, the current
	// one remains. This is "stale versions stop appearing after correction".
	if live, err := v1.LiveAt(t3, t3); err != nil || live {
		t.Fatalf("v1 is still live as-of now (live=%v err=%v): a superseded version that keeps "+
			"appearing is exactly the stale record Phase 7 forbids -- a correction that does not "+
			"actually retract the old belief", live, err)
	}
	if live, err := v2.LiveAt(t3, t3); err != nil || !live {
		t.Fatalf("v2 is not live as-of now (live=%v err=%v): the current version must be the one a "+
			"present query returns, or a correction hides the record entirely instead of replacing it",
			live, err)
	}

	// Historical belief, as-of t1: v1 was current then, and asking what was
	// believed at t1 must still return it. This is "contradictions preserve
	// temporal history": correction closes the interval, it does not erase it.
	if live, err := v1.LiveAt(t1, t3); err != nil || !live {
		t.Fatalf("v1 is not live as-of t1 (live=%v err=%v): the belief interval was closed by the "+
			"correction, not erased, so a query as-of when it was current must still see it -- "+
			"otherwise history is rewritten and an audit of what was believed is impossible",
			live, err)
	}
}

// TestHalfOpenIntervalsTileWithoutOverlapAtTheBoundary pins the boundary
// instant. At exactly t2 -- the instant v1 is superseded and v2 recorded --
// exactly one version is live, not both and not neither. A closed-closed
// interval would return both, which is the overlap the deletion-lineage exit
// evidence forbids.
func TestHalfOpenIntervalsTileWithoutOverlapAtTheBoundary(t *testing.T) {
	v1 := Validity{ValidFrom: t1, RecordedAt: t1, SupersededAt: t2}
	v2 := Validity{ValidFrom: t1, RecordedAt: t2}

	v1Live, err := v1.LiveAt(t2, t3)
	if err != nil {
		t.Fatalf("v1.LiveAt at the boundary errored: %v", err)
	}
	v2Live, err := v2.LiveAt(t2, t3)
	if err != nil {
		t.Fatalf("v2.LiveAt at the boundary errored: %v", err)
	}
	if v1Live == v2Live {
		t.Fatalf("at the supersession instant t2 both versions report live=%v: half-open belief "+
			"intervals must hand the boundary instant to exactly the successor, or a query at the "+
			"switchover sees the record twice (overlap) or not at all (gap)", v1Live)
	}
	if v1Live {
		t.Fatal("v1 owns the supersession instant t2: [recorded_at, superseded_at) is exclusive at the " +
			"top, so t2 belongs to v2, the version that replaced it")
	}
}

// TestValidTimeExcludesInstantsOutsideTheFactWindow keeps the two axes
// independent: a version can be the current belief and still not hold at a world
// instant outside its valid window. Belief is about the store; validity is about
// the world, and conflating them is the defect the bitemporal split exists to
// prevent.
func TestValidTimeExcludesInstantsOutsideTheFactWindow(t *testing.T) {
	// Current belief (never superseded), but the fact only held [t1, t2).
	v := Validity{ValidFrom: t1, ValidTo: t2, RecordedAt: t1}

	if live, err := v.LiveAt(t3, t0); err != nil || live {
		t.Fatalf("version reports live at world instant t0 before its fact began (live=%v err=%v): "+
			"a current belief about a fact that did not yet hold must not answer a world-time query "+
			"outside its valid window", live, err)
	}
	if live, err := v.LiveAt(t3, t2); err != nil || live {
		t.Fatalf("version reports live at world instant t2, the exclusive end of its valid window "+
			"(live=%v err=%v): [valid_from, valid_to) is exclusive at the top, so the fact no longer "+
			"holds at valid_to", live, err)
	}
	if live, err := v.LiveAt(t3, t1); err != nil || !live {
		t.Fatalf("version reports not live at world instant t1, the start of its valid window "+
			"(live=%v err=%v): [valid_from, valid_to) includes valid_from", live, err)
	}
}

// TestLiveAtRequiresBothQueryInstants keeps the package off the wall clock. An
// as-of query with a missing instant must fail rather than defaulting to now:
// defaulting would read a clock this package is forbidden to touch, and would
// make the same query answer differently on a different afternoon.
func TestLiveAtRequiresBothQueryInstants(t *testing.T) {
	v := Validity{ValidFrom: t1, RecordedAt: t1}
	if _, err := v.LiveAt("", t3); err == nil {
		t.Fatal("LiveAt accepted an empty decision_as_of: an as-of query with no as-of is not a " +
			"bitemporal query, and answering it by defaulting to now would read the wall clock this " +
			"package must never touch")
	}
	if _, err := v.LiveAt(t3, ""); err == nil {
		t.Fatal("LiveAt accepted an empty valid_at: a world-time query with no world instant cannot " +
			"place the fact on the valid axis, so it must fail rather than guess")
	}
}
