package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kbelokon/readout/internal/web/templates"
)

func TestLiveProjectionModifyRendersExactlyOneRowAndCard(t *testing.T) {
	before := liveProjectionFixture(2)
	after := cloneLiveProjectionFixture(&before)
	after.Tables[0].Rows[1].Cells[1].Value = "Pending"
	after.Tables[0].Rows[1].Cells[1].Tone = "warn"

	previous := mustProjectLiveList(t, &before)
	current := mustProjectLiveList(t, &after)
	probe := newLiveProjectionRenderProbe()
	result, err := diffLiveList(context.Background(), previous, current, &after, nil, probe.renderers())
	if err != nil {
		t.Fatalf("diff Live list: %v", err)
	}
	if result.RequireSnapshot || result.Delta == nil {
		t.Fatalf("result = %+v, want one delta", result)
	}
	if probe.rowCalls != 1 || probe.cardCalls != 1 || len(probe.regionCalls) != 0 {
		t.Fatalf("render calls row/card/regions = %d/%d/%v, want 1/1/none", probe.rowCalls, probe.cardCalls, probe.regionCalls)
	}
	if got := result.Delta.Upserts; len(got) != 1 || got[0].Key != "cluster/default/pod-001" || got[0].RowHTML == "" || got[0].CardHTML == "" {
		t.Fatalf("upserts = %+v, want one complete pod-001 patch", got)
	}
	if len(result.Delta.Removals) != 0 || result.Delta.Order != nil || len(result.Delta.Regions) != 0 {
		t.Fatalf("unrelated delta fields populated: %+v", result.Delta)
	}
	if result.Delta.Base != previous.revision || result.Delta.Revision != current.revision {
		t.Fatalf("revision chain = %q -> %q, want %q -> %q", result.Delta.Base, result.Delta.Revision, previous.revision, current.revision)
	}
}

func TestLiveProjectionSemanticNoopEmitsNothingAndRendersNothing(t *testing.T) {
	before := liveProjectionFixture(2)
	after := cloneLiveProjectionFixture(&before)
	after.DurationSeconds = 19.75
	after.ShowStaleBanner = true

	previous := mustProjectLiveList(t, &before)
	current := mustProjectLiveList(t, &after)
	probe := newLiveProjectionRenderProbe()
	result, err := diffLiveList(context.Background(), previous, current, &after, nil, probe.renderers())
	if err != nil {
		t.Fatalf("diff Live list: %v", err)
	}
	if result.RequireSnapshot || result.Delta != nil {
		t.Fatalf("result = %+v, want no frame", result)
	}
	probe.wantNoRenders(t)
	if previous.revision != current.revision || previous.schema != current.schema {
		t.Fatal("duration/stale-only change altered semantic projection")
	}
	if result.Projection.chrome.found.duration != previous.chrome.found.duration {
		t.Fatal("duration-only no-frame advanced the last-rendered timing token")
	}
	if current.chrome.found.duration == previous.chrome.found.duration {
		t.Fatal("test duration did not cross a formatted display boundary")
	}
}

func TestLiveProjectionVolatileEventAgeTracksResourceState(t *testing.T) {
	before := liveProjectionFixture(2)
	for i := range before.Tables[0].Rows {
		before.Tables[0].Rows[i].ResourceVersion = fmt.Sprintf("10%d", i)
		before.Tables[0].Rows[i].Cells = append(before.Tables[0].Rows[i].Cells,
			templates.TableCell{Kind: templates.CellEvAge, Value: "4m", Class: "age-new", ColClass: "cell-age", EvAgeRest: "(first 1h ago)", Volatile: true})
	}
	clockTick := cloneLiveProjectionFixture(&before)
	clockTick.Tables[0].Rows[1].Cells[2].Value = "5m"
	clockTick.Tables[0].Rows[1].Cells[2].Class = "age-mid"
	clockTick.Tables[0].Rows[1].Cells[2].EvAgeRest = "(first 1h 1m ago)"

	committed := mustProjectLiveList(t, &before)
	ticked := mustProjectLiveList(t, &clockTick)
	probe := newLiveProjectionRenderProbe()
	noop, err := diffLiveList(context.Background(), committed, ticked, &clockTick, nil, probe.renderers())
	if err != nil {
		t.Fatalf("diff clock-only Event refresh: %v", err)
	}
	if noop.Delta != nil || noop.RequireSnapshot || committed.revision != ticked.revision {
		t.Fatalf("clock-only Event refresh changed projection: %+v", noop)
	}
	probe.wantNoRenders(t)

	modified := cloneLiveProjectionFixture(&clockTick)
	modified.Tables[0].Rows[1].ResourceVersion = "102"
	candidate := mustProjectLiveList(t, &modified)
	delta, err := diffLiveList(context.Background(), committed, candidate, &modified, nil, probe.renderers())
	if err != nil {
		t.Fatalf("diff modified Event resource: %v", err)
	}
	if delta.Delta == nil || len(delta.Delta.Upserts) != 1 || delta.Delta.Upserts[0].Key != modified.Tables[0].Rows[1].Key {
		t.Fatalf("modified Event delta = %+v, want exactly the changed resource", delta.Delta)
	}
}

