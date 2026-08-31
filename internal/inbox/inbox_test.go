package inbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/logstore"
)

// blocked writes a real run directory whose log leaves backend waiting on an
// approval, and returns its path.
//
// The log is written THROUGH logstore rather than by hand, which is what makes
// these tests worth having: a hand-written file would let this package's reader
// and the real writer drift apart, and the drift would look like "this run has
// no questions" -- indistinguishable from a run that has none.
func blocked(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "r1")

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blueprint.snapshot.yaml"), []byte(
		"name: team\n"+
			"members:\n"+
			"  - name: backend\n"+
			"    tools: [read, write, bash]\n"+
			"stages:\n"+
			"  - name: execute\n"+
			"    advance_when: all\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := logstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Append([]kernel.Event{
		// run_id is in the payload because the real `run start` puts it there and
		// the reducer reads it from there. An earlier version of this fixture
		// omitted it, and TestTheListNamesTheRun caught the empty RunID -- the
		// fixture was wrong, not the code, and a fixture that is not faithful to
		// the real writer is worse than no fixture.
		{ID: "e1", Type: kernel.RunStarted, Payload: map[string]any{
			"actor": "team", "run_id": "r1", "budget_usd": 1.0}},
		{ID: "e2", Type: kernel.StageEntered, Payload: map[string]any{"stage": "execute", "index": 0}},
		{ID: "e3", Type: kernel.AgentActivated, Actor: "backend", Payload: map[string]any{"agent": "backend"}},
		{ID: "e4", Type: kernel.ToolCallDenied, Actor: "backend",
			Payload: map[string]any{"tool": "bash", "policy": "ask"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestAQuestionSurvivesTheProcessThatAskedIt(t *testing.T) {
	dir := blocked(t)

	// Nothing of the asking process remains: this is a fresh read of a
	// directory, which is the whole claim the package makes.
	r, err := OpenRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	items := r.List(true)
	if len(items) != 1 {
		t.Fatalf("read %d pending questions from the log, want 1: %+v", len(items), items)
	}
	got := items[0]
	if got.ID != "inbox-1" {
		t.Errorf("id = %q, want inbox-1 (the reducer mints it, so a different id means "+
			"the answer will match nothing)", got.ID)
	}
	if got.Kind != "tool_approval" {
		t.Errorf("kind = %q, want tool_approval", got.Kind)
	}
	if !strings.Contains(got.Question, "bash") {
		t.Errorf("question = %q, want it to name the tool being approved", got.Question)
	}
	if got.Agent != "backend" {
		t.Errorf("agent = %q, want backend", got.Agent)
	}
	if got.OnTimeout != "deny" {
		t.Errorf("on_timeout = %q, want deny: a human deciding whether to deal with "+
			"this now needs to know what ignoring it costs", got.OnTimeout)
	}
	if got.Replied {
		t.Error("the question reads as already answered, and nobody answered it")
	}
}

func TestTheListNamesTheRunBecauseIdsRepeatAcrossRuns(t *testing.T) {
	dir := blocked(t)
	r, err := OpenRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	items := r.List(true)
	if len(items) == 0 {
		t.Fatal("no questions")
	}
	if items[0].RunID == "" {
		t.Error("the item carries no run id. inbox-1 is minted per run, so two blocked " +
			"runs both have one, and a list showing only the id invites approving the wrong")
	}
}

func TestAnsweringUnblocksTheMemberThatWasWaiting(t *testing.T) {
	dir := blocked(t)

	r, err := OpenRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	st := r.State()
	if m := st.Member("backend"); m == nil || m.State != kernel.MemberWaiting {
		t.Fatalf("backend is not waiting before the answer: %+v", m)
	}

	if _, err := Answer(dir, "inbox-1", Reply{Decision: DecisionApprove}); err != nil {
		t.Fatal(err)
	}

	// Re-read from disk. Folding the same directory again is how another
	// process would see the answer, which is the only view that matters.
	r2, err := OpenRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	st2 := r2.State()
	m := st2.Member("backend")
	if m == nil {
		t.Fatal("backend vanished from the run")
	}
	if m.State == kernel.MemberWaiting {
		t.Error("backend is still waiting after the approval was appended: the answer " +
			"did not reach the reducer, so the run waits forever while looking healthy")
	}
	if m.BlockedOn != nil {
		t.Errorf("blocked_on survived the answer: %v", m.BlockedOn)
	}
	if pending := r2.List(true); len(pending) != 0 {
		t.Errorf("%d questions still pending after answering the only one: %+v",
			len(pending), pending)
	}
}

func TestTheLogSaysWhetherAHumanApprovedOrRejected(t *testing.T) {
	// The reducer unblocks on inbox_id alone and reads neither the text nor the
	// decision, so nothing downstream forces this to be recorded. That is
	// exactly why it is tested: for a command whose purpose is authorising a
	// tool, "who said yes" is the one fact the history must not lose.
	for _, tc := range []struct {
		name  string
		reply Reply
	}{
		{"approve", Reply{Decision: DecisionApprove}},
		{"reject", Reply{Decision: DecisionReject, Text: "it hits staging"}},
		{"answer", Reply{Decision: DecisionAnswer, Text: "use -short"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := blocked(t)
			ev, err := Answer(dir, "inbox-1", tc.reply)
			if err != nil {
				t.Fatal(err)
			}
			if got := ev.Str("decision"); got != tc.reply.Decision {
				t.Errorf("logged decision = %q, want %q: the reducer cannot tell these "+
					"apart, so if the event does not say it, nothing does",
					got, tc.reply.Decision)
			}
			if got := ev.Str("text"); got != tc.reply.Text {
				t.Errorf("logged text = %q, want %q", got, tc.reply.Text)
			}
			if ev.Source != kernel.SourceHuman {
				t.Errorf("source = %q, want %q: it is the only thing distinguishing a "+
					"human approving a tool from the runtime timing the question out",
					ev.Source, kernel.SourceHuman)
			}
			if ev.Seq == 0 {
				t.Error("the appended event came back with no seq, so it was never written")
			}
		})
	}
}

func TestAnsweringTwiceIsRefusedBecauseTheSecondReplyBuysATurn(t *testing.T) {
	dir := blocked(t)
	if _, err := Answer(dir, "inbox-1", Reply{Decision: DecisionApprove}); err != nil {
		t.Fatal(err)
	}
	_, err := Answer(dir, "inbox-1", Reply{Decision: DecisionApprove})
	if !errors.Is(err, ErrAlreadyAnswered) {
		t.Fatalf("second answer returned %v, want ErrAlreadyAnswered: applyInboxReplied "+
			"spawns a turn for the member it unblocks, so a duplicate costs money", err)
	}
}

func TestAnUnknownIdIsToldApartFromAnAnsweredOne(t *testing.T) {
	dir := blocked(t)
	_, err := Answer(dir, "inbox-99", Reply{Decision: DecisionApprove})
	if !errors.Is(err, ErrNoSuchItem) {
		t.Fatalf("unknown id returned %v, want ErrNoSuchItem", err)
	}
	if errors.Is(err, ErrAlreadyAnswered) {
		t.Error("an unknown id also matches ErrAlreadyAnswered, so the two cannot be " +
			"told apart: one is a typo, the other is two people at one run")
	}
}

func TestARejectionWithoutAReasonIsRefused(t *testing.T) {
	dir := blocked(t)
	_, err := Answer(dir, "inbox-1", Reply{Decision: DecisionReject})
	if err == nil {
		t.Fatal("a reason-less rejection was accepted. The reason reaches the agent as " +
			"context, and without one its best guess is to retry what was just refused")
	}
	if !strings.Contains(err.Error(), "reason") {
		t.Errorf("error does not mention the reason: %v", err)
	}
	// And it must not have been written.
	r, err2 := OpenRun(dir)
	if err2 != nil {
		t.Fatal(err2)
	}
	if pending := r.List(true); len(pending) != 1 {
		t.Errorf("the refused rejection changed the log: %d pending, want 1", len(pending))
	}
}

func TestWhitespaceIsNotAReason(t *testing.T) {
	dir := blocked(t)
	if _, err := Answer(dir, "inbox-1", Reply{Decision: DecisionReject, Text: "   \n\t"}); err == nil {
		t.Fatal("spaces were accepted as a reason, which reaches the agent as nothing")
	}
}

func TestAnUnrecognisedDecisionIsRefusedRatherThanDefaulted(t *testing.T) {
	dir := blocked(t)
	for _, d := range []string{"", "yes", "APPROVE", "ok"} {
		_, err := Answer(dir, "inbox-1", Reply{Decision: d, Text: "x"})
		if err == nil {
			t.Errorf("decision %q was accepted. Every available default is a lie about "+
				"what a human said: approve authorises what nobody authorised, reject "+
				"discards work over a typo", d)
		}
	}
}

func TestAnsweredQuestionsAreStillReadable(t *testing.T) {
	dir := blocked(t)
	if _, err := Answer(dir, "inbox-1", Reply{Decision: DecisionApprove}); err != nil {
		t.Fatal(err)
	}
	r, err := OpenRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	all := r.List(false)
	if len(all) != 1 {
		t.Fatalf("history lost the answered question: %+v", all)
	}
	if !all[0].Replied {
		t.Error(`the answered item does not read as replied, so "did I already approve ` +
			`that?" has no honest answer`)
	}
}

func TestTheReaderWorksWhileTheRunHoldsTheWriterLock(t *testing.T) {
	dir := blocked(t)

	// A run is in progress: something owns the directory for writing. This is
	// the normal state of affairs when a human types `arxi inbox`, because the
	// reason they are typing it is that a run stopped and is waiting.
	held, err := logstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	r, err := OpenRun(dir)
	if err != nil {
		t.Fatalf("reading the inbox failed while the run held the lock: %v\n"+
			"  this is the exact situation the command exists for", err)
	}
	if len(r.List(true)) != 1 {
		t.Error("the question was not readable while the log was locked for writing")
	}
}

func TestATruncatedLastLineIsStaleRatherThanCorrupt(t *testing.T) {
	dir := blocked(t)

	// A batch in flight: the writer has put bytes down and not yet finished the
	// line. Refusing here would make `arxi inbox` fail while a run is active.
	path := filepath.Join(dir, eventsFileName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"id":"e5","type":"inbox.replied","pay`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	r, err := OpenRun(dir)
	if err != nil {
		t.Fatalf("an unterminated final line was treated as corruption: %v", err)
	}
	if pending := r.List(true); len(pending) != 1 {
		t.Errorf("%d pending, want 1: the half-written line should be ignored, not "+
			"folded", len(pending))
	}
}

func TestAGarbledCompleteLineIsRefusedRatherThanSkipped(t *testing.T) {
	dir := blocked(t)
	path := filepath.Join(dir, eventsFileName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{not json at all}\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if _, err := OpenRun(dir); err == nil {
		t.Fatal("a complete but unparseable line was skipped. The log is the run's only " +
			"history, so silently dropping part of it produces a state with no events " +
			"to justify it")
	}
}

func TestADirectoryWithNoLogSaysSoAndPointsSomewhere(t *testing.T) {
	_, err := OpenRun(t.TempDir())
	if err == nil {
		t.Fatal("an empty directory was accepted as a run")
	}
	if !strings.Contains(err.Error(), "runs/") {
		t.Errorf("the error does not say where runs live, so the user has nowhere to "+
			"look: %v", err)
	}
}

func TestTheEventsFileNameAgreesWithLogstore(t *testing.T) {
	// This package names the log file itself rather than importing the constant,
	// which logstore keeps private. The risk of a copy is that it drifts and
	// every run then reads as having no questions -- indistinguishable from a
	// run that has none. So the agreement is checked by behaviour: logstore
	// wrote the file in blocked(), and this asserts the name it chose.
	dir := blocked(t)
	if _, err := os.Stat(filepath.Join(dir, eventsFileName)); err != nil {
		t.Fatalf("logstore did not write %q, so this package is reading the wrong "+
			"file and would report every run as having no questions: %v",
			eventsFileName, err)
	}
}

func TestQuestionsAreOrderedNumericallyNotLexicographically(t *testing.T) {
	// inbox-10 sorting before inbox-2 is cosmetic right up until somebody
	// approves the wrong line of a list they picked from by eye.
	ids := []string{"inbox-10", "inbox-2", "inbox-1"}
	for i := 0; i < len(ids); i++ {
		for j := i + 1; j < len(ids); j++ {
			a, b := ids[i], ids[j]
			if a == "inbox-10" && b == "inbox-2" && itemLess(a, b) {
				t.Error("inbox-10 sorts before inbox-2")
			}
		}
	}
	if !itemLess("inbox-2", "inbox-10") {
		t.Error("inbox-2 does not sort before inbox-10")
	}
	if !itemLess("inbox-1", "inbox-2") {
		t.Error("inbox-1 does not sort before inbox-2")
	}
	// A non-conforming id must not panic or reorder silently.
	if itemLess("inbox-x", "inbox-x") {
		t.Error("an id is less than itself")
	}
}

func TestARunWithNoFrozenBlueprintIsStillReadable(t *testing.T) {
	dir := blocked(t)
	if err := os.Remove(filepath.Join(dir, "blueprint.snapshot.yaml")); err != nil {
		t.Fatal(err)
	}
	r, err := OpenRun(dir)
	if err != nil {
		t.Fatalf("a run with no frozen blueprint could not be read: %v\n"+
			"  the questions are in the events, and every testdata scenario has no "+
			"blueprint", err)
	}
	if len(r.List(true)) != 1 {
		t.Error("the question was not readable without a blueprint")
	}
}

func TestItemFindsOneQuestionAndNamesTheRunWhenItCannot(t *testing.T) {
	dir := blocked(t)
	r, err := OpenRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Item("inbox-1"); err != nil {
		t.Fatalf("the question that List returned was not findable by id: %v", err)
	}
	_, err = r.Item("inbox-7")
	if !errors.Is(err, ErrNoSuchItem) {
		t.Fatalf("missing id returned %v, want ErrNoSuchItem", err)
	}
}
