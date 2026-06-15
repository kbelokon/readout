package demo

// breathing_test.go proves the breathing driver drives the demo's own engines
// through Server.Apply (no /__control/): its targets resolve to real seeded
// pods, a pulse lands on the engine LIST state, and Pause/Stop are idempotent.

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

	// A pulse must apply without error against the real seeded pod routes, and
	// the breathing pod must remain listable afterwards (the MODIFIED upsert
	// matched an existing row, not invented a dangling one).
	d.pulse()
	for _, tg := range d.targets {
		resp, err := http.Get(tg.server.URL + tg.listPath)
		if err != nil {
			t.Fatalf("list %s: %v", tg.listPath, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("list %s status = %d, want 200", tg.listPath, resp.StatusCode)
		}
		var doc map[string]any
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Fatalf("list %s decode: %v", tg.listPath, err)
		}
		items, _ := doc["items"].([]any)
		found := false
		for _, it := range items {
			m, _ := it.(map[string]any)
			meta, _ := m["metadata"].(map[string]any)
			if name, _ := meta["name"].(string); name == tg.name {
				found = true
			}
		}
		if !found {
			t.Fatalf("breathing pod %q missing from %s after a pulse", tg.name, tg.listPath)
		}
	}
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
