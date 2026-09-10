package provider

import "encoding/json"

const anthropicVersion = "2023-06-01"

type anthropicRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	Tools       []anthropicTool    `json:"tools,omitempty"`
	Temperature *float64           `json:"temperature,omitempty"`
	Stream      bool               `json:"stream"`
}

type anthropicMessage struct {
	Role    string                  `json:"role"`
	Content []anthropicContentBlock `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   any             `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Source    any             `json:"source,omitempty"`
}

type anthropicResponse struct {
	ID          string                  `json:"id"`
	Type        string                  `json:"type"`
	Role        string                  `json:"role"`
	Model       string                  `json:"model"`
	Content     []anthropicContentBlock `json:"content"`
	StopReason  string                  `json:"stop_reason"`
	StopDetails *anthropicStopDetails   `json:"stop_details"`
	Usage       anthropicUsage          `json:"usage"`
	Error       *wireError              `json:"error"`
}

type anthropicStopDetails struct {
	Category    string `json:"category"`
	Explanation string `json:"explanation"`
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}
