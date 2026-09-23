package internal_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"
)

// The family's atomic write closes the temp file before it renames it: write,
// fsync, close, then chmod and os.Rename. Closing first is not housekeeping --
// it is a portability correctness requirement. On Windows os.Rename refuses a
// file that is still open, failing with a sharing violation (ERROR_SHARING_
// VIOLATION, syscall.Errno(32)), the very error logstore/pending_remove_windows.go
// already retries around for pending markers. Rename the temp while its handle
// is open and the publish never completes on Windows, while every test on a
// POSIX runner -- where an open file renames fine -- stays green. That asymmetry
// is exactly the kind that hides: correct on the developer's Linux CI, broken on
// the user's machine.
//
// ADR-0037 derives the fsync order (file synced before the directory) and
// ADR-0039 derives that the temp is created in the destination's directory, but
// neither holds the close before the rename: Close and Sync are distinct
// operations (Close releases the OS handle; Sync flushes bytes to disk), and a
// rename-before-close bug changes no durability property, only whether the
// publish runs at all on Windows.
//
// closeOrderViolation names one publish whose temp is not closed before it is
// renamed, in terms a reader can act on without opening the analyzer.
type closeOrderViolation struct {
	fn     string
	reason string
}

// analyzeCloseBeforeRename parses one store source file and holds every
// atomic-rename publish in it -- a function that both creates a temp file and
// renames it -- to closing that temp before the rename.
//
// It classifies calls by selector name, the way the durability-order analyzer
// does: os.CreateTemp and os.Rename by their package-qualified names, and the
// temp's Close by the ".Close()" selector. Every close in a correct publish --
// the error-path closes after a failed Write or Sync, and the final close on the
// success path -- sits textually before the rename, so requiring the LAST close
// to precede the FIRST rename holds the whole function to "closed before
// renamed" and flags a rename moved ahead of the close.
//
// publishChecked counts the publish functions actually examined so the caller
// can fail closed when the corpus scan finds none -- the case where the write
// moved or the calls were renamed and this guard silently began holding over an
// empty set.
func analyzeCloseBeforeRename(filename string, src []byte) (publishChecked int, violations []closeOrderViolation, parseErr error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return 0, nil, err
	}
	// isPkgCall reports whether call is `pkg.name(...)` -- e.g. os.CreateTemp or
	// os.Rename -- so a method named Rename on some type is never mistaken for
	// the standard-library call the atomic publish uses.
	isPkgCall := func(call *ast.CallExpr, pkg, name string) bool {
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != name {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		return ok && id.Name == pkg
	}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		var haveTemp, haveRename bool
		var closes, renames []token.Pos
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch {
			case isPkgCall(call, "os", "CreateTemp"):
				haveTemp = true
			case isPkgCall(call, "os", "Rename"):
				haveRename = true
				renames = append(renames, call.Pos())
			case sel.Sel.Name == "Close":
				closes = append(closes, call.Pos())
			}
			return true
		})
		// A function without both a temp and a rename is not this pattern -- it
		// is some other move, or a create with no publish, and the close order
		// this guard is about does not apply to it.
		if !haveTemp || !haveRename {
			continue
		}
		publishChecked++
		if len(closes) == 0 {
			violations = append(violations, closeOrderViolation{
				fn:     fn.Name.Name,
				reason: "renames its temp file without ever closing it, so on Windows os.Rename fails with a sharing violation on the still-open handle and the publish never completes",
			})
			continue
		}
		if maxPos(closes) >= minPos(renames) {
			violations = append(violations, closeOrderViolation{
				fn:     fn.Name.Name,
				reason: "renames its temp file before closing it, so on Windows the open handle makes os.Rename fail with a sharing violation and the publish never completes",
			})
		}
	}
	return publishChecked, violations, nil
}

