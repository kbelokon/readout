package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kbelokon/readout/internal/config"
	"github.com/kbelokon/readout/tests/unit/fakeapi"
)

// TestDomainMetricsScrape exercises the three domain-metric boundaries against
// the fakeapi harness — a kube list (through a list page), a stream terminal,
// and a hook call (the authorization hook in headers auth mode) — then scrapes
// /metrics and asserts each series family is present with its expected labels.
// It is the end-to-end proof that the observer wiring reaches the registry.
func TestDomainMetricsScrape(t *testing.T) {
	setStreamVar(t, &streamIdleCap, 200*time.Millisecond)

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

	// 2) A stream driven to its idle terminal: increments the terminal counter.
	streamReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/clusters/test/namespaces/default/pods/_stream?g=1", nil)
	streamReq.Header.Set("X-Forwarded-User", "alice")
	s := openStreamRequest(t, streamReq)
	s.requireEvent(t, "ro-table", 5*time.Second)
	term := s.requireEvent(t, "ro-terminal", 3*time.Second)
	if reason := decodeFrame(t, term).Reason; reason != "idle" {
		t.Fatalf("terminal reason = %q, want idle", reason)
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
		// stream terminal counter for the idle reason.
		`readout_stream_terminal_total{reason="idle"}`,
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
