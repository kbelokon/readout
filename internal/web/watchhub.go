package web

// watchhub.go is the process-local WatchHub: the owner of every upstream
// Kubernetes list+watch a Live stream reads from. One source per
// credential/cluster/resource/namespace/selector key (watchHubKey) serves
// every subscriber on this pod, so a hundred browsers on one list cost the
// apiserver one LIST and one watch instead of a hundred of each.
//
// Each source is a single-goroutine ACTOR: subscribe, unsubscribe, the initial
// list result, watch events and retention expiry all arrive as commands on one
// mailbox, so the source's state needs no locks and every ordering question
// ("did this event land before that attach?") has one answer. The only state
// read outside the actor is the latest published revision, held in an atomic
// pointer.
//
// A revision is immutable once published: events are applied by copying the
// row slice and swapping the pointer, never by editing a table a subscriber
// may be rendering. Subscribers are notified level-triggered through a
// capacity-one mailbox that only ever means "a newer revision exists", so a
// slow browser can neither queue unbounded events nor block the source.
//
// The hub map is guarded by an ordinary mutex, held only to find, insert or
// remove a source pointer -- never while waiting on an actor.

import (
	"context"
	"errors"
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

// Results reported to the metrics sink for one source lookup.
const (
	hubSourceCreated = "created"
	hubSourceReused  = "reused"
	hubSourceFailed  = "failed"
)

var (
	// errHubSourceLimit is the per-pod distinct-source bound: the scope asked
	// for would be a NEW upstream watch and there is no room for one.
	errHubSourceLimit = errors.New("live source limit reached")

	// errHubSourceGone means the attach raced the source's own shutdown. It is
	// internal to the hub: Subscribe retries against a fresh source.
	errHubSourceGone = errors.New("live source is shutting down")
)

// hubClock is the hub's time surface. Sources schedule the retention window
// through it, so a test can release a 30-second window without waiting.
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
}

type noopHubMetrics struct{}

func (noopHubMetrics) observeHubSource(string)   {}
func (noopHubMetrics) observeHubCounts(int, int) {}

// hubListFunc performs the source's authoritative Table LIST. hubWatchFunc
// opens one watch from a resourceVersion. Both are injected so the hub owns
// sharing and lifecycle while the caller owns credentials and HTTP.
type (
	hubListFunc  func(ctx context.Context) (kube.Table, error)
	hubWatchFunc func(ctx context.Context, resourceVersion string) (streamTableWatch, error)
)

// hubSourceSpec is everything needed to CREATE a source. Only the key
// participates in sharing: an attach that matches an existing source ignores
// the rest, because a matching key means the same upstream request under the
// same identity.
type hubSourceSpec struct {
	key   watchHubKey
	list  hubListFunc
	watch hubWatchFunc
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
	clock   hubClock
	metrics hubMetricsSink

	mu          sync.Mutex
	sources     map[watchHubKey]*hubSource
	subscribers int
}

func newWatchHub(ctx context.Context, limits liveLimits, clock hubClock, metrics hubMetricsSink) *watchHub {
	if clock == nil {
		clock = realHubClock{}
	}
	if metrics == nil {
		metrics = noopHubMetrics{}
	}
	return &watchHub{
		ctx:     ctx,
		limits:  limits,
		clock:   clock,
		metrics: metrics,
		sources: map[watchHubKey]*hubSource{},
	}
}

// Subscribe attaches one Live stream to the source for spec.key, creating that
// source if this is the first attach. It returns the current revision, so a
// subscriber renders from the shared state immediately and only then waits for
// notifications. While the source is initializing the caller blocks on ITS OWN
// context; a caller giving up never cancels the shared initialization.
func (h *watchHub) Subscribe(ctx context.Context, spec *hubSourceSpec) (*hubSubscription, *hubRevision, error) {
	for range hubAttachAttempts {
		if err := h.ctx.Err(); err != nil {
			return nil, nil, err
		}
		src, err := h.source(spec)
		if err != nil {
			return nil, nil, err
		}
		sub, rev, err := src.attach(ctx)
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
	h.mu.Lock()
	defer h.mu.Unlock()
	if src, ok := h.sources[spec.key]; ok {
		h.metrics.observeHubSource(hubSourceReused)
		return src, nil
	}
	if len(h.sources) >= h.limits.maxSources {
		return nil, errHubSourceLimit
	}
	src := newHubSource(h, spec)
	h.sources[spec.key] = src
	h.metrics.observeHubSource(hubSourceCreated)
	h.observeCountsLocked()
	go src.run()
	return src, nil
}

// remove drops a source from the map, but only if it is still the source
// registered for its key: a replacement created after this one started
// shutting down must survive.
func (h *watchHub) remove(src *hubSource) {
	h.mu.Lock()
	defer h.mu.Unlock()
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

func (h *watchHub) observeCountsLocked() {
	h.metrics.observeHubCounts(len(h.sources), h.subscribers)
}

func (h *watchHub) sourceCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.sources)
}

