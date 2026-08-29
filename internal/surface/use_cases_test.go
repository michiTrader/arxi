package surface

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The use-case document is the only place where the surface is checked against
// reality instead of against itself. surface_test.go proves the registry is
// internally consistent; that cannot prove the set of capabilities is
// *sufficient* to complete a real task, nor that every capability is reachable
// from one. Both of those are claims about the document, so they are tested here.
//
// Without these tests the document is prose, and prose drifts from code
// silently. That is exactly the failure the translation pass had to repair: five
// references to a file named decides.go that never existed, and a README whose
// build command did not run. A doc that cannot go stale without turning the
// build red is worth more than a doc that is merely correct today.
const useCaseDoc = "../../docs/design/20-use-cases.md"

func readUseCases(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(useCaseDoc))
	if err != nil {
		t.Fatalf("cannot read %s: %v\n"+
			"This file is part of the design, not decoration: it is what proves the "+
			"declared capabilities are sufficient and reachable. If it was renamed, "+
			"update useCaseDoc; if it was deleted, restore it.", useCaseDoc, err)
	}
	return string(b)
}

// countedDocs are every file that states the capability split in prose.
//
// TestUseCasesDocumentToolCount guarded only the design document, and the two
// files here drifted for exactly that reason: they made the same claim with
// nothing tying it to the registry. When `trigger run` was declared, the design
// doc turned the build red and was fixed in the same commit, while these two sat
// at "45 capabilities, 32 tools" — two capabilities and one tool out of date.
//
// The numbers themselves are minor. The claim they support is not: the gap
// between capabilities and tools is a security boundary, and a reader deciding
// whether to trust it has no way to tell a current count from a stale one.
var countedDocs = []string{
	"../../README.md",
	"../../AGENTS.md",
}

// TestEveryDocumentStatingTheSplitIsCurrent checks the counts wherever they
// appear, not just where somebody remembered to write a test.
//
// It reads the two numbers out of each document rather than requiring an exact
// sentence, because the three files word the claim differently and pinning
// phrasing would make every rewording a test failure. What must not drift is
// the arithmetic.
//
// A document that states neither number is not a failure — most files have no
// business repeating it. This only catches the ones that took on the obligation
// by stating it and then let it rot.
func TestEveryDocumentStatingTheSplitIsCurrent(t *testing.T) {
	tools := 0
	for _, c := range Registry {
		if c.Kind&AgentTool != 0 {
			tools++
		}
	}
	total := len(Registry)

	// Matches "47 declared capabilities", "declares 47 capabilities", and the
	// bolded variants, without caring which the author chose.
	capRe := regexp.MustCompile(`(?:\*\*)?(\d+)(?:\*\*)? declared capabilities|declares (?:\*\*)?(\d+)(?:\*\*)? capabilities`)
	toolRe := regexp.MustCompile(`(?:\*\*)?(\d+)(?:\*\*)? (?:are )?exposed as (?:agent )?tools|(?:\*\*)?(\d+)(?:\*\*)? are exposed as agent tools`)

	for _, path := range countedDocs {
		b, err := os.ReadFile(filepath.FromSlash(path))
		if err != nil {
			t.Errorf("cannot read %s: %v\n"+
				"if the file moved, update countedDocs; this test is what keeps "+
				"its capability counts honest.", path, err)
			continue
		}
		doc := string(b)

		if got, ok := firstNumber(capRe.FindStringSubmatch(doc)); ok && got != total {
			t.Errorf("%s says %d declared capabilities; the registry has %d.\n"+
				"why this matters: the gap between capabilities and agent tools is a "+
				"security boundary, and a stale count is a claim the reader cannot "+
				"verify.\n"+
				"what to do: correct the number in %s. It drifted because only "+
				"docs/design was test-guarded; this test now covers this file too.",
				path, got, total, path)
		}

		if got, ok := firstNumber(toolRe.FindStringSubmatch(doc)); ok && got != tools {
			t.Errorf("%s says %d capabilities are exposed as agent tools; the "+
				"registry exposes %d.\n"+
				"why this matters: this is the number an agent's blast radius is read "+
				"off. Understating it is the dangerous direction.\n"+
				"what to do: correct the number in %s, and if a capability crossed "+
				"the line, say why where the exclusions are listed.",
				path, got, tools, path)
		}
	}
}

// firstNumber returns the first non-empty capture of a regexp match.
//
// The patterns above have alternating groups so they can accept more than one
// phrasing; only one branch matches, so the rest are empty strings.
func firstNumber(m []string) (int, bool) {
	if m == nil {
		return 0, false
	}
	for _, g := range m[1:] {
		if g == "" {
			continue
		}
		n := 0
		for _, r := range g {
			n = n*10 + int(r-'0')
		}
		return n, true
	}
	return 0, false
}

// TestEveryCapabilityHasAUseCase fails if a declared capability is not reachable
// by walking the document.
//
// A capability no scenario reaches is one we will implement, test, document and
// maintain for nobody. Catching that here costs a table edit; catching it after
// the executor exists costs a deprecation.
func TestEveryCapabilityHasAUseCase(t *testing.T) {
	doc := readUseCases(t)

	var missing []string
	for _, c := range Registry {
		// Accept the CLI form ("run why") or the tool form ("iash_run_why"):
		// the agent-facing sections legitimately use the tool name, and both
		// are projections of the same Path (TestOneSingleSurface).
		if strings.Contains(doc, c.CLI()) || strings.Contains(doc, c.Name()) {
			continue
		}
		missing = append(missing, c.CLI())
	}
	sort.Strings(missing)

	if len(missing) > 0 {
		t.Errorf("%d declared capabilities appear in no use case:\n  %s\n\n"+
			"why this matters: a capability that no realistic scenario reaches is one "+
			"we are going to build and maintain for nobody, and the surface stops being "+
			"a design and becomes a wish list.\n"+
			"what to do: either add a scenario to %s that genuinely needs it, or delete "+
			"the entry from Registry. Do not add a bare mention to silence this test -- "+
			"that converts a real signal into decoration.",
			len(missing), strings.Join(missing, "\n  "), useCaseDoc)
	}
}

