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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// detail_redesign_test.go pins the redesign object-detail spine through the REAL
// render pipeline (buildDetailView assembly -> toDetailData bridge -> the
// ResourceView templ). Each fact is an independent statement about how an object
// maps onto the redesign detail vocabulary -- the .ro-rd content marker, the
// .ro-detail-title / .ro-kind-badge header, the Default/YAML/Events(/Logs) tabs,
// the NEUTRAL label chips (D3: every label is a plain .ro-chip with the
// .ck/.cs/.cv ink-weight split; the green .app accent is retired), the
// collapsible+copyable YAML cards, the toned Events table, and the chroma token
// spans in the YAML body.

// detailObject builds a kube.Object for the given kind/labels, used to drive the
// detail assembly without a fake-API round-trip.
func detailObject(plural, kind string, namespaced bool, labels, annotations map[string]any) *kube.Object {
	rt := kube.ResourceType{APIVersion: "v1", Version: "v1", Plural: plural, Kind: kind, Namespaced: namespaced}
	md := map[string]any{
		"name":              strings.ToLower(kind) + "-0",
		"creationTimestamp": "2024-03-01T08:00:00Z",
		"resourceVersion":   "12345",
	}
	if namespaced {
		md["namespace"] = "default"
	}
	if len(labels) > 0 {
		md["labels"] = labels
	}
	if len(annotations) > 0 {
		md["annotations"] = annotations
	}
	obj := kube.NewObject(&rt, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       kind,
		"metadata":   md,
		"spec":       map[string]any{"replicas": int64(1)},
		"status":     map[string]any{"phase": "Running"},
	}})
	return &obj
}

// renderDetailView drives a package-web detailView through the REAL bridge
// (toDetailData -> the ResourceView templ) and parses the output, so the detail
// view-models are asserted through the production render path rather than a
// hand-built templates.DetailData.
func renderDetailView(t *testing.T, v *detailView) *goquery.Document {
	t.Helper()
	var sb strings.Builder
	if err := templates.ResourceView(toDetailData(v)).Render(context.Background(), &sb); err != nil {
		t.Fatalf("render detail view: %v", err)
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(sb.String()))
	if err != nil {
		t.Fatalf("parse detail view: %v", err)
	}
	return doc
}

// buildDefaultDetailView assembles a Default-tab detailView for obj through the
// real assembly helpers (label/annotation chips + YAML cards), mirroring the
// non-YAML branch of buildDetailView without the live kube client.
func buildDefaultDetailView(t *testing.T, obj *kube.Object) *detailView {
	t.Helper()
	app := newServer(t, baseConfig(t), time.Now())
	v := &detailView{
		Cluster:      "test",
		Namespace:    obj.Namespace(),
		Object:       *obj,
		DefaultTab:   true,
		CreatedMeta:  formatTimestamp(obj.CreationTimestamp()),
		Version:      nestedString(obj.Raw, "metadata", "resourceVersion"),
		DownloadHref: objectDownloadYAMLHref("test", obj.Namespace(), obj),
	}
	v.NameHead, v.NameTail, v.NameTitle = detailNameParts(obj)
	v.Labels = buildLabelChips("test", obj.Namespace(), obj)
	v.Annotations, v.AnnotationsLong = buildAnnotationChips(obj)
	v.YAMLCards = app.buildYAMLCards("test", obj.Namespace(), obj)
	return v
}

// TestResourceViewDetailSpine pins the redesign detail header + content marker:
// the outermost content element carries .ro-rd (D13), the title is the
// .ro-detail-title row with an H1.ro-title name + a .ro-kind-badge + a quiet
// .ro-detail-actions Download button + the .ro-detail-meta line.
func TestResourceViewDetailSpine(t *testing.T) {
	obj := detailObject("deployments", "Deployment", true, nil, nil)
	doc := renderDetailView(t, buildDefaultDetailView(t, obj))

	if doc.Find(".ro-rd").Length() == 0 {
		t.Fatalf("detail view missing the .ro-rd content marker (D13)")
	}
	if doc.Find(".ro-rd .ro-detail-title").Length() == 0 {
		t.Fatalf(".ro-detail-title is not under the .ro-rd marker")
	}
	if got := normSpace(doc.Find(".ro-detail-title h1.ro-title").Text()); got != "deployment-0" {
		t.Fatalf("detail H1 = %q, want deployment-0", got)
	}
	if got := normSpace(doc.Find(".ro-detail-title .ro-kind-badge").Text()); got != "Deployment" {
		t.Fatalf("kind badge = %q, want Deployment", got)
	}
	if doc.Find(`.ro-detail-actions a[title="Download resource object as YAML"]`).Length() != 1 {
		t.Fatalf("missing the quiet Download-YAML action button in .ro-detail-actions")
	}
	if got := normSpace(doc.Find(".ro-detail-meta").Text()); got != "created 2024-03-01 08:00:00, version 12345" {
		t.Fatalf("detail meta = %q", got)
	}
}