func TestLiveProjectionSemanticDeltaRepairsBothDisplayedDurations(t *testing.T) {
	before := liveProjectionFixture(2)
	after := cloneLiveProjectionFixture(&before)
	after.DurationSeconds = 0.9876
	after.Tables[0].Rows[0].StatusClass = "warn"

	result, err := diffLiveList(
		context.Background(),
		mustProjectLiveList(t, &before),
		mustProjectLiveList(t, &after),
		&after,
		nil,
		defaultLiveProjectionRenderers(),
	)
	if err != nil {
		t.Fatalf("diff Live list: %v", err)
	}
	if result.Delta == nil || len(result.Delta.Upserts) != 1 {
		t.Fatalf("delta = %+v, want changed row", result.Delta)
	}
	wantDuration := templates.FormatListDuration(after.DurationSeconds)
	regions := make(map[liveProjectionRegion]string, len(result.Delta.Regions))
	for _, patch := range result.Delta.Regions {
		regions[patch.Region] = patch.HTML
	}
	for _, region := range []liveProjectionRegion{liveRegionPhase, liveRegionFound} {
		html := regions[region]
		if html == "" || !strings.Contains(html, wantDuration) {
			t.Fatalf("%s region = %q, want exact timing token %q", region, html, wantDuration)
		}
	}
	if len(regions) != 2 {
		t.Fatalf("regions = %v, want only phase and found", regions)
	}
}

func TestLiveProjectionHiddenPhaseDoesNotPatchTiming(t *testing.T) {
	before := liveProjectionFixture(2)
	before.Tables[0].Phase = nil
	before.Tables[0].PhaseRows = 0
	after := cloneLiveProjectionFixture(&before)
	after.DurationSeconds = 0.9876
	after.Tables[0].Rows[0].StatusClass = "warn"

	result, err := diffLiveList(
		context.Background(),
		mustProjectLiveList(t, &before),
		mustProjectLiveList(t, &after),
		&after,
		nil,
		defaultLiveProjectionRenderers(),
	)
	if err != nil {
		t.Fatalf("diff Live list: %v", err)
	}
	if result.Delta == nil || len(result.Delta.Regions) != 1 || result.Delta.Regions[0].Region != liveRegionFound {
		t.Fatalf("regions = %+v, want found only for hidden phase mount", result.Delta)
	}
}

func TestLiveProjectionHiddenPhaseCSSContract(t *testing.T) {
	data := liveProjectionFixture(2)
	data.Tables[0].Phase = nil
	data.Tables[0].PhaseRows = 17 // ignored while the timing mount is hidden
	html, err := renderLiveProjectionComponent(context.Background(), templates.LivePhaseRegion(data, data.Tables[0]))
	if err != nil {
		t.Fatalf("render hidden phase: %v", err)
	}
	if !strings.Contains(html, `class="ro-phase-strip" data-ro-live-region="phase" hidden`) {
		t.Fatalf("hidden phase mount = %q", html)
	}

	css, err := os.ReadFile(filepath.Join("..", "assets", "src", "css", "table.css"))
	if err != nil {
		t.Fatalf("read table.css: %v", err)
	}
	if !strings.Contains(string(css), ".ro-phase-strip[hidden] { display: none; }") {
		t.Fatal("table.css lacks an authored hidden override for the flex phase mount")
	}
}

