package web

// stream_test.go pins the `_stream` SSE endpoint (the Live refresh mode) against
// scripted fakeapi fixtures: the handshake + framing (generation echo), the
// coalescing pacing (floor, ceiling, churn degradation), render-time filter
// transitions over the unfiltered snapshot, the complete lifecycle (410
// relist, EOF-storm terminal, auth terminal, idle cap, server shutdown), the
// stream cap with slot release, and the metrics plumbing (histogram
// exclusion, join sub-poll). Every branch is driven end to end through the
// real middleware chain (a real httptest.Server — flushing through
// statusWriter is part of what is under test).

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kbelokon/readout/internal/auth"
	"github.com/kbelokon/readout/internal/config"
	fakeapi "github.com/kbelokon/readout/internal/fakekube"
	"github.com/kbelokon/readout/internal/kube"
)

const streamPodsPath = "/api/v1/namespaces/default/pods"

// newStreamFixture builds the standard one-cluster app over a fresh fakeapi
// and serves it on a REAL HTTP server (SSE needs live flushing, which
// httptest.NewRecorder cannot exercise).
func newStreamFixture(t *testing.T, tune ...func(*streamTuning)) (*httptest.Server, *fakeapi.Server) {
	t.Helper()
	return newStreamFixtureWithRecorder(t, nil, tune...)
}

func newStreamFixtureWithRecorder(t *testing.T, listRecorder func(*http.Request), tune ...func(*streamTuning)) (*httptest.Server, *fakeapi.Server) {
	t.Helper()
	var opts []fakeapi.Option
	if listRecorder != nil {
		opts = append(opts, fakeapi.WithListRecorder(listRecorder))
	}
	fake, err := fakeapi.New(opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	app := newTestServerWithConfig(t, &config.Config{Port: 8080, Clusters: []config.ClusterConnection{{Name: "test", Server: fake.URL}}, DefaultTheme: "dark"})
	for _, apply := range tune {
		apply(&app.streamTuning)
	}
	ts := httptest.NewServer(app.Handler())
	t.Cleanup(ts.Close)
	return ts, fake
}

// postStreamScript queues scripted watch events on the fakeapi control
// surface (the same vocabulary the kube watch tests use).
func postStreamScript(t *testing.T, baseURL, script string) {
	t.Helper()
	resp, err := http.Post(baseURL+"/__control/watch-script", "application/json", strings.NewReader(script))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("watch-script status = %d body = %s", resp.StatusCode, body)
	}
}

// podModifiedEvent builds one scripted pods MODIFIED entry with the given
// Status cell value and delay.
func podModifiedEvent(status string, delayMs int) string {
	return fmt.Sprintf(`{"path":%q,"type":"MODIFIED","delayMs":%d,"cells":["nginx","0/1",%q,"3","10m"],"object":{"apiVersion":"v1","kind":"Pod","metadata":{"name":"nginx","namespace":"default"},"status":{"phase":%q}}}`,
		streamPodsPath, delayMs, status, status)
}

type sseEvent struct {
	name string
	data string
	at   time.Time
}

// streamFrame decodes the v2 snapshot/delta/terminal envelope. HTML is a test
// convenience populated by decodeFrame from either the full snapshot or the
// fragments in a delta.
type streamFrame struct {
	V        int                  `json:"v"`
	Kind     string               `json:"kind"`
	G        string               `json:"g"`
	Seq      uint64               `json:"seq"`
	Rev      string               `json:"rev"`
	RV       string               `json:"rv"`
	Schema   string               `json:"schema"`
	HTML     string               `json:"html"`
	Reason   string               `json:"reason"`
	Delta    *liveProjectionDelta `json:"delta"`
	Snapshot *struct {
		HTML string `json:"html"`
	} `json:"snapshot"`
}

type sseStream struct {
	resp   *http.Response
	events chan sseEvent
}

// dialStream GETs a stream URL and returns the raw response (no status
// assertion — the non-200 taxonomy tests read the code directly).
func dialStream(t *testing.T, url, generation string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	setTestLiveHeaders(req, generation)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// assertStreamPreHandshakeResponse pins the cache and representation metadata
// shared by every exit before the initial snapshot commits SSE. Error writers
// may choose text/plain (and a 204 has no type), but only a successful stream
// may advertise text/event-stream.
func assertStreamPreHandshakeResponse(t *testing.T, resp *http.Response) {
	t.Helper()
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("pre-handshake status %d Cache-Control = %q, want no-store", resp.StatusCode, got)
	}
	for _, selector := range []string{streamVersionHeader, streamGenerationHeader} {
		if !headerHasToken(resp.Header.Values("Vary"), selector) {
			t.Fatalf("pre-handshake status %d Vary = %q, want %s", resp.StatusCode, resp.Header.Values("Vary"), selector)
		}
	}
	if contentType := resp.Header.Get("Content-Type"); strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		t.Fatalf("pre-handshake status %d advertised SSE Content-Type %q", resp.StatusCode, contentType)
	}
}

// openStream dials a stream URL, asserts the 200 handshake, and starts a
// background SSE parser delivering events on a channel. The body closes at
// cleanup so the test server can drain its handler.
func openStream(t *testing.T, url, generation string) *sseStream {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	setTestLiveHeaders(req, generation)
	return openStreamRequest(t, req)
}

// openStreamRequest is openStream for a fully-built request. Callers attach the
// explicit v2 negotiation headers plus any cookies or fault-specific headers.
func openStreamRequest(t *testing.T, req *http.Request) *sseStream {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("stream status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	s := &sseStream{resp: resp, events: make(chan sseEvent, 64)}
	t.Cleanup(s.close)
	go s.read()
	return s
}

func setTestLiveHeaders(req *http.Request, generation string) {
	req.Header.Set(streamVersionHeader, "2")
	req.Header.Set(streamGenerationHeader, generation)
}

// waitForOpenWatch polls the fakeapi hub snapshot until at least one
// ?watch=true connection is registered. Control entries (GONE/EOF) never
// replay to late watches, and emissions fan out to zero connections silently,
// so a test posting them right after the SSE handshake races the server's
// first watch connect — the GONE/EOF can vanish and the test hangs waiting
// for a reaction that never comes (the reproduced TestStreamGoneRelists
// flake). Data events (ADDED/MODIFIED/DELETED) are replayable and need no
// guard.
func waitForOpenWatch(t *testing.T, baseURL string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(baseURL + "/__control/watch-script")
		if err != nil {
			t.Fatal(err)
		}
		var snapshot struct {
			OpenWatches []string `json:"openWatches"`
		}
		err = json.NewDecoder(resp.Body).Decode(&snapshot)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(snapshot.OpenWatches) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no upstream watch opened within 5s")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (s *sseStream) close() { _ = s.resp.Body.Close() }

// read parses the SSE wire format: `event:`/`data:` lines accumulate until a
// blank line completes one event. The channel closes when the stream ends.
func (s *sseStream) read() {
	defer close(s.events)
	br := bufio.NewReader(s.resp.Body)
	var name, data string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if name != "" || data != "" {
				s.events <- sseEvent{name: name, data: data, at: time.Now()}
			}
			name, data = "", ""
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		}
	}
}

// requireEvent waits for the next SSE event and asserts its name.
func (s *sseStream) requireEvent(t *testing.T, name string, timeout time.Duration) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-s.events:
		if !ok {
			t.Fatalf("stream closed while waiting for %s", name)
		}
		if ev.name != name {
			t.Fatalf("event = %s (data %s), want %s", ev.name, ev.data, name)
		}
		return ev
	case <-time.After(timeout):
		t.Fatalf("no %s event within %s", name, timeout)
		return sseEvent{}
	}
}

