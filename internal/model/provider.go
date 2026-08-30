// Package model decides what a provider and a model ARE, and nothing about
// where they are stored.
//
// It exists as its own package for the reason stated in every other pure
// package in this tree: `internal/modelstore` reads and writes the disk, and
// this one is forbidden by internal/arch_test.go from importing os, net/http or
// time. That separation is not tidiness. The rule that decides whether a model
// may be used — it must exist, be unambiguous and be enabled — is the rule the
// live executor will consult before spending money, and a rule that can only be
// exercised by first creating a directory is a rule nobody tests at the edges.
//
// # Why this package comes before the executor
//
// `iash run start` refuses a non-simulated run today because there is no
// LLM-backed Executor. Writing one needs three things that do not exist yet: an
// endpoint, a credential and a model id. docs/design §20.1 puts them in exactly
// that position — `provider add`, `model list`, `model enable` are the first
// three commands a new user types, before the first run. So this is the step in
// order, not a detour around the executor.
//
// # The key is never here
//
// A Provider stores the NAME of an environment variable, never a key. That is a
// decision from §20.1 ("`provider add` takes `--api-key-env`, the *name of a
// variable*, not the key") and it is enforced rather than documented: see
// validateKeyEnv, which refuses a value that looks like a secret. A key that
// reaches this struct reaches a JSON file in the repository, and from there a
// commit.
package model

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Provider is one place models can be called, and the credential to call it.
//
// Models are held INSIDE the provider rather than in a registry of their own,
// and that placement is what makes `model enable` cheap and safe: it rewrites
// the file of the one provider that owns the model, so a crash mid-write cannot
// lose the credentials of a provider the command never mentioned. It is the
// same reasoning trigstore gives for one file per trigger.
type Provider struct {
	Name string `json:"name"`

	// BaseURL is an OpenAI-compatible endpoint, as the surface declares it.
	// Stored even when it came from the table in Known, because a default that
	// lives only in the binary changes under the user when the binary is
	// upgraded, and a run that worked yesterday would then talk to a different
	// host with no record of why.
	BaseURL string `json:"base_url"`

	// APIKeyEnv is the NAME of the variable holding the key. See the package
	// comment: the key itself must never appear in this struct.
	APIKeyEnv string `json:"api_key_env"`

	Models []Model `json:"models,omitempty"`

	// AddedAt is stamped by the caller, which is why this package can stay off
	// the clock. Informational only: nothing resolves against it.
	AddedAt string `json:"added_at,omitempty"`
}

// Model is one callable model and whether it may be used.
//
// Enabled is stored per model rather than derived from a list of disabled ids,
// because `model list` has to print a STATUS for every model and a derived
// answer would silently report "enabled" for an id the provider no longer
// offers.
type Model struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
}

// known is the table of providers whose endpoint and models ship with the
// binary.
//
// It exists because §20.1 registers a provider with no --base-url and expects
// `model list` to have something in it immediately. Without the table that
// scenario is impossible: a fresh install would list nothing, and the user's
// first command after `provider add` would be to look up an endpoint in someone
// else's documentation.
//
// The FIRST model of each provider is the one enabled on registration and the
// rest arrive disabled. That is a spend decision, not a style one: enabling
// every model would leave the most expensive one in the tree a single
// `--model` typo away, and §20.1's own output shows opus disabled next to
// sonnet enabled.
var known = []struct {
	name    string
	baseURL string
	models  []string
}{
	{"anthropic", "https://api.anthropic.com/v1", []string{
		"claude-sonnet-4-6", "claude-opus-4-1", "claude-haiku-4-5"}},
	{"openai", "https://api.openai.com/v1", []string{
		"gpt-5.1", "gpt-5.1-mini"}},
	// A local server is in the table on purpose. It is the only provider that
	// can be exercised end to end without a credential and without a bill, so
	// the first real executor test has somewhere to point.
	{"local", "http://127.0.0.1:11434/v1", []string{"llama3.1"}},
}

