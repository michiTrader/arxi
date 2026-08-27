package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/michiTrader/iash/internal/surface"
)

// exchange runs lines through the protocol and returns the responses, skipping
// the hello. Tests need the same helper the protocol's own clients need, and
// having one here is also the cheapest proof that serveConn works on any
// reader/writer: no socket is bound anywhere in this file.
func exchange(t *testing.T, lines ...string) []protoResponse {
	t.Helper()
	var in strings.Builder
	for _, l := range lines {
		in.WriteString(l)
		in.WriteString("\n")
	}
	var out strings.Builder
	if err := serveConn(strings.NewReader(in.String()), &out); err != nil {
		t.Fatalf("serveConn returned an error on well-formed input: %v", err)
	}

	var got []protoResponse
	dec := json.NewDecoder(strings.NewReader(out.String()))
	first := true
	for dec.More() {
		if first {
			// The hello is a different shape and is asserted on its own below.
			var h helloMsg
			if err := dec.Decode(&h); err != nil {
				t.Fatalf("the first line is not a hello: %v", err)
			}
			if h.Type != "hello" {
				t.Fatalf("the first line has type %q, expected hello", h.Type)
			}
			first = false
			continue
		}
		var r protoResponse
		if err := dec.Decode(&r); err != nil {
			t.Fatalf("a response line is not valid JSON: %v\nstream:\n%s", err, out.String())
		}
		got = append(got, r)
	}
	return got
}

// one runs a single request and returns its response.
func one(t *testing.T, line string) protoResponse {
	t.Helper()
	rs := exchange(t, line)
	if len(rs) != 1 {
		t.Fatalf("expected exactly 1 response to one request, got %d.\n"+
			"  consequence: a client pairing responses to requests by position "+
			"would be off by one for the rest of the session, attributing every "+
			"answer to the wrong question", len(rs))
	}
	return rs[0]
}

// Every line gets exactly one response, in order.
//
// This is the property the whole protocol rests on and it is the one a client
// cannot defend itself against. A skipped or duplicated response shifts the
// pairing for the rest of the connection: the client attributes an answer to the
// wrong request and, for a mutating command, believes something happened that did
// not.
func TestEveryRequestGetsExactlyOneResponseInOrder(t *testing.T) {
	rs := exchange(t,
		`{"id":"a","type":"schema"}`,
		`{"id":"b","type":"nope.nope"}`,
		`{"id":"c","type":"run.why","params":{"run":"r1"}}`,
		`{"id":"d","type":"schema"}`,
	)
	want := []string{"a", "b", "c", "d"}
	if len(rs) != len(want) {
		t.Fatalf("sent %d requests and got %d responses.\n"+
			"  consequence: the client's request/response pairing is shifted for "+
			"the rest of the connection, so answers are attributed to the wrong "+
			"questions", len(want), len(rs))
	}
	for i, id := range want {
		if rs[i].ID != id {
			t.Errorf("response %d carries id %q, expected %q.\n"+
				"  consequence: responses came back reordered, so a client reading "+
				"them in a goroutine cannot match them to what it sent",
				i, rs[i].ID, id)
		}
	}
}

// The id is echoed verbatim and is the CLIENT's. Rewriting or normalizing it
// breaks the only mechanism a client with several requests in flight has for
// pairing them up.
func TestTheClientsIDComesBackUntouched(t *testing.T) {
	for _, id := range []string{"1", "req-42", "a b c", "🙂", strings.Repeat("x", 200)} {
		body, err := json.Marshal(protoRequest{ID: id, Type: "schema"})
		if err != nil {
			t.Fatal(err)
		}
		got := one(t, string(body))
		if got.ID != id {
			t.Errorf("sent id %q, got %q back.\n"+
				"  consequence: the client cannot match this response to the "+
				"request that caused it, which is the only purpose the field has",
				id, got.ID)
		}
	}
}

