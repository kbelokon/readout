package kube

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	appconfig "github.com/kbelokon/readout/internal/config"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestManagerSelectAndClusterOrdering(t *testing.T) {
	m := &Manager{clusters: map[string]*Cluster{
		"b": {Name: "b"},
		"a": {Name: "a"},
	}}
	if got := m.Clusters(); len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
		t.Fatalf("Clusters ordering = %#v", got)
	}
	if cluster, ok := m.Get("a"); !ok || cluster.Name != "a" {
		t.Fatalf("Get(a) = %#v %v", cluster, ok)
	}
	selected, all, err := m.Select("b,a")
	if err != nil || all || len(selected) != 2 || selected[0].Name != "b" || selected[1].Name != "a" {
		t.Fatalf("Select(b,a) = %#v all=%t err=%v", selected, all, err)
	}
	selected, all, err = m.Select(AllClusters)
	if err != nil || !all || len(selected) != 2 {
		t.Fatalf("Select(_all) = %#v all=%t err=%v", selected, all, err)
	}
	if _, _, err := m.Select("missing"); err == nil {
		t.Fatal("Select(missing) unexpectedly succeeded")
	}
}

func TestManagerHelpers(t *testing.T) {
	if got := SanitizeClusterName("a/b c"); got != "a:b:c" {
		t.Fatalf("SanitizeClusterName = %q", got)
	}
	labels := registryLabels(map[string]any{
		"id":                     "1",
		"channel":                "stage",
		"environment":            "dev",
		"infrastructure_account": "acc",
		"region":                 "fra1",
		"ignored":                "x",
	})
	if labels["infrastructure-account"] != "acc" || labels["ignored"] != "" {
		t.Fatalf("registryLabels = %#v", labels)
	}
	if !labelsMatch(map[string]string{"region": "fra1", "channel!": "prod"}, labels) {
		t.Fatalf("labels should match: %#v", labels)
	}
	if labelsMatch(map[string]string{"channel!": "stage"}, labels) {
		t.Fatalf("negative selector should reject: %#v", labels)
	}
	raw := &clientcmdapi.Config{Contexts: map[string]*clientcmdapi.Context{"ctx": {Cluster: "c", AuthInfo: "u"}}}
	if got := kubeconfigLabels(raw, "ctx"); len(got) != 0 {
		t.Fatalf("kubeconfigLabels should return no labels for a context without them, got %#v", got)
	}
	if got := kubeconfigLabels(raw, "missing"); len(got) != 0 {
		t.Fatalf("missing context labels = %#v", got)
	}
}

