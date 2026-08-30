package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kbelokon/readout/internal/kube"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fakeHubClock is a manually advanced clock: Now only moves when a test says
// so, and AfterFunc callbacks fire from Advance (in their own goroutine, like
// time.AfterFunc) instead of from real elapsed time. The 30-second source
// retention is therefore exercised without a 30-second test.
type fakeHubClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeHubTimer
}

type fakeHubTimer struct {
	clock *fakeHubClock
	at    time.Time
	fn    func()
	done  bool
}

func newFakeHubClock() *fakeHubClock {
	return &fakeHubClock{now: time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeHubClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeHubClock) AfterFunc(d time.Duration, f func()) hubTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &fakeHubTimer{clock: c, at: c.now.Add(d), fn: f}
	c.timers = append(c.timers, timer)
	return timer
}

// Advance moves the clock and fires every timer whose deadline has passed.
func (c *fakeHubClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []*fakeHubTimer
	kept := c.timers[:0]
	for _, timer := range c.timers {
		if !timer.done && !timer.at.After(c.now) {
			timer.done = true
			due = append(due, timer)
			continue
		}
		if !timer.done {
			kept = append(kept, timer)
		}
	}
	c.timers = kept
	c.mu.Unlock()
	for _, timer := range due {
		go timer.fn()
	}
}

func (t *fakeHubTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if t.done {
		return false
	}
	t.done = true
	return true
}

// hubTestUpstream is a scripted list+watch pair: it counts attempts, can hold
// the initial LIST open while a test lines up racing subscribers, and feeds
// watch events through a channel.
type hubTestUpstream struct {
	mu       sync.Mutex
	lists    int
	watches  int
	watchRVs []string
	table    kube.Table
	listErr  error
	watchErr error
	closes   int
	last     *hubTestWatch

	// overlays counts the demand-driven join polls and records the demand each
	// one was made with, so a test can prove the count does not scale with
	// subscribers.
	overlays      int
	overlayDemand []hubDemand

	// listGate blocks the initial LIST until the test closes it, so every
	// concurrent attach is guaranteed to arrive while the source is still
	// initializing.
	listGate chan struct{}
	events   chan kube.WatchEvent
	opened   chan struct{}
}

func newHubTestUpstream(table *kube.Table) *hubTestUpstream {
	return &hubTestUpstream{
		table:    *table,
		listGate: make(chan struct{}),
		events:   make(chan kube.WatchEvent),
		opened:   make(chan struct{}, 16),
	}
}

func (u *hubTestUpstream) openGate() { close(u.listGate) }

func (u *hubTestUpstream) spec(key *watchHubKey) *hubSourceSpec {
	return &hubSourceSpec{key: *key, list: u.list, watch: u.watch, overlay: u.overlay}
}

// overlay is the shared join poll. It records the demand it was asked for and
// returns a distinguishable overlay per call so a test can see the revision
// change.
func (u *hubTestUpstream) overlay(_ context.Context, demand hubDemand) renderOverlays {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.overlays++
	var overlays renderOverlays
	if demand.metrics {
		overlays.metrics = map[string][2]float64{"poll": {float64(u.overlays), 0}}
	}
	if demand.nodes {
		overlays.nodes = map[string]map[string]any{}
	}
	u.overlayDemand = append(u.overlayDemand, demand)
	return overlays
}

// setTable swaps what the NEXT list returns, so a relist is distinguishable
// from the initial snapshot.
func (u *hubTestUpstream) setTable(table *kube.Table) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.table = *table
}

func (u *hubTestUpstream) overlayCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.overlays
}

// setListErr swaps the LIST outcome mid-test, so a relist can fail after the
// initial list succeeded.
func (u *hubTestUpstream) setListErr(err error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.listErr = err
}

// failWatch ends the CURRENT attempt with a specific upstream error instead of
// the clean EOF Close produces.
func (u *hubTestUpstream) failWatch(t *testing.T, err error) {
	t.Helper()
	u.mu.Lock()
	w := u.last
	u.mu.Unlock()
	if w == nil {
		t.Fatal("no watch attempt to fail")
	}
	w.fail(err)
}

// watchAttempts reports the resourceVersion each attempt started from, which is
// how a test proves a re-watch resumed instead of relisting.
func (u *hubTestUpstream) watchAttempts() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.watchRVs...)
}

func (u *hubTestUpstream) list(ctx context.Context) (kube.Table, error) {
	u.mu.Lock()
	u.lists++
	u.mu.Unlock()
	select {
	case <-u.listGate:
	case <-ctx.Done():
		return kube.Table{}, ctx.Err()
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.listErr != nil {
		return kube.Table{}, u.listErr
	}
	return cloneTableForRender(&u.table), nil
}

func (u *hubTestUpstream) watch(ctx context.Context, rv string) (streamTableWatch, error) {
	u.mu.Lock()
	u.watches++
	u.watchRVs = append(u.watchRVs, rv)
	if err := u.watchErr; err != nil {
		u.mu.Unlock()
		return nil, err
	}
	w := &hubTestWatch{up: u, ctx: ctx, events: u.events, closed: make(chan struct{})}
	u.last = w
	u.mu.Unlock()
	select {
	case u.opened <- struct{}{}:
	default:
	}
	return w, nil
}

// endWatch ends the current attempt the way a clean upstream close does.
func (u *hubTestUpstream) endWatch(t *testing.T) {
	t.Helper()
	u.mu.Lock()
	w := u.last
	u.mu.Unlock()
	if w == nil {
		t.Fatal("no watch attempt to end")
	}
	_ = w.Close()
}

func (u *hubTestUpstream) counts() (lists, watches, closes int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.lists, u.watches, u.closes
}

// emit publishes one watch event to whichever attempt is currently reading.
func (u *hubTestUpstream) emit(t *testing.T, ev *kube.WatchEvent) {
	t.Helper()
	select {
	case u.events <- *ev:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out emitting a watch event: no reader")
	}
}

type hubTestWatch struct {
	up     *hubTestUpstream
	ctx    context.Context
	events chan kube.WatchEvent
	closed chan struct{}
	once   sync.Once

	// failErr is the error Next reports instead of io.EOF when the test ended
	// this attempt with a specific upstream failure (a 410, a 403).
	failMu sync.Mutex
	failed error
}

// fail ends the attempt with err rather than a clean close.
func (w *hubTestWatch) fail(err error) {
	w.failMu.Lock()
	w.failed = err
	w.failMu.Unlock()
	_ = w.Close()
}

func (w *hubTestWatch) failErr() error {
	w.failMu.Lock()
	defer w.failMu.Unlock()
	return w.failed
}

func (w *hubTestWatch) Next() (kube.WatchEvent, error) {
	select {
	case ev := <-w.events:
		return ev, nil
	case <-w.closed:
		if err := w.failErr(); err != nil {
			return kube.WatchEvent{}, err
		}
		return kube.WatchEvent{}, io.EOF
	case <-w.ctx.Done():
		return kube.WatchEvent{}, w.ctx.Err()
	}
}

func (w *hubTestWatch) Close() error {
	w.once.Do(func() {
		w.up.mu.Lock()
		w.up.closes++
		w.up.mu.Unlock()
		close(w.closed)
	})
	return nil
}

// recordingHubMetrics captures the hub's source-lifecycle observations.
type recordingHubMetrics struct {
	mu          sync.Mutex
	results     map[string]int
	sources     int
	subscribers int
	cache       int64
	connections int
	relists     int
	snapshots   []int64
}

func newRecordingHubMetrics() *recordingHubMetrics {
	return &recordingHubMetrics{results: map[string]int{}}
}

func (m *recordingHubMetrics) observeHubSource(result string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.results[result]++
}

func (m *recordingHubMetrics) observeHubCounts(sources, subscribers int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sources = sources
	m.subscribers = subscribers
}

func (m *recordingHubMetrics) observeHubCache(bytes int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cache = bytes
}

func (m *recordingHubMetrics) observeHubConnections(active int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.connections = active
}

func (m *recordingHubMetrics) observeHubRelist() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.relists++
}

func (m *recordingHubMetrics) observeHubSnapshotBytes(bytes int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snapshots = append(m.snapshots, bytes)
}

func (m *recordingHubMetrics) relistCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.relists
}

func (m *recordingHubMetrics) snapshotSamples() []int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int64(nil), m.snapshots...)
}

func (m *recordingHubMetrics) connectionCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.connections
}

func (m *recordingHubMetrics) cacheBytes() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cache
}

