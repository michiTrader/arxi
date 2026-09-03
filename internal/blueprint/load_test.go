package blueprint

import (
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/kernel"
)

const designDocBlueprint = `name: feature-team
members:
  - {name: backend,  role: implementer, tools: [read, write, bash]}
  - {name: frontend, role: implementer, tools: [read, write]}
  - {name: security, role: reviewer,    tools: [read], advisory: true}
stages:
  - {name: build,  advance_when: all,      timeout_ms: 1800000}
  - {name: review, advance_when: quorum:2}
interaction:
  steer_target: coordinator
`

func mustLoad(t *testing.T, src string) *Blueprint {
	t.Helper()
	bp, err := Load([]byte(src))
	if err != nil {
		t.Fatalf("blueprint did not load: %v", err)
	}
	return bp
}

// TestResolvedDefaultsMatchTheDocumentedOutput protects the five lines
// `blueprint validate` prints in docs/design/20-use-cases.md §20.4. Three of
// them are decisions the user never wrote, and the doc's whole argument is that
// a default you cannot see is indistinguishable from a bug when it fires.
func TestResolvedDefaultsMatchTheDocumentedOutput(t *testing.T) {
	bp := mustLoad(t, designDocBlueprint)
	c := bp.Config

	if c.Workspace != "worktree" {
		t.Errorf("workspace = %q, expected worktree. backend and frontend both hold write, "+
			"and two writers sharing one directory overwrite each other's work; the KV lock "+
			"does not prevent it because only the filesystem gives isolation", c.Workspace)
	}
	for _, st := range c.Stages {
		if st.OnTimeout != "escalate" {
			t.Errorf("stage %s: on_timeout = %q, expected escalate. Failing by default trains "+
				"users to set absurdly long timeouts, which is worse than having none", st.Name, st.OnTimeout)
		}
	}
	for _, m := range c.Members {
		if m.Activation != "coalesce" {
			t.Errorf("member %s: activation = %q, expected coalesce. One turn per event "+
				"multiplies the bill by the number of causes in exchange for nothing", m.Name, m.Activation)
		}
	}
	if c.Inter.SteerTarget != "coordinator" {
		t.Errorf("steer_target = %q, expected coordinator", c.Inter.SteerTarget)
	}
	if c.Members[2].Name != "security" || !c.Members[2].Advisory {
		t.Errorf("security must be advisory: it gives an opinion and does not count toward advance rules")
	}
}

// TestWorkspaceStaysNoneWhenNobodyCanWrite protects that the fatal-hole default
// is triggered by a mechanical condition and not applied to everything. Forcing
// a worktree on read-only members would cost setup for isolation nobody needs,
// and a default that fires when it should not gets disabled by users, taking
// the case that mattered with it.
func TestWorkspaceStaysNoneWhenNobodyCanWrite(t *testing.T) {
	bp := mustLoad(t, "name: readers\nmembers:\n  - {name: a, tools: [read]}\n  - {name: b, tools: [read]}\n")
	if bp.Config.Workspace != "none" {
		t.Fatalf("workspace = %q, expected none: no member holds write, bash or edit, "+
			"so there is nothing to isolate", bp.Config.Workspace)
	}
}

// TestUnsatisfiableQuorumIsRejected protects against the exact failure ADR-0004
// exists for. A quorum above the member count does not make the run fail; it
// makes it go silent after everyone has submitted, which the design doc calls
// the case that is hardest to see by eye. Refusing the blueprint costs nothing;
// discovering it at runtime costs a full run of paid turns.
func TestUnsatisfiableQuorumIsRejected(t *testing.T) {
	_, err := Load([]byte("name: t\nmembers:\n  - {name: a}\n  - {name: b}\n  - {name: c}\nstages:\n  - {name: s, advance_when: quorum:5}\n"))
	if err == nil {
		t.Fatal("quorum:5 over 3 members was accepted. The run will not fail, it will go " +
			"quiescent once everybody has submitted, after paying for every turn")
	}
	if !strings.Contains(err.Error(), "never be satisfied") {
		t.Fatalf("error was %q; it must say the rule can never be satisfied, otherwise the "+
			"user rewrites the members instead of the rule", err)
	}
}

