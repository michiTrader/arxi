package kernel

// RunStatus is the status of the whole run.
//
// Note that "quiescent" is NOT here. A system having gone silent is not a
// terminal state, it is an event (run.quiescent) that wakes the coordinator with
// a diagnosis. Making it a terminal state was the obvious temptation and would
// be the worst possible design error: the most common failure mode of these
// systems is going mute, and the right answer is to intervene, not to die.
type RunStatus string

const (
	StatusQueued    RunStatus = "queued"
	StatusRunning   RunStatus = "running"
	StatusBlocked   RunStatus = "blocked"
	StatusPaused    RunStatus = "paused"
	StatusSucceeded RunStatus = "succeeded"
	StatusFailed    RunStatus = "failed"
	StatusCancelled RunStatus = "cancelled"
	StatusExpired   RunStatus = "expired"
)

// Terminal reports whether the run no longer accepts events that change its
// state. An event arriving late to a terminal run is not an error: it is
// recorded and ignored. It happens all the time (a slow tool answering after a
// cancel) and treating it as an error would fail perfectly valid replays.
func (s RunStatus) Terminal() bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusCancelled, StatusExpired:
		return true
	}
	return false
}

// MemberState is the state of one member inside the run.
type MemberState string

const (
	MemberIdle      MemberState = "idle"
	MemberThinking  MemberState = "thinking"
	MemberTool      MemberState = "tool"
	MemberSubmitted MemberState = "submitted"
	MemberWaiting   MemberState = "waiting"
	MemberInactive  MemberState = "inactive"
	MemberFailed    MemberState = "failed"
)

// Member is a participant in the run.
//
// Advisory is a generic trait of the role, not a feature flag. It replaces the
// --counts-toward-advance of the first draft: instead of a boolean that only
// means anything to stages, an advisory member is "someone who weighs in but
// does not decide", and each blueprint reads that property as appropriate (does
// not count toward quorum, does not block advancement, may keep talking).
type Member struct {
	Name      string         `json:"name"`
	Role      string         `json:"role,omitempty"`
	Advisory  bool           `json:"advisory,omitempty"`
	State     MemberState    `json:"state"`
	Detail    string         `json:"detail,omitempty"`
	BlockedOn map[string]any `json:"blocked_on,omitempty"`
	SinceSeq  int64          `json:"since_seq"`
	SpentUSD  float64        `json:"spent_usd"`
	Turns     int            `json:"turns"`
	Submitted bool           `json:"submitted,omitempty"`

	// TurnOpen records that this member has been activated and its turn has not
	// finished yet, independently of the state it is currently displaying.
	//
	// It exists because State alone cannot answer "is a turn still running".
	// A member that submits mid-turn moves to MemberSubmitted, which is neither
	// Busy nor Runnable, while agent.turn_done is still on its way in the same
	// batch of events. checkQuiescence folding that submit therefore saw nobody
	// busy, nobody runnable and nothing armed, and diagnosed a run that was
	// working normally as silent forever. Since a watcher on run.quiescent opens a
	// paid turn, that false positive cost money as well as trust.
	TurnOpen bool `json:"turn_open,omitempty"`

	// PendingCauses are events that arrived while the member was busy. They are
	// neither lost nor do they open a new turn: they accumulate and are drained
	// together on the next turn. This IS `on_busy: queue`, and it is the same
	// machinery that implements follow-up.
	PendingCauses []string `json:"pending_causes,omitempty"`
}

// Runnable reports whether a turn for this member is actually going to happen.
//
// EXPENSIVE SUBTLETY: MemberSubmitted is excluded. The temptation is to include
// it ("already answered, could answer again"), and that breaks quiescence
// detection in the worst way: in a staged run where everyone submitted but the
// advance rule is not met, the system looks eternally healthy (there are
// "runnable" members) when in fact it is stuck forever. That is exactly the case
// run.quiescent exists to catch.
//
// THE SAME SUBTLETY, FROM THE OTHER SIDE: being idle is not enough either. The
// reducer never opens a turn merely because somebody is idle; every turn is
// opened by a CAUSE (a stage was entered, a watcher matched, a human injected a
// prompt, a queued cause drained). So an idle member with no pending cause is not
// waiting to work, it has finished working, and reporting it as runnable makes
// checkQuiescence conclude that something is still going to happen when nothing
// is.
//
// That hid the exact failure ADR-0004 is about, in a case no earlier test could
// reach: a member whose turn ends WITHOUT submitting returns to idle, so a stage
// whose rule needs everybody sat silent forever and no diagnosis was emitted at
// all. The run loop could only report it as "idle", which is indistinguishable
// from a run legitimately waiting on a human.
//
// PendingCauses is the honest test of "a turn is coming", because that is the
// only thing the reducer drains into a new turn on its own (see applyTurnDone).
func (m Member) Runnable() bool {
	return m.State == MemberIdle && len(m.PendingCauses) > 0
}

