// Package metrics exposes account-level counters and gauges for the auth pool
// (Codex/Claude/... OAuth token files under the auths dir). These exist so an
// operator can answer "how many times has this specific account been used,
// how many logical session homes does it retain, how often has it failed"
// without reconstructing the answer from rotated text logs by hand — see the
// 2026-07-15 charlie@twobird.site ban investigation in the downstream
// claude-relay-server repo for the motivating incident.
//
// auth_id is the same value already used in log lines (selector.go's
// "session-affinity: ... auth=%s" and logging_helpers.go's "auth_id=%s") —
// the OAuth token file name (e.g. "codex-charlie@twobird.site-pro.json"), not
// a synthetic identifier. Host/instance labeling is intentionally NOT done
// here: the downstream Alloy remote_write pipeline already stamps a "host"
// external label on every scraped series, so duplicating it here would be
// redundant and would multiply series cardinality for no benefit.
package metrics

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// AccountRequestsTotal counts every time an auth was successfully handed
	// back by the session-affinity selector to serve a request attempt,
	// regardless of whether that specific attempt later succeeded or failed
	// upstream (a retried-and-failed-over request still counts once for the
	// account it was first routed to).
	AccountRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cliproxy_account_requests_total",
		Help: "Cumulative number of times this account was selected to serve a request.",
	}, []string{"auth_id"})

	// AccountSessionsTotal counts new logical home-binding events. A binding
	// recreated after process restart or idle expiry is counted again, so this
	// counter is not an exact all-time distinct-session cardinality.
	AccountSessionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cliproxy_account_sessions_total",
		Help: "Cumulative number of new logical session home bindings created for this account.",
	}, []string{"auth_id"})

	// AccountActiveSessions reports current logical session home bindings. Unlike
	// AccountSessionsTotal this gauge decreases when bindings expire, move, or
	// are invalidated, and aliases for one logical session count only once.
	AccountActiveSessions = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cliproxy_account_active_sessions",
		Help: "Current number of logical session home bindings retained for this account.",
	}, []string{"auth_id"})

	// AccountSessionRebindsTotal counts routing changes away from or back to a
	// session's home auth without treating the temporary route as a new logical
	// session. The reason label is intentionally bounded by the selector.
	AccountSessionRebindsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cliproxy_account_session_rebinds_total",
		Help: "Cumulative number of session routing changes involving this account, by bounded reason.",
	}, []string{"auth_id", "reason"})

	// AccountFailuresTotal counts upstream provider errors attributed to a
	// specific account, labeled by the HTTP status the upstream returned.
	AccountFailuresTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cliproxy_account_failures_total",
		Help: "Cumulative number of upstream provider errors for this account, labeled by http_status.",
	}, []string{"auth_id", "http_status"})

	// AccountInvalidationsTotal counts credential revocations detected on the
	// refresh path (conductor marks the auth unavailable after an unauthorized
	// refresh). Request-path 401s are already visible in AccountFailuresTotal,
	// but a revocation discovered only via token refresh leaves no request-path
	// trace at all — this counter is what makes such events durable in the
	// metrics store (the 2026-07-19/20 alice revocation was invisible without
	// it). Incremented once per revocation transition, not per failed refresh
	// attempt.
	AccountInvalidationsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cliproxy_account_invalidations_total",
		Help: "Cumulative number of times this account's credential was detected revoked on the refresh path.",
	}, []string{"auth_id"})
)

// RecordAccountPick increments the per-account request counter. No-op if
// authID is empty (defensive: some callers may not have a resolved auth yet).
func RecordAccountPick(authID string) {
	if authID == "" {
		return
	}
	AccountRequestsTotal.WithLabelValues(authID).Inc()
}

// RecordNewSession increments the per-account cumulative home-binding event
// counter. Call exactly once when a logical session has no retained home.
func RecordNewSession(authID string) {
	if authID == "" {
		return
	}
	AccountSessionsTotal.WithLabelValues(authID).Inc()
}

// SetAccountActiveSessions publishes the current logical home-binding count
// for an account. The session cache calls this on create, expiry, invalidation,
// and shutdown so stale values do not survive selector replacement.
func SetAccountActiveSessions(authID string, count int) {
	if authID == "" {
		return
	}
	if count < 0 {
		count = 0
	}
	if count == 0 {
		AccountActiveSessions.DeleteLabelValues(authID)
		return
	}
	AccountActiveSessions.WithLabelValues(authID).Set(float64(count))
}

// RecordSessionRebind records a bounded session-routing transition.
func RecordSessionRebind(authID, reason string) {
	if authID == "" || reason == "" {
		return
	}
	AccountSessionRebindsTotal.WithLabelValues(authID, reason).Inc()
}

// RecordUpstreamFailure increments the per-account failure counter for the
// given HTTP status. authID may be empty for errors that occur before an
// account was resolved; such calls are skipped since there is nothing
// meaningful to attribute them to.
func RecordUpstreamFailure(authID string, httpStatus int) {
	if authID == "" {
		return
	}
	AccountFailuresTotal.WithLabelValues(authID, strconv.Itoa(httpStatus)).Inc()
}

// RecordAccountInvalidation increments the per-account revocation counter.
// Callers must invoke it only on the transition into the revoked/unavailable
// state, never per failed refresh attempt, so one ban counts exactly once.
func RecordAccountInvalidation(authID string) {
	if authID == "" {
		return
	}
	AccountInvalidationsTotal.WithLabelValues(authID).Inc()
}
