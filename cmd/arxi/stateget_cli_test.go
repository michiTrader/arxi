package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `arxi state get`, exercised as a process against real run directories.
//
// # The contract is which stream, which exit code, and what "" means
//
// The command prints one string, which makes it look like the least testable half
// of the store. It is the opposite, for `run result`'s reason: this value's normal
// destination is another command's argument --
//
//	arxi state set r2 upstream.contract "$(arxi state get r1 api.contract)"
//
// -- so three things no screenshot shows are the whole of it. A headline on stdout
// silently becomes part of the value. An explanation on stdout for a key that is
// NOT set becomes the value of a key that has none. And an exit code that cannot
// tell "not set yet" from "no such run" turns a polling loop into either a spin on
// a typo or a wait for a write that already happened.
//
// # "" is a value, and every negative assertion here turns on that
//
// spec/events.md gives the store no delete, on the grounds that "a key that
// vanished from the fold could not be told from a key nobody ever set", so
// emptying a key is how a member retracts one. That makes `if value == ""` the
// single most likely defect in this command: it would report a key somebody
// deliberately emptied as a key that was never written, and the caller would go
// looking for the member who owes them a write that has already been made.
//
// # Fixtures are hand-written logs, one test excepted on purpose
//
// runAt (runlist_cli_test.go) takes extra log lines verbatim, so these reuse it.
// Building the fixture with `state set` would leave every failure ambiguous
// between the two halves, and two of the states below -- a key set by an agent, a
// key read out of a terminal run -- are ones `state set` will not produce at all,
// the second because it refuses a terminal run outright. The exception is the
// round trip at the bottom, which uses both commands deliberately and says why.

// stateSetLine is one state.set log line, with the payload encoded rather than
// pasted.
//
// A value carrying a quote or a newline is exactly what two of these tests are
// about, and a fixture built by concatenating strings would be the one place in
// the file where such a value is spelled differently from how the log holds it.
func stateSetLine(t *testing.T, seq int64, source, actor, key, value string) string {
	t.Helper()
	e := map[string]any{
		"id":      fmt.Sprintf("state-set-%d", seq),
		"seq":     seq,
		"type":    "state.set",
		"source":  source,
		"ts":      "2026-02-01T09:00:0" + fmt.Sprint(seq%10) + "Z",
		"payload": map[string]any{"key": key, "value": value},
	}
	if actor != "" {
		e["actor"] = actor
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("encoding a state.set fixture: %v", err)
	}
	return string(b) + "\n"
}

// kvRunAt is a running run whose store holds the given keys, written in order.
func kvRunAt(t *testing.T, dir, id string, kv ...[2]string) {
	t.Helper()
	var log strings.Builder
	for i, pair := range kv {
		log.WriteString(stateSetLine(t, int64(3+i), "human", "", pair[0], pair[1]))
	}
	runAt(t, dir, id, "feature-team", 1.0, log.String())
}

// TestStateGetPutsTheValueOnStdoutAndNothingElse is the pipe contract.
func TestStateGetPutsTheValueOnStdoutAndNothingElse(t *testing.T) {
	dir := t.TempDir()
	kvRunAt(t, dir, "r1", [2]string{"api.contract", "POST /v1/tokens"})

	out, errb, code := arxiStreams(t, dir, "state", "get", "r1", "api.contract")
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out, errb)
	}
	if out != "POST /v1/tokens\n" {
		t.Errorf("stdout is %q, want exactly %q\n"+
			"  consequence: this value is substituted into another command with "+
			"$(...), so anything else on stdout -- a label, the key, the run id -- "+
			"becomes part of the value the next member reads.",
			out, "POST /v1/tokens\n")
	}
	if errb != "" {
		t.Errorf("stderr for an ordinary hit is %q, want empty\n"+
			"  consequence: a note on every read is noise in a loop that polls this "+
			"command, and the reader stops looking at the stream that carries the "+
			"one message that matters.", errb)
	}
}

