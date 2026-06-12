package web

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/kbelokon/readout/internal/config"
)

// hostAllowlist is the DNS-rebinding close-out for the loopback no-auth bind.
// It is active ONLY when the resolved bind is loopback under auth.mode=none
// (config.EnforceLoopbackHostAllowlist); in that mode the only legitimate Host
// values are the loopback names, so a request whose Host header is anything
// else is a forged-Host rebinding attempt and gets 421 Misdirected Request. It
// never rejects the operator's own access (localhost/127.0.0.1/[::1] pass), and
// when the bind is non-loopback or auth is enabled it is a pass-through: the
// operator reaches readout by its real name and any Host is accepted.
func (s *Server) hostAllowlist(next http.Handler) http.Handler {
	if !s.cfg.EnforceLoopbackHostAllowlist() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !config.AllowedHost(r.Host) {
			http.Error(w, "misdirected request: host not allowed", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) readOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && (r.Method != http.MethodPost || r.URL.Path != "/preferences") {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sameOrigin is the stateless CSRF guard. It runs for every non-GET/HEAD method
// (so any future state-changing route inherits it the moment it is added) and
// rejects a request a cross-site page drove from a victim's browser. It is
// method-based on purpose: the allowlist of WHICH routes accept a non-GET stays
// in readOnly; this guard only decides whether a permitted non-GET request is
// same-origin. SameSite=Lax cookies are the defense-in-depth layer; this is the
// active gate. The decision is sameSitePermitted (see its doc for the
// Origin/Sec-Fetch-Site/Referer ladder and the older-browser fallback).
func (s *Server) sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		if !sameSitePermitted(r, s.cfg.PublicURL) {
			http.Error(w, "cross-site request rejected", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sameSitePermitted reports whether a state-changing request is same-origin
// enough to admit. The ladder, strongest signal first:
//
//  1. Origin present — its host must equal the request Host or the configured
//     publicUrl host; a mismatch is a cross-site form post and is rejected.
//  2. No Origin (older browsers omit it on same-origin form posts) — fall back
//     to Sec-Fetch-Site: same-origin/same-site/none allow, cross-site rejects.
//  3. Neither Origin nor Sec-Fetch-Site — fall back to Referer host the same way
//     Origin is matched.
//  4. None of the three present — allow (an old browser with no usable signal;
//     this is the SameSite=Lax-only annoyance-grade gap the unit accepts).
func sameSitePermitted(r *http.Request, publicURL string) bool {
	if origin := r.Header.Get("Origin"); origin != "" {
		return originHostMatches(origin, r.Host, publicURL)
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site != "cross-site"
	}
	if referer := r.Header.Get("Referer"); referer != "" {
		return originHostMatches(referer, r.Host, publicURL)
	}
	return true
}

// originHostMatches reports whether the host of rawURL (an Origin or Referer
// value) equals the request host or the configured publicUrl host. An
// unparseable rawURL or one with no host never matches, so a malformed Origin
// fails closed.
func originHostMatches(rawURL, requestHost, publicURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, requestHost) {
		return true
	}
	if publicURL != "" {
		if pu, err := url.Parse(publicURL); err == nil && pu.Host != "" && strings.EqualFold(u.Host, pu.Host) {
			return true
		}
	}
	return false
}

// hstsValue is the Strict-Transport-Security policy emitted only on https
// deployments: one year, including subdomains.
const hstsValue = "max-age=31536000; includeSubDomains"

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	// Resolve the HSTS gate once when the middleware is built, not per-request:
	// HSTS is emitted only when the configured public URL is https. The gate is
	// the public URL scheme, NOT the request scheme -- a TLS-terminating proxy
	// can hand the app a plain-http request while the public origin is https, so
	// keying on the request scheme would suppress HSTS on a TLS deployment.
	hsts := publicURLIsHTTPS(s.cfg.PublicURL)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		if hsts {
			w.Header().Set("Strict-Transport-Security", hstsValue)
		}
		next.ServeHTTP(w, r)
	})
}

// publicURLIsHTTPS reports whether the configured public URL resolves to an
// https origin. An empty or unparseable value is not https (no HSTS), so a plain
// http or unset deployment never gets the HSTS header.
func publicURLIsHTTPS(publicURL string) bool {
	if publicURL == "" {
		return false
	}
	u, err := url.Parse(publicURL)
	if err != nil {
		return false
	}
	return u.Scheme == "https"
}
