package kernel

import (
	"fmt"
	"strings"
)

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
type Why struct {
	RunID string    `json:"run_id"`
	Lines []WhyLine `json:"lines"`
	Fix   []string  `json:"fix,omitempty"`
}

// Implementation note.
type WhyLine struct {
	Depth int    `json:"depth"`
	Text  string `json:"text"`
}

// Implementation note.
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
		add(0, "run %s: pausado for pedido explícito", s.RunID)
		w.Fix = append(w.Fix, "iash run unpause "+s.RunID)
		return w
	}

	add(0, "run %s: %s", s.RunID, s.Status)

	// Implementation note.
	// Implementation note.
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

	// Implementation note.
	blocked := 0
	for _, m := range s.Members {
		if m.State != MemberWaiting && m.State != MemberFailed {
			continue
		}
		blocked++
		add(1, "%s: %s (%s) from seq %d", m.Name, m.State, m.Detail, m.SinceSeq)
		for _, l := range walkCause(m) {
			add(2, "%s", l.text)
			if l.fix != "" {
				w.Fix = append(w.Fix, l.fix)
			}
		}
	}

	if blocked == 0 && !anyBusy(s) && !anyRunnable(s) {
		add(1, "nadie is working and nadie can empezar: the run is quiescent")
		if st := c.StageAt(s.StageIndex); st != nil {
			add(2, "the rule of avance of %q not is meets and not remains who the cumpla", st.Name)
		}
		w.Fix = append(w.Fix,
			"iash run prompt "+s.RunID+" \"...\"   # inyectar a cause nueva",
			"iash run why "+s.RunID+" --json     # the diagnóstico complete",
		)
	}

	for _, m := range s.Members {
		if m.Busy() {
			add(1, "%s yes is working (%s) from seq %d", m.Name, m.State, m.SinceSeq)
		}
	}

	if s.BudgetUSD > 0 {
		add(1, "budget: %.4f of %.4f USD spent en the tree", s.TreeSpentUSD, s.BudgetUSD)
	}
	return w
}

type whyLeaf struct {
	text string
	fix  string
}

// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
// Implementation note.
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
			{text: fmt.Sprintf("waits approval of the tool %q (inbox %s)", tool, id),
				fix: "iash inbox approve " + id},
			{text: "for not volver a ask for this tool:",
				fix: fmt.Sprintf("iash agent tool policy --agent %s --allow %s", m.Name, tool)},
		}
	case "lock":
		key, holder := get("key"), get("holder")
		return []whyLeaf{
			{text: fmt.Sprintf("waits the lock %q that has %s", key, holder),
				fix: "iash state unlock " + key},
		}
	case "peer":
		return []whyLeaf{
			{text: fmt.Sprintf("waits a %s, that a its vez is waiting", get("peer"))},
		}
	case "budget":
		return []whyLeaf{
			{text: "the budget of the tree is agotó",
				fix: "iash run unpause <run> --budget <mayor>"},
		}
	case "timer":
		return []whyLeaf{
			{text: fmt.Sprintf("waits the timer %q", get("timer_id"))},
		}
	case "tool":
		return []whyLeaf{
			{text: fmt.Sprintf("waits that termine the tool %q", get("tool"))},
		}
	case "workspace":
		return []whyLeaf{
			{text: fmt.Sprintf("waits the workspace %q (conflicto of escritura)", get("path")),
				fix: "iash run show <run> --workspace"},
		}
	}
	return []whyLeaf{{
		text: "blocked without reference structured: is a violación of the schema, " +
			"everything waiting:* must traer blocked_ref (see spec/events.md)",
	}}
}
