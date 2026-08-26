package kernel

// Implementation note.
// Implementation note.
type Config struct {
	Blueprint  string         `json:"blueprint,omitempty"`
	Stages     []StageConfig  `json:"stages,omitempty"`
	Members    []MemberConfig `json:"members,omitempty"`
	Watchers   []Watcher      `json:"watchers,omitempty"`
	Inter      Interaction    `json:"interaction"`
	Workspace  string         `json:"workspace,omitempty"`
	Context    ContextSpec    `json:"context_policy"`
	ResultFrom string         `json:"result_from,omitempty"`

	BudgetWarnPct float64 `json:"budget_warn_pct,omitempty"`
	MaxDepth      int     `json:"max_depth,omitempty"`
}

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
type Interaction struct {
	SteerTarget string `json:"steer_target,omitempty"`
}

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
type StageConfig struct {
	Name        string `json:"name"`
	AdvanceWhen string `json:"advance_when,omitempty"`
	TimeoutMs   int64  `json:"timeout_ms,omitempty"`
	OnTimeout   string `json:"on_timeout,omitempty"`
	Workspace   string `json:"workspace,omitempty"`
	OnConflict  string `json:"on_conflict,omitempty"`
}

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
type MemberConfig struct {
	Name       string   `json:"name"`
	Role       string   `json:"role,omitempty"`
	Advisory   bool     `json:"advisory,omitempty"`
	Tools      []string `json:"tools,omitempty"`
	Activation string   `json:"activation,omitempty"`
	Stages     []string `json:"stages,omitempty"`
}

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
type Watcher struct {
	Agent       string `json:"agent"`
	Pattern     string `json:"pattern"`
	Action      string `json:"action,omitempty"`
	Tool        string `json:"tool,omitempty"`
	IncludeSelf bool   `json:"include_self,omitempty"`
}

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
type ContextSpec struct {
	Identity   string   `json:"identity,omitempty"`
	Situation  []string `json:"situation,omitempty"`
	Memory     string   `json:"memory,omitempty"`
	Shared     []string `json:"shared,omitempty"`
	Cause      []string `json:"cause,omitempty"`
	MaxTokens  int      `json:"max_tokens,omitempty"`
	OnOverflow string   `json:"on_overflow,omitempty"`
}

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
func (c Config) ResolveDefaults() Config {
	out := c

	if out.Inter.SteerTarget == "" {
		out.Inter.SteerTarget = "coordinator"
	}
	if out.BudgetWarnPct == 0 {
		out.BudgetWarnPct = 0.8
	}
	if out.MaxDepth == 0 {
		out.MaxDepth = 12
	}
	if out.ResultFrom == "" {
		out.ResultFrom = "last_submit"
	}

	// Implementation note.
	// Implementation note.
	// Implementation note.
	if out.Workspace == "" {
		out.Workspace = "none"
		for _, m := range out.Members {
			for _, t := range m.Tools {
				if t == "write" || t == "bash" || t == "edit" {
					out.Workspace = "worktree"
				}
			}
		}
	}

	if out.Stages != nil {
		out.Stages = append([]StageConfig(nil), out.Stages...)
		for i := range out.Stages {
			if out.Stages[i].AdvanceWhen == "" {
				out.Stages[i].AdvanceWhen = "all"
			}
			if out.Stages[i].OnTimeout == "" {
				out.Stages[i].OnTimeout = "escalate"
			}
		}
	}
	if out.Members != nil {
		out.Members = append([]MemberConfig(nil), out.Members...)
		for i := range out.Members {
			if out.Members[i].Activation == "" {
				out.Members[i].Activation = "coalesce"
			}
		}
	}
	return out
}

// Implementation note.
func (c Config) StageAt(i int) *StageConfig {
	if i < 0 || i >= len(c.Stages) {
		return nil
	}
	return &c.Stages[i]
}

// Implementation note.
func (c Config) MemberCfg(name string) *MemberConfig {
	for i := range c.Members {
		if c.Members[i].Name == name {
			return &c.Members[i]
		}
	}
	return nil
}
