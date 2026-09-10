package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func (c *Client) CompleteAnthropic(ctx context.Context, req anthropicRequest) (*anthropicResponse, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("an Anthropic message needs a model; the caller resolved nothing")
	}
	key, err := c.credential()
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode the request for %s: %w", req.Model, err)
	}
	url := strings.TrimSuffix(c.BaseURL, "/") + "/messages"
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build the request for %s: %w", req.Model, err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("anthropic-version", anthropicVersion)
	if key != "" {
		hreq.Header.Set("x-api-key", key)
	}
	client := c.HTTP
	if client == nil {
		client = DefaultHTTP()
	}
	resp, err := client.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("call %s for model %s: %w", url, req.Model, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read the reply from %s for model %s: %w", url, req.Model, err)
	}
	parsed, decErr := decodeAnthropicResponse(raw)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := &APIError{Status: resp.StatusCode, Message: anthropicErrorMessage(raw, parsed), Model: req.Model}
		if decErr == nil {
			return parsed, apiErr
		}
		return nil, apiErr
	}
	if decErr != nil {
		return nil, decErr
	}
	if parsed.Error != nil {
		return parsed, &APIError{Status: resp.StatusCode, Message: parsed.Error.String(), Model: req.Model}
	}
	return parsed, nil
}
