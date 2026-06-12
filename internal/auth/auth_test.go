package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kbelokon/readout/internal/config"
	"github.com/kbelokon/readout/internal/hooks"
	"golang.org/x/oauth2"
)

func newAuth(t *testing.T, cfg *config.Config) *Authenticator {
	t.Helper()
	a, err := New(cfg, cfg.SessionSecret, time.Now, hooks.NewClient())
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestSealedSessionFixtureOpens is the wire-compatibility oracle. The fixture in
// testdata/sealed_session_v1.txt was sealed on the pre-split tree by the old web
// session codec using the test-only secret below and the fixed payload asserted
// here (a 100-year envelope TTL keeps it openable). A sealed session minted
// before this package existed must still open and decode field-for-field after
// the move -- that is the product law the split preserves.
func TestSealedSessionFixtureOpens(t *testing.T) {
	const secret = "fixture-secret-do-not-use-in-production"
	codec, err := newSessionCodec(secret)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("testdata/sealed_session_v1.txt")
	if err != nil {
		t.Fatal(err)
	}
	value := strings.TrimRight(string(data), "\n")
	var got Session
	if err := codec.Open(SessionCookieName, value, &got); err != nil {
		t.Fatalf("pre-move sealed session failed to open after the move: %v", err)
	}
	want := Session{
		AccessToken:  "fixture-access-token",
		TokenType:    "Bearer",
		RefreshToken: "fixture-refresh-token",
		IDToken:      "fixture-id-token",
		Expires:      4102444800,
		User:         "fixture-user",
		Email:        "fixture@example.test",
		Groups:       []string{"viewers", "ops"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fixture decode mismatch:\n got=%#v\nwant=%#v", got, want)
	}
}

func TestAuthMiddlewareModes(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	a := newAuth(t, &config.Config{
		AuthMode:           config.AuthModeHeaders,
		TrustedHeaderUser:  "X-User",
		TrustedHeaderEmail: "X-Email",
	})

	public := httptest.NewRecorder()
	a.Middleware(next).ServeHTTP(public, httptest.NewRequest(http.MethodGet, "/health", nil))
	if public.Code != http.StatusNoContent {
		t.Fatalf("public status = %d", public.Code)
	}

	missing := httptest.NewRecorder()
	a.Middleware(next).ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/clusters", nil))
	if missing.Code != http.StatusUnauthorized {
		t.Fatalf("missing header status = %d", missing.Code)
	}

	authorized := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/clusters", nil)
	req.Header.Set("X-Email", "user@example.test")
	a.Middleware(next).ServeHTTP(authorized, req)
	if authorized.Code != http.StatusNoContent {
		t.Fatalf("trusted header status = %d", authorized.Code)
	}

	badMode := newAuth(t, &config.Config{AuthMode: "bogus"})
	rec := httptest.NewRecorder()
	badMode.Middleware(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/clusters", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("bad auth mode status = %d", rec.Code)
	}
}

func TestHeaderGroupsReachHook(t *testing.T) {
	var payload struct {
		Token   map[string]any `json:"token"`
		Session Session        `json:"session"`
	}
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": true})
	}))
	defer hook.Close()

	a := newAuth(t, &config.Config{
		AuthMode:             config.AuthModeHeaders,
		TrustedHeaderUser:    "X-User",
		TrustedHeaderEmail:   "X-Email",
		TrustedHeaderGroups:  "X-Groups",
		AuthorizationHookURL: hook.URL,
	})
	nextCalled := false
	handler := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/clusters", nil)
	req.Header.Set("X-User", "kirill")
	req.Header.Set("X-Email", "kirill@example.test")
	req.Header.Set("X-Groups", "viewers, ops, ,debug")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || !nextCalled {
		t.Fatalf("headers auth status=%d nextCalled=%t body=%s", rec.Code, nextCalled, rec.Body.String())
	}
	// Token minimization: with no includeTokens configured, the hook receives only
	// token_type and expiry metadata -- never the access/refresh/id tokens.
	if _, ok := payload.Token["access_token"]; ok {
		t.Fatalf("access_token leaked to hook by default: %#v", payload.Token)
	}
	if payload.Token["expiry"] == "" {
		t.Fatalf("headers hook token payload = %#v", payload.Token)
	}
	if payload.Session.User != "kirill" || payload.Session.Email != "kirill@example.test" || !reflect.DeepEqual(payload.Session.Groups, []string{"viewers", "ops", "debug"}) {
		t.Fatalf("headers hook session = %#v", payload.Session)
	}

	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": false})
	}))
	defer deny.Close()
	a.cfg.AuthorizationHookURL = deny.URL
	nextCalled = false
	req = httptest.NewRequest(http.MethodGet, "/clusters", nil)
	req.Header.Set("X-Email", "kirill@example.test")
	req.Header.Set("X-Groups", "viewers")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || nextCalled {
		t.Fatalf("headers deny status=%d nextCalled=%t body=%s", rec.Code, nextCalled, rec.Body.String())
	}
}

func TestHeaderModeNoHookPassthrough(t *testing.T) {
	a := newAuth(t, &config.Config{
		AuthMode:           config.AuthModeHeaders,
		TrustedHeaderUser:  "X-User",
		TrustedHeaderEmail: "X-Email",
	})
	nextCalled := false
	handler := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/clusters", nil)
	req.Header.Set("X-User", "kirill")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || !nextCalled {
		t.Fatalf("headers no-hook status=%d nextCalled=%t body=%s", rec.Code, nextCalled, rec.Body.String())
	}
}

