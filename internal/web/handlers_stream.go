package web

// handlers_stream.go is the server half of Live mode: the read-only
// `GET …/{plural}/_stream` SSE endpoint. It keeps one UNFILTERED per-cluster
// Table snapshot in memory, feeds it from a Table watch (kube.WatchTable), and
// projects render-time list state from it. Clients receive JSON
// snapshot/delta/terminal envelopes in `event: ro-live`. `f`/`sort`/columns
// apply at render time, never to the kube snapshot,
// so an object that starts (or stops) matching the active filter appears (or
// disappears) on the next push.
//
// The lifecycle is complete by contract: clean watch EOF / non-410 errors
// re-watch from the last seen resourceVersion with capped backoff (an EOF
// storm terminates instead of spinning); 410 relists silently and pushes the
// fresh table; auth expiry, the idle cap, a re-watch failure, and server
// shutdown all emit a terminal `event: ro-live` envelope before closing. New
// streams beyond the cap get 429 BEFORE any SSE headers; watch-less kinds get
// 204 (the client falls back to polling). Cleanup is part of the contract:
// the watch reader goroutine and every timer are bound to the request
// context, upstream watch bodies close on every attempt end, and the cap slot
// releases on every handler exit path (deferred at acquisition).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/kbelokon/readout/internal/config"
	"github.com/kbelokon/readout/internal/kube"
)

// streamTuning is the immutable timing policy copied from a Server into each
// Live stream at connect time. Keeping it server-local avoids hidden mutable
// package state: independently constructed servers cannot change one another's
// backoff, lifetime, polling, idle, or write-deadline behavior. Production uses
// defaultStreamTuning; tests can adjust a server before it starts serving.
type streamTuning struct {
	idleCap               time.Duration
	backoffBase           time.Duration
	backoffCap            time.Duration
	healthyReset          time.Duration
	immediateWindow       time.Duration
	metricsPoll           time.Duration
	maxLifetime           time.Duration
	writeTimeout          time.Duration
	heartbeat             time.Duration
	checkpointInterval    time.Duration
	checkpointDeltas      uint64
	handshakeTimeout      time.Duration
	initialMetricsTimeout time.Duration
	metricsRequestTimeout time.Duration
}

func defaultStreamTuning() streamTuning {
	return streamTuning{
		// A stream with no watch data ends as idle after 30 minutes.
		idleCap: 30 * time.Minute,
		// Re-watch delay doubles from 250ms to a 10s cap. A watch that lives for
		// one minute resets the schedule to its base.
		backoffBase:  250 * time.Millisecond,
		backoffCap:   10 * time.Second,
		healthyReset: time.Minute,
		// An event-less watch ending inside one second contributes to the
		// immediate-EOF storm limit.
		immediateWindow: time.Second,
		// Joined metrics are refreshed independently every 30 seconds.
		metricsPoll: 30 * time.Second,
		// Trusted-headers / none streams have no session expiry, so bound their
		// total lifetime to 12 hours.
		maxLifetime: 12 * time.Hour,
		// Bound every SSE frame write so a non-reading peer cannot retain a
		// stream-cap slot indefinitely.
		writeTimeout: 30 * time.Second,
		// Application-level comments keep otherwise quiet streams alive through
		// ingress/LB idle timeouts. They carry no domain sequence or state.
		heartbeat: 20 * time.Second,
		// Periodic full snapshots bound client/server drift and refresh the
		// recovery checkpoint even on otherwise delta-only v2 sessions.
		checkpointInterval: 10 * time.Minute,
		checkpointDeltas:   2048,
		// Discovery plus the initial LIST share one pre-handshake budget while
		// already holding a stream-cap slot.
		handshakeTimeout: 15 * time.Second,
		// The optional pre-handshake metrics join still owns a stream-cap slot.
		// Bound it even though the post-handshake loop has not started yet.
		initialMetricsTimeout: 10 * time.Second,
		// Every post-handshake metrics poll gets its own shorter-lived request
		// budget. A stalled poll must not suppress every later refresh.
		metricsRequestTimeout: 10 * time.Second,
	}
}

const (
	// streamMaxImmediateEOFs consecutive immediate EOFs are a re-watch
	// failure (terminal reason "watch-failed") — an EOF storm must not
	// spin re-watch attempts forever.
	streamMaxImmediateEOFs = 5

	// streamMinPushGap / streamMaxPushLatency are the pacing bounds: pushes
	// are at least 300ms apart, and while events pend a push happens at most
	// 2s after the previous one.
	streamMinPushGap     = 300 * time.Millisecond
	streamMaxPushLatency = 2 * time.Second

	// High-churn detection: at least streamChurnEvents data events inside the
	// trailing streamChurnWindow (>~5 events/s sustained) degrades pushes to
	// the fixed streamMaxPushLatency interval — the apiserver-side cost
	// argument does not cover readout's own render/transfer/morph cost.
	streamChurnWindow = 2 * time.Second
	streamChurnEvents = 10

	// streamMaxEventBytes bounds one JSON payload before anything is written to
	// the response. Live is optional, so an abnormally large rendered table can
	// close the stream and fall back to the ordinary bounded polling path.
	streamMaxEventBytes = 16 << 20

	// A generation is reflected in every stream frame. Bound the v2 header
	// before the SSE handshake.
	streamMaxGenerationBytes = 64

	streamVersionHeader    = "RO-Live-Version"
	streamGenerationHeader = "RO-Live-Generation"
)

