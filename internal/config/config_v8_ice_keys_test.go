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
	raw := "api-keys: [k]\naccount-concurrency-limit: 5\nxai-oauth-max-concurrency: 3\nrequest-retry: 1\n"
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
