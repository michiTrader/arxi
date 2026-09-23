package internal_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"
)

// The family's atomic write registers `defer os.Remove(tmpName)` the instant it
// has the temp's name, before the first thing that can fail. Every rename store
// carries it with the same comment -- "a no-op once the rename has succeeded" --
// and it is what keeps a failed publish from leaving litter behind: a write,
// fsync, close, chmod or rename that returns early runs the defer and removes the
// temp, while a rename that succeeds leaves nothing for the defer to find.
//
// Dropping it does not break any test. ADR-0036 guarantees the temp name can
// never end in the globbed extension, so an orphan is never *offered* as a real
// record -- which is exactly what makes its accumulation invisible: one stray
// `agent.yaml.tmp-4817231` per interrupted write, unseen by the store's own
// listing but present on disk and tracked by git, since roles, agents and
// triggers are team-visible files the user commits. The defer is the cleanup
// that keeps a transient failure from leaving a permanent artifact in a
// version-controlled directory, and nothing derived it.
//
// tempCleanupViolation names one publish that does not defer removing its temp,
// in terms a reader can act on without opening the analyzer.
type tempCleanupViolation struct {
	fn     string
	reason string
}

// analyzeTempCleanup parses one store source file and holds every atomic-rename
// publish in it -- a function that both creates a temp file and renames it -- to
// deferring the removal of that temp.
//
// It ties the deferred remove to the temp it must clean up rather than accepting
// any os.Remove: it reads the variable os.CreateTemp is assigned to, the name
// variable bound from that file's `.Name()`, and requires a `defer os.Remove` of
// that name (or of `<tmp>.Name()` directly). A rename-publish with no such defer
// -- or one whose deferred remove targets something this guard cannot tie to the
// temp -- fails closed, rather than passing because some unrelated os.Remove
// happened to be present.
//
// publishChecked counts the publish functions actually examined so the caller
// can fail closed when the corpus scan finds none -- the case where the write
// moved or the calls were renamed and this guard silently began holding over an
// empty set.
func analyzeTempCleanup(filename string, src []byte) (publishChecked int, violations []tempCleanupViolation, parseErr error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return 0, nil, err
	}
	// isPkgCall reports whether call is `pkg.name(...)`.
	isPkgCall := func(call *ast.CallExpr, pkg, name string) bool {
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != name {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		return ok && id.Name == pkg
	}
	// isNameCallOn reports whether expr is `recv.Name()` for the given receiver
	// identifier -- the call that turns the *os.File from CreateTemp into the
	// path string os.Remove needs.
	isNameCallOn := func(expr ast.Expr, recv string) bool {
		call, ok := expr.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Name" {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		return ok && id.Name == recv
	}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		var tempVar string
		var haveTemp, haveRename bool
		nameVars := map[string]bool{}
		var deferArgs []ast.Expr

		// First pass: the temp variable is established by the assignment from
		// os.CreateTemp, and the rename establishes that this is a publish.
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if as, ok := n.(*ast.AssignStmt); ok && len(as.Rhs) == 1 && len(as.Lhs) >= 1 {
				if call, ok := as.Rhs[0].(*ast.CallExpr); ok && isPkgCall(call, "os", "CreateTemp") {
					if id, ok := as.Lhs[0].(*ast.Ident); ok {
						tempVar = id.Name
						haveTemp = true
					}
				}
			}
			if call, ok := n.(*ast.CallExpr); ok && isPkgCall(call, "os", "Rename") {
				haveRename = true
			}
			return true
		})
		if !haveTemp || !haveRename {
			continue
		}
		// Second pass, now that tempVar is known: bind its name variables and
		// collect deferred os.Remove arguments.
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if as, ok := n.(*ast.AssignStmt); ok && len(as.Rhs) == 1 && len(as.Lhs) >= 1 {
				if isNameCallOn(as.Rhs[0], tempVar) {
					if id, ok := as.Lhs[0].(*ast.Ident); ok {
						nameVars[id.Name] = true
					}
				}
			}
			if d, ok := n.(*ast.DeferStmt); ok && isPkgCall(d.Call, "os", "Remove") && len(d.Call.Args) == 1 {
				deferArgs = append(deferArgs, d.Call.Args[0])
			}
			return true
		})

		publishChecked++
		cleaned := false
		for _, arg := range deferArgs {
			if id, ok := arg.(*ast.Ident); ok && nameVars[id.Name] {
				cleaned = true
				break
			}
			if isNameCallOn(arg, tempVar) {
				cleaned = true
				break
			}
		}
		if !cleaned {
			violations = append(violations, tempCleanupViolation{
				fn:     fn.Name.Name,
				reason: "does not defer removing its temp file, so a publish that fails before the rename leaves an orphaned temp in the store directory -- invisible to the listing but tracked by git",
			})
		}
	}
	return publishChecked, violations, nil
}

