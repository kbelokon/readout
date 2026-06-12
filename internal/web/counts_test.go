package web

// counts_test.go pins the sidebar per-kind counts and the
// sidebar composition delta:
//
//   - the count formula `len(rows) + metadata.remainingItemCount` across all
//     three server shapes -- a paginating apiserver (1 row + remaining 106 ->
//     107, the live-probed do-nyc3 shape), a limit-IGNORING server (40 rows,
//     no remainder -> 40), and a zero-item list (renders the literal "0");
//   - an erroring kind renders NO count while its siblings stay counted;
//   - the (cluster, type, namespace) TTL cache: a second render inside the
//     TTL issues no new limit=1 fetches, a render after the TTL re-fetches;
//   - counts attach to group entries and the Events meta entry, never to
//     Resource Types, and only in single-cluster scope (the `_all` sidebar
//     shows none);
//   - a slow counts backend cannot stall page render beyond the shared fetch
//     deadline;
//   - Secrets joins Pod Management (only with IncludeSecrets) and the existing
//     Meta section stays single and intact; config-driven sidebar overrides
//     keep working and gain counts.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/kbelokon/readout/internal/config"
	"github.com/kbelokon/readout/internal/kube"
	"github.com/kbelokon/readout/tests/unit/fakeapi"
)

// TestCountsTableFormula pins the sidebar-count formula on the decoded Table chunk:
// count = len(rows) + metadata.remainingItemCount, which folds the three
// server behaviors into one expression.
func TestCountsTableFormula(t *testing.T) {
	for _, tc := range []struct {
		name      string
		rows      int
		remaining *int64
		want      int64
	}{
		{"zero items", 0, nil, 0},
		{"exactly one item, no remainder", 1, nil, 1},
		{"chunked apiserver: 1 row + remaining 106", 1, i64(106), 107},
		{"limit-ignoring server: all 40 rows, no remainder", 40, nil, 40},
	} {
		t.Run(tc.name, func(t *testing.T) {
			table := kube.Table{Rows: make([]kube.Row, tc.rows), RemainingItemCount: tc.remaining}
			if got := tableCount(&table); got != tc.want {
				t.Fatalf("tableCount(rows=%d, remaining=%v) = %d, want %d", tc.rows, tc.remaining, got, tc.want)
			}
		})
	}
}

// TestSidebarCountsRemainingItemCountFromWire pins the wire path of the
// paginating-apiserver branch: the limit=1 chunk for pods answers with ONE row
// and metadata.remainingItemCount 106 (the shape probed live: limit=1 over 107
// objects), and the sidebar pods entry renders the mono count "107".
func TestSidebarCountsRemainingItemCountFromWire(t *testing.T) {
	fake := newServerFakeAPI(t)
	proxy := newCountsProxy(t, fake.URL, func(r *http.Request) ([]byte, bool) {
		if r.URL.Path == "/api/v1/namespaces/default/pods" && r.URL.Query().Get("limit") == "1" {
			return countsTableJSON(t, 1, i64(106)), true
		}
		return nil, false
	})
	app := newServer(t, configForFixture(proxy.URL), time.Now())

	p := get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)
	p.wantText(`.ro-sidebar a.menu-item[href="/clusters/test/namespaces/default/pods"] .menu-count`, "107")
}

// TestSidebarCountsLimitIgnoringServer pins the limit-IGNORING branch: the
// server answers the limit=1 request with all 40 rows and NO
// remainingItemCount, so the row length alone is the count.
func TestSidebarCountsLimitIgnoringServer(t *testing.T) {
	fake := newServerFakeAPI(t)
	proxy := newCountsProxy(t, fake.URL, func(r *http.Request) ([]byte, bool) {
		if r.URL.Path == "/api/v1/namespaces/default/pods" && r.URL.Query().Get("limit") == "1" {
			return countsTableJSON(t, 40, nil), true
		}
		return nil, false
	})
	app := newServer(t, configForFixture(proxy.URL), time.Now())

	p := get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)
	p.wantText(`.ro-sidebar a.menu-item[href="/clusters/test/namespaces/default/pods"] .menu-count`, "40")
}

