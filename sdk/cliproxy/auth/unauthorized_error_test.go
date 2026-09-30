package auth

import (
	"errors"
	"testing"
)

func TestIsUnauthorizedError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "status 401", err: errors.New("codex token request failed with status 401: {}"), want: true},
		{name: "401 unauthorized", err: errors.New("401 Unauthorized"), want: true},
		// 2026-09-30 merge: invalid_grant is NOT unauthorized any more. xAI still
		// answers a revoked refresh token with 400 invalid_grant, but upstream now
		// owns that case in its own path (isInvalidGrantError + exponential
		// backoff for enabled credentials, unschedule for disabled ones). Widening
		// isUnauthorizedError to cover it short-circuited that path and zeroed
		// RefreshFailures — see TestRefreshAuthForRequest_EnabledAuth_InvalidGrant_ExponentialBackoff.
		{name: "oauth invalid_grant on 400 handled by the invalid_grant path", err: errors.New(`xai token request failed with status 400: {"error":"invalid_grant","error_description":"refresh token is invalid"}`), want: false},
		{name: "oauth invalid_token", err: errors.New(`token request failed: {"error":"invalid_token"}`), want: false},
		{name: "transient network error", err: errors.New("dial tcp: i/o timeout"), want: false},
		{name: "unrelated 400", err: errors.New(`token request failed with status 400: {"error":"invalid_request"}`), want: false},
		{name: "server error", err: errors.New("token request failed with status 500"), want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUnauthorizedError(tc.err); got != tc.want {
				t.Fatalf("isUnauthorizedError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
