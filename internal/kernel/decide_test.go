package kernel

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Implementation note.

// Implementation note.
// Implementation note.
// Implementation note.
var seq int64

func nextSeq() int64 { seq++; return seq }

func ev(t EventType, actor string, payload map[string]any) Event {
	n := nextSeq()
	return Event{
		Seq: n, ID: "e" + strconv.FormatInt(n, 10),
		Ts: "2026-08-26T00:00:00Z", Type: t, Scope: "run:r1",
		Source: SourceAgent, Actor: actor, Payload: payload,
	}
}

// Implementation note.
// Implementation note.
func started(c Config) State {
	n := nextSeq()
	e := Event{
		Seq: n, ID: "e" + strconv.FormatInt(n, 10),
		Ts: "2026-08-26T00:00:00Z", Type: RunStarted, Scope: "run:r1",
		Source: SourceRuntime,
		Payload: map[string]any{
			"run_id": "r1", "actor": "team", "budget_usd": 5.0,
			"blueprint_sha": "abc123",
		},
	}
	s, _ := Decide(State{}, e, c)
	return s
}

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
func bp() Config {
	return Config{
		Blueprint: "test",
		Members: []MemberConfig{
			{Name: "backend", Role: "coordinator", Tools: []string{"write", "bash"}},
			{Name: "designer"},
			{Name: "frontend", Tools: []string{"write"}},
			{Name: "mediator", Advisory: true},
		},
		Stages: []StageConfig{
			{Name: "execute", AdvanceWhen: "all", TimeoutMs: 1_800_000, OnTimeout: "escalate"},
			{Name: "integrate", AdvanceWhen: "any", OnConflict: "merge"},
		},
		Watchers: []Watcher{
			{Agent: "mediator", Pattern: "stage.timeout", Action: "notify"},
			{Agent: "mediator", Pattern: "resource.conflict", Action: "notify"},
			{Agent: "mediator", Pattern: "run.quiescent", Action: "notify"},
		},
	}.ResolveDefaults()
}

func countEffects[T Effect](fx []Effect) int {
	n := 0
	for _, f := range fx {
		if _, ok := f.(T); ok {
			n++
		}
	}
	return n
}

func firstEffect[T Effect](fx []Effect) (T, bool) {
	for _, f := range fx {
		if v, ok := f.(T); ok {
			return v, true
		}
	}
	var zero T
	return zero, false
}

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
func drive(s State, e Event, c Config) (State, []Effect) {
	s, fx := Decide(s, e, c)
	all := append([]Effect(nil), fx...)
	for _, f := range fx {
		if em, ok := f.(Emit); ok {
			em.Event.Seq = nextSeq()
			em.Event.ID = "d" + strconv.FormatInt(em.Event.Seq, 10)
			var sub []Effect
			s, sub = drive(s, em.Event, c)
			all = append(all, sub...)
		}
	}
	return s, all
}

// Implementation note.

func TestRunStartedInitializesMembers(t *testing.T) {
	c := bp()
	s := started(c)

	if s.Status != StatusRunning {
		t.Fatalf("status = %q", s.Status)
	}
	if len(s.Members) != 4 {
		t.Fatalf("members = %d", len(s.Members))
	}
	if s.StageIndex != -1 {
		t.Errorf("StageIndex = %d, is esperaba -1 (still not entró a not stage)", s.StageIndex)
	}
	if m := s.Member("mediator"); m.State != MemberInactive {
		t.Errorf("advisory arrancó en %q, is esperaba inactive", m.State)
	}
	if m := s.Member("backend"); m.State != MemberIdle {
		t.Errorf("not-advisory arrancó en %q", m.State)
	}
	if s.BlueprintSHA != "abc123" {
		t.Errorf("the run not fixed the blueprint: %q", s.BlueprintSHA)
	}
}

// Implementation note.
// Implementation note.
func TestWorkspaceWorktreeByDefault(t *testing.T) {
	c := bp()
	if c.Workspace != "worktree" {
		t.Fatalf("workspace = %q; with members that have write/bash the default safe is worktree", c.Workspace)
	}
	without := Config{Members: []MemberConfig{{Name: "a"}, {Name: "b"}}}.ResolveDefaults()
	if without.Workspace != "none" {
		t.Errorf("without write/bash the workspace should ser none, was %q", without.Workspace)
	}
}

