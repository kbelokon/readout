package fakekube

// watch.go owns the scripted watch-event queue and the watch request surface:
// the queue, immediate list-state mutation, AND stream playback to open
// ?watch=true connections. Every scripted data event upserts/removes its
// object in the targeted collection's List items and Table rows (so
// subsequent LIST responses reflect it) and is delivered to the path's open
// watches as a Kubernetes watch-wire frame — one JSON object per line,
// {"type": ..., "object": ...} — whose object is a single-row meta.k8s.io
// Table. Mirroring the real apiserver's Table watch, the FIRST frame each
// connection sends carries columnDefinitions and subsequent frames do not
// (consumers cache the first event's columns).
//
// Script entry vocabulary (POST /__control/watch-script):
//
//   - ADDED / MODIFIED / DELETED — data events: mutate the list state and
//     stream a Table frame. DelayMs holds BOTH the state application and the
//     stream emission.
//   - BOOKMARK — advances the collection resourceVersion without touching
//     content and streams a BOOKMARK frame (empty rows; the RV rides the
//     Table's list metadata). Object/Cells are ignored.
//   - GONE — streams a 410 ERROR frame (a Status with reason Expired) to the
//     path's open watches, then closes them. Never mutates list state;
//     Object/Cells are ignored.
//   - EOF — closes the path's open watches cleanly (no frame). Never mutates
//     list state; Object/Cells are ignored.
//
// A watch connecting with ?resourceVersion=N first replays already-applied
// DATA events with resourceVersion > N (the relist-then-rewatch flow), then
// streams live; an absent or non-numeric resourceVersion starts live-only.
// Control entries (GONE/EOF/BOOKMARK) never replay. Paths sharing one list
// state (/api/v1/pods and /api/v1/namespaces/default/pods) receive each
// other's frames.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// ScriptEvent is one scripted watch event posted to /__control/watch-script.
//
//   - Path targets a collection route (it must exist in the fixture store);
//     paths sharing one state (e.g. /api/v1/pods and
//     /api/v1/namespaces/default/pods) are both affected — in their LIST
//     responses and on their open watch streams.
//   - Type is ADDED, MODIFIED or DELETED (data events), or one of the
//     stream-control pseudo-events BOOKMARK, GONE, EOF (see the file header).
//   - DelayMs holds the state application AND the stream emission for race
//     tests; 0 applies synchronously before the control POST returns.
//   - Object is the full object JSON; metadata.name is required for data
//     events and is the upsert/delete key (with metadata.namespace when both
//     sides carry one). Ignored for pseudo-events.
//   - Cells is the Table row for table-backed collections: required for ADDED
//     (a new row cannot render without cells), optional for MODIFIED (absent
//     cells keep the existing row cells), ignored for DELETED and
//     pseudo-events.
type ScriptEvent struct {
	Path    string         `json:"path"`
	Type    string         `json:"type"`
	DelayMs int            `json:"delayMs,omitempty"`
	Cells   []any          `json:"cells,omitempty"`
	Object  map[string]any `json:"object"`
}

// preparedScriptEvent is the admission result: canonical JSON owned by the
// hub, plus an overflow-safe duration validated before any timer is created.
// Keeping pending/history as bytes avoids retaining caller-owned maps and makes
// their memory budgets exact instead of estimating decoded map expansion.
type preparedScriptEvent struct {
	data  []byte
	delay time.Duration
}

// queuedEvent is one owned script entry plus its application state. Pending
// entries and the bounded applied-history queue share this representation.
// Replay emissions live only in replayEntry, so history cannot accidentally
// retain the two serialized watch frames after replay eviction.
type queuedEvent struct {
	id              uint64
	data            []byte
	resourceVersion string
	applied         bool
}

type queuedEventSnapshot struct {
	Event           json.RawMessage `json:"event"`
	ResourceVersion string          `json:"resourceVersion,omitempty"`
	Applied         bool            `json:"applied"`

	id uint64
}

type watchQueueSnapshot struct {
	Path     string `json:"path"`
	Frames   int    `json:"frames"`
	Bytes    int64  `json:"bytes"`
	Pending  int    `json:"pending"`
	InFlight bool   `json:"inFlight"`
	Closing  bool   `json:"closing"`
}

// replayEntry is the minimal reconnect history for one applied data event.
// The shared listState pointer is the identity: namespaced and all-namespace
// aliases backed by the same collection therefore share one replay floor.
type replayEntry struct {
	state *listState
	rv    int64
	emit  *emission
	bytes int64
}

// emission is one prepared watch frame, serialized once at apply time in both
// column variants; connections pick a variant by whether they already sent a
// frame (the first-frame-carries-columns rule). Replay owns these immutable
// buffers; each connection clones only its selected variant into its separately
// byte-budgeted queue.
type emission struct {
	path        string
	withCols    []byte // frame including columnDefinitions (a conn's first frame)
	withoutCols []byte // frame without columnDefinitions (subsequent frames)
	closeAfter  bool   // close the stream after writing (GONE) or silently (EOF)
	replayable  bool   // data events replay to late watches; control entries do not
}