// Known reports the shipped endpoint and models for a provider name.
func Known(name string) (baseURL string, models []string, ok bool) {
	for _, k := range known {
		if k.name == strings.ToLower(strings.TrimSpace(name)) {
			return k.baseURL, append([]string(nil), k.models...), true
		}
	}
	return "", nil, false
}

// KnownNames lists the providers in the table, sorted. Used to name the
// alternatives in an error, because "unknown provider" without the list makes
// the user guess.
func KnownNames() []string {
	out := make([]string, 0, len(known))
	for _, k := range known {
		out = append(out, k.name)
	}
	sort.Strings(out)
	return out
}

// DefaultKeyEnv is the variable a provider's key is read from when the user did
// not name one: ANTHROPIC_API_KEY for anthropic.
//
// Derived rather than looked up in the table so that an unknown provider gets a
// sensible default too. A user registering `together` should not have to invent
// a convention that the rest of the tool already has.
func DefaultKeyEnv(name string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(name)) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			// Anything else becomes an underscore: `azure-openai` has to yield
			// a name a shell can actually export, and a dash in an environment
			// variable is not exportable in POSIX sh.
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return b.String() + "_API_KEY"
}

// New builds a provider, filling in whatever the user left out.
//
// baseURL and keyEnv may both be empty, which is the §20.1 invocation. An
// unknown provider with no --base-url is refused rather than guessed: there is
// no convention that turns a vendor name into a hostname, and inventing one
// would produce a provider that looks registered and fails on the first call
// with a DNS error nobody can connect back to this command.
func New(name, baseURL, keyEnv, addedAt string) (Provider, error) {
	name = strings.ToLower(strings.TrimSpace(name))

	tableURL, models, inTable := Known(name)
	if baseURL == "" {
		if !inTable {
			return Provider{}, fmt.Errorf("provider %q is not one of the providers "+
				"this build knows (%s), so --base-url is required.\n"+
				"  it must be an OpenAI-compatible endpoint, for example:\n"+
				"    iash provider add %s --base-url https://api.example.com/v1",
				name, strings.Join(KnownNames(), ", "), name)
		}
		baseURL = tableURL
	}
	// A loopback endpoint gets NO credential variable unless the user named one.
	//
	// Found by RUNNING it: `provider add local` assigned $LOCAL_API_KEY, and the
	// run then refused because that variable was empty -- so the one provider
	// that can be exercised with no key and no bill was the only one that could
	// not be used without inventing a meaningless secret. There is no network to
	// read a key off on loopback, which is the entire reason plain http is
	// permitted there at all.
	//
	// An explicit --api-key-env is still honoured: some local servers do check a
	// token, and refusing to store the name would make those unusable instead.
	if keyEnv == "" && !isLoopbackAnyScheme(baseURL) {
		keyEnv = DefaultKeyEnv(name)
	}

	p := Provider{
		Name:      name,
		BaseURL:   strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		APIKeyEnv: strings.TrimSpace(keyEnv),
		AddedAt:   addedAt,
	}
	for i, id := range models {
		// Only for a provider taken from the table. A provider registered by
		// --base-url gets no models, because this build cannot know what that
		// endpoint serves; `model list` says so rather than inventing rows.
		p.Models = append(p.Models, Model{ID: id, Enabled: i == 0})
	}
	if err := p.Validate(); err != nil {
		return Provider{}, err
	}
	return p, nil
}

// Validate is run on the way out AND on the way in, because the stored file is
// text a human edits.
func (p Provider) Validate() error {
	if err := validateName(p.Name); err != nil {
		return err
	}
	if err := validateBaseURL(p.Name, p.BaseURL); err != nil {
		return err
	}
	if err := validateKeyEnv(p.Name, p.APIKeyEnv); err != nil {
		return err
	}

	seen := map[string]bool{}
	for _, m := range p.Models {
		if strings.TrimSpace(m.ID) == "" {
			return fmt.Errorf("provider %q has a model with no id: it would appear "+
				"in `model list` as a blank row that no `model enable` can address", p.Name)
		}
		// A duplicate id inside one provider is refused rather than deduped:
		// SetEnabled would change the first and `model list` would print the
		// second, so the command would report success and the status shown
		// would not move.
		if seen[m.ID] {
			return fmt.Errorf("provider %q lists model %q twice: enabling it would "+
				"change one entry while `model list` printed the other, so the "+
				"status would never appear to change", p.Name, m.ID)
		}
		seen[m.ID] = true
	}
	return nil
}