// TestStateGetPrintsAMultiLineValueVerbatim.
//
// `state set` restricts the KEY and not the value: checkStateKey refuses a newline
// in a name because every reader of the store prints one key per line, and says
// nothing about the content, because an API contract or a schema is the ordinary
// thing to park in this store and neither fits on one line.
//
// So the tempting defect is quoting -- printing the value through %q so the output
// is one tidy line. That would substitute `"POST /v1/tokens\nAuthorization: ..."`,
// backslashes and surrounding quotes included, into the next command's argument.
func TestStateGetPrintsAMultiLineValueVerbatim(t *testing.T) {
	dir := t.TempDir()
	const contract = "POST /v1/tokens\nAuthorization: Bearer <jwt>\n\"quoted\" and\ttabbed"
	kvRunAt(t, dir, "r1", [2]string{"api.contract", contract})

	out, _, code := arxiStreams(t, dir, "state", "get", "r1", "api.contract")
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout:\n%s", code, out)
	}
	if out != contract+"\n" {
		t.Errorf("stdout is %q,\nwant %q\n"+
			"  consequence: the value is bytes a member wrote, not a display form. "+
			"Quoting or escaping it here means the value the next member reads is "+
			"not the value this one set.", out, contract+"\n")
	}
}

// TestStateGetLeavesStdoutEmptyWhenTheKeyIsNotSet.
//
// printRunResult's rule, applied to the store: `arxi state get r1 k > v.txt` on an
// unset key has to leave v.txt EMPTY rather than write an explanation into it,
// because the next command in the pipeline reads that file as the value. A file
// saying "run r1 has no k set" is worse than an empty one -- it is a value.
func TestStateGetLeavesStdoutEmptyWhenTheKeyIsNotSet(t *testing.T) {
	dir := t.TempDir()
	kvRunAt(t, dir, "r1", [2]string{"api.contract", "frozen"})

	out, errb, code := arxiStreams(t, dir, "state", "get", "r1", "db.schema")
	if code != 3 {
		t.Fatalf("exit %d, want 3\nstdout:\n%s\nstderr:\n%s", code, out, errb)
	}
	if out != "" {
		t.Errorf("stdout for an unset key is %q, want empty\n"+
			"  consequence: redirected to a file, that text becomes the value of a "+
			"key that has none, and every member downstream treats the explanation "+
			"as the contract.", out)
	}
	if !strings.Contains(errb, "no db.schema set") {
		t.Errorf("stderr never says which key is unset:\n%s", errb)
	}
	if !strings.Contains(errb, "exit 3") {
		t.Errorf("stderr never names the exit code:\n%s\n"+
			"  consequence: the code is the whole answer for a caller that scripts "+
			"this, and it is the one part of the answer a person cannot see.", errb)
	}
}

// TestStateGetNamesTheKeysTheStoreDoesHold.
//
// The overwhelmingly likely cause of a miss is a name that does not match -- a
// typo, or a member writing under a different one -- and the store is the only
// place the real name exists. `run show` does not print KV, so a listing here is
// not a convenience: without it the user's next move is to guess again.
//
// Sorted, for describeBlockedOn's reason: output that reshuffles between two
// identical invocations looks broken even when it is right. Quoted, because a key
// differing by padding or by an invisible character is precisely the case a
// listing has to make visible.
func TestStateGetNamesTheKeysTheStoreDoesHold(t *testing.T) {
	dir := t.TempDir()
	kvRunAt(t, dir, "r1",
		[2]string{"phase", "build"},
		[2]string{"api.contract", "frozen"},
		[2]string{"owner ", "backend"}) // padded: written by a tool call, not by the CLI

	_, errb, code := arxiStreams(t, dir, "state", "get", "r1", "api.contarct")
	if code != 3 {
		t.Fatalf("exit %d, want 3\nstderr:\n%s", code, errb)
	}
	if !strings.Contains(errb, "holds 3 keys") {
		t.Errorf("stderr does not count the store:\n%s", errb)
	}

	api, phase := strings.Index(errb, `"api.contract"`), strings.Index(errb, `"phase"`)
	if api < 0 || phase < 0 {
		t.Fatalf("the keys are not listed quoted:\n%s\n"+
			"  consequence: `owner ` and `owner` print identically unquoted, so the "+
			"listing shows the user a name that looks like the one they typed.", errb)
	}
	if api > phase {
		t.Errorf("the keys are listed in log order, not sorted:\n%s\n"+
			"  consequence: two identical reads print two different listings, which "+
			"reads as the store changing under the user.", errb)
	}
	if !strings.Contains(errb, `"owner "`) {
		t.Errorf("a padded key is not shown as padded:\n%s", errb)
	}
}

