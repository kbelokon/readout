package web

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/kbelokon/readout/internal/kube"
	"github.com/kbelokon/readout/internal/web/templates"
	"k8s.io/client-go/util/jsonpath"
)

// TestBuildEventViewsNormalizesBothEventShapes proves the dual-shape events
// decode: the core/v1 `events` endpoint dual-writes BOTH the old core/v1 Event
// shape AND the newer events.k8s.io/v1 shape, and buildEventViews normalizes
// the spelling differences with the PINNED last-seen precedence:
//   - last-seen: series.lastObservedTime -> lastTimestamp ->
//     deprecatedLastTimestamp -> eventTime, rendered as the compressed
//     duration since (the full timestamp moves into the AgeTitle tooltip)
//   - message: message -> note
//   - from:    source.component -> reportingController
func TestBuildEventViewsNormalizesBothEventShapes(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC))

	events := []map[string]any{
		// Old core/v1 shape: lastTimestamp / message / source.component.
		{
			"type": "Normal", "reason": "Scheduled",
			"message":       "old-style assigned",
			"lastTimestamp": "2024-03-01T10:00:05Z",
			"source":        map[string]any{"component": "default-scheduler"},
		},
		// New events.k8s.io/v1 shape: eventTime / note / reportingController.
		// These used to render "Unknown" age + empty message/from.
		{
			"type": "Warning", "reason": "BackOff",
			"note":                "new-style back-off",
			"eventTime":           "2024-03-02T11:00:00Z",
			"reportingController": "kubelet",
		},
		// Series shape: series.lastObservedTime OUTRANKS the also-present
		// eventTime (the series aggregate is the freshest observation).
		{
			"type": "Normal", "reason": "Pulled",
			"note":      "from series",
			"eventTime": "2024-03-01T00:00:00Z",
			"series":    map[string]any{"lastObservedTime": "2024-03-03T12:00:00Z"},
		},
		// New shape with a RECENT eventTime (1h before the fixed clock) so its
		// age bucket is age-fresh, not age-old. This makes the AgeClass check
		// below non-vacuous: ageClass returns age-old for an empty/unparseable
		// timestamp, so seeing age-fresh proves the normalized eventTime actually
		// reaches ageClass (a normalization regression to "" would flip it).
		{
			"type": "Normal", "reason": "Created",
			"note":                "fresh new-style",
			"eventTime":           "2024-05-31T23:00:00Z",
			"reportingController": "kubelet",
		},
	}
	views := app.buildEventViews(events)
	if len(views) != 4 {
		t.Fatalf("event views = %d, want 4", len(views))
	}

	// Old-style row: the compressed duration since lastTimestamp (Mar 1 ->
	// Jun 1 = 91d) with the full timestamp in the tooltip; a countless event
	// occurred once -> the faint ×1 (the count defaults to 1 for a countless event).
	if views[0].Age != "91d" || views[0].Message != "old-style assigned" || views[0].From != "default-scheduler" {
		t.Fatalf("old-style event view = %#v", views[0])
	}
	if views[0].AgeTitle != "last seen 2024-03-01 10:00:05" {
		t.Fatalf("old-style AgeTitle = %q, want the full last-seen timestamp tooltip", views[0].AgeTitle)
	}
	if views[0].Count != "1" || views[0].CountClass != "faint" {
		t.Fatalf("countless event count = ×%q class %q, want the faint ×1", views[0].Count, views[0].CountClass)
	}
	// New-style row: eventTime -> age, note -> message, reportingController ->
	// from. The Warning type maps to the redesign "warn" tone (via the SAME
	// CellClass the list path uses, then statusTone), proving normalization still
	// feeds the cell classer; a Normal event with no class defaults to "mute".
	if views[1].Age != "90d" || views[1].Message != "new-style back-off" || views[1].From != "kubelet" {
		t.Fatalf("new-style event view = %#v, want eventTime/note/reportingController normalized", views[1])
	}
	if views[1].Tone != "warn" {
		t.Fatalf("new-style Warning event tone = %q, want warn", views[1].Tone)
	}
	if views[0].Tone != "mute" {
		t.Fatalf("Normal event tone = %q, want mute (no kube class -> mute default)", views[0].Tone)
	}
	// Series row: series.lastObservedTime (Mar 3 -> 89d) beats the older
	// eventTime (92d) — the head of the last-seen precedence chain.
	if views[2].Age != "89d" || views[2].Message != "from series" {
		t.Fatalf("series event view = %#v, want the series.lastObservedTime age", views[2])
	}
	// Recent new-style row: eventTime normalizes AND flows to the age classer
	// (1h before the clock -> "60m").
	if views[3].Age != "60m" || views[3].From != "kubelet" {
		t.Fatalf("recent new-style event view = %#v", views[3])
	}
	// The first three timestamps are months before the fixed clock (age-old);
	// the fourth is 1h before it (age-fresh). The fresh row is the non-vacuous
	// check: an empty/unparseable timestamp would also be age-old, so age-fresh
	// is what actually proves the normalized eventTime reaches ageClass.
	wantAge := []string{"age-old", "age-old", "age-old", "age-fresh"}
	for i, v := range views {
		if v.AgeClass != wantAge[i] {
			t.Fatalf("event %d age class = %q, want %q", i, v.AgeClass, wantAge[i])
		}
	}
}