// streamLiveEnvelope is the v2 snapshot/delta/terminal envelope.
// Schema fingerprints the rendered contract independently from the semantic
// revision; Delta is a closed patch over the last committed projection.
type streamLiveEnvelope struct {
	V        int                  `json:"v"`
	Kind     string               `json:"kind"`
	G        string               `json:"g"`
	Seq      uint64               `json:"seq"`
	Rev      string               `json:"rev,omitempty"`
	RV       string               `json:"rv,omitempty"`
	Schema   string               `json:"schema,omitempty"`
	Snapshot *streamLiveSnapshot  `json:"snapshot,omitempty"`
	Delta    *liveProjectionDelta `json:"delta,omitempty"`
	Reason   string               `json:"reason,omitempty"`
}

type streamLiveSnapshot struct {
	HTML string `json:"html"`
}

type liveStreamNegotiation struct {
	gen string
}

// negotiateLiveStream requires the v2 header contract. Duplicate,
// comma-folded, or absent negotiation headers are rejected before the SSE
// handshake; there is no legacy query-string generation fallback.
func negotiateLiveStream(r *http.Request) (liveStreamNegotiation, int) {
	versionValues, versionPresent := rawHeaderValues(r.Header, streamVersionHeader)
	generationValues, generationPresent := rawHeaderValues(r.Header, streamGenerationHeader)
	if !versionPresent || !generationPresent || len(versionValues) != 1 || len(generationValues) != 1 ||
		strings.Contains(versionValues[0], ",") || strings.Contains(generationValues[0], ",") {
		return liveStreamNegotiation{}, http.StatusBadRequest
	}
	if versionValues[0] != "2" {
		return liveStreamNegotiation{}, http.StatusNotAcceptable
	}
	gen := generationValues[0]
	if !validLiveGeneration(gen) {
		return liveStreamNegotiation{}, http.StatusBadRequest
	}
	return liveStreamNegotiation{gen: gen}, 0
}

// rawHeaderValues returns every physical value for a case-insensitive header
// name. Negotiation must not depend on Header.Get choosing one duplicate or on
// an intermediary comma-folding several values into one string.
func rawHeaderValues(headers http.Header, name string) ([]string, bool) {
	var values []string
	present := false
	for key, rawValues := range headers {
		if strings.EqualFold(key, name) {
			present = true
			values = append(values, rawValues...)
		}
	}
	return values, present
}

// validLiveGeneration accepts UUID/base64url and other RFC 3986 unreserved
// tokens only. That keeps the echoed protocol identity printable and portable
// across clients and intermediaries without normalisation surprises.
func validLiveGeneration(gen string) bool {
	if gen == "" || len(gen) > streamMaxGenerationBytes {
		return false
	}
	for i := range len(gen) {
		c := gen[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			continue
		}
		switch c {
		case '-', '.', '_', '~':
			continue
		default:
			return false
		}
	}
	return true
}

// streamEventWindow is a fixed-size trailing-event ring. High-churn detection
// only needs the threshold's most recent timestamps; retaining every event in
// a pathological two-second burst would make an otherwise bounded stream grow.
type streamEventWindow struct {
	times [streamChurnEvents]time.Time
	next  int
	count int
}

func (w *streamEventWindow) note(now time.Time) {
	w.times[w.next] = now
	w.next = (w.next + 1) % len(w.times)
	if w.count < len(w.times) {
		w.count++
	}
}

func (w *streamEventWindow) high(now time.Time) bool {
	if w.count < streamChurnEvents {
		return false
	}
	cutoff := now.Add(-streamChurnWindow)
	for i := range w.count {
		if !w.times[i].After(cutoff) {
			return false
		}
	}
	return true
}

// streamBackoff is the re-watch delay schedule: the server's base doubles per
// attempt up to its cap. noteAttempt resets the schedule after a healthy watch.
type streamBackoff struct {
	tuning  streamTuning
	attempt int
}

// next returns the delay before the upcoming re-watch attempt and advances
// the schedule.
func (b *streamBackoff) next() time.Duration {
	d := b.tuning.backoffBase
	for i := 0; i < b.attempt && d < b.tuning.backoffCap; i++ {
		d *= 2
	}
	if d > b.tuning.backoffCap {
		d = b.tuning.backoffCap
	}
	if b.attempt < 63 {
		b.attempt++
	}
	return d
}

// noteAttempt records a finished watch attempt's lifetime: a healthy attempt
// resets the schedule so the next re-watch waits only the base delay again.
func (b *streamBackoff) noteAttempt(lived time.Duration) {
	if lived >= b.tuning.healthyReset {
		b.attempt = 0
	}
}

// watchResult is one delivery from the watch reader goroutine: a decoded
// event, or the error that ended the attempt (io.EOF for a clean upstream
// close — the error taxonomy is kube.TableWatch's).
type watchResult struct {
	ev  kube.WatchEvent
	err error
}

// streamTableWatch is the narrow lifecycle surface the session owns. The
// concrete kube.TableWatch implements it; the interface also makes late-open
// cleanup deterministic to test without exposing kube's response body.
type streamTableWatch interface {
	Next() (kube.WatchEvent, error)
	Close() error
}

type watchOpenResult struct {
	watch streamTableWatch
	err   error
}

type streamRelistResult struct {
	table kube.Table
	err   error
}

// streamOverlayResult carries one sub-poll's join overlays. A field is nil
// when the render does not ask for that join, so an unwanted overlay never
// costs an upstream request and never marks the session dirty.
type streamOverlayResult struct {
	usage map[string][2]float64
	nodes map[string]map[string]any
}