// requireQuiet asserts NO event arrives (and the stream stays open) for d.
func (s *sseStream) requireQuiet(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case ev, ok := <-s.events:
		if !ok {
			t.Fatal("stream closed during expected quiet window")
		}
		t.Fatalf("unexpected %s event during quiet window: %.120s", ev.name, ev.data)
	case <-time.After(d):
	}
}

// requireClosed drains the stream until the server closes it.
func (s *sseStream) requireClosed(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case _, ok := <-s.events:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("stream did not close")
		}
	}
}

func decodeFrame(t *testing.T, ev sseEvent) streamFrame {
	t.Helper()
	var f streamFrame
	if err := json.Unmarshal([]byte(ev.data), &f); err != nil {
		t.Fatalf("frame %q does not decode: %v", ev.data, err)
	}
	if f.Snapshot != nil {
		f.HTML = f.Snapshot.HTML
	} else if f.Delta != nil {
		var fragments strings.Builder
		for _, upsert := range f.Delta.Upserts {
			fragments.WriteString(upsert.RowHTML)
			fragments.WriteString(upsert.CardHTML)
		}
		for _, region := range f.Delta.Regions {
			fragments.WriteString(region.HTML)
		}
		f.HTML = fragments.String()
	}
	return f
}

// TestMergeTableEventDeleteReleasesRows pins both halves of snapshot deletion:
// visible rows retain their order, and the obsolete backing-array slot is zeroed
// so a long-lived Live stream does not retain deleted cells or object maps. A
// delete for an already-absent object is a no-op.
func TestMergeTableEventDeleteReleasesRows(t *testing.T) {
	row := func(name string) kube.Row {
		return kube.Row{
			Cells:   []any{name},
			Object:  map[string]any{"metadata": map[string]any{"name": name, "namespace": "default"}},
			Cluster: "test",
		}
	}
	deleted := func(name string) kube.WatchEvent {
		return kube.WatchEvent{Type: kube.WatchDeleted, Table: kube.Table{Rows: []kube.Row{row(name)}}}
	}
	snapshot := kube.Table{Rows: []kube.Row{row("alpha"), row("beta"), row("gamma")}}
	assertNames := func(want ...string) {
		t.Helper()
		if len(want) != len(snapshot.Rows) {
			t.Fatalf("snapshot row count = %d, want %d", len(snapshot.Rows), len(want))
		}
		for i, name := range want {
			if got := nestedString(snapshot.Rows[i].Object, "metadata", "name"); got != name {
				t.Fatalf("snapshot row %d name = %q, want %q", i, got, name)
			}
		}
	}
	assertClearedTail := func() {
		t.Helper()
		backing := snapshot.Rows[:cap(snapshot.Rows)]
		for i := len(snapshot.Rows); i < len(backing); i++ {
			if backing[i].Cells != nil || backing[i].Object != nil || backing[i].Cluster != "" {
				t.Fatalf("obsolete backing row %d retained data: %#v", i, backing[i])
			}
		}
	}

	beta := deleted("beta")
	mergeTableEvent(&snapshot, &beta)
	assertNames("alpha", "gamma")
	assertClearedTail()

	missing := deleted("missing")
	mergeTableEvent(&snapshot, &missing)
	assertNames("alpha", "gamma")
	assertClearedTail()

	gamma := deleted("gamma")
	mergeTableEvent(&snapshot, &gamma)
	assertNames("alpha")
	assertClearedTail()
}

// TestStreamHandshakeInitialPush pins the SSE handshake: event-stream
// headers (Content-Type / no-store / X-Accel-Buffering through the real
// middleware chain incl. statusWriter flushing) and the initial full push as
// a v2 snapshot frame echoing the client-minted generation verbatim.
func TestStreamHandshakeInitialPush(t *testing.T) {
	ts, _ := newStreamFixture(t)
	s := openStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream", "42")
	for header, want := range map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-store",
		"X-Accel-Buffering": "no",
	} {
		if got := s.resp.Header.Get(header); got != want {
			t.Fatalf("%s = %q, want %q", header, got, want)
		}
	}
	if got := s.resp.Header.Get(streamVersionHeader); got != "2" {
		t.Fatalf("%s = %q, want 2", streamVersionHeader, got)
	}
	ev := s.requireEvent(t, "ro-live", 5*time.Second)
	frame := decodeFrame(t, ev)
	if frame.Kind != "snapshot" || frame.G != "42" {
		t.Fatalf("generation echo = %q, want the client-minted \"42\"", frame.G)
	}
	for _, needle := range []string{"nginx", "my-app", `data-key="test/default/nginx"`} {
		if !strings.Contains(frame.HTML, needle) {
			t.Fatalf("initial push missing %q", needle)
		}
	}
}

func TestStreamInitialListFailureStaysPreHandshake(t *testing.T) {
	ts, fake := newStreamFixture(t)
	arm, err := http.Get(fake.URL + "/__control/fail-lists?mode=500")
	if err != nil {
		t.Fatal(err)
	}
	_ = arm.Body.Close()
	if arm.StatusCode != http.StatusOK {
		t.Fatalf("arm fail-lists status = %d, want 200", arm.StatusCode)
	}

	resp := dialStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream", "list-failure")
	defer func() { _ = resp.Body.Close() }()
	assertStreamPreHandshakeResponse(t, resp)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("initial-list failure status = %d, want 502", resp.StatusCode)
	}
}

func TestStreamV2NegotiationSnapshotAndTerminal(t *testing.T) {
	ts, _ := newStreamFixture(t, func(tuning *streamTuning) {
		tuning.idleCap = 100 * time.Millisecond
		tuning.heartbeat = 10 * time.Millisecond
	})
	rawQuery := "g=query-generation&%67=encoded-generation&f=status%3DRunning,Pending"
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/clusters/test/namespaces/default/pods/_stream?"+rawQuery, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(streamVersionHeader, "2")
	req.Header.Set(streamGenerationHeader, "header-generation")
	s := openStreamRequest(t, req)
	if got := s.resp.Header.Get(streamVersionHeader); got != "2" {
		t.Fatalf("response live version = %q, want 2", got)
	}
	if got := s.resp.Header.Get(streamGenerationHeader); got != "header-generation" {
		t.Fatalf("response live generation = %q, want negotiated header generation", got)
	}
	vary := strings.Join(s.resp.Header.Values("Vary"), ",")
	if !strings.Contains(vary, streamVersionHeader) || !strings.Contains(vary, streamGenerationHeader) {
		t.Fatalf("v2 Vary = %q, want both negotiation headers", vary)
	}

	snapshot := decodeFrame(t, s.requireEvent(t, "ro-live", 5*time.Second))
	if snapshot.V != 2 || snapshot.Kind != "snapshot" || snapshot.G != "header-generation" || snapshot.Seq != 1 {
		t.Fatalf("v2 snapshot identity = %+v", snapshot)
	}
	if snapshot.Rev == "" || snapshot.RV == "" || snapshot.Schema == "" || snapshot.Snapshot == nil || !strings.Contains(snapshot.Snapshot.HTML, "nginx") {
		t.Fatalf("v2 snapshot state is incomplete: %+v", snapshot)
	}

	terminal := decodeFrame(t, s.requireEvent(t, "ro-live", 2*time.Second))
	if terminal.V != 2 || terminal.Kind != "terminal" || terminal.G != snapshot.G || terminal.Seq != 2 {
		t.Fatalf("v2 terminal identity = %+v", terminal)
	}
	if terminal.Rev != snapshot.Rev || terminal.Schema != snapshot.Schema || terminal.Reason != "idle" {
		t.Fatalf("v2 terminal state = %+v, snapshot = %+v", terminal, snapshot)
	}
	s.requireClosed(t, time.Second)
}