// TestResourceViewTabsPodVsNonPod pins the tab count rule: a non-pod object gets
// three tabs (Default / YAML / Events); a Pod (namespaced, with a Logs href)
// gets a fourth Logs tab. The active tab carries .is-active.
func TestResourceViewTabsPodVsNonPod(t *testing.T) {
	// Non-pod: three tabs.
	nonPod := buildDefaultDetailView(t, detailObject("deployments", "Deployment", true, nil, nil))
	doc := renderDetailView(t, nonPod)
	tabs := docTexts(doc, ".ro-tabs a")
	if strings.Join(tabs, "|") != "Default|YAML|Events" {
		t.Fatalf("non-pod tabs = %v, want Default|YAML|Events", tabs)
	}
	if got := normSpace(doc.Find(".ro-tabs a.is-active").Text()); got != "Default" {
		t.Fatalf("active tab = %q, want Default", got)
	}

	// Pod: a Logs href adds the fourth Logs tab.
	pod := buildDefaultDetailView(t, detailObject("pods", "Pod", true, nil, nil))
	pod.LogsHref = "/clusters/test/namespaces/default/pods/pod-0/logs"
	podDoc := renderDetailView(t, pod)
	podTabs := docTexts(podDoc, ".ro-tabs a")
	if strings.Join(podTabs, "|") != "Default|YAML|Events|Logs" {
		t.Fatalf("pod tabs = %v, want Default|YAML|Events|Logs", podTabs)
	}
	if href, _ := podDoc.Find(`.ro-tabs a:contains("Logs")`).Attr("href"); href != "/clusters/test/namespaces/default/pods/pod-0/logs" {
		t.Fatalf("Logs tab href = %q", href)
	}
}

// TestDetailLabelChipsNeutral pins the D3 colour law on the detail labels: an
// app.kubernetes.io/* label renders as a PLAIN neutral .ro-chip anchor exactly
// like any other label -- the retired green .app accent never appears -- and
// every chip splits its key (.ck), ghost separator (.cs), and value (.cv) so
// ink weight, not hue, differentiates them. The href is the click-to-filter
// chip link (D7/SPEC §8.1): this kind's list with `?f=label:key=value`, the
// chip text QueryEscape'd whole so '/' and '=' survive literally.
func TestDetailLabelChipsNeutral(t *testing.T) {
	obj := detailObject("deployments", "Deployment", true, map[string]any{
		"app.kubernetes.io/component": "master",
		"tier":                        "backend",
	}, nil)
	doc := renderDetailView(t, buildDefaultDetailView(t, obj))

	// NEGATIVE (the regression net for the retired class): no rendered chip on
	// the detail page carries the .app accent -- not even for app.kubernetes.io/*.
	if got := doc.Find(".ro-chip.app").Length(); got != 0 {
		t.Fatalf("retired .ro-chip.app accent rendered %d time(s); labels are neutral (D3)", got)
	}

	// The app.kubernetes.io/* label is an ordinary neutral chip: addressed by its
	// `?f=label:key=value` chip href, carrying the .ck/.cs/.cv ink-weight split.
	appChip := doc.Find(`.ro-chips a.ro-chip[href="/clusters/test/namespaces/default/deployments?f=label%3Aapp.kubernetes.io%2Fcomponent%3Dmaster"]`)
	if appChip.Length() != 1 {
		t.Fatalf("expected the app.kubernetes.io/component label as one plain .ro-chip anchor, got %d; hrefs=%v",
			appChip.Length(), attrsOf(doc, ".ro-chips a.ro-chip", "href"))
	}
	if k, v := normSpace(appChip.Find(".ck").Text()), normSpace(appChip.Find(".cv").Text()); k != "app.kubernetes.io/component" || v != "master" {
		t.Fatalf("chip ck/cv = %q/%q, want app.kubernetes.io/component/master", k, v)
	}
	if appChip.Find(".cs").Length() != 1 {
		t.Fatalf("chip missing the .cs separator span")
	}
	// The ordinary "tier" label renders identically (one plain chip, same split).
	tierChip := doc.Find(`.ro-chips a.ro-chip:has(.ck:contains("tier"))`)
	if tierChip.Length() != 1 {
		t.Fatalf("expected the tier label as a plain .ro-chip, got %d", tierChip.Length())
	}
	if v := normSpace(tierChip.Find(".cv").Text()); v != "backend" {
		t.Fatalf("tier chip .cv = %q, want backend", v)
	}
}

