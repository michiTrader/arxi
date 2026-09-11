package v1_test

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"
)

const packagePath = "github.com/michiTrader/arxi/host/v1"

// TestExportedAPIIsIndependent inspects every exported declaration, including
// nested method signatures and field types. This catches leaks that an external
// compile test alone misses, such as an exported struct field typed with an
// internal alias.
func TestExportedAPIIsIndependent(t *testing.T) {
	fset := token.NewFileSet()
	files, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatalf("parse public package: %v", err)
	}
	parsed := files["v1"]
	if parsed == nil {
		t.Fatal("public package v1 was not parsed")
	}
	astFiles := make([]*ast.File, 0, len(parsed.Files))
	for name, file := range parsed.Files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		astFiles = append(astFiles, file)
	}
	conf := types.Config{Importer: importer.ForCompiler(fset, "source", nil)}
	pkg, err := conf.Check(packagePath, fset, astFiles, nil)
	if err != nil {
		t.Fatalf("type-check public package: %v", err)
	}
	seen := map[types.Type]bool{}
	for _, name := range pkg.Scope().Names() {
		if !ast.IsExported(name) {
			continue
		}
		obj := pkg.Scope().Lookup(name)
		checkPublicType(t, name, obj.Type(), seen)
	}
}

func TestExportedNamesDoNotPublishPersistenceVocabulary(t *testing.T) {
	fset := token.NewFileSet()
	files, err := parser.ParseDir(fset, ".", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse public package: %v", err)
	}
	for filename, file := range files["v1"].Files {
		if strings.HasSuffix(filename, "_test.go") {
			continue
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch declaration := node.(type) {
			case *ast.TypeSpec:
				checkExportedName(t, filename, declaration.Name.Name)
			case *ast.Field:
				for _, name := range declaration.Names {
					checkExportedName(t, filename, name.Name)
				}
			}
			return true
		})
	}
}

func checkExportedName(t *testing.T, filename, name string) {
	t.Helper()
	if !ast.IsExported(name) {
		return
	}
	lower := strings.ToLower(name)
	for _, forbidden := range []string{"path", "offset", "lockfile", "directory", "pendingcause", "blockedref", "workmanifest", "jobstore", "storagepath"} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("%s exports %q, which publishes forbidden persistence/path vocabulary %q", filename, name, forbidden)
		}
	}
}

func TestPhaseOneTextProviderExcludesLaterPhaseOutcomeAndAccounting(t *testing.T) {
	fset := token.NewFileSet()
	files, err := parser.ParseDir(fset, ".", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse public package: %v", err)
	}
	for filename, file := range files["v1"].Files {
		if strings.HasSuffix(filename, "_test.go") {
			continue
		}
		ast.Inspect(file, func(node ast.Node) bool {
			name, ok := node.(*ast.Ident)
			if !ok {
				return true
			}
			switch name.Name {
			case "TextOutcome", "TextCompleted", "TextRefused", "Usage", "Refusal", "Retryable":
				t.Errorf("%s publishes Phase 2 text-provider concept %q", filename, name.Name)
			}
			return true
		})
	}
}

func TestSelectedMemberProjectionExcludesReducerBookkeeping(t *testing.T) {
	fset := token.NewFileSet()
	files, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	parsed := files["v1"]
	astFiles := make([]*ast.File, 0, len(parsed.Files))
	for name, file := range parsed.Files {
		if !strings.HasSuffix(name, "_test.go") {
			astFiles = append(astFiles, file)
		}
	}
	pkg, err := (&types.Config{Importer: importer.ForCompiler(fset, "source", nil)}).Check(packagePath, fset, astFiles, nil)
	if err != nil {
		t.Fatal(err)
	}
	named := pkg.Scope().Lookup("Member").Type().(*types.Named)
	fields := named.Underlying().(*types.Struct)
	allowed := map[string]bool{
		"Name": true, "Role": true, "State": true, "Detail": true,
		"Turns": true, "SpentUSD": true, "Busy": true, "Runnable": true,
		"Submitted": true,
	}
	for i := 0; i < fields.NumFields(); i++ {
		name := fields.Field(i).Name()
		if !allowed[name] {
			t.Errorf("Member exports reducer bookkeeping field %q", name)
		}
	}
}

func checkPublicType(t *testing.T, where string, typ types.Type, seen map[types.Type]bool) {
	t.Helper()
	if typ == nil || seen[typ] {
		return
	}
	seen[typ] = true

	text := types.TypeString(typ, func(p *types.Package) string { return p.Path() })
	for _, forbidden := range []string{"/internal/", "filepath.", "os.File", "byte offset", "lockfile", "pending_cause", "blocked_ref", "work_manifest"} {
		if strings.Contains(strings.ToLower(text), strings.ToLower(forbidden)) {
			t.Errorf("exported API %s leaks forbidden implementation detail %q through %s", where, forbidden, text)
		}
	}

	switch value := typ.(type) {
	case *types.Named:
		if obj := value.Obj(); obj != nil && obj.Pkg() != nil {
			path := obj.Pkg().Path()
			if strings.Contains(path, "/internal/") || strings.HasSuffix(path, "/internal") {
				t.Errorf("exported API %s references internal type %s", where, text)
			}
		}
		checkPublicType(t, where, value.Underlying(), seen)
		for i := 0; i < value.NumMethods(); i++ {
			method := value.Method(i)
			if method.Exported() {
				checkPublicType(t, where+"."+method.Name(), method.Type(), seen)
			}
		}
	case *types.Pointer:
		checkPublicType(t, where, value.Elem(), seen)
	case *types.Slice:
		checkPublicType(t, where, value.Elem(), seen)
	case *types.Array:
		checkPublicType(t, where, value.Elem(), seen)
	case *types.Map:
		checkPublicType(t, where, value.Key(), seen)
		checkPublicType(t, where, value.Elem(), seen)
	case *types.Chan:
		checkPublicType(t, where, value.Elem(), seen)
	case *types.Struct:
		for i := 0; i < value.NumFields(); i++ {
			field := value.Field(i)
			if field.Exported() {
				checkPublicType(t, where+"."+field.Name(), field.Type(), seen)
			}
		}
	case *types.Interface:
		for i := 0; i < value.NumMethods(); i++ {
			method := value.Method(i)
			if method.Exported() {
				checkPublicType(t, where+"."+method.Name(), method.Type(), seen)
			}
		}
	case *types.Signature:
		checkTuple(t, where+" parameters", value.Params(), seen)
		checkTuple(t, where+" results", value.Results(), seen)
	}
}

func checkTuple(t *testing.T, where string, tuple *types.Tuple, seen map[types.Type]bool) {
	t.Helper()
	for i := 0; i < tuple.Len(); i++ {
		checkPublicType(t, where, tuple.At(i).Type(), seen)
	}
}
