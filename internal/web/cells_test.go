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

// cells_test.go pins the SPEC §4 cell-type cookbook (Unit 10): the corner-case
// cell constructors (pending/ports/hosts/tls/lastrun/keys/count/evobj/evage/
// msg), the name middle-truncation + restarts thousands-separator wiring in
// buildCellView, and the label/data-chip in-cell overflow machinery
// (.xtra/.expanded + the +N button). Fixtures mirror the design handoff's
// corner-case dataset (docs/design_handoff_readout_v2/js/data-extra.js) so the
// designed-for inputs are the tested inputs. Every expectation is an
// INDEPENDENT fact from SPEC §4 (thresholds: head>42 -> 26…12, evobj 34 ->
// 20…8, 2 ports / 1 host / 3 keys / 2 chips before overflow, count ≥20 amber,
// capacity >55/>80, replica cap 12, age fractions of a 24h window), never an
// echo of the emitted markup.

// renderCellViews renders crafted cellViews through the REAL bridge + templ
// pipeline (toListData -> toTableData -> ResourceTable) inside a minimal
// single-table list, so the new renderers are asserted on the production
// render path rather than a hand-built templates.ListData. Cells align 1:1
// with colNames; the row carries no object (the constructors already resolved
// the display).
func renderCellViews(t *testing.T, plural, kind string, colNames []string, cells []cellView) *goquery.Document {
	t.Helper()
	table := kube.Table{
		Resource: kube.ResourceType{Plural: plural, Kind: kind, Namespaced: true, Version: "v1", APIVersion: "v1"},
		Clusters: []string{"test"},
	}
	for _, name := range colNames {
		table.Columns = append(table.Columns, kube.Column{Name: name})
	}
	tv := tableView{Table: table, Kind: kind}
	tv.Columns = make([]columnView, len(table.Columns))
	tv.Rows = []rowView{{Cells: cells}}
	v := listView{Cluster: "test", Plural: plural, ClusterCount: 1, Tables: []tableView{tv}}
	return renderListView(t, &v)
}

// firstRowTD selects the idx-th body cell of the first rendered table row.
func firstRowTD(t *testing.T, doc *goquery.Document, idx int) *goquery.Selection {
	t.Helper()
	tds := doc.Find("table.ro-table tbody tr").First().Find("td")
	if tds.Length() <= idx {
		t.Fatalf("row has %d cells, wanted index %d", tds.Length(), idx)
	}
	return tds.Eq(idx)
}

// TestNameCellMiddleTruncation pins SPEC §4.2 on the REAL pods cell assembly:
// a head longer than 42 chars displays as first-26 + "…" + last-12 with the
// FULL name in Title; the hash tail is NEVER truncated; a 42-char head stays
// whole. The 64+-char cron pod name is the data-extra.js corner-case row.
func TestNameCellMiddleTruncation(t *testing.T) {
	cols := []string{"Name", "Ready", "Status", "Restarts", "Age"}

	// data-extra.js corner case: the hourly-rollup cron pod. The pod-name split
	// peels the job/pod hash tail first; only the 53-char HEAD truncates.
	const cronPod = "cron-data-warehouse-hourly-rollup-partition-compactor-29678881-x7k2v"
	cv := podsCellView(t, cols, []any{cronPod, "0/1", "Completed", "0", "3h2m"}, 0)
	if cv.Kind != cellName {
		t.Fatalf("kind = %v, want cellName", cv.Kind)
	}
	if cv.NameHead != "cron-data-warehouse-hourly…on-compactor" {
		t.Fatalf("truncated head = %q, want first26+…+last12 of the 53-char head", cv.NameHead)
	}
	if cv.NameTail != "-29678881-x7k2v" {
		t.Fatalf("hash tail = %q, want -29678881-x7k2v INTACT (the tail is never truncated)", cv.NameTail)
	}
	if cv.Title != cronPod {
		t.Fatalf("Title = %q, want the FULL name (the tooltip escape hatch)", cv.Title)
	}

	// data-extra.js corner case: a 45-char manual-run name with NO recognisable
	// hash tail -- the whole name is the head and still truncates.
	const manualRun = "db-schema-migrations-runner-manual-2026-06-10"
	cv = podsCellView(t, cols, []any{manualRun, "0/1", "Completed", "0", "7h"}, 0)
	if cv.NameHead != "db-schema-migrations-runne…l-2026-06-10" || cv.NameTail != "" {
		t.Fatalf("no-tail name = head %q tail %q, want truncated head + empty tail", cv.NameHead, cv.NameTail)
	}
	if cv.Title != manualRun {
		t.Fatalf("no-tail Title = %q, want full name", cv.Title)
	}

	// Boundary: exactly 42 chars stays whole (no tooltip); 43 truncates.
	const head42 = "exactly-forty-two-characters-name-aaaaaaaa"
	cv = podsCellView(t, cols, []any{head42, "1/1", "Running", "0", "1h"}, 0)
	if cv.NameHead != head42 || cv.Title != "" {
		t.Fatalf("42-char head must stay whole with no Title, got head %q title %q", cv.NameHead, cv.Title)
	}
	cv = podsCellView(t, cols, []any{head42 + "b", "1/1", "Running", "0", "1h"}, 0)
	if cv.NameHead != "exactly-forty-two-characte…me-aaaaaaaab" || cv.Title != head42+"b" {
		t.Fatalf("43-char head must truncate, got head %q title %q", cv.NameHead, cv.Title)
	}
}

