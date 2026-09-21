package helps

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

// turnStateTestBlob returns a fake blob-shaped NORMAL header value (Fernet
// tokens start with "gAAAAA") of exactly the normal length.
func turnStateTestBlob() string {
	return "gAAAAA" + strings.Repeat("b", turnStateNormalLength-len("gAAAAA"))
}

func TestDecideTurnStateInjectionMatrix(t *testing.T) {
	base := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	normalClient := strings.Repeat("n", turnStateNormalLength)
	degradedClient := strings.Repeat("d", turnStateDegradedLength)
	unknownClient := strings.Repeat("u", 100)

	cases := []struct {
		name        string
		seedNormal  bool // store a fresh NORMAL ticket before deciding
		clientValue string
		wantAction  string
		wantTicket  bool
	}{
		{"client normal keeps client state, ticket available", true, normalClient, TurnStateActionKeepClient, false},
		{"client normal keeps client state, no ticket", false, normalClient, TurnStateActionKeepClient, false},
		{"client degraded replaced with stored ticket", true, degradedClient, TurnStateActionReplace, true},
		{"client degraded without ticket keeps client", false, degradedClient, TurnStateActionKeepClient, false},
		{"client absent injects stored ticket", true, "", TurnStateActionInject, true},
		{"client absent without ticket passes", false, "", TurnStateActionPassNoTicket, false},
		{"client unknown length never touched, ticket available", true, unknownClient, TurnStateActionKeepUnknown, false},
		{"client unknown length never touched, no ticket", false, unknownClient, TurnStateActionKeepUnknown, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetTurnStateStoreForTest(t, base)
			blob := turnStateTestBlob()
			if tc.seedNormal {
				ObserveTurnState("auth-inject.json", "gpt-5.5", blob)
			}
			for _, dryRun := range []bool{true, false} {
				action, ticket := DecideTurnStateInjection("auth-inject.json", "gpt-5.5", tc.clientValue, dryRun)
				if action != tc.wantAction {
					t.Errorf("dryRun=%v: action = %q, want %q", dryRun, action, tc.wantAction)
				}
				if tc.wantTicket {
					if ticket != blob {
						t.Errorf("dryRun=%v: ticket = %q, want the stored blob", dryRun, ticket)
					}
				} else if ticket != "" {
					t.Errorf("dryRun=%v: ticket = %q, want empty", dryRun, ticket)
				}
			}
		})
	}
}

func TestDecideTurnStateInjectionTTLMargin(t *testing.T) {
	base := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	degradedClient := strings.Repeat("d", turnStateDegradedLength)

	cases := []struct {
		name           string
		age            time.Duration
		wantAction     string
		wantTicketKept bool
	}{
		// Injectable iff remaining TTL (1h - age) exceeds the 10m margin,
		// i.e. age < 50m.
		{"fresh ticket injectable", 45 * time.Minute, TurnStateActionReplace, true},
		{"remaining exactly at margin not injectable", 50 * time.Minute, TurnStateActionKeepClient, false},
		{"remaining below margin not injectable", 51 * time.Minute, TurnStateActionKeepClient, false},
		{"expired ticket not injectable", 61 * time.Minute, TurnStateActionKeepClient, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetTurnStateStoreForTest(t, base)
			ObserveTurnState("auth-ttl.json", "gpt-5.5", turnStateTestBlob())
			turnStateNowFunc = func() time.Time { return base.Add(tc.age) }

			action, ticket := DecideTurnStateInjection("auth-ttl.json", "gpt-5.5", degradedClient, false)
			if action != tc.wantAction {
				t.Errorf("action = %q, want %q", action, tc.wantAction)
			}
			if tc.wantTicketKept && ticket == "" {
				t.Error("expected the stored ticket to be returned")
			}
			if !tc.wantTicketKept && ticket != "" {
				t.Errorf("ticket = %q, want empty", ticket)
			}
		})
	}
}

func TestObserveTurnStateNonNormalNeverTouchesStoredTicket(t *testing.T) {
	base := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	resetTurnStateStoreForTest(t, base)

	blob := turnStateTestBlob()
	ObserveTurnState("auth-keep.json", "gpt-5.5", blob)
	// Degraded, absent, and other observations must not replace or extend the
	// stored NORMAL ticket.
	turnStateNowFunc = func() time.Time { return base.Add(30 * time.Minute) }
	ObserveTurnState("auth-keep.json", "gpt-5.5", strings.Repeat("d", turnStateDegradedLength))
	ObserveTurnState("auth-keep.json", "gpt-5.5", "")
	ObserveTurnState("auth-keep.json", "gpt-5.5", strings.Repeat("u", 100))

	ticket, ok := lookupInjectableTurnState("auth-keep.json", "gpt-5.5", base.Add(30*time.Minute))
	if !ok || ticket != blob {
		t.Fatalf("stored ticket = %q, injectable=%v; want original blob, injectable", ticket, ok)
	}
	// The capture time belongs to the NORMAL observation, so the ticket stops
	// being injectable once its own TTL margin is exceeded.
	_, ok = lookupInjectableTurnState("auth-keep.json", "gpt-5.5", base.Add(55*time.Minute))
	if ok {
		t.Fatal("ticket still injectable 55m after the NORMAL capture; non-normal observations must not extend it")
	}
}

