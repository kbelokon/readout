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
}

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
			// state's hint carries the verb/resource/namespace.
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
		Clusters:     map[string]string{"test": clusterURL},
		DefaultTheme: "dark",
		NoAccessLogs: true,
	}, time.Now())
}

// TestForbiddenListState proves a single-cluster pods list whose backend returns
// a 403 renders the forbidden whole-list state (no table): the `.ro-empty-lg`
// card naming the verb/resource/namespace + a 403 hint, AND it does NOT render
// the all-cluster partial-failure banner (the single-cluster invariant).
func TestForbiddenListState(t *testing.T) {
	api := newStateFakeAPI(t, stateFakeOptions{forbidPods: true})
	app := newStateServer(t, api.URL)
	p := get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)

	// No table rows -- the state replaces the table.
	p.wantAbsent("td.cell-name")
	// The forbidden state card names the verb/resource/namespace.
	title := p.text(".ro-rd .ro-empty-lg h3")
	for _, needle := range []string{"list", "pods", "default"} {
		if !strings.Contains(title, needle) {
			t.Fatalf("forbidden title %q missing %q (verb/resource/namespace)", title, needle)
		}
	}
	// The hint carries 403 + the apiserver's verb/resource/namespace detail.
	hint := p.text(".ro-rd .ro-empty-lg .hint")
	for _, needle := range []string{"403", "forbidden", "list", "pods"} {
		if !strings.Contains(strings.ToLower(hint), strings.ToLower(needle)) {
			t.Fatalf("forbidden hint %q missing %q", hint, needle)
		}
	}
	// Forbidden offers Back to clusters (a stable denial has nothing to retry).
	p.wantText(".ro-rd .ro-empty-lg .ro-empty-actions a", "Back to clusters")
	p.wantAttr(".ro-rd .ro-empty-lg .ro-empty-actions a", "href", "/clusters")
	// The single-cluster invariant: NO partial-failure banner.
	p.wantAbsent(".ro-partial-note")
	p.wantAbsent(".ro-banner.warn:not(.ro-stale-banner)")
}

// TestUnreachableListState proves a single-cluster list pointed at a dead backend
// renders the unreachable state with the REAL transport error string (never a
// cute message, Principles §11) + a read-only Retry GET + Back to clusters.
func TestUnreachableListState(t *testing.T) {
	app := newStateServer(t, newDeadCluster(t))
	p := get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)

	p.wantAbsent("td.cell-name")
	p.wantText(".ro-rd .ro-empty-lg h3", "Can't reach this cluster")
	// The hint is the REAL transport error (a dial/connection-refused string),
	// rendered mono -- not a sanitized placeholder.
	hint := p.text(".ro-rd .ro-empty-lg .hint.mono")
	if !strings.Contains(strings.ToLower(hint), "connect") && !strings.Contains(strings.ToLower(hint), "refused") && !strings.Contains(strings.ToLower(hint), "dial") {
		t.Fatalf("unreachable hint %q does not look like a real transport error", hint)
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
// filter) renders the plain empty sentence + a broad next action.
func TestEmptyListState(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/namespaces/empty/pods", http.StatusOK)

	p.wantAbsent("td.cell-name")
	p.wantText(".ro-empty-row .ro-empty-lg .ro-empty-title", `No Pod objects in namespace "empty" found.`)
	// The broad next action.
	p.wantText(".ro-empty-row .ro-empty-lg .ro-empty-actions a", "Show pods across all namespaces")
	p.wantAttr(".ro-empty-row .ro-empty-lg .ro-empty-actions a", "href", "/clusters/test/namespaces/_all/pods?")
}

// TestEmptyFilterListState proves a filter that hides every row renders the
// empty-FILTERED state: removable filter chips (each ✕ a read-only GET dropping
// just that filter) + a Clear-filters action that drops the whole set.
func TestEmptyFilterListState(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/namespaces/default/pods?filter=zzz-none&selector=app%3Dnope", http.StatusOK)

	p.wantAbsent("td.cell-name")
	p.wantText(".ro-empty-row .ro-empty-lg h3", "No Pod objects match your filters")

	// One removable chip per active filter (filter + selector), each ✕ dropping
	// only that param.
	chips := p.texts(".ro-empty-row .ro-scope .ro-scope-chip")
	if len(chips) != 2 {
		t.Fatalf("empty-filtered chips = %v, want one per active filter (filter, selector)", chips)
	}
	joined := strings.Join(chips, " | ")
	if !strings.Contains(joined, "zzz-none") || !strings.Contains(joined, "app=nope") {
		t.Fatalf("empty-filtered chips %q do not name both active filters", joined)
	}
	removeHrefs := p.attrs(".ro-empty-row .ro-scope .ro-scope-chip a.retry", "href")
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
// (#resource-list-content points its hx-indicator at it). The old per-list loading
// skeleton was removed: it had no valid moment in readout (the first paint is
// server-rendered with rows and the morph refresh keeps them), and it flashed on
// every navigation because hx-boost marks an ancestor as loading.
func TestGlobalProgressBar(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)

	// One global progress rail in the layout, an htmx indicator.
	p.wantHas("#ro-progress.ro-progress.htmx-indicator")
	p.wantHas("#ro-progress .ro-progress-bar")
	// The body drives it on every boosted navigation; the list refresh reuses it.
	p.wantAttr("body", "hx-indicator", "#ro-progress")
	p.wantAttr("#resource-list-content", "hx-indicator", "#ro-progress")
	// The retired loading skeleton must be gone everywhere.
	p.wantAbsent(".ro-skeleton")
	p.wantAbsent(".sk-row")
	p.wantAbsent("#ro-list-skeleton")
}

// TestStatesStaleMarkupHooks proves the CLIENT-SIDE stale path has its markup
// hooks in the FIRST server response: a hidden `.ro-banner.warn` readout.js
// reveals, and the dim target (#resource-list-content) the JS dims. The server
// never decides stale (no last-good cache); these are the hooks the JS needs.
func TestStatesStaleMarkupHooks(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)

	// A hidden stale banner exists (a `.ro-banner.warn` that is `hidden` on first
	// paint) and carries the retry control + the "last known data" copy.
	banner := p.doc.Find(".ro-banner.warn.ro-stale-banner")
	if banner.Length() != 1 {
		t.Fatalf("expected exactly one hidden stale banner, found %d", banner.Length())
	}
	if _, hidden := banner.Attr("hidden"); !hidden {
		t.Fatalf("stale banner must be hidden on first paint (JS reveals it)")
	}
	if !strings.Contains(strings.ToLower(banner.Text()), "last known data") {
		t.Fatalf("stale banner copy = %q, want it to say last known data", banner.Text())
	}
	p.wantHas(".ro-stale-banner .ro-stale-retry")
	// The dim target the JS toggles exists.
	p.wantHas("#resource-list-content")
}

