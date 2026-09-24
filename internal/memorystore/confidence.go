package memorystore

import (
	"fmt"
	"sort"
	"strings"
)

// Confidence is how much the writer vouches for a record's correctness, and the
// first dimension that feeds ranking rather than authorization.
//
// A named type rather than a bare string so the vocabulary is a closed set the
// compiler participates in, the same shape as Principal, Sensitivity, Purpose
// and EvidenceClass and for the same reason ADR-0022 gives: two lists of values
// drift, and a value valid in one place and unknown in another leaks by omission
// rather than by decision. It is added now, with the ranker that reads it, and
// not before -- a confidence with nothing that fails when it is absent or ignored
// is the field-nothing-fails-on defect this corpus has recorded four times, and
// the roadmap names confidence explicitly as a place it could recur.
//
// # Confidence is a genuinely ranked dimension, and that is the contrast worth stating
//
// Evidence class looked rankable and was not: the trust order of stated, observed
// and imported flips with the question asked, so any rank there would be an order
// the domain does not have (ADR-0044). Confidence is the opposite case and worth
// recording as such. It is a self-contained degree of belief in the record's own
// correctness, and that belief does not invert with the question: a record its
// writer was sure of outranks one they guessed at, whether the caller is asking
// about a preference or an external fact. So confidence carries a rank honestly,
// where evidence class could not, and the two together are the worked examples of
// when a rank is earned and when it is invented.
//
// # It orders, it does not authorize
//
// The four dimensions before it -- scope, sensitivity, purpose, evidence class --
// each decide whether a record may be seen at all, and Retrieve withholds a record
// that fails any of them before the ranker runs. Confidence decides none of that.
// A low-confidence record the caller is authorized for is still returned; it is
// simply ordered below a high-confidence one. So confidence has no query-side set
// to match against and never removes a record from the result -- putting it in the
// authorization step would withhold material the caller is entitled to merely
// because the writer was unsure, which is not what "unsure" means. Its whole
// mechanism is the ranking comparator in Retrieve, and the guard that keeps it from
// being decoration is that removing it there reorders the result.
type Confidence string

// The canonical confidence vocabulary, least to most vouched-for. ADR-0045 fixed
// these three. Retrieval orders a higher-confidence record ahead of a lower one at
// the same scope specificity, so the ordering is ranking, not decoration.
const (
	// Low is a record its writer offers with little assurance: a weak inference, a
	// guess worth keeping but not worth trusting over anything better. It is the
	// rank floor, ordered last among records that are otherwise tied.
	Low Confidence = "low"
	// Medium is an ordinary assertion the writer stands behind without special
	// corroboration.
	Medium Confidence = "medium"
	// High is a record the writer vouches for strongly: a corroborated fact, a
	// preference the user stated outright. It outranks the levels below it when the
	// ranker breaks a tie on anything else.
	High Confidence = "high"
)

// confidences enumerates the vocabulary once, as a map, for the same reason
// sensitivities is a map: two lists would let a level be valid in one place and
// unknown in the other by omission rather than by decision.
//
// The value is the confidence rank. A higher rank is more vouched-for, so the
// ranker places it earlier. Low is the floor at 0. Unlike the evidence-class map,
// which is a presence set carrying no order, this one carries a rank on purpose:
// confidence is the dimension whose order the domain actually has.
var confidences = map[Confidence]int{
	Low:    0,
	Medium: 1,
	High:   2,
}

// Rank reports the confidence rank of the level, and whether it is part of the
// vocabulary at all.
//
// An unknown level reports false rather than a default rank, the same fail-closed
// direction Sensitivity.Rank and Principal.Specificity take. A rank invented for
// an unrecognized level would sort it against real ones: a record carrying a
// garbage confidence would take a rank it never earned and displace records the
// writer actually vouched for. A level nobody defined gets no standing, not the
// standing of whatever it sorts beside.
func (c Confidence) Rank() (int, bool) {
	rank, known := confidences[c]
	return rank, known
}

// Validate refuses a record confidence that cannot rank anything.
//
// The empty case is refused rather than defaulted, and that is the load-bearing
// choice of ADR-0045. It is tempting to argue that confidence, being ranked, has a
// safe least-privilege default the way sensitivity nearly does -- treat an unstated
// confidence as Low and bury it. That is the defect, not the safe path. A confidence
// nobody stated is not low confidence; it is no assessment, and ranking it as Low
// launders a missing judgment into a stated one -- the record reads as "the writer
// judged this weak" when the truth is "nobody judged it". Defaulting the other way,
// to High, is worse: it promotes unvetted material over records a writer deliberately
// rated. Neither default is honest, so the field is required, and requiring it means
// every writer states how far to trust the record. That cost is the decision. A
// confidence a caller may skip is the field nothing fails on.
func (c Confidence) Validate() error {
	if c == "" {
		return fmt.Errorf("memory record has no confidence: an unstated confidence is no assessment, "+
			"not low confidence, and defaulting it either buries a record nobody judged or promotes "+
			"unvetted material over one a writer rated -- state it as one of %s", confidenceVocabulary())
	}
	if _, known := confidences[c]; !known {
		return fmt.Errorf("memory record confidence %q is not one of %s: the vocabulary is closed, so "+
			"an unrecognized level gets no standing rather than the standing of whatever it sorts "+
			"beside", c, confidenceVocabulary())
	}
	return nil
}

// confidenceVocabulary renders the closed set in rank order for error messages.
// Sorted by rank rather than alphabetically so the message also teaches the
// least-to-most-vouched-for ordering the ranker reads.
func confidenceVocabulary() string {
	levels := make([]Confidence, 0, len(confidences))
	for c := range confidences {
		levels = append(levels, c)
	}
	sort.Slice(levels, func(i, j int) bool { return confidences[levels[i]] < confidences[levels[j]] })
	parts := make([]string, 0, len(levels))
	for _, l := range levels {
		parts = append(parts, string(l))
	}
	return strings.Join(parts, ", ")
}
