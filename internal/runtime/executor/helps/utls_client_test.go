package helps

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type utlsClientRoundTripFunc func(*http.Request) (*http.Response, error)

func (f utlsClientRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestNewUtlsHTTPClientUsesContextRoundTripperForProtectedHost(t *testing.T) {
	t.Parallel()

	called := false
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		called = true
		if req.URL.Hostname() != "chatgpt.com" {
			t.Fatalf("hostname = %q, want chatgpt.com", req.URL.Hostname())
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("{}")),
			Request:    req,
		}, nil
	}))

	client := NewUtlsHTTPClient(ctx, nil, nil, 0)
	resp, err := client.Get("https://chatgpt.com/backend-api/codex/responses")
	if err != nil {
		t.Fatalf("client.Get returned error: %v", err)
	}
	if errClose := resp.Body.Close(); errClose != nil {
		t.Fatalf("response body close returned error: %v", errClose)
	}
	if !called {
		t.Fatal("expected context RoundTripper to handle protected host request")
	}
}

func TestNewUtlsRoundTripperRoutesByHost(t *testing.T) {
	t.Parallel()

	fallbackUsed := false
	fallback := utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		fallbackUsed = true
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("{}")),
			Request:    req,
		}, nil
	})

	rt, ok := NewUtlsRoundTripper("", fallback).(*fallbackRoundTripper)
	if !ok {
		t.Fatal("NewUtlsRoundTripper did not return a *fallbackRoundTripper")
	}
	if rt.utls == nil {
		t.Fatal("expected utls RoundTripper to be configured")
	}

	// Non-protected host must go through the fallback, never the utls path.
	req, _ := http.NewRequest(http.MethodGet, "https://example.com/", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip returned error: %v", err)
	}
	if errClose := resp.Body.Close(); errClose != nil {
		t.Fatalf("response body close returned error: %v", errClose)
	}
	if !fallbackUsed {
		t.Fatal("expected non-protected host to use the fallback RoundTripper")
	}

	// chatgpt.com is in the protected set, so the utls leg owns it.
	if _, okProtected := utlsProtectedHosts["chatgpt.com"]; !okProtected {
		t.Fatal("chatgpt.com must be a utls-protected host for Codex fingerprinting")
	}
}