// A malformed line is answered, not fatal.
//
// Dropping the connection over one bad line makes a single typo cost every other
// in-flight request on that connection. The failure is the client's and belongs
// on the wire where the client can read it.
func TestAMalformedLineIsAnsweredAndTheStreamContinues(t *testing.T) {
	rs := exchange(t,
		`not json at all`,
		`{"id":"after","type":"schema"}`,
	)
	if len(rs) != 2 {
		t.Fatalf("got %d responses; a bad line must be answered and the stream "+
			"must continue.\n  consequence: one typo drops the connection and "+
			"every other in-flight request on it dies too", len(rs))
	}
	if rs[0].OK || rs[0].Error == nil || rs[0].Error.Code != errMalformed {
		t.Errorf("a non-JSON line answered %+v, expected code %q", rs[0], errMalformed)
	}
	if !rs[1].OK {
		t.Errorf("the request AFTER a malformed line failed: %+v.\n"+
			"  consequence: a bad line poisons the rest of the connection, so a "+
			"client has to reconnect after every mistake", rs[1])
	}
}

// A blank line produces NO response. Clients emit them when flushing, and
// answering with an error would make every well-behaved client manufacture
// failures in its own logs.
func TestABlankLineIsNotARequest(t *testing.T) {
	rs := exchange(t, ``, `   `, `{"id":"real","type":"schema"}`)
	if len(rs) != 1 {
		t.Fatalf("blank lines produced %d responses, expected 1 (only the real "+
			"request).\n  consequence: a client that flushes with a newline gets "+
			"spurious errors it did not earn, and learns to ignore error "+
			"responses", len(rs))
	}
	if rs[0].ID != "real" {
		t.Errorf("the single response is for %q, not the real request", rs[0].ID)
	}
}

// unknown_type and not_implemented must be DIFFERENT codes.
//
// This is the distinction a client acts on: "you asked wrongly" is its own bug
// and retrying will never help, while "no executor yet" is not its bug and
// retrying after an upgrade will. Collapsed into one code, every client either
// retries forever or gives up permanently, and both are wrong half the time.
func TestAnUnknownTypeIsNotTheSameAsAnUnimplementedOne(t *testing.T) {
	unknown := one(t, `{"id":"1","type":"run.nonsense"}`)
	if unknown.OK || unknown.Error.Code != errUnknownType {
		t.Errorf("a nonexistent type answered %+v, expected %q",
			unknown.Error, errUnknownType)
	}

	// run.why is declared Kind|Protocol and has no handler in this build.
	declared := one(t, `{"id":"2","type":"run.why","params":{"run":"r1"}}`)
	if declared.OK {
		t.Fatal("run.why succeeded; this build has no executor for it, so a " +
			"success response is a lie about work that did not happen")
	}
	if declared.Error.Code != errNotImplemented {
		t.Errorf("a declared-but-unimplemented type answered %q, expected %q.\n"+
			"  consequence: the client cannot tell its own bug from this build "+
			"being behind, so it either retries forever or gives up on a "+
			"capability that is coming", declared.Error.Code, errNotImplemented)
	}
	if !strings.Contains(declared.Error.Message, "retrying will not help") {
		t.Errorf("the not_implemented message does not tell the client retrying "+
			"is pointless, which is the one thing it needs to know: %q",
			declared.Error.Message)
	}
}

// A real capability that is deliberately off the wire must SAY so.
//
// A flat "unknown type" sends the client hunting for a typo it never made — the
// exact failure main.go's fallthrough exists to prevent on the CLI. §20.12 keeps
// these 13 off the protocol as a security boundary, and a client is entitled to
// be told that rather than left to conclude the server is broken.
func TestACapabilityHeldOffTheWireExplainsItself(t *testing.T) {
	got := one(t, `{"id":"1","type":"agent.tool.policy"}`)
	if got.OK {
		t.Fatal("agent.tool.policy was accepted over the protocol.\n" +
			"  consequence: an agent can widen its own tool policy over a socket, " +
			"which is the same as having no policy at all")
	}
	if !strings.Contains(got.Error.Message, "deliberate") {
		t.Errorf("the message does not say the omission is deliberate: %q.\n"+
			"  consequence: the client looks for a typo it never made, or files a "+
			"bug against a security boundary working as designed", got.Error.Message)
	}
	if len(got.Error.Fix) == 0 {
		t.Error("no fix offered. The capability exists on the CLI, and naming the " +
			"command is the difference between a refusal and a dead end")
	}
}

// serve must not be reachable over the protocol it serves.
//
// A client that could send `serve` would make the server spawn another server:
// either it fails on an address already bound, or it forks a process per request.
func TestServeIsNotReachableOverItsOwnProtocol(t *testing.T) {
	got := one(t, `{"id":"1","type":"serve"}`)
	if got.OK {
		t.Fatal("the server accepted `serve` over the wire.\n" +
			"  consequence: a client can make the server start another server, " +
			"forking a process per request or failing on a bound address")
	}
	if got.Error.Code != errUnknownType {
		t.Errorf("serve answered %q, expected %q", got.Error.Code, errUnknownType)
	}
}

