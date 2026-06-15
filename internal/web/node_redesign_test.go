package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/kbelokon/readout/internal/config"
	"github.com/kbelokon/readout/internal/kube"
	"github.com/kbelokon/readout/internal/web/templates"
)

// node_redesign_test.go pins the redesign behavioral invariants of the NODE rich
// cells (roles chips / capacity bars / condition pills) added to the canonical
// resource-list engine. Every expectation is an INDEPENDENT fact about how a
// Kubernetes node value maps onto the redesign vocabulary, asserted against the
// documented thresholds (81%->hi, 60%->mid, 40%->lo; control-plane->.cp; a clean
// node->"—"; a NotReady condition->a pill), never an echo of the emitted class.
// The mapping is driven through the REAL pipeline (buildCellView /
// fetchMetricsUsage + applyMetricsUsage) from a crafted node Table, not
// re-implemented in the test.

// nodeObject builds a node Row.Object carrying the status/labels the rich node
// cells read: status.capacity (cpu/memory/pods), status.conditions, and the
// node-role labels. capacity/conditions/labels default to a healthy worker; the
// per-test overrides set the fields a case exercises.
func nodeObject(name string, capacity map[string]any, conditions []any, roleLabels ...string) map[string]any {
	labels := map[string]any{"kubernetes.io/hostname": name}
	for _, role := range roleLabels {
		labels["node-role.kubernetes.io/"+role] = ""
	}
	status := map[string]any{}
	if capacity != nil {
		status["capacity"] = capacity
	}
	if conditions != nil {
		status["conditions"] = conditions
	}
	return map[string]any{
		"kind":       "Node",
		"apiVersion": "v1",
		"metadata": map[string]any{
			"name":              name,
			"creationTimestamp": "2024-02-01T08:00:00Z",
			"labels":            labels,
		},
		"status": status,
	}
}

// renderListView drives a package-web listView through the REAL render bridge
// (toListData -> toTableData -> the ResourceTable templ) and parses the output, so
// the node cell view-models are asserted through the production assembly+render
// pipeline rather than a hand-built templates.ListData.
func renderListView(t *testing.T, v *listView) *goquery.Document {
	t.Helper()
	var sb strings.Builder
	if err := templates.ResourceTable(toListData(v)).Render(context.Background(), &sb); err != nil {
		t.Fatalf("render list view: %v", err)
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(sb.String()))
	if err != nil {
		t.Fatalf("parse list view: %v", err)
	}
	return doc
}

// nodesCellView runs the real buildCellView for one cell of a crafted nodes table,
// so the node-cell mapping (capacity bucket/roles/conditions) is asserted end to
// end from a Kubernetes node object, not re-implemented in the test. obj is the
// row object; columns/cells/colIdx address the cell under test.
func nodesCellView(t *testing.T, columns []string, cells []any, obj map[string]any, colIdx int) cellView {
	t.Helper()
	app := newServer(t, baseConfig(t), time.Now())
	table := &kube.Table{
		Resource: kube.ResourceType{Plural: "nodes", Kind: "Node", Namespaced: false, Version: "v1", APIVersion: "v1"},
		Clusters: []string{"test"},
	}
	for _, name := range columns {
		table.Columns = append(table.Columns, kube.Column{Name: name})
	}
	row := kube.Row{Cells: cells, Cluster: "test", Object: obj}
	req := httptest.NewRequest(http.MethodGet, "/clusters/test/nodes", nil)
	name := nestedString(obj, "metadata", "name")
	return app.buildCellView(req, table, row, colIdx, cells[colIdx], "", name)
}

