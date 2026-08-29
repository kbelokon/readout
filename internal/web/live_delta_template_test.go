package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"
	"github.com/kbelokon/readout/internal/web/icons"
	"github.com/kbelokon/readout/internal/web/templates"
)

const liveRenderContractPath = "testdata/live_render_contract.json"

type liveRenderContract struct {
	Version int                        `json:"version"`
	Rows    []liveRenderContractRow    `json:"rows"`
	Regions []liveRenderContractRegion `json:"regions"`
}

type liveRenderContractRow struct {
	Name string `json:"name"`
	Key  string `json:"key"`
	Row  string `json:"row"`
	Card string `json:"card"`
}

type liveRenderContractRegion struct {
	Region string `json:"region"`
	HTML   string `json:"html"`
}

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

// TestLiveRenderContractGolden keeps one shared server/client fixture at the
// actual templ boundary. The companion Vitest reads this file, decodes one Live
// delta, and applies every row, card, and closed region. That catches drift in
// rich markup without reimplementing the templates in TypeScript.
func TestLiveRenderContractGolden(t *testing.T) {
	got := marshalLiveRenderContract(t, buildLiveRenderContract(t))
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(liveRenderContractPath, got, 0o644); err != nil {
			t.Fatalf("update %s: %v", liveRenderContractPath, err)
		}
	}
	want, err := os.ReadFile(liveRenderContractPath)
	if err != nil {
		t.Fatalf("read %s: %v", liveRenderContractPath, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is stale; run UPDATE_GOLDEN=1 go test ./internal/web -run TestLiveRenderContractGolden", filepath.ToSlash(liveRenderContractPath))
	}
}

func buildLiveRenderContract(t *testing.T) liveRenderContract {
	t.Helper()
	allKinds := allLiveContractCellKinds()
	if got, want := len(allKinds), int(templates.CellMsg)+1; got != want {
		t.Fatalf("Live render contract covers %d CellKind values, want %d", got, want)
	}
	seenKinds := make(map[templates.CellKind]struct{}, len(allKinds))
	for i := range allKinds {
		cell := &allKinds[i]
		seenKinds[cell.Kind] = struct{}{}
	}
	for kind := templates.CellPlain; kind <= templates.CellMsg; kind++ {
		if _, ok := seenKinds[kind]; !ok {
			t.Fatalf("Live render contract is missing CellKind %d", kind)
		}
	}
	rows := []struct {
		name  string
		kind  string
		cells []templates.TableCell
	}{
		{
			name: "pod-rich",
			kind: "Pods",
			cells: []templates.TableCell{
				{Kind: templates.CellName, Value: "api-7c8d9", NameHead: "api", NameTail: "-7c8d9", Href: "/clusters/test/namespaces/default/pods/api-7c8d9"},
				{Kind: templates.CellReady, Value: "2/3", Ratio: "partial"},
				{Kind: templates.CellStatus, Value: "Running", Tone: "ok"},
				{Kind: templates.CellRestarts, Value: "2", Tone: "warn", Ago: "(1m ago)"},
			},
		},
		{
			name: "node-capacity-conditions",
			kind: "Nodes",
			cells: []templates.TableCell{
				{Kind: templates.CellName, Value: "node-a", NameHead: "node-a", Href: "/clusters/test/nodes/node-a"},
				{Kind: templates.CellCapacity, Value: "67%", CapBar: true, CapBucket: "mid", CapPct: 67},
				{Kind: templates.CellConditions, Conds: []templates.Cond{{Name: "DiskPressure", Tone: "warn"}, {Name: "Ready", Tone: "ok"}}},
			},
		},
		{
			name: "event-unknown-kind",
			kind: "Events",
			cells: []templates.TableCell{
				{Kind: templates.CellName, Value: "widget.0001", NameHead: "widget.0001", Href: "/clusters/test/namespaces/default/events/widget.0001"},
				{Kind: templates.CellEvObj, EvKind: "Widget", EvName: "sample", Title: "Widget/sample", CellIcon: string(icons.KindIcon("Widget", "", false, ""))},
				{Kind: templates.CellEvAge, Value: "1s", Class: "age-new", EvAgeRest: "first 2s"},
				{Kind: templates.CellMsg, Value: "Observed a previously unknown object kind"},
			},
		},
		{
			name: "crd-icon",
			kind: "Custom Resources",
			cells: []templates.TableCell{
				{Kind: templates.CellName, Value: "app-sync", NameHead: "app-sync", Href: "/clusters/test/namespaces/default/kustomizations/app-sync"},
				{Kind: templates.CellEvObj, EvKind: "Kustomization", EvName: "app-sync", CellIcon: string(icons.KindIcon("Kustomization", "kustomize.toolkit.fluxcd.io", true, ""))},
			},
		},
		{name: "all-cell-kinds", kind: "Contract", cells: allKinds},
	}

	contract := liveRenderContract{Version: 1}
	for index, fixture := range rows {
		key := "test/default/" + fixture.name
		columns := make([]templates.TableColumn, len(fixture.cells))
		for cellIndex := range columns {
			columns[cellIndex] = templates.TableColumn{Name: "column-" + strconv.Itoa(cellIndex)}
		}
		row := templates.TableRow{
			StatusClass:  "ok",
			Key:          key,
			DomID:        rowDomID(key),
			Name:         fixture.name,
			OpenHref:     "/clusters/test/namespaces/default/contract/" + fixture.name,
			YAMLHref:     "/clusters/test/namespaces/default/contract/" + fixture.name + "?view=yaml",
			DownloadHref: "/clusters/test/namespaces/default/contract/" + fixture.name + "?download=yaml",
			Cells:        fixture.cells,
			CreatedText:  "2026-06-04 11:59:00",
		}
		table := templates.TableData{
			Kind:        fixture.kind,
			Count:       1,
			ColumnCount: len(columns),
			Columns:     columns,
			Rows:        []templates.TableRow{row},
		}
		data := templates.ListData{
			Plural:          "contract",
			ClusterCount:    1,
			TableCount:      1,
			TotalRows:       len(rows),
			DurationSeconds: 0.012 + float64(index)/1000,
			Tables:          []templates.TableData{table},
		}
		contract.Rows = append(contract.Rows, liveRenderContractRow{
			Name: fixture.name,
			Key:  key,
			Row:  renderLiveComponent(t, templates.LiveTableRow(data, table, row)),
			Card: renderLiveComponent(t, templates.LiveResourceCard(data, table, row)),
		})
	}

	regionTable := templates.TableData{
		Kind:      "Pods",
		Count:     len(rows),
		Phase:     []templates.PhaseChip{{Tone: "ok", Label: "Running", Count: "4"}, {Tone: "warn", Label: "Pending", Count: "1"}},
		PhaseRows: len(rows),
	}
	regionData := templates.ListData{
		ClusterCount:    1,
		TableCount:      1,
		TotalRows:       len(rows),
		DurationSeconds: 0.019,
		Tables:          []templates.TableData{regionTable},
	}
	contract.Regions = []liveRenderContractRegion{
		{Region: "count", HTML: renderLiveComponent(t, templates.LiveCountRegion(regionTable))},
		{Region: "phase", HTML: renderLiveComponent(t, templates.LivePhaseRegion(regionData, regionTable))},
		{Region: "found", HTML: renderLiveComponent(t, templates.LiveFoundRegion(regionData))},
	}
	return contract
}

