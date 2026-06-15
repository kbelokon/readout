// Package fakeapi is the shared fake Kubernetes apiserver behind readout's
// test suites and the e2e harness. It unifies the two fixture servers that
// previously lived inline in internal/web/server_test.go (recorder-instrumented,
// runtime-patched discovery) and internal/kube/client_test.go (Accept-header
// tracking) behind one constructor:
//
//   - The served state is seeded from a typed Go object graph (basedata.go's
//     baseTestCluster, through Seed/buildStore), so the server works from any
//     working directory (unit tests, the e2e harness binary) with no embedded
//     files. Seeding happens once at New() time and returns errors instead of
//     calling t.Fatal from inside handlers.
//   - Recorder hooks are functional options (WithRequestRecorder,
//     WithDiscoveryRecorder, WithListRecorder, WithLogRecorder).
//   - The fixture store is MUTABLE in-memory state seeded from the JSON
//     files: control-applied watch-script events change subsequent LIST
//     responses. Some collection paths share one state by design, matching the
//     original fixtures (/api/v1/pods serves the same state as
//     /api/v1/namespaces/default/pods), so a mutation through either path is
//     visible through both.
//   - A deterministic control surface for e2e lives under /__control/ (a path
//     prefix no Kubernetes client ever requests; see control.go). It never
//     ships in readout itself.
//   - List endpoints honor ?limit=N with metadata.continue +
//     metadata.remainingItemCount, mirroring the live-probed apiserver shape
//     (sidebar counts consume this later).
//
// Watch requests (?watch=true) stream scripted /__control/watch-script events
// as Table watch frames (columnDefinitions only in a connection's first
// frame), with scripted BOOKMARK/GONE/EOF stream controls and replay of
// applied data events above the connection's ?resourceVersion. See watch.go.
package fakekube

import (
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
)

// Option configures the fake apiserver at construction time.
type Option func(*Server)

// WithRequestRecorder registers fn to run for every Kubernetes API request the
// server handles (data, discovery, and log routes; /__control/ requests are
// excluded). The web suite feeds its Authorization recorder through this hook.
func WithRequestRecorder(fn func(*http.Request)) Option {
	return func(s *Server) { s.requestRecorders = append(s.requestRecorders, fn) }
}

// WithDiscoveryRecorder registers fn for discovery requests only (/api, /apis,
// the group-version documents, and /version). The web suite counts discovery
// round-trips through this hook.
func WithDiscoveryRecorder(fn func(*http.Request)) Option {
	return func(s *Server) { s.discoveryRecorders = append(s.discoveryRecorders, fn) }
}

// WithListRecorder registers fn for every collection (list) request. The kube
// suite captures the Accept header through this hook to assert server-side
// Table negotiation.
func WithListRecorder(fn func(*http.Request)) Option {
	return func(s *Server) { s.listRecorders = append(s.listRecorders, fn) }
}

// WithLogRecorder registers fn for pod-log subresource requests. The web suite
// captures the tailLines query through this hook.
func WithLogRecorder(fn func(*http.Request)) Option {
	return func(s *Server) { s.logRecorders = append(s.logRecorders, fn) }
}

// WithListenAddress pins the server to a fixed host:port (the e2e harness
// needs a deterministic control URL). The default is an ephemeral port.
func WithListenAddress(addr string) Option {
	return func(s *Server) { s.listenAddr = addr }
}

// WithoutControl suppresses the deterministic /__control/ surface. Control is
// registered by DEFAULT (every existing control-driving test constructs via a
// plain New()), so this is the explicit opt-OUT the demo uses to serve a clean
// fake with no control routes. Nothing else constructs the engine with it.
func WithoutControl() Option {
	return func(s *Server) { s.withoutControl = true }
}

// Server is a running fake apiserver. URL carries the base address, matching
// the httptest.Server field the embedded test servers used to expose.
type Server struct {
	URL string

	httpServer     *httptest.Server
	listenAddr     string
	withoutControl bool
	// done releases held-open watch streams before httptest.Server.Close
	// drains outstanding requests (a held watch would otherwise deadlock it).
	done      chan struct{}
	closeOnce sync.Once

	store   *store
	ctrl    *controlState
	watches *watchHub

	// mux is the active route table. It is swappable so Seed() can register
	// routes derived from a typed Cluster graph after construction; the root
	// handler reads it under muxMu on every request.
	muxMu sync.RWMutex
	mux   *http.ServeMux

	requestRecorders   []func(*http.Request)
	discoveryRecorders []func(*http.Request)
	listRecorders      []func(*http.Request)
	logRecorders       []func(*http.Request)
}