// build_assembly_test.go exercises the data-assembly layer (build_*.go) in
// isolation — a payoff of the handler -> view-model -> render seam. These tests
// drive the assembly functions directly (no HTTP round trip, no render) over the
// hermetic fake API, asserting the non-trivial joins and the cell view model.

// TestJoinMetricsJoinsCPUAndMemoryByObjectKey proves the metrics join: it sums
// per-container usage keyed by namespace/name and appends two numeric cells per
// row (the joined pod gets its usage; an unmatched pod gets zeros). The fake API
// metrics fixture has default/nginx at cpu=250m, memory=128Mi.
func TestJoinMetricsJoinsCPUAndMemoryByObjectKey(t *testing.T) {
	app := newTestServer(t)
	cluster, ok := app.manager.Get("test")
	if !ok {
		t.Fatal("test cluster missing")
	}
	table := kube.Table{
		Resource: kube.ResourceType{Group: "", Version: "v1", APIVersion: "v1", Plural: "pods", Kind: "Pod", Namespaced: true},
		Columns:  []kube.Column{{Name: "Name"}},
		Rows: []kube.Row{
			{Cells: []any{"nginx"}, Object: map[string]any{"metadata": map[string]any{"name": "nginx", "namespace": "default"}}},
			{Cells: []any{"other"}, Object: map[string]any{"metadata": map[string]any{"name": "other", "namespace": "default"}}},
		},
	}
	ctx := httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods?join=metrics", nil).Context()
	applyMetricsUsage(&table, app.fetchMetricsUsage(ctx, cluster.Client, table.Resource.Namespaced, "default", false, ""))

	if len(table.Columns) != 3 || table.Columns[1].Name != "CPU Usage" || table.Columns[2].Name != "Memory Usage" {
		t.Fatalf("metrics columns not appended: %#v", table.Columns)
	}
	// nginx: 250m -> 0.25 CPU, 128Mi -> 134217728 bytes.
	const wantMem = float64(128 * 1024 * 1024)
	cpu, ok := table.Rows[0].Cells[1].(float64)
	if !ok || cpu != 0.25 {
		t.Fatalf("nginx CPU cell = %#v want 0.25", table.Rows[0].Cells[1])
	}
	mem, ok := table.Rows[0].Cells[2].(float64)
	if !ok || mem != wantMem {
		t.Fatalf("nginx memory cell = %#v want %v", table.Rows[0].Cells[2], wantMem)
	}
	// Unmatched pod gets zero-valued usage cells (the [2]float64 zero value).
	if table.Rows[1].Cells[1] != float64(0) || table.Rows[1].Cells[2] != float64(0) {
		t.Fatalf("unmatched pod usage = %#v / %#v want 0/0", table.Rows[1].Cells[1], table.Rows[1].Cells[2])
	}

	// And the cell view model formats those joined values exactly as the table
	// render emits them: "250m" and "128" (MiB), under cellCPU / cellMemory.
	r := httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods?join=metrics", nil)
	cpuCell := app.buildCellView(r, &table, table.Rows[0], 1, table.Rows[0].Cells[1], "default", "nginx")
	if cpuCell.Kind != cellCPU || cpuCell.Value != "250m" {
		t.Fatalf("cpu cellView = %#v want kind=cellCPU value=250m", cpuCell)
	}
	memCell := app.buildCellView(r, &table, table.Rows[0], 2, table.Rows[0].Cells[2], "default", "nginx")
	if memCell.Kind != cellMemory || memCell.Value != "128" {
		t.Fatalf("memory cellView = %#v want kind=cellMemory value=128", memCell)
	}
}

