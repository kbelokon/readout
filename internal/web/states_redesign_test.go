package web

// states_redesign_test.go drives the Unit-14 list/detail STATE surface (D11)
// through the REAL render pipeline: the whole-list forbidden / unreachable states
// (a single-cluster list that wholly failed), the genuinely-empty + empty-FILTERED
// states, the detail-page forbidden / unreachable states, and the client-side
// stale markup hooks (the hidden `.ro-banner.warn` + the dim target + the
// readout.js handler). Forbidden names the verb/resource/namespace + 403;
// unreachable shows the REAL transport error string + a read-only Retry + Back to
// clusters; the all-cluster partial-failure banner (Unit 5) is NOT involved (the
// single-cluster invariant).

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/kbelokon/readout/internal/config"
)

// stateFakeOptions parametrizes a single-cluster fake API for the state tests.
type stateFakeOptions struct {
	// forbidPods makes the pods table/list endpoints return a real apiserver 403
	// Status (so kube.IsForbidden is true) AFTER discovery succeeds -- the
	// forbidden whole-list state.
	forbidPods bool
	// serverErrorPods makes the pods endpoints return an apiserver 500
	// InternalError Status (the fakeapi fail-lists?mode=500 shape) -- the 5xx
	// half of the unreachable whole-list state (D16).
	serverErrorPods bool
}

// serverErrorFixtureMessage is the EXACT InternalError Status message the state
// fake returns in serverErrorPods mode; the unreachable card must carry it
// verbatim (SPEC §1.5).
const serverErrorFixtureMessage = "Internal error occurred: state fixture 500 mode is active"

// newStateFakeAPI builds a fake kube API with full discovery plus a pods
// endpoint that is either healthy or returns a 403 Status. It reuses the shared
// discovery + pods fixtures so the resource-list path resolves the type the same
// way the production fixtures do.
func newStateFakeAPI(t *testing.T, opts stateFakeOptions) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	fixture := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(stateFixture(t, name))
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
	podsHandler := func(w http.ResponseWriter, r *http.Request) {
		if opts.forbidPods {
			// A real apiserver 403: a Status object with reason Forbidden naming the
			// verb/resource/namespace, so client-go surfaces it as IsForbidden and the
			// state card carries the verb/resource/namespace.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","message":"pods is forbidden: User \"viewer\" cannot list resource \"pods\" in API group \"\" in the namespace \"default\"","reason":"Forbidden","details":{"kind":"pods"},"code":403}`))
			return
		}
		if opts.serverErrorPods {
			// A real apiserver 500: an InternalError Status (the fakeapi
			// fail-lists?mode=500 shape), so kube.IsServerError is true.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","message":"` + serverErrorFixtureMessage + `","reason":"InternalError","code":500}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.Header.Get("Accept"), "as=Table") {
			_, _ = w.Write(stateFixture(t, "data/pods_table.json"))
			return
		}
		_, _ = w.Write(stateFixture(t, "data/pods_with_node_list.json"))
	}
	mux.HandleFunc("/api/v1/namespaces/default/pods", podsHandler)
	mux.HandleFunc("/api/v1/namespaces/default/pods/", podsHandler) // detail Get
	mux.HandleFunc("/api/v1/pods", podsHandler)
	mux.HandleFunc("/api/v1/namespaces", fixture("data/render_namespaces_list.json"))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// newToggleableStateAPI builds a single-cluster fake API whose pods endpoint is
// HEALTHY until `forbid` flips true, then returns a real apiserver 403. The flag
// is atomic so the concurrent discovery goroutines stay race-safe under -race.
// It models a list that loaded rows, then went forbidden on the next refresh.
func newToggleableStateAPI(t *testing.T, forbid *atomic.Bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	fixture := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(stateFixture(t, name))
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
	podsHandler := func(w http.ResponseWriter, r *http.Request) {
		if forbid.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","message":"pods is forbidden: User \"viewer\" cannot list resource \"pods\" in API group \"\" in the namespace \"default\"","reason":"Forbidden","details":{"kind":"pods"},"code":403}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.Header.Get("Accept"), "as=Table") {
			_, _ = w.Write(stateFixture(t, "data/pods_table.json"))
			return
		}
		_, _ = w.Write(stateFixture(t, "data/pods_with_node_list.json"))
	}
	mux.HandleFunc("/api/v1/namespaces/default/pods", podsHandler)
	mux.HandleFunc("/api/v1/pods", podsHandler)
	mux.HandleFunc("/api/v1/namespaces", fixture("data/render_namespaces_list.json"))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// stateFixture reads a fakeapi fixture (a sibling of readFixture, kept local so
// this file does not depend on server_test.go internals beyond the shared
// fixtures directory).
func stateFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "tests", "unit", "fakeapi", "fixtures", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

// newDeadCluster returns the URL of an httptest server that has been closed, so
// every request to it is refused at the transport layer (a "connection refused"
// dial error) -- the unreachable state.
func newDeadCluster(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NewServeMux())
	url := srv.URL
	srv.Close()
	return url
}

