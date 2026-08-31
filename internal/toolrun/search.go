package toolrun

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// maxMatches caps how many hits one grep may report.
//
// The same reason as maxReadBytes, arrived at from the other end: a grep for "e"
// across a source tree matches nearly every line, and tool output becomes an
// event in the run log. The cap is on MATCHES rather than bytes because that is
// the number the model is reasoning about -- "200 matches, showing the first 50"
// is a fact it can act on, where "truncated at 256 KiB" tells it nothing about
// how much it did not see.
const maxMatches = 200

// maxGrepFiles caps how many files one grep may open.
//
// A separate limit from maxMatches, because the failure it prevents is different:
// a pattern that matches nothing still walks the whole tree, so a cap on matches
// alone would let a no-match grep spend unbounded time in a workspace somebody
// unpacked a dependency tree into.
const maxGrepFiles = 2000

// grepResult is one matching line.
type grepMatch struct {
	Path string
	Line int
	Text string
}

// Grep searches the workspace for a regular expression.
//
// # Regexp and not a substring
//
// `grep` is a name with a meaning, and a model that has been told it has grep
// will send it `func \w+\(` sooner or later. Accepting the pattern as a literal
// substring would find nothing, report success, and teach the model that the
// file does not contain what it does contain -- a wrong answer delivered as a
// confident one, which is the failure mode this whole package's error messages
// are written to avoid.
//
// The pattern is compiled BEFORE the walk, so a bad pattern is a refusal rather
// than an empty result. `regexp` is Go's RE2: it has no backreferences and no
// lookahead, and cannot backtrack exponentially. That last property is why no
// timeout is needed here, unlike bash.
//
// # The walk respects the boundary the same way a read does
//
// Every candidate goes through ReadFile, which means Resolve plus openNoFollow,
// which means a symlink pointing out of the workspace is refused at the open
// rather than followed. That is the control, and it is refused in the kernel:
// measured, ReadFile on a link to a file outside says "refused in the kernel by
// O_NOFOLLOW", and a path THROUGH a linked directory is refused by Resolve
// before any open happens.
//
// The walk also skips symlinked entries, and an earlier version of this comment
// claimed that skip was what kept a link to / from putting the filesystem in
// scope. It is not, and the claim was found by breaking the skip and watching
// the test that guards the boundary still pass. Two measurements say why:
// filepath.WalkDir does not descend into symlinked directories at all (it
// yields the link as one non-directory entry), and the entry it does yield is
// refused by ReadFile. The skip is therefore an optimisation -- it avoids an
// open that would fail -- and the boundary is held by controls that have their
// own tests in file_test.go and workspace_test.go. Documenting it as the
// control would have pointed the next reader at the wrong line to be careful
// with.
//
// Binary files are skipped rather than matched. A NUL byte in the first 8 KiB is
// the test, which is what git uses, and the reason is the log again: a match
// inside a compiled object dumps bytes nobody can read into the run history.
func (w *Workspace) Grep(pattern, sub string) ([]grepMatch, bool, error) {
	if strings.TrimSpace(pattern) == "" {
		return nil, false, fmt.Errorf("toolrun: %s gave an empty grep pattern\n"+
			"  an empty pattern matches every line of every file, which is a way "+
			"of asking for the whole workspace rather than searching it", w.Member)
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, false, fmt.Errorf("toolrun: %s gave a grep pattern that does "+
			"not compile: %w\n"+
			"  the pattern is a regular expression (RE2: no backreferences, no "+
			"lookahead)\n"+
			"  to search for it literally, escape it: %s",
			w.Member, err, regexp.QuoteMeta(pattern))
	}

	// A subdirectory is resolved through the same gate as a file path, so
	// `grep --path ../..` is refused here rather than walked.
	root := w.Root
	if strings.TrimSpace(sub) != "" {
		full, err := w.Resolve(sub)
		if err != nil {
			return nil, false, err
		}

		// Resolve permits a final component that does not exist yet, which is
		// correct for a WRITE and wrong for a search. Measured: `path: nope-dir`
		// reported "no matches", and so did a path naming a symlink out of the
		// workspace -- WalkDir on a missing root calls the callback once with an
		// error, which this function deliberately swallows so one unreadable
		// subtree cannot fail a whole grep.
		//
		// So a search that could not happen reported that it happened and found
		// nothing. That is the same defect this file's own Edit refuses to
		// commit: a tool reporting an answer it did not obtain. The model would
		// conclude the string is absent from a directory it never opened.
		st, serr := os.Lstat(full)
		switch {
		case serr != nil:
			return nil, false, fmt.Errorf("toolrun: %s: %s does not exist in its "+
				"workspace\n"+
				"  refusing rather than reporting \"no matches\": a search that "+
				"could not happen must not look like one that found nothing",
				w.Member, sub)
		case st.Mode()&os.ModeSymlink != 0:
			// Refused, not followed, and not silently skipped either. Following
			// would put whatever it points at in scope; skipping would be the
			// silent-empty-answer bug again, one level down.
			return nil, false, fmt.Errorf("toolrun: %s: %s is a symlink, so it is "+
				"not searched\n"+
				"  a link is followed nowhere in this package: what it points at "+
				"is not bounded by the workspace, and the boundary is the only "+
				"reason a tool may run unattended", w.Member, sub)
		case !st.IsDir():
			// A single file IS searchable and this is not an error -- it is the
			// obvious reading of `grep pattern path=a.go`, and WalkDir handles a
			// file root correctly.
		}

		root = full
	}

	var out []grepMatch
	files := 0
	truncated := false

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory that cannot be read is skipped, not fatal. A workspace
			// with one unreadable subtree should still be searchable, and the
			// alternative reports "permission denied" as though the whole grep
			// had failed.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if len(out) >= maxMatches || files >= maxGrepFiles {
			truncated = true
			return filepath.SkipAll
		}

		if d.IsDir() {
			return nil
		}
		// Type()&Symlink rather than Stat: WalkDir does not follow links, so this
		// is the link itself. This is an optimisation and not the confinement --
		// see the doc above. ReadFile refuses the same entry a line later; this
		// just avoids the open.
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if !d.Type().IsRegular() {
			// A fifo would block the read forever; a device would return
			// content forever. Neither is a file an agent meant to search.
			return nil
		}

		rel, rerr := filepath.Rel(w.Root, path)
		if rerr != nil {
			return nil
		}

		// Read through the workspace's own gate, so the confinement rules are
		// the ones ReadFile already enforces rather than a second copy of them.
		data, rerr := w.ReadFile(rel)
		if rerr != nil {
			// An oversized or unreadable file is skipped. It is NOT reported as a
			// match and not reported as an error: a grep that fails because one
			// file in the tree is 2 MiB has told the caller nothing about the
			// other files, which is worse than an answer with a gap in it.
			return nil
		}
		files++

		if isBinary(data) {
			return nil
		}

		for i, line := range strings.Split(string(data), "\n") {
			if len(out) >= maxMatches {
				truncated = true
				return filepath.SkipAll
			}
			if re.MatchString(line) {
				out = append(out, grepMatch{Path: rel, Line: i + 1, Text: line})
			}
		}
		return nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("toolrun: %s searching %s: %w", w.Member, root, err)
	}

	// Sorted, because filepath.WalkDir's order is lexical per directory but the
	// interleaving across a tree is not something a caller should depend on, and
	// a model diffing two greps needs the same input to produce the same output.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Line < out[j].Line
	})
	return out, truncated, nil
}

