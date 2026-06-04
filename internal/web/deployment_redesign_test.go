package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/kbelokon/readout/internal/kube"
	"github.com/kbelokon/readout/internal/web/templates"
)

// deployment_redesign_test.go pins the redesign behavioral invariants of the
// DEPLOYMENT rich cells (the replica track + the rollout pill) added to the
// canonical resource-list engine. Every expectation is an INDEPENDENT fact about
// how a Kubernetes Deployment status/spec maps onto the redesign vocabulary,
// asserted against the documented semantics (segments from readyReplicas /
// updatedReplicas / spec.replicas; rollout from status conditions + spec.paused;
// the track capped so a 500-replica deployment never explodes the DOM), never an
// echo of the emitted class. The mapping is driven through the REAL pipeline
// (buildCellView / buildListView) from a crafted deployments Table, not
// re-implemented in the test.

// deploymentObject builds a deployment Row.Object carrying the spec/status the
// rich deployment cells read: spec.replicas (desired), spec.paused, and the
// status replica counters + conditions. desired defaults via spec.replicas; the
// status counters and conditions are supplied per case.
func deploymentObject(name string, desired int, status map[string]any, paused bool) map[string]any {
	spec := map[string]any{"replicas": int64(desired)}
	if paused {
		spec["paused"] = true
	}
	return map[string]any{
		"kind":       "Deployment",
		"apiVersion": "apps/v1",
		"metadata": map[string]any{
			"name":              name,
			"namespace":         "default",
			"creationTimestamp": "2024-03-01T10:00:00Z",
			"labels":            map[string]any{"app": name},
		},
		"spec":   spec,
		"status": status,
	}
}

// depStatus is a compact status map builder for the replica counters + an
// optional Progressing-condition reason (the rollout-completeness signal).
func depStatus(replicas, ready, updated, available int, progressingReason string) map[string]any {
	st := map[string]any{
		"replicas":          int64(replicas),
		"readyReplicas":     int64(ready),
		"updatedReplicas":   int64(updated),
		"availableReplicas": int64(available),
	}
	if progressingReason != "" {
		st["conditions"] = []any{
			map[string]any{"type": "Progressing", "status": "True", "reason": progressingReason},
		}
	}
	return st
}

// deploymentsCellView runs the real buildCellView for one cell of a crafted
// deployments table, so the deployment-cell mapping (replica track / rollout) is
// asserted end to end from a Kubernetes Deployment object, not re-implemented in
// the test. obj is the row object; columns/cells/colIdx address the cell under
// test.
func deploymentsCellView(t *testing.T, columns []string, cells []any, obj map[string]any, colIdx int) cellView {
	t.Helper()
	app := newServer(t, baseConfig(t), time.Now())
	table := &kube.Table{
		Resource: kube.ResourceType{Group: "apps", Plural: "deployments", Kind: "Deployment", Namespaced: true, Version: "v1", APIVersion: "apps/v1"},
		Clusters: []string{"test"},
	}
	for _, name := range columns {
		table.Columns = append(table.Columns, kube.Column{Name: name})
	}
	row := kube.Row{Cells: cells, Cluster: "test", Object: obj}
	req := httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/deployments", nil)
	name := nestedString(obj, "metadata", "name")
	return app.buildCellView(req, table, row, colIdx, cells[colIdx], "default", name)
}

