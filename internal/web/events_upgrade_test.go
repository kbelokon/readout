package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/kbelokon/readout/internal/kube"
)

// events_upgrade_test.go pins the events upgrade (rich schemas for the
// remaining kinds + the events decode): the dual-API
// event decode with the PINNED precedence (count = series.count → count →
// deprecatedCount; first-seen = firstTimestamp → deprecatedFirstTimestamp →
// eventTime; last-seen = series.lastObservedTime → lastTimestamp →
// deprecatedLastTimestamp → eventTime), the ×N / two-layer-age / evobj / msg
// cells on the events list screen and the detail Events tab, and the
// cronjob/job/persistentvolume schema surfaces (Suspend status mapping,
// lastrun <never>, verbatim BackoffLimitExceeded, uuid PV names never
// split/truncated). Fixture values mirror the design handoff's corner-case
// dataset; both event API
// shapes and both sides of the 60s two-layer threshold are pinned because the
// decode precedence and threshold were review-flagged ambiguities.

// eventsClock is the fixed instant the events fixtures are built against
// (internal/fakekube/fixtures/data/events_table.json): the BackOff aggregate
// reads 3m/41h from here, the series row 24s/4m, the burst row 45s.
var eventsClock = time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)

// TestEventDecodePrecedence pins the events decode across the THREE wire
// spellings one struct must absorb: core/v1 (count/firstTimestamp/
// lastTimestamp/involvedObject/message/source.component), events.k8s.io/v1
// (deprecated*/regarding/note/reportingController), and the series aggregate
// that outranks both.
func TestEventDecodePrecedence(t *testing.T) {
	// Core shape: count + first/lastTimestamp (numbers arrive as JSON float64).
	core, ok := decodeEventItem(map[string]any{
		"type": "Warning", "reason": "BackOff",
		"count":          float64(141),
		"firstTimestamp": "2026-06-08T19:00:00Z",
		"lastTimestamp":  "2026-06-10T11:57:00Z",
		"message":        "Back-off restarting failed container",
		"source":         map[string]any{"component": "kubelet"},
		"involvedObject": map[string]any{"kind": "Pod", "name": "ugc-backend-8b9fc9d44-nxxz9"},
	})
	if !ok {
		t.Fatalf("core-shape event failed to decode")
	}
	if core.eventCount() != 141 || core.firstSeen() != "2026-06-08T19:00:00Z" || core.lastSeen() != "2026-06-10T11:57:00Z" {
		t.Fatalf("core-shape decode = count %d first %q last %q", core.eventCount(), core.firstSeen(), core.lastSeen())
	}
	if core.refKind() != "Pod" || core.refName() != "ugc-backend-8b9fc9d44-nxxz9" || core.from() != "kubelet" {
		t.Fatalf("core-shape ref/from = %q/%q/%q", core.refKind(), core.refName(), core.from())
	}

	// Series shape: series.count and series.lastObservedTime OUTRANK the
	// also-present count/lastTimestamp; first-seen falls through to eventTime
	// when no firstTimestamp exists.
	series, ok := decodeEventItem(map[string]any{
		"type": "Warning", "reason": "FailedScheduling",
		"count":         int64(3), // a stale legacy mirror the series must beat
		"lastTimestamp": "2026-06-10T11:00:00Z",
		"eventTime":     "2026-06-10T11:56:00.000000Z",
		"series":        map[string]any{"count": int64(12), "lastObservedTime": "2026-06-10T11:59:36.000000Z"},
	})
	if !ok {
		t.Fatalf("series-shape event failed to decode")
	}
	if series.eventCount() != 12 {
		t.Fatalf("series count = %d, want series.count 12 over count 3", series.eventCount())
	}
	if series.lastSeen() != "2026-06-10T11:59:36.000000Z" {
		t.Fatalf("series last-seen = %q, want series.lastObservedTime over lastTimestamp", series.lastSeen())
	}
	if series.firstSeen() != "2026-06-10T11:56:00.000000Z" {
		t.Fatalf("series first-seen = %q, want the eventTime fallback", series.firstSeen())
	}

	// events.k8s.io/v1 spelling: deprecated* fields + note/regarding/
	// reportingController normalize through the same accessors.
	deprecated, ok := decodeEventItem(map[string]any{
		"type": "Normal", "reason": "Pulled",
		"deprecatedCount":          float64(7),
		"deprecatedFirstTimestamp": "2026-06-10T10:00:00Z",
		"deprecatedLastTimestamp":  "2026-06-10T11:30:00Z",
		"note":                     "image already present",
		"reportingController":      "kubelet",
		"regarding":                map[string]any{"kind": "Pod", "name": "nginx"},
	})
	if !ok {
		t.Fatalf("deprecated-shape event failed to decode")
	}
	if deprecated.eventCount() != 7 || deprecated.firstSeen() != "2026-06-10T10:00:00Z" || deprecated.lastSeen() != "2026-06-10T11:30:00Z" {
		t.Fatalf("deprecated-shape decode = count %d first %q last %q", deprecated.eventCount(), deprecated.firstSeen(), deprecated.lastSeen())
	}
	if deprecated.message() != "image already present" || deprecated.from() != "kubelet" || deprecated.refName() != "nginx" {
		t.Fatalf("deprecated-shape note/from/ref = %q/%q/%q", deprecated.message(), deprecated.from(), deprecated.refName())
	}

	// No count anywhere: the event occurred once — ×1, never ×0.
	bare, ok := decodeEventItem(map[string]any{
		"type": "Normal", "reason": "Scheduled", "eventTime": "2026-06-10T11:59:00Z",
	})
	if !ok || bare.eventCount() != 1 {
		t.Fatalf("countless event count = %d, want the implicit 1", bare.eventCount())
	}
	if bare.lastSeen() != "2026-06-10T11:59:00Z" || bare.firstSeen() != "2026-06-10T11:59:00Z" {
		t.Fatalf("eventTime-only event first/last = %q/%q, want the eventTime tail of both chains", bare.firstSeen(), bare.lastSeen())
	}
}

