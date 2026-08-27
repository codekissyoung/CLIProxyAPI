package auth

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type failExecutor struct {
	provider string
	calls    atomic.Int32
}

func (e *failExecutor) Identifier() string { return e.provider }
func (e *failExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.calls.Add(1)
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusInternalServerError, Message: "upstream failure"}
}
func (e *failExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.calls.Add(1)
	return nil, &Error{HTTPStatus: http.StatusInternalServerError, Message: "upstream failure"}
}
func (e *failExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) { return auth, nil }
func (e *failExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (e *failExecutor) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	return nil, nil
}

type successExecutor struct {
	provider string
	calls    atomic.Int32
}

func (e *successExecutor) Identifier() string { return e.provider }
func (e *successExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.calls.Add(1)
	return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}
func (e *successExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.calls.Add(1)
	return nil, nil
}
func (e *successExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) { return auth, nil }
func (e *successExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (e *successExecutor) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestSessionAffinityAtomicCompareAndDeleteProtectsReboundSession(t *testing.T) {
	cache := NewSessionCache(time.Hour)
	defer cache.Stop()

	sessionKey := "mixed::sess-rebound::model-x"

	// 1. Initial binding to auth-A
	cache.Set(sessionKey, "auth-A")
	if got, ok := cache.Get(sessionKey); !ok || got != "auth-A" {
		t.Fatalf("Get() = %q, %v; want %q, true", got, ok, "auth-A")
	}

	// 2. Session rebinds to auth-B
	cache.Set(sessionKey, "auth-B")
	if got, ok := cache.Get(sessionKey); !ok || got != "auth-B" {
		t.Fatalf("Get() = %q, %v; want %q, true", got, ok, "auth-B")
	}

	// 3. Stale failure for auth-A tries to delete
	deleted := cache.CompareAndDelete(sessionKey, "auth-A")
	if deleted {
		t.Fatalf("CompareAndDelete with stale auth-A unexpectedly returned true")
	}
	// Session must still be bound to auth-B
	if got, ok := cache.Get(sessionKey); !ok || got != "auth-B" {
		t.Fatalf("Get() after stale delete attempt = %q, %v; want %q, true", got, ok, "auth-B")
	}

	// 4. Valid failure for auth-B deletes
	deletedValid := cache.CompareAndDelete(sessionKey, "auth-B")
	if !deletedValid {
		t.Fatalf("CompareAndDelete with active auth-B returned false")
	}
	if _, ok := cache.Get(sessionKey); ok {
		t.Fatalf("sessionKey still present in cache after valid CompareAndDelete")
	}
}

func TestSessionAffinityDelayedSuccessDoesNotOverwriteReboundAuth(t *testing.T) {
	affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &RoundRobinSelector{},
		TTL:      time.Hour,
	})
	defer affinity.Stop()

	sessionKey := "mixed::header:sess-delay-success::model-x"

	// 1. Initially auth-A is bound
	affinity.cache.Set(sessionKey, "auth-A")

	// 2. Session rebinds to auth-B
	affinity.cache.Set(sessionKey, "auth-B")

	// 3. A delayed success for auth-A arrives
	opts := cliproxyexecutor.Options{
		Headers: http.Header{"X-Session-Id": []string{"sess-delay-success"}},
		Metadata: map[string]any{
			cliproxyexecutor.SessionAffinityProviderMetadataKey: "mixed",
			cliproxyexecutor.SessionAffinityModelMetadataKey:    "model-x",
		},
	}
	affinity.OnResult(Result{
		AuthID:   "auth-A",
		Provider: "provider-a",
		Model:    "model-x",
		Success:  true,
		Options:  opts,
	})

	// 4. Cache must remain bound to auth-B, not overwritten by auth-A
	got, ok := affinity.cache.Get(sessionKey)
	if !ok || got != "auth-B" {
		t.Fatalf("cache binding = %q, %v; want auth-B, true (delayed success of auth-A must not overwrite auth-B)", got, ok)
	}
}

func TestSessionAffinityOnResultWithMismatchedNamespaceFailsToUnbind(t *testing.T) {
	affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &RoundRobinSelector{},
		TTL:      time.Hour,
	})
	defer affinity.Stop()

	sessionID := "header:sess-ns-1"
	model := "test-model"
	authID := "auth-1"

	// Bind under "mixed" namespace
	mixedKey := "mixed::" + sessionID + "::" + model
	affinity.cache.Set(mixedKey, authID)

	// Call OnResult with options carrying the propagated "mixed" namespace
	res := Result{
		AuthID:   authID,
		Provider: "gemini", // actual provider
		Model:    model,
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusInternalServerError},
		Options: cliproxyexecutor.Options{
			Headers: http.Header{"X-Session-Id": []string{"sess-ns-1"}},
			Metadata: map[string]any{
				cliproxyexecutor.SessionAffinityProviderMetadataKey: "mixed",
				cliproxyexecutor.SessionAffinityModelMetadataKey:    model,
			},
		},
	}

	affinity.OnResult(res)

	// Verify mixedKey is cleanly removed
	if _, ok := affinity.cache.Get(mixedKey); ok {
		t.Fatalf("expected mixed key to be removed after OnResult with propagated namespace")
	}
}
