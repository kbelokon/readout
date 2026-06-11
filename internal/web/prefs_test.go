package web

// prefs_test.go pins the D9 ro_prefs cookie contract: the v1.base64url wire
// envelope (round-trip incl. column names with spaces and the explicit-empty
// hide set), the 3KB tail eviction, the URL-beats-cookie precedence on sort,
// the history-restore bypass (sort un-filled, column visibility KEPT), the
// render-only fill (HX-Push-Url never carries a cookie-filled sort), the
// hidden-column SSR render with its config-default interplay, the
// namespace-per-cluster href-only mechanism (applied in cluster-entry links,
// ignored on direct URL loads), and the persisted-refresh topbar render. The
// JS writer half is pinned needle-style like the other readout.js contracts
// (no headless JS runner in this suite).

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// prefsGet drives one GET with a ro_prefs cookie (and optional headers)
// through the full handler chain, mirroring the shared get() helper.
func prefsGet(t *testing.T, app *Server, path, cookie string, headers map[string]string) *page {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: prefsCookieName, Value: cookie})
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200\nbody=%s", path, rec.Code, rec.Body.String())
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(rec.Body.String()))
	if err != nil {
		t.Fatalf("GET %s: parse HTML: %v", path, err)
	}
	return &page{t: t, path: path, rec: rec, doc: doc}
}

// podsSortNameCookie is the recurring fixture: a persisted Name sort for pods.
func podsSortNameCookie() string {
	return encodePrefs(prefs{Kinds: []kindPrefs{{Plural: "pods", Sort: "Name"}}})
}

// TestPrefsEnvelopeRoundTrip pins the wire format: `v1.<base64url(JSON)>`,
// cookie-safe octets only (the reason raw JSON is rejected -- column names like
// "Nominated Node" carry spaces, JSON carries quotes/commas), and a lossless
// round-trip of every schema field: a column name WITH A SPACE, a `:desc` sort,
// the stringly refresh mode, `_all` as a namespace value, and the explicit
// EMPTY hide set kept distinct from an absent one.
func TestPrefsEnvelopeRoundTrip(t *testing.T) {
	in := prefs{
		Kinds: []kindPrefs{
			{Plural: "pods", Sort: "Status:desc", Hide: &[]string{"Nominated Node", "Readiness Gates"}},
			{Plural: "deployments", Hide: &[]string{}}, // explicit "hide nothing"
			{Plural: "nodes", Sort: "Created"},         // no column preference at all
		},
		Refresh:    "30",
		Namespaces: map[string]string{"test": "_all", "prod": "kube-system"},
	}
	value := encodePrefs(in)
	if !strings.HasPrefix(value, "v1.") {
		t.Fatalf("encoded value = %q, want the v1. version prefix", value)
	}
	// Cookie-value safety: nothing outside the base64url alphabet (+ the v1.
	// tag). A space/quote/comma/semicolon here would be an RFC 6265 violation.
	if !regexp.MustCompile(`^v1\.[A-Za-z0-9_-]+$`).MatchString(value) {
		t.Fatalf("encoded value %q carries non-cookie-safe octets", value)
	}
	out, ok := decodePrefs(value)
	if !ok {
		t.Fatalf("decodePrefs rejected its own encoder output %q", value)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round-trip mismatch:\n in=%+v\nout=%+v", in, out)
	}
	// The space-bearing column name survived the envelope byte-exact.
	pods := out.kind("pods")
	if pods == nil || pods.Hide == nil || (*pods.Hide)[0] != "Nominated Node" {
		t.Fatalf("pods hide round-trip = %+v, want [Nominated Node Readiness Gates]", pods)
	}
	// Explicit-empty vs absent hide: the D8 user-override-wins rule needs them
	// distinguishable (empty suppresses the config default, absent falls to it).
	if d := out.kind("deployments"); d == nil || d.Hide == nil || len(*d.Hide) != 0 {
		t.Fatalf("explicit-empty hide decoded as %+v, want a non-nil empty list", out.kind("deployments"))
	}
	if n := out.kind("nodes"); n == nil || n.Hide != nil {
		t.Fatalf("absent hide decoded as %+v, want nil", out.kind("nodes"))
	}
	if out.Namespaces["test"] != "_all" {
		t.Fatalf("namespace _all round-trip = %q, want _all", out.Namespaces["test"])
	}
}

