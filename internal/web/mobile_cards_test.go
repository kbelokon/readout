package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/kbelokon/readout/internal/web/templates"
)

// mobile_cards_test.go pins Unit 15: below 760px the resource table is shown as a
// `.ro-cardlist` of `.ro-pcard` (base.css's media query hides
// `.ro-table-wrap.has-cards` and shows `.ro-cardlist`), and the topbar carries a
// `.menu-toggle` hamburger that reveals the sidebar. The responsive switch + the
// focus ring + reduced-motion are VISUAL (no headless runner here, the wave
// reviewer confirms them at <760px); these tests assert the DOM contract instead:
// the engine emits the card list ALONGSIDE the table with the SAME row data (so
// <760px shows cards, not a horizontally-scrolling table), and the menu-toggle
// button exists wherever a sidebar does.

// TestMobileCardsRenderAlongsideTableWithSameData drives the real pods handler and
// asserts the engine emits BOTH the `.ro-table-wrap.has-cards` table AND a
// `.ro-cardlist` of `.ro-pcard` carrying the IDENTICAL row data: one card per table
// body row, each card's `.pc-name` the full pod name, its `.pc-status` the same
// tone the table status cell carries, and its `.pc-meta` the rich ready/restarts/age
// cells in the same vocabulary. Reverting the card block makes <760px fall back to a
// horizontally-scrolling table (the regression this guards).
func TestMobileCardsRenderAlongsideTableWithSameData(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC))
	p := get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)

	// The table-wrap carries `has-cards` (the responsive-switch hook base.css hides
	// below 760px) AND a sibling `.ro-cardlist` is emitted -- both render server-side;
	// the media query (not the template) decides which is visible.
	p.wantHas(".ro-table-wrap.has-cards table.ro-table")
	p.wantHas(".ro-cardlist")

	// One card per table body row (the SAME rows). The pods fixture is nginx + my-app.
	tableNames := p.texts("table.ro-table td.cell-name")
	cardNames := p.texts(".ro-cardlist .ro-pcard .pc-name")
	if strings.Join(tableNames, "|") != "nginx|my-app" {
		t.Fatalf("table name cells = %v, want [nginx my-app]", tableNames)
	}
	if strings.Join(cardNames, "|") != strings.Join(tableNames, "|") {
		t.Fatalf("card names = %v, want the SAME as the table rows %v", cardNames, tableNames)
	}
	if got := p.count(".ro-cardlist .ro-pcard"); got != len(tableNames) {
		t.Fatalf(".ro-pcard count = %d, want one per table row (%d)", got, len(tableNames))
	}

	// The nginx card carries the SAME rich row data the table row does: the full name
	// linking to the object, the ok (steady, non-pulsing) status pill, the .ready.full
	// ratio, the .restarts.zero cell -- all in a single card identified by its name link.
	nginxCard := p.doc.Find(`.ro-pcard:has(.pc-name a[href="/clusters/test/namespaces/default/pods/nginx"])`)
	if nginxCard.Length() != 1 {
		t.Fatalf("nginx card missing (or not addressed by its name link)")
	}
	if nginxCard.Find(".pc-status.ok .ro-dot.ok").Length() != 1 {
		t.Fatalf("nginx card status pill missing .pc-status.ok > .ro-dot.ok: %s", normSpace(nginxCard.Text()))
	}
	if nginxCard.Find(".ro-dot.pulse").Length() != 0 {
		t.Fatalf("steady Running card status must not pulse")
	}
	if got := normSpace(nginxCard.Find(".pc-meta .ready.full").Text()); got != "1/1" {
		t.Fatalf("nginx card ready meta = %q, want 1/1 in .ready.full", got)
	}
	if nginxCard.Find(".pc-meta .restarts.zero").Length() != 1 {
		t.Fatalf("nginx card restarts meta missing .restarts.zero")
	}

	// The card meta rows are keyed by their (lowercased) column header, and the Name
	// + Status columns are NOT repeated as meta rows (they live in `.pc-top`).
	metaKeys := p.texts(".ro-pcard .pc-meta .m .k")
	if !contains(metaKeys, "ready") || !contains(metaKeys, "restarts") || !contains(metaKeys, "age") {
		t.Fatalf("card meta keys = %v, want ready/restarts/age", metaKeys)
	}
	if contains(metaKeys, "name") || contains(metaKeys, "status") {
		t.Fatalf("name/status must live in .pc-top, not be repeated as meta keys: %v", metaKeys)
	}
}