// TestEventTwoLayerAgeThreshold pins the two-layer-age second-layer gate: it renders
// when count > 1 AND last − first > 60s — both sides of the threshold and the
// count gate are independent facts.
func TestEventTwoLayerAgeThreshold(t *testing.T) {
	now := eventsClock
	item := func(count int64, firstT, lastT string) *eventItem {
		e := &eventItem{Count: count, FirstTimestamp: firstT, LastTimestamp: lastT}
		return e
	}

	// count > 1, spread 41h: two layers.
	if got := eventAgeText(item(141, "2026-06-08T19:00:00Z", "2026-06-10T11:57:00Z"), now); got != "3m (first 41h ago)" {
		t.Fatalf("aggregate age = %q, want the two-layer 3m (first 41h ago)", got)
	}
	// Spread exactly 60s: NOT > 60s — single layer.
	if got := eventAgeText(item(5, "2026-06-10T11:58:15Z", "2026-06-10T11:59:15Z"), now); got != "45s" {
		t.Fatalf("60s-spread age = %q, want the single-layer 45s (threshold is strictly >60s)", got)
	}
	// Spread 61s: just over — two layers (106s since first: HumanDuration
	// keeps sub-2m values in seconds).
	if got := eventAgeText(item(5, "2026-06-10T11:58:14Z", "2026-06-10T11:59:15Z"), now); got != "45s (first 106s ago)" {
		t.Fatalf("61s-spread age = %q, want the two-layer form", got)
	}
	// count == 1: no second layer no matter the spread.
	if got := eventAgeText(item(1, "2026-06-08T19:00:00Z", "2026-06-10T11:57:00Z"), now); got != "3m" {
		t.Fatalf("single-occurrence age = %q, want the bare 3m", got)
	}
	// No last-seen at all: empty, the caller keeps its fallback.
	if got := eventAgeText(&eventItem{Count: 3}, now); got != "" {
		t.Fatalf("timestampless age = %q, want empty", got)
	}
}