// TestHeaderModeTrustedProxyCIDR pins the peer-CIDR gate: with trustedProxyCidrs
// set, a spoofed identity header from a peer OUTSIDE the CIDR is rejected
// (403), while the same header from INSIDE the CIDR is served. The gate reads
// r.RemoteAddr, never a forwarded header, so X-Forwarded-For cannot move the
// peer into the trusted range.
func TestHeaderModeTrustedProxyCIDR(t *testing.T) {
	a := newAuth(t, &config.Config{
		AuthMode:          config.AuthModeHeaders,
		TrustedHeaderUser: "X-User",
		TrustedProxyCIDRs: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
	})
	handler := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	// Outside the CIDR: spoofed header must NOT be honored.
	outside := httptest.NewRequest(http.MethodGet, "/clusters", nil)
	outside.RemoteAddr = "203.0.113.7:5555"
	outside.Header.Set("X-User", "attacker")
	// A spoofed X-Forwarded-For claiming an in-CIDR address must not help.
	outside.Header.Set("X-Forwarded-For", "10.1.2.3")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, outside)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("outside-CIDR spoof status=%d, want 403; body=%s", rec.Code, rec.Body.String())
	}

	// Inside the CIDR: header identity is honored.
	inside := httptest.NewRequest(http.MethodGet, "/clusters", nil)
	inside.RemoteAddr = "10.4.5.6:5555"
	inside.Header.Set("X-User", "kirill")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, inside)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("inside-CIDR status=%d, want 204; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHeaderModeTrustedProxyDeniesUnparseablePeer pins fail-closed behavior:
// when a CIDR is configured but the peer address is empty or not an IP (unix
// socket / garbage), the request is denied rather than trusted.
func TestHeaderModeTrustedProxyDeniesUnparseablePeer(t *testing.T) {
	a := newAuth(t, &config.Config{
		AuthMode:          config.AuthModeHeaders,
		TrustedHeaderUser: "X-User",
		TrustedProxyCIDRs: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
	})
	handler := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, peer := range []string{"", "@", "/run/readout.sock", "not-an-ip"} {
		req := httptest.NewRequest(http.MethodGet, "/clusters", nil)
		req.RemoteAddr = peer
		req.Header.Set("X-User", "kirill")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("unparseable peer %q status=%d, want 403 (fail closed)", peer, rec.Code)
		}
	}
}

// TestHeaderModeTrustedProxyIPv6 pins that an IPv6 peer inside an IPv6 CIDR is
// honored (host:port parsing of the bracketed form, zone stripped).
func TestHeaderModeTrustedProxyIPv6(t *testing.T) {
	a := newAuth(t, &config.Config{
		AuthMode:          config.AuthModeHeaders,
		TrustedHeaderUser: "X-User",
		TrustedProxyCIDRs: []netip.Prefix{netip.MustParsePrefix("2001:db8::/32")},
	})
	handler := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	in := httptest.NewRequest(http.MethodGet, "/clusters", nil)
	in.RemoteAddr = "[2001:db8::1]:5555"
	in.Header.Set("X-User", "kirill")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, in)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("in-CIDR IPv6 status=%d, want 204; body=%s", rec.Code, rec.Body.String())
	}

	out := httptest.NewRequest(http.MethodGet, "/clusters", nil)
	out.RemoteAddr = "[2001:dead::1]:5555"
	out.Header.Set("X-User", "attacker")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, out)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("out-of-CIDR IPv6 status=%d, want 403", rec.Code)
	}
}

// TestHeaderModeNoCIDRTrustsHeaders pins the unset-CIDR behavior: with no
// trustedProxyCidrs the peer is not checked at all and header identity is
// trusted as before (the loud startup warning is emitted at server build, not
// here).
func TestHeaderModeNoCIDRTrustsHeaders(t *testing.T) {
	a := newAuth(t, &config.Config{
		AuthMode:          config.AuthModeHeaders,
		TrustedHeaderUser: "X-User",
	})
	handler := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/clusters", nil)
	req.RemoteAddr = "203.0.113.7:5555" // off-host peer, but no CIDR gate
	req.Header.Set("X-User", "kirill")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("no-CIDR headers status=%d, want 204; body=%s", rec.Code, rec.Body.String())
	}
}

// TestSplitHeaderGroupsCapped pins the group-header bounds: an enormous header
// is capped by count, and an oversized total length is truncated before the
// split fans out.
func TestSplitHeaderGroupsCapped(t *testing.T) {
	var b strings.Builder
	for i := 0; i < maxHeaderGroups+50; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "g%d", i)
	}
	groups := splitHeaderGroups(b.String())
	if len(groups) > maxHeaderGroups {
		t.Fatalf("group count = %d, want <= %d", len(groups), maxHeaderGroups)
	}
	if len(groups) != maxHeaderGroups {
		t.Fatalf("group count = %d, want exactly the cap %d", len(groups), maxHeaderGroups)
	}

	// A single oversized value (no commas) is length-truncated, never returned
	// whole, and never panics.
	huge := strings.Repeat("a", maxHeaderGroupsLen*4)
	got := splitHeaderGroups(huge)
	if len(got) != 1 || len(got[0]) > maxHeaderGroupsLen {
		t.Fatalf("oversized single group not truncated: count=%d len=%d", len(got), len(got[0]))
	}
}

