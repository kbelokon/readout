package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/kbelokon/readout/internal/config"
)

// clusterFakeOptions parametrizes a per-cluster fake API for the fan-out tests.
type clusterFakeOptions struct {
	// delay is slept before every discovery/list/table response, used to force
	// out-of-order completion across clusters (a slow cluster finishes last).
	delay time.Duration
	// failList makes the pods table/list endpoints return 500 so the cluster is
	// a partial failure (its FindResource succeeds, its Table fails) -- the
	// other clusters must still render and the partial notice must appear.
	failList bool
	// searchFixtures swaps the pods table for the search-shaped fixture
	// (api-backend / metrics-api / redis-master names) and adds a deployments
	// route (api-gateway), so the grouped-search tests can assert the search
	// `<mark>` highlight + the multi-kind totals. Off by default: existing tests keep the
	// nginx/my-app rows and no deployments route.
	searchFixtures bool
}

// newClusterFakeAPI builds a minimal fake kube API (discovery + pods table/list)
// for one cluster in the multi-cluster fan-out tests. The pods table carries two
// rows (nginx, my-app) so merged multi-cluster output groups rows per cluster in
// merge order, exposing the cluster ordering.
func newClusterFakeAPI(t *testing.T, opts clusterFakeOptions) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	delay := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if opts.delay > 0 {
				time.Sleep(opts.delay)
			}
			h(w, r)
		}
	}
	fixture := func(name string) http.HandlerFunc {
		return delay(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(readFixture(t, name))
		})
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
	podsFixture := "data/pods_table.json"
	if opts.searchFixtures {
		podsFixture = "data/search_pods_table.json"
	}
	podsHandler := delay(func(w http.ResponseWriter, r *http.Request) {
		if opts.failList {
			http.Error(w, "boom: pods backend unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.Header.Get("Accept"), "as=Table") {
			_, _ = w.Write(readFixture(t, podsFixture))
			return
		}
		_, _ = w.Write(readFixture(t, "data/pods_with_node_list.json"))
	})
	mux.HandleFunc("/api/v1/namespaces/default/pods", podsHandler)
	mux.HandleFunc("/api/v1/pods", podsHandler)
	if opts.searchFixtures {
		deploymentsHandler := delay(func(w http.ResponseWriter, _ *http.Request) {
			if opts.failList {
				http.Error(w, "boom: deployments backend unavailable", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(readFixture(t, "data/search_deployments_table.json"))
		})
		mux.HandleFunc("/apis/apps/v1/namespaces/default/deployments", deploymentsHandler)
		mux.HandleFunc("/apis/apps/v1/deployments", deploymentsHandler)
	}
	// Namespaces list (the navbar / _all-namespaces resolution may touch it).
	mux.HandleFunc("/api/v1/namespaces", fixture("data/render_namespaces_list.json"))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func newMultiClusterServer(t *testing.T, clusters map[string]string) *Server {
	t.Helper()
	return newTestServerWithConfig(t, &config.Config{
		Port:                 8080,
		Clusters:             clusterConnections(clusters),
		DefaultTheme:         "dark",
		SearchMaxConcurrency: 100,
	})
}

// clusterConnections adapts the name->server map the multi-cluster test helpers
// take into the runtime []config.ClusterConnection. Order is irrelevant: the
// manager sorts clusters by name.
func clusterConnections(m map[string]string) []config.ClusterConnection {
	out := make([]config.ClusterConnection, 0, len(m))
	for name, server := range m {
		out = append(out, config.ClusterConnection{Name: name, Server: server})
	}
	return out
}

// clusterCellOrder returns the cluster names in the order their cells appear in
// the rendered multi-cluster list body. The templ emits one
// `<a href="/clusters/NAME">NAME</a>` per row's Cluster cell, so the sequence of
// hrefs is the merged row order.
func clusterCellOrder(body string) []string {
	// Scope to the .ro-table Cluster column (td.cell-clu): the engine now ALSO emits
	// the mobile `.ro-cardlist` projection of the same rows (the mobile cards layer), which carries
	// its own `cluster` meta link, so a raw href scan would double-count. Parsing the
	// table's cluster cells pins the merge order on the table body alone.
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		return nil
	}
	var order []string
	doc.Find("table.ro-table td.cell-clu a").Each(func(_ int, s *goquery.Selection) {
		order = append(order, strings.TrimSpace(s.Text()))
	})
	return order
}

// TestMultiClusterListFanInIsDeterministic forces out-of-order cluster
// completion (the alphabetically-FIRST cluster is the SLOWEST, so it finishes
// last) and asserts the rendered cluster/row order is IDENTICAL across repeated
// requests and equals the fixed cluster-name order -- proving the fan-out merges
// in cluster-name order regardless of completion order.
func TestMultiClusterListFanInIsDeterministic(t *testing.T) {
	// aaa is slowest -> completes LAST but must still merge FIRST.
	aaa := newClusterFakeAPI(t, clusterFakeOptions{delay: 60 * time.Millisecond})
	bbb := newClusterFakeAPI(t, clusterFakeOptions{delay: 30 * time.Millisecond})
	ccc := newClusterFakeAPI(t, clusterFakeOptions{delay: 0})
	app := newMultiClusterServer(t, map[string]string{"aaa": aaa.URL, "bbb": bbb.URL, "ccc": ccc.URL})

	want := []string{"aaa", "aaa", "bbb", "bbb", "ccc", "ccc"} // 2 pod rows per cluster, in name order
	var first []string
	for run := 0; run < 5; run++ {
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/clusters/_all/namespaces/default/pods", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("run %d: status = %d body=%s", run, rec.Code, rec.Body.String())
		}
		got := clusterCellOrder(rec.Body.String())
		if len(got) != len(want) {
			t.Fatalf("run %d: cluster cell order = %v, want %v (len mismatch)", run, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("run %d: cluster cell order = %v, want %v", run, got, want)
			}
		}
		if run == 0 {
			first = got
			continue
		}
		for i := range first {
			if got[i] != first[i] {
				t.Fatalf("run %d: order %v differs from first run %v (non-deterministic fan-in)", run, got, first)
			}
		}
	}
}

// TestMultiClusterListPartialFailureRendersOthers proves the errgroup fan-out
// preserves partial-failure semantics: one cluster's pods backend returns
// 500, but the request still succeeds (200), the healthy clusters' rows render,
// and the partial notice + the failing cluster's error record are present -- the
// failure is a RESULT RECORD, not a whole-request failure.
func TestMultiClusterListPartialFailureRendersOthers(t *testing.T) {
	good := newClusterFakeAPI(t, clusterFakeOptions{})
	bad := newClusterFakeAPI(t, clusterFakeOptions{failList: true})
	app := newMultiClusterServer(t, map[string]string{"good": good.URL, "zbad": bad.URL})

	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/clusters/_all/namespaces/default/pods", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (partial failure must NOT fail the whole request) body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// The healthy cluster still rendered its rows. With only one surviving
	// cluster the table has no Cluster column, so assert via the row Name links
	// (which carry the cluster: /clusters/<cluster>/namespaces/.../pods/<name>).
	if !strings.Contains(body, `href="/clusters/good/namespaces/default/pods/nginx"`) ||
		!strings.Contains(body, `href="/clusters/good/namespaces/default/pods/my-app"`) {
		t.Fatalf("healthy cluster rows did not render on partial failure: %s", body)
	}
	// The failing cluster contributed no rows.
	if strings.Contains(body, "/clusters/zbad/namespaces/default/pods/") {
		t.Fatalf("failing cluster zbad should contribute no rows: %s", body)
	}
	// The all-cluster partial-failure banner (redesign `.ro-banner.warn`) and the
	// failing cluster's per-cluster error line are present. Parse the DOM so this
	// asserts the banner structure, not an incidental substring.
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse partial-failure body: %v", err)
	}
	banner := doc.Find(".ro-banner.warn:not(.ro-stale-banner)")
	if banner.Length() == 0 {
		t.Fatalf("all-cluster partial-failure banner missing: %s", body)
	}
	if got := normSpace(banner.Find(".bn-title").Text()); got != "Partial results: 1 failed" {
		t.Fatalf("partial banner title = %q, want 'Partial results: 1 failed'", got)
	}
	if !strings.Contains(banner.Text(), "zbad/pods") {
		t.Fatalf("failing cluster error line missing (want zbad/pods): %s", banner.Text())
	}
}

