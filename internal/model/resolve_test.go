package model

import (
	"strings"
	"testing"
)

func mustNew(t *testing.T, name, baseURL string) Provider {
	t.Helper()
	p, err := New(name, baseURL, "", "")
	if err != nil {
		t.Fatalf("New(%q): %v", name, err)
	}
	return p
}

// The ordinary case: a bare id, one provider, enabled.
func TestABareIDResolvesToTheProviderThatOffersIt(t *testing.T) {
	ps := []Provider{mustNew(t, "anthropic", "")}
	got, err := Resolve(ps, ps[0].Models[0].ID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Provider != "anthropic" {
		t.Errorf("provider is %q, want anthropic", got.Provider)
	}
	if got.BaseURL == "" || got.APIKeyEnv == "" {
		t.Errorf("resolution is missing what a call needs: %+v", got)
	}
}

// A disabled model does not resolve. `model disable` is listed in §20.11 as an
// operator decision, usually about cost; a resolver that ignored the flag would
// make the command decorative and bill for the model anyway.
func TestADisabledModelDoesNotResolve(t *testing.T) {
	p := mustNew(t, "anthropic", "")
	var disabled string
	for _, m := range p.Models {
		if !m.Enabled {
			disabled = m.ID
			break
		}
	}
	if disabled == "" {
		t.Fatal("the fixture has no disabled model, so this test proves nothing")
	}

	_, err := Resolve([]Provider{p}, disabled)
	if err == nil {
		t.Fatalf("%s resolved while disabled: the operator turned it off, usually "+
			"because of what it costs", disabled)
	}
	if !strings.Contains(err.Error(), "model enable") {
		t.Errorf("the error does not say how to turn it on: %v", err)
	}
}

// THE ambiguity refusal. Two providers offering the same id must not be
// silently disambiguated: picking the first alphabetically means that adding a
// provider months later reroutes every existing agent to a different provider
// at a different price, with nothing in the log to say so.
func TestAModelTwoProvidersOfferIsRefusedRatherThanChosen(t *testing.T) {
	local := mustNew(t, "local", "http://localhost:11434/v1")
	// A local server serving a vendor id under its own name is the realistic
	// shape of this collision, not a contrived one.
	local.Models = []Model{{ID: "claude-sonnet-4-6", Enabled: true}}

	ps := []Provider{mustNew(t, "anthropic", ""), local}

	_, err := Resolve(ps, "claude-sonnet-4-6")
	if err == nil {
		t.Fatal("resolved: which provider gets billed was decided by sort order")
	}
	if !strings.Contains(err.Error(), "anthropic/claude-sonnet-4-6") {
		t.Errorf("the error does not show the qualified spelling that fixes it: %v", err)
	}
}

// ...and the qualified spelling resolves it.
func TestAQualifiedRefResolvesAnAmbiguousID(t *testing.T) {
	local := mustNew(t, "local", "http://localhost:11434/v1")
	local.Models = []Model{{ID: "claude-sonnet-4-6", Enabled: true}}
	ps := []Provider{mustNew(t, "anthropic", ""), local}

	got, err := Resolve(ps, "local/claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Provider != "local" {
		t.Errorf("resolved to %q, want local: the ref named it explicitly", got.Provider)
	}
	if !strings.HasPrefix(got.BaseURL, "http://localhost") {
		t.Errorf("base URL is %q: it came from the wrong provider", got.BaseURL)
	}
}

// A typo gets the id it probably meant, because the failure this replaces costs
// the user another command to discover that sonnet-4-5 is sonnet-4-6.
func TestATypoIsToldWhatItProbablyMeant(t *testing.T) {
	ps := []Provider{mustNew(t, "anthropic", "")}
	_, err := Resolve(ps, "claude-sonnet-4-5")
	if err == nil {
		t.Fatal("a model that does not exist resolved")
	}
	if !strings.Contains(err.Error(), "claude-sonnet-4-6") {
		t.Errorf("no suggestion for an obvious typo: %v", err)
	}
}

// The suggestion must NOT fire across families. "did you mean gpt-5.1?" to
// somebody who typed a Claude id reads as a claim that the two are
// interchangeable, which is worse than saying nothing.
func TestNoSuggestionIsMadeAcrossUnrelatedModels(t *testing.T) {
	ps := []Provider{mustNew(t, "openai", "")}
	_, err := Resolve(ps, "claude-sonnet-4-6")
	if err == nil {
		t.Fatal("resolved a model no provider offers")
	}
	if strings.Contains(err.Error(), "Did you mean") {
		t.Errorf("suggested an unrelated model: %v", err)
	}
	// It must still list what IS available, or the user is left guessing.
	if !strings.Contains(err.Error(), "available:") {
		t.Errorf("the error does not list the real models: %v", err)
	}
}

// No providers at all is its own message: the user has not registered one yet,
// and "no model called x" would send them looking for a typo.
func TestNoProvidersAtAllSaysSoAndNamesTheFix(t *testing.T) {
	_, err := Resolve(nil, "claude-sonnet-4-6")
	if err == nil {
		t.Fatal("resolved against an empty registry")
	}
	if !strings.Contains(err.Error(), "provider add") {
		t.Errorf("the error does not name the command that fixes it: %v", err)
	}
}

// A provider registered by --base-url has no models, and that is not a typo on
// the user's part. Saying "did you mean" here would be wrong.
func TestAProviderWithNoModelsExplainsWhyItIsEmpty(t *testing.T) {
	ps := []Provider{mustNew(t, "together", "https://api.together.xyz/v1")}
	_, err := Resolve(ps, "llama-3-70b")
	if err == nil {
		t.Fatal("resolved against a provider with no models")
	}
	if !strings.Contains(err.Error(), "--base-url") {
		t.Errorf("the error does not explain why the provider is empty: %v", err)
	}
}

// A ref naming a provider that is not registered says so, rather than
// reporting the model as unknown. The two are different mistakes.
func TestAnUnregisteredProviderInARefIsNamed(t *testing.T) {
	ps := []Provider{mustNew(t, "anthropic", "")}
	_, err := Resolve(ps, "openai/gpt-5.1")
	if err == nil {
		t.Fatal("resolved through a provider that is not registered")
	}
	if !strings.Contains(err.Error(), "no provider called") {
		t.Errorf("the error blames the model instead of the provider: %v", err)
	}
}

// An empty ref is its own error. A run with no model is not a typo.
func TestAnEmptyRefIsRefused(t *testing.T) {
	ps := []Provider{mustNew(t, "anthropic", "")}
	for _, ref := range []string{"", "   ", "anthropic/"} {
		if _, err := Resolve(ps, ref); err == nil {
			t.Errorf("ref %q resolved", ref)
		}
	}
}

// `model list` prints enabled models first inside a provider, because the
// question the user arrived with is "what can I use right now", and sorting
// purely by id buries the two usable models among a dozen that are off.
func TestModelListPutsUsableModelsFirst(t *testing.T) {
	rows := Rows([]Provider{mustNew(t, "anthropic", "")})
	if len(rows) < 2 {
		t.Fatal("not enough rows to test ordering")
	}
	if !rows[0].Enabled {
		t.Errorf("the first row is disabled (%+v): the models a user can actually "+
			"use are buried", rows[0])
	}
	seenDisabled := false
	for _, r := range rows {
		if !r.Enabled {
			seenDisabled = true
		} else if seenDisabled {
			t.Errorf("an enabled model (%s) sorts after a disabled one", r.Name)
		}
	}
}

// Providers are ordered by name, so `model list` does not reshuffle between
// invocations. Directory read order is not stable.
func TestModelListIsOrderedByProvider(t *testing.T) {
	rows := Rows([]Provider{
		mustNew(t, "openai", ""),
		mustNew(t, "anthropic", ""),
	})
	if rows[0].Provider != "anthropic" {
		t.Errorf("first provider is %q, want anthropic: an unordered list "+
			"reshuffles between invocations", rows[0].Provider)
	}
}

// The status column has to be the words §20.1 prints, or the doc and the binary
// disagree on the only output of this command.
func TestTheStatusColumnUsesTheDocumentedWords(t *testing.T) {
	if got := (Row{Enabled: true}).Status(); got != "enabled" {
		t.Errorf("status is %q, want enabled", got)
	}
	if got := (Row{}).Status(); got != "disabled" {
		t.Errorf("status is %q, want disabled", got)
	}
}

// ParseRef accepts both spellings, and a bare id must not be mistaken for a
// provider.
func TestParseRefAcceptsBothSpellings(t *testing.T) {
	if p, id := ParseRef("claude-sonnet-4-6"); p != "" || id != "claude-sonnet-4-6" {
		t.Errorf("bare id parsed as provider=%q id=%q", p, id)
	}
	if p, id := ParseRef("Anthropic/claude-sonnet-4-6"); p != "anthropic" || id != "claude-sonnet-4-6" {
		t.Errorf("qualified ref parsed as provider=%q id=%q; the provider half "+
			"must be lowercased because provider names are", p, id)
	}
}