func TestOAuthLoginLogoutAndURLHelpers(t *testing.T) {
	a := newAuth(t, &config.Config{
		OIDCClientID:       "client-id",
		OAuth2AuthorizeURL: "https://auth.example.test/authorize",
		OAuth2TokenURL:     "https://auth.example.test/token",
		OAuth2Scope:        "openid email",
		SessionSecret:      "test-secret",
		AuthMode:           config.AuthModeOIDC,
	})

	rec := httptest.NewRecorder()
	a.Login(rec, httptest.NewRequest(http.MethodGet, "/oauth2/login?next=https://evil.example/path", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("login status = %d body=%s", rec.Code, rec.Body.String())
	}
	location := rec.Header().Get("Location")
	if !strings.HasPrefix(location, "https://auth.example.test/authorize?") || queryValue(location, "scope") != "openid email" {
		t.Fatalf("bad authorize redirect: %s", location)
	}
	var state oauthState
	if err := a.sessions.Open(StateCookieName, cookieNamed(t, rec.Result().Cookies(), StateCookieName).Value, &state); err != nil {
		t.Fatal(err)
	}
	if state.OriginalURL != "/" || state.Nonce == "" || queryValue(location, "state") != state.Nonce {
		t.Fatalf("state = %#v location=%s", state, location)
	}
	for _, tc := range []struct {
		next string
		want string
	}{
		{"/clusters/test", "/clusters/test"},
		{"//evil.example/path", "/"},
		{`/\evil.example/path`, "/"},
	} {
		rec := httptest.NewRecorder()
		a.Login(rec, httptest.NewRequest(http.MethodGet, "/oauth2/login?next="+url.QueryEscape(tc.next), nil))
		if rec.Code != http.StatusFound {
			t.Fatalf("login %q status = %d body=%s", tc.next, rec.Code, rec.Body.String())
		}
		var state oauthState
		if err := a.sessions.Open(StateCookieName, cookieNamed(t, rec.Result().Cookies(), StateCookieName).Value, &state); err != nil {
			t.Fatal(err)
		}
		if state.OriginalURL != tc.want {
			t.Fatalf("login next %q stored OriginalURL %q, want %q", tc.next, state.OriginalURL, tc.want)
		}
	}

	logout := httptest.NewRecorder()
	a.Logout(logout, httptest.NewRequest(http.MethodGet, "/oauth2/logout", nil))
	if logout.Code != http.StatusFound || logout.Header().Get("Location") != "/" {
		t.Fatalf("logout redirect status=%d location=%q", logout.Code, logout.Header().Get("Location"))
	}
	for _, name := range []string{SessionCookieName, StateCookieName} {
		cookie := cookieNamed(t, logout.Result().Cookies(), name)
		if cookie.MaxAge >= 0 {
			t.Fatalf("%s was not cleared: %#v", name, cookie)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "http://internal.example/path", nil)
	req.Header.Set("X-Forwarded-Proto", "https, http")
	req.Header.Set("X-Forwarded-Host", "public.example.test, internal.example")
	if got := externalURL(req, CallbackPath); got != "https://public.example.test/oauth2/callback" {
		t.Fatalf("externalURL = %q", got)
	}
	if firstForwarded(" a, b ") != "a" || !secureCookie(req) {
		t.Fatalf("forwarded helpers mismatch")
	}
}

// TestRedirectURLPrecedence pins the callback-URL resolution order: an explicit
// oidc.redirectUrl wins outright; with it empty a configured publicUrl yields a
// stable origin+CallbackPath callback that ignores request headers; with both
// empty the redirect falls back to the per-request X-Forwarded reconstruction
// (the retained dev convenience).
func TestRedirectURLPrecedence(t *testing.T) {
	const proto, host = "https", "fwd.example.test"
	reqWithForwarded := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "http://internal.example/clusters", nil)
		r.Header.Set("X-Forwarded-Proto", proto)
		r.Header.Set("X-Forwarded-Host", host)
		return r
	}
	cases := []struct {
		name        string
		redirectURL string
		publicURL   string
		want        string
	}{
		{
			name:        "explicit redirectUrl wins over publicUrl",
			redirectURL: "https://explicit.example/oauth2/callback",
			publicURL:   "https://public.example",
			want:        "https://explicit.example/oauth2/callback",
		},
		{
			name:      "publicUrl derives callback when redirectUrl empty",
			publicURL: "https://public.example",
			want:      "https://public.example" + CallbackPath,
		},
		{
			name: "falls back to X-Forwarded when both empty",
			want: proto + "://" + host + CallbackPath,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newAuth(t, &config.Config{
				AuthMode:           config.AuthModeOIDC,
				OIDCClientID:       "client-id",
				OAuth2AuthorizeURL: "https://auth.example/authorize",
				OAuth2TokenURL:     "https://auth.example/token",
				OIDCRedirectURL:    tc.redirectURL,
				PublicURL:          tc.publicURL,
				SessionSecret:      "test-secret",
			})
			cfg, _, err := a.oauth2Config(context.Background(), reqWithForwarded())
			if err != nil {
				t.Fatal(err)
			}
			if cfg.RedirectURL != tc.want {
				t.Fatalf("RedirectURL = %q, want %q", cfg.RedirectURL, tc.want)
			}
		})
	}
}

