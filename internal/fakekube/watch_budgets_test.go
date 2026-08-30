package fakekube

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const budgetTestPodsPath = "/api/v1/namespaces/default/pods"

func newInternalFakeServer(t *testing.T) *Server {
	t.Helper()
	srv, err := New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	return srv
}

func budgetTestEvent(padding int, delayMs int) ScriptEvent {
	return ScriptEvent{
		Path:    budgetTestPodsPath,
		Type:    "MODIFIED",
		DelayMs: delayMs,
		Cells:   []any{"nginx", "1/1", "Running", "0", "10m"},
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]any{
				"name":      "nginx",
				"namespace": "default",
				"annotations": map[string]any{
					"budget.test/padding": strings.Repeat("x", padding),
				},
			},
			"status": map[string]any{"phase": "Running"},
		},
	}
}

func postInternalScript(t *testing.T, srv *Server, events []ScriptEvent) *http.Response {
	t.Helper()
	body, err := json.Marshal(map[string]any{"events": events})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(srv.URL+"/__control/watch-script", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestWatchRetainedBytesEvictAndAdvanceFloor(t *testing.T) {
	srv := newInternalFakeServer(t)
	state := srv.store.listStateFor(budgetTestPodsPath)
	if state == nil {
		t.Fatal("seed pod state is missing")
	}
	srv.store.mu.Lock()
	seedMeta, _ := state.table["metadata"].(map[string]any)
	seedRV, _ := seedMeta["resourceVersion"].(string)
	srv.store.mu.Unlock()

	// Fewer than either count ceiling, but enough owned JSON/wire bytes to cross
	// both independent byte budgets.
	const events = 200
	for i := range events {
		ev := budgetTestEvent(96<<10, 0)
		ev.Cells[3] = strconv.Itoa(i)
		if err := srv.Apply(ev); err != nil {
			t.Fatalf("Apply %d: %v", i, err)
		}
	}

	srv.watches.mu.Lock()
	historyLen := srv.watches.historyLenLocked()
	replayLen := srv.watches.replayLenLocked()
	historyBytes := srv.watches.historyBytes
	replayBytes := srv.watches.replayBytes
	historyDropped := srv.watches.historyDropped
	historyDroppedBytes := srv.watches.historyDroppedBytes
	replayDropped := srv.watches.replayDropped
	replayDroppedBytes := srv.watches.replayDroppedBytes
	floor := srv.watches.replayFloor[state]
	srv.watches.mu.Unlock()

	if historyLen >= watchHistoryCountLimit || replayLen >= watchReplayCountLimit {
		t.Fatalf("byte eviction did not precede count ceilings: history=%d replay=%d", historyLen, replayLen)
	}
	if historyBytes <= 0 || historyBytes > watchHistoryByteLimit {
		t.Fatalf("history bytes = %d, limit %d", historyBytes, watchHistoryByteLimit)
	}
	if replayBytes <= 0 || replayBytes > watchReplayByteLimit {
		t.Fatalf("replay bytes = %d, limit %d", replayBytes, watchReplayByteLimit)
	}
	if historyDropped == 0 || historyDroppedBytes == 0 {
		t.Fatalf("history byte eviction not observed: dropped=%d bytes=%d", historyDropped, historyDroppedBytes)
	}
	if replayDropped == 0 || replayDroppedBytes == 0 {
		t.Fatalf("replay byte eviction not observed: dropped=%d bytes=%d", replayDropped, replayDroppedBytes)
	}
	if floor <= 0 {
		t.Fatal("replay byte eviction did not advance the shared-state floor")
	}

	resp, err := http.Get(srv.URL + budgetTestPodsPath + "?watch=true&resourceVersion=" + seedRV)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("watch below byte-evicted floor status = %d, want 410", resp.StatusCode)
	}

	snapshot := srv.watches.snapshot(srv.store)
	for key, want := range map[string]any{
		"scriptByteLimit":           watchScriptMaxBytes,
		"maxEventBytes":             watchMaxEventBytes,
		"maxDelayMs":                watchMaxDelayMs,
		"pendingByteLimit":          watchPendingByteLimit,
		"historyByteLimit":          watchHistoryByteLimit,
		"replayByteLimit":           watchReplayByteLimit,
		"historyCountLimit":         watchHistoryCountLimit,
		"replayCountLimit":          watchReplayCountLimit,
		"watchConnectionLimit":      watchConnectionLimit,
		"watchQueueCountLimit":      watchConnQueueCountLimit,
		"watchQueueByteLimit":       watchConnQueueByteLimit,
		"watchGlobalQueueByteLimit": watchGlobalQueueByteLimit,
		"watchWriteTimeoutMs":       int(watchWriteTimeout / time.Millisecond),
	} {
		if got := snapshot[key]; got != want {
			t.Errorf("snapshot %s = %v, want %v", key, got, want)
		}
	}
}

// The block above proves the snapshot REPORTS each budget; it cannot prove any
// of them is a sane value, because it compares each constant with itself. These
// are the relationships the fixture's apiserver behaviour actually depends on.
func TestWatchBudgetsHoldTheirRelationships(t *testing.T) {
	for name, value := range map[string]int{
		"watchScriptMaxBytes":       watchScriptMaxBytes,
		"watchMaxEventBytes":        watchMaxEventBytes,
		"watchPendingByteLimit":     watchPendingByteLimit,
		"watchPendingCountLimit":    watchPendingCountLimit,
		"watchHistoryByteLimit":     watchHistoryByteLimit,
		"watchHistoryCountLimit":    watchHistoryCountLimit,
		"watchReplayByteLimit":      watchReplayByteLimit,
		"watchReplayCountLimit":     watchReplayCountLimit,
		"watchConnectionLimit":      watchConnectionLimit,
		"watchConnQueueCountLimit":  watchConnQueueCountLimit,
		"watchConnQueueByteLimit":   watchConnQueueByteLimit,
		"watchGlobalQueueByteLimit": watchGlobalQueueByteLimit,
	} {
		if value <= 0 {
			t.Errorf("%s = %d, want a positive budget", name, value)
		}
	}
	// A retained replay must be able to EXCEED one connection's delivery
	// window: that gap is the only thing that makes the 410-instead-of-partial-
	// replay path reachable, and readout's own relist tests depend on it.
	if watchReplayByteLimit <= watchConnQueueByteLimit {
		t.Fatalf("replay retention %d must exceed the per-connection queue %d, or a reconnect can never be Expired",
			watchReplayByteLimit, watchConnQueueByteLimit)
	}
	// ...and a replay that DOES fit must be deliverable whole by count.
	if watchConnQueueCountLimit < watchReplayCountLimit {
		t.Fatalf("per-connection queue count %d is below the replay ring %d, so a full replay could never be delivered",
			watchConnQueueCountLimit, watchReplayCountLimit)
	}
	if watchGlobalQueueByteLimit < watchConnQueueByteLimit {
		t.Fatalf("global queue %d is below one connection's own budget %d", watchGlobalQueueByteLimit, watchConnQueueByteLimit)
	}
	// One event must fit the pending and history budgets, or nothing is ever
	// queued or retained.
	if watchMaxEventBytes >= watchPendingByteLimit || watchMaxEventBytes >= watchHistoryByteLimit {
		t.Fatalf("one max-size event (%d) does not fit pending (%d) / history (%d)",
			watchMaxEventBytes, watchPendingByteLimit, watchHistoryByteLimit)
	}
	if watchScriptMaxBytes < watchMaxEventBytes {
		t.Fatalf("script body cap %d cannot carry one max-size event %d", watchScriptMaxBytes, watchMaxEventBytes)
	}
	if watchMaxDelayMs != int(watchMaxDelay/time.Millisecond) || watchMaxDelay <= 0 {
		t.Fatalf("delay cap is inconsistent: %d ms vs %s", watchMaxDelayMs, watchMaxDelay)
	}
	if watchWriteTimeout <= 0 {
		t.Fatalf("watch write timeout = %s, want a positive deadline", watchWriteTimeout)
	}
}

func TestWatchPendingByteAdmissionAndSingleEventCap(t *testing.T) {
	srv := newInternalFakeServer(t)
	const padding = 220 << 10

	accepted := 0
	for {
		err := srv.Apply(budgetTestEvent(padding, watchMaxDelayMs))
		if errors.Is(err, errWatchPendingLimit) {
			break
		}
		if err != nil {
			t.Fatalf("Apply pending event %d: %v", accepted, err)
		}
		accepted++
	}
	if accepted == 0 || accepted >= watchPendingCountLimit {
		t.Fatalf("pending byte budget admitted %d events; wanted byte-before-count exhaustion", accepted)
	}

	srv.watches.mu.Lock()
	pendingBefore := len(srv.watches.pending)
	bytesBefore := srv.watches.pendingBytes
	srv.watches.mu.Unlock()
	if bytesBefore <= 0 || bytesBefore > watchPendingByteLimit {
		t.Fatalf("pending bytes = %d, limit %d", bytesBefore, watchPendingByteLimit)
	}

	// The same cumulative budget applies to the HTTP batch path atomically.
	resp := postInternalScript(t, srv, []ScriptEvent{budgetTestEvent(padding, watchMaxDelayMs)})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("HTTP pending byte overflow status = %d, want 429", resp.StatusCode)
	}
	srv.watches.mu.Lock()
	if len(srv.watches.pending) != pendingBefore || srv.watches.pendingBytes != bytesBefore {
		t.Fatalf("rejected HTTP batch changed pending state: count %d->%d bytes %d->%d", pendingBefore, len(srv.watches.pending), bytesBefore, srv.watches.pendingBytes)
	}
	srv.watches.mu.Unlock()

	// One over-cap event is rejected before store mutation or queue admission.
	srv.store.mu.Lock()
	rvBefore := srv.store.rv
	srv.store.mu.Unlock()
	oversized := budgetTestEvent(watchMaxEventBytes+1, 0)
	if err := srv.Apply(oversized); !errors.Is(err, errWatchEventTooLarge) {
		t.Fatalf("oversized Apply error = %v, want errWatchEventTooLarge", err)
	}
	resp = postInternalScript(t, srv, []ScriptEvent{oversized})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized HTTP event status = %d, want 413", resp.StatusCode)
	}
	srv.store.mu.Lock()
	rvAfter := srv.store.rv
	srv.store.mu.Unlock()
	srv.watches.mu.Lock()
	defer srv.watches.mu.Unlock()
	if rvAfter != rvBefore || len(srv.watches.pending) != pendingBefore || srv.watches.pendingBytes != bytesBefore {
		t.Fatalf("oversized event mutated state: rv %d->%d pending %d->%d bytes %d->%d", rvBefore, rvAfter, pendingBefore, len(srv.watches.pending), bytesBefore, srv.watches.pendingBytes)
	}
}

