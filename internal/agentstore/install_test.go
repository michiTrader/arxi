package agentstore

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
)

// published is a blueprint of the shape `blueprint install` fetches: written by
// somebody else, carrying declarations no Record and no Team can hold.
//
// The watcher and the per-stage timeout are the load-bearing part of the fixture,
// not decoration. They are exactly what a render-and-write Install would drop, and
// both spend money on their own -- a watcher hands a member a turn when a matching
// event lands, with nobody typing a command, and an on_timeout of escalate opens an
// inbox question. A test whose fixture was two members and a stage would pass
// against an Install that silently reduced the file to what this package knows how
// to render.
const published = `name: code-review
workspace: worktree
members:
  - name: reviewer
    model: claude-sonnet-4-6
    tools: [read, grep]
  - name: fixer
    model: claude-sonnet-4-6
    tools: [read, write]
stages:
  - name: review
    advance_when: all
    timeout_ms: 600000
    on_timeout: escalate
  - name: fix
    advance_when: any
watchers:
  - agent: reviewer
    pattern: state.*
    action: notify
`

// TestAnInstalledBlueprintIsTheBytesThatWereFetched is the point of Install.
//
// Byte equality, and not a comparison of the loaded configs, because the defect
// being guarded against survives a config comparison: an Install that rendered the
// file through Team would produce a config with the same two members and the same
// two stage names, and would have thrown away the watcher, the timeout and the
// workspace line. Comparing the bytes is the only assertion that fails for every
// field this package does not happen to know about, including the ones added after
// this test was written.
func TestAnInstalledBlueprintIsTheBytesThatWereFetched(t *testing.T) {
	s := open(t)

	path, err := s.Install("code-review", []byte(published))
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(onDisk) != published {
		t.Errorf("the installed file is not the bytes that were handed in\n  want:\n%s\n  got:\n%s\n"+
			"  consequence: what was installed is not what was published. A digest "+
			"recorded at fetch time no longer describes the file, so nobody can "+
			"check the blueprint they are about to run against the one upstream "+
			"serves -- and whatever this store cannot render is gone without a word.",
			published, onDisk)
	}

	// And it is a blueprint the rest of the system reads, not just a file that
	// arrived: everything downstream of `run start` goes through Load.
	bp, err := s.Load("code-review")
	if err != nil {
		t.Fatalf("Load after Install: %v", err)
	}
	if n := len(bp.Config.Watchers); n != 1 {
		t.Errorf("the installed blueprint loads with %d watchers, want 1\n"+
			"  consequence: the one declaration that hands a member a turn with "+
			"nobody typing a command did not survive being installed.", n)
	}
	if got := bp.Config.Stages[0].TimeoutMs; got != 600000 {
		t.Errorf("stage review loaded with timeout_ms=%d, want 600000\n"+
			"  consequence: the published stage timeout was lost, so a stage that "+
			"was meant to escalate after ten minutes waits forever instead.", got)
	}
}

// TestInstallRefusesBytesTheValidatorWouldRefuse holds Install to the promise the
// package doc opens with.
//
// Create keeps that promise by construction -- Render loads what it rendered and
// returns the error instead of the bytes -- so this is the writer where it can be
// broken, and the input is the one nobody in this process wrote. An invalid file
// installed anyway is discovered by `run start`, after the name is in agents/ and
// after somebody has read `run it:` off a success message.
func TestInstallRefusesBytesTheValidatorWouldRefuse(t *testing.T) {
	s := open(t)

	// A member granted a tool the runtime has no implementation for. It parses as
	// YAML, so only the blueprint validator can catch it.
	const bad = `name: broken
members:
  - name: reviewer
    tools: [reed]
stages:
  - name: work
`
	if _, err := s.Install("broken", []byte(bad)); err == nil {
		t.Fatal("Install accepted a blueprint the validator rejects\n" +
			"  consequence: agents/broken.yaml exists, `agent list` shows it, and " +
			"`run start broken` is the thing that finally reports the problem -- " +
			"after the install printed success.")
	}
	if _, err := os.Stat(s.Path("broken")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused Install left %s behind (%v)\n"+
			"  consequence: a refusal that still writes is worse than an accept, "+
			"because the user was told nothing happened.", s.Path("broken"), err)
	}
}