func TestSessionCodecAndBearerSources(t *testing.T) {
	codec, err := newSessionCodec("test-secret")
	if err != nil {
		t.Fatal(err)
	}
	value, err := codec.Seal("session", map[string]string{"user": "user"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var opened map[string]string
	if err := codec.Open("session", value, &opened); err != nil || opened["user"] != "user" {
		t.Fatalf("open = %#v err=%v", opened, err)
	}
	if err := codec.Open("wrong", value, &opened); err == nil {
		t.Fatal("expected wrong associated data to fail")
	}
	if err := codec.Open("session", "not-base64", &opened); err == nil {
		t.Fatal("expected invalid base64 to fail")
	}
	expired, err := codec.Seal("session", map[string]string{"user": "old"}, -time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := codec.Open("session", expired, &opened); err == nil {
		t.Fatal("expected expired session to fail")
	}

	a := newAuth(t, &config.Config{SessionSecret: "test-secret"})
	req := httptest.NewRequest(http.MethodGet, "/clusters", nil)
	req.Header.Set("Authorization", "Bearer header-token")
	if got := a.RequestBearer(req); got != "header-token" {
		t.Fatalf("header bearer = %q", got)
	}

	// The legacy access_token cookie is no longer a bearer source: only the
	// Authorization header and the sealed session cookie are honored. A request
	// carrying only this cookie must resolve to no bearer (anonymous).
	cookieReq := httptest.NewRequest(http.MethodGet, "/clusters", nil)
	cookieReq.AddCookie(&http.Cookie{Name: "access_token", Value: "cookie-token"})
	if got := a.RequestBearer(cookieReq); got != "" {
		t.Fatalf("legacy access_token cookie should not be a bearer source, got %q", got)
	}

	sessionValue, err := a.SealSession(&Session{AccessToken: "session-token", Expires: time.Now().Add(time.Hour).Unix()}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	sessionReq := httptest.NewRequest(http.MethodGet, "/clusters", nil)
	sessionReq.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessionValue})
	if got := a.RequestBearer(sessionReq); got != "session-token" {
		t.Fatalf("session bearer = %q", got)
	}

	expiredValue, err := a.SealSession(&Session{AccessToken: "expired-token", Expires: time.Now().Add(-time.Hour).Unix()}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	expiredReq := httptest.NewRequest(http.MethodGet, "/clusters", nil)
	expiredReq.AddCookie(&http.Cookie{Name: SessionCookieName, Value: expiredValue})
	if _, ok := a.Session(expiredReq); ok {
		t.Fatal("expired auth session should not be accepted")
	}
}

func TestOAuthCallbackRejectsExternalOriginalURL(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"session-token","token_type":"Bearer","expires_in":3600}`))
	}))
	defer tokenServer.Close()

	a := newAuth(t, &config.Config{
		OIDCClientID:       "client-id",
		OIDCClientSecret:   "client-secret",
		OAuth2AuthorizeURL: "https://auth.example.test/authorize",
		OAuth2TokenURL:     tokenServer.URL,
		OIDCRedirectURL:    "http://example.test/oauth2/callback",
		SessionSecret:      "test-secret",
	})
	for _, originalURL := range []string{"//evil.example/path", `/\evil.example/path`} {
		value, err := a.sessions.Seal(StateCookieName, oauthState{Nonce: "good", OriginalURL: originalURL}, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodGet, "/oauth2/callback?state=good&code=ok", nil)
		req.AddCookie(&http.Cookie{Name: StateCookieName, Value: value})
		rec := httptest.NewRecorder()
		a.Callback(rec, req)
		if rec.Code != http.StatusFound {
			t.Fatalf("callback for %q status = %d body=%s", originalURL, rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != "/" {
			t.Fatalf("callback for %q Location = %q, want /", originalURL, loc)
		}
	}
}

func TestOAuthCallbackRejectsBadInputs(t *testing.T) {
	a := newAuth(t, &config.Config{
		OIDCClientID:       "client-id",
		OAuth2AuthorizeURL: "https://auth.example.test/authorize",
		OAuth2TokenURL:     "https://auth.example.test/token",
		SessionSecret:      "test-secret",
	})

	cases := []struct {
		name   string
		req    *http.Request
		status int
	}{
		{"missing state cookie", httptest.NewRequest(http.MethodGet, "/oauth2/callback?state=s", nil), http.StatusBadRequest},
		{"invalid state cookie", func() *http.Request {
			req := httptest.NewRequest(http.MethodGet, "/oauth2/callback?state=s", nil)
			req.AddCookie(&http.Cookie{Name: StateCookieName, Value: "bad"})
			return req
		}(), http.StatusBadRequest},
		{"state mismatch", func() *http.Request {
			req := httptest.NewRequest(http.MethodGet, "/oauth2/callback?state=bad", nil)
			value, err := a.sessions.Seal(StateCookieName, oauthState{Nonce: "good", OriginalURL: "/clusters"}, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			req.AddCookie(&http.Cookie{Name: StateCookieName, Value: value})
			return req
		}(), http.StatusBadRequest},
		{"provider error", func() *http.Request {
			req := httptest.NewRequest(http.MethodGet, "/oauth2/callback?state=good&error=access_denied", nil)
			value, err := a.sessions.Seal(StateCookieName, oauthState{Nonce: "good", OriginalURL: "/clusters"}, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			req.AddCookie(&http.Cookie{Name: StateCookieName, Value: value})
			return req
		}(), http.StatusUnauthorized},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		a.Callback(rec, tc.req)
		if rec.Code != tc.status {
			t.Fatalf("%s status = %d, want %d body=%s", tc.name, rec.Code, tc.status, rec.Body.String())
		}
	}
}

func TestOAuthCallbackRejectsDeniedExpiredAndHookErrors(t *testing.T) {
	cases := []struct {
		name          string
		tokenResponse string
		hookResponse  string
		hookStatus    int
		wantStatus    int
	}{
		{
			name:          "denied by hook",
			tokenResponse: `{"access_token":"session-token","token_type":"Bearer","expires_in":3600}`,
			hookResponse:  `{"allowed":false}`,
			hookStatus:    http.StatusOK,
			wantStatus:    http.StatusForbidden,
		},
		{
			name:          "hook error",
			tokenResponse: `{"access_token":"session-token","token_type":"Bearer","expires_in":3600}`,
			hookResponse:  `broken`,
			hookStatus:    http.StatusInternalServerError,
			wantStatus:    http.StatusForbidden,
		},
		{
			name:          "expired token",
			tokenResponse: `{"access_token":"session-token","token_type":"Bearer","expires_in":-60}`,
			wantStatus:    http.StatusUnauthorized,
		},
	}
	for _, tc := range cases {
		tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(tc.tokenResponse))
		}))
		hookURL := ""
		var hook *httptest.Server
		if tc.hookResponse != "" {
			hook = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.hookStatus)
				_, _ = w.Write([]byte(tc.hookResponse))
			}))
			hookURL = hook.URL
		}
		a := newAuth(t, &config.Config{
			OIDCClientID:         "client-id",
			OIDCClientSecret:     "client-secret",
			OAuth2AuthorizeURL:   "https://auth.example.test/authorize",
			OAuth2TokenURL:       tokenServer.URL,
			OIDCRedirectURL:      "http://example.test/oauth2/callback",
			SessionSecret:        "test-secret",
			AuthorizationHookURL: hookURL,
		})
		value, err := a.sessions.Seal(StateCookieName, oauthState{Nonce: "good", OriginalURL: "/clusters"}, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodGet, "/oauth2/callback?state=good&code=ok", nil)
		req.AddCookie(&http.Cookie{Name: StateCookieName, Value: value})
		rec := httptest.NewRecorder()
		a.Callback(rec, req)
		if rec.Code != tc.wantStatus {
			t.Fatalf("%s status = %d, want %d body=%s", tc.name, rec.Code, tc.wantStatus, rec.Body.String())
		}
		tokenServer.Close()
		if hook != nil {
			hook.Close()
		}
	}
}

func TestOAuth2ConfigErrors(t *testing.T) {
	a := newAuth(t, &config.Config{})
	if _, _, err := a.oauth2Config(context.Background(), httptest.NewRequest(http.MethodGet, "/clusters", nil)); err == nil {
		t.Fatal("expected missing client id error")
	}
	a.cfg.OIDCClientID = "client-id"
	if _, _, err := a.oauth2Config(context.Background(), httptest.NewRequest(http.MethodGet, "/clusters", nil)); err == nil {
		t.Fatal("expected missing provider endpoints error")
	}
	rec := httptest.NewRecorder()
	a.startOAuth2(rec, httptest.NewRequest(http.MethodGet, "/clusters", nil), "/clusters")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("startOAuth2 error status = %d", rec.Code)
	}
}

func TestOAuthHandlersAndSessionCodecEdges(t *testing.T) {
	a := newAuth(t, &config.Config{
		OIDCClientID:       "client",
		OAuth2AuthorizeURL: "https://auth.example/authorize",
		OAuth2TokenURL:     "https://auth.example/token",
		SessionSecret:      "test-secret",
	})
	rec := httptest.NewRecorder()
	a.Login(rec, httptest.NewRequest(http.MethodGet, "/oauth2/login?next=https://evil.example", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("login status = %d", rec.Code)
	}
	var state oauthState
	if err := a.sessions.Open(StateCookieName, cookieNamed(t, rec.Result().Cookies(), StateCookieName).Value, &state); err != nil {
		t.Fatal(err)
	}
	if state.OriginalURL != "/" {
		t.Fatalf("unsafe next original URL = %q", state.OriginalURL)
	}

	rec = httptest.NewRecorder()
	a.Logout(rec, httptest.NewRequest(http.MethodGet, "/oauth2/logout", nil))
	if rec.Code != http.StatusFound || len(rec.Result().Cookies()) != 2 {
		t.Fatalf("logout status=%d cookies=%#v", rec.Code, rec.Result().Cookies())
	}

	for name, target := range map[string]string{
		"missing state": "/oauth2/callback",
		"bad state":     "/oauth2/callback?state=x",
	} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		if name == "bad state" {
			req.AddCookie(&http.Cookie{Name: StateCookieName, Value: "not-a-sealed-cookie"})
		}
		rec = httptest.NewRecorder()
		a.Callback(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d", name, rec.Code)
		}
	}

	sealed, err := a.sessions.Seal(StateCookieName, oauthState{Nonce: "nonce", OriginalURL: "/clusters"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/oauth2/callback?state=wrong", nil)
	req.AddCookie(&http.Cookie{Name: StateCookieName, Value: sealed})
	rec = httptest.NewRecorder()
	a.Callback(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("state mismatch status = %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/oauth2/callback?state=nonce&error=denied", nil)
	req.AddCookie(&http.Cookie{Name: StateCookieName, Value: sealed})
	rec = httptest.NewRecorder()
	a.Callback(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("oauth error status = %d", rec.Code)
	}

	if _, _, err := a.oauth2Config(context.Background(), httptest.NewRequest(http.MethodGet, "/", nil)); err != nil {
		t.Fatalf("oauth2Config generic endpoints failed: %v", err)
	}
	missing := newAuth(t, &config.Config{})
	if _, _, err := missing.oauth2Config(context.Background(), httptest.NewRequest(http.MethodGet, "/", nil)); err == nil {
		t.Fatal("oauth2Config without client id unexpectedly succeeded")
	}
	if got := externalURL(httptest.NewRequest(http.MethodGet, "http://internal.example/path", nil), "/oauth2/callback"); got != "http://internal.example/oauth2/callback" {
		t.Fatalf("externalURL plain = %q", got)
	}
	req = httptest.NewRequest(http.MethodGet, "http://internal/path", nil)
	req.Header.Set("X-Forwarded-Proto", "https,http")
	req.Header.Set("X-Forwarded-Host", "kwv.example,proxy")
	if got := externalURL(req, "/oauth2/callback"); got != "https://kwv.example/oauth2/callback" {
		t.Fatalf("externalURL forwarded = %q", got)
	}

	codec, err := newSessionCodec("test-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := codec.Open(SessionCookieName, "%%%bad", &Session{}); err == nil {
		t.Fatal("bad base64 unexpectedly opened")
	}
	if err := codec.Open(SessionCookieName, "short", &Session{}); err == nil {
		t.Fatal("short sealed value unexpectedly opened")
	}
	value, err := codec.Seal(SessionCookieName, Session{AccessToken: "x"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := codec.Open("wrong-name", value, &Session{}); err == nil {
		t.Fatal("wrong associated data unexpectedly opened")
	}
	expired, err := codec.Seal(SessionCookieName, Session{AccessToken: "x"}, -time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := codec.Open(SessionCookieName, expired, &Session{}); err == nil {
		t.Fatal("expired sealed value unexpectedly opened")
	}
	if oauthExpiry(&oauth2.Token{}).Before(time.Now().Add(6 * 24 * time.Hour)) {
		t.Fatal("zero-expiry OAuth token did not get default lifetime")
	}
}

func TestAuthModesHeadersOIDCAndBearerSources(t *testing.T) {
	a := newAuth(t, &config.Config{AuthMode: config.AuthModeHeaders, TrustedHeaderUser: "X-User", TrustedHeaderEmail: "X-Email"})
	called := false
	handler := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/clusters", nil))
	if rec.Code != http.StatusUnauthorized || called {
		t.Fatalf("missing trusted headers status=%d called=%t", rec.Code, called)
	}
	req := httptest.NewRequest(http.MethodGet, "/clusters", nil)
	req.Header.Set("X-User", "user")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || !called {
		t.Fatalf("trusted header status=%d called=%t", rec.Code, called)
	}

	a.cfg.AuthMode = "bogus"
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/clusters", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("invalid auth status = %d", rec.Code)
	}

	a = newAuth(t, &config.Config{
		AuthMode:           config.AuthModeOIDC,
		OIDCClientID:       "client",
		OAuth2AuthorizeURL: "https://auth.example/authorize",
		OAuth2TokenURL:     "https://auth.example/token",
		SessionSecret:      "test-secret",
	})
	handler = a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/clusters?x=1", nil))
	if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "https://auth.example/authorize") {
		t.Fatalf("OIDC redirect status=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}
	if cookieNamed(t, rec.Result().Cookies(), StateCookieName).Path != CallbackPath {
		t.Fatalf("state cookie path mismatch: %#v", rec.Result().Cookies())
	}

	value, err := a.SealSession(&Session{AccessToken: "session-token", Expires: time.Now().Add(time.Hour).Unix()}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/clusters", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: value})
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("valid session status = %d", rec.Code)
	}
	if got := a.RequestBearer(req); got != "session-token" {
		t.Fatalf("session bearer = %q", got)
	}
	req.Header.Set("Authorization", "Bearer direct-token")
	if got := a.RequestBearer(req); got != "direct-token" {
		t.Fatalf("direct bearer = %q", got)
	}
	// The legacy access_token cookie is no longer a bearer source: a request
	// carrying only this cookie resolves to no bearer (anonymous).
	req = httptest.NewRequest(http.MethodGet, "/clusters", nil)
	req.AddCookie(&http.Cookie{Name: "access_token", Value: "cookie-token"})
	if got := a.RequestBearer(req); got != "" {
		t.Fatalf("legacy access_token cookie should not be a bearer source, got %q", got)
	}
}

func TestAuthorizationHookAllowsDeniesAndUpdatesSession(t *testing.T) {
	a := newAuth(t, &config.Config{})
	session := Session{AccessToken: "old", User: "old-user"}
	token := (&oauth2.Token{
		AccessToken:  "hook-token",
		TokenType:    "Bearer",
		RefreshToken: "refresh",
		Expiry:       time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
	}).WithExtra(map[string]any{"id_token": "id.jwt"})

	allowed, err := a.authorizationHook(context.Background(), token, &session)
	if err != nil || !allowed || session.User != "old-user" {
		t.Fatalf("empty hook allowed=%v updated=%#v err=%v", allowed, session, err)
	}

	var payload struct {
		Token   map[string]any `json:"token"`
		Session Session        `json:"session"`
	}
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("bad hook request method=%s content-type=%s", r.Method, r.Header.Get("Content-Type"))
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"allowed":false,"user":"new-user","email":"new@example.test","groups":["ops","dev"]}`))
	}))
	defer hook.Close()

	a.cfg.AuthorizationHookURL = hook.URL
	// Opt every token in so this case exercises the include-tokens lane: with
	// access|id|refresh listed, all three reach the hook.
	a.cfg.AuthorizationHookIncludeTokens = []string{"access", "id", "refresh"}
	allowed, err = a.authorizationHook(context.Background(), token, &session)
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Fatal("hook should deny access")
	}
	if payload.Token["access_token"] != "hook-token" || payload.Token["id_token"] != "id.jwt" || payload.Token["refresh_token"] != "refresh" || payload.Session.User != "old-user" {
		t.Fatalf("unexpected hook payload: %#v", payload)
	}
	if session.User != "new-user" || session.Email != "new@example.test" || !reflect.DeepEqual(session.Groups, []string{"ops", "dev"}) {
		t.Fatalf("updated session = %#v", session)
	}
}