func newStateServer(t *testing.T, clusterURL string) *Server {
	t.Helper()
	return newServer(t, &config.Config{
		Port:         8080,
		Clusters:     []config.ClusterConnection{{Name: "test", Server: clusterURL}},
		DefaultTheme: "dark",
		NoAccessLogs: true,
	}, time.Now())
}

// forbiddenFixtureMessage is the EXACT apiserver Status message the state fake
// returns on a forbidden pods list -- the verbatim-error law (SPEC §1.5/D16)
// demands the card carry it byte-for-byte in the mono errdetail block.
const forbiddenFixtureMessage = `pods is forbidden: User "viewer" cannot list resource "pods" in API group "" in the namespace "default"`

// TestForbiddenListState proves a single-cluster pods list whose backend returns
// a 403 renders the forbidden whole-list state (no table) with the prototype
// markup (D16): the warn lock tile, the headline naming the verb/resource/
// namespace, ONE plain-language line, the VERBATIM 403 Status message in the
// mono `.errdetail` block, Retry + Back to clusters -- AND it does NOT render
// the all-cluster partial-failure banner (the single-cluster invariant).
func TestForbiddenListState(t *testing.T) {
	api := newStateFakeAPI(t, stateFakeOptions{forbidPods: true})
	app := newStateServer(t, api.URL)
	p := get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)

	// No table rows -- the state replaces the table.
	p.wantAbsent("td.cell-name")
	// The forbidden headline (prototype copy): verb + resource + quoted namespace.
	p.wantText(".ro-rd .ro-empty-lg h3", "Not allowed to list pods in “default”")
	// The glyph tile carries the warn tone.
	p.wantHas(".ro-rd .ro-empty-lg .ro-empty-glyph.warn")
	// ONE plain-language line above the verbatim block.
	p.wantText(".ro-rd .ro-empty-lg p", "Your credentials can browse this cluster, but RBAC denies this view.")
	// The errdetail block carries the REAL apiserver Status message VERBATIM.
	detail := p.text(".ro-rd .ro-empty-lg .errdetail")
	if !strings.Contains(detail, "403 Forbidden") {
		t.Fatalf("forbidden errdetail %q missing the 403 prefix", detail)
	}
	if !strings.Contains(detail, forbiddenFixtureMessage) {
		t.Fatalf("forbidden errdetail %q does not carry the verbatim fixture Status message", detail)
	}
	// Both actions (prototype): Retry (a read-only GET) + Back to clusters.
	labels := p.texts(".ro-rd .ro-empty-lg .ro-empty-actions a")
	if !contains(labels, "Retry") || !contains(labels, "Back to clusters") {
		t.Fatalf("forbidden actions = %v, want Retry + Back to clusters", labels)
	}
	if !contains(p.attrs(".ro-rd .ro-empty-lg .ro-empty-actions a", "href"), "/clusters") {
		t.Fatalf("forbidden missing the Back to clusters href")
	}
	// The single-cluster invariant: NO partial-failure banner.
	p.wantAbsent(".ro-partial-note")
	p.wantAbsent(".ro-banner.warn:not(.ro-stale-banner)")
}

// TestUnreachableListState proves a single-cluster list pointed at a dead backend
// renders the unreachable state with the prototype markup (D16): the err unplug
// tile, "Can’t reach <cluster>" naming the cluster, the plain transport line,
// the REAL transport error string in the mono `.errdetail` block (never a cute
// message, SPEC §1.5) + a read-only Retry GET + Back to clusters.
func TestUnreachableListState(t *testing.T) {
	app := newStateServer(t, newDeadCluster(t))
	p := get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)

	p.wantAbsent("td.cell-name")
	p.wantText(".ro-rd .ro-empty-lg h3", "Can’t reach test")
	p.wantHas(".ro-rd .ro-empty-lg .ro-empty-glyph.err")
	p.wantText(".ro-rd .ro-empty-lg p", "The request never made it to the apiserver.")
	// The errdetail is the REAL transport error (a dial/connection-refused
	// string) -- not a sanitized placeholder.
	detail := p.text(".ro-rd .ro-empty-lg .errdetail")
	if !strings.Contains(strings.ToLower(detail), "connect") && !strings.Contains(strings.ToLower(detail), "refused") && !strings.Contains(strings.ToLower(detail), "dial") {
		t.Fatalf("unreachable errdetail %q does not look like a real transport error", detail)
	}
	// Retry is a read-only GET back to the same list URL; Back to clusters escapes.
	actions := p.attrs(".ro-rd .ro-empty-lg .ro-empty-actions a", "href")
	if !contains(actions, "/clusters/test/namespaces/default/pods") {
		t.Fatalf("unreachable Retry href missing the read-only list GET: %v", actions)
	}
	if !contains(actions, "/clusters") {
		t.Fatalf("unreachable missing Back to clusters: %v", actions)
	}
	labels := p.texts(".ro-rd .ro-empty-lg .ro-empty-actions a")
	if !contains(labels, "Retry") {
		t.Fatalf("unreachable missing a Retry action: %v", labels)
	}
	// Still no partial banner (single cluster).
	p.wantAbsent(".ro-partial-note")
}

