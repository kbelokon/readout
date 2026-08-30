//go:build unix

package web

// watchhub_load_test.go is the in-repo capacity run for the WatchHub: three
// operating points driven through REAL SSE streams against the fakeapi
// fixture, plus the bound assertions that make the measured numbers mean
// something. It is gated behind RO_LOAD=1 because the 512-subscriber anchor
// costs seconds of CPU and hundreds of megabytes of transient heap -- too much
// to pay on every `go test ./...` -- and because its job is to produce the
// numbers recorded in the README capacity profile and the accounting headroom
// constant, not to guard behavior the ordinary suite already covers.
//
// Per anchor it records process CPU time and peak RSS, live goroutine and open
// file-descriptor counts, the accounted and the actual heap cost of the
// retained source, and the event-to-flush distribution read out of the
// histogram the server itself exports. Goroutines and descriptors count BOTH
// ends of every stream: the test's own readers live in this process too, so
// they are "N streams end to end in one process", not a server-only figure.
//
// The clock here reports real time (an event-to-flush histogram measured
// against a frozen clock would be meaningless) while still letting the run
// fire the hub's 30-second retention timer on demand.

import (
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"text/tabwriter"
	"time"

	"github.com/kbelokon/readout/internal/config"
)

// loadGateEnv is the opt-in: the run is skipped unless it is set to "1".
const loadGateEnv = "RO_LOAD"

// loadFrameTimeout is deliberately generous. At the top anchor five hundred
// handler goroutines render the same 600-row scope at once on whatever core
// count the machine has, and the whole point of the run is to measure that
// cost rather than to fail on it.
const loadFrameTimeout = 90 * time.Second

// loadChurnStatuses are the successive Status cell values one upstream row is
// driven through. Each round is awaited on every subscriber before the next is
// posted, so push coalescing cannot collapse the rounds and the event-to-flush
// histogram carries real fan-out samples and not only the initial snapshots.
var loadChurnStatuses = []string{"Phase1", "Phase2", "Phase3"}

// loadAnchor is one measured operating point.
type loadAnchor struct {
	name        string
	namespace   string
	subscribers int
	// cacheLimit is this anchor's live.maxCacheAccountedBytes. Zero means the
	// shipped default; the ceiling anchor sets it to exactly the charged size
	// of its own scope, which is what makes that scope the largest snapshot
	// the pod would admit.
	cacheLimit int64
}

// loadSample is everything one anchor measured.
type loadSample struct {
	anchor        string
	subscribers   int
	sources       float64
	snapshotBytes float64
	accounted     int64
	heapDelta     int64
	cpuSeconds    float64
	maxRSSBytes   int64
	goroutines    int
	descriptors   int
	flushP50      float64
	flushP99      float64
	flushMean     float64
}

// TestWatchHubLoad is the capacity run. It measures the three anchors, prints
// the profile table, and then asserts the three properties the table is only
// trustworthy alongside: every limit refuses a stream BEFORE the resource it
// bounds is over its configured value, replace/delete churn does not grow the
// accounted total when the retained state does not grow, and every _active
// gauge is back at baseline once the browsers are gone and retention expires
// (that last one is checked at the end of each anchor, against its own hub).
func TestWatchHubLoad(t *testing.T) {
	if os.Getenv(loadGateEnv) != "1" {
		t.Skipf("capacity run is opt-in: %s=1 go test ./internal/web -run TestWatchHubLoad", loadGateEnv)
	}

	retainedAccounted, retainedHeap := measureRetainedSourceHeap(t)
	small := runLoadAnchor(t, loadAnchor{name: "small scope / 1 subscriber", namespace: "default", subscribers: 1})
	big := runLoadAnchor(t, loadAnchor{name: "600-row scope / 100 subscribers", namespace: "big", subscribers: 100})
	// The ceiling anchor runs the same scope under a cache limit set to
	// exactly its own charged size, so it IS the largest snapshot this pod
	// would admit, at the full default connection capacity.
	ceiling := runLoadAnchor(t, loadAnchor{
		name:        "cache-ceiling scope / 512 subscribers",
		namespace:   "big",
		subscribers: config.DefaultLiveMaxConnections,
		cacheLimit:  big.accounted * cacheAccountingHeadroom,
	})

	t.Log("\n" + renderLoadProfile([]loadSample{small, big, ceiling}, retainedAccounted, retainedHeap))

	t.Run("every limit rejects before its bound is crossed", func(t *testing.T) {
		assertLimitsRejectBeforeTheirBound(t)
	})
	t.Run("replace and delete churn do not grow accounted bytes", func(t *testing.T) {
		assertChurnDoesNotGrowAccounting(t)
	})
}

