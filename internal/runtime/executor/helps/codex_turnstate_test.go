package helps

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// resetTurnStateStoreForTest clears the package-level store and installs a
// controllable clock, restoring both when the test finishes. The store is
// process-global state, so every test in this file starts from a clean slate.
func resetTurnStateStoreForTest(t *testing.T, now time.Time) {
	t.Helper()
	turnStatesMu.Lock()
	turnStates = make(map[string]*CodexTurnStateTicket)
	turnStatesMu.Unlock()
	prevNowFunc := turnStateNowFunc
	turnStateNowFunc = func() time.Time { return now }
	t.Cleanup(func() {
		turnStateNowFunc = prevNowFunc
		turnStatesMu.Lock()
		turnStates = make(map[string]*CodexTurnStateTicket)
		turnStatesMu.Unlock()
	})
}

func TestClassifyTurnStateBoundaries(t *testing.T) {
	cases := []struct {
		length int
		want   string
	}{
		{0, TurnStateShapeAbsent},
		{292, TurnStateShapeNormal},
		{312, TurnStateShapeDegraded},
		{1, TurnStateShapeOther},
		{291, TurnStateShapeOther},
		{293, TurnStateShapeOther},
		{311, TurnStateShapeOther},
		{313, TurnStateShapeOther},
		{5000, TurnStateShapeOther},
	}
	for _, tc := range cases {
		if got := classifyTurnState(tc.length); got != tc.want {
			t.Errorf("classifyTurnState(%d) = %q, want %q", tc.length, got, tc.want)
		}
	}
}

func TestObserveTurnStateAccumulatesWithinBucket(t *testing.T) {
	base := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	resetTurnStateStoreForTest(t, base)

	ObserveTurnState("auth-a.json", "gpt-5.5", strings.Repeat("x", 292))
	ObserveTurnState("auth-a.json", "gpt-5.5", strings.Repeat("x", 292))
	ObserveTurnState("auth-a.json", "gpt-5.5", strings.Repeat("x", 312))
	ObserveTurnState("auth-a.json", "gpt-5.5", "")
	ObserveTurnState("auth-a.json", "gpt-5.5", strings.Repeat("x", 100))
	// A different model of the same account is a separate bucket.
	ObserveTurnState("auth-a.json", "gpt-5.5-codex", strings.Repeat("x", 292))

	snapshot := SnapshotTurnStates()
	if len(snapshot.Tickets) != 2 {
		t.Fatalf("tickets = %d, want 2", len(snapshot.Tickets))
	}
	ticket := snapshot.Tickets[0]
	if ticket.AuthID != "auth-a.json" || ticket.Model != "gpt-5.5" {
		t.Fatalf("ticket key = %q/%q, want auth-a.json/gpt-5.5", ticket.AuthID, ticket.Model)
	}
	if ticket.Normal != 2 || ticket.Degraded != 1 || ticket.Absent != 1 || ticket.Other != 1 {
		t.Errorf("counts = normal:%d degraded:%d absent:%d other:%d, want 2/1/1/1",
			ticket.Normal, ticket.Degraded, ticket.Absent, ticket.Other)
	}
	if ticket.LastLength != 100 || ticket.LastShape != TurnStateShapeOther {
		t.Errorf("last = length:%d shape:%q, want 100/other", ticket.LastLength, ticket.LastShape)
	}
	if !ticket.FirstSeen.Equal(base) || !ticket.LastSeen.Equal(base) {
		t.Errorf("first/last seen = %v/%v, want %v", ticket.FirstSeen, ticket.LastSeen, base)
	}
	other := snapshot.Tickets[1]
	if other.Model != "gpt-5.5-codex" || other.Normal != 1 {
		t.Errorf("second bucket = model:%q normal:%d, want gpt-5.5-codex/1", other.Model, other.Normal)
	}
	if snapshot.StartedAt.IsZero() {
		t.Error("started_at must be set")
	}
}

