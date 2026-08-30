package blueprint

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/michiTrader/arxi/internal/kernel"
)

// Blueprint is a loaded blueprint: the raw bytes, their digest, and the config
// the reducer will see.
//
// Raw is kept because ADR-0002 requires the run to freeze the blueprint at
// start: the reducer never reads the live file, it reads the copy written to
// runs/<id>/blueprint.snapshot.yaml. Re-serializing the parsed config instead
// of keeping the original bytes would make the snapshot a lossy paraphrase and
// break the digest, and then a replay would be reproducing something the user
// never wrote.
type Blueprint struct {
	Name string
	Raw  []byte
	SHA  string

	// Config is the config with defaults ALREADY resolved. Resolving here and
	// not on each Decide is what ADR-0002 requires: if defaults were applied
	// per step, shipping a binary with a different default would silently
	// change the outcome of replaying an old log.
	Config kernel.Config
}

// ValidationError collects every problem in the file at once.
//
// It reports all of them rather than the first, because a blueprint is edited
// by hand and fixing one typo per run is how people give up on a validator and
// start guessing.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string {
	if len(e.Problems) == 1 {
		return e.Problems[0]
	}
	return fmt.Sprintf("%d problems:\n  - %s", len(e.Problems), strings.Join(e.Problems, "\n  - "))
}

// LoadFile reads and validates a blueprint from disk.
func LoadFile(path string) (*Blueprint, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	bp, err := Load(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return bp, nil
}

// Load parses, validates and resolves a blueprint held in memory.
func Load(raw []byte) (*Blueprint, error) {
	doc, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	root, ok := doc.(map[string]any)
	if !ok {
		return nil, &ValidationError{Problems: []string{
			"the blueprint must be a mapping of keys at the top level (name:, members:, stages:)"}}
	}

	v := &validator{}
	cfg := v.config(root)
	if len(v.problems) > 0 {
		sort.Strings(v.problems)
		return nil, &ValidationError{Problems: v.problems}
	}

	sum := sha256.Sum256(raw)
	name, _ := root["name"].(string)
	return &Blueprint{
		Name:   name,
		Raw:    append([]byte(nil), raw...),
		SHA:    hex.EncodeToString(sum[:]),
		Config: cfg.ResolveDefaults(),
	}, nil
}

// validator accumulates problems instead of returning on the first one.
type validator struct{ problems []string }

func (v *validator) errf(format string, a ...any) {
	v.problems = append(v.problems, fmt.Sprintf(format, a...))
}

// known checks for unknown keys, and this is the whole reason the parser
// returns map[string]any instead of decoding into a struct directly.
//
// A misspelled `advance_when` silently dropped is the most expensive kind of
// typo in this system: the stage falls back to the default rule, the run either
// advances when it should not or hangs when it should not, and the file on
// screen says something that never took effect. Suggesting the nearest known
// key turns a hunt into a fix.
func (v *validator) known(where string, m map[string]any, allowed ...string) {
	for k := range m {
		if contains(allowed, k) {
			continue
		}
		if near := nearest(k, allowed); near != "" {
			v.errf("%s: unknown key %q, did you mean %q?", where, k, near)
		} else {
			v.errf("%s: unknown key %q (valid: %s)", where, k, strings.Join(allowed, ", "))
		}
	}
}

func (v *validator) config(root map[string]any) kernel.Config {
	v.known("blueprint", root,
		"name", "members", "stages", "watchers", "interaction",
		"workspace", "context_policy", "result_from", "budget_warn_pct", "max_depth")

	var cfg kernel.Config
	cfg.Blueprint = v.str("blueprint", root, "name")
	cfg.Workspace = v.enum("blueprint", root, "workspace", "none", "shared", "worktree")
	cfg.ResultFrom = v.str("blueprint", root, "result_from")

	if raw, ok := root["budget_warn_pct"]; ok && raw != nil {
		f, ok := toFloat(raw)
		switch {
		case !ok:
			v.errf("blueprint: budget_warn_pct must be a number between 0 and 1, found %v", raw)
		case f <= 0 || f > 1:
			// A percentage written as 80 instead of 0.8 would put the warning
			// threshold above any budget, so the warning never fires and the
			// user discovers the overspend from the bill.
			v.errf("blueprint: budget_warn_pct = %v; it is a fraction between 0 and 1 (0.8 = warn at 80%%)", raw)
		default:
			cfg.BudgetWarnPct = f
		}
	}
	if raw, ok := root["max_depth"]; ok && raw != nil {
		if n, ok := raw.(int64); !ok || n < 1 {
			v.errf("blueprint: max_depth must be an integer of at least 1, found %v", raw)
		} else {
			cfg.MaxDepth = int(n)
		}
	}

	cfg.Members = v.members(root["members"])
	cfg.Stages = v.stages(root["stages"], cfg.Members)
	cfg.Watchers = v.watchers(root["watchers"], cfg.Members)
	cfg.Inter = v.interaction(root["interaction"])
	cfg.Context = v.context(root["context_policy"])
	return cfg
}

func (v *validator) members(raw any) []kernel.MemberConfig {
	if raw == nil {
		// Not an error here: `blueprint validate` on a fragment is legitimate.
		// A run with no members is caught when the run starts, where there is a
		// budget and a prompt to complain about.
		return nil
	}
	items, ok := raw.([]any)
	if !ok {
		v.errf("members: must be a list, found %T", raw)
		return nil
	}

	var out []kernel.MemberConfig
	seen := map[string]bool{}
	for i, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			v.errf("members[%d]: each member is a mapping with at least a name", i)
			continue
		}
		where := fmt.Sprintf("members[%d]", i)
		v.known(where, m, "name", "role", "advisory", "tools", "activation", "stages")

		mc := kernel.MemberConfig{
			Name:       v.str(where, m, "name"),
			Role:       v.str(where, m, "role"),
			Advisory:   v.bool(where, m, "advisory"),
			Tools:      v.strList(where, m, "tools"),
			Activation: v.enum(where, m, "activation", "coalesce", "queue", "steer", "reject"),
			Stages:     v.strList(where, m, "stages"),
		}
		if mc.Name == "" {
			v.errf("%s: a member with no name cannot be addressed by `run steer` or named in a diagnosis", where)
		} else if seen[mc.Name] {
			// Two members with one name makes State.Member() return the first
			// every time, so the second silently never works and never shows up
			// as blocked either.
			v.errf("%s: duplicate member %q; names address members, so two cannot share one", where, mc.Name)
		} else {
			seen[mc.Name] = true
		}
		out = append(out, mc)
	}
	return out
}