// runLoadAnchor drives one anchor end to end and returns its measurements. It
// also asserts this anchor's own leak contract: once every subscriber is gone
// and the retention window has been released, all four _active gauges read
// zero.
func runLoadAnchor(t *testing.T, anchor loadAnchor) loadSample {
	t.Helper()
	clock := newLoadHubClock()
	app, ts, fake := newLiveMetricsFixture(t,
		&config.Config{LiveMaxCacheAccountedBytes: anchor.cacheLimit},
		func(app *Server) { app.hubClock = clock })
	url := ts.URL + "/clusters/test/namespaces/" + anchor.namespace + "/pods/_stream"

	// A throwaway stream first: discovery, the render caches and the client-go
	// machinery all allocate on their first use, and none of that is the cost
	// of the anchor.
	warm := openStream(t, url, "warm-"+anchor.namespace)
	warm.requireEvent(t, "ro-live", loadFrameTimeout)
	warm.close()
	releaseHub(t, app, clock)

	baseCPU := processCPUSeconds(t)
	baseHeap := heapInUseBytes()

	streams := make([]*sseStream, 0, anchor.subscribers)
	for i := range anchor.subscribers {
		streams = append(streams, openStream(t, url, fmt.Sprintf("load-%s-%d", anchor.namespace, i)))
	}
	for i, stream := range streams {
		if frame := decodeFrame(t, stream.requireEvent(t, "ro-live", loadFrameTimeout)); frame.Kind != "snapshot" {
			t.Fatalf("%s: subscriber %d first frame kind = %q, want snapshot", anchor.name, i, frame.Kind)
		}
	}
	// The retained size is read here, off the untouched initial LIST: the
	// churn rounds below replace one row with a scripted stand-in whose object
	// is nothing like the seeded one, and a scope size that moved with the
	// test's own events would not be the scope size an operator sizes against.
	accounted := app.liveHub().accountedBytes()

	waitForOpenWatch(t, fake.URL)
	for _, status := range loadChurnStatuses {
		postStreamScript(t, fake.URL, `{"events":[`+loadPodEvent(anchor.namespace, "MODIFIED", status)+`]}`)
		for _, stream := range streams {
			awaitFrame(t, stream, status)
		}
	}

	sample := loadSample{
		anchor:      anchor.name,
		subscribers: anchor.subscribers,
		accounted:   accounted,
		cpuSeconds:  processCPUSeconds(t) - baseCPU,
		maxRSSBytes: processMaxRSSBytes(t),
		goroutines:  runtime.NumGoroutine(),
		descriptors: openDescriptorCount(),
	}
	sample.heapDelta = int64(heapInUseBytes()) - int64(baseHeap)
	body := scrapeMetrics(t, app)
	sample.sources, _ = metricValue(t, body, "readout_watchhub_sources_active")
	sample.snapshotBytes = histogramMean(t, body, "readout_watchhub_snapshot_bytes")
	flush := scrapeHistogram(t, body, "readout_watchhub_event_to_flush_seconds")
	sample.flushP50 = flush.quantile(0.5)
	sample.flushP99 = flush.quantile(0.99)
	sample.flushMean = flush.mean()

	if got := int(sample.sources); got != 1 {
		t.Fatalf("%s: sources_active = %d, want the one shared source", anchor.name, got)
	}
	requireMetric(t, body, "readout_watchhub_subscribers_active", float64(anchor.subscribers))
	requireMetric(t, body, "readout_live_connections_active", float64(anchor.subscribers))

	for _, stream := range streams {
		stream.close()
	}
	releaseHub(t, app, clock)
	baseline := scrapeMetrics(t, app)
	for _, series := range []string{
		"readout_live_connections_active",
		"readout_watchhub_sources_active",
		"readout_watchhub_subscribers_active",
		"readout_watchhub_cache_accounted_bytes",
	} {
		requireMetric(t, baseline, series, 0)
	}
	return sample
}

