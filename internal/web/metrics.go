package web

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kbelokon/readout/internal/hooks"
	"github.com/kbelokon/readout/internal/kube"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type appMetrics struct {
	registry       *prometheus.Registry
	requestCount   *prometheus.CounterVec
	requestLatency *prometheus.HistogramVec
	up             prometheus.Gauge

	// Domain metrics name the backend boundary when something is slow or
	// failing: which cluster + kube operation + result, which stream terminal
	// reason, which hook + result. Their label cardinality is bounded by
	// construction (configured cluster names, the fixed operation/hook enums, the
	// FailureKind + "ok" results, the seven stream terminals).
	kubeRequests   *prometheus.CounterVec
	kubeDuration   *prometheus.HistogramVec
	streamTerminal *prometheus.CounterVec
	hookDuration   *prometheus.HistogramVec

	// Live capacity metrics answer the operator's questions about this pod:
	// is it full, what is it full OF (connections, distinct watches, retained
	// bytes), how much sharing is it getting, and how fast does a cluster
	// change reach a browser. The gauges carry no labels at all, and the two
	// counters carry only their closed result enums; nothing here is labelled
	// by user, token, cluster, namespace, resource, selector or object, so the
	// whole surface is bounded regardless of traffic.
	liveConnections         prometheus.Gauge
	liveConnectionsCapacity prometheus.Gauge
	liveAdmissions          *prometheus.CounterVec
	liveTimeToSnapshot      prometheus.Histogram
	liveSessionDuration     prometheus.Histogram
	hubSources              prometheus.Gauge
	hubSourcesCapacity      prometheus.Gauge
	hubSubscribers          prometheus.Gauge
	hubCacheBytes           prometheus.Gauge
	hubCacheCapacity        prometheus.Gauge
	hubSourceOutcomes       *prometheus.CounterVec
	hubRelists              prometheus.Counter
	hubEventToFlush         prometheus.Histogram
	hubSnapshotBytes        prometheus.Histogram
}

// Admission results for readout_live_admissions_total. The enum is closed: a
// Live stream is either admitted or refused by exactly one of the three
// per-pod bounds. Failures that are not capacity decisions (a denied or
// unreachable cluster) are not admissions and are counted by the kube metrics
// instead.
const (
	liveAdmissionAccepted        = "accepted"
	liveAdmissionConnectionLimit = "connection_limit"
	liveAdmissionSourceLimit     = "source_limit"
	liveAdmissionCacheLimit      = "cache_limit"
)

// The closed label enums the counters are pre-initialized with, so a scrape
// carries an explicit zero for an outcome that has not happened yet: an alert
// on `rate(...{result="cache_limit"}[5m])` must be able to tell "never
// happened" from "the series does not exist".
var (
	liveAdmissionResults  = []string{liveAdmissionAccepted, liveAdmissionConnectionLimit, liveAdmissionSourceLimit, liveAdmissionCacheLimit}
	hubSourceResultValues = []string{hubSourceCreated, hubSourceReused, hubSourceFailed}
	streamTerminalReasons = []string{streamTerminalAuth, streamTerminalWatchFailed, streamTerminalShutdown, streamTerminalLifetime, streamTerminalClientClose, streamTerminalSlowWriter, streamTerminalProtocol}
)

// appMetrics is the hub's metrics sink: the hub reports source lifecycle,
// connection slots and retained bytes through the narrow interface in
// watchhub.go, and the Prometheus types stay here.
var _ hubMetricsSink = (*appMetrics)(nil)

