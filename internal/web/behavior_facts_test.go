package web

// Hermetic behavior-fact net for the rendered pages.
//
// This file pins the STRUCTURAL + BEHAVIORAL contract of every page the server
// renders, via named goquery assertions (exact selector + value) over the
// `newServerFakeAPI` httptest fixtures. It is the rendering-correctness net: it
// must stay green on the current code, and any change to a rendered fact -- a
// cell value, a `?sort=` href, an age-bucket cell class, an `hx-*` wire, the
// palette `<template>`, the secret barrier -- has to break a named assertion
// here.
//
// Scope notes:
//   - These facts certify what the server emits today, read off its own output.
//   - They cover the route+query matrix, cell VALUES (not just headers), sort
//     hrefs, per-bucket age cell classes, and the JS-contract attributes by
//     exact selector.
//   - The JS files are exercised only through their contract: the ids/data-*/
//     hx-* on #resource-list-content, the palette <template>, and the toggle/
//     dropdown/unselect hooks are guarded by these named facts.
//   - Search is pinned with its full rich body (the type checkboxes, the result
//     cards, the per-cluster error articles, the count footer).

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kbelokon/readout/internal/config"
	"github.com/kbelokon/readout/internal/kube"
)

// rawPassword/rawToken are the base64 secret bytes baked into the secret
// fixtures (render_secret.json / secrets_list.json). They must NEVER reach the
// browser: the secret-barrier facts assert they appear nowhere in masked
// detail / YAML / download / custom-column output.
const (
	rawPassword = "c3VwZXItc2VjcmV0LXZhbHVl" // base64("super-secret-value")
	rawToken    = "dG9rZW4="                 // base64("token")
)

// ---------------------------------------------------------------------------
// App chrome: the base layout + the JS-contract surfaces that ride on EVERY
// page (navbar data-targets, refresh dropdown, theme toggle, namespace dropdown
// filter, the command-palette <template>). These guard readout.js's delegated
// selectors.
// ---------------------------------------------------------------------------

func TestBehaviorAppChromeAndJSContract(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Date(2024, 1, 3, 6, 0, 0, 0, time.UTC))
	// A namespaced list page exercises the fullest chrome: the namespace
	// dropdown is populated and the sidebar carries resource-type menu items.
	p := get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)

	// Title contract.
	p.wantText("title", "pods in test - readout")

	// Strict CSP head wiring: htmx-config opts out of eval/script tags.
	p.wantAttr(`meta[name="htmx-config"]`, "content", `{"allowEval": false, "allowScriptTags": false, "includeIndicatorStyles": false}`)

	// hx-boost body shell + preload ext. The redesign shell offsets content under
	// the sticky topbar via body.has-ro-topbar (D13 chrome migration).
	p.wantAttr("body", "hx-boost", "true")
	p.wantAttr("body", "hx-ext", "preload")
	p.wantHas("body.has-ro-topbar")

	// Redesign chrome scoping (D13): the topbar is a <header class="ro-topbar">
	// (NOT a <nav>) so the header.ro-topbar CSS applies, and the sidebar + main
	// ride inside the .ro-shell grid. The #aside-menu hook is kept (readout.js
	// mobile reveal). The mobile hamburger BUTTON is a tracked gap owned by Unit 15
	// (inert until then), so the old navbar-burger/aside-burger toggles are gone.
	p.wantHas("header.ro-topbar")
	p.wantAbsent("nav.navbar")
	p.wantHas(".ro-shell aside.ro-sidebar #aside-menu")
	p.wantHas(".ro-shell main.ro-main")
	// The tools-table toggle is CONTENT (the resource-list tools form), unchanged
	// by the shell migration; it still names its target via data-target.
	p.wantAttr("a.toggle-tools", "data-target", "tools-table-1")
	p.wantHas("#tools-table-1")

	// Auto-refresh dropdown: each option carries data-interval; the handler
	// stores that value and re-arms the poll.
	if got := p.attrs("#refresh-dropdown .refresh-option", "data-interval"); strings.Join(got, ",") != "0,5,15,30,60" {
		t.Fatalf("refresh-option data-interval set = %v, want [0 5 15 30 60]", got)
	}
	p.wantHas("#refresh-label")

	// Theme toggle: cookieless render is data-theme-explicit="false" (readout.js
	// then derives the POST target from prefers-color-scheme). The toggle lives
	// in a POST form that opts OUT of hx-boost (a real navigation write).
	p.wantAttr("#btn-theme-toggle", "data-theme-explicit", "false")
	p.wantAttr(`form[action="/preferences"][method="post"]`, "hx-boost", "false")

	// Namespace dropdown + searchbox filter: readout.js filters .namespace-item
	// by the #namespace-searchbox value.
	p.wantHas("#namespace-dropdown")
	p.wantHas("#namespace-searchbox")
	if got := p.attrs(".namespace-item", "href"); len(got) != 3 {
		t.Fatalf("expected 3 .namespace-item links, got %d: %v", len(got), got)
	}
	p.wantAttr(".namespace-item", "href", "/clusters/test/namespaces/default/pods")

	// An explicit theme choice flips data-theme-explicit to true and pins a
	// data-theme on <html>. Note theme() renders the NEXT-toggle value, so an
	// explicit ?theme=light yields data-theme="dark" today -- pin that exact
	// current behaviour (the fact is "explicit choice => html carries data-theme").
	pExplicit := get(t, app, "/clusters/test/namespaces/default/pods?theme=light", http.StatusOK)
	pExplicit.wantAttr("#btn-theme-toggle", "data-theme-explicit", "true")
	pExplicit.wantAttr("html", "data-theme", "dark")
}

// TestPaletteRendersGroupedDataDriven pins the redesign ⌘K palette overlay
// (Unit 4, D10): the server emits a STATIC overlay shell -- the rows are built
// client-side by readout.js from the #ro-palette-data JSON blob (no <template>,
// no DOM harvest). The overlay ROOT carries BOTH `ro-rd` AND
// `ro-palette-backdrop` so the redesign palette container CSS (gated by `.ro-rd`
// on the backdrop root, since the overlay lives outside the `.ro-rd` content
// subtree) applies. The `.ro-pal-*` search/list/foot vocabulary is the JS +
// base.css contract; the retired old `.ro-palette-panel/-row` + the
// `<template id="ro-palette-row-tmpl">` MUST be gone.
func TestPaletteRendersGroupedDataDriven(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	// The palette ships on every page; assert it on the clusters page so the
	// structural fact does not depend on any list data.
	p := get(t, app, "/clusters", http.StatusOK)

	// Outer overlay: a dialog whose ROOT carries the D13 redesign marker AND the
	// canonical backdrop class on the SAME element (so `.ro-rd.ro-palette-backdrop`
	// matches). readout.js toggles the `open` class to reveal it (no is-hidden).
	p.wantHas("#ro-palette.ro-rd.ro-palette-backdrop")
	p.wantAttr("#ro-palette", "role", "dialog")
	p.wantAttr("#ro-palette", "aria-hidden", "true")

	// The inner panel + the grouped data-driven palette vocabulary.
	p.wantHas("#ro-palette .ro-palette")
	p.wantHas("#ro-palette .ro-palette .ro-pal-search")
	p.wantHas("#ro-palette .ro-pal-search .ico svg") // the search glyph
	p.wantHas("#ro-palette-scope.ro-pal-scope")      // scope chip JS fills from the blob
	p.wantHas("#ro-palette .ro-pal-foot")            // keyboard-hint footer

	// Query box + list ids the input/keydown handlers resolve. The list is the
	// EMPTY container readout.js writes the grouped rows into at open time.
	p.wantAttr("#ro-palette-input", "role", "combobox")
	p.wantAttr("#ro-palette-list", "role", "listbox")
	// Structural emptiness: the list ships with NO children at all (JS fills it
	// at open). Asserting any descendant (not just .ro-pal-item) catches a
	// regression that server-renders rows with any element/class.
	if kids := p.count("#ro-palette-list *"); kids != 0 {
		t.Fatalf("server-rendered palette list should ship empty (JS fills it), got %d descendants", kids)
	}

	// The retired markup is GONE: no old panel/row classes and, crucially, no
	// <template> (the data-driven palette builds rows from the JSON blob, never a
	// cloned template).
	p.wantAbsent(".ro-palette-panel")
	p.wantAbsent(".ro-palette-row")
	p.wantAbsent(".ro-palette-list") // OLD <ul class="ro-palette-list">; new list is .ro-pal-list
	p.wantAbsent("template#ro-palette-row-tmpl")
	p.wantAbsent("[data-palette-close]")

	// The #ro-palette-data blob the JS reads is present, a non-<script> element
	// (htmx allowScriptTags:false strips <script> on swap), and a GROUPED shape
	// (clusters / namespaces / kinds / actions) -- the contract the palette builds
	// its groups from. (TestLayoutPaletteDataBlob asserts the field values; here we
	// certify the rendered overlay is wired to a valid grouped blob.)
	blob := p.doc.Find(`#ro-palette-data`)
	if blob.Length() != 1 {
		t.Fatalf("expected exactly one #ro-palette-data element, got %d", blob.Length())
	}
	if blob.Is("script") {
		t.Fatalf("#ro-palette-data must NOT be a <script>: htmx strips it on swap, emptying the palette after an hx-boost nav")
	}
	var data paletteFeedJSON
	if err := json.Unmarshal([]byte(blob.Text()), &data); err != nil {
		t.Fatalf("parse #ro-palette-data JSON: %v\nblob=%s", err, blob.Text())
	}
	// Grouped shape: the clusters group is always populated (the registry is
	// request-independent), and the actions group always carries the "All clusters"
	// jump -- so the palette has at least two non-empty groups to render even on the
	// cluster-less entry page.
	if len(data.Clusters) == 0 {
		t.Fatalf("palette blob clusters group is empty; want the registry clusters")
	}
	if len(data.Actions) == 0 {
		t.Fatalf("palette blob actions group is empty; want at least the All-clusters jump")
	}
}

// ---------------------------------------------------------------------------
// Clusters list: the cluster rows (the ro-cell-name cells), the search-select
// checkboxes with data-toggle-button, and the disabled search button they gate.
// ---------------------------------------------------------------------------

