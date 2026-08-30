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
	"time"
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
	// The breath ALTERNATES: one pulse proving "restarts == 1" would still pass
	// with a constant, and a demo whose Live screens stop moving is the whole
	// feature failing silently.
	for beat, wantRestarts := range []float64{1, 0, 1, 0} {
		d.pulse()
		for _, tg := range d.targets {
			for _, path := range []string{tg.listPath, allNamespacesPodsPath} {
				rv, restarts, found := breathingPodState(t, tg.server.URL, path, tg.name)
				if !found {
					t.Fatalf("breathing pod %q missing from %s after beat %d", tg.name, path, beat)
				}
				if rv == beforeRV[tg.server.URL+path] {
					t.Fatalf("beat %d: list %s resourceVersion did not advance from %q", beat, path, rv)
				}
				beforeRV[tg.server.URL+path] = rv
				if restarts != wantRestarts {
					t.Fatalf("beat %d: breathing pod %q restarts in %s = %v, want %v", beat, tg.name, path, restarts, wantRestarts)
				}
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

	// Drive the real ticker path rather than calling pulse directly: without
	// this the loop goroutine is never executed by any test, and a Stop gutted
	// to a no-op would leak it for the process lifetime with everything green.
	d.mu.Lock()
	d.interval = 2 * time.Millisecond
	d.mu.Unlock()

	d.Start()
	d.Start() // idempotent: still exactly one ticker and one goroutine
	waitForBeats(t, d, 2)

	d.Pause()
	d.Pause()
	paused := beats(d)
	time.Sleep(30 * time.Millisecond)
	if got := beats(d); got != paused {
		t.Fatalf("a paused driver kept breathing: %d -> %d", paused, got)
	}

	// Resume, then stop for good.
	d.Start()
	waitForBeats(t, d, paused+2)
	d.Stop()
	d.Stop()
	stopped := beats(d)
	d.Start() // no-op after Stop
	time.Sleep(30 * time.Millisecond)
	if got := beats(d); got != stopped {
		t.Fatalf("a stopped driver kept breathing: %d -> %d", stopped, got)
	}
	d.mu.Lock()
	running, ticker := d.running, d.ticker
	d.mu.Unlock()
	if running || ticker != nil {
		t.Fatalf("Stop left the loop armed: running=%t ticker=%v", running, ticker)
	}
}

func beats(d *BreathingDriver) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.beat
}

func waitForBeats(t *testing.T, d *BreathingDriver, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for beats(d) < want {
		if time.Now().After(deadline) {
			t.Fatalf("the breathing loop produced %d beats, want at least %d", beats(d), want)
		}
		time.Sleep(time.Millisecond)
	}
}