// TestNameCellTruncationThroughRender drives the cron-pod corner case through
// the REAL pipeline (buildListView -> bridge -> templ) and asserts the DOM:
// the sticky name cell renders the truncated head in .pn-head, the INTACT
// hash tail in .pn-tail, and carries the full name in the link's title= -- the
// SPEC §4.2 escape hatch. A short name keeps the v1 markup (no title).
func TestNameCellTruncationThroughRender(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	const cronPod = "cron-data-warehouse-hourly-rollup-partition-compactor-29678881-x7k2v"

	podObject := func(name string) map[string]any {
		return map[string]any{
			"kind": "Pod", "apiVersion": "v1",
			"metadata": map[string]any{"name": name, "namespace": "default", "creationTimestamp": "2026-06-10T00:00:00Z"},
		}
	}
	table := &kube.Table{
		Resource: kube.ResourceType{Plural: "pods", Kind: "Pod", Namespaced: true, Version: "v1", APIVersion: "v1"},
		Clusters: []string{"test"},
		Columns:  []kube.Column{{Name: "Name"}, {Name: "Status"}},
		Rows: []kube.Row{
			{Cluster: "test", Object: podObject(cronPod), Cells: []any{cronPod, "Completed"}},
			{Cluster: "test", Object: podObject("nginx"), Cells: []any{"nginx", "Running"}},
		},
	}
	lc := &listContext{Cluster: "test", Namespace: "default", Plural: "pods", ClusterCount: 1, Tables: []kube.Table{*table}}
	req := httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods", nil)
	v := app.buildListView(req, lc)
	doc := renderListView(t, &v)

	link := doc.Find(`table.ro-table td.cell-name a[title="` + cronPod + `"]`)
	if link.Length() != 1 {
		t.Fatalf("truncated name link must carry the FULL name in title=, found %d", link.Length())
	}
	if got := normSpace(link.Find(".pn-head").Text()); got != "cron-data-warehouse-hourly…on-compactor" {
		t.Fatalf("rendered pn-head = %q, want the 26…12 truncated head", got)
	}
	if got := normSpace(link.Find(".pn-tail").Text()); got != "-29678881-x7k2v" {
		t.Fatalf("rendered pn-tail = %q, want the intact hash tail", got)
	}

	// The short name keeps the plain v1 markup: no title attribute.
	nginx := doc.Find(`table.ro-table td.cell-name a[href$="/pods/nginx"]`)
	if _, ok := nginx.Attr("title"); ok || nginx.Length() != 1 {
		t.Fatalf("short name must render without a title tooltip (links found: %d)", nginx.Length())
	}
}

// TestRestartsCellThousandsSeparator pins SPEC §4.5 on the real pods cell
// assembly: the webhook-dispatcher corner case (1047 restarts, data-extra.js)
// renders "1,047" with the faint "(4m ago)" suffix and the amber tone; zero
// stays "0"/faint. Unit 6's filter engine strips the comma when parsing, so
// the display format is filter-compatible by design.
func TestRestartsCellThousandsSeparator(t *testing.T) {
	cols := []string{"Name", "Ready", "Status", "Restarts", "Age"}

	cv := podsCellView(t, cols, []any{"webhook-dispatcher-79c8d6f5b-m2n3b", "1/1", "Running", "1047 (4m ago)", "17d"}, 3)
	if cv.Kind != cellRestarts || cv.Value != "1,047" {
		t.Fatalf("restarts cell = %#v, want Value 1,047", cv)
	}
	if cv.Ago != "(4m ago)" || cv.Tone != "some" {
		t.Fatalf("restarts cell = %#v, want ago=(4m ago) tone=some", cv)
	}

	zero := podsCellView(t, cols, []any{"idle", "1/1", "Running", "0", "1d"}, 3)
	if zero.Value != "0" || zero.Tone != "zero" {
		t.Fatalf("zero restarts cell = %#v, want 0/zero (no separator damage)", zero)
	}
}