// TestDeploymentCells drives the real cell-assembly for the rich Deployment
// columns and asserts the resolved view-model in one deployment row: the Ready
// column becomes the replica track (readyReplicas=3, updatedReplicas=4,
// spec.replicas=5 -> 3 filled + 1 updating + 1 pending segments, .rep-num 3/5
// partial), and the synthetic Rollout column becomes the rollout pill. This is the
// mapping proof the render test then confirms reaches the DOM.
func TestDeploymentCells(t *testing.T) {
	// A mid-rollout deployment: 5 desired, 3 ready, 4 updated, 3 available.
	obj := deploymentObject("api", 5, depStatus(5, 3, 4, 3, "ReplicaSetUpdated"), false)
	cols := []string{"Name", "Ready", "Up-to-date", "Available", "Age", "Rollout"}
	cells := []any{"api", "3/5", "4", "3", "5m", "rolling out"}

	rep := deploymentsCellView(t, cols, cells, obj, 1)
	if rep.Kind != cellReplicas {
		t.Fatalf("Ready cell kind = %v, want cellReplicas", rep.Kind)
	}
	if rep.RepNum != "3/5" {
		t.Fatalf("rep-num = %q, want 3/5", rep.RepNum)
	}
	if rep.Ratio != "partial" {
		t.Fatalf("rep ratio = %q, want partial (3 of 5 ready)", rep.Ratio)
	}
	// Independent expectation: 3 filled (ready) + 1 updating (updated beyond ready)
	// + 1 pending (desired beyond updated) = 5 segments.
	filled, updating, pending := countSegments(rep.RepSegments)
	if filled != 3 || updating != 1 || pending != 1 {
		t.Fatalf("segments filled/updating/pending = %d/%d/%d, want 3/1/1", filled, updating, pending)
	}

	roll := deploymentsCellView(t, cols, cells, obj, 5)
	if roll.Kind != cellRollout {
		t.Fatalf("Rollout cell kind = %v, want cellRollout", roll.Kind)
	}
	// Mid-rollout (not complete) -> prog ("rolling out").
	if roll.RolloutState != "prog" || roll.Value != "rolling out" {
		t.Fatalf("rollout = %q/%q, want prog/'rolling out'", roll.RolloutState, roll.Value)
	}
}

// countSegments tallies a replica-track's filled ("")/updating/pending segments so
// the test asserts the COUNTS, never the raw class string the renderer emits.
func countSegments(segs []repSegment) (filled, updating, pending int) {
	for _, s := range segs {
		switch s.State {
		case "":
			filled++
		case "updating":
			updating++
		case "pending":
			pending++
		}
	}
	return filled, updating, pending
}

// TestReplicaTrack pins the segment-state mapping against the DOCUMENTED semantics
// (ready segments filled, [ready,updated) updating, [updated,desired) pending) and
// the .rep-num ratio tone (all-ready full, some-ready partial, none-ready zero),
// driven through the real buildCellView for a deployments Ready column. The
// fixtures feed known replica counters so each bucket is asserted from the status,
// never by echoing the emitted class.
func TestReplicaTrack(t *testing.T) {
	cols := []string{"Name", "Ready"}
	cases := []struct {
		name                                  string
		desired, ready, updated               int
		wantFilled, wantUpdating, wantPending int
		wantNum                               string
		wantRatio                             string
	}{
		// The unit's canonical case: 5 desired, 3 ready, 4 updated.
		{"mid-rollout", 5, 3, 4, 3, 1, 1, "3/5", "partial"},
		// Fully ready & updated -> all filled, full tone.
		{"complete", 4, 4, 4, 4, 0, 0, "4/4", "full"},
		// Scaled up, nothing ready yet -> all pending, zero tone.
		{"cold", 3, 0, 0, 0, 0, 3, "0/3", "zero"},
		// All replicas updated to the new revision but not all are ready yet: 3
		// desired, 2 ready, 3 updated -> 2 filled (ready) + 1 updating (updated beyond
		// ready, the new pod still coming up) + 0 pending (nothing un-updated left).
		{"updated-not-ready", 3, 2, 3, 2, 1, 0, "2/3", "partial"},
		// A single-replica deployment fully ready -> one filled segment, full.
		{"singleton", 1, 1, 1, 1, 0, 0, "1/1", "full"},
		// Scaled up with the extra replicas not yet created (updated == ready): the
		// ready ones are filled, the missing ones are pending, none updating.
		{"scaling-up", 4, 2, 2, 2, 0, 2, "2/4", "partial"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obj := deploymentObject(c.name, c.desired, depStatus(c.desired, c.ready, c.updated, c.ready, ""), false)
			cv := deploymentsCellView(t, cols, []any{c.name, c.wantNum}, obj, 1)
			if cv.Kind != cellReplicas {
				t.Fatalf("kind = %v, want cellReplicas", cv.Kind)
			}
			if cv.RepNum != c.wantNum {
				t.Fatalf("rep-num = %q, want %q", cv.RepNum, c.wantNum)
			}
			if cv.Ratio != c.wantRatio {
				t.Fatalf("ratio = %q, want %q", cv.Ratio, c.wantRatio)
			}
			filled, updating, pending := countSegments(cv.RepSegments)
			if filled != c.wantFilled || updating != c.wantUpdating || pending != c.wantPending {
				t.Fatalf("segments filled/updating/pending = %d/%d/%d, want %d/%d/%d",
					filled, updating, pending, c.wantFilled, c.wantUpdating, c.wantPending)
			}
			// The total rendered segment count equals desired (under the cap).
			if got := len(cv.RepSegments); got != c.desired {
				t.Fatalf("segment count = %d, want %d (one per desired under cap)", got, c.desired)
			}
		})
	}
}

