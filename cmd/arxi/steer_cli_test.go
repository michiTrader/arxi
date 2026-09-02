package main

import (
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/surface"
)

// Tests for `arxi run steer`, run as the binary.
//
// # What this file does NOT re-pin, and why that is deliberate
//
// `run steer` and `run prompt` are one implementation: injectCause. The ten
// guards in prompt_cli_test.go -- empty message, malformed --if-seq, terminal
// run, paused run, unknown recipient, the CAS in both directions, the writer
// lock a refused CAS used to leave behind -- exercise that shared body, and
// copying them here would test the same lines twice while doubling the cost of
// changing them.
//
// So this file pins the two things prompt's tests cannot:
//
//   - what is steer's ALONE: the event type it records, and that the log can
//     tell a correction from a new requirement afterwards. That distinction is
//     the only reason two verbs exist, and no reducer test can see it because
//     the reducer treats both identically on purpose.
//   - that the sharing is real. TestBothInjectionVerbsRefuseAnUnknownRecipient-
//     InTheSameWords fails the moment somebody re-duplicates the body and edits
//     one copy, which is the failure the extraction exists to prevent.
//
// # The defect that made this file necessary before it was useful
//
// The surface declared `--on-busy` with a default of `steer`, and
// parseInvocation applies declared defaults. Since injectCause refuses any
// --on-busy the reducer does not implement, `arxi run steer <run> "..."` would
// have exited 2 -- refusing itself, over a flag nobody typed, quoting the
// surface's own default back at the caller. TestNoInjectionVerbIsRefusedByIts-
// OwnDeclaredDefault is what would have caught that, and it is written over
// both verbs so a third injection verb inherits it.

// TestASteerIsActedOnAndNotJustRecorded is the same claim prompt's first draft
// failed, asserted for the verb that was wired second.
//
// It is not redundant with prompt's version even though the code is shared: what
// it proves for steer is that the event type actually reaches applyInjection.
// The reducer folds run.prompt, agent.steered and agent.notified in one case
// arm, so a verb that appended, say, agent.steered with the wrong scope would
// still print success and change nothing -- the exact shape of the defect that
// cost `run prompt` a draft.
func TestASteerIsActedOnAndNotJustRecorded(t *testing.T) {
	dir := workdir(t)
	// Quiescent, not merely stopped: a halted run PARKS the cause, which is
	// correct behaviour and would hide the defect. The bug lives where the run is
	// free to work and nobody makes it.
	run := quiescentRun(t, dir)

	before := countEvents(t, dir, run, "agent.turn_done")

	got := arxi(t, dir, "run", "steer", run, "rate-limit by API key, not by IP")
	if got.code != 0 {
		t.Fatalf("steering exited %d:\n%s", got.code, got.out)
	}

	ev := eventOfType(t, dir, run, "agent.steered")
	if ev["source"] != "human" {
		t.Errorf("agent.steered is attributed to %v, want human -- a correction "+
			"injected from outside has no antecedent in the log, so an audit that "+
			"cannot say a human ordered it cannot explain why the run changed "+
			"course", ev["source"])
	}
	if ts, _ := ev["ts"].(string); ts == "" {
		t.Errorf("agent.steered has no timestamp: nothing else will stamp it, "+
			"because a human typed this command and it does not pass through the "+
			"effect runner:\n%v", ev)
	}

	// The banner names the verb the user typed. Cheap, and it catches the copy of
	// injection{} that forgets to change `past` -- which would report a prompt
	// for a steer and send a reader to the wrong half of the log.
	if !strings.Contains(got.out, "steered") {
		t.Errorf("the banner does not say the run was steered:\n%s\n\n"+
			"consequence: the output names a different verb than the one recorded, "+
			"so `event trace` and the terminal disagree about what happened.",
			got.out)
	}

	// And the run MOVED. Without this the command is a no-op that reports success.
	if after := countEvents(t, dir, run, "agent.turn_done"); after <= before {
		t.Errorf("the steer opened no turn: %d turns before, %d after.\n\n"+
			"The cause reached the log and was discarded -- spawnCauses returns a "+
			"transient effect for an idle member on a running run, so a correction "+
			"that is not driven changes nothing at all.", before, after)
	}
}

