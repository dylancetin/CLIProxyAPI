package webtools

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

const (
	pageCacheTTL     = 10 * time.Minute
	pageCacheEntries = 256
)

type cacheEntry struct {
	page    *page
	expires time.Time
}

// pageCache keeps extracted pages per (key, URL) for paging and find. Each
// page is stored under its final URL and the URL originally asked for.
type pageCache struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]cacheEntry
}

func newPageCache(now func() time.Time) *pageCache {
	return &pageCache{now: now, entries: map[string]cacheEntry{}}
}

func cacheKey(apiKey, url string) string {
	sum := sha256.Sum256([]byte(apiKey + "\x00" + url))
	return hex.EncodeToString(sum[:])
}

func (c *pageCache) get(apiKey, url string) *page {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := cacheKey(apiKey, url)
	entry, ok := c.entries[key]
	if !ok {
		return nil
	}
	if !c.now().Before(entry.expires) {
		delete(c.entries, key)
		return nil
	}
	return entry.page
}

func (c *pageCache) put(apiKey string, p *page, urls ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if len(c.entries)+len(urls) > pageCacheEntries {
		c.evictLocked(now)
	}
	for _, url := range urls {
		c.entries[cacheKey(apiKey, url)] = cacheEntry{page: p, expires: now.Add(pageCacheTTL)}
	}
}

// evictLocked drops expired entries, then the ones closest to expiry.
func (c *pageCache) evictLocked(now time.Time) {
	for key, entry := range c.entries {
		if !now.Before(entry.expires) {
			delete(c.entries, key)
		}
	}
	for len(c.entries) >= pageCacheEntries {
		var oldestKey string
		var oldest time.Time
		for key, entry := range c.entries {
			if oldestKey == "" || entry.expires.Before(oldest) {
				oldestKey, oldest = key, entry.expires
			}
		}
		delete(c.entries, oldestKey)
	}
}