// newStreamChildContext transfers cancellation ownership to the session loop.
// The loop stores each returned cancel function in its lane and invokes it on
// completion or in the common exit defer.
func newStreamChildContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(parent)
}

func newStreamTimeoutContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, timeout)
}

// openWatchAsync performs only the potentially blocking response-header phase.
// Handoff is unbuffered: if the session has already canceled/superseded this
// attempt, nobody can retain an unowned successful watch and this goroutine
// closes it before returning.
func openWatchAsync(
	ctx context.Context,
	open func(context.Context) (streamTableWatch, error),
	out chan<- watchOpenResult,
) {
	watch, err := open(ctx)
	if err != nil && watch != nil {
		_ = watch.Close()
		watch = nil
	}
	if ctx.Err() != nil {
		if watch != nil {
			_ = watch.Close()
		}
		return
	}
	result := watchOpenResult{watch: watch, err: err}
	select {
	case out <- result:
		// The session goroutine now owns a successful watch.
	case <-ctx.Done():
		if watch != nil {
			_ = watch.Close()
		}
	}
}

func relistAsync(
	ctx context.Context,
	list func(context.Context) (kube.Table, error),
	out chan<- streamRelistResult,
) {
	table, err := list(ctx)
	if ctx.Err() != nil {
		return
	}
	select {
	case out <- streamRelistResult{table: table, err: err}:
	case <-ctx.Done():
	}
}

func overlayAsync(
	ctx context.Context,
	fetch func(context.Context) streamOverlayResult,
	out chan<- streamOverlayResult,
) {
	result := fetch(ctx)
	if ctx.Err() != nil {
		return
	}
	select {
	case out <- result:
	case <-ctx.Done():
	}
}

