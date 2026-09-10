package v1

import (
	"context"

	"github.com/michiTrader/arxi/internal/capability"
)

var phaseOneCapabilities = []capability.Name{
	capability.Name(CapabilitySubmit),
	capability.Name(CapabilityInspect),
	capability.Name(CapabilityCancel),
	capability.Name(CapabilityApprove),
	capability.Name(CapabilityReject),
	capability.Name(CapabilityAnswer),
	capability.Name(CapabilityWait),
	capability.Name(CapabilitySubscribe),
}

// capabilityResolver adapts host requests to internal capability resolution.
type capabilityResolver struct {
	registry *capability.Registry
	auth     Authorizer
}

func newCapabilityResolver(installed map[Capability]any, auth Authorizer) (*capabilityResolver, error) {
	internalInstalled := make(map[capability.Name]capability.Handler, len(installed))
	for name, handler := range installed {
		internalInstalled[capability.Name(name)] = handler
	}
	registry, err := capability.NewRegistry(phaseOneCapabilities, internalInstalled)
	if err != nil {
		return nil, err
	}
	return &capabilityResolver{registry: registry, auth: auth}, nil
}

func (r *capabilityResolver) snapshot(ctx context.Context, principal Principal) (capabilitySnapshot, error) {
	if r == nil {
		return capabilitySnapshot{}, nil
	}
	snapshot, err := r.registry.Snapshot(ctx, internalPrincipal(principal), authorizerAdapter{r.auth})
	if err != nil {
		return capabilitySnapshot{}, err
	}
	return capabilitySnapshot{snapshot: snapshot}, nil
}

type capabilitySnapshot struct{ snapshot capability.Snapshot }

func (s capabilitySnapshot) set() CapabilitySet {
	names := s.snapshot.Names()
	out := CapabilitySet{Capabilities: make([]Capability, len(names))}
	for i, name := range names {
		out.Capabilities[i] = Capability(name)
	}
	return out
}

func (s capabilitySnapshot) resolve(ctx context.Context, name Capability, jobID JobID) (any, ErrorCode, error) {
	resolved := s.snapshot.Resolve(ctx, capability.Name(name), string(jobID))
	switch resolved.Outcome {
	case capability.Available:
		return resolved.Handler, "", nil
	case capability.Unknown, capability.Concealed:
		return nil, CodeNotFound, resolved.Cause
	case capability.Uninstalled:
		return nil, CodeCapabilityUnavailable, resolved.Cause
	default:
		return nil, CodePermissionDenied, resolved.Cause
	}
}

type authorizerAdapter struct{ authorizer Authorizer }

func (a authorizerAdapter) Authorize(ctx context.Context, req capability.Request) (capability.Decision, error) {
	if a.authorizer == nil {
		return capability.Decision{Allowed: true}, nil
	}
	decision, err := a.authorizer.Authorize(ctx, AuthorizationRequest{
		Principal:  clonePrincipal(Principal{ID: req.Principal.ID, Attributes: req.Principal.Attributes}),
		Capability: Capability(req.Capability), JobID: JobID(req.Resource),
	})
	return capability.Decision{Allowed: decision.Allowed, Conceal: decision.Conceal}, err
}

func internalPrincipal(principal Principal) capability.Principal {
	cloned := clonePrincipal(principal)
	return capability.Principal{ID: cloned.ID, Attributes: cloned.Attributes}
}
