package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/kbelokon/readout/internal/kube"
	"github.com/kbelokon/readout/internal/web/templates"
)

// list_redesign_test.go pins the redesign behavioral invariants of the canonical
// resource-list ENGINE (resource_table.templ + the package-web cell assembly).
// Every expectation here is an INDEPENDENT fact about how a Kubernetes value
// should map onto the redesign vocabulary (the loud identifier is never
// truncated and carries the full name; only transient states pulse; a ready
// ratio earns a tone; an unmocked kind still renders natively), never an echo of
// the class the code happens to emit.

// renderResourceTable renders the ResourceTable templ component over a crafted
// ListData and parses the output, so the generic-fallback + cell-render branches
// can be asserted from deterministic in-memory input (no HTTP, no fixtures).
func renderResourceTable(t *testing.T, d *templates.ListData) *goquery.Document {
	t.Helper()
	var sb strings.Builder
	if err := templates.ResourceTable(*d).Render(context.Background(), &sb); err != nil {
		t.Fatalf("render ResourceTable: %v", err)
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(sb.String()))
	if err != nil {
		t.Fatalf("parse ResourceTable: %v", err)
	}
	return doc
}

// TestPodNameSplitKeepsFullName pins the sticky-name invariant: the pn-head +
// pn-tail split NEVER drops or rewrites a character -- head+tail reconstructs the
// exact object name for every input. Pod/ReplicaSet names with a generated hash
// suffix split (workload bright, hash muted); everything else stays whole.
func TestPodNameSplitKeepsFullName(t *testing.T) {
	cases := []struct {
		plural   string
		name     string
		wantHead string
		wantTail string
	}{
		// Deployment-style pod: workload + replicaset hash + pod hash.
		{"pods", "admin-backend-7c9f7cd495-6fff6", "admin-backend", "-7c9f7cd495-6fff6"},
		// Short bare names (no recognisable suffix) stay whole.
		{"pods", "nginx", "nginx", ""},
		{"pods", "my-app", "my-app", ""},
		// ReplicaSet-style: workload + single template hash.
		{"replicasets", "admin-backend-7c9f7cd495", "admin-backend", "-7c9f7cd495"},
		// A non-pod/rs identifier is never split (a Service named like a pod).
		{"services", "admin-backend-7c9f7cd495-6fff6", "admin-backend-7c9f7cd495-6fff6", ""},
	}
	for _, c := range cases {
		head, tail := splitObjectName(c.plural, c.name)
		if head != c.wantHead || tail != c.wantTail {
			t.Fatalf("splitObjectName(%q,%q) = (%q,%q), want (%q,%q)", c.plural, c.name, head, tail, c.wantHead, c.wantTail)
		}
		// The load-bearing invariant: the visible name is preserved exactly.
		if head+tail != c.name {
			t.Fatalf("splitObjectName(%q,%q): head+tail=%q != name", c.plural, c.name, head+tail)
		}
	}
}

// TestStatusToneMapping pins the Bulma-tone -> redesign-tone mapping AND the
// transient-state classification. The expectations are the documented mapping
// (success->ok, warning->warn, danger->err, info->info, grey->mute, ""->"")
// and the rulebook's "only in-flight states pulse".
func TestStatusToneMapping(t *testing.T) {
	tones := map[string]string{
		"has-text-success": "ok",
		"has-text-warning": "warn",
		"has-text-danger":  "err",
		"has-text-info":    "info",
		"has-text-grey":    "mute",
		"":                 "", // unmocked kind / unknown value -> no tone colour
	}
	for in, want := range tones {
		if got := statusTone(in); got != want {
			t.Fatalf("statusTone(%q) = %q, want %q", in, got, want)
		}
	}

	transient := []string{"ContainerCreating", "Terminating", "PodInitializing", "Pending", "Init:0/1"}
	for _, phase := range transient {
		if !transientPodPhase(phase) {
			t.Fatalf("transientPodPhase(%q) = false, want true (in-flight state)", phase)
		}
	}
	steady := []string{"Running", "Completed", "CrashLoopBackOff", "Error", "Init:CrashLoopBackOff", "ImagePullBackOff"}
	for _, phase := range steady {
		if transientPodPhase(phase) {
			t.Fatalf("transientPodPhase(%q) = true, want false (steady state must not pulse)", phase)
		}
	}
}

