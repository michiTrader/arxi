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

// ------------------------------------------------------------------ helpers

// seq es el contador de secuencia de los tests. Cada test que compare golden
// tiene que resetearlo (ver TestGolden): si no, el resultado depende de qué
// otros tests corrieron antes, que es la peor clase de test frágil.
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

// started arranca un run y devuelve el estado ya inicializado. Toma la Config
// porque los miembros del estado se derivan del blueprint congelado.
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

// bp es el blueprint de prueba: cuatro miembros (uno advisory), dos etapas con
// reglas de avance distintas, tres watchers. Es deliberadamente el caso
// interesante y no el mínimo, porque los bugs de coordinación no aparecen con
// un solo agente y una sola etapa.
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

// drive hace lo que hace el ejecutor: si el reducer devolvió Emit, esos eventos
// se escriben en el log y vuelven a pasar por Decide.
//
// Este helper existe porque la primera versión de varios tests estaba MAL:
// asertaban "la etapa avanzó" mirando el State que devolvió Decide, y la etapa
// no avanza en ese paso — se emite un evento que avanza la etapa en el paso
// siguiente. Sin este helper, los tests verifican una cosa distinta de la que
// pasa en producción.
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

// ------------------------------------------------------------- arranque

func TestRunStartedInicializaMiembros(t *testing.T) {
	c := bp()
	s := started(c)

	if s.Status != StatusRunning {
		t.Fatalf("status = %q", s.Status)
	}
	if len(s.Members) != 4 {
		t.Fatalf("miembros = %d", len(s.Members))
	}
	if s.StageIndex != -1 {
		t.Errorf("StageIndex = %d, se esperaba -1 (todavía no entró a ninguna etapa)", s.StageIndex)
	}
	if m := s.Member("mediator"); m.State != MemberInactive {
		t.Errorf("advisory arrancó en %q, se esperaba inactive", m.State)
	}
	if m := s.Member("backend"); m.State != MemberIdle {
		t.Errorf("no-advisory arrancó en %q", m.State)
	}
	if s.BlueprintSHA != "abc123" {
		t.Errorf("el run no fijó el blueprint: %q", s.BlueprintSHA)
	}
}

// Protege el default que evita el agujero más caro: dos agentes con `write`
// sobre el mismo directorio se pisan y el lock del KV store no lo impide.
func TestWorkspaceWorktreePorDefault(t *testing.T) {
	c := bp()
	if c.Workspace != "worktree" {
		t.Fatalf("workspace = %q; con miembros que tienen write/bash el default seguro es worktree", c.Workspace)
	}
	sin := Config{Members: []MemberConfig{{Name: "a"}, {Name: "b"}}}.ResolveDefaults()
	if sin.Workspace != "none" {
		t.Errorf("sin write/bash el workspace debería ser none, fue %q", sin.Workspace)
	}
}

// --------------------------------------------------------------- etapas

func TestStageEnteredActivaSoloNoAdvisory(t *testing.T) {
	c := bp()
	s := started(c)
	s, fx := Decide(s, ev(StageEntered, "", map[string]any{"stage": "execute", "index": 0}), c)

	if got := countEffects[SpawnTurn](fx); got != 3 {
		t.Fatalf("turnos abiertos = %d, se esperaban 3 (los 4 miembros menos el advisory)", got)
	}
	for _, f := range fx {
		if sp, ok := f.(SpawnTurn); ok && sp.Agent == "mediator" {
			t.Error("se abrió un turno pago para el advisory")
		}
	}
	if countEffects[SetTimer](fx) != 1 {
		t.Error("la etapa tiene timeout pero no se armó el timer")
	}
	if s.Stage != "execute" {
		t.Errorf("stage = %q", s.Stage)
	}
}

// Resuelve el hueco de IA A: el ejecutor necesita saber qué correr en orden y
// qué en paralelo. Si un SpawnTurn se colara antes del SetTimer, el turno
// podría terminar antes de que exista el timer que lo vigila.
func TestOrdenDeEfectosControlAntesDeIndependientes(t *testing.T) {
	c := bp()
	s := started(c)
	_, fx := Decide(s, ev(StageEntered, "", map[string]any{"stage": "execute", "index": 0}), c)

	visto := false
	for _, f := range fx {
		if f.Class() == ClassIndependent {
			visto = true
		} else if visto {
			t.Fatalf("un efecto de control apareció después de uno independiente: %T", f)
		}
	}
}

