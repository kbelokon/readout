package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
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

func TestToolsFormUniqueIDs(t *testing.T) {
	doc := renderResourceTable(t, &templates.ListData{
		Tables: []templates.TableData{
			{Kind: "Pods", Tools: templates.TableTools{}},
			{Kind: "Services", Tools: templates.TableTools{}},
		},
	})
	var targets []string
	doc.Find(".toggle-tools").Each(func(_ int, s *goquery.Selection) {
		target, _ := s.Attr("data-target")
		targets = append(targets, target)
	})
	if strings.Join(targets, ",") != "tools-table-1,tools-table-2" {
		t.Fatalf("toggle tools targets = %#v, want tools-table-1/tools-table-2", targets)
	}
	for _, id := range []string{"tools-table-1", "tools-table-2"} {
		if doc.Find("form#"+id).Length() != 1 {
			t.Fatalf("expected exactly one form#%s, got %d", id, doc.Find("form#"+id).Length())
		}
	}
}

func TestListToolsRoundTripApiVersion(t *testing.T) {
	app := newTestServer(t)
	// Single-type pages re-home the labelcols/selector inputs into the D8
	// columns popover (form.ro-pop-form); the hidden-input param round-trip is
	// the contract under test and must survive the move.
	p := get(t, app, "/clusters/test/namespaces/default/pods?apiVersion=v1&api_version=v1&limit=2&label-columns=app&hide-columns=Age", http.StatusOK)
	form := p.doc.Find("form.ro-pop-form")
	if form.Length() != 1 {
		t.Fatalf("popover forms = %d, want 1", form.Length())
	}
	if p.doc.Find("form.tools-form").Length() != 0 {
		t.Fatalf("single-type page still renders the retired v1 tools form")
	}
	for name, want := range map[string]string{
		"apiVersion":    "v1",
		"api_version":   "v1",
		"limit":         "2",
		"label-columns": "app",
		"hide-columns":  "Age",
	} {
		input := form.Find(`input[type="hidden"][name="` + name + `"]`)
		if input.Length() != 1 {
			t.Fatalf("hidden input %q count = %d, want 1", name, input.Length())
		}
		if got, _ := input.Attr("value"); got != want {
			t.Fatalf("hidden input %q value = %q, want %q", name, got, want)
		}
	}
	// The live labelcols/selector inputs moved with their values.
	if got, _ := form.Find(`input[name="labelcols"]`).Attr("value"); got != "app" {
		t.Fatalf("popover labelcols value = %q, want %q", got, "app")
	}
	if form.Find(`input[name="selector"]`).Length() != 1 {
		t.Fatalf("popover lost the selector input")
	}
}

