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
		// xAI answers a revoked refresh token with 400 invalid_grant, so the
		// status code alone never marks the credential unavailable.
		{name: "oauth invalid_grant on 400", err: errors.New(`xai token request failed with status 400: {"error":"invalid_grant","error_description":"refresh token is invalid"}`), want: true},
		{name: "oauth invalid_token", err: errors.New(`token request failed: {"error":"invalid_token"}`), want: true},
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