func allLiveContractCellKinds() []templates.TableCell {
	return []templates.TableCell{
		{Kind: templates.CellPlain, Value: "plain", Class: "mono"},
		{Kind: templates.CellName, Value: "all-kinds", NameHead: "all-kinds", Href: "/clusters/test/namespaces/default/contract/all-kinds"},
		{Kind: templates.CellLabel, Value: "default", Href: "/clusters/test/namespaces/default"},
		{Kind: templates.CellNode, Value: "node-a", Href: "/clusters/test/nodes/node-a"},
		{Kind: templates.CellCPU, Value: "125m"},
		{Kind: templates.CellMemory, Value: "512"},
		{Kind: templates.CellStatus, Value: "Running", Tone: "ok", Pulse: true},
		{Kind: templates.CellReady, Value: "2/3", Ratio: "partial"},
		{Kind: templates.CellRestarts, Value: "3", Tone: "warn", Ago: "(1m ago)"},
		{Kind: templates.CellCapacity, Value: "73%", CapBar: true, CapBucket: "hi", CapPct: 73},
		{Kind: templates.CellRoles, Roles: []string{"control-plane", "worker"}},
		{Kind: templates.CellConditions, Conds: []templates.Cond{{Name: "MemoryPressure", Tone: "warn"}}},
		{Kind: templates.CellReplicas, RepSegments: []templates.RepSegment{{}, {State: "updating"}, {State: "pending"}}, RepNum: "1/3", Ratio: "partial"},
		{Kind: templates.CellRollout, Value: "Progressing", RolloutState: "prog", RolloutIcon: icon("sync")},
		{Kind: templates.CellChips, Chips: []templates.RowChip{{Key: "app", Val: "readout"}, {Key: "tier", Val: "web"}, {Key: "track", Val: "canary"}}},
		{Kind: templates.CellPending, Value: "Waiting", Tone: "warn", Pulse: true},
		{Kind: templates.CellPorts, Value: "80/TCP", More: "+2", Title: "80/TCP, 443/TCP, 9090/TCP"},
		{Kind: templates.CellHosts, Value: "readout.test", More: "+1 hosts", Title: "readout.test, api.readout.test"},
		{Kind: templates.CellTLS, Value: "readout-tls", CellIcon: icon("lock")},
		{Kind: templates.CellLastRun, Value: "2m ago", Class: "age-warm"},
		{Kind: templates.CellKeys, Keys: []templates.KeyChip{{Name: "config.yaml", Size: "2 KiB"}, {Name: "token", Size: "32 B"}}},
		{Kind: templates.CellCount, Value: "7", Class: "count-warn"},
		{Kind: templates.CellEvObj, EvKind: "Widget", EvName: "sample", CellIcon: string(icons.KindIcon("Widget", "", false, ""))},
		{Kind: templates.CellEvAge, Value: "3s", Class: "age-new", EvAgeRest: "first 5s"},
		{Kind: templates.CellMsg, Value: "all CellKind branches rendered"},
	}
}

func marshalLiveRenderContract(t *testing.T, contract liveRenderContract) []byte {
	t.Helper()
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(contract); err != nil {
		t.Fatalf("encode Live render contract: %v", err)
	}
	return buf.Bytes()
}

func renderLiveComponent(t *testing.T, component templ.Component) string {
	t.Helper()
	var buf bytes.Buffer
	if err := component.Render(context.Background(), &buf); err != nil {
		t.Fatalf("render Live component: %v", err)
	}
	return buf.String()
}
