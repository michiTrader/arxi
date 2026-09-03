// Package tool resolves what an agent is allowed to do, and runs what it is
// allowed to do.
//
// Split from the executor deliberately. Deciding whether a call is permitted is
// a pure question about declarations -- it has no filesystem, no subprocess and
// no clock -- while running the call is the opposite. Keeping them in one place
// would mean the permission rules could only be tested by actually executing
// something, which is the one thing a test of a DENY must never do.
//
// The vocabulary is surface.Policy rather than a new enum. There were already
// three policy words in this codebase, attached to arxi's own agent-facing
// tools, and a second set spelled the same way would be a trap: the first
// person to write PolicyAsk against the wrong type would get a compile error if
// they were lucky and a silent mismatch if they were not.
package tool

import (
	"fmt"
	"sort"
	"strings"

	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/surface"
)

// Mutating lists the tools that change something outside the process.
//
// This set already existed, uncredited, inside kernel.Normalize, where it
// decides that a blueprint whose members can write needs a worktree rather than
// a shared directory. It is named here because two places now need the same
// answer, and the failure mode of letting them drift is silent: a tool that
// counts as mutating for workspace isolation but not for policy would get a
// private directory to scribble in and no approval gate on the scribbling.
//
// It is a var and not a const so a test can prove the two agree, not so that
// callers can edit it.
var Mutating = map[string]bool{
	"write": true,
	"bash":  true,
	"edit":  true,
}

// Known lists the tools an agent may be granted.
//
// A closed list, because the alternative is treating an unrecognised name as a
// tool nobody has reviewed. `arxi agent create backend --tools reed` should say
// so, rather than silently granting a tool that will fail at the moment it is
// first needed, which is halfway through a paid run.
//
// The entries come from kernel.KnownTools rather than being written again here.
// They were written twice for exactly as long as it took the blueprint loader to
// need the same answer, and a second copy of a closed list is only ever one
// commit away from being a different closed list -- with the difference showing
// up as a grant the loader accepts and this package resolves as if it named
// nothing.
//
// The shape is still a set, because that is what callers ask it: membership. An
// earlier version of this comment said the value recorded "whether each mutates",
// which is what Mutating above is for. A reader who trusted it would have
// concluded that read and grep mutate, and looked for an approval gate on a
// search. Membership is the only question this map answers.
var Known = knownSet()

func knownSet() map[string]bool {
	m := make(map[string]bool, len(kernel.KnownTools))
	for _, t := range kernel.KnownTools {
		m[t] = true
	}
	return m
}

// Resolve returns the effective policy for one tool on one agent.
//
// The four rules the design commits to, in the order they apply:
//
//	a tool not granted to the agent  -> deny
//	a granted name that is not Known -> deny
//	a granted tool that mutates      -> ask
//	a granted tool that reads        -> allow
//
// Undeclared is deny, and that is the whole point rather than a detail. A
// permissive default turns every oversight into a silent hole, and the person
// who pays for the hole is never the person who forgot. Mutating tools are
// never allow by default for the same reason: if an agent can change the world
// without asking, that has to be a written decision.
//
// The Known check is why the second rule exists, and it is not defensive
// tidiness. Mutating is a closed list, so anything absent from it read as a
// reader and fell through to allow: `tools: [bahs]` resolved to ALLOW while
// `bash` resolved to ASK. A one-character typo did not disable the tool, it
// disabled the approval gate on the tool -- and it did so invisibly, because
// `arxi agent show` printed a policy for the misspelled name as if it were real.
// Resolving to deny instead means the worst a typo can do is refuse work.
//
// It sits before the override consult, not after, because an overrides file is
// hand-edited and `--allow bahs` must not be able to resurrect a name that has
// no implementation behind it. Overrides pick between the policies that apply to
// a real tool; they are not a second grant list.
//
// Overrides are otherwise consulted first and can say allow for a mutating tool,
// because `arxi agent tool policy --agent backend --allow bash` is a real
// command in the surface and the person typing it is making exactly that written
// decision. An override cannot grant a tool the agent was never given, though --
// that would make the grant list decorative.
func Resolve(granted []string, overrides map[string]surface.Policy, name string) surface.Policy {
	if !hasTool(granted, name) {
		return surface.PolicyDeny
	}
	if !Known[name] {
		return surface.PolicyDeny
	}
	if p, ok := overrides[name]; ok {
		return p
	}
	if Mutating[name] {
		return surface.PolicyAsk
	}
	return surface.PolicyAllow
}

// hasTool reports whether name was granted.
func hasTool(granted []string, name string) bool {
	for _, g := range granted {
		if g == name {
			return true
		}
	}
	return false
}

// ResolveAll returns the effective policy for every tool granted to an agent,
// in sorted order.
//
// Sorted because this is what `arxi agent show` prints, and a resolved policy
// listing that reorders itself between runs is unusable for the thing people
// actually do with it: diffing what an agent may do today against what it could
// do last week.
func ResolveAll(granted []string, overrides map[string]surface.Policy) []Resolved {
	seen := map[string]bool{}
	names := make([]string, 0, len(granted))
	for _, g := range granted {
		if seen[g] {
			continue
		}
		seen[g] = true
		names = append(names, g)
	}
	sort.Strings(names)

	out := make([]Resolved, 0, len(names))
	for _, n := range names {
		out = append(out, Resolved{Tool: n, Policy: Resolve(granted, overrides, n)})
	}
	return out
}

// Resolved is one tool and the policy that applies to it.
type Resolved struct {
	Tool   string         `json:"tool"`
	Policy surface.Policy `json:"policy"`
}

// ValidateGrants reports the tool names in a grant list that are not known.
//
// It returns every bad name rather than the first, because a member declaring
// `tools: [reed, writ]` has two typos and being told about one at a time means
// two edit-run cycles to learn something both discoverable at once.
func ValidateGrants(granted []string) error {
	var bad []string
	for _, g := range granted {
		if !Known[g] {
			bad = append(bad, g)
		}
	}
	if len(bad) == 0 {
		return nil
	}

	names := make([]string, 0, len(Known))
	for k := range Known {
		names = append(names, k)
	}
	sort.Strings(names)

	return fmt.Errorf("unknown tool(s): %s\n  known tools: %s",
		strings.Join(bad, ", "), strings.Join(names, ", "))
}