// validateName keeps a provider name usable as a filename and as a lookup key.
func validateName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("a provider needs a name: it is what `iash agent create " +
			"--model` resolves through and what the stored file is called")
	}
	if name != strings.ToLower(name) {
		return fmt.Errorf("provider name %q has upper case in it: names become "+
			"filenames, and on macOS and Windows %q and %q are the same file, so "+
			"one would overwrite the other there while working here",
			name, name, strings.ToLower(name))
	}
	for _, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			return fmt.Errorf("provider name %q contains %q: a name becomes a "+
				"filename, so a separator or a dot would escape the store directory "+
				"or hide the file", name, string(r))
		}
	}
	return nil
}

// validateBaseURL refuses what cannot be called, and refuses plaintext to a
// remote host.
func validateBaseURL(name, url string) error {
	switch {
	case url == "":
		return fmt.Errorf("provider %q has no base URL, so there is nothing to "+
			"call", name)
	case strings.HasPrefix(url, "https://"):
		return nil
	case strings.HasPrefix(url, "http://"):
		// http is allowed ONLY to the loopback interface. A local model server
		// is the one case where there is no network to eavesdrop on, and
		// refusing it outright would make the only free, testable provider
		// unusable. Anywhere else, http means the API key travels in clear text
		// over somebody's wifi.
		if isLoopback(url) {
			return nil
		}
		return fmt.Errorf("provider %q would be called over plain http at %s: the "+
			"API key travels in the request, so it would cross the network in "+
			"clear text.\n  http is accepted only for localhost, where there is "+
			"no network to read it off", name, url)
	default:
		return fmt.Errorf("provider %q has base URL %q, which is not an http or "+
			"https endpoint", name, url)
	}
}

// isLoopback reports whether the host of an http URL is the local machine.
//
// The bracketed IPv6 case is handled separately and that is a correction of a
// real bug rather than defensiveness: splitting on the first ":" cuts
// "[::1]:8080" down to "[", which is not loopback, so the one URL a developer
// running a local server over IPv6 would type was refused as if it were
// sending the key across the internet. Anything ending in a colon-port after
// the bracket has to be trimmed at the bracket, not at the first colon.
//
// A host that merely BEGINS with "localhost" is deliberately NOT loopback:
// "localhost.evil.com" resolves to whatever its owner points it at, so a prefix
// test here would ship the API key in clear text to a name chosen to look safe.
func isLoopback(url string) bool {
	host := strings.TrimPrefix(url, "http://")

	if strings.HasPrefix(host, "[") {
		end := strings.Index(host, "]")
		if end < 0 {
			return false // an unterminated bracket is not a URL we will call
		}
		host = host[:end+1]
	} else if i := strings.IndexAny(host, "/:"); i >= 0 {
		host = host[:i]
	}

	switch host {
	case "localhost", "127.0.0.1", "[::1]", "::1":
		return true
	}
	return false
}

// isLoopbackAnyScheme asks whether a URL points at this machine, whatever the
// scheme.
//
// isLoopback exists to answer a narrower and more dangerous question -- "is it
// safe to send a credential over plain http" -- so it strips only "http://" and
// treats everything else as remote. Reusing it here would be wrong in the
// harmless-looking direction: https://127.0.0.1/v1 would be judged remote and
// would silently acquire a credential requirement it does not need.
//
// The two are kept separate rather than merged. Widening isLoopback to accept
// https would widen the set of URLs the plain-http rule considers safe, and that
// rule is the one keeping a key off a network.
func isLoopbackAnyScheme(url string) bool {
	if rest, ok := afterScheme(url, "https://"); ok {
		return isLoopback("http://" + rest)
	}
	return isLoopback(url)
}