// TestNodeCellsMapRolesCapacityConditions drives the real cell-assembly for the
// rich Node columns and asserts the resolved view-model in one node row: the roles
// cell splits into chips (control-plane leads), the metrics-joined CPU capacity
// cell earns a bucket + fill + usage/capacity label, the conditions cell surfaces
// only the abnormal pill. This is the mapping proof the render test then confirms
// reaches the DOM.
func TestNodeCellsMapRolesCapacityConditions(t *testing.T) {
	// A 4-core node at 2.4 cores usage (60% -> mid), one abnormal MemoryPressure.
	cap := map[string]any{"cpu": "4", "memory": "16Gi", "pods": "110"}
	conds := []any{
		map[string]any{"type": "Ready", "status": "True"},
		map[string]any{"type": "MemoryPressure", "status": "True"},
	}
	obj := nodeObject("cp-1", cap, conds, "control-plane")

	// Columns mirror a metrics-joined node table: Name, Roles, then the joined
	// "CPU Usage"/"Memory Usage" capacity columns, Pods, Conditions.
	cols := []string{"Name", "Roles", "CPU Usage", "Memory Usage", "Pods", "Conditions"}
	cells := []any{"cp-1", "control-plane", 2.4, float64(8) * 1024 * 1024 * 1024, "110", "MemoryPressure"}

	roles := nodesCellView(t, cols, cells, obj, 1)
	if roles.Kind != cellRoles || len(roles.Roles) != 1 || roles.Roles[0] != "control-plane" {
		t.Fatalf("roles cell = %#v, want cellRoles [control-plane]", roles)
	}

	cpu := nodesCellView(t, cols, cells, obj, 2)
	if cpu.Kind != cellCapacity || cpu.CapBucket != "mid" {
		t.Fatalf("cpu capacity cell = %#v, want cellCapacity bucket=mid (60%%)", cpu)
	}
	if cpu.Value != "2.4/4" {
		t.Fatalf("cpu capacity label = %q, want 2.4/4", cpu.Value)
	}

	conditions := nodesCellView(t, cols, cells, obj, 5)
	if conditions.Kind != cellConditions || len(conditions.Conds) != 1 || conditions.Conds[0].Name != "MemoryPressure" {
		t.Fatalf("conditions cell = %#v, want cellConditions [MemoryPressure]", conditions)
	}
	if conditions.Conds[0].Tone != "warn" {
		t.Fatalf("MemoryPressure pill tone = %q, want warn", conditions.Conds[0].Tone)
	}
}

// TestCapacityBucketThresholds pins the capacity-bar bucket boundary against the
// DOCUMENTED thresholds (lifted from the mockup capCell: pct > 80 -> hi, pct > 55
// -> mid, else lo), driven through the real buildCellView for a metrics-joined CPU
// column. The fixtures feed known usage/capacity numbers so the bucket is asserted
// at the boundary (81% hi, 60% mid, 40% lo), never by echoing the emitted class.
func TestCapacityBucketThresholds(t *testing.T) {
	cols := []string{"Name", "CPU Usage"}
	cap := map[string]any{"cpu": "1"} // 1 core capacity -> usage cores == pct/100.

	// The documented thresholds are pct > 80 -> hi, pct > 55 -> mid, else lo. The
	// cases bracket each boundary on both sides with exactly-representable usages so
	// the assertion pins the bucket DIRECTION, not float equality AT the boundary.
	cases := []struct {
		usage      float64
		wantBucket string
		wantPct    int
	}{
		{0.81, "hi", 81},  // 81% -> hi (> 80)   -- the unit's hi case
		{0.85, "hi", 85},  // clearly above the hi boundary
		{0.75, "mid", 75}, // just below 80, above 55 -> mid (the hi/mid divide)
		{0.60, "mid", 60}, // 60% -> mid          -- the unit's mid case
		{0.56, "mid", 56}, // just above the mid boundary -> mid
		{0.54, "lo", 54},  // just below the mid boundary -> lo (the mid/lo divide)
		{0.40, "lo", 40},  // 40% -> lo           -- the unit's lo case
	}
	for _, c := range cases {
		obj := nodeObject("n", cap, nil)
		cv := nodesCellView(t, cols, []any{"n", c.usage}, obj, 1)
		if cv.Kind != cellCapacity {
			t.Fatalf("usage=%v: kind = %v, want cellCapacity", c.usage, cv.Kind)
		}
		if cv.CapBucket != c.wantBucket {
			t.Fatalf("usage=%v (cap 1 core): bucket = %q, want %q", c.usage, cv.CapBucket, c.wantBucket)
		}
		if cv.CapPct != c.wantPct {
			t.Fatalf("usage=%v: fill pct = %d, want %d", c.usage, cv.CapPct, c.wantPct)
		}
		if !cv.CapBar {
			t.Fatalf("usage=%v: CapBar = false, want true (metrics joined draws the bar)", c.usage)
		}
	}

	// No-metrics default: with NO ?join=metrics the column is a bare capacity column
	// ("CPU"/"Memory"), the cell has no usage -> capacity VALUE text, empty/0-width
	// bar, NO lo/mid/hi colour.
	obj := nodeObject("n", map[string]any{"cpu": "4", "memory": "16Gi"}, nil)
	cpuNoMetrics := nodesCellView(t, []string{"Name", "CPU"}, []any{"n", "4"}, obj, 1)
	if cpuNoMetrics.Kind != cellCapacity || cpuNoMetrics.CapBucket != "" || cpuNoMetrics.CapPct != 0 || cpuNoMetrics.CapBar {
		t.Fatalf("no-metrics cpu cell = %#v, want cellCapacity no bucket no fill no bar", cpuNoMetrics)
	}
	if cpuNoMetrics.Value != "4" {
		t.Fatalf("no-metrics cpu value = %q, want capacity text 4", cpuNoMetrics.Value)
	}
	memNoMetrics := nodesCellView(t, []string{"Name", "Memory"}, []any{"n", "16Gi"}, obj, 1)
	if memNoMetrics.CapBucket != "" || memNoMetrics.CapBar || memNoMetrics.Value != "16 GiB" {
		t.Fatalf("no-metrics memory cell = %#v, want value 16 GiB no bucket no bar", memNoMetrics)
	}

	// A capacity cell with usage but MISSING capacity never panics and falls back to
	// the (empty) value text with no colour.
	objNoCapacity := nodeObject("n", nil, nil)
	missing := nodesCellView(t, cols, []any{"n", 0.5}, objNoCapacity, 1)
	if missing.Kind != cellCapacity || missing.CapBucket != "" {
		t.Fatalf("missing-capacity cell = %#v, want cellCapacity no bucket (no panic)", missing)
	}
}

