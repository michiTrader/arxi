package modelstore

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/model"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), DefaultDir))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func add(t *testing.T, s *Store, name, baseURL string) model.Provider {
	t.Helper()
	p, err := model.New(name, baseURL, "", "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add(p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAProviderSurvivesARoundTrip(t *testing.T) {
	s := open(t)
	want := add(t, s, "anthropic", "")

	got, err := s.Load("anthropic")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.BaseURL != want.BaseURL || got.APIKeyEnv != want.APIKeyEnv {
		t.Errorf("round trip changed the provider:\n got %+v\nwant %+v", got, want)
	}
	if len(got.Models) != len(want.Models) {
		t.Fatalf("round trip lost models: %d vs %d", len(got.Models), len(want.Models))
	}
	for i := range got.Models {
		if got.Models[i] != want.Models[i] {
			t.Errorf("model %d changed: %+v vs %+v", i, got.Models[i], want.Models[i])
		}
	}
}

// Adding a provider that exists is refused. An overwrite would repoint every
// agent using it at a new endpoint and a new credential, and reset the enabled
// flags an operator chose.
func TestAddingAProviderTwiceIsRefused(t *testing.T) {
	s := open(t)
	p := add(t, s, "anthropic", "")

	err := s.Add(p)
	if err == nil {
		t.Fatal("accepted: the endpoint, the credential and every enabled flag " +
			"would have been silently replaced")
	}
	if !strings.Contains(err.Error(), "already registered") {
		t.Errorf("the error does not say what happened: %v", err)
	}
}

// A name that collides case-insensitively is refused, because on macOS and
// Windows the two are the same file: the add succeeds on Linux and destroys a
// credential pointer on a laptop, which is the machine with no tests on it.
func TestACaseCollidingNameIsRefused(t *testing.T) {
	s := open(t)
	add(t, s, "anthropic", "")

	// model.New lowercases, so build the colliding record directly — which is
	// also what a hand-written file would look like.
	p := model.Provider{Name: "anthropic", BaseURL: "https://x.test/v1"}
	if err := s.Add(p); err == nil {
		t.Fatal("a colliding name was accepted")
	}
}

// Save refuses a provider that does not exist, so a typo in `model enable`
// reports the typo instead of creating a second provider no run can use.
func TestSavingAProviderThatDoesNotExistIsRefused(t *testing.T) {
	s := open(t)
	p, err := model.New("anthropic", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(p); err == nil {
		t.Fatal("Save created a provider that was never added")
	}
}

// The enabled flag has to survive a save, because that is the entire effect of
// `model enable`.
func TestEnablingAModelPersists(t *testing.T) {
	s := open(t)
	p := add(t, s, "anthropic", "")

	var target string
	for _, m := range p.Models {
		if !m.Enabled {
			target = m.ID
			break
		}
	}
	if target == "" {
		t.Fatal("no disabled model in the fixture")
	}

	changed, err := p.SetEnabled(target, true)
	if err != nil || !changed {
		t.Fatalf("SetEnabled: changed=%v err=%v", changed, err)
	}
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}

	back, err := s.Load("anthropic")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range back.Models {
		if m.ID == target && !m.Enabled {
			t.Errorf("%s came back disabled: `model enable` reported success and "+
				"changed nothing on disk", target)
		}
	}
}

// The stored file is 0600, not 0644. It names the variable holding an API key
// and the endpoint it is sent to: that is a map to a credential, and no other
// user on the machine has a reason to read it.
func TestTheProviderFileIsNotWorldReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions")
	}
	s := open(t)
	add(t, s, "anthropic", "")

	info, err := os.Stat(s.Path("anthropic"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("providers/anthropic.json is %o, want 600: this file is a map to "+
			"an API key", perm)
	}
}

