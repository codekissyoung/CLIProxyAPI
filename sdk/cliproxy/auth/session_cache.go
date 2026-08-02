package auth

import (
	"strings"
	"sync"
	"time"
)

const maxStableSessionAliases = 64

// sessionEntry stores an auth binding, its identifier aliases, and expiration.
type sessionEntry struct {
	authID    string
	expiresAt time.Time
	aliases   []string
}

// SessionCache provides TTL-based session to auth mapping with automatic cleanup.
type SessionCache struct {
	mu                   sync.RWMutex
	entries              map[string]sessionEntry
	bindingCounts        map[string]int
	bindingCountObserver func(authID string, count int)
	ttl                  time.Duration
	stopCh               chan struct{}
	stopped              bool
}

// NewSessionCache creates a cache with the specified TTL.
// A background goroutine periodically cleans expired entries.
func NewSessionCache(ttl time.Duration) *SessionCache {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	c := &SessionCache{
		entries:       make(map[string]sessionEntry),
		bindingCounts: make(map[string]int),
		ttl:           ttl,
		stopCh:        make(chan struct{}),
	}
	go c.cleanupLoop()
	return c
}

// SetBindingCountObserver registers a callback for absolute logical-binding
// counts. Alias keys for one session are reported as a single binding.
func (c *SessionCache) SetBindingCountObserver(observer func(authID string, count int)) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.bindingCountObserver = observer
	counts := make(map[string]int, len(c.bindingCounts))
	for authID, count := range c.bindingCounts {
		counts[authID] = count
	}
	c.mu.Unlock()
	if observer != nil {
		for authID, count := range counts {
			observer(authID, count)
		}
	}
}

