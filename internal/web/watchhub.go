package web

// watchhub.go is the process-local WatchHub: the owner of every upstream
// Kubernetes list+watch a Live stream reads from. One source per
// credential/cluster/resource/namespace/selector key (watchHubKey) serves
// every subscriber on this pod, so a hundred browsers on one list cost the
// apiserver one LIST and one watch instead of a hundred of each.
//
// Each source is a single-goroutine ACTOR: subscribe, unsubscribe, the initial
// list result, watch events, re-watch/relist completions, overlay polls and
// retention expiry all arrive as commands on one mailbox, so the source's
// state needs no locks and every ordering question ("did this event land
// before that attach?") has one answer. The only state read outside the actor
// is the latest published revision, held in an atomic pointer.
//
// A revision is immutable once published: events are applied by copying the
// row slice and swapping the pointer, never by editing a table a subscriber
// may be rendering. Subscribers are notified level-triggered through a
// capacity-one mailbox that only ever means "a newer revision exists", so a
// slow browser can neither queue unbounded events nor block the source.
//
// The source also owns the whole watch lifecycle. A clean EOF or transient
// error re-watches from the last seen resourceVersion with bounded backoff; a
// 410 relists once and publishes a forced-snapshot revision; only an upstream
// 401/403, an immediate-EOF storm, a failed relist or hub shutdown is
// terminal. Subscribers stay attached through recovery and are closed once,
// with one reason, when a source really dies.
//
// The hub map is guarded by an ordinary mutex, held only to find, insert or
// remove a source pointer -- never while waiting on an actor.

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kbelokon/readout/internal/kube"
)

// hubSourceRetention keeps a source alive after its last subscriber leaves, so
// ordinary navigation and reconnect churn re-attaches to warm retained state
// instead of paying a fresh LIST.
const hubSourceRetention = 30 * time.Second

// hubMailbox is the per-source command buffer. It exists only to keep the
// watch reader and short control commands from ping-ponging on every event; a
// full mailbox is backpressure onto the reader, never a dropped command.
const hubMailbox = 64

// hubAttachAttempts bounds the retry when an attach lands on a source that is
// shutting down. Every retry implies that source left the map, so the next
// lookup creates a replacement; the bound is a safety net, not a schedule.
const hubAttachAttempts = 8

// streamMaxImmediateEOFs consecutive immediate EOFs are a re-watch failure
// (terminal reason "watch-failed") — an EOF storm must not spin re-watch
// attempts forever.
const streamMaxImmediateEOFs = 5

// cacheAccountingHeadroom multiplies a source's accounted bytes before they
// are compared against live.maxCacheAccountedBytes. Accounting measures the
// ENCODED size of the Table metadata and rows; the Go heap holds the decoded
// maps, slices and strings those bytes turn into, and a map[string]any costs
// several times what its JSON does. Measuring one retained 600-row scope in
// isolation (heap with the source retained minus heap after it is dropped)
// puts the real ratio just under 9x, so the configured bound is compared
// against a rounded-up 10x of the estimate and therefore reads as roughly
// "bytes of process memory this pod will hold in shared Live state".
const cacheAccountingHeadroom = 10

// Results reported to the metrics sink for one source lookup.
const (
	hubSourceCreated = "created"
	hubSourceReused  = "reused"
	hubSourceFailed  = "failed"
)

// Terminal reasons a source can close its subscribers with. They are the
// existing Live v2 terminal vocabulary: the browser already knows that "auth"
// must not be retried and that "watch-failed"/"shutdown" reconnect. A source
// released for any non-terminal reason (unsubscribe, retention) carries the
// empty reason: nothing failed, so the subscriber decides what to report.
const (
	hubTerminalAuth        = streamTerminalAuth
	hubTerminalWatchFailed = streamTerminalWatchFailed
	hubTerminalShutdown    = streamTerminalShutdown
)

var (
	// errHubSourceLimit is the per-pod distinct-source bound: the scope asked
	// for would be a NEW upstream watch and there is no room for one.
	errHubSourceLimit = errors.New("live source limit reached")

	// errHubSourceGone means the attach raced the source's own shutdown. It is
	// internal to the hub: Subscribe retries against a fresh source.
	errHubSourceGone = errors.New("live source is shutting down")

	// errHubCacheLimit is the per-pod retained-state bound: the scope's own
	// initial LIST pushed the hub past live.maxCacheAccountedBytes, so the new
	// source is failed and its waiters are rejected before any of them
	// commits to SSE. Sources that were already admitted keep serving.
	errHubCacheLimit = errors.New("live cache limit reached")
)

// hubClock is the hub's time surface. Sources schedule retention, re-watch
// backoff and the overlay poll through it, so a test can release a 30-second
// window without waiting.
type hubClock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) hubTimer
}

// hubTimer is the cancellable handle returned by hubClock.AfterFunc
// (*time.Timer satisfies it).
type hubTimer interface {
	Stop() bool
}

type realHubClock struct{}

func (realHubClock) Now() time.Time { return time.Now() }

func (realHubClock) AfterFunc(d time.Duration, f func()) hubTimer {
	return time.AfterFunc(d, f)
}

// hubMetricsSink receives the hub's source lifecycle. The hub takes an
// interface rather than the app's metric types so it can be exercised without
// a Prometheus registry and so no metric plumbing can reach into hub state.
type hubMetricsSink interface {
	// observeHubSource records one source lookup outcome: hubSourceCreated,
	// hubSourceReused or hubSourceFailed.
	observeHubSource(result string)
	// observeHubCounts reports the live source and subscriber totals after a
	// change.
	observeHubCounts(sources, subscribers int)
	// observeHubCache reports the hub-wide accounted retained bytes after a
	// change.
	observeHubCache(bytes int64)
	// observeHubConnections reports the open Live connection slots after a
	// change. Slots are the first admission gate, so this is the gauge that
	// says whether the pod is full.
	observeHubConnections(active int)
	// observeHubRelist records one 410 recovery LIST.
	observeHubRelist()
	// observeHubSnapshotBytes records the accounted size of one authoritative
	// snapshot (an initial list or a relist).
	observeHubSnapshotBytes(bytes int64)
}

type noopHubMetrics struct{}

