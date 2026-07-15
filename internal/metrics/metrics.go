// Package metrics exposes account-level cumulative counters for the auth pool
// (Codex/Claude/... OAuth token files under the auths dir). These exist so an
// operator can answer "how many times has this specific account been used,
// how many distinct sessions has it ever served, how often has it failed"
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

	// AccountSessionsTotal counts distinct new session-affinity bindings ever
	// created for this account (i.e. the "cache miss, new binding" event in
	// selector.go). This is the cumulative session count operators care about
	// when judging whether an account is being shared across an unusually
	// large number of concurrent end users.
	AccountSessionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cliproxy_account_sessions_total",
		Help: "Cumulative number of distinct new session bindings created for this account.",
	}, []string{"auth_id"})

	// AccountFailuresTotal counts upstream provider errors attributed to a
	// specific account, labeled by the HTTP status the upstream returned.
	AccountFailuresTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cliproxy_account_failures_total",
		Help: "Cumulative number of upstream provider errors for this account, labeled by http_status.",
	}, []string{"auth_id", "http_status"})
)

// RecordAccountPick increments the per-account request counter. No-op if
// authID is empty (defensive: some callers may not have a resolved auth yet).
func RecordAccountPick(authID string) {
	if authID == "" {
		return
	}
	AccountRequestsTotal.WithLabelValues(authID).Inc()
}

// RecordNewSession increments the per-account cumulative distinct-session
// counter. Call exactly once per genuinely new session-affinity binding.
func RecordNewSession(authID string) {
	if authID == "" {
		return
	}
	AccountSessionsTotal.WithLabelValues(authID).Inc()
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
