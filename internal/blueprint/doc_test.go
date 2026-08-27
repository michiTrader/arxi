package blueprint

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestDesignDocBlueprintsStillLoad extracts every YAML block from
// docs/design/20-use-cases.md and loads it.
//
// The documentation is the contract: §20.4 tells the user exactly what to
// write, and a parser that cannot read its own documented example is worse than
// no parser, because the user has no reason to doubt the file. This test is the
// only thing that keeps the doc and the loader from drifting, since nothing
// else compiles the doc.
func TestDesignDocBlueprintsStillLoad(t *testing.T) {
	raw, err := os.ReadFile("../../docs/design/20-use-cases.md")
	if err != nil {
		t.Skipf("design doc not readable (%v); the check needs the repository layout", err)
	}

	blocks := regexp.MustCompile("(?s)```yaml\n(.*?)```").FindAllStringSubmatch(string(raw), -1)
	if len(blocks) == 0 {
		t.Fatal("no ```yaml block found in 20-use-cases.md: either the doc stopped showing " +
			"blueprints or the fence changed, and this test silently stopped protecting anything")
	}

	for _, b := range blocks {
		src := b[1]
		title := strings.SplitN(strings.TrimSpace(src), "\n", 2)[0]
		t.Run(title, func(t *testing.T) {
			if _, err := Load([]byte(src)); err != nil {
				t.Fatalf("a blueprint printed in the design doc does not load:\n%s\nerror: %v\n"+
					"the documented example is what users copy; fix the loader or fix the doc, "+
					"but they cannot disagree", src, err)
			}
		})
	}
}