func (m *recordingHubMetrics) result(name string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.results[name]
}

func (m *recordingHubMetrics) counts() (sources, subscribers int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sources, m.subscribers
}

func hubTestKey(namespace string) *watchHubKey {
	return &watchHubKey{identity: "id", cluster: "test", resource: "v1/pods", namespace: namespace}
}

func hubTestTable(rv string, names ...string) *kube.Table {
	table := &kube.Table{
		Columns:         []kube.Column{{Name: "Name"}},
		ResourceVersion: rv,
	}
	for _, name := range names {
		table.Rows = append(table.Rows, hubTestRow(name))
	}
	return table
}

func hubTestRow(name string) kube.Row {
	return kube.Row{
		Cells: []any{name},
		Object: map[string]any{
			"metadata": map[string]any{"name": name, "namespace": "ns"},
		},
	}
}

func hubTestEvent(kind kube.WatchEventType, rv, name string) *kube.WatchEvent {
	return &kube.WatchEvent{
		Type:            kind,
		Table:           kube.Table{Rows: []kube.Row{hubTestRow(name)}},
		ResourceVersion: rv,
	}
}

func hubRowNames(table *kube.Table) []string {
	names := make([]string, 0, len(table.Rows))
	for i := range table.Rows {
		names = append(names, nestedString(table.Rows[i].Object, "metadata", "name"))
	}
	return names
}

// newTestWatchHub builds a hub with the manual clock, the production timing
// policy and a recording sink. The hub context is canceled by cleanup so no
// source outlives its test.
func newTestWatchHub(t *testing.T, limits liveLimits) (*watchHub, *fakeHubClock, *recordingHubMetrics) {
	t.Helper()
	tuning := defaultStreamTuning()
	return newTestWatchHubTuned(t, limits, &tuning)
}

func newTestWatchHubTuned(t *testing.T, limits liveLimits, tuning *streamTuning) (*watchHub, *fakeHubClock, *recordingHubMetrics) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	clock := newFakeHubClock()
	metrics := newRecordingHubMetrics()
	return newWatchHub(ctx, limits, tuning, clock, metrics), clock, metrics
}

func testHubLimits() liveLimits {
	return liveLimits{maxConnections: 512, maxSources: 128, maxCacheAccountedBytes: 1 << 27}
}

// waitFor polls until cond holds, so tests never sleep for a fixed duration.
func waitFor(t *testing.T, label string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", label)
		}
		time.Sleep(time.Millisecond)
	}
}

// advanceUntil drives the fake clock until cond holds. Hub timers are armed
// from the actor goroutine, so one advance can land before the timer it was
// meant to fire even exists; repeating it is how a test reaches a scheduled
// step without waiting out the real delay.
func advanceUntil(t *testing.T, clock *fakeHubClock, step time.Duration, label string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", label)
		}
		clock.Advance(step)
		time.Sleep(time.Millisecond)
	}
}

// waitRevision blocks until the subscription can see revision num or later.
func waitRevision(t *testing.T, sub *hubSubscription, num uint64) *hubRevision {
	t.Helper()
	var rev *hubRevision
	waitFor(t, fmt.Sprintf("revision %d", num), func() bool {
		rev = sub.Revision()
		return rev != nil && rev.num >= num
	})
	return rev
}

func drainNotifications(sub *hubSubscription) int {
	count := 0
	for {
		select {
		case <-sub.Notify():
			count++
		default:
			return count
		}
	}
}

// A hundred browsers opening the same list must cost the cluster one LIST and
// one watch, not one hundred: every attach that arrives while the source is
// still initializing joins that same initialization.
func TestWatchHubConcurrentSubscribesShareOneSource(t *testing.T) {
	const subscribers = 100
	hub, _, metrics := newTestWatchHub(t, testHubLimits())
	up := newHubTestUpstream(hubTestTable("10", "alpha"))
	key := hubTestKey("ns")

	type attach struct {
		sub *hubSubscription
		rev *hubRevision
		err error
	}
	results := make(chan attach, subscribers)
	for range subscribers {
		go func() {
			sub, rev, err := hub.Subscribe(context.Background(), up.spec(key), hubDemand{})
			results <- attach{sub: sub, rev: rev, err: err}
		}()
	}
	waitFor(t, "all subscribers to queue on the initializing source", func() bool {
		stats, ok := hub.sourceStats(key)
		return ok && stats.waiters == subscribers
	})
	up.openGate()

	var first *hubRevision
	for range subscribers {
		got := <-results
		if got.err != nil {
			t.Fatalf("Subscribe failed: %v", got.err)
		}
		if got.rev == nil {
			t.Fatal("Subscribe returned no revision")
		}
		if first == nil {
			first = got.rev
		} else if got.rev != first {
			t.Fatal("subscribers received different revision pointers for one source")
		}
		if n := drainNotifications(got.sub); n != 0 {
			t.Fatalf("fresh subscriber had %d pending notifications, want 0", n)
		}
		t.Cleanup(got.sub.Close)
	}
	waitFor(t, "the source watch to open", func() bool {
		_, watches, _ := up.counts()
		return watches == 1
	})
	lists, watches, _ := up.counts()
	if lists != 1 || watches != 1 {
		t.Fatalf("upstream calls = %d LIST / %d watch, want 1 / 1", lists, watches)
	}
	stats, ok := hub.sourceStats(key)
	if got := stats.subscribers; !ok || got != subscribers {
		t.Fatalf("source subscribers = %d, want %d", got, subscribers)
	}
	if got := metrics.result(hubSourceCreated); got != 1 {
		t.Fatalf("created observations = %d, want 1", got)
	}
	if got := metrics.result(hubSourceReused); got != subscribers-1 {
		t.Fatalf("reused observations = %d, want %d", got, subscribers-1)
	}
	if sources, subs := metrics.counts(); sources != 1 || subs != subscribers {
		t.Fatalf("gauge counts = %d sources / %d subscribers, want 1 / %d", sources, subs, subscribers)
	}
}

// The initial revision is the list itself: numbered 1, carrying the list's
// resourceVersion, no forced snapshot and no join overlays.
func TestWatchHubInitialRevision(t *testing.T) {
	hub, clock, _ := newTestWatchHub(t, testHubLimits())
	up := newHubTestUpstream(hubTestTable("10", "alpha"))
	up.openGate()

	sub, rev, err := hub.Subscribe(context.Background(), up.spec(hubTestKey("ns")), hubDemand{})
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}
	t.Cleanup(sub.Close)
	if rev.num != 1 {
		t.Fatalf("initial revision num = %d, want 1", rev.num)
	}
	if rev.rv != "10" {
		t.Fatalf("initial revision rv = %q, want %q", rev.rv, "10")
	}
	if rev.forceSnapshot {
		t.Fatal("initial revision asked for a forced snapshot")
	}
	if rev.overlays.metrics != nil || rev.overlays.nodes != nil {
		t.Fatal("initial revision carried join overlays nobody asked for")
	}
	if !rev.eventAt.Equal(clock.Now()) {
		t.Fatalf("initial revision eventAt = %v, want the hub clock %v", rev.eventAt, clock.Now())
	}
	if got := hubRowNames(rev.table); len(got) != 1 || got[0] != "alpha" {
		t.Fatalf("initial revision rows = %v, want [alpha]", got)
	}
	waitFor(t, "the watch to resume from the list resourceVersion", func() bool {
		_, watches, _ := up.counts()
		return watches == 1
	})
	up.mu.Lock()
	rvs := append([]string(nil), up.watchRVs...)
	up.mu.Unlock()
	if len(rvs) != 1 || rvs[0] != "10" {
		t.Fatalf("watch resourceVersions = %v, want [10]", rvs)
	}
}

