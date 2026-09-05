package auth

import (
	"container/list"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	maxStableSessionAliases  = 64
	defaultMaxSessionEntries = 65536
)

// sessionEntry stores an auth binding, its identifier aliases, and expiration.
type sessionEntry struct {
	authID    string
	expiresAt time.Time
	aliases   []string
}

// SessionCache provides TTL-based session to auth mapping with automatic cleanup.
type SessionCache struct {
	mu               sync.RWMutex
	entries          map[string]sessionEntry
	groups           map[string]sessionEntry
	evictionOrder    *list.List
	evictionElements map[string]*list.Element
	maxEntries       int
	// ice divergence: binding counting + stopped guards are local-only features;
	// upstream owns the LRU fields above. Keep both on merge. docs/ice-divergences.md
	bindingCounts        map[string]int
	bindingCountObserver func(authID string, count int)
	ttl                  time.Duration
	stopCh               chan struct{}
	stopped              bool
	stopOnce             sync.Once
}

// NewSessionCache creates a cache with the specified TTL.
// A background goroutine periodically cleans expired entries.
func NewSessionCache(ttl time.Duration) *SessionCache {
	return NewSessionCacheWithCapacity(ttl, defaultMaxSessionEntries)
}

// NewSessionCacheWithCapacity creates a cache with the specified TTL and max entries limit.
func NewSessionCacheWithCapacity(ttl time.Duration, maxEntries int) *SessionCache {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	if maxEntries <= 0 {
		maxEntries = defaultMaxSessionEntries
	}
	c := &SessionCache{
		entries:          make(map[string]sessionEntry),
		groups:           make(map[string]sessionEntry),
		evictionOrder:    list.New(),
		evictionElements: make(map[string]*list.Element),
		maxEntries:       maxEntries,
		bindingCounts:    make(map[string]int),
		ttl:              ttl,
		stopCh:           make(chan struct{}),
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

func (c *SessionCache) ensureInitializedLocked() {
	if c.entries == nil {
		c.entries = make(map[string]sessionEntry)
	}
	if c.groups == nil {
		c.groups = make(map[string]sessionEntry)
	}
	if c.evictionOrder == nil {
		c.evictionOrder = list.New()
	}
	if c.evictionElements == nil {
		c.evictionElements = make(map[string]*list.Element)
	}
	if c.bindingCounts == nil {
		c.bindingCounts = make(map[string]int)
	}
}

// Get retrieves the auth ID bound to a session, if still valid.
// Does NOT refresh the TTL on access.
func (c *SessionCache) Get(sessionID string) (string, bool) {
	if c == nil || sessionID == "" {
		return "", false
	}
	c.mu.RLock()
	if c.stopped {
		c.mu.RUnlock()
		return "", false
	}
	now := time.Now()
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
	c.ensureInitializedLocked()
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
	if c == nil || sessionID == "" {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return "", false
	}
	c.ensureInitializedLocked()
	entry, ok := c.entries[sessionID]
	if !ok {
		return "", false
	}
	now := time.Now()
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
	if c == nil {
		return
	}
	c.SetAliases(authID, sessionID)
}

// SetAliases binds multiple identifiers for one logical session to an auth ID.
func (c *SessionCache) SetAliases(authID string, sessionIDs ...string) {
	if c == nil || authID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return
	}
	c.ensureInitializedLocked()
	now := time.Now()

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
	if len(aliases) == 0 {
		return
	}
	primaryKey := aliases[0]
	if existing, ok := c.groups[primaryKey]; ok {
		c.removeAliasGroupLocked(existing)
	}
	entry := sessionEntry{authID: authID, expiresAt: expiresAt, aliases: append([]string(nil), aliases...)}
	c.groups[primaryKey] = entry
	for _, alias := range aliases {
		c.entries[alias] = entry
	}
	c.adjustBindingCountLocked(authID, 1)
	c.evictionElements[primaryKey] = c.evictionOrder.PushBack(primaryKey)
	if c.maxEntries > 0 && len(c.entries) > c.maxEntries {
		c.evictExcessLocked()
	}
}

func (c *SessionCache) evictExcessLocked() {
	for len(c.entries) > c.maxEntries {
		oldest := c.evictionOrder.Front()
		if oldest == nil {
			break
		}
		primaryKey, _ := oldest.Value.(string)
		group, ok := c.groups[primaryKey]
		if !ok {
			c.evictionOrder.Remove(oldest)
			delete(c.evictionElements, primaryKey)
			continue
		}
		// Eviction silently drops a live session binding: that session loses its
		// home-affinity assignment and is re-picked on its next request. Log it so
		// operators can distinguish eviction from TTL expiry and judge maxEntries.
		log.WithFields(log.Fields{
			"auth_id":     group.authID,
			"entries":     len(c.entries),
			"max_entries": c.maxEntries,
		}).Warn("session cache evicted oldest bound session; its home affinity will be reassigned")
		c.removeAliasGroupLocked(group)
	}
}

func (c *SessionCache) removeAliasGroupLocked(entry sessionEntry) bool {
	if len(entry.aliases) == 0 {
		return false
	}
	matched := false
	primaryKey := entry.aliases[0]
	if currentGroup, ok := c.groups[primaryKey]; ok && sameSessionEntryGroup(currentGroup, entry) {
		delete(c.groups, primaryKey)
		if elem, ok := c.evictionElements[primaryKey]; ok {
			c.evictionOrder.Remove(elem)
			delete(c.evictionElements, primaryKey)
		}
		matched = true
	}
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

func sameSessionEntryGroup(left, right sessionEntry) bool {
	return left.authID == right.authID && left.expiresAt.Equal(right.expiresAt) &&
		equalSessionAliases(left.aliases, right.aliases)
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

// Touch refreshes the expiration for a session binding if it currently matches expectedAuthID.
func (c *SessionCache) Touch(sessionID, expectedAuthID string) bool {
	if c == nil || sessionID == "" || expectedAuthID == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return false
	}
	c.ensureInitializedLocked()
	now := time.Now()
	entry, ok := c.entries[sessionID]
	if !ok || entry.authID != expectedAuthID || !now.Before(entry.expiresAt) {
		return false
	}
	aliases := compactSessionAliases(mergeSessionAliases([]string{sessionID}, entry.aliases...))
	c.replaceAliasGroupsLocked(expectedAuthID, now.Add(c.ttl), aliases, entry)
	return true
}

// CompareAndDelete removes a session binding only when it still matches expectedAuthID.
func (c *SessionCache) CompareAndDelete(sessionID, expectedAuthID string) bool {
	if c == nil || sessionID == "" || expectedAuthID == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return false
	}
	c.ensureInitializedLocked()
	entry, ok := c.entries[sessionID]
	if !ok || entry.authID != expectedAuthID {
		return false
	}
	c.removeAliasGroupLocked(entry)

	surviving := make([]string, 0, len(entry.aliases))
	for _, alias := range entry.aliases {
		if alias != sessionID {
			surviving = append(surviving, alias)
		}
	}
	if len(surviving) > 0 {
		c.replaceAliasGroupsLocked(entry.authID, entry.expiresAt, surviving)
	}
	return true
}

// Invalidate removes a specific session binding without allowing another alias
// in the same group to recreate it on its next refresh.
func (c *SessionCache) Invalidate(sessionID string) {
	if c == nil || sessionID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return
	}
	c.ensureInitializedLocked()
	entry, ok := c.entries[sessionID]
	if !ok {
		return
	}
	c.removeAliasGroupLocked(entry)

	surviving := make([]string, 0, len(entry.aliases))
	for _, alias := range entry.aliases {
		if alias != sessionID {
			surviving = append(surviving, alias)
		}
	}
	if len(surviving) > 0 {
		c.replaceAliasGroupsLocked(entry.authID, entry.expiresAt, surviving)
	}
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
	if c == nil || authID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return
	}
	c.ensureInitializedLocked()
	for _, group := range c.groups {
		if group.authID == authID {
			c.removeAliasGroupLocked(group)
		}
	}
}

// Stop terminates the background cleanup goroutine.
func (c *SessionCache) Stop() {
	if c == nil {
		return
	}
	c.stopOnce.Do(func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.stopped = true
		if c.stopCh != nil {
			close(c.stopCh)
		}
		for _, group := range c.groups {
			c.removeAliasGroupLocked(group)
		}
	})
}

func (c *SessionCache) cleanupLoop() {
	interval := c.ttl / 2
	if interval < time.Millisecond {
		interval = time.Millisecond
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

// Len returns the current count of tracked session aliases.
func (c *SessionCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

func (c *SessionCache) cleanup() {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return
	}
	c.ensureInitializedLocked()
	for _, group := range c.groups {
		if !now.Before(group.expiresAt) {
			c.removeAliasGroupLocked(group)
		}
	}
}
