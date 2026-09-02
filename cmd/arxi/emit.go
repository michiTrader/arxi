package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/logstore"
	"github.com/michiTrader/arxi/internal/surface"
)

// `arxi event emit <run> <type> [--payload JSON]` -- the only way in from outside
// to wake a watcher a blueprint declared.
//
// # Why this verb, and why now
//
// `event log` was wired in the previous step and made the source of truth
// readable. It answers "what happened". This is the other half: the one verb that
// can make something happen that no other command can cause.
//
// Every other writer in this binary appends an event the reducer has a case for.
// `run prompt` appends run.prompt and applyInjection acts on it; `run pause`
// appends run.paused and the reducer sets a status. This one appends a type the
// reducer has NO case for -- and that is the point, not a limitation. A custom.*
// event falls through Decide's switch untouched and reaches the tail:
//
//	if !isWatcherDispatched(e.Type) && e.Source != SourceRuntime {
//	    fx = append(fx, wakeWatchers(&out, e, c)...)
//	}
//
// So a blueprint that declares `watchers: [{agent: qa, pattern: custom.*}]` has,
// until this command exists, declared a reaction to something nothing in the
// system can produce. The watcher machinery was reachable only from events the
// runtime happened to emit; the whole `custom.` namespace was a hole in the
// middle of it. This is the verb that fills the hole.
//
// # The declaration was written for the agent and did not survive the CLI
//
// The registry entry declared two parameters -- type and payload -- and no run.
// That reads correctly from inside a turn, where the run is ambient. From a shell
// it is unusable: there is no run to write to, resolveRunDir has no "the latest
// one" default, and no agent-side tool bridge exists yet (nothing outside
// internal/trigger/action.go so much as names this capability). The parameter was
// added to the surface rather than worked around here, because a command that
// took the run from an environment variable would be a second way to name a run.
// See the note on the entry in internal/surface/surface.go.
//
// # custom.* is enforced against everybody, not only agents
//
// spec/events.md says the namespace is reserved for agents and that agents may
// emit nothing else: "if they could emit stage.advanced, they could skip the
// advance rule of their own blueprint." That sentence was, before this file, a
// promise kept by nothing -- there was no emitter at all, so there was nothing to
// check it in.
//
// The obvious reading is that the restriction is an agent-side sandbox and a
// human at a terminal should be trusted with the other 32 types. That reading was
// rejected, and the reason is not trust:
//
//   - Each of the 32 catalogued types has a verb that maintains its invariants.
//     stage.advanced comes with a from/to/to_index the reducer indexes against
//     the blueprint's stage list; agent.blocked must bring blocked_ref or `run
//     why` reports a schema violation; run.result is what `run result` reads. A
//     hand-typed stage.advanced does not skip a safety check, it produces a log
//     that the fold and every reader afterwards disagree about.
//   - custom.* is the one namespace with NO invariant, which is exactly why an
//     outsider may write in it. Nothing downstream indexes it, so nothing
//     downstream can be corrupted by it.
//
// A human who genuinely needs to hand-write a catalogued event still can: the log
// is a file. What they cannot do is have this binary do it for them and then read
// the result back with commands that assume the invariants hold.
//
// # It drives the run, for the reason `run prompt` had to learn twice
//
// Appending and returning is not an option, and this is the second command in a
// row where that was the tempting mistake. wakeWatchers' effects -- CallTool, or
// SpawnTurn via spawnCauses -- live only in Decide's return value. Nobody
// executing them means they are discarded, so `event emit` would append an event,
// print success, and leave a matching watcher unfired. Indistinguishable, from
// the outside, from a watcher whose pattern was wrong.
//
// So: when a declared watcher matches AND the reducer would really act on it, the
// run is driven here. When none matches, nothing is driven and the command SAYS
// SO -- a recorded event nobody observes is a legitimate outcome (spec/events.md
// makes the same point about resource.conflict), but it is not the outcome a user
// typing this expects, and the difference has to be on screen.
const eventEmitUsage = "usage: arxi event emit <run> <type> [--payload JSON]\n" +
	"  <type>     must be in the custom.* namespace -- custom.contract_frozen\n" +
	"  --payload  a JSON object: --payload '{\"v\":2}'\n" +
	"  short: -r run · -T type\n" +
	"  see what exists:   arxi run list\n" +
	"  see what it wrote: arxi event log <run> --type 'custom.*'\n"