// An event racing the subscribe command is either already represented by the
// returned revision or produces exactly one notification -- never lost, never
// delivered twice.
func TestWatchHubSubscribeRacingEventIsNeitherLostNorDuplicated(t *testing.T) {
	for attempt := range 25 {
		hub, _, _ := newTestWatchHub(t, testHubLimits())
		up := newHubTestUpstream(hubTestTable("10", "alpha"))
		up.openGate()
		key := hubTestKey("ns")

		primary, _, err := hub.Subscribe(context.Background(), up.spec(key), hubDemand{})
		if err != nil {
			t.Fatalf("attempt %d: primary Subscribe failed: %v", attempt, err)
		}
		waitFor(t, "the source watch to open", func() bool {
			_, watches, _ := up.counts()
			return watches == 1
		})

		var (
			wg      sync.WaitGroup
			racer   *hubSubscription
			raceRev *hubRevision
			raceErr error
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			racer, raceRev, raceErr = hub.Subscribe(context.Background(), up.spec(key), hubDemand{})
		}()
		go func() {
			defer wg.Done()
			up.emit(t, hubTestEvent(kube.WatchAdded, "11", "beta"))
		}()
		wg.Wait()
		if raceErr != nil {
			t.Fatalf("attempt %d: racing Subscribe failed: %v", attempt, raceErr)
		}
		waitRevision(t, primary, 2)
		// The revision is stored before subscribers are woken, so seeing it is
		// not enough: one more actor round-trip guarantees that the attach and
		// the publish (with its wakeups) have both finished.
		_, _ = hub.sourceStats(key)

		sawBeta := len(hubRowNames(raceRev.table)) == 2
		notifications := drainNotifications(racer)
		switch {
		case sawBeta && notifications != 0:
			t.Fatalf("attempt %d: event was in the attach revision AND notified", attempt)
		case !sawBeta && notifications != 1:
			t.Fatalf("attempt %d: event missing from the attach revision produced %d notifications, want 1", attempt, notifications)
		}
		if got := hubRowNames(racer.Revision().table); len(got) != 2 {
			t.Fatalf("attempt %d: latest revision rows = %v, want alpha and beta", attempt, got)
		}
		racer.Close()
		primary.Close()
	}
}

// Notifications are level-triggered: a subscriber that has not drained its
// capacity-one mailbox sees one wakeup for a burst, and the burst is fully
// represented by the latest revision.
func TestWatchHubNotificationsCoalesce(t *testing.T) {
	hub, _, _ := newTestWatchHub(t, testHubLimits())
	up := newHubTestUpstream(hubTestTable("10", "alpha"))
	up.openGate()

	sub, _, err := hub.Subscribe(context.Background(), up.spec(hubTestKey("ns")), hubDemand{})
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}
	t.Cleanup(sub.Close)
	waitFor(t, "the source watch to open", func() bool {
		_, watches, _ := up.counts()
		return watches == 1
	})
	for i := range 5 {
		up.emit(t, hubTestEvent(kube.WatchAdded, fmt.Sprintf("1%d", i), fmt.Sprintf("pod-%d", i)))
	}
	rev := waitRevision(t, sub, 6)
	if n := drainNotifications(sub); n != 1 {
		t.Fatalf("burst produced %d notifications, want 1 coalesced", n)
	}
	if got := hubRowNames(rev.table); len(got) != 6 {
		t.Fatalf("latest revision rows = %v, want six", got)
	}
	if rev.rv != "14" {
		t.Fatalf("latest revision rv = %q, want the last event's %q", rev.rv, "14")
	}
}

// Bookmarks advance the re-watch point without publishing a revision, and a
// delete drops the row from the next revision.
func TestWatchHubAppliesDeletesAndBookmarks(t *testing.T) {
	hub, _, _ := newTestWatchHub(t, testHubLimits())
	up := newHubTestUpstream(hubTestTable("10", "alpha", "beta"))
	up.openGate()
	key := hubTestKey("ns")

	sub, rev, err := hub.Subscribe(context.Background(), up.spec(key), hubDemand{})
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}
	t.Cleanup(sub.Close)
	waitFor(t, "the source watch to open", func() bool {
		_, watches, _ := up.counts()
		return watches == 1
	})
	before := rev

	up.emit(t, &kube.WatchEvent{Type: kube.WatchBookmark, ResourceVersion: "11"})
	up.emit(t, hubTestEvent(kube.WatchDeleted, "12", "alpha"))
	next := waitRevision(t, sub, 2)
	if next.num != 2 {
		t.Fatalf("bookmark published a revision: num = %d, want 2", next.num)
	}
	if got := hubRowNames(next.table); len(got) != 1 || got[0] != "beta" {
		t.Fatalf("rows after delete = %v, want [beta]", got)
	}
	if got := hubRowNames(before.table); len(got) != 2 {
		t.Fatalf("the published revision was mutated in place: rows = %v", got)
	}
}

// A published revision is immutable: the render clone the subscribers take may
// be rewritten freely while the source keeps applying events.
func TestWatchHubPublishedRevisionsAreImmutable(t *testing.T) {
	hub, _, _ := newTestWatchHub(t, testHubLimits())
	up := newHubTestUpstream(hubTestTable("10", "alpha"))
	up.openGate()

	sub, rev, err := hub.Subscribe(context.Background(), up.spec(hubTestKey("ns")), hubDemand{})
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}
	t.Cleanup(sub.Close)
	waitFor(t, "the source watch to open", func() bool {
		_, watches, _ := up.counts()
		return watches == 1
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 20 {
			up.emit(t, hubTestEvent(kube.WatchAdded, fmt.Sprintf("2%d", i), fmt.Sprintf("pod-%d", i)))
		}
	}()
	for range 200 {
		current := sub.Revision()
		names := hubRowNames(current.table)
		clone := cloneTableForRender(current.table)
		clone.Rows = append(clone.Rows, hubTestRow("injected"))
		clone.Columns = append(clone.Columns, kube.Column{Name: "Extra"})
		for i := range clone.Rows {
			clone.Rows[i].Cells = append(clone.Rows[i].Cells, "mutated")
		}
		if after := hubRowNames(current.table); len(after) != len(names) {
			t.Fatalf("render clone mutated its revision: %v then %v", names, after)
		}
	}
	<-done
	if got := hubRowNames(waitRevision(t, sub, 21).table); len(got) != 21 {
		t.Fatalf("final revision rows = %d, want 21", len(got))
	}
	if rev.num != 1 {
		t.Fatalf("the attach revision number changed to %d", rev.num)
	}
}

// The last subscriber leaving keeps the source for the retention window so
// ordinary navigation churn does not re-LIST, then releases it.
func TestWatchHubRetentionReleasesIdleSource(t *testing.T) {
	hub, clock, metrics := newTestWatchHub(t, testHubLimits())
	up := newHubTestUpstream(hubTestTable("10", "alpha"))
	up.openGate()
	key := hubTestKey("ns")

	sub, _, err := hub.Subscribe(context.Background(), up.spec(key), hubDemand{})
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}
	waitFor(t, "the source watch to open", func() bool {
		_, watches, _ := up.counts()
		return watches == 1
	})
	sub.Close()
	requireSignal(t, sub.Done(), "closed subscription")
	waitFor(t, "the source to drop its subscriber", func() bool {
		stats, ok := hub.sourceStats(key)
		return ok && stats.subscribers == 0
	})
	if hub.sourceCount() != 1 {
		t.Fatal("the idle source was released before its retention window")
	}
	if _, _, closes := up.counts(); closes != 0 {
		t.Fatal("the upstream watch closed before the retention window")
	}

	clock.Advance(hubSourceRetention)
	waitFor(t, "the idle source to be released", func() bool { return hub.sourceCount() == 0 })
	waitFor(t, "the upstream watch to close", func() bool {
		_, _, closes := up.counts()
		return closes == 1
	})
	if sources, subs := metrics.counts(); sources != 0 || subs != 0 {
		t.Fatalf("gauge counts after release = %d sources / %d subscribers, want 0 / 0", sources, subs)
	}
}

// Re-subscribing inside the retention window cancels the release and reuses
// the retained state: no second LIST, and no late timer tears the source down.
func TestWatchHubResubscribeCancelsRetention(t *testing.T) {
	hub, clock, _ := newTestWatchHub(t, testHubLimits())
	up := newHubTestUpstream(hubTestTable("10", "alpha"))
	up.openGate()
	key := hubTestKey("ns")

	first, _, err := hub.Subscribe(context.Background(), up.spec(key), hubDemand{})
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}
	waitFor(t, "the source watch to open", func() bool {
		_, watches, _ := up.counts()
		return watches == 1
	})
	first.Close()
	waitFor(t, "the source to drop its subscriber", func() bool {
		stats, ok := hub.sourceStats(key)
		return ok && stats.subscribers == 0
	})

	second, rev, err := hub.Subscribe(context.Background(), up.spec(key), hubDemand{})
	if err != nil {
		t.Fatalf("re-Subscribe failed: %v", err)
	}
	t.Cleanup(second.Close)
	if rev == nil || rev.num != 1 {
		t.Fatal("re-subscribe did not reuse the retained revision")
	}
	clock.Advance(2 * hubSourceRetention)
	// The canceled timer must not fire late: give the hub a moment to prove it
	// keeps the source that now has a subscriber.
	waitFor(t, "the retained source to stay attached", func() bool {
		stats, ok := hub.sourceStats(key)
		return ok && stats.subscribers == 1
	})
	if hub.sourceCount() != 1 {
		t.Fatal("a canceled retention timer released a source with a subscriber")
	}
	lists, watches, closes := up.counts()
	if lists != 1 || watches != 1 || closes != 0 {
		t.Fatalf("upstream calls = %d LIST / %d watch / %d close, want 1 / 1 / 0", lists, watches, closes)
	}
}