// TestStateGetSaysTheStoreIsEmptyRatherThanListingNothing.
//
// A run with no state.set in it at all is the case where the key listing would be
// a header above a blank space. The two situations send the reader somewhere
// different: an empty store means nobody has written yet, so wait or go and ask;
// keys present means the name is wrong, so look at the list.
func TestStateGetSaysTheStoreIsEmptyRatherThanListingNothing(t *testing.T) {
	dir := t.TempDir()
	kvRunAt(t, dir, "r1")

	_, errb, code := arxiStreams(t, dir, "state", "get", "r1", "api.contract")
	if code != 3 {
		t.Fatalf("exit %d, want 3\nstderr:\n%s", code, errb)
	}
	if !strings.Contains(errb, "store is empty") {
		t.Errorf("an empty store is not reported as empty:\n%s", errb)
	}
	if !strings.Contains(errb, "arxi state set r1 api.contract") {
		t.Errorf("the message does not offer the write:\n%s\n"+
			"  consequence: on a run where nothing has been written, the useful next "+
			"command is the other half of the store, and this is the one case where "+
			"the user may well be the one who should write it.", errb)
	}
}

// TestStateGetTellsAnEmptyValueFromAnUnsetKey is the defect this command was
// written around.
//
// Both answers print nothing readable on a terminal, and they mean opposite
// things: one member retracted the contract, or no member has published it. The
// exit code is what separates them, and the note on stderr is what makes the
// difference visible to a person -- on stderr, so a redirect still captures
// exactly the empty line.
func TestStateGetTellsAnEmptyValueFromAnUnsetKey(t *testing.T) {
	dir := t.TempDir()
	kvRunAt(t, dir, "r1", [2]string{"api.contract", ""})

	out, errb, code := arxiStreams(t, dir, "state", "get", "r1", "api.contract")
	if code != 0 {
		t.Fatalf("an empty value exited %d, want 0\nstderr:\n%s\n"+
			"  consequence: spec/events.md has no delete, so \"\" is how a member "+
			"retracts a key. Exiting 3 for it tells a polling caller to wait for a "+
			"write that has already been made.", code, errb)
	}
	if out != "\n" {
		t.Errorf("stdout for an empty value is %q, want %q: the value is still a "+
			"line, and $(...) strips the terminator for the caller.", out, "\n")
	}
	if !strings.Contains(errb, "empty string, which is a value") {
		t.Errorf("nothing on stderr distinguishes an emptied key from an unset one:\n%s\n"+
			"  consequence: in a terminal the two are the same blank space, and the "+
			"reader concludes the command is broken.", errb)
	}
}

// TestStateGetKeepsNotSetApartFromNoSuchRun is what makes a polling loop safe.
//
//	until v=$(arxi state get r1 api.contract); do sleep 5; done
//
// is the loop this command exists for, and it is only correct because the two
// failures have different codes. 3 means the write has not happened yet, so wait;
// 1 means the run could not be read at all -- a typo in the id, a directory that
// was never there -- so waiting is waiting forever. A caller that treated every
// non-zero exit as "not yet" would spin until somebody noticed.
//
// 2 is the third, and it belongs to the invocation rather than to the run: a key
// `state set` refuses to create is a key no write will ever produce, so answering
// 3 for it would be the same forever-wait dressed as a legitimate not-yet.
func TestStateGetKeepsNotSetApartFromNoSuchRun(t *testing.T) {
	dir := t.TempDir()
	kvRunAt(t, dir, "r1", [2]string{"api.contract", "frozen"})

	for _, tc := range []struct {
		what string
		args []string
		want int
	}{
		{"a key nobody has written", []string{"state", "get", "r1", "db.schema"}, 3},
		{"a run that does not exist", []string{"state", "get", "r9", "db.schema"}, 1},
		{"a key with no name", []string{"state", "get", "r1", "-k", ""}, 2},
		{"a key the store could not hold", []string{"state", "get", "r1", " api.contract"}, 2},
		{"no key at all", []string{"state", "get", "r1"}, 2},
	} {
		out, errb, code := arxiStreams(t, dir, tc.args...)
		if code != tc.want {
			t.Errorf("%s exited %d, want %d\nstdout:\n%s\nstderr:\n%s",
				tc.what, code, tc.want, out, errb)
		}
		if out != "" {
			t.Errorf("%s wrote %q to stdout, want empty: every one of these is a "+
				"non-answer, and a non-answer on stdout is read as the value.",
				tc.what, out)
		}
	}
}