// TestApiserver500ListStateIsUnreachable pins the 5xx half of the unreachable
// classification (D16, the e2e fail-lists?mode=500 contract): an apiserver that
// answers the list with an InternalError STATUS (HTTP 500 + a Status body)
// renders the SAME unreachable card -- the verbatim Status message in the mono
// errdetail block -- with the truthful apiserver-answered plain line. 4xx
// Statuses keep their existing non-state handling (the boundary is pinned by
// TestDetailNotFoundStaysA404 and the partial-banner facts).
func TestApiserver500ListStateIsUnreachable(t *testing.T) {
	api := newStateFakeAPI(t, stateFakeOptions{serverErrorPods: true})
	app := newStateServer(t, api.URL)
	p := get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)

	p.wantAbsent("td.cell-name")
	p.wantText(".ro-rd .ro-empty-lg h3", "Can’t reach test")
	p.wantText(".ro-rd .ro-empty-lg p", "The apiserver answered with an error.")
	detail := p.text(".ro-rd .ro-empty-lg .errdetail")
	if !strings.Contains(detail, serverErrorFixtureMessage) {
		t.Fatalf("500 errdetail %q does not carry the verbatim Status message", detail)
	}
	p.wantAbsent(".ro-partial-note")
}

// TestUnreachableRetryIsReadOnlyGET pins that the unreachable Retry is a plain
// anchor (a GET), never a POST/form -- the read-only floor must hold for the
// retry affordance.
func TestUnreachableRetryIsReadOnlyGET(t *testing.T) {
	app := newStateServer(t, newDeadCluster(t))
	p := get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)
	// The state actions are anchors (GET), and there is no form inside the state.
	if p.count(".ro-rd .ro-empty-lg .ro-empty-actions a") == 0 {
		t.Fatalf("unreachable state has no anchor actions")
	}
	if p.has(".ro-rd .ro-empty-lg form") || p.has(".ro-rd .ro-empty-lg button[type=submit]") {
		t.Fatalf("unreachable state must not carry a write form/submit")
	}
}

// TestAutoRefreshWholeListErrorIsNon2xxNotStateCard pins the stale-preservation
// invariant (Wave 7 regression): a single-cluster list loads rows, then the
// cluster goes forbidden/unreachable. The full-page FIRST LOAD shows the state
// card (no prior rows to keep), but the AUTO-REFRESH `_table` partial must NOT
// return a 200 carrying the state card -- morph would swap the last-good rows out
// for the card and htmx:afterSwap would clear the dim, blanking the data on a
// transient blip. The partial must instead return a NON-2xx so htmx keeps the
// existing rows and htmx:responseError fires the client-side stale path.
//
// MUTATION CHECK (documented): with the OLD partial behaviour (render
// ResourceTable at 200 regardless of view.State), the partial GET below returned
// 200 with the "Not allowed to list pods..." card -- so the `code == 200` /
// state-card-body assertions here FAIL. Verified by reverting resourceListPartial
// to the unconditional 200 render: this test went red (partial code = 200, body
// carried `.ro-empty-lg`).
func TestAutoRefreshWholeListErrorIsNon2xxNotStateCard(t *testing.T) {
	var forbid atomic.Bool
	api := newToggleableStateAPI(t, &forbid)
	app := newStateServer(t, api.URL)

	// First load (full page) succeeds with rows -- the baseline last-good data.
	p := get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)
	if got := p.texts("td.cell-name"); len(got) == 0 {
		t.Fatalf("first load rendered no rows; cannot exercise stale preservation")
	}

	// The cluster now fails on the next fetch (forbidden whole-list).
	forbid.Store(true)

	// Sanity: the FULL page now renders the state card (first-load behaviour
	// unchanged -- there are no prior rows in a fresh full-page request).
	full := get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)
	full.wantHas(".ro-rd .ro-empty-lg h3") // the whole-list state card

	// The AUTO-REFRESH partial (ro:refresh target) must NOT 200-with-state-card.
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods/_table", nil))
	if rec.Code == http.StatusOK {
		t.Fatalf("partial refresh on a whole-list error returned 200 (morph would blank the rows); want a non-2xx so htmx keeps them. body=%s", rec.Body.String())
	}
	if rec.Code < 400 {
		t.Fatalf("partial refresh status = %d, want a >=400 error so htmx:responseError fires", rec.Code)
	}
	// And it must NOT carry the state card markup (which would be swapped in if the
	// non-2xx body were ever morphed); the body is the error page, not the card.
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(rec.Body.String()))
	if err != nil {
		t.Fatalf("parse partial body: %v", err)
	}
	if doc.Find(".ro-empty-lg").Length() != 0 {
		t.Fatalf("partial error response must not carry the whole-list state card; body=%s", rec.Body.String())
	}
}