// TestJoinCustomColumnsEvaluatesJSONPath proves the kubectl-style JSONPath
// custom-columns join happy path: a parsed expression is evaluated against the
// live object fetched for each row, and the result is appended as a new cell.
// The fake API pod list fixture has default/nginx with spec.containers[0].image
// set. Both a "Name=expr" spelling and a bare expression (column name derived via
// humanTitle, exercising the relaxer's {.expr} auto-wrap) are covered.
func TestJoinCustomColumnsEvaluatesJSONPath(t *testing.T) {
	app := newTestServer(t)
	cluster, ok := app.manager.Get("test")
	if !ok {
		t.Fatal("test cluster missing")
	}
	table := kube.Table{
		Resource: kube.ResourceType{Group: "", Version: "v1", APIVersion: "v1", Plural: "pods", Kind: "Pod", Namespaced: true},
		Columns:  []kube.Column{{Name: "Name"}},
		Rows: []kube.Row{
			{Cells: []any{"nginx"}, Object: map[string]any{"metadata": map[string]any{"name": "nginx", "namespace": "default"}}},
		},
	}
	ctx := httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods", nil).Context()
	app.joinCustomColumns(ctx, cluster.Client, &table, "default", false, "Image=spec.containers[0].image", nil)

	if len(table.Columns) != 2 || table.Columns[1].Name != "Image" {
		t.Fatalf("custom column not appended: %#v", table.Columns)
	}
	if len(table.Rows[0].Cells) != 2 {
		t.Fatalf("custom cell not appended: %#v", table.Rows[0].Cells)
	}
	image, ok := table.Rows[0].Cells[1].(string)
	if !ok || image != "nginx:1.27" {
		t.Fatalf("JSONPath image cell = %#v want \"nginx:1.27\"", table.Rows[0].Cells[1])
	}
	// The default column name is derived via humanTitle when no "Name=" prefix is
	// given; confirm that path too (bare expression relaxed + parsed, column
	// titled "Metadata Name").
	bare := kube.Table{
		Resource: table.Resource,
		Columns:  []kube.Column{{Name: "Name"}},
		Rows: []kube.Row{
			{Cells: []any{"nginx"}, Object: map[string]any{"metadata": map[string]any{"name": "nginx", "namespace": "default"}}},
		},
	}
	app.joinCustomColumns(ctx, cluster.Client, &bare, "default", false, "metadata.name", nil)
	if len(bare.Columns) != 2 || bare.Columns[1].Name != "Metadata Name" {
		t.Fatalf("humanTitle column name = %#v want 'Metadata Name'", bare.Columns)
	}
	if bare.Rows[0].Cells[1] != "nginx" {
		t.Fatalf("bare JSONPath cell = %#v want nginx", bare.Rows[0].Cells[1])
	}

	// A missing path through the REAL engine construction (not the seam helper)
	// yields an empty cell, not an error and not kubectl's "<none>" sentinel.
	// This pins joinCustomColumns' AllowMissingKeys(true): without it the engine
	// errors on the absent key and the cell would carry the error/sentinel.
	missing := kube.Table{
		Resource: table.Resource,
		Columns:  []kube.Column{{Name: "Name"}},
		Rows: []kube.Row{
			{Cells: []any{"nginx"}, Object: map[string]any{"metadata": map[string]any{"name": "nginx", "namespace": "default"}}},
		},
	}
	app.joinCustomColumns(ctx, cluster.Client, &missing, "default", false, "Absent=metadata.labels.absent", nil)
	if missing.Rows[0].Cells[1] != "" {
		t.Fatalf("missing custom-column path = %#v want empty string (not <none>/error)", missing.Rows[0].Cells[1])
	}
}

