package web

// handlers_stream.go is the server half of Live mode: the read-only
// `GET …/{plural}/_stream` SSE endpoint. A session owns no upstream
// Kubernetes state of its own: it subscribes to the process-local WatchHub
// (watchhub.go), which keeps one LIST+watch per credential/cluster/resource/
// namespace/selector key and publishes immutable Table revisions. The session
// renders those revisions — `f`/`sort`/columns apply at render time on a
// clone, never to the shared snapshot — and ships JSON snapshot/delta/terminal
// envelopes in `event: ro-live`.
//
// The session therefore owns only what is genuinely per-browser: the v2
// sequence and committed projection, push coalescing, heartbeats, the write
// deadline, recovery checkpoints, and the connect-time lifetime bound (the
// OIDC session's expiry, or the hard 12-hour cap otherwise). Watch recovery
// — re-watch backoff, the 410 relist, the EOF-storm terminal — belongs to the
// shared source, so a hundred browsers on one list recover once, together.
//
// Admission is staged and complete before any SSE header is written: a
// connection slot, then the source (joined when it already exists, created and
// measured against the retained-state bound otherwise). Every rejection is a
// 429 with Retry-After and no `text/event-stream`; a draining pod answers 503;
// watch-less kinds get 204. After the handshake every failure is an in-stream
// terminal `ro-live` envelope, and exactly one terminal outcome is counted per
// session.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/kbelokon/readout/internal/config"
	"github.com/kbelokon/readout/internal/kube"
)

// streamTuning is the immutable timing policy a Server hands to its WatchHub
// and copies into each Live stream at connect time. Keeping it server-local
// avoids hidden mutable package state: independently constructed servers
// cannot change one another's backoff, lifetime, polling, or write-deadline
// behavior. Production uses defaultStreamTuning; tests can adjust a server
// before it starts serving.
type streamTuning struct {
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
		// total lifetime to 12 hours. A quiet stream is NOT capped: an idle
		// namespace is healthy, and heartbeats plus the write deadline already
		// detect a dead peer.
		maxLifetime: 12 * time.Hour,
		// Bound every SSE frame write so a peer that stops reading is closed as
		// a slow writer instead of retaining a connection slot indefinitely.
		writeTimeout: 30 * time.Second,
		// Application-level comments keep otherwise quiet streams alive through
		// ingress/LB idle timeouts. They carry no domain sequence or state.
		heartbeat: 20 * time.Second,
		// Periodic full snapshots bound client/server drift and refresh the
		// recovery checkpoint even on otherwise delta-only v2 sessions.
		checkpointInterval: 10 * time.Minute,
		checkpointDeltas:   2048,
		// Discovery plus the shared source's initial LIST share one
		// pre-handshake budget while already holding a connection slot.
		handshakeTimeout: 15 * time.Second,
		// How long the handshake waits for the shared source to publish the
		// join overlays this render needs. Giving up renders join placeholders
		// and the next published revision fills them in.
		initialMetricsTimeout: 10 * time.Second,
		// Every join poll gets its own shorter-lived request budget. A stalled
		// poll must not suppress every later refresh.
		metricsRequestTimeout: 10 * time.Second,
	}
}

const (
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
	// the response. Live is optional, so an abnormally large rendered table
	// closes the stream with the `protocol` terminal and the browser stops
	// retrying it -- the one-shot Refresh button is still the way to re-read.
	streamMaxEventBytes = 16 << 20

	// A generation is reflected in every stream frame. Bound the v2 header
	// before the SSE handshake.
	streamMaxGenerationBytes = 64

	streamVersionHeader    = "RO-Live-Version"
	streamGenerationHeader = "RO-Live-Generation"
)