func TestQuorumEqualToMemberCountIsAccepted(t *testing.T) {
	// The boundary matters: quorum:3 over 3 members is `all` spelled
	// differently, and rejecting it would refuse a valid blueprint.
	if _, err := Load([]byte("name: t\nmembers:\n  - {name: a}\n  - {name: b}\n  - {name: c}\nstages:\n  - {name: s, advance_when: quorum:3}\n")); err != nil {
		t.Fatalf("quorum:3 over 3 members must be valid, it is `all` written another way: %v", err)
	}
}

// TestUnknownKeysAreRejectedWithASuggestion is the reason Parse returns
// map[string]any instead of decoding into a struct. A dropped `advance_when`
// leaves the stage on the default rule and the file describes a run that never
// ran.
func TestUnknownKeysAreRejectedWithASuggestion(t *testing.T) {
	cases := []struct {
		name       string
		src        string
		wantTypo   string
		wantIntent string
	}{
		{"stage rule", "name: t\nstages:\n  - {name: s, advance_wen: all}\n", "advance_wen", "advance_when"},
		{"member tools", "name: t\nmembers:\n  - {name: a, tool: [read]}\n", "tool", "tools"},
		{"top level", "name: t\nmembrs: []\n", "membrs", "members"},
		{"timeout", "name: t\nstages:\n  - {name: s, timeout_m: 10}\n", "timeout_m", "timeout_ms"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load([]byte(tc.src))
			if err == nil {
				t.Fatalf("%q was accepted; the key is silently dropped and the run uses a "+
					"rule the file does not show", tc.wantTypo)
			}
			if !strings.Contains(err.Error(), tc.wantIntent) {
				t.Fatalf("error was %q; it must suggest %q, otherwise the user re-reads a "+
					"file whose typo they already cannot see", err, tc.wantIntent)
			}
		})
	}
}

// TestAGrantOfAToolThatDoesNotExistIsRejected closes the one way an unknown tool
// name used to reach a run.
//
// Every other writer of a grant validated already -- agentstore, rolestore,
// toolstore.Set, `agent create`, `role define` -- and a file did not, so this was
// the gap: `tools: [bahs]` passed `blueprint validate`, got frozen into
// blueprint.snapshot.yaml looking reviewed, resolved to ALLOW rather than to
// nothing (it is absent from the mutating set, so it read as a reader), and then
// stopped the run on ErrUnknownTool at the first call -- after the turn that
// produced that call was billed.
//
// Refusing here costs nothing, which is the whole argument: nothing has been
// spent at validate time.
func TestAGrantOfAToolThatDoesNotExistIsRejected(t *testing.T) {
	_, err := Load([]byte("name: t\nmembers:\n  - {name: a, tools: [read, telepathy]}\n"))
	if err == nil {
		t.Fatal("tools: [read, telepathy] was accepted\n" +
			"  consequence: the grant resolves to allow because it is not in the " +
			"mutating set, `agent show` prints a policy for a tool with no body, and " +
			"the run dies at the first call to it with the turn already paid for.")
	}
	if !strings.Contains(err.Error(), "telepathy") {
		t.Fatalf("error was %q; it must name the offending tool, or the user diffs "+
			"the grant list against the known tools by hand", err)
	}
	if !strings.Contains(err.Error(), "members[0]") {
		t.Fatalf("error was %q; it must locate the member, because a team of six "+
			"otherwise turns one typo into six candidates", err)
	}
}

// TestAToolTypoSuggestsTheToolThatWasMeant. Naming the bad value is the floor;
// this is the difference between a refusal and a fix. `bahs` and `bash` differ by
// a transposition the eye reads straight over, which is exactly why the file was
// saved that way in the first place.
func TestAToolTypoSuggestsTheToolThatWasMeant(t *testing.T) {
	for typo, want := range map[string]string{"bahs": "bash", "reed": "read", "wirte": "write"} {
		_, err := Load([]byte("name: t\nmembers:\n  - {name: a, tools: [" + typo + "]}\n"))
		if err == nil {
			t.Fatalf("tools: [%s] was accepted", typo)
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("tools: [%s] gave %q; it must suggest %q, otherwise the user "+
				"re-reads a grant list whose typo they already could not see",
				typo, err, want)
		}
	}
}

