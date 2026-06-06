package kube

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
)

type fakeAPIServer struct {
	t          *testing.T
	server     *httptest.Server
	lastAccept string
}

func newFakeAPIServer(t *testing.T) *fakeAPIServer {
	f := &fakeAPIServer{t: t}
	mux := http.NewServeMux()
	mux.HandleFunc("/api", f.fixture("discovery/api.json"))
	mux.HandleFunc("/api/v1", f.fixture("discovery/api__v1.json"))
	mux.HandleFunc("/apis", f.fixture("discovery/apis.json"))
	mux.HandleFunc("/apis/apps/v1", f.fixture("discovery/apis__apps__v1.json"))
	mux.HandleFunc("/apis/cert-manager.io/v1", f.fixture("discovery/apis__cert-manager.io__v1.json"))
	mux.HandleFunc("/apis/gateway.networking.k8s.io/v1", f.fixture("discovery/apis__gateway.networking.k8s.io__v1.json"))
	mux.HandleFunc("/apis/gateway.networking.k8s.io/v1beta1", f.fixture("discovery/apis__gateway.networking.k8s.io__v1beta1.json"))
	mux.HandleFunc("/apis/metrics.k8s.io/v1beta1", f.fixture("discovery/apis__metrics.k8s.io__v1beta1.json"))
	mux.HandleFunc("/apis/storage.k8s.io/v1", f.fixture("discovery/apis__storage.k8s.io__v1.json"))
	mux.HandleFunc("/version", f.fixture("discovery/version.json"))
	mux.HandleFunc("/api/v1/namespaces/default/pods", func(w http.ResponseWriter, r *http.Request) {
		f.lastAccept = r.Header.Get("Accept")
		if strings.Contains(f.lastAccept, "as=Table") {
			f.writeFixture(w, "data/pods_table.json")
			return
		}
		f.writeFixture(w, "data/pods_list.json")
	})
	mux.HandleFunc("/api/v1/namespaces/default/pods/nginx", f.fixture("data/pod_nginx.json"))
	mux.HandleFunc("/api/v1/namespaces/default/pods/nginx/log", f.text("data/pod_log.txt"))
	mux.HandleFunc("/api/v1/nodes", f.fixture("data/nodes_list.json"))
	mux.HandleFunc("/api/v1/nodes/worker-1", f.fixture("data/render_node.json"))
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeAPIServer) client(t *testing.T, includeSecrets bool) *Client {
	t.Helper()
	client, err := NewClient(&rest.Config{Host: f.server.URL}, nil, includeSecrets)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestTableUsesServerSideTableAccept(t *testing.T) {
	f := newFakeAPIServer(t)
	client := f.client(t, false)

	rt, err := client.FindResource(context.Background(), "pods", true, "")
	if err != nil {
		t.Fatal(err)
	}
	table, err := client.Table(context.Background(), &rt, ListOptions{Namespace: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.lastAccept, "as=Table") {
		t.Fatalf("expected server-side Table Accept header, got %q", f.lastAccept)
	}
	if len(table.Columns) == 0 || table.Columns[0].Name != "Name" {
		t.Fatalf("unexpected columns: %#v", table.Columns)
	}
	if len(table.Rows) == 0 {
		t.Fatalf("unexpected rows: %#v", table.Rows)
	}
	if cell := table.Rows[0].Cells[0]; cell != "nginx" {
		t.Fatalf("unexpected first cell: %#v", cell)
	}
}

func TestClientDiscoveryListGetAndBearerHelpers(t *testing.T) {
	f := newFakeAPIServer(t)
	client := f.client(t, false)
	if client.RESTMapper() == nil {
		t.Fatal("RESTMapper returned nil")
	}
	withBearer, err := client.WithBearer("Bearer session-token")
	if err != nil {
		t.Fatal(err)
	}
	if withBearer.config.BearerToken != "session-token" {
		t.Fatalf("WithBearer token = %q", withBearer.config.BearerToken)
	}
	nsTypes, err := client.NamespacedResourceTypes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	clusterTypes, err := client.ClusterResourceTypes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(nsTypes) == 0 || len(clusterTypes) == 0 {
		t.Fatalf("empty discovery: ns=%d cluster=%d", len(nsTypes), len(clusterTypes))
	}
	rt, err := client.FindResourceByKind(context.Background(), "v1", "Pod", true)
	if err != nil {
		t.Fatal(err)
	}
	list, err := client.List(context.Background(), &rt, ListOptions{Namespace: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) == 0 {
		t.Fatalf("empty pod list: %#v", list)
	}
	obj, err := client.Get(context.Background(), &rt, "default", "nginx")
	if err != nil {
		t.Fatal(err)
	}
	if obj.GetName() != "nginx" {
		t.Fatalf("Get name = %q", obj.GetName())
	}
	nodeRT, err := client.FindResource(context.Background(), "nodes", false, "")
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := client.List(context.Background(), &nodeRT, ListOptions{})
	if err != nil || len(nodes.Items) == 0 {
		t.Fatalf("node list = %#v err=%v", nodes, err)
	}
	node, err := client.Get(context.Background(), &nodeRT, "", "worker-1")
	if err != nil || node.GetName() != "worker-1" {
		t.Fatalf("node get = %#v err=%v", node, err)
	}
	if !IsNotFound(ErrResourceTypeNotFound) {
		t.Fatal("IsNotFound should recognize ErrResourceTypeNotFound")
	}
}

// TestWithBearerClearsImpersonation pins the D4 security property at the field
// level: the per-request passthrough clone carries the viewer token, drops the
// rotation file, and clears any static Impersonate so the request evaluates as
// the viewer, not the static impersonation identity.
func TestWithBearerClearsImpersonation(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("base-file-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	base, err := NewClient(&rest.Config{
		Host:            "https://api.example",
		BearerTokenFile: tokenFile,
		Impersonate:     rest.ImpersonationConfig{UserName: "robot", Groups: []string{"viewers"}},
	}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	wb, err := base.WithBearer("viewer-token")
	if err != nil {
		t.Fatal(err)
	}
	if wb.config.BearerToken != "viewer-token" {
		t.Fatalf("BearerToken = %q, want viewer-token", wb.config.BearerToken)
	}
	if wb.config.BearerTokenFile != "" {
		t.Fatalf("BearerTokenFile not cleared on the passthrough clone: %q", wb.config.BearerTokenFile)
	}
	if wb.config.Impersonate.UserName != "" || len(wb.config.Impersonate.Groups) != 0 {
		t.Fatalf("Impersonate not cleared: %#v -- passthrough would get the impersonated identity's RBAC", wb.config.Impersonate)
	}
}

// TestImpersonationClearedOnPassthrough proves the clear end-to-end: a base
// connection with a static Impersonate identity, after WithBearer, reaches the
// apiserver as the viewer's Bearer with NO Impersonate-User (Act-As) header.
func TestImpersonationClearedOnPassthrough(t *testing.T) {
	srv, rec := newAuthCapturingTLSServer(t)
	base, err := NewClient(&rest.Config{
		Host:            srv.URL,
		TLSClientConfig: rest.TLSClientConfig{CAData: serverCAPEM(t, srv)},
		BearerToken:     "base-token",
		Impersonate:     rest.ImpersonationConfig{UserName: "robot", Groups: []string{"admins"}},
	}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	wb, err := base.WithBearer("viewer-token")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := wb.ResourceTypes(context.Background()); err != nil {
		t.Fatalf("discovery: %v", err)
	}
	if rec.Authorization() != "Bearer viewer-token" {
		t.Fatalf("apiserver saw Authorization %q, want Bearer viewer-token", rec.Authorization())
	}
	if rec.ImpersonateUser() != "" {
		t.Fatalf("Impersonate-User leaked to apiserver: %q -- viewer would get the impersonated RBAC", rec.ImpersonateUser())
	}
}

// TestIsAnonymous pins the base-anonymous predicate behind the D8d denial: a
// connection is anonymous iff it carries no authenticating credential.
// Impersonation alone does not count (it needs a base credential to authenticate).
func TestIsAnonymous(t *testing.T) {
	cert, key := genClientCert(t)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		cfg  *rest.Config
		want bool
	}{
		{"bare", &rest.Config{Host: "https://x"}, true},
		{"inline token", &rest.Config{Host: "https://x", BearerToken: "t"}, false},
		{"token file", &rest.Config{Host: "https://x", BearerTokenFile: tokenFile}, false},
		{"client cert", &rest.Config{Host: "https://x", TLSClientConfig: rest.TLSClientConfig{CertData: cert, KeyData: key}}, false},
		{"impersonate only", &rest.Config{Host: "https://x", Impersonate: rest.ImpersonationConfig{UserName: "u"}}, true},
	}
	for _, tc := range cases {
		c, err := NewClient(tc.cfg, nil, false)
		if err != nil {
			t.Fatalf("%s: NewClient: %v", tc.name, err)
		}
		if got := c.IsAnonymous(); got != tc.want {
			t.Fatalf("%s: IsAnonymous = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestDeniedClientIsForbidden pins that a denied client refuses every request
// method with a Forbidden error (and never panics on its shared-but-unused
// internals), so the web layer renders the standard forbidden state.
func TestDeniedClientIsForbidden(t *testing.T) {
	base, err := NewClient(&rest.Config{Host: "https://x"}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	d := base.Denied()
	rt := &ResourceType{Version: "v1", Plural: "pods", Namespaced: true}
	if _, _, err := d.ResourceTypes(context.Background()); !IsForbidden(err) {
		t.Fatalf("ResourceTypes denied err = %v, want forbidden", err)
	}
	if _, err := d.List(context.Background(), rt, ListOptions{Namespace: "ns"}); !IsForbidden(err) {
		t.Fatalf("List denied err = %v, want forbidden", err)
	}
	if _, err := d.Get(context.Background(), rt, "ns", "n"); !IsForbidden(err) {
		t.Fatalf("Get denied err = %v, want forbidden", err)
	}
	if _, err := d.Table(context.Background(), rt, ListOptions{Namespace: "ns"}); !IsForbidden(err) {
		t.Fatalf("Table denied err = %v, want forbidden", err)
	}
	if _, err := d.Logs(context.Background(), LogOptions{Namespace: "ns", Pod: "p"}); !IsForbidden(err) {
		t.Fatalf("Logs denied err = %v, want forbidden", err)
	}
}

func TestDefaultPreferredResourcesKeepCorePodsAheadOfMetrics(t *testing.T) {
	f := newFakeAPIServer(t)
	client := f.client(t, false)
	types := []ResourceType{
		metricsResourceType(true),
		{APIVersion: "v1", Version: "v1", Kind: "Pod", Plural: "pods", Namespaced: true},
	}
	sortResourceTypes(types, client.preferred)
	if got := types[0]; got.Kind != "Pod" || got.APIVersion != "v1" {
		t.Fatalf("first resource = %#v, want core v1 Pod before metrics.k8s.io PodMetrics", got)
	}

	eventTypes := []ResourceType{
		{APIVersion: "events.k8s.io/v1", Group: "events.k8s.io", Version: "v1", Kind: "Event", Plural: "events", Namespaced: true},
		{APIVersion: "v1", Version: "v1", Kind: "Event", Plural: "events", Namespaced: true},
	}
	sortResourceTypes(eventTypes, client.preferred)
	if got := eventTypes[0]; got.APIVersion != "v1" {
		t.Fatalf("first event resource = %#v, want core v1 Event before events.k8s.io", got)
	}
}

func TestSecretTypeDroppedByDefault(t *testing.T) {
	f := newFakeAPIServer(t)
	client := f.client(t, false)

	if _, err := client.FindResource(context.Background(), "secrets", true, ""); err == nil {
		t.Fatal("expected secrets to be absent when includeSecrets=false")
	}

	withSecrets := f.client(t, true)
	rt, err := withSecrets.FindResource(context.Background(), "secrets", true, "")
	if err != nil {
		t.Fatal(err)
	}
	if rt.Kind != "Secret" {
		t.Fatalf("expected Secret resource, got %#v", rt)
	}
}

func TestLogsUsePlainPodLogSubresource(t *testing.T) {
	f := newFakeAPIServer(t)
	client := f.client(t, false)
	logs, err := client.Logs(context.Background(), LogOptions{Namespace: "default", Pod: "nginx", Container: "nginx", Timestamps: true, TailLines: 20})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs, "GET / 200") {
		t.Fatalf("unexpected log payload %q", logs)
	}
}

func TestTableURLPreservesAPIServerBasePath(t *testing.T) {
	client, err := NewClient(&rest.Config{Host: "https://proxy.example/root"}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	u, err := client.tableURL(&ResourceType{Version: "v1", APIVersion: "v1", Plural: "pods", Namespaced: true}, "default")
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Path; got != "/root/api/v1/namespaces/default/pods" {
		t.Fatalf("path = %q", got)
	}
}

func (f *fakeAPIServer) fixture(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		f.writeFixture(w, name)
	}
}

func (f *fakeAPIServer) text(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		data := f.read(name)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(data)
	}
}

func (f *fakeAPIServer) writeFixture(w http.ResponseWriter, name string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(f.read(name))
}

func (f *fakeAPIServer) read(name string) []byte {
	path := filepath.Join("..", "..", "tests", "unit", "fakeapi", "fixtures", name)
	data, err := os.ReadFile(path)
	if err != nil {
		f.t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}
