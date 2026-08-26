package kernel

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
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

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
func (s RunStatus) Terminal() bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusCancelled, StatusExpired:
		return true
	}
	return false
}

// Implementation note.
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

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
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

	// Implementation note.
	// Implementation note.
	// Implementation note.
	// Implementation note.
	PendingCauses []string `json:"pending_causes,omitempty"`
}

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
func (m Member) Runnable() bool { return m.State == MemberIdle }

// Implementation note.
func (m Member) Busy() bool { return m.State == MemberThinking || m.State == MemberTool }

// Implementation note.
type InboxItem struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Question  string `json:"question"`
	Agent     string `json:"agent,omitempty"`
	OnTimeout string `json:"on_timeout,omitempty"`
	Replied   bool   `json:"replied,omitempty"`
}

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
type Lock struct {
	Key    string `json:"key"`
	Holder string `json:"holder"`
}

// Implementation note.
// Implementation note.
// Implementation note.
type State struct {
	RunID  string    `json:"run_id"`
	Actor  string    `json:"actor"`
	Status RunStatus `json:"status"`
	Seq    int64     `json:"seq"`

	// Implementation note.
	// Implementation note.
	// Implementation note.
	// Implementation note.
	// Implementation note.
	BlueprintSHA string `json:"blueprint_sha,omitempty"`

	Stage      string `json:"stage,omitempty"`
	StageIndex int    `json:"stage_index"`

	Members []Member `json:"members,omitempty"`

	SpentUSD  float64 `json:"spent_usd"`
	BudgetUSD float64 `json:"budget_usd"`
	Turns     int     `json:"turns"`
	MaxTurns  int     `json:"max_turns,omitempty"`

	// Implementation note.
	// Implementation note.
	// Implementation note.
	TreeSpentUSD float64 `json:"tree_spent_usd"`
	ParentRunID  string  `json:"parent_run_id,omitempty"`
	SpawnDepth   int     `json:"spawn_depth"`

	Locks []Lock      `json:"locks,omitempty"`
	Inbox []InboxItem `json:"inbox,omitempty"`

	ActiveTimer string `json:"active_timer,omitempty"`
	Result      string `json:"result,omitempty"`

	// Implementation note.
	// Implementation note.
	// Implementation note.
	BudgetWarned     bool `json:"budget_warned,omitempty"`
	QuiescentEmitted bool `json:"quiescent_emitted,omitempty"`

	NextInboxID int `json:"next_inbox_id"`
}

// Implementation note.
func (s *State) Member(name string) *Member {
	for i := range s.Members {
		if s.Members[i].Name == name {
			return &s.Members[i]
		}
	}
	return nil
}

// Implementation note.
// Implementation note.
// Implementation note.
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
