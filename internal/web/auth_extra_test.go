package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kbelokon/readout/internal/config"
)

func TestAuthMiddlewareModes(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	app := newTestServerWithConfig(t, &config.Config{
		Port:               8080,
		Clusters:           []config.ClusterConnection{{Name: "test", Server: newServerFakeAPI(t).URL}},
		DefaultTheme:       "dark",
		AuthMode:           config.AuthModeHeaders,
		TrustedHeaderUser:  "X-User",
		TrustedHeaderEmail: "X-Email",
	})

	public := httptest.NewRecorder()
	app.auth(next).ServeHTTP(public, httptest.NewRequest(http.MethodGet, "/health", nil))
	if public.Code != http.StatusNoContent {
		t.Fatalf("public status = %d", public.Code)
	}

	missing := httptest.NewRecorder()
	app.auth(next).ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/clusters", nil))
	if missing.Code != http.StatusUnauthorized {
		t.Fatalf("missing header status = %d", missing.Code)
	}

	authorized := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/clusters", nil)
	req.Header.Set("X-Email", "user@example.test")
	app.auth(next).ServeHTTP(authorized, req)
	if authorized.Code != http.StatusNoContent {
		t.Fatalf("trusted header status = %d", authorized.Code)
	}

	badMode := newTestServerWithConfig(t, &config.Config{
		Port:         8080,
		Clusters:     []config.ClusterConnection{{Name: "test", Server: newServerFakeAPI(t).URL}},
		DefaultTheme: "dark",
		AuthMode:     "bogus",
	})
	rec := httptest.NewRecorder()
	badMode.auth(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/clusters", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("bad auth mode status = %d", rec.Code)
	}
}