// An unknown parameter is REFUSED, not ignored.
//
// This is the most expensive silent failure the protocol can have. `run prompt`
// carries if_seq, the compare-and-swap of ADR-0006: a client that misspells it
// and is ignored believes its write was conditional when it was
// last-write-wins. The lost update the CAS exists to catch happens anyway, and
// the log records the write as intended.
func TestAnUnknownParameterIsRefusedRatherThanIgnored(t *testing.T) {
	got := one(t, `{"id":"1","type":"run.prompt","params":{"run":"r1","text":"t","if_sec":41}}`)
	if got.OK {
		t.Fatal("a misspelled if_seq was silently ignored.\n" +
			"  consequence: the client believes its write was conditional when it " +
			"was last-write-wins. The lost update the CAS exists to catch happens " +
			"anyway, and the log records the write as intended, so nothing " +
			"downstream can tell it was unguarded.")
	}
	if got.Error.Code != errBadParams {
		t.Errorf("answered %q, expected %q", got.Error.Code, errBadParams)
	}
	// The message must name BOTH the rejected key and the accepted ones. Saying
	// only "bad parameters" makes the client diff its own request against a schema
	// it has to fetch separately.
	if !strings.Contains(got.Error.Message, "if_sec") {
		t.Errorf("the error does not name the offending key: %q", got.Error.Message)
	}
	if !strings.Contains(got.Error.Message, "if_seq") {
		t.Errorf("the error does not list the accepted keys, so the client cannot "+
			"see its own typo: %q", got.Error.Message)
	}
}

// A missing required parameter is refused. `budget` is the one that matters: a
// run with no ceiling is the surprise bill TestBudgetIsMandatory exists to
// prevent, and the protocol is a second entry point to the same command.
func TestAMissingRequiredParameterIsRefused(t *testing.T) {
	got := one(t, `{"id":"1","type":"run.start","params":{"actor":"a","prompt":"p"}}`)
	if got.OK {
		t.Fatal("run.start was accepted with no budget.\n" +
			"  consequence: the surface declares --budget required and the socket " +
			"entry point does not enforce it, so the registry describes a promise " +
			"this build does not keep and the user meets their real ceiling on the " +
			"invoice")
	}
	if !strings.Contains(got.Error.Message, "budget") {
		t.Errorf("the error does not name budget, so the client must guess which "+
			"of three parameters is missing: %q", got.Error.Message)
	}
}

// Values are not coerced.
//
// {"budget": "2.00"} accepted as a number becomes 0: the string does not parse,
// the zero looks deliberate, and the most cautious-looking request becomes the
// most dangerous one. Same argument as TestRunStartRefusesANonPositiveBudget, one
// layer out.
func TestAValueOfTheWrongTypeIsRefusedRatherThanCoerced(t *testing.T) {
	got := one(t, `{"id":"1","type":"run.start","params":{"actor":"a","prompt":"p","budget":"2.00"}}`)
	if got.OK {
		t.Fatal("a string budget was accepted.\n" +
			"  consequence: coerced to a number it becomes 0, which looks like a " +
			"deliberate ceiling of nothing. The most cautious-looking request the " +
			"client could send is the one that breaks")
	}
	if !strings.Contains(got.Error.Message, "number") {
		t.Errorf("the error does not say what type was expected: %q", got.Error.Message)
	}

	// A bool where a string belongs must fail too, so the check is not
	// number-specific by accident.
	got = one(t, `{"id":"2","type":"run.why","params":{"run":true}}`)
	if got.OK || got.Error.Code != errBadParams {
		t.Errorf("a bool passed as a string parameter answered %+v", got.Error)
	}
}