func TestDiscoveryAndBearerTokenHelpers(t *testing.T) {
	staticCfg := testConfig(map[string]string{"one": "https://one"})
	static := discoverStatic(&staticCfg)
	if len(static) != 1 || static[0].Name != "one" || static[0].Config.Host != "https://one" {
		t.Fatalf("discoverStatic = %#v", static)
	}
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte("token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &rest.Config{Host: "https://cluster", BearerTokenFile: "old"}
	if err := applyBearerToken(cfg, tokenPath); err != nil {
		t.Fatal(err)
	}
	if cfg.BearerToken != "token" || cfg.BearerTokenFile != "" {
		t.Fatalf("applyBearerToken = %#v", cfg)
	}
	if err := applyBearerToken(cfg, filepath.Join(dir, "missing")); err == nil {
		t.Fatal("applyBearerToken missing file unexpectedly succeeded")
	}
}

func TestDiscoverRegistryFiltersAndLabelsClusters(t *testing.T) {
	var auth string
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.URL.Path != "/kubernetes-clusters" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{
			{
				"alias":                  "stage/one",
				"api_server_url":         "https://one",
				"lifecycle_status":       "ready",
				"id":                     "1",
				"channel":                "stage",
				"environment":            "dev",
				"infrastructure_account": "acc",
				"region":                 "fra1",
			},
			{"alias": "dead", "api_server_url": "https://dead", "lifecycle_status": "deleting"},
			{"alias": "", "api_server_url": "https://missing"},
		}})
	}))
	defer registry.Close()
	dir := t.TempDir()
	registryToken := filepath.Join(dir, "registry-token")
	clusterToken := filepath.Join(dir, "cluster-token")
	if err := os.WriteFile(registryToken, []byte("registry\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(clusterToken, []byte("cluster\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	items, err := discoverRegistry(context.Background(), &appconfig.Config{
		ClusterRegistryURL:                   registry.URL + "/",
		ClusterRegistryOAuth2BearerTokenPath: registryToken,
		ClusterAuthTokenPath:                 clusterToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer registry" || len(items) != 1 {
		t.Fatalf("registry auth/items = %q %#v", auth, items)
	}
	if items[0].Name != "stage/one" || items[0].Config.BearerToken != "cluster" || items[0].Labels["region"] != "fra1" {
		t.Fatalf("registry item mismatch: %#v labels=%#v", items[0], items[0].Labels)
	}
}

func TestDiscoverKubeconfigAndNewManagerStatic(t *testing.T) {
	dir := t.TempDir()
	kubeconfigPath := filepath.Join(dir, "config")
	raw := clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{
			"cluster-a": {Server: "https://a"},
			"cluster-b": {Server: "https://b"},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			"user-a": {Token: "a"},
			"user-b": {Token: "b"},
		},
		Contexts: map[string]*clientcmdapi.Context{
			"ctx-a": {Cluster: "cluster-a", AuthInfo: "user-a"},
			"ctx-b": {Cluster: "cluster-b", AuthInfo: "user-b"},
		},
		CurrentContext: "ctx-a",
	}
	if err := clientcmd.WriteToFile(raw, kubeconfigPath); err != nil {
		t.Fatal(err)
	}
	discovered, err := discoverKubeconfig(&appconfig.Config{KubeconfigPath: kubeconfigPath, KubeconfigContexts: []string{"ctx-b"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(discovered) != 1 || discovered[0].Name != "ctx-b" || discovered[0].Config.Host != "https://b" {
		t.Fatalf("discoverKubeconfig = %#v", discovered)
	}

	manager, err := NewManager(context.Background(), &appconfig.Config{Clusters: map[string]string{"static/one": "https://one"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := manager.Get("static:one"); !ok {
		t.Fatalf("sanitized static cluster not found: %#v", manager.Clusters())
	}
}

func TestDiscoverClustersDispatchesConfiguredSources(t *testing.T) {
	static, err := discoverClusters(context.Background(), &appconfig.Config{Clusters: map[string]string{"one": "https://one"}})
	if err != nil || len(static) != 1 || static[0].Name != "one" {
		t.Fatalf("static discoverClusters = %#v err=%v", static, err)
	}

	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{
			{"alias": "from-registry", "api_server_url": "https://registry-cluster", "lifecycle_status": "ready"},
		}})
	}))
	defer registry.Close()
	fromRegistry, err := discoverClusters(context.Background(), &appconfig.Config{ClusterRegistryURL: registry.URL})
	if err != nil || len(fromRegistry) != 1 || fromRegistry[0].Name != "from-registry" {
		t.Fatalf("registry discoverClusters = %#v err=%v", fromRegistry, err)
	}

	dir := t.TempDir()
	kubeconfigPath := filepath.Join(dir, "config")
	raw := clientcmdapi.Config{
		Clusters:       map[string]*clientcmdapi.Cluster{"cluster-a": {Server: "https://a"}},
		AuthInfos:      map[string]*clientcmdapi.AuthInfo{"user-a": {Token: "a"}},
		Contexts:       map[string]*clientcmdapi.Context{"ctx-a": {Cluster: "cluster-a", AuthInfo: "user-a"}},
		CurrentContext: "ctx-a",
	}
	if err := clientcmd.WriteToFile(raw, kubeconfigPath); err != nil {
		t.Fatal(err)
	}
	fromKubeconfig, err := discoverClusters(context.Background(), &appconfig.Config{KubeconfigPath: kubeconfigPath})
	if err != nil || len(fromKubeconfig) != 1 || fromKubeconfig[0].Name != "ctx-a" {
		t.Fatalf("kubeconfig discoverClusters = %#v err=%v", fromKubeconfig, err)
	}
	t.Setenv("KUBECONFIG", kubeconfigPath)
	fromDefaultKubeconfig, err := discoverClusters(context.Background(), &appconfig.Config{})
	if err != nil || len(fromDefaultKubeconfig) != 1 || fromDefaultKubeconfig[0].Name != "ctx-a" {
		t.Fatalf("default kubeconfig discoverClusters = %#v err=%v", fromDefaultKubeconfig, err)
	}
}

func testConfig(clusters map[string]string) appconfig.Config {
	return appconfig.Config{Clusters: clusters}
}
