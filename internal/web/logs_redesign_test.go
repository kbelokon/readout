package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
	"github.com/kbelokon/readout/internal/config"
	"github.com/kbelokon/readout/internal/web/templates"
)

// logs_redesign_test.go pins the redesign container-logs page through the REAL
// render pipeline (the Logs templ + the package-web logPreHTML log-block builder
// + the live handler). Each fact is an independent statement about how the logs
// view maps onto the redesign vocabulary: the .ro-rd content marker (D13), the
// detail title + .ro-tabs chrome shared with resource_view (so detail and logs
// stay consistent), the .ro-logs-form tail/filter/Refresh form, the .ro-logtabs
// per-container pills (active = green pill), the .ro-logpre block whose lines are
// .log-line > .log-src + the colored .log-cN container name + .log-ts + message,
// the container-name -> .log-cN palette index via podColor, and the
// showContainerLogs-off disabled notice.

// renderLogs drives a templates.LogsData through the Logs templ and parses the
// output, so the logs view-model is asserted through the production render path
// (mirroring renderDetailView for the detail spine).
func renderLogs(t *testing.T, d *templates.LogsData) *goquery.Document {
	t.Helper()
	var sb strings.Builder
	if err := templates.Logs(*d).Render(context.Background(), &sb); err != nil {
		t.Fatalf("render logs view: %v", err)
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(sb.String()))
	if err != nil {
		t.Fatalf("parse logs view: %v", err)
	}
	return doc
}

// TestLogsRedesignChrome pins the logs spine: the outermost content element
// carries .ro-rd (D13), the title is the .ro-detail-title row (H1 name + kind
// badge) and the tabs are the SAME .ro-tabs set as resource_view
// (Default/YAML/Events/Logs) with Logs the .is-active tab and the sibling tabs
// linking back to the detail GETs -- so the detail and logs screens read
// consistently.
func TestLogsRedesignChrome(t *testing.T) {
	base := "/clusters/test/namespaces/default/pods/redis-master-0"
	doc := renderLogs(t, &templates.LogsData{
		Name:              "redis-master-0",
		Kind:              "Pod",
		DefaultHref:       base,
		YAMLHref:          base + "?view=yaml",
		EventsHref:        base + "?view=events",
		ShowContainerLogs: true,
		TailLines:         200,
		PodCount:          1,
		LogPre:            `<pre class="ro-logpre">` + "\n</pre>",
	})

	if doc.Find(".ro-rd").Length() == 0 {
		t.Fatalf("logs view missing the .ro-rd content marker (D13)")
	}
	if doc.Find(".ro-rd .ro-detail-title").Length() == 0 {
		t.Fatalf(".ro-detail-title is not under the .ro-rd marker")
	}
	if got := normSpace(doc.Find(".ro-detail-title h1.ro-title").Text()); got != "redis-master-0" {
		t.Fatalf("logs H1 = %q, want redis-master-0", got)
	}
	if got := normSpace(doc.Find(".ro-detail-title .ro-kind-badge").Text()); got != "Pod" {
		t.Fatalf("logs kind badge = %q, want Pod", got)
	}
	// Same tab set + order as the detail view, Logs active.
	if tabs := docTexts(doc, ".ro-tabs a"); strings.Join(tabs, "|") != "Default|YAML|Events|Logs" {
		t.Fatalf("logs tabs = %v, want Default|YAML|Events|Logs", tabs)
	}
	if got := normSpace(doc.Find(".ro-tabs a.is-active").Text()); got != "Logs" {
		t.Fatalf("active logs tab = %q, want Logs", got)
	}
	// The sibling tabs link back to the detail GETs (read-only).
	if href, _ := doc.Find(`.ro-tabs a:contains("Default")`).Attr("href"); href != base {
		t.Fatalf("Default tab href = %q, want %q", href, base)
	}
	if href, _ := doc.Find(`.ro-tabs a:contains("Events")`).Attr("href"); href != base+"?view=events" {
		t.Fatalf("Events tab href = %q", href)
	}
}