func TestTSVDownloadSelectsTable(t *testing.T) {
	app := newTestServer(t)
	page := get(t, app, "/clusters/test/namespaces/default/pods,services", http.StatusOK)
	var hrefs []string
	page.doc.Find(`a[title="Download resource list as Tab-Separated-Values (TSV)"]`).Each(func(_ int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		hrefs = append(hrefs, href)
	})
	for _, want := range []string{
		"/clusters/test/namespaces/default/pods,services?download=tsv&download_table=pods",
		"/clusters/test/namespaces/default/pods,services?download=tsv&download_table=services",
	} {
		if !contains(hrefs, want) {
			t.Fatalf("download hrefs = %v, missing %q", hrefs, want)
		}
	}

	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods,services?download=tsv&download_table=services", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "text/tab-separated-values") {
		t.Fatalf("service TSV response: status=%d ct=%q", rec.Code, rec.Header().Get("Content-Type"))
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Cluster-IP") || !strings.Contains(body, "frontend") || strings.Contains(body, "nginx") {
		t.Fatalf("service TSV body selected wrong table:\n%s", body)
	}

	fallback := httptest.NewRecorder()
	app.Handler().ServeHTTP(fallback, httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods,services?download=tsv&download_table=missing", nil))
	if !strings.Contains(fallback.Body.String(), "nginx") || strings.Contains(fallback.Body.String(), "Cluster-IP") {
		t.Fatalf("missing download_table did not fall back to first table:\n%s", fallback.Body.String())
	}
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
		if !transientStatus(phase) {
			t.Fatalf("transientStatus(%q) = false, want true (in-flight state)", phase)
		}
	}
	steady := []string{"Running", "Completed", "CrashLoopBackOff", "Error", "Init:CrashLoopBackOff", "ImagePullBackOff"}
	for _, phase := range steady {
		if transientStatus(phase) {
			t.Fatalf("transientStatus(%q) = true, want false (steady state must not pulse)", phase)
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

// TestGenericCellTruncationFollowsIdentifierRule pins Principles §3 ("identifiers
// are sacred — never truncate them"): in a generic list, secondary free-text
// columns (selectors, node selectors, images, labels, messages) truncate with a
// `title=` tooltip, while identifiers (IPs, container names) and numeric counts
// stay FULL — the table wrapper scrolls horizontally under the pinned name column
// to reveal them. A DaemonSet's long Node Selector previously overflowed the row
// because it was missing from the secondary-text allow-list.
func TestGenericCellTruncationFollowsIdentifierRule(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	cols := []kube.Column{
		{Name: "Name"},
		{Name: "IP"},
		{Name: "Internal IP"},
		{Name: "Node Selector"},
		{Name: "Containers"},
		{Name: "Selector"},
		{Name: "Desired", Class: "num"},
		{Name: "Age"},
	}
	cells := []any{
		"do-node-agent", "10.1.2.3", "10.1.2.3",
		"digitalocean.com/nvidia-dcgm-enabled=true,kubernetes.io/os=linux",
		"nrr-status-patcher,nvidia-dcgm-container,do-node-agent",
		"k8s-app=do-node-agent", "71", "286d",
	}
	table := &kube.Table{
		Resource: kube.ResourceType{Plural: "daemonsets", Kind: "DaemonSet", Namespaced: true, Version: "v1", APIVersion: "apps/v1"},
		Clusters: []string{"test"},
		Columns:  cols,
		Rows: []kube.Row{{Cells: cells, Cluster: "test", Object: map[string]any{
			"metadata": map[string]any{"name": "do-node-agent", "namespace": "kube-system", "creationTimestamp": "2025-08-22T21:30:22Z"},
		}}},
	}
	req := httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/_all/daemonsets", nil)

	wantTrunc := map[int]bool{
		1: false, // IP            — identifier, stays full
		2: false, // Internal IP   — identifier, stays full
		3: true,  // Node Selector — secondary free-text (a selector), truncates (the bug fix)
		4: false, // Containers    — container names are identifiers, stay full (scroll)
		5: true,  // Selector      — secondary free-text, truncates
		6: false, // Desired       — numeric count
		7: false, // Age           — short bucketed value
	}
	for idx, want := range wantTrunc {
		cv := app.buildCellView(req, table, table.Rows[0], idx, cells[idx], "kube-system", "do-node-agent")
		if cv.Trunc != want {
			t.Errorf("col %q (idx %d): Trunc=%v, want %v", cols[idx].Name, idx, cv.Trunc, want)
		}
		if want && cv.Title == "" {
			t.Errorf("col %q truncates but carries no tooltip Title", cols[idx].Name)
		}
	}
	// Name is its own sticky, never-truncated cell kind.
	if cv := app.buildCellView(req, table, table.Rows[0], 0, cells[0], "kube-system", "do-node-agent"); cv.Kind != cellName || cv.Trunc {
		t.Errorf("Name cell: kind=%v trunc=%v, want cellName + no trunc", cv.Kind, cv.Trunc)
	}
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

// statesRow returns the body row for a pod in the "states" namespace, addressed
// by its detail link (the pod-name split keeps the full name in the href).
func statesRow(p *page, name string) *goquery.Selection {
	return p.doc.Find(`tr:has(a[href="/clusters/test/namespaces/states/pods/` + name + `"])`)
}

// TestPodsListTransientStatusPulsesThroughRender closes the load-bearing gap: the
// POSITIVE pulse direction reaching the DOM. It drives the real handler against
// the "states" pods fixture (statusTone + transientStatus run in assembly), and
// asserts that a TRANSIENT pod's status dot pulses while a STEADY Running pod in
// the SAME render does not. Reverting dotClass2 to never append " pulse" makes
// this fail (the transient rows lose their .ro-dot.pulse).
func TestPodsListTransientStatusPulsesThroughRender(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC))
	p := get(t, app, "/clusters/test/namespaces/states/pods", http.StatusOK)

	// A ContainerCreating pod pulses (transient): exactly one .ro-dot.pulse in its
	// row, and it is the warn-toned dot.
	creating := statesRow(p, "web-creating-7c9f7cd495-6fff6")
	if creating.Length() == 0 {
		t.Fatalf("ContainerCreating pod row missing")
	}
	if got := creating.Find(".ro-dot.pulse").Length(); got != 1 {
		t.Fatalf("ContainerCreating dot pulse count = %d, want 1", got)
	}
	if creating.Find(".cell-status.warn .ro-dot.warn.pulse").Length() != 1 {
		t.Fatalf("ContainerCreating status cell missing .cell-status.warn > .ro-dot.warn.pulse")
	}

	// A Terminating pod also pulses (transient).
	terminating := statesRow(p, "web-terminating-7c9f7cd495-aaaaa")
	if terminating.Find(".ro-dot.pulse").Length() != 1 {
		t.Fatalf("Terminating dot must pulse (transient): %s", normSpace(terminating.Text()))
	}

	// A steady Running pod in the SAME render does NOT pulse.
	steady := statesRow(p, "web-steady-7c9f7cd495-ccccc")
	if steady.Find(".cell-status.ok .ro-dot.ok").Length() != 1 {
		t.Fatalf("steady Running status cell missing .cell-status.ok > .ro-dot.ok")
	}
	if steady.Find(".ro-dot.pulse").Length() != 0 {
		t.Fatalf("steady Running dot must NOT pulse")
	}

	// Whole-table sanity: exactly the two transient pods pulse (creating +
	// terminating), so a broadened-pulse regression that animated steady states
	// would also trip here. Scoped to the .ro-table: the engine now ALSO emits the
	// mobile `.ro-cardlist` projection of the same rows (Unit 15), whose status
	// pills pulse identically -- TestMobileCards pins that the card pulse matches.
	if got := p.doc.Find("table.ro-table .ro-dot.pulse").Length(); got != 2 {
		t.Fatalf("transient pulse dots in table = %d, want 2 (creating + terminating)", got)
	}
}

// TestPodsListReadyAndRestartTonesThroughRender closes the ready partial/zero +
// restarts some/.ago gap in the DOM. It drives the real handler against the
// "states" fixture (readyRatioClass + splitRestarts run in assembly) and asserts
// the partial/zero ratio tones and the restarted "(… ago)" suffix reach the DOM.
func TestPodsListReadyAndRestartTonesThroughRender(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC))
	p := get(t, app, "/clusters/test/namespaces/states/pods", http.StatusOK)

	// ready 0/1 -> .ready.zero (ContainerCreating pod).
	zero := statesRow(p, "web-creating-7c9f7cd495-6fff6")
	if got := normSpace(zero.Find(".ready.zero").Text()); got != "0/1" {
		t.Fatalf("0/1 pod ready cell = %q, want 0/1 in .ready.zero", got)
	}

	// ready 2/3 -> .ready.partial; restarts "5 (2m ago)" -> .restarts.some + the
	// muted .ago suffix carrying "(2m ago)".
	degraded := statesRow(p, "web-degraded-7c9f7cd495-bbbbb")
	if got := normSpace(degraded.Find(".ready.partial").Text()); got != "2/3" {
		t.Fatalf("2/3 pod ready cell = %q, want 2/3 in .ready.partial", got)
	}
	if got := normSpace(degraded.Find(".restarts.some").Text()); got != "5" {
		t.Fatalf("restarted pod count = %q, want 5 in .restarts.some", got)
	}
	// The "(… ago)" suffix is an adjacent sibling of .restarts inside the same
	// restarts cell (the cell holding .restarts.some). Find .ago there.
	restartCell := degraded.Find("td").FilterFunction(func(_ int, s *goquery.Selection) bool {
		return s.Find(".restarts.some").Length() == 1
	}).First()
	if got := normSpace(restartCell.Find(".ago").Text()); got != "(2m ago)" {
		t.Fatalf("restarted pod ago suffix = %q, want (2m ago)", got)
	}
	// The .ago span sits immediately after .restarts (the CSS muting matches
	// `.restarts + .ago`), so a sibling-vs-descendant regression in the markup
	// would surface as a missing adjacent .ago.
	if restartCell.Find(".restarts + .ago").Length() != 1 {
		t.Fatalf("ago suffix must be an adjacent sibling of .restarts")
	}

	// Cross-check the render carries all three ratio tones at least once, so a
	// regression collapsing the ratio classification surfaces here.
	for _, tone := range []string{".ready.full", ".ready.partial", ".ready.zero"} {
		if p.doc.Find(tone).Length() == 0 {
			t.Fatalf("ready ratio tone %s missing from the states render", tone)
		}
	}
}