// sourceStats reports one source's actor-owned counters. The map lock is
// released before the actor is asked, because the actor itself takes that lock.
func (h *watchHub) sourceStats(key *watchHubKey) hubSourceStats {
	h.mu.Lock()
	src, ok := h.sources[*key]
	h.mu.Unlock()
	if !ok {
		return hubSourceStats{}
	}
	return src.stats()
}

// hubCommand is one unit of work executed on a source's own goroutine.
type hubCommand func(*hubSource)

// hubSourceStats is a point-in-time view of a source, answered by the actor so
// it can never race the state it reports.
type hubSourceStats struct {
	subscribers int
	waiters     int
	revision    uint64
	initialized bool
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
	revNum   uint64
	table    *kube.Table
	lastRV   string
	overlays renderOverlays
	watch    streamTableWatch
	subs     map[*hubSubscription]struct{}
	waiters  []*hubWaiter

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
		s.fail(err)
		return
	}
	s.state = hubReady
	s.table = table
	s.lastRV = table.ResourceVersion
	s.publish(table, false)
	for _, waiter := range s.waiters {
		waiter.serve(s.newSubscription(), s.current.Load(), nil)
	}
	s.waiters = nil
	s.checkIdle()
	s.startWatch()
}

// startWatch opens the source's watch and pumps its events into the mailbox.
// The reader is bound to the source context, and every hand-off checks that
// the source is still running, so a stopped source retains no goroutine and no
// upstream body.
func (s *hubSource) startWatch() {
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

// watchEnded handles the end of the source's watch attempt. Recovery
// (re-watch with backoff, relist on 410, the terminal taxonomy) is the
// lifecycle work that follows; for now an ended watch ends the source, and
// the next attach starts a fresh one.
func (s *hubSource) watchEnded(err error) {
	if err == nil {
		err = errors.New("live watch ended")
	}
	s.fail(err)
}

// applyEvent folds one watch event into a NEW retained table and publishes it.
// Bookmarks carry no rows: they only advance the re-watch point, so they
// publish nothing.
func (s *hubSource) applyEvent(ev *kube.WatchEvent) {
	if s.state != hubReady {
		return
	}
	if ev.ResourceVersion != "" {
		s.lastRV = ev.ResourceVersion
	}
	if ev.Type == kube.WatchBookmark {
		return
	}
	s.table = hubApplyEvent(s.table, ev)
	s.publish(s.table, false)
}

// publish stores the next immutable revision and wakes every subscriber. The
// store happens BEFORE the wakeups, so a woken subscriber always finds at
// least the revision it was woken for.
func (s *hubSource) publish(table *kube.Table, forceSnapshot bool) {
	s.revNum++
	s.current.Store(&hubRevision{
		num:           s.revNum,
		table:         table,
		rv:            s.lastRV,
		forceSnapshot: forceSnapshot,
		overlays:      s.overlays,
		eventAt:       s.hub.clock.Now(),
	})
	for sub := range s.subs {
		sub.wake()
	}
}

// fail records the source's terminal error, hands it to every waiter that was
// still expecting a snapshot, and ends the source. Waiters are served before
// the source leaves the map so a burst of attaches sees ONE failure rather
// than each starting a doomed replacement.
func (s *hubSource) fail(err error) {
	s.state = hubFailed
	s.err = err
	s.hub.metrics.observeHubSource(hubSourceFailed)
	for _, waiter := range s.waiters {
		waiter.serve(nil, nil, err)
	}
	s.waiters = nil
	s.stopping = true
}

// stop is the single teardown path: leave the map first so no new attach can
// find this source, then cancel the source context (which unblocks the watch
// reader), close the upstream watch, release the subscribers, and finally open
// the stopped gate that tells racing attaches to retry elsewhere.
func (s *hubSource) stop() {
	s.hub.remove(s)
	s.cancel()
	if s.watch != nil {
		_ = s.watch.Close()
		s.watch = nil
	}
	for sub := range s.subs {
		delete(s.subs, sub)
		s.hub.noteSubscribers(-1)
		sub.finish()
	}
	for _, waiter := range s.waiters {
		waiter.serve(nil, nil, s.stopErr())
	}
	s.waiters = nil
	close(s.stopped)
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
func (s *hubSource) attach(ctx context.Context) (*hubSubscription, *hubRevision, error) {
	waiter := &hubWaiter{reply: make(chan hubAttachResult, 1)}
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
		waiter.serve(s.newSubscription(), s.current.Load(), nil)
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

func (s *hubSource) newSubscription() *hubSubscription {
	sub := &hubSubscription{
		src:    s,
		notify: make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
	s.subs[sub] = struct{}{}
	s.hub.noteSubscribers(1)
	return sub
}

// detach releases one subscription. The source itself survives: it keeps
// serving everyone else, and an empty source starts its retention window.
func (s *hubSource) detach(sub *hubSubscription) {
	if _, ok := s.subs[sub]; !ok {
		return
	}
	delete(s.subs, sub)
	s.hub.noteSubscribers(-1)
	sub.finish()
	s.checkIdle()
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
	gen := s.idleGen
	s.retention = s.hub.clock.AfterFunc(hubSourceRetention, func() {
		s.post(func(s *hubSource) { s.retentionExpired(gen) })
	})
}

// cancelRetention stops a pending release. Stop can lose the race with a timer
// that already fired, so the generation counter -- not the Stop result -- is
// what makes the late command a no-op.
func (s *hubSource) cancelRetention() {
	if s.retention == nil {
		return
	}
	s.retention.Stop()
	s.retention = nil
	s.idleGen++
}

func (s *hubSource) retentionExpired(gen uint64) {
	if gen != s.idleGen || s.retention == nil {
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
			subscribers: len(s.subs),
			waiters:     len(s.waiters),
			revision:    s.revNum,
			initialized: s.state == hubReady,
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
	reply chan hubAttachResult
	sub   *hubSubscription
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
	notify chan struct{}
	done   chan struct{}

	closeOnce  sync.Once
	finishOnce sync.Once
}

// Notify fires when a newer revision exists. It carries no payload and
// coalesces: one wakeup may cover any number of events.
func (sub *hubSubscription) Notify() <-chan struct{} { return sub.notify }

// Done closes when the source has released this subscription.
func (sub *hubSubscription) Done() <-chan struct{} { return sub.done }

// Revision returns the latest published revision. It is immutable: render from
// a cloneTableForRender copy, never from its table directly.
func (sub *hubSubscription) Revision() *hubRevision { return sub.src.current.Load() }

// Close detaches the subscription. It is safe to call more than once and from
// any goroutine.
func (sub *hubSubscription) Close() {
	sub.closeOnce.Do(func() {
		if !sub.src.post(func(s *hubSource) { s.detach(sub) }) {
			sub.finish()
		}
	})
}

func (sub *hubSubscription) wake() {
	select {
	case sub.notify <- struct{}{}:
	default:
	}
}

func (sub *hubSubscription) finish() {
	sub.finishOnce.Do(func() { close(sub.done) })
}

// hubApplyEvent folds one watch event into a NEW Table value: the row slice is
// copied first, so the previously published revision -- which a subscriber may
// be rendering right now -- keeps its own rows. Row objects are shared by
// reference; the merge replaces them wholesale rather than editing in place,
// so no subscriber can observe a half-applied object.
func hubApplyEvent(prev *kube.Table, ev *kube.WatchEvent) *kube.Table {
	next := *prev
	next.Rows = make([]kube.Row, len(prev.Rows))
	copy(next.Rows, prev.Rows)
	mergeTableEvent(&next, ev)
	return &next
}