// Implementation note.

func TestStageEnteredActivatesOnlyNonAdvisory(t *testing.T) {
	c := bp()
	s := started(c)
	s, fx := Decide(s, ev(StageEntered, "", map[string]any{"stage": "execute", "index": 0}), c)

	if got := countEffects[SpawnTurn](fx); got != 3 {
		t.Fatalf("turns abiertos = %d, is esperaban 3 (the 4 members less the advisory)", got)
	}
	for _, f := range fx {
		if sp, ok := f.(SpawnTurn); ok && sp.Agent == "mediator" {
			t.Error("is opened a turn pago for the advisory")
		}
	}
	if countEffects[SetTimer](fx) != 1 {
		t.Error("the stage has timeout pero not is armó the timer")
	}
	if s.Stage != "execute" {
		t.Errorf("stage = %q", s.Stage)
	}
}

// Implementation note.
// Implementation note.
// Implementation note.
func TestControlEffectsBeforeIndependentEffects(t *testing.T) {
	c := bp()
	s := started(c)
	_, fx := Decide(s, ev(StageEntered, "", map[string]any{"stage": "execute", "index": 0}), c)

	visto := false
	for _, f := range fx {
		if f.Class() == ClassIndependent {
			visto = true
		} else if visto {
			t.Fatalf("a effect of control apareció after of one independent: %T", f)
		}
	}
}

func TestQuorumAllAdvancesAndCancelsTimer(t *testing.T) {
	c := bp()
	s := started(c)
	s, _ = Decide(s, ev(StageEntered, "", map[string]any{"stage": "execute", "index": 0}), c)

	var fx []Effect
	for _, who := range []string{"backend", "designer", "frontend"} {
		s, fx = Decide(s, ev(StageSubmitted, who, nil), c)
	}

	if countEffects[CancelTimer](fx) != 1 {
		t.Error("the stage advanced pero the timer siguió armado: the run podría 'expirar' after of terminar")
	}
	if s.ActiveTimer != "" {
		t.Errorf("ActiveTimer = %q after of avanzar", s.ActiveTimer)
	}
	if countEffects[Emit](fx) < 2 {
		t.Errorf("is esperaban stage.advanced and stage.entered, hubo %d emits", countEffects[Emit](fx))
	}
}

// Implementation note.
func TestAdvisoryDoesNotCountTowardQuorum(t *testing.T) {
	c := bp()
	s := started(c)
	s, _ = drive(s, ev(StageEntered, "", map[string]any{"stage": "execute", "index": 0}), c)

	s, _ = drive(s, ev(StageSubmitted, "mediator", nil), c)
	if s.Stage != "execute" {
		t.Fatal("the submit of a advisory hizo avanzar the stage")
	}
	for _, who := range []string{"backend", "designer", "frontend"} {
		s, _ = drive(s, ev(StageSubmitted, who, nil), c)
	}
	if s.Stage != "integrate" {
		t.Errorf("stage = %q; with the 3 not-advisory ya should estar en integrate", s.Stage)
	}
}

// Implementation note.
// Implementation note.
func TestStageTimeoutEscalatesWithoutFailing(t *testing.T) {
	c := bp()
	s := started(c)
	s, _ = Decide(s, ev(StageEntered, "", map[string]any{"stage": "execute", "index": 0}), c)

	s2, fx := Decide(s, ev(StageTimeout, "", map[string]any{"stage": "execute"}), c)
	if s2.Status == StatusFailed {
		t.Fatal("a timeout of stage mató the run; the default has that ser escalate")
	}
	if countEffects[SpawnTurn](fx) != 1 {
		t.Errorf("is esperaba wake to the mediator, effects: %#v", fx)
	}
}

func TestStageTimeoutWithoutWatcherAsksHuman(t *testing.T) {
	c := bp()
	c.Watchers = nil
	s := started(c)
	s, _ = Decide(s, ev(StageEntered, "", map[string]any{"stage": "execute", "index": 0}), c)

	s, fx := Decide(s, ev(StageTimeout, "", map[string]any{"stage": "execute"}), c)
	if countEffects[AskHuman](fx) != 1 {
		t.Fatal("without observador there is that ask, not throw the work a the basura")
	}
	if s.Status != StatusBlocked {
		t.Errorf("status = %q, is esperaba blocked", s.Status)
	}
}

