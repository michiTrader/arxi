package capability

import "context"

// Snapshot is an immutable effective capability view for one principal. Its
// unexported storage cannot be changed by callers and Names always returns a
// copy. It is suitable for both hello advertisement and later dispatch.
type Snapshot struct {
	registry    *Registry
	principal   Principal
	authorizer  Authorizer
	allowed     map[Name]struct{}
	unavailable map[Name]Outcome
	names       []Name
}

// Snapshot computes declared ∩ installed ∩ authorized. Denied capabilities,
// concealed or not, are omitted. Authorization errors abort construction so a
// partial view is never advertised as authoritative.
func (r *Registry) Snapshot(ctx context.Context, principal Principal, auth Authorizer) (Snapshot, error) {
	s := Snapshot{
		registry: r, principal: clonePrincipal(principal), authorizer: auth,
		allowed: make(map[Name]struct{}), unavailable: make(map[Name]Outcome),
	}
	if r == nil {
		return s, nil
	}
	for _, name := range r.names {
		if _, installed := r.installed[name]; !installed {
			s.unavailable[name] = Uninstalled
			continue
		}
		decision := Decision{Allowed: true}
		var err error
		if auth != nil {
			decision, err = auth.Authorize(ctx, Request{Principal: clonePrincipal(s.principal), Capability: name})
			if err != nil {
				return Snapshot{}, err
			}
		}
		if decision.Allowed {
			s.allowed[name] = struct{}{}
			s.names = append(s.names, name)
		} else if decision.Conceal {
			s.unavailable[name] = Concealed
		} else {
			s.unavailable[name] = Denied
		}
	}
	return s, nil
}

// Names returns a fresh, stably ordered copy of the effective set.
func (s Snapshot) Names() []Name { return append([]Name(nil), s.names...) }

// Has reports membership in this immutable snapshot.
func (s Snapshot) Has(name Name) bool {
	_, ok := s.allowed[name]
	return ok
}

// Resolve checks vocabulary before any authorization, then preserves the
// distinction between an uninstalled handler and denied access. For effective
// members, resource authorization is rechecked on every resource-specific
// dispatch with the principal and authorizer captured by Snapshot. A concealed
// denial has the stable Concealed outcome.
func (s Snapshot) Resolve(ctx context.Context, name Name, resource string) Resolution {
	if s.registry == nil {
		return Resolution{Name: name, Outcome: Unknown}
	}
	if _, declared := s.registry.declared[name]; !declared {
		return Resolution{Name: name, Outcome: Unknown}
	}
	handler, installed := s.registry.installed[name]
	if !installed {
		return Resolution{Name: name, Outcome: Uninstalled}
	}
	if outcome, unavailable := s.unavailable[name]; unavailable {
		return Resolution{Name: name, Outcome: outcome}
	}
	if _, allowed := s.allowed[name]; !allowed {
		return Resolution{Name: name, Outcome: Denied}
	}
	if resource != "" && s.authorizer != nil {
		decision, err := s.authorizer.Authorize(ctx, Request{
			Principal: clonePrincipal(s.principal), Capability: name, Resource: resource,
		})
		if err != nil {
			return Resolution{Name: name, Outcome: Denied, Cause: err}
		}
		if !decision.Allowed {
			outcome := Denied
			if decision.Conceal {
				outcome = Concealed
			}
			return Resolution{Name: name, Outcome: outcome}
		}
	}
	return Resolution{Name: name, Outcome: Available, Handler: handler}
}

func clonePrincipal(p Principal) Principal {
	out := Principal{ID: p.ID}
	if p.Attributes != nil {
		out.Attributes = make(map[string]string, len(p.Attributes))
		for key, value := range p.Attributes {
			out.Attributes[key] = value
		}
	}
	return out
}
