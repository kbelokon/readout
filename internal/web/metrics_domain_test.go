package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kbelokon/readout/internal/config"
	fakeapi "github.com/kbelokon/readout/internal/fakekube"
)

// TestDomainMetricsScrape exercises the three domain-metric boundaries against
// the fakeapi harness — a kube list (through a list page), a stream terminal,
// and a hook call (the authorization hook in headers auth mode) — then scrapes
// /metrics and asserts each series family is present with its expected labels.
// It is the end-to-end proof that the observer wiring reaches the registry.
func TestDomainMetricsScrape(t *testing.T) {
	fake, err := fakeapi.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)

	// The authorization hook: a trivial allow. Headers auth mode runs it on every
	// non-public request, so it fires on the list and stream requests below.
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"allowed":true}`))
	}))
	t.Cleanup(hook.Close)

	app := newTestServerWithConfig(t, &config.Config{
		Port:                 8080,
		Clusters:             []config.ClusterConnection{{Name: "test", Server: fake.URL}},
		DefaultTheme:         "dark",
		AuthMode:             config.AuthModeHeaders,
		TrustedHeaderUser:    "X-Forwarded-User",
		AuthorizationHookURL: hook.URL,
	})
	app.streamTuning.maxLifetime = 200 * time.Millisecond
	ts := httptest.NewServer(app.Handler())
	t.Cleanup(ts.Close)

	// 1) A list page: routes a kube Table list AND fires the authorization hook.
	listReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/clusters/test/namespaces/default/pods", nil)
	listReq.Header.Set("X-Forwarded-User", "alice")
	listResp, err := http.DefaultClient.Do(listReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d", listResp.StatusCode)
	}

	// 2) A stream driven to its lifetime terminal: increments the terminal counter.
	streamReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/clusters/test/namespaces/default/pods/_stream", nil)
	streamReq.Header.Set("X-Forwarded-User", "alice")
	setTestLiveHeaders(streamReq, "metrics-domain")
	s := openStreamRequest(t, streamReq)
	s.requireEvent(t, "ro-live", 5*time.Second)
	term := s.requireEvent(t, "ro-live", 3*time.Second)
	if reason := decodeFrame(t, term).Reason; reason != streamTerminalLifetime {
		t.Fatalf("terminal reason = %q, want lifetime", reason)
	}

	// 3) Scrape /metrics (public, bypasses auth) through the real handler.
	rec := httptest.NewRecorder()
	app.MetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", rec.Code)
	}
	body := rec.Body.String()

	wantSeries := []string{
		// kube requests: a list against the configured cluster with an ok result.
		`readout_kube_requests_total{operation="list",result="ok",target_cluster="test"}`,
		// kube duration histogram for the same cluster/operation.
		`readout_kube_request_duration_seconds_count{operation="list",target_cluster="test"}`,
		// stream terminal counter for the lifetime reason.
		`readout_stream_terminal_total{reason="lifetime"}`,
		// hook duration histogram for the authorization hook, ok result.
		`readout_hook_duration_seconds_count{hook="authorization",result="ok"}`,
	}
	for _, needle := range wantSeries {
		if !strings.Contains(body, needle) {
			t.Fatalf("metrics missing series %q in:\n%s", needle, body)
		}
	}
}

// TestDomainMetricsScrapeErrorLabels is the error-side sibling of
// TestDomainMetricsScrape: it drives a failing kube list (the fakeapi
// fail-lists 500 mode, which the client classifies as upstream_5xx) and a
// failing hook (the resource-prerender hook pointed at a server that always
// returns 500), then scrapes /metrics and asserts the error-side label values
// are present. Without this, an observer that recorded result="ok"
// unconditionally would still pass the ok-only scrape test.
func TestDomainMetricsScrapeErrorLabels(t *testing.T) {
	fake, err := fakeapi.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)

	// The authorization hook allows every request through, so the kube list and
	// the prerender hook below are actually reached.
	authHook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"allowed":true}`))
	}))
	t.Cleanup(authHook.Close)

	// The prerender hook always fails: a 500 makes the hook call error, which the
	// observer must record as result="error".
	prerenderHook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(prerenderHook.Close)

	app := newTestServerWithConfig(t, &config.Config{
		Port:                     8080,
		Clusters:                 []config.ClusterConnection{{Name: "test", Server: fake.URL}},
		DefaultTheme:             "dark",
		AuthMode:                 config.AuthModeHeaders,
		TrustedHeaderUser:        "X-Forwarded-User",
		AuthorizationHookURL:     authHook.URL,
		ResourcePrerenderHookURL: prerenderHook.URL,
	})
	ts := httptest.NewServer(app.Handler())
	t.Cleanup(ts.Close)

	// 1) A detail render fires the prerender hook. The object GET succeeds, the
	// hook returns 500, so the request itself fails -- but the hook observer
	// records result="error" before the error propagates.
	detailReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/clusters/test/namespaces/default/pods/nginx", nil)
	detailReq.Header.Set("X-Forwarded-User", "alice")
	detailResp, err := http.DefaultClient.Do(detailReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = detailResp.Body.Close()

	// 2) Arm the fail-lists 500 mode, then issue a list page: the kube list call
	// reaches the apiserver and gets a 5xx Status, classified as upstream_5xx.
	armReq, _ := http.NewRequest(http.MethodGet, fake.URL+"/__control/fail-lists?mode=500", nil)
	armResp, err := http.DefaultClient.Do(armReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = armResp.Body.Close()
	if armResp.StatusCode != http.StatusOK {
		t.Fatalf("arm fail-lists status = %d", armResp.StatusCode)
	}

	listReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/clusters/test/namespaces/default/pods", nil)
	listReq.Header.Set("X-Forwarded-User", "alice")
	listResp, err := http.DefaultClient.Do(listReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = listResp.Body.Close()

	// 3) Scrape /metrics and assert the error-side label values are present.
	rec := httptest.NewRecorder()
	app.MetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", rec.Code)
	}
	body := rec.Body.String()

	wantSeries := []string{
		// kube list against the configured cluster, classified as a 5xx upstream.
		`readout_kube_requests_total{operation="list",result="upstream_5xx",target_cluster="test"}`,
		// prerender hook duration histogram with the error result.
		`readout_hook_duration_seconds_count{hook="prerender",result="error"}`,
	}
	for _, needle := range wantSeries {
		if !strings.Contains(body, needle) {
			t.Fatalf("metrics missing series %q in:\n%s", needle, body)
		}
	}
}

// newLiveMetricsFixture builds the standard one-cluster app over a fresh
// fakeapi and returns the Server ITSELF alongside the running HTTP server, so
// a test can drive real SSE streams and then read the registry those streams
// fed. cfg may be nil (the plain single-cluster default) and tune runs before
// the server starts serving, which is when seams like hubClock are still
// settable.
func newLiveMetricsFixture(t *testing.T, cfg *config.Config, tune ...func(*Server)) (*Server, *httptest.Server, *fakeapi.Server) {
	t.Helper()
	fake, err := fakeapi.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	if cfg == nil {
		cfg = &config.Config{}
	}
	cfg.Port = 8080
	cfg.DefaultTheme = "dark"
	cfg.Clusters = []config.ClusterConnection{{Name: "test", Server: fake.URL}}
	app := newTestServerWithConfig(t, cfg)
	for _, apply := range tune {
		apply(app)
	}
	ts := httptest.NewServer(app.Handler())
	t.Cleanup(ts.Close)
	return app, ts, fake
}

// scrapeMetrics renders the app's own registry through the real Prometheus
// handler, which is the only view an operator ever gets.
func scrapeMetrics(t *testing.T, app *Server) string {
	t.Helper()
	rec := httptest.NewRecorder()
	app.MetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", rec.Code)
	}
	return rec.Body.String()
}

