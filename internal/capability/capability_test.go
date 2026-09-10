package capability

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestDeclaredInstalledAuthorizedTable(t *testing.T) {
	for _, tc := range []struct {
		name      string
		declared  bool
		installed bool
		allowed   bool
		conceal   bool
		want      Outcome
		inSet     bool
		calls     int
	}{
		{"000", false, false, false, false, Unknown, false, 0},
		{"001", false, false, true, false, Unknown, false, 0},
		{"010", false, true, false, false, Unknown, false, 0},
		{"011", false, true, true, false, Unknown, false, 0},
		{"100", true, false, false, false, Uninstalled, false, 0},
		{"101", true, false, true, false, Uninstalled, false, 0},
		{"110", true, true, false, false, Denied, false, 1},
		{"110-conceal", true, true, false, true, Concealed, false, 1},
		{"111", true, true, true, false, Available, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var declared []Name
			installed := map[Name]Handler{}
			if tc.declared {
				declared = []Name{"known"}
			}
			if tc.installed {
				installed["known"] = "handler"
			}
			r, err := NewRegistry(declared, installed)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			auth := AuthorizeFunc(func(context.Context, Request) (Decision, error) {
				calls++
				return Decision{Allowed: tc.allowed, Conceal: tc.conceal}, nil
			})
			snapshot, err := r.Snapshot(context.Background(), Principal{ID: "p"}, auth)
			if err != nil {
				t.Fatal(err)
			}
			got := snapshot.Resolve(context.Background(), "known", "")
			if got.Outcome != tc.want || snapshot.Has("known") != tc.inSet || calls != tc.calls {
				t.Fatalf("outcome=%s inSet=%v calls=%d; want %s %v %d", got.Outcome, snapshot.Has("known"), calls, tc.want, tc.inSet, tc.calls)
			}
		})
	}
}

func TestSnapshotAndRegistryAreImmutable(t *testing.T) {
	declared := []Name{"b", "a"}
	installed := map[Name]Handler{"a": "A", "b": "B"}
	r, err := NewRegistry(declared, installed)
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.Snapshot(context.Background(), Principal{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	declared[0] = "changed"
	installed["a"] = "changed"
	first := s.Names()
	first[0] = "changed"
	if got := s.Names(); !reflect.DeepEqual(got, []Name{"a", "b"}) {
		t.Fatalf("snapshot changed: %v", got)
	}
	if got := s.Resolve(context.Background(), "a", ""); got.Handler != "A" {
		t.Fatalf("handler changed: %#v", got.Handler)
	}
}

func TestUnknownAndUninstalledNeverAuthorize(t *testing.T) {
	r, _ := NewRegistry([]Name{"installed", "missing"}, map[Name]Handler{"installed": true})
	calls := 0
	auth := AuthorizeFunc(func(context.Context, Request) (Decision, error) {
		calls++
		return Decision{Allowed: true}, nil
	})
	s, _ := r.Snapshot(context.Background(), Principal{}, auth)
	calls = 0
	if got := s.Resolve(context.Background(), "typo", "job").Outcome; got != Unknown {
		t.Fatalf("unknown = %s", got)
	}
	if got := s.Resolve(context.Background(), "missing", "job").Outcome; got != Uninstalled {
		t.Fatalf("uninstalled = %s", got)
	}
	if calls != 0 {
		t.Fatalf("authorization called %d times", calls)
	}
}

func TestResourceAuthorizationIsRecheckedAndConcealed(t *testing.T) {
	r, _ := NewRegistry([]Name{"job.inspect"}, map[Name]Handler{"job.inspect": "inspect"})
	var requests []Request
	auth := AuthorizeFunc(func(_ context.Context, req Request) (Decision, error) {
		requests = append(requests, req)
		if req.Resource == "secret" {
			return Decision{Conceal: true}, nil
		}
		if req.Resource == "denied" {
			return Decision{}, nil
		}
		return Decision{Allowed: true}, nil
	})
	principal := Principal{ID: "p", Attributes: map[string]string{"role": "reader"}}
	s, err := r.Snapshot(context.Background(), principal, auth)
	if err != nil {
		t.Fatal(err)
	}
	principal.Attributes["role"] = "changed"
	if got := s.Resolve(context.Background(), "job.inspect", "secret"); got.Outcome != Concealed {
		t.Fatalf("secret = %s", got.Outcome)
	}
	if got := s.Resolve(context.Background(), "job.inspect", "denied"); got.Outcome != Denied {
		t.Fatalf("denied = %s", got.Outcome)
	}
	if got := s.Resolve(context.Background(), "job.inspect", "public"); !got.Available() {
		t.Fatalf("public = %s", got.Outcome)
	}
	if len(requests) != 4 || requests[0].Resource != "" || requests[1].Resource != "secret" {
		t.Fatalf("requests = %#v", requests)
	}
	if requests[0].Principal.Attributes["role"] != "reader" {
		t.Fatalf("snapshot authorization observed caller mutation: %#v", requests[0].Principal)
	}
}

func TestAuthorizationErrorsDoNotProducePartialSnapshotOrAvailability(t *testing.T) {
	boom := errors.New("authorizer unavailable")
	r, _ := NewRegistry([]Name{"a"}, map[Name]Handler{"a": true})
	if _, err := r.Snapshot(context.Background(), Principal{}, AuthorizeFunc(func(context.Context, Request) (Decision, error) {
		return Decision{}, boom
	})); !errors.Is(err, boom) {
		t.Fatalf("snapshot error = %v", err)
	}
	s, _ := r.Snapshot(context.Background(), Principal{}, AuthorizeFunc(func(_ context.Context, req Request) (Decision, error) {
		if req.Resource != "" {
			return Decision{}, boom
		}
		return Decision{Allowed: true}, nil
	}))
	got := s.Resolve(context.Background(), "a", "resource")
	if got.Outcome != Denied || !errors.Is(got.Cause, boom) {
		t.Fatalf("resolution = %#v", got)
	}
}

func TestRegistryRejectsInvalidDeclarations(t *testing.T) {
	for _, tc := range []struct {
		declared  []Name
		installed map[Name]Handler
	}{
		{[]Name{""}, nil},
		{[]Name{"a", "a"}, nil},
	} {
		if _, err := NewRegistry(tc.declared, tc.installed); !errors.Is(err, ErrInvalidDeclaration) {
			t.Fatalf("NewRegistry(%v, %v) error = %v", tc.declared, tc.installed, err)
		}
	}
}
