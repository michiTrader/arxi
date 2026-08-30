package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/surface"
)

// exp is expandShort with the command looked up by path, so the tests read like
// the invocations they describe.
func exp(t *testing.T, path string, args ...string) ([]string, error) {
	t.Helper()
	c := surface.Lookup(strings.Split(path, " ")...)
	if c == nil {
		t.Fatalf("%q is not in the registry, so this test is checking short-flag "+
			"expansion against a command that does not exist", path)
	}
	return expandShort(c, args)
}

// TestAShortFlagBecomesItsLongForm is the whole feature in one assertion.
func TestAShortFlagBecomesItsLongForm(t *testing.T) {
	got, err := exp(t, "run start", "-p", "add rate limiting", "-b", "2.00", "-S")
	if err != nil {
		t.Fatalf("expanding documented short flags failed: %v. Every letter here "+
			"is printed by `arxi surface --flags`, so if one is rejected the help "+
			"is advertising flags the parser does not accept", err)
	}
	want := []string{"--prompt", "add rate limiting", "--budget", "2.00", "--sim"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expanded to %q, want %q. The values have to survive expansion "+
			"in place: a rewrite that drops or reorders them changes the budget "+
			"or the objective of the run", got, want)
	}
}

// TestTheValueAfterAShortFlagIsNotTouched: expandShort must not interpret
// values. A value that happens to look like a flag ("-r" as the text of a
// message) belongs to the flag before it, and rewriting it would silently change
// what the user asked for.
func TestTheValueAfterAShortFlagIsNotTouched(t *testing.T) {
	got, err := exp(t, "run prompt", "-t", "-r")
	if err != nil {
		t.Fatalf("a value that looks like a flag was rejected: %v. The word after "+
			"a value-taking flag is data, and refusing it makes any text starting "+
			"with a dash unpassable", err)
	}
	want := []string{"--text", "-r"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expanded to %q, want %q. The %q was the VALUE of --text and "+
			"expanding it into --run rewrites the user's message into a different "+
			"parameter", got, want, "-r")
	}
}

// TestInlineValuesSurviveExpansion: --budget=2 is accepted, so -b=2 has to be.
// Two spellings of the same flag that differ in which forms they accept is a
// difference no help text can express and every user discovers by accident.
func TestInlineValuesSurviveExpansion(t *testing.T) {
	got, err := exp(t, "run start", "-b=2.00")
	if err != nil {
		t.Fatalf("-b=2.00 was rejected: %v. The long form --budget=2.00 works, so "+
			"refusing the short one makes the two spellings differ in a way "+
			"nothing documents", err)
	}
	want := []string{"--budget=2.00"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expanded to %q, want %q. The inline value has to stay attached: "+
			"splitting it here would hand the parser a --budget with no number",
			got, want)
	}
}

// TestLongFlagsAndPositionalsPassThroughUntouched. The expander is a pre-pass
// over a small subset; anything it does not recognise must reach the parser
// exactly as typed, or it becomes a second opinion about the whole command line.
func TestLongFlagsAndPositionalsPassThroughUntouched(t *testing.T) {
	in := []string{"./bp.yaml", "the prompt", "--budget", "2.00", "--sim", "-"}
	got, err := exp(t, "run start", in...)
	if err != nil {
		t.Fatalf("ordinary arguments were rejected by the short-flag pre-pass: "+
			"%v. It must only rewrite single-dash letters and leave the rest of "+
			"the command line alone", err)
	}
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("rewrote %q into %q. A bare %q is the conventional name for "+
			"stdin and long flags are already in final form; changing either "+
			"means the pre-pass is making decisions that belong to the parser",
			in, got, "-")
	}
}