// measureRetainedSourceHeap isolates what ONE retained source actually costs
// the Go heap, which is the ratio cacheAccountingHeadroom has to cover. The
// measurement brackets the source: heap with it retained (all subscribers
// already gone, so no per-session state is counted) minus heap after retention
// dropped it. Everything else in the process is identical across the two
// readings.
func measureRetainedSourceHeap(t *testing.T) (accounted, heap int64) {
	t.Helper()
	clock := newLoadHubClock()
	app, ts, _ := newLiveMetricsFixture(t, nil, func(app *Server) { app.hubClock = clock })
	url := ts.URL + "/clusters/test/namespaces/big/pods/_stream"

	warm := openStream(t, url, "retained-warm")
	warm.requireEvent(t, "ro-live", loadFrameTimeout)
	warm.close()
	releaseHub(t, app, clock)

	s := openStream(t, url, "retained")
	s.requireEvent(t, "ro-live", loadFrameTimeout)
	s.close()
	hub := app.liveHub()
	waitFor(t, "the subscriber to detach while the source is still retained", func() bool {
		return hub.connectionCount() == 0 && hub.sourceCount() == 1
	})
	accounted = hub.accountedBytes()
	withSource := heapInUseBytes()

	releaseHub(t, app, clock)
	heap = int64(withSource) - int64(heapInUseBytes())
	if accounted <= 0 || heap <= 0 {
		t.Fatalf("retained-source measurement is not usable: accounted=%d heap=%d", accounted, heap)
	}
	// The measurement is not just for the printed table. cacheAccountingHeadroom
	// is the ONLY thing that makes live.maxCacheAccountedBytes readable as
	// bytes of process memory, so if the real ratio ever climbs above it the
	// documented bound has silently become an under-estimate and a pod at its
	// configured limit OOMs.
	if heap > accounted*cacheAccountingHeadroom {
		t.Fatalf("one retained source costs %d heap bytes for %d accounted (ratio %.2f), past cacheAccountingHeadroom = %d",
			heap, accounted, float64(heap)/float64(accounted), cacheAccountingHeadroom)
	}
	return accounted, heap
}

// TestCacheAccountingHeadroomCoversOneRetainedSource is the ungated half of the
// same invariant, measured on every CI run so a decoded-row layout change that
// blows past the headroom fails here instead of in a pod. It uses the 600-row
// scope on purpose: the multiplier models RETAINED ROWS, and a source's own
// fixed cost (an actor goroutine, its channels and maps -- tens of KiB) swamps
// a tiny scope's rows no matter what the multiplier is.
func TestCacheAccountingHeadroomCoversOneRetainedSource(t *testing.T) {
	clock := newLoadHubClock()
	app, ts, _ := newLiveMetricsFixture(t, nil, func(app *Server) { app.hubClock = clock })
	url := ts.URL + "/clusters/test/namespaces/big/pods/_stream"

	// Warm first: the second measurement then brackets only the retained source.
	warm := openStream(t, url, "headroom-warm")
	warm.requireEvent(t, "ro-live", loadFrameTimeout)
	warm.close()
	releaseHub(t, app, clock)

	s := openStream(t, url, "headroom")
	s.requireEvent(t, "ro-live", loadFrameTimeout)
	s.close()
	hub := app.liveHub()
	waitFor(t, "the subscriber to detach while the source is still retained", func() bool {
		return hub.connectionCount() == 0 && hub.sourceCount() == 1
	})
	accounted := hub.accountedBytes()
	withSource := heapInUseBytes()
	releaseHub(t, app, clock)
	heap := int64(withSource) - int64(heapInUseBytes())

	if accounted <= 0 {
		t.Fatalf("retained source accounted %d bytes, want a real measurement", accounted)
	}
	if heap > accounted*cacheAccountingHeadroom {
		t.Fatalf("one retained source costs %d heap bytes for %d accounted (ratio %.2f), past cacheAccountingHeadroom = %d",
			heap, accounted, float64(heap)/float64(accounted), cacheAccountingHeadroom)
	}
}