func (v *validator) stages(raw any, members []kernel.MemberConfig) []kernel.StageConfig {
	if raw == nil {
		return nil
	}
	items, ok := raw.([]any)
	if !ok {
		v.errf("stages: must be a list, found %T", raw)
		return nil
	}

	var out []kernel.StageConfig
	seen := map[string]bool{}
	for i, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			v.errf("stages[%d]: each stage is a mapping with at least a name", i)
			continue
		}
		where := fmt.Sprintf("stages[%d]", i)
		v.known(where, m, "name", "advance_when", "timeout_ms", "on_timeout", "workspace", "on_conflict")

		sc := kernel.StageConfig{
			Name:       v.str(where, m, "name"),
			OnTimeout:  v.enum(where, m, "on_timeout", "escalate", "advance", "fail", "ask"),
			Workspace:  v.enum(where, m, "workspace", "none", "shared", "worktree"),
			OnConflict: v.enum(where, m, "on_conflict", "queue", "steer", "reject"),
		}
		if sc.Name == "" {
			v.errf("%s: a stage with no name cannot be reported by `run why`", where)
		} else if seen[sc.Name] {
			v.errf("%s: duplicate stage %q; `run why` names the stage, so two cannot share one", where, sc.Name)
		} else {
			seen[sc.Name] = true
		}

		if raw, ok := m["timeout_ms"]; ok && raw != nil {
			if n, ok := raw.(int64); !ok || n <= 0 {
				v.errf("%s: timeout_ms must be a positive integer in milliseconds, found %v", where, raw)
			} else {
				sc.TimeoutMs = n
			}
		}
		sc.AdvanceWhen = v.advanceWhen(where, m, len(members))
		out = append(out, sc)
	}
	return out
}

