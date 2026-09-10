package runconfig

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/model"
)

func fixture() Artifact {
	return New("r1", "live", strings.Repeat("a", 64), "ship it", "gpt-5.1",
		kernel.Config{Blueprint: "team", Members: []kernel.MemberConfig{{Name: "backend"}}},
		[]Route{{
			Ref: "gpt-5.1", Provider: "openai", Protocol: model.ProtocolOpenAIChatCompletions,
			Model: "gpt-5.1", BaseURL: "https://api.openai.com/v1",
			APIKeyEnv: "OPENAI_API_KEY", Price: model.Price{InUSDPerMTok: 1.25, OutUSDPerMTok: 10},
		}}, nil)
}

func TestSimulationVersionSelectsNativeSemanticsAndPreservesLegacy(t *testing.T) {
	a := New("r1", "sim", strings.Repeat("a", 64), "ship it", "", kernel.Config{}, nil, nil)
	if a.SimVersion != SimulationNative {
		t.Fatalf("new simulation version = %d, want %d", a.SimVersion, SimulationNative)
	}
	for _, version := range []int{SimulationLegacy, SimulationNative} {
		a.SimVersion = version
		if _, _, err := Encode(a); err != nil {
			t.Errorf("supported simulation version %d was refused: %v", version, err)
		}
	}
	for _, version := range []int{0, SimulationNative + 1} {
		a.SimVersion = version
		if _, _, err := Encode(a); err == nil {
			t.Errorf("unsupported simulation version %d was accepted", version)
		}
	}
}

func TestPublishRoundTripsExactDigestAndRefusesReplacement(t *testing.T) {
	dir := t.TempDir()
	a := fixture()
	digest, err := Publish(dir, a)
	if err != nil {
		t.Fatal(err)
	}
	got, loadedDigest, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loadedDigest != digest || got.Prompt != a.Prompt || got.DefaultModel != a.DefaultModel {
		t.Fatalf("loaded config differs: digest %s/%s config %+v", loadedDigest, digest, got)
	}
	if _, err := Publish(dir, a); err == nil {
		t.Fatal("a second publish replaced an immutable execution contract")
	}
}

func TestLoadIsStrictAndSecretBearingURLsAreRefused(t *testing.T) {
	for _, rawURL := range []string{
		"https://user:secret@example.com/v1",
		"https://example.com/v1?key=secret",
		"https://example.com/v1#secret",
	} {
		a := fixture()
		a.Routes[0].BaseURL = rawURL
		if _, _, err := Encode(a); err == nil {
			t.Errorf("secret-bearing URL %q was accepted", rawURL)
		}
	}

	dir := t.TempDir()
	body := `{"schema":"arxi.effective-config/v1","unknown":true}`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(dir); err == nil {
		t.Fatal("unknown fields were silently accepted")
	}
}

func TestRoutesAcceptEveryImplementedProtocolAndRejectUnknownOnes(t *testing.T) {
	for _, protocol := range []string{model.ProtocolOpenAIChatCompletions, model.ProtocolAnthropicMessages} {
		a := fixture()
		a.Routes[0].Protocol = protocol
		if _, _, err := Encode(a); err != nil {
			t.Errorf("implemented protocol %q was refused: %v", protocol, err)
		}
	}
	for _, protocol := range []string{"", "unknown/v1"} {
		a := fixture()
		a.Routes[0].Protocol = protocol
		if _, _, err := Encode(a); err == nil {
			t.Errorf("unsupported protocol %q was accepted", protocol)
		}
	}
}

func TestCredentialReferenceMustBeANameNotAValue(t *testing.T) {
	a := fixture()
	for _, value := range []string{"sk-live-secret", "secret value", "A=B"} {
		a.Routes[0].APIKeyEnv = value
		if _, _, err := Encode(a); err == nil {
			t.Errorf("credential material-shaped api_key_env %q was accepted", value)
		}
	}
	a.Routes[0].APIKeyEnv = "OPENAI_API_KEY"
	if _, _, err := Encode(a); err != nil {
		t.Fatalf("valid credential reference was refused: %v", err)
	}
}

func TestEncodedArtifactNeverContainsCredentialValue(t *testing.T) {
	const secret = "sk-live-this-value-must-never-be-frozen"
	t.Setenv("OPENAI_API_KEY", secret)

	body, _, err := Encode(fixture())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), secret) {
		t.Fatal("effective config persisted the credential value")
	}
	if !strings.Contains(string(body), `"api_key_env": "OPENAI_API_KEY"`) {
		t.Fatal("effective config did not retain the credential reference")
	}
}

func TestMissingArtifactIsDistinguishableForLegacyResume(t *testing.T) {
	_, _, err := Load(t.TempDir())
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing artifact returned %v, want os.ErrNotExist", err)
	}
}