// TestSidebarCountsZeroRendersAndErrorAbsent pins the remaining branches on
// the plain fixture server: the "empty" namespace pods list has zero rows ->
// the literal "0" renders; services has NO route in that namespace (the fetch
// 404s) -> the entry renders WITHOUT a count.
func TestSidebarCountsZeroRendersAndErrorAbsent(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/namespaces/empty/pods", http.StatusOK)

	p.wantText(`.ro-sidebar a.menu-item[href="/clusters/test/namespaces/empty/pods"] .menu-count`, "0")

	p.wantHas(`.ro-sidebar a.menu-item[href="/clusters/test/namespaces/empty/services"]`)
	p.wantAbsent(`.ro-sidebar a.menu-item[href="/clusters/test/namespaces/empty/services"] .menu-count`)
}

// TestSidebarCountsGroupEntriesAndEventsMeta pins where counts attach: the
// group entries carry their fixture totals, the Events META entry carries the
// events count, Resource Types (not a kind) carries none, and an erroring kind
// (deployments has no fixture route -> 404) stays uncounted WITHOUT poisoning
// its counted siblings.
func TestSidebarCountsGroupEntriesAndEventsMeta(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)

	for href, want := range map[string]string{
		"/clusters/test/namespaces/default/pods":       "2", // pods_table.json rows
		"/clusters/test/namespaces/default/configmaps": "3",
		"/clusters/test/nodes":                         "1", // cluster-scoped count
		"/clusters/test/namespaces/default/events":     "4", // events meta entry
	} {
		p.wantText(fmt.Sprintf(`.ro-sidebar a.menu-item[href=%q] .menu-count`, href), want)
	}

	// The erroring kind renders, uncounted.
	p.wantHas(`.ro-sidebar a.menu-item[href="/clusters/test/namespaces/default/deployments"]`)
	p.wantAbsent(`.ro-sidebar a.menu-item[href="/clusters/test/namespaces/default/deployments"] .menu-count`)

	// Resource Types is chrome, not a kind: never counted.
	p.wantAbsent(`.ro-sidebar a.menu-item[href="/clusters/test/namespaces/default/_resource-types"] .menu-count`)
}

// TestSidebarCountsCachedForTTL pins the Server-held cache: the first render
// issues the limit=1 fan-out, a second render inside the 15s TTL issues ZERO
// new limit=1 fetches, and a render after the TTL expires re-fetches. The
// clock is the injected s.now, advanced explicitly.
func TestSidebarCountsCachedForTTL(t *testing.T) {
	var fetches atomic.Int64
	fake, err := fakeapi.New(fakeapi.WithListRecorder(func(r *http.Request) {
		if r.URL.Query().Get("limit") == "1" {
			fetches.Add(1)
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)

	t0 := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	app := newServer(t, configForFixture(fake.URL), t0)

	get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)
	first := fetches.Load()
	if first == 0 {
		t.Fatal("first render issued no limit=1 count fetches")
	}

	get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)
	if got := fetches.Load(); got != first {
		t.Fatalf("second render inside the TTL re-hit the fixture: %d limit=1 fetches, want %d", got, first)
	}

	app.now = fixedClock(t0.Add(countTTL + time.Second))
	get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)
	if got := fetches.Load(); got <= first {
		t.Fatalf("render after the TTL did not re-fetch: %d limit=1 fetches, want > %d", got, first)
	}
}