func TestAuthorizationHookErrors(t *testing.T) {
	a := newAuth(t, &config.Config{})
	token := &oauth2.Token{AccessToken: "hook-token", Expiry: time.Now().Add(time.Hour)}
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer fail.Close()

	a.cfg.AuthorizationHookURL = fail.URL
	if _, err := a.authorizationHook(context.Background(), token, &Session{}); err == nil {
		t.Fatal("expected non-2xx hook to fail")
	}
	a.cfg.AuthorizationHookURL = "://bad-url"
	if _, err := a.authorizationHook(context.Background(), token, &Session{}); err == nil {
		t.Fatal("expected invalid hook URL to fail")
	}
}

func TestAuthorizationHookPayload(t *testing.T) {
	a := newAuth(t, &config.Config{})
	token := (&oauth2.Token{AccessToken: "token", TokenType: "Bearer", RefreshToken: "refresh", Expiry: time.Now().Add(time.Hour)}).WithExtra(map[string]any{"id_token": "id-token"})
	session := Session{User: "old"}
	allowed, err := a.authorizationHook(context.Background(), token, &session)
	if err != nil || !allowed || session.User != "old" {
		t.Fatalf("empty authorization hook allowed=%t session=%#v err=%v", allowed, session, err)
	}

	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Accept") != "application/json" {
			t.Fatalf("hook headers = %#v", r.Header)
		}
		var payload struct {
			Token map[string]any `json:"token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Token["id_token"] != "id-token" {
			t.Fatalf("hook payload = %#v", payload)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": true, "user": "new", "email": "n@example", "groups": []string{"ops"}})
	}))
	defer hook.Close()
	a.cfg.AuthorizationHookURL = hook.URL
	a.cfg.AuthorizationHookIncludeTokens = []string{"id"}
	session = Session{User: "old"}
	allowed, err = a.authorizationHook(context.Background(), token, &session)
	if err != nil || !allowed || session.User != "new" || session.Email != "n@example" || len(session.Groups) != 1 {
		t.Fatalf("authorization hook result allowed=%t session=%#v err=%v", allowed, session, err)
	}

	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"allowed":false}`))
	}))
	defer deny.Close()
	a.cfg.AuthorizationHookURL = deny.URL
	allowed, err = a.authorizationHook(context.Background(), token, &Session{})
	if err != nil || allowed {
		t.Fatalf("deny hook allowed=%t err=%v", allowed, err)
	}
}

