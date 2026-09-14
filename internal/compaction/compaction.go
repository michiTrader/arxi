// Package compaction produces the verified lossy artifact that lets a
// presentation shed material without losing the account of what it shed.
//
// The canonical transcript is never touched here. A compaction artifact only
// ever describes, by item identity, what the presentation keeps verbatim, what
// it cites through extractive claims, and what it omits. The extractive rule
// is load-bearing: every claim must be contained in the exact text of the
// items it cites, so a committed artifact cannot contain a fact its sources
// do not.
package compaction

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/michiTrader/arxi/internal/transcript"
	"github.com/michiTrader/arxi/internal/turn"
)

const (
	// Schema names the versioned compaction artifact.
	Schema = "arxi.compaction/v1"
	// GeneratorName identifies the deterministic extractive generator.
	GeneratorName = "arxi.compactor-extractive"
	// GeneratorVersion advances only when selection behavior changes.
	GeneratorVersion = "v1"
	// BudgetSchema names the versioned layer-budget policy.
	BudgetSchema = "arxi.context-budget/v1"
	// BudgetVersion advances only when the derivation changes.
	BudgetVersion = "v1"

	contentDomain      = "arxi.compaction-content/v1"
	omissionTextDomain = "arxi.compaction-omission/v1"
)

// Budgets is the versioned allocation policy the compactor obeys. Values are
// recorded inside every artifact that was produced under them, so an audit can
// always tell which policy shaped a summary.
type Budgets struct {
	Schema     string `json:"schema"`
	Version    string `json:"version"`
	InputLimit int    `json:"input_limit"`
	Static     int    `json:"static_tokens"`
	Summary    int    `json:"summary_tokens"`
	Verbatim   int    `json:"verbatim_tokens"`
	Input      int    `json:"input_tokens"`
}

// DeriveBudgets splits a known input limit into fixed quarters by integer
// division: static 1/4, summary 1/8, verbatim 1/2, input 1/8. The floored
// remainder stays as headroom, so the allocations never exceed the limit they
// came from. The derivation is deterministic and versioned because every
// committed artifact records the values it obeyed.
func DeriveBudgets(inputLimit int) Budgets {
	return Budgets{Schema: BudgetSchema, Version: BudgetVersion, InputLimit: inputLimit,
		Static: inputLimit / 4, Summary: inputLimit / 8, Verbatim: inputLimit / 2, Input: inputLimit / 8}
}

// Claim is one extractive summary statement. Text must be contained in the
// concatenated text of exactly the cited items; a claim shortened to fit the
// summary budget is labelled Incomplete rather than silently cut.
type Claim struct {
	Text       string   `json:"text"`
	Items      []string `json:"items"`
	Incomplete bool     `json:"incomplete,omitempty"`
}

// Range records one consecutive run of summarized transcript material by
// source sequence and boundary item IDs.
type Range struct {
	FromSeq     int64  `json:"from_seq"`
	ThroughSeq  int64  `json:"through_seq"`
	FromItem    string `json:"from_item"`
	ThroughItem string `json:"through_item"`
}

// Omission records one item the presentation dropped without citing it. The
// content digest binds what the item contained, because the item ID alone does
// not cover content.
type Omission struct {
	ItemID        string `json:"item_id"`
	Kind          string `json:"kind"`
	ContentDigest string `json:"content_digest"`
}

// Artifact is the verified lossy record. Window, Retained, claim citations
// and Omissions must partition the transcript items; Verify enforces that
// partition before the artifact may commit.
type Artifact struct {
	Schema           string     `json:"schema"`
	ContextID        string     `json:"context_id"`
	RunID            string     `json:"run_id"`
	Subject          string     `json:"subject_agent"`
	SourceThroughSeq int64      `json:"source_through_seq"`
	Generator        string     `json:"generator"`
	GeneratorVersion string     `json:"generator_version"`
	Budgets          Budgets    `json:"budgets"`
	Claims           []Claim    `json:"claims"`
	Window           []string   `json:"window"`
	Ranges           []Range    `json:"ranges"`
	Retained         []string   `json:"retained,omitempty"`
	Omissions        []Omission `json:"omissions"`
	BeforeTokens     int        `json:"before_tokens"`
	AfterTokens      int        `json:"after_tokens"`
	ContentDigest    string     `json:"content_digest"`
}

// Request names one compaction commission: the projected transcript items and
// the budget policy the generator must obey.
type Request struct {
	ContextID        string
	RunID            string
	Subject          string
	SourceThroughSeq int64
	Budgets          Budgets
	Items            []transcript.Item
}

// Generator selects what a presentation keeps, cites and omits. Implementations
// must be pure functions of the request so the same inputs always yield the
// same artifact.
type Generator interface {
	Compact(req Request) (Artifact, error)
}