// watchReader pumps TableWatch.Next into out until the attempt ends. It is
// bound to the request context twice over: a canceled request closes the
// watch body (unblocking Next), and the send select frees the goroutine if
// the session stopped draining.
func watchReader(ctx context.Context, w streamTableWatch, out chan<- watchResult) {
	for {
		ev, err := w.Next()
		select {
		case out <- watchResult{ev: ev, err: err}:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

// resourceStream serves `GET …/{plural}/_stream` for Live mode. Order is load-
// bearing: the scope/namespace checks are free and run first; the cap slot is
// acquired before any upstream work and before SSE headers (a cap-exceeded
// stream 429s without ever connecting); discovery then classifies watch-less
// kinds (204). Only after the initial list succeeds do the SSE headers go
// out — every failure before that point is a plain HTTP status, every
// failure after it is an in-stream terminal `ro-live` envelope.
func (s *Server) resourceStream(w http.ResponseWriter, r *http.Request) {
	// Negotiation and every pre-handshake failure are explicitly non-cacheable.
	// Vary is set before any scope/auth/cap/discovery branch; Content-Type stays
	// unset here so only a successful initial snapshot commits SSE semantics.
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	addVary(h, streamVersionHeader)
	addVary(h, streamGenerationHeader)

	clusterName := r.PathValue("cluster")
	namespace := r.PathValue("namespace")
	plural := r.PathValue("plural")
	// Live scope cut: Live covers single-type AND single-cluster lists only.
	// Multi-type pages (plural "all"/"_all"/CSV) and multi-cluster scope
	// (cluster "_all"/CSV) get 404 — the dropdown renders the option disabled.
	if !isSingleListType(plural) || clusterName == kube.AllClusters || strings.Contains(clusterName, ",") {
		http.Error(w, "live streams cover single-type, single-cluster lists only", http.StatusNotFound)
		return
	}
	if namespace != "" && namespace != kube.AllNamespaces && !s.namespaceAllowed(namespace) {
		http.Error(w, "namespace is not allowed", http.StatusForbidden)
		return
	}
	negotiation, status := negotiateLiveStream(r)
	if status != 0 {
		if status == http.StatusNotAcceptable {
			http.Error(w, "unsupported live stream version", status)
		} else {
			http.Error(w, "invalid live stream negotiation", status)
		}
		return
	}
	renderReq := streamRenderRequest(r)
	cluster, ok := s.manager.Get(clusterName)
	if !ok {
		http.Error(w, "cluster not found", http.StatusNotFound)
		return
	}
	// Stream cap: acquire before SSE headers and any upstream call; release
	// on EVERY exit path. The deferred receive is the single release point —
	// 204/initial-list-failure/terminal/client-gone all pass through it.
	select {
	case s.streamSlots <- struct{}{}:
	default:
		http.Error(w, "too many live streams", http.StatusTooManyRequests)
		return
	}
	defer func() { <-s.streamSlots }()

	ctx := r.Context()
	handshakeTimeout := s.streamTuning.handshakeTimeout
	if handshakeTimeout <= 0 {
		handshakeTimeout = defaultStreamTuning().handshakeTimeout
	}
	handshakeCtx, cancelHandshake := context.WithTimeout(ctx, handshakeTimeout)
	defer cancelHandshake()
	client := s.kubeClient(r, cluster)
	rt, err := client.FindResource(handshakeCtx, plural, namespace != "", apiVersionParam(r))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			http.Error(w, "resource discovery timed out", http.StatusBadGateway)
			return
		}
		http.Error(w, "resource type not found", http.StatusNotFound)
		return
	}
	// Watch-less kinds (no watch verb — componentstatuses, the metrics
	// pseudo-types) cannot stream: 204 tells the client to fall back to
	// polling silently.
	if !slices.Contains(rt.Verbs, "watch") {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	listNS := namespace
	if namespace == kube.AllNamespaces {
		listNS = ""
	}
	lifetime, lifetimeReason := s.streamLifetime(r)
	sess := &streamSession{
		srv:            s,
		w:              w,
		rc:             http.NewResponseController(w),
		renderReq:      renderReq,
		client:         client,
		rt:             rt,
		cluster:        clusterName,
		listNS:         listNS,
		selector:       r.URL.Query().Get("selector"),
		gen:            negotiation.gen,
		wantMetrics:    r.URL.Query().Get("join") == "metrics" && (plural == "pods" || plural == "nodes"),
		wantNodes:      streamWantsNodeJoin(r, s.cfg.DefaultCustomColumns[plural], plural),
		lifetime:       lifetime,
		lifetimeReason: lifetimeReason,
		tuning:         s.streamTuning,
	}
	sess.run(ctx, handshakeCtx)
}

// streamLifetime resolves the stream's total-lifetime bound at connect time
// (the only auth check an SSE stream ever gets — the idle cap resets on watch
// data, so without this a revoked/expired session keeps receiving cluster
// state indefinitely). OIDC mode: the session cookie's own Expires, terminal
// reason "auth" (the client's no-reconnect taxonomy). Trusted-headers / none
// modes have no per-session expiry: the server's hard max-lifetime cap applies,
// terminal reason "idle".
func (s *Server) streamLifetime(r *http.Request) (time.Duration, string) {
	if s.cfg.AuthMode == config.AuthModeOIDC {
		if session, ok := s.auth.Session(r); ok {
			return time.Until(time.Unix(session.Expires, 0)), "auth"
		}
	}
	return s.streamTuning.maxLifetime, "idle"
}

// streamSession is one open Live stream: the unfiltered snapshot, the cached
// metrics overlay, and the pacing state. All fields are owned by the handler
// goroutine — the only other goroutine (the watch reader) communicates
// exclusively over its channel.
type streamSession struct {
	srv       *Server
	w         http.ResponseWriter
	rc        *http.ResponseController
	renderReq *http.Request
	client    *kube.Client
	rt        kube.ResourceType
	cluster   string
	listNS    string
	selector  string
	gen       string
	seq       uint64

	// snapshot is the per-cluster UNFILTERED Table for the stream's scope
	// (namespace + label selector — apiserver-level params). The readout-side
	// `f`/`filter`/`sort` params apply at render time on a clone, never here,
	// so filter-transition pushes work by construction.
	snapshot kube.Table
	// lastRV is the last seen resourceVersion — the re-watch point after a
	// clean EOF (and the replay floor, so already-seen events never repeat).
	lastRV string

	// wantMetrics / wantNodes record which join overlays the render query
	// asks for; the sub-poll fetches exactly those and nothing else.
	wantMetrics bool
	wantNodes   bool
	overlays    renderOverlays

	// lifetime / lifetimeReason bound the stream's TOTAL lifetime (resolved
	// at connect by streamLifetime; the loop arms a single never-reset timer).
	lifetime       time.Duration
	lifetimeReason string
	tuning         streamTuning

	dirty       bool
	lastPush    time.Time
	eventWindow streamEventWindow

	// Live v2 commits only after an encoded frame has been written and
	// flushed. These fields therefore describe the exact client-visible base,
	// never merely the latest locally-rendered candidate.
	projection          liveProjectionState
	deletedKeys         map[string]struct{}
	forceSnapshot       bool
	deltasSinceSnapshot uint64
	lastSnapshotAt      time.Time
	lastSnapshotBytes   int
	renderers           streamLiveRenderers
	watchOpener         func(context.Context) (streamTableWatch, error)
}

func (st *streamSession) openTableWatch(ctx context.Context) (streamTableWatch, error) {
	if st.watchOpener != nil {
		return st.watchOpener(ctx)
	}
	watch, err := st.client.WatchTable(ctx, &st.rt, kube.WatchOptions{
		Namespace:       st.listNS,
		LabelSelector:   st.selector,
		ResourceVersion: st.lastRV,
	})
	if watch == nil {
		return nil, err
	}
	return watch, err
}

// streamHandshakeStatus maps an initial-list failure to the plain HTTP status
// the handshake fails with: a 403 for a forbidden/unauthorized denial, a 404 for
// a missing resource, and a 502 for everything else (the cluster could not serve
// the snapshot). It classifies once through the shared classifier and maps the
// kind. The stream never half-connects, so this is the whole response.
func streamHandshakeStatus(err error) int {
	return failureHandshakeStatus(kube.ClassifyError(err))
}

// run fetches the initial snapshot, completes the SSE handshake with the initial
// full push, and hands off to the event loop. A failure before the handshake
// stays a plain HTTP status — the stream never half-connects.
func (st *streamSession) run(ctx, handshakeCtx context.Context) {
	table, err := st.list(handshakeCtx)
	if err != nil {
		http.Error(st.w, "initial list failed", streamHandshakeStatus(err))
		return
	}
	st.snapshot = table
	st.lastRV = table.ResourceVersion
	if st.wantMetrics || st.wantNodes {
		timeout := st.tuning.initialMetricsTimeout
		if timeout <= 0 {
			timeout = defaultStreamTuning().initialMetricsTimeout
		}
		overlayCtx, cancel := context.WithTimeout(handshakeCtx, timeout)
		st.applyOverlays(st.fetchOverlays(overlayCtx))
		cancel()
	}

	h := st.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("X-Accel-Buffering", "no")
	h.Set(streamVersionHeader, "2")
	h.Set(streamGenerationHeader, st.gen)
	st.w.WriteHeader(http.StatusOK)
	if err := st.push(ctx); err != nil {
		return
	}
	st.loop(ctx)
}

// list fetches the stream's pristine scope Table: namespace + label selector
// apply (apiserver-level), readout-side filters and sort do NOT — the
// snapshot stays unfiltered by contract.
func (st *streamSession) list(ctx context.Context) (kube.Table, error) {
	return st.client.Table(ctx, &st.rt, kube.ListOptions{Namespace: st.listNS, LabelSelector: st.selector})
}

// streamWantsNodeJoin decides whether the stream must carry the ?join=nodes
// overlay: Pod lists whose render actually has custom columns to evaluate it
// in (the join only ever feeds a custom-column expression, so without one the
// Nodes LIST would be pure waste).
func streamWantsNodeJoin(r *http.Request, defaultCustom, plural string) bool {
	q := r.URL.Query()
	if q.Get("join") != "nodes" || plural != "pods" {
		return false
	}
	return first(q.Get("customcols"), q.Get("custom-columns"), defaultCustom) != ""
}

// fetchOverlays resolves the join overlays this stream asked for, normalizing a
// failed fetch to an empty non-nil map so renders never fall back to a live
// per-push fetch inside applyTableOptionsWithOverlays (nil there means "fetch
// now"). An unwanted overlay stays nil and is never fetched.
func (st *streamSession) fetchOverlays(ctx context.Context) streamOverlayResult {
	var result streamOverlayResult
	if st.wantMetrics {
		result.usage = st.srv.fetchMetricsUsage(ctx, st.client, st.rt.Namespaced, st.listNS, false, st.selector)
		if result.usage == nil {
			result.usage = map[string][2]float64{}
		}
	}
	if st.wantNodes {
		result.nodes = st.srv.fetchNodeObjects(ctx, st.client)
		if result.nodes == nil {
			result.nodes = map[string]map[string]any{}
		}
	}
	return result
}

// emptyOverlays is the "upstream is not answering" overlay state: an empty
// non-nil map for every join the render asked for, so a timed-out source
// publishes placeholders instead of leaving stale values on screen.
func (st *streamSession) emptyOverlays() streamOverlayResult {
	var result streamOverlayResult
	if st.wantMetrics {
		result.usage = map[string][2]float64{}
	}
	if st.wantNodes {
		result.nodes = map[string]map[string]any{}
	}
	return result
}

// applyOverlays adopts a sub-poll result, reporting whether anything the
// render can see actually changed (so an unchanged poll never schedules a
// push). The adoption is UNCONDITIONAL: a nil overlay means "fetch it now" at
// render time, so the empty-map normalization must land even though it renders
// identically to nil -- otherwise a failed fetch would leave the render path
// re-listing upstream on every push.
func (st *streamSession) applyOverlays(result streamOverlayResult) bool {
	changed := !maps.Equal(result.usage, st.overlays.metrics) || !sameNodeOverlay(result.nodes, st.overlays.nodes)
	st.overlays.metrics = result.usage
	st.overlays.nodes = result.nodes
	return changed
}

// sameNodeOverlay compares two node overlays by name and resourceVersion: a
// mutated Node always gets a fresh resourceVersion, so this settles "did the
// join change?" without deep-comparing whole Node objects on every sub-poll.
func sameNodeOverlay(a, b map[string]map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for name, obj := range a {
		other, ok := b[name]
		if !ok || nestedString(obj, "metadata", "resourceVersion") != nestedString(other, "metadata", "resourceVersion") {
			return false
		}
	}
	return true
}

// loop is the stream's single event loop: watch lifecycle (connect, re-watch
// with backoff, relist on 410, terminal taxonomy), push pacing, the metrics
// sub-poll, and the idle cap all live in one select so no state needs locks.
func (st *streamSession) loop(ctx context.Context) {
	idleTimer := time.NewTimer(st.tuning.idleCap)
	defer idleTimer.Stop()
	// The total-lifetime bound (session expiry in OIDC mode, the hard cap
	// otherwise). NEVER reset — unlike the idle timer, watch data must not
	// extend it.
	lifetimeTimer := time.NewTimer(st.lifetime)
	defer lifetimeTimer.Stop()
	pushTimer := time.NewTimer(time.Hour)
	pushTimer.Stop()
	defer pushTimer.Stop()
	// The zero-delay first fire connects the initial watch through the same
	// path every re-watch takes.
	rewatchTimer := time.NewTimer(0)
	defer rewatchTimer.Stop()
	var overlayCh <-chan time.Time
	if st.wantMetrics || st.wantNodes {
		ticker := time.NewTicker(st.tuning.metricsPoll)
		defer ticker.Stop()
		overlayCh = ticker.C
	}
	var heartbeatCh <-chan time.Time
	if st.tuning.heartbeat > 0 {
		ticker := time.NewTicker(st.tuning.heartbeat)
		defer ticker.Stop()
		heartbeatCh = ticker.C
	}
	var (
		checkpointTimer *time.Timer
		checkpointCh    <-chan time.Time
	)
	if st.tuning.checkpointInterval > 0 {
		checkpointTimer = time.NewTimer(st.tuning.checkpointInterval)
		defer checkpointTimer.Stop()
		checkpointCh = checkpointTimer.C
	}
	resetCheckpoint := func() {
		if checkpointTimer == nil {
			return
		}
		if !checkpointTimer.Stop() {
			select {
			case <-checkpointTimer.C:
			default:
			}
		}
		delay := st.tuning.checkpointInterval
		if !st.lastSnapshotAt.IsZero() {
			delay = time.Until(st.lastSnapshotAt.Add(st.tuning.checkpointInterval))
			if delay < 0 {
				delay = 0
			}
		}
		checkpointTimer.Reset(delay)
		checkpointCh = checkpointTimer.C
	}

	var (
		cur             streamTableWatch
		events          chan watchResult
		openResults     = make(chan watchOpenResult)
		opening         bool
		attemptCtx      context.Context
		attemptCancel   context.CancelFunc
		relistResults   = make(chan streamRelistResult)
		relisting       bool
		relistCancel    context.CancelFunc
		overlayResults  <-chan streamOverlayResult
		overlayDone     <-chan struct{}
		overlayInFlight bool
		overlayCancel   context.CancelFunc
		attemptStart    time.Time
		attemptSawEvent bool
		backoff         = streamBackoff{tuning: st.tuning}
		immediateEOFs   int
	)
	cancelAttempt := func() {
		if attemptCancel != nil {
			attemptCancel()
			attemptCancel = nil
			attemptCtx = nil
		}
	}
	cancelRelist := func() {
		if relistCancel != nil {
			relistCancel()
			relistCancel = nil
		}
	}
	cancelOverlays := func() {
		if overlayCancel != nil {
			overlayCancel()
		}
		overlayCancel = nil
		overlayResults = nil
		overlayDone = nil
		overlayInFlight = false
	}
	startRelist := func() {
		if relisting {
			return
		}
		relistCtx, cancel := newStreamChildContext(ctx)
		relistCancel = cancel
		relisting = true
		go relistAsync(relistCtx, st.list, relistResults)
	}
	startOverlays := func() {
		if overlayInFlight {
			return
		}
		timeout := st.tuning.metricsRequestTimeout
		if timeout <= 0 {
			timeout = defaultStreamTuning().metricsRequestTimeout
		}
		overlayCtx, cancel := newStreamTimeoutContext(ctx, timeout)
		results := make(chan streamOverlayResult)
		overlayCancel = cancel
		overlayResults = results
		overlayDone = overlayCtx.Done()
		overlayInFlight = true
		go overlayAsync(overlayCtx, st.fetchOverlays, results)
	}
	defer func() {
		cancelAttempt()
		cancelRelist()
		cancelOverlays()
		if cur != nil {
			_ = cur.Close()
		}
	}()

	// endAttempt classifies a finished watch attempt: 410 relists and
	// re-watches immediately; upstream 401/403 is terminal "auth"; everything
	// else (clean EOF included) re-watches from lastRV with backoff — unless
	// it is the streamMaxImmediateEOFs-th consecutive immediate end, which is
	// the re-watch failure terminal. Returns false when the stream must end.
	endAttempt := func(err error) bool {
		cancelAttempt()
		opening = false
		if cur != nil {
			_ = cur.Close()
			cur = nil
		}
		events = nil
		lived := time.Since(attemptStart)
		switch {
		case errors.Is(err, kube.ErrWatchGone):
			// 410: the RV fell out of the apiserver history window. Silent
			// relist + full push, then re-watch from the fresh RV. The LIST is
			// asynchronous too: a stalled recovery must not freeze stream timers.
			startRelist()
			return true
		case kube.IsForbidden(err):
			// Upstream 401/403 — e.g. session token expiry in passthrough
			// mode. The stream cannot recover by retrying.
			st.terminal("auth")
			return false
		}
		if !attemptSawEvent && lived < st.tuning.immediateWindow {
			immediateEOFs++
			if immediateEOFs >= streamMaxImmediateEOFs {
				st.terminal("watch-failed")
				return false
			}
		} else {
			immediateEOFs = 0
		}
		backoff.noteAttempt(lived)
		rewatchTimer.Reset(backoff.next())
		return true
	}

	for {
		select {
		case <-ctx.Done():
			// The client went away (or the request ended): nobody is left to
			// write a terminal to. Deferred cleanup releases everything.
			return
		case <-st.srv.shutdownCh:
			st.terminal("shutdown")
			return
		case <-rewatchTimer.C:
			// Opening an HTTP watch can block before response headers. Keep that
			// phase outside this select so heartbeat/checkpoint/idle/lifetime and
			// downstream cancellation remain live. Exactly one child attempt owns
			// both setup and the resulting watch lifetime.
			if opening || cur != nil || relisting {
				continue
			}
			attemptCtx, attemptCancel = newStreamChildContext(ctx)
			opening = true
			go openWatchAsync(attemptCtx, st.openTableWatch, openResults)
		case opened := <-openResults:
			opening = false
			attemptStart = time.Now()
			attemptSawEvent = false
			if opened.err != nil || opened.watch == nil {
				err := opened.err
				if err == nil {
					err = errors.New("watch open returned no watch")
				}
				if ctx.Err() != nil {
					cancelAttempt()
					return
				}
				if !endAttempt(err) {
					return
				}
			} else {
				cur = opened.watch
				events = make(chan watchResult)
				go watchReader(attemptCtx, cur, events)
			}
		case relisted := <-relistResults:
			relisting = false
			cancelRelist()
			if relisted.err != nil {
				st.terminal("watch-failed")
				return
			}
			st.snapshot = relisted.table
			st.lastRV = relisted.table.ResourceVersion
			st.dirty = true
			st.forceSnapshot = true
			backoff = streamBackoff{tuning: st.tuning}
			immediateEOFs = 0
			st.schedulePush(pushTimer)
			rewatchTimer.Reset(0)
		case res := <-events:
			if res.err != nil {
				if ctx.Err() != nil {
					return
				}
				if !endAttempt(res.err) {
					return
				}
			} else {
				attemptSawEvent = true
				immediateEOFs = 0
				if res.ev.ResourceVersion != "" {
					st.lastRV = res.ev.ResourceVersion
				}
				switch res.ev.Type {
				case kube.WatchBookmark:
					// Bookmarks advance the re-watch point only; their rows are
					// NEVER read (the real apiserver may attach one).
				default:
					st.noteWatchMutation(&res.ev)
					mergeTableEvent(&st.snapshot, &res.ev)
					st.dirty = true
					st.noteEvent(time.Now())
					idleTimer.Reset(st.tuning.idleCap)
					st.schedulePush(pushTimer)
				}
			}
		case <-pushTimer.C:
			if st.dirty {
				lastSnapshotAt := st.lastSnapshotAt
				if err := st.push(ctx); err != nil {
					return
				}
				if st.lastSnapshotAt != lastSnapshotAt {
					resetCheckpoint()
				}
			}
		case <-overlayCh:
			startOverlays()
		case result := <-overlayResults:
			cancelOverlays()
			if st.applyOverlays(result) {
				st.dirty = true
				st.schedulePush(pushTimer)
			}
		case <-overlayDone:
			// The owner clears the lane on deadline even though overlayAsync drops
			// its canceled result. The next ticker edge can therefore recover with
			// a fresh request; the old per-attempt channel can never feed it. A
			// timed-out overlay source is not valid indefinitely: clear the stale
			// overlays and publish the same empty state as an upstream failure.
			cancelOverlays()
			st.applyOverlays(st.emptyOverlays())
			st.dirty = true
			st.schedulePush(pushTimer)
		case <-heartbeatCh:
			if err := st.writeHeartbeat(); err != nil {
				return
			}
		case <-checkpointCh:
			// Recovery checkpoints are transport maintenance, not user/watch
			// activity: schedule a full snapshot without extending the idle cap.
			checkpointCh = nil
			st.forceSnapshot = true
			st.dirty = true
			st.schedulePush(pushTimer)
		case <-idleTimer.C:
			st.terminal("idle")
			return
		case <-lifetimeTimer.C:
			st.terminal(st.lifetimeReason)
			return
		}
	}
}

// noteEvent records a data-event arrival for churn detection and prunes the
// trailing window.
func (st *streamSession) noteEvent(now time.Time) {
	st.eventWindow.note(now)
}

// highChurn reports sustained churn: at least streamChurnEvents data events
// inside the trailing streamChurnWindow (>~5 events/s sustained).
func (st *streamSession) highChurn(now time.Time) bool {
	return st.eventWindow.high(now)
}

// schedulePush arms the push timer for the pending changes: at least
// streamMinPushGap after the previous push (immediately once that gap has
// passed), degraded to the fixed streamMaxPushLatency interval under
// sustained churn — so while events pend, a push is never further than
// streamMaxPushLatency from the previous one and never closer than
// streamMinPushGap.
func (st *streamSession) schedulePush(timer *time.Timer) {
	if !st.dirty {
		return
	}
	now := time.Now()
	target := st.lastPush.Add(streamMinPushGap)
	if st.highChurn(now) {
		target = st.lastPush.Add(streamMaxPushLatency)
	}
	if target.Before(now) {
		target = now
	}
	timer.Reset(target.Sub(now))
}

// push runs the v2 projection/delta transaction.
func (st *streamSession) push(ctx context.Context) error {
	return st.pushLiveV2(ctx)
}

// terminal writes a v2 ro-live terminal frame.
// Write errors are ignored — the stream is closing either way.
func (st *streamSession) terminal(reason string) {
	st.srv.observeStreamTerminal(reason)
	st.terminalLiveV2(reason)
}

var (
	errStreamEventTooLarge  = errors.New("live stream event exceeds size limit")
	errStreamEventMultiline = errors.New("live stream JSON payload is not one line")
)

// cappedJSONBuffer rejects an encoder write that would cross its limit without
// retaining a partial oversized payload. encoding/json may build its own
// temporary representation, but this avoids a second unbounded allocation in
// the response staging buffer and guarantees no partial stream payload is emitted.
type cappedJSONBuffer struct {
	bytes.Buffer
	limit int
}

func (b *cappedJSONBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, errStreamEventTooLarge
	}
	return b.Buffer.Write(p)
}

// encodeStreamPayload produces exactly one JSON line. Disabling HTML escaping
// avoids expanding the rendered markup's ubiquitous <, >, and & bytes on an
// intentionally uncompressed streaming response.
func encodeStreamPayload(payload any, maxBytes int) ([]byte, error) {
	if maxBytes < 0 {
		return nil, errStreamEventTooLarge
	}
	buf := &cappedJSONBuffer{limit: maxBytes + 1} // Encoder adds one trailing LF.
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return nil, err
	}
	data := buf.Bytes()
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return nil, errStreamEventMultiline
	}
	data = data[:len(data)-1]
	if bytes.ContainsAny(data, "\r\n") {
		return nil, errStreamEventMultiline
	}
	return data, nil
}

