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
	"fmt"
	"net/http"
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

// queuedEvent is one script entry plus its application state; the slice of
// these is the playback queue. emit is the prepared watch frame for replay to
// late-connecting watches (data events only) — unexported, so the snapshot
// JSON stays unchanged.
type queuedEvent struct {
	Event           ScriptEvent `json:"event"`
	ResourceVersion string      `json:"resourceVersion,omitempty"`
	Applied         bool        `json:"applied"`

	emit *emission
}

// emission is one prepared watch frame, serialized once at apply time in both
// column variants; connections pick a variant by whether they already sent a
// frame (the first-frame-carries-columns rule). Emissions are immutable after
// build, so conns and the replay path may share them freely.
type emission struct {
	path        string
	withCols    []byte // frame including columnDefinitions (a conn's first frame)
	withoutCols []byte // frame without columnDefinitions (subsequent frames)
	closeAfter  bool   // close the stream after writing (GONE) or silently (EOF)
	replayable  bool   // data events replay to late watches; control entries do not
}

// watchConnBuffer sizes each connection's emission channel. Scripts are
// e2e-scale (tens of events); a full buffer means the conn stopped draining —
// the hub then kills the stream visibly (dead) instead of losing frames
// silently.
const watchConnBuffer = 256

// watchConn is one open ?watch=true connection: a buffered emission channel
// the hub fans events into, plus a dead marker for buffer overflow. sentFrame
// is touched only by the serving goroutine.
type watchConn struct {
	path      string
	ch        chan *emission
	dead      chan struct{}
	deadOnce  sync.Once
	sentFrame bool
}

func newWatchConn(path string) *watchConn {
	return &watchConn{path: path, ch: make(chan *emission, watchConnBuffer), dead: make(chan struct{})}
}

func (c *watchConn) markDead() {
	c.deadOnce.Do(func() { close(c.dead) })
}

// watchHub tracks the scripted-event queue, pending delay timers, and open
// watch connections. generation guards delayed applications across resets.
type watchHub struct {
	mu         sync.Mutex
	generation int
	queue      []queuedEvent
	timers     []*time.Timer
	conns      map[*watchConn]struct{}
}

func newWatchHub() *watchHub {
	return &watchHub{conns: map[*watchConn]struct{}{}}
}

func (h *watchHub) removeConn(c *watchConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.conns, c)
}

// snapshot dumps queue and connection state for GET /__control/watch-script.
func (h *watchHub) snapshot() map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	events := make([]queuedEvent, len(h.queue))
	copy(events, h.queue)
	open := make([]string, 0, len(h.conns))
	for c := range h.conns {
		open = append(open, c.path)
	}
	return map[string]any{
		"generation":  h.generation,
		"events":      events,
		"openWatches": open,
	}
}

// reset stops pending timers, clears the queue, and bumps the generation so a
// timer that already fired cannot apply a stale event afterwards.
func (h *watchHub) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, t := range h.timers {
		t.Stop()
	}
	h.timers = nil
	h.queue = nil
	h.generation++
}

func (h *watchHub) stopTimers() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, t := range h.timers {
		t.Stop()
	}
	h.timers = nil
}

