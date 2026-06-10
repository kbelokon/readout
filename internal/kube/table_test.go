package kube

import (
	"reflect"
	"testing"
)

func sampleTable() Table {
	return Table{
		Resource: ResourceType{Plural: "pods", Kind: "Pod", Namespaced: true},
		Columns: []Column{
			{Name: "Name"},
			{Name: "Status"},
			{Name: "Restarts"},
			{Name: "Age"},
		},
		Rows: []Row{
			{Cells: []any{"api", "Running", "0", "2d"}, Object: map[string]any{"metadata": map[string]any{"name": "api", "namespace": "prod", "creationTimestamp": "2026-01-02T00:00:00Z", "labels": map[string]any{"app": "api", "tier": "web"}}}},
			{Cells: []any{"worker", "Pending", "5", "1d"}, Object: map[string]any{"metadata": map[string]any{"name": "worker", "namespace": "dev", "creationTimestamp": "2026-01-03T00:00:00Z", "labels": map[string]any{"app": "worker"}}}},
			{Cells: []any{"cron", "Completed", "0", "3d"}, Object: map[string]any{"metadata": map[string]any{"name": "cron", "namespace": "prod", "creationTimestamp": "2026-01-01T00:00:00Z", "labels": map[string]any{"app": "cron"}}}},
		},
		Clusters: []string{"one"},
	}
}

func TestSortTableByColumnCreatedAndAge(t *testing.T) {
	table := sampleTable()
	SortTable(&table, "Name:desc")
	if got := []any{table.Rows[0].Cells[0], table.Rows[2].Cells[0]}; !reflect.DeepEqual(got, []any{"worker", "api"}) {
		t.Fatalf("name desc sort = %#v", got)
	}
	SortTable(&table, "Created")
	if got := table.Rows[0].Cells[0]; got != "cron" {
		t.Fatalf("created sort first = %v", got)
	}
	SortTable(&table, "Age")
	if got := table.Rows[0].Cells[0]; got != "worker" {
		t.Fatalf("age sort first = %v", got)
	}
	table = Table{
		Columns: []Column{{Name: "Status"}, {Name: "Name"}},
		Rows: []Row{
			{Cells: []any{"Running", "zeta"}},
			{Cells: []any{"Running", "alpha"}},
			{Cells: nil},
		},
	}
	SortTable(&table, "Status")
	if got := table.Rows[0].Cells; len(got) != 0 {
		t.Fatalf("empty first cell should sort first on tie: %#v", table.Rows)
	}
}

func TestSortTableNumericColumnSortsByValue(t *testing.T) {
	// Memory/CPU usage cells are raw float bytes/cores. fmt.Sprint renders them in
	// scientific notation, so a lexicographic compare mis-ordered them (95 MiB
	// landing after 942 MiB). They must sort by numeric value.
	table := Table{
		Columns: []Column{{Name: "Name"}, {Name: "Memory Usage"}},
		Rows: []Row{
			{Cells: []any{"a", float64(95 * 1024 * 1024)}},
			{Cells: []any{"b", float64(1 * 1024 * 1024)}},
			{Cells: []any{"c", float64(942 * 1024 * 1024)}},
		},
	}
	SortTable(&table, "Memory Usage:desc")
	got := []any{table.Rows[0].Cells[0], table.Rows[1].Cells[0], table.Rows[2].Cells[0]}
	if !reflect.DeepEqual(got, []any{"c", "a", "b"}) {
		t.Fatalf("numeric desc sort = %#v, want [c a b] (942 > 95 > 1 MiB)", got)
	}
	SortTable(&table, "Memory Usage:asc")
	if got := table.Rows[0].Cells[0]; got != "b" {
		t.Fatalf("numeric asc sort first = %v, want b (1 MiB)", got)
	}
}

