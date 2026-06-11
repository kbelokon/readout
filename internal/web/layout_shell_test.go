package web

// layout_shell_test.go pins the redesign page-shell chrome (Unit 3): the
// blurred sticky topbar emitted as <header class="ro-topbar">, the grouped
// sticky sidebar (.ro-sidebar) whose entries carry a resolved kind icon and an
// .is-active marker on the current path, the .ro-shell grid wrapper with
// .ro-main, the clusters entry page that omits the sidebar + namespace context
// (D11), and the server-emitted #ro-palette-data JSON blob (D10) that the ⌘K
// palette (Unit 4) consumes. These facts certify what the layout emits today,
// read off its own output via goquery; they stand alongside the chrome facts in
// behavior_facts_test.go and survive attribute reordering.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// TestShellTopbarChrome pins the redesign topbar: it is a <header class="ro-topbar">
// (NOT a <nav>, per D13 chrome scoping), carries the brand mask + name, the
// read-only ⌘K search box, the live-refresh control with its label, and the
// server-POST theme toggle (hx-boost="false", D5) -- never a client-JS toggle.
func TestShellTopbarChrome(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Date(2024, 1, 3, 6, 0, 0, 0, time.UTC))
	p := get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)

	// The redesign chrome marker: the topbar is a <header class="ro-topbar"> so
	// the header.ro-topbar CSS applies. There must be no leftover <nav class="navbar">.
	p.wantHas("header.ro-topbar")
	p.wantAbsent("nav.navbar")

	// Brand chip + mask + name: the SPEC 2.5 .brand-chip wrapper carries the
	// dark chip behind the .brand-logo CSS mask (which inherits --brand).
	p.wantHas("header.ro-topbar .brand-item .brand-chip .brand-logo")
	p.wantText("header.ro-topbar .brand-name", "readout")

	// The body opts into the fixed-topbar offset class for the redesign shell.
	p.wantHas("body.has-ro-topbar")

	// Read-only search box: clicking it opens ⌘K (Unit 4); it carries the ⌘K hint
	// and a data hook the palette JS keys off, and is NOT a submitting <form>.
	p.wantHas("header.ro-topbar .ro-search input")
	p.wantHas("header.ro-topbar .ro-search .kbd-hint .ro-kbd")
	p.wantAttr("header.ro-topbar .ro-search", "data-ro-palette-open", "true")

	// Refresh control: the five interval options (10 replaced 15 per D18/SPEC
	// §8.3) plus the Live mode (Unit 27/D19), the #refresh-label, the
	// #refresh-dropdown hook.
	if got := p.attrs("#refresh-dropdown .refresh-option", "data-ro-interval"); strings.Join(got, ",") != "0,5,10,30,60,Live" {
		t.Fatalf("refresh-option data-ro-interval set = %v, want [0 5 10 30 60 Live]", got)
	}
	p.wantHas(".tb-group .tb-btn.refresh-trigger")
	p.wantHas(".tb-group .tb-btn.refresh-trigger .ro-livedot")
	// The livedot's live state has exactly ONE owner: `refresh-on` on
	// #refresh-dropdown (SSR refreshDropdownClass + JS syncRefreshUI). The old
	// static `refresh-live` class painted the dot brand-green even at Off -- a
	// false live-health signal (colour law §1.1, the ctx-dot.none precedent) --
	// so it must never come back.
	p.wantAbsent(".refresh-live")
	p.wantHas("#refresh-label")

	// Theme toggle stays a server POST /preferences that opts OUT of hx-boost (D5).
	p.wantAttr("#btn-theme-toggle", "data-theme-explicit", "false")
	p.wantAttr(`header.ro-topbar form[action="/preferences"][method="post"]`, "hx-boost", "false")
	p.wantHas("#btn-theme-toggle .theme-icon-dark")
	p.wantHas("#btn-theme-toggle .theme-icon-light")
}