func (noopHubMetrics) observeHubSource(string)       {}
func (noopHubMetrics) observeHubCounts(int, int)     {}
func (noopHubMetrics) observeHubCache(int64)         {}
func (noopHubMetrics) observeHubConnections(int)     {}
func (noopHubMetrics) observeHubRelist()             {}
func (noopHubMetrics) observeHubSnapshotBytes(int64) {}

// hubListFunc performs the source's authoritative Table LIST. hubWatchFunc
// opens one watch from a resourceVersion. hubOverlayFunc resolves the join
// overlays the attached subscribers currently ask for. All three are injected
// so the hub owns sharing and lifecycle while the caller owns credentials and
// HTTP.
type (
	hubListFunc    func(ctx context.Context) (kube.Table, error)
	hubWatchFunc   func(ctx context.Context, resourceVersion string) (streamTableWatch, error)
	hubOverlayFunc func(ctx context.Context, demand hubDemand) renderOverlays
)

// hubDemand is one subscriber's join requirement. The source keeps a count per
// join and polls upstream only while at least one subscriber needs it, so a
// hundred subscribers on a ?join=metrics list still cost one metrics read per
// interval and a list with no join costs none at all.
type hubDemand struct {
	metrics bool
	nodes   bool
}

// hubSourceSpec is everything needed to CREATE a source. Only the key
// participates in sharing: an attach that matches an existing source ignores
// the rest, because a matching key means the same upstream request under the
// same identity.
type hubSourceSpec struct {
	key     watchHubKey
	list    hubListFunc
	watch   hubWatchFunc
	overlay hubOverlayFunc
}

// hubRevision is one immutable published state of a source. Subscribers read
// the pointer and render from it; nothing ever mutates a revision after it is
// stored.
type hubRevision struct {
	// num is the source-local revision counter, starting at 1 for the initial
	// list. It orders revisions without comparing table contents.
	num uint64
	// table is the UNFILTERED scope Table. Render-time filters, sort and
	// columns apply to cloneTableForRender copies, never to this.
	table *kube.Table
	// rv is the last seen resourceVersion (the list's, then each event's).
	rv string
	// forceSnapshot marks a discontinuity -- a relist -- after which a
	// subscriber must send a full snapshot rather than a delta.
	forceSnapshot bool
	// highChurn reports that the SOURCE was taking sustained event traffic
	// when this revision was published. Only the source sees the raw events,
	// so it is the only place push pacing can learn the rate from.
	highChurn bool
	// overlays carries the demand-driven join reads (metrics, nodes) that were
	// current when this revision was published.
	overlays renderOverlays
	// eventAt is when the source applied the change this revision represents;
	// subscriber flush latency is measured from it.
	eventAt time.Time
}

type hubSourceState int

const (
	hubInitializing hubSourceState = iota
	hubReady
	hubFailed
)

// watchHub is the per-process source map.
type watchHub struct {
	// ctx bounds every source: source I/O runs under it, NOT under the
	// request context of whichever subscriber happened to arrive first.
	ctx     context.Context
	limits  liveLimits
	tuning  streamTuning
	clock   hubClock
	metrics hubMetricsSink

	mu          sync.Mutex
	sources     map[watchHubKey]*hubSource
	subscribers int
	accounted   int64
	// idle records the sources nobody is attached to, in the order they went
	// idle. They are still holding a slot, a watch and their retained bytes for
	// the rest of the retention window, so they are what a limit reclaims
	// before it refuses a subscriber who actually wants one.
	idle    map[*hubSource]uint64
	idleSeq uint64
	// perSource is each live source's contribution to accounted, so a reclaim
	// can return one source's bytes without waiting for its actor to stop.
	perSource map[*hubSource]int64
	// refused remembers, per key, that a scope's own LIST measured over the
	// retained-state bound. The bound is only knowable AFTER a full LIST, and a
	// refused source is dropped from the map, so without this memory every
	// browser retry of the 429 would pay another full Table LIST of the very
	// scope that just exhausted the pod's cache budget.
	refused map[watchHubKey]hubRefusal
	// connections counts open Live SSE handlers on this pod. It is the first
	// admission gate: a rejected connection never reaches a source, so an
	// over-capacity pod does no upstream work at all.
	connections int
}

// acquireConnection takes the first admission slot: one per open Live stream,
// released on every handler exit. It is deliberately independent of source
// sharing -- a hundred subscribers on one shared watch still cost a hundred
// sockets, goroutines and render pipelines.
func (h *watchHub) acquireConnection() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.connections >= h.limits.maxConnections {
		return false
	}
	h.connections++
	h.metrics.observeHubConnections(h.connections)
	return true
}

func (h *watchHub) releaseConnection() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.connections > 0 {
		h.connections--
		h.metrics.observeHubConnections(h.connections)
	}
}

func (h *watchHub) connectionCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.connections
}

func newWatchHub(ctx context.Context, limits liveLimits, tuning *streamTuning, clock hubClock, metrics hubMetricsSink) *watchHub {
	if tuning == nil {
		defaults := defaultStreamTuning()
		tuning = &defaults
	}
	if clock == nil {
		clock = realHubClock{}
	}
	if metrics == nil {
		metrics = noopHubMetrics{}
	}
	return &watchHub{
		ctx:       ctx,
		limits:    limits,
		tuning:    *tuning,
		clock:     clock,
		metrics:   metrics,
		sources:   map[watchHubKey]*hubSource{},
		idle:      map[*hubSource]uint64{},
		perSource: map[*hubSource]int64{},
		refused:   map[watchHubKey]hubRefusal{},
	}
}

// hubCacheRefusalTTL is how long a key that measured over the retained-state
// bound is refused WITHOUT re-listing. It is deliberately several times the
// `Retry-After` a refused browser waits, so the repeated handshakes an
// unstreamable scope produces cost a map lookup rather than a full LIST of the
// largest scope the pod knows about.
const hubCacheRefusalTTL = 60 * time.Second

// hubRefusal is one remembered cache-bound verdict, together with the budget it
// was measured against: `others` is what the REST of the hub held at the time,
// deliberately excluding the refused source's own bytes (those come back with
// its teardown, which is not the pod making room).
type hubRefusal struct {
	until  time.Time
	others int64
}