// TestGenericKindListRendersThroughRealAssembly closes the generic-fallback gap
// at the PRODUCTION boundary: it drives a generic kind (services -- no Status
// column, no per-kind rich cells) through the real handler + buildCellView, so
// the `colName == "Status"` gate is genuinely exercised. The rows render from the
// Table API with the sticky td.cell-name and NO status dot anywhere (a regression
// that broadened the status-dot branch to a non-pod kind would trip the dot
// count).
func TestGenericKindListRendersThroughRealAssembly(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC))
	p := get(t, app, "/clusters/test/namespaces/default/services", http.StatusOK)

	// Canonical table shell + the generic rows from the Table API.
	p.wantHas(".ro-table-wrap table.ro-table")
	if got := p.texts("td.cell-name"); strings.Join(got, "|") != "frontend|kubernetes" {
		t.Fatalf("service name cells = %v, want [frontend kubernetes]", got)
	}
	// The name cell links to the object via the generic name branch.
	p.wantAttr("td.cell-name a", "href", "/clusters/test/namespaces/default/services/frontend")
	// A generic kind has no Status column -> NO status dot is emitted anywhere
	// (the production status-dot branch is pod/per-kind gated).
	if got := p.doc.Find(".ro-dot").Length(); got != 0 {
		t.Fatalf("generic kind emitted %d status dot(s), want 0", got)
	}
	// Identifier columns (Cluster-IP, Port(s)) are never truncated.
	if p.doc.Find("td.trunc").Length() != 0 {
		t.Fatalf("generic identifier cells must not carry .trunc")
	}
	// The Table-API cell values reach the DOM (Cluster-IP + ports).
	frontend := p.doc.Find(`tr:has(a[href="/clusters/test/namespaces/default/services/frontend"])`)
	rowText := normSpace(frontend.Text())
	for _, want := range []string{"ClusterIP", "10.96.0.10", "80/TCP"} {
		if !strings.Contains(rowText, want) {
			t.Fatalf("generic service row missing %q: %q", want, rowText)
		}
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

	// The redesign partial-failure banner. Scoped to exclude the hidden
	// client-side stale banner (also a `.ro-banner.warn`, Unit 14) so this pins
	// the partial-failure banner specifically.
	banner := p.doc.Find(".ro-banner.warn:not(.ro-stale-banner)")
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
// The hidden client-side stale banner (Unit 14) is a `.ro-banner.warn` too, so
// the partial-failure absence is asserted via the partial-only marker
// (.ro-partial-note) and a visible (non-stale) warn banner.
func TestListSingleClusterNeverShowsPartialFailure(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)
	p.wantAbsent(".ro-banner.warn:not(.ro-stale-banner)")
	p.wantAbsent(".ro-partial-note")
}