func TestLiveProjectionInsertRendersOnlyNewIdentityAndSendsCompleteOrder(t *testing.T) {
	before := liveProjectionFixture(2)
	after := cloneLiveProjectionFixture(&before)
	inserted := liveProjectionRow(99)
	after.Tables[0].Rows = []templates.TableRow{after.Tables[0].Rows[0], inserted, after.Tables[0].Rows[1]}
	setLiveProjectionCounts(&after)

	previous := mustProjectLiveList(t, &before)
	current := mustProjectLiveList(t, &after)
	probe := newLiveProjectionRenderProbe()
	result, err := diffLiveList(context.Background(), previous, current, &after, nil, probe.renderers())
	if err != nil {
		t.Fatalf("diff Live list: %v", err)
	}
	if result.Delta == nil || len(result.Delta.Upserts) != 1 || result.Delta.Upserts[0].Key != inserted.Key {
		t.Fatalf("upserts = %+v, want only inserted identity", result.Delta)
	}
	wantOrder := []string{before.Tables[0].Rows[0].Key, inserted.Key, before.Tables[0].Rows[1].Key}
	if !slices.Equal(result.Delta.Order, wantOrder) {
		t.Fatalf("order = %v, want %v", result.Delta.Order, wantOrder)
	}
	if probe.rowCalls != 1 || probe.cardCalls != 1 {
		t.Fatalf("row/card renders = %d/%d, want 1/1", probe.rowCalls, probe.cardCalls)
	}
}

func TestLiveProjectionRemovalClassifiesDeleteAndProjectionExit(t *testing.T) {
	for _, tc := range []struct {
		name      string
		deleted   map[string]struct{}
		wantCause liveRemovalCause
	}{
		{name: "watch delete", deleted: map[string]struct{}{"cluster/default/pod-001": {}}, wantCause: liveRemovalDelete},
		{name: "filter exit", deleted: nil, wantCause: liveRemovalProject},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := liveProjectionFixture(3)
			after := cloneLiveProjectionFixture(&before)
			after.Tables[0].Rows = slices.Delete(after.Tables[0].Rows, 1, 2)
			setLiveProjectionCounts(&after)

			probe := newLiveProjectionRenderProbe()
			result, err := diffLiveList(
				context.Background(),
				mustProjectLiveList(t, &before),
				mustProjectLiveList(t, &after),
				&after,
				tc.deleted,
				probe.renderers(),
			)
			if err != nil {
				t.Fatalf("diff Live list: %v", err)
			}
			if result.Delta == nil || len(result.Delta.Removals) != 1 {
				t.Fatalf("removals = %+v, want one", result.Delta)
			}
			removal := result.Delta.Removals[0]
			if removal.Key != "cluster/default/pod-001" || removal.Cause != tc.wantCause {
				t.Fatalf("removal = %+v, want pod-001/%s", removal, tc.wantCause)
			}
			if len(result.Delta.Upserts) != 0 {
				t.Fatalf("upserts = %+v, want none", result.Delta.Upserts)
			}
			if !slices.Equal(result.Delta.Order, []string{"cluster/default/pod-000", "cluster/default/pod-002"}) {
				t.Fatalf("order = %v, want complete surviving order", result.Delta.Order)
			}
			if probe.rowCalls != 0 || probe.cardCalls != 0 {
				t.Fatalf("removal rendered rows/cards: %d/%d", probe.rowCalls, probe.cardCalls)
			}
		})
	}
}

func TestLiveProjectionReorderHasNoRowRenders(t *testing.T) {
	before := liveProjectionFixture(3)
	after := cloneLiveProjectionFixture(&before)
	slices.Reverse(after.Tables[0].Rows)

	probe := newLiveProjectionRenderProbe()
	result, err := diffLiveList(
		context.Background(),
		mustProjectLiveList(t, &before),
		mustProjectLiveList(t, &after),
		&after,
		nil,
		probe.renderers(),
	)
	if err != nil {
		t.Fatalf("diff Live list: %v", err)
	}
	want := []string{"cluster/default/pod-002", "cluster/default/pod-001", "cluster/default/pod-000"}
	if result.Delta == nil || !slices.Equal(result.Delta.Order, want) {
		t.Fatalf("delta = %+v, want order %v", result.Delta, want)
	}
	if len(result.Delta.Upserts) != 0 || len(result.Delta.Removals) != 0 {
		t.Fatalf("reorder produced row mutations: %+v", result.Delta)
	}
	probe.wantNoRenders(t)
}

