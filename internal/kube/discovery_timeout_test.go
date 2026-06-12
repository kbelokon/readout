package kube

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/rest"
)

// hangingDiscoveryServer is a TLS fake apiserver whose discovery endpoints block
// until the test releases them (or the connection is torn down). It models a
// TCP-blackholed cluster: the request reaches the socket but no response ever
// comes back within the caller's budget. release() unblocks every pending and
// future handler so the test can stop the server cleanly.
type hangingDiscoveryServer struct {
	srv     *httptest.Server
	ca      []byte
	release chan struct{}
	once    sync.Once
}

func newHangingDiscoveryServer(t *testing.T) *hangingDiscoveryServer {
	t.Helper()
	h := &hangingDiscoveryServer{release: make(chan struct{})}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-h.release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api":
			_, _ = w.Write([]byte(`{"kind":"APIVersions","versions":["v1"]}`))
		case "/api/v1":
			_, _ = w.Write([]byte(`{"kind":"APIResourceList","groupVersion":"v1","resources":[]}`))
		case "/apis":
			_, _ = w.Write([]byte(`{"kind":"APIGroupList","apiVersion":"v1","groups":[]}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	})
	h.srv = httptest.NewTLSServer(handler)
	h.ca = serverCAPEM(t, h.srv)
	t.Cleanup(func() {
		h.unblock()
		h.srv.Close()
	})
	return h
}

func (h *hangingDiscoveryServer) unblock() {
	h.once.Do(func() { close(h.release) })
}

func (h *hangingDiscoveryServer) client(t *testing.T) *Client {
	t.Helper()
	cfg := &rest.Config{Host: h.srv.URL, TLSClientConfig: rest.TLSClientConfig{CAData: h.ca}}
	client, err := NewClient(cfg, nil, false)
	if err != nil {
		t.Fatalf("NewClient against hanging server: %v", err)
	}
	return client
}

// TestResourceTypesDiscoveryTimeout proves the ctx deadline cuts a discovery
// call that would otherwise block on a blackholed cluster: with a short-deadline
// ctx against a server that never answers, ResourceTypes returns well within the
// deadline (not after the multi-minute OS TCP timeout) and its error classifies
// as a timeout.
func TestResourceTypesDiscoveryTimeout(t *testing.T) {
	srv := newHangingDiscoveryServer(t)
	client := srv.client(t)

	const deadline = 200 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	start := time.Now()
	_, _, err := client.ResourceTypes(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout error from a hanging discovery, got nil")
	}
	// Wall time must be near the deadline, far below the OS TCP reap (~1-2 min).
	if elapsed > 5*time.Second {
		t.Fatalf("ResourceTypes returned after %v -- the ctx deadline did not cut the blocking discovery call", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error does not unwrap to context.DeadlineExceeded: %v", err)
	}
	if got := ClassifyError(err); got != FailureTimeout {
		t.Fatalf("ClassifyError(%v) = %q, want %q", err, got, FailureTimeout)
	}
}

// TestResourceTypesTimeoutDoesNotPoisonCache proves a timed-out discovery leaves
// the cache empty: after the hang-induced failure on one client, a second client
// pointed at a healthy, responsive server completes discovery normally. The
// failed path must not have populated the shared cache fields with an empty
// result, which would otherwise be served as a fresh (but wrong) discovery.
func TestResourceTypesTimeoutDoesNotPoisonCache(t *testing.T) {
	// First: a hanging server with a short deadline -> timeout, empty cache.
	hang := newHangingDiscoveryServer(t)
	hangClient := hang.client(t)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, _, err := hangClient.ResourceTypes(ctx); err == nil {
		t.Fatal("expected the hanging discovery to fail")
	}
	// The failed discovery must not have populated the cache.
	hangClient.mu.Lock()
	poisoned := !hangClient.discoveredAt.IsZero() ||
		len(hangClient.namespacedTypes) > 0 || len(hangClient.clusterTypes) > 0
	hangClient.mu.Unlock()
	if poisoned {
		t.Fatal("a timed-out discovery poisoned the cache: discoveredAt/types were populated on the failure path")
	}

	// Second: a healthy server answers discovery normally.
	healthy := newTLSFakeAPIServer(t)
	cfg := &rest.Config{Host: healthy.URL, TLSClientConfig: rest.TLSClientConfig{CAData: serverCAPEM(t, healthy)}}
	healthyClient, err := NewClient(cfg, nil, false)
	if err != nil {
		t.Fatalf("NewClient against healthy server: %v", err)
	}
	ns, cluster, err := healthyClient.ResourceTypes(context.Background())
	if err != nil {
		t.Fatalf("healthy discovery after a timed-out one failed: %v", err)
	}
	// The metrics pseudo-types are always present on a successful discovery,
	// so a non-empty result confirms the healthy path ran end to end.
	if len(ns) == 0 && len(cluster) == 0 {
		t.Fatal("healthy discovery returned no resource types")
	}
}