// TestTheLogTellsASteerFromAPrompt is the only justification the second verb has.
//
// The reducer cannot tell them apart, on purpose (ADR-0005: one injection
// mechanism). So if the LOG could not either, `run steer` would be an alias and
// this whole file would be arguing for a synonym. It records agent.steered where
// prompt records run.prompt, and that is what lets `event trace` answer, weeks
// later, whether a turn was bought because the plan was incomplete or because
// the team was going the wrong way. A log cannot be re-derived, so a distinction
// not recorded at the time is gone.
func TestTheLogTellsASteerFromAPrompt(t *testing.T) {
	dir := workdir(t)
	run := quiescentRun(t, dir)

	if got := arxi(t, dir, "run", "steer", run, "not by IP, by API key"); got.code != 0 {
		t.Fatalf("steering exited %d:\n%s", got.code, got.out)
	}
	if got := arxi(t, dir, "run", "prompt", run, "also add a metrics endpoint"); got.code != 0 {
		t.Fatalf("prompting exited %d:\n%s", got.code, got.out)
	}

	steers := countEvents(t, dir, run, "agent.steered")
	prompts := countEvents(t, dir, run, "run.prompt")
	if steers != 1 || prompts != 1 {
		t.Errorf("the log holds %d agent.steered and %d run.prompt, want one of "+
			"each.\n\n"+
			"consequence: the two verbs share an executor by design, and the event "+
			"type is the ONLY place their difference survives. If one of them "+
			"records the other's type, `event trace` can no longer separate a "+
			"change of direction from a new requirement, and the answer cannot be "+
			"reconstructed afterwards.\n"+
			"fix: check the typ field of the injection literal in steer.go and "+
			"prompt.go.", steers, prompts)
	}
}

// TestNoInjectionVerbIsRefusedByItsOwnDeclaredDefault is the guard for the
// defect wiring this verb exposed, and it is written over BOTH verbs so the
// third injection verb inherits it for free.
//
// The mechanism, because it is not obvious from either file alone: the surface
// declares --on-busy with a default, parseInvocation fills declared defaults in
// before it checks anything, and injectCause refuses any --on-busy the reducer
// does not implement. Those three are individually correct. Together, a default
// naming an unimplemented mode makes the verb refuse every plain invocation --
// exit 2, quoting a flag back at a caller who typed no flags at all.
//
// Two assertions, because either alone can be satisfied by the wrong fix. The
// invocation proves today's binary works; the declared default proves it works
// for the reason claimed, and not because somebody deleted the refusal.
func TestNoInjectionVerbIsRefusedByItsOwnDeclaredDefault(t *testing.T) {
	for _, verb := range []string{"prompt", "steer"} {
		dir := workdir(t)
		run := quiescentRun(t, dir)

		got := arxi(t, dir, "run", verb, run, "carry on")
		if got.code != 0 {
			t.Errorf("`arxi run %s <run> \"...\"` -- no flags -- exited %d:\n%s\n\n"+
				"consequence: the verb is unusable in its plainest form. If the "+
				"output mentions on-busy, the surface's declared default is a mode "+
				"the reducer does not implement, parseInvocation supplied it, and "+
				"the command refused itself over a flag nobody typed.\n"+
				"fix: the default in internal/surface/surface.go must be queue, "+
				"which is the only mode applyInjection has.", verb, got.code, got.out)
			continue
		}
		if strings.Contains(got.out, "on-busy") {
			t.Errorf("`arxi run %s` succeeded but still talks about on-busy:\n%s\n\n"+
				"consequence: a flag the caller never typed is being reported on, "+
				"which means a default is being applied and remarked upon.", verb, got.out)
		}

		c := surface.Lookup("run", verb)
		if c == nil {
			t.Fatalf("run %s is not in the surface registry, so this verb is wired "+
				"to a capability that is not declared", verb)
		}
		for _, pp := range c.Params {
			if pp.Name != "on-busy" {
				continue
			}
			if pp.Default != "queue" {
				t.Errorf("run %s declares --on-busy default %q, want \"queue\"\n\n"+
					"consequence: queue is the only mode applyInjection implements, "+
					"so any other default is a value the command must refuse -- and "+
					"parseInvocation applies defaults before validation, so it "+
					"refuses itself. `steer` in particular names the alternative "+
					"ADR-0005 discarded.", verb, pp.Default)
			}
		}
	}
}

