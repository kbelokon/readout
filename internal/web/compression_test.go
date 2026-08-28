package web

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCompressionNegotiatesGzipQuality(t *testing.T) {
	tests := []struct {
		name   string
		header []string
		want   bool
	}{
		{name: "absent", want: false},
		{name: "gzip", header: []string{"gzip"}, want: true},
		{name: "case and positive q", header: []string{"br, GZip; Q=0.25"}, want: true},
		{name: "zero q", header: []string{"br, gzip;q=0"}, want: false},
		{name: "explicit zero beats wildcard", header: []string{"gzip;q=0, *;q=1"}, want: false},
		{name: "wildcard", header: []string{"br;q=1, *;q=0.5"}, want: true},
		{name: "zero wildcard", header: []string{"br, *;q=0"}, want: false},
		{name: "minimum positive thousandth", header: []string{"gzip;q=0.001"}, want: true},
		{name: "one with three zeroes", header: []string{"gzip;q=1.000"}, want: true},
		{name: "one with bare dot", header: []string{"gzip;q=1."}, want: true},
		{name: "zero with bare dot", header: []string{"gzip;q=0."}, want: false},
		{name: "malformed q", header: []string{"gzip;q=wat"}, want: false},
		{name: "nan q", header: []string{"gzip;q=NaN"}, want: false},
		{name: "out of range q", header: []string{"gzip;q=2"}, want: false},
		{name: "one with nonzero fraction", header: []string{"gzip;q=1.001"}, want: false},
		{name: "too many decimal places", header: []string{"gzip;q=0.1234"}, want: false},
		{name: "quoted qvalue", header: []string{`gzip;q="0.5"`}, want: false},
		{name: "scientific qvalue", header: []string{"gzip;q=5e-1"}, want: false},
		{name: "bare q parameter", header: []string{"gzip;q"}, want: false},
		{name: "empty qvalue", header: []string{"gzip;q="}, want: false},
		{name: "space before equals", header: []string{"gzip;q =0.5"}, want: false},
		{name: "space after equals", header: []string{"gzip;q= 0.5"}, want: false},
		{name: "leading decimal point", header: []string{"gzip;q=.5"}, want: false},
		{name: "leading plus", header: []string{"gzip;q=+0.5"}, want: false},
		{name: "unknown parameter", header: []string{"gzip;level=9"}, want: false},
		{name: "unknown parameter after q", header: []string{"gzip;q=0.5;level=9"}, want: false},
		{name: "duplicate q parameter", header: []string{"gzip;q=0.5;q=0.4"}, want: false},
		{name: "malformed explicit blocks wildcard", header: []string{"gzip;level=9, *;q=1"}, want: false},
		{name: "multiple field lines", header: []string{"br", "gzip; q=0.8"}, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := acceptsEncoding(tc.header, "gzip"); got != tc.want {
				t.Fatalf("acceptsEncoding(%q, gzip) = %v, want %v", tc.header, got, tc.want)
			}
		})
	}
}

func TestCompressionGzipsLargeTextAndRepairsHeaders(t *testing.T) {
	body := strings.Repeat("readout table row ", 200)
	h := compressResponses(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("Vary", "HX-Request")
		_, _ = io.WriteString(w, body)
	}))

	req := httptest.NewRequest(http.MethodGet, "/clusters", nil)
	req.Header.Set("Accept-Encoding", "br, gzip;q=0.7")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := rec.Header().Get("Content-Length"); got != "" {
		t.Fatalf("stale Content-Length survived compression: %q", got)
	}
	if !varyContains(rec.Header(), "HX-Request") || !varyContains(rec.Header(), "Accept-Encoding") || !varyContains(rec.Header(), "Cache-Control") {
		t.Fatalf("Vary = %q, want HX-Request, Accept-Encoding, and Cache-Control", rec.Header().Values("Vary"))
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if err := zr.Close(); err != nil {
		t.Fatal(err)
	}
	if string(decoded) != body {
		t.Fatal("gzip body did not round-trip")
	}
}