func TestNegotiateLiveStreamHeaderContract(t *testing.T) {
	tests := []struct {
		name       string
		rawQuery   string
		headers    http.Header
		wantGen    string
		wantStatus int
	}{
		{name: "headers absent", wantStatus: http.StatusBadRequest},
		{name: "query generation is not negotiation", rawQuery: "g=legacy", wantStatus: http.StatusBadRequest},
		{name: "generation header alone", rawQuery: "g=legacy", headers: http.Header{streamGenerationHeader: {"ignored"}}, wantStatus: http.StatusBadRequest},
		{name: "header v2 ignores query duplicates", rawQuery: "g=first&%67=second&bad%ZZ=kept", headers: http.Header{streamVersionHeader: {"2"}, streamGenerationHeader: {"header"}}, wantGen: "header"},
		{name: "unsupported header", headers: http.Header{streamVersionHeader: {"3"}, streamGenerationHeader: {"header"}}, wantStatus: http.StatusNotAcceptable},
		{name: "empty explicit version", headers: http.Header{streamVersionHeader: {""}, streamGenerationHeader: {"header"}}, wantStatus: http.StatusNotAcceptable},
		{name: "missing header generation", headers: http.Header{streamVersionHeader: {"2"}}, wantStatus: http.StatusBadRequest},
		{name: "duplicate same version header", headers: http.Header{streamVersionHeader: {"2", "2"}, streamGenerationHeader: {"header"}}, wantStatus: http.StatusBadRequest},
		{name: "duplicate conflicting version header", headers: http.Header{streamVersionHeader: {"2", "3"}, streamGenerationHeader: {"header"}}, wantStatus: http.StatusBadRequest},
		{name: "comma folded version header", headers: http.Header{streamVersionHeader: {"2, 2"}, streamGenerationHeader: {"header"}}, wantStatus: http.StatusBadRequest},
		{name: "duplicate same generation header", headers: http.Header{streamVersionHeader: {"2"}, streamGenerationHeader: {"header", "header"}}, wantStatus: http.StatusBadRequest},
		{name: "duplicate conflicting generation header", headers: http.Header{streamVersionHeader: {"2"}, streamGenerationHeader: {"header", "other"}}, wantStatus: http.StatusBadRequest},
		{name: "comma folded generation header", headers: http.Header{streamVersionHeader: {"2"}, streamGenerationHeader: {"header,other"}}, wantStatus: http.StatusBadRequest},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods/_stream", nil)
			req.URL.RawQuery = tc.rawQuery
			for key, values := range tc.headers {
				for _, value := range values {
					req.Header.Add(key, value)
				}
			}
			got, status := negotiateLiveStream(req)
			if status != tc.wantStatus || got.gen != tc.wantGen {
				t.Fatalf("negotiation = {gen:%q}, status %d; want {gen:%q}, status %d", got.gen, status, tc.wantGen, tc.wantStatus)
			}
		})
	}
}

func TestStreamNegotiationRejectsInvalidGeneration(t *testing.T) {
	ts, _ := newStreamFixture(t)
	path := ts.URL + "/clusters/test/namespaces/default/pods/_stream"

	request := func(version, generation, query string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, path+query, nil)
		if err != nil {
			t.Fatal(err)
		}
		if version != "" {
			req.Header.Set(streamVersionHeader, version)
		}
		if generation != "" {
			req.Header.Set(streamGenerationHeader, generation)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		assertStreamPreHandshakeResponse(t, resp)
		return resp
	}

	if got := request("2", "", "").StatusCode; got != http.StatusBadRequest {
		t.Fatalf("v2 missing generation status = %d, want 400", got)
	}
	if got := request("3", "short", "").StatusCode; got != http.StatusNotAcceptable {
		t.Fatalf("unsupported version status = %d, want 406", got)
	}
	long := strings.Repeat("g", streamMaxGenerationBytes+1)
	if got := request("2", long, "").StatusCode; got != http.StatusBadRequest {
		t.Fatalf("v2 long generation status = %d, want 400", got)
	}
	if got := request("2", "bad generation", "").StatusCode; got != http.StatusBadRequest {
		t.Fatalf("v2 non-token generation status = %d, want 400", got)
	}
	if got := request("", "", "?g="+long).StatusCode; got != http.StatusBadRequest {
		t.Fatalf("missing v2 headers with query generation status = %d, want 400", got)
	}
}

func TestValidLiveGeneration(t *testing.T) {
	for _, gen := range []string{"550e8400-e29b-41d4-a716-446655440000", "AbC_123-xyz", "token.~"} {
		if !validLiveGeneration(gen) {
			t.Errorf("valid generation %q rejected", gen)
		}
	}
	for _, gen := range []string{"", "comma,separated", "has space", "slash/value", "é", strings.Repeat("a", streamMaxGenerationBytes+1)} {
		if validLiveGeneration(gen) {
			t.Errorf("invalid generation %q accepted", gen)
		}
	}
}

func TestEncodeStreamPayloadIsBoundedAndDoesNotEscapeHTML(t *testing.T) {
	payload := streamLiveEnvelope{V: 2, Kind: "snapshot", G: "g", Seq: 1, Snapshot: &streamLiveSnapshot{HTML: "<div data-x=\"a&b\">line 1\nline 2</div>"}}
	data, err := encodeStreamPayload(payload, 1024)
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(data)
	if !strings.Contains(encoded, `<div data-x=\"a&b\">`) {
		t.Fatalf("HTML was escaped in stream JSON: %s", encoded)
	}
	if strings.Contains(encoded, `\u003c`) || strings.Contains(encoded, `\u003e`) || strings.Contains(encoded, `\u0026`) {
		t.Fatalf("stream JSON contains HTML escapes: %s", encoded)
	}
	if strings.ContainsAny(encoded, "\r\n") || !strings.Contains(encoded, `line 1\nline 2`) {
		t.Fatalf("stream JSON is not a single escaped-control line: %q", encoded)
	}
	if _, err := encodeStreamPayload(payload, len(data)-1); !errors.Is(err, errStreamEventTooLarge) {
		t.Fatalf("oversized payload error = %v, want errStreamEventTooLarge", err)
	}
}

func TestStreamHeartbeatComment(t *testing.T) {
	ts, _ := newStreamFixture(t, func(tuning *streamTuning) {
		tuning.heartbeat = 10 * time.Millisecond
		tuning.idleCap = 150 * time.Millisecond
	})
	resp := dialStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream", "heartbeat")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", resp.StatusCode)
	}
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended before heartbeat: %v", err)
		}
		if line == ": heartbeat\n" {
			return
		}
		if strings.Contains(line, `"reason":"idle"`) {
			t.Fatal("idle terminal arrived before heartbeat")
		}
	}
}