// ---------------------------------------------------------------------------
// V2 interaction loop (D6) + the D1 surface boundary.
// ---------------------------------------------------------------------------

// TestRowKeyAndDomID pins the row-identity encoding (D6): the key collapses
// empty segments, and the derived DOM id stays unique and safe inside the
// quoted attribute selector idiomorph matches ids with ([id="…"]) -- '%', '"',
// '\', and whitespace are percent-escaped (escaping '%' itself keeps distinct
// keys mapping to distinct ids); everything else, '/' included, is literal.
func TestRowKeyAndDomID(t *testing.T) {
	cases := []struct {
		cluster, ns, name string
		wantKey, wantID   string
	}{
		{"test", "default", "nginx", "test/default/nginx", "row-test/default/nginx"},
		{"test", "", "worker-1", "test/worker-1", "row-test/worker-1"}, // cluster-scoped: empty ns collapses
		{"c", "n", `we"ird`, `c/n/we"ird`, "row-c/n/we%22ird"},
		{"c", "n", "pct%20", "c/n/pct%20", "row-c/n/pct%2520"},
		{"c", "n", "sp ace", "c/n/sp ace", "row-c/n/sp%20ace"},
	}
	for _, tc := range cases {
		if got := rowKey(tc.cluster, tc.ns, tc.name); got != tc.wantKey {
			t.Fatalf("rowKey(%q,%q,%q) = %q, want %q", tc.cluster, tc.ns, tc.name, got, tc.wantKey)
		}
		if got := rowDomID(tc.wantKey); got != tc.wantID {
			t.Fatalf("rowDomID(%q) = %q, want %q", tc.wantKey, got, tc.wantID)
		}
	}
	if got := rowDomID(""); got != "" {
		t.Fatalf("rowDomID(\"\") = %q, want empty (multi-type rows emit no id)", got)
	}
}