// TestEventColumnsDecoration pins decorateEventColumns: the printer-shaped
// 5-column events Table gains a Count column BEFORE Message (the wrapping msg
// column stays last) whose cells carry the decoded int64 (sort/TSV truth);
// rows stay in lockstep; the pass is idempotent and never duplicates a
// server-provided Count column.
func TestEventColumnsDecoration(t *testing.T) {
	table := &kube.Table{
		Resource: kube.ResourceType{Plural: "events", Kind: "Event", Namespaced: true, Version: "v1", APIVersion: "v1"},
		Clusters: []string{"test"},
		Columns: []kube.Column{
			{Name: "Last Seen"}, {Name: "Type"}, {Name: "Reason"}, {Name: "Object"}, {Name: "Message"},
		},
		Rows: []kube.Row{
			{Cluster: "test", Object: map[string]any{
				"kind": "Event", "apiVersion": "v1",
				"metadata":      map[string]any{"name": "e1", "namespace": "default", "creationTimestamp": "2026-06-08T19:00:00Z"},
				"count":         float64(141),
				"lastTimestamp": "2026-06-10T11:57:00Z",
			}, Cells: []any{"3m", "Warning", "BackOff", "pod/x", "back-off"}},
			{Cluster: "test", Object: map[string]any{
				"kind": "Event", "apiVersion": "v1",
				"metadata": map[string]any{"name": "e2", "namespace": "default", "creationTimestamp": "2026-06-10T11:56:00Z"},
				"series":   map[string]any{"count": float64(12), "lastObservedTime": "2026-06-10T11:59:36Z"},
			}, Cells: []any{"24s", "Warning", "FailedScheduling", "pod/y", "0/20 nodes"}},
		},
	}
	decorateEventColumns(table)

	countIdx := columnIndex(table.Columns, "Count")
	msgIdx := columnIndex(table.Columns, "Message")
	if countIdx < 0 || msgIdx != countIdx+1 {
		t.Fatalf("Count column at %d, Message at %d — Count must sit right before the wrapping Message", countIdx, msgIdx)
	}
	if got := table.Rows[0].Cells[countIdx]; got != int64(141) {
		t.Fatalf("core-shape Count cell = %#v, want int64 141", got)
	}
	if got := table.Rows[1].Cells[countIdx]; got != int64(12) {
		t.Fatalf("series-shape Count cell = %#v, want the series.count 12", got)
	}
	for i, row := range table.Rows {
		if len(row.Cells) != len(table.Columns) {
			t.Fatalf("row %d has %d cells for %d columns (table went ragged)", i, len(row.Cells), len(table.Columns))
		}
	}

	// Idempotence: a second pass (or a server that already provides Count)
	// adds nothing.
	decorateEventColumns(table)
	names := map[string]int{}
	for _, col := range table.Columns {
		names[col.Name]++
	}
	if names["Count"] != 1 {
		t.Fatalf("decoration duplicated the Count column: %v", names)
	}
}

