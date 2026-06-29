package proxy

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	ResponseCacheDefaultTTL        = 1 * time.Hour
	ResponseCacheDefaultMaxEntries = 256
	ResponseCacheCleanupInterval   = 5 * time.Minute
)

type ResponseCacheEntry struct {
	Model        string
	PromptTokens int
	Body         []byte
	CreatedAt    time.Time
	ExpiresAt    time.Time
}

type ResponseCache struct {
	entries    sync.Map
	enabled    atomic.Bool
	ttl        atomic.Int64
	maxEntries int
	hits       atomic.Int64
	misses     atomic.Int64
	started    atomic.Bool
	stop       chan struct{}
	once       sync.Once
}

func NewResponseCache(ttl time.Duration, maxEntries int) *ResponseCache {
	if ttl <= 0 {
		ttl = ResponseCacheDefaultTTL
	}
	if maxEntries <= 0 {
		maxEntries = ResponseCacheDefaultMaxEntries
	}
	c := &ResponseCache{
		ttl:        atomic.Int64{},
		maxEntries: maxEntries,
		stop:       make(chan struct{}),
	}
	c.ttl.Store(int64(ttl))
	c.enabled.Store(true)
	return c
}

func (c *ResponseCache) Enabled() bool { return c.enabled.Load() }

func (c *ResponseCache) SetEnabled(v bool) { c.enabled.Store(v) }

func (c *ResponseCache) TTL() time.Duration { return time.Duration(c.ttl.Load()) }

func (c *ResponseCache) SetTTL(d time.Duration) { c.ttl.Store(int64(d)) }

func (c *ResponseCache) MaxEntries() int { return c.maxEntries }

func (c *ResponseCache) Start() {
	c.once.Do(func() {
		c.started.Store(true)
		go c.cleanupLoop()
	})
}

func (c *ResponseCache) Stop() {
	if c.started.Load() {
		close(c.stop)
	}
}

func (c *ResponseCache) Get(key string) (*ResponseCacheEntry, bool) {
	if !c.enabled.Load() || key == "" {
		c.misses.Add(1)
		return nil, false
	}
	val, ok := c.entries.Load(key)
	if !ok {
		c.misses.Add(1)
		return nil, false
	}
	entry := val.(*ResponseCacheEntry)
	if time.Now().After(entry.ExpiresAt) {
		c.entries.Delete(key)
		c.misses.Add(1)
		return nil, false
	}
	c.hits.Add(1)
	return entry, true
}

func (c *ResponseCache) Set(key, model string, promptTokens int, body []byte) {
	if !c.enabled.Load() || key == "" || len(body) == 0 {
		return
	}
	now := time.Now()
	entry := &ResponseCacheEntry{
		Model:        model,
		PromptTokens: promptTokens,
		Body:         body,
		CreatedAt:    now,
		ExpiresAt:    now.Add(c.TTL()),
	}
	c.entries.Store(key, entry)
	if c.entryCount() > c.maxEntries {
		go c.evictOldest(1)
	}
}

func (c *ResponseCache) SetWithExpiry(key, model string, promptTokens int, body []byte, createdAt, expiresAt time.Time) {
	if !c.enabled.Load() || key == "" || len(body) == 0 {
		return
	}
	entry := &ResponseCacheEntry{
		Model:        model,
		PromptTokens: promptTokens,
		Body:         body,
		CreatedAt:    createdAt,
		ExpiresAt:    expiresAt,
	}
	c.entries.Store(key, entry)
	if c.entryCount() > c.maxEntries {
		go c.evictOldest(1)
	}
}

func (c *ResponseCache) Delete(key string) {
	c.entries.Delete(key)
}

func (c *ResponseCache) Clear() {
	c.entries.Range(func(k, _ any) bool {
		c.entries.Delete(k)
		return true
	})
}

func (c *ResponseCache) Stats() (hits, misses int64) {
	return c.hits.Load(), c.misses.Load()
}

func (c *ResponseCache) entryCount() int {
	count := 0
	c.entries.Range(func(_, _ any) bool {
		count++
		return true
	})
	return count
}

func (c *ResponseCache) evictOldest(count int) {
	type kv struct {
		key   string
		entry *ResponseCacheEntry
	}
	var candidates []kv
	c.entries.Range(func(k, v any) bool {
		candidates = append(candidates, kv{key: k.(string), entry: v.(*ResponseCacheEntry)})
		return true
	})
	if len(candidates) <= c.maxEntries {
		return
	}
	excess := len(candidates) - c.maxEntries
	if count > excess {
		count = excess
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].entry.CreatedAt.Before(candidates[j].entry.CreatedAt)
	})
	for i := 0; i < count && i < len(candidates); i++ {
		c.entries.Delete(candidates[i].key)
	}
}

func (c *ResponseCache) cleanupLoop() {
	ticker := time.NewTicker(ResponseCacheCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
			c.purgeExpired()
		}
	}
}

func (c *ResponseCache) purgeExpired() {
	now := time.Now()
	c.entries.Range(func(k, v any) bool {
		entry := v.(*ResponseCacheEntry)
		if now.After(entry.ExpiresAt) {
			c.entries.Delete(k)
		}
		return true
	})
}