// TestInstallDoesNotOverwriteAndDoesNotEscapeTheStore covers the two ways the
// caller's argument decides which file gets written.
//
// Both come from the same place: `blueprint install <ref> --as <name>` lets the
// name be typed, where `agent create` and `blueprint create` take one they also
// validate. The ref may be a URL, so in the general case part of the invocation is
// authored by whoever published the blueprint.
func TestInstallDoesNotOverwriteAndDoesNotEscapeTheStore(t *testing.T) {
	s := open(t)

	if _, err := s.Create(Record{Name: "code-review", Tools: []string{"read"}}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	before, err := os.ReadFile(s.Path("code-review"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	if _, err := s.Install("code-review", []byte(published)); !errors.Is(err, ErrExists) {
		t.Errorf("Install over an existing agent returned %v, want ErrExists\n"+
			"  consequence: a file fetched from somewhere else replaces an agent "+
			"somebody wrote, with its tool grant and its hand edits, and the command "+
			"that did it reported success.", err)
	}
	after, err := os.ReadFile(s.Path("code-review"))
	if err != nil {
		t.Fatalf("read back after the refusal: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("the file changed under a refused Install\n  before:\n%s\n  after:\n%s\n"+
			"  consequence: the refusal is a lie -- the user was told nothing was "+
			"written and their agent is gone.", before, after)
	}

	for _, name := range []string{"../escaped", "sub/nested", "", "  spaced  "} {
		if _, err := s.Install(name, []byte(published)); err == nil {
			t.Errorf("Install accepted the name %q\n"+
				"  consequence: --as decides a path, so a name with a separator in it "+
				"writes outside agents/ -- and `blueprint install` is the one verb whose "+
				"input can come from a URL. A name with surrounding whitespace is the "+
				"quieter half: the file exists and `run start` can never name it.", name)
		}
	}
}

// TestAnInstalledFileIsAnOrdinaryBlueprintTheUserCanEdit pins the mode and the
// fact that a leading comment block is not special.
//
// `blueprint install` prepends a provenance header -- where it came from, the
// digest of what was fetched, when -- and that header only works if it is an
// ordinary YAML comment: strippable by the loader, editable by the user, invisible
// to the reducer. If a comment block above `name:` ever stopped parsing, install
// would write files that `run start` refuses, and the failure would name the
// header rather than the change that broke it.
func TestAnInstalledFileIsAnOrdinaryBlueprintTheUserCanEdit(t *testing.T) {
	s := open(t)

	const header = "# installed by arxi blueprint install\n" +
		"# source: https://example.test/code-review.yaml\n" +
		"# sha256: 0000000000000000000000000000000000000000000000000000000000000000\n" +
		"\n"
	path, err := s.Install("code-review", []byte(header+published))
	if err != nil {
		t.Fatalf("Install with a provenance header: %v\n"+
			"  consequence: the header install writes is not parseable, so every "+
			"installed blueprint is a file `run start` refuses.", err)
	}

	bp, err := s.Load("code-review")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if bp.Name != "code-review" {
		t.Errorf("the loaded name is %q, want code-review\n"+
			"  consequence: the comment header is being read as content.", bp.Name)
	}
	if n := len(bp.Config.Members); n != 2 {
		t.Errorf("loaded %d members through the header, want 2", n)
	}

	// 0644 for write's reason: an agent definition is meant to be read, diffed and
	// committed. An installed one is meant to be read most of all -- it is the one
	// file in agents/ nobody here wrote.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o644 {
			t.Errorf("installed %s with mode %v, want -rw-r--r--\n"+
				"  consequence: the file a reviewer most needs to open before running "+
				"is the one they cannot read.", path, perm)
		}
	}
	if strings.Contains(string(published), "\r\n") {
		t.Fatal("the fixture picked up CRLF line endings, which makes the byte " +
			"comparison above test the checkout instead of Install")
	}
}
