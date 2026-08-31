package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/michiTrader/arxi/internal/surface"
)

// Subprocess tests for `provider add` and the `model` commands.
//
// Subprocess and not direct calls, for the reason the trigger CLI tests give:
// these commands exit with a code, and the code is half of what is being
// tested. Calling the function in-process would take the whole test binary down
// with os.Exit.
//
// They share the harness in trigger_cli_test.go (TestMain, buildIash, arxi,
// workdir), and the working directory is what isolates them: providerDir is
// relative, so each test's t.TempDir() gets its own providers/.

// addProvider registers a provider in dir and fails loudly if it did not work.
func addProvider(t *testing.T, dir string, args ...string) result {
	t.Helper()
	r := arxi(t, dir, append([]string{"provider", "add"}, args...)...)
	if r.code != 0 {
		t.Fatalf("provider add %v failed (%d):\n%s", args, r.code, r.out)
	}
	return r
}

// statusOf reads the STATUS column for one model out of `model list --json`.
//
// JSON and not the table, because a column-position assertion breaks on a
// harmless width change and then gets deleted rather than fixed.
func statusOf(t *testing.T, dir, id string) (enabled, found bool) {
	t.Helper()
	r := arxi(t, dir, "model", "list", "--json")
	if r.code != 0 {
		t.Fatalf("model list --json failed (%d):\n%s", r.code, r.out)
	}
	var doc struct {
		Models []struct {
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
		} `json:"models"`
	}
	if err := json.Unmarshal([]byte(r.out), &doc); err != nil {
		t.Fatalf("model list --json is not JSON: %v\n%s", err, r.out)
	}
	for _, m := range doc.Models {
		if m.Name == id {
			return m.Enabled, true
		}
	}
	return false, false
}

// The §20.1 invocation, end to end. If this needs flags, the documented first
// command a new user types does not work.
func TestTheDocumentedFirstCommandsWork(t *testing.T) {
	dir := workdir(t)

	r := addProvider(t, dir, "anthropic")
	if !strings.Contains(r.out, "key from $ANTHROPIC_API_KEY") {
		t.Errorf("the output does not name the variable the key is read from:\n%s", r.out)
	}

	list := arxi(t, dir, "model", "list")
	if list.code != 0 {
		t.Fatalf("model list failed (%d):\n%s", list.code, list.out)
	}
	// The header is the documented output of this command, not decoration.
	for _, col := range []string{"NAME", "PROVIDER", "STATUS"} {
		if !strings.Contains(list.out, col) {
			t.Errorf("the %s column is missing from `model list`:\n%s", col, list.out)
		}
	}
	if !strings.Contains(list.out, "enabled") || !strings.Contains(list.out, "disabled") {
		t.Errorf("both statuses should appear (one model enabled on registration, "+
			"the rest not):\n%s", list.out)
	}
}

// THE security test at the CLI boundary. `--api-key-env sk-...` is one
// keystroke of misunderstanding from the correct command, and if it succeeds
// the secret is written to a file in the working directory and the next commit
// publishes it.
func TestAKeyPassedToTheFlagIsRefusedAndNothingIsWritten(t *testing.T) {
	dir := workdir(t)

	r := arxi(t, dir, "provider", "add", "anthropic",
		"--api-key-env", "sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123")
	if r.code != 2 {
		t.Errorf("exit %d, want 2 (misuse):\n%s", r.code, r.out)
	}
	if !strings.Contains(r.out, "looks like the key itself") {
		t.Errorf("the error does not explain the mistake:\n%s", r.out)
	}

	// The refusal has to happen BEFORE the write. An error message printed
	// after the file exists is not protection.
	if _, err := os.Stat(filepath.Join(dir, "providers")); err == nil {
		body, _ := os.ReadFile(filepath.Join(dir, "providers", "anthropic.json"))
		if strings.Contains(string(body), "sk-ant") {
			t.Fatalf("the secret was written to disk anyway:\n%s", body)
		}
		t.Errorf("a providers directory was created by a refused command")
	}
}

// The provider file must not be world-readable: it names the variable holding
// an API key and the endpoint it is sent to, which is a map to a credential.
func TestTheProviderFileTheCLIWritesIsNotWorldReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions")
	}
	dir := workdir(t)
	addProvider(t, dir, "anthropic")

	info, err := os.Stat(filepath.Join(dir, "providers", "anthropic.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("the file is %o, want 600", perm)
	}
}

