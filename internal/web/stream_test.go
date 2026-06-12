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
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kbelokon/readout/internal/config"
	"github.com/kbelokon/readout/tests/unit/fakeapi"
)

const streamPodsPath = "/api/v1/namespaces/default/pods"

// setStreamVar swaps a stream tuning knob for one test and restores it at
// cleanup. MUST be called BEFORE newStreamFixture: cleanups run LIFO, so the
// later-registered test-server Close (which waits out every live handler)
// runs first and no handler can read the knob after it is restored.
func setStreamVar[T any](t *testing.T, v *T, val T) {
	t.Helper()
	old := *v
	*v = val
	t.Cleanup(func() { *v = old })
}

// newStreamFixture builds the standard one-cluster app over a fresh fakeapi
// and serves it on a REAL HTTP server (SSE needs live flushing, which
// httptest.NewRecorder cannot exercise).
func newStreamFixture(t *testing.T) (*httptest.Server, *fakeapi.Server) {
	t.Helper()
	return newStreamFixtureWithRecorder(t, nil)
}

func newStreamFixtureWithRecorder(t *testing.T, listRecorder func(*http.Request)) (*httptest.Server, *fakeapi.Server) {
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

// streamFrame decodes either pinned data payload: ro-table carries g + html,
// ro-terminal carries g + reason.
type streamFrame struct {
	G      string `json:"g"`
	HTML   string `json:"html"`
	Reason string `json:"reason"`
}

type sseStream struct {
	resp   *http.Response
	events chan sseEvent
}

// dialStream GETs a stream URL and returns the raw response (no status
// assertion — the non-200 taxonomy tests read the code directly).
func dialStream(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// openStream dials a stream URL, asserts the 200 handshake, and starts a
// background SSE parser delivering events on a channel. The body closes at
// cleanup so the test server can drain its handler.
func openStream(t *testing.T, url string) *sseStream {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return openStreamRequest(t, req)
}

// openStreamRequest is openStream for a caller-built request (the OIDC
// session-expiry test attaches a session cookie).
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
	return f
}

// TestStreamHandshakeInitialPush pins the SSE handshake: event-stream
// headers (Content-Type / no-store / X-Accel-Buffering through the real
// middleware chain incl. statusWriter flushing) and the initial full push as
// an `ro-table` frame echoing the client-minted generation verbatim.
func TestStreamHandshakeInitialPush(t *testing.T) {
	ts, _ := newStreamFixture(t)
	s := openStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream?g=42")
	for header, want := range map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-store",
		"X-Accel-Buffering": "no",
	} {
		if got := s.resp.Header.Get(header); got != want {
			t.Fatalf("%s = %q, want %q", header, got, want)
		}
	}
	ev := s.requireEvent(t, "ro-table", 5*time.Second)
	frame := decodeFrame(t, ev)
	if frame.G != "42" {
		t.Fatalf("generation echo = %q, want the client-minted \"42\"", frame.G)
	}
	for _, needle := range []string{"nginx", "my-app", `data-key="test/default/nginx"`} {
		if !strings.Contains(frame.HTML, needle) {
			t.Fatalf("initial push missing %q", needle)
		}
	}
}

// TestStreamModifiedEventPushes pins the core loop: a MODIFIED watch event
// merges into the snapshot and the next push carries the changed cell — with
// `?sort=` applied at RENDER time (the snapshot stays raw): sort=Name flips
// the fixture's nginx-first order to my-app first.
func TestStreamModifiedEventPushes(t *testing.T) {
	ts, fake := newStreamFixture(t)
	s := openStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream?g=1&sort=Name")
	initial := decodeFrame(t, s.requireEvent(t, "ro-table", 5*time.Second))
	myApp := strings.Index(initial.HTML, `data-key="test/default/my-app"`)
	nginx := strings.Index(initial.HTML, `data-key="test/default/nginx"`)
	if myApp < 0 || nginx < 0 || myApp > nginx {
		t.Fatalf("sort=Name not applied at render: my-app@%d nginx@%d", myApp, nginx)
	}
	postStreamScript(t, fake.URL, `{"events":[`+podModifiedEvent("Error", 0)+`]}`)
	push := decodeFrame(t, s.requireEvent(t, "ro-table", 3*time.Second))
	if !strings.Contains(push.HTML, "Error") {
		t.Fatal("push after MODIFIED is missing the changed Status cell")
	}
	if push.G != "1" {
		t.Fatalf("push generation = %q, want \"1\" on every message", push.G)
	}
}