// TestPendingCellStates pins SPEC §4.12 through the constructor + the real
// render: an empty address -> the faint <none>; the literal <pending> -> an
// amber PULSING dot + the word "pending" (an in-flight state, law §1.3); a
// real address -> plain text with no dot.
func TestPendingCellStates(t *testing.T) {
	if cv := pendingCellView("<pending>"); cv.Value != "pending" || cv.Tone != "warn" || !cv.Pulse {
		t.Fatalf("<pending> cell = %#v, want pending/warn/pulse", cv)
	}
	if cv := pendingCellView(""); cv.Value != "" || cv.Pulse {
		t.Fatalf("empty cell = %#v, want empty value, no pulse", cv)
	}
	if cv := pendingCellView("45.55.107.21"); cv.Value != "45.55.107.21" || cv.Tone != "" || cv.Pulse {
		t.Fatalf("plain address cell = %#v, want untouched value", cv)
	}

	cols := []string{"External-IP"}
	// The preview-env LB corner case (data-extra.js): <pending> external IP.
	doc := renderCellViews(t, "services", "Services", cols, []cellView{pendingCellView("<pending>")})
	td := firstRowTD(t, doc, 0)
	if td.Find(".cell-status.warn .ro-dot.warn.pulse").Length() != 1 {
		t.Fatalf("<pending> must render the amber pulsing dot, got %q", td.Text())
	}
	if got := normSpace(td.Text()); got != "pending" {
		t.Fatalf("<pending> cell text = %q, want the word pending (no angle brackets)", got)
	}

	doc = renderCellViews(t, "services", "Services", cols, []cellView{pendingCellView("")})
	td = firstRowTD(t, doc, 0)
	if got := normSpace(td.Find(".faint").Text()); got != "<none>" {
		t.Fatalf("empty address = %q, want the faint <none>", got)
	}
}

// TestPortsCellOverflow pins SPEC §4.11: the first 2 ports show, the rest
// collapse into a faint +N, and the FULL list rides in the tooltip (both on
// the cell and the +N). The 5-port observability-metrics service is the
// data-extra.js corner case. No ports -> the muted "—".
func TestPortsCellOverflow(t *testing.T) {
	ports := []string{"9090/TCP", "9091/TCP", "9100/TCP", "8443/TCP", "6060/TCP"}
	cv := portsCellView(ports)
	if cv.Kind != cellPorts || cv.Value != "9090/TCP, 9091/TCP" {
		t.Fatalf("ports cell = %#v, want the first 2 ports", cv)
	}
	if cv.More != "+3" || cv.Title != "9090/TCP, 9091/TCP, 9100/TCP, 8443/TCP, 6060/TCP" {
		t.Fatalf("ports cell = %#v, want +3 and the full list in Title", cv)
	}
	if cv := portsCellView([]string{"80/TCP", "443/TCP"}); cv.More != "" {
		t.Fatalf("2 ports must not overflow, got More=%q", cv.More)
	}

	doc := renderCellViews(t, "services", "Services", []string{"Ports"}, []cellView{portsCellView(ports)})
	td := firstRowTD(t, doc, 0)
	if title, _ := td.Attr("title"); title != cv.Title {
		t.Fatalf("td title = %q, want the full port list", title)
	}
	more := td.Find("span.faint")
	if normSpace(more.Text()) != "+3" {
		t.Fatalf("+N suffix = %q, want +3", more.Text())
	}
	if title, _ := more.Attr("title"); title != cv.Title {
		t.Fatalf("+N title = %q, want the full port list", title)
	}

	doc = renderCellViews(t, "services", "Services", []string{"Ports"}, []cellView{portsCellView(nil)})
	if got := normSpace(firstRowTD(t, doc, 0).Find(".faint").Text()); got != "—" {
		t.Fatalf("no ports = %q, want —", got)
	}
}

// TestHostsCellOverflow pins SPEC §4.11 for ingress hosts: ONE host shows +
// "+N hosts" faint, full list in the +N tooltip. The 4-host slr-www ingress is
// the data-extra.js corner case.
func TestHostsCellOverflow(t *testing.T) {
	hosts := []string{"sexlikereal.com", "www.sexlikereal.com", "m.sexlikereal.com", "cdn.sexlikereal.com"}
	cv := hostsCellView(hosts)
	if cv.Value != "sexlikereal.com" || cv.More != "+3 hosts" {
		t.Fatalf("hosts cell = %#v, want first host + '+3 hosts'", cv)
	}
	if cv.Title != strings.Join(hosts, "\n") {
		t.Fatalf("hosts Title = %q, want the newline-joined full list", cv.Title)
	}
	if cv := hostsCellView([]string{"api.sexlikereal.com"}); cv.More != "" || cv.Value != "api.sexlikereal.com" {
		t.Fatalf("single host must not overflow: %#v", cv)
	}

	doc := renderCellViews(t, "ingresses", "Ingresses", []string{"Hosts"}, []cellView{hostsCellView(hosts)})
	td := firstRowTD(t, doc, 0)
	if !strings.Contains(normSpace(td.Text()), "sexlikereal.com +3 hosts") {
		t.Fatalf("hosts cell text = %q, want 'sexlikereal.com +3 hosts'", normSpace(td.Text()))
	}
}