func TestWatchHTTPByteBatchAdmissionIsAtomic(t *testing.T) {
	srv := newInternalFakeServer(t)
	event := budgetTestEvent(64<<10, watchMaxDelayMs)
	prepared, err := srv.prepareScriptEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	eventBytes := int64(len(prepared.data))
	// Leave room for exactly one event. The two-entry HTTP batch must reject
	// both, proving cumulative byte admission happens before the first enqueue.
	prefill := int(watchPendingByteLimit/eventBytes) - 1
	if prefill <= 0 || prefill+2 >= watchPendingCountLimit {
		t.Fatalf("test fixture cannot isolate byte admission: size=%d prefill=%d", eventBytes, prefill)
	}
	for i := 0; i < prefill; i++ {
		if err := srv.Apply(event); err != nil {
			t.Fatalf("prefill Apply %d: %v", i, err)
		}
	}

	srv.watches.mu.Lock()
	pendingBefore := len(srv.watches.pending)
	bytesBefore := srv.watches.pendingBytes
	srv.watches.mu.Unlock()
	remaining := int64(watchPendingByteLimit) - bytesBefore
	if remaining < eventBytes || remaining >= 2*eventBytes {
		t.Fatalf("prefill left %d bytes, want room for one but not two %d-byte events", remaining, eventBytes)
	}

	resp := postInternalScript(t, srv, []ScriptEvent{event, event})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("cumulative HTTP byte overflow status = %d, want 429", resp.StatusCode)
	}
	srv.watches.mu.Lock()
	defer srv.watches.mu.Unlock()
	if len(srv.watches.pending) != pendingBefore || srv.watches.pendingBytes != bytesBefore {
		t.Fatalf("rejected byte batch partially queued: count %d->%d bytes %d->%d", pendingBefore, len(srv.watches.pending), bytesBefore, srv.watches.pendingBytes)
	}
}