func TestBehaviorClustersPage(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters", http.StatusOK)

	p.wantText("title", "Clusters - readout")
	// Redesign entry page (D4/D13): the content root carries the .ro-rd marker and
	// the title is the .ro-title-row (.ro-title + .ro-count), not the legacy h1.title.
	p.wantHas(".ro-rd")
	p.wantText(".ro-title-row .ro-title", "Clusters")
	p.wantText(".ro-title-row .ro-count", "1")

	// Redesign select-table: the cluster name is the net-new canonical `.cl-name`
	// mono-link cell, the API URL is the canonical `.ro-cell-url` cell rendered
	// FULL (never truncated). Exactly one cluster row ("test").
	if got := p.texts("td.cl-name"); strings.Join(got, "|") != "test" {
		t.Fatalf("cluster rows = %v, want [test]", got)
	}
	p.wantAttr("td.cl-name a", "href", "/clusters/test")
	// The API-URL cell carries the full URL verbatim (no truncation / ellipsis).
	apiURL := p.text("td.ro-cell-url")
	if !strings.HasPrefix(apiURL, "http") || strings.Contains(apiURL, "…") {
		t.Fatalf("API URL cell = %q, want the full untruncated URL", apiURL)
	}

	// The clusters page must NOT render the sidebar resource labels (no sidebar /
	// no namespace context on the entry page, D11).
	p.wantAbsent(".menu-label")
	p.wantAbsent(".ro-sidebar")

	// Search-select contract: a per-row checkbox carries data-toggle-button
	// pointing at the (initially disabled, until >=1 selected) primary search CTA.
	p.wantAttr(`input.ro-check[type="checkbox"][name="cluster"]`, "data-toggle-button", "search-clusters-button")
	p.wantHas("button.ro-btn#search-clusters-button[disabled]")
}

// ---------------------------------------------------------------------------
// Cluster overview: the namespaces table (cluster-scoped kind WITH rendered
// rows + per-bucket age cell classes) and the cluster resource-types table.
// ---------------------------------------------------------------------------

func TestBehaviorClusterOverview(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Date(2024, 1, 3, 6, 0, 0, 0, time.UTC))
	p := get(t, app, "/clusters/test", http.StatusOK)

	p.wantText("title", "test Cluster - readout")
	// Redesign overview (D4/D11/D13): the content root carries .ro-rd, the title is
	// the .ro-title-row, and the section headers are .ro-section-label (the first is
	// the borrowed select-table's Namespaces section).
	p.wantHas(".ro-rd")
	p.wantText(".ro-title-row .ro-title", "test")
	p.wantText(".ro-section-label:first-of-type", "Namespaces")

	// Namespace rows ride the BORROWED clusters `.ro-select-table` treatment (D11):
	// the name cell is the canonical `.cl-name` mono link. Cell VALUES + the
	// search-select data-toggle-button.
	if got := p.texts("td.cl-name"); strings.Join(got, "|") != "default|kube-system|my-app" {
		t.Fatalf("namespace rows = %v, want [default kube-system my-app]", got)
	}
	p.wantAttr(`input.ro-check[type="checkbox"][name="namespace"]`, "data-toggle-button", "search-namespaces-button")
	p.wantHas("button.ro-btn#search-namespaces-button[disabled]")
	// Clicking a namespace drops into its pods (the redesign contract).
	p.wantAttr("td.cl-name a", "href", "/clusters/test/namespaces/default/pods")

	// Cluster resource types include the cluster-scoped CSINode (storage.k8s.io)
	// and Node. These are the cluster-scoped matrix cells. (The kind name is the
	// link text; a CRD badge, where present, is a sibling span -- so read the <a>.)
	clusterKinds := p.texts(".ro-cell-kind a")
	assertContainsAll(t, "cluster resource-types kinds", clusterKinds, "CSINode", "Node", "Namespace")
}