// refusedLocked reports whether this key may still be refused without listing
// it again, dropping the entry when it may not. A verdict is only as good as
// the budget behind it: once the window is out, once other sources have given
// bytes back, or once there is anything idle this subscriber could reclaim, the
// scope has to be measured again rather than blacklisted.
func (h *watchHub) refusedLocked(key *watchHubKey) bool {
	refusal, ok := h.refused[*key]
	if !ok {
		return false
	}
	if h.clock.Now().Before(refusal.until) && h.accounted >= refusal.others && len(h.idle) == 0 {
		return true
	}
	delete(h.refused, *key)
	return false
}

// noteCacheRefused records that this source's own LIST crossed the
// retained-state bound. Expired entries are swept on the way in, so the map
// stays bounded by the distinct scopes refused within one window.
func (h *watchHub) noteCacheRefused(src *hubSource) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.clock.Now()
	for existing, refusal := range h.refused {
		if !now.Before(refusal.until) {
			delete(h.refused, existing)
		}
	}
	h.refused[src.spec.key] = hubRefusal{
		until:  now.Add(hubCacheRefusalTTL),
		others: h.accounted - h.perSource[src],
	}
}

// Subscribe attaches one Live stream to the source for spec.key, creating that
// source if this is the first attach. It returns the current revision, so a
// subscriber renders from the shared state immediately and only then waits for
// notifications. While the source is initializing the caller blocks on ITS OWN
// context; a caller giving up never cancels the shared initialization.
//
// demand is this subscriber's join requirement, not the source's: it is added
// to the source's per-join counters for exactly as long as the subscription
// lives.
func (h *watchHub) Subscribe(ctx context.Context, spec *hubSourceSpec, demand hubDemand) (*hubSubscription, *hubRevision, error) {
	for range hubAttachAttempts {
		if err := h.ctx.Err(); err != nil {
			return nil, nil, err
		}
		src, err := h.source(spec)
		if err != nil {
			return nil, nil, err
		}
		sub, rev, err := src.attach(ctx, demand)
		if errors.Is(err, errHubSourceGone) {
			// The source shut down between the map lookup and the attach. It
			// is already out of the map, so the next pass creates its
			// replacement.
			continue
		}
		return sub, rev, err
	}
	return nil, nil, errHubSourceGone
}

// source finds or creates the source for a key. The new source is inserted
// into the map BEFORE any upstream I/O starts, so every concurrent attach for
// the same key joins one initialization instead of racing to start its own.
func (h *watchHub) source(spec *hubSourceSpec) (*hubSource, error) {
	return h.sourceLocked(spec)
}

// sourceLocked is source's critical section.
func (h *watchHub) sourceLocked(spec *hubSourceSpec) (*hubSource, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if src, ok := h.sources[spec.key]; ok {
		h.metrics.observeHubSource(hubSourceReused)
		return src, nil
	}
	// A scope that already measured over the cache bound is refused here,
	// before any upstream I/O, for the rest of its refusal window: re-listing it
	// on every retry would put the pod's heaviest LIST on a ten-second loop at
	// exactly the moment its retained state is at capacity.
	if h.refusedLocked(&spec.key) {
		h.metrics.observeHubSource(hubSourceFailed)
		return nil, errHubCacheLimit
	}
	if len(h.sources) >= h.limits.maxSources {
		// Retention is an optimization for the next visitor, not a reservation.
		// Give the slot to the subscriber in front of us rather than answering
		// 429 while a source nobody is watching sits out its window.
		if !h.reclaimIdleLocked(1) {
			return nil, errHubSourceLimit
		}
		h.observeCacheLocked()
	}
	src := newHubSource(h, spec)
	h.sources[spec.key] = src
	h.metrics.observeHubSource(hubSourceCreated)
	h.observeCountsLocked()
	go src.run()
	return src, nil
}

// noteIdle tracks whether a source currently has anybody attached. Sources call
// it from their actor goroutine as the retention timer arms and cancels; it only
// takes the map lock, never waits on an actor, so it cannot deadlock.
func (h *watchHub) noteIdle(src *hubSource, idle bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !idle {
		delete(h.idle, src)
		return
	}
	if _, ok := h.idle[src]; ok {
		return
	}
	h.idleSeq++
	h.idle[src] = h.idleSeq
}

// reclaimIdleLocked releases up to n subscriber-less sources, oldest-idle
// first, and reports whether it released any. Cancelling the source context is
// what tears it down: its actor loop returns and its stop path releases the
// watch, the subscribers it does not have, and its accounted bytes.
func (h *watchHub) reclaimIdleLocked(n int) bool {
	released := 0
	for released < n {
		var oldest *hubSource
		var oldestSeq uint64
		for src, seq := range h.idle {
			if oldest == nil || seq < oldestSeq {
				oldest, oldestSeq = src, seq
			}
		}
		if oldest == nil {
			break
		}
		delete(h.idle, oldest)
		if current, ok := h.sources[oldest.spec.key]; ok && current == oldest {
			delete(h.sources, oldest.spec.key)
			h.observeCountsLocked()
		}
		// Return the bytes now. The source's own teardown is asynchronous, and
		// the caller is about to re-check the bound it just reclaimed for.
		h.setSourceAccountedLocked(oldest, 0)
		oldest.cancel()
		released++
	}
	return released > 0
}

// reclaimIdle releases every subscriber-less source. It is the cache bound's
// last move before refusing a new source: retained bytes nobody is reading are
// not worth a 429.
func (h *watchHub) reclaimIdle() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	released := h.reclaimIdleLocked(len(h.idle))
	if released {
		h.observeCacheLocked()
	}
	return released
}

// remove drops a source from the map, but only if it is still the source
// registered for its key: a replacement created after this one started
// shutting down must survive.
func (h *watchHub) remove(src *hubSource) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.idle, src)
	if current, ok := h.sources[src.spec.key]; ok && current == src {
		delete(h.sources, src.spec.key)
		h.observeCountsLocked()
	}
}

// noteSubscribers keeps the hub-wide subscriber total. Sources call it from
// their actor goroutine; the hub lock is never held while waiting on an actor,
// so this cannot deadlock.
func (h *watchHub) noteSubscribers(delta int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.subscribers += delta
	h.observeCountsLocked()
}