// The count ceilings bound map/slice/timer structural overhead. The byte
// ceilings independently bound every owned variable-sized buffer: at most
// 8 MiB pending canonical JSON + 8 MiB history JSON/RVs + 16 MiB replay wire
// frames. Together the hub stays comfortably below hundreds of MiB even under
// hostile scripts; ordinary demo events are only a few KiB.
const (
	watchPendingCountLimit = 1024
	watchHistoryCountLimit = 1024
	watchReplayCountLimit  = 1024

	watchMaxEventBytes    = 256 << 10
	watchPendingByteLimit = 8 << 20
	watchHistoryByteLimit = 8 << 20
	watchReplayByteLimit  = 16 << 20

	// Every queued frame is a connection-owned exact-capacity byte slice. The
	// per-connection and global ceilings therefore bound actual retained frame
	// memory (not pointer references to replay-owned emissions). With 256
	// connections, the fixed 1024-slot rings add only 2 MiB of pointers; queued
	// frame buffers are capped globally at 16 MiB. Combined with pending,
	// history, and replay, all watch-owned variable buffers stay below 48 MiB.
	watchConnectionLimit      = 256
	watchConnQueueCountLimit  = 1024
	watchConnQueueByteLimit   = 2 << 20
	watchGlobalQueueByteLimit = 16 << 20
	watchWriteTimeout         = 5 * time.Second

	// Delays exist for deterministic race tests, not as a durable scheduler.
	// Validate milliseconds before converting, so multiplication cannot wrap.
	watchMaxDelay   = 10 * time.Minute
	watchMaxDelayMs = int(watchMaxDelay / time.Millisecond)
)

var (
	errWatchPendingLimit  = errors.New("fakeapi watch pending-event budget reached")
	errWatchEventTooLarge = errors.New("fakeapi watch event exceeds size limit")
)

// queuedWatchFrame is one connection-owned frame variant. The hub chooses the
// first-frame-with-columns variant before cloning, so a slow connection owns
// one exact-capacity data buffer per retained frame rather than two aliases of
// a replay emission. `bytes` is len(data), cached for exact accounting.
type queuedWatchFrame struct {
	data       []byte
	bytes      int64
	closeAfter bool
}

// watchConn is one open ?watch=true connection. The fixed ring bounds pointer
// overhead; queuedBytes/queuedFrames include the in-flight frame until its
// write (or error) completes, closing the otherwise-unobserved retention gap
// between dequeue and socket write. All fields except immutable path/channels
// are guarded by watchHub.mu.
type watchConn struct {
	path string

	notify chan struct{}
	deadCh chan struct{}
	dead   bool

	queue        [watchConnQueueCountLimit]*queuedWatchFrame
	queueHead    int
	queueCount   int
	inflight     *queuedWatchFrame
	queuedBytes  int64
	queuedFrames int
	sentFrame    bool
}

func newWatchConn(path string) *watchConn {
	return &watchConn{
		path:   path,
		notify: make(chan struct{}, 1),
		deadCh: make(chan struct{}),
	}
}

// watchHub tracks bounded applied history/replay, pending delayed entries, and
// open watch connections. generation guards delayed applications across
// resets. Timers are keyed by event ID and removed as soon as they fire, so a
// long-running breathing demo retains neither an unbounded event log nor dead
// timer objects.
type watchHub struct {
	mu                  sync.Mutex
	generation          int
	nextEventID         uint64
	history             []*queuedEvent
	historyHead         int
	historyBytes        int64
	historyDropped      uint64
	historyDroppedBytes uint64
	pending             map[uint64]*queuedEvent
	pendingBytes        int64
	timers              map[uint64]*time.Timer
	replay              []replayEntry
	replayHead          int
	replayBytes         int64
	replayDropped       uint64
	replayDroppedBytes  uint64
	replayFloor         map[*listState]int64
	conns               map[*watchConn]struct{}
	watchQueuedBytes    int64
	watchQueuedFrames   int
	watchQueueDrops     uint64
	watchQueueDropBytes uint64
	watchConnRejects    uint64
}

func newWatchHub() *watchHub {
	return &watchHub{
		pending:     map[uint64]*queuedEvent{},
		timers:      map[uint64]*time.Timer{},
		replayFloor: map[*listState]int64{},
		conns:       map[*watchConn]struct{}{},
	}
}

func (h *watchHub) removeConn(c *watchConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closeConnLocked(c, false, true)
	delete(h.conns, c)
}

// closeConnLocked makes a connection stop promptly and releases every pending
// owned frame. Producer/reset closes pass releaseInflight=false because the
// serving goroutine still owns that pointer until its bounded write returns;
// final removeConn runs after finishWatchFrame and releases it defensively.
func (h *watchHub) closeConnLocked(c *watchConn, producerDrop, releaseInflight bool) {
	if !c.dead {
		c.dead = true
		close(c.deadCh)
		if producerDrop {
			h.watchQueueDrops++
		}
	}
	for c.queueCount > 0 {
		frame := c.queue[c.queueHead]
		c.queue[c.queueHead] = nil
		c.queueHead = (c.queueHead + 1) % watchConnQueueCountLimit
		c.queueCount--
		h.releaseQueuedFrameLocked(c, frame, producerDrop)
	}
	if releaseInflight && c.inflight != nil {
		frame := c.inflight
		c.inflight = nil
		h.releaseQueuedFrameLocked(c, frame, false)
	}
}

func (h *watchHub) releaseQueuedFrameLocked(c *watchConn, frame *queuedWatchFrame, dropped bool) {
	if frame == nil {
		return
	}
	c.queuedBytes -= frame.bytes
	c.queuedFrames--
	h.watchQueuedBytes -= frame.bytes
	h.watchQueuedFrames--
	if dropped {
		h.watchQueueDropBytes += uint64(frame.bytes)
	}
}

func watchFrameDataForConn(c *watchConn, emit *emission) []byte {
	if !c.sentFrame && c.queuedFrames == 0 && len(emit.withCols) > 0 {
		return emit.withCols
	}
	return emit.withoutCols
}

func cloneWatchFrame(data []byte, closeAfter bool) *queuedWatchFrame {
	owned := make([]byte, len(data))
	copy(owned, data)
	return &queuedWatchFrame{data: owned, bytes: int64(len(owned)), closeAfter: closeAfter}
}

