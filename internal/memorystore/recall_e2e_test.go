package memorystore_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/contextprep"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/memorystore"
	"github.com/michiTrader/arxi/internal/transcript"
	"github.com/michiTrader/arxi/internal/turn"
)

// prepareFor freezes a presentation for one run, retrieving memory for the
// given scopes the way a runtime would: retrieve, render, receipt, prepare.
func prepareFor(t *testing.T, store *memorystore.Store, runID string, scopes []memorystore.Scope) contextprep.Artifact {
	t.Helper()
	records, _, err := store.Retrieve(memorystore.Query{EvidenceClasses: []memorystore.EvidenceClass{memorystore.Stated}, Purposes: []memorystore.Purpose{memorystore.Operate}, Scopes: scopes})
	if err != nil {
		t.Fatalf("retrieve for %s: %v", runID, err)
	}
	const sha = "sha-effective-config"
	artifact, err := contextprep.Prepare(contextprep.Request{
		ContextID: "ctx-" + runID, RunID: runID, ParentWorkID: "w-1", EffectiveConfigSHA: sha,
		Effect: kernel.SpawnTurn{Agent: "coder", Context: kernel.ContextSpec{
			Identity: "backend", Cause: []string{"the user asked a question"}}},
		History:           transcript.Artifact{RunID: runID, Subject: "coder"},
		RetrievedMemory:   memorystore.Text(records),
		RetrievedReceipts: memorystore.Receipts(records, sha),
	})
	if err != nil {
		t.Fatalf("prepare for %s: %v", runID, err)
	}
	return artifact
}

func presented(artifact contextprep.Artifact) string {
	var b strings.Builder
	for _, m := range artifact.Messages {
		for _, block := range m.Content {
			b.WriteString(string(m.Role) + ":" + block.Text + "\n")
		}
	}
	return b.String()
}