// TestEventListThroughHandler drives the fakeapi events Table fixture through
// the REAL handler chain (the events list page negotiates as=Table): the
// decorated Count header lands before Message; the count=141 BackOff
// aggregate renders the amber ×141 + the two-layer "3m (first 41h ago)" age;
// the series-shape row decodes via series.count/lastObservedTime (×12 +
// "24s (first 4m ago)"); the single event reads the faint ×1 with NO second
// layer; the ≤60s burst stays single-layer despite count>1; the Object cell
// is the evobj kind-icon + faint "Pod/" prefix; Message is the wrapping
// td.ro-event-msg; and the Type dots are ONLY warn/mute — events never earn
// an invented stronger severity.
func TestEventListThroughHandler(t *testing.T) {
	app := newServer(t, baseConfig(t), eventsClock)
	p := get(t, app, "/clusters/test/namespaces/default/events", http.StatusOK)

	headers := p.texts("table.ro-table thead th")
	joined := strings.Join(headers, "|")
	if !strings.Contains(joined, "Count|Message") {
		t.Fatalf("Count header must sit right before Message, got %v", headers)
	}

	// The chronic aggregate: ×141 amber, two-layer age.
	backoff := p.doc.Find(`table.ro-table tbody tr:has(td:contains("BackOff"))`)
	if got := normSpace(backoff.Find(".restarts.some").Text()); got != "×141" {
		t.Fatalf("BackOff count cell = %q, want the amber ×141 (≥20 is chronic)", got)
	}
	if got := normSpace(backoff.Find("span.age-fresh").Text()); got != "3m" {
		t.Fatalf("BackOff age lead token = %q, want the bucket-coloured 3m", got)
	}
	if got := normSpace(backoff.Find(".faint.evage-rest").Text()); got != "(first 41h ago)" {
		t.Fatalf("BackOff age second layer = %q, want (first 41h ago)", got)
	}

	// The series-shape row decodes via series.count / series.lastObservedTime.
	series := p.doc.Find(`table.ro-table tbody tr:has(td:contains("FailedScheduling"))`)
	if !strings.Contains(normSpace(series.Text()), "×12") {
		t.Fatalf("series row missing the ×12 series.count: %q", normSpace(series.Text()))
	}
	if got := normSpace(series.Find(".faint.evage-rest").Text()); got != "(first 4m ago)" {
		t.Fatalf("series age second layer = %q, want (first 4m ago) from eventTime", got)
	}

	// The single event: faint ×1 (in the right-aligned count cell),
	// single-layer age.
	single := p.doc.Find(`table.ro-table tbody tr:has(td:contains("Scheduled"))`)
	if got := normSpace(single.Find("td.num span.faint").Text()); got != "×1" {
		t.Fatalf("single event count = %q, want the faint ×1", got)
	}
	if single.Find(".evage-rest").Length() != 0 {
		t.Fatalf("single-occurrence event must not render the second age layer")
	}

	// The tight burst (count=5, spread 45s ≤ 60s): single-layer despite count>1.
	burst := p.doc.Find(`table.ro-table tbody tr:has(td:contains("Unhealthy"))`)
	if !strings.Contains(normSpace(burst.Text()), "×5") {
		t.Fatalf("burst row missing its ×5 count: %q", normSpace(burst.Text()))
	}
	if burst.Find(".evage-rest").Length() != 0 {
		t.Fatalf("a ≤60s spread must stay single-layer (threshold is >60s)")
	}

	// evobj: kind icon + faint "Pod/" prefix + the object name.
	evobj := backoff.Find(".res-kind")
	if evobj.Length() != 1 {
		t.Fatalf("BackOff row missing its evobj cell")
	}
	if got := normSpace(evobj.Find(".faint").First().Text()); got != "Pod/" {
		t.Fatalf("evobj kind prefix = %q, want the faint Pod/", got)
	}
	if !strings.Contains(normSpace(evobj.Text()), "ugc-backend-8b9fc9d44-nxxz9") {
		t.Fatalf("evobj cell missing the object name: %q", normSpace(evobj.Text()))
	}
	if evobj.Find("svg, .kind-tile").Length() == 0 {
		t.Fatalf("evobj cell must carry a kind icon")
	}

	// msg: the wrapping cell, one per row.
	if got := p.count("table.ro-table tbody td.ro-event-msg"); got != 4 {
		t.Fatalf("wrapping message cells = %d, want one per event row", got)
	}

	// Severity law: Type dots are warn (Warning) and mute (Normal) ONLY —
	// no ok/err dot is ever invented for an event type.
	if got := p.count("table.ro-table tbody .cell-status.warn"); got != 3 {
		t.Fatalf("warn Type cells = %d, want the 3 Warning events", got)
	}
	if got := p.count("table.ro-table tbody .cell-status.mute"); got != 1 {
		t.Fatalf("mute Type cells = %d, want the 1 Normal event", got)
	}
	if got := p.count("table.ro-table tbody .cell-status.err, table.ro-table tbody .cell-status.ok"); got != 0 {
		t.Fatalf("events table rendered %d ok/err status cells — severities beyond Normal/Warning are invented", got)
	}
}

// TestEventDetailTabInheritsCells pins the events decode on the detail Events tab: the same
// ×N + two-layer-age cells the list renders, driven through buildEventViews +
// the ResourceView templ over crafted dual-shape events.
func TestEventDetailTabInheritsCells(t *testing.T) {
	app := newServer(t, baseConfig(t), eventsClock)
	events := app.buildEventViews([]map[string]any{
		// Core-shape chronic aggregate.
		{
			"type": "Warning", "reason": "BackOff",
			"message":        "Back-off restarting failed container",
			"count":          float64(141),
			"firstTimestamp": "2026-06-08T19:00:00Z",
			"lastTimestamp":  "2026-06-10T11:57:00Z",
			"source":         map[string]any{"component": "kubelet"},
		},
		// events.k8s.io/v1 spelling: the deprecated* fields drive the same cells.
		{
			"type": "Normal", "reason": "Pulled",
			"note":                     "image already present",
			"deprecatedCount":          float64(25),
			"deprecatedFirstTimestamp": "2026-06-10T08:00:00Z",
			"deprecatedLastTimestamp":  "2026-06-10T11:58:00Z",
			"reportingController":      "kubelet",
		},
	})
	v := &detailView{Cluster: "test", Namespace: "default", Object: *detailObject("pods", "Pod", true, nil, nil), EventsTab: true, IsEventsView: true, Events: events}
	doc := renderDetailView(t, v)

	rows := doc.Find("table.ro-table tbody tr")
	if rows.Length() != 2 {
		t.Fatalf("event rows = %d, want 2", rows.Length())
	}
	core := rows.Eq(0)
	if got := normSpace(core.Find(".restarts.some").Text()); got != "×141" {
		t.Fatalf("detail count cell = %q, want the amber ×141", got)
	}
	if got := normSpace(core.Find("span.age-fresh").Text()); got != "3m" {
		t.Fatalf("detail age lead token = %q, want 3m", got)
	}
	if got := normSpace(core.Find(".faint.evage-rest").Text()); got != "(first 41h ago)" {
		t.Fatalf("detail age second layer = %q, want (first 41h ago)", got)
	}
	if title, _ := core.Find(`td[title^="last seen"]`).Attr("title"); title != "last seen 2026-06-10 11:57:00" {
		t.Fatalf("detail age tooltip = %q, want the full last-seen timestamp", title)
	}
	// The deprecated-* spelling feeds the SAME cells: ×25 amber (≥20),
	// two-layer (spread 3h58m > 60s) from the deprecated timestamps.
	dep := rows.Eq(1)
	if got := normSpace(dep.Find(".restarts.some").Text()); got != "×25" {
		t.Fatalf("deprecated-shape count cell = %q, want ×25", got)
	}
	if got := normSpace(dep.Find(".faint.evage-rest").Text()); got != "(first 4h ago)" {
		t.Fatalf("deprecated-shape second layer = %q, want (first 4h ago)", got)
	}
	if got := normSpace(dep.Find("td.ro-event-msg").Text()); got != "image already present" {
		t.Fatalf("deprecated-shape message = %q, want the note spelling", got)
	}
}

