package helps

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	tls "github.com/refraction-networking/utls"
	internalcache "github.com/router-for-me/CLIProxyAPI/v7/internal/cache"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/httpwire"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/proxy"
)

// codexCLIHTTPRoundTripper reproduces the Codex CLI 0.147.0 HTTPS transport:
// an OpenSSL-style ClientHello over HTTP/1.1 with a reusable connection pool.
type codexCLIHTTPRoundTripper struct {
	dialer    proxy.Dialer
	transport *http.Transport
}

func newCodexCLIHTTPRoundTripper(proxyURL string) *codexCLIHTTPRoundTripper {
	roundTripper := &codexCLIHTTPRoundTripper{dialer: codexCLIProxyDialer(proxyURL)}
	roundTripper.transport = &http.Transport{
		ForceAttemptHTTP2: false,
		DialTLSContext:    roundTripper.dialTLSContext,
	}
	return roundTripper
}

func codexCLIProxyDialer(proxyURL string) proxy.Dialer {
	var dialer proxy.Dialer = proxyutil.IPv4OnlyDirect
	if strings.TrimSpace(proxyURL) != "" {
		proxyDialer, mode, errBuild := proxyutil.BuildDialer(proxyURL)
		if errBuild != nil {
			log.Errorf("codex cli tls: failed to configure proxy dialer for %q: %v", proxyutil.Redact(proxyURL), errBuild)
		} else if mode == proxyutil.ModeProxy && proxyDialer != nil {
			dialer = proxyDialer
		}
	}
	return dialer
}

func dialCodexCLIContext(ctx context.Context, dialer proxy.Dialer, network, addr string) (net.Conn, error) {
	contextDialer, ok := dialer.(proxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("codex cli tls: dialer does not support context cancellation")
	}
	if network == "tcp" {
		network = "tcp4"
	}
	conn, errDial := contextDialer.DialContext(ctx, network, addr)
	if errDial != nil {
		return nil, fmt.Errorf("codex cli tls: dial upstream: %w", errDial)
	}
	return conn, nil
}

func (t *codexCLIHTTPRoundTripper) dialTLSContext(ctx context.Context, network, addr string) (net.Conn, error) {
	conn, errDial := dialCodexCLIContext(ctx, t.dialer, network, addr)
	if errDial != nil {
		return nil, errDial
	}
	host, _, errSplit := net.SplitHostPort(addr)
	if errSplit != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("codex cli tls: split upstream address: %w", errSplit)
	}

	tlsConfig := &tls.Config{ServerName: host}
	tlsConn := tls.UClient(conn, tlsConfig, tls.HelloCustom)
	if errPreset := tlsConn.ApplyPreset(codexCLIHTTPClientHelloSpec()); errPreset != nil {
		_ = tlsConn.Close()
		return nil, fmt.Errorf("codex cli tls: apply HTTP ClientHello: %w", errPreset)
	}
	if errHandshake := tlsConn.HandshakeContext(ctx); errHandshake != nil {
		_ = tlsConn.Close()
		return nil, fmt.Errorf("codex cli tls: HTTP handshake upstream: %w", errHandshake)
	}
	return httpwire.NewOrderedRequestConn(tlsConn, codexCLIRequestHeaderOrder), nil
}

func (t *codexCLIHTTPRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.transport.RoundTrip(req)
}

func (t *codexCLIHTTPRoundTripper) CloseIdleConnections() {
	if t != nil && t.transport != nil {
		t.transport.CloseIdleConnections()
	}
}

var codexCLIHTTPHeaderOrder = []string{
	"Version",
	"X-Codex-Beta-Features",
	"X-Codex-Window-Id",
	"X-Codex-Turn-Metadata",
	"X-OpenAI-Internal-Codex-Responses-Lite",
	"X-Client-Request-Id",
	"Session-Id",
	"Thread-Id",
	"Accept",
	"Content-Type",
	"Authorization",
	"Originator",
	"User-Agent",
	"Host",
	"Content-Length",
}

func codexCLIRequestHeaderOrder(_, _ string) []string {
	return codexCLIHTTPHeaderOrder
}

var codexCLIWebsocketHeaderOrder = []string{
	"Host",
	"Connection",
	"Upgrade",
	"Sec-WebSocket-Version",
	"Sec-WebSocket-Key",
	"Authorization",
	"User-Agent",
	"Originator",
	"OpenAI-Beta",
	"X-Codex-Turn-Metadata",
	"Version",
	"X-Codex-Beta-Features",
	"X-Client-Request-Id",
	"Session-Id",
	"Thread-Id",
	"X-Codex-Window-Id",
	"Sec-WebSocket-Extensions",
}