func newAppMetrics() *appMetrics {
	registry := prometheus.NewRegistry()
	requestCount := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "readout_http_requests_total",
		Help: "Total HTTP requests processed, by method, route and status code.",
	}, []string{"method", "path", "status"})
	requestLatency := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "readout_http_request_duration_seconds",
		Help:    "HTTP request latency in seconds, by method and route.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path"})
	up := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "readout_up",
		Help: "Application liveness.",
	})
	up.Set(1)
	kubeRequests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "readout_kube_requests_total",
		Help: "Total kube API requests, by target cluster, operation and result.",
	}, []string{"target_cluster", "operation", "result"})
	kubeDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "readout_kube_request_duration_seconds",
		Help:    "Kube API request latency in seconds, by target cluster and operation.",
		Buckets: prometheus.DefBuckets,
	}, []string{"target_cluster", "operation"})
	streamTerminal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "readout_stream_terminal_total",
		Help: "Total Live stream terminations, by reason.",
	}, []string{"reason"})
	hookDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "readout_hook_duration_seconds",
		Help:    "Hook call latency in seconds, by hook and result.",
		Buckets: prometheus.DefBuckets,
	}, []string{"hook", "result"})
	liveConnections := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "readout_live_connections_active",
		Help: "Open Live SSE connections on this pod.",
	})
	liveConnectionsCapacity := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "readout_live_connections_capacity",
		Help: "Configured live.maxConnections for this pod.",
	})
	liveAdmissions := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "readout_live_admissions_total",
		Help: "Live stream admission decisions, by result.",
	}, []string{"result"})
	liveTimeToSnapshot := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "readout_live_time_to_snapshot_seconds",
		Help:    "Seconds from a Live request arriving to its first snapshot frame being flushed.",
		Buckets: prometheus.DefBuckets,
	})
	liveSessionDuration := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "readout_live_session_duration_seconds",
		Help: "Seconds an admitted Live session stayed connected, sampled once at its terminal outcome.",
		// Sessions run from a page load to the 12-hour lifetime cap, so the
		// buckets span seconds to days rather than the default 10s ceiling.
		Buckets: prometheus.ExponentialBuckets(1, 4, 10),
	})
	hubSources := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "readout_watchhub_sources_active",
		Help: "Distinct upstream list+watch sources this pod currently owns.",
	})
	hubSourcesCapacity := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "readout_watchhub_sources_capacity",
		Help: "Configured live.maxSources for this pod.",
	})
	hubSubscribers := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "readout_watchhub_subscribers_active",
		Help: "Live subscriptions attached to this pod's sources; divided by sources_active it is the sharing ratio.",
	})
	hubCacheBytes := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "readout_watchhub_cache_accounted_bytes",
		Help: "Accounted retained Table state across this pod's sources.",
	})
	hubCacheCapacity := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "readout_watchhub_cache_accounted_bytes_capacity",
		Help: "Configured live.maxCacheAccountedBytes. Admission compares the accounted total multiplied by an internal headroom factor against this bound.",
	})
	hubSourceOutcomes := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "readout_watchhub_sources_total",
		Help: "WatchHub source lookups, by result.",
	}, []string{"result"})
	hubRelists := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "readout_watchhub_relists_total",
		Help: "Recovery LISTs performed after an expired watch resourceVersion (410 Gone).",
	})
	hubEventToFlush := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "readout_watchhub_event_to_flush_seconds",
		Help:    "Seconds from a source applying a change to a subscriber flushing the frame that carries it.",
		Buckets: prometheus.DefBuckets,
	})
	hubSnapshotBytes := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "readout_watchhub_snapshot_bytes",
		Help: "Accounted size of one authoritative source snapshot (initial list or relist).",
		// One row is hundreds of bytes and a big namespace is tens of MiB:
		// 1 KiB to 256 MiB in ten buckets covers every admissible scope.
		Buckets: prometheus.ExponentialBuckets(1024, 4, 10),
	})
	registry.MustRegister(requestCount, requestLatency, up, kubeRequests, kubeDuration, streamTerminal, hookDuration,
		liveConnections, liveConnectionsCapacity, liveAdmissions, liveTimeToSnapshot, liveSessionDuration,
		hubSources, hubSourcesCapacity, hubSubscribers, hubCacheBytes, hubCacheCapacity,
		hubSourceOutcomes, hubRelists, hubEventToFlush, hubSnapshotBytes)
	for _, result := range liveAdmissionResults {
		liveAdmissions.WithLabelValues(result)
	}
	for _, result := range hubSourceResultValues {
		hubSourceOutcomes.WithLabelValues(result)
	}
	for _, reason := range streamTerminalReasons {
		streamTerminal.WithLabelValues(reason)
	}
	return &appMetrics{
		registry:                registry,
		requestCount:            requestCount,
		requestLatency:          requestLatency,
		up:                      up,
		kubeRequests:            kubeRequests,
		kubeDuration:            kubeDuration,
		streamTerminal:          streamTerminal,
		hookDuration:            hookDuration,
		liveConnections:         liveConnections,
		liveConnectionsCapacity: liveConnectionsCapacity,
		liveAdmissions:          liveAdmissions,
		liveTimeToSnapshot:      liveTimeToSnapshot,
		liveSessionDuration:     liveSessionDuration,
		hubSources:              hubSources,
		hubSourcesCapacity:      hubSourcesCapacity,
		hubSubscribers:          hubSubscribers,
		hubCacheBytes:           hubCacheBytes,
		hubCacheCapacity:        hubCacheCapacity,
		hubSourceOutcomes:       hubSourceOutcomes,
		hubRelists:              hubRelists,
		hubEventToFlush:         hubEventToFlush,
		hubSnapshotBytes:        hubSnapshotBytes,
	}
}