// TestTLSCellEarnedGreen pins SPEC §4.13 / the D3 colour law: the green lock +
// "tls" renders ONLY when TLS is terminated (live protection -- an earned
// green); otherwise the muted "—" with no lock and no green anywhere in the
// cell.
func TestTLSCellEarnedGreen(t *testing.T) {
	doc := renderCellViews(t, "ingresses", "Ingresses", []string{"TLS"}, []cellView{tlsCellView(true)})
	td := firstRowTD(t, doc, 0)
	on := td.Find(`.cell-status.ok[title="TLS terminated"]`)
	if on.Length() != 1 || normSpace(on.Text()) != "tls" {
		t.Fatalf("terminated TLS cell = %q, want .cell-status.ok with the word tls", td.Text())
	}
	if on.Find("svg").Length() == 0 {
		t.Fatalf("terminated TLS cell must carry the lock glyph")
	}

	doc = renderCellViews(t, "ingresses", "Ingresses", []string{"TLS"}, []cellView{tlsCellView(false)})
	td = firstRowTD(t, doc, 0)
	if got := normSpace(td.Find(".faint").Text()); got != "—" {
		t.Fatalf("unterminated TLS cell = %q, want the muted —", got)
	}
	if td.Find(".cell-status.ok, svg").Length() != 0 {
		t.Fatalf("unterminated TLS cell must show NO green and NO lock (green is earned)")
	}
}

// TestLastRunCellBuckets pins SPEC §4.14: the value (already a kubectl
// compressed duration) is bucket-coloured on the 24h window and suffixed
// " ago"; a cronjob that never ran reads the faint <never>. Fixtures are the
// data-extra.js cronjob lastRun values.
func TestLastRunCellBuckets(t *testing.T) {
	cases := []struct {
		value     string
		wantClass string
	}{
		{"59s", "age-fresh"},   // cron-billing-queues
		{"44m", "age-fresh"},   // hourly-rollup: 44m / 1440m = 0.03
		{"8h54m", "age-day"},   // backfill-epoch: 534m = 0.37 of the day
		{"41d", "age-old"},     // legacy-cleanup
		{"16h", "age-week"},    // 0.67 of the day
		{"3h2m", "age-recent"}, // 182m = 0.126
	}
	for _, c := range cases {
		cv := lastRunCellView(c.value)
		if cv.Kind != cellLastRun || cv.Value != c.value+" ago" || cv.Class != c.wantClass {
			t.Fatalf("lastRun(%q) = %#v, want %q + ' ago' in %s", c.value, cv, c.value, c.wantClass)
		}
	}

	// Never ran (cron-new-feature-warmup): the faint <never>.
	doc := renderCellViews(t, "cronjobs", "CronJobs", []string{"Last Run"}, []cellView{lastRunCellView("")})
	if got := normSpace(firstRowTD(t, doc, 0).Find(".faint").Text()); got != "<never>" {
		t.Fatalf("never-ran cell = %q, want the faint <never>", got)
	}

	doc = renderCellViews(t, "cronjobs", "CronJobs", []string{"Last Run"}, []cellView{lastRunCellView("44m")})
	if got := normSpace(firstRowTD(t, doc, 0).Find(".age-fresh").Text()); got != "44m ago" {
		t.Fatalf("lastRun DOM = %q, want '44m ago' inside the bucket class", got)
	}
}

