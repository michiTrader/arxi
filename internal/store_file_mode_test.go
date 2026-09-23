package internal_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The integrity floor a published record file must satisfy, expressed as three
// bit tests over the permission bits: the owner can read and write it, nobody
// can execute it, and neither group nor other can write it. Both modes the
// family actually uses -- 0644 for the files a team reads and 0600 for the one
// that maps a credential -- clear it; 0755 (executable), 0664 (group-writable)
// and 0666 (world-writable) do not.
//
// The floor guards integrity, not secrecy: it does not distinguish 0644 from
// 0600, so it does not by itself hold modelstore's credential file to 0600.
// That read-secrecy choice is a per-store decision guarded by modelstore's own
// test; what every store shares -- and what this derives over the family -- is
// that a stored record is data the owner writes, never a program and never a
// file some other local account may rewrite.
const (
	modeOwnerReadWrite  = 0o600
	modeAnyExecute      = 0o111
	modeGroupOtherWrite = 0o022
)

// fileModeViolation names one publish that sets a published record file to a
// mode outside the integrity floor, in terms a reader can act on without
// opening the analyzer.
type fileModeViolation struct {
	fn     string
	mode   string
	reason string
}

// analyzeFileModes parses one store source file and reports the mode every
// file-creating call in it stamps on the file, holding each to the floor.
//
// It classifies calls by selector name, the way the durability-order analyzer
// does: os.Chmod's second argument, os.WriteFile's third, and os.OpenFile's
// third are the mode a created file receives. os.OpenFile is counted only when
// its flags contain O_CREATE, because without it the OS ignores the perm
// argument -- reading a read path's 0 as a mode would flag it for a mode it
// never sets. os.MkdirAll and os.Mkdir are deliberately not matched: 0755 is
// correct for a directory and would fail the no-execute test written for files.
//
// modesChecked counts the modes actually evaluated so the caller can fail
// closed per store when a store that declares `const ext` yields none -- the
// case where the chmod moved or was renamed and this guard silently began
// holding over an empty set.
func analyzeFileModes(filename string, src []byte) (modesChecked int, violations []fileModeViolation, parseErr error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return 0, nil, err
	}
	nodeText := func(n ast.Node) string {
		return string(src[fset.Position(n.Pos()).Offset:fset.Position(n.End()).Offset])
	}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			var modeArg ast.Expr
			switch sel.Sel.Name {
			case "Chmod":
				if len(call.Args) >= 2 {
					modeArg = call.Args[1]
				}
			case "WriteFile":
				if len(call.Args) >= 3 {
					modeArg = call.Args[2]
				}
			case "OpenFile":
				if len(call.Args) >= 3 && strings.Contains(nodeText(call.Args[1]), "O_CREATE") {
					modeArg = call.Args[2]
				}
			}
			if modeArg == nil {
				return true
			}
			modesChecked++
			lit, ok := modeArg.(*ast.BasicLit)
			if !ok || lit.Kind != token.INT {
				violations = append(violations, fileModeViolation{
					fn:     fn.Name.Name,
					mode:   nodeText(modeArg),
					reason: "sets the published file's mode from an expression this guard cannot evaluate, so the mode it ships is invisible to review",
				})
				return true
			}
			m, perr := strconv.ParseInt(lit.Value, 0, 64)
			if perr != nil {
				violations = append(violations, fileModeViolation{
					fn:     fn.Name.Name,
					mode:   lit.Value,
					reason: "sets a mode literal this guard cannot parse",
				})
				return true
			}
			var reason string
			switch {
			case m&modeOwnerReadWrite != modeOwnerReadWrite:
				reason = "publishes a file the owner cannot both read and write, so the store cannot round-trip its own record"
			case m&modeAnyExecute != 0:
				reason = "publishes an executable file, but a stored record is data, not a program"
			case m&modeGroupOtherWrite != 0:
				reason = "publishes a file writable by group or other, so any other local account can rewrite a stored record behind the owner's back"
			}
			if reason != "" {
				violations = append(violations, fileModeViolation{fn: fn.Name.Name, mode: lit.Value, reason: reason})
			}
			return true
		})
	}
	return modesChecked, violations, nil
}