// TestAuthorizationHookFailsClosed pins the fail-closed contract: a configured
// hook that does NOT return an explicit {"allowed": true} denies. A missing
// `allowed` field, an explicit false, an empty object, and a malformed JSON body
// all deny -- only an explicit true admits. A bug or compromise in the hook can
// no longer fail open.
func TestAuthorizationHookFailsClosed(t *testing.T) {
	token := &oauth2.Token{AccessToken: "tok", Expiry: time.Now().Add(time.Hour)}

	cases := []struct {
		name string
		body string
		// for malformed JSON the call errors out; otherwise it returns deny w/o err.
		wantErr bool
	}{
		{name: "empty object", body: `{}`},
		{name: "explicit false", body: `{"allowed":false}`},
		{name: "missing allowed, other fields", body: `{"user":"x","groups":["ops"]}`},
		{name: "null allowed", body: `{"allowed":null}`},
		{name: "malformed json", body: `{not json`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer hook.Close()
			a := newAuth(t, &config.Config{AuthorizationHookURL: hook.URL})
			allowed, err := a.authorizationHook(context.Background(), token, &Session{})
			if allowed {
				t.Fatalf("%s: hook allowed access, must fail closed", tc.name)
			}
			if tc.wantErr && err == nil {
				t.Fatalf("%s: expected an error", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("%s: unexpected error %v", tc.name, err)
			}
		})
	}

	// The one admit lane: an explicit {"allowed": true} passes.
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"allowed":true}`))
	}))
	defer ok.Close()
	a := newAuth(t, &config.Config{AuthorizationHookURL: ok.URL})
	allowed, err := a.authorizationHook(context.Background(), token, &Session{})
	if err != nil || !allowed {
		t.Fatalf("explicit allow lane allowed=%t err=%v", allowed, err)
	}
}

// TestAuthorizationHookOmitsTokensByDefault pins token minimization: with no
// includeTokens configured, the hook receives token_type and expiry only -- never
// access/refresh/id tokens -- and listed tokens (and only those) appear when the
// operator opts in.
func TestAuthorizationHookOmitsTokensByDefault(t *testing.T) {
	token := (&oauth2.Token{
		AccessToken:  "access-secret",
		TokenType:    "Bearer",
		RefreshToken: "refresh-secret",
		Expiry:       time.Now().Add(time.Hour),
	}).WithExtra(map[string]any{"id_token": "id-secret"})

	var got map[string]any
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Token map[string]any `json:"token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		got = payload.Token
		_, _ = w.Write([]byte(`{"allowed":true}`))
	}))
	defer hook.Close()

	a := newAuth(t, &config.Config{AuthorizationHookURL: hook.URL})

	// Default: no tokens.
	if _, err := a.authorizationHook(context.Background(), token, &Session{}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"access_token", "refresh_token", "id_token"} {
		if _, ok := got[k]; ok {
			t.Fatalf("%s leaked to hook by default: %#v", k, got)
		}
	}
	if got["token_type"] != "Bearer" || got["expiry"] == "" {
		t.Fatalf("default token metadata missing: %#v", got)
	}

	// Opt only access in: access_token present, refresh/id still absent.
	a.cfg.AuthorizationHookIncludeTokens = []string{"access"}
	if _, err := a.authorizationHook(context.Background(), token, &Session{}); err != nil {
		t.Fatal(err)
	}
	if got["access_token"] != "access-secret" {
		t.Fatalf("opted-in access_token missing: %#v", got)
	}
	if _, ok := got["refresh_token"]; ok {
		t.Fatalf("refresh_token leaked when only access opted in: %#v", got)
	}
	if _, ok := got["id_token"]; ok {
		t.Fatalf("id_token leaked when only access opted in: %#v", got)
	}
}

