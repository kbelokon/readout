package kube

import (
	"container/list"
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
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
	builds  singleflight.Group
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
	// A burst of requests from one viewer must not construct a separate
	// discovery/client stack for every cache miss. The base pointer is part of
	// the identity because the same bearer token can legitimately be used
	// against multiple clusters; only the token's digest enters the flight key.
	flightKey := fmt.Sprintf("%p:%x", base, key.tokenHash)
	value, err, _ := c.builds.Do(flightKey, func() (any, error) {
		return c.getOrBuild(key, token, build)
	})
	if err != nil {
		return nil, err
	}
	return value.(*Client), nil
}

func (c *PassthroughClientCache) getOrBuild(key passthroughClientCacheKey, token string, build PassthroughClientBuilder) (*Client, error) {
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

	client, err := build(key.base, token)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	// Construction can involve client-go setup, so TTL starts when the client is
	// ready rather than at the beginning of that potentially slow operation.
	entry := &passthroughClientCacheEntry{key: key, client: client, expiresAt: c.nowTime().Add(c.ttl)}
	entry.element = c.lru.PushFront(entry)
	c.entries[key] = entry
	// The cache was within its bound before this one insertion, so at most one
	// entry needs eviction. LRU nodes carry the entry itself, keeping the list
	// and map on a single invariant instead of recovering an entry through a
	// second lookup with impossible nil-list/nil-map fallback branches.
	if len(c.entries) > c.max {
		c.removeLocked(c.lru.Back().Value.(*passthroughClientCacheEntry))
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
