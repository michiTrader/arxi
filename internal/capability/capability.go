// Package capability resolves declared, installed, and authorized operations.
package capability

import (
	"context"
	"errors"
	"sort"
)

// Name is a stable operation identifier.
type Name string

// Handler is deliberately opaque here. A transport or application adapter owns
// the invocation signature and type-asserts the value after resolution.
type Handler any

// Principal is authorization input that does not couple this package to a host
// or transport DTO.
type Principal struct {
	ID         string
	Attributes map[string]string
}

// Request asks about capability access. Resource is empty for discovery and is
// populated when dispatch rechecks access to a specific resource.
type Request struct {
	Principal  Principal
	Capability Name
	Resource   string
}

// Decision separates denial from concealment. Conceal affects the public
// outcome, never whether the request was denied.
type Decision struct {
	Allowed bool
	Conceal bool
}

// Authorizer evaluates general and resource-scoped access.
type Authorizer interface {
	Authorize(context.Context, Request) (Decision, error)
}

type AuthorizeFunc func(context.Context, Request) (Decision, error)

func (f AuthorizeFunc) Authorize(ctx context.Context, req Request) (Decision, error) {
	return f(ctx, req)
}

// Outcome is a stable resolution category.
type Outcome string

const (
	Available   Outcome = "available"
	Unknown     Outcome = "unknown"
	Uninstalled Outcome = "uninstalled"
	Denied      Outcome = "denied"
	Concealed   Outcome = "concealed"
)

// Resolution is the result of resolving one name. Handler is populated only
// for Available. Cause is reserved for authorizer failures.
type Resolution struct {
	Name    Name
	Outcome Outcome
	Handler Handler
	Cause   error
}

func (r Resolution) Available() bool { return r.Outcome == Available }
func (r Resolution) Unwrap() error   { return r.Cause }

var ErrInvalidDeclaration = errors.New("invalid capability declaration")

// Registry is an immutable declared vocabulary and installed-handler map.
type Registry struct {
	declared  map[Name]struct{}
	installed map[Name]Handler
	names     []Name
}

// NewRegistry copies all inputs and rejects empty/duplicate declarations.
// Installed handlers outside the declared vocabulary are retained only as
// implementation detail: resolution still classifies their names as Unknown
// before authorization. A nil handler is not installed.
func NewRegistry(declared []Name, installed map[Name]Handler) (*Registry, error) {
	r := &Registry{declared: make(map[Name]struct{}, len(declared)), installed: make(map[Name]Handler)}
	for _, name := range declared {
		if name == "" {
			return nil, ErrInvalidDeclaration
		}
		if _, exists := r.declared[name]; exists {
			return nil, ErrInvalidDeclaration
		}
		r.declared[name] = struct{}{}
		r.names = append(r.names, name)
	}
	for name, handler := range installed {
		if handler != nil {
			r.installed[name] = handler
		}
	}
	sort.Slice(r.names, func(i, j int) bool { return r.names[i] < r.names[j] })
	return r, nil
}

// Declared returns a stable copy of the vocabulary.
func (r *Registry) Declared() []Name {
	if r == nil {
		return nil
	}
	return append([]Name(nil), r.names...)
}