// enqueueEmissionLocked owns exactly one selected frame copy on behalf of c.
// Both byte ceilings are checked before allocation. Overload closes the slow
// connection and releases its pending frames immediately; at most one bounded
// in-flight frame remains until the write deadline fires.
func (h *watchHub) enqueueEmissionLocked(c *watchConn, emit *emission) bool {
	if c.dead {
		return false
	}
	data := watchFrameDataForConn(c, emit)
	size := int64(len(data))
	if c.queuedFrames >= watchConnQueueCountLimit ||
		size > watchConnQueueByteLimit-c.queuedBytes ||
		size > watchGlobalQueueByteLimit-h.watchQueuedBytes {
		h.closeConnLocked(c, true, false)
		return false
	}
	frame := cloneWatchFrame(data, emit.closeAfter)
	index := (c.queueHead + c.queueCount) % watchConnQueueCountLimit
	c.queue[index] = frame
	c.queueCount++
	c.queuedBytes += frame.bytes
	c.queuedFrames++
	h.watchQueuedBytes += frame.bytes
	h.watchQueuedFrames++
	select {
	case c.notify <- struct{}{}:
	default:
	}
	return true
}

// canQueueReplayLocked preflights the complete reconnect replay without
// allocating or mutating c. A replay larger than the delivery window must be
// answered with 410 so the consumer relists; partial replay is never allowed.
func (h *watchHub) canQueueReplayLocked(c *watchConn, replay []*emission) bool {
	if len(replay) > watchConnQueueCountLimit-c.queuedFrames {
		return false
	}
	var bytes int64
	frames := c.queuedFrames
	sent := c.sentFrame
	for _, emit := range replay {
		data := emit.withoutCols
		if !sent && frames == 0 && len(emit.withCols) > 0 {
			data = emit.withCols
		}
		size := int64(len(data))
		if size > watchConnQueueByteLimit-c.queuedBytes-bytes ||
			size > watchGlobalQueueByteLimit-h.watchQueuedBytes-bytes {
			return false
		}
		bytes += size
		frames++
	}
	return true
}

func (s *Server) nextWatchFrame(ctxDone <-chan struct{}, c *watchConn) (*queuedWatchFrame, bool) {
	for {
		s.watches.mu.Lock()
		if c.dead {
			s.watches.mu.Unlock()
			return nil, false
		}
		if c.queueCount > 0 {
			frame := c.queue[c.queueHead]
			c.queue[c.queueHead] = nil
			c.queueHead = (c.queueHead + 1) % watchConnQueueCountLimit
			c.queueCount--
			c.inflight = frame
			s.watches.mu.Unlock()
			return frame, true
		}
		notify := c.notify
		dead := c.deadCh
		s.watches.mu.Unlock()

		select {
		case <-notify:
		case <-dead:
			return nil, false
		case <-ctxDone:
			return nil, false
		case <-s.done:
			return nil, false
		}
	}
}

func (s *Server) finishWatchFrame(c *watchConn, frame *queuedWatchFrame, sent bool) {
	s.watches.mu.Lock()
	defer s.watches.mu.Unlock()
	if c.inflight != frame {
		return
	}
	c.inflight = nil
	if sent {
		c.sentFrame = true
	}
	s.watches.releaseQueuedFrameLocked(c, frame, false)
}

// snapshot dumps the retained queue and connection state for GET
// /__control/watch-script. `events` keeps its original enqueue-order contract
// for the retained window (including not-yet-applied delayed entries); the
// additional counters make truncation and timer/replay bounds observable.
func (h *watchHub) snapshot(st *store) map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	events := make([]queuedEventSnapshot, 0, h.historyLenLocked()+len(h.pending))
	for _, entry := range h.history[h.historyHead:] {
		events = append(events, entry.snapshot())
	}
	for _, entry := range h.pending {
		events = append(events, entry.snapshot())
	}
	sort.Slice(events, func(i, j int) bool { return events[i].id < events[j].id })
	open := make([]string, 0, len(h.conns))
	queues := make([]watchQueueSnapshot, 0, len(h.conns))
	for c := range h.conns {
		open = append(open, c.path)
		queues = append(queues, watchQueueSnapshot{
			Path:     c.path,
			Frames:   c.queuedFrames,
			Bytes:    c.queuedBytes,
			Pending:  c.queueCount,
			InFlight: c.inflight != nil,
			Closing:  c.dead,
		})
	}
	sort.Strings(open)
	sort.Slice(queues, func(i, j int) bool {
		if queues[i].Path != queues[j].Path {
			return queues[i].Path < queues[j].Path
		}
		if queues[i].Bytes != queues[j].Bytes {
			return queues[i].Bytes < queues[j].Bytes
		}
		return queues[i].Frames < queues[j].Frames
	})
	floors := make(map[string]string)
	st.mu.Lock()
	for path, state := range st.lists {
		if floor := h.replayFloor[state]; floor > 0 {
			floors[path] = strconv.FormatInt(floor, 10)
		}
	}
	st.mu.Unlock()
	return map[string]any{
		"generation":                h.generation,
		"events":                    events,
		"openWatches":               open,
		"watchConnections":          len(h.conns),
		"watchConnectionLimit":      watchConnectionLimit,
		"watchQueues":               queues,
		"watchQueuedFrames":         h.watchQueuedFrames,
		"watchQueuedBytes":          h.watchQueuedBytes,
		"watchQueueCountLimit":      watchConnQueueCountLimit,
		"watchQueueByteLimit":       watchConnQueueByteLimit,
		"watchGlobalQueueByteLimit": watchGlobalQueueByteLimit,
		"watchWriteTimeoutMs":       int(watchWriteTimeout / time.Millisecond),
		"watchQueueDrops":           h.watchQueueDrops,
		"watchQueueDropBytes":       h.watchQueueDropBytes,
		"watchConnectionRejects":    h.watchConnRejects,
		"scriptByteLimit":           watchScriptMaxBytes,
		"maxEventBytes":             watchMaxEventBytes,
		"maxDelayMs":                watchMaxDelayMs,
		"pendingEvents":             len(h.pending),
		"pendingTimers":             len(h.timers),
		"pendingBytes":              h.pendingBytes,
		"pendingCountLimit":         watchPendingCountLimit,
		"pendingByteLimit":          watchPendingByteLimit,
		"retainedHistory":           h.historyLenLocked(),
		"historyBytes":              h.historyBytes,
		"historyCountLimit":         watchHistoryCountLimit,
		"historyByteLimit":          watchHistoryByteLimit,
		"droppedHistory":            h.historyDropped,
		"droppedHistoryBytes":       h.historyDroppedBytes,
		"retainedReplay":            h.replayLenLocked(),
		"replayBytes":               h.replayBytes,
		"replayCountLimit":          watchReplayCountLimit,
		"replayByteLimit":           watchReplayByteLimit,
		"droppedReplay":             h.replayDropped,
		"droppedReplayBytes":        h.replayDroppedBytes,
		"replayFloors":              floors,
	}
}

