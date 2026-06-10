package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/kbelokon/readout/internal/kube"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// containers_test.go pins the Unit-13 pod-detail surface (D14 / SPEC §7.15 /
// SPEC §6.6): the containers table (status.containerStatuses /
// status.initContainerStatuses joined with spec.containers + the PodMetrics
// containers[] join), the ≤120/>120 annotation chip/block split, and the
// detail-title head/hash split. Everything renders through the REAL pipeline
// (assembly -> toDetailData bridge -> the ResourceView templ) so the facts pin
// rendered structure, not view-model fields.

// containersClock is the fixed render instant: 6 days after the
// metrics-exporter container's last termination, so the restart suffix reads
// the acceptance "(6d ago)".
var containersClock = time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)

// containersPodObject builds the fixture pod: 1 init container + 2 regular
// containers, full spec/status join material (images, ports, states, ready,
// restart counts, last-termination timestamp), and the Deployment-style hash
// name the title split keys on.
func containersPodObject() *kube.Object {
	rt := kube.ResourceType{APIVersion: "v1", Version: "v1", Plural: "pods", Kind: "Pod", Namespaced: true}
	obj := kube.NewObject(&rt, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":              "parse-prod-server-9ff6cbdbb-vkrzf",
			"namespace":         "default",
			"creationTimestamp": "2024-03-01T08:00:00Z",
			"resourceVersion":   "12345",
		},
		"spec": map[string]any{
			"initContainers": []any{
				map[string]any{"name": "wait-for-mongo", "image": "busybox:1.36"},
			},
			"containers": []any{
				map[string]any{
					"name":  "parse-server",
					"image": "registry.digitalocean.com/docker-hub-do/parse-server:6.2.3",
					"ports": []any{map[string]any{"containerPort": int64(1337), "protocol": "TCP"}},
				},
				map[string]any{
					"name":  "metrics-exporter",
					"image": "prom/statsd-exporter:v0.26.1",
					"ports": []any{map[string]any{"containerPort": int64(9102), "protocol": "TCP"}},
				},
			},
		},
		"status": map[string]any{
			"phase": "Running",
			"initContainerStatuses": []any{
				map[string]any{
					"name":         "wait-for-mongo",
					"ready":        false,
					"restartCount": int64(0),
					"state":        map[string]any{"terminated": map[string]any{"reason": "Completed", "exitCode": int64(0)}},
				},
			},
			"containerStatuses": []any{
				map[string]any{
					"name":         "parse-server",
					"ready":        true,
					"restartCount": int64(0),
					"state":        map[string]any{"running": map[string]any{"startedAt": "2024-05-15T00:00:00Z"}},
				},
				map[string]any{
					"name":         "metrics-exporter",
					"ready":        true,
					"restartCount": int64(2),
					"state":        map[string]any{"running": map[string]any{"startedAt": "2024-05-26T00:05:00Z"}},
					"lastState":    map[string]any{"terminated": map[string]any{"reason": "Error", "exitCode": int64(1), "finishedAt": "2024-05-26T00:00:00Z"}},
				},
			},
		},
	}})
	return &obj
}

// renderContainersDetail assembles the fixture pod's Default-tab detail view
// (with the given metrics-join map) and renders it through the real bridge.
func renderContainersDetail(t *testing.T, usage map[string]kube.ContainerUsage) *goquery.Document {
	t.Helper()
	app := newServer(t, baseConfig(t), containersClock)
	obj := containersPodObject()
	v := buildDefaultDetailView(t, obj)
	v.Containers = app.buildContainersView(obj, usage)
	return renderDetailView(t, v)
}