// New seeds the in-memory fixture store and starts the server. All fixture
// parsing happens here so a broken fixture is a constructor error, never a
// mid-request failure.
func New(opts ...Option) (*Server, error) {
	s := &Server{
		done:    make(chan struct{}),
		ctrl:    &controlState{},
		watches: newWatchHub(),
	}
	for _, opt := range opts {
		opt(s)
	}
	st, err := seedStore()
	if err != nil {
		return nil, err
	}
	s.store = st

	mux := http.NewServeMux()
	s.registerRoutes(mux)
	s.mux = mux
	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, controlPrefix) {
			for _, fn := range s.requestRecorders {
				fn(r)
			}
		}
		s.muxMu.RLock()
		active := s.mux
		s.muxMu.RUnlock()
		active.ServeHTTP(w, r)
	})

	s.httpServer = httptest.NewUnstartedServer(root)
	if s.listenAddr != "" {
		// Close the default ephemeral listener BEFORE attempting the custom
		// listen so a failed listen does not leak it.
		_ = s.httpServer.Listener.Close()
		listener, err := net.Listen("tcp", s.listenAddr)
		if err != nil {
			return nil, fmt.Errorf("fakeapi: listen on %s: %w", s.listenAddr, err)
		}
		s.httpServer.Listener = listener
	}
	s.httpServer.Start()
	s.URL = s.httpServer.URL
	return s, nil
}

// Close releases held watch streams, stops pending scripted-event timers, and
// shuts the HTTP server down. Safe to call more than once.
func (s *Server) Close() {
	s.closeOnce.Do(func() {
		close(s.done)
		s.watches.stopTimers()
		s.httpServer.Close()
	})
}

func (s *Server) registerRoutes(mux *http.ServeMux) {
	for path := range s.store.discovery {
		mux.HandleFunc(path, s.discoveryHandler(path))
	}
	for path := range s.store.lists {
		mux.HandleFunc(path, s.listHandler(path))
	}
	for path := range s.store.objects {
		mux.HandleFunc(path, s.objectHandler(path))
	}
	for path := range s.store.logs {
		mux.HandleFunc(path, s.logHandler(path))
	}
	if !s.withoutControl {
		s.registerControl(mux)
	}
}

func (s *Server) discoveryHandler(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for _, fn := range s.discoveryRecorders {
			fn(r)
		}
		s.store.mu.Lock()
		data := s.store.discovery[path]
		s.store.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}
}

func (s *Server) objectHandler(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		s.store.mu.Lock()
		data := s.store.objects[path]
		s.store.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}
}

func (s *Server) logHandler(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for _, fn := range s.logRecorders {
			fn(r)
		}
		s.store.mu.Lock()
		data := s.store.logs[path]
		s.store.mu.Unlock()
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(data)
	}
}

// listHandler serves a collection route: watch requests are routed to the
// watch surface, the fail-lists control mode short-circuits plain lists, and
// everything else serves the mutable store state (Table or List form by Accept
// negotiation, with ?limit handling).
func (s *Server) listHandler(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for _, fn := range s.listRecorders {
			fn(r)
		}
		if isWatch, _ := strconv.ParseBool(r.URL.Query().Get("watch")); isWatch {
			s.serveWatch(w, r, path)
			return
		}
		if mode := s.ctrl.failListsMode(); mode != "" {
			s.serveListFailure(w, r, mode)
			return
		}
		s.serveList(w, r, path)
	}
}