// The closed terminal vocabulary. Exactly one of these is counted per session
// (streamSession.finish), and every reason EXCEPT the two write failures is
// also written to the client as a terminal `ro-live` envelope. The browser's
// reconnect taxonomy reads them: "watch-failed"/"shutdown"/"lifetime" are what
// the reconnect ladder exists for, while "auth" and "protocol" must never be
// retried -- replaying the same request byte for byte reproduces the same
// failure, so the browser stops instead of re-rendering an unencodable table
// every thirty seconds forever. Only "client-close" and "slow-writer" go
// unannounced: they describe a peer that cannot be told anything.
const (
	streamTerminalAuth        = "auth"
	streamTerminalWatchFailed = "watch-failed"
	streamTerminalShutdown    = "shutdown"
	streamTerminalLifetime    = "lifetime"
	streamTerminalClientClose = "client-close"
	streamTerminalSlowWriter  = "slow-writer"
	streamTerminalProtocol    = "protocol"
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

// streamTableWatch is the narrow watch lifecycle surface the WatchHub source
// owns. The concrete kube.TableWatch implements it; the interface also makes
// late-open cleanup deterministic to test without exposing kube's response
// body.
type streamTableWatch interface {
	Next() (kube.WatchEvent, error)
	Close() error
}

// resourceStream serves `GET …/{plural}/_stream` for Live mode. Order is
// load-bearing: the scope/namespace checks are free and run first; a draining
// pod stops here with 503; the connection slot is taken before any upstream
// work; discovery then classifies watch-less kinds (204). Only after the
// shared source has published its first revision do the SSE headers go out —
// every failure before that point is a plain HTTP status, every failure after
// it is an in-stream terminal `ro-live` envelope.
func (s *Server) resourceStream(w http.ResponseWriter, r *http.Request) {
	// Negotiation and every pre-handshake failure are explicitly non-cacheable.
	// Vary is set before any scope/auth/admission/discovery branch; Content-Type
	// stays unset here so only a successful initial snapshot commits SSE
	// semantics.
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	addVary(h, streamVersionHeader)
	addVary(h, streamGenerationHeader)
	// ServeMux routes HEAD to a GET pattern, and net/http silently discards
	// every body byte written to a HEAD response. A HEAD stream would therefore
	// never fail a write, never trip the write deadline, and never reach the
	// slow-writer terminal -- it would hold a connection slot and a hub
	// subscription until the 12h lifetime cap. Live is GET only.
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Time-to-snapshot and session duration are both measured from here: what
	// an operator cares about is how long the BROWSER waited, which includes
	// discovery and the shared source's LIST, not just the render.
	started := time.Now()

	clusterName := r.PathValue("cluster")
	namespace := r.PathValue("namespace")
	plural := r.PathValue("plural")
	// Live scope cut: Live covers single-type AND single-cluster lists only.
	// Multi-type pages (plural "all"/"_all"/CSV) and multi-cluster scope
	// (cluster "_all"/CSV) get 404 — the toolbar renders no Live toggle.
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
	// A draining pod admits no new Live stream: readiness already reports 503,
	// and a stream started now would only receive the shutdown terminal.
	if s.shuttingDown() {
		http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
		return
	}
	// Admission stage one: the connection slot, taken before any upstream call
	// and before SSE headers, released on EVERY exit path.
	hub := s.liveHub()
	if !hub.acquireConnection() {
		s.observeLiveAdmission(liveAdmissionConnectionLimit)
		streamOverCapacity(w, "too many live connections")
		return
	}
	defer hub.releaseConnection()

	ctx := r.Context()
	handshakeCtx, cancelHandshake := context.WithTimeout(ctx, s.streamTuning.handshakeTimeout)
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
	// pseudo-types) cannot stream: 204 tells the client to stop without
	// retrying. The toolbar already hides the Live toggle for them.
	if !slices.Contains(rt.Verbs, "watch") {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	listNS := namespace
	if namespace == kube.AllNamespaces {
		listNS = ""
	}
	selector := r.URL.Query().Get("selector")
	key, err := newWatchHubKey(client, clusterName, &rt, namespace, selector)
	if err != nil {
		// The apiserver would reject the list anyway; failing here also keeps
		// one broken selector from creating a source per spelling of it.
		http.Error(w, "invalid label selector", http.StatusBadRequest)
		return
	}

	lifetime, lifetimeReason := s.streamLifetime(r)
	sess := &streamSession{
		srv:            s,
		w:              w,
		rc:             http.NewResponseController(w),
		renderReq:      renderReq,
		client:         client,
		cluster:        clusterName,
		selector:       selector,
		gen:            negotiation.gen,
		wantMetrics:    r.URL.Query().Get("join") == "metrics" && (plural == "pods" || plural == "nodes"),
		wantNodes:      streamWantsNodeJoin(r, s.cfg.DefaultCustomColumns[plural], rt.Kind),
		lifetime:       lifetime,
		lifetimeReason: lifetimeReason,
		tuning:         s.streamTuning,
		startedAt:      started,
	}
	// Admission stages two and three live inside the hub: joining an existing
	// source costs nothing, creating one is bounded by live.maxSources and then
	// -- once its own LIST is measured -- by live.maxCacheAccountedBytes.
	sub, rev, err := hub.Subscribe(handshakeCtx, s.streamSourceSpec(&key, client, &rt, listNS, selector), sess.demand())
	if err != nil {
		s.streamSubscribeFailure(w, err)
		return
	}
	s.observeLiveAdmission(liveAdmissionAccepted)
	defer sub.Close()
	sess.sub = sub
	sess.run(ctx, handshakeCtx, rev)
}

// streamOverCapacity is the shared shape of every Live admission rejection: a
// 429 with a retry hint and NO SSE content type, so a refused browser waits
// instead of reconnecting immediately or mistaking the body for a stream.
func streamOverCapacity(w http.ResponseWriter, message string) {
	w.Header().Set("Retry-After", "10")
	http.Error(w, message, http.StatusTooManyRequests)
}

// streamSubscribeFailure maps a failed hub attach to the plain HTTP status the
// handshake fails with: the two capacity bounds are 429s the client retries
// later, a timed-out shared LIST is a 502, and an upstream denial keeps the
// classifier's own status.
func (s *Server) streamSubscribeFailure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errHubSourceLimit):
		s.observeLiveAdmission(liveAdmissionSourceLimit)
		streamOverCapacity(w, "too many live sources")
	case errors.Is(err, errHubCacheLimit):
		s.observeLiveAdmission(liveAdmissionCacheLimit)
		streamOverCapacity(w, "live cache is at capacity")
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		// The shared initialization outlived this subscriber's handshake budget
		// (or the browser gave up). The source keeps initializing for whoever
		// asks next; this request just cannot be served a snapshot.
		http.Error(w, "initial list timed out", http.StatusBadGateway)
	default:
		http.Error(w, "initial list failed", streamHandshakeStatus(err))
	}
}

