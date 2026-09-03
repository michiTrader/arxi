package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/michiTrader/arxi/internal/agentstore"
	"github.com/michiTrader/arxi/internal/blueprint"
	"github.com/michiTrader/arxi/internal/surface"
)

// maxRefBytes caps what a URL is allowed to hand back.
//
// The loader is not streaming: it reads the whole document into memory and builds
// a tree from it, so without a cap the size of this process is decided by whoever
// serves the URL. A megabyte is far more than a blueprint -- examples/ tops out in
// the low kilobytes -- and the refusal names the number so a genuinely large file
// is a decision somebody makes rather than a mystery.
const maxRefBytes = 1 << 20

// fetchTimeout bounds the whole request, connect through body.
//
// http.Client's Timeout covers all of it, which is what is wanted here: a server
// that accepts the connection and then dribbles one byte a minute would otherwise
// hang the command forever, and `blueprint install` has no progress output that
// would make the hang visible.
const fetchTimeout = 30 * time.Second

// cmdBlueprintInstall implements `arxi blueprint install <ref> [--as name]`.
//
// The bytes are installed verbatim. That is the whole shape of this command and
// every decision below follows from it: what lands in agents/ is what the ref
// served, plus a comment header, so the digest printed on success describes the
// artifact rather than this program's idea of it. Re-rendering through Team would
// have dropped the watchers, timeouts and advance rules that are the reason to
// install somebody else's blueprint instead of composing one.
//
// The ref is a local path or an https URL, and nothing else. It is deliberately
// not a registry name: there is no registry, and accepting a bare word here would
// mean inventing a resolution rule now that a registry would have to keep later.
//
// Exit 2 is the invocation being wrong -- a ref that is not one of the two forms,
// an unusable --as, a name already taken. Exit 1 is the invocation being right and
// the artifact being wrong: unreachable, too large, not a valid blueprint. That is
// `blueprint validate`'s split, and it is the one a CI job needs.
func cmdBlueprintInstall(args []string) {
	c := surface.Lookup("blueprint", "install")
	vals, err := parseInvocation(c, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi blueprint install: %v\n\n%s", err, blueprintUsage)
		os.Exit(2)
	}

	ref := strings.TrimSpace(vals["ref"])
	as := vals["as"]

	// --as is checked before the ref is fetched, for the reason ValidateName's
	// comment gives: this command's only outward effect is a request to somebody
	// else's server, and an invocation that cannot succeed should not make it.
	if as != "" {
		if err := agentstore.ValidateName("blueprint", as); err != nil {
			fmt.Fprintf(os.Stderr, "arxi blueprint install: %v\n"+
				"  that name came from --as, which decides the filename, so it has to be\n"+
				"  a name this store can hold. nothing was fetched.\n", err)
			os.Exit(2)
		}
	}

	source, raw := readRef(ref)
	bp, err := blueprint.Load(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi blueprint install: %s is not a valid blueprint.\n\n%v\n", source, err)
		if looksLikeMarkup(raw) {
			// The single most common way this fails with a URL that "works" in a
			// browser: the page ABOUT the file was served instead of the file. The
			// loader's own message for that is a complaint about `<`, which reads
			// like a corrupt blueprint rather than the wrong URL.
			fmt.Fprint(os.Stderr, "\n  what came back starts with `<`, so this looks like HTML rather than YAML.\n"+
				"  a browser URL is usually a page about the file: look for a raw or\n"+
				"  download link and install that.\n")
		}
		fmt.Fprint(os.Stderr, "\n  nothing was written. the file was read and refused, not stored\n"+
			"  and then refused, so there is nothing in "+agentstore.DefaultDir+"/ to clean up.\n")
		os.Exit(1)
	}

	name := as
	if name == "" {
		name = bp.Name
		if name == "" {
			// No --as and no `name:`. Refusing beats deriving one from the ref: a
			// URL tail is a URL artifact, and installing `download` because the
			// path ended in `/download?id=7` names an agent after a query string.
			fmt.Fprintf(os.Stderr, "arxi blueprint install: %s declares no `name:`, and --as was not given\n"+
				"  the name becomes %s/<name>%s and is what `arxi run start <name>` resolves,\n"+
				"  so there is nothing here to call it. say so: --as <name>\n",
				source, agentstore.DefaultDir, ".yaml")
			os.Exit(2)
		}
		if err := agentstore.ValidateName("blueprint", name); err != nil {
			// Exit 1, not 2: the invocation was right and the fetched file is what
			// cannot be stored. --as is the way past it, and it does not require
			// editing bytes that a digest was recorded for.
			fmt.Fprintf(os.Stderr, "arxi blueprint install: %v\n"+
				"  that is the `name:` in %s, and it becomes the filename.\n"+
				"  install it under another name instead: --as <name>\n", err, source)
			os.Exit(1)
		}
	}

	path, err := openAgents().Install(name, []byte(provenanceHeader(source, bp.SHA)+string(raw)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi blueprint install: %v\n", err)
		if errors.Is(err, agentstore.ErrExists) {
			// The refusal `agent create` and `blueprint create` both give, and it
			// matters more here: the file that would have been replaced is one
			// somebody in this repository wrote, and the bytes replacing it came
			// from outside it.
			fmt.Fprintf(os.Stderr, "  nothing was written. read the one that is there: "+
				"arxi agent show %s\n  or install alongside it: --as %s-upstream\n", name, name)
			os.Exit(2)
		}
		os.Exit(1)
	}
	printInstalled(name, path, source, bp, as)
}

// readRef turns the one positional into bytes, and decides what a ref may be.
//
// The order is the grammar: a scheme wins over a path, because `https://x/y.yaml`
// is not a relative directory called `https:`. Everything without a scheme is a
// path, which keeps `./code-review.yaml`, `../vendor/x.yaml` and an absolute path
// working without a form of their own.
//
// It returns the source string the provenance header will record, which is not
// always the ref as typed: a relative path is made absolute, because a header
// saying `source: ../x.yaml` records a fact about a shell that has since moved on.
func readRef(ref string) (string, []byte) {
	if ref == "" {
		fmt.Fprintf(os.Stderr, "arxi blueprint install: the ref is empty\n\n%s", blueprintUsage)
		os.Exit(2)
	}
	if strings.IndexFunc(ref, unicode.IsControl) >= 0 {
		// A newline in the ref would be copied into the provenance header, where
		// the line after it no longer starts with `#` and stops being a comment.
		// Refused rather than escaped, so what the header records is what was
		// typed.
		fmt.Fprintf(os.Stderr, "arxi blueprint install: the ref contains a control character\n"+
			"  it is recorded in a comment at the top of the installed file, and a\n"+
			"  newline there would end the comment and start being blueprint.\n")
		os.Exit(2)
	}

	if _, _, ok := strings.Cut(ref, "://"); ok {
		return readURL(ref)
	}
	return readPath(ref)
}

// readPath reads a ref with no scheme.
//
// The interesting case is a name that is not a path at all. `arxi blueprint
// install code-review` is what somebody types who expects a registry, and the
// honest answer is that there is not one yet -- said in those words, because the
// alternative is a file-not-found error about `./code-review` that reads like a
// typo and sends them looking for the file.
func readPath(ref string) (string, []byte) {
	raw, err := os.ReadFile(ref)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && looksLikeARegistryName(ref) {
			fmt.Fprintf(os.Stderr, "arxi blueprint install: %q is not a path or a URL\n"+
				"  there is no registry to install %q from yet. a ref is one of two things:\n"+
				"    a file:      arxi blueprint install ./%s.yaml\n"+
				"    an https URL: arxi blueprint install https://example.com/%s.yaml\n"+
				"  to compose one from agents you already have: arxi blueprint create\n",
				ref, ref, ref, ref)
			os.Exit(2)
		}
		// Exit 1: the ref is a path, the path is the problem. Same code
		// `blueprint validate` gives for a file it cannot read.
		fmt.Fprintf(os.Stderr, "arxi blueprint install: %v\n", err)
		os.Exit(1)
	}

	// Absolute, so the header records a path that still means something from
	// another directory. Falling back to the ref as typed rather than failing:
	// Abs only errors when the working directory is gone, and refusing an install
	// over the spelling of a comment line would be the wrong trade.
	source := ref
	if abs, err := filepath.Abs(ref); err == nil {
		source = abs
	}
	return source, raw
}

