package demo

// breathing_test.go proves the breathing driver drives the demo's own engines
// through Server.Apply (no /__control/): its targets resolve to real seeded
// pods, a pulse lands on both namespaced and all-namespaces engine state, and
// Pause/Stop are idempotent.

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func TestBreathingPulseLandsOnSeededPods(t *testing.T) {
	servers, conns, err := StartEngines()
	if err != nil {
		t.Fatalf("StartEngines: %v", err)
	}
	t.Cleanup(func() {
		for _, s := range servers {
			s.Close()
		}
	})

	var names []string
	for _, c := range conns {
		names = append(names, c.Name)
	}
	d := NewBreathingDriver(servers, names)
	if len(d.targets) != 2 {
		t.Fatalf("breathing targets = %d, want 2 (prod + staging)", len(d.targets))
	}

	beforeRV := make(map[string]string)
	for _, tg := range d.targets {
		for _, path := range []string{tg.listPath, allNamespacesPodsPath} {
			rv, _, found := breathingPodState(t, tg.server.URL, path, tg.name)
			if !found {
				t.Fatalf("seeded breathing pod %q missing from %s", tg.name, path)
			}
			beforeRV[tg.server.URL+path] = rv
		}
	}

	// A pulse must apply without error against both real seeded pod routes, and
	// the breathing pod must remain listable afterwards (the MODIFIED upsert
	// matched an existing row, not invented a dangling one). fakekube owns
	// separate collection state per route, so checking both paths prevents the
	// All-namespaces Live view from silently going static.
	d.pulse()
	for _, tg := range d.targets {
		for _, path := range []string{tg.listPath, allNamespacesPodsPath} {
			rv, restarts, found := breathingPodState(t, tg.server.URL, path, tg.name)
			if !found {
				t.Fatalf("breathing pod %q missing from %s after a pulse", tg.name, path)
			}
			if rv == beforeRV[tg.server.URL+path] {
				t.Fatalf("list %s resourceVersion did not advance from %q", path, rv)
			}
			if restarts != 1 {
				t.Fatalf("breathing pod %q restarts in %s = %v, want 1", tg.name, path, restarts)
			}
		}
	}
}

func breathingPodState(t *testing.T, serverURL, path, name string) (string, float64, bool) {
	t.Helper()
	resp, err := http.Get(serverURL + path)
	if err != nil {
		t.Fatalf("list %s: %v", path, err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list %s status = %d, want 200", path, resp.StatusCode)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("list %s decode: %v", path, err)
	}
	metadata, _ := doc["metadata"].(map[string]any)
	rv, _ := metadata["resourceVersion"].(string)
	items, _ := doc["items"].([]any)
	for _, it := range items {
		object, _ := it.(map[string]any)
		meta, _ := object["metadata"].(map[string]any)
		if itemName, _ := meta["name"].(string); itemName != name {
			continue
		}
		status, _ := object["status"].(map[string]any)
		statuses, _ := status["containerStatuses"].([]any)
		if len(statuses) == 0 {
			return rv, 0, true
		}
		container, _ := statuses[0].(map[string]any)
		restarts, _ := container["restartCount"].(float64)
		return rv, restarts, true
	}
	return rv, 0, false
}

func TestBreathingLifecycleIdempotent(t *testing.T) {
	servers, conns, err := StartEngines()
	if err != nil {
		t.Fatalf("StartEngines: %v", err)
	}
	t.Cleanup(func() {
		for _, s := range servers {
			s.Close()
		}
	})
	var names []string
	for _, c := range conns {
		names = append(names, c.Name)
	}
	d := NewBreathingDriver(servers, names)

	// Start/Pause/Start/Stop must not panic or deadlock, and repeated Pause/Stop
	// are no-ops (a later snapshot unit relies on Pause being safe to call).
	d.Start()
	d.Pause()
	d.Pause()
	d.Start()
	d.Stop()
	d.Stop()
	d.Start() // no-op after Stop
}