// TestAuthorizationHookErrorSanitized pins that a hook 5xx body never reaches the
// caller's error: the surfaced error carries the status but NOT the response body
// (which on a compromised hook is attacker-chosen).
func TestAuthorizationHookErrorSanitized(t *testing.T) {
	const secretBody = "secret=super-sensitive-leak-marker"
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, secretBody, http.StatusInternalServerError)
	}))
	defer hook.Close()

	a := newAuth(t, &config.Config{AuthorizationHookURL: hook.URL})
	token := &oauth2.Token{AccessToken: "tok", Expiry: time.Now().Add(time.Hour)}
	allowed, err := a.authorizationHook(context.Background(), token, &Session{})
	if allowed {
		t.Fatal("5xx hook must deny")
	}
	if err == nil {
		t.Fatal("expected an error from a 5xx hook")
	}
	if strings.Contains(err.Error(), secretBody) {
		t.Fatalf("hook response body leaked into error: %v", err)
	}
}

// TestOAuthCallbackTokenExchangeErrorSanitized proves the auth-edge sanitization
// law for the UNauthenticated login flow: when the token exchange fails, the
// recon-useful detail (here a marker the token endpoint embeds in its error body,
// standing in for issuer/transport/token-exchange strings) must NOT reach the
// browser. The client sees only a generic message plus a short correlation ID,
// and the raw detail appears ONLY in the server-side slog line keyed by that same
// ID. Not parallel: it swaps the process-global default logger.
func TestOAuthCallbackTokenExchangeErrorSanitized(t *testing.T) {
	const reconMarker = "issuer-internal-host.svc.cluster.local-LEAK-MARKER"

	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	var logBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelError})))

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A non-2xx token response makes oauth2.Exchange fail with an error that
		// carries the response body; the body stands in for recon-useful detail.
		http.Error(w, reconMarker, http.StatusInternalServerError)
	}))
	defer tokenServer.Close()

	a := newAuth(t, &config.Config{
		OIDCClientID:       "client-id",
		OIDCClientSecret:   "client-secret",
		OAuth2AuthorizeURL: "https://auth.example.test/authorize",
		OAuth2TokenURL:     tokenServer.URL,
		OIDCRedirectURL:    "http://example.test/oauth2/callback",
		SessionSecret:      "test-secret",
	})
	value, err := a.sessions.Seal(StateCookieName, oauthState{Nonce: "good", OriginalURL: "/clusters"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/oauth2/callback?state=good&code=ok", nil)
	req.AddCookie(&http.Cookie{Name: StateCookieName, Value: value})
	rec := httptest.NewRecorder()
	a.Callback(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	body := rec.Body.String()
	if strings.Contains(body, reconMarker) {
		t.Fatalf("recon detail leaked to client body: %q", body)
	}
	if strings.Contains(body, "token exchange") {
		t.Fatalf("stage detail leaked to client body: %q", body)
	}

	// The client body must carry a short correlation ID...
	idRE := regexp.MustCompile(`reference ([0-9a-f]{8,12})`)
	m := idRE.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no correlation id in client body: %q", body)
	}
	clientID := m[1]

	// ...and the raw detail must appear ONLY in the server log, under that same ID.
	logged := logBuf.String()
	if !strings.Contains(logged, reconMarker) {
		t.Fatalf("raw detail missing from server log: %q", logged)
	}
	if !strings.Contains(logged, clientID) {
		t.Fatalf("server log not keyed by the client correlation id %q: %q", clientID, logged)
	}
}

// fakeIssuer is a minimal OIDC discovery endpoint that counts how many times the
// discovery document is fetched. When fail is set it returns 500 instead, to
// simulate a transient issuer outage.
type fakeIssuer struct {
	server    *httptest.Server
	discovery int32
	fail      atomic.Bool
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	fi := &fakeIssuer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fi.discovery, 1)
		if fi.fail.Load() {
			http.Error(w, "issuer down", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 fi.server.URL,
			"authorization_endpoint": fi.server.URL + "/authorize",
			"token_endpoint":         fi.server.URL + "/token",
			"jwks_uri":               fi.server.URL + "/keys",
		})
	})
	fi.server = httptest.NewServer(mux)
	t.Cleanup(fi.server.Close)
	return fi
}