// metricValue reads one exact series ("name" or `name{labels}`) out of a
// scrape. The second result distinguishes an ABSENT series from one that is
// present at zero — the whole point of pre-initializing the closed label
// enums.
func metricValue(t *testing.T, body, series string) (float64, bool) {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		rest, ok := strings.CutPrefix(line, series+" ")
		if !ok {
			continue
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
		if err != nil {
			t.Fatalf("series %q has unparsable value %q", series, rest)
		}
		return value, true
	}
	return 0, false
}

// requireMetric asserts a series is present with an exact value.
func requireMetric(t *testing.T, body, series string, want float64) {
	t.Helper()
	got, ok := metricValue(t, body, series)
	if !ok {
		t.Fatalf("metrics are missing series %q", series)
	}
	if got != want {
		t.Fatalf("%s = %v, want %v", series, got, want)
	}
}

// waitForMetric polls the registry until a series reaches a value. Metrics on
// the far side of a closed connection are recorded by the handler goroutine,
// which the client cannot observe directly.
func waitForMetric(t *testing.T, app *Server, series string, want float64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		body := scrapeMetrics(t, app)
		if got, ok := metricValue(t, body, series); ok && got == want {
			return
		}
		if time.Now().After(deadline) {
			got, ok := metricValue(t, scrapeMetrics(t, app), series)
			t.Fatalf("%s = %v (present=%v), want %v", series, got, ok, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestLiveMetricsSurface is the end-to-end proof that one open Live stream
// lights up every family in the Live/WatchHub surface with the right values:
// the three capacity gauges report the resolved limits, the three _active
// gauges report the one connection/source/subscriber, the accounted-bytes
// gauge is non-zero, and admission, source-lookup, time-to-snapshot,
// event-to-flush and snapshot-size all carry exactly one sample. It also pins
// the closed label enums: every admission result, source result and terminal
// reason is present in the scrape even before it has ever happened, so an
// alert can tell "never happened" from "series missing".
func TestLiveMetricsSurface(t *testing.T) {
	app, ts, _ := newLiveMetricsFixture(t, nil)
	s := openStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream", "live-metrics")
	s.requireEvent(t, "ro-live", 5*time.Second)

	body := scrapeMetrics(t, app)
	for _, result := range liveAdmissionResults {
		if _, ok := metricValue(t, body, `readout_live_admissions_total{result="`+result+`"}`); !ok {
			t.Fatalf("admission result %q has no series", result)
		}
	}
	for _, result := range hubSourceResultValues {
		if _, ok := metricValue(t, body, `readout_watchhub_sources_total{result="`+result+`"}`); !ok {
			t.Fatalf("source result %q has no series", result)
		}
	}
	for _, reason := range streamTerminalReasons {
		if _, ok := metricValue(t, body, `readout_stream_terminal_total{reason="`+reason+`"}`); !ok {
			t.Fatalf("terminal reason %q has no series", reason)
		}
	}

	requireMetric(t, body, "readout_live_connections_capacity", float64(config.DefaultLiveMaxConnections))
	requireMetric(t, body, "readout_watchhub_sources_capacity", float64(config.DefaultLiveMaxSources))
	requireMetric(t, body, "readout_watchhub_cache_accounted_bytes_capacity", float64(config.DefaultLiveMaxCacheAccountedBytes))
	requireMetric(t, body, "readout_live_connections_active", 1)
	requireMetric(t, body, "readout_watchhub_sources_active", 1)
	requireMetric(t, body, "readout_watchhub_subscribers_active", 1)
	requireMetric(t, body, `readout_live_admissions_total{result="accepted"}`, 1)
	requireMetric(t, body, `readout_watchhub_sources_total{result="created"}`, 1)
	requireMetric(t, body, "readout_watchhub_relists_total", 0)
	requireMetric(t, body, "readout_live_time_to_snapshot_seconds_count", 1)
	requireMetric(t, body, "readout_watchhub_snapshot_bytes_count", 1)
	// The session is still open, so nothing has ended yet.
	requireMetric(t, body, "readout_live_session_duration_seconds_count", 0)

	if cache, _ := metricValue(t, body, "readout_watchhub_cache_accounted_bytes"); cache <= 0 {
		t.Fatalf("cache_accounted_bytes = %v, want the retained snapshot to be measured", cache)
	}
	if flushes, ok := metricValue(t, body, "readout_watchhub_event_to_flush_seconds_count"); !ok || flushes < 1 {
		t.Fatalf("event_to_flush count = %v (present=%v), want at least the initial snapshot", flushes, ok)
	}

	// Closing the browser ends the session exactly once, with the outcome that
	// describes it and one session-duration sample beside it.
	s.close()
	waitForMetric(t, app, `readout_stream_terminal_total{reason="client-close"}`, 1)
	waitForMetric(t, app, "readout_live_session_duration_seconds_count", 1)
}

// TestLiveAdmissionRejectsAreCounted drives each of the three per-pod bounds
// and asserts the rejection lands on its own admission result. Without this a
// single mislabelled call site would leave an operator unable to tell WHICH
// resource the pod ran out of, which is the whole point of the enum.
func TestLiveAdmissionRejectsAreCounted(t *testing.T) {
	streamURL := func(ts *httptest.Server, namespace string) string {
		return ts.URL + "/clusters/test/namespaces/" + namespace + "/pods/_stream"
	}

	t.Run("connection limit", func(t *testing.T) {
		app, ts, _ := newLiveMetricsFixture(t, &config.Config{LiveMaxConnections: 1})
		s := openStream(t, streamURL(ts, "default"), "admission-conn-1")
		s.requireEvent(t, "ro-live", 5*time.Second)
		over := dialStream(t, streamURL(ts, "default"), "admission-conn-2")
		_ = over.Body.Close()
		if over.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("over-capacity status = %d, want 429", over.StatusCode)
		}
		body := scrapeMetrics(t, app)
		requireMetric(t, body, `readout_live_admissions_total{result="accepted"}`, 1)
		requireMetric(t, body, `readout_live_admissions_total{result="connection_limit"}`, 1)
		requireMetric(t, body, `readout_live_admissions_total{result="source_limit"}`, 0)
		requireMetric(t, body, `readout_live_admissions_total{result="cache_limit"}`, 0)
	})

	t.Run("source limit", func(t *testing.T) {
		app, ts, _ := newLiveMetricsFixture(t, &config.Config{LiveMaxSources: 1})
		s := openStream(t, streamURL(ts, "default"), "admission-source-1")
		s.requireEvent(t, "ro-live", 5*time.Second)
		// A second subscriber on the SAME scope joins the admitted source: it
		// is an accepted admission and a reused source, never a reject.
		shared := openStream(t, streamURL(ts, "default"), "admission-source-shared")
		shared.requireEvent(t, "ro-live", 5*time.Second)
		over := dialStream(t, streamURL(ts, "big"), "admission-source-2")
		_ = over.Body.Close()
		if over.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("second-scope status = %d, want 429", over.StatusCode)
		}
		body := scrapeMetrics(t, app)
		requireMetric(t, body, `readout_live_admissions_total{result="accepted"}`, 2)
		requireMetric(t, body, `readout_live_admissions_total{result="source_limit"}`, 1)
		requireMetric(t, body, `readout_live_admissions_total{result="connection_limit"}`, 0)
		requireMetric(t, body, `readout_watchhub_sources_total{result="created"}`, 1)
		requireMetric(t, body, `readout_watchhub_sources_total{result="reused"}`, 1)
	})

	t.Run("cache limit", func(t *testing.T) {
		app, ts, _ := newLiveMetricsFixture(t, &config.Config{LiveMaxCacheAccountedBytes: 1024})
		over := dialStream(t, streamURL(ts, "big"), "admission-cache")
		_ = over.Body.Close()
		if over.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("oversized-scope status = %d, want 429", over.StatusCode)
		}
		body := scrapeMetrics(t, app)
		requireMetric(t, body, `readout_live_admissions_total{result="cache_limit"}`, 1)
		requireMetric(t, body, `readout_live_admissions_total{result="accepted"}`, 0)
		requireMetric(t, body, `readout_watchhub_cache_accounted_bytes_capacity`, 1024)
		// The source measured itself and then failed: both halves are visible.
		if failed, _ := metricValue(t, body, `readout_watchhub_sources_total{result="failed"}`); failed < 1 {
			t.Fatalf("sources_total{result=failed} = %v, want the rejected source counted", failed)
		}
		if bytes, ok := metricValue(t, body, "readout_watchhub_snapshot_bytes_count"); !ok || bytes < 1 {
			t.Fatalf("snapshot_bytes count = %v (present=%v), want the measured LIST", bytes, ok)
		}
	})
}