// TestContainersSectionJoinOrder pins the D14 join + order law: the fixture
// pod (1 init + 2 regular) renders exactly 3 rows, init container FIRST with
// the mute `init` badge, then the regular containers in spec order; each row
// joins its status entry (state word, ready grammar, restarts) with its spec
// entry (image, ports). The section sits between the Owner line position and
// the YAML cards inside the Default tab, labelled `Containers · 2 + 1 init`.
func TestContainersSectionJoinOrder(t *testing.T) {
	doc := renderContainersDetail(t, nil)

	section := doc.Find(".ro-rd .ro-section.ro-containers")
	if section.Length() != 1 {
		t.Fatalf("expected one .ro-containers section, got %d", section.Length())
	}
	if got := normSpace(section.Find(".ro-section-label").Text()); got != "Containers · 2 + 1 init" {
		t.Fatalf("containers section label = %q, want %q", got, "Containers · 2 + 1 init")
	}

	rows := section.Find("table.ro-table tbody tr")
	if rows.Length() != 3 {
		t.Fatalf("container rows = %d, want 3 (1 init + 2 regular)", rows.Length())
	}
	names := docTexts(doc, ".ro-containers tbody td.cell-name .pn-head")
	if strings.Join(names, "|") != "wait-for-mongo|parse-server|metrics-exporter" {
		t.Fatalf("container row order = %v, want init first then spec order", names)
	}

	// The init row (and ONLY it) carries the mute `init` kind-badge.
	initRow := rows.Eq(0)
	if got := normSpace(initRow.Find(".ro-kind-badge.init").Text()); got != "init" {
		t.Fatalf("init row badge = %q, want %q", got, "init")
	}
	if got := section.Find(".ro-kind-badge.init").Length(); got != 1 {
		t.Fatalf("init badges = %d, want exactly 1 (regular rows are unbadged)", got)
	}
	// Init state: terminated reason Completed -> mute status cell; init Ready
	// stays the faint "—" (its readiness IS its completion).
	if initRow.Find("td .cell-status.mute .ro-dot.mute").Length() != 1 {
		t.Fatalf("init row state cell missing the mute Completed status pair: %s", normSpace(initRow.Text()))
	}
	if got := normSpace(initRow.Find("td").Eq(1).Text()); got != "Completed" {
		t.Fatalf("init state = %q, want Completed", got)
	}
	if got := normSpace(initRow.Find("td").Eq(2).Find("span.faint").Text()); got != "—" {
		t.Fatalf("init Ready cell = %q, want the faint —", got)
	}

	// Regular row: Running ok state, ready/full grammar, spec-joined ports +
	// image (faint, truncated, full ref in title).
	parseRow := rows.Eq(1)
	if parseRow.Find("td .cell-status.ok .ro-dot.ok").Length() != 1 {
		t.Fatalf("parse-server row missing the ok Running status pair: %s", normSpace(parseRow.Text()))
	}
	if got := normSpace(parseRow.Find("span.ready.full").Text()); got != "ready" {
		t.Fatalf("parse-server ready cell = %q, want ready/full", got)
	}
	if got := normSpace(parseRow.Find("td").Eq(4).Text()); got != "1337/TCP" {
		t.Fatalf("parse-server ports = %q, want 1337/TCP (spec join)", got)
	}
	image := parseRow.Find("td.faint span.trunc")
	if image.Length() != 1 {
		t.Fatalf("parse-server image cell missing the faint truncated span")
	}
	if title, _ := image.Attr("title"); title != "registry.digitalocean.com/docker-hub-do/parse-server:6.2.3" {
		t.Fatalf("image title = %q, want the full image ref", title)
	}
	// Restarts: zero tone for the unrestarted container; the restarted one is
	// asserted in TestContainerRestartAgoSuffix.
	if got := normSpace(parseRow.Find("span.restarts.zero").Text()); got != "0" {
		t.Fatalf("parse-server restarts = %q, want the zero-toned 0", got)
	}
}

// TestContainerRestartAgoSuffix pins the restart grammar: a container with
// restartCount > 0 renders the amber-toned count plus the faint "(6d ago)"
// suffix derived from lastState.terminated.finishedAt against the fixed clock.
func TestContainerRestartAgoSuffix(t *testing.T) {
	doc := renderContainersDetail(t, nil)
	row := doc.Find(".ro-containers tbody tr").Eq(2)
	if got := normSpace(row.Find("span.restarts.some").Text()); got != "2" {
		t.Fatalf("restarted container count = %q, want the some-toned 2", got)
	}
	if got := normSpace(row.Find("span.ago").Text()); got != "(6d ago)" {
		t.Fatalf("restart ago suffix = %q, want %q", got, "(6d ago)")
	}
}