// TestPrefsDecodeLenient: a corrupt/foreign cookie yields zero prefs (never an
// error or panic), and a page render with a junk cookie stays a plain 200 --
// exactly as if no preferences existed.
func TestPrefsDecodeLenient(t *testing.T) {
	for _, value := range []string{
		"",
		"garbage",
		"v2.AAAA",             // foreign version tag
		"v1.!!!not-base64!!!", // broken base64
		"v1.bm90LWpzb24",      // valid base64url whose payload is not JSON
	} {
		if p, ok := decodePrefs(value); ok || len(p.Kinds) != 0 || p.Refresh != "" || p.Namespaces != nil {
			t.Fatalf("decodePrefs(%q) = (%+v, %v), want zero prefs and ok=false", value, p, ok)
		}
	}
	app := newServer(t, baseConfig(t), time.Now())
	p := prefsGet(t, app, "/clusters/test/namespaces/default/pods", "v1.!!!", nil)
	if got := p.texts("table.ro-table td.cell-name"); strings.Join(got, "|") != "nginx|my-app" {
		t.Fatalf("junk-cookie render rows = %v, want the plain fixture order", got)
	}
}

// TestPrefsEvictionDropsTailKinds pins the D9 eviction mechanics: above the
// 3KB encoded cap, kind entries drop from the array TAIL (the array is
// most-recent-first, so the least recently used kinds evict) while the head
// entries, the refresh mode, and the namespace map survive untouched.
func TestPrefsEvictionDropsTailKinds(t *testing.T) {
	in := prefs{Refresh: "5", Namespaces: map[string]string{"test": "default"}}
	for i := 0; i < 40; i++ {
		hide := []string{}
		for j := 0; j < 6; j++ {
			hide = append(hide, fmt.Sprintf("Some Long Column Name %02d-%d", i, j))
		}
		in.Kinds = append(in.Kinds, kindPrefs{
			Plural: fmt.Sprintf("kind-%02d", i),
			Sort:   "Name:desc",
			Hide:   &hide,
		})
	}
	value := encodePrefs(in)
	if len(value) > prefsMaxEncoded {
		t.Fatalf("encoded value is %d bytes, want <= %d (eviction did not run)", len(value), prefsMaxEncoded)
	}
	out, ok := decodePrefs(value)
	if !ok {
		t.Fatalf("decodePrefs rejected the evicted value")
	}
	if len(out.Kinds) == 0 || len(out.Kinds) >= len(in.Kinds) {
		t.Fatalf("evicted kinds = %d of %d, want a non-empty strict subset", len(out.Kinds), len(in.Kinds))
	}
	// Tail eviction ONLY: the surviving entries are exactly the original head.
	if !reflect.DeepEqual(out.Kinds, in.Kinds[:len(out.Kinds)]) {
		t.Fatalf("survivors are not the head prefix; eviction must drop from the tail only")
	}
	// MINIMALITY: keeping even one more kind must overflow the cap, or the loop
	// over-evicted (a head prefix passing the checks above could still be the
	// result of dropping half the entries). Marshal the would-be payload
	// directly -- never through encodePrefs, whose eviction loop is the very
	// code under test.
	oneMore := in
	oneMore.Kinds = in.Kinds[:len(out.Kinds)+1]
	raw, err := json.Marshal(&oneMore)
	if err != nil {
		t.Fatalf("marshal the one-more-kind payload: %v", err)
	}
	if got := len(prefsVersionPrefix + base64.RawURLEncoding.EncodeToString(raw)); got <= prefsMaxEncoded {
		t.Fatalf("over-eviction: %d kinds encode to %d bytes (<= %d cap), so dropping down to %d was not necessary",
			len(out.Kinds)+1, got, prefsMaxEncoded, len(out.Kinds))
	}
	if out.Refresh != "5" || out.Namespaces["test"] != "default" {
		t.Fatalf("refresh/namespaces lost in eviction: %+v", out)
	}
	// The caller's slice is never mutated by the eviction loop.
	if len(in.Kinds) != 40 {
		t.Fatalf("encodePrefs mutated the caller's kinds slice to %d entries", len(in.Kinds))
	}
}