func codexCLIWebsocketRequestHeaderOrder(_, _ string) []string {
	return codexCLIWebsocketHeaderOrder
}

// codexCLIHTTPClientHelloSpec reproduces the deterministic reqwest/OpenSSL
// ClientHello emitted by Codex CLI 0.147.0 on Ubuntu x86_64.
func codexCLIHTTPClientHelloSpec() *tls.ClientHelloSpec {
	return &tls.ClientHelloSpec{
		CipherSuites: []uint16{
			0x1302, 0x1303, 0x1301, 0xc02c, 0xc030, 0x009f, 0xcca9, 0xcca8,
			0xccaa, 0xc02b, 0xc02f, 0x009e, 0xc024, 0xc028, 0x006b, 0xc023,
			0xc027, 0x0067, 0xc00a, 0xc014, 0x0039, 0xc009, 0xc013, 0x0033,
			0x009d, 0x009c, 0x003d, 0x003c, 0x0035, 0x002f,
		},
		CompressionMethods: []uint8{0},
		Extensions: []tls.TLSExtension{
			&tls.RenegotiationInfoExtension{Renegotiation: tls.RenegotiateOnceAsClient},
			&tls.SNIExtension{},
			&tls.SupportedPointsExtension{SupportedPoints: []byte{0}},
			&tls.SupportedCurvesExtension{Curves: []tls.CurveID{
				tls.X25519MLKEM768, tls.X25519, tls.CurveP256, tls.CurveID(30),
				tls.CurveP384, tls.CurveP521, tls.FakeCurveFFDHE2048, tls.FakeCurveFFDHE3072,
			}},
			&tls.SessionTicketExtension{},
			&tls.GenericExtension{Id: 22},
			&tls.ExtendedMasterSecretExtension{},
			&tls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []tls.SignatureScheme{
				0x0905, 0x0906, 0x0904, 0x0403, 0x0503, 0x0603, 0x0807,
				0x0808, 0x081a, 0x081b, 0x081c, 0x0809, 0x080a, 0x080b,
				0x0804, 0x0805, 0x0806, 0x0401, 0x0501, 0x0601, 0x0303,
				0x0301, 0x0302, 0x0402, 0x0502, 0x0602,
			}},
			&tls.SupportedVersionsExtension{Versions: []uint16{tls.VersionTLS13, tls.VersionTLS12}},
			&tls.PSKKeyExchangeModesExtension{Modes: []uint8{tls.PskModeDHE}},
			&tls.KeyShareExtension{KeyShares: []tls.KeyShare{
				{Group: tls.X25519MLKEM768},
				{Group: tls.X25519},
			}},
		},
	}
}

// codexCLIWebsocketClientHelloSpec reproduces the rustls/AWS-LC handshake
// emitted by Codex CLI 0.147.0. Rustls randomizes extension order per dial.
func codexCLIWebsocketClientHelloSpec() *tls.ClientHelloSpec {
	extensions := []tls.TLSExtension{
		&tls.SNIExtension{},
		&tls.StatusRequestExtension{},
		&tls.SupportedCurvesExtension{Curves: []tls.CurveID{
			tls.X25519MLKEM768, tls.X25519, tls.CurveP256, tls.CurveP384,
		}},
		&tls.SupportedPointsExtension{SupportedPoints: []byte{0}},
		&tls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []tls.SignatureScheme{
			0x0503, 0x0403, 0x0603, 0x0807, 0x0806,
			0x0805, 0x0804, 0x0601, 0x0501, 0x0401,
		}},
		&tls.ExtendedMasterSecretExtension{},
		&tls.SessionTicketExtension{},
		&tls.SupportedVersionsExtension{Versions: []uint16{tls.VersionTLS13, tls.VersionTLS12}},
		&tls.PSKKeyExchangeModesExtension{Modes: []uint8{tls.PskModeDHE}},
		&tls.KeyShareExtension{KeyShares: []tls.KeyShare{
			{Group: tls.X25519MLKEM768},
			{Group: tls.X25519},
		}},
	}
	shuffleCodexCLIExtensions(extensions)
	return &tls.ClientHelloSpec{
		CipherSuites: []uint16{
			0x1302, 0x1301, 0x1303, 0xc02c, 0xc02b,
			0xcca9, 0xc030, 0xc02f, 0xcca8, 0x00ff,
		},
		CompressionMethods: []uint8{0},
		Extensions:         extensions,
	}
}

