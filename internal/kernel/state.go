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

	// PendingCauses are events that arrived while the member was busy. They are
	// neither lost nor do they open a new turn: they accumulate and are drained
	// together on the next turn. This IS `on_busy: queue`, and it is the same
	// machinery that implements follow-up.
	PendingCauses []string `json:"pending_causes,omitempty"`
}

// Runnable reports whether it is worth opening a turn for this member.
//
// EXPENSIVE SUBTLETY: MemberIdle only. The temptation is to include
// MemberSubmitted ("already answered, could answer again"), and that breaks
// quiescence detection in the worst way: in a staged run where everyone
// submitted but the advance rule is not met, the system looks eternally healthy
// (there are "runnable" members) when in fact it is stuck forever. That is
// exactly the case run.quiescent exists to catch.
func (m Member) Runnable() bool { return m.State == MemberIdle }

// Busy reports whether the member is spending money right now.
func (m Member) Busy() bool { return m.State == MemberThinking || m.State == MemberTool }

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

	ActiveTimer string `json:"active_timer,omitempty"`
	Result      string `json:"result,omitempty"`

	// BudgetWarned and QuiescentEmitted keep warnings from repeating. They live
	// in the state and not in the executor because the fold has to be
	// reproducible: if the warning depended on process memory, a replay would
	// emit different warnings.
	BudgetWarned     bool `json:"budget_warned,omitempty"`
	QuiescentEmitted bool `json:"quiescent_emitted,omitempty"`

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
	return out
}