// TestEveryKnownToolIsAcceptedInAGrant is the other half, and it is not padding:
// a validator that rejects a real tool name is worse than the hole it replaced,
// because the file it refuses is correct and the user has nothing to fix. It
// reads the list from the kernel so adding a tool cannot leave this behind.
func TestEveryKnownToolIsAcceptedInAGrant(t *testing.T) {
	for _, name := range kernel.KnownTools {
		raw := "name: t\nmembers:\n  - {name: a, tools: [" + name + "]}\n"
		bp, err := Load([]byte(raw))
		if err != nil {
			t.Errorf("tools: [%s] was refused: %v\n"+
				"  consequence: a grant the binary implements does not load, so the "+
				"remedy the message asks for is impossible.", name, err)
			continue
		}
		if got := bp.Config.Members[0].Tools; len(got) != 1 || got[0] != name {
			t.Errorf("tools: [%s] loaded as %v, want exactly [%s]", name, got, name)
		}
	}
}

// TestADuplicateGrantIsNotAnError. `[read, read]` is redundant, not wrong, and
// ResolveAll already collapses it. Refusing it would reject a file that behaves
// correctly, and the validator's job is to catch what cannot work.
func TestADuplicateGrantIsNotAnError(t *testing.T) {
	if _, err := Load([]byte("name: t\nmembers:\n  - {name: a, tools: [read, read]}\n")); err != nil {
		t.Errorf("tools: [read, read] was refused: %v\n"+
			"  consequence: a redundant grant is treated as a broken one, and the "+
			"user is asked to fix a file that already does what they meant.", err)
	}
}

// TestEnumTyposAreRejected protects the fatal hole from the other direction: a
// misspelled `worktree` would fall back to the default and quietly drop the
// isolation the user explicitly asked for.
func TestEnumTyposAreRejected(t *testing.T) {
	_, err := Load([]byte("name: t\nworkspace: worktee\nmembers:\n  - {name: a, tools: [write]}\n"))
	if err == nil {
		t.Fatal("workspace: worktee was accepted; the user asked for isolation and would " +
			"silently not get it")
	}
	if !strings.Contains(err.Error(), "worktree") {
		t.Fatalf("error was %q; it must name the intended value", err)
	}
}

// TestWatcherOnUndeclaredAgentIsRejected: a watcher naming a member that does
// not exist never fires and nothing reports it. The user sees a rule in the
// file and a reaction that never happens, which is indistinguishable from the
// watcher being broken.
func TestWatcherOnUndeclaredAgentIsRejected(t *testing.T) {
	_, err := Load([]byte("name: t\nmembers:\n  - {name: a}\nwatchers:\n  - {agent: ghost, pattern: stage.*}\n"))
	if err == nil {
		t.Fatal("a watcher on an undeclared agent was accepted; it would never fire and " +
			"nothing would say so")
	}
	if !strings.Contains(err.Error(), "never fire") {
		t.Fatalf("error was %q; it must say the watcher never fires", err)
	}
}

// TestIncludeSelfIsRejected protects the one setting in the file that can bill
// an unbounded amount. A watcher on `agent.*` woken by its own events is an
// infinite loop with a credit card, and self-exclusion is one of the two cheap
// filters that run before a single token is spent.
func TestIncludeSelfIsRejected(t *testing.T) {
	_, err := Load([]byte("name: t\nmembers:\n  - {name: a}\nwatchers:\n  - {agent: a, pattern: agent.*, include_self: true}\n"))
	if err == nil {
		t.Fatal("include_self: true was accepted silently; this is the one setting that can " +
			"run up an unbounded bill")
	}
	if !strings.Contains(err.Error(), "infinite loop") {
		t.Fatalf("error was %q; it must name the consequence, not just flag the field", err)
	}
}

// TestUnsupportedWildcardIsRejected: a pattern that parses here and matches
// nothing in the reducer's matchPattern looks like a broken watcher rather than
// an unsupported pattern.
func TestUnsupportedWildcardIsRejected(t *testing.T) {
	for _, p := range []string{"stage.*.done", "*.entered"} {
		src := "name: t\nmembers:\n  - {name: a}\nwatchers:\n  - {agent: a, pattern: \"" + p + "\"}\n"
		if _, err := Load([]byte(src)); err == nil {
			t.Errorf("pattern %q was accepted; the reducer supports only a single trailing "+
				"wildcard, so it would silently match nothing", p)
		}
	}
}