// TestPrefsSortFillPrecedence pins the URL <-> cookie precedence table for
// sort: the cookie fills an ABSENT ?sort= at SSR (rows re-ordered, th.sorted +
// the asc icon rendered, the header href toggling to :desc), an explicit URL
// ?sort= beats the cookie outright, and multi-type pages (the D1 boundary) see
// no fill at all.
func TestPrefsSortFillPrecedence(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	cookie := podsSortNameCookie()

	// Cookie fills the absent param: Name-ascending render.
	p := prefsGet(t, app, "/clusters/test/namespaces/default/pods", cookie, nil)
	if got := p.texts("table.ro-table td.cell-name"); strings.Join(got, "|") != "my-app|nginx" {
		t.Fatalf("cookie-filled sort rows = %v, want [my-app nginx]", got)
	}
	p.wantText("th.sorted", "Name")
	p.wantHas("th.sorted .sort-ico.sort-asc") // ascending icon: the fill IS the effective sort
	// The Name header link toggles to :desc exactly as if ?sort=Name were in
	// the URL -- the user sees an ascending Name sort, so the next click flips.
	if !p.containsHref("thead th a", "/clusters/test/namespaces/default/pods?sort=Name%3Adesc") {
		t.Fatalf("Name header href did not toggle to :desc under the cookie fill: %v", p.attrs("thead th a", "href"))
	}

	// URL wins: an explicit ?sort=Name:desc reverses the order and renders the
	// descending icon even though the cookie says plain Name.
	p = prefsGet(t, app, "/clusters/test/namespaces/default/pods?sort=Name%3Adesc", cookie, nil)
	if got := p.texts("table.ro-table td.cell-name"); strings.Join(got, "|") != "nginx|my-app" {
		t.Fatalf("URL-sort rows = %v, want [nginx my-app] (URL beats cookie)", got)
	}
	p.wantAbsent("th.sorted .sort-ico.sort-asc") // descending icon has no sort-asc

	// Multi-type pages sit outside the loop (D1): no fill, fixture order, no
	// sorted header anywhere.
	p = prefsGet(t, app, "/clusters/test/namespaces/default/pods,services", cookie, nil)
	if got := p.texts("table.ro-table td.cell-name"); !strings.HasPrefix(strings.Join(got, "|"), "nginx|my-app") {
		t.Fatalf("multi-type rows = %v, want the unfilled fixture order", got)
	}
	p.wantAbsent("th.sorted")
}

// TestPrefsHistoryRestoreSkipsSortKeepsColumns pins the back-button rule
// (D9): a request carrying htmx's HX-History-Restore-Request header skips the
// cookie fill for URL-REPRESENTABLE state (sort -- the back button must not be
// defeated by a freshly written sort pref) while column visibility, which has
// NO URL form, stays filled -- stripping it would make a back-render differ
// from a hard reload of the same URL.
func TestPrefsHistoryRestoreSkipsSortKeepsColumns(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	hide := []string{"Restarts"}
	cookie := encodePrefs(prefs{Kinds: []kindPrefs{{Plural: "pods", Sort: "Name", Hide: &hide}}})

	// Control: a plain load fills BOTH (sorted + Restarts hidden).
	p := prefsGet(t, app, "/clusters/test/namespaces/default/pods", cookie, nil)
	if got := p.texts("table.ro-table td.cell-name"); strings.Join(got, "|") != "my-app|nginx" {
		t.Fatalf("plain-load rows = %v, want the cookie sort applied", got)
	}
	if cols := strings.Join(p.texts("thead th"), "|"); strings.Contains(cols, "Restarts") {
		t.Fatalf("plain-load columns = %q, want Restarts hidden", cols)
	}

	// History restore: sort fill OFF (URL truth -- fixture order, no sorted
	// header), column fill STILL ON.
	p = prefsGet(t, app, "/clusters/test/namespaces/default/pods", cookie,
		map[string]string{"HX-Request": "true", "HX-History-Restore-Request": "true"})
	if got := p.texts("table.ro-table td.cell-name"); strings.Join(got, "|") != "nginx|my-app" {
		t.Fatalf("history-restore rows = %v, want the UN-sorted URL truth", got)
	}
	p.wantAbsent("th.sorted")
	if cols := strings.Join(p.texts("thead th"), "|"); strings.Contains(cols, "Restarts") {
		t.Fatalf("history-restore columns = %q, want Restarts STILL hidden (colvis has no URL form)", cols)
	}
}

