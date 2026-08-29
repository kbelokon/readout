package web

import (
	"bufio"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCompressionUsesGzipForLargeDynamicText(t *testing.T) {
	body := strings.Repeat("readout dynamic response\n", 200)
	handler := compressResponses(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("ETag", `"dynamic-v1"`)
		_, _ = io.WriteString(w, body)
	}))
	req := httptest.NewRequest(http.MethodGet, "/clusters", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if !headerHasToken(rec.Header().Values("Vary"), "Accept-Encoding") {
		t.Fatalf("Vary = %q, want Accept-Encoding", rec.Header().Values("Vary"))
	}
	if got := rec.Header().Get("ETag"); got != `W/"dynamic-v1"` {
		t.Fatalf("ETag = %q, want weak validator", got)
	}
	reader, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if string(decoded) != body {
		t.Fatalf("decoded body differs: got %d bytes, want %d", len(decoded), len(body))
	}
}

func TestCompressionHeadMatchesCompressedGetMetadata(t *testing.T) {
	body := strings.Repeat("head representation ", 200)
	ts := httptest.NewServer(compressResponses(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("ETag", `"head-v1"`)
		_, _ = io.WriteString(w, body)
	})))
	t.Cleanup(ts.Close)
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
		defer func() { _ = resp.Body.Close() }()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp, raw
	}

	get, getBody := request(http.MethodGet)
	head, headBody := request(http.MethodHead)
	if len(headBody) != 0 {
		t.Fatalf("HEAD body length = %d, want zero", len(headBody))
	}
	if get.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("GET Content-Encoding = %q, want gzip", get.Header.Get("Content-Encoding"))
	}
	for _, name := range []string{"Content-Type", "Content-Encoding", "Vary", "ETag"} {
		if got, want := strings.Join(head.Header.Values(name), ","), strings.Join(get.Header.Values(name), ","); got != want {
			t.Fatalf("HEAD %s = %q, GET = %q", name, got, want)
		}
	}
	if got, want := head.Header.Get("Content-Length"), strconv.Itoa(len(getBody)); got != want {
		t.Fatalf("HEAD Content-Length = %q, want selected gzip length %q", got, want)
	}
}

func TestCompressionKeepsSmallAndBinaryResponsesIdentity(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
	}{
		{name: "small text", contentType: "text/plain", body: "small"},
		{name: "binary", contentType: "application/octet-stream", body: strings.Repeat("x", minCompressSize+100)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := compressResponses(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.Header().Set("ETag", `"identity-v1"`)
				_, _ = io.WriteString(w, tc.body)
			}))
			req := httptest.NewRequest(http.MethodGet, "/dynamic", nil)
			req.Header.Set("Accept-Encoding", "gzip")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if got := rec.Header().Get("Content-Encoding"); got != "" {
				t.Fatalf("Content-Encoding = %q, want identity", got)
			}
			if got := rec.Body.String(); got != tc.body {
				t.Fatalf("body = %q, want %q", got, tc.body)
			}
			if !headerHasToken(rec.Header().Values("Vary"), "Accept-Encoding") {
				t.Fatalf("Vary = %q, want Accept-Encoding", rec.Header().Values("Vary"))
			}
			if got := rec.Header().Get("ETag"); got != `W/"identity-v1"` {
				t.Fatalf("ETag = %q, want weak validator", got)
			}
		})
	}
}

func TestCompressionBypassesAssets(t *testing.T) {
	body := strings.Repeat("asset", minCompressSize)
	handler := compressResponses(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		w.Header().Set("ETag", `"asset-v1"`)
		_, _ = io.WriteString(w, body)
	}))
	req := httptest.NewRequest(http.MethodGet, "/assets/readout.css", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") != "" || headerHasToken(rec.Header().Values("Vary"), "Accept-Encoding") {
		t.Fatalf("asset compression metadata = encoding %q vary %q", rec.Header().Get("Content-Encoding"), rec.Header().Values("Vary"))
	}
	if got := rec.Header().Get("ETag"); got != `"asset-v1"` {
		t.Fatalf("asset ETag = %q, want untouched", got)
	}
	if rec.Body.String() != body {
		t.Fatal("asset body changed")
	}
}

func TestCompressionBypassesLiveStream(t *testing.T) {
	ts, _ := newStreamFixture(t)
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/clusters/test/namespaces/default/pods/_stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set(streamVersionHeader, "2")
	req.Header.Set(streamGenerationHeader, "compression")
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}, Timeout: 5 * time.Second}
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
	if headerHasToken(resp.Header.Values("Vary"), "Accept-Encoding") {
		t.Fatalf("stream Vary changed: %q", resp.Header.Values("Vary"))
	}

	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read initial flushed frame: %v", err)
		}
		if strings.TrimSpace(line) == "event: ro-live" {
			break
		}
	}
}