// TestAutoRefreshEmptyPartialStillRendersNormally pins the boundary of the fix: a
// partial refresh that comes back EMPTY (zero rows, NO error -> no whole-list
// State) still renders a normal 200 table fragment (the empty-state row), NOT a
// non-2xx. Only the unreachable/forbidden whole-list ERROR goes non-2xx.
func TestAutoRefreshEmptyPartialStillRendersNormally(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/empty/pods/_table", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("empty partial refresh status = %d, want 200 (an empty list is not an error)\nbody=%s", rec.Code, rec.Body.String())
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(rec.Body.String()))
	if err != nil {
		t.Fatalf("parse empty partial body: %v", err)
	}
	if doc.Find(".ro-empty-row .ro-empty-title").Length() == 0 {
		t.Fatalf("empty partial refresh must render the empty-state row; body=%s", rec.Body.String())
	}
}

// TestEmptyListState proves a genuinely-empty single-cluster list (zero rows, no
// filter) renders the prototype plain-empty sentence + the broad next action
// ("Show <plural> across all namespaces" -- namespaced kinds only).
func TestEmptyListState(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/namespaces/empty/pods", http.StatusOK)

	p.wantAbsent("td.cell-name")
	p.wantText(".ro-empty-row .ro-empty-lg .ro-empty-title", "No Pods found in namespace “empty”")
	// The broad next action.
	p.wantText(".ro-empty-row .ro-empty-lg .ro-empty-actions a", "Show pods across all namespaces")
	p.wantAttr(".ro-empty-row .ro-empty-lg .ro-empty-actions a", "href", "/clusters/test/namespaces/_all/pods?")
}

// TestClusterScopedListStateHasNoNamespaceCrumbOrLink pins the SPEC §5/§9
// clusterScoped rule on the shipped engine: a cluster-scoped kind (nodes)
// renders NO namespace breadcrumb segment and NO "across all namespaces" link
// -- the canonical route carries no namespace, so neither affordance may
// appear.
func TestClusterScopedListStateHasNoNamespaceCrumbOrLink(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/nodes", http.StatusOK)

	// Breadcrumb: exactly cluster + the active plural -- no namespace segment.
	crumbs := p.texts(".ro-rd .ro-breadcrumb li")
	if len(crumbs) != 2 || crumbs[0] != "test" || crumbs[1] != "nodes" {
		t.Fatalf("cluster-scoped breadcrumb = %v, want [test nodes]", crumbs)
	}
	// No all-namespaces link anywhere in the list chrome.
	for _, text := range p.texts(".ro-table-meta a") {
		if strings.Contains(text, "across all namespaces") {
			t.Fatalf("cluster-scoped list offers an all-namespaces link: %q", text)
		}
	}
}

// TestEmptyFilterListState proves a filter that hides every row renders the
// empty-FILTERED state per the prototype: the card headline, the plain line,
// the ACTIVE chips inline in `.ro-empty-chips` (each ✕ a read-only GET dropping
// just that filter) + a Clear-filters action that drops the whole set.
func TestEmptyFilterListState(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/namespaces/default/pods?filter=zzz-none&selector=app%3Dnope", http.StatusOK)

	p.wantAbsent("td.cell-name")
	p.wantText(".ro-empty-row .ro-empty-lg h3", "No Pods match the active filters")
	p.wantText(".ro-empty-row .ro-empty-lg p", "Each chip narrows the list. Remove one, or clear them all.")

	// One removable chip per active filter (filter + selector), INLINE in the
	// card, each ✕ dropping only that param.
	chips := p.texts(".ro-empty-row .ro-empty-lg .ro-empty-chips .ro-scope-chip")
	if len(chips) != 2 {
		t.Fatalf("empty-filtered chips = %v, want one per active filter (filter, selector)", chips)
	}
	joined := strings.Join(chips, " | ")
	if !strings.Contains(joined, "zzz-none") || !strings.Contains(joined, "app=nope") {
		t.Fatalf("empty-filtered chips %q do not name both active filters", joined)
	}
	removeHrefs := p.attrs(".ro-empty-row .ro-empty-lg .ro-empty-chips .ro-scope-chip a.chip-x", "href")
	// Removing the filter chip keeps the selector (and vice-versa) -- a one-at-a-time
	// peel. The selector value's '=' is %-encoded in the query (only parens stay
	// literal in this codec).
	if !contains(removeHrefs, "/clusters/test/namespaces/default/pods?selector=app%3Dnope") {
		t.Fatalf("filter chip ✕ should drop only filter, keeping selector: %v", removeHrefs)
	}
	if !contains(removeHrefs, "/clusters/test/namespaces/default/pods?filter=zzz-none") {
		t.Fatalf("selector chip ✕ should drop only selector, keeping filter: %v", removeHrefs)
	}
	// Clear filters drops the whole set (a read-only GET back to the bare list).
	p.wantText(".ro-empty-row .ro-empty-lg .ro-empty-actions a", "Clear filters")
	p.wantAttr(".ro-empty-row .ro-empty-lg .ro-empty-actions a", "href", "/clusters/test/namespaces/default/pods")
}