func (entry *queuedEvent) snapshot() queuedEventSnapshot {
	return queuedEventSnapshot{
		Event:           json.RawMessage(entry.data),
		ResourceVersion: entry.resourceVersion,
		Applied:         entry.applied,
		id:              entry.id,
	}
}

func (entry *queuedEvent) ownedBytes() int64 {
	return int64(len(entry.data) + len(entry.resourceVersion))
}

func (h *watchHub) historyLenLocked() int { return len(h.history) - h.historyHead }
func (h *watchHub) replayLenLocked() int  { return len(h.replay) - h.replayHead }

// resetLocked stops pending timers, clears every retained pointer/buffer, and
// bumps the generation. Caller holds watches.mu; resetWithStore additionally
// holds store.mu so no observer can see reset watches paired with the old store.
func (h *watchHub) resetLocked() {
	for _, t := range h.timers {
		t.Stop()
	}
	// Existing watches belong to the old store generation. Close them and drop
	// their pending owned frames before swapping listState pointers; an in-flight
	// write remains counted until its bounded write returns and removeConn runs.
	for conn := range h.conns {
		h.closeConnLocked(conn, false, false)
	}
	h.nextEventID = 0
	h.history = nil
	h.historyHead = 0
	h.historyBytes = 0
	h.historyDropped = 0
	h.historyDroppedBytes = 0
	clear(h.pending)
	h.pendingBytes = 0
	clear(h.timers)
	h.replay = nil
	h.replayHead = 0
	h.replayBytes = 0
	h.replayDropped = 0
	h.replayDroppedBytes = 0
	clear(h.replayFloor)
	h.generation++
}

func (h *watchHub) stopTimers() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, t := range h.timers {
		t.Stop()
	}
	clear(h.timers)
	clear(h.pending)
	h.pendingBytes = 0
}

// retainHistoryLocked retains canonical event JSON/RV under independent count
// and byte ceilings. History owns no emission pointer; replay is the only
// long-lived owner of serialized watch frames.
func (h *watchHub) retainHistoryLocked(entry *queuedEvent) {
	size := entry.ownedBytes()
	for h.historyLenLocked() > 0 && (h.historyLenLocked() >= watchHistoryCountLimit || size > watchHistoryByteLimit-h.historyBytes) {
		h.dropOldestHistoryLocked()
	}
	if size > watchHistoryByteLimit {
		h.historyDropped++
		h.historyDroppedBytes += uint64(size)
		return
	}
	h.history = append(h.history, entry)
	h.historyBytes += size
}

func (h *watchHub) dropOldestHistoryLocked() {
	entry := h.history[h.historyHead]
	h.history[h.historyHead] = nil
	h.historyHead++
	size := entry.ownedBytes()
	h.historyBytes -= size
	h.historyDropped++
	h.historyDroppedBytes += uint64(size)
	if h.historyHead >= 256 && h.historyHead*2 >= len(h.history) {
		h.history = append([]*queuedEvent(nil), h.history[h.historyHead:]...)
		h.historyHead = 0
	}
}

// retainReplayLocked appends one applied data event under separate wire-byte
// and count ceilings. Every eviction — including a single frame too large to
// retain — advances that shared list state's floor, so reconnect never receives
// an incomplete delta history. A request exactly at the floor remains safe.
func (h *watchHub) retainReplayLocked(entry replayEntry) {
	for h.replayLenLocked() > 0 && (h.replayLenLocked() >= watchReplayCountLimit || entry.bytes > watchReplayByteLimit-h.replayBytes) {
		h.dropOldestReplayLocked()
	}
	if entry.bytes > watchReplayByteLimit {
		h.advanceReplayFloorLocked(entry)
		h.replayDropped++
		h.replayDroppedBytes += uint64(entry.bytes)
		return
	}
	h.replay = append(h.replay, entry)
	h.replayBytes += entry.bytes
}

func (h *watchHub) dropOldestReplayLocked() {
	entry := h.replay[h.replayHead]
	h.replay[h.replayHead] = replayEntry{}
	h.replayHead++
	h.replayBytes -= entry.bytes
	h.advanceReplayFloorLocked(entry)
	h.replayDropped++
	h.replayDroppedBytes += uint64(entry.bytes)
	if h.replayHead >= 256 && h.replayHead*2 >= len(h.replay) {
		h.replay = append([]replayEntry(nil), h.replay[h.replayHead:]...)
		h.replayHead = 0
	}
}

func (h *watchHub) advanceReplayFloorLocked(entry replayEntry) {
	if entry.rv > h.replayFloor[entry.state] {
		h.replayFloor[entry.state] = entry.rv
	}
}

// validateScriptEvent rejects malformed events at POST time so application
// (possibly delayed, far from the POST) can never fail half-way.
func (s *Server) validateScriptEvent(ev *ScriptEvent) error {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	return s.validateScriptEventLocked(ev)
}