// Extractive is the default generator. It selects a recent verbatim window,
// cites continuity anchors with exact excerpts, and records everything else as
// omissions. It never paraphrases: a claim is a substring of its source.
type Extractive struct{}

// Compact selects the window, claims, ranges and omissions. It does not fill
// token accounting or the content digest: the caller measures the real
// presentations, sets BeforeTokens and AfterTokens, then Finalizes.
func (Extractive) Compact(req Request) (Artifact, error) {
	artifact := Artifact{Schema: Schema, ContextID: req.ContextID, RunID: req.RunID, Subject: req.Subject,
		SourceThroughSeq: req.SourceThroughSeq, Generator: GeneratorName, GeneratorVersion: GeneratorVersion,
		Budgets: req.Budgets, Claims: []Claim{}, Window: []string{}, Ranges: []Range{}, Omissions: []Omission{}}
	if req.Budgets.Schema != BudgetSchema || req.Budgets.Version != BudgetVersion {
		return Artifact{}, fmt.Errorf("budget policy %q/%q is not %s/%s: compacting under an unknown policy would make recorded budgets uninterpretable",
			req.Budgets.Schema, req.Budgets.Version, BudgetSchema, BudgetVersion)
	}
	if len(req.Items) == 0 {
		return Artifact{}, fmt.Errorf("compaction received no transcript items: an empty history never needs compaction and an empty artifact would prove nothing")
	}
	anchorAt := anchors(req.Items)
	cut := verbatimWindow(req.Items, req.Budgets.Verbatim)
	artifact.Window = itemIDs(req.Items[cut:])
	if count := len(anchorAt); count > req.Budgets.Summary {
		return Artifact{}, fmt.Errorf("%d continuity anchors exceed the summary budget of %d: anchors cannot be cited and cannot be dropped, so this limit cannot be compacted",
			count, req.Budgets.Summary)
	} else if count > 0 {
		share := req.Budgets.Summary / count
		for i := 0; i < cut; i++ {
			if !anchorAt[i] {
				continue
			}
			text, incomplete := excerpt(itemText(req.Items[i]), share)
			artifact.Claims = append(artifact.Claims, Claim{Text: text, Items: []string{req.Items[i].ID}, Incomplete: incomplete})
		}
	}
	presented := map[string]bool{}
	for _, id := range artifact.Window {
		presented[id] = true
	}
	for _, id := range artifact.Retained {
		presented[id] = true
	}
	artifact.Ranges = summarizedRanges(req.Items, presented)
	cited := map[string]bool{}
	for _, claim := range artifact.Claims {
		for _, id := range claim.Items {
			cited[id] = true
		}
	}
	for _, item := range req.Items {
		if presented[item.ID] || cited[item.ID] {
			continue
		}
		artifact.Omissions = append(artifact.Omissions, Omission{ItemID: item.ID, Kind: string(item.Kind),
			ContentDigest: itemContentDigest(item)})
	}
	return artifact, nil
}

// anchors marks the items continuity depends on: user inputs, human decisions
// and the final model output of each completed turn. An anchor is never
// silently dropped — it must end up inside the window or cited by a claim.
func anchors(items []transcript.Item) map[int]bool {
	result := map[int]bool{}
	for i, item := range items {
		switch item.Kind {
		case transcript.UserInput:
			result[i] = true
		case transcript.HumanDecision:
			if item.Decision != "" {
				result[i] = true
			}
		case transcript.ModelOutput:
			if i == len(items)-1 || items[i+1].Kind == transcript.UserInput {
				if hasText(item) {
					result[i] = true
				}
			}
		}
	}
	return result
}

// verbatimWindow returns the cut index of the longest item suffix that fits
// the verbatim budget, extended left whenever the cut would separate a tool
// result from its call: a presented result without its preceding call would be
// a conversation no provider accepts.
func verbatimWindow(items []transcript.Item, budget int) int {
	cut := len(items)
	spent := 0
	for cut > 0 {
		cost := len([]rune(itemText(items[cut-1])))
		if spent > 0 && spent+cost > budget {
			break
		}
		spent += cost
		cut--
	}
	for cut > 0 && cut < len(items) && items[cut].Kind == transcript.ToolResult &&
		items[cut].Result != nil && items[cut].Result.CallID != "" {
		found := -1
		for j := cut - 1; j >= 0; j-- {
			if items[j].Kind == transcript.ToolCall && items[j].Call != nil && items[j].Call.ID == items[cut].Result.CallID {
				found = j
				break
			}
		}
		if found < 0 {
			break
		}
		cut = found
	}
	return cut
}

