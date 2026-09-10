package config

import "testing"

func TestParseConfigBytesAccountConcurrencyLimit(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte("account-concurrency-limit: 5\n"))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}
	if cfg.AccountConcurrencyLimit != 5 {
		t.Fatalf("AccountConcurrencyLimit = %d, want 5", cfg.AccountConcurrencyLimit)
	}
}

func TestParseConfigBytesAccountConcurrencyLimitDefaultsToDisabled(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte("{}\n"))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}
	if cfg.AccountConcurrencyLimit != 0 {
		t.Fatalf("AccountConcurrencyLimit = %d, want 0", cfg.AccountConcurrencyLimit)
	}
}

func TestParseConfigBytesAccountConcurrencyLimitClampsNegative(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte("account-concurrency-limit: -1\n"))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}
	if cfg.AccountConcurrencyLimit != 0 {
		t.Fatalf("AccountConcurrencyLimit = %d, want 0", cfg.AccountConcurrencyLimit)
	}
}
