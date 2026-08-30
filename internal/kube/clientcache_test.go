package kube

import (
	"testing"
	"time"

	"k8s.io/client-go/rest"
)

// TestPassthroughClientCacheKeysOnDerivedIdentity: the cache entry key IS the
// identity the built client reports, so the cache and everything downstream that
// pools work per credential agree on what "the same viewer" means without a
// second derivation that could drift.
func TestPassthroughClientCacheKeysOnDerivedIdentity(t *testing.T) {
	cache := NewPassthroughClientCache(time.Hour, 8)
	base, err := NewClient(&rest.Config{Host: "https://cluster.example"}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	// nil builder: the real WithBearer path, which is the derivation under test.
	client, err := cache.Get(base, "Bearer viewer-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cache.entries) != 1 {
		t.Fatalf("cache entries = %d, want 1", len(cache.entries))
	}
	for key := range cache.entries {
		if key != client.IdentityKey() {
			t.Fatalf("cache key %q, want the built client's identity %q", key, client.IdentityKey())
		}
	}
}

// TestPassthroughClientCacheIdentityStableAcrossEviction: a viewer whose cached
// client expired gets a NEW client with the SAME identity, so the pools keyed on
// it (shared list/watch sources) survive an eviction instead of forking.
func TestPassthroughClientCacheIdentityStableAcrossEviction(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	cache := NewPassthroughClientCache(5*time.Minute, 8)
	cache.now = func() time.Time { return now }
	base, err := NewClient(&rest.Config{Host: "https://cluster.example"}, nil, false)
	if err != nil {
		t.Fatal(err)
	}

	first, err := cache.Get(base, "viewer-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(6 * time.Minute)
	rebuilt, err := cache.Get(base, "viewer-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt == first {
		t.Fatal("an expired passthrough client should be rebuilt")
	}
	if rebuilt.IdentityKey() != first.IdentityKey() {
		t.Fatalf("identity after eviction = %q, want the original %q", rebuilt.IdentityKey(), first.IdentityKey())
	}
	if len(cache.entries) != 1 {
		t.Fatalf("cache entries = %d after rebuilding one credential, want 1", len(cache.entries))
	}
}