// TestWithdrawnTurnSourceIsExplained: someone carrying a blueprint over from
// the first draft deserves to know the key was withdrawn and why, rather than
// have it ignored.
func TestWithdrawnTurnSourceIsExplained(t *testing.T) {
	_, err := Load([]byte("name: t\ninteraction:\n  turn_source: coordinator\n"))
	if err == nil {
		t.Fatal("turn_source was accepted; it was withdrawn by ADR-0006 and ignoring it " +
			"leaves the user believing a race is handled")
	}
	if !strings.Contains(err.Error(), "ADR-0006") {
		t.Fatalf("error was %q; it must point at the decision that withdrew the key", err)
	}
}

// TestDuplicateMemberIsRejected: State.Member() returns the first match, so the
// second member with that name silently never works and never shows as blocked.
func TestDuplicateMemberIsRejected(t *testing.T) {
	if _, err := Load([]byte("name: t\nmembers:\n  - {name: a}\n  - {name: a}\n")); err == nil {
		t.Fatal("two members named `a` were accepted; lookups return the first, so the " +
			"second never runs and never reports itself blocked")
	}
}

// TestBudgetWarnPctAsPercentageIsRejected: 80 instead of 0.8 puts the warning
// threshold above any budget, so the warning never fires and the user finds out
// from the bill.
func TestBudgetWarnPctAsPercentageIsRejected(t *testing.T) {
	if _, err := Load([]byte("name: t\nbudget_warn_pct: 80\n")); err == nil {
		t.Fatal("budget_warn_pct: 80 was accepted; the warning would never fire and the " +
			"overspend would surface on the invoice")
	}
	bp := mustLoad(t, "name: t\nbudget_warn_pct: 0.5\n")
	if bp.Config.BudgetWarnPct != 0.5 {
		t.Errorf("budget_warn_pct = %v, expected 0.5", bp.Config.BudgetWarnPct)
	}
}

// TestAllProblemsAreReportedAtOnce: returning only the first error means one
// fix per run, which is how people stop using a validator and start guessing.
func TestAllProblemsAreReportedAtOnce(t *testing.T) {
	src := "name: t\nmembers:\n  - {name: a, tool: [read]}\nstages:\n  - {name: s, advance_wen: all, on_timeout: escalat}\n"
	_, err := Load([]byte(src))
	if err == nil {
		t.Fatal("a blueprint with three mistakes was accepted")
	}
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("error is %T, expected *ValidationError so callers can list every problem", err)
	}
	if len(ve.Problems) < 3 {
		t.Fatalf("reported %d problems, expected at least 3:\n%s\nfixing one typo per run is "+
			"how people give up on a validator", len(ve.Problems), err)
	}
}

// TestSHAPinsTheExactBytes protects ADR-0002. The digest covers the original
// file, not a re-serialization of the parsed config: a paraphrase would make
// the frozen snapshot differ from what the user wrote, and a replay would then
// reproduce something that never ran.
func TestSHAPinsTheExactBytes(t *testing.T) {
	a := mustLoad(t, designDocBlueprint)
	b := mustLoad(t, designDocBlueprint)
	if a.SHA != b.SHA {
		t.Fatal("the same bytes produced different digests: the blueprint could not be pinned at all")
	}
	if len(a.SHA) != 64 {
		t.Errorf("SHA = %q, expected a 64-char sha256", a.SHA)
	}

	// A comment changes no behaviour but does change the bytes, and the digest
	// must follow the bytes: it identifies the file that was frozen, and the
	// snapshot on disk is that file verbatim.
	c := mustLoad(t, "# a comment\n"+designDocBlueprint)
	if c.SHA == a.SHA {
		t.Error("adding a comment did not change the digest; the digest must identify the " +
			"exact bytes frozen into runs/<id>/blueprint.snapshot.yaml")
	}
	if string(a.Raw) != designDocBlueprint {
		t.Error("Raw is not the original source; the frozen snapshot has to be the file the " +
			"user wrote, not a paraphrase of it")
	}
}

// TestLoadDoesNotAliasTheCallersBytes: keeping the caller's slice would let a
// later write mutate the bytes the digest was computed over, so the snapshot
// and its SHA would silently disagree.
func TestLoadDoesNotAliasTheCallersBytes(t *testing.T) {
	src := []byte(designDocBlueprint)
	bp := mustLoad(t, string(src))
	src[0] = 'X'
	if bp.Raw[0] == 'X' {
		t.Fatal("Load kept the caller's slice: a later write would change the frozen bytes " +
			"without changing the digest computed from them")
	}
}

