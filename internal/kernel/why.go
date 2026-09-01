package kernel

import (
	"fmt"
	"strings"
)

// Why is the output of `arxi run why`.
//
// The value of this command is not printing the state nicely: it is that the
// answer to "why is nothing happening?" is DERIVED from the wait graph, not from
// a list of cases somebody wrote by hand. Every blocked member left a structured
// reference (Member.BlockedOn) when it emitted agent.blocked; here we walk that
// reference and translate it into a concrete command that unblocks.
//
// If a new blocking reason shows up tomorrow, the schema forces it to bring its
// structured reference and this code displays it without changes.
type Why struct {
	RunID string    `json:"run_id"`
	Lines []WhyLine `json:"lines"`
	Fix   []string  `json:"fix,omitempty"`
}

// WhyLine is one line of the cause tree. Depth is the indentation.
type WhyLine struct {
	Depth int    `json:"depth"`
	Text  string `json:"text"`
}

// Explain walks the run's wait graph and builds the explanation.
func Explain(s State, c Config) Why {
	w := Why{RunID: s.RunID}
	add := func(d int, f string, a ...any) {
		w.Lines = append(w.Lines, WhyLine{Depth: d, Text: fmt.Sprintf(f, a...)})
	}

	if s.Status.Terminal() {
		add(0, "run %s: %s", s.RunID, s.Status)
		if s.Result != "" {
			add(1, "%s", s.Result)
		}
		return w
	}
	if s.Status == StatusPaused {
		add(0, "run %s: paused by explicit request", s.RunID)
		w.Fix = append(w.Fix, "arxi run unpause "+s.RunID)
		return w
	}

	add(0, "run %s: %s", s.RunID, s.Status)

	// The advance rule first: it is the most common cause of "it is stuck and
	// every member looks fine".
	if st := c.StageAt(s.StageIndex); st != nil {
		var missing []string
		for _, m := range s.Members {
			if !m.Advisory && !m.Submitted && participates(c, m.Name, st.Name) {
				missing = append(missing, m.Name)
			}
		}
		add(1, "stage %q advances when: %s", st.Name, st.AdvanceWhen)
		if len(missing) > 0 {
			add(2, "missing the submit of: %s", strings.Join(missing, ", "))
		}
	}

	// Then each blocked member with its remedy.
	blocked := 0
	for _, m := range s.Members {
		if m.State != MemberWaiting && m.State != MemberFailed {
			continue
		}
		blocked++
		add(1, "%s: %s (%s) since seq %d", m.Name, m.State, m.Detail, m.SinceSeq)
		for _, l := range walkCause(m) {
			add(2, "%s", l.text)
			if l.fix != "" {
				w.Fix = append(w.Fix, l.fix)
			}
		}
	}

	if blocked == 0 && !anyBusy(s) && !anyRunnable(s) {
		add(1, "nobody is working and nobody can start: the run is quiescent")
		if st := c.StageAt(s.StageIndex); st != nil {
			add(2, "the advance rule of %q is not met and nobody is left to meet it", st.Name)
		}
		w.Fix = append(w.Fix,
			"arxi run prompt "+s.RunID+" \"...\"   # inject a new cause",
			"arxi run why "+s.RunID+" --json     # the full diagnosis",
		)
	}

	for _, m := range s.Members {
		if m.Busy() {
			add(1, "%s is in fact working (%s) since seq %d", m.Name, m.State, m.SinceSeq)
		}
	}

	// Questions waiting on a human, which is a cause in its own right.
	//
	// This was missing, and running the command is what showed it. A run
	// blocked by an exhausted budget parks every member's causes rather than
	// marking anybody MemberWaiting (see spawnCauses and spendingHalted in the
	// reducer), so the loop above found nothing to remediate and `run why`
	// printed a cause tree with no remedies at all -- on a run where `run show`
	// said "1 pending question / it is waiting on you". The command whose whole
	// declared purpose is "and how to unblock it" answered the one question it
	// exists to answer with silence.
	//
	// The remedy does not come from the member graph here because it cannot:
	// nobody is blocked. It comes from the inbox, which is the other place the
	// run can be waiting on somebody. Replied items are skipped for the reason
	// pendingAsks exists in the CLI -- the log is append-only, so an answered
	// question stays in Inbox forever and offering to approve it again would
	// send the user to re-answer something already settled.
	for _, it := range s.Inbox {
		if it.Replied {
			continue
		}
		if it.Agent != "" {
			add(1, "question %s (%s) waits on you: %s -- asked by %s",
				it.ID, it.Kind, it.Question, it.Agent)
		} else {
			add(1, "question %s (%s) waits on you: %s", it.ID, it.Kind, it.Question)
		}
		// Deduplicated against the member walk, which already emits
		// `arxi inbox approve <id>` for a member blocked on approval. The same
		// command printed twice reads like two separate steps to take.
		w.Fix = appendFix(w.Fix, "arxi inbox approve "+it.ID)
	}

	if s.BudgetUSD > 0 {
		add(1, "budget: %.4f of %.4f USD spent in the tree", s.TreeSpentUSD, s.BudgetUSD)
		// A breach is not the same fact as the spend, and the previous line
		// stated only the spend. `run show` learned to say "-- OVER by" for the
		// same reason: 0.0200 of 0.0010 is readable as ordinary progress unless
		// the reader happens to compare the two numbers.
		if s.TreeSpentUSD > s.BudgetUSD {
			add(2, "the budget is exhausted, so no new turn will start until it is raised")
			w.Fix = appendFix(w.Fix, "arxi run unpause "+s.RunID+" --budget <higher>")
		}
	}
	return w
}

