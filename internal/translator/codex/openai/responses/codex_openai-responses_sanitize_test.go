package responses

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestReasoningEffortFieldDeletion(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"reasoning_effort": "high",
		"input": [{"role": "user", "content": "hello"}]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	if gjson.Get(outputStr, "reasoning_effort").Exists() {
		t.Errorf("reasoning_effort field should be deleted, but it was found with value: %s", gjson.Get(outputStr, "reasoning_effort").Raw)
	}
}

func TestInputItemStatusStripped(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [
			{"type": "message", "role": "user", "status": "completed", "content": [{"type": "input_text", "text": "hi"}]},
			{"type": "reasoning", "id": "rs_1"},
			{"type": "function_call", "status": "completed", "call_id": "c1", "name": "f", "arguments": "{}"}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	for i, path := range []string{"input.0.status", "input.1.status", "input.2.status"} {
		if gjson.Get(outputStr, path).Exists() {
			t.Errorf("%s should be deleted, but it was found with value: %s", path, gjson.Get(outputStr, path).Raw)
		}
		_ = i
	}
	// Non-status item fields must survive.
	if got := gjson.Get(outputStr, "input.0.content.0.text").String(); got != "hi" {
		t.Errorf("input.0 content should be preserved, got %q", got)
	}
	if got := gjson.Get(outputStr, "input.2.call_id").String(); got != "c1" {
		t.Errorf("input.2 call_id should be preserved, got %q", got)
	}
}

func TestInputItemStatusStrippedStringInput(t *testing.T) {
	// String input is normalized into an array before sanitizing; stripping must
	// not crash or corrupt the payload either way.
	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", []byte(`{"model":"gpt-5.2","input":"hello"}`), false)
	if got := gjson.Get(string(output), "input.0.content.0.text").String(); got != "hello" {
		t.Errorf("string input should be normalized to array, got input.0.content.0.text=%q", got)
	}
}