// TestKeysCellChipsAndSecretSafety pins SPEC §4.10: data keys render as
// `name · size` chips (name firm .cv, size faint .ck), 3 show and the rest
// hide behind the `+N keys` in-cell expand (the same .xtra machinery as label
// chips); empty data reads the muted "—". Secret VALUES are STRUCTURALLY
// absent: keyChipView carries only name + size, so no constructor input can
// leak a value into the DOM. Fixtures mirror the parse-prod secret
// (data-extra.js: 6 keys).
func TestKeysCellChipsAndSecretSafety(t *testing.T) {
	keys := []keyChipView{
		{Name: "MONGODB_HOST", Size: "34 B"},
		{Name: "MONGODB_USERNAME", Size: "12 B"},
		{Name: "MONGODB_PASSWORD", Size: "44 B"},
		{Name: "PARSE_MASTER_KEY", Size: "64 B"},
		{Name: "S3_ACCESS_KEY", Size: "20 B"},
		{Name: "S3_SECRET_KEY", Size: "40 B"},
	}
	doc := renderCellViews(t, "secrets", "Secrets", []string{"Data"}, []cellView{keysCellView(keys)})
	td := firstRowTD(t, doc, 0)

	chips := td.Find(".ro-chips .ro-chip").Not(".more")
	if chips.Length() != 6 {
		t.Fatalf("key chips = %d, want 6 (one per key)", chips.Length())
	}
	if got := td.Find(".ro-chips .ro-chip.xtra").Length(); got != 3 {
		t.Fatalf("hidden overflow chips = %d, want 3 (6 keys, 3 shown)", got)
	}
	first := chips.First()
	if normSpace(first.Find(".cv").Text()) != "MONGODB_HOST" || normSpace(first.Find(".ck").Text()) != "34 B" {
		t.Fatalf("first key chip = %q, want name MONGODB_HOST (.cv) · size 34 B (.ck)", normSpace(first.Text()))
	}
	more := td.Find("button.ro-chip.more[data-more]")
	if more.Length() != 1 || normSpace(more.Find(".more-n").Text()) != "+3 keys" {
		t.Fatalf("keys overflow button = %q, want '+3 keys'", normSpace(more.Text()))
	}
	if normSpace(more.Find(".more-less").Text()) != "less" {
		t.Fatalf("expand button must carry the 'less' flip text")
	}

	// A chip is name+separator+size and NOTHING else -- the value slot does not
	// exist in the view model, so no secret material can be serialized.
	if got := normSpace(first.Text()); got != "MONGODB_HOST·34 B" && got != "MONGODB_HOST · 34 B" {
		t.Fatalf("key chip text = %q, want only name · size", got)
	}

	// Empty data (rotation-marker-empty): the muted "—", no chips.
	doc = renderCellViews(t, "secrets", "Secrets", []string{"Data"}, []cellView{keysCellView(nil)})
	td = firstRowTD(t, doc, 0)
	if got := normSpace(td.Find(".faint").Text()); got != "—" {
		t.Fatalf("empty secret data = %q, want —", got)
	}
	if td.Find(".ro-chip").Length() != 0 {
		t.Fatalf("empty secret data must render no chips")
	}
}

// TestCountCellFormat pins SPEC §4.15: events counts render ×N with a
// thousands separator; ≥20 reads chronic (the amber restarts ink), 0/1 fades,
// in-between stays plain. The 141-count BackOff event is the data-extra.js
// corner case.
func TestCountCellFormat(t *testing.T) {
	if cv := countCellView(141); cv.Value != "141" || cv.Class != "restarts some" {
		t.Fatalf("count 141 = %#v, want amber (≥20 is chronic)", cv)
	}
	if cv := countCellView(12); cv.Value != "12" || cv.Class != "" {
		t.Fatalf("count 12 = %#v, want plain (under the chronic threshold)", cv)
	}
	// The exact chronic boundary: 19 is the last plain count, 20 the first amber.
	if cv := countCellView(19); cv.Value != "19" || cv.Class != "" {
		t.Fatalf("count 19 = %#v, want plain (just under the chronic threshold)", cv)
	}
	if cv := countCellView(20); cv.Value != "20" || cv.Class != "restarts some" {
		t.Fatalf("count 20 = %#v, want amber (the ≥20 chronic boundary)", cv)
	}
	if cv := countCellView(1); cv.Class != "faint" {
		t.Fatalf("count 1 = %#v, want faint", cv)
	}
	if cv := countCellView(1047); cv.Value != "1,047" {
		t.Fatalf("count 1047 = %#v, want the thousands separator", cv)
	}

	doc := renderCellViews(t, "events", "Events", []string{"Count"}, []cellView{countCellView(141)})
	td := firstRowTD(t, doc, 0)
	if got := normSpace(td.Find(".restarts.some").Text()); got != "×141" {
		t.Fatalf("count DOM = %q, want ×141 in the amber span", got)
	}
}

// TestEvObjCellTruncation pins the SPEC §4 evobj recipe: a kind icon + the
// faint "Kind/" prefix + the object name middle-truncated at 34 chars to
// 20…8 -- with the full name in the tooltip (SPEC §4.2 beats the prototype
// DOM, which dropped it). Short names stay whole.
func TestEvObjCellTruncation(t *testing.T) {
	short := evObjCellView("Pod", "ugc-backend-8b9fc9d44-nxxz9")
	if short.Kind != cellEvObj || short.EvKind != "Pod" || short.EvName != "ugc-backend-8b9fc9d44-nxxz9" || short.Title != "" {
		t.Fatalf("short evobj = %#v, want untruncated name, no Title", short)
	}

	const long = "cron-data-warehouse-hourly-rollup-partition-compactor-29678881-x7k2v"
	cv := evObjCellView("Pod", long)
	if cv.EvName != "cron-data-warehouse-…81-x7k2v" {
		t.Fatalf("evobj name = %q, want the 20…8 middle truncation", cv.EvName)
	}
	if cv.Title != long {
		t.Fatalf("evobj Title = %q, want the full name", cv.Title)
	}

	doc := renderCellViews(t, "events", "Events", []string{"Object"}, []cellView{cv})
	td := firstRowTD(t, doc, 0)
	if title, _ := td.Attr("title"); title != long {
		t.Fatalf("evobj td title = %q, want the full name", title)
	}
	resKind := td.Find(".res-kind")
	if got := normSpace(resKind.Find(".faint").First().Text()); got != "Pod/" {
		t.Fatalf("evobj kind prefix = %q, want the faint Pod/", got)
	}
	// The kind icon resolves through the same 3-tier resolver as every other
	// kind surface (Pod is a curated tier-1 glyph -> an inline SVG).
	if resKind.Find("svg, .kind-tile").Length() == 0 {
		t.Fatalf("evobj cell must carry a kind icon")
	}
}