func shuffleCodexCLIExtensions(extensions []tls.TLSExtension) {
	var random [8]byte
	for i := len(extensions) - 1; i > 0; i-- {
		if _, errRead := cryptorand.Read(random[:]); errRead != nil {
			return
		}
		j := int(binary.LittleEndian.Uint64(random[:]) % uint64(i+1))
		extensions[i], extensions[j] = extensions[j], extensions[i]
	}
}

// NewCodexCLIWebsocketDialFunctions returns proxy-aware plain and TLS dial
// functions for the Codex Responses websocket. Direct dials stay IPv4-only.
func NewCodexCLIWebsocketDialFunctions(proxyURL string) (
	func(context.Context, string, string) (net.Conn, error),
	func(context.Context, string, string) (net.Conn, error),
) {
	dialer := codexCLIProxyDialer(proxyURL)
	dialContext := func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, errDial := dialCodexCLIContext(ctx, dialer, network, addr)
		if errDial != nil {
			return nil, errDial
		}
		return httpwire.NewOrderedFirstRequestConn(conn, codexCLIWebsocketRequestHeaderOrder), nil
	}
	dialTLSContext := func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, errDial := dialCodexCLIContext(ctx, dialer, network, addr)
		if errDial != nil {
			return nil, errDial
		}
		host, _, errSplit := net.SplitHostPort(addr)
		if errSplit != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("codex cli tls: split websocket address: %w", errSplit)
		}
		tlsConn := tls.UClient(conn, &tls.Config{ServerName: host}, tls.HelloCustom)
		if errPreset := tlsConn.ApplyPreset(codexCLIWebsocketClientHelloSpec()); errPreset != nil {
			_ = tlsConn.Close()
			return nil, fmt.Errorf("codex cli tls: apply websocket ClientHello: %w", errPreset)
		}
		if errHandshake := tlsConn.HandshakeContext(ctx); errHandshake != nil {
			_ = tlsConn.Close()
			return nil, fmt.Errorf("codex cli tls: websocket handshake upstream: %w", errHandshake)
		}
		return httpwire.NewOrderedFirstRequestConn(tlsConn, codexCLIWebsocketRequestHeaderOrder), nil
	}
	return dialContext, dialTLSContext
}

// claudeCodeSessionCacheCapacity bounds the per-transport TLS session cache for
// the Anthropic inference plane.
const claudeCodeSessionCacheCapacity = 32

// newClaudeCodeTLSConfig builds the uTLS config for one inference-plane dial.
//
// OmitEmptyPsk keeps the pre_shared_key extension silent until a session is
// cached, so an unresumed ClientHello stays byte-identical to the captured
// native handshake. PreferSkipResumptionOnNilExtension turns uTLS's HelloCustom
// "resume without the matching extension" panic into a skipped resumption.
func newClaudeCodeTLSConfig(host string, sessionCache tls.ClientSessionCache) *tls.Config {
	return &tls.Config{
		ServerName:                         host,
		ClientSessionCache:                 sessionCache,
		OmitEmptyPsk:                       true,
		PreferSkipResumptionOnNilExtension: true,
	}
}

// claudeCodeTLSClientHelloSpec reproduces the deterministic Node/OpenSSL
// ClientHello emitted by Claude Code 2.1.220 on macOS arm64. Keep this spec in
// sync with a fresh native capture whenever the advertised Claude Code version
// changes.
func claudeCodeTLSClientHelloSpec() *tls.ClientHelloSpec {
	return &tls.ClientHelloSpec{
		CipherSuites: []uint16{
			tls.TLS_AES_128_GCM_SHA256,
			tls.TLS_AES_256_GCM_SHA384,
			tls.TLS_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
			tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
			tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
			tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_RSA_WITH_AES_128_CBC_SHA,
			tls.TLS_RSA_WITH_AES_256_CBC_SHA,
		},
		CompressionMethods: []uint8{0},
		Extensions: []tls.TLSExtension{
			&tls.SNIExtension{},
			&tls.ExtendedMasterSecretExtension{},
			&tls.RenegotiationInfoExtension{Renegotiation: tls.RenegotiateOnceAsClient},
			&tls.SupportedCurvesExtension{Curves: []tls.CurveID{tls.X25519, tls.CurveP256, tls.CurveP384}},
			&tls.SupportedPointsExtension{SupportedPoints: []byte{0}},
			&tls.SessionTicketExtension{},
			&tls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}},
			&tls.StatusRequestExtension{},
			&tls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []tls.SignatureScheme{
				tls.ECDSAWithP256AndSHA256,
				tls.PSSWithSHA256,
				tls.PKCS1WithSHA256,
				tls.ECDSAWithP384AndSHA384,
				tls.PSSWithSHA384,
				tls.PKCS1WithSHA384,
				tls.PSSWithSHA512,
				tls.PKCS1WithSHA512,
				tls.PKCS1WithSHA1,
			}},
			&tls.SCTExtension{},
			&tls.KeyShareExtension{KeyShares: []tls.KeyShare{{Group: tls.X25519}}},
			&tls.PSKKeyExchangeModesExtension{Modes: []uint8{tls.PskModeDHE}},
			&tls.SupportedVersionsExtension{Versions: []uint16{tls.VersionTLS13, tls.VersionTLS12}},
			&tls.UtlsPaddingExtension{GetPaddingLen: tls.BoringPaddingStyle},
			// pre_shared_key MUST be the final extension (RFC 8446 4.2.11), after
			// padding. It contributes zero bytes until a cached session exists.
			&tls.UtlsPreSharedKeyExtension{},
		},
	}
}

