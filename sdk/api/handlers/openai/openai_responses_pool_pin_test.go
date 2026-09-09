package openai

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	requestlogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

const poolPinTestModel = "pool-pin-test-model"

type poolPinCaptureExecutor struct {
	mu       sync.Mutex
	auths    []string
	metadata []map[string]any
}

func (*poolPinCaptureExecutor) Identifier() string { return "codex" }

func (e *poolPinCaptureExecutor) Execute(_ context.Context, auth *coreauth.Auth, _ coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.auths = append(e.auths, auth.ID)
	e.metadata = append(e.metadata, maps.Clone(opts.Metadata))
	return coreexecutor.Response{Payload: []byte(`{"id":"resp-pool-pin-ok"}`)}, nil
}

func (*poolPinCaptureExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, errors.New("not implemented")
}

func (*poolPinCaptureExecutor) Refresh(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
	return nil, errors.New("not implemented")
}

func (*poolPinCaptureExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (*poolPinCaptureExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func (e *poolPinCaptureExecutor) captured() (int, []string, []map[string]any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.auths), append([]string(nil), e.auths...), append([]map[string]any(nil), e.metadata...)
}

func newPoolPinTestServer(t *testing.T, cfg *sdkconfig.SDKConfig, executor *poolPinCaptureExecutor, authIDs ...string) *httptest.Server {
	t.Helper()

	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	for _, authID := range authIDs {
		auth := &coreauth.Auth{ID: authID, Provider: "codex", Status: coreauth.StatusActive}
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("manager.Register(%q): %v", authID, errRegister)
		}
		registry.GetGlobalRegistry().RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: poolPinTestModel}})
		t.Cleanup(func() {
			registry.GetGlobalRegistry().UnregisterClient(authID)
		})
	}

	gin.SetMode(gin.TestMode)
	base := handlers.NewBaseAPIHandlers(cfg, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.Use(requestlogging.CPATraceIDMiddleware())
	router.POST("/v1/responses", h.Responses)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server
}

func postPoolPinRequest(t *testing.T, server *httptest.Server, pinAuthID string) *httptest.ResponseRecorder {
	t.Helper()

	requestBody := `{"model":"` + poolPinTestModel + `","input":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(requestBody))
	if pinAuthID != "" {
		req.Header.Set(handlers.PoolPinAccountHeader, pinAuthID)
	}
	recorder := httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(recorder, req)
	return recorder
}

func TestResponsesPoolPinHeaderSelectsPinnedAuth(t *testing.T) {
	executor := &poolPinCaptureExecutor{}
	server := newPoolPinTestServer(t, &sdkconfig.SDKConfig{AllowPoolPinHeader: true}, executor, "pool-a.json", "pool-b.json")

	recorder := postPoolPinRequest(t, server, "pool-b.json")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := recorder.Header().Get(requestlogging.CPAPoolAccountHeader); got != "pool-b.json" {
		t.Fatalf("X-Pool-Account = %q, want %q", got, "pool-b.json")
	}
	calls, auths, metadata := executor.captured()
	if calls != 1 {
		t.Fatalf("executor calls = %d, want 1", calls)
	}
	if auths[0] != "pool-b.json" {
		t.Fatalf("serving auth = %q, want pinned %q", auths[0], "pool-b.json")
	}
	if got := metadata[0][coreexecutor.PinnedAuthMetadataKey]; got != "pool-b.json" {
		t.Fatalf("pinned auth metadata = %#v, want %q", got, "pool-b.json")
	}
	if body := recorder.Body.String(); !strings.Contains(body, "resp-pool-pin-ok") {
		t.Fatalf("body = %q, want executor payload", body)
	}
}

func TestResponsesPoolPinHeaderIgnoredWhenDisabled(t *testing.T) {
	executor := &poolPinCaptureExecutor{}
	server := newPoolPinTestServer(t, &sdkconfig.SDKConfig{}, executor, "pool-a.json", "pool-b.json")

	recorder := postPoolPinRequest(t, server, "pool-b.json")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	calls, auths, metadata := executor.captured()
	if calls != 1 {
		t.Fatalf("executor calls = %d, want 1", calls)
	}
	if _, exists := metadata[0][coreexecutor.PinnedAuthMetadataKey]; exists {
		t.Fatalf("pinned auth metadata present despite disabled flag: %#v", metadata[0][coreexecutor.PinnedAuthMetadataKey])
	}
	// Attribution stays on regardless of the pin flag.
	if got := recorder.Header().Get(requestlogging.CPAPoolAccountHeader); got != auths[0] {
		t.Fatalf("X-Pool-Account = %q, want serving auth %q", got, auths[0])
	}
}

func TestResponsesPoolPinHeaderUnknownAuthFails(t *testing.T) {
	executor := &poolPinCaptureExecutor{}
	server := newPoolPinTestServer(t, &sdkconfig.SDKConfig{AllowPoolPinHeader: true}, executor, "pool-a.json")

	recorder := postPoolPinRequest(t, server, "missing.json")

	if recorder.Code == http.StatusOK {
		t.Fatalf("status = %d, want failure for unknown pinned auth; body: %s", recorder.Code, recorder.Body.String())
	}
	if calls, _, _ := executor.captured(); calls != 0 {
		t.Fatalf("executor calls = %d, want 0 for unknown pinned auth", calls)
	}
	if got := recorder.Header().Get(requestlogging.CPAPoolAccountHeader); got != "" {
		t.Fatalf("X-Pool-Account = %q, want empty when no auth served", got)
	}
}
