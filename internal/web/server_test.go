package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kbelokon/readout/internal/auth"
	"github.com/kbelokon/readout/internal/config"
	fakeapi "github.com/kbelokon/readout/internal/fakekube"
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

func TestPreferencesRejectsExternalNext(t *testing.T) {
	app := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/preferences", strings.NewReader("theme=light&next=//evil.example"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/preferences" {
		t.Fatalf("Location = %q, want /preferences", loc)
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

func TestMetricsSeparatePort(t *testing.T) {
	// metricsPort == 0: /metrics is served on the main mux and must be
	// AUTH-GATED, so IsPublicPath must NOT exempt it from auth.
	mainPort := newTestServer(t)
	if mainPort.auth.IsPublicPath("/metrics") {
		t.Fatal("metrics must be auth-gated on the main mux when metricsPort is unset")
	}

	separate := newTestServerWithConfig(t, &config.Config{
		Port:         8080,
		MetricsPort:  9092,
		Clusters:     []config.ClusterConnection{{Name: "test", Server: newServerFakeAPI(t).URL}},
		DefaultTheme: "dark",
	})
	// metricsPort != 0: /metrics stays public so the main mux can return its
	// disabled-404 (the dedicated listener serves the real metrics).
	if !separate.auth.IsPublicPath("/metrics") {
		t.Fatal("metrics must bypass auth so the main mux can return the disabled 404 when metricsPort is set")
	}
	rec := httptest.NewRecorder()
	separate.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("separate metricsPort main /metrics status = %d, want 404 body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	separate.MetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "# HELP readout_up") {
		t.Fatalf("separate metrics handler status=%d body=%s", rec.Code, rec.Body.String())
	}

	authenticatedMain := newTestServerWithConfig(t, &config.Config{
		Port:               8080,
		MetricsPort:        9092,
		Clusters:           []config.ClusterConnection{{Name: "test", Server: newServerFakeAPI(t).URL}},
		DefaultTheme:       "dark",
		AuthMode:           config.AuthModeOIDC,
		OIDCClientID:       "client-id",
		OAuth2AuthorizeURL: "https://auth.example.test/authorize",
		OAuth2TokenURL:     "https://auth.example.test/token",
		OIDCRedirectURL:    "https://readout.example.test/oauth2/callback",
		SessionSecret:      "test-secret",
	})
	rec = httptest.NewRecorder()
	authenticatedMain.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusNotFound || rec.Header().Get("Location") != "" {
		t.Fatalf("authenticated main /metrics status=%d location=%q body=%s, want 404 without auth redirect", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
}

// TestMetricsMainPortAuthGated proves that when metrics are served on the main
// mux (metricsPort == 0) and auth is on, /metrics does not leak through: headers
// mode returns 401/403 directly, and OIDC mode redirects to the IdP instead of
// serving the metrics payload.
func TestMetricsMainPortAuthGated(t *testing.T) {
	headers := newTestServerWithConfig(t, &config.Config{
		Port:               8080,
		Clusters:           []config.ClusterConnection{{Name: "test", Server: newServerFakeAPI(t).URL}},
		DefaultTheme:       "dark",
		AuthMode:           config.AuthModeHeaders,
		TrustedHeaderUser:  "X-Forwarded-User",
		TrustedHeaderEmail: "X-Forwarded-Email",
	})
	rec := httptest.NewRecorder()
	headers.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("headers-mode main /metrics status=%d, want 401/403 body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "# HELP readout_up") {
		t.Fatalf("headers-mode main /metrics leaked metrics payload: %s", rec.Body.String())
	}

	oidc := newTestServerWithConfig(t, &config.Config{
		Port:               8080,
		Clusters:           []config.ClusterConnection{{Name: "test", Server: newServerFakeAPI(t).URL}},
		DefaultTheme:       "dark",
		AuthMode:           config.AuthModeOIDC,
		OIDCClientID:       "client-id",
		OAuth2AuthorizeURL: "https://auth.example.test/authorize",
		OAuth2TokenURL:     "https://auth.example.test/token",
		OIDCRedirectURL:    "https://readout.example.test/oauth2/callback",
		SessionSecret:      "test-secret",
	})
	rec = httptest.NewRecorder()
	oidc.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("oidc-mode main /metrics status=%d, want 302 redirect body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "# HELP readout_up") {
		t.Fatalf("oidc-mode main /metrics leaked metrics payload: %s", rec.Body.String())
	}
}

func TestPassthroughDiscoveryOncePerCluster(t *testing.T) {
	discovery := &discoveryCounter{}
	fake := newRecordingServerFakeAPIWithDiscoveryCounter(t, discovery)
	app := newTestServerWithConfig(t, &config.Config{
		Port:                       8080,
		Clusters:                   []config.ClusterConnection{{Name: "test", Server: fake.URL}},
		DefaultTheme:               "dark",
		ClusterAuthUseSessionToken: true,
	})
	req := httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods", nil)
	req.Header.Set("Authorization", "Bearer viewer-token")
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("resource list status = %d body=%s", rec.Code, rec.Body.String())
	}
	assertDiscoveryCountsAtMostOnce(t, discovery.snapshot())

	beforeAPI := discovery.count("/api")
	beforeAPIs := discovery.count("/apis")
	req = httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods", nil)
	req.Header.Set("Authorization", "Bearer viewer-token")
	rec = httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("second resource list status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := discovery.count("/api"); got != beforeAPI {
		t.Fatalf("second same-viewer request added /api discovery requests: before=%d after=%d", beforeAPI, got)
	}
	if got := discovery.count("/apis"); got != beforeAPIs {
		t.Fatalf("second same-viewer request added /apis discovery requests: before=%d after=%d", beforeAPIs, got)
	}

	firstDiscovery := &discoveryCounter{}
	secondDiscovery := &discoveryCounter{}
	firstFake := newRecordingServerFakeAPIWithDiscoveryCounter(t, firstDiscovery)
	secondFake := newRecordingServerFakeAPIWithDiscoveryCounter(t, secondDiscovery)
	multi := newTestServerWithConfig(t, &config.Config{
		Port: 8080,
		Clusters: []config.ClusterConnection{
			{Name: "first", Server: firstFake.URL},
			{Name: "second", Server: secondFake.URL},
		},
		DefaultTheme:               "dark",
		ClusterAuthUseSessionToken: true,
	})
	req = httptest.NewRequest(http.MethodGet, "/clusters/_all/namespaces/default/pods", nil)
	req.Header.Set("Authorization", "Bearer viewer-token")
	rec = httptest.NewRecorder()
	multi.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("all-clusters resource list status = %d body=%s", rec.Code, rec.Body.String())
	}
	firstCounts := firstDiscovery.snapshot()
	secondCounts := secondDiscovery.snapshot()
	if firstCounts["/api"] == 0 || secondCounts["/api"] == 0 {
		t.Fatalf("both clusters should be queried independently: first=%v second=%v", firstCounts, secondCounts)
	}
	assertDiscoveryCountsAtMostOnce(t, firstCounts)
	assertDiscoveryCountsAtMostOnce(t, secondCounts)
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
	state := cookieNamed(t, rec.Result().Cookies(), auth.StateCookieName)
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
	session := cookieNamed(t, cbRec.Result().Cookies(), auth.SessionCookieName)
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
	value, err := app.auth.SealSession(&auth.Session{AccessToken: "forwarded-token", Expires: time.Now().Add(time.Hour).Unix()}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: value})
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := lastAuth.value(); got != "Bearer forwarded-token" {
		t.Fatalf("Authorization = %q, want forwarded session token", got)
	}
}