func TestCompressionHeadMatchesGetNegotiatedMetadata(t *testing.T) {
	body := strings.Repeat("head representation ", 200)
	ts := httptest.NewServer(compressResponses(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("ETag", `"head-v1"`)
		_, _ = io.WriteString(w, body)
	})))
	defer ts.Close()
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}

	for _, tc := range []struct {
		name            string
		acceptEncoding  string
		contentEncoding string
	}{
		{name: "gzip", acceptEncoding: "gzip", contentEncoding: "gzip"},
		{name: "identity", acceptEncoding: "gzip;q=0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := func(method string) (*http.Response, []byte) {
				t.Helper()
				req, err := http.NewRequest(method, ts.URL, nil)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Accept-Encoding", tc.acceptEncoding)
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				raw, readErr := io.ReadAll(resp.Body)
				closeErr := resp.Body.Close()
				if readErr != nil {
					t.Fatal(readErr)
				}
				if closeErr != nil {
					t.Fatal(closeErr)
				}
				return resp, raw
			}

			getResp, getBody := request(http.MethodGet)
			headResp, headBody := request(http.MethodHead)
			if len(headBody) != 0 {
				t.Fatalf("HEAD body length = %d, want zero", len(headBody))
			}
			for _, header := range []string{"Content-Type", "Content-Encoding", "Vary", "ETag"} {
				if got, want := headResp.Header.Values(header), getResp.Header.Values(header); strings.Join(got, ",") != strings.Join(want, ",") {
					t.Fatalf("HEAD %s = %q, GET = %q", header, got, want)
				}
			}
			if got := getResp.Header.Get("Content-Encoding"); got != tc.contentEncoding {
				t.Fatalf("Content-Encoding = %q, want %q", got, tc.contentEncoding)
			}
			selectedLength := strconv.Itoa(len(getBody))
			if got := headResp.Header.Get("Content-Length"); got != selectedLength {
				t.Fatalf("HEAD Content-Length = %q, want selected representation length %q", got, selectedLength)
			}
			if got := getResp.Header.Get("Content-Length"); got != "" && got != selectedLength {
				t.Fatalf("GET Content-Length = %q, want absent or selected representation length %q", got, selectedLength)
			}
			if got := getResp.Header.Get("ETag"); got != `W/"head-v1"` {
				t.Fatalf("ETag = %q, want weak validator", got)
			}
			if !varyContains(getResp.Header, "Accept-Encoding") || !varyContains(getResp.Header, "Cache-Control") {
				t.Fatalf("Vary = %q, want Accept-Encoding and Cache-Control", getResp.Header.Values("Vary"))
			}
			if tc.contentEncoding == "gzip" {
				zr, err := gzip.NewReader(bytes.NewReader(getBody))
				if err != nil {
					t.Fatal(err)
				}
				getBody, err = io.ReadAll(zr)
				if err != nil {
					t.Fatal(err)
				}
				if err := zr.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if string(getBody) != body {
				t.Fatal("GET selected representation did not round-trip")
			}
		})
	}
}

func TestCompressionHeadReportsLargeIdentityLengthWithoutHandlerLength(t *testing.T) {
	body := strings.Repeat("head identity without an explicit length ", 128)
	ts := httptest.NewServer(compressResponses(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, body)
	})))
	defer ts.Close()
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}

	request := func(method string) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, ts.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Accept-Encoding", "gzip;q=0")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, readErr := io.ReadAll(resp.Body)
		closeErr := resp.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
		return resp, raw
	}

	getResp, getBody := request(http.MethodGet)
	headResp, headBody := request(http.MethodHead)
	if len(getBody) <= 2048 {
		t.Fatalf("fixture body length = %d, want larger than historical HTTP/1 auto-length buffer", len(getBody))
	}
	if string(getBody) != body || len(headBody) != 0 {
		t.Fatalf("body lengths/content = GET %d bytes, HEAD %d bytes", len(getBody), len(headBody))
	}
	if got := getResp.Header.Get("Content-Length"); got != "" {
		t.Fatalf("GET Content-Length = %q, want protocol framing to omit it", got)
	}
	if got, want := headResp.Header.Get("Content-Length"), strconv.Itoa(len(getBody)); got != want {
		t.Fatalf("HEAD Content-Length = %q, want exact selected length %q", got, want)
	}
}

func TestCompressionHeadAndGetSuppressAutoLengthForTrailers(t *testing.T) {
	body := "small response with trailers"
	tests := []struct {
		name  string
		setup func(http.Header)
	}{
		{name: "empty Trailer value", setup: func(h http.Header) { h["Trailer"] = []string{""} }},
		{name: "declared trailer", setup: func(h http.Header) { h.Set("Trailer", "X-Readout-Trailer") }},
		{name: "TrailerPrefix key", setup: func(h http.Header) { h[http.TrailerPrefix+"X-Readout-Trailer"] = []string{"complete"} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(compressResponses(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				w.Header().Set("ETag", `"trailer-v1"`)
				tc.setup(w.Header())
				_, _ = io.WriteString(w, body)
			})))
			defer ts.Close()
			client := &http.Client{Transport: &http.Transport{DisableCompression: true}}

			request := func(method string) (*http.Response, []byte) {
				t.Helper()
				req, err := http.NewRequest(method, ts.URL, nil)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Accept-Encoding", "gzip")
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				raw, readErr := io.ReadAll(resp.Body)
				closeErr := resp.Body.Close()
				if readErr != nil {
					t.Fatal(readErr)
				}
				if closeErr != nil {
					t.Fatal(closeErr)
				}
				return resp, raw
			}

			getResp, getBody := request(http.MethodGet)
			headResp, headBody := request(http.MethodHead)
			if string(getBody) != body || len(headBody) != 0 {
				t.Fatalf("body lengths/content = GET %q, HEAD %d bytes", getBody, len(headBody))
			}
			if getResp.Header.Get("Content-Length") != "" || headResp.Header.Get("Content-Length") != "" {
				t.Fatalf("trailer response Content-Length: GET=%q HEAD=%q, want absent", getResp.Header.Get("Content-Length"), headResp.Header.Get("Content-Length"))
			}
			for _, header := range []string{"Content-Type", "Content-Encoding", "Vary", "ETag"} {
				if got, want := headResp.Header.Values(header), getResp.Header.Values(header); strings.Join(got, ",") != strings.Join(want, ",") {
					t.Fatalf("HEAD %s = %q, GET = %q", header, got, want)
				}
			}
		})
	}
}