// advanceWhen validates the advance rule, including the quorum arithmetic.
//
// This is the highest-value check in the file. `quorum:5` on a team of three is
// the exact failure ADR-0004 exists for: the run does not fail, it goes silent,
// and the diagnosis has to explain that the rule can never be satisfied. It is
// far cheaper to refuse the blueprint than to let the user pay for turns that
// were never going to be enough.
func (v *validator) advanceWhen(where string, m map[string]any, memberCount int) string {
	raw, ok := m["advance_when"]
	if !ok || raw == nil {
		return ""
	}
	s, ok := raw.(string)
	if !ok {
		v.errf("%s: advance_when must be text (all, any, quorum:N), found %v", where, raw)
		return ""
	}
	switch s {
	case "all", "any":
		return s
	}
	if n, ok := strings.CutPrefix(s, "quorum:"); ok {
		k, err := strconv.Atoi(n)
		if err != nil || k < 1 {
			v.errf("%s: advance_when = %q; the quorum is a whole number of at least 1, as in quorum:2", where, s)
			return ""
		}
		if memberCount > 0 && k > memberCount {
			v.errf("%s: advance_when = %q but the blueprint declares %d members; "+
				"the rule can never be satisfied and the run would go quiescent instead of failing",
				where, s, memberCount)
			return ""
		}
		return s
	}
	v.errf("%s: advance_when = %q is not a rule; valid ones are all, any, quorum:N", where, s)
	return ""
}

func (v *validator) watchers(raw any, members []kernel.MemberConfig) []kernel.Watcher {
	if raw == nil {
		return nil
	}
	items, ok := raw.([]any)
	if !ok {
		v.errf("watchers: must be a list, found %T", raw)
		return nil
	}

	var out []kernel.Watcher
	for i, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			v.errf("watchers[%d]: each watcher is a mapping with an agent and a pattern", i)
			continue
		}
		where := fmt.Sprintf("watchers[%d]", i)
		v.known(where, m, "agent", "pattern", "action", "tool", "include_self")

		w := kernel.Watcher{
			Agent:       v.str(where, m, "agent"),
			Pattern:     v.str(where, m, "pattern"),
			Action:      v.str(where, m, "action"),
			Tool:        v.str(where, m, "tool"),
			IncludeSelf: v.bool(where, m, "include_self"),
		}
		if w.Agent == "" {
			v.errf("%s: a watcher with no agent has nobody to wake", where)
		} else if len(members) > 0 && !hasMember(members, w.Agent) {
			// A watcher on a member that does not exist never fires, and
			// nothing reports it: the user sees a rule in the file and a
			// reaction that never happens.
			v.errf("%s: agent %q is not declared in members; the watcher would never fire", where, w.Agent)
		}
		if w.Pattern == "" {
			v.errf("%s: a watcher with no pattern matches nothing", where)
		} else {
			v.pattern(where, w.Pattern)
		}
		if w.IncludeSelf {
			// Not an error, but it is the one setting in the file that can bill
			// an unbounded amount, so it does not get to be silent.
			v.errf("%s: include_self is true on pattern %q; a watcher that reacts to its own "+
				"events is an infinite loop with a credit card. Remove it, or narrow the pattern "+
				"so the agent cannot match itself", where, w.Pattern)
		}
		out = append(out, w)
	}
	return out
}

// pattern checks the shape the reducer's matchPattern actually supports: exact
// match or one trailing `*`. Any other glob would parse here and match nothing
// there, which looks like the watcher is broken rather than the pattern.
func (v *validator) pattern(where, p string) {
	if i := strings.IndexByte(p, '*'); i >= 0 && i != len(p)-1 {
		v.errf("%s: pattern %q; only a single trailing wildcard is supported (stage.*), "+
			"because a pattern nobody can read at a glance is a bug waiting to happen", where, p)
		return
	}
	if strings.HasSuffix(p, "*") && !strings.HasSuffix(p, ".*") {
		v.errf("%s: pattern %q; the wildcard covers a whole segment, so write it as %q",
			where, p, strings.TrimSuffix(p, "*")+".*")
	}
}

func (v *validator) interaction(raw any) kernel.Interaction {
	if raw == nil {
		return kernel.Interaction{}
	}
	m, ok := raw.(map[string]any)
	if !ok {
		v.errf("interaction: must be a mapping, found %T", raw)
		return kernel.Interaction{}
	}
	// turn_source was withdrawn by ADR-0006, and someone carrying a blueprint
	// over from the first draft deserves to be told why rather than have the
	// key ignored.
	if _, ok := m["turn_source"]; ok {
		v.errf("interaction: turn_source was withdrawn (ADR-0006). The race it tried to " +
			"prevent is solved by CAS on `seq` (--if-seq plus on_busy), not by declaring who may speak")
	}
	v.known("interaction", m, "steer_target")
	return kernel.Interaction{SteerTarget: v.str("interaction", m, "steer_target")}
}