// `model enable` must actually change the stored flag. This is the test that
// would have caught the dry-run class of bug in this command: a report of
// success with no effect on disk.
func TestEnablingAModelChangesWhatIsStored(t *testing.T) {
	dir := workdir(t)
	addProvider(t, dir, "anthropic")

	if on, found := statusOf(t, dir, "claude-opus-4-1"); !found || on {
		t.Fatalf("fixture: claude-opus-4-1 found=%v enabled=%v, want found and "+
			"disabled", found, on)
	}

	r := arxi(t, dir, "model", "enable", "claude-opus-4-1")
	if r.code != 0 {
		t.Fatalf("model enable failed (%d):\n%s", r.code, r.out)
	}

	if on, _ := statusOf(t, dir, "claude-opus-4-1"); !on {
		t.Error("the command reported success and the stored flag did not move")
	}
}

// Disable is the same path in reverse, and it has to work on the model that was
// enabled at registration — the one an operator would actually want to withhold.
func TestDisablingTheModelEnabledAtRegistrationWorks(t *testing.T) {
	dir := workdir(t)
	addProvider(t, dir, "anthropic")

	if on, found := statusOf(t, dir, "claude-sonnet-4-6"); !found || !on {
		t.Fatalf("fixture: claude-sonnet-4-6 found=%v enabled=%v", found, on)
	}

	if r := arxi(t, dir, "model", "disable", "claude-sonnet-4-6"); r.code != 0 {
		t.Fatalf("model disable failed (%d):\n%s", r.code, r.out)
	}
	if on, _ := statusOf(t, dir, "claude-sonnet-4-6"); on {
		t.Error("the model is still enabled after `model disable`")
	}
}

// Enabling twice is not an error and does not claim to have changed anything.
// An error would break an idempotent deploy script; a bare success would teach
// the user that the command does nothing.
func TestEnablingTwiceSucceedsAndSaysItChangedNothing(t *testing.T) {
	dir := workdir(t)
	addProvider(t, dir, "anthropic")

	if r := arxi(t, dir, "model", "enable", "claude-opus-4-1"); r.code != 0 {
		t.Fatalf("first enable failed: %s", r.out)
	}
	r := arxi(t, dir, "model", "enable", "claude-opus-4-1")
	if r.code != 0 {
		t.Errorf("exit %d on a second enable: an idempotent deploy script that "+
			"enables a model every time would break:\n%s", r.code, r.out)
	}
	if !strings.Contains(r.out, "already enabled") {
		t.Errorf("the output claims a change it did not make:\n%s", r.out)
	}
}

// Registering the same provider twice is refused. An overwrite would repoint
// every agent at a new endpoint and forget which models an operator enabled.
func TestRegisteringAProviderTwiceIsRefusedAndKeepsTheFlags(t *testing.T) {
	dir := workdir(t)
	addProvider(t, dir, "anthropic")
	if r := arxi(t, dir, "model", "enable", "claude-opus-4-1"); r.code != 0 {
		t.Fatal(r.out)
	}

	r := arxi(t, dir, "provider", "add", "anthropic")
	if r.code != 1 {
		t.Errorf("exit %d, want 1 (operational, not misuse: the command was "+
			"spelled correctly):\n%s", r.code, r.out)
	}

	// The flags the operator chose must survive the refusal.
	if on, _ := statusOf(t, dir, "claude-opus-4-1"); !on {
		t.Error("the refused add reset an enabled flag anyway")
	}
}

// An unknown provider with no endpoint is refused as misuse, and the message
// names the flag and the alternatives.
func TestAnUnknownProviderWithoutAnEndpointIsRefusedAsMisuse(t *testing.T) {
	dir := workdir(t)
	r := arxi(t, dir, "provider", "add", "together")
	if r.code != 2 {
		t.Errorf("exit %d, want 2:\n%s", r.code, r.out)
	}
	if !strings.Contains(r.out, "--base-url") {
		t.Errorf("the message does not name the flag that fixes it:\n%s", r.out)
	}
}

// Plain http to a remote host would put the API key on the wire in clear text.
func TestPlainHTTPToARemoteHostIsRefusedByTheCLI(t *testing.T) {
	dir := workdir(t)
	r := arxi(t, dir, "provider", "add", "together",
		"--base-url", "http://api.together.xyz/v1")
	if r.code != 2 {
		t.Errorf("exit %d, want 2:\n%s", r.code, r.out)
	}
	if !strings.Contains(r.out, "clear text") {
		t.Errorf("the message does not say what the risk is:\n%s", r.out)
	}
}