// assertLimitsRejectBeforeTheirBound drives each of the three per-pod bounds
// past its ceiling and asserts two things at once: the extra stream is refused
// with 429, and the resource that limit governs never actually exceeded its
// configured value. A limit that admitted first and measured afterwards would
// pass the status check and fail here.
func assertLimitsRejectBeforeTheirBound(t *testing.T) {
	t.Helper()
	streamURL := func(ts *httptest.Server, namespace string) string {
		return ts.URL + "/clusters/test/namespaces/" + namespace + "/pods/_stream"
	}
	requireRejected := func(t *testing.T, resp *http.Response, label string) {
		t.Helper()
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("%s status = %d, want 429", label, resp.StatusCode)
		}
		if got := resp.Header.Get("Retry-After"); got != "10" {
			t.Fatalf("%s Retry-After = %q, want 10", label, got)
		}
	}

	t.Run("connections", func(t *testing.T) {
		const maxConnections = 8
		app, ts, _ := newLiveMetricsFixture(t, &config.Config{LiveMaxConnections: maxConnections})
		for i := range maxConnections {
			openStream(t, streamURL(ts, "default"), fmt.Sprintf("bound-conn-%d", i)).
				requireEvent(t, "ro-live", loadFrameTimeout)
		}
		requireRejected(t, dialStream(t, streamURL(ts, "default"), "bound-conn-over"), "over-capacity connection")
		requireMetric(t, scrapeMetrics(t, app), "readout_live_connections_active", maxConnections)
		if got := app.liveHub().connectionCount(); got > maxConnections {
			t.Fatalf("connections = %d, want no more than the configured %d", got, maxConnections)
		}
	})

	t.Run("sources", func(t *testing.T) {
		const maxSources = 2
		app, ts, _ := newLiveMetricsFixture(t, &config.Config{LiveMaxSources: maxSources})
		for i, namespace := range []string{"default", "big"} {
			openStream(t, streamURL(ts, namespace), fmt.Sprintf("bound-source-%d", i)).
				requireEvent(t, "ro-live", loadFrameTimeout)
		}
		requireRejected(t, dialStream(t, streamURL(ts, "states"), "bound-source-over"), "third distinct scope")
		requireMetric(t, scrapeMetrics(t, app), "readout_watchhub_sources_active", maxSources)
		if got := app.liveHub().sourceCount(); got > maxSources {
			t.Fatalf("sources = %d, want no more than the configured %d", got, maxSources)
		}
	})

	t.Run("cache", func(t *testing.T) {
		// Wide enough for the handful of rows in `default`, far too small for
		// the 600-row scope at any headroom multiplier.
		const maxCacheAccountedBytes = 64 << 10
		app, ts, _ := newLiveMetricsFixture(t, &config.Config{LiveMaxCacheAccountedBytes: maxCacheAccountedBytes})
		hub := app.liveHub()
		openStream(t, streamURL(ts, "default"), "bound-cache-small").requireEvent(t, "ro-live", loadFrameTimeout)
		if charged := hub.cacheChargedBytes(); charged <= 0 || charged > maxCacheAccountedBytes {
			t.Fatalf("small scope charged %d bytes against a %d bound; the anchor scope must fit", charged, maxCacheAccountedBytes)
		}
		requireRejected(t, dialStream(t, streamURL(ts, "big"), "bound-cache-over"), "oversized scope")
		waitFor(t, "the rejected source to be discarded", func() bool { return hub.sourceCount() == 1 })
		if charged := hub.cacheChargedBytes(); charged > maxCacheAccountedBytes {
			t.Fatalf("charged bytes = %d, want no more than the configured %d", charged, maxCacheAccountedBytes)
		}
		requireMetric(t, scrapeMetrics(t, app), `readout_live_admissions_total{result="cache_limit"}`, 1)
	})
}