func TestStreamEventWindowIsFixedSize(t *testing.T) {
	var window streamEventWindow
	now := time.Now()
	for range 100_000 {
		window.note(now)
	}
	if window.count != streamChurnEvents || len(window.times) != streamChurnEvents {
		t.Fatalf("event window retained %d/%d timestamps, want fixed %d", window.count, len(window.times), streamChurnEvents)
	}
	if !window.high(now) {
		t.Fatal("100,000 simultaneous events were not classified as high churn")
	}
	if window.high(now.Add(streamChurnWindow)) {
		t.Fatal("events at the trailing-window boundary remained high churn")
	}
}

func TestStreamRenderRequestClearsRawPathAndKeepsQuery(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/clusters/test/namespaces/default/pods/_stream?f=status%3DRunning,Pending", nil)
	req.URL.RawPath = "/clusters/test/namespaces/default/pods/%5fstream"
	renderReq := streamRenderRequest(req)
	if renderReq.URL.RequestURI() != "/clusters/test/namespaces/default/pods?f=status%3DRunning,Pending" || renderReq.URL.RawPath != "" {
		t.Fatalf("render URI = %q rawPath=%q", renderReq.URL.RequestURI(), renderReq.URL.RawPath)
	}
}

// TestStreamModifiedEventPushes pins the core loop: a MODIFIED watch event
// merges into the snapshot and the next push carries the changed cell — with
// `?sort=` applied at RENDER time (the snapshot stays raw): sort=Name flips
// the fixture's nginx-first order to my-app first.
func TestStreamModifiedEventPushes(t *testing.T) {
	ts, fake := newStreamFixture(t)
	s := openStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream?sort=Name", "1")
	initial := decodeFrame(t, s.requireEvent(t, "ro-live", 5*time.Second))
	myApp := strings.Index(initial.HTML, `data-key="test/default/my-app"`)
	nginx := strings.Index(initial.HTML, `data-key="test/default/nginx"`)
	if myApp < 0 || nginx < 0 || myApp > nginx {
		t.Fatalf("sort=Name not applied at render: my-app@%d nginx@%d", myApp, nginx)
	}
	postStreamScript(t, fake.URL, `{"events":[`+podModifiedEvent("Error", 0)+`]}`)
	push := decodeFrame(t, s.requireEvent(t, "ro-live", 3*time.Second))
	if !strings.Contains(push.HTML, "Error") {
		t.Fatal("push after MODIFIED is missing the changed Status cell")
	}
	if push.G != "1" {
		t.Fatalf("push generation = %q, want \"1\" on every message", push.G)
	}
}

// TestStreamBookmarkAdvancesSilently pins the BOOKMARK lane at the real SSE
// seam: a bookmark advances only the upstream re-watch point, so it must not
// dirty or push the table. The same watch must still deliver the next data event.
func TestStreamBookmarkAdvancesSilently(t *testing.T) {
	ts, fake := newStreamFixture(t)
	s := openStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream", "bookmark")
	s.requireEvent(t, "ro-live", 5*time.Second)

	// BOOKMARK is a non-replayable control frame, so arm it only after the
	// upstream watch is open. A regressed data-path treatment schedules a push
	// after the 300ms minimum gap; the wider quiet window observes that failure.
	waitForOpenWatch(t, fake.URL)
	postStreamScript(t, fake.URL, fmt.Sprintf(`{"events":[{"path":%q,"type":"BOOKMARK"}]}`, streamPodsPath))
	s.requireQuiet(t, 750*time.Millisecond)

	postStreamScript(t, fake.URL, `{"events":[`+podModifiedEvent("Error", 0)+`]}`)
	after := decodeFrame(t, s.requireEvent(t, "ro-live", 3*time.Second))
	if !strings.Contains(after.HTML, "Error") {
		t.Fatal("data event after BOOKMARK did not update the Live table")
	}
}

// TestStreamCoalescesBurst pins the coalescing window: two events 50ms apart
// produce exactly ONE push (carrying the latest state), no earlier than the
// 300ms floor after the initial push, and nothing further follows.
func TestStreamCoalescesBurst(t *testing.T) {
	ts, fake := newStreamFixture(t)
	s := openStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream", "2")
	initial := s.requireEvent(t, "ro-live", 5*time.Second)
	postStreamScript(t, fake.URL, `{"events":[`+podModifiedEvent("Error", 0)+`,`+podModifiedEvent("CrashLoopBackOff", 50)+`]}`)
	push := s.requireEvent(t, "ro-live", 3*time.Second)
	if gap := push.at.Sub(initial.at); gap < 280*time.Millisecond {
		t.Fatalf("coalesced push arrived %s after the initial push, violating the 300ms floor", gap)
	}
	if html := decodeFrame(t, push).HTML; !strings.Contains(html, "CrashLoopBackOff") {
		t.Fatal("coalesced push must carry the SECOND event's state")
	}
	// One push for both events — the 50ms-later event must not produce a second.
	s.requireQuiet(t, 700*time.Millisecond)
}

// TestStreamChurnPacing pins the pacing bounds under continuous churn
// (events every 100ms for 2.6s): pushes never closer than the 300ms floor
// and, while events pend, never further apart than the 2s ceiling (small
// transport/scheduler jitter allowance on top).
func TestStreamChurnPacing(t *testing.T) {
	ts, fake := newStreamFixture(t)
	s := openStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream", "3")
	initial := s.requireEvent(t, "ro-live", 5*time.Second)

	var events []string
	for i := 0; i < 26; i++ {
		status := "Error"
		if i%2 == 1 {
			status = "Running"
		}
		events = append(events, podModifiedEvent(status, (i+1)*100))
	}
	postStreamScript(t, fake.URL, `{"events":[`+strings.Join(events, ",")+`]}`)

	times := []time.Time{initial.at}
	deadline := time.After(3400 * time.Millisecond)
collect:
	for {
		select {
		case ev, ok := <-s.events:
			if !ok {
				t.Fatal("stream closed during churn")
			}
			if ev.name != "ro-live" {
				t.Fatalf("unexpected %s during churn: %s", ev.name, ev.data)
			}
			times = append(times, ev.at)
		case <-deadline:
			break collect
		}
	}
	if len(times) < 3 {
		t.Fatalf("only %d pushes during 3.4s of churn — coalescing starved the screen", len(times))
	}
	// Degradation must ENGAGE, not merely stay inside the [floor, ceiling]
	// envelope (floor-paced pushes the whole window would also satisfy it).
	// Budget: the initial push + ~3 floor-paced pushes before the 10-events-
	// in-2s detection trips (~t+1.0s) + 1 degraded push at ~t+2.9s = 5
	// nominal, 6 with one jitter-delayed detection. The undegraded behavior
	// pushes every ~300ms for the whole 3.4s window (~11-12 pushes), so the
	// bound separates cleanly.
	if len(times) > 6 {
		t.Fatalf("%d pushes during 3.4s of sustained churn — degradation never engaged (want ≤6)", len(times))
	}
	for i := 1; i < len(times); i++ {
		gap := times[i].Sub(times[i-1])
		if gap < 280*time.Millisecond {
			t.Fatalf("pushes %d→%d only %s apart, violating the 300ms floor", i-1, i, gap)
		}
		if gap > 2500*time.Millisecond {
			t.Fatalf("pushes %d→%d %s apart while events pended, violating the 2s ceiling", i-1, i, gap)
		}
	}
}

