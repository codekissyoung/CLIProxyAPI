package auth

import (
	"strconv"
	"strings"
	"sync"
)

// AttributeConcurrency overrides the per-credential in-flight capacity limit
// for one account through the auth file "attributes" map. A parsed value of 0
// marks the account unlimited; missing, empty, or unparsable values fall back
// to the global account-concurrency-limit.
//
// ice divergence: per-account in-flight capacity gate (sub2api-style
// accounts.concurrency). Not present upstream.
const AttributeConcurrency = "concurrency"

var accountConcurrencyCapacity = struct {
	sync.Mutex
	limit    int
	inFlight map[string]int
}{}

// SetAccountConcurrencyLimit configures the global per-credential in-flight
// capacity limit. Values <= 0 leave the gate disabled for credentials without
// an explicit attribute override, preserving the legacy selection behavior.
func SetAccountConcurrencyLimit(limit int) {
	accountConcurrencyCapacity.Lock()
	defer accountConcurrencyCapacity.Unlock()
	if limit < 0 {
		limit = 0
	}
	accountConcurrencyCapacity.limit = limit
}

func accountConcurrencyLimit() int {
	accountConcurrencyCapacity.Lock()
	defer accountConcurrencyCapacity.Unlock()
	return accountConcurrencyCapacity.limit
}

// effectiveAccountConcurrencyLimit resolves the capacity limit for auth: the
// per-credential attribute override when present and valid, otherwise the
// global default. A result <= 0 means the credential is not gated.
func effectiveAccountConcurrencyLimit(auth *Auth) int {
	if auth == nil {
		return 0
	}
	if raw, ok := auth.Attributes[AttributeConcurrency]; ok {
		if parsed, errParse := strconv.Atoi(strings.TrimSpace(raw)); errParse == nil && parsed >= 0 {
			return parsed
		}
	}
	return accountConcurrencyLimit()
}

func accountInFlightCount(authID string) int {
	accountConcurrencyCapacity.Lock()
	defer accountConcurrencyCapacity.Unlock()
	return accountConcurrencyCapacity.inFlight[authID]
}

// accountCapacityBlocked reports whether auth already holds its configured
// in-flight capacity and must be skipped by selection.
func accountCapacityBlocked(auth *Auth) bool {
	limit := effectiveAccountConcurrencyLimit(auth)
	if limit <= 0 {
		return false
	}
	return accountInFlightCount(auth.ID) >= limit
}

// trackAuthInFlight reserves one in-flight slot for auth and returns a
// release function that frees it exactly once. Ungated credentials (effective
// limit <= 0) return a no-op so disabled deployments keep zero overhead and
// zero behavior change.
func trackAuthInFlight(auth *Auth) func() {
	if auth == nil || strings.TrimSpace(auth.ID) == "" || effectiveAccountConcurrencyLimit(auth) <= 0 {
		return func() {}
	}
	accountConcurrencyCapacity.Lock()
	if accountConcurrencyCapacity.inFlight == nil {
		accountConcurrencyCapacity.inFlight = make(map[string]int)
	}
	accountConcurrencyCapacity.inFlight[auth.ID]++
	accountConcurrencyCapacity.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			accountConcurrencyCapacity.Lock()
			defer accountConcurrencyCapacity.Unlock()
			if accountConcurrencyCapacity.inFlight[auth.ID] <= 1 {
				delete(accountConcurrencyCapacity.inFlight, auth.ID)
				return
			}
			accountConcurrencyCapacity.inFlight[auth.ID]--
		})
	}
}

// acquireExecutionConcurrency composes the per-credential execution admission
// gates for one attempt: the xAI OAuth admission gate plus the account
// in-flight capacity tracker. The returned release must run exactly once when
// the attempt (including a consumed stream) ends.
//
// ice divergence: composes the local capacity tracker with the xAI gate so
// every existing release call site keeps its exact-once stream lifecycle.
func (m *Manager) acquireExecutionConcurrency(auth *Auth) (func(), bool) {
	releaseXAI, acquired := m.acquireXAIOAuthConcurrency(auth)
	if !acquired {
		return nil, false
	}
	if m.HomeEnabled() {
		// Home-dispatched executions are accounted by the Home concurrency
		// lifecycle; local tracking would double count and leak under the
		// homeMode release guards.
		return releaseXAI, true
	}
	releaseCapacity := trackAuthInFlight(auth)
	return func() {
		releaseCapacity()
		releaseXAI()
	}, true
}
