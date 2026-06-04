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
	podsHandler := delay(func(w http.ResponseWriter, r *http.Request) {
		if opts.failList {
			http.Error(w, "boom: pods backend unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.Header.Get("Accept"), "as=Table") {
			_, _ = w.Write(readFixture(t, "data/pods_table.json"))
			return
		}
		_, _ = w.Write(readFixture(t, "data/pods_with_node_list.json"))
	})
	mux.HandleFunc("/api/v1/namespaces/default/pods", podsHandler)
	mux.HandleFunc("/api/v1/pods", podsHandler)
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
		Clusters:             clusters,
		DefaultTheme:         "dark",
		SearchMaxConcurrency: 100,
	})
}

// clusterCellOrder returns the cluster names in the order their cells appear in
// the rendered multi-cluster list body. The templ emits one
// `<a href="/clusters/NAME">NAME</a>` per row's Cluster cell, so the sequence of
// hrefs is the merged row order.
func clusterCellOrder(body string) []string {
	const marker = `href="/clusters/`
	var order []string
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
		name := body[start : start+end]
		// The Cluster cells are bare names; skip hrefs that carry a path suffix
		// (e.g. namespaces/ or pod detail links) so only the Cluster column counts.
		if !strings.Contains(name, "/") {
			order = append(order, name)
		}
		i = start + end
	}
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
	banner := doc.Find(".ro-banner.warn")
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
// healthy result-card order is identical across repeats, (b) the per-cluster
// error articles render in fixed cluster-name order (ybad before zbad) even
// though zbad completes first, and (c) the whole request still succeeds -- the
// search analog of partial-failure + fan-in determinism.
func TestMultiClusterSearchFanInIsDeterministicAndPartialSafe(t *testing.T) {
	// aaa/bbb are healthy (aaa slowest). ybad/zbad both fail; zbad is delayed so
	// it COMPLETES before ybad, yet the cluster-order merge must still place the
	// ybad error article first.
	aaa := newClusterFakeAPI(t, clusterFakeOptions{delay: 50 * time.Millisecond})
	bbb := newClusterFakeAPI(t, clusterFakeOptions{delay: 0})
	ybad := newClusterFakeAPI(t, clusterFakeOptions{failList: true, delay: 40 * time.Millisecond})
	zbad := newClusterFakeAPI(t, clusterFakeOptions{failList: true, delay: 0})
	app := newTestServerWithConfig(t, &config.Config{
		Port:                       8080,
		Clusters:                   map[string]string{"aaa": aaa.URL, "bbb": bbb.URL, "ybad": ybad.URL, "zbad": zbad.URL},
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
		// Both failing clusters surface per-cluster error articles, in fixed
		// cluster-name order (ybad before zbad) regardless of completion order.
		yi := strings.Index(body, "Error for cluster ybad")
		zi := strings.Index(body, "Error for cluster zbad")
		if yi < 0 || zi < 0 {
			t.Fatalf("run %d: missing per-cluster error articles (ybad=%d zbad=%d): %s", run, yi, zi, body)
		}
		if yi > zi {
			t.Fatalf("run %d: error articles out of order (ybad at %d, zbad at %d) -- merge is not cluster-name ordered", run, yi, zi)
		}
		// The healthy result-card link order must be stable across repeats.
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
