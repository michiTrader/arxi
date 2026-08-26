package kernel

import (
	"fmt"
	"strings"
)

// Why es la salida de `iash run why`.
//
// El valor de este comando no es imprimir el estado bonito: es que la respuesta
// a "¿por qué no pasa nada?" se DERIVA del grafo de espera, no de una lista de
// casos que alguien escribió a mano. Cada miembro bloqueado dejó una referencia
// estructurada (Member.BlockedOn) al emitir agent.blocked; acá se camina esa
// referencia y se traduce a un comando concreto que desbloquea.
//
// Si mañana aparece una razón nueva de bloqueo, el schema la obliga a traer su
// referencia estructurada y este código la muestra sin cambios.
type Why struct {
	RunID string    `json:"run_id"`
	Lines []WhyLine `json:"lines"`
	Fix   []string  `json:"fix,omitempty"`
}

// WhyLine es una línea del árbol de causas. Depth es la indentación.
type WhyLine struct {
	Depth int    `json:"depth"`
	Text  string `json:"text"`
}

// Explain camina el grafo de espera del run y arma la explicación.
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
		add(0, "run %s: pausado por pedido explícito", s.RunID)
		w.Fix = append(w.Fix, "iash run unpause "+s.RunID)
		return w
	}

	add(0, "run %s: %s", s.RunID, s.Status)

	// Primero la regla de avance: es la causa más común de "está trabado y todos
	// los miembros se ven bien".
	if st := c.StageAt(s.StageIndex); st != nil {
		var missing []string
		for _, m := range s.Members {
			if !m.Advisory && !m.Submitted && participates(c, m.Name, st.Name) {
				missing = append(missing, m.Name)
			}
		}
		add(1, "etapa %q avanza cuando: %s", st.Name, st.AdvanceWhen)
		if len(missing) > 0 {
			add(2, "falta el submit de: %s", strings.Join(missing, ", "))
		}
	}

	// Después, cada miembro bloqueado con su remedio.
	blocked := 0
	for _, m := range s.Members {
		if m.State != MemberWaiting && m.State != MemberFailed {
			continue
		}
		blocked++
		add(1, "%s: %s (%s) desde seq %d", m.Name, m.State, m.Detail, m.SinceSeq)
		for _, l := range walkCause(m) {
			add(2, "%s", l.text)
			if l.fix != "" {
				w.Fix = append(w.Fix, l.fix)
			}
		}
	}

	if blocked == 0 && !anyBusy(s) && !anyRunnable(s) {
		add(1, "nadie está trabajando y nadie puede empezar: el run está quiescente")
		if st := c.StageAt(s.StageIndex); st != nil {
			add(2, "la regla de avance de %q no se cumple y no queda quién la cumpla", st.Name)
		}
		w.Fix = append(w.Fix,
			"iash run prompt "+s.RunID+" \"...\"   # inyectar una causa nueva",
			"iash run why "+s.RunID+" --json     # el diagnóstico completo",
		)
	}

	for _, m := range s.Members {
		if m.Busy() {
			add(1, "%s sí está trabajando (%s) desde seq %d", m.Name, m.State, m.SinceSeq)
		}
	}

	if s.BudgetUSD > 0 {
		add(1, "presupuesto: %.4f de %.4f USD gastados en el árbol", s.TreeSpentUSD, s.BudgetUSD)
	}
	return w
}

type whyLeaf struct {
	text string
	fix  string
}

// walkCause es la tabla de remediación. Se indexa por Detail (el blocked_on del
// evento) y lee la referencia estructurada para armar el comando exacto.
//
// Nótese que no hay ni un `if runID == ...` ni un caso especial por blueprint:
// el remedio sale del dato que el evento estaba obligado a traer.
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
			{text: fmt.Sprintf("espera aprobación de la tool %q (inbox %s)", tool, id),
				fix: "iash inbox approve " + id},
			{text: "para no volver a preguntar por esta tool:",
				fix: fmt.Sprintf("iash agent tool policy --agent %s --allow %s", m.Name, tool)},
		}
	case "lock":
		key, holder := get("key"), get("holder")
		return []whyLeaf{
			{text: fmt.Sprintf("espera el lock %q que tiene %s", key, holder),
				fix: "iash state unlock " + key},
		}
	case "peer":
		return []whyLeaf{
			{text: fmt.Sprintf("espera a %s, que a su vez está esperando", get("peer"))},
		}
	case "budget":
		return []whyLeaf{
			{text: "el presupuesto del árbol se agotó",
				fix: "iash run unpause <run> --budget <mayor>"},
		}
	case "timer":
		return []whyLeaf{
			{text: fmt.Sprintf("espera el timer %q", get("timer_id"))},
		}
	case "tool":
		return []whyLeaf{
			{text: fmt.Sprintf("espera que termine la tool %q", get("tool"))},
		}
	case "workspace":
		return []whyLeaf{
			{text: fmt.Sprintf("espera el workspace %q (conflicto de escritura)", get("path")),
				fix: "iash run show <run> --workspace"},
		}
	}
	return []whyLeaf{{
		text: "bloqueado sin referencia estructurada: es una violación del schema, " +
			"todo waiting:* debe traer blocked_ref (ver spec/events.md)",
	}}
}