// excerpt shortens text to at most budget runes, cutting at a sentence boundary
// when one exists and labelling the result incomplete either way. The excerpt
// is always an exact prefix of the source, which is what makes containment
// provable.
func excerpt(text string, budget int) (string, bool) {
	runes := []rune(text)
	if budget <= 0 {
		return "", true
	}
	if len(runes) <= budget {
		return text, false
	}
	cut := budget
	for i := budget - 1; i > 0; i-- {
		if r := runes[i]; r == '.' || r == '!' || r == '?' || r == '\n' {
			cut = i + 1
			break
		}
	}
	return string(runes[:cut]), true
}

// summarizedRanges groups the items the presentation drops into consecutive
// source ranges. Generator and verifier share this one derivation so their
// ranges can never disagree.
func summarizedRanges(items []transcript.Item, presented map[string]bool) []Range {
	var ranges []Range
	open := -1
	for i := range items {
		if presented[items[i].ID] {
			if open >= 0 {
				ranges = append(ranges, Range{FromSeq: items[open].SourceSeq, ThroughSeq: items[i-1].SourceSeq,
					FromItem: items[open].ID, ThroughItem: items[i-1].ID})
				open = -1
			}
			continue
		}
		if open < 0 {
			open = i
		}
	}
	if open >= 0 {
		ranges = append(ranges, Range{FromSeq: items[open].SourceSeq, ThroughSeq: items[len(items)-1].SourceSeq,
			FromItem: items[open].ID, ThroughItem: items[len(items)-1].ID})
	}
	if ranges == nil {
		ranges = []Range{}
	}
	return ranges
}

func hasText(item transcript.Item) bool {
	for _, block := range item.Content {
		if block.Type == turn.BlockText && strings.TrimSpace(block.Text) != "" {
			return true
		}
	}
	return false
}

// itemText renders the exact text a claim may be proven against. The verifier
// uses the same function, so the generator and the gate can never disagree
// about what a citation covers.
func itemText(item transcript.Item) string {
	var parts []string
	switch item.Kind {
	case transcript.UserInput, transcript.ModelOutput:
		for _, block := range item.Content {
			if block.Type == turn.BlockText {
				parts = append(parts, block.Text)
			}
		}
	case transcript.ToolCall:
		if item.Call != nil {
			parts = append(parts, item.Call.Name+" "+string(item.Call.Arguments))
		}
	case transcript.ToolResult:
		if item.Result != nil {
			for _, block := range item.Result.Content {
				if block.Type == turn.BlockText {
					parts = append(parts, block.Text)
				}
			}
		}
	case transcript.HumanDecision:
		parts = append(parts, item.Decision)
	}
	return strings.Join(parts, "\n")
}

func itemContentDigest(item transcript.Item) string {
	body, err := json.Marshal(struct {
		Content  []turn.ContentBlock `json:"content,omitempty"`
		Call     *turn.ToolCall      `json:"call,omitempty"`
		Result   *turn.ToolResult    `json:"result,omitempty"`
		Decision string              `json:"decision,omitempty"`
	}{item.Content, item.Call, item.Result, item.Decision})
	if err != nil {
		return ""
	}
	return digest(omissionTextDomain, body)
}

// Finalize sets the token accounting and computes the content digest over the
// selection. It must run after the caller has measured the real presentations,
// because before/after counts are claims about presented bytes, not about
// selected items.
func Finalize(artifact *Artifact) error {
	artifact.ContentDigest = ""
	body, err := json.Marshal(struct {
		ContextID        string     `json:"context_id"`
		RunID            string     `json:"run_id"`
		Subject          string     `json:"subject_agent"`
		SourceThroughSeq int64      `json:"source_through_seq"`
		Generator        string     `json:"generator"`
		GeneratorVersion string     `json:"generator_version"`
		Budgets          Budgets    `json:"budgets"`
		Claims           []Claim    `json:"claims"`
		Window           []string   `json:"window"`
		Ranges           []Range    `json:"ranges"`
		Retained         []string   `json:"retained,omitempty"`
		Omissions        []Omission `json:"omissions"`
		BeforeTokens     int        `json:"before_tokens"`
		AfterTokens      int        `json:"after_tokens"`
	}{artifact.ContextID, artifact.RunID, artifact.Subject, artifact.SourceThroughSeq, artifact.Generator,
		artifact.GeneratorVersion, artifact.Budgets, artifact.Claims, artifact.Window, artifact.Ranges,
		artifact.Retained, artifact.Omissions, artifact.BeforeTokens, artifact.AfterTokens})
	if err != nil {
		return fmt.Errorf("encode compaction content: %w", err)
	}
	artifact.ContentDigest = digest(contentDomain, body)
	return nil
}