func cmdEventEmit(args []string) {
	c := surface.Lookup("event", "emit")
	vals, err := parseInvocation(c, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi event emit: %v\n\n%s", err, eventEmitUsage)
		os.Exit(2)
	}

	runArg := strings.TrimSpace(vals["run"])
	if runArg == "" {
		// `arxi event emit "$RUN" custom.x` with RUN unset satisfies
		// parseInvocation. Without this the run directory would resolve to the
		// runs directory itself and the error would be about a missing log.
		fmt.Fprint(os.Stderr, "arxi event emit: which run?\n\n"+eventEmitUsage)
		os.Exit(2)
	}

	typ := strings.TrimSpace(vals["type"])
	if err := checkCustomType(typ); err != nil {
		fmt.Fprintf(os.Stderr, "arxi event emit: %v\n\n%s", err, eventEmitUsage)
		os.Exit(2)
	}

	// The payload is decoded BEFORE the run is opened, so a malformed JSON string
	// is reported without having touched the log -- the same ordering `run
	// prompt` uses for --if-seq and `run unpause` for --budget, and for the same
	// reason: an append-only log offers no way to refuse a write after making it.
	payload, err := parseEmitPayload(vals["payload"])
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi event emit: %v\n\n%s", err, eventEmitUsage)
		os.Exit(2)
	}

	dir := resolveRunDir(runArg)

	// Folded before the append, because every refusal below is about what the run
	// WAS, and afterwards the event is in the log for good.
	pre, cfg, simulated, err := foldRunDir(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi event emit: %v\n", err)
		os.Exit(1)
	}

	// A terminal run is refused. This is not a formality: Decide's first line is
	// `if s.Status.Terminal() { return out, nil }`, so the append would succeed,
	// the event would be in the log, and the reducer would ignore it completely.
	// A watcher on custom.* would not fire and this command would have printed
	// success -- the exact silent no-op the guard exists to turn into a sentence.
	if pre.Status.Terminal() {
		fmt.Fprintf(os.Stderr, "arxi event emit: run %s is %s, which is final.\n"+
			"  the reducer ignores every event after a terminal status, so the "+
			"emit would be recorded and observed by nobody.\n"+
			"  to carry on from here: arxi run fork %s --at-seq %d\n",
			pre.RunID, pre.Status, pre.RunID, pre.Seq)
		os.Exit(1)
	}

	watch := emitWatcherOutcomes(pre, cfg, typ)
	acts := false
	for _, o := range watch {
		if o.acts {
			acts = true
		}
	}

	// A halted run is refused ONLY when something would have acted, and that
	// asymmetry is the whole judgement in this guard.
	//
	// `run prompt` refuses paused unconditionally, because a prompt has exactly
	// one purpose and spendingHalted parks it. An emit has two: it can be a cause,
	// and it can be a record. If no watcher matches the type, appending to a
	// paused run is a perfectly good thing to do -- the event is a marked point in
	// the history, and nothing was withheld. Refusing there would block a harmless
	// write and teach the user to unpause a run to write a note in it.
	//
	// When a watcher DOES match, the cause is parked by spawnCauses and handed
	// back by drainParked on resume. Nothing is lost, but nothing happens either,
	// and a user who emitted an event to make something happen would see success
	// and silence.
	if acts && spendingHalted(pre) {
		remedy := "arxi run unpause " + pre.RunID
		if pre.Status == kernel.StatusBlocked {
			// Blocked has several causes and each has its own remedy, which is
			// what `run why` is for. Naming unpause here would be a guess, and
			// the wrong guess prints a command that refuses.
			remedy = "arxi run why " + pre.RunID
		}
		fmt.Fprintf(os.Stderr, "arxi event emit: run %s is %s, and %d watcher(s) "+
			"match %s -- so the cause would be parked rather than acted on.\n"+
			"  it is not lost when that clears, but nothing runs meanwhile, which "+
			"looks exactly like this command failing.\n"+
			"  clear it first: %s\n", pre.RunID, pre.Status, len(watch), typ, remedy)
		os.Exit(1)
	}

	store, err := logstore.Open(dir)
	if err != nil {
		fatal(err)
	}
	// Both, and not redundant: the defer covers ordinary returns and atExit covers
	// os.Exit, which runs no defers. Measured on `run unpause`, where the missing
	// half left writer.lock holding a dead pid and bricked the run.
	defer store.Close()
	atExit(func() { store.Close() })

	ev := kernel.Event{
		// The seq is assigned by the writer, so the id is built off the head --
		// the same scheme as "unpause-<n>" and "prompt-<n>".
		ID: "emit-" + strconv.FormatInt(store.Head()+1, 10),
		// The one place in this binary that constructs an EventType from a string
		// rather than from a constant. That is what `custom.*` means, and it is
		// why checkCustomType above is a hard gate and not advice.
		Type: kernel.EventType(typ),
		// SourceHuman, and load-bearing twice over. wakeWatchers is skipped
		// outright for SourceRuntime -- stamping this runtime would make the
		// command append an event that provably wakes nobody -- and an audit that
		// cannot say a person caused this cannot explain why the run turned.
		Source: kernel.SourceHuman,
		Scope:  "run:" + pre.RunID,
		// Actor is left EMPTY, deliberately. wakeWatchers skips a watcher whose
		// agent equals the actor unless include_self, so claiming an actor here
		// would silently disable that agent's own watcher. Nobody in the run
		// emitted this; a shell did.
		Ts:      nowFunc().UTC().Format(time.RFC3339),
		Payload: payload,
	}

	// No CAS. --if-seq is not declared on this capability, and inventing it here
	// would be a flag that exists in the CLI and not in the surface -- a second
	// surface, which is the one thing internal/surface exists to prevent. Worth
	// revisiting in the registry: an emit is as racy as a prompt.
	written, err := store.Append([]kernel.Event{ev})
	if err != nil {
		fatal(fmt.Errorf("record %s: %w", typ, err))
	}
	at := written[0].Seq

	fmt.Printf("run %s emitted %s (seq %d)\n", pre.RunID, typ, at)
	printEmitOutlook(pre, cfg, typ, watch, acts)

	if !acts {
		// Nothing to drive, and nothing to charge for. Returning here is not a
		// weaker path: driving a run no watcher woke would run the loop from the
		// tip, find no pending work, and print a summary that suggests the emit
		// did something.
		return
	}
	if simulated {
		fmt.Printf("  this run was started with --sim, so the turn is taken by the " +
			"same fake executor: no model is called and no money is spent.\n")
	}
	driveResumedRun(dir, cfg, store, pre.RunID, simulated)
}