// A failed initial LIST is delivered to every waiter that joined it, once, and
// the source is discarded so the next attach starts a fresh attempt.
func TestWatchHubInitialListFailureReachesEveryWaiter(t *testing.T) {
	const waiters = 100
	hub, _, metrics := newTestWatchHub(t, testHubLimits())
	up := newHubTestUpstream(&kube.Table{})
	up.listErr = errors.New("boom")
	key := hubTestKey("ns")

	errs := make(chan error, waiters)
	for range waiters {
		go func() {
			_, _, err := hub.Subscribe(context.Background(), up.spec(key), hubDemand{})
			errs <- err
		}()
	}
	waitFor(t, "all waiters to queue on the initializing source", func() bool {
		stats, ok := hub.sourceStats(key)
		return ok && stats.waiters == waiters
	})
	up.openGate()
	for range waiters {
		if err := <-errs; err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("waiter error = %v, want the initial LIST failure", err)
		}
	}
	waitFor(t, "the failed source to be discarded", func() bool { return hub.sourceCount() == 0 })
	if lists, watches, _ := up.counts(); lists != 1 || watches != 0 {
		t.Fatalf("upstream calls = %d LIST / %d watch, want 1 / 0", lists, watches)
	}
	if got := metrics.result(hubSourceFailed); got != 1 {
		t.Fatalf("failed observations = %d, want 1", got)
	}
}

// Source initialization belongs to the source, not to the first subscriber:
// that request going away must not cancel the LIST the others are waiting on.
func TestWatchHubInitializationSurvivesTheFirstSubscriber(t *testing.T) {
	hub, _, _ := newTestWatchHub(t, testHubLimits())
	up := newHubTestUpstream(hubTestTable("10", "alpha"))
	key := hubTestKey("ns")

	ctx, cancel := context.WithCancel(context.Background())
	firstErr := make(chan error, 1)
	go func() {
		_, _, err := hub.Subscribe(ctx, up.spec(key), hubDemand{})
		firstErr <- err
	}()
	waitFor(t, "the first subscriber to queue", func() bool {
		stats, ok := hub.sourceStats(key)
		return ok && stats.waiters == 1
	})
	secondErr := make(chan error, 1)
	var second *hubSubscription
	go func() {
		sub, _, err := hub.Subscribe(context.Background(), up.spec(key), hubDemand{})
		second = sub
		secondErr <- err
	}()
	waitFor(t, "the second subscriber to queue", func() bool {
		stats, ok := hub.sourceStats(key)
		return ok && stats.waiters == 2
	})

	cancel()
	if err := <-firstErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Subscribe error = %v, want context.Canceled", err)
	}
	up.openGate()
	if err := <-secondErr; err != nil {
		t.Fatalf("second Subscribe failed: %v", err)
	}
	t.Cleanup(second.Close)
	waitFor(t, "the source to settle with one subscriber", func() bool {
		stats, ok := hub.sourceStats(key)
		return ok && stats.subscribers == 1
	})
	if lists, _, _ := up.counts(); lists != 1 {
		t.Fatalf("LIST attempts = %d, want the one shared initialization", lists)
	}
}

// A canceled attach never leaves a registered subscriber behind: the retention
// window still starts as if the source had never been joined.
func TestWatchHubCanceledAttachLeavesNoSubscriber(t *testing.T) {
	hub, clock, _ := newTestWatchHub(t, testHubLimits())
	up := newHubTestUpstream(hubTestTable("10", "alpha"))
	key := hubTestKey("ns")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := hub.Subscribe(ctx, up.spec(key), hubDemand{})
		done <- err
	}()
	waitFor(t, "the subscriber to queue", func() bool {
		stats, ok := hub.sourceStats(key)
		return ok && stats.waiters == 1
	})
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Subscribe error = %v, want context.Canceled", err)
	}
	up.openGate()
	waitFor(t, "the abandoned source to settle", func() bool {
		stats, _ := hub.sourceStats(key)
		return stats.initialized && stats.subscribers == 0 && stats.waiters == 0
	})
	clock.Advance(hubSourceRetention)
	waitFor(t, "the abandoned source to be released", func() bool { return hub.sourceCount() == 0 })
}

// Different keys are different sources, and the source limit rejects a new key
// without disturbing the sources already running.
func TestWatchHubSourceLimitRejectsNewKeys(t *testing.T) {
	limits := testHubLimits()
	limits.maxSources = 1
	hub, _, _ := newTestWatchHub(t, limits)
	up := newHubTestUpstream(hubTestTable("10", "alpha"))
	up.openGate()

	sub, _, err := hub.Subscribe(context.Background(), up.spec(hubTestKey("ns")), hubDemand{})
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}
	t.Cleanup(sub.Close)

	if _, _, err := hub.Subscribe(context.Background(), up.spec(hubTestKey("other")), hubDemand{}); !errors.Is(err, errHubSourceLimit) {
		t.Fatalf("second key error = %v, want errHubSourceLimit", err)
	}
	// The existing key still joins its running source.
	reused, _, err := hub.Subscribe(context.Background(), up.spec(hubTestKey("ns")), hubDemand{})
	if err != nil {
		t.Fatalf("reuse of an admitted key failed: %v", err)
	}
	t.Cleanup(reused.Close)
	if lists, _, _ := up.counts(); lists != 1 {
		t.Fatalf("LIST attempts = %d, want 1", lists)
	}
}

// Hub shutdown closes every subscription and releases every source.
func TestWatchHubShutdownClosesSources(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	hub := newWatchHub(ctx, testHubLimits(), nil, newFakeHubClock(), newRecordingHubMetrics())
	up := newHubTestUpstream(hubTestTable("10", "alpha"))
	up.openGate()

	sub, _, err := hub.Subscribe(context.Background(), up.spec(hubTestKey("ns")), hubDemand{})
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}
	waitFor(t, "the source watch to open", func() bool {
		_, watches, _ := up.counts()
		return watches == 1
	})

	cancel()
	requireSignal(t, sub.Done(), "subscription closed by hub shutdown")
	if got := sub.Reason(); got != hubTerminalShutdown {
		t.Fatalf("terminal reason = %q, want %q", got, hubTerminalShutdown)
	}
	waitFor(t, "sources to be released", func() bool { return hub.sourceCount() == 0 })
	waitFor(t, "the upstream watch to close", func() bool {
		_, _, closes := up.counts()
		return closes == 1
	})
	if _, _, err := hub.Subscribe(context.Background(), up.spec(hubTestKey("ns")), hubDemand{}); err == nil {
		t.Fatal("Subscribe succeeded after hub shutdown")
	}
}

// A clean upstream EOF is recovery, not a terminal: the source re-watches from
// the last seen resourceVersion after the backoff delay, does NOT relist, and
// every subscriber stays attached across the gap.
func TestWatchHubCleanEOFRewatchesWithoutTerminal(t *testing.T) {
	hub, clock, _ := newTestWatchHub(t, testHubLimits())
	up := newHubTestUpstream(hubTestTable("10", "alpha"))
	up.openGate()
	key := hubTestKey("ns")

	sub, _, err := hub.Subscribe(context.Background(), up.spec(key), hubDemand{})
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}
	t.Cleanup(sub.Close)
	waitFor(t, "the source watch to open", func() bool {
		_, watches, _ := up.counts()
		return watches == 1
	})
	up.emit(t, hubTestEvent(kube.WatchModified, "11", "alpha"))
	waitRevision(t, sub, 2)

	up.endWatch(t)
	advanceUntil(t, clock, time.Minute, "the scheduled re-watch", func() bool {
		_, watches, _ := up.counts()
		return watches == 2
	})

	attempts := up.watchAttempts()
	if len(attempts) != 2 || attempts[1] != "11" {
		t.Fatalf("watch attempts = %v, want the second to resume from the last event 11", attempts)
	}
	select {
	case <-sub.Done():
		t.Fatal("a clean EOF closed the subscription instead of re-watching")
	default:
	}
	if lists, _, _ := up.counts(); lists != 1 {
		t.Fatalf("LIST count = %d, want 1: a clean EOF must not relist", lists)
	}
	if hub.sourceCount() != 1 {
		t.Fatal("a clean EOF discarded the source")
	}
}

