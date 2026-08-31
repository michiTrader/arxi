package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// blockedRun writes a run directory under dir/runs/<id> whose log leaves
// backend waiting on a bash approval.
//
// These tests run the real binary against a real directory, which is not
// belt-and-braces over the package tests: BOTH defects this file guards were
// invisible to the package suite and obvious the first time the command was
// run. One lived in the CLI's own search, the other in a field nothing in the
// package was responsible for setting.
func blockedRun(t *testing.T, dir, id string) {
	t.Helper()
	run := filepath.Join(dir, "runs", id)
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run, "blueprint.snapshot.yaml"), []byte(
		"name: team\n"+
			"members:\n"+
			"  - name: backend\n"+
			"    tools: [read, write, bash]\n"+
			"stages:\n"+
			"  - name: execute\n"+
			"    advance_when: all\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	log := `{"id":"e1","seq":1,"type":"run.started","payload":{"actor":"team","run_id":"` + id + `","budget_usd":5.0}}
{"id":"e2","seq":2,"type":"stage.entered","payload":{"stage":"execute","index":0}}
{"id":"e3","seq":3,"type":"agent.activated","actor":"backend","payload":{"agent":"backend"}}
{"id":"e4","seq":4,"type":"tool.call_denied","actor":"backend","payload":{"tool":"bash","policy":"ask"}}
`
	if err := os.WriteFile(filepath.Join(run, "events.ndjson"), []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInboxListsAPendingApprovalWithItsRunAndAgent(t *testing.T) {
	dir := t.TempDir()
	blockedRun(t, dir, "r1")

	got := arxi(t, dir, "inbox")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.out)
	}
	for _, want := range []string{"inbox-1", "r1", "backend", "tool_approval", "bash"} {
		if !strings.Contains(got.out, want) {
			t.Errorf("the listing does not mention %q:\n%s", want, got.out)
		}
	}
}

func TestAnEmptyInboxSaysWhereItLooked(t *testing.T) {
	// Otherwise "no pending questions" cannot be told apart from being in the
	// wrong directory, and those have very different next steps.
	got := arxi(t, t.TempDir(), "inbox")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.out)
	}
	if !strings.Contains(got.out, "runs") {
		t.Errorf("an empty inbox does not say where it looked:\n%s", got.out)
	}
}

func TestApprovingPrintsTheAgentAndTheSeqTheAnswerLandedAt(t *testing.T) {
	dir := t.TempDir()
	blockedRun(t, dir, "r1")

	got := arxi(t, dir, "inbox", "approve", "inbox-1")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.out)
	}
	// §20.2 prints "approved. backend unblocked (r1 seq 6)". The seq is the
	// part worth asserting: it is the point in the log the answer landed at,
	// which is what `run replay --until-seq` is anchored to.
	for _, want := range []string{"approved", "backend", "r1", "seq 5"} {
		if !strings.Contains(got.out, want) {
			t.Errorf("the confirmation does not mention %q:\n%s", want, got.out)
		}
	}
	// The run does not continue by itself, and a user who is not told that
	// concludes the command did not work.
	if !strings.Contains(got.out, "log") {
		t.Errorf("the confirmation does not say the answer is in the log:\n%s", got.out)
	}
}

func TestTheApprovalIsWrittenWithATimestampAndTheDecision(t *testing.T) {
	dir := t.TempDir()
	blockedRun(t, dir, "r1")

	if got := arxi(t, dir, "inbox", "approve", "inbox-1"); got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.out)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "runs", "r1", "events.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	var ev struct {
		Seq     int64          `json:"seq"`
		Ts      string         `json:"ts"`
		Type    string         `json:"type"`
		Source  string         `json:"source"`
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &ev); err != nil {
		t.Fatal(err)
	}

	if ev.Type != "inbox.replied" {
		t.Fatalf("last event is %q, want inbox.replied", ev.Type)
	}
	// Measured empty on a real log before this was fixed. The append does not go
	// through the effect runner -- a human typed a command, there is no run loop
	// in the process -- so nothing else stamps it. An audit record of who
	// authorised a tool, with no "when", answers half the question.
	if ev.Ts == "" {
		t.Error(`the reply was written with "ts":"", so the log cannot say WHEN a ` +
			`human authorised the tool`)
	}
	if ev.Source != "human" {
		t.Errorf("source = %q, want human: it is the only thing distinguishing a "+
			"human approving from the runtime timing the question out", ev.Source)
	}
	if ev.Payload["decision"] != "approve" {
		t.Errorf("decision = %v, want approve: the reducer cannot tell approve from "+
			"reject, so if the event does not record it, nothing does",
			ev.Payload["decision"])
	}
}

func TestApprovingTwiceSaysAlreadyAnsweredRatherThanNoSuchQuestion(t *testing.T) {
	// The defect this guards: the package had ErrAlreadyAnswered and the CLI
	// destroyed the distinction one layer up by searching only pending items.
	// The user then reads "no pending question inbox-1" about an id in their own
	// terminal history, and goes hunting for a typo they never made.
	dir := t.TempDir()
	blockedRun(t, dir, "r1")

	if got := arxi(t, dir, "inbox", "approve", "inbox-1"); got.code != 0 {
		t.Fatalf("first approve: exit %d: %s", got.code, got.out)
	}

	got := arxi(t, dir, "inbox", "approve", "inbox-1")
	if got.code == 0 {
		t.Fatal("the second approval succeeded, and it would spawn a second turn")
	}
	low := strings.ToLower(got.out)
	if !strings.Contains(low, "already") {
		t.Errorf("the second approval does not say the question was already "+
			"answered:\n%s", got.out)
	}
	if strings.Contains(low, "no pending question") {
		t.Errorf("an answered question is reported as not existing:\n%s", got.out)
	}
	// And it must name the run, because ids repeat across runs.
	if !strings.Contains(got.out, "r1") {
		t.Errorf("it does not say which run:\n%s", got.out)
	}
}

func TestAnIdPendingInTwoRunsIsRefusedRatherThanGuessed(t *testing.T) {
	// inbox-1 is minted per run, so every blocked run has one. Picking one would
	// authorise a tool in a run the user was not looking at.
	dir := t.TempDir()
	blockedRun(t, dir, "r1")
	blockedRun(t, dir, "r2")

	got := arxi(t, dir, "inbox", "approve", "inbox-1")
	if got.code == 0 {
		t.Fatalf("an ambiguous id was resolved by guessing:\n%s", got.out)
	}
	if !strings.Contains(got.out, "r1") || !strings.Contains(got.out, "r2") {
		t.Errorf("the refusal does not name both candidates, so the user cannot act "+
			"on the right one:\n%s", got.out)
	}
}

func TestAnIdAnsweredInOneRunAndPendingInAnotherIsNotAmbiguous(t *testing.T) {
	// The search is over PENDING questions, so a run that already answered
	// inbox-1 is not a candidate. Without that, answering the first run would
	// permanently block the second behind a false ambiguity.
	dir := t.TempDir()
	blockedRun(t, dir, "r1")
	blockedRun(t, dir, "r2")

	// Disambiguate the first one by answering it out of band is not possible
	// through the CLI (that is the point), so answer r1 by removing r2 briefly.
	moved := filepath.Join(dir, "hidden")
	if err := os.Rename(filepath.Join(dir, "runs", "r2"), moved); err != nil {
		t.Fatal(err)
	}
	if got := arxi(t, dir, "inbox", "approve", "inbox-1"); got.code != 0 {
		t.Fatalf("approving the only pending item failed: %s", got.out)
	}
	if err := os.Rename(moved, filepath.Join(dir, "runs", "r2")); err != nil {
		t.Fatal(err)
	}

	got := arxi(t, dir, "inbox", "approve", "inbox-1")
	if got.code != 0 {
		t.Fatalf("inbox-1 was reported ambiguous even though only r2 still has it "+
			"pending:\n%s", got.out)
	}
	if !strings.Contains(got.out, "r2") {
		t.Errorf("the answer did not go to r2:\n%s", got.out)
	}
}

func TestRejectingWithoutAReasonIsAUsageErrorAndWritesNothing(t *testing.T) {
	dir := t.TempDir()
	blockedRun(t, dir, "r1")

	got := arxi(t, dir, "inbox", "reject", "inbox-1")
	if got.code == 0 {
		t.Fatalf("a reason-less rejection was accepted:\n%s", got.out)
	}
	if got.code != 2 {
		t.Errorf("exit %d, want 2: a missing argument is a usage error, and a script "+
			"that separates the two should not file a broken-storage report for it",
			got.code)
	}
	// The question must still be waiting.
	if list := arxi(t, dir, "inbox"); !strings.Contains(list.out, "inbox-1") {
		t.Errorf("the refused rejection changed the log:\n%s", list.out)
	}
}

func TestRejectingCarriesTheReasonIntoTheLog(t *testing.T) {
	dir := t.TempDir()
	blockedRun(t, dir, "r1")

	if got := arxi(t, dir, "inbox", "reject", "inbox-1",
		"--reason", "it hits staging"); got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.out)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "runs", "r1", "events.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "it hits staging") {
		t.Error("the reason is not in the log, so it never reaches the agent as " +
			"context and its best guess is to retry what was refused")
	}
	if !strings.Contains(string(raw), `"decision":"reject"`) {
		t.Error("the log does not record that this was a rejection")
	}
}

func TestReplyIsADifferentActFromReject(t *testing.T) {
	dir := t.TempDir()
	blockedRun(t, dir, "r1")

	if got := arxi(t, dir, "inbox", "reply", "inbox-1", "use the -short flag"); got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.out)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "runs", "r1", "events.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	// Collapsing reply and reject would force the agent to guess whether "no"
	// meant NOT ALLOWED or NOT THAT WAY, and those lead to opposite next moves.
	if !strings.Contains(string(raw), `"decision":"answer"`) {
		t.Errorf("a reply was logged as something other than an answer:\n%s", raw)
	}
	if strings.Contains(string(raw), `"decision":"reject"`) {
		t.Error("a reply was recorded as a rejection")
	}
}

func TestAnUnknownIdIsReportedWithSomewhereToLook(t *testing.T) {
	dir := t.TempDir()
	blockedRun(t, dir, "r1")

	got := arxi(t, dir, "inbox", "approve", "inbox-99")
	if got.code == 0 {
		t.Fatal("an unknown id was accepted")
	}
	if !strings.Contains(got.out, "arxi inbox") {
		t.Errorf("the error does not point at the command that lists what is "+
			"waiting:\n%s", got.out)
	}
}

func TestAnIdWithoutAVerbIsToldWhichVerbsExist(t *testing.T) {
	// `arxi inbox inbox-1` is the natural mistake. Listing the whole inbox as
	// though nothing had been typed would silently ignore what the user asked
	// for.
	dir := t.TempDir()
	blockedRun(t, dir, "r1")

	got := arxi(t, dir, "inbox", "inbox-1")
	if got.code == 0 {
		t.Fatalf("a bare id was silently treated as a listing:\n%s", got.out)
	}
	for _, want := range []string{"approve", "reject", "reply"} {
		if !strings.Contains(got.out, want) {
			t.Errorf("the error does not mention %q:\n%s", want, got.out)
		}
	}
}

func TestTheJSONListingIsMachineReadableAndNamesTheRun(t *testing.T) {
	dir := t.TempDir()
	blockedRun(t, dir, "r1")

	got := arxi(t, dir, "inbox", "--json")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.out)
	}
	var payload struct {
		Items []struct {
			ID       string `json:"id"`
			Run      string `json:"run"`
			Agent    string `json:"agent"`
			Question string `json:"question"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(got.out), &payload); err != nil {
		t.Fatalf("the --json listing is not JSON: %v\n%s", err, got.out)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("got %d items, want 1: %s", len(payload.Items), got.out)
	}
	it := payload.Items[0]
	if it.ID != "inbox-1" || it.Run != "r1" || it.Agent != "backend" {
		t.Errorf("item = %+v, want inbox-1/r1/backend", it)
	}
}