// TestCronJobCells pins the cronjob cell mapping through the real
// buildCellView: the printer's Suspend boolean maps false→Active (ok dot,
// live health) / true→Suspended (mute per the status-tone table) with no pulse and the
// kube.Table cell untouched; Last Schedule is the lastrun cell
// (duration + " ago" + age bucket; the printer's literal <none> → the faint
// <never>); the Schedule column stays the verbatim plain cell.
func TestCronJobCells(t *testing.T) {
	table := &kube.Table{
		Resource: kube.ResourceType{Group: "batch", Plural: "cronjobs", Kind: "CronJob", Namespaced: true, Version: "v1", APIVersion: "batch/v1"},
		Clusters: []string{"test"},
		Columns: []kube.Column{
			{Name: "Name"}, {Name: "Schedule"}, {Name: "Suspend"}, {Name: "Active"}, {Name: "Last Schedule"}, {Name: "Age"},
		},
	}
	obj := func(name string, suspend bool) map[string]any {
		return map[string]any{
			"kind": "CronJob", "apiVersion": "batch/v1",
			"metadata": map[string]any{"name": name, "namespace": "default", "creationTimestamp": "2026-05-24T12:00:00Z"},
			"spec":     map[string]any{"suspend": suspend},
		}
	}
	active := kube.Row{Cluster: "test", Object: obj("cron-billing-queues", false), Cells: []any{"cron-billing-queues", "*/1 * * * *", false, int64(1), "59s", "17d"}}
	suspended := kube.Row{Cluster: "test", Object: obj("cron-legacy-cleanup", true), Cells: []any{"cron-legacy-cleanup", "0 4 * * 0", true, int64(0), "41d", "1y127d"}}
	never := kube.Row{Cluster: "test", Object: obj("cron-new-feature-warmup", false), Cells: []any{"cron-new-feature-warmup", "30 6 * * *", false, int64(0), "<none>", "3h"}}
	table.Rows = []kube.Row{active, suspended, never}

	// Suspend false → the ok Active status (a live, scheduled cron).
	cv := schemaCellView(t, table, active, 2)
	if cv.Kind != cellStatus || cv.Value != "Active" || cv.Tone != "ok" || cv.Pulse {
		t.Fatalf("unsuspended cell = %#v, want the steady ok Active status", cv)
	}
	// Suspend true → Suspended, mute (per the status-tone table), never a pulse.
	cv = schemaCellView(t, table, suspended, 2)
	if cv.Kind != cellStatus || cv.Value != "Suspended" || cv.Tone != "mute" || cv.Pulse {
		t.Fatalf("suspended cell = %#v, want the steady mute Suspended status", cv)
	}
	// Schedule: verbatim plain (mono comes from the table type law, not a class).
	cv = schemaCellView(t, table, active, 1)
	if cv.Kind != cellPlain || cv.Value != "*/1 * * * *" || cv.Trunc {
		t.Fatalf("schedule cell = %#v, want the verbatim untruncated plain cell", cv)
	}
	// Last Schedule: the lastrun cell — duration + " ago" + its age bucket.
	cv = schemaCellView(t, table, active, 4)
	if cv.Kind != cellLastRun || cv.Value != "59s ago" || cv.Class != "age-fresh" {
		t.Fatalf("last-schedule cell = %#v, want 59s ago / age-fresh", cv)
	}
	cv = schemaCellView(t, table, suspended, 4)
	if cv.Value != "41d ago" || cv.Class != "age-old" {
		t.Fatalf("41d last-schedule cell = %#v, want 41d ago / age-old", cv)
	}
	// Never ran: the printer's literal <none> IS the empty case → faint <never>.
	cv = schemaCellView(t, table, never, 4)
	if cv.Kind != cellLastRun || cv.Value != "" {
		t.Fatalf("never-ran cell = %#v, want the empty lastrun (renders <never>)", cv)
	}
}