// TestStreamFilterTransitions pins the unfiltered-snapshot contract: with an
// active `?f=` the filter applies at RENDER time, so a MODIFY that makes a
// non-matching object match brings it into the next push, and the inverse
// removes it — the snapshot itself never drops non-matching rows.
func TestStreamFilterTransitions(t *testing.T) {
	ts, fake := newStreamFixture(t)
	s := openStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream?f=Status:Error", "7")
	const nginxKey = `data-key="test/default/nginx"`

	initial := decodeFrame(t, s.requireEvent(t, "ro-live", 5*time.Second))
	if strings.Contains(initial.HTML, nginxKey) {
		t.Fatal("initial push shows nginx although it does not match f=Status:Error")
	}

	// non-matching → matching: nginx turns Error and must APPEAR.
	postStreamScript(t, fake.URL, `{"events":[`+podModifiedEvent("Error", 0)+`]}`)
	appeared := decodeFrame(t, s.requireEvent(t, "ro-live", 3*time.Second))
	if !strings.Contains(appeared.HTML, nginxKey) {
		t.Fatal("nginx did not appear after MODIFY made it match the active filter")
	}

	// matching → non-matching: nginx recovers and must DISAPPEAR.
	postStreamScript(t, fake.URL, fmt.Sprintf(`{"events":[{"path":%q,"type":"MODIFIED","cells":["nginx","1/1","Running","0","10m"],"object":{"apiVersion":"v1","kind":"Pod","metadata":{"name":"nginx","namespace":"default"},"status":{"phase":"Running"}}}]}`, streamPodsPath))
	gone := decodeFrame(t, s.requireEvent(t, "ro-live", 3*time.Second))
	if strings.Contains(gone.HTML, nginxKey) {
		t.Fatal("nginx still shown after MODIFY made it stop matching the active filter")
	}
}

// TestStreamGoneRelists pins the 410 branch: a scripted GONE triggers a
// silent relist + full push (never a terminal), and the re-watched stream
// keeps delivering subsequent changes. The relist itself is proven by the
// recorder — a fresh non-watch LIST must hit the pods path after the GONE.
// The pushed "nginx" alone is NOT proof: the stale snapshot also contains it,
// so a handler that skipped the relist would pass that needle.
func TestStreamGoneRelists(t *testing.T) {
	var mu sync.Mutex
	goneArmed := false
	listsAfterGone := 0
	ts, fake := newStreamFixtureWithRecorder(t, func(r *http.Request) {
		if r.URL.Query().Get("watch") == "true" || !strings.HasSuffix(r.URL.Path, "/pods") {
			return
		}
		mu.Lock()
		if goneArmed {
			listsAfterGone++
		}
		mu.Unlock()
	})
	s := openStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream", "4")
	s.requireEvent(t, "ro-live", 5*time.Second)

	// The GONE must land on an OPEN watch (control entries never replay):
	// wait for the server's first watch connect before posting.
	waitForOpenWatch(t, fake.URL)
	mu.Lock()
	goneArmed = true
	mu.Unlock()
	postStreamScript(t, fake.URL, fmt.Sprintf(`{"events":[{"path":%q,"type":"GONE"}]}`, streamPodsPath))
	relist := s.requireEvent(t, "ro-live", 3*time.Second) // the relist snapshot, not a terminal envelope
	if !strings.Contains(decodeFrame(t, relist).HTML, "nginx") {
		t.Fatal("relist push is missing the listed rows")
	}
	mu.Lock()
	relists := listsAfterGone
	mu.Unlock()
	if relists == 0 {
		t.Fatal("no fresh pods LIST after the GONE — the 410 path skipped the relist")
	}

	// The re-watch from the fresh RV is live: a new MODIFY still pushes.
	postStreamScript(t, fake.URL, `{"events":[`+podModifiedEvent("Error", 0)+`]}`)
	after := decodeFrame(t, s.requireEvent(t, "ro-live", 3*time.Second))
	if !strings.Contains(after.HTML, "Error") {
		t.Fatal("stream stopped delivering changes after the 410 relist")
	}
}

// TestStreamWatchlessKind204 pins the watch-less taxonomy: a kind without
// the watch verb (the metrics pseudo-type pods printer) gets 204 — the
// client falls back to polling silently.
func TestStreamWatchlessKind204(t *testing.T) {
	ts, _ := newStreamFixture(t)
	resp := dialStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream?apiVersion=metrics.k8s.io/v1beta1", "watchless")
	defer func() { _ = resp.Body.Close() }()
	assertStreamPreHandshakeResponse(t, resp)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("watch-less kind stream status = %d, want 204", resp.StatusCode)
	}
}

// TestStreamScope404 pins the Live-mode scope cut: multi-type plurals (all / CSV)
// and multi-cluster scope (_all / CSV) get 404 — Live is single-type,
// single-cluster only.
func TestStreamScope404(t *testing.T) {
	ts, _ := newStreamFixture(t)
	for _, path := range []string{
		"/clusters/_all/pods/_stream",
		"/clusters/test,other/pods/_stream",
		"/clusters/test/namespaces/default/all/_stream",
		"/clusters/test/namespaces/default/pods,services/_stream",
		"/clusters/test/namespaces/_all/_all/_stream",
	} {
		resp := dialStream(t, ts.URL+path, "invalid-scope")
		_ = resp.Body.Close()
		assertStreamPreHandshakeResponse(t, resp)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", path, resp.StatusCode)
		}
	}

	for _, path := range []string{
		"/clusters/missing/namespaces/default/pods/_stream",
		"/clusters/test/namespaces/default/not-a-resource/_stream",
	} {
		resp := dialStream(t, ts.URL+path, "missing-resource")
		_ = resp.Body.Close()
		assertStreamPreHandshakeResponse(t, resp)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestStreamForbiddenNamespaceStaysPreHandshake(t *testing.T) {
	fake := newServerFakeAPI(t)
	app := newTestServerWithConfig(t, &config.Config{
		Port:              8080,
		Clusters:          []config.ClusterConnection{{Name: "test", Server: fake.URL}},
		DefaultTheme:      "dark",
		ExcludeNamespaces: []*regexp.Regexp{regexp.MustCompile(`^secret$`)},
	})
	ts := httptest.NewServer(app.Handler())
	t.Cleanup(ts.Close)

	resp := dialStream(t, ts.URL+"/clusters/test/namespaces/secret/pods/_stream", "forbidden-namespace")
	defer func() { _ = resp.Body.Close() }()
	assertStreamPreHandshakeResponse(t, resp)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("forbidden namespace stream status = %d, want 403", resp.StatusCode)
	}
}