// TestNavbarNamespaceContext pins the namespace context dropdown (.ctx-dd): it
// renders only with a cluster in scope, shows the current namespace, and keeps
// the JS hooks (#namespace-dropdown / #namespace-searchbox / .namespace-item).
// The pill dot is green only when a namespace is SET (SPEC §6.1 "green dot when
// set" + law §1.1): the "None" state carries .ctx-dot.none, which the prototype
// greys (chrome.css:77).
func TestNavbarNamespaceContext(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)

	p.wantHas("header.ro-topbar .ctx-dd")
	p.wantHas(".ctx-dd .ctx-dot")
	p.wantAbsent(".ctx-dd .ctx-dot.none")
	p.wantText(".ctx-dd .context-name", "default")

	// Cluster-scope page (no namespace in the URL): the pill reads "None" and
	// the dot drops its green (the .none variant).
	none := get(t, app, "/clusters/test/namespaces", http.StatusOK)
	none.wantText(".ctx-dd .context-name", "None")
	none.wantHas(".ctx-dd .ctx-dot.none")

	// Namespace filter hooks preserved for readout.js.
	p.wantHas("#namespace-dropdown")
	p.wantHas("#namespace-searchbox")
	if n := p.count(".namespace-item"); n != 3 {
		t.Fatalf("namespace-item count = %d, want 3", n)
	}
	p.wantAttr(".namespace-item", "href", "/clusters/test/namespaces/default/pods")
}

// TestSidebarGroupedIconsAndActive pins the redesign sidebar: it is an
// <aside class="ro-sidebar"> wrapped in the .ro-shell grid, its grouped entries
// each carry a resolved kind icon (the Unit 1 resolver), and the entry whose
// href equals the current path carries the .is-active marker.
func TestSidebarGroupedIconsAndActive(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)

	// Shell grid wrapper + sticky sidebar + main content region.
	p.wantHas(".ro-shell aside.ro-sidebar")
	p.wantHas(".ro-shell main.ro-main")

	// Grouped nav labels in declaration order.
	if labels := p.texts(".ro-sidebar .menu-label"); strings.Join(labels, "|") != "Cluster Resources|Controllers|Pod Management|Meta" {
		t.Fatalf("sidebar menu-labels = %v", labels)
	}

	// Every sidebar entry carries an icon slot (the Unit 1 resolver emits either a
	// curated `.ico` glyph span or a `.kind-tile` monogram). Assert each <a> has at
	// least one of those leading icon elements.
	links := p.doc.Find(".ro-sidebar .menu-list a.menu-item")
	if links.Length() == 0 {
		t.Fatalf("sidebar rendered no menu-item links")
	}
	links.Each(func(i int, s *goquery.Selection) {
		if s.Find(".ico, .kind-tile, .kind-curated, .kind-emoji, .kind-img").Length() == 0 {
			href, _ := s.Attr("href")
			t.Fatalf("sidebar entry %q carries no resolved kind icon", href)
		}
	})

	// The Pods entry is the current path -> it carries .is-active; a sibling does not.
	podsLink := p.doc.Find(`.ro-sidebar .menu-list a.menu-item[href="/clusters/test/namespaces/default/pods"]`)
	if podsLink.Length() != 1 {
		t.Fatalf("expected exactly one Pods sidebar entry, got %d", podsLink.Length())
	}
	if cls, _ := podsLink.Attr("class"); !strings.Contains(cls, "is-active") {
		t.Fatalf("current-path Pods entry class = %q, want to contain is-active", cls)
	}
	cmLink := p.doc.Find(`.ro-sidebar .menu-list a.menu-item[href="/clusters/test/namespaces/default/configmaps"]`)
	if cmLink.Length() == 1 {
		if cls, _ := cmLink.Attr("class"); strings.Contains(cls, "is-active") {
			t.Fatalf("non-current ConfigMaps entry should NOT be is-active, class = %q", cls)
		}
	}
}

// TestLayoutClustersPageOmitsSidebarAndContext pins D11: the Clusters entry page
// renders the topbar chrome but NO sidebar and NO namespace context pill (it has
// no cluster scope), so the .ro-shell/.ro-sidebar and the .ctx-dd are absent.
func TestLayoutClustersPageOmitsSidebarAndContext(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters", http.StatusOK)

	// Topbar still present.
	p.wantHas("header.ro-topbar")
	p.wantText("header.ro-topbar .brand-name", "readout")

	// No sidebar, no grouped labels, no namespace context dropdown.
	p.wantAbsent("aside.ro-sidebar")
	p.wantAbsent(".ro-sidebar .menu-label")
	p.wantAbsent(".ctx-dd")
}

