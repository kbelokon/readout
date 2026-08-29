package web

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"
	"github.com/kbelokon/readout/internal/web/templates"
)

// TestLiveDeltaMountAndAnnouncementContract pins the stable DOM islands Live v2
// may replace. The resource list itself is deliberately not an aria-live region:
// high-churn watches announce one coalesced summary through the persistent status
// node instead of causing assistive technology to re-read the whole table.
func TestLiveDeltaMountAndAnnouncementContract(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC))
	p := get(t, app, "/clusters/test/namespaces/default/pods", http.StatusOK)

	p.wantAbsent("#resource-list-content[aria-live]")
	p.wantAttr("#ro-live-status", "role", "status")
	p.wantAttr("#ro-live-status", "aria-live", "polite")
	p.wantAttr("#ro-live-status", "aria-atomic", "true")

	for _, region := range []string{"count", "phase", "found"} {
		selector := `[data-ro-live-region="` + region + `"]`
		if got := p.count(selector); got != 1 {
			t.Fatalf("%s count = %d, want one stable Live delta mount", selector, got)
		}
	}
}

// TestLiveDeltaFragmentsAreSnapshotComponents proves the delta fragments and
// the full ResourceTable use the exact same templ components. This is a byte
// contract: changing identity attributes or rich cell/card markup in only one
// render path must fail before a rolling client/server pair can drift.
func TestLiveDeltaFragmentsAreSnapshotComponents(t *testing.T) {
	row := templates.TableRow{
		StatusClass:  "ok",
		Key:          "test/default/pod-a",
		DomID:        "row-test-default-pod-a",
		Name:         "pod-a",
		OpenHref:     "/clusters/test/namespaces/default/pods/pod-a",
		YAMLHref:     "/clusters/test/namespaces/default/pods/pod-a?view=yaml",
		DownloadHref: "/clusters/test/namespaces/default/pods/pod-a?download=yaml",
		Cells: []templates.TableCell{
			{Kind: templates.CellName, Value: "pod-a", NameHead: "pod-a", Href: "/clusters/test/namespaces/default/pods/pod-a"},
			{Kind: templates.CellStatus, Value: "Running", Tone: "ok", ColClass: "cell-status"},
		},
		CreatedText: "2026-06-04 11:59:00",
	}
	table := templates.TableData{
		Kind:        "Pods",
		Count:       1,
		ColumnCount: 2,
		Columns: []templates.TableColumn{
			{Name: "Name"},
			{Name: "Status"},
		},
		Phase:     []templates.PhaseChip{{Tone: "ok", Label: "Running", Count: "1"}},
		PhaseRows: 1,
		Rows:      []templates.TableRow{row},
	}
	d := templates.ListData{
		Plural:          "pods",
		ClusterCount:    1,
		TableCount:      1,
		TotalRows:       1,
		DurationSeconds: 0.012,
		Tables:          []templates.TableData{table},
	}

	full := renderLiveComponent(t, templates.ResourceTable(d))
	for name, component := range map[string]templ.Component{
		"row":   templates.LiveTableRow(d, table, row),
		"card":  templates.LiveResourceCard(d, table, row),
		"count": templates.LiveCountRegion(table),
		"phase": templates.LivePhaseRegion(d, table),
		"found": templates.LiveFoundRegion(d),
	} {
		fragment := renderLiveComponent(t, component)
		if fragment == "" || !strings.Contains(full, fragment) {
			t.Fatalf("full ResourceTable does not contain byte-identical %s component: %q", name, fragment)
		}
	}
}

func renderLiveComponent(t *testing.T, component templ.Component) string {
	t.Helper()
	var buf bytes.Buffer
	if err := component.Render(context.Background(), &buf); err != nil {
		t.Fatalf("render Live component: %v", err)
	}
	return buf.String()
}