// TestStatesStaleHandlerInReadoutJS pins the readout.js side of the stale path:
// the refresh-error handlers (htmx:responseError / htmx:sendError) that mark the
// list stale, the afterSwap handler that clears it, the dim class, and the
// read-only retry trigger. There is no headless JS runner in this suite, so this
// asserts the source wires the exact hooks the rendered markup exposes.
func TestStatesStaleHandlerInReadoutJS(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "assets", "static", "readout.js"))
	if err != nil {
		t.Fatalf("read readout.js: %v", err)
	}
	js := string(src)
	for _, needle := range []string{
		"htmx:responseError",    // a non-2xx refresh reply -> stale
		"htmx:sendError",        // a transport failure on refresh -> stale
		"htmx:afterSwap",        // a recovered refresh clears stale
		"ro-stale",              // the dim class on #resource-list-content
		"ro-stale-banner",       // the hidden banner the handler reveals
		"ro-stale-retry",        // the read-only retry control
		"resource-list-content", // the dim target / refresh element id
		"ro:refresh",            // the read-only GET the retry re-fires
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
// a Get that 403s renders the `.ro-empty-lg` card (verb/resource/namespace + 403)
// at 200, with the breadcrumb chrome intact -- not the bare error panel.
func TestForbiddenDetailState(t *testing.T) {
	api := newStateFakeAPI(t, stateFakeOptions{forbidPods: true})
	app := newStateServer(t, api.URL)
	p := get(t, app, "/clusters/test/namespaces/default/pods/nginx", http.StatusOK)

	// The detail chrome (breadcrumb) is intact; the body is the forbidden state.
	p.wantHas(".ro-rd .ro-breadcrumb")
	title := p.text(".ro-rd .ro-empty-lg h3")
	// The detail verb is the distinct literal "get" (build_resource.go), NOT the
	// list verb "list" -- pin it so a wrong/empty detail verb is caught.
	for _, needle := range []string{"get", "pods", "default"} {
		if !strings.Contains(title, needle) {
			t.Fatalf("detail forbidden title %q missing %q", title, needle)
		}
	}
	hint := p.text(".ro-rd .ro-empty-lg .hint")
	if !strings.Contains(hint, "403") {
		t.Fatalf("detail forbidden hint %q missing 403", hint)
	}
	p.wantAttr(".ro-rd .ro-empty-lg .ro-empty-actions a", "href", "/clusters")
}

// TestUnreachableDetailState proves the detail page ships the unreachable state
// with the REAL transport error + Retry + Back to clusters, at 200.
func TestUnreachableDetailState(t *testing.T) {
	app := newStateServer(t, newDeadCluster(t))
	p := get(t, app, "/clusters/test/namespaces/default/pods/nginx", http.StatusOK)

	p.wantHas(".ro-rd .ro-breadcrumb")
	p.wantText(".ro-rd .ro-empty-lg h3", "Can't reach this cluster")
	hint := p.text(".ro-rd .ro-empty-lg .hint.mono")
	if !strings.Contains(strings.ToLower(hint), "connect") && !strings.Contains(strings.ToLower(hint), "refused") && !strings.Contains(strings.ToLower(hint), "dial") {
		t.Fatalf("detail unreachable hint %q does not look like a real transport error", hint)
	}
	actions := p.attrs(".ro-rd .ro-empty-lg .ro-empty-actions a", "href")
	if !contains(actions, "/clusters") {
		t.Fatalf("detail unreachable missing Back to clusters: %v", actions)
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