// TestStreamCapAndRelease pins the concurrency cap: the 33rd concurrent
// stream gets 429 BEFORE SSE headers, and closing one stream releases its
// slot so a new 33rd can connect (the deferred-release cleanup contract).
func TestStreamCapAndRelease(t *testing.T) {
	ts, _ := newStreamFixture(t)
	streams := make([]*http.Response, 0, streamCapMax)
	for i := 0; i < streamCapMax; i++ {
		resp := dialStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream", fmt.Sprintf("normal-%d", i))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream %d status = %d, want 200", i, resp.StatusCode)
		}
		streams = append(streams, resp)
	}
	t.Cleanup(func() {
		for _, resp := range streams {
			_ = resp.Body.Close()
		}
	})

	over := dialStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream", "normal-over-cap")
	_ = over.Body.Close()
	assertStreamPreHandshakeResponse(t, over)
	if over.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("33rd stream status = %d, want 429", over.StatusCode)
	}
	if ct := over.Header.Get("Content-Type"); strings.Contains(ct, "text/event-stream") {
		t.Fatalf("cap-exceeded stream got SSE headers (Content-Type %q) — it must 429 before them", ct)
	}

	// Release one slot and prove a new stream can take it.
	_ = streams[0].Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		retry := dialStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream", "normal-retry")
		status := retry.StatusCode
		_ = retry.Body.Close()
		if status == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("slot never released: last retry status = %d", status)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestDemoStreamCapacityAdmitsMoreThanNormalCap(t *testing.T) {
	fake := newServerFakeAPI(t)
	app := newTestServerWithConfig(t, &config.Config{
		Port:         8080,
		Demo:         true,
		Clusters:     []config.ClusterConnection{{Name: "test", Server: fake.URL}},
		DefaultTheme: "dark",
	})
	if got := cap(app.streamSlots); got != demoStreamCapMax || got < 256 {
		t.Fatalf("demo Live capacity = %d, want %d and at least 256", got, demoStreamCapMax)
	}
	ts := httptest.NewServer(app.Handler())
	t.Cleanup(ts.Close)

	streams := make([]*http.Response, 0, streamCapMax+1)
	t.Cleanup(func() {
		for _, resp := range streams {
			_ = resp.Body.Close()
		}
	})
	for i := 0; i <= streamCapMax; i++ {
		resp := dialStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream", fmt.Sprintf("demo-%d", i))
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			t.Fatalf("demo stream %d status = %d, want 200 beyond the normal %d-stream cap", i+1, resp.StatusCode, streamCapMax)
		}
		streams = append(streams, resp)
	}
}

// TestStreamIdleTerminal pins the server-local idle cap: a stream with no
// watch data for the cap emits a terminal `ro-live` envelope and closes.
func TestStreamIdleTerminal(t *testing.T) {
	ts, _ := newStreamFixture(t, func(tuning *streamTuning) {
		tuning.idleCap = 250 * time.Millisecond
	})
	s := openStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream", "5")
	s.requireEvent(t, "ro-live", 5*time.Second)
	term := s.requireEvent(t, "ro-live", 3*time.Second)
	frame := decodeFrame(t, term)
	if frame.Reason != "idle" {
		t.Fatalf("terminal reason = %q, want idle", frame.Reason)
	}
	if frame.G != "5" {
		t.Fatalf("terminal generation = %q, want \"5\" (echoed in every message)", frame.G)
	}
	s.requireClosed(t, 2*time.Second)
}

// TestStreamEOFStormTerminal pins the storm rule: consecutive immediate EOFs
// are retried with backoff a bounded number of times (observable as exactly
// streamMaxImmediateEOFs watch connects — never a spin), then the stream
// terminates with reason "watch-failed". The backoff schedule itself is
// pinned at its real defaults by TestStreamBackoffSchedule; here it is
// compressed so the storm completes quickly.
func TestStreamEOFStormTerminal(t *testing.T) {
	var mu sync.Mutex
	watchConnects := 0
	ts, fake := newStreamFixtureWithRecorder(t, func(r *http.Request) {
		if r.URL.Query().Get("watch") == "true" {
			mu.Lock()
			watchConnects++
			mu.Unlock()
		}
	}, func(tuning *streamTuning) {
		tuning.backoffBase = 20 * time.Millisecond
		tuning.backoffCap = 100 * time.Millisecond
	})
	s := openStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream", "6")
	s.requireEvent(t, "ro-live", 5*time.Second)

	// Five EOFs, each landing while a (re-)watch is open: spacing 400ms vs a
	// ≤100ms re-watch delay leaves a wide margin. Every killed attempt ends
	// event-less within the immediate window, so the 5th is terminal. The
	// margin argument starts from the FIRST watch being open — EOFs are
	// control entries (never replayed, dropped on zero conns), so the post
	// must wait for the initial watch connect or the first EOF can vanish.
	waitForOpenWatch(t, fake.URL)
	var eofs []string
	for i := 0; i < streamMaxImmediateEOFs; i++ {
		eofs = append(eofs, fmt.Sprintf(`{"path":%q,"type":"EOF","delayMs":%d}`, streamPodsPath, (i+1)*400))
	}
	postStreamScript(t, fake.URL, `{"events":[`+strings.Join(eofs, ",")+`]}`)

	term := s.requireEvent(t, "ro-live", 6*time.Second)
	if reason := decodeFrame(t, term).Reason; reason != "watch-failed" {
		t.Fatalf("terminal reason = %q, want watch-failed", reason)
	}
	s.requireClosed(t, 2*time.Second)

	mu.Lock()
	connects := watchConnects
	mu.Unlock()
	if connects != streamMaxImmediateEOFs {
		t.Fatalf("watch connects = %d, want exactly %d (initial + backed-off re-watches, no spin)", connects, streamMaxImmediateEOFs)
	}
}

// TestStreamBackoffSchedule pins the re-watch backoff at its REAL defaults:
// 250ms doubling to the 10s cap, and the healthy-minute reset — a short-lived
// attempt must NOT reset the schedule.
func TestStreamBackoffSchedule(t *testing.T) {
	tuning := defaultStreamTuning()
	b := streamBackoff{tuning: tuning}
	want := []time.Duration{
		250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second,
		4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second,
	}
	for i, w := range want {
		if got := b.next(); got != w {
			t.Fatalf("backoff attempt %d = %s, want %s", i, got, w)
		}
	}
	b.noteAttempt(tuning.healthyReset) // a healthy minute resets the schedule
	if got := b.next(); got != 250*time.Millisecond {
		t.Fatalf("backoff after a healthy attempt = %s, want the 250ms base", got)
	}
	b.noteAttempt(time.Second) // a short-lived attempt must NOT reset
	if got := b.next(); got != 500*time.Millisecond {
		t.Fatalf("backoff after a short attempt = %s, want 500ms (no reset)", got)
	}
}

func TestDefaultStreamTuning(t *testing.T) {
	got := defaultStreamTuning()
	want := streamTuning{
		idleCap:               30 * time.Minute,
		backoffBase:           250 * time.Millisecond,
		backoffCap:            10 * time.Second,
		healthyReset:          time.Minute,
		immediateWindow:       time.Second,
		metricsPoll:           30 * time.Second,
		maxLifetime:           12 * time.Hour,
		writeTimeout:          30 * time.Second,
		heartbeat:             20 * time.Second,
		checkpointInterval:    10 * time.Minute,
		checkpointDeltas:      2048,
		handshakeTimeout:      15 * time.Second,
		initialMetricsTimeout: 10 * time.Second,
		metricsRequestTimeout: 10 * time.Second,
	}
	if got != want {
		t.Fatalf("default stream tuning = %#v, want %#v", got, want)
	}

	// A session owns the connection-time snapshot. Later setup of another
	// Server (or accidental mutation of this Server in a test) cannot alter an
	// already-running stream's timing behavior.
	session := streamSession{tuning: got}
	got.idleCap = time.Millisecond
	if session.tuning.idleCap != 30*time.Minute {
		t.Fatalf("session idle cap changed through server-local tuning: %s", session.tuning.idleCap)
	}
}