// checkCustomType is the namespace gate, and the only validation in this binary
// that refuses a value the reducer would have accepted.
//
// The messages name the consequence rather than the rule, because "must start
// with custom." tells a user what to type and not why, and the why is the thing
// that stops them looking for a flag to turn it off.
func checkCustomType(t string) error {
	if t == "" {
		return fmt.Errorf("which type? an event with no type is a row the log " +
			"cannot be read by: `event log --type` and every watcher pattern " +
			"select on it")
	}
	// A wildcard is refused before the prefix check, because `custom.*` is the
	// most likely thing to be typed here by somebody who has just used
	// `event log --type 'custom.*'`, and it passes the prefix check.
	if strings.Contains(t, "*") {
		return fmt.Errorf("%q is a pattern, not a type; `*` selects events in "+
			"`event log --type` and in a watcher, so emitting it would create one "+
			"event whose literal type is %q and which no readable pattern matches", t, t)
	}
	if strings.ContainsAny(t, " \t\n") {
		return fmt.Errorf("%q has whitespace in it; a type is one word with dotted "+
			"segments, and a space almost always means the shell split an argument "+
			"that needed quoting", t)
	}
	if !strings.HasPrefix(t, "custom.") || t == "custom." {
		return fmt.Errorf("%q is not in the custom.* namespace.\n"+
			"  custom.* is the only namespace with no invariant, which is why it is "+
			"the one an outsider may write in: the other types carry fields the "+
			"reducer indexes (stage.advanced's to_index, agent.blocked's "+
			"blocked_ref), and a hand-written one produces a log its own readers "+
			"disagree about.\n"+
			"  see spec/events.md; try custom.%s", t, strings.TrimPrefix(t, "custom."))
	}
	for _, seg := range strings.Split(t, ".") {
		if seg == "" {
			return fmt.Errorf("%q has an empty segment; a watcher pattern matches "+
				"whole segments, so an empty one can never be selected", t)
		}
	}
	return nil
}

