package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/kbelokon/readout/internal/config"
	"golang.org/x/oauth2"
)

const (
	sessionCookieName = "READOUT"
	stateCookieName   = "READOUT_STATE"
	oauthCallbackPath = "/oauth2/callback"
)

type authSession struct {
	AccessToken  string   `json:"access_token"`
	TokenType    string   `json:"token_type,omitempty"`
	RefreshToken string   `json:"refresh_token,omitempty"`
	IDToken      string   `json:"id_token,omitempty"`
	Expires      int64    `json:"expires"`
	User         string   `json:"user,omitempty"`
	Email        string   `json:"email,omitempty"`
	Groups       []string `json:"groups,omitempty"`
}

type oauthState struct {
	Nonce       string `json:"nonce"`
	OriginalURL string `json:"original_url"`
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPublicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		switch s.effectiveAuthMode() {
		case "", config.AuthModeNone:
			next.ServeHTTP(w, r)
			return
		case config.AuthModeHeaders:
			if r.Header.Get(s.cfg.TrustedHeaderUser) == "" && r.Header.Get(s.cfg.TrustedHeaderEmail) == "" {
				http.Error(w, "missing trusted identity header", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
			return
		case config.AuthModeOIDC:
			if _, ok := s.authSession(r); ok {
				next.ServeHTTP(w, r)
				return
			}
			s.startOAuth2(w, r, r.URL.RequestURI())
			return
		default:
			http.Error(w, "invalid auth mode", http.StatusInternalServerError)
			return
		}
	})
}

func (s *Server) oauth2Login(w http.ResponseWriter, r *http.Request) {
	next := r.URL.Query().Get("next")
	if !isLocalRedirect(next) {
		next = "/"
	}
	s.startOAuth2(w, r, next)
}

func (s *Server) oauth2Logout(w http.ResponseWriter, r *http.Request) {
	clearCookie(w, r, sessionCookieName, "/")
	clearCookie(w, r, stateCookieName, oauthCallbackPath)
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) oauth2Callback(w http.ResponseWriter, r *http.Request) {
	stateCookie, err := r.Cookie(stateCookieName)
	if err != nil {
		http.Error(w, "missing OAuth state cookie", http.StatusBadRequest)
		return
	}
	var state oauthState
	if err := s.sessions.Open(stateCookieName, stateCookie.Value, &state); err != nil {
		http.Error(w, "invalid OAuth state", http.StatusBadRequest)
		return
	}
	clearCookie(w, r, stateCookieName, oauthCallbackPath)
	if state.Nonce == "" || state.Nonce != r.URL.Query().Get("state") {
		http.Error(w, "OAuth state mismatch", http.StatusBadRequest)
		return
	}
	if msg := r.URL.Query().Get("error"); msg != "" {
		http.Error(w, "OAuth error: "+msg, http.StatusUnauthorized)
		return
	}
	oauthConfig, verifier, err := s.oauth2Config(r.Context(), r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	token, err := oauthConfig.Exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		http.Error(w, "OAuth token exchange failed: "+err.Error(), http.StatusUnauthorized)
		return
	}
	session := authSession{
		AccessToken:  token.AccessToken,
		TokenType:    token.TokenType,
		RefreshToken: token.RefreshToken,
		Expires:      oauthExpiry(token).Unix(),
	}
	if idToken, _ := token.Extra("id_token").(string); idToken != "" {
		session.IDToken = idToken
		if verifier != nil {
			verified, err := verifier.Verify(r.Context(), idToken)
			if err != nil {
				http.Error(w, "OIDC ID token verification failed: "+err.Error(), http.StatusUnauthorized)
				return
			}
			var claims struct {
				Subject           string   `json:"sub"`
				Email             string   `json:"email"`
				PreferredUsername string   `json:"preferred_username"`
				Name              string   `json:"name"`
				Groups            []string `json:"groups"`
			}
			if err := verified.Claims(&claims); err != nil {
				http.Error(w, "OIDC claims parse failed: "+err.Error(), http.StatusUnauthorized)
				return
			}
			session.User = first(claims.PreferredUsername, claims.Name, claims.Subject)
			session.Email = claims.Email
			session.Groups = claims.Groups
		}
	}
	allowed, err := s.authorizationHook(r.Context(), token, &session)
	if err != nil {
		http.Error(w, "authorization hook failed: "+err.Error(), http.StatusForbidden)
		return
	}
	if !allowed {
		http.Error(w, "Access Denied", http.StatusForbidden)
		return
	}
	ttl := time.Until(time.Unix(session.Expires, 0))
	if ttl <= 0 {
		http.Error(w, "OAuth token already expired", http.StatusUnauthorized)
		return
	}
	value, err := s.sessions.Seal(sessionCookieName, session, ttl)
	if err != nil {
		http.Error(w, "failed to create session", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		Secure:   secureCookie(r),
		SameSite: http.SameSiteLaxMode,
	})
	target := state.OriginalURL
	if !isLocalRedirect(target) {
		target = "/"
	}
	http.Redirect(w, r, target, http.StatusFound)
}

