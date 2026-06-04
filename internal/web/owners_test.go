package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kbelokon/readout/internal/kube"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestOwnerLinksResolveNamespacedAndClusterOwners(t *testing.T) {
	app := newTestServer(t)
	cluster, ok := app.manager.Get("test")
	if !ok {
		t.Fatal("test cluster not found")
	}
	req := httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods/nginx", nil)
	pod := kube.NewObject(&kube.ResourceType{APIVersion: "v1", Version: "v1", Plural: "pods", Kind: "Pod", Namespaced: true}, &unstructured.Unstructured{Object: map[string]any{
		"kind": "Pod",
		"metadata": map[string]any{
			"name":      "nginx",
			"namespace": "default",
			"ownerReferences": []any{
				map[string]any{"apiVersion": "v1", "kind": "Pod", "name": "owner-pod"},
				map[string]any{"apiVersion": "v1", "kind": "Node", "name": "node-a"},
				map[string]any{"kind": "", "name": "ignored"},
				map[string]any{"apiVersion": "missing/v1", "kind": "Missing", "name": "ignored"},
			},
		},
	}})
	links := app.ownerLinks(req, cluster, &pod)
	if len(links) != 2 {
		t.Fatalf("owner links = %#v", links)
	}
	if !strings.Contains(links[0].Href, "/clusters/test/namespaces/default/pods/owner-pod") || links[0].Title != "Pod/owner-pod" {
		t.Fatalf("pod owner link = %#v", links[0])
	}
	if links[1].Href != "/clusters/test/nodes/node-a" || links[1].Title != "Node/node-a" {
		t.Fatalf("node link = %#v", links[1])
	}

	if links := app.ownerLinks(req, cluster, &kube.Object{Raw: map[string]any{}}); links != nil {
		t.Fatalf("empty owner refs = %#v", links)
	}
}