// A 410 costs exactly one relist no matter how many browsers are attached, and
// every one of them sees the same single forced-snapshot revision -- the
// discontinuity the delta chain cannot survive.
func TestWatchHubGoneRelistsOnceForEverySubscriber(t *testing.T) {
	const subscribers = 8
	hub, _, _ := newTestWatchHub(t, testHubLimits())
	up := newHubTestUpstream(hubTestTable("10", "alpha"))
	up.openGate()
	key := hubTestKey("ns")

	subs := make([]*hubSubscription, 0, subscribers)
	for range subscribers {
		sub, rev, err := hub.Subscribe(context.Background(), up.spec(key), hubDemand{})
		if err != nil {
			t.Fatalf("Subscribe failed: %v", err)
		}
		if rev.forceSnapshot {
			t.Fatal("the initial revision was marked forceSnapshot")
		}
		t.Cleanup(sub.Close)
		subs = append(subs, sub)
	}
	waitFor(t, "the source watch to open", func() bool {
		_, watches, _ := up.counts()
		return watches == 1
	})

	up.setTable(hubTestTable("20", "alpha", "beta"))
	up.failWatch(t, fmt.Errorf("%w: too old", kube.ErrWatchGone))

	for _, sub := range subs {
		rev := waitRevision(t, sub, 2)
		if !rev.forceSnapshot {
			t.Fatal("the relist revision was not marked forceSnapshot")
		}
		if rev.num != 2 {
			t.Fatalf("relist revision num = %d, want 2: the relist published more than one revision", rev.num)
		}
		if rev.rv != "20" {
			t.Fatalf("relist revision rv = %q, want the fresh list version 20", rev.rv)
		}
		if got := hubRowNames(rev.table); len(got) != 2 {
			t.Fatalf("relist revision rows = %v, want the fresh two-row list", got)
		}
		select {
		case <-sub.Done():
			t.Fatal("a 410 closed a subscription instead of relisting")
		default:
		}
	}
	waitFor(t, "the watch to resume from the relist", func() bool {
		_, watches, _ := up.counts()
		return watches == 2
	})
	if lists, _, _ := up.counts(); lists != 2 {
		t.Fatalf("LIST count = %d, want exactly one relist on top of the initial list", lists)
	}
	if attempts := up.watchAttempts(); attempts[1] != "20" {
		t.Fatalf("re-watch resumed from %q, want the relisted version 20", attempts[1])
	}
}

// A relist that fails has no recovery left: it is the "watch-failed" terminal,
// delivered once to every subscriber.
func TestWatchHubFailedRelistIsTerminal(t *testing.T) {
	hub, _, _ := newTestWatchHub(t, testHubLimits())
	up := newHubTestUpstream(hubTestTable("10", "alpha"))
	up.openGate()
	key := hubTestKey("ns")

	sub, _, err := hub.Subscribe(context.Background(), up.spec(key), hubDemand{})
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}
	waitFor(t, "the source watch to open", func() bool {
		_, watches, _ := up.counts()
		return watches == 1
	})

	up.setListErr(errors.New("relist refused"))
	up.failWatch(t, fmt.Errorf("%w: too old", kube.ErrWatchGone))

	requireSignal(t, sub.Done(), "subscription closed by the failed relist")
	if got := sub.Reason(); got != hubTerminalWatchFailed {
		t.Fatalf("terminal reason = %q, want %q", got, hubTerminalWatchFailed)
	}
	waitFor(t, "the source to be discarded", func() bool { return hub.sourceCount() == 0 })
}

// An immediate-EOF storm ends the source once: every subscriber is closed with
// the same reason, and the reconnect burst that follows builds exactly ONE
// replacement source (one LIST), not one per reconnecting browser.
func TestWatchHubEOFStormClosesSubscribersOnce(t *testing.T) {
	const subscribers = 3
	const reconnects = 20
	// A generous immediate-EOF window keeps the storm classification
	// independent of how far the test has to wind the clock to release each
	// scheduled re-watch.
	tuning := defaultStreamTuning()
	tuning.immediateWindow = time.Hour
	hub, clock, _ := newTestWatchHubTuned(t, testHubLimits(), &tuning)
	up := newHubTestUpstream(hubTestTable("10", "alpha"))
	up.openGate()
	key := hubTestKey("ns")

	subs := make([]*hubSubscription, 0, subscribers)
	for range subscribers {
		sub, _, err := hub.Subscribe(context.Background(), up.spec(key), hubDemand{})
		if err != nil {
			t.Fatalf("Subscribe failed: %v", err)
		}
		subs = append(subs, sub)
	}
	for attempt := 1; attempt <= streamMaxImmediateEOFs; attempt++ {
		waitFor(t, fmt.Sprintf("watch attempt %d", attempt), func() bool {
			_, watches, _ := up.counts()
			return watches == attempt
		})
		up.endWatch(t)
		if attempt == streamMaxImmediateEOFs {
			break
		}
		advanceUntil(t, clock, time.Minute, "the next re-watch attempt", func() bool {
			_, watches, _ := up.counts()
			return watches == attempt+1
		})
	}

	for _, sub := range subs {
		requireSignal(t, sub.Done(), "subscription closed by the EOF storm")
		if got := sub.Reason(); got != hubTerminalWatchFailed {
			t.Fatalf("terminal reason = %q, want %q", got, hubTerminalWatchFailed)
		}
	}
	waitFor(t, "the source to be discarded", func() bool { return hub.sourceCount() == 0 })

	results := make(chan error, reconnects)
	for range reconnects {
		go func() {
			sub, _, err := hub.Subscribe(context.Background(), up.spec(key), hubDemand{})
			if sub != nil {
				t.Cleanup(sub.Close)
			}
			results <- err
		}()
	}
	for range reconnects {
		if err := <-results; err != nil {
			t.Fatalf("reconnect Subscribe failed: %v", err)
		}
	}
	if lists, _, _ := up.counts(); lists != 2 {
		t.Fatalf("LIST count = %d, want 2: the reconnect burst must build one replacement source", lists)
	}
	if got := hub.sourceCount(); got != 1 {
		t.Fatalf("source count = %d, want 1", got)
	}
}

// An upstream 401/403 is the one outcome no retry can fix: the source publishes
// the "auth" terminal instead of scheduling another attempt.
func TestWatchHubForbiddenWatchIsAuthTerminal(t *testing.T) {
	hub, _, _ := newTestWatchHub(t, testHubLimits())
	up := newHubTestUpstream(hubTestTable("10", "alpha"))
	up.openGate()
	key := hubTestKey("ns")

	sub, _, err := hub.Subscribe(context.Background(), up.spec(key), hubDemand{})
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}
	waitFor(t, "the source watch to open", func() bool {
		_, watches, _ := up.counts()
		return watches == 1
	})

	up.failWatch(t, apiStatus(403, metav1.StatusReasonForbidden, "forbidden"))
	requireSignal(t, sub.Done(), "subscription closed by the forbidden watch")
	if got := sub.Reason(); got != hubTerminalAuth {
		t.Fatalf("terminal reason = %q, want %q", got, hubTerminalAuth)
	}
	waitFor(t, "the source to be discarded", func() bool { return hub.sourceCount() == 0 })
	if _, watches, _ := up.counts(); watches != 1 {
		t.Fatalf("watch attempts = %d, want 1: a 403 must not be retried", watches)
	}
}

// A watch that never opens is an ordinary transient failure: it re-watches on
// the backoff schedule until the storm guard turns it into "watch-failed".
func TestWatchHubWatchOpenFailureRetriesThenFails(t *testing.T) {
	tuning := defaultStreamTuning()
	tuning.immediateWindow = time.Hour
	hub, clock, _ := newTestWatchHubTuned(t, testHubLimits(), &tuning)
	up := newHubTestUpstream(hubTestTable("10", "alpha"))
	up.watchErr = errors.New("watch refused")
	up.openGate()

	sub, _, err := hub.Subscribe(context.Background(), up.spec(hubTestKey("ns")), hubDemand{})
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}
	advanceUntil(t, clock, time.Minute, "the source to exhaust its re-watch attempts", func() bool {
		select {
		case <-sub.Done():
			return true
		default:
			return false
		}
	})
	if got := sub.Reason(); got != hubTerminalWatchFailed {
		t.Fatalf("terminal reason = %q, want %q", got, hubTerminalWatchFailed)
	}
	if _, watches, _ := up.counts(); watches != streamMaxImmediateEOFs {
		t.Fatalf("watch attempts = %d, want %d", watches, streamMaxImmediateEOFs)
	}
	waitFor(t, "the source to be discarded", func() bool { return hub.sourceCount() == 0 })
}