// appendFix adds a remedy unless it is already there.
//
// Remedies now come from two places -- the member wait graph and the inbox --
// and both can point at the same inbox id, because a member blocked on approval
// is blocked BY a pending question. Printing "arxi inbox approve inbox-1" twice
// under "possible remedies" implies two things to do.
func appendFix(fix []string, cmd string) []string {
	for _, f := range fix {
		if f == cmd {
			return fix
		}
	}
	return append(fix, cmd)
}

type whyLeaf struct {
	text string
	fix  string
}

// walkCause is the remediation table. It is indexed by Detail (the blocked_on of
// the event) and reads the structured reference to build the exact command.
//
// Note that there is not a single `if runID == ...`, nor any special case per
// blueprint: the remedy comes out of the data the event was obliged to bring.
func walkCause(m Member) []whyLeaf {
	ref := m.BlockedOn
	get := func(k string) string {
		if ref == nil {
			return ""
		}
		v, _ := ref[k].(string)
		return v
	}

	switch m.Detail {
	case "approval":
		id, tool := get("inbox_id"), get("tool")
		return []whyLeaf{
			{text: fmt.Sprintf("waits for approval of the tool %q (inbox %s)", tool, id),
				fix: "arxi inbox approve " + id},
			{text: "to avoid being asked about this tool again:",
				fix: fmt.Sprintf("arxi agent tool policy --agent %s --allow %s", m.Name, tool)},
		}
	case "lock":
		key, holder := get("key"), get("holder")
		return []whyLeaf{
			{text: fmt.Sprintf("waits for the lock %q held by %s", key, holder),
				fix: "arxi state unlock " + key},
		}
	case "peer":
		return []whyLeaf{
			{text: fmt.Sprintf("waits for %s, which is itself waiting", get("peer"))},
		}
	case "budget":
		return []whyLeaf{
			{text: "the budget of the tree ran out",
				fix: "arxi run unpause <run> --budget <higher>"},
		}
	case "timer":
		return []whyLeaf{
			{text: fmt.Sprintf("waits for the timer %q", get("timer_id"))},
		}
	case "tool":
		return []whyLeaf{
			{text: fmt.Sprintf("waits for the tool %q to finish", get("tool"))},
		}
	case "workspace":
		return []whyLeaf{
			{text: fmt.Sprintf("waits for the workspace %q (write conflict)", get("path")),
				fix: "arxi run show <run> --workspace"},
		}
	}
	return []whyLeaf{{
		text: "blocked without a structured reference: this is a schema violation, " +
			"every waiting:* must bring blocked_ref (see spec/events.md)",
	}}
}