// TestStreamCoalescesBurst pins the coalescing window: two events 50ms apart
// produce exactly ONE push (carrying the latest state), no earlier than the
// 300ms floor after the initial push, and nothing further follows.
func TestStreamCoalescesBurst(t *testing.T) {
	ts, fake := newStreamFixture(t)
	s := openStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream?g=2")
	initial := s.requireEvent(t, "ro-table", 5*time.Second)
	postStreamScript(t, fake.URL, `{"events":[`+podModifiedEvent("Error", 0)+`,`+podModifiedEvent("CrashLoopBackOff", 50)+`]}`)
	push := s.requireEvent(t, "ro-table", 3*time.Second)
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
	s := openStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream?g=3")
	initial := s.requireEvent(t, "ro-table", 5*time.Second)

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
			if ev.name != "ro-table" {
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
	s := openStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream?g=7&f=Status:Error")
	const nginxKey = `data-key="test/default/nginx"`

	initial := decodeFrame(t, s.requireEvent(t, "ro-table", 5*time.Second))
	if strings.Contains(initial.HTML, nginxKey) {
		t.Fatal("initial push shows nginx although it does not match f=Status:Error")
	}

	// non-matching → matching: nginx turns Error and must APPEAR.
	postStreamScript(t, fake.URL, `{"events":[`+podModifiedEvent("Error", 0)+`]}`)
	appeared := decodeFrame(t, s.requireEvent(t, "ro-table", 3*time.Second))
	if !strings.Contains(appeared.HTML, nginxKey) {
		t.Fatal("nginx did not appear after MODIFY made it match the active filter")
	}

	// matching → non-matching: nginx recovers and must DISAPPEAR.
	postStreamScript(t, fake.URL, fmt.Sprintf(`{"events":[{"path":%q,"type":"MODIFIED","cells":["nginx","1/1","Running","0","10m"],"object":{"apiVersion":"v1","kind":"Pod","metadata":{"name":"nginx","namespace":"default"},"status":{"phase":"Running"}}}]}`, streamPodsPath))
	gone := decodeFrame(t, s.requireEvent(t, "ro-table", 3*time.Second))
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
	s := openStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream?g=4")
	s.requireEvent(t, "ro-table", 5*time.Second)

	// The GONE must land on an OPEN watch (control entries never replay):
	// wait for the server's first watch connect before posting.
	waitForOpenWatch(t, fake.URL)
	mu.Lock()
	goneArmed = true
	mu.Unlock()
	postStreamScript(t, fake.URL, fmt.Sprintf(`{"events":[{"path":%q,"type":"GONE"}]}`, streamPodsPath))
	relist := s.requireEvent(t, "ro-table", 3*time.Second) // the relist full push, NOT ro-terminal
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
	after := decodeFrame(t, s.requireEvent(t, "ro-table", 3*time.Second))
	if !strings.Contains(after.HTML, "Error") {
		t.Fatal("stream stopped delivering changes after the 410 relist")
	}
}

// TestStreamWatchlessKind204 pins the watch-less taxonomy: a kind without
// the watch verb (the metrics pseudo-type pods printer) gets 204 — the
// client falls back to polling silently.
func TestStreamWatchlessKind204(t *testing.T) {
	ts, _ := newStreamFixture(t)
	resp := dialStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream?g=1&apiVersion=metrics.k8s.io/v1beta1")
	defer func() { _ = resp.Body.Close() }()
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
		resp := dialStream(t, ts.URL+path+"?g=1")
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", path, resp.StatusCode)
		}
	}
}