// TestPrefsHiddenColumnsRender pins the column-visibility consumption: the
// cookie's hide list renders hidden on the full page AND the `_table` partial;
// an explicit URL ?hidecols= wins outright (no merging); an explicit EMPTY
// cookie hide set suppresses the DefaultHiddenColumns config default (user
// override wins, D8) while an absent one falls back to it.
func TestPrefsHiddenColumnsRender(t *testing.T) {
	cfg := baseConfig(t)
	cfg.DefaultHiddenColumns = map[string]string{"pods": "Status"}
	app := newServer(t, cfg, time.Now())
	hide := []string{"Restarts"}
	cookie := encodePrefs(prefs{Kinds: []kindPrefs{{Plural: "pods", Hide: &hide}}})

	// No cookie: the config default hides Status.
	p := prefsGet(t, app, "/clusters/test/namespaces/default/pods", "", nil)
	if cols := strings.Join(p.texts("thead th"), "|"); strings.Contains(cols, "Status") || !strings.Contains(cols, "Restarts") {
		t.Fatalf("config-default columns = %q, want Status hidden + Restarts shown", cols)
	}

	// Cookie hide list: REPLACES the config default (Restarts hidden, Status
	// back) on the full page and on the partial fragment alike.
	for _, path := range []string{
		"/clusters/test/namespaces/default/pods",
		"/clusters/test/namespaces/default/pods/_table",
	} {
		p = prefsGet(t, app, path, cookie, nil)
		if cols := strings.Join(p.texts("thead th"), "|"); strings.Contains(cols, "Restarts") || !strings.Contains(cols, "Status") {
			t.Fatalf("GET %s columns = %q, want Restarts hidden + Status shown (cookie beats config)", path, cols)
		}
	}

	// URL param wins, NOT merged: ?hidecols=Status hides Status only; the
	// cookie's Restarts hide is ignored while the URL speaks.
	p = prefsGet(t, app, "/clusters/test/namespaces/default/pods?hidecols=Status", cookie, nil)
	if cols := strings.Join(p.texts("thead th"), "|"); strings.Contains(cols, "Status") || !strings.Contains(cols, "Restarts") {
		t.Fatalf("URL-hidecols columns = %q, want Status hidden + Restarts shown (URL beats cookie, no merge)", cols)
	}

	// Explicit empty hide set: the user toggled everything visible -- the
	// config default must NOT resurface.
	empty := []string{}
	p = prefsGet(t, app, "/clusters/test/namespaces/default/pods",
		encodePrefs(prefs{Kinds: []kindPrefs{{Plural: "pods", Hide: &empty}}}), nil)
	if cols := strings.Join(p.texts("thead th"), "|"); !strings.Contains(cols, "Status") || !strings.Contains(cols, "Restarts") {
		t.Fatalf("explicit-empty columns = %q, want BOTH Status and Restarts shown", cols)
	}
}