// TestLayoutPaletteDataBlob pins the palette feed contract (D10): the layout
// emits the #ro-palette-data feed as a NON-<script> element. htmx runs with
// allowScriptTags:false, which makes it strip EVERY <script> from swapped
// content -- so a <script> feed disappears after the first hx-boost navigation
// and the palette goes empty. The feed must ride on an element htmx preserves.
// Its parsed shape carries the current scope plus the real cluster / namespace /
// kind / action lists from the same server context the sidebar + navbar have.
func TestLayoutPaletteDataBlob(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)

	blob := p.doc.Find(`#ro-palette-data`)
	if blob.Length() != 1 {
		t.Fatalf("expected exactly one #ro-palette-data element, got %d", blob.Length())
	}
	if blob.Is("script") {
		t.Fatalf("#ro-palette-data must NOT be a <script>: htmx allowScriptTags:false strips it on swap, emptying the palette after an hx-boost nav")
	}

	var data paletteFeedJSON
	if err := json.Unmarshal([]byte(blob.Text()), &data); err != nil {
		t.Fatalf("parse #ro-palette-data JSON: %v\nblob=%s", err, blob.Text())
	}

	// Current scope reflects the path.
	if data.CurrentCluster == nil || *data.CurrentCluster != "test" {
		t.Fatalf("currentCluster = %v, want \"test\"", data.CurrentCluster)
	}
	if data.CurrentNamespace == nil || *data.CurrentNamespace != "default" {
		t.Fatalf("currentNamespace = %v, want \"default\"", data.CurrentNamespace)
	}

	// Cluster list: the one fake cluster, linking to its overview.
	if len(data.Clusters) != 1 || data.Clusters[0].Name != "test" || data.Clusters[0].Href != "/clusters/test" {
		t.Fatalf("clusters feed = %+v, want one {test, /clusters/test}", data.Clusters)
	}

	// Namespace list: the three fixture namespaces, each linking to its pods.
	var nsNames []string
	for _, ns := range data.Namespaces {
		nsNames = append(nsNames, ns.Name)
		if !strings.HasPrefix(ns.Href, "/clusters/test/namespaces/") {
			t.Fatalf("namespace href = %q, want a cluster-scoped link", ns.Href)
		}
	}
	if strings.Join(nsNames, "|") != "default|kube-system|my-app" {
		t.Fatalf("namespaces feed names = %v, want [default kube-system my-app]", nsNames)
	}

	// Kind list: the sidebar resource types, each carrying kind/plural/group/href
	// and the resolver's icon markup. Pods must be present and carry a non-empty icon.
	var foundPods bool
	for _, k := range data.Kinds {
		if k.Href == "" || k.Icon == "" || k.Kind == "" {
			t.Fatalf("kind feed entry missing required field: %+v", k)
		}
		if k.Plural == "pods" {
			foundPods = true
			if k.Kind != "Pods" && k.Kind != "Pod" {
				t.Fatalf("pods kind label = %q, want Pod/Pods", k.Kind)
			}
			if !strings.Contains(k.Icon, "<") {
				t.Fatalf("pods icon markup looks empty: %q", k.Icon)
			}
		}
	}
	if !foundPods {
		t.Fatalf("kinds feed missing the pods entry: %+v", data.Kinds)
	}

	// Actions: at least one navigable action (e.g. the resource-types / clusters jump).
	if len(data.Actions) == 0 {
		t.Fatalf("actions feed is empty")
	}
	for _, a := range data.Actions {
		if a.Label == "" || (a.Href == "" && a.Action == "") {
			t.Fatalf("action feed entry missing label or target: %+v", a)
		}
	}

	// The theme client-action must be present so the palette JS action==="theme"
	// branch (clicking #btn-theme-toggle) is live: it carries action="theme" and NO
	// href (a named client action, not a navigation).
	var themeAction bool
	for _, a := range data.Actions {
		if a.Action == "theme" {
			themeAction = true
			if a.Href != "" {
				t.Fatalf("theme action must carry no href, got %q", a.Href)
			}
			if a.Label == "" {
				t.Fatalf("theme action must carry a label")
			}
		}
	}
	if !themeAction {
		t.Fatalf("actions feed missing the theme client-action: %+v", data.Actions)
	}
}

