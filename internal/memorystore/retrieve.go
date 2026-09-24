package memorystore

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/michiTrader/arxi/internal/contextprep"
)

// RetrievalVersion names the ranking implementation. Phase 7's exit evidence
// requires retrieval receipts to record "ranking/index versions and reasons",
// so the identity of the ranker is part of the evidence, not a build detail.
const RetrievalVersion = "arxi.memory-retrieval/v1"

// Query names one retrieval. The scopes are the principals the caller is
// authorized for, not the ones it would like to read.
type Query struct {
	// Scopes is the authorization set. A record is visible only if its scope
	// appears here exactly — same principal, same ID.
	Scopes []Scope
	// Clearance is the highest sensitivity the caller may receive. A record
	// whose level outranks it is withheld in the authorization step, before
	// ranking, on the same reasoning ADR-0027 gives for scope. Empty is the
	// public floor: a caller that names no clearance receives only material that
	// needs none, which is the fail-closed default rather than a wildcard.
	Clearance Sensitivity
	// Purposes is the set of uses the caller is authorized to serve. A record is
	// visible only if its purpose is in this set (ADR-0043). Empty authorizes
	// nothing -- a caller that names no purpose is authorized for no purpose --
	// which is the fail-closed default and the honest one for an unranked
	// dimension: purpose has no floor member to fall back to, so this mirrors the
	// scope rule (an empty scope set authorizes no holder) rather than the
	// clearance rule (an empty clearance is the public floor).
	Purposes []Purpose
	// EvidenceClasses is the set of evidence kinds the caller will act on. A record
	// is visible only if its class is in this set (ADR-0044). Empty authorizes
	// nothing, the same fail-closed floor as the purpose set and for the same
	// reason: evidence class is unranked, so there is no floor member to fall back
	// to, and a caller that names no class accepts no evidence rather than all of
	// it. This mirrors the scope and purpose rules, not the clearance floor.
	EvidenceClasses []EvidenceClass
	// There is deliberately no confidence field here. Confidence ranks the
	// authorized set, it does not authorize (ADR-0045), so there is nothing for a
	// query to match against: a caller does not ask for "records of at least
	// medium confidence" the way it names the scopes, purposes and classes it is
	// authorized for. A record of any confidence the caller is otherwise entitled
	// to is returned; confidence only decides the order. Adding a query-side
	// confidence floor here would turn a ranking dimension into an authorization
	// one and withhold material the caller may see merely because the writer was
	// unsure -- the exact confusion this dimension is the worked example against.
	// Limit caps the returned records. Zero means no cap.
	Limit int
}

// Retrieval is the evidence of one retrieval, returned beside the records.
//
// Returned as a value rather than logged internally because the caller is what
// commits it: contextprep freezes the receipts into a prepared-context artifact
// under ADR-0013's barrier, and a store that logged its own reasoning somewhere
// else would put half the evidence outside the artifact that claims to hold it.
type Retrieval struct {
	Schema           string   `json:"schema"`
	RetrievalVersion string   `json:"retrieval_version"`
	Scopes           []string `json:"authorized_scopes"`
	// Clearance is the sensitivity ceiling the retrieval was authorized under,
	// resolved to the public floor when the query named none. Recorded so an
	// audit can answer not only which records were presented but under what
	// clearance, the "every influence identifies its source" requirement widened
	// to the level that influence was cleared at.
	Clearance string `json:"clearance"`
	// Purposes is the set of uses the retrieval was authorized for, sorted.
	// Recorded beside the clearance so an audit can answer not only which records
	// were presented and under what clearance but for which use each was
	// authorized -- the "every influence identifies its source" requirement,
	// widened once more (ADR-0043).
	Purposes []string `json:"authorized_purposes"`
	// EvidenceClasses is the set of evidence kinds the retrieval was authorized
	// for, sorted. Recorded beside the purposes so an audit can answer not only
	// which records were presented and for which use but on what kind of evidence
	// each rested -- the "every influence identifies its source" requirement,
	// widened once more (ADR-0044).
	EvidenceClasses []string `json:"authorized_evidence_classes"`
	// Considered is how many live versions existed before authorization,
	// Authorized how many survived it. The pair is the leakage measurement:
	// a cross-tenant test asserts Authorized is zero, and Considered proves
	// the records were actually present to be leaked rather than absent.
	// Authorization is scope, purpose, evidence class and clearance together, so a
	// record in the caller's scope but approved for a use the caller is not
	// authorized for, backed by a class it does not accept, or above its
	// clearance, is considered and not authorized -- the same contained-refusal
	// witness as a cross-scope record, one dimension over.
	Considered int         `json:"considered"`
	Authorized int         `json:"authorized"`
	Selections []Selection `json:"selections"`
	// Forked names the caller's own records that were withheld because their
	// supersession chain has more than one current version. Reported rather
	// than dropped silently: a record excluded with no trace looks exactly
	// like a record that was never written, so a correction that lost a race
	// would present as memory the user never saved. Record IDs only -- the
	// competing version IDs are an operator concern and live on Store.Forks.
	Forked []string `json:"forked,omitempty"`
}