// TestEverythingAfterADoubleDashIsData is the escape hatch.
//
// A prompt is free text and can start with a dash: "--sim is broken" is a
// legitimate objective. Without `--` it is unpassable, and quoting does not help
// because the quotes belong to the shell and are gone before main runs.
func TestEverythingAfterADoubleDashIsData(t *testing.T) {
	got, err := exp(t, "run start", "-S", "--", "-p", "-b")
	if err != nil {
		t.Fatalf("the -- escape was rejected: %v. Without it, any prompt "+
			"beginning with a dash cannot be given at all", err)
	}
	want := []string{"--sim", "--", "-p", "-b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expanded to %q, want %q. Letters after -- must survive as "+
			"literal text; expanding them turns the user's words into flags",
			got, want)
	}
}

// TestADashedPromptSurvivesTheWholePipeline checks the escape end to end, since
// expandShort passing `--` through is only half the path: parseStartFlags has to
// honour it too. The first implementation expanded correctly and then died on
// "unknown flag --", which made the escape hatch look present and be absent.
func TestADashedPromptSurvivesTheWholePipeline(t *testing.T) {
	args, err := exp(t, "run start", "bp.yaml", "-b", "1", "-S", "--", "--sim is broken")
	if err != nil {
		t.Fatalf("expansion rejected a dashed prompt behind --: %v", err)
	}
	f, err := parseStartFlags(args)
	if err != nil {
		t.Fatalf("parseStartFlags rejected %q: %v.\nEverything after -- is data. "+
			"If the parser treats it as a flag, a prompt beginning with a dash "+
			"cannot be given at all and the escape hatch exists only in the "+
			"pre-pass", args, err)
	}
	if f.prompt != "--sim is broken" {
		t.Fatalf("the prompt came through as %q, want %q. A prompt that is "+
			"silently altered runs a different objective than the one asked for",
			f.prompt, "--sim is broken")
	}
	if f.sim != true {
		t.Fatal("--sim before the -- was lost. The escape must stop flag parsing " +
			"at the point it appears, not retroactively unset what came before")
	}
}

// TestALetterTheCommandHasNoParameterForIsRefused.
//
// This is the decision behind LongFor taking a receiver. -r is --run on thirteen
// commands, so a user will try it on the fourteenth. Binding it to some other
// parameter, or accepting and ignoring it, means the value is discarded and the
// command reports success on input it never read.
func TestALetterTheCommandHasNoParameterForIsRefused(t *testing.T) {
	_, err := exp(t, "blueprint validate", "-r", "r1")
	if err == nil {
		t.Fatal("blueprint validate accepted -r, which it has no parameter for. " +
			"Accepting it means the run id is silently dropped and the command " +
			"validates whatever file it finds, reporting success on input the " +
			"user believes was used")
	}
	// The message has to name what the letter means elsewhere. "unknown flag"
	// sends a user who learned -r on `run why` hunting for a typo they did not
	// make.
	if !strings.Contains(err.Error(), "--run") {
		t.Fatalf("the refusal was %q, which does not mention --run. A letter that "+
			"is valid elsewhere in the surface has to say so, or the user cannot "+
			"tell a typo from a parameter this command does not have", err)
	}
}

// TestAnUnknownLetterListsWhatIsAccepted: a rejection that does not say what
// WOULD work leaves the user guessing, and the next guess is another wrong flag.
func TestAnUnknownLetterListsWhatIsAccepted(t *testing.T) {
	_, err := exp(t, "run start", "-z", "x")
	if err == nil {
		t.Fatal("-z was accepted although no parameter in the surface uses it. " +
			"An invented letter must fail, or a typo becomes a silently ignored " +
			"argument")
	}
	for _, want := range []string{"-b", "-p"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal of -z was %q, which does not offer %s. Listing "+
				"the flags this command does take is what turns a rejection into "+
				"a fix; without it the user's next attempt is another guess",
				err, want)
		}
	}
}

