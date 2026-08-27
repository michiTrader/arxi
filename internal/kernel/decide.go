package kernel

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Decide is the only function that decides anything in iash.
//
//	Decide(State, Event, Config) -> (State', []Effect)
//
// Pure: same input, same output, always. It does not look at the clock, does not
// touch the network, does not write anything. Everything it wants to happen in
// the world it returns as an effect and somebody else executes it.
//
// This is what makes four features be the same feature:
//   - `run`     = fold + real executor
//   - `--sim`   = fold + fake executor
//   - `replay`  = fold over an old log, with no executor
//   - `run why` = read the State that came out of the fold
//
// If the reducer were not pure, these would be four different programs that have
// to be kept in sync by hand, and the third one would always be broken.
func Decide(s State, e Event, c Config) (State, []Effect) {
	out := s.Clone()
	out.Seq = e.Seq

	// An event arriving at a terminal run is recorded and ignored. It is not an
	// error: it happens all the time (a slow tool answering after the cancel)
	// and treating it as an error would fail perfectly valid replays.
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
		// A conflict does NOT fail the run on its own. It wakes whoever is
		// observing it; if nobody observes, it stays recorded and the run
		// continues until quiescence detects it. Failing here would let a
		// trivial merge conflict kill half an hour of work.
		fx = append(fx, wakeWatchers(&out, e, c)...)

	case BudgetWarning:
		out.BudgetWarned = true
	case BudgetExceeded:
		// Block and ask, do not kill. The work done up to here is worth real
		// money; the human decides whether to raise the budget or stop.
		out.Status = StatusBlocked
		fx = append(fx, AskHuman{
			Kind:      "budget",
			Question:  fmt.Sprintf("budget exhausted (%.4f of %.4f USD in the tree). raise or cancel?", out.TreeSpentUSD, out.BudgetUSD),
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
		// Quiescence wakes the coordinator. Only if NOBODY observes it does the
		// run fail, and it fails with the diagnosis inside so that `run why`
		// has something to tell.
		w := wakeWatchers(&out, e, c)
		fx = append(fx, w...)
		if len(w) == 0 {
			out.Status = StatusFailed
			out.Result = "quiescent run with no observer: " + e.Str("diagnosis")
		}

	case RunResult:
		out.Status = StatusSucceeded
		out.Result = e.Str("summary")
	}

	// Watchers see every event that is not derived from a watcher (so as not to
	// echo themselves). The cases that already called wakeWatchers above are
	// skipped so nobody gets woken twice.
	if !isWatcherDispatched(e.Type) && e.Source != SourceRuntime {
		fx = append(fx, wakeWatchers(&out, e, c)...)
	}

	// Quiescence is checked at the end of EVERY step. It is the detector of the
	// most frequent and most expensive failure mode of these systems: nobody is
	// busy, nobody is blocked on anything nameable, no timer is armed, and the
	// run simply sits staring at the ceiling forever.
	fx = append(fx, checkQuiescence(&out, e, c, fx)...)

	return out, orderEffects(fx)
}

// Fold rebuilds the state from the log. State = fold(Decide, State0, events).
// The snapshot is an optimization, never the truth.
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

	// StageIndex = -1 means "has not entered any stage yet". Starting at 0 would
	// make the first stage.entered look like a re-entry.
	out.StageIndex = -1

	out.Members = nil
	for _, mc := range c.Members {
		st := MemberIdle
		if mc.Advisory {
			// An advisory starts inactive: it gives its opinion when called, it
			// does not take a paid turn the moment the run starts.
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

// applyInjection unifies run.prompt, agent.steered and agent.notified because
// they are the same mechanism with different provenance: text arrives for
// somebody.
//
// And here is the key piece: if the recipient is busy, the text is NOT lost and
// does NOT open a new turn. It accumulates in PendingCauses and is drained when
// the current turn finishes. That IS `on_busy: queue`, and `queue` IS follow-up.
// Three features, one mechanism.
func applyInjection(out *State, e Event, c Config) []Effect {
	target := resolveSteerTarget(out, c, e.Str("to"))
	var fx []Effect

	for i := range out.Members {
		m := &out.Members[i]
		if target != "*" && m.Name != target {
			continue
		}
		// A broadcast talks to whoever is participating, not to whoever has not
		// been activated yet. An inactive advisory wakes up if you name it, not
		// for being on the list: otherwise every steer to the team pays for a
		// turn for every commentator nobody invoked.
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

// resolveSteerTarget translates the declared target into a concrete name.
// "coordinator" is not a special kind of agent: it is the first non-advisory, or
// whoever holds that role. A single namespace, with no parallel categories.
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
			"summary":     "all stages completed",
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

// quorumMet evaluates the advance rule. Advisory members NEVER count: that is
// the concrete consequence of the trait, not an exception hardwired here.
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

// applyStageTimeout: the default is to escalate, not to fail. A timeout almost
// never means "impossible", it means "something got stuck, take a look". And if
// nobody is looking, a human is asked before throwing the work in the trash.
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
				"summary": "last stage expired, advancing",
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
			Question:  "the stage " + out.Stage + " expired and nobody observes it. advance, extend or cancel?",
			OnTimeout: "fail",
		}}
	}
}

// applyTurnDone is where coalescing lives. If while the agent was thinking five
// reasons to talk to it again arrived, ONE turn is opened with the five causes,
// not five turns. The difference is literally 5x on the bill.
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

	// The structured payload is what keeps `run why` free of hard-coded cases:
	// it walks the graph reading these references.
	if raw, ok := e.Payload["blocked_ref"].(map[string]any); ok {
		m.BlockedOn = raw
	}
}