// TestContainerMetricsJoin pins the D14 metrics rule on BOTH branches: with a
// live PodMetrics containers[] join the CPU/Memory cells show real per-
// container values; without it they show the faint "—" — never zeros invented
// for a dead join.
func TestContainerMetricsJoin(t *testing.T) {
	// Live join: per-container usage by name.
	doc := renderContainersDetail(t, map[string]kube.ContainerUsage{
		"parse-server":     {CPU: 0.21, Memory: 780 * 1024 * 1024},
		"metrics-exporter": {CPU: 0.004, Memory: 18 * 1024 * 1024},
	})
	rows := doc.Find(".ro-containers tbody tr")
	parseRow := rows.Eq(1)
	if got := normSpace(parseRow.Find("td").Eq(5).Text()); got != "210m" {
		t.Fatalf("parse-server CPU = %q, want 210m", got)
	}
	if got := normSpace(parseRow.Find("td").Eq(6).Text()); got != "780Mi" {
		t.Fatalf("parse-server Memory = %q, want 780Mi", got)
	}
	if got := normSpace(rows.Eq(2).Find("td").Eq(5).Text()); got != "4m" {
		t.Fatalf("metrics-exporter CPU = %q, want 4m", got)
	}
	// The init container has no metrics entry (it already exited): faint "—".
	if got := normSpace(rows.Eq(0).Find("td").Eq(5).Find("span.faint").Text()); got != "—" {
		t.Fatalf("init CPU cell = %q, want the faint —", got)
	}

	// Dead join (no metrics-server / fetch failed): every CPU/Memory cell is
	// the faint "—" and no fabricated zero value appears.
	noMetrics := renderContainersDetail(t, nil)
	noRows := noMetrics.Find(".ro-containers tbody tr")
	for i := 0; i < noRows.Length(); i++ {
		for _, col := range []int{5, 6} {
			cell := noRows.Eq(i).Find("td").Eq(col)
			if got := normSpace(cell.Find("span.faint").Text()); got != "—" {
				t.Fatalf("row %d col %d without metrics = %q, want the faint —", i, col, normSpace(cell.Text()))
			}
		}
	}
}

// TestContainerStateToneByReason pins the D4 law on container state cells: the
// waiting/terminated REASON is the state word, toned by kube.StatusTone (the
// single value->tone owner) — CrashLoopBackOff err (never pulsing), the
// in-flight ContainerCreating warn WITH the transient pulse (law §1.3).
func TestContainerStateToneByReason(t *testing.T) {
	rt := kube.ResourceType{APIVersion: "v1", Version: "v1", Plural: "pods", Kind: "Pod", Namespaced: true}
	obj := kube.NewObject(&rt, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]any{"name": "crash-0", "namespace": "default", "creationTimestamp": "2024-03-01T08:00:00Z", "resourceVersion": "1"},
		"spec": map[string]any{
			"containers": []any{
				map[string]any{"name": "boom", "image": "boom:1"},
				map[string]any{"name": "slow", "image": "slow:1"},
			},
		},
		"status": map[string]any{
			"containerStatuses": []any{
				map[string]any{
					"name": "boom", "ready": false, "restartCount": int64(6),
					"state":     map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff"}},
					"lastState": map[string]any{"terminated": map[string]any{"reason": "Error", "exitCode": int64(1), "finishedAt": "2024-05-31T23:00:00Z"}},
				},
				map[string]any{
					"name": "slow", "ready": false, "restartCount": int64(0),
					"state": map[string]any{"waiting": map[string]any{"reason": "ContainerCreating"}},
				},
			},
		},
	}})
	app := newServer(t, baseConfig(t), containersClock)
	v := buildDefaultDetailView(t, &obj)
	v.Containers = app.buildContainersView(&obj, nil)
	doc := renderDetailView(t, v)

	rows := doc.Find(".ro-containers tbody tr")
	boom := rows.Eq(0)
	if got := normSpace(boom.Find(".cell-status.err").Text()); got != "CrashLoopBackOff" {
		t.Fatalf("waiting-reason state = %q, want CrashLoopBackOff toned err", got)
	}
	if boom.Find(".ro-dot.err.pulse").Length() != 0 {
		t.Fatalf("CrashLoopBackOff must NOT pulse (errors never animate, law §1.3)")
	}
	if got := normSpace(boom.Find("span.ready.partial").Text()); got != "not ready" {
		t.Fatalf("crashing container ready cell = %q, want not ready/partial", got)
	}
	slow := rows.Eq(1)
	if slow.Find(".cell-status.warn .ro-dot.warn.pulse").Length() != 1 {
		t.Fatalf("ContainerCreating must render the warn-toned PULSING dot: %s", normSpace(slow.Text()))
	}
}