// TestRollout pins the rollout-state derivation against the DOCUMENTED rules:
// spec.paused -> paused; a deployment whose replica counts all meet desired AND
// whose Progressing condition reports NewReplicaSetAvailable -> done (up to date);
// anything mid-flight -> prog (rolling out). Driven through buildCellView, then the
// done/paused/prog markup confirmed through the render pipeline (the .rollout.<state>
// class + the icon), never by echoing the class.
func TestRollout(t *testing.T) {
	cols := []string{"Name", "Rollout"}

	// Complete: all counters meet desired + Progressing reason NewReplicaSetAvailable.
	complete := deploymentObject("done", 3, depStatus(3, 3, 3, 3, "NewReplicaSetAvailable"), false)
	cv := deploymentsCellView(t, cols, []any{"done", "up to date"}, complete, 1)
	if cv.RolloutState != "done" || cv.Value != "up to date" {
		t.Fatalf("complete rollout = %q/%q, want done/'up to date'", cv.RolloutState, cv.Value)
	}

	// Paused wins even when the counts look complete.
	paused := deploymentObject("hold", 3, depStatus(3, 3, 3, 3, "NewReplicaSetAvailable"), true)
	cv = deploymentsCellView(t, cols, []any{"hold", "paused"}, paused, 1)
	if cv.RolloutState != "paused" || cv.Value != "paused" {
		t.Fatalf("paused rollout = %q/%q, want paused/'paused'", cv.RolloutState, cv.Value)
	}

	// Mid-flight: counts do not meet desired -> prog.
	rolling := deploymentObject("roll", 4, depStatus(4, 2, 3, 2, "ReplicaSetUpdated"), false)
	cv = deploymentsCellView(t, cols, []any{"roll", "rolling out"}, rolling, 1)
	if cv.RolloutState != "prog" {
		t.Fatalf("rolling rollout = %q, want prog", cv.RolloutState)
	}

	// Counts complete but Progressing still reports an in-flight reason -> prog (the
	// condition gate, not just the counts, decides done).
	stillProgressing := deploymentObject("slow", 2, depStatus(2, 2, 2, 2, "ReplicaSetUpdated"), false)
	cv = deploymentsCellView(t, cols, []any{"slow", "rolling out"}, stillProgressing, 1)
	if cv.RolloutState != "prog" {
		t.Fatalf("counts-complete-but-progressing rollout = %q, want prog", cv.RolloutState)
	}

	// Render-side confirmation: the three states map to .rollout.done/.paused/.prog
	// with the expected icon glyph reaching the DOM.
	assertRolloutRenders(t, "done", "up to date")
	assertRolloutRenders(t, "paused", "paused")
	assertRolloutRenders(t, "prog", "rolling out")
}

