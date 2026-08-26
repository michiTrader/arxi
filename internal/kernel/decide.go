package kernel

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
func Decide(s State, e Event, c Config) (State, []Effect) {
	out := s.Clone()
	out.Seq = e.Seq

	// Implementation note.
	// Implementation note.
	// Implementation note.
	if s.Status.Terminal() {
		return out, nil
	}

	var fx []Effect

	switch e.Type {
	case RunStarted:
		applyRunStarted(&out, e, c)

	case RunPrompt, AgentSteered, AgentNotified:
		fx = append(fx, applyInjection(&out, e, c)...)

	case RunPaused:
		out.Status = StatusPaused
	case RunUnpaused:
		out.Status = StatusRunning
	case RunCancelled:
		out.Status = StatusCancelled
	case RunExpired:
		out.Status = StatusExpired

	case StageEntered:
		fx = append(fx, applyStageEntered(&out, e, c)...)
	case StageSubmitted:
		fx = append(fx, applyStageSubmitted(&out, e, c)...)
	case StageAdvanced:
		out.Stage = e.Str("to")
		out.StageIndex = int(e.Num("to_index"))
	case StageTimeout:
		fx = append(fx, applyStageTimeout(&out, e, c)...)

	case AgentActivated:
		if m := out.Member(e.Actor); m != nil {
			m.State = MemberThinking
			m.SinceSeq = e.Seq
			m.Turns++
			out.Turns++
		}
	case AgentTurnDone:
		fx = append(fx, applyTurnDone(&out, e, c)...)
	case AgentBlocked:
		applyBlocked(&out, e)
	case AgentUnblocked:
		if m := out.Member(e.Actor); m != nil {
			m.State = MemberIdle
			m.Detail = ""
			m.BlockedOn = nil
			m.SinceSeq = e.Seq
		}
	case AgentFailed:
		if m := out.Member(e.Actor); m != nil {
			m.State = MemberFailed
			m.Detail = e.Str("error")
			m.SinceSeq = e.Seq
		}

	case ToolCall:
		if m := out.Member(e.Actor); m != nil {
			m.State = MemberTool
			m.Detail = e.Str("tool")
			m.SinceSeq = e.Seq
		}
	case ToolCallCompleted:
		if m := out.Member(e.Actor); m != nil && m.State == MemberTool {
			m.State = MemberThinking
			m.Detail = ""
		}
	case ToolCallDenied:
		fx = append(fx, applyToolDenied(&out, e)...)

	case LLMResponse:
		applyCost(&out, e, c, &fx)

	case LockAcquired:
		out.Locks = append(out.Locks, Lock{Key: e.Str("key"), Holder: e.Actor})
	case LockReleased:
		releaseLock(&out, e.Str("key"))

	case ResourceConflict:
		// Implementation note.
		// Implementation note.
		// Implementation note.
		// Implementation note.
		fx = append(fx, wakeWatchers(&out, e, c)...)

	case BudgetWarning:
		out.BudgetWarned = true
	case BudgetExceeded:
		// Implementation note.
		// Implementation note.
		out.Status = StatusBlocked
		fx = append(fx, AskHuman{
			Kind:      "budget",
			Question:  fmt.Sprintf("budget agotado (%.4f of %.4f USD en the tree). ¿subir or cancel?", out.TreeSpentUSD, out.BudgetUSD),
			OnTimeout: "fail",
		})

	case InboxCreated:
		out.Inbox = append(out.Inbox, InboxItem{
			ID:        e.Str("inbox_id"),
			Kind:      e.Str("kind"),
			Question:  e.Str("question"),
			Agent:     e.Str("agent"),
			OnTimeout: e.Str("on_timeout"),
		})
	case InboxReplied:
		fx = append(fx, applyInboxReplied(&out, e, c)...)
	case InboxTimeout:
		fx = append(fx, applyInboxTimeout(&out, e)...)

	case TimerTick:
		if out.ActiveTimer == e.Str("timer_id") {
			out.ActiveTimer = ""
		}

	case RunQuiescent:
		// Implementation note.
		// Implementation note.
		// Implementation note.
		w := wakeWatchers(&out, e, c)
		fx = append(fx, w...)
		if len(w) == 0 {
			out.Status = StatusFailed
			out.Result = "run quiescent without observador: " + e.Str("diagnosis")
		}

	case RunResult:
		out.Status = StatusSucceeded
		out.Result = e.Str("summary")
	}

	// Implementation note.
	// Implementation note.
	// Implementation note.
	if !isWatcherDispatched(e.Type) && e.Source != SourceRuntime {
		fx = append(fx, wakeWatchers(&out, e, c)...)
	}

	// Implementation note.
	// Implementation note.
	// Implementation note.
	// Implementation note.
	fx = append(fx, checkQuiescence(&out, e, c, fx)...)

	return out, orderEffects(fx)
}