// Verify is the gate an artifact must pass before it may exist. Every rule
// encodes one way a summary could lie: cite a source that does not contain the
// claim, drop an anchor, split a tool pair, or lose an omission from the
// ledger.
func Verify(artifact Artifact, items []transcript.Item) error {
	check := artifact
	if err := Finalize(&check); err != nil {
		return err
	}
	if check.ContentDigest != artifact.ContentDigest {
		return fmt.Errorf("compaction content digest %q disagrees with its bytes: the selection is not the one the accounting claims", artifact.ContentDigest)
	}
	if artifact.Schema != Schema {
		return fmt.Errorf("compaction schema %q, want %q: an unknown artifact shape would be read with the wrong rules", artifact.Schema, Schema)
	}
	byID := make(map[string]transcript.Item, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}
	cited := map[string]bool{}
	for _, claim := range artifact.Claims {
		if claim.Text == "" || len(claim.Items) == 0 {
			return fmt.Errorf("claim cites nothing: a claim without provenance is exactly the invented fact this gate exists to refuse")
		}
		var sources []string
		for _, id := range claim.Items {
			item, ok := byID[id]
			if !ok {
				return fmt.Errorf("claim cites unknown item %q: a citation that resolves to nothing proves nothing", id)
			}
			cited[id] = true
			sources = append(sources, itemText(item))
		}
		if !strings.Contains(strings.Join(sources, "\n"), claim.Text) {
			return fmt.Errorf("claim %q is not contained in its cited items: an unprovable claim is an invented fact and must not commit", claim.Text)
		}
	}
	inWindow := map[string]bool{}
	for _, id := range artifact.Window {
		if _, ok := byID[id]; !ok {
			return fmt.Errorf("window names unknown item %q", id)
		}
		inWindow[id] = true
	}
	retained := map[string]bool{}
	for _, id := range artifact.Retained {
		if _, ok := byID[id]; !ok {
			return fmt.Errorf("retained names unknown item %q", id)
		}
		retained[id] = true
	}
	if len(artifact.Window) > len(items) {
		return fmt.Errorf("window holds %d items over a transcript of %d: the window must be a suffix of the item order", len(artifact.Window), len(items))
	}
	for i, id := range artifact.Window {
		if items[len(items)-len(artifact.Window)+i].ID != id {
			return fmt.Errorf("window item %q is not a suffix of the item order: a reshuffled window would present history in an order the log does not support", id)
		}
	}
	for _, item := range items {
		if !inWindow[item.ID] || item.Kind != transcript.ToolResult || item.Result == nil || item.Result.CallID == "" {
			continue
		}
		paired := false
		for _, callID := range artifact.Window {
			if call, ok := byID[callID]; ok && call.Kind == transcript.ToolCall && call.Call != nil && call.Call.ID == item.Result.CallID {
				paired = true
				break
			}
		}
		if !paired {
			return fmt.Errorf("window presents tool result %q without its call: a result without its preceding call is a conversation no provider accepts", item.Result.CallID)
		}
	}
	presented := func(id string) bool { return inWindow[id] || retained[id] || cited[id] }
	for index, isAnchor := range anchors(items) {
		if isAnchor && !presented(items[index].ID) {
			return fmt.Errorf("anchor item %q is neither verbatim nor cited: dropping a goal, decision or final answer silently is the failure compaction exists to prevent", items[index].ID)
		}
	}
	var expected []Omission
	for _, item := range items {
		if presented(item.ID) {
			continue
		}
		expected = append(expected, Omission{ItemID: item.ID, Kind: string(item.Kind), ContentDigest: itemContentDigest(item)})
	}
	if expected == nil {
		expected = []Omission{}
	}
	if len(expected) != len(artifact.Omissions) {
		return fmt.Errorf("omission ledger holds %d entries, want %d: every unpresented item must be accounted for by identity or the summary hides material", len(artifact.Omissions), len(expected))
	}
	for i, omission := range expected {
		if artifact.Omissions[i] != omission {
			return fmt.Errorf("omission ledger entry %d = %+v, want %+v: the ledger must name exactly the items the presentation dropped", i, artifact.Omissions[i], omission)
		}
	}
	// Ranges cover the material presented neither verbatim nor through a
	// retained slot: exactly what the generator grouped, citation included.
	verbatimPresented := map[string]bool{}
	for _, item := range items {
		if inWindow[item.ID] || retained[item.ID] {
			verbatimPresented[item.ID] = true
		}
	}
	expectedRanges := summarizedRanges(items, verbatimPresented)
	if len(expectedRanges) != len(artifact.Ranges) {
		return fmt.Errorf("source ranges hold %d entries, want %d: ranges must cover exactly the summarized material", len(artifact.Ranges), len(expectedRanges))
	}
	for i, r := range expectedRanges {
		if artifact.Ranges[i] != r {
			return fmt.Errorf("source range %d = %+v, want %+v", i, artifact.Ranges[i], r)
		}
	}
	return nil
}

func itemIDs(items []transcript.Item) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

func digest(domain string, body []byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte(domain))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}