// TestListLoopSurfaceBoundary pins the D1 boundary: a multi-TYPE page
// (plural=CSV) keeps the v1 contract untouched -- the baked-partial-URL
// container re-fetched on ro:refresh, plain boosted sort links (no hx-get),
// identity-less rows, no bulk-bar mount -- while the single-type page (pinned
// in TestBehaviorPodListFacts) runs the v2 loop.
func TestListLoopSurfaceBoundary(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/namespaces/default/pods,services", http.StatusOK)

	// v1 container contract, byte-for-byte.
	p.wantAttr("#resource-list-content", "hx-get", "/clusters/test/namespaces/default/pods,services/_table")
	p.wantAttr("#resource-list-content", "hx-trigger", "ro:refresh")
	p.wantAttr("#resource-list-content", "hx-ext", "morph")
	p.wantAttr("#resource-list-content", "hx-swap", "morph:innerHTML")
	p.wantAbsent("#resource-list-content[data-live-url]")

	// v1 sort headers: plain boosted links, no partial hx-get.
	p.wantAbsent("thead th a[hx-get]")

	// v1 rows: no identity attributes, no bulk-bar mount.
	p.wantAbsent("tr[data-key]")
	p.wantAbsent("#ro-bulkbar")
}

// TestListPartialFragmentCarriesCanonicalHrefs proves the `_table` fragment
// resolves its request-derived hrefs against the CANONICAL list URL, never the
// partial's own path (D6 state coherence). Before the buildListView
// canonicalization, a fragment delivered by a refresh tick baked
// `…/_table?sort=…` into its sort hrefs, so the next header click navigated to
// the bare fragment.
func TestListPartialFragmentCarriesCanonicalHrefs(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	p := get(t, app, "/clusters/test/namespaces/default/pods/_table?selector=app%3Dnginx", http.StatusOK)

	// Canonical hrefs (history/new-tab) + partial hx-get (the loop), both
	// carrying the live query.
	if !contains(p.attrs("thead th a", "href"), "/clusters/test/namespaces/default/pods?selector=app%3Dnginx&sort=Name") {
		t.Fatalf("partial fragment sort href not canonical: %v", p.attrs("thead th a", "href"))
	}
	if !contains(p.attrs("thead th a", "hx-get"), "/clusters/test/namespaces/default/pods/_table?selector=app%3Dnginx&sort=Name") {
		t.Fatalf("partial fragment sort hx-get not the partial URL: %v", p.attrs("thead th a", "hx-get"))
	}
	// The refreshed fragment keeps the row identity attributes (the morph
	// re-keys selection/focus from them after every swap).
	p.wantAttr(`tr[data-key="test/default/nginx"]`, "id", "row-test/default/nginx")
}