// Busy reports whether the member is spending money right now.
//
// TurnOpen is part of the answer and not a redundant check. A member that
// submits during its turn displays MemberSubmitted, which is deliberately
// neither Busy nor Runnable, yet its turn is still running and its agent.turn_done
// is still to come. Without TurnOpen, quiescence fired in the middle of a
// perfectly healthy turn.
//
// MemberWaiting overrides it, and that exclusion is the point of the whole
// mechanism rather than an exception to it. A member blocked on a human has an
// open turn and is spending nothing: the question could sit in the inbox for a
// day. Counting it as busy would suppress precisely the diagnosis the user needs
// ("waiting for approval of inbox-1") and make `run why` contradict itself,
// reporting the member as blocked and then as working two lines later.
//
// MemberFailed is excluded for the same reason: a turn that died never emits
// agent.turn_done, so its TurnOpen is never cleared, and a failed member would
// look eternally busy and mask every subsequent silence.
func (m Member) Busy() bool {
	if m.State == MemberWaiting || m.State == MemberFailed {
		return false
	}
	return m.TurnOpen || m.State == MemberThinking || m.State == MemberTool
}

// InboxItem is a pending question for a human.
type InboxItem struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Question  string `json:"question"`
	Agent     string `json:"agent,omitempty"`
	OnTimeout string `json:"on_timeout,omitempty"`
	Replied   bool   `json:"replied,omitempty"`
}

// Lock is a cooperative lock over a key. It is cooperative and not
// filesystem-level on purpose: it coordinates intent between agents. Real
// filesystem isolation comes from the workspace (see Config.Workspace), because
// a lock in the KV store does not stop two processes from writing the same file.
type Lock struct {
	Key    string `json:"key"`
	Holder string `json:"holder"`
}