// A local server over plain http is allowed, and is the only provider that can
// be exercised with no credential and no bill.
func TestALocalProviderOverPlainHTTPIsAccepted(t *testing.T) {
	dir := workdir(t)
	addProvider(t, dir, "local")

	r := arxi(t, dir, "model", "list")
	if !strings.Contains(r.out, "local") {
		t.Errorf("the local provider is not listed:\n%s", r.out)
	}
}

// `model list` on a fresh directory is a question, and the answer has to
// include what to do about it. Exit 0: nothing is wrong.
func TestModelListOnAFreshDirectoryExplainsTheNextStep(t *testing.T) {
	dir := workdir(t)
	r := arxi(t, dir, "model", "list")
	if r.code != 0 {
		t.Errorf("exit %d on an empty store: an empty list is not an error:\n%s",
			r.code, r.out)
	}
	if !strings.Contains(r.out, "provider add") {
		t.Errorf("the output does not name the command that fixes it:\n%s", r.out)
	}
}

// A model two providers offer is refused rather than chosen. Enabling one and
// reporting success would contradict the run that then fails to resolve.
func TestAnAmbiguousModelIsRefusedRatherThanChosen(t *testing.T) {
	dir := workdir(t)
	addProvider(t, dir, "anthropic")
	addProvider(t, dir, "local")

	// Make the collision by hand — this is the realistic shape, a local server
	// serving a vendor id under its own name.
	path := filepath.Join(dir, "providers", "local.json")
	body := `{
  "name": "local",
  "base_url": "http://127.0.0.1:11434/v1",
  "api_key_env": "LOCAL_API_KEY",
  "models": [{"id": "claude-opus-4-1", "enabled": false}]
}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	r := arxi(t, dir, "model", "enable", "claude-opus-4-1")
	if r.code == 0 {
		t.Fatalf("the command chose a provider for the user:\n%s", r.out)
	}
	if !strings.Contains(r.out, "local/claude-opus-4-1") &&
		!strings.Contains(r.out, "anthropic/claude-opus-4-1") {
		t.Errorf("the error does not show the qualified spelling that fixes it:\n%s", r.out)
	}

	// And the qualified spelling has to work, or the advice is useless.
	q := arxi(t, dir, "model", "enable", "local/claude-opus-4-1")
	if q.code != 0 {
		t.Errorf("the qualified spelling the error recommended does not work "+
			"(%d):\n%s", q.code, q.out)
	}
}

// A typo is told what it probably meant, because the alternative costs the user
// a second command to discover that 4-5 is 4-6.
func TestATypoInModelEnableSuggestsTheRealID(t *testing.T) {
	dir := workdir(t)
	addProvider(t, dir, "anthropic")

	r := arxi(t, dir, "model", "enable", "claude-sonnet-4-5")
	if r.code != 1 {
		t.Errorf("exit %d, want 1:\n%s", r.code, r.out)
	}
	if !strings.Contains(r.out, "claude-sonnet-4-6") {
		t.Errorf("no suggestion for an obvious typo:\n%s", r.out)
	}
}

// A hand-edited file with the key pasted in is caught on READ, so a secret that
// got there by another route still does not reach a provider.
func TestAKeyPastedIntoTheFileIsCaughtWhenTheCLIReadsIt(t *testing.T) {
	dir := workdir(t)
	addProvider(t, dir, "anthropic")

	path := filepath.Join(dir, "providers", "anthropic.json")
	body := `{
  "name": "anthropic",
  "base_url": "https://api.anthropic.com/v1",
  "api_key_env": "sk-ant-api03-abcdefghijklmnopqrstuvwxyz"
}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	r := arxi(t, dir, "model", "list")
	if r.code == 0 {
		t.Fatalf("listed a provider whose api_key_env is the key itself:\n%s", r.out)
	}
}

// `model list --json` has to be parseable, because it is declared as an agent
// tool and an agent has no other way to read it.
func TestModelListJSONIsParseable(t *testing.T) {
	dir := workdir(t)
	addProvider(t, dir, "anthropic")

	r := arxi(t, dir, "model", "list", "--json")
	if r.code != 0 {
		t.Fatalf("exit %d:\n%s", r.code, r.out)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(r.out), &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, r.out)
	}
	// A named key rather than a bare array, so a later addition does not change
	// the type of the whole document.
	if _, ok := doc["models"]; !ok {
		t.Errorf("the document has no \"models\" key: %s", r.out)
	}
}