// streamSourceSpec describes ONE shared upstream list+watch: the credentials
// and scope behind the key, plus the demand-driven join reads. Only the key
// decides sharing, so these closures belong to whichever subscriber happened
// to create the source -- every other subscriber for the same key resolves to
// the same upstream request under the same identity.
func (s *Server) streamSourceSpec(key *watchHubKey, client *kube.Client, rt *kube.ResourceType, listNS, selector string) *hubSourceSpec {
	resource := *rt
	return &hubSourceSpec{
		key: *key,
		list: func(ctx context.Context) (kube.Table, error) {
			// The pristine scope Table: namespace + label selector apply
			// (apiserver-level), readout-side filters and sort do NOT.
			return client.Table(ctx, &resource, kube.ListOptions{Namespace: listNS, LabelSelector: selector})
		},
		watch: func(ctx context.Context, resourceVersion string) (streamTableWatch, error) {
			watch, err := client.WatchTable(ctx, &resource, kube.WatchOptions{
				Namespace:       listNS,
				LabelSelector:   selector,
				ResourceVersion: resourceVersion,
			})
			if watch == nil {
				return nil, err
			}
			return watch, err
		},
		overlay: func(ctx context.Context, demand hubDemand) renderOverlays {
			// A failed fetch normalizes to an empty non-nil map: nil means
			// "fetch it now" on the render path, which is exactly the per-push
			// upstream LIST the shared poll exists to avoid.
			var overlays renderOverlays
			if demand.metrics {
				overlays.metrics = s.fetchMetricsUsage(ctx, client, resource.Namespaced, listNS, false, selector)
				if overlays.metrics == nil {
					overlays.metrics = map[string][2]float64{}
				}
			}
			if demand.nodes {
				overlays.nodes = s.fetchNodeObjects(ctx, client)
				if overlays.nodes == nil {
					overlays.nodes = map[string]map[string]any{}
				}
			}
			return overlays
		},
	}
}

