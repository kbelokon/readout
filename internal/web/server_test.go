package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kbelokon/readout/internal/config"
	"github.com/kbelokon/readout/internal/kube"
	"k8s.io/client-go/rest"
)

func TestReadOnlyMiddlewareRejectsWritesEverywhere(t *testing.T) {
	app := newTestServer(t)
	req := httptest.NewRequest(http.MethodPut, "/not-a-route", nil)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestCSPAndClusterPageRender(t *testing.T) {
	app := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/clusters", nil)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Security-Policy"); !strings.Contains(got, "script-src 'self'") {
		t.Fatalf("missing strict CSP: %q", got)
	}
	// script-src stays strict (no inline/eval); style-src allows 'unsafe-inline'
	// because the design pins per-row values as inline style attributes
	// (capacity-bar width, kind-tile --kh) the cascade cannot express as classes.
	if got := rec.Header().Get("Content-Security-Policy"); strings.Contains(got, "script-src 'self' 'unsafe-inline'") {
		t.Fatalf("script-src must NOT allow unsafe-inline (code-exec protection): %q", got)
	}
	if got := rec.Header().Get("Content-Security-Policy"); !strings.Contains(got, "style-src 'self' 'unsafe-inline'") {
		t.Fatalf("style-src must allow 'unsafe-inline' for inline width/hue styles: %q", got)
	}
	if !strings.Contains(rec.Body.String(), `<span class="brand-name">readout</span>`) || !strings.Contains(rec.Body.String(), "readout.css") {
		t.Fatalf("page did not render expected app chrome: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "- readout</title>") || strings.Contains(rec.Body.String(), "Cluster Resources") {
		t.Fatalf("clusters page did not match chrome/sidebar contract: %s", rec.Body.String())
	}
}

func TestPreferencesPostIsOnlyWriteAllowlist(t *testing.T) {
	app := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/preferences", strings.NewReader("theme=light"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
}

// The resource-list render contract is pinned by named goquery facts: the
// #resource-list-content hx-get (exact attribute value), the ro-list-table
// headers + the nginx row cells in TestBehaviorPodListFacts, and the htmx script
// wiring in TestBehaviorAppChromeAndJSContract. The _table partial fragment is
// pinned by TestBehaviorResourceListPartial.

func TestDownloadsTSVAndYAML(t *testing.T) {
	app := newTestServer(t)
	tsv := httptest.NewRecorder()
	app.Handler().ServeHTTP(tsv, httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods?download=tsv", nil))
	if tsv.Code != http.StatusOK || !strings.Contains(tsv.Header().Get("Content-Type"), "text/tab-separated-values") || !strings.Contains(tsv.Body.String(), "nginx") {
		t.Fatalf("bad TSV response: status=%d headers=%v body=%s", tsv.Code, tsv.Header(), tsv.Body.String())
	}
	yamlRec := httptest.NewRecorder()
	app.Handler().ServeHTTP(yamlRec, httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods/nginx?download=yaml", nil))
	if yamlRec.Code != http.StatusOK || !strings.Contains(yamlRec.Header().Get("Content-Type"), "text/vnd.yaml") || !strings.Contains(yamlRec.Body.String(), "kind: Pod") {
		t.Fatalf("bad YAML response: status=%d headers=%v body=%s", yamlRec.Code, yamlRec.Header(), yamlRec.Body.String())
	}
}

func TestMetricsEndpointCountsRoutedRequests(t *testing.T) {
	app := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/clusters", nil)
	app.Handler().ServeHTTP(httptest.NewRecorder(), req)

	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, needle := range []string{"# HELP readout_http_requests_total", `path="/clusters"`, `status="200"`} {
		if !strings.Contains(body, needle) {
			t.Fatalf("metrics missing %q: %s", needle, body)
		}
	}
}

func TestGenericOAuth2FlowCreatesEncryptedSession(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			t.Fatalf("unexpected token path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"session-token","token_type":"Bearer","expires_in":3600}`))
	}))
	defer tokenServer.Close()

	app := newTestServerWithConfig(t, &config.Config{
		Port:               8080,
		Clusters:           []config.ClusterConnection{{Name: "test", Server: newServerFakeAPI(t).URL}},
		DefaultTheme:       "dark",
		AuthMode:           config.AuthModeOIDC,
		OIDCClientID:       "client-id",
		OIDCClientSecret:   "client-secret",
		OAuth2AuthorizeURL: "https://auth.example/authorize",
		OAuth2TokenURL:     tokenServer.URL + "/token",
		OIDCRedirectURL:    "http://example.test/oauth2/callback",
		SessionSecret:      "test-secret",
	})
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/clusters", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	state := cookieNamed(t, rec.Result().Cookies(), stateCookieName)
	location := rec.Header().Get("Location")
	if !strings.Contains(location, "https://auth.example/authorize") || !strings.Contains(location, "client_id=client-id") {
		t.Fatalf("unexpected authorize redirect: %s", location)
	}
	stateValue := queryValue(location, "state")
	cb := httptest.NewRequest(http.MethodGet, "/oauth2/callback?code=ok&state="+stateValue, nil)
	cb.AddCookie(state)
	cbRec := httptest.NewRecorder()
	app.Handler().ServeHTTP(cbRec, cb)
	if cbRec.Code != http.StatusFound {
		t.Fatalf("callback status = %d body=%s", cbRec.Code, cbRec.Body.String())
	}
	session := cookieNamed(t, cbRec.Result().Cookies(), sessionCookieName)
	if session.Value == "" || strings.Contains(session.Value, "session-token") {
		t.Fatalf("session cookie is empty or not encrypted: %q", session.Value)
	}
	next := httptest.NewRequest(http.MethodGet, "/clusters", nil)
	next.AddCookie(session)
	nextRec := httptest.NewRecorder()
	app.Handler().ServeHTTP(nextRec, next)
	if nextRec.Code != http.StatusOK {
		t.Fatalf("authorized status = %d body=%s", nextRec.Code, nextRec.Body.String())
	}
}

func TestPrerenderHookInjectsDetailLink(t *testing.T) {
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Cluster  string         `json:"cluster"`
			Plural   string         `json:"plural"`
			Resource map[string]any `json:"resource"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Cluster != "test" || payload.Plural != "pods" || payload.Resource["kind"] != "Pod" {
			t.Fatalf("unexpected hook payload: %#v", payload)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"links":[{"href":"https://ops.example/pod/nginx","title":"Ops"}]}`))
	}))
	defer hook.Close()
	app := newTestServerWithConfig(t, &config.Config{Port: 8080, Clusters: []config.ClusterConnection{{Name: "test", Server: newServerFakeAPI(t).URL}}, DefaultTheme: "dark", ResourcePrerenderHookURL: hook.URL})
	req := httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods/nginx?view=yaml", nil)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "https://ops.example/pod/nginx") {
		t.Fatalf("hook link was not rendered: %s", rec.Body.String())
	}
}