// TestLiveMetricsGaugesReturnToBaseline pins the leak contract: after the last
// browser disconnects AND the source's retention window expires, every _active
// gauge is back at zero. A source that stayed in the map, a connection slot
// that was never released or accounted bytes that were never dropped would all
// show up here as a gauge stuck above baseline.
func TestLiveMetricsGaugesReturnToBaseline(t *testing.T) {
	clock := newFakeHubClock()
	app, ts, _ := newLiveMetricsFixture(t, nil, func(app *Server) { app.hubClock = clock })
	s := openStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream", "gauge-baseline")
	s.requireEvent(t, "ro-live", 5*time.Second)
	requireMetric(t, scrapeMetrics(t, app), "readout_watchhub_sources_active", 1)

	s.close()
	// Retention is the hub's own 30 seconds, released on the injected clock.
	advanceUntil(t, clock, 10*time.Second, "the live gauges to return to baseline", func() bool {
		body := scrapeMetrics(t, app)
		for _, series := range []string{
			"readout_live_connections_active",
			"readout_watchhub_sources_active",
			"readout_watchhub_subscribers_active",
			"readout_watchhub_cache_accounted_bytes",
		} {
			if value, ok := metricValue(t, body, series); !ok || value != 0 {
				return false
			}
		}
		return true
	})
}