func TestHeaderGroupsReachHook(t *testing.T) {
	var payload struct {
		Token   map[string]any `json:"token"`
		Session authSession    `json:"session"`
	}
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": true})
	}))
	defer hook.Close()

	app := newTestServerWithConfig(t, &config.Config{
		Port:                 8080,
		Clusters:             []config.ClusterConnection{{Name: "test", Server: newServerFakeAPI(t).URL}},
		DefaultTheme:         "dark",
		AuthMode:             config.AuthModeHeaders,
		TrustedHeaderUser:    "X-User",
		TrustedHeaderEmail:   "X-Email",
		TrustedHeaderGroups:  "X-Groups",
		AuthorizationHookURL: hook.URL,
	})
	nextCalled := false
	handler := app.auth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
	if payload.Token["access_token"] != "" || payload.Token["expiry"] == "" {
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
	app.cfg.AuthorizationHookURL = deny.URL
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
	app := newTestServerWithConfig(t, &config.Config{
		Port:               8080,
		Clusters:           []config.ClusterConnection{{Name: "test", Server: newServerFakeAPI(t).URL}},
		DefaultTheme:       "dark",
		AuthMode:           config.AuthModeHeaders,
		TrustedHeaderUser:  "X-User",
		TrustedHeaderEmail: "X-Email",
	})
	nextCalled := false
	handler := app.auth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

func TestOAuthLoginLogoutAndURLHelpers(t *testing.T) {
	app := newTestServerWithConfig(t, &config.Config{
		Port:               8080,
		Clusters:           []config.ClusterConnection{{Name: "test", Server: newServerFakeAPI(t).URL}},
		DefaultTheme:       "dark",
		OIDCClientID:       "client-id",
		OAuth2AuthorizeURL: "https://auth.example.test/authorize",
		OAuth2TokenURL:     "https://auth.example.test/token",
		OAuth2Scope:        "openid email",
		SessionSecret:      "test-secret",
		AuthMode:           config.AuthModeNone,
	})
	if !app.oauthConfigured() || app.effectiveAuthMode() != config.AuthModeOIDC {
		t.Fatalf("oauth should be auto-enabled from endpoint config")
	}

	rec := httptest.NewRecorder()
	app.oauth2Login(rec, httptest.NewRequest(http.MethodGet, "/oauth2/login?next=https://evil.example/path", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("login status = %d body=%s", rec.Code, rec.Body.String())
	}
	location := rec.Header().Get("Location")
	if !strings.HasPrefix(location, "https://auth.example.test/authorize?") || queryValue(location, "scope") != "openid email" {
		t.Fatalf("bad authorize redirect: %s", location)
	}
	var state oauthState
	if err := app.sessions.Open(stateCookieName, cookieNamed(t, rec.Result().Cookies(), stateCookieName).Value, &state); err != nil {
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
		app.oauth2Login(rec, httptest.NewRequest(http.MethodGet, "/oauth2/login?next="+url.QueryEscape(tc.next), nil))
		if rec.Code != http.StatusFound {
			t.Fatalf("login %q status = %d body=%s", tc.next, rec.Code, rec.Body.String())
		}
		var state oauthState
		if err := app.sessions.Open(stateCookieName, cookieNamed(t, rec.Result().Cookies(), stateCookieName).Value, &state); err != nil {
			t.Fatal(err)
		}
		if state.OriginalURL != tc.want {
			t.Fatalf("login next %q stored OriginalURL %q, want %q", tc.next, state.OriginalURL, tc.want)
		}
	}

	logout := httptest.NewRecorder()
	app.oauth2Logout(logout, httptest.NewRequest(http.MethodGet, "/oauth2/logout", nil))
	if logout.Code != http.StatusFound || logout.Header().Get("Location") != "/" {
		t.Fatalf("logout redirect status=%d location=%q", logout.Code, logout.Header().Get("Location"))
	}
	for _, name := range []string{sessionCookieName, stateCookieName} {
		cookie := cookieNamed(t, logout.Result().Cookies(), name)
		if cookie.MaxAge >= 0 {
			t.Fatalf("%s was not cleared: %#v", name, cookie)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "http://internal.example/path", nil)
	req.Header.Set("X-Forwarded-Proto", "https, http")
	req.Header.Set("X-Forwarded-Host", "public.example.test, internal.example")
	if got := externalURL(req, oauthCallbackPath); got != "https://public.example.test/oauth2/callback" {
		t.Fatalf("externalURL = %q", got)
	}
	if firstForwarded(" a, b ") != "a" || !secureCookie(req) {
		t.Fatalf("forwarded helpers mismatch")
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

	app := newTestServerWithConfig(t, &config.Config{
		Port:          8080,
		Clusters:      []config.ClusterConnection{{Name: "test", Server: newServerFakeAPI(t).URL}},
		DefaultTheme:  "dark",
		SessionSecret: "test-secret",
	})
	req := httptest.NewRequest(http.MethodGet, "/clusters", nil)
	req.Header.Set("Authorization", "Bearer header-token")
	if got := app.requestBearer(req); got != "header-token" {
		t.Fatalf("header bearer = %q", got)
	}

	// The legacy access_token cookie is no longer a bearer source: only the
	// Authorization header and the sealed session cookie are honored. A request
	// carrying only this cookie must resolve to no bearer (anonymous).
	cookieReq := httptest.NewRequest(http.MethodGet, "/clusters", nil)
	cookieReq.AddCookie(&http.Cookie{Name: "access_token", Value: "cookie-token"})
	if got := app.requestBearer(cookieReq); got != "" {
		t.Fatalf("legacy access_token cookie should not be a bearer source, got %q", got)
	}

	sessionValue, err := app.sessions.Seal(sessionCookieName, authSession{AccessToken: "session-token", Expires: time.Now().Add(time.Hour).Unix()}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	sessionReq := httptest.NewRequest(http.MethodGet, "/clusters", nil)
	sessionReq.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionValue})
	if got := app.requestBearer(sessionReq); got != "session-token" {
		t.Fatalf("session bearer = %q", got)
	}

	expiredValue, err := app.sessions.Seal(sessionCookieName, authSession{AccessToken: "expired-token", Expires: time.Now().Add(-time.Hour).Unix()}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	expiredReq := httptest.NewRequest(http.MethodGet, "/clusters", nil)
	expiredReq.AddCookie(&http.Cookie{Name: sessionCookieName, Value: expiredValue})
	if _, ok := app.authSession(expiredReq); ok {
		t.Fatal("expired auth session should not be accepted")
	}
}

func TestOAuthCallbackRejectsExternalOriginalURL(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"session-token","token_type":"Bearer","expires_in":3600}`))
	}))
	defer tokenServer.Close()

	app := newTestServerWithConfig(t, &config.Config{
		Port:               8080,
		Clusters:           []config.ClusterConnection{{Name: "test", Server: newServerFakeAPI(t).URL}},
		DefaultTheme:       "dark",
		OIDCClientID:       "client-id",
		OIDCClientSecret:   "client-secret",
		OAuth2AuthorizeURL: "https://auth.example.test/authorize",
		OAuth2TokenURL:     tokenServer.URL,
		OIDCRedirectURL:    "http://example.test/oauth2/callback",
		SessionSecret:      "test-secret",
	})
	for _, originalURL := range []string{"//evil.example/path", `/\evil.example/path`} {
		value, err := app.sessions.Seal(stateCookieName, oauthState{Nonce: "good", OriginalURL: originalURL}, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodGet, "/oauth2/callback?state=good&code=ok", nil)
		req.AddCookie(&http.Cookie{Name: stateCookieName, Value: value})
		rec := httptest.NewRecorder()
		app.oauth2Callback(rec, req)
		if rec.Code != http.StatusFound {
			t.Fatalf("callback for %q status = %d body=%s", originalURL, rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != "/" {
			t.Fatalf("callback for %q Location = %q, want /", originalURL, loc)
		}
	}
}

func TestOAuthCallbackRejectsBadInputs(t *testing.T) {
	app := newTestServerWithConfig(t, &config.Config{
		Port:               8080,
		Clusters:           []config.ClusterConnection{{Name: "test", Server: newServerFakeAPI(t).URL}},
		DefaultTheme:       "dark",
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
			req.AddCookie(&http.Cookie{Name: stateCookieName, Value: "bad"})
			return req
		}(), http.StatusBadRequest},
		{"state mismatch", func() *http.Request {
			req := httptest.NewRequest(http.MethodGet, "/oauth2/callback?state=bad", nil)
			value, err := app.sessions.Seal(stateCookieName, oauthState{Nonce: "good", OriginalURL: "/clusters"}, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			req.AddCookie(&http.Cookie{Name: stateCookieName, Value: value})
			return req
		}(), http.StatusBadRequest},
		{"provider error", func() *http.Request {
			req := httptest.NewRequest(http.MethodGet, "/oauth2/callback?state=good&error=access_denied", nil)
			value, err := app.sessions.Seal(stateCookieName, oauthState{Nonce: "good", OriginalURL: "/clusters"}, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			req.AddCookie(&http.Cookie{Name: stateCookieName, Value: value})
			return req
		}(), http.StatusUnauthorized},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		app.oauth2Callback(rec, tc.req)
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
		app := newTestServerWithConfig(t, &config.Config{
			Port:                 8080,
			Clusters:             []config.ClusterConnection{{Name: "test", Server: newServerFakeAPI(t).URL}},
			DefaultTheme:         "dark",
			OIDCClientID:         "client-id",
			OIDCClientSecret:     "client-secret",
			OAuth2AuthorizeURL:   "https://auth.example.test/authorize",
			OAuth2TokenURL:       tokenServer.URL,
			OIDCRedirectURL:      "http://example.test/oauth2/callback",
			SessionSecret:        "test-secret",
			AuthorizationHookURL: hookURL,
		})
		value, err := app.sessions.Seal(stateCookieName, oauthState{Nonce: "good", OriginalURL: "/clusters"}, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodGet, "/oauth2/callback?state=good&code=ok", nil)
		req.AddCookie(&http.Cookie{Name: stateCookieName, Value: value})
		rec := httptest.NewRecorder()
		app.oauth2Callback(rec, req)
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
	app := newTestServer(t)
	if _, _, err := app.oauth2Config(context.Background(), httptest.NewRequest(http.MethodGet, "/clusters", nil)); err == nil {
		t.Fatal("expected missing client id error")
	}
	app.cfg.OIDCClientID = "client-id"
	if _, _, err := app.oauth2Config(context.Background(), httptest.NewRequest(http.MethodGet, "/clusters", nil)); err == nil {
		t.Fatal("expected missing provider endpoints error")
	}
	rec := httptest.NewRecorder()
	app.startOAuth2(rec, httptest.NewRequest(http.MethodGet, "/clusters", nil), "/clusters")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("startOAuth2 error status = %d", rec.Code)
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

func newIssuerServer(t *testing.T, fi *fakeIssuer) *Server {
	return newTestServerWithConfig(t, &config.Config{
		Port:          8080,
		Clusters:      []config.ClusterConnection{{Name: "test", Server: newServerFakeAPI(t).URL}},
		DefaultTheme:  "dark",
		OIDCClientID:  "client-id",
		OIDCIssuerURL: fi.server.URL,
		SessionSecret: "test-secret",
	})
}

func TestOIDCProviderCached(t *testing.T) {
	fi := newFakeIssuer(t)
	app := newIssuerServer(t, fi)

	req := httptest.NewRequest(http.MethodGet, "/clusters", nil)
	for i := 0; i < 3; i++ {
		if _, _, err := app.oauth2Config(context.Background(), req); err != nil {
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
	app2 := newIssuerServer(t, failing)
	if _, _, err := app2.oauth2Config(context.Background(), req); err == nil {
		t.Fatal("expected discovery failure while issuer is down")
	}
	if got := failing.hits(); got != 1 {
		t.Fatalf("failed discovery hits = %d, want 1", got)
	}
	failing.fail.Store(false)
	for i := 0; i < 2; i++ {
		if _, _, err := app2.oauth2Config(context.Background(), req); err != nil {
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
	app := newIssuerServer(t, fi)

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
			if _, _, err := app.oauth2Config(context.Background(), req); err != nil {
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