func TestQuorumAllAvanzaYCancelaTimer(t *testing.T) {
	c := bp()
	s := started(c)
	s, _ = Decide(s, ev(StageEntered, "", map[string]any{"stage": "execute", "index": 0}), c)

	var fx []Effect
	for _, who := range []string{"backend", "designer", "frontend"} {
		s, fx = Decide(s, ev(StageSubmitted, who, nil), c)
	}

	if countEffects[CancelTimer](fx) != 1 {
		t.Error("la etapa avanzó pero el timer siguió armado: el run podría 'expirar' después de terminar")
	}
	if s.ActiveTimer != "" {
		t.Errorf("ActiveTimer = %q después de avanzar", s.ActiveTimer)
	}
	if countEffects[Emit](fx) < 2 {
		t.Errorf("se esperaban stage.advanced y stage.entered, hubo %d emits", countEffects[Emit](fx))
	}
}

// Protege la consecuencia concreta del rasgo advisory: no cuenta para el quórum.
func TestAdvisoryNoCuentaParaElQuorum(t *testing.T) {
	c := bp()
	s := started(c)
	s, _ = drive(s, ev(StageEntered, "", map[string]any{"stage": "execute", "index": 0}), c)

	s, _ = drive(s, ev(StageSubmitted, "mediator", nil), c)
	if s.Stage != "execute" {
		t.Fatal("el submit de un advisory hizo avanzar la etapa")
	}
	for _, who := range []string{"backend", "designer", "frontend"} {
		s, _ = drive(s, ev(StageSubmitted, who, nil), c)
	}
	if s.Stage != "integrate" {
		t.Errorf("stage = %q; con los 3 no-advisory ya debería estar en integrate", s.Stage)
	}
}

// Protege el default que evita entrenar al usuario a poner timeouts absurdos:
// un timeout de etapa escala, no mata el run.
func TestStageTimeoutEscalaNoFalla(t *testing.T) {
	c := bp()
	s := started(c)
	s, _ = Decide(s, ev(StageEntered, "", map[string]any{"stage": "execute", "index": 0}), c)

	s2, fx := Decide(s, ev(StageTimeout, "", map[string]any{"stage": "execute"}), c)
	if s2.Status == StatusFailed {
		t.Fatal("un timeout de etapa mató el run; el default tiene que ser escalar")
	}
	if countEffects[SpawnTurn](fx) != 1 {
		t.Errorf("se esperaba despertar al mediator, efectos: %#v", fx)
	}
}

func TestStageTimeoutSinObservadorPreguntaAlHumano(t *testing.T) {
	c := bp()
	c.Watchers = nil
	s := started(c)
	s, _ = Decide(s, ev(StageEntered, "", map[string]any{"stage": "execute", "index": 0}), c)

	s, fx := Decide(s, ev(StageTimeout, "", map[string]any{"stage": "execute"}), c)
	if countEffects[AskHuman](fx) != 1 {
		t.Fatal("sin observador hay que preguntar, no tirar el trabajo a la basura")
	}
	if s.Status != StatusBlocked {
		t.Errorf("status = %q, se esperaba blocked", s.Status)
	}
}

// ------------------------------------------------------- turnos y causas

