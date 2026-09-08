package executor

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestStripCodexBodyTurnMetadataWorkspaces(t *testing.T) {
	t.Run("string form stripped", func(t *testing.T) {
		in := []byte(`{"model":"gpt-5","client_metadata":{"x-codex-turn-metadata":"{\"session_id\":\"s1\",\"workspaces\":[{\"cwd\":\"/home/user/private\",\"git_remote\":\"git@github.com:x/y.git\"}]}"}}`)
		out := stripCodexBodyTurnMetadataWorkspaces(in)
		turnMetadata := gjson.GetBytes(out, "client_metadata.x-codex-turn-metadata").String()
		if gjson.Get(turnMetadata, "workspaces").Exists() {
			t.Fatalf("workspaces still present in body turn metadata: %s", turnMetadata)
		}
		if got := gjson.Get(turnMetadata, "session_id").String(); got != "s1" {
			t.Fatalf("session_id = %q, want preserved %q", got, "s1")
		}
	})
	t.Run("object form stripped", func(t *testing.T) {
		in := []byte(`{"client_metadata":{"x-codex-turn-metadata":{"session_id":"s1","workspaces":[{"cwd":"/home/user/private"}]}}}`)
		out := stripCodexBodyTurnMetadataWorkspaces(in)
		if gjson.GetBytes(out, "client_metadata.x-codex-turn-metadata.workspaces").Exists() {
			t.Fatalf("workspaces still present: %s", out)
		}
		if got := gjson.GetBytes(out, "client_metadata.x-codex-turn-metadata.session_id").String(); got != "s1" {
			t.Fatalf("session_id = %q, want preserved %q", got, "s1")
		}
	})
	t.Run("no workspaces unchanged", func(t *testing.T) {
		in := []byte(`{"client_metadata":{"x-codex-turn-metadata":"{\"session_id\":\"s1\"}"}}`)
		if out := stripCodexBodyTurnMetadataWorkspaces(in); string(out) != string(in) {
			t.Fatalf("payload changed without workspaces: %s", out)
		}
	})
}

func TestStripCodexBodyIdentityMetadataMirrors(t *testing.T) {
	in := []byte(`{"client_metadata":{` +
		`"ws_request_header_session_id":"sess-1",` +
		`"ws_request_header_x_codex_turn_state":"opaque",` +
		`"ws_request_header_x_codex_turn_metadata":"{}",` +
		`"ws_request_header_thread_id":"thread-1",` +
		`"ws_request_header_x_openai_internal_codex_responses_lite":"true",` +
		`"other_field":"keep"}}`)
	out := stripCodexBodyIdentityMetadataMirrors(in)
	for _, key := range []string{
		"ws_request_header_session_id",
		"ws_request_header_x_codex_turn_state",
		"ws_request_header_x_codex_turn_metadata",
		"ws_request_header_thread_id",
	} {
		if gjson.GetBytes(out, "client_metadata."+key).Exists() {
			t.Fatalf("identity mirror %s still present: %s", key, out)
		}
	}
	if got := gjson.GetBytes(out, "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite").String(); got != "true" {
		t.Fatalf("responses-lite mirror = %q, want preserved", got)
	}
	if got := gjson.GetBytes(out, "client_metadata.other_field").String(); got != "keep" {
		t.Fatalf("other_field = %q, want preserved", got)
	}
}

func TestApplyCodexWebsocketHeadersDropsTurnState(t *testing.T) {
	ctx := contextWithGinHeaders(map[string]string{
		"X-Codex-Turn-State":    "opaque-turn-state",
		"X-Codex-Turn-Metadata": `{"session_id":"s1"}`,
	})
	auth := &cliproxyauth.Auth{ID: "auth-1", Provider: "codex"}
	headers := applyCodexWebsocketHeaders(ctx, nil, auth, "oauth-token", nil)
	if got := headers.Get("x-codex-turn-state"); got != "" {
		t.Fatalf("x-codex-turn-state = %q, want stripped from upstream handshake", got)
	}
	if got := headers.Get("x-codex-turn-metadata"); got == "" {
		t.Fatal("x-codex-turn-metadata should still pass through")
	}
}

func TestApplyCodexTurnMetadataIdentityConfuseSessionAndThread(t *testing.T) {
	state := &codexIdentityConfuseState{enabled: true, authID: "auth-1"}
	in := `{"prompt_cache_key":"cache-1","turn_id":"turn-1","session_id":"sess-1","thread_id":"thread-1","window_id":"cache-1:0"}`
	out := applyCodexTurnMetadataIdentityConfuse(in, state)

	wantSession := codexIdentityConfuseUUID("auth-1", "session", "sess-1")
	wantThread := codexIdentityConfuseUUID("auth-1", "thread", "thread-1")
	if got := gjson.Get(out, "session_id").String(); got != wantSession {
		t.Fatalf("session_id = %q, want %q", got, wantSession)
	}
	if got := gjson.Get(out, "thread_id").String(); got != wantThread {
		t.Fatalf("thread_id = %q, want %q", got, wantThread)
	}

	// A second account must produce different values for the same identifiers.
	otherState := &codexIdentityConfuseState{enabled: true, authID: "auth-2"}
	otherOut := applyCodexTurnMetadataIdentityConfuse(in, otherState)
	if gjson.Get(otherOut, "session_id").String() == wantSession {
		t.Fatal("session_id identical across accounts, want per-account rewrite")
	}

	// Responses carrying the confused identifiers must be restored for the client.
	payload := []byte(`{"session_id":"` + wantSession + `","thread_id":"` + wantThread + `"}`)
	restored := applyCodexIdentityExposeResponsePayload(payload, *state)
	if got := gjson.GetBytes(restored, "session_id").String(); got != "sess-1" {
		t.Fatalf("restored session_id = %q, want %q", got, "sess-1")
	}
	if got := gjson.GetBytes(restored, "thread_id").String(); got != "thread-1" {
		t.Fatalf("restored thread_id = %q, want %q", got, "thread-1")
	}
}