// TestLiveMetricsCountOneTerminalPerSession pins the census property of
// readout_stream_terminal_total: a session that hits its lifetime bound
// contributes exactly ONE sample across the whole reason enum (not one per
// terminal call site), with one session-duration sample beside it.
func TestLiveMetricsCountOneTerminalPerSession(t *testing.T) {
	app, ts, _ := newLiveMetricsFixture(t, nil, func(app *Server) {
		app.streamTuning.maxLifetime = 200 * time.Millisecond
	})
	s := openStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream", "one-terminal")
	s.requireEvent(t, "ro-live", 5*time.Second)
	term := s.requireEvent(t, "ro-live", 3*time.Second)
	if reason := decodeFrame(t, term).Reason; reason != streamTerminalLifetime {
		t.Fatalf("terminal reason = %q, want lifetime", reason)
	}
	s.requireClosed(t, 3*time.Second)

	waitForMetric(t, app, "readout_live_session_duration_seconds_count", 1)
	body := scrapeMetrics(t, app)
	total := 0.0
	for _, reason := range streamTerminalReasons {
		value, ok := metricValue(t, body, `readout_stream_terminal_total{reason="`+reason+`"}`)
		if !ok {
			t.Fatalf("terminal reason %q has no series", reason)
		}
		total += value
	}
	if total != 1 {
		t.Fatalf("terminal samples across every reason = %v, want exactly 1 per session", total)
	}
	requireMetric(t, body, `readout_stream_terminal_total{reason="lifetime"}`, 1)
}