// TestStreamAuthExpiryTerminal pins the auth branch: an upstream 401 on the
// watch (the fakeapi one-shot arm — the session-token-expiry shape) is
// terminal with reason "auth", never retried.
func TestStreamAuthExpiryTerminal(t *testing.T) {
	ts, fake := newStreamFixture(t)
	// Arm BEFORE the stream opens: lists are unaffected, so the handshake
	// succeeds and the FIRST watch connect consumes the 401.
	resp, err := http.Get(fake.URL + "/__control/watch-401")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("arming watch-401 status = %d", resp.StatusCode)
	}

	s := openStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream", "8")
	s.requireEvent(t, "ro-live", 5*time.Second)
	term := s.requireEvent(t, "ro-live", 3*time.Second)
	if reason := decodeFrame(t, term).Reason; reason != "auth" {
		t.Fatalf("terminal reason = %q, want auth", reason)
	}
	s.requireClosed(t, 2*time.Second)
}

// TestStreamShutdownTerminal pins the shutdown branch: cancelling the
// server's base context (the New() ctx) sends a terminal `ro-live` reason
// "shutdown" to open streams before they close.
func TestStreamShutdownTerminal(t *testing.T) {
	fake := newServerFakeAPI(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	app, err := New(ctx, &config.Config{Port: 8080, Clusters: []config.ClusterConnection{{Name: "test", Server: fake.URL}}, DefaultTheme: "dark", NoAccessLogs: true})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(app.Handler())
	t.Cleanup(ts.Close)

	s := openStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream", "9")
	s.requireEvent(t, "ro-live", 5*time.Second)
	cancel()
	term := s.requireEvent(t, "ro-live", 3*time.Second)
	if reason := decodeFrame(t, term).Reason; reason != "shutdown" {
		t.Fatalf("terminal reason = %q, want shutdown", reason)
	}
	s.requireClosed(t, 2*time.Second)
}

// TestStreamMaxLifetimeTerminal pins the hard lifetime bound (security
// review, waves E+F): in trusted-headers/none auth modes a stream has no
// per-session expiry, and the idle cap resets on every watch event — without
// a total-lifetime bound a stream runs forever. The server-local 12h default
// terminates it with reason "idle". The 30-minute default idle cap is four
// orders of magnitude above the injected bound, so a terminal arriving within
// seconds can only be the lifetime timer.
func TestStreamMaxLifetimeTerminal(t *testing.T) {
	ts, _ := newStreamFixture(t, func(tuning *streamTuning) {
		tuning.maxLifetime = 300 * time.Millisecond
	})
	s := openStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream", "12")
	s.requireEvent(t, "ro-live", 5*time.Second)
	term := s.requireEvent(t, "ro-live", 3*time.Second)
	if reason := decodeFrame(t, term).Reason; reason != "idle" {
		t.Fatalf("terminal reason = %q, want idle", reason)
	}
	s.requireClosed(t, 2*time.Second)
}

// TestStreamOIDCSessionExpiryTerminal pins the session-bound lifetime
// (security review, waves E+F): in OIDC mode the connect-time cookie check is
// the ONLY auth check an SSE stream ever gets, so the stream must not outlive
// the session it was authorized with — at the session's Expires instant the
// server emits a terminal `ro-live` reason "auth" and closes. The expiry is
// injectable through the session cookie itself (Expires is unix seconds, so
// the shortest deterministic TTL is ~2s).
func TestStreamOIDCSessionExpiryTerminal(t *testing.T) {
	fake, err := fakeapi.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	app := newTestServerWithConfig(t, &config.Config{
		Port:          8080,
		Clusters:      []config.ClusterConnection{{Name: "test", Server: fake.URL}},
		DefaultTheme:  "dark",
		AuthMode:      config.AuthModeOIDC,
		OIDCIssuerURL: "https://issuer.invalid",
	})
	ts := httptest.NewServer(app.Handler())
	t.Cleanup(ts.Close)

	value, err := app.auth.SealSession(&auth.Session{
		AccessToken: "session-token",
		Expires:     time.Now().Add(2 * time.Second).Unix(),
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/clusters/test/namespaces/default/pods/_stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: value})
	setTestLiveHeaders(req, "13")

	s := openStreamRequest(t, req)
	s.requireEvent(t, "ro-live", 5*time.Second)
	term := s.requireEvent(t, "ro-live", 4*time.Second)
	if reason := decodeFrame(t, term).Reason; reason != "auth" {
		t.Fatalf("terminal reason = %q, want auth", reason)
	}
	s.requireClosed(t, 2*time.Second)
}

// TestStreamWriteDeadlineFreesWedgedSlot pins the non-draining-client armor
// (security review, waves E+F): a connected peer that stops READING wedges
// the SSE write (Fprintf/Flush block once TCP buffers fill) — the handler
// never returns to its select loop, no timer can fire, and the deferred cap
// slot leaks until restart. The server-local per-write deadline turns the wedge
// into a write error — the normal client-gone exit — and the slot releases. The
// 600-row "big" fixture makes each push large enough to fill the loopback
// buffers within a few frames.
func TestStreamWriteDeadlineFreesWedgedSlot(t *testing.T) {
	fake, err := fakeapi.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	app := newTestServerWithConfig(t, &config.Config{Port: 8080, Clusters: []config.ClusterConnection{{Name: "test", Server: fake.URL}}, DefaultTheme: "dark"})
	app.streamTuning.writeTimeout = 250 * time.Millisecond
	// V2 normally emits a tiny row delta. Force every changed projection to a
	// full checkpoint so the non-reading peer exercises a genuinely blocked
	// downstream write rather than an obsolete v1 full-table assumption.
	app.streamTuning.checkpointDeltas = 1
	ts := httptest.NewServer(app.Handler())
	t.Cleanup(ts.Close)

	// A raw TCP client that sends the request and then NEVER reads: kernel
	// buffers fill and the server's writes stop completing. Closed at cleanup
	// FIRST (LIFO), so even a regressed (deadline-less) handler unblocks
	// before ts.Close drains.
	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		if err := tcp.SetReadBuffer(1024); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := fmt.Fprintf(conn, "GET /clusters/test/namespaces/big/pods/_stream HTTP/1.1\r\nHost: readout-test\r\n%s: 2\r\n%s: wedge\r\n\r\n", streamVersionHeader, streamGenerationHeader); err != nil {
		t.Fatal(err)
	}

	// The handler acquired its slot (the request routed and the stream started).
	acquire := time.Now().Add(5 * time.Second)
	for len(app.streamSlots) == 0 {
		if time.Now().After(acquire) {
			t.Fatal("stream handler never acquired its cap slot")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Keep producing dirty state so the handler keeps writing frames until
	// one wedges (the initial 600-row push may fit in the buffers); then the
	// injected write deadline must error the write and release the slot.
	deadline := time.Now().Add(10 * time.Second)
	for len(app.streamSlots) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("cap slot still held — the wedged write never hit the deadline")
		}
		postStreamScript(t, fake.URL, `{"events":[{"path":"/api/v1/namespaces/big/pods","type":"MODIFIED","object":{"apiVersion":"v1","kind":"Pod","metadata":{"name":"big-pod-0001","namespace":"big"}}}]}`)
		time.Sleep(150 * time.Millisecond)
	}
}

// TestStreamMetricsJoinSubPoll pins the ?join=metrics plumbing: the initial
// push carries the merged usage cells, and the 30s sub-poll (compressed here)
// picks up changed usage and pushes it — without any per-push metrics fetch.
func TestStreamMetricsJoinSubPoll(t *testing.T) {
	ts, fake := newStreamFixture(t, func(tuning *streamTuning) {
		tuning.metricsPoll = 150 * time.Millisecond
	})
	s := openStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream?join=metrics", "10")
	initial := decodeFrame(t, s.requireEvent(t, "ro-live", 5*time.Second))
	if !strings.Contains(initial.HTML, "250m") {
		t.Fatal("initial push is missing the merged CPU usage cell (250m)")
	}

	postStreamScript(t, fake.URL, `{"events":[{"path":"/apis/metrics.k8s.io/v1beta1/namespaces/default/pods","type":"MODIFIED","object":{"kind":"PodMetrics","apiVersion":"metrics.k8s.io/v1beta1","metadata":{"name":"nginx","namespace":"default"},"containers":[{"name":"nginx","usage":{"cpu":"900m","memory":"128Mi"}}]}}]}`)

	deadline := time.Now().Add(4 * time.Second)
	for {
		ev := s.requireEvent(t, "ro-live", 4*time.Second)
		if strings.Contains(decodeFrame(t, ev).HTML, "900m") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("metrics sub-poll never surfaced the new usage value")
		}
	}
}