// validateScriptEvent rejects malformed events at POST time so application
// (possibly delayed, far from the POST) can never fail half-way.
func (s *Server) validateScriptEvent(ev *ScriptEvent) error {
	ls := s.store.listStateFor(ev.Path)
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

// enqueueEvent appends the event to the playback queue and applies it to the
// list state and open watch streams: synchronously for DelayMs == 0 (the
// control POST returns only after subsequent LISTs reflect the change), via
// timer otherwise.
func (s *Server) enqueueEvent(ev ScriptEvent) {
	s.watches.mu.Lock()
	generation := s.watches.generation
	index := len(s.watches.queue)
	s.watches.queue = append(s.watches.queue, queuedEvent{Event: ev})
	s.watches.mu.Unlock()

	if ev.DelayMs <= 0 {
		s.applyQueuedEvent(generation, index)
		return
	}
	timer := time.AfterFunc(time.Duration(ev.DelayMs)*time.Millisecond, func() {
		s.applyQueuedEvent(generation, index)
	})
	s.watches.mu.Lock()
	s.watches.timers = append(s.watches.timers, timer)
	s.watches.mu.Unlock()
}

// applyQueuedEvent applies queue entry index: data events mutate the list
// state, every entry type fans a prepared frame (or stream close) out to the
// open watches of its path. watches.mu is held across the WHOLE application,
// generation check included, so a /__control/reset can never reseed the store
// between the check and the apply (the lock order is watches.mu -> store.mu;
// nothing acquires them in reverse). The apply works on deep copies of the
// queued event — one for the store, one for the emitted frame — and writes
// only the scalar Applied/ResourceVersion (plus the immutable emit pointer)
// back: queued maps are never written after enqueue, so the snapshot encoder
// may read them outside watches.mu. Emitted objects are stamped with the
// applied resourceVersion from that scalar, never the queued object.
func (s *Server) applyQueuedEvent(generation, index int) {
	s.watches.mu.Lock()
	defer s.watches.mu.Unlock()
	if generation != s.watches.generation || index >= len(s.watches.queue) {
		return
	}
	entry := &s.watches.queue[index]
	var emit *emission
	switch entry.Event.Type {
	case "EOF":
		emit = &emission{path: entry.Event.Path, closeAfter: true}
	case "GONE":
		frame := goneWatchFrame()
		emit = &emission{path: entry.Event.Path, withCols: frame, withoutCols: frame, closeAfter: true}
	case "BOOKMARK":
		rv, columnsJSON := s.store.bookmarkRV(entry.Event.Path)
		if rv == "" {
			return // path validated at POST time; unreachable
		}
		entry.ResourceVersion = rv
		emit = buildWatchEmission("BOOKMARK", entry.Event.Path, rv, nil, nil, columnsJSON, false)
	default: // ADDED / MODIFIED / DELETED — validated at POST time
		storeEvent, err := cloneScriptEvent(&entry.Event)
		if err != nil {
			return // queued events were JSON-decoded at POST time; a failed round-trip is unreachable
		}
		rv, cells, columnsJSON := s.store.applyScriptEvent(storeEvent)
		if rv == "" {
			return // path validated at POST time; unreachable
		}
		entry.ResourceVersion = rv
		// A second clone for the emitted frame: the first clone's maps are
		// store state now, and the frame must alias neither store nor queue.
		emitEvent, err := cloneScriptEvent(&entry.Event)
		if err != nil {
			return
		}
		emit = buildWatchEmission(entry.Event.Type, entry.Event.Path, rv, cells, emitEvent.Object, columnsJSON, true)
	}
	entry.Applied = true
	entry.emit = emit
	s.deliverLocked(entry.Event.Path, emit)
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
		select {
		case conn.ch <- emit:
		default:
			// The conn stopped draining its buffer (a test bug, not a normal
			// state) — end the stream visibly instead of losing frames.
			conn.markDead()
		}
	}
}

// registerWatch adds the connection to the hub and collects the applied data
// events it must replay: entries whose resourceVersion is strictly above the
// connection's ?resourceVersion= and whose path shares the connection's list
// state. An absent or non-numeric resourceVersion replays nothing
// (live-only). Lock order: watches.mu -> store.mu.
func (s *Server) registerWatch(conn *watchConn, fromRV string) []*emission {
	s.watches.mu.Lock()
	defer s.watches.mu.Unlock()
	s.watches.conns[conn] = struct{}{}
	if fromRV == "" {
		return nil
	}
	from, err := strconv.ParseInt(fromRV, 10, 64)
	if err != nil {
		return nil
	}
	var replay []*emission
	s.store.mu.Lock()
	target := s.store.lists[conn.path]
	if target != nil {
		for i := range s.watches.queue {
			entry := &s.watches.queue[i]
			if !entry.Applied || entry.emit == nil || !entry.emit.replayable {
				continue
			}
			rv, err := strconv.ParseInt(entry.ResourceVersion, 10, 64)
			if err != nil || rv <= from {
				continue
			}
			if s.store.lists[entry.emit.path] == target {
				replay = append(replay, entry.emit)
			}
		}
	}
	s.store.mu.Unlock()
	return replay
}

