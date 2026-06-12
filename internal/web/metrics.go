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
	// FailureKind + "ok" results, the four stream terminals).
	kubeRequests   *prometheus.CounterVec
	kubeDuration   *prometheus.HistogramVec
	streamTerminal *prometheus.CounterVec
	hookDuration   *prometheus.HistogramVec
}

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
	registry.MustRegister(requestCount, requestLatency, up, kubeRequests, kubeDuration, streamTerminal, hookDuration)
	return &appMetrics{
		registry:       registry,
		requestCount:   requestCount,
		requestLatency: requestLatency,
		up:             up,
		kubeRequests:   kubeRequests,
		kubeDuration:   kubeDuration,
		streamTerminal: streamTerminal,
		hookDuration:   hookDuration,
	}
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

// observeStreamTerminal counts one Live stream termination by reason. All six
// terminal call sites funnel through streamSession.terminal, which calls this.
func (s *Server) observeStreamTerminal(reason string) {
	s.metrics.streamTerminal.WithLabelValues(reason).Inc()
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
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// Flush forwards to the wrapped writer's http.Flusher. The embedded
// ResponseWriter field hides the interface (a struct-embedded interface only
// re-exposes its OWN methods), which would buffer SSE pushes indefinitely —
// the `_stream` endpoint flushes per message through this.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the wrapped writer for http.ResponseController, so any
// future wrapper stacked above statusWriter can still reach the underlying
// connection's Flusher/deadline controls through the standard unwrap chain.
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