// TestReadyAndRestartsTones pins the ratio + restart tone helpers against the
// documented rule: ready full=all, partial=some, zero=none; restarts zero vs
// some; and the "(… ago)" suffix split off the restart count.
func TestReadyAndRestartsTones(t *testing.T) {
	ready := map[string]string{"3/3": "full", "2/3": "partial", "0/1": "zero", "1/1": "full", "x": ""}
	for in, want := range ready {
		if got := readyRatioClass(in); got != want {
			t.Fatalf("readyRatioClass(%q) = %q, want %q", in, got, want)
		}
	}
	if c, a := splitRestarts("2 (38h ago)"); c != "2" || a != "(38h ago)" {
		t.Fatalf("splitRestarts restarted = (%q,%q), want (2,(38h ago))", c, a)
	}
	if c, a := splitRestarts("0"); c != "0" || a != "" {
		t.Fatalf("splitRestarts zero = (%q,%q), want (0,)", c, a)
	}
	if restartsTone("0") != "zero" || restartsTone("") != "zero" || restartsTone("2") != "some" {
		t.Fatalf("restartsTone mismatch: %q %q %q", restartsTone("0"), restartsTone(""), restartsTone("2"))
	}
}

// podsCellView is a small harness: it runs the real buildCellView for one cell of
// a crafted pods table, so the assembly mapping (kind/tone/ratio/pulse/ago/split)
// is asserted end-to-end from a Kubernetes value, not re-implemented in the test.
func podsCellView(t *testing.T, columns []string, cells []any, colIdx int) cellView {
	t.Helper()
	app := newServer(t, baseConfig(t), time.Now())
	table := &kube.Table{
		Resource: kube.ResourceType{Plural: "pods", Kind: "Pod", Namespaced: true, Version: "v1", APIVersion: "v1"},
		Clusters: []string{"test"},
	}
	for _, name := range columns {
		table.Columns = append(table.Columns, kube.Column{Name: name})
	}
	row := kube.Row{Cells: cells, Cluster: "test", Object: map[string]any{
		"metadata": map[string]any{"name": cellString(kube.Row{Cells: cells}, 0), "namespace": "default", "creationTimestamp": "2026-06-02T10:45:45Z"},
	}}
	req := httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods", nil)
	return app.buildCellView(req, table, row, colIdx, cells[colIdx], "default", cellString(row, 0))
}

