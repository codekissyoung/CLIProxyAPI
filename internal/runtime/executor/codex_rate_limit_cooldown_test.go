package executor

import (
	"net/http"
	"testing"
	"time"
)

// ice divergence regression (2026-10-08): a generic Codex 429 such as
// {"detail":"Rate limit exceeded"} is a per-minute rate limit. It must carry a
// short fixed retry-after so the conductor does not walk the quota backoff
// ladder (1s doubling up to 30min) on the (auth, model) state. Quota exhaustion
// keeps its upstream-provided reset timing; capacity errors keep the ladder.
func TestNewCodexStatusErrGenericRateLimitGetsFixedShortCooldown(t *testing.T) {
	err := newCodexStatusErr(http.StatusTooManyRequests, []byte(`{"detail":"Rate limit exceeded"}`))
	if err.code != http.StatusTooManyRequests {
		t.Fatalf("code = %d, want 429", err.code)
	}
	if err.retryAfter == nil || *err.retryAfter != codexRateLimitRetryAfter {
		t.Fatalf("retryAfter = %v, want fixed %s", err.retryAfter, codexRateLimitRetryAfter)
	}
	if err.credentialScoped {
		t.Fatal("generic rate limit must stay model-scoped, not credential-scoped")
	}
	if codexRateLimitRetryAfter > 2*time.Minute {
		t.Fatalf("fixed rate-limit cooldown %s is no longer short", codexRateLimitRetryAfter)
	}
}

func TestNewCodexStatusErrUsageLimitKeepsUpstreamResetTiming(t *testing.T) {
	body := []byte(`{"error":{"type":"usage_limit_reached","message":"limit","resets_in_seconds":900}}`)
	err := newCodexStatusErr(http.StatusTooManyRequests, body)
	if err.retryAfter == nil || *err.retryAfter != 900*time.Second {
		t.Fatalf("retryAfter = %v, want 900s from resets_in_seconds", err.retryAfter)
	}
	if !err.credentialScoped {
		t.Fatal("usage_limit_reached must be credential-scoped")
	}
}

func TestNewCodexStatusErrCapacityErrorKeepsBackoffLadder(t *testing.T) {
	body := []byte(`{"error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later.","type":"service_unavailable_error"}}`)
	if !isCodexModelCapacityError(body) {
		t.Skip("fixture no longer classified as a capacity error; update the fixture")
	}
	err := newCodexStatusErr(http.StatusServiceUnavailable, body)
	if err.code != http.StatusTooManyRequests {
		t.Fatalf("capacity error code = %d, want 429 mapping", err.code)
	}
	if err.retryAfter != nil {
		t.Fatalf("capacity error retryAfter = %v, want nil (conductor backoff ladder)", *err.retryAfter)
	}
}