// TestNodeConditionsAbnormalOnly pins the condition-pill rule: only ABNORMAL
// conditions surface; a clean node yields no pills (rendered "—"). Driven through
// buildCellView, asserted against the condition semantics (Ready healthy=True;
// pressure healthy=False), never by echoing a class.
func TestNodeConditionsAbnormalOnly(t *testing.T) {
	cols := []string{"Name", "Conditions"}

	// Clean node: Ready=True, all pressures False -> NO pills.
	clean := nodeObject("ok", nil, []any{
		map[string]any{"type": "Ready", "status": "True"},
		map[string]any{"type": "MemoryPressure", "status": "False"},
		map[string]any{"type": "DiskPressure", "status": "False"},
		map[string]any{"type": "PIDPressure", "status": "False"},
	})
	cv := nodesCellView(t, cols, []any{"ok", "—"}, clean, 1)
	if cv.Kind != cellConditions || len(cv.Conds) != 0 {
		t.Fatalf("clean node conditions = %#v, want no pills (rendered —)", cv)
	}

	// NotReady node: Ready=False -> exactly one err pill (the abnormal Ready).
	notReady := nodeObject("down", nil, []any{
		map[string]any{"type": "Ready", "status": "False"},
		map[string]any{"type": "MemoryPressure", "status": "False"},
	})
	cv = nodesCellView(t, cols, []any{"down", "Ready"}, notReady, 1)
	if len(cv.Conds) != 1 || cv.Conds[0].Name != "Ready" || cv.Conds[0].Tone != "err" {
		t.Fatalf("NotReady node conditions = %#v, want one Ready/err pill", cv.Conds)
	}

	// A pressure condition that IS set surfaces as a warn pill, alongside the Ready
	// (which stays clean and is not surfaced).
	pressure := nodeObject("hot", nil, []any{
		map[string]any{"type": "Ready", "status": "True"},
		map[string]any{"type": "DiskPressure", "status": "True"},
	})
	cv = nodesCellView(t, cols, []any{"hot", "DiskPressure"}, pressure, 1)
	if len(cv.Conds) != 1 || cv.Conds[0].Name != "DiskPressure" || cv.Conds[0].Tone != "warn" {
		t.Fatalf("DiskPressure node conditions = %#v, want one DiskPressure/warn pill", cv.Conds)
	}
}

