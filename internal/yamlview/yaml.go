// Package yamlview serializes Kubernetes objects to YAML and renders the
// Pygments-compatible highlighted HTML that the resource-view frontend
// (readout.js deep-link/fold/copy + the baked .highlight CSS palette) depends
// on. It is a PURE package: no net/http, no kube.Client, no config -- callers
// pass plain values and, for timestamp links, a per-line transform callback.
//
// Serialization uses sigs.k8s.io/yaml (marshals through JSON struct tags, with
// deterministic alphabetically-sorted keys): both the YAML pane and the
// ?download=yaml body follow sigs.k8s.io/yaml's shape. Equivalence is guaranteed
// at the value level (re-parsing the marshalled YAML yields the same object).
package yamlview

import "sigs.k8s.io/yaml"

// Marshal renders value as YAML using sigs.k8s.io/yaml. Map keys are emitted in
// deterministic sorted order; structs marshal through their JSON tags. The
// output is suitable for both the highlighted YAML pane and the YAML download.
func Marshal(value any) ([]byte, error) {
	return yaml.Marshal(value)
}