// streamLifetime resolves the stream's total-lifetime bound at connect time
// (the only auth check an SSE stream ever gets — without this a revoked or
// expired session keeps receiving cluster state indefinitely). OIDC mode: the
// session cookie's own Expires, terminal reason "auth" (the client's
// no-reconnect taxonomy). Trusted-headers / none modes have no per-session
// expiry: the server's hard max-lifetime cap applies, terminal reason
// "lifetime".
func (s *Server) streamLifetime(r *http.Request) (time.Duration, string) {
	if s.cfg.AuthMode == config.AuthModeOIDC {
		if session, ok := s.auth.Session(r); ok {
			return time.Until(time.Unix(session.Expires, 0)), streamTerminalAuth
		}
	}
	return s.streamTuning.maxLifetime, streamTerminalLifetime
}

// streamSession is one open Live stream: the hub subscription it renders from
// and the per-browser projection, pacing and sequence state. Every field is
// owned by the handler goroutine; the shared source communicates only through
// the subscription's channels and its immutable revisions.
type streamSession struct {
	srv       *Server
	w         http.ResponseWriter
	rc        *http.ResponseController
	renderReq *http.Request
	client    *kube.Client
	cluster   string
	selector  string
	gen       string
	seq       uint64

	// sub is this session's handle on the shared source: level-triggered
	// wakeups, the latest immutable revision, and the terminal reason the
	// source was released with.
	sub *hubSubscription

	// rev is the newest revision adopted for rendering; base is the revision
	// the COMMITTED projection describes. Rows present in base and absent from
	// rev are the actual apiserver deletions this push must classify as such.
	rev  *hubRevision
	base *hubRevision

	// lastRV mirrors the adopted revision's resourceVersion for the wire.
	lastRV string

	// wantMetrics / wantNodes record which join overlays this render asks for.
	// They become the subscription's demand, so the shared source polls a join
	// only while somebody needs it.
	wantMetrics bool
	wantNodes   bool

	// lifetime / lifetimeReason bound the stream's TOTAL lifetime (resolved
	// at connect by streamLifetime; the loop arms a single never-reset timer).
	lifetime       time.Duration
	lifetimeReason string
	tuning         streamTuning

	// startedAt is when the request arrived. It is the origin of both Live
	// latency metrics: time-to-snapshot ends at the first flushed frame,
	// session duration at the single terminal outcome.
	startedAt time.Time

	dirty    bool
	lastPush time.Time
	// churn carries the SOURCE's sustained-event-rate verdict: only the source
	// sees the raw events, so it is the only place pacing can learn the rate.
	churn bool

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

	// writeBound shortens the frame write deadline for the shutdown terminal,
	// so a non-reading peer cannot consume the whole graceful drain.
	writeBound time.Duration

	// finished guards the single terminal outcome counted per session.
	finished bool
}

// demand is this session's join requirement, handed to the source on attach
// and released with the subscription.
func (st *streamSession) demand() hubDemand {
	return hubDemand{metrics: st.wantMetrics, nodes: st.wantNodes}
}

// streamHandshakeStatus maps an initial-list failure to the plain HTTP status
// the handshake fails with: a 403 for a forbidden/unauthorized denial, a 404 for
// a missing resource, and a 502 for everything else (the cluster could not serve
// the snapshot). It classifies once through the shared classifier and maps the
// kind. The stream never half-connects, so this is the whole response.
func streamHandshakeStatus(err error) int {
	return failureHandshakeStatus(kube.ClassifyError(err))
}

