package fakekube

// seed.go is the D3 Seed entry point: it turns ONE typed Cluster graph into the
// served store (discovery + List + object + log + metrics responses) the engine
// answers from, replacing the embedded-JSON seedStore path additively. The
// referential-integrity validator (integrity.go) runs FIRST; a dangling
// reference fails here and is never served.
//
// Construction outline:
//   1. validate the cluster's object graph (owner refs, selectors, metrics
//      keys, PVC/PV bindings, CRD discovery coverage) — error names the
//      dangling ref.
//   2. build a fresh store: one List route per (groupVersion, namespace,
//      resource) holding the typed objects marshalled to the apiserver List
//      wire shape, one object route per object, one log route per Pod, the
//      per-namespace metrics list + node metrics list, and discovery documents
//      derived from the kinds actually present plus the registered CRDs.
//   3. swap the store in and re-register the route table (the mux is swappable
//      so post-construction Seed routes resolve; see fakeapi.go).
//
// Table forms are a LATER unit; this unit produces List/object/log/metrics +
// discovery only. The store/route shape already carries a Table slot per list
// (listState.table), so the next unit fills it without reshaping routes.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
)

// gvrInfo is the resource metadata Seed needs to place an object on the wire:
// its API path prefix (/api for the core group, /apis otherwise), group,
// version, resource plural, singular, list kind, and namespaced flag.
type gvrInfo struct {
	apiPrefix  string // "/api" (core) or "/apis"
	group      string // "" for core
	version    string
	resource   string // plural, the path segment
	singular   string
	kind       string
	listKind   string
	namespaced bool
}

// groupVersion renders the discovery groupVersion string ("v1" for core,
// "<group>/<version>" otherwise).
func (g gvrInfo) groupVersion() string {
	if g.group == "" {
		return g.version
	}
	return g.group + "/" + g.version
}

// gvPath renders the group-version path segment under apiPrefix
// (/api/v1 or /apis/<group>/<version>).
func (g gvrInfo) gvPath() string {
	if g.group == "" {
		return g.apiPrefix + "/" + g.version
	}
	return g.apiPrefix + "/" + g.group + "/" + g.version
}

// listPath builds the collection route for a (possibly namespaced) resource.
func (g gvrInfo) listPath(namespace string) string {
	if g.namespaced && namespace != "" {
		return fmt.Sprintf("%s/namespaces/%s/%s", g.gvPath(), namespace, g.resource)
	}
	return g.gvPath() + "/" + g.resource
}

// objectPath builds the single-object route for a named (possibly namespaced)
// object.
func (g gvrInfo) objectPath(namespace, name string) string {
	return g.listPath(namespace) + "/" + name
}