func TestTimestampLinksDecorateYAML(t *testing.T) {
	app := newTestServerWithConfig(t, &config.Config{
		Port:         8080,
		Clusters:     []config.ClusterConnection{{Name: "test", Server: newServerFakeAPI(t).URL}},
		DefaultTheme: "dark",
		TimestampLinks: map[string][]config.Link{
			"pods": {{Href: "https://logs.example/{cluster}/{namespace}/{name}/{timestamp}", Title: "Logs {timestamp}"}},
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods/nginx?view=yaml", nil)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "https://logs.example/test/default/nginx/") {
		t.Fatalf("timestamp link was not rendered: %s", body)
	}
}

func TestCustomColumnsCanJoinNodes(t *testing.T) {
	app := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods?join=nodes&custom-columns=NodeName=node.metadata.name", nil)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "NodeName") || !strings.Contains(body, "127.0.0.1") {
		t.Fatalf("node custom column did not render: %s", body)
	}
}

func TestSecretDetailMasksDataWhenSecretsAreIncluded(t *testing.T) {
	app := newTestServerWithConfig(t, &config.Config{Port: 8080, Clusters: []config.ClusterConnection{{Name: "test", Server: newServerFakeAPI(t).URL}}, DefaultTheme: "dark", IncludeSecrets: true})
	req := httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/secrets/my-secret", nil)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Values masked") || !strings.Contains(body, "value masked") || strings.Contains(body, "c3VwZXItc2VjcmV0LXZhbHVl") {
		t.Fatalf("secret was not masked: %s", body)
	}
}