// run adopts the revision the attach returned, completes the SSE handshake
// with the initial full push, and hands off to the event loop. A failure before
// the handshake stays a plain HTTP status — the stream never half-connects.
func (st *streamSession) run(ctx, handshakeCtx context.Context, rev *hubRevision) {
	if !st.adopt(rev) {
		http.Error(st.w, "live source published no snapshot", http.StatusBadGateway)
		return
	}
	st.awaitOverlays(handshakeCtx)

	h := st.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("X-Accel-Buffering", "no")
	h.Set(streamVersionHeader, "2")
	h.Set(streamGenerationHeader, st.gen)
	st.w.WriteHeader(http.StatusOK)
	if err := st.push(ctx); err != nil {
		st.failFrame(err)
		return
	}
	st.srv.observeLiveTimeToSnapshot(time.Since(st.startedAt))
	st.loop(ctx)
}

// adopt takes a newly published revision as this session's render source.
// Revisions are monotonically numbered, so an older or repeated one is
// ignored; a discontinuity (a relist) latches forceSnapshot until the next
// committed push, because the delta chain the client holds is broken.
func (st *streamSession) adopt(rev *hubRevision) bool {
	if rev == nil || (st.rev != nil && rev.num <= st.rev.num) {
		return false
	}
	if rev.forceSnapshot {
		st.forceSnapshot = true
	}
	st.rev = rev
	st.lastRV = rev.rv
	st.churn = rev.highChurn
	st.dirty = true
	return true
}

// awaitOverlays holds the handshake until the shared source has published the
// joins this render needs. The source starts its poll the moment this
// subscriber registers demand, so the wait is normally one upstream round trip;
// giving up renders join placeholders and the next published revision fills
// them in. Nothing here is per-subscriber upstream work.
func (st *streamSession) awaitOverlays(ctx context.Context) {
	if st.overlaysReady() {
		return
	}
	timer := time.NewTimer(st.tuning.initialMetricsTimeout)
	defer timer.Stop()
	for {
		select {
		case <-st.sub.Notify():
			if st.adopt(st.sub.Revision()) && st.overlaysReady() {
				return
			}
		case <-st.sub.Done():
			return
		case <-timer.C:
			return
		case <-ctx.Done():
			return
		}
	}
}

// overlaysReady reports whether the adopted revision already carries every
// join this render asks for.
func (st *streamSession) overlaysReady() bool {
	if st.rev == nil {
		return false
	}
	return (!st.wantMetrics || st.rev.overlays.metrics != nil) &&
		(!st.wantNodes || st.rev.overlays.nodes != nil)
}

// overlaysForRender is the adopted revision's join state with every requested
// join forced non-nil. A nil overlay means "fetch it now" to the decoration
// pass, so this substitution is what keeps a push off the upstream path while
// the shared poll is still in flight.
func (st *streamSession) overlaysForRender() renderOverlays {
	overlays := st.rev.overlays
	if st.wantMetrics && overlays.metrics == nil {
		overlays.metrics = map[string][2]float64{}
	}
	if st.wantNodes && overlays.nodes == nil {
		overlays.nodes = map[string]map[string]any{}
	}
	return overlays
}

// streamWantsNodeJoin decides whether the stream must carry the ?join=nodes
// overlay: Pod lists whose render actually has custom columns to evaluate it
// in (the join only ever feeds a custom-column expression, so without one the
// Nodes LIST would be pure waste). The Kind test is the same one wantsNodeJoin
// applies at render time -- the two must agree, or a render whose overlay was
// never demanded falls back to fetching Nodes on every push.
func streamWantsNodeJoin(r *http.Request, defaultCustom, kind string) bool {
	q := r.URL.Query()
	if q.Get("join") != "nodes" || kind != "Pod" {
		return false
	}
	return first(q.Get("customcols"), q.Get("custom-columns"), defaultCustom) != ""
}