func TestSortTableQuantityColumnSortsByValue(t *testing.T) {
	// Node capacity cells are k8s quantity strings ("8138032Ki", "16Gi"). A
	// lexicographic compare mis-ordered them ("8138032Ki" < "16Gi" as text); they
	// must sort by the parsed byte value.
	table := Table{
		Columns: []Column{{Name: "Name"}, {Name: "Memory"}},
		Rows: []Row{
			{Cells: []any{"a", "16Gi"}},
			{Cells: []any{"b", "8138032Ki"}},
			{Cells: []any{"c", "32Gi"}},
		},
	}
	SortTable(&table, "Memory:desc")
	got := []any{table.Rows[0].Cells[0], table.Rows[1].Cells[0], table.Rows[2].Cells[0]}
	if !reflect.DeepEqual(got, []any{"c", "a", "b"}) {
		t.Fatalf("quantity desc sort = %#v, want [c a b] (32Gi > 16Gi > ~7.8GiB)", got)
	}
}

func TestLabelHideFilterAndNamespaceTransforms(t *testing.T) {
	table := sampleTable()
	AddLabelColumns(&table, "app,*")
	if got := columnNames(table.Columns); !reflect.DeepEqual(got[:3], []string{"Name", "App", "Labels"}) {
		t.Fatalf("columns after labels = %#v", got)
	}
	if table.Rows[0].Cells[1] != "api" || table.Rows[0].Cells[2] != "app=api,tier=web" {
		t.Fatalf("label cells not inserted: %#v", table.Rows[0].Cells)
	}
	RemoveColumns(&table, "Age,Labels")
	if got := columnNames(table.Columns); !reflect.DeepEqual(got, []string{"Name", "App", "Status", "Restarts"}) {
		t.Fatalf("unexpected columns after removal = %#v", got)
	}
	FilterTable(&table, "Status=Running,api", false)
	if len(table.Rows) != 1 || table.Rows[0].Cells[0] != "api" {
		t.Fatalf("filter result = %#v", table.Rows)
	}

	table = sampleTable()
	AddLabelColumns(&table, "app")
	FilterTable(&table, "App=worker", false)
	if len(table.Rows) != 1 || table.Rows[0].Cells[0] != "worker" {
		t.Fatalf("label column filter result = %#v", table.Rows)
	}
	table = sampleTable()
	FilterTable(&table, "web", true)
	if len(table.Rows) != 1 || table.Rows[0].Cells[0] != "api" {
		t.Fatalf("label text filter result = %#v", table.Rows)
	}
	table = sampleTable()
	FilterRowsByNamespace(&table, res("prod"), res("dev"))
	if len(table.Rows) != 2 {
		t.Fatalf("namespace filter result = %#v", table.Rows)
	}

	table = sampleTable()
	FilterTable(&table, "Missing=value", false)
	if len(table.Rows) != 0 {
		t.Fatalf("missing filter column should remove all rows: %#v", table.Rows)
	}
	table = sampleTable()
	FilterTable(&table, "Status!=Pending", false)
	if len(table.Rows) != 2 || table.Rows[0].Cells[0] != "api" || table.Rows[1].Cells[0] != "cron" {
		t.Fatalf("not-equals filter result = %#v", table.Rows)
	}
	table = sampleTable()
	RemoveColumns(&table, "*")
	if len(table.Columns) != 0 || len(table.Rows[0].Cells) != 0 {
		t.Fatalf("wildcard removal failed: %#v %#v", table.Columns, table.Rows[0].Cells)
	}
}

