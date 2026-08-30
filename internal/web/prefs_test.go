package web

// prefs_test.go pins the ro_prefs cookie contract: the v1.base64url wire
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
		Refresh: "30",
		Namespaces: map[string]string{
			"test":           "_all",
			"prod":           "kube-system",
			"__proto__":      "proto-ns",
			"constructor":    "constructor-ns",
			"prototype":      "prototype-ns",
			"toString":       "string-ns",
			"hasOwnProperty": "own-ns",
		},
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
	// Explicit-empty vs absent hide: the user-override-wins rule needs them
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
	for cluster, namespace := range in.Namespaces {
		if out.Namespaces[cluster] != namespace {
			t.Fatalf("special namespace key %q round-trip = %q, want %q", cluster, out.Namespaces[cluster], namespace)
		}
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

// TestPrefsRawURLNewlineParity pins the non-obvious RawURLEncoding rule shared
// with the browser decoder: CR and LF are ignored anywhere in the payload, but
// other ASCII whitespace is not.
func TestPrefsRawURLNewlineParity(t *testing.T) {
	for _, value := range []string{"v1.\r\ne30", "v1.e\r\n30", "v1.e\n3\r0\n"} {
		p, ok := decodePrefs(value)
		if !ok || len(p.Kinds) != 0 || p.Refresh != "" || len(p.Namespaces) != 0 {
			t.Fatalf("decodePrefs(%q) = (%+v, %v), want accepted empty prefs", value, p, ok)
		}
	}
	if p, ok := decodePrefs("v1.e\t30"); ok || len(p.Kinds) != 0 || p.Refresh != "" || len(p.Namespaces) != 0 {
		t.Fatalf("decodePrefs with tab = (%+v, %v), want zero prefs and ok=false", p, ok)
	}
}

// TestPrefsEvictionDropsTailKinds pins the cookie eviction mechanics: above the
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
// ?sort= beats the cookie outright, and multi-type pages (outside the single-type loop) see
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

	// Multi-type pages sit outside the single-type loop: no fill, fixture order, no
	// sorted header anywhere.
	p = prefsGet(t, app, "/clusters/test/namespaces/default/pods,services", cookie, nil)
	if got := p.texts("table.ro-table td.cell-name"); !strings.HasPrefix(strings.Join(got, "|"), "nginx|my-app") {
		t.Fatalf("multi-type rows = %v, want the unfilled fixture order", got)
	}
	p.wantAbsent("th.sorted")
}

// TestPrefsHistoryRestoreSkipsSortKeepsColumns pins the back-button rule
// for the prefs cookie: a request carrying htmx's HX-History-Restore-Request header skips the
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
// override wins) while an absent one falls back to it.
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
// surfaces (the persisted-namespace cookie, href-only): the clusters page's row link and the palette's
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

// TestPrefsLiveToggleRendered pins the SSR half of the Live persistence: the
// topbar renders the persisted choice as the toggle's aria-pressed so it paints
// without the JS sync flash -- readout.js re-derives the identical state from
// the same cookie. The vocabulary is exactly two values: only the literal
// "Live" presses the toggle, and everything else -- "Off", a legacy polling
// interval written by an older build, junk -- renders off. That is the whole
// migration: a stale numeric value can never re-arm a poll loop that no longer
// exists.
func TestPrefsLiveToggleRendered(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	const toggle = `[data-ro-action="toggle-live"]`
	// A watchable single-type list is the scope that renders the toggle at all.
	const listPath = "/clusters/test/namespaces/default/pods"

	// No pref: off, exactly the markup the JS sync would produce.
	p := prefsGet(t, app, listPath, "", nil)
	p.wantAttr(toggle, "aria-pressed", "false")

	// Persisted Live: pressed.
	p = prefsGet(t, app, listPath, encodePrefs(prefs{Refresh: "Live"}), nil)
	p.wantAttr(toggle, "aria-pressed", "true")

	// Persisted Off: an explicit choice renders like the default.
	p = prefsGet(t, app, listPath, encodePrefs(prefs{Refresh: "Off"}), nil)
	p.wantAttr(toggle, "aria-pressed", "false")

	// A legacy interval and outright junk both render off -- never pressed, and
	// never a third state.
	for _, stale := range []string{"30", "0", "5", "live", "LIVE", "Live "} {
		p = prefsGet(t, app, listPath, encodePrefs(prefs{Refresh: stale}), nil)
		if got := p.attr(toggle, "aria-pressed"); got != "false" {
			t.Fatalf("stored refresh %q rendered aria-pressed=%q, want false", stale, got)
		}
	}
}

