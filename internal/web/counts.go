package web

// counts.go implements the sidebar per-kind object counts:
// one chunked list request per sidebar kind with `limit=1`, where
//
//	count = len(rows) + metadata.remainingItemCount
//
// handles all three server shapes uniformly -- a paginating apiserver returns 1
// row + remainingItemCount (the live-probed shape: limit=1 over 107 namespaces
// -> remainingItemCount 106), a server with exactly one object returns 1 row
// and no remainder, and a server that IGNORES limit returns every row and no
// remainder, so the row length alone is already the count. The fetch rides the
// same meta.k8s.io Table negotiation the list pages use.
//
// Counts are cached on the Server (TTL-invalidated, hardcoded -- deliberately
// NO config knob), fetched concurrently during layout assembly with one shared
// short deadline so a slow or dead kind can never delay a page render beyond
// countFetchTimeout, and render on full page loads only -- the `_table`
// refresh partial never rebuilds the sidebar, so ticks never re-fetch
// (a deliberate cost cut: the design calls for live counts, but page-load +
// TTL is the sanctioned trade-off). A failed fetch renders NO count (and the failure is
// remembered for the same TTL so a broken kind is not re-probed on every page
// load) -- EXCEPT when the failure is the caller's own cancellation (an
// aborted page load), which says nothing about the kind and is never cached;
// zero objects render "0".

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/kbelokon/readout/internal/kube"
)

const (
	// countTTL is how long a fetched count (or a fetch failure) is served from
	// the Server cache before a page load re-fetches it.
	countTTL = 15 * time.Second
	// countFetchTimeout bounds the whole per-render count fan-out: every fetch
	// shares one deadline, so layout assembly stalls at most this long even if
	// every kind hangs.
	countFetchTimeout = 800 * time.Millisecond
)

// countKey identifies one cached count. The unit of caching is the exact list
// the sidebar entry points at: the cluster, the resolved type (apiVersion +
// plural -- plural alone could collide across groups), and the namespace scope
// ("" for cluster-wide lists). Without the namespace in the key, switching
// namespaces inside the TTL would serve the previous namespace's counts.
type countKey struct {
	cluster    string
	apiVersion string
	plural     string
	namespace  string
}

// countEntry is one cache slot: the resolved count when ok, or a remembered
// failure (ok=false renders no count and suppresses re-fetching until the TTL
// expires).
type countEntry struct {
	count     int64
	ok        bool
	fetchedAt time.Time
}

// countCache is the Server-held count cache. The zero value is ready to use
// (the map allocates lazily under the lock), so the Server struct needs no
// constructor change and zero-value test Servers stay valid.
type countCache struct {
	mu      sync.Mutex
	entries map[countKey]countEntry
}

// lookup returns the cached entry when it exists and is fresher than the TTL.
func (c *countCache) lookup(key countKey, now time.Time) (countEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || now.Sub(entry.fetchedAt) >= countTTL {
		return countEntry{}, false
	}
	return entry, true
}

func (c *countCache) store(key countKey, entry countEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[countKey]countEntry{}
	}
	c.entries[key] = entry
}

// tableCount is the sidebar count formula over a limit=1 Table chunk:
// len(rows) + metadata.remainingItemCount. It covers zero items (0 rows, no
// remainder -> 0), an exact-one list (1 row, no remainder -> 1), the normal
// paginating apiserver (1 row + remainingItemCount 106 -> 107), and a server
// that ignores limit (all rows, no remainder -> the row length).
// remainingItemCount is documented as an ESTIMATE under churn -- acceptable
// for orientation counts.
func tableCount(t *kube.Table) int64 {
	n := int64(len(t.Rows))
	if t.RemainingItemCount != nil {
		n += *t.RemainingItemCount
	}
	return n
}

// countTarget is one sidebar entry awaiting a count: the slot to write into
// plus the resolved type and the namespace scope of the list it points at.
type countTarget struct {
	item      *navItem
	resource  kube.ResourceType
	namespace string
}

// attachSidebarCounts resolves and writes the `.menu-count` value for every
// counted sidebar entry, concurrently, against the count cache. It is a no-op
// without a concrete cluster client -- the multi-cluster (_all) sidebar and
// the no-discovery fallback render no counts by design.
func (s *Server) attachSidebarCounts(ctx context.Context, client *kube.Client, cluster string, targets []countTarget) {
	if client == nil || cluster == "" || cluster == kube.AllClusters || len(targets) == 0 {
		return
	}
	fetchCtx, cancel := context.WithTimeout(ctx, countFetchTimeout)
	defer cancel()
	var g errgroup.Group
	for i := range targets {
		target := &targets[i]
		// The metrics.k8s.io pseudo-types (PodMetrics/NodeMetrics) are join
		// sources, not navigable kinds -- never count them (their plurals also
		// collide with pods/nodes). Mirrors the palette-feed skip.
		if target.resource.Group == "metrics.k8s.io" {
			continue
		}
		g.Go(func() error {
			count, ok := s.cachedCount(fetchCtx, client, cluster, target)
			if ok {
				target.item.Count = strconv.FormatInt(count, 10)
				target.item.HasCount = true
			}
			// Errors degrade to an absent count; never fail the group (a g.Wait
			// error would have no consumer, and sibling counts must still land).
			return nil
		})
	}
	_ = g.Wait()
}

// cachedCount returns the count for one target, consulting the TTL cache
// before issuing the limit=1 chunk fetch. Fetch failures are cached too, so a
// broken kind costs one probe per TTL window instead of one per page load.
func (s *Server) cachedCount(ctx context.Context, client *kube.Client, cluster string, target *countTarget) (int64, bool) {
	key := countKey{
		cluster:    cluster,
		apiVersion: target.resource.APIVersion,
		plural:     target.resource.Plural,
		namespace:  target.namespace,
	}
	now := s.clock()
	if entry, ok := s.counts.lookup(key, now); ok {
		return entry.count, entry.ok
	}
	table, err := client.Table(ctx, &target.resource, kube.ListOptions{Namespace: target.namespace, Limit: 1})
	entry := countEntry{fetchedAt: now}
	if err == nil {
		entry.count = tableCount(&table)
		entry.ok = true
	} else if ctx.Err() == context.Canceled || errors.Is(err, context.Canceled) {
		// The CALLER's own cancellation (an aborted page load propagating into
		// the fan-out ctx) says nothing about the kind -- never negative-cache
		// it, or one aborted load blanks every sidebar count for a full TTL.
		// The per-fetch deadline stays cached deliberately: countFetchTimeout
		// firing surfaces as DeadlineExceeded (not Canceled), so a dead-slow
		// kind still costs one probe per TTL window, not one per page load.
		return 0, false
	}
	s.counts.store(key, entry)
	return entry.count, entry.ok
}