// TestCustomColumnJSONPathRelaxAndEval pins the three behavior changes of the
// kubectl-style JSONPath custom-column engine at the evaluation seam,
// deterministically and without depending on the shared fixtures:
//   - the relaxer auto-wraps a bare path as a {.path} template, and an explicit
//     {.path} template is passed through unchanged;
//   - a MISSING path yields "" (not an error), because the engine is built with
//     AllowMissingKeys(true);
//   - a MULTI-VALUE match (every container image) renders SPACE-JOINED
//     ("nginx:1.27 redis:7.2"), the kubectl `-o jsonpath` rendering -- NOT the
//     Go-slice-bracketed "[nginx:1.27 redis:7.2]" the old raw value produced.
func TestCustomColumnJSONPathRelaxAndEval(t *testing.T) {
	// relaxer: bare path is wrapped; an explicit {.} template is left alone.
	if got := relaxJSONPath("spec.containers[*].image"); got != "{.spec.containers[*].image}" {
		t.Fatalf("relaxJSONPath(bare) = %q want {.spec.containers[*].image}", got)
	}
	if got := relaxJSONPath("{.metadata.name}"); got != "{.metadata.name}" {
		t.Fatalf("relaxJSONPath(template) = %q want it unchanged", got)
	}
	// A leading dot is stripped (kubectl-style), NOT doubled into "{..path}":
	// the engine reads a leading ".." as the recursive-descent operator, which
	// would match same-named keys nested anywhere and leak ghost values.
	if got := relaxJSONPath(".metadata.name"); got != "{.metadata.name}" {
		t.Fatalf("relaxJSONPath(leading-dot) = %q want {.metadata.name} (no recursive-descent ..)", got)
	}

	pod := map[string]any{
		"metadata": map[string]any{"name": "web"},
		"spec": map[string]any{"containers": []any{
			map[string]any{"image": "nginx:1.27"},
			map[string]any{"image": "redis:7.2"},
		}},
	}

	eval := func(expr string) string {
		jp := newCustomColumnJSONPath(t, expr)
		return evalJSONPath(jp, pod)
	}

	// Bare single-value path.
	if got := eval("metadata.name"); got != "web" {
		t.Fatalf("single-value bare path = %q want \"web\"", got)
	}
	// Multi-value path renders space-joined, not slice-bracketed.
	if got := eval("spec.containers[*].image"); got != "nginx:1.27 redis:7.2" {
		t.Fatalf("multi-value path = %q want \"nginx:1.27 redis:7.2\"", got)
	}
	// Missing path yields empty (AllowMissingKeys), never an error.
	if got := eval("metadata.labels.missing"); got != "" {
		t.Fatalf("missing path = %q want \"\"", got)
	}
	// A leading-dot expression navigates children, not recursive descent: a
	// same-named key nested elsewhere must NOT leak. Pre-fix this returned
	// "top ghost"; the stripped leading dot keeps it child-scoped to "top".
	ghost := map[string]any{
		"metadata": map[string]any{"name": "top"},
		"spec":     map[string]any{"nested": map[string]any{"metadata": map[string]any{"name": "ghost"}}},
	}
	if got := evalJSONPath(newCustomColumnJSONPath(t, ".metadata.name"), ghost); got != "top" {
		t.Fatalf("leading-dot eval = %q want \"top\" (no recursive-descent ghost)", got)
	}
}

// newCustomColumnJSONPath parses a relaxed custom-column expression exactly as
// joinCustomColumns does (AllowMissingKeys + relaxer), failing the test if the
// expression does not parse.
func newCustomColumnJSONPath(t *testing.T, expr string) *jsonpath.JSONPath {
	t.Helper()
	jp := jsonpath.New("test").AllowMissingKeys(true)
	if err := jp.Parse(relaxJSONPath(expr)); err != nil {
		t.Fatalf("parse %q: %v", expr, err)
	}
	return jp
}

// TestHasLogTimestamp guards the log-grouping decision: a line whose first
// space-delimited token parses as RFC3339[Nano] starts a fresh log entry; any
// other line is folded into the previous one. A pre-2000 timestamp must be
// recognized and a non-timestamped continuation line must not — neither is
// exercised by the all-RFC3339 logs fixture, so without this the predicate has
// no regression guard.
func TestHasLogTimestamp(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{"2026-01-02T15:04:05Z GET / 200", true},
		{"2026-01-02T15:04:05.123456789Z stdout F hi", true}, // RFC3339Nano
		{"1999-12-31T23:59:59Z pre-2000 line", true},         // old "20" heuristic missed this
		{"  at com.example.Frame(Frame.java:42)", false},     // wrapped stack-trace continuation
		{"notatimestamp something", false},
		{"singletoken-no-space", false},
		{"", false},
	}
	for _, c := range cases {
		if got := hasLogTimestamp(c.line); got != c.want {
			t.Fatalf("hasLogTimestamp(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}

// TestToTableDataPhaseChipCarry pins the bridge: kube.PhaseCount -> templates
// .PhaseChip. The kube-side typed output and the rendered Label/Count are pinned
// elsewhere, but the field-for-field carry here (incl. the int->string Count via
// strconv.Itoa AND the Bulma-tone -> redesign-tone mapping) had no isolated
// guard, so a Tone/Label swap or a wrong Count conversion could slip. The Tone is
// asserted against the DOCUMENTED mapping (has-text-danger->err,
// has-text-success->ok), not by echoing the emitted class.
func TestToTableDataPhaseChipCarry(t *testing.T) {
	td := toTableData(&tableView{
		Phase: []kube.PhaseCount{
			{Class: "has-text-danger", Label: "Error", Count: 3},
			{Class: "has-text-success", Label: "OK", Count: 0},
		},
	})
	want := []templates.PhaseChip{
		{Tone: "err", Label: "Error", Count: "3"},
		{Tone: "ok", Label: "OK", Count: "0"},
	}
	if !reflect.DeepEqual(td.Phase, want) {
		t.Fatalf("PhaseChip carry = %#v, want %#v", td.Phase, want)
	}
}