// noteAccounted moves one source to a new retained-bytes total. The hub keeps
// the per-source figure so the sum is never recomputed AND so a reclaim can
// return a source's bytes at the moment it is dropped -- the source's own
// teardown runs on its goroutine, which is far too late for the admission
// decision that reclaimed it.
func (h *watchHub) noteAccounted(src *hubSource, total int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.setSourceAccountedLocked(src, total) {
		h.observeCacheLocked()
	}
}

func (h *watchHub) setSourceAccountedLocked(src *hubSource, total int64) bool {
	previous := h.perSource[src]
	if previous == total {
		return false
	}
	h.accounted += total - previous
	if total == 0 {
		delete(h.perSource, src)
	} else {
		h.perSource[src] = total
	}
	return true
}

func (h *watchHub) observeCountsLocked() {
	h.metrics.observeHubCounts(len(h.sources), h.subscribers)
}

// observeCacheLocked publishes the cache gauge while h.mu is still held, so
// the value the gauge ends on is the value the hub ended on. Publishing after
// the unlock lets two source actors interleave -- A reads 5MB, B reads 0 and
// publishes first -- and the gauge then latches A's stale total until the next
// accounting change, which on a draining pod may never come.
func (h *watchHub) observeCacheLocked() {
	h.metrics.observeHubCache(h.accounted)
}

func (h *watchHub) sourceCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.sources)
}

// accountedBytes is the hub-wide retained-state estimate: encoded Table
// metadata plus current row/object sizes across every live source. It never
// accumulates historical watch-event bytes.
func (h *watchHub) accountedBytes() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.accounted
}

// cacheChargedBytes is accountedBytes with the headroom multiplier applied:
// the number admission compares against live.maxCacheAccountedBytes.
func (h *watchHub) cacheChargedBytes() int64 {
	return h.accountedBytes() * cacheAccountingHeadroom
}

// overCacheLimit reports whether the retained state charged with headroom has
// crossed the configured bound.
func (h *watchHub) overCacheLimit() bool {
	return h.cacheChargedBytes() > h.limits.maxCacheAccountedBytes
}

// sourceStats reports one source's actor-owned counters, and whether the key
// still HAS a source. The two are distinct on purpose: a zero-valued answer for
// a vanished source would satisfy every "…is back to zero" assertion by the
// source simply not existing. The map lock is released before the actor is
// asked, because the actor itself takes that lock.
func (h *watchHub) sourceStats(key *watchHubKey) (hubSourceStats, bool) {
	h.mu.Lock()
	src, ok := h.sources[*key]
	h.mu.Unlock()
	if !ok {
		return hubSourceStats{}, false
	}
	return src.stats(), true
}

// hubCommand is one unit of work executed on a source's own goroutine.
type hubCommand func(*hubSource)

// hubSourceStats is a point-in-time view of a source, answered by the actor so
// it can never race the state it reports.
type hubSourceStats struct {
	subscribers   int
	waiters       int
	initialized   bool
	accounted     int64
	demandMetrics int
	demandNodes   int
}

// hubSource owns ONE upstream list+watch and the retained Table state it
// produces. Every field below the mailbox is owned by the actor goroutine
// except current, which is published atomically for subscribers to read.
type hubSource struct {
	hub  *watchHub
	spec hubSourceSpec

	cmds   chan hubCommand
	ctx    context.Context
	cancel context.CancelFunc
	// stopped closes after the source has left the hub map and released its
	// subscribers. A command posted before that point may still be dropped, so
	// every caller awaiting a reply also selects on it.
	stopped chan struct{}

	// current is the latest published revision, read without locks.
	current atomic.Pointer[hubRevision]

	state    hubSourceState
	stopping bool
	err      error
	reason   string
	revNum   uint64
	table    *kube.Table
	lastRV   string
	overlays renderOverlays
	watch    streamTableWatch
	subs     map[*hubSubscription]struct{}
	waiters  []*hubWaiter

	// accounted is this source's share of the hub-wide retained-bytes total.
	accounted int64

	// Watch-attempt lifecycle. attemptStart/attemptSawEvent classify how an
	// attempt ended, immediateEOFs counts the consecutive event-less instant
	// ends that make an EOF storm, and backoff schedules the next attempt.
	// Attempts are strictly serial: exactly one in-flight attempt reports
	// exactly one end, which is what starts the next one.
	attemptStart    time.Time
	attemptSawEvent bool
	immediateEOFs   int
	backoff         streamBackoff
	relisting       bool
	eventWindow     streamEventWindow

	// rewatch holds the pending re-watch delay; rewatchGen invalidates a timer
	// that already fired when the schedule changed underneath it.
	rewatch    hubTimer
	rewatchGen uint64

	// Demand-driven join polling: counters over the attached subscribers, the
	// pending interval timer, and the in-flight guard that keeps one slow poll
	// from stacking up behind the next interval.
	demandMetrics   int
	demandNodes     int
	overlayActive   bool
	overlayInFlight bool
	overlayTimer    hubTimer
	overlayGen      uint64
	// overlayPolled is the demand the in-flight (or most recent) poll was
	// started for; overlayDemandDirty records that demand grew past it while a
	// poll was already running.
	overlayPolled      hubDemand
	overlayDemandDirty bool

	// retention holds the no-subscriber release timer; idleGen invalidates a
	// timer that already fired when a new subscriber arrives.
	retention hubTimer
	idleGen   uint64
}

func newHubSource(hub *watchHub, spec *hubSourceSpec) *hubSource {
	ctx, cancel := context.WithCancel(hub.ctx)
	return &hubSource{
		hub:     hub,
		spec:    *spec,
		cmds:    make(chan hubCommand, hubMailbox),
		ctx:     ctx,
		cancel:  cancel,
		stopped: make(chan struct{}),
		subs:    map[*hubSubscription]struct{}{},
		backoff: streamBackoff{tuning: hub.tuning},
	}
}

// run is the actor loop. Nothing else may touch the fields it owns.
func (s *hubSource) run() {
	defer s.stop()
	go s.initialize()
	for {
		select {
		case cmd := <-s.cmds:
			cmd(s)
			if s.stopping {
				return
			}
		case <-s.ctx.Done():
			return
		}
	}
}

// post hands a command to the actor. It reports false when the source has
// stopped, which is the caller's signal to fall back to a fresh source.
func (s *hubSource) post(cmd hubCommand) bool {
	select {
	case s.cmds <- cmd:
		return true
	case <-s.stopped:
		return false
	}
}