// TestStateGetRefusesAPaddedKeyByPointingAtTheRealOne.
//
// `state set` will not create a padded or line-broken key, so this lookup can only
// ever answer "not set" -- whatever the run holds. Refusing it is therefore more
// honest than answering, and the message has to quote both sides: unquoted, the
// suggestion is character-for-character what the user believes they typed, which
// reads as the command being broken rather than as a fixable mistake.
func TestStateGetRefusesAPaddedKeyByPointingAtTheRealOne(t *testing.T) {
	dir := t.TempDir()
	kvRunAt(t, dir, "r1", [2]string{"api.contract", "frozen"})

	_, errb, code := arxiStreams(t, dir, "state", "get", "r1", "api.contract ")
	if code != 2 {
		t.Fatalf("exit %d, want 2\nstderr:\n%s", code, errb)
	}
	if !strings.Contains(errb, `did you mean "api.contract"?`) {
		t.Errorf("the refusal does not name the key to ask for instead:\n%s", errb)
	}
	if !strings.Contains(errb, `"api.contract "`) {
		t.Errorf("the refusal does not quote what was typed:\n%s\n"+
			"  consequence: the two strings differ by one invisible character, so a "+
			"message that shows neither of them quoted shows the same key twice.", errb)
	}
}

// stateGetJSON runs the command with --json and decodes stdout.
//
// It insists stdout is a document even when the exit code is non-zero, because
// that is the half of the contract a machine caller depends on: it asked for one
// JSON document and it gets one whether or not the key is there.
func stateGetJSON(t *testing.T, dir string, args ...string) (map[string]any, int) {
	t.Helper()
	out, errb, code := arxiStreams(t, dir, append([]string{"state", "get"}, args...)...)
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\nstdout:\n%s\nstderr:\n%s", err, out, errb)
	}
	return got, code
}

// TestStateGetJSONAnswersOnStdoutEvenWhenTheKeyIsUnset.
//
// The negative answer stays a document, and only the exit code carries the "no".
// Switching to prose on stderr for the miss is what makes a --json caller parse
// stdout, find nothing there, and report the command as broken -- and the miss is
// the answer such a caller polls for, so it is the common case, not the edge.
//
// found is a field of its own for resultPayload's reason about has_result: "" is a
// value here, so a reader that tested `value` for emptiness would report a key a
// member emptied on purpose as one that was never set.
func TestStateGetJSONAnswersOnStdoutEvenWhenTheKeyIsUnset(t *testing.T) {
	dir := t.TempDir()
	kvRunAt(t, dir, "r1", [2]string{"api.contract", ""})

	unset, code := stateGetJSON(t, dir, "r1", "db.schema", "--json")
	if code != 3 {
		t.Errorf("a --json miss exited %d, want 3", code)
	}
	if unset["found"] != false {
		t.Errorf("found is %v for a key nobody wrote, want false: %v", unset["found"], unset)
	}
	for _, k := range []string{"set_at_seq", "set_source", "set_ts", "set_by"} {
		if _, ok := unset[k]; ok {
			t.Errorf("%s is present for a key with no write in the log: %v\n"+
				"  consequence: \"no write in this log\" and \"written at seq 0\" are "+
				"different facts, and a reader handed the second prints it as a place "+
				"to go and look.", k, unset)
		}
	}

	// The emptied key is the contrast: same blank value, opposite answer.
	empty, code := stateGetJSON(t, dir, "r1", "api.contract", "-J")
	if code != 0 || empty["found"] != true || empty["value"] != "" {
		t.Errorf("an emptied key reads back as %v with exit %d, want found=true, "+
			"value=\"\", exit 0", empty, code)
	}
}