// TestCronJobListThroughHandler drives the fakeapi cronjobs fixture through
// the real handler: the suspended row renders the mute Suspended dot, the
// live rows the ok Active dot, the schedule strings land verbatim, the
// never-ran row shows the faint <never>, and the 59s last-run reads
// "59s ago" in the fresh bucket.
func TestCronJobListThroughHandler(t *testing.T) {
	app := newServer(t, baseConfig(t), eventsClock)
	p := get(t, app, "/clusters/test/namespaces/default/cronjobs", http.StatusOK)

	billing := p.doc.Find(`table.ro-table tr:has(td.cell-name a[href$="/cronjobs/cron-billing-queues"])`)
	if billing.Find(".cell-status.ok .ro-dot.ok").Length() != 1 || !strings.Contains(billing.Text(), "Active") {
		t.Fatalf("unsuspended cron must render the ok Active dot: %q", normSpace(billing.Text()))
	}
	if !strings.Contains(billing.Text(), "*/1 * * * *") {
		t.Fatalf("schedule must render verbatim: %q", normSpace(billing.Text()))
	}
	if got := normSpace(billing.Find("span.age-fresh").First().Text()); got != "59s ago" {
		t.Fatalf("billing last-run = %q, want the fresh 59s ago", got)
	}

	cleanup := p.doc.Find(`table.ro-table tr:has(td.cell-name a[href$="/cronjobs/cron-legacy-cleanup"])`)
	if cleanup.Find(".cell-status.mute .ro-dot.mute").Length() != 1 || !strings.Contains(cleanup.Text(), "Suspended") {
		t.Fatalf("suspended cron must render the mute Suspended dot: %q", normSpace(cleanup.Text()))
	}
	if cleanup.Find(".ro-dot.pulse").Length() != 0 {
		t.Fatalf("Suspended is a steady state — it must never pulse")
	}

	warmup := p.doc.Find(`table.ro-table tr:has(td.cell-name a[href$="/cronjobs/cron-new-feature-warmup"])`)
	if warmup.Find(`td span.faint:contains("<never>")`).Length() == 0 {
		t.Fatalf("never-ran cron must render the faint <never>: %q", normSpace(warmup.Text()))
	}
}

