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
	ImageItems      int            // input items that carry base64 image data
	ImageBytes      int            // bytes of items that carry base64 image data
}

// base64ImageMarkers are signatures that identify an inlined base64 image:
// a data URL, or the magic prefixes of PNG (iVBORw0KGgo) and JPEG (/9j/).
// Detection is intentionally cheap (substring scan), since the goal is to flag
// "this request carries an image" for triage, not to fully parse it.
var base64ImageMarkers = []string{"data:image", "iVBORw0KGgo", "/9j/4AAQ", "iVBORw0KGg"}

// rawHasBase64Image reports whether the raw JSON of an input item contains an
// inlined base64 image.
func rawHasBase64Image(raw string) bool {
	for _, m := range base64ImageMarkers {
		if strings.Contains(raw, m) {
			return true
		}
	}
	return false
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
		if rawHasBase64Image(item.Raw) {
			p.ImageItems++
			p.ImageBytes += n
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
		"codex request profile | site=%s model=%s total_bytes=%d input_bytes=%d input_items=%d tool_output_bytes=%d (%.0f%%) image_items=%d image_bytes=%d (%.0f%%) largest=%s/%dB types=%s",
		label, model, p.TotalBytes, p.InputBytes, p.InputItems,
		p.ToolOutputBytes, percent(p.ToolOutputBytes, p.TotalBytes),
		p.ImageItems, p.ImageBytes, percent(p.ImageBytes, p.TotalBytes),
		p.LargestType, p.LargestBytes, formatTypeBreakdown(p.TypeCounts, p.TypeBytes),
	)
	// Surface a loud, greppable warning when inlined base64 images dominate the
	// request: in long codex sessions an image stored in function_call_output is
	// re-sent every turn and is by far the most common cause of context blowups.
	// Token accounting is unaffected — the image is still counted as the upstream
	// counts it; this only makes "an image is eating the window" visible in logs.
	if p.ImageItems > 0 && percent(p.ImageBytes, p.TotalBytes) >= 50 {
		entry.Warnf(
			"codex request dominated by inlined base64 image(s) | site=%s model=%s image_items=%d image_bytes=%d (%.0f%% of request) total_bytes=%d — a tool returned image data into the conversation history; it re-sends every turn and rapidly fills the context window",
			label, model, p.ImageItems, p.ImageBytes, percent(p.ImageBytes, p.TotalBytes), p.TotalBytes,
		)
	}
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