// TestBehaviorClusterOverviewAgeBuckets walks the cluster-overview namespace rows
// through every age bucket by re-rendering at several fixed clocks. The three
// fixture namespaces are spaced 24h apart (2024-01-01/02/03), so at any one
// clock at most one row is inside the <1-day window -- but stepping the clock
// places the youngest row (my-app, 2024-01-03T00:00:00Z) into each bucket in
// turn, pinning that the render path wires s.ageClass into the row's age cell
// class (the cell is the LAST <td> of the namespace row). The bucket math is
// also pinned at the ageClass() unit level; this pins it at the RENDER level.
func TestBehaviorClusterOverviewAgeBuckets(t *testing.T) {
	base := time.Date(2024, 1, 3, 0, 0, 0, 0, time.UTC) // my-app's creationTimestamp
	cases := []struct {
		name  string
		clock time.Time
		want  string
	}{
		{"fresh: my-app aged 30s", base.Add(30 * time.Second), "age-fresh"},
		{"recent: my-app aged 6h", base.Add(6 * time.Hour), "age-recent"},
		{"day: my-app aged 12h", base.Add(12 * time.Hour), "age-day"},
		{"week: my-app aged 20h", base.Add(20 * time.Hour), "age-week"},
		{"old: my-app aged 48h", base.Add(48 * time.Hour), "age-old"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := newServer(t, baseConfig(t), tc.clock)
			p := get(t, app, "/clusters/test", http.StatusOK)
			// my-app is the third namespace row; its age cell is the last <td>.
			// Address it precisely: the row whose name cell links to my-app/pods.
			row := p.doc.Find(`tr:has(a[href="/clusters/test/namespaces/my-app/pods"])`)
			if row.Length() == 0 {
				t.Fatalf("my-app namespace row not found\nbody=%s", p.rec.Body.String())
			}
			ageCell := row.Find("td").Last()
			class, _ := ageCell.Attr("class")
			if !strings.Contains(class, tc.want) {
				t.Fatalf("my-app age cell class = %q, want to contain %q (clock %s)", class, tc.want, tc.clock)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Resource-types pages: the full kind matrix WITHOUT needing per-kind list
// fixtures -- a non-pod namespaced built-in (ReplicaSet), a CRD (Certificate
// with the CRD badge), plus the cluster/namespaced split and the quiet
// bordered scope badges (D3: categorical values get a badge, never a green
// boolean or a dot).
// ---------------------------------------------------------------------------

func TestBehaviorResourceTypesMatrix(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/namespaces/default/_resource-types", http.StatusOK)

	p.wantText("title", "Resource Types - readout")

	// Kind name is the link text; the CRD badge is a sibling span in the same
	// cell, so read the <a> to get just the kind.
	kinds := p.texts(".ro-cell-kind a")
	// Non-pod namespaced built-in + a CRD shape + core kinds all surface here.
	assertContainsAll(t, "namespaced resource-types kinds", kinds,
		"ReplicaSet", "Deployment", "Service", "Pod", "Event", "Certificate")

	// The CRD badge rides ONLY on the custom resource (cert-manager.io), not on
	// built-ins (apps, *.k8s.io). isCRD(cert-manager.io)==true.
	badges := p.texts(".ro-crd-badge")
	if len(badges) == 0 {
		t.Fatalf("expected a CRD badge on the namespaced resource-types page, found none")
	}
	// The Certificate row carries the CRD badge; the Deployment row does not.
	certRow := p.doc.Find(`tr:has(a[href="/clusters/test/namespaces/default/certificates"])`)
	if certRow.Find(".ro-crd-badge").Length() != 1 {
		t.Fatalf("Certificate row should carry exactly one CRD badge")
	}
	deployRow := p.doc.Find(`tr:has(a[href="/clusters/test/namespaces/default/deployments"])`)
	if deployRow.Find(".ro-crd-badge").Length() != 0 {
		t.Fatalf("Deployment (apps/v1 built-in) must NOT carry a CRD badge")
	}

	// Scope badge bound to the ROW's flag (not just "some badge exists"): a known
	// namespaced row (Deployment) carries the quiet .scope-badge.ns reading
	// "namespaced", and on the cluster page a known cluster-scoped row carries
	// .scope-badge.cluster reading "cluster" (the amber-bordered variant). An
	// inverted scope would break one of these. D3: no green boolean text, no dot.
	deployScope := p.doc.Find(`tr:has(a[href="/clusters/test/namespaces/default/deployments"])`)
	if deployScope.Find(".scope-badge.ns").Length() != 1 || deployScope.Find(".scope-badge.cluster").Length() != 0 {
		t.Fatalf("Deployment (namespaced) row should carry .scope-badge.ns, not .scope-badge.cluster")
	}
	if got := normSpace(deployScope.Find(".scope-badge.ns").Text()); got != "namespaced" {
		t.Fatalf("Deployment scope badge text = %q, want namespaced", got)
	}
	pc := get(t, app, "/clusters/test/_resource-types", http.StatusOK)
	// CSINode has a unique href (Node + NodeMetrics both link to /nodes, so that
	// href matches two rows); CSINode is cluster-scoped, so its row carries the
	// .scope-badge.cluster variant and never .scope-badge.ns.
	csiScope := pc.doc.Find(`tr:has(a[href="/clusters/test/csinodes"])`)
	if csiScope.Find(".scope-badge.cluster").Length() != 1 || csiScope.Find(".scope-badge.ns").Length() != 0 {
		t.Fatalf("CSINode (cluster-scoped) row should carry .scope-badge.cluster, not .scope-badge.ns")
	}
	if got := normSpace(csiScope.Find(".scope-badge.cluster").Text()); got != "cluster" {
		t.Fatalf("CSINode scope badge text = %q, want cluster", got)
	}
	// The retired green boolean pills are gone from both pages.
	if p.doc.Find(".ro-bool-yes, .ro-bool-no").Length() != 0 || pc.doc.Find(".ro-bool-yes, .ro-bool-no").Length() != 0 {
		t.Fatalf("retired .ro-bool-yes/.ro-bool-no pills still rendered on a resource-types page")
	}
	// Cluster tab vs Namespaced tab active state. Resource-types borrows the
	// detail-page `.ro-tabs` chrome (anchor-based, the active tab carries
	// is-active on the <a> itself) while keeping the KEEP-AS-IS `ro-rt-tabs`
	// marker class on the tab container (D11/D13).
	pc.wantText(".ro-rt-tabs a.is-active", "Cluster")
	p.wantText(".ro-rt-tabs a.is-active", "Namespaced")
}

// TestResourceTypesRender pins the redesign borrow-rule application on the
// resource-types page (D11/D13): the `.ro-rd` content marker, the borrowed
// detail-tab `.ro-tabs` chrome (carrying the KEEP-AS-IS `ro-rt-tabs` marker) with
// the active tab on the <a>, a PLAIN `.ro-table` (in `.ro-table-wrap`, not the
// legacy `.ro-list-table`) for the kind matrix, the KEEP-AS-IS cell classes
// (`.ro-cell-kind` kind link + sibling `.ro-crd-badge`, `.ro-rt-group`,
// `.ro-rt-version`), the quiet bordered `.scope-badge` scope cell (D3), and the
// kind-icon resolver in the Kind cell (`.res-kind .ico`).
func TestResourceTypesRender(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/namespaces/default/_resource-types", http.StatusOK)

	// Redesign content marker + borrowed chrome.
	p.wantHas(".ro-rd")
	p.wantHas(".ro-rd .ro-tabs.ro-rt-tabs")
	p.wantText(".ro-tabs.ro-rt-tabs a.is-active", "Namespaced")
	// PLAIN `.ro-table` in a `.ro-table-wrap` -- not the legacy `.ro-list-table`.
	p.wantHas(".ro-table-wrap table.ro-table")
	p.wantAbsent(".ro-list-table")

	// KEEP-AS-IS cell classes survive: the Deployment row carries the kind link in
	// `.ro-cell-kind`, the mono group/version cells, and the quiet scope badge.
	deployRow := p.doc.Find(`tr:has(a[href="/clusters/test/namespaces/default/deployments"])`)
	if deployRow.Length() == 0 {
		t.Fatalf("Deployment row missing from resource-types table")
	}
	if got := normSpace(deployRow.Find("td.ro-cell-kind a").Text()); got != "Deployment" {
		t.Fatalf("Deployment `.ro-cell-kind a` text = %q, want Deployment", got)
	}
	if deployRow.Find("td.ro-rt-group").Length() != 1 || deployRow.Find("td.ro-rt-version").Length() != 1 {
		t.Fatalf("Deployment row missing the KEEP-AS-IS group/version cells")
	}
	if deployRow.Find(".scope-badge.ns").Length() != 1 {
		t.Fatalf("Deployment (namespaced) row should carry .scope-badge.ns")
	}
	// The Kind cell pairs the resolved kind icon with the link (borrow rule:
	// icons.KindIcon).
	if deployRow.Find("td.ro-cell-kind .res-kind .ico, td.ro-cell-kind .res-kind .kind-tile, td.ro-cell-kind .res-kind svg").Length() == 0 {
		t.Fatalf("Deployment Kind cell missing the resolved kind icon")
	}
}

// ---------------------------------------------------------------------------
// Resource-list (pods, namespaced): cell VALUES, the phase strip, the column
// sort hrefs, the htmx wiring on #resource-list-content, and the row name links.
// This is the heart of the fact net.
// ---------------------------------------------------------------------------

func TestBehaviorPodListFacts(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)

	p.wantText("title", "pods in test - readout")
	p.wantText("h1.title", "Pods")

	// #resource-list-content htmx wiring -- the v2 single-type loop (D6): the
	// container bakes NO request URL (data-live-url="location" is the readout.js
	// contract: the refresh tick derives the `_table` URL from location.href at
	// fire time, so it can never revert a pushed sort/filter), and swaps go
	// through the CSP-safe ro-morph extension (hx-swap="morph", config delivered
	// as a JS object -- attribute-spec morph config would be eval'd and blocked).
	p.wantAttr("#resource-list-content", "data-live-url", "location")
	p.wantAttr("#resource-list-content", "hx-target", "this")
	p.wantAttr("#resource-list-content", "hx-ext", "ro-morph")
	p.wantAttr("#resource-list-content", "hx-swap", "morph")
	// The render-time-baked PartialURL contract is REPLACED on single-type pages:
	// a baked hx-get/hx-trigger here would resurrect the tick-reverts-sort bug.
	p.wantAbsent("#resource-list-content[hx-get]")
	p.wantAbsent("#resource-list-content[hx-trigger]")
	// The refresh points its in-flight indicator at the single global top progress
	// bar (#ro-progress in the layout), the same rail every hx-boost navigation uses.
	p.wantAttr("#resource-list-content", "hx-indicator", "#ro-progress")
	// The bulk-bar mount (Unit 16 content) sits OUTSIDE the swap target so a
	// morph never touches it.
	p.wantHas("#ro-bulkbar")
	p.wantAbsent("#resource-list-content #ro-bulkbar")

	// Column headers in order, each linking to its own ?sort=<col>. The redesign
	// list engine renders the canonical `.ro-table` inside `.ro-table-wrap`.
	headers := p.texts("table.ro-table thead th")
	if strings.Join(headers, "|") != "Name|Ready|Status|Restarts|Age|Created" {
		t.Fatalf("pod table headers = %v", headers)
	}
	for _, col := range []string{"Name", "Ready", "Status", "Restarts", "Age", "Created"} {
		want := "/clusters/test/namespaces/default/pods?sort=" + col
		if !p.containsHref("thead th a", want) {
			t.Fatalf("missing column sort href %q among %v", want, p.attrs("thead th a", "href"))
		}
		// The v2 loop (D6): every header ALSO carries an hx-get of the same sort
		// against the `_table` partial, morph-swapped into the persistent
		// container (the canonical href stays for history/new-tab/no-JS).
		wantPartial := "/clusters/test/namespaces/default/pods/_table?sort=" + col
		if !contains(p.attrs("thead th a", "hx-get"), wantPartial) {
			t.Fatalf("missing column partial sort hx-get %q among %v", wantPartial, p.attrs("thead th a", "hx-get"))
		}
	}
	p.wantAttr("thead th a[hx-get]", "hx-target", "#resource-list-content")
	p.wantAttr("thead th a[hx-get]", "hx-swap", "morph")

	// Row identity (D6): every row carries data-key="cluster/ns/name" plus the
	// id derived from it, so idiomorph matches rows by object identity (never
	// position) and client selection/focus state re-keys across morphs.
	p.wantAttr(`tr[data-key="test/default/nginx"]`, "id", "row-test/default/nginx")
	p.wantAttr(`tr[data-key="test/default/my-app"]`, "id", "row-test/default/my-app")

	// Phase strip: the Running tally (redesign phase strip, dot tone "ok").
	p.wantText(".ro-phase-label", "Running")
	p.wantText(".ro-phase-strip .ro-phase-tally .ro-phase-count", "2")

	// Row name cells + their detail links (cell VALUES, not just headers). The
	// sticky name cell is `td.cell-name`; the cell TEXT is the full untruncated
	// pod name (pn-head+pn-tail when split).
	names := p.texts("td.cell-name")
	if strings.Join(names, "|") != "nginx|my-app" {
		t.Fatalf("pod name cells = %v, want [nginx my-app]", names)
	}
	p.wantAttr("td.cell-name a", "href", "/clusters/test/namespaces/default/pods/nginx")

	// The nginx row's Status cell value + the redesign status dot toned `ok`
	// (mapped from the kube success class) inside `.cell-status.ok`.
	nginxRow := p.doc.Find(`tr:has(a[href="/clusters/test/namespaces/default/pods/nginx"])`)
	if got := normSpace(nginxRow.Find("td").Eq(2).Text()); got != "Running" {
		t.Fatalf("nginx Status cell = %q, want Running", got)
	}
	if nginxRow.Find(".cell-status.ok .ro-dot.ok").Length() == 0 {
		t.Fatalf("nginx Status cell missing the ok status dot")
	}
	// Running is a STEADY state -> its dot must NOT pulse.
	if nginxRow.Find(".ro-dot.pulse").Length() != 0 {
		t.Fatalf("Running status dot should not pulse (steady state)")
	}
	// Ready cell value 1/1 wrapped in `.ready.full` (all replicas ready).
	if got := normSpace(nginxRow.Find("td .ready.full").First().Text()); got != "1/1" {
		t.Fatalf("nginx Ready cell = %q, want 1/1 in .ready.full", got)
	}

	// "Show CPU/Memory Usage" affordance (join=metrics not yet applied).
	p.wantAttr(`a[href="/clusters/test/namespaces/default/pods?join=metrics"]`, "href", "/clusters/test/namespaces/default/pods?join=metrics")

	// The tools form carries the owned .ro-* layout and the labelcols / selector /
	// filter inputs the delegated submit handler blanks-when-empty.
	p.wantHas(`form.tools-form .ro-tools-grid`)
	p.wantHas(`form.tools-form .ro-tools-field .ro-input[name="labelcols"]`)
	p.wantHas(`form.tools-form .ro-tools-field .ro-input[name="selector"]`)
	p.wantHas(`form.tools-form .ro-tools-field .ro-input[name="filter"]`)
	p.wantHas(`form.tools-form .ro-field-icon`)
	p.wantHas(`form.tools-form button.ro-btn.quiet[type="submit"]`)
}

// TestBehaviorPodListSortToggle pins the descending-toggle behaviour: with
// ?sort=Name the Name header flips to ?sort=Name:desc and grows a sort icon,
// while the other headers keep their plain ascending sort href. The row order
// also flips (my-app before nginx) -- a cell-value fact, not just a header href.
func TestBehaviorPodListSortToggle(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/namespaces/default/pods?sort=Name", http.StatusOK)

	// The Name header now points at the :desc toggle (percent-encoded colon) and
	// carries a sort icon; goquery returns the href HTML-decoded (&amp;->&) but
	// keeps %3A literal. Its hx-get carries the SAME toggle against the `_table`
	// partial (D6: the sort interaction is a partial morph; the container no
	// longer bakes a request URL -- the tick derives it from location).
	nameHeader := p.doc.Find(`thead th:has(a) a:contains("Name")`).First()
	href, _ := nameHeader.Attr("href")
	if href != "/clusters/test/namespaces/default/pods?sort=Name%3Adesc" {
		t.Fatalf("Name header href = %q, want ...?sort=Name%%3Adesc", href)
	}
	if hxGet, _ := nameHeader.Attr("hx-get"); hxGet != "/clusters/test/namespaces/default/pods/_table?sort=Name%3Adesc" {
		t.Fatalf("Name header hx-get = %q, want .../_table?sort=Name%%3Adesc", hxGet)
	}
	if nameHeader.Find(".icon").Length() == 0 {
		t.Fatalf("active sort column should render a sort icon")
	}
	// A non-active column keeps the plain ascending sort.
	if !p.containsHref("thead th a", "/clusters/test/namespaces/default/pods?sort=Ready") {
		t.Fatalf("non-active column lost its plain ascending sort href")
	}

	// Ascending name sort: my-app sorts before nginx (row-order cell fact).
	names := p.texts("td.cell-name")
	if strings.Join(names, "|") != "my-app|nginx" {
		t.Fatalf("sorted name cells = %v, want [my-app nginx]", names)
	}

	// The synthetic "Created" header has its OWN render branch (createdSortParam
	// maps Created->Created:desc), separate from the column-header loop. Pin its
	// :desc toggle + sort icon under ?sort=Created, the same way as the Name case
	// so a miswired Created toggle has to break here.
	pc := get(t, app, "/clusters/test/namespaces/default/pods?sort=Created", http.StatusOK)
	createdHeader := pc.doc.Find(`thead th:has(a) a:contains("Created")`).First()
	createdHref, _ := createdHeader.Attr("href")
	if createdHref != "/clusters/test/namespaces/default/pods?sort=Created%3Adesc" {
		t.Fatalf("Created header href = %q, want ...?sort=Created%%3Adesc", createdHref)
	}
	if createdHeader.Find(".icon").Length() == 0 {
		t.Fatalf("active Created sort column should render a sort icon")
	}
}

// TestBehaviorPodListAllNamespaces pins the all-namespaces variant: a leading
// Namespace column appears, the breadcrumb shows "all", and the v2 loop's
// partial sort headers target the _all path (an all-NAMESPACES pods list is
// still a single-TYPE page, so the D6 loop applies).
func TestBehaviorPodListAllNamespaces(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/namespaces/_all/pods", http.StatusOK)

	p.wantAttr("#resource-list-content", "data-live-url", "location")
	if !contains(p.attrs("thead th a", "hx-get"), "/clusters/test/namespaces/_all/pods/_table?sort=Name") {
		t.Fatalf("missing _all-path partial sort hx-get among %v", p.attrs("thead th a", "hx-get"))
	}
	headers := p.texts("table.ro-table thead th")
	if headers[0] != "Namespace" {
		t.Fatalf("all-namespaces first header = %q, want Namespace (headers=%v)", headers[0], headers)
	}
	// Breadcrumb middle crumb is the all-namespaces link.
	p.wantHas(`nav.breadcrumb a[href="/clusters/test/namespaces"]`)
}

// TestBehaviorListQueryMatrix walks the query-variant matrix and pins, per
// variant, that the option round-trips into the form input / hx-get and shapes
// the table as expected.
func TestBehaviorListQueryMatrix(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())

	t.Run("labelcols adds an App column and activates the tools form", func(t *testing.T) {
		p := get(t, app, "/clusters/test/namespaces/default/pods?labelcols=app", http.StatusOK)
		p.wantAttr(`form.tools-form .ro-input[name="labelcols"]`, "value", "app")
		p.wantHas("form.tools-form.is-active")
		p.wantHas("form.tools-form.is-active .ro-tools-grid")
		if !contains(p.texts("thead th"), "App") {
			t.Fatalf("labelcols=app did not add an App column: %v", p.texts("thead th"))
		}
	})

	t.Run("selector round-trips into the selector input", func(t *testing.T) {
		p := get(t, app, "/clusters/test/namespaces/default/pods?selector=app%3Dnginx", http.StatusOK)
		p.wantAttr(`form.tools-form .ro-input[name="selector"]`, "value", "app=nginx")
		// The selector rides every header's partial sort hx-get (D6: the partial
		// request must carry the full current query so a sort keeps the selector).
		want := "/clusters/test/namespaces/default/pods/_table?selector=app%3Dnginx&sort=Name"
		if !contains(p.attrs("thead th a", "hx-get"), want) {
			t.Fatalf("selector missing from partial sort hx-get: %v", p.attrs("thead th a", "hx-get"))
		}
	})

	t.Run("filter narrows rows and round-trips into the filter input", func(t *testing.T) {
		p := get(t, app, "/clusters/test/namespaces/default/pods?filter=nginx", http.StatusOK)
		p.wantAttr(`form.tools-form .ro-input[name="filter"]`, "value", "nginx")
		if got := p.texts("td.cell-name"); strings.Join(got, "|") != "nginx" {
			t.Fatalf("filter=nginx rows = %v, want [nginx]", got)
		}
		p.wantText(".ro-phase-strip .ro-phase-tally .ro-phase-count", "1")
	})

	t.Run("a filter matching nothing renders the empty-FILTERED state", func(t *testing.T) {
		// A filter that hides every row renders the empty-FILTERED state (Unit 14):
		// a removable filter chip naming the active filter + a Clear-filters action,
		// NOT the plain empty sentence. The chip's ✕ drops just that one filter; the
		// Clear button drops the whole filter set (both read-only GETs back to the
		// same list path).
		p := get(t, app, "/clusters/test/namespaces/default/pods?filter=zzz-no-such-pod", http.StatusOK)
		p.wantAbsent("td.cell-name")
		p.wantText(".ro-empty-row .ro-empty-lg h3", "No Pod objects match your filters")
		if got := p.text(".ro-empty-row .ro-scope .ro-scope-chip"); !strings.Contains(got, "zzz-no-such-pod") {
			t.Fatalf("empty-filtered chip = %q, want it to name the filter", got)
		}
		// The chip ✕ removes just the filter param; Clear filters drops the set.
		p.wantAttr(".ro-empty-row .ro-scope .ro-scope-chip a.retry", "href", "/clusters/test/namespaces/default/pods")
		p.wantAttr(".ro-empty-row .ro-empty-actions a", "href", "/clusters/test/namespaces/default/pods")
	})

	t.Run("a genuinely empty list renders the plain empty sentence + broad action", func(t *testing.T) {
		// Pins the empty-state sentence "No <Kind> objects in namespace "<ns>"
		// found." verbatim (the redesign .ro-empty-lg in-table state). This guards
		// the templ empty-state against the @templ.Raw children-drop that would
		// silently lose the trailing "found." -- a regression the row-bearing facts
		// cannot see. With no filter active the broad next action ("Show pods across
		// all namespaces") is offered.
		p := get(t, app, "/clusters/test/namespaces/empty/pods", http.StatusOK)
		p.wantAbsent("td.cell-name")
		p.wantText(".ro-empty-row .ro-empty-lg .ro-empty-title", `No Pod objects in namespace "empty" found.`)
		p.wantText(".ro-empty-row .ro-empty-lg .ro-empty-hint", "Nothing to show here yet.")
		p.wantText(".ro-empty-row .ro-empty-lg .ro-empty-actions a", "Show pods across all namespaces")
	})

	t.Run("hidecols removes the Status column", func(t *testing.T) {
		p := get(t, app, "/clusters/test/namespaces/default/pods?hidecols=Status", http.StatusOK)
		if contains(p.texts("thead th"), "Status") {
			t.Fatalf("hidecols=Status left a Status header: %v", p.texts("thead th"))
		}
		// The Status sort href is gone; Name's remains.
		if p.containsHref("thead th a", "/clusters/test/namespaces/default/pods?hidecols=Status&sort=Status") {
			t.Fatalf("hidden Status column still has a sort href")
		}
	})

	t.Run("join=metrics adds CPU/Memory columns with formatted values", func(t *testing.T) {
		p := get(t, app, "/clusters/test/namespaces/default/pods?join=metrics", http.StatusOK)
		headers := p.texts("thead th")
		assertContainsAll(t, "metrics headers", headers, "CPU Usage", "Memory Usage")
		// nginx's metrics row: 250m CPU, 128 MiB (from metrics_pods_list.json).
		nginxRow := p.doc.Find(`tr:has(a[href="/clusters/test/namespaces/default/pods/nginx"])`)
		rowText := normSpace(nginxRow.Text())
		for _, want := range []string{"250m", "128 MiB"} {
			if !strings.Contains(rowText, want) {
				t.Fatalf("nginx metrics row missing %q: %q", want, rowText)
			}
		}
	})

	t.Run("custom-columns round-trips into hx-get and adds the column", func(t *testing.T) {
		p := get(t, app, "/clusters/test/namespaces/default/pods?custom-columns=Image=spec.containers[0].image", http.StatusOK)
		// The custom-columns spec rides the partial sort hx-get (addQuery
		// re-encodes the query, so '=' and brackets are percent-escaped).
		want := "/clusters/test/namespaces/default/pods/_table?custom-columns=Image%3Dspec.containers%5B0%5D.image&sort=Name"
		if !contains(p.attrs("thead th a", "hx-get"), want) {
			t.Fatalf("custom-columns missing from partial sort hx-get: %v", p.attrs("thead th a", "hx-get"))
		}
		if !contains(p.texts("thead th"), "Image") {
			t.Fatalf("custom-columns did not add an Image column: %v", p.texts("thead th"))
		}
		// The joined cell value is the container image.
		nginxRow := p.doc.Find(`tr:has(a[href="/clusters/test/namespaces/default/pods/nginx"])`)
		if !strings.Contains(nginxRow.Text(), "nginx:1.27") {
			t.Fatalf("custom Image column missing nginx:1.27: %q", nginxRow.Text())
		}
	})
}

// TestBehaviorResourceListPartial pins the _table partial: it renders the bare
// table fragment (no <html>/<body> chrome) so the htmx morph-swap replaces only
// the list region.
func TestBehaviorResourceListPartial(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/namespaces/default/pods/_table", http.StatusOK)
	// Fragment: no document chrome. (goquery wraps any fragment in html/body
	// when parsing, so detect the absence of the REAL chrome markers the page
	// layout emits -- the navbar, the command palette, the doctype/title.)
	if p.has("nav.navbar") || p.has("#ro-palette") || p.has("title") || p.has("#resource-list-content") {
		t.Fatalf("_table partial leaked full-page chrome\nbody=%s", p.rec.Body.String())
	}
	// But it DOES carry the table with the real rows.
	p.wantText("h1.title", "Pods")
	if got := p.texts("td.cell-name"); strings.Join(got, "|") != "nginx|my-app" {
		t.Fatalf("_table partial rows = %v", got)
	}
}

// ---------------------------------------------------------------------------
// Resource-view: detail + YAML. Pins the title/kind badge, the tab set, the
// summary sections, the empty Events table, and the Pygments-compatible YAML
// id scheme (yaml-<prefix>line-N) that readout.js depends on.
// ---------------------------------------------------------------------------

func TestBehaviorPodDetailFacts(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/namespaces/default/pods/nginx", http.StatusOK)

	p.wantText("title", "nginx (Pod in default) - readout")
	// Redesign detail spine: .ro-detail-title carries the H1 name + the kind
	// badge; the Download-YAML affordance is a quiet icon button in .ro-detail-actions.
	p.wantText(".ro-detail-title .ro-kind-badge", "Pod")
	p.wantText(".ro-detail-title h1.ro-title", "nginx")
	p.wantAttr(`.ro-detail-actions a[title="Download resource object as YAML"]`, "href", "/clusters/test/namespaces/default/pods/nginx?download=yaml")
	// The detail page carries the .ro-rd content marker so the canonical class
	// names route to the redesign CSS (D13).
	p.wantHas(".ro-rd .ro-detail-title")

	// Redesign tabs (.ro-tabs bare anchors): a Pod gets four -- Default active,
	// then YAML / Events / Logs.
	tabs := p.texts(".ro-tabs a")
	if strings.Join(tabs, "|") != "Default|YAML|Events|Logs" {
		t.Fatalf("detail tabs = %v, want Default|YAML|Events|Logs", tabs)
	}
	p.wantText(".ro-tabs a.is-active", "Default")
	p.wantAttr(`.ro-tabs a:contains("Events")`, "href", "?view=events")

	// Default tab: Spec + Status YAML cards (collapsible[data-name] + copyable),
	// NOT the events table (events moved to their own tab).
	cardNames := p.attrs(".ro-yaml-card", "data-name")
	assertContainsAll(t, "yaml card sections", cardNames, "spec", "status")
	p.wantHas(".ro-yaml-card .ro-copy-btn")
	p.wantAbsent(".ro-event-msg") // events render only under ?view=events
	// Labels + the generic annotation surface.
	p.wantBodyContains("generic-annotation")
}

// TestBehaviorPodEventsTabFacts pins the Events TAB (a separate ?view=events
// GET): the toned events table renders the fixture's single Scheduled event with
// the redesign status-dot/cell-status pair on the Type cell, the age bucket on
// the Age cell, and the wrapping .ro-event-msg on the Message cell.
func TestBehaviorPodEventsTabFacts(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/namespaces/default/pods/nginx?view=events", http.StatusOK)

	// Events is the active tab.
	p.wantText(".ro-tabs a.is-active", "Events")
	// The events table is the redesign .ro-table inside .ro-table-wrap.
	p.wantHas(".ro-section .ro-table-wrap table.ro-table")

	eventRow := p.doc.Find("table.ro-table tbody tr")
	if eventRow.Length() != 1 {
		t.Fatalf("expected exactly one event row, got %d", eventRow.Length())
	}
	// The Message cell is the one wrapping cell (td.ro-event-msg).
	if got := normSpace(eventRow.Find("td.ro-event-msg").Text()); got != "Successfully assigned default/nginx to 127.0.0.1" {
		t.Fatalf("event message cell (td.ro-event-msg) = %q, want the Scheduled message", got)
	}
	// Type cell: a Normal event tones to mute -> .cell-status.mute with a .ro-dot.mute.
	if eventRow.Find(".cell-status.mute .ro-dot.mute").Length() != 1 {
		t.Fatalf("event Type cell missing .cell-status.mute > .ro-dot.mute: %s", normSpace(eventRow.Text()))
	}
	if got := normSpace(eventRow.Find(".cell-status").Text()); got != "Normal" {
		t.Fatalf("event Type text = %q, want Normal", got)
	}
	// Age cell carries the age class for lastTimestamp at days=1 == age-old.
	if ageCls, _ := eventRow.Find("td:nth-child(3)").Attr("class"); ageCls != "age-old" {
		t.Fatalf("event Age cell class = %q, want age-old (age_color lastTimestamp days=1)", ageCls)
	}
}

func TestBehaviorPodYAMLViewIDScheme(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/namespaces/default/pods/nginx?view=yaml", http.StatusOK)

	p.wantText(".ro-tabs a.is-active", "YAML")

	// Pygments-compatible highlight table: linenos column + code column.
	p.wantHas("table.highlighttable td.linenos")
	p.wantHas("table.highlighttable td.code")

	// The per-line span id scheme readout.js keys off: getElementById('yaml-'+id)
	// and buildYamlFolds() both require `yaml-line-N` ids in the code cell, with
	// matching `#line-N` gutter anchors. (The YAML view uses an empty prefix.)
	p.wantHas("td.code pre span#yaml-line-1")
	p.wantHas("td.code pre span#yaml-line-2")
	p.wantHas(`td.linenos a[href="#line-1"]`)

	// D7: chroma server-side highlighting stays -- the YAML body carries the
	// Pygments token spans (recoloured by Unit 2's CSS, not re-tokenised). The
	// nginx manifest has mapping keys (.nt) and an unquoted literal value (.l, the
	// kind/apiVersion scalars), so both classes must appear in the code cell.
	p.wantHas("td.code .highlight .nt, td.code pre .nt")
	if p.count("td.code .nt") == 0 {
		t.Fatalf("YAML body missing chroma key token spans (.nt)")
	}
	if p.count("td.code .l") == 0 {
		t.Fatalf("YAML body missing chroma unquoted-literal token spans (.l)")
	}

	// Body fact: apiVersion + kind are present (check_yaml).
	p.wantBodyContains("apiVersion")
	if !strings.Contains(p.text("span#yaml-line-2"), "Pod") {
		t.Fatalf("YAML line 2 should render kind: Pod, got %q", p.text("span#yaml-line-2"))
	}
}

func TestBehaviorNodeDetailFacts(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/nodes/worker-1", http.StatusOK)
	p.wantText(".ro-detail-title .ro-kind-badge", "Node")
	// System info renders + the field-selected pods subtable carries data-name.
	p.wantBodyContains("kubeletVersion")
	p.wantBodyContains("v1.29.2")
	p.wantHas(`[data-name="pods"]`)
	// The related-pods subtable migrated to the redesign .ro-table.
	p.wantHas(`.collapsible[data-name="pods"] .ro-table-wrap table.ro-table`)
	p.wantHas(`.collapsible[data-name="pods"] table.ro-table .cell-status.ok .ro-dot.ok`)

	// Node summary blocks (named facts replacing the retired TestRenderNodeSummary
	// byte asserts). The three section labels are present, in order.
	sectionLabels := p.texts(".ro-section .ro-section-label")
	assertContainsAll(t, "node section labels", sectionLabels, "Conditions", "Capacity / Allocatable", "System Info")

	// Condition pills: the Ready=True pill carries the redesign ok tone + the
	// .ro-dot + the name/value spans. nodeConditionTone(Ready,True)=ok; the
	// fixture's pressure conditions are False so they are ok too -- assert the
	// Ready pill precisely (tone bound to the condition, not "some pill exists").
	readyPill := p.doc.Find(`.ro-cond-pill:has(.ro-cond-name:contains("Ready"))`)
	if readyPill.Length() != 1 {
		t.Fatalf("expected exactly one Ready condition pill, got %d", readyPill.Length())
	}
	if cls, _ := readyPill.Attr("class"); !strings.Contains(cls, "ok") {
		t.Fatalf("Ready=True pill tone = %q, want ok", cls)
	}
	if normSpace(readyPill.Find(".ro-cond-val").Text()) != "True" {
		t.Fatalf("Ready pill value = %q, want True", normSpace(readyPill.Find(".ro-cond-val").Text()))
	}
	if readyPill.Find(".ro-dot").Length() != 1 {
		t.Fatalf("Ready pill missing its .ro-dot")
	}

	// Capacity / Allocatable KV rows: the allocatable column prefixes its keys
	// with "allocatable " (the fixture has allocatable cpu=1930m). The capacity
	// cpu row reads cpu / 2.
	kvKeys := p.texts(".ro-kv-row dt")
	assertContainsAll(t, "node KV keys", kvKeys, "cpu", "allocatable cpu", "kubeletVersion")
	allocCPU := p.doc.Find(`.ro-kv-row:has(dt:contains("allocatable cpu")) dd`)
	if normSpace(allocCPU.Text()) != "1930m" {
		t.Fatalf("allocatable cpu value = %q, want 1930m", normSpace(allocCPU.Text()))
	}
	// System Info kubeletVersion value bound to its row.
	kubeletRow := p.doc.Find(`.ro-kv-row:has(dt:contains("kubeletVersion")) dd`)
	if normSpace(kubeletRow.Text()) != "v1.29.2" {
		t.Fatalf("kubeletVersion value = %q, want v1.29.2", normSpace(kubeletRow.Text()))
	}
}

// TestBehaviorDetailLabelChips pins the resource-view label/annotation chips in
// the v2 vocabulary (D3: chips are NEUTRAL; key and value differ by ink weight
// through the .ck/.cs/.cv spans, never by hue): each label is a click-to-filter
// anchor (D7/SPEC §8.1) to this kind's list in the same cluster/namespace with
// the `label:key=value` chip applied via `?f=` (the chip text QueryEscape'd
// whole). The annotations render as non-link .ro-chip.anno pills that truncate
// with a title= tooltip.
func TestBehaviorDetailLabelChips(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/namespaces/default/pods/nginx", http.StatusOK)

	// The Labels section + at least one chip anchor.
	p.wantText(`.ro-section:has(.ro-section-label:contains("Labels")) .ro-section-label`, "Labels")
	chip := p.doc.Find(`.ro-chips a.ro-chip`).First()
	if chip.Length() != 1 {
		t.Fatalf("expected a label chip anchor on the pod detail page")
	}
	href, _ := chip.Attr("href")
	// The chip links to the namespaced list with a `?f=label:key=value` chip
	// (Filters v2 click-to-filter), never the legacy selector= form.
	if !strings.HasPrefix(href, "/clusters/test/namespaces/default/pods?f=label%3A") {
		t.Fatalf("label chip href = %q, want a ?f=label:key=value chip link", href)
	}
	// The nginx pod fixture carries app=nginx; that chip's exact `?f=` href +
	// the ink-weight key/value split: the key sits in .ck, the ghost colon in
	// .cs, the firm value in .cv (D3 -- weight, not hue, separates key from value).
	appChip := p.doc.Find(`.ro-chips a[href="/clusters/test/namespaces/default/pods?f=label%3Aapp%3Dnginx"]`)
	if appChip.Length() != 1 {
		t.Fatalf("expected the app=nginx label chip with a ?f= chip href, hrefs=%v", p.attrs(".ro-chips a.ro-chip", "href"))
	}
	if k, v := normSpace(appChip.Find(".ck").Text()), normSpace(appChip.Find(".cv").Text()); k != "app" || v != "nginx" {
		t.Fatalf("label chip ck/cv = %q/%q, want app/nginx", k, v)
	}
	if appChip.Find(".cs").Length() != 1 {
		t.Fatalf("label chip missing the .cs separator span")
	}

	// Annotations render as non-link .ro-chip.anno pills carrying the full value
	// in title= (the fixture's example.com/note: generic-annotation), with the
	// same .ck/.cs/.cv split inside the chip body.
	p.wantText(`.ro-section:has(.ro-section-label:contains("Annotations")) .ro-section-label`, "Annotations")
	anno := p.doc.Find(`span.ro-chip.anno[title="example.com/note: generic-annotation"]`)
	if anno.Length() != 1 {
		t.Fatalf("expected the example.com/note annotation chip with the full key: value tooltip, titles=%v", p.attrs("span.ro-chip.anno", "title"))
	}
	if k, v := normSpace(anno.Find(".ck").Text()), normSpace(anno.Find(".cv").Text()); k != "example.com/note" || v != "generic-annotation" {
		t.Fatalf("annotation chip ck/cv = %q/%q, want example.com/note/generic-annotation", k, v)
	}

	// D3 negative: the retired green chip accent never reaches the detail DOM.
	if p.doc.Find(".ro-chip.app").Length() != 0 {
		t.Fatalf("retired .ro-chip.app accent rendered on the detail page")
	}
}

// TestBehaviorDetailBreadcrumb pins the resource-view object breadcrumb (named
// fact replacing the retired renderObjectBreadcrumb byte asserts): cluster ->
// namespace -> plural -> the active object name, each crumb linking to its
// scope.
func TestBehaviorDetailBreadcrumb(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/namespaces/default/pods/nginx", http.StatusOK)

	crumbs := p.texts("nav.breadcrumb li a")
	if strings.Join(crumbs, "|") != "test|default|pods|nginx" {
		t.Fatalf("detail breadcrumb crumbs = %v, want [test default pods nginx]", crumbs)
	}
	p.wantHas(`nav.breadcrumb a[href="/clusters/test"]`)
	p.wantHas(`nav.breadcrumb a[href="/clusters/test/namespaces/default"]`)
	p.wantHas(`nav.breadcrumb a[href="/clusters/test/namespaces/default/pods"]`)
	p.wantText("nav.breadcrumb li.is-active a", "nginx")
}

// ---------------------------------------------------------------------------
// Logs page.
// ---------------------------------------------------------------------------

func TestBehaviorLogsPage(t *testing.T) {
	cfg := baseConfig(t)
	cfg.ShowContainerLogs = true
	app := newServer(t, cfg, time.Now())
	p := get(t, app, "/clusters/test/namespaces/default/pods/nginx/logs", http.StatusOK)
	// The logs body carries the .ro-rd content marker (D13) and shares the detail
	// title + .ro-tabs chrome with resource_view (Default/YAML/Events/Logs, Logs
	// active here) so the two screens read consistently.
	p.wantHas(".ro-rd")
	p.wantText(".ro-rd .ro-detail-title .ro-kind-badge", "Pod")
	if tabs := p.texts(".ro-rd .ro-tabs a"); strings.Join(tabs, "|") != "Default|YAML|Events|Logs" {
		t.Fatalf("logs tabs = %v, want Default|YAML|Events|Logs", tabs)
	}
	p.wantText(".ro-rd .ro-tabs a.is-active", "Logs")
	// The tail/filter/Refresh form renders.
	p.wantHas("form.ro-logs-form")
	// Log lines render in a ro-logpre block; the fixture's "GET / 200" shows.
	p.wantHas("pre.ro-logpre")
	if !strings.Contains(p.text("pre.ro-logpre"), "GET / 200") {
		t.Fatalf("log line GET / 200 missing: %q", p.text("pre.ro-logpre"))
	}

	// With logs DISABLED (default) the page renders the disabled notice instead.
	disabled := newServer(t, baseConfig(t), time.Now())
	pd := get(t, disabled, "/clusters/test/namespaces/default/pods/nginx/logs", http.StatusOK)
	pd.wantBodyContains("Container Logs Disabled")
}

// ---------------------------------------------------------------------------
// Search: the redesign multi-cluster /search body -- the `.search-hero` (big
// query input + scope-opts line), the per-cluster `.ro-scope-chip` strip, the
// results `.ro-table` (Cluster/Namespace/Kind icon+name/Name/Age), and the
// `.ro-foundline` footer. The multi-cluster partial-failure banner + `.err` chip
// + inline retry are pinned by TestSearchPartialFailure (a multi-cluster vehicle).
// ---------------------------------------------------------------------------

// TestSearchRender pins the redesign search structure on a clean single-cluster
// success: the `.search-hero` with the big `.search-big` query input (the GET
// form round-trips q + hidden cluster/namespace/type), the `.search-opts` scope
// line, the all-ok per-cluster `.ro-scope-chip.ok` (no failure banner), and the
// results `.ro-table` row -- the Cluster + Namespace cells, the Kind cell pairing
// the resolved kind icon (`.res-kind .ico`) with the kind name, the sticky Name
// link, and the Age cell. The nginx pod is the lone `default`-namespace hit.
func TestSearchRender(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/search?q=nginx&cluster=test&namespace=default&type=pods", http.StatusOK)

	p.wantText("title", "Search - readout")
	// The redesign content root carries the `ro-rd` marker; the title is the
	// `.ro-title` (not the legacy `h1.title`).
	p.wantHas(".ro-rd")
	p.wantText(".search-hero .ro-title", "Search")

	// The `.search-big` GET form round-trips the query + the scope (cluster /
	// namespace / type) as hidden inputs so re-submitting keeps the scope. The
	// search is a read-only GET to /search.
	p.wantAttr(`.search-hero form[action="/search"]`, "method", "get")
	p.wantAttr(`.search-big input[name="q"]`, "value", "nginx")
	p.wantAttr(`.search-hero form input[type="hidden"][name="cluster"]`, "value", "test")
	p.wantAttr(`.search-hero form input[type="hidden"][name="namespace"]`, "value", "default")
	p.wantHas(`.search-hero form input[type="hidden"][name="type"][value="pods"]`)

	// Scope-opts line: a `.ok` cluster chip naming the single cluster + the
	// namespace + the type summary.
	p.wantText(".search-opts .ro-scope-chip.ok", "test")
	if opts := normSpace(p.text(".search-opts")); !strings.Contains(opts, "default") {
		t.Fatalf("search-opts line = %q, want it to name the namespace", opts)
	}

	// Per-cluster scope strip: with one healthy cluster the chip is `.ok` (no
	// `.err`, no partial-failure banner). The `.ok` chip names the cluster.
	p.wantAbsent(".ro-scope .ro-scope-chip.err")
	p.wantAbsent(".ro-banner.warn")
	if got := p.text(".ro-scope .ro-scope-chip.ok"); !strings.HasPrefix(got, "test") {
		t.Fatalf("scope `.ok` chip = %q, want it to start with the cluster name", got)
	}

	// Results table: the nginx row carries its Cluster + Namespace links, the Kind
	// cell pairs an icon with the kind name, the Name cell is the sticky object
	// link, and the Age column header is present.
	row := p.doc.Find(`.ro-table tbody tr:has(td.cell-name a[href="/clusters/test/namespaces/default/pods/nginx"])`)
	if row.Length() != 1 {
		t.Fatalf("expected exactly one nginx result row, found %d", row.Length())
	}
	if got := normSpace(row.Find("td.cell-clu a").Text()); got != "test" {
		t.Fatalf("result Cluster cell = %q, want test", got)
	}
	if got := normSpace(row.Find("td.cell-ns a").Text()); got != "default" {
		t.Fatalf("result Namespace cell = %q, want default", got)
	}
	if got := normSpace(row.Find("td .res-kind").Text()); got != "Pod" {
		t.Fatalf("result Kind cell text = %q, want Pod", got)
	}
	if row.Find("td .res-kind .ico, td .res-kind .kind-tile, td .res-kind svg").Length() == 0 {
		t.Fatalf("result Kind cell missing the resolved kind icon")
	}
	p.wantText(".ro-table thead th.num", "Age")

	// Foundline footer: the redesign `.ro-foundline` count sentence.
	if got := p.text(".ro-table-meta .ro-foundline"); !strings.Contains(got, `Found 1 object matching "nginx"`) {
		t.Fatalf("foundline = %q, want a 'Found 1 object matching \"nginx\"' sentence", got)
	}

	// Shell sidebar + navbar context render from the ?cluster=/?namespace= QUERY
	// (the /search route is param-less): the sidebar is built from the cluster
	// query value. With ?cluster=test&namespace=default the shell matches a
	// cluster-scoped page: the grouped resource-type menu, the Meta links, the
	// namespace dropdown, and the context pill.
	if labels := p.texts(".menu-label"); strings.Join(labels, "|") != "Cluster Resources|Controllers|Pod Management|Meta" {
		t.Fatalf("search sidebar menu-labels = %v", labels)
	}
	p.wantHas(`#aside-menu .menu-item[href="/clusters/test/namespaces/default/pods"]`)
	p.wantHas(`#aside-menu .menu-item[href="/clusters/test/namespaces/default/_resource-types"]`)
	p.wantText(".context-name", "default")
	if n := p.count(".namespace-item"); n != 3 {
		t.Fatalf("search navbar namespace-item count = %d, want 3", n)
	}
}

func TestSearchMultiNamespace(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	assertMultiNamespaceSearch := func(path string) {
		p := get(t, app, path, http.StatusOK)
		p.wantAttr(`.search-hero form input[type="hidden"][name="namespace"]`, "value", "default,states")
		if opts := normSpace(p.text(".search-opts")); !strings.Contains(opts, "2 namespaces") {
			t.Fatalf("search-opts line = %q, want it to name the multi-namespace scope", opts)
		}
		p.wantHas(`.ro-table tbody tr:has(td.cell-name a[href="/clusters/test/namespaces/default/pods/nginx"])`)
		p.wantHas(`.ro-table tbody tr:has(td.cell-name a[href="/clusters/test/namespaces/states/pods/web-creating-7c9f7cd495-6fff6"])`)
		p.wantAbsent(".ro-scope .ro-scope-chip.err")
		p.wantBodyExcludes("default%2Cstates")
	}

	assertMultiNamespaceSearch("/search?q=g&cluster=test&namespace=default&namespace=states&type=pods")
	assertMultiNamespaceSearch("/search?q=g&cluster=test&namespace=default,states&type=pods")
}

// TestSearchPartialFailure pins the SEARCH flavour of partial failure (D11): a
// multi-cluster search where one cluster's backend fails must still render the
// answering cluster's results, surface a `.ro-banner.warn` "Searched N of M
// clusters" summary, and mark the failed cluster with a `.ro-scope-chip.err`
// carrying an inline read-only `.retry` GET. The healthy cluster gets a `.ro-scope-chip.ok`.
// This is distinct from the all-cluster LIST banner (Unit 5); both are legitimate.
func TestSearchPartialFailure(t *testing.T) {
	good := newClusterFakeAPI(t, clusterFakeOptions{})
	bad := newClusterFakeAPI(t, clusterFakeOptions{failList: true})
	app := newMultiClusterServer(t, map[string]string{"good": good.URL, "zbad": bad.URL})

	p := get(t, app, "/search?q=nginx&cluster=_all&namespace=default&type=pods", http.StatusOK)

	// The answering cluster's results render (the failure is a partial, not a
	// whole-request failure).
	p.wantHas(`.ro-table tbody tr td.cell-name a[href="/clusters/good/namespaces/default/pods/nginx"]`)
	// The failed cluster contributed no result rows.
	p.wantAbsent(`.ro-table td.cell-name a[href^="/clusters/zbad/"]`)

	// Partial-failure banner: "Searched 1 of 2 clusters — 1 didn't respond".
	p.wantHas(".ro-banner.warn")
	if got := p.text(".ro-banner.warn .bn-title"); got != "Searched 1 of 2 clusters — 1 didn't respond" {
		t.Fatalf("partial banner title = %q", got)
	}
	// The banner's "Retry failed" action is a read-only GET re-running the search
	// scoped to the failed cluster(s) -- an <a> href, never a write form/button.
	retry := p.attr(".ro-banner.warn .bn-actions a", "href")
	if !strings.HasPrefix(retry, "/search?") || !strings.Contains(retry, "zbad") {
		t.Fatalf("banner retry href = %q, want a read-only /search GET scoped to zbad", retry)
	}

	// Per-cluster chips: the healthy cluster is `.ok`, the failed cluster is
	// `.err` and carries an inline `.retry` GET (also a /search re-run, not a write).
	okChips := p.texts(".ro-scope .ro-scope-chip.ok")
	if !containsPrefix(okChips, "good") {
		t.Fatalf("scope `.ok` chips = %v, want one naming the healthy cluster", okChips)
	}
	errChip := p.doc.Find(".ro-scope .ro-scope-chip.err")
	if errChip.Length() != 1 {
		t.Fatalf("expected exactly one `.err` scope chip, found %d", errChip.Length())
	}
	if got := normSpace(errChip.Text()); !strings.HasPrefix(got, "zbad") {
		t.Fatalf("`.err` scope chip = %q, want it to name the failed cluster zbad", got)
	}
	chipRetry, ok := errChip.Find("a.retry").Attr("href")
	if !ok || !strings.HasPrefix(chipRetry, "/search?") || !strings.Contains(chipRetry, "cluster=zbad") {
		t.Fatalf("`.err` chip retry href = %q (ok=%v), want a read-only /search GET scoped to cluster=zbad", chipRetry, ok)
	}

	// Foundline names the failed-cluster clause.
	if got := p.text(".ro-foundline"); !strings.Contains(got, "1 cluster failed") {
		t.Fatalf("foundline = %q, want it to note the failed cluster", got)
	}
}

// TestBehaviorSearchAllClustersNoSidebar pins the all-clusters search shell: a
// /search with NO ?cluster= (or ?cluster=_all) renders the rich body but NO
// cluster sidebar and NO navbar context pill -- the sidebar is built only when
// a cluster is set. This is the negative half of the cluster-from-query
// behavior: the scoped case grows a sidebar while the all-clusters case must
// stay sidebar-free.
func TestBehaviorSearchAllClustersNoSidebar(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	for _, path := range []string{
		"/search?q=nginx",
		"/search?q=nginx&cluster=_all",
	} {
		p := get(t, app, path, http.StatusOK)

		// The redesign body still renders (the search-hero + scope strip are present
		// for a query).
		p.wantText(".search-hero .ro-title", "Search")
		p.wantHas(".search-big input[name=\"q\"]")
		p.wantHas(".ro-scope")
		// No sidebar groups, no Meta links, no context pill.
		p.wantAbsent(".menu-label")
		p.wantAbsent(".menu-item")
		p.wantAbsent(".context-name")
	}

	first := newServerFakeAPI(t)
	second := newServerFakeAPI(t)
	multi := newMultiClusterServer(t, map[string]string{"first": first.URL, "second": second.URL})
	p := get(t, multi, "/search?q=nginx&cluster=first,second&namespace=default&type=pods", http.StatusOK)
	p.wantAbsent(".menu-label")
	p.wantAbsent(".menu-item")
	p.wantAbsent(".context-name")
}

// TestBehaviorSearchNoResults pins the redesign "no results" block + the
// foundline footer for a query that matches nothing: no results table, the
// `.ro-noresults` sentence, and a "Found 0 objects" foundline.
func TestBehaviorSearchNoResults(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/search?q=zzzznomatch&cluster=test&namespace=default&type=pods", http.StatusOK)

	p.wantAbsent(".ro-table tbody tr")
	p.wantText(".ro-noresults p", `No results found for "zzzznomatch".`)
	if got := p.text(".ro-foundline"); !strings.Contains(got, `Found 0 objects matching "zzzznomatch"`) {
		t.Fatalf("foundline = %q, want a 'Found 0 objects matching \"zzzznomatch\"' sentence", got)
	}
}

// TestBehaviorSearchRespectsExcludeNamespaces pins that search applies
// --exclude-namespaces (like the list path), so a result in an excluded
// namespace never appears. The fake API's pods all live in `default`; excluding
// `default` removes the nginx hit that the same query surfaces under the default
// config, proving the filter is wired into buildSearchView (not just present in
// kube). Without the exclude the row is present (asserted by TestSearchRender);
// with it, none.
func TestBehaviorSearchRespectsExcludeNamespaces(t *testing.T) {
	cfg := baseConfig(t)
	cfg.ExcludeNamespaces = []*regexp.Regexp{regexp.MustCompile(`^default$`)}
	app := newServer(t, cfg, time.Now())
	p := get(t, app, "/search?q=nginx&cluster=test&namespace=_all&type=pods", http.StatusOK)

	p.wantAbsent(".ro-table tbody tr")
	p.wantBodyExcludes("/clusters/test/namespaces/default/pods/nginx")
	p.wantText(".ro-noresults p", `No results found for "nginx".`)
}

// ---------------------------------------------------------------------------
// Preferences + error pages.
// ---------------------------------------------------------------------------

func TestBehaviorPreferencesPage(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/preferences", http.StatusOK)
	p.wantText("h1.title", "Preferences")
	p.wantAttr(`form.ro-prefs[action="/preferences"]`, "method", "post")
	p.wantHas(`select.ro-select[name="theme"]`)
}

func TestBehaviorErrorPages(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())

	// Missing cluster -> 500 with a GENERIC error-card body. Per D14 the raw error
	// detail (here the cluster name) is logged server-side, not rendered into
	// the client page, so the body must NOT leak "missing". Read the rendered
	// (entity-decoded) card text rather than the raw escaped body.
	miss := get(t, app, "/clusters/missing", http.StatusInternalServerError)
	miss.wantText("h2", "Internal Server Error")
	if got := miss.text("main .ro-error-card p"); !strings.Contains(got, "Internal server error") {
		t.Fatalf("error card text = %q, want generic internal-server-error body", got)
	}
	// The raw apiserver/Go error string (the leak D14 closes) must not appear
	// anywhere in the page. The bare cluster name still legitimately appears in
	// URL-derived chrome (navbar/sidebar links), so assert the full error
	// string is absent rather than the bare token.
	miss.wantBodyExcludes(`cluster "missing" not found`)
	miss.wantBodyExcludes("not found")

	// Missing object -> 404.
	ghost := get(t, app, "/clusters/test/namespaces/default/pods/ghost", http.StatusNotFound)
	ghost.wantText("h2", "Not Found")
}

// ---------------------------------------------------------------------------
// Secret barrier: default-OFF (Secret absent everywhere) AND masked-ON (detail/
// YAML/download/custom-column present but values masked, no raw bytes leak).
// Enforced in internal/kube/client.go:142 (Secret dropped from discovery when
// !includeSecrets).
// ---------------------------------------------------------------------------

func TestBehaviorSecretBarrierDefaultOff(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now()) // IncludeSecrets defaults false

	// Secret type absent from the namespaced resource-types list.
	rt := get(t, app, "/clusters/test/namespaces/default/_resource-types", http.StatusOK)
	if contains(rt.texts(".ro-cell-kind a"), "Secret") {
		t.Fatalf("Secret kind leaked into resource-types under default config")
	}

	// The secrets list route cannot resolve the type -> renders the partial
	// error notice, NOT a secret row. No secret name, no raw bytes.
	list := get(t, app, "/clusters/test/namespaces/default/secrets", http.StatusOK)
	list.wantBodyContains("resource type not found")
	list.wantAbsent("td.cell-name")
	list.wantBodyExcludes("my-secret")
	list.wantBodyExcludes(rawPassword)

	// Search for secrets yields no secret result row (type not discoverable); the
	// search surfaces the unresolved type as a per-cluster failure instead, and
	// never leaks a result row with the secret name.
	srch := get(t, app, "/search?q=my-secret&cluster=test&namespace=default&type=secrets", http.StatusOK)
	srch.wantBodyExcludes(rawPassword)
	srch.wantAbsent(".ro-table tbody tr")
	if contains(srch.texts(".ro-table td.cell-name a"), "my-secret") {
		t.Fatalf("secret leaked into search results under default config")
	}
}

