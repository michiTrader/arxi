package kernel

import (
	"encoding/json"
	"strings"
	"testing"
)

// ------------------------------------------------------------- shared state
//
// The KV store is how one member tells another something without paying for a
// turn to say it (docs/design/20-use-cases.md §20.8). These tests pin the
// decisions in the StateSet arm and the one in Clone, because each of them is
// invisible in the few lines of code that make it.

// TestStateSetIsReadableFromTheFold is the whole point of the event type: a key
// written into the log has to come back out of the fold, or `state get` is a
// command that reads nothing.
func TestStateSetIsReadableFromTheFold(t *testing.T) {
	c := bp()
	s := started(c)

	s, _ = Decide(s, ev(StateSet, "backend", map[string]any{
		"key": "api.contract", "value": "openapi at docs/api.yaml"}), c)
	if got := s.KV["api.contract"]; got != "openapi at docs/api.yaml" {
		t.Fatalf("state.set folded to %q, want the value it carried.\n"+
			"  consequence: `arxi state get api.contract` answers nothing for a key "+
			"an agent set, so two members coordinating through it each believe the "+
			"other never wrote.\n"+
			"  fix: the StateSet arm in decide.go", got)
	}

	// The second write to a key wins and nothing keeps the first. The history of a
	// key is the log's job (`event log --type state.set`), not the state's.
	s, _ = Decide(s, ev(StateSet, "designer", map[string]any{
		"key": "api.contract", "value": "openapi at docs/api-v2.yaml"}), c)
	if got := s.KV["api.contract"]; got != "openapi at docs/api-v2.yaml" {
		t.Errorf("the second write to a key folded to %q, want the newer value.\n"+
			"  consequence: a key that cannot be corrected is a key whose first "+
			"value is permanent, and whoever reads it acts on a contract that was "+
			"superseded hours ago.", got)
	}
	if len(s.KV) != 1 {
		t.Errorf("two writes to one key left %d entries: %v\n"+
			"  consequence: `state get` returns one of them and which one is an "+
			"accident of map iteration.", len(s.KV), s.KV)
	}
}

// TestStateSetDoesNotWriteThroughToTheStateItWasHanded is the test for the KV arm
// in Clone, and it is the one whose absence nothing else would notice.
//
// `out := s` copies a slice HEADER but a map REFERENCE, so without that arm
// writing out.KV[k] writes s.KV[k] as well and Decide mutates its own input.
// Walking forwards the fold still gives the right answer, which is exactly why
// this is worth a test: the damage only shows up where an earlier state is kept
// around -- a replay, a `--at-seq`, a golden -- and there it reads a value from
// the future with no error anywhere.
func TestStateSetDoesNotWriteThroughToTheStateItWasHanded(t *testing.T) {
	c := bp()
	s := started(c)
	s, _ = Decide(s, ev(StateSet, "backend", map[string]any{
		"key": "phase", "value": "design"}), c)

	before := s
	_, _ = Decide(s, ev(StateSet, "backend", map[string]any{
		"key": "phase", "value": "build"}), c)

	if got := before.KV["phase"]; got != "design" {
		t.Fatalf("the state handed to Decide now reads %q for a key the NEXT event "+
			"changed.\n"+
			"  consequence: the fold stops being a function of the events before it, "+
			"so replaying to seq N produces whatever seq N+1 did.\n"+
			"  fix: the KV arm in State.Clone", got)
	}
}

// TestStateSetWithNoKeyStoresNothing pins the drop. The CLI refuses an empty key
// up front, so this only fires on a hand-written log or a bridge that lost the
// field.
func TestStateSetWithNoKeyStoresNothing(t *testing.T) {
	c := bp()
	s := started(c)
	s, _ = Decide(s, ev(StateSet, "backend", map[string]any{"value": "orphan"}), c)

	if len(s.KV) != 0 {
		t.Fatalf("a state.set carrying no key stored %v\n"+
			"  consequence: the value sits in the state under a name `state get` has "+
			"no way to ask for, and the snapshot grows an entry whose key is the "+
			"empty string.", s.KV)
	}
}

// TestAKeyLandingWakesAStateWatcher is the coordination §20.8 is actually about:
// a member waiting on a contract gets a turn when it appears, and nobody had to
// spend one telling it.
func TestAKeyLandingWakesAStateWatcher(t *testing.T) {
	c := bp()
	c.Watchers = []Watcher{{Agent: "mediator", Pattern: "state.*", Action: "notify"}}
	s := started(c)

	_, fx := Decide(s, ev(StateSet, "backend", map[string]any{
		"key": "api.contract", "value": "frozen"}), c)
	if countEffects[SpawnTurn](fx) != 1 {
		t.Fatalf("a state.* watcher was not woken by state.set: %#v\n"+
			"  consequence: a blueprint can declare a reaction to a key landing and "+
			"get silence forever, so the member waiting on the contract has to be "+
			"prodded by a human -- the cost the KV store exists to avoid.\n"+
			"  check: StateSet must stay out of isWatcherDispatched.", fx)
	}
}

// TestTheKVKeepsItsNameInTheSnapshot pins the wire name, which is not cosmetic.
// The snapshot at runs/<id>/state.json is written by one command and read by the
// next, so a field that serializes under another name is a store that empties
// itself between two invocations while every fold in the same process looks fine.
func TestTheKVKeepsItsNameInTheSnapshot(t *testing.T) {
	raw, err := json.Marshal(State{KV: map[string]string{"api.contract": "frozen"}})
	if err != nil {
		t.Fatalf("a state holding a KV does not marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"kv":{"api.contract":"frozen"}`) {
		t.Errorf("the store is not in the snapshot as `kv`: %s\n"+
			"  consequence: each command reads the store back empty, so two agents "+
			"coordinating through a key see their own write and nothing else.", raw)
	}

	var back State
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("the snapshot does not read back: %v", err)
	}
	if back.KV["api.contract"] != "frozen" {
		t.Errorf("the value did not survive the round trip: %v\n"+
			"  consequence: the snapshot is a cache that disagrees with the log, and "+
			"the log is the one that is right.", back.KV)
	}
}
