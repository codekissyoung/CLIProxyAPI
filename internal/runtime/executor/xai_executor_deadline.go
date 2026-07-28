package executor

import (
	"context"
	"fmt"
	"net/http"
	"time"

	log "github.com/sirupsen/logrus"
)

// xaiResponseHeaderTimeout bounds how long an xAI chat request may take to
// return response headers. The CLI chat proxy can accept a request and then
// never answer it (2026-07-22/23: 13 requests hung until the downstream relay
// gave up at its own 80s first-byte deadline), and because each hung request
// holds a per-credential concurrency slot, a handful of them starves the pool.
//
// 30 days of production first-byte samples on this upstream: p50 1.5s,
// p95 13.7s, p99 41s, max 74s. 60s aborts 1 request in 2353 while still
// failing ~20s before the relay's deadline, so the relay receives a real
// error it can fail over on instead of a connection that simply dies.
//
// A var rather than a const so tests can shorten it.
var xaiResponseHeaderTimeout = 60 * time.Second

// xaiHeaderTimeoutError reports the watchdog abort. It is request scoped on
// purpose: a swallowed or slow upstream says nothing about the credential, and
// with a single xAI account in the pool, marking it unavailable would take the
// whole pool down for a failure the next request may not even hit.
type xaiHeaderTimeoutError struct {
	statusErr
}

func newXAIHeaderTimeoutError(timeout time.Duration) xaiHeaderTimeoutError {
	return xaiHeaderTimeoutError{statusErr: statusErr{
		code: http.StatusGatewayTimeout,
		msg:  fmt.Sprintf("xai upstream returned no response headers within %s", timeout),
	}}
}

func (xaiHeaderTimeoutError) IsRequestScoped() bool {
	return true
}

// xaiDoWithHeaderDeadline executes req under a watchdog that aborts it when the
// upstream does not return response headers in time.
//
// On success it returns a cancel func that owns the request context. The caller
// must invoke it once the response body has been fully consumed — deferring it
// at the call site would cut an SSE stream short. On failure the context is
// already released and the returned cancel func is nil.
func xaiDoWithHeaderDeadline(ctx context.Context, client *http.Client, req *http.Request) (*http.Response, context.CancelFunc, error) {
	reqCtx, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(xaiResponseHeaderTimeout, cancel)

	resp, err := client.Do(req.WithContext(reqCtx))

	// Stop reports false once the watchdog has fired, which means the request
	// context is already being canceled. Any response handed back in that race
	// is unusable, so treat it as a timeout rather than streaming from a body
	// that is about to be torn down.
	if !timer.Stop() {
		if resp != nil {
			if errClose := resp.Body.Close(); errClose != nil {
				log.Debugf("xai executor: close timed out response body error: %v", errClose)
			}
		}
		cancel()
		return nil, nil, newXAIHeaderTimeoutError(xaiResponseHeaderTimeout)
	}
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return resp, cancel, nil
}