// initialize performs the one authoritative LIST for this source, under the
// SOURCE's context so no subscriber's request can cancel it.
func (s *hubSource) initialize() {
	table, err := s.spec.list(s.ctx)
	s.post(func(s *hubSource) { s.listed(&table, err) })
}

// listed adopts the initial LIST: it publishes revision 1, serves every waiter
// that queued during initialization, and opens the watch.
func (s *hubSource) listed(table *kube.Table, err error) {
	if s.state != hubInitializing {
		return
	}
	if err != nil {
		s.failTerminal(hubListTerminal(err), err)
		return
	}
	s.state = hubReady
	s.adoptTable(table)
	// The retained-state bound is checked on the source that would cross it,
	// after its own LIST is measured and before any of its waiters commits to
	// SSE. Sources with subscribers are never evicted to make room; sources in
	// their retention window are, because holding bytes for a viewer who left
	// is worth less than serving the one who is here.
	if s.hub.overCacheLimit() && (!s.hub.reclaimIdle() || s.hub.overCacheLimit()) {
		s.hub.noteCacheRefused(s)
		s.failTerminal(hubTerminalWatchFailed, errHubCacheLimit)
		return
	}
	s.lastRV = table.ResourceVersion
	s.publish(s.table, false)
	for _, waiter := range s.waiters {
		waiter.serve(s.newSubscription(waiter.demand), s.current.Load(), nil)
	}
	s.waiters = nil
	s.checkIdle()
	s.startWatch()
}

// hubListTerminal classifies a failed LIST for the subscribers waiting on it:
// an upstream 401/403 is the no-retry "auth" outcome, anything else is a
// recoverable "watch-failed" the browser may reconnect from.
func hubListTerminal(err error) string {
	if kube.IsForbidden(err) {
		return hubTerminalAuth
	}
	return hubTerminalWatchFailed
}

// startWatch opens the source's watch and pumps its events into the mailbox.
// The reader is bound to the source context, and every hand-off checks that
// the source is still running, so a stopped source retains no goroutine and no
// upstream body.
func (s *hubSource) startWatch() {
	if s.stopping || s.state != hubReady {
		return
	}
	s.attemptStart = s.hub.clock.Now()
	s.attemptSawEvent = false
	from := s.lastRV
	go func() {
		watch, err := s.spec.watch(s.ctx, from)
		if err != nil {
			if watch != nil {
				_ = watch.Close()
			}
			s.post(func(s *hubSource) { s.watchEnded(err) })
			return
		}
		if !s.post(func(s *hubSource) { s.watchOpened(watch) }) {
			_ = watch.Close()
			return
		}
		for {
			ev, err := watch.Next()
			if err != nil {
				s.post(func(s *hubSource) { s.watchEnded(err) })
				return
			}
			if !s.post(func(s *hubSource) { s.applyEvent(&ev) }) {
				return
			}
		}
	}()
}

func (s *hubSource) watchOpened(watch streamTableWatch) {
	if s.stopping {
		_ = watch.Close()
		return
	}
	s.watch = watch
}

// watchEnded classifies a finished watch attempt, exactly as the per-stream
// session used to: a 410 relists once and re-watches from the fresh
// resourceVersion; an upstream 401/403 is the terminal "auth"; everything else
// (clean EOF included) re-watches from lastRV with bounded backoff -- unless it
// is the streamMaxImmediateEOFs-th consecutive event-less instant end, which is
// the terminal "watch-failed". Subscribers stay attached through every
// recoverable branch: only a terminal closes them.
func (s *hubSource) watchEnded(err error) {
	if s.stopping || s.state != hubReady {
		return
	}
	if s.watch != nil {
		_ = s.watch.Close()
		s.watch = nil
	}
	if err == nil {
		err = io.EOF
	}
	lived := s.hub.clock.Now().Sub(s.attemptStart)
	switch {
	case errors.Is(err, kube.ErrWatchGone):
		// 410: the resourceVersion fell out of the apiserver history window.
		// One asynchronous relist recovers it; a stalled relist must not
		// freeze the source's other timers.
		s.startRelist()
		return
	case kube.IsForbidden(err):
		// Upstream 401/403 — e.g. session token expiry in passthrough mode.
		// No retry can recover it.
		s.failTerminal(hubTerminalAuth, err)
		return
	}
	if !s.attemptSawEvent && lived < s.hub.tuning.immediateWindow {
		s.immediateEOFs++
		if s.immediateEOFs >= streamMaxImmediateEOFs {
			s.failTerminal(hubTerminalWatchFailed, err)
			return
		}
	} else {
		s.immediateEOFs = 0
	}
	s.backoff.noteAttempt(lived)
	s.scheduleRewatch(s.backoff.next())
}

// scheduleRewatch arms the next attempt. The generation makes a timer that
// already fired a no-op once the schedule moved on (a relist, or teardown).
func (s *hubSource) scheduleRewatch(d time.Duration) {
	s.cancelRewatch()
	gen := s.rewatchGen
	s.rewatch = s.hub.clock.AfterFunc(d, func() {
		s.post(func(s *hubSource) { s.rewatchDue(gen) })
	})
}

func (s *hubSource) cancelRewatch() {
	if s.rewatch != nil {
		s.rewatch.Stop()
		s.rewatch = nil
	}
	s.rewatchGen++
}

func (s *hubSource) rewatchDue(gen uint64) {
	if gen != s.rewatchGen || s.relisting {
		return
	}
	s.rewatch = nil
	s.startWatch()
}

// startRelist performs the 410 recovery LIST off the actor goroutine. Only one
// runs at a time; its result comes back as a command like everything else.
func (s *hubSource) startRelist() {
	if s.relisting {
		return
	}
	s.cancelRewatch()
	s.relisting = true
	s.hub.metrics.observeHubRelist()
	go func() {
		table, err := s.spec.list(s.ctx)
		s.post(func(s *hubSource) { s.relisted(&table, err) })
	}()
}