// afterScheme strips a scheme prefix and reports whether it was there.
func afterScheme(url, scheme string) (string, bool) {
	if strings.HasPrefix(url, scheme) {
		return url[len(scheme):], true
	}
	return "", false
}

// validateKeyEnv refuses a name a shell cannot export, and refuses a value that
// is evidently the key itself.
//
// The second check is the one that matters. `iash provider add anthropic
// --api-key-env sk-ant-api03-...` is an easy mistake — the flag is next to the
// word "key" — and it succeeds silently, writes the secret into a JSON file in
// the working directory, and the next commit publishes it. Every real key
// format is far longer than any variable name and none of them are upper case
// with underscores, so the two are cleanly separable.
func validateKeyEnv(name, env string) error {
	if env == "" {
		// Empty is legal: a local model server needs no credential, and
		// demanding one would make the free provider the awkward one.
		return nil
	}
	if looksLikeASecret(env) {
		return fmt.Errorf("--api-key-env for provider %q looks like the key "+
			"itself, not the name of a variable.\n"+
			"  pass the NAME so the secret stays out of this file, the shell "+
			"history and the process table:\n"+
			"    export %s=<the key>\n"+
			"    iash provider add %s --api-key-env %s",
			name, DefaultKeyEnv(name), name, DefaultKeyEnv(name))
	}
	first := rune(env[0])
	if !(first >= 'A' && first <= 'Z') && first != '_' {
		return fmt.Errorf("--api-key-env %q is not a usable variable name for "+
			"provider %q: it must start with a letter or an underscore, or no "+
			"shell can export it", env, name)
	}
	for _, r := range env {
		ok := (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_'
		if !ok {
			return fmt.Errorf("--api-key-env %q contains %q, which is not valid in "+
				"an environment variable name (upper case, digits and underscore "+
				"only). Did you mean %s?", env, string(r), DefaultKeyEnv(name))
		}
	}
	return nil
}

// looksLikeASecret recognises the shape of a credential rather than one
// vendor's prefix.
//
// Prefix matching alone ("sk-") would pass every key from every provider that
// does not use it, and the consequence of a miss is a published secret. The
// length test is the general one: no environment variable name in practice runs
// past forty characters, and no API key is shorter.
func looksLikeASecret(v string) bool {
	lower := strings.ToLower(v)
	for _, pre := range []string{"sk-", "sk_", "pk-", "api-", "api_", "key-", "bearer "} {
		if strings.HasPrefix(lower, pre) {
			return true
		}
	}
	if len(v) > 40 && strings.ToUpper(v) != v {
		return true
	}
	return false
}

// SetEnabled turns one model on or off, reporting whether anything changed.
//
// The bool is returned rather than swallowed so `model enable` can say "already
// enabled" instead of printing a success that did nothing. A command that
// reports a change it did not make is how a user concludes the setting does not
// work.
func (p *Provider) SetEnabled(id string, on bool) (changed bool, err error) {
	for i := range p.Models {
		if p.Models[i].ID != id {
			continue
		}
		if p.Models[i].Enabled == on {
			return false, nil
		}
		p.Models[i].Enabled = on
		return true, nil
	}
	return false, fmt.Errorf("provider %q does not offer a model called %q.\n"+
		"  see what it offers: iash model list", p.Name, id)
}

// Encode renders a provider for storage.
//
// Indented, because this file is one a user opens to check which variable their
// key is read from, and a single-line JSON object answers that question badly.
// The trailing newline is for the same reason: a file without one makes `cat`
// run the next prompt into the closing brace.
func (p Provider) Encode() ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	body, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode provider %q: %w", p.Name, err)
	}
	return append(body, '\n'), nil
}

// SortProviders orders providers by name, the order `model list` prints.
func SortProviders(ps []Provider) {
	sort.Slice(ps, func(i, j int) bool { return ps[i].Name < ps[j].Name })
}