// TestBooleansCanBeGrouped keeps the shorthand worth using: -SJ is the reason
// single letters exist at all.
func TestBooleansCanBeGrouped(t *testing.T) {
	c := surface.Lookup("run", "why")
	if c == nil {
		t.Fatal("run why is missing; this test needs a non-mutating command with " +
			"a boolean short flag to check grouping")
	}
	got, err := expandShort(c, []string{"-J"})
	if err != nil {
		t.Fatalf("-J alone was rejected on run why: %v. It is the synthesized "+
			"--json flag every reading command has, so the letter must reach it",
			err)
	}
	if !reflect.DeepEqual(got, []string{"--json"}) {
		t.Fatalf("-J expanded to %q, want [--json]. WireParams synthesizes json, "+
			"so an expander reading Params alone would fail here", got)
	}
}

// TestAValueTakingFlagCannotBeGrouped is the one place expansion refuses rather
// than guesses.
//
// In `-Sb 2` there is no way to know whether 2 belongs to b, and a parser that
// guesses about a spend ceiling is exactly the failure --budget's
// mandatory-ness was designed to prevent. Silently attaching it to the last
// letter would work often enough to be trusted and then be wrong.
func TestAValueTakingFlagCannotBeGrouped(t *testing.T) {
	_, err := exp(t, "run start", "-Sb", "2.00")
	if err == nil {
		t.Fatal("-Sb was expanded although -b takes a value. Inside a group " +
			"there is nothing to say which letter 2.00 belongs to, so accepting " +
			"it means the budget is set by a guess")
	}
	if !strings.Contains(err.Error(), "--budget") {
		t.Fatalf("the refusal was %q, which does not name --budget as the "+
			"value-taking flag. A grouping error has to point at the letter that "+
			"caused it, or the user tries a different combination at random", err)
	}
}

// TestAnUnknownLetterInsideAGroupIsRefused: groups are expanded letter by
// letter, so one bad letter makes the whole group meaningless. Expanding the
// rest and dropping the unknown one would enable half of what was asked for,
// which is indistinguishable from all of it having worked.
func TestAnUnknownLetterInsideAGroupIsRefused(t *testing.T) {
	_, err := exp(t, "run start", "-Sz")
	if err == nil {
		t.Fatal("-Sz was accepted although -z is not a flag. Expanding the S and " +
			"discarding the z would enable one option and silently ignore the " +
			"other, which the user cannot tell apart from both having worked")
	}
}

// TestEveryShortFlagReachesItsParameter is the obligation the design created,
// and the reason it is a test rather than a promise.
//
// Because a declared parameter is always reachable by name, expandShort turns -p
// into --prompt and hands it to a parser that may not accept --prompt. That
// failure is silent in the worst way: the flag parses, the value is dropped, and
// the command runs with a value the user believes they supplied. This walks
// every short flag of every implemented command and asserts the long form is
// understood.
func TestEveryShortFlagReachesItsParameter(t *testing.T) {
	// Only the implemented commands have parsers to check. The rest answer
	// "declared but not implemented" before any flag is read.
	// The probe has to give a value to a value-taking flag and NOT to a boolean.
	// Getting that backwards is a bug in the test rather than the code, and it
	// produced a confusing failure once already: passing a value after --json
	// left the value as a stray argument, and the parser's complaint about the
	// stray was reported as the flag being unknown.
	argsFor := func(c *surface.Cmd, long string) []string {
		if isBool(c, long) {
			return []string{"--" + long}
		}
		return []string{"--" + long, "x"}
	}

	cases := []struct {
		path string
		// probe returns an error only if the long flag was not recognised.
		// Anything else the parser dislikes about the probe arguments is not
		// what this test is about.
		probe func(c *surface.Cmd, long string) error
	}{
		{"run start", func(c *surface.Cmd, long string) error {
			args := append([]string{"bp", "prompt", "--budget", "1"}, argsFor(c, long)...)
			_, err := parseStartFlags(args)
			if err != nil && strings.Contains(err.Error(), "unknown flag") {
				return err
			}
			return nil
		}},
		{"serve", func(c *surface.Cmd, long string) error {
			args := argsFor(c, long)
			if long == "listen" {
				args = []string{"--listen", "unix:///tmp/x.sock"}
			}
			_, err := parseServeFlags(args)
			if err != nil && strings.Contains(err.Error(), "unknown") {
				return err
			}
			return nil
		}},
	}

	checked := 0
	for _, tc := range cases {
		c := surface.Lookup(strings.Split(tc.path, " ")...)
		if c == nil {
			t.Fatalf("%q vanished from the registry", tc.path)
		}
		for _, pp := range c.WireParams() {
			cliName := strings.ReplaceAll(pp.Name, "_", "-")
			letter := surface.Short(cliName)
			if letter == "" {
				continue
			}
			if err := tc.probe(c, cliName); err != nil {
				t.Fatalf("%s advertises -%s for --%s, but its parser does not "+
					"accept --%s: %v.\nexpandShort rewrites the letter into the "+
					"long name, so a parser that does not know the long name "+
					"drops the value silently and the command runs with input "+
					"the user believes they gave",
					tc.path, letter, cliName, cliName, err)
			}
			checked++
		}
	}

	if checked == 0 {
		t.Fatal("no short flags were checked, so this test proves nothing. " +
			"Either shortFlags was emptied or the probes stopped finding " +
			"parameters")
	}
}