// TestDetailAnnotationChipTruncation pins the annotation tooltip contract: a
// long annotation VALUE (> 45 runes, past the truncate(...,40) threshold) must
// render with the chip BODY clipped (ends with the "..." ellipsis the truncate
// helper appends, and is shorter than the full value) while the title= carries
// the FULL untruncated "key: value". The body and the title therefore DIVERGE --
// the whole point of the tooltip. A short annotation (below threshold) keeps body
// == title, which is also asserted so both branches are exercised. Driven through
// the real assembly + render pipeline (buildAnnotationChips -> the templ).
func TestDetailAnnotationChipTruncation(t *testing.T) {
	const fullVal = "this-is-a-very-long-annotation-value-that-must-be-clipped-in-the-chip-body"
	const shortVal = "generic-annotation"
	obj := detailObject("deployments", "Deployment", true, nil, map[string]any{
		"example.com/note":  fullVal,  // > 45 runes -> truncates
		"example.com/short": shortVal, // < 45 runes -> stays whole
	})
	doc := renderDetailView(t, buildDefaultDetailView(t, obj))

	// The long-value chip: addressed by the FULL value in its title= (the title is
	// what should carry the untruncated string).
	fullTitle := "example.com/note: " + fullVal
	longChip := doc.Find(`span.ro-chip.anno[title="` + fullTitle + `"]`)
	if longChip.Length() != 1 {
		t.Fatalf("expected one annotation chip whose title= holds the FULL value %q; titles=%v",
			fullTitle, attrsOf(doc, "span.ro-chip.anno", "title"))
	}

	// The visible VALUE (.cv span) is the clipped form: ends with the "..."
	// ellipsis and is strictly shorter than the full value.
	bodyVal := normSpace(longChip.Find(".cv").Text())
	if !strings.HasSuffix(bodyVal, "...") {
		t.Fatalf("annotation chip value = %q, want it clipped with a trailing %q ellipsis", bodyVal, "...")
	}
	if len([]rune(bodyVal)) >= len([]rune(fullVal)) {
		t.Fatalf("annotation chip value (%d runes) must be SHORTER than the full value (%d runes): value=%q", len([]rune(bodyVal)), len([]rune(fullVal)), bodyVal)
	}
	// The value is the exact truncate(value,40) output; the key sits in .ck.
	if bodyVal != "this-is-a-very-long-annotation-value-..." {
		t.Fatalf("annotation chip value = %q, want the truncate(...,40) clipped form", bodyVal)
	}
	if k := normSpace(longChip.Find(".ck").Text()); k != "example.com/note" {
		t.Fatalf("annotation chip key = %q, want example.com/note", k)
	}
	// Body and title DIVERGE: the title must NOT equal the clipped body text.
	if title, _ := longChip.Attr("title"); title == normSpace(longChip.Text()) {
		t.Fatalf("annotation tooltip is useless: title equals the clipped body %q (full value lost)", title)
	}

	// The short-value chip keeps its full value visible (below the truncate
	// threshold), so the non-truncating branch is exercised too.
	shortTitle := "example.com/short: " + shortVal
	shortChip := doc.Find(`span.ro-chip.anno[title="` + shortTitle + `"]`)
	if shortChip.Length() != 1 {
		t.Fatalf("expected the short annotation chip with title=%q", shortTitle)
	}
	if got := normSpace(shortChip.Find(".cv").Text()); got != shortVal {
		t.Fatalf("short annotation chip value = %q, want it whole (== %q)", got, shortVal)
	}
}

// TestYAMLCardCollapsibleCopyable pins the per-section YAML card contract that
// readout.js keys off: a collapsible[data-name] card whose head holds the
// h4.title fold target + the .ro-copy-btn, with the highlighted body in
// .ro-card-content.
func TestYAMLCardCollapsibleCopyable(t *testing.T) {
	obj := detailObject("deployments", "Deployment", true, nil, nil)
	doc := renderDetailView(t, buildDefaultDetailView(t, obj))

	// The object's top-level sections (spec/status) each render a card.
	specCard := doc.Find(`.ro-yaml-card.collapsible[data-name="spec"]`)
	if specCard.Length() != 1 {
		t.Fatalf("expected a collapsible spec YAML card, got %d", specCard.Length())
	}
	if specCard.Find(".ro-card-head h4.title").Length() != 1 {
		t.Fatalf("spec card head missing its h4.title fold target")
	}
	if specCard.Find(".ro-card-head .ro-copy-btn").Length() != 1 {
		t.Fatalf("spec card missing its .ro-copy-btn")
	}
	if specCard.Find(".ro-card-content .highlighttable").Length() != 1 {
		t.Fatalf("spec card body missing the highlighted .ro-card-content .highlighttable")
	}
}