// TestLiveToggleScopeGate pins the server-rendered Live availability. The
// toggle renders ONLY where `_stream` answers: one type, one cluster, a list
// page (not a detail page), and a kind whose discovery verbs include `watch`.
// Everywhere else the page shows Refresh alone -- an offered toggle is a
// promise the endpoint keeps, so an unsupported page must not offer one.
func TestLiveToggleScopeGate(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	const toggle = `[data-ro-action="toggle-live"]`
	const refresh = `[data-ro-action="refresh-now"]`

	// Single-type, single-cluster list of a watchable kind: offered.
	p := prefsGet(t, app, "/clusters/test/namespaces/default/pods", "", nil)
	p.wantHas(toggle)
	p.wantHas(refresh)

	// Every scope `_stream` refuses renders Refresh and no toggle.
	for _, tc := range []struct {
		name string
		path string
	}{
		{"multi-type plural", "/clusters/test/namespaces/default/all"},
		{"CSV multi-type", "/clusters/test/namespaces/default/pods,services"},
		{"multi-cluster union", "/clusters/_all/pods"},
		{"detail page", "/clusters/test/namespaces/default/pods/nginx"},
		{"cluster-less page", "/clusters"},
		// A kind without the watch verb (the metrics pseudo-type, which
		// `_stream` answers with 204). The toolbar must not offer Live for it.
		{"watch-less kind", "/clusters/test/namespaces/default/pods?apiVersion=metrics.k8s.io/v1beta1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := prefsGet(t, app, tc.path, "", nil)
			p.wantAbsent(toggle)
			p.wantHas(refresh)
		})
	}

	// The watch-less case must fail for the RIGHT reason: the page really is a
	// single-type single-cluster list (so only the missing verb rules it out),
	// and its rows rendered.
	p = prefsGet(t, app, "/clusters/test/namespaces/default/pods?apiVersion=metrics.k8s.io/v1beta1", "", nil)
	if rows := p.doc.Find("table.ro-table tbody tr").Length(); rows == 0 {
		t.Fatalf("metrics pseudo-type list rendered no rows; the watch-less gate would pass vacuously")
	}
}

// TestPrefsReadoutJSContract pins the JS writer half needle-style (the suite
// has no JS runtime; the e2e layer exercises the live behavior): the cookie
// name/envelope/cap/attribute constants, the four user-interaction write
// surfaces (sort click, column-visibility toggle, interval pick, namespace
// switch), the programmatic do-not-write guards, and the roRefresh migration
// (read-once fallback only -- the legacy localStorage WRITE is retired).
func TestPrefsReadoutJSContract(t *testing.T) {
	js := readoutJS(t)
	for _, needle := range []string{
		"'ro_prefs='",                           // the cookie name and separator
		"'v1.'",                                 // the pinned version prefix
		"PREFS_MAX_ENCODED = 3072",              // the eviction cap
		"Path=/; SameSite=Lax; Max-Age=",        // the pinned attributes
		"PREFS_COOKIE_MAX_AGE = 31536e3",        // one-year Max-Age (esbuild emits the shortest numeric form)
		"window.location.protocol === 'https:'", // Secure on https only
		"'; Secure'",
		"roPrefsSetSort",          // sort-click write
		"roPrefsSetHiddenColumns", // the column-visibility toggle surface
		"roPrefsSetRefresh",       // interval pick (+ Live mode)
		"roPrefsSetNamespace",     // namespace switch
		"closest('thead th')",     // sort writes ONLY from header gestures
		"#namespace-dropdown [data-ro-action='pick-namespace']", // the namespace-switch surface
		"localStorage.getItem('roRefresh')",                     // the read-once roRefresh migration
		"refreshMode",                                           // cookie-canonical mode reader
	} {
		if !strings.Contains(js, needle) {
			t.Fatalf("readout.js prefs contract missing %q", needle)
		}
	}
	// Decoder normalization is behavior-tested in prefs.test.ts against the
	// shared Go/TypeScript golden fixtures. Do not pin its emitted JavaScript
	// spelling here: equivalent iteration or narrowing must remain a safe refactor.
	// The sort-write hook treats RO-No-Push programmatic traffic as do-not-write
	// before it discriminates on the thead ancestor. Hover preload no longer
	// exists, so its retired request header is absent from this contract.
	if !strings.Contains(js, "if (cfg.headers['RO-No-Push'])") {
		t.Fatalf("sort-write hook lost its RO-No-Push do-not-write guard")
	}
	if !strings.Contains(js, "roPrefsSetSort(plural, sort)") {
		t.Fatalf("sort-write hook lost its roPrefsSetSort write")
	}
	// The legacy roRefresh localStorage WRITE is gone: the cookie is canonical
	// (the key survives only as refreshMode()'s migration read).
	if strings.Contains(js, "localStorage.setItem('roRefresh'") {
		t.Fatalf("readout.js still WRITES the legacy roRefresh localStorage key; the ro_prefs cookie is canonical")
	}
}