func TestValidBlueprintReportsItsName(t *testing.T) {
	bp := mustLoad(t, designDocBlueprint)
	if bp.Name != "feature-team" {
		t.Errorf("Name = %q, expected feature-team", bp.Name)
	}
	if bp.Config.Blueprint != "feature-team" {
		t.Errorf("Config.Blueprint = %q, expected feature-team", bp.Config.Blueprint)
	}
}

// TestAMemberMayNameItsModel protects the promise in §20.2: `agent create
// --model claude-sonnet-4-6`, and `agent show` printing it back. A blueprint is
// the declarative spelling of the same thing, so the field has to survive a
// load.
func TestAMemberMayNameItsModel(t *testing.T) {
	bp := mustLoad(t, "name: t\nmembers:\n  - {name: a, model: claude-sonnet-4-6}\n")
	if got := bp.Config.Members[0].Model; got != "claude-sonnet-4-6" {
		t.Errorf("model = %q, expected claude-sonnet-4-6. The run resolves this ref to "+
			"an endpoint and a credential; dropped here, every agent silently falls back "+
			"to whatever the run defaults to, which is how you get billed at one model's "+
			"rate for another model's work", got)
	}
}

// TestAMemberNeedNotNameAModel is the additive half. Every blueprint in the
// tree predates the field, including the one the design doc prints, and a
// required field would have broken all of them at once.
func TestAMemberNeedNotNameAModel(t *testing.T) {
	bp := mustLoad(t, designDocBlueprint)
	for _, m := range bp.Config.Members {
		if m.Model != "" {
			t.Errorf("member %s got model %q out of a blueprint that names none; "+
				"an invented default here is a spend decision taken behind the user's back",
				m.Name, m.Model)
		}
	}
}

// TestAQualifiedModelRefLoads: two providers can offer one id, and the
// qualified spelling is the only way to say which one. If the loader rejected
// the slash, the ambiguity `model list` reports would have no fix.
func TestAQualifiedModelRefLoads(t *testing.T) {
	bp := mustLoad(t, "name: t\nmembers:\n  - {name: a, model: anthropic/claude-sonnet-4-6}\n")
	if got := bp.Config.Members[0].Model; got != "anthropic/claude-sonnet-4-6" {
		t.Errorf("model = %q, expected the qualified ref to survive intact", got)
	}
}

// TestAModelWithStrayWhitespaceIsRejected: ids are compared exactly, so " x" is
// a model that does not exist. Caught at load, the message names the file and
// the member; caught at run time it is a refusal after the run directory exists.
func TestAModelWithStrayWhitespaceIsRejected(t *testing.T) {
	if _, err := Load([]byte("name: t\nmembers:\n  - {name: a, model: \"claude-sonnet-4-6 \"}\n")); err == nil {
		t.Fatal("a model id with a trailing space was accepted; ids are compared exactly, " +
			"so this names a model that does not exist and the run fails later for a " +
			"reason that looks like a missing provider")
	}
}

// TestAModelRefWithTwoSlashesIsRejected: the spelling is `id` or
// `provider/id`. Anything else would be split somewhere arbitrary, and the
// arbitrary choice would pick a provider.
func TestAModelRefWithTwoSlashesIsRejected(t *testing.T) {
	if _, err := Load([]byte("name: t\nmembers:\n  - {name: a, model: a/b/c}\n")); err == nil {
		t.Fatal("model `a/b/c` was accepted; the ref would be split at an arbitrary point " +
			"and the arbitrary point chooses which provider gets billed")
	}
}

// TestBlueprintValidationDoesNotConsultRegisteredProviders is the rule that
// keeps a static file's meaning static.
//
// A model that is not registered anywhere must still LOAD. Checking existence
// here would mean the same blueprint is valid in CI and invalid on a laptop
// that has not run `provider add`, and validation would start depending on the
// working directory.
func TestBlueprintValidationDoesNotConsultRegisteredProviders(t *testing.T) {
	bp := mustLoad(t, "name: t\nmembers:\n  - {name: a, model: a-model-nobody-registered}\n")
	if bp.Config.Members[0].Model != "a-model-nobody-registered" {
		t.Fatal("the unregistered model did not survive the load")
	}
}