// A declared-but-unbuilt subcommand must not be called unknown, which would
// send the user hunting for a typo they never made.
func TestADeclaredProviderSubcommandIsNotCalledUnknown(t *testing.T) {
	dir := workdir(t)
	// `provider add` is the only provider subcommand in the registry today, so
	// this asserts the FALLBACK shape rather than a specific capability: an
	// unbuilt one has to reach notImplemented.
	r := arxi(t, dir, "provider", "list")
	if strings.Contains(r.out, "unknown command") {
		t.Errorf("a provider subcommand was called unknown:\n%s", r.out)
	}
}

// `model enable` with no argument is misuse, refused before anything is
// touched, and says what is missing.
func TestModelEnableWithNoArgumentIsRefused(t *testing.T) {
	dir := workdir(t)
	addProvider(t, dir, "anthropic")

	r := arxi(t, dir, "model", "enable")
	if r.code != 2 {
		t.Errorf("exit %d, want 2:\n%s", r.code, r.out)
	}
	if !strings.Contains(strings.ToLower(r.out), "model") {
		t.Errorf("the message does not name the missing parameter:\n%s", r.out)
	}
}

// A live run must not describe itself as a simulation, and a --sim run must.
//
// The `simulated` field on run.started is the ONLY thing in the log that
// distinguishes the two. That is not an oversight, it is the point of --sim:
// the same reducer, the same loop, the same effect ordering and the same event
// shapes, so a simulated log is worth reading precisely because it is
// indistinguishable in every other respect. Which leaves this one field
// carrying the whole distinction.
//
// It was a hardcoded `true`, correct for as long as --sim was the only mode and
// false from the moment the live executor landed. It was found by reading the
// log of a real run -- one that had really called a real HTTP server -- and
// finding it labelled a simulation. Nothing failed; the run succeeded and the
// summary was accurate. Only the log lied, and the log is the artefact that
// outlives the terminal.
//
// The test asserts BOTH directions. Pinning only the live case would pass for a
// hardcoded `false`, which is the same bug pointing the other way and the more
// dangerous one: a simulation that claims to be real invites a reader to
// believe work happened that never did.
func TestALiveRunIsNotLoggedAsASimulation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the loopback model server used here is POSIX-only")
	}
	dir := workdir(t)

	// A local model server, so the live path can be exercised with no
	// credential and no bill. This is the case the loopback-credential fix
	// exists to make possible.
	srv := startLoopbackModelServer(t)
	addProvider(t, dir, "local", "--base-url", srv+"/v1")

	bp := filepath.Join(dir, "bp.yaml")
	body := "name: live-test\n" +
		"members:\n  - {name: backend, role: implementer, model: llama3.1}\n" +
		"stages:\n  - {name: build, advance_when: all}\n"
	if err := os.WriteFile(bp, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{"live", []string{"run", "start", bp, "do it", "--budget", "5.00"}, false},
		{"sim", []string{"run", "start", bp, "do it", "--budget", "5.00", "--sim"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runDir := filepath.Join(dir, "run-"+tc.name)
			r := arxi(t, dir, append(tc.args, "--dir", runDir)...)
			if r.code != 0 {
				t.Fatalf("run start failed (%d):\n%s", r.code, r.out)
			}
			log := filepath.Join(runDir, "events.ndjson")

			// A turn must actually have happened, and this assertion is here
			// because without it the test passed for a bad reason. Breaking
			// the loopback-credential fix on purpose made the live run refuse
			// before calling anything -- the effect error is reported in the
			// summary and the exit code stays 0, by design, since a run that
			// stopped early still has an account to give. The log then held
			// only run.started and stage.entered, `simulated` was correctly
			// false, and the test was satisfied by a run that did nothing.
			//
			// Asserting the label alone tests the label. The point is the
			// label on a log that records real work, so the work is asserted
			// too.
			if err := requireResponse(t, log); err != nil {
				t.Fatalf("no model response was recorded: %v\n"+
					"  consequence: the run reached no provider, so this test "+
					"would be checking how an empty log labels itself.\n%s",
					err, r.out)
			}

			got, ok := simulatedFlagOf(t, log)
			if !ok {
				t.Fatal("run.started carries no `simulated` field.\n" +
					"  consequence: the one fact in the log that says which " +
					"executor produced it is missing, and no other field can " +
					"supply it -- --sim is designed to be identical in every " +
					"other respect.")
			}
			if got != tc.want {
				t.Errorf("a %s run logged simulated=%v, expected %v.\n"+
					"  consequence: the log is the durable record and the only "+
					"place this distinction exists. A real run labelled a "+
					"simulation gets its costs dismissed as pretend; a "+
					"simulation labelled real gets credited with work nobody "+
					"did.", tc.name, got, tc.want)
			}
		})
	}
}