// setLiveCapacity publishes the resolved per-pod bounds, so utilization is
// readable from the scrape alone: an operator comparing an _active gauge with
// its _capacity sibling never has to know what this pod's config file said.
func (m *appMetrics) setLiveCapacity(limits liveLimits) {
	m.liveConnectionsCapacity.Set(float64(limits.maxConnections))
	m.hubSourcesCapacity.Set(float64(limits.maxSources))
	m.hubCacheCapacity.Set(float64(limits.maxCacheAccountedBytes))
}

// The hubMetricsSink implementation. Every method is called from the hub's own
// goroutines (an actor, or a caller holding the hub map lock), so each does
// exactly one atomic Prometheus update and nothing else.
func (m *appMetrics) observeHubSource(result string) {
	m.hubSourceOutcomes.WithLabelValues(result).Inc()
}

func (m *appMetrics) observeHubCounts(sources, subscribers int) {
	m.hubSources.Set(float64(sources))
	m.hubSubscribers.Set(float64(subscribers))
}

func (m *appMetrics) observeHubCache(bytes int64) {
	m.hubCacheBytes.Set(float64(bytes))
}

func (m *appMetrics) observeHubConnections(active int) {
	m.liveConnections.Set(float64(active))
}

func (m *appMetrics) observeHubRelist() {
	m.hubRelists.Inc()
}

func (m *appMetrics) observeHubSnapshotBytes(bytes int64) {
	m.hubSnapshotBytes.Observe(float64(bytes))
}

// kubeObserverFactory returns the per-cluster request observer the kube Manager
// bakes into each Client. The cluster name is closed over here (the kube
// observer signature carries no cluster), and the result label is "ok" or the
// shared FailureKind classification so it lines up with every other failure
// surface. The setup-only WatchTable timing lands in the same histogram as the
// short list/get calls.
func (m *appMetrics) kubeObserverFactory() func(cluster string) kube.RequestObserver {
	return func(cluster string) kube.RequestObserver {
		return func(operation string, err error, elapsed time.Duration) {
			result := "ok"
			if err != nil {
				result = string(kube.ClassifyError(err))
			}
			m.kubeRequests.WithLabelValues(cluster, operation, result).Inc()
			m.kubeDuration.WithLabelValues(cluster, operation).Observe(elapsed.Seconds())
		}
	}
}

// hookObserver records hook call duration with an "ok"/"error" result. Hook
// failures are coarse (a non-2xx or a transport error), so the binary result is
// the right grain here rather than the kube FailureKind taxonomy.
func (m *appMetrics) hookObserver() hooks.Observer {
	return func(hook string, err error, elapsed time.Duration) {
		result := "ok"
		if err != nil {
			result = "error"
		}
		m.hookDuration.WithLabelValues(hook, result).Observe(elapsed.Seconds())
	}
}