// An out-of-enum value is refused rather than defaulted.
//
// Defaulting answers a different question than the one asked: `on_busy: "abort"`
// resolving to `queue` means the client asked to REJECT the injection and got it
// applied.
func TestAnOutOfEnumValueIsRefusedRatherThanDefaulted(t *testing.T) {
	got := one(t, `{"id":"1","type":"run.prompt","params":{"run":"r1","text":"t","on_busy":"abort"}}`)
	if got.OK {
		t.Fatal("on_busy=abort was accepted.\n" +
			"  consequence: it falls back to queue, so the client asked to reject " +
			"the injection and got it applied. The server answered a different " +
			"question than the one it was asked and reported success.")
	}
	if !strings.Contains(got.Error.Message, "reject") {
		t.Errorf("the error does not list the accepted values: %q", got.Error.Message)
	}
}

// A JSON null is treated as absent, not as a value of the wrong type. Clients
// that serialize omitted fields as null are common, and rejecting them would fail
// requests that are correct in every way that matters.
func TestANullParameterCountsAsAbsent(t *testing.T) {
	got := one(t, `{"id":"1","type":"blueprint.validate","params":{"path":"../../examples/feature-team.yaml","json":null}}`)
	if !got.OK {
		t.Fatalf("an explicit null for an optional parameter was rejected: %+v.\n"+
			"  consequence: every client that serializes omitted fields as null "+
			"fails on requests that are correct in every way that matters",
			got.Error)
	}

	// But a null for a REQUIRED parameter must still be refused: absent is absent.
	got = one(t, `{"id":"2","type":"run.start","params":{"actor":"a","prompt":"p","budget":null}}`)
	if got.OK {
		t.Error("a null budget was accepted.\n" +
			"  consequence: null read as absent must still fail the required " +
			"check, or `\"budget\": null` becomes the way to start a run with no " +
			"ceiling")
	}
}

// The hello must arrive BEFORE the server reads anything.
//
// The client needs the surface version before it commits to a request, and there
// is nowhere else to get it: `schema` describes agent TOOLS, a different set —
// `run attach` and the three `inbox` replies are on the wire and deliberately not
// tools.
func TestTheHelloPrecedesEverythingAndDescribesTheProtocol(t *testing.T) {
	var out strings.Builder
	// Empty input: the hello must still be written, because a client that
	// connects and waits before sending is the normal case.
	if err := serveConn(strings.NewReader(""), &out); err != nil {
		t.Fatalf("serveConn failed on an empty stream: %v", err)
	}
	var h helloMsg
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &h); err != nil {
		t.Fatalf("no hello was written to a client that sent nothing: %v.\n"+
			"  consequence: a client that connects and waits learns nothing about "+
			"which vocabulary it may use, and has to guess", err)
	}
	if h.SurfaceVersion != surface.SurfaceVersion {
		t.Errorf("hello announces surface v%d, binary is v%d",
			h.SurfaceVersion, surface.SurfaceVersion)
	}

	// The advertised set must BE the registry's, not a copy that can drift.
	want := map[string]bool{}
	for _, c := range surface.ProtocolCommands() {
		want[c.ProtocolType()] = true
	}
	if len(h.Types) != len(want) {
		t.Errorf("hello advertises %d types, the registry exposes %d.\n"+
			"  consequence: the server's own greeting disagrees with the surface, "+
			"so a client following it either misses capabilities or asks for ones "+
			"that do not exist", len(h.Types), len(want))
	}
	for _, ty := range h.Types {
		if !want[ty] {
			t.Errorf("hello advertises %q, which is not Kind|Protocol in the registry", ty)
		}
	}

	// `implemented` must be a SUBSET of `types` and must be accurate, because it
	// is what stops a client discovering the unimplemented state one failed
	// request at a time — which makes a permanent condition look transient.
	for _, ty := range h.Implemented {
		if !want[ty] {
			t.Errorf("hello lists %q as implemented but it is not even a protocol type", ty)
		}
		if _, ok := protoHandlers[ty]; !ok {
			t.Errorf("hello claims %q is implemented and there is no handler.\n"+
				"  consequence: the client sends it, gets not_implemented, and can "+
				"no longer trust the one field that exists to spare it that", ty)
		}
	}
	if len(h.Implemented) != len(protoHandlers) {
		t.Errorf("hello lists %d implemented types and there are %d handlers.\n"+
			"  consequence: a working capability is undiscoverable, so it is used "+
			"by whoever read the source and by nobody else",
			len(h.Implemented), len(protoHandlers))
	}
}