// TestGlobalProgressBar proves the single global top progress rail (#ro-progress
// in the layout) is the loading indicator for BOTH every hx-boost navigation (the
// body carries hx-indicator="#ro-progress") AND the in-place list auto-refresh
// (#resource-list-content points its hx-indicator at it). The v1 always-on
// per-list skeleton stays retired (it flashed on every navigation); the D16
// skeleton is a DIFFERENT, gated mechanism -- an inert template outside the
// swap target, cloned only into an EMPTY region (TestLoadingSkeletonStateHooks).
func TestGlobalProgressBar(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)

	// One global progress rail in the layout, an htmx indicator.
	p.wantHas("#ro-progress.ro-progress.htmx-indicator")
	p.wantHas("#ro-progress .ro-progress-bar")
	// The body drives it on every boosted navigation; the list refresh reuses it.
	p.wantAttr("body", "hx-indicator", "#ro-progress")
	p.wantAttr("#resource-list-content", "hx-indicator", "#ro-progress")
	// The retired v1 skeleton must be gone everywhere.
	p.wantAbsent(".ro-skeleton")
	p.wantAbsent(".sk-row")
	p.wantAbsent("#ro-list-skeleton")
}

// TestLoadingSkeletonStateHooks pins the D16/SPEC §7.19 loading skeleton: the
// full single-type page ships an INERT hidden #ro-skel-template OUTSIDE the
// #resource-list-content swap target (so it survives morphs and exists when the
// region is empty), whose rows mirror the VISIBLE column count and fade toward
// the bottom; the live region itself never renders skeleton rows on a normal
// paint (a populated table NEVER gets one -- the data-never-disappears law).
// The JS half (the empty-target gate) is pinned source-level below, like the
// stale handlers.
func TestLoadingSkeletonStateHooks(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)

	// The template exists, hidden, OUTSIDE the swap target.
	p.wantHas("#ro-skel-template[hidden]")
	p.wantAbsent("#resource-list-content #ro-skel-template")
	// The live region carries NO skeleton rows on a server-rendered paint.
	p.wantAbsent("#resource-list-content .skel-row")

	// Rows mirror the visible column layout: every skeleton row has exactly as
	// many bars as the table has headers, and the rows fade toward the bottom.
	headerCount := p.count("#resource-list-content thead th")
	rows := p.doc.Find("#ro-skel-template .skel-row")
	if rows.Length() < 2 {
		t.Fatalf("skeleton template has %d rows, want a stack of them", rows.Length())
	}
	rows.Each(func(i int, row *goquery.Selection) {
		if bars := row.Find(".skel").Length(); bars != headerCount {
			t.Fatalf("skeleton row %d has %d bars, want %d (the visible column count)", i, bars, headerCount)
		}
	})
	firstStyle, _ := rows.First().Find(".skel").First().Attr("style")
	lastStyle, _ := rows.Last().Find(".skel").First().Attr("style")
	if !strings.Contains(firstStyle, "opacity:1.00") {
		t.Fatalf("first skeleton row style = %q, want full opacity", firstStyle)
	}
	if lastStyle == firstStyle || !strings.Contains(lastStyle, "opacity:0.") {
		t.Fatalf("last skeleton row style = %q, want it faded below the first (%q)", lastStyle, firstStyle)
	}

	// Hiding a column shrinks the mirror: the template is column-aware.
	hidden := get(t, app, "/clusters/test/namespaces/default/pods?hidecols=Status", http.StatusOK)
	if got := hidden.doc.Find("#ro-skel-template .skel-row").First().Find(".skel").Length(); got != headerCount-1 {
		t.Fatalf("hidecols skeleton bars = %d, want %d", got, headerCount-1)
	}

	// The JS half: the skeleton fires ONLY into a BLANK region (zero element
	// children -- ANY existing content, table, state card, or banner, is
	// something a clone would wipe), clones the inert template, and a failed
	// request clears it. There is no headless JS runner in this suite, so pin
	// the source wiring exactly like the stale-handler test does; the behavior
	// itself is driven end to end by the designed-states e2e skeleton case.
	js := readoutJS(t)
	for _, needle := range []string{
		"ro-skel-template",        // the inert source template
		"listRegionIsEmpty",       // the empty-target gate
		"childElementCount === 0", // blank region = zero element children (a selector denylist once missed the banner-only region)
		"clearListSkeleton",       // failed request removes the skeleton
	} {
		if !strings.Contains(js, needle) {
			t.Fatalf("readout.js skeleton path missing %q", needle)
		}
	}
	// The gate is REAL *and the polarity is anchored*: the clone bails on
	// `|| !listRegionIsEmpty(content))` -- a populated region returns early.
	// The old pin (`listRegionIsEmpty\(content\)\)\s*\{\s*return`) matched the
	// INVERTED gate too (skeleton over live rows), so the `\|\|\s*!` prefix is
	// load-bearing.
	gated := regexp.MustCompile(`\|\|\s*!listRegionIsEmpty\(content\)\)\s*\{\s*return`)
	if !gated.MatchString(js) {
		t.Fatalf("skeleton clone is not polarity-gated on !listRegionIsEmpty (a populated table would get a skeleton)")
	}
}