func TestApplyCodexIdentityConfuseBodyTopLevelSessionIdentifiers(t *testing.T) {
	cfg := &config.Config{Codex: config.CodexConfig{IdentityConfuse: true}}
	auth := &cliproxyauth.Auth{ID: "auth-1", Provider: "codex"}
	userPayload := []byte(`{"model":"gpt-5","prompt_cache_key":"cache-1","session_id":"sess-1","conversation":"conv-1"}`)
	upstreamBody, state := applyCodexIdentityConfuseBody(cfg, auth, userPayload, userPayload)

	wantSession := codexIdentityConfuseUUID("auth-1", "session", "sess-1")
	wantConversation := codexIdentityConfuseUUID("auth-1", "conversation", "conv-1")
	if got := gjson.GetBytes(upstreamBody, "session_id").String(); got != wantSession {
		t.Fatalf("session_id = %q, want %q", got, wantSession)
	}
	if got := gjson.GetBytes(upstreamBody, "conversation").String(); got != wantConversation {
		t.Fatalf("conversation = %q, want %q", got, wantConversation)
	}
	restored := applyCodexIdentityExposeResponsePayload(upstreamBody, state)
	if got := gjson.GetBytes(restored, "session_id").String(); got != "sess-1" {
		t.Fatalf("restored session_id = %q, want %q", got, "sess-1")
	}
}

func TestAccountScopedPromptCacheKey(t *testing.T) {
	authA := &cliproxyauth.Auth{ID: "auth-a", Provider: "codex"}
	authB := &cliproxyauth.Auth{ID: "auth-b", Provider: "codex"}

	if got := accountScopedPromptCacheKey("client-key", true, authA); got != "client-key" {
		t.Fatalf("client-provided key = %q, want passthrough %q", got, "client-key")
	}
	gotA := accountScopedPromptCacheKey("generated-key", false, authA)
	if gotA == "generated-key" || gotA == "" {
		t.Fatalf("generated key not account-scoped: %q", gotA)
	}
	if again := accountScopedPromptCacheKey("generated-key", false, authA); again != gotA {
		t.Fatalf("generated key unstable within one account: %q vs %q", again, gotA)
	}
	if gotB := accountScopedPromptCacheKey("generated-key", false, authB); gotB == gotA {
		t.Fatal("generated key identical across accounts, want per-account scope")
	}
	if got := accountScopedPromptCacheKey("generated-key", false, nil); got != "generated-key" {
		t.Fatalf("nil auth = %q, want unchanged %q", got, "generated-key")
	}
}

func TestApplyCodexPromptCacheHeadersScopesGeneratedKeyPerAccount(t *testing.T) {
	req := cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: []byte(`{"model":"gpt-5-codex"}`),
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "exec-session-1",
		},
	}
	payload := []byte(`{"model":"gpt-5-codex","input":[]}`)
	authA := &cliproxyauth.Auth{ID: "auth-a", Provider: "codex"}
	authB := &cliproxyauth.Auth{ID: "auth-b", Provider: "codex"}

	bodyA, headersA, err := applyCodexPromptCacheHeadersWithAuth(context.Background(), authA, sdktranslator.FormatOpenAIResponse, req, payload)
	if err != nil {
		t.Fatalf("auth A error: %v", err)
	}
	bodyB, _, err := applyCodexPromptCacheHeadersWithAuth(context.Background(), authB, sdktranslator.FormatOpenAIResponse, req, payload)
	if err != nil {
		t.Fatalf("auth B error: %v", err)
	}
	keyA := gjson.GetBytes(bodyA, "prompt_cache_key").String()
	keyB := gjson.GetBytes(bodyB, "prompt_cache_key").String()
	if keyA == "" || keyB == "" {
		t.Fatalf("missing generated prompt_cache_key: A=%q B=%q", keyA, keyB)
	}
	if keyA == keyB {
		t.Fatalf("generated prompt_cache_key identical across accounts: %q", keyA)
	}
	if got := headersA["session_id"]; len(got) != 1 || got[0] != keyA {
		t.Fatalf("session_id header = %#v, want [%q]", got, keyA)
	}

	// A client-provided key passes through unscoped here (confuse stage remaps it).
	reqWithKey := req
	reqWithKey.Payload = []byte(`{"model":"gpt-5-codex","prompt_cache_key":"client-key"}`)
	bodyClient, _, err := applyCodexPromptCacheHeadersWithAuth(context.Background(), authA, sdktranslator.FormatOpenAIResponse, reqWithKey, reqWithKey.Payload)
	if err != nil {
		t.Fatalf("client key error: %v", err)
	}
	if got := gjson.GetBytes(bodyClient, "prompt_cache_key").String(); got != "client-key" {
		t.Fatalf("client-provided prompt_cache_key = %q, want %q", got, "client-key")
	}
}

func TestCodexIdentityConfuseEnabledIgnoresRoutingStrategy(t *testing.T) {
	cfg := &config.Config{Codex: config.CodexConfig{IdentityConfuse: true}}
	if !codexIdentityConfuseEnabled(cfg) {
		t.Fatal("identity confuse disabled despite identity-confuse: true; routing strategy must not gate it")
	}
	if codexIdentityConfuseEnabled(&config.Config{}) {
		t.Fatal("identity confuse enabled without identity-confuse: true")
	}
	if codexIdentityConfuseEnabled(nil) {
		t.Fatal("identity confuse enabled with nil config")
	}
}
