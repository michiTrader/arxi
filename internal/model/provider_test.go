package model

import (
	"encoding/json"
	"strings"
	"testing"
)

// The §20.1 invocation: a known provider, no --base-url, no --api-key-env.
// If this needs flags, the documented first command a user types does not work.
func TestAKnownProviderNeedsNothingButItsName(t *testing.T) {
	p, err := New("anthropic", "", "", "")
	if err != nil {
		t.Fatalf("provider add anthropic: %v", err)
	}
	if p.BaseURL == "" {
		t.Error("no base URL was filled in, so there is nothing to call")
	}
	if p.APIKeyEnv != "ANTHROPIC_API_KEY" {
		t.Errorf("api key env is %q, want ANTHROPIC_API_KEY: docs/design 20.1 prints "+
			"exactly that variable", p.APIKeyEnv)
	}
	if len(p.Models) == 0 {
		t.Fatal("no models: `model list` straight after `provider add` would be " +
			"empty, and the user's next command would be to look up an endpoint " +
			"in someone else's documentation")
	}
}

// Exactly one model is enabled on registration, and it is the first.
// Enabling all of them would leave the most expensive model in the tree one
// --model typo away from being billed.
func TestOnlyOneModelIsEnabledOnRegistration(t *testing.T) {
	p, err := New("anthropic", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	on := 0
	for _, m := range p.Models {
		if m.Enabled {
			on++
		}
	}
	if on != 1 {
		t.Errorf("%d models enabled, want 1: every model enabled by default puts "+
			"the most expensive one a typo away from the invoice", on)
	}
	if !p.Models[0].Enabled {
		t.Errorf("the enabled model is not the first (%v)", p.Models)
	}
}

// An unknown provider with no endpoint is refused rather than guessed. There is
// no convention that turns a vendor name into a hostname.
func TestAnUnknownProviderWithoutAnEndpointIsRefused(t *testing.T) {
	_, err := New("together", "", "", "")
	if err == nil {
		t.Fatal("accepted: the provider would look registered and fail on the " +
			"first call with a DNS error nobody can trace back to this command")
	}
	if !strings.Contains(err.Error(), "--base-url") {
		t.Errorf("the error does not name the flag that fixes it: %v", err)
	}
	// The alternatives have to be listed, or the user guesses.
	if !strings.Contains(err.Error(), "anthropic") {
		t.Errorf("the error does not list the known providers: %v", err)
	}
}

// An unknown provider WITH an endpoint is accepted, and arrives with no models.
// Inventing rows for an endpoint this build has never called would print a
// `model list` that lies.
func TestAnUnknownProviderWithAnEndpointIsAcceptedAndEmpty(t *testing.T) {
	p, err := New("together", "https://api.together.xyz/v1", "", "")
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	if len(p.Models) != 0 {
		t.Errorf("invented %d models for an endpoint this build has never called: %v",
			len(p.Models), p.Models)
	}
	if p.APIKeyEnv != "TOGETHER_API_KEY" {
		t.Errorf("api key env is %q: the naming convention should apply to an "+
			"unknown provider too, or the user has to invent one", p.APIKeyEnv)
	}
}

// THE security test. `--api-key-env sk-ant-api03-...` is one keystroke of
// misunderstanding away from the correct command, and it succeeds silently,
// writes the secret to a file in the working directory, and the next commit
// publishes it.
func TestAKeyPassedWhereAVariableNameBelongsIsRefused(t *testing.T) {
	secrets := []string{
		"sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789",
		"sk_live_abcdefghijklmnop",
		"Bearer abcdefghijklmnopqrstuvwxyz",
		"api-key-abcdefghijklmnopqrst",
		// No recognised prefix at all: caught by shape, because prefix matching
		// alone passes every provider that does not use one, and the cost of a
		// miss is a published secret.
		"abcdefGHIJKLmnopqrSTUVwxyz0123456789abcdefghij",
	}
	for _, s := range secrets {
		if _, err := New("anthropic", "", s, ""); err == nil {
			t.Errorf("--api-key-env %q was accepted: that secret would be written "+
				"to providers/anthropic.json and committed", s)
		} else if !strings.Contains(err.Error(), "looks like the key itself") &&
			!strings.Contains(err.Error(), "not valid in an environment variable") {
			t.Errorf("--api-key-env %q refused for the wrong reason: %v", s, err)
		}
	}
}

// The other direction: a legitimate variable name must not be mistaken for a
// secret. A guard that refuses valid input is a guard that gets deleted.
func TestARealVariableNameIsAccepted(t *testing.T) {
	for _, env := range []string{
		"ANTHROPIC_API_KEY", "MY_KEY", "_KEY", "OPENAI_KEY_2",
		// Long but upper case: a variable name, not a key. The shape test must
		// not fire on it.
		"VERY_LONG_BUT_ENTIRELY_LEGITIMATE_VARIABLE_NAME_HERE",
	} {
		if _, err := New("anthropic", "", env, ""); err != nil {
			t.Errorf("--api-key-env %q refused: %v", env, err)
		}
	}
}

// Plain http to a remote host puts the API key on the wire in clear text.
func TestPlainHTTPToARemoteHostIsRefused(t *testing.T) {
	_, err := New("together", "http://api.together.xyz/v1", "", "")
	if err == nil {
		t.Fatal("accepted: the API key travels in the request, so it would cross " +
			"somebody's wifi in clear text")
	}
	if !strings.Contains(err.Error(), "clear text") {
		t.Errorf("the error does not say what the risk is: %v", err)
	}
}

// ...but plain http to localhost is allowed. A local model server is the one
// provider that can be exercised end to end with no credential and no bill, so
// refusing it would make the only free, testable provider unusable.
func TestPlainHTTPToLocalhostIsAllowed(t *testing.T) {
	for _, url := range []string{
		"http://localhost:11434/v1",
		"http://127.0.0.1:8080/v1",
		"http://[::1]:8080/v1",
	} {
		if _, err := New("local", url, "", ""); err != nil {
			t.Errorf("%s refused: there is no network to eavesdrop on: %v", url, err)
		}
	}
}

// A host that merely BEGINS with localhost is remote. "localhost.evil.com"
// resolves to whatever its owner wants.
func TestAHostThatOnlyLooksLikeLocalhostIsRemote(t *testing.T) {
	if _, err := New("x", "http://localhost.evil.com/v1", "", ""); err == nil {
		t.Fatal("localhost.evil.com accepted as loopback: it resolves to whatever " +
			"its owner points it at, and the key would be sent there in clear text")
	}
}

// A provider name becomes a filename, so a separator would escape the store
// directory or hide the file.
func TestAProviderNameThatWouldEscapeTheStoreIsRefused(t *testing.T) {
	for _, name := range []string{"../etc", "a/b", ".hidden", "with space", "Anthropic"} {
		if _, err := New(name, "https://x.test/v1", "", ""); err == nil {
			// New lowercases, so "Anthropic" is fine through New; check Validate
			// directly for that one.
			p := Provider{Name: name, BaseURL: "https://x.test/v1"}
			if err := p.Validate(); err == nil {
				t.Errorf("provider name %q accepted: names become filenames", name)
			}
		}
	}
}

// SetEnabled reports whether it changed anything, so `model enable` can say
// "already enabled" instead of printing a success that did nothing.
func TestEnablingAnAlreadyEnabledModelReportsNoChange(t *testing.T) {
	p, err := New("anthropic", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	id := p.Models[0].ID

	changed, err := p.SetEnabled(id, true)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Errorf("%s was already enabled but SetEnabled reported a change: the "+
			"command would print a success it did not perform", id)
	}

	changed, err = p.SetEnabled(id, false)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("disabling an enabled model reported no change")
	}
	if p.Models[0].Enabled {
		t.Error("the flag did not actually move")
	}
}