// Protege el ahorro más directo del diseño: cinco razones para hablarle al
// mismo agente son UN turno con cinco causas, no cinco turnos. La diferencia
// es literalmente 5x en la factura.
func TestCoalescingUnTurnoConVariasCausas(t *testing.T) {
	c := bp()
	s := started(c)
	s, _ = Decide(s, ev(AgentActivated, "backend", nil), c)

	for i := 0; i < 3; i++ {
		next, fx := Decide(s, ev(RunPrompt, "", map[string]any{"to": "backend"}), c)
		if countEffects[SpawnTurn](fx) != 0 {
			t.Fatal("se abrió un turno para un agente ocupado")
		}
		s = next
	}
	n := len(s.Member("backend").PendingCauses)
	if n != 3 {
		t.Fatalf("causas encoladas = %d, se esperaban 3: las causas perdidas son trabajo del usuario tirado a la basura", n)
	}

	s, fx := Decide(s, ev(AgentTurnDone, "backend", nil), c)
	if got := countEffects[SpawnTurn](fx); got != 1 {
		t.Fatalf("turnos = %d, se esperaba 1 turno con las %d causas fusionadas", got, n)
	}
	sp, _ := firstEffect[SpawnTurn](fx)
	if len(sp.CauseEvents) != n {
		t.Errorf("causas en el turno = %d, se esperaban %d", len(sp.CauseEvents), n)
	}
	if sp.Coalesced != n {
		t.Errorf("Coalesced = %d; el número tiene que ser auditable", sp.Coalesced)
	}
	if len(s.Member("backend").PendingCauses) != 0 {
		t.Error("las causas no se drenaron: se van a reprocesar para siempre")
	}
}

// Protege que `on_busy: queue` sea encolar de verdad y no abrir un turno
// paralelo que compita con el que ya está corriendo.
func TestSteerAgenteOcupadoSeEncolaNoAbreTurno(t *testing.T) {
	c := bp()
	s := started(c)
	s, _ = Decide(s, ev(AgentActivated, "backend", nil), c)

	s, fx := Decide(s, ev(AgentSteered, "", map[string]any{"to": "backend"}), c)
	if countEffects[SpawnTurn](fx) != 0 {
		t.Fatal("on_busy: queue significa encolar, no abrir un turno paralelo")
	}
	if len(s.Member("backend").PendingCauses) != 1 {
		t.Fatal("el steer se perdió")
	}
}

func TestSteerABlueprintConBroadcast(t *testing.T) {
	c := bp()
	c.Inter.SteerTarget = "broadcast"
	s := started(c)
	_, fx := Decide(s, ev(AgentSteered, "", map[string]any{"text": "cambien de rumbo"}), c)

	// El advisory está inactivo: un broadcast no paga un turno por cada
	// opinador que nadie invocó.
	if got := countEffects[SpawnTurn](fx); got != 3 {
		t.Fatalf("broadcast abrió %d turnos, se esperaban 3", got)
	}
}

// Protege que "coordinator" no sea un tipo especial de agente sino un rol. Un
// solo namespace, sin categorías paralelas.
func TestCoordinatorSeResuelvePorRolNoPorTipo(t *testing.T) {
	c := bp()
	s := started(c)
	_, fx := Decide(s, ev(RunPrompt, "", map[string]any{"text": "hola"}), c)

	sp, ok := firstEffect[SpawnTurn](fx)
	if !ok {
		t.Fatal("un prompt sin destinatario no despertó a nadie")
	}
	if sp.Agent != "backend" {
		t.Errorf("el steer fue a %q; backend tiene role=coordinator", sp.Agent)
	}

	// Sin nadie con ese rol, cae al primer no-advisory: el sistema sigue
	// funcionando en vez de exigir configuración ceremonial.
	c2 := bp()
	c2.Members[0].Role = ""
	s2 := started(c2)
	_, fx2 := Decide(s2, ev(RunPrompt, "", map[string]any{"text": "hola"}), c2)
	sp2, _ := firstEffect[SpawnTurn](fx2)
	if sp2.Agent != "backend" {
		t.Errorf("sin rol declarado el fallback debería ser el primer no-advisory, fue %q", sp2.Agent)
	}
}

// ------------------------------------------------------------- watchers

// Protege contra el bug que se cobra en dólares: un watcher que reacciona a
// sus propios eventos es un bucle infinito con tarjeta de crédito.
func TestAutoExclusionDelWatcher(t *testing.T) {
	c := bp()
	c.Watchers = []Watcher{{Agent: "backend", Pattern: "lock.*", Action: "notify"}}
	s := started(c)

	_, fx := Decide(s, ev(LockAcquired, "backend", map[string]any{"key": "k"}), c)
	for _, f := range fx {
		if sp, ok := f.(SpawnTurn); ok && sp.Agent == "backend" {
			t.Fatal("el watcher se despertó con su propio evento: bucle infinito")
		}
	}

	// Con include_self sí se despierta: la exclusión es un default, no una
	// prohibición. Hay patrones legítimos que necesitan verse a sí mismos.
	c.Watchers[0].IncludeSelf = true
	_, fx = Decide(s, ev(LockAcquired, "backend", map[string]any{"key": "k"}), c)
	if countEffects[SpawnTurn](fx) == 0 {
		t.Error("include_self=true no despertó al watcher")
	}
}