func TestBehaviorSecretBarrierMaskedOn(t *testing.T) {
	app := newServer(t, withSecrets(t), time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC))

	// Now the Secret type is admitted into resource-types.
	rt := get(t, app, "/clusters/test/namespaces/default/_resource-types", http.StatusOK)
	if !contains(rt.texts(".ro-cell-kind a"), "Secret") {
		t.Fatalf("Secret kind should be admitted with IncludeSecrets=true")
	}

	// Secret DETAIL: masked-values notice + per-key mask, no raw base64 anywhere,
	// and the annotations are replaced with the hidden marker.
	detail := get(t, app, "/clusters/test/namespaces/default/secrets/my-secret", http.StatusOK)
	detail.wantText(".ro-detail-title .ro-kind-badge", "Secret")
	detail.wantHas(".ro-secret-data")
	detail.wantText(".ro-secret-data .ro-notice-title", "Values masked")
	keys := detail.texts(".ro-secret-key")
	if strings.Join(keys, "|") != "api-token|password" {
		t.Fatalf("secret data keys = %v, want [api-token password]", keys)
	}
	if n := detail.count(".ro-secret-mask"); n != 2 {
		t.Fatalf("expected 2 masked secret values, got %d", n)
	}
	detail.wantBodyExcludes(rawPassword)
	detail.wantBodyExcludes(rawToken)
	detail.wantBodyContains("annotations-hidden")

	// Secret YAML view: BOTH data values are the masked sentinel (the fixture has
	// exactly two keys), never the bytes. Counting the sentinel -- not just
	// "appears somewhere" -- catches a regression that masks one key but drops
	// the other, matching the detail half's count==2 strength.
	yamlView := get(t, app, "/clusters/test/namespaces/default/secrets/my-secret?view=yaml", http.StatusOK)
	if n := strings.Count(yamlView.rec.Body.String(), kube.SecretContentHidden); n != 2 {
		t.Fatalf("secret YAML view masked-value count = %d, want 2", n)
	}
	yamlView.wantBodyExcludes(rawPassword)
	yamlView.wantBodyExcludes(rawToken)

	// Secret YAML DOWNLOAD: same both-keys-masked payload (MIME asserted in the
	// targeted download test below).
	dl := httptest.NewRecorder()
	app.Handler().ServeHTTP(dl, httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/secrets/my-secret?download=yaml", nil))
	if dl.Code != http.StatusOK {
		t.Fatalf("secret download status = %d", dl.Code)
	}
	body := dl.Body.String()
	if n := strings.Count(body, kube.SecretContentHidden); n != 2 {
		t.Fatalf("secret download masked-value count = %d, want 2\nbody=%s", n, body)
	}
	if strings.Contains(body, rawPassword) || strings.Contains(body, rawToken) {
		t.Fatalf("secret download YAML leaked raw bytes: %s", body)
	}

	// Custom column over secret data is masked too (no raw bytes).
	cc := get(t, app, "/clusters/test/namespaces/default/secrets?custom-columns=Password=data.password", http.StatusOK)
	cc.wantBodyContains(kube.SecretContentHidden)
	cc.wantBodyExcludes(rawPassword)
}