// Implementation note.
// Implementation note.
func Fold(s State, events []Event, c Config) (State, []Effect) {
	var all []Effect
	for _, e := range events {
		var fx []Effect
		s, fx = Decide(s, e, c)
		all = append(all, fx...)
	}
	return s, all
}

func applyRunStarted(out *State, e Event, c Config) {
	out.RunID = e.Str("run_id")
	out.Actor = e.Str("actor")
	out.Status = StatusRunning
	out.BlueprintSHA = e.Str("blueprint_sha")
	out.BudgetUSD = e.Num("budget_usd")
	out.MaxTurns = int(e.Num("max_turns"))
	out.ParentRunID = e.Str("parent_run_id")
	out.SpawnDepth = int(e.Num("spawn_depth"))
	out.NextInboxID = 1

	// Implementation note.
	// Implementation note.
	out.StageIndex = -1

	out.Members = nil
	for _, mc := range c.Members {
		st := MemberIdle
		if mc.Advisory {
			// Implementation note.
			// Implementation note.
			st = MemberInactive
		}
		out.Members = append(out.Members, Member{
			Name:     mc.Name,
			Role:     mc.Role,
			Advisory: mc.Advisory,
			State:    st,
			SinceSeq: e.Seq,
		})
	}
}

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
func applyInjection(out *State, e Event, c Config) []Effect {
	target := resolveSteerTarget(out, c, e.Str("to"))
	var fx []Effect

	for i := range out.Members {
		m := &out.Members[i]
		if target != "*" && m.Name != target {
			continue
		}
		// Implementation note.
		// Implementation note.
		// Implementation note.
		// Implementation note.
		if target == "*" && m.State == MemberInactive {
			continue
		}
		if m.Busy() || m.State == MemberWaiting {
			m.PendingCauses = append(m.PendingCauses, e.ID)
			continue
		}
		if m.State == MemberFailed {
			continue
		}
		m.State = MemberIdle
		m.Submitted = false
		fx = append(fx, spawnFor(out, *m, c, []string{e.ID}, 0))
	}
	return fx
}

// Implementation note.
// Implementation note.
// Implementation note.
func resolveSteerTarget(out *State, c Config, explicit string) string {
	if explicit != "" {
		return explicit
	}
	switch t := c.Inter.SteerTarget; {
	case t == "broadcast":
		return "*"
	case strings.HasPrefix(t, "slot:"):
		return strings.TrimPrefix(t, "slot:")
	case t == "coordinator":
		for _, m := range out.Members {
			if m.Role == "coordinator" {
				return m.Name
			}
		}
		for _, m := range out.Members {
			if !m.Advisory {
				return m.Name
			}
		}
	}
	return c.Inter.SteerTarget
}

func applyStageEntered(out *State, e Event, c Config) []Effect {
	name := e.Str("stage")
	idx := int(e.Num("index"))
	out.Stage = name
	out.StageIndex = idx

	var fx []Effect
	if st := c.StageAt(idx); st != nil && st.TimeoutMs > 0 {
		id := "stage:" + name
		out.ActiveTimer = id
		fx = append(fx, SetTimer{ID: id, FiresAtMs: st.TimeoutMs})
	}

	for i := range out.Members {
		m := &out.Members[i]
		m.Submitted = false
		if m.Advisory || !participates(c, m.Name, name) {
			continue
		}
		if m.State == MemberFailed {
			continue
		}
		m.State = MemberIdle
		fx = append(fx, spawnFor(out, *m, c, []string{e.ID}, 0))
	}
	return fx
}

func participates(c Config, member, stage string) bool {
	mc := c.MemberCfg(member)
	if mc == nil || len(mc.Stages) == 0 {
		return true
	}
	for _, s := range mc.Stages {
		if s == stage {
			return true
		}
	}
	return false
}