// requireResponse reports whether the log contains a successful llm.response.
//
// It rejects an explicit ok:false rather than merely requiring the event,
// because a refusal is also recorded as an llm.response -- that is the
// executor's contract, a domain failure is a fact and belongs in the log. For
// this test's purpose an event saying the provider said no is the same as no
// event: no turn was taken and there is nothing whose labelling matters.
//
// An ABSENT ok is a success, not a refusal, and the difference was found by
// running it: the fake executor omits the field (internal/exec/fake.go), the
// live one always sets it. Reading absence as failure made the --sim subtest
// fail against a perfectly good simulated log -- a test wrong about the code
// rather than the reverse, which is the error the "correct the code, not the
// test" rule is easiest to misapply to.
func requireResponse(t *testing.T, path string) error {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("cannot read the log at %s: %w", path, err)
	}
	var types []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev struct {
			Type    string         `json:"type"`
			Payload map[string]any `json:"payload"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			return fmt.Errorf("the log is not NDJSON: %w", err)
		}
		types = append(types, ev.Type)
		if ev.Type == "llm.response" {
			v, present := ev.Payload["ok"]
			if !present {
				return nil // the fake does not set it; absence is not failure
			}
			if ok, _ := v.(bool); ok {
				return nil
			}
			return fmt.Errorf("the only llm.response is a refusal: %v",
				ev.Payload["error"])
		}
	}
	return fmt.Errorf("the log holds %v and no llm.response", types)
}

// simulatedFlagOf reads run.started's `simulated` out of a log.
//
// It reads the LOG and not the terminal output, because the terminal is gone
// tomorrow and the log is what an incident is reconstructed from.
func simulatedFlagOf(t *testing.T, path string) (bool, bool) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read the log at %s: %v", path, err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev struct {
			Type    string         `json:"type"`
			Payload map[string]any `json:"payload"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("the log is not NDJSON: %v\n%s", err, line)
		}
		if ev.Type != "run.started" {
			continue
		}
		v, present := ev.Payload["simulated"]
		if !present {
			return false, false
		}
		b, isBool := v.(bool)
		if !isBool {
			t.Fatalf("`simulated` is %T, not a bool: %v", v, v)
		}
		return b, true
	}
	t.Fatalf("no run.started in %s", path)
	return false, false
}

// startLoopbackModelServer serves one OpenAI-compatible reply on 127.0.0.1.
//
// In-process and on a port the OS chooses: a fixed port makes two packages'
// tests collide when run in parallel, which fails as a timeout somewhere
// unrelated rather than as a bound-address error here.
//
// It reports a KNOWN-priced model (llama3.1 is priced at zero, deliberately, so
// a loopback run is free rather than unpriced) and a usage block, because the
// executor charges the budget from what the provider reports.
func startLoopbackModelServer(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			// Not fatal from a handler goroutine; recorded as a failure.
			t.Errorf("a credential was sent to a loopback provider that has "+
				"none configured: %q\n"+
				"  consequence: a key crossing a link arxi decided was safe "+
				"BECAUSE it is loopback.", got)
		}
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"cmpl-1","model":"llama3.1","choices":[{"index":0,`+
			`"finish_reason":"stop","message":{"role":"assistant",`+
			`"content":"The test fails on a nil map write."}}],`+
			`"usage":{"prompt_tokens":812,"completion_tokens":137,"total_tokens":949}}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// A real group with a bad subcommand must not be reported as a group that does
