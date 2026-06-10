package fakeapi

// watch.go owns the scripted watch-event queue and the watch request surface.
// THIS unit ships the queue plus immediate list-state mutation: every scripted
// event upserts/removes its object in the targeted collection's List items and
// Table rows, so subsequent LIST responses reflect it. Open ?watch=true
// requests are registered and held open (a stream for events to land on);
// actual STREAM playback of the queue to those connections is completed by the
// kube watch unit (Unit 25), which consumes watchHub.queue.

import (
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
//     /api/v1/namespaces/default/pods) are both affected.
//   - Type is ADDED, MODIFIED, or DELETED.
//   - DelayMs holds the state application (and, once Unit 25 lands, the
//     stream emission) for race tests; 0 applies synchronously before the
//     control POST returns.
//   - Object is the full object JSON; metadata.name is required and is the
//     upsert/delete key (with metadata.namespace when both sides carry one).
//   - Cells is the Table row for table-backed collections: required for ADDED
//     (a new row cannot render without cells), optional for MODIFIED (absent
//     cells keep the existing row cells), ignored for DELETED.
type ScriptEvent struct {
	Path    string         `json:"path"`
	Type    string         `json:"type"`
	DelayMs int            `json:"delayMs,omitempty"`
	Cells   []any          `json:"cells,omitempty"`
	Object  map[string]any `json:"object"`
}

// queuedEvent is one script entry plus its application state; the slice of
// these is the playback queue Unit 25 streams to open watches.
type queuedEvent struct {
	Event           ScriptEvent `json:"event"`
	ResourceVersion string      `json:"resourceVersion,omitempty"`
	Applied         bool        `json:"applied"`
}

// watchConn is one open ?watch=true connection. Unit 25 attaches the event
// stream; today the registry exists so scripted events have somewhere to land.
type watchConn struct {
	path string
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

func (h *watchHub) addConn(c *watchConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.conns[c] = struct{}{}
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
	default:
		return fmt.Errorf("event type %q must be ADDED, MODIFIED or DELETED", ev.Type)
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
// list state: synchronously for DelayMs == 0 (the control POST returns only
// after subsequent LISTs reflect the change), via timer otherwise.
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

func (s *Server) applyQueuedEvent(generation, index int) {
	s.watches.mu.Lock()
	if generation != s.watches.generation || index >= len(s.watches.queue) {
		s.watches.mu.Unlock()
		return
	}
	event := s.watches.queue[index].Event
	s.watches.mu.Unlock()

	rv := s.store.applyScriptEvent(&event)

	s.watches.mu.Lock()
	if generation == s.watches.generation && index < len(s.watches.queue) {
		s.watches.queue[index].Applied = true
		s.watches.queue[index].ResourceVersion = rv
	}
	s.watches.mu.Unlock()
	// Watch STREAM playback to open connections lands in Unit 25.
}

// applyScriptEvent mutates the targeted collection state and returns the new
// collection resourceVersion. The event object is stamped with that
// resourceVersion when it does not carry its own.
func (st *store) applyScriptEvent(ev *ScriptEvent) string {
	st.mu.Lock()
	defer st.mu.Unlock()
	ls := st.lists[ev.Path]
	if ls == nil {
		return "" // validated at POST time; a reset can race a delayed event
	}
	st.rv++
	rv := strconv.FormatInt(st.rv, 10)
	if meta, ok := ev.Object["metadata"].(map[string]any); ok && meta["resourceVersion"] == nil {
		meta["resourceVersion"] = rv
	}
	name, namespace := objectKey(ev.Object)
	if ls.list != nil {
		applyToItems(ls.list, ev, name, namespace)
		setCollectionResourceVersion(ls.list, rv)
	}
	if ls.table != nil {
		applyToRows(ls.table, ev, name, namespace)
		setCollectionResourceVersion(ls.table, rv)
	}
	return rv
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
// rows by their embedded object metadata. MODIFIED without cells keeps the
// existing row cells (an object-only update).
func applyToRows(doc map[string]any, ev *ScriptEvent, name, namespace string) {
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
		if index >= 0 {
			rows = append(rows[:index], rows[index+1:]...)
		}
		doc["rows"] = rows
		return
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
// fires first; otherwise the connection is registered and held open until the
// client goes away or the server closes. Unit 25 attaches queue playback.
func (s *Server) serveWatch(w http.ResponseWriter, r *http.Request, path string) {
	if s.ctrl.consumeWatch401() {
		writeStatusJSON(w, http.StatusUnauthorized, unauthorizedStatus())
		return
	}
	conn := &watchConn{path: path}
	s.watches.addConn(conn)
	defer s.watches.removeConn(conn)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	select {
	case <-r.Context().Done():
	case <-s.done:
	}
}
