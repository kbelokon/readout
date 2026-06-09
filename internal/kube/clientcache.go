package kube

import (
	"container/list"
	"crypto/sha256"
	"strings"
	"sync"
	"time"
)

const (
	defaultPassthroughClientCacheTTL = 5 * time.Minute
	defaultPassthroughClientCacheMax = 256
)

type PassthroughClientBuilder func(base *Client, token string) (*Client, error)

type passthroughClientCacheKey struct {
	base      *Client
	tokenHash [32]byte
}

type passthroughClientCacheEntry struct {
	key       passthroughClientCacheKey
	client    *Client
	expiresAt time.Time
	element   *list.Element
}

type PassthroughClientCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	now     func() time.Time
	entries map[passthroughClientCacheKey]*passthroughClientCacheEntry
	lru     *list.List
}

func NewPassthroughClientCache(ttl time.Duration, max int) *PassthroughClientCache {
	if ttl <= 0 {
		ttl = defaultPassthroughClientCacheTTL
	}
	if max <= 0 {
		max = defaultPassthroughClientCacheMax
	}
	return &PassthroughClientCache{
		ttl:     ttl,
		max:     max,
		now:     time.Now,
		entries: map[passthroughClientCacheKey]*passthroughClientCacheEntry{},
		lru:     list.New(),
	}
}

func (c *PassthroughClientCache) Get(base *Client, token string, build PassthroughClientBuilder) (*Client, error) {
	if build == nil {
		build = func(base *Client, token string) (*Client, error) {
			return base.WithBearer(token)
		}
	}
	if c == nil {
		return build(base, token)
	}
	token = strings.TrimPrefix(token, "Bearer ")
	key := passthroughClientCacheKey{base: base, tokenHash: sha256.Sum256([]byte(token))}
	now := c.nowTime()

	c.mu.Lock()
	if entry := c.entries[key]; entry != nil {
		if now.Before(entry.expiresAt) {
			c.lru.MoveToFront(entry.element)
			client := entry.client
			c.mu.Unlock()
			return client, nil
		}
		c.removeLocked(entry)
	}
	c.mu.Unlock()

	client, err := build(base, token)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	if entry := c.entries[key]; entry != nil {
		c.removeLocked(entry)
	}
	entry := &passthroughClientCacheEntry{key: key, client: client, expiresAt: now.Add(c.ttl)}
	entry.element = c.lru.PushFront(key)
	c.entries[key] = entry
	for len(c.entries) > c.max {
		back := c.lru.Back()
		if back == nil {
			break
		}
		if victim := c.entries[back.Value.(passthroughClientCacheKey)]; victim != nil {
			c.removeLocked(victim)
		} else {
			c.lru.Remove(back)
		}
	}
	c.mu.Unlock()
	return client, nil
}

func (c *PassthroughClientCache) nowTime() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *PassthroughClientCache) removeLocked(entry *passthroughClientCacheEntry) {
	delete(c.entries, entry.key)
	if entry.element != nil {
		c.lru.Remove(entry.element)
	}
}