func TestLiveProjectionClosedRegionsAreIndependent(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*templates.ListData)
		want   liveProjectionRegion
	}{
		{name: "count", mutate: func(data *templates.ListData) { data.Tables[0].Count++ }, want: liveRegionCount},
		{name: "phase", mutate: func(data *templates.ListData) { data.Tables[0].Phase[0].Count = "17" }, want: liveRegionPhase},
		{name: "found", mutate: func(data *templates.ListData) { data.TotalRows++ }, want: liveRegionFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := liveProjectionFixture(2)
			after := cloneLiveProjectionFixture(&before)
			tc.mutate(&after)
			probe := newLiveProjectionRenderProbe()
			result, err := diffLiveList(
				context.Background(),
				mustProjectLiveList(t, &before),
				mustProjectLiveList(t, &after),
				&after,
				nil,
				probe.renderers(),
			)
			if err != nil {
				t.Fatalf("diff Live list: %v", err)
			}
			if result.Delta == nil || len(result.Delta.Regions) != 1 || result.Delta.Regions[0].Region != tc.want {
				t.Fatalf("regions = %+v, want only %s", result.Delta, tc.want)
			}
			if !strings.Contains(result.Delta.Regions[0].HTML, string(tc.want)) {
				t.Fatalf("region HTML = %q, want probe marker", result.Delta.Regions[0].HTML)
			}
			if !slices.Equal(probe.regionCalls, []liveProjectionRegion{tc.want}) {
				t.Fatalf("region calls = %v, want %s", probe.regionCalls, tc.want)
			}
			if probe.rowCalls != 0 || probe.cardCalls != 0 {
				t.Fatalf("chrome-only delta rendered rows/cards: %d/%d", probe.rowCalls, probe.cardCalls)
			}
		})
	}
}

func TestLiveProjectionWindowedOffscreenModifyOmitsCard(t *testing.T) {
	before := liveProjectionFixture(600)
	after := cloneLiveProjectionFixture(&before)
	after.Tables[0].Rows[550].Cells[1].Title = "offscreen semantic change"

	probe := newLiveProjectionRenderProbe()
	result, err := diffLiveList(
		context.Background(),
		mustProjectLiveList(t, &before),
		mustProjectLiveList(t, &after),
		&after,
		nil,
		probe.renderers(),
	)
	if err != nil {
		t.Fatalf("diff Live list: %v", err)
	}
	if result.Delta == nil || len(result.Delta.Upserts) != 1 || result.Delta.Upserts[0].Key != "cluster/default/pod-550" {
		t.Fatalf("upserts = %+v, want one offscreen row", result.Delta)
	}
	if result.Delta.Upserts[0].CardHTML != "" {
		t.Fatalf("windowed upsert carried card HTML: %q", result.Delta.Upserts[0].CardHTML)
	}
	if probe.rowCalls != 1 || probe.cardCalls != 0 {
		t.Fatalf("row/card renders = %d/%d, want 1/0", probe.rowCalls, probe.cardCalls)
	}
}

func TestLiveProjectionStructuralBoundariesRequireSnapshotWithoutRendering(t *testing.T) {
	tests := []struct {
		name       string
		rows       int
		mutate     func(*templates.ListData)
		wantReason liveSnapshotReason
	}{
		{
			name: "nonempty to empty",
			rows: 1,
			mutate: func(data *templates.ListData) {
				data.Tables[0].Rows = nil
				setLiveProjectionCounts(data)
			},
			wantReason: liveSnapshotEmptyBoundary,
		},
		{
			name: "empty to nonempty",
			rows: 0,
			mutate: func(data *templates.ListData) {
				data.Tables[0].Rows = []templates.TableRow{liveProjectionRow(0)}
				setLiveProjectionCounts(data)
			},
			wantReason: liveSnapshotEmptyBoundary,
		},
		{
			name: "500 to 501",
			rows: 500,
			mutate: func(data *templates.ListData) {
				data.Tables[0].Rows = append(data.Tables[0].Rows, liveProjectionRow(500))
				setLiveProjectionCounts(data)
			},
			wantReason: liveSnapshotWindowBoundary,
		},
		{
			name: "schema",
			rows: 2,
			mutate: func(data *templates.ListData) {
				data.Tables[0].Columns[1].Name = "State"
			},
			wantReason: liveSnapshotSchema,
		},
		{
			name: "list state",
			rows: 2,
			mutate: func(data *templates.ListData) {
				data.State = templates.ListState{Kind: "forbidden", Detail: "denied"}
			},
			wantReason: liveSnapshotListState,
		},
		{
			name: "multiple tables",
			rows: 2,
			mutate: func(data *templates.ListData) {
				data.Tables = append(data.Tables, data.Tables[0])
			},
			wantReason: liveSnapshotMultiTable,
		},
		{
			name: "missing identity",
			rows: 2,
			mutate: func(data *templates.ListData) {
				data.Tables[0].Rows[1].Key = ""
			},
			wantReason: liveSnapshotIdentity,
		},
		{
			name: "duplicate identity",
			rows: 2,
			mutate: func(data *templates.ListData) {
				data.Tables[0].Rows[1].Key = data.Tables[0].Rows[0].Key
			},
			wantReason: liveSnapshotIdentity,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := liveProjectionFixture(tc.rows)
			after := cloneLiveProjectionFixture(&before)
			tc.mutate(&after)
			probe := newLiveProjectionRenderProbe()
			result, err := diffLiveList(
				context.Background(),
				mustProjectLiveList(t, &before),
				mustProjectLiveList(t, &after),
				&after,
				nil,
				probe.renderers(),
			)
			if err != nil {
				t.Fatalf("diff Live list: %v", err)
			}
			if !result.RequireSnapshot || result.Delta != nil || result.SnapshotReason != tc.wantReason {
				t.Fatalf("result = %+v, want snapshot reason %s", result, tc.wantReason)
			}
			if result.Projection.revision == "" {
				t.Fatal("snapshot result did not carry the candidate projection")
			}
			probe.wantNoRenders(t)
		})
	}
}

