package v1

import (
	"context"
	"errors"
	"testing"
	"time"
)

type blockingTextProvider struct {
	started chan struct{}
	release chan struct{}
}

func (p *blockingTextProvider) CompleteText(context.Context, TextRequest) (TextResponse, error) {
	select {
	case <-p.started:
	default:
		close(p.started)
	}
	<-p.release
	return TextResponse{Text: "done"}, nil
}

func TestStoragePortDrivesFullLifecycle(t *testing.T) {
	storage := newMemoryStorage()
	h := New(Options{Storage: storage, Provider: textProviderStub{}})
	defer h.Close()
	result, err := h.Submit(context.Background(), SubmitRequest{Blueprint: testBlueprint, Prompt: "work", BudgetUSD: 1, Simulated: true})
	if err != nil {
		t.Fatal(err)
	}
	job, err := h.Wait(context.Background(), WaitRequest{JobID: result.JobID})
	if err != nil {
		t.Fatal(err)
	}
	if !job.Terminal || job.Status != JobSucceeded {
		t.Fatalf("job = %#v", job)
	}
	stored, err := storage.Load(context.Background(), result.JobID)
	if err != nil || stored.ID != result.JobID {
		t.Fatalf("storage load = %#v, %v", stored, err)
	}
	batch, err := storage.ReadConfirmed(context.Background(), result.JobID, ConfirmedRead{})
	if err != nil || len(batch.Records) < 2 {
		t.Fatalf("confirmed batch = %#v, %v", batch, err)
	}
}

func TestStoragePortPreservesExclusiveWriterAndResidentMutation(t *testing.T) {
	storage := newMemoryStorage()
	provider := &blockingTextProvider{started: make(chan struct{}), release: make(chan struct{})}
	h := New(Options{Storage: storage, Provider: provider})
	defer h.Close()
	result, err := h.Submit(context.Background(), SubmitRequest{Blueprint: testBlueprint, Prompt: "work", BudgetUSD: 1})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-provider.started:
	case <-time.After(time.Second):
		t.Fatal("provider did not start")
	}
	if _, err := storage.OpenWriter(context.Background(), result.JobID); !errors.Is(err, ErrStorageConflict) {
		t.Fatalf("second writer error = %v", err)
	}
	cancelled := make(chan struct {
		job Job
		err error
	}, 1)
	go func() {
		job, err := h.Cancel(context.Background(), CancelRequest{JobID: result.JobID})
		cancelled <- struct {
			job Job
			err error
		}{job, err}
	}()
	select {
	case <-cancelled:
		t.Fatal("cancel returned during external call")
	case <-time.After(20 * time.Millisecond):
	}
	close(provider.release)
	select {
	case got := <-cancelled:
		if got.err != nil || got.job.Status != JobCancelled {
			t.Fatalf("cancel = %#v, %v", got.job, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not reach safe boundary")
	}
}

func TestStoragePortRejectsMutationAfterSuccessfulWorkerCompletion(t *testing.T) {
	storage := newMemoryStorage()
	h := New(Options{Storage: storage, Provider: textProviderStub{}})
	defer h.Close()
	result, err := h.Submit(context.Background(), SubmitRequest{
		Blueprint: testBlueprint, Prompt: "work", BudgetUSD: 1, Simulated: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Wait(context.Background(), WaitRequest{JobID: result.JobID}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Cancel(context.Background(), CancelRequest{JobID: result.JobID}); !IsCode(err, CodeAlreadyTerminal) {
		t.Fatalf("cancel after completion error = %v, want already_terminal", err)
	}
}

type textProviderStub struct{}

func (textProviderStub) CompleteText(context.Context, TextRequest) (TextResponse, error) {
	return TextResponse{Text: "done"}, nil
}

const testBlueprint = "name: example\nmembers:\n  - {name: worker}\nstages:\n  - {name: work, advance_when: all}\n"