// parseEmitPayload decodes --payload, and insists on an object.
//
// The parameter is declared as a "JSON payload" string -- confirmed against
// docs/design/20-use-cases.md, which shows the agent form as
// {"type": "custom.contract_frozen", "payload": "{\"v\":2}"}: a string that
// CONTAINS JSON, not a nested object.
//
// An object and not any JSON value, because Event.Payload is a map[string]any and
// nothing else will fit. `--payload 42` and `--payload '"done"'` are both valid
// JSON and both decode into nothing this log can hold, so they are refused here
// rather than dropped by the encoder later.
//
// Absent means no payload at all, and that is left as a nil map rather than an
// empty one: an event whose type is the whole message is a normal thing to emit,
// and `{}` in the log would read as a payload somebody forgot to fill in.
func parseEmitPayload(raw string) (map[string]any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		// The single-quote advice is here because it is the failure that actually
		// happens: an unquoted {"v":2} is mangled by the shell before this process
		// sees it, and the JSON error then complains about bytes the user never
		// typed.
		return nil, fmt.Errorf("--payload is not a JSON object: %v\n"+
			"  it is one argument holding JSON, so quote it: --payload '{\"v\":2}'\n"+
			"  a bare number or string cannot be a payload -- the log stores keyed "+
			"fields, and `event log` prints them as key=value", err)
	}
	if m == nil {
		// `--payload null` parses into a nil map with no error, which would append
		// an event that claims a payload and carries none.
		return nil, fmt.Errorf("--payload null carries nothing; omit the flag to " +
			"emit a type with no payload")
	}
	return m, nil
}

// emitWatcherOutcome is one declared watcher's answer to this event.
//
// Resolved once, before the append, and reused for the refusal above, the printed
// outlook and the decision to drive. Three places asking the same question
// separately is how a command comes to refuse a write for a watcher it then does
// not mention.
type emitWatcherOutcome struct {
	agent string
	// acts is "the reducer will return an effect for this watcher". It is NOT "the
	// watcher matched": a matching watcher whose agent is not in the run, or has
	// failed, or is mid-turn, produces either nothing or a parked cause.
	acts bool
	// tool names the tool a run_tool watcher will call, and is empty for every
	// other action. It exists so a caller can tell the ONE outcome that cannot
	// survive a halted run from the ones that can: a notify or activate cause is
	// parked in PendingCauses, which is part of State and therefore comes back on
	// unpause, while CallTool lives only in Decide's return value and is dropped by
	// the next fold. `state set` refuses on that difference (see state.go), and
	// deriving it there by re-matching the patterns would be a second copy of the
	// duplication this function exists to keep down to one.
	tool string
	note string
}