// validateScriptEventLocked validates against the currently installed store.
// Caller holds store.mu; enqueueEvents additionally holds watches.mu first so
// a reset cannot swap listState pointers between this check and admission.
func (s *Server) validateScriptEventLocked(ev *ScriptEvent) error {
	if ev.DelayMs < 0 || ev.DelayMs > watchMaxDelayMs {
		return fmt.Errorf("delayMs %d must be between 0 and %d", ev.DelayMs, watchMaxDelayMs)
	}
	ls := s.store.lists[ev.Path]
	if ls == nil {
		return fmt.Errorf("unknown list path %q", ev.Path)
	}
	switch ev.Type {
	case "ADDED", "MODIFIED", "DELETED":
	case "BOOKMARK", "GONE", "EOF":
		// Stream-control pseudo-events never touch list state, so the
		// object/cells requirements below do not apply.
		return nil
	default:
		return fmt.Errorf("event type %q must be ADDED, MODIFIED, DELETED, BOOKMARK, GONE or EOF", ev.Type)
	}
	name, _ := objectKey(ev.Object)
	if name == "" {
		return fmt.Errorf("event for %q has no object.metadata.name", ev.Path)
	}
	if ev.Type == "ADDED" && ls.table != nil && len(ev.Cells) == 0 {
		return fmt.Errorf("ADDED event for table-backed %q requires cells", ev.Path)
	}
	return nil
}

// prepareScriptEvent serializes before admission, enforcing the single-event
// wire cap and producing a canonical deep-owned representation. Validation is
// run on a decode of those exact bytes, so Apply cannot retain or later observe
// caller mutations of Object/Cells and non-JSON values fail before mutation.
func (s *Server) prepareScriptEvent(ev ScriptEvent) (preparedScriptEvent, error) {
	data, err := json.Marshal(ev)
	if err != nil {
		return preparedScriptEvent{}, fmt.Errorf("serialize watch event: %w", err)
	}
	if len(data) > watchMaxEventBytes {
		return preparedScriptEvent{}, fmt.Errorf("%w: got %d bytes, limit %d", errWatchEventTooLarge, len(data), watchMaxEventBytes)
	}
	owned := make([]byte, len(data))
	copy(owned, data)
	var decoded ScriptEvent
	if err := json.Unmarshal(owned, &decoded); err != nil {
		return preparedScriptEvent{}, fmt.Errorf("decode owned watch event: %w", err)
	}
	if err := s.validateScriptEvent(&decoded); err != nil {
		return preparedScriptEvent{}, err
	}
	return preparedScriptEvent{
		data:  owned,
		delay: time.Duration(decoded.DelayMs) * time.Millisecond,
	}, nil
}

func decodePreparedScriptEvent(data []byte) (*ScriptEvent, error) {
	var event ScriptEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return nil, err
	}
	return &event, nil
}

// Apply is the exported in-process mutation/breathing primitive: it validates
// ev, then applies it to the list state and open watch streams exactly as a
// POSTed /__control/watch-script entry would — no HTTP, no control prefix
// (the demo strips /__control/, so the breathing loop drives the engine
// through here). Data events (ADDED/MODIFIED/DELETED) mutate subsequent LIST
// responses and stream a Table frame to open watches; BOOKMARK/GONE/EOF drive
// the stream controls (see the file header). With DelayMs == 0 the call is
// synchronous (subsequent LISTs reflect the change before Apply returns); a
// positive DelayMs schedules the application on a timer.
//
// Each applied data event (and BOOKMARK) bumps the collection resourceVersion
// strictly above store.rv (st.rv++ in applyScriptEvent/bookmarkRV), so watch
// replay ordering holds and an in-process caller can rely on monotonic RVs.
//
// Apply is the SAME machinery handleWatchScript drives, so it is the single
// validate-then-enqueue path for both the in-process driver and the HTTP
// control surface; a malformed event returns an error and never mutates state.
func (s *Server) Apply(ev ScriptEvent) error {
	prepared, err := s.prepareScriptEvent(ev)
	if err != nil {
		return err
	}
	return s.enqueueEvents([]preparedScriptEvent{prepared})
}

// enqueueEvents atomically admits a validated batch under the pending cap,
// then applies its zero-delay entries synchronously in enqueue order. Delayed
// timers are installed while holding watches.mu, so even a very short delay
// cannot fire before it is observable/removable. No entry from an oversized
// batch is enqueued.
func (s *Server) enqueueEvents(events []preparedScriptEvent) error {
	s.watches.mu.Lock()
	s.store.mu.Lock()
	// Preparation can race a reset after its first store validation. Decode and
	// revalidate the complete owned batch against the store paired with this
	// watch generation while both locks are held; no entry is admitted if any
	// one became invalid.
	for i := range events {
		event, err := decodePreparedScriptEvent(events[i].data)
		if err != nil {
			s.store.mu.Unlock()
			s.watches.mu.Unlock()
			return fmt.Errorf("decode owned watch event: %w", err)
		}
		if err := s.validateScriptEventLocked(event); err != nil {
			s.store.mu.Unlock()
			s.watches.mu.Unlock()
			return err
		}
		events[i].delay = time.Duration(event.DelayMs) * time.Millisecond
	}
	generation := s.watches.generation
	var batchBytes int64
	for i := range events {
		size := int64(len(events[i].data))
		if size > watchPendingByteLimit-batchBytes {
			s.store.mu.Unlock()
			s.watches.mu.Unlock()
			return errWatchPendingLimit
		}
		batchBytes += size
	}
	if len(events) > watchPendingCountLimit-len(s.watches.pending) || batchBytes > watchPendingByteLimit-s.watches.pendingBytes {
		s.store.mu.Unlock()
		s.watches.mu.Unlock()
		return errWatchPendingLimit
	}
	immediate := make([]uint64, 0, len(events))
	for _, event := range events {
		s.watches.nextEventID++
		id := s.watches.nextEventID
		s.watches.pending[id] = &queuedEvent{id: id, data: event.data}
		s.watches.pendingBytes += int64(len(event.data))
		if event.delay > 0 {
			s.watches.timers[id] = time.AfterFunc(event.delay, func() {
				s.applyQueuedEvent(generation, id)
			})
		} else {
			immediate = append(immediate, id)
		}
	}
	s.store.mu.Unlock()
	s.watches.mu.Unlock()

	for _, id := range immediate {
		s.applyQueuedEvent(generation, id)
	}
	return nil
}