// A model that does not exist is an error, not a silent no-op, and the error
// points at the command that lists the real ones.
func TestEnablingAModelThatDoesNotExistIsAnError(t *testing.T) {
	p, err := New("anthropic", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.SetEnabled("claude-sonnet-4-5", true)
	if err == nil {
		t.Fatal("accepted a model the provider does not offer")
	}
	if !strings.Contains(err.Error(), "model list") {
		t.Errorf("the error does not say how to see the real ids: %v", err)
	}
}

// A duplicate id inside one provider is refused: SetEnabled would change the
// first and `model list` would print the second, so the status would never
// appear to move.
func TestADuplicateModelIDIsRefused(t *testing.T) {
	p := Provider{
		Name: "x", BaseURL: "https://x.test/v1",
		Models: []Model{{ID: "m", Enabled: true}, {ID: "m"}},
	}
	if err := p.Validate(); err == nil {
		t.Fatal("accepted: enabling m would change one entry while `model list` " +
			"printed the other")
	}
}

// The stored form must round-trip, because it is written by one command and
// read by another. And api_key must not be a field, ever.
func TestTheStoredFormRoundTripsAndHasNoKeyField(t *testing.T) {
	p, err := New("anthropic", "", "", "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	body, err := p.Encode()
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(body), `"api_key"`) {
		t.Fatal("the stored form has an api_key field: a secret in this file is a " +
			"secret in the repository")
	}

	var back Provider
	if err := json.Unmarshal(body, &back); err != nil {
		t.Fatal(err)
	}
	if back.Name != p.Name || back.BaseURL != p.BaseURL || back.APIKeyEnv != p.APIKeyEnv {
		t.Errorf("round trip lost fields:\n got %+v\nwant %+v", back, p)
	}
	if len(back.Models) != len(p.Models) {
		t.Errorf("round trip lost models: %d vs %d", len(back.Models), len(p.Models))
	}
	if !strings.HasSuffix(string(body), "\n") {
		t.Error("no trailing newline: `cat` would run the next prompt into the brace")
	}
}

// A trailing slash on the endpoint is trimmed, because the caller joins paths
// onto it and "…/v1//chat/completions" is a 404 on some servers and a redirect
// on others.
func TestATrailingSlashOnTheEndpointIsTrimmed(t *testing.T) {
	p, err := New("together", "https://api.together.xyz/v1/", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(p.BaseURL, "/") {
		t.Errorf("base URL is %q: joining a path onto it yields a double slash", p.BaseURL)
	}
}

// The default variable name has to be exportable by a shell, so a dash in the
// provider name cannot survive into it.
func TestTheDefaultVariableNameIsExportable(t *testing.T) {
	got := DefaultKeyEnv("azure-openai")
	if strings.Contains(got, "-") {
		t.Errorf("DefaultKeyEnv(azure-openai) = %q: a dash is not exportable in "+
			"POSIX sh", got)
	}
	if got != "AZURE_OPENAI_API_KEY" {
		t.Errorf("DefaultKeyEnv(azure-openai) = %q, want AZURE_OPENAI_API_KEY", got)
	}
}

// TestALoopbackProviderNeedsNoCredentialVariable pins a bug found by RUNNING the
// binary, not by reading it.
//
// `provider add local` used to assign $LOCAL_API_KEY, and the run then refused
// because that variable was empty. The effect was backwards: the one provider
// that can be exercised with no key and no bill became the only one unusable
// without inventing a meaningless secret. There is no network to read a key off
// on loopback, which is the whole reason plain http is permitted there.
func TestALoopbackProviderNeedsNoCredentialVariable(t *testing.T) {
	p, err := New("local", "", "", "")
	if err != nil {
		t.Fatalf("registering the local provider failed: %v", err)
	}
	if p.APIKeyEnv != "" {
		t.Errorf("the local provider wants $%s. It is served on loopback, so there is "+
			"no credential to send and no network to read one off; demanding a variable "+
			"makes the only free, testable provider the only unusable one", p.APIKeyEnv)
	}
}

// TestAnExplicitKeyVariableIsHonouredEvenOnLoopback is the opposite direction.
// Some local servers do check a token, and dropping the name the user gave would
// make those unusable instead -- trading one broken case for another.
func TestAnExplicitKeyVariableIsHonouredEvenOnLoopback(t *testing.T) {
	p, err := New("local", "", "MY_LOCAL_TOKEN", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.APIKeyEnv != "MY_LOCAL_TOKEN" {
		t.Errorf("APIKeyEnv = %q, expected MY_LOCAL_TOKEN. A name the user typed must "+
			"survive, or a local server that does check a token cannot be used", p.APIKeyEnv)
	}
}

// TestARemoteProviderStillGetsADefaultKeyVariable guards the fix from going too
// far. A hosted endpoint needs a credential, and silently dropping the default
// would move the failure from `provider add` (immediate, free) to the first turn
// of a run (late, after a run directory exists).
func TestARemoteProviderStillGetsADefaultKeyVariable(t *testing.T) {
	p, err := New("anthropic", "", "", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.APIKeyEnv != "ANTHROPIC_API_KEY" {
		t.Errorf("APIKeyEnv = %q, expected ANTHROPIC_API_KEY; a hosted provider needs a "+
			"credential and the default is what makes `provider add anthropic` enough",
			p.APIKeyEnv)
	}
}

// TestLoopbackOverHTTPSAlsoNeedsNoVariable is the case that made
// isLoopbackAnyScheme a separate function.
//
// isLoopback answers the narrower, dangerous question ("may a credential cross
// this plain-http link"), so it strips only http:// and calls everything else
// remote. Reusing it here would judge https://127.0.0.1 remote and hand it a
// credential requirement it does not need -- while widening isLoopback itself
// would widen the set of URLs the plain-http rule considers safe, which is the
// rule keeping a key off a network.
func TestLoopbackOverHTTPSAlsoNeedsNoVariable(t *testing.T) {
	for _, url := range []string{
		"https://127.0.0.1:8443/v1",
		"https://localhost:8443/v1",
		"http://[::1]:11434/v1",
	} {
		p, err := New("mylocal", url, "", "")
		if err != nil {
			t.Fatalf("New(%q): %v", url, err)
		}
		if p.APIKeyEnv != "" {
			t.Errorf("%s wants $%s; it points at this machine", url, p.APIKeyEnv)
		}
	}
}

// TestAHostThatMerelyLooksLocalStillGetsAVariable: localhost.evil.com resolves
// to whatever its owner points it at, so treating it as local would send a
// credential to a stranger -- and here the symptom would be the opposite of
// alarming, a provider that quietly needs no key.
func TestAHostThatMerelyLooksLocalStillGetsAVariable(t *testing.T) {
	p, err := New("sneaky", "https://localhost.evil.com/v1", "", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.APIKeyEnv == "" {
		t.Error("localhost.evil.com was treated as this machine; it resolves to whatever " +
			"its owner points it at, so a credential would be sent to a stranger")
	}
}
