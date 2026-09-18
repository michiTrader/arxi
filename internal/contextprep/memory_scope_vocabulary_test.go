package contextprep

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ADR-0022 reconciles the memory scope vocabulary, which three places in this
// repository once stated differently:
//
//	vision:   user, application, project, team, agent, run   (no tenant)
//	roadmap:  tenant, subject, application, project, team/agent, run
//	code:     Subject == the subject agent, committed as subject_agent in five
//	          artifact schemas
//
// A vocabulary decision has no runtime behaviour to pin, and inventing one
// would be the decoration ADR-0021 forbids. What can fail is the documents
// drifting apart again, so that is what these tests measure -- by reading the
// committed files, not a copy of the list.

// memoryScopes is the vocabulary ADR-0022 decided. `subject` is deliberately
// absent: it named the retrieving agent in committed artifacts and a separate
// scope in the roadmap, and one word cannot mean both in the same system.
var memoryScopes = []string{"tenant", "user", "application", "project", "team", "agent", "run"}

// TestMemoryScopeVocabularyAgreesAcrossDocuments fails if a scope is added to
// one document and not the other, which is the defect ADR-0022 corrects.
func TestMemoryScopeVocabularyAgreesAcrossDocuments(t *testing.T) {
	for _, doc := range []struct {
		path string
		line string
	}{
		{"docs/roadmap.md", "Authorization occurs before semantic ranking"},
		{"docs/design/30-vision.md", "Persistent memory should be an optional capability"},
	} {
		t.Run(doc.path, func(t *testing.T) {
			list := scopeSentence(t, doc.path, doc.line)
			for _, scope := range memoryScopes {
				if !strings.Contains(list, scope) {
					t.Errorf("%s does not list the %q scope:\n  %s\n"+
						"  every scope must appear in both documents, or a store built from one "+
						"authorizes a dimension the other never stated",
						doc.path, scope, list)
				}
			}
		})
	}
}

// TestSubjectIsNotAMemoryScope is the assertion that would have caught the
// original collision.
//
// It checks the scope sentences specifically rather than the whole file,
// because both documents legitimately say "subject agent" elsewhere -- and a
// whole-file search would therefore fail forever and be deleted by the next
// person who ran it.
func TestSubjectIsNotAMemoryScope(t *testing.T) {
	for _, doc := range []struct {
		path string
		line string
	}{
		{"docs/roadmap.md", "Authorization occurs before semantic ranking"},
		{"docs/design/30-vision.md", "Persistent memory should be an optional capability"},
	} {
		t.Run(doc.path, func(t *testing.T) {
			list := scopeSentence(t, doc.path, doc.line)
			if strings.Contains(list, "subject") {
				t.Errorf("%s lists a %q scope:\n  %s\n"+
					"  Subject already means the subject agent in five committed artifact "+
					"schemas, so a reader wires the scope to the field that exists and gets a "+
					"store whose subject scope is its agent scope, silently",
					doc.path, "subject", list)
			}
		})
	}
}

// TestTenantIsStatedInBothDocuments is separate from the agreement test on
// purpose.
//
// Comparing the two lists only to each other would pass on two lists that both
// omitted tenant, and tenant is the one scope whose absence is a containment
// gap rather than a naming inconsistency: every other scope narrows retrieval
// within a trust boundary, while tenant is the boundary.
func TestTenantIsStatedInBothDocuments(t *testing.T) {
	roadmap := scopeSentence(t, "docs/roadmap.md", "Authorization occurs before semantic ranking")
	vision := scopeSentence(t, "docs/design/30-vision.md", "Persistent memory should be an optional capability")

	for path, list := range map[string]string{"docs/roadmap.md": roadmap, "docs/design/30-vision.md": vision} {
		if !strings.Contains(list, "tenant") {
			t.Errorf("%s does not state the tenant scope:\n  %s\n"+
				"  a record with no project may be visible across projects, but a record with "+
				"no tenant must not be visible at all, so the trust boundary cannot be left to "+
				"the implementation to infer", path, list)
		}
	}
}

// scopeSentence returns the scope list from a document: the line matching
// marker plus the line after it, since both lists wrap.
func scopeSentence(t *testing.T, path, marker string) string {
	t.Helper()
	full := filepath.Join("..", "..", path)
	data, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if !strings.Contains(line, marker) {
			continue
		}
		sentence := line
		if i+1 < len(lines) {
			sentence += " " + lines[i+1]
		}
		return sentence
	}
	t.Fatalf("%s no longer contains the scope sentence marked by %q: if the wording moved, "+
		"this test must be pointed at the new line rather than deleted, because the drift it "+
		"detects is what ADR-0022 corrected", path, marker)
	return ""
}