// TestPodsCellAssemblyMapsTones drives the real cell-assembly for the rich Pods
// columns and asserts the resolved view-model: the name splits, a transient
// status pulses while Running does not, a 0/1 ratio is zero, a restarted count is
// "some" with its ago suffix. This is the mapping proof the render test then
// confirms reaches the DOM.
func TestPodsCellAssemblyMapsTones(t *testing.T) {
	cols := []string{"Name", "Ready", "Status", "Restarts", "Age"}

	// Name (deployment-style pod) -> split head/tail, full name preserved.
	name := podsCellView(t, cols, []any{"admin-backend-7c9f7cd495-6fff6", "3/3", "Running", "0", "47h"}, 0)
	if name.Kind != cellName || name.NameHead != "admin-backend" || name.NameHead+name.NameTail != "admin-backend-7c9f7cd495-6fff6" {
		t.Fatalf("name cell = %#v", name)
	}

	// Running status -> tone ok, NOT pulsing.
	running := podsCellView(t, cols, []any{"nginx", "1/1", "Running", "0", "10m"}, 2)
	if running.Kind != cellStatus || running.Tone != "ok" || running.Pulse {
		t.Fatalf("Running status cell = %#v, want tone=ok pulse=false", running)
	}

	// ContainerCreating -> tone warn, PULSING (transient).
	creating := podsCellView(t, cols, []any{"web-0", "0/1", "ContainerCreating", "0", "3s"}, 2)
	if creating.Tone != "warn" || !creating.Pulse {
		t.Fatalf("ContainerCreating status cell = %#v, want tone=warn pulse=true", creating)
	}

	// Ready 3/3 -> full; 0/1 -> zero.
	full := podsCellView(t, cols, []any{"web-0", "3/3", "Running", "0", "1d"}, 1)
	zero := podsCellView(t, cols, []any{"web-0", "0/1", "Pending", "0", "5s"}, 1)
	if full.Kind != cellReady || full.Ratio != "full" || zero.Ratio != "zero" {
		t.Fatalf("ready ratios = full:%#v zero:%#v", full, zero)
	}

	// Restarts "5 (2m ago)" -> tone some + ago suffix; count carried in Value.
	restarts := podsCellView(t, cols, []any{"web-0", "1/1", "Running", "5 (2m ago)", "1d"}, 3)
	if restarts.Kind != cellRestarts || restarts.Tone != "some" || restarts.Value != "5" || restarts.Ago != "(2m ago)" {
		t.Fatalf("restarts cell = %#v", restarts)
	}
	zeroRestarts := podsCellView(t, cols, []any{"web-0", "1/1", "Running", "0", "1d"}, 3)
	if zeroRestarts.Tone != "zero" || zeroRestarts.Ago != "" {
		t.Fatalf("zero restarts cell = %#v", zeroRestarts)
	}

	// Age cell carries the short bucketed value + the full timestamp in its title
	// (the redundant full-timestamp column is collapsed into the tooltip). The
	// harness object's creationTimestamp is 2026-06-02T10:45:45Z.
	age := podsCellView(t, cols, []any{"web-0", "1/1", "Running", "0", "47h"}, 4)
	if age.Value != "47h" || !strings.Contains(age.Class, "age-") || age.Title != "created 2026-06-02 10:45:45" {
		t.Fatalf("age cell = %#v, want value=47h age-* class title='created 2026-06-02 10:45:45'", age)
	}
}

// TestPodsListRendersRichCells drives the FULL handler against the pods fixture
// (nginx 1/1 Running 0; my-app 1/1 Running 0) and asserts the redesign DOM: the
// sticky never-truncated name cell carrying the full name, the ok status dot that
// does NOT pulse (Running is steady), the .ready.full ratio, the .restarts.zero
// cell, and the age cell carrying the short value with the full timestamp in its
// title.
func TestPodsListRendersRichCells(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)

	// Canonical table shell.
	p.wantHas(".ro-table-wrap table.ro-table")
	// Sticky name cell carries the FULL untruncated name (no .trunc on it).
	if got := p.texts("td.cell-name"); strings.Join(got, "|") != "nginx|my-app" {
		t.Fatalf("name cells = %v", got)
	}
	p.wantAbsent("td.cell-name.trunc")

	nginxRow := p.doc.Find(`tr:has(a[href="/clusters/test/namespaces/default/pods/nginx"])`)
	// Status: ok dot inside .cell-status.ok, steady -> no pulse.
	if nginxRow.Find(".cell-status.ok .ro-dot.ok").Length() != 1 {
		t.Fatalf("nginx status cell missing .cell-status.ok > .ro-dot.ok")
	}
	if nginxRow.Find(".ro-dot.pulse").Length() != 0 {
		t.Fatalf("Running dot must not pulse")
	}
	// Ready 1/1 -> .ready.full.
	if got := normSpace(nginxRow.Find(".ready.full").Text()); got != "1/1" {
		t.Fatalf("nginx ready = %q, want 1/1 in .ready.full", got)
	}
	// Restarts 0 -> .restarts.zero, no ago suffix.
	if nginxRow.Find(".restarts.zero").Length() != 1 {
		t.Fatalf("nginx restarts cell missing .restarts.zero")
	}
	if nginxRow.Find(".restarts .ago").Length() != 0 {
		t.Fatalf("unrestarted pod must not render an ago suffix")
	}
	// Age cell carries the short bucketed value with an age-* class (the fixture's
	// PartialObjectMetadata has no creationTimestamp, so the tooltip-from-timestamp
	// is exercised in the assembly test where the timestamp is controlled).
	ageCell := nginxRow.Find("td.age-old, td[class*='age-']").FilterFunction(func(_ int, s *goquery.Selection) bool {
		return normSpace(s.Text()) == "10m"
	}).First()
	if ageCell.Length() == 0 {
		t.Fatalf("nginx age cell (short value 10m + age-* class) missing: %s", normSpace(nginxRow.Text()))
	}
}

