package kube

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	appconfig "github.com/kbelokon/readout/internal/config"
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

func TestSanitizeClusterName(t *testing.T) {
	if got := SanitizeClusterName("a/b c"); got != "a:b:c" {
		t.Fatalf("SanitizeClusterName = %q", got)
	}
}

// TestDiscoverStaticBuildsConnectionThroughClientcmd pins that a static cluster's
// rest.Config is produced via the Connection model (clientcmd), carrying the
// configured server as Host.
func TestDiscoverStaticBuildsConnectionThroughClientcmd(t *testing.T) {
	cfg := &appconfig.Config{Clusters: []appconfig.ClusterConnection{{Name: "one", Server: "https://one"}}}
	got := discoverStatic(cfg)
	if len(got) != 1 || got[0].Err != nil || got[0].Name != "one" || got[0].Config.Host != "https://one" {
		t.Fatalf("discoverStatic = %#v", got)
	}
	if got[0].Source != SourceStatic {
		t.Fatalf("static source = %v", got[0].Source)
	}
}

// TestStaticAuthThreadsBearerToken is the D8a regression guard: a static cluster
// configured with a token must reach the apiserver as Bearer auth, NOT silently
// anonymous (the old discoverStatic dropped it). Verified end-to-end against the
// auth-capturing TLS server.
func TestStaticAuthThreadsBearerToken(t *testing.T) {
	srv, rec := newAuthCapturingTLSServer(t)
	cfg := &appconfig.Config{Clusters: []appconfig.ClusterConnection{{
		Name:                     "prod",
		Server:                   srv.URL,
		CertificateAuthorityData: serverCAPEM(t, srv),
		Token:                    "static-token",
	}}}
	m, err := NewManager(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	cluster, ok := m.Get("prod")
	if !ok {
		t.Fatalf("static cluster not loaded: %#v / broken=%#v", m.Clusters(), m.Broken())
	}
	if _, _, err := cluster.Client.ResourceTypes(context.Background()); err != nil {
		t.Fatalf("discovery against static TLS cluster: %v", err)
	}
	if rec.Authorization() != "Bearer static-token" {
		t.Fatalf("static cluster reached apiserver as %q, want Bearer static-token (silent anonymous regression)", rec.Authorization())
	}
}

// TestStaticAnonymousLoads pins that a static cluster with no auth still loads as
// an anonymous connection (identity supplied per request) -- no Authorization.
func TestStaticAnonymousLoads(t *testing.T) {
	srv, rec := newAuthCapturingTLSServer(t)
	cfg := &appconfig.Config{Clusters: []appconfig.ClusterConnection{{
		Name:                     "anon",
		Server:                   srv.URL,
		CertificateAuthorityData: serverCAPEM(t, srv),
	}}}
	m, err := NewManager(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	cluster, ok := m.Get("anon")
	if !ok {
		t.Fatalf("anonymous static cluster not loaded: broken=%#v", m.Broken())
	}
	if _, _, err := cluster.Client.ResourceTypes(context.Background()); err != nil {
		t.Fatalf("discovery: %v", err)
	}
	if rec.Authorization() != "" {
		t.Fatalf("anonymous cluster sent Authorization %q", rec.Authorization())
	}
}

// TestStaticNonHTTPSWithAuthIsBroken pins the finding-C guard: a static cluster
// that sets TLS/auth fields on a non-https server is surfaced as broken (clientcmd
// would silently drop the credentials), not run as a silently-anonymous cluster.
func TestStaticNonHTTPSWithAuthIsBroken(t *testing.T) {
	cfg := &appconfig.Config{Clusters: []appconfig.ClusterConnection{{
		Name:   "insecure",
		Server: "http://plain.example",
		Token:  "would-be-dropped",
	}}}
	m, err := NewManager(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Get("insecure"); ok {
		t.Fatal("non-https cluster with auth should not load")
	}
	broken := m.Broken()
	if len(broken) != 1 || broken[0].Name != "insecure" {
		t.Fatalf("expected one broken cluster, got %#v", broken)
	}
}

// TestLoadMultiSourceCoexistAndPerContextError pins D3: static and kubeconfig
// sources COEXIST (no longer mutually exclusive), and a malformed cluster is
// skipped-with-error without failing its siblings.
func TestLoadMultiSourceCoexistAndPerContextError(t *testing.T) {
	good := newTLSFakeAPIServer(t)
	kubeconfigPath := writeKubeconfig(t, map[string]string{"ctx-a": "https://a"})

	cfg := &appconfig.Config{
		Clusters: []appconfig.ClusterConnection{
			{Name: "static-good", Server: good.URL, CertificateAuthorityData: serverCAPEM(t, good)},
			{Name: "static-bad", Server: "http://plain.example", Token: "dropped"}, // guard -> broken
		},
		KubeconfigPath: kubeconfigPath,
	}
	m, err := NewManager(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	// static-good + the kubeconfig context both load; static-bad is broken.
	if _, ok := m.Get("static-good"); !ok {
		t.Fatalf("static-good not loaded: %#v", m.Clusters())
	}
	if _, ok := m.Get("ctx-a"); !ok {
		t.Fatalf("kubeconfig context not loaded alongside static: %#v", m.Clusters())
	}
	if len(m.Clusters()) != 2 {
		t.Fatalf("expected static+kubeconfig coexistence (2 clusters), got %#v", m.Clusters())
	}
	if broken := m.Broken(); len(broken) != 1 || broken[0].Name != "static-bad" {
		t.Fatalf("expected static-bad broken, got %#v", broken)
	}
}

// TestDuplicateSanitizedCollision pins D8c (loader-half): two distinct configured
// names that sanitize to the same key must not silently collapse -- the second is
// surfaced as a collision error, the first stays loaded.
func TestDuplicateSanitizedCollision(t *testing.T) {
	srv := newTLSFakeAPIServer(t)
	ca := serverCAPEM(t, srv)
	cfg := &appconfig.Config{Clusters: []appconfig.ClusterConnection{
		{Name: "team/prod", Server: srv.URL, CertificateAuthorityData: ca},
		{Name: "team:prod", Server: srv.URL, CertificateAuthorityData: ca},
	}}
	m, err := NewManager(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Get("team:prod"); !ok {
		t.Fatalf("first cluster should win the sanitized key: %#v", m.Clusters())
	}
	if len(m.Clusters()) != 1 {
		t.Fatalf("colliding names must not both load: %#v", m.Clusters())
	}
	broken := m.Broken()
	if len(broken) != 1 {
		t.Fatalf("expected one collision-broken cluster, got %#v", broken)
	}
	if got := broken[0].Err.Error(); !strings.Contains(got, "collides") {
		t.Fatalf("collision error should explain the collision: %v", got)
	}
}

// TestDiscoverKubeconfigLoadsSelectedContext pins kubeconfig discovery + context
// selection through the loader.
func TestDiscoverKubeconfigLoadsSelectedContext(t *testing.T) {
	kubeconfigPath := writeKubeconfig(t, map[string]string{"ctx-a": "https://a", "ctx-b": "https://b"})
	cfg := &appconfig.Config{KubeconfigPath: kubeconfigPath, KubeconfigContexts: []string{"ctx-b"}}
	discovered, err := discoverKubeconfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovered) != 1 || discovered[0].Name != "ctx-b" || discovered[0].Config.Host != "https://b" {
		t.Fatalf("discoverKubeconfig = %#v", discovered)
	}
}

func TestReloadMissingKubeconfigErrors(t *testing.T) {
	cfg := &appconfig.Config{KubeconfigPath: filepath.Join(t.TempDir(), "missing")}
	if _, err := NewManager(context.Background(), cfg); err == nil {
		t.Fatal("missing explicit kubeconfig should be a fatal source error")
	}
}

// writeKubeconfig writes a kubeconfig with one context per name->server entry
// (each with its own cluster+user) and returns the path.
func writeKubeconfig(t *testing.T, contexts map[string]string) string {
	t.Helper()
	raw := clientcmdapi.Config{
		Clusters:  map[string]*clientcmdapi.Cluster{},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{},
		Contexts:  map[string]*clientcmdapi.Context{},
	}
	for name, server := range contexts {
		raw.Clusters[name] = &clientcmdapi.Cluster{Server: server}
		raw.AuthInfos[name] = &clientcmdapi.AuthInfo{Token: "t"}
		raw.Contexts[name] = &clientcmdapi.Context{Cluster: name, AuthInfo: name}
	}
	path := filepath.Join(t.TempDir(), "config")
	if err := clientcmd.WriteToFile(raw, path); err != nil {
		t.Fatal(err)
	}
	return path
}