// TestContainersLiveMetricsThroughHandler pins the e2e wiring: the REAL
// detail handler fetches the pod's PodMetrics object (the fakeapi serves one
// for default/nginx) and the rendered page carries the live per-container
// CPU/Memory — proving buildDetailView -> podContainerMetrics ->
// kube.PodContainerUsage end to end, availability detection included.
func TestContainersLiveMetricsThroughHandler(t *testing.T) {
	app := newServer(t, baseConfig(t), containersClock)
	p := get(t, app, "/clusters/test/namespaces/default/pods/nginx", http.StatusOK)

	p.wantText(".ro-containers .ro-section-label", "Containers · 1")
	row := p.doc.Find(".ro-containers tbody tr").First()
	if got := normSpace(row.Find("td.cell-name .pn-head").Text()); got != "nginx" {
		t.Fatalf("handler-rendered container name = %q, want nginx", got)
	}
	// The full three-way join through the real handler: status (state/ready),
	// spec (ports), PodMetrics (CPU/Memory).
	if row.Find("td .cell-status.ok .ro-dot.ok").Length() != 1 {
		t.Fatalf("handler-rendered state cell missing the ok Running pair: %s", normSpace(row.Text()))
	}
	if got := normSpace(row.Find("span.ready.full").Text()); got != "ready" {
		t.Fatalf("handler-rendered ready cell = %q, want ready/full", got)
	}
	if got := normSpace(row.Find("td").Eq(4).Text()); got != "80/TCP" {
		t.Fatalf("handler-rendered ports = %q, want 80/TCP (spec join)", got)
	}
	if got := normSpace(row.Find("td").Eq(5).Text()); got != "250m" {
		t.Fatalf("handler-rendered CPU = %q, want 250m (live PodMetrics)", got)
	}
	if got := normSpace(row.Find("td").Eq(6).Text()); got != "128Mi" {
		t.Fatalf("handler-rendered Memory = %q, want 128Mi (live PodMetrics)", got)
	}
	// SPEC §7.15 section order through the handler: labels -> annotations ->
	// containers (the owner line, absent on this fixture, sits between
	// annotations and containers in the template), then the YAML cards with
	// the Spec card open and the Status card collapsed by default.
	flow := docTexts(p.doc, ".ro-rd .ro-section-label")
	if strings.Join(flow, "|") != "Labels|Annotations|Containers · 1" {
		t.Fatalf("default-tab section order = %v, want Labels|Annotations|Containers · 1", flow)
	}
	if seq := p.doc.Find(`.ro-containers, .ro-yaml-card[data-name="spec"]`); !seq.First().HasClass("ro-containers") {
		t.Fatalf("the containers section must precede the Spec YAML card")
	}
	if p.doc.Find(`.ro-yaml-card[data-name="status"].is-collapsed`).Length() != 1 {
		t.Fatalf("the Status YAML card must start is-collapsed (SPEC §7.15)")
	}
	if p.doc.Find(`.ro-yaml-card[data-name="spec"].is-collapsed`).Length() != 0 {
		t.Fatalf("the Spec YAML card must start OPEN")
	}
}