// TestPrefsPushURLExcludesCookieSort pins the render-only fill decision: a
// cookie-filled sort orders the rows and lights th.sorted, but the canonical
// HX-Push-Url (and thus the address bar/history) carries ONLY what the user
// explicitly chose -- a pushed URL is user-truth, and the cookie re-fills it
// identically on any later load. An explicit URL sort keeps riding the push
// unchanged.
func TestPrefsPushURLExcludesCookieSort(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	cookie := podsSortNameCookie()

	p := prefsGet(t, app, "/clusters/test/namespaces/default/pods/_table", cookie,
		map[string]string{"HX-Request": "true"})
	if got := p.rec.Header().Get("HX-Push-Url"); got != "/clusters/test/namespaces/default/pods" {
		t.Fatalf("push URL = %q, want the bare canonical URL (no cookie-filled sort)", got)
	}
	// ...while the fragment itself IS sorted by the fill.
	p.wantText("th.sorted", "Name")
	if got := p.texts("td.cell-name"); strings.Join(got, "|") != "my-app|nginx" {
		t.Fatalf("fragment rows = %v, want the cookie sort applied", got)
	}

	// An explicit URL sort still pushes verbatim.
	p = prefsGet(t, app, "/clusters/test/namespaces/default/pods/_table?sort=Status", cookie,
		map[string]string{"HX-Request": "true"})
	if got := p.rec.Header().Get("HX-Push-Url"); got != "/clusters/test/namespaces/default/pods?sort=Status" {
		t.Fatalf("explicit-sort push URL = %q, want it carried through", got)
	}
}

// TestPrefsNamespaceClusterEntryHrefs pins the namespace-per-cluster CONSUMER
// surfaces (D9, href-only): the clusters page's row link and the palette's
// topbar cluster nav both point into the persisted namespace's pods list
// (`_all` included); without a pref both keep the plain cluster-overview link.
func TestPrefsNamespaceClusterEntryHrefs(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())

	// No pref: the plain overview link.
	p := prefsGet(t, app, "/clusters", "", nil)
	p.wantAttr("td.cl-name a", "href", "/clusters/test")

	// Persisted namespace: the row link enters the cluster AT that namespace.
	cookie := encodePrefs(prefs{Namespaces: map[string]string{"test": "states"}})
	p = prefsGet(t, app, "/clusters", cookie, nil)
	p.wantAttr("td.cl-name a", "href", "/clusters/test/namespaces/states/pods")

	// The palette cluster jump (the topbar's cluster nav) carries the same href.
	var feed paletteFeedJSON
	if err := json.Unmarshal([]byte(p.doc.Find("#ro-palette-data").Text()), &feed); err != nil {
		t.Fatalf("parse palette blob: %v", err)
	}
	if len(feed.Clusters) != 1 || feed.Clusters[0].Href != "/clusters/test/namespaces/states/pods" {
		t.Fatalf("palette cluster hrefs = %+v, want the persisted-namespace entry link", feed.Clusters)
	}

	// `_all` is a persistable value and builds the all-namespaces list link.
	p = prefsGet(t, app, "/clusters", encodePrefs(prefs{Namespaces: map[string]string{"test": "_all"}}), nil)
	p.wantAttr("td.cl-name a", "href", "/clusters/test/namespaces/_all/pods")
}

// TestPrefsNamespaceIgnoredOnDirectLoads pins the other half of the href-only
// mechanism: a persisted namespace NEVER alters a direct URL load -- no
// redirect and no scope injection. A ns-less cluster-scoped list renders
// normally (cluster-scoped kinds unaffected), and an explicit namespace in the
// URL keeps rendering THAT namespace.
func TestPrefsNamespaceIgnoredOnDirectLoads(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	cookie := encodePrefs(prefs{Namespaces: map[string]string{"test": "states"}})

	// Direct ns-less list URL: 200 (prefsGet fails on any redirect status),
	// table rendered, untouched by the pref.
	p := prefsGet(t, app, "/clusters/test/nodes", cookie, nil)
	p.wantHas("table.ro-table")
	if got := p.rec.Header().Get("Location"); got != "" {
		t.Fatalf("ns-less load answered with a Location header %q; the pref must never redirect", got)
	}

	// Direct namespace-scoped URL: the URL's namespace renders, not the
	// persisted one (default's pods, not the states fixtures).
	p = prefsGet(t, app, "/clusters/test/namespaces/default/pods", cookie, nil)
	if got := strings.Join(p.texts("table.ro-table td.cell-name"), "|"); got != "nginx|my-app" {
		t.Fatalf("explicit-namespace rows = %q, want default's pods (URL truth)", got)
	}
}

