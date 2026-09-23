package internal_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"testing"
)

// A durable publish in this family is three ordered steps: fsync the file so its
// bytes are on disk, then (for the rename stores) rename the temp over the final
// name, then fsync the directory so the entry that now names those bytes is
// itself durable. The order is the whole point. Reversed -- directory synced
// before the file -- a crash can leave a directory entry pointing at a file
// whose contents never reached the platter, which is precisely the torn read the
// sequence exists to prevent.
//
// durabilityOrderViolation names one function whose publish path breaks that
// order, in terms a reader can act on without opening the analyzer.
type durabilityOrderViolation struct {
	fn     string
	reason string
}

// analyzeDurabilityOrder parses one store source file and holds every durable
// publish function in it to the file-before-directory order.
//
// It classifies calls by selector name rather than by receiver, because the
// receiver varies (`tmp`, `f`, `file`) but the operation does not: `.Sync()` is
// a file fsync, `fsdurability.SyncDirectory(...)` a directory fsync, `os.Rename`
// the atomic publish, and `os.CreateTemp` / `.Write(...)` the evidence that the
// function writes file content at all. `SyncDirectory` and `Sync` are distinct
// selector names, so the directory fsync is never miscounted as a file fsync.
//
// A function is a durable publish when it both writes file content and fsyncs a
// directory. That pairing is the load-bearing case; a pure delete (an
// `os.Remove` followed by a directory fsync, with no file to sync) writes no
// content and is correctly not held to a file-before-directory order it has no
// file for. Keying the requirement on the content write, not on the directory
// fsync, is what makes a publish that dropped its file fsync fail here rather
// than pass as if it were a delete.
//
// publishChecked counts the publish functions actually examined, so the caller
// can fail closed when the corpus scan finds none -- the case where the write
// moved or was renamed and this guard silently began holding over an empty set.
func analyzeDurabilityOrder(filename string, src []byte) (publishChecked int, violations []durabilityOrderViolation, parseErr error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return 0, nil, err
	}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		var fileSync, dirSync, rename, contentWrite []token.Pos
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "SyncDirectory":
				dirSync = append(dirSync, call.Pos())
			case "Sync":
				fileSync = append(fileSync, call.Pos())
			case "Rename":
				rename = append(rename, call.Pos())
			case "CreateTemp", "Write":
				contentWrite = append(contentWrite, call.Pos())
			}
			return true
		})

		// No directory fsync: this function makes no claim about the durability
		// of a directory entry, so there is no file-before-directory order to
		// hold it to.
		if len(dirSync) == 0 {
			continue
		}
		// A directory fsync with no content write is a delete/tombstone: it makes
		// the removal of an entry durable and has no file of its own to fsync.
		if len(contentWrite) == 0 {
			continue
		}

		publishChecked++
		name := fn.Name.Name
		firstDir := minPos(dirSync)

		if len(fileSync) == 0 {
			violations = append(violations, durabilityOrderViolation{
				fn:     name,
				reason: "writes file content and fsyncs its directory but never fsyncs the file, so a crash can make the directory entry durable while the file's bytes are not",
			})
			continue
		}
		if minPos(fileSync) >= firstDir {
			violations = append(violations, durabilityOrderViolation{
				fn:     name,
				reason: "fsyncs its directory before it fsyncs the file, so a crash can leave the entry naming bytes that never reached disk",
			})
		}
		if len(rename) > 0 {
			if minPos(fileSync) >= minPos(rename) {
				violations = append(violations, durabilityOrderViolation{
					fn:     name,
					reason: "renames the temp file into place before fsyncing it, so the final name can point at unwritten bytes after a crash",
				})
			}
			if maxPos(rename) >= firstDir {
				violations = append(violations, durabilityOrderViolation{
					fn:     name,
					reason: "fsyncs its directory before renaming the temp file into place, so the directory fsync cannot be making the new entry durable",
				})
			}
		}
	}
	return publishChecked, violations, nil
}

// minPos and maxPos bound a set of call positions. A publish function has one of
// each call in practice; the analyzer takes the extreme so that even a function
// with several would be held to "some file fsync before the first directory
// fsync, and the last rename before it".
func minPos(ps []token.Pos) token.Pos {
	m := ps[0]
	for _, p := range ps[1:] {
		if p < m {
			m = p
		}
	}
	return m
}

func maxPos(ps []token.Pos) token.Pos {
	m := ps[0]
	for _, p := range ps[1:] {
		if p > m {
			m = p
		}
	}
	return m
}