func TestCompressionIdentityStillVariesWhenGzipIsDeclined(t *testing.T) {
	body := strings.Repeat("x", minCompressSize+100)
	h := compressResponses(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	req := httptest.NewRequest(http.MethodGet, "/search", nil)
	req.Header.Set("Accept-Encoding", "gzip;q=0")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want identity", got)
	}
	if !varyContains(rec.Header(), "Accept-Encoding") || !varyContains(rec.Header(), "Cache-Control") {
		t.Fatalf("Vary = %q, want Accept-Encoding and Cache-Control", rec.Header().Values("Vary"))
	}
	if rec.Body.String() != body {
		t.Fatal("identity body changed")
	}
}

func TestCompressionSmall200AndContentTypeLess304ShareMetadata(t *testing.T) {
	h := compressResponses(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"small-v1"`)
		if r.Header.Get("If-None-Match") != "" {
			// A normal 304 omits representation Content-Type; compression must
			// conservatively repeat the 200 negotiation contract anyway.
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "small body")
	}))

	request := func(ifNoneMatch string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/small", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		if ifNoneMatch != "" {
			req.Header.Set("If-None-Match", ifNoneMatch)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	ok := request("")
	notModified := request(`W/"small-v1"`)
	if ok.Code != http.StatusOK || notModified.Code != http.StatusNotModified {
		t.Fatalf("statuses = (%d, %d), want (200, 304)", ok.Code, notModified.Code)
	}
	if got := ok.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("small 200 Content-Encoding = %q, want identity", got)
	}
	if notModified.Header().Get("Content-Type") != "" {
		t.Fatalf("304 unexpectedly gained Content-Type %q", notModified.Header().Get("Content-Type"))
	}
	for _, rec := range []*httptest.ResponseRecorder{ok, notModified} {
		if !varyContains(rec.Header(), "Accept-Encoding") || !varyContains(rec.Header(), "Cache-Control") {
			t.Fatalf("status %d Vary = %q, want Accept-Encoding and Cache-Control", rec.Code, rec.Header().Values("Vary"))
		}
		if got := rec.Header().Get("ETag"); got != `W/"small-v1"` {
			t.Fatalf("status %d ETag = %q, want weak validator", rec.Code, got)
		}
	}
	if got, want := strings.Join(notModified.Header().Values("Vary"), ","), strings.Join(ok.Header().Values("Vary"), ","); got != want {
		t.Fatalf("304 Vary = %q, want same cache selectors as small 200 %q", got, want)
	}
}

func TestCompressionSkipsUnsafeResponses(t *testing.T) {
	large := strings.Repeat("payload ", 300)
	tests := []struct {
		name        string
		method      string
		path        string
		status      int
		contentType string
		setup       func(http.Header)
		wantVary    bool
	}{
		{name: "small", method: http.MethodGet, path: "/clusters", contentType: "text/html", wantVary: true},
		{name: "sse content type", method: http.MethodGet, path: "/events", contentType: "text/event-stream"},
		{name: "multipart", method: http.MethodGet, path: "/parts", contentType: "multipart/mixed; boundary=readout"},
		{name: "binary", method: http.MethodGet, path: "/binary", contentType: "application/octet-stream"},
		{name: "no content", method: http.MethodGet, path: "/empty", status: http.StatusNoContent, contentType: "text/plain"},
		{name: "not modified", method: http.MethodGet, path: "/cached", status: http.StatusNotModified, contentType: "text/plain", wantVary: true},
		{name: "partial content", method: http.MethodGet, path: "/partial", status: http.StatusPartialContent, contentType: "text/plain"},
		{name: "already encoded", method: http.MethodGet, path: "/encoded", contentType: "text/plain", setup: func(h http.Header) { h.Set("Content-Encoding", "br") }},
		{name: "response no-transform", method: http.MethodGet, path: "/private", contentType: "text/plain", setup: func(h http.Header) { h.Set("Cache-Control", "private, no-transform") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload := large
			if tc.name == "small" {
				payload = "short"
			}
			h := compressResponses(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				if tc.setup != nil {
					tc.setup(w.Header())
				}
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
				_, _ = io.WriteString(w, payload)
			}))
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Header.Set("Accept-Encoding", "gzip")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if got := rec.Header().Get("Content-Encoding"); got == "gzip" {
				t.Fatal("response was unexpectedly gzip-compressed")
			}
			if tc.wantVary {
				if !varyContains(rec.Header(), "Accept-Encoding") || !varyContains(rec.Header(), "Cache-Control") {
					t.Fatalf("Vary = %q, want Accept-Encoding and Cache-Control", rec.Header().Values("Vary"))
				}
			} else if varyContains(rec.Header(), "Accept-Encoding") || varyContains(rec.Header(), "Cache-Control") {
				t.Fatalf("ineligible response unexpectedly advertises compression selectors: %q", rec.Header().Values("Vary"))
			}
		})
	}
}

func TestCompressionPreservesBodylessStatusSemantics(t *testing.T) {
	body := strings.Repeat("status payload ", 200)

	for _, status := range []int{http.StatusNoContent, http.StatusResetContent, http.StatusNotModified} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var writeN int
			var writeErr error
			h := compressResponses(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.Header().Set("ETag", `"status-v1"`)
				// Every bodyless status must discard this deliberately invalid
				// non-zero value; for 304 it is the stale identity length.
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				w.WriteHeader(status)
				writeN, writeErr = io.WriteString(w, body)
			}))
			req := httptest.NewRequest(http.MethodGet, "/bodyless", nil)
			req.Header.Set("Accept-Encoding", "gzip")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != status {
				t.Fatalf("status = %d, want %d", rec.Code, status)
			}
			if got := rec.Header().Get("Content-Length"); got != "" {
				t.Fatalf("Content-Length = %q, want absent", got)
			}
			if rec.Body.Len() != 0 {
				t.Fatalf("body length = %d, want zero", rec.Body.Len())
			}
			if writeN != 0 || !errors.Is(writeErr, http.ErrBodyNotAllowed) {
				t.Fatalf("bodyless Write = (%d, %v), want (0, http.ErrBodyNotAllowed)", writeN, writeErr)
			}
			if rec.Header().Get("Content-Encoding") != "" {
				t.Fatalf("bodyless Content-Encoding = %q, want absent", rec.Header().Get("Content-Encoding"))
			}
			if status == http.StatusNotModified {
				if !varyContains(rec.Header(), "Accept-Encoding") || !varyContains(rec.Header(), "Cache-Control") {
					t.Fatalf("304 Vary = %q, want Accept-Encoding and Cache-Control", rec.Header().Values("Vary"))
				}
				if got := rec.Header().Get("ETag"); got != `W/"status-v1"` {
					t.Fatalf("304 ETag = %q, want weak validator", got)
				}
			} else {
				if varyContains(rec.Header(), "Accept-Encoding") {
					t.Fatalf("%d unexpectedly varies by Accept-Encoding: %q", status, rec.Header().Values("Vary"))
				}
				if got := rec.Header().Get("ETag"); got != `"status-v1"` {
					t.Fatalf("%d ETag = %q, want unchanged", status, got)
				}
			}
		})
	}

	t.Run("error status survives gzip", func(t *testing.T) {
		h := compressResponses(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, body)
		}))
		req := httptest.NewRequest(http.MethodGet, "/failed", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
		}
		if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
			t.Fatalf("Content-Encoding = %q, want gzip", got)
		}
	})
}

func TestCompressionRespectsRequestNoTransform(t *testing.T) {
	body := strings.Repeat("payload ", 300)
	h := compressResponses(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("ETag", `"no-transform-v1"`)
		_, _ = io.WriteString(w, body)
	}))
	request := func(cacheControl string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/clusters", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		if cacheControl != "" {
			req.Header.Set("Cache-Control", cacheControl)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	ordinary := request("")
	noTransform := request("max-age=0, no-transform")
	if got := ordinary.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("ordinary Content-Encoding = %q, want gzip", got)
	}
	if got := noTransform.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want identity", got)
	}
	if noTransform.Body.String() != body {
		t.Fatal("no-transform response body changed")
	}
	for _, rec := range []*httptest.ResponseRecorder{ordinary, noTransform} {
		if !varyContains(rec.Header(), "Accept-Encoding") || !varyContains(rec.Header(), "Cache-Control") {
			t.Fatalf("Content-Encoding %q Vary = %q, want Accept-Encoding and Cache-Control", rec.Header().Get("Content-Encoding"), rec.Header().Values("Vary"))
		}
		if got := rec.Header().Get("ETag"); got != `W/"no-transform-v1"` {
			t.Fatalf("Content-Encoding %q ETag = %q, want weak validator", rec.Header().Get("Content-Encoding"), got)
		}
	}
	if got, want := strings.Join(noTransform.Header().Values("Vary"), ","), strings.Join(ordinary.Header().Values("Vary"), ","); got != want {
		t.Fatalf("no-transform Vary = %q, want same cache selectors as ordinary gzip %q", got, want)
	}
}

func TestCompressionWeakensStrongETagForGzipAndIdentity(t *testing.T) {
	body := strings.Repeat("etag representation ", 200)
	for _, tc := range []struct {
		name            string
		acceptEncoding  string
		contentEncoding string
	}{
		{name: "gzip", acceptEncoding: "gzip", contentEncoding: "gzip"},
		{name: "identity", acceptEncoding: "gzip;q=0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := compressResponses(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				w.Header().Set("ETag", `"representation-v1"`)
				_, _ = io.WriteString(w, body)
			}))
			req := httptest.NewRequest(http.MethodGet, "/etag", nil)
			req.Header.Set("Accept-Encoding", tc.acceptEncoding)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if got := rec.Header().Get("Content-Encoding"); got != tc.contentEncoding {
				t.Fatalf("Content-Encoding = %q, want %q", got, tc.contentEncoding)
			}
			if got := rec.Header().Get("ETag"); got != `W/"representation-v1"` {
				t.Fatalf("ETag = %q, want weak validator", got)
			}
			if !varyContains(rec.Header(), "Accept-Encoding") || !varyContains(rec.Header(), "Cache-Control") {
				t.Fatalf("Vary = %q, want Accept-Encoding and Cache-Control", rec.Header().Values("Vary"))
			}
		})
	}
}

func TestCompressionSingleLargeWriteCapsBufferAndStreamsRemainder(t *testing.T) {
	body := bytes.Repeat([]byte("large-single-write-"), 16_384)
	rec := httptest.NewRecorder()
	cw := &compressionWriter{ResponseWriter: rec, acceptsGzip: true}
	cw.Header().Set("Content-Type", "text/plain")
	if n, err := cw.Write(body); err != nil || n != len(body) {
		t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len(body))
	}
	if got := len(cw.buffer); got != 0 {
		t.Fatalf("buffer length after threshold commit = %d, want zero", got)
	}
	if got := cap(cw.buffer); got > minCompressSize {
		t.Fatalf("buffer capacity = %d, exceeds %d-byte invariant", got, minCompressSize)
	}
	if err := cw.finish(); err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if err := zr.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, body) {
		t.Fatal("large single Write did not round-trip")
	}
}

func TestCompressionThresholdPartialWriteReportsCurrentAcceptedBytes(t *testing.T) {
	wantErr := errors.New("partial downstream failure")
	downstream := &partialErrorResponseWriter{
		header: make(http.Header),
		accept: 900,
		err:    wantErr,
	}
	cw := &compressionWriter{ResponseWriter: downstream}
	cw.Header().Set("Content-Type", "text/plain")
	first := bytes.Repeat([]byte{'a'}, 800)
	if n, err := cw.Write(first); err != nil || n != len(first) {
		t.Fatalf("first Write = (%d, %v), want (%d, nil)", n, err, len(first))
	}
	crossing := bytes.Repeat([]byte{'b'}, 400)
	n, err := cw.Write(crossing)
	// The threshold buffer was 800 prior bytes + 224 current bytes. The
	// downstream accepted 900 total, so exactly 100 bytes belong to this call.
	if n != 100 || !errors.Is(err, wantErr) {
		t.Fatalf("crossing Write = (%d, %v), want (100, %v)", n, err, wantErr)
	}
	if downstream.calls != 1 {
		t.Fatalf("downstream calls = %d, want 1", downstream.calls)
	}
	wantAccepted := append(append([]byte(nil), first...), crossing[:100]...)
	if !bytes.Equal(downstream.accepted, wantAccepted) {
		t.Fatalf("accepted bytes length/content = %d, want %d without duplication", len(downstream.accepted), len(wantAccepted))
	}
	if retryN, retryErr := cw.Write(crossing[n:]); retryN != 0 || !errors.Is(retryErr, wantErr) {
		t.Fatalf("retry Write = (%d, %v), want sticky (0, %v)", retryN, retryErr, wantErr)
	}
	if err := cw.finish(); !errors.Is(err, wantErr) {
		t.Fatalf("finish error = %v, want sticky %v", err, wantErr)
	}
	if downstream.calls != 1 {
		t.Fatalf("downstream retried after sticky error: %d calls", downstream.calls)
	}
}

func TestCompressionNormalizesShortWrites(t *testing.T) {
	t.Run("already committed", func(t *testing.T) {
		downstream := &partialErrorResponseWriter{header: make(http.Header), accept: 1}
		cw := &compressionWriter{ResponseWriter: downstream, committed: true}
		n, err := cw.Write([]byte("abc"))
		if n != 1 || !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("Write = (%d, %v), want (1, io.ErrShortWrite)", n, err)
		}
		if retryN, retryErr := cw.Write([]byte("bc")); retryN != 0 || !errors.Is(retryErr, io.ErrShortWrite) {
			t.Fatalf("retry Write = (%d, %v), want sticky (0, io.ErrShortWrite)", retryN, retryErr)
		}
	})

	t.Run("threshold commit", func(t *testing.T) {
		downstream := &partialErrorResponseWriter{header: make(http.Header), accept: 900}
		cw := &compressionWriter{ResponseWriter: downstream}
		cw.Header().Set("Content-Type", "text/plain")
		if n, err := cw.Write(bytes.Repeat([]byte{'a'}, 800)); n != 800 || err != nil {
			t.Fatalf("first Write = (%d, %v), want (800, nil)", n, err)
		}
		n, err := cw.Write(bytes.Repeat([]byte{'b'}, 400))
		if n != 100 || !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("crossing Write = (%d, %v), want (100, io.ErrShortWrite)", n, err)
		}
	})

	t.Run("streamed remainder", func(t *testing.T) {
		downstream := &sequenceResponseWriter{
			header:  make(http.Header),
			accepts: []int{minCompressSize, 50},
		}
		cw := &compressionWriter{ResponseWriter: downstream}
		cw.Header().Set("Content-Type", "text/plain")
		body := bytes.Repeat([]byte{'x'}, minCompressSize+100)
		n, err := cw.Write(body)
		if n != minCompressSize+50 || !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("Write = (%d, %v), want (%d, io.ErrShortWrite)", n, err, minCompressSize+50)
		}
		if downstream.calls != 2 {
			t.Fatalf("downstream calls = %d, want threshold + remainder writes", downstream.calls)
		}
	})
}

func TestCompressionBodylessStatusesOnRealServer(t *testing.T) {
	body := strings.Repeat("must not be sent ", 100)
	for _, status := range []int{http.StatusNoContent, http.StatusResetContent, http.StatusNotModified} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			type writeResult struct {
				n   int
				err error
			}
			writes := make(chan writeResult, 1)
			ts := httptest.NewServer(compressResponses(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				w.Header().Set("ETag", `"real-server-v1"`)
				w.WriteHeader(status)
				n, err := io.WriteString(w, body)
				writes <- writeResult{n: n, err: err}
			})))
			defer ts.Close()

			req, err := http.NewRequest(http.MethodGet, ts.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Accept-Encoding", "gzip")
			client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			raw, readErr := io.ReadAll(resp.Body)
			closeErr := resp.Body.Close()
			if readErr != nil {
				t.Fatal(readErr)
			}
			if closeErr != nil {
				t.Fatal(closeErr)
			}
			if resp.StatusCode != status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, status)
			}
			if len(raw) != 0 {
				t.Fatalf("body length = %d, want zero", len(raw))
			}
			result := <-writes
			if result.n != 0 || !errors.Is(result.err, http.ErrBodyNotAllowed) {
				t.Fatalf("bodyless Write = (%d, %v), want (0, http.ErrBodyNotAllowed)", result.n, result.err)
			}
			wantLength := ""
			if status == http.StatusResetContent {
				// net/http emits the canonical zero length required for a 205.
				wantLength = "0"
			}
			if got := resp.Header.Get("Content-Length"); got != wantLength {
				t.Fatalf("Content-Length = %q, want %q", got, wantLength)
			}
			if status == http.StatusNotModified {
				if got := resp.Header.Get("ETag"); got != `W/"real-server-v1"` {
					t.Fatalf("ETag = %q, want weak validator", got)
				}
				if !varyContains(resp.Header, "Accept-Encoding") || !varyContains(resp.Header, "Cache-Control") {
					t.Fatalf("Vary = %q, want Accept-Encoding and Cache-Control", resp.Header.Values("Vary"))
				}
			}
		})
	}
}

func TestCompressionFinishReturnsWriteErrorWithoutStatusRewrite(t *testing.T) {
	wantErr := errors.New("downstream write failed")
	downstream := &errorResponseWriter{header: make(http.Header), err: wantErr}
	cw := &compressionWriter{ResponseWriter: downstream}
	cw.Header().Set("Content-Type", "text/plain")
	if n, err := io.WriteString(cw, "buffered"); err != nil || n != len("buffered") {
		t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len("buffered"))
	}
	if err := cw.finish(); !errors.Is(err, wantErr) {
		t.Fatalf("finish error = %v, want %v", err, wantErr)
	}
	if len(downstream.statuses) != 1 || downstream.statuses[0] != http.StatusOK {
		t.Fatalf("downstream statuses = %v, want one 200 with no error rewrite", downstream.statuses)
	}
}

func TestCompressionFinalizationCompletesBeforeMetricsObservation(t *testing.T) {
	s := &Server{metrics: newAppMetrics()}
	s.cfg.NoAccessLogs = true
	downstream := &blockingResponseWriter{
		header:  make(http.Header),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	h := s.observeMetrics(compressResponses(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "buffered until finish")
	})))
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(downstream, httptest.NewRequest(http.MethodGet, "/order", nil))
		close(done)
	}()

	select {
	case <-downstream.started:
	case <-time.After(2 * time.Second):
		t.Fatal("compression finalization never reached downstream Write")
	}
	if got := requestMetricValue(t, s.metrics, http.MethodGet, "__unmatched__", "200"); got != 0 {
		t.Fatalf("request metric recorded before compression finish: %v", got)
	}
	close(downstream.release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("middleware did not return after compression finish")
	}
	if got := requestMetricValue(t, s.metrics, http.MethodGet, "__unmatched__", "200"); got != 1 {
		t.Fatalf("request metric after compression finish = %v, want 1", got)
	}
}

func TestStatusWriterRetainsFirstFinalStatus(t *testing.T) {
	t.Run("informational then final", func(t *testing.T) {
		downstream := &captureResponseWriter{header: make(http.Header)}
		sw := &statusWriter{ResponseWriter: downstream, status: http.StatusOK}
		sw.WriteHeader(http.StatusEarlyHints)
		sw.WriteHeader(http.StatusCreated)
		sw.WriteHeader(http.StatusBadGateway)
		sw.WriteHeader(http.StatusContinue)
		if sw.status != http.StatusCreated {
			t.Fatalf("metric status = %d, want %d", sw.status, http.StatusCreated)
		}
		if len(downstream.statuses) != 2 || downstream.statuses[0] != http.StatusEarlyHints || downstream.statuses[1] != http.StatusCreated {
			t.Fatalf("downstream statuses = %v, want [103 201]", downstream.statuses)
		}
	})

	t.Run("implicit success is final", func(t *testing.T) {
		downstream := &captureResponseWriter{header: make(http.Header)}
		sw := &statusWriter{ResponseWriter: downstream, status: http.StatusOK}
		_, _ = io.WriteString(sw, "body")
		sw.WriteHeader(http.StatusInternalServerError)
		if sw.status != http.StatusOK {
			t.Fatalf("metric status = %d, want 200", sw.status)
		}
		if len(downstream.statuses) != 0 {
			t.Fatalf("late status reached downstream: %v", downstream.statuses)
		}
	})

	t.Run("switching protocols is final", func(t *testing.T) {
		downstream := &captureResponseWriter{header: make(http.Header)}
		sw := &statusWriter{ResponseWriter: downstream, status: http.StatusOK}
		sw.WriteHeader(http.StatusSwitchingProtocols)
		sw.WriteHeader(http.StatusOK)
		if sw.status != http.StatusSwitchingProtocols {
			t.Fatalf("metric status = %d, want 101", sw.status)
		}
		if len(downstream.statuses) != 1 || downstream.statuses[0] != http.StatusSwitchingProtocols {
			t.Fatalf("downstream statuses = %v, want [101]", downstream.statuses)
		}
	})
}

func TestStatusWriterFlushErrorReachesUnderlying(t *testing.T) {
	for _, viaController := range []bool{false, true} {
		name := "direct"
		if viaController {
			name = "ResponseController"
		}
		t.Run(name, func(t *testing.T) {
			wantErr := errors.New("flush transport failed")
			downstream := &flushErrorResponseWriter{
				captureResponseWriter: captureResponseWriter{header: make(http.Header)},
				err:                   wantErr,
			}
			sw := &statusWriter{ResponseWriter: downstream, status: http.StatusOK}
			var err error
			if viaController {
				err = http.NewResponseController(sw).Flush()
			} else {
				err = sw.FlushError()
			}
			if !errors.Is(err, wantErr) {
				t.Fatalf("Flush error = %v, want %v", err, wantErr)
			}
			if downstream.flushes != 1 {
				t.Fatalf("underlying FlushError calls = %d, want 1", downstream.flushes)
			}
			if !sw.wroteFinal || sw.status != http.StatusOK {
				t.Fatalf("implicit status = (%v, %d), want (true, 200)", sw.wroteFinal, sw.status)
			}
			if sw.Unwrap() != downstream {
				t.Fatal("Unwrap no longer exposes the underlying writer")
			}
		})
	}
}

func TestCompressionFlushCommitsIdentityAndUnwraps(t *testing.T) {
	rec := httptest.NewRecorder()
	cw := &compressionWriter{ResponseWriter: rec, acceptsGzip: true}
	cw.Header().Set("Content-Type", "text/plain")
	_, _ = io.WriteString(cw, strings.Repeat("x", minCompressSize-1))
	if err := http.NewResponseController(cw).Flush(); err != nil {
		t.Fatalf("ResponseController.Flush: %v", err)
	}
	if !rec.Flushed {
		t.Fatal("Flush did not reach the underlying writer")
	}
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("flushed response Content-Encoding = %q, want identity", got)
	}
	if cw.Unwrap() != rec {
		t.Fatal("Unwrap must expose the underlying writer")
	}
}

func TestCompressionFlushPinsImplicit200AgainstLateBodylessStatus(t *testing.T) {
	for _, lateStatus := range []int{http.StatusNoContent, http.StatusResetContent, http.StatusNotModified} {
		t.Run(http.StatusText(lateStatus), func(t *testing.T) {
			rec := httptest.NewRecorder()
			metricsWriter := &statusWriter{ResponseWriter: rec, status: http.StatusOK}
			cw := &compressionWriter{ResponseWriter: metricsWriter, acceptsGzip: true}
			if err := http.NewResponseController(cw).Flush(); err != nil {
				t.Fatalf("initial Flush: %v", err)
			}

			cw.WriteHeader(lateStatus)
			body := "body after committed implicit success"
			if n, err := io.WriteString(cw, body); err != nil || n != len(body) {
				t.Fatalf("Write after late %d = (%d, %v), want (%d, nil)", lateStatus, n, err, len(body))
			}
			if err := cw.finish(); err != nil {
				t.Fatal(err)
			}

			if rec.Code != http.StatusOK || cw.status != http.StatusOK || metricsWriter.status != http.StatusOK {
				t.Fatalf("wire/compression/metrics statuses = (%d, %d, %d), want all 200", rec.Code, cw.status, metricsWriter.status)
			}
			if !cw.wroteHeader || !metricsWriter.wroteFinal {
				t.Fatalf("final-state flags = compression %v, metrics %v, want true", cw.wroteHeader, metricsWriter.wroteFinal)
			}
			if got := rec.Body.String(); got != body {
				t.Fatalf("body = %q, want %q", got, body)
			}
		})
	}
}

// TestCompressionBypassesLiveStream proves the installed app middleware leaves
// the real /_stream handshake and per-frame Flush path untouched even when the
// client explicitly asks for gzip.
func TestCompressionBypassesLiveStream(t *testing.T) {
	ts, _ := newStreamFixture(t)
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/clusters/test/namespaces/default/pods/_stream?g=compression", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept-Encoding", "gzip")
	client := &http.Client{
		Transport: &http.Transport{DisableCompression: true},
		Timeout:   5 * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	for header, want := range map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-store",
		"X-Accel-Buffering": "no",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Fatalf("%s = %q, want %q", header, got, want)
		}
	}
	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("stream Content-Encoding = %q, want absent", got)
	}
	if varyContains(resp.Header, "Accept-Encoding") {
		t.Fatalf("stream Vary changed: %q", resp.Header.Values("Vary"))
	}

	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read initial flushed frame: %v", err)
		}
		if strings.TrimSpace(line) == "event: ro-table" {
			break
		}
	}
}

func varyContains(header http.Header, token string) bool {
	for _, value := range header.Values("Vary") {
		for _, item := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(item), token) {
				return true
			}
		}
	}
	return false
}

func requestMetricValue(t *testing.T, metrics *appMetrics, method, path, status string) float64 {
	t.Helper()
	families, err := metrics.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "readout_http_requests_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := make(map[string]string, len(metric.GetLabel()))
			for _, label := range metric.GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}
			if labels["method"] == method && labels["path"] == path && labels["status"] == status {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}

type errorResponseWriter struct {
	header   http.Header
	statuses []int
	err      error
}

func (w *errorResponseWriter) Header() http.Header { return w.header }

func (w *errorResponseWriter) WriteHeader(status int) {
	w.statuses = append(w.statuses, status)
}

func (w *errorResponseWriter) Write([]byte) (int, error) { return 0, w.err }

type partialErrorResponseWriter struct {
	header   http.Header
	accept   int
	err      error
	calls    int
	accepted []byte
}

type sequenceResponseWriter struct {
	header  http.Header
	accepts []int
	calls   int
}

func (w *sequenceResponseWriter) Header() http.Header { return w.header }

func (w *sequenceResponseWriter) WriteHeader(int) {}

func (w *sequenceResponseWriter) Write(p []byte) (int, error) {
	index := w.calls
	w.calls++
	if index >= len(w.accepts) {
		return len(p), nil
	}
	return min(w.accepts[index], len(p)), nil
}

func (w *partialErrorResponseWriter) Header() http.Header { return w.header }

func (w *partialErrorResponseWriter) WriteHeader(int) {}

func (w *partialErrorResponseWriter) Write(p []byte) (int, error) {
	w.calls++
	n := min(w.accept, len(p))
	w.accepted = append(w.accepted, p[:n]...)
	return n, w.err
}

type blockingResponseWriter struct {
	header  http.Header
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockingResponseWriter) Header() http.Header { return w.header }

func (w *blockingResponseWriter) WriteHeader(int) {}

func (w *blockingResponseWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return len(p), nil
}

type captureResponseWriter struct {
	header   http.Header
	statuses []int
	body     strings.Builder
}

func (w *captureResponseWriter) Header() http.Header { return w.header }

func (w *captureResponseWriter) WriteHeader(status int) {
	w.statuses = append(w.statuses, status)
}

func (w *captureResponseWriter) Write(p []byte) (int, error) {
	return w.body.Write(p)
}

type flushErrorResponseWriter struct {
	captureResponseWriter
	err     error
	flushes int
}

func (w *flushErrorResponseWriter) FlushError() error {
	w.flushes++
	return w.err
}