func TestApplyTurnStateInjectionDecisionMutation(t *testing.T) {
	ticket := turnStateTestBlob()
	degradedClient := strings.Repeat("d", turnStateDegradedLength)

	t.Run("dry-run never mutates", func(t *testing.T) {
		header := http.Header{}
		ApplyTurnStateInjectionDecision(header, TurnStateActionInject, ticket, true)
		if got := header.Get(CodexTurnStateHeader); got != "" {
			t.Errorf("dry-run inject set header to %q", got)
		}
		header.Set(CodexTurnStateHeader, degradedClient)
		ApplyTurnStateInjectionDecision(header, TurnStateActionReplace, ticket, true)
		if got := header.Get(CodexTurnStateHeader); got != degradedClient {
			t.Errorf("dry-run replace changed header to %q", got)
		}
	})

	t.Run("enforce sets header only for inject and replace", func(t *testing.T) {
		for _, action := range []string{TurnStateActionKeepClient, TurnStateActionPassNoTicket, TurnStateActionKeepUnknown} {
			header := http.Header{}
			header.Set(CodexTurnStateHeader, degradedClient)
			ApplyTurnStateInjectionDecision(header, action, ticket, false)
			if got := header.Get(CodexTurnStateHeader); got != degradedClient {
				t.Errorf("enforce %s changed header to %q", action, got)
			}
		}

		header := http.Header{}
		ApplyTurnStateInjectionDecision(header, TurnStateActionInject, ticket, false)
		if got := header.Get(CodexTurnStateHeader); got != ticket {
			t.Errorf("enforce inject header = %q, want the stored ticket", got)
		}

		header.Set(CodexTurnStateHeader, degradedClient)
		ApplyTurnStateInjectionDecision(header, TurnStateActionReplace, ticket, false)
		if got := header.Get(CodexTurnStateHeader); got != ticket {
			t.Errorf("enforce replace header = %q, want the stored ticket", got)
		}
	})

	t.Run("empty ticket never mutates", func(t *testing.T) {
		header := http.Header{}
		ApplyTurnStateInjectionDecision(header, TurnStateActionInject, "", false)
		if got := header.Get(CodexTurnStateHeader); got != "" {
			t.Errorf("empty ticket set header to %q", got)
		}
	})
}

// TestTurnStateInjectionNeverLeaksBlob pins the safety invariant for the
// injection path: the stored NORMAL blob appears only in the Decide ticket
// return value and the outbound request header - never in the snapshot JSON
// and never in decision log lines.
func TestTurnStateInjectionNeverLeaksBlob(t *testing.T) {
	base := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	resetTurnStateStoreForTest(t, base)

	blob := turnStateTestBlob()
	clientValue := strings.Repeat("c", turnStateDegradedLength)
	ObserveTurnState("auth-leak.json", "gpt-5.5", blob)

	// The store retains the blob (same-package check), the snapshot copy does not.
	turnStatesMu.RLock()
	stored := turnStates["auth-leak.json|gpt-5.5"].normalValue
	turnStatesMu.RUnlock()
	if stored != blob {
		t.Fatal("store must retain the NORMAL blob as the injection ticket")
	}
	snapshot := SnapshotTurnStates()
	if snapshot.Tickets[0].normalValue != "" {
		t.Fatal("snapshot copy must zero the stored blob")
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if strings.Contains(string(raw), "gAAAAA") {
		t.Fatalf("snapshot JSON leaks the turn-state blob: %s", raw)
	}

	var logBuf bytes.Buffer
	prevOut := log.StandardLogger().Out
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(prevOut) })

	action, ticket := DecideTurnStateInjection("auth-leak.json", "gpt-5.5", clientValue, true)
	if action != TurnStateActionReplace || ticket != blob {
		t.Fatalf("decision = %q with ticket leak-guard fixture, want replace with the stored blob", action)
	}
	logLine := logBuf.String()
	if strings.Contains(logLine, "gAAAAA") || strings.Contains(logLine, clientValue) {
		t.Fatalf("decision log leaks blob or client header value: %s", logLine)
	}
	for _, field := range []string{`auth_id=auth-leak.json`, `model=gpt-5.5`, `action=replace`, `dry_run=true`} {
		if !strings.Contains(logLine, field) {
			t.Errorf("decision log missing %q: %s", field, logLine)
		}
	}
}
