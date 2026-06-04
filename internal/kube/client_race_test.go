package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/rest"
)

// TestResourceTypesConcurrentDiscoveryIsRaceFree proves ResourceTypes does not
// touch the shared c.preferred map outside c.mu during discovery.
//
// The unfixed code read (ranged) and wrote c.preferred AFTER releasing c.mu --
// the discovery loop did `c.preferred[plural] = ...` and sortResourceTypes ranged
// it -- so concurrent cold/expired callers raced on that map. The race is real
// but elusive: a normal fake server makes the per-call HTTP round trips dominate
// the timeline, so the brief map touches rarely overlap and `-race` slips past
// it (this is exactly why the pre-existing suite never tripped it). This test
// makes the bite DETERMINISTIC: a discovery group with many distinct new plurals
// makes the c.preferred write loop long and write-dense, and a release barrier on
// that group's response frees every caller out of ServerGroupsAndResources at the
// same instant so the dense loops run truly concurrently. Under `go test -race`
// the unfixed code fails (concurrent map iterate/write at client.go:119/146/156);
// the local-map fix is clean and every caller observes a consistent type set.
//
// The expired sub-case zeroes discoveredAt between rounds (same-package access)
// so each round re-runs concurrent discovery, covering the cache-expiry refresh
// route, not just first cold discovery.
func TestResourceTypesConcurrentDiscoveryIsRaceFree(t *testing.T) {
	const callers = 16
	const resourcesPerGroup = 400

	run := func(t *testing.T, c *Client, b *barrier) {
		var wg sync.WaitGroup
		wg.Add(callers)
		nsCounts := make([]int, callers)
		clusterCounts := make([]int, callers)
		errs := make([]error, callers)
		for i := 0; i < callers; i++ {
			go func(i int) {
				defer wg.Done()
				ns, cluster, err := c.ResourceTypes(context.Background())
				nsCounts[i] = len(ns)
				clusterCounts[i] = len(cluster)
				errs[i] = err
			}(i)
		}
		wg.Wait()
		b.reset()
		for i := 0; i < callers; i++ {
			if errs[i] != nil {
				t.Fatalf("caller %d: ResourceTypes error: %v", i, errs[i])
			}
		}
		// A torn c.preferred (or torn type slice) would skew the sort/count and
		// make concurrent callers disagree on the observed type-set size.
		for i := 1; i < callers; i++ {
			if nsCounts[i] != nsCounts[0] || clusterCounts[i] != clusterCounts[0] {
				t.Fatalf("caller %d saw ns=%d cluster=%d, caller 0 saw ns=%d cluster=%d (inconsistent discovery)",
					i, nsCounts[i], clusterCounts[i], nsCounts[0], clusterCounts[0])
			}
		}
		if nsCounts[0] == 0 || clusterCounts[0] == 0 {
			t.Fatalf("empty discovery: ns=%d cluster=%d", nsCounts[0], clusterCounts[0])
		}
	}

	t.Run("cold", func(t *testing.T) {
		b := newBarrier(callers)
		srv := newDenseDiscoveryFake(t, b, resourcesPerGroup)
		c, err := NewClient(&rest.Config{Host: srv.URL}, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		run(t, c, b)
	})

	t.Run("expired", func(t *testing.T) {
		b := newBarrier(callers)
		srv := newDenseDiscoveryFake(t, b, resourcesPerGroup)
		c, err := NewClient(&rest.Config{Host: srv.URL}, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		for round := 0; round < 3; round++ {
			c.mu.Lock()
			c.discoveredAt = time.Time{}
			c.mu.Unlock()
			run(t, c, b)
		}
	})
}

// barrier releases parked goroutines in waves of `width`: each Wait blocks until
// `width` goroutines are parked, then frees that whole wave at once so they
// resume simultaneously.
type barrier struct {
	width int
	mu    sync.Mutex
	cond  *sync.Cond
	count int
	gen   int
}

func newBarrier(width int) *barrier {
	b := &barrier{width: width}
	b.cond = sync.NewCond(&b.mu)
	return b
}

func (b *barrier) Wait() {
	b.mu.Lock()
	defer b.mu.Unlock()
	gen := b.gen
	b.count++
	if b.count >= b.width {
		b.count = 0
		b.gen++
		b.cond.Broadcast()
		return
	}
	for gen == b.gen {
		b.cond.Wait()
	}
}

// reset drops any goroutines parked from a partial wave so a reused barrier
// starts the next round clean (the expired sub-case reuses one barrier).
func (b *barrier) reset() {
	b.mu.Lock()
	b.count = 0
	b.gen++
	b.cond.Broadcast()
	b.mu.Unlock()
}

// newDenseDiscoveryFake serves a minimal discovery whose single group-version
// (bench.io/v1) carries `resources` distinct listable types, all new plurals.
// The group-version response is gated on the barrier so every concurrent caller
// exits ServerGroupsAndResources together and runs the dense c.preferred write
// loop at the same time -- the deterministic collision the race detector needs.
func newDenseDiscoveryFake(t *testing.T, b *barrier, resources int) *httptest.Server {
	t.Helper()
	resourceList := make([]map[string]any, 0, resources)
	for i := 0; i < resources; i++ {
		resourceList = append(resourceList, map[string]any{
			"name":         fmt.Sprintf("widgets%d", i),
			"singularName": fmt.Sprintf("widget%d", i),
			"namespaced":   true,
			"kind":         fmt.Sprintf("Widget%d", i),
			"verbs":        []string{"get", "list"},
		})
	}
	big, err := json.Marshal(map[string]any{
		"kind":         "APIResourceList",
		"apiVersion":   "v1",
		"groupVersion": "bench.io/v1",
		"resources":    resourceList,
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"APIVersions","versions":["v1"]}`))
	})
	mux.HandleFunc("/api/v1", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"APIResourceList","groupVersion":"v1","resources":[]}`))
	})
	mux.HandleFunc("/apis", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"APIGroupList","groups":[{"name":"bench.io","versions":[{"groupVersion":"bench.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"bench.io/v1","version":"v1"}}]}`))
	})
	mux.HandleFunc("/apis/bench.io/v1", func(w http.ResponseWriter, _ *http.Request) {
		b.Wait()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(big)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}