// TestCountsCallerCancellationIsNotNegativeCached pins the cancellation
// boundary of the negative cache: a count fetch that fails because the
// CALLER's own context was cancelled (an aborted page load) says nothing about
// the kind, so it must NOT be remembered as a failure -- the old behaviour
// blanked every sidebar count for a full TTL after one aborted load. The very
// next render re-fetches and the count lands. Genuine kind failures keep their
// deliberate TTL caching (TestSidebarCountsCachedForTTL), and the per-fetch
// deadline (DeadlineExceeded, not Canceled) stays cached too.
func TestCountsCallerCancellationIsNotNegativeCached(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC))
	cluster, ok := app.manager.Get("test")
	if !ok {
		t.Fatal("fixture cluster missing")
	}
	resource := kube.ResourceType{APIVersion: "v1", Version: "v1", Kind: "Pod", Plural: "pods", Namespaced: true}
	newTargets := func() []countTarget {
		return []countTarget{{item: &navItem{}, resource: resource, namespace: "default"}}
	}

	// The aborted page load: the parent (request) context is already cancelled
	// when the count fan-out starts.
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	aborted := newTargets()
	app.attachSidebarCounts(cancelledCtx, cluster.Client, "test", aborted)
	if aborted[0].item.HasCount {
		t.Fatalf("a cancelled fetch cannot produce a count, got %q", aborted[0].item.Count)
	}
	key := countKey{cluster: "test", apiVersion: "v1", plural: "pods", namespace: "default"}
	if entry, hit := app.counts.lookup(key, app.clock()); hit {
		t.Fatalf("the caller's own cancellation was negative-cached as %+v -- one aborted load would blank the sidebar counts for %v", entry, countTTL)
	}

	// The next render (inside the same TTL window) re-fetches and the count
	// lands (pods_table.json: 2 rows, the limit-ignoring fixture shape).
	next := newTargets()
	app.attachSidebarCounts(context.Background(), cluster.Client, "test", next)
	if !next[0].item.HasCount || next[0].item.Count != "2" {
		t.Fatalf("the render after the aborted load did not re-fetch the count: HasCount=%t Count=%q", next[0].item.HasCount, next[0].item.Count)
	}
}

// TestCountsPerFetchDeadlineIsNegativeCached pins the complementary law to
// TestCountsCallerCancellationIsNotNegativeCached: a fetch that fails because
// the PER-FETCH deadline fired (countFetchTimeout, surfacing as
// DeadlineExceeded) while the CALLER's context is still alive DOES get
// negative-cached -- a dead-slow kind costs one probe per TTL window, not one
// per render. The backend stalls the limit=1 request until its fetch context is
// cancelled, so the count fetch returns DeadlineExceeded.
func TestCountsPerFetchDeadlineIsNegativeCached(t *testing.T) {
	fake := newServerFakeAPI(t)
	target, err := url.Parse(fake.URL)
	if err != nil {
		t.Fatal(err)
	}
	rp := httputil.NewSingleHostReverseProxy(target)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("limit") == "1" {
			<-r.Context().Done() // the count fetch hit its per-fetch deadline
			http.Error(w, "stalled", http.StatusInternalServerError)
			return
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(slow.Close)
	app := newServer(t, configForFixture(slow.URL), time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC))
	cluster, ok := app.manager.Get("test")
	if !ok {
		t.Fatal("fixture cluster missing")
	}
	resource := kube.ResourceType{APIVersion: "v1", Version: "v1", Kind: "Pod", Plural: "pods", Namespaced: true}
	targets := []countTarget{{item: &navItem{}, resource: resource, namespace: "default"}}

	// The parent context stays alive; only the per-fetch countFetchTimeout fires.
	app.attachSidebarCounts(context.Background(), cluster.Client, "test", targets)
	if targets[0].item.HasCount {
		t.Fatalf("a deadline-failed fetch cannot produce a count, got %q", targets[0].item.Count)
	}
	key := countKey{cluster: "test", apiVersion: "v1", plural: "pods", namespace: "default"}
	entry, hit := app.counts.lookup(key, app.clock())
	if !hit {
		t.Fatalf("the per-fetch deadline was NOT cached -- a dead-slow kind would re-probe every render")
	}
	if entry.ok {
		t.Fatalf("the deadline failure was cached as a success %+v, want a negative entry", entry)
	}
}

