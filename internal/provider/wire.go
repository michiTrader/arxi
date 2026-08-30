// Package provider is the live executor: the thing that actually calls a model.
//
// It exists as a package of its own rather than inside internal/exec because an
// architecture rule holds internal/exec to a single dependency, the kernel
// (TestExecutorDependsOnlyOnTheKernel). That rule is right and this package is
// what it asks for: exec DECLARES the Executor interface, and the concrete
// implementation lives out here where it may import internal/model to resolve a
// ref and net/http to make the call. Go interfaces are satisfied structurally,
// so nothing has to be registered anywhere -- cmd/iash hands one of these to
// exec.Runner and the runner never learns which kind it got.
//
// The whole package speaks OpenAI's chat-completions shape, because that is what
// `provider add` promises: the surface calls --base-url "an OpenAI-compatible
// endpoint". One wire format for every provider is what makes a local llama and
// a hosted model the same code path, and it means the case that can be tested
// with no credential and no bill is the same case that runs in production.
package provider

import (
	"encoding/json"
	"fmt"
)

// chatRequest is the request body.
//
// Only the fields this executor actually sets are present. A struct mirroring
// the whole API would invite setting fields nobody tested, and every one of them
// is a way to change what a turn costs.
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`

	// MaxTokens bounds the reply. It is always sent, never left to the
	// provider's default: an unbounded reply is an unbounded bill, and the
	// budget is only charged AFTER the response arrives, so a runaway
	// generation cannot be stopped by the ceiling that exists to stop it.
	MaxTokens int `json:"max_tokens,omitempty"`

	// Temperature is a pointer so that "unset" and "0" are different.
	//
	// Zero is the value a reproducibility-minded caller most wants to send, and
	// a plain float64 with omitempty would silently drop exactly that value
	// while appearing to honour it.
	Temperature *float64 `json:"temperature,omitempty"`

	// Stream stays false. Streaming would deliver the reply in fragments and
	// the usage block last, which means the cost of a turn would be unknown
	// until the end of it -- and a turn whose cost is unknown cannot be charged
	// to a budget as it happens. The run loop has no use for partial text.
	Stream bool `json:"stream"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatResponse is the part of the response this executor reads.
//
// Unknown fields are IGNORED here, which is the opposite of the rule the
// provider store follows, and the difference is deliberate. A provider file is
// written by us and a surprise field means corruption or a hand-edit. This
// document is written by somebody else's server, which adds fields whenever it
// likes; refusing them would mean a routine upstream release breaks every run
// with a parse error, and the failure would arrive as "unknown field" in the
// middle of a turn somebody was paying for.
type chatResponse struct {
	ID      string       `json:"id"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   usage        `json:"usage"`

	// Error carries a provider-side refusal. Some OpenAI-compatible servers
	// answer 200 with an error object rather than a non-2xx status, so the
	// status line alone cannot be trusted to mean success. Missing this makes a
	// refusal look like an empty but valid reply, and the run would record a
	// turn that never happened.
	Error *wireError `json:"error"`
}

type chatChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// usage is the token count the bill is computed from.
//
// This is the most important struct in the package: everything the budget knows
// about what a run cost comes through these two integers.
type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type wireError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

func (e *wireError) String() string {
	if e == nil {
		return ""
	}
	if e.Type != "" {
		return fmt.Sprintf("%s: %s", e.Type, e.Message)
	}
	return e.Message
}

// text returns the reply, and says whether there was one.
//
// A response with no choices is not an empty answer, it is an absent one, and
// the two must not collapse: an absent answer means the turn produced nothing
// and the log should say so, while an empty string is a model that chose to say
// nothing. Only the first choice is read because only one is ever requested.
func (r *chatResponse) text() (string, bool) {
	if len(r.Choices) == 0 {
		return "", false
	}
	return r.Choices[0].Message.Content, true
}

// finishReason reports why generation stopped, or "" when the provider did not
// say.
//
// It travels into the log because "length" is not a success: the reply was cut
// off at max_tokens, and a turn whose answer was truncated looks identical to a
// complete one unless this is recorded.
func (r *chatResponse) finishReason() string {
	if len(r.Choices) == 0 {
		return ""
	}
	return r.Choices[0].FinishReason
}

// decodeResponse parses a response body.
//
// The body is included in the error on failure, truncated. Without it a
// malformed reply produces "invalid character '<' looking for beginning of
// value", which is the signature of an HTML error page from a proxy and is
// completely unrecognisable as such to the person reading the log.
func decodeResponse(body []byte) (*chatResponse, error) {
	var r chatResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("the provider's reply is not JSON (%w): %s",
			err, truncate(string(body), 256))
	}
	return &r, nil
}

// truncate bounds a string for an error message.
//
// Provider error bodies can be enormous, and an unbounded one ends up in the
// run log, in a returned error and on the terminal. It counts BYTES and cuts on
// a rune boundary, so a truncated multi-byte character cannot corrupt the JSON
// the log is written as.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + "... (truncated)"
}

// utf8Start reports whether b begins a UTF-8 sequence, i.e. is not a
// continuation byte.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
