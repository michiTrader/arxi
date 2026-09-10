package jobstore

import (
	"fmt"
	"math"

	"github.com/michiTrader/arxi/internal/job"
)

func addAmounts(left, right job.Amount) (job.Amount, error) {
	if !left.Canonical() || !right.Canonical() {
		return job.Amount{}, fmt.Errorf("non-canonical amount")
	}
	scale := left.Scale
	if right.Scale > scale {
		scale = right.Scale
	}
	l, ok := scaleCoefficient(left, scale)
	if !ok {
		return job.Amount{}, ErrAmountOverflow
	}
	r, ok := scaleCoefficient(right, scale)
	if !ok || math.MaxUint64-l < r {
		return job.Amount{}, ErrAmountOverflow
	}
	return job.NewAmount(l+r, scale), nil
}

func compareAmounts(left, right job.Amount) (int, error) {
	scale := left.Scale
	if right.Scale > scale {
		scale = right.Scale
	}
	l, ok := scaleCoefficient(left, scale)
	if !ok {
		return 0, ErrAmountOverflow
	}
	r, ok := scaleCoefficient(right, scale)
	if !ok {
		return 0, ErrAmountOverflow
	}
	switch {
	case l < r:
		return -1, nil
	case l > r:
		return 1, nil
	default:
		return 0, nil
	}
}

func scaleCoefficient(amount job.Amount, scale uint8) (uint64, bool) {
	value := amount.Coefficient
	for n := amount.Scale; n < scale; n++ {
		if value > math.MaxUint64/10 {
			return 0, false
		}
		value *= 10
	}
	return value, true
}

func windowKey(window job.LedgerWindow) string {
	return string(window.TriggerID) + "\x00" + string(window.Period) + "\x00" + window.StartsAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")
}