// TestAnnotationLongCollapsedToggle pins the >120-char annotation form (SPEC
// §7.15): a 2 KiB last-applied-configuration value renders NOT as a chip but
// as a collapsed `key · size` toggle (byte size on the face, aria-expanded
// false) plus a [hidden] scrollable <pre> carrying the FULL value — the value
// is reachable only after the readout.js toggle reveals the pre.
func TestAnnotationLongCollapsedToggle(t *testing.T) {
	longVal := strings.Repeat("x", 2048)
	obj := detailObject("pods", "Pod", true, nil, map[string]any{
		"kubectl.kubernetes.io/last-applied-configuration": longVal,
		"prometheus.io/scrape":                             "true",
	})
	doc := renderDetailView(t, buildDefaultDetailView(t, obj))

	// The short annotation stays a chip; the long one must NOT become one.
	if doc.Find(`span.ro-chip.anno[title="prometheus.io/scrape: true"]`).Length() != 1 {
		t.Fatalf("short annotation chip missing")
	}
	if got := doc.Find("span.ro-chip.anno").Length(); got != 1 {
		t.Fatalf("annotation chips = %d, want 1 (the long value must not render as a chip)", got)
	}

	long := doc.Find(".anno-long")
	if long.Length() != 1 {
		t.Fatalf("expected one .anno-long block, got %d", long.Length())
	}
	toggle := long.Find("button.ro-chip.anno-toggle[data-annolong]")
	if toggle.Length() != 1 {
		t.Fatalf("long annotation missing its [data-annolong] toggle button")
	}
	if expanded, _ := toggle.Attr("aria-expanded"); expanded != "false" {
		t.Fatalf("toggle aria-expanded = %q, want false (collapsed by default)", expanded)
	}
	if got := normSpace(toggle.Find(".ck").Text()); got != "kubectl.kubernetes.io/last-applied-configuration" {
		t.Fatalf("toggle key = %q", got)
	}
	if got := normSpace(toggle.Find(".cv").Text()); got != "2 KiB" {
		t.Fatalf("toggle size = %q, want the 2 KiB byte size", got)
	}

	// The payload: a [hidden] .anno-pre carrying the FULL value. hidden is the
	// reachable-only-after-expand contract (readout.js flips it).
	pre := long.Find("pre.anno-pre")
	if pre.Length() != 1 {
		t.Fatalf("long annotation missing its .anno-pre payload")
	}
	if _, ok := pre.Attr("hidden"); !ok {
		t.Fatalf("the .anno-pre payload must start [hidden] (collapsed)")
	}
	if got := pre.Text(); got != longVal {
		t.Fatalf("pre payload = %d chars, want the full %d-char value", len(got), len(longVal))
	}
	// The full value appears NOWHERE outside the hidden pre (no chip body, no
	// title= tooltip carries it).
	if doc.Find(`span.ro-chip.anno[title*="last-applied-configuration"]`).Length() != 0 {
		t.Fatalf("long annotation leaked into a chip tooltip")
	}
}

// TestAnnotationChipBlockBoundary pins the exact 120 split: a 120-char value
// stays a chip (40-char display cut, full value in the title tooltip); one
// char more becomes the collapsed block.
func TestAnnotationChipBlockBoundary(t *testing.T) {
	at := strings.Repeat("a", 120)
	over := strings.Repeat("b", 121)
	obj := detailObject("pods", "Pod", true, nil, map[string]any{
		"example.com/at-threshold":   at,
		"example.com/over-threshold": over,
	})
	doc := renderDetailView(t, buildDefaultDetailView(t, obj))

	chip := doc.Find(`span.ro-chip.anno[title="example.com/at-threshold: ` + at + `"]`)
	if chip.Length() != 1 {
		t.Fatalf("120-char annotation must stay a chip with the full value in title=")
	}
	body := normSpace(chip.Find(".cv").Text())
	if !strings.HasSuffix(body, "...") || len([]rune(body)) >= 120 {
		t.Fatalf("120-char chip body = %q, want the 40-char display cut", body)
	}
	if got := doc.Find(".anno-long").Length(); got != 1 {
		t.Fatalf("anno-long blocks = %d, want exactly 1 (only the 121-char value)", got)
	}
	if got := doc.Find(".anno-long .anno-pre").Text(); got != over {
		t.Fatalf("121-char value must render whole inside the block's pre")
	}
}