// TestPaletteFeedIncludesCustomResourceKinds pins that the palette "resource
// type" group lists ALL discovered types -- including CRDs (cert-manager
// certificates), not only the curated sidebar built-ins -- so ⌘K can jump
// straight to a custom resource. Every entry stays fully wired (href + icon).
func TestPaletteFeedIncludesCustomResourceKinds(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)

	var data paletteFeedJSON
	if err := json.Unmarshal([]byte(p.doc.Find(`#ro-palette-data`).Text()), &data); err != nil {
		t.Fatalf("parse palette feed: %v", err)
	}
	has := func(plural string) bool {
		for _, k := range data.Kinds {
			if k.Plural == plural {
				return true
			}
		}
		return false
	}
	if !has("certificates") {
		var got []string
		for _, k := range data.Kinds {
			got = append(got, k.Plural)
		}
		t.Fatalf("palette kinds missing the certificates CRD (jump-to-custom-resource); got %v", got)
	}
	if !has("pods") {
		t.Fatalf("palette kinds missing the built-in pods entry")
	}
	for _, k := range data.Kinds {
		if k.Href == "" || k.Icon == "" || k.Kind == "" {
			t.Fatalf("kind feed entry missing a required field: %+v", k)
		}
	}

	// Each kind carries its api group + scope so the palette can label it (group
	// text + a namespaced/cluster badge). Certificate is a namespaced CRD; nodes
	// is a cluster-scoped built-in.
	var cert, node *struct {
		group string
		ns    bool
		found bool
	}
	cert, node = &struct {
		group string
		ns    bool
		found bool
	}{}, &struct {
		group string
		ns    bool
		found bool
	}{}
	for _, k := range data.Kinds {
		if k.Plural == "certificates" {
			cert.group, cert.ns, cert.found = k.Group, k.Namespaced, true
		}
		if k.Plural == "nodes" {
			node.group, node.ns, node.found = k.Group, k.Namespaced, true
		}
	}
	if !cert.found || cert.group != "cert-manager.io" || !cert.ns {
		t.Fatalf("certificates kind = %+v, want group cert-manager.io + namespaced", cert)
	}
	if !node.found || node.group != "" || node.ns {
		t.Fatalf("nodes kind = %+v, want core group + cluster-scoped (namespaced=false)", node)
	}
}

// TestPaletteFeedDetailTabActions pins that on a detail page the palette adds
// jump-to-tab actions for the object in scope -- Default / YAML / Events, plus
// Logs for a workload -- each a full navigable href, so ⌘K can dive into a view
// without clicking the tabs.
func TestPaletteFeedDetailTabActions(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	base := "/clusters/test/namespaces/default/pods/nginx"

	// Both the detail page AND its /logs sub-page must produce tabs that point at
	// the BASE detail path. On /logs, r.URL.Path is .../nginx/logs, so building tab
	// hrefs from the request path produced broken .../logs?view=events. And the
	// object's Events tab must not duplicate the namespace-level Events meta action.
	check := func(path string) {
		p := get(t, app, path, http.StatusOK)
		var data paletteFeedJSON
		if err := json.Unmarshal([]byte(p.doc.Find(`#ro-palette-data`).Text()), &data); err != nil {
			t.Fatalf("parse palette feed (%s): %v", path, err)
		}
		has := func(label, href string) bool {
			for _, a := range data.Actions {
				if a.Label == label && a.Href == href {
					return true
				}
			}
			return false
		}
		want := map[string]string{
			"Default view": base,
			"YAML":         base + "?view=yaml",
			"Events":       base + "?view=events",
			"Logs":         base + "/logs",
		}
		for label, href := range want {
			if !has(label, href) {
				t.Fatalf("%s: palette missing detail tab %q -> %q (actions=%+v)", path, label, href, data.Actions)
			}
		}
		events := 0
		for _, a := range data.Actions {
			if a.Label == "Events" {
				events++
			}
		}
		if events != 1 {
			t.Fatalf("%s: %d \"Events\" actions, want exactly 1 (object tab dedups the namespace meta)", path, events)
		}
	}
	check(base)
	check(base + "/logs")
}