// Protege el otro filtro barato: sin límite de profundidad, un watcher que
// reacciona a lo que otro watcher causó no tiene fondo.
func TestLimiteDeProfundidadCausal(t *testing.T) {
	c := bp()
	c.MaxDepth = 3
	c.Watchers = []Watcher{{Agent: "mediator", Pattern: "lock.*", Action: "notify"}}
	s := started(c)

	e := ev(LockAcquired, "backend", map[string]any{"key": "k"})
	e.Depth = 3 // ya en el límite
	_, fx := Decide(s, e, c)
	if countEffects[SpawnTurn](fx) != 0 {
		t.Fatal("se despertó un watcher pasado el límite de profundidad: la cascada no tiene fondo")
	}
}

// Protege que un merge conflict trivial no mate media hora de trabajo.
func TestConflictoDeRecursoNoFallaElRun(t *testing.T) {
	c := bp()
	s := started(c)
	s, fx := Decide(s, ev(ResourceConflict, "backend", map[string]any{"path": "src/main.go"}), c)

	if s.Status == StatusFailed {
		t.Fatal("un conflicto de recurso mató el run")
	}
	if countEffects[SpawnTurn](fx) != 1 {
		t.Errorf("el conflicto no despertó al observador: %#v", fx)
	}
}

// -------------------------------------------------- humano en el bucle

// Protege que policy=ask sea una pregunta y no un error, y que la pregunta deje
// la referencia estructurada que hace posible el remedio automático.
func TestToolDenegadaCreaInboxConReferenciaEstructurada(t *testing.T) {
	c := bp()
	s := started(c)
	s, fx := Decide(s, ev(ToolCallDenied, "backend", map[string]any{
		"tool": "bash", "policy": "ask",
	}), c)

	if countEffects[AskHuman](fx) != 1 {
		t.Fatal("policy=ask no es un error, es una pregunta")
	}
	m := s.Member("backend")
	if m.State != MemberWaiting || m.Detail != "approval" {
		t.Fatalf("miembro = %q/%q", m.State, m.Detail)
	}
	if m.BlockedOn == nil || m.BlockedOn["inbox_id"] == nil || m.BlockedOn["tool"] != "bash" {
		t.Fatalf("blocked_ref incompleto: %#v", m.BlockedOn)
	}
	if len(s.Inbox) != 1 {
		t.Errorf("inbox = %d items", len(s.Inbox))
	}
}

func TestInboxRespondidoDestrabaYAbreTurno(t *testing.T) {
	c := bp()
	s := started(c)
	s, _ = Decide(s, ev(ToolCallDenied, "backend", map[string]any{"tool": "bash", "policy": "ask"}), c)

	id := s.Inbox[0].ID
	s, fx := Decide(s, ev(InboxReplied, "", map[string]any{"inbox_id": id, "text": "dale"}), c)

	m := s.Member("backend")
	if m.State != MemberIdle {
		t.Errorf("el miembro quedó en %q después de la respuesta", m.State)
	}
	if m.BlockedOn != nil {
		t.Error("blocked_on no se limpió: `run why` seguiría reportando un bloqueo resuelto")
	}
	if countEffects[SpawnTurn](fx) != 1 {
		t.Error("responder no reanudó el trabajo")
	}
	if !s.Inbox[0].Replied {
		t.Error("el item quedó marcado como pendiente")
	}
}

// ---------------------------------------------------------- presupuesto