// TestThePromptCannotBeGivenTwice. --prompt and a positional prompt are the same
// parameter by two routes, and keeping one silently runs an objective the user
// did not choose. Refusing names both, so the fix is obvious.
func TestThePromptCannotBeGivenTwice(t *testing.T) {
	_, err := parseStartFlags([]string{"bp", "positional words", "--prompt", "flag words", "--budget", "1"})
	if err == nil {
		t.Fatal("the prompt was accepted twice. One of the two was silently " +
			"discarded, so the run executes an objective the user did not choose " +
			"and nothing in the output says which one was used")
	}
	for _, want := range []string{"positional words", "flag words"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal was %q, which does not quote %q. Naming both "+
				"candidates is what lets the user see the collision; without them "+
				"the message describes a problem the user cannot locate",
				err, want)
		}
	}
}

// TestTheActorCanBeGivenByName is the rule the map settled: a declared parameter
// is always reachable by name, and the position is a convenience on top. If
// --actor stops working, -a expands into a flag the parser drops.
func TestTheActorCanBeGivenByName(t *testing.T) {
	f, err := parseStartFlags([]string{"--actor", "bp.yaml", "--prompt", "do it", "--budget", "1"})
	if err != nil {
		t.Fatalf("--actor was rejected: %v.\nThe registry declares the parameter "+
			"as `actor` and publishes that name to the tool schema and the "+
			"protocol, so refusing it on the CLI is the surface contradicting "+
			"itself -- and -a expands to exactly this flag", err)
	}
	if f.actor != "bp.yaml" {
		t.Fatalf("--actor bp.yaml produced actor %q. A flag that parses and does "+
			"not reach the field is worse than one that errors: the run starts "+
			"against a different blueprint", f.actor)
	}
	if f.prompt != "do it" {
		t.Fatalf("--prompt produced prompt %q, want %q", f.prompt, "do it")
	}
}

// TestExpandShortWithNoCommandChangesNothing. A nil command means the caller is
// dispatching something not in the registry, and inventing expansions for it
// would rewrite arguments on the way to a "does not exist" error.
func TestExpandShortWithNoCommandChangesNothing(t *testing.T) {
	in := []string{"-p", "x"}
	got, err := expandShort(nil, in)
	if err != nil {
		t.Fatalf("expandShort(nil) failed: %v. It has no basis to reject "+
			"anything, since without a command there is no parameter list to "+
			"check against", err)
	}
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("expandShort(nil) rewrote %q into %q. With no command it cannot "+
			"know what a letter means, and guessing produces flags the eventual "+
			"error message will quote back wrongly", in, got)
	}
}