// TestAnonymousBaseDeniedWithoutToken pins the no-bearer denial for an anonymous
// base: with passthrough on and no viewer token, a cluster whose BASE connection
// is itself anonymous is denied (a forbidden client) rather than served as anonymous.
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

// TestNonAnonymousBaseDeniedWithoutToken pins the strict no-bearer denial: with
// passthrough on, a cluster whose base connection carries a real identity (an SA
// token) is ALSO denied when the viewer has no token -- the broad base identity is
// never silently served. A present viewer token yields a per-request passthrough
// clone rather than the base client.
func TestNonAnonymousBaseDeniedWithoutToken(t *testing.T) {
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

	// No viewer token: a non-anonymous base is denied, NOT served under the base SA.
	noToken := httptest.NewRequest(http.MethodGet, "/clusters/sa/namespaces/default/pods", nil)
	got := app.kubeClient(noToken, cluster)
	if got == saBase {
		t.Fatal("non-anonymous base must NOT be served under its own identity when the viewer has no token")
	}
	if _, _, err := got.ResourceTypes(context.Background()); !kube.IsForbidden(err) {
		t.Fatalf("non-anonymous base + no viewer token should be denied (forbidden), got err=%v", err)
	}

	// Viewer token present: a per-request passthrough clone, not the base client.
	withToken := httptest.NewRequest(http.MethodGet, "/clusters/sa/namespaces/default/pods", nil)
	withToken.Header.Set("Authorization", "Bearer viewer-token")
	if got := app.kubeClient(withToken, cluster); got == saBase {
		t.Fatal("a present viewer token should yield a passthrough clone, not the base client")
	}
}