// not exist.
//
// `arxi model --help` answered "model --help does not exist in the surface",
// which names the GROUP and is false: `model` is declared and three of its
// subcommands are built. The user was told the opposite of the truth about the
// one word they got right, and sent to look for a typo they had not made.
//
// The two answers are for different mistakes. "does not exist in the surface"
// means check the spelling. "is not a model command" means the group is real and
// only the verb is wrong, which is why it lists the verbs that would work.
//
// `trigger` and `eval` already answered correctly from their own dispatchers, so
// this was inconsistent as well as wrong -- the same input got a helpful answer
// from two groups and a misleading one from five.
func TestABadSubcommandBlamesTheSubcommandNotTheGroup(t *testing.T) {
	dir := workdir(t)

	// Every group the surface declares subcommands for, so a group added later
	// is covered without editing this test.
	groups := map[string]bool{}
	for _, c := range surface.Registry {
		if len(c.Path) >= 2 {
			groups[c.Path[0]] = true
		}
	}
	if len(groups) == 0 {
		t.Fatal("no multi-word groups found in the registry; this test would " +
			"pass vacuously")
	}

	for g := range groups {
		r := arxi(t, dir, g, "definitely-not-a-verb")
		if strings.Contains(r.out, "does not exist in the surface") {
			t.Errorf("arxi %s definitely-not-a-verb says the whole thing is "+
				"not in the surface:\n%s\n"+
				"  consequence: %q IS in the surface. The user is told the "+
				"opposite of the truth about the one word they got right, and "+
				"goes looking for a typo they did not make.", g, r.out, g)
			continue
		}
		if !strings.Contains(r.out, g) {
			t.Errorf("arxi %s definitely-not-a-verb does not name the group:\n%s",
				g, r.out)
		}
		// The refusal has to say what WOULD work, because a refusal that only
		// says no leaves the user running `arxi surface` to find out -- the
		// work the message exists to save.
		//
		// It checks that a real verb is NAMED rather than that a particular
		// phrase appears. The first version required "it accepts:" and failed
		// on `trigger` and `eval`, which print a fuller `usage:` block listing
		// every subcommand with its arguments. That is better than the phrasing
		// the test demanded, so the test was wrong, not the code -- and pinning
		// wording here would have quietly punished the two groups that did the
		// most for the user.
		if !strings.Contains(r.out, "not implemented yet") {
			named := false
			for _, sub := range surface.SubcommandsOf(g) {
				if strings.Contains(r.out, sub) {
					named = true
					break
				}
			}
			if !named {
				t.Errorf("arxi %s definitely-not-a-verb refuses without "+
					"naming a single verb that would work:\n%s\n"+
					"  consequence: the user has to go read `arxi surface` to "+
					"recover from a one-word mistake the binary could have "+
					"corrected in place.", g, r.out)
			}
		}
		if r.code == 0 {
			t.Errorf("arxi %s definitely-not-a-verb exited 0; a refusal that "+
				"reports success is worse than a wrong message, because a "+
				"script cannot see it", g)
		}
	}
}

// The article has to agree with the group name.
//
// Small, and it is here because the wrong constant was the more visible one:
// hardcoding "an" produced "is not an model command" on five of seven groups,
// and only "agent" and "eval" read correctly. Sloppiness in the one message
// whose entire job is to be believed about what went wrong costs more than the
// fix.
func TestTheRefusalArticleAgreesWithTheGroupName(t *testing.T) {
	dir := workdir(t)
	for _, tc := range []struct{ group, want string }{
		{"model", "is not a model command"},
		{"run", "is not a run command"},
		{"agent", "is not an agent command"},
		{"eval", "is not an eval command"},
	} {
		r := arxi(t, dir, tc.group, "definitely-not-a-verb")
		if !strings.Contains(r.out, tc.want) {
			t.Errorf("arxi %s: expected %q in the refusal, got:\n%s",
				tc.group, tc.want, r.out)
		}
	}
}