func registerBudgetWatch(t *testing.T, srv *Server, path string) *watchConn {
	t.Helper()
	conn := newWatchConn(path)
	stale, accepted := srv.registerWatch(conn, "")
	if stale || !accepted {
		t.Fatalf("registerWatch(%q) = stale %t accepted %t", path, stale, accepted)
	}
	return conn
}

func TestWatchConnectionQueueByteBoundsAndPromptDrop(t *testing.T) {
	srv := newInternalFakeServer(t)
	conn := registerBudgetWatch(t, srv, budgetTestPodsPath)
	data := bytes.Repeat([]byte("q"), 128<<10)
	emit := &emission{withoutCols: data}
	frames := watchConnQueueByteLimit / len(data)
	if frames <= 0 || frames >= watchConnQueueCountLimit {
		t.Fatalf("test frame size cannot isolate byte bound: frames=%d", frames)
	}

	srv.watches.mu.Lock()
	for i := 0; i < frames; i++ {
		if !srv.watches.enqueueEmissionLocked(conn, emit) {
			srv.watches.mu.Unlock()
			t.Fatalf("frame %d rejected below per-connection byte limit", i)
		}
	}
	data[0] = 'z'
	gotConnBytes := conn.queuedBytes
	gotGlobalBytes := srv.watches.watchQueuedBytes
	gotFrames := conn.queuedFrames
	firstOwnedByte := conn.queue[conn.queueHead].data[0]
	srv.watches.mu.Unlock()
	if firstOwnedByte != 'q' {
		t.Fatalf("connection queue retained source emission alias: first byte %q", firstOwnedByte)
	}
	if gotConnBytes != int64(watchConnQueueByteLimit) || gotGlobalBytes != gotConnBytes || gotFrames != frames {
		t.Fatalf("full queue accounting: conn=%d global=%d frames=%d, want %d/%d", gotConnBytes, gotGlobalBytes, gotFrames, watchConnQueueByteLimit, frames)
	}

	srv.watches.mu.Lock()
	accepted := srv.watches.enqueueEmissionLocked(conn, emit)
	dead := conn.dead
	remainingBytes := conn.queuedBytes
	remainingFrames := conn.queuedFrames
	globalBytes := srv.watches.watchQueuedBytes
	droppedBytes := srv.watches.watchQueueDropBytes
	srv.watches.mu.Unlock()
	if accepted || !dead {
		t.Fatalf("overbudget non-reader accepted=%t dead=%t, want prompt close", accepted, dead)
	}
	if remainingBytes != 0 || remainingFrames != 0 || globalBytes != 0 {
		t.Fatalf("overbudget close retained queue: conn=%d/%d global=%d", remainingBytes, remainingFrames, globalBytes)
	}
	if droppedBytes != uint64(watchConnQueueByteLimit) {
		t.Fatalf("dropped queued bytes = %d, want %d", droppedBytes, watchConnQueueByteLimit)
	}
	select {
	case <-conn.deadCh:
	default:
		t.Fatal("overbudget connection dead channel was not closed")
	}
	srv.watches.removeConn(conn)
}

