package kube

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	appconfig "github.com/kbelokon/readout/internal/config"
)

func TestDiscoverClustersSelectionAndManagerReloadFiltering(t *testing.T) {
	static, err := discoverClusters(context.Background(), &appconfig.Config{Clusters: map[string]string{"one/two": "https://one"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(static) != 1 || static[0].Name != "one/two" {
		t.Fatalf("static discoverClusters = %#v", static)
	}
	if _, err := discoverClusters(context.Background(), &appconfig.Config{KubeconfigPath: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Fatal("missing kubeconfig unexpectedly succeeded")
	}
	manager, err := NewManager(context.Background(), &appconfig.Config{
		Clusters:             map[string]string{"one": "https://one"},
		ClusterLabelSelector: map[string]string{"region": "fra1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(manager.Clusters()) != 0 {
		t.Fatalf("label-filtered static clusters = %#v", manager.Clusters())
	}
}

func TestDiscoverRegistryErrorBranches(t *testing.T) {
	if _, err := discoverRegistry(context.Background(), &appconfig.Config{ClusterRegistryURL: "://bad-url"}); err == nil {
		t.Fatal("bad registry URL unexpectedly succeeded")
	}
	dir := t.TempDir()
	if _, err := discoverRegistry(context.Background(), &appconfig.Config{
		ClusterRegistryURL:                   "https://registry.example",
		ClusterRegistryOAuth2BearerTokenPath: filepath.Join(dir, "missing-token"),
	}); err == nil {
		t.Fatal("missing registry token unexpectedly succeeded")
	}

	statusServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusTeapot)
	}))
	defer statusServer.Close()
	if _, err := discoverRegistry(context.Background(), &appconfig.Config{ClusterRegistryURL: statusServer.URL}); err == nil {
		t.Fatal("registry 4xx unexpectedly succeeded")
	}

	jsonServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{`))
	}))
	defer jsonServer.Close()
	if _, err := discoverRegistry(context.Background(), &appconfig.Config{ClusterRegistryURL: jsonServer.URL}); err == nil {
		t.Fatal("bad registry JSON unexpectedly succeeded")
	}

	clusterTokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"alias": "one", "api_server_url": "https://one"}}})
	}))
	defer clusterTokenServer.Close()
	if _, err := discoverRegistry(context.Background(), &appconfig.Config{
		ClusterRegistryURL:   clusterTokenServer.URL,
		ClusterAuthTokenPath: filepath.Join(dir, "missing-cluster-token"),
	}); err == nil {
		t.Fatal("missing cluster token unexpectedly succeeded")
	}
}