// TestTheUsageScreenListsWhatIsActuallyImplemented asks the binary about every
// command the usage screen advertises under IMPLEMENTED TODAY.
//
// The usage screen is the third place the same claim is made -- the README
// table, the package doc comment, and `arxi` with no arguments -- and it is the
// one a user reads FIRST. It had already drifted twice, saying `run start ...
// (--sim only today)` and that the rest of the surface "has no executor yet",
// both false since the live executor landed, and neither breaking anything. A
// doc that cannot go stale without turning the build red is worth more than one
// that merely happens to be correct today.
//
// The binary is its own oracle here, which is the point: the screen and the
// dispatcher cannot drift apart without this failing, and no list has to be
// maintained in two places.
func TestTheUsageScreenListsWhatIsActuallyImplemented(t *testing.T) {
	dir := workdir(t)

	help := arxi(t, dir)
	if help.out == "" {
		t.Fatal("arxi with no arguments printed nothing")
	}

	listed := commandsListedAsImplemented(help.out)
	if len(listed) < 5 {
		t.Fatalf("parsed only %d commands out of the IMPLEMENTED TODAY block, which means the parser broke rather than the screen; got %q\nscreen:\n%s",
			len(listed), listed, help.out)
	}

	// EVERY line must have produced an entry, and this is the correction that
	// matters most in this test.
	//
	// The parser skips a line it cannot resolve against the registry, and a
	// skipped line is indistinguishable from a line that passed. That is not
	// hypothetical: `why`, `surface` and `version` were all implemented,
	// advertised, and undeclared, so three of eleven lines were skipped and this
	// guard examined eight of the eleven commands it appeared to examine — while
	// reporting success.
	//
	// Counting lines against entries turns that class of hole from invisible into
	// a failure, which is the only difference between a guard and a decoration.
	if n := linesInImplementedBlock(help.out); n != len(listed) {
		t.Errorf("the IMPLEMENTED TODAY block has %d lines but only %d resolved to a command: %q\n"+
			"  the unresolved ones were SKIPPED, not checked, so this test was "+
			"quietly examining less than it claims.\n"+
			"  a line goes unresolved when its leading words are neither a declared "+
			"path nor a declared CLIAlias -- so either the screen advertises "+
			"something undeclared, or something real is missing from the registry.\nscreen:\n%s",
			n, len(listed), listed, help.out)
	}

	firstWords := make([]string, 0, len(listed))
	for _, e := range listed {
		firstWords = append(firstWords, strings.Fields(e)[0])
	}

	// Direction one: everything advertised is real.
	for _, entry := range listed {
		// The FULL path is probed, not just the first word, and that correction
		// came from watching this test fail to fail. A bogus `agent create`
		// sailed past the first version, because probing only `agent` gets
		// "does not exist in the surface" -- which this test was not looking
		// for. Truncating a command path turns "not built yet" into "no such
		// command", the same bug in the same flattering direction that made the
		// capability probe report 36.2% instead of 34.0% by truncating
		// `agent tool policy` to `agent tool`.
		path := strings.Fields(entry)
		r := arxi(t, dir, append(append([]string{}, path...), "--help")...)

		if strings.Contains(r.out, "not implemented yet") {
			t.Errorf("the usage screen lists %q under IMPLEMENTED TODAY, but the binary says it is not implemented yet:\n%s\n  consequence: the first thing a user reads promises a command that refuses them.",
				entry, r.out)
		}
		if strings.Contains(r.out, "does not exist in the surface") {
			t.Errorf("the usage screen lists %q under IMPLEMENTED TODAY, but the binary does not know that path at all:\n%s\n  consequence: worse than unimplemented -- the screen names something that was never declared.",
				entry, r.out)
		}
	}

	// Direction two: everything real is advertised. Without this the screen
	// could pass by listing less and less, which is how it went stale before.
	for _, group := range implementedTopLevelGroups(t, dir) {
		if !containsCommand(firstWords, group) {
			t.Errorf("%q is implemented but the usage screen never mentions it\n  consequence: the screen understates the binary, so a working command stays invisible to the person most likely to need it.",
				group)
		}
	}
}

