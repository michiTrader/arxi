package model

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// close compares dollars without demanding bit-exact float equality.
func close(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("cost = %.10f, expected %.10f", got, want)
	}
}

// TestAnUnknownPriceIsNotAPriceOfZero is the most important test in this file.
//
// Zero is a legitimate price (a model on loopback), so a caller cannot use
// "cost == 0" to mean "unpriced". If PriceOf returned a zero Price with no
// signal, every unrecognised model would spend without charging the budget:
// --budget 5.00 would be enforced against a total that never moves, so the
// ceiling would never be reached and the run would stop only when the money ran
// out somewhere else.
func TestAnUnknownPriceIsNotAPriceOfZero(t *testing.T) {
	p, ok := PriceOf("a-model-nobody-published")
	if ok {
		t.Fatalf("an unknown model reported a known price of %+v; the budget would be "+
			"charged with a number nobody published", p)
	}

	zero, ok := PriceOf("llama3.1")
	if !ok {
		t.Fatal("llama3.1 has no price entry; a model on loopback bills nothing, and " +
			"that has to be a KNOWN zero or a local run cannot enforce a budget either")
	}
	if !zero.Zero() {
		t.Errorf("llama3.1 costs %+v, expected nothing; it is served on loopback", zero)
	}
}

// TestOutputCostsMoreThanInput protects the reason the two rates are separate
// fields. If a future edit blends them, the arithmetic still runs and every
// number is still plausible -- which is exactly why it needs a test.
func TestOutputCostsMoreThanInput(t *testing.T) {
	for _, id := range PricedIDs() {
		p, ok := PriceOf(id)
		if !ok {
			t.Fatalf("%s is listed by PricedIDs but PriceOf does not know it", id)
		}
		if p.Zero() {
			continue // loopback: nothing to compare
		}
		if p.OutUSDPerMTok <= p.InUSDPerMTok {
			t.Errorf("%s: output %.2f is not dearer than input %.2f. Every provider in "+
				"the table charges more for output; if this is now false the table is "+
				"stale, and a stale table underquotes the bill",
				id, p.OutUSDPerMTok, p.InUSDPerMTok)
		}
	}
}

// TestCostIsPerMillionTokens pins the unit. The factor-of-1e6 mistake is the one
// this file exists to prevent, and it is invisible: a bill 1,000,000x too small
// looks like a free run, and one that large looks like a bug in the provider.
func TestCostIsPerMillionTokens(t *testing.T) {
	p := Price{InUSDPerMTok: 3, OutUSDPerMTok: 15}

	close(t, p.Cost(1_000_000, 0), 3)
	close(t, p.Cost(0, 1_000_000), 15)
	close(t, p.Cost(1_000_000, 1_000_000), 18)

	// A realistic turn: 8k in, 500 out. Sub-cent, and that is the point.
	close(t, p.Cost(8_000, 500), 8_000*3/1e6+500*15/1e6)
}

// TestASmallTurnIsNotRoundedToNothing: rounding per turn would floor most turns
// to zero, so a hundred turns would report a bill of nothing while the provider
// charged for all of them.
func TestASmallTurnIsNotRoundedToNothing(t *testing.T) {
	p, ok := PriceOf("claude-haiku-4-5")
	if !ok {
		t.Fatal("claude-haiku-4-5 is unpriced")
	}
	got := p.Cost(100, 10)
	if got <= 0 {
		t.Fatalf("a 110-token turn cost %v; rounded to zero here, a hundred such turns "+
			"report a bill of nothing while the provider charges for every one", got)
	}
	if got >= 0.01 {
		t.Errorf("a 110-token turn cost %v, which is more than a cent; the unit is wrong", got)
	}
}

// TestAQualifiedRefIsPriced: callers must not have to remember to strip the
// provider, because forgetting looks exactly like an unpriced model and the
// consequence of that is an unenforced budget.
func TestAQualifiedRefIsPriced(t *testing.T) {
	bare, ok1 := PriceOf("claude-sonnet-4-6")
	qual, ok2 := PriceOf("anthropic/claude-sonnet-4-6")
	if !ok1 || !ok2 {
		t.Fatalf("bare=%v qualified=%v; both spellings name one model", ok1, ok2)
	}
	if bare != qual {
		t.Errorf("the same model priced %+v bare and %+v qualified", bare, qual)
	}
}

// TestEveryKnownModelIsPriced is the guard against the table drifting apart.
//
// A model offered by `provider add` but unpriced is a model a run cannot spend
// against safely, and the gap would only be discovered by trying to use it.
func TestEveryKnownModelIsPriced(t *testing.T) {
	for _, name := range KnownNames() {
		_, models, ok := Known(name)
		if !ok {
			t.Fatalf("KnownNames lists %q but Known does not know it", name)
		}
		for _, id := range models {
			if _, priced := PriceOf(id); !priced {
				t.Errorf("provider %s offers %s, which this build cannot price.\n"+
					"  consequence: a run using it cannot charge --budget, so the ceiling "+
					"is never reached and the overspend appears on the invoice.\n"+
					"  fix: add it to prices in price.go, or stop offering it.", name, id)
			}
		}
	}
}

// TestTheNoPriceErrorNamesTheAlternatives: an error that says only "unknown
// model" leaves the user reading source to find out what they may use.
func TestTheNoPriceErrorNamesTheAlternatives(t *testing.T) {
	err := &ErrNoPrice{Ref: "some-model"}
	msg := err.Error()
	if !strings.Contains(msg, "some-model") {
		t.Errorf("error %q does not name the model that was refused", msg)
	}
	if !strings.Contains(msg, "claude-sonnet-4-6") {
		t.Errorf("error %q does not list a model that WOULD work; the user is left "+
			"guessing which is how a refusal becomes a dead end", msg)
	}
	if !strings.Contains(msg, "--budget") {
		t.Errorf("error %q does not say why an unpriced model is refused", msg)
	}
}

// TestTheNoPriceErrorIsDistinguishable: the caller has a real decision to make
// (refuse, or proceed with the budget unenforced and say so), and it cannot make
// it if the error is indistinguishable from every other failure.
func TestTheNoPriceErrorIsDistinguishable(t *testing.T) {
	var err error = &ErrNoPrice{Ref: "x"}
	var target *ErrNoPrice
	if !errors.As(err, &target) {
		t.Fatal("ErrNoPrice is not recoverable with errors.As, so a caller cannot tell " +
			"an unpriced model from a broken connection and must treat both the same")
	}
	if target.Ref != "x" {
		t.Errorf("Ref = %q, expected x", target.Ref)
	}
}

// TestPricedIDsIsSorted: the list appears in an error message, and an
// unstable order makes the message differ between runs for no reason.
func TestPricedIDsIsSorted(t *testing.T) {
	ids := PricedIDs()
	if len(ids) == 0 {
		t.Fatal("no models are priced at all")
	}
	for i := 1; i < len(ids); i++ {
		if ids[i-1] > ids[i] {
			t.Fatalf("PricedIDs is unsorted at %d (%q before %q)", i, ids[i-1], ids[i])
		}
	}
}