// assertChurnDoesNotGrowAccounting pins the accounting property a long-lived
// source depends on: watch traffic that replaces rows with equally sized ones
// leaves the total exactly where it was (no per-event accumulation), a delete
// gives its bytes back, and re-adding the row returns the total to its
// starting value. A source that added on replace without subtracting would
// climb here until it tripped its own cache limit.
func assertChurnDoesNotGrowAccounting(t *testing.T) {
	t.Helper()
	app, ts, fake := newLiveMetricsFixture(t, nil)
	hub := app.liveHub()
	s := openStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream", "churn")
	s.requireEvent(t, "ro-live", loadFrameTimeout)

	// Both statuses are the same length, so an unchanged retained state means
	// a byte-identical accounted total, not merely a similar one.
	const (
		statusA = "Alpha"
		statusB = "Bravo"
	)
	churn := func(status string) {
		t.Helper()
		postStreamScript(t, fake.URL, `{"events":[`+loadPodEvent("default", "MODIFIED", status)+`]}`)
		awaitFrame(t, s, status)
	}
	churn(statusA)
	settled := hub.accountedBytes()
	for range 20 {
		churn(statusB)
		churn(statusA)
	}
	if got := hub.accountedBytes(); got != settled {
		t.Fatalf("accounted bytes after 40 replacements = %d, want the unchanged %d", got, settled)
	}

	postStreamScript(t, fake.URL, `{"events":[`+loadPodEvent("default", "DELETED", statusA)+`]}`)
	waitFor(t, "the delete to give its bytes back", func() bool { return hub.accountedBytes() < settled })

	postStreamScript(t, fake.URL, `{"events":[`+loadPodEvent("default", "ADDED", statusA)+`]}`)
	waitFor(t, "the re-added row to restore the original total", func() bool { return hub.accountedBytes() == settled })
}

// awaitFrame reads frames until one carries the needle. It is
// requireFrameContaining with the run's own budget rather than the ordinary
// suite's five seconds: fanning one change out to five hundred subscribers
// under the race detector takes considerably longer than that.
func awaitFrame(t *testing.T, s *sseStream, needle string) {
	t.Helper()
	deadline := time.Now().Add(loadFrameTimeout)
	for {
		frame := decodeFrame(t, s.requireEvent(t, "ro-live", loadFrameTimeout))
		if strings.Contains(frame.HTML, needle) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no frame carrying %q within %s", needle, loadFrameTimeout)
		}
	}
}

// loadPodEvent builds one scripted pods entry against the first row of a
// namespace's list, with a Status cell the frame can be recognised by.
func loadPodEvent(namespace, kind, status string) string {
	name := "nginx"
	if namespace == "big" {
		name = "big-pod-0001"
	}
	return fmt.Sprintf(
		`{"path":"/api/v1/namespaces/%s/pods","type":%q,"cells":[%q,"0/1",%q,"3","10m"],`+
			`"object":{"apiVersion":"v1","kind":"Pod","metadata":{"name":%q,"namespace":%q},"status":{"phase":%q}}}`,
		namespace, kind, name, status, name, namespace, status)
}

