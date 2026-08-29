package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kbelokon/readout/internal/web/templates"
)

func TestResourceListETagSemanticNormalization(t *testing.T) {
	t.Parallel()
	data := templates.ListData{
		Plural:            "pods",
		ClusterCount:      1,
		TableCount:        1,
		TotalRows:         1,
		DurationSeconds:   0.125,
		ShowStaleBanner:   true,
		AllNamespacesHref: "/clusters/test/namespaces/_all/pods",
		Tables: []templates.TableData{{
			Kind: "Pods",
			Rows: []templates.TableRow{{
				Key:  "test/default/nginx",
				Name: "nginx",
				Cells: []templates.TableCell{{
					Kind:  templates.CellName,
					Value: "nginx",
				}},
			}},
		}},
	}

	base, err := resourceListETag(&data)
	if err != nil {
		t.Fatalf("resourceListETag: %v", err)
	}
	if !strings.HasPrefix(base, `W/"ro-list-v1-`) || !strings.HasSuffix(base, `"`) || strings.Contains(base, "=") {
		t.Fatalf("ETag = %q, want weak base64url validator", base)
	}

	diagnosticOnly := data
	diagnosticOnly.DurationSeconds = 91.75
	diagnosticOnly.ShowStaleBanner = false
	got, err := resourceListETag(&diagnosticOnly)
	if err != nil {
		t.Fatalf("resourceListETag diagnostic variant: %v", err)
	}
	if got != base {
		t.Fatalf("diagnostic-only fields changed ETag: base=%q variant=%q", base, got)
	}

	visibleChange := data
	visibleChange.Tables = append([]templates.TableData(nil), data.Tables...)
	visibleChange.Tables[0].Kind = "Workloads"
	got, err = resourceListETag(&visibleChange)
	if err != nil {
		t.Fatalf("resourceListETag visible variant: %v", err)
	}
	if got == base {
		t.Fatalf("visible ListData change kept ETag %q", base)
	}

	again, err := resourceListETag(&data)
	if err != nil {
		t.Fatalf("resourceListETag repeat: %v", err)
	}
	if again != base {
		t.Fatalf("ETag is not deterministic: first=%q second=%q", base, again)
	}

	renderer := resourceListRendererFingerprint()
	renderer[0] ^= 0xff
	changedRenderer, err := resourceListETagWithRendererFingerprint(&data, renderer)
	if err != nil {
		t.Fatalf("resourceListETag changed renderer: %v", err)
	}
	if changedRenderer == base {
		t.Fatalf("renderer fingerprint change kept ETag %q", base)
	}
}

func TestResourceListETagIgnoresClockOnlyEventAge(t *testing.T) {
	data := liveProjectionFixture(1)
	row := &data.Tables[0].Rows[0]
	row.ResourceVersion = "101"
	row.Cells = append(row.Cells, templates.TableCell{
		Kind: templates.CellEvAge, Value: "4m", Class: "age-new", ColClass: "cell-age", EvAgeRest: "(first 1h ago)", Volatile: true,
	})
	base, err := resourceListETag(&data)
	if err != nil {
		t.Fatal(err)
	}

	clockTick := cloneLiveProjectionFixture(&data)
	clockTick.Tables[0].Rows[0].Cells[2].Value = "5m"
	clockTick.Tables[0].Rows[0].Cells[2].Class = "age-mid"
	clockTick.Tables[0].Rows[0].Cells[2].EvAgeRest = "(first 1h 1m ago)"
	ticked, err := resourceListETag(&clockTick)
	if err != nil {
		t.Fatal(err)
	}
	if ticked != base {
		t.Fatalf("clock-only Event age changed ETag: %q != %q", ticked, base)
	}

	clockTick.Tables[0].Rows[0].ResourceVersion = "102"
	modified, err := resourceListETag(&clockTick)
	if err != nil {
		t.Fatal(err)
	}
	if modified == base {
		t.Fatalf("modified Event resource kept ETag %q", base)
	}
}

