package fakekube

// integrity.go is the D5 "integrity-as-law" validator: it runs inside Seed
// (seed.go) BEFORE any store is built, and rejects a Cluster whose object graph
// has a dangling reference. The guarantee is load-bearing — a forgotten link
// must fail a test, never ship as a dead click — so every reference is resolved
// here, not merely documented:
//
//   - every object's group-version-kind is registered (builtin or CRD);
//   - an object's explicit metadata.namespace matches the namespace it sits in;
//   - ownerReferences resolve to an object in the same namespace (the
//     Deployment->ReplicaSet->Pod owner chain);
//   - Pod.spec.nodeName resolves to a real Node;
//   - Service spec.selector matches at least one Pod's labels in the namespace;
//   - Ingress backend service names resolve to a Service in the namespace
//     (the Ingress->Service->Pod chain's first hop; the Service->Pod hop is the
//     selector check above);
//   - Event involvedObject names a real object in its namespace;
//   - PersistentVolumeClaim spec.volumeName resolves to a PersistentVolume, and
//     a Pod volume's persistentVolumeClaim.claimName resolves to a PVC in the
//     namespace;
//   - PodMetrics names resolve to a Pod in the namespace, NodeMetrics names to
//     a Node.
//
// The error names the dangling reference so the failing test reads as a
// diagnosis. Validation reads each object as its JSON wire map (the same form
// Seed serves), so it covers typed objects and any runtime.Object uniformly.

import (
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
)

// objIndex is the resolved-identity index the validator resolves references
// against: every object keyed by namespace + kind + name, plus the node and
// PV name sets and per-namespace pod-label list.
type objIndex struct {
	// byKey holds "<namespace>/<kind>/<name>" -> true for every namespaced and
	// cluster-scoped object (cluster-scoped objects use an empty namespace).
	byKey map[string]bool
	// nodes is the set of Node names.
	nodes map[string]bool
	// pvs is the set of PersistentVolume names.
	pvs map[string]bool
	// pvcsByNS["<ns>"] is the set of PVC names in that namespace.
	pvcsByNS map[string]map[string]bool
	// podLabelsByNS["<ns>"] is the slice of pod label maps in that namespace
	// (Service selectors are matched against these).
	podLabelsByNS map[string][]map[string]string
	// servicesByNS["<ns>"] is the set of Service names in that namespace.
	servicesByNS map[string]map[string]bool
}

func objKey(namespace, kind, name string) string {
	return namespace + "/" + kind + "/" + name
}

// validateCluster runs the integrity law over a Cluster. It returns the first
// dangling reference as an error naming it; nil means every reference resolves.
func validateCluster(c Cluster, reg map[string]gvrInfo) error {
	idx, wires, err := indexCluster(c, reg)
	if err != nil {
		return err
	}

	for _, w := range wires {
		if err := validateObject(w, idx); err != nil {
			return err
		}
	}

	// Metrics keys.
	for _, ns := range c.Namespaces {
		for _, pm := range ns.PodMetrics {
			if !idx.byKey[objKey(ns.Name, "Pod", pm.Name)] {
				return fmt.Errorf("integrity: PodMetrics %q in namespace %q references no Pod", pm.Name, ns.Name)
			}
		}
	}
	for _, nm := range c.NodeMetrics {
		if !idx.nodes[nm.Name] {
			return fmt.Errorf("integrity: NodeMetrics %q references no Node", nm.Name)
		}
	}
	return nil
}

// wireObject is one object's identity plus its JSON wire map, the unit the
// validator resolves references from. skipRefIntegrity exempts it from the
// reference-resolution checks (a base-cluster event referencing a churned pod).
type wireObject struct {
	kind             string
	namespace        string
	name             string
	m                map[string]any
	skipRefIntegrity bool
}