// assertRolloutRenders renders a single rollout cell (a CellRollout TableCell)
// through the ResourceTable templ and asserts the .rollout.<state> class + the
// label reach the markup, with a non-empty icon SVG.
func assertRolloutRenders(t *testing.T, state, label string) {
	t.Helper()
	d := templates.ListData{Tables: []templates.TableData{{
		Kind: "Deployments", Count: 1, ColumnCount: 1,
		Columns: []templates.TableColumn{{Name: "Rollout"}},
		Rows: []templates.TableRow{{Cells: []templates.TableCell{
			{Kind: templates.CellRollout, RolloutState: state, Value: label, RolloutIcon: icon(rolloutIconName(state))},
		}}},
	}}}
	doc := renderResourceTable(t, &d)
	sel := doc.Find(".rollout." + state)
	if sel.Length() != 1 {
		t.Fatalf("state %q: want one .rollout.%s, got %d", state, state, sel.Length())
	}
	if !strings.Contains(normSpace(sel.Text()), label) {
		t.Fatalf("state %q: label %q not rendered, got %q", state, label, normSpace(sel.Text()))
	}
	if sel.Find("svg").Length() == 0 {
		t.Fatalf("state %q: rollout pill missing its icon svg", state)
	}
}

// TestReplicaCap pins the no-DOM-explosion invariant: a deployment with 500
// desired replicas renders AT MOST replicaTrackCap (12) `<i>` segments -- NOT 500
// -- while the .rep-num ratio span still shows the REAL ready/desired ratio (the
// source of truth beyond the cap). The cap is 12. Driven through the real render
// pipeline (buildListView -> ResourceTable templ) so the segment count is asserted
// on the actual emitted DOM, not on the view-model alone.
func TestReplicaCap(t *testing.T) {
	const cap = 12 // replicaTrackCap: the fixed segment ceiling stated by this test.
	if replicaTrackCap != cap {
		t.Fatalf("replicaTrackCap = %d, this test asserts the cap is %d", replicaTrackCap, cap)
	}

	app := newServer(t, baseConfig(t), time.Now())
	// 500 desired, 480 ready, 500 updated -> a real ratio of 480/500, partial.
	obj := deploymentObject("huge", 500, depStatus(500, 480, 500, 480, "ReplicaSetUpdated"), false)
	table := &kube.Table{
		Resource: kube.ResourceType{Group: "apps", Plural: "deployments", Kind: "Deployment", Namespaced: true, Version: "v1", APIVersion: "apps/v1"},
		Clusters: []string{"test"},
		Columns: []kube.Column{
			{Name: "Name"}, {Name: "Ready"}, {Name: "Up-to-date"}, {Name: "Available"}, {Name: "Age"},
		},
		Rows: []kube.Row{
			{Cluster: "test", Object: obj, Cells: []any{"huge", "480/500", "500", "480", "5m"}},
		},
	}
	lc := &listContext{Cluster: "test", Plural: "deployments", ClusterCount: 1, Tables: []kube.Table{*table}}
	req := httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/deployments", nil)
	v := app.buildListView(req, lc)
	doc := renderListView(t, &v)

	row := doc.Find(`tr:has(td.cell-name a:contains("huge"))`)
	if row.Length() == 0 {
		t.Fatalf("huge deployment row missing")
	}
	segs := row.Find(".rep .rep-track i")
	if segs.Length() == 0 {
		t.Fatalf("replica track rendered no segments")
	}
	if segs.Length() > cap {
		t.Fatalf("replica track rendered %d segments, want <= cap %d (no DOM explosion)", segs.Length(), cap)
	}
	// The .rep-num carries the REAL ratio (the truth beyond the cap), not a scaled
	// or truncated count.
	if got := normSpace(row.Find(".rep-num").Text()); got != "480/500" {
		t.Fatalf("rep-num = %q, want the real 480/500 ratio", got)
	}
	// 480 of 500 is partial (not all ready) -> .rep-num.partial.
	if row.Find(".rep-num.partial").Length() != 1 {
		t.Fatalf("rep-num should carry .partial for 480/500")
	}
}