func TestIfNoneMatchWeakListGrammar(t *testing.T) {
	t.Parallel()
	const current = `W/"alpha,beta"`
	tests := []struct {
		name   string
		values []string
		want   bool
	}{
		{name: "absent"},
		{name: "empty", values: []string{""}},
		{name: "exact weak", values: []string{current}, want: true},
		{name: "strong spelling weakly matches", values: []string{`"alpha,beta"`}, want: true},
		{name: "match later in list", values: []string{`"miss", W/"alpha,beta"`}, want: true},
		{name: "repeated fields", values: []string{`W/"miss"`, `"alpha,beta"`}, want: true},
		{name: "empty list elements", values: []string{`,, "miss",, W/"alpha,beta",`}, want: true},
		{name: "standalone wildcard", values: []string{"  *\t"}, want: true},
		{name: "no match", values: []string{`W/"miss", "also-miss"`}},
		{name: "matching prefix then malformed fails open", values: []string{`W/"alpha,beta", garbage`}},
		{name: "malformed before match", values: []string{`garbage, W/"alpha,beta"`}},
		{name: "lowercase weak marker", values: []string{`w/"alpha,beta"`}},
		{name: "unterminated", values: []string{`W/"alpha,beta`}},
		{name: "wildcard mixed with list", values: []string{`*, W/"alpha,beta"`}},
		{name: "wildcard in repeated field", values: []string{"*", current}},
		{name: "bare opaque value", values: []string{"alpha,beta"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ifNoneMatch(tc.values, current); got != tc.want {
				t.Fatalf("ifNoneMatch(%q, %q) = %t, want %t", tc.values, current, got, tc.want)
			}
		})
	}
}

func TestResourceListPartialConditionalETag(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Date(2026, time.August, 28, 12, 0, 0, 0, time.UTC))
	const path = "/clusters/test/namespaces/default/pods/_table?sort=Name"

	first := serveListETagRequest(app, http.MethodGet, path, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("first GET status = %d, want 200\nbody=%s", first.Code, first.Body.String())
	}
	etag := first.Header().Get("ETag")
	assertResourceListValidatorHeaders(t, first, etag)
	if etag == "" || !strings.HasPrefix(etag, "W/\"") {
		t.Fatalf("first GET ETag = %q, want weak validator", etag)
	}
	if first.Body.Len() == 0 {
		t.Fatal("first GET returned an empty fragment")
	}

	for _, tc := range []struct {
		name  string
		value string
	}{
		{name: "exact", value: etag},
		{name: "strong spelling", value: strings.TrimPrefix(etag, "W/")},
		{name: "list", value: `W/"miss", ` + etag},
		{name: "wildcard", value: "*"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := http.Header{
				"HX-Request":    {"true"},
				"RO-No-Push":    {"true"},
				"If-None-Match": {tc.value},
			}
			rec := serveListETagRequest(app, http.MethodGet, path, headers)
			assertNotModifiedListResponse(t, rec, etag)
		})
	}

	malformed := serveListETagRequest(app, http.MethodGet, path, http.Header{
		"HX-Request":    {"true"},
		"RO-No-Push":    {"true"},
		"If-None-Match": {etag + ", garbage"},
	})
	if malformed.Code != http.StatusOK || malformed.Body.Len() == 0 {
		t.Fatalf("malformed If-None-Match response = %d/%d bytes, want fail-open 200 with body", malformed.Code, malformed.Body.Len())
	}

	// A matching validator on a USER request must not suppress the body or the
	// canonical history push. The frontend avoids sending this combination; the
	// server gate makes that invariant hold even if a header is injected.
	user := serveListETagRequest(app, http.MethodGet, path, http.Header{
		"HX-Request":    {"true"},
		"If-None-Match": {etag},
	})
	if user.Code != http.StatusOK || user.Body.Len() == 0 {
		t.Fatalf("conditional user request = %d/%d bytes, want 200 with body", user.Code, user.Body.Len())
	}
	if got := user.Header().Get("HX-Push-Url"); got != "/clusters/test/namespaces/default/pods?sort=Name" {
		t.Fatalf("conditional user HX-Push-Url = %q, want canonical list URL", got)
	}

	changed := serveListETagRequest(app, http.MethodGet,
		"/clusters/test/namespaces/default/pods/_table?sort=Status",
		http.Header{
			"HX-Request":    {"true"},
			"RO-No-Push":    {"true"},
			"If-None-Match": {etag},
		})
	if changed.Code != http.StatusOK {
		t.Fatalf("changed representation status = %d, want 200", changed.Code)
	}
	if got := changed.Header().Get("ETag"); got == "" || got == etag {
		t.Fatalf("changed representation ETag = %q, want a new validator (old %q)", got, etag)
	}
}