func (fi *fakeIssuer) hits() int32 {
	return atomic.LoadInt32(&fi.discovery)
}

func newIssuerAuth(t *testing.T, fi *fakeIssuer) *Authenticator {
	return newAuth(t, &config.Config{
		OIDCClientID:  "client-id",
		OIDCIssuerURL: fi.server.URL,
		SessionSecret: "test-secret",
	})
}

func TestOIDCProviderCached(t *testing.T) {
	fi := newFakeIssuer(t)
	a := newIssuerAuth(t, fi)

	req := httptest.NewRequest(http.MethodGet, "/clusters", nil)
	for i := 0; i < 3; i++ {
		if _, _, err := a.oauth2Config(context.Background(), req); err != nil {
			t.Fatalf("oauth2Config call %d: %v", i, err)
		}
	}
	if got := fi.hits(); got != 1 {
		t.Fatalf("discovery hits across 3 calls = %d, want 1", got)
	}

	// A failed first discovery must NOT be cached: the next call after the issuer
	// recovers has to succeed and cache from then on.
	failing := newFakeIssuer(t)
	failing.fail.Store(true)
	a2 := newIssuerAuth(t, failing)
	if _, _, err := a2.oauth2Config(context.Background(), req); err == nil {
		t.Fatal("expected discovery failure while issuer is down")
	}
	if got := failing.hits(); got != 1 {
		t.Fatalf("failed discovery hits = %d, want 1", got)
	}
	failing.fail.Store(false)
	for i := 0; i < 2; i++ {
		if _, _, err := a2.oauth2Config(context.Background(), req); err != nil {
			t.Fatalf("oauth2Config after recovery call %d: %v", i, err)
		}
	}
	// 1 failed hit + 1 successful (cached thereafter) = 2 total.
	if got := failing.hits(); got != 2 {
		t.Fatalf("discovery hits after recovery = %d, want 2", got)
	}
}

func TestOIDCProviderCachedConcurrent(t *testing.T) {
	fi := newFakeIssuer(t)
	a := newIssuerAuth(t, fi)

	const callers = 16
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			req := httptest.NewRequest(http.MethodGet, "/clusters", nil)
			if _, _, err := a.oauth2Config(context.Background(), req); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent oauth2Config: %v", err)
	}
	if got := fi.hits(); got != 1 {
		t.Fatalf("discovery hits across %d concurrent callers = %d, want 1", callers, got)
	}
}

func queryValue(rawURL, key string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return parsed.Query().Get(key)
}

func cookieNamed(t *testing.T, cookies []*http.Cookie, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie
		}
	}
	t.Fatalf("cookie %s not found in %#v", name, cookies)
	return nil
}

// TestLogoutRejectsCrossSite proves the GET-logout CSRF close-out: a request
// with Sec-Fetch-Site: cross-site (a cross-site page force-logging-out the
// victim) is rejected with 403 and clears no cookies, while same-origin and
// `none` (user-typed/bookmark) requests, and a request with no Sec-Fetch-Site
// at all (older browser, annoyance-grade gap), clear the session and redirect.
func TestLogoutRejectsCrossSite(t *testing.T) {
	a := newAuth(t, &config.Config{SessionSecret: "test-secret", AuthMode: config.AuthModeOIDC})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/oauth2/logout", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	a.Logout(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site logout status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatalf("cross-site logout cleared cookies: %#v", rec.Result().Cookies())
	}

	for _, site := range []string{"same-origin", "same-site", "none", ""} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/oauth2/logout", nil)
		if site != "" {
			req.Header.Set("Sec-Fetch-Site", site)
		}
		a.Logout(rec, req)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
			t.Fatalf("Sec-Fetch-Site %q logout status=%d location=%q", site, rec.Code, rec.Header().Get("Location"))
		}
		if len(rec.Result().Cookies()) != 2 {
			t.Fatalf("Sec-Fetch-Site %q logout did not clear both cookies: %#v", site, rec.Result().Cookies())
		}
	}
}