// looksLikeARegistryName reports whether a missing path was probably meant as a
// registry name: one bare word, no separator, no YAML extension.
//
// Kept narrow on purpose. `./x.yaml` and `vendor/x.yaml` are paths that are simply
// not there, and answering those with a lecture about registries would be telling
// somebody who typed a path correctly that they typed the wrong kind of thing.
func looksLikeARegistryName(ref string) bool {
	if strings.ContainsAny(ref, `/\.`) {
		return false
	}
	return ref != ""
}

// readURL fetches a ref that has a scheme.
//
// https is the form. http is accepted only against loopback, and that exception
// exists for two reasons worth writing down: an attacker who can redirect your
// loopback traffic can already write agents/ directly, so plaintext buys them
// nothing there -- and it is what makes this command testable against httptest
// without a certificate, which is the difference between the fetch path having
// tests and having none. Every other scheme is refused by name, including file://,
// because a path already works and two spellings of the same thing is one more
// grammar to keep.
func readURL(ref string) (string, []byte) {
	u, err := url.Parse(ref)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi blueprint install: %q is not a URL: %v\n", ref, err)
		os.Exit(2)
	}
	if err := checkFetchURL(u); err != nil {
		fmt.Fprintf(os.Stderr, "arxi blueprint install: %v\n", err)
		os.Exit(2)
	}

	client := &http.Client{
		Timeout: fetchTimeout,
		// Every hop is checked, not just the one that was typed. Without this a
		// server answers the https URL with a 302 to http://elsewhere and the
		// blueprint that gets installed arrived in plaintext from a host nobody
		// named. The hop limit restates net/http's own default, which setting
		// CheckRedirect replaces: it is the existing behaviour spelled out, not a
		// new policy.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("stopped after 10 redirects")
			}
			return checkFetchURL(req.URL)
		},
	}

	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi blueprint install: %v\n", err)
		os.Exit(1)
	}
	req.Header.Set("User-Agent", "arxi/"+version)
	// Stated so a server that negotiates has something to go on. It does not stop
	// a page being served -- looksLikeMarkup handles that -- but it turns the
	// common case into the file rather than a preview of it.
	req.Header.Set("Accept", "application/yaml, text/yaml, text/plain;q=0.9, */*;q=0.1")

	resp, err := client.Do(req)
	if err != nil {
		// Exit 1: the URL was well formed and the fetch failed. A CI job reads
		// this as "the network or the server", not "the command line".
		fmt.Fprintf(os.Stderr, "arxi blueprint install: fetching %s: %v\n"+
			"  nothing was written.\n", u.Redacted(), err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "arxi blueprint install: %s answered %s\n", u.Redacted(), resp.Status)
		switch resp.StatusCode {
		case http.StatusNotFound:
			fmt.Fprint(os.Stderr, "  check the URL serves the YAML itself rather than a page about it.\n")
		case http.StatusUnauthorized, http.StatusForbidden:
			// Said plainly because the obvious next move is to look for a --token
			// flag that does not exist, and a private blueprint has a working
			// answer: fetch it however you already authenticate, then install the
			// file.
			fmt.Fprint(os.Stderr, "  this command sends no credentials. fetch it with a tool that has them\n"+
				"  and install the file: arxi blueprint install ./downloaded.yaml\n")
		}
		os.Exit(1)
	}

	// LimitReader takes one byte more than the cap so the overrun is detectable;
	// reading exactly the cap cannot tell a file that fits from one that was cut.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRefBytes+1))
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi blueprint install: reading %s: %v\n", u.Redacted(), err)
		os.Exit(1)
	}
	if len(raw) > maxRefBytes {
		fmt.Fprintf(os.Stderr, "arxi blueprint install: %s served more than %d bytes\n"+
			"  a blueprint is a few kilobytes, and the loader reads the whole document\n"+
			"  into memory, so the size of this command is otherwise decided by the\n"+
			"  server. download it and look at it before installing it.\n",
			u.Redacted(), maxRefBytes)
		os.Exit(1)
	}
	return u.Redacted(), raw
}

