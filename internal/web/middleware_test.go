package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kbelokon/readout/internal/config"
)

// TestSameOriginRejectsCrossSitePost proves the stateless CSRF guard: a non-GET
// request carrying an Origin whose host differs from the request Host (a
// cross-site form post) is rejected with 403 and never reaches the downstream
// handler.
func TestSameOriginRejectsCrossSitePost(t *testing.T) {
	s := &Server{cfg: config.Config{}}
	h := s.sameOrigin(okNext())

	req := httptest.NewRequest(http.MethodPost, "http://readout.example/preferences", nil)
	req.Host = "readout.example"
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site POST: status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

// TestSameOriginAllowsSameOriginPost proves a non-GET request whose Origin host
// equals the request Host passes the guard and reaches the handler (200).
func TestSameOriginAllowsSameOriginPost(t *testing.T) {
	s := &Server{cfg: config.Config{}}
	h := s.sameOrigin(okNext())

	req := httptest.NewRequest(http.MethodPost, "http://readout.example/preferences", nil)
	req.Host = "readout.example"
	req.Header.Set("Origin", "http://readout.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("same-origin POST: status = %d, want 200", rec.Code)
	}
}

// TestSameOriginAllowsPublicURLOrigin proves the guard accepts an Origin that
// matches the configured publicUrl host even when it differs from the request
// Host (readout behind a proxy that rewrites Host).
func TestSameOriginAllowsPublicURLOrigin(t *testing.T) {
	s := &Server{cfg: config.Config{PublicURL: "https://readout.public.test"}}
	h := s.sameOrigin(okNext())

	req := httptest.NewRequest(http.MethodPost, "http://internal.svc/preferences", nil)
	req.Host = "internal.svc"
	req.Header.Set("Origin", "https://readout.public.test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("publicUrl-origin POST: status = %d, want 200", rec.Code)
	}
}

// TestSameOriginGetPassthrough proves the guard is method-based: GET and HEAD
// requests pass without any Origin/Sec-Fetch-Site inspection.
func TestSameOriginGetPassthrough(t *testing.T) {
	s := &Server{cfg: config.Config{}}
	h := s.sameOrigin(okNext())

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		req := httptest.NewRequest(method, "http://readout.example/", nil)
		req.Host = "readout.example"
		req.Header.Set("Origin", "https://evil.example")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s passthrough: status = %d, want 200", method, rec.Code)
		}
	}
}

// TestSameOriginSecFetchFallback proves the Sec-Fetch-Site fallback when Origin
// is absent: cross-site is rejected; same-origin/same-site/none are admitted.
func TestSameOriginSecFetchFallback(t *testing.T) {
	s := &Server{cfg: config.Config{}}
	h := s.sameOrigin(okNext())

	cases := []struct {
		site string
		want int
	}{
		{"cross-site", http.StatusForbidden},
		{"same-origin", http.StatusOK},
		{"same-site", http.StatusOK},
		{"none", http.StatusOK},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodPost, "http://readout.example/preferences", nil)
		req.Host = "readout.example"
		req.Header.Set("Sec-Fetch-Site", tc.site)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("Sec-Fetch-Site %q: status = %d, want %d", tc.site, rec.Code, tc.want)
		}
	}
}

// TestSameOriginRefererFallback proves the Referer fallback when neither Origin
// nor Sec-Fetch-Site is present: a cross-site Referer rejects, a same-host
// Referer admits.
func TestSameOriginRefererFallback(t *testing.T) {
	s := &Server{cfg: config.Config{}}
	h := s.sameOrigin(okNext())

	cross := httptest.NewRequest(http.MethodPost, "http://readout.example/preferences", nil)
	cross.Host = "readout.example"
	cross.Header.Set("Referer", "https://evil.example/page")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, cross)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site Referer: status = %d, want %d", rec.Code, http.StatusForbidden)
	}

	same := httptest.NewRequest(http.MethodPost, "http://readout.example/preferences", nil)
	same.Host = "readout.example"
	same.Header.Set("Referer", "http://readout.example/preferences")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, same)
	if rec.Code != http.StatusOK {
		t.Fatalf("same-host Referer: status = %d, want 200", rec.Code)
	}
}

// TestSameOriginNoSignalAllowed proves the older-browser fallback: a non-GET
// request with no Origin, Sec-Fetch-Site, or Referer is admitted (SameSite=Lax
// is the only remaining gate — the annoyance-grade gap the unit accepts).
func TestSameOriginNoSignalAllowed(t *testing.T) {
	s := &Server{cfg: config.Config{}}
	h := s.sameOrigin(okNext())

	req := httptest.NewRequest(http.MethodPost, "http://readout.example/preferences", nil)
	req.Host = "readout.example"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("no-signal POST: status = %d, want 200", rec.Code)
	}
}