// TestDeploymentListThroughHandlerPreservesGenerics drives a deployments list
// through buildListView + the render pipeline and confirms the rich replica/rollout
// cells coexist with the preserved generics: the Up-to-date / Available columns
// still render their plain numeric cells, the synthetic Rollout column lands as a
// rollout pill, and the replica track renders for the Ready column -- so
// hidecols/labelcols/customcols/sort/TSV (which all operate on the same kube.Table
// columns/cells the generic engine reads) keep working unchanged.
func TestDeploymentListThroughHandlerPreservesGenerics(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())

	doneObj := deploymentObject("web", 2, depStatus(2, 2, 2, 2, "NewReplicaSetAvailable"), false)
	rollObj := deploymentObject("worker", 3, depStatus(3, 1, 2, 1, "ReplicaSetUpdated"), false)

	table := &kube.Table{
		Resource: kube.ResourceType{Group: "apps", Plural: "deployments", Kind: "Deployment", Namespaced: true, Version: "v1", APIVersion: "apps/v1"},
		Clusters: []string{"test"},
		Columns: []kube.Column{
			{Name: "Name"}, {Name: "Ready"}, {Name: "Up-to-date"}, {Name: "Available"}, {Name: "Age"},
		},
		Rows: []kube.Row{
			{Cluster: "test", Object: doneObj, Cells: []any{"web", "2/2", "2", "2", "5m"}},
			{Cluster: "test", Object: rollObj, Cells: []any{"worker", "1/3", "2", "1", "5m"}},
		},
	}
	// Run the REAL column decoration that applyTableOptions performs for the
	// deployments plural (it appends the synthetic Rollout column + per-row cell
	// derived from status), so this test exercises the same Table shape the handler
	// builds before buildListView/render.
	decorateDeploymentColumns(table)
	lc := &listContext{Cluster: "test", Plural: "deployments", ClusterCount: 1, Tables: []kube.Table{*table}}
	req := httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/deployments", nil)
	v := app.buildListView(req, lc)
	doc := renderListView(t, &v)

	// Both replica tracks render (one per row).
	if got := doc.Find(".rep .rep-track").Length(); got != 2 {
		t.Fatalf("replica tracks = %d, want 2 (one per deployment row)", got)
	}
	// The done deployment renders a .rollout.done pill; the rolling one .rollout.prog.
	if doc.Find(".rollout.done").Length() != 1 {
		t.Fatalf("want one .rollout.done (the complete deployment)")
	}
	if doc.Find(".rollout.prog").Length() != 1 {
		t.Fatalf("want one .rollout.prog (the rolling deployment)")
	}

	// The Up-to-date / Available generic numeric cells survive: the web row's
	// Up-to-date=2 and Available=2 still appear as plain cell text (not swallowed by
	// the rich cells).
	webRow := doc.Find(`tr:has(td.cell-name a:contains("web"))`)
	if webRow.Length() == 0 {
		t.Fatalf("web deployment row missing")
	}
	cellTexts := webRow.Find("td").Map(func(_ int, s *goquery.Selection) string { return normSpace(s.Text()) })
	if !containsString(cellTexts, "2") {
		t.Fatalf("Up-to-date/Available generic cells missing from web row: %v", cellTexts)
	}

	// The header carries the synthetic Rollout column alongside the original columns,
	// so the column/cell contract the generics depend on stays intact.
	headers := doc.Find("table.ro-table thead th").Map(func(_ int, s *goquery.Selection) string { return normSpace(s.Text()) })
	assertHeader := func(name string) {
		for _, h := range headers {
			if strings.Contains(h, name) {
				return
			}
		}
		t.Fatalf("header %q missing from %v", name, headers)
	}
	for _, h := range []string{"Ready", "Up-to-date", "Available", "Rollout"} {
		assertHeader(h)
	}
}

// containsString reports whether want is present in xs (a tiny local helper so the
// generic-preservation assertions read cleanly).
func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