// TestStreamCapAndRelease pins the concurrency cap: the 33rd concurrent
// stream gets 429 BEFORE SSE headers, and closing one stream releases its
// slot so a new 33rd can connect (the deferred-release cleanup contract).
func TestStreamCapAndRelease(t *testing.T) {
	ts, _ := newStreamFixture(t)
	streams := make([]*http.Response, 0, streamCapMax)
	for i := 0; i < streamCapMax; i++ {
		resp := dialStream(t, fmt.Sprintf("%s/clusters/test/namespaces/default/pods/_stream?g=%d", ts.URL, i))
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

	over := dialStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream?g=33")
	_ = over.Body.Close()
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
		retry := dialStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream?g=34")
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

// TestStreamIdleTerminal pins the idle cap (test-injectable package var): a
// stream with no watch data for the cap emits `ro-terminal` reason "idle"
// and closes.
func TestStreamIdleTerminal(t *testing.T) {
	setStreamVar(t, &streamIdleCap, 250*time.Millisecond)
	ts, _ := newStreamFixture(t)
	s := openStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream?g=5")
	s.requireEvent(t, "ro-table", 5*time.Second)
	term := s.requireEvent(t, "ro-terminal", 3*time.Second)
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
	setStreamVar(t, &streamBackoffBase, 20*time.Millisecond)
	setStreamVar(t, &streamBackoffCap, 100*time.Millisecond)

	var mu sync.Mutex
	watchConnects := 0
	ts, fake := newStreamFixtureWithRecorder(t, func(r *http.Request) {
		if r.URL.Query().Get("watch") == "true" {
			mu.Lock()
			watchConnects++
			mu.Unlock()
		}
	})
	s := openStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream?g=6")
	s.requireEvent(t, "ro-table", 5*time.Second)

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

	term := s.requireEvent(t, "ro-terminal", 6*time.Second)
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
	var b streamBackoff
	want := []time.Duration{
		250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second,
		4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second,
	}
	for i, w := range want {
		if got := b.next(); got != w {
			t.Fatalf("backoff attempt %d = %s, want %s", i, got, w)
		}
	}
	b.noteAttempt(streamHealthyReset) // a healthy minute resets the schedule
	if got := b.next(); got != 250*time.Millisecond {
		t.Fatalf("backoff after a healthy attempt = %s, want the 250ms base", got)
	}
	b.noteAttempt(time.Second) // a short-lived attempt must NOT reset
	if got := b.next(); got != 500*time.Millisecond {
		t.Fatalf("backoff after a short attempt = %s, want 500ms (no reset)", got)
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

	s := openStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream?g=8")
	s.requireEvent(t, "ro-table", 5*time.Second)
	term := s.requireEvent(t, "ro-terminal", 3*time.Second)
	if reason := decodeFrame(t, term).Reason; reason != "auth" {
		t.Fatalf("terminal reason = %q, want auth", reason)
	}
	s.requireClosed(t, 2*time.Second)
}

// TestStreamShutdownTerminal pins the shutdown branch: cancelling the
// server's base context (the New() ctx) sends `ro-terminal` reason
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

	s := openStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream?g=9")
	s.requireEvent(t, "ro-table", 5*time.Second)
	cancel()
	term := s.requireEvent(t, "ro-terminal", 3*time.Second)
	if reason := decodeFrame(t, term).Reason; reason != "shutdown" {
		t.Fatalf("terminal reason = %q, want shutdown", reason)
	}
	s.requireClosed(t, 2*time.Second)
}

// TestStreamMaxLifetimeTerminal pins the hard lifetime bound (security
// review, waves E+F): in trusted-headers/none auth modes a stream has no
// per-session expiry, and the idle cap resets on every watch event — without
// a total-lifetime bound a stream runs forever. The streamMaxLifetime package
// var (test-injectable, 12h default) terminates it with reason "idle". The
// 30-minute default idle cap is four orders of magnitude above the injected
// bound, so a terminal arriving within seconds can only be the lifetime
// timer.
func TestStreamMaxLifetimeTerminal(t *testing.T) {
	setStreamVar(t, &streamMaxLifetime, 300*time.Millisecond)
	ts, _ := newStreamFixture(t)
	s := openStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream?g=12")
	s.requireEvent(t, "ro-table", 5*time.Second)
	term := s.requireEvent(t, "ro-terminal", 3*time.Second)
	if reason := decodeFrame(t, term).Reason; reason != "idle" {
		t.Fatalf("terminal reason = %q, want idle", reason)
	}
	s.requireClosed(t, 2*time.Second)
}

// TestStreamOIDCSessionExpiryTerminal pins the session-bound lifetime
// (security review, waves E+F): in OIDC mode the connect-time cookie check is
// the ONLY auth check an SSE stream ever gets, so the stream must not outlive
// the session it was authorized with — at the session's Expires instant the
// server emits `ro-terminal` reason "auth" and closes. The expiry is
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

	value, err := app.sessions.Seal(sessionCookieName, authSession{
		AccessToken: "session-token",
		Expires:     time.Now().Add(2 * time.Second).Unix(),
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/clusters/test/namespaces/default/pods/_stream?g=13", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: value})

	s := openStreamRequest(t, req)
	s.requireEvent(t, "ro-table", 5*time.Second)
	term := s.requireEvent(t, "ro-terminal", 4*time.Second)
	if reason := decodeFrame(t, term).Reason; reason != "auth" {
		t.Fatalf("terminal reason = %q, want auth", reason)
	}
	s.requireClosed(t, 2*time.Second)
}

// TestStreamWriteDeadlineFreesWedgedSlot pins the non-draining-client armor
// (security review, waves E+F): a connected peer that stops READING wedges
// the SSE write (Fprintf/Flush block once TCP buffers fill) — the handler
// never returns to its select loop, no timer can fire, and the deferred cap
// slot leaks until restart. The per-write deadline (streamWriteTimeout,
// test-injectable) turns the wedge into a write error — the normal
// client-gone exit — and the slot releases. The 600-row "big" fixture makes
// each push large enough to fill the loopback buffers within a few frames.
func TestStreamWriteDeadlineFreesWedgedSlot(t *testing.T) {
	setStreamVar(t, &streamWriteTimeout, 250*time.Millisecond)
	fake, err := fakeapi.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	app := newTestServerWithConfig(t, &config.Config{Port: 8080, Clusters: []config.ClusterConnection{{Name: "test", Server: fake.URL}}, DefaultTheme: "dark"})
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
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := fmt.Fprintf(conn, "GET /clusters/test/namespaces/big/pods/_stream?g=wedge HTTP/1.1\r\nHost: readout-test\r\n\r\n"); err != nil {
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
	setStreamVar(t, &streamMetricsPoll, 150*time.Millisecond)
	ts, fake := newStreamFixture(t)
	s := openStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream?g=10&join=metrics")
	initial := decodeFrame(t, s.requireEvent(t, "ro-table", 5*time.Second))
	if !strings.Contains(initial.HTML, "250m") {
		t.Fatal("initial push is missing the merged CPU usage cell (250m)")
	}

	postStreamScript(t, fake.URL, `{"events":[{"path":"/apis/metrics.k8s.io/v1beta1/namespaces/default/pods","type":"MODIFIED","object":{"kind":"PodMetrics","apiVersion":"metrics.k8s.io/v1beta1","metadata":{"name":"nginx","namespace":"default"},"containers":[{"name":"nginx","usage":{"cpu":"900m","memory":"128Mi"}}]}}]}`)

	deadline := time.Now().Add(4 * time.Second)
	for {
		ev := s.requireEvent(t, "ro-table", 4*time.Second)
		if strings.Contains(decodeFrame(t, ev).HTML, "900m") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("metrics sub-poll never surfaced the new usage value")
		}
	}
}

// TestStreamExcludedFromDurationHistogram pins the metrics contract: a
// completed stream request appears in readout_http_requests_total but NEVER
// in the duration histogram (a 30-minute stream is not request latency).
func TestStreamExcludedFromDurationHistogram(t *testing.T) {
	ts, _ := newStreamFixture(t)
	s := openStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream?g=11")
	s.requireEvent(t, "ro-table", 5*time.Second)
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