// builtinKinds is the kind registry for the typed Kubernetes objects the demo
// seeds. Keyed by "<group>/<version>/<Kind>" (core group is ""). CRDs extend it
// per Cluster (see kindRegistry). This is a small static map, not a RESTMapper:
// the demo's kind set is closed and the path shapes must match the live-probed
// apiserver exactly.
var builtinKinds = map[string]gvrInfo{
	"/v1/Pod":                   {apiPrefix: "/api", version: "v1", resource: "pods", singular: "pod", kind: "Pod", listKind: "PodList", namespaced: true},
	"/v1/Service":               {apiPrefix: "/api", version: "v1", resource: "services", singular: "service", kind: "Service", listKind: "ServiceList", namespaced: true},
	"/v1/Secret":                {apiPrefix: "/api", version: "v1", resource: "secrets", singular: "secret", kind: "Secret", listKind: "SecretList", namespaced: true},
	"/v1/ConfigMap":             {apiPrefix: "/api", version: "v1", resource: "configmaps", singular: "configmap", kind: "ConfigMap", listKind: "ConfigMapList", namespaced: true},
	"/v1/Event":                 {apiPrefix: "/api", version: "v1", resource: "events", singular: "event", kind: "Event", listKind: "EventList", namespaced: true},
	"/v1/PersistentVolumeClaim": {apiPrefix: "/api", version: "v1", resource: "persistentvolumeclaims", singular: "persistentvolumeclaim", kind: "PersistentVolumeClaim", listKind: "PersistentVolumeClaimList", namespaced: true},
	"/v1/Node":                  {apiPrefix: "/api", version: "v1", resource: "nodes", singular: "node", kind: "Node", listKind: "NodeList", namespaced: false},
	"/v1/PersistentVolume":      {apiPrefix: "/api", version: "v1", resource: "persistentvolumes", singular: "persistentvolume", kind: "PersistentVolume", listKind: "PersistentVolumeList", namespaced: false},
	"/v1/Namespace":             {apiPrefix: "/api", version: "v1", resource: "namespaces", singular: "namespace", kind: "Namespace", listKind: "NamespaceList", namespaced: false},

	"apps/v1/Deployment":  {apiPrefix: "/apis", group: "apps", version: "v1", resource: "deployments", singular: "deployment", kind: "Deployment", listKind: "DeploymentList", namespaced: true},
	"apps/v1/ReplicaSet":  {apiPrefix: "/apis", group: "apps", version: "v1", resource: "replicasets", singular: "replicaset", kind: "ReplicaSet", listKind: "ReplicaSetList", namespaced: true},
	"apps/v1/StatefulSet": {apiPrefix: "/apis", group: "apps", version: "v1", resource: "statefulsets", singular: "statefulset", kind: "StatefulSet", listKind: "StatefulSetList", namespaced: true},
	"apps/v1/DaemonSet":   {apiPrefix: "/apis", group: "apps", version: "v1", resource: "daemonsets", singular: "daemonset", kind: "DaemonSet", listKind: "DaemonSetList", namespaced: true},

	"batch/v1/Job":     {apiPrefix: "/apis", group: "batch", version: "v1", resource: "jobs", singular: "job", kind: "Job", listKind: "JobList", namespaced: true},
	"batch/v1/CronJob": {apiPrefix: "/apis", group: "batch", version: "v1", resource: "cronjobs", singular: "cronjob", kind: "CronJob", listKind: "CronJobList", namespaced: true},

	"networking.k8s.io/v1/Ingress": {apiPrefix: "/apis", group: "networking.k8s.io", version: "v1", resource: "ingresses", singular: "ingress", kind: "Ingress", listKind: "IngressList", namespaced: true},

	"autoscaling/v2/HorizontalPodAutoscaler": {apiPrefix: "/apis", group: "autoscaling", version: "v2", resource: "horizontalpodautoscalers", singular: "horizontalpodautoscaler", kind: "HorizontalPodAutoscaler", listKind: "HorizontalPodAutoscalerList", namespaced: true},
}

// kindRegistry returns the builtin kinds plus one entry per registered CRD, so
// resolveKind covers custom resources. CRD plurals/kinds extend the static map.
func kindRegistry(crds []CRD) map[string]gvrInfo {
	reg := make(map[string]gvrInfo, len(builtinKinds)+len(crds))
	for k, v := range builtinKinds {
		reg[k] = v
	}
	for _, c := range crds {
		key := c.Group + "/" + c.Version + "/" + c.Kind
		reg[key] = gvrInfo{
			apiPrefix:  "/apis",
			group:      c.Group,
			version:    c.Version,
			resource:   c.Plural,
			singular:   strings.ToLower(c.Kind),
			kind:       c.Kind,
			listKind:   c.Kind + "List",
			namespaced: c.Namespaced,
		}
	}
	return reg
}

// gvkKey renders an object's "<group>/<version>/<Kind>" registry key from its
// apiVersion + kind. Core objects (apiVersion "v1") key as "/v1/<Kind>".
func gvkKey(apiVersion, kind string) string {
	if apiVersion == "v1" || apiVersion == "" {
		return "/v1/" + kind
	}
	return apiVersion + "/" + kind
}