// writeEvent writes one SSE frame and flushes it — per-message flush is part
// of the Live stream plumbing (statusWriter forwards Flush; the anti-buffering
// header set at the handshake keeps proxies honest). Every frame is bounded by a
// write deadline (via statusWriter's Unwrap → http.ResponseController): a
// connected-but-not-reading peer otherwise blocks the write forever once TCP
// buffers fill, wedging the handler outside its select loop with the cap slot
// held. A deadline error surfaces as the write/flush error — the normal
// client-gone exit. The deadline disarms after a successful frame (pushes can
// be arbitrarily far apart, and the next frame re-arms it anyway); deadline
// (dis)arming itself is best-effort — an unsupported writer just keeps the
// old unbounded behavior.
func (st *streamSession) writeEvent(event string, payload any) error {
	data, err := encodeStreamPayload(payload, streamMaxEventBytes)
	if err != nil {
		return err
	}
	return st.writeEncodedEvent(event, data)
}

// writeEncodedEvent frames a payload that has already passed its kind-specific
// bound. v2 preparation calls this directly so the exact bytes used for the
// delta-ratio decision and snapshot checkpoint accounting are the bytes sent.
func (st *streamSession) writeEncodedEvent(event string, data []byte) error {
	frame := make([]byte, 0, len(event)+len(data)+16)
	frame = append(frame, "event: "...)
	frame = append(frame, event...)
	frame = append(frame, "\ndata: "...)
	frame = append(frame, data...)
	frame = append(frame, '\n', '\n')
	return st.writeSSE(frame)
}

