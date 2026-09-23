package internal_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"
)

// The family's atomic write is the same five steps in every rename store:
// os.CreateTemp beside the destination, write, fsync the file, chmod, then
// os.Rename the temp over the final name (and fsync the directory). ADR-0036
// guards that the temp name can never end in the globbed extension, so a write
// interrupted before the rename leaves nothing a reader offers; ADR-0037 guards
// the fsync order. Both of those rest on one property neither derives: the
// rename is atomic ONLY within a single filesystem, and the stores get that by
// creating the temp in the destination's own directory -- os.CreateTemp(s.dir,
// ...) renamed to a path under that same s.dir. Route the temp through
// os.TempDir() instead and the rename crosses a filesystem boundary: on Linux
// it fails outright with EXDEV, and where a runtime papers over that with a
// copy-then-delete the publish stops being atomic -- which is exactly the torn
// read ADR-0036's suffix rule and this project's whole "a reader never sees a
// half-written record" guarantee exist to prevent. No guard held it; it lived
// only in the shape of the CreateTemp call.
//
// renameDirViolation names one publish whose temp directory and rename
// destination directory cannot be shown to be the same, in terms a reader can
// act on without opening the analyzer.
type renameDirViolation struct {
	fn     string
	reason string
}

// analyzeRenamePublishDir parses one store source file and holds every
// atomic-rename publish in it -- a function that both creates a temp file and
// renames it -- to the requirement that the temp is created in the directory it
// is renamed into.
//
// The destination directory is resolved from source without evaluating it: a
// destination of filepath.Join(D, ...) has directory D, and a destination of
// s.Path(...) has the directory that Path's own filepath.Join joins on -- which
// this file also carries, since Path and the publish live in the same store.go.
// A destination in neither form is not assumed same-directory; it is reported,
// so an unrecognized publish fails closed rather than passing as safe, the way
// this project's other derived guards treat a form they cannot read.
//
// renamesChecked counts the renames actually evaluated so the caller can fail
// closed when the corpus scan finds none -- the case where the write moved or
// the rename was renamed and this guard silently began holding over an empty
// set.
func analyzeRenamePublishDir(filename string, src []byte) (renamesChecked int, violations []renameDirViolation, parseErr error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return 0, nil, err
	}
	nodeText := func(n ast.Node) string {
		return string(src[fset.Position(n.Pos()).Offset:fset.Position(n.End()).Offset])
	}
	// isPkgCall reports whether call is `pkg.name(...)` -- e.g. os.Rename or
	// filepath.Join -- so a method named Rename on some type is never mistaken
	// for the standard-library call the atomic publish uses.
	isPkgCall := func(call *ast.CallExpr, pkg, name string) bool {
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != name {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		return ok && id.Name == pkg
	}
	// joinBase returns the text of the first argument of a filepath.Join call,
	// which is the directory the joined path lives in.
	joinBase := func(call *ast.CallExpr) (string, bool) {
		if !isPkgCall(call, "filepath", "Join") || len(call.Args) == 0 {
			return "", false
		}
		return nodeText(call.Args[0]), true
	}

	// methodJoinDir maps a receiver method name to the directory its returned
	// filepath.Join joins on -- Path -> "s.dir". The rename stores build the
	// final name with s.Path(...), so resolving that call needs the base Path
	// itself joins on, and Path sits in this same file beside the publish.
	methodJoinDir := map[string]string{}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if base, ok := joinBase(call); ok {
					if _, seen := methodJoinDir[fn.Name.Name]; !seen {
						methodJoinDir[fn.Name.Name] = base
					}
				}
			}
			return true
		})
	}

	// destDir resolves the directory a rename destination expression names, in
	// the two forms the family uses: filepath.Join(D, ...) directly (logstore's
	// snapshot), and s.Path(...) resolved through methodJoinDir (the ext
	// stores). Anything else is unrecognized and fails closed.
	destDir := func(dst ast.Expr) (string, bool) {
		call, ok := dst.(*ast.CallExpr)
		if !ok {
			return "", false
		}
		if base, ok := joinBase(call); ok {
			return base, true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			if base, ok := methodJoinDir[sel.Sel.Name]; ok {
				return base, true
			}
		}
		return "", false
	}

	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		var tempDir string
		var haveTemp bool
		var renames []*ast.CallExpr
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch {
			case isPkgCall(call, "os", "CreateTemp") && len(call.Args) >= 1:
				tempDir = nodeText(call.Args[0])
				haveTemp = true
			case isPkgCall(call, "os", "Rename") && len(call.Args) >= 2:
				renames = append(renames, call)
			}
			return true
		})
		// A rename with no temp in the same function is not this pattern -- it is
		// some other move, not the create-temp-then-publish this guard is about.
		if !haveTemp {
			continue
		}
		for _, r := range renames {
			renamesChecked++
			dir, ok := destDir(r.Args[1])
			if !ok {
				violations = append(violations, renameDirViolation{
					fn:     fn.Name.Name,
					reason: "renames its temp into a destination this guard cannot resolve to a directory, so it cannot confirm the temp is created beside the file it publishes",
				})
				continue
			}
			if dir != tempDir {
				violations = append(violations, renameDirViolation{
					fn:     fn.Name.Name,
					reason: "creates its temp in " + tempDir + " but renames it into " + dir + "; a rename across directories can cross a filesystem boundary, where it is not atomic (or fails outright), so a reader can observe a half-written record",
				})
			}
		}
	}
	return renamesChecked, violations, nil
}

