package metrics

import (
	"strconv"
	"testing"
	"time"
)

func TestDailyDistinctSessionTrackerCountsDistinctSessions(t *testing.T) {
	tracker := newDailyDistinctSessionTracker()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

	if count, _ := tracker.observe("auth-a", "s1", now); count != 1 {
		t.Fatalf("first session count = %d, want 1", count)
	}
	if count, _ := tracker.observe("auth-a", "s1", now); count != 1 {
		t.Fatalf("repeat session count = %d, want 1", count)
	}
	if count, _ := tracker.observe("auth-a", "s2", now); count != 2 {
		t.Fatalf("second distinct session count = %d, want 2", count)
	}
	if count, _ := tracker.observe("auth-b", "s1", now); count != 1 {
		t.Fatalf("other account must count independently, got %d", count)
	}
	if count, _ := tracker.observe("", "s1", now); count != 0 {
		t.Fatalf("empty auth ID must be ignored, got %d", count)
	}
	if count, _ := tracker.observe("auth-a", "", now); count != 0 {
		t.Fatalf("empty session ID must be ignored, got %d", count)
	}
}

func TestDailyDistinctSessionTrackerUTCDayRollover(t *testing.T) {
	tracker := newDailyDistinctSessionTracker()
	// 2026-08-29 07:59:59 +08:00 is still 2026-08-28 in UTC.
	lateDayOne := time.Date(2026, 8, 29, 7, 59, 59, 0, time.FixedZone("HKT", 8*3600))
	// 2026-08-29 08:00:01 +08:00 is 2026-08-29 00:00:01 UTC.
	earlyDayTwo := time.Date(2026, 8, 29, 8, 0, 1, 0, time.FixedZone("HKT", 8*3600))

	if count, _ := tracker.observe("auth-a", "s1", lateDayOne); count != 1 {
		t.Fatalf("day one count = %d, want 1", count)
	}
	count, reset := tracker.observe("auth-b", "s1", earlyDayTwo)
	if count != 1 {
		t.Fatalf("day two count = %d, want 1", count)
	}
	if len(reset) != 1 || reset[0] != "auth-a" {
		t.Fatalf("rollover must report dropped auth IDs, got %v", reset)
	}
	// The previous day's sessions do not carry over: s1 counts as new for auth-a.
	if count, _ := tracker.observe("auth-a", "s1", earlyDayTwo); count != 1 {
		t.Fatalf("post-rollover count = %d, want 1", count)
	}
	// A later rollover drops every account that accumulated state since.
	dayThree := earlyDayTwo.Add(24 * time.Hour)
	if _, reset := tracker.observe("auth-a", "s1", dayThree); len(reset) != 2 {
		t.Fatalf("rollover must drop all auths with state, got %v", reset)
	}
}

func TestDailyDistinctSessionTrackerCapSaturates(t *testing.T) {
	tracker := newDailyDistinctSessionTracker()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	for i := 0; i < dailySessionPerAccountCap+100; i++ {
		tracker.observe("auth-cap", "s"+strconv.Itoa(i), now)
	}
	if count, _ := tracker.observe("auth-cap", "overflow", now); count != dailySessionPerAccountCap {
		t.Fatalf("capped count = %d, want %d", count, dailySessionPerAccountCap)
	}
}

// Gauge values come straight from the tracker count, so these tests assert
// series lifecycle only; DeleteLabelValues reports whether the series existed,
// which doubles as an existence probe that leaves no extra imports behind.
func TestObserveAccountDailySessionCreatesSeries(t *testing.T) {
	ObserveAccountDailySession("auth-daily-gauge", "s1")
	ObserveAccountDailySession("auth-daily-gauge", "s1")
	ObserveAccountDailySession("auth-daily-gauge", "s2")
	if !AccountDailyDistinctSessions.DeleteLabelValues("auth-daily-gauge") {
		t.Fatal("expected gauge series for the observed account")
	}
}

func TestObserveAccountDailySessionRolloverDeletesStaleSeries(t *testing.T) {
	dayOne := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	dayTwo := time.Date(2026, 8, 28, 0, 0, 1, 0, time.UTC)
	observeAccountDailySessionAt("auth-stale-series", "s1", dayOne)
	observeAccountDailySessionAt("auth-fresh-series", "s1", dayTwo)
	if AccountDailyDistinctSessions.DeleteLabelValues("auth-stale-series") {
		t.Error("stale previous-day series must be deleted on rollover")
	}
	if !AccountDailyDistinctSessions.DeleteLabelValues("auth-fresh-series") {
		t.Error("expected series for auth-fresh-series after rollover")
	}
}
