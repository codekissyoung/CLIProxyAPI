package executor

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// Multi-user Pro account hardening converges OAuth traffic on the captured
// local Codex CLI 0.153.4 identity. These tests pin the rewrite behavior in
// applyCodexHeaders and applyCodexWebsocketHeaders.

func TestApplyCodexHeadersForcesNonMacOSClientUAToCanonical(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	req = req.WithContext(contextWithGinHeaders(map[string]string{
		"User-Agent": "codex-tui/0.128.0 (Linux 6.8.0; x86_64) iTerm.app/3.6.9 (codex-tui; 0.128.0)",
		"Originator": "codex-tui",
	}))

	applyCodexHeaders(req, auth, "oauth-token", true, nil)

	if got := req.Header.Get("User-Agent"); got != codexUserAgent {
		t.Fatalf("User-Agent = %s, want canonical %s", got, codexUserAgent)
	}
	if got := req.Header.Get("Originator"); got != codexOriginator {
		t.Fatalf("Originator = %s, want %s", got, codexOriginator)
	}
}

func TestApplyCodexHeadersForcesOriginatorEvenWhenClientSendsConflicting(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	req = req.WithContext(contextWithGinHeaders(map[string]string{
		"User-Agent": "codex_cli_rs/0.1.0 (linux-gnu)",
		"Originator": "Codex Desktop",
	}))

	applyCodexHeaders(req, auth, "oauth-token", true, nil)

	if got := req.Header.Get("User-Agent"); got != codexUserAgent {
		t.Fatalf("User-Agent = %s, want canonical %s", got, codexUserAgent)
	}
	// UA was forced, so Originator must follow the captured TUI identity.
	if got := req.Header.Get("Originator"); got != codexOriginator {
		t.Fatalf("Originator = %s, want %s (UA was rewritten, must stay in lockstep)", got, codexOriginator)
	}
}

func TestApplyCodexHeadersAlsoConvergesMacOSClientUA(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	macUA := "codex-tui/0.130.0 (Mac OS 14.6.1; arm64) iTerm.app/3.6.9 (codex-tui; 0.130.0)"
	req = req.WithContext(contextWithGinHeaders(map[string]string{
		"User-Agent": macUA,
		"Originator": "codex-tui",
	}))

	applyCodexHeaders(req, auth, "oauth-token", true, nil)

	if got := req.Header.Get("User-Agent"); got != codexUserAgent {
		t.Fatalf("User-Agent = %s, want canonical %s", got, codexUserAgent)
	}
	if got := req.Header.Get("Originator"); got != codexOriginator {
		t.Fatalf("Originator = %s, want %s", got, codexOriginator)
	}
}

func TestApplyCodexHeadersDoesNotForceUAForAPIKeyAuth(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	auth := &cliproxyauth.Auth{
		Provider:   "codex",
		Attributes: map[string]string{"api_key": "sk-test"},
	}
	customUA := "my-third-party/2.0 (Linux)"
	req = req.WithContext(contextWithGinHeaders(map[string]string{
		"User-Agent": customUA,
		"Originator": "my-app",
	}))

	applyCodexHeaders(req, auth, "sk-test", true, nil)

	if got := req.Header.Get("User-Agent"); got != customUA {
		t.Fatalf("User-Agent = %s, want untouched %s (API-key auth)", got, customUA)
	}
	if got := req.Header.Get("Originator"); got != "my-app" {
		t.Fatalf("Originator = %s, want my-app (API-key auth)", got)
	}
}

func TestApplyCodexHeadersRespectsAdminCfgUserAgentEvenIfNonMacOS(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{UserAgent: "admin-set-ua/1.0 (linux)"},
	}
	req = req.WithContext(contextWithGinHeaders(map[string]string{
		"User-Agent": "client-ua",
		"Originator": "admin-origin",
	}))

	applyCodexHeaders(req, auth, "oauth-token", true, cfg)

	if got := req.Header.Get("User-Agent"); got != "admin-set-ua/1.0 (linux)" {
		t.Fatalf("User-Agent = %s, want admin override untouched", got)
	}
	// cfg UA was honored — admin's choice; we do not also force Originator.
	if got := req.Header.Get("Originator"); got != "admin-origin" {
		t.Fatalf("Originator = %s, want admin-origin (no forced override when cfg UA is set)", got)
	}
}