// isBinary reports whether data looks like something a human would not read.
//
// A NUL in the first 8 KiB, which is git's test. It is a heuristic and stated as
// one: the alternative is charset detection, which would be wrong more often and
// in less predictable ways.
func isBinary(data []byte) bool {
	n := len(data)
	if n > 8<<10 {
		n = 8 << 10
	}
	for i := 0; i < n; i++ {
		if data[i] == 0 {
			return true
		}
	}
	return false
}

// Edit replaces occurrences of old with new in a file inside the workspace.
//
// # Why not "write the whole file"
//
// `write` already exists and takes whole contents. `edit` earns its place by
// being the operation that does NOT require the model to reproduce the parts it
// is not changing -- which is where a whole-file write goes wrong: the model
// re-emits 300 lines from memory to change one, and quietly drops a line it
// never looked at. An edit that names what it is replacing cannot do that.
//
// # A replacement that matches nothing is an ERROR
//
// This is the decision in this function worth arguing about, and it goes against
// the shape of the rest of the file tools, which are permissive about
// vocabulary. It is an error because the alternative is silent success on a
// no-op: the model believes the file now says something it does not, continues
// on that belief, and the run's later steps are all built on it. A tool that
// reports "edited" without editing is the same class of defect as a grep that
// reports no matches in a file full of them.
//
// # An ambiguous replacement is also an error
//
// If old appears more than once and the caller did not say `all`, the edit is
// refused rather than applied to the first hit. "The first one" is a position in
// a file the model cannot see, so which occurrence it got would be luck, and a
// tool that silently picks one is a tool whose result nobody can predict. The
// caller is told how many there are, which is the fact it needs to decide
// between narrowing the pattern and asking for all of them.
func (w *Workspace) Edit(path, old, new string, all bool) (int, error) {
	if old == "" {
		return 0, fmt.Errorf("toolrun: %s gave an empty string to replace in %s\n"+
			"  an empty match is at every position in the file, so this would "+
			"insert between every character rather than edit anything\n"+
			"  to replace the whole file, use write", w.Member, path)
	}

	data, err := w.ReadFile(path)
	if err != nil {
		return 0, err
	}

	n := strings.Count(string(data), old)
	switch {
	case n == 0:
		// The refusal shows the string that was not found, quoted, because the
		// usual cause is whitespace: a model that reconstructed a line from its
		// own earlier output has the indentation wrong, and a message that says
		// only "not found" sends it to re-read the file rather than to look at
		// what it sent.
		return 0, fmt.Errorf("toolrun: %s: %s does not contain %q\n"+
			"  nothing was written: an edit that matched nothing would report "+
			"success for a file it did not change, and every later step would be "+
			"built on the belief that it had\n"+
			"  check whitespace and indentation first: that is the usual cause",
			w.Member, path, old)
	case n > 1 && !all:
		return 0, fmt.Errorf("toolrun: %s: %s contains %d occurrences of %q\n"+
			"  nothing was written: \"the first one\" is a position this caller "+
			"cannot see, so which one got edited would be luck\n"+
			"  include more surrounding text to name one, or pass all=true to "+
			"replace every occurrence", w.Member, path, n, old)
	}

	count := n
	if !all {
		count = 1
	}
	updated := strings.Replace(string(data), old, new, count)

	if err := w.WriteFile(path, []byte(updated)); err != nil {
		return 0, err
	}
	return count, nil
}