func (v *validator) context(raw any) kernel.ContextSpec {
	if raw == nil {
		return kernel.ContextSpec{}
	}
	m, ok := raw.(map[string]any)
	if !ok {
		v.errf("context_policy: must be a mapping, found %T", raw)
		return kernel.ContextSpec{}
	}
	v.known("context_policy", m,
		"identity", "situation", "memory", "shared", "cause", "max_tokens", "on_overflow")

	cs := kernel.ContextSpec{
		Identity:   v.str("context_policy", m, "identity"),
		Situation:  v.strList("context_policy", m, "situation"),
		Memory:     v.str("context_policy", m, "memory"),
		Shared:     v.strList("context_policy", m, "shared"),
		Cause:      v.strList("context_policy", m, "cause"),
		OnOverflow: v.str("context_policy", m, "on_overflow"),
	}
	if raw, ok := m["max_tokens"]; ok && raw != nil {
		if n, ok := raw.(int64); !ok || n <= 0 {
			v.errf("context_policy: max_tokens must be a positive integer, found %v", raw)
		} else {
			cs.MaxTokens = int(n)
		}
	}
	return cs
}

func (v *validator) str(where string, m map[string]any, key string) string {
	raw, ok := m[key]
	if !ok || raw == nil {
		return ""
	}
	s, ok := raw.(string)
	if !ok {
		v.errf("%s: %s must be text, found %v", where, key, raw)
		return ""
	}
	return s
}

func (v *validator) bool(where string, m map[string]any, key string) bool {
	raw, ok := m[key]
	if !ok || raw == nil {
		return false
	}
	b, ok := raw.(bool)
	if !ok {
		v.errf("%s: %s must be true or false, found %v", where, key, raw)
		return false
	}
	return b
}

func (v *validator) strList(where string, m map[string]any, key string) []string {
	raw, ok := m[key]
	if !ok || raw == nil {
		return nil
	}
	items, ok := raw.([]any)
	if !ok {
		v.errf("%s: %s must be a list, found %v", where, key, raw)
		return nil
	}
	out := make([]string, 0, len(items))
	for i, it := range items {
		s, ok := it.(string)
		if !ok {
			v.errf("%s: %s[%d] must be text, found %v", where, key, i, it)
			continue
		}
		out = append(out, s)
	}
	return out
}

// enum validates a closed set. Misspelling `worktree` as `worktee` would fall
// back to the default and silently drop the isolation the user asked for, which
// config.go calls THE FATAL HOLE.
func (v *validator) enum(where string, m map[string]any, key string, allowed ...string) string {
	s := v.str(where, m, key)
	if s == "" || contains(allowed, s) {
		return s
	}
	if near := nearest(s, allowed); near != "" {
		v.errf("%s: %s = %q, did you mean %q?", where, key, s, near)
	} else {
		v.errf("%s: %s = %q; valid values are %s", where, key, s, strings.Join(allowed, ", "))
	}
	return ""
}

// toFloat accepts both integer and float, because `budget_warn_pct: 1` and
// `budget_warn_pct: 0.8` are both things a user writes and the parser hands
// back different Go types for them. Rejecting the integer form would be a
// distinction the file gives no hint of.
func toFloat(raw any) (float64, bool) {
	switch n := raw.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	}
	return 0, false
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func hasMember(members []kernel.MemberConfig, name string) bool {
	for _, m := range members {
		if m.Name == name {
			return true
		}
	}
	return false
}

// nearest returns the closest candidate within edit distance 2, or "".
//
// The bound is deliberately tight: a wrong suggestion is worse than none,
// because the user trusts it and edits the file into a different mistake.
func nearest(got string, candidates []string) string {
	best, bestD := "", 3
	for _, c := range candidates {
		if d := editDistance(got, c); d < bestD {
			best, bestD = c, d
		}
	}
	return best
}

func editDistance(a, b string) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	cur := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		cur[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[lb]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}