// applyQueuedEvent applies pending entry id: data events mutate the list
// state, every entry type fans a prepared frame (or stream close) out to the
// open watches of its path. watches.mu is held across the WHOLE application,
// generation check included, so a /__control/reset can never reseed the store
// between the check and the apply (the lock order is watches.mu -> store.mu;
// nothing acquires them in reverse). The canonical queued bytes are decoded
// independently for store mutation and frame construction, so neither aliases
// caller memory or the history snapshot.
func (s *Server) applyQueuedEvent(generation int, id uint64) {
	s.watches.mu.Lock()
	defer s.watches.mu.Unlock()
	if generation != s.watches.generation {
		return
	}
	entry, ok := s.watches.pending[id]
	if !ok {
		return
	}
	delete(s.watches.pending, id)
	s.watches.pendingBytes -= int64(len(entry.data))
	delete(s.watches.timers, id)
	event, err := decodePreparedScriptEvent(entry.data)
	if err != nil {
		return // canonical JSON was decoded during preparation; unreachable
	}
	var emit *emission
	var replayState *listState
	switch event.Type {
	case "EOF":
		emit = &emission{path: event.Path, closeAfter: true}
	case "GONE":
		frame := goneWatchFrame()
		emit = &emission{path: event.Path, withoutCols: frame, closeAfter: true}
	case "BOOKMARK":
		rv, columnsJSON := s.store.bookmarkRV(event.Path)
		if rv == "" {
			return // path validated at POST time; unreachable
		}
		entry.resourceVersion = rv
		emit = buildWatchEmission("BOOKMARK", event.Path, rv, nil, nil, columnsJSON, false)
	default: // ADDED / MODIFIED / DELETED — validated at POST time
		// This decode is owned by the store after applyScriptEvent. Decode again
		// for the frame so store state and emission construction never alias.
		storeEvent := event
		rv, cells, columnsJSON, state := s.store.applyScriptEvent(storeEvent)
		if rv == "" {
			return // path validated at POST time; unreachable
		}
		replayState = state
		entry.resourceVersion = rv
		emitEvent, err := decodePreparedScriptEvent(entry.data)
		if err != nil {
			return
		}
		emit = buildWatchEmission(event.Type, event.Path, rv, cells, emitEvent.Object, columnsJSON, true)
	}
	entry.applied = true
	s.watches.retainHistoryLocked(entry)
	if emit.replayable {
		rv, err := strconv.ParseInt(entry.resourceVersion, 10, 64)
		if err == nil && replayState != nil {
			s.watches.retainReplayLocked(replayEntry{
				state: replayState,
				rv:    rv,
				emit:  emit,
				bytes: emit.ownedBytes(),
			})
		}
	}
	s.deliverLocked(event.Path, emit)
}

// deliverLocked fans the emission out to the open watch connections whose
// path resolves to the same list state as the event path (path aliases share
// one state, so /api/v1/pods watches receive /api/v1/namespaces/default/pods
// events and vice versa). Caller holds watches.mu.
func (s *Server) deliverLocked(path string, emit *emission) {
	s.store.mu.Lock()
	target := s.store.lists[path]
	receivers := make([]*watchConn, 0, len(s.watches.conns))
	if target != nil {
		for conn := range s.watches.conns {
			if s.store.lists[conn.path] == target {
				receivers = append(receivers, conn)
			}
		}
	}
	s.store.mu.Unlock()
	for _, conn := range receivers {
		s.watches.enqueueEmissionLocked(conn, emit)
	}
}

// registerWatch adds the connection to the hub and atomically queues the
// applied data events it must replay: entries whose resourceVersion is strictly
// above ?resourceVersion= and whose path shares the connection's list state.
// An absent or non-numeric RV starts live-only. An RV below the retained floor,
// or whose complete replay cannot fit the bounded delivery window, returns
// stale=true without registration so serveWatch sends 410 before committing
// watch headers. accepted=false/stale=false is the connection-count 429 path.
// Lock order: watches.mu -> store.mu.
func (s *Server) registerWatch(conn *watchConn, fromRV string) (stale, accepted bool) {
	s.watches.mu.Lock()
	defer s.watches.mu.Unlock()
	if len(s.watches.conns) >= watchConnectionLimit {
		s.watches.watchConnRejects++
		return false, false
	}
	from, parseErr := strconv.ParseInt(fromRV, 10, 64)
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	target := s.store.lists[conn.path]
	if fromRV != "" && parseErr == nil && target != nil && from < s.watches.replayFloor[target] {
		return true, false
	}

	if fromRV == "" || parseErr != nil || target == nil {
		s.watches.conns[conn] = struct{}{}
		return false, true
	}
	replay := make([]*emission, 0)
	for _, entry := range s.watches.replay[s.watches.replayHead:] {
		if entry.state == target && entry.rv > from {
			replay = append(replay, entry.emit)
		}
	}
	if !s.watches.canQueueReplayLocked(conn, replay) {
		return true, false
	}
	s.watches.conns[conn] = struct{}{}
	for _, emit := range replay {
		if !s.watches.enqueueEmissionLocked(conn, emit) {
			// The complete replay was preflighted under the same lock; unreachable.
			s.watches.closeConnLocked(conn, false, true)
			delete(s.watches.conns, conn)
			return true, false
		}
	}
	return false, true
}

