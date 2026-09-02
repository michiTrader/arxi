package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/surface"
)

// `arxi state get <run> <key>` -- the read half of the run's shared key/value
// store, and the other end of what `state set` wrote.
//
// # Reading the fold is the whole implementation
//
// There is no store to open and no index to consult: the value is State.KV[key],
// and State is fold(Decide, State0, events). So this command takes no writer lock
// -- a reader that locked the run would fail on a live one and say nothing at all
// about the key -- and refuses nothing about the run's status. `state set` has to
// refuse a terminal run because the reducer would drop the write; a READ of a
// cancelled run is the case somebody working out what happened needs most.
//
// # stdout is the value, alone
//
// A deliberate departure from `run result`, whose stdout is a report with the
// answer inside it and which says so at length. The normal destination of this
// value is another command's argument:
//
//	arxi state set r2 upstream.contract "$(arxi state get r1 api.contract)"
//
// so a headline above it would be captured too and quietly become part of the
// value. Everything ABOUT the value goes to stderr, or behind --json.
const stateGetUsage = "usage: arxi state get <run> <key> [--json]\n" +
	"  <key>      the name it was written under -- e.g. api.contract\n" +
	"  --json     the value, who set it, and the seq to guard the next write with\n" +
	"  short: -r run · -k key · -J json\n" +
	"  exit 3     the key is not set, which is not the same as an empty value\n" +
	"  write it:     arxi state set <run> <key> <value>\n" +
	"  its history:  arxi event log <run> --type state.set\n"

// exitStateKeyUnset is `run result`'s "not yet", deliberately shared.
//
// Aliased rather than written as 3 for the reason attach.go gives for the same
// alias: a caller that waits for a member to publish a key polls this command,
// and a caller that waits for the whole run polls `run result`. One number means
// "the answer is not there yet" on both, so the two cannot drift apart.
//
// 1 stays reserved for "the run could not be read", and that distinction is what
// makes such a loop safe: a script that treated every non-zero exit as "not yet"
// would spin forever on a run id with a typo in it.
const exitStateKeyUnset = exitResultNotYet

// stateGetKeysListed caps the key listing in the not-set message. A store with
// two hundred keys in it would otherwise bury the one line that says what went
// wrong under a listing nobody scrolls back through.
const stateGetKeysListed = 8

// stateSetRef is where a key's current value came from, for --json.
type stateSetRef struct {
	seq    int64
	source kernel.Source
	actor  string
	ts     string
}

func cmdStateGet(args []string) {
	c := surface.Lookup("state", "get")
	vals, err := parseInvocation(c, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi state get: %v\n\n%s", err, stateGetUsage)
		os.Exit(2)
	}

	runArg := strings.TrimSpace(vals["run"])
	if runArg == "" {
		fmt.Fprint(os.Stderr, "arxi state get: which run?\n\n"+stateGetUsage)
		os.Exit(2)
	}

	key := vals["key"]
	if err := checkStateGetKey(key); err != nil {
		fmt.Fprintf(os.Stderr, "arxi state get: %v\n\n%s", err, stateGetUsage)
		os.Exit(2)
	}

	dir := resolveRunDir(runArg)

	// foldRunDirEvents rather than foldRunDir, for `run result`'s reason: the
	// provenance --json reports lives only in the events, and reading the log a
	// second time to find it could answer with a different write than the value
	// printed beside it, on a run that is being appended to right now.
	st, _, _, events, err := foldRunDirEvents(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi state get: %v\n", err)
		os.Exit(1)
	}

	id := st.RunID
	if id == "" {
		id = filepath.Base(dir)
	}

	// Two results, not one, and every branch below turns on the second rather than
	// on the value: "" is a value, so a command that asked `if value == ""` would
	// report a key a member had deliberately emptied as a key nobody ever set.
	value, found := st.KV[key]

	if vals["json"] == "true" {
		emitJSON(stateGetPayload(id, key, value, found, st, lastStateSet(events, key)))
		// The negative answer is JSON on stdout too, and only the exit code carries
		// it. A machine reader asked for one document and gets one whether or not the
		// key is there; switching to prose on stderr for the "no" case is what makes a
		// --json caller parse stdout, find nothing, and report a broken command.
		if !found {
			os.Exit(exitStateKeyUnset)
		}
		return
	}

	if !found {
		printKeyUnset(id, key, st)
		os.Exit(exitStateKeyUnset)
	}

	// Println rather than Print: a value is a line. `$(...)` strips the trailing
	// newline for the caller who substitutes it, and the caller reading a pipe needs
	// the terminator to know where the value ended.
	fmt.Println(value)

	// An empty value is a value -- spec/events.md has no delete, so emptying a key is
	// how a member retracts one -- and in a terminal an empty line is indistinguishable
	// from this command having printed nothing at all. Hence the note, and hence its
	// going to stderr: what a redirect captured is still exactly the empty line.
	if value == "" {
		fmt.Fprintf(os.Stderr, "run %s has %s set to the empty string, which is a value "+
			"and not an absence: there is no delete, so emptying a key is how a member "+
			"retracts one. an unset key prints nothing and exits %d.\n",
			id, key, exitStateKeyUnset)
	}
}