// BindingCount returns the number of logical session groups bound to an auth.
func (c *SessionCache) BindingCount(authID string) int {
	if c == nil || authID == "" {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.bindingCounts[authID]
}

// Get retrieves the auth ID bound to a session, if still valid.
// Does NOT refresh the TTL on access.
func (c *SessionCache) Get(sessionID string) (string, bool) {
	if sessionID == "" {
		return "", false
	}
	now := time.Now()
	c.mu.RLock()
	if c.stopped {
		c.mu.RUnlock()
		return "", false
	}
	entry, ok := c.entries[sessionID]
	if ok && now.Before(entry.expiresAt) {
		c.mu.RUnlock()
		return entry.authID, true
	}
	c.mu.RUnlock()
	if !ok {
		return "", false
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok = c.entries[sessionID]
	if !ok {
		return "", false
	}
	if time.Now().Before(entry.expiresAt) {
		return entry.authID, true
	}
	c.removeAliasGroupLocked(entry)
	return "", false
}

// GetAndRefresh retrieves the auth ID bound to a session and refreshes the TTL
// for every identifier known to represent the same logical session.
func (c *SessionCache) GetAndRefresh(sessionID string) (string, bool) {
	if sessionID == "" {
		return "", false
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return "", false
	}
	entry, ok := c.entries[sessionID]
	if !ok {
		return "", false
	}
	if !now.Before(entry.expiresAt) {
		c.removeAliasGroupLocked(entry)
		return "", false
	}

	aliases := compactSessionAliases(mergeSessionAliases([]string{sessionID}, entry.aliases...))
	c.replaceAliasGroupsLocked(entry.authID, now.Add(c.ttl), aliases, entry)
	return entry.authID, true
}

// Set binds a session to an auth ID with TTL refresh. Existing aliases for the
// same logical session remain attached when the binding is refreshed or moved.
func (c *SessionCache) Set(sessionID, authID string) {
	c.SetAliases(authID, sessionID)
}

// SetAliases binds multiple identifiers for one logical session to an auth ID.
func (c *SessionCache) SetAliases(authID string, sessionIDs ...string) {
	if authID == "" {
		return
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return
	}

	aliases := mergeSessionAliases(nil, sessionIDs...)
	previousGroups := make([]sessionEntry, 0, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		entry, ok := c.entries[sessionID]
		if !ok {
			continue
		}
		if !now.Before(entry.expiresAt) {
			c.removeAliasGroupLocked(entry)
			continue
		}
		previousGroups = append(previousGroups, entry)
		aliases = mergeSessionAliases(aliases, entry.aliases...)
	}
	aliases = compactSessionAliases(aliases)
	if len(aliases) == 0 {
		return
	}
	c.replaceAliasGroupsLocked(authID, now.Add(c.ttl), aliases, previousGroups...)
}

func (c *SessionCache) replaceAliasGroupsLocked(authID string, expiresAt time.Time, aliases []string, previousGroups ...sessionEntry) {
	for _, previous := range previousGroups {
		c.removeAliasGroupLocked(previous)
	}
	entry := sessionEntry{authID: authID, expiresAt: expiresAt, aliases: aliases}
	for _, alias := range aliases {
		c.entries[alias] = entry
	}
	c.adjustBindingCountLocked(authID, 1)
}

func (c *SessionCache) removeAliasGroupLocked(entry sessionEntry) bool {
	matched := false
	for _, alias := range entry.aliases {
		current, ok := c.entries[alias]
		if !ok || current.authID != entry.authID || !current.expiresAt.Equal(entry.expiresAt) ||
			!equalSessionAliases(current.aliases, entry.aliases) {
			continue
		}
		delete(c.entries, alias)
		matched = true
	}
	if matched {
		c.adjustBindingCountLocked(entry.authID, -1)
	}
	return matched
}

func (c *SessionCache) adjustBindingCountLocked(authID string, delta int) {
	if authID == "" || delta == 0 {
		return
	}
	count := c.bindingCounts[authID] + delta
	if count <= 0 {
		delete(c.bindingCounts, authID)
		count = 0
	} else {
		c.bindingCounts[authID] = count
	}
	if c.bindingCountObserver != nil {
		c.bindingCountObserver(authID, count)
	}
}

func compactSessionAliases(aliases []string) []string {
	return compactSessionAliasesWith(aliases, isLocalPromptCacheSessionAlias)
}

func compactHomeSessionAliases(aliases []string) []string {
	return compactSessionAliasesWith(aliases, func(alias string) bool {
		return strings.HasPrefix(alias, "pck:")
	})
}

func compactSessionAliasesWith(aliases []string, isPromptCacheAlias func(string) bool) []string {
	compacted := make([]string, 0, len(aliases))
	hasPromptCacheKey := false
	stableAliases := 0
	for _, alias := range aliases {
		if isPromptCacheAlias(alias) {
			if hasPromptCacheKey {
				continue
			}
			hasPromptCacheKey = true
		} else {
			if stableAliases >= maxStableSessionAliases {
				continue
			}
			stableAliases++
		}
		compacted = append(compacted, alias)
	}
	return compacted
}

func isLocalPromptCacheSessionAlias(alias string) bool {
	if strings.HasPrefix(alias, "pck:") {
		return true
	}
	_, sessionAndModel, ok := strings.Cut(alias, "::")
	return ok && strings.HasPrefix(sessionAndModel, "pck:")
}

func equalSessionAliases(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func mergeSessionAliases(existing []string, candidates ...string) []string {
	aliases := make([]string, 0, len(existing)+len(candidates))
	seen := make(map[string]struct{}, cap(aliases))
	add := func(alias string) {
		if alias == "" {
			return
		}
		if _, ok := seen[alias]; ok {
			return
		}
		seen[alias] = struct{}{}
		aliases = append(aliases, alias)
	}
	for _, alias := range existing {
		add(alias)
	}
	for _, alias := range candidates {
		add(alias)
	}
	return aliases
}

// Invalidate removes a specific session binding without allowing another alias
// in the same group to recreate it on its next refresh.
func (c *SessionCache) Invalidate(sessionID string) {
	if sessionID == "" {
		return
	}
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	entry, ok := c.entries[sessionID]
	if ok {
		if len(entry.aliases) <= 1 {
			c.removeAliasGroupLocked(entry)
			c.mu.Unlock()
			return
		}
		delete(c.entries, sessionID)
		for _, alias := range entry.aliases {
			if alias == sessionID {
				continue
			}
			current, exists := c.entries[alias]
			if !exists || current.authID != entry.authID {
				continue
			}
			filtered := make([]string, 0, len(current.aliases))
			for _, candidate := range current.aliases {
				if candidate != sessionID {
					filtered = append(filtered, candidate)
				}
			}
			current.aliases = filtered
			c.entries[alias] = current
		}
	}
	c.mu.Unlock()
}

// InvalidateGroup removes the entire logical session group containing sessionID.
func (c *SessionCache) InvalidateGroup(sessionID string) {
	if sessionID == "" {
		return
	}
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	if entry, ok := c.entries[sessionID]; ok {
		c.removeAliasGroupLocked(entry)
	}
	c.mu.Unlock()
}

// InvalidateAuth removes all sessions bound to a specific auth ID.
// Used when an auth is permanently disabled or removed.
func (c *SessionCache) InvalidateAuth(authID string) {
	if authID == "" {
		return
	}
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	for _, entry := range c.entries {
		if entry.authID == authID {
			c.removeAliasGroupLocked(entry)
		}
	}
	c.mu.Unlock()
}

// Stop terminates the background cleanup goroutine.
func (c *SessionCache) Stop() {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	c.stopped = true
	close(c.stopCh)
	for _, entry := range c.entries {
		c.removeAliasGroupLocked(entry)
	}
	c.mu.Unlock()
}

func (c *SessionCache) cleanupLoop() {
	interval := c.ttl / 2
	if interval <= 0 {
		interval = time.Nanosecond
	}
	if interval > 5*time.Minute {
		interval = 5 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.cleanup()
		}
	}
}

func (c *SessionCache) cleanup() {
	now := time.Now()
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	for _, entry := range c.entries {
		if !now.Before(entry.expiresAt) {
			c.removeAliasGroupLocked(entry)
		}
	}
	c.mu.Unlock()
}
