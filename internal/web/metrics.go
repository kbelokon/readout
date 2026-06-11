package web

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type appMetrics struct {
	registry       *prometheus.Registry
	requestCount   *prometheus.CounterVec
	requestLatency *prometheus.HistogramVec
	up             prometheus.Gauge
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
	registry.MustRegister(requestCount, requestLatency, up)
	return &appMetrics{registry: registry, requestCount: requestCount, requestLatency: requestLatency, up: up}
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
		// The `_stream` SSE routes are excluded from the duration histogram
		// (D19): a stream's lifetime is minutes of intentional held-open
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
// the `_stream` endpoint flushes per message through this (D19).
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
