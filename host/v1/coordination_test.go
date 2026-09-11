package v1

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type memoryCoordination struct {
	mu          sync.Mutex
	bindings    map[string]SubmissionBinding
	jobs        map[JobID]bool
	claims      map[JobID]ExecutionClaim
	nextFence   uint64
	claimErr    error
	completions []ExecutionOutcome
}

func newMemoryCoordination() *memoryCoordination {
	return &memoryCoordination{bindings: map[string]SubmissionBinding{}, jobs: map[JobID]bool{}, claims: map[JobID]ExecutionClaim{}}
}

func (c *memoryCoordination) RegisterJob(_ context.Context, id JobID) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.jobs[id] = true
	return nil
}

func (c *memoryCoordination) BindSubmission(_ context.Context, wanted SubmissionBinding) (SubmissionBinding, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.bindings[wanted.Key]; ok {
		if old.RequestDigest != wanted.RequestDigest {
			return SubmissionBinding{}, ErrStorageConflict
		}
		return old, nil
	}
	c.bindings[wanted.Key] = wanted
	return wanted, nil
}
func (c *memoryCoordination) Claim(_ context.Context, id JobID) (ExecutionClaim, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.claimErr != nil {
		return ExecutionClaim{}, c.claimErr
	}
	if !c.jobs[id] {
		return ExecutionClaim{}, ErrJobNotFound
	}
	if _, active := c.claims[id]; active {
		return ExecutionClaim{}, ErrStorageConflict
	}
	c.nextFence++
	claim := ExecutionClaim{JobID: id, AttemptID: "attempt", Fence: c.nextFence}
	c.claims[id] = claim
	return claim, nil
}
func (c *memoryCoordination) Validate(_ context.Context, claim ExecutionClaim) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.claims[claim.JobID] != claim {
		return ErrStorageConflict
	}
	return nil
}
func (c *memoryCoordination) Heartbeat(ctx context.Context, claim ExecutionClaim) error {
	return c.Validate(ctx, claim)
}
func (c *memoryCoordination) Checkpoint(ctx context.Context, checkpoint ExecutionCheckpoint) error {
	return c.Validate(ctx, checkpoint.Claim)
}
func (c *memoryCoordination) Complete(ctx context.Context, claim ExecutionClaim, outcome ExecutionOutcome) error {
	if err := c.Validate(ctx, claim); err != nil {
		return err
	}
	c.mu.Lock()
	c.completions = append(c.completions, outcome)
	delete(c.claims, claim.JobID)
	c.mu.Unlock()
	return nil
}
func (c *memoryCoordination) Close() error { return nil }

type coordinatedMemoryStorage struct {
	*memoryStorage
	coordination *memoryCoordination
}

func (s *coordinatedMemoryStorage) OpenClaimedWriter(ctx context.Context, claim ExecutionClaim) (JobWriter, error) {
	writer, err := s.OpenWriter(ctx, claim.JobID)
	if err != nil {
		return nil, err
	}
	return &validatingMemoryWriter{JobWriter: writer, validate: func(ctx context.Context) error {
		return s.coordination.Validate(ctx, claim)
	}}, nil
}

type validatingMemoryWriter struct {
	JobWriter
	validate func(context.Context) error
}

func (w *validatingMemoryWriter) Append(ctx context.Context, batch AppendBatch) (AppendResult, error) {
	if err := w.validate(ctx); err != nil {
		return AppendResult{}, ErrStorageConflict
	}
	return w.JobWriter.Append(ctx, batch)
}
func (w *validatingMemoryWriter) WriteSnapshot(ctx context.Context, snapshot Snapshot) error {
	if err := w.validate(ctx); err != nil {
		return ErrStorageConflict
	}
	return w.JobWriter.WriteSnapshot(ctx, snapshot)
}

func TestCoordinatedSubmitRegistersEveryJobBeforeClaim(t *testing.T) {
	coordination := newMemoryCoordination()
	storage := &coordinatedMemoryStorage{memoryStorage: newMemoryStorage(), coordination: coordination}
	h := New(Options{Storage: storage, Coordination: coordination, Provider: textProviderStub{}, CoordinationHeartbeat: time.Hour})
	defer h.Close()
	result, err := h.Submit(context.Background(), SubmitRequest{Blueprint: testBlueprint, Prompt: "work", BudgetUSD: 1, Simulated: true})
	if err != nil {
		t.Fatalf("coordinated submission without a caller key failed: %v: every accepted job needs durable registration before it can be claimed", err)
	}
	coordination.mu.Lock()
	registered := coordination.jobs[result.JobID]
	coordination.mu.Unlock()
	if !registered {
		t.Fatal("submitted job was claimed without durable registration: restart recovery would lose an accepted unkeyed job")
	}
}

