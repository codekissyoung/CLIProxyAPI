package auth

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

const xaiOAuthConcurrencyRetryAfter = time.Second

type xaiOAuthConcurrencyBusyError struct {
	cause      *Error
	retryAfter time.Duration
}

func newXAIOAuthConcurrencyBusyError() error {
	return &xaiOAuthConcurrencyBusyError{
		cause: &Error{
			Code:       "credential_concurrency_exceeded",
			Message:    "all xAI OAuth credentials reached the per-credential concurrency limit",
			Retryable:  true,
			HTTPStatus: http.StatusTooManyRequests,
		},
		retryAfter: xaiOAuthConcurrencyRetryAfter,
	}
}

func (e *xaiOAuthConcurrencyBusyError) Error() string {
	if e == nil || e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

func (e *xaiOAuthConcurrencyBusyError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *xaiOAuthConcurrencyBusyError) StatusCode() int {
	if e == nil || e.cause == nil {
		return 0
	}
	return e.cause.StatusCode()
}

func (e *xaiOAuthConcurrencyBusyError) RetryAfter() *time.Duration {
	if e == nil || e.retryAfter <= 0 {
		return nil
	}
	value := e.retryAfter
	return &value
}

func (e *xaiOAuthConcurrencyBusyError) SafeResponseHeaders() http.Header {
	if e == nil {
		return nil
	}
	return safeRetryAfterHeader(e.retryAfter)
}

func (m *Manager) acquireXAIOAuthConcurrency(auth *Auth) (func(), bool) {
	if m == nil || auth == nil || strings.TrimSpace(auth.ID) == "" || !strings.EqualFold(strings.TrimSpace(auth.Provider), "xai") || auth.AuthKind() != AuthKindOAuth {
		return func() {}, true
	}
	limit := m.xaiOAuthConcurrencyLimit()
	if limit <= 0 {
		return func() {}, true
	}

	m.xaiOAuthConcurrencyMu.Lock()
	if m.xaiOAuthInFlight == nil {
		m.xaiOAuthInFlight = make(map[string]int)
	}
	if m.xaiOAuthInFlight[auth.ID] >= limit {
		m.xaiOAuthConcurrencyMu.Unlock()
		return nil, false
	}
	m.xaiOAuthInFlight[auth.ID]++
	m.xaiOAuthConcurrencyMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			m.xaiOAuthConcurrencyMu.Lock()
			defer m.xaiOAuthConcurrencyMu.Unlock()
			if m.xaiOAuthInFlight[auth.ID] <= 1 {
				delete(m.xaiOAuthInFlight, auth.ID)
				return
			}
			m.xaiOAuthInFlight[auth.ID]--
		})
	}, true
}

func (m *Manager) xaiOAuthConcurrencyLimit() int {
	if m == nil {
		return 0
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil || cfg.Home.Enabled || cfg.XAIOAuthMaxConcurrency <= 0 {
		return 0
	}
	return cfg.XAIOAuthMaxConcurrency
}

func xaiOAuthConcurrencyBusy(err error) bool {
	var busy *xaiOAuthConcurrencyBusyError
	return errors.As(err, &busy) && busy != nil
}