// TestMultiClusterSearchFanInIsDeterministicAndPartialSafe forces out-of-order
// search completion across healthy AND failing clusters: it asserts (a) the
// healthy result-row order is identical across repeats, (b) the per-cluster
// `.ro-scope-chip.err` chips render in fixed cluster-name order (ybad before
// zbad) even though zbad completes first, and (c) the whole request still
// succeeds -- the search analog of partial-failure + fan-in determinism.
func TestMultiClusterSearchFanInIsDeterministicAndPartialSafe(t *testing.T) {
	// aaa/bbb are healthy (aaa slowest). ybad/zbad both fail; zbad is delayed so
	// it COMPLETES before ybad, yet the cluster-order merge must still place the
	// ybad `.err` chip first.
	aaa := newClusterFakeAPI(t, clusterFakeOptions{delay: 50 * time.Millisecond})
	bbb := newClusterFakeAPI(t, clusterFakeOptions{delay: 0})
	ybad := newClusterFakeAPI(t, clusterFakeOptions{failList: true, delay: 40 * time.Millisecond})
	zbad := newClusterFakeAPI(t, clusterFakeOptions{failList: true, delay: 0})
	app := newTestServerWithConfig(t, &config.Config{
		Port:                       8080,
		Clusters:                   []config.ClusterConnection{{Name: "aaa", Server: aaa.URL}, {Name: "bbb", Server: bbb.URL}, {Name: "ybad", Server: ybad.URL}, {Name: "zbad", Server: zbad.URL}},
		DefaultTheme:               "dark",
		SearchMaxConcurrency:       100,
		SearchDefaultResourceTypes: []string{"pods"},
	})

	var first string
	for run := 0; run < 5; run++ {
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/search?q=nginx&cluster=_all&namespace=default&type=pods", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("run %d: status = %d (partial failure must NOT fail the whole search) body=%s", run, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		// Both failing clusters surface a `.ro-scope-chip.err` chip, in fixed
		// cluster-name order (ybad before zbad) regardless of completion order.
		doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
		if err != nil {
			t.Fatalf("run %d: parse search body: %v", run, err)
		}
		var errClusters []string
		doc.Find(".ro-scope .ro-scope-chip.err").Each(func(_ int, s *goquery.Selection) {
			// The chip text begins with the cluster name ("<name> — <reason> retry").
			errClusters = append(errClusters, strings.Fields(normSpace(s.Text()))[0])
		})
		if len(errClusters) != 2 || errClusters[0] != "ybad" || errClusters[1] != "zbad" {
			t.Fatalf("run %d: `.err` scope chips = %v, want [ybad zbad] in cluster-name order", run, errClusters)
		}
		// The healthy result-row link order must be stable across repeats.
		links := resultLinkOrder(body)
		if len(links) == 0 {
			t.Fatalf("run %d: no result links rendered: %s", run, body)
		}
		if run == 0 {
			first = strings.Join(links, "|")
			continue
		}
		if got := strings.Join(links, "|"); got != first {
			t.Fatalf("run %d: result link order %q differs from first %q (non-deterministic search fan-in)", run, got, first)
		}
	}
}

