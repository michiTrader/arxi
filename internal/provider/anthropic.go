package provider

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/michiTrader/arxi/internal/turn"
)

func anthropicTurnRequest(req turn.Request) (anthropicRequest, error) {
	if req.Schema != turn.Schema {
		return anthropicRequest{}, fmt.Errorf("turn schema %q, want %q", req.Schema, turn.Schema)
	}
	if req.Stream {
		return anthropicRequest{}, fmt.Errorf("streaming is not implemented for Anthropic Messages")
	}
	out := anthropicRequest{
		Model: req.Model, MaxTokens: req.MaxTokens,
		Temperature: req.Temperature, Stream: req.Stream,
	}
	for i, msg := range req.Messages {
		if msg.Role == turn.RoleSystem {
			text, err := textContent(msg.Content)
			if err != nil {
				return anthropicRequest{}, fmt.Errorf("system message %d: %w", i, err)
			}
			if out.System != "" {
				out.System += "\n\n"
			}
			out.System += text
			continue
		}
		wire, err := anthropicTurnMessage(msg)
		if err != nil {
			return anthropicRequest{}, fmt.Errorf("message %d: %w", i, err)
		}
		out.Messages = append(out.Messages, wire)
	}
	for _, tool := range req.Tools {
		out.Tools = append(out.Tools, anthropicTool{
			Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema,
		})
	}
	return out, nil
}

func anthropicTurnMessage(msg turn.Message) (anthropicMessage, error) {
	wire := anthropicMessage{}
	switch msg.Role {
	case turn.RoleUser:
		wire.Role = "user"
	case turn.RoleAssistant:
		wire.Role = "assistant"
	case turn.RoleTool:
		wire.Role = "user"
	default:
		return wire, fmt.Errorf("unsupported turn role %q", msg.Role)
	}
	for _, block := range msg.Content {
		switch block.Type {
		case turn.BlockText:
			if msg.Role == turn.RoleTool {
				return wire, fmt.Errorf("tool message contains text outside a tool result")
			}
			wire.Content = append(wire.Content, anthropicContentBlock{Type: "text", Text: block.Text})
		case turn.BlockToolCall:
			if msg.Role != turn.RoleAssistant || block.ToolCall == nil {
				return wire, fmt.Errorf("tool_call block is only valid in assistant messages")
			}
			if err := turn.ValidateToolCall(*block.ToolCall); err != nil {
				return wire, err
			}
			wire.Content = append(wire.Content, anthropicContentBlock{
				Type: "tool_use", ID: block.ToolCall.ID, Name: block.ToolCall.Name,
				Input: block.ToolCall.Arguments,
			})
		case turn.BlockToolResult:
			if msg.Role != turn.RoleTool || block.ToolResult == nil {
				return wire, fmt.Errorf("tool_result block is only valid in tool messages")
			}
			text, err := textContent(block.ToolResult.Content)
			if err != nil {
				return wire, fmt.Errorf("tool result %s: %w", block.ToolResult.CallID, err)
			}
			wire.Content = append(wire.Content, anthropicContentBlock{
				Type: "tool_result", ToolUseID: block.ToolResult.CallID,
				Content: text, IsError: block.ToolResult.IsError,
			})
		default:
			return wire, fmt.Errorf("content type %q is not supported by Anthropic Messages", block.Type)
		}
	}
	return wire, nil
}

func canonicalAnthropicResponse(resp *anthropicResponse) (turn.Response, error) {
	out := turn.Response{Schema: turn.Schema}
	if resp == nil {
		return out, fmt.Errorf("provider returned no response")
	}
	out.ID, out.Model = resp.ID, resp.Model
	out.Usage = turn.Usage{
		InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens,
		CacheReadTokens:  resp.Usage.CacheReadInputTokens,
		CacheWriteTokens: resp.Usage.CacheCreationInputTokens,
	}
	if resp.Error != nil {
		out.FinishReason = turn.FinishRefusal
		out.Refusal = &turn.Refusal{Message: resp.Error.String()}
		return out, nil
	}
	for i, wire := range resp.Content {
		switch wire.Type {
		case "text":
			out.Content = append(out.Content, turn.ContentBlock{Type: turn.BlockText, Text: wire.Text})
		case "tool_use":
			call, err := turn.NewToolCall(wire.ID, wire.Name, wire.Input)
			if err != nil {
				return out, fmt.Errorf("provider tool use %d (%q): %w", i, wire.ID, err)
			}
			out.Content = append(out.Content, turn.ContentBlock{Type: turn.BlockToolCall, ToolCall: &call})
		default:
			return out, fmt.Errorf("provider content block %d has unsupported type %q", i, wire.Type)
		}
	}
	out.FinishReason = anthropicFinishReason(resp.StopReason)
	if out.FinishReason == turn.FinishRefusal {
		refusal := &turn.Refusal{Code: "refusal", Message: "the provider refused the request"}
		if resp.StopDetails != nil {
			refusal.Code = resp.StopDetails.Category
			refusal.Message = resp.StopDetails.Explanation
		}
		out.Refusal = refusal
	}
	if out.FinishReason == turn.FinishError {
		out.Refusal = &turn.Refusal{Code: "unsupported_finish_reason", Message: fmt.Sprintf("the provider returned unsupported stop reason %q", resp.StopReason)}
	}
	return out, nil
}

func anthropicFinishReason(reason string) turn.FinishReason {
	switch reason {
	case "end_turn", "stop_sequence", "pause_turn":
		return turn.FinishStop
	case "max_tokens", "model_context_window_exceeded":
		return turn.FinishLength
	case "tool_use":
		return turn.FinishToolCalls
	case "refusal":
		return turn.FinishRefusal
	default:
		return turn.FinishError
	}
}

func decodeAnthropicResponse(body []byte) (*anthropicResponse, error) {
	var out anthropicResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("the provider's reply is not JSON (%w): %s", err, truncate(string(body), 256))
	}
	return &out, nil
}

func anthropicErrorMessage(body []byte, parsed *anthropicResponse) string {
	if parsed != nil && parsed.Error != nil {
		return parsed.Error.String()
	}
	return strings.TrimSpace(truncate(string(body), 512))
}