// TestStateGetJSONNamesTheWriteTheValueCameFrom.
//
// The store keeps one value per key and the reducer overwrites, so the question
// "who set this, and when" has exactly one answer and it is not in the State at
// all -- it is the LAST state.set for the key in the log. Two writes here, because
// a fixture with one passes against an implementation that reports the first.
//
// set_by is the member, and it appears only when a member really set the key: every
// write `arxi state set` makes leaves Actor empty on purpose, so wakeWatchers does
// not read the key as self-set and skip that agent's own watcher on state.*. An
// agent's tool call is the write that does carry one.
func TestStateGetJSONNamesTheWriteTheValueCameFrom(t *testing.T) {
	dir := t.TempDir()
	runAt(t, dir, "r1", "feature-team", 1.0,
		stateSetLine(t, 3, "human", "", "phase", "design")+
			stateSetLine(t, 4, "agent", "backend", "phase", "build"))

	got, code := stateGetJSON(t, dir, "r1", "phase", "--json")
	if code != 0 {
		t.Fatalf("exit %d, want 0: %v", code, got)
	}
	if got["value"] != "build" {
		t.Errorf("value is %v, want build: the fold overwrites, so the value is the "+
			"LAST write and the provenance has to be that same write's.", got["value"])
	}
	if got["set_at_seq"] != 4.0 {
		t.Errorf("set_at_seq is %v, want 4\n"+
			"  consequence: reported as 3, it points a reader at the write this value "+
			"replaced -- the one that says design where the run says build.",
			got["set_at_seq"])
	}
	if got["set_by"] != "backend" || got["set_source"] != "agent" {
		t.Errorf("the write is credited to %v/%v, want backend/agent: %v",
			got["set_by"], got["set_source"], got)
	}
	if got["set_ts"] != "2026-02-01T09:00:04Z" {
		t.Errorf("set_ts is %v, want the timestamp of seq 4", got["set_ts"])
	}
}

// TestStateGetReadsBackWhatStateSetWrote is the one test here built with the code
// under test, and it is deliberate.
//
// Every other fixture in this file is a hand-written log, so a failure cannot be
// blamed on the writer. That leaves one thing unpinned, and it is the thing the
// store is: the two commands agree on the payload. A writer that filed the value
// under `val` and a reader that looked for `value` would pass every test above --
// the fixtures spell the payload the way the reducer reads it, and both halves
// would be tested against the fixture rather than against each other.
//
// The value carries a quote, spaces and a dollar sign for a second reason: these
// go through exec with no shell, so anything that came back different would be
// this pipeline mangling it rather than a shell expanding it.
func TestStateGetReadsBackWhatStateSetWrote(t *testing.T) {
	dir := t.TempDir()
	kvRunAt(t, dir, "r1")

	const value = "POST /v1/tokens\n\"Authorization: Bearer $HOME\" # not a shell"
	if got := arxi(t, dir, "state", "set", "r1", "api.contract", value); got.code != 0 {
		t.Fatalf("state set exited %d:\n%s", got.code, got.out)
	}

	out, _, code := arxiStreams(t, dir, "state", "get", "r1", "api.contract")
	if code != 0 {
		t.Fatalf("state get exited %d after a successful write", code)
	}
	if out != value+"\n" {
		t.Errorf("read back %q,\nwrote      %q\n"+
			"  consequence: the two halves are the store. If they disagree about the "+
			"payload, or about the bytes, then one member's note is unreadable by the "+
			"member it was left for -- and nothing in either command reports it.",
			out, value+"\n")
	}
}

