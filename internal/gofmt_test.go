package internal_test

import (
	"os/exec"
	"strings"
	"testing"
)

// TestEveryGoFileIsFormatted fails when any tracked Go file is not gofmt-clean.
//
// This exists because an unformatted file reached main and stayed there. The
// damage was not aesthetic: scripts/bootstrap.sh -- the repository's own
// recovery script, which exists because this project's sandbox is reset
// repeatedly -- runs `gofmt -l .` and calls die() on any output. So a single
// mis-indented continuation line in internal/toolrun/file.go made the script
// abort BEFORE it ever reached `go test`, on a clean checkout of main.
//
// That is the shape worth naming: the check already existed, in the one place
// nobody runs on purpose. `go test ./...` passed the whole time, so every
// contributor who verified the normal way saw green while the recovery path
// was broken. A guard that only fires inside a script somebody has to remember
// to execute is a guard that reports success by being skipped.
//
// Putting it in the suite closes that gap. gofmt is not a style preference in
// Go -- it is a single canonical form, so there is no judgement call here and
// nothing to argue about in review.
//
// It runs the real gofmt binary rather than go/format because that is what
// bootstrap.sh and any future CI will run; reimplementing the check with a
// library invites the two disagreeing, which would put us back to a green
// suite over a red script.
func TestEveryGoFileIsFormatted(t *testing.T) {
	if _, err := exec.LookPath("gofmt"); err != nil {
		// Skipping is correct only because the failure is environmental. If
		// gofmt is absent the test cannot make any claim, and pretending
		// otherwise would be worse than saying so.
		t.Skipf("gofmt is not in the PATH: %v", err)
	}

	out, err := exec.Command("gofmt", "-l", "..").Output()
	if err != nil {
		t.Fatalf("gofmt -l: %v", err)
	}

	var unformatted []string
	for _, line := range strings.Split(string(out), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			unformatted = append(unformatted, name)
		}
	}
	if len(unformatted) == 0 {
		return
	}

	t.Errorf("%d file(s) are not gofmt-clean:\n  %s\n"+
		"  Run `gofmt -w .`. This is not a style note: scripts/bootstrap.sh runs "+
		"`gofmt -l .` and aborts on any output, so an unformatted file breaks the "+
		"repository's recovery path while `go test ./...` still reports green.",
		len(unformatted), strings.Join(unformatted, "\n  "))
}