// Protege el techo que hace que --budget signifique algo con spawn anidado.
func TestPresupuestoAvisaYAgotaSobreElArbol(t *testing.T) {
	c := bp()
	s := started(c) // budget 5.0, umbral 0.8 => avisa en 4.0
	s, fx := Decide(s, ev(LLMResponse, "backend", map[string]any{"cost_usd": 4.2}), c)

	em, ok := firstEffect[Emit](fx)
	if !ok || em.Event.Type != BudgetWarning {
		t.Fatalf("4.2 de 5.0 con umbral 0.8 tenía que avisar; efectos: %#v", fx)
	}

	// El aviso no se repite: si avisara en cada llamada, el usuario aprendería
	// a ignorarlo y el aviso dejaría de servir.
	s, fx = Decide(s, ev(LLMResponse, "backend", map[string]any{"cost_usd": 0.1}), c)
	for _, f := range fx {
		if e2, ok := f.(Emit); ok && e2.Event.Type == BudgetWarning {
			t.Error("el aviso de presupuesto se repitió")
		}
	}

	s, fx = Decide(s, ev(LLMResponse, "designer", map[string]any{"cost_usd": 1.0}), c)
	em2, ok := firstEffect[Emit](fx)
	if !ok || em2.Event.Type != BudgetExceeded {
		t.Fatalf("gasto %.2f sobre presupuesto 5.0 y no se agotó", s.TreeSpentUSD)
	}
	if em2.Event.Payload["tree_spent_usd"] == nil {
		t.Error("budget.exceeded sin tree_spent_usd: el gasto del subárbol es el que importa")
	}
}

// Protege que agotar el presupuesto no tire a la basura el trabajo ya pagado.
func TestPresupuestoAgotadoBloqueaYPregunta(t *testing.T) {
	c := bp()
	s := started(c)
	s, fx := Decide(s, ev(BudgetExceeded, "", map[string]any{
		"tree_spent_usd": 5.0, "budget_usd": 5.0,
	}), c)

	if s.Status != StatusBlocked {
		t.Fatalf("status = %q; agotar el presupuesto bloquea, no mata", s.Status)
	}
	ah, ok := firstEffect[AskHuman](fx)
	if !ok {
		t.Fatal("no se le preguntó a nadie si quiere subir el techo")
	}
	if ah.OnTimeout == "" {
		t.Error("la pregunta no declara qué hacer si nadie contesta")
	}
}

func TestGastoAtribuidoPorMiembro(t *testing.T) {
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
		t.Errorf("run=%v árbol=%v, quiero 0.75 en los dos", s.SpentUSD, s.TreeSpentUSD)
	}
}

// -------------------------------------------------------- quiescencia

// quietCfg es un blueprint construido para quedarse trabado: la regla pide tres
// entregas y solo hay dos miembros que puedan entregar. Es el escenario real
// (una regla de avance imposible de cumplir) donde el run no falla, no termina
// y simplemente se queda callado.
func quietCfg(watch bool) Config {
	c := Config{
		Stages:  []StageConfig{{Name: "solo", AdvanceWhen: "quorum:3"}},
		Members: []MemberConfig{{Name: "a"}, {Name: "b"}, {Name: "vigia", Advisory: true}},
	}
	if watch {
		c.Watchers = []Watcher{{Agent: "vigia", Pattern: "run.quiescent", Action: "activate"}}
	}
	return c.ResolveDefaults()
}

// hastaQuiescencia lleva el run hasta el punto donde nadie tiene nada que hacer.
func hastaQuiescencia(t *testing.T, c Config) (State, []Effect) {
	t.Helper()
	s := started(c)
	s, _ = Decide(s, ev(StageEntered, "", map[string]any{"stage": "solo", "index": 0}), c)
	s, _ = Decide(s, ev(AgentActivated, "a", nil), c)
	s, _ = Decide(s, ev(AgentActivated, "b", nil), c)
	s, _ = Decide(s, ev(StageSubmitted, "a", nil), c)
	return Decide(s, ev(StageSubmitted, "b", nil), c)
}

