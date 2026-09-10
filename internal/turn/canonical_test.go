package turn

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCanonicalArgumentsIgnoreObjectKeyOrder(t *testing.T) {
	left, leftDigest, err := CanonicalArguments([]byte(`{"path":"a","options":{"line":1,"all":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	right, rightDigest, err := CanonicalArguments([]byte(` { "options": { "all": true, "line": 1 }, "path": "a" } `))
	if err != nil {
		t.Fatal(err)
	}
	if string(left) != string(right) || leftDigest != rightDigest {
		t.Fatalf("equivalent arguments differ: %s/%s and %s/%s", left, leftDigest, right, rightDigest)
	}
}

func TestToolCallDigestDetectsMutation(t *testing.T) {
	call, err := NewToolCall("call-1", "read", []byte(`{"path":"a"}`))
	if err != nil {
		t.Fatal(err)
	}
	call.Arguments = json.RawMessage(`{"path":"b"}`)
	if err := ValidateToolCall(call); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("mutation error = %v", err)
	}
}

func TestCanonicalArgumentsRequireOneObject(t *testing.T) {
	for _, raw := range []string{`[]`, `null`, `{"a":1} {"b":2}`, `{"a":`} {
		if _, _, err := CanonicalArguments([]byte(raw)); err == nil {
			t.Errorf("CanonicalArguments(%q) succeeded", raw)
		}
	}
}

func TestResponseRoundTripPreservesCanonicalOutcome(t *testing.T) {
	call, err := NewToolCall("call-7", "read", []byte(`{"path":"README.md"}`))
	if err != nil {
		t.Fatal(err)
	}
	want := Response{
		Schema: Schema, ID: "response-1", Model: "model-1",
		Content:      []ContentBlock{{Type: BlockText, Text: "checking"}, {Type: BlockToolCall, ToolCall: &call}},
		FinishReason: FinishToolCalls,
		Usage:        Usage{InputTokens: 12, OutputTokens: 4, CacheReadTokens: 3, CacheWriteTokens: 2},
		Refusal:      &Refusal{Code: "policy", Message: "not allowed", Retryable: false},
	}
	body, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got Response
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.FinishReason != FinishToolCalls || got.Usage != want.Usage || got.Refusal == nil || got.Refusal.Message != "not allowed" {
		t.Fatalf("round trip lost outcome: %#v", got)
	}
	if len(got.Content) != 2 || got.Content[1].ToolCall == nil || got.Content[1].ToolCall.ID != "call-7" {
		t.Fatalf("round trip lost content: %#v", got.Content)
	}
}

func TestCancellationAndStreamingAreExplicit(t *testing.T) {
	response := Response{Schema: Schema, FinishReason: FinishCanceled}
	event := StreamEvent{Type: StreamCanceled, Response: &response}
	body, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"type":"canceled"`) || !strings.Contains(string(body), `"finish_reason":"canceled"`) {
		t.Fatalf("cancellation was not explicit: %s", body)
	}
}
