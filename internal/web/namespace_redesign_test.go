package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/kbelokon/readout/internal/kube"
)

// namespace_redesign_test.go pins the redesign behavioral invariants of the
// NAMESPACE rich cells (the Active/Terminating status reuse + the label chips)
// added to the canonical resource-list engine. Every expectation is an
// INDEPENDENT fact about how a Kubernetes Namespace value maps onto the redesign
// vocabulary, asserted against the documented semantics (Active -> the ok status
// dot; EVERY label -> a neutral .ro-chip with the .ck/.cs/.cv ink-weight split,
// the green .app accent being retired by the colour law; NO fabricated
// pod-count column because a Namespace object carries no pod count), never an
// echo of the emitted class. The mapping is driven through the REAL pipeline
// (buildCellView / buildListView), not re-implemented in the test.

// namespaceObject builds a Namespace Row.Object carrying the phase + labels the
// rich namespace cells read: status.phase (Active/Terminating) and
// metadata.labels. Labels default to none; the per-test overrides supply them.
func namespaceObject(name, phase string, labels map[string]any) map[string]any {
	meta := map[string]any{
		"name":              name,
		"creationTimestamp": "2024-01-01T00:00:00Z",
	}
	if labels != nil {
		meta["labels"] = labels
	}
	return map[string]any{
		"kind":       "Namespace",
		"apiVersion": "v1",
		"metadata":   meta,
		"status":     map[string]any{"phase": phase},
	}
}

// namespacesCellView runs the real buildCellView for one cell of a crafted
// namespaces table, so the namespace-cell mapping (status reuse / label chips) is
// asserted end to end from a Kubernetes Namespace object, not re-implemented in
// the test. obj is the row object; columns/cells/colIdx address the cell.
func namespacesCellView(t *testing.T, columns []string, cells []any, obj map[string]any, colIdx int) cellView {
	t.Helper()
	app := newServer(t, baseConfig(t), time.Now())
	table := &kube.Table{
		Resource: kube.ResourceType{Plural: "namespaces", Kind: "Namespace", Namespaced: false, Version: "v1", APIVersion: "v1"},
		Clusters: []string{"test"},
	}
	for _, name := range columns {
		table.Columns = append(table.Columns, kube.Column{Name: name})
	}
	row := kube.Row{Cells: cells, Cluster: "test", Object: obj}
	req := httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces", nil)
	name := nestedString(obj, "metadata", "name")
	return app.buildCellView(req, table, row, colIdx, cells[colIdx], "", name)
}

// TestNamespaceCells drives the real cell-assembly for the namespace columns and
// asserts the resolved view-model in one namespace row: the Status column reuses
// the status-dot cell (Active -> ok tone), and the synthetic Labels column becomes
// the label-chips cell carrying the sorted key/value pairs -- with NO class
// plumbing at all (under the colour law every chip is neutral; an app.kubernetes.io/*
// label maps to exactly the same chip shape as any other label). The acceptance
// proof that the render test then confirms reaches the DOM. The expectations are
// INDEPENDENT documented facts (Active is the steady ok phase; labels sort by
// key), never an echo of the emitted markup.
func TestNamespaceCells(t *testing.T) {
	// A namespace carrying one app.kubernetes.io/* label and one plain label, in
	// the Active phase. Under the colour law both map to the SAME neutral chip shape.
	labels := map[string]any{
		"app.kubernetes.io/name": "ingress-nginx",
		"team":                   "platform",
	}
	obj := namespaceObject("ingress-nginx", "Active", labels)

	// Columns mirror the namespace Table after decoration: Name, Status, Age, and
	// the synthetic Labels column appended by decorateNamespaceColumns. The Labels
	// display cell is the comma-joined labels (the plain value for sort/TSV/fallback).
	cols := []string{"Name", "Status", "Age", "Labels"}
	cells := []any{"ingress-nginx", "Active", "30d", "app.kubernetes.io/name=ingress-nginx,team=platform"}

	// Status reuse: Active -> the ok status-dot cell (the shared cellStatus
	// branch), NOT a fabricated namespace-specific branch.
	status := namespacesCellView(t, cols, cells, obj, 1)
	if status.Kind != cellStatus {
		t.Fatalf("Status cell kind = %v, want cellStatus (reused, not a new namespace branch)", status.Kind)
	}
	if status.Tone != "ok" {
		t.Fatalf("Active status tone = %q, want ok", status.Tone)
	}

	// Labels: the synthetic Labels column becomes the label-chips cell, one
	// key/value chip per label, sorted by key (app.kubernetes.io/name < team).
	chips := namespacesCellView(t, cols, cells, obj, 3)
	if chips.Kind != cellChips {
		t.Fatalf("Labels cell kind = %v, want cellChips", chips.Kind)
	}
	if len(chips.Chips) != 2 {
		t.Fatalf("chips = %d, want 2 (one per label)", len(chips.Chips))
	}
	appChip := findChip(chips.Chips, "app.kubernetes.io/name")
	if appChip == nil || appChip.Val != "ingress-nginx" {
		t.Fatalf("app.kubernetes.io/name chip missing or wrong value: %#v", chips.Chips)
	}
	plainChip := findChip(chips.Chips, "team")
	if plainChip == nil || plainChip.Val != "platform" {
		t.Fatalf("team chip missing or wrong value: %#v", chips.Chips)
	}
}