// Seed builds the server's served store from ONE typed Cluster graph and swaps
// it in, replacing whatever the store held (the embedded-JSON seed or a prior
// Seed). The referential-integrity validator runs first; a dangling reference
// returns an error and nothing is swapped. Routes are re-registered from the
// freshly seeded path set, so handlers registered before Seed keep resolving
// and the new routes resolve too.
func (s *Server) Seed(c Cluster) error {
	reg := kindRegistry(c.CRDs)
	if err := validateCluster(c, reg); err != nil {
		return err
	}
	st, err := buildStore(c, reg)
	if err != nil {
		return err
	}
	s.store.replaceWith(st)

	mux := http.NewServeMux()
	s.registerRoutes(mux)
	s.muxMu.Lock()
	s.mux = mux
	s.muxMu.Unlock()
	return nil
}

// objectMeta is the metadata Seed reads off a typed object via the meta
// accessor: name, namespace, apiVersion, and kind. apiVersion/kind come from
// the object's GVK (typed objects often carry an empty TypeMeta, so Seed fills
// it from the registry match by Go type when needed — handled at marshal time).
type objectMeta struct {
	name      string
	namespace string
}

// readMeta extracts name + namespace from any runtime.Object.
func readMeta(obj runtime.Object) (objectMeta, error) {
	acc, err := meta.Accessor(obj)
	if err != nil {
		return objectMeta{}, fmt.Errorf("fakeapi: object lacks metadata: %w", err)
	}
	return objectMeta{name: acc.GetName(), namespace: acc.GetNamespace()}, nil
}

// objectGVK resolves an object's group/version/kind. Typed objects frequently
// carry an empty TypeMeta (the decoder fills it, the constructor does not), so
// Seed derives the kind from the Go type name when the embedded TypeMeta is
// blank, then matches it against the registry. The returned apiVersion/kind are
// also stamped onto the marshalled JSON so the wire object self-describes.
func objectGVK(obj runtime.Object, reg map[string]gvrInfo) (gvrInfo, string, string, error) {
	gvk := obj.GetObjectKind().GroupVersionKind()
	apiVersion := gvk.GroupVersion().String()
	kind := gvk.Kind
	if kind == "" {
		// TypeMeta empty: fall back to the Go type name. Typed constructors
		// leave TypeMeta blank, so this is the common path.
		kind = goTypeName(obj)
	}
	// When apiVersion is carried, the (apiVersion, kind) key is exact.
	if apiVersion != "" {
		if info, ok := reg[gvkKey(apiVersion, kind)]; ok {
			return info, info.groupVersion(), info.kind, nil
		}
	}
	// Empty TypeMeta (the common typed-constructor case) or an unmatched
	// apiVersion: resolve by kind alone. Kinds are unique across the registry.
	if info, ok := infoByKind(reg, kind); ok {
		return info, info.groupVersion(), info.kind, nil
	}
	return gvrInfo{}, "", "", fmt.Errorf("fakeapi: no kind registered for %q (kind %q): register a CRD or use a builtin kind", apiVersion, kind)
}

// infoByKind resolves a registry entry by kind alone (the empty-TypeMeta
// path). Kinds are unique across builtins and CRDs, so the match is
// unambiguous; if two CRDs share a Kind in different groups the caller must
// carry TypeMeta to disambiguate (not a case the demo hits).
func infoByKind(reg map[string]gvrInfo, kind string) (gvrInfo, bool) {
	for _, info := range reg {
		if info.kind == kind {
			return info, true
		}
	}
	return gvrInfo{}, false
}

// goTypeName returns the bare Go type name of a typed object (e.g. "Pod" for
// *corev1.Pod), used as the kind when the object's TypeMeta is empty.
func goTypeName(obj runtime.Object) string {
	t := fmt.Sprintf("%T", obj)
	if i := strings.LastIndex(t, "."); i >= 0 {
		t = t[i+1:]
	}
	return strings.TrimPrefix(t, "*")
}