// TestPaletteFeedServerSideTruncation pins the D5/D21 truncation seat: LONG
// palette labels are middle-truncated SERVER-side, in the feed builder, via the
// shared MiddleTruncate (SPEC §4.2: >42 runes -> first 26 + "…" + last 12),
// carried as the omitempty `display` field next to the untouched full `name`
// (the JS renders display and keeps name in the row title). Short names emit
// no display at all -- the wire stays byte-compatible for them.
func TestPaletteFeedServerSideTruncation(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())

	long := strings.Repeat("a", 30) + "-" + strings.Repeat("b", 30) // 61 runes
	wantDisplay, truncated := MiddleTruncate(long, nameHeadMax, nameHeadLead, nameHeadTrail)
	if !truncated {
		t.Fatalf("fixture name %q should exceed the truncation budget", long)
	}

	// paletteDisplayName is the single truncation seat: long -> the §4.2 form,
	// short / exactly-at-budget -> "" (the omitempty field stays off the wire).
	if got := paletteDisplayName(long); got != wantDisplay {
		t.Fatalf("paletteDisplayName(long) = %q, want %q", got, wantDisplay)
	}
	if got := paletteDisplayName("pods"); got != "" {
		t.Fatalf("paletteDisplayName(short) = %q, want \"\"", got)
	}
	if got := paletteDisplayName(strings.Repeat("x", nameHeadMax)); got != "" {
		t.Fatalf("paletteDisplayName(at-budget) = %q, want \"\" (42 runes fit)", got)
	}

	// Feed-level: a long namespace link flows through buildPaletteFeed with the
	// full name intact and the truncated display alongside.
	r := httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods", nil)
	navbar := navbarView{NamespaceLinks: []navItem{{Text: long, Href: "/clusters/test/namespaces/" + long + "/pods"}}}
	sidebar := sidebarView{}
	feed := app.buildPaletteFeed(r, "test", "default", requestKubeClients{}, &navbar, &sidebar)

	if len(feed.Namespaces) != 1 {
		t.Fatalf("namespaces feed length = %d, want 1", len(feed.Namespaces))
	}
	ns := feed.Namespaces[0]
	if ns.Name != long {
		t.Fatalf("namespace feed name = %q, want the FULL name (truncation must not destroy identity)", ns.Name)
	}
	if ns.Display != wantDisplay {
		t.Fatalf("namespace feed display = %q, want %q", ns.Display, wantDisplay)
	}

	// Short labels (the fixture cluster + every fixture kind) carry NO display.
	if len(feed.Clusters) != 1 || feed.Clusters[0].Display != "" {
		t.Fatalf("short cluster name must emit no display, got %+v", feed.Clusters)
	}
	for _, k := range feed.Kinds {
		if k.Display != "" {
			t.Fatalf("short kind label %q must emit no display, got %q", k.Kind, k.Display)
		}
	}
}

// paletteFeedJSON mirrors the pinned palette-feed wire shape so the test parses
// the emitted blob structurally (the camelCase keys are the public contract Unit
// 4's JS reads). `display` is the Unit 19 extension: the server-truncated
// label form, present only when the name overruns the SPEC §4.2 budget.
type paletteFeedJSON struct {
	CurrentCluster   *string `json:"currentCluster"`
	CurrentNamespace *string `json:"currentNamespace"`
	Clusters         []struct {
		Name    string `json:"name"`
		Href    string `json:"href"`
		Display string `json:"display"`
	} `json:"clusters"`
	Namespaces []struct {
		Name    string `json:"name"`
		Href    string `json:"href"`
		Display string `json:"display"`
	} `json:"namespaces"`
	Kinds []struct {
		Kind       string `json:"kind"`
		Plural     string `json:"plural"`
		Group      string `json:"group"`
		Namespaced bool   `json:"namespaced"`
		Href       string `json:"href"`
		Icon       string `json:"icon"`
		Display    string `json:"display"`
	} `json:"kinds"`
	Actions []struct {
		Label  string `json:"label"`
		Href   string `json:"href"`
		Action string `json:"action"`
	} `json:"actions"`
}
