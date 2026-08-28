package metrics

import (
	"hash/fnv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// dailySessionPerAccountCap bounds the per-account distinct-session set. The
// alert threshold for shared-usage review is a few hundred sessions per day,
// so saturating well above it keeps the signal usable while bounding memory:
// 4096 64-bit hashes cost ~32KB per account even under abuse.
const dailySessionPerAccountCap = 4096

// AccountDailyDistinctSessions reports how many distinct logical sessions an
// account has served in the current UTC day. It is the "shared usage pattern"
// observability signal: one account serving an implausible number of distinct
// sessions per day looks like gateway resale rather than a personal
// subscription. State is in-memory only and resets at UTC midnight; a process
// restart restarts the day count, which is acceptable for a same-day signal.
var AccountDailyDistinctSessions = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Name: "cliproxy_account_daily_distinct_sessions",
	Help: "Distinct logical sessions served by this account in the current UTC day (resets at UTC midnight and on restart).",
}, []string{"auth_id"})

// dailyDistinctSessionTracker records which logical sessions each account has
// served within the current UTC day. Session IDs are stored as 64-bit FNV-1a
// hashes so memory stays bounded regardless of session ID length; the
// per-account cap saturates instead of growing without bound. Day rollover is
// lazy: the first observation of a new UTC day drops all previous state and
// reports the auth IDs that held it, so their exported gauge series can be
// deleted instead of going stale.
type dailyDistinctSessionTracker struct {
	mu    sync.Mutex
	day   string // current UTC day, YYYY-MM-DD; empty until first observation
	auths map[string]map[uint64]struct{}
}

func newDailyDistinctSessionTracker() *dailyDistinctSessionTracker {
	return &dailyDistinctSessionTracker{auths: make(map[string]map[uint64]struct{})}
}

func hashDailySessionID(sessionID string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(sessionID))
	return h.Sum64()
}

// observe records that authID served sessionID in the UTC day containing now
// and returns the account's distinct session count for that day. On UTC day
// rollover it returns the auth IDs whose previous-day state was dropped.
func (t *dailyDistinctSessionTracker) observe(authID, sessionID string, now time.Time) (count int, reset []string) {
	if authID == "" || sessionID == "" {
		return 0, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	day := now.UTC().Format("2006-01-02")
	if day != t.day {
		if len(t.auths) > 0 {
			reset = make([]string, 0, len(t.auths))
			for staleAuthID := range t.auths {
				reset = append(reset, staleAuthID)
			}
		}
		t.auths = make(map[string]map[uint64]struct{})
		t.day = day
	}
	set, ok := t.auths[authID]
	if !ok {
		set = make(map[uint64]struct{})
		t.auths[authID] = set
	}
	hash := hashDailySessionID(sessionID)
	if _, exists := set[hash]; !exists && len(set) < dailySessionPerAccountCap {
		set[hash] = struct{}{}
	}
	return len(set), reset
}

var accountDailySessions = newDailyDistinctSessionTracker()

// ObserveAccountDailySession records that authID served sessionID and refreshes
// the per-account daily gauge. The session-affinity pick path is the intended
// caller: it is where (account, session) pairs are resolved.
func ObserveAccountDailySession(authID, sessionID string) {
	observeAccountDailySessionAt(authID, sessionID, time.Now())
}

// observeAccountDailySessionAt is ObserveAccountDailySession with an injectable
// clock so tests can exercise UTC day rollover deterministically. On rollover,
// series for accounts with no fresh observation are deleted so the gauge never
// reports a stale previous-day count.
func observeAccountDailySessionAt(authID, sessionID string, now time.Time) {
	if authID == "" || sessionID == "" {
		return
	}
	count, reset := accountDailySessions.observe(authID, sessionID, now)
	for _, staleAuthID := range reset {
		if staleAuthID != authID {
			AccountDailyDistinctSessions.DeleteLabelValues(staleAuthID)
		}
	}
	AccountDailyDistinctSessions.WithLabelValues(authID).Set(float64(count))
}
