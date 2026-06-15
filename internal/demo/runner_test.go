package demo

// runner_test.go pins the demo wiring contract (design D2/D6/D8): StartEngines
// builds exactly the two scenario clusters as in-process engines WITHOUT the
// /__control/ surface, returning static connections that point at their
// ephemeral loopback URLs.

import (
	"net/http"
	"testing"
)

// TestDemoStartEnginesRegistersTwoClusters proves demo startup yields exactly
// two static connections — the prod + staging clusters — each pointing at a
// distinct in-process loopback engine URL.
func TestDemoStartEnginesRegistersTwoClusters(t *testing.T) {
	servers, conns, err := StartEngines()
	if err != nil {
		t.Fatalf("StartEngines: %v", err)
	}
	t.Cleanup(func() {
		for _, s := range servers {
			s.Close()
		}
	})

	if len(servers) != 2 {
		t.Fatalf("started %d engines, want exactly 2", len(servers))
	}
	if len(conns) != 2 {
		t.Fatalf("returned %d connections, want exactly 2", len(conns))
	}

	names := map[string]bool{}
	urls := map[string]bool{}
	for i, c := range conns {
		if c.Name == "" {
			t.Fatalf("connection %d has empty name", i)
		}
		if c.Server == "" {
			t.Fatalf("connection %d (%s) has empty server URL", i, c.Name)
		}
		// The connection URL must be the matching engine's URL (positional).
		if c.Server != servers[i].URL {
			t.Fatalf("connection %d server %q != engine URL %q", i, c.Server, servers[i].URL)
		}
		names[c.Name] = true
		urls[c.Server] = true
	}
	if !names["prod"] || !names["staging"] {
		t.Fatalf("connection names = %v, want prod + staging", names)
	}
	if len(urls) != 2 {
		t.Fatalf("connections share a server URL: %v", urls)
	}
}

// TestDemoOmitsControl is the control-omission law: the demo's own engines are
// built with fakekube.WithoutControl(), so a request to the /__control/ surface
// on each engine's ACTUAL loopback port returns 404 — the deterministic control
// routes never ship in the demo. (Hitting readout's own port would prove
// nothing: readout has no /__control/ route and always 404s; this test inspects
// the demo's fakekube engines directly.)
func TestDemoOmitsControl(t *testing.T) {
	servers, _, err := StartEngines()
	if err != nil {
		t.Fatalf("StartEngines: %v", err)
	}
	t.Cleanup(func() {
		for _, s := range servers {
			s.Close()
		}
	})

	if len(servers) == 0 {
		t.Fatal("StartEngines returned no engines")
	}

	for _, srv := range servers {
		// A data route must still serve (the engine is seeded + reachable), so a
		// 404 below is the absence of control, not a dead server.
		dataResp, err := http.Get(srv.URL + "/api/v1/namespaces")
		if err != nil {
			t.Fatalf("engine %s data route: %v", srv.URL, err)
		}
		_ = dataResp.Body.Close()
		if dataResp.StatusCode != http.StatusOK {
			t.Fatalf("engine %s data route status = %d, want 200 (engine should be live)", srv.URL, dataResp.StatusCode)
		}

		// The control surface must be absent: /__control/reset is not served.
		ctrlResp, err := http.Get(srv.URL + "/__control/reset")
		if err != nil {
			t.Fatalf("engine %s control probe: %v", srv.URL, err)
		}
		_ = ctrlResp.Body.Close()
		if ctrlResp.StatusCode != http.StatusNotFound {
			t.Fatalf("engine %s /__control/reset status = %d, want 404 (control must be omitted)", srv.URL, ctrlResp.StatusCode)
		}
	}
}