// findChip returns the chip carrying the given label key, or nil. Lets the test
// assert per-chip values by the label it represents rather than by position.
func findChip(chips []chipView, key string) *chipView {
	for i := range chips {
		if chips[i].Key == key {
			return &chips[i]
		}
	}
	return nil
}

// TestNamespaceLabelChipsThroughRender drives a namespaces list through the REAL
// pipeline (decorateNamespaceColumns -> buildListView -> ResourceTable templ) and
// asserts the redesign DOM: the Active status dot, NEUTRAL label chips with the
// .ck/.cs/.cv ink-weight split inside .ro-chips (under the colour law -- the retired
// .ro-chip.app green accent must NEVER render, asserted negatively across the
// whole document), a no-label namespace's muted "—", and -- the load-bearing
// constraint -- that NO pod-count column is fabricated (a Namespace object has
// no pod count, and readout has no per-namespace pod-count seam).
func TestNamespaceLabelChipsThroughRender(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())

	withLabels := namespaceObject("ingress-nginx", "Active", map[string]any{
		"app.kubernetes.io/name": "ingress-nginx",
		"team":                   "platform",
	})
	noLabels := namespaceObject("kube-system", "Active", nil)

	table := &kube.Table{
		Resource: kube.ResourceType{Plural: "namespaces", Kind: "Namespace", Namespaced: false, Version: "v1", APIVersion: "v1"},
		Clusters: []string{"test"},
		Columns: []kube.Column{
			{Name: "Name"}, {Name: "Status"}, {Name: "Age"},
		},
		Rows: []kube.Row{
			{Cluster: "test", Object: withLabels, Cells: []any{"ingress-nginx", "Active", "30d"}},
			{Cluster: "test", Object: noLabels, Cells: []any{"kube-system", "Active", "200d"}},
		},
	}
	// Run the REAL decoration applyTableOptions performs for the namespaces plural
	// (appends the synthetic Labels column + per-row chips-source cell), so this
	// test exercises the same Table shape the handler builds.
	decorateNamespaceColumns(table)
	lc := &listContext{Cluster: "test", Plural: "namespaces", ClusterCount: 1, Tables: []kube.Table{*table}}
	req := httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces", nil)
	v := app.buildListView(req, lc)
	doc := renderListView(t, &v)

	ingressRow := doc.Find(`tr:has(td.cell-name a:contains("ingress-nginx"))`)
	if ingressRow.Length() == 0 {
		t.Fatalf("ingress-nginx namespace row missing")
	}
	// Active -> the ok status dot inside .cell-status.ok.
	if ingressRow.Find(".cell-status.ok .ro-dot.ok").Length() != 1 {
		t.Fatalf("ingress-nginx status cell missing .cell-status.ok > .ro-dot.ok: %s", normSpace(ingressRow.Text()))
	}
	// Labels render as .ro-chips with one neutral chip per label. Each chip
	// splits its key into .ck and its value into .cv (ink weight, not hue, per the colour law).
	if got := ingressRow.Find(".ro-chips .ro-chip").Length(); got != 2 {
		t.Fatalf("ingress-nginx label chips = %d, want 2: %s", got, normSpace(ingressRow.Text()))
	}
	appChip := ingressRow.Find(`.ro-chips .ro-chip:has(.ck:contains("app.kubernetes.io/name"))`).First()
	if appChip.Length() != 1 {
		t.Fatalf("app.kubernetes.io/name chip missing from the chips cell")
	}
	if v := normSpace(appChip.Find(".cv").Text()); v != "ingress-nginx" {
		t.Fatalf("app.kubernetes.io/name chip .cv = %q, want ingress-nginx", v)
	}
	teamChip := ingressRow.Find(`.ro-chips .ro-chip:has(.ck:contains("team"))`).First()
	if teamChip.Length() != 1 || normSpace(teamChip.Find(".cv").Text()) != "platform" {
		t.Fatalf("team chip missing or wrong: %s", normSpace(ingressRow.Find(".ro-chips").Text()))
	}

	// The no-label namespace renders the muted "—" (the empty-labels fallback),
	// with NO chips.
	sysRow := doc.Find(`tr:has(td.cell-name a:contains("kube-system"))`)
	if sysRow.Find(".ro-chip").Length() != 0 {
		t.Fatalf("kube-system has no labels and must render no chips")
	}
	if got := normSpace(sysRow.Find(".faint").Text()); got != "—" {
		t.Fatalf("no-label namespace should render the muted —, got %q", got)
	}

	// THE COLOUR-LAW NEGATIVE ASSERTION (the regression net for the retired class):
	// across the WHOLE rendered document -- table, mobile `.ro-cardlist`
	// projection, chrome -- no element ever carries the retired `.ro-chip.app`
	// green accent. Any surface re-emitting it trips this count.
	if got := doc.Find(".ro-chip.app").Length(); got != 0 {
		t.Fatalf("retired .ro-chip.app accent rendered %d time(s); every label chip must be neutral (D3)", got)
	}

	// The load-bearing constraint: NO pod-count column is fabricated. A Namespace
	// object carries no pod count and readout has no per-namespace pod-count seam,
	// so neither the header nor the body may invent one.
	headers := doc.Find("table.ro-table thead th").Map(func(_ int, s *goquery.Selection) string { return normSpace(s.Text()) })
	for _, h := range headers {
		low := strings.ToLower(h)
		if strings.Contains(low, "pod") {
			t.Fatalf("a Pods/pod-count column was fabricated (%q); namespaces have no pod-count seam: headers=%v", h, headers)
		}
	}
}