func TestApplyCodexWebsocketHeadersForcesNonMacOSClientUAToCanonical(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	ctx := contextWithGinHeaders(map[string]string{
		"User-Agent": "codex_cli_rs/0.1.0",
		"Originator": "Codex Desktop",
	})

	headers := applyCodexWebsocketHeaders(ctx, http.Header{}, auth, "", nil)

	if got := headers.Get("User-Agent"); got != codexUserAgent {
		t.Fatalf("User-Agent = %s, want canonical %s", got, codexUserAgent)
	}
	if got := headers.Get("Originator"); got != codexOriginator {
		t.Fatalf("Originator = %s, want %s (UA rewritten, must stay in lockstep)", got, codexOriginator)
	}
}

func TestApplyCodexWebsocketHeadersAlsoConvergesMacOSClientUA(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	macUA := "codex-tui/0.130.0 (Mac OS 14.6.1; arm64) iTerm.app/3.6.9 (codex-tui; 0.130.0)"
	ctx := contextWithGinHeaders(map[string]string{
		"User-Agent": macUA,
		"Originator": "Codex Desktop",
	})

	headers := applyCodexWebsocketHeaders(ctx, http.Header{}, auth, "", nil)

	if got := headers.Get("User-Agent"); got != codexUserAgent {
		t.Fatalf("User-Agent = %s, want canonical %s", got, codexUserAgent)
	}
	if got := headers.Get("Originator"); got != codexOriginator {
		t.Fatalf("Originator = %s, want %s", got, codexOriginator)
	}
}

func TestApplyCodexWebsocketHeadersRespectsAdminCfgUserAgent(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{UserAgent: "admin-set-ua/1.0 (linux)"},
	}

	headers := applyCodexWebsocketHeaders(context.Background(), http.Header{}, auth, "", cfg)

	if got := headers.Get("User-Agent"); got != "admin-set-ua/1.0 (linux)" {
		t.Fatalf("User-Agent = %s, want admin override untouched", got)
	}
}

func TestCanonicalCodexUserAgentMatchesOriginatorAndVersion(t *testing.T) {
	if !strings.HasPrefix(codexUserAgent, codexOriginator+"/"+codexVersion+" ") {
		t.Fatalf("codexUserAgent = %q, want %s/%s prefix", codexUserAgent, codexOriginator, codexVersion)
	}
	if !strings.Contains(codexUserAgent, "("+codexOriginator+"; "+codexVersion+")") {
		t.Fatalf("codexUserAgent = %q, want matching originator/version suffix", codexUserAgent)
	}
}

// A Codex request must never reach chatgpt.com with Go's default
// Go-http-client UA. The risk window is an API-key client that sends no UA at
// all (OAuth identity forcing is skipped for API-key auth), so the safety-net fallback
// must pin codexUserAgent on both the REST and websocket paths.

func TestApplyCodexHeadersNeverLeavesEmptyUAForAPIKeyWithoutClientUA(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	auth := &cliproxyauth.Auth{
		Provider:   "codex",
		Attributes: map[string]string{"api_key": "sk-test"},
	}
	// Client sends no User-Agent header at all.
	req = req.WithContext(contextWithGinHeaders(map[string]string{}))

	applyCodexHeaders(req, auth, "sk-test", true, nil)

	got := req.Header.Get("User-Agent")
	if got == "" || strings.Contains(got, "Go-http-client") {
		t.Fatalf("User-Agent = %q, must never be empty or Go-http-client", got)
	}
	if got != codexUserAgent {
		t.Fatalf("User-Agent = %q, want canonical fallback %q", got, codexUserAgent)
	}
}

func TestApplyCodexWebsocketHeadersNeverLeavesEmptyUAForAPIKeyWithoutClientUA(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider:   "codex",
		Attributes: map[string]string{"api_key": "sk-test"},
	}
	// Client sends no User-Agent header at all.
	ctx := contextWithGinHeaders(map[string]string{})

	headers := applyCodexWebsocketHeaders(ctx, http.Header{}, auth, "sk-test", nil)

	got := headers.Get("User-Agent")
	if got == "" || strings.Contains(got, "Go-http-client") {
		t.Fatalf("User-Agent = %q, must never be empty or Go-http-client", got)
	}
	if got != codexUserAgent {
		t.Fatalf("User-Agent = %q, want canonical fallback %q", got, codexUserAgent)
	}
}