// TestBothInjectionVerbsRefuseAnUnknownRecipientInTheSameWords is the test that
// makes the extraction real rather than aspirational.
//
// prompt.go and steer.go are forty lines each because injectCause holds the
// body. Nothing in the compiler stops a later change from inlining one of them
// "just to add a small thing", and the copy would keep passing every test in
// this directory -- prompt's guards would test prompt's copy, steer's would test
// steer's, and the four defects prompt paid for would quietly stop being pinned
// on the path steer takes.
//
// So this compares the two verbs' OUTPUT, on the one refusal with enough
// substance to be worth comparing: an unknown --to prints the members that do
// exist, which is a sentence a divergent copy is unlikely to reproduce
// character-for-character.
//
// Two things are normalised out first, and only those two. The invocation
// prefix, because `arxi run prompt:` naming the verb the user typed is the
// difference that is SUPPOSED to be there. And the run id, because each verb is
// given its own fixture.
//
// Deliberately NOT normalised: the bare word. Replacing every occurrence of
// "steer" was the first draft and it invented a difference -- this refusal ends
// "omit --to to use the blueprint's steer target", where steer is the name of a
// blueprint FIELD and identical in both outputs. A normaliser that rewrites
// shared text on one side only reports drift where there is none.
func TestBothInjectionVerbsRefuseAnUnknownRecipientInTheSameWords(t *testing.T) {
	say := map[string]string{}

	for _, verb := range []string{"prompt", "steer"} {
		dir := workdir(t)
		run := promptableRun(t, dir)

		got := arxi(t, dir, "run", verb, run, "hi", "--to", "nobody")
		if got.code != 1 {
			t.Fatalf("`run %s --to nobody` exited %d, want 1:\n%s", verb, got.code, got.out)
		}
		out := strings.ReplaceAll(got.out, "arxi run "+verb+":", "arxi run <verb>:")
		say[verb] = strings.ReplaceAll(out, run, "<run>")
	}

	if say["prompt"] != say["steer"] {
		t.Errorf("the two injection verbs refuse an unknown recipient differently.\n"+
			"prompt:\n%s\nsteer:\n%s\n\n"+
			"consequence: they are supposed to be one implementation (injectCause), "+
			"differing only in the event type they record. Divergent wording here "+
			"means the body was copied, and a copy stops inheriting the four "+
			"defects prompt_cli_test.go pins -- the discarded cause, the advice to "+
			"run a command that refuses, the wrong outlook on a halted run, and the "+
			"writer.lock a refused CAS left behind.\n"+
			"fix: both verbs call injectCause; neither should have a body of its own.",
			say["prompt"], say["steer"])
	}
}

// TestSteeringIntoABusyReducerSaysADR0005DiscardedIt.
//
// --on-busy steer is the one refusal in this binary that is not a gap waiting to
// be filled. The other unimplemented modes are work not done; this one is work
// decided against. Interrupting a running turn and restarting it with the new
// context is the alternative ADR-0005 rejected, because it throws away tokens
// already paid for -- so a reader who is told only "not implemented" will
// reasonably go and implement it.
//
// The refusal therefore has to carry the reason, and the reason has to be
// findable, which is why the ADR number is asserted rather than a paraphrase of
// it.
func TestSteeringIntoABusyReducerSaysADR0005DiscardedIt(t *testing.T) {
	dir := workdir(t)
	run := promptableRun(t, dir)

	for _, mode := range []string{"reject", "steer"} {
		got := arxi(t, dir, "run", "steer", run, "hi", "--on-busy", mode)
		if got.code != 2 {
			t.Errorf("--on-busy %s exited %d, want 2 (misuse):\n%s\n\n"+
				"consequence: applyInjection queues whatever it is handed, so "+
				"accepting this reports that a mode was honoured when it was "+
				"ignored -- and the caller asked NOT to disturb a busy member.",
				mode, got.code, got.out)
			continue
		}
		if !strings.Contains(got.out, "not implemented") {
			t.Errorf("--on-busy %s is refused without saying why:\n%s", mode, got.out)
		}
	}

	got := arxi(t, dir, "run", "steer", run, "hi", "--on-busy", "steer")
	if !strings.Contains(got.out, "ADR-0005") {
		t.Errorf("refusing --on-busy steer does not say the decision was made:\n%s\n\n"+
			"consequence: read as an unfinished feature, this is an invitation to "+
			"build the design's discarded alternative. The refusal is the only "+
			"place a caller meets it, so it is where the reason has to be.", got.out)
	}

	// And the implemented mode is accepted, so the refusals above are about
	// behaviour rather than about the flag existing at all.
	if ok := arxi(t, dir, "run", "steer", run, "hi", "--on-busy", "queue"); ok.code != 0 {
		t.Errorf("--on-busy queue was refused, but queueing is what the reducer "+
			"does: exit %d\n%s", ok.code, ok.out)
	}
}
