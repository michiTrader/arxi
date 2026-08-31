// Package toolstore persists per-agent tool policy overrides on disk.
//
// Separate from internal/tool for the reason every pure package in this tree has
// a store beside it: internal/tool is forbidden by arch_test from touching os,
// so the rule about what an agent may do can be tested without a filesystem.
// This package decides only where the bytes live.
//
// # What an override is, and what it is not
//
// tool.Resolve already had a place for these:
//
//	a tool not granted to the agent  -> deny
//	an override, if one exists       -> whatever it says
//	a granted tool that mutates      -> ask
//	a granted tool that reads        -> allow
//
// The override slot existed and was always nil. `run start` built its executor
// with ToolPolicy unset, so the second line of that table was unreachable and
// the only way to stop being asked about a tool was to stop granting it. This
// package fills the slot.
//
// An override does NOT grant a tool. tool.Resolve checks the grant list first
// and returns deny for anything the agent was never given, so `--allow bash` on
// an agent without bash changes nothing. That ordering is deliberate and is not
// this package's to relax: an override that could grant would make the
// blueprint's `tools:` list decorative, and the blueprint is the thing under
// review.
//
// # Why a store and not an event
//
// Every other durable decision in this project is an event in a run's log, and
// this one is not. The reason is in the surface: `agent tool policy` takes an
// --agent and no --run. It is an operator decision about an agent across runs,
// not a fact about one run, and writing it into one run's log would mean the
// next run could not see it.
//
// The sharper reason is the frozen blueprint. `run start` freezes
// blueprint.snapshot.yaml so a run is judged by rules that cannot change
// underneath it. Overrides sit OUTSIDE that snapshot on purpose: they are the
// operator's standing answer to "stop asking me about this", and they are read
// fresh at the start of every run rather than baked into one.
//
// The cost of that choice is real and is documented rather than hidden: a policy
// change does not reach a run that is already going. See Load.
//
// # One file per agent
//
// policies/<agent>.json, not one index. Same reasoning as trigstore and
// modelstore: `agent tool policy --agent backend --allow bash` rewrites the
// record of the agent it names and nothing else, so a crash mid-write cannot
// lose the policy of an agent the command never mentioned.
//
// # Not in the home directory
//
// Policies sit beside runs/, triggers/ and providers/ in the working directory,
// following the same trade-off those made. A home-directory store would make an
// override follow the user between projects, so `--allow bash` typed once for a
// throwaway experiment would silently widen an agent's reach in every
// repository afterwards. That is precisely the kind of authorization nobody
// remembers granting.
package toolstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/michiTrader/arxi/internal/surface"
	"github.com/michiTrader/arxi/internal/tool"
)

// DefaultDir is where policies live, relative to the working directory.
const DefaultDir = "policies"

// ext is the suffix that makes a file a policy. Temp files are written as
// <agent>.json.tmp-NNNN, which does not end in ext, which is why names() can
// glob for ext and never see a half-written policy. Load-bearing.
const ext = ".json"

// Record is one agent's overrides.
//
// Tools maps a tool name to the policy that replaces the default for it. An
// absent tool is not an override of "use the default" -- it is the absence of an
// override, which is the same thing but says so.
type Record struct {
	Agent string                    `json:"agent"`
	Tools map[string]surface.Policy `json:"tools"`
}