func TestMergeTablesAndClasses(t *testing.T) {
	left := Table{
		Resource: ResourceType{Plural: "pods"},
		Columns:  []Column{{Name: "Name"}, {Name: "Status"}},
		Rows:     []Row{{Cells: []any{"api", "Running"}}},
		Clusters: []string{"one"},
	}
	right := Table{
		Resource: ResourceType{Plural: "pods"},
		Columns:  []Column{{Name: "Name"}, {Name: "Restarts"}},
		Rows:     []Row{{Cells: []any{"worker", "5"}}},
		Clusters: []string{"two"},
	}
	if !MergeTables(&left, &right) {
		t.Fatal("expected merge to succeed")
	}
	if got := columnNames(left.Columns); !reflect.DeepEqual(got, []string{"Name", "Status", "Restarts"}) {
		t.Fatalf("merged columns = %#v", got)
	}
	if len(left.Rows[0].Cells) != 3 || left.Rows[1].Cells[2] != "5" || len(left.Clusters) != 2 {
		t.Fatalf("merged rows/clusters = %#v %#v", left.Rows, left.Clusters)
	}
	if MergeTables(&left, &Table{Resource: ResourceType{Plural: "services"}}) {
		t.Fatal("merge with different plural should fail")
	}
	same := Table{
		Resource: ResourceType{Plural: "pods"},
		Columns:  []Column{{Name: "Name"}},
		Rows:     []Row{{Cells: []any{"one"}}},
		Clusters: []string{"one"},
	}
	if !MergeTables(&same, &Table{Resource: ResourceType{Plural: "pods"}, Columns: []Column{{Name: "Name"}}, Rows: []Row{{Cells: []any{"two"}}}, Clusters: []string{"two"}}) {
		t.Fatal("expected equal-column merge to succeed")
	}
	if len(same.Rows) != 2 || len(same.Columns) != 1 || !reflect.DeepEqual(same.Clusters, []string{"one", "two"}) {
		t.Fatalf("equal-column merge = %#v", same)
	}

	classes := Table{Columns: []Column{{Name: "Name"}, {Name: "Count"}}, Rows: []Row{{Cells: []any{"x", 3}}}}
	GuessColumnClasses(&classes)
	if classes.Columns[1].Class != "num" {
		t.Fatalf("numeric class not guessed: %#v", classes.Columns)
	}
}

func TestMetricsUsageDecodesQuantitiesForPodAndNode(t *testing.T) {
	// PodMetrics: a per-container usage list is decoded typed and summed via
	// resource.Quantity. Two containers (250m + 100m cpu, 128Mi + 2Ki memory)
	// exercise the cpu (m), binary-Mi, and binary-Ki suffixes. The values are
	// the intended resource.Quantity outputs (== the retired hand-rolled
	// parser's on these inputs; resource.ParseQuantity additionally handles the
	// Pi/Ei/exponent edge inputs the old parser dropped).
	pod := map[string]any{
		"kind": "PodMetrics", "apiVersion": "metrics.k8s.io/v1beta1",
		"metadata": map[string]any{"name": "nginx", "namespace": "default"},
		"containers": []any{
			map[string]any{"name": "a", "usage": map[string]any{"cpu": "250m", "memory": "128Mi"}},
			map[string]any{"name": "b", "usage": map[string]any{"cpu": "100m", "memory": "2Ki"}},
		},
	}
	key, cpu, mem := MetricsUsage(pod)
	if key != "default/nginx" || cpu != 0.35 || mem != float64(128*1024*1024+2048) {
		t.Fatalf("PodMetrics usage = key=%q cpu=%v mem=%v, want default/nginx 0.35 %v", key, cpu, mem, float64(128*1024*1024+2048))
	}
	// NodeMetrics: a single top-level usage map (cpu "3" cores, memory "0" zero).
	node := map[string]any{
		"kind": "NodeMetrics", "apiVersion": "metrics.k8s.io/v1beta1",
		"metadata": map[string]any{"name": "worker-1"},
		"usage":    map[string]any{"cpu": "3", "memory": "0"},
	}
	key, cpu, mem = MetricsUsage(node)
	if key != "/worker-1" || cpu != 3 || mem != 0 {
		t.Fatalf("NodeMetrics usage = key=%q cpu=%v mem=%v, want /worker-1 3 0", key, cpu, mem)
	}
	// An empty/missing usage decodes to zero, not a panic (the nil-on-missing
	// quantity behavior).
	key, cpu, mem = MetricsUsage(map[string]any{"metadata": map[string]any{"name": "x", "namespace": "y"}})
	if key != "y/x" || cpu != 0 || mem != 0 {
		t.Fatalf("empty usage = key=%q cpu=%v mem=%v, want y/x 0 0", key, cpu, mem)
	}
	// A present-but-unparseable quantity fails the typed decode as a WHOLE: the
	// item drops to an empty key + zero values (the seam converts the usage map
	// in one FromUnstructured pass, unlike the retired per-field parser, which
	// zeroed only the bad field and kept the key). Unreachable from a real
	// metrics-server — it always emits valid quantities — but pinned so the
	// FromUnstructured error branch and its empty-key behavior stay intentional:
	// a regression returning a partial value or a non-empty key on malformed
	// input would fail here.
	key, cpu, mem = MetricsUsage(map[string]any{
		"metadata": map[string]any{"name": "x", "namespace": "y"},
		"usage":    map[string]any{"cpu": "bad", "memory": "128Mi"},
	})
	if key != "" || cpu != 0 || mem != 0 {
		t.Fatalf("malformed usage = key=%q cpu=%v mem=%v, want \"\" 0 0 (whole-item drop)", key, cpu, mem)
	}
}

