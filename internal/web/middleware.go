package web

import (
	"net/http"

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

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}