// Implementation note.

// Implementation note.
// Implementation note.
// Implementation note.
func TestCoalescingUnTurnoConVariasCausas(t *testing.T) {
	c := bp()
	s := started(c)
	s, _ = Decide(s, ev(AgentActivated, "backend", nil), c)

	for i := 0; i < 3; i++ {
		next, fx := Decide(s, ev(RunPrompt, "", map[string]any{"to": "backend"}), c)
		if countEffects[SpawnTurn](fx) != 0 {
			t.Fatal("is opened a turn for a agente busy")
		}
		s = next
	}
	n := len(s.Member("backend").PendingCauses)
	if n != 3 {
		t.Fatalf("causes encoladas = %d, is esperaban 3: the causes perdidas are work of the usuario tirado a the basura", n)
	}

	s, fx := Decide(s, ev(AgentTurnDone, "backend", nil), c)
	if got := countEffects[SpawnTurn](fx); got != 1 {
		t.Fatalf("turns = %d, is esperaba 1 turn with the %d causes fusionadas", got, n)
	}
	sp, _ := firstEffect[SpawnTurn](fx)
	if len(sp.CauseEvents) != n {
		t.Errorf("causes en the turn = %d, is esperaban %d", len(sp.CauseEvents), n)
	}
	if sp.Coalesced != n {
		t.Errorf("Coalesced = %d; the number has that ser auditable", sp.Coalesced)
	}
	if len(s.Member("backend").PendingCauses) != 0 {
		t.Error("the causes not is drenaron: is van a reprocesar for always")
	}
}

// Implementation note.
// Implementation note.
func TestSteerBusyAgentQueuesWithoutOpeningTurn(t *testing.T) {
	c := bp()
	s := started(c)
	s, _ = Decide(s, ev(AgentActivated, "backend", nil), c)

	s, fx := Decide(s, ev(AgentSteered, "", map[string]any{"to": "backend"}), c)
	if countEffects[SpawnTurn](fx) != 0 {
		t.Fatal("on_busy: queue means encolar, not open a turn parallel")
	}
	if len(s.Member("backend").PendingCauses) != 1 {
		t.Fatal("the steer is perdió")
	}
}

func TestSteerABlueprintConBroadcast(t *testing.T) {
	c := bp()
	c.Inter.SteerTarget = "broadcast"
	s := started(c)
	_, fx := Decide(s, ev(AgentSteered, "", map[string]any{"text": "cambien of rumbo"}), c)

	// Implementation note.
	// Implementation note.
	if got := countEffects[SpawnTurn](fx); got != 3 {
		t.Fatalf("broadcast opened %d turns, is esperaban 3", got)
	}
}

// Implementation note.
// Implementation note.
func TestCoordinatorResolvesByRoleNotType(t *testing.T) {
	c := bp()
	s := started(c)
	_, fx := Decide(s, ev(RunPrompt, "", map[string]any{"text": "hola"}), c)

	sp, ok := firstEffect[SpawnTurn](fx)
	if !ok {
		t.Fatal("a prompt without destinatario not despertó a nadie")
	}
	if sp.Agent != "backend" {
		t.Errorf("the steer was a %q; backend has role=coordinator", sp.Agent)
	}

	// Implementation note.
	// Implementation note.
	c2 := bp()
	c2.Members[0].Role = ""
	s2 := started(c2)
	_, fx2 := Decide(s2, ev(RunPrompt, "", map[string]any{"text": "hola"}), c2)
	sp2, _ := firstEffect[SpawnTurn](fx2)
	if sp2.Agent != "backend" {
		t.Errorf("without rol declared the fallback should ser the primer not-advisory, was %q", sp2.Agent)
	}
}

// Implementation note.

