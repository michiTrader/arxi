package internal_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// extConst matches a store's `const ext = "..."` -- the suffix its listing
// selects on. A store declares it precisely so Names/List can glob for it, so a
// temp file that ends in the same suffix is a file the listing will offer.
var extConst = regexp.MustCompile(`(?m)^const ext = "([^"]*)"`)

// createTempArgs returns the argument text of every os.CreateTemp(...) call in
// src, found by scanning for balanced parentheses rather than a regex, because
// the pattern argument is itself a concatenation and a regex cannot bound the
// call reliably.
func createTempArgs(src string) []string {
	const marker = "os.CreateTemp("
	var out []string
	for i := 0; ; {
		at := strings.Index(src[i:], marker)
		if at < 0 {
			return out
		}
		start := i + at + len(marker)
		depth := 1
		j := start
		for ; j < len(src) && depth > 0; j++ {
			switch src[j] {
			case '(':
				depth++
			case ')':
				depth--
			}
		}
		out = append(out, strings.TrimSpace(src[start:j-1]))
		i = j
	}
}

// tempPatternEndsInWildcard reports whether a CreateTemp argument list ends in a
// string literal whose last character is the "*" wildcard.
//
// os.CreateTemp substitutes its random component for the last "*" in the
// pattern, so a pattern that ends in "*" produces a name ending in random
// digits -- which can never end in a store's ".json" or ".yaml" extension. Any
// other tail (`+ext`, another literal, a call) is refused: the check is a
// sufficient, auditable condition for the real property, and an unrecognized
// form fails closed rather than being guessed safe.
func tempPatternEndsInWildcard(args string) bool {
	return strings.HasSuffix(strings.TrimSpace(args), `*"`)
}

// TestEveryStoreTempFileCannotEndInItsGlobbedExtension holds the atomic-write
// convention every content store's comments call load-bearing: the temp file a
// write creates must not end in the extension the store's listing globs for, so
// a define or save interrupted between CreateTemp and Rename leaves nothing a
// reader will offer as a real record.
//
// # The defect this was written for
//
// Six stores (rolestore, modelstore, agentstore, toolstore, trigstore,
// evalstore) each carry a comment calling this "load-bearing, not cosmetic" and
// a test named for it -- "a half-written X is never visible". Probed at its
// widest point, not one of those tests guarded it. Each either writes a
// hand-built temp name (`auditor.json.tmp-4711`) and checks the listing skips
// it, or reconstructs the pattern by hand (`"backend"+ext+".tmp-123"`) and
// asserts that string does not end in ext. Both model the convention in the test
// instead of reading it from the write, so moving ext after the wildcard in the
// real write -- `CreateTemp(dir, name+".tmp-*"+ext)`, producing
// `name.tmp-<n>.json`, which the listing WOULD offer -- leaves every one of the
// six suites green. Confirmed by mutation: that edit passes all six.
//
// agentstore's own test even says the suffix assertion "is the one that survives
// a refactor". It does not survive the refactor it names, because it re-derives
// the convention by hand rather than from the code it guards -- the exact shape
// AGENTS.md keeps finding: a guard whose subject is reconstructed by hand goes
// stale in the case nobody re-checked.
//
// # Why the subject is derived
//
// The convention lives in each store's own CreateTemp call, duplicated across
// six packages. So the guard reads that call from source and holds it to the
// property, rather than trusting six comments and six decoupled tests. A store
// added later, or an existing pattern "cleaned up" to give the temp the right
// extension for tooling, is caught the first time this runs.
//
// memorystore declares ext but writes version files directly under an O_EXCL
// name and reseals every file on read (ADR-0034), so it has no CreateTemp and
// contributes nothing here -- correctly, since content addressing, not the name,
// is what makes its half-written file unreadable.
func TestEveryStoreTempFileCannotEndInItsGlobbedExtension(t *testing.T) {
	// Prove the detector fires before trusting it against the corpus: it must
	// accept the wildcard-last form and reject the ext-last form, or a negative
	// result below would mean nothing.
	if !tempPatternEndsInWildcard(`s.dir, name+ext+".tmp-*"`) {
		t.Fatal("tempPatternEndsInWildcard rejects the wildcard-last form.\n" +
			"  Consequence: the corpus scan below would flag every correct store. " +
			"Remedy: re-derive the check from the CreateTemp pattern the stores now use.")
	}
	if tempPatternEndsInWildcard(`s.dir, name+".tmp-*"+ext`) {
		t.Fatal("tempPatternEndsInWildcard accepts the ext-last form, the exact defect " +
			"this test exists to catch.\n" +
			"  Consequence: a temp file that ends in the globbed extension passes, so a " +
			"half-written record becomes visible to a reader. Remedy: the check must require " +
			"the pattern to end in the \"*\" wildcard.")
	}

	checked := 0
	for _, pkg := range storePackageDirs(t) {
		for _, file := range storeSourceFiles(t, pkg) {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("cannot read %s: %v", file, err)
			}
			src := string(raw)
			// Only stores that select files by an `ext` suffix are exposed: the
			// temp must avoid that suffix precisely because the listing globs it.
			if !extConst.MatchString(src) {
				continue
			}
			for _, args := range createTempArgs(src) {
				checked++
				if !tempPatternEndsInWildcard(args) {
					t.Errorf("%s calls os.CreateTemp with a pattern that does not end in the "+
						"\"*\" wildcard: %s\n"+
						"  A store whose listing globs on its extension must create its temp file "+
						"with the random suffix LAST, so the temp name cannot end in that "+
						"extension. With ext after the wildcard, a write interrupted between "+
						"CreateTemp and Rename leaves a file the listing offers as a real record -- "+
						"a truncated policy loaded at run start, or a half-written agent reported as "+
						"a blueprint the user never wrote and cannot correct.\n"+
						"  Remedy: keep the wildcard last, as name+ext+\".tmp-*\".",
						displayPath(file), args)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no os.CreateTemp call was found in any store declaring `const ext`.\n" +
			"  Every content store wrote through CreateTemp when this test was written, so " +
			"finding none means the write moved or the call was renamed and this check now " +
			"holds over an empty set. Remedy: re-derive createTempArgs and extConst from the " +
			"form the stores now use.")
	}
}