// Protege el detector del modo de falla más caro de estos sistemas: el sistema
// no falla, no termina, deja de pasar cosas, y el usuario se entera a la mañana
// siguiente. Se emite un EVENTO con diagnóstico, no un estado terminal.
func TestQuiescenciaSeDetectaConDiagnostico(t *testing.T) {
	c := quietCfg(true)
	s, fx := hastaQuiescencia(t, c)

	em, ok := firstEffect[Emit](fx)
	if !ok || em.Event.Type != RunQuiescent {
		t.Fatalf("no se emitió run.quiescent; efectos: %#v", fx)
	}
	diag, _ := em.Event.Payload["diagnosis"].(string)
	if diag == "" {
		t.Fatal("run.quiescent sin diagnóstico: 'el run está quieto' no le sirve a nadie")
	}
	if !strings.Contains(diag, "quorum:3") {
		t.Errorf("el diagnóstico no nombra la regla que no se cumple: %q", diag)
	}
	if s.Status != StatusRunning {
		t.Errorf("status = %q: quiescente NO es un estado terminal", s.Status)
	}
	if !s.QuiescentEmitted {
		t.Error("QuiescentEmitted no quedó marcado: se repetiría en cada evento")
	}
}

// Protege: la quiescencia despierta al coordinador y el run NO falla. Es la
// diferencia entre un sistema que se recupera y uno que solo te avisa que
// perdiste.
func TestQuiescenciaDespiertaAlObservadorNoFalla(t *testing.T) {
	c := quietCfg(true)
	s, fx := hastaQuiescencia(t, c)

	em, _ := firstEffect[Emit](fx)
	em.Event.Seq = nextSeq()
	em.Event.ID = "eq"

	s, fx2 := Decide(s, em.Event, c)

	sp, ok := firstEffect[SpawnTurn](fx2)
	if !ok {
		t.Fatalf("el run se quedó callado y nadie fue despertado; efectos: %#v", fx2)
	}
	if sp.Agent != "vigia" {
		t.Errorf("despertó a %q en vez del observador", sp.Agent)
	}
	if s.Status == StatusFailed {
		t.Error("la quiescencia mató el run en vez de pedir intervención")
	}
}

func TestQuiescenciaSinObservadorSiFalla(t *testing.T) {
	c := quietCfg(false)
	s, fx := hastaQuiescencia(t, c)

	em, ok := firstEffect[Emit](fx)
	if !ok {
		t.Fatal("no se emitió run.quiescent")
	}
	em.Event.Seq = nextSeq()
	em.Event.ID = "eq"

	s, _ = Decide(s, em.Event, c)

	if s.Status != StatusFailed {
		t.Fatalf("status = %q, quiero failed", s.Status)
	}
	if !strings.Contains(s.Result, "quorum:3") {
		t.Errorf("el resultado no arrastra el diagnóstico: %q", s.Result)
	}
}

// Protege la sutileza que hizo fallar la primera implementación: un miembro en
// estado `submitted` NO es runnable. Parece disponible (no piensa, no espera)
// pero ya entregó y no tiene nada que hacer. Contarlo como runnable hace que la
// quiescencia nunca se detecte y el bug es invisible.
func TestSubmittedNoEsRunnable(t *testing.T) {
	if (Member{State: MemberSubmitted}).Runnable() {
		t.Fatal("submitted cuenta como runnable: la quiescencia nunca se detectaría")
	}
	if !(Member{State: MemberIdle}).Runnable() {
		t.Error("idle no cuenta como runnable")
	}
	for _, st := range []MemberState{MemberThinking, MemberTool, MemberWaiting, MemberInactive, MemberFailed} {
		if (Member{State: st}).Runnable() {
			t.Errorf("%q cuenta como runnable", st)
		}
	}
}

// ------------------------------------------------------------ run why

// Protege que `run why` no solo explique sino que dé el comando exacto. La
// diferencia entre "está bloqueado" y "corré `iash inbox approve inbox-1`" es
// toda la utilidad del comando.
func TestRunWhyExplicaYRemedia(t *testing.T) {
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
		t.Fatalf("why no nombra al bloqueado ni la causa:\n%s", txt.String())
	}
	if len(w.Fix) == 0 {
		t.Fatal("why explicó el problema pero no dio el comando que lo arregla")
	}
	if !strings.Contains(strings.Join(w.Fix, "\n"), "iash inbox approve") {
		t.Errorf("el remedio no es ejecutable: %v", w.Fix)
	}
}

