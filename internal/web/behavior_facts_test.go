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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
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

	// hx-boost body shell + preload ext.
	p.wantAttr("body", "hx-boost", "true")
	p.wantAttr("body", "hx-ext", "preload")

	// navbar-burger / aside-burger toggles name their target via data-target;
	// readout.js toggles is-active on #<data-target>.
	p.wantAttr("a.navbar-burger", "data-target", "nav-menu")
	p.wantHas("#nav-menu")
	p.wantAttr("a.aside-burger", "data-target", "aside-menu")
	p.wantHas("#aside-menu")
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

func TestBehaviorCommandPaletteTemplate(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	// The palette ships hidden on every page; assert it on the clusters page so
	// the fact does not depend on any list data.
	p := get(t, app, "/clusters", http.StatusOK)

	// Outer overlay: hidden via the Bulma is-hidden CLASS (not inline style), a
	// dialog. readout.js toggles is-active to show it.
	p.wantHas("#ro-palette.ro-palette.is-hidden")
	p.wantAttr("#ro-palette", "role", "dialog")
	p.wantAttr("#ro-palette", "aria-hidden", "true")

	// The backdrop carries the close marker the click handler keys off.
	p.wantAttr(".ro-palette-backdrop", "data-palette-close", "true")

	// Query box + list + empty-state ids the input/filter handlers resolve.
	p.wantAttr("#ro-palette-input", "role", "combobox")
	p.wantAttr("#ro-palette-list", "role", "listbox")
	p.wantHas("#ro-palette-empty.is-hidden")

	// The row <template>: renderPaletteTargets clones #ro-palette-row-tmpl and
	// fills .ro-palette-tag / .ro-palette-label / .ro-palette-path on the
	// real <a class="ro-palette-row" role="option">. goquery does not descend
	// into <template> content for normal Find, so address the template by id and
	// parse its inner HTML.
	tmpl := p.doc.Find("template#ro-palette-row-tmpl")
	if tmpl.Length() != 1 {
		t.Fatalf("expected exactly one #ro-palette-row-tmpl, got %d", tmpl.Length())
	}
	inner, err := tmpl.Html()
	if err != nil {
		t.Fatalf("read template html: %v", err)
	}
	// Assert the JS contract structurally, not as adjacent-attribute substrings:
	// goquery/x/net does not preserve source attribute order when it re-serializes
	// the <template> inner HTML, so a textual `class="..." role="..."` match is
	// brittle across x/net versions. Re-parse the fragment and check each element
	// carries its required class (and the row its role) on the element itself.
	frag, err := goquery.NewDocumentFromReader(strings.NewReader(inner))
	if err != nil {
		t.Fatalf("parse palette row template fragment: %v", err)
	}
	row := frag.Find("a.ro-palette-row")
	if row.Length() != 1 || row.AttrOr("role", "") != "option" {
		t.Fatalf("palette row needs one <a class=ro-palette-row role=option>\ntemplate=%s", inner)
	}
	for _, sel := range []string{
		"li.ro-palette-item",
		"span.ro-ktag.ro-palette-tag",
		"span.ro-palette-label",
		"span.ro-palette-path",
	} {
		if frag.Find(sel).Length() != 1 {
			t.Fatalf("palette row template missing %q\ntemplate=%s", sel, inner)
		}
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
	p.wantText("h1.title", "Clusters 1")

	// Exactly one cluster row ("test"), addressed by its ro-cell-name selector.
	if got := p.texts("td.ro-cell-name"); strings.Join(got, "|") != "test" {
		t.Fatalf("cluster rows = %v, want [test]", got)
	}
	p.wantAttr("td.ro-cell-name a", "href", "/clusters/test")

	// The clusters page must NOT render the sidebar resource labels (check_clusters).
	p.wantAbsent(".menu-label")

	// Search-select contract: a per-row checkbox carries data-toggle-button
	// pointing at the (initially disabled) search button.
	p.wantAttr(`input[type="checkbox"][name="cluster"]`, "data-toggle-button", "search-clusters-button")
	p.wantHas("#search-clusters-button[disabled]")
}

// ---------------------------------------------------------------------------
// Cluster overview: the namespaces table (cluster-scoped kind WITH rendered
// rows + per-bucket age cell classes) and the cluster resource-types table.
// ---------------------------------------------------------------------------

func TestBehaviorClusterOverview(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Date(2024, 1, 3, 6, 0, 0, 0, time.UTC))
	p := get(t, app, "/clusters/test", http.StatusOK)

	p.wantText("title", "test Cluster - readout")
	p.wantText("h1.title", "test")
	p.wantText("h4.title.is-5:first-of-type", "Namespaces")

	// Namespace rows: cell VALUES + the search-select data-toggle-button.
	if got := p.texts("td.ro-cell-name"); strings.Join(got, "|") != "default|kube-system|my-app" {
		t.Fatalf("namespace rows = %v, want [default kube-system my-app]", got)
	}
	p.wantAttr(`input[type="checkbox"][name="namespace"]`, "data-toggle-button", "search-namespaces-button")
	p.wantHas("#search-namespaces-button[disabled]")
	// Clicking a namespace drops into its pods (the redesign contract).
	p.wantAttr("td.ro-cell-name a", "href", "/clusters/test/namespaces/default/pods")

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
// with the CRD badge), plus the cluster/namespaced split and the Namespaced
// boolean cells.
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

	// Namespaced boolean pill bound to the ROW's flag (not just "some pill
	// exists"): a known namespaced row (Deployment) carries ro-bool-yes, and on
	// the cluster page a known cluster-scoped row (Node) carries ro-bool-no. An
	// inverted boolean would break one of these.
	deployBool := p.doc.Find(`tr:has(a[href="/clusters/test/namespaces/default/deployments"])`)
	if deployBool.Find(".ro-bool-yes").Length() != 1 || deployBool.Find(".ro-bool-no").Length() != 0 {
		t.Fatalf("Deployment (namespaced) row should carry ro-bool-yes, not ro-bool-no")
	}
	pc := get(t, app, "/clusters/test/_resource-types", http.StatusOK)
	// CSINode has a unique href (Node + NodeMetrics both link to /nodes, so that
	// href matches two rows); CSINode is cluster-scoped, so its row carries
	// ro-bool-no and never ro-bool-yes.
	csiBool := pc.doc.Find(`tr:has(a[href="/clusters/test/csinodes"])`)
	if csiBool.Find(".ro-bool-no").Length() != 1 || csiBool.Find(".ro-bool-yes").Length() != 0 {
		t.Fatalf("CSINode (cluster-scoped) row should carry ro-bool-no, not ro-bool-yes")
	}
	// Cluster tab vs Namespaced tab active state.
	pc.wantText(".ro-rt-tabs li.is-active a", "Cluster")
	p.wantText(".ro-rt-tabs li.is-active a", "Namespaced")
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

	// #resource-list-content htmx wiring (the live-refresh contract readout.js
	// fires ro:refresh against). Every attribute pinned by exact value.
	p.wantAttr("#resource-list-content", "hx-get", "/clusters/test/namespaces/default/pods/_table")
	p.wantAttr("#resource-list-content", "hx-trigger", "ro:refresh")
	p.wantAttr("#resource-list-content", "hx-target", "this")
	p.wantAttr("#resource-list-content", "hx-ext", "morph")
	p.wantAttr("#resource-list-content", "hx-swap", "morph:innerHTML")
	p.wantAttr("#resource-list-content", "hx-indicator", "previous .ro-progress")

	// Column headers in order, each linking to its own ?sort=<col>.
	headers := p.texts("table.ro-list-table thead th")
	if strings.Join(headers, "|") != "Name|Ready|Status|Restarts|Age|Created" {
		t.Fatalf("pod table headers = %v", headers)
	}
	for _, col := range []string{"Name", "Ready", "Status", "Restarts", "Age", "Created"} {
		want := "/clusters/test/namespaces/default/pods?sort=" + col
		if !p.containsHref("thead th a", want) {
			t.Fatalf("missing column sort href %q among %v", want, p.attrs("thead th a", "href"))
		}
	}

	// Phase strip: the Running tally.
	p.wantText(".ro-phase-label", "Running")
	p.wantText(".ro-phase-strip .ro-phase-tally .ro-phase-count", "2")

	// Row name cells + their detail links (cell VALUES, not just headers).
	names := p.texts("td.ro-cell-name")
	if strings.Join(names, "|") != "nginx|my-app" {
		t.Fatalf("pod name cells = %v, want [nginx my-app]", names)
	}
	p.wantAttr("td.ro-cell-name a", "href", "/clusters/test/namespaces/default/pods/nginx")

	// The nginx row's Status cell value + success class + the status dot.
	nginxRow := p.doc.Find(`tr:has(a[href="/clusters/test/namespaces/default/pods/nginx"])`)
	if got := normSpace(nginxRow.Find("td").Eq(2).Text()); got != "Running" {
		t.Fatalf("nginx Status cell = %q, want Running", got)
	}
	if nginxRow.Find(".ro-status-dot.has-text-success").Length() == 0 {
		t.Fatalf("nginx Status cell missing success status dot")
	}
	// Ready cell value 1/1 wrapped in has-text-success.
	if got := normSpace(nginxRow.Find("td .has-text-success").First().Text()); got != "1/1" {
		t.Fatalf("nginx Ready cell = %q, want 1/1", got)
	}

	// "Show CPU/Memory Usage" affordance (join=metrics not yet applied).
	p.wantAttr(`a[href="/clusters/test/namespaces/default/pods?join=metrics"]`, "href", "/clusters/test/namespaces/default/pods?join=metrics")

	// The tools form carries the labelcols / selector / filter inputs the
	// delegated submit handler blanks-when-empty.
	p.wantHas(`form.tools-form input[name="labelcols"]`)
	p.wantHas(`form.tools-form input[name="selector"]`)
	p.wantHas(`form.tools-form input[name="filter"]`)
}