func TestSubmitIdempotencyBindsCanonicalRequest(t *testing.T) {
	coordination := newMemoryCoordination()
	storage := &coordinatedMemoryStorage{memoryStorage: newMemoryStorage(), coordination: coordination}
	h := New(Options{Storage: storage, Coordination: coordination, Provider: textProviderStub{}, CoordinationHeartbeat: time.Hour})
	defer h.Close()
	req := SubmitRequest{Blueprint: testBlueprint, Prompt: "work", BudgetUSD: 1, Simulated: true, IdempotencyKey: "request-1"}
	first, err := h.Submit(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.Submit(context.Background(), req)
	if err != nil || second.JobID != first.JobID {
		t.Fatalf("identical submission = %#v, %v; an idempotency retry must return the original job", second, err)
	}
	req.Prompt = "different"
	if _, err := h.Submit(context.Background(), req); !IsCode(err, CodeConflict) {
		t.Fatalf("conflicting submission error = %v, want conflict: one key cannot authorize different work", err)
	}
}

func TestCoordinatedSubmitCapabilityRequiresFenceableStorage(t *testing.T) {
	coordination := newMemoryCoordination()
	unsafe := New(Options{Storage: newMemoryStorage(), Coordination: coordination, Provider: textProviderStub{}})
	defer unsafe.Close()
	set, err := unsafe.Capabilities(context.Background(), CapabilitiesRequest{})
	if err != nil || set.Has(CapabilitySubmit) || set.Has(CapabilityRecover) {
		t.Fatalf("unsafe coordinated capabilities = %#v, %v: Submit and Recover must not be advertised when storage cannot fence writers", set, err)
	}
	if _, err := unsafe.Submit(context.Background(), SubmitRequest{Blueprint: testBlueprint, Prompt: "work", BudgetUSD: 1}); !IsCode(err, CodeCapabilityUnavailable) {
		t.Fatalf("unsafe coordinated Submit returned %v: capability validation must fail before publishing an accepted job", err)
	}
}

func TestRecoveryCapabilityRequiresCoordinationAndFencedStorage(t *testing.T) {
	legacy := New(Options{Storage: newMemoryStorage(), Provider: textProviderStub{}})
	defer legacy.Close()
	set, err := legacy.Capabilities(context.Background(), CapabilitiesRequest{})
	if err != nil || set.Has(CapabilityRecover) {
		t.Fatalf("legacy capabilities = %#v, %v: process-local execution must not advertise restart recovery", set, err)
	}
	coordination := newMemoryCoordination()
	durable := New(Options{Storage: &coordinatedMemoryStorage{newMemoryStorage(), coordination}, Coordination: coordination, Provider: textProviderStub{}})
	defer durable.Close()
	set, err = durable.Capabilities(context.Background(), CapabilitiesRequest{})
	if err != nil || !set.Has(CapabilityRecover) {
		t.Fatalf("durable capabilities = %#v, %v: installed fenced coordination must advertise recovery", set, err)
	}
}

func TestWaitWithCoordinationWaitsForActiveClaimInsteadOfReportingNotResident(t *testing.T) {
	coordination := newMemoryCoordination()
	storage := &coordinatedMemoryStorage{memoryStorage: newMemoryStorage(), coordination: coordination}
	provider := &blockingTextProvider{started: make(chan struct{}), release: make(chan struct{})}
	creator := New(Options{Storage: storage, Coordination: coordination,
		Provider: provider, CoordinationHeartbeat: time.Hour})
	result, err := creator.Submit(context.Background(), SubmitRequest{Blueprint: testBlueprint, Prompt: "work", BudgetUSD: 1, Simulated: true})
	if err != nil {
		t.Fatal(err)
	}
	waiter := New(Options{Storage: storage, Coordination: coordination, Provider: textProviderStub{}, CoordinationHeartbeat: time.Hour})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = waiter.Wait(ctx, WaitRequest{JobID: result.JobID})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait behind active claim returned %v: coordinated hosts must wait, not report a nonresident job", err)
	}
	close(provider.release)
	_ = waiter.Close()
	_ = creator.Close()
}

var _ Coordination = (*memoryCoordination)(nil)
var _ CoordinatedJobStorageV1 = (*coordinatedMemoryStorage)(nil)
