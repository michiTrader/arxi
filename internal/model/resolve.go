package model

import (
	"fmt"
	"sort"
	"strings"
)

// Resolution is a model that may be called, together with everything needed to
// call it.
//
// It carries the provider's fields by value rather than a pointer to the
// Provider, because the caller is about to make an HTTP request and must not be
// holding a handle to a record another command may be rewriting.
type Resolution struct {
	Provider  string
	Model     string
	BaseURL   string
	APIKeyEnv string
}

// Ref is how a model is named on a command line or in a blueprint: either a
// bare id ("claude-sonnet-4-6") or a qualified one ("anthropic/claude-sonnet-4-6").
//
// Both spellings are accepted because the bare one is what a user types and the
// qualified one is what they need the moment two providers offer the same id —
// which happens in practice, since a local server can serve a model under the
// vendor's own name.
func ParseRef(ref string) (provider, id string) {
	ref = strings.TrimSpace(ref)
	if i := strings.Index(ref, "/"); i >= 0 {
		return strings.ToLower(ref[:i]), ref[i+1:]
	}
	return "", ref
}

// Resolve finds the model a ref names and refuses every case where calling it
// would be a guess.
//
// This is the gate the live executor consults before spending money, and each
// refusal below is a distinct way a run could otherwise cost money and produce
// nothing:
//
//   - Not found: a typo. Failing here costs a message; proceeding would send a
//     request that the provider bills for the tokens it read before rejecting
//     the model name.
//   - Ambiguous: two providers offer the id. Picking one — say, the first
//     alphabetically — would route the run to whichever provider happened to
//     sort first, so adding a provider months later silently reroutes every
//     existing agent to a different model at a different price.
//   - Disabled: the operator turned it off. §20.11 lists `model disable` as an
//     operator decision precisely so it can withhold an expensive model, and a
//     resolver that ignored the flag would make the command decorative.
func Resolve(ps []Provider, ref string) (Resolution, error) {
	wantProvider, wantID := ParseRef(ref)

	if strings.TrimSpace(wantID) == "" {
		return Resolution{}, fmt.Errorf("no model named: a run needs a model to " +
			"call.\n  see what is available: iash model list")
	}

	if len(ps) == 0 {
		return Resolution{}, fmt.Errorf("no providers are registered, so %q cannot "+
			"be resolved to anything callable.\n"+
			"  register one first: iash provider add anthropic --api-key-env "+
			"ANTHROPIC_API_KEY", ref)
	}

	type hit struct {
		p Provider
		m Model
	}
	var hits []hit
	for _, p := range ps {
		if wantProvider != "" && p.Name != wantProvider {
			continue
		}
		for _, m := range p.Models {
			if m.ID == wantID {
				hits = append(hits, hit{p, m})
			}
		}
	}

	switch len(hits) {
	case 0:
		return Resolution{}, notFound(ps, wantProvider, wantID, ref)
	case 1:
		// fall through
	default:
		names := make([]string, 0, len(hits))
		for _, h := range hits {
			names = append(names, h.p.Name+"/"+h.m.ID)
		}
		sort.Strings(names)
		return Resolution{}, fmt.Errorf("model %q is offered by %d providers (%s), "+
			"so which one to bill is not decided.\n"+
			"  name it in full: --model %s",
			wantID, len(hits), strings.Join(names, ", "), names[0])
	}

	h := hits[0]
	if !h.m.Enabled {
		return Resolution{}, fmt.Errorf("model %s/%s is disabled, so this run will "+
			"not call it.\n"+
			"  a disabled model is an operator decision, usually about cost; if it "+
			"is meant to be used:\n    iash model enable %s",
			h.p.Name, h.m.ID, h.m.ID)
	}

	return Resolution{
		Provider:  h.p.Name,
		Model:     h.m.ID,
		BaseURL:   h.p.BaseURL,
		APIKeyEnv: h.p.APIKeyEnv,
	}, nil
}

// notFound builds the "no such model" error, and names the model that was
// probably meant.
//
// The suggestion is worth the code because the failure mode it replaces is
// expensive in attention: a user reading "unknown model claude-sonnet-4-5" with
// no list has to go and run another command to discover the id is
// claude-sonnet-4-6.
func notFound(ps []Provider, wantProvider, wantID, ref string) error {
	if wantProvider != "" {
		found := false
		for _, p := range ps {
			if p.Name == wantProvider {
				found = true
			}
		}
		if !found {
			names := make([]string, 0, len(ps))
			for _, p := range ps {
				names = append(names, p.Name)
			}
			sort.Strings(names)
			return fmt.Errorf("no provider called %q is registered (registered: %s)",
				wantProvider, strings.Join(names, ", "))
		}
	}

	var all []string
	for _, p := range ps {
		if wantProvider != "" && p.Name != wantProvider {
			continue
		}
		for _, m := range p.Models {
			all = append(all, p.Name+"/"+m.ID)
		}
	}
	sort.Strings(all)

	if len(all) == 0 {
		return fmt.Errorf("no model called %q, and no provider offers any models "+
			"at all.\n  a provider registered with --base-url starts empty, "+
			"because this build cannot know what that endpoint serves", ref)
	}

	msg := fmt.Sprintf("no model called %q", ref)
	if near := nearest(wantID, all); near != "" {
		msg += fmt.Sprintf(". Did you mean %s?", near)
	}
	return fmt.Errorf("%s\n  available: %s", msg, strings.Join(all, ", "))
}

// nearest returns the closest available id within a small edit distance, or "".
//
// Bounded at three edits and only for candidates of similar length, so it
// suggests a typo and not an unrelated model. An unbounded nearest-neighbour
// would answer "did you mean gpt-5.1?" to somebody who typed a Claude id, which
// is worse than saying nothing: it reads as a claim that the two are
// interchangeable.
func nearest(want string, candidates []string) string {
	best, bestD := "", 4
	for _, c := range candidates {
		id := c
		if i := strings.Index(c, "/"); i >= 0 {
			id = c[i+1:]
		}
		if abs(len(id)-len(want)) > 3 {
			continue
		}
		if d := distance(want, id); d < bestD {
			best, bestD = c, d
		}
	}
	return best
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// distance is Levenshtein over bytes.
//
// Bytes and not runes: model ids are ASCII by convention across every provider,
// and a rune implementation would be more code for a case that does not arise.
// It uses two rows rather than a full matrix because the only value wanted is
// the last cell.
func distance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

// Rows renders every model of every provider in the order `model list` prints
// them: by provider, then by id, with the enabled ones first inside a provider.
//
// Enabled first because that is the answer to the question the user came with —
// "what can I use right now" — and a list sorted purely by id buries the two
// usable models among a dozen that are off.
func Rows(ps []Provider) []Row {
	SortProviders(ps)
	var out []Row
	for _, p := range ps {
		ms := append([]Model(nil), p.Models...)
		sort.SliceStable(ms, func(i, j int) bool {
			if ms[i].Enabled != ms[j].Enabled {
				return ms[i].Enabled
			}
			return ms[i].ID < ms[j].ID
		})
		for _, m := range ms {
			out = append(out, Row{Name: m.ID, Provider: p.Name, Enabled: m.Enabled})
		}
	}
	return out
}

// Row is one line of `model list`.
type Row struct {
	Name     string
	Provider string
	Enabled  bool
}

// Status is the word `model list` prints in its STATUS column.
func (r Row) Status() string {
	if r.Enabled {
		return "enabled"
	}
	return "disabled"
}
