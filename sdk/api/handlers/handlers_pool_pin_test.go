package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"golang.org/x/net/context"
)

func TestWithPoolPinFromRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newGinCtx := func() *gin.Context {
		ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		ginCtx.Request.Header.Set(PoolPinAccountHeader, "codex-foo.json")
		return ginCtx
	}

	t.Run("pins auth when enabled", func(t *testing.T) {
		ctx := WithPoolPinFromRequest(context.Background(), &config.SDKConfig{AllowPoolPinHeader: true}, newGinCtx())
		if got := pinnedAuthIDFromContext(ctx); got != "codex-foo.json" {
			t.Fatalf("pinned auth ID = %q, want %q", got, "codex-foo.json")
		}
	})

	t.Run("ignores header when disabled", func(t *testing.T) {
		ctx := WithPoolPinFromRequest(context.Background(), &config.SDKConfig{}, newGinCtx())
		if got := pinnedAuthIDFromContext(ctx); got != "" {
			t.Fatalf("pinned auth ID = %q, want empty", got)
		}
	})

	t.Run("ignores header with nil config", func(t *testing.T) {
		ctx := WithPoolPinFromRequest(context.Background(), nil, newGinCtx())
		if got := pinnedAuthIDFromContext(ctx); got != "" {
			t.Fatalf("pinned auth ID = %q, want empty", got)
		}
	})

	t.Run("ignores empty header", func(t *testing.T) {
		ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		ctx := WithPoolPinFromRequest(context.Background(), &config.SDKConfig{AllowPoolPinHeader: true}, ginCtx)
		if got := pinnedAuthIDFromContext(ctx); got != "" {
			t.Fatalf("pinned auth ID = %q, want empty", got)
		}
	})
}

func TestRequestExecutionMetadataPoolAccountCallback(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("records selected auth ID", func(t *testing.T) {
		ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		ctx := context.WithValue(context.Background(), "gin", ginCtx)

		meta := requestExecutionMetadata(ctx)
		callback, ok := meta[coreexecutor.SelectedAuthCallbackMetadataKey].(func(string))
		if !ok || callback == nil {
			t.Fatal("missing selected auth callback for ordinary HTTP request")
		}
		callback("codex-foo.json")
		if got := logging.GetGinCPAPoolAccount(ginCtx); got != "codex-foo.json" {
			t.Fatalf("pool account = %q, want %q", got, "codex-foo.json")
		}
	})

	t.Run("chains an existing selected auth callback", func(t *testing.T) {
		ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		var existing string
		ctx := WithSelectedAuthIDCallback(context.Background(), func(authID string) { existing = authID })
		ctx = context.WithValue(ctx, "gin", ginCtx)

		meta := requestExecutionMetadata(ctx)
		callback, ok := meta[coreexecutor.SelectedAuthCallbackMetadataKey].(func(string))
		if !ok || callback == nil {
			t.Fatal("missing selected auth callback")
		}
		callback("codex-foo.json")
		if existing != "codex-foo.json" {
			t.Fatalf("existing callback received %q, want %q", existing, "codex-foo.json")
		}
		if got := logging.GetGinCPAPoolAccount(ginCtx); got != "codex-foo.json" {
			t.Fatalf("pool account = %q, want %q", got, "codex-foo.json")
		}
	})

	t.Run("skips websocket upgrade", func(t *testing.T) {
		ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
		ginCtx.Request.Header.Set("Connection", "Upgrade")
		ginCtx.Request.Header.Set("Upgrade", "websocket")
		ctx := context.WithValue(context.Background(), "gin", ginCtx)

		meta := requestExecutionMetadata(ctx)
		if _, exists := meta[coreexecutor.SelectedAuthCallbackMetadataKey]; exists {
			t.Fatal("unexpected selected auth callback for websocket upgrade")
		}
	})
}

func TestPoolAccountHeaderIsReserved(t *testing.T) {
	if !IsCPAReservedResponseHeader("X-Pool-Account") {
		t.Fatal("X-Pool-Account must be a CPA-reserved response header")
	}
	filtered := FilterUpstreamHeaders(http.Header{
		"X-Pool-Account": []string{"spoofed.json"},
		"X-Other":        []string{"kept"},
	})
	if filtered.Get("X-Pool-Account") != "" {
		t.Fatal("upstream X-Pool-Account header was not filtered")
	}
	if filtered.Get("X-Other") != "kept" {
		t.Fatal("unrelated header was filtered")
	}
}
