package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func withXAIResponseHeaderTimeout(t *testing.T, timeout time.Duration) {
	t.Helper()
	previous := xaiResponseHeaderTimeout
	xaiResponseHeaderTimeout = timeout
	t.Cleanup(func() { xaiResponseHeaderTimeout = previous })
}

// A stream whose headers arrive in time must keep working after the watchdog
// window would have elapsed: the body is read long after, and only the caller
// releases the request context.
func TestXAIDoWithHeaderDeadlineKeepsSlowBodyAlive(t *testing.T) {
	withXAIResponseHeaderTimeout(t, 200*time.Millisecond)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(500 * time.Millisecond)
		_, _ = w.Write([]byte("data: late\n\n"))
	}))
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	resp, release, err := xaiDoWithHeaderDeadline(context.Background(), server.Client(), req)
	if err != nil {
		t.Fatalf("xaiDoWithHeaderDeadline() error = %v", err)
	}
	defer release()
	defer func() { _ = resp.Body.Close() }()

	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		t.Fatalf("ReadAll() error = %v", errRead)
	}
	if string(body) != "data: late\n\n" {
		t.Fatalf("body = %q, want the late chunk", string(body))
	}
}

func TestXAIDoWithHeaderDeadlineAbortsSilentUpstream(t *testing.T) {
	withXAIResponseHeaderTimeout(t, 100*time.Millisecond)

	released := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
		close(released)
	}))
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	start := time.Now()
	resp, release, errDo := xaiDoWithHeaderDeadline(context.Background(), server.Client(), req)
	elapsed := time.Since(start)

	if errDo == nil {
		t.Fatal("xaiDoWithHeaderDeadline() error = nil, want timeout")
	}
	if resp != nil || release != nil {
		t.Fatal("timed out call must not hand back a response or a cancel func")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("watchdog took %v, want it to fire near the deadline", elapsed)
	}

	var timeoutErr xaiHeaderTimeoutError
	if !errors.As(errDo, &timeoutErr) {
		t.Fatalf("error = %T (%v), want xaiHeaderTimeoutError", errDo, errDo)
	}
	if got := timeoutErr.StatusCode(); got != http.StatusGatewayTimeout {
		t.Fatalf("StatusCode() = %d, want %d", got, http.StatusGatewayTimeout)
	}

	// The credential must not be blamed for a silent upstream: with one xAI
	// account in the pool, a credential-scoped failure would take it offline.
	var requestScoped cliproxyexecutor.RequestScopedError
	if !errors.As(errDo, &requestScoped) {
		t.Fatal("timeout error does not implement RequestScopedError")
	}
	if !requestScoped.IsRequestScoped() {
		t.Fatal("IsRequestScoped() = false, want true")
	}

	select {
	case <-released:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream request was not canceled by the watchdog")
	}
}

// A caller that goes away must surface its own error, not a watchdog timeout.
func TestXAIDoWithHeaderDeadlinePropagatesCallerCancel(t *testing.T) {
	withXAIResponseHeaderTimeout(t, 5*time.Second)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequest(http.MethodPost, server.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	time.AfterFunc(100*time.Millisecond, cancel)

	_, _, errDo := xaiDoWithHeaderDeadline(ctx, server.Client(), req)
	if errDo == nil {
		t.Fatal("error = nil, want context cancellation")
	}
	var timeoutErr xaiHeaderTimeoutError
	if errors.As(errDo, &timeoutErr) {
		t.Fatalf("caller cancellation reported as a watchdog timeout: %v", errDo)
	}
	if !errors.Is(errDo, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", errDo)
	}
}