// marshalObject renders a typed object to the apiserver JSON wire shape, with
// apiVersion + kind stamped (typed constructors leave TypeMeta blank). The
// returned map is the object's wire form, also used as a List item.
func marshalObject(obj runtime.Object, info gvrInfo, apiVersion, kind string) (map[string]any, error) {
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("fakeapi: marshal %s/%s: %w", apiVersion, kind, err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if _, ok := m["apiVersion"]; !ok || m["apiVersion"] == "" {
		m["apiVersion"] = apiVersion
	}
	if _, ok := m["kind"]; !ok || m["kind"] == "" {
		m["kind"] = kind
	}
	return m, nil
}

// seededObject couples an object's wire form with its resolved resource info
// and identity, the unit of work buildStore assembles lists/objects/logs from.
type seededObject struct {
	info       gvrInfo
	apiVersion string
	kind       string
	namespace  string
	name       string
	wire       map[string]any
	isPod      bool
}

// collectObjects resolves and marshals every object in the cluster (nodes,
// cluster objects, namespaced objects) once, plus a synthetic Namespace object
// per declared namespace so the namespaces list/object routes resolve.
func collectObjects(c Cluster, reg map[string]gvrInfo) ([]seededObject, error) {
	var out []seededObject
	add := func(obj runtime.Object, nsOverride string) error {
		info, apiVersion, kind, err := objectGVK(obj, reg)
		if err != nil {
			return err
		}
		m, err := readMeta(obj)
		if err != nil {
			return err
		}
		ns := m.namespace
		if info.namespaced && nsOverride != "" {
			ns = nsOverride
		}
		wire, err := marshalObject(obj, info, apiVersion, kind)
		if err != nil {
			return err
		}
		out = append(out, seededObject{
			info:       info,
			apiVersion: apiVersion,
			kind:       kind,
			namespace:  ns,
			name:       m.name,
			wire:       wire,
			isPod:      info.group == "" && info.kind == "Pod",
		})
		return nil
	}

	for _, n := range c.Nodes {
		if err := add(n, ""); err != nil {
			return nil, err
		}
	}
	for _, o := range c.ClusterObjects {
		if err := add(o, ""); err != nil {
			return nil, err
		}
	}
	for _, ns := range c.Namespaces {
		// Synthetic Namespace object so the namespaces list/object routes serve.
		nsObj := namespaceWire(ns)
		out = append(out, seededObject{
			info:       builtinKinds["/v1/Namespace"],
			apiVersion: "v1",
			kind:       "Namespace",
			namespace:  "",
			name:       ns.Name,
			wire:       nsObj,
		})
		for _, o := range ns.Objects {
			if err := add(o, ns.Name); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// namespaceWire builds the served Namespace object for a declared namespace.
func namespaceWire(ns Namespace) map[string]any {
	metadata := map[string]any{"name": ns.Name}
	if len(ns.Labels) > 0 {
		labels := make(map[string]any, len(ns.Labels))
		for k, v := range ns.Labels {
			labels[k] = v
		}
		metadata["labels"] = labels
	}
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata":   metadata,
		"status":     map[string]any{"phase": "Active"},
	}
}

// buildStore assembles the served store from the resolved objects: object
// routes, namespace-grouped List routes (with the cross-namespace alias route
// for namespaced kinds, mirroring /api/v1/pods sharing /api/v1/namespaces/*/pods
// state), pod-log routes, metrics list routes, and discovery documents.
func buildStore(c Cluster, reg map[string]gvrInfo) (*store, error) {
	objs, err := collectObjects(c, reg)
	if err != nil {
		return nil, err
	}

	st := &store{
		rv:        100000,
		discovery: map[string][]byte{},
		objects:   map[string][]byte{},
		logs:      map[string][]byte{},
		lists:     map[string]*listState{},
	}

	// listBuckets groups wire objects by their served collection route. The
	// cross-namespace route (e.g. /api/v1/pods) and the per-namespace route
	// (/api/v1/namespaces/<ns>/pods) are separate keys: the namespaced route
	// holds that namespace's items, the all-namespaces route holds them all.
	listBuckets := map[string]*listBucket{}
	bucketFor := func(path string, info gvrInfo) *listBucket {
		b, ok := listBuckets[path]
		if !ok {
			b = &listBucket{info: info}
			listBuckets[path] = b
		}
		return b
	}

	for _, o := range objs {
		// Object route.
		objData, err := json.Marshal(o.wire)
		if err != nil {
			return nil, err
		}
		st.objects[o.info.objectPath(o.namespace, o.name)] = objData

		// List routes: the namespaced (or cluster-scoped) collection, plus the
		// all-namespaces alias for namespaced kinds.
		bucketFor(o.info.listPath(o.namespace), o.info).items = append(
			bucketFor(o.info.listPath(o.namespace), o.info).items, o.wire)
		if o.info.namespaced {
			allPath := o.info.gvPath() + "/" + o.info.resource
			bucketFor(allPath, o.info).items = append(bucketFor(allPath, o.info).items, o.wire)
		}
	}

	for path, b := range listBuckets {
		doc, err := listDocFor(b)
		if err != nil {
			return nil, err
		}
		// Fill the Table slot too (D5): a client that negotiates as=Table gets
		// the meta.k8s.io Table form, with the SAME item set as the List form and
		// the full object riding each row (readout's curated cells re-read it).
		st.lists[path] = &listState{list: doc, table: buildTableDoc(b)}
	}

	// Ensure an EMPTY list route exists for every (resource, namespace) that a
	// CRD or kind declares but has no objects in — not strictly required here,
	// but keeps the route registered so a list returns [] instead of 404.
	// (Handled implicitly: only routes with at least one object are created;
	// the demo seeds objects for every served kind.)

	// Pod log routes: a deterministic log line per pod.
	for _, o := range objs {
		if o.isPod {
			logPath := o.info.objectPath(o.namespace, o.name) + "/log"
			st.logs[logPath] = []byte(fmt.Sprintf("seeded log for pod %s/%s\n", o.namespace, o.name))
		}
	}

	// Metrics list routes (JSON wire shape; metrics.k8s.io is not a dep).
	if err := buildMetrics(c, st); err != nil {
		return nil, err
	}

	// Discovery documents derived from the kinds present + registered CRDs.
	if err := buildDiscovery(c, objs, st); err != nil {
		return nil, err
	}

	return st, nil
}

// listBucket accumulates the wire items for one collection route.
type listBucket struct {
	info  gvrInfo
	items []map[string]any
}

// listDocFor renders one collection's List wire document (kind <Kind>List,
// items the bucket's objects), sorted by name for deterministic output.
func listDocFor(b *listBucket) (map[string]any, error) {
	items := make([]map[string]any, len(b.items))
	copy(items, b.items)
	sort.SliceStable(items, func(i, j int) bool {
		return itemName(items[i]) < itemName(items[j])
	})
	anyItems := make([]any, len(items))
	for i, it := range items {
		anyItems[i] = it
	}
	apiVersion := b.info.groupVersion()
	return map[string]any{
		"apiVersion": apiVersion,
		"kind":       b.info.listKind,
		"metadata":   map[string]any{"resourceVersion": "100000"},
		"items":      anyItems,
	}, nil
}

func itemName(m map[string]any) string {
	meta, ok := m["metadata"].(map[string]any)
	if !ok {
		return ""
	}
	name, _ := meta["name"].(string)
	return name
}

// buildMetrics seeds the per-namespace PodMetrics list routes and the cluster
// NodeMetrics list route in the metrics.k8s.io JSON wire shape.
func buildMetrics(c Cluster, st *store) error {
	const mAPI = "metrics.k8s.io/v1beta1"
	for _, ns := range c.Namespaces {
		if len(ns.PodMetrics) == 0 {
			continue
		}
		items := make([]any, 0, len(ns.PodMetrics))
		for _, pm := range ns.PodMetrics {
			containers := make([]any, 0, len(pm.Containers))
			for _, ct := range pm.Containers {
				containers = append(containers, map[string]any{
					"name":  ct.Name,
					"usage": map[string]any{"cpu": ct.CPU, "memory": ct.Memory},
				})
			}
			items = append(items, map[string]any{
				"kind":       "PodMetrics",
				"apiVersion": mAPI,
				"metadata":   map[string]any{"name": pm.Name, "namespace": ns.Name},
				"containers": containers,
			})
		}
		doc := map[string]any{
			"kind":       "PodMetricsList",
			"apiVersion": mAPI,
			"metadata":   map[string]any{"resourceVersion": "100000"},
			"items":      items,
		}
		st.lists[fmt.Sprintf("/apis/metrics.k8s.io/v1beta1/namespaces/%s/pods", ns.Name)] = &listState{list: doc}
		st.lists["/apis/metrics.k8s.io/v1beta1/pods"] = mergeMetricsAll(st.lists["/apis/metrics.k8s.io/v1beta1/pods"], items, mAPI, "PodMetricsList")
	}
	if len(c.NodeMetrics) > 0 {
		items := make([]any, 0, len(c.NodeMetrics))
		for _, nm := range c.NodeMetrics {
			items = append(items, map[string]any{
				"kind":       "NodeMetrics",
				"apiVersion": mAPI,
				"metadata":   map[string]any{"name": nm.Name},
				"usage":      map[string]any{"cpu": nm.CPU, "memory": nm.Memory},
			})
		}
		st.lists["/apis/metrics.k8s.io/v1beta1/nodes"] = &listState{list: map[string]any{
			"kind":       "NodeMetricsList",
			"apiVersion": mAPI,
			"metadata":   map[string]any{"resourceVersion": "100000"},
			"items":      items,
		}}
	}
	return nil
}

// buildDiscovery derives the discovery documents from the resource kinds that
// actually appear in the cluster plus the registered CRDs and the metrics
// group: /api (APIVersions), /api/v1 (core APIResourceList), /apis
// (APIGroupList), and one /apis/<group>/<version> APIResourceList per non-core
// group present. A CR object whose group-version is not registered fails the
// integrity validator before this runs, so every served kind has discovery.
func buildDiscovery(c Cluster, objs []seededObject, st *store) error {
	// Collect the gvrInfo for every kind that appears, deduped by groupVersion
	// + resource. The metrics group is always present (pods + nodes metrics).
	type gvKey struct{ apiPrefix, group, version string }
	groups := map[gvKey][]gvrInfo{}
	seen := map[string]bool{}
	addInfo := func(info gvrInfo) {
		dedupe := info.groupVersion() + "/" + info.resource
		if seen[dedupe] {
			return
		}
		seen[dedupe] = true
		k := gvKey{info.apiPrefix, info.group, info.version}
		groups[k] = append(groups[k], info)
	}
	for _, o := range objs {
		addInfo(o.info)
	}
	// Namespace kind is always served (synthetic objects) — collectObjects adds
	// it, so it is already covered. Metrics resources:
	addInfo(gvrInfo{apiPrefix: "/apis", group: "metrics.k8s.io", version: "v1beta1", resource: "pods", singular: "", kind: "PodMetrics", listKind: "PodMetricsList", namespaced: true})
	addInfo(gvrInfo{apiPrefix: "/apis", group: "metrics.k8s.io", version: "v1beta1", resource: "nodes", singular: "", kind: "NodeMetrics", listKind: "NodeMetricsList", namespaced: false})

	// /api (core APIVersions) + /api/v1 (core resource list).
	apiVersions := map[string]any{
		"kind":     "APIVersions",
		"versions": []any{"v1"},
		"serverAddressByClientCIDRs": []any{
			map[string]any{"clientCIDR": "0.0.0.0/0", "serverAddress": "127.0.0.1:6443"},
		},
	}
	if data, err := json.Marshal(apiVersions); err == nil {
		st.discovery["/api"] = data
	} else {
		return err
	}

	// Non-core groups present, for the /apis APIGroupList. Sorted for
	// deterministic output.
	groupNames := map[string]string{} // group -> version (one version per group here)
	for k := range groups {
		if k.group == "" {
			continue
		}
		groupNames[k.group] = k.version
	}

	// Emit each group-version's APIResourceList.
	for k, infos := range groups {
		resources := resourceListFor(infos, k.group == "")
		doc := map[string]any{
			"kind":         "APIResourceList",
			"groupVersion": gvString(k.group, k.version),
			"resources":    resources,
		}
		data, err := json.Marshal(doc)
		if err != nil {
			return err
		}
		st.discovery[gvDiscoveryPath(k.apiPrefix, k.group, k.version)] = data
	}

	// /apis APIGroupList.
	sortedGroups := make([]string, 0, len(groupNames))
	for g := range groupNames {
		sortedGroups = append(sortedGroups, g)
	}
	sort.Strings(sortedGroups)
	groupEntries := make([]any, 0, len(sortedGroups))
	for _, g := range sortedGroups {
		gv := gvString(g, groupNames[g])
		groupEntries = append(groupEntries, map[string]any{
			"name":             g,
			"versions":         []any{map[string]any{"groupVersion": gv, "version": groupNames[g]}},
			"preferredVersion": map[string]any{"groupVersion": gv, "version": groupNames[g]},
		})
	}
	apiGroupList := map[string]any{
		"kind":       "APIGroupList",
		"apiVersion": "v1",
		"groups":     groupEntries,
	}
	data, err := json.Marshal(apiGroupList)
	if err != nil {
		return err
	}
	st.discovery["/apis"] = data
	return nil
}

// resourceListFor renders the resources array of an APIResourceList. core marks
// the core group (adds the pods/log subresource so log routes are discoverable).
func resourceListFor(infos []gvrInfo, core bool) []any {
	sort.SliceStable(infos, func(i, j int) bool { return infos[i].resource < infos[j].resource })
	resources := make([]any, 0, len(infos)+1)
	for _, info := range infos {
		entry := map[string]any{
			"name":         info.resource,
			"singularName": info.singular,
			"namespaced":   info.namespaced,
			"kind":         info.kind,
			"verbs":        []any{"get", "list", "watch"},
		}
		resources = append(resources, entry)
		if core && info.kind == "Pod" {
			resources = append(resources, map[string]any{
				"name":         "pods/log",
				"singularName": "",
				"namespaced":   true,
				"kind":         "Pod",
				"verbs":        []any{"get"},
			})
		}
	}
	return resources
}

func gvString(group, version string) string {
	if group == "" {
		return version
	}
	return group + "/" + version
}

func gvDiscoveryPath(apiPrefix, group, version string) string {
	if group == "" {
		return apiPrefix + "/" + version
	}
	return apiPrefix + "/" + group + "/" + version
}

// mergeMetricsAll appends items to the all-namespaces pod-metrics list,
// creating it on first use.
func mergeMetricsAll(existing *listState, items []any, apiVersion, listKind string) *listState {
	if existing == nil {
		existing = &listState{list: map[string]any{
			"kind":       listKind,
			"apiVersion": apiVersion,
			"metadata":   map[string]any{"resourceVersion": "100000"},
			"items":      []any{},
		}}
	}
	cur, _ := existing.list["items"].([]any)
	existing.list["items"] = append(cur, items...)
	return existing
}
