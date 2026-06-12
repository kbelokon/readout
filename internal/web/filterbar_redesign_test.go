package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kbelokon/readout/internal/kube"
)

// filterbar_redesign_test.go pins the SERVER half of the Filters v2 chips
// editor: the chips render server-side in the tools row (a shareable URL
// lands with its chips visible), each chip's ✕ removes exactly its own raw
// `?f=` occurrence, the headers carry the data-hint filterable-field markers
// the client autocomplete reads, the editor exists ONLY on single-type pages
// (multi-type pages ignore `?f=`), and a namespace row's
// label chips become click-to-filter anchors appending `label:key=value`.

// podsListView builds a single-type pods listView through the real
// buildListView for the given request URL (path values set explicitly --
// httptest requests carry no route context).
func podsListView(t *testing.T, rawURL string, plural string) *listView {
	t.Helper()
	app := newServer(t, baseConfig(t), time.Now())
	table := kube.Table{
		Resource: kube.ResourceType{Plural: "pods", Kind: "Pod", Namespaced: true, Version: "v1", APIVersion: "v1"},
		Clusters: []string{"test"},
		Columns: []kube.Column{
			{Name: "Name"}, {Name: "Status"}, {Name: "Restarts"}, {Name: "Age"},
		},
		Rows: []kube.Row{
			{Cluster: "test", Object: map[string]any{"metadata": map[string]any{"name": "nginx", "namespace": "default"}}, Cells: []any{"nginx", "Running", "0", "5m"}},
		},
	}
	lc := &listContext{Cluster: "test", Namespace: "default", Plural: plural, ClusterCount: 1, Tables: []kube.Table{table}}
	req := httptest.NewRequest(http.MethodGet, rawURL, nil)
	req.SetPathValue("plural", plural)
	v := app.buildListView(req, lc)
	return &v
}

// TestFilterBarRendersChipsServerSide: a shareable URL with two `?f=` chips
// lands with BOTH chips rendered inside the editor field, each split into the
// .ck field / accent operator / .v value spans, each ✕ removing exactly its own
// raw occurrence (the sibling chip's raw OR-comma encoding survives untouched).
func TestFilterBarRendersChipsServerSide(t *testing.T) {
	v := podsListView(t, "/clusters/test/namespaces/default/pods?f=status%3ARunning,Pending&f=restarts%3E0", "pods")
	doc := renderListView(t, v)

	field := doc.Find("#ro-filter-field")
	if field.Length() != 1 {
		t.Fatalf("chips editor field missing (want one #ro-filter-field)")
	}
	chips := field.Find(".ro-scope-chip")
	if chips.Length() != 2 {
		t.Fatalf("editor chips = %d, want 2", chips.Length())
	}
	// First chip: status : Running,Pending (the OR comma survives in display).
	first := chips.First()
	if k, op, val := normSpace(first.Find(".ck").Text()), normSpace(first.Find("b").Text()), normSpace(first.Find(".v").Text()); k != "status" || op != ":" || val != "Running,Pending" {
		t.Fatalf("first chip ck/op/v = %q/%q/%q, want status/:/Running,Pending", k, op, val)
	}
	// Its ✕ removes ONLY that raw occurrence; the sibling keeps its raw form.
	href, _ := first.Find(".chip-x").Attr("href")
	if strings.Contains(href, "status") || !strings.Contains(href, "f=restarts%3E0") {
		t.Fatalf("first chip remove href = %q, want the status chip dropped and restarts%%3E0 kept raw", href)
	}
	// The input is the JS seam: stable id, empty placeholder once chips exist.
	input := field.Find("input#ro-filter-input")
	if input.Length() != 1 {
		t.Fatalf("editor input#ro-filter-input missing")
	}
	if ph, _ := input.Attr("placeholder"); ph != "" {
		t.Fatalf("placeholder with chips = %q, want empty", ph)
	}
	// The unknown-field hint mount + the legend line render with the editor.
	if doc.Find("#ro-filter-error[hidden]").Length() != 1 {
		t.Fatalf("hidden #ro-filter-error mount missing")
	}
	if hint := normSpace(doc.Find(".filter-hint").Text()); !strings.Contains(hint, "free text matches the name") || !strings.Contains(hint, "removes the last chip") {
		t.Fatalf("filter-hint legend copy wrong: %q", hint)
	}
}

