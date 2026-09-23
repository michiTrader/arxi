package memorystore_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/michiTrader/arxi/internal/contextprep"
	"github.com/michiTrader/arxi/internal/memorystore"
)

func open(t *testing.T) *memorystore.Store {
	t.Helper()
	store, err := memorystore.Open(filepath.Join(t.TempDir(), "memory"))
	if err != nil {
		t.Fatalf("open memory store: %v", err)
	}
	return store
}

func user(id string) memorystore.Scope {
	return memorystore.Scope{Principal: memorystore.User, ID: id}
}

// TestCrossScopeRetrievalReturnsZeroRecords is the leakage guarantee Phase 7
// names first in its exit evidence: "cross-user and cross-project leakage tests
// return zero records".
//
// It asserts Considered as well as Authorized, because a leakage test that only
// checks the result is zero passes just as well against an empty store. The
// records must be present and refused, not absent.
func TestCrossScopeRetrievalReturnsZeroRecords(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("deploy-window", user("ana"), memorystore.Public, "deploys land on Tuesdays", "ana"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := store.Approve("secret", memorystore.Scope{Principal: memorystore.Project, ID: "atlas"},
		memorystore.Public, "the atlas key rotates monthly", "operator"); err != nil {
		t.Fatalf("approve: %v", err)
	}

	got, evidence, err := store.Retrieve(memorystore.Query{Scopes: []memorystore.Scope{user("bruno")}})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("retrieval for user:bruno returned %d records scoped to other principals: memory "+
			"leaked across a principal boundary, which is the one failure Phase 7 names first, and "+
			"a store that leaks is worse than a store that forgets", len(got))
	}
	if evidence.Considered != 2 {
		t.Fatalf("retrieval considered %d records, want 2: the two foreign records must be present "+
			"and refused for this to be evidence of containment -- as written the test would also "+
			"pass against an empty store, proving nothing", evidence.Considered)
	}
	if evidence.Authorized != 0 {
		t.Fatalf("retrieval authorized %d of 2 foreign records: authorization admitted a record "+
			"whose scope the caller does not hold", evidence.Authorized)
	}
}

// TestScopeMatchIsExactNotHierarchical pins the decision that a principal is
// never expanded into the scopes it appears to contain.
//
// Prefix or hierarchy matching is the tempting shortcut, and it is a leak:
// `project:arxi` would authorize `project:arxi-secret`. The containment mapping
// lives in whatever system owns identity, so inferring it here is this store
// guessing about a boundary it cannot see.
func TestScopeMatchIsExactNotHierarchical(t *testing.T) {
	store := open(t)
	secret := memorystore.Scope{Principal: memorystore.Project, ID: "arxi-secret"}
	if _, err := store.Approve("key", secret, memorystore.Public, "the signing key lives in vault", "operator"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	got, _, err := store.Retrieve(memorystore.Query{Scopes: []memorystore.Scope{
		{Principal: memorystore.Project, ID: "arxi"}}})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("scope project:arxi retrieved %d records scoped to project:arxi-secret: scope "+
			"matching became a prefix test, so every scope authorizes every longer scope that "+
			"starts with it", len(got))
	}
}

// TestCorrectionStopsTheStaleVersionFromAppearing is the second exit guarantee:
// "stale versions stop appearing after correction".
func TestCorrectionStopsTheStaleVersionFromAppearing(t *testing.T) {
	store := open(t)
	first, err := store.Approve("deploy-window", user("ana"), memorystore.Public, "deploys land on Tuesdays", "ana")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	second, err := store.Correct("deploy-window", "deploys land on Thursdays", "ana")
	if err != nil {
		t.Fatalf("correct: %v", err)
	}
	if second.Supersedes != first.VersionID {
		t.Fatalf("correction supersedes %q, want the first version %q: without a supersession "+
			"link the two versions are independent records and nothing is stale",
			second.Supersedes, first.VersionID)
	}
	got, _, err := store.Retrieve(memorystore.Query{Scopes: []memorystore.Scope{user("ana")}})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("retrieval returned %d versions of one corrected record, want 1: both the stale "+
			"and the corrected version are being presented, so the model receives two "+
			"contradictory memories and no way to prefer either", len(got))
	}
	if got[0].VersionID != second.VersionID {
		t.Fatalf("retrieval returned version %q, want the correction %q: the superseded version "+
			"is still being presented after a correction was stored, which is the exact failure "+
			"Phase 7's exit evidence forbids", got[0].VersionID, second.VersionID)
	}
	if strings.Contains(got[0].Body, "Tuesdays") {
		t.Fatalf("presented body is %q: it still carries the corrected-away text", got[0].Body)
	}
}

// TestDeletionLeavesATombstoneRatherThanRemovingFiles pins deletion as an
// append, which is what makes "deletion propagates without resurrection"
// achievable.
//
// Unlinking the versions would pass a naive "is it gone" assertion while
// leaving every replica and backup able to reintroduce the record, with nothing
// in the data saying it was deleted.
func TestDeletionLeavesATombstoneRatherThanRemovingFiles(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("deploy-window", user("ana"), memorystore.Public, "deploys land on Tuesdays", "ana"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	tomb, err := store.Delete("deploy-window", "ana")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !tomb.Deleted {
		t.Fatal("delete produced a version that is not marked deleted: the tombstone is the only " +
			"thing that tells another replica the record is gone")
	}
	got, evidence, err := store.Retrieve(memorystore.Query{Scopes: []memorystore.Scope{user("ana")}})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("retrieval returned %d records after deletion: a deleted record is still being "+
			"presented", len(got))
	}
	if evidence.Considered != 0 {
		t.Fatalf("retrieval considered %d records after deletion, want 0: a tombstone is the "+
			"absence of a record, and counting it as a considered record reports a deleted "+
			"record as present in the corpus", evidence.Considered)
	}
	// The earlier version must still be on disk: an audit asking what was
	// deleted has to be able to answer.
	versions, err := store.Versions()
	if err != nil {
		t.Fatalf("versions: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("store holds %d versions after one approval and one deletion, want 2: deletion "+
			"removed history, so nothing can say what the deleted record said", len(versions))
	}
}

