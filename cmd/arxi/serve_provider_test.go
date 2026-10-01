package main

import (
	"encoding/json"
	"testing"

	"github.com/michiTrader/arxi/internal/modelstore"
)

// isolateProviders points the package-level providerDir at a fresh temp dir for
// the duration of one test and restores it after.
//
// The provider verbs open modelstore.Open(providerDir) at call time, so setting
// the global is the only seam that isolates them — there is no per-request store
// to inject. These tests therefore must not run with t.Parallel(): the global is
// shared, and a second test flipping it mid-exchange would read or write the
// wrong store. The restore matters for the same reason the other serve tests
// that touch "providers" relative to cwd keep working after these run.
func isolateProviders(t *testing.T) {
	t.Helper()
	saved := providerDir
	providerDir = t.TempDir()
	t.Cleanup(func() { providerDir = saved })
}

// TestProviderAddOverTheWireRegistersAndReturnsTheSnapshot proves the whole
// round trip: a well-formed provider.add registers the provider in the store and
// answers with the snapshot the frozen K2 schema promises
// ({name, protocol, base_url, api_key_env, models:[{id, enabled}]}).
//
// It uses a KNOWN provider (openai) because model.New populates its endpoint,
// protocol and model list from the shipped table — so the result carries the
// models the TUI will list, and the test exercises the same path a user typing
// `/provider add openai` into the frontend would.
func TestProviderAddOverTheWireRegistersAndReturnsTheSnapshot(t *testing.T) {
	isolateProviders(t)

	got := one(t, `{"id":"1","type":"provider.add","params":{"name":"openai","api_key_env":"OPENAI_API_KEY"}}`)
	if !got.OK {
		t.Fatalf("provider.add failed over the protocol: %+v\n"+
			"  consequence: the TUI cannot register a provider over the wire, so the "+
			"user is pushed back to the CLI the frontend exists to replace.", got.Error)
	}

	var snap struct {
		Name      string `json:"name"`
		Protocol  string `json:"protocol"`
		BaseURL   string `json:"base_url"`
		APIKeyEnv string `json:"api_key_env"`
		Models    []struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
		} `json:"models"`
	}
	raw, _ := json.Marshal(got.Result)
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("provider.add result is not the frozen snapshot shape: %v\n  raw: %s", err, raw)
	}
	if snap.Name != "openai" {
		t.Errorf("snapshot name is %q, want \"openai\".\n"+
			"  consequence: the client cannot tell which provider it just registered, "+
			"so the frontend's confirmation names the wrong thing.", snap.Name)
	}
	if snap.APIKeyEnv != "OPENAI_API_KEY" {
		t.Errorf("snapshot api_key_env is %q, want \"OPENAI_API_KEY\".", snap.APIKeyEnv)
	}
	if len(snap.Models) == 0 {
		t.Fatalf("snapshot carries no models, but openai is a known provider that " +
			"ships them.\n  consequence: the TUI shows a provider with nothing to " +
			"enable, and the user has no model to run against.")
	}

	// The store on disk must actually hold it: a snapshot the handler built but
	// never persisted would let the TUI believe a provider exists that the next
	// `model.list` cannot find.
	store, err := modelstore.Open(providerDir)
	if err != nil {
		t.Fatalf("reopening the store: %v", err)
	}
	ps, err := store.List()
	if err != nil {
		t.Fatalf("listing the store: %v", err)
	}
	if len(ps) != 1 || ps[0].Name != "openai" {
		t.Fatalf("the store holds %d providers, want exactly openai.\n"+
			"  consequence: provider.add answered success without writing, so the "+
			"state the TUI was promised does not survive the response.", len(ps))
	}
}

// TestProviderAddWithASecretShapedKeyEnvIsRefusedOverTheWire is the invariant
// this whole increment is arranged to protect: api_key_env carries the NAME of
// an environment variable, never the key itself. A client that mistakes the two
// must be refused by the core, over the wire, exactly as on the command line —
// and nothing may be written to the store.
//
// The enforcement site is model.New/validateKeyEnv on the arxi side; the TUI
// adds no second validator. This test proves that single site fires over the
// protocol, so the frontend cannot become a hole that lets a secret reach disk.
func TestProviderAddWithASecretShapedKeyEnvIsRefusedOverTheWire(t *testing.T) {
	isolateProviders(t)

	got := one(t, `{"id":"1","type":"provider.add","params":{"name":"openai","api_key_env":"sk-live-0123456789abcdef"}}`)
	if got.OK {
		t.Fatalf("provider.add accepted a secret-shaped api_key_env.\n" +
			"  consequence: a real key travelled the wire and landed in the store " +
			"file, the shell history and the process table — the exact leak " +
			"api_key_env exists to prevent. It must be refused, not stored.")
	}

	store, err := modelstore.Open(providerDir)
	if err != nil {
		t.Fatalf("reopening the store: %v", err)
	}
	ps, err := store.List()
	if err != nil {
		t.Fatalf("listing the store: %v", err)
	}
	if len(ps) != 0 {
		t.Fatalf("the refused provider was written anyway (%d on disk).\n"+
			"  consequence: a refusal that still persists the record leaks the very "+
			"value it refused; the write must not happen on the error path.", len(ps))
	}
}

