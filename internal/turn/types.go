// Package turn defines the provider-neutral model turn contract.
package turn

import "encoding/json"

const Schema = "arxi.turn/v1"

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type BlockType string

const (
	BlockText       BlockType = "text"
	BlockImage      BlockType = "image"
	BlockDocument   BlockType = "document"
	BlockToolCall   BlockType = "tool_call"
	BlockToolResult BlockType = "tool_result"
)

type MediaSource struct {
	MediaType string `json:"media_type"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type ContentBlock struct {
	Type       BlockType    `json:"type"`
	Text       string       `json:"text,omitempty"`
	Source     *MediaSource `json:"source,omitempty"`
	ToolCall   *ToolCall    `json:"tool_call,omitempty"`
	ToolResult *ToolResult  `json:"tool_result,omitempty"`
}

type Message struct {
	Role    Role           `json:"role"`
	Content []ContentBlock `json:"content"`
}

type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type ToolCall struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	Arguments      json.RawMessage `json:"arguments"`
	ArgumentDigest string          `json:"argument_digest"`
}

type ToolResult struct {
	CallID  string         `json:"call_id"`
	Content []ContentBlock `json:"content"`
	IsError bool           `json:"is_error,omitempty"`
}

type Request struct {
	Schema      string           `json:"schema"`
	Provider    string           `json:"provider"`
	Protocol    string           `json:"protocol"`
	BaseURL     string           `json:"base_url"`
	APIKeyEnv   string           `json:"api_key_env,omitempty"`
	Model       string           `json:"model"`
	Messages    []Message        `json:"messages"`
	Tools       []ToolDefinition `json:"tools,omitempty"`
	MaxTokens   int              `json:"max_tokens"`
	Temperature *float64         `json:"temperature,omitempty"`
	Stream      bool             `json:"stream,omitempty"`
}

type FinishReason string

const (
	FinishStop      FinishReason = "stop"
	FinishLength    FinishReason = "length"
	FinishToolCalls FinishReason = "tool_calls"
	FinishRefusal   FinishReason = "refusal"
	FinishCanceled  FinishReason = "canceled"
	FinishError     FinishReason = "error"
)

type Usage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

type Refusal struct {
	Code      string `json:"code,omitempty"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable,omitempty"`
}

type Response struct {
	Schema       string         `json:"schema"`
	ID           string         `json:"id,omitempty"`
	Model        string         `json:"model,omitempty"`
	Content      []ContentBlock `json:"content,omitempty"`
	FinishReason FinishReason   `json:"finish_reason"`
	Usage        Usage          `json:"usage"`
	Refusal      *Refusal       `json:"refusal,omitempty"`
}

type StreamEventType string

const (
	StreamContentDelta StreamEventType = "content_delta"
	StreamUsage        StreamEventType = "usage"
	StreamCompleted    StreamEventType = "completed"
	StreamCanceled     StreamEventType = "canceled"
)

type StreamEvent struct {
	Type     StreamEventType `json:"type"`
	Index    int             `json:"index,omitempty"`
	Text     string          `json:"text,omitempty"`
	Usage    *Usage          `json:"usage,omitempty"`
	Response *Response       `json:"response,omitempty"`
}