// TestDetailTitleHeadTailSplit pins the SPEC §6.6 title law through the D14
// name helper: a pod's H1 splits into the bright .pn-head workload prefix and
// the faint .pn-tail hash tail, glued (head+tail reconstructs the exact name);
// a kind without a hash grammar (Deployment) renders an un-split head.
func TestDetailTitleHeadTailSplit(t *testing.T) {
	doc := renderContainersDetail(t, nil)

	h1 := doc.Find(".ro-detail-title h1.ro-title")
	if got := normSpace(h1.Find(".pn-head").Text()); got != "parse-prod-server" {
		t.Fatalf("title head = %q, want parse-prod-server", got)
	}
	if got := normSpace(h1.Find(".pn-tail").Text()); got != "-9ff6cbdbb-vkrzf" {
		t.Fatalf("title tail = %q, want the intact -9ff6cbdbb-vkrzf hash (faint via .pn-tail)", got)
	}
	// Glued invariant: the H1 text reconstructs the full name with NO space
	// between head and tail.
	if got := normSpace(h1.Text()); got != "parse-prod-server-9ff6cbdbb-vkrzf" {
		t.Fatalf("title text = %q, want the exact full name", got)
	}

	// A non-hash kind never splits: whole name in .pn-head, no .pn-tail.
	dep := renderDetailView(t, buildDefaultDetailView(t, detailObject("deployments", "Deployment", true, nil, nil)))
	depH1 := dep.Find(".ro-detail-title h1.ro-title")
	if got := normSpace(depH1.Find(".pn-head").Text()); got != "deployment-0" {
		t.Fatalf("deployment title head = %q, want the whole name", got)
	}
	if depH1.Find(".pn-tail").Length() != 0 {
		t.Fatalf("deployment title must not grow a hash tail")
	}
}

// TestDetailTitleTruncatesLongHead pins SPEC §4.2 in the title: a head longer
// than 42 chars middle-truncates to 26…12 (the hash tail stays INTACT) and
// the H1 carries the FULL name in its title= tooltip.
func TestDetailTitleTruncatesLongHead(t *testing.T) {
	const fullName = "cron-data-warehouse-hourly-rollup-partition-compactor-29678881-x7k2v"
	rt := kube.ResourceType{APIVersion: "v1", Version: "v1", Plural: "pods", Kind: "Pod", Namespaced: true}
	obj := kube.NewObject(&rt, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]any{"name": fullName, "namespace": "default", "creationTimestamp": "2024-03-01T08:00:00Z", "resourceVersion": "1"},
		"spec":       map[string]any{"containers": []any{map[string]any{"name": "c", "image": "img:1"}}},
		"status":     map[string]any{"phase": "Running"},
	}})
	doc := renderDetailView(t, buildDefaultDetailView(t, &obj))

	h1 := doc.Find(".ro-detail-title h1.ro-title")
	if got := normSpace(h1.Find(".pn-head").Text()); got != "cron-data-warehouse-hourly…on-compactor" {
		t.Fatalf("truncated title head = %q, want the 26…12 form", got)
	}
	if got := normSpace(h1.Find(".pn-tail").Text()); got != "-29678881-x7k2v" {
		t.Fatalf("title tail = %q, want the INTACT hash tail", got)
	}
	if title, _ := h1.Attr("title"); title != fullName {
		t.Fatalf("truncated title tooltip = %q, want the full name", title)
	}
}