// TestModelListOverTheWireIsEmptyWhenNoProvidersRegistered proves an empty store
// is a valid empty answer, not a failure. A user who has registered nothing yet
// asked a well-formed question; model.list must answer {models:[]} with ok:true,
// because the TUI opens on an empty list and an error there would read as a
// broken connection rather than an empty inventory.
func TestModelListOverTheWireIsEmptyWhenNoProvidersRegistered(t *testing.T) {
	isolateProviders(t)

	got := one(t, `{"id":"1","type":"model.list"}`)
	if !got.OK {
		t.Fatalf("model.list failed on an empty store: %+v\n"+
			"  consequence: an empty inventory reads as a broken connection, so the "+
			"TUI shows an error where it should show 'no providers yet'.", got.Error)
	}
	var res struct {
		Models []struct {
			Provider string `json:"provider"`
			ID       string `json:"id"`
			Enabled  bool   `json:"enabled"`
		} `json:"models"`
	}
	raw, _ := json.Marshal(got.Result)
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("model.list result is not the {models:[...]} shape: %v\n  raw: %s", err, raw)
	}
	if len(res.Models) != 0 {
		t.Fatalf("model.list returned %d rows from an empty store, want 0.", len(res.Models))
	}
}

// TestModelListAndToggleOverTheWire walks the TUI's real sequence: register a
// provider, list its models, disable the one that shipped enabled, and read the
// change back. It pins the row shape ({provider, id, enabled}) and the
// enable/disable result ({provider, model, enabled, changed}) the frontend
// decodes, and proves changed reflects an actual state transition.
func TestModelListAndToggleOverTheWire(t *testing.T) {
	isolateProviders(t)

	if add := one(t, `{"id":"1","type":"provider.add","params":{"name":"openai","api_key_env":"OPENAI_API_KEY"}}`); !add.OK {
		t.Fatalf("provider.add failed, cannot set up the toggle test: %+v", add.Error)
	}

	// openai ships gpt-5.1 enabled and gpt-5.1-mini disabled (first-enabled rule
	// in model.New). Disabling gpt-5.1 is a real transition, so changed must be
	// true; disabling it again is a no-op, so changed must be false.
	type toggle struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Enabled  bool   `json:"enabled"`
		Changed  bool   `json:"changed"`
	}
	decode := func(t *testing.T, r protoResponse) toggle {
		t.Helper()
		if !r.OK {
			t.Fatalf("toggle failed over the protocol: %+v", r.Error)
		}
		var out toggle
		raw, _ := json.Marshal(r.Result)
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("toggle result is not {provider,model,enabled,changed}: %v\n  raw: %s", err, raw)
		}
		return out
	}

	first := decode(t, one(t, `{"id":"2","type":"model.disable","params":{"model":"gpt-5.1"}}`))
	if !first.Changed {
		t.Errorf("disabling a model that shipped enabled reported changed=false.\n" +
			"  consequence: the TUI cannot tell a real toggle from a no-op, so its " +
			"confirmation lies about whether anything happened.")
	}
	if first.Enabled {
		t.Errorf("model.disable reported enabled=true for the disabled model.")
	}
	if first.Model != "gpt-5.1" || first.Provider != "openai" {
		t.Errorf("toggle result named %s/%s, want openai/gpt-5.1.", first.Provider, first.Model)
	}

	again := decode(t, one(t, `{"id":"3","type":"model.disable","params":{"model":"gpt-5.1"}}`))
	if again.Changed {
		t.Errorf("disabling an already-disabled model reported changed=true.\n" +
			"  consequence: an idempotent no-op is reported as a transition, so a " +
			"client that retries sees a change that did not occur.")
	}

	// The list must now show gpt-5.1 disabled: the toggle has to be visible to
	// the read the TUI refreshes with, or the two disagree about what is on.
	got := one(t, `{"id":"4","type":"model.list"}`)
	if !got.OK {
		t.Fatalf("model.list failed after a toggle: %+v", got.Error)
	}
	var res struct {
		Models []struct {
			Provider string `json:"provider"`
			ID       string `json:"id"`
			Enabled  bool   `json:"enabled"`
		} `json:"models"`
	}
	raw, _ := json.Marshal(got.Result)
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("model.list result shape: %v\n  raw: %s", err, raw)
	}
	var seen bool
	for _, m := range res.Models {
		if m.ID == "gpt-5.1" {
			seen = true
			if m.Enabled {
				t.Errorf("model.list still shows gpt-5.1 enabled after disabling it.\n" +
					"  consequence: the toggle did not persist, so the list the TUI " +
					"refreshes with contradicts the confirmation it just showed.")
			}
		}
	}
	if !seen {
		t.Fatalf("model.list did not include gpt-5.1 after registering openai.")
	}
}

// TestModelEnableUnknownRefIsRefusedOverTheWire proves a ref no provider offers
// is a located refusal, not a silent success. store.Owner errors on an unknown
// ref, and that error must reach the client as ok:false so the TUI can report
// "no such model" rather than appearing to toggle something that does not exist.
func TestModelEnableUnknownRefIsRefusedOverTheWire(t *testing.T) {
	isolateProviders(t)

	got := one(t, `{"id":"1","type":"model.enable","params":{"model":"nonesuch/ghost-model"}}`)
	if got.OK {
		t.Fatalf("model.enable reported success for a model no provider offers.\n" +
			"  consequence: the TUI confirms a toggle that touched nothing, so the " +
			"user believes a model is enabled that does not exist.")
	}
}