// TestMobileCardsPreserveTransientPulse pins that the card status pill honours the
// SAME transient-pulse classification the table does: a ContainerCreating pod's card
// dot pulses (warn) while a steady Running pod's card dot does not -- the card is the
// same data, so a pulse in the table is a pulse in the card.
func TestMobileCardsPreserveTransientPulse(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC))
	p := get(t, app, "/clusters/test/namespaces/states/pods", http.StatusOK)

	creatingCard := p.doc.Find(`.ro-pcard:has(.pc-name a[href="/clusters/test/namespaces/states/pods/web-creating-7c9f7cd495-6fff6"])`)
	if creatingCard.Length() != 1 {
		t.Fatalf("ContainerCreating card missing")
	}
	if creatingCard.Find(".pc-status.warn .ro-dot.warn.pulse").Length() != 1 {
		t.Fatalf("ContainerCreating card pill missing .pc-status.warn > .ro-dot.warn.pulse: %s", normSpace(creatingCard.Text()))
	}
	// The card's name split is preserved (head/tail), so the full name is intact.
	if got := normSpace(creatingCard.Find(".pc-name").Text()); got != "web-creating-7c9f7cd495-6fff6" {
		t.Fatalf("card name = %q, want the full split name", got)
	}

	steadyCard := p.doc.Find(`.ro-pcard:has(.pc-name a[href="/clusters/test/namespaces/states/pods/web-steady-7c9f7cd495-ccccc"])`)
	if steadyCard.Find(".pc-status.ok .ro-dot.ok").Length() != 1 {
		t.Fatalf("steady card missing .pc-status.ok > .ro-dot.ok")
	}
	if steadyCard.Find(".ro-dot.pulse").Length() != 0 {
		t.Fatalf("steady card dot must NOT pulse")
	}

	// The card list mirrors the table's two transient pulses (creating + terminating).
	if got := p.doc.Find(".ro-cardlist .ro-dot.pulse").Length(); got != 2 {
		t.Fatalf("card pulse dots = %d, want 2 (creating + terminating), mirroring the table", got)
	}
}

// TestMobileCardsGenericKindHasNoStatusPill pins that a generic kind (no Status
// column) renders cards from the SAME Table-API rows, with a `.pc-name` link but NO
// `.pc-status` pill (and no status dot) -- the card status branch is, like the table
// status cell, only emitted when the row actually has a status cell.
func TestMobileCardsGenericKindHasNoStatusPill(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC))
	p := get(t, app, "/clusters/test/namespaces/default/services", http.StatusOK)

	// Cards render for the generic kind alongside the table.
	p.wantHas(".ro-table-wrap.has-cards table.ro-table")
	cardNames := p.texts(".ro-cardlist .ro-pcard .pc-name")
	if strings.Join(cardNames, "|") != "frontend|kubernetes" {
		t.Fatalf("generic card names = %v, want [frontend kubernetes]", cardNames)
	}
	// The frontend card links to the object and carries no status pill / dot.
	frontendCard := p.doc.Find(`.ro-pcard:has(.pc-name a[href="/clusters/test/namespaces/default/services/frontend"])`)
	if frontendCard.Length() != 1 {
		t.Fatalf("frontend service card missing its name link")
	}
	if p.doc.Find(".ro-cardlist .pc-status").Length() != 0 {
		t.Fatalf("a generic kind (no Status column) must render no .pc-status pill")
	}
	if p.doc.Find(".ro-cardlist .ro-dot").Length() != 0 {
		t.Fatalf("a generic kind must render no status dot in its cards")
	}
}