// renderLoadProfile lays the anchors out in a fixed table, followed by the
// derivation line for the accounting headroom constant.
func renderLoadProfile(samples []loadSample, retainedAccounted, retainedHeap int64) string {
	var out strings.Builder
	out.WriteString("WatchHub capacity profile (" + runtime.GOOS + "/" + runtime.GOARCH +
		", " + strconv.Itoa(runtime.NumCPU()) + " CPU)\n")
	w := tabwriter.NewWriter(&out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ANCHOR\tSUBS\tSOURCES\tSNAPSHOT_B\tACCOUNTED_B\tHEAP_DELTA_MIB\tHEAP_PER_SUB_KIB\tCPU_S\tMAXRSS_MIB\tGOROUTINES\tFDS\tFLUSH_P50_MS\tFLUSH_P99_MS\tFLUSH_MEAN_MS")
	for i := range samples {
		s := &samples[i]
		_, _ = fmt.Fprintf(w, "%s\t%d\t%.0f\t%.0f\t%d\t%.1f\t%.1f\t%.2f\t%.1f\t%d\t%d\t%.1f\t%.1f\t%.1f\n",
			s.anchor, s.subscribers, s.sources, s.snapshotBytes, s.accounted,
			float64(s.heapDelta)/(1<<20), float64(s.heapDelta)/float64(s.subscribers)/(1<<10),
			s.cpuSeconds, float64(s.maxRSSBytes)/(1<<20),
			s.goroutines, s.descriptors,
			s.flushP50*1000, s.flushP99*1000, s.flushMean*1000)
	}
	_ = w.Flush()
	fmt.Fprintf(&out, "\none retained source in isolation: accounted %d B, heap %d B, ratio %.2f (cacheAccountingHeadroom = %d)\n",
		retainedAccounted, retainedHeap, float64(retainedHeap)/float64(retainedAccounted), cacheAccountingHeadroom)
	return out.String()
}

// loadHistogram is one scraped Prometheus histogram: its cumulative buckets in
// ascending order plus the sample count and sum.
type loadHistogram struct {
	bounds []float64
	cum    []float64
	count  float64
	sum    float64
}

func (h loadHistogram) mean() float64 {
	if h.count == 0 {
		return 0
	}
	return h.sum / h.count
}

// quantile interpolates within the bucket the rank falls in, the same way
// Prometheus' histogram_quantile does, so a number read here matches what an
// operator would see on a dashboard.
func (h loadHistogram) quantile(q float64) float64 {
	if h.count == 0 {
		return 0
	}
	rank := q * h.count
	prevCum, prevBound := 0.0, 0.0
	for i, cum := range h.cum {
		if cum < rank {
			prevCum, prevBound = cum, h.bounds[i]
			continue
		}
		if math.IsInf(h.bounds[i], 1) || cum == prevCum {
			return prevBound
		}
		return prevBound + (h.bounds[i]-prevBound)*(rank-prevCum)/(cum-prevCum)
	}
	return prevBound
}

// scrapeHistogram parses one histogram family out of a text exposition body.
func scrapeHistogram(t *testing.T, body, name string) loadHistogram {
	t.Helper()
	h := loadHistogram{}
	prefix := name + `_bucket{le="`
	for _, line := range strings.Split(body, "\n") {
		rest, ok := strings.CutPrefix(line, prefix)
		if !ok {
			continue
		}
		bound, after, ok := strings.Cut(rest, `"} `)
		if !ok {
			t.Fatalf("unparsable bucket line %q", line)
		}
		upper, err := strconv.ParseFloat(bound, 64)
		if err != nil {
			t.Fatalf("bucket %q of %s has an unparsable bound: %v", bound, name, err)
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(after), 64)
		if err != nil {
			t.Fatalf("bucket %q of %s has an unparsable value: %v", bound, name, err)
		}
		h.bounds = append(h.bounds, upper)
		h.cum = append(h.cum, value)
	}
	if len(h.bounds) == 0 {
		t.Fatalf("no buckets for histogram %q", name)
	}
	h.count, _ = metricValue(t, body, name+"_count")
	h.sum, _ = metricValue(t, body, name+"_sum")
	return h
}

// histogramMean is the average sample of a histogram, which for the snapshot
// family is the average measured size of one authoritative source snapshot.
func histogramMean(t *testing.T, body, name string) float64 {
	t.Helper()
	count, _ := metricValue(t, body, name+"_count")
	sum, _ := metricValue(t, body, name+"_sum")
	if count == 0 {
		return 0
	}
	return sum / count
}

// heapInUseBytes settles the heap and reports what is still reachable. Two
// collections rather than one so finalizer-freed objects from the first pass
// are actually gone by the reading.
func heapInUseBytes() uint64 {
	runtime.GC()
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.HeapAlloc
}

func processCPUSeconds(t *testing.T) float64 {
	t.Helper()
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		t.Fatalf("getrusage: %v", err)
	}
	return float64(usage.Utime.Nano()+usage.Stime.Nano()) / float64(time.Second)
}