// TestEveryStorePublishSetsAnIntegrityPreservingFileMode holds the file mode
// the family stamps on a published record -- claimed in prose across the stores
// ("A role is not a secret", "An agent definition is not a secret", "0600 and
// not 0644, unlike a trigger") -- to every content store's actual write, rather
// than to those sentences.
//
// # The defect this was written for
//
// The stores decide the published mode by hand and explain it in comments that
// reference each other: rolestore and agentstore chmod 0644 because the record
// "is not a secret", and modelstore chmods 0600 "and not 0644, unlike a trigger"
// because the file maps a credential. Probed at its widest point -- every
// content store in the family -- the behaviour is correct: rolestore,
// agentstore, toolstore, trigstore and evalstore publish 0644, modelstore
// publishes 0600, and memorystore's O_EXCL create asks for 0644.
//
// But the family invariant was guarded unevenly. Four stores had a mode
// assertion in their own test (agentstore, trigstore, modelstore, rolestore);
// toolstore, evalstore and memorystore had none, and nothing derived the rule
// over the corpus, so a store added later, or one whose chmod was widened to
// 0666 or dropped for an umask-honouring os.Create, was caught by no test at
// all. This is the shape ADR-0035, ADR-0036 and ADR-0037 recorded: a family
// property argued in comments, generalised to N packages, and pinned by a hand
// list that goes stale in the store nobody added to it.
//
// # Why the subject is derived, and what it guards
//
// The guard reads the mode each store's own publish stamps on the file -- the
// second argument of os.Chmod, the third of os.OpenFile with O_CREATE -- and
// holds it to an integrity floor: owner read/write, never executable, never
// writable by group or other. That floor is what every store shares regardless
// of the 0644-vs-0600 split, and it is the security-load-bearing part: a record
// writable by another local account is a tool policy or an agent definition that
// account can rewrite behind the owner's back. The floor deliberately does not
// separate 0600 from 0644, so it does not restate modelstore's read-secrecy
// choice -- that stays modelstore's own test's job; stating the narrower scope
// here is the point, not a gap.
//
// The corpus is the same family as ADR-0035, ADR-0036 and ADR-0037: the
// `internal/*store` packages that declare `const ext`. jobstore and logstore do
// not, and publish through a different shape; they are out of this guard's scope
// by that boundary, not by exemption.
func TestEveryStorePublishSetsAnIntegrityPreservingFileMode(t *testing.T) {
	// Prove the analyzer fires before trusting a clean corpus result. A
	// group/other-writable chmod must be flagged; a 0644 chmod and an O_CREATE
	// OpenFile at 0644 must pass; and a read-only OpenFile must contribute no
	// mode at all, so its ignored perm argument is never read as a violation.
	// Without this, a clean result below could mean the analyzer stopped
	// recognising the calls rather than that the stores are correct.
	const badMode = `package p
func write() error {
	tmp, _ := os.CreateTemp(dir, "x-*")
	return os.Chmod(tmp.Name(), 0o666)
}`
	const goodMode = `package p
func write() error {
	tmp, _ := os.CreateTemp(dir, "x-*")
	return os.Chmod(tmp.Name(), 0o644)
}`
	const openCreate = `package p
func write() error {
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	_ = f
	return nil
}`
	const openRead = `package p
func load() error {
	f, _ := os.OpenFile(path, os.O_RDONLY, 0)
	_ = f
	return nil
}`
	if c, v, err := analyzeFileModes("bad.go", []byte(badMode)); err != nil || c != 1 || len(v) == 0 {
		t.Fatalf("analyzer did not flag a group/other-writable publish (checked=%d, violations=%d, err=%v).\n"+
			"  Consequence: the corpus scan below would report success whether or not a store "+
			"widened its published mode. Remedy: re-derive analyzeFileModes from the calls the "+
			"stores now make.", c, len(v), err)
	}
	if c, v, err := analyzeFileModes("good.go", []byte(goodMode)); err != nil || c != 1 || len(v) != 0 {
		t.Fatalf("analyzer flagged a correct 0644 publish (checked=%d, violations=%d, err=%v).\n"+
			"  Consequence: every store would be reported as broken. Remedy: re-derive "+
			"analyzeFileModes so a floor-clearing mode passes.", c, len(v), err)
	}
	if c, v, err := analyzeFileModes("open.go", []byte(openCreate)); err != nil || c != 1 || len(v) != 0 {
		t.Fatalf("analyzer did not evaluate the mode of an O_CREATE OpenFile (checked=%d, violations=%d, err=%v).\n"+
			"  Consequence: memorystore's create, its only mode-setting call, would go unchecked. "+
			"Remedy: count os.OpenFile's perm argument when its flags contain O_CREATE.", c, len(v), err)
	}
	if c, v, err := analyzeFileModes("read.go", []byte(openRead)); err != nil || c != 0 || len(v) != 0 {
		t.Fatalf("analyzer read a mode from a non-creating OpenFile (checked=%d, violations=%d, err=%v).\n"+
			"  Consequence: every read path passing perm 0 would be flagged for a mode it never "+
			"sets. Remedy: evaluate OpenFile's perm only when its flags contain O_CREATE.", c, len(v), err)
	}

	checked := 0
	for _, pkg := range storePackageDirs(t) {
		files := storeSourceFiles(t, pkg)
		declaresExt := false
		srcs := make(map[string][]byte, len(files))
		for _, file := range files {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("cannot read %s: %v", file, err)
			}
			srcs[file] = raw
			if extConst.Match(raw) {
				declaresExt = true
			}
		}
		if !declaresExt {
			continue
		}
		storeModes := 0
		for _, file := range sortedKeys(srcs) {
			n, violations, err := analyzeFileModes(file, srcs[file])
			if err != nil {
				t.Fatalf("cannot parse %s: %v", file, err)
			}
			storeModes += n
			checked += n
			for _, v := range violations {
				t.Errorf("%s: %s %s (mode %s).\n"+
					"  Every content store in this family publishes a record the owner reads and "+
					"writes, never an executable and never a file another local account may rewrite. "+
					"This function breaks that floor.\n"+
					"  Remedy: publish the record with 0644 (0600 where it maps a credential), the "+
					"modes the family uses.", displayPath(file), v.fn, v.reason, v.mode)
			}
		}
		if storeModes == 0 {
			t.Errorf("%s declares `const ext` but no publish in it sets a file mode this guard can see.\n"+
				"  Consequence: this store's published mode is now unguarded -- the chmod or create "+
				"moved or was renamed, and the floor check holds over an empty set for it. Remedy: "+
				"re-derive analyzeFileModes from the call the store now uses to stamp the mode.", displayPath(pkg))
		}
	}
	if checked == 0 {
		t.Fatal("no file-mode-setting call was found in any store declaring `const ext`.\n" +
			"  Every content store stamped a mode on its published record when this test was written, " +
			"so finding none means the calls moved or were renamed and this check now holds over an " +
			"empty set. Remedy: re-derive analyzeFileModes from the form the stores now use.")
	}
}
