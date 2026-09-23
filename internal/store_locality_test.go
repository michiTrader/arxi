package internal_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// defaultDirConst matches a package-level `const DefaultDir = "..."` and
// captures the literal it is bound to. The stores that carry a default all
// write it this way (memorystore "memory", rolestore "roles", ...), so the
// literal is readable from source without importing every package by hand --
// which is the point: a hand list of packages goes stale in exactly the store
// nobody remembered to add.
var defaultDirConst = regexp.MustCompile(`(?m)^const DefaultDir = "([^"]*)"`)

// homeDerivation matches the standard-library calls that would root a store in
// the user's home directory instead of beside runs/. These are the only ways a
// Go program reaches $HOME without an import the kernel purity test already
// forbids, so matching them in store source catches the leak at its source.
var homeDerivation = regexp.MustCompile(`UserHomeDir|UserConfigDir|UserCacheDir|Getenv\("HOME"\)|Getenv\("XDG_[A-Z_]+"\)`)

// storePackageDirs discovers every internal store package by globbing the tree
// rather than listing packages here.
//
// Derived, not enumerated, for the reason ADR-0035 records: the defect this
// file guards against is a claim ("the sibling stores all keep memory beside
// runs/, not in $HOME") that held for one store and was generalised to four.
// A hand-written package list repeats that mistake one layer down -- it goes
// stale in the store nobody adds to it, and a store missing from the list is a
// store the leak guard never sees. The glob covers whatever the tree contains,
// including a store added after this test was written.
func storePackageDirs(t *testing.T) []string {
	t.Helper()
	dirs, err := filepath.Glob("../internal/*store")
	if err != nil {
		t.Fatalf("cannot glob internal store packages: %v", err)
	}
	var pkgs []string
	for _, d := range dirs {
		info, err := os.Stat(d)
		if err != nil || !info.IsDir() {
			continue
		}
		pkgs = append(pkgs, d)
	}
	if len(pkgs) == 0 {
		t.Fatal("no internal/*store package found.\n" +
			"  Consequence: every assertion below holds over an empty set and this test " +
			"reports success while checking nothing. Remedy: re-derive the glob from the " +
			"layout the stores now use.")
	}
	return pkgs
}

// displayPath renders a globbed path as a forward-slash path relative to the
// repository root, so a failure message reads the same on every platform rather
// than leaking the "..\internal\..." the Windows globber produces.
func displayPath(file string) string {
	return strings.TrimPrefix(filepath.ToSlash(file), "../")
}

// storeSourceFiles returns the non-test Go files of a store package.
func storeSourceFiles(t *testing.T, pkgDir string) []string {
	t.Helper()
	all, err := filepath.Glob(filepath.Join(pkgDir, "*.go"))
	if err != nil {
		t.Fatalf("cannot glob Go files in %s: %v", pkgDir, err)
	}
	var src []string
	for _, f := range all {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src = append(src, f)
	}
	return src
}