// processMaxRSSBytes is the process-wide high-water mark, so it only ever
// grows across anchors: read it as "the peak this run reached by here", not as
// one anchor's own footprint. Linux reports kilobytes, Darwin bytes.
func processMaxRSSBytes(t *testing.T) int64 {
	t.Helper()
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		t.Fatalf("getrusage: %v", err)
	}
	maxrss := int64(usage.Maxrss)
	if runtime.GOOS == "darwin" {
		return maxrss
	}
	return maxrss * 1024
}

// openDescriptorCount reports how many file descriptors this process holds
// (-1 where neither per-process directory exists).
func openDescriptorCount() int {
	for _, dir := range []string{"/proc/self/fd", "/dev/fd"} {
		entries, err := os.ReadDir(dir)
		if err == nil {
			return len(entries)
		}
	}
	return -1
}

// releaseHub fires the hub's pending timers until nothing is retained, which
// is how the run reaches the far side of the 30-second retention window
// without waiting it out.
func releaseHub(t *testing.T, app *Server, clock *loadHubClock) {
	t.Helper()
	hub := app.liveHub()
	deadline := time.Now().Add(30 * time.Second)
	for {
		clock.Release()
		if hub.sourceCount() == 0 && hub.connectionCount() == 0 && hub.accountedBytes() == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("hub never drained: sources=%d connections=%d accounted=%d",
				hub.sourceCount(), hub.connectionCount(), hub.accountedBytes())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// loadHubClock reports REAL time -- an event-to-flush histogram measured
// against a frozen clock would be meaningless -- while keeping every timer it
// hands out so the run can fire them early. Timers still fire on their own
// schedule if nobody releases them.
type loadHubClock struct {
	mu     sync.Mutex
	timers map[*loadHubTimer]struct{}
}

func newLoadHubClock() *loadHubClock {
	return &loadHubClock{timers: map[*loadHubTimer]struct{}{}}
}

func (c *loadHubClock) Now() time.Time { return time.Now() }

func (c *loadHubClock) AfterFunc(d time.Duration, f func()) hubTimer {
	timer := &loadHubTimer{clock: c, fn: f}
	// Registered BEFORE it is armed: a timer that fired or was stopped first
	// would otherwise be forgotten and then re-registered by this insert, and
	// a dead entry in the map pins everything its function closes over.
	c.mu.Lock()
	c.timers[timer] = struct{}{}
	c.mu.Unlock()
	timer.mu.Lock()
	timer.timer = time.AfterFunc(d, timer.run)
	timer.mu.Unlock()
	return timer
}

// Release fires every timer that has neither fired nor been stopped.
func (c *loadHubClock) Release() {
	c.mu.Lock()
	pending := make([]*loadHubTimer, 0, len(c.timers))
	for timer := range c.timers {
		pending = append(pending, timer)
	}
	c.mu.Unlock()
	for _, timer := range pending {
		timer.run()
	}
}

func (c *loadHubClock) forget(timer *loadHubTimer) {
	c.mu.Lock()
	delete(c.timers, timer)
	c.mu.Unlock()
}

// loadHubTimer runs its function at most once, whichever of the real deadline
// and an early Release gets there first.
type loadHubTimer struct {
	clock *loadHubClock
	timer *time.Timer
	fn    func()

	mu   sync.Mutex
	done bool
}

func (t *loadHubTimer) run() {
	t.mu.Lock()
	already := t.done
	t.done = true
	timer, fn := t.timer, t.fn
	t.fn = nil
	t.mu.Unlock()
	// Disarm the underlying runtime timer even when this call IS that timer
	// firing: an early Release otherwise leaves the real deadline armed for
	// its full delay, and a live runtime timer pins everything its function
	// closes over -- which for the retention timer is the whole source.
	if timer != nil {
		timer.Stop()
	}
	t.clock.forget(t)
	if !already && fn != nil {
		fn()
	}
}

func (t *loadHubTimer) Stop() bool {
	t.mu.Lock()
	already := t.done
	t.done = true
	timer := t.timer
	t.fn = nil
	t.mu.Unlock()
	if timer != nil {
		timer.Stop()
	}
	t.clock.forget(t)
	return !already
}