func TestWatchInflightFrameStaysAccountedAcrossReset(t *testing.T) {
	srv := newInternalFakeServer(t)
	conn := registerBudgetWatch(t, srv, budgetTestPodsPath)
	data := bytes.Repeat([]byte("i"), 128<<10)
	srv.watches.mu.Lock()
	if !srv.watches.enqueueEmissionLocked(conn, &emission{withoutCols: data}) {
		srv.watches.mu.Unlock()
		t.Fatal("inflight fixture frame was rejected")
	}
	srv.watches.mu.Unlock()
	frame, ok := srv.nextWatchFrame(nil, conn)
	if !ok || frame == nil {
		t.Fatal("queued fixture frame was not dequeued")
	}

	fresh, err := seedStore()
	if err != nil {
		t.Fatal(err)
	}
	srv.resetWithStore(fresh, nil)
	srv.watches.mu.Lock()
	accounted := srv.watches.watchQueuedBytes
	frames := srv.watches.watchQueuedFrames
	inflight := conn.inflight
	dead := conn.dead
	srv.watches.mu.Unlock()
	if !dead || inflight != frame || accounted != int64(len(data)) || frames != 1 {
		t.Fatalf("reset lost inflight accounting: dead=%t frame=%p/%p bytes=%d frames=%d", dead, inflight, frame, accounted, frames)
	}

	srv.finishWatchFrame(conn, frame, false)
	srv.watches.removeConn(conn)
	srv.watches.mu.Lock()
	defer srv.watches.mu.Unlock()
	if srv.watches.watchQueuedBytes != 0 || srv.watches.watchQueuedFrames != 0 {
		t.Fatalf("finished inflight frame retained accounting: %d/%d", srv.watches.watchQueuedBytes, srv.watches.watchQueuedFrames)
	}
}

func TestWatchGlobalQueueBoundAcrossDisjointWindows(t *testing.T) {
	srv := newInternalFakeServer(t)
	const frameBytes = 128 << 10
	const framesPerConn = 8 // one MiB per disjoint window
	connCount := watchGlobalQueueByteLimit / (frameBytes * framesPerConn)
	conns := make([]*watchConn, 0, connCount+1)
	for i := 0; i < connCount+1; i++ {
		conns = append(conns, registerBudgetWatch(t, srv, budgetTestPodsPath))
	}

	for i := 0; i < connCount; i++ {
		for j := 0; j < framesPerConn; j++ {
			// Each queue receives a distinct owned window; no replay/shared-frame
			// alias can make the global accounting look smaller than real memory.
			data := bytes.Repeat([]byte{byte(i*framesPerConn + j + 1)}, frameBytes)
			srv.watches.mu.Lock()
			ok := srv.watches.enqueueEmissionLocked(conns[i], &emission{withoutCols: data})
			srv.watches.mu.Unlock()
			if !ok {
				t.Fatalf("disjoint window %d/%d rejected below global byte limit", i, j)
			}
		}
	}

	srv.watches.mu.Lock()
	var summed int64
	for _, conn := range conns {
		summed += conn.queuedBytes
	}
	globalBytes := srv.watches.watchQueuedBytes
	globalFrames := srv.watches.watchQueuedFrames
	srv.watches.mu.Unlock()
	wantFrames := connCount * framesPerConn
	if globalBytes != int64(watchGlobalQueueByteLimit) || summed != globalBytes || globalFrames != wantFrames {
		t.Fatalf("global disjoint accounting: bytes=%d sum=%d frames=%d, want %d/%d", globalBytes, summed, globalFrames, watchGlobalQueueByteLimit, wantFrames)
	}
	snapshot := srv.watches.snapshot(srv.store)
	if snapshot["watchQueuedBytes"] != globalBytes || snapshot["watchQueuedFrames"] != globalFrames {
		t.Fatalf("queue observability = %v bytes/%v frames, want %d/%d", snapshot["watchQueuedBytes"], snapshot["watchQueuedFrames"], globalBytes, globalFrames)
	}
	queueSnapshots, ok := snapshot["watchQueues"].([]watchQueueSnapshot)
	if !ok || len(queueSnapshots) != len(conns) {
		t.Fatalf("per-connection queue observability = %#v", snapshot["watchQueues"])
	}
	var observedBytes int64
	for _, queue := range queueSnapshots {
		observedBytes += queue.Bytes
	}
	if observedBytes != globalBytes {
		t.Fatalf("observable per-connection bytes sum = %d, want global %d", observedBytes, globalBytes)
	}

	// The next non-reading connection is closed before a clone is allocated;
	// existing disjoint windows remain within the exact global ceiling.
	srv.watches.mu.Lock()
	overflowAccepted := srv.watches.enqueueEmissionLocked(conns[len(conns)-1], &emission{withoutCols: bytes.Repeat([]byte("x"), frameBytes)})
	afterBytes := srv.watches.watchQueuedBytes
	lastDead := conns[len(conns)-1].dead
	srv.watches.mu.Unlock()
	if overflowAccepted || !lastDead || afterBytes != int64(watchGlobalQueueByteLimit) {
		t.Fatalf("global overflow accepted=%t dead=%t bytes=%d", overflowAccepted, lastDead, afterBytes)
	}

	for _, conn := range conns {
		srv.watches.removeConn(conn)
	}
	srv.watches.mu.Lock()
	defer srv.watches.mu.Unlock()
	if srv.watches.watchQueuedBytes != 0 || srv.watches.watchQueuedFrames != 0 {
		t.Fatalf("connection removal retained frames: %d bytes/%d frames", srv.watches.watchQueuedBytes, srv.watches.watchQueuedFrames)
	}
}