// The join polls belong to the SOURCE, not to its subscribers: no demand means
// no upstream join request at all, and any amount of demand costs one request
// per interval.
func TestWatchHubOverlayPollIsSharedAndDemandDriven(t *testing.T) {
	const demanding = 10
	hub, clock, _ := newTestWatchHub(t, testHubLimits())
	up := newHubTestUpstream(hubTestTable("10", "alpha"))
	up.openGate()
	key := hubTestKey("ns")

	quiet, _, err := hub.Subscribe(context.Background(), up.spec(key), hubDemand{})
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}
	waitFor(t, "the source watch to open", func() bool {
		_, watches, _ := up.counts()
		return watches == 1
	})
	clock.Advance(5 * defaultStreamTuning().metricsPoll)
	if got := up.overlayCount(); got != 0 {
		t.Fatalf("join polls with no demand = %d, want 0", got)
	}

	subs := make([]*hubSubscription, 0, demanding)
	for range demanding {
		sub, _, err := hub.Subscribe(context.Background(), up.spec(key), hubDemand{metrics: true})
		if err != nil {
			t.Fatalf("Subscribe with demand failed: %v", err)
		}
		subs = append(subs, sub)
	}
	waitFor(t, "the first shared join poll", func() bool { return up.overlayCount() == 1 })
	stats, ok := hub.sourceStats(key)
	if got := stats.demandMetrics; !ok || got != demanding {
		t.Fatalf("metrics demand = %d, want %d", got, demanding)
	}
	rev := waitRevision(t, subs[0], 2)
	if rev.overlays.metrics == nil {
		t.Fatal("the join poll did not publish a revision carrying the overlay")
	}

	for interval := 2; interval <= 3; interval++ {
		advanceUntil(t, clock, defaultStreamTuning().metricsPoll, fmt.Sprintf("join poll %d", interval), func() bool {
			return up.overlayCount() == interval
		})
	}
	for _, demand := range up.overlayDemand {
		if !demand.metrics || demand.nodes {
			t.Fatalf("join poll demand = %+v, want metrics only", demand)
		}
	}

	for _, sub := range subs {
		sub.Close()
	}
	waitFor(t, "the demand to drop back to zero", func() bool {
		stats, ok := hub.sourceStats(key)
		return ok && stats.demandMetrics == 0
	})
	settled := up.overlayCount()
	clock.Advance(5 * defaultStreamTuning().metricsPoll)
	if got := up.overlayCount(); got != settled {
		t.Fatalf("join polls after the demand left = %d, want %d", got, settled)
	}
	quiet.Close()
}

// Accounting tracks CURRENT retained state, never the history of events that
// produced it: churn is neutral, a delete shrinks the total, and a relist
// replaces it outright.
func TestWatchHubAccountsRetainedBytes(t *testing.T) {
	hub, clock, metrics := newTestWatchHub(t, testHubLimits())
	up := newHubTestUpstream(hubTestTable("10", "alpha", "beta"))
	up.openGate()
	key := hubTestKey("ns")

	sub, _, err := hub.Subscribe(context.Background(), up.spec(key), hubDemand{})
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}
	waitFor(t, "the source watch to open", func() bool {
		_, watches, _ := up.counts()
		return watches == 1
	})

	base := hub.accountedBytes()
	if want := hubTableBytes(hubTestTable("10", "alpha", "beta")); base != want {
		t.Fatalf("accounted bytes after the initial list = %d, want %d", base, want)
	}

	up.emit(t, hubTestEvent(kube.WatchModified, "11", "alpha"))
	waitRevision(t, sub, 2)
	if got := hub.accountedBytes(); got != base {
		t.Fatalf("accounted bytes after replace churn = %d, want the unchanged %d", got, base)
	}

	up.emit(t, hubTestEvent(kube.WatchDeleted, "12", "beta"))
	waitRevision(t, sub, 3)
	afterDelete := hub.accountedBytes()
	if afterDelete >= base {
		t.Fatalf("accounted bytes after a delete = %d, want less than %d", afterDelete, base)
	}
	if want := hubTableBytes(hubTestTable("10", "alpha")); afterDelete != want {
		t.Fatalf("accounted bytes after a delete = %d, want %d", afterDelete, want)
	}

	up.setTable(hubTestTable("20", "gamma"))
	up.failWatch(t, fmt.Errorf("%w: too old", kube.ErrWatchGone))
	waitRevision(t, sub, 4)
	relisted := hub.accountedBytes()
	if want := hubTableBytes(hubTestTable("20", "gamma")); relisted != want {
		t.Fatalf("accounted bytes after a relist = %d, want the replaced total %d", relisted, want)
	}
	if got := metrics.cacheBytes(); got != relisted {
		t.Fatalf("reported cache bytes = %d, want %d", got, relisted)
	}
	if got, want := hub.cacheChargedBytes(), relisted*cacheAccountingHeadroom; got != want {
		t.Fatalf("charged bytes = %d, want the accounted total with headroom %d", got, want)
	}
	stats, ok := hub.sourceStats(key)
	if got := stats.accounted; !ok || got != relisted {
		t.Fatalf("source accounted bytes = %d, want %d", got, relisted)
	}

	sub.Close()
	advanceUntil(t, clock, hubSourceRetention, "the released source", func() bool {
		return hub.sourceCount() == 0
	})
	waitFor(t, "the accounted total to return to zero", func() bool { return hub.accountedBytes() == 0 })
}

// Only the source sees the raw event rate, so it is the source that marks a
// revision as high-churn for the subscribers' push pacing.
func TestWatchHubRevisionReportsChurn(t *testing.T) {
	hub, _, _ := newTestWatchHub(t, testHubLimits())
	up := newHubTestUpstream(hubTestTable("10", "alpha"))
	up.openGate()

	sub, rev, err := hub.Subscribe(context.Background(), up.spec(hubTestKey("ns")), hubDemand{})
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}
	t.Cleanup(sub.Close)
	if rev.highChurn {
		t.Fatal("the initial revision reported high churn")
	}
	waitFor(t, "the source watch to open", func() bool {
		_, watches, _ := up.counts()
		return watches == 1
	})
	for i := range streamChurnEvents {
		up.emit(t, hubTestEvent(kube.WatchModified, fmt.Sprintf("%d", 11+i), "alpha"))
	}
	churned := waitRevision(t, sub, uint64(1+streamChurnEvents))
	if !churned.highChurn {
		t.Fatalf("revision %d did not report the sustained event rate", churned.num)
	}
}

// Hub shutdown during initialization fails the waiters instead of leaving them
// blocked on a snapshot that will never arrive.
func TestWatchHubShutdownDuringInitializationFailsWaiters(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	hub := newWatchHub(ctx, testHubLimits(), nil, newFakeHubClock(), newRecordingHubMetrics())
	up := newHubTestUpstream(hubTestTable("10", "alpha"))
	key := hubTestKey("ns")

	done := make(chan error, 1)
	go func() {
		_, _, err := hub.Subscribe(context.Background(), up.spec(key), hubDemand{})
		done <- err
	}()
	waitFor(t, "the waiter to queue", func() bool {
		stats, ok := hub.sourceStats(key)
		return ok && stats.waiters == 1
	})
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter error = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the waiter to be failed by hub shutdown")
	}
	up.openGate()
}