// checkFetchURL is the rule applied to the typed URL and to every redirect hop.
//
// One function for both, because a rule enforced only on the first URL is not a
// rule: the interesting fetch is the one that ends up somewhere else.
func checkFetchURL(u *url.URL) error {
	switch u.Scheme {
	case "https":
		if u.Host == "" {
			return fmt.Errorf("%q has no host", u.Redacted())
		}
		return nil
	case "http":
		if !isLoopback(u.Hostname()) {
			return fmt.Errorf("%s is plain http to %s\n"+
				"  a blueprint decides which tools an agent may call and what a run may\n"+
				"  spend, and over http anybody on the path can choose it. use https.\n"+
				"  http is accepted for loopback only, where there is no path to be on",
				u.Redacted(), u.Hostname())
		}
		return nil
	case "file":
		path := u.Path
		if path == "" {
			path = u.Opaque
		}
		return fmt.Errorf("file:// is not a ref; give the path itself: %s", path)
	case "":
		return fmt.Errorf("%q has no scheme", u.Redacted())
	default:
		return fmt.Errorf("%s is not a scheme this installs from; use https, or a path", u.Scheme)
	}
}

// isLoopback reports whether a host is unambiguously this machine.
//
// Literal addresses and the exact word localhost, and nothing else. Not
// `*.localhost`, which only some resolvers map to loopback, and not a name that
// happens to resolve to 127.0.0.1 today: the point of the exception is that no
// network is involved, and a hostname whose answer comes from DNS is a network.
func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// looksLikeMarkup reports whether the bytes begin like HTML or XML.
func looksLikeMarkup(raw []byte) bool {
	return strings.HasPrefix(strings.TrimSpace(string(raw)), "<")
}