// TestResourceTableGenericFallbackReskins proves an UNMOCKED kind (no per-kind
// rich cells) still renders its rows from the k8s Table cells, just reskinned:
// the canonical .ro-table, the sticky td.cell-name name link, and every other
// column as a plain cell. It renders the engine directly over a crafted
// ConfigMap table whose Status-less columns have NO tone -> no status dot.
func TestResourceTableGenericFallbackReskins(t *testing.T) {
	d := templates.ListData{
		Plural: "configmaps",
		Tables: []templates.TableData{{
			Kind:        "ConfigMaps",
			Count:       1,
			ColumnCount: 2,
			Columns: []templates.TableColumn{
				{Name: "Name"},
				{Name: "Data"},
			},
			Rows: []templates.TableRow{{
				Cells: []templates.TableCell{
					{Kind: templates.CellName, Value: "kube-root-ca.crt", NameHead: "kube-root-ca.crt", Href: "/clusters/test/namespaces/default/configmaps/kube-root-ca.crt"},
					{Kind: templates.CellPlain, Value: "3", ColClass: "num"},
				},
				CreatedText: "2026-06-01 00:00:00",
			}},
		}},
	}
	doc := renderResourceTable(t, &d)

	if doc.Find(".ro-table-wrap table.ro-table").Length() == 0 {
		t.Fatalf("generic kind did not render the canonical .ro-table")
	}
	if got := normSpace(doc.Find("td.cell-name a").Text()); got != "kube-root-ca.crt" {
		t.Fatalf("generic name cell = %q", got)
	}
	if href, _ := doc.Find("td.cell-name a").Attr("href"); href != "/clusters/test/namespaces/default/configmaps/kube-root-ca.crt" {
		t.Fatalf("generic name link = %q", href)
	}
	// A generic kind has no Status column -> no status dot is emitted at all.
	if doc.Find(".ro-dot").Length() != 0 {
		t.Fatalf("generic kind must not emit a status dot")
	}
	// The plain data cell renders its value untoned.
	if !strings.Contains(normSpace(doc.Find("tbody tr").Text()), "3") {
		t.Fatalf("generic data cell value missing")
	}
}