// TestLogsRedesignFormAndTabs pins the .ro-logs-form (tail input + filter input +
// Refresh button) and the per-container .ro-logtabs: each container is an anchor,
// exactly one carries .is-active (the green pill), and the active one is a plain
// span (no link) while the others link to their container-scoped log GET.
func TestLogsRedesignFormAndTabs(t *testing.T) {
	base := "/clusters/test/namespaces/default/pods/redis-master-0/logs"
	doc := renderLogs(t, &templates.LogsData{
		Name:              "redis-master-0",
		Kind:              "Pod",
		ShowContainerLogs: true,
		TailLines:         200,
		PodCount:          1,
		FilterVal:         "warn",
		Containers: []templates.LogContainerTab{
			{Active: true, Label: "all"},
			{Label: "metrics", Href: base + "?container=metrics&tail_lines=200&filter=warn"},
			{Label: "redis", Href: base + "?container=redis&tail_lines=200&filter=warn"},
		},
		LogPre: `<pre class="ro-logpre">` + "\n</pre>",
	})

	// The tail/filter/Refresh form.
	form := doc.Find("form.ro-logs-form")
	if form.Length() != 1 {
		t.Fatalf("expected exactly one form.ro-logs-form, got %d", form.Length())
	}
	if form.Find(`input[name="tail_lines"]`).Length() != 1 {
		t.Fatalf("logs form missing the tail_lines input")
	}
	if val, _ := form.Find(`input[name="filter"]`).Attr("value"); val != "warn" {
		t.Fatalf("logs filter input value = %q, want warn", val)
	}
	if got := normSpace(form.Find("button.ro-btn[type=submit]").Text()); got != "Refresh" {
		t.Fatalf("logs Refresh button = %q, want Refresh", got)
	}

	// The container pills: three anchors, exactly one active.
	pills := doc.Find(".ro-logtabs a")
	if pills.Length() != 3 {
		t.Fatalf("ro-logtabs anchors = %d, want 3", pills.Length())
	}
	active := doc.Find(".ro-logtabs a.is-active")
	if active.Length() != 1 {
		t.Fatalf("ro-logtabs active pills = %d, want exactly 1", active.Length())
	}
	if got := normSpace(active.Text()); got != "all" {
		t.Fatalf("active container pill = %q, want all", got)
	}
	// The active pill is a plain span (no link); a non-active pill links.
	if _, ok := active.Attr("href"); ok {
		t.Fatalf("active container pill must not be a link")
	}
	if href, _ := doc.Find(`.ro-logtabs a:contains("redis")`).Attr("href"); href != base+"?container=redis&tail_lines=200&filter=warn" {
		t.Fatalf("redis pill href = %q", href)
	}
}

// TestLogsRedesignLogLineStructure pins one rendered .ro-logpre line through the
// REAL logPreHTML builder: a .log-line block wrapping the .log-src source pod, the
// container name in a colored .log-cN span (palette index = podColor(container)),
// the .log-ts timestamp split off the entry, then the bare message.
func TestLogsRedesignLogLineStructure(t *testing.T) {
	pre := logPreHTML([]logLine{
		{Text: "2026-01-01T00:00:02Z GET / 200", Pod: "redis-master-0", Container: "redis"},
	}, "")
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(pre))
	if err != nil {
		t.Fatalf("parse log pre: %v", err)
	}

	line := doc.Find("pre.ro-logpre .log-line")
	if line.Length() != 1 {
		t.Fatalf("log lines = %d, want 1", line.Length())
	}
	if got := normSpace(line.Find(".log-src").Text()); got != "redis-master-0" {
		t.Fatalf(".log-src = %q, want redis-master-0", got)
	}
	// The container name carries the palette index keyed off the CONTAINER name,
	// not the pod -- podColor("redis") == log-c7.
	ctr := line.Find("." + podColor("redis"))
	if ctr.Length() != 1 {
		t.Fatalf("expected the container span to carry %q, got none\nhtml=%s", podColor("redis"), pre)
	}
	if got := normSpace(ctr.Text()); got != "redis" {
		t.Fatalf("colored container span text = %q, want redis", got)
	}
	if got := normSpace(line.Find(".log-ts").Text()); got != "2026-01-01T00:00:02Z" {
		t.Fatalf(".log-ts = %q, want the RFC3339 timestamp", got)
	}
	// The message is the bare remainder after the timestamp.
	if !strings.Contains(normSpace(line.Text()), "GET / 200") {
		t.Fatalf("log line missing the bare message: %q", normSpace(line.Text()))
	}
}