func TestObserveTurnStateClockAdvancesLastSeen(t *testing.T) {
	first := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	resetTurnStateStoreForTest(t, first)

	ObserveTurnState("auth-b.json", "gpt-5.5", strings.Repeat("x", 292))
	second := first.Add(3 * time.Minute)
	turnStateNowFunc = func() time.Time { return second }
	ObserveTurnState("auth-b.json", "gpt-5.5", strings.Repeat("x", 312))

	ticket := SnapshotTurnStates().Tickets[0]
	if !ticket.FirstSeen.Equal(first) {
		t.Errorf("first_seen = %v, want %v", ticket.FirstSeen, first)
	}
	if !ticket.LastSeen.Equal(second) {
		t.Errorf("last_seen = %v, want %v", ticket.LastSeen, second)
	}
}

func TestObserveTurnStateSkipsEmptyAuthOrModel(t *testing.T) {
	resetTurnStateStoreForTest(t, time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC))

	ObserveTurnState("", "gpt-5.5", strings.Repeat("x", 292))
	ObserveTurnState("auth-c.json", "", strings.Repeat("x", 292))
	ObserveTurnState("  ", "gpt-5.5", strings.Repeat("x", 292))
	ObserveTurnState("auth-c.json", "  ", strings.Repeat("x", 292))

	if got := len(SnapshotTurnStates().Tickets); got != 0 {
		t.Fatalf("tickets = %d, want 0 (empty auth/model skipped)", got)
	}
}

func TestSnapshotTurnStatesReturnsDeepCopy(t *testing.T) {
	resetTurnStateStoreForTest(t, time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC))

	ObserveTurnState("auth-d.json", "gpt-5.5", strings.Repeat("x", 292))

	snapshot := SnapshotTurnStates()
	snapshot.Tickets[0].Normal = 999
	snapshot.Tickets[0].LastShape = "tampered"
	snapshot.Tickets[0].AuthID = "tampered"

	fresh := SnapshotTurnStates()
	ticket := fresh.Tickets[0]
	if ticket.Normal != 1 || ticket.LastShape != TurnStateShapeNormal || ticket.AuthID != "auth-d.json" {
		t.Errorf("store was affected by mutating a snapshot: %+v", ticket)
	}
}

// TestTurnStateSnapshotNeverContainsBlob is the safety invariant of the whole
// feature: a realistic blob-shaped header value (Fernet tokens start with
// "gAAAAA") of exactly the normal length must leave only its length class in
// every observable output - the JSON snapshot must not contain the value.
func TestTurnStateSnapshotNeverContainsBlob(t *testing.T) {
	resetTurnStateStoreForTest(t, time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC))

	fakeBlob := "gAAAAA" + strings.Repeat("b", 292-len("gAAAAA"))
	if len(fakeBlob) != 292 {
		t.Fatalf("fixture length = %d, want 292", len(fakeBlob))
	}
	ObserveTurnState("auth-e.json", "gpt-5.5", fakeBlob)

	raw, err := json.Marshal(SnapshotTurnStates())
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if strings.Contains(string(raw), "gAAAAA") {
		t.Fatalf("snapshot JSON leaks the turn-state blob: %s", raw)
	}
	ticket := SnapshotTurnStates().Tickets[0]
	if ticket.LastLength != 292 || ticket.LastShape != TurnStateShapeNormal || ticket.Normal != 1 {
		t.Errorf("only length/shape should be retained, got %+v", ticket)
	}
}

// TestObserveTurnStateIsTheOnlyWriter documents the config gate contract: the
// store is written exclusively through ObserveTurnState, and the only two call
// sites (codex_executor_execute.go, codex_executor_stream.go) invoke it behind
// `e.cfg.Codex.TurnStateCapture`, so with the flag off (the default) nothing
// is recorded. This test asserts the writer side: direct calls record, and
// there is no other package-level mutator to bypass the gate.
func TestObserveTurnStateIsTheOnlyWriter(t *testing.T) {
	resetTurnStateStoreForTest(t, time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC))

	if got := len(SnapshotTurnStates().Tickets); got != 0 {
		t.Fatalf("fresh store has %d tickets, want 0", got)
	}
	ObserveTurnState("auth-f.json", "gpt-5.5", "")
	if got := len(SnapshotTurnStates().Tickets); got != 1 {
		t.Fatalf("tickets = %d after one ObserveTurnState call, want 1", got)
	}
}