// watchDeletions classifies the rows that left the shared scope between the
// committed base revision and the one about to be rendered: those are actual
// apiserver deletions, and the v2 delta reports them with the delete cause so
// the client may prune its selection. Every other disappearance is a
// projection change (a row that stopped matching the active filter).
//
// Kubernetes watch predicate semantics map old-match/new-no-match to a
// synthetic DELETED, so a label-selected scope has no wire-level distinction
// between selector exit and a real delete: that lane is never classified.
// Consequently an actual deletion already outside the rendered projection may
// leave latent cross-filter selection until a normal action/reload reconciles
// it. The v2 remove operation cannot fix that safely -- it is a projection
// operation and the client rejects absent-key tombstones by contract.
func (st *streamSession) watchDeletions() map[string]struct{} {
	if st.selector != "" || st.forceSnapshot || st.base == nil || st.rev == nil || st.base == st.rev {
		return nil
	}
	present := make(map[string]struct{}, len(st.rev.table.Rows))
	for i := range st.rev.table.Rows {
		if key, ok := st.rowIdentity(st.rev.table.Rows[i].Object); ok {
			present[key] = struct{}{}
		}
	}
	var deleted map[string]struct{}
	for i := range st.base.table.Rows {
		key, ok := st.rowIdentity(st.base.table.Rows[i].Object)
		if !ok {
			continue
		}
		if _, still := present[key]; still {
			continue
		}
		if len(deleted) >= streamMaxDeletedKeys {
			// The set cannot be carried safely; a full snapshot re-establishes
			// the client's state without classifying anything.
			st.forceSnapshot = true
			return nil
		}
		if deleted == nil {
			deleted = make(map[string]struct{})
		}
		deleted[key] = struct{}{}
	}
	return deleted
}

// rowIdentity is the projection key for one retained row object. An object
// without a name cannot be identified, and is not classified either way.
func (st *streamSession) rowIdentity(obj map[string]any) (string, bool) {
	name := nestedString(obj, "metadata", "name")
	if name == "" {
		return "", false
	}
	return rowKey(st.cluster, nestedString(obj, "metadata", "namespace"), name), true
}

// loop is the stream's single event loop: shared-source notifications and
// release, push pacing, heartbeats, recovery checkpoints, the total-lifetime
// bound and shutdown all live in one select so no session state needs locks.
// There is no idle cap: a quiet namespace is healthy, and the heartbeat plus
// the write deadline already detect a dead peer.
func (st *streamSession) loop(ctx context.Context) {
	// The total-lifetime bound (session expiry in OIDC mode, the hard cap
	// otherwise). NEVER reset — watch data must not extend it.
	lifetimeTimer := time.NewTimer(st.lifetime)
	defer lifetimeTimer.Stop()
	pushTimer := time.NewTimer(time.Hour)
	pushTimer.Stop()
	defer pushTimer.Stop()
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

	for {
		select {
		case <-ctx.Done():
			// The client went away (or the request ended): nobody is left to
			// write a terminal to.
			st.finish(streamTerminalClientClose)
			return
		case <-st.srv.shutdownCh:
			st.terminal(streamTerminalShutdown)
			return
		case <-st.sub.Notify():
			if st.adopt(st.sub.Revision()) {
				st.schedulePush(pushTimer)
			}
		case <-st.sub.Done():
			// The shared source died. Every subscriber gets the same reason,
			// once; recoverable watch failures never reach here.
			reason := st.sub.Reason()
			if reason == "" {
				reason = streamTerminalWatchFailed
			}
			st.terminal(reason)
			return
		case <-pushTimer.C:
			if st.dirty {
				lastSnapshotAt := st.lastSnapshotAt
				if err := st.push(ctx); err != nil {
					st.failFrame(err)
					return
				}
				if st.lastSnapshotAt != lastSnapshotAt {
					resetCheckpoint()
				}
			}
		case <-heartbeatCh:
			if err := st.writeHeartbeat(); err != nil {
				st.failFrame(err)
				return
			}
		case <-checkpointCh:
			// Recovery checkpoints are transport maintenance, not watch
			// activity: they schedule a full snapshot and nothing else.
			checkpointCh = nil
			st.forceSnapshot = true
			st.dirty = true
			st.schedulePush(pushTimer)
		case <-lifetimeTimer.C:
			st.terminal(st.lifetimeReason)
			return
		}
	}
}