func TestResourceListPartialETagEncodingAndHead(t *testing.T) {
	app := newServer(t, baseConfig(t), time.Date(2026, time.August, 28, 12, 0, 0, 0, time.UTC))
	const path = "/clusters/test/namespaces/default/pods/_table"

	identity := serveListETagRequest(app, http.MethodGet, path, nil)
	if identity.Code != http.StatusOK {
		t.Fatalf("identity GET status = %d, want 200", identity.Code)
	}
	etag := identity.Header().Get("ETag")

	gzip := serveListETagRequest(app, http.MethodGet, path, http.Header{"Accept-Encoding": {"gzip"}})
	if gzip.Code != http.StatusOK {
		t.Fatalf("gzip GET status = %d, want 200", gzip.Code)
	}
	if got := gzip.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("gzip Content-Encoding = %q, want gzip", got)
	}
	if got := gzip.Header().Get("ETag"); got != etag {
		t.Fatalf("gzip ETag = %q, identity = %q", got, etag)
	}
	assertResourceListValidatorHeaders(t, gzip, etag)

	gzip304 := serveListETagRequest(app, http.MethodGet, path, http.Header{
		"Accept-Encoding": {"gzip"},
		"HX-Request":      {"true"},
		"RO-No-Push":      {"true"},
		"If-None-Match":   {etag},
	})
	assertNotModifiedListResponse(t, gzip304, etag)

	// net/http owns HEAD body suppression. Exercise that protocol seam through
	// a real server rather than expecting ResponseRecorder to emulate it.
	ts := httptest.NewServer(app.Handler())
	t.Cleanup(ts.Close)
	headReq, err := http.NewRequest(http.MethodHead, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	head, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(headReq)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = head.Body.Close() }()
	headBody, err := io.ReadAll(head.Body)
	if err != nil {
		t.Fatal(err)
	}
	if head.StatusCode != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200", head.StatusCode)
	}
	if len(headBody) != 0 {
		t.Fatalf("HEAD body = %d bytes, want empty", len(headBody))
	}
	if got := head.Header.Get("ETag"); got != etag {
		t.Fatalf("HEAD ETag = %q, GET = %q", got, etag)
	}

	head304 := serveListETagRequest(app, http.MethodHead, path, http.Header{
		"HX-Request":    {"true"},
		"RO-No-Push":    {"true"},
		"If-None-Match": {etag},
	})
	assertNotModifiedListResponse(t, head304, etag)
}

func TestResourceListPartialErrorPrecedesConditionalETag(t *testing.T) {
	var forbid atomic.Bool
	api := newToggleableStateAPI(t, &forbid)
	app := newStateServer(t, api.URL)
	const path = "/clusters/test/namespaces/default/pods/_table"

	first := serveListETagRequest(app, http.MethodGet, path, nil)
	if first.Code != http.StatusOK || first.Header().Get("ETag") == "" {
		t.Fatalf("first response = status %d ETag %q, want successful validator", first.Code, first.Header().Get("ETag"))
	}
	forbid.Store(true)

	rec := serveListETagRequest(app, http.MethodGet, path, http.Header{
		"HX-Request":    {"true"},
		"RO-No-Push":    {"true"},
		"If-None-Match": {"*"},
	})
	if rec.Code < 400 || rec.Code == http.StatusNotModified {
		t.Fatalf("whole-list failure status = %d, want real non-2xx error before conditional handling", rec.Code)
	}
	if rec.Header().Get("ETag") != "" {
		t.Fatalf("whole-list failure leaked successful ETag %q", rec.Header().Get("ETag"))
	}
}

func serveListETagRequest(app *Server, method, path string, headers http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for name, values := range headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	return rec
}

func assertResourceListValidatorHeaders(t *testing.T, rec *httptest.ResponseRecorder, etag string) {
	t.Helper()
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want text/html; charset=utf-8", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("Cache-Control = %q, want private, no-store", got)
	}
	if got := rec.Header().Get("ETag"); got != etag {
		t.Fatalf("ETag = %q, want %q", got, etag)
	}
	if values := rec.Header().Values("Vary"); len(values) != 1 || !headerHasToken(values, "Accept-Encoding") {
		t.Fatalf("Vary = %q, want only Accept-Encoding", values)
	}
}

func assertNotModifiedListResponse(t *testing.T, rec *httptest.ResponseRecorder, etag string) {
	t.Helper()
	if rec.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304\nbody=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("304 body = %d bytes, want empty", rec.Body.Len())
	}
	if got := rec.Header().Get("Content-Length"); got != "" {
		t.Fatalf("304 Content-Length = %q, want absent", got)
	}
	assertResourceListValidatorHeaders(t, rec, etag)
}