// writeHeartbeat emits a transport-only SSE comment. Browsers ignore it, but
// the write+flush keeps quiet connections active through intermediaries. It
// deliberately does not touch dirty, lastPush, resourceVersion, revision, or
// sequence state.
func (st *streamSession) writeHeartbeat() error {
	return st.writeSSE([]byte(": heartbeat\n\n"))
}

func (st *streamSession) writeSSE(frame []byte) error {
	_ = st.rc.SetWriteDeadline(time.Now().Add(st.tuning.writeTimeout))
	n, err := st.w.Write(frame)
	if err != nil {
		return err
	}
	if n != len(frame) {
		return io.ErrShortWrite
	}
	if err := st.rc.Flush(); err != nil {
		return err
	}
	_ = st.rc.SetWriteDeadline(time.Time{})
	return nil
}

// mergeTableEvent folds one watch data event into the unfiltered snapshot:
// ADDED/MODIFIED upsert the row by object identity (namespace/name), DELETED
// removes it. Watch frames carry columnDefinitions only in the stream's
// first event; the snapshot keeps the initial list's columns and adopts event
// columns only if the list somehow had none — cells align either way because
// both come from the same printer.
func mergeTableEvent(snapshot *kube.Table, ev *kube.WatchEvent) {
	if len(snapshot.Columns) == 0 && len(ev.Table.Columns) > 0 {
		snapshot.Columns = ev.Table.Columns
	}
	for _, row := range ev.Table.Rows {
		name := nestedString(row.Object, "metadata", "name")
		if name == "" {
			continue
		}
		ns := nestedString(row.Object, "metadata", "namespace")
		idx := -1
		for i := range snapshot.Rows {
			obj := snapshot.Rows[i].Object
			if nestedString(obj, "metadata", "name") == name && nestedString(obj, "metadata", "namespace") == ns {
				idx = i
				break
			}
		}
		switch ev.Type {
		case kube.WatchDeleted:
			if idx >= 0 {
				// A Live snapshot can outlive many delete events. slices.Delete
				// preserves row order and clears the obsolete backing-array slot,
				// so deleted row cells and object maps are not retained until the
				// slice grows again or the stream ends.
				snapshot.Rows = slices.Delete(snapshot.Rows, idx, idx+1)
			}
		default: // ADDED / MODIFIED
			if idx >= 0 {
				snapshot.Rows[idx] = row
			} else {
				snapshot.Rows = append(snapshot.Rows, row)
			}
		}
	}
}

