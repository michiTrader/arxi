package kernel

// Config is the already-resolved blueprint: the frozen copy the run pinned at
// startup (see State.BlueprintSHA). The reducer treats it as immutable.
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

// Interaction now only holds SteerTarget.
//
// The turn_source field from the first draft is RETIRED (ADR-0006). The idea was
// to declare who may open turns in order to avoid races, and AI B's correction
// is that this solves nothing: the real race is "two writers modify the same
// state" and it is solved with CAS on `seq` (if_seq + on_busy), not with a
// declarative policy about who is allowed to speak.
type Interaction struct {
	SteerTarget string `json:"steer_target,omitempty"`
}

// StageConfig is a stage of the blueprint.
//
// OnTimeout defaults to "escalate", not "fail". A stage timeout almost never
// means "the work is impossible", it means "something got stuck and somebody
// has to look at it". Failing by default trains the user to set absurdly long
// timeouts, which is worse than having none.
type StageConfig struct {
	Name        string `json:"name"`
	AdvanceWhen string `json:"advance_when,omitempty"`
	TimeoutMs   int64  `json:"timeout_ms,omitempty"`
	OnTimeout   string `json:"on_timeout,omitempty"`
	Workspace   string `json:"workspace,omitempty"`
	OnConflict  string `json:"on_conflict,omitempty"`
}

// MemberConfig declares a participant.
//
// Activation defaults to "coalesce": if five reasons to wake the same agent
// arrive, one turn is opened carrying all five. The alternative ("one event, one
// turn") multiplies the bill by five in exchange for nothing.
// Model names the model this member thinks with, in the spelling `model list`
// prints: either a bare id ("claude-sonnet-4-6") or one qualified by provider
// ("anthropic/claude-sonnet-4-6"). §20.2 shows it on `agent create --model` and
// in `agent show`, so it is a promise already made.
//
// It is a plain string and the kernel never interprets it. Resolving a ref to an
// endpoint and a credential is a question about THIS MACHINE -- which providers
// are registered, which models are enabled -- and the reducer must stay a pure
// function of (State, Event, Config). Were the resolution to happen here, the
// same log would fold to different states on two machines, and replay is the one
// property the whole design rests on.
//
// Empty means "the run decides", which is what makes the field additive: every
// blueprint written before it existed still loads, and a single-model setup
// never has to name the model twice.
type MemberConfig struct {
	Name       string   `json:"name"`
	Role       string   `json:"role,omitempty"`
	Model      string   `json:"model,omitempty"`
	Advisory   bool     `json:"advisory,omitempty"`
	Tools      []string `json:"tools,omitempty"`
	Activation string   `json:"activation,omitempty"`
	Stages     []string `json:"stages,omitempty"`
}

// Watcher is a declared reaction to an event pattern.
//
// IncludeSelf defaults to false, and that is a safety decision, not a
// convenience one: a watcher on `agent.*` that wakes up on its own events is an
// infinite loop billed in dollars. Self-exclusion and the depth limit are the
// two cheap filters that run BEFORE spending a single token.
type Watcher struct {
	Agent       string `json:"agent"`
	Pattern     string `json:"pattern"`
	Action      string `json:"action,omitempty"`
	Tool        string `json:"tool,omitempty"`
	IncludeSelf bool   `json:"include_self,omitempty"`
}

// ContextSpec describes what context gets assembled for a turn.
//
// The ORDER of the layers matters and is not alphabetical: identity -> situation
// -> memory -> shared -> cause. It goes from most stable to most volatile so
// that the provider's prefix cache hits on the leading layers. Inverting the
// order works exactly the same and costs several times more.
type ContextSpec struct {
	Identity   string   `json:"identity,omitempty"`
	Situation  []string `json:"situation,omitempty"`
	Memory     string   `json:"memory,omitempty"`
	Shared     []string `json:"shared,omitempty"`
	Cause      []string `json:"cause,omitempty"`
	MaxTokens  int      `json:"max_tokens,omitempty"`
	OnOverflow string   `json:"on_overflow,omitempty"`
}

// ResolveDefaults fills in the default values. It is a pure function over the
// Config and runs once when the blueprint is pinned, not on every Decide: if the
// defaults were applied at each step, changing a default in a new version of the
// binary would change the result of an old replay.
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

	// THE FATAL HOLE: if any member can write files and they share a single
	// directory, two agents are going to overwrite each other and the KV store
	// lock will not prevent it. The safe default is worktree, not shared.
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

// StageAt returns the stage at the given index, or nil if out of range.
func (c Config) StageAt(i int) *StageConfig {
	if i < 0 || i >= len(c.Stages) {
		return nil
	}
	return &c.Stages[i]
}

// MemberCfg looks up a member declaration by name.
func (c Config) MemberCfg(name string) *MemberConfig {
	for i := range c.Members {
		if c.Members[i].Name == name {
			return &c.Members[i]
		}
	}
	return nil
}