// TestEvAgeCellLayers pins the SPEC §4 evage recipe: the LEADING age token is
// bucket-coloured; the "(first … ago)" remainder renders as the faint 11px
// second layer. Fixtures mirror the data-extra.js event ages.
func TestEvAgeCellLayers(t *testing.T) {
	cv := evAgeCellView("3m (first 41h ago)")
	if cv.Value != "3m" || cv.EvAgeRest != "(first 41h ago)" || cv.Class != "age-fresh" {
		t.Fatalf("evage = %#v, want 3m/age-fresh + the (first 41h ago) layer", cv)
	}
	if cv := evAgeCellView("2d (first 15d ago)"); cv.Class != "age-old" || cv.EvAgeRest != "(first 15d ago)" {
		t.Fatalf("evage 2d = %#v, want age-old + remainder", cv)
	}
	if cv := evAgeCellView("59s"); cv.EvAgeRest != "" || cv.Class != "age-fresh" {
		t.Fatalf("single-layer evage = %#v, want no remainder", cv)
	}

	doc := renderCellViews(t, "events", "Events", []string{"Age"}, []cellView{evAgeCellView("3m (first 41h ago)")})
	td := firstRowTD(t, doc, 0)
	if got := normSpace(td.Find(".age-fresh").Text()); got != "3m" {
		t.Fatalf("evage first token = %q, want 3m bucket-coloured", got)
	}
	if got := normSpace(td.Find(".faint.evage-rest").Text()); got != "(first 41h ago)" {
		t.Fatalf("evage second layer = %q, want the faint (first 41h ago)", got)
	}
}

// TestMsgCellWrapsAndEscapes pins SPEC §4.16: the events message is the ONLY
// wrapping cell (td.ro-event-msg; the 520px clamp + white-space:normal live in
// CSS keyed on that class), and -- the templ auto-escaping law -- runtime
// message text can never inject markup.
func TestMsgCellWrapsAndEscapes(t *testing.T) {
	const msg = `0/20 nodes are available: 3 Insufficient cpu, 12 node(s) didn't match Pod's node affinity/selector.`
	doc := renderCellViews(t, "events", "Events", []string{"Message"}, []cellView{msgCellView(msg)})
	td := firstRowTD(t, doc, 0)
	if !td.HasClass("ro-event-msg") {
		t.Fatalf("message cell must carry td.ro-event-msg (the only wrapping cell)")
	}
	if normSpace(td.Text()) != msg {
		t.Fatalf("message text = %q, want the verbatim message", normSpace(td.Text()))
	}

	// Auto-escaping: an HTML-bearing message renders as TEXT, never as markup.
	hostile := msgCellView(`<script>alert(1)</script>`)
	doc = renderCellViews(t, "events", "Events", []string{"Message"}, []cellView{hostile})
	td = firstRowTD(t, doc, 0)
	if td.Find("script").Length() != 0 {
		t.Fatalf("message HTML must be escaped, a <script> element rendered")
	}
	if normSpace(td.Text()) != "<script>alert(1)</script>" {
		t.Fatalf("escaped message text = %q", normSpace(td.Text()))
	}
}

