package helps

import "testing"

func TestProfileCodexRequestCountsTypesAndToolOutput(t *testing.T) {
	body := []byte(`{
		"model": "gpt-5.5",
		"input": [
			{"type": "message", "role": "user", "content": "hi"},
			{"type": "function_call_output", "output": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
			{"type": "reasoning", "summary": "x"},
			{"role": "assistant", "content": "ok"}
		]
	}`)

	p := ProfileCodexRequest(body)

	if p.InputItems != 4 {
		t.Fatalf("InputItems = %d, want 4", p.InputItems)
	}
	if p.TypeCounts["function_call_output"] != 1 {
		t.Fatalf("function_call_output count = %d, want 1", p.TypeCounts["function_call_output"])
	}
	// The inline item with a role but no type is classified as "message".
	if p.TypeCounts["message"] != 2 {
		t.Fatalf("message count = %d, want 2", p.TypeCounts["message"])
	}
	if p.ToolOutputBytes <= 0 {
		t.Fatalf("ToolOutputBytes = %d, want > 0", p.ToolOutputBytes)
	}
	// The function_call_output item is the largest by bytes.
	if p.LargestType != "function_call_output" {
		t.Fatalf("LargestType = %q, want function_call_output", p.LargestType)
	}
	if p.TotalBytes != len(body) {
		t.Fatalf("TotalBytes = %d, want %d", p.TotalBytes, len(body))
	}
}

func TestProfileCodexRequestHandlesMissingInput(t *testing.T) {
	p := ProfileCodexRequest([]byte(`{"model":"gpt-5.5"}`))
	if p.InputItems != 0 || p.InputBytes != 0 {
		t.Fatalf("expected zero input stats, got items=%d bytes=%d", p.InputItems, p.InputBytes)
	}
	if p.TotalBytes == 0 {
		t.Fatalf("TotalBytes should reflect whole body")
	}
}