// applyToolDenied: policy=ask is not an error, it is a question. And the
// question is stored with a structured reference so that `run why` can name the
// exact command that unblocks it.
func applyToolDenied(out *State, e Event) []Effect {
	tool := e.Str("tool")
	if e.Str("policy") != "ask" {
		if m := out.Member(e.Actor); m != nil {
			m.State = MemberIdle
			m.Detail = "tool " + tool + " denied"
		}
		return nil
	}

	id := "inbox-" + strconv.Itoa(out.NextInboxID)
	out.NextInboxID++
	item := InboxItem{
		ID:        id,
		Kind:      "tool_approval",
		Question:  "approve " + tool + " for " + e.Actor + "?",
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
		out.Result = "nobody answered " + id
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

// applyCost attributes the spend to the member AND to the tree. The tree is what
// makes --budget of the root run mean something when there is nested spawn:
// without TreeSpentUSD, N levels of depth multiply the ceiling by N.
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

// checkQuiescence detects that the run went quiet.
//
// This is not in any competitor's specification and it is the failure mode that
// costs the most money and the most patience: the system does not fail, does not
// finish, does not advance. Things simply stop happening and the user finds out
// the next morning that they spent 40 dollars on nothing.
//
// Detection is conservative: if there is ANY reason to believe something is
// going to happen (a pending effect that will produce an event, somebody busy,
// somebody wakeable, an armed timer, an unanswered question), nothing is
// emitted.
func checkQuiescence(out *State, e Event, c Config, pending []Effect) []Effect {
	if out.Status != StatusRunning || out.QuiescentEmitted {
		return nil
	}
	if e.Type == RunQuiescent {
		return nil
	}
	// Any pending effect is going to generate an event: there is no silence yet.
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

	// Diagnosis: the concrete reason why it is quiet. Without this the event
	// would be a useless "something happened", and `run why` would have nothing
	// to tell.
	diag := "nobody is working and nobody can start"
	var waiting []string
	for _, m := range out.Members {
		if m.State == MemberWaiting {
			waiting = append(waiting, m.Name+" waits for "+m.Detail)
		}
	}
	if len(waiting) > 0 {
		diag = strings.Join(waiting, "; ")
	} else if st := c.StageAt(out.StageIndex); st != nil {
		// The diagnosis ALWAYS names the advance rule, even when nobody is
		// missing. That is precisely the hardest case to debug by eye: the rule
		// asks for three submissions and only two members exist that can
		// submit, so everybody "complied" and the rule still never holds. A
		// diagnosis that only said "nobody can start" would leave the user
		// staring at an apparently correct blueprint.
		var missing []string
		for _, m := range out.Members {
			if !m.Advisory && !m.Submitted && participates(c, m.Name, st.Name) {
				missing = append(missing, m.Name)
			}
		}
		diag = "stage " + st.Name + " advances with " + st.AdvanceWhen + " and it is not met"
		if len(missing) > 0 {
			diag += "; missing the submit of: " + strings.Join(missing, ", ")
		} else {
			diag += "; everyone who could has already submitted: the rule is unsatisfiable with this blueprint"
		}
	}

	out.QuiescentEmitted = true
	return []Effect{Emit{Event: derived(out, e, RunQuiescent, map[string]any{
		"diagnosis": diag,
		"stage":     out.Stage,
	})}}
}

// wakeWatchers evaluates the declared watchers.
//
// The two filters here (self-exclusion and depth limit) run BEFORE generating a
// single expensive effect. They are cheap and they prevent the class of bug that
// gets billed in dollars: a watcher on `agent.*` that reacts to its own events
// is an infinite loop with a credit card.
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

// matchPattern supports exact match and a single suffix wildcard (`stage.*`).
// There is no full glob on purpose: a pattern nobody can read at a glance is a
// pattern that is going to wake agents nobody expected.
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

// buildContext assembles the layers in order of decreasing stability:
// identity -> situation -> memory -> shared -> cause. That order exists so the
// provider's prefix cache hits on the layers that do not change between turns.
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

// derived builds a derived event. It inherits correlation_id (so the full causal
// thread can be followed) and increments depth, which is what makes the depth
// limit enforceable.
//
// Seq stays 0 on purpose: the reducer is not the single writer of the log, so
// assigning sequence numbers is not its job.
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

// orderEffects puts the control ones first, preserving the relative order
// within each class. SliceStable and not Slice: the order of the Emits among
// themselves is semantic (stage.advanced before stage.entered) and an unstable
// sort would break it intermittently, which is the worst way to break.
func orderEffects(fx []Effect) []Effect {
	if len(fx) < 2 {
		return fx
	}
	sort.SliceStable(fx, func(i, j int) bool {
		return fx[i].Class() < fx[j].Class()
	})
	return fx
}

// isWatcherDispatched marks the types that already called wakeWatchers inside
// the switch, so the same watcher is not woken twice for the same event.
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
