package helps

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

// CodexContextRejectCache remembers request fingerprints that the upstream has
// already rejected for being over the context window. A subsequent request with
// the EXACT same semantic content can then be failed fast without a wasted
// upstream round-trip.
//
// The fingerprint is computed over only the stable, size-determining fields
// (model + instructions + input array), deliberately excluding volatile
// per-turn metadata (prompt_cache_key, client_metadata, previous_response_id,
// etc.). This gives a zero-false-positive guarantee: if the user changes
// anything that affects the input (shortens the conversation, /clear, edits a
// message), the fingerprint changes and the request is no longer blocked.
type CodexContextRejectCache struct {
	mu      sync.Mutex
	entries map[string]time.Time // fingerprint -> expiry
	ttl     time.Duration
}

// NewCodexContextRejectCache creates a cache with the given TTL.
func NewCodexContextRejectCache(ttl time.Duration) *CodexContextRejectCache {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &CodexContextRejectCache{
		entries: make(map[string]time.Time),
		ttl:     ttl,
	}
}

// CodexContextFingerprint returns a stable fingerprint of a codex-format request
// body, derived only from model + instructions + the input array. Returns "" if
// there is nothing meaningful to fingerprint (so callers can skip caching).
func CodexContextFingerprint(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	model := gjson.GetBytes(body, "model").String()
	instructions := gjson.GetBytes(body, "instructions").String()
	input := gjson.GetBytes(body, "input")
	if !input.Exists() {
		return ""
	}
	h := sha256.New()
	h.Write([]byte("model:"))
	h.Write([]byte(model))
	h.Write([]byte("\ninstructions:"))
	h.Write([]byte(instructions))
	h.Write([]byte("\ninput:"))
	h.Write([]byte(input.Raw))
	return hex.EncodeToString(h.Sum(nil))
}

// Record marks a fingerprint as upstream-rejected for being over the context
// window. now is passed in for testability.
func (c *CodexContextRejectCache) Record(fingerprint string, now time.Time) {
	if c == nil || fingerprint == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[fingerprint] = now.Add(c.ttl)
	c.evictExpiredLocked(now)
}

// Blocked reports whether a fingerprint is currently remembered as rejected.
// Expired entries are treated as not blocked and cleaned up.
func (c *CodexContextRejectCache) Blocked(fingerprint string, now time.Time) bool {
	if c == nil || fingerprint == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	expiry, ok := c.entries[fingerprint]
	if !ok {
		return false
	}
	if !now.Before(expiry) {
		delete(c.entries, fingerprint)
		return false
	}
	return true
}

// evictExpiredLocked removes expired entries. Caller must hold the lock.
func (c *CodexContextRejectCache) evictExpiredLocked(now time.Time) {
	for k, expiry := range c.entries {
		if !now.Before(expiry) {
			delete(c.entries, k)
		}
	}
}