const claudeCodeRoundTripperCacheCapacity = 64

var claudeCodeRoundTripperCache = internalcache.NewBoundedLRU[string, http.RoundTripper](
	claudeCodeRoundTripperCacheCapacity,
	func(_ string, roundTripper http.RoundTripper) {
		if transport, ok := roundTripper.(interface{ CloseIdleConnections() }); ok {
			transport.CloseIdleConnections()
		}
	},
)

var claudeCodeMessagesHeaderOrder = []string{
	"Accept",
	"Authorization",
	"Content-Type",
	"User-Agent",
	"X-Claude-Code-Session-Id",
	"X-Stainless-Arch",
	"X-Stainless-Lang",
	"X-Stainless-OS",
	"X-Stainless-Package-Version",
	"X-Stainless-Retry-Count",
	"X-Stainless-Runtime",
	"X-Stainless-Runtime-Version",
	"X-Stainless-Timeout",
	"anthropic-beta",
	"anthropic-dangerous-direct-browser-access",
	"anthropic-version",
	"x-app",
	"x-client-request-id",
	"Connection",
	"Host",
	"Accept-Encoding",
	"Content-Length",
}

var claudeCodeCountTokensHeaderOrder = []string{
	"Accept",
	"Authorization",
	"Content-Type",
	"User-Agent",
	"X-Claude-Code-Session-Id",
	"X-Stainless-Arch",
	"X-Stainless-Lang",
	"X-Stainless-OS",
	"X-Stainless-Package-Version",
	"X-Stainless-Retry-Count",
	"X-Stainless-Runtime",
	"X-Stainless-Runtime-Version",
	"anthropic-beta",
	"anthropic-dangerous-direct-browser-access",
	"anthropic-version",
	"x-app",
	"x-client-request-id",
	"Connection",
	"Host",
	"Accept-Encoding",
	"Content-Length",
}

func claudeCodeRequestHeaderOrder(_, requestTarget string) []string {
	if strings.HasPrefix(requestTarget, "/v1/messages/count_tokens") {
		return claudeCodeCountTokensHeaderOrder
	}
	return claudeCodeMessagesHeaderOrder
}

func cachedClaudeCodeRoundTripper(proxyURL string) http.RoundTripper {
	return claudeCodeRoundTripperCache.GetOrAdd(proxyURL, func() http.RoundTripper {
		return newClaudeCodeRoundTripper(proxyURL)
	})
}

func newClaudeCodeRoundTripper(proxyURL string) http.RoundTripper {
	// The cache is scoped to this round tripper, which is already keyed by proxy,
	// so resumption never crosses proxy boundaries.
	sessionCache := tls.NewLRUClientSessionCache(claudeCodeSessionCacheCapacity)
	var dialer proxy.Dialer = proxy.Direct
	if proxyURL != "" {
		proxyDialer, mode, errBuild := proxyutil.BuildDialer(proxyURL)
		if errBuild != nil {
			log.Errorf("claude tls: failed to configure proxy dialer for %q: %v", proxyutil.Redact(proxyURL), errBuild)
		} else if mode != proxyutil.ModeInherit && proxyDialer != nil {
			dialer = proxyDialer
		}
	}

	transport := &http.Transport{
		ForceAttemptHTTP2: false,
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var (
				conn net.Conn
				err  error
			)
			if contextDialer, ok := dialer.(proxy.ContextDialer); ok {
				conn, err = contextDialer.DialContext(ctx, network, addr)
			} else {
				conn, err = dialer.Dial(network, addr)
			}
			if err != nil {
				return nil, fmt.Errorf("claude tls: dial upstream: %w", err)
			}

			host, _, errSplit := net.SplitHostPort(addr)
			if errSplit != nil {
				if errClose := conn.Close(); errClose != nil {
					log.Debugf("claude tls: close failed connection: %v", errClose)
				}
				return nil, fmt.Errorf("claude tls: split upstream address: %w", errSplit)
			}
			tlsConn := tls.UClient(conn, newClaudeCodeTLSConfig(host, sessionCache), tls.HelloCustom)
			if errPreset := tlsConn.ApplyPreset(claudeCodeTLSClientHelloSpec()); errPreset != nil {
				if errClose := tlsConn.Close(); errClose != nil {
					log.Debugf("claude tls: close connection after preset failure: %v", errClose)
				}
				return nil, fmt.Errorf("claude tls: apply Claude Code ClientHello: %w", errPreset)
			}
			if errHandshake := tlsConn.HandshakeContext(ctx); errHandshake != nil {
				if errClose := tlsConn.Close(); errClose != nil {
					log.Debugf("claude tls: close connection after handshake failure: %v", errClose)
				}
				return nil, fmt.Errorf("claude tls: handshake upstream: %w", errHandshake)
			}
			return httpwire.NewOrderedRequestConn(tlsConn, claudeCodeRequestHeaderOrder), nil
		},
	}
	return transport
}

