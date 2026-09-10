package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

const overloadUpstreamBody = `{"error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later.","param":null}}`

func TestIsOverloadResultError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  *Error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "503 production overload body",
			err:  &Error{HTTPStatus: http.StatusServiceUnavailable, Message: overloadUpstreamBody},
			want: true,
		},
		{
			name: "502 wrapped overload body",
			err:  &Error{HTTPStatus: http.StatusBadGateway, Message: overloadUpstreamBody},
			want: true,
		},
		{
			name: "structured code without status",
			err:  &Error{Code: "server_is_overloaded", Message: "Our servers are currently overloaded. Please try again later."},
			want: true,
		},
		{
			name: "503 gateway family with overloaded message",
			err:  &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "upstream server is overloaded"},
			want: true,
		},
		{
			name: "504 gateway family with overloaded message",
			err:  &Error{HTTPStatus: http.StatusGatewayTimeout, Message: "Overloaded"},
			want: true,
		},
		{
			name: "429 rate limit is quota accounting",
			err:  &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limit exceeded"},
			want: false,
		},
		{
			name: "429 mentioning overload code stays excluded",
			err:  &Error{HTTPStatus: http.StatusTooManyRequests, Message: overloadUpstreamBody},
			want: false,
		},
		{
			name: "401 unauthorized",
			err:  &Error{HTTPStatus: http.StatusUnauthorized, Message: "invalid authentication credentials"},
			want: false,
		},
		{
			name: "generic 500",
			err:  &Error{HTTPStatus: http.StatusInternalServerError, Message: "internal server error"},
			want: false,
		},
		{
			name: "503 without overload signal",
			err:  &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "service temporarily unavailable"},
			want: false,
		},
		{
			name: "request-scoped error",
			err:  &Error{Code: ErrorCodeRequestScoped, Message: "request invalid"},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isOverloadResultError(tc.err); got != tc.want {
				t.Fatalf("isOverloadResultError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// claudeAffinityTestOptions builds explicit Claude harness session options with a
// parent session so the home, model, and alias bindings are all populated.
func claudeAffinityTestOptions() cliproxyexecutor.Options {
	return cliproxyexecutor.Options{
		Headers: http.Header{
			"X-Claude-Code-Session-Id": []string{"sess-1"},
		},
		OriginalRequest: []byte(`{"parent_session_id":"parent-1","messages":[{"role":"user","content":"hi"}]}`),
		Metadata:        map[string]any{},
	}
}

func TestSessionAffinitySelectorOnResultOverloadPreservesExplicitBinding(t *testing.T) {
	t.Parallel()

	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	defer selector.Stop()

	auths := []*Auth{{ID: "auth-a"}, {ID: "auth-b"}}
	opts := claudeAffinityTestOptions()

	picked, errPick := selector.Pick(context.Background(), "claude", "claude-3-7-sonnet", opts, auths)
	if errPick != nil || picked == nil {
		t.Fatalf("Pick() error = %v", errPick)
	}
	if picked.ID != "auth-a" {
		t.Fatalf("Pick() = %q, want auth-a", picked.ID)
	}

	homeKey := "claude::claude:sess-1"
	modelKey := homeKey + "::claude-3-7-sonnet"
	for _, key := range []string{homeKey, modelKey} {
		if bound, ok := selector.cache.Get(key); !ok || bound != "auth-a" {
			t.Fatalf("precondition: cache[%q] = %q, %v; want auth-a, true", key, bound, ok)
		}
	}

	selector.OnResult(Result{
		AuthID:   "auth-a",
		Provider: "claude",
		Model:    "claude-3-7-sonnet",
		Error:    &Error{HTTPStatus: http.StatusServiceUnavailable, Message: overloadUpstreamBody},
		Options:  opts,
	})

	for _, key := range []string{homeKey, modelKey} {
		if bound, ok := selector.cache.Get(key); !ok || bound != "auth-a" {
			t.Fatalf("overload OnResult released cache[%q] = %q, %v; want auth-a, true", key, bound, ok)
		}
	}

	next, errNext := selector.Pick(context.Background(), "claude", "claude-3-7-sonnet", claudeAffinityTestOptions(), auths)
	if errNext != nil || next == nil {
		t.Fatalf("next Pick() error = %v", errNext)
	}
	if next.ID != "auth-a" {
		t.Fatalf("next Pick() = %q, want auth-a (binding preserved across overload)", next.ID)
	}
}

func TestSessionAffinitySelectorOnResultNonOverloadFailureReleasesExplicitBinding(t *testing.T) {
	t.Parallel()

	failures := map[string]*Error{
		"401 unauthorized": {HTTPStatus: http.StatusUnauthorized, Message: "invalid authentication credentials"},
		"generic 500":      {HTTPStatus: http.StatusInternalServerError, Message: "internal server error"},
	}
	for name, failure := range failures {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			selector := NewSessionAffinitySelector(&RoundRobinSelector{})
			defer selector.Stop()

			auths := []*Auth{{ID: "auth-a"}, {ID: "auth-b"}}
			opts := claudeAffinityTestOptions()

			picked, errPick := selector.Pick(context.Background(), "claude", "claude-3-7-sonnet", opts, auths)
			if errPick != nil || picked == nil {
				t.Fatalf("Pick() error = %v", errPick)
			}
			if picked.ID != "auth-a" {
				t.Fatalf("Pick() = %q, want auth-a", picked.ID)
			}

			selector.OnResult(Result{
				AuthID:   "auth-a",
				Provider: "claude",
				Model:    "claude-3-7-sonnet",
				Error:    failure,
				Options:  opts,
			})

			homeKey := "claude::claude:sess-1"
			modelKey := homeKey + "::claude-3-7-sonnet"
			for _, key := range []string{homeKey, modelKey} {
				if bound, ok := selector.cache.Get(key); ok {
					t.Fatalf("failure OnResult kept cache[%q] = %q; want released", key, bound)
				}
			}

			next, errNext := selector.Pick(context.Background(), "claude", "claude-3-7-sonnet", claudeAffinityTestOptions(), auths)
			if errNext != nil || next == nil {
				t.Fatalf("next Pick() error = %v", errNext)
			}
			if next.ID != "auth-b" {
				t.Fatalf("next Pick() = %q, want auth-b after binding release", next.ID)
			}
		})
	}
}

func TestSessionAffinitySelectorOnResultOverloadPreservesLCPBinding(t *testing.T) {
	t.Parallel()

	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &RoundRobinSelector{},
		TTL:      time.Minute,
	})
	defer selector.Stop()

	auths := []*Auth{{ID: "auth-a"}, {ID: "auth-b"}}
	opts := cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatOpenAI,
		OriginalRequest: []byte(`{"messages":[{"role":"user","content":"overload keeps LCP"}]}`),
		Metadata: map[string]any{
			cliproxyexecutor.CallerScopeMetadataKey: "caller-a",
		},
	}
	first, errFirst := selector.Pick(context.Background(), "openai", "model", opts, auths)
	if errFirst != nil {
		t.Fatalf("first Pick() error = %v", errFirst)
	}
	if first.ID != "auth-a" {
		t.Fatalf("first Pick() = %q, want auth-a", first.ID)
	}

	selector.OnResult(Result{
		AuthID:   first.ID,
		Provider: "openai",
		Model:    "model",
		Error:    &Error{HTTPStatus: http.StatusServiceUnavailable, Message: overloadUpstreamBody},
		Options:  opts,
	})

	next, errNext := selector.Pick(context.Background(), "openai", "model", opts, auths)
	if errNext != nil {
		t.Fatalf("next Pick() error = %v", errNext)
	}
	if next.ID != "auth-a" {
		t.Fatalf("next Pick() = %q, want auth-a (LCP binding preserved across overload)", next.ID)
	}
}

