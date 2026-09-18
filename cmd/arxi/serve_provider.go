package main

import (
	"context"
	"fmt"
	"strings"

	hostv1 "github.com/michiTrader/arxi/host/v1"
	"github.com/michiTrader/arxi/internal/model"
	"github.com/michiTrader/arxi/internal/modelstore"
	"github.com/michiTrader/arxi/internal/provider"
	"github.com/michiTrader/arxi/internal/turn"
)

// serveTextProvider is the production TextProvider behind `arxi serve`: it
// resolves the requested model through the operator's modelstore and completes
// the call through the same provider transports the CLI uses. This is what
// makes a protocol-submitted job bill the same providers a CLI run bills, and
// why it lives in the composition root rather than in host/v1, whose
// independence from internal/ packages is guarded by architecture tests.
//
// The host's text contract is one completed response per request, so the
// adapter performs a single non-streaming canonical turn. Errors are returned
// unwrapped: the public port promises that a Go error means no trustworthy
// completion can be asserted, and the adapter adds nothing it can prove.
type serveTextProvider struct {
	store    *modelstore.Store
	executor *provider.Executor
}

// newServeTextProvider opens the modelstore. An empty provider directory is
// not an error here: submit then fails with the resolver's remedy ("no
// providers are registered..."), which tells the operator what to do instead
// of refusing at startup when serving a read-only inspection surface is a
// legitimate use.
func newServeTextProvider() (*serveTextProvider, error) {
	store, err := modelstore.Open(providerDir)
	if err != nil {
		return nil, fmt.Errorf("open provider store %s: %w", providerDir, err)
	}
	return &serveTextProvider{store: store, executor: &provider.Executor{}}, nil
}

// serveMessages maps one public text request onto canonical messages.
//
// Split out of CompleteText so the mapping is assertable without a resolver, a
// provider store or a live endpoint. That is not a cosmetic refactor: while
// this logic lived inline, a mutation that folded req.Memory back into the
// system message passed the entire cmd suite, because no test could reach the
// assembly without standing up a real provider. A decision no test can reach
// is a decision nothing enforces.
//
// Memory becomes its own user-role message and is never merged into the system
// message (ADR-0020, carried to this adapter by ADR-0025). Appending it to
// req.System here would discard the separation the port exists to express, and
// on Anthropic the damage would be invisible: that adapter joins every system
// message into one string, so the collapse happens below the neutral layer
// where no assertion above it can see.
func serveMessages(req hostv1.TextRequest) []turn.Message {
	messages := []turn.Message{}
	if text := strings.TrimSpace(req.System); text != "" {
		messages = append(messages, turn.Message{Role: turn.RoleSystem,
			Content: []turn.ContentBlock{{Type: turn.BlockText, Text: text}}})
	}
	if memory := strings.TrimSpace(req.Memory); memory != "" {
		messages = append(messages, turn.Message{Role: turn.RoleUser,
			Content: []turn.ContentBlock{{Type: turn.BlockText, Text: "Memory:\n" + memory + "\n"}}})
	}
	return append(messages, turn.Message{Role: turn.RoleUser,
		Content: []turn.ContentBlock{{Type: turn.BlockText, Text: req.Prompt}}})
}

// CompleteText resolves the model and performs one canonical text turn. The
// request carries no provider route — that is the point of the port — so
// resolution is the adapter's job, and a model nobody registered is a client
// error the caller can act on, not a server fault.
func (p *serveTextProvider) CompleteText(ctx context.Context, req hostv1.TextRequest) (hostv1.TextResponse, error) {
	providers, err := p.store.List()
	if err != nil {
		return hostv1.TextResponse{}, fmt.Errorf("list providers: %w", err)
	}
	resolution, err := model.Resolve(providers, req.Model)
	if err != nil {
		return hostv1.TextResponse{}, err
	}
	messages := serveMessages(req)
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		// The provider default, not an invented one: passing zero would ask
		// the wire transport to omit the cap, and some providers then answer
		// with the longest response they allow.
		maxTokens = 4096
	}
	response, err := p.executor.CompleteTurn(ctx, turn.Request{
		Schema: turn.Schema, Provider: resolution.Provider, Protocol: resolution.Protocol,
		BaseURL: resolution.BaseURL, APIKeyEnv: resolution.APIKeyEnv, Model: resolution.Model,
		MaxTokens: maxTokens, Temperature: req.Temperature, Messages: messages,
	})
	if err != nil {
		return hostv1.TextResponse{}, err
	}
	if response.FinishReason == turn.FinishRefusal {
		if response.Refusal != nil {
			return hostv1.TextResponse{}, fmt.Errorf("model %s refused: %s", req.Model, response.Refusal.Message)
		}
		return hostv1.TextResponse{}, fmt.Errorf("model %s refused", req.Model)
	}
	return hostv1.TextResponse{Text: responseText(response)}, nil
}

// responseText extracts the text of a canonical response. A response with no
// text blocks is not an error: the turn completed, and the empty string is the
// honest answer, not a fabricated one.
func responseText(response turn.Response) string {
	text := ""
	for _, block := range response.Content {
		if block.Type == turn.BlockText && block.Text != "" {
			if text != "" {
				text += "\n"
			}
			text += block.Text
		}
	}
	return text
}

var _ hostv1.TextProvider = (*serveTextProvider)(nil)