// TestLiveMetricsQuietCheckpointsAreNotFlushSamples pins what
// readout_watchhub_event_to_flush_seconds measures: the delivery latency of a
// CHANGE. Recovery checkpoints re-send the current state on a stream where the
// source published nothing, so measuring them from a source event an interval
// or more old would fill the histogram with samples of the checkpoint period
// and turn its p99 into an alert on a healthy, quiet namespace.
func TestLiveMetricsQuietCheckpointsAreNotFlushSamples(t *testing.T) {
	app, ts, _ := newLiveMetricsFixture(t, nil, func(app *Server) {
		app.streamTuning.checkpointInterval = 50 * time.Millisecond
		app.streamTuning.heartbeat = 0
	})
	s := openStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream", "quiet-checkpoints")
	for i := range 3 {
		frame := decodeFrame(t, s.requireEvent(t, "ro-live", 5*time.Second))
		if frame.Kind != "snapshot" {
			t.Fatalf("frame %d kind = %q, want the initial snapshot and its checkpoint re-sends", i, frame.Kind)
		}
	}
	defer s.close()

	body := scrapeMetrics(t, app)
	requireMetric(t, body, "readout_watchhub_event_to_flush_seconds_count", 1)
}

// TestLiveMetricsRelistCounted pins readout_watchhub_relists_total to the 410
// recovery path: a scripted GONE on the shared watch produces exactly one
// relist regardless of how many subscribers are attached to it.
func TestLiveMetricsRelistCounted(t *testing.T) {
	app, ts, fake := newLiveMetricsFixture(t, nil)
	first := openStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream", "relist-1")
	first.requireEvent(t, "ro-live", 5*time.Second)
	second := openStream(t, ts.URL+"/clusters/test/namespaces/default/pods/_stream", "relist-2")
	second.requireEvent(t, "ro-live", 5*time.Second)
	requireMetric(t, scrapeMetrics(t, app), "readout_watchhub_relists_total", 0)

	waitForOpenWatch(t, fake.URL)
	postStreamScript(t, fake.URL, fmt.Sprintf(`{"events":[{"path":%q,"type":"GONE"}]}`, streamPodsPath))
	first.requireEvent(t, "ro-live", 5*time.Second)
	second.requireEvent(t, "ro-live", 5*time.Second)

	body := scrapeMetrics(t, app)
	requireMetric(t, body, "readout_watchhub_relists_total", 1)
	// The relist republishes the whole scope, so it is a second authoritative
	// snapshot measurement on the one shared source.
	requireMetric(t, body, "readout_watchhub_snapshot_bytes_count", 2)
	requireMetric(t, body, `readout_watchhub_sources_total{result="created"}`, 1)
}