// TestNamespaceListPreservesGenerics drives a namespaces list through buildListView
// + the render pipeline and confirms the rich label-chips cell coexists with the
// preserved generics: a user-added labelcols column still renders its own generic
// label cell (not swallowed by the synthetic chips column), the synthetic Labels
// chips column lands alongside it, and the column/cell contract the generics depend
// on (sort/TSV/hidecols/customcols all read the same kube.Table columns/cells) stays
// intact -- so the synthetic column never collides with the user's labelcols column.
func TestNamespaceListPreservesGenerics(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())

	obj := namespaceObject("ingress-nginx", "Active", map[string]any{
		"app.kubernetes.io/name": "ingress-nginx",
	})

	// Simulate a user-added single label column (labelcols=app.kubernetes.io/name):
	// AddLabelColumns inserts a column carrying the label VALUE and a Label tag, which
	// must keep falling through to the generic single-label cell (a selector link),
	// NOT be hijacked by the namespace chips branch.
	table := &kube.Table{
		Resource: kube.ResourceType{Plural: "namespaces", Kind: "Namespace", Namespaced: false, Version: "v1", APIVersion: "v1"},
		Clusters: []string{"test"},
		Columns: []kube.Column{
			{Name: "Name"},
			{Name: "Name (app.k8s.io)", Label: "app.kubernetes.io/name"},
			{Name: "Status"},
			{Name: "Age"},
		},
		Rows: []kube.Row{
			{Cluster: "test", Object: obj, Cells: []any{"ingress-nginx", "ingress-nginx", "Active", "30d"}},
		},
	}
	decorateNamespaceColumns(table)
	lc := &listContext{Cluster: "test", Plural: "namespaces", ClusterCount: 1, Tables: []kube.Table{*table}}
	req := httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces", nil)
	v := app.buildListView(req, lc)
	doc := renderListView(t, &v)

	// The synthetic chips column rendered (one .ro-chips block in the table row).
	// Scoped to the .ro-table: the engine now ALSO emits the mobile `.ro-cardlist`
	// projection of the same row (the mobile cards layer), which repeats the chips block as a card
	// meta row; TestMobileCards pins the card projection.
	if doc.Find("table.ro-table .ro-chips").Length() != 1 {
		t.Fatalf("synthetic Labels chips column missing")
	}
	// The user's labelcols column is still a generic single-label selector link
	// (the cellLabel branch -> an <a href=...selector=...>), NOT turned into chips.
	// The slash/equals in the label are URL-encoded in the href, so match on the
	// stable "selector=" prefix rather than the raw label spelling.
	row := doc.Find(`tr:has(td.cell-name a:contains("ingress-nginx"))`)
	selectorLink := row.Find(`a[href*="selector="]`)
	if selectorLink.Length() == 0 {
		t.Fatalf("user labelcols column lost its generic selector link: %s", normSpace(row.Text()))
	}
	// That selector link is the user's generic label cell, NOT one of the synthetic
	// chips (the chips are <span>s with no href), so the generic single-label path
	// was preserved alongside the chips column.
	if row.Find(`.ro-chips a[href*="selector="]`).Length() != 0 {
		t.Fatalf("the selector link must be the generic label cell, not inside the chips block")
	}

	// The header carries BOTH the user's label column and the synthetic Labels
	// column, so the generic column/cell contract stays intact.
	headers := doc.Find("table.ro-table thead th").Map(func(_ int, s *goquery.Selection) string { return normSpace(s.Text()) })
	var haveUserLabel, haveSyntheticLabels bool
	for _, h := range headers {
		if strings.Contains(h, "Name (app.k8s.io)") {
			haveUserLabel = true
		}
		if h == "Labels" {
			haveSyntheticLabels = true
		}
	}
	if !haveUserLabel || !haveSyntheticLabels {
		t.Fatalf("headers must keep both the user label column and the synthetic Labels column: %v", headers)
	}

	// Still no fabricated pod-count column.
	for _, h := range headers {
		if strings.Contains(strings.ToLower(h), "pod") {
			t.Fatalf("pod-count column fabricated (%q): %v", h, headers)
		}
	}
}