// TestCorrectingATombstoneIsRefused closes the resurrection path that
// correction would otherwise open.
func TestCorrectingATombstoneIsRefused(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("gone", user("ana"), memorystore.Public, "original", "ana"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := store.Delete("gone", "ana"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Correct("gone", "back again", "ana"); err == nil {
		t.Fatal("correcting a deleted record succeeded: the record is live again under a new " +
			"version, which is the resurrection the deletion guarantee exists to prevent")
	}
}

// TestCandidatesAreStoredAndNeverRetrieved pins the containment rule Phase 7
// opens with: model material "may propose candidates but cannot create active
// memory".
func TestCandidatesAreStoredAndNeverRetrieved(t *testing.T) {
	store := open(t)
	candidate, err := store.Propose("guess", user("ana"), memorystore.Public, "ana probably prefers dark mode",
		"agent:coder", "run-1", 12)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if candidate.Kind != memorystore.Candidate {
		t.Fatalf("proposed record has kind %q, want %q: a model write that lands as approved "+
			"memory is the containment failure the phase names first", candidate.Kind,
			memorystore.Candidate)
	}
	got, evidence, err := store.Retrieve(memorystore.Query{Scopes: []memorystore.Scope{user("ana")}})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("retrieval returned %d model-proposed candidates: a candidate that can be "+
			"presented is not a candidate, and the model has just written its own memory", len(got))
	}
	if evidence.Authorized != 1 {
		t.Fatalf("retrieval authorized %d candidates, want 1: the candidate must pass "+
			"authorization and be refused on authority, otherwise this test proves the scope "+
			"filter works rather than the authority filter", evidence.Authorized)
	}
}

// TestPromotionSupersedesTheCandidateAndKeepsIt verifies the only sanctioned
// path from proposed to active memory.
func TestPromotionSupersedesTheCandidateAndKeepsIt(t *testing.T) {
	store := open(t)
	candidate, err := store.Propose("pref", user("ana"), memorystore.Public, "ana prefers dark mode",
		"agent:coder", "run-1", 12)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	promoted, err := store.Promote("pref", "ana")
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if promoted.Kind != memorystore.Approved {
		t.Fatalf("promoted record has kind %q, want approved", promoted.Kind)
	}
	if promoted.Supersedes != candidate.VersionID {
		t.Fatalf("promotion supersedes %q, want the candidate %q: an unlinked promotion leaves "+
			"the candidate current, so the record is both proposed and approved",
			promoted.Supersedes, candidate.VersionID)
	}
	if promoted.Origin != "ana" {
		t.Fatalf("promoted origin is %q, want the approving principal: recording the model as "+
			"the origin of an approved record would attribute the approval to the proposer",
			promoted.Origin)
	}
	got, _, err := store.Retrieve(memorystore.Query{Scopes: []memorystore.Scope{user("ana")}})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(got) != 1 || got[0].VersionID != promoted.VersionID {
		t.Fatalf("retrieval after promotion returned %d records: the promoted version must be "+
			"the one presented", len(got))
	}
	// The candidate version stays: an audit must be able to establish that a
	// presented record was originally model-generated.
	versions, err := store.Versions()
	if err != nil {
		t.Fatalf("versions: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("store holds %d versions after propose+promote, want 2: the candidate was "+
			"discarded, so nothing records that this memory began as a model proposal",
			len(versions))
	}
}

// TestPromotingAnApprovedRecordIsRefused keeps promotion meaning one transition.
func TestPromotingAnApprovedRecordIsRefused(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("fact", user("ana"), memorystore.Public, "ana works in CET", "ana"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := store.Promote("fact", "operator"); err == nil {
		t.Fatal("promoting an already-approved record succeeded: promotion is the proposed-to-" +
			"approved transition, and allowing it elsewhere makes it a no-op that looks like an " +
			"authorization event in the audit trail")
	}
}

// TestRetrievedRecordsProduceGovernedReceipts is the third exit guarantee:
// "every influence identifies its source and version". It asserts against
// contextprep's own validator rather than re-checking the fields here, so the
// store cannot satisfy a weaker rule than the barrier enforces.
func TestRetrievedRecordsProduceGovernedReceipts(t *testing.T) {
	store := open(t)
	stored, err := store.Approve("deploy-window", user("ana"), memorystore.Public, "deploys land on Tuesdays", "ana")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	got, _, err := store.Retrieve(memorystore.Query{Scopes: []memorystore.Scope{user("ana")}})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	receipts := memorystore.Receipts(got, "sha-of-effective-config")
	if len(receipts) != 1 {
		t.Fatalf("got %d receipts for 1 retrieved record", len(receipts))
	}
	receipt := receipts[0]
	if err := receipt.Validate(); err != nil {
		t.Fatalf("receipt for a retrieved record is not presentable evidence: %v\nthe preparer "+
			"refuses it at the barrier, so this record can never reach a model -- the store "+
			"produces memory the rest of the system cannot present", err)
	}
	if !receipt.Governed() {
		t.Fatal("receipt for a stored record does not report as governed: it would then be held " +
			"to the frozen-configuration rules, which do not require a version ID, and " +
			"correction propagation becomes unverifiable")
	}
	if receipt.VersionID != stored.VersionID || receipt.RecordID != "deploy-window" {
		t.Fatalf("receipt names record %q version %q, want %q/%q: a receipt that misidentifies "+
			"its source is worse than none, because an audit trusts it",
			receipt.RecordID, receipt.VersionID, "deploy-window", stored.VersionID)
	}
	if receipt.Kind != contextprep.KindApprovedMemoryRecord {
		t.Fatalf("receipt kind is %q, want %q", receipt.Kind, contextprep.KindApprovedMemoryRecord)
	}
}

// TestRankingPrefersTheMoreSpecificScope pins the ordering rule and, more
// importantly, that every selection states its reason -- the exit evidence
// requires receipts to record "ranking/index versions and reasons".
func TestRankingPrefersTheMoreSpecificScope(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("style", memorystore.Scope{Principal: memorystore.Tenant, ID: "acme"},
		memorystore.Public, "tenant standard is tabs", "operator"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := store.Approve("style-run", memorystore.Scope{Principal: memorystore.Run, ID: "r1"},
		memorystore.Public, "this run agreed on spaces", "ana"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	got, evidence, err := store.Retrieve(memorystore.Query{Scopes: []memorystore.Scope{
		{Principal: memorystore.Tenant, ID: "acme"}, {Principal: memorystore.Run, ID: "r1"}}})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d records, want 2", len(got))
	}
	if got[0].Scope.Principal != memorystore.Run {
		t.Fatalf("most specific record ranked %d, want first: the tenant-wide belief outranked "+
			"what this run established, so a general policy overrides a specific correction "+
			"the user just made", 1)
	}
	if len(evidence.Selections) != 2 {
		t.Fatalf("evidence carries %d selections for 2 records: a retrieval that cannot say why "+
			"it selected what it did is not auditable", len(evidence.Selections))
	}
	for _, sel := range evidence.Selections {
		if strings.TrimSpace(sel.Reason) == "" {
			t.Fatalf("selection of %s has no reason: the exit evidence requires retrieval "+
				"receipts to record ranking reasons", sel.VersionID)
		}
	}
	if evidence.RetrievalVersion == "" {
		t.Fatal("retrieval evidence names no ranking version: a later ranker change would be " +
			"indistinguishable from this one in the audit trail")
	}
}

// TestRetrievalIsDeterministic protects ADR-0013's reproducibility argument.
// A store whose order depends on directory iteration would make the prepared
// context a different artifact from the same inputs.
func TestRetrievalIsDeterministic(t *testing.T) {
	store := open(t)
	for _, id := range []string{"c", "a", "b", "d", "e"} {
		if _, err := store.Approve(id, user("ana"), memorystore.Public, "body "+id, "ana"); err != nil {
			t.Fatalf("approve %s: %v", id, err)
		}
	}
	first, _, err := store.Retrieve(memorystore.Query{Scopes: []memorystore.Scope{user("ana")}})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	for i := 0; i < 8; i++ {
		again, _, err := store.Retrieve(memorystore.Query{Scopes: []memorystore.Scope{user("ana")}})
		if err != nil {
			t.Fatalf("retrieve: %v", err)
		}
		for j := range first {
			if first[j].VersionID != again[j].VersionID {
				t.Fatalf("two retrievals over an unchanged store disagreed at position %d "+
					"(%s then %s): the prepared context is no longer a pure function of its "+
					"inputs, so the same run replays to a different presentation", j,
					first[j].VersionID, again[j].VersionID)
			}
		}
	}
}

// TestVersionIDIsContentAddressed pins that a version ID is derived rather than
// allocated, which is what lets two processes write without coordinating.
func TestVersionIDIsContentAddressed(t *testing.T) {
	a := open(t)
	b := open(t)
	one, err := a.Approve("fact", user("ana"), memorystore.Public, "ana works in CET", "ana")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	two, err := b.Approve("fact", user("ana"), memorystore.Public, "ana works in CET", "ana")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if one.VersionID != two.VersionID {
		t.Fatalf("two stores given the same write produced version IDs %q and %q: the ID is "+
			"allocated rather than derived, so a receipt is not portable between stores and two "+
			"writers cannot proceed without coordinating", one.VersionID, two.VersionID)
	}
	if one.VersionID == "" || one.ContentDigest == "" {
		t.Fatal("sealed record carries no version ID or content digest")
	}
}

// TestAModifiedVersionFileIsRefused protects the immutability that every
// issued receipt depends on.
func TestAModifiedVersionFileIsRefused(t *testing.T) {
	store := open(t)
	stored, err := store.Approve("fact", user("ana"), memorystore.Public, "ana works in CET", "ana")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	path := filepath.Join(store.Dir(), stored.VersionID+".json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read version: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode version: %v", err)
	}
	raw["body"] = "ana works in PST"
	tampered, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("encode tampered version: %v", err)
	}
	if err := os.WriteFile(path, tampered, 0o644); err != nil {
		t.Fatalf("write tampered version: %v", err)
	}
	if _, err := store.Versions(); err == nil {
		t.Fatal("a version file edited after it was written was accepted: every receipt naming " +
			"that version now describes content it never carried, and an audit reading the " +
			"store would confirm the wrong body as the presented one")
	}
}

// TestRewritingATamperedVersionIsRefused covers the collision O_EXCL reports.
//
// This test exists because a mutation replacing O_EXCL with O_TRUNC survived
// the entire suite. Probing why found a real defect rather than a missing
// assertion: a re-Put over a tampered file returned success and a valid-looking
// record while the disk still held the forged body, so the caller walked away
// with a receipt for content the store does not have. Content addressing makes
// the collision normally harmless, which is exactly why the one harmful case
// had no witness.
func TestRewritingATamperedVersionIsRefused(t *testing.T) {
	store := open(t)
	stored, err := store.Approve("fact", user("ana"), memorystore.Public, "ana works in CET", "ana")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	path := filepath.Join(store.Dir(), stored.VersionID+".json")
	if err := os.WriteFile(path, []byte(`{"record_id":"fact","body":"FORGED"}`), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	// Writing the same record again collides on the content-addressed name.
	if _, err := store.Approve("fact", user("ana"), memorystore.Public, "ana works in CET", "ana"); err == nil {
		t.Fatal("re-storing a record over a tampered version file succeeded: the caller receives " +
			"a receipt naming a version whose stored content is something else entirely, and " +
			"every later audit reads the forged body as the presented one")
	}
	// The forged bytes must still be on disk. Repairing the file would destroy
	// the only evidence that anything was wrong.
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read version after refusal: %v", err)
	}
	if !strings.Contains(string(body), "FORGED") {
		t.Fatalf("version file was rewritten on refusal (now %q): overwriting it erases the "+
			"evidence that a version somebody holds a receipt for was tampered with, which is "+
			"the one thing an audit of this failure would need", string(body))
	}
}

// TestIdenticalRePutIsANoOp keeps the refusal above from becoming a refusal of
// the benign collision. Content addressing means writing the same record twice
// is expected, and failing it would make every idempotent retry an error.
func TestIdenticalRePutIsANoOp(t *testing.T) {
	store := open(t)
	first, err := store.Approve("fact", user("ana"), memorystore.Public, "ana works in CET", "ana")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	second, err := store.Approve("fact", user("ana"), memorystore.Public, "ana works in CET", "ana")
	if err != nil {
		t.Fatalf("re-storing byte-identical content was refused: %v\ncontent addressing makes "+
			"this the expected collision, so failing it turns every retry after a crash into "+
			"an error the caller cannot resolve", err)
	}
	if first.VersionID != second.VersionID {
		t.Fatalf("identical writes produced versions %q and %q", first.VersionID, second.VersionID)
	}
	versions, err := store.Versions()
	if err != nil {
		t.Fatalf("versions: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("store holds %d versions after writing the same record twice, want 1", len(versions))
	}
}

// importFork writes two competing successors of one record straight to disk,
// bypassing Put.
//
// It has to bypass Put, because ADR-0028 made Put refuse the second one: a
// predecessor can be claimed by exactly one successor. The remaining way a
// fork reaches a store is the way ADR-0027 anticipated when it chose tombstones
// over unlinking — bytes arriving from a replica, a backup or a synced
// directory, where the competing versions were produced elsewhere and no claim
// travelled with them. That is what this writes, so the containment tests
// exercise the case that is still reachable rather than one the store now
// prevents.
func importFork(t *testing.T, store *memorystore.Store, recordID string, scope memorystore.Scope,
	predecessor string, bodies ...string) []string {
	t.Helper()
	var ids []string
	for _, body := range bodies {
		sealed, err := memorystore.Record{RecordID: recordID, Supersedes: predecessor,
			Scope: scope, Kind: memorystore.Approved, Sensitivity: memorystore.Public,
			Body: body, Origin: "replica"}.Seal()
		if err != nil {
			t.Fatalf("seal imported version %q: %v", body, err)
		}
		encoded, err := json.MarshalIndent(sealed, "", "  ")
		if err != nil {
			t.Fatalf("encode imported version %q: %v", body, err)
		}
		path := filepath.Join(store.Dir(), sealed.VersionID+".json")
		if err := os.WriteFile(path, append(encoded, '\n'), 0o644); err != nil {
			t.Fatalf("write imported version %q: %v", body, err)
		}
		ids = append(ids, sealed.VersionID)
	}
	return ids
}

// TestAForkedSupersessionChainIsRefused keeps ADR-0027's rule enforceable: a
// record with two current versions is never presented. Picking a winner by file
// order would be deterministic and unrelated to which correction the user meant.
//
// It asserts absence from the returned records rather than an error from
// Retrieve. Under ADR-0028 the error is gone on purpose — it was raised for the
// whole store, so it also refused every healthy record belonging to every other
// principal. The guarantee being protected was always "this record is not
// presented", and an error was only ever one way to achieve it.
func TestAForkedSupersessionChainIsRefused(t *testing.T) {
	store := open(t)
	first, err := store.Approve("fact", user("ana"), memorystore.Public, "original", "ana")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	importFork(t, store, "fact", user("ana"), first.VersionID, "correction one", "correction two")

	got, evidence, err := store.Retrieve(memorystore.Query{Scopes: []memorystore.Scope{user("ana")}})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	for _, r := range got {
		if r.RecordID == "fact" {
			t.Fatalf("a forked record was presented as version %q with body %q: two versions are "+
				"current, so the store picked one by file order -- a rule with no relationship "+
				"to which correction the user intended", r.VersionID, r.Body)
		}
	}
	// Withheld and said so. A record excluded with no trace is
	// indistinguishable from one that was never written, which would present a
	// correction that lost a race as memory the user never saved.
	if len(evidence.Forked) != 1 || evidence.Forked[0] != "fact" {
		t.Fatalf("retrieval evidence reports forked records %v, want [fact]: a record withheld "+
			"without evidence looks exactly like a record nobody ever stored, and silent loss "+
			"is the failure this store exists to prevent", evidence.Forked)
	}
}

// TestAForkedRecordDoesNotDenyRetrievalToOtherRecords is the blast-radius half
// of ADR-0028, and it is the defect that record was written about.
//
// tips() used to return one error for the whole set, so Retrieve failed before
// authorization ran. One corrupt record in one tenant denied memory to every
// tenant in the store, and the refusal named version IDs belonging to a record
// the caller was not authorized to see -- across the tenant boundary ADR-0027
// calls the one boundary no retrieval crosses.
func TestAForkedRecordDoesNotDenyRetrievalToOtherRecords(t *testing.T) {
	store := open(t)
	acme := memorystore.Scope{Principal: memorystore.Tenant, ID: "acme"}
	globex := memorystore.Scope{Principal: memorystore.Tenant, ID: "globex"}

	broken, err := store.Approve("shared-fact", acme, memorystore.Public, "original", "ana")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := store.Approve("unrelated", globex, memorystore.Public, "globex deploys on Fridays", "bo"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	forkIDs := importFork(t, store, "shared-fact", acme, broken.VersionID, "A", "B")

	got, evidence, err := store.Retrieve(memorystore.Query{Scopes: []memorystore.Scope{globex}})
	if err != nil {
		t.Fatalf("a fork in tenant:acme denied retrieval to tenant:globex: %v\n"+
			"  consequence: one damaged record is a denial of service for every other "+
			"principal in the store, and the message discloses version IDs across the tenant "+
			"boundary. Authorization must precede ranking; a store-wide failure precedes "+
			"authorization, which is that same argument violated from the other side.", err)
	}
	if len(got) != 1 || got[0].RecordID != "unrelated" {
		t.Fatalf("tenant:globex retrieved %d records %v, want its own single healthy record: "+
			"a fork in another tenant must not remove a caller's own memory", len(got), got)
	}
	// The other tenant's fork is not this caller's evidence, and naming it
	// would disclose the existence of a record it cannot read.
	if len(evidence.Forked) != 0 {
		t.Fatalf("retrieval for tenant:globex reported forked records %v belonging to another "+
			"tenant: that discloses the existence of records this caller is not authorized "+
			"for, which is the leak authorization-before-ranking exists to prevent",
			evidence.Forked)
	}

	// The operator's view does carry the version IDs, because resolving a fork
	// is impossible without them.
	forks, err := store.Forks()
	if err != nil {
		t.Fatalf("forks: %v", err)
	}
	if len(forks) != 1 || forks[0].RecordID != "shared-fact" {
		t.Fatalf("Forks() reported %v, want one entry for shared-fact: containment excludes the "+
			"record from retrieval, so this is the only place an operator can learn it needs "+
			"resolving -- without it the exclusion is silent loss", forks)
	}
	sort.Strings(forkIDs)
	if !reflect.DeepEqual(forks[0].Successors, forkIDs) {
		t.Fatalf("Forks() named successors %v, want %v: an operator resolves a fork by comparing "+
			"the competing versions, which requires both identities", forks[0].Successors, forkIDs)
	}
}

// TestAForkedRecordDoesNotDisableRepairOfHealthyRecords is the permanence half.
//
// Correct, Delete and Promote all resolve the current version through tip(),
// which called tips(), which is the function a fork made fail. So the three
// verbs that could repair a fork were the three a fork disabled, store-wide,
// and a probe confirmed no API call could clear it. Phase 7 promises
// "inspection, correction, supersession, export and deletion controls"; four of
// the five were gone after one fork.
func TestAForkedRecordDoesNotDisableRepairOfHealthyRecords(t *testing.T) {
	store := open(t)
	broken, err := store.Approve("broken", user("ana"), memorystore.Public, "original", "ana")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := store.Approve("healthy", user("ana"), memorystore.Public, "deploys land on Tuesdays", "ana"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	importFork(t, store, "broken", user("ana"), broken.VersionID, "A", "B")

	if _, err := store.Correct("healthy", "deploys land on Wednesdays", "ana"); err != nil {
		t.Fatalf("correcting a healthy record failed while another record was forked: %v\n"+
			"  consequence: one damaged record permanently disables correction of every other "+
			"record, including the corrections that would repair the damage.", err)
	}
	if _, err := store.Delete("healthy", "ana"); err != nil {
		t.Fatalf("deleting a healthy record failed while another record was forked: %v\n"+
			"  consequence: deletion is a Phase 7 control and a user right. A fork in an "+
			"unrelated record must not be able to withhold it.", err)
	}

	// The forked record itself stays unrepairable through Correct, and
	// deliberately: both verbs need a single current version to supersede, and
	// inventing one is the tiebreak ADR-0027 rejected. The refusal must name
	// the record so the operator knows where to look.
	_, err = store.Correct("broken", "reconciled", "ana")
	if err == nil {
		t.Fatal("correcting a forked record succeeded: Correct supersedes the current version, " +
			"and with two current versions it must have silently chosen one -- the tiebreak " +
			"ADR-0027 refused because no available rule relates to what the user meant")
	}
	if !strings.Contains(err.Error(), "broken") {
		t.Fatalf("the refusal for a forked record does not name it: %v\n"+
			"  consequence: the operator is told something is forked and not which record, so "+
			"Forks() is the only way to find out and the message wasted the chance to say.", err)
	}
}

// TestConcurrentCorrectionsCannotForkTheChain runs the race that produced
// ADR-0028.
//
// Correct reads the current tip and then writes a version superseding it. Two
// callers read the same tip and both write. Before the supersession claim both
// returned nil and the store was left with two current versions -- unreadable,
// and unrepairable through its own API. This raced to that state in three runs
// out of five.
//
// ADR-0027 states that being read and written by many runs is the store's whole
// point, and cites exactly that as why version IDs are content-addressed rather
// than allocated from a counter. The concurrency the ID scheme was chosen to
// support was the concurrency that destroyed the store.
func TestConcurrentCorrectionsCannotForkTheChain(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("fact", user("ana"), memorystore.Public, "original", "ana"); err != nil {
		t.Fatalf("approve: %v", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, body := range []string{"correction A", "correction B"} {
		wg.Add(1)
		go func(i int, body string) {
			defer wg.Done()
			_, errs[i] = store.Correct("fact", body, "ana")
		}(i, body)
	}
	wg.Wait()

	// Both may succeed only if they serialized -- the loser then re-read a tip
	// the winner had already written, which is a chain, not a fork. What must
	// never happen is a fork, so the store's readability is the assertion.
	got, evidence, err := store.Retrieve(memorystore.Query{Scopes: []memorystore.Scope{user("ana")}})
	if err != nil {
		t.Fatalf("two concurrent corrections left the store unreadable: %v\n"+
			"  consequence: both callers were told their correction succeeded, and the record "+
			"is now unrecoverable through this store's own API -- Correct, Delete and Promote "+
			"all resolve a tip and a forked record has two.", err)
	}
	if len(evidence.Forked) != 0 {
		t.Fatalf("two concurrent corrections forked record(s) %v: one predecessor must have one "+
			"successor, and the loser must be refused rather than both being accepted",
			evidence.Forked)
	}
	if len(got) != 1 {
		t.Fatalf("after two concurrent corrections the store holds %d current versions of one "+
			"record, want 1: more than one means the chain forked", len(got))
	}
	if errs[0] != nil && errs[1] != nil {
		t.Fatalf("both concurrent corrections were refused (%v / %v): one of two racing writers "+
			"must win, or a correction is lost whenever two arrive together", errs[0], errs[1])
	}
}

// TestASupersededVersionCannotBeClaimedTwice pins the refusal itself, apart
// from the race that motivates it.
//
// The race test above can pass by serializing, so on a lucky scheduler it never
// exercises the refusal at all. This one takes the claim deterministically and
// asserts the second writer is told what to do about it, and that nothing was
// written -- a refusal that leaves the losing version on disk would have forked
// the chain while reporting failure.
func TestASupersededVersionCannotBeClaimedTwice(t *testing.T) {
	store := open(t)
	first, err := store.Approve("fact", user("ana"), memorystore.Public, "original", "ana")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	winner, err := store.Correct("fact", "correction one", "ana")
	if err != nil {
		t.Fatalf("correct: %v", err)
	}

	loser := memorystore.Record{RecordID: "fact", Supersedes: first.VersionID, Scope: user("ana"),
		Kind: memorystore.Approved, Sensitivity: memorystore.Public, Body: "correction two", Origin: "ana"}
	_, err = store.Put(loser)
	if err == nil {
		t.Fatal("a second version superseding the same predecessor was accepted: one predecessor " +
			"with two successors makes both current, and no rule here can say which correction " +
			"the user intended")
	}
	// The loser has to be told to re-read, because a correction is a human
	// assertion about content: replaying it onto a version its author never
	// saw would silently overwrite the correction that won.
	if !strings.Contains(err.Error(), "Re-read") {
		t.Fatalf("the refusal does not tell the caller to re-read the current version: %v\n"+
			"  consequence: the obvious response to a rejected write is to retry it, and a "+
			"blind retry here reapplies a correction against a predecessor that is no longer "+
			"current -- overwriting the winner with stale intent.", err)
	}

	versions, err := store.Versions()
	if err != nil {
		t.Fatalf("versions: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("the store holds %d versions after a refused supersession, want 2: the refusal "+
			"must leave nothing behind, or it persists the losing version and forks the chain "+
			"while reporting failure", len(versions))
	}
	got, _, err := store.Retrieve(memorystore.Query{Scopes: []memorystore.Scope{user("ana")}})
	if err != nil {
		t.Fatalf("retrieve after a refused supersession: %v", err)
	}
	if len(got) != 1 || got[0].VersionID != winner.VersionID {
		t.Fatalf("after a refused supersession the current version is %v, want the winner %q: "+
			"the refusal must not disturb the correction that succeeded", got, winner.VersionID)
	}
}

// TestReapplyingAnIdenticalCorrectionIsIdempotent pins the benign collision, so
// the claim cannot later be tightened into something that breaks re-Put.
//
// Version IDs are content-addressed, so the same correction produces the same
// version. Refusing that would make every retry above this store fail, and
// ADR-0027 relies on re-Put of identical content being a no-op.
func TestReapplyingAnIdenticalCorrectionIsIdempotent(t *testing.T) {
	store := open(t)
	first, err := store.Approve("fact", user("ana"), memorystore.Public, "original", "ana")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	correction := memorystore.Record{RecordID: "fact", Supersedes: first.VersionID,
		Scope: user("ana"), Kind: memorystore.Approved, Sensitivity: memorystore.Public,
		Body: "corrected", Origin: "ana"}

	one, err := store.Put(correction)
	if err != nil {
		t.Fatalf("first correction: %v", err)
	}
	two, err := store.Put(correction)
	if err != nil {
		t.Fatalf("re-applying an identical correction was refused: %v\n"+
			"  consequence: version IDs are content-addressed, so a retry produces the same "+
			"version. Refusing it makes every retry path above this store fail on success.", err)
	}
	if one.VersionID != two.VersionID {
		t.Fatalf("identical corrections produced versions %q and %q", one.VersionID, two.VersionID)
	}
	versions, err := store.Versions()
	if err != nil {
		t.Fatalf("versions: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("the store holds %d versions after applying one correction twice, want 2",
			len(versions))
	}
}

// TestSubjectIsRefusedAsAPrincipal keeps ADR-0022's removal enforceable. The
// roadmap itself said `subject` before that ADR, so someone implementing
// against a stale copy will type it.
func TestSubjectIsRefusedAsAPrincipal(t *testing.T) {
	store := open(t)
	_, err := store.Approve("fact", memorystore.Scope{Principal: "subject", ID: "coder"}, memorystore.Public, "body", "ana")
	if err == nil {
		t.Fatal("a record scoped to `subject` was stored: `subject` already denotes the subject " +
			"agent in five committed artifact schemas, so this record's scope reads as a user " +
			"or tenant dimension while authorizing the agent one")
	}
	// Asserted against the reasoning, not against the words "user" and
	// "agent". Those two were the first version of this check and it did not
	// work: the generic unknown-principal message lists the whole vocabulary,
	// which contains both words, so deleting the dedicated `subject` branch
	// entirely left this test green. Measured with a mutation, not supposed --
	// the same defect class this package exists to close, found inside its own
	// suite. The phrase below appears only in the branch that explains why
	// `subject` specifically is refused.
	if !strings.Contains(err.Error(), "subject agent") {
		t.Fatalf("refusal of `subject` does not explain why it is excluded: %v\nit fell through "+
			"to the generic unknown-principal message, which reads as a typo and invites a "+
			"retry with the same word -- the reader has to learn that `subject` already means "+
			"the subject agent, or they will keep typing it", err)
	}
	if !strings.Contains(err.Error(), "`user`") || !strings.Contains(err.Error(), "`agent`") {
		t.Fatalf("refusal of `subject` does not name the replacements: %v\na reader who typed it "+
			"from the old vocabulary needs to be told which principal to use instead", err)
	}
	var scopeErr *memorystore.ErrScope
	if !errors.As(err, &scopeErr) {
		t.Fatalf("vocabulary failure is not an *ErrScope: %v\ncallers then have to match on "+
			"message text to tell a bad principal from a disk failure", err)
	}
}

// TestAnUnknownPrincipalGetsNoStanding is ADR-0023's failure direction applied
// to the scope vocabulary rather than the kind enumeration.
func TestAnUnknownPrincipalGetsNoStanding(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("fact", memorystore.Scope{Principal: "organisation", ID: "acme"},
		memorystore.Public, "body", "ana"); err == nil {
		t.Fatal("a record scoped to an unenumerated principal was stored: the vocabulary is " +
			"closed precisely so material nobody enumerated gets no standing, rather than the " +
			"standing of whichever real principal it sorts beside")
	}
	if _, _, err := store.Retrieve(memorystore.Query{Scopes: []memorystore.Scope{
		{Principal: "organisation", ID: "acme"}}}); err == nil {
		t.Fatal("a query for an unenumerated principal was answered: an unknown principal must " +
			"fail rather than authorize nothing silently, because a caller that believes it " +
			"asked for memory and received none cannot tell that from an empty store")
	}
}

// TestABareScopeIsNotAWildcard covers the leak where an empty ID is treated as
// "any holder in this dimension".
func TestABareScopeIsNotAWildcard(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("fact", memorystore.Scope{Principal: memorystore.User}, memorystore.Public, "body", "ana"); err == nil {
		t.Fatal("a record scoped to `user` with no id was stored: the bare principal names a " +
			"dimension rather than a holder, and a store that accepts it authorizes every user")
	}
	if _, err := store.Approve("fact", user("ana"), memorystore.Public, "body", "ana"); err != nil {
		t.Fatalf("approve with a valid scope: %v", err)
	}
	if _, _, err := store.Retrieve(memorystore.Query{Scopes: []memorystore.Scope{
		{Principal: memorystore.User}}}); err == nil {
		t.Fatal("a query scoped to `user` with no id was answered: had it matched, one caller " +
			"would read every user's memory")
	}
}

// TestFrozenConfigurationMemoryCannotBeStored keeps the two memory sources from
// collapsing into one. ADR-0021 left RecordID empty for frozen memory because a
// configuration field has no record identity; storing one here would mint the
// synthetic version ID that ADR explicitly refuses.
func TestFrozenConfigurationMemoryCannotBeStored(t *testing.T) {
	store := open(t)
	_, err := store.Put(memorystore.Record{RecordID: "from-blueprint", Scope: user("ana"),
		Kind: contextprep.KindFrozenContextMemory, Sensitivity: memorystore.Public,
		Body: "You remember the deploy window", Origin: "operator"})
	if err == nil {
		t.Fatal("frozen configuration memory was stored as a governed record: it would receive a " +
			"record and version ID that no blueprint change could ever correct, and downstream " +
			"nothing could distinguish it from a record a user actually approved")
	}
}

// TestAnEmptyBodyIsRefusedButATombstoneIsNot pins that the body requirement is
// scoped to live versions.
func TestAnEmptyBodyIsRefusedButATombstoneIsNot(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("empty", user("ana"), memorystore.Public, "   ", "ana"); err == nil {
		t.Fatal("a record with an empty body was stored: it would occupy the context window and " +
			"a retrieval receipt while influencing nothing -- cost with no evidence of benefit")
	}
	if _, err := store.Approve("real", user("ana"), memorystore.Public, "a real fact", "ana"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := store.Delete("real", "ana"); err != nil {
		t.Fatalf("a tombstone was refused for having no body: deletion would then require the "+
			"caller to invent text that retrieval must remember never to present: %v", err)
	}
}

// TestOriginIsRequired holds the "identifies its source" half of the exit
// evidence, which the scope does not cover: the scope says which holder the
// record belongs to, not who wrote it.
func TestOriginIsRequired(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("fact", user("ana"), memorystore.Public, "a fact", ""); err == nil {
		t.Fatal("a record with no origin was stored: the exit evidence requires every influence " +
			"to identify its source, and the scope names the holder rather than the author")
	}
}

// TestASeqWithoutItsRunIsRefused covers a binding that is meaningless alone.
func TestASeqWithoutItsRunIsRefused(t *testing.T) {
	store := open(t)
	_, err := store.Put(memorystore.Record{RecordID: "fact", Scope: user("ana"),
		Kind: memorystore.Candidate, Sensitivity: memorystore.Public, Body: "a guess", Origin: "agent:coder", CreatedSeq: 12})
	if err == nil {
		t.Fatal("a record carrying created_seq with no created_run was stored: a sequence number " +
			"is only meaningful inside one log, so it points at every run and none of them")
	}
}

// TestMissingRecordIsDistinguishable keeps ErrNotFound matchable, so callers do
// not have to compare message text to tell absence from failure.
func TestMissingRecordIsDistinguishable(t *testing.T) {
	store := open(t)
	_, err := store.Correct("never-stored", "body", "ana")
	if !errors.Is(err, memorystore.ErrNotFound) {
		t.Fatalf("correcting an unknown record returned %v, want ErrNotFound: a caller cannot "+
			"otherwise distinguish a record that does not exist from a store it failed to read, "+
			"and those need different handling", err)
	}
}

// TestTextRendersOneBodyForEveryAssembler pins the rendering seam ADR-0025's
// defect came through: three assemblers formatting memory their own way is how
// the channel decision reached one of them only.
func TestTextRendersOneBodyForEveryAssembler(t *testing.T) {
	store := open(t)
	if _, err := store.Approve("deploy-window", user("ana"), memorystore.Public, "deploys land on Tuesdays", "ana"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	got, _, err := store.Retrieve(memorystore.Query{Scopes: []memorystore.Scope{user("ana")}})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	text := memorystore.Text(got)
	if !strings.Contains(text, "deploys land on Tuesdays") {
		t.Fatalf("rendered memory %q does not contain the record body", text)
	}
	if !strings.Contains(text, "deploy-window") {
		t.Fatalf("rendered memory %q does not label the record: an operator comparing a "+
			"presentation against a receipt has nothing to join them on", text)
	}
	if memorystore.Text(nil) != "" {
		t.Fatal("rendering no records produced non-empty text: an assembler would then place an " +
			"empty memory message on the channel and the preparer would emit a receipt for " +
			"memory that does not exist")
	}
}

// obstructVersionWrite makes the version file that `body` would produce
// impossible to create, by occupying its exact path with a directory.
//
// This models the ordinary I/O failure -- a full disk, a permission change, a
// filesystem error -- rather than a crash or a tampered file. That distinction
// is the point: the claim residue it produces needs no hostile actor and no
// kill signal, only a write that did not finish, which is the failure mode any
// store on a real disk must survive.
func obstructVersionWrite(t *testing.T, store *memorystore.Store, recordID, predecessor string,
	scope memorystore.Scope, body, origin string) string {
	t.Helper()
	blocked, err := memorystore.Record{RecordID: recordID, Supersedes: predecessor, Scope: scope,
		Kind: memorystore.Approved, Sensitivity: memorystore.Public, Body: body, Origin: origin}.Seal()
	if err != nil {
		t.Fatalf("seal the version whose write is to be obstructed: %v", err)
	}
	path := filepath.Join(store.Dir(), blocked.VersionID+".json")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("obstruct version path %s: %v", path, err)
	}
	return blocked.VersionID
}

// TestAFailedWriteReleasesTheClaimItTook is the defect ADR-0029 was written
// about, and it is the half ADR-0028 did not measure.
//
// ADR-0028 preferred a claim file to a lock because a claim "needs no release:
// it is the durable record of a fact that does not expire -- that predecessor
// now has a successor". When the write fails, that fact never became true. The
// claim that outlives it is exactly the stale lock the ADR rejected locking to
// avoid, and it froze Correct and Delete permanently for a record whose chain
// was entirely healthy.
func TestAFailedWriteReleasesTheClaimItTook(t *testing.T) {
	store := open(t)
	first, err := store.Approve("deploy-window", user("ana"), memorystore.Public, "deploys land on Tuesdays", "ana")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	obstructVersionWrite(t, store, "deploy-window", first.VersionID, user("ana"),
		"deploys land on Wednesdays", "ana")

	if _, err := store.Correct("deploy-window", "deploys land on Wednesdays", "ana"); err == nil {
		t.Fatal("a correction whose version file could not be written reported success: the " +
			"caller would believe a correction was stored that is on no disk")
	}

	// The claim must be gone, because the successor it names never existed.
	claims, err := store.Claims()
	if err != nil {
		t.Fatalf("claims: %v", err)
	}
	for _, c := range claims {
		if c.Predecessor == first.VersionID {
			t.Fatalf("the failed write left a supersession claim on %s naming successor %s, "+
				"which was never written: Correct and Delete both supersede the tip, so this "+
				"record is now frozen against every repair verb -- permanently, for a chain "+
				"that never forked", c.Predecessor, c.Successor)
		}
	}

	// And the record must still be correctable, which is the consequence a
	// user actually feels.
	corrected, err := store.Correct("deploy-window", "deploys land on Thursdays", "ana")
	if err != nil {
		t.Fatalf("correcting after a failed write was refused (%v): the record was frozen by "+
			"the leftover exclusion of a write that never completed, so a transient disk "+
			"error became permanent data loss for that record", err)
	}
	if corrected.Body != "deploys land on Thursdays" {
		t.Fatalf("correction stored body %q, want the corrected text", corrected.Body)
	}
}

// TestAFailedWriteDoesNotFreezeDeletion is the deletion half. Correct and
// Delete are separate verbs and a user locked out of deletion cannot exercise
// the "deletion propagates without resurrection" guarantee Phase 7 exits on.
func TestAFailedWriteDoesNotFreezeDeletion(t *testing.T) {
	store := open(t)
	first, err := store.Approve("secret", user("ana"), memorystore.Public, "the original", "ana")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	obstructVersionWrite(t, store, "secret", first.VersionID, user("ana"), "amended", "ana")
	if _, err := store.Correct("secret", "amended", "ana"); err == nil {
		t.Fatal("the obstructed correction reported success")
	}

	if _, err := store.Delete("secret", "ana"); err != nil {
		t.Fatalf("deleting after a failed correction was refused (%v): a user asking for their "+
			"memory to be deleted would be told no, because an unrelated write failed earlier "+
			"-- and deletion is the one control Phase 7 requires to always work", err)
	}
	got, _, err := store.Retrieve(memorystore.Query{Scopes: []memorystore.Scope{user("ana")}})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	for _, r := range got {
		if r.RecordID == "secret" {
			t.Fatalf("a deleted record was still presented as %q", r.Body)
		}
	}
}

// TestAFulfilledClaimIsNeverReleased pins the boundary of the rollback. The
// claim of a write that SUCCEEDED is load-bearing -- it is the whole exclusion
// ADR-0028 added -- and releasing it would let a second successor supersede the
// same predecessor, recreating by hand the fork that ADR made unreachable.
func TestAFulfilledClaimIsNeverReleased(t *testing.T) {
	store := open(t)
	first, err := store.Approve("fact", user("ana"), memorystore.Public, "original", "ana")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := store.Correct("fact", "corrected", "ana"); err != nil {
		t.Fatalf("correct: %v", err)
	}

	err = store.ReleaseClaim(first.VersionID)
	if err == nil {
		t.Fatalf("releasing the fulfilled claim on %s succeeded: its successor exists, so the "+
			"next write may now supersede the same predecessor and fork the chain -- which is "+
			"the exact state ADR-0028 made unreachable through this API", first.VersionID)
	}
	if !strings.Contains(err.Error(), "fork") {
		t.Fatalf("the refusal %q does not tell the operator that releasing a fulfilled claim "+
			"forks the chain, which is the consequence that makes it refusable", err)
	}
	claims, err := store.Claims()
	if err != nil {
		t.Fatalf("claims: %v", err)
	}
	var found bool
	for _, c := range claims {
		if c.Predecessor == first.VersionID {
			found = true
			if !c.Fulfilled {
				t.Fatalf("the claim on %s reports unfulfilled, but its successor %s was "+
					"written: an operator reading this would release a load-bearing claim "+
					"and fork the record", c.Predecessor, c.Successor)
			}
		}
	}
	if !found {
		t.Fatalf("the fulfilled claim on %s vanished from Claims(): the refusal above depends "+
			"on it still being there", first.VersionID)
	}
}

// TestAnUnfulfilledClaimIsVisibleAndReleasable covers the residue no in-process
// rollback can reach: a crash between taking the claim and writing the version.
//
// Before ADR-0029 this state was permanent AND invisible. A fork is two
// versions and Forks() reports it; this is zero versions, so Forks saw nothing,
// Versions skipped the claim because it does not end in .json, and Retrieve
// went on serving the pre-correction body as current while every repair verb
// refused.
func TestAnUnfulfilledClaimIsVisibleAndReleasable(t *testing.T) {
	store := open(t)
	first, err := store.Approve("fact", user("ana"), memorystore.Public, "original", "ana")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	// The exact disk state a process leaves when it dies after claiming and
	// before writing: a claim naming a version that is on no disk.
	ghost := "mv-000000000000000000000000"
	claimPath := filepath.Join(store.Dir(), first.VersionID+".claim")
	if err := os.WriteFile(claimPath, []byte(ghost+"\n"), 0o644); err != nil {
		t.Fatalf("write the stranded claim: %v", err)
	}

	// It must be visible. This is the verb whose absence made the freeze
	// undiagnosable.
	claims, err := store.Claims()
	if err != nil {
		t.Fatalf("claims: %v", err)
	}
	var stranded *memorystore.Claim
	for i := range claims {
		if claims[i].Predecessor == first.VersionID {
			stranded = &claims[i]
		}
	}
	if stranded == nil {
		t.Fatalf("Claims() does not report the stranded claim on %s: no other verb can see it "+
			"either -- Forks needs two versions and this has none, Versions ignores .claim "+
			"files -- so the record is frozen with nothing in the package able to say why",
			first.VersionID)
	}
	if stranded.Fulfilled {
		t.Fatalf("the claim on %s naming absent successor %s reports fulfilled: an operator "+
			"would leave it in place and the record stays frozen forever",
			stranded.Predecessor, stranded.Successor)
	}
	if stranded.RecordID != "fact" {
		t.Fatalf("the stranded claim reports record %q, want \"fact\": an operator holding a "+
			"version ID and no record ID cannot tell whose memory is frozen", stranded.RecordID)
	}

	// The refusal must tell the truth: this is not a fork.
	_, err = store.Correct("fact", "a correction", "ana")
	if err == nil {
		t.Fatal("correcting past a stranded claim succeeded: the claim is the exclusion that " +
			"keeps one predecessor to one successor, so bypassing it forks the chain")
	}
	if strings.Contains(err.Error(), "already superseded by") {
		t.Fatalf("the refusal %q says the predecessor is already superseded by a version that "+
			"was never written: it asserts a supersession that did not happen and sends the "+
			"operator looking for a version on no disk", err)
	}
	if !strings.Contains(err.Error(), "ReleaseClaim") {
		t.Fatalf("the refusal %q does not name the verb that repairs it: a permanent freeze "+
			"whose remedy is unnamed is a freeze the operator cannot lift", err)
	}

	// And releasing it must restore the record.
	if err := store.ReleaseClaim(first.VersionID); err != nil {
		t.Fatalf("releasing the stranded claim on %s failed: %v", first.VersionID, err)
	}
	corrected, err := store.Correct("fact", "a correction", "ana")
	if err != nil {
		t.Fatalf("correcting after the claim was released was still refused (%v): the repair "+
			"verb does not repair", err)
	}
	if corrected.Body != "a correction" {
		t.Fatalf("stored body %q, want the correction", corrected.Body)
	}
}

// TestReleasingAClaimThatIsNotHeldIsRefused keeps the repair verb honest.
// Reporting success for a claim that was never there would tell an operator
// the freeze was lifted while the real cause is untouched.
func TestReleasingAClaimThatIsNotHeldIsRefused(t *testing.T) {
	store := open(t)
	if err := store.ReleaseClaim("mv-000000000000000000000000"); err == nil {
		t.Fatal("releasing a claim that is not held reported success: an operator would " +
			"believe a frozen record was repaired and stop looking for the real cause")
	}
	if err := store.ReleaseClaim(""); err == nil {
		t.Fatal("releasing an unnamed predecessor reported success")
	}
}

// TestTheRollbackDoesNotStealAnotherWritersClaim is the safety boundary on the
// rollback, and it is why the release is scoped to the claim this call created.
//
// A rule of "the successor is missing, so take the claim" cannot distinguish an
// abandoned claim from a writer a few microseconds into its own Put. Under
// concurrency that rule lets two writers supersede one predecessor, which is
// the fork the store refuses to resolve.
func TestTheRollbackDoesNotStealAnotherWritersClaim(t *testing.T) {
	for attempt := 0; attempt < 10; attempt++ {
		store := open(t)
		first, err := store.Approve("fact", user("ana"), memorystore.Public, "original", "ana")
		if err != nil {
			t.Fatalf("approve: %v", err)
		}
		// One correction will fail on a blocked path; the other must succeed.
		// If the failing one released the winner's claim, the chain forks.
		obstructVersionWrite(t, store, "fact", first.VersionID, user("ana"), "blocked", "ana")

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); store.Correct("fact", "blocked", "ana") }()
		go func() { defer wg.Done(); store.Correct("fact", "allowed", "ana") }()
		wg.Wait()

		forks, err := store.Forks()
		if err != nil {
			t.Fatalf("forks: %v", err)
		}
		if len(forks) != 0 {
			t.Fatalf("attempt %d: a failing write released a claim it did not own, so two "+
				"versions now supersede %s and the record is forked: %+v -- the rollback "+
				"reintroduced the exact race ADR-0028 closed", attempt, first.VersionID, forks)
		}
	}
}

// TestAFailedWriteNeverReleasesAClaimItDidNotCreate is why the rollback is
// scoped to the claim this call created, and it was added because a mutation
// removing that scope survived the rest of this file.
//
// The sequence is three writers, and only the third makes the damage visible:
// writer A takes the claim on P naming successor X and is still in flight; B
// attempts the identical correction, so it meets A's claim as the benign
// content-addressed collision and does not own it; B's write then fails. If B
// released A's claim on the way out, A still goes on to write X, but P is now
// unclaimed -- so a later writer C supersedes P with a different version and
// the chain forks. Content addressing hides this from any two-writer test,
// because A and B produce identical bytes; it takes a third, differing
// correction to expose it.
func TestAFailedWriteNeverReleasesAClaimItDidNotCreate(t *testing.T) {
	store := open(t)
	first, err := store.Approve("fact", user("ana"), memorystore.Public, "original", "ana")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}

	// Writer A: in flight, claim taken, version not yet written. Its successor
	// is the version the identical correction below would produce.
	inFlight := obstructVersionWrite(t, store, "fact", first.VersionID, user("ana"),
		"shared correction", "ana")
	claimPath := filepath.Join(store.Dir(), first.VersionID+".claim")
	if err := os.WriteFile(claimPath, []byte(inFlight+"\n"), 0o644); err != nil {
		t.Fatalf("write the in-flight claim: %v", err)
	}

	// Writer B: the same correction, so it meets A's claim as the benign
	// collision rather than as a conflict, and its own write then fails.
	if _, err := store.Correct("fact", "shared correction", "ana"); err == nil {
		t.Fatal("the obstructed correction reported success")
	}

	claims, err := store.Claims()
	if err != nil {
		t.Fatalf("claims: %v", err)
	}
	var held bool
	for _, c := range claims {
		if c.Predecessor == first.VersionID && c.Successor == inFlight {
			held = true
		}
	}
	if !held {
		t.Fatalf("a failed write released the claim on %s that another writer created and is "+
			"still in flight on: once that writer lands successor %s, the predecessor is "+
			"superseded but no longer claimed, so the next differing correction supersedes it "+
			"too and the chain forks -- the exact race ADR-0028 closed, reopened by the "+
			"rollback meant to repair a different one", first.VersionID, inFlight)
	}
}

// TestAnIdempotentRePutKeepsTheClaimItCreated is the other boundary of the
// rollback, and it was added because a mutation releasing the claim on this
// path survived every other test here.
//
// The path is a re-Put whose version file already exists with identical bytes.
// It is a SUCCESS, not a failure, so the rollback must not run -- but the claim
// it created a moment earlier is the exclusion protecting that predecessor, and
// dropping it would leave a superseded version unclaimed. The next differing
// correction would then supersede it a second time and fork the chain.
//
// It is reachable: claims do not travel with replicated bytes, so a successor
// imported from a replica arrives with no claim beside it, and re-Putting that
// content is how a claim gets created for an already-present version.
func TestAnIdempotentRePutKeepsTheClaimItCreated(t *testing.T) {
	store := open(t)
	first, err := store.Approve("fact", user("ana"), memorystore.Public, "original", "ana")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}

	// A successor that arrived as bytes from a replica: version file present,
	// no claim beside it.
	sealed, err := memorystore.Record{RecordID: "fact", Supersedes: first.VersionID,
		Scope: user("ana"), Kind: memorystore.Approved, Sensitivity: memorystore.Public,
		Body: "imported correction", Origin: "replica"}.Seal()
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	encoded, err := json.MarshalIndent(sealed, "", "  ")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := os.WriteFile(filepath.Join(store.Dir(), sealed.VersionID+".json"),
		append(encoded, '\n'), 0o644); err != nil {
		t.Fatalf("write imported version: %v", err)
	}

	// Re-Put of that identical content: creates the claim, then finds the
	// version already on disk and returns success.
	if _, err := store.Put(sealed); err != nil {
		t.Fatalf("re-Put of an identical imported version was refused (%v): content addressing "+
			"makes this the benign collision, and refusing it would break every retry", err)
	}

	claims, err := store.Claims()
	if err != nil {
		t.Fatalf("claims: %v", err)
	}
	var guarded bool
	for _, c := range claims {
		if c.Predecessor == first.VersionID && c.Successor == sealed.VersionID {
			guarded = true
		}
	}
	if !guarded {
		t.Fatalf("a successful re-Put dropped the claim it created on %s: that predecessor is "+
			"superseded by %s and now carries no exclusion, so the next differing correction "+
			"supersedes it a second time and forks the chain -- the rollback is for writes "+
			"that FAILED, and this one succeeded", first.VersionID, sealed.VersionID)
	}
}