// buildWatchEmission serializes one watch frame in both column variants. The
// object (nil for bookmarks) rides a single-row Table whose list metadata
// carries the applied resourceVersion; the row object is stamped with that
// resourceVersion when the script author did not provide an explicit one
// (mirroring the store-side stamp).
func buildWatchEmission(evType, path, rv string, cells []any, obj map[string]any, columnsJSON []byte, replayable bool) *emission {
	table := map[string]any{
		"kind":       "Table",
		"apiVersion": "meta.k8s.io/v1",
		"metadata":   map[string]any{"resourceVersion": rv},
		"rows":       []any{},
	}
	if obj != nil {
		if meta, ok := obj["metadata"].(map[string]any); ok && meta["resourceVersion"] == nil {
			meta["resourceVersion"] = rv
		}
		table["rows"] = []any{map[string]any{"cells": cells, "object": obj}}
	}
	frame := map[string]any{"type": evType, "object": table}
	withoutCols, err := marshalOwnedJSON(frame)
	if err != nil {
		return &emission{path: path, replayable: replayable} // unreachable: inputs are JSON round-tripped
	}
	var withCols []byte
	if len(columnsJSON) > 0 {
		table["columnDefinitions"] = json.RawMessage(columnsJSON)
		if data, err := marshalOwnedJSON(frame); err == nil {
			withCols = data
		}
	}
	return &emission{path: path, withCols: withCols, withoutCols: withoutCols, replayable: replayable}
}

func marshalOwnedJSON(value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	owned := make([]byte, len(data))
	copy(owned, data)
	return owned, nil
}

func (e *emission) ownedBytes() int64 {
	if e == nil {
		return 0
	}
	return int64(len(e.path) + len(e.withCols) + len(e.withoutCols))
}

// goneWatchFrame is the scripted-410 frame: the in-stream ERROR event the
// real apiserver sends when a watch's resourceVersion expired (a Status with
// reason Expired, code 410). The watch closes right after it.
func goneWatchFrame() []byte {
	frame := map[string]any{
		"type":   "ERROR",
		"object": expiredWatchStatus("too old resource version: scripted fakeapi GONE"),
	}
	data, _ := marshalOwnedJSON(frame)
	return data
}

func expiredWatchStatus(message string) map[string]any {
	return map[string]any{
		"kind":       "Status",
		"apiVersion": "v1",
		"metadata":   map[string]any{},
		"status":     "Failure",
		"message":    message,
		"reason":     "Expired",
		"code":       410,
	}
}

// applyScriptEvent mutates the targeted collection state and returns the new
// collection resourceVersion plus the frame material: the EFFECTIVE table row
// cells after application (for MODIFIED-without-cells the kept row cells; for
// DELETED the removed row's last cells), cloned so they alias no store state,
// and the collection's columnDefinitions serialized once (nil for list-only
// collections). The event object is stamped with the resourceVersion when it
// does not carry its own.
func (st *store) applyScriptEvent(ev *ScriptEvent) (rv string, cells []any, columnsJSON []byte, state *listState) {
	st.mu.Lock()
	defer st.mu.Unlock()
	ls := st.lists[ev.Path]
	if ls == nil {
		return "", nil, nil, nil // validated at POST time; a reset can race a delayed event
	}
	st.rv++
	rv = strconv.FormatInt(st.rv, 10)
	if meta, ok := ev.Object["metadata"].(map[string]any); ok && meta["resourceVersion"] == nil {
		meta["resourceVersion"] = rv
	}
	name, namespace := objectKey(ev.Object)
	if ls.list != nil {
		applyToItems(ls.list, ev, name, namespace)
		setCollectionResourceVersion(ls.list, rv)
	}
	cells = ev.Cells
	if ls.table != nil {
		cells = applyToRows(ls.table, ev, name, namespace)
		setCollectionResourceVersion(ls.table, rv)
		columnsJSON = marshalColumns(ls.table)
	}
	return rv, cloneCells(cells), columnsJSON, ls
}

// bookmarkRV advances the collection resourceVersion without touching its
// content — the scripted BOOKMARK's only state effect — and returns the new
// RV plus the collection's serialized columnDefinitions.
func (st *store) bookmarkRV(path string) (string, []byte) {
	st.mu.Lock()
	defer st.mu.Unlock()
	ls := st.lists[path]
	if ls == nil {
		return "", nil
	}
	st.rv++
	rv := strconv.FormatInt(st.rv, 10)
	var columnsJSON []byte
	if ls.list != nil {
		setCollectionResourceVersion(ls.list, rv)
	}
	if ls.table != nil {
		setCollectionResourceVersion(ls.table, rv)
		columnsJSON = marshalColumns(ls.table)
	}
	return rv, columnsJSON
}

// cloneCells deep-copies row cells via a JSON round-trip; effective cells may
// alias store row state, and emitted frames must not.
func cloneCells(cells []any) []any {
	if cells == nil {
		return nil
	}
	data, err := json.Marshal(cells)
	if err != nil {
		return nil
	}
	var out []any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil
	}
	return out
}

// marshalColumns serializes a Table document's columnDefinitions (caller
// holds store.mu). Nil when the document has none.
func marshalColumns(table map[string]any) []byte {
	cols, ok := table["columnDefinitions"]
	if !ok {
		return nil
	}
	data, err := json.Marshal(cols)
	if err != nil {
		return nil
	}
	return data
}

// applyToItems upserts/removes the event object in a List document's items.
func applyToItems(doc map[string]any, ev *ScriptEvent, name, namespace string) {
	items, _ := doc["items"].([]any)
	index := -1
	for i, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if objectMatches(obj, name, namespace) {
			index = i
			break
		}
	}
	switch {
	case ev.Type == "DELETED":
		if index >= 0 {
			items = append(items[:index], items[index+1:]...)
		}
	case index >= 0:
		items[index] = ev.Object
	default:
		items = append(items, ev.Object)
	}
	doc["items"] = items
}