// TestStatesStaleMarkupHooks proves the CLIENT-SIDE stale path has its markup
// hooks in the FIRST server response: a hidden `.ro-banner.warn` readout.js
// reveals (with the D16 copy: "Auto-refresh failed — showing the last good
// data", the "Retrying in Ns" countdown hook Unit 21 wires, and the Retry now
// control), and the dim target (#resource-list-content) the JS dims. The
// server never decides stale (no last-good cache); these are the hooks the JS
// needs.
func TestStatesStaleMarkupHooks(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)

	// A hidden stale banner exists (a `.ro-banner.warn` that is `hidden` on first
	// paint) and carries the D16 copy + the retry control.
	banner := p.doc.Find(".ro-banner.warn.ro-stale-banner")
	if banner.Length() != 1 {
		t.Fatalf("expected exactly one hidden stale banner, found %d", banner.Length())
	}
	if _, hidden := banner.Attr("hidden"); !hidden {
		t.Fatalf("stale banner must be hidden on first paint (JS reveals it)")
	}
	if title := normSpace(banner.Find(".bn-title").Text()); title != "Auto-refresh failed — showing the last good data" {
		t.Fatalf("stale banner title = %q, want the D16 copy", title)
	}
	if !strings.Contains(banner.Find(".bn-text").Text(), "Retrying in") {
		t.Fatalf("stale banner text = %q, want the Retrying-in line", banner.Find(".bn-text").Text())
	}
	// The countdown span is a wiring hook for Unit 21 (mono, data-stale-countdown).
	p.wantHas(".ro-stale-banner .bn-text span.mono[data-stale-countdown]")
	if got := normSpace(banner.Find(".ro-stale-retry").Text()); got != "Retry now" {
		t.Fatalf("stale banner retry label = %q, want %q", got, "Retry now")
	}
	// The dim target the JS toggles exists.
	p.wantHas("#resource-list-content")
}

// TestStatesStaleHandlerInReadoutJS pins the readout.js side of the stale path:
// the refresh-error handlers (htmx:responseError / htmx:sendError) that mark the
// list stale, the afterSwap handler that clears it, the dim class, and the
// read-only retry trigger. There is no headless JS runner in this suite, so this
// asserts the source wires the exact hooks the rendered markup exposes.
func TestStatesStaleHandlerInReadoutJS(t *testing.T) {
	js := readoutJS(t)
	for _, needle := range []string{
		"htmx:responseError",     // a non-2xx refresh reply -> stale
		"htmx:sendError",         // a transport failure on refresh -> stale
		"htmx:afterSwap",         // a recovered refresh clears stale
		"ro-stale",               // the dim class on #resource-list-content
		"ro-stale-banner",        // the hidden banner the handler reveals
		"data-ro-action='retry'", // the read-only retry control hook
		"resource-list-content",  // the dim target / refresh element id
		"ro:refresh",             // the read-only GET the retry re-fires
	} {
		if !strings.Contains(js, needle) {
			t.Fatalf("readout.js stale path missing %q", needle)
		}
	}
	// The stale path keeps the rows (it must NOT swap on error): the handlers mark
	// stale rather than blanking, and the retry triggers the existing ro:refresh
	// (a read-only GET), never a write.
	if strings.Contains(js, "innerHTML = ''") {
		t.Fatalf("stale path must not blank the rows")
	}

	// Gate is REAL, not just tokens-present: the stale handlers must be GATED on
	// the refresh element id (#resource-list-content), so a refactor that marked
	// EVERY htmx error stale (dropping the gate) would fail here. Pin (a) the
	// literal id gate substring, and (b) that the responseError listener routes
	// through the gate (isListRefreshEvent) rather than calling markListStale
	// unconditionally.
	if !strings.Contains(js, "id === 'resource-list-content'") {
		t.Fatalf("readout.js stale handler missing the literal refresh-element-id gate")
	}
	gatedResponseError := regexp.MustCompile(`(?s)htmx:responseError.{0,200}isListRefreshEvent`)
	if !gatedResponseError.MatchString(js) {
		t.Fatalf("htmx:responseError handler is not gated on isListRefreshEvent (would mark every htmx error stale)")
	}
	// The reveal/clear is not dropped: marking stale reveals the banner and a
	// recovered refresh re-hides it (banner.hidden flips both ways).
	if !strings.Contains(js, "banner.hidden = false") {
		t.Fatalf("stale handler must reveal the banner (banner.hidden = false)")
	}
	if !strings.Contains(js, "banner.hidden = true") {
		t.Fatalf("recovered refresh must re-hide the banner (banner.hidden = true)")
	}
}

