package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kbelokon/readout/internal/config"
	fakeapi "github.com/kbelokon/readout/internal/fakekube"
	"github.com/kbelokon/readout/internal/kube"
	"github.com/kbelokon/readout/internal/web/templates"
)

type testLiveEnvelope struct {
	V        int                  `json:"v"`
	Kind     string               `json:"kind"`
	G        string               `json:"g"`
	Seq      uint64               `json:"seq"`
	Rev      string               `json:"rev"`
	RV       string               `json:"rv"`
	Schema   string               `json:"schema"`
	Snapshot *streamLiveSnapshot  `json:"snapshot"`
	Delta    *liveProjectionDelta `json:"delta"`
	Reason   string               `json:"reason"`
}

func newLiveV2TestSession(renderers streamLiveRenderers) *streamSession {
	tuning := defaultStreamTuning()
	tuning.checkpointInterval = 0
	tuning.checkpointDeltas = 0
	return &streamSession{
		gen:       "generation",
		lastRV:    "101",
		tuning:    tuning,
		dirty:     true,
		renderers: renderers,
	}
}

func decodePreparedEnvelope(t testing.TB, payload []byte) testLiveEnvelope {
	t.Helper()
	var envelope testLiveEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("decode prepared Live envelope: %v; payload=%.200s", err, payload)
	}
	return envelope
}

func prepareInitialLiveSnapshot(t testing.TB, st *streamSession, data *templates.ListData, now time.Time) livePreparedPush {
	t.Helper()
	prepared, err := st.prepareLiveV2Data(context.Background(), data, now)
	if err != nil {
		t.Fatalf("prepare initial Live snapshot: %v", err)
	}
	if prepared.kind != livePreparedSnapshot {
		t.Fatalf("initial prepared kind = %d, want snapshot", prepared.kind)
	}
	return prepared
}

// newStreamInterceptFixture places a deterministic fault-injection proxy in
// front of fakekube while preserving its real discovery/list/watch behavior for
// every request the interceptor does not consume.
func newStreamInterceptFixture(
	t *testing.T,
	intercept func(http.ResponseWriter, *http.Request) bool,
	tune ...func(*streamTuning),
) (*httptest.Server, *Server, *fakeapi.Server) {
	t.Helper()
	fake, err := fakeapi.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	target, err := url.Parse(fake.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if intercept != nil && intercept(w, r) {
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.Close)
	app := newTestServerWithConfig(t, &config.Config{Port: 8080, Clusters: []config.ClusterConnection{{Name: "test", Server: upstream.URL}}, DefaultTheme: "dark"})
	for _, apply := range tune {
		apply(&app.streamTuning)
	}
	ts := httptest.NewServer(app.Handler())
	t.Cleanup(ts.Close)
	return ts, app, fake
}

func openRawLiveV2(t *testing.T, ctx context.Context, baseURL, generation, query string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/clusters/test/namespaces/default/pods/_stream"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(streamVersionHeader, "2")
	req.Header.Set(streamGenerationHeader, generation)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("raw Live status = %d, body=%s", resp.StatusCode, body)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func readHeartbeatAndTerminal(t *testing.T, resp *http.Response) (bool, int, testLiveEnvelope) {
	t.Helper()
	reader := bufio.NewReader(resp.Body)
	heartbeat := false
	snapshots := 0
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("Live stream ended before terminal: %v", err)
		}
		if line == ": heartbeat\n" {
			heartbeat = true
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var envelope testLiveEnvelope
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data: "))), &envelope); err != nil {
			t.Fatalf("decode raw Live frame: %v", err)
		}
		if envelope.Kind == "snapshot" {
			snapshots++
		}
		if envelope.Kind == "terminal" {
			return heartbeat, snapshots, envelope
		}
	}
}

