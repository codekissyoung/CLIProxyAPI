package auth

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/metrics"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// The session-affinity pick path is where (account, session) pairs are
// resolved, so every successful pick with a session must feed the account's
// daily distinct-session gauge; sessionless picks must not create series.
func TestSessionAffinitySelector_RecordsDailyDistinctSessions(t *testing.T) {
	t.Parallel()

	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	defer selector.Stop()

	auths := []*Auth{{ID: "auth-daily-spread-a"}}
	opts := cliproxyexecutor.Options{OriginalRequest: []byte(`{"metadata":{"user_id":"user_xxx_account__session_daily-spread"}}`)}

	picked, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if picked == nil || picked.ID != "auth-daily-spread-a" {
		t.Fatalf("Pick() auth = %v, want auth-daily-spread-a", picked)
	}
	if !metrics.AccountDailyDistinctSessions.DeleteLabelValues("auth-daily-spread-a") {
		t.Fatal("Pick with a session must record the account daily distinct-session gauge")
	}

	if _, err = selector.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths); err != nil {
		t.Fatalf("sessionless Pick() error = %v", err)
	}
	if metrics.AccountDailyDistinctSessions.DeleteLabelValues("auth-daily-spread-a") {
		t.Fatal("sessionless Pick must not create a daily distinct-session series")
	}
}