// TestChipsCellOverflowInTable pins SPEC §4.9 through the REAL namespaces
// pipeline (decorateNamespaceColumns -> buildListView -> templ): a 5-label row
// renders 2 visible chips, 3 extras carrying the hidden .xtra class, and the
// +N button (a real keyboard-reachable <button> with data-more for the
// delegated CSP-safe toggle) whose face flips +3 <-> "less" via the
// .more-n/.more-less pair. A 2-label row renders NO overflow machinery.
func TestChipsCellOverflowInTable(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())

	// The data-extra.js detail-label corner set, trimmed to 5: sorted by key the
	// first 2 stay visible, cost-center/team/tier overflow.
	fiveLabels := namespaceObject("baas-prod", "Active", map[string]any{
		"app.kubernetes.io/name":   "parse-server",
		"backup.velero.io/include": "true",
		"cost-center":              "eng-platform-services",
		"team":                     "backend",
		"tier":                     "production",
	})
	twoLabels := namespaceObject("ingress-nginx", "Active", map[string]any{
		"app.kubernetes.io/name": "ingress-nginx",
		"team":                   "platform",
	})

	table := &kube.Table{
		Resource: kube.ResourceType{Plural: "namespaces", Kind: "Namespace", Namespaced: false, Version: "v1", APIVersion: "v1"},
		Clusters: []string{"test"},
		Columns:  []kube.Column{{Name: "Name"}, {Name: "Status"}, {Name: "Age"}},
		Rows: []kube.Row{
			{Cluster: "test", Object: fiveLabels, Cells: []any{"baas-prod", "Active", "17d"}},
			{Cluster: "test", Object: twoLabels, Cells: []any{"ingress-nginx", "Active", "30d"}},
		},
	}
	decorateNamespaceColumns(table)
	lc := &listContext{Cluster: "test", Plural: "namespaces", ClusterCount: 1, Tables: []kube.Table{*table}}
	req := httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces", nil)
	v := app.buildListView(req, lc)
	doc := renderListView(t, &v)

	row := doc.Find(`table.ro-table tr:has(td.cell-name a:contains("baas-prod"))`)
	strip := row.Find(".ro-chips")
	if strip.Length() != 1 {
		t.Fatalf("baas-prod chips strip missing")
	}
	// 5 label chips + the +N button (which is itself a .ro-chip.more).
	if got := strip.Find(".ro-chip").Not(".more").Length(); got != 5 {
		t.Fatalf("label chips = %d, want 5 (every label renders; extras hide via CSS)", got)
	}
	xtras := strip.Find(".ro-chip.xtra")
	if xtras.Length() != 3 {
		t.Fatalf("hidden .xtra chips = %d, want 3 (5 labels, 2 shown)", xtras.Length())
	}
	// The extras are the SORTED tail (cost-center/team/tier), so the 2 visible
	// chips are the deterministic first two by key.
	xtras.Each(func(_ int, s *goquery.Selection) {
		key := normSpace(s.Find(".ck").Text())
		if key != "cost-center" && key != "team" && key != "tier" {
			t.Errorf("unexpected overflow chip %q (the first 2 sorted keys must stay visible)", key)
		}
	})
	more := strip.Find("button.ro-chip.more[data-more]")
	if more.Length() != 1 {
		t.Fatalf("the +N expand button is missing (must be a real <button> with data-more)")
	}
	if got, _ := more.Attr("type"); got != "button" {
		t.Fatalf("expand button type = %q, want button (never submits)", got)
	}
	if got, _ := more.Attr("aria-expanded"); got != "false" {
		t.Fatalf("expand button aria-expanded = %q, want false (collapsed server truth)", got)
	}
	if normSpace(more.Find(".more-n").Text()) != "+3" || normSpace(more.Find(".more-less").Text()) != "less" {
		t.Fatalf("expand button faces = %q, want +3 / less", normSpace(more.Text()))
	}

	// The mobile card projection reuses the SAME strip machinery.
	if got := doc.Find(`.ro-cardlist .ro-chips .ro-chip.xtra`).Length(); got != 3 {
		t.Fatalf("card-projection .xtra chips = %d, want 3 (same strip component)", got)
	}

	// A 2-label namespace renders all chips visible and NO overflow machinery.
	nginxRow := doc.Find(`table.ro-table tr:has(td.cell-name a:contains("ingress-nginx"))`)
	if got := nginxRow.Find(".ro-chip").Not(".more").Length(); got != 2 {
		t.Fatalf("2-label chips = %d, want 2", got)
	}
	if nginxRow.Find(".ro-chip.xtra, .ro-chip.more").Length() != 0 {
		t.Fatalf("a 2-label row must carry no .xtra chips and no +N button")
	}
}

// TestCapacityBucketCellBoundaries pins the SPEC §4.6 capacity thresholds at
// the exact boundary values the unit names: 54 -> lo, 56 -> mid, 81 -> hi
// (>55 mid, >80 hi -- 55 and 80 stay in the lower bucket).
func TestCapacityBucketCellBoundaries(t *testing.T) {
	cases := []struct {
		pct  float64
		want string
	}{
		{54, "lo"}, {55, "lo"}, {56, "mid"}, {80, "mid"}, {81, "hi"},
	}
	for _, c := range cases {
		if got := capacityBucket(c.pct); got != c.want {
			t.Fatalf("capacityBucket(%v) = %q, want %q", c.pct, got, c.want)
		}
	}
}