// TestRolesControlPlaneAccent pins the role-chip mapping: roles come from the
// node-role.kubernetes.io/* labels, control-plane leads and earns the .cp accent
// (asserted via the render-side roleClass), a worker stays a plain chip, and a node
// with no role label renders nothing (the "—" fallback). Driven through
// buildCellView + the render helper.
func TestRolesControlPlaneAccent(t *testing.T) {
	cols := []string{"Name", "Roles"}

	// control-plane + worker -> control-plane leads (regardless of label-map order);
	// the .cp accent on the control-plane chip is asserted through the render below.
	multi := nodeObject("cp", nil, nil, "worker", "control-plane")
	cv := nodesCellView(t, cols, []any{"cp", "control-plane,worker"}, multi, 1)
	if cv.Kind != cellRoles || len(cv.Roles) != 2 || cv.Roles[0] != "control-plane" || cv.Roles[1] != "worker" {
		t.Fatalf("multi-role cell = %#v, want cellRoles [control-plane worker]", cv)
	}
	// Render the roles cell and assert the control-plane chip earns .cp while the
	// worker chip stays plain (the render-side roleClass mapping). Scoped to the
	// .ro-table: the engine now ALSO emits the mobile `.ro-cardlist` projection of
	// the same row (the mobile cards layer), repeating the roles as a card meta row;
	// TestMobileCards pins the card projection.
	doc := renderRolesCell(t, cv.Roles)
	cpChip := doc.Find("table.ro-table .ro-role-chip.cp")
	if cpChip.Length() != 1 || normSpace(cpChip.Text()) != "control-plane" {
		t.Fatalf("control-plane chip not rendered with .cp: %s", normSpace(doc.Text()))
	}
	// The worker chip is a plain .ro-role-chip without .cp.
	plainChips := doc.Find("table.ro-table .ro-role-chip:not(.cp)")
	if plainChips.Length() != 1 || normSpace(plainChips.Text()) != "worker" {
		t.Fatalf("worker chip should be a plain .ro-role-chip (no .cp): %s", normSpace(doc.Text()))
	}

	// No role label -> no chips (the renderer shows the muted "—").
	none := nodeObject("plain", nil, nil)
	cv = nodesCellView(t, cols, []any{"plain", "<none>"}, none, 1)
	if cv.Kind != cellRoles || len(cv.Roles) != 0 {
		t.Fatalf("no-role cell = %#v, want cellRoles with no chips", cv)
	}
	emptyDoc := renderRolesCell(t, cv.Roles)
	// Scoped to the .ro-table cell: the engine now ALSO emits the mobile
	// `.ro-cardlist` projection of the same row (the mobile cards layer), which renders its own
	// muted "—" for the empty roles meta, so an unscoped `.faint` text would
	// concatenate both; TestMobileCards pins the card projection.
	if emptyDoc.Find("table.ro-table .ro-role-chip").Length() != 0 || normSpace(emptyDoc.Find("table.ro-table .faint").Text()) != "—" {
		t.Fatalf("no-role cell should render the muted —, got %q", normSpace(emptyDoc.Text()))
	}
}

// renderRolesCell renders a single roles cell (a CellRoles TableCell) through the
// ResourceTable templ and parses it, so the role chip class mapping (control-plane
// -> .cp) is asserted on the real markup.
func renderRolesCell(t *testing.T, roles []string) *goquery.Document {
	t.Helper()
	d := templates.ListData{Tables: []templates.TableData{{
		Kind: "Nodes", Count: 1, ColumnCount: 1,
		Columns: []templates.TableColumn{{Name: "Roles"}},
		Rows: []templates.TableRow{{Cells: []templates.TableCell{
			{Kind: templates.CellRoles, Roles: roles},
		}}},
	}}}
	return renderResourceTable(t, &d)
}