// ---------------------------------------------------------------------------
// Targeted non-fact tests: MIME+body for TSV/YAML, metrics number formatting,
// and access-log default-on / NoAccessLogs-off. (OIDC sealing + hook links are
// covered by the existing server_test.go cases; not duplicated here.)
// ---------------------------------------------------------------------------

func TestBehaviorDownloadMIMEAndBody(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())

	tsv := httptest.NewRecorder()
	app.Handler().ServeHTTP(tsv, httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods?download=tsv", nil))
	if tsv.Code != http.StatusOK {
		t.Fatalf("tsv status = %d", tsv.Code)
	}
	if ct := tsv.Header().Get("Content-Type"); !strings.Contains(ct, "text/tab-separated-values") {
		t.Fatalf("tsv content-type = %q", ct)
	}
	if !strings.Contains(tsv.Header().Get("Content-Disposition"), ".tsv") {
		t.Fatalf("tsv missing attachment filename: %q", tsv.Header().Get("Content-Disposition"))
	}
	// Header row + a data row with the pod name, tab-separated.
	tsvBody := tsv.Body.String()
	if !strings.Contains(tsvBody, "Name\t") || !strings.Contains(tsvBody, "nginx") {
		t.Fatalf("tsv body shape wrong: %q", tsvBody)
	}

	yamlRec := httptest.NewRecorder()
	app.Handler().ServeHTTP(yamlRec, httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods/nginx?download=yaml", nil))
	if yamlRec.Code != http.StatusOK {
		t.Fatalf("yaml status = %d", yamlRec.Code)
	}
	if ct := yamlRec.Header().Get("Content-Type"); !strings.Contains(ct, "text/vnd.yaml") {
		t.Fatalf("yaml content-type = %q", ct)
	}
	if !strings.Contains(yamlRec.Body.String(), "kind: Pod") {
		t.Fatalf("yaml body missing kind: Pod")
	}
}

// TestBehaviorMetricsNumberFormatting pins the resource-quantity formatters the
// metrics join renders through (the `125m` CPU / `64 MiB` memory contract).
// These are the exact transforms a templ rewrite must preserve.
func TestBehaviorMetricsNumberFormatting(t *testing.T) {
	// CPU: cores -> milli (round to whole m).
	if got := cpuFormat(0.125); got != "125m" {
		t.Fatalf("cpuFormat(0.125) = %q, want 125m", got)
	}
	if got := cpuFormat(0.25); got != "250m" {
		t.Fatalf("cpuFormat(0.25) = %q, want 250m", got)
	}
	// Memory: bytes -> MiB (the template appends the " MiB" suffix).
	if got := memoryMiBFormat(float64(64 * 1024 * 1024)); got != "64" {
		t.Fatalf("memoryMiBFormat(64MiB) = %q, want 64", got)
	}
	if got := memoryMiBFormat(float64(128 * 1024 * 1024)); got != "128" {
		t.Fatalf("memoryMiBFormat(128MiB) = %q, want 128", got)
	}
	// Non-numeric input passes through unchanged.
	if got := cpuFormat("n/a"); got != "n/a" {
		t.Fatalf("cpuFormat(non-numeric) = %q, want n/a", got)
	}
}

// TestBehaviorAccessLogDefaultOnAndSuppressed pins the access-log behaviour: with
// the default config a request emits one slog "request" line (method/path/route/
// status), and with NoAccessLogs set it emits none. The middleware logs through
// the DEFAULT slog logger, so we swap in a buffer-backed handler for the
// duration of the test. Not parallel: it mutates the process-global default
// logger.
func TestBehaviorAccessLogDefaultOnAndSuppressed(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	render := func(t *testing.T, noAccessLogs bool) string {
		var buf bytes.Buffer
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
		cfg := config.Config{
			Port:         8080,
			Clusters:     []config.ClusterConnection{{Name: "test", Server: newServerFakeAPI(t).URL}},
			DefaultTheme: "dark",
			NoAccessLogs: noAccessLogs,
		}
		// Build directly via New so NoAccessLogs is honoured (newTestServerWithConfig
		// force-sets it true).
		app, err := New(context.Background(), &cfg)
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/clusters", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("clusters status = %d", rec.Code)
		}
		return buf.String()
	}

	on := render(t, false)
	if !strings.Contains(on, `msg=request`) || !strings.Contains(on, `path=/clusters`) || !strings.Contains(on, `status=200`) {
		t.Fatalf("default config did not emit an access log line: %q", on)
	}

	off := render(t, true)
	if strings.Contains(off, `msg=request`) {
		t.Fatalf("NoAccessLogs should suppress the access log line, got: %q", off)
	}
}