// TestBehaviorPodListSortToggle pins the descending-toggle behaviour: with
// ?sort=Name the Name header flips to ?sort=Name:desc and grows a sort icon,
// while the other headers keep their plain ascending sort href. The row order
// also flips (my-app before nginx) -- a cell-value fact, not just a header href.
func TestBehaviorPodListSortToggle(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/namespaces/default/pods?sort=Name", http.StatusOK)

	// hx-get carries the sort through to the partial refresh URL.
	p.wantAttr("#resource-list-content", "hx-get", "/clusters/test/namespaces/default/pods/_table?sort=Name")

	// The Name header now points at the :desc toggle (percent-encoded colon) and
	// carries a sort icon; goquery returns the href HTML-decoded (&amp;->&) but
	// keeps %3A literal.
	nameHeader := p.doc.Find(`thead th:has(a) a:contains("Name")`).First()
	href, _ := nameHeader.Attr("href")
	if href != "/clusters/test/namespaces/default/pods?sort=Name%3Adesc" {
		t.Fatalf("Name header href = %q, want ...?sort=Name%%3Adesc", href)
	}
	if nameHeader.Find(".icon").Length() == 0 {
		t.Fatalf("active sort column should render a sort icon")
	}
	// A non-active column keeps the plain ascending sort.
	if !p.containsHref("thead th a", "/clusters/test/namespaces/default/pods?sort=Ready") {
		t.Fatalf("non-active column lost its plain ascending sort href")
	}

	// Ascending name sort: my-app sorts before nginx (row-order cell fact).
	names := p.texts("td.ro-cell-name")
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
// Namespace column appears, the breadcrumb shows "all", and the partial URL
// targets the _all path.
func TestBehaviorPodListAllNamespaces(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/namespaces/_all/pods", http.StatusOK)

	p.wantAttr("#resource-list-content", "hx-get", "/clusters/test/namespaces/_all/pods/_table")
	headers := p.texts("table.ro-list-table thead th")
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
		p.wantAttr(`form.tools-form input[name="labelcols"]`, "value", "app")
		p.wantHas("form.tools-form.is-active")
		if !contains(p.texts("thead th"), "App") {
			t.Fatalf("labelcols=app did not add an App column: %v", p.texts("thead th"))
		}
	})

	t.Run("selector round-trips into the selector input", func(t *testing.T) {
		p := get(t, app, "/clusters/test/namespaces/default/pods?selector=app%3Dnginx", http.StatusOK)
		p.wantAttr(`form.tools-form input[name="selector"]`, "value", "app=nginx")
		p.wantAttr("#resource-list-content", "hx-get", "/clusters/test/namespaces/default/pods/_table?selector=app%3Dnginx")
	})

	t.Run("filter narrows rows and round-trips into the filter input", func(t *testing.T) {
		p := get(t, app, "/clusters/test/namespaces/default/pods?filter=nginx", http.StatusOK)
		p.wantAttr(`form.tools-form input[name="filter"]`, "value", "nginx")
		if got := p.texts("td.ro-cell-name"); strings.Join(got, "|") != "nginx" {
			t.Fatalf("filter=nginx rows = %v, want [nginx]", got)
		}
		p.wantText(".ro-phase-strip .ro-phase-tally .ro-phase-count", "1")
	})

	t.Run("a filter matching nothing renders the empty-state row", func(t *testing.T) {
		// Pins the empty-state sentence "No <Kind> objects in namespace "<ns>"
		// found." verbatim (the ro-empty-row colspan cell). This guards the templ
		// empty-state against the @templ.Raw children-drop that would silently lose
		// the trailing "found." -- a regression the row-bearing facts cannot see.
		p := get(t, app, "/clusters/test/namespaces/default/pods?filter=zzz-no-such-pod", http.StatusOK)
		p.wantAbsent("td.ro-cell-name")
		p.wantText(".ro-empty-row .ro-empty-title", `No Pod objects in namespace "default" found.`)
		p.wantText(".ro-empty-row .ro-empty-hint", "Nothing to show here yet.")
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
		p.wantAttr("#resource-list-content", "hx-get", "/clusters/test/namespaces/default/pods/_table?custom-columns=Image=spec.containers[0].image")
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
	if got := p.texts("td.ro-cell-name"); strings.Join(got, "|") != "nginx|my-app" {
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
	p.wantText("h1.title .ro-kind-badge", "Pod")
	p.wantAttr(`h1.title a[title="Download resource object as YAML"]`, "href", "/clusters/test/namespaces/default/pods/nginx?download=yaml")

	// Detail tabs: Default active, YAML + Logs present.
	tabs := p.texts(".ro-detail-tabs li a")
	if strings.Join(tabs, "|") != "Default|YAML|Logs" {
		t.Fatalf("detail tabs = %v", tabs)
	}
	p.wantText(".ro-detail-tabs li.is-active a", "Default")

	// Summary sections (check_detail-style): Spec + Status YAML cards + the
	// Events collapsible carrying data-name="events".
	cardNames := p.attrs(".ro-yaml-card", "data-name")
	assertContainsAll(t, "yaml card sections", cardNames, "spec", "status")
	p.wantHas(`.collapsible[data-name="events"]`)
	// The Events subtable renders the fixture's Scheduled event (cell values).
	eventsTable := p.doc.Find(`.collapsible[data-name="events"] table`)
	if !strings.Contains(eventsTable.Text(), "Successfully assigned default/nginx") {
		t.Fatalf("events table missing the Scheduled event: %q", eventsTable.Text())
	}
	// Event-row CELL CLASSES are pinned per cell. For the fixture's single event
	// (type=Normal, reason=Scheduled, lastTimestamp 2024-03 so it is age-old
	// under time.Now(), with a message):
	//  - Message cell carries the static ro-event-msg class.
	//  - Type cell carries a ro-status-dot span (the type tone is "" for Normal,
	//    so the span class is exactly "ro-status-dot ").
	//  - Reason cell carries the events/Reason/Scheduled cell class
	//    has-text-success.
	//  - Age cell carries the age class for lastTimestamp at days=1 == age-old.
	eventRow := p.doc.Find(`.collapsible[data-name="events"] tbody tr`)
	if eventRow.Length() != 1 {
		t.Fatalf("expected exactly one event row, got %d", eventRow.Length())
	}
	if got := normSpace(eventRow.Find("td.ro-event-msg").Text()); got != "Successfully assigned default/nginx to 127.0.0.1" {
		t.Fatalf("event message cell (td.ro-event-msg) = %q, want the Scheduled message", got)
	}
	if eventRow.Find("td .ro-status-dot").Length() != 1 {
		t.Fatalf("event Type cell missing its ro-status-dot span")
	}
	if reasonCls, _ := eventRow.Find("td:nth-child(2)").Attr("class"); reasonCls != "has-text-success" {
		t.Fatalf("event Reason cell class = %q, want has-text-success (get_cell_class events/Reason/Scheduled)", reasonCls)
	}
	if ageCls, _ := eventRow.Find("td:nth-child(3)").Attr("class"); ageCls != "age-old" {
		t.Fatalf("event Age cell class = %q, want age-old (age_color lastTimestamp days=1)", ageCls)
	}
	// Labels + the generic annotation surface.
	p.wantBodyContains("generic-annotation")

	// Each YAML card carries a per-section copy button (readout.js .ro-copy-btn).
	p.wantHas(".ro-yaml-card .ro-copy-btn")
}

func TestBehaviorPodYAMLViewIDScheme(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/namespaces/default/pods/nginx?view=yaml", http.StatusOK)

	p.wantText(".ro-detail-tabs li.is-active a", "YAML")

	// Pygments-compatible highlight table: linenos column + code column.
	p.wantHas("table.highlighttable td.linenos")
	p.wantHas("table.highlighttable td.code")

	// The per-line span id scheme readout.js keys off: getElementById('yaml-'+id)
	// and buildYamlFolds() both require `yaml-line-N` ids in the code cell, with
	// matching `#line-N` gutter anchors. (The YAML view uses an empty prefix.)
	p.wantHas("td.code pre span#yaml-line-1")
	p.wantHas("td.code pre span#yaml-line-2")
	p.wantHas(`td.linenos a[href="#line-1"]`)

	// Body fact: apiVersion + kind are present (check_yaml).
	p.wantBodyContains("apiVersion")
	if !strings.Contains(p.text("span#yaml-line-2"), "Pod") {
		t.Fatalf("YAML line 2 should render kind: Pod, got %q", p.text("span#yaml-line-2"))
	}
}

func TestBehaviorNodeDetailFacts(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/nodes/worker-1", http.StatusOK)
	p.wantText("h1.title .ro-kind-badge", "Node")
	// System info renders + the field-selected pods subtable carries data-name.
	p.wantBodyContains("kubeletVersion")
	p.wantBodyContains("v1.29.2")
	p.wantHas(`[data-name="pods"]`)

	// Node summary blocks (named facts replacing the retired TestRenderNodeSummary
	// byte asserts). The three section labels are present, in order.
	sectionLabels := p.texts(".ro-section .ro-section-label")
	assertContainsAll(t, "node section labels", sectionLabels, "Conditions", "Capacity / Allocatable", "System Info")

	// Condition pills: the Ready=True pill carries the ok tone + the dot + the
	// name/value spans. nodeConditionTone(Ready,True)=ro-st-ok; the fixture's
	// pressure conditions are False so they are ok too -- assert the Ready pill
	// precisely (tone bound to the condition, not "some pill exists").
	readyPill := p.doc.Find(`.ro-cond-pill:has(.ro-cond-name:contains("Ready"))`)
	if readyPill.Length() != 1 {
		t.Fatalf("expected exactly one Ready condition pill, got %d", readyPill.Length())
	}
	if cls, _ := readyPill.Attr("class"); !strings.Contains(cls, "ro-st-ok") {
		t.Fatalf("Ready=True pill tone = %q, want ro-st-ok", cls)
	}
	if normSpace(readyPill.Find(".ro-cond-val").Text()) != "True" {
		t.Fatalf("Ready pill value = %q, want True", normSpace(readyPill.Find(".ro-cond-val").Text()))
	}
	if readyPill.Find(".ro-status-dot").Length() != 1 {
		t.Fatalf("Ready pill missing its status dot")
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

// TestBehaviorDetailLabelChips pins the resource-view label/annotation chips
// (named facts replacing the retired renderObjectSummary byte asserts in
// TestObjectRenderingLinksAndSearchHelpers): each label is an anchor to the
// selector-filtered list with the ro-chip class, the selector value kept
// literal (key=value, NOT url-encoded), and the app.kubernetes.io/* accent on
// the matching key. The annotations render as non-link ro-chip-trunc pills.
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
	// The chip links to the namespaced list filtered by selector=key=value, with
	// the '=' kept literal (goquery decodes the attribute; a %3D here would be the
	// double-encoding regression this fact guards against).
	if !strings.HasPrefix(href, "/clusters/test/namespaces/default/pods?selector=") || strings.Contains(href, "%3D") {
		t.Fatalf("label chip href = %q, want a literal selector=key=value link", href)
	}
	// The nginx pod fixture carries app=nginx; that chip's exact selector href.
	appChip := p.doc.Find(`.ro-chips a[href="/clusters/test/namespaces/default/pods?selector=app=nginx"]`)
	if appChip.Length() != 1 {
		t.Fatalf("expected the app=nginx label chip with a literal selector href, hrefs=%v", p.attrs(".ro-chips a.ro-chip", "href"))
	}
	p.wantText("a.ro-chip .tag.is-link", "app: nginx")

	// Annotations render as non-link truncated pills (the fixture's
	// example.com/note: generic-annotation), replacing the body-contains check.
	p.wantText(`.ro-section:has(.ro-section-label:contains("Annotations")) .ro-section-label`, "Annotations")
	annText := p.texts("span.tag.ro-chip.ro-chip-trunc")
	assertContainsAll(t, "annotation chips", annText, "example.com/note: generic-annotation")
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
	p.wantText(".tabs li.is-active a", "Logs")
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
// Search: the rich /search body -- the GET form with resource-type checkboxes,
// the scope chips, the result cards, the count footer, and the per-cluster
// error articles.
// ---------------------------------------------------------------------------

// TestBehaviorSearchRichRender pins the rich search structure: the resource-type
// checkboxes + the .unselect control, the scope chips, the result CARDS (kind
// tag + title + meta + snippet <em> highlight + label chips), the count footer
// with "repeat across all namespaces", and a per-cluster `message is-danger`
// error article.
//
// The request asks for two types: `pods` (a resolvable namespaced type with the
// nginx fixture row) and `deployments` (advertised in apps/v1 discovery but with
// NO list handler in the fake API, so its Table request 404s) -- so a single GET
// drives BOTH a result card AND a per-cluster error record (partial failure).
func TestBehaviorSearchRichRender(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/search?q=nginx&cluster=test&namespace=default&type=pods&type=deployments", http.StatusOK)

	p.wantText("title", "Search - readout")
	p.wantText("h1.title", "Search")

	// Tools-form: the q box round-trips the query; the resource-type checkbox
	// list + the .unselect control render.
	p.wantAttr(`form.tools-form[action="/search"] input[name="q"]`, "value", "nginx")
	p.wantHas("#type-checkboxes")
	p.wantHas(`.unselect[data-target="type-checkboxes"]`)
	if n := p.count(`#type-checkboxes input[type="checkbox"]`); n < 2 {
		t.Fatalf("expected the offered resource-type checkboxes, found %d", n)
	}
	// pods is the requested type, so its checkbox is checked.
	p.wantHas(`#type-checkboxes input[type="checkbox"][value="pods"][checked]`)

	// Scope chips: the cluster chip + the "type N" muted chip.
	scope := p.texts(".ro-scope-chip")
	if !contains(scope, "test") {
		t.Fatalf("scope chips missing cluster chip: %v", scope)
	}
	if !contains(scope, "type 2") {
		t.Fatalf("scope chips missing type count chip: %v", scope)
	}

	// Result card: the kind tag (PO), the title link, the kind/path meta, and at
	// least one snippet with the <em> highlight on the matched text.
	p.wantText(".search-result .ro-ktag", "PO")
	p.wantAttr(".search-result .ro-ktag", "title", "Pod")
	p.wantAttr(".search-result .r-title", "href", "/clusters/test/namespaces/default/pods/nginx")
	p.wantText(".search-result .r-title", "nginx")
	p.wantText(".search-result .r-meta .r-kind", "pod")
	p.wantAttr(".search-result .r-meta .r-path", "href", "/clusters/test/namespaces/default/pods/nginx")
	p.wantHas(".search-result .r-snip.match em")
	if got := p.text(".search-result .r-snip.match em"); got != "nginx" {
		t.Fatalf("snippet <em> highlight = %q, want nginx", got)
	}
	// Label chip from the pod's app=nginx label.
	p.wantHas(".search-result .ro-chips.r-labels .ro-chip")
	p.wantText(".search-result .ro-chips.r-labels .tag.is-link", "app: nginx")

	// Count footer: the "repeat across all namespaces" link + the count sentence.
	p.wantHas(`.ro-search-count a[href*="namespace="]`)
	if got := p.text(`.ro-search-count a`); got != "Repeat search across all namespaces" {
		t.Fatalf("repeat-all-namespaces link text = %q", got)
	}
	p.wantBodyContains("Searched 2 resource types in 1 cluster")

	// Per-cluster error article: the failing `deployments` Table surfaces as a
	// `message is-danger` article naming the cluster + the failed resource type.
	p.wantHas("article.message.is-danger")
	p.wantText("article.message.is-danger .message-header p", "Error for cluster test")
	if got := p.text("article.message.is-danger .message-body p"); !strings.HasPrefix(got, "Failed to search deployments:") {
		t.Fatalf("per-cluster error line = %q, want a 'Failed to search deployments:' message", got)
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

// TestBehaviorSearchAllClustersNoSidebar pins the all-clusters search shell: a
// /search with NO ?cluster= (or ?cluster=_all) renders the rich body but NO
// cluster sidebar and NO navbar context pill -- the sidebar is built only when
// a cluster is set. This is the negative half of the cluster-from-query
// behavior: the scoped case grows a sidebar while the all-clusters case must
// stay sidebar-free.
func TestBehaviorSearchAllClustersNoSidebar(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/search?q=nginx", http.StatusOK)

	// The body still renders (scope chip strip is present for a query).
	p.wantText("h1.title", "Search")
	p.wantHas("#type-checkboxes")
	// No sidebar groups, no Meta links, no context pill.
	p.wantAbsent(".menu-label")
	p.wantAbsent(".menu-item")
	p.wantAbsent(".context-name")
}

// TestBehaviorSearchNoResults pins the "no results" block + the count footer for a
// query that matches nothing, and confirms the type checkboxes still render.
func TestBehaviorSearchNoResults(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/search?q=zzzznomatch&cluster=test&namespace=default&type=pods", http.StatusOK)

	p.wantAbsent(".search-result")
	p.wantText(".ro-noresults p", `No results found for "zzzznomatch".`)
	p.wantHas("#type-checkboxes")
	p.wantBodyContains("0 results found.")
}

// TestBehaviorSearchRespectsExcludeNamespaces pins that search applies
// --exclude-namespaces (like the list path), so a result in an excluded
// namespace never appears. The fake API's pods all live in `default`; excluding
// `default` removes the nginx hit that the same query surfaces under the default
// config, proving the filter is wired into buildSearchView (not just present in
// kube). Without the exclude the card is present (asserted by
// TestBehaviorSearchRichRender); with it, none.
func TestBehaviorSearchRespectsExcludeNamespaces(t *testing.T) {
	cfg := baseConfig(t)
	cfg.ExcludeNamespaces = []*regexp.Regexp{regexp.MustCompile(`default`)}
	app := newServer(t, cfg, time.Now())
	p := get(t, app, "/search?q=nginx&cluster=test&namespace=_all&type=pods", http.StatusOK)

	p.wantAbsent(".search-result")
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
	p.wantAttr(`form.box[action="/preferences"]`, "method", "post")
	p.wantHas(`select[name="theme"]`)
}

func TestBehaviorErrorPages(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())

	// Missing cluster -> 500 with a GENERIC panel body. Per D14 the raw error
	// detail (here the cluster name) is logged server-side, not rendered into
	// the client page, so the body must NOT leak "missing". Read the rendered
	// (entity-decoded) panel text rather than the raw escaped body.
	miss := get(t, app, "/clusters/missing", http.StatusInternalServerError)
	miss.wantText("h2", "Internal Server Error")
	if got := miss.text("main .panel p"); !strings.Contains(got, "Internal server error") {
		t.Fatalf("error panel text = %q, want generic internal-server-error body", got)
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
	list.wantAbsent("td.ro-cell-name")
	list.wantBodyExcludes("my-secret")
	list.wantBodyExcludes(rawPassword)

	// Search for secrets yields no secret result card (type not discoverable);
	// the rich search surfaces the unresolved type as a per-cluster error
	// article instead, and never leaks a result card with the secret name.
	srch := get(t, app, "/search?q=my-secret&cluster=test&namespace=default&type=secrets", http.StatusOK)
	srch.wantBodyExcludes(rawPassword)
	srch.wantAbsent(".search-result")
	if contains(srch.texts(".search-result .r-title"), "my-secret") {
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
	detail.wantText("h1.title .ro-kind-badge", "Secret")
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
			Clusters:     map[string]string{"test": newServerFakeAPI(t).URL},
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

func assertContainsAll(t *testing.T, what string, haystack []string, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if !contains(haystack, n) {
			t.Fatalf("%s: missing %q in %v", what, n, haystack)
		}
	}
}