// schedulePush arms the push timer for the pending changes: at least
// streamMinPushGap after the previous push (immediately once that gap has
// passed), degraded to the fixed streamMaxPushLatency interval while the
// source reports sustained churn — so while events pend, a push is never
// further than streamMaxPushLatency from the previous one and never closer
// than streamMinPushGap.
func (st *streamSession) schedulePush(timer *time.Timer) {
	if !st.dirty {
		return
	}
	now := time.Now()
	target := st.lastPush.Add(streamMinPushGap)
	if st.churn {
		target = st.lastPush.Add(streamMaxPushLatency)
	}
	if target.Before(now) {
		target = now
	}
	timer.Reset(target.Sub(now))
}

// push classifies the deletions this frame must report, then runs the v2
// projection/delta transaction.
func (st *streamSession) push(ctx context.Context) error {
	st.deletedKeys = st.watchDeletions()
	return st.pushLiveV2(ctx)
}

// finish records this session's single final outcome. Every exit funnels
// through it (terminal adds the client-visible frame), so the terminal counter
// carries exactly one sample per session.
func (st *streamSession) finish(reason string) {
	if st.finished {
		return
	}
	st.finished = true
	st.srv.observeStreamTerminal(reason)
	st.srv.observeLiveSessionDuration(time.Since(st.startedAt))
}

// observeFlush records how long the change this frame carries took to reach
// the browser, measured from the SOURCE's own event timestamp — the only place
// the pre-render half of the latency is visible. A session assembled directly
// by a render unit test has no server and no published revision behind it, so
// there is nothing to sample.
func (st *streamSession) observeFlush(now time.Time) {
	if st.srv == nil || st.rev == nil || st.rev.eventAt.IsZero() {
		return
	}
	st.srv.observeLiveEventToFlush(now.Sub(st.rev.eventAt))
}

// terminal writes a v2 ro-live terminal frame and records the outcome.
// Write errors are ignored — the stream is closing either way.
func (st *streamSession) terminal(reason string) {
	if st.finished {
		return
	}
	st.writeBound = terminalWriteBound(reason, st.writeBound)
	st.finish(reason)
	st.terminalLiveV2(reason)
}

// terminalWriteBound is the deadline the terminal frame is written under. The
// drain bounds a shutdown terminal wherever it came from: shutdown cancels the
// hub context too, so the reason arrives through the shared source's Done
// channel as readily as through shutdownCh, and a peer that stopped reading
// must not outlive the grace on either path.
func terminalWriteBound(reason string, current time.Duration) time.Duration {
	if reason == streamTerminalShutdown {
		return ShutdownGrace
	}
	return current
}

// streamWriteError marks a failure that happened while writing to the client,
// so the session's final outcome can tell a peer that went away from one that
// is connected but no longer reading. It wraps, so the underlying sentinel
// (io.ErrShortWrite, the transport's own error) still matches errors.Is.
type streamWriteError struct {
	err     error
	timeout bool
}

func (e *streamWriteError) Error() string { return "live stream write: " + e.err.Error() }

func (e *streamWriteError) Unwrap() error { return e.err }

func newStreamWriteError(err error) error {
	timeout := errors.Is(err, os.ErrDeadlineExceeded)
	if !timeout {
		var netErr net.Error
		timeout = errors.As(err, &netErr) && netErr.Timeout()
	}
	return &streamWriteError{err: err, timeout: timeout}
}