// Selection records one returned record and why it ranked where it did.
type Selection struct {
	RecordID      string `json:"record_id"`
	VersionID     string `json:"version_id"`
	Scope         string `json:"scope"`
	Sensitivity   string `json:"sensitivity"`
	Purpose       string `json:"purpose"`
	EvidenceClass string `json:"evidence_class"`
	// Confidence is recorded because it is the first dimension that changed where
	// a record ranked rather than whether it was authorized (ADR-0045). An audit
	// reading the reason must be able to see that a record placed below another
	// was placed there for its confidence and not withheld, which is the whole
	// distinction between a ranking dimension and an authorization one.
	Confidence string `json:"confidence"`
	Reason     string `json:"reason"`
}

// Retrieve returns the live, authorized, presentable records for a query,
// most specific scope first, beside the receipt evidence for each.
//
// # Authorization strictly precedes ranking, and the order is the decision
//
// Phase 7 states it as a sequence: "Authorization occurs before semantic
// ranking". That is not an optimization. Ranking first and filtering after
// means the ranker has seen every record in the store, so a bug that leaks one
// record into the output leaks it from the whole corpus rather than from the
// caller's own scope — and a relevance score computed across tenants is itself
// a cross-tenant inference even when the record is dropped afterwards. So the
// candidate set here is built from the authorized scopes and the ranker never
// receives anything else.
//
// # Exact scope match, never prefix or hierarchy
//
// A record scoped `project:arxi` is not returned to a caller authorized for
// `tenant:acme`, even though the project belongs to that tenant. Implying
// containment would require this package to know the tenant of every project
// and the membership of every team, which it does not and cannot: that mapping
// lives in whatever product owns identity (Asha, per the roadmap's product
// boundary). Inferring it from string prefixes would authorize
// `project:arxi-secret` for a caller holding `project:arxi`. The caller passes
// the scopes it holds; expanding a principal into the scopes it contains is the
// identity system's job, and doing it here would be this store guessing.
func (s *Store) Retrieve(q Query) ([]Record, Retrieval, error) {
	for _, scope := range q.Scopes {
		if err := scope.Validate(); err != nil {
			return nil, Retrieval{}, err
		}
	}
	// A named clearance must be a level the vocabulary knows; an empty one is the
	// public floor and needs no validation. An unknown clearance is refused
	// rather than treated as the floor, because silently narrowing a caller's
	// clearance to public because it was misspelled would present a different set
	// than intended with no error to say so -- the closed-vocabulary rule the
	// record's own level obeys, applied to the query.
	effectiveClearance := q.Clearance
	if effectiveClearance == "" {
		effectiveClearance = Public
	} else if _, known := q.Clearance.Rank(); !known {
		return nil, Retrieval{}, fmt.Errorf("memory query clearance %q is not one of %s: an "+
			"unrecognized clearance is refused rather than narrowed to the floor, because a "+
			"misspelled clearance would silently present a different set than intended",
			q.Clearance, sensitivityVocabulary())
	}
	// The authorized purpose set is built and validated the same way. An unknown
	// purpose is refused rather than dropped from the set, because silently
	// dropping a misspelled purpose would narrow the caller's authorization with
	// no error to say so -- the closed-vocabulary rule the record's own purpose
	// obeys, applied to the query. The set is deduplicated for the same reason
	// the scope set is. An empty set is not an error: it authorizes nothing,
	// which is the fail-closed floor of an unranked dimension.
	authorizedPurposes := make(map[Purpose]bool, len(q.Purposes))
	purposeNames := make([]string, 0, len(q.Purposes))
	for _, p := range q.Purposes {
		if !p.Known() {
			return nil, Retrieval{}, fmt.Errorf("memory query purpose %q is not one of %s: an "+
				"unrecognized purpose is refused rather than dropped from the authorized set, because "+
				"a misspelled purpose would silently narrow authorization and present a different set "+
				"than intended", p, purposeVocabulary())
		}
		if authorizedPurposes[p] {
			continue
		}
		authorizedPurposes[p] = true
		purposeNames = append(purposeNames, string(p))
	}
	sort.Strings(purposeNames)
	// The accepted evidence-class set is built and validated the same way as the
	// purpose set, and for the same reasons: an unknown class is refused rather
	// than dropped, because silently dropping a misspelled class would narrow what
	// the caller accepts with no error to say so; the set is deduplicated; and an
	// empty set is not an error but authorizes nothing, the fail-closed floor of
	// an unranked dimension.
	acceptedClasses := make(map[EvidenceClass]bool, len(q.EvidenceClasses))
	classNames := make([]string, 0, len(q.EvidenceClasses))
	for _, e := range q.EvidenceClasses {
		if !e.Known() {
			return nil, Retrieval{}, fmt.Errorf("memory query evidence class %q is not one of %s: an "+
				"unrecognized class is refused rather than dropped from the accepted set, because a "+
				"misspelled class would silently narrow what the caller accepts and present a different "+
				"set than intended", e, evidenceClassVocabulary())
		}
		if acceptedClasses[e] {
			continue
		}
		acceptedClasses[e] = true
		classNames = append(classNames, string(e))
	}
	sort.Strings(classNames)
	authorized := make(map[string]bool, len(q.Scopes))
	names := make([]string, 0, len(q.Scopes))
	for _, scope := range q.Scopes {
		if authorized[scope.String()] {
			continue
		}
		authorized[scope.String()] = true
		names = append(names, scope.String())
	}
	sort.Strings(names)

	versions, err := s.Versions()
	if err != nil {
		return nil, Retrieval{}, err
	}
	// A forked record is excluded, and the store is still read. Before
	// ADR-0028 a single fork made this function return an error, so one
	// damaged record in one tenant denied memory to every tenant -- and the
	// error text named version IDs across the tenant boundary ADR-0027 calls
	// the one boundary no retrieval crosses. Authorization must precede
	// ranking; a store-wide failure precedes authorization, which is that same
	// argument violated from the other side.
	live, forked := tips(versions)
	evidence := Retrieval{Schema: Schema, RetrievalVersion: RetrievalVersion,
		Scopes: names, Clearance: string(effectiveClearance), Purposes: purposeNames,
		EvidenceClasses: classNames, Selections: []Selection{}, Forked: []string{}}

	var kept []Record
	for _, r := range live {
		// A tombstone is not a record that failed to be selected; it is the
		// absence of a record. Counting it as considered would report a
		// deleted record as present in the corpus, which reads as a leak.
		if r.Deleted {
			continue
		}
		evidence.Considered++
		if !authorized[r.Scope.String()] {
			continue
		}
		// Purpose is the third authorization dimension (ADR-0043), checked in the
		// same step as scope and clearance and before ranking. A record approved
		// for a use the caller is not authorized to serve is withheld exactly as
		// one outside its scope is, and for the same reason: a ranker that scored
		// it would already have treated disallowed material as a candidate.
		// Membership, not a ceiling -- the record's purpose must be in the query's
		// authorized set, and an empty set authorizes nothing.
		if !authorizedPurposes[r.Purpose] {
			continue
		}
		// Evidence class is the fourth authorization dimension (ADR-0044), checked
		// in the same pre-ranking step as scope, purpose and clearance. A record
		// backed by a class the caller does not accept is withheld exactly as one
		// outside its scope is, and for the same reason: a ranker that scored it
		// would already have treated disallowed material as a candidate. Membership,
		// not a ceiling -- the record's class must be in the query's accepted set,
		// and an empty set accepts nothing.
		if !acceptedClasses[r.EvidenceClass] {
			continue
		}
		// Clearance is the second authorization dimension (ADR-0042), checked in
		// the same step as scope and before ranking. A record above the caller's
		// clearance is withheld exactly as one outside its scope is, and for the
		// same reason ADR-0027 gives: a ranker that scored it would already have
		// treated disallowed material as a candidate. It is filtered before
		// Authorized++ because a record the caller may not receive is not
		// authorized, so the Considered/Authorized pair measures a clearance leak
		// the same way it measures a scope one.
		if !effectiveClearance.admits(r.Sensitivity) {
			continue
		}
		evidence.Authorized++
		// Authority is checked after authorization and before ranking. A
		// candidate belonging to the caller is authorized and still not
		// presentable: `Presentable` is contextprep's enumeration, so the
		// answer here and the barrier's answer come from one table rather
		// than from two lists that can disagree.
		if !r.Receipt().Presentable() {
			continue
		}
		kept = append(kept, r)
	}

	// A forked record the caller owns is named, so a correction that lost a
	// race is visibly withheld rather than silently absent -- indistinguishable
	// otherwise from a record nobody ever wrote, which is the silent loss this
	// store exists to prevent.
	//
	// Scoped to the caller's own records for the same reason authorization
	// precedes ranking: a fork in another tenant is not this caller's evidence,
	// and naming it here would disclose the existence of records the caller is
	// not authorized for. An operator reads the whole set from Store.Forks.
	// Record IDs only; the competing version IDs stay in Forks, which is a
	// local inspection verb rather than a value committed into an artifact.
	if len(forked) > 0 {
		scopeOf := make(map[string]Scope, len(forked))
		for _, v := range versions {
			if _, bad := forked[v.RecordID]; bad {
				scopeOf[v.RecordID] = v.Scope
			}
		}
		for recordID := range forked {
			if authorized[scopeOf[recordID].String()] {
				evidence.Forked = append(evidence.Forked, recordID)
			}
		}
		sort.Strings(evidence.Forked)
	}

	// Ranking. There is no semantic ranker yet and this deliberately does not
	// pretend to be one: it orders by scope specificity, so what this run or
	// this agent learned outranks what the tenant believes in general, then by
	// confidence, so a record its writer vouched for strongly outranks a weaker
	// one at the same specificity, and breaks the remaining ties by record ID for
	// determinism. Calling it semantic would be the kind of unearned claim this
	// project keeps finding in its own docs. The reason string on every selection
	// says exactly which rule applied, which is what the exit evidence asks for.
	//
	// Confidence enters here and nowhere else, and that is the load-bearing fact of
	// ADR-0045: it is a ranking dimension, so its only mechanism is this comparator.
	// An unknown confidence would already have been refused by Validate on the way
	// in, so a kept record always carries a ranked one; ranks are read through the
	// vocabulary rather than compared as strings, so "high" correctly outranks
	// "low" instead of losing to it alphabetically. Remove the confidence clause and
	// two records tied on specificity fall back to record-ID order, presenting a
	// guess ahead of a vouched-for fact -- which is the reordering the guard catches.
	sort.SliceStable(kept, func(i, j int) bool {
		ri, _ := kept[i].Scope.Principal.Specificity()
		rj, _ := kept[j].Scope.Principal.Specificity()
		if ri != rj {
			return ri > rj
		}
		ci, _ := kept[i].Confidence.Rank()
		cj, _ := kept[j].Confidence.Rank()
		if ci != cj {
			return ci > cj
		}
		return kept[i].RecordID < kept[j].RecordID
	})
	if q.Limit > 0 && len(kept) > q.Limit {
		kept = kept[:q.Limit]
	}
	for _, r := range kept {
		rank, _ := r.Scope.Principal.Specificity()
		evidence.Selections = append(evidence.Selections, Selection{
			RecordID: r.RecordID, VersionID: r.VersionID, Scope: r.Scope.String(),
			Sensitivity: string(r.Sensitivity), Purpose: string(r.Purpose),
			EvidenceClass: string(r.EvidenceClass), Confidence: string(r.Confidence),
			Reason: fmt.Sprintf("scope specificity %d (%s), confidence %s, no semantic ranking in %s",
				rank, r.Scope.Principal, r.Confidence, RetrievalVersion),
		})
	}
	return kept, evidence, nil
}

