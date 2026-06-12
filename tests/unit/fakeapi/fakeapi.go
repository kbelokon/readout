// Package fakeapi is the shared fake Kubernetes apiserver behind readout's
// test suites and the e2e harness. It unifies the two fixture servers that
// previously lived inline in internal/web/server_test.go (recorder-instrumented,
// runtime-patched discovery) and internal/kube/client_test.go (Accept-header
// tracking) behind one constructor:
//
//   - Fixtures are //go:embed'd, so the server works from any working
//     directory (unit tests, the e2e harness binary). Fixture loading happens
//     once at New() time and returns errors instead of calling t.Fatal from
//     inside handlers.
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
package fakeapi

import (
	"embed"
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

//go:embed fixtures
var fixturesFS embed.FS

// Fixture returns the raw bytes of an embedded fixture, e.g.
// "data/pods_table.json" or "discovery/api.json".
func Fixture(name string) ([]byte, error) {
	return fixturesFS.ReadFile("fixtures/" + name)
}

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

// Server is a running fake apiserver. URL carries the base address, matching
// the httptest.Server field the embedded test servers used to expose.
type Server struct {
	URL string

	httpServer *httptest.Server
	listenAddr string
	// done releases held-open watch streams before httptest.Server.Close
	// drains outstanding requests (a held watch would otherwise deadlock it).
	done      chan struct{}
	closeOnce sync.Once

	store   *store
	ctrl    *controlState
	watches *watchHub

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
	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, controlPrefix) {
			for _, fn := range s.requestRecorders {
				fn(r)
			}
		}
		mux.ServeHTTP(w, r)
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
	s.registerControl(mux)
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
	doc, itemsKey := ls.responseDoc(r.Header.Get("Accept"))
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

// namespaceDefaultJSON and nodeMetricsJSON carry the two literal payloads the
// web suite's embedded server wrote inline. podMetricsNginxJSON is the
// single-object PodMetrics GET for the nginx render pod (the pod-detail
// containers table fetches it; the item mirrors data/metrics_pods_list.json).
const (
	namespaceDefaultJSON = `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"default","creationTimestamp":"2024-01-01T00:00:00Z","resourceVersion":"1"},"status":{"phase":"Active"}}`
	nodeMetricsJSON      = `{"apiVersion":"metrics.k8s.io/v1beta1","kind":"NodeMetricsList","items":[{"apiVersion":"metrics.k8s.io/v1beta1","kind":"NodeMetrics","metadata":{"name":"worker-1"},"usage":{"cpu":"1","memory":"256Mi"}}]}`
	podMetricsNginxJSON  = `{"kind":"PodMetrics","apiVersion":"metrics.k8s.io/v1beta1","metadata":{"name":"nginx","namespace":"default","creationTimestamp":"2024-03-01T10:00:00Z"},"containers":[{"name":"nginx","usage":{"cpu":"250m","memory":"128Mi"}}]}`
)

// seedStore builds the in-memory state from the embedded fixtures. The route
// map is the union of the two previously embedded servers; the discovery
// patches (events on /api/v1, replicasets on /apis/apps/v1) that the web
// server applied per-request are baked in once here.
func seedStore() (*store, error) {
	st := &store{
		rv:        100000,
		discovery: map[string][]byte{},
		objects:   map[string][]byte{},
		logs:      map[string][]byte{},
		lists:     map[string]*listState{},
	}

	for path, name := range map[string]string{
		"/api":                               "discovery/api.json",
		"/apis":                              "discovery/apis.json",
		"/apis/batch/v1":                     "discovery/apis__batch__v1.json",
		"/apis/cert-manager.io/v1":           "discovery/apis__cert-manager.io__v1.json",
		"/apis/gateway.networking.k8s.io/v1": "discovery/apis__gateway.networking.k8s.io__v1.json",
		"/apis/gateway.networking.k8s.io/v1beta1": "discovery/apis__gateway.networking.k8s.io__v1beta1.json",
		"/apis/metrics.k8s.io/v1beta1":            "discovery/apis__metrics.k8s.io__v1beta1.json",
		"/apis/networking.k8s.io/v1":              "discovery/apis__networking.k8s.io__v1.json",
		"/apis/storage.k8s.io/v1":                 "discovery/apis__storage.k8s.io__v1.json",
		"/version":                                "discovery/version.json",
	} {
		data, err := Fixture(name)
		if err != nil {
			return nil, err
		}
		st.discovery[path] = data
	}

	apiV1, err := patchedDiscovery("discovery/api__v1.json", map[string]any{
		"name":         "events",
		"singularName": "event",
		"namespaced":   true,
		"kind":         "Event",
		"verbs":        []any{"get", "list", "watch"},
		"shortNames":   []any{"ev"},
	})
	if err != nil {
		return nil, err
	}
	st.discovery["/api/v1"] = apiV1
	appsV1, err := patchedDiscovery("discovery/apis__apps__v1.json", map[string]any{
		"name":         "replicasets",
		"singularName": "replicaset",
		"namespaced":   true,
		"kind":         "ReplicaSet",
		"verbs":        []any{"get", "list", "watch"},
		"shortNames":   []any{"rs"},
	})
	if err != nil {
		return nil, err
	}
	st.discovery["/apis/apps/v1"] = appsV1

	for path, name := range map[string]string{
		"/api/v1/namespaces/default/pods/nginx":        "data/render_pod_nginx.json",
		"/api/v1/namespaces/default/secrets/my-secret": "data/render_secret.json",
		"/api/v1/nodes/worker-1":                       "data/render_node.json",
	} {
		data, err := Fixture(name)
		if err != nil {
			return nil, err
		}
		st.objects[path] = data
	}
	st.objects["/api/v1/namespaces/default"] = []byte(namespaceDefaultJSON)
	st.objects["/apis/metrics.k8s.io/v1beta1/namespaces/default/pods/nginx"] = []byte(podMetricsNginxJSON)

	logData, err := Fixture("data/pod_log.txt")
	if err != nil {
		return nil, err
	}
	st.logs["/api/v1/namespaces/default/pods/nginx/log"] = logData

	// Pods in "default" (and the all-namespaces route, which shares the same
	// state): Table + List forms.
	pods, err := newListState("data/pods_table.json", "data/pods_with_node_list.json")
	if err != nil {
		return nil, err
	}
	st.lists["/api/v1/namespaces/default/pods"] = pods
	st.lists["/api/v1/pods"] = pods

	// Pods in "states" exercise the status/ready/restart tones end to end; the
	// resource-list path always negotiates a Table, so only that form exists.
	statesPods, err := newListState("data/pods_states_table.json", "")
	if err != nil {
		return nil, err
	}
	st.lists["/api/v1/namespaces/states/pods"] = statesPods

	// Pods in "empty" return a zero-row Table: the genuinely-EMPTY list state.
	emptyPods, err := newListState("data/table_empty_rows.json", "")
	if err != nil {
		return nil, err
	}
	st.lists["/api/v1/namespaces/empty/pods"] = emptyPods

	// Services in "default" exercise the generic no-status fallback.
	services, err := newListState("data/services_table.json", "")
	if err != nil {
		return nil, err
	}
	st.lists["/api/v1/namespaces/default/services"] = services

	// Events in "default" carry BOTH forms: the Table form feeds the events
	// LIST screen (which negotiates as=Table; the dual-API count/timestamp
	// rows exercise the events decode — a core-shape count=141 aggregate, a
	// series-shape event, a single event, and a tight-burst spread ≤60s) and
	// the List form feeds the detail Events tab (client.List).
	events, err := newListState("data/events_table.json", "data/render_events_nginx.json")
	if err != nil {
		return nil, err
	}
	st.lists["/api/v1/namespaces/default/events"] = events

	secrets, err := newListState("data/render_secrets_table.json", "data/secrets_list.json")
	if err != nil {
		return nil, err
	}
	st.lists["/api/v1/namespaces/default/secrets"] = secrets

	// ConfigMaps in "default" exercise the keys chips: the rows
	// carry FULL ConfigMap objects (data + binaryData) so the `name · size`
	// chips and the +N-keys in-cell expand have real key/size material -- the
	// e2e expand spec clicks the app-config row's +2 keys button.
	configmaps, err := newListState("data/configmaps_table.json", "")
	if err != nil {
		return nil, err
	}
	st.lists["/api/v1/namespaces/default/configmaps"] = configmaps

	// Ingresses in "default" exercise the hosts/+N, pending-address, and TLS
	// cells: full Ingress objects ride each row (spec.tls drives the synthetic
	// TLS column) and the preview-env row carries the literal <pending> address.
	ingresses, err := newListState("data/ingresses_table.json", "")
	if err != nil {
		return nil, err
	}
	st.lists["/apis/networking.k8s.io/v1/namespaces/default/ingresses"] = ingresses

	// CronJobs in "default" exercise the cronjob cells: schedule verbatim,
	// the Suspend boolean (false→Active ok / true→Suspended mute), and the
	// Last Schedule lastrun cell incl. the never-ran <none> → <never>.
	cronjobs, err := newListState("data/cronjobs_table.json", "")
	if err != nil {
		return nil, err
	}
	st.lists["/apis/batch/v1/namespaces/default/cronjobs"] = cronjobs

	// Jobs in "default" exercise the verbatim job statuses: the printer's
	// bare "Failed" refines to the Failed condition's BackoffLimitExceeded,
	// and Completions ride the ready-ratio grammar (1/1 full, 8/10 partial).
	jobs, err := newListState("data/jobs_table.json", "")
	if err != nil {
		return nil, err
	}
	st.lists["/apis/batch/v1/namespaces/default/jobs"] = jobs

	// PersistentVolumes (cluster-scoped) exercise the uuid-shaped names that
	// must never split or truncate, plus the Bound/Released/Failed tones.
	persistentvolumes, err := newListState("data/persistentvolumes_table.json", "")
	if err != nil {
		return nil, err
	}
	st.lists["/api/v1/persistentvolumes"] = persistentvolumes

	// Namespaces carry BOTH forms: the Table form (rows with labels -- the
	// resource-list page negotiates as=Table, and the label-chip click-to-filter
	// e2e spec needs labelled rows) and the List form (the namespace dropdown /
	// palette feed). The Table rows mirror the List items one to one.
	namespaces, err := newListState("data/namespaces_table.json", "data/render_namespaces_list.json")
	if err != nil {
		return nil, err
	}
	st.lists["/api/v1/namespaces"] = namespaces

	// Nodes carry BOTH forms: a REAL nodes Table (kubectl -o wide printer
	// columns incl. External-IP / OS-Image / Kernel-Version, with the full Node
	// object riding each row -- the column-visibility surface and the rich
	// capacity/conditions cells read it) and the node List form consumed by the
	// pods `join=nodes` custom-column join. The Table's worker-1 row matches the
	// /api/v1/nodes/worker-1 object route, so the list page's name click
	// resolves. (Historically the Table form served the namespaces fixture,
	// which decoded as a ZERO-row table -- the nodes list page rendered empty.)
	nodes, err := newListState("data/nodes_table.json", "data/nodes_list.json")
	if err != nil {
		return nil, err
	}
	st.lists["/api/v1/nodes"] = nodes

	// The "big" namespace (list virtualization): 600-row pods + events Tables,
	// generated in bigfixtures.go but ordinary store state like every other
	// fixture -- the windowing e2e matrix (tick-while-windowed, sort, clamp,
	// out-of-window free text) runs on plain LIST responses, no injection.
	st.lists["/api/v1/namespaces/big/pods"] = &listState{table: bigPodsTable()}
	st.lists["/api/v1/namespaces/big/events"] = &listState{table: bigEventsTable()}

	podMetrics, err := newListState("", "data/metrics_pods_list.json")
	if err != nil {
		return nil, err
	}
	st.lists["/apis/metrics.k8s.io/v1beta1/namespaces/default/pods"] = podMetrics
	st.lists["/apis/metrics.k8s.io/v1beta1/pods"] = podMetrics

	nodeMetrics := &listState{}
	if err := json.Unmarshal([]byte(nodeMetricsJSON), &nodeMetrics.list); err != nil {
		return nil, fmt.Errorf("fakeapi: parse node metrics literal: %w", err)
	}
	st.lists["/apis/metrics.k8s.io/v1beta1/nodes"] = nodeMetrics

	return st, nil
}

func newListState(tableFixture, listFixture string) (*listState, error) {
	ls := &listState{}
	if tableFixture != "" {
		doc, err := parseFixture(tableFixture)
		if err != nil {
			return nil, err
		}
		ls.table = doc
	}
	if listFixture != "" {
		doc, err := parseFixture(listFixture)
		if err != nil {
			return nil, err
		}
		ls.list = doc
	}
	return ls, nil
}

func parseFixture(name string) (map[string]any, error) {
	data, err := Fixture(name)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("fakeapi: parse fixture %s: %w", name, err)
	}
	return doc, nil
}

// patchedDiscovery appends one resource to a discovery document's resources
// list, reproducing the web suite's per-request runtime patch at seed time.
func patchedDiscovery(name string, resource map[string]any) ([]byte, error) {
	doc, err := parseFixture(name)
	if err != nil {
		return nil, err
	}
	resources, ok := doc["resources"].([]any)
	if !ok {
		return nil, fmt.Errorf("fakeapi: discovery fixture %s has no resources list", name)
	}
	doc["resources"] = append(resources, resource)
	return json.Marshal(doc)
}