func TestLiveProjectionRendererErrorsAreTransactional(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*templates.ListData)
		renderers func(error) liveProjectionRenderers
		wantText  string
	}{
		{
			name: "row",
			mutate: func(data *templates.ListData) {
				data.Tables[0].Rows[0].Cells[1].Value = "changed"
			},
			renderers: func(boom error) liveProjectionRenderers {
				probe := newLiveProjectionRenderProbe().renderers()
				probe.row = func(context.Context, *templates.ListData, *templates.TableData, *templates.TableRow) (string, error) {
					return "", boom
				}
				return probe
			},
			wantText: "render Live row",
		},
		{
			name: "card after row",
			mutate: func(data *templates.ListData) {
				data.Tables[0].Rows[0].Cells[1].Value = "changed"
			},
			renderers: func(boom error) liveProjectionRenderers {
				probe := newLiveProjectionRenderProbe().renderers()
				probe.card = func(context.Context, *templates.ListData, *templates.TableData, *templates.TableRow) (string, error) {
					return "", boom
				}
				return probe
			},
			wantText: "render Live card",
		},
		{
			name: "region after row set",
			mutate: func(data *templates.ListData) {
				data.Tables[0].Count++
			},
			renderers: func(boom error) liveProjectionRenderers {
				probe := newLiveProjectionRenderProbe().renderers()
				probe.region = func(context.Context, liveProjectionRegion, *templates.ListData, *templates.TableData) (string, error) {
					return "", boom
				}
				return probe
			},
			wantText: "render Live count region",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := liveProjectionFixture(2)
			after := cloneLiveProjectionFixture(&before)
			tc.mutate(&after)
			previous := mustProjectLiveList(t, &before)
			previousRevision := previous.revision
			previousOrder := slices.Clone(previous.order)
			previousRows := cloneLiveProjectionHashes(previous.rows)
			candidate := mustProjectLiveList(t, &after)
			boom := errors.New("renderer exploded")

			result, err := diffLiveList(context.Background(), previous, candidate, &after, nil, tc.renderers(boom))
			if !errors.Is(err, boom) || !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("error = %v, want wrapped %q", err, tc.wantText)
			}
			if result.Delta != nil || result.RequireSnapshot {
				t.Fatalf("failed render returned committable output: %+v", result)
			}
			if result.Projection.initialized {
				t.Fatal("failed render leaked a candidate projection that could be committed")
			}
			if previous.revision != previousRevision || !slices.Equal(previous.order, previousOrder) || !reflect.DeepEqual(previous.rows, previousRows) {
				t.Fatal("failed diff mutated committed projection")
			}
		})
	}
}