// observeStreamTerminal counts one Live stream termination by reason. Every
// exit funnels through streamSession.finish, which calls this exactly once per
// admitted session, so the counter is a session census and not an event count.
func (s *Server) observeStreamTerminal(reason string) {
	s.metrics.streamTerminal.WithLabelValues(reason).Inc()
}

// observeLiveAdmission counts one admission decision. Exactly one sample is
// recorded per Live request that reached a capacity decision.
func (s *Server) observeLiveAdmission(result string) {
	s.metrics.liveAdmissions.WithLabelValues(result).Inc()
}

// observeLiveTimeToSnapshot records how long an admitted request took to reach
// its first flushed snapshot: discovery, the shared source's LIST (or the wait
// for one already in flight), the join overlays and the initial render.
func (s *Server) observeLiveTimeToSnapshot(d time.Duration) {
	s.metrics.liveTimeToSnapshot.Observe(d.Seconds())
}

// observeLiveSessionDuration records how long a session lasted, sampled once
// beside its terminal outcome.
func (s *Server) observeLiveSessionDuration(d time.Duration) {
	s.metrics.liveSessionDuration.Observe(d.Seconds())
}

// observeLiveEventToFlush records the delivery latency of one frame: the
// source applied the change at the revision's eventAt, this subscriber
// flushed it now. A frame that carries a revision older than the session's
// clock reading cannot produce a negative sample.
func (s *Server) observeLiveEventToFlush(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.metrics.hubEventToFlush.Observe(d.Seconds())
}

func (s *Server) observeMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(ww, r)
		route := r.Pattern
		if route == "" {
			route = "__unmatched__"
		} else if method, path, ok := strings.Cut(route, " "); ok && method == r.Method {
			route = path
		}
		// The `_stream` SSE routes are excluded from the duration histogram:
		// a stream's lifetime is minutes of intentional held-open
		// connection, not request latency — one 30-minute stream would
		// permanently distort every latency quantile. Streams stay counted in
		// the request totals below.
		if !strings.HasSuffix(route, "/_stream") {
			s.metrics.requestLatency.WithLabelValues(r.Method, route).Observe(time.Since(start).Seconds())
		}
		s.metrics.requestCount.WithLabelValues(r.Method, route, strconv.Itoa(ww.status)).Inc()
		if !s.cfg.NoAccessLogs {
			slog.Info("request", "method", r.Method, "path", r.URL.Path, "route", route, "status", ww.status, "duration", time.Since(start).String())
		}
	})
}

func (s *Server) metricsHandler(w http.ResponseWriter, r *http.Request) {
	s.MetricsHandler().ServeHTTP(w, r)
}

func (s *Server) MetricsHandler() http.Handler {
	return promhttp.HandlerFor(s.metrics.registry, promhttp.HandlerOpts{})
}

type statusWriter struct {
	http.ResponseWriter
	status     int
	wroteFinal bool
}

func (w *statusWriter) WriteHeader(status int) {
	if w.wroteFinal {
		return
	}
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		// Informational responses (for example, 103 Early Hints) precede the
		// final response and must not become the status metric.
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.wroteFinal = true
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if !w.wroteFinal {
		w.wroteFinal = true
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

// FlushError preserves an underlying error-aware flush through the metrics
// wrapper. ResponseController stops at the first Flush/FlushError method it
// finds, so a void-only Flush here would otherwise hide transport failures from
// compression and SSE callers.
func (w *statusWriter) FlushError() error {
	if !w.wroteFinal {
		w.wroteFinal = true
		w.status = http.StatusOK
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *statusWriter) Flush() {
	_ = w.FlushError()
}

// Unwrap exposes the wrapped writer for http.ResponseController, so any
// future wrapper stacked above statusWriter can still reach the underlying
// connection's Flusher/deadline controls through the standard unwrap chain.
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
