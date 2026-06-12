package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kbelokon/readout/internal/config"
	"github.com/kbelokon/readout/internal/kube"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestResourcePrerenderHookUpdatesLinksAndResource(t *testing.T) {
	app := newTestServer(t)
	object := kube.NewObject(&kube.ResourceType{Plural: "pods", Kind: "Pod", Namespaced: true}, &unstructured.Unstructured{
		Object: map[string]any{
			"kind": "Pod",
			"metadata": map[string]any{
				"name":      "nginx",
				"namespace": "default",
			},
		},
	})
	links := []config.Link{{Href: "/base", Title: "Base"}}
	gotLinks, replacement, err := app.resourcePrerenderHook(context.Background(), "test", "default", "pods", &object, links)
	if err != nil || replacement != nil || !reflect.DeepEqual(gotLinks, links) {
		t.Fatalf("empty prerender hook links=%#v replacement=%#v err=%v", gotLinks, replacement, err)
	}

	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Cluster   string        `json:"cluster"`
			Namespace string        `json:"namespace"`
			Plural    string        `json:"plural"`
			Links     []config.Link `json:"links"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Cluster != "test" || payload.Namespace != "default" || payload.Plural != "pods" || len(payload.Links) != 1 {
			t.Fatalf("unexpected prerender payload: %#v", payload)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"links":[{"href":"/extra","title":"Extra","icon":"external-link"}],"resource":{"kind":"Pod","metadata":{"name":"rewritten"}}}`))
	}))
	defer hook.Close()
	app.cfg.ResourcePrerenderHookURL = hook.URL
	gotLinks, replacement, err = app.resourcePrerenderHook(context.Background(), "test", "default", "pods", &object, links)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotLinks) != 2 || gotLinks[1].Href != "/extra" || replacement["kind"] != "Pod" {
		t.Fatalf("prerender result links=%#v replacement=%#v", gotLinks, replacement)
	}
}

func TestLoadPartialsFallbackAndPrecedence(t *testing.T) {
	if got := loadPartials(""); len(got) != 0 {
		t.Fatalf("empty root partials = %#v", got)
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "partials"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "extrahead.html"), []byte("root head"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "footer.html"), []byte("root footer"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "partials", "extrahead.html"), []byte("partial head"), 0o644); err != nil {
		t.Fatal(err)
	}
	partials := loadPartials(root)
	if partials["partials/extrahead.html"] != "partial head" {
		t.Fatalf("partials extrahead precedence = %#v", partials)
	}
	if partials["partials/footer.html"] != "root footer" {
		t.Fatalf("footer fallback = %#v", partials)
	}
}
