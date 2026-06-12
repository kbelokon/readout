package hooks

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestPostJSONErrorLanes pins the postJSON error semantics: a non-2xx status
// surfaces an error carrying the status, a malformed URL fails before any
// request, and an unmarshalable payload fails at marshal time.
func TestPostJSONErrorLanes(t *testing.T) {
	c := NewClient()

	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer ok.Close()

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusTeapot)
	}))
	defer bad.Close()

	if err := c.postJSON(context.Background(), bad.URL, map[string]string{"x": "y"}, nil); err == nil || !strings.Contains(err.Error(), "418") {
		t.Fatalf("postJSON status err = %v", err)
	}
	if err := c.postJSON(context.Background(), "://bad-url", map[string]string{"x": "y"}, nil); err == nil {
		t.Fatal("bad URL unexpectedly succeeded")
	}
	if err := c.postJSON(context.Background(), ok.URL, func() {}, nil); err == nil {
		t.Fatal("unmarshalable payload unexpectedly posted")
	}
}

// TestPostJSONInvalidJSON pins that a 2xx body that is not valid JSON surfaces
// an unmarshal error, while an empty 2xx body is a success that leaves out
// untouched.
func TestPostJSONInvalidJSON(t *testing.T) {
	c := NewClient()

	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer garbage.Close()
	var out map[string]any
	if err := c.postJSON(context.Background(), garbage.URL, map[string]string{}, &out); err == nil {
		t.Fatal("invalid JSON body unexpectedly decoded")
	}

	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer empty.Close()
	out = nil
	if err := c.postJSON(context.Background(), empty.URL, map[string]string{}, &out); err != nil {
		t.Fatalf("empty 2xx body err = %v", err)
	}
	if out != nil {
		t.Fatalf("empty 2xx body wrote out = %#v", out)
	}
}

// TestPostJSONTimeout pins that a hung endpoint fails fast on the same lane as
// any transport error: the call returns an error well before the server's own
// sleep would finish.
func TestPostJSONTimeout(t *testing.T) {
	const serverSleep = 5 * time.Second
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(serverSleep)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer slow.Close()

	c := newClient(100 * time.Millisecond)
	start := time.Now()
	err := c.postJSON(context.Background(), slow.URL, map[string]string{}, nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("slow endpoint past the timeout unexpectedly succeeded")
	}
	if elapsed >= serverSleep {
		t.Fatalf("timeout did not fire early: elapsed=%s server sleep=%s", elapsed, serverSleep)
	}
}

// TestPostJSONResponseCap pins that a body comfortably over the old 1 MiB cap
// still decodes under the 4 MiB cap.
func TestPostJSONResponseCap(t *testing.T) {
	big := strings.Repeat("a", 2<<20) // 2 MiB, over the old 1 MiB cap.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"blob": big})
	}))
	defer server.Close()

	c := NewClient()
	var out map[string]string
	if err := c.postJSON(context.Background(), server.URL, map[string]string{}, &out); err != nil {
		t.Fatalf("2 MiB body err = %v", err)
	}
	if out["blob"] != big {
		t.Fatalf("2 MiB body decoded length = %d, want %d", len(out["blob"]), len(big))
	}
}

// TestAuthorizationEmptyURL pins the no-hook-configured short circuit: access is
// allowed without any network call.
func TestAuthorizationEmptyURL(t *testing.T) {
	c := NewClient()
	result, err := c.Authorization(context.Background(), "", AuthorizationRequest{})
	if err != nil {
		t.Fatalf("empty-url authorization err = %v", err)
	}
	if result.Allowed == nil || !*result.Allowed {
		t.Fatalf("empty-url authorization result = %#v", result)
	}
}

// TestPrerenderEmptyURL pins the no-hook-configured short circuit: an empty
// result and no network call.
func TestPrerenderEmptyURL(t *testing.T) {
	c := NewClient()
	result, err := c.Prerender(context.Background(), "", &PrerenderRequest{})
	if err != nil {
		t.Fatalf("empty-url prerender err = %v", err)
	}
	if result.Links != nil || result.Resource != nil {
		t.Fatalf("empty-url prerender result = %#v", result)
	}
}

// hookObservation is one captured observer call: the hook name and whether the
// call errored.
type hookObservation struct {
	hook   string
	failed bool
}

// TestObserverRecordsRealCalls pins the hook observer contract: a configured
// authorization call records an "ok" observation under the authorization name,
// and a failing prerender call records a failed observation under the prerender
// name. The hook name and result line up with the metric labels web wires.
func TestObserverRecordsRealCalls(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ok.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer bad.Close()

	var hits []hookObservation
	c := NewClient()
	c.SetObserver(func(hook string, err error, _ time.Duration) {
		hits = append(hits, hookObservation{hook: hook, failed: err != nil})
	})

	if _, err := c.Authorization(context.Background(), ok.URL, AuthorizationRequest{}); err != nil {
		t.Fatalf("authorization call err = %v", err)
	}
	if _, err := c.Prerender(context.Background(), bad.URL, &PrerenderRequest{}); err == nil {
		t.Fatal("prerender against a 500 should fail")
	}

	want := []hookObservation{
		{hook: HookAuthorization, failed: false},
		{hook: HookPrerender, failed: true},
	}
	if len(hits) != len(want) {
		t.Fatalf("observer hits = %#v, want %#v", hits, want)
	}
	for i := range want {
		if hits[i] != want[i] {
			t.Fatalf("observer hit %d = %#v, want %#v", i, hits[i], want[i])
		}
	}
}

// TestObserverNotCalledForEmptyURL pins that the no-hook short circuit records
// nothing: only real outbound calls land in the duration histogram.
func TestObserverNotCalledForEmptyURL(t *testing.T) {
	c := NewClient()
	called := false
	c.SetObserver(func(string, error, time.Duration) { called = true })

	if _, err := c.Authorization(context.Background(), "", AuthorizationRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Prerender(context.Background(), "", &PrerenderRequest{}); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("observer fired for an unconfigured (empty-url) hook")
	}
}
