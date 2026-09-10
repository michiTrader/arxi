package v1

import (
	"context"
	"errors"
	"testing"
)

type retryCloseBackend struct {
	backend
	calls int
}

func (b *retryCloseBackend) Close() error {
	b.calls++
	if b.calls == 1 {
		return errors.New("still closing")
	}
	return nil
}

func TestHostCloseRetriesBackendWithoutReopeningOperations(t *testing.T) {
	b := &retryCloseBackend{}
	h := &Host{backend: b}
	if err := h.Close(); err == nil {
		t.Fatal("first Close returned nil")
	}
	if _, err := h.current("inspect"); err == nil {
		t.Fatal("operation reopened after failed Close")
	}
	if err := h.Close(); err != nil {
		t.Fatalf("retry Close: %v", err)
	}
	if b.calls != 2 {
		t.Fatalf("backend Close calls = %d, want 2", b.calls)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("idempotent Close: %v", err)
	}
	if b.calls != 2 {
		t.Fatalf("backend Close calls after success = %d, want 2", b.calls)
	}
}

func TestHostWithoutStorageReportsNoCapabilities(t *testing.T) {
	h := New(Options{})
	caps, err := h.Capabilities(context.Background(), CapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(caps.Capabilities) != 0 {
		t.Fatalf("capabilities = %#v", caps)
	}
	_, err = h.Inspect(context.Background(), InspectRequest{JobID: "missing"})
	if ErrorCodeOf(err) != CodeCapabilityUnavailable {
		t.Fatalf("inspect error = %v", err)
	}
}
