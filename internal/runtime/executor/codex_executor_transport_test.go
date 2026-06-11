package executor

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestCodexHTTPClient_RetiresStaleProxyTransports(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "transport-test-auth"}
	defer EvictCodexTransportsForAuthID(auth.ID)
	ctx := context.Background()

	cfgA := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://127.0.0.1:18080"}}
	if client := codexHTTPClient(ctx, cfgA, auth); client == nil {
		t.Fatal("codexHTTPClient returned nil client")
	}
	keyA := auth.ID + "|" + cfgA.ProxyURL
	cachedA, ok := codexTransportCache.Load(keyA)
	if !ok {
		t.Fatalf("expected cached transport for key %q", keyA)
	}
	// The cached entry must carry both the utls-wrapped RoundTripper handed to
	// callers and the underlying *http.Transport used for idle-conn cleanup.
	entry, okEntry := cachedA.(*codexCachedTransport)
	if !okEntry || entry == nil || entry.rt == nil || entry.base == nil {
		t.Fatalf("cached entry has unexpected shape: %#v", cachedA)
	}

	cfgB := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://127.0.0.1:18081"}}
	if client := codexHTTPClient(ctx, cfgB, auth); client == nil {
		t.Fatal("codexHTTPClient returned nil client")
	}
	keyB := auth.ID + "|" + cfgB.ProxyURL
	if _, ok := codexTransportCache.Load(keyB); !ok {
		t.Fatalf("expected cached transport for key %q", keyB)
	}
	if _, ok := codexTransportCache.Load(keyA); ok {
		t.Fatalf("expected stale transport %q to be retired after proxy change", keyA)
	}
}

func TestEvictCodexTransportsForAuthID(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "evict-test-auth"}
	other := &cliproxyauth.Auth{ID: "evict-test-other"}
	defer EvictCodexTransportsForAuthID(other.ID)
	ctx := context.Background()
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://127.0.0.1:18080"}}

	_ = codexHTTPClient(ctx, cfg, auth)
	_ = codexHTTPClient(ctx, cfg, other)

	EvictCodexTransportsForAuthID(auth.ID)
	if _, ok := codexTransportCache.Load(auth.ID + "|" + cfg.ProxyURL); ok {
		t.Fatal("expected all transports for evicted auth to be removed")
	}
	if _, ok := codexTransportCache.Load(other.ID + "|" + cfg.ProxyURL); !ok {
		t.Fatal("eviction must not touch other auths' transports")
	}

	EvictCodexTransportsForAuthID("")
}

func TestCollectCodexOutputItemDone_ReturnsRetainedBytes(t *testing.T) {
	byIndex := make(map[int64][]byte)
	var fallback [][]byte

	item := `{"type":"message","content":"hi"}`
	indexed := []byte(`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`)
	if got := collectCodexOutputItemDone(indexed, byIndex, &fallback); got != len(item) {
		t.Fatalf("indexed item retained = %d, want %d", got, len(item))
	}
	if len(byIndex) != 1 {
		t.Fatalf("byIndex size = %d, want 1", len(byIndex))
	}

	unindexed := []byte(`{"type":"response.output_item.done","item":` + item + `}`)
	if got := collectCodexOutputItemDone(unindexed, byIndex, &fallback); got != len(item) {
		t.Fatalf("fallback item retained = %d, want %d", got, len(item))
	}
	if len(fallback) != 1 {
		t.Fatalf("fallback size = %d, want 1", len(fallback))
	}

	noItem := []byte(`{"type":"response.output_item.done","output_index":1}`)
	if got := collectCodexOutputItemDone(noItem, byIndex, &fallback); got != 0 {
		t.Fatalf("missing item retained = %d, want 0", got)
	}
}