func TestLiveProjectionHashesAreDeterministicAndPartitionSemantics(t *testing.T) {
	firstData := liveProjectionFixture(2)
	secondData := cloneLiveProjectionFixture(&firstData)
	first := mustProjectLiveList(t, &firstData)
	second := mustProjectLiveList(t, &secondData)
	if first.revision != second.revision || first.schema != second.schema || first.chrome != second.chrome || !reflect.DeepEqual(first.rows, second.rows) {
		t.Fatal("identical ListData produced different projection hashes")
	}

	secondData.DurationSeconds = 987.654
	secondData.ShowStaleBanner = true
	diagnosticsOnly := mustProjectLiveList(t, &secondData)
	if first.revision != diagnosticsOnly.revision || first.schema != diagnosticsOnly.schema || !reflect.DeepEqual(first.rows, diagnosticsOnly.rows) {
		t.Fatal("duration/stale diagnostics leaked into semantic revision or row/schema hashes")
	}
	if first.chrome == diagnosticsOnly.chrome {
		t.Fatal("formatted duration was not fingerprinted by the displayed chrome")
	}

	secondData = cloneLiveProjectionFixture(&firstData)
	secondData.Tables[0].Rows[0].CreatedTitle = "new semantic tooltip"
	rowChange := mustProjectLiveList(t, &secondData)
	if first.revision == rowChange.revision || first.rows[first.order[0]] == rowChange.rows[rowChange.order[0]] {
		t.Fatal("row semantic change did not alter revision and row hash")
	}
	if first.schema != rowChange.schema || first.rows[first.order[1]] != rowChange.rows[rowChange.order[1]] {
		t.Fatal("row semantic change leaked into schema or an unrelated row")
	}

	secondData = cloneLiveProjectionFixture(&firstData)
	secondData.Tables[0].Columns[0].Name = "Object"
	schemaChange := mustProjectLiveList(t, &secondData)
	if first.schema == schemaChange.schema {
		t.Fatal("structural template input did not alter schema hash")
	}
}

func TestLiveProjectionRevisionCoversEveryDeltaPartition(t *testing.T) {
	baseData := liveProjectionFixture(3)
	base := mustProjectLiveList(t, &baseData)
	tests := []struct {
		name   string
		mutate func(*templates.ListData)
	}{
		{name: "row", mutate: func(data *templates.ListData) { data.Tables[0].Rows[0].CreatedTitle = "changed" }},
		{name: "order", mutate: func(data *templates.ListData) { slices.Reverse(data.Tables[0].Rows) }},
		{name: "count", mutate: func(data *templates.ListData) { data.Tables[0].Count++ }},
		{name: "phase", mutate: func(data *templates.ListData) { data.Tables[0].Phase[0].Count = "99" }},
		{name: "found", mutate: func(data *templates.ListData) { data.TotalRows++ }},
		{name: "schema", mutate: func(data *templates.ListData) { data.Tables[0].Columns[0].Name = "Object" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			changedData := cloneLiveProjectionFixture(&baseData)
			tc.mutate(&changedData)
			changed := mustProjectLiveList(t, &changedData)
			if changed.revision == base.revision {
				t.Fatalf("%s semantic input did not alter Live revision", tc.name)
			}
		})
	}
}

func TestLiveProjectionUnsupportedRevisionsRemainSensitive(t *testing.T) {
	t.Run("multi-table rows", func(t *testing.T) {
		beforeData := liveProjectionFixture(2)
		beforeData.Tables = append(beforeData.Tables, cloneLiveProjectionFixture(&beforeData).Tables[0])
		afterData := cloneLiveProjectionFixture(&beforeData)
		afterData.Tables[1].Rows[1].CreatedTitle = "changed in unsupported table"
		before := mustProjectLiveList(t, &beforeData)
		after := mustProjectLiveList(t, &afterData)
		if before.boundary != liveSnapshotMultiTable || after.boundary != liveSnapshotMultiTable || before.revision == after.revision {
			t.Fatalf("multi-table revisions %q / %q, boundaries %q / %q", before.revision, after.revision, before.boundary, after.boundary)
		}
	})

	t.Run("invalid identities", func(t *testing.T) {
		beforeData := liveProjectionFixture(2)
		beforeData.Tables[0].Rows[1].Key = beforeData.Tables[0].Rows[0].Key
		afterData := cloneLiveProjectionFixture(&beforeData)
		afterData.Tables[0].Rows[1].CreatedTitle = "changed duplicate"
		before := mustProjectLiveList(t, &beforeData)
		after := mustProjectLiveList(t, &afterData)
		if before.boundary != liveSnapshotIdentity || after.boundary != liveSnapshotIdentity || before.revision == after.revision {
			t.Fatalf("identity-invalid revisions %q / %q, boundaries %q / %q", before.revision, after.revision, before.boundary, after.boundary)
		}
	})
}

