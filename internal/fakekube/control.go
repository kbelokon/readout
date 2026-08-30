package fakekube

// control.go is the e2e determinism surface. Everything lives under
// /__control/ -- a path prefix no Kubernetes client ever requests -- and only
// in this fixture package, never in readout:
//
//	/__control/fail-lists?mode=500|403|off
//	    Until untoggled (mode=off), every plain LIST request fails: mode=500
//	    returns an InternalError Status, mode=403 returns a real apiserver
//	    Forbidden Status naming the verb/resource/namespace (the shape the
//	    forbidden whole-list state consumes). Watch requests are not affected;
//	    /__control/watch-401 owns watch auth failures.
//	/__control/watch-script  (POST a {"events": [...]} script; GET dumps state)
//	    Queues scripted watch events that mutate the in-memory list state --
//	    subsequent LIST responses reflect them immediately (delayMs > 0 holds
//	    the application AND the stream emission for race tests). The queue
//	    also streams to open ?watch=true connections as Table watch frames;
//	    BOOKMARK/GONE/EOF entries drive stream controls. See watch.go.
//	/__control/watch-401[?path=<collection route>]
//	    Arms a one-shot 401: the next ?watch=true request returns an
//	    Unauthorized Status, then the flag clears. With ?path= only a watch on
//	    that EXACT route consumes it -- path aliases (/api/v1/pods and its
//	    namespaced spelling) share one list state and are woken by the same
//	    events, so an unscoped arm is a race between their reconnects.
//	/__control/reset
//	    Reseeds the fixture store and clears every control flag, the script
//	    queue, and pending timers -- spec isolation for e2e runs. Cursors
//	    minted before the reset expire: the fresh collections restart above
//	    every resourceVersion the old ones issued, so a consumer that
//	    reconnects across a reset is answered 410 and relists.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

const controlPrefix = "/__control/"

// watchScriptMaxBytes bounds the whole deterministic mutation request before
// decoding. Per-event and cumulative owned-buffer budgets are enforced again
// after canonical serialization by prepareScriptEvent/enqueueEvents.
const watchScriptMaxBytes = 4 << 20

// controlState carries the toggles the control surface flips.
type controlState struct {
	mu       sync.Mutex
	failMode string // "" (off), "500", or "403"
	watch401 bool
	// watch401Path scopes the armed one-shot to one collection route; "" arms
	// it for whichever watch reconnects first.
	watch401Path string
}

func (c *controlState) setFailMode(mode string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failMode = mode
}

func (c *controlState) failListsMode() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failMode
}

func (c *controlState) armWatch401(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.watch401 = true
	c.watch401Path = path
}

// consumeWatch401 reports whether the one-shot 401 is armed for this watch and
// disarms it. An arm scoped to another route is left in place for its own
// watch.
func (c *controlState) consumeWatch401(path string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.watch401 || (c.watch401Path != "" && c.watch401Path != path) {
		return false
	}
	c.watch401 = false
	c.watch401Path = ""
	return true
}

func (c *controlState) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failMode = ""
	c.watch401 = false
	c.watch401Path = ""
}

func (s *Server) registerControl(mux *http.ServeMux) {
	mux.HandleFunc(controlPrefix+"fail-lists", s.handleFailLists)
	mux.HandleFunc(controlPrefix+"watch-401", s.handleWatch401)
	mux.HandleFunc(controlPrefix+"watch-script", s.handleWatchScript)
	mux.HandleFunc(controlPrefix+"reset", s.handleReset)
}

func (s *Server) handleFailLists(w http.ResponseWriter, r *http.Request) {
	mode := r.URL.Query().Get("mode")
	switch mode {
	case "500", "403":
		s.ctrl.setFailMode(mode)
	case "off":
		s.ctrl.setFailMode("")
		mode = "off"
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "mode must be 500, 403 or off"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"failLists": mode})
}

func (s *Server) handleWatch401(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	s.ctrl.armWatch401(path)
	writeJSON(w, http.StatusOK, map[string]any{"watch401": "armed", "path": path})
}