func TestStatusHelpers(t *testing.T) {
	// Pin every arm of the three enum methods directly: slug() is the
	// row-status-<slug> CSS source, class() the has-text-* cell color, label()
	// the chip text. A mistyped arm (e.g. statusWarn.slug() -> "warning") would
	// silently break the CSS stripe and is otherwise unguarded for info/warn.
	for _, c := range []struct {
		s     status
		slug  string
		class string
		label string
	}{
		{statusNeutral, "neutral", "has-text-grey", "Neutral"},
		{statusOK, "ok", "has-text-success", "OK"},
		{statusInfo, "info", "has-text-info", "Info"},
		{statusWarn, "warn", "has-text-warning", "Warning"},
		{statusErr, "err", "has-text-danger", "Error"},
	} {
		if c.s.slug() != c.slug || c.s.class() != c.class || c.s.label() != c.label {
			t.Fatalf("status %d: slug=%q class=%q label=%q want %q/%q/%q", c.s, c.s.slug(), c.s.class(), c.s.label(), c.slug, c.class, c.label)
		}
	}
	// iota order is the strength rank (neutral weakest, err strongest), so the
	// strongest-wins rowStatus comparison is a plain >.
	ranks := []status{statusNeutral, statusOK, statusInfo, statusWarn, statusErr}
	for i := 1; i < len(ranks); i++ {
		if ranks[i-1] >= ranks[i] {
			t.Fatalf("status rank order broken at %d: %d >= %d", i, ranks[i-1], ranks[i])
		}
	}
	if got := splitCSV(" a, ,b "); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("splitCSV = %#v", got)
	}
	if !equalStrings([]string{"a"}, []string{"a"}) || equalStrings([]string{"a"}, []string{"b"}) {
		t.Fatal("equalStrings mismatch")
	}
}

// specStatusToneRows is the SPEC §3 status table verbatim -- one case per row,
// including the Init:* prefix family and the explicit mute fallback. It backs
// both the direct StatusTone pin and the CellClass delegation proof, so the
// vocabulary is written down exactly once in this package's tests.
var specStatusToneRows = []struct {
	value string
	tone  string
}{
	// ok
	{"Running", "ok"},
	{"Ready", "ok"},
	{"Active", "ok"},
	{"Bound", "ok"},
	{"Complete", "ok"},
	// mute
	{"Completed", "mute"},
	{"Succeeded", "mute"},
	{"Normal", "mute"},
	{"Suspended", "mute"},
	// warn
	{"Pending", "warn"},
	{"ContainerCreating", "warn"},
	{"PodInitializing", "warn"},
	{"Terminating", "warn"},
	{"Warning", "warn"},
	{"Released", "warn"},
	{"Init:0/1", "warn"},
	{"Init:1/2", "warn"},
	// err
	{"CrashLoopBackOff", "err"},
	{"Error", "err"},
	{"Failed", "err"},
	{"NotReady", "err"},
	{"OOMKilled", "err"},
	{"ImagePullBackOff", "err"},
	{"Evicted", "err"},
	{"BackoffLimitExceeded", "err"},
	// err — D4 amendment 2026-06-10 (user-approved): terminal pod failure
	// words v1 rendered red; SPEC §3 omitted them and the mute fallback was
	// a signal regression on real clusters.
	{"ErrImagePull", "err"},
	{"CreateContainerConfigError", "err"},
	{"InvalidImageName", "err"},
	{"OutOfcpu", "err"},
	{"Init:CrashLoopBackOff", "err"},
	{"Init:Error", "err"},
	{"Init:ImagePullBackOff", "err"},
	{"Init:CreateContainerConfigError", "err"},
	// fallback: anything outside the table is mute (shown grey, never invented)
	{"SomeOperatorPhase", "mute"},
	{"Ready,SchedulingDisabled", "mute"},
	{"", "mute"},
}

