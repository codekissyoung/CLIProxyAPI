package helps

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/tidwall/gjson"
)

// CodexRequestProfile summarizes the size and composition of an outbound Codex
// (openai-response) request body. It is a debugging aid for diagnosing
// context_too_large rejections: it shows whether the bloat comes from
// accumulated history, tool outputs (function_call_output), or a single
// oversized item.
type CodexRequestProfile struct {
	TotalBytes      int            // size of the whole request body
	InputBytes      int            // size of the "input" array alone
	InputItems      int            // number of items in "input"
	TypeCounts      map[string]int // count of items per "type"
	TypeBytes       map[string]int // bytes per "type"
	ToolOutputBytes int            // bytes of function_call_output items (usual culprit)
	LargestType     string         // type of the single largest input item
	LargestBytes    int            // bytes of the single largest input item
}

// ProfileCodexRequest inspects an openai-response request body and returns its
// composition. Safe to call on any payload; missing fields yield zero counts.
func ProfileCodexRequest(body []byte) CodexRequestProfile {
	p := CodexRequestProfile{
		TotalBytes: len(body),
		TypeCounts: map[string]int{},
		TypeBytes:  map[string]int{},
	}
	input := gjson.GetBytes(body, "input")
	if !input.Exists() || !input.IsArray() {
		return p
	}
	p.InputBytes = len(input.Raw)
	input.ForEach(func(_, item gjson.Result) bool {
		p.InputItems++
		itemType := item.Get("type").String()
		if itemType == "" {
			// Inline message form has no "type" but carries a role.
			if item.Get("role").Exists() {
				itemType = "message"
			} else {
				itemType = "unknown"
			}
		}
		n := len(item.Raw)
		p.TypeCounts[itemType]++
		p.TypeBytes[itemType] += n
		if itemType == "function_call_output" {
			p.ToolOutputBytes += n
		}
		if n > p.LargestBytes {
			p.LargestBytes = n
			p.LargestType = itemType
		}
		return true
	})
	return p
}

// LogCodexRequestProfile emits one debug line describing the request composition.
// Tail it alongside the session-affinity and upstream-error lines to see which
// request was oversized and what made it so. label distinguishes call sites
// (e.g. "codex-http", "codex-ws").
func LogCodexRequestProfile(ctx context.Context, label, model string, body []byte) {
	p := ProfileCodexRequest(body)
	entry := LogWithRequestID(ctx)
	entry.Debugf(
		"codex request profile | site=%s model=%s total_bytes=%d input_bytes=%d input_items=%d tool_output_bytes=%d (%.0f%%) largest=%s/%dB types=%s",
		label, model, p.TotalBytes, p.InputBytes, p.InputItems,
		p.ToolOutputBytes, percent(p.ToolOutputBytes, p.TotalBytes),
		p.LargestType, p.LargestBytes, formatTypeBreakdown(p.TypeCounts, p.TypeBytes),
	)
}

func percent(part, whole int) float64 {
	if whole <= 0 {
		return 0
	}
	return float64(part) * 100 / float64(whole)
}

// formatTypeBreakdown renders "type:count/bytes" entries, largest-bytes first,
// e.g. "function_call_output:4/812340,message:9/12044,reasoning:3/1502".
func formatTypeBreakdown(counts, bytesByType map[string]int) string {
	if len(counts) == 0 {
		return "-"
	}
	types := make([]string, 0, len(counts))
	for t := range counts {
		types = append(types, t)
	}
	sort.Slice(types, func(i, j int) bool {
		return bytesByType[types[i]] > bytesByType[types[j]]
	})
	parts := make([]string, 0, len(types))
	for _, t := range types {
		parts = append(parts, fmt.Sprintf("%s:%d/%d", t, counts[t], bytesByType[t]))
	}
	return strings.Join(parts, ",")
}