// TestDecorateNamespaceColumnsNoDuplicateLabels pins that decorateNamespaceColumns
// never appends a SECOND "Labels" column when one already exists (e.g. a user's
// labelcols=* produced a "Labels" column): the synthetic decoration must be a no-op
// in that case, keeping the table from going ragged or double-rendering chips.
func TestDecorateNamespaceColumnsNoDuplicateLabels(t *testing.T) {
	obj := namespaceObject("ns", "Active", map[string]any{"app.kubernetes.io/name": "x"})
	table := &kube.Table{
		Resource: kube.ResourceType{Plural: "namespaces", Kind: "Namespace", Namespaced: false, Version: "v1", APIVersion: "v1"},
		Clusters: []string{"test"},
		Columns: []kube.Column{
			{Name: "Name"},
			// A pre-existing "Labels" column (as labelcols=* would create).
			{Name: "Labels", Label: "*"},
			{Name: "Status"},
		},
		Rows: []kube.Row{
			{Cluster: "test", Object: obj, Cells: []any{"ns", "app.kubernetes.io/name=x", "Active"}},
		},
	}
	before := len(table.Columns)
	decorateNamespaceColumns(table)
	if len(table.Columns) != before {
		t.Fatalf("decorateNamespaceColumns added a column despite an existing Labels column: %d -> %d", before, len(table.Columns))
	}
	// The row stays in lockstep (no ragged row).
	if len(table.Rows[0].Cells) != len(table.Columns) {
		t.Fatalf("row went ragged: %d cells vs %d columns", len(table.Rows[0].Cells), len(table.Columns))
	}
}
