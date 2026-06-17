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

// TestSameOriginNullOriginFallsThrough proves the opaque-origin case: a browser
// sends Origin: null on a same-origin top-level form POST when the page carries
// Referrer-Policy: no-referrer (which readout sets; Firefox ties Origin to the
// referrer policy). "null" has no host to match, so the guard must treat it as
// no usable Origin and fall through to the unspoofable Sec-Fetch-Site signal --
// admitting the genuine same-origin POST while still rejecting a cross-site one.
func TestSameOriginNullOriginFallsThrough(t *testing.T) {
	s := &Server{cfg: config.Config{}}
	h := s.sameOrigin(okNext())

	cases := []struct {
		site string
		want int
	}{
		{"same-origin", http.StatusOK},
		{"cross-site", http.StatusForbidden},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodPost, "http://readout.example/preferences", nil)
		req.Host = "readout.example"
		req.Header.Set("Origin", "null")
		req.Header.Set("Sec-Fetch-Site", tc.site)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("Origin null + Sec-Fetch-Site %q: status = %d, want %d", tc.site, rec.Code, tc.want)
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

// TestSecurityHeadersUnconditional proves the defense-in-depth headers the CSP
// does not cover are set on every response: Referrer-Policy, Permissions-Policy,
// Cross-Origin-Opener-Policy, and the unchanged CSP + X-Content-Type-Options.
func TestSecurityHeadersUnconditional(t *testing.T) {
	s := &Server{cfg: config.Config{}}
	h := s.securityHeaders(okNext())

	req := httptest.NewRequest(http.MethodGet, "http://readout.example/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	want := map[string]string{
		"Referrer-Policy":            "no-referrer",
		"Permissions-Policy":         "camera=(), microphone=(), geolocation=()",
		"Cross-Origin-Opener-Policy": "same-origin",
		"X-Content-Type-Options":     "nosniff",
		"Content-Security-Policy":    csp,
	}
	for header, value := range want {
		if got := rec.Header().Get(header); got != value {
			t.Errorf("%s = %q, want %q", header, got, value)
		}
	}
}

// TestSecurityHeadersHSTSGatedOnHTTPSPublicURL proves HSTS is emitted only when
// the resolved public URL is https. The gate is the public URL scheme, not the
// request scheme: a TLS-terminating proxy can hand the app a plain-http request
// while the public origin is https.
func TestSecurityHeadersHSTSGatedOnHTTPSPublicURL(t *testing.T) {
	const wantHSTS = "max-age=31536000; includeSubDomains"
	tests := []struct {
		name      string
		publicURL string
		want      string // "" means HSTS absent
	}{
		{name: "https public url", publicURL: "https://readout.example", want: wantHSTS},
		{name: "http public url", publicURL: "http://readout.example", want: ""},
		{name: "unset public url", publicURL: "", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{cfg: config.Config{PublicURL: tc.publicURL}}
			h := s.securityHeaders(okNext())

			// Plain-http request on purpose: a TLS-terminating proxy makes the
			// request look http even when the public origin is https.
			req := httptest.NewRequest(http.MethodGet, "http://internal.svc/", nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if got := rec.Header().Get("Strict-Transport-Security"); got != tc.want {
				t.Errorf("Strict-Transport-Security = %q, want %q", got, tc.want)
			}
		})
	}
}