// Implementation note.
// Implementation note.
func TestWatcherAutoExclusion(t *testing.T) {
	c := bp()
	c.Watchers = []Watcher{{Agent: "backend", Pattern: "lock.*", Action: "notify"}}
	s := started(c)

	_, fx := Decide(s, ev(LockAcquired, "backend", map[string]any{"key": "k"}), c)
	for _, f := range fx {
		if sp, ok := f.(SpawnTurn); ok && sp.Agent == "backend" {
			t.Fatal("the watcher is despertó with its own event: bucle infinito")
		}
	}

	// Implementation note.
	// Implementation note.
	c.Watchers[0].IncludeSelf = true
	_, fx = Decide(s, ev(LockAcquired, "backend", map[string]any{"key": "k"}), c)
	if countEffects[SpawnTurn](fx) == 0 {
		t.Error("include_self=true not despertó to the watcher")
	}
}

// Implementation note.
// Implementation note.
func TestCausalDepthLimit(t *testing.T) {
	c := bp()
	c.MaxDepth = 3
	c.Watchers = []Watcher{{Agent: "mediator", Pattern: "lock.*", Action: "notify"}}
	s := started(c)

	e := ev(LockAcquired, "backend", map[string]any{"key": "k"})
	e.Depth = 3 // ya en the límite
	_, fx := Decide(s, e, c)
	if countEffects[SpawnTurn](fx) != 0 {
		t.Fatal("is despertó a watcher pasado the límite of depth: the cascada not has depth")
	}
}

// Implementation note.
func TestResourceConflictDoesNotFailRun(t *testing.T) {
	c := bp()
	s := started(c)
	s, fx := Decide(s, ev(ResourceConflict, "backend", map[string]any{"path": "src/main.go"}), c)

	if s.Status == StatusFailed {
		t.Fatal("a conflicto of recurso mató the run")
	}
	if countEffects[SpawnTurn](fx) != 1 {
		t.Errorf("the conflicto not despertó to the observador: %#v", fx)
	}
}

// Implementation note.

// Implementation note.
// Implementation note.
func TestDeniedToolCreatesInboxWithStructuredReference(t *testing.T) {
	c := bp()
	s := started(c)
	s, fx := Decide(s, ev(ToolCallDenied, "backend", map[string]any{
		"tool": "bash", "policy": "ask",
	}), c)

	if countEffects[AskHuman](fx) != 1 {
		t.Fatal("policy=ask not is a error, is a question")
	}
	m := s.Member("backend")
	if m.State != MemberWaiting || m.Detail != "approval" {
		t.Fatalf("member = %q/%q", m.State, m.Detail)
	}
	if m.BlockedOn == nil || m.BlockedOn["inbox_id"] == nil || m.BlockedOn["tool"] != "bash" {
		t.Fatalf("blocked_ref incompleto: %#v", m.BlockedOn)
	}
	if len(s.Inbox) != 1 {
		t.Errorf("inbox = %d items", len(s.Inbox))
	}
}

func TestAnsweredInboxUnblocksAndOpensTurn(t *testing.T) {
	c := bp()
	s := started(c)
	s, _ = Decide(s, ev(ToolCallDenied, "backend", map[string]any{"tool": "bash", "policy": "ask"}), c)

	id := s.Inbox[0].ID
	s, fx := Decide(s, ev(InboxReplied, "", map[string]any{"inbox_id": id, "text": "dale"}), c)

	m := s.Member("backend")
	if m.State != MemberIdle {
		t.Errorf("the member remained en %q after of the answer", m.State)
	}
	if m.BlockedOn != nil {
		t.Error("blocked_on not is limpió: `run why` seguiría reportando a block resuelto")
	}
	if countEffects[SpawnTurn](fx) != 1 {
		t.Error("responder not reanudó the work")
	}
	if !s.Inbox[0].Replied {
		t.Error("the item remained marcado como pendiente")
	}
}

// Implementation note.

