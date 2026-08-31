package proxy

import (
	"strings"
	"sync"
	"time"
)

const (
	reasoningCacheMaxEntries = 4096
	reasoningCacheTTL        = 6 * time.Hour
)

type reasoningCacheEntry struct {
	reasoning string
	created   time.Time
}

type reasoningContentCache struct {
	mu      sync.Mutex
	entries map[string]reasoningCacheEntry
}

var defaultReasoningContentCache = &reasoningContentCache{
	entries: make(map[string]reasoningCacheEntry),
}

func cacheReasoningForToolCalls(reasoning string, toolIDs ...string) {
	defaultReasoningContentCache.store(reasoning, strings.TrimSpace(reasoning) != "", toolIDs...)
}

func cachedReasoningForToolCalls(toolIDs []string) string {
	reasoning, _ := defaultReasoningContentCache.lookup(toolIDs)
	return reasoning
}

func cacheReasoningStateForToolCalls(reasoning string, present bool, toolIDs ...string) {
	defaultReasoningContentCache.store(reasoning, present, toolIDs...)
}

func cachedReasoningStateForToolCalls(toolIDs []string) (string, bool) {
	return defaultReasoningContentCache.lookup(toolIDs)
}

func resetReasoningContentCacheForTest() {
	defaultReasoningContentCache.mu.Lock()
	defer defaultReasoningContentCache.mu.Unlock()
	defaultReasoningContentCache.entries = make(map[string]reasoningCacheEntry)
}

func (c *reasoningContentCache) store(reasoning string, present bool, toolIDs ...string) {
	reasoning = strings.TrimSpace(reasoning)
	if !present || len(toolIDs) == 0 {
		return
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked(now)
	for _, id := range toolIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		c.entries[id] = reasoningCacheEntry{reasoning: reasoning, created: now}
	}
	if len(c.entries) > reasoningCacheMaxEntries {
		c.pruneOverflowLocked()
	}
}

func (c *reasoningContentCache) lookup(toolIDs []string) (string, bool) {
	if len(toolIDs) == 0 {
		return "", false
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked(now)
	for _, id := range toolIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if entry, ok := c.entries[id]; ok {
			return entry.reasoning, true
		}
	}
	return "", false
}

func (c *reasoningContentCache) pruneLocked(now time.Time) {
	for id, entry := range c.entries {
		if now.Sub(entry.created) > reasoningCacheTTL {
			delete(c.entries, id)
		}
	}
}

func (c *reasoningContentCache) pruneOverflowLocked() {
	for len(c.entries) > reasoningCacheMaxEntries {
		var oldestID string
		var oldestTime time.Time
		for id, entry := range c.entries {
			if oldestID == "" || entry.created.Before(oldestTime) {
				oldestID = id
				oldestTime = entry.created
			}
		}
		if oldestID == "" {
			return
		}
		delete(c.entries, oldestID)
	}
}
