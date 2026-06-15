package fakekube_test

// scenario_test.go pins the typed-graph Seed path and the integrity law:
// a small scenario yields the expected List/object/log/metrics/discovery
// responses, and a scenario with a dangling reference is rejected at Seed time
// (never served). The embedded-JSON seed path (fakeapi_test.go) stays green in
// parallel — Seed is additive.

import (
	"net/http"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	fakeapi "github.com/kbelokon/readout/internal/fakekube"
)

// goodCluster is a small but referentially-complete cluster: a Node, a
// Deployment owning a ReplicaSet owning a Pod scheduled on the Node, a Service
// whose selector matches the Pod, pod + node metrics keyed to real names.
func goodCluster() fakeapi.Cluster {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
	}
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
	}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "web-abc",
			Namespace:       "default",
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web"}},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "web-abc-123",
			Namespace:       "default",
			Labels:          map[string]string{"app": "web"},
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-abc"}},
		},
		Spec: corev1.PodSpec{NodeName: "worker-1"},
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "web"}},
	}
	return fakeapi.Cluster{
		Name:  "prod",
		Nodes: []runtime.Object{node},
		Namespaces: []fakeapi.Namespace{{
			Name:    "default",
			Labels:  map[string]string{"env": "prod"},
			Objects: []runtime.Object{deploy, rs, pod, svc},
			PodMetrics: []fakeapi.PodMetric{{
				Name:       "web-abc-123",
				Containers: []fakeapi.ContainerMetric{{Name: "web", CPU: "250m", Memory: "128Mi"}},
			}},
		}},
		NodeMetrics: []fakeapi.NodeMetric{{Name: "worker-1", CPU: "1", Memory: "256Mi"}},
	}
}

func seededServer(t *testing.T, c *fakeapi.Cluster) *fakeapi.Server {
	t.Helper()
	srv, err := fakeapi.New(fakeapi.WithoutControl())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	if err := srv.Seed(c); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	return srv
}

func TestSeed(t *testing.T) {
	c := goodCluster()
	srv := seededServer(t, &c)

	// List form: pods in default, with the all-namespaces alias sharing it.
	t.Run("pod list", func(t *testing.T) {
		status, doc := get(t, srv.URL+"/api/v1/namespaces/default/pods", "")
		if status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}
		if kind, _ := doc["kind"].(string); kind != "PodList" {
			t.Fatalf("kind = %q, want PodList", kind)
		}
		items, _ := doc["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("items = %d, want 1", len(items))
		}
		if got := itemNameAt(items, 0); got != "web-abc-123" {
			t.Fatalf("item name = %q", got)
		}
		// The all-namespaces alias serves the same pod.
		_, allDoc := get(t, srv.URL+"/api/v1/pods", "")
		if all, _ := allDoc["items"].([]any); len(all) != 1 {
			t.Fatalf("all-namespaces pods = %d, want 1", len(all))
		}
	})

	// Object form.
	t.Run("pod object", func(t *testing.T) {
		status, doc := get(t, srv.URL+"/api/v1/namespaces/default/pods/web-abc-123", "")
		if status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}
		if kind, _ := doc["kind"].(string); kind != "Pod" {
			t.Fatalf("kind = %q, want Pod", kind)
		}
		if name := metaName(doc); name != "web-abc-123" {
			t.Fatalf("name = %q", name)
		}
	})

	// Log form.
	t.Run("pod log", func(t *testing.T) {
		body := getText(t, srv.URL+"/api/v1/namespaces/default/pods/web-abc-123/log")
		if !strings.Contains(body, "web-abc-123") {
			t.Fatalf("log body = %q", body)
		}
	})

	// Metrics form.
	t.Run("pod metrics", func(t *testing.T) {
		status, doc := get(t, srv.URL+"/apis/metrics.k8s.io/v1beta1/namespaces/default/pods", "")
		if status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}
		items, _ := doc["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("pod metrics items = %d, want 1", len(items))
		}
		if got := itemNameAt(items, 0); got != "web-abc-123" {
			t.Fatalf("metrics name = %q", got)
		}
	})

	t.Run("node metrics", func(t *testing.T) {
		_, doc := get(t, srv.URL+"/apis/metrics.k8s.io/v1beta1/nodes", "")
		items, _ := doc["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("node metrics items = %d, want 1", len(items))
		}
		if got := itemNameAt(items, 0); got != "worker-1" {
			t.Fatalf("node metrics name = %q", got)
		}
	})

	// Discovery: the deployment's group-version is registered, so the apps/v1
	// list route resolves (a missing discovery group-version would 404 it).
	t.Run("discovery + apps list", func(t *testing.T) {
		status, doc := get(t, srv.URL+"/apis/apps/v1", "")
		if status != http.StatusOK {
			t.Fatalf("apps/v1 discovery status = %d", status)
		}
		if kind, _ := doc["kind"].(string); kind != "APIResourceList" {
			t.Fatalf("discovery kind = %q", kind)
		}
		dstatus, dl := get(t, srv.URL+"/apis/apps/v1/namespaces/default/deployments", "")
		if dstatus != http.StatusOK {
			t.Fatalf("deployments list status = %d", dstatus)
		}
		if items, _ := dl["items"].([]any); len(items) != 1 {
			t.Fatalf("deployments = %d, want 1", len(items))
		}
	})
}