func TestWatchConnectionCountIsBounded(t *testing.T) {
	srv := newInternalFakeServer(t)
	conns := make([]*watchConn, 0, watchConnectionLimit)
	for i := 0; i < watchConnectionLimit; i++ {
		conns = append(conns, registerBudgetWatch(t, srv, budgetTestPodsPath))
	}
	recorder := httptest.NewRecorder()
	srv.serveWatch(recorder, httptest.NewRequest(http.MethodGet, budgetTestPodsPath+"?watch=true", nil), budgetTestPodsPath)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("connection %d HTTP status = %d, want 429", watchConnectionLimit+1, recorder.Code)
	}
	srv.watches.mu.Lock()
	count := len(srv.watches.conns)
	rejects := srv.watches.watchConnRejects
	srv.watches.mu.Unlock()
	if count != watchConnectionLimit || rejects != 1 {
		t.Fatalf("connection bound state: count=%d rejects=%d", count, rejects)
	}
	for _, conn := range conns {
		srv.watches.removeConn(conn)
	}
}

type watchFrameTestWriter struct {
	header       http.Header
	status       int
	deadline     time.Time
	writes       bytes.Buffer
	writeFn      func([]byte) (int, error)
	flushErr     error
	handshake    chan struct{}
	handshakeSet bool
}

func newWatchFrameTestWriter() *watchFrameTestWriter {
	return &watchFrameTestWriter{header: make(http.Header)}
}

func (w *watchFrameTestWriter) Header() http.Header { return w.header }

func (w *watchFrameTestWriter) WriteHeader(status int) { w.status = status }

func (w *watchFrameTestWriter) Write(data []byte) (int, error) {
	if w.writeFn != nil {
		return w.writeFn(data)
	}
	return w.writes.Write(data)
}

func (w *watchFrameTestWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	return nil
}

func (w *watchFrameTestWriter) FlushError() error {
	if w.handshake != nil && !w.handshakeSet {
		close(w.handshake)
		w.handshakeSet = true
	}
	return w.flushErr
}

func TestWriteWatchFrameDeadlineAndErrorPropagation(t *testing.T) {
	errBlocked := errors.New("non-reading writer deadline")
	nonReader := newWatchFrameTestWriter()
	nonReader.writeFn = func([]byte) (int, error) {
		wait := time.Until(nonReader.deadline)
		if wait > 0 {
			timer := time.NewTimer(wait)
			<-timer.C
		}
		return 0, errBlocked
	}
	started := time.Now()
	_, sent, err := writeWatchFrame(nonReader, http.NewResponseController(nonReader), cloneWatchFrame([]byte("frame"), false), 10*time.Millisecond)
	if !errors.Is(err, errBlocked) || sent {
		t.Fatalf("non-reader write = sent %t err %v, want deadline error", sent, err)
	}
	if nonReader.deadline.IsZero() || time.Since(started) > 250*time.Millisecond {
		t.Fatalf("non-reader was not bounded by write deadline: deadline=%v elapsed=%s", nonReader.deadline, time.Since(started))
	}

	short := newWatchFrameTestWriter()
	short.writeFn = func(data []byte) (int, error) { return len(data) - 1, nil }
	if _, sent, err := writeWatchFrame(short, http.NewResponseController(short), cloneWatchFrame([]byte("frame"), false), time.Second); !errors.Is(err, io.ErrShortWrite) || sent {
		t.Fatalf("short write = sent %t err %v, want io.ErrShortWrite", sent, err)
	}

	errWrite := errors.New("writer failed")
	failed := newWatchFrameTestWriter()
	failed.writeFn = func([]byte) (int, error) { return 0, errWrite }
	if _, sent, err := writeWatchFrame(failed, http.NewResponseController(failed), cloneWatchFrame([]byte("frame"), false), time.Second); !errors.Is(err, errWrite) || sent {
		t.Fatalf("write error = sent %t err %v, want propagated writer error", sent, err)
	}

	errFlush := errors.New("flush failed")
	flushFailed := newWatchFrameTestWriter()
	flushFailed.flushErr = errFlush
	if _, sent, err := writeWatchFrame(flushFailed, http.NewResponseController(flushFailed), cloneWatchFrame([]byte("frame"), false), time.Second); !errors.Is(err, errFlush) || sent {
		t.Fatalf("flush error = sent %t err %v, want propagated flush error", sent, err)
	}

	success := newWatchFrameTestWriter()
	closeAfter, sent, err := writeWatchFrame(success, http.NewResponseController(success), cloneWatchFrame([]byte("frame"), true), time.Second)
	if err != nil || !sent || !closeAfter || success.writes.String() != "frame\n" {
		t.Fatalf("successful close frame = close %t sent %t err %v data %q", closeAfter, sent, err, success.writes.String())
	}
}

func TestServeWatchClosesAndReleasesQueueOnWriteError(t *testing.T) {
	srv := newInternalFakeServer(t)
	errWrite := errors.New("socket write failed")
	w := newWatchFrameTestWriter()
	w.handshake = make(chan struct{})
	w.writeFn = func([]byte) (int, error) { return 0, errWrite }
	req := httptest.NewRequest(http.MethodGet, budgetTestPodsPath+"?watch=true", nil)
	done := make(chan struct{})
	go func() {
		srv.serveWatch(w, req, budgetTestPodsPath)
		close(done)
	}()

	select {
	case <-w.handshake:
	case <-time.After(5 * time.Second):
		t.Fatal("watch handshake did not flush")
	}
	if err := srv.Apply(budgetTestEvent(0, 0)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watch handler did not close after write error")
	}
	if w.status != http.StatusOK {
		t.Fatalf("watch handshake status = %d, want 200", w.status)
	}
	srv.watches.mu.Lock()
	defer srv.watches.mu.Unlock()
	if len(srv.watches.conns) != 0 || srv.watches.watchQueuedBytes != 0 || srv.watches.watchQueuedFrames != 0 {
		t.Fatalf("write-error close leaked connection/queue: conns=%d bytes=%d frames=%d", len(srv.watches.conns), srv.watches.watchQueuedBytes, srv.watches.watchQueuedFrames)
	}
}

