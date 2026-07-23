package config

import "testing"

func TestParseConfigBytesXAIOAuthMaxConcurrency(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte("xai-oauth-max-concurrency: 3\n"))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}
	if cfg.XAIOAuthMaxConcurrency != 3 {
		t.Fatalf("XAIOAuthMaxConcurrency = %d, want 3", cfg.XAIOAuthMaxConcurrency)
	}
}

func TestParseConfigBytesXAIOAuthMaxConcurrencyClampsNegative(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte("xai-oauth-max-concurrency: -1\n"))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}
	if cfg.XAIOAuthMaxConcurrency != 0 {
		t.Fatalf("XAIOAuthMaxConcurrency = %d, want 0", cfg.XAIOAuthMaxConcurrency)
	}
}
