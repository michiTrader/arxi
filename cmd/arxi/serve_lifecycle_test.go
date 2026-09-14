package main

import (
	"os"
	"path/filepath"
	"testing"

	hostv1 "github.com/michiTrader/arxi/host/v1"
)

// A decision operation without its job parameter must be a client error, not a
// dispatch. Host authorization is job-scoped by design: guessing the job by
// searching every run would duplicate resource selection outside host dispatch
// and make its reauthorization check run against an invented or ambiguous
// resource. The wire shape enforces what the host always demanded.
func TestDecisionOperationsRequireRunAndItem(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{"approve without run", `{"id":"1","type":"inbox.approve","params":{"item":"q1"}}`},
		{"approve without item", `{"id":"1","type":"inbox.approve","params":{"run":"r1"}}`},
		{"reject without run", `{"id":"1","type":"inbox.reject","params":{"item":"q1"}}`},
		{"reply without run", `{"id":"1","type":"inbox.reply","params":{"item":"q1","text":"postgres"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := &recordingLifecycleHost{capabilities: allDecisionCapabilities()}
			session := newProtoSession(hostv1.Principal{ID: "p"}, host)
			got := oneSession(t, session, tc.line)
			if got.OK {
				t.Fatalf("decision dispatched without its job identity: a job-scoped authorization cannot run against a guessed resource")
			}
		})
	}
}

// The three decision operations must carry both identities and the principal
// from the connection, exactly as the CLI would supply them.
func TestDecisionOperationsCarryRunAndItemToTheHost(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{"approve", `{"id":"1","type":"inbox.approve","params":{"run":"r1","item":"q1"}}`},
		{"reject with reason", `{"id":"1","type":"inbox.reject","params":{"run":"r1","item":"q1","reason":"too risky"}}`},
		{"reply with text", `{"id":"1","type":"inbox.reply","params":{"run":"r1","item":"q1","text":"use postgres"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := &recordingLifecycleHost{capabilities: allDecisionCapabilities(), response: hostv1.Job{ID: "r1"}}
			session := newProtoSession(hostv1.Principal{ID: "operator"}, host)
			got := oneSession(t, session, tc.line)
			if !got.OK {
				t.Fatalf("decision refused: %+v", got.Error)
			}
			switch tc.name {
			case "approve":
				if host.approve.JobID != "r1" || host.approve.ItemID != "q1" || host.approve.Principal.ID != "operator" {
					t.Fatalf("approve request = %#v: both identities and the connection principal must reach host dispatch", host.approve)
				}
			case "reject with reason":
				if host.reject.JobID != "r1" || host.reject.ItemID != "q1" || host.reject.Reason != "too risky" {
					t.Fatalf("reject request = %#v: identities and reason must reach host dispatch", host.reject)
				}
			case "reply with text":
				if host.answer.JobID != "r1" || host.answer.ItemID != "q1" || host.answer.Text != "use postgres" {
					t.Fatalf("answer request = %#v: identities and answer text must reach host dispatch — the text is the substance of the decision", host.answer)
				}
			}
		})
	}
}

// Submit resolves the actor through the same store `run start` uses, so a
// protocol client can only submit blueprints the operator has (stored agents
// or files the operator can read), and rides the model parameter through to
// the host request. The budget ceiling is mandatory on the wire exactly as it
// is on the CLI: an invisible default is a surprise bill.
func TestSubmitResolvesActorAndMapsParametersToTheHost(t *testing.T) {
	dir := t.TempDir()
	oldDir := agentDir
	agentDir = filepath.Join(dir, "agents")
	t.Cleanup(func() { agentDir = oldDir })
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "summarizer.yaml"),
		[]byte("name: summarizer\nmembers:\n  - {name: a}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	host := &recordingLifecycleHost{capabilities: allDecisionCapabilities(),
		submitResponse: hostv1.SubmitResult{JobID: "r1", Status: hostv1.JobRunning}}
	session := newProtoSession(hostv1.Principal{ID: "asha"}, host)
	got := oneSession(t, session, `{"id":"1","type":"run.start","params":{
		"actor":"summarizer","prompt":"summarize the incident","budget":5,"max_turns":3,"sim":true,
		"model":"openai/gpt-4o"}}`)
	if !got.OK {
		t.Fatalf("submit refused: %+v", got.Error)
	}
	req := host.submit
	if req.Principal.ID != "asha" || req.Actor != "summarizer" || req.Prompt != "summarize the incident" ||
		req.BudgetUSD != 5 || req.MaxTurns != 3 || !req.Simulated || req.Model != "openai/gpt-4o" {
		t.Fatalf("submit request = %#v: the protocol must map parameters exactly or a durable job is accepted on altered input", req)
	}
	if string(req.Blueprint) == "" {
		t.Fatal("the resolved blueprint never reached acceptance")
	}
}

// An actor nobody stored and no file can satisfy is a client error naming the
// actor, the same refusal `run start` gives.
func TestSubmitRefusesUnresolvableActors(t *testing.T) {
	host := &recordingLifecycleHost{capabilities: allDecisionCapabilities()}
	session := newProtoSession(hostv1.Principal{ID: "asha"}, host)
	got := oneSession(t, session, `{"id":"1","type":"run.start","params":{
		"actor":"nobody-stored-this","prompt":"x","budget":1}}`)
	if got.OK {
		t.Fatalf("submit of an unresolvable actor was accepted: a protocol client must not be able to invent a blueprint")
	}
}

// Wait returns the terminal projection the host computes; the adapter adds
// nothing. This is the observation half of the first milestone: a client with
// no streaming can still learn how the run ended.
func TestWaitMapsRunToTheHost(t *testing.T) {
	host := &recordingLifecycleHost{capabilities: allDecisionCapabilities(),
		response: hostv1.Job{ID: "r1", Status: hostv1.JobSucceeded, Terminal: true, Result: "done"}}
	session := newProtoSession(hostv1.Principal{ID: "asha"}, host)
	got := oneSession(t, session, `{"id":"1","type":"run.result","params":{"run":"r1"}}`)
	if !got.OK {
		t.Fatalf("wait refused: %+v", got.Error)
	}
	if host.wait.JobID != "r1" || host.wait.Principal.ID != "asha" {
		t.Fatalf("wait request = %#v: the job identity and principal must reach host dispatch", host.wait)
	}
	if got.Result == nil {
		t.Fatal("wait response carries no job projection")
	}
}

func allDecisionCapabilities() hostv1.CapabilitySet {
	return hostv1.CapabilitySet{Capabilities: []hostv1.Capability{
		hostv1.CapabilitySubmit, hostv1.CapabilityWait, hostv1.CapabilityInspect,
		hostv1.CapabilityCancel, hostv1.CapabilityApprove, hostv1.CapabilityReject,
		hostv1.CapabilityAnswer,
	}}
}