func TestOneBrokenRunDirectoryDoesNotHideTheHealthyOnes(t *testing.T) {
	// A single bad directory reported as a fatal error would look exactly like
	// an empty inbox, and the user would go looking for why their run is not
	// blocked when it is.
	dir := t.TempDir()
	blockedRun(t, dir, "r1")

	bad := filepath.Join(dir, "runs", "r0")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "events.ndjson"),
		[]byte("{not json}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := arxi(t, dir, "inbox")
	if !strings.Contains(got.out, "inbox-1") {
		t.Errorf("the healthy run's question is missing because another directory "+
			"was unreadable:\n%s", got.out)
	}
	if !strings.Contains(strings.ToLower(got.out), "warning") {
		t.Errorf("the unreadable directory was not reported at all, so damage is "+
			"silent:\n%s", got.out)
	}
}

func TestADirectoryThatIsNotARunIsIgnoredRatherThanWarnedAbout(t *testing.T) {
	// Requiring an event log is what stops a stray folder under runs/ from
	// producing a warning on every single listing, which is how warnings stop
	// being read.
	dir := t.TempDir()
	blockedRun(t, dir, "r1")
	if err := os.MkdirAll(filepath.Join(dir, "runs", "notes"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := arxi(t, dir, "inbox")
	if strings.Contains(strings.ToLower(got.out), "warning") {
		t.Errorf("a directory with no log was reported as a broken run:\n%s", got.out)
	}
}