func TestLiveProjectionStateDoesNotRetainRenderInputsOrHTML(t *testing.T) {
	for _, forbidden := range []reflect.Type{
		reflect.TypeFor[templates.ListData](),
		reflect.TypeFor[templates.TableData](),
		reflect.TypeFor[templates.TableRow](),
	} {
		if typeContains(reflect.TypeFor[liveProjectionState](), forbidden, map[reflect.Type]bool{}) {
			t.Fatalf("liveProjectionState retains forbidden heavy type %v", forbidden)
		}
	}
	state := mustProjectLiveList(t, ptrTo(liveProjectionFixture(2)))
	if strings.Contains(fmt.Sprintf("%v", state), "<tr") {
		t.Fatal("liveProjectionState retained rendered HTML")
	}
}

func TestDefaultLiveProjectionRenderersUseCanonicalComponents(t *testing.T) {
	before := liveProjectionFixture(2)
	after := cloneLiveProjectionFixture(&before)
	after.Tables[0].Rows[0].StatusClass = "warn"
	result, err := diffLiveList(
		context.Background(),
		mustProjectLiveList(t, &before),
		mustProjectLiveList(t, &after),
		&after,
		nil,
		defaultLiveProjectionRenderers(),
	)
	if err != nil {
		t.Fatalf("diff Live list: %v", err)
	}
	if result.Delta == nil || len(result.Delta.Upserts) != 1 {
		t.Fatalf("delta = %+v, want one upsert", result.Delta)
	}
	upsert := result.Delta.Upserts[0]
	if !strings.Contains(upsert.RowHTML, `<tr class="warn" id="row-pod-000" data-key="cluster/default/pod-000"`) {
		t.Fatalf("row HTML is not canonical LiveTableRow output: %q", upsert.RowHTML)
	}
	if !strings.Contains(upsert.CardHTML, `<div class="ro-pcard" data-key="cluster/default/pod-000"`) {
		t.Fatalf("card HTML is not canonical LiveResourceCard output: %q", upsert.CardHTML)
	}
}

func BenchmarkLiveProjectionDiff(b *testing.B) {
	for _, rows := range []int{2, 540, 600} {
		b.Run(fmt.Sprintf("rows=%d", rows), func(b *testing.B) {
			benchmarkLiveProjectionRows(b, rows, false)
		})
	}
}

func TestLiveProjection600RowAllocationBudget(t *testing.T) {
	optimized := testing.Benchmark(func(b *testing.B) {
		benchmarkLiveProjectionRows(b, 600, false)
	})
	legacy := testing.Benchmark(func(b *testing.B) {
		benchmarkLiveProjectionRows(b, 600, true)
	})
	optimizedBytes := optimized.AllocedBytesPerOp()
	legacyBytes := legacy.AllocedBytesPerOp()
	if optimizedBytes*10 > legacyBytes*3 {
		t.Fatalf("600-row projection allocates %d B/op versus %d B/op with the removed full-list marshal; want at least 70%% lower", optimizedBytes, legacyBytes)
	}
}

func benchmarkLiveProjectionRows(b *testing.B, rows int, includeRemovedFullMarshal bool) {
	data := liveProjectionFixture(rows)
	committed, err := projectLiveList(&data)
	if err != nil {
		b.Fatal(err)
	}
	probe := newLiveProjectionRenderProbe()
	renderers := probe.renderers()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		data.Tables[0].Rows[rows-1].CreatedTitle = fmt.Sprintf("revision-%d", i)
		if includeRemovedFullMarshal {
			if _, etagErr := resourceListETag(&data); etagErr != nil {
				b.Fatal(etagErr)
			}
		}
		candidate, projectErr := projectLiveList(&data)
		if projectErr != nil {
			b.Fatal(projectErr)
		}
		result, diffErr := diffLiveList(context.Background(), &committed, &candidate, &data, nil, renderers)
		if diffErr != nil {
			b.Fatal(diffErr)
		}
		if result.Delta == nil {
			b.Fatal("missing delta")
		}
		committed = result.Projection
	}
}

type liveProjectionRenderProbe struct {
	rowCalls    int
	cardCalls   int
	regionCalls []liveProjectionRegion
}

func newLiveProjectionRenderProbe() *liveProjectionRenderProbe {
	return &liveProjectionRenderProbe{}
}

func (probe *liveProjectionRenderProbe) renderers() liveProjectionRenderers {
	return liveProjectionRenderers{
		row: func(_ context.Context, _ *templates.ListData, _ *templates.TableData, row *templates.TableRow) (string, error) {
			probe.rowCalls++
			return `<tr data-key="` + row.Key + `"></tr>`, nil
		},
		card: func(_ context.Context, _ *templates.ListData, _ *templates.TableData, row *templates.TableRow) (string, error) {
			probe.cardCalls++
			return `<div data-key="` + row.Key + `"></div>`, nil
		},
		region: func(_ context.Context, region liveProjectionRegion, _ *templates.ListData, _ *templates.TableData) (string, error) {
			probe.regionCalls = append(probe.regionCalls, region)
			return `<span data-region="` + string(region) + `"></span>`, nil
		},
	}
}