func applyStageSubmitted(out *State, e Event, c Config) []Effect {
	if m := out.Member(e.Actor); m != nil {
		m.Submitted = true
		m.State = MemberSubmitted
		m.SinceSeq = e.Seq
	}

	st := c.StageAt(out.StageIndex)
	if st == nil || !quorumMet(*out, c, *st) {
		return nil
	}

	var fx []Effect
	if out.ActiveTimer != "" {
		fx = append(fx, CancelTimer{ID: out.ActiveTimer})
		out.ActiveTimer = ""
	}

	next := out.StageIndex + 1
	if next >= len(c.Stages) {
		fx = append(fx, Emit{Event: derived(out, e, RunResult, map[string]any{
			"summary":     "all the stages completed",
			"result_from": c.ResultFrom,
		})})
		return fx
	}

	fx = append(fx,
		Emit{Event: derived(out, e, StageAdvanced, map[string]any{
			"from":     out.Stage,
			"to":       c.Stages[next].Name,
			"to_index": next,
		})},
		Emit{Event: derived(out, e, StageEntered, map[string]any{
			"stage": c.Stages[next].Name,
			"index": next,
		})},
		Snapshot{AtSeq: e.Seq},
	)
	return fx
}

// Implementation note.
// Implementation note.
func quorumMet(s State, c Config, st StageConfig) bool {
	var total, done int
	for _, m := range s.Members {
		if m.Advisory || !participates(c, m.Name, st.Name) {
			continue
		}
		total++
		if m.Submitted {
			done++
		}
	}
	if total == 0 {
		return false
	}
	switch {
	case st.AdvanceWhen == "any":
		return done >= 1
	case st.AdvanceWhen == "all":
		return done == total
	case strings.HasPrefix(st.AdvanceWhen, "quorum:"):
		n, err := strconv.Atoi(strings.TrimPrefix(st.AdvanceWhen, "quorum:"))
		if err != nil {
			return done == total
		}
		return done >= n
	case st.AdvanceWhen == "coordinator":
		for _, m := range s.Members {
			if m.Role == "coordinator" {
				return m.Submitted
			}
		}
		return done == total
	}
	return done == total
}

// Implementation note.
// Implementation note.
// Implementation note.
func applyStageTimeout(out *State, e Event, c Config) []Effect {
	out.ActiveTimer = ""
	st := c.StageAt(out.StageIndex)
	action := "escalate"
	if st != nil && st.OnTimeout != "" {
		action = st.OnTimeout
	}

	switch action {
	case "fail":
		out.Status = StatusFailed
		out.Result = "stage " + out.Stage + " expired"
		return nil

	case "advance":
		next := out.StageIndex + 1
		if next >= len(c.Stages) {
			return []Effect{Emit{Event: derived(out, e, RunResult, map[string]any{
				"summary": "última stage expired, advancing",
			})}}
		}
		return []Effect{
			Emit{Event: derived(out, e, StageAdvanced, map[string]any{
				"from": out.Stage, "to": c.Stages[next].Name, "to_index": next,
			})},
			Emit{Event: derived(out, e, StageEntered, map[string]any{
				"stage": c.Stages[next].Name, "index": next,
			})},
		}

	default: // escalate
		fx := wakeWatchers(out, e, c)
		if len(fx) > 0 {
			return fx
		}
		out.Status = StatusBlocked
		return []Effect{AskHuman{
			Kind:      "stage_timeout",
			Question:  "the stage " + out.Stage + " expired and nadie the observa. ¿avanzar, extender or cancel?",
			OnTimeout: "fail",
		}}
	}
}

// Implementation note.
// Implementation note.
// Implementation note.
func applyTurnDone(out *State, e Event, c Config) []Effect {
	m := out.Member(e.Actor)
	if m == nil {
		return nil
	}
	m.State = MemberIdle
	m.Detail = ""
	m.SinceSeq = e.Seq

	if len(m.PendingCauses) == 0 {
		return nil
	}
	causes := m.PendingCauses
	m.PendingCauses = nil
	return []Effect{spawnFor(out, *m, c, causes, len(causes))}
}

func applyBlocked(out *State, e Event) {
	m := out.Member(e.Actor)
	if m == nil {
		return
	}
	m.State = MemberWaiting
	m.SinceSeq = e.Seq
	m.Detail = e.Str("blocked_on")

	// Implementation note.
	// Implementation note.
	if raw, ok := e.Payload["blocked_ref"].(map[string]any); ok {
		m.BlockedOn = raw
	}
}

// Implementation note.
// Implementation note.
// Implementation note.
func applyToolDenied(out *State, e Event) []Effect {
	tool := e.Str("tool")
	if e.Str("policy") != "ask" {
		if m := out.Member(e.Actor); m != nil {
			m.State = MemberIdle
			m.Detail = "tool " + tool + " denegada"
		}
		return nil
	}

	id := "inbox-" + strconv.Itoa(out.NextInboxID)
	out.NextInboxID++
	item := InboxItem{
		ID:        id,
		Kind:      "tool_approval",
		Question:  "aprobar " + tool + " for " + e.Actor + "?",
		Agent:     e.Actor,
		OnTimeout: "deny",
	}
	out.Inbox = append(out.Inbox, item)

	if m := out.Member(e.Actor); m != nil {
		m.State = MemberWaiting
		m.Detail = "approval"
		m.SinceSeq = e.Seq
		m.BlockedOn = map[string]any{"inbox_id": id, "tool": tool, "policy": "ask"}
	}

	return []Effect{AskHuman{
		Kind:      item.Kind,
		Question:  item.Question,
		Agent:     e.Actor,
		OnTimeout: item.OnTimeout,
	}}
}