func waitStreamSlotRelease(t *testing.T, app *Server) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for len(app.streamSlots) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("stream cap slot remained held: %d", len(app.streamSlots))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func requireSignal(t *testing.T, signal <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func TestStreamLiveV2InitialSnapshotCarriesSchemaAndNoReason(t *testing.T) {
	defaults := defaultStreamLiveRenderers()
	fullCalls := 0
	renderers := defaults
	renderers.full = func(ctx context.Context, data *templates.ListData) (string, error) {
		fullCalls++
		return defaults.full(ctx, data)
	}
	st := newLiveV2TestSession(renderers)
	data := liveProjectionFixture(3)
	now := time.Unix(1_800_000_000, 0)
	prepared := prepareInitialLiveSnapshot(t, st, &data, now)
	if fullCalls != 1 {
		t.Fatalf("full renders = %d, want 1", fullCalls)
	}
	envelope := decodePreparedEnvelope(t, prepared.payload)
	if envelope.V != 2 || envelope.Kind != "snapshot" || envelope.Seq != 1 || envelope.Rev != prepared.projection.revision {
		t.Fatalf("initial envelope = %+v", envelope)
	}
	if envelope.Schema != liveProjectionSchemaToken(&prepared.projection) || envelope.Schema == "" {
		t.Fatalf("schema = %q, want committed projection token", envelope.Schema)
	}
	decodedSchema, err := base64.RawURLEncoding.DecodeString(envelope.Schema)
	if err != nil || len(decodedSchema) != 32 || strings.ContainsAny(envelope.Schema, "=+/") {
		t.Fatalf("schema is not a raw base64url SHA-256 token: %q (%v, %d bytes)", envelope.Schema, err, len(decodedSchema))
	}
	if envelope.Snapshot == nil || envelope.Snapshot.HTML == "" || envelope.Delta != nil || envelope.Reason != "" {
		t.Fatalf("snapshot payload is not exact: %+v", envelope)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(prepared.payload, &object); err != nil {
		t.Fatal(err)
	}
	if _, present := object["reason"]; present {
		t.Fatal("snapshot emitted forbidden reason member")
	}

	st.commitLivePush(&prepared, now)
	if st.seq != 1 || st.projection.revision != envelope.Rev || st.lastSnapshotBytes != len(prepared.payload) || st.lastSnapshotAt != now {
		t.Fatalf("snapshot commit state is inconsistent: seq=%d rev=%q bytes=%d at=%s", st.seq, st.projection.revision, st.lastSnapshotBytes, st.lastSnapshotAt)
	}
}

func TestStreamLiveV2LargeListModifyIsTinyRowOnlyDelta(t *testing.T) {
	for _, rowCount := range []int{540, 600} {
		t.Run(strconv.Itoa(rowCount), func(t *testing.T) {
			defaults := defaultStreamLiveRenderers()
			fullCalls, rowCalls, cardCalls := 0, 0, 0
			renderers := defaults
			renderers.full = func(ctx context.Context, data *templates.ListData) (string, error) {
				fullCalls++
				return defaults.full(ctx, data)
			}
			renderers.projection.row = func(ctx context.Context, data *templates.ListData, table *templates.TableData, row *templates.TableRow) (string, error) {
				rowCalls++
				return defaults.projection.row(ctx, data, table, row)
			}
			renderers.projection.card = func(ctx context.Context, data *templates.ListData, table *templates.TableData, row *templates.TableRow) (string, error) {
				cardCalls++
				return defaults.projection.card(ctx, data, table, row)
			}
			st := newLiveV2TestSession(renderers)
			before := liveProjectionFixture(rowCount)
			now := time.Unix(1_800_000_100, 0)
			initial := prepareInitialLiveSnapshot(t, st, &before, now)
			st.commitLivePush(&initial, now)

			after := cloneLiveProjectionFixture(&before)
			changed := rowCount / 2
			after.Tables[0].Rows[changed].Cells[1].Value = "Pending"
			after.Tables[0].Rows[changed].Cells[1].Tone = "warn"
			prepared, err := st.prepareLiveV2Data(context.Background(), &after, now.Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if prepared.kind != livePreparedDelta {
				t.Fatalf("prepared = %d/%s, want delta", prepared.kind, prepared.reason)
			}
			if len(prepared.payload) > 4<<10 || len(prepared.payload)*100 > len(initial.payload) {
				t.Fatalf("delta/snapshot bytes = %d/%d, want <=4KiB and <=1%%", len(prepared.payload), len(initial.payload))
			}
			if fullCalls != 1 || rowCalls != 1 || cardCalls != 0 {
				t.Fatalf("renders full/row/card = %d/%d/%d, want 1/1/0", fullCalls, rowCalls, cardCalls)
			}
			t.Logf("rows=%d delta=%d snapshot=%d ratio=%.4f%% renders full/row/card=%d/%d/%d", rowCount, len(prepared.payload), len(initial.payload), float64(len(prepared.payload))*100/float64(len(initial.payload)), fullCalls, rowCalls, cardCalls)
			envelope := decodePreparedEnvelope(t, prepared.payload)
			if envelope.Delta == nil || len(envelope.Delta.Upserts) != 1 || envelope.Delta.Upserts[0].Key != after.Tables[0].Rows[changed].Key || envelope.Delta.Upserts[0].CardHTML != "" {
				t.Fatalf("large-list delta = %+v", envelope.Delta)
			}
			if envelope.Snapshot != nil || envelope.Schema == "" || envelope.Rev != envelope.Delta.Revision {
				t.Fatalf("delta envelope mismatch: %+v", envelope)
			}
		})
	}
}

func TestStreamLiveV2SmallListDeltaIncludesCompleteCard(t *testing.T) {
	probe := newLiveProjectionRenderProbe()
	st := newLiveV2TestSession(streamLiveRenderers{projection: probe.renderers()})
	before := liveProjectionFixture(3)
	now := time.Unix(1_800_000_200, 0)
	initial := prepareInitialLiveSnapshot(t, st, &before, now)
	st.commitLivePush(&initial, now)
	st.lastSnapshotBytes = 1 << 20

	after := cloneLiveProjectionFixture(&before)
	after.Tables[0].Rows[1].StatusClass = "warn"
	prepared, err := st.prepareLiveV2Data(context.Background(), &after, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	envelope := decodePreparedEnvelope(t, prepared.payload)
	if prepared.kind != livePreparedDelta || envelope.Delta == nil || len(envelope.Delta.Upserts) != 1 || envelope.Delta.Upserts[0].RowHTML == "" || envelope.Delta.Upserts[0].CardHTML == "" {
		t.Fatalf("small-list delta = %+v", envelope.Delta)
	}
	if probe.rowCalls != 1 || probe.cardCalls != 1 {
		t.Fatalf("row/card renders = %d/%d, want 1/1", probe.rowCalls, probe.cardCalls)
	}
	st.commitLivePush(&prepared, now.Add(time.Second))
	if st.seq != 2 || st.deltasSinceSnapshot != 1 || st.projection.revision != envelope.Rev || st.dirty {
		t.Fatalf("delta commit state seq=%d deltas=%d rev=%q dirty=%t", st.seq, st.deltasSinceSnapshot, st.projection.revision, st.dirty)
	}
}

func TestStreamLiveV2WatchModifyEmitsRevisionChainedDelta(t *testing.T) {
	ts, fake := newStreamFixture(t, func(tuning *streamTuning) { tuning.heartbeat = 0 })
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/clusters/test/namespaces/default/pods/_stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(streamVersionHeader, "2")
	req.Header.Set(streamGenerationHeader, "modify")
	stream := openStreamRequest(t, req)
	initial := decodeFrame(t, stream.requireEvent(t, "ro-live", 5*time.Second))
	postStreamScript(t, fake.URL, `{"events":[`+podModifiedEvent("Error", 0)+`]}`)
	changed := decodeFrame(t, stream.requireEvent(t, "ro-live", 3*time.Second))
	if initial.Kind != "snapshot" || changed.Kind != "delta" || changed.Seq != initial.Seq+1 || changed.Schema != initial.Schema || changed.Rev == initial.Rev || changed.Delta == nil || changed.Delta.Base != initial.Rev || changed.Delta.Revision != changed.Rev {
		t.Fatalf("watch revision chain initial=%+v changed=%+v", initial, changed)
	}
	if len(changed.Delta.Upserts) != 1 || changed.Delta.Upserts[0].Key != "test/default/nginx" || !strings.Contains(changed.Delta.Upserts[0].RowHTML, "Error") || changed.Delta.Upserts[0].CardHTML == "" {
		t.Fatalf("watch upsert = %+v", changed.Delta.Upserts)
	}
}

func TestStreamLiveV2SemanticNoopDoesNotAdvanceAndNextDeltaUsesCommittedBase(t *testing.T) {
	probe := newLiveProjectionRenderProbe()
	st := newLiveV2TestSession(streamLiveRenderers{projection: probe.renderers()})
	before := liveProjectionFixture(3)
	now := time.Unix(1_800_000_300, 0)
	initial := prepareInitialLiveSnapshot(t, st, &before, now)
	st.commitLivePush(&initial, now)
	base := st.projection.revision
	st.dirty = true
	st.deletedKeys = map[string]struct{}{"cluster/default/already-filtered": {}}

	noopData := cloneLiveProjectionFixture(&before)
	noopData.DurationSeconds = 99
	noopData.ShowStaleBanner = true
	noop, err := st.prepareLiveV2Data(context.Background(), &noopData, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if noop.kind != livePreparedNoop || len(noop.payload) != 0 {
		t.Fatalf("duration/stale prepared = %d bytes=%d, want no-op", noop.kind, len(noop.payload))
	}
	noopAt := now.Add(time.Hour)
	st.commitLivePush(&noop, noopAt)
	if st.seq != 1 || st.projection.revision != base || st.lastPush != noopAt || st.dirty || st.deletedKeys != nil {
		t.Fatalf("no-op commit state: seq=%d rev=%q lastPush=%s dirty=%t deletes=%v", st.seq, st.projection.revision, st.lastPush, st.dirty, st.deletedKeys)
	}
	probe.wantNoRenders(t)

	after := cloneLiveProjectionFixture(&noopData)
	after.Tables[0].Rows[0].StatusClass = "warn"
	st.lastSnapshotBytes = 1 << 20
	next, err := st.prepareLiveV2Data(context.Background(), &after, now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	envelope := decodePreparedEnvelope(t, next.payload)
	if next.kind != livePreparedDelta || envelope.Seq != 2 || envelope.Delta == nil || envelope.Delta.Base != base {
		t.Fatalf("next delta chain = %+v, base=%q", envelope, base)
	}
}

func TestStreamLiveV2DeleteProjectAndDeleteAddClassification(t *testing.T) {
	prepareRemoval := func(t *testing.T, deleted bool) *liveProjectionDelta {
		t.Helper()
		st := newLiveV2TestSession(streamLiveRenderers{projection: newLiveProjectionRenderProbe().renderers()})
		before := liveProjectionFixture(3)
		now := time.Unix(1_800_000_400, 0)
		initial := prepareInitialLiveSnapshot(t, st, &before, now)
		st.commitLivePush(&initial, now)
		st.lastSnapshotBytes = 1 << 20
		after := cloneLiveProjectionFixture(&before)
		key := after.Tables[0].Rows[1].Key
		after.Tables[0].Rows = slices.Delete(after.Tables[0].Rows, 1, 2)
		setLiveProjectionCounts(&after)
		if deleted {
			st.deletedKeys = map[string]struct{}{key: {}}
		}
		prepared, err := st.prepareLiveV2Data(context.Background(), &after, now.Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		delta := decodePreparedEnvelope(t, prepared.payload).Delta
		if prepared.kind != livePreparedDelta || delta == nil || len(delta.Removals) != 1 || delta.Removals[0].Key != key || !slices.Equal(delta.Order, []string{before.Tables[0].Rows[0].Key, before.Tables[0].Rows[2].Key}) {
			t.Fatalf("removal delta = %+v", delta)
		}
		return delta
	}
	if cause := prepareRemoval(t, true).Removals[0].Cause; cause != liveRemovalDelete {
		t.Fatalf("tracked removal cause = %q, want delete", cause)
	}
	if cause := prepareRemoval(t, false).Removals[0].Cause; cause != liveRemovalProject {
		t.Fatalf("untracked removal cause = %q, want project", cause)
	}

	row := kube.Row{Object: map[string]any{"metadata": map[string]any{"name": "pod-001", "namespace": "default"}}}
	deleted := kube.WatchEvent{Type: kube.WatchDeleted, Table: kube.Table{Rows: []kube.Row{row}}}
	added := kube.WatchEvent{Type: kube.WatchAdded, Table: kube.Table{Rows: []kube.Row{row}}}
	key := "test/default/pod-001"
	unfiltered := newLiveV2TestSession(streamLiveRenderers{})
	unfiltered.cluster = "test"
	unfiltered.noteWatchMutation(&deleted)
	if _, present := unfiltered.deletedKeys[key]; !present {
		t.Fatal("selector-free DELETED was not classified as an actual delete")
	}
	unfiltered.noteWatchMutation(&added)
	if _, present := unfiltered.deletedKeys[key]; present {
		t.Fatal("delete→add did not clear the pending delete classification")
	}
	filtered := newLiveV2TestSession(streamLiveRenderers{})
	filtered.cluster = "test"
	filtered.selector = "app=web"
	filtered.noteWatchMutation(&deleted)
	if len(filtered.deletedKeys) != 0 {
		t.Fatal("selector-bearing ambiguous DELETED was misclassified as actual delete")
	}
}

func TestStreamLiveV2DeleteTrackingCapForcesSnapshot(t *testing.T) {
	st := newLiveV2TestSession(streamLiveRenderers{})
	st.cluster = "test"
	st.deletedKeys = make(map[string]struct{}, streamMaxDeletedKeys)
	for i := 0; i < streamMaxDeletedKeys; i++ {
		st.deletedKeys["existing/"+strconv.Itoa(i)] = struct{}{}
	}
	event := kube.WatchEvent{Type: kube.WatchDeleted, Table: kube.Table{Rows: []kube.Row{{Object: map[string]any{"metadata": map[string]any{"name": "overflow", "namespace": "default"}}}}}}
	st.noteWatchMutation(&event)
	if !st.forceSnapshot || st.deletedKeys != nil {
		t.Fatalf("delete cap state force=%t keys=%d, want forced snapshot and cleared set", st.forceSnapshot, len(st.deletedKeys))
	}
}

func TestStreamLiveV2SortSendsExactFinalOrder(t *testing.T) {
	st := newLiveV2TestSession(streamLiveRenderers{projection: newLiveProjectionRenderProbe().renderers()})
	before := liveProjectionFixture(3)
	now := time.Unix(1_800_000_500, 0)
	initial := prepareInitialLiveSnapshot(t, st, &before, now)
	st.commitLivePush(&initial, now)
	st.lastSnapshotBytes = 1 << 20
	after := cloneLiveProjectionFixture(&before)
	after.Tables[0].Rows[0], after.Tables[0].Rows[2] = after.Tables[0].Rows[2], after.Tables[0].Rows[0]
	prepared, err := st.prepareLiveV2Data(context.Background(), &after, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	delta := decodePreparedEnvelope(t, prepared.payload).Delta
	want := []string{after.Tables[0].Rows[0].Key, after.Tables[0].Rows[1].Key, after.Tables[0].Rows[2].Key}
	if prepared.kind != livePreparedDelta || delta == nil || !slices.Equal(delta.Order, want) || len(delta.Removals) != 0 || len(delta.Upserts) != 0 {
		t.Fatalf("sort delta = %+v, want order %v only", delta, want)
	}
}

func TestStreamLiveV2MetricsCellChangeIsRowDelta(t *testing.T) {
	probe := newLiveProjectionRenderProbe()
	st := newLiveV2TestSession(streamLiveRenderers{projection: probe.renderers()})
	before := liveProjectionFixture(3)
	now := time.Unix(1_800_000_600, 0)
	initial := prepareInitialLiveSnapshot(t, st, &before, now)
	st.commitLivePush(&initial, now)
	st.lastSnapshotBytes = 1 << 20
	after := cloneLiveProjectionFixture(&before)
	after.Tables[0].Rows[2].Cells = append(after.Tables[0].Rows[2].Cells, templates.TableCell{Value: "250m", ColClass: "cell-metric"})
	prepared, err := st.prepareLiveV2Data(context.Background(), &after, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	delta := decodePreparedEnvelope(t, prepared.payload).Delta
	if prepared.kind != livePreparedDelta || delta == nil || len(delta.Upserts) != 1 || delta.Upserts[0].Key != after.Tables[0].Rows[2].Key || probe.rowCalls != 1 {
		t.Fatalf("metrics delta = %+v renders=%d", delta, probe.rowCalls)
	}
}

func TestStreamLiveV2MetricsJoinSubPollEmitsOneRowDelta(t *testing.T) {
	ts, fake := newStreamFixture(t, func(tuning *streamTuning) {
		tuning.metricsPoll = 100 * time.Millisecond
		tuning.heartbeat = 0
	})
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/clusters/test/namespaces/default/pods/_stream?join=metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(streamVersionHeader, "2")
	req.Header.Set(streamGenerationHeader, "metrics")
	stream := openStreamRequest(t, req)
	initial := decodeFrame(t, stream.requireEvent(t, "ro-live", 5*time.Second))
	if initial.Kind != "snapshot" || initial.Snapshot == nil || !strings.Contains(initial.Snapshot.HTML, "nginx") {
		t.Fatalf("initial metrics snapshot is incomplete: %+v", initial)
	}
	postStreamScript(t, fake.URL, `{"events":[{"path":"/apis/metrics.k8s.io/v1beta1/namespaces/default/pods","type":"MODIFIED","object":{"kind":"PodMetrics","apiVersion":"metrics.k8s.io/v1beta1","metadata":{"name":"nginx","namespace":"default"},"containers":[{"name":"nginx","usage":{"cpu":"900m","memory":"128Mi"}}]}}]}`)

	deadline := time.Now().Add(4 * time.Second)
	for {
		frame := decodeFrame(t, stream.requireEvent(t, "ro-live", 4*time.Second))
		if frame.Kind == "delta" && frame.Delta != nil && len(frame.Delta.Upserts) == 1 && strings.Contains(frame.Delta.Upserts[0].RowHTML, "900m") {
			if frame.Delta.Upserts[0].CardHTML == "" || frame.Delta.Upserts[0].Key != "test/default/nginx" {
				t.Fatalf("metrics delta upsert = %+v", frame.Delta.Upserts[0])
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("metrics sub-poll never emitted the expected row delta: %+v", frame)
		}
	}
}

func TestStreamLiveV2GoneForcesSnapshot(t *testing.T) {
	var (
		mu             sync.Mutex
		fake           *fakeapi.Server
		goneArmed      bool
		listsAfterGone int
		applyErr       error
	)
	ts, created := newStreamFixtureWithRecorder(t, func(r *http.Request) {
		if r.URL.Query().Get("watch") == "true" || r.URL.Path != streamPodsPath {
			return
		}
		mu.Lock()
		if !goneArmed {
			mu.Unlock()
			return
		}
		goneArmed = false
		listsAfterGone++
		mu.Unlock()
		err := fake.Apply(fakeapi.ScriptEvent{
			Path:  streamPodsPath,
			Type:  "MODIFIED",
			Cells: []any{"nginx", "0/1", "Relisted", "3", "10m"},
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "Pod",
				"metadata": map[string]any{
					"name": "nginx", "namespace": "default",
				},
				"status": map[string]any{"phase": "Relisted"},
			},
		})
		mu.Lock()
		applyErr = err
		mu.Unlock()
	}, func(tuning *streamTuning) {
		tuning.heartbeat = 0
	})
	fake = created
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/clusters/test/namespaces/default/pods/_stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(streamVersionHeader, "2")
	req.Header.Set(streamGenerationHeader, "gone")
	stream := openStreamRequest(t, req)
	initial := decodeFrame(t, stream.requireEvent(t, "ro-live", 5*time.Second))
	waitForOpenWatch(t, fake.URL)
	mu.Lock()
	goneArmed = true
	mu.Unlock()
	postStreamScript(t, fake.URL, `{"events":[{"path":"`+streamPodsPath+`","type":"GONE"}]}`)
	resync := decodeFrame(t, stream.requireEvent(t, "ro-live", 3*time.Second))
	mu.Lock()
	relists, mutationErr := listsAfterGone, applyErr
	mu.Unlock()
	if mutationErr != nil || relists != 1 {
		t.Fatalf("410 relist mutation err=%v lists=%d, want nil/1", mutationErr, relists)
	}
	if initial.Kind != "snapshot" || resync.Kind != "snapshot" || resync.Seq != initial.Seq+1 || resync.Rev == initial.Rev || resync.RV == initial.RV || resync.Snapshot == nil || !strings.Contains(resync.Snapshot.HTML, "Relisted") {
		t.Fatalf("410 resync initial=%+v resync=%+v", initial, resync)
	}

	// A fresh watch starts from the relisted RV and remains live.
	postStreamScript(t, fake.URL, `{"events":[`+podModifiedEvent("Error", 0)+`]}`)
	after := decodeFrame(t, stream.requireEvent(t, "ro-live", 3*time.Second))
	if after.Kind != "delta" || after.Delta == nil || after.Delta.Base != resync.Rev || len(after.Delta.Upserts) != 1 || !strings.Contains(after.Delta.Upserts[0].RowHTML, "Error") {
		t.Fatalf("post-relist delta = %+v", after)
	}
}

func TestStreamLiveV2ProjectionBoundariesFallBackToSnapshot(t *testing.T) {
	base := liveProjectionFixture(3)
	cases := map[string]struct {
		mutate func(*templates.ListData)
		reason liveSnapshotReason
	}{
		"schema": {mutate: func(data *templates.ListData) {
			data.Tables[0].Columns = append(data.Tables[0].Columns, templates.TableColumn{Name: "Node"})
		}, reason: liveSnapshotSchema},
		"empty": {mutate: func(data *templates.ListData) {
			data.Tables[0].Rows = nil
			setLiveProjectionCounts(data)
		}, reason: liveSnapshotEmptyBoundary},
		"window": {mutate: func(data *templates.ListData) {
			for i := len(data.Tables[0].Rows); i <= 500; i++ {
				data.Tables[0].Rows = append(data.Tables[0].Rows, liveProjectionRow(i))
			}
			setLiveProjectionCounts(data)
		}, reason: liveSnapshotWindowBoundary},
		"list-state": {mutate: func(data *templates.ListData) {
			data.State.Kind = "forbidden"
			data.Tables = nil
			data.TableCount = 0
		}, reason: liveSnapshotListState},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			st := newLiveV2TestSession(streamLiveRenderers{})
			now := time.Unix(1_800_000_700, 0)
			initial := prepareInitialLiveSnapshot(t, st, &base, now)
			st.commitLivePush(&initial, now)
			after := cloneLiveProjectionFixture(&base)
			tc.mutate(&after)
			prepared, err := st.prepareLiveV2Data(context.Background(), &after, now.Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if prepared.kind != livePreparedSnapshot || prepared.reason != tc.reason || decodePreparedEnvelope(t, prepared.payload).Snapshot == nil {
				t.Fatalf("boundary prepared = %d/%s, want snapshot/%s", prepared.kind, prepared.reason, tc.reason)
			}
		})
	}
}

func TestStreamLiveV2DeltaLimitsAndRatioFallback(t *testing.T) {
	if !liveDeltaWorthSending(59, 100) || liveDeltaWorthSending(60, 100) || liveDeltaWorthSending(1, 0) {
		t.Fatal("delta ratio must be strictly less than 60% of last snapshot payload")
	}
	candidate := liveProjectionState{initialized: true, revision: "new", mode: liveProjectionWindowed}
	tooMuchHTML := &liveProjectionDelta{
		Base:     "old",
		Revision: "new",
		Upserts: []liveProjectionUpsert{
			{Key: "a", RowHTML: strings.Repeat("a", 100<<10)},
			{Key: "b", RowHTML: strings.Repeat("b", 100<<10)},
			{Key: "c", RowHTML: strings.Repeat("c", 100<<10)},
		},
	}
	if liveDeltaWireSafe(tooMuchHTML, &candidate) {
		t.Fatal("aggregate delta fragments above 256KiB were accepted")
	}

	probe := newLiveProjectionRenderProbe()
	st := newLiveV2TestSession(streamLiveRenderers{projection: probe.renderers()})
	before := liveProjectionFixture(3)
	now := time.Unix(1_800_000_800, 0)
	initial := prepareInitialLiveSnapshot(t, st, &before, now)
	st.commitLivePush(&initial, now)
	after := cloneLiveProjectionFixture(&before)
	after.Tables[0].Rows[0].StatusClass = "warn"
	st.lastSnapshotBytes = 1 // every nonempty delta is >=60%
	prepared, err := st.prepareLiveV2Data(context.Background(), &after, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if prepared.kind != livePreparedSnapshot || prepared.reason != liveSnapshotDeltaRatio {
		t.Fatalf("ratio fallback = %d/%s, want snapshot/delta-ratio", prepared.kind, prepared.reason)
	}
}

func TestStreamLiveV2EncodedDeltaOver256KiBFallsBackWithoutConsumingSequence(t *testing.T) {
	defaults := defaultStreamLiveRenderers()
	largeFragment := func(tag string) string {
		return "<" + tag + ">" + strings.Repeat("x", 131_050) + "</" + tag + ">"
	}
	renderers := streamLiveRenderers{
		full: defaults.full,
		projection: liveProjectionRenderers{
			row: func(context.Context, *templates.ListData, *templates.TableData, *templates.TableRow) (string, error) {
				return largeFragment("tr"), nil
			},
			card: func(context.Context, *templates.ListData, *templates.TableData, *templates.TableRow) (string, error) {
				return largeFragment("div"), nil
			},
			region: defaults.projection.region,
		},
	}
	st := newLiveV2TestSession(renderers)
	before := liveProjectionFixture(3)
	now := time.Unix(1_800_000_850, 0)
	initial := prepareInitialLiveSnapshot(t, st, &before, now)
	st.commitLivePush(&initial, now)
	st.lastSnapshotBytes = streamMaxEventBytes
	after := cloneLiveProjectionFixture(&before)
	after.Tables[0].Rows[0].StatusClass = "warn"
	prepared, err := st.prepareLiveV2Data(context.Background(), &after, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if prepared.kind != livePreparedSnapshot || prepared.reason != liveSnapshotDeltaLimit || st.seq != 1 {
		t.Fatalf("encoded-limit fallback = %d/%s seq=%d, want snapshot/delta-limit with seq still 1", prepared.kind, prepared.reason, st.seq)
	}
	envelope := decodePreparedEnvelope(t, prepared.payload)
	if envelope.Seq != 2 || envelope.Snapshot == nil || envelope.Delta != nil {
		t.Fatalf("fallback envelope = %+v", envelope)
	}
}

func TestStreamLiveV2CheckpointCountAndTime(t *testing.T) {
	base := liveProjectionFixture(3)
	now := time.Unix(1_800_000_900, 0)
	for name, configure := range map[string]func(*streamSession){
		"count": func(st *streamSession) {
			st.tuning.checkpointDeltas = 2
			st.deltasSinceSnapshot = 1 // the second would-be delta checkpoints
		},
		"time": func(st *streamSession) {
			st.tuning.checkpointInterval = time.Minute
			st.lastSnapshotAt = now.Add(-time.Minute)
		},
	} {
		t.Run(name, func(t *testing.T) {
			st := newLiveV2TestSession(streamLiveRenderers{})
			initial := prepareInitialLiveSnapshot(t, st, &base, now)
			st.commitLivePush(&initial, now)
			configure(st)
			after := cloneLiveProjectionFixture(&base)
			after.Tables[0].Rows[0].StatusClass = "warn"
			prepared, err := st.prepareLiveV2Data(context.Background(), &after, now.Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if prepared.kind != livePreparedSnapshot || prepared.reason != liveSnapshotCheckpoint {
				t.Fatalf("checkpoint prepared = %d/%s", prepared.kind, prepared.reason)
			}
		})
	}
}

func TestStreamLiveV2QuietCheckpointDoesNotResetIdle(t *testing.T) {
	ts, _ := newStreamFixture(t, func(tuning *streamTuning) {
		tuning.checkpointInterval = 50 * time.Millisecond
		tuning.idleCap = 850 * time.Millisecond
		tuning.heartbeat = 0
	})
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/clusters/test/namespaces/default/pods/_stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(streamVersionHeader, "2")
	req.Header.Set(streamGenerationHeader, "checkpoint")
	stream := openStreamRequest(t, req)
	started := time.Now()
	first := decodeFrame(t, stream.requireEvent(t, "ro-live", 5*time.Second))
	checkpoint := decodeFrame(t, stream.requireEvent(t, "ro-live", time.Second))
	if first.Kind != "snapshot" || checkpoint.Kind != "snapshot" || checkpoint.Seq != 2 {
		t.Fatalf("quiet checkpoint frames = %+v then %+v", first, checkpoint)
	}
	for {
		frame := decodeFrame(t, stream.requireEvent(t, "ro-live", 2*time.Second))
		if frame.Kind != "terminal" {
			continue
		}
		if frame.Reason != "idle" || time.Since(started) > 1400*time.Millisecond {
			t.Fatalf("idle after checkpoints reason=%q elapsed=%s", frame.Reason, time.Since(started))
		}
		break
	}
}

func TestStreamHangingWatchOpenKeepsHeartbeatLifetimeAndCapLive(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	var startOnce, cancelOnce sync.Once
	var opens atomic.Int32
	ts, app, _ := newStreamInterceptFixture(t, func(_ http.ResponseWriter, r *http.Request) bool {
		if r.URL.Query().Get("watch") != "true" {
			return false
		}
		opens.Add(1)
		startOnce.Do(func() { close(started) })
		<-r.Context().Done()
		cancelOnce.Do(func() { close(canceled) })
		return true
	}, func(tuning *streamTuning) {
		tuning.heartbeat = 20 * time.Millisecond
		tuning.maxLifetime = 550 * time.Millisecond
		tuning.idleCap = 5 * time.Second
		tuning.checkpointInterval = 50 * time.Millisecond
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp := openRawLiveV2(t, ctx, ts.URL, "hang-watch", "")
	requireSignal(t, started, "hanging watch open")
	startedAt := time.Now()
	heartbeat, snapshots, terminal := readHeartbeatAndTerminal(t, resp)
	if !heartbeat || snapshots < 2 || terminal.Reason != "idle" || time.Since(startedAt) > time.Second {
		t.Fatalf("hanging watch liveness heartbeat=%t snapshots=%d terminal=%+v elapsed=%s", heartbeat, snapshots, terminal, time.Since(startedAt))
	}
	requireSignal(t, canceled, "watch-open context cancellation")
	waitStreamSlotRelease(t, app)
	if got := opens.Load(); got != 1 {
		t.Fatalf("concurrent/repeated watch opens = %d, want exactly 1", got)
	}
}

func TestStreamHangingWatchOpenClientCancelReleasesCap(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	var startOnce, cancelOnce sync.Once
	ts, app, _ := newStreamInterceptFixture(t, func(_ http.ResponseWriter, r *http.Request) bool {
		if r.URL.Query().Get("watch") != "true" {
			return false
		}
		startOnce.Do(func() { close(started) })
		<-r.Context().Done()
		cancelOnce.Do(func() { close(canceled) })
		return true
	}, func(tuning *streamTuning) {
		tuning.heartbeat = 0
		tuning.maxLifetime = time.Hour
		tuning.idleCap = time.Hour
		tuning.checkpointInterval = 0
	})
	ctx, cancel := context.WithCancel(context.Background())
	resp := openRawLiveV2(t, ctx, ts.URL, "cancel-watch", "")
	requireSignal(t, started, "hanging watch open")
	cancel()
	_ = resp.Body.Close()
	requireSignal(t, canceled, "client-canceled watch open")
	waitStreamSlotRelease(t, app)
}

func TestStreamHangingRelistKeepsHeartbeatLifetimeAndCapLive(t *testing.T) {
	relistStarted := make(chan struct{})
	relistCanceled := make(chan struct{})
	var startOnce, cancelOnce sync.Once
	var lists, watches atomic.Int32
	ts, app, _ := newStreamInterceptFixture(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != streamPodsPath {
			return false
		}
		if r.URL.Query().Get("watch") == "true" {
			watches.Add(1)
			http.Error(w, "expired", http.StatusGone)
			return true
		}
		if lists.Add(1) == 1 {
			return false // initial pre-handshake LIST
		}
		startOnce.Do(func() { close(relistStarted) })
		<-r.Context().Done()
		cancelOnce.Do(func() { close(relistCanceled) })
		return true
	}, func(tuning *streamTuning) {
		tuning.heartbeat = 20 * time.Millisecond
		tuning.maxLifetime = 5 * time.Second
		tuning.idleCap = 250 * time.Millisecond
		tuning.checkpointInterval = 0
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp := openRawLiveV2(t, ctx, ts.URL, "hang-relist", "")
	requireSignal(t, relistStarted, "hanging 410 relist")
	heartbeat, _, terminal := readHeartbeatAndTerminal(t, resp)
	if !heartbeat || terminal.Reason != "idle" {
		t.Fatalf("hanging relist liveness heartbeat=%t terminal=%+v", heartbeat, terminal)
	}
	requireSignal(t, relistCanceled, "relist context cancellation")
	waitStreamSlotRelease(t, app)
	if got := watches.Load(); got != 1 {
		t.Fatalf("watch opens during stalled relist = %d, want 1", got)
	}
	if got := lists.Load(); got != 2 {
		t.Fatalf("pods LIST calls = %d, want initial + one relist", got)
	}
}

func TestStreamHangingMetricsRefreshKeepsHeartbeatLifetimeAndCapLive(t *testing.T) {
	refreshStarted := make(chan struct{})
	refreshCanceled := make(chan struct{})
	var startOnce, cancelOnce sync.Once
	var metricsCalls atomic.Int32
	ts, app, _ := newStreamInterceptFixture(t, func(_ http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/apis/metrics.k8s.io/v1beta1/namespaces/default/pods" {
			return false
		}
		if metricsCalls.Add(1) == 1 {
			return false // bounded pre-handshake metrics fetch
		}
		startOnce.Do(func() { close(refreshStarted) })
		<-r.Context().Done()
		cancelOnce.Do(func() { close(refreshCanceled) })
		return true
	}, func(tuning *streamTuning) {
		tuning.metricsPoll = 30 * time.Millisecond
		tuning.metricsRequestTimeout = 50 * time.Millisecond
		tuning.heartbeat = 20 * time.Millisecond
		tuning.maxLifetime = 300 * time.Millisecond
		tuning.idleCap = 5 * time.Second
		tuning.checkpointInterval = 0
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp := openRawLiveV2(t, ctx, ts.URL, "hang-metrics", "?join=metrics")
	requireSignal(t, refreshStarted, "hanging metrics refresh")
	heartbeat, _, terminal := readHeartbeatAndTerminal(t, resp)
	if !heartbeat || terminal.Reason != "idle" {
		t.Fatalf("hanging metrics liveness heartbeat=%t terminal=%+v", heartbeat, terminal)
	}
	requireSignal(t, refreshCanceled, "metrics refresh context cancellation")
	waitStreamSlotRelease(t, app)
	if got := metricsCalls.Load(); got < 3 {
		t.Fatalf("metrics calls = %d, want initial plus retried bounded refreshes", got)
	}
}

func TestStreamMetricsRefreshTimeoutAllowsNextPollRecovery(t *testing.T) {
	refreshStarted := make(chan struct{})
	refreshCanceled := make(chan struct{})
	recoveryStarted := make(chan struct{})
	var startOnce, cancelOnce, recoveryOnce sync.Once
	var metricsCalls atomic.Int32
	ts, _, fake := newStreamInterceptFixture(t, func(_ http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/apis/metrics.k8s.io/v1beta1/namespaces/default/pods" {
			return false
		}
		switch call := metricsCalls.Add(1); call {
		case 1:
			return false // initial pre-handshake metrics fetch succeeds
		case 2:
			startOnce.Do(func() { close(refreshStarted) })
			<-r.Context().Done()
			cancelOnce.Do(func() { close(refreshCanceled) })
			return true
		case 3:
			recoveryOnce.Do(func() { close(recoveryStarted) })
			return false
		default:
			return false
		}
	}, func(tuning *streamTuning) {
		// Keep the recovery poll well after the timed-out attempt's empty-state
		// push so the stale-clear behavior is independently observable.
		tuning.metricsPoll = 300 * time.Millisecond
		tuning.metricsRequestTimeout = 80 * time.Millisecond
		tuning.heartbeat = 0
		tuning.maxLifetime = 5 * time.Second
		tuning.idleCap = 5 * time.Second
		tuning.checkpointInterval = 0
	})
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/clusters/test/namespaces/default/pods/_stream?join=metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(streamVersionHeader, "2")
	req.Header.Set(streamGenerationHeader, "metrics-timeout-recovery")
	stream := openStreamRequest(t, req)
	initial := decodeFrame(t, stream.requireEvent(t, "ro-live", 5*time.Second))
	if initial.Kind != "snapshot" || initial.Snapshot == nil || !strings.Contains(initial.Snapshot.HTML, "250m") {
		t.Fatalf("initial metrics frame = %+v, want snapshot", initial)
	}

	postStreamScript(t, fake.URL, `{"events":[{"path":"/apis/metrics.k8s.io/v1beta1/namespaces/default/pods","type":"MODIFIED","object":{"kind":"PodMetrics","apiVersion":"metrics.k8s.io/v1beta1","metadata":{"name":"nginx","namespace":"default"},"containers":[{"name":"nginx","usage":{"cpu":"900m","memory":"128Mi"}}]}}]}`)
	requireSignal(t, refreshStarted, "stalled metrics refresh")
	// The next ticker edge is later than this attempt's request budget, so no
	// concurrent poll can mask the timeout transition.
	requireSignal(t, refreshCanceled, "stalled metrics refresh timeout")
	if got := metricsCalls.Load(); got != 2 {
		t.Fatalf("metrics calls during one in-flight refresh = %d, want 2", got)
	}
	cleared := decodeFrame(t, stream.requireEvent(t, "ro-live", 3*time.Second))
	if cleared.Kind != "delta" || cleared.Delta == nil || strings.Contains(cleared.HTML, "250m") || strings.Contains(cleared.HTML, "900m") {
		t.Fatalf("metrics timeout frame = %+v, want a stale-value clearing delta", cleared)
	}
	requireSignal(t, recoveryStarted, "next metrics poll after timeout")

	frame := decodeFrame(t, stream.requireEvent(t, "ro-live", 3*time.Second))
	if frame.Kind != "delta" || frame.Delta == nil || len(frame.Delta.Upserts) != 1 || !strings.Contains(frame.Delta.Upserts[0].RowHTML, "900m") {
		t.Fatalf("metrics recovery frame = %+v, want one-row 900m delta", frame)
	}
}

func TestStreamInitialMetricsFetchHasBoundedCapHold(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	var startOnce, cancelOnce sync.Once
	ts, app, _ := newStreamInterceptFixture(t, func(_ http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/apis/metrics.k8s.io/v1beta1/namespaces/default/pods" {
			return false
		}
		startOnce.Do(func() { close(started) })
		<-r.Context().Done()
		cancelOnce.Do(func() { close(canceled) })
		return true
	}, func(tuning *streamTuning) {
		tuning.initialMetricsTimeout = 50 * time.Millisecond
		tuning.heartbeat = 0
		tuning.checkpointInterval = 0
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	startedAt := time.Now()
	resp := openRawLiveV2(t, ctx, ts.URL, "initial-metrics", "?join=metrics")
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("initial metrics timeout held the cap/handshake for %s", elapsed)
	}
	requireSignal(t, started, "initial metrics fetch")
	requireSignal(t, canceled, "initial metrics timeout cancellation")
	_ = resp.Body.Close()
	waitStreamSlotRelease(t, app)
}

func TestStreamHandshakeDiscoveryTimeoutReturns502AndReleasesCap(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	var startOnce, cancelOnce sync.Once
	var requests atomic.Int32
	ts, app, _ := newStreamInterceptFixture(t, func(_ http.ResponseWriter, r *http.Request) bool {
		// The first Kubernetes request belongs to FindResource discovery. Keep
		// the assertion path-agnostic so client-go may change discovery ordering
		// without weakening the response-header stall.
		if requests.Add(1) != 1 {
			return false
		}
		startOnce.Do(func() { close(started) })
		<-r.Context().Done()
		cancelOnce.Do(func() { close(canceled) })
		return true
	}, func(tuning *streamTuning) {
		tuning.handshakeTimeout = 50 * time.Millisecond
	})
	startedAt := time.Now()
	resp := dialStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream", "discovery-timeout")
	defer func() { _ = resp.Body.Close() }()
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("discovery timeout held stream cap for %s", elapsed)
	}
	assertStreamPreHandshakeResponse(t, resp)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("discovery timeout status = %d, want 502", resp.StatusCode)
	}
	requireSignal(t, started, "stalled discovery request")
	requireSignal(t, canceled, "discovery timeout cancellation")
	waitStreamSlotRelease(t, app)
}

func TestStreamHandshakeInitialListTimeoutReturns502AndReleasesCap(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	var startOnce, cancelOnce sync.Once
	ts, app, _ := newStreamInterceptFixture(t, func(_ http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != streamPodsPath || r.URL.Query().Get("watch") == "true" {
			return false
		}
		startOnce.Do(func() { close(started) })
		<-r.Context().Done()
		cancelOnce.Do(func() { close(canceled) })
		return true
	}, func(tuning *streamTuning) {
		tuning.handshakeTimeout = 50 * time.Millisecond
	})
	startedAt := time.Now()
	resp := dialStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream", "list-timeout")
	defer func() { _ = resp.Body.Close() }()
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("initial LIST timeout held stream cap for %s", elapsed)
	}
	assertStreamPreHandshakeResponse(t, resp)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("initial LIST timeout status = %d, want 502", resp.StatusCode)
	}
	requireSignal(t, started, "stalled initial LIST")
	requireSignal(t, canceled, "initial LIST timeout cancellation")
	waitStreamSlotRelease(t, app)
}

type lateStreamWatch struct {
	closed chan struct{}
	once   sync.Once
}

func (w *lateStreamWatch) Next() (kube.WatchEvent, error) {
	<-w.closed
	return kube.WatchEvent{}, io.EOF
}

func (w *lateStreamWatch) Close() error {
	w.once.Do(func() { close(w.closed) })
	return nil
}

func TestStreamAsyncUpstreamLateResultsDoNotHandoffOrLeak(t *testing.T) {
	t.Run("watch success closes", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		release := make(chan struct{})
		result := make(chan watchOpenResult)
		done := make(chan struct{})
		watch := &lateStreamWatch{closed: make(chan struct{})}
		go func() {
			openWatchAsync(ctx, func(context.Context) (streamTableWatch, error) {
				<-release // deliberately ignore cancellation and finish late
				return watch, nil
			}, result)
			close(done)
		}()
		cancel()
		close(release)
		requireSignal(t, done, "late watch opener exit")
		requireSignal(t, watch.closed, "late successful watch close")
		select {
		case <-result:
			t.Fatal("late canceled watch was handed to an absent owner")
		default:
		}
	})

	t.Run("relist result drops", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		release := make(chan struct{})
		result := make(chan streamRelistResult)
		done := make(chan struct{})
		go func() {
			relistAsync(ctx, func(context.Context) (kube.Table, error) {
				<-release
				return kube.Table{ResourceVersion: "late"}, nil
			}, result)
			close(done)
		}()
		cancel()
		close(release)
		requireSignal(t, done, "late relist exit")
		select {
		case <-result:
			t.Fatal("late canceled relist mutated the owner channel")
		default:
		}
	})

	t.Run("overlay result drops", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		release := make(chan struct{})
		result := make(chan streamOverlayResult)
		done := make(chan struct{})
		go func() {
			overlayAsync(ctx, func(context.Context) streamOverlayResult {
				<-release
				return streamOverlayResult{usage: map[string][2]float64{"late": {1, 2}}}
			}, result)
			close(done)
		}()
		cancel()
		close(release)
		requireSignal(t, done, "late overlay exit")
		select {
		case <-result:
			t.Fatal("late canceled overlay refresh mutated the owner channel")
		default:
		}
	})
}

type liveFaultWriter struct {
	header   http.Header
	buffer   bytes.Buffer
	writeN   int
	writeErr error
	flushErr error
	flushes  int
}

func (w *liveFaultWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *liveFaultWriter) WriteHeader(int) {}

func (w *liveFaultWriter) Write(p []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	n := len(p)
	if w.writeN >= 0 && w.writeN < n {
		n = w.writeN
	}
	_, _ = w.buffer.Write(p[:n])
	return n, nil
}

func (w *liveFaultWriter) FlushError() error {
	w.flushes++
	return w.flushErr
}

func (w *liveFaultWriter) SetWriteDeadline(time.Time) error { return nil }

func TestStreamLiveV2WriteAndFlushAreTransactional(t *testing.T) {
	data := liveProjectionFixture(3)
	now := time.Unix(1_800_001_000, 0)
	writeBoom := errors.New("write failed")
	flushBoom := errors.New("flush failed")
	cases := []struct {
		name    string
		writer  *liveFaultWriter
		wantErr error
	}{
		{name: "write", writer: &liveFaultWriter{writeN: -1, writeErr: writeBoom}, wantErr: writeBoom},
		{name: "short", writer: &liveFaultWriter{writeN: 7}, wantErr: io.ErrShortWrite},
		{name: "flush", writer: &liveFaultWriter{writeN: -1, flushErr: flushBoom}, wantErr: flushBoom},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newLiveV2TestSession(streamLiveRenderers{})
			st.w = tc.writer
			st.rc = http.NewResponseController(tc.writer)
			st.deletedKeys = map[string]struct{}{"pending": {}}
			prepared := prepareInitialLiveSnapshot(t, st, &data, now)
			err := st.pushPreparedLiveV2(&prepared)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("write error = %v, want %v", err, tc.wantErr)
			}
			if st.seq != 0 || st.projection.initialized || !st.dirty || len(st.deletedKeys) != 1 || st.lastSnapshotBytes != 0 || !st.lastSnapshotAt.IsZero() {
				t.Fatalf("failed frame committed state: seq=%d init=%t dirty=%t deletes=%d bytes=%d at=%s", st.seq, st.projection.initialized, st.dirty, len(st.deletedKeys), st.lastSnapshotBytes, st.lastSnapshotAt)
			}
		})
	}

	w := &liveFaultWriter{writeN: -1}
	st := newLiveV2TestSession(streamLiveRenderers{})
	st.w = w
	st.rc = http.NewResponseController(w)
	prepared := prepareInitialLiveSnapshot(t, st, &data, now)
	if err := st.pushPreparedLiveV2(&prepared); err != nil {
		t.Fatal(err)
	}
	wantWire := append([]byte("event: ro-live\ndata: "), prepared.payload...)
	wantWire = append(wantWire, '\n', '\n')
	if !bytes.Equal(w.buffer.Bytes(), wantWire) || w.flushes != 1 {
		t.Fatalf("successful wire mismatch bytes=%d/%d flushes=%d", w.buffer.Len(), len(wantWire), w.flushes)
	}
	if st.seq != 1 || !st.projection.initialized || st.lastSnapshotBytes != len(prepared.payload) {
		t.Fatalf("successful frame did not commit: seq=%d init=%t bytes=%d", st.seq, st.projection.initialized, st.lastSnapshotBytes)
	}

	// A failed delta must retain the exact acknowledged base and every pending
	// classification/counter just as strictly as a failed initial snapshot.
	deltaSession := newLiveV2TestSession(streamLiveRenderers{projection: newLiveProjectionRenderProbe().renderers()})
	deltaInitial := prepareInitialLiveSnapshot(t, deltaSession, &data, now)
	deltaSession.commitLivePush(&deltaInitial, now)
	deltaSession.lastSnapshotBytes = 1 << 20
	baseRevision := deltaSession.projection.revision
	baseLastPush := deltaSession.lastPush
	after := cloneLiveProjectionFixture(&data)
	after.Tables[0].Rows[0].StatusClass = "warn"
	deltaSession.deletedKeys = map[string]struct{}{"pending": {}}
	deltaSession.dirty = true
	deltaPrepared, err := deltaSession.prepareLiveV2Data(context.Background(), &after, now.Add(time.Second))
	if err != nil || deltaPrepared.kind != livePreparedDelta {
		t.Fatalf("prepare transactional delta = %d, %v", deltaPrepared.kind, err)
	}
	deltaWriter := &liveFaultWriter{writeN: -1, flushErr: flushBoom}
	deltaSession.w = deltaWriter
	deltaSession.rc = http.NewResponseController(deltaWriter)
	if err := deltaSession.pushPreparedLiveV2(&deltaPrepared); !errors.Is(err, flushBoom) {
		t.Fatalf("delta flush error = %v, want %v", err, flushBoom)
	}
	if deltaSession.seq != 1 || deltaSession.projection.revision != baseRevision || deltaSession.deltasSinceSnapshot != 0 || deltaSession.lastPush != baseLastPush || !deltaSession.dirty || len(deltaSession.deletedKeys) != 1 {
		t.Fatalf("failed delta committed state: seq=%d projection=%q deltas=%d lastPush=%s dirty=%t deletes=%d", deltaSession.seq, deltaSession.projection.revision, deltaSession.deltasSinceSnapshot, deltaSession.lastPush, deltaSession.dirty, len(deltaSession.deletedKeys))
	}
}

func TestStreamLiveV2OversizedSnapshotFailsBeforeCommit(t *testing.T) {
	st := newLiveV2TestSession(streamLiveRenderers{
		full: func(context.Context, *templates.ListData) (string, error) {
			return strings.Repeat("x", streamMaxEventBytes), nil
		},
	})
	data := liveProjectionFixture(1)
	prepared, err := st.prepareLiveV2Data(context.Background(), &data, time.Now())
	if !errors.Is(err, errStreamEventTooLarge) {
		t.Fatalf("oversized snapshot error = %v, want errStreamEventTooLarge (prepared=%d bytes)", err, len(prepared.payload))
	}
	if st.seq != 0 || st.projection.initialized || st.lastSnapshotBytes != 0 {
		t.Fatalf("oversized snapshot mutated session: seq=%d init=%t bytes=%d", st.seq, st.projection.initialized, st.lastSnapshotBytes)
	}
}

func TestStreamLiveV2TerminalUsesCommittedSchemaAndSanitizesRV(t *testing.T) {
	data := liveProjectionFixture(2)
	now := time.Unix(1_800_001_100, 0)
	w := &liveFaultWriter{writeN: -1}
	st := newLiveV2TestSession(streamLiveRenderers{})
	initial := prepareInitialLiveSnapshot(t, st, &data, now)
	st.commitLivePush(&initial, now)
	st.lastRV = strings.Repeat("r", streamMaxResourceVersionBytes+1)
	st.w = w
	st.rc = http.NewResponseController(w)
	st.terminalLiveV2("idle")
	if st.seq != 2 {
		t.Fatalf("terminal seq = %d, want 2", st.seq)
	}
	wire := w.buffer.String()
	start := strings.Index(wire, "data: ")
	if start < 0 {
		t.Fatalf("terminal wire = %q", wire)
	}
	dataLine := strings.TrimSpace(wire[start+len("data: "):])
	var envelope testLiveEnvelope
	if err := json.Unmarshal([]byte(dataLine), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Kind != "terminal" || envelope.Reason != "idle" || envelope.Rev != initial.projection.revision || envelope.Schema != liveProjectionSchemaToken(&initial.projection) || envelope.RV != "" || envelope.Snapshot != nil || envelope.Delta != nil {
		t.Fatalf("terminal envelope = %+v", envelope)
	}

	failed := &liveFaultWriter{writeN: -1, writeErr: errors.New("closed")}
	st.w = failed
	st.rc = http.NewResponseController(failed)
	st.terminalLiveV2("shutdown")
	if st.seq != 2 {
		t.Fatalf("failed terminal advanced seq to %d", st.seq)
	}
}

func TestStreamLiveV2ClientCaps(t *testing.T) {
	if got := liveWireResourceVersion(strings.Repeat("r", streamMaxResourceVersionBytes)); got == "" {
		t.Fatal("exact resourceVersion boundary was omitted")
	}
	for _, rv := range []string{strings.Repeat("r", streamMaxResourceVersionBytes+1), "bad\nrv"} {
		if got := liveWireResourceVersion(rv); got != "" {
			t.Fatalf("invalid resourceVersion survived: %q", got)
		}
	}

	key := strings.Repeat("k", streamMaxDeltaKeyBytes)
	candidate := liveProjectionState{initialized: true, revision: "new", mode: liveProjectionCards, order: []string{key}}
	delta := &liveProjectionDelta{Base: "old", Revision: "new", Upserts: []liveProjectionUpsert{{Key: key, RowHTML: "<tr></tr>", CardHTML: "<div></div>"}}}
	if !liveDeltaWireSafe(delta, &candidate) {
		t.Fatal("exact key boundary was rejected")
	}
	delta.Upserts[0].Key += "x"
	if liveDeltaWireSafe(delta, &candidate) {
		t.Fatal("overlong key was accepted")
	}

	order := make([]string, streamMaxDeltaOperations)
	for i := range order {
		order[i] = strings.Repeat("k", i%5+1) + string(rune(i+0x1000))
	}
	candidate = liveProjectionState{initialized: true, revision: "new", mode: liveProjectionWindowed, order: slices.Clone(order)}
	delta = &liveProjectionDelta{Base: "old", Revision: "new", Order: order}
	if !liveDeltaWireSafe(delta, &candidate) {
		t.Fatal("exact order-operation boundary was rejected")
	}
	delta.Order = append(delta.Order, "overflow")
	candidate.order = delta.Order
	if liveDeltaWireSafe(delta, &candidate) {
		t.Fatal("overlong order-operation list was accepted")
	}

	candidate = liveProjectionState{initialized: true, revision: "new", mode: liveProjectionWindowed}
	delta = &liveProjectionDelta{Base: "old", Revision: "new"}
	for i := 0; i < streamMaxDeltaOperations/2; i++ {
		delta.Removals = append(delta.Removals, liveProjectionRemoval{Key: "old/" + strconv.Itoa(i), Cause: liveRemovalDelete})
		delta.Upserts = append(delta.Upserts, liveProjectionUpsert{Key: "new/" + strconv.Itoa(i), RowHTML: "<tr></tr>"})
	}
	if !liveDeltaWireSafe(delta, &candidate) {
		t.Fatal("exact remove+upsert operation boundary was rejected")
	}
	delta.Removals = append(delta.Removals, liveProjectionRemoval{Key: "overflow", Cause: liveRemovalDelete})
	if liveDeltaWireSafe(delta, &candidate) {
		t.Fatal("overlong remove+upsert operation list was accepted")
	}
}

func TestStreamLiveV2SequenceStopsAtJavaScriptSafeInteger(t *testing.T) {
	st := newLiveV2TestSession(streamLiveRenderers{})
	st.seq = streamMaxSafeSequence - 1
	if next, err := st.nextLiveSequence(); err != nil || next != streamMaxSafeSequence {
		t.Fatalf("last safe sequence = %d, %v", next, err)
	}
	st.seq = streamMaxSafeSequence
	if next, err := st.nextLiveSequence(); next != 0 || !errors.Is(err, errStreamSequenceExhausted) {
		t.Fatalf("exhausted sequence = %d, %v", next, err)
	}
	st.dirty = true
	st.deletedKeys = map[string]struct{}{"pending": {}}
	data := liveProjectionFixture(2)
	prepared, err := st.prepareLiveV2Data(context.Background(), &data, time.Now())
	if !errors.Is(err, errStreamSequenceExhausted) || len(prepared.payload) != 0 {
		t.Fatalf("prepare after sequence exhaustion = %d bytes, %v", len(prepared.payload), err)
	}
	if st.seq != streamMaxSafeSequence || st.projection.initialized || !st.dirty || len(st.deletedKeys) != 1 {
		t.Fatalf("sequence exhaustion committed state seq=%d init=%t dirty=%t deletes=%d", st.seq, st.projection.initialized, st.dirty, len(st.deletedKeys))
	}
}

func BenchmarkPrepareLiveV2Delta600(b *testing.B) {
	st := newLiveV2TestSession(streamLiveRenderers{})
	before := liveProjectionFixture(600)
	now := time.Unix(1_800_002_000, 0)
	initial := prepareInitialLiveSnapshot(b, st, &before, now)
	st.commitLivePush(&initial, now)
	after := cloneLiveProjectionFixture(&before)
	after.Tables[0].Rows[300].StatusClass = "warn"
	b.ReportAllocs()
	b.SetBytes(int64(len(before.Tables[0].Rows)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := st.prepareLiveV2Data(context.Background(), &after, now.Add(time.Second)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPrepareLiveV2Snapshot600(b *testing.B) {
	st := newLiveV2TestSession(streamLiveRenderers{})
	data := liveProjectionFixture(600)
	st.forceSnapshot = true
	b.ReportAllocs()
	b.SetBytes(int64(len(data.Tables[0].Rows)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := st.prepareLiveV2Data(context.Background(), &data, time.Unix(1_800_002_100, 0)); err != nil {
			b.Fatal(err)
		}
	}
}