func (s *Server) startOAuth2(w http.ResponseWriter, r *http.Request, originalURL string) {
	oauthConfig, _, err := s.oauth2Config(r.Context(), r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	nonce, err := randomToken(32)
	if err != nil {
		http.Error(w, "failed to generate OAuth state", http.StatusInternalServerError)
		return
	}
	state := oauthState{Nonce: nonce, OriginalURL: originalURL}
	cookieValue, err := s.sessions.Seal(stateCookieName, state, 10*time.Minute)
	if err != nil {
		http.Error(w, "failed to create OAuth state", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    cookieValue,
		Path:     oauthCallbackPath,
		MaxAge:   int((10 * time.Minute).Seconds()),
		HttpOnly: true,
		Secure:   secureCookie(r),
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, oauthConfig.AuthCodeURL(nonce), http.StatusFound)
}

func (s *Server) oauth2Config(ctx context.Context, r *http.Request) (*oauth2.Config, *oidc.IDTokenVerifier, error) {
	if s.cfg.OIDCClientID == "" {
		return nil, nil, errors.New("missing OIDC/OAuth2 client ID")
	}
	redirectURL := s.cfg.OIDCRedirectURL
	if redirectURL == "" {
		redirectURL = externalURL(r, oauthCallbackPath)
	}
	scopes := strings.Fields(s.cfg.OAuth2Scope)
	var endpoint oauth2.Endpoint
	var verifier *oidc.IDTokenVerifier
	switch {
	case s.cfg.OIDCIssuerURL != "":
		provider, err := oidc.NewProvider(ctx, s.cfg.OIDCIssuerURL)
		if err != nil {
			return nil, nil, fmt.Errorf("OIDC discovery failed: %w", err)
		}
		endpoint = provider.Endpoint()
		if len(scopes) == 0 {
			scopes = []string{oidc.ScopeOpenID, "email", "profile"}
		}
		verifier = provider.Verifier(&oidc.Config{ClientID: s.cfg.OIDCClientID})
	case s.cfg.OAuth2AuthorizeURL != "" && s.cfg.OAuth2TokenURL != "":
		endpoint = oauth2.Endpoint{
			AuthURL:  s.cfg.OAuth2AuthorizeURL,
			TokenURL: s.cfg.OAuth2TokenURL,
		}
	default:
		return nil, nil, errors.New("missing OIDC issuer URL or generic OAuth2 authorize/token URLs")
	}
	return &oauth2.Config{
		ClientID:     s.cfg.OIDCClientID,
		ClientSecret: s.cfg.OIDCClientSecret,
		Endpoint:     endpoint,
		RedirectURL:  redirectURL,
		Scopes:       scopes,
	}, verifier, nil
}

func (s *Server) authSession(r *http.Request) (authSession, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return authSession{}, false
	}
	var session authSession
	if err := s.sessions.Open(sessionCookieName, cookie.Value, &session); err != nil {
		return authSession{}, false
	}
	if session.AccessToken == "" || session.Expires <= time.Now().Unix() {
		return authSession{}, false
	}
	return session, true
}

func (s *Server) requestBearer(r *http.Request) string {
	authz := r.Header.Get("Authorization")
	if strings.HasPrefix(authz, "Bearer ") {
		return strings.TrimPrefix(authz, "Bearer ")
	}
	if session, ok := s.authSession(r); ok {
		return session.AccessToken
	}
	if cookie, err := r.Cookie("access_token"); err == nil {
		return cookie.Value
	}
	return ""
}

func (s *Server) effectiveAuthMode() string {
	if s.cfg.AuthMode == config.AuthModeNone && s.oauthConfigured() {
		return config.AuthModeOIDC
	}
	return s.cfg.AuthMode
}

func (s *Server) oauthConfigured() bool {
	return s.cfg.OIDCIssuerURL != "" || (s.cfg.OAuth2AuthorizeURL != "" && s.cfg.OAuth2TokenURL != "")
}

func isPublicPath(path string) bool {
	return path == "/health" ||
		path == "/healthz" ||
		path == "/readyz" ||
		path == "/metrics" ||
		path == oauthCallbackPath ||
		path == "/oauth2/login" ||
		path == "/oauth2/logout" ||
		strings.HasPrefix(path, "/assets/")
}

func oauthExpiry(token *oauth2.Token) time.Time {
	expires := token.Expiry
	if expires.IsZero() {
		expires = time.Now().Add(7 * 24 * time.Hour)
	}
	remaining := time.Until(expires)
	if remaining > 10*time.Minute {
		expires = expires.Add(-5 * time.Minute)
	}
	return expires
}

func externalURL(r *http.Request, path string) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(firstForwarded(r.Header.Get("X-Forwarded-Proto")), "https") {
		scheme = "https"
	}
	host := firstForwarded(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host + path
}

func firstForwarded(value string) string {
	part, _, _ := strings.Cut(value, ",")
	return strings.TrimSpace(part)
}

func clearCookie(w http.ResponseWriter, r *http.Request, name, path string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     path,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secureCookie(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func secureCookie(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(firstForwarded(r.Header.Get("X-Forwarded-Proto")), "https")
}

func randomToken(n int) (string, error) {
	data := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}