// TestStatusToneSpecTable pins kube.StatusTone -- THE single value->tone
// mapping (SPEC §3) -- row by row, including the Init:* error/backoff split and
// the mute fallback.
func TestStatusToneSpecTable(t *testing.T) {
	for _, c := range specStatusToneRows {
		if got := StatusTone(c.value); got != c.tone {
			t.Fatalf("StatusTone(%q) = %q, want %q", c.value, got, c.tone)
		}
	}
	// Whitespace-tolerant like CellClass's trimmed values.
	if got := StatusTone("  Running "); got != "ok" {
		t.Fatalf("StatusTone untrimmed = %q, want ok", got)
	}
}

// TestCellClassDelegatesStatusWordsToStatusTone is the delegation proof for the
// single-owner law (D4): for EVERY SPEC §3 word, CellClass's class for a Status
// column is exactly the statusToneClass encoding of StatusTone -- across the
// previously hand-switched plurals (pods/namespaces/nodes/pv/pvc), kinds that
// never had a switch (jobs), an unknown plural, and the events Type column
// (Normal/Warning are SPEC vocabulary too). A re-introduced per-plural status
// override in CellClass cannot pass this.
func TestCellClassDelegatesStatusWordsToStatusTone(t *testing.T) {
	plurals := []string{"pods", "namespaces", "nodes", "persistentvolumes", "persistentvolumeclaims", "jobs", "widgets"}
	for _, c := range specStatusToneRows {
		want := statusToneClass(StatusTone(c.value))
		for _, plural := range plurals {
			if got := CellClass(plural, "Status", c.value); got != want {
				t.Fatalf("CellClass(%q, Status, %q) = %q, want %q (disagrees with StatusTone)", plural, c.value, got, want)
			}
		}
		if got := CellClass("events", "Type", c.value); got != want {
			t.Fatalf("CellClass(events, Type, %q) = %q, want %q (disagrees with StatusTone)", c.value, got, want)
		}
	}
	// The encoding itself is lossless for the four tones the table speaks.
	enc := map[string]string{"ok": "has-text-success", "warn": "has-text-warning", "err": "has-text-danger", "mute": "has-text-grey"}
	for tone, class := range enc {
		if got := statusToneClass(tone); got != class {
			t.Fatalf("statusToneClass(%q) = %q, want %q", tone, got, class)
		}
	}
}