// TestEveryStorePublishFsyncsTheFileBeforeTheDirectory holds the durability
// order that memorystore's comment claims for the whole family -- "Sync the file
// before the directory ... The same order every other store in this project
// uses" -- to every content store's actual write, rather than to that sentence.
//
// # The defect this was written for
//
// memorystore/store.go:172 states the order and adds "The same order every other
// store in this project uses"; agentstore/store.go:844 says its own sequence "is
// toolstore's, for the same reasons". Both are cross-package claims: one store's
// comment asserting a property of six others. Probed at its widest point -- every
// content store in the family -- the behaviour is correct: rolestore, modelstore,
// agentstore, toolstore, trigstore and evalstore all fsync the temp file, rename,
// then fsync the directory; memorystore fsyncs its O_EXCL file then the directory.
//
// But nothing guarded it. There was no test anywhere asserting the order --
// unlike the temp-suffix convention of ADR-0036, which at least had six decoupled
// tests. Reversing memorystore to fsync the directory before the file, or moving
// a rename store's directory fsync ahead of its rename, changes no test result,
// because the order lived only in prose. This is the shape ADR-0035 and ADR-0036
// recorded on the two turns before it: a family-wide property argued in one
// comment, generalised to N packages, and pinned in none.
//
// # Why the subject is derived
//
// The order is the same in every store and duplicated across the family, so the
// guard reads each store's own write from source -- the file fsync, the rename,
// the directory fsync, in the positions the code actually places them -- and
// holds that to the order, rather than trusting one comment's claim about the
// others. A store added later, or an existing write reordered so the directory
// fsync moves ahead of the file fsync or the rename, is caught the first time
// this runs.
//
// The corpus is the same family as ADR-0035 and ADR-0036: `internal/*store`
// packages that declare `const ext`, which all route their directory durability
// through a direct `fsdurability.SyncDirectory` call. jobstore and logstore
// reach the same order through a `syncDir` helper and an ops seam rather than a
// direct call, so the direct-call analyzer does not see them; they are out of
// this guard's scope by that boundary, not by exemption.
func TestEveryStorePublishFsyncsTheFileBeforeTheDirectory(t *testing.T) {
	// Prove the analyzer fires before trusting a clean corpus result: a write
	// with the directory fsync moved ahead of the file fsync must be flagged, and
	// the correct order must pass. Without this, a negative result below could
	// mean the analyzer stopped recognising the calls rather than that the stores
	// are correct -- the vacuity this project keeps finding in its own guards.
	const badOrder = `package p
func write() error {
	tmp, _ := os.CreateTemp(dir, "x-*")
	tmp.Write(body)
	if err := fsdurability.SyncDirectory(dir); err != nil { return err }
	if err := tmp.Sync(); err != nil { return err }
	return os.Rename(tmp.Name(), final)
}`
	const goodOrder = `package p
func write() error {
	tmp, _ := os.CreateTemp(dir, "x-*")
	tmp.Write(body)
	if err := tmp.Sync(); err != nil { return err }
	if err := os.Rename(tmp.Name(), final); err != nil { return err }
	return fsdurability.SyncDirectory(dir)
}`
	if checked, v, err := analyzeDurabilityOrder("bad.go", []byte(badOrder)); err != nil || checked != 1 || len(v) == 0 {
		t.Fatalf("analyzer did not flag a directory-before-file write (checked=%d, violations=%d, err=%v).\n"+
			"  Consequence: the corpus scan below would report success whether or not a store "+
			"reversed its durability order. Remedy: re-derive analyzeDurabilityOrder from the "+
			"calls the stores now make.", checked, len(v), err)
	}
	if checked, v, err := analyzeDurabilityOrder("good.go", []byte(goodOrder)); err != nil || checked != 1 || len(v) != 0 {
		t.Fatalf("analyzer flagged the correct file-before-directory order (checked=%d, violations=%d, err=%v).\n"+
			"  Consequence: every store would be reported as broken. Remedy: re-derive "+
			"analyzeDurabilityOrder so the correct order passes.", checked, len(v), err)
	}

	checked := 0
	for _, pkg := range storePackageDirs(t) {
		files := storeSourceFiles(t, pkg)
		// Only the ext-declaring content stores are in this family: the same
		// corpus ADR-0035 and ADR-0036 derive over, and the ones whose comments
		// make the cross-store order claim. A store package with no `const ext`
		// (jobstore, logstore) publishes through a different shape and is not held
		// to this direct-call order.
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
		for _, file := range sortedKeys(srcs) {
			n, violations, err := analyzeDurabilityOrder(file, srcs[file])
			if err != nil {
				t.Fatalf("cannot parse %s: %v", file, err)
			}
			checked += n
			for _, v := range violations {
				t.Errorf("%s: %s %s.\n"+
					"  Every content store in this family fsyncs the file, then (for the rename "+
					"stores) renames, then fsyncs the directory, so that a crash can never leave a "+
					"directory entry naming bytes that are not yet durable. This function breaks "+
					"that order.\n"+
					"  Remedy: fsync the file before the rename, and fsync the directory only "+
					"after -- the order memorystore's comment calls the one every store uses.",
					displayPath(file), v.fn, v.reason)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no durable publish function was found in any store declaring `const ext`.\n" +
			"  Every content store paired a file fsync with a directory fsync when this test was " +
			"written, so finding none means the write moved or the calls were renamed and this " +
			"check now holds over an empty set. Remedy: re-derive analyzeDurabilityOrder from the " +
			"form the stores now use.")
	}
}

// sortedKeys returns the map keys in a stable order so failure messages and the
// checked count do not depend on map iteration order.
func sortedKeys(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