func TestRunWhyExplicaLaQuiescencia(t *testing.T) {
	c := quietCfg(false)
	s, _ := hastaQuiescencia(t, c)

	w := Explain(s, c)
	var txt strings.Builder
	for _, l := range w.Lines {
		txt.WriteString(l.Text + "\n")
	}
	if !strings.Contains(txt.String(), "quorum:3") {
		t.Errorf("why no nombra la regla de avance que no se cumple:\n%s", txt.String())
	}
	if len(w.Fix) == 0 {
		t.Error("why no sugiere cómo destrabar un run quiescente")
	}
}

// Protege el contrato del schema: si un bloqueo no trae blocked_ref, `run why`
// lo dice explícitamente en vez de mostrar una línea vacía y dejar al usuario
// pensando que el sistema no sabe nada.
func TestRunWhyDelataBloqueoSinReferencia(t *testing.T) {
	c := bp()
	s := started(c)
	s, _ = Decide(s, ev(AgentBlocked, "backend", map[string]any{"blocked_on": "motivo_raro"}), c)

	w := Explain(s, c)
	var txt strings.Builder
	for _, l := range w.Lines {
		txt.WriteString(l.Text + "\n")
	}
	if !strings.Contains(txt.String(), "violación del schema") {
		t.Errorf("why no delató el bloqueo sin referencia estructurada:\n%s", txt.String())
	}
}

// --------------------------------------------------------- invariantes

// Protege que un tool lento que contesta después del cancel no sea un error: si
// lo fuera, replays perfectamente válidos fallarían.
func TestRunTerminalIgnoraEventosTardios(t *testing.T) {
	c := bp()
	s := started(c)
	s, _ = Decide(s, ev(RunCancelled, "", nil), c)

	s2, fx := Decide(s, ev(ToolCallCompleted, "backend", map[string]any{"tool": "bash"}), c)
	if len(fx) != 0 {
		t.Fatalf("un run cancelado produjo efectos: %#v", fx)
	}
	if s2.Status != StatusCancelled {
		t.Errorf("status cambió a %q después de terminal", s2.Status)
	}
}

// Protege la propiedad de la que dependen replay, --sim y eval: el mismo log
// da el mismo estado, y el fold no toca su entrada.
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
		t.Fatal("dos folds del mismo log dieron estados distintos: replay no sirve para nada")
	}
	afterJSON, _ := json.Marshal(base)
	if string(baseJSON) != string(afterJSON) {
		t.Fatal("Fold mutó el estado de entrada")
	}
}

// Este test es el reemplazo del `match` exhaustivo que Rust da gratis (ver
// ADR-0007). Si alguien agrega una variante de Effect y no la registra, esto
// falla y le dice exactamente qué hacer.
func TestEffectExhaustivo(t *testing.T) {
	got := len(EffectVariants())
	const want = 7
	if got != want {
		t.Fatalf("variantes registradas = %d, se esperaban %d.\n"+
			"Si agregaste una variante de Effect, agregala a allEffectVariants "+
			"y revisá TODOS los switch sobre Effect (grep 'case SpawnTurn').", got, want)
	}
	seen := map[string]bool{}
	for _, v := range EffectVariants() {
		name := fmt.Sprintf("%T", v)
		if seen[name] {
			t.Errorf("variante duplicada en el registro: %s", name)
		}
		seen[name] = true
		if v.Class() != ClassControl && v.Class() != ClassIndependent {
			t.Errorf("%s no declara clase válida", name)
		}
	}
}

// ------------------------------------------------------------- golden

func TestGolden(t *testing.T) {
	// seq se resetea para que el golden no dependa del orden en que corrieron
	// los otros tests. Sin esto, `go test -run TestGolden` da un resultado y
	// `go test` da otro, que es la peor clase de test frágil.
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
		t.Skip("golden ausente; correr con UPDATE_GOLDEN=1 para generarlo")
	}
	if string(want) != string(out) {
		t.Errorf("el fold cambió.\n"+
			"Si el cambio es intencional: UPDATE_GOLDEN=1 go test ./internal/kernel/\n"+
			"y revisá el diff con cuidado.\n--- quiero ---\n%s\n--- obtuve ---\n%s", want, out)
	}
}