// TestEventsTabTonedTable pins the Events-tab table (the ?view=events branch):
// the redesign .ro-table renders each event with a toned .cell-status/.ro-dot
// Type cell, an age-bucketed Age cell, a faint From cell, and the wrapping
// .ro-event-msg Message cell.
func TestEventsTabTonedTable(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC))
	events := app.buildEventViews([]map[string]any{
		{
			"type": "Warning", "reason": "Unhealthy",
			"message":       "Readiness probe failed",
			"lastTimestamp": "2024-05-31T23:00:00Z",
			"source":        map[string]any{"component": "kubelet"},
		},
		{
			"type": "Normal", "reason": "Scheduled",
			"message":       "Successfully assigned default/x to node-1",
			"lastTimestamp": "2024-03-01T10:00:00Z",
			"source":        map[string]any{"component": "default-scheduler"},
		},
	})
	v := &detailView{Cluster: "test", Namespace: "default", Object: *detailObject("pods", "Pod", true, nil, nil), EventsTab: true, IsEventsView: true, Events: events}
	doc := renderDetailView(t, v)

	if doc.Find(".ro-section .ro-table-wrap table.ro-table").Length() != 1 {
		t.Fatalf("events tab missing the redesign .ro-table")
	}
	rows := doc.Find("table.ro-table tbody tr")
	if rows.Length() != 2 {
		t.Fatalf("event rows = %d, want 2", rows.Length())
	}
	// Warning -> warn tone on the first row; Normal -> mute on the second.
	warnRow := rows.Eq(0)
	if warnRow.Find(".cell-status.warn .ro-dot.warn").Length() != 1 {
		t.Fatalf("Warning event row missing .cell-status.warn > .ro-dot.warn: %s", normSpace(warnRow.Text()))
	}
	muteRow := rows.Eq(1)
	if muteRow.Find(".cell-status.mute .ro-dot.mute").Length() != 1 {
		t.Fatalf("Normal event row missing .cell-status.mute > .ro-dot.mute: %s", normSpace(muteRow.Text()))
	}
	// The Message cell is the one wrapping cell.
	if got := normSpace(muteRow.Find("td.ro-event-msg").Text()); got != "Successfully assigned default/x to node-1" {
		t.Fatalf("event message cell = %q", got)
	}
	// The detail tab inherits the events-list cells (D15): a countless event
	// reads the faint ×1 in the Count column.
	if got := normSpace(muteRow.Find("td.num span.faint").Text()); got != "×1" {
		t.Fatalf("event count cell = %q, want the faint ×1", got)
	}
	// Age: the compressed duration since lastTimestamp, bucket class on the
	// span (the 2024-03 event is months before the fixed clock -> 91d/age-old).
	if got := normSpace(muteRow.Find("td span.age-old").Text()); got != "91d" {
		t.Fatalf("event age cell = %q, want the age-old 91d duration", got)
	}
	// From cell is faint.
	if muteRow.Find("td.faint").Length() == 0 {
		t.Fatalf("event From cell missing the faint class")
	}
}

// TestResourceViewYAMLChromaSpans pins D7 end to end: the YAML tab renders the
// full manifest through the chroma highlighter (server-side), so the body carries
// the Pygments token spans (.nt keys, .l unquoted literals) -- the recolour-only
// path, not a re-tokeniser.
func TestResourceViewYAMLChromaSpans(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Now())
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods/nginx?view=yaml", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(rec.Body.String()))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if doc.Find("td.code .nt").Length() == 0 {
		t.Fatalf("YAML body missing chroma key token spans (.nt)")
	}
	if doc.Find("td.code .l").Length() == 0 {
		t.Fatalf("YAML body missing chroma unquoted-literal token spans (.l)")
	}
	// The YAML view is wrapped so the redesign full-manifest styles can hook it.
	if doc.Find(".ro-rd .ro-yaml-view").Length() == 0 {
		t.Fatalf("YAML view not wrapped in .ro-rd .ro-yaml-view")
	}
}

// docTexts is a goquery convenience mirroring page.texts for a raw document
// (the detail render helper returns a *goquery.Document, not a *page).
func docTexts(doc *goquery.Document, selector string) []string {
	var out []string
	doc.Find(selector).Each(func(_ int, s *goquery.Selection) {
		out = append(out, normSpace(s.Text()))
	})
	return out
}

// attrsOf returns the named attribute of every match of selector (for readable
// failure messages); mirrors page.attrs for a raw *goquery.Document.
func attrsOf(doc *goquery.Document, selector, name string) []string {
	var out []string
	doc.Find(selector).Each(func(_ int, s *goquery.Selection) {
		if v, ok := s.Attr(name); ok {
			out = append(out, v)
		}
	})
	return out
}