// TestPassthroughOffServesBaseUnchanged pins the passthrough-OFF path: with
// session-token passthrough disabled, kubeClient returns the base client verbatim
// regardless of viewer bearer presence -- the strict no-bearer denial is scoped to
// passthrough ON only.
func TestPassthroughOffServesBaseUnchanged(t *testing.T) {
	app := newTestServerWithConfig(t, &config.Config{
		Port:         8080,
		DefaultTheme: "dark",
		Clusters:     []config.ClusterConnection{{Name: "test", Server: newServerFakeAPI(t).URL}},
	})
	saBase, err := kube.NewClient(&rest.Config{Host: "http://sa.example", BearerToken: "sa-token"}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	cluster := &kube.Cluster{Name: "sa", Client: saBase}

	noToken := httptest.NewRequest(http.MethodGet, "/clusters/sa/namespaces/default/pods", nil)
	if got := app.kubeClient(noToken, cluster); got != saBase {
		t.Fatal("passthrough off: base client must be returned unchanged with no viewer token")
	}
	withToken := httptest.NewRequest(http.MethodGet, "/clusters/sa/namespaces/default/pods", nil)
	withToken.Header.Set("Authorization", "Bearer viewer-token")
	if got := app.kubeClient(withToken, cluster); got != saBase {
		t.Fatal("passthrough off: base client must be returned unchanged even with a viewer token")
	}
}

func TestKubeClientDeniedOnBuildFailure(t *testing.T) {
	app := newTestServerWithConfig(t, &config.Config{
		Port:                       8080,
		DefaultTheme:               "dark",
		ClusterAuthUseSessionToken: true,
		Clusters:                   []config.ClusterConnection{{Name: "test", Server: newServerFakeAPI(t).URL}},
	})
	base, err := kube.NewClient(&rest.Config{Host: "http://sa.example", BearerToken: "sa-token"}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	cluster := &kube.Cluster{Name: "sa", Client: base}

	orig := withBearerClient
	withBearerClient = func(_ *kube.Client, _ string) (*kube.Client, error) {
		return nil, errors.New("forced passthrough build failure")
	}
	t.Cleanup(func() { withBearerClient = orig })

	req := httptest.NewRequest(http.MethodGet, "/clusters/sa/namespaces/default/pods", nil)
	req.Header.Set("Authorization", "Bearer viewer-token")
	got := app.kubeClient(req, cluster)
	if _, _, err := got.ResourceTypes(context.Background()); !kube.IsForbidden(err) {
		t.Fatalf("passthrough build failure should return a denied client, got err=%v", err)
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

func newServerFakeAPI(t *testing.T) *fakeapi.Server {
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

func newRecordingServerFakeAPI(t *testing.T, lastAuth *authRecorder) *fakeapi.Server {
	return newRecordingServerFakeAPIWithLogRecorder(t, lastAuth, nil)
}

type logQueryRecorder struct {
	mu        sync.Mutex
	tailLines []string
}

func (l *logQueryRecorder) record(r *http.Request) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.tailLines = append(l.tailLines, r.URL.Query().Get("tailLines"))
	l.mu.Unlock()
}

func (l *logQueryRecorder) values() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.tailLines...)
}

type discoveryCounter struct {
	mu     sync.Mutex
	counts map[string]int
}

func (d *discoveryCounter) record(r *http.Request) {
	if d == nil {
		return
	}
	d.mu.Lock()
	if d.counts == nil {
		d.counts = map[string]int{}
	}
	d.counts[r.URL.Path]++
	d.mu.Unlock()
}

func (d *discoveryCounter) count(path string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.counts[path]
}

func (d *discoveryCounter) snapshot() map[string]int {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]int, len(d.counts))
	for path, count := range d.counts {
		out[path] = count
	}
	return out
}

func assertDiscoveryCountsAtMostOnce(t *testing.T, counts map[string]int) {
	t.Helper()
	if counts["/api"] == 0 || counts["/apis"] == 0 {
		t.Fatalf("expected root discovery paths to be hit, got %v", counts)
	}
	for path, count := range counts {
		if count > 1 {
			t.Fatalf("%s discovery requests = %d, want at most one per passthrough list request; counts=%v", path, count, counts)
		}
	}
}

func newRecordingServerFakeAPIWithLogRecorder(t *testing.T, lastAuth *authRecorder, logQuery *logQueryRecorder) *fakeapi.Server {
	return newRecordingServerFakeAPIWithRecorders(t, lastAuth, logQuery, nil)
}

func newRecordingServerFakeAPIWithDiscoveryCounter(t *testing.T, discovery *discoveryCounter) *fakeapi.Server {
	return newRecordingServerFakeAPIWithRecorders(t, nil, nil, discovery)
}

// newRecordingServerFakeAPIWithRecorders builds the shared fakeapi fixture
// server, feeding this suite's recorders through the package's functional
// options. The recorder methods are nil-receiver-safe, so absent recorders are
// passed as-is. Route map, discovery patching (events/replicasets), and the
// Table-vs-List Accept negotiation live in the fakeapi package now.
func newRecordingServerFakeAPIWithRecorders(t *testing.T, lastAuth *authRecorder, logQuery *logQueryRecorder, discovery *discoveryCounter) *fakeapi.Server {
	t.Helper()
	server, err := fakeapi.New(
		fakeapi.WithRequestRecorder(lastAuth.record),
		fakeapi.WithLogRecorder(logQuery.record),
		fakeapi.WithDiscoveryRecorder(discovery.record),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	return server
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := fakeapi.Fixture(name)
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