// TestEveryStoreRenamePublishTargetsItsTempDirectory holds the one property the
// family's atomic write rests on but never derived: the temp file is created in
// the directory it is renamed into, so os.Rename stays within a single
// filesystem and is atomic.
//
// # The defect this was written for
//
// Every rename store writes the same shape -- os.CreateTemp(s.dir, ...) then
// os.Rename(tmp.Name(), s.Path(...)), with Path joining on that same s.dir --
// and logstore's snapshot does the same with filepath.Join(s.dir, ...) as the
// destination. That the temp sits beside its destination is what makes the
// rename atomic, and the atomicity is what ADR-0036 (the temp never ends in the
// globbed extension) and the family's "a reader never sees a half-written
// record" comments assume. But nothing derived it. Routing a temp through
// os.TempDir() -- a plausible "use the system temp dir" cleanup -- leaves every
// store's suite green while turning the publish into a cross-filesystem rename:
// EXDEV on Linux, or a non-atomic copy-then-delete elsewhere. This is the shape
// ADR-0035 through ADR-0038 recorded: a family-wide property carried by the
// shape of a call and pinned by no guard.
//
// # Why the subject is derived
//
// The property lives in each store's own CreateTemp and Rename, duplicated
// across the family, so the guard reads those two calls from source and holds
// the rename destination's directory equal to the temp's, rather than trusting
// the shape to stay right. A store added later, or an existing publish whose
// temp is moved off the destination's directory, is caught the first time this
// runs. A destination the analyzer cannot resolve to a directory is reported,
// not assumed safe, so an unfamiliar publish fails closed.
//
// # The corpus is the rename-publishers, a measured boundary of its own
//
// Unlike ADR-0038, whose corpus is the `const ext` stores, this guard's subject
// is every store that publishes through CreateTemp+Rename. That boundary
// includes logstore's snapshot -- which declares no `const ext` and so is out of
// ADR-0038's scope, yet renames a temp into place exactly like the ext stores --
// and excludes memorystore (an O_EXCL content-addressed create with no rename,
// ADR-0034) and jobstore (an append-only journal, no CreateTemp+Rename). The
// boundary is stated as what the guard measures, not enumerated by hand.
func TestEveryStoreRenamePublishTargetsItsTempDirectory(t *testing.T) {
	// Prove the analyzer fires before trusting a clean corpus result: a
	// cross-directory publish must be flagged; the two same-directory forms the
	// family uses -- filepath.Join directly and a resolved s.Path(...) -- must
	// pass; and a destination in neither form must fail closed. Without this, a
	// clean scan below could mean the analyzer stopped recognizing the calls
	// rather than that the stores are correct.
	const crossDir = `package p
type Store struct{ dir string }
func (s *Store) write(name string) error {
	tmp, _ := os.CreateTemp(os.TempDir(), name+".tmp-*")
	return os.Rename(tmp.Name(), filepath.Join(s.dir, name))
}`
	const sameDirJoin = `package p
type Store struct{ dir string }
func (s *Store) write(name string) error {
	tmp, _ := os.CreateTemp(s.dir, name+".tmp-*")
	return os.Rename(tmp.Name(), filepath.Join(s.dir, name))
}`
	const sameDirMethod = `package p
type Store struct{ dir string }
func (s *Store) Path(name string) string { return filepath.Join(s.dir, name+".json") }
func (s *Store) write(name string, body []byte) error {
	tmp, _ := os.CreateTemp(s.dir, name+".json.tmp-*")
	_ = body
	return os.Rename(tmp.Name(), s.Path(name))
}`
	const unresolvedDest = `package p
type Store struct{ dir string }
func (s *Store) write(name, dst string) error {
	tmp, _ := os.CreateTemp(s.dir, name+".tmp-*")
	return os.Rename(tmp.Name(), dst)
}`
	if c, v, err := analyzeRenamePublishDir("cross.go", []byte(crossDir)); err != nil || c != 1 || len(v) == 0 {
		t.Fatalf("analyzer did not flag a cross-directory publish (checked=%d, violations=%d, err=%v).\n"+
			"  Consequence: the corpus scan below would report success whether or not a store "+
			"moved its temp off the destination's directory. Remedy: re-derive "+
			"analyzeRenamePublishDir from the CreateTemp and Rename calls the stores now make.", c, len(v), err)
	}
	if c, v, err := analyzeRenamePublishDir("join.go", []byte(sameDirJoin)); err != nil || c != 1 || len(v) != 0 {
		t.Fatalf("analyzer flagged a correct same-directory publish using filepath.Join (checked=%d, violations=%d, err=%v).\n"+
			"  Consequence: logstore's snapshot publish would be reported as broken. Remedy: "+
			"re-derive destDir so a filepath.Join destination in the temp's directory passes.", c, len(v), err)
	}
	if c, v, err := analyzeRenamePublishDir("method.go", []byte(sameDirMethod)); err != nil || c != 1 || len(v) != 0 {
		t.Fatalf("analyzer flagged a correct same-directory publish using s.Path(...) (checked=%d, violations=%d, err=%v).\n"+
			"  Consequence: every ext store, which renames into s.Path(name), would be reported "+
			"as broken. Remedy: resolve a method destination through the directory that method's "+
			"own filepath.Join joins on.", c, len(v), err)
	}
	if c, v, err := analyzeRenamePublishDir("unresolved.go", []byte(unresolvedDest)); err != nil || c != 1 || len(v) == 0 {
		t.Fatalf("analyzer did not fail closed on an unresolvable rename destination (checked=%d, violations=%d, err=%v).\n"+
			"  Consequence: a publish whose destination this guard cannot read would pass as if "+
			"its temp were beside it. Remedy: report a destination that is neither filepath.Join "+
			"nor a resolvable method call, rather than assuming it same-directory.", c, len(v), err)
	}

	checked := 0
	for _, pkg := range storePackageDirs(t) {
		for _, file := range storeSourceFiles(t, pkg) {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("cannot read %s: %v", file, err)
			}
			n, violations, err := analyzeRenamePublishDir(file, raw)
			if err != nil {
				t.Fatalf("cannot parse %s: %v", file, err)
			}
			checked += n
			for _, v := range violations {
				t.Errorf("%s: %s %s.\n"+
					"  Every store in this family that publishes by renaming a temp into place "+
					"creates that temp in the destination's own directory, so the rename never "+
					"crosses a filesystem boundary and stays atomic. This function breaks that.\n"+
					"  Remedy: create the temp with os.CreateTemp in the same directory the rename "+
					"targets, as the rest of the family does.", displayPath(file), v.fn, v.reason)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no os.CreateTemp+os.Rename publish was found in any store.\n" +
			"  Every rename store published this way when this test was written, so finding none " +
			"means the write moved or the calls were renamed and this check now holds over an " +
			"empty set. Remedy: re-derive analyzeRenamePublishDir from the form the stores now use.")
	}
}