// relisted adopts the recovery LIST: the retained table is replaced wholesale
// (so accounted bytes are replaced, not adjusted), the new revision is marked
// forceSnapshot because the delta chain is broken, and the watch restarts from
// the fresh resourceVersion with a clean backoff schedule.
func (s *hubSource) relisted(table *kube.Table, err error) {
	if !s.relisting {
		return
	}
	s.relisting = false
	if s.stopping || s.state != hubReady {
		return
	}
	if err != nil {
		s.failTerminal(hubTerminalWatchFailed, err)
		return
	}
	s.adoptTable(table)
	s.lastRV = table.ResourceVersion
	s.publish(s.table, true)
	s.backoff = streamBackoff{tuning: s.hub.tuning}
	s.immediateEOFs = 0
	s.startWatch()
}

// applyEvent folds one watch event into a NEW retained table and publishes it.
// Bookmarks carry no rows: they only advance the re-watch point, so they
// publish nothing and count as neither churn nor watch data.
func (s *hubSource) applyEvent(ev *kube.WatchEvent) {
	if s.state != hubReady || s.stopping {
		return
	}
	s.attemptSawEvent = true
	s.immediateEOFs = 0
	if ev.ResourceVersion != "" {
		s.lastRV = ev.ResourceVersion
	}
	if ev.Type == kube.WatchBookmark {
		return
	}
	prev := s.table
	next, delta := hubApplyEvent(prev, ev)
	s.table = next
	if len(prev.Columns) == 0 && len(next.Columns) > 0 {
		// The first event supplied the column definitions the list lacked, so
		// the metadata half of the estimate has to be measured again.
		s.setAccounted(hubTableBytes(next))
	} else {
		s.setAccounted(s.accounted + delta)
	}
	s.eventWindow.note(s.hub.clock.Now())
	s.publish(s.table, false)
}

// adoptTable replaces the retained table wholesale and re-measures its
// accounted bytes. It is the initial-list and relist path; ordinary events
// adjust the running total instead.
func (s *hubSource) adoptTable(table *kube.Table) {
	s.table = table
	size := hubTableBytes(table)
	// One sample per authoritative snapshot: the distribution of retained
	// scope sizes is what live.maxCacheAccountedBytes has to be set against,
	// and watch events adjust that size rather than restating it.
	s.hub.metrics.observeHubSnapshotBytes(size)
	s.setAccounted(size)
}

// setAccounted moves this source to a new retained-bytes total and reports the
// difference to the hub, so the hub-wide sum never has to be recomputed.
func (s *hubSource) setAccounted(total int64) {
	if total < 0 {
		total = 0
	}
	s.accounted = total
	s.hub.noteAccounted(s, total)
}

// publish stores the next immutable revision and wakes every subscriber. The
// store happens BEFORE the wakeups, so a woken subscriber always finds at
// least the revision it was woken for.
func (s *hubSource) publish(table *kube.Table, forceSnapshot bool) {
	now := s.hub.clock.Now()
	s.revNum++
	s.current.Store(&hubRevision{
		num:           s.revNum,
		table:         table,
		rv:            s.lastRV,
		forceSnapshot: forceSnapshot,
		highChurn:     s.eventWindow.high(now),
		overlays:      s.overlays,
		eventAt:       now,
	})
	for sub := range s.subs {
		sub.wake()
	}
}

// failTerminal records the source's terminal outcome, hands the error to every
// waiter that was still expecting a snapshot, and ends the source. Waiters are
// served before the source leaves the map so a burst of attaches sees ONE
// failure rather than each starting a doomed replacement; the subscribers are
// then all closed with this one reason by stop.
func (s *hubSource) failTerminal(reason string, err error) {
	s.state = hubFailed
	s.err = err
	s.reason = reason
	s.hub.metrics.observeHubSource(hubSourceFailed)
	for _, waiter := range s.waiters {
		waiter.serve(nil, nil, err)
	}
	s.waiters = nil
	s.stopping = true
}

// stop is the single teardown path: leave the map first so no new attach can
// find this source, then cancel the source context (which unblocks the watch
// reader), close the upstream watch, release the subscribers with the terminal
// reason, drop this source's share of the accounted bytes, and finally open
// the stopped gate that tells racing attaches to retry elsewhere.
func (s *hubSource) stop() {
	s.hub.remove(s)
	s.cancel()
	s.cancelRewatch()
	s.cancelOverlayTimer()
	if s.watch != nil {
		_ = s.watch.Close()
		s.watch = nil
	}
	reason := s.stopReason()
	for sub := range s.subs {
		delete(s.subs, sub)
		s.hub.noteSubscribers(-1)
		sub.finish(reason)
	}
	for _, waiter := range s.waiters {
		waiter.serve(nil, nil, s.stopErr())
	}
	s.waiters = nil
	s.setAccounted(0)
	close(s.stopped)
}

// stopReason is the terminal reason the released subscribers are closed with:
// the source's own failure when it had one, "shutdown" when the hub itself is
// going away, and no reason at all for an ordinary retention release (nobody
// is attached to hear it).
func (s *hubSource) stopReason() string {
	switch {
	case s.reason != "":
		return s.reason
	case s.hub.ctx.Err() != nil:
		return hubTerminalShutdown
	default:
		return ""
	}
}

// stopErr is what a waiter still queued at teardown is told: the source's own
// failure when it had one, the hub's shutdown cause otherwise.
func (s *hubSource) stopErr() error {
	switch {
	case s.err != nil:
		return s.err
	case s.ctx.Err() != nil:
		return s.ctx.Err()
	default:
		return errHubSourceGone
	}
}

// attach registers one subscriber. Registration and event application are both
// actor commands, so an event racing this call is either already in the
// returned revision or arrives later as exactly one notification.
func (s *hubSource) attach(ctx context.Context, demand hubDemand) (*hubSubscription, *hubRevision, error) {
	waiter := &hubWaiter{reply: make(chan hubAttachResult, 1), demand: demand}
	if !s.post(func(s *hubSource) { s.serveWaiter(waiter) }) {
		return nil, nil, errHubSourceGone
	}
	select {
	case res := <-waiter.reply:
		return res.sub, res.rev, res.err
	case <-ctx.Done():
		// Withdraw through the actor: it alone knows whether this waiter was
		// already served a live subscription that now has to be released.
		s.post(func(s *hubSource) { s.withdrawWaiter(waiter) })
		return nil, nil, ctx.Err()
	case <-s.stopped:
		select {
		case res := <-waiter.reply:
			return res.sub, res.rev, res.err
		default:
			return nil, nil, errHubSourceGone
		}
	}
}

