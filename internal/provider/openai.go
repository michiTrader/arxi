package provider

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/michiTrader/arxi/internal/turn"
)

// openAIRequest translates the provider-neutral contract to Chat Completions.
func openAIRequest(req turn.Request) (chatRequest, error) {
	if req.Schema != turn.Schema {
		return chatRequest{}, fmt.Errorf("turn schema %q, want %q", req.Schema, turn.Schema)
	}
	if req.Stream {
		return chatRequest{}, fmt.Errorf("streaming is not implemented for OpenAI Chat Completions")
	}
	out := chatRequest{
		Model: req.Model, MaxTokens: req.MaxTokens,
		Temperature: req.Temperature, Stream: req.Stream,
	}
	for i, msg := range req.Messages {
		wire, err := openAIMessage(msg)
		if err != nil {
			return chatRequest{}, fmt.Errorf("message %d: %w", i, err)
		}
		out.Messages = append(out.Messages, wire...)
	}
	for _, tool := range req.Tools {
		out.Tools = append(out.Tools, chatTool{Type: "function", Function: chatToolFunction{
			Name: tool.Name, Description: tool.Description, Parameters: tool.InputSchema,
		}})
	}
	return out, nil
}

func openAIMessage(msg turn.Message) ([]chatMessage, error) {
	switch msg.Role {
	case turn.RoleSystem, turn.RoleUser:
		text, err := textContent(msg.Content)
		if err != nil {
			return nil, err
		}
		return []chatMessage{{Role: string(msg.Role), Content: text}}, nil
	case turn.RoleAssistant:
		var text strings.Builder
		wire := chatMessage{Role: "assistant"}
		for _, block := range msg.Content {
			switch block.Type {
			case turn.BlockText:
				text.WriteString(block.Text)
			case turn.BlockToolCall:
				if block.ToolCall == nil {
					return nil, fmt.Errorf("assistant tool_call block has no call")
				}
				if err := turn.ValidateToolCall(*block.ToolCall); err != nil {
					return nil, err
				}
				wire.ToolCalls = append(wire.ToolCalls, chatToolCall{
					ID: block.ToolCall.ID, Type: "function", Function: chatToolFunction{
						Name: block.ToolCall.Name, Arguments: string(block.ToolCall.Arguments),
					},
				})
			default:
				return nil, fmt.Errorf("assistant content type %q is not supported by OpenAI Chat Completions", block.Type)
			}
		}
		if text.Len() > 0 {
			wire.Content = text.String()
		} else if len(wire.ToolCalls) > 0 {
			wire.Content = nil
		} else {
			wire.Content = ""
		}
		return []chatMessage{wire}, nil
	case turn.RoleTool:
		out := make([]chatMessage, 0, len(msg.Content))
		for _, block := range msg.Content {
			if block.Type != turn.BlockToolResult || block.ToolResult == nil {
				return nil, fmt.Errorf("tool message contains non-result block %q", block.Type)
			}
			text, err := textContent(block.ToolResult.Content)
			if err != nil {
				return nil, fmt.Errorf("tool result %s: %w", block.ToolResult.CallID, err)
			}
			out = append(out, chatMessage{Role: "tool", ToolCallID: block.ToolResult.CallID, Content: text})
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported turn role %q", msg.Role)
	}
}

func textContent(blocks []turn.ContentBlock) (string, error) {
	var text strings.Builder
	for _, block := range blocks {
		if block.Type != turn.BlockText {
			return "", fmt.Errorf("content type %q is not supported by OpenAI Chat Completions", block.Type)
		}
		text.WriteString(block.Text)
	}
	return text.String(), nil
}

func canonicalOpenAIResponse(resp *chatResponse) (turn.Response, error) {
	out := turn.Response{Schema: turn.Schema}
	if resp == nil {
		return out, fmt.Errorf("provider returned no response")
	}
	out.ID, out.Model = resp.ID, resp.Model
	out.Usage = turn.Usage{
		InputTokens: resp.Usage.PromptTokens, OutputTokens: resp.Usage.CompletionTokens,
		CacheReadTokens: resp.Usage.PromptTokensDetails.CachedTokens,
	}
	if resp.Error != nil {
		out.FinishReason = turn.FinishRefusal
		out.Refusal = &turn.Refusal{Message: resp.Error.String()}
		return out, nil
	}
	if len(resp.Choices) == 0 {
		return out, fmt.Errorf("provider response %q has no choices", resp.ID)
	}
	choice := resp.Choices[0]
	if choice.Message.Content != nil {
		out.Content = append(out.Content, turn.ContentBlock{Type: turn.BlockText, Text: *choice.Message.Content})
	}
	for _, wire := range choice.Message.ToolCalls {
		if wire.Type != "" && wire.Type != "function" {
			return out, fmt.Errorf("provider tool call %q has unsupported type %q", wire.ID, wire.Type)
		}
		call, err := turn.NewToolCall(wire.ID, wire.Function.Name, []byte(wire.Function.Arguments))
		if err != nil {
			return out, fmt.Errorf("provider tool call %q: %w", wire.ID, err)
		}
		out.Content = append(out.Content, turn.ContentBlock{Type: turn.BlockToolCall, ToolCall: &call})
	}
	if presentJSON(choice.Message.FunctionCall) {
		return out, fmt.Errorf("legacy function_call has no provider call ID and cannot be resumed safely")
	}
	out.FinishReason = openAIFinishReason(choice.FinishReason, len(choice.Message.ToolCalls) > 0)
	if out.FinishReason == turn.FinishRefusal {
		out.Refusal = &turn.Refusal{Code: "content_filter", Message: "the provider filtered the response"}
	}
	if out.FinishReason == turn.FinishError {
		out.Refusal = &turn.Refusal{Code: "unsupported_finish_reason", Message: fmt.Sprintf("the provider returned unsupported finish reason %q", choice.FinishReason)}
	}
	return out, nil
}

func openAIFinishReason(reason string, hasTools bool) turn.FinishReason {
	if hasTools || reason == "tool_calls" || reason == "function_call" {
		return turn.FinishToolCalls
	}
	switch reason {
	case "stop":
		return turn.FinishStop
	case "length":
		return turn.FinishLength
	case "content_filter":
		return turn.FinishRefusal
	default:
		return turn.FinishError
	}
}

func decodeArguments(raw json.RawMessage) (map[string]any, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var args map[string]any
	if err := dec.Decode(&args); err != nil {
		return nil, fmt.Errorf("decode canonical tool arguments: %w", err)
	}
	return args, nil
}