// TestSidebarCountsAbsentInMultiClusterScope pins the single-cluster-only
// rule: the `_all` (multi-cluster) sidebar renders its entries but NO counts.
func TestSidebarCountsAbsentInMultiClusterScope(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/_all/pods", http.StatusOK)

	p.wantHas(".ro-sidebar a.menu-item")
	p.wantAbsent(".ro-sidebar .menu-count")
}

// TestSidebarCountsSlowFetchDoesNotStallRender pins the shared fetch deadline:
// with every limit=1 request stalling far beyond the timeout, the page must
// still render promptly (counts simply absent). The stalling handler returns
// as soon as the client gives up, so the bound proves the deadline fired.
func TestSidebarCountsSlowFetchDoesNotStallRender(t *testing.T) {
	fake := newServerFakeAPI(t)
	target, err := url.Parse(fake.URL)
	if err != nil {
		t.Fatal(err)
	}
	rp := httputil.NewSingleHostReverseProxy(target)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("limit") == "1" {
			select {
			case <-r.Context().Done(): // the count fetch hit its deadline
			case <-time.After(10 * time.Second):
			}
			http.Error(w, "stalled", http.StatusInternalServerError)
			return
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(slow.Close)
	app := newServer(t, configForFixture(slow.URL), time.Now())

	start := time.Now()
	p := get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("count fan-out stalled the page render for %v; the %v deadline did not bound it", elapsed, countFetchTimeout)
	}
	p.wantAbsent(".ro-sidebar .menu-count")
}

// TestSidebarSecretsJoinPodManagement pins the sidebar composition delta: with
// IncludeSecrets the Secrets entry renders INSIDE Pod Management (after
// ConfigMaps, matching the prototype order), the label set stays exactly the
// three groups + ONE Meta (the hardcoded Meta section is not duplicated), and
// the Meta links stay Resource Types + Events. Without IncludeSecrets the
// entry does not render at all (the secret barrier).
func TestSidebarSecretsJoinPodManagement(t *testing.T) {
	app := newServer(t, withSecrets(t), time.Now())
	p := get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)

	if labels := p.texts(".ro-sidebar .menu-label"); strings.Join(labels, "|") != "Cluster Resources|Controllers|Pod Management|Meta" {
		t.Fatalf("sidebar menu-labels = %v, want the three groups + exactly one Meta", labels)
	}

	var podMgmt []string
	p.doc.Find(".ro-sidebar .menu-list").Eq(2).Find("a.menu-item").Each(func(_ int, s *goquery.Selection) {
		if href, ok := s.Attr("href"); ok {
			podMgmt = append(podMgmt, href)
		}
	})
	want := []string{
		"/clusters/test/namespaces/default/ingresses",
		"/clusters/test/namespaces/default/services",
		"/clusters/test/namespaces/default/pods",
		"/clusters/test/namespaces/default/configmaps",
		"/clusters/test/namespaces/default/secrets",
	}
	if strings.Join(podMgmt, "|") != strings.Join(want, "|") {
		t.Fatalf("Pod Management entries = %v, want %v (Secrets after ConfigMaps)", podMgmt, want)
	}

	// The hardcoded Meta section stays intact alongside the group change.
	p.wantHas(`.ro-sidebar a.menu-item[href="/clusters/test/namespaces/default/_resource-types"]`)
	p.wantHas(`.ro-sidebar a.menu-item[href="/clusters/test/namespaces/default/events"]`)

	// Secrets count rides the same pipeline (render_secrets_table.json: 3 rows).
	p.wantText(`.ro-sidebar a.menu-item[href="/clusters/test/namespaces/default/secrets"] .menu-count`, "3")

	// Secret barrier: under the default config the entry is absent entirely.
	bare := newServer(t, baseConfig(t), time.Now())
	pb := get(t, bare, "/clusters/test/namespaces/default/pods", http.StatusOK)
	pb.wantAbsent(`.ro-sidebar a.menu-item[href="/clusters/test/namespaces/default/secrets"]`)
	if labels := pb.texts(".ro-sidebar .menu-label"); strings.Join(labels, "|") != "Cluster Resources|Controllers|Pod Management|Meta" {
		t.Fatalf("default-config menu-labels = %v", labels)
	}
}