// A hand-edited file with the key pasted in is caught on READ, not sent to a
// provider. This is the last line of the "the key is never stored" rule.
func TestAKeyPastedIntoTheFileByHandIsCaughtOnRead(t *testing.T) {
	s := open(t)
	add(t, s, "anthropic", "")

	body := `{
  "name": "anthropic",
  "base_url": "https://api.anthropic.com/v1",
  "api_key_env": "sk-ant-api03-abcdefghijklmnopqrstuvwxyz"
}
`
	if err := os.WriteFile(s.Path("anthropic"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load("anthropic"); err == nil {
		t.Fatal("loaded a provider whose api_key_env is the key itself")
	}
}

// An api_key field added by hand is refused rather than ignored. Ignoring it
// would leave a secret sitting in the file with the tool reporting the provider
// as perfectly fine.
func TestAnApiKeyFieldAddedByHandIsRefused(t *testing.T) {
	s := open(t)
	add(t, s, "anthropic", "")

	body := `{
  "name": "anthropic",
  "base_url": "https://api.anthropic.com/v1",
  "api_key_env": "ANTHROPIC_API_KEY",
  "api_key": "sk-ant-api03-secret"
}
`
	if err := os.WriteFile(s.Path("anthropic"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := s.Load("anthropic")
	if err == nil {
		t.Fatal("accepted an api_key field: the secret would sit in the file with " +
			"the tool reporting the provider as fine")
	}
	if !strings.Contains(err.Error(), "api_key") {
		t.Errorf("the error does not name the offending field: %v", err)
	}
}

// A misspelled key is refused rather than left at its zero value. An empty
// api_key_env is LEGAL (a local server needs none), so ignoring the typo would
// register a provider with no credential and every call would come back
// unauthorized with nothing in the file to explain it.
func TestAMisspelledFieldIsRefusedRatherThanIgnored(t *testing.T) {
	s := open(t)
	add(t, s, "anthropic", "")

	body := `{
  "name": "anthropic",
  "base_url": "https://api.anthropic.com/v1",
  "api_key_evn": "ANTHROPIC_API_KEY"
}
`
	if err := os.WriteFile(s.Path("anthropic"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load("anthropic"); err == nil {
		t.Fatal("accepted api_key_evn: the provider would have no credential and " +
			"nothing would say why")
	}
}

// A record whose name disagrees with its filename is refused: it is addressable
// by one name and reports another, and a `model enable` would write a new file
// and leave this one in place with the old flags.
func TestAProviderWhoseNameDisagreesWithItsFileIsRefused(t *testing.T) {
	s := open(t)
	add(t, s, "anthropic", "")

	body := `{
  "name": "openai",
  "base_url": "https://api.anthropic.com/v1",
  "api_key_env": "ANTHROPIC_API_KEY"
}
`
	if err := os.WriteFile(s.Path("anthropic"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load("anthropic"); err == nil {
		t.Fatal("accepted a provider that answers to one name and reports another")
	}
}

// An unreadable file fails the whole listing instead of being skipped, because
// a list quietly missing a row looks exactly like a complete list.
func TestAnUnreadableProviderFailsTheListing(t *testing.T) {
	s := open(t)
	add(t, s, "anthropic", "")
	if err := os.WriteFile(s.Path("broken"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.List(); err == nil {
		t.Fatal("listed successfully while one provider was unreadable: the user " +
			"would read a partial list as a complete one")
	}
}

// No directory is no providers, not a failure: `model list` on a fresh checkout
// has to print an empty list, not an error about a missing directory.
func TestAnAbsentDirectoryListsNothing(t *testing.T) {
	s := &Store{dir: filepath.Join(t.TempDir(), "nope")}
	ps, err := s.List()
	if err != nil {
		t.Fatalf("an absent directory should be no providers: %v", err)
	}
	if len(ps) != 0 {
		t.Errorf("got %d providers from a directory that does not exist", len(ps))
	}
}

// Temp files must not be visible as providers. The suffix convention is what
// makes that true, and it is load-bearing rather than cosmetic.
func TestAHalfWrittenFileIsNotAProvider(t *testing.T) {
	s := open(t)
	add(t, s, "anthropic", "")
	if err := os.WriteFile(filepath.Join(s.Dir(), "x.json.tmp-1234"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	ps, err := s.List()
	if err != nil {
		t.Fatalf("a temp file broke the listing: %v", err)
	}
	if len(ps) != 1 {
		t.Errorf("got %d providers, want 1: a half-written file was treated as one", len(ps))
	}
}

// Owner finds the provider that owns a model, which is what `model enable`
// needs before it can save anything.
func TestOwnerFindsTheProviderThatOffersTheModel(t *testing.T) {
	s := open(t)
	add(t, s, "anthropic", "")

	p, id, err := s.Owner("claude-opus-4-1")
	if err != nil {
		t.Fatalf("owner: %v", err)
	}
	if p.Name != "anthropic" || id != "claude-opus-4-1" {
		t.Errorf("owner is %q/%q", p.Name, id)
	}
}

// Owner must find a DISABLED model. `model enable` exists precisely to act on
// one, so reusing Resolve's refusal wholesale would make the command unable to
// enable anything.
func TestOwnerFindsADisabledModelBecauseThatIsWhatEnableActsOn(t *testing.T) {
	s := open(t)
	p := add(t, s, "anthropic", "")

	var disabled string
	for _, m := range p.Models {
		if !m.Enabled {
			disabled = m.ID
		}
	}
	if disabled == "" {
		t.Fatal("no disabled model in the fixture")
	}

	if _, _, err := s.Owner(disabled); err != nil {
		t.Fatalf("Owner refused a disabled model (%s): `model enable` would be "+
			"unable to enable anything: %v", disabled, err)
	}
}

// Owner refuses an ambiguous id rather than picking one, the same refusal
// Resolve makes: enabling one of two and reporting success would contradict the
// run that then fails to resolve.
func TestOwnerRefusesAnAmbiguousModel(t *testing.T) {
	s := open(t)
	add(t, s, "anthropic", "")

	local, err := model.New("local", "http://localhost:11434/v1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	local.Models = []model.Model{{ID: "claude-opus-4-1"}}
	if err := s.Add(local); err != nil {
		t.Fatal(err)
	}

	_, _, err = s.Owner("claude-opus-4-1")
	if err == nil {
		t.Fatal("Owner chose between two providers: the command would report " +
			"success and the run would still fail to resolve")
	}
	if !strings.Contains(err.Error(), "model enable") {
		t.Errorf("the error does not show the qualified command that fixes it: %v", err)
	}
}

// Owner accepts the qualified spelling, which is the fix its own error offers.
func TestOwnerAcceptsTheQualifiedSpelling(t *testing.T) {
	s := open(t)
	add(t, s, "anthropic", "")
	local, err := model.New("local", "http://localhost:11434/v1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	local.Models = []model.Model{{ID: "claude-opus-4-1"}}
	if err := s.Add(local); err != nil {
		t.Fatal(err)
	}

	p, id, err := s.Owner("local/claude-opus-4-1")
	if err != nil {
		t.Fatalf("owner: %v", err)
	}
	if p.Name != "local" || id != "claude-opus-4-1" {
		t.Errorf("owner is %q/%q, want local/claude-opus-4-1", p.Name, id)
	}
}

// Loading a provider that was never registered says how to register it, rather
// than reporting a missing file.
func TestLoadingAnUnregisteredProviderNamesTheFix(t *testing.T) {
	s := open(t)
	_, err := s.Load("anthropic")
	if err == nil {
		t.Fatal("loaded a provider that does not exist")
	}
	if !strings.Contains(err.Error(), "provider add") {
		t.Errorf("the error does not name the command that fixes it: %v", err)
	}
}

// Open with no directory is refused rather than defaulting silently, so a
// caller that forgot to pass one does not scatter provider files into the
// working directory root.
func TestOpenWithNoDirectoryIsRefused(t *testing.T) {
	if _, err := Open("  "); err == nil {
		t.Fatal("opened a store with no directory")
	}
}
