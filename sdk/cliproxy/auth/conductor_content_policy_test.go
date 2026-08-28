package auth

import (
	"context"
	"testing"
)

// Content-policy (moderation/safety) refusals are request-content outcomes, not
// credential health signals. They must be classified as request-invalid so the
// conductor neither fans the same violating payload out across every pooled
// account nor marks the serving account unhealthy.
func TestIsRequestInvalidErrorContentPolicyRefusal(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "403 content_policy_violation code",
			err:  invalidParamStatusError{code: 403, msg: `{"error":{"code":"content_policy_violation","type":"invalid_request_error","message":"Rejected by policy."}}`},
			want: true,
		},
		{
			name: "403 moderation_blocked in detail object",
			err:  invalidParamStatusError{code: 403, msg: `{"detail":{"code":"moderation_blocked","message":"Blocked."}}`},
			want: true,
		},
		{
			name: "403 detail string marker",
			err:  invalidParamStatusError{code: 403, msg: `{"detail":"safety_violation"}`},
			want: true,
		},
		{
			name: "400 marker in error type",
			err:  invalidParamStatusError{code: 400, msg: `{"error":{"type":"safety_violation","message":"Blocked."}}`},
			want: true,
		},
		{
			name: "451 cyber_policy code",
			err:  invalidParamStatusError{code: 451, msg: `{"error":{"code":"cyber_policy","message":"Flagged."}}`},
			want: true,
		},
		{
			// Fork fe28d582: Codex reports the same cyber_policy rejection as
			// 502 through the websocket disconnect channel.
			name: "502 cyber_policy websocket disconnect shape",
			err:  invalidParamStatusError{code: 502, msg: `{"error":{"type":"invalid_request","code":"cyber_policy","message":"This content was flagged for possible cybersecurity risk."}}`},
			want: true,
		},
		{
			name: "502 quoted marker in non-JSON body",
			err:  invalidParamStatusError{code: 502, msg: `upstream error: "session_blocked_by_cyber_policy"`},
			want: true,
		},
		{
			name: "400 marker as exact message",
			err:  invalidParamStatusError{code: 400, msg: `{"error":{"message":"sensitive_words_detected"}}`},
			want: true,
		},
		{
			name: "conductor Error carrying marker code",
			err:  &Error{Code: "moderation_blocked", Message: "blocked", HTTPStatus: 403},
			want: true,
		},
		{
			name: "401 keeps credential semantics even with marker",
			err:  invalidParamStatusError{code: 401, msg: `{"error":{"code":"moderation_blocked"}}`},
			want: false,
		},
		{
			name: "402 keeps billing semantics even with marker",
			err:  invalidParamStatusError{code: 402, msg: `{"error":{"code":"content_policy_violation"}}`},
			want: false,
		},
		{
			name: "429 keeps quota semantics even with marker",
			err:  invalidParamStatusError{code: 429, msg: `{"error":{"code":"safety_violation"}}`},
			want: false,
		},
		{
			name: "403 without marker still rotates",
			err:  invalidParamStatusError{code: 403, msg: `{"error":{"code":"insufficient_quota","message":"payment required"}}`},
			want: false,
		},
		{
			name: "403 prose mentioning safety is not a refusal",
			err:  invalidParamStatusError{code: 403, msg: `{"error":{"message":"Your safety settings are not available on this plan"}}`},
			want: false,
		},
		{
			name: "500 without marker stays transient",
			err:  invalidParamStatusError{code: 500, msg: `{"error":{"message":"internal error"}}`},
			want: false,
		},
		{
			name: "missing status is not classifiable",
			err:  invalidParamStatusError{code: 0, msg: `{"error":{"code":"moderation_blocked"}}`},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRequestInvalidError(tc.err); got != tc.want {
				t.Errorf("isRequestInvalidError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// A content-policy refusal must be recorded as request-scoped so MarkResult
// neither cools the credential nor suspends the model.
func TestMarkResultContentPolicyRefusalSkipsCooldown(t *testing.T) {
	withQuotaCooldownEnabled(t)

	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-content-policy",
		Provider: "codex",
		Metadata: map[string]any{"type": "codex"},
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register returned error: %v", errRegister)
	}

	upstreamErr := invalidParamStatusError{code: 403, msg: `{"error":{"code":"content_policy_violation","type":"invalid_request_error","message":"Rejected by policy."}}`}
	resultErr := resultErrorFromError(upstreamErr)
	if resultErr == nil || resultErr.Code != requestScopedErrorCode {
		t.Fatalf("expected request-scoped result error, got %#v", resultErr)
	}
	if !shouldSkipCredentialCooldown(resultErr) {
		t.Fatalf("shouldSkipCredentialCooldown(%#v) = false, want true", resultErr)
	}

	manager.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: "codex", Model: "gpt-5", Success: false, Error: resultErr})

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("expected auth to remain registered")
	}
	if updated.Unavailable {
		t.Fatal("content-policy refusal must not mark the auth unavailable")
	}
	if !updated.NextRetryAfter.IsZero() {
		t.Fatalf("content-policy refusal must not cool the auth, got next_retry_after=%v", updated.NextRetryAfter)
	}
	if state := updated.ModelStates["gpt-5"]; state != nil && (state.Unavailable || !state.NextRetryAfter.IsZero()) {
		t.Fatalf("content-policy refusal must not cool the model state, got %#v", state)
	}
}