// The hub reports the three things its own state machine is the only witness
// to: connection slots as they are taken and released, one relist per 410, and
// the accounted size of each AUTHORITATIVE snapshot (the initial list and the
// relist -- watch events adjust that size rather than restating it).
func TestWatchHubReportsSlotsAndRecoveryToTheMetricsSink(t *testing.T) {
	hub, _, metrics := newTestWatchHub(t, testHubLimits())
	for i := range 2 {
		if !hub.acquireConnection() {
			t.Fatalf("the empty hub refused connection slot %d", i)
		}
	}
	if got := metrics.connectionCount(); got != 2 {
		t.Fatalf("reported connections = %d, want 2", got)
	}
	hub.releaseConnection()
	if got := metrics.connectionCount(); got != 1 {
		t.Fatalf("reported connections after one release = %d, want 1", got)
	}

	up := newHubTestUpstream(hubTestTable("10", "alpha"))
	up.openGate()
	key := hubTestKey("ns")
	sub, _, err := hub.Subscribe(context.Background(), up.spec(key), hubDemand{})
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}
	t.Cleanup(sub.Close)
	waitFor(t, "the source watch to open", func() bool {
		_, watches, _ := up.counts()
		return watches == 1
	})
	if got := metrics.relistCount(); got != 0 {
		t.Fatalf("reported relists on a healthy source = %d, want 0", got)
	}

	// An ordinary event changes the retained size without being a snapshot.
	up.emit(t, hubTestEvent(kube.WatchAdded, "11", "beta"))
	waitRevision(t, sub, 2)
	if got := metrics.snapshotSamples(); len(got) != 1 {
		t.Fatalf("snapshot samples after a watch event = %v, want only the initial list", got)
	}

	up.setTable(hubTestTable("20", "gamma"))
	up.failWatch(t, fmt.Errorf("%w: too old", kube.ErrWatchGone))
	waitRevision(t, sub, 3)
	if got := metrics.relistCount(); got != 1 {
		t.Fatalf("reported relists after one 410 = %d, want 1", got)
	}
	samples := metrics.snapshotSamples()
	want := []int64{hubTableBytes(hubTestTable("10", "alpha")), hubTableBytes(hubTestTable("20", "gamma"))}
	if len(samples) != len(want) || samples[0] != want[0] || samples[1] != want[1] {
		t.Fatalf("snapshot samples = %v, want the initial list and the relist %v", samples, want)
	}

	hub.releaseConnection()
	if got := metrics.connectionCount(); got != 0 {
		t.Fatalf("reported connections after every release = %d, want 0", got)
	}
}

// A source already polling one join kind must re-poll the moment a subscriber
// needs the OTHER one. Overlay demand is per kind, and a single on/off flag
// would leave the second subscriber's handshake blocked on a join the running
// poll never asked for -- for the whole interval, holding a connection slot.
func TestWatchHubOverlayDemandForASecondJoinPollsImmediately(t *testing.T) {
	hub, clock, _ := newTestWatchHub(t, testHubLimits())
	up := newHubTestUpstream(hubTestTable("10", "alpha"))
	up.openGate()
	key := hubTestKey("ns")

	metricsSub, _, err := hub.Subscribe(context.Background(), up.spec(key), hubDemand{metrics: true})
	if err != nil {
		t.Fatalf("Subscribe with metrics demand failed: %v", err)
	}
	defer metricsSub.Close()
	waitFor(t, "the first shared join poll", func() bool { return up.overlayCount() == 1 })

	nodesSub, _, err := hub.Subscribe(context.Background(), up.spec(key), hubDemand{nodes: true})
	if err != nil {
		t.Fatalf("Subscribe with nodes demand failed: %v", err)
	}
	defer nodesSub.Close()

	// No clock advance: the re-poll must happen on demand, not on the interval.
	waitFor(t, "the join re-poll for the newly demanded kind", func() bool {
		return up.overlayCount() == 2
	})
	waitFor(t, "a revision carrying the nodes overlay", func() bool {
		rev := nodesSub.Revision()
		return rev != nil && rev.overlays.nodes != nil && rev.overlays.metrics != nil
	})
	stats, _ := hub.sourceStats(key)
	if stats.demandMetrics != 1 || stats.demandNodes != 1 {
		t.Fatalf("demand = %+v, want one of each kind", stats)
	}

	// One shared poll still serves both kinds from here on.
	before := up.overlayCount()
	advanceUntil(t, clock, defaultStreamTuning().metricsPoll, "the next interval poll", func() bool {
		return up.overlayCount() == before+1
	})
	last := up.overlayDemand[len(up.overlayDemand)-1]
	if !last.metrics || !last.nodes {
		t.Fatalf("interval poll demand = %+v, want both kinds", last)
	}
}

// Retention is an optimization for the next visitor, not a reservation. A pod
// must not answer "too many live sources" while sources nobody is attached to
// sit out their 30s window holding the slots.
func TestWatchHubSourceLimitReclaimsAnIdleSource(t *testing.T) {
	limits := testHubLimits()
	limits.maxSources = 1
	hub, _, _ := newTestWatchHub(t, limits)
	first := newHubTestUpstream(hubTestTable("10", "alpha"))
	first.openGate()
	firstKey := hubTestKey("first")

	sub, _, err := hub.Subscribe(context.Background(), first.spec(firstKey), hubDemand{})
	if err != nil {
		t.Fatalf("first Subscribe failed: %v", err)
	}

	second := newHubTestUpstream(hubTestTable("20", "beta"))
	second.openGate()
	secondKey := hubTestKey("second")
	if _, _, err := hub.Subscribe(context.Background(), second.spec(secondKey), hubDemand{}); !errors.Is(err, errHubSourceLimit) {
		t.Fatalf("Subscribe past the source limit err = %v, want errHubSourceLimit", err)
	}

	// The first viewer leaves. Its source is retained, not released -- and that
	// retention must not cost the next viewer a 429.
	sub.Close()
	waitFor(t, "the first source to go idle", func() bool {
		stats, ok := hub.sourceStats(firstKey)
		return ok && stats.subscribers == 0
	})

	replacement, rev, err := hub.Subscribe(context.Background(), second.spec(secondKey), hubDemand{})
	if err != nil {
		t.Fatalf("Subscribe after the idle source was reclaimed failed: %v", err)
	}
	defer replacement.Close()
	if rev == nil || rev.table.ResourceVersion != "20" {
		t.Fatalf("revision = %+v, want the second scope's own snapshot", rev)
	}
	waitFor(t, "the reclaimed source to leave the hub", func() bool { return hub.sourceCount() == 1 })
	if lists, _, _ := second.counts(); lists != 1 {
		t.Fatalf("second scope LISTs = %d, want exactly one", lists)
	}
}

// The same reclamation applies to the retained-bytes bound: holding a cache for
// a viewer who left is worth less than admitting the one who is here.
func TestWatchHubCacheLimitReclaimsAnIdleSource(t *testing.T) {
	limits := testHubLimits()
	limits.maxCacheAccountedBytes = hubTableBytes(hubTestTable("10", "alpha")) * cacheAccountingHeadroom * 3 / 2
	hub, _, _ := newTestWatchHub(t, limits)
	first := newHubTestUpstream(hubTestTable("10", "alpha"))
	first.openGate()
	firstKey := hubTestKey("first")

	sub, _, err := hub.Subscribe(context.Background(), first.spec(firstKey), hubDemand{})
	if err != nil {
		t.Fatalf("first Subscribe failed: %v", err)
	}

	second := newHubTestUpstream(hubTestTable("20", "beta"))
	second.openGate()
	secondKey := hubTestKey("second")
	if _, _, err := hub.Subscribe(context.Background(), second.spec(secondKey), hubDemand{}); err == nil {
		t.Fatal("Subscribe past the cache bound succeeded, want a cache-limit failure")
	}

	sub.Close()
	waitFor(t, "the first source to go idle", func() bool {
		stats, ok := hub.sourceStats(firstKey)
		return ok && stats.subscribers == 0
	})

	third := newHubTestUpstream(hubTestTable("30", "gamma"))
	third.openGate()
	replacement, _, err := hub.Subscribe(context.Background(), third.spec(hubTestKey("third")), hubDemand{})
	if err != nil {
		t.Fatalf("Subscribe after the idle cache was reclaimed failed: %v", err)
	}
	replacement.Close()
}