// indexCluster resolves every object's kind (erroring on an unregistered
// group-version-kind — the dangling-discovery case), builds the resolution
// index, and returns the per-object wire maps for reference checking. It also
// enforces that an object's explicit metadata.namespace matches the namespace
// it is declared in.
func indexCluster(c Cluster, reg map[string]gvrInfo) (*objIndex, []wireObject, error) {
	idx := &objIndex{
		byKey:         map[string]bool{},
		nodes:         map[string]bool{},
		pvs:           map[string]bool{},
		pvcsByNS:      map[string]map[string]bool{},
		podLabelsByNS: map[string][]map[string]string{},
		servicesByNS:  map[string]map[string]bool{},
	}
	var wires []wireObject

	register := func(obj runtime.Object, declaredNS string) error {
		// Unwrap a base-cluster cell/log override so the validator reads the bare
		// typed object exactly as discovery / List / the marshal path do.
		skipRef := false
		if co, ok := obj.(*cellObject); ok {
			skipRef = co.skipRefIntegrity
		}
		_, _, obj = unwrapCellObject(obj)
		info, _, kind, err := objectGVK(obj, reg)
		if err != nil {
			return fmt.Errorf("integrity: %w", err)
		}
		m, err := toMap(obj)
		if err != nil {
			return err
		}
		name := mapMetaString(m, "name")
		explicitNS := mapMetaString(m, "namespace")
		ns := declaredNS
		if !info.namespaced {
			ns = ""
		}
		if info.namespaced && explicitNS != "" && explicitNS != declaredNS {
			return fmt.Errorf("integrity: %s %q declares metadata.namespace %q but sits in namespace %q", kind, name, explicitNS, declaredNS)
		}
		idx.byKey[objKey(ns, kind, name)] = true
		switch {
		case info.group == "" && kind == "Node":
			idx.nodes[name] = true
		case info.group == "" && kind == "PersistentVolume":
			idx.pvs[name] = true
		case info.group == "" && kind == "PersistentVolumeClaim":
			if idx.pvcsByNS[ns] == nil {
				idx.pvcsByNS[ns] = map[string]bool{}
			}
			idx.pvcsByNS[ns][name] = true
		case info.group == "" && kind == "Pod":
			idx.podLabelsByNS[ns] = append(idx.podLabelsByNS[ns], mapLabels(m))
		case info.group == "" && kind == "Service":
			if idx.servicesByNS[ns] == nil {
				idx.servicesByNS[ns] = map[string]bool{}
			}
			idx.servicesByNS[ns][name] = true
		}
		wires = append(wires, wireObject{kind: kind, namespace: ns, name: name, m: m, skipRefIntegrity: skipRef})
		return nil
	}

	for _, n := range c.Nodes {
		if err := register(n, ""); err != nil {
			return nil, nil, err
		}
	}
	for _, o := range c.ClusterObjects {
		if err := register(o, ""); err != nil {
			return nil, nil, err
		}
	}
	for _, ns := range c.Namespaces {
		if ns.Name == "" {
			return nil, nil, fmt.Errorf("integrity: a namespace has an empty name")
		}
		idx.byKey[objKey("", "Namespace", ns.Name)] = true
		for _, o := range ns.Objects {
			if err := register(o, ns.Name); err != nil {
				return nil, nil, err
			}
		}
	}
	return idx, wires, nil
}

// validateObject resolves the references a single object carries. An object
// flagged skipRefIntegrity (a base-cluster event about a churned pod) is exempt.
func validateObject(w wireObject, idx *objIndex) error {
	if w.skipRefIntegrity {
		return nil
	}
	// ownerReferences resolve to an object in the same namespace.
	for _, ref := range mapOwnerRefs(w.m) {
		if !idx.byKey[objKey(w.namespace, ref.kind, ref.name)] {
			return fmt.Errorf("integrity: %s %q owner reference %s/%q resolves to no object in namespace %q", w.kind, w.name, ref.kind, ref.name, w.namespace)
		}
	}

	switch w.kind {
	case "Pod":
		if node := specString(w.m, "nodeName"); node != "" && !idx.nodes[node] {
			return fmt.Errorf("integrity: Pod %q (namespace %q) spec.nodeName %q resolves to no Node", w.name, w.namespace, node)
		}
		for _, claim := range podClaimNames(w.m) {
			if !idx.pvcsByNS[w.namespace][claim] {
				return fmt.Errorf("integrity: Pod %q (namespace %q) volume claimName %q resolves to no PersistentVolumeClaim", w.name, w.namespace, claim)
			}
		}
	case "Service":
		selector := specStringMap(w.m, "selector")
		if len(selector) > 0 && !selectorMatchesAnyPod(selector, idx.podLabelsByNS[w.namespace]) {
			return fmt.Errorf("integrity: Service %q (namespace %q) selector %v matches no Pod", w.name, w.namespace, selector)
		}
	case "Ingress":
		for _, svc := range ingressServiceNames(w.m) {
			if !idx.servicesByNS[w.namespace][svc] {
				return fmt.Errorf("integrity: Ingress %q (namespace %q) backend service %q resolves to no Service", w.name, w.namespace, svc)
			}
		}
	case "Event":
		if ref, ok := eventInvolvedObject(w.m); ok {
			// The involved object may be namespaced (its own namespace, or the
			// Event's namespace when unset) or cluster-scoped (empty namespace).
			nsKey := objKey(refNamespaceOrDefault(ref, w.namespace), ref.kind, ref.name)
			clusterKey := objKey("", ref.kind, ref.name)
			if !idx.byKey[nsKey] && !idx.byKey[clusterKey] {
				return fmt.Errorf("integrity: Event %q (namespace %q) involvedObject %s/%q resolves to no object", w.name, w.namespace, ref.kind, ref.name)
			}
		}
	case "PersistentVolumeClaim":
		if vol := specString(w.m, "volumeName"); vol != "" && !idx.pvs[vol] {
			return fmt.Errorf("integrity: PersistentVolumeClaim %q (namespace %q) spec.volumeName %q resolves to no PersistentVolume", w.name, w.namespace, vol)
		}
	}
	return nil
}