func (s *Server) serveList(w http.ResponseWriter, r *http.Request, path string) {
	s.store.mu.Lock()
	ls := s.store.lists[path]
	if ls == nil {
		// A registered route with no list state (e.g. after a control reset
		// reseeds the base store while the mux still carries a Seed-only route).
		// Serve a 404 rather than nil-deref responseDoc.
		s.store.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	doc, itemsKey := ls.responseDoc(r.Header.Get("Accept"))
	doc = applyInvolvedObjectFieldSelector(doc, itemsKey, r.URL.Query().Get("fieldSelector"))
	doc = applyLimit(doc, itemsKey, r.URL.Query().Get("limit"))
	data, err := json.Marshal(doc)
	s.store.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

// applyInvolvedObjectFieldSelector narrows an events List response to the items
// matching an involvedObject.* field selector (the detail Events tab fetches
// `client.List(events, fieldSelector=involvedObject.name=<obj>,...)`). A real
// apiserver server-side-filters events this way; the fake honors the name +
// namespace + kind + uid keys (an EMPTY selector value matches any item, so a
// selector key for an object with no uid does not over-filter). Non-event lists
// carry no involvedObject.* selector and pass through untouched.
func applyInvolvedObjectFieldSelector(doc map[string]any, itemsKey, fieldSelector string) map[string]any {
	if doc == nil || fieldSelector == "" || !strings.Contains(fieldSelector, "involvedObject.") {
		return doc
	}
	want := map[string]string{}
	for _, clause := range strings.Split(fieldSelector, ",") {
		kv := strings.SplitN(clause, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key, val := strings.TrimSpace(kv[0]), strings.TrimSpace(kv[1])
		if val != "" && strings.HasPrefix(key, "involvedObject.") {
			want[strings.TrimPrefix(key, "involvedObject.")] = val
		}
	}
	if len(want) == 0 {
		return doc
	}
	items, _ := doc[itemsKey].([]any)
	kept := make([]any, 0, len(items))
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		ref, _ := m["involvedObject"].(map[string]any)
		if involvedObjectMatches(ref, want) {
			kept = append(kept, it)
		}
	}
	out := maps.Clone(doc)
	out[itemsKey] = kept
	return out
}

// involvedObjectMatches reports whether an event's involvedObject map satisfies
// every wanted field (name/namespace/kind/uid).
func involvedObjectMatches(ref map[string]any, want map[string]string) bool {
	for field, val := range want {
		got, _ := ref[field].(string)
		if got != val {
			return false
		}
	}
	return true
}

// listState is the mutable state behind one or more collection routes. table
// holds the meta.k8s.io Table form, list the plain List form; either may be
// absent, mirroring the original per-route fixture wiring.
type listState struct {
	table map[string]any
	list  map[string]any
}

// responseDoc picks the served form: the Table form when the client negotiated
// as=Table (or when the route only has a Table form), the List form otherwise.
// This reproduces the embedded servers' tableOrList and unconditional-fixture
// behaviors exactly.
func (ls *listState) responseDoc(accept string) (map[string]any, string) {
	if ls.table != nil && (ls.list == nil || strings.Contains(accept, "as=Table")) {
		return ls.table, "rows"
	}
	return ls.list, "items"
}

// applyLimit implements the apiserver's chunked-list shape: when ?limit=N is
// present and N < total, the response carries the first N entries plus
// metadata.continue and metadata.remainingItemCount (the sidebar-count
// contract probed live: limit=1 => remainingItemCount = total-1).
func applyLimit(doc map[string]any, itemsKey, limitValue string) map[string]any {
	if doc == nil || limitValue == "" {
		return doc
	}
	limit, err := strconv.Atoi(limitValue)
	if err != nil || limit <= 0 {
		return doc
	}
	items, _ := doc[itemsKey].([]any)
	if limit >= len(items) {
		return doc
	}
	out := maps.Clone(doc)
	out[itemsKey] = items[:limit]
	meta, _ := doc["metadata"].(map[string]any)
	outMeta := maps.Clone(meta)
	if outMeta == nil {
		outMeta = map[string]any{}
	}
	outMeta["continue"] = "fakeapi-continue"
	outMeta["remainingItemCount"] = int64(len(items) - limit)
	out["metadata"] = outMeta
	return out
}

// store holds the seeded, mutable fixture state. All access goes through mu;
// handlers marshal under the lock so scripted mutations are atomic with reads.
type store struct {
	mu sync.Mutex
	// rv is the monotonic resourceVersion counter for control-applied
	// mutations; it starts above every fixture resourceVersion.
	rv        int64
	discovery map[string][]byte
	objects   map[string][]byte
	logs      map[string][]byte
	lists     map[string]*listState
}

// replaceWith swaps in a freshly seeded state (the /__control/reset path).
// The route set is derived from the same seed, so registered handlers keep
// resolving their paths.
func (st *store) replaceWith(fresh *store) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.discovery = fresh.discovery
	st.objects = fresh.objects
	st.logs = fresh.logs
	st.lists = fresh.lists
	st.rv = fresh.rv
}

func (st *store) listStateFor(path string) *listState {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.lists[path]
}

// seedStore builds the in-memory state served by a default New() (no Seed). It
// runs the SAME typed pipeline Seed(Cluster) uses — validateCluster +
// buildStore — over baseTestCluster() (basedata.go), the typed object graph that
// replaces the 44 hand-JSON base fixtures. The store it returns serves the same
// routes/shapes the embedded-fixture seed did (default pods/services/configmaps/
// secrets/ingresses/cronjobs/jobs/events, states + empty + big namespaces, the
// worker-1 node + PVs, metrics, and discovery), with the literal printer cells
// the suite + e2e assert carried as explicit Table cells. The embedded fixtures
// (Fixture(), //go:embed) stay for the hand-built-mux tests; the base seed no
// longer reads them.
func seedStore() (*store, error) {
	c := baseTestCluster()
	reg := kindRegistry(c.CRDs)
	if err := validateCluster(&c, reg); err != nil {
		return nil, err
	}
	return buildStore(&c, reg)
}
