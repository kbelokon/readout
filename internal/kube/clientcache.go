package kube

import (
	"container/list"
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

type passthroughClientCacheEntry struct {
	// identity is the cache key: the derived credential identity of the client
	// this entry holds, never the raw token.
	identity  string
	client    *Client
	expiresAt time.Time
	element   *list.Element
}

type PassthroughClientCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	now     func() time.Time
	entries map[string]*passthroughClientCacheEntry
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
		entries: map[string]*passthroughClientCacheEntry{},
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
	// Entries are keyed by exactly the identity WithBearer stamps on the client
	// it builds, through the one shared derivation: the base client is part of
	// it because the same bearer token can legitimately address several
	// clusters, and the token itself enters only as a digest. That single key
	// also serializes the flight, so a burst of requests from one viewer does
	// not construct a separate discovery/client stack per cache miss.
	identity := bearerIdentity(base, token)
	value, err, _ := c.builds.Do(identity, func() (any, error) {
		return c.getOrBuild(identity, base, token, build)
	})
	if err != nil {
		return nil, err
	}
	return value.(*Client), nil
}

func (c *PassthroughClientCache) getOrBuild(identity string, base *Client, token string, build PassthroughClientBuilder) (*Client, error) {
	now := c.nowTime()

	c.mu.Lock()
	if entry := c.entries[identity]; entry != nil {
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
	// Construction can involve client-go setup, so TTL starts when the client is
	// ready rather than at the beginning of that potentially slow operation.
	entry := &passthroughClientCacheEntry{identity: identity, client: client, expiresAt: c.nowTime().Add(c.ttl)}
	entry.element = c.lru.PushFront(entry)
	c.entries[identity] = entry
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
	delete(c.entries, entry.identity)
	if entry.element != nil {
		c.lru.Remove(entry.element)
	}
}
