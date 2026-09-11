package auth

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AttributeConcurrency overrides the per-credential in-flight capacity limit
// for one account through the auth file "attributes" map. A parsed value of 0
// marks the account unlimited; missing, empty, or unparsable values fall back
// to the global account-concurrency-limit.
//
// ice divergence: per-account in-flight capacity gate (sub2api-style
// accounts.concurrency). Not present upstream.
const AttributeConcurrency = "concurrency"

const accountConcurrencyRetryAfter = time.Second

var accountConcurrencyCapacity = struct {
	sync.Mutex
	limit     int
	inFlight  map[string]int
	highWater map[string]int
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

// accountInFlightHighWater reports the highest in-flight count ever observed
// for authID. It backs capacity stress assertions and pool observability.
func accountInFlightHighWater(authID string) int {
	accountConcurrencyCapacity.Lock()
	defer accountConcurrencyCapacity.Unlock()
	return accountConcurrencyCapacity.highWater[authID]
}

// tryAcquireAccountCapacity atomically checks the remaining capacity of auth
// and reserves one in-flight slot under the same lock, so concurrent pickers
// can never observe the same free slot (Codex review: the previous read-then-
// increment split let a burst push the counter far past the limit). The
// returned release frees the reservation exactly once. Ungated credentials
// (effective limit <= 0) succeed with a no-op release, keeping disabled
// deployments at zero overhead and zero behavior change.
//
// ice divergence: atomic check-and-reserve admission for the per-account
// capacity gate; upstream has no equivalent.
func tryAcquireAccountCapacity(auth *Auth) (func(), bool) {
	if auth == nil || strings.TrimSpace(auth.ID) == "" {
		return func() {}, true
	}
	limit := effectiveAccountConcurrencyLimit(auth)
	if limit <= 0 {
		return func() {}, true
	}
	accountConcurrencyCapacity.Lock()
	if accountConcurrencyCapacity.inFlight == nil {
		accountConcurrencyCapacity.inFlight = make(map[string]int)
		accountConcurrencyCapacity.highWater = make(map[string]int)
	}
	if accountConcurrencyCapacity.inFlight[auth.ID] >= limit {
		accountConcurrencyCapacity.Unlock()
		return nil, false
	}
	accountConcurrencyCapacity.inFlight[auth.ID]++
	if accountConcurrencyCapacity.inFlight[auth.ID] > accountConcurrencyCapacity.highWater[auth.ID] {
		accountConcurrencyCapacity.highWater[auth.ID] = accountConcurrencyCapacity.inFlight[auth.ID]
	}
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
	}, true
}

// capacityBusyTakesPriority reports whether the capacity busy error is the
// truthful final error for a failed pick: only when the pick exhausted every
// candidate without a real unavailability reason (a bare auth_not_found, which
// is what remains once capacity-rejected credentials sit in tried). A pick
// failure carrying cooldown or unavailable state means some candidate was NOT
// rejected for capacity, so that reason must win over the 429 + Retry-After
// busy contract (review minor: busy previously masked cooldown causes).
func capacityBusyTakesPriority(errPick error) bool {
	if errPick == nil {
		return false
	}
	var cooldownErr *modelCooldownError
	if errors.As(errPick, &cooldownErr) && cooldownErr != nil {
		return false
	}
	var authErr *Error
	if errors.As(errPick, &authErr) && authErr != nil && authErr.Code == "auth_unavailable" {
		return false
	}
	return true
}

// accountConcurrencyBusyError reports that every eligible credential is at its
// per-account in-flight capacity. It mirrors the xAI OAuth busy contract
// (retryable 429 with a short Retry-After) so clients back off briefly instead
// of hammering saturated accounts.
type accountConcurrencyBusyError struct {
	cause      *Error
	retryAfter time.Duration
}

func newAccountConcurrencyBusyError() error {
	return &accountConcurrencyBusyError{
		cause: &Error{
			Code:       "credential_concurrency_exceeded",
			Message:    "all credentials reached the per-account concurrency limit",
			Retryable:  true,
			HTTPStatus: http.StatusTooManyRequests,
		},
		retryAfter: accountConcurrencyRetryAfter,
	}
}

func (e *accountConcurrencyBusyError) Error() string {
	if e == nil || e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

func (e *accountConcurrencyBusyError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *accountConcurrencyBusyError) StatusCode() int {
	if e == nil || e.cause == nil {
		return 0
	}
	return e.cause.StatusCode()
}

func (e *accountConcurrencyBusyError) RetryAfter() *time.Duration {
	if e == nil || e.retryAfter <= 0 {
		return nil
	}
	value := e.retryAfter
	return &value
}

func (e *accountConcurrencyBusyError) SafeResponseHeaders() http.Header {
	if e == nil {
		return nil
	}
	return safeRetryAfterHeader(e.retryAfter)
}

// acquireExecutionConcurrency composes the per-credential execution admission
// gates for one attempt: the xAI OAuth admission gate plus the atomic account
// in-flight capacity reservation. On success it returns (release, nil); when
// either gate rejects it returns (nil, busyErr) so the caller fails over to
// the next credential. The release must run exactly once when the attempt
// (including a consumed stream) ends.
//
// Semantics: every execution attempt reserves and releases independently
// (per-attempt, never held across retry rounds). The reservation is taken
// immediately after the pick in the same loop iteration, and every path out
// of the iteration -- prepare failure, interceptor error, per-upstream-model
// failure, success, or stream handoff -- flows through the release, so an
// abandoned attempt can never strand a slot.
//
// ice divergence: composes the local capacity reservation with the xAI gate
// so every existing release call site keeps its exact-once stream lifecycle.
func (m *Manager) acquireExecutionConcurrency(auth *Auth) (func(), error) {
	releaseXAI, acquired := m.acquireXAIOAuthConcurrency(auth)
	if !acquired {
		return nil, newXAIOAuthConcurrencyBusyError()
	}
	if m.HomeEnabled() {
		// Home-dispatched executions are accounted by the Home concurrency
		// lifecycle; local tracking would double count and leak under the
		// homeMode release guards.
		return releaseXAI, nil
	}
	releaseCapacity, okCapacity := tryAcquireAccountCapacity(auth)
	if !okCapacity {
		releaseXAI()
		return nil, newAccountConcurrencyBusyError()
	}
	return func() {
		releaseCapacity()
		releaseXAI()
	}, nil
}