// TestEveryStoreRenamePublishClosesTheTempBeforeRenaming holds the one step of
// the family's atomic write that is a portability requirement rather than a
// durability one: the temp file is closed before it is renamed.
//
// # The defect this was written for
//
// Every rename store writes the same sequence -- os.CreateTemp, write, fsync,
// close, chmod, os.Rename -- and agentstore's comment calls it "toolstore's, for
// the same reasons", a cross-package claim about the whole family. Probed at its
// widest point, the close-before-rename step is correct in all of them, and in
// logstore's snapshot too. But nothing guarded it. Moving the rename ahead of
// the close changes no durability property, so ADR-0037's fsync-order guard
// stays green, and on a POSIX runner -- where renaming an open file is fine --
// the whole suite stays green. On Windows the same code fails with a sharing
// violation (ERROR_SHARING_VIOLATION, syscall.Errno(32)) and the publish never
// completes; the repo already retries around exactly that error for pending
// markers in logstore/pending_remove_windows.go, so it is a demonstrated, live
// constraint here, not a hypothetical. A guard that passes on Linux while the
// user's Windows machine cannot save a role is the asymmetry this exists to
// catch. This is the shape ADR-0035 through ADR-0039 recorded: a family-wide
// property carried by the shape of a call and pinned by no derived guard.
//
// # Why the subject is derived
//
// The property lives in each store's own close and rename, duplicated across the
// family, so the guard reads those calls from source and holds the last close
// before the first rename, rather than trusting the sequence to stay right. A
// store added later, or an existing publish whose rename is moved ahead of the
// close, is caught the first time this runs -- including on the Linux CI where
// the runtime itself would not.
//
// The corpus is the rename-publishers, the same measured boundary as ADR-0039:
// every store that publishes through CreateTemp+Rename, which includes
// logstore's snapshot and excludes memorystore (an O_EXCL create with no rename,
// ADR-0034) and jobstore (an append-only journal, no CreateTemp+Rename).
func TestEveryStoreRenamePublishClosesTheTempBeforeRenaming(t *testing.T) {
	// Prove the analyzer fires before trusting a clean corpus result: the
	// correct order (close, then rename) must pass, including a publish with
	// error-path closes before the success-path one; a rename moved ahead of the
	// close must fail; and a rename with no close at all must fail closed.
	// Without this, a clean scan below could mean the analyzer stopped
	// recognizing the calls rather than that the stores are correct.
	const closeThenRename = `package p
func write() error {
	tmp, _ := os.CreateTemp(dir, "x-*")
	if _, err := tmp.Write(nil); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}`
	const renameThenClose = `package p
func write() error {
	tmp, _ := os.CreateTemp(dir, "x-*")
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return err
	}
	return tmp.Close()
}`
	const noClose = `package p
func write() error {
	tmp, _ := os.CreateTemp(dir, "x-*")
	return os.Rename(tmp.Name(), dst)
}`
	if c, v, err := analyzeCloseBeforeRename("good.go", []byte(closeThenRename)); err != nil || c != 1 || len(v) != 0 {
		t.Fatalf("analyzer flagged a correct close-before-rename publish (checked=%d, violations=%d, err=%v).\n"+
			"  Consequence: every store, which closes before renaming, would be reported as "+
			"broken. Remedy: require the last Close to precede the first os.Rename, so error-path "+
			"closes before the success-path close do not trip it.", c, len(v), err)
	}
	if c, v, err := analyzeCloseBeforeRename("bad.go", []byte(renameThenClose)); err != nil || c != 1 || len(v) == 0 {
		t.Fatalf("analyzer did not flag a rename-before-close publish (checked=%d, violations=%d, err=%v).\n"+
			"  Consequence: the corpus scan below would report success whether or not a store "+
			"renamed its temp while the handle was still open -- the Windows sharing-violation "+
			"bug this exists to catch. Remedy: re-derive analyzeCloseBeforeRename from the Close "+
			"and os.Rename calls the stores make.", c, len(v), err)
	}
	if c, v, err := analyzeCloseBeforeRename("none.go", []byte(noClose)); err != nil || c != 1 || len(v) == 0 {
		t.Fatalf("analyzer did not fail closed on a rename with no close (checked=%d, violations=%d, err=%v).\n"+
			"  Consequence: a publish that never closes its temp would pass. Remedy: report a "+
			"rename-publish that contains no Close call at all.", c, len(v), err)
	}

	checked := 0
	for _, pkg := range storePackageDirs(t) {
		for _, file := range storeSourceFiles(t, pkg) {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("cannot read %s: %v", file, err)
			}
			n, violations, err := analyzeCloseBeforeRename(file, raw)
			if err != nil {
				t.Fatalf("cannot parse %s: %v", file, err)
			}
			checked += n
			for _, v := range violations {
				t.Errorf("%s: %s %s.\n"+
					"  Every store in this family that publishes by renaming a temp into place "+
					"closes that temp first, so os.Rename never operates on an open handle and the "+
					"publish completes on Windows as well as on POSIX. This function breaks that.\n"+
					"  Remedy: close the temp file (checking the error) before os.Rename, as the "+
					"rest of the family does.", displayPath(file), v.fn, v.reason)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no os.CreateTemp+os.Rename publish was found in any store.\n" +
			"  Every rename store published this way when this test was written, so finding none " +
			"means the write moved or the calls were renamed and this check now holds over an " +
			"empty set. Remedy: re-derive analyzeCloseBeforeRename from the form the stores now use.")
	}
}
