package executor

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// ice divergence: the Codex Responses websocket dials through the captured
// uTLS ClientHello dial functions, so the execution-scoped proxy override is
// carried by those dialers instead of upstream's websocket.Dialer.Proxy hook.
// The priority contract (request proxy > credential proxy > global proxy) is
// asserted the same way upstream asserts it.
func TestRequestProxyOverridesCredentialProxyForWebsocketAndAntigravity(t *testing.T) {
	t.Parallel()

	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("listen: %v", errListen)
	}
	defer func() {
		if errClose := listener.Close(); errClose != nil {
			t.Logf("close listener: %v", errClose)
		}
	}()
	accepted := make(chan struct{}, 1)
	go func() {
		conn, errAccept := listener.Accept()
		if errAccept != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		reader := bufio.NewReader(conn)
		if _, errRead := http.ReadRequest(reader); errRead != nil {
			return
		}
		accepted <- struct{}{}
		<-accepted
	}()

	requestProxy := "http://" + listener.Addr().String()
	ctx := cliproxyexecutor.WithRequestProxyURL(context.Background(), requestProxy)
	cfg := &config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example:8082"}}
	auth := &cliproxyauth.Auth{ProxyURL: "http://auth-proxy.example:8080"}

	if got := antigravityProxyURL(ctx, cfg, auth); got != requestProxy {
		t.Fatalf("antigravity proxy = %q, want %q", got, requestProxy)
	}
	if got := executionProxyURL(ctx, cfg, auth); got != requestProxy {
		t.Fatalf("execution proxy = %q, want %q", got, requestProxy)
	}

	dialer := newProxyAwareWebsocketDialer(ctx, cfg, auth)
	if dialer.Proxy != nil {
		t.Fatal("websocket dialer must not use the stock proxy hook (uTLS dial functions carry the proxy)")
	}
	if dialer.NetDialContext == nil || dialer.NetDialTLSContext == nil {
		t.Fatal("websocket dialer is missing the codex CLI uTLS dial functions")
	}

	dialCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	dialDone := make(chan struct{})
	go func() {
		defer close(dialDone)
		conn, errDial := dialer.NetDialContext(dialCtx, "tcp", "upstream.example:443")
		if conn != nil {
			_ = conn.Close()
		}
		_ = errDial
	}()

	select {
	case <-accepted:
		accepted <- struct{}{}
	case <-dialDone:
		t.Fatal("websocket dial did not reach the execution-scoped proxy")
	}
	cancel()
	<-dialDone
}
