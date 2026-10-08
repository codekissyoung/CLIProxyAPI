package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// ice divergence regression: the fork's top-level keys (per-credential
// concurrency gates) are not part of upstream's v8 layout. Between 2026-10-01
// and 2026-10-08 the v8 migration "commented out" both keys on every load, so
// account-concurrency-limit: 5 decoded as 0 and the gate was silently off.
func TestV8MigrationKeepsIceTopLevelKeys(t *testing.T) {
	raw := "api-keys: [k]\naccount-concurrency-limit: 5\nxai-oauth-max-concurrency: 3\nallow-pool-pin-header: true\ncodex:\n  model-level-cooling: true\nrequest-retry: 1\n"
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var warned []string
	SetV8MigrationWarnFunc(func(section, msg string) {
		mu.Lock()
		defer mu.Unlock()
		warned = append(warned, section)
	})
	defer SetV8MigrationWarnFunc(nil)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AccountConcurrencyLimit != 5 {
		t.Fatalf("AccountConcurrencyLimit = %d, want 5 (key dropped by v8 migration?)", cfg.AccountConcurrencyLimit)
	}
	if cfg.XAIOAuthMaxConcurrency != 3 {
		t.Fatalf("XAIOAuthMaxConcurrency = %d, want 3 (key dropped by v8 migration?)", cfg.XAIOAuthMaxConcurrency)
	}
	if !cfg.AllowPoolPinHeader {
		t.Fatal("AllowPoolPinHeader = false, want true (key dropped by v8 migration?)")
	}
	if !cfg.Codex.ModelLevelCooling {
		t.Fatal("Codex.ModelLevelCooling = false, want true (codex block dropped by v8 migration?)")
	}
	// The migrate=true pass (management save/preview) must also keep the fork keys.
	out, _, err := NormalizeConfigLayout([]byte(raw), true)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"account-concurrency-limit: 5", "xai-oauth-max-concurrency: 3", "allow-pool-pin-header: true", "model-level-cooling: true"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("migrate=true output lost %q:\n%s", want, out)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, section := range warned {
		for _, key := range iceV8TopLevelKeys {
			if section == key || strings.HasSuffix(section, "."+key) {
				t.Fatalf("v8 migration warned about ice key %q; it must be an allowed root", section)
			}
		}
	}
	allowed := v8AllowedRoots()
	for _, key := range iceV8TopLevelKeys {
		if !allowed[key] {
			t.Fatalf("v8AllowedRoots() lacks ice key %q", key)
		}
	}
}