func (probe *liveProjectionRenderProbe) wantNoRenders(t *testing.T) {
	t.Helper()
	if probe.rowCalls != 0 || probe.cardCalls != 0 || len(probe.regionCalls) != 0 {
		t.Fatalf("unexpected render calls row/card/regions = %d/%d/%v", probe.rowCalls, probe.cardCalls, probe.regionCalls)
	}
}

func liveProjectionFixture(rowCount int) templates.ListData {
	rows := make([]templates.TableRow, rowCount)
	for i := range rows {
		rows[i] = liveProjectionRow(i)
	}
	data := templates.ListData{
		Plural:          "pods",
		ClusterCount:    1,
		TableCount:      1,
		TotalRows:       rowCount,
		DurationSeconds: 0.012,
		Tables: []templates.TableData{{
			Kind:        "Pods",
			Count:       rowCount,
			ColumnCount: 2,
			Columns: []templates.TableColumn{
				{Name: "Name", Description: "Object name"},
				{Name: "Status", Description: "Current phase"},
			},
			Phase:     []templates.PhaseChip{{Tone: "ok", Label: "Running", Count: fmt.Sprint(rowCount)}},
			PhaseRows: rowCount,
			Rows:      rows,
		}},
	}
	return data
}

func liveProjectionRow(index int) templates.TableRow {
	name := fmt.Sprintf("pod-%03d", index)
	key := "cluster/default/" + name
	return templates.TableRow{
		StatusClass:  "ok",
		Key:          key,
		DomID:        "row-" + name,
		Name:         name,
		OpenHref:     "/pods/" + name,
		YAMLHref:     "/pods/" + name + "?view=yaml",
		DownloadHref: "/pods/" + name + "?download=yaml",
		Cells: []templates.TableCell{
			{Kind: templates.CellName, Value: name, NameHead: name, Href: "/pods/" + name},
			{Kind: templates.CellStatus, Value: "Running", Tone: "ok", ColClass: "cell-status"},
		},
		CreatedText:  "2026-08-28 12:00:00",
		CreatedTitle: "created recently",
	}
}

func setLiveProjectionCounts(data *templates.ListData) {
	rows := len(data.Tables[0].Rows)
	data.Tables[0].Count = rows
	data.Tables[0].PhaseRows = rows
	data.Tables[0].Phase[0].Count = fmt.Sprint(rows)
	data.TotalRows = rows
}

// The JSON round trip is intentional test isolation: it deep-copies every
// nested slice/pointer in this renderer contract without maintaining a second
// hand-written clone as ListData evolves.
func cloneLiveProjectionFixture(data *templates.ListData) templates.ListData {
	payload, err := json.Marshal(data)
	if err != nil {
		panic(err)
	}
	var clone templates.ListData
	if err := json.Unmarshal(payload, &clone); err != nil {
		panic(err)
	}
	return clone
}

func mustProjectLiveList(t *testing.T, data *templates.ListData) *liveProjectionState {
	t.Helper()
	projection, err := projectLiveList(data)
	if err != nil {
		t.Fatalf("project Live list: %v", err)
	}
	return &projection
}

func cloneLiveProjectionHashes(source map[string][32]byte) map[string][32]byte {
	clone := make(map[string][32]byte, len(source))
	for key, hash := range source {
		clone[key] = hash
	}
	return clone
}

func typeContains(current, forbidden reflect.Type, seen map[reflect.Type]bool) bool {
	if current == forbidden {
		return true
	}
	if seen[current] {
		return false
	}
	seen[current] = true
	switch current.Kind() {
	case reflect.Array, reflect.Pointer, reflect.Slice:
		return typeContains(current.Elem(), forbidden, seen)
	case reflect.Map:
		return typeContains(current.Key(), forbidden, seen) || typeContains(current.Elem(), forbidden, seen)
	case reflect.Struct:
		for i := 0; i < current.NumField(); i++ {
			if typeContains(current.Field(i).Type, forbidden, seen) {
				return true
			}
		}
	}
	return false
}

func ptrTo[T any](value T) *T { return &value }