// failFrame ends the session on a frame that could not be delivered. A write
// failure means the peer is already gone or has stopped reading, so nothing
// more is sent. A render/encode fault is this SERVER's own and the connection
// is still healthy, so the peer is told: a bare EOF is indistinguishable from a
// transport drop, and the browser would climb its reconnect ladder and re-run
// the same failing render every thirty seconds for the life of the tab.
func (st *streamSession) failFrame(err error) {
	reason := streamFailureReason(err)
	if reason == streamTerminalProtocol {
		st.terminal(reason)
		return
	}
	st.finish(reason)
}

// streamFailureReason maps a failed frame to this session's terminal outcome:
// a write that hit its deadline is a peer that stopped reading, any other
// write failure is the ordinary client-gone exit, and a failure before the
// wire is a render/protocol fault the v2 contract cannot carry.
func streamFailureReason(err error) string {
	var writeErr *streamWriteError
	if errors.As(err, &writeErr) {
		if writeErr.timeout {
			return streamTerminalSlowWriter
		}
		return streamTerminalClientClose
	}
	return streamTerminalProtocol
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
// buffers fill, wedging the handler outside its select loop with its
// connection slot held. A deadline error surfaces as the "slow-writer"
// outcome; the shared source and every other subscriber are untouched. The
// deadline disarms after a successful frame (pushes can be arbitrarily far
// apart, and the next frame re-arms it anyway); deadline (dis)arming itself is
// best-effort — an unsupported writer just keeps the old unbounded behavior.
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
	deadline := st.tuning.writeTimeout
	if st.writeBound > 0 && st.writeBound < deadline {
		deadline = st.writeBound
	}
	_ = st.rc.SetWriteDeadline(time.Now().Add(deadline))
	n, err := st.w.Write(frame)
	if err != nil {
		return newStreamWriteError(err)
	}
	if n != len(frame) {
		return newStreamWriteError(io.ErrShortWrite)
	}
	if err := st.rc.Flush(); err != nil {
		return newStreamWriteError(err)
	}
	_ = st.rc.SetWriteDeadline(time.Time{})
	return nil
}

// mergeTableEvent folds one watch data event into an unfiltered scope Table and
// returns the change in accounted retained bytes. The merge already locates the
// row each event names, so measuring the replaced/deleted/added rows here costs
// one size computation per NAMED row rather than a table scan per event.
//
// ADDED/MODIFIED upsert the row by object identity (namespace/name), DELETED
// removes it. Watch frames carry columnDefinitions only in the stream's
// first event; the snapshot keeps the initial list's columns and adopts event
// columns only if the list somehow had none — cells align either way because
// both come from the same printer.
func mergeTableEvent(snapshot *kube.Table, ev *kube.WatchEvent) int64 {
	if len(snapshot.Columns) == 0 && len(ev.Table.Columns) > 0 {
		snapshot.Columns = ev.Table.Columns
	}
	var accounted int64
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
				accounted -= hubRowBytes(&snapshot.Rows[idx])
				// A Live snapshot can outlive many delete events. slices.Delete
				// preserves row order and clears the obsolete backing-array slot,
				// so deleted row cells and object maps are not retained until the
				// slice grows again or the stream ends.
				snapshot.Rows = slices.Delete(snapshot.Rows, idx, idx+1)
			}
		default: // ADDED / MODIFIED
			if idx >= 0 {
				accounted -= hubRowBytes(&snapshot.Rows[idx])
				snapshot.Rows[idx] = row
			} else {
				snapshot.Rows = append(snapshot.Rows, row)
			}
			accounted += hubRowBytes(&row)
		}
	}
	return accounted
}

// cloneTableForRender deep-copies a published revision's table STRUCTURE
// (columns, rows, cells slices) so the render pipeline's mutations —
// decorations, hidecols removal, filters, sort — never touch shared state a
// hundred other subscribers are reading. Row objects are shared by reference:
// the render path reads them without mutating, and the source replaces objects
// wholesale rather than editing in place, so a pushed frame can never see a
// half-merged object.
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
