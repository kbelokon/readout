package web

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kbelokon/readout/internal/config"
	"github.com/kbelokon/readout/internal/kube"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The resource-view + resource-list render contract is pinned by named goquery
// facts:
//   - pod detail Labels/annotation + events table + the Scheduled event ->
//     TestBehaviorDetailLabelChips + TestBehaviorPodDetailFacts.
//   - node System Info / kubeletVersion / v1.29.2 / the field-selected pods
//     subtable -> TestBehaviorNodeDetailFacts.
//   - the metrics join CPU/Memory columns + the 250m / 128 MiB values ->
//     TestBehaviorListQueryMatrix (join=metrics) + TestBehaviorMetricsNumberFormatting.

func TestNodeMetricsAndSecretCustomColumns(t *testing.T) {
	app := newTestServerWithConfig(t, &config.Config{
		Port:           8080,
		Clusters:       []config.ClusterConnection{{Name: "test", Server: newServerFakeAPI(t).URL}},
		DefaultTheme:   "dark",
		IncludeSecrets: true,
	})
	nodes := httptest.NewRecorder()
	app.Handler().ServeHTTP(nodes, httptest.NewRequest(http.MethodGet, "/clusters/test/nodes?join=metrics", nil))
	if nodes.Code != http.StatusOK {
		t.Fatalf("nodes metrics status=%d body=%s", nodes.Code, nodes.Body.String())
	}
	for _, needle := range []string{"CPU Usage", "Memory Usage", "No Node objects"} {
		if !strings.Contains(nodes.Body.String(), needle) {
			t.Fatalf("node metrics missing %q: %s", needle, nodes.Body.String())
		}
	}

	secrets := httptest.NewRecorder()
	app.Handler().ServeHTTP(secrets, httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/secrets?custom-columns=Password=data.password", nil))
	if secrets.Code != http.StatusOK {
		t.Fatalf("secrets custom status=%d body=%s", secrets.Code, secrets.Body.String())
	}
	if !strings.Contains(secrets.Body.String(), kube.SecretContentHidden) {
		t.Fatalf("secret custom column was not masked: %s", secrets.Body.String())
	}
}

func TestAllResourceListSearchAndClusterBranches(t *testing.T) {
	app := newTestServerWithConfig(t, &config.Config{
		Port:             8080,
		Clusters:         []config.ClusterConnection{{Name: "test", Server: newServerFakeAPI(t).URL}},
		DefaultTheme:     "dark",
		ExternalClusters: map[string]string{"external": "https://kwv.example"},
	})
	clusters := httptest.NewRecorder()
	app.Handler().ServeHTTP(clusters, httptest.NewRequest(http.MethodGet, "/clusters?filter=external", nil))
	if clusters.Code != http.StatusOK || !strings.Contains(clusters.Body.String(), "https://kwv.example") {
		t.Fatalf("external clusters response: status=%d body=%s", clusters.Code, clusters.Body.String())
	}

	all := httptest.NewRecorder()
	app.Handler().ServeHTTP(all, httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/all?limit=0", nil))
	if all.Code != http.StatusOK || !strings.Contains(all.Body.String(), "Partial results") {
		t.Fatalf("all resources response: status=%d body=%s", all.Code, all.Body.String())
	}
	resourceTypes := httptest.NewRecorder()
	app.Handler().ServeHTTP(resourceTypes, httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/_all?limit=0", nil))
	if resourceTypes.Code != http.StatusOK || !strings.Contains(resourceTypes.Body.String(), "Found") {
		t.Fatalf("_all resources response: status=%d body=%s", resourceTypes.Code, resourceTypes.Body.String())
	}
	clusterTypes := httptest.NewRecorder()
	app.Handler().ServeHTTP(clusterTypes, httptest.NewRequest(http.MethodGet, "/clusters/test/_resource-types", nil))
	if clusterTypes.Code != http.StatusOK || !strings.Contains(clusterTypes.Body.String(), "ro-bool-no") {
		t.Fatalf("cluster resource types response: status=%d body=%s", clusterTypes.Code, clusterTypes.Body.String())
	}
	blockedNamespace := newTestServerWithConfig(t, &config.Config{
		Port:              8080,
		Clusters:          []config.ClusterConnection{{Name: "test", Server: newServerFakeAPI(t).URL}},
		DefaultTheme:      "dark",
		ExcludeNamespaces: []*regexp.Regexp{regexp.MustCompile(`^secret$`)},
	})
	forbidden := httptest.NewRecorder()
	blockedNamespace.Handler().ServeHTTP(forbidden, httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/secret/_resource-types", nil))
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("forbidden resource types status=%d body=%s", forbidden.Code, forbidden.Body.String())
	}
	search := httptest.NewRecorder()
	app.Handler().ServeHTTP(search, httptest.NewRequest(http.MethodGet, "/search?q=test&cluster=_all&type=pods&selector=app%3Dnginx", nil))
	if search.Code != http.StatusOK || !strings.Contains(search.Body.String(), "Cluster") {
		t.Fatalf("search response: status=%d body=%s", search.Code, search.Body.String())
	}
}

func TestLimitParam(t *testing.T) {
	app := &Server{}
	req := func(query string) *http.Request {
		return httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods?"+query, nil)
	}
	table := func() kube.Table {
		return kube.Table{
			Resource: kube.ResourceType{Plural: "pods", Kind: "Pod", Namespaced: true},
			Columns:  []kube.Column{{Name: "Name"}},
			Rows: []kube.Row{
				{Cells: []any{"one"}},
				{Cells: []any{"two"}},
				{Cells: []any{"three"}},
			},
		}
	}
	cases := []struct {
		query string
		want  int
	}{
		{"limit=abc", 3},
		{"limit=0", 3},
		{"limit=-1", 3},
		{"limit=2", 2},
		{"limit=99", 3},
	}
	for _, tc := range cases {
		tbl := table()
		app.applyTableOptions(req(tc.query), nil, &tbl, "default", false)
		if len(tbl.Rows) != tc.want {
			t.Fatalf("%s left %d rows, want %d", tc.query, len(tbl.Rows), tc.want)
		}
		if tc.query == "limit=2" {
			got := []string{cellDisplayString(tbl.Rows[0].Cells[0]), cellDisplayString(tbl.Rows[1].Cells[0])}
			if strings.Join(got, ",") != "one,two" {
				t.Fatalf("limit=2 kept rows %v, want first two rows one,two", got)
			}
		}
	}
}

func TestBuildCellViewExtraCells(t *testing.T) {
	app := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods", nil)
	table := kube.Table{
		Resource: kube.ResourceType{Plural: "pods", Kind: "Pod", Namespaced: true},
		Columns:  []kube.Column{{Name: "Name"}, {Name: "Status"}},
	}
	row := kube.Row{Cluster: "test", Cells: []any{"nginx", "Running", "extra"}}
	cv := app.buildCellView(req, &table, row, 2, row.Cells[2], "default", "nginx")
	if cv.Kind != cellPlain || cv.Value != "extra" {
		t.Fatalf("extra cell view = %#v, want plain extra", cv)
	}
}

func TestTailLinesClamp(t *testing.T) {
	logQuery := &logQueryRecorder{}
	fake := newRecordingServerFakeAPIWithLogRecorder(t, nil, logQuery)
	app := newTestServerWithConfig(t, &config.Config{
		Port:              8080,
		Clusters:          []config.ClusterConnection{{Name: "test", Server: fake.URL}},
		DefaultTheme:      "dark",
		ShowContainerLogs: true,
	})
	cases := []struct {
		query string
		want  string
	}{
		{"tail_lines=bad", "200"},
		{"tail_lines=-5", "1"},
		{"tail_lines=100001", "100000"},
	}
	for _, tc := range cases {
		before := len(logQuery.values())
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods/nginx/logs?container=nginx&"+tc.query, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", tc.query, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `name="tail_lines" value="`+tc.want+`"`) {
			t.Fatalf("%s rendered wrong tail value, want %s body=%s", tc.query, tc.want, rec.Body.String())
		}
		values := logQuery.values()
		if len(values) != before+1 || values[len(values)-1] != tc.want {
			t.Fatalf("%s log tailLines queries = %v, want last %s", tc.query, values, tc.want)
		}
	}
}

func TestSidebarMetaLinksEscapePathSegments(t *testing.T) {
	app := &Server{}
	sidebar := app.buildSidebarView(httptest.NewRequest(http.MethodGet, "/search", nil), "c/a", "team a", nil)
	want := map[string]string{
		"Resource Types": "/clusters/c%2Fa/namespaces/team%20a/_resource-types",
		"Events":         "/clusters/c%2Fa/namespaces/team%20a/events",
	}
	for _, item := range sidebar.Meta {
		if expected, ok := want[item.Text]; ok {
			if item.Href != expected {
				t.Fatalf("%s href = %q, want %q", item.Text, item.Href, expected)
			}
			delete(want, item.Text)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing sidebar meta links: %v", want)
	}
}

func TestErrorPageNoClusterRefetch(t *testing.T) {
	var namespaceLists atomic.Int64
	fake := newErrorPageCountingFakeAPI(t, &namespaceLists)
	app := newTestServerWithConfig(t, &config.Config{
		Port:         8080,
		Clusters:     []config.ClusterConnection{{Name: "test", Server: fake.URL}},
		DefaultTheme: "dark",
	})

	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods/nginx", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500 body=%s", rec.Code, rec.Body.String())
	}
	if got := namespaceLists.Load(); got != 0 {
		t.Fatalf("error render issued %d namespace LIST calls against the failed cluster, want 0", got)
	}
}

func newErrorPageCountingFakeAPI(t *testing.T, namespaceLists *atomic.Int64) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	fixture := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(readFixture(t, name))
		}
	}
	mux.HandleFunc("/api", fixture("discovery/api.json"))
	mux.HandleFunc("/api/v1", fixture("discovery/api__v1.json"))
	mux.HandleFunc("/apis", fixture("discovery/apis.json"))
	mux.HandleFunc("/apis/apps/v1", fixture("discovery/apis__apps__v1.json"))
	mux.HandleFunc("/apis/cert-manager.io/v1", fixture("discovery/apis__cert-manager.io__v1.json"))
	mux.HandleFunc("/apis/gateway.networking.k8s.io/v1", fixture("discovery/apis__gateway.networking.k8s.io__v1.json"))
	mux.HandleFunc("/apis/gateway.networking.k8s.io/v1beta1", fixture("discovery/apis__gateway.networking.k8s.io__v1beta1.json"))
	mux.HandleFunc("/apis/metrics.k8s.io/v1beta1", fixture("discovery/apis__metrics.k8s.io__v1beta1.json"))
	mux.HandleFunc("/apis/storage.k8s.io/v1", fixture("discovery/apis__storage.k8s.io__v1.json"))
	mux.HandleFunc("/version", fixture("discovery/version.json"))
	mux.HandleFunc("/api/v1/namespaces/default/pods/nginx", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "pod backend unavailable", http.StatusInternalServerError)
	})
	mux.HandleFunc("/api/v1/namespaces", func(w http.ResponseWriter, _ *http.Request) {
		namespaceLists.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(readFixture(t, "data/render_namespaces_list.json"))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func TestLogsDisabledAndFilteredBranches(t *testing.T) {
	disabled := newTestServer(t)
	rec := httptest.NewRecorder()
	disabled.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods/nginx/logs", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Container Logs Disabled") {
		t.Fatalf("disabled logs response: status=%d body=%s", rec.Code, rec.Body.String())
	}

	enabled := newTestServerWithConfig(t, &config.Config{
		Port:              8080,
		Clusters:          []config.ClusterConnection{{Name: "test", Server: newServerFakeAPI(t).URL}},
		DefaultTheme:      "dark",
		ShowContainerLogs: true,
	})
	filtered := httptest.NewRecorder()
	enabled.Handler().ServeHTTP(filtered, httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods/nginx/logs?filter=GET&container=nginx&tail_lines=bad", nil))
	if filtered.Code != http.StatusOK || !strings.Contains(filtered.Body.String(), "GET / 200") || !strings.Contains(filtered.Body.String(), `name="tail_lines" value="200"`) {
		t.Fatalf("filtered logs response: status=%d body=%s", filtered.Code, filtered.Body.String())
	}
	for _, tc := range []struct {
		query string
		want  string
	}{
		{"tail_lines=-5", `name="tail_lines" value="1"`},
		{"tail_lines=100001", `name="tail_lines" value="100000"`},
	} {
		rec := httptest.NewRecorder()
		enabled.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods/nginx/logs?container=nginx&"+tc.query, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), tc.want) {
			t.Fatalf("%s logs response: status=%d want %q body=%s", tc.query, rec.Code, tc.want, rec.Body.String())
		}
	}

	missingContainer := httptest.NewRecorder()
	enabled.Handler().ServeHTTP(missingContainer, httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods/nginx/logs?container=missing", nil))
	if missingContainer.Code != http.StatusOK || strings.Contains(missingContainer.Body.String(), "GET / 200") {
		t.Fatalf("missing container logs response: status=%d body=%s", missingContainer.Code, missingContainer.Body.String())
	}
}

func TestObjectLinkOwnerLinkAndSelectorPodsHelpers(t *testing.T) {
	app := newTestServerWithConfig(t, &config.Config{
		Port:         8080,
		Clusters:     []config.ClusterConnection{{Name: "test", Server: newServerFakeAPI(t).URL}},
		DefaultTheme: "dark",
		ObjectLinks: map[string][]config.Link{
			"pods": {{Href: "https://obj/{cluster}/{namespace}/{name}", Title: "Object {name}", Icon: "external-link"}},
		},
		LabelLinks: map[string][]config.Link{
			"app": {{Href: "https://label/{label}/{label_value}", Title: "Label {labelValue}", Icon: "tag"}},
		},
	})
	rt := kube.ResourceType{Group: "", Version: "v1", APIVersion: "v1", Plural: "pods", Kind: "Pod", Namespaced: true}
	object := kube.NewObject(&rt, &unstructured.Unstructured{Object: map[string]any{
		"kind": "Pod",
		"metadata": map[string]any{
			"name":      "nginx",
			"namespace": "default",
			"labels":    map[string]any{"app": "nginx"},
			"ownerReferences": []any{
				map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "name": "nginx"},
				map[string]any{"apiVersion": "apps/v1", "kind": "", "name": "ignored"},
			},
		},
	}})
	links := app.objectLinks("test", "default", &object)
	if len(links) != 2 || links[0].Href != "https://obj/test/default/nginx" || links[1].Title != "Label nginx" {
		t.Fatalf("object links = %#v", links)
	}
	cluster, ok := app.manager.Get("test")
	if !ok {
		t.Fatal("test cluster missing")
	}
	req := httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods/nginx", nil)
	client := app.kubeClient(req, cluster)
	owners := app.ownerLinks(req, client, cluster, &object)
	if len(owners) != 1 || !strings.Contains(owners[0].Href, "/clusters/test/namespaces/default/deployments/nginx") {
		t.Fatalf("owners = %#v", owners)
	}
	controller := kube.NewObject(&kube.ResourceType{Group: "apps", Version: "v1", APIVersion: "apps/v1", Plural: "deployments", Kind: "Deployment", Namespaced: true}, &unstructured.Unstructured{Object: map[string]any{
		"kind": "Deployment",
		"metadata": map[string]any{
			"name":      "nginx",
			"namespace": "default",
		},
		"spec": map[string]any{"selector": map[string]any{"matchLabels": map[string]any{"app": "nginx"}}},
	}})
	pods := app.podsForSelector(httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/deployments/nginx/logs", nil), client, &controller, "default")
	if len(pods) == 0 || pods[0].Name() != "nginx" {
		t.Fatalf("podsForSelector = %#v", pods)
	}
	expanded := expandLink(config.Link{Href: "/{cluster}/{namespace}/{name}/{label}/{labelValue}", Title: "{label_value}"}, "c", "ns", "n", "app", "api")
	if expanded.Href != "/c/ns/n/app/api" || expanded.Title != "api" {
		t.Fatalf("expandLink = %#v", expanded)
	}
}

func TestJoinCustomColumnsErrorAndEmptyExpressionBranches(t *testing.T) {
	app := newTestServer(t)
	cluster, ok := app.manager.Get("test")
	if !ok {
		t.Fatal("test cluster missing")
	}
	table := kube.Table{
		Resource: kube.ResourceType{Group: "", Version: "v1", APIVersion: "v1", Plural: "pods", Kind: "Pod", Namespaced: true},
		Columns:  []kube.Column{{Name: "Name"}},
		Rows: []kube.Row{{
			Cells:  []any{"nginx"},
			Object: map[string]any{"metadata": map[string]any{"name": "nginx", "namespace": "missing"}},
		}},
	}
	app.joinCustomColumns(httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/missing/pods", nil).Context(), cluster.Client, &table, "missing", false, "Bad=[", nil)
	if len(table.Columns) != 1 || len(table.Rows[0].Cells) != 1 {
		t.Fatalf("invalid expression should leave table untouched: %#v", table)
	}
	app.joinCustomColumns(httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/missing/pods", nil).Context(), cluster.Client, &table, "missing", false, "Image=spec.containers[0].image", nil)
	if len(table.Rows[0].Cells) != 2 || table.Rows[0].Cells[1] != nil {
		t.Fatalf("list error should append nil custom cell: %#v", table.Rows[0].Cells)
	}
}

func TestNamespaceAllowedStatusErrorAndErrorPage(t *testing.T) {
	app := newTestServer(t)
	app.cfg.IncludeNamespaces = []*regexp.Regexp{regexp.MustCompile(`^prod.*$`)}
	app.cfg.ExcludeNamespaces = []*regexp.Regexp{regexp.MustCompile(`^prod-secret$`)}
	if !app.namespaceAllowed("prod-a") || app.namespaceAllowed("default") || app.namespaceAllowed("prod-secret") {
		t.Fatalf("namespaceAllowed include/exclude mismatch")
	}

	err := statusError{status: http.StatusTeapot, message: "short and stout"}
	if err.Error() != "short and stout" || err.StatusCode() != http.StatusTeapot {
		t.Fatalf("statusError accessors mismatch: %#v", err)
	}
	// 4xx canary: a 418 (< 500) still renders its verbatim message. Do not
	// remove — this pins that client-facing 4xx detail is preserved.
	rec := httptest.NewRecorder()
	app.error(rec, httptest.NewRequest(http.MethodGet, "/clusters/test", nil), err)
	if rec.Code != http.StatusTeapot || !strings.Contains(rec.Body.String(), "short and stout") {
		t.Fatalf("error page status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 5xx: the raw error string must NOT reach the client body; a generic body
	// is rendered instead (the detail is logged server-side).
	const secret = "dial tcp 10.0.0.1:6443: connection refused"
	serverErr := statusError{status: http.StatusBadGateway, message: secret}
	rec500 := httptest.NewRecorder()
	app.error(rec500, httptest.NewRequest(http.MethodGet, "/clusters/test", nil), serverErr)
	if rec500.Code != http.StatusBadGateway {
		t.Fatalf("5xx error page status=%d body=%s", rec500.Code, rec500.Body.String())
	}
	if strings.Contains(rec500.Body.String(), secret) {
		t.Fatalf("5xx error page leaked raw error detail: %s", rec500.Body.String())
	}
	if !strings.Contains(rec500.Body.String(), "Internal server error") {
		t.Fatalf("5xx error page did not render generic body: %s", rec500.Body.String())
	}

	// 4xx (404/403): the specific message is preserved.
	notFound := statusError{status: http.StatusNotFound, message: "widget not found"}
	rec404 := httptest.NewRecorder()
	app.error(rec404, httptest.NewRequest(http.MethodGet, "/clusters/test", nil), notFound)
	if rec404.Code != http.StatusNotFound || !strings.Contains(rec404.Body.String(), "widget not found") {
		t.Fatalf("404 error page status=%d body=%s", rec404.Code, rec404.Body.String())
	}
	forbidden := statusError{status: http.StatusForbidden, message: "namespace forbidden"}
	rec403 := httptest.NewRecorder()
	app.error(rec403, httptest.NewRequest(http.MethodGet, "/clusters/test", nil), forbidden)
	if rec403.Code != http.StatusForbidden || !strings.Contains(rec403.Body.String(), "namespace forbidden") {
		t.Fatalf("403 error page status=%d body=%s", rec403.Code, rec403.Body.String())
	}
}

// TestSessionSecretWarning pins the startup warning: when the effective auth
// mode resolves to OIDC and READOUT_SESSION_SECRET (cfg.SessionSecret) is
// empty, warnMissingSessionSecret emits a slog WARN. It covers explicit OIDC,
// implicit OIDC (AuthModeNone + OIDC config resolved via effectiveAuthMode),
// and the no-warn modes (none/headers). Not parallel: it mutates the
// process-global default logger.
func TestSessionSecretWarning(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	warned := func(t *testing.T, cfg config.Config) bool {
		t.Helper()
		var buf bytes.Buffer
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
		s := &Server{cfg: cfg}
		s.warnMissingSessionSecret()
		return strings.Contains(buf.String(), "READOUT_SESSION_SECRET")
	}

	cases := []struct {
		name string
		cfg  config.Config
		want bool
	}{
		{
			name: "explicit OIDC no secret warns",
			cfg:  config.Config{AuthMode: config.AuthModeOIDC, OIDCIssuerURL: "https://issuer.example"},
			want: true,
		},
		{
			name: "implicit OIDC (none + OIDC config) no secret warns",
			cfg:  config.Config{AuthMode: config.AuthModeNone, OIDCIssuerURL: "https://issuer.example"},
			want: true,
		},
		{
			name: "explicit OIDC with secret does not warn",
			cfg:  config.Config{AuthMode: config.AuthModeOIDC, OIDCIssuerURL: "https://issuer.example", SessionSecret: "stable"},
			want: false,
		},
		{
			name: "none without OIDC config does not warn",
			cfg:  config.Config{AuthMode: config.AuthModeNone},
			want: false,
		},
		{
			name: "headers mode does not warn",
			cfg:  config.Config{AuthMode: config.AuthModeHeaders},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := warned(t, tc.cfg); got != tc.want {
				t.Fatalf("warned=%v want=%v for cfg=%#v", got, tc.want, tc.cfg)
			}
		})
	}
}