// TestStateGetSeqGuardsTheNextWrite closes ADR-0006's compare-and-set loop.
//
//	seq=$(arxi state get r1 phase -J | jq .seq)
//	arxi state set r1 phase build --if-seq $seq
//
// The seq in the document is the RUN's, not the write's, precisely so it can be
// handed straight back: read and guard then take their number from one fold of one
// log. Reporting set_at_seq there instead would produce a guard that passes on any
// run where somebody else has appended since -- which is the only situation the
// guard exists for.
func TestStateGetSeqGuardsTheNextWrite(t *testing.T) {
	dir := t.TempDir()
	kvRunAt(t, dir, "r1", [2]string{"phase", "design"})

	got, code := stateGetJSON(t, dir, "r1", "phase", "-J")
	if code != 0 {
		t.Fatalf("exit %d, want 0: %v", code, got)
	}
	if got["seq"] != 3.0 {
		t.Fatalf("seq is %v, want the run's head of 3: %v", got["seq"], got)
	}

	seq := fmt.Sprint(int64(got["seq"].(float64)))
	if w := arxi(t, dir, "state", "set", "r1", "phase", "build", "--if-seq", seq); w.code != 0 {
		t.Fatalf("the seq this command handed back was refused as a guard:\n%s\n"+
			"  consequence: the read/guard pair is the store's answer to two members "+
			"writing one key. If the number does not fit --if-seq, the loop cannot be "+
			"written and the second writer wins silently.", w.out)
	}

	// And the write that followed is now the provenance -- with no set_by, because
	// `state set` leaves Actor empty so a member's own state.* watcher still fires.
	after, _ := stateGetJSON(t, dir, "r1", "phase", "-J")
	if after["value"] != "build" || after["set_at_seq"] != 4.0 {
		t.Errorf("after the guarded write the store reads %v, want build at seq 4", after)
	}
	if _, ok := after["set_by"]; ok {
		t.Errorf("set_by is %v for a write a shell made\n"+
			"  consequence: it names a member who did not set the key, and that member "+
			"is exactly the one whose own watcher would have been skipped had the "+
			"command claimed them as the actor.", after["set_by"])
	}
}

// TestStateGetReadsATerminalRunThatStateSetWouldRefuse.
//
// The asymmetry is the judgement in this command. `state set` refuses a terminal
// run because the reducer returns before the StateSet arm on a terminal status, so
// the key would be recorded in the log and read back by nobody. A READ has no such
// problem, and a cancelled or finished run is the case somebody working out what
// happened needs most: the whole point of the store is that a member left a note in
// it, and the run ending is usually when somebody comes looking for the note.
func TestStateGetReadsATerminalRunThatStateSetWouldRefuse(t *testing.T) {
	dir := t.TempDir()
	runAt(t, dir, "r1", "feature-team", 1.0,
		stateSetLine(t, 3, "agent", "backend", "api.contract", "frozen at v2")+
			`{"id":"e4","seq":4,"type":"run.cancelled","payload":{"reason":"superseded"}}
`)

	out, errb, code := arxiStreams(t, dir, "state", "get", "r1", "api.contract")
	if code != 0 {
		t.Fatalf("reading a cancelled run exited %d, want 0\nstderr:\n%s\n"+
			"  consequence: the note a member left is unreachable exactly when it is "+
			"wanted, and the only remaining way to read it is to grep the log.",
			code, errb)
	}
	if out != "frozen at v2\n" {
		t.Errorf("stdout is %q, want the stored value", out)
	}

	// The other half is refused on the same run, and that is not an inconsistency:
	// a write there is one the fold would drop.
	if w := arxi(t, dir, "state", "set", "r1", "api.contract", "reopened"); w.code == 0 {
		t.Errorf("state set accepted a write into a terminal run:\n%s", w.out)
	}
}

// TestStateGetReadsARunSomebodyElseIsWriting.
//
// A live run is held by its executor, which owns the writer lock for as long as it
// is open. This command must not take that lock: the reader would fail with
//
//	run directory runs/... is already open for writing by pid 5871
//
// which says nothing whatever about the key -- and the run being live is the state
// a member polling for another member's contract is in every single time.
//
// The lock is planted by hand rather than by holding a Store, because the assertion
// is about the file's presence and the pid inside it is deliberately not this
// process: a reader that consulted the lock at all would have to decide what to do
// about a holder it cannot see, and there is no answer to that question that
// beats not asking it.
func TestStateGetReadsARunSomebodyElseIsWriting(t *testing.T) {
	dir := t.TempDir()
	kvRunAt(t, dir, "r1", [2]string{"api.contract", "frozen"})

	lock := filepath.Join(dir, "runs", "r1", "writer.lock")
	if err := os.WriteFile(lock, []byte("5871\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, errb, code := arxiStreams(t, dir, "state", "get", "r1", "api.contract")
	if code != 0 || out != "frozen\n" {
		t.Fatalf("a locked run answered %q with exit %d\nstderr:\n%s", out, code, errb)
	}

	// And the lock is still there: a reader that released somebody else's lock would
	// be worse than one that failed on it.
	if _, err := os.Stat(lock); err != nil {
		t.Errorf("the writer lock is gone after a read: %v\n"+
			"  consequence: the process that holds it carries on appending, and the "+
			"next command opens the same run for writing alongside it.", err)
	}
}