// statusWriter decides what the request counter's `status` label says, and it
// sits under every handler in the process. Its two non-obvious rules -- an
// informational response is not the final status, and a duplicate WriteHeader
// neither re-labels the request nor reaches the connection -- have no other
// coverage.
func TestStatusWriterRecordsExactlyOneFinalStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		write      func(w http.ResponseWriter)
		wantStatus int
		wantBody   string
		wantCodes  []int
	}{
		{
			name:       "an implicit body write is 200",
			write:      func(w http.ResponseWriter) { _, _ = w.Write([]byte("hi")) },
			wantStatus: http.StatusOK,
			wantBody:   "hi",
			wantCodes:  []int{http.StatusOK},
		},
		{
			name: "early hints precede the final status without becoming it",
			write: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusEarlyHints)
				w.WriteHeader(http.StatusNotFound)
			},
			wantStatus: http.StatusNotFound,
			wantCodes:  []int{http.StatusEarlyHints, http.StatusNotFound},
		},
		{
			name: "a duplicate WriteHeader is swallowed, not forwarded",
			write: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusCreated)
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("body"))
			},
			wantStatus: http.StatusCreated,
			wantBody:   "body",
			wantCodes:  []int{http.StatusCreated},
		},
		{
			// The flush itself commits the header at the transport, so nothing
			// is forwarded here -- but the label must still say 200 rather than
			// whatever a later WriteHeader tries to claim.
			name: "a flush with no prior write commits 200",
			write: func(w http.ResponseWriter) {
				_ = http.NewResponseController(w).Flush()
				w.WriteHeader(http.StatusInternalServerError)
			},
			wantStatus: http.StatusOK,
			wantCodes:  nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := &recordingHeaderWriter{ResponseRecorder: httptest.NewRecorder()}
			w := &statusWriter{ResponseWriter: rec, status: http.StatusOK}
			tc.write(w)
			if w.status != tc.wantStatus {
				t.Fatalf("recorded status = %d, want %d", w.status, tc.wantStatus)
			}
			if got := rec.Body.String(); got != tc.wantBody {
				t.Fatalf("body = %q, want %q", got, tc.wantBody)
			}
			if !slices.Equal(rec.codes, tc.wantCodes) {
				t.Fatalf("forwarded status codes = %v, want %v", rec.codes, tc.wantCodes)
			}
		})
	}
}

// recordingHeaderWriter remembers every WriteHeader that actually reached the
// transport, which httptest.ResponseRecorder collapses into one.
type recordingHeaderWriter struct {
	*httptest.ResponseRecorder
	codes []int
}

func (w *recordingHeaderWriter) WriteHeader(status int) {
	w.codes = append(w.codes, status)
	w.ResponseRecorder.WriteHeader(status)
}

func (w *recordingHeaderWriter) Write(p []byte) (int, error) {
	if len(w.codes) == 0 {
		w.codes = append(w.codes, http.StatusOK)
	}
	return w.ResponseRecorder.Write(p)
}