// TestMobileCardsMultiClusterMetaKeepsClusterAndNamespace pins that the leading
// Cluster / Namespace columns (present on an all-clusters, all-namespaces list) are
// surfaced as card meta rows (the card is the full row, not just the per-kind cells),
// so a mobile user still sees which cluster/namespace a row belongs to.
func TestMobileCardsMultiClusterMetaKeepsClusterAndNamespace(t *testing.T) {
	good := newClusterFakeAPI(t, clusterFakeOptions{})
	other := newClusterFakeAPI(t, clusterFakeOptions{})
	app := newMultiClusterServer(t, map[string]string{"aaa": good.URL, "bbb": other.URL})

	p := get(t, app, "/clusters/_all/namespaces/_all/pods", http.StatusOK)

	// At least one card surfaces a `cluster` meta row linking to the cluster overview
	// and a `namespace` meta row linking into the namespace.
	clusterMeta := p.doc.Find(`.ro-pcard .pc-meta .m:has(.k:contains("cluster")) a`)
	if clusterMeta.Length() == 0 {
		t.Fatalf("multi-cluster card missing a cluster meta link")
	}
	if href, _ := clusterMeta.First().Attr("href"); !strings.HasPrefix(href, "/clusters/") {
		t.Fatalf("cluster meta link = %q, want a /clusters/<name> link", href)
	}
	nsMeta := p.doc.Find(`.ro-pcard .pc-meta .m:has(.k:contains("namespace")) a`)
	if nsMeta.Length() == 0 {
		t.Fatalf("all-namespaces card missing a namespace meta link")
	}
}

// TestMobileCardsThroughEngineMirrorTableCells closes the engine-level contract: the
// ResourceTable templ, rendered directly over a crafted ListData, emits the
// `.ro-cardlist`/`.ro-pcard` structure with the SAME cell data as the `.ro-table`
// body -- the name in `.pc-name`, the status tone in `.pc-status`, and a generic data
// cell as a keyed meta row. This pins the markup independent of the assembly layer.
func TestMobileCardsThroughEngineMirrorTableCells(t *testing.T) {
	d := templates.ListData{
		Plural: "pods",
		Tables: []templates.TableData{{
			Kind:        "Pods",
			Count:       1,
			ColumnCount: 3,
			Columns: []templates.TableColumn{
				{Name: "Name"},
				{Name: "Status"},
				{Name: "Ready"},
			},
			Rows: []templates.TableRow{{
				Cells: []templates.TableCell{
					{Kind: templates.CellName, Value: "web-0", NameHead: "web-0", Href: "/clusters/test/namespaces/default/pods/web-0"},
					{Kind: templates.CellStatus, Value: "Running", Tone: "ok", ColClass: "cell-status"},
					{Kind: templates.CellReady, Value: "1/1", Ratio: "full", ColClass: "num"},
				},
				CreatedText: "2026-06-01 00:00:00",
			}},
		}},
	}
	doc := renderResourceTable(t, &d)

	// The table-wrap carries has-cards and a cardlist is emitted with exactly one card.
	if doc.Find(".ro-table-wrap.has-cards table.ro-table").Length() == 0 {
		t.Fatalf("engine did not mark the table-wrap has-cards")
	}
	card := doc.Find(".ro-cardlist .ro-pcard")
	if card.Length() != 1 {
		t.Fatalf("engine emitted %d cards, want 1 (one per row)", card.Length())
	}

	// pc-top: the name link + the ok status pill (same tone the table status cell has).
	if href, _ := card.Find(".pc-name a").Attr("href"); href != "/clusters/test/namespaces/default/pods/web-0" {
		t.Fatalf("card name link = %q, want the object href", href)
	}
	if card.Find(".pc-status.ok .ro-dot.ok").Length() != 1 {
		t.Fatalf("card status pill missing .pc-status.ok > .ro-dot.ok")
	}

	// pc-meta: the Ready cell becomes a keyed meta row (.ready.full), and the synthetic
	// Created column is surfaced too; Name + Status are NOT repeated as meta rows.
	readyMeta := card.Find(`.pc-meta .m:has(.k:contains("ready")) .ready.full`)
	if normSpace(readyMeta.Text()) != "1/1" {
		t.Fatalf("card ready meta = %q, want 1/1", normSpace(readyMeta.Text()))
	}
	keys := card.Find(".pc-meta .m .k").Map(func(_ int, s *goquery.Selection) string { return normSpace(s.Text()) })
	if containsString(keys, "name") || containsString(keys, "status") {
		t.Fatalf("name/status must not be repeated as meta keys: %v", keys)
	}
	if !containsString(keys, "created") {
		t.Fatalf("the synthetic Created column should surface as a card meta row: %v", keys)
	}
}