// TestListColumnCustomizationPreserved proves the rich Pods cells do NOT bypass
// the column pipeline: hidecols/labelcols/customcols/sort and the TSV download
// all still shape the SAME canonical .ro-table. A user-added label column falls
// through to the generic (selector-link) cell, not a rich cell.
func TestListColumnCustomizationPreserved(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())

	t.Run("hidecols drops a column from the .ro-table head", func(t *testing.T) {
		p := get(t, app, "/clusters/test/namespaces/default/pods?hidecols=Status", http.StatusOK)
		if contains(p.texts("table.ro-table thead th"), "Status") {
			t.Fatalf("hidecols=Status left a Status header: %v", p.texts("table.ro-table thead th"))
		}
	})

	t.Run("labelcols adds a column whose cell is generic, not rich", func(t *testing.T) {
		p := get(t, app, "/clusters/test/namespaces/default/pods?labelcols=app", http.StatusOK)
		if !contains(p.texts("table.ro-table thead th"), "App") {
			t.Fatalf("labelcols=app did not add an App column: %v", p.texts("table.ro-table thead th"))
		}
		// The App column carries the label value as a selector link (generic
		// fallback), and is NOT one of the rich cells (no ready/restarts/dot in it).
		nginxRow := p.doc.Find(`tr:has(a[href="/clusters/test/namespaces/default/pods/nginx"])`)
		appCell := nginxRow.Find(`td:has(a[href*="selector=app%3Dnginx"])`)
		if appCell.Length() == 0 {
			t.Fatalf("App label cell missing its selector link: %s", normSpace(nginxRow.Text()))
		}
		if appCell.Find(".ready, .restarts, .ro-dot").Length() != 0 {
			t.Fatalf("user label column must fall through to a generic cell, not a rich one")
		}
	})

	t.Run("customcols adds a column rendered as a generic cell", func(t *testing.T) {
		p := get(t, app, "/clusters/test/namespaces/default/pods?custom-columns=Image=spec.containers[0].image", http.StatusOK)
		if !contains(p.texts("table.ro-table thead th"), "Image") {
			t.Fatalf("customcols did not add an Image column: %v", p.texts("table.ro-table thead th"))
		}
		nginxRow := p.doc.Find(`tr:has(a[href="/clusters/test/namespaces/default/pods/nginx"])`)
		if !strings.Contains(nginxRow.Text(), "nginx:1.27") {
			t.Fatalf("custom Image column missing its value: %s", nginxRow.Text())
		}
	})

	t.Run("sort flips row order in the .ro-table body", func(t *testing.T) {
		p := get(t, app, "/clusters/test/namespaces/default/pods?sort=Name", http.StatusOK)
		if got := p.texts("table.ro-table td.cell-name"); strings.Join(got, "|") != "my-app|nginx" {
			t.Fatalf("sorted rows = %v, want [my-app nginx]", got)
		}
		// The active sort column header carries the redesign `sorted` marker.
		if p.doc.Find("table.ro-table thead th.sorted").Length() == 0 {
			t.Fatalf("active sort column missing the .sorted header marker")
		}
	})

	t.Run("TSV download still streams the rows", func(t *testing.T) {
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods?download=tsv", nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "text/tab-separated-values") {
			t.Fatalf("TSV download regressed: status=%d ct=%q", rec.Code, rec.Header().Get("Content-Type"))
		}
		if !strings.Contains(rec.Body.String(), "nginx") {
			t.Fatalf("TSV download missing nginx row")
		}
	})
}

// TestAllClusterPartialFailureBanner proves the all-cluster list partial-failure
// state is preserved AND reskinned: with one cluster's pods backend failing, the
// request still succeeds, the healthy cluster's rows render in the .ro-table, and
// the redesign `.ro-banner.warn` carries the "Partial results: N failed" title
// plus the failed cluster's per-cluster error line.
func TestAllClusterPartialFailureBanner(t *testing.T) {
	good := newClusterFakeAPI(t, clusterFakeOptions{})
	bad := newClusterFakeAPI(t, clusterFakeOptions{failList: true})
	app := newMultiClusterServer(t, map[string]string{"good": good.URL, "zbad": bad.URL})

	p := get(t, app, "/clusters/_all/namespaces/default/pods", http.StatusOK)

	// Healthy cluster rows rendered into the canonical table.
	p.wantHas(`table.ro-table td.cell-name a[href="/clusters/good/namespaces/default/pods/nginx"]`)

	// The redesign partial-failure banner.
	banner := p.doc.Find(".ro-banner.warn")
	if banner.Length() == 0 {
		t.Fatalf("all-cluster partial-failure banner missing")
	}
	if got := normSpace(banner.Find(".bn-title").Text()); got != "Partial results: 1 failed" {
		t.Fatalf("partial banner title = %q, want 'Partial results: 1 failed'", got)
	}
	if !strings.Contains(banner.Text(), "zbad/pods") {
		t.Fatalf("partial banner missing the failed-cluster error line (zbad/pods): %s", banner.Text())
	}
}

// TestListSingleClusterNeverShowsPartialFailure pins the D11 invariant boundary:
// a SINGLE-cluster list (even on a healthy fixture) NEVER renders the all-cluster
// partial-failure banner. "Some clusters failed" is an all-cluster story only.
func TestListSingleClusterNeverShowsPartialFailure(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)
	p.wantAbsent(".ro-banner.warn")
	p.wantAbsent(".ro-partial-note")
}