// TestPrefsRefreshModeRendered pins the SSR half of the refresh persistence:
// the topbar renders the persisted mode (label text, active interval option,
// the refresh-on styling hook) so the choice paints without the JS sync flash
// -- readout.js re-derives the identical state from the same cookie.
func TestPrefsRefreshModeRendered(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())

	// No pref: Off, exactly the markup the JS sync would produce.
	p := prefsGet(t, app, "/clusters", "", nil)
	p.wantText("#refresh-label", "Off")
	p.wantAttr("#refresh-dropdown", "class", "refresh-dropdown")
	p.wantAttr(`.refresh-option[data-interval="0"]`, "class", "refresh-option is-active")

	// Persisted interval: label + active option + the refresh-on hook.
	p = prefsGet(t, app, "/clusters", encodePrefs(prefs{Refresh: "30"}), nil)
	p.wantText("#refresh-label", "30s")
	p.wantAttr("#refresh-dropdown", "class", "refresh-dropdown refresh-on")
	p.wantAttr(`.refresh-option[data-interval="30"]`, "class", "refresh-option is-active")
	p.wantAttr(`.refresh-option[data-interval="0"]`, "class", "refresh-option")

	// Persisted Off: an explicit choice renders like the default (and the
	// legacy "0" never reaches the cookie -- the JS writes "Off").
	p = prefsGet(t, app, "/clusters", encodePrefs(prefs{Refresh: "Off"}), nil)
	p.wantText("#refresh-label", "Off")
	p.wantAttr("#refresh-dropdown", "class", "refresh-dropdown")
	p.wantAttr(`.refresh-option[data-interval="0"]`, "class", "refresh-option is-active")

	// Persisted Live (Unit 27/D19): the label says Live, the Live option is the
	// active one (NOT Off, even though Live arms no polling interval), and the
	// refresh-on hook keeps the livedot pulsing at SSR.
	p = prefsGet(t, app, "/clusters", encodePrefs(prefs{Refresh: "Live"}), nil)
	p.wantText("#refresh-label", "Live")
	p.wantAttr("#refresh-dropdown", "class", "refresh-dropdown refresh-on")
	p.wantAttr(`.refresh-option[data-interval="Live"]`, "class", "refresh-option is-active")
	p.wantAttr(`.refresh-option[data-interval="0"]`, "class", "refresh-option")
}

// TestLiveOptionScopeGate pins the server-rendered Live availability (Unit 27/
// D19 scope cut): the dropdown's Live option is DISABLED (with an explanatory
// title) on multi-type and multi-cluster list pages -- the `_stream` endpoint
// 404s that scope -- and enabled on single-type single-cluster lists. Non-list
// pages (detail) keep it enabled-but-inert, like the interval options.
func TestLiveOptionScopeGate(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	live := `.refresh-option[data-interval="Live"]`

	// Single-type, single-cluster list: enabled.
	p := prefsGet(t, app, "/clusters/test/namespaces/default/pods", "", nil)
	p.wantHas(live)
	p.wantAbsent(live + "[disabled]")

	// Multi-type list (plural "all"): disabled with the scope title.
	p = prefsGet(t, app, "/clusters/test/namespaces/default/all", "", nil)
	p.wantHas(live + "[disabled]")
	if title := p.attr(live, "title"); !strings.Contains(title, "single-type") {
		t.Fatalf("disabled Live option title = %q, want a single-type scope explanation", title)
	}

	// CSV multi-type list: disabled too.
	p = prefsGet(t, app, "/clusters/test/namespaces/default/pods,services", "", nil)
	p.wantHas(live + "[disabled]")

	// Multi-cluster list (_all union): disabled.
	p = prefsGet(t, app, "/clusters/_all/pods", "", nil)
	p.wantHas(live + "[disabled]")

	// A detail page is not a list: the option stays enabled (inert client-side,
	// exactly like picking an interval there).
	p = prefsGet(t, app, "/clusters/test/namespaces/default/pods/nginx", "", nil)
	p.wantAbsent(live + "[disabled]")
}