// cliCommandRE finds the command invocations in the document: shell lines
// starting with "$ iash" and agent lines starting with "> iash_".
var (
	cliCommandRE  = regexp.MustCompile(`(?m)^\$ iash ([a-z][a-z0-9 -]*)`)
	toolCommandRE = regexp.MustCompile(`(?m)^> (iash_[a-z_]+)`)
)

// TestUseCasesOnlyInvokeRealCommands fails if the document demonstrates a verb
// that does not exist.
//
// This is the stricter direction and the one that protects the reader. A
// scenario that shows `iash run abort` teaches a command that will never work,
// and the reader blames themselves before blaming the doc. It is also how the
// vocabulary stays single: if somebody renames `cancel` to `abort` in Registry,
// this test names every example that has to change.
func TestUseCasesOnlyInvokeRealCommands(t *testing.T) {
	doc := readUseCases(t)

	// Longest CLI paths first, so "agent tool policy" is matched before "agent".
	paths := make([][]string, 0, len(Registry))
	for _, c := range Registry {
		paths = append(paths, c.Path)
	}
	sort.Slice(paths, func(i, j int) bool { return len(paths[i]) > len(paths[j]) })

	resolves := func(words []string) bool {
		for _, p := range paths {
			if len(p) > len(words) {
				continue
			}
			match := true
			for i := range p {
				if words[i] != p[i] {
					match = false
					break
				}
			}
			if match {
				return true
			}
		}
		return false
	}

	var bad []string
	for _, m := range cliCommandRE.FindAllStringSubmatch(doc, -1) {
		words := strings.Fields(m[1])
		// Flags start the argument list; the verb is the leading words.
		for i, w := range words {
			if strings.HasPrefix(w, "-") {
				words = words[:i]
				break
			}
		}
		if len(words) == 0 {
			continue
		}
		// `iash why` and `iash version` are implemented-today shortcuts, not
		// registry paths; main.go handles them directly.
		if words[0] == "why" || words[0] == "version" || words[0] == "help" {
			continue
		}
		if !resolves(words) {
			bad = append(bad, "$ iash "+strings.Join(words, " "))
		}
	}

	names := map[string]bool{}
	for _, c := range Registry {
		names[c.Name()] = true
	}
	for _, m := range toolCommandRE.FindAllStringSubmatch(doc, -1) {
		if !names[m[1]] {
			bad = append(bad, "> "+m[1])
		}
	}

	if len(bad) > 0 {
		sort.Strings(bad)
		t.Errorf("%s demonstrates %d command(s) that do not exist in the surface:\n  %s\n\n"+
			"why this matters: an example the reader cannot run teaches a CLI we do not "+
			"have, and they will blame themselves before they blame the document.\n"+
			"what to do: fix the example, or add the capability to Registry if the "+
			"scenario really needs it. If a verb was renamed, this list is every place "+
			"that has to follow.",
			useCaseDoc, len(bad), strings.Join(bad, "\n  "))
	}
}

// TestUseCasesDocumentToolCount pins the 32/45 split that §20.12 states in prose.
//
// The document argues that the 14 non-tools are a security boundary and explains
// each one. If somebody exposes `agent tool policy` as an agent tool, the prose
// silently becomes false while every other test still passes -- and the false
// sentence is precisely the one a reader would rely on to trust the boundary.
func TestUseCasesDocumentToolCount(t *testing.T) {
	tools := 0
	for _, c := range Registry {
		if c.Kind&AgentTool != 0 {
			tools++
		}
	}
	total := len(Registry)

	doc := readUseCases(t)

	// The two halves are reported separately on purpose: "the registry changed"
	// and "the prose no longer matches the registry" need different fixes, and a
	// message that blurs them sends the reader to the wrong file.
	const statedTools, statedTotal = 33, 47
	const statedPhrase = "**33 are exposed as agent tools**"

	if tools != statedTools || total != statedTotal {
		t.Errorf("the agent-tool split changed: the registry now has %d agent tools "+
			"out of %d capabilities, while %s is written around %d of %d.\n"+
			"why this matters: that section explains why each of the %d non-tools is a "+
			"security boundary (an agent that can widen its own tool policy does not "+
			"have a policy). A stale count there is a security claim the reader cannot "+
			"verify.\n"+
			"what to do: if the change was intended, update the numbers in the document "+
			"AND the table of exclusions -- including the reasoning for whichever "+
			"capability crossed the line -- then update statedTools/statedTotal here.",
			tools, total, useCaseDoc, statedTools, statedTotal, statedTotal-statedTools)
		return
	}

	if !strings.Contains(doc, statedPhrase) {
		t.Errorf("the registry still has %d agent tools of %d, but %s no longer "+
			"contains the sentence that states it (%q).\n"+
			"why this matters: this test is the only thing tying that security argument "+
			"to the code. If the sentence is reworded, the argument stops being checked "+
			"and can rot without any test noticing.\n"+
			"what to do: restore the phrase, or update statedPhrase here to match the "+
			"new wording.",
			tools, total, useCaseDoc, statedPhrase)
	}
}