// TestMobileCardsEventsMessageKeepsMsgTreatment pins the card projection of the
// events Message cell (SPEC §4.16): below 760px the card meta value must carry
// the SAME `.ro-event-msg` treatment marker the table's td does (muted ink +
// wrapping live in CSS keyed on that class; the td-scoped rule cannot reach a
// card <span>, so a card-scope rule keys on the same class). Without the
// CellMsg branch in pcardCell the message rendered as bare unstyled text.
// Driven through the REAL bridge pipeline (renderCellViews), which emits the
// card list alongside the table.
func TestMobileCardsEventsMessageKeepsMsgTreatment(t *testing.T) {
	const msg = "Back-off restarting failed container app in pod ugc-backend-8b9fc9d44-nxxz9"
	doc := renderCellViews(t, "events", "Events", []string{"Message"}, []cellView{msgCellView(msg)})

	// The table half (the existing desktop contract) still holds.
	if doc.Find("table.ro-table td.ro-event-msg").Length() != 1 {
		t.Fatalf("table message cell lost its td.ro-event-msg treatment")
	}

	// The card half: the meta value rides in a .ro-event-msg span, verbatim.
	cardMsg := doc.Find(".ro-cardlist .ro-pcard .pc-meta .m .ro-event-msg")
	if cardMsg.Length() != 1 {
		t.Fatalf("card message meta missing the .ro-event-msg treatment; card meta html=%s", htmlOf(t, doc.Find(".ro-cardlist .ro-pcard .pc-meta")))
	}
	if got := normSpace(cardMsg.Text()); got != msg {
		t.Fatalf("card message text = %q, want the verbatim message %q", got, msg)
	}
	// And the meta row is keyed by its column header, like every other cell.
	if key := normSpace(doc.Find(".ro-cardlist .ro-pcard .pc-meta .m .k").First().Text()); key != "message" {
		t.Fatalf("card message meta key = %q, want message", key)
	}
}

// htmlOf renders a selection back to HTML for failure messages.
func htmlOf(t *testing.T, sel *goquery.Selection) string {
	t.Helper()
	h, err := sel.Html()
	if err != nil {
		t.Fatalf("render selection html: %v", err)
	}
	return h
}

// TestMobileMenuToggleExistsWhereverSidebarDoes pins the hamburger contract (D11):
// the topbar carries a `.menu-toggle` <button> on every page that renders a sidebar
// (so the delegated readout.js click can reveal it), and OMITS it on the Clusters
// entry page, which has no sidebar. Being a <button>, the app-layer
// button:focus-visible ring covers it (the focus ring itself is visual, verified at
// <760px by the reviewer).
func TestMobileMenuToggleExistsWhereverSidebarDoes(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())

	t.Run("a list page has both the sidebar and the menu-toggle", func(t *testing.T) {
		p := get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)
		p.wantHas("aside.ro-sidebar")
		// The toggle is a real <button> in the topbar wiring to the sidebar by
		// aria-controls -- a single one, before the brand.
		toggle := p.doc.Find("header.ro-topbar button.menu-toggle")
		if toggle.Length() != 1 {
			t.Fatalf("menu-toggle button count = %d, want exactly 1 in the topbar", toggle.Length())
		}
		if got, _ := toggle.Attr("aria-controls"); got != "aside-menu" {
			t.Fatalf("menu-toggle aria-controls = %q, want aside-menu (the sidebar menu it reveals)", got)
		}
		// The aside it controls is present in the DOM.
		p.wantHas("#aside-menu")
	})

	t.Run("the all-clusters list (no namespace context) still has a sidebar + toggle", func(t *testing.T) {
		good := newClusterFakeAPI(t, clusterFakeOptions{})
		multi := newMultiClusterServer(t, map[string]string{"aaa": good.URL})
		p := get(t, multi, "/clusters/_all/namespaces/default/pods", http.StatusOK)
		// ShowContext is false here (no single cluster in scope) but the sidebar IS
		// rendered, so the hamburger must be too (gating on the sidebar, not context).
		p.wantHas("aside.ro-sidebar")
		p.wantHas("header.ro-topbar button.menu-toggle")
	})

	t.Run("the Clusters entry page has no sidebar and no menu-toggle", func(t *testing.T) {
		p := get(t, app, "/clusters", http.StatusOK)
		p.wantAbsent("aside.ro-sidebar")
		p.wantAbsent("button.menu-toggle")
	})
}