// Implementation note.
func TestBudgetWarnsAndExhaustsTree(t *testing.T) {
	c := bp()
	s := started(c) // budget 5.0, umbral 0.8 => avisa en 4.0
	s, fx := Decide(s, ev(LLMResponse, "backend", map[string]any{"cost_usd": 4.2}), c)

	em, ok := firstEffect[Emit](fx)
	if !ok || em.Event.Type != BudgetWarning {
		t.Fatalf("4.2 of 5.0 with umbral 0.8 had that avisar; effects: %#v", fx)
	}

	// Implementation note.
	// Implementation note.
	s, fx = Decide(s, ev(LLMResponse, "backend", map[string]any{"cost_usd": 0.1}), c)
	for _, f := range fx {
		if e2, ok := f.(Emit); ok && e2.Event.Type == BudgetWarning {
			t.Error("the aviso of budget is repitió")
		}
	}

	s, fx = Decide(s, ev(LLMResponse, "designer", map[string]any{"cost_usd": 1.0}), c)
	em2, ok := firstEffect[Emit](fx)
	if !ok || em2.Event.Type != BudgetExceeded {
		t.Fatalf("spending %.2f sobre budget 5.0 and not is agotó", s.TreeSpentUSD)
	}
	if em2.Event.Payload["tree_spent_usd"] == nil {
		t.Error("budget.exceeded without tree_spent_usd: the spending of the subárbol is the that importa")
	}
}

// Implementation note.
func TestExhaustedBudgetBlocksAndAsks(t *testing.T) {
	c := bp()
	s := started(c)
	s, fx := Decide(s, ev(BudgetExceeded, "", map[string]any{
		"tree_spent_usd": 5.0, "budget_usd": 5.0,
	}), c)

	if s.Status != StatusBlocked {
		t.Fatalf("status = %q; agotar the budget bloquea, not kills", s.Status)
	}
	ah, ok := firstEffect[AskHuman](fx)
	if !ok {
		t.Fatal("not is le preguntó a nadie if quiere subir the techo")
	}
	if ah.OnTimeout == "" {
		t.Error("the question not declara what make if nadie answers")
	}
}

func TestSpendingAttributedByMember(t *testing.T) {
	c := bp()
	s := started(c)
	s, _ = Decide(s, ev(LLMResponse, "backend", map[string]any{"cost_usd": 0.5}), c)
	s, _ = Decide(s, ev(LLMResponse, "designer", map[string]any{"cost_usd": 0.25}), c)

	if got := s.Member("backend").SpentUSD; got != 0.5 {
		t.Errorf("backend gastó %v, quiero 0.5", got)
	}
	if got := s.Member("designer").SpentUSD; got != 0.25 {
		t.Errorf("designer gastó %v, quiero 0.25", got)
	}
	if s.SpentUSD != 0.75 || s.TreeSpentUSD != 0.75 {
		t.Errorf("run=%v tree=%v, quiero 0.75 en the two", s.SpentUSD, s.TreeSpentUSD)
	}
}

// Implementation note.

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
func quietCfg(watch bool) Config {
	c := Config{
		Stages:  []StageConfig{{Name: "only", AdvanceWhen: "quorum:3"}},
		Members: []MemberConfig{{Name: "a"}, {Name: "b"}, {Name: "vigia", Advisory: true}},
	}
	if watch {
		c.Watchers = []Watcher{{Agent: "vigia", Pattern: "run.quiescent", Action: "activate"}}
	}
	return c.ResolveDefaults()
}

// Implementation note.
func untilQuiescent(t *testing.T, c Config) (State, []Effect) {
	t.Helper()
	s := started(c)
	s, _ = Decide(s, ev(StageEntered, "", map[string]any{"stage": "only", "index": 0}), c)
	s, _ = Decide(s, ev(AgentActivated, "a", nil), c)
	s, _ = Decide(s, ev(AgentActivated, "b", nil), c)
	s, _ = Decide(s, ev(StageSubmitted, "a", nil), c)
	return Decide(s, ev(StageSubmitted, "b", nil), c)
}

// Implementation note.
// Implementation note.
// Implementation note.
func TestQuiescenceDetectedWithDiagnosis(t *testing.T) {
	c := quietCfg(true)
	s, fx := untilQuiescent(t, c)

	em, ok := firstEffect[Emit](fx)
	if !ok || em.Event.Type != RunQuiescent {
		t.Fatalf("not is emitió run.quiescent; effects: %#v", fx)
	}
	diag, _ := em.Event.Payload["diagnosis"].(string)
	if diag == "" {
		t.Fatal("run.quiescent without diagnóstico: 'the run is still' not le works a nadie")
	}
	if !strings.Contains(diag, "quorum:3") {
		t.Errorf("the diagnóstico not nombra the rule that not is meets: %q", diag)
	}
	if s.Status != StatusRunning {
		t.Errorf("status = %q: quiescent NO is a state terminal", s.Status)
	}
	if !s.QuiescentEmitted {
		t.Error("QuiescentEmitted not remained marcado: is repetiría en each event")
	}
}