func TestSeedIntegrity(t *testing.T) {
	t.Run("service selector matching no pod", func(t *testing.T) {
		c := goodCluster()
		// Mutate the Service selector so it matches no pod in the namespace.
		for _, obj := range c.Namespaces[0].Objects {
			if svc, ok := obj.(*corev1.Service); ok {
				svc.Spec.Selector = map[string]string{"app": "nonexistent"}
			}
		}
		srv, err := fakeapi.New(fakeapi.WithoutControl())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(srv.Close)
		err = srv.Seed(&c)
		if err == nil {
			t.Fatal("Seed accepted a Service selector matching no Pod; want a dangling-reference error")
		}
		if !strings.Contains(err.Error(), "matches no Pod") {
			t.Fatalf("error = %q, want it to name the dangling Service selector", err)
		}
	})

	t.Run("dangling owner reference", func(t *testing.T) {
		c := goodCluster()
		for _, obj := range c.Namespaces[0].Objects {
			if rs, ok := obj.(*appsv1.ReplicaSet); ok {
				rs.OwnerReferences = []metav1.OwnerReference{{Kind: "Deployment", Name: "ghost"}}
			}
		}
		if err := mustSeedErr(&c); err == nil || !strings.Contains(err.Error(), "owner reference") {
			t.Fatalf("error = %v, want owner reference dangling", err)
		}
	})

	t.Run("pod on missing node", func(t *testing.T) {
		c := goodCluster()
		for _, obj := range c.Namespaces[0].Objects {
			if pod, ok := obj.(*corev1.Pod); ok {
				pod.Spec.NodeName = "ghost-node"
			}
		}
		if err := mustSeedErr(&c); err == nil || !strings.Contains(err.Error(), "spec.nodeName") {
			t.Fatalf("error = %v, want nodeName dangling", err)
		}
	})

	t.Run("pod metrics for missing pod", func(t *testing.T) {
		c := goodCluster()
		c.Namespaces[0].PodMetrics[0].Name = "ghost-pod"
		if err := mustSeedErr(&c); err == nil || !strings.Contains(err.Error(), "references no Pod") {
			t.Fatalf("error = %v, want pod-metrics dangling", err)
		}
	})

	t.Run("good cluster passes", func(t *testing.T) {
		c := goodCluster()
		if err := seedOnly(&c); err != nil {
			t.Fatalf("good cluster rejected: %v", err)
		}
	})
}

// mustSeedErr seeds a throwaway server and returns the Seed error (server is
// closed on return).
func mustSeedErr(c *fakeapi.Cluster) error {
	return seedOnly(c)
}

func seedOnly(c *fakeapi.Cluster) error {
	srv, err := fakeapi.New(fakeapi.WithoutControl())
	if err != nil {
		return err
	}
	defer srv.Close()
	return srv.Seed(c)
}

// --- helpers ---------------------------------------------------------------

func metaName(doc map[string]any) string {
	meta, _ := doc["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	return name
}

func itemNameAt(items []any, i int) string {
	if i >= len(items) {
		return ""
	}
	m, _ := items[i].(map[string]any)
	return metaName(m)
}

func getText(t *testing.T, url string) string {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	buf := make([]byte, 4096)
	n, _ := res.Body.Read(buf)
	return string(buf[:n])
}
