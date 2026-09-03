package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// publishedBlueprint is the file a stranger wrote, and the fixture every walk
// below installs.
//
// The watcher, the stage timeout and the workspace line are load-bearing. They are
// exactly what an install that re-rendered through `blueprint create`'s Team would
// drop, and they are also the three declarations that spend money without anybody
// typing a command: a watcher hands a member a turn when a matching event lands,
// `on_timeout: escalate` opens an inbox question, and `workspace: worktree` decides
// where a member holding `write` writes. A fixture of two members and a stage would
// pass against an install that quietly reduced the file to what this binary knows
// how to render.
const publishedBlueprint = `name: code-review
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

// writeRef drops a blueprint in dir and returns the relative path to install from.
//
// Relative, because that is what a person types, and because it is the spelling
// that proves the provenance header does not simply echo the argument: the header
// has to record an absolute path or it records a fact about a shell that has since
// moved on.
func writeRef(t *testing.T, dir, name, body string) string {
	t.Helper()

	rel := name + ".yaml"
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return "./" + rel
}

// serveBlueprint stands up a loopback server for the fetch path.
//
// httptest speaks plain http, which is why `blueprint install` accepts http for
// loopback at all: the alternative is a certificate in the test tree, and the
// realistic outcome of that is a fetch path with no tests. The exception is
// defensible on its own terms -- anybody who can redirect loopback traffic can
// already write agents/ -- but this is the reason it is worth having.
func serveBlueprint(t *testing.T, body string) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// installedFile reads back what landed in agents/, failing if nothing did.
func installedFile(t *testing.T, dir, name string) string {
	t.Helper()

	b, err := os.ReadFile(filepath.Join(dir, "agents", name+".yaml"))
	if err != nil {
		t.Fatalf("reading back the installed blueprint: %v", err)
	}
	return string(b)
}

// TestInstallFromAPathKeepsTheBytesAndReportsWhatWillRun is the verb's whole
// purpose, asserted the only way that catches the interesting defect.
//
// The body after the header is compared byte for byte, because an install that
// rendered the file through Team would produce something with the same two members
// and the same two stage names -- a test comparing members and stages would pass
// against it -- while the watcher, the timeout, the advance rules and the workspace
// line were gone. Byte equality is the assertion that fails for every field this
// binary does not happen to know about, including ones added after today.
func TestInstallFromAPathKeepsTheBytesAndReportsWhatWillRun(t *testing.T) {
	dir := t.TempDir()
	ref := writeRef(t, dir, "code-review", publishedBlueprint)

	got := arxi(t, dir, "blueprint", "install", ref)
	if got.code != 0 {
		t.Fatalf("install exited %d, want 0:\n%s", got.code, got.out)
	}

	onDisk := installedFile(t, dir, "code-review")
	_, body, found := strings.Cut(onDisk, "\n\n")
	if !found {
		t.Fatalf("the installed file has no header/body split:\n%s", onDisk)
	}
	if body != publishedBlueprint {
		t.Errorf("the installed body is not the bytes that were read\n  want:\n%s\n  got:\n%s\n"+
			"  consequence: what was installed is not what was published, so the digest "+
			"printed on success describes nothing anybody can re-fetch -- and whatever "+
			"this binary cannot render was dropped without a word.", publishedBlueprint, body)
	}

	// The report is the screen where somebody else's tool grants get read, so the
	// facts that cost money have to be on it.
	for _, want := range []string{
		"blueprint code-review installed: 2 members, 2 stages",
		"fixer: tools: read (allow), write (ask)",
		"watcher reviewer on state.*: notify",
		"stages: review -> fix",
		"workspace: worktree",
		"nobody here wrote this",
	} {
		if !strings.Contains(got.out, want) {
			t.Errorf("the install report is missing %q:\n%s\n"+
				"  consequence: with `agent create` the user chose the model and the tools, "+
				"so the report confirms a decision they made. here they chose a URL, and "+
				"this is the first and possibly only place these are read.", want, got.out)
		}
	}

	// And it is an agent the rest of the CLI resolves, not just a file that landed.
	show := arxi(t, dir, "agent", "show", "code-review")
	if show.code != 0 {
		t.Fatalf("agent show after install exited %d, want 0:\n%s\n"+
			"  consequence: install printed success and wrote a file no other verb accepts.",
			show.code, show.out)
	}
}

// TestTheProvenanceHeaderSaysWhereItCameFromAndStaysAComment covers the one thing
// install adds to the bytes.
//
// Once written, an installed blueprint is indistinguishable from one a colleague
// composed -- while being the only file in agents/ nobody here wrote. `tools: [bash]`
// in it means something different from the same line in a file from `agent create`,
// and the header is the only place that difference is recorded.
//
// It has to stay a YAML comment, which is the second half of this test: a header
// that stopped parsing would make every installed blueprint a file `run start`
// refuses, and the failure would name the header rather than the change that broke
// it.
func TestTheProvenanceHeaderSaysWhereItCameFromAndStaysAComment(t *testing.T) {
	dir := t.TempDir()
	ref := writeRef(t, dir, "code-review", publishedBlueprint)

	got := arxi(t, dir, "blueprint", "install", ref)
	if got.code != 0 {
		t.Fatalf("install exited %d, want 0:\n%s", got.code, got.out)
	}
	onDisk := installedFile(t, dir, "code-review")

	header, _, _ := strings.Cut(onDisk, "\n\n")
	for _, line := range strings.Split(header, "\n") {
		if !strings.HasPrefix(line, "#") {
			t.Fatalf("the header line %q is not a comment\n"+
				"  consequence: install writes files `arxi run start` refuses, and the "+
				"parse error names the header instead of whatever change broke it.", line)
		}
	}

	// The absolute path, not the `./code-review.yaml` that was typed: a header
	// recording a relative path records a fact about a shell that is already gone.
	abs, err := filepath.Abs(filepath.Join(dir, "code-review.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(header, abs) {
		t.Errorf("the header does not record %s:\n%s\n"+
			"  consequence: the one fact that distinguishes this file from one somebody "+
			"here wrote is a relative path that means something else from another "+
			"directory.", abs, header)
	}

	// The digest is of the fetched bytes, so it is NOT the digest of this file --
	// and the header has to say so, or it is a digest that fails every check
	// anybody makes against it.
	if !strings.Contains(header, "of what was fetched, not of this file") {
		t.Errorf("the header does not say the digest excludes the header:\n%s\n"+
			"  consequence: `sha256sum agents/code-review.yaml` disagrees with the "+
			"recorded digest and the header gives no reason, so the honest conclusion "+
			"is that the file was tampered with.", header)
	}

	// The full digest is on disk and its first twelve characters are what the
	// success screen printed, which is what makes the two comparable at all.
	short := ""
	for _, line := range strings.Split(header, "\n") {
		if rest, ok := strings.CutPrefix(line, "# sha256:"); ok {
			full := strings.TrimSpace(rest)
			if len(full) != 64 {
				t.Fatalf("the recorded digest is %d characters, want 64: %q", len(full), full)
			}
			short = full[:12]
		}
	}
	if short == "" {
		t.Fatalf("the header records no sha256 line:\n%s", header)
	}
	if !strings.Contains(got.out, short) {
		t.Errorf("the success screen does not print %s, which the header records:\n%s\n"+
			"  consequence: the digest on screen and the digest in the file are two "+
			"different numbers, so neither can be checked against the other.", short, got.out)
	}

	// A blueprint file is meant to be read, diffed and committed; an installed one
	// most of all.
	if info, err := os.Stat(filepath.Join(dir, "agents", "code-review.yaml")); err != nil {
		t.Fatal(err)
	} else if perm := info.Mode().Perm(); perm != 0o644 && runtime.GOOS != "windows" {
		t.Errorf("installed with mode %v, want -rw-r--r--\n"+
			"  consequence: the file a reviewer most needs to open before running it is "+
			"the one they cannot read.", perm)
	}
}

// TestInstallFetchesOverHTTPAndWritesWhatTheServerSent is the URL half of the ref
// grammar, which is the half that makes this verb worth having.
//
// The served bytes end up on disk unchanged and the header records the URL rather
// than a path, because those two facts are what a re-fetch is checked against.
func TestInstallFetchesOverHTTPAndWritesWhatTheServerSent(t *testing.T) {
	dir := t.TempDir()
	srv := serveBlueprint(t, publishedBlueprint)

	url := srv.URL + "/code-review.yaml"
	got := arxi(t, dir, "blueprint", "install", url)
	if got.code != 0 {
		t.Fatalf("install from %s exited %d, want 0:\n%s", url, got.code, got.out)
	}

	onDisk := installedFile(t, dir, "code-review")
	header, body, _ := strings.Cut(onDisk, "\n\n")
	if body != publishedBlueprint {
		t.Errorf("the fetched body was changed on the way in\n  want:\n%s\n  got:\n%s",
			publishedBlueprint, body)
	}
	if !strings.Contains(header, url) {
		t.Errorf("the header does not record %s:\n%s\n"+
			"  consequence: the file came from the network and nothing on disk says "+
			"from where, so it cannot be re-fetched or compared.", url, header)
	}
	if !strings.Contains(got.out, "from:   "+url) {
		t.Errorf("the success screen does not name the source:\n%s", got.out)
	}
}

// TestInstallRefusesEverySchemeThatIsNotHTTPSOrLoopback is the security boundary,
// and it is a boundary because of what a blueprint is.
//
// The file decides which tools an agent may call and what a run may spend. Over
// plain http anybody on the path chooses that, and the failure mode is not a
// corrupt download: it is a working blueprint with `bash` added. So http is
// refused unless there is no path to be on, and every other scheme is refused by
// name rather than by a parse error about a directory called `ftp:`.
func TestInstallRefusesEverySchemeThatIsNotHTTPSOrLoopback(t *testing.T) {
	dir := t.TempDir()

	for _, tc := range []struct{ ref, want string }{
		{"http://example.com/bp.yaml", "use https"},
		{"file:///tmp/bp.yaml", "give the path itself"},
		{"ftp://example.com/bp.yaml", "not a scheme this installs from"},
		{"git+ssh://example.com/bp.yaml", "not a scheme this installs from"},
	} {
		got := arxi(t, dir, "blueprint", "install", tc.ref)
		if got.code != 2 {
			t.Errorf("install %s exited %d, want 2:\n%s\n"+
				"  consequence: exit 2 is the invocation being wrong, which is what this "+
				"is -- a CI job that retries on 1 would retry this forever.", tc.ref, got.code, got.out)
		}
		if !strings.Contains(got.out, tc.want) {
			t.Errorf("install %s said:\n%s\nwant something containing %q", tc.ref, got.out, tc.want)
		}
		if _, err := os.Stat(filepath.Join(dir, "agents")); err == nil {
			t.Errorf("install %s created agents/\n"+
				"  consequence: a refusal that still touches the store is worse than an "+
				"accept, because the user was told nothing happened.", tc.ref)
		}
	}
}

// TestInstallRedirectedToPlainHTTPRefuses is the same rule applied where it is
// actually load-bearing.
//
// A rule enforced only on the URL you typed is not a rule. The attack is a server
// that answers an https URL with a 302 to http://elsewhere, and the blueprint that
// gets installed then arrived in plaintext from a host nobody named.
func TestInstallRedirectedToPlainHTTPRefuses(t *testing.T) {
	dir := t.TempDir()

	// The destination the redirect names is deliberately NOT loopback, so the only
	// thing that can refuse it is the per-hop check -- and the assertion on the
	// message is what makes this test real: a fetch that failed for any other
	// reason would exit non-zero too, and only the check prints this sentence.
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.com/code-review.yaml", http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	got := arxi(t, dir, "blueprint", "install", redirector.URL+"/code-review.yaml")
	if got.code == 0 {
		t.Fatalf("install followed a redirect to plain http and succeeded:\n%s\n"+
			"  consequence: the scheme check applies only to the URL the user typed, so "+
			"any server can serve its blueprint over http by answering with a 302.", got.out)
	}
	if !strings.Contains(got.out, "use https") {
		t.Errorf("the refusal does not name the reason:\n%s\n"+
			"  consequence: the message has to say the redirect target was plaintext, "+
			"or the obvious reading is that the server is broken.", got.out)
	}
}

// TestABareNameSaysThereIsNoRegistryYet is about the ref somebody types first.
//
// `arxi blueprint install code-review` is what a person types who expects a
// registry, and there is not one. The honest answer says so. The alternative -- an
// error about `./code-review` not existing -- reads like a typo and sends them
// looking for a file they never had.
func TestABareNameSaysThereIsNoRegistryYet(t *testing.T) {
	dir := t.TempDir()

	got := arxi(t, dir, "blueprint", "install", "code-review")
	if got.code != 2 {
		t.Errorf("install of a bare name exited %d, want 2:\n%s", got.code, got.out)
	}
	for _, want := range []string{"no registry", "arxi blueprint create"} {
		if !strings.Contains(got.out, want) {
			t.Errorf("the refusal is missing %q:\n%s\n"+
				"  consequence: the user reads a file-not-found about a path they did not "+
				"type, and goes looking for the file instead of learning that refs are "+
				"paths and URLs.", want, got.out)
		}
	}

	// A path that is simply not there gets the ordinary error, and exit 1: the
	// invocation was right, the artifact is missing. Answering this with the
	// registry lecture would tell somebody who typed a path correctly that they
	// typed the wrong kind of thing.
	missing := arxi(t, dir, "blueprint", "install", "./vendor/code-review.yaml")
	if missing.code != 1 {
		t.Errorf("install of a missing path exited %d, want 1:\n%s", missing.code, missing.out)
	}
	if strings.Contains(missing.out, "no registry") {
		t.Errorf("a missing path got the registry lecture:\n%s", missing.out)
	}
}

// TestInstallRefusesAnInvalidBlueprintAndWritesNothing holds install to the
// promise the store makes: nothing lands that `blueprint validate` would reject.
//
// The input is the one nobody in this process wrote, which is why this is the
// writer where the promise can break. An invalid file installed anyway is
// discovered by `run start`, after the name is in agents/ and after somebody has
// read `run it:` off a success message.
func TestInstallRefusesAnInvalidBlueprintAndWritesNothing(t *testing.T) {
	dir := t.TempDir()

	// A member granted a tool the runtime has no implementation for: valid YAML,
	// so only the blueprint validator catches it.
	ref := writeRef(t, dir, "broken", "name: broken\nmembers:\n  - name: reviewer\n    tools: [reed]\nstages:\n  - name: work\n")

	got := arxi(t, dir, "blueprint", "install", ref)
	if got.code != 1 {
		t.Errorf("install of an invalid blueprint exited %d, want 1:\n%s\n"+
			"  consequence: exit 1 is the invocation being right and the artifact being "+
			"wrong, which is the split a CI job gates on.", got.code, got.out)
	}
	if _, err := os.Stat(filepath.Join(dir, "agents", "broken.yaml")); err == nil {
		t.Errorf("a refused install left agents/broken.yaml behind\n" +
			"  consequence: `agent list` shows it and `run start broken` is the thing " +
			"that finally reports the problem -- after the install printed success.")
	}

	// HTML is the way this fails with a URL that works in a browser: the page
	// ABOUT the file was served instead of the file. The loader's own complaint is
	// about a `<`, which reads like a corrupt blueprint rather than a wrong URL.
	srv := serveBlueprint(t, "<!doctype html>\n<html><body>code-review.yaml</body></html>\n")
	page := arxi(t, dir, "blueprint", "install", srv.URL+"/code-review.yaml")
	if page.code != 1 {
		t.Errorf("install of an HTML page exited %d, want 1:\n%s", page.code, page.out)
	}
	if !strings.Contains(page.out, "looks like HTML") {
		t.Errorf("the refusal does not name the likely cause:\n%s\n"+
			"  consequence: the user is told their YAML is malformed when the real "+
			"problem is that they pasted the browser URL.", page.out)
	}
}

// TestInstallDoesNotOverwriteAndPointsAtTheWayPast covers the name colliding.
//
// It is the store's refusal, and it matters more here than for `agent create`: the
// file that would be replaced is one somebody in this repository wrote, with its
// tool grants and its hand edits, and the bytes replacing it came from outside.
func TestInstallDoesNotOverwriteAndPointsAtTheWayPast(t *testing.T) {
	dir := t.TempDir()
	storeAgent(t, dir, "code-review", "--tools", "read")
	before := installedFile(t, dir, "code-review")

	ref := writeRef(t, dir, "published", publishedBlueprint)
	got := arxi(t, dir, "blueprint", "install", ref)
	if got.code != 2 {
		t.Errorf("install over an existing agent exited %d, want 2:\n%s", got.code, got.out)
	}
	if after := installedFile(t, dir, "code-review"); after != before {
		t.Errorf("the file changed under a refused install\n  before:\n%s\n  after:\n%s\n"+
			"  consequence: the refusal is a lie -- the user was told nothing was written "+
			"and their agent is gone.", before, after)
	}
	for _, want := range []string{"arxi agent show code-review", "--as"} {
		if !strings.Contains(got.out, want) {
			t.Errorf("the refusal is missing %q:\n%s\n"+
				"  consequence: the user is stopped with no way forward but deleting an "+
				"agent they have not read.", want, got.out)
		}
	}
}

// TestAsInstallsUnderAnotherNameAndSaysTheTwoNowDiffer is why `as` exists.
//
// The name a stranger chose can already be taken here, and renaming on install is
// the only way to have both. It creates a divergence that is load-bearing in both
// directions -- the filename is what `run start` resolves, the `name:` inside is
// what the run records -- so the report has to say so. The line inside the file is
// NOT rewritten, because the digest was computed over the fetched bytes.
func TestAsInstallsUnderAnotherNameAndSaysTheTwoNowDiffer(t *testing.T) {
	dir := t.TempDir()
	ref := writeRef(t, dir, "published", publishedBlueprint)

	got := arxi(t, dir, "blueprint", "install", ref, "--as", "code-review-upstream")
	if got.code != 0 {
		t.Fatalf("install --as exited %d, want 0:\n%s", got.code, got.out)
	}

	onDisk := installedFile(t, dir, "code-review-upstream")
	if !strings.Contains(onDisk, "name: code-review\n") {
		t.Errorf("--as rewrote the `name:` line inside the file:\n%s\n"+
			"  consequence: the bytes no longer hash to the digest recorded above them, "+
			"so the provenance header describes a file that never existed anywhere.", onDisk)
	}
	if !strings.Contains(got.out, "the file declares `name: code-review`") {
		t.Errorf("the report does not mention the divergence:\n%s\n"+
			"  consequence: `run start` resolves the filename and the run records the "+
			"declared name, so the operator meets the difference when a run is named "+
			"after something they did not install.", got.out)
	}

	// A name that cannot become a file is refused before anything is fetched, which
	// is the ordering `blueprint create` already uses: an invocation that cannot
	// succeed should not first make a request to somebody else's server.
	for _, bad := range []string{"../escaped", "sub/nested", "  spaced  "} {
		r := arxi(t, dir, "blueprint", "install", ref, "--as", bad)
		if r.code != 2 {
			t.Errorf("--as %q exited %d, want 2:\n%s\n"+
				"  consequence: --as decides a path, and this is the one verb whose input "+
				"can come from a URL.", bad, r.code, r.out)
		}
		if !strings.Contains(r.out, "nothing was fetched") {
			t.Errorf("--as %q did not say the name was checked first:\n%s", bad, r.out)
		}
	}
}

// TestABlueprintWithNoNameIsRefusedRatherThanNamedAfterTheURL is the case where
// the name has to come from somewhere and there is nowhere honest to get it.
//
// Deriving it from the ref is the tempting answer and the wrong one: a URL tail is
// a URL artifact, so `/download?id=7` installs an agent called `download`, and
// every other writer in this store guarantees the filename matches the declared
// name -- a divergence `agent show` reports as a hand edit.
func TestABlueprintWithNoNameIsRefusedRatherThanNamedAfterTheURL(t *testing.T) {
	dir := t.TempDir()
	srv := serveBlueprint(t, "members:\n  - name: a\n    tools: [read]\nstages:\n  - name: s\n")

	got := arxi(t, dir, "blueprint", "install", srv.URL+"/download?id=7")
	if got.code != 2 {
		t.Errorf("install of a nameless blueprint exited %d, want 2:\n%s", got.code, got.out)
	}
	if !strings.Contains(got.out, "--as") {
		t.Errorf("the refusal does not name the way forward:\n%s", got.out)
	}
	if entries, err := os.ReadDir(filepath.Join(dir, "agents")); err == nil && len(entries) > 0 {
		t.Errorf("something was installed anyway: %v\n"+
			"  consequence: an agent named after a query string, whose filename and "+
			"declared name disagree for reasons nobody chose.", entries)
	}
}

// TestInstallStopsReadingAtTheCapAndSaysSo bounds what a server can make this
// process do.
//
// The loader is not streaming: it reads the whole document and builds a tree from
// it, so without a cap the size of this process is chosen by whoever serves the
// URL. The refusal names the number, so a genuinely large blueprint is a decision
// somebody makes rather than a mystery.
func TestInstallStopsReadingAtTheCapAndSaysSo(t *testing.T) {
	dir := t.TempDir()

	// Valid YAML the whole way, so nothing but the cap can refuse it: a trailing
	// comment padded past a megabyte.
	big := publishedBlueprint + "# " + strings.Repeat("padding ", (1<<20)/8) + "\n"
	srv := serveBlueprint(t, big)

	got := arxi(t, dir, "blueprint", "install", srv.URL+"/code-review.yaml")
	if got.code != 1 {
		t.Errorf("install of an oversize body exited %d, want 1:\n%s", got.code, got.out)
	}
	if !strings.Contains(got.out, "1048576 bytes") {
		t.Errorf("the refusal does not name the cap:\n%s\n"+
			"  consequence: the user cannot tell whether the file is too large or the "+
			"server is broken, and has no number to compare theirs against.", got.out)
	}
	if _, err := os.Stat(filepath.Join(dir, "agents", "code-review.yaml")); err == nil {
		t.Error("the oversize install wrote a file")
	}
}

// TestANon200IsReportedWithTheStatusAndTheURL keeps the two failures a user
// actually hits distinguishable.
//
// 404 is usually a URL that serves a page rather than the file. 401 and 403 lead
// straight to looking for a --token flag that does not exist, so the message says
// this verb sends no credentials and names the move that works: fetch it with
// something that has them, then install the file.
func TestANon200IsReportedWithTheStatusAndTheURL(t *testing.T) {
	dir := t.TempDir()

	for _, tc := range []struct {
		code int
		want string
	}{
		{http.StatusNotFound, "rather than a page about it"},
		{http.StatusUnauthorized, "sends no credentials"},
		{http.StatusForbidden, "sends no credentials"},
		{http.StatusInternalServerError, "500"},
	} {
		status := tc.code
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "no", status)
		}))
		got := arxi(t, dir, "blueprint", "install", srv.URL+"/code-review.yaml")
		srv.Close()

		if got.code != 1 {
			t.Errorf("a %d answer exited %d, want 1:\n%s", status, got.code, got.out)
		}
		if !strings.Contains(got.out, tc.want) {
			t.Errorf("a %d answer said:\n%s\nwant something containing %q", status, got.out, tc.want)
		}
		if !strings.Contains(got.out, srv.URL) {
			t.Errorf("a %d answer does not name the URL that was fetched:\n%s\n"+
				"  consequence: with a redirect in the way, the URL that failed is not "+
				"necessarily the one the user typed.", status, got.out)
		}
	}
}

// TestAZeroStageInstallSaysTheRunWillDoNothing is a real file, not a defensive
// branch.
//
// ResolveDefaults synthesises no stage, and applyRunStarted returns nil when there
// are none, so `run start` on this blueprint enters no stage, activates nobody and
// records run.quiescent after zero turns. Every other caveat on the screen names
// stages[0], so this one has to come first and stop.
func TestAZeroStageInstallSaysTheRunWillDoNothing(t *testing.T) {
	dir := t.TempDir()
	ref := writeRef(t, dir, "stageless", "name: stageless\nmembers:\n  - name: a\n    model: claude-sonnet-4-6\n    tools: [read]\n")

	got := arxi(t, dir, "blueprint", "install", ref)
	if got.code != 0 {
		t.Fatalf("install of a stageless blueprint exited %d, want 0:\n%s\n"+
			"  consequence: the file is valid and installing it is not the error; running "+
			"it is the surprise, which is what the caveat is for.", got.code, got.out)
	}
	if !strings.Contains(got.out, "no stages") {
		t.Errorf("the report does not warn about the missing stages:\n%s\n"+
			"  consequence: the run starts, spends nothing, ends quiescent, and the "+
			"reason is a `stages:` key the published file never had.", got.out)
	}
	if strings.Contains(got.out, "stages: ") {
		t.Errorf("a stageless install printed a stage list:\n%s", got.out)
	}
}
