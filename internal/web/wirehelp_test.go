package web

import (
	"net/http"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	fakeapi "github.com/kbelokon/readout/internal/fakekube"
)

// wirehelp_test.go replaces the deleted embedded-JSON fixture plumbing the
// bespoke hand-built mux tests used to read (the old embedded-bytes + filesystem
// fixture readers). Each
// hand mux now builds a small typed fakekube.Cluster for exactly what it asserts
// and serves the engine's WIRE BYTES for a route (discovery docs + Table/List
// forms) through fakeapi.WireResponses, reusing the same validateCluster +
// buildStore pipeline a running Server runs — no JSON files, no running server.
//
// The Table cells are DERIVED by the engine's tableForKind extractors from the
// typed object (the hand muxes assert row NAMES / counts / the <mark> split, not
// the Age/Restart cell text), so no explicit-cell override is needed here.

// buildWire turns a typed Cluster into its served wire bytes, failing the test
// on a build/validation error.
func buildWire(t *testing.T, c *fakeapi.Cluster) *fakeapi.Wire {
	t.Helper()
	w, err := fakeapi.WireResponses(c)
	if err != nil {
		t.Fatalf("build wire responses: %v", err)
	}
	return w
}

// registerDiscovery wires every discovery document the cluster serves
// (/api, /api/v1, /apis, each group doc, the metrics group doc) onto mux through
// the supplied wrap (delay / plain). The mux must advertise exactly the groups
// it serves so client-go's discovery walk stays consistent.
func registerDiscovery(mux *http.ServeMux, w *fakeapi.Wire, wrap func(http.HandlerFunc) http.HandlerFunc) {
	for path, data := range w.Discovery {
		mux.HandleFunc(path, wrap(jsonBytes(data)))
	}
}

// plainWrap is the identity handler wrapper (no delay), for muxes that serve
// their discovery docs without an injected stall.
func plainWrap(h http.HandlerFunc) http.HandlerFunc { return h }

// jsonBytes serves a fixed JSON body.
func jsonBytes(data []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}
}

// podsScenarioCluster is the typed scenario the default (non-search) hand muxes
// assert: the nginx + my-app pods (Table rows nginx/my-app, List form carrying
// nodeName 127.0.0.1 for the join), the 127.0.0.1 join node, and the
// default/kube-system/my-app namespaces. It reproduces the data the
// data/pods_table.json + data/pods_with_node_list.json + render_namespaces_list
// fixtures carried, now from typed objects.
func podsScenarioCluster() fakeapi.Cluster {
	nginx := wirePod("nginx", "nginx:1.27", map[string]string{"app": "nginx"}, "2024-03-01T10:00:00Z")
	myApp := wirePod("my-app", "my-app:latest", map[string]string{"app": "my-app"}, "2024-03-02T11:30:00Z")
	return fakeapi.Cluster{
		Name:  "test",
		Nodes: []runtime.Object{wireJoinNode()},
		Namespaces: []fakeapi.Namespace{
			{Name: "default", Created: "2024-01-01T00:00:00Z", Labels: map[string]string{"team": "core"}, Objects: []runtime.Object{nginx, myApp}},
			{Name: "kube-system", Created: "2024-01-02T00:00:00Z", Labels: map[string]string{}},
			{Name: "my-app", Created: "2024-01-03T00:00:00Z", Labels: map[string]string{"app": "my-app"}},
		},
	}
}

// searchScenarioCluster is the typed scenario the grouped-search hand muxes
// assert: three pods (api-backend / metrics-api / redis-master-0 — the first two
// match `q=api`, redis-master is filtered out) and one api-gateway deployment,
// so the search `<mark>` highlight + the multi-kind totals read the same data
// the data/search_pods_table.json + data/search_deployments_table.json fixtures
// carried.
func searchScenarioCluster() fakeapi.Cluster {
	apiBackend := wirePod("api-backend-7c9f7cd495-6fff6", "api-backend:latest", map[string]string{"app": "api-backend"}, "2024-03-01T10:00:00Z")
	metricsAPI := wirePod("metrics-api-6b9d4c8f5d-q2w4e", "metrics-api:latest", map[string]string{"app": "metrics-api"}, "2024-03-01T11:00:00Z")
	redis := wirePod("redis-master-0", "redis:7", map[string]string{"app": "redis"}, "2024-03-01T09:00:00Z")
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api-gateway", Namespace: "default", CreationTimestamp: wireTS("2024-03-01T08:00:00Z"), Labels: map[string]string{"app": "api-gateway"}},
		Spec:       appsv1.DeploymentSpec{Replicas: wireI32(1)},
		Status:     appsv1.DeploymentStatus{Replicas: 1, ReadyReplicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1},
	}
	return fakeapi.Cluster{
		Name:  "test",
		Nodes: []runtime.Object{wireJoinNode()},
		Namespaces: []fakeapi.Namespace{
			{Name: "default", Created: "2024-01-01T00:00:00Z", Labels: map[string]string{"team": "core"}, Objects: []runtime.Object{apiBackend, metricsAPI, redis, dep}},
			{Name: "kube-system", Created: "2024-01-02T00:00:00Z", Labels: map[string]string{}},
			{Name: "my-app", Created: "2024-01-03T00:00:00Z", Labels: map[string]string{"app": "my-app"}},
		},
	}
}

// wirePod builds a Running pod with one container on the 127.0.0.1 join node,
// the shape the pods Table/List forms carry (the join reads nodeName/podIP).
func wirePod(name, image string, labels map[string]string, created string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", CreationTimestamp: wireTS(created), Labels: labels},
		Spec:       corev1.PodSpec{NodeName: "127.0.0.1", Containers: []corev1.Container{{Name: name, Image: image}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning, PodIP: "127.0.0.1",
			ContainerStatuses: []corev1.ContainerStatus{{Name: name, Ready: true, Image: image}},
		},
	}
}

// wireJoinNode is the 127.0.0.1 node the default pods run on, so a pods
// join=nodes custom column resolves node.metadata.name.
func wireJoinNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "127.0.0.1", Labels: map[string]string{"kubernetes.io/hostname": "127.0.0.1"}},
		Status:     corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "127.0.0.1"}}},
	}
}

func wireTS(rfc3339 string) metav1.Time {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		panic("wirehelp: bad timestamp " + rfc3339 + ": " + err.Error())
	}
	return metav1.NewTime(t)
}

func wireI32(v int32) *int32 { return &v }