// provenanceHeader records where the file came from, as YAML comments.
//
// The store records nothing about origin, and once written an installed blueprint
// is indistinguishable from one a colleague composed -- while being the one file
// in agents/ that nobody here wrote. `tools: [bash, write]` in it means something
// different from the same line in a file from `agent create`, and a reviewer
// reading the directory has no other way to know which is which.
//
// The digest is of the fetched bytes, computed before these lines exist, so the
// file's own SHA differs from what is recorded here. That is stated in the header
// rather than worked around: the digest is there to be compared against a re-fetch
// or against what a publisher advertises, and rewriting it after prepending
// comments would make it a digest of nothing anybody else can produce.
//
// Comments, so the loader strips them, the reducer never sees them, and a user can
// edit or delete the block. TestAnInstalledFileIsAnOrdinaryBlueprintTheUserCanEdit
// pins that a comment block above `name:` parses, so this cannot quietly become a
// file `run start` refuses. readRef refuses a control character in the ref, which
// is what keeps every line here starting with `#`.
func provenanceHeader(source, sha string) string {
	return "# installed by arxi blueprint install -- this file was written elsewhere.\n" +
		"# source:    " + source + "\n" +
		"# sha256:    " + sha + "\n" +
		"# installed: " + time.Now().UTC().Format(time.RFC3339) + "\n" +
		"# the digest is of what was fetched, not of this file: these comment lines\n" +
		"# were added on the way in. edit the blueprint freely -- keeping the header\n" +
		"# is what lets somebody compare it against the source later.\n" +
		"\n"
}