// --- wire-map accessors -----------------------------------------------------

func toMap(obj runtime.Object) (map[string]any, error) {
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("integrity: marshal object: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("integrity: decode object: %w", err)
	}
	return m, nil
}

func mapMetaString(m map[string]any, key string) string {
	meta, ok := m["metadata"].(map[string]any)
	if !ok {
		return ""
	}
	v, _ := meta[key].(string)
	return v
}

func mapLabels(m map[string]any) map[string]string {
	meta, ok := m["metadata"].(map[string]any)
	if !ok {
		return nil
	}
	raw, ok := meta["labels"].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

type ownerRef struct{ kind, name string }

func mapOwnerRefs(m map[string]any) []ownerRef {
	meta, ok := m["metadata"].(map[string]any)
	if !ok {
		return nil
	}
	raw, ok := meta["ownerReferences"].([]any)
	if !ok {
		return nil
	}
	var refs []ownerRef
	for _, r := range raw {
		ref, ok := r.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := ref["kind"].(string)
		name, _ := ref["name"].(string)
		if kind != "" && name != "" {
			refs = append(refs, ownerRef{kind: kind, name: name})
		}
	}
	return refs
}

func specString(m map[string]any, key string) string {
	spec, ok := m["spec"].(map[string]any)
	if !ok {
		return ""
	}
	v, _ := spec[key].(string)
	return v
}

func specStringMap(m map[string]any, key string) map[string]string {
	spec, ok := m["spec"].(map[string]any)
	if !ok {
		return nil
	}
	raw, ok := spec[key].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// selectorMatchesAnyPod reports whether at least one pod's labels are a
// superset of the selector (the apiserver's label-selector match).
func selectorMatchesAnyPod(selector map[string]string, podLabels []map[string]string) bool {
	for _, labels := range podLabels {
		if labelsMatch(selector, labels) {
			return true
		}
	}
	return false
}

func labelsMatch(selector, labels map[string]string) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// podClaimNames returns the persistentVolumeClaim.claimName of every pod
// volume that has one.
func podClaimNames(m map[string]any) []string {
	spec, ok := m["spec"].(map[string]any)
	if !ok {
		return nil
	}
	volumes, ok := spec["volumes"].([]any)
	if !ok {
		return nil
	}
	var names []string
	for _, v := range volumes {
		vol, ok := v.(map[string]any)
		if !ok {
			continue
		}
		pvc, ok := vol["persistentVolumeClaim"].(map[string]any)
		if !ok {
			continue
		}
		if name, _ := pvc["claimName"].(string); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// ingressServiceNames returns every backend service name referenced by an
// Ingress (default backend + per-path rules), v1 Ingress shape.
func ingressServiceNames(m map[string]any) []string {
	spec, ok := m["spec"].(map[string]any)
	if !ok {
		return nil
	}
	var names []string
	addBackend := func(backend map[string]any) {
		svc, ok := backend["service"].(map[string]any)
		if !ok {
			return
		}
		if name, _ := svc["name"].(string); name != "" {
			names = append(names, name)
		}
	}
	if def, ok := spec["defaultBackend"].(map[string]any); ok {
		addBackend(def)
	}
	rules, _ := spec["rules"].([]any)
	for _, r := range rules {
		rule, ok := r.(map[string]any)
		if !ok {
			continue
		}
		httpRule, ok := rule["http"].(map[string]any)
		if !ok {
			continue
		}
		paths, _ := httpRule["paths"].([]any)
		for _, p := range paths {
			path, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if backend, ok := path["backend"].(map[string]any); ok {
				addBackend(backend)
			}
		}
	}
	return names
}

type involvedRef struct {
	kind      string
	name      string
	namespace string
}

func eventInvolvedObject(m map[string]any) (involvedRef, bool) {
	raw, ok := m["involvedObject"].(map[string]any)
	if !ok {
		return involvedRef{}, false
	}
	kind, _ := raw["kind"].(string)
	name, _ := raw["name"].(string)
	namespace, _ := raw["namespace"].(string)
	if kind == "" || name == "" {
		return involvedRef{}, false
	}
	return involvedRef{kind: kind, name: name, namespace: namespace}, true
}

func refNamespaceOrDefault(ref involvedRef, fallback string) string {
	if ref.namespace != "" {
		return ref.namespace
	}
	return fallback
}
