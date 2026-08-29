package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kbelokon/readout/internal/kube"
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
	return &hubSourceSpec{key: *key, list: u.list, watch: u.watch}
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
}

func (w *hubTestWatch) Next() (kube.WatchEvent, error) {
	select {
	case ev := <-w.events:
		return ev, nil
	case <-w.closed:
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

// newTestWatchHub builds a hub with the manual clock and a recording sink. The
// hub context is canceled by cleanup so no source outlives its test.
func newTestWatchHub(t *testing.T, limits liveLimits) (*watchHub, *fakeHubClock, *recordingHubMetrics) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	clock := newFakeHubClock()
	metrics := newRecordingHubMetrics()
	return newWatchHub(ctx, limits, clock, metrics), clock, metrics
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
			sub, rev, err := hub.Subscribe(context.Background(), up.spec(key))
			results <- attach{sub: sub, rev: rev, err: err}
		}()
	}
	waitFor(t, "all subscribers to queue on the initializing source", func() bool {
		return hub.sourceStats(key).waiters == subscribers
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
	if got := hub.sourceStats(key).subscribers; got != subscribers {
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

	sub, rev, err := hub.Subscribe(context.Background(), up.spec(hubTestKey("ns")))
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

		primary, _, err := hub.Subscribe(context.Background(), up.spec(key))
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
			racer, raceRev, raceErr = hub.Subscribe(context.Background(), up.spec(key))
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
		hub.sourceStats(key)

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

	sub, _, err := hub.Subscribe(context.Background(), up.spec(hubTestKey("ns")))
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

	sub, rev, err := hub.Subscribe(context.Background(), up.spec(key))
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

	sub, rev, err := hub.Subscribe(context.Background(), up.spec(hubTestKey("ns")))
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

	sub, _, err := hub.Subscribe(context.Background(), up.spec(key))
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
		return hub.sourceStats(key).subscribers == 0
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

	first, _, err := hub.Subscribe(context.Background(), up.spec(key))
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}
	waitFor(t, "the source watch to open", func() bool {
		_, watches, _ := up.counts()
		return watches == 1
	})
	first.Close()
	waitFor(t, "the source to drop its subscriber", func() bool {
		return hub.sourceStats(key).subscribers == 0
	})

	second, rev, err := hub.Subscribe(context.Background(), up.spec(key))
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
		return hub.sourceStats(key).subscribers == 1
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
	const waiters = 10
	hub, _, metrics := newTestWatchHub(t, testHubLimits())
	up := newHubTestUpstream(&kube.Table{})
	up.listErr = errors.New("boom")
	key := hubTestKey("ns")

	errs := make(chan error, waiters)
	for range waiters {
		go func() {
			_, _, err := hub.Subscribe(context.Background(), up.spec(key))
			errs <- err
		}()
	}
	waitFor(t, "all waiters to queue on the initializing source", func() bool {
		return hub.sourceStats(key).waiters == waiters
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
		_, _, err := hub.Subscribe(ctx, up.spec(key))
		firstErr <- err
	}()
	waitFor(t, "the first subscriber to queue", func() bool {
		return hub.sourceStats(key).waiters == 1
	})
	secondErr := make(chan error, 1)
	var second *hubSubscription
	go func() {
		sub, _, err := hub.Subscribe(context.Background(), up.spec(key))
		second = sub
		secondErr <- err
	}()
	waitFor(t, "the second subscriber to queue", func() bool {
		return hub.sourceStats(key).waiters == 2
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
		return hub.sourceStats(key).subscribers == 1
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
		_, _, err := hub.Subscribe(ctx, up.spec(key))
		done <- err
	}()
	waitFor(t, "the subscriber to queue", func() bool {
		return hub.sourceStats(key).waiters == 1
	})
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Subscribe error = %v, want context.Canceled", err)
	}
	up.openGate()
	waitFor(t, "the abandoned source to settle", func() bool {
		stats := hub.sourceStats(key)
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

	sub, _, err := hub.Subscribe(context.Background(), up.spec(hubTestKey("ns")))
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}
	t.Cleanup(sub.Close)

	if _, _, err := hub.Subscribe(context.Background(), up.spec(hubTestKey("other"))); !errors.Is(err, errHubSourceLimit) {
		t.Fatalf("second key error = %v, want errHubSourceLimit", err)
	}
	// The existing key still joins its running source.
	reused, _, err := hub.Subscribe(context.Background(), up.spec(hubTestKey("ns")))
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
	hub := newWatchHub(ctx, testHubLimits(), newFakeHubClock(), newRecordingHubMetrics())
	up := newHubTestUpstream(hubTestTable("10", "alpha"))
	up.openGate()

	sub, _, err := hub.Subscribe(context.Background(), up.spec(hubTestKey("ns")))
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}
	waitFor(t, "the source watch to open", func() bool {
		_, watches, _ := up.counts()
		return watches == 1
	})

	cancel()
	requireSignal(t, sub.Done(), "subscription closed by hub shutdown")
	waitFor(t, "sources to be released", func() bool { return hub.sourceCount() == 0 })
	waitFor(t, "the upstream watch to close", func() bool {
		_, _, closes := up.counts()
		return closes == 1
	})
	if _, _, err := hub.Subscribe(context.Background(), up.spec(hubTestKey("ns"))); err == nil {
		t.Fatal("Subscribe succeeded after hub shutdown")
	}
}

// A watch that ends releases every subscriber and discards the source, so the
// next attach starts a fresh attempt rather than joining dead state.
func TestWatchHubWatchEndReleasesSubscribers(t *testing.T) {
	hub, _, _ := newTestWatchHub(t, testHubLimits())
	up := newHubTestUpstream(hubTestTable("10", "alpha"))
	up.openGate()
	key := hubTestKey("ns")

	sub, _, err := hub.Subscribe(context.Background(), up.spec(key))
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}
	waitFor(t, "the source watch to open", func() bool {
		_, watches, _ := up.counts()
		return watches == 1
	})

	up.endWatch(t)
	requireSignal(t, sub.Done(), "subscription closed by the ended watch")
	waitFor(t, "the source to be discarded", func() bool { return hub.sourceCount() == 0 })

	replacement, _, err := hub.Subscribe(context.Background(), up.spec(key))
	if err != nil {
		t.Fatalf("Subscribe after the ended watch failed: %v", err)
	}
	t.Cleanup(replacement.Close)
	waitFor(t, "the replacement source to list", func() bool {
		lists, _, _ := up.counts()
		return lists == 2
	})
}

// A watch that never opens is the same terminal outcome: the source fails
// instead of retaining state nothing will ever update.
func TestWatchHubWatchOpenFailureFailsSource(t *testing.T) {
	hub, _, _ := newTestWatchHub(t, testHubLimits())
	up := newHubTestUpstream(hubTestTable("10", "alpha"))
	up.watchErr = errors.New("watch refused")
	up.openGate()

	sub, _, err := hub.Subscribe(context.Background(), up.spec(hubTestKey("ns")))
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}
	requireSignal(t, sub.Done(), "subscription closed by the failed watch open")
	waitFor(t, "the source to be discarded", func() bool { return hub.sourceCount() == 0 })
}

// Hub shutdown during initialization fails the waiters instead of leaving them
// blocked on a snapshot that will never arrive.
func TestWatchHubShutdownDuringInitializationFailsWaiters(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	hub := newWatchHub(ctx, testHubLimits(), newFakeHubClock(), newRecordingHubMetrics())
	up := newHubTestUpstream(hubTestTable("10", "alpha"))
	key := hubTestKey("ns")

	done := make(chan error, 1)
	go func() {
		_, _, err := hub.Subscribe(context.Background(), up.spec(key))
		done <- err
	}()
	waitFor(t, "the waiter to queue", func() bool {
		return hub.sourceStats(key).waiters == 1
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