// TestRowStatusStripeOnlyErrAndWarn pins the SPEC §3 stripe rule: ONLY err and
// warn rows carry a row-status-* class. A Running (ok) row and a Completed
// (mute -> neutral) row carry none -- the "warn excluding Completed" SPEC
// clause holds with no special case because Completed never tones warn. The
// kept events Reason map still feeds the stripe (a danger Reason stripes err).
func TestRowStatusStripeOnlyErrAndWarn(t *testing.T) {
	pods := Table{
		Resource: ResourceType{Plural: "pods", Kind: "Pod"},
		Columns:  []Column{{Name: "Name"}, {Name: "Status"}},
	}
	cases := []struct {
		status string
		want   string
	}{
		{"Running", ""},
		{"Completed", ""},
		{"Pending", "row-status-warn"},
		{"Init:1/2", "row-status-warn"},
		{"CrashLoopBackOff", "row-status-err"},
		{"Init:CrashLoopBackOff", "row-status-err"},
		{"SomeOperatorPhase", ""},
	}
	for _, c := range cases {
		row := Row{Cells: []any{"p", c.status}}
		if got := RowStatusClass(&pods, row); got != c.want {
			t.Fatalf("pods %q stripe = %q, want %q", c.status, got, c.want)
		}
	}
	events := Table{
		Resource: ResourceType{Plural: "events", Kind: "Event"},
		Columns:  []Column{{Name: "Type"}, {Name: "Reason"}},
	}
	if got := RowStatusClass(&events, Row{Cells: []any{"Warning", "FailedScheduling"}}); got != "row-status-err" {
		t.Fatalf("events danger-Reason stripe = %q, want row-status-err", got)
	}
	// An ok-Reason row (Started) must NOT stripe green -- ok stripes are retired.
	if got := RowStatusClass(&events, Row{Cells: []any{"Normal", "Started"}}); got != "" {
		t.Fatalf("events ok-Reason stripe = %q, want none", got)
	}
}

func TestPhaseSummaryForPodsUsesStatusCellLabels(t *testing.T) {
	table := Table{
		Resource: ResourceType{Plural: "pods", Kind: "Pod"},
		Columns:  []Column{{Name: "Name"}, {Name: "Status"}},
		Rows: []Row{
			{Cells: []any{"one", "Running"}},
			{Cells: []any{"two", "Completed"}},
			{Cells: []any{"three", "Running"}},
			{Cells: []any{"four", "ImagePullBackOff"}},
		},
	}
	got := PhaseSummary(&table)
	want := []struct {
		label string
		count int
		class string
	}{
		{"Running", 2, "has-text-success"},
		// Completed is mute under SPEC §3 (a finished pod is history, not info).
		{"Completed", 1, "has-text-grey"},
		{"ImagePullBackOff", 1, "has-text-danger"},
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Label != want[i].label || got[i].Count != want[i].count || got[i].Class != want[i].class {
			t.Fatalf("chip %d = %#v, want label=%q count=%d class=%q", i, got[i], want[i].label, want[i].count, want[i].class)
		}
	}
}

func TestPhaseSummaryRequiresFormattedResource(t *testing.T) {
	table := Table{
		Resource: ResourceType{Plural: "cronjobs", Kind: "CronJob"},
		Columns:  []Column{{Name: "Name"}, {Name: "Active"}},
		Rows:     []Row{{Cells: []any{"cleanup", "0"}}},
	}
	if got := PhaseSummary(&table); len(got) != 0 {
		t.Fatalf("cronjob phase summary = %#v, want empty", got)
	}
}

func TestRowStatusClassNames(t *testing.T) {
	table := Table{
		Resource: ResourceType{Plural: "pods", Kind: "Pod"},
		Columns:  []Column{{Name: "Status"}},
	}
	// SPEC §3: ok rows carry NO stripe class (only err/warn stripe).
	row := Row{Cells: []any{"Running"}}
	if got := RowStatusClass(&table, row); got != "" {
		t.Fatalf("class = %q, want none for an ok row", got)
	}
	table = Table{
		Resource: ResourceType{Plural: "events", Kind: "Event"},
		Columns:  []Column{{Name: "Type"}, {Name: "Reason"}},
		Rows: []Row{
			{Cells: []any{"Warning", "FailedScheduling"}},
			{Cells: []any{"Normal", "Started"}},
			{Cells: []any{"", ""}},
		},
	}
	if got := RowStatusClass(&table, table.Rows[0]); got != "row-status-err" {
		t.Fatalf("event error class = %q", got)
	}
	summary := PhaseSummary(&table)
	if len(summary) != 3 || summary[0].Label != "Error" || summary[1].Label != "OK" || summary[2].Label != "Neutral" {
		t.Fatalf("event phase summary = %#v", summary)
	}
}