// Every handler key must be a real protocol type.
//
// A typo here is invisible in the worst way: the handler simply never runs, the
// capability reports itself unimplemented, and its code sits in the binary
// looking correct. Nothing else in the system would notice.
func TestEveryHandlerAnswersARealProtocolType(t *testing.T) {
	for ty := range protoHandlers {
		c := surface.LookupProtocol(ty)
		if c == nil {
			t.Errorf("there is a handler for %q, which is not a protocol type in "+
				"surface v%d.\n"+
				"  consequence: the handler never runs. The capability reports "+
				"itself unimplemented while its implementation sits in the binary, "+
				"and no other test would catch it.", ty, surface.SurfaceVersion)
		}
	}
}

// `schema` over the protocol must be the SAME manifest the CLI prints. Two
// documents claiming to be the surface is the failure this design exists to
// prevent, and a second projection is how it would happen.
func TestSchemaOverTheWireIsTheSameManifest(t *testing.T) {
	got := one(t, `{"id":"1","type":"schema"}`)
	if !got.OK {
		t.Fatalf("schema failed over the protocol: %+v", got.Error)
	}

	// Compared as decoded VALUES, not as encoded bytes. The wire result has
	// already round-tripped through `any`, so re-marshalling it orders keys
	// alphabetically while marshalling the struct directly uses field order. A
	// byte comparison would fail on that difference and say the manifests
	// disagree, which is the wrong claim: JSON object key order is not
	// meaningful and no client can observe it. Comparing bytes here would make
	// the test fail for a reason its own message denies.
	if !sameJSON(t, got.Result, surface.BuildManifest()) {
		wire, _ := json.Marshal(got.Result)
		direct, _ := json.Marshal(surface.BuildManifest())
		t.Errorf("the manifest over the wire differs from BuildManifest().\n"+
			"  consequence: two documents both claim to describe the surface, and "+
			"an agent that read one is wrong about the other.\n  wire:   %s\n  direct: %s",
			truncate(string(wire)), truncate(string(direct)))
	}
}

