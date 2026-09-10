package turn

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
)

// CanonicalArguments normalizes one JSON object to a stable byte representation.
func CanonicalArguments(raw []byte) (json.RawMessage, string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, "", fmt.Errorf("decode tool arguments: %w", err)
	}
	if err := requireEOF(dec); err != nil {
		return nil, "", fmt.Errorf("decode tool arguments: %w", err)
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, "", fmt.Errorf("tool arguments must be a JSON object")
	}
	body, err := json.Marshal(value)
	if err != nil {
		return nil, "", fmt.Errorf("encode tool arguments: %w", err)
	}
	sum := sha256.Sum256(body)
	return body, hex.EncodeToString(sum[:]), nil
}

// NewToolCall binds a provider call ID and tool name to canonical arguments.
func NewToolCall(id, name string, raw []byte) (ToolCall, error) {
	if id == "" {
		return ToolCall{}, fmt.Errorf("tool call id is required")
	}
	if name == "" {
		return ToolCall{}, fmt.Errorf("tool name is required")
	}
	args, digest, err := CanonicalArguments(raw)
	if err != nil {
		return ToolCall{}, err
	}
	return ToolCall{ID: id, Name: name, Arguments: args, ArgumentDigest: digest}, nil
}

// ValidateToolCall detects mutation of a call after its identity was recorded.
func ValidateToolCall(call ToolCall) error {
	if call.ID == "" || call.Name == "" {
		return fmt.Errorf("tool call id and name are required")
	}
	args, digest, err := CanonicalArguments(call.Arguments)
	if err != nil {
		return err
	}
	if !bytes.Equal(args, call.Arguments) {
		return fmt.Errorf("tool call %s arguments are not canonical", call.ID)
	}
	if digest != call.ArgumentDigest {
		return fmt.Errorf("tool call %s argument digest does not match its arguments", call.ID)
	}
	return nil
}

func requireEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("more than one JSON value")
		}
		return err
	}
	return nil
}