// TestMemoryWrittenInOneRunIsPresentedInTheNext is the whole point of the
// package, asserted end to end through the real preparer rather than against
// the store's own return value.
//
// The store returning a record proves only that the store works. What the user
// asked for is that a *later run* sees it, and the only thing that can
// demonstrate that is a prepared presentation for a different run ID
// containing the text. Run 1 writes nothing and sees nothing; run 2 sees what
// was approved in between.
func TestMemoryWrittenInOneRunIsPresentedInTheNext(t *testing.T) {
	store, err := memorystore.Open(filepath.Join(t.TempDir(), "memory"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	scopes := []memorystore.Scope{{Principal: memorystore.User, ID: "ana"}}

	before := prepareFor(t, store, "run-1", scopes)
	if strings.Contains(presented(before), "Tuesdays") {
		t.Fatal("the first run already presented memory nobody stored")
	}
	if len(before.MemoryReceipts) != 0 {
		t.Fatalf("first run carried %d memory receipts with an empty store", len(before.MemoryReceipts))
	}

	if _, err := store.Approve("deploy-window", scopes[0], memorystore.Public, memorystore.Operate, memorystore.Stated, memorystore.Medium, memorystore.Permanent, "deploys land on Tuesdays", "ana"); err != nil {
		t.Fatalf("approve: %v", err)
	}

	after := prepareFor(t, store, "run-2", scopes)
	if !strings.Contains(presented(after), "deploys land on Tuesdays") {
		t.Fatalf("a second run did not present memory approved after the first run ended:\n%s\n"+
			"cross-run recall is the entire purpose of this store -- without this the agent "+
			"forgets between runs and every run starts from nothing", presented(after))
	}
	if len(after.MemoryReceipts) != 1 {
		t.Fatalf("second run carried %d memory receipts, want 1: the presentation influenced the "+
			"model with memory the artifact cannot attribute to a record version",
			len(after.MemoryReceipts))
	}
	receipt := after.MemoryReceipts[0]
	if receipt.RecordID != "deploy-window" || receipt.VersionID == "" {
		t.Fatalf("receipt names record %q version %q: an audit of this presentation cannot say "+
			"which stored version influenced it", receipt.RecordID, receipt.VersionID)
	}
}

// TestRecalledMemoryArrivesOnTheMemoryChannel holds ADR-0020 for retrieved
// memory, not only for frozen configuration memory.
//
// This is the assertion ADR-0025 exists because nobody had made: the channel
// decision reached one assembler of three, and the test that claimed to prove
// it asserted against hand-built literals instead of a real presentation. So
// this one reads the messages the preparer actually produced and checks the
// role, which is where the guarantee lives -- a system-role placement would be
// a structural grant of authority to data.
func TestRecalledMemoryArrivesOnTheMemoryChannel(t *testing.T) {
	store, err := memorystore.Open(filepath.Join(t.TempDir(), "memory"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	scope := memorystore.Scope{Principal: memorystore.User, ID: "ana"}
	if _, err := store.Approve("deploy-window", scope, memorystore.Public, memorystore.Operate, memorystore.Stated, memorystore.Medium, memorystore.Permanent, "deploys land on Tuesdays", "ana"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	artifact := prepareFor(t, store, "run-1", []memorystore.Scope{scope})

	var carriedBy []turn.Role
	for _, m := range artifact.Messages {
		for _, block := range m.Content {
			if strings.Contains(block.Text, "deploys land on Tuesdays") {
				carriedBy = append(carriedBy, m.Role)
			}
		}
	}
	if len(carriedBy) != 1 {
		t.Fatalf("retrieved memory appears in %d messages, want exactly 1: duplicated memory is "+
			"paid for twice and a receipt names one presentation of it", len(carriedBy))
	}
	if carriedBy[0] != turn.RoleUser {
		t.Fatalf("retrieved memory was presented on the %q channel, want %q: the system channel "+
			"is a structural grant of authority, so a retrieved record placed there is obeyed "+
			"for where it sits rather than for what its provenance says -- exactly the defect "+
			"ADR-0020 exists to remove and ADR-0025 found still live in two assemblers",
			carriedBy[0], turn.RoleUser)
	}
	// The system message must carry only operator-authored framing.
	for _, m := range artifact.Messages {
		if m.Role != turn.RoleSystem {
			continue
		}
		for _, block := range m.Content {
			if strings.Contains(block.Text, "deploys land on Tuesdays") {
				t.Fatalf("the system message contains retrieved memory:\n%s", block.Text)
			}
		}
	}
}

// TestPreparingRetrievedMemoryWithoutReceiptsIsRefused pins the half-and-half
// failures. Both directions are un-auditable, and ADR-0013 freezes the artifact
// once committed, so they must fail before the barrier rather than be corrected
// after it.
func TestPreparingRetrievedMemoryWithoutReceiptsIsRefused(t *testing.T) {
	base := func() contextprep.Request {
		return contextprep.Request{
			ContextID: "ctx-1", RunID: "run-1", ParentWorkID: "w-1",
			EffectiveConfigSHA: "sha-effective-config",
			Effect: kernel.SpawnTurn{Agent: "coder", Context: kernel.ContextSpec{
				Identity: "backend", Cause: []string{"a question"}}},
			History: transcript.Artifact{RunID: "run-1", Subject: "coder"},
		}
	}
	t.Run("memory without receipts", func(t *testing.T) {
		req := base()
		req.RetrievedMemory = "- [x] something the model will read\n"
		if _, err := contextprep.Prepare(req); err == nil {
			t.Fatal("a presentation carrying retrieved memory with no receipts was frozen: the " +
				"model is influenced by memory the artifact cannot attribute to any record " +
				"version, and the artifact is immutable once committed")
		}
	})
	t.Run("receipts without memory", func(t *testing.T) {
		req := base()
		req.RetrievedReceipts = []contextprep.MemoryReceipt{{
			Kind: contextprep.KindApprovedMemoryRecord, ContentDigest: "abc",
			RecordID: "r1", VersionID: "v1", EffectiveConfigSHA: "sha-effective-config"}}
		if _, err := contextprep.Prepare(req); err == nil {
			t.Fatal("a presentation carrying receipts with no memory was frozen: the artifact " +
				"claims an influence that never reached the model, which is a false audit " +
				"trail rather than a missing one")
		}
	})
}

// TestARetrievedCandidateCannotBePreparedIsRefusedAtTheBarrier checks the second
// of the two independent refusals protecting Phase 7's containment rule.
//
// Retrieve already filters candidates. This asserts the barrier refuses one
// anyway, because a single point of enforcement is a single point of
// regression: if a future retrieval path forgets the filter, the preparer must
// still refuse rather than present model-generated material as memory.
func TestARetrievedCandidateCannotBePreparedIsRefusedAtTheBarrier(t *testing.T) {
	req := contextprep.Request{
		ContextID: "ctx-1", RunID: "run-1", ParentWorkID: "w-1",
		EffectiveConfigSHA: "sha-effective-config",
		Effect: kernel.SpawnTurn{Agent: "coder", Context: kernel.ContextSpec{
			Identity: "backend", Cause: []string{"a question"}}},
		History:         transcript.Artifact{RunID: "run-1", Subject: "coder"},
		RetrievedMemory: "- [guess] the model believes something\n",
		RetrievedReceipts: []contextprep.MemoryReceipt{{
			Kind: contextprep.KindProposedMemoryCandidate, ContentDigest: "abc",
			RecordID: "r1", VersionID: "v1"}},
	}
	if _, err := contextprep.Prepare(req); err == nil {
		t.Fatal("a prepared context presented a model-proposed candidate: the containment rule " +
			"Phase 7 opens with says model material cannot create active memory, and a " +
			"candidate that reaches a model is active memory no matter what its kind says")
	}
}

// TestRecallIsScopedPerUserAcrossRuns is the leakage guarantee asserted through
// the presentation rather than through the store's return value, because the
// presentation is what actually reaches a provider.
func TestRecallIsScopedPerUserAcrossRuns(t *testing.T) {
	store, err := memorystore.Open(filepath.Join(t.TempDir(), "memory"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ana := memorystore.Scope{Principal: memorystore.User, ID: "ana"}
	if _, err := store.Approve("salary", ana, memorystore.Public, memorystore.Operate, memorystore.Stated, memorystore.Medium, memorystore.Permanent, "ana negotiated a raise in March", "ana"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	bruno := memorystore.Scope{Principal: memorystore.User, ID: "bruno"}
	artifact := prepareFor(t, store, "run-bruno", []memorystore.Scope{bruno})
	if strings.Contains(presented(artifact), "raise") {
		t.Fatalf("a run for user:bruno presented user:ana's memory:\n%s\nthe record reached a "+
			"provider, so the leak is not recoverable by fixing the store afterwards",
			presented(artifact))
	}
	if len(artifact.MemoryReceipts) != 0 {
		t.Fatalf("a run for user:bruno carried %d memory receipts", len(artifact.MemoryReceipts))
	}
}

// TestCorrectionReachesTheNextRun closes the loop the exit evidence describes:
// a correction stored between two runs must change what the second one is told.
func TestCorrectionReachesTheNextRun(t *testing.T) {
	store, err := memorystore.Open(filepath.Join(t.TempDir(), "memory"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	scope := memorystore.Scope{Principal: memorystore.User, ID: "ana"}
	if _, err := store.Approve("deploy-window", scope, memorystore.Public, memorystore.Operate, memorystore.Stated, memorystore.Medium, memorystore.Permanent, "deploys land on Tuesdays", "ana"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := store.Correct("deploy-window", "deploys land on Thursdays", "ana"); err != nil {
		t.Fatalf("correct: %v", err)
	}
	text := presented(prepareFor(t, store, "run-2", []memorystore.Scope{scope}))
	if strings.Contains(text, "Tuesdays") {
		t.Fatalf("a run after a correction was still told the stale fact:\n%s\nstale versions "+
			"must stop appearing after correction, and a model told both facts has no way to "+
			"prefer the corrected one", text)
	}
	if !strings.Contains(text, "Thursdays") {
		t.Fatalf("the corrected fact did not reach the run:\n%s", text)
	}
}

// TestDeletionReachesTheNextRun is the resurrection guarantee measured where it
// matters: the presentation, not the store.
func TestDeletionReachesTheNextRun(t *testing.T) {
	store, err := memorystore.Open(filepath.Join(t.TempDir(), "memory"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	scope := memorystore.Scope{Principal: memorystore.User, ID: "ana"}
	if _, err := store.Approve("deploy-window", scope, memorystore.Public, memorystore.Operate, memorystore.Stated, memorystore.Medium, memorystore.Permanent, "deploys land on Tuesdays", "ana"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := store.Delete("deploy-window", "ana"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	artifact := prepareFor(t, store, "run-2", []memorystore.Scope{scope})
	if strings.Contains(presented(artifact), "Tuesdays") {
		t.Fatalf("a deleted record was presented to a later run:\n%s", presented(artifact))
	}
	if len(artifact.MemoryReceipts) != 0 {
		t.Fatalf("a run after deletion carried %d memory receipts: the artifact attests to an "+
			"influence that a tombstone says must not exist", len(artifact.MemoryReceipts))
	}
}

// TestFrozenAndRetrievedMemoryCoexist checks the two sources do not displace
// each other. A blueprint's static prose and a store's records are different
// kinds of thing with different provenance, and an audit must see both.
func TestFrozenAndRetrievedMemoryCoexist(t *testing.T) {
	store, err := memorystore.Open(filepath.Join(t.TempDir(), "memory"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	scope := memorystore.Scope{Principal: memorystore.User, ID: "ana"}
	if _, err := store.Approve("deploy-window", scope, memorystore.Public, memorystore.Operate, memorystore.Stated, memorystore.Medium, memorystore.Permanent, "deploys land on Tuesdays", "ana"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	records, _, err := store.Retrieve(memorystore.Query{EvidenceClasses: []memorystore.EvidenceClass{memorystore.Stated}, Purposes: []memorystore.Purpose{memorystore.Operate}, Scopes: []memorystore.Scope{scope}})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	const sha = "sha-effective-config"
	artifact, err := contextprep.Prepare(contextprep.Request{
		ContextID: "ctx-1", RunID: "run-1", ParentWorkID: "w-1", EffectiveConfigSHA: sha,
		Effect: kernel.SpawnTurn{Agent: "coder", Context: kernel.ContextSpec{
			Identity: "backend", Memory: "the team prefers small pull requests",
			Cause: []string{"a question"}}},
		History:           transcript.Artifact{RunID: "run-1", Subject: "coder"},
		RetrievedMemory:   memorystore.Text(records),
		RetrievedReceipts: memorystore.Receipts(records, sha),
	})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	text := presented(artifact)
	if !strings.Contains(text, "small pull requests") {
		t.Fatalf("frozen configuration memory disappeared when retrieved memory was present:\n%s", text)
	}
	if !strings.Contains(text, "deploys land on Tuesdays") {
		t.Fatalf("retrieved memory disappeared when frozen memory was present:\n%s", text)
	}
	if len(artifact.MemoryReceipts) != 2 {
		t.Fatalf("got %d memory receipts, want 2 (one frozen, one governed): the artifact must "+
			"account for both sources separately, because they have different provenance and "+
			"only one of them can ever be corrected", len(artifact.MemoryReceipts))
	}
	var frozen, governed int
	for _, r := range artifact.MemoryReceipts {
		if r.Governed() {
			governed++
		} else {
			frozen++
		}
	}
	if frozen != 1 || governed != 1 {
		t.Fatalf("receipts split %d frozen / %d governed, want 1 and 1", frozen, governed)
	}
}