func TestApplyOwnsDelayedObjectAndCells(t *testing.T) {
	srv := newInternalFakeServer(t)
	nestedCell := map[string]any{"value": "owned-cell"}
	metadata := map[string]any{"name": "nginx", "namespace": "default"}
	status := map[string]any{"phase": "Owned"}
	ev := ScriptEvent{
		Path:    budgetTestPodsPath,
		Type:    "MODIFIED",
		DelayMs: 25,
		Cells:   []any{"nginx", nestedCell, "Owned", "0", "10m"},
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata":   metadata,
			"status":     status,
		},
	}
	if err := srv.Apply(ev); err != nil {
		t.Fatal(err)
	}

	// Mutate caller-owned nested values only after Apply returns. Under -race,
	// any retained alias in pending/snapshot/timer application is reported.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 10_000 {
			metadata["name"] = "caller-mutated-" + strconv.Itoa(i)
			status["phase"] = "caller-mutated"
			ev.Cells[0] = "caller-mutated"
			nestedCell["value"] = i
		}
	}()
	for {
		_ = srv.watches.snapshot(srv.store)
		srv.watches.mu.Lock()
		pending := len(srv.watches.pending)
		srv.watches.mu.Unlock()
		if pending == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	<-done

	srv.store.mu.Lock()
	defer srv.store.mu.Unlock()
	rows, _ := srv.store.lists[budgetTestPodsPath].table["rows"].([]any)
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		obj, _ := row["object"].(map[string]any)
		name, _ := objectKey(obj)
		if name != "nginx" {
			continue
		}
		gotStatus, _ := obj["status"].(map[string]any)
		cells, _ := row["cells"].([]any)
		cellMap, _ := cells[1].(map[string]any)
		if gotStatus["phase"] != "Owned" || cells[0] != "nginx" || cellMap["value"] != "owned-cell" {
			t.Fatalf("stored event observed caller mutations: object=%v cells=%v", obj, cells)
		}
		return
	}
	t.Fatal("owned delayed event did not update nginx")
}

func TestWatchEventSerializationAndDelayValidation(t *testing.T) {
	srv := newInternalFakeServer(t)
	badJSON := budgetTestEvent(0, 0)
	badJSON.Cells = append(badJSON.Cells, math.NaN())
	if err := srv.Apply(badJSON); err == nil {
		t.Fatal("Apply accepted a non-JSON NaN cell")
	}
	cycle := map[string]any{}
	cycle["self"] = cycle
	badJSON = budgetTestEvent(0, 0)
	badJSON.Object["cycle"] = cycle
	if err := srv.Apply(badJSON); err == nil {
		t.Fatal("Apply accepted a cyclic object")
	}

	for _, ev := range []ScriptEvent{
		{Path: budgetTestPodsPath, Type: "BOOKMARK", DelayMs: -1},
		{Path: budgetTestPodsPath, Type: "EOF", DelayMs: watchMaxDelayMs + 1},
		{Path: budgetTestPodsPath, Type: "GONE", DelayMs: math.MaxInt},
	} {
		if err := srv.Apply(ev); err == nil {
			t.Fatalf("Apply accepted invalid delay %d for %s", ev.DelayMs, ev.Type)
		}
		resp := postInternalScript(t, srv, []ScriptEvent{ev})
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("HTTP invalid delay %d for %s status = %d, want 400", ev.DelayMs, ev.Type, resp.StatusCode)
		}
	}

	// The inclusive upper bound converts safely and is admitted.
	if err := srv.Apply(ScriptEvent{Path: budgetTestPodsPath, Type: "EOF", DelayMs: watchMaxDelayMs}); err != nil {
		t.Fatalf("Apply rejected max safe delay: %v", err)
	}
}