func (s *Server) handleWatchScript(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusOK, s.watches.snapshot(s.store))
		return
	}
	var script struct {
		Events []ScriptEvent `json:"events"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, watchScriptMaxBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&script); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "watch script body is too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "parse script: " + err.Error()})
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "watch script body is too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "watch script must contain one JSON document"})
		return
	}
	// Canonicalize and validate the WHOLE batch up front so malformed,
	// non-serializable, delayed-too-long, and oversized entries reject the entire
	// script before mutation. enqueueEvents then performs one atomic cumulative
	// byte/count admission for the prepared batch.
	prepared := make([]preparedScriptEvent, len(script.Events))
	for i := range script.Events {
		event, err := s.prepareScriptEvent(script.Events[i])
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, errWatchEventTooLarge) {
				status = http.StatusRequestEntityTooLarge
			}
			writeJSON(w, status, map[string]any{"error": err.Error()})
			return
		}
		prepared[i] = event
	}
	if err := s.enqueueEvents(prepared); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errWatchPendingLimit) {
			status = http.StatusTooManyRequests
		}
		writeJSON(w, status, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"queued": len(script.Events)})
}

func (s *Server) handleReset(w http.ResponseWriter, _ *http.Request) {
	fresh, err := seedStore()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.resetWithStore(fresh, nil)
	s.ctrl.reset()
	writeJSON(w, http.StatusOK, map[string]any{"reset": true})
}

// resetWithStore is the reset transaction. Every watcher/apply/snapshot path
// uses watches.mu -> store.mu; holding both across watch-cache clearing and the
// store pointer swap prevents a reconnect or delayed apply from binding an old
// listState into the new generation. barrier is nil in production and gives the
// race test a deterministic observation point while both locks are held.
func (s *Server) resetWithStore(fresh *store, barrier func()) {
	s.watches.mu.Lock()
	defer s.watches.mu.Unlock()
	s.store.mu.Lock()
	defer s.store.mu.Unlock()

	s.watches.resetLocked()
	if barrier != nil {
		barrier()
	}
	s.store.discovery = fresh.discovery
	s.store.objects = fresh.objects
	s.store.logs = fresh.logs
	s.store.lists = fresh.lists
	// A reset REPLACES the collections: their watch cache is gone, so a cursor
	// minted before it can no longer be served. Start the fresh collections
	// ABOVE every resourceVersion the old ones issued and floor the replay
	// window there, so a consumer reconnecting with a pre-reset cursor is
	// answered 410 Expired and relists -- exactly what a real apiserver does
	// once its watch cache no longer covers the cursor. Without it, a consumer
	// that holds a watch across the reset (readout's WatchHub retains a source
	// for 30s after its last subscriber leaves) would silently keep serving the
	// pre-reset table, and `reset` would stop meaning isolation.
	baseline := s.store.rv + 1
	if fresh.rv > baseline {
		baseline = fresh.rv
	}
	s.store.rv = baseline
	rv := strconv.FormatInt(baseline, 10)
	seen := make(map[*listState]struct{}, len(fresh.lists))
	for _, state := range fresh.lists {
		if _, done := seen[state]; done {
			continue // path aliases (/api/v1/pods and its namespaced spelling) share one state
		}
		seen[state] = struct{}{}
		s.watches.replayFloor[state] = baseline
		// The collection's own metadata must carry the baseline too: a relisting
		// consumer re-watches from the LIST's resourceVersion, and a seeded value
		// below the floor would 410 it straight back into another relist.
		if state.table != nil {
			setCollectionResourceVersion(state.table, rv)
		}
		if state.list != nil {
			setCollectionResourceVersion(state.list, rv)
		}
	}
}

// serveListFailure renders the armed fail-lists mode as a real apiserver
// Status payload: mode 500 is an InternalError, mode 403 the Forbidden Status
// naming the list verb, resource, and namespace of the failing request.
func (s *Server) serveListFailure(w http.ResponseWriter, r *http.Request, mode string) {
	if mode == "403" {
		writeStatusJSON(w, http.StatusForbidden, forbiddenListStatus(r.URL.Path))
		return
	}
	writeStatusJSON(w, http.StatusInternalServerError, map[string]any{
		"kind":       "Status",
		"apiVersion": "v1",
		"status":     "Failure",
		"message":    "Internal error occurred: fakeapi fail-lists mode 500 is active",
		"reason":     "InternalError",
		"code":       500,
	})
}

// forbiddenListStatus builds the apiserver 403 Status for a list request, the
// exact shape the forbidden whole-list state consumes (a Status object with
// reason Forbidden whose message names the verb/resource/namespace).
func forbiddenListStatus(path string) map[string]any {
	plural, namespace := pluralAndNamespace(path)
	message := fmt.Sprintf("%s is forbidden: User %q cannot list resource %q in API group %q", plural, "viewer", plural, "")
	if namespace != "" {
		message += fmt.Sprintf(" in the namespace %q", namespace)
	} else {
		message += " at the cluster scope"
	}
	return map[string]any{
		"kind":       "Status",
		"apiVersion": "v1",
		"status":     "Failure",
		"message":    message,
		"reason":     "Forbidden",
		"details":    map[string]any{"kind": plural},
		"code":       403,
	}
}

func unauthorizedStatus() map[string]any {
	return map[string]any{
		"kind":       "Status",
		"apiVersion": "v1",
		"status":     "Failure",
		"message":    "Unauthorized",
		"reason":     "Unauthorized",
		"code":       401,
	}
}

// pluralAndNamespace derives the resource plural and (when namespaced) the
// namespace from a collection path such as /api/v1/namespaces/default/pods or
// /apis/metrics.k8s.io/v1beta1/pods.
func pluralAndNamespace(path string) (plural, namespace string) {
	segments := strings.Split(strings.Trim(path, "/"), "/")
	if len(segments) == 0 {
		return "", ""
	}
	plural = segments[len(segments)-1]
	if len(segments) >= 3 && segments[len(segments)-3] == "namespaces" {
		namespace = segments[len(segments)-2]
	}
	return plural, namespace
}

func writeStatusJSON(w http.ResponseWriter, code int, status map[string]any) {
	writeJSON(w, code, status)
}

func writeJSON(w http.ResponseWriter, code int, payload map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}