// sameJSON compares two values by their decoded JSON content.
//
// It exists so equality means "a client cannot tell these apart" rather than
// "these serialize to identical bytes". The second is stricter in a way that has
// nothing to do with what the protocol promises: object key order is not
// meaningful in JSON, and asserting on it makes a test fail for a reason its own
// failure message contradicts.
func sameJSON(t *testing.T, a, b any) bool {
	t.Helper()
	norm := func(v any) any {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var out any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	return reflect.DeepEqual(norm(a), norm(b))
}

// blueprint.validate must return STRUCTURE, not the CLI's table.
//
// A client parsing the human output breaks the first time a column widens. And it
// must carry the resolved workspace WITH its reason: `worktree` alone invites a
// client to override it as noise, and naming the members that forced it is what
// makes the decision reviewable (§20.4).
func TestBlueprintValidateReturnsResolvedStructure(t *testing.T) {
	got := one(t, `{"id":"1","type":"blueprint.validate","params":{"path":"../../examples/feature-team.yaml"}}`)
	if !got.OK {
		t.Fatalf("validating the example blueprint failed: %+v", got.Error)
	}
	raw, err := json.Marshal(got.Result)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Name            string `json:"name"`
		SHA             string `json:"sha"`
		Workspace       string `json:"workspace"`
		WorkspaceReason string `json:"workspace_reason"`
		Stages          []struct {
			Name        string `json:"name"`
			AdvanceWhen string `json:"advance_when"`
			OnTimeout   string `json:"on_timeout"`
		} `json:"stages"`
		Members []struct {
			Name     string `json:"name"`
			Advisory bool   `json:"advisory"`
		} `json:"members"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("the result is not the declared structure: %v\n%s", err, raw)
	}

	if out.Name == "" || out.SHA == "" {
		t.Errorf("name or sha missing: %s.\n"+
			"  consequence: the sha is how a client pins the blueprint a run was "+
			"decided against; without it a replay cannot be verified", raw)
	}
	if out.Workspace == "" {
		t.Error("no resolved workspace. It is the isolation boundary between " +
			"agents that write files, and a client cannot review a value it is " +
			"not told")
	}
	if out.WorkspaceReason == "" {
		t.Error("the workspace has no reason attached.\n" +
			"  consequence: `worktree` with no explanation reads as arbitrary, so " +
			"a client overrides it as noise and two writers end up in one " +
			"directory — the exact hole the default exists to close")
	}
	// The resolved fields are what this command is FOR. A stage echoed back
	// without its on_timeout would be the file read back, not the resolution.
	for _, st := range out.Stages {
		if st.AdvanceWhen == "" || st.OnTimeout == "" {
			t.Errorf("stage %q came back without a resolved advance_when/on_timeout.\n"+
				"  consequence: the command degenerates into echoing the file, and "+
				"the defaults the user never wrote stay invisible", st.Name)
		}
	}
	if len(out.Members) == 0 {
		t.Error("no members returned")
	}
}

// A blueprint that does not load is a `failed`, not a crash and not an `ok`.
// The request was well formed; the file is what is wrong, and the codes exist to
// keep those apart.
func TestAnInvalidBlueprintFailsWithoutKillingTheConnection(t *testing.T) {
	rs := exchange(t,
		`{"id":"1","type":"blueprint.validate","params":{"path":"/nonexistent/nope.yaml"}}`,
		`{"id":"2","type":"schema"}`,
	)
	if len(rs) != 2 {
		t.Fatalf("got %d responses, expected 2: a bad file must not end the "+
			"connection", len(rs))
	}
	if rs[0].OK {
		t.Error("a nonexistent blueprint reported success")
	}
	if rs[0].Error.Code != errFailed {
		t.Errorf("a bad file answered %q, expected %q: the request was well "+
			"formed, so bad_params would blame the client for the file's problem",
			rs[0].Error.Code, errFailed)
	}
	if !rs[1].OK {
		t.Error("the request after a failed one also failed, so one bad file " +
			"costs the whole connection")
	}
}

// Success and failure must be distinguishable from `ok` ALONE.
//
// Inferring it from the presence of `error` disagrees with `ok` the first time a
// server omits an empty error object or a client checks the wrong field, and the
// disagreement reads as success.
func TestOKAndErrorNeverContradictEachOther(t *testing.T) {
	rs := exchange(t,
		`{"id":"1","type":"schema"}`,
		`{"id":"2","type":"nope.nope"}`,
	)
	for _, r := range rs {
		if r.OK && r.Error != nil {
			t.Errorf("response %s is ok AND carries an error.\n"+
				"  consequence: a client checking `error` and one checking `ok` "+
				"disagree about the same response, and the disagreement reads as "+
				"success", r.ID)
		}
		if !r.OK && r.Error == nil {
			t.Errorf("response %s failed with no error object.\n"+
				"  consequence: the client is told something went wrong and given "+
				"nothing to act on or report", r.ID)
		}
		if !r.OK && r.Error.Message == "" {
			t.Errorf("response %s carries code %q with no message.\n"+
				"  consequence: every client reimplements the English, and they "+
				"will not agree", r.ID, r.Error.Code)
		}
	}
}

// An oversized line is reported and then the connection ENDS.
//
// Continuing is the dangerous option: after a truncated line the stream position
// is unknown, so the tail of the oversized request would be read as the next one
// and dispatched as whatever it parsed as. That turns one oversized request into
// an arbitrary command nobody sent.
func TestAnOversizedLineEndsTheConnectionInsteadOfDesynchronising(t *testing.T) {
	huge := `{"id":"1","type":"schema","params":{"json":"` +
		strings.Repeat("x", maxLineBytes+1024) + `"}}`
	var out strings.Builder
	err := serveConn(strings.NewReader(huge+"\n"+`{"id":"2","type":"schema"}`+"\n"), &out)
	if err == nil {
		t.Fatal("an oversized line was tolerated.\n" +
			"  consequence: the reader grows to hold whatever arrives, so one " +
			"client can exhaust memory by never sending a newline, and the death " +
			"is an OOM kill with no request in flight to blame")
	}

	// The client must be TOLD, not just disconnected: a socket that closes with
	// no explanation is indistinguishable from a crash.
	if !strings.Contains(out.String(), errLineTooLong) {
		t.Errorf("the client was disconnected without being told why:\n%s",
			truncate(out.String()))
	}
	// And the request AFTER the oversized one must not have been executed.
	if strings.Contains(out.String(), `"id":"2"`) {
		t.Error("the request after an oversized line was answered.\n" +
			"  consequence: the stream was resynchronised on a guess. The tail of " +
			"the oversized line can be dispatched as whatever it happens to parse " +
			"as, which is an arbitrary command nobody sent.")
	}
}

// --listen must refuse everything that is not a unix socket.
//
// The protocol has no authentication and a connected client can spend the budget,
// so the unix socket's file permissions ARE the authentication. A network port
// offers the same control to anyone who reaches it.
//
// The list is deliberately wider than "tcp://". An earlier version of the guard
// special-cased that one scheme and mutation testing showed the clause was dead
// weight — the general check was doing all the work. So the test asserts the
// GENERAL property (only unix:// is admitted) rather than enumerating the schemes
// somebody happened to think of, which is what let the dead clause hide.
func TestListenRefusesEverythingThatIsNotAUnixSocket(t *testing.T) {
	for _, addr := range []string{
		"tcp://0.0.0.0:9000",
		"tcp://127.0.0.1:9000",
		"tcp6://[::]:9000",
		"vsock://3:9000",
		"http://localhost:9000",
		":9000",
		"0.0.0.0:9000",
		"localhost:9000",
		"/tmp/iash.sock", // a bare path: plausible, and not an address
		"unix:/tmp/iash.sock",
		"stdio",
	} {
		if _, err := parseServeFlags([]string{"--listen", addr}); err == nil {
			t.Errorf("--listen %q was accepted.\n"+
				"  consequence: this protocol has no handshake and no token, so any "+
				"address reachable over a network hands whoever reaches it the "+
				"ability to start runs and spend the budget. The unix socket's 0700 "+
				"mode is the only authentication that exists, so anything that is "+
				"not unix:// must be refused by default rather than by having been "+
				"enumerated.", addr)
		}
	}

	// An address carrying a port must be told WHY, not just handed the syntax.
	// "use unix:///path" in answer to `--listen :9000` reads as a formatting nit,
	// and the operator retries with `unix://0.0.0.0:9000`.
	for _, addr := range []string{"tcp://0.0.0.0:9000", ":9000", "localhost:9000"} {
		_, err := parseServeFlags([]string{"--listen", addr})
		if err == nil {
			continue // already reported above
		}
		if !strings.Contains(err.Error(), "authentication") {
			t.Errorf("--listen %q was refused without explaining that the socket "+
				"permissions are the authentication: %v.\n"+
				"  consequence: the refusal reads as a syntax preference, so the "+
				"operator looks for the spelling that works instead of learning "+
				"that a port will never be supported", addr, err)
		}
	}
}

// The accepted forms must keep working, and an empty --listen must mean stdio
// rather than an error: stdio is the default and the common case.
func TestListenAcceptsAUnixSocketAndDefaultsToStdio(t *testing.T) {
	got, err := parseServeFlags(nil)
	if err != nil || got != "" {
		t.Errorf("no flags gave (%q, %v), expected stdio (empty, nil).\n"+
			"  consequence: the common case — a supervisor spawning one server on "+
			"a pipe — cannot be invoked", got, err)
	}
	for _, addr := range []string{"unix:///tmp/iash.sock", "unix://relative.sock"} {
		got, err := parseServeFlags([]string{"--listen", addr})
		if err != nil {
			t.Errorf("--listen %q was rejected: %v", addr, err)
		}
		if got != addr {
			t.Errorf("--listen %q parsed to %q", addr, got)
		}
	}
	// --flag=value must work too, for the same reason as run start: supporting
	// only one spelling makes the other a silent misparse.
	if got, err := parseServeFlags([]string{"--listen=unix:///tmp/x.sock"}); err != nil ||
		got != "unix:///tmp/x.sock" {
		t.Errorf("--listen=value gave (%q, %v)", got, err)
	}
	// A unix:// with no path is an error rather than a silent stdio fallback:
	// the operator asked for a socket and would get a pipe.
	if _, err := parseServeFlags([]string{"--listen", "unix://"}); err == nil {
		t.Error("--listen unix:// with no path was accepted.\n" +
			"  consequence: it falls back to stdio, so the operator asked for a " +
			"socket, got a pipe, and every client fails to connect to an address " +
			"nothing is listening on")
	}
	// An unknown flag is an error rather than ignored, so a typo cannot silently
	// leave the server on stdio when a socket was intended.
	if _, err := parseServeFlags([]string{"--lisen", "unix:///tmp/x.sock"}); err == nil {
		t.Error("a misspelled --listen was ignored, so the server would start on " +
			"stdio while the operator waits for a socket that never appears")
	}
}

func truncate(s string) string {
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}