// Receipts renders the governed receipts for a set of retrieved records,
// stamped with the effective config SHA of the presentation that carries them.
//
// The SHA is a parameter because it identifies the blueprint doing the
// presenting, which a store cannot know. It is stamped on governed receipts too,
// not only frozen ones: ADR-0021 leaves RecordID empty for frozen memory and
// requires it for governed records, and neither statement says a governed
// receipt may omit the config version. Recording both means an audit can ask
// which blueprint presented which record version, rather than only one of the
// two.
func Receipts(records []Record, effectiveConfigSHA string) []contextprep.MemoryReceipt {
	out := make([]contextprep.MemoryReceipt, 0, len(records))
	for _, r := range records {
		receipt := r.Receipt()
		receipt.EffectiveConfigSHA = effectiveConfigSHA
		out = append(out, receipt)
	}
	return out
}

// Text renders retrieved records as the memory body a context assembler places
// on the memory channel.
//
// Rendered here so that every assembler gets the same bytes. ADR-0025 was
// written because three assemblers disagreed about the memory channel while one
// test asserted the mapping against hand-built literals; the same shape would
// recur if each assembler formatted retrieved records its own way, and the
// content digest in every receipt would then describe text that only one of
// them produced.
//
// Each record is labelled with its record ID. That is legibility, not a
// security boundary — ADR-0020 discarded delimiter marking for exactly this
// reason, and the guarantee comes from the user role the assembler uses, never
// from this text.
func Text(records []Record) string {
	if len(records) == 0 {
		return ""
	}
	var b strings.Builder
	for i, r := range records {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("- [")
		b.WriteString(r.RecordID)
		b.WriteString("] ")
		b.WriteString(strings.TrimSpace(r.Body))
		b.WriteString("\n")
	}
	return b.String()
}

// JSON renders the retrieval evidence for a log or an inspection command.
func (r Retrieval) JSON() (string, error) {
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode retrieval evidence: %w", err)
	}
	return string(body), nil
}