// emitWatcherOutcomes reproduces wakeWatchers' filters, in wakeWatchers' order.
//
// DUPLICATED from the reducer, knowingly, and this is the third command to make
// the same trade (see kernelSteerTargetFor and runshow.go's spendingHalted): the
// alternative is exporting the reducer's watcher evaluation so a printer may
// borrow it, which widens a frozen surface for a display concern. The order is
// copied exactly, because the order is what decides which sentence is true --
// checking the member before the depth limit would report "it opens a turn for
// qa" about a cascade the reducer refuses to extend.
//
// The risk is drift, and it is named rather than mitigated: the pinning test
// emits into a run with a matching watcher and checks that the printed line and
// what the log then contains agree.
func emitWatcherOutcomes(st kernel.State, cfg kernel.Config, typ string) []emitWatcherOutcome {
	var out []emitWatcherOutcome
	for _, w := range cfg.Watchers {
		if !kernel.MatchEventType(w.Pattern, typ) {
			continue
		}
		o := emitWatcherOutcome{agent: w.Agent}

		// The self-exclusion cannot fire from here -- the event carries no actor,
		// on purpose (see the Event above) -- so there is no branch for it. Said
		// here rather than left as an absence, because a reader comparing this
		// with wakeWatchers will look for it.

		switch {
		case cfg.MaxDepth <= 0:
			// Only reachable with no frozen blueprint, since ResolveDefaults sets
			// 12. Then Watchers is empty too, so this is belt-and-braces on a
			// hand-built Config rather than a case a real run reaches.
			o.note = "would be skipped: max_depth is 0, so no watcher can fire at all"
		case st.Member(w.Agent) == nil:
			o.note = "declared on " + w.Agent + ", who is not a member of this run, " +
				"so nothing will act on it"
		case st.Member(w.Agent).State == kernel.MemberFailed:
			o.note = w.Agent + " has failed, and a failed member takes no new causes"
		case w.Action == "run_tool":
			// run_tool does NOT consult the member's busy state: wakeWatchers
			// returns CallTool unconditionally, with the event's payload as the
			// arguments. Worth stating, because it is the one action that fires
			// while its agent is mid-turn.
			o.acts = true
			o.tool = w.Tool
			o.note = "runs " + w.Tool + " for " + w.Agent +
				" now, with this payload as its arguments"
		case st.Member(w.Agent).Busy():
			o.note = "queued: " + w.Agent + " is mid-turn, and the cause is drained " +
				"when that turn finishes"
		case st.Member(w.Agent).State == kernel.MemberWaiting:
			// Detail is what the member is blocked ON, and it is optional:
			// applyBlocked reads it from the `blocked_on` payload key, which a
			// caller may omit. Running this branch against a member blocked
			// without one printed "security is blocked ()" -- an empty
			// parenthetical that reads like a lost string, not like an absent
			// one. So the parenthetical only appears when it says something.
			o.note = "queued: " + w.Agent + " is blocked"
			if d := st.Member(w.Agent).Detail; d != "" {
				o.note += " on " + d
			}
			o.note += ", and the cause is drained once that clears"
		default:
			o.acts = true
			o.note = "opens a turn for " + w.Agent + " now"
		}
		out = append(out, o)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].agent < out[j].agent })
	return out
}

// printEmitOutlook says who heard it, and it is the whole reason this command is
// more than three lines.
//
// The no-watcher case gets the most words, and that is not padding. An emit that
// matches nothing succeeds: the event is in the log, `event log` will show it, and
// spec/events.md makes the same point about resource.conflict -- a recorded event
// nobody observes stays recorded. But it is almost never what somebody typing
// this wanted, and there is no error to tell them so. The one thing that
// distinguishes "nothing is watching custom.deploy" from "custom.deploy woke qa"
// is this paragraph, so it names the patterns the run DOES watch -- measured
// against this blueprint, the same choice distinctTypes makes for a mistyped
// --type: the useful answer is what exists here, not what could exist.
func printEmitOutlook(st kernel.State, cfg kernel.Config, typ string, watch []emitWatcherOutcome, acts bool) {
	if len(watch) == 0 {
		fmt.Printf("  recorded, and observed by nobody: no watcher in this run's "+
			"blueprint matches %s.\n", typ)
		if pats := declaredWatcherPatterns(cfg); len(pats) > 0 {
			fmt.Printf("  this run watches: %s\n", strings.Join(pats, ", "))
		} else {
			fmt.Printf("  this run declares no watchers at all, so no event of any " +
				"type would start work here.\n")
		}
		fmt.Printf("  the event is in the log either way: arxi event log %s "+
			"--type '%s'\n", st.RunID, typ)
		return
	}

	for _, o := range watch {
		fmt.Printf("  %s\n", o.note)
	}
	if !acts {
		// Every match was filtered out. Said explicitly, because the lines above
		// each explain one watcher and none of them says the sum: no turn opens
		// now, and the run is not driven below.
		fmt.Printf("  so nothing starts now -- the run is left where it is.\n")
	}
}

// declaredWatcherPatterns lists the patterns this run's frozen blueprint watches.
//
// It reads the same Config the outcomes above were computed from, and that is the
// point of passing it rather than re-reading the snapshot: a list of patterns from
// one blueprint beside conclusions from another is the kind of disagreement this
// paragraph exists to settle.
func declaredWatcherPatterns(cfg kernel.Config) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range cfg.Watchers {
		if w.Pattern == "" || seen[w.Pattern] {
			continue
		}
		seen[w.Pattern] = true
		out = append(out, w.Pattern)
	}
	sort.Strings(out)
	return out
}