// TestForbiddenDetailState proves the detail page also ships the forbidden state:
// a Get that 403s renders the `.ro-empty-lg` card (the prototype markup: warn
// tile, headline naming verb/resource/namespace, the verbatim 403 in errdetail)
// at 200, with the breadcrumb chrome intact -- not the bare error panel.
func TestForbiddenDetailState(t *testing.T) {
	api := newStateFakeAPI(t, stateFakeOptions{forbidPods: true})
	app := newStateServer(t, api.URL)
	p := get(t, app, "/clusters/test/namespaces/default/pods/nginx", http.StatusOK)

	// The detail chrome (breadcrumb) is intact; the body is the forbidden state.
	p.wantHas(".ro-rd .ro-breadcrumb")
	// The detail verb is the distinct literal "get" (build_resource.go), NOT the
	// list verb "list" -- pin it so a wrong/empty detail verb is caught.
	p.wantText(".ro-rd .ro-empty-lg h3", "Not allowed to get pods in “default”")
	p.wantHas(".ro-rd .ro-empty-lg .ro-empty-glyph.warn")
	detail := p.text(".ro-rd .ro-empty-lg .errdetail")
	if !strings.Contains(detail, "403") {
		t.Fatalf("detail forbidden errdetail %q missing 403", detail)
	}
	if !contains(p.attrs(".ro-rd .ro-empty-lg .ro-empty-actions a", "href"), "/clusters") {
		t.Fatalf("detail forbidden missing Back to clusters")
	}
}

// TestUnreachableDetailState proves the detail page ships the unreachable state
// with the REAL transport error in errdetail + Retry + Back to clusters, at 200.
func TestUnreachableDetailState(t *testing.T) {
	app := newStateServer(t, newDeadCluster(t))
	p := get(t, app, "/clusters/test/namespaces/default/pods/nginx", http.StatusOK)

	p.wantHas(".ro-rd .ro-breadcrumb")
	p.wantText(".ro-rd .ro-empty-lg h3", "Can’t reach test")
	p.wantHas(".ro-rd .ro-empty-lg .ro-empty-glyph.err")
	detail := p.text(".ro-rd .ro-empty-lg .errdetail")
	if !strings.Contains(strings.ToLower(detail), "connect") && !strings.Contains(strings.ToLower(detail), "refused") && !strings.Contains(strings.ToLower(detail), "dial") {
		t.Fatalf("detail unreachable errdetail %q does not look like a real transport error", detail)
	}
	actions := p.attrs(".ro-rd .ro-empty-lg .ro-empty-actions a", "href")
	if !contains(actions, "/clusters") {
		t.Fatalf("detail unreachable missing Back to clusters: %v", actions)
	}
}

// TestApiserver500DetailStateIsUnreachable pins the 5xx half of the DETAIL
// unreachable classification (build_resource.go detailState, the twin of
// TestApiserver500ListStateIsUnreachable): an apiserver that answers the object
// Get with an InternalError STATUS (HTTP 500 + a Status body) renders the SAME
// unreachable card at 200 -- the verbatim Status message in the mono errdetail
// block under the truthful apiserver-answered plain line -- with the detail
// chrome (breadcrumb) intact and Back to clusters present.
func TestApiserver500DetailStateIsUnreachable(t *testing.T) {
	api := newStateFakeAPI(t, stateFakeOptions{serverErrorPods: true})
	app := newStateServer(t, api.URL)
	p := get(t, app, "/clusters/test/namespaces/default/pods/nginx", http.StatusOK)

	p.wantHas(".ro-rd .ro-breadcrumb")
	p.wantText(".ro-rd .ro-empty-lg h3", "Can’t reach test")
	p.wantHas(".ro-rd .ro-empty-lg .ro-empty-glyph.err")
	p.wantText(".ro-rd .ro-empty-lg p", "The apiserver answered with an error.")
	detail := p.text(".ro-rd .ro-empty-lg .errdetail")
	if !strings.Contains(detail, serverErrorFixtureMessage) {
		t.Fatalf("detail 500 errdetail %q does not carry the verbatim Status message", detail)
	}
	actions := p.attrs(".ro-rd .ro-empty-lg .ro-empty-actions a", "href")
	if !contains(actions, "/clusters") {
		t.Fatalf("detail 500 state missing Back to clusters: %v", actions)
	}
	labels := p.texts(".ro-rd .ro-empty-lg .ro-empty-actions a")
	if !contains(labels, "Retry") {
		t.Fatalf("detail 500 state missing a Retry action: %v", labels)
	}
}

// TestDetailNotFoundStaysA404 pins the boundary: a missing object is NOT a
// cluster failure, so it keeps its real 404 status page (the state path only
// captures forbidden / unreachable). Guards against the state path swallowing a
// genuine not-found.
func TestDetailNotFoundStaysA404(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	get(t, app, "/clusters/test/namespaces/default/pods/ghost", http.StatusNotFound)
}

// newFirstRunServer builds a Server with ZERO configured clusters: no static
// clusters, no explicit kubeconfig, the in-cluster env blanked, and KUBECONFIG
// pointed at a zero-context kubeconfig -- the exact boot the e2e first-run
// harness performs. The kube manager must come up empty (not fail) for the
// screen to be reachable at all (the D17 reachability prerequisite, pinned on
// the kube side by TestZeroContextKubeconfigStartsEmpty).
func newFirstRunServer(t *testing.T) *Server {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", path)
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	return newServer(t, &config.Config{
		Port:         8080,
		DefaultTheme: "dark",
		NoAccessLogs: true,
	}, time.Now())
}

