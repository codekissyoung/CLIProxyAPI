package helps

import (
	"testing"
	"time"
)

func TestCodexContextFingerprintIgnoresVolatileFields(t *testing.T) {
	// Same model + input, but different volatile per-turn metadata => same fingerprint.
	a := []byte(`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":"hi"}],"prompt_cache_key":"key-A","previous_response_id":"resp-A","client_metadata":{"x-codex-window-id":"w1"}}`)
	b := []byte(`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":"hi"}],"prompt_cache_key":"key-B","previous_response_id":"resp-B","client_metadata":{"x-codex-window-id":"w2"}}`)

	if CodexContextFingerprint(a) != CodexContextFingerprint(b) {
		t.Fatal("fingerprint should ignore volatile fields (prompt_cache_key, etc.)")
	}
}

func TestCodexContextFingerprintChangesWithInput(t *testing.T) {
	a := []byte(`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":"hi"}]}`)
	b := []byte(`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":"hi there"}]}`)
	if CodexContextFingerprint(a) == CodexContextFingerprint(b) {
		t.Fatal("fingerprint must change when input content changes")
	}
}

func TestCodexContextFingerprintEmptyWhenNoInput(t *testing.T) {
	if CodexContextFingerprint([]byte(`{"model":"gpt-5.5"}`)) != "" {
		t.Fatal("expected empty fingerprint when there is no input array")
	}
}

func TestCodexContextRejectCacheRecordThenBlock(t *testing.T) {
	c := NewCodexContextRejectCache(time.Minute)
	now := time.Unix(1_000_000, 0)
	body := []byte(`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":"huge"}]}`)
	fp := CodexContextFingerprint(body)

	if c.Blocked(fp, now) {
		t.Fatal("should not be blocked before recording")
	}
	c.Record(fp, now)
	if !c.Blocked(fp, now) {
		t.Fatal("should be blocked right after recording")
	}
	// A different request (different fingerprint) is not blocked.
	other := CodexContextFingerprint([]byte(`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":"small"}]}`))
	if c.Blocked(other, now) {
		t.Fatal("a different request must not be blocked")
	}
}

func TestCodexContextRejectCacheExpires(t *testing.T) {
	c := NewCodexContextRejectCache(time.Minute)
	now := time.Unix(2_000_000, 0)
	fp := "fp-x"
	c.Record(fp, now)
	if !c.Blocked(fp, now.Add(30*time.Second)) {
		t.Fatal("should still be blocked within TTL")
	}
	if c.Blocked(fp, now.Add(2*time.Minute)) {
		t.Fatal("should no longer be blocked after TTL expiry")
	}
}

func TestCodexContextRejectCacheEmptyFingerprintNoop(t *testing.T) {
	c := NewCodexContextRejectCache(time.Minute)
	now := time.Unix(3_000_000, 0)
	c.Record("", now)
	if c.Blocked("", now) {
		t.Fatal("empty fingerprint must never block")
	}
}