// TestNodeListRendersRichCellsThroughRender drives the rich node cells through the
// REAL render pipeline (toTableData -> ResourceTable templ) over a crafted node
// listView and asserts the redesign DOM: a coloured capacity bar with a fill width
// + value, the control-plane role chip with .cp, an abnormal condition pill, and
// the clean node's "—". This is the render-side confirmation that the assembly
// mapping reaches the markup.
func TestNodeListRendersRichCellsThroughRender(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())

	cap := map[string]any{"cpu": "4", "memory": "16Gi", "pods": "110"}
	cpObj := nodeObject("cp-1", cap, []any{
		map[string]any{"type": "Ready", "status": "True"},
		map[string]any{"type": "MemoryPressure", "status": "True"},
	}, "control-plane")
	workerObj := nodeObject("worker-1", cap, []any{
		map[string]any{"type": "Ready", "status": "True"},
	}, "worker")
	// A worker node at 60% CPU (mid bucket) that is also NotReady (Ready=False ->
	// an abnormal err condition pill). One row closes BOTH the .cap.mid and the
	// .ro-cond-pill.err DOM gaps.
	downObj := nodeObject("down-1", cap, []any{
		map[string]any{"type": "Ready", "status": "False"},
	}, "worker")

	table := &kube.Table{
		Resource: kube.ResourceType{Plural: "nodes", Kind: "Node", Namespaced: false, Version: "v1", APIVersion: "v1"},
		Clusters: []string{"test"},
		Columns: []kube.Column{
			{Name: "Name"}, {Name: "Roles"}, {Name: "CPU Usage"}, {Name: "Conditions"},
		},
		Rows: []kube.Row{
			{Cluster: "test", Object: cpObj, Cells: []any{"cp-1", "control-plane", 3.4, "MemoryPressure"}},
			{Cluster: "test", Object: workerObj, Cells: []any{"worker-1", "worker", 1.0, "—"}},
			{Cluster: "test", Object: downObj, Cells: []any{"down-1", "worker", 2.4, "Ready"}},
		},
	}
	lc := &listContext{Cluster: "test", Plural: "nodes", ClusterCount: 1, Tables: []kube.Table{*table}}
	req := httptest.NewRequest(http.MethodGet, "/clusters/test/nodes?join=metrics", nil)
	v := app.buildListView(req, lc)
	doc := renderListView(t, &v)

	cpRow := doc.Find(`tr:has(td.cell-name a:contains("cp-1"))`)
	if cpRow.Length() == 0 {
		t.Fatalf("cp-1 node row missing")
	}
	// CPU 3.4/4 -> 85% -> .cap.hi with a filled bar (i width) + the value text.
	if cpRow.Find(".cap.hi .cap-bar > i").Length() != 1 {
		t.Fatalf("cp-1 cpu cell missing .cap.hi > .cap-bar > i: %s", normSpace(cpRow.Text()))
	}
	width, _ := cpRow.Find(".cap.hi .cap-bar > i").Attr("style")
	if !strings.Contains(width, "width:85%") {
		t.Fatalf("cp-1 cpu bar width = %q, want width:85%%", width)
	}
	if got := normSpace(cpRow.Find(".cap-val").Text()); got != "3.4/4" {
		t.Fatalf("cp-1 cpu value = %q, want 3.4/4", got)
	}
	// Roles: control-plane chip carries .cp.
	if cpRow.Find(".ro-role-chip.cp").Length() != 1 {
		t.Fatalf("cp-1 control-plane chip missing .cp")
	}
	if got := normSpace(cpRow.Find(".ro-role-chip.cp").Text()); got != "control-plane" {
		t.Fatalf("cp-1 role chip text = %q, want control-plane", got)
	}
	// Abnormal MemoryPressure -> a warn condition pill with a TONED dot + name. The
	// dot must carry the `warn` tone class (`.ro-dot.warn`), matching the err-pill
	// dot assertion below -- both pin "the tone rides on the dot, not just the pill".
	if cpRow.Find(".ro-cond-pill.warn .ro-cond-name").Length() != 1 {
		t.Fatalf("cp-1 missing .ro-cond-pill.warn > .ro-cond-name")
	}
	if cpRow.Find(".ro-cond-pill.warn .ro-dot.warn").Length() != 1 {
		t.Fatalf("cp-1 warn condition dot missing the .ro-dot.warn tone class")
	}
	if got := normSpace(cpRow.Find(".ro-cond-pill .ro-cond-name").Text()); got != "MemoryPressure" {
		t.Fatalf("cp-1 condition name = %q, want MemoryPressure", got)
	}

	// The clean worker row shows the muted "—" for conditions and a plain role chip
	// (no .cp).
	workerRow := doc.Find(`tr:has(td.cell-name a:contains("worker-1"))`)
	if workerRow.Find(".ro-cond-pill").Length() != 0 {
		t.Fatalf("clean worker must render no condition pill")
	}
	if workerRow.Find(".ro-role-chip.cp").Length() != 0 {
		t.Fatalf("worker role chip must not carry .cp")
	}
	// CPU 1.0/4 -> 25% -> .cap.lo (the green/low bucket) reaches the DOM with its
	// fill bar + width. Closes the .cap.lo render-coverage gap.
	if workerRow.Find(".cap.lo .cap-bar > i").Length() != 1 {
		t.Fatalf("worker-1 cpu cell missing .cap.lo > .cap-bar > i: %s", normSpace(workerRow.Text()))
	}
	if width, _ := workerRow.Find(".cap.lo .cap-bar > i").Attr("style"); !strings.Contains(width, "width:25%") {
		t.Fatalf("worker-1 cpu bar width = %q, want width:25%%", width)
	}

	// The down-1 row is at CPU 2.4/4 -> 60% -> .cap.mid (the amber/mid bucket) AND
	// NotReady (Ready=False -> an err condition pill). Closes the .cap.mid and the
	// .ro-cond-pill.err render-coverage gaps in one row.
	downRow := doc.Find(`tr:has(td.cell-name a:contains("down-1"))`)
	if downRow.Length() == 0 {
		t.Fatalf("down-1 node row missing")
	}
	if downRow.Find(".cap.mid .cap-bar > i").Length() != 1 {
		t.Fatalf("down-1 cpu cell missing .cap.mid > .cap-bar > i: %s", normSpace(downRow.Text()))
	}
	if width, _ := downRow.Find(".cap.mid .cap-bar > i").Attr("style"); !strings.Contains(width, "width:60%") {
		t.Fatalf("down-1 cpu bar width = %q, want width:60%%", width)
	}
	if downRow.Find(".ro-cond-pill.err .ro-cond-name").Length() != 1 {
		t.Fatalf("down-1 missing .ro-cond-pill.err > .ro-cond-name: %s", normSpace(downRow.Text()))
	}
	if got := normSpace(downRow.Find(".ro-cond-pill.err .ro-cond-name").Text()); got != "Ready" {
		t.Fatalf("down-1 err condition name = %q, want Ready (the NotReady condition)", got)
	}
	// The err pill's DOT must carry the `err` tone class (`.ro-dot.err`), not a
	// bare `.ro-dot`: the dot color is driven by the tone class ON the dot via the
	// global `.ro-dot.err` background rule, and the `.ro-rd .ro-cond-pill` overlay
	// only tints ok/warn dots -- so a bare dot on an err/mute condition (the
	// NotReady node, the most severe state) renders INVISIBLE. Asserting
	// `.ro-cond-pill.err .ro-dot.err` makes that regression trippable: reverting the
	// emit site to a bare `<span class="ro-dot">` fails this (the `.err` tone would
	// be absent from the dot).
	if downRow.Find(".ro-cond-pill.err .ro-dot.err").Length() != 1 {
		t.Fatalf("down-1 err condition dot missing the .ro-dot.err tone class (invisible-dot regression): %s", normSpace(downRow.Text()))
	}
	// down-1 is a worker -> its role chip must NOT carry .cp.
	if downRow.Find(".ro-role-chip.cp").Length() != 0 {
		t.Fatalf("down-1 worker role chip must not carry .cp")
	}

	// Whole-table counts. Only cp-1 is control-plane, so exactly one .cp chip. Two
	// abnormal condition pills now reach the DOM (cp-1 MemoryPressure=warn, down-1
	// Ready=err), so the total pill count is 2 -- and asserting exactly one warn +
	// one err keeps a broadened-tone regression (e.g. condPillClass mapping the
	// wrong token) trippable. Scoped to the .ro-table: the engine now ALSO emits the
	// mobile `.ro-cardlist` projection of the same rows (the mobile cards layer), which repeats the
	// roles + condition pills as card meta rows; TestMobileCards pins the projection.
	if doc.Find("table.ro-table .ro-role-chip.cp").Length() != 1 {
		t.Fatalf(".cp chip count = %d, want 1", doc.Find("table.ro-table .ro-role-chip.cp").Length())
	}
	if doc.Find("table.ro-table .ro-cond-pill").Length() != 2 {
		t.Fatalf("condition pill count = %d, want 2 (cp-1 warn + down-1 err)", doc.Find("table.ro-table .ro-cond-pill").Length())
	}
	if doc.Find("table.ro-table .ro-cond-pill.warn").Length() != 1 {
		t.Fatalf("warn condition pill count = %d, want 1 (cp-1 MemoryPressure)", doc.Find("table.ro-table .ro-cond-pill.warn").Length())
	}
	if doc.Find("table.ro-table .ro-cond-pill.err").Length() != 1 {
		t.Fatalf("err condition pill count = %d, want 1 (down-1 Ready)", doc.Find("table.ro-table .ro-cond-pill.err").Length())
	}
	// All three capacity buckets reach the DOM in this single render (lo/mid/hi), so
	// a templ regression that broke capClass for any one bucket token trips here.
	for _, bucket := range []string{".cap.lo", ".cap.mid", ".cap.hi"} {
		if doc.Find(bucket).Length() == 0 {
			t.Fatalf("capacity bucket %s missing from the node render", bucket)
		}
	}
}