// Validate rejects a record that would resolve to something meaningless.
//
// Both checks are about names rather than about permission, because the
// dangerous direction here is a typo, not a decision. `--allow bahs` is not a
// widening of anything; it is an override that will never match a tool and will
// therefore never fire, so the agent keeps being asked and the operator has a
// file that says otherwise. Refusing at write time makes that a message instead
// of a mystery.
func (r Record) Validate() error {
	if strings.TrimSpace(r.Agent) == "" {
		return errors.New("a policy needs an agent name")
	}
	if strings.ContainsAny(r.Agent, `/\`) || r.Agent == "." || r.Agent == ".." {
		// The agent name becomes a filename, so a path separator in it would
		// write outside the store. Refused rather than sanitised: an operator
		// who typed a slash meant something, and quietly renaming their agent
		// is a worse answer than telling them it cannot be spelled that way.
		return fmt.Errorf("agent name %q cannot contain a path separator", r.Agent)
	}

	var badTools, badPolicies []string
	for name, pol := range r.Tools {
		if !tool.Known[name] {
			badTools = append(badTools, name)
		}
		switch pol {
		case surface.PolicyAllow, surface.PolicyAsk, surface.PolicyDeny:
		default:
			badPolicies = append(badPolicies, fmt.Sprintf("%s=%s", name, pol))
		}
	}
	sort.Strings(badTools)
	sort.Strings(badPolicies)

	if len(badTools) > 0 {
		return fmt.Errorf("unknown tool(s): %s\n  known tools: %s",
			strings.Join(badTools, ", "), strings.Join(knownNames(), ", "))
	}
	if len(badPolicies) > 0 {
		return fmt.Errorf("unknown policy in %s\n  a policy is one of: allow, ask, deny",
			strings.Join(badPolicies, ", "))
	}
	return nil
}

// knownNames lists the known tools in a stable order, for error messages.
func knownNames() []string {
	out := make([]string, 0, len(tool.Known))
	for k := range tool.Known {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Encode renders a record as the bytes on disk.
func (r Record) Encode() ([]byte, error) {
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("toolstore: encode policy for %q: %w", r.Agent, err)
	}
	return append(body, '\n'), nil
}

// Store is a directory of policies.
type Store struct{ dir string }

// Open prepares dir, creating it if necessary.
func Open(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("toolstore: no directory given (default is %q)", DefaultDir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("toolstore: create %s: %w", dir, err)
	}
	return &Store{dir: dir}, nil
}

// Dir is the directory this store reads and writes.
func (s *Store) Dir() string { return s.dir }

// Path is the file an agent's policy occupies.
func (s *Store) Path(agent string) string { return filepath.Join(s.dir, agent+ext) }

// Set records one override, leaving the agent's other overrides alone.
//
// Read-modify-write rather than replace, because the command sets one tool at a
// time: `--allow bash` after `--deny write` must not silently restore write.
// Losing a DENY is the direction that matters -- it is the only one that can
// widen an agent's reach as a side effect of narrowing it somewhere else.
//
// It returns the policy that was in effect before, and whether there was one, so
// a caller can tell "changed" from "already said that" without reading twice.
func (s *Store) Set(agent, toolName string, pol surface.Policy) (surface.Policy, bool, error) {
	rec, err := s.Load(agent)
	if err != nil {
		return "", false, err
	}
	if rec.Tools == nil {
		rec.Tools = map[string]surface.Policy{}
	}
	rec.Agent = agent

	prev, had := rec.Tools[toolName]
	rec.Tools[toolName] = pol

	if err := rec.Validate(); err != nil {
		return "", false, err
	}
	if err := s.write(rec); err != nil {
		return "", false, err
	}
	return prev, had, nil
}

// Clear removes one override, so the tool goes back to its default policy.
//
// Removing the file entirely when the last override goes is deliberate. An empty
// {"tools":{}} on disk reads as "this agent has been configured", and the next
// person to look would try to work out what was decided. Nothing decided leaves
// nothing behind.
func (s *Store) Clear(agent, toolName string) (bool, error) {
	rec, err := s.Load(agent)
	if err != nil {
		return false, err
	}
	if _, had := rec.Tools[toolName]; !had {
		return false, nil
	}
	delete(rec.Tools, toolName)

	if len(rec.Tools) == 0 {
		if err := os.Remove(s.Path(agent)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("toolstore: remove policy for %q: %w", agent, err)
		}
		return true, fsyncDir(s.dir)
	}
	return true, s.write(rec)
}

// Load reads one agent's overrides, returning an empty record when there are
// none.
//
// A missing file is not an error, and that is the important half of this
// function. Every run start reads the policy of every member, and the normal
// case -- by a wide margin -- is that no override exists. Treating that as an
// error would mean a run could not start until somebody had configured
// something they had no reason to configure.
//
// # This is read at run start, and not again
//
// The value returned here is copied into the executor when a run begins. A run
// already in flight does not see a later change, so `agent tool policy` does not
// unblock a run that is currently waiting for an approval -- it stops the NEXT
// run being asked. That is a real limitation with a real cause: the run loop
// holds the config it was built with, and re-reading policy mid-run would mean
// the rules a run is judged by could change between two turns of the same run.
//
// The consequence is documented in the CLI's own output rather than only here,
// because the person who needs to know is mid-incident and is not reading Go
// doc comments.
func (s *Store) Load(agent string) (Record, error) {
	if strings.TrimSpace(agent) == "" {
		return Record{}, errors.New("toolstore: no agent name given")
	}

	raw, err := os.ReadFile(s.Path(agent))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Record{Agent: agent, Tools: map[string]surface.Policy{}}, nil
		}
		return Record{}, fmt.Errorf("toolstore: read policy for %q: %w", agent, err)
	}

	var rec Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return Record{}, fmt.Errorf("toolstore: parse %s: %w\n"+
			"  this file is text a human can edit; fix it or delete it to go back "+
			"to the default policy", s.Path(agent), err)
	}
	// Validated on the way IN as well as on the way out, because the file is
	// text a human can edit and does edit. A hand-written "bash": "alow" would
	// otherwise be an override that never matches any policy the resolver knows,
	// so the agent would keep asking while the file said it should not.
	if err := rec.Validate(); err != nil {
		return Record{}, fmt.Errorf("toolstore: %s: %w", s.Path(agent), err)
	}
	if rec.Tools == nil {
		rec.Tools = map[string]surface.Policy{}
	}
	// The filename is authoritative over the field. They can only disagree if
	// the file was moved by hand, and in that case the name the operator typed
	// is the one they meant.
	rec.Agent = agent
	return rec, nil
}

// LoadAll reads the overrides for every agent that has any, keyed by agent.
//
// This is the shape provider.Executor.ToolPolicy wants, so a run start can pass
// it straight through without a conversion loop that could drop an agent.
func (s *Store) LoadAll() (map[string]map[string]surface.Policy, error) {
	agents, err := s.names()
	if err != nil {
		return nil, err
	}

	out := map[string]map[string]surface.Policy{}
	for _, a := range agents {
		rec, err := s.Load(a)
		if err != nil {
			return nil, err
		}
		if len(rec.Tools) == 0 {
			continue
		}
		out[a] = rec.Tools
	}
	return out, nil
}

// Agents lists the agents that have at least one override, sorted.
func (s *Store) Agents() ([]string, error) { return s.names() }

// names lists the agents with a policy file, sorted.
func (s *Store) names() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("toolstore: read %s: %w", s.dir, err)
	}

	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ext) {
			continue
		}
		out = append(out, strings.TrimSuffix(e.Name(), ext))
	}
	sort.Strings(out)
	return out, nil
}

// write publishes a record atomically: a crash leaves either the old file or the
// new one, never a truncated policy that fails to parse.
//
// A truncated policy is worse here than a truncated trigger. Load refuses a file
// it cannot parse, and every run start reads these, so a half-written file would
// stop runs from starting rather than merely stop one trigger from firing.
func (s *Store) write(r Record) error {
	body, err := r.Encode()
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(s.dir, r.Agent+ext+".tmp-*")
	if err != nil {
		return fmt.Errorf("toolstore: create temp file for policy %q: %w", r.Agent, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // a no-op once the rename has succeeded

	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("toolstore: write policy %q: %w", r.Agent, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("toolstore: fsync policy %q: %w", r.Agent, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("toolstore: close temp file for policy %q: %w", r.Agent, err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("toolstore: chmod policy %q: %w", r.Agent, err)
	}
	if err := os.Rename(tmpName, s.Path(r.Agent)); err != nil {
		return fmt.Errorf("toolstore: publish policy %q: %w", r.Agent, err)
	}
	// The rename is a directory operation. Fsyncing the file does not make the
	// entry that names it durable, so the directory needs its own sync.
	return fsyncDir(s.dir)
}

// fsyncDir makes a rename durable.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("toolstore: open %s to sync: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("toolstore: sync %s: %w", dir, err)
	}
	return nil
}