// A scope whose own retained state does not fit is refused for a window WITHOUT
// re-listing it. The bound is only knowable after a full LIST and the refused
// source is dropped from the map, so without the memory every retry of the
// `Retry-After: 10` a refused browser honours would pay another full Table LIST
// of the pod's single largest scope -- an unbounded LIST loop, running at the
// exact moment the pod's retained state is at its ceiling.
func TestWatchHubCacheLimitRefusalDoesNotRelist(t *testing.T) {
	limits := testHubLimits()
	// Half of what this one scope charges: nothing can make it fit.
	limits.maxCacheAccountedBytes = hubTableBytes(hubTestTable("10", "alpha")) * cacheAccountingHeadroom / 2
	hub, clock, _ := newTestWatchHub(t, limits)
	upstream := newHubTestUpstream(hubTestTable("10", "alpha"))
	upstream.openGate()
	key := hubTestKey("oversized")

	refuse := func(attempt int) {
		t.Helper()
		if _, _, err := hub.Subscribe(context.Background(), upstream.spec(key), hubDemand{}); !errors.Is(err, errHubCacheLimit) {
			t.Fatalf("Subscribe attempt %d = %v, want errHubCacheLimit", attempt, err)
		}
		// The failed source leaves the hub asynchronously; waiting for that is
		// what makes the NEXT attempt go through source() rather than being
		// answered by the still-registered failed source.
		waitFor(t, "the refused source to leave the hub", func() bool { return hub.sourceCount() == 0 })
	}

	for attempt := 1; attempt <= 3; attempt++ {
		refuse(attempt)
	}
	if lists, _, _ := upstream.counts(); lists != 1 {
		t.Fatalf("LISTs across three refused attempts = %d, want exactly one", lists)
	}

	// The memory expires on its own: a scope that shrank, or a pod that has
	// since lost another source, is measured again rather than blacklisted.
	clock.Advance(hubCacheRefusalTTL)
	refuse(4)
	if lists, _, _ := upstream.counts(); lists != 2 {
		t.Fatalf("LISTs after the refusal window expired = %d, want a second measurement", lists)
	}
}

// A reclaim that actually returns bytes retires every refusal: those verdicts
// were measured against a budget that no longer holds, so the next subscriber
// must be re-measured immediately rather than waiting out the window.
func TestWatchHubCacheRefusalClearsOnReclaim(t *testing.T) {
	limits := testHubLimits()
	limits.maxCacheAccountedBytes = hubTableBytes(hubTestTable("10", "alpha")) * cacheAccountingHeadroom * 3 / 2
	hub, _, _ := newTestWatchHub(t, limits)
	first := newHubTestUpstream(hubTestTable("10", "alpha"))
	first.openGate()
	firstKey := hubTestKey("resident")
	sub, _, err := hub.Subscribe(context.Background(), first.spec(firstKey), hubDemand{})
	if err != nil {
		t.Fatalf("first Subscribe failed: %v", err)
	}

	second := newHubTestUpstream(hubTestTable("20", "beta"))
	second.openGate()
	secondKey := hubTestKey("refused")
	if _, _, err := hub.Subscribe(context.Background(), second.spec(secondKey), hubDemand{}); !errors.Is(err, errHubCacheLimit) {
		t.Fatalf("Subscribe past the cache bound = %v, want errHubCacheLimit", err)
	}
	waitFor(t, "the refused source to leave the hub", func() bool { return hub.sourceCount() == 1 })

	// The resident source goes idle, so the second scope's bytes now fit.
	sub.Close()
	waitFor(t, "the resident source to go idle", func() bool {
		stats, ok := hub.sourceStats(firstKey)
		return ok && stats.subscribers == 0
	})

	admitted, _, err := hub.Subscribe(context.Background(), second.spec(secondKey), hubDemand{})
	if err != nil {
		t.Fatalf("Subscribe after the idle source was reclaimed = %v, want success", err)
	}
	admitted.Close()
}

// The accounted-bytes estimate is what live.maxCacheAccountedBytes is measured
// in, so it has to track a real encode -- not just agree with itself. Every
// other accounting assertion computes its expectation with the function under
// test, which would survive gutting the recursion entirely.
func TestHubTableBytesTracksTheEncodedSize(t *testing.T) {
	tests := []struct {
		name  string
		table *kube.Table
	}{
		{"flat rows", hubTestTable("10", "alpha", "beta", "gamma")},
		{"rich objects", hubRichContractTable()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// The retained state that SCALES is the rows; the Table's own
			// fixed struct fields are not what a memory bound is about.
			encoded, err := json.Marshal(tc.table.Rows)
			if err != nil {
				t.Fatalf("marshal rows: %v", err)
			}
			estimate := hubTableBytes(tc.table) - hubTableMetaBytes(tc.table)
			actual := int64(len(encoded))
			// A deliberate approximation, but one that must stay the same
			// ORDER OF MAGNITUDE as the bytes it stands in for.
			if estimate < actual/2 || estimate > actual*2 {
				t.Fatalf("row estimate = %d, encoded JSON = %d: not within 2x", estimate, actual)
			}
		})
	}
}

// Each hubValueBytes arm pinned against a real encode of the same value, so a
// dropped recursion (the map/slice arms are the ones that carry a Kubernetes
// object) cannot pass by agreeing with itself.
func TestHubValueBytesPerJSONShape(t *testing.T) {
	values := []any{
		nil,
		"",
		"readout",
		true,
		false,
		float64(0),
		float64(4711),
		[]any{},
		[]any{"a", "b", "c"},
		map[string]any{},
		map[string]any{"app": "readout"},
		map[string]any{"metadata": map[string]any{"name": "api", "labels": map[string]any{"tier": "web"}}},
		hubRichContractTable().Rows[0].Object,
	}
	for _, value := range values {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal %#v: %v", value, err)
		}
		estimate := hubValueBytes(value)
		actual := int64(len(encoded))
		if estimate < actual/2 || estimate > actual*2+8 {
			t.Fatalf("hubValueBytes(%#v) = %d, encoded JSON = %d: not within 2x", value, estimate, actual)
		}
	}
}

// hubRichContractTable exercises every hubValueBytes arm: nested maps, arrays,
// numbers, bools and nils inside a realistic object.
func hubRichContractTable() *kube.Table {
	return &kube.Table{
		ResourceVersion: "4711",
		Columns: []kube.Column{
			{Name: "Name", Type: "string", Format: "name", Description: "the object name"},
			{Name: "Ready", Type: "string"},
		},
		Clusters: []string{"prod", "staging"},
		Rows: []kube.Row{{
			Cluster: "prod",
			Cells:   []any{"api-7c8d9", "2/3", 17, true, nil},
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "Pod",
				"metadata": map[string]any{
					"name":      "api-7c8d9",
					"namespace": "default",
					"labels":    map[string]any{"app": "readout", "tier": "web"},
					"ownerReferences": []any{
						map[string]any{"kind": "ReplicaSet", "name": "api", "controller": true},
					},
				},
				"spec": map[string]any{
					"nodeName": "node-a",
					"containers": []any{
						map[string]any{"name": "api", "image": "readout:1.2.3", "ports": []any{8080, 9090}},
					},
				},
				"status": map[string]any{
					"phase":             "Running",
					"hostIP":            "10.0.0.4",
					"containerStatuses": []any{map[string]any{"ready": true, "restartCount": 2}},
					"terminationTime":   nil,
				},
			},
		}},
	}
}

// A browser can disconnect in the window between the actor granting a
// subscription and the attaching goroutine reading its reply. The withdraw has
// to release the granted subscription, not just dequeue a waiter: otherwise the
// source never goes idle and its watch, retained bytes and join poll leak for
// the pod's lifetime.
func TestWatchHubWithdrawReleasesAnAlreadyGrantedSubscription(t *testing.T) {
	hub, _, _ := newTestWatchHub(t, testHubLimits())
	up := newHubTestUpstream(hubTestTable("10", "alpha"))
	up.openGate()
	key := hubTestKey("ns")

	sub, _, err := hub.Subscribe(context.Background(), up.spec(key), hubDemand{})
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}
	hub.mu.Lock()
	src := hub.sources[*key]
	hub.mu.Unlock()
	if src == nil {
		t.Fatal("no source for the subscribed key")
	}

	// A waiter that is granted a subscription and then withdrawn, exactly as a
	// cancelled attach does through the actor.
	waiter := &hubWaiter{reply: make(chan hubAttachResult, 1), demand: hubDemand{metrics: true}}
	if !src.post(func(s *hubSource) { s.serveWaiter(waiter) }) {
		t.Fatal("serveWaiter was not accepted")
	}
	waitFor(t, "the waiter to be granted a subscription", func() bool {
		stats, ok := hub.sourceStats(key)
		return ok && stats.subscribers == 2 && stats.demandMetrics == 1
	})
	if !src.post(func(s *hubSource) { s.withdrawWaiter(waiter) }) {
		t.Fatal("withdrawWaiter was not accepted")
	}

	waitFor(t, "the granted subscription to be released", func() bool {
		stats, ok := hub.sourceStats(key)
		return ok && stats.subscribers == 1 && stats.demandMetrics == 0
	})

	// The surviving subscriber is untouched, and the source still publishes.
	sub.Close()
	waitFor(t, "the source to go idle once the real subscriber leaves", func() bool {
		stats, ok := hub.sourceStats(key)
		return ok && stats.subscribers == 0
	})
}