// cloneScriptEvent deep-copies a scripted event via a JSON round-trip so the
// store never aliases the queued event's maps (see applyQueuedEvent).
func cloneScriptEvent(ev *ScriptEvent) (*ScriptEvent, error) {
	data, err := json.Marshal(ev)
	if err != nil {
		return nil, err
	}
	out := &ScriptEvent{}
	if err := json.Unmarshal(data, out); err != nil {
		return nil, err
	}
	return out, nil
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
	withoutCols, err := json.Marshal(frame)
	if err != nil {
		return &emission{path: path, replayable: replayable} // unreachable: inputs are JSON round-tripped
	}
	withCols := withoutCols
	if len(columnsJSON) > 0 {
		table["columnDefinitions"] = json.RawMessage(columnsJSON)
		if data, err := json.Marshal(frame); err == nil {
			withCols = data
		}
	}
	return &emission{path: path, withCols: withCols, withoutCols: withoutCols, replayable: replayable}
}

// goneWatchFrame is the scripted-410 frame: the in-stream ERROR event the
// real apiserver sends when a watch's resourceVersion expired (a Status with
// reason Expired, code 410). The watch closes right after it.
func goneWatchFrame() []byte {
	frame := map[string]any{
		"type": "ERROR",
		"object": map[string]any{
			"kind":       "Status",
			"apiVersion": "v1",
			"metadata":   map[string]any{},
			"status":     "Failure",
			"message":    "too old resource version: scripted fakeapi GONE",
			"reason":     "Expired",
			"code":       410,
		},
	}
	data, _ := json.Marshal(frame)
	return data
}

// applyScriptEvent mutates the targeted collection state and returns the new
// collection resourceVersion plus the frame material: the EFFECTIVE table row
// cells after application (for MODIFIED-without-cells the kept row cells; for
// DELETED the removed row's last cells), cloned so they alias no store state,
// and the collection's columnDefinitions serialized once (nil for list-only
// collections). The event object is stamped with the resourceVersion when it
// does not carry its own.
func (st *store) applyScriptEvent(ev *ScriptEvent) (rv string, cells []any, columnsJSON []byte) {
	st.mu.Lock()
	defer st.mu.Unlock()
	ls := st.lists[ev.Path]
	if ls == nil {
		return "", nil, nil // validated at POST time; a reset can race a delayed event
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
	return rv, cloneCells(cells), columnsJSON
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
	if s.ctrl.consumeWatch401() {
		writeStatusJSON(w, http.StatusUnauthorized, unauthorizedStatus())
		return
	}
	conn := newWatchConn(path)
	replay := s.registerWatch(conn, r.URL.Query().Get("resourceVersion"))
	defer s.watches.removeConn(conn)
	w.Header().Set("Content-Type", "application/json;stream=watch")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	for _, emit := range replay {
		if writeWatchFrame(w, flusher, conn, emit) {
			return
		}
	}
	for {
		select {
		case emit := <-conn.ch:
			if writeWatchFrame(w, flusher, conn, emit) {
				return
			}
		case <-conn.dead:
			return
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		}
	}
}

// writeWatchFrame writes one watch frame (one JSON object per line) choosing
// the columns variant: the FIRST frame a connection sends carries
// columnDefinitions, subsequent ones do not — mirroring the real apiserver's
// Table watch. Reports whether the stream must close (GONE after its frame,
// EOF without one).
func writeWatchFrame(w http.ResponseWriter, flusher http.Flusher, conn *watchConn, emit *emission) (closeStream bool) {
	data := emit.withoutCols
	if !conn.sentFrame {
		data = emit.withCols
	}
	if len(data) > 0 {
		conn.sentFrame = true
		_, _ = w.Write(data)
		_, _ = w.Write([]byte("\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}
	return emit.closeAfter
}
