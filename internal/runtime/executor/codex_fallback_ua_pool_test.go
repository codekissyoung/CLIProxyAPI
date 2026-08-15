package executor

import (
	"net/http"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestCodexFallbackUserAgentMatchesCapturedTUI0147(t *testing.T) {
	want := "codex-tui/0.147.0 (Ubuntu 24.4.0; x86_64) dumb (codex-tui; 0.147.0)"
	if codexUserAgent != want {
		t.Fatalf("codexUserAgent = %q, want captured Codex CLI 0.147.0 UA %q", codexUserAgent, want)
	}
	if codexOriginator != "codex-tui" {
		t.Fatalf("codexOriginator = %q, want codex-tui", codexOriginator)
	}
	if codexVersion != "0.147.0" {
		t.Fatalf("codexVersion = %q, want 0.147.0", codexVersion)
	}
}

func TestCodexFallbackUserAgentIsStableAcrossAccounts(t *testing.T) {
	auths := []*cliproxyauth.Auth{
		nil,
		{Provider: "codex"},
		{ID: "account-a", Provider: "codex"},
		{ID: "account-b", Provider: "codex"},
	}
	for _, auth := range auths {
		if got := codexFallbackUserAgent(auth); got != codexUserAgent {
			t.Fatalf("codexFallbackUserAgent(%v) = %q, want %q", auth, got, codexUserAgent)
		}
	}
}

func TestApplyCodexHeadersForcesStableCLIIdentityForOAuth(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "account-a", Provider: "codex"}
	req, errRequest := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if errRequest != nil {
		t.Fatal(errRequest)
	}
	req = req.WithContext(contextWithGinHeaders(map[string]string{
		"User-Agent": "Codex Desktop/0.146.0-alpha.3.1 (Mac OS 26.5.2; arm64)",
		"Originator": "Codex Desktop",
	}))

	applyCodexHeaders(req, auth, "oauth-token", true, nil)

	if got := req.Header.Get("User-Agent"); got != codexUserAgent {
		t.Fatalf("User-Agent = %q, want stable CLI UA %q", got, codexUserAgent)
	}
	if got := req.Header.Get("Originator"); got != codexOriginator {
		t.Fatalf("Originator = %q, want %q", got, codexOriginator)
	}
	if got := req.Header.Get("Version"); got != codexVersion {
		t.Fatalf("Version = %q, want %q", got, codexVersion)
	}
	if got := req.Header.Get("X-Codex-Beta-Features"); got != codexBetaFeatures {
		t.Fatalf("X-Codex-Beta-Features = %q, want %q", got, codexBetaFeatures)
	}
}
