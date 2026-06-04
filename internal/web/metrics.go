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
		s.metrics.requestLatency.WithLabelValues(r.Method, route).Observe(time.Since(start).Seconds())
		s.metrics.requestCount.WithLabelValues(r.Method, route, strconv.Itoa(ww.status)).Inc()
		if !s.cfg.NoAccessLogs {
			slog.Info("request", "method", r.Method, "path", r.URL.Path, "route", route, "status", ww.status, "duration", time.Since(start).String())
		}
	})
}

func (s *Server) metricsHandler(w http.ResponseWriter, r *http.Request) {
	promhttp.HandlerFor(s.metrics.registry, promhttp.HandlerOpts{}).ServeHTTP(w, r)
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