// TestEveryStoreDefaultDirectoryIsProjectLocal holds every store's DefaultDir to
// the property memorystore's comment claims for the whole family: it names a
// location beside runs/, relative to the working directory, never an absolute or
// home-anchored path.
//
// # The defect this was written for
//
// memorystore/store.go said its store sits beside runs/ "for the reason
// rolestore, trigstore, modelstore and agentstore all give". Probed at its
// widest point, that was false: only trigstore argues the trade-off; the other
// three state the location and nothing more, and only trigstore had a test
// pinning it. So a property asserted for four packages was argued in one and
// enforced in one -- the same shape as the memory channel that was true in one
// assembler and false in the two others ADR-0020 named, and the roadmap count
// that was right for a subset and generalised to the whole.
//
// The remedy this corpus reaches for is to derive or pin the fact rather than
// trust the prose. This test enforces the invariant across whatever stores the
// tree holds, so the guarantee no longer rests on three comments that never made
// the argument.
//
// Not every store declares a DefaultDir: jobstore and logstore take their
// directory from the caller and have no default to check. So the loop asserts
// the property of every default it finds and fails closed only if it finds none
// at all -- the case where the literal format drifted and the check silently
// stopped seeing any store.
func TestEveryStoreDefaultDirectoryIsProjectLocal(t *testing.T) {
	found := 0
	for _, pkg := range storePackageDirs(t) {
		for _, file := range storeSourceFiles(t, pkg) {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("cannot read %s: %v", file, err)
			}
			for _, m := range defaultDirConst.FindAllStringSubmatch(string(raw), -1) {
				found++
				dir := m[1]
				rel := filepath.ToSlash(dir)
				bad := dir == "" ||
					filepath.IsAbs(dir) ||
					strings.HasPrefix(dir, "~") ||
					strings.HasPrefix(rel, "/") ||
					strings.HasPrefix(rel, "../") ||
					rel == ".." ||
					strings.Contains(dir, ":") // a Windows volume such as C:\
				if bad {
					t.Errorf("%s declares DefaultDir = %q, which is not project-local.\n"+
						"  A store rooted anywhere but beside runs/, relative to the working "+
						"directory, would follow the user between repositories: data written "+
						"while working on one project would silently influence an agent working "+
						"on the next. Cross-run recall is the feature; cross-repository recall is "+
						"a leak nobody asked for.\n"+
						"  Remedy: give DefaultDir a bare relative name, or move the reasoning "+
						"with the default if this store genuinely must live elsewhere.",
						displayPath(file), dir)
				}
			}
		}
	}
	if found == 0 {
		t.Fatal("no store declares `const DefaultDir = \"...\"`.\n" +
			"  Every content store had one when this test was written (memory, roles, " +
			"triggers, providers, agents, evals, policies), so finding none means the " +
			"const format changed and this check stopped seeing any store -- it now holds " +
			"over an empty set. Remedy: re-derive defaultDirConst from the form the stores " +
			"now use.")
	}
}

// TestNoStoreRootsItselfInTheHomeDirectory holds every store package to the
// invariant that it does not reach the user's home directory to decide where its
// data lives.
//
// This is the direct form of the anti-leak property. TestEveryStore... above
// checks the declared default; this checks that no store computes a path from
// $HOME at all, which also covers jobstore and logstore, the two that take their
// directory from the caller and so have no DefaultDir to inspect. A store that
// grew a UserHomeDir call to pick a "convenient" default would reintroduce the
// cross-repository leak without touching any DefaultDir literal.
//
// The detector is proven to fire against a known-bad fixture before the corpus
// is trusted, so a regex that stopped matching cannot make this pass vacuously
// -- the failure shape a negative assertion is most exposed to.
func TestNoStoreRootsItselfInTheHomeDirectory(t *testing.T) {
	const knownBad = `dir, _ := os.UserHomeDir()`
	if !homeDerivation.MatchString(knownBad) {
		t.Fatalf("homeDerivation does not match %q.\n"+
			"  Consequence: the corpus scan below asserts nothing, because a broken "+
			"detector matches no store whether or not one leaks. Remedy: re-derive "+
			"homeDerivation from the calls that reach $HOME.", knownBad)
	}

	scanned := 0
	for _, pkg := range storePackageDirs(t) {
		for _, file := range storeSourceFiles(t, pkg) {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("cannot read %s: %v", file, err)
			}
			scanned++
			if loc := homeDerivation.FindString(string(raw)); loc != "" {
				t.Errorf("%s reaches the home directory via %q to place its data.\n"+
					"  A store rooted in $HOME follows the user between repositories, so data "+
					"written while working on one project silently influences an agent working "+
					"on the next. Every store keeps its data beside runs/, relative to the "+
					"working directory, for exactly this reason.\n"+
					"  Remedy: root the store at a project-local relative path instead.",
					displayPath(file), loc)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("no store source files were read.\n" +
			"  Consequence: the negative assertion above passed over nothing. Remedy: " +
			"re-derive the source globs from the layout the stores now use.")
	}
}