// TestSearchGroupedByClusterMarksAndTotals pins the grouped search render
// over the proven fan-out: a two-cluster search (the alphabetically-first
// cluster is the SLOWEST, finishing last) renders one `.search-group` per
// cluster in fixed cluster-name order across repeated runs, each group header
// carrying the ok dot + mono cluster name + count chip; the matched query
// fragment is wrapped in a server-side `<mark>` inside the cell-cookbook
// pn-head/pn-tail name split; and the totals strip counts
// objects/clusters/kinds with the searched-clusters timing meta.
func TestSearchGroupedByClusterMarksAndTotals(t *testing.T) {
	// aaa is slowest -> completes LAST but its group must still render FIRST.
	aaa := newClusterFakeAPI(t, clusterFakeOptions{delay: 40 * time.Millisecond, searchFixtures: true})
	bbb := newClusterFakeAPI(t, clusterFakeOptions{searchFixtures: true})
	app := newMultiClusterServer(t, map[string]string{"aaa": aaa.URL, "bbb": bbb.URL})

	var first string
	for run := 0; run < 4; run++ {
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/search?q=api&cluster=_all&namespace=default&type=pods&type=deployments", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("run %d: status = %d body=%s", run, rec.Code, rec.Body.String())
		}
		doc, err := goquery.NewDocumentFromReader(strings.NewReader(rec.Body.String()))
		if err != nil {
			t.Fatalf("run %d: parse search body: %v", run, err)
		}

		// One group per cluster, in fixed cluster-name order (aaa before bbb)
		// regardless of completion order.
		groups := doc.Find(".search-group")
		if groups.Length() != 2 {
			t.Fatalf("run %d: .search-group count = %d, want 2: %s", run, groups.Length(), rec.Body.String())
		}
		var headers []string
		groups.Find(".search-cluster .mono").Each(func(_ int, s *goquery.Selection) {
			headers = append(headers, strings.TrimSpace(s.Text()))
		})
		if strings.Join(headers, "|") != "aaa|bbb" {
			t.Fatalf("run %d: group header order = %v, want [aaa bbb]", run, headers)
		}
		// Group header anatomy: ok dot + mono name + count chip (2 matching pods
		// + 1 matching deployment per cluster; redis-master-0 filtered out).
		firstGroup := groups.First()
		if firstGroup.Find(".search-cluster .ro-dot.ok").Length() != 1 {
			t.Fatalf("run %d: first group header missing the ok dot", run)
		}
		if got := normSpace(firstGroup.Find(".search-cluster .ro-count").Text()); got != "3" {
			t.Fatalf("run %d: first group count chip = %q, want 3", run, got)
		}
		if firstGroup.Find(`td.cell-name a[href^="/clusters/bbb/"]`).Length() != 0 {
			t.Fatalf("run %d: aaa group leaked bbb rows", run)
		}

		// The matched fragment is <mark>-wrapped inside the pn-head split: the
		// api-backend pod renders pre "" + mark "api" + post "-backend" with the
		// hash tail muted and unmarked.
		nameCell := firstGroup.Find(`td.cell-name a[href="/clusters/aaa/namespaces/default/pods/api-backend-7c9f7cd495-6fff6"]`)
		if nameCell.Length() != 1 {
			t.Fatalf("run %d: api-backend name link missing (links=%v)", run, resultLinkOrder(rec.Body.String()))
		}
		if got := nameCell.Find(".pn-head mark").Text(); got != "api" {
			t.Fatalf("run %d: <mark> fragment = %q, want %q", run, got, "api")
		}
		if got := nameCell.Find(".pn-head").Text(); got != "api-backend" {
			t.Fatalf("run %d: pn-head = %q, want api-backend", run, got)
		}
		if got := nameCell.Find(".pn-tail").Text(); got != "-7c9f7cd495-6fff6" {
			t.Fatalf("run %d: pn-tail = %q, want the unmarked hash tail", run, got)
		}
		if nameCell.Find(".pn-tail mark").Length() != 0 {
			t.Fatalf("run %d: hash tail must never carry a mark", run)
		}

		// Totals strip: 6 objects (3 per cluster) across 2 clusters in 2 kinds
		// (Pod + Deployment), with the right-side searched-clusters timing meta.
		if got := normSpace(doc.Find(".ro-phase-strip .ro-phase-chip").First().Text()); got != "6 objects · 2 clusters · 2 kinds" {
			t.Fatalf("run %d: totals chip = %q, want %q", run, got, "6 objects · 2 clusters · 2 kinds")
		}
		meta := normSpace(doc.Find(".ro-phase-strip .ro-phase-meta").Text())
		if !strings.HasPrefix(meta, "searched 2 clusters in ") || !strings.HasSuffix(meta, "s") {
			t.Fatalf("run %d: totals meta = %q, want 'searched 2 clusters in <T>s'", run, meta)
		}

		// Row order within + across groups is stable across repeats.
		links := strings.Join(resultLinkOrder(rec.Body.String()), "|")
		if run == 0 {
			first = links
			continue
		}
		if links != first {
			t.Fatalf("run %d: result link order %q differs from first %q", run, links, first)
		}
	}
}