// TestBehaviorAPIVersionParamSnakeCase pins that apiVersionParam reads the
// resource-type pin from BOTH the camelCase `apiVersion` spelling and the
// snake_case `api_version` spelling, so either link resolves the same resource
// type and the common camelCase-only case is unchanged.
//
// The fake API serves apps/v1 with two namespaced kinds (deployments +
// replicasets), so the apiVersion pin is load-bearing: with it, FindResource
// must match the apps/v1 Deployment; with a NONEXISTENT version it must NOT
// resolve. That bad-version case is the regression guard -- if `api_version`
// were ignored, the pin would collapse to "" and `deployments` would resolve
// regardless, so the negative assertion would fail.
func TestBehaviorAPIVersionParamSnakeCase(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	cluster, ok := app.manager.Get("test")
	if !ok {
		t.Fatal("fake cluster \"test\" not registered")
	}
	client := cluster.Client
	ctx := context.Background()

	req := func(rawURL string) *http.Request {
		return httptest.NewRequest(http.MethodGet, rawURL, nil)
	}

	// Baseline: the camelCase spelling (Go's original) pins apps/v1 Deployment.
	camel, err := client.FindResource(ctx, "deployments", true, apiVersionParam(req("/?apiVersion=apps/v1")))
	if err != nil {
		t.Fatalf("?apiVersion=apps/v1 should resolve deployments: %v", err)
	}
	if camel.APIVersion != "apps/v1" || camel.Kind != "Deployment" {
		t.Fatalf("?apiVersion=apps/v1 resolved %s/%s, want apps/v1/Deployment", camel.APIVersion, camel.Kind)
	}

	// The snake_case spelling resolves the SAME resource type. The pin must be
	// non-empty for this to distinguish a version.
	snake, err := client.FindResource(ctx, "deployments", true, apiVersionParam(req("/?api_version=apps/v1")))
	if err != nil {
		t.Fatalf("?api_version=apps/v1 should resolve deployments: %v", err)
	}
	if snake.APIVersion != camel.APIVersion || snake.Kind != camel.Kind || snake.Plural != camel.Plural {
		t.Fatalf("snake_case resolved %s/%s (%s), camelCase resolved %s/%s (%s); spellings must pin the SAME type",
			snake.APIVersion, snake.Kind, snake.Plural, camel.APIVersion, camel.Kind, camel.Plural)
	}

	// Regression guard: a NONEXISTENT version via the snake_case spelling must
	// NOT resolve. This only holds if `api_version` is actually consumed -- if it
	// were ignored, the pin would be "" and `deployments` would resolve anyway.
	if rt, err := client.FindResource(ctx, "deployments", true, apiVersionParam(req("/?api_version=apps/v2"))); err == nil {
		t.Fatalf("?api_version=apps/v2 (nonexistent) must NOT resolve, but got %s/%s -- snake_case param is being ignored", rt.APIVersion, rt.Kind)
	}

	// Precedence: when BOTH spellings are present, camelCase wins, so the common
	// case (a request that sets `apiVersion`) keeps its exact current behavior
	// regardless of any stray `api_version`.
	if got := apiVersionParam(req("/?apiVersion=apps/v1&api_version=bogus/v9")); got != "apps/v1" {
		t.Fatalf("apiVersionParam with both spellings = %q, want camelCase \"apps/v1\"", got)
	}
	// And with neither spelling, the pin is empty (plural-only resolution).
	if got := apiVersionParam(req("/?selector=app=nginx")); got != "" {
		t.Fatalf("apiVersionParam with neither spelling = %q, want \"\"", got)
	}
}

// ---------------------------------------------------------------------------
// small slice helpers (test-local; keep the fact assertions readable)
// ---------------------------------------------------------------------------

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// containsPrefix reports whether any element of haystack starts with prefix.
// Used for the search scope chips, whose text is "<cluster> · <count>" (so an
// exact match would have to spell out the count).
func containsPrefix(haystack []string, prefix string) bool {
	for _, s := range haystack {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

func assertContainsAll(t *testing.T, what string, haystack []string, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if !contains(haystack, n) {
			t.Fatalf("%s: missing %q in %v", what, n, haystack)
		}
	}
}