// TestJobColumnsDecoration pins decorateJobColumns on both printer shapes: a
// Table WITHOUT the Status column (pre-1.30 apiservers) gains a synthetic one
// right after Name derived from status.conditions (Failed surfaces its
// verbatim reason); a Table WITH it keeps every printer word except the bare
// "Failed", which refines to the condition's reason. Idempotent, lockstep.
func TestJobColumnsDecoration(t *testing.T) {
	jobObject := func(name string, conditions []any) map[string]any {
		status := map[string]any{}
		if conditions != nil {
			status["conditions"] = conditions
		}
		return map[string]any{
			"kind": "Job", "apiVersion": "batch/v1",
			"metadata": map[string]any{"name": name, "namespace": "default", "creationTimestamp": "2026-06-05T12:00:00Z"},
			"status":   status,
		}
	}
	failedCond := []any{map[string]any{"type": "Failed", "status": "True", "reason": "BackoffLimitExceeded"}}
	completeCond := []any{map[string]any{"type": "Complete", "status": "True", "reason": "CompletionsReached"}}
	suspendedCond := []any{map[string]any{"type": "Suspended", "status": "True", "reason": "JobSuspended"}}

	// Old printer: no Status column — the synthetic one derives from conditions.
	old := &kube.Table{
		Resource: kube.ResourceType{Group: "batch", Plural: "jobs", Kind: "Job", Namespaced: true, Version: "v1", APIVersion: "batch/v1"},
		Clusters: []string{"test"},
		Columns:  []kube.Column{{Name: "Name"}, {Name: "Completions"}, {Name: "Duration"}, {Name: "Age"}},
		Rows: []kube.Row{
			{Cluster: "test", Object: jobObject("done", completeCond), Cells: []any{"done", "1/1", "42s", "2m"}},
			{Cluster: "test", Object: jobObject("dead", failedCond), Cells: []any{"dead", "0/1", "46m", "5d"}},
			{Cluster: "test", Object: jobObject("busy", nil), Cells: []any{"busy", "8/10", "38m", "38m"}},
			{Cluster: "test", Object: jobObject("paused", suspendedCond), Cells: []any{"paused", "0/1", "", "1h"}},
		},
	}
	decorateJobColumns(old)
	statusIdx := columnIndex(old.Columns, "Status")
	if statusIdx != 1 {
		t.Fatalf("synthetic Status column at %d, want 1 (right after Name)", statusIdx)
	}
	want := []string{"Complete", "BackoffLimitExceeded", "Running", "Suspended"}
	for i, row := range old.Rows {
		if got := cellString(row, statusIdx); got != want[i] {
			t.Fatalf("row %d derived status = %q, want the verbatim %q", i, got, want[i])
		}
		if len(row.Cells) != len(old.Columns) {
			t.Fatalf("row %d has %d cells for %d columns (table went ragged)", i, len(row.Cells), len(old.Columns))
		}
	}

	// Modern printer: the Status column exists — only the bare "Failed"
	// refines (to the condition's verbatim reason); other words stay.
	modern := &kube.Table{
		Resource: kube.ResourceType{Group: "batch", Plural: "jobs", Kind: "Job", Namespaced: true, Version: "v1", APIVersion: "batch/v1"},
		Clusters: []string{"test"},
		Columns:  []kube.Column{{Name: "Name"}, {Name: "Status"}, {Name: "Completions"}, {Name: "Duration"}, {Name: "Age"}},
		Rows: []kube.Row{
			{Cluster: "test", Object: jobObject("dead", failedCond), Cells: []any{"dead", "Failed", "0/1", "46m", "5d"}},
			{Cluster: "test", Object: jobObject("done", completeCond), Cells: []any{"done", "Complete", "1/1", "42s", "2m"}},
		},
	}
	decorateJobColumns(modern)
	if got := cellString(modern.Rows[0], 1); got != "BackoffLimitExceeded" {
		t.Fatalf("refined status = %q, want the verbatim BackoffLimitExceeded", got)
	}
	if got := cellString(modern.Rows[1], 1); got != "Complete" {
		t.Fatalf("non-Failed status = %q, must stay the printer's Complete", got)
	}
	// Idempotence: re-running adds no second Status column.
	decorateJobColumns(modern)
	names := map[string]int{}
	for _, col := range modern.Columns {
		names[col.Name]++
	}
	if names["Status"] != 1 {
		t.Fatalf("decoration duplicated the Status column: %v", names)
	}
}

// TestJobListThroughHandler drives the fakeapi jobs fixture through the real
// handler: the failed job renders its FULL verbatim status name
// (BackoffLimitExceeded) with the err dot and the err row
// stripe; completions ride the ready grammar (1/1 full green, 8/10 partial
// amber).
func TestJobListThroughHandler(t *testing.T) {
	app := newServer(t, baseConfig(t), eventsClock)
	p := get(t, app, "/clusters/test/namespaces/default/jobs", http.StatusOK)

	dead := p.doc.Find(`table.ro-table tr:has(td.cell-name a[href$="/jobs/legacy-export-once-20260605"])`)
	status := dead.Find(".cell-status.err")
	if status.Length() != 1 || normSpace(status.Text()) != "BackoffLimitExceeded" {
		t.Fatalf("failed job status = %q, want the verbatim err BackoffLimitExceeded", normSpace(status.Text()))
	}
	if status.Find(".ro-dot.err").Length() != 1 {
		t.Fatalf("failed job status missing its err dot")
	}
	if !dead.HasClass("row-status-err") {
		t.Fatalf("failed job row must carry the err stripe")
	}
	if got := normSpace(dead.Find(".ready.zero").Text()); got != "0/1" {
		t.Fatalf("failed completions = %q, want the faint 0/1", got)
	}

	done := p.doc.Find(`table.ro-table tr:has(td.cell-name a[href$="/jobs/cron-billing-queues-29678683"])`)
	if got := normSpace(done.Find(".ready.full").Text()); got != "1/1" {
		t.Fatalf("complete completions = %q, want the full 1/1", got)
	}
	busy := p.doc.Find(`table.ro-table tr:has(td.cell-name a[href$="/jobs/ml-embeddings-reindex-29677500"])`)
	if got := normSpace(busy.Find(".ready.partial").Text()); got != "8/10" {
		t.Fatalf("running completions = %q, want the partial 8/10", got)
	}
}