// formatGrep renders matches as the text a model will read next.
//
// path:line:text, which is grep's own format, because it is the format every
// tool that consumes grep output already parses -- including the model, which
// has seen millions of lines of it. Inventing a nicer one would be a private
// convention with no readers.
func formatGrep(matches []grepMatch, truncated bool, pattern string) string {
	if len(matches) == 0 {
		// Stated as a result, not as an absence. "no matches" answers the
		// question; an empty string looks like the tool failed to run, and a
		// model that cannot tell those apart will retry a search that worked.
		return fmt.Sprintf("no matches for %s\n", pattern)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d match", len(matches))
	if len(matches) != 1 {
		b.WriteString("es")
	}
	fmt.Fprintf(&b, " for %s\n\n", pattern)

	for _, m := range matches {
		// Trimmed on the right only. Leading whitespace is indentation, which is
		// meaning in most of the files an agent edits -- and it is exactly what
		// the caller needs to get right for a later `edit` to match.
		fmt.Fprintf(&b, "%s:%d:%s\n", m.Path, m.Line, strings.TrimRight(m.Text, " \t\r"))
	}

	if truncated {
		fmt.Fprintf(&b, "\n[stopped at %d matches: there may be more, and a run log "+
			"nobody can open is the artefact this project exists to produce]\n"+
			"  narrow the pattern, or search one directory with path\n", maxMatches)
	}
	return b.String()
}
