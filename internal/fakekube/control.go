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
//	/__control/watch-401
//	    Arms a one-shot 401: the next ?watch=true request returns an
//	    Unauthorized Status, then the flag clears.
//	/__control/reset
//	    Reseeds the fixture store and clears every control flag, the script
//	    queue, and pending timers -- spec isolation for e2e runs.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

const controlPrefix = "/__control/"

// controlState carries the toggles the control surface flips.
type controlState struct {
	mu       sync.Mutex
	failMode string // "" (off), "500", or "403"
	watch401 bool
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

func (c *controlState) armWatch401() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.watch401 = true
}

// consumeWatch401 reports whether the one-shot 401 is armed and disarms it.
func (c *controlState) consumeWatch401() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	armed := c.watch401
	c.watch401 = false
	return armed
}

func (c *controlState) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failMode = ""
	c.watch401 = false
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

func (s *Server) handleWatch401(w http.ResponseWriter, _ *http.Request) {
	s.ctrl.armWatch401()
	writeJSON(w, http.StatusOK, map[string]any{"watch401": "armed"})
}

func (s *Server) handleWatchScript(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusOK, s.watches.snapshot())
		return
	}
	var script struct {
		Events []ScriptEvent `json:"events"`
	}
	if err := json.NewDecoder(r.Body).Decode(&script); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "parse script: " + err.Error()})
		return
	}
	for i := range script.Events {
		if err := s.validateScriptEvent(&script.Events[i]); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
	}
	for i := range script.Events {
		s.enqueueEvent(script.Events[i])
	}
	writeJSON(w, http.StatusOK, map[string]any{"queued": len(script.Events)})
}

func (s *Server) handleReset(w http.ResponseWriter, _ *http.Request) {
	fresh, err := seedStore()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.watches.reset()
	s.store.replaceWith(fresh)
	s.ctrl.reset()
	writeJSON(w, http.StatusOK, map[string]any{"reset": true})
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