// fallbackRoundTripper uses provider-specific TLS fingerprints for protected
// HTTPS hosts and falls back to the standard transport for all other requests.
type fallbackRoundTripper struct {
	anthropic http.RoundTripper
	codex     http.RoundTripper
	fallback  http.RoundTripper
}

func (f *fallbackRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if IsAnthropicUpstreamURL(req.URL) {
		return f.anthropic.RoundTrip(req)
	}
	if req.URL.Scheme == "https" && strings.EqualFold(req.URL.Hostname(), "chatgpt.com") {
		return f.codex.RoundTrip(req)
	}
	return f.fallback.RoundTrip(req)
}

func (f *fallbackRoundTripper) CloseIdleConnections() {
	for _, roundTripper := range []http.RoundTripper{f.anthropic, f.codex, f.fallback} {
		if roundTripper == nil {
			continue
		}
		if closer, ok := roundTripper.(interface{ CloseIdleConnections() }); ok {
			closer.CloseIdleConnections()
		}
	}
}

// NewUtlsRoundTripper builds a long-lived RoundTripper that applies
// provider-specific TLS fingerprints to protected hosts (Claude Code's
// Node/OpenSSL profile for Anthropic, Codex CLI 0.147.0 for chatgpt.com) and routes
// everything else through fallback. Unlike NewUtlsHTTPClient it returns the
// RoundTripper directly so callers that maintain their own per-auth client
// cache (e.g. the Codex executor) can hold onto it and reuse its connection
// pool across requests for a single account. fallback is used for
// non-protected hosts; when nil it defaults to http.DefaultTransport.
func NewUtlsRoundTripper(proxyURL string, fallback http.RoundTripper) http.RoundTripper {
	if fallback == nil {
		fallback = http.DefaultTransport
	}
	proxyURL = strings.TrimSpace(proxyURL)
	return &fallbackRoundTripper{
		anthropic: cachedClaudeCodeRoundTripper(proxyURL),
		codex:     newCodexCLIHTTPRoundTripper(proxyURL),
		fallback:  fallback,
	}
}

// NewUtlsHTTPClient creates an HTTP client using provider-specific TLS
// fingerprints for protected hosts. It uses Claude Code's Node/OpenSSL profile
// for Anthropic and a Codex CLI 0.147.0 profile for ChatGPT, with a standard-transport
// fallback for other hosts.
func NewUtlsHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	var proxyURL string
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}
	if proxyURL == "" && cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}

	var ctxRoundTripper http.RoundTripper
	if ctx != nil {
		ctxRoundTripper, _ = ctx.Value("cliproxy.roundtripper").(http.RoundTripper)
	}

	var codexRT http.RoundTripper = newCodexCLIHTTPRoundTripper(proxyURL)
	var anthropicRT http.RoundTripper = cachedClaudeCodeRoundTripper(proxyURL)
	var standardTransport http.RoundTripper = http.DefaultTransport
	if proxyURL != "" {
		if transport := buildProxyTransport(proxyURL); transport != nil {
			standardTransport = transport
		}
	} else if ctxRoundTripper != nil {
		codexRT = ctxRoundTripper
		anthropicRT = ctxRoundTripper
		standardTransport = ctxRoundTripper
	}

	client := &http.Client{
		Transport: &fallbackRoundTripper{
			anthropic: anthropicRT,
			codex:     codexRT,
			fallback:  standardTransport,
		},
	}
	if timeout > 0 {
		client.Timeout = timeout
	}
	return client
}