func TestClusterAuthUsesEncryptedSessionToken(t *testing.T) {
	var lastAuth authRecorder
	fake := newRecordingServerFakeAPI(t, &lastAuth)
	app := newTestServerWithConfig(t, &config.Config{Port: 8080, Clusters: []config.ClusterConnection{{Name: "test", Server: fake.URL}}, DefaultTheme: "dark", ClusterAuthUseSessionToken: true, SessionSecret: "test-secret"})
	value, err := app.sessions.Seal(sessionCookieName, authSession{AccessToken: "forwarded-token", Expires: time.Now().Add(time.Hour).Unix()}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: value})
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := lastAuth.value(); got != "Bearer forwarded-token" {
		t.Fatalf("Authorization = %q, want forwarded session token", got)
	}
}

// TestAnonymousBaseDeniedWithoutToken pins the D8d denial: with passthrough on
// and no viewer token, a cluster whose BASE connection is itself anonymous is
// denied (a forbidden client) rather than silently served as anonymous.
func TestAnonymousBaseDeniedWithoutToken(t *testing.T) {
	app := newTestServerWithConfig(t, &config.Config{
		Port:                       8080,
		DefaultTheme:               "dark",
		ClusterAuthUseSessionToken: true,
		Clusters:                   []config.ClusterConnection{{Name: "test", Server: newServerFakeAPI(t).URL}},
	})
	anonBase, err := kube.NewClient(&rest.Config{Host: "http://anon.example"}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	cluster := &kube.Cluster{Name: "anon", Client: anonBase}
	req := httptest.NewRequest(http.MethodGet, "/clusters/anon/namespaces/default/pods", nil) // no auth
	got := app.kubeClient(req, cluster)
	if _, _, err := got.ResourceTypes(context.Background()); !kube.IsForbidden(err) {
		t.Fatalf("anonymous base + no viewer token should be denied (forbidden), got err=%v", err)
	}
}

// TestPassthroughServesWithViewerToken pins the non-deny side of D8d: a cluster
// whose base connection carries a real identity (an SA token) is served with that
// identity when the viewer has no token (NOT denied), and a present viewer token
// yields a per-request passthrough clone rather than the base client.
func TestPassthroughServesWithViewerToken(t *testing.T) {
	app := newTestServerWithConfig(t, &config.Config{
		Port:                       8080,
		DefaultTheme:               "dark",
		ClusterAuthUseSessionToken: true,
		Clusters:                   []config.ClusterConnection{{Name: "test", Server: newServerFakeAPI(t).URL}},
	})
	saBase, err := kube.NewClient(&rest.Config{Host: "http://sa.example", BearerToken: "sa-token"}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	cluster := &kube.Cluster{Name: "sa", Client: saBase}

	// No viewer token: a non-anonymous base falls through to its own identity.
	noToken := httptest.NewRequest(http.MethodGet, "/clusters/sa/namespaces/default/pods", nil)
	if got := app.kubeClient(noToken, cluster); got != saBase {
		t.Fatal("non-anonymous base must serve with its own identity when the viewer has no token (must not be denied)")
	}

	// Viewer token present: a per-request passthrough clone, not the base client.
	withToken := httptest.NewRequest(http.MethodGet, "/clusters/sa/namespaces/default/pods", nil)
	withToken.Header.Set("Authorization", "Bearer viewer-token")
	if got := app.kubeClient(withToken, cluster); got == saBase {
		t.Fatal("a present viewer token should yield a passthrough clone, not the base client")
	}
}

func TestRoutesAndPartials(t *testing.T) {
	app := newTestServerWithConfig(t, &config.Config{
		Port:                       8080,
		Clusters:                   []config.ClusterConnection{{Name: "test", Server: newServerFakeAPI(t).URL}},
		DefaultTheme:               "dark",
		ShowContainerLogs:          true,
		SearchDefaultResourceTypes: []string{"pods"},
	})
	cases := []struct {
		path   string
		status int
		body   string
	}{
		{"/", http.StatusFound, ""},
		{"/preferences", http.StatusOK, "Preferences"},
		{"/clusters/test", http.StatusOK, "Namespaces"},
		{"/clusters/test/_resource-types", http.StatusOK, "Resource Types"},
		{"/clusters/test/namespaces/default", http.StatusOK, "/clusters/test/namespaces/default/namespaces/default?download=yaml"},
		{"/clusters/test/namespaces/default/namespaces/default?download=yaml", http.StatusOK, "kind: Namespace"},
		{"/clusters/test/namespaces/default/_resource-types", http.StatusOK, "Resource Types"},
		{"/clusters/test/namespaces/default/pods/_table", http.StatusOK, "nginx"},
		{"/clusters/test/namespaces/default/pods/nginx/logs", http.StatusOK, "GET / 200"},
		{"/clusters/test/nodes/worker-1?view=yaml", http.StatusOK, "/clusters/test/nodes/worker-1?download=yaml"},
		{"/search?q=nginx&cluster=test&namespace=default&type=pods", http.StatusOK, "nginx"},
		{"/clusters/missing", http.StatusInternalServerError, "cluster"},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != tc.status {
			t.Fatalf("%s status = %d, want %d body=%s", tc.path, rec.Code, tc.status, rec.Body.String())
		}
		if tc.body != "" && !strings.Contains(rec.Body.String(), tc.body) {
			t.Fatalf("%s missing %q in body=%s", tc.path, tc.body, rec.Body.String())
		}
	}
	asset := httptest.NewRecorder()
	app.Handler().ServeHTTP(asset, httptest.NewRequest(http.MethodGet, "/assets/readout.css", nil))
	if asset.Code != http.StatusOK || !strings.Contains(asset.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("bad asset response: status=%d cache=%q", asset.Code, asset.Header().Get("Cache-Control"))
	}
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	fake := newServerFakeAPI(t)
	return newTestServerWithConfig(t, &config.Config{Port: 8080, Clusters: []config.ClusterConnection{{Name: "test", Server: fake.URL}}, DefaultTheme: "dark"})
}

func newTestServerWithConfig(t *testing.T, cfg *config.Config) *Server {
	t.Helper()
	cfg.NoAccessLogs = true
	app, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func newServerFakeAPI(t *testing.T) *httptest.Server {
	return newRecordingServerFakeAPI(t, nil)
}

// authRecorder captures the most recent Authorization header seen by the fake
// API. The fake server's discovery endpoints are hit by concurrent client-go
// goroutines, so the capture must be synchronized to stay race-free under
// `go test -race`; record is nil-safe so the non-recording fixture can pass nil.
type authRecorder struct {
	mu   sync.Mutex
	last string
}

func (a *authRecorder) record(r *http.Request) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.last = r.Header.Get("Authorization")
	a.mu.Unlock()
}

func (a *authRecorder) value() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.last
}

func newRecordingServerFakeAPI(t *testing.T, lastAuth *authRecorder) *httptest.Server {
	mux := http.NewServeMux()
	fixture := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			lastAuth.record(r)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(readFixture(t, name))
		}
	}
	tableOrList := func(tableFixture, listFixture string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			lastAuth.record(r)
			w.Header().Set("Content-Type", "application/json")
			if strings.Contains(r.Header.Get("Accept"), "as=Table") {
				_, _ = w.Write(readFixture(t, tableFixture))
				return
			}
			_, _ = w.Write(readFixture(t, listFixture))
		}
	}
	mux.HandleFunc("/api", fixture("discovery/api.json"))
	mux.HandleFunc("/api/v1", func(w http.ResponseWriter, r *http.Request) {
		lastAuth.record(r)
		var body map[string]any
		if err := json.Unmarshal(readFixture(t, "discovery/api__v1.json"), &body); err != nil {
			t.Fatal(err)
		}
		body["resources"] = append(body["resources"].([]any), map[string]any{
			"name":         "events",
			"singularName": "event",
			"namespaced":   true,
			"kind":         "Event",
			"verbs":        []string{"get", "list", "watch"},
			"shortNames":   []string{"ev"},
		})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("/apis", fixture("discovery/apis.json"))
	mux.HandleFunc("/apis/apps/v1", func(w http.ResponseWriter, r *http.Request) {
		lastAuth.record(r)
		var body map[string]any
		if err := json.Unmarshal(readFixture(t, "discovery/apis__apps__v1.json"), &body); err != nil {
			t.Fatal(err)
		}
		body["resources"] = append(body["resources"].([]any), map[string]any{
			"name":         "replicasets",
			"singularName": "replicaset",
			"namespaced":   true,
			"kind":         "ReplicaSet",
			"verbs":        []string{"get", "list", "watch"},
			"shortNames":   []string{"rs"},
		})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("/apis/cert-manager.io/v1", fixture("discovery/apis__cert-manager.io__v1.json"))
	mux.HandleFunc("/apis/gateway.networking.k8s.io/v1", fixture("discovery/apis__gateway.networking.k8s.io__v1.json"))
	mux.HandleFunc("/apis/gateway.networking.k8s.io/v1beta1", fixture("discovery/apis__gateway.networking.k8s.io__v1beta1.json"))
	mux.HandleFunc("/apis/metrics.k8s.io/v1beta1", fixture("discovery/apis__metrics.k8s.io__v1beta1.json"))
	mux.HandleFunc("/apis/storage.k8s.io/v1", fixture("discovery/apis__storage.k8s.io__v1.json"))
	mux.HandleFunc("/version", fixture("discovery/version.json"))
	mux.HandleFunc("/api/v1/namespaces/default/pods", tableOrList("data/pods_table.json", "data/pods_with_node_list.json"))
	mux.HandleFunc("/api/v1/namespaces/default/pods/nginx", fixture("data/render_pod_nginx.json"))
	mux.HandleFunc("/api/v1/namespaces/default/pods/nginx/log", func(w http.ResponseWriter, r *http.Request) {
		lastAuth.record(r)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(readFixture(t, "data/pod_log.txt"))
	})
	mux.HandleFunc("/api/v1/pods", tableOrList("data/pods_table.json", "data/pods_with_node_list.json"))
	// Pods in the "states" namespace exercise the redesign status/ready/restart
	// tones END TO END: a transient pod (ContainerCreating/Terminating) pulses, a
	// degraded pod is 2/3 partial with restarts, a steady pod does not pulse. The
	// resource-list path always requests the server-side Table, so the Table
	// fixture is served directly.
	mux.HandleFunc("/api/v1/namespaces/states/pods", fixture("data/pods_states_table.json"))
	// Pods in the "empty" namespace return a zero-row Table, exercising the
	// genuinely-EMPTY list state (the plain "No Pod objects ... found." sentence +
	// the broad next action) through the real assembly with NO filter active.
	mux.HandleFunc("/api/v1/namespaces/empty/pods", fixture("data/table_empty_rows.json"))
	// Services in "default" exercise the GENERIC fallback through the real
	// assembly: a kind with NO Status column and no per-kind rich cells renders
	// its rows from the Table API with no status dot.
	mux.HandleFunc("/api/v1/namespaces/default/services", fixture("data/services_table.json"))
	mux.HandleFunc("/api/v1/namespaces/default/events", fixture("data/render_events_nginx.json"))
	mux.HandleFunc("/api/v1/namespaces/default/secrets", tableOrList("data/render_secrets_table.json", "data/secrets_list.json"))
	mux.HandleFunc("/api/v1/namespaces/default/secrets/my-secret", fixture("data/render_secret.json"))
	mux.HandleFunc("/api/v1/namespaces/default", func(w http.ResponseWriter, r *http.Request) {
		lastAuth.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"default","creationTimestamp":"2024-01-01T00:00:00Z","resourceVersion":"1"},"status":{"phase":"Active"}}`))
	})
	mux.HandleFunc("/api/v1/namespaces", fixture("data/render_namespaces_list.json"))
	mux.HandleFunc("/api/v1/nodes", tableOrList("data/render_namespaces_list.json", "data/nodes_list.json"))
	mux.HandleFunc("/api/v1/nodes/worker-1", fixture("data/render_node.json"))
	mux.HandleFunc("/apis/metrics.k8s.io/v1beta1/namespaces/default/pods", fixture("data/metrics_pods_list.json"))
	mux.HandleFunc("/apis/metrics.k8s.io/v1beta1/pods", fixture("data/metrics_pods_list.json"))
	mux.HandleFunc("/apis/metrics.k8s.io/v1beta1/nodes", func(w http.ResponseWriter, r *http.Request) {
		lastAuth.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apiVersion":"metrics.k8s.io/v1beta1","kind":"NodeMetricsList","items":[{"apiVersion":"metrics.k8s.io/v1beta1","kind":"NodeMetrics","metadata":{"name":"worker-1"},"usage":{"cpu":"1","memory":"256Mi"}}]}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("..", "..", "tests", "unit", "fakeapi", "fixtures", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

func queryValue(rawURL, key string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return parsed.Query().Get(key)
}

func cookieNamed(t *testing.T, cookies []*http.Cookie, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie
		}
	}
	t.Fatalf("cookie %s not found in %#v", name, cookies)
	return nil
}