// Implementation note.
// Implementation note.
// Implementation note.
func TestQuiescenceWakesWatcherWithoutFailing(t *testing.T) {
	c := quietCfg(true)
	s, fx := untilQuiescent(t, c)

	em, _ := firstEffect[Emit](fx)
	em.Event.Seq = nextSeq()
	em.Event.ID = "eq"

	s, fx2 := Decide(s, em.Event, c)

	sp, ok := firstEffect[SpawnTurn](fx2)
	if !ok {
		t.Fatalf("the run is remained silent and nadie was despertado; effects: %#v", fx2)
	}
	if sp.Agent != "vigia" {
		t.Errorf("despertó a %q en vez of the observador", sp.Agent)
	}
	if s.Status == StatusFailed {
		t.Error("the quiescence mató the run en vez of pedir intervención")
	}
}

func TestQuiescenceFailsWithoutWatcher(t *testing.T) {
	c := quietCfg(false)
	s, fx := untilQuiescent(t, c)

	em, ok := firstEffect[Emit](fx)
	if !ok {
		t.Fatal("not is emitió run.quiescent")
	}
	em.Event.Seq = nextSeq()
	em.Event.ID = "eq"

	s, _ = Decide(s, em.Event, c)

	if s.Status != StatusFailed {
		t.Fatalf("status = %q, quiero failed", s.Status)
	}
	if !strings.Contains(s.Result, "quorum:3") {
		t.Errorf("the resultado not arrastra the diagnóstico: %q", s.Result)
	}
}

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
func TestSubmittedNoEsRunnable(t *testing.T) {
	if (Member{State: MemberSubmitted}).Runnable() {
		t.Fatal("submitted cuenta como runnable: the quiescence never is detectaría")
	}
	if !(Member{State: MemberIdle}).Runnable() {
		t.Error("idle not cuenta como runnable")
	}
	for _, st := range []MemberState{MemberThinking, MemberTool, MemberWaiting, MemberInactive, MemberFailed} {
		if (Member{State: st}).Runnable() {
			t.Errorf("%q cuenta como runnable", st)
		}
	}
}

// Implementation note.

// Implementation note.
// Implementation note.
// Implementation note.
func TestRunWhyExplainsAndRemediates(t *testing.T) {
	c := bp()
	s := started(c)
	s, _ = Decide(s, ev(StageEntered, "", map[string]any{"stage": "execute", "index": 0}), c)
	s, _ = Decide(s, ev(ToolCallDenied, "backend", map[string]any{"tool": "bash", "policy": "ask"}), c)

	w := Explain(s, c)
	var txt strings.Builder
	for _, l := range w.Lines {
		txt.WriteString(l.Text + "\n")
	}
	if !strings.Contains(txt.String(), "backend") || !strings.Contains(txt.String(), "bash") {
		t.Fatalf("why not nombra to the blocked ni the cause:\n%s", txt.String())
	}
	if len(w.Fix) == 0 {
		t.Fatal("why explicó the problema pero not dio the command that lo arregla")
	}
	if !strings.Contains(strings.Join(w.Fix, "\n"), "iash inbox approve") {
		t.Errorf("the remedy not is ejecutable: %v", w.Fix)
	}
}

func TestRunWhyExplainsQuiescence(t *testing.T) {
	c := quietCfg(false)
	s, _ := untilQuiescent(t, c)

	w := Explain(s, c)
	var txt strings.Builder
	for _, l := range w.Lines {
		txt.WriteString(l.Text + "\n")
	}
	if !strings.Contains(txt.String(), "quorum:3") {
		t.Errorf("why not nombra the rule of avance that not is meets:\n%s", txt.String())
	}
	if len(w.Fix) == 0 {
		t.Error("why not sugiere how destrabar a run quiescent")
	}
}