// commandsListedAsImplemented pulls command paths out of the IMPLEMENTED TODAY
// block, taking the longest leading run of words the surface recognises.
//
// The block's lines are "path  description", with no marker between the two, so
// the split has to be discovered rather than assumed: `run start <bp> <prompt>`
// is two path words, `blueprint validate <file>` is two, `schema` is one. The
// surface registry is the only thing that actually knows.
func commandsListedAsImplemented(help string) []string {
	lines := strings.Split(help, "\n")
	start := -1
	for i, ln := range lines {
		if strings.TrimSpace(ln) == "IMPLEMENTED TODAY" {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return nil
	}

	var out []string
	for _, ln := range lines[start:] {
		if strings.TrimSpace(ln) == "" {
			break // the block ends at the first blank line
		}
		fields := strings.Fields(ln)
		if len(fields) == 0 {
			break
		}
		best := 0
		for n := 1; n <= len(fields); n++ {
			if !isPathWord(fields[n-1]) {
				break
			}
			if isKnownPrefix(fields[:n]) {
				best = n
			}
		}
		if best > 0 {
			out = append(out, strings.Join(fields[:best], " "))
		}
	}
	return out
}

// linesInImplementedBlock counts the non-blank lines of the IMPLEMENTED TODAY
// block, so the caller can tell "checked" from "skipped".
//
// It shares no code with commandsListedAsImplemented on purpose. The point is a
// second, dumber opinion about how many commands are on the screen: a counter
// that reused the same resolution logic would agree with it about exactly the
// lines it got wrong, which is precisely the agreement that hid three of them.
func linesInImplementedBlock(help string) int {
	lines := strings.Split(help, "\n")
	start := -1
	for i, ln := range lines {
		if strings.TrimSpace(ln) == "IMPLEMENTED TODAY" {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return 0
	}
	n := 0
	for _, ln := range lines[start:] {
		if strings.TrimSpace(ln) == "" {
			break
		}
		n++
	}
	return n
}

// isPathWord is a cheap filter so placeholders and prose never reach the
// registry lookup: real path words are lowercase letters and hyphens.
func isPathWord(w string) bool {
	if w == "" {
		return false
	}
	for _, r := range w {
		if (r < 'a' || r > 'z') && r != '-' {
			return false
		}
	}
	return true
}

// isKnownPrefix reports whether p is a declared command or the start of one.
//
// LookupCLI, not Lookup, and that one word is the whole reason Cmd.CLIAlias
// exists. `why` is a real, implemented, advertised invocation that has no Path
// of its own, so Lookup returned nil, so the `why <file>` line of the usage
// screen matched nothing and produced no entry. This function reporting false
// did not fail the guard — it made the guard SKIP a line and pass.
func isKnownPrefix(p []string) bool {
	if surface.LookupCLI(p...) != nil {
		return true
	}
	if len(p) == 1 {
		return len(surface.SubcommandsOf(p[0])) > 0
	}
	return false
}

// implementedTopLevelGroups asks the binary which top-level words it actually
// runs, so direction two of the guard needs no maintained list either.
//
// A group counts as implemented when at least one of its SUBCOMMANDS runs, and
// that is deliberate rather than incidental. The first version asked the group
// word itself -- `arxi agent --help` -- and treated the absence of a refusal as
// success. That was already fragile, and the group-aware refusal added in this
// same body of work broke it outright: the new message is neither "not
// implemented yet" nor "does not exist in the surface", so every declared group
// suddenly looked implemented and the guard reported agent, role, state and
// event as missing from a screen that is right to omit them. The test was
// wrong, not the screen.
//
// The lesson is the one this file keeps relearning: a group word is not a
// command, and asking about a truncated path answers a different question than
// the one being asked.
func implementedTopLevelGroups(t *testing.T, dir string) []string {
	t.Helper()

	seen := map[string]bool{}
	var out []string
	for i := range surface.Registry {
		g := surface.Registry[i].Path[0]
		if seen[g] {
			continue
		}
		seen[g] = true

		// serve is skipped because without --listen it speaks the protocol on
		// stdin and would block this test forever.
		if g == "serve" {
			continue
		}

		// The registry's paths ARE the declared commands, at whatever depth each
		// one really has, so they are used directly rather than rebuilt from
		// the group word. Reconstructing them assumed two words and missed
		// `agent tool policy`, whose parent `agent tool` answers "needs a
		// subcommand" -- neither a refusal nor a run, which is exactly how a
		// group came to look implemented on the strength of a command that does
		// not exist.
		for j := range surface.Registry {
			p := surface.Registry[j].Path
			if p[0] != g {
				continue
			}

			// A short deadline, because this runs real commands rather than
			// asking for help: `trigger run` and friends would otherwise sit in
			// their loop until the test binary times out. Whether the refusal
			// was printed is decided long before the deadline.
			if isRefusal(arxiBounded(t, dir, 3*time.Second, p...)) {
				continue
			}
			out = append(out, g)
			break
		}
	}
	return out
}

// isRefusal reports whether output is the binary declining to run a command,
// rather than the command running.
//
// One named place for the set, because the set grew twice while this guard was
// being written and both times the guard broke silently rather than loudly. It
// started as two strings; the group-aware refusal made three and every declared
// group started looking implemented; "needs a subcommand" made four. A test that
// infers success from the absence of known failure strings is only as good as
// its list, and that is a bad property to leave spread across call sites.
func isRefusal(out string) bool {
	for _, s := range []string{
		"not implemented yet",
		"does not exist in the surface",
		"is not a", // covers "is not a X command" and "is not an X command"
		"needs a subcommand",
	} {
		if strings.Contains(out, s) {
			return true
		}
	}
	return false
}

// containsCommand reports whether want is in list.
func containsCommand(list []string, want string) bool {
	for _, e := range list {
		if e == want {
			return true
		}
	}
	return false
}
