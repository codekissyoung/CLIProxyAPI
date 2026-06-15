package executor

import (
	"context"
	"strings"
	"testing"
)

type guardIdent struct{}

func (guardIdent) Identifier() string { return "codex" }

func codexInputBody(text string) []byte {
	// Minimal codex-format body: one user message whose content carries `text`.
	var b strings.Builder
	b.WriteString(`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":`)
	b.WriteString(`"`)
	b.WriteString(text)
	b.WriteString(`"`)
	b.WriteString(`}]}]}`)
	return []byte(b.String())
}

func TestCheckCodexContextWindowAllowsSmallRequest(t *testing.T) {
	body := codexInputBody("hello world, this is a short prompt")
	if err := checkCodexContextWindow(context.Background(), guardIdent{}, "gpt-5.5", body, nil, "codex:test"); err != nil {
		t.Fatalf("small request should pass, got %v", err)
	}
}

func TestCheckCodexContextWindowRejectsOversizedRequest(t *testing.T) {
	// gpt-5.5 window is 272000 tokens; build text comfortably above it.
	// ~1 token per word here, so 400k words >> window.
	huge := strings.Repeat("token ", 400000)
	body := codexInputBody(huge)

	err := checkCodexContextWindow(context.Background(), guardIdent{}, "gpt-5.5", body, nil, "codex:test")
	if err == nil {
		t.Fatal("oversized request should be rejected by the guard")
	}
	if !strings.Contains(err.Error(), "context_too_large") {
		t.Fatalf("error should carry context_too_large code, got %v", err)
	}
	type statusCoder interface{ StatusCode() int }
	if sc, ok := err.(statusCoder); !ok || sc.StatusCode() != 400 {
		t.Fatalf("expected 400 status, got %v", err)
	}
}

func TestCheckCodexContextWindowFailsOpenOnUnknownModel(t *testing.T) {
	huge := strings.Repeat("token ", 400000)
	body := codexInputBody(huge)
	// Unknown model -> window 0 -> guard must not fire (fail-open).
	if err := checkCodexContextWindow(context.Background(), guardIdent{}, "totally-unknown-model", body, nil, ""); err != nil {
		t.Fatalf("unknown model should fail open, got %v", err)
	}
}

func TestCheckCodexContextWindowRejectsAt98Percent(t *testing.T) {
	// gpt-5.5 window = 272000; 98% = 266560. A request at ~270000 tokens is
	// below the full window but above the 98% threshold, so it must be rejected.
	words := strings.Repeat("token ", 270000)
	body := codexInputBody(words)

	err := checkCodexContextWindow(context.Background(), guardIdent{}, "gpt-5.5", body, nil, "codex:test")
	if err == nil {
		t.Fatal("request at ~270k tokens (>98% of 272000) should be rejected")
	}
	if !strings.Contains(err.Error(), "context_too_large") {
		t.Fatalf("expected context_too_large, got %v", err)
	}
}

func TestCheckCodexContextWindowAllowsBelow98Percent(t *testing.T) {
	// ~200000 tokens is well under 98% of 272000 (=266560); must pass.
	words := strings.Repeat("token ", 200000)
	body := codexInputBody(words)

	if err := checkCodexContextWindow(context.Background(), guardIdent{}, "gpt-5.5", body, nil, "codex:test"); err != nil {
		t.Fatalf("request comfortably under the threshold should pass, got %v", err)
	}
}