// State is the complete state of the run, derived from the log. It is never the
// source of truth: State = fold(Decide, State0, events). If the snapshot and the
// log disagree, the log wins.
type State struct {
	RunID  string    `json:"run_id"`
	Actor  string    `json:"actor"`
	Status RunStatus `json:"status"`
	Seq    int64     `json:"seq"`

	// BlueprintSHA pins the resolved copy of the blueprint this run uses. It
	// closes IA A's second gap: the reducer never reads the live file, it reads
	// the frozen copy at runs/<id>/blueprint.snapshot.yaml. Without this, a
	// replay of last week would use today's config and produce a different
	// result, which is the same as having no replay at all.
	BlueprintSHA string `json:"blueprint_sha,omitempty"`

	Stage      string `json:"stage,omitempty"`
	StageIndex int    `json:"stage_index"`

	Members []Member `json:"members,omitempty"`

	SpentUSD  float64 `json:"spent_usd"`
	BudgetUSD float64 `json:"budget_usd"`
	Turns     int     `json:"turns"`
	MaxTurns  int     `json:"max_turns,omitempty"`

	// TreeSpentUSD is the spend of the whole subtree. The budget is a sub-pool:
	// a child consumes from its parent. Without this, N levels of spawn multiply
	// the cost by N and the root run's --budget is decorative.
	TreeSpentUSD float64 `json:"tree_spent_usd"`
	ParentRunID  string  `json:"parent_run_id,omitempty"`
	SpawnDepth   int     `json:"spawn_depth"`

	Locks []Lock      `json:"locks,omitempty"`
	Inbox []InboxItem `json:"inbox,omitempty"`

	// KV is the run's shared key/value store: what one member wants another to
	// know without paying for a turn to say it.
	//
	// A map, and not the []Something every other collection here is. The others
	// are ordered histories -- Members has slots, Inbox has questions in the
	// order they were asked -- and this is a lookup where the last write is the
	// whole answer. A slice would scan linearly per read and, worse, would let
	// two entries share a key, which is a state `state get` cannot answer from.
	//
	// The values are strings because the surface declares the parameter as one,
	// and that is worth keeping rather than widening to `any`: a map[string]any
	// round-trips through JSON as float64, so a key set in Go and the same key
	// after a fold from disk would not compare equal, and replay fidelity is the
	// property this whole design exists for.
	KV map[string]string `json:"kv,omitempty"`

	ActiveTimer string `json:"active_timer,omitempty"`
	Result      string `json:"result,omitempty"`

	// BudgetWarned and QuiescentEmitted keep warnings from repeating. They live
	// in the state and not in the executor because the fold has to be
	// reproducible: if the warning depended on process memory, a replay would
	// emit different warnings.
	BudgetWarned     bool `json:"budget_warned,omitempty"`
	QuiescentEmitted bool `json:"quiescent_emitted,omitempty"`

	// BudgetBlocked records that the exhausted budget was already reported and
	// asked about. Same shape as BudgetWarned and for the same reason: being over
	// the ceiling stays true for every later cost event, so without a memory of
	// having responded, each one emits another budget.exceeded and asks another
	// human. That is not a cosmetic duplicate. Every copy of the question is a
	// separate inbox item with OnTimeout "fail", so one unanswered copy can fail
	// a run a human already agreed to pay for, and the person answering cannot
	// tell which of the four identical questions is the live one.
	//
	// It is cleared in applyCost as soon as spend is below the ceiling again,
	// which is what makes a raised budget enforceable: the next breach is a new
	// fact about a new ceiling and has to be asked about again.
	BudgetBlocked bool `json:"budget_blocked,omitempty"`

	// StageResolved records that the current stage already met its advance rule
	// and acted on it. Same purpose as the two flags above and for the same
	// reason: the advance rule STAYS met once satisfied, so without a memory of
	// having responded, every further submit to the stage advances it again. It is
	// cleared on stage entry, so it scopes to one stage.
	StageResolved bool `json:"stage_resolved,omitempty"`

	NextInboxID int `json:"next_inbox_id"`
}

// Member looks up a member by name and returns a pointer so it can be modified.
func (s *State) Member(name string) *Member {
	for i := range s.Members {
		if s.Members[i].Name == name {
			return &s.Members[i]
		}
	}
	return nil
}

// InboxItem looks up a question by id and returns a pointer so it can be
// modified. Nil means no such question, which is how the InboxCreated case tells
// a new item from the confirmation of one already recorded.
func (s *State) InboxItem(id string) *InboxItem {
	if id == "" {
		return nil
	}
	for i := range s.Inbox {
		if s.Inbox[i].ID == id {
			return &s.Inbox[i]
		}
	}
	return nil
}

// Clone makes a deep copy. Decide clones before touching anything: the signature
// says (State, Event) -> State, and if it mutated its input the fold would stop
// being reproducible and the "does not mutate input" tests would exist for
// nothing.
func (s State) Clone() State {
	out := s
	if s.Members != nil {
		out.Members = make([]Member, len(s.Members))
		for i, m := range s.Members {
			out.Members[i] = m
			if m.BlockedOn != nil {
				out.Members[i].BlockedOn = make(map[string]any, len(m.BlockedOn))
				for k, v := range m.BlockedOn {
					out.Members[i].BlockedOn[k] = v
				}
			}
			if m.PendingCauses != nil {
				out.Members[i].PendingCauses = append([]string(nil), m.PendingCauses...)
			}
		}
	}
	if s.Locks != nil {
		out.Locks = append([]Lock(nil), s.Locks...)
	}
	if s.Inbox != nil {
		out.Inbox = append([]InboxItem(nil), s.Inbox...)
	}
	// The second map in this state, after Member.BlockedOn, and it needs the same
	// treatment for a reason the slices above hide: `out := s` copies a slice
	// HEADER, so appending to out.Locks leaves s.Locks alone, but it copies a map
	// REFERENCE, so writing out.KV[k] writes s.KV[k] too. Without this arm the
	// StateSet arm would mutate the state Decide was handed, the fold would stop
	// being reproducible, and the tests that assert Decide does not touch its
	// input would be checking the one field that no longer holds.
	if s.KV != nil {
		out.KV = make(map[string]string, len(s.KV))
		for k, v := range s.KV {
			out.KV[k] = v
		}
	}
	return out
}