// printInstalled reports what was installed and what running it would do.
//
// It prints the resolved config rather than echoing the flags, for `blueprint
// validate`'s reason and one more: with `agent create` the user chose the model and
// the tools, so a report is a confirmation. Here they chose a URL. The tool grants,
// the stage list, the watchers and the workspace are all somebody else's decisions,
// and this screen is the first and possibly only place they are read.
func printInstalled(name, path, source string, bp *blueprint.Blueprint, as string) {
	c := bp.Config
	fmt.Printf("blueprint %s installed: %d member%s, %d stage%s\n",
		name, len(c.Members), plural(len(c.Members)), len(c.Stages), plural(len(c.Stages)))
	fmt.Printf("  file:   %s\n", path)
	fmt.Printf("  from:   %s\n", source)
	fmt.Printf("  sha256: %s  (of what was fetched, before the header)\n", shortSHA(bp.SHA))

	if as != "" && bp.Name != "" && bp.Name != as {
		// The two names now differ, and both are load-bearing: `agent show` prints
		// the same difference and says which command uses which. Silence here would
		// leave the operator to discover it when a run is named after something
		// they did not install.
		fmt.Printf("  note:   the file declares `name: %s` and --as installed it as %s.\n"+
			"          the line inside the file was not rewritten, so the digest above still\n"+
			"          describes what was fetched: `run start %s` resolves the filename, and\n"+
			"          the run records %s. arxi agent show %s says the same thing.\n",
			bp.Name, name, name, bp.Name, name)
	}

	for _, m := range c.Members {
		fmt.Printf("  - %s: %s\n", m.Name, grantSummary(m.Tools, overridesFor(m.Name)))
		detail := "advisory"
		if !m.Advisory {
			detail = "counts toward advance"
		}
		if m.Role != "" {
			detail = m.Role + ", " + detail
		}
		fmt.Printf("      %s, %s\n", dash(m.Model), detail)
	}

	// Watchers are shown for `blueprint validate`'s reason -- they are the only
	// declaration that acts without a member being activated -- and an installed
	// file is where that matters most, because nobody here wrote the patterns.
	for _, w := range c.Watchers {
		action := w.Action
		if action == "" {
			action = "activate"
		}
		fmt.Printf("  watcher %s on %s: %s  (fires on a matching event, unprompted)\n",
			w.Agent, w.Pattern, action)
	}

	if len(c.Stages) == 0 {
		// Handled before the shared caveats, which name stages[0]: ResolveDefaults
		// synthesises no stage, so this is a real file and not a defensive branch.
		// applyRunStarted returns nil rather than entering one, so the run starts,
		// activates nobody and goes quiescent.
		fmt.Printf("  caveat: no stages, so `arxi run start %s` enters none, activates nobody\n"+
			"          and goes quiescent after zero turns. add `stages:` with at least one\n"+
			"          name, then arxi blueprint validate %s\n", name, path)
		return
	}

	var stages []string
	for _, st := range c.Stages {
		stages = append(stages, st.Name)
	}
	fmt.Printf("  stages: %s  (in order)\n", strings.Join(stages, " -> "))
	fmt.Printf("  workspace: %s  (%s)\n", c.Workspace, workspaceReason(c))

	if writers := writeCapableMembers(c); len(writers) > 0 {
		// The sentence this command exists to make somebody read. A tool grant in a
		// file from `agent create` was typed by the person running it; the same
		// grant here was chosen by whoever published the ref.
		fmt.Printf("  caveat: %s can change files or run commands, and nobody here wrote this\n"+
			"          blueprint. read it before it runs: arxi agent show %s\n",
			strings.Join(writers, ", "), name)
	}

	printTeamCaveats(name, c.Members, stages)
}
