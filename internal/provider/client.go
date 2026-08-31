package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Client calls one OpenAI-compatible endpoint.
//
// It holds the endpoint and the NAME of the environment variable carrying the
// credential, never the credential itself. That is the same rule internal/model
// enforces on disk, kept here for the same reason: a struct holding a key gets
// logged eventually, by a %+v somebody adds to a debug line in a hurry.
type Client struct {
	BaseURL   string
	APIKeyEnv string

	// HTTP is injectable so the tests can point at an httptest server. Nil
	// means DefaultClient() -- a client with a timeout, never http.DefaultClient,
	// whose zero timeout means a hung provider hangs the run forever.
	HTTP *http.Client

	// Getenv is injectable for the same reason. Tests must be able to supply a
	// credential without putting one in the process environment, where it would
	// leak into every subprocess the test suite starts.
	Getenv func(string) string
}

// DefaultTimeout bounds one model call.
//
// Chosen long enough that a large reasoning turn is not cut off, and short
// enough that a provider which has stopped answering does not hold the run open
// indefinitely. It exists at all because http.Client's zero value has NO
// timeout: a connection that is accepted and then ignored would block the run
// loop until the operator killed it, and the run would show a member thinking
// forever with no error to explain it.
const DefaultTimeout = 5 * time.Minute

// DefaultHTTP returns the client used when none is supplied.
func DefaultHTTP() *http.Client {
	return &http.Client{Timeout: DefaultTimeout}
}

// ErrNoCredential means the variable naming the key is unset or empty.
//
// Distinct so the caller can tell "you have not set this up" from "the provider
// rejected your key". The fixes are completely different -- export a variable
// versus get a new key -- and a single "unauthorized" would send the user
// looking in the wrong place.
type ErrNoCredential struct {
	Env      string
	Provider string
}

func (e *ErrNoCredential) Error() string {
	return fmt.Sprintf("provider %s reads its key from $%s, which is empty.\n"+
		"  fix: export %s=... in the shell that runs arxi.\n"+
		"  note: arxi never stores the key; it stores only the NAME of this variable, "+
		"which is why it has to be present in the environment at run time",
		e.Provider, e.Env, e.Env)
}

// APIError is a refusal from the provider: it answered, and the answer was no.
//
// This is a DOMAIN failure in the executor's vocabulary. It happened, the
// provider is reachable, and the fact belongs in the log. Status is kept because
// the remedy depends on it and a message alone hides that: 401 is a bad key, 429
// is rate limiting that will pass, 400 is a request this build should not have
// sent, and treating all three as "the call failed" means retrying the two that
// cannot succeed.
type APIError struct {
	Status  int
	Message string
	Model   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("provider refused the call for model %s: HTTP %d: %s",
		e.Model, e.Status, e.Message)
}

// Retryable reports whether trying the same call again could plausibly work.
//
// 429 and 5xx are the provider saying "not now"; 4xx otherwise is the provider
// saying "not this". Retrying the latter spends money on a request that is
// already known to be wrong -- and 401 in a loop is how an account gets locked.
func (e *APIError) Retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

// Complete performs one chat completion.
//
// The two return paths mirror the contract in exec.Executor, and keeping them
// apart is the entire reason this function is shaped the way it is:
//
//   - *APIError means the provider answered and refused. A DOMAIN fact.
//   - anything else means the call could not be turned into a fact at all: no
//     connection, a timeout, a body that is not JSON. TRANSPORT.
//
// Usage comes back even on some refusals, because a provider can charge for a
// prompt it then declines to answer, and a cost that is real but unreported is
// the one kind of budget error the user cannot detect.
func (c *Client) Complete(ctx context.Context, req chatRequest) (*chatResponse, error) {
	if req.Model == "" {
		return nil, errors.New("a completion needs a model; the caller resolved nothing")
	}

	key, err := c.credential()
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode the request for %s: %w", req.Model, err)
	}

	url := strings.TrimSuffix(c.BaseURL, "/") + "/chat/completions"
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build the request for %s: %w", req.Model, err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	if key != "" {
		hreq.Header.Set("Authorization", "Bearer "+key)
	}

	client := c.HTTP
	if client == nil {
		client = DefaultHTTP()
	}

	resp, err := client.Do(hreq)
	if err != nil {
		// Transport. The URL is named but the header is not: an error from Do
		// can be logged anywhere, and Go's own error strings already include the
		// URL, so adding the request would be the moment a key reached a log.
		return nil, fmt.Errorf("call %s for model %s: %w", url, req.Model, err)
	}
	defer resp.Body.Close()

	// The body is read with a ceiling. A provider (or something impersonating
	// one) that streams an endless body would otherwise consume memory until the
	// process died, and it would do so inside a run holding a lock on a run
	// directory.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read the reply from %s for model %s: %w", url, req.Model, err)
	}

	// Decoded BEFORE the status is judged, because the body is where the reason
	// lives. A 400 whose message says "max_tokens too large" is actionable; a
	// bare "HTTP 400" is not.
	parsed, decErr := decodeResponse(raw)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := strings.TrimSpace(truncate(string(raw), 512))
		if decErr == nil && parsed.Error != nil {
			msg = parsed.Error.String()
		}
		apiErr := &APIError{Status: resp.StatusCode, Message: msg, Model: req.Model}
		if decErr == nil {
			// Returned WITH the parsed response so the caller can still bill any
			// usage the provider reported alongside its refusal.
			return parsed, apiErr
		}
		return nil, apiErr
	}

	if decErr != nil {
		return nil, decErr
	}

	// A 200 carrying an error object. Some OpenAI-compatible servers answer this
	// way, and trusting the status line alone would record a turn that never
	// happened as a turn that produced an empty reply.
	if parsed.Error != nil {
		return parsed, &APIError{
			Status:  resp.StatusCode,
			Message: parsed.Error.String(),
			Model:   req.Model,
		}
	}

	return parsed, nil
}

// maxResponseBytes bounds one reply. Generous for a completion, far below what
// would threaten the process.
const maxResponseBytes = 8 << 20 // 8 MiB

// credential reads the key out of the environment.
//
// An empty APIKeyEnv is not an error: a model on loopback needs no credential,
// and that is the one case testable with no key and no bill. Requiring one would
// make the only free path the only impossible one.
func (c *Client) credential() (string, error) {
	if c.APIKeyEnv == "" {
		return "", nil
	}
	getenv := c.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	key := strings.TrimSpace(getenv(c.APIKeyEnv))
	if key == "" {
		return "", &ErrNoCredential{Env: c.APIKeyEnv, Provider: hostOf(c.BaseURL)}
	}
	return key, nil
}

// hostOf names the endpoint for an error message without dragging in net/url
// for one field.
func hostOf(base string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return base
	}
	return s
}
