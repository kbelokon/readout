package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kbelokon/readout/internal/config"
)

// okNext is a sentinel downstream handler: a 200 proves the request passed the
// host allowlist; the test asserts on status only.
func okNext() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

// TestHostAllowlistLoopbackNoAuthRejectsForgedHost proves the DNS-rebinding
// close-out: under a loopback bind with auth disabled, a request whose Host is
// not an allowlisted loopback name (a forged-Host rebinding attempt) is rejected
// with 421, while the operator's own loopback Host values pass through.
func TestHostAllowlistLoopbackNoAuthRejectsForgedHost(t *testing.T) {
	s := &Server{cfg: config.Config{AuthMode: config.AuthModeNone, ListenAddress: "127.0.0.1", Port: 8080}}
	h := s.hostAllowlist(okNext())

	forged := httptest.NewRequest(http.MethodGet, "http://evil.example/", nil)
	forged.Host = "evil.example"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, forged)
	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("forged Host: status = %d, want %d", rec.Code, http.StatusMisdirectedRequest)
	}

	for _, host := range []string{"localhost", "localhost:8080", "127.0.0.1", "127.0.0.1:8080", "[::1]:8080"} {
		ok := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
		ok.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, ok)
		if rec.Code != http.StatusOK {
			t.Fatalf("allowlisted Host %q: status = %d, want 200", host, rec.Code)
		}
	}
}

// TestHostAllowlistNonLoopbackBindAcceptsAnyHost proves the allowlist is scoped
// to the loopback no-auth bind: when the operator binds a network address, the
// middleware is a pass-through and any Host (including the operator's real name)
// is accepted.
func TestHostAllowlistNonLoopbackBindAcceptsAnyHost(t *testing.T) {
	s := &Server{cfg: config.Config{AuthMode: config.AuthModeNone, ListenAddress: "0.0.0.0", Port: 8080}}
	h := s.hostAllowlist(okNext())

	req := httptest.NewRequest(http.MethodGet, "http://readout.internal/", nil)
	req.Host = "readout.internal"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("non-loopback bind should accept any Host, got status %d", rec.Code)
	}
}

// TestHostAllowlistAuthEnabledAcceptsAnyHost proves an enabled auth mode (which
// already gates access) layers no Host allowlist, even on a loopback bind.
func TestHostAllowlistAuthEnabledAcceptsAnyHost(t *testing.T) {
	s := &Server{cfg: config.Config{AuthMode: config.AuthModeHeaders, ListenAddress: "127.0.0.1", Port: 8080}}
	h := s.hostAllowlist(okNext())

	req := httptest.NewRequest(http.MethodGet, "http://anything.example/", nil)
	req.Host = "anything.example"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("auth-enabled bind should accept any Host, got status %d", rec.Code)
	}
}