func applyInboxReplied(out *State, e Event, c Config) []Effect {
	id := e.Str("inbox_id")
	for i := range out.Inbox {
		if out.Inbox[i].ID == id {
			out.Inbox[i].Replied = true
		}
	}
	var fx []Effect
	for i := range out.Members {
		m := &out.Members[i]
		if m.BlockedOn == nil || m.BlockedOn["inbox_id"] != id {
			continue
		}
		m.State = MemberIdle
		m.Detail = ""
		m.BlockedOn = nil
		m.SinceSeq = e.Seq
		if out.Status == StatusBlocked {
			out.Status = StatusRunning
		}
		fx = append(fx, spawnFor(out, *m, c, []string{e.ID}, 0))
	}
	if out.Status == StatusBlocked {
		out.Status = StatusRunning
	}
	return fx
}

func applyInboxTimeout(out *State, e Event) []Effect {
	id := e.Str("inbox_id")
	action := ""
	for i := range out.Inbox {
		if out.Inbox[i].ID == id {
			action = out.Inbox[i].OnTimeout
			out.Inbox[i].Replied = true
		}
	}
	if action == "fail" {
		out.Status = StatusFailed
		out.Result = "nadie contestó " + id
		return nil
	}
	for i := range out.Members {
		m := &out.Members[i]
		if m.BlockedOn != nil && m.BlockedOn["inbox_id"] == id {
			m.State = MemberIdle
			m.Detail = ""
			m.BlockedOn = nil
			m.SinceSeq = e.Seq
		}
	}
	return nil
}

// Implementation note.
// Implementation note.
// Implementation note.
func applyCost(out *State, e Event, c Config, fx *[]Effect) {
	cost := e.Num("cost_usd")
	out.SpentUSD += cost
	out.TreeSpentUSD += cost
	if m := out.Member(e.Actor); m != nil {
		m.SpentUSD += cost
		if m.State == MemberThinking {
			m.State = MemberIdle
		}
	}

	if out.BudgetUSD <= 0 {
		return
	}
	if out.TreeSpentUSD >= out.BudgetUSD {
		*fx = append(*fx, Emit{Event: derived(out, e, BudgetExceeded, map[string]any{
			"tree_spent_usd": out.TreeSpentUSD,
			"budget_usd":     out.BudgetUSD,
		})})
		return
	}
	if !out.BudgetWarned && out.TreeSpentUSD >= out.BudgetUSD*c.BudgetWarnPct {
		out.BudgetWarned = true
		*fx = append(*fx, Emit{Event: derived(out, e, BudgetWarning, map[string]any{
			"tree_spent_usd": out.TreeSpentUSD,
			"budget_usd":     out.BudgetUSD,
			"pct":            c.BudgetWarnPct,
		})})
	}
}

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
func checkQuiescence(out *State, e Event, c Config, pending []Effect) []Effect {
	if out.Status != StatusRunning || out.QuiescentEmitted {
		return nil
	}
	if e.Type == RunQuiescent {
		return nil
	}
	// Implementation note.
	for _, f := range pending {
		switch f.(type) {
		case SpawnTurn, CallTool, SetTimer, AskHuman, Emit:
			return nil
		}
	}
	if out.ActiveTimer != "" || anyBusy(*out) || anyRunnable(*out) {
		return nil
	}
	for _, it := range out.Inbox {
		if !it.Replied {
			return nil
		}
	}

	// Implementation note.
	// Implementation note.
	diag := "nadie is working and nadie can empezar"
	var waiting []string
	for _, m := range out.Members {
		if m.State == MemberWaiting {
			waiting = append(waiting, m.Name+" waits "+m.Detail)
		}
	}
	if len(waiting) > 0 {
		diag = strings.Join(waiting, "; ")
	} else if st := c.StageAt(out.StageIndex); st != nil {
		// Implementation note.
		// Implementation note.
		// Implementation note.
		// Implementation note.
		// Implementation note.
		// Implementation note.
		var missing []string
		for _, m := range out.Members {
			if !m.Advisory && !m.Submitted && participates(c, m.Name, st.Name) {
				missing = append(missing, m.Name)
			}
		}
		diag = "stage " + st.Name + " advances with " + st.AdvanceWhen + " and not is meets"
		if len(missing) > 0 {
			diag += "; missing the submit of: " + strings.Join(missing, ", ")
		} else {
			diag += "; ya submitted all the that could: the rule is unsatisfiable with this blueprint"
		}
	}

	out.QuiescentEmitted = true
	return []Effect{Emit{Event: derived(out, e, RunQuiescent, map[string]any{
		"diagnosis": diag,
		"stage":     out.Stage,
	})}}
}

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
func wakeWatchers(out *State, e Event, c Config) []Effect {
	var fx []Effect
	for _, w := range c.Watchers {
		if !matchPattern(w.Pattern, string(e.Type)) {
			continue
		}
		if !w.IncludeSelf && w.Agent == e.Actor {
			continue
		}
		if e.Depth >= c.MaxDepth {
			continue
		}
		m := out.Member(w.Agent)
		if m == nil || m.State == MemberFailed {
			continue
		}

		switch w.Action {
		case "run_tool":
			fx = append(fx, CallTool{Agent: w.Agent, Tool: w.Tool, Args: e.Payload})
		case "notify":
			if m.Busy() || m.State == MemberWaiting {
				m.PendingCauses = append(m.PendingCauses, e.ID)
				continue
			}
			m.State = MemberIdle
			fx = append(fx, spawnFor(out, *m, c, []string{e.ID}, 0))
		default: // activate
			if m.Busy() || m.State == MemberWaiting {
				m.PendingCauses = append(m.PendingCauses, e.ID)
				continue
			}
			m.State = MemberIdle
			fx = append(fx, spawnFor(out, *m, c, []string{e.ID}, 0))
		}
	}
	return fx
}