// The production overload flow: the home credential fails with an overload
// rejection, the conductor cooldown marks it unavailable, the session temp
// fails over, and once the home recovers the session migrates back.
func TestSessionAffinitySelectorOverloadCooldownFailoverAndHomeRecovery(t *testing.T) {
	t.Parallel()

	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	defer selector.Stop()

	healthyA := &Auth{ID: "auth-a"}
	authB := &Auth{ID: "auth-b"}
	coolingA := &Auth{ID: "auth-a", Unavailable: true, NextRetryAfter: time.Now().Add(time.Hour)}

	pick := func(candidates ...*Auth) string {
		t.Helper()
		auth, errPick := selector.Pick(context.Background(), "claude", "claude-3-7-sonnet", claudeAffinityTestOptions(), candidates)
		if errPick != nil || auth == nil {
			t.Fatalf("Pick() error = %v", errPick)
		}
		return auth.ID
	}

	if got := pick(healthyA, authB); got != "auth-a" {
		t.Fatalf("cold Pick() = %q, want auth-a", got)
	}

	selector.OnResult(Result{
		AuthID:   "auth-a",
		Provider: "claude",
		Model:    "claude-3-7-sonnet",
		Error:    &Error{HTTPStatus: http.StatusServiceUnavailable, Message: overloadUpstreamBody},
		Options:  claudeAffinityTestOptions(),
	})

	// The binding survives the overload result, but the conductor cooldown
	// keeps auth-a out of the candidate set, so the session temp-fails over.
	if got := pick(coolingA, authB); got != "auth-b" {
		t.Fatalf("Pick() with cooling home = %q, want auth-b temporary fallback", got)
	}
	if got := pick(coolingA, authB); got != "auth-b" {
		t.Fatalf("Pick() with cooling home and warm fallback cache = %q, want auth-b", got)
	}

	// Recovery: the preserved home binding routes the session back to auth-a.
	if got := pick(healthyA, authB); got != "auth-a" {
		t.Fatalf("Pick() after home recovery = %q, want auth-a (home_recovered)", got)
	}
	if got := pick(healthyA, authB); got != "auth-a" {
		t.Fatalf("Pick() after home recovery = %q, want stable auth-a", got)
	}
}
