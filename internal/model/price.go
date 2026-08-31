package model

import (
	"fmt"
	"sort"
	"strings"
)

// Price is what one model costs, per MILLION tokens, in US dollars.
//
// Per million rather than per token because that is the unit every provider
// publishes, and converting at the point of entry is where the factor-of-1e6
// mistake gets made. Holding the published number means a price can be checked
// against a pricing page by eye, which is the only review this table will ever
// realistically get.
//
// Input and output are separate because output costs several times more than
// input on every provider in the table. Averaging them would misprice every
// turn in one direction or the other, and the direction depends on the shape of
// the work: a turn that reads a large file and answers briefly is mostly input,
// a turn that writes a file is mostly output. A single blended rate would make
// `--budget` wrong by a multiple, not by a rounding error.
type Price struct {
	InUSDPerMTok  float64 `json:"in_usd_per_mtok"`
	OutUSDPerMTok float64 `json:"out_usd_per_mtok"`
}

// Cost converts a token count into dollars.
//
// Nothing here rounds. A turn costing less than a cent is common, and rounding
// each turn to cents would floor most of them to zero, so a hundred turns would
// report a bill of nothing while the provider charged for all of them. The
// rounding, if any, belongs where the number is PRINTED.
func (p Price) Cost(inTok, outTok int) float64 {
	const perMillion = 1_000_000.0
	return (float64(inTok)/perMillion)*p.InUSDPerMTok +
		(float64(outTok)/perMillion)*p.OutUSDPerMTok
}

// Zero reports whether this price charges nothing for any conversation.
//
// This is a real answer and not a missing one: a model served on loopback costs
// nothing per token, and that is different in kind from a model whose price this
// binary has never heard of. See PriceOf.
func (p Price) Zero() bool {
	return p.InUSDPerMTok == 0 && p.OutUSDPerMTok == 0
}

// prices is the published price of every model in the known table.
//
// It lives in the binary rather than in the provider file on purpose. A price is
// a fact about the world that changes without asking us, and the provider file
// is a record of what the OPERATOR decided (which endpoint, which variable,
// which models are allowed). Mixing the two would mean a vendor's price change
// silently rewrites a file the operator wrote, and `provider add` would start
// producing files that differ between binary versions for reasons the operator
// never chose.
//
// Past runs are unaffected by an update here, because a run records the dollars
// it spent INTO ITS LOG at the moment it spends them. The log is the truth about
// what a run cost; this table is only how the next turn is priced.
//
// Keyed by bare model id. Two providers offering one id would be a genuine
// problem, and the resolver already refuses that case rather than choosing.
var prices = map[string]Price{
	// Anthropic, per million tokens.
	"claude-sonnet-4-6": {InUSDPerMTok: 3, OutUSDPerMTok: 15},
	"claude-opus-4-1":   {InUSDPerMTok: 15, OutUSDPerMTok: 75},
	"claude-haiku-4-5":  {InUSDPerMTok: 1, OutUSDPerMTok: 5},

	// OpenAI, per million tokens.
	"gpt-5.1":      {InUSDPerMTok: 1.25, OutUSDPerMTok: 10},
	"gpt-5.1-mini": {InUSDPerMTok: 0.25, OutUSDPerMTok: 2},

	// A model served on loopback bills nothing. This entry is what makes that a
	// KNOWN price of zero rather than an unknown one, which is the difference
	// between a run that can enforce a budget and a run that cannot.
	"llama3.1": {InUSDPerMTok: 0, OutUSDPerMTok: 0},
}

// PriceOf returns the published price of a model id.
//
// The bool is the whole point of the signature, and callers may not discard it.
// An unknown price is not a price of zero: reporting zero would let a run spend
// without ever charging the budget, so `--budget 5.00` would be enforced against
// a running total that never moves and the ceiling would never be reached. That
// is not a wrong number, it is a disabled safety mechanism, and the user would
// discover it on the invoice.
//
// A qualified ref ("anthropic/claude-sonnet-4-6") is accepted so callers do not
// have to remember to strip the provider first; forgetting would look like an
// unpriced model.
func PriceOf(ref string) (Price, bool) {
	_, id := ParseRef(ref)
	p, ok := prices[id]
	return p, ok
}

// ErrNoPrice is returned when a model's price is not known.
//
// It is a distinct error rather than a generic one because the caller has a real
// decision to make: refuse to spend, or spend with the budget unenforced and say
// so in the log.
type ErrNoPrice struct {
	Ref string
}

func (e *ErrNoPrice) Error() string {
	return fmt.Sprintf("no published price for model %q: "+
		"this build cannot charge a budget for it, and a run that cannot charge a "+
		"budget cannot enforce --budget. Use a model this build prices (%s), or "+
		"a provider whose models bill nothing",
		e.Ref, strings.Join(PricedIDs(), ", "))
}

// PricedIDs lists every model id this build can price, sorted.
//
// Exported so the error above can name the alternatives rather than telling the
// user to go and read a table in the source.
func PricedIDs() []string {
	out := make([]string, 0, len(prices))
	for id := range prices {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