// applyToRows upserts/removes the event in a Table document's rows, matching
// rows by their embedded object metadata, and returns the EFFECTIVE row cells
// after application: MODIFIED without cells keeps the existing row cells (an
// object-only update), DELETED returns the removed row's last cells.
func applyToRows(doc map[string]any, ev *ScriptEvent, name, namespace string) []any {
	rows, _ := doc["rows"].([]any)
	index := -1
	for i, item := range rows {
		row, ok := item.(map[string]any)
		if !ok {
			continue
		}
		obj, ok := row["object"].(map[string]any)
		if !ok {
			continue
		}
		if objectMatches(obj, name, namespace) {
			index = i
			break
		}
	}
	if ev.Type == "DELETED" {
		var last []any
		if index >= 0 {
			if old, ok := rows[index].(map[string]any); ok {
				last, _ = old["cells"].([]any)
			}
			rows = append(rows[:index], rows[index+1:]...)
		}
		doc["rows"] = rows
		return last
	}
	row := map[string]any{"cells": ev.Cells, "object": ev.Object}
	if index >= 0 {
		if old, ok := rows[index].(map[string]any); ok && len(ev.Cells) == 0 {
			row["cells"] = old["cells"]
		}
		rows[index] = row
	} else {
		rows = append(rows, row)
	}
	doc["rows"] = rows
	cells, _ := row["cells"].([]any)
	return cells
}

func objectMatches(obj map[string]any, name, namespace string) bool {
	objName, objNamespace := objectKey(obj)
	if objName != name {
		return false
	}
	if namespace != "" && objNamespace != "" && objNamespace != namespace {
		return false
	}
	return true
}

func objectKey(obj map[string]any) (name, namespace string) {
	meta, ok := obj["metadata"].(map[string]any)
	if !ok {
		return "", ""
	}
	name, _ = meta["name"].(string)
	namespace, _ = meta["namespace"].(string)
	return name, namespace
}

func setCollectionResourceVersion(doc map[string]any, rv string) {
	meta, ok := doc["metadata"].(map[string]any)
	if !ok {
		meta = map[string]any{}
		doc["metadata"] = meta
	}
	meta["resourceVersion"] = rv
}

// serveWatch handles ?watch=true on a collection route: an armed one-shot 401
// fires first; otherwise the connection is registered, replays applied data
// events above its ?resourceVersion=, and then streams scripted events live
// until a scripted GONE/EOF closes it, the client goes away, or the server
// closes.
func (s *Server) serveWatch(w http.ResponseWriter, r *http.Request, path string) {
	if s.ctrl.consumeWatch401(path) {
		writeStatusJSON(w, http.StatusUnauthorized, unauthorizedStatus())
		return
	}
	conn := newWatchConn(path)
	fromRV := r.URL.Query().Get("resourceVersion")
	stale, accepted := s.registerWatch(conn, fromRV)
	if stale {
		writeStatusJSON(w, http.StatusGone, expiredWatchStatus(
			fmt.Sprintf("resource version %s is outside the retained fakeapi delivery window", fromRV),
		))
		return
	}
	if !accepted {
		writeStatusJSON(w, http.StatusTooManyRequests, map[string]any{
			"kind":       "Status",
			"apiVersion": "v1",
			"status":     "Failure",
			"message":    "too many fakeapi watch connections",
			"reason":     "TooManyRequests",
			"code":       http.StatusTooManyRequests,
		})
		return
	}
	defer s.watches.removeConn(conn)
	rc := http.NewResponseController(w)
	if err := setWatchWriteDeadline(rc, watchWriteTimeout); err != nil {
		http.Error(w, "watch response writer does not support bounded writes", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json;stream=watch")
	w.WriteHeader(http.StatusOK)
	if err := flushWatchResponse(rc); err != nil {
		return
	}
	for {
		frame, ok := s.nextWatchFrame(r.Context().Done(), conn)
		if !ok {
			return
		}
		closeStream, sent, err := writeWatchFrame(w, rc, frame, watchWriteTimeout)
		s.finishWatchFrame(conn, frame, sent)
		if err != nil || closeStream {
			return
		}
	}
}

// writeWatchFrame writes one already-selected connection-owned frame. Every
// socket write gets a fresh deadline; short writes, write errors, and flush
// errors propagate to serveWatch, whose defer closes and releases the conn.
func writeWatchFrame(w http.ResponseWriter, rc *http.ResponseController, frame *queuedWatchFrame, timeout time.Duration) (closeStream, sent bool, err error) {
	if frame == nil || len(frame.data) == 0 {
		if frame == nil {
			return false, false, nil
		}
		return frame.closeAfter, false, nil
	}
	if err := setWatchWriteDeadline(rc, timeout); err != nil {
		return frame.closeAfter, false, err
	}
	if err := writeWatchBytes(w, frame.data); err != nil {
		return frame.closeAfter, false, err
	}
	if err := writeWatchBytes(w, []byte("\n")); err != nil {
		return frame.closeAfter, false, err
	}
	if err := flushWatchResponse(rc); err != nil {
		return frame.closeAfter, false, err
	}
	return frame.closeAfter, true, nil
}

func writeWatchBytes(w io.Writer, data []byte) error {
	n, err := w.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	return nil
}

func setWatchWriteDeadline(rc *http.ResponseController, timeout time.Duration) error {
	if rc == nil || timeout <= 0 {
		return nil
	}
	return rc.SetWriteDeadline(time.Now().Add(timeout))
}

func flushWatchResponse(rc *http.ResponseController) error {
	if rc == nil {
		return nil
	}
	return rc.Flush()
}