// TestPrefsReadoutJSContract pins the JS writer half needle-style (the suite
// has no JS runtime; the e2e layer exercises the live behavior): the cookie
// name/envelope/cap/attribute constants, the four user-interaction write
// surfaces (sort click, column toggle for Unit 9, interval pick, namespace
// switch), the programmatic do-not-write guards, and the roRefresh migration
// (read-once fallback only -- the legacy localStorage WRITE is retired).
func TestPrefsReadoutJSContract(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "assets", "static", "readout.js"))
	if err != nil {
		t.Fatalf("read readout.js: %v", err)
	}
	js := string(src)
	for _, needle := range []string{
		"'ro_prefs'",                            // the cookie name
		"'v1.'",                                 // the pinned version prefix
		"PREFS_MAX_ENCODED = 3072",              // the eviction cap
		"Path=/; SameSite=Lax; Max-Age=",        // the pinned attributes
		"PREFS_COOKIE_MAX_AGE = 31536000",       // one-year Max-Age
		"window.location.protocol === 'https:'", // Secure on https only
		"'; Secure'",
		"roPrefsSetSort",                      // sort-click write
		"roPrefsSetHiddenColumns",             // Unit 9's column-toggle surface
		"roPrefsSetRefresh",                   // interval pick (+ Unit 27 Live)
		"roPrefsSetNamespace",                 // namespace switch
		"closest('thead th')",                 // sort writes ONLY from header gestures
		"#namespace-dropdown .namespace-item", // the namespace-switch surface
		"localStorage.getItem(REFRESH_KEY)",   // the read-once roRefresh migration
		"refreshMode",                         // cookie-canonical mode reader
		// readPrefs drops wrongly-typed INNER fields instead of perpetuating
		// them: Go's decodePrefs rejects the whole payload on one mistyped
		// field (json.Unmarshal is all-or-nothing), so a passthrough JS reader
		// would keep rewriting a cookie SSR can never apply. Field-level type
		// guards make the next JS write self-heal the cookie.
		"typeof e.sort === 'string'",              // kind sort kept only as a string
		"Array.isArray(e.hide) && e.hide.every",   // hide kept only as an all-string array
		"typeof decoded.ns[cluster] === 'string'", // ns map rebuilt from string values only
	} {
		if !strings.Contains(js, needle) {
			t.Fatalf("readout.js prefs contract missing %q", needle)
		}
	}
	// The sort-write hook treats programmatic traffic as do-not-write: the
	// RO-No-Push marker and preload warm-ups are both guarded in the same
	// beforeRequest hook that discriminates on the thead ancestor.
	start := strings.Index(js, "Sort-click pref write")
	if start < 0 {
		t.Fatalf("readout.js lost the sort-write hook section marker")
	}
	// The closing marker is the NEXT section header after the hook ("Auto-
	// refresh interval" also names an earlier click-handler comment, so the
	// search must begin at the hook).
	length := strings.Index(js[start:], "Auto-refresh interval")
	if length < 0 {
		t.Fatalf("readout.js lost the section header after the sort-write hook")
	}
	hook := js[start : start+length]
	for _, guard := range []string{"RO-No-Push", "HX-Preloaded"} {
		if !strings.Contains(hook, guard) {
			t.Fatalf("sort-write hook lost its %q do-not-write guard", guard)
		}
	}
	// The legacy roRefresh localStorage WRITE is gone: the cookie is canonical
	// (the key survives only as refreshMode()'s migration read).
	if strings.Contains(js, "localStorage.setItem(REFRESH_KEY") {
		t.Fatalf("readout.js still WRITES the legacy roRefresh localStorage key; the ro_prefs cookie is canonical")
	}
}
