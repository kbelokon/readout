package kube

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kbelokon/readout/tests/unit/fakeapi"
	"k8s.io/client-go/rest"
)

// fakeAPIServer wraps the shared fakeapi fixture server, capturing the Accept
// header of the most recent collection request through the package's list
// recorder option (synchronized: discovery and list requests can run on
// concurrent client-go goroutines under -race).
type fakeAPIServer struct {
	server *fakeapi.Server

	mu         sync.Mutex
	lastAccept string
}

func TestPassthroughClientCache(t *testing.T) {
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	cache := NewPassthroughClientCache(5*time.Minute, 8)
	cache.now = func() time.Time { return now }

	baseA := &Client{}
	baseB := &Client{}
	builds := 0
	build := func(_ *Client, token string) (*Client, error) {
		builds++
		return &Client{config: &rest.Config{BearerToken: token}}, nil
	}

	first, err := cache.Get(baseA, "viewer-token", build)
	if err != nil {
		t.Fatal(err)
	}
	again, err := cache.Get(baseA, "viewer-token", build)
	if err != nil {
		t.Fatal(err)
	}
	if first != again || builds != 1 {
		t.Fatalf("same base/token should reuse cached client: first=%p again=%p builds=%d", first, again, builds)
	}
	if got := first.config.BearerToken; got != "viewer-token" {
		t.Fatalf("cached client bearer token = %q", got)
	}
	for key := range cache.entries {
		if strings.Contains(fmt.Sprintf("%#v", key), "viewer-token") {
			t.Fatalf("cache key includes raw token: %#v", key)
		}
	}

	otherToken, err := cache.Get(baseA, "other-token", build)
	if err != nil {
		t.Fatal(err)
	}
	otherBase, err := cache.Get(baseB, "viewer-token", build)
	if err != nil {
		t.Fatal(err)
	}
	if otherToken == first {
		t.Fatal("different tokens should not share a passthrough client")
	}
	if otherBase == first {
		t.Fatal("same token on a different base client should not share a passthrough client")
	}

	now = now.Add(6 * time.Minute)
	expired, err := cache.Get(baseA, "viewer-token", build)
	if err != nil {
		t.Fatal(err)
	}
	if expired == first {
		t.Fatal("expired passthrough client should be rebuilt")
	}

	lru := NewPassthroughClientCache(time.Hour, 2)
	lru.now = func() time.Time { return now }
	one, err := lru.Get(baseA, "one", build)
	if err != nil {
		t.Fatal(err)
	}
	two, err := lru.Get(baseA, "two", build)
	if err != nil {
		t.Fatal(err)
	}
	oneAgain, err := lru.Get(baseA, "one", build)
	if err != nil {
		t.Fatal(err)
	}
	if oneAgain != one {
		t.Fatal("cache hit for one should return the original client before eviction")
	}
	if _, err := lru.Get(baseA, "three", build); err != nil {
		t.Fatal(err)
	}
	if len(lru.entries) != 2 {
		t.Fatalf("size-bounded cache entries = %d, want 2", len(lru.entries))
	}
	oneAfterEvict, err := lru.Get(baseA, "one", build)
	if err != nil {
		t.Fatal(err)
	}
	if oneAfterEvict != one {
		t.Fatal("recently used passthrough client should be retained by LRU eviction")
	}
	twoAfterEvict, err := lru.Get(baseA, "two", build)
	if err != nil {
		t.Fatal(err)
	}
	if twoAfterEvict == two {
		t.Fatal("least-recently used passthrough client should be evicted when max size is exceeded")
	}
}

func newFakeAPIServer(t *testing.T) *fakeAPIServer {
	t.Helper()
	f := &fakeAPIServer{}
	server, err := fakeapi.New(fakeapi.WithListRecorder(func(r *http.Request) {
		f.mu.Lock()
		f.lastAccept = r.Header.Get("Accept")
		f.mu.Unlock()
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	f.server = server
	return f
}

// accept returns the Accept header of the most recent collection request.
func (f *fakeAPIServer) accept() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastAccept
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
	if !strings.Contains(f.accept(), "as=Table") {
		t.Fatalf("expected server-side Table Accept header, got %q", f.accept())
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

// TestTableLimitChunkAndRemainingItemCount pins the chunked Table fetch the
// sidebar counts ride on: ListOptions.Limit becomes the `?limit=N` query
// parameter, the response chunk is decoded as-is, and the chunk's
// metadata.remainingItemCount surfaces on Table.RemainingItemCount. An
// unlimited fetch keeps RemainingItemCount nil.
func TestTableLimitChunkAndRemainingItemCount(t *testing.T) {
	f := newFakeAPIServer(t)
	client := f.client(t, false)

	rt, err := client.FindResource(context.Background(), "pods", true, "")
	if err != nil {
		t.Fatal(err)
	}
	table, err := client.Table(context.Background(), &rt, ListOptions{Namespace: "default", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	// The pods fixture has 2 rows: limit=1 must return one row and report the
	// remainder (the fakeapi mirrors the live-probed apiserver shape).
	if len(table.Rows) != 1 {
		t.Fatalf("limited table rows = %d, want 1", len(table.Rows))
	}
	if table.RemainingItemCount == nil || *table.RemainingItemCount != 1 {
		t.Fatalf("limited table RemainingItemCount = %v, want 1", table.RemainingItemCount)
	}

	full, err := client.Table(context.Background(), &rt, ListOptions{Namespace: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Rows) != 2 {
		t.Fatalf("unlimited table rows = %d, want 2", len(full.Rows))
	}
	if full.RemainingItemCount != nil {
		t.Fatalf("unlimited table RemainingItemCount = %v, want nil", full.RemainingItemCount)
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