// TestSidebarSecretsFallbackRespectsBarrier pins the no-discovery fallback
// (multi-cluster `_all` scope): the curated Secrets entry follows
// IncludeSecrets there too, instead of advertising a dead link.
func TestSidebarSecretsFallbackRespectsBarrier(t *testing.T) {
	bare := newServer(t, baseConfig(t), time.Now())
	p := get(t, bare, "/clusters/_all/pods", http.StatusOK)
	p.wantHas(`.ro-sidebar a.menu-item[href="/clusters/_all/pods"]`)
	p.wantAbsent(`.ro-sidebar a.menu-item[href="/clusters/_all/secrets"]`)

	withS := newServer(t, withSecrets(t), time.Now())
	ps := get(t, withS, "/clusters/_all/pods", http.StatusOK)
	ps.wantHas(`.ro-sidebar a.menu-item[href="/clusters/_all/secrets"]`)
}

// TestSidebarConfigOverrideCarriesCounts pins that a config-driven sidebar
// keeps working and its entries ride the same count pipeline.
func TestSidebarConfigOverrideCarriesCounts(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Sidebar = []config.SidebarGroup{{Label: "Mine", Resources: []string{"pods", "nodes"}}}
	app := newServer(t, cfg, time.Now())
	p := get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)

	if labels := p.texts(".ro-sidebar .menu-label"); strings.Join(labels, "|") != "Mine|Meta" {
		t.Fatalf("config-sidebar menu-labels = %v, want [Mine Meta]", labels)
	}
	p.wantText(`.ro-sidebar a.menu-item[href="/clusters/test/namespaces/default/pods"] .menu-count`, "2")
	p.wantText(`.ro-sidebar a.menu-item[href="/clusters/test/nodes"] .menu-count`, "1")
}

// configForFixture is baseConfig with the cluster pointed at an explicit
// fixture URL (a counts proxy or a recorder-instrumented fakeapi).
func configForFixture(serverURL string) *config.Config {
	return &config.Config{
		Port:         8080,
		Clusters:     []config.ClusterConnection{{Name: "test", Server: serverURL}},
		DefaultTheme: "dark",
		NoAccessLogs: true,
	}
}

// newCountsProxy fronts the fakeapi with an interceptor: requests the
// intercept func claims are answered with its payload verbatim; everything
// else (discovery, the page's own table fetch) passes through to the fixture
// server. This lets one test serve an exact wire shape for the limit=1 count
// fetch while the rest of the page renders normally.
func newCountsProxy(t *testing.T, upstream string, intercept func(*http.Request) ([]byte, bool)) *httptest.Server {
	t.Helper()
	target, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	rp := httputil.NewSingleHostReverseProxy(target)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body, ok := intercept(r); ok {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
			return
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// countsTableJSON builds a minimal meta.k8s.io Table response with `rows`
// rows and an optional metadata.remainingItemCount + continue token --
// the exact wire shapes of a paginating apiserver, an exact list, and a
// limit-ignoring server.
func countsTableJSON(t *testing.T, rows int, remaining *int64) []byte {
	t.Helper()
	rowDocs := make([]map[string]any, 0, rows)
	for i := range rows {
		rowDocs = append(rowDocs, map[string]any{
			"cells": []any{fmt.Sprintf("obj-%d", i)},
			"object": map[string]any{
				"apiVersion": "v1", "kind": "Pod",
				"metadata": map[string]any{"name": fmt.Sprintf("obj-%d", i), "namespace": "default"},
			},
		})
	}
	meta := map[string]any{"resourceVersion": "1"}
	if remaining != nil {
		meta["remainingItemCount"] = *remaining
		meta["continue"] = "proxy-continue"
	}
	doc := map[string]any{
		"kind":              "Table",
		"apiVersion":        "meta.k8s.io/v1",
		"metadata":          meta,
		"columnDefinitions": []map[string]any{{"name": "Name", "type": "string"}},
		"rows":              rowDocs,
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func i64(v int64) *int64 { return &v }
