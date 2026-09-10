// Package v1 defines arxi's supported Phase 1 embedding API.
//
// The package owns its DTOs and extension contracts. Callers do not need, and
// cannot receive, values from arxi's internal reducer, executor, or stores.
package v1

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Options supplies optional extension ports. Storage is the Phase 1 lifecycle
// boundary: when present, all acceptance, reads, mutations, waits, and
// subscriptions use it.
type Options struct {
	Provider   TextProvider
	Tools      ToolExecutor
	Workspaces WorkspaceProvisioner
	Storage    JobStorage
	Authorizer Authorizer
	// Now supplies the lifecycle event clock. It is primarily useful to composition
	// roots that already expose a deterministic clock; nil uses time.Now.
	Now func() time.Time
}

// Host is an in-process arxi host.
type Host struct {
	mu      sync.RWMutex
	backend backend
	closed  bool
	closing bool
}

// New returns a host whose installed lifecycle capabilities are determined by
// the supplied storage and provider ports.
func New(options Options) *Host {
	return &Host{backend: newBackend(options)}
}

// backend is the private application-service seam. Embedding applications
// implement extension ports, not arxi's lifecycle.
type backend interface {
	Submit(context.Context, SubmitRequest) (SubmitResult, error)
	Inspect(context.Context, InspectRequest) (Job, error)
	Cancel(context.Context, CancelRequest) (Job, error)
	Approve(context.Context, ApproveRequest) (Job, error)
	Reject(context.Context, RejectRequest) (Job, error)
	Answer(context.Context, AnswerRequest) (Job, error)
	Wait(context.Context, WaitRequest) (Job, error)
	Subscribe(context.Context, SubscribeRequest) (Subscription, error)
	Capabilities(context.Context, CapabilitiesRequest) (CapabilitySet, error)
	Close() error
}

func (h *Host) current(operation string) (backend, error) {
	if h == nil {
		return nil, newError(CodeCapabilityUnavailable, operation, "host is not initialized", nil)
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.closed {
		return nil, newError(CodeConflict, operation, "host is closed", nil)
	}
	if h.backend == nil {
		return nil, newError(CodeCapabilityUnavailable, operation,
			fmt.Sprintf("capability %q is not installed", Capability(operation)), nil)
	}
	return h.backend, nil
}

// Submit accepts a job for background execution.
func (h *Host) Submit(ctx context.Context, req SubmitRequest) (SubmitResult, error) {
	app, err := h.current(string(CapabilitySubmit))
	if err != nil {
		return SubmitResult{}, err
	}
	return app.Submit(ctx, req)
}

// Inspect returns the selected public projection of a job.
func (h *Host) Inspect(ctx context.Context, req InspectRequest) (Job, error) {
	app, err := h.current(string(CapabilityInspect))
	if err != nil {
		return Job{}, err
	}
	return app.Inspect(ctx, req)
}

// Cancel requests cancellation at the next safe execution boundary.
func (h *Host) Cancel(ctx context.Context, req CancelRequest) (Job, error) {
	app, err := h.current(string(CapabilityCancel))
	if err != nil {
		return Job{}, err
	}
	return app.Cancel(ctx, req)
}

// Approve approves one pending approval item.
func (h *Host) Approve(ctx context.Context, req ApproveRequest) (Job, error) {
	app, err := h.current(string(CapabilityApprove))
	if err != nil {
		return Job{}, err
	}
	return app.Approve(ctx, req)
}

// Reject rejects one pending approval item.
func (h *Host) Reject(ctx context.Context, req RejectRequest) (Job, error) {
	app, err := h.current(string(CapabilityReject))
	if err != nil {
		return Job{}, err
	}
	return app.Reject(ctx, req)
}

// Answer answers one pending question item.
func (h *Host) Answer(ctx context.Context, req AnswerRequest) (Job, error) {
	app, err := h.current(string(CapabilityAnswer))
	if err != nil {
		return Job{}, err
	}
	return app.Answer(ctx, req)
}

// Wait waits until a job reaches a terminal state or the context ends.
func (h *Host) Wait(ctx context.Context, req WaitRequest) (Job, error) {
	app, err := h.current(string(CapabilityWait))
	if err != nil {
		return Job{}, err
	}
	return app.Wait(ctx, req)
}

// Subscribe observes confirmed events after an exclusive logical sequence.
func (h *Host) Subscribe(ctx context.Context, req SubscribeRequest) (Subscription, error) {
	app, err := h.current(string(CapabilitySubscribe))
	if err != nil {
		return nil, err
	}
	return app.Subscribe(ctx, req)
}

// Capabilities returns the installed and authorized lifecycle snapshot. An
// unwired Phase 1 host truthfully returns an empty snapshot.
func (h *Host) Capabilities(ctx context.Context, req CapabilitiesRequest) (CapabilitySet, error) {
	if h == nil {
		return CapabilitySet{}, newError(CodeCapabilityUnavailable, "capabilities", "host is not initialized", nil)
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.closed {
		return CapabilitySet{}, newError(CodeConflict, "capabilities", "host is closed", nil)
	}
	if h.backend == nil {
		return CapabilitySet{Capabilities: []Capability{}}, nil
	}
	return h.backend.Capabilities(ctx, req)
}

// Close releases host-owned application resources. It is idempotent.
func (h *Host) Close() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	if h.closed && !h.closing {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	h.closing = true
	app := h.backend
	h.mu.Unlock()
	var err error
	if app != nil {
		err = app.Close()
	}
	if err == nil {
		h.mu.Lock()
		h.closing = false
		h.mu.Unlock()
	}
	return err
}