// Implementation note.
// Implementation note.
// Implementation note.
func matchPattern(pattern, typ string) bool {
	if pattern == typ || pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, ".*") {
		return strings.HasPrefix(typ, strings.TrimSuffix(pattern, "*"))
	}
	return false
}

func spawnFor(out *State, m Member, c Config, causes []string, coalesced int) Effect {
	slice := 0.0
	if out.BudgetUSD > 0 {
		if rest := out.BudgetUSD - out.TreeSpentUSD; rest > 0 {
			slice = rest
		}
	}
	return SpawnTurn{
		Agent:       m.Name,
		Context:     buildContext(out, m, c, causes),
		CauseEvents: causes,
		BudgetSlice: slice,
		Coalesced:   coalesced,
	}
}

// Implementation note.
// Implementation note.
// Implementation note.
func buildContext(out *State, m Member, c Config, causes []string) ContextSpec {
	cs := c.Context
	cs.Identity = m.Name
	if m.Role != "" {
		cs.Identity = m.Name + " (" + m.Role + ")"
	}
	cs.Situation = []string{"run:" + out.RunID, "stage:" + out.Stage}
	cs.Cause = causes
	if cs.MaxTokens == 0 {
		cs.MaxTokens = 24000
	}
	if cs.OnOverflow == "" {
		cs.OnOverflow = "summarize"
	}
	return cs
}

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
func derived(out *State, cause Event, typ EventType, payload map[string]any) Event {
	corr := cause.CorrelationID
	if corr == "" {
		corr = cause.ID
	}
	return Event{
		Type:          typ,
		Scope:         "run:" + out.RunID,
		Source:        SourceRuntime,
		CorrelationID: corr,
		CausedBy:      []string{cause.ID},
		Depth:         cause.Depth + 1,
		Payload:       payload,
	}
}

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
func orderEffects(fx []Effect) []Effect {
	if len(fx) < 2 {
		return fx
	}
	sort.SliceStable(fx, func(i, j int) bool {
		return fx[i].Class() < fx[j].Class()
	})
	return fx
}

// Implementation note.
// Implementation note.
func isWatcherDispatched(t EventType) bool {
	switch t {
	case ResourceConflict, RunQuiescent, StageTimeout:
		return true
	}
	return false
}

func anyRunnable(s State) bool {
	for _, m := range s.Members {
		if m.Runnable() {
			return true
		}
	}
	return false
}

func anyBusy(s State) bool {
	for _, m := range s.Members {
		if m.Busy() {
			return true
		}
	}
	return false
}

func releaseLock(out *State, key string) {
	kept := out.Locks[:0]
	for _, l := range out.Locks {
		if l.Key != key {
			kept = append(kept, l)
		}
	}
	out.Locks = kept
}