// TestReplicaTrackCellCap pins the SPEC §4.7 segment ceiling: a deployment
// with MORE desired replicas than the 12-segment cap renders exactly 12
// segments while the honest ratio text keeps the real numbers.
func TestReplicaTrackCellCap(t *testing.T) {
	segments, repNum := replicaTrack(20, 20, 20)
	if len(segments) != 12 {
		t.Fatalf("desired=20 renders %d segments, want the 12-segment cap", len(segments))
	}
	if repNum != "20/20" {
		t.Fatalf("ratio text = %q, want the REAL 20/20 (truth beyond the cap)", repNum)
	}
	for i, seg := range segments {
		if seg.State != "" {
			t.Fatalf("segment %d state = %q, want all-ready (filled)", i, seg.State)
		}
	}

	// Under the cap each desired replica keeps its own segment.
	segments, repNum = replicaTrack(3, 2, 2)
	if len(segments) != 3 || repNum != "2/3" {
		t.Fatalf("desired=3 = %d segments %q, want 3 segments 2/3", len(segments), repNum)
	}
}

// TestDurationAgeCellBuckets pins the SPEC §4.3 age buckets as fractions of a
// 24h window over the duration-STRING parser (units s m h d w y): <10% fresh,
// <35% recent, <65% day, <100% week, ≥1d old -- bracketing every boundary on
// both sides, plus the compound multi-unit tokens the corner-case dataset
// carries.
func TestDurationAgeCellBuckets(t *testing.T) {
	cases := []struct {
		value string
		want  string
	}{
		{"59s", "age-fresh"},
		{"2h", "age-fresh"},     // 120m = 0.083, under the 0.10 line
		{"2h24m", "age-recent"}, // 144m = exactly 0.10 -> not < 0.10
		{"8h", "age-recent"},    // 480m = 0.33
		{"8h24m", "age-day"},    // 504m = exactly 0.35
		{"15h", "age-day"},      // 900m = 0.625
		{"15h36m", "age-week"},  // 936m = exactly 0.65
		{"23h", "age-week"},     // 0.958
		{"24h", "age-old"},      // exactly the full window
		{"1d", "age-old"},
		{"41d", "age-old"},
		{"1y127d", "age-old"}, // compound year+day (legacy-billing corner case)
		{"3h2m", "age-recent"},
	}
	for _, c := range cases {
		if got := durationAgeClass(c.value); got != c.want {
			t.Fatalf("durationAgeClass(%q) = %q, want %q", c.value, got, c.want)
		}
	}

	// The parser sums multi-unit tokens (3h2m = 182 minutes).
	if got := ageMinutes("3h2m"); got != 182 {
		t.Fatalf("ageMinutes(3h2m) = %v, want 182", got)
	}
	if got := ageMinutes("1y127d"); got != 525600+127*1440 {
		t.Fatalf("ageMinutes(1y127d) = %v", got)
	}
	// Unparseable text contributes nothing (matches the reference scan).
	if got := ageMinutes("<unknown>"); got != 0 {
		t.Fatalf("ageMinutes(<unknown>) = %v, want 0", got)
	}
}

// TestGroupThousandsCellFormat pins the separator helper itself: pure digit
// strings group ("1047" -> "1,047", "1234567" -> "1,234,567"); short, empty,
// already-grouped, and decorated values pass through untouched -- so the
// helper can never corrupt a non-numeric cell.
func TestGroupThousandsCellFormat(t *testing.T) {
	cases := map[string]string{
		"0":         "0",
		"999":       "999",
		"1000":      "1,000",
		"1047":      "1,047",
		"1234567":   "1,234,567",
		"":          "",
		"1,047":     "1,047",     // already grouped: untouched
		"3 (4m":     "3 (4m",     // decorated fragments: untouched
		"<unknown>": "<unknown>", // non-numeric: untouched
	}
	for in, want := range cases {
		if got := groupThousands(in); got != want {
			t.Fatalf("groupThousands(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestMiddleTruncateCellHelper pins the exported helper directly (the palette
// feed and the detail title consume it in later units): rune-safe slicing, the
// <=max passthrough, and the lead+trail guard that refuses to "truncate" a
// name into something longer than itself.
func TestMiddleTruncateCellHelper(t *testing.T) {
	if got, tr := MiddleTruncate("short", 42, 26, 12); got != "short" || tr {
		t.Fatalf("short name must pass through, got %q %v", got, tr)
	}
	// Rune safety: multi-byte runes are never split (the result stays valid).
	multibyte := strings.Repeat("é", 50)
	got, tr := MiddleTruncate(multibyte, 42, 26, 12)
	if !tr || got != strings.Repeat("é", 26)+"…"+strings.Repeat("é", 12) {
		t.Fatalf("multibyte truncation = %q (%v)", got, tr)
	}
	// lead+trail >= len: refuse to truncate (the guard against growing a name).
	if got, tr := MiddleTruncate("0123456789012345678901234567890123456789", 34, 20, 20); tr || got != "0123456789012345678901234567890123456789" {
		t.Fatalf("lead+trail >= len must pass through, got %q %v", got, tr)
	}
}
