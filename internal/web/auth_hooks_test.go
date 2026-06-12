package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kbelokon/readout/internal/config"
	"github.com/kbelokon/readout/internal/hooks"
	"github.com/kbelokon/readout/internal/kube"
	"golang.org/x/oauth2"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func newBareServer(t *testing.T, cfg *config.Config) *Server {
	t.Helper()
	codec, err := newSessionCodec(cfg.SessionSecret)
	if err != nil {
		t.Fatal(err)
	}
	return &Server{cfg: *cfg, sessions: codec, hooks: hooks.NewClient()}
}

func TestAuthModesHeadersOIDCAndBearerSources(t *testing.T) {
	s := newBareServer(t, &config.Config{AuthMode: config.AuthModeHeaders, TrustedHeaderUser: "X-User", TrustedHeaderEmail: "X-Email"})
	called := false
	handler := s.auth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

	s.cfg.AuthMode = "bogus"
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/clusters", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("invalid auth status = %d", rec.Code)
	}

	s = newBareServer(t, &config.Config{
		AuthMode:           config.AuthModeNone,
		OIDCClientID:       "client",
		OAuth2AuthorizeURL: "https://auth.example/authorize",
		OAuth2TokenURL:     "https://auth.example/token",
		SessionSecret:      "test-secret",
	})
	handler = s.auth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/clusters?x=1", nil))
	if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "https://auth.example/authorize") {
		t.Fatalf("OIDC redirect status=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}
	if cookieNamed(t, rec.Result().Cookies(), stateCookieName).Path != oauthCallbackPath {
		t.Fatalf("state cookie path mismatch: %#v", rec.Result().Cookies())
	}

	value, err := s.sessions.Seal(sessionCookieName, authSession{AccessToken: "session-token", Expires: time.Now().Add(time.Hour).Unix()}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/clusters", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: value})
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("valid session status = %d", rec.Code)
	}
	if got := s.requestBearer(req); got != "session-token" {
		t.Fatalf("session bearer = %q", got)
	}
	req.Header.Set("Authorization", "Bearer direct-token")
	if got := s.requestBearer(req); got != "direct-token" {
		t.Fatalf("direct bearer = %q", got)
	}
	// The legacy access_token cookie is no longer a bearer source: a request
	// carrying only this cookie resolves to no bearer (anonymous).
	req = httptest.NewRequest(http.MethodGet, "/clusters", nil)
	req.AddCookie(&http.Cookie{Name: "access_token", Value: "cookie-token"})
	if got := s.requestBearer(req); got != "" {
		t.Fatalf("legacy access_token cookie should not be a bearer source, got %q", got)
	}
}

func TestOAuthHandlersAndSessionCodecEdges(t *testing.T) {
	s := newBareServer(t, &config.Config{
		OIDCClientID:       "client",
		OAuth2AuthorizeURL: "https://auth.example/authorize",
		OAuth2TokenURL:     "https://auth.example/token",
		SessionSecret:      "test-secret",
	})
	rec := httptest.NewRecorder()
	s.oauth2Login(rec, httptest.NewRequest(http.MethodGet, "/oauth2/login?next=https://evil.example", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("login status = %d", rec.Code)
	}
	var state oauthState
	if err := s.sessions.Open(stateCookieName, cookieNamed(t, rec.Result().Cookies(), stateCookieName).Value, &state); err != nil {
		t.Fatal(err)
	}
	if state.OriginalURL != "/" {
		t.Fatalf("unsafe next original URL = %q", state.OriginalURL)
	}

	rec = httptest.NewRecorder()
	s.oauth2Logout(rec, httptest.NewRequest(http.MethodGet, "/oauth2/logout", nil))
	if rec.Code != http.StatusFound || len(rec.Result().Cookies()) != 2 {
		t.Fatalf("logout status=%d cookies=%#v", rec.Code, rec.Result().Cookies())
	}

	for name, target := range map[string]string{
		"missing state": "/oauth2/callback",
		"bad state":     "/oauth2/callback?state=x",
	} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		if name == "bad state" {
			req.AddCookie(&http.Cookie{Name: stateCookieName, Value: "not-a-sealed-cookie"})
		}
		rec = httptest.NewRecorder()
		s.oauth2Callback(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d", name, rec.Code)
		}
	}

	sealed, err := s.sessions.Seal(stateCookieName, oauthState{Nonce: "nonce", OriginalURL: "/clusters"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/oauth2/callback?state=wrong", nil)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: sealed})
	rec = httptest.NewRecorder()
	s.oauth2Callback(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("state mismatch status = %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/oauth2/callback?state=nonce&error=denied", nil)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: sealed})
	rec = httptest.NewRecorder()
	s.oauth2Callback(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("oauth error status = %d", rec.Code)
	}

	if _, _, err := s.oauth2Config(context.Background(), httptest.NewRequest(http.MethodGet, "/", nil)); err != nil {
		t.Fatalf("oauth2Config generic endpoints failed: %v", err)
	}
	missing := newBareServer(t, &config.Config{})
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
	if err := codec.Open(sessionCookieName, "%%%bad", &authSession{}); err == nil {
		t.Fatal("bad base64 unexpectedly opened")
	}
	if err := codec.Open(sessionCookieName, "short", &authSession{}); err == nil {
		t.Fatal("short sealed value unexpectedly opened")
	}
	value, err := codec.Seal(sessionCookieName, authSession{AccessToken: "x"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := codec.Open("wrong-name", value, &authSession{}); err == nil {
		t.Fatal("wrong associated data unexpectedly opened")
	}
	expired, err := codec.Seal(sessionCookieName, authSession{AccessToken: "x"}, -time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := codec.Open(sessionCookieName, expired, &authSession{}); err == nil {
		t.Fatal("expired sealed value unexpectedly opened")
	}
	if oauthExpiry(&oauth2.Token{}).Before(time.Now().Add(6 * 24 * time.Hour)) {
		t.Fatal("zero-expiry OAuth token did not get default lifetime")
	}
}

func TestAuthorizationAndPrerenderHooks(t *testing.T) {
	s := newBareServer(t, &config.Config{})
	token := (&oauth2.Token{AccessToken: "token", TokenType: "Bearer", RefreshToken: "refresh", Expiry: time.Now().Add(time.Hour)}).WithExtra(map[string]any{"id_token": "id-token"})
	session := authSession{User: "old"}
	allowed, err := s.authorizationHook(context.Background(), token, &session)
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
	s.cfg.AuthorizationHookURL = hook.URL
	session = authSession{User: "old"}
	allowed, err = s.authorizationHook(context.Background(), token, &session)
	if err != nil || !allowed || session.User != "new" || session.Email != "n@example" || len(session.Groups) != 1 {
		t.Fatalf("authorization hook result allowed=%t session=%#v err=%v", allowed, session, err)
	}

	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"allowed":false}`))
	}))
	defer deny.Close()
	s.cfg.AuthorizationHookURL = deny.URL
	allowed, err = s.authorizationHook(context.Background(), token, &authSession{})
	if err != nil || allowed {
		t.Fatalf("deny hook allowed=%t err=%v", allowed, err)
	}

	obj := kube.NewObject(&kube.ResourceType{APIVersion: "v1", Version: "v1", Plural: "pods", Kind: "Pod", Namespaced: true}, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]any{"name": "nginx", "namespace": "default"},
	}})
	links := []config.Link{{Href: "/old", Title: "Old"}}
	gotLinks, replacement, err := s.resourcePrerenderHook(context.Background(), "test", "default", "pods", &obj, links)
	if err != nil || replacement != nil || len(gotLinks) != 1 {
		t.Fatalf("empty prerender hook links=%#v replacement=%#v err=%v", gotLinks, replacement, err)
	}
	prerender := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Cluster  string         `json:"cluster"`
			Resource map[string]any `json:"resource"`
			Links    []config.Link  `json:"links"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Cluster != "test" || payload.Resource["kind"] != "Pod" || len(payload.Links) != 1 {
			t.Fatalf("prerender payload = %#v", payload)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"links":    []config.Link{{Href: "/new", Title: "New"}},
			"resource": map[string]any{"kind": "Pod", "metadata": map[string]any{"name": "replacement"}},
		})
	}))
	defer prerender.Close()
	s.cfg.ResourcePrerenderHookURL = prerender.URL
	gotLinks, replacement, err = s.resourcePrerenderHook(context.Background(), "test", "default", "pods", &obj, links)
	if err != nil || len(gotLinks) != 2 || replacement["kind"] != "Pod" {
		t.Fatalf("prerender hook links=%#v replacement=%#v err=%v", gotLinks, replacement, err)
	}
}