func TestPreparedBatchRevalidatesAtomicallyAfterReset(t *testing.T) {
	srv := newInternalFakeServer(t)
	validAfterReset := budgetTestEvent(0, watchMaxDelayMs)
	validAfterReset.Path = "/api/v1/nodes"
	validAfterReset.Object["kind"] = "Node"
	validAfterReset.Object["metadata"] = map[string]any{"name": "kind-control-plane"}
	invalidAfterReset := budgetTestEvent(0, watchMaxDelayMs)
	prepared := make([]preparedScriptEvent, 2)
	var err error
	prepared[0], err = srv.prepareScriptEvent(validAfterReset)
	if err != nil {
		t.Fatalf("prepare first event against old store: %v", err)
	}
	prepared[1], err = srv.prepareScriptEvent(invalidAfterReset)
	if err != nil {
		t.Fatalf("prepare second event against old store: %v", err)
	}

	fresh, err := seedStore()
	if err != nil {
		t.Fatal(err)
	}
	fresh.mu.Lock()
	delete(fresh.lists, budgetTestPodsPath)
	fresh.mu.Unlock()

	entered := make(chan struct{})
	release := make(chan struct{})
	resetDone := make(chan struct{})
	go func() {
		srv.resetWithStore(fresh, func() {
			close(entered)
			<-release
		})
		close(resetDone)
	}()
	<-entered

	enqueueStarted := make(chan struct{})
	enqueueDone := make(chan error, 1)
	go func() {
		close(enqueueStarted)
		enqueueDone <- srv.enqueueEvents(prepared)
	}()
	<-enqueueStarted
	select {
	case err := <-enqueueDone:
		t.Fatalf("prepared batch crossed reset barrier: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case <-resetDone:
	case <-time.After(5 * time.Second):
		t.Fatal("reset did not finish")
	}
	select {
	case err := <-enqueueDone:
		if err == nil || !strings.Contains(err.Error(), "unknown list path") {
			t.Fatalf("post-reset batch revalidation error = %v, want unknown new-store path", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("post-reset batch stayed blocked")
	}

	srv.watches.mu.Lock()
	pending := len(srv.watches.pending)
	timers := len(srv.watches.timers)
	nextID := srv.watches.nextEventID
	srv.watches.mu.Unlock()
	if pending != 0 || timers != 0 || nextID != 0 {
		t.Fatalf("failed revalidation partially admitted batch: pending=%d timers=%d nextID=%d", pending, timers, nextID)
	}
}

func TestResetStoreSwapIsAtomic(t *testing.T) {
	srv := newInternalFakeServer(t)
	if err := srv.Apply(budgetTestEvent(0, 0)); err != nil {
		t.Fatal(err)
	}
	oldState := srv.store.listStateFor(budgetTestPodsPath)
	fresh, err := seedStore()
	if err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	resetDone := make(chan struct{})
	go func() {
		srv.resetWithStore(fresh, func() {
			close(entered)
			<-release
		})
		close(resetDone)
	}()
	<-entered

	applyStarted := make(chan struct{})
	applyDone := make(chan error, 1)
	go func() {
		close(applyStarted)
		applyDone <- srv.Apply(budgetTestEvent(0, 0))
	}()
	registerDone := make(chan struct {
		stale    bool
		accepted bool
	}, 1)
	conn := newWatchConn(budgetTestPodsPath)
	registerStarted := make(chan struct{})
	go func() {
		close(registerStarted)
		stale, accepted := srv.registerWatch(conn, "")
		registerDone <- struct {
			stale    bool
			accepted bool
		}{stale: stale, accepted: accepted}
	}()
	<-applyStarted
	<-registerStarted

	select {
	case err := <-applyDone:
		t.Fatalf("Apply crossed reset barrier before store swap: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case <-registerDone:
		t.Fatal("registerWatch crossed reset barrier before store swap")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case <-resetDone:
	case <-time.After(5 * time.Second):
		t.Fatal("atomic reset did not finish")
	}
	select {
	case err := <-applyDone:
		if err != nil {
			t.Fatalf("Apply after reset: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Apply stayed blocked after reset")
	}
	select {
	case result := <-registerDone:
		if result.stale {
			t.Fatal("fresh-state registration reported stale")
		}
		if !result.accepted {
			t.Fatal("fresh-state registration was rejected")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("registerWatch stayed blocked after reset")
	}
	srv.watches.removeConn(conn)

	currentState := srv.store.listStateFor(budgetTestPodsPath)
	if currentState == oldState {
		t.Fatal("reset retained the old listState pointer")
	}
	srv.watches.mu.Lock()
	defer srv.watches.mu.Unlock()
	for state := range srv.watches.replayFloor {
		if state == oldState {
			t.Fatal("new-generation replay floor retained old listState")
		}
	}
	for _, entry := range srv.watches.replay[srv.watches.replayHead:] {
		if entry.state != currentState {
			t.Fatalf("new-generation replay entry points at stale state %p, current %p", entry.state, currentState)
		}
	}
}

// A reconnect whose retained replay does not fit the delivery window must be
// answered 410 so the consumer relists. Partial replay is silent data loss, and
// it is the exact shape readout's own 410/relist tests depend on this fixture
// to produce.
func TestWatchReplayTooLargeForTheQueueIsExpired(t *testing.T) {
	srv := newInternalFakeServer(t)
	seedRV := budgetTestListRV(t, srv)

	// 24 x 128 KiB is over the 2 MiB per-connection queue but well under the
	// 16 MiB replay retention, so the replay exists and simply does not fit.
	const frames = 24
	for i := range frames {
		if err := srv.Apply(budgetTestEvent(128<<10, 0)); err != nil {
			t.Fatalf("apply replay event %d: %v", i, err)
		}
	}

	conn := newWatchConn(budgetTestPodsPath)
	stale, accepted := srv.registerWatch(conn, seedRV)
	if !stale || accepted {
		t.Fatalf("registerWatch over the queue budget = stale %t accepted %t, want stale and refused", stale, accepted)
	}
	srv.watches.mu.Lock()
	queued := conn.queuedFrames
	registered := len(srv.watches.conns)
	srv.watches.mu.Unlock()
	if queued != 0 || registered != 0 {
		t.Fatalf("an expired reconnect left state behind: queued=%d registered=%d", queued, registered)
	}
}

// The sibling case: a replay that fits is delivered whole, not truncated.
func TestWatchReplayWithinTheQueueIsDeliveredWhole(t *testing.T) {
	srv := newInternalFakeServer(t)
	seedRV := budgetTestListRV(t, srv)

	const frames = 8 // 1 MiB, half the per-connection budget
	for i := range frames {
		if err := srv.Apply(budgetTestEvent(128<<10, 0)); err != nil {
			t.Fatalf("apply replay event %d: %v", i, err)
		}
	}

	conn := newWatchConn(budgetTestPodsPath)
	stale, accepted := srv.registerWatch(conn, seedRV)
	if stale || !accepted {
		t.Fatalf("registerWatch within budget = stale %t accepted %t", stale, accepted)
	}
	defer srv.watches.removeConn(conn)
	srv.watches.mu.Lock()
	queued := conn.queuedFrames
	srv.watches.mu.Unlock()
	if queued != frames {
		t.Fatalf("queued replay frames = %d, want the complete %d", queued, frames)
	}
}

// The count arm of the same preflight. The replay ring itself is capped at
// watchReplayCountLimit, so this bound is only reachable for a connection that
// already holds frames -- exercise it directly rather than leaving it unproven.
func TestWatchReplayCountPreflightRefusesAPartialWindow(t *testing.T) {
	srv := newInternalFakeServer(t)
	conn := registerBudgetWatch(t, srv, budgetTestPodsPath)
	replay := make([]*emission, watchConnQueueCountLimit)
	for i := range replay {
		replay[i] = &emission{withoutCols: []byte("x")}
	}

	srv.watches.mu.Lock()
	defer srv.watches.mu.Unlock()
	if !srv.watches.canQueueReplayLocked(conn, replay) {
		t.Fatal("a replay exactly at the count limit should fit an empty queue")
	}
	conn.queuedFrames = 1
	if srv.watches.canQueueReplayLocked(conn, replay) {
		t.Fatal("a replay past the count limit was accepted, which would truncate it")
	}
}

// /__control/reset is what the serial Playwright suite leans on for cross-spec
// isolation. A delayed event that fires after the swap must not reach the fresh
// store: without the generation guard, reset silently stops meaning isolation.
func TestWatchDelayedEventIsDroppedAcrossReset(t *testing.T) {
	srv := newInternalFakeServer(t)
	if err := srv.Apply(budgetTestEvent(64, 250)); err != nil {
		t.Fatalf("apply delayed event: %v", err)
	}
	srv.watches.mu.Lock()
	timers := len(srv.watches.timers)
	srv.watches.mu.Unlock()
	if timers != 1 {
		t.Fatalf("pending timers before reset = %d, want 1", timers)
	}

	fresh, err := seedStore()
	if err != nil {
		t.Fatal(err)
	}
	srv.resetWithStore(fresh, nil)
	before := budgetTestListRV(t, srv)

	// Well past the delay: the stale timer fires against the old generation.
	deadline := time.Now().Add(2 * time.Second)
	for {
		srv.watches.mu.Lock()
		pending := len(srv.watches.timers)
		history := srv.watches.historyLenLocked()
		srv.watches.mu.Unlock()
		if pending == 0 {
			if history != 0 {
				t.Fatalf("a delayed event from the old generation was applied: history = %d", history)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("delayed event timer never fired: pending = %d", pending)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if after := budgetTestListRV(t, srv); after != before {
		t.Fatalf("reseeded store advanced from %q to %q after the stale timer", before, after)
	}
}

// The history and replay rings reslice once their head passes 256 AND half the
// backing array. Nothing reached that threshold before, so neither the leak it
// closes nor an off-by-one in the reslice was covered.
func TestWatchRingsCompactOnceTheirHeadPassesTheThreshold(t *testing.T) {
	srv := newInternalFakeServer(t)
	// Compaction needs head >= 256 AND head*2 >= len(ring); with a 1024-entry
	// retention that first happens at 2048 applied events.
	const applied = 2100
	for i := range applied {
		if err := srv.Apply(budgetTestEvent(0, 0)); err != nil {
			t.Fatalf("apply event %d: %v", i, err)
		}
	}

	srv.watches.mu.Lock()
	defer srv.watches.mu.Unlock()
	h := srv.watches
	if got := h.historyLenLocked(); got != watchHistoryCountLimit {
		t.Fatalf("retained history = %d, want %d", got, watchHistoryCountLimit)
	}
	if got := h.replayLenLocked(); got != watchReplayCountLimit {
		t.Fatalf("retained replay = %d, want %d", got, watchReplayCountLimit)
	}
	// Compaction ran, so the heads are back near zero and the backing arrays
	// never grew to applied-many slots.
	if h.historyHead >= 256 || len(h.history) > 2*watchHistoryCountLimit {
		t.Fatalf("history ring never compacted: head=%d len=%d", h.historyHead, len(h.history))
	}
	if h.replayHead >= 256 || len(h.replay) > 2*watchReplayCountLimit {
		t.Fatalf("replay ring never compacted: head=%d len=%d", h.replayHead, len(h.replay))
	}
	// The retained window is still the newest events, in order.
	if h.history[h.historyHead].resourceVersion == "" {
		t.Fatal("compacted history head is not a live entry")
	}
}

func budgetTestListRV(t *testing.T, srv *Server) string {
	t.Helper()
	srv.store.mu.Lock()
	defer srv.store.mu.Unlock()
	if srv.store.lists[budgetTestPodsPath] == nil {
		t.Fatalf("no list state for %s", budgetTestPodsPath)
	}
	return strconv.FormatInt(srv.store.rv, 10)
}
