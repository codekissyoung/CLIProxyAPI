package executor

import (
	"net/http"
	"strings"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// The responses-lite marker header must reach the upstream: codex-tui sends a
// lite-shaped body (developer message with embedded tools) that the backend
// only deserializes when X-OpenAI-Internal-Codex-Responses-Lite accompanies
// it. Dropping the header produced 422 "untagged enum ModelInput" rejections
// that Codex CLI retried in a tight loop (2026-07-15 incident).

func TestApplyCodexHeadersForwardsResponsesLiteHeader(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	req = req.WithContext(contextWithGinHeaders(map[string]string{
		"User-Agent":             "codex-tui/0.144.1 (Ubuntu 26.4.0; x86_64) xterm-256color (codex-tui; 0.144.1)",
		"Originator":             "codex-tui",
		codexResponsesLiteHeader: "true",
	}))

	applyCodexHeaders(req, auth, "oauth-token", true, nil)

	if got := req.Header.Get(codexResponsesLiteHeader); got != "true" {
		t.Fatalf("%s = %q, want forwarded %q", codexResponsesLiteHeader, got, "true")
	}
}

func TestApplyCodexHeadersOmitsResponsesLiteHeaderWhenClientDidNotSendIt(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	req = req.WithContext(contextWithGinHeaders(map[string]string{
		"User-Agent": "codex-tui/0.144.1 (Ubuntu 26.4.0; x86_64) xterm-256color (codex-tui; 0.144.1)",
		"Originator": "codex-tui",
	}))

	applyCodexHeaders(req, auth, "oauth-token", true, nil)

	if got := req.Header.Get(codexResponsesLiteHeader); got != "" {
		t.Fatalf("%s = %q, want absent", codexResponsesLiteHeader, got)
	}
}

func TestNewCodexStatusErrDowngrades422ToNonRetryable400(t *testing.T) {
	serdeMsg := "Failed to deserialize the JSON body into the target type: data did not match any variant of untagged enum ModelInput"
	statusError := newCodexStatusErr(http.StatusUnprocessableEntity, []byte(`{"error":"`+serdeMsg+`"}`))

	if statusError.code != http.StatusBadRequest {
		t.Fatalf("code = %d, want %d", statusError.code, http.StatusBadRequest)
	}
	if got := gjson.Get(statusError.msg, "error.type").String(); got != "invalid_request_error" {
		t.Fatalf("error.type = %q, want invalid_request_error (conductor keys non-retryable handling off this)", got)
	}
	if got := gjson.Get(statusError.msg, "error.message").String(); got != serdeMsg {
		t.Fatalf("error.message = %q, want original upstream message preserved", got)
	}
}

func TestNewCodexStatusErrLeavesOtherStatusesAlone(t *testing.T) {
	statusError := newCodexStatusErr(http.StatusBadGateway, []byte(`upstream exploded`))
	if statusError.code != http.StatusBadGateway {
		t.Fatalf("code = %d, want %d", statusError.code, http.StatusBadGateway)
	}
	if statusError.msg != "upstream exploded" {
		t.Fatalf("msg = %q, want passthrough", statusError.msg)
	}
}

func TestXAIStatusErrDowngrades422ToNonRetryable400(t *testing.T) {
	serdeMsg := "Failed to deserialize the JSON body into the target type: data did not match any variant of untagged enum ModelInput"
	statusError := xaiStatusErr(http.StatusUnprocessableEntity, []byte(`{"error":"`+serdeMsg+`"}`))

	if statusError.code != http.StatusBadRequest {
		t.Fatalf("code = %d, want %d", statusError.code, http.StatusBadRequest)
	}
	if !strings.Contains(statusError.msg, "invalid_request_error") {
		t.Fatalf("msg = %q, want invalid_request_error marker", statusError.msg)
	}
	if got := gjson.Get(statusError.msg, "error.message").String(); got != serdeMsg {
		t.Fatalf("error.message = %q, want original upstream message preserved", got)
	}
}

func TestXAIStatusErrKeepsFreeUsageExhaustedCooldown(t *testing.T) {
	statusError := xaiStatusErr(http.StatusTooManyRequests, []byte(`{"code":"free-usage-exhausted","error":"quota"}`))
	if statusError.code != http.StatusTooManyRequests {
		t.Fatalf("code = %d, want %d", statusError.code, http.StatusTooManyRequests)
	}
	if statusError.retryAfter == nil {
		t.Fatalf("retryAfter = nil, want cooldown preserved")
	}
}
