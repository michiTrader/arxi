package memorystore_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	if _, err := store.Approve("deploy-window", user("ana"), "deploys land on Tuesdays", "ana"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := store.Approve("secret", memorystore.Scope{Principal: memorystore.Project, ID: "atlas"},
		"the atlas key rotates monthly", "operator"); err != nil {
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
	if _, err := store.Approve("key", secret, "the signing key lives in vault", "operator"); err != nil {
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
	first, err := store.Approve("deploy-window", user("ana"), "deploys land on Tuesdays", "ana")
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
	if _, err := store.Approve("deploy-window", user("ana"), "deploys land on Tuesdays", "ana"); err != nil {
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
	if _, err := store.Approve("gone", user("ana"), "original", "ana"); err != nil {
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
	candidate, err := store.Propose("guess", user("ana"), "ana probably prefers dark mode",
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
	candidate, err := store.Propose("pref", user("ana"), "ana prefers dark mode",
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
	if _, err := store.Approve("fact", user("ana"), "ana works in CET", "ana"); err != nil {
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
	stored, err := store.Approve("deploy-window", user("ana"), "deploys land on Tuesdays", "ana")
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
		"tenant standard is tabs", "operator"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := store.Approve("style-run", memorystore.Scope{Principal: memorystore.Run, ID: "r1"},
		"this run agreed on spaces", "ana"); err != nil {
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
		if _, err := store.Approve(id, user("ana"), "body "+id, "ana"); err != nil {
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
	one, err := a.Approve("fact", user("ana"), "ana works in CET", "ana")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	two, err := b.Approve("fact", user("ana"), "ana works in CET", "ana")
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
	stored, err := store.Approve("fact", user("ana"), "ana works in CET", "ana")
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
	stored, err := store.Approve("fact", user("ana"), "ana works in CET", "ana")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	path := filepath.Join(store.Dir(), stored.VersionID+".json")
	if err := os.WriteFile(path, []byte(`{"record_id":"fact","body":"FORGED"}`), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	// Writing the same record again collides on the content-addressed name.
	if _, err := store.Approve("fact", user("ana"), "ana works in CET", "ana"); err == nil {
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
	first, err := store.Approve("fact", user("ana"), "ana works in CET", "ana")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	second, err := store.Approve("fact", user("ana"), "ana works in CET", "ana")
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

// TestAForkedSupersessionChainIsRefused covers the case where two corrections
// race. Picking a winner by file order would be deterministic and unrelated to
// which correction the user meant.
func TestAForkedSupersessionChainIsRefused(t *testing.T) {
	store := open(t)
	first, err := store.Approve("fact", user("ana"), "original", "ana")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	for _, body := range []string{"correction one", "correction two"} {
		if _, err := store.Put(memorystore.Record{RecordID: "fact", Supersedes: first.VersionID,
			Scope: user("ana"), Kind: memorystore.Approved, Body: body, Origin: "ana"}); err != nil {
			t.Fatalf("put %q: %v", body, err)
		}
	}
	if _, _, err := store.Retrieve(memorystore.Query{Scopes: []memorystore.Scope{user("ana")}}); err == nil {
		t.Fatal("a forked supersession chain was retrieved without complaint: two versions are " +
			"current, so the store silently picked one by file order -- a rule with no " +
			"relationship to which correction the user intended")
	}
}

// TestSubjectIsRefusedAsAPrincipal keeps ADR-0022's removal enforceable. The
// roadmap itself said `subject` before that ADR, so someone implementing
// against a stale copy will type it.
func TestSubjectIsRefusedAsAPrincipal(t *testing.T) {
	store := open(t)
	_, err := store.Approve("fact", memorystore.Scope{Principal: "subject", ID: "coder"}, "body", "ana")
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
		"body", "ana"); err == nil {
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
	if _, err := store.Approve("fact", memorystore.Scope{Principal: memorystore.User}, "body", "ana"); err == nil {
		t.Fatal("a record scoped to `user` with no id was stored: the bare principal names a " +
			"dimension rather than a holder, and a store that accepts it authorizes every user")
	}
	if _, err := store.Approve("fact", user("ana"), "body", "ana"); err != nil {
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
		Kind: contextprep.KindFrozenContextMemory, Body: "You remember the deploy window", Origin: "operator"})
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
	if _, err := store.Approve("empty", user("ana"), "   ", "ana"); err == nil {
		t.Fatal("a record with an empty body was stored: it would occupy the context window and " +
			"a retrieval receipt while influencing nothing -- cost with no evidence of benefit")
	}
	if _, err := store.Approve("real", user("ana"), "a real fact", "ana"); err != nil {
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
	if _, err := store.Approve("fact", user("ana"), "a fact", ""); err == nil {
		t.Fatal("a record with no origin was stored: the exit evidence requires every influence " +
			"to identify its source, and the scope names the holder rather than the author")
	}
}

// TestASeqWithoutItsRunIsRefused covers a binding that is meaningless alone.
func TestASeqWithoutItsRunIsRefused(t *testing.T) {
	store := open(t)
	_, err := store.Put(memorystore.Record{RecordID: "fact", Scope: user("ana"),
		Kind: memorystore.Candidate, Body: "a guess", Origin: "agent:coder", CreatedSeq: 12})
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
	if _, err := store.Approve("deploy-window", user("ana"), "deploys land on Tuesdays", "ana"); err != nil {
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