// TestPersistentVolumeListThroughHandler drives the fakeapi PV fixture
// through the real handler (a CLUSTER-scoped list): the uuid-shaped names
// render whole — never split into a hash tail, never middle-truncated (no
// tooltip needed) — and the phase words carry their status-tone-table tones: Bound ok,
// Released warn (+ warn stripe), Failed err (+ err stripe).
func TestPersistentVolumeListThroughHandler(t *testing.T) {
	app := newServer(t, baseConfig(t), eventsClock)
	p := get(t, app, "/clusters/test/persistentvolumes", http.StatusOK)

	const bound = "pvc-9f2c4e7a-1b3d-4f6a-8c0e-2d5b7a3f9e2c"
	row := p.doc.Find(`table.ro-table tr:has(td.cell-name a[href$="/persistentvolumes/` + bound + `"])`)
	link := row.Find("td.cell-name a")
	if got := normSpace(link.Text()); got != bound {
		t.Fatalf("PV name = %q, want the FULL untruncated uuid name", got)
	}
	if link.Find(".pn-tail").Length() != 0 {
		t.Fatalf("uuid PV name must never split into a hash tail")
	}
	if _, ok := link.Attr("title"); ok {
		t.Fatalf("untruncated PV name must carry no tooltip")
	}
	if row.Find(".cell-status.ok .ro-dot.ok").Length() != 1 {
		t.Fatalf("Bound PV must render the ok dot: %q", normSpace(row.Text()))
	}

	released := p.doc.Find(`table.ro-table tr:has(td.cell-name a[href$="/persistentvolumes/pvc-7e1d3f9b-4c6a-4d8e-9f2b-0a3c5e7d9b1f"])`)
	if released.Find(".cell-status.warn .ro-dot.warn").Length() != 1 {
		t.Fatalf("Released PV must render the warn dot: %q", normSpace(released.Text()))
	}
	if !released.HasClass("row-status-warn") {
		t.Fatalf("Released PV row must carry the warn stripe")
	}

	failed := p.doc.Find(`table.ro-table tr:has(td.cell-name a[href$="/persistentvolumes/pvc-1c4f7a9d-2e5b-4c8a-b6d9-3f0e2a4c6b8e"])`)
	if failed.Find(".cell-status.err .ro-dot.err").Length() != 1 {
		t.Fatalf("Failed PV must render the err dot: %q", normSpace(failed.Text()))
	}
	if !failed.HasClass("row-status-err") {
		t.Fatalf("Failed PV row must carry the err stripe")
	}
}

// TestPersistentVolumeNameNeverSplits pins the splitObjectName law for PVs at
// the unit level: the uuid-shaped name stays one head for the
// persistentvolumes plural (only pods/replicasets carry template-hash tails),
// and at 40 runes it sits under the 42-rune middle-truncation threshold.
func TestPersistentVolumeNameNeverSplits(t *testing.T) {
	const name = "pvc-9f2c4e7a-1b3d-4f6a-8c0e-2d5b7a3f9e2c"
	head, tail := splitObjectName("persistentvolumes", name)
	if head != name || tail != "" {
		t.Fatalf("splitObjectName(persistentvolumes) = %q + %q, want the whole name as head", head, tail)
	}
	if display, truncated := MiddleTruncate(name, nameHeadMax, nameHeadLead, nameHeadTrail); truncated || display != name {
		t.Fatalf("40-rune PV name must not middle-truncate, got %q (truncated=%v)", display, truncated)
	}
}

// TestEventListSortsByCount pins the decorated Count column's numeric truth:
// sorting the events list by Count orders rows by the decoded int64 (141 >
// 12 > 5 > 1), not lexicographically — the cell the decorator wrote is the
// sort key.
func TestEventListSortsByCount(t *testing.T) {
	app := newServer(t, baseConfig(t), eventsClock)
	p := get(t, app, "/clusters/test/namespaces/default/events?sort=Count:desc", http.StatusOK)

	var counts []string
	p.doc.Find("table.ro-table tbody tr").Each(func(_ int, s *goquery.Selection) {
		counts = append(counts, normSpace(s.Find("td.num span").First().Text()))
	})
	want := []string{"×141", "×12", "×5", "×1"}
	if strings.Join(counts, "|") != strings.Join(want, "|") {
		t.Fatalf("Count:desc order = %v, want %v (numeric, not lexicographic)", counts, want)
	}
}