// TestSearchGroupedPartialFailureKeepsFailedChip pins the grouped-search partial-failure
// composition: a failed cluster renders its `.ro-scope-chip.err` (+ inline
// read-only retry GET) ALONGSIDE the healthy cluster's `.search-group`, and the
// failed cluster never grows a group.
func TestSearchGroupedPartialFailureKeepsFailedChip(t *testing.T) {
	good := newClusterFakeAPI(t, clusterFakeOptions{searchFixtures: true})
	bad := newClusterFakeAPI(t, clusterFakeOptions{failList: true, searchFixtures: true})
	app := newMultiClusterServer(t, map[string]string{"good": good.URL, "zbad": bad.URL})

	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/search?q=api&cluster=_all&namespace=default&type=pods", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (partial failure must NOT fail the whole search) body=%s", rec.Code, rec.Body.String())
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(rec.Body.String()))
	if err != nil {
		t.Fatalf("parse search body: %v", err)
	}

	// Exactly one group: the healthy cluster's. The failed cluster has no group.
	groups := doc.Find(".search-group")
	if groups.Length() != 1 {
		t.Fatalf(".search-group count = %d, want 1 (only the healthy cluster groups)", groups.Length())
	}
	if got := normSpace(groups.Find(".search-cluster .mono").Text()); got != "good" {
		t.Fatalf("group header = %q, want good", got)
	}

	// The failed cluster keeps the scope-chip + retry treatment alongside the group.
	errChip := doc.Find(".ro-scope .ro-scope-chip.err")
	if errChip.Length() != 1 {
		t.Fatalf("`.err` scope chip count = %d, want 1", errChip.Length())
	}
	if got := normSpace(errChip.Text()); !strings.HasPrefix(got, "zbad") {
		t.Fatalf("`.err` chip = %q, want it to name zbad", got)
	}
	retry, ok := errChip.Find("a.retry").Attr("href")
	if !ok || !strings.HasPrefix(retry, "/search?") || !strings.Contains(retry, "cluster=zbad") {
		t.Fatalf("`.err` chip retry href = %q (ok=%v), want a read-only /search GET scoped to zbad", retry, ok)
	}
	// The partial banner renders too.
	if doc.Find(".ro-banner.warn").Length() == 0 {
		t.Fatalf("partial-failure banner missing")
	}
}

// resultLinkOrder returns the ordered pod-detail result links in the search body
// (the per-result card anchors), used to assert deterministic search ordering.
func resultLinkOrder(body string) []string {
	const marker = `href="/clusters/`
	var links []string
	for i := 0; ; {
		idx := strings.Index(body[i:], marker)
		if idx < 0 {
			break
		}
		start := i + idx + len(marker)
		end := strings.IndexByte(body[start:], '"')
		if end < 0 {
			break
		}
		val := body[start : start+end]
		if strings.Contains(val, "/pods/") {
			links = append(links, val)
		}
		i = start + end
	}
	return links
}
