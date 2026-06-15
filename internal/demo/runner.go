package demo

// runner.go is the in-process demo wiring: it mirrors the e2e
// harness (tests/e2e/harness/main.go) but WITHOUT writing a kubeconfig — it
// starts one fakekube engine per demo cluster on an ephemeral loopback port and
// returns the static cluster-connection entries that point straight at those
// in-process server URLs. main.go injects the connections into the config's
// static cluster list (cfg.Clusters) BEFORE kube.NewManager builds the manager,
// so discoverStatic consumes them and discoverAll treats the run as explicit
// (no kubeconfig/in-cluster fallback fires).
//
// Each engine is built with fakekube.WithoutControl(): the demo serves a clean
// fake with NO /__control/ surface. The breathing loop (breathing.go)
// drives churn through Server.Apply, not the control prefix, so the control
// routes are never needed.

import (
	"fmt"

	"github.com/kbelokon/readout/internal/config"
	fakekube "github.com/kbelokon/readout/internal/fakekube"
)

// demoClusterLabels gives each demo cluster the label chips a real cluster
// carries on the clusters page (environment, region, provider), keyed by the
// scenario's cluster name.
var demoClusterLabels = map[string]map[string]string{
	"prod": {
		"environment": "production",
		"region":      "us-east-1",
	},
	"staging": {
		"environment": "staging",
		"region":      "us-west-2",
	},
}

// StartEngines starts one in-process fakekube engine per demo cluster, seeds it
// from DemoScenario(), and returns the running engines alongside the static
// cluster-connection entries that point at them. The connection Name is the
// cluster's display name; the Server is the engine's ephemeral loopback URL.
//
// On any failure every already-started engine is closed before returning, so a
// partial start never leaks a listener. The caller (cmd/readout/main.go and the
// TestDemoOmitsControl test) owns Close() of the returned engines and is
// responsible for stopping the breathing driver built over them.
func StartEngines() (servers []*fakekube.Server, conns []config.ClusterConnection, err error) {
	scenario := DemoScenario()
	servers = make([]*fakekube.Server, 0, len(scenario.Clusters))
	conns = make([]config.ClusterConnection, 0, len(scenario.Clusters))

	// Close every started engine on a mid-loop failure so a partial start does
	// not leak listeners.
	defer func() {
		if err != nil {
			for _, s := range servers {
				s.Close()
			}
			servers = nil
			conns = nil
		}
	}()

	for i := range scenario.Clusters {
		cluster := &scenario.Clusters[i]
		srv, newErr := fakekube.New(fakekube.WithoutControl())
		if newErr != nil {
			return servers, conns, fmt.Errorf("demo: start engine for cluster %q: %w", cluster.Name, newErr)
		}
		servers = append(servers, srv)
		if seedErr := srv.Seed(cluster); seedErr != nil {
			return servers, conns, fmt.Errorf("demo: seed cluster %q: %w", cluster.Name, seedErr)
		}
		conns = append(conns, config.ClusterConnection{
			Name:   cluster.Name,
			Server: srv.URL,
			Labels: demoClusterLabels[cluster.Name],
		})
	}
	return servers, conns, nil
}