// newNodeMetricsRaggedAPI is a fake kube API whose metrics DISCOVERY succeeds but
// whose NodeMetrics LIST call returns 500 -- the precise shape of the ragged-rows
// hazard: applyMetricsUsage appends the CPU/Memory columns up front, but the
// fetch fails after discovery, so without the placeholder guard the rows are
// left short two cells.
func newNodeMetricsRaggedAPI(t *testing.T) *httptest.Server {
	t.Helper()
	cluster := podsScenarioCluster()
	wire := buildWire(t, &cluster)
	mux := http.NewServeMux()
	registerDiscovery(mux, wire, plainWrap)
	// Metrics DISCOVERY (the resource list) succeeds, but the NodeMetrics LIST 500s.
	mux.HandleFunc("/apis/metrics.k8s.io/v1beta1/nodes", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom: metrics backend unavailable", http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// TestMetricsRaggedGuard pins the no-ragged-rows invariant: when metrics discovery
// succeeds but the metrics LIST call fails (fetchMetricsUsage returns nil),
// applyMetricsUsage MUST still append the two placeholder cells to EVERY row, so
// the row cell count stays equal to the column count (no ragged table). It drives
// the real fetch+apply pair against a node table with a metrics-list-failure
// fake, then asserts column/row cell parity. Reverting the list-failure guard
// (the bare `return`) makes this fail (rows left two cells short).
func TestMetricsRaggedGuard(t *testing.T) {
	api := newNodeMetricsRaggedAPI(t)
	app := newTestServerWithConfig(t, &config.Config{
		Port:         8080,
		Clusters:     []config.ClusterConnection{{Name: "test", Server: api.URL}},
		DefaultTheme: "dark",
		NoAccessLogs: true,
	})
	cluster, ok := app.manager.Get("test")
	if !ok {
		t.Fatal("test cluster missing")
	}

	table := kube.Table{
		Resource: kube.ResourceType{Group: "", Version: "v1", APIVersion: "v1", Plural: "nodes", Kind: "Node", Namespaced: false},
		Columns:  []kube.Column{{Name: "Name"}, {Name: "Status"}},
		Rows: []kube.Row{
			{Cells: []any{"worker-1", "Ready"}, Object: nodeObject("worker-1", map[string]any{"cpu": "4"}, nil)},
			{Cells: []any{"worker-2", "Ready"}, Object: nodeObject("worker-2", map[string]any{"cpu": "4"}, nil)},
		},
	}
	ctx := httptest.NewRequest(http.MethodGet, "/clusters/test/nodes?join=metrics", nil).Context()
	applyMetricsUsage(&table, app.fetchMetricsUsage(ctx, cluster.Client, table.Resource.Namespaced, "", false, ""))

	// applyMetricsUsage appended the two metrics columns.
	if got := len(table.Columns); got != 4 {
		t.Fatalf("columns after applyMetricsUsage = %d, want 4 (Name,Status,CPU Usage,Memory Usage)", got)
	}
	// EVERY row must have a cell count equal to the column count (no ragged rows).
	for i := range table.Rows {
		if got := len(table.Rows[i].Cells); got != len(table.Columns) {
			t.Fatalf("row %d cell count = %d, want %d (== column count); metrics-failure left a ragged row",
				i, got, len(table.Columns))
		}
	}
}

// TestMetricsRaggedGuardRendersThroughHandler closes the ragged guard at the
// PRODUCTION boundary: a real /clusters/test/nodes?join=metrics request against a
// node fixture whose metrics list fails must still render every node row with a
// cell per column (the table never goes ragged) and never 500/panic. The node
// table here carries real node rows (unlike the placeholder fixture in the base
// fake), so the rich node cells + the metrics placeholders both render.
func TestMetricsRaggedGuardRendersThroughHandler(t *testing.T) {
	api := newNodeListMetricsFailAPI(t)
	app := newTestServerWithConfig(t, &config.Config{
		Port:         8080,
		Clusters:     []config.ClusterConnection{{Name: "test", Server: api.URL}},
		DefaultTheme: "dark",
		NoAccessLogs: true,
	})
	p := get(t, app, "/clusters/test/nodes?join=metrics", http.StatusOK)

	// The canonical table renders with both node rows.
	p.wantHas(".ro-table-wrap table.ro-table")
	if got := p.texts("td.cell-name"); strings.Join(got, "|") != "worker-1|worker-2" {
		t.Fatalf("node name cells = %v, want [worker-1 worker-2]", got)
	}
	// No row is ragged: every body row has the same cell count as the header.
	headerCells := p.doc.Find("table.ro-table thead tr th").Length()
	p.doc.Find("table.ro-table tbody tr").Each(func(_ int, row *goquery.Selection) {
		if got := row.Find("td").Length(); got != headerCells {
			t.Fatalf("ragged row: %d cells vs %d header columns", got, headerCells)
		}
	})
	// The metrics CPU/Memory columns are present (applyMetricsUsage ran) even
	// though the metrics list failed.
	headers := p.texts("table.ro-table thead th")
	assertContainsAll(t, "node metrics headers", headers, "CPU Usage", "Memory Usage")
}

// newNodeListMetricsFailAPI serves a real node Table (two worker nodes carrying
// status.capacity) for the resource-list path, with metrics DISCOVERY succeeding
// but the NodeMetrics LIST failing -- the production-boundary twin of
// newNodeMetricsRaggedAPI.
func newNodeListMetricsFailAPI(t *testing.T) *httptest.Server {
	t.Helper()
	cluster := podsScenarioCluster()
	wire := buildWire(t, &cluster)
	mux := http.NewServeMux()
	registerDiscovery(mux, wire, plainWrap)
	mux.HandleFunc("/api/v1/nodes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(nodesTableJSON))
	})
	mux.HandleFunc("/apis/metrics.k8s.io/v1beta1/nodes", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom: metrics backend unavailable", http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// nodesTableJSON is a server-side node Table (includeObject=Object) with two
// worker nodes carrying status.capacity, so the resource-list path renders real
// node rows whose rich cells read the node status.
const nodesTableJSON = `{
  "kind": "Table",
  "apiVersion": "meta.k8s.io/v1",
  "columnDefinitions": [
    {"name": "Name", "type": "string", "format": "name"},
    {"name": "Status", "type": "string"},
    {"name": "Roles", "type": "string"},
    {"name": "Version", "type": "string"}
  ],
  "rows": [
    {"cells": ["worker-1", "Ready", "worker", "v1.31.1"], "object": {
      "kind": "Node", "apiVersion": "v1",
      "metadata": {"name": "worker-1", "creationTimestamp": "2024-02-01T08:00:00Z", "labels": {"node-role.kubernetes.io/worker": ""}},
      "status": {"capacity": {"cpu": "4", "memory": "16Gi", "pods": "110"}, "conditions": [{"type": "Ready", "status": "True"}]}
    }},
    {"cells": ["worker-2", "Ready", "worker", "v1.31.1"], "object": {
      "kind": "Node", "apiVersion": "v1",
      "metadata": {"name": "worker-2", "creationTimestamp": "2024-02-01T08:00:00Z", "labels": {"node-role.kubernetes.io/worker": ""}},
      "status": {"capacity": {"cpu": "4", "memory": "16Gi", "pods": "110"}, "conditions": [{"type": "Ready", "status": "True"}]}
    }}
  ]
}`