// TestStreamCustomColumnsAndNodeJoinListOnce pins the render cost of a Live
// stream that asks for BOTH custom columns and the ?join=nodes overlay: the
// handshake makes exactly one pods Table LIST and one Nodes LIST, and every
// subsequent push re-renders the retained snapshot with ZERO upstream LISTs.
// (Before the overlays were hoisted, each push re-listed the pods collection
// for the JSONPath objects and re-listed Nodes for the join -- two upstream
// requests per push, at up to ~3 pushes/s per subscriber.)
func TestStreamCustomColumnsAndNodeJoinListOnce(t *testing.T) {
	var mu sync.Mutex
	lists := map[string]int{}
	ts, fake := newStreamFixtureWithRecorder(t, func(r *http.Request) {
		if r.URL.Query().Get("watch") == "true" {
			return
		}
		mu.Lock()
		lists[r.URL.Path]++
		mu.Unlock()
	}, func(tuning *streamTuning) {
		// Park the overlay sub-poll and the maintenance frames outside the test
		// window so the counts below describe the render path alone.
		tuning.metricsPoll = time.Hour
		tuning.heartbeat = 0
		tuning.checkpointInterval = 0
	})
	count := func(path string) int {
		mu.Lock()
		defer mu.Unlock()
		return lists[path]
	}

	// NodeAddr resolves through the synthetic `node` key the join installs, so
	// an "InternalIP" cell proves the overlay reached the JSONPath engine.
	const spec = "?join=nodes&custom-columns=NodeAddr%3Dnode.status.addresses%5B0%5D.type"
	s := openStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream"+spec, "40")
	initial := decodeFrame(t, s.requireEvent(t, "ro-live", 5*time.Second))
	if !strings.Contains(initial.HTML, "NodeAddr") || !strings.Contains(initial.HTML, "InternalIP") {
		t.Fatalf("initial push did not render the node-joined custom column: %s", initial.HTML)
	}
	if got := count(streamPodsPath); got != 1 {
		t.Fatalf("pods LISTs during handshake = %d, want exactly 1", got)
	}
	if got := count("/api/v1/nodes"); got != 1 {
		t.Fatalf("nodes LISTs during handshake = %d, want exactly 1", got)
	}

	waitForOpenWatch(t, fake.URL)
	for _, status := range []string{"Error", "CrashLoopBackOff", "Completed"} {
		postStreamScript(t, fake.URL, `{"events":[`+podModifiedEvent(status, 0)+`]}`)
		frame := decodeFrame(t, s.requireEvent(t, "ro-live", 3*time.Second))
		if !strings.Contains(frame.HTML, status) {
			t.Fatalf("push did not carry the %s change: %s", status, frame.HTML)
		}
	}
	if got := count(streamPodsPath); got != 1 {
		t.Fatalf("pods LISTs after three pushes = %d, want still 1 (a push re-listed upstream)", got)
	}
	if got := count("/api/v1/nodes"); got != 1 {
		t.Fatalf("nodes LISTs after three pushes = %d, want still 1 (a push re-listed the join)", got)
	}
}

// TestStreamNodeJoinSubPoll pins the ?join=nodes plumbing: the Nodes overlay
// rides the SAME 30s sub-poll (compressed here) the metrics overlay does, so a
// Node change reaches the joined column without any per-push Nodes LIST.
func TestStreamNodeJoinSubPoll(t *testing.T) {
	ts, fake := newStreamFixture(t, func(tuning *streamTuning) {
		tuning.metricsPoll = 150 * time.Millisecond
	})
	const spec = "?join=nodes&custom-columns=NodeAddr%3Dnode.status.addresses%5B0%5D.type"
	s := openStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream"+spec, "41")
	initial := decodeFrame(t, s.requireEvent(t, "ro-live", 5*time.Second))
	if !strings.Contains(initial.HTML, "InternalIP") {
		t.Fatalf("initial push is missing the joined node address type: %s", initial.HTML)
	}

	postStreamScript(t, fake.URL, `{"events":[{"path":"/api/v1/nodes","type":"MODIFIED","object":{"apiVersion":"v1","kind":"Node","metadata":{"name":"127.0.0.1","resourceVersion":"9001"},"status":{"addresses":[{"type":"ExternalIP","address":"203.0.113.9"}]}}}]}`)

	deadline := time.Now().Add(4 * time.Second)
	for {
		ev := s.requireEvent(t, "ro-live", 4*time.Second)
		if strings.Contains(decodeFrame(t, ev).HTML, "ExternalIP") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("node sub-poll never surfaced the changed node address type")
		}
	}
}

// TestStreamExcludedFromDurationHistogram pins the metrics contract: a
// completed stream request appears in readout_http_requests_total but NEVER
// in the duration histogram (a 30-minute stream is not request latency).
func TestStreamExcludedFromDurationHistogram(t *testing.T) {
	ts, _ := newStreamFixture(t)
	s := openStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream", "11")
	s.requireEvent(t, "ro-live", 5*time.Second)
	s.close() // end the stream; the middleware records it when the handler returns

	const routeLabel = `path="/clusters/{cluster}/namespaces/{namespace}/{plural}/_stream"`
	deadline := time.Now().Add(3 * time.Second)
	var body string
	for {
		resp, err := http.Get(ts.URL + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		body = string(raw)
		if strings.Contains(body, "readout_http_requests_total") && strings.Contains(body, routeLabel) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stream request never appeared in readout_http_requests_total; metrics body:\n%.2000s", body)
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "readout_http_request_duration_seconds") && strings.Contains(line, "_stream") {
			t.Fatalf("stream leaked into the duration histogram: %s", line)
		}
	}
}

// TestStreamStatusWriterFlushUnwrap pins the SSE-streaming plumbing on statusWriter:
// Flush reaches the wrapped writer (the embedded field used to hide
// http.Flusher, buffering SSE forever) and Unwrap exposes it for
// http.ResponseController.
func TestStreamStatusWriterFlushUnwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec, status: http.StatusOK}
	if err := http.NewResponseController(sw).Flush(); err != nil {
		t.Fatalf("ResponseController.Flush through statusWriter: %v", err)
	}
	if !rec.Flushed {
		t.Fatal("Flush did not reach the underlying writer")
	}
	if sw.Unwrap() != rec {
		t.Fatal("Unwrap must expose the wrapped ResponseWriter")
	}
}