// TestFirstRunScreen proves the SPEC §7.2 first-run screen (D17): a server with
// zero configured clusters renders the instruction card on /clusters -- the
// literal "No clusters configured" headline, the command block carrying the
// binary's REAL config surface (KUBECONFIG env + the --config file alternative;
// the prototype's nonexistent --kubeconfig flag must NOT ship), the Setup docs
// link, and a Re-check that is a plain read-only GET reload -- instead of the
// empty clusters table.
func TestFirstRunScreen(t *testing.T) {
	app := newFirstRunServer(t)
	p := get(t, app, "/clusters", http.StatusOK)

	// The instruction card, not an empty table / filter chrome.
	p.wantText(".ro-rd .ro-firstrun h3", "No clusters configured")
	p.wantHas(".ro-rd .ro-firstrun .ro-empty-glyph")
	p.wantAbsent(".ro-rd table.ro-select-table")
	p.wantAbsent(".ro-rd form.tools-row")
	// The title row still names the surface with its zero count.
	p.wantText(".ro-rd .ro-title-row .ro-count", "0")
	// The command block: the REAL config surface, terminal-style.
	detail := p.text(".ro-firstrun .errdetail")
	if !strings.Contains(detail, "KUBECONFIG=~/.kube/config readout") {
		t.Fatalf("first-run command block %q missing the KUBECONFIG line", detail)
	}
	if !strings.Contains(detail, "--config") {
		t.Fatalf("first-run command block %q missing the --config alternative", detail)
	}
	if strings.Contains(detail, "--kubeconfig") {
		t.Fatalf("first-run command block %q ships the nonexistent --kubeconfig flag", detail)
	}
	// Setup docs + Re-check (a plain GET reload of this same page).
	labels := p.texts(".ro-firstrun .ro-empty-actions a")
	if !contains(labels, "Setup docs") || !contains(labels, "Re-check") {
		t.Fatalf("first-run actions = %v, want Setup docs + Re-check", labels)
	}
	if !contains(p.attrs(".ro-firstrun .ro-empty-actions a", "href"), "/clusters") {
		t.Fatalf("Re-check must be a read-only GET reload of /clusters")
	}
	// Re-check is an anchor, never a form/submit (the read-only floor).
	if p.has(".ro-firstrun form") || p.has(".ro-firstrun button[type=submit]") {
		t.Fatalf("first-run must not carry a write form/submit")
	}
}

// TestFirstRunHasNoLoginUI pins the D17 user decision: NO login screen
// anywhere. The v2 login screen (SPEC §7.1) is deliberately not implemented --
// /login is not a route (the "GET /" catch-all just bounces it to /clusters),
// POST /login does not exist, and no token input / SSO button renders; auth
// stays config-only.
func TestFirstRunHasNoLoginUI(t *testing.T) {
	app := newFirstRunServer(t)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/clusters" {
		t.Fatalf("GET /login = %d -> %q, want the catch-all 302 to /clusters (no login page exists, D17)", rec.Code, rec.Header().Get("Location"))
	}
	post := httptest.NewRecorder()
	app.Handler().ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/login", nil))
	if post.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /login = %d, want 405 (no login submission surface, D17)", post.Code)
	}
	p := get(t, app, "/clusters", http.StatusOK)
	p.wantAbsent("input[type=password]")
	p.wantAbsent(".login-card")
	p.wantBodyExcludes("Sign in")
}

// TestFirstRunNotShownWithClusters pins the gate boundary: a server WITH a
// configured cluster renders the normal clusters table, never the first-run
// instruction card.
func TestFirstRunNotShownWithClusters(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters", http.StatusOK)
	p.wantAbsent(".ro-firstrun")
	p.wantHas(".ro-rd table.ro-select-table")
}

// TestFirstRunNotShownOnBrokenInCluster pins the other gate boundary (the
// manager half is TestBrokenInClusterServiceAccountSurfacesAsBroken): a pod
// whose in-cluster ServiceAccount is BROKEN (env set, token unreadable) is a
// configured-but-broken cluster, NOT "nothing configured" -- the first-run
// instruction card must not swallow the failure (broken clusters suppress
// FirstRun in buildClustersData).
func TestFirstRunNotShownOnBrokenInCluster(t *testing.T) {
	if _, err := os.Stat("/var/run/secrets/kubernetes.io/serviceaccount/token"); err == nil {
		t.Skip("a real in-cluster ServiceAccount token exists; cannot fake a broken in-cluster env")
	}
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", path)
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")
	app := newServer(t, &config.Config{
		Port:         8080,
		DefaultTheme: "dark",
		NoAccessLogs: true,
	}, time.Now())

	p := get(t, app, "/clusters", http.StatusOK)
	p.wantAbsent(".ro-firstrun")
	p.wantHas(".ro-rd table.ro-select-table")
}