// cloneTableForRender deep-copies the snapshot's table STRUCTURE (columns,
// rows, cells slices) so the render pipeline's mutations — decorations,
// hidecols removal, filters, sort — never touch the live snapshot. Row
// objects are shared by reference: the render path reads them without
// mutating, and the merge loop replaces objects wholesale rather than editing
// in place, so a pushed frame can never see a half-merged object.
func cloneTableForRender(t *kube.Table) kube.Table {
	clone := *t
	clone.Columns = append([]kube.Column(nil), t.Columns...)
	clone.Clusters = append([]string(nil), t.Clusters...)
	clone.Rows = make([]kube.Row, len(t.Rows))
	for i := range t.Rows {
		clone.Rows[i] = kube.Row{
			Cells:   append([]any(nil), t.Rows[i].Cells...),
			Object:  t.Rows[i].Object,
			Cluster: t.Rows[i].Cluster,
		}
	}
	return clone
}

// streamRenderRequest derives the render-path request: the canonical LIST
// page URL (path minus `/_stream`), so
// every href buildListView resolves matches what a `_table` partial bakes —
// byte-identical fragments morph cleanly client-side. The shallow request copy
// keeps the context and mux path values used by buildListView's canonicalizer.
func streamRenderRequest(r *http.Request) *http.Request {
	clone := *r
	u := *r.URL
	u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/_stream")
	// RawPath still describes the old /_stream path, so it cannot remain a
	// valid alternate encoding after Path changes.
	u.RawPath = ""
	clone.URL = &u
	return &clone
}