// TestEveryStoreRenamePublishDefersRemovingItsTemp holds the cleanup step the
// family's atomic write registers before anything can fail: `defer
// os.Remove(tmpName)`, so an interrupted publish removes its temp rather than
// orphaning it.
//
// # The defect this was written for
//
// Every rename store, and logstore's snapshot, defers removing the temp with the
// same comment -- "a no-op once the rename has succeeded". Probed at its widest
// point, the cleanup is present in all of them. But nothing guarded it, and
// dropping it breaks no test: ADR-0036 guarantees the temp name can never end in
// the globbed extension, so an orphan is never offered as a real record, and the
// listing-based tests stay green while the store directory quietly accumulates
// one stray temp per failed write -- unseen by the store, but present on disk
// and committed by a user who tracks roles, agents and triggers in git. This is
// the shape ADR-0035 through ADR-0040 recorded: a family-wide property carried by
// the shape of a call and pinned by no derived guard.
//
// # Why the subject is derived
//
// The cleanup lives in each store's own deferred os.Remove of its temp,
// duplicated across the family, so the guard reads that defer from source and
// ties it to the temp -- the variable os.CreateTemp returns and the name bound
// from its `.Name()` -- rather than trusting the comment. A store added later, or
// an existing publish whose defer is dropped, is caught the first time this runs.
// A defer whose target the guard cannot tie to the temp fails closed, so an
// unrelated os.Remove cannot stand in for the cleanup.
//
// The corpus is the rename-publishers, the same measured boundary as ADR-0039
// and ADR-0040: every store that publishes through CreateTemp+Rename, which
// includes logstore's snapshot and excludes memorystore (an O_EXCL create with
// no rename, ADR-0034) and jobstore (an append-only journal, no
// CreateTemp+Rename).
func TestEveryStoreRenamePublishDefersRemovingItsTemp(t *testing.T) {
	// Prove the analyzer fires before trusting a clean corpus result: the two
	// correct forms -- a deferred remove of the bound name variable and of
	// `tmp.Name()` directly -- must pass; a publish with no deferred remove must
	// fail; and a deferred remove of something other than the temp must fail
	// closed. Without this, a clean scan below could mean the analyzer stopped
	// recognizing the calls rather than that the stores are correct.
	const goodNameVar = `package p
func write() error {
	tmp, _ := os.CreateTemp(dir, "x-*")
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	return os.Rename(tmpName, dst)
}`
	const goodDirect = `package p
func write() error {
	tmp, _ := os.CreateTemp(dir, "x-*")
	defer os.Remove(tmp.Name())
	return os.Rename(tmp.Name(), dst)
}`
	const missing = `package p
func write() error {
	tmp, _ := os.CreateTemp(dir, "x-*")
	tmpName := tmp.Name()
	return os.Rename(tmpName, dst)
}`
	const wrongTarget = `package p
func write(other string) error {
	tmp, _ := os.CreateTemp(dir, "x-*")
	tmpName := tmp.Name()
	defer os.Remove(other)
	return os.Rename(tmpName, dst)
}`
	if c, v, err := analyzeTempCleanup("namevar.go", []byte(goodNameVar)); err != nil || c != 1 || len(v) != 0 {
		t.Fatalf("analyzer flagged a correct deferred remove of the bound name variable (checked=%d, violations=%d, err=%v).\n"+
			"  Consequence: every store, which defers os.Remove(tmpName), would be reported as "+
			"broken. Remedy: accept a deferred os.Remove of the variable bound from the temp's "+
			".Name().", c, len(v), err)
	}
	if c, v, err := analyzeTempCleanup("direct.go", []byte(goodDirect)); err != nil || c != 1 || len(v) != 0 {
		t.Fatalf("analyzer flagged a correct deferred remove of tmp.Name() directly (checked=%d, violations=%d, err=%v).\n"+
			"  Consequence: a store that defers os.Remove(tmp.Name()) without a name variable "+
			"would be reported as broken. Remedy: accept a deferred os.Remove whose argument is "+
			"the temp's .Name() call.", c, len(v), err)
	}
	if c, v, err := analyzeTempCleanup("missing.go", []byte(missing)); err != nil || c != 1 || len(v) == 0 {
		t.Fatalf("analyzer did not flag a publish with no deferred remove (checked=%d, violations=%d, err=%v).\n"+
			"  Consequence: the corpus scan below would report success whether or not a store "+
			"dropped its cleanup. Remedy: re-derive analyzeTempCleanup from the defer the stores "+
			"register.", c, len(v), err)
	}
	if c, v, err := analyzeTempCleanup("wrong.go", []byte(wrongTarget)); err != nil || c != 1 || len(v) == 0 {
		t.Fatalf("analyzer did not fail closed on a deferred remove of something other than the temp (checked=%d, violations=%d, err=%v).\n"+
			"  Consequence: an unrelated os.Remove would stand in for the temp cleanup. Remedy: "+
			"tie the deferred remove to the variable os.CreateTemp returns.", c, len(v), err)
	}

	checked := 0
	for _, pkg := range storePackageDirs(t) {
		for _, file := range storeSourceFiles(t, pkg) {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("cannot read %s: %v", file, err)
			}
			n, violations, err := analyzeTempCleanup(file, raw)
			if err != nil {
				t.Fatalf("cannot parse %s: %v", file, err)
			}
			checked += n
			for _, v := range violations {
				t.Errorf("%s: %s %s.\n"+
					"  Every store in this family that publishes by renaming a temp into place "+
					"defers removing that temp, so a write that fails before the rename cleans up "+
					"after itself instead of littering the directory. This function breaks that.\n"+
					"  Remedy: defer os.Remove of the temp's name right after creating it, as the "+
					"rest of the family does.", displayPath(file), v.fn, v.reason)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no os.CreateTemp+os.Rename publish was found in any store.\n" +
			"  Every rename store published this way when this test was written, so finding none " +
			"means the write moved or the calls were renamed and this check now holds over an " +
			"empty set. Remedy: re-derive analyzeTempCleanup from the form the stores now use.")
	}
}