// TestLogsRedesignContainerPalette pins that the .log-cN palette index follows the
// CONTAINER name (the mockup colours per container, e.g. redis->c7, sidecar->c1):
// two lines from the SAME pod but DIFFERENT containers get DIFFERENT colored
// container spans, proving the colour is hashed off the container, not the pod.
func TestLogsRedesignContainerPalette(t *testing.T) {
	pre := logPreHTML([]logLine{
		{Text: "2026-01-01T00:00:00Z up", Pod: "app-pod", Container: "redis"},
		{Text: "2026-01-01T00:00:01Z up", Pod: "app-pod", Container: "sidecar"},
	}, "")
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(pre))
	if err != nil {
		t.Fatalf("parse log pre: %v", err)
	}
	// redis -> log-c7, sidecar -> log-c1 -- distinct palette classes for distinct
	// containers under one pod.
	if doc.Find(".log-line ."+podColor("redis")).Length() != 1 {
		t.Fatalf("redis line missing its %q container span\nhtml=%s", podColor("redis"), pre)
	}
	if doc.Find(".log-line ."+podColor("sidecar")).Length() != 1 {
		t.Fatalf("sidecar line missing its %q container span\nhtml=%s", podColor("sidecar"), pre)
	}
	if podColor("redis") == podColor("sidecar") {
		t.Fatalf("test fixture broken: redis and sidecar must hash to different palette indices")
	}
}

// TestLogsRedesignDisabledNotice pins the showContainerLogs gate: with container
// logs DISABLED the page renders the disabled .ro-notice (and no form / no
// .ro-logpre), while with logs ENABLED the live handler renders the form + the
// .ro-logpre stream carrying the fixture's "GET / 200" line. Driven through the
// live handler so the gate is exercised end to end.
func TestLogsRedesignDisabledNotice(t *testing.T) {
	// Disabled (default): the notice replaces the form + stream.
	off := renderLogs(t, &templates.LogsData{
		Name: "nginx", Kind: "Pod", ShowContainerLogs: false,
	})
	if off.Find(".ro-rd .ro-notice").Length() != 1 {
		t.Fatalf("disabled logs view missing the .ro-notice")
	}
	if !strings.Contains(normSpace(off.Find(".ro-notice").Text()), "Container Logs Disabled") {
		t.Fatalf("disabled notice text = %q", normSpace(off.Find(".ro-notice").Text()))
	}
	if off.Find("form.ro-logs-form").Length() != 0 || off.Find("pre.ro-logpre").Length() != 0 {
		t.Fatalf("disabled logs view must not render the form or the log stream")
	}

	// Enabled, through the live handler: the form + the .ro-logpre stream render.
	cfg := &config.Config{
		Port:              8080,
		Clusters:          []config.ClusterConnection{{Name: "test", Server: newServerFakeAPI(t).URL}},
		DefaultTheme:      "dark",
		ShowContainerLogs: true,
	}
	app := newTestServerWithConfig(t, cfg)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods/nginx/logs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("enabled logs status = %d", rec.Code)
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(rec.Body.String()))
	if err != nil {
		t.Fatalf("parse enabled logs: %v", err)
	}
	if doc.Find("form.ro-logs-form").Length() != 1 {
		t.Fatalf("enabled logs view missing the .ro-logs-form")
	}
	stream := doc.Find("pre.ro-logpre")
	if stream.Length() != 1 {
		t.Fatalf("enabled logs view missing the .ro-logpre stream")
	}
	// The fixture pod's single container is nginx -> log-c3; the stream line
	// colours that container span and carries the GET / 200 message.
	if doc.Find(".log-line ."+podColor("nginx")).Length() == 0 {
		t.Fatalf("enabled log stream missing the %q container span", podColor("nginx"))
	}
	if !strings.Contains(normSpace(stream.Text()), "GET / 200") {
		t.Fatalf("enabled log stream missing GET / 200: %q", normSpace(stream.Text()))
	}
}