// serveWaiter answers one attach: immediately when the source is ready,
// otherwise by queuing it onto the shared initialization.
func (s *hubSource) serveWaiter(waiter *hubWaiter) {
	s.cancelRetention()
	switch s.state {
	case hubInitializing:
		s.waiters = append(s.waiters, waiter)
	case hubReady:
		waiter.serve(s.newSubscription(waiter.demand), s.current.Load(), nil)
	case hubFailed:
		waiter.serve(nil, nil, s.err)
	}
}

// withdrawWaiter undoes an attach whose caller gave up. The waiter is either
// still queued (drop it) or was already served a subscription (release it).
func (s *hubSource) withdrawWaiter(waiter *hubWaiter) {
	for i, queued := range s.waiters {
		if queued == waiter {
			s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
			s.checkIdle()
			return
		}
	}
	if waiter.sub != nil {
		s.detach(waiter.sub)
	}
}

func (s *hubSource) newSubscription(demand hubDemand) *hubSubscription {
	sub := &hubSubscription{
		src:    s,
		demand: demand,
		notify: make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
	s.subs[sub] = struct{}{}
	if demand.metrics {
		s.demandMetrics++
	}
	if demand.nodes {
		s.demandNodes++
	}
	s.hub.noteSubscribers(1)
	s.syncOverlayDemand()
	return sub
}

// detach releases one subscription. The source itself survives: it keeps
// serving everyone else, and an empty source starts its retention window.
func (s *hubSource) detach(sub *hubSubscription) {
	if _, ok := s.subs[sub]; !ok {
		return
	}
	delete(s.subs, sub)
	if sub.demand.metrics {
		s.demandMetrics--
	}
	if sub.demand.nodes {
		s.demandNodes--
	}
	s.hub.noteSubscribers(-1)
	sub.finish("")
	s.syncOverlayDemand()
	s.checkIdle()
}

// syncOverlayDemand keeps the shared join poll aligned with what the attached
// subscribers actually need. Demand is per KIND, not a single on/off: a source
// already polling metrics must re-poll the moment somebody needs nodes, or that
// subscriber stalls its whole handshake waiting for a join the running poll
// never asked for. Losing all demand cancels the pending timer, so an unwatched
// source makes no upstream join requests at all.
func (s *hubSource) syncOverlayDemand() {
	demand := hubDemand{metrics: s.demandMetrics > 0, nodes: s.demandNodes > 0}
	if !demand.metrics && !demand.nodes {
		s.overlayActive = false
		s.overlayPolled = hubDemand{}
		s.overlayDemandDirty = false
		s.cancelOverlayTimer()
		return
	}
	gained := (demand.metrics && !s.overlayPolled.metrics) || (demand.nodes && !s.overlayPolled.nodes)
	s.overlayActive = true
	if !gained {
		return
	}
	if s.overlayInFlight {
		// The in-flight poll was started for a narrower demand. Re-poll the
		// moment it lands instead of at the next interval.
		s.overlayDemandDirty = true
		return
	}
	s.pollOverlays()
}

// pollOverlays fetches the joins the currently attached subscribers need. One
// poll serves every subscriber: the count never multiplies upstream requests.
func (s *hubSource) pollOverlays() {
	if s.spec.overlay == nil || s.overlayInFlight || !s.overlayActive || s.stopping {
		return
	}
	s.cancelOverlayTimer()
	s.overlayInFlight = true
	demand := hubDemand{metrics: s.demandMetrics > 0, nodes: s.demandNodes > 0}
	s.overlayPolled = demand
	s.overlayDemandDirty = false
	fetch := s.spec.overlay
	timeout := s.hub.tuning.metricsRequestTimeout
	go func() {
		ctx, cancel := context.WithTimeout(s.ctx, timeout)
		defer cancel()
		overlays := fetch(ctx, demand)
		s.post(func(s *hubSource) { s.overlaysFetched(overlays) })
	}()
}

// overlaysFetched publishes the poll as a new revision -- an overlay change is
// a render change even when no watch event happened -- and arms the next
// interval while the demand is still there.
func (s *hubSource) overlaysFetched(overlays renderOverlays) {
	s.overlayInFlight = false
	if s.stopping {
		return
	}
	s.overlays = overlays
	if s.state == hubReady {
		s.publish(s.table, false)
	}
	if !s.overlayActive {
		return
	}
	if s.overlayDemandDirty {
		s.pollOverlays()
		return
	}
	s.armOverlayTimer()
}

func (s *hubSource) armOverlayTimer() {
	s.cancelOverlayTimer()
	gen := s.overlayGen
	s.overlayTimer = s.hub.clock.AfterFunc(s.hub.tuning.metricsPoll, func() {
		s.post(func(s *hubSource) { s.overlayDue(gen) })
	})
}

func (s *hubSource) cancelOverlayTimer() {
	if s.overlayTimer != nil {
		s.overlayTimer.Stop()
		s.overlayTimer = nil
	}
	s.overlayGen++
}

func (s *hubSource) overlayDue(gen uint64) {
	if gen != s.overlayGen {
		return
	}
	s.overlayTimer = nil
	s.pollOverlays()
}

// checkIdle arms the retention window once nobody is attached and nobody is
// waiting for the initial snapshot.
func (s *hubSource) checkIdle() {
	if len(s.subs) > 0 || len(s.waiters) > 0 {
		return
	}
	s.armRetention()
}

func (s *hubSource) armRetention() {
	if s.retention != nil {
		return
	}
	s.hub.noteIdle(s, true)
	gen := s.idleGen
	s.retention = s.hub.clock.AfterFunc(hubSourceRetention, func() {
		s.post(func(s *hubSource) { s.retentionExpired(gen) })
	})
}

// cancelRetention stops a pending release. Stop can lose the race with a timer
// that already fired, so the generation counter -- not the Stop result -- is
// what makes the late command a no-op.
func (s *hubSource) cancelRetention() {
	s.hub.noteIdle(s, false)
	if s.retention == nil {
		return
	}
	s.retention.Stop()
	s.retention = nil
	s.idleGen++
}

func (s *hubSource) retentionExpired(gen uint64) {
	if gen != s.idleGen {
		return
	}
	s.retention = nil
	if len(s.subs) > 0 || len(s.waiters) > 0 {
		return
	}
	s.stopping = true
}

func (s *hubSource) stats() hubSourceStats {
	reply := make(chan hubSourceStats, 1)
	if !s.post(func(s *hubSource) {
		reply <- hubSourceStats{
			subscribers:   len(s.subs),
			waiters:       len(s.waiters),
			initialized:   s.state == hubReady,
			accounted:     s.accounted,
			demandMetrics: s.demandMetrics,
			demandNodes:   s.demandNodes,
		}
	}) {
		return hubSourceStats{}
	}
	select {
	case stats := <-reply:
		return stats
	case <-s.stopped:
		select {
		case stats := <-reply:
			return stats
		default:
			return hubSourceStats{}
		}
	}
}

type hubAttachResult struct {
	sub *hubSubscription
	rev *hubRevision
	err error
}

// hubWaiter is one in-flight attach. reply is buffered so the actor never
// blocks on a caller that has already given up; sub is actor-owned and lets a
// withdrawal release a subscription that was granted concurrently.
type hubWaiter struct {
	reply  chan hubAttachResult
	demand hubDemand
	sub    *hubSubscription
}

func (w *hubWaiter) serve(sub *hubSubscription, rev *hubRevision, err error) {
	w.sub = sub
	w.reply <- hubAttachResult{sub: sub, rev: rev, err: err}
}

// hubSubscription is one Live stream's handle on a shared source: a
// level-triggered wakeup channel, lock-free access to the latest revision, and
// a done channel that closes when the source releases it.
type hubSubscription struct {
	src    *hubSource
	demand hubDemand
	notify chan struct{}
	done   chan struct{}

	// reason is the terminal reason the source released this subscription
	// with, published atomically because Close can race the source's own
	// teardown.
	reason atomic.Pointer[string]

	closeOnce  sync.Once
	finishOnce sync.Once
}

// Notify fires when a newer revision exists. It carries no payload and
// coalesces: one wakeup may cover any number of events.
func (sub *hubSubscription) Notify() <-chan struct{} { return sub.notify }

// Done closes when the source has released this subscription.
func (sub *hubSubscription) Done() <-chan struct{} { return sub.done }

// Reason is the source's terminal reason once Done has closed: "auth",
// "watch-failed" or "shutdown" for a source that died, and empty when the
// subscription was simply released.
func (sub *hubSubscription) Reason() string {
	if reason := sub.reason.Load(); reason != nil {
		return *reason
	}
	return ""
}

// Revision returns the latest published revision. It is immutable: render from
// a cloneTableForRender copy, never from its table directly.
func (sub *hubSubscription) Revision() *hubRevision { return sub.src.current.Load() }

// Close detaches the subscription. It is safe to call more than once and from
// any goroutine.
func (sub *hubSubscription) Close() {
	sub.closeOnce.Do(func() {
		if !sub.src.post(func(s *hubSource) { s.detach(sub) }) {
			sub.finish("")
		}
	})
}

func (sub *hubSubscription) wake() {
	select {
	case sub.notify <- struct{}{}:
	default:
	}
}

func (sub *hubSubscription) finish(reason string) {
	sub.finishOnce.Do(func() {
		sub.reason.Store(&reason)
		close(sub.done)
	})
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

// hubApplyEvent folds one watch event into a NEW Table value: the row slice is
// copied first, so the previously published revision -- which a subscriber may
// be rendering right now -- keeps its own rows. Row objects are shared by
// reference; the merge replaces them wholesale rather than editing in place,
// so no subscriber can observe a half-applied object.
//
// It also reports the accounted-bytes delta the merge produced: a replacement
// subtracts the old row and adds the new one, a delete subtracts, an add adds.
// Rows the event does not name are never measured.
func hubApplyEvent(prev *kube.Table, ev *kube.WatchEvent) (*kube.Table, int64) {
	next := *prev
	next.Rows = make([]kube.Row, len(prev.Rows))
	copy(next.Rows, prev.Rows)
	return &next, mergeTableEvent(&next, ev)
}

// hubTableBytes is the full retained-state estimate for one Table: encoded
// metadata plus every current row. It is the initial-list and relist
// measurement; events adjust the result rather than repeating this walk.
func hubTableBytes(table *kube.Table) int64 {
	total := hubTableMetaBytes(table)
	for i := range table.Rows {
		total += hubRowBytes(&table.Rows[i])
	}
	return total
}

// hubTableMetaBytes estimates the encoded size of the Table's non-row state:
// the column definitions the rows' cells are read through, the cluster list,
// and the consistency point.
func hubTableMetaBytes(table *kube.Table) int64 {
	total := int64(len(table.ResourceVersion) + 2)
	for i := range table.Columns {
		col := &table.Columns[i]
		total += int64(len(col.Name)+len(col.Type)+len(col.Format)+len(col.Description)+len(col.Class)+len(col.Label)) + 12
	}
	for _, cluster := range table.Clusters {
		total += int64(len(cluster)) + 3
	}
	return total
}

// hubRowBytes estimates one retained row: its printed cells plus the raw
// object the custom-column and detail paths read.
func hubRowBytes(row *kube.Row) int64 {
	total := int64(len(row.Cluster)) + 2
	for _, cell := range row.Cells {
		total += hubValueBytes(cell) + 1
	}
	return total + hubValueBytes(row.Object)
}

// hubValueBytes estimates the encoded JSON size of one decoded value. It is a
// deliberate approximation -- allocation-free and error-free, unlike a real
// encode on every watch event -- and it is only ever compared against a
// configured bound that already carries cacheAccountingHeadroom.
func hubValueBytes(v any) int64 {
	switch value := v.(type) {
	case nil:
		return 4 // null
	case string:
		return int64(len(value)) + 2 // quotes
	case bool:
		return 5
	case map[string]any:
		total := int64(2) // braces
		for key, item := range value {
			total += int64(len(key)) + 4 // "key":
			total += hubValueBytes(item)
		}
		if len(value) > 1 {
			total += int64(len(value)) - 1 // commas
		}
		return total
	case []any:
		total := int64(2) // brackets
		for _, item := range value {
			total += hubValueBytes(item)
		}
		if len(value) > 1 {
			total += int64(len(value)) - 1 // commas
		}
		return total
	default:
		// Numbers and anything the decoder produced that is not a container:
		// one short scalar literal.
		return 8
	}
}
