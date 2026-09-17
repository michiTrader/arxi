package internal_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestEveryDeclaredPlatformStillCompiles builds the whole module, tests
// included, for each operating system this repository writes code for.
//
// The gap is structural rather than hypothetical. Several packages select an
// implementation with build tags -- internal/toolrun alone has file_linux.go,
// file_unsupported.go (`!linux`), command_linux.go, command_windows.go and
// command_unsupported.go (`!linux && !windows`), plus Windows-only test files.
// A suite run on Linux compiles exactly one branch of each. The others are
// never parsed, so a rename, a changed signature or an unused import in them
// is invisible: `go build ./...`, `go vet ./...`, `go test ./...` and
// scripts/bootstrap.sh all pass while a platform is broken.
//
// That matters here more than it would in most repositories, because the
// unbuilt branches are not incidental. ADR-0012 says a workspace name
// describes provisioned, platform-verified guarantees, and the Windows and
// non-Linux branches are precisely where the project states what it CANNOT
// promise: openWorkspaceRoot refusing, the command adapter refusing, the
// environment allowlist refusing. Those refusals are the honest-platform
// contract, and a refusal that stopped compiling would be discovered by a
// user, not by us.
//
// `go vet` is used rather than `go build` deliberately: build ignores _test.go
// files, and containment_windows_test.go and file_windows_test.go exist only
// on that platform. Vet type-checks the test files too, so it covers the code
// that documents Windows behaviour. It is also the closest thing to `go test`
// that works without a matching kernel -- the tests cannot RUN here, but
// nothing excuses them from having to compile.
func TestEveryDeclaredPlatformStillCompiles(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-platform type-checking is slow; -short skips it")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go is not in the PATH: %v", err)
	}

	// The three build-tag branches the repository actually writes code for.
	// darwin stands in for `!linux && !windows`: it is the branch selected by
	// command_unsupported.go and file_unsupported.go, and nothing in the tree
	// distinguishes it from other Unixes.
	for _, goos := range []string{"linux", "windows", "darwin"} {
		goos := goos
		t.Run(goos, func(t *testing.T) {
			t.Parallel()

			cmd := exec.Command("go", "vet", "./...")
			cmd.Dir = ".."
			// GOARCH is pinned so a host on another architecture does not ask
			// for a combination Go does not ship, which would look like a
			// project failure rather than an environment one.
			cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=amd64")

			out, err := cmd.CombinedOutput()
			if err == nil {
				return
			}
			t.Errorf("the module does not type-check for GOOS=%s:\n%s\n"+
				"  Only one build-tag branch is compiled by a normal test run, so this is "+
				"invisible to `go test ./...` and to scripts/bootstrap.sh. The unbuilt "+
				"branches are where this project states what it cannot promise (ADR-0012): "+
				"the refusals in file_unsupported.go, command_unsupported.go and the Windows "+
				"adapters. A refusal that stopped compiling would be found by a user.",
				goos, strings.TrimSpace(string(out)))
		})
	}
}