// TestFilterBarHintsAndPlaceholder: without chips the input carries the
// worked-example placeholder, and every REAL Table column header (never the
// synthetic Created header) carries the data-hint filterable marker with the
// duration/number/text typing the autocomplete shows.
func TestFilterBarHintsAndPlaceholder(t *testing.T) {
	v := podsListView(t, "/clusters/test/namespaces/default/pods", "pods")
	doc := renderListView(t, v)

	input := doc.Find("input#ro-filter-input")
	if ph, _ := input.Attr("placeholder"); !strings.Contains(ph, "Filter pods…") || !strings.Contains(ph, "⏎ makes a chip") {
		t.Fatalf("no-chips placeholder = %q, want the worked-example copy", ph)
	}
	wantHints := map[string]string{"Name": "text", "Status": "text", "Restarts": "number", "Age": "duration"}
	for name, want := range wantHints {
		th := doc.Find(`table.ro-table thead th:has(a:contains("` + name + `"))`).First()
		hint, ok := th.Attr("data-hint")
		if !ok || hint != want {
			t.Fatalf("th %s data-hint = %q (present=%v), want %q", name, hint, ok, want)
		}
	}
	created := doc.Find(`table.ro-table thead th:has(a:contains("Created"))`).First()
	if _, ok := created.Attr("data-hint"); ok {
		t.Fatalf("the synthetic Created header must NOT carry data-hint (it is not a filterable Table column)")
	}
}

// TestFilterBarSingleTypeOnly: a multi-type page (plural=all) renders NO chips
// editor -- `?f=` is ignored on multi-type pages, so no editor may suggest it works.
func TestFilterBarSingleTypeOnly(t *testing.T) {
	v := podsListView(t, "/clusters/test/namespaces/default/all?f=status%3ARunning", "all")
	doc := renderListView(t, v)
	if doc.Find("#ro-filter-field").Length() != 0 {
		t.Fatalf("multi-type page rendered the chips editor; it is a single-type-page surface")
	}
	if doc.Find("input#ro-filter-input").Length() != 0 {
		t.Fatalf("multi-type page rendered the editor input")
	}
}

// TestNamespaceLabelChipClickToFilter: on the single-type namespaces list each
// row label chip is an ANCHOR appending its `label:key=value` chip to the
// current `?f=` set -- raw string append, so an existing raw chip param keeps
// its exact wire encoding.
func TestNamespaceLabelChipClickToFilter(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	table := &kube.Table{
		Resource: kube.ResourceType{Plural: "namespaces", Kind: "Namespace", Namespaced: false, Version: "v1", APIVersion: "v1"},
		Clusters: []string{"test"},
		Columns:  []kube.Column{{Name: "Name"}, {Name: "Status"}, {Name: "Age"}},
		Rows: []kube.Row{
			{Cluster: "test", Object: namespaceObject("default", "Active", map[string]any{"team": "core"}), Cells: []any{"default", "Active", "2y"}},
		},
	}
	decorateNamespaceColumns(table)
	lc := &listContext{Cluster: "test", Plural: "namespaces", ClusterCount: 1, Tables: []kube.Table{*table}}
	req := httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces?f=status%3AActive,Terminating", nil)
	req.SetPathValue("plural", "namespaces")
	v := app.buildListView(req, lc)
	doc := renderListView(t, &v)

	chip := doc.Find(`tbody .ro-chips a.ro-chip`).First()
	if chip.Length() != 1 {
		t.Fatalf("expected the team=core label chip as a click-to-filter anchor")
	}
	href, _ := chip.Attr("href")
	if !strings.Contains(href, "f=label%3Ateam%3Dcore") {
		t.Fatalf("label chip href = %q, want the appended f=label%%3Ateam%%3Dcore chip", href)
	}
	// The pre-existing raw chip param survives byte-exact (raw OR comma intact).
	if !strings.Contains(href, "f=status%3AActive,Terminating") {
		t.Fatalf("label chip href = %q, want the existing raw f param kept byte-exact", href)
	}
}