// printKeyUnset explains an unset key, and lists what the store does hold.
//
// stdout stays EMPTY, per printRunResult: `arxi state get r1 k > v.txt` on an unset
// key has to leave v.txt empty rather than write an explanation into it, because
// the next command in the pipeline reads that file as the value.
//
// The keys are listed because the overwhelmingly likely cause is a name that does
// not match -- a typo, or a member writing under a different one -- and the store
// is the only place the real name exists. Sorted, for the reason describeBlockedOn
// sorts: output that reshuffles between two identical invocations looks broken even
// when it is right. Quoted, because a key differing by padding or by an invisible
// character is exactly the case a listing has to make visible.
func printKeyUnset(id, key string, st kernel.State) {
	fmt.Fprintf(os.Stderr, "run %s has no %s set (seq %d).\n", id, key, st.Seq)

	unset := fmt.Sprintf(
		"  exit %d, which is how a poller tells \"not set yet\" from \"no such run\".\n",
		exitStateKeyUnset)

	if len(st.KV) == 0 {
		fmt.Fprintf(os.Stderr, "  its store is empty: no state.set has been folded into "+
			"this run at all.\n  write it: arxi state set %s %s <value>\n%s",
			id, key, unset)
		return
	}

	keys := make([]string, 0, len(st.KV))
	for k := range st.KV {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	noun := "keys"
	if len(keys) == 1 {
		noun = "key"
	}
	fmt.Fprintf(os.Stderr, "  its store holds %d %s:\n", len(keys), noun)
	for i, k := range keys {
		if i == stateGetKeysListed {
			fmt.Fprintf(os.Stderr, "    ... and %d more, all of them in: arxi event log "+
				"%s --type %s\n", len(keys)-i, id, kernel.StateSet)
			break
		}
		fmt.Fprintf(os.Stderr, "    %s\n", strconv.Quote(k))
	}
	fmt.Fprintf(os.Stderr, "  the store is an exact lookup, so a name differing by one "+
		"character or by padding is a different key.\n%s", unset)
}

// lastStateSet finds the write the current value came from.
//
// Backwards, stopping at the first hit: the store keeps one value per key and the
// reducer overwrites, so the LAST state.set for a key is the one the fold left
// behind. Walking forwards and keeping the latest match would answer the same
// question after touching every event in the run.
//
// The payload is read the same way the reducer reads it, key first, so a write the
// reducer DROPPED -- one with an empty key, which the CLI refuses but an agent's
// tool call can still submit -- is never reported here as the source of a value it
// did not set.
func lastStateSet(events []kernel.Event, key string) *stateSetRef {
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.Type != kernel.StateSet || e.Str("key") != key {
			continue
		}
		return &stateSetRef{seq: e.Seq, source: e.Source, actor: e.Actor, ts: e.Ts}
	}
	return nil
}

// stateGetPayload is the machine answer, and it carries three things the plain
// output cannot.
//
// `found` is a field of its own rather than something to infer from `value`, for
// resultPayload's reason about has_result: "" is a value here, so a reader that
// tested the value for emptiness would report a retracted key as one that was
// never set -- the distinction the exit code exists to keep.
//
// `seq` is the RUN's, not the write's, and it is here to be handed straight back:
//
//	seq=$(arxi state get r1 phase -J | jq .seq)
//	arxi state set r1 phase build --if-seq $seq
//
// which is ADR-0006's compare-and-set loop with the read and the guard taking
// their number from one fold of one log.
//
// The provenance is prefixed set_ and omitted entirely, rather than zeroed, when
// the log holds no write for the key -- the ordinary case for a key that is not
// set. "No write in this log" and "written at seq 0" are different facts, and a
// reader handed the second one prints it as a location.
func stateGetPayload(id, key, value string, found bool, st kernel.State, ref *stateSetRef) map[string]any {
	out := map[string]any{
		"run":   id,
		"key":   key,
		"found": found,
		"value": value,
		"seq":   st.Seq,
	}
	if ref == nil {
		return out
	}
	out["set_at_seq"] = ref.seq
	out["set_source"] = string(ref.source)
	out["set_ts"] = ref.ts
	// Empty for every write `arxi state set` makes: that command leaves Actor unset
	// on purpose, so wakeWatchers does not read the key as self-set and skip the
	// agent's own watcher on state.*. Omitted rather than "" so a reader is not
	// invited to print a member name that does not exist.
	if ref.actor != "" {
		out["set_by"] = ref.actor
	}
	return out
}

// checkStateGetKey refuses the keys the store could not be holding.
//
// It DECIDES with checkStateKey, deliberately. That function refuses to CREATE an
// empty, padded or line-broken key, so a lookup for one can only ever answer "not
// set" -- and answering that with exit 3 would tell a polling caller to wait for a
// write no command in this binary will perform. One predicate, two wordings: if
// the write side ever accepts one of these, the read side follows without being
// edited, which is the direction that would otherwise rot.
//
// The wordings have to differ because the consequences are opposite. On the write
// the damage is a recorded event nothing can read; here nothing is damaged at all,
// and the whole of the message is which key to ask for instead.
func checkStateGetKey(k string) error {
	if checkStateKey(k) == nil {
		return nil
	}
	if strings.TrimSpace(k) == "" {
		return fmt.Errorf("which key? this reads one key by its exact name; every key "+
			"the run has ever set is in: arxi event log <run> --type %s", kernel.StateSet)
	}
	// %q on both, since the difference between the two is invisible unquoted, and a
	// message whose suggestion looks identical to what the user typed reads as a bug
	// in this command rather than as a fixable mistake.
	return fmt.Errorf("the key %q carries padding or a line break, and `arxi state set` "+
		"refuses to create a key like that -- so this lookup would answer \"not set\" "+
		"whatever the run holds. did you mean %s?",
		k, strconv.Quote(strings.TrimSpace(k)))
}
