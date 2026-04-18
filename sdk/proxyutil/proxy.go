package proxyutil

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

// Mode describes how a proxy setting should be interpreted.
type Mode int

const (
	// ModeInherit means no explicit proxy behavior was configured.
	ModeInherit Mode = iota
	// ModeDirect means outbound requests must bypass proxies explicitly.
	ModeDirect
	// ModeProxy means a concrete proxy URL was configured.
	ModeProxy
	// ModeInvalid means the proxy setting is present but malformed or unsupported.
	ModeInvalid
)

// Setting is the normalized interpretation of a proxy configuration value.
type Setting struct {
	Raw  string
	Mode Mode
	URL  *url.URL
}

// Parse normalizes a proxy configuration value into inherit, direct, or proxy modes.
func Parse(raw string) (Setting, error) {
	trimmed := strings.TrimSpace(raw)
	setting := Setting{Raw: trimmed}

	if trimmed == "" {
		setting.Mode = ModeInherit
		return setting, nil
	}

	if strings.EqualFold(trimmed, "direct") || strings.EqualFold(trimmed, "none") {
		setting.Mode = ModeDirect
		return setting, nil
	}

	parsedURL, errParse := url.Parse(trimmed)
	if errParse != nil {
		setting.Mode = ModeInvalid
		return setting, fmt.Errorf("parse proxy URL failed: %w", errParse)
	}
	if parsedURL.Scheme == "" || parsedURL.Host == "" {
		setting.Mode = ModeInvalid
		return setting, fmt.Errorf("proxy URL missing scheme/host")
	}

	switch parsedURL.Scheme {
	case "socks5", "socks5h", "http", "https":
		setting.Mode = ModeProxy
		setting.URL = parsedURL
		return setting, nil
	default:
		setting.Mode = ModeInvalid
		return setting, fmt.Errorf("unsupported proxy scheme: %s", parsedURL.Scheme)
	}
}

var ipv4OnlyDialer = &net.Dialer{
	Timeout:   30 * time.Second,
	KeepAlive: 30 * time.Second,
}

// IPv4OnlyDialContext is an http.Transport.DialContext that forces "tcp" to "tcp4".
// Use this anywhere an http.Transport is constructed by hand so outbound connections
// avoid AAAA resolution paths that are subject to Cloudflare/VPS IPv6 risk control.
func IPv4OnlyDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if network == "tcp" {
		network = "tcp4"
	}
	return ipv4OnlyDialer.DialContext(ctx, network, addr)
}

// IPv4OnlyDirect is a drop-in replacement for proxy.Direct that rewrites "tcp" to "tcp4".
// Substitute for proxy.Direct in dialer chains that feed custom transports (e.g. utls).
var IPv4OnlyDirect proxy.Dialer = ipv4OnlyDirectDialer{}

type ipv4OnlyDirectDialer struct{}

func (ipv4OnlyDirectDialer) Dial(network, addr string) (net.Conn, error) {
	if network == "tcp" {
		network = "tcp4"
	}
	return ipv4OnlyDialer.Dial(network, addr)
}

// EnforceIPv4OnlyDefaultTransport mutates http.DefaultTransport in place so every
// http.Client that relies on the default transport dials IPv4 only. Call once at
// program startup, before any HTTP client issues requests.
func EnforceIPv4OnlyDefaultTransport() {
	if transport, ok := http.DefaultTransport.(*http.Transport); ok && transport != nil {
		transport.DialContext = IPv4OnlyDialContext
	}
}

func cloneDefaultTransport() *http.Transport {
	if transport, ok := http.DefaultTransport.(*http.Transport); ok && transport != nil {
		clone := transport.Clone()
		clone.DialContext = IPv4OnlyDialContext
		return clone
	}
	return &http.Transport{DialContext: IPv4OnlyDialContext}
}

// NewDirectTransport returns a transport that bypasses environment proxies.
func NewDirectTransport() *http.Transport {
	clone := cloneDefaultTransport()
	clone.Proxy = nil
	return clone
}

// BuildHTTPTransport constructs an HTTP transport for the provided proxy setting.
func BuildHTTPTransport(raw string) (*http.Transport, Mode, error) {
	setting, errParse := Parse(raw)
	if errParse != nil {
		return nil, setting.Mode, errParse
	}

	switch setting.Mode {
	case ModeInherit:
		return nil, setting.Mode, nil
	case ModeDirect:
		return NewDirectTransport(), setting.Mode, nil
	case ModeProxy:
		if setting.URL.Scheme == "socks5" || setting.URL.Scheme == "socks5h" {
			var proxyAuth *proxy.Auth
			if setting.URL.User != nil {
				username := setting.URL.User.Username()
				password, _ := setting.URL.User.Password()
				proxyAuth = &proxy.Auth{User: username, Password: password}
			}
			dialer, errSOCKS5 := proxy.SOCKS5("tcp", setting.URL.Host, proxyAuth, proxy.Direct)
			if errSOCKS5 != nil {
				return nil, setting.Mode, fmt.Errorf("create SOCKS5 dialer failed: %w", errSOCKS5)
			}
			transport := cloneDefaultTransport()
			transport.Proxy = nil
			transport.DialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			}
			return transport, setting.Mode, nil
		}
		transport := cloneDefaultTransport()
		transport.Proxy = http.ProxyURL(setting.URL)
		return transport, setting.Mode, nil
	default:
		return nil, setting.Mode, nil
	}
}

// BuildDialer constructs a proxy dialer for settings that operate at the connection layer.
func BuildDialer(raw string) (proxy.Dialer, Mode, error) {
	setting, errParse := Parse(raw)
	if errParse != nil {
		return nil, setting.Mode, errParse
	}

	switch setting.Mode {
	case ModeInherit:
		return nil, setting.Mode, nil
	case ModeDirect:
		return proxy.Direct, setting.Mode, nil
	case ModeProxy:
		dialer, errDialer := proxy.FromURL(setting.URL, proxy.Direct)
		if errDialer != nil {
			return nil, setting.Mode, fmt.Errorf("create proxy dialer failed: %w", errDialer)
		}
		return dialer, setting.Mode, nil
	default:
		return nil, setting.Mode, nil
	}
}