// Implementation note.
// Implementation note.
// Implementation note.
func TestRunWhyReportsBlockWithoutReference(t *testing.T) {
	c := bp()
	s := started(c)
	s, _ = Decide(s, ev(AgentBlocked, "backend", map[string]any{"blocked_on": "motivo_raro"}), c)

	w := Explain(s, c)
	var txt strings.Builder
	for _, l := range w.Lines {
		txt.WriteString(l.Text + "\n")
	}
	if !strings.Contains(txt.String(), "violación of the schema") {
		t.Errorf("why not delató the block without reference structured:\n%s", txt.String())
	}
}

// Implementation note.

// Implementation note.
// Implementation note.
func TestRunTerminalIgnoraEventosTardios(t *testing.T) {
	c := bp()
	s := started(c)
	s, _ = Decide(s, ev(RunCancelled, "", nil), c)

	s2, fx := Decide(s, ev(ToolCallCompleted, "backend", map[string]any{"tool": "bash"}), c)
	if len(fx) != 0 {
		t.Fatalf("a run cancelado produjo effects: %#v", fx)
	}
	if s2.Status != StatusCancelled {
		t.Errorf("status cambió a %q after of terminal", s2.Status)
	}
}

// Implementation note.
// Implementation note.
func TestFoldEsDeterministaYNoMutaLaEntrada(t *testing.T) {
	c := bp()
	events := []Event{
		ev(StageEntered, "", map[string]any{"stage": "execute", "index": 0}),
		ev(AgentActivated, "backend", nil),
		ev(LLMResponse, "backend", map[string]any{"cost_usd": 0.5}),
		ev(StageSubmitted, "backend", nil),
	}

	base := started(c)
	baseJSON, _ := json.Marshal(base)

	a, _ := Fold(base, events, c)
	b, _ := Fold(base, events, c)

	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatal("two folds of the same log dieron states distintos: replay not works for nothing")
	}
	afterJSON, _ := json.Marshal(base)
	if string(baseJSON) != string(afterJSON) {
		t.Fatal("Fold mutó the state of input")
	}
}

// Implementation note.
// Implementation note.
// Implementation note.
func TestEffectExhaustivo(t *testing.T) {
	got := len(EffectVariants())
	const want = 7
	if got != want {
		t.Fatalf("variants registradas = %d, is esperaban %d.\n"+
			"Si agregaste a variant of Effect, agregala a allEffectVariants "+
			"and review TODOS the switch sobre Effect (grep 'case SpawnTurn').", got, want)
	}
	seen := map[string]bool{}
	for _, v := range EffectVariants() {
		name := fmt.Sprintf("%T", v)
		if seen[name] {
			t.Errorf("variant duplicada en the registry: %s", name)
		}
		seen[name] = true
		if v.Class() != ClassControl && v.Class() != ClassIndependent {
			t.Errorf("%s not declara clase válida", name)
		}
	}
}

// Implementation note.

func TestGolden(t *testing.T) {
	// Implementation note.
	// Implementation note.
	// Implementation note.
	seq = 0

	c := bp()
	s := started(c)
	events := []Event{
		ev(StageEntered, "", map[string]any{"stage": "execute", "index": 0}),
		ev(AgentActivated, "backend", nil),
		ev(ToolCall, "backend", map[string]any{"tool": "bash"}),
		ev(ToolCallDenied, "backend", map[string]any{"tool": "bash", "policy": "ask"}),
		ev(LLMResponse, "designer", map[string]any{"cost_usd": 0.42}),
		ev(StageSubmitted, "designer", nil),
	}
	s, _ = Fold(s, events, c)
	w := Explain(s, c)

	out, err := json.MarshalIndent(map[string]any{"state": s, "why": w}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	out = append(out, '\n')

	path := filepath.Join("..", "..", "testdata", "scenarios", "blocked-on-approval.json")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, out, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Log("golden regenerado: " + path)
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Skip("golden ausente; correr with UPDATE_GOLDEN=1 for generarlo")
	}
	if string(want) != string(out) {
		t.Errorf("the fold cambió.\n"+
			"Si the change is intencional: UPDATE_GOLDEN=1 go test ./internal/kernel/\n"+
			"and review the diff with cuidado.\n--- quiero ---\n%s\n--- obtuve ---\n%s", want, out)
	}
}