// TestListPartialPushURLContract pins the D6 history-push matrix on the
// `_table` handler: ONLY a user-initiated htmx request gets HX-Push-Url, and
// the pushed URL is the CANONICAL list path + the live query -- never the
// partial URL. Ticks/programmatic re-fetches (RO-No-Push), preload warm-ups
// (HX-Preloaded), non-htmx requests, and multi-type pages (D1) get no push:
// htmx pushes one history entry per header occurrence with no same-URL dedupe,
// so any of those pushing would spray junk history entries.
func TestListPartialPushURLContract(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	tableGET := func(path string, headers map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200\nbody=%s", path, rec.Code, rec.Body.String())
		}
		return rec
	}

	const partial = "/clusters/test/namespaces/default/pods/_table?sort=Name"

	// User-initiated sort/filter (an htmx request with no programmatic marker):
	// push the canonical page URL with the query intact.
	rec := tableGET(partial, map[string]string{"HX-Request": "true"})
	if got := rec.Header().Get("HX-Push-Url"); got != "/clusters/test/namespaces/default/pods?sort=Name" {
		t.Fatalf("user sort push = %q, want the canonical list URL", got)
	}

	// Tick / programmatic re-fetch: marked RO-No-Push by the client -> no push.
	rec = tableGET(partial, map[string]string{"HX-Request": "true", "RO-No-Push": "true"})
	if got := rec.Header().Get("HX-Push-Url"); got != "" {
		t.Fatalf("tick push = %q, want none (a 5s interval would spray history)", got)
	}

	// Preload warm-up: never a user gesture -> no push.
	rec = tableGET(partial, map[string]string{"HX-Request": "true", "HX-Preloaded": "true"})
	if got := rec.Header().Get("HX-Push-Url"); got != "" {
		t.Fatalf("preload push = %q, want none", got)
	}

	// A non-htmx GET (curl, crawler): no push header.
	rec = tableGET(partial, nil)
	if got := rec.Header().Get("HX-Push-Url"); got != "" {
		t.Fatalf("non-htmx push = %q, want none", got)
	}

	// Multi-type partials sit outside the loop (D1): no push even for an htmx
	// request.
	rec = tableGET("/clusters/test/namespaces/default/pods,services/_table", map[string]string{"HX-Request": "true"})
	if got := rec.Header().Get("HX-Push-Url"); got != "" {
		t.Fatalf("multi-type push = %q, want none", got)
	}
}

// TestListLoopReadoutJSContract pins the readout.js half of the D6 loop the
// same way the stale-path test does (no headless JS runner in this suite; the
// e2e suite exercises the runtime behavior): the CSP-safe ro-morph extension
// delivering the morph config as a JS object, the location-derived tick URL,
// the RO-No-Push programmatic marker, the user-request in-flight suppression,
// and the identity-keyed row-state store re-applied after swaps.
func TestListLoopReadoutJSContract(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "assets", "static", "readout.js"))
	if err != nil {
		t.Fatalf("read readout.js: %v", err)
	}
	js := string(src)
	for _, needle := range []string{
		"htmx.defineExtension('ro-morph'", // the JS-config morph path (no attribute eval)
		"ignoreActiveValue: true",         // filter draft/focus survives a mid-typing morph
		"dataset.liveUrl === 'location'",  // the v2 container marker readout.js keys on
		"'/_table'",                       // the tick derives the partial URL from location
		"RO-No-Push",                      // programmatic requests opt out of history push
		"userListRequestsInFlight",        // tick suppression while a user request runs
		"htmx:abort",                      // a user action aborts an in-flight tick
		"reapplyRowState",                 // identity-keyed state re-applied after swaps
		"roRowState",                      // the Unit-16 selection-gesture seam
		"tr[data-key]",                    // state re-keys onto rows by object identity
	} {
		if !strings.Contains(js, needle) {
			t.Fatalf("readout.js v2 loop missing %q", needle)
		}
	}
	// The morph config must be delivered as an explicit JS OBJECT inside the
	// extension -- never serialized into an hx-swap attribute value (the
	// vendored extension evals "morph:{…}" specs via Function(), which CSP
	// script-src 'self' blocks at runtime). Pin that no code path WRITES an
	// hx-swap config spec; the rendered-markup side is pinned below.
	if regexp.MustCompile(`hx-swap[^\n]*morph:\{`).MatchString(js) {
		t.Fatalf("readout.js must not write attribute-spec morph config (CSP-blocked eval)")
	}

	// And the rendered page never emits an eval-needing morph spec either: the
	// only hx-swap values on a single-type list are "morph" (the ro-morph JS
	// path) on the container/headers; the v1 multi-type page keeps
	// "morph:innerHTML" (a literal string compare in the vendored ext, no eval).
	app := newServer(t, baseConfig(t), time.Now())
	get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK).wantBodyExcludes("morph:{")
	get(t, app, "/clusters/test/namespaces/default/pods,services", http.StatusOK).wantBodyExcludes("morph:{")
}
